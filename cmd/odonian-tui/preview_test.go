package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/charmbracelet/x/ansi"
)

var previewNow = time.Date(2026, 11, 24, 13, 0, 0, 0, time.UTC)

const previewPR = "https://github.com/boldfield/odonian/pull/212"

func newTestPreview(item *tuiview.WorkItem, width, height int) *PreviewPane {
	p := NewPreviewPane(item, width, height)
	p.now = func() time.Time { return previewNow }
	return p
}

func checksItem() *tuiview.WorkItem {
	started := previewNow.Add(-18 * time.Minute).Format(time.RFC3339Nano)
	return &tuiview.WorkItem{Implement: &tuiclient.Task{
		ID: "impl-checks", Kind: "implement", Title: "D-31 Repair loop", State: stateChecks,
		ReviewRound: 1, Model: "sonnet", ChecksStartedAt: &started,
	}}
}

func reviewItem() *tuiview.WorkItem {
	return &tuiview.WorkItem{
		Implement: &tuiclient.Task{
			ID: "impl-review", Kind: "implement", Title: "D-30 Rubric loader", State: stateReview,
			ReviewRound: 2, Model: "sonnet",
		},
		Reviews: []*tuiclient.Task{
			{ID: "rev-1", Kind: "review", Model: "opus", ReviewRound: 2, Verdict: strPtr("approve")},
			{ID: "rev-2", Kind: "review", Model: "gpt-5.6-sol", ReviewRound: 2},
		},
	}
}

func testEvent(minute int, kind, actor string, note *string) tuiclient.Event {
	return tuiclient.Event{
		Kind: kind, Actor: actor, Note: note,
		CreatedAt: time.Date(2026, 11, 24, 12, minute, 0, 0, time.UTC).Format(time.RFC3339Nano),
	}
}

func viewString(p *PreviewPane) string {
	return p.View().Content
}

func assertWithinBounds(t *testing.T, out string, width, height int) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) > height {
		t.Errorf("rendered %d lines, height is %d:\n%s", len(lines), height, out)
	}
	for _, line := range lines {
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("line %q is %d cells, width is %d", line, w, width)
		}
		if !utf8.ValidString(line) {
			t.Errorf("line %q is not valid UTF-8", line)
		}
	}
}

func TestPreviewPane_InChecks(t *testing.T) {
	p := newTestPreview(checksItem(), 40, 20)
	p.SetPRURL(previewPR)
	out := viewString(p)

	for _, want := range []string{
		"D-31 Repair loop",
		"checks · round 1 · sonnet",
		"PR #212 · in checks 18m",
		"Reviews (round 1)",
		"  none yet: waiting for CI",
		"History",
		"  loading…",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "github.com") {
		t.Errorf("PR should be shown as a number, not a URL:\n%s", out)
	}
	assertWithinBounds(t, out, 40, 20)
}

func TestPreviewPane_ReviewOneVerdictOnePending(t *testing.T) {
	p := newTestPreview(reviewItem(), 40, 20)
	out := viewString(p)

	for _, want := range []string{
		"review · round 2 · sonnet",
		"Reviews (round 2)",
		"  opus ✔ approve",
		"  gpt-5.6-sol … pending",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "in checks") {
		t.Errorf("time in checks shown outside checks:\n%s", out)
	}
}

func TestPreviewPane_SetDetailUsesCurrentRoundPR(t *testing.T) {
	p := newTestPreview(reviewItem(), 40, 20)
	p.SetDetail(tuiclient.TaskDetail{
		Links: []tuiclient.TaskLink{
			{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/545"},
			{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/547"},
		},
		CurrentRoundLinks: []tuiclient.TaskLink{
			{Kind: "branch", Value: "mr/abc"},
			{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/547"},
		},
	})
	out := viewString(p)
	if !strings.Contains(out, "PR #547") || strings.Contains(out, "#545") {
		t.Errorf("want only the current round's PR #547:\n%s", out)
	}
}

func TestPreviewPane_HistoryLoadingThenLoaded(t *testing.T) {
	p := newTestPreview(checksItem(), 40, 20)
	if out := viewString(p); !strings.Contains(out, "loading…") {
		t.Fatalf("want loading line before events arrive:\n%s", out)
	}

	// Events for another task are ignored.
	p.Update(PreviewEventsMsg{TaskID: "other", Events: []tuiclient.Event{testEvent(1, "submit", "w", nil)}})
	if out := viewString(p); !strings.Contains(out, "loading…") {
		t.Fatalf("events for another task should be ignored:\n%s", out)
	}

	p.Update(PreviewEventsMsg{TaskID: "impl-checks", Events: []tuiclient.Event{
		testEvent(12, "claim", "worker-abc-c4wqd", nil),
		testEvent(41, "submit", "worker-abc-c4wqd", nil),
		testEvent(42, "transition", "system", strPtr("in_progress->checks")),
	}})
	out := viewString(p)
	if strings.Contains(out, "loading") {
		t.Errorf("loading line still shown after events arrived:\n%s", out)
	}
	want := "History\n  12:42 in_progress->checks\n  12:41 submitted\n  12:12 claimed by @c4wqd"
	if !strings.Contains(out, want) {
		t.Errorf("want newest-first history\n%s\nin:\n%s", want, out)
	}
}

func TestPreviewPane_HistoryLoadedEmpty(t *testing.T) {
	p := newTestPreview(checksItem(), 40, 20)
	p.Update(PreviewEventsMsg{TaskID: "impl-checks", Events: nil})
	out := viewString(p)
	if strings.Contains(out, "loading") || !strings.Contains(out, "  no events yet") {
		t.Errorf("loaded empty history should differ from loading:\n%s", out)
	}
}

func TestPreviewPane_SetItemResetsHistory(t *testing.T) {
	p := newTestPreview(checksItem(), 40, 20)
	p.SetPRURL(previewPR)
	p.SetEvents([]tuiclient.Event{testEvent(1, "submit", "w", nil)})
	p.SetItem(reviewItem())
	out := viewString(p)
	if !strings.Contains(out, "loading…") || strings.Contains(out, "PR #212") {
		t.Errorf("switching items should reset history and PR:\n%s", out)
	}
}

func TestPreviewPane_TooShortForAllEvents(t *testing.T) {
	p := newTestPreview(reviewItem(), 40, 12)
	var events []tuiclient.Event
	for i := 0; i < 10; i++ {
		events = append(events, testEvent(i, fmt.Sprintf("kind_%d", i), "system", nil))
	}
	p.SetEvents(events)
	out := viewString(p)
	assertWithinBounds(t, out, 40, 12)

	if n := len(strings.Split(out, "\n")); n != 12 {
		t.Errorf("want the pane filled to 12 lines, got %d:\n%s", n, out)
	}
	// Title, status, blank, reviews header, two reviews, blank, history
	// header: 8 lines, leaving 4 for the newest events.
	for i := 9; i >= 6; i-- {
		if !strings.Contains(out, fmt.Sprintf("kind %d", i)) {
			t.Errorf("newest event kind %d missing:\n%s", i, out)
		}
	}
	for i := 0; i <= 5; i++ {
		if strings.Contains(out, fmt.Sprintf("kind %d", i)) {
			t.Errorf("older event kind %d should be dropped:\n%s", i, out)
		}
	}
	if strings.Index(out, "kind 9") > strings.Index(out, "kind 6") {
		t.Errorf("events should be newest first:\n%s", out)
	}
}

func TestPreviewPane_WindowSizeMsg(t *testing.T) {
	p := newTestPreview(checksItem(), 40, 20)
	p.Update(tea.WindowSizeMsg{Width: 10, Height: 3})
	out := viewString(p)
	assertWithinBounds(t, out, 10, 3)
	if n := len(strings.Split(out, "\n")); n != 3 {
		t.Errorf("want 3 lines after resize, got %d:\n%s", n, out)
	}
}

// TestPreviewPane_NeverExceedsBounds renders every scenario at narrow and
// normal widths, and checks both the bounds and that wrapping keeps the
// content: with all whitespace removed, the required text is still there.
func TestPreviewPane_NeverExceedsBounds(t *testing.T) {
	longModel := "claude-opus-long-model-name"
	scenarios := map[string]func() *PreviewPane{
		"checks with PR": func() *PreviewPane {
			p := newTestPreview(checksItem(), 0, 0)
			p.SetPRURL(previewPR)
			return p
		},
		"review with verdicts": func() *PreviewPane {
			item := reviewItem()
			item.Reviews[0].Model = longModel
			return newTestPreview(item, 0, 0)
		},
		"loaded events": func() *PreviewPane {
			p := newTestPreview(checksItem(), 0, 0)
			p.SetEvents([]tuiclient.Event{
				testEvent(12, "claim", "worker-abc-c4wqd", nil),
				testEvent(41, "transition", "system", strPtr("checks failed: returned to ready")),
			})
			return p
		},
		"wide characters": func() *PreviewPane {
			item := checksItem()
			item.Implement.Title = "修复中文标题的预览窗格宽度问题"
			return newTestPreview(item, 0, 0)
		},
		"emoji": func() *PreviewPane {
			item := checksItem()
			item.Implement.Title = previewEmoji
			return newTestPreview(item, 0, 0)
		},
	}
	required := map[string][]string{
		"checks with PR":       {"checks·round1·sonnet", "PR#212", "inchecks18m", "noneyet:waitingforCI"},
		"review with verdicts": {longModel + "✔approve", "gpt-5.6-sol…pending"},
		"loaded events":        {"12:12claimedby@c4wqd", "12:41checksfailed:returnedtoready"},
		"wide characters":      {"修复中文标题的预览窗格宽度问题"},
		"emoji":                {previewEmoji},
	}

	for name, build := range scenarios {
		for _, width := range []int{1, 2, 3, 5, 8, 12, 30, 60} {
			t.Run(fmt.Sprintf("%s/width %d", name, width), func(t *testing.T) {
				p := build()
				p.SetSize(width, 200)
				out := viewString(p)
				assertWithinBounds(t, out, width, 200)

				// A 2-cell rune cannot be shown in a 1-cell pane.
				if (name == "wide characters" || name == "emoji") && width < 2 {
					return
				}
				flat := strings.Join(strings.Fields(out), "")
				for _, want := range required[name] {
					if !strings.Contains(flat, want) {
						t.Errorf("content %q lost at width %d:\n%s", want, width, out)
					}
				}
			})
		}
	}
}

// previewEmoji holds 2-cell grapheme clusters made of several runes: emoji
// with a variation selector, a keycap sequence and a ZWJ sequence.
const previewEmoji = "❤️✈️1️⃣👩‍💻"

func TestWrapCells_SplitsBetweenGraphemeClusters(t *testing.T) {
	want := []string{"❤️", "✈️", "1️⃣", "👩‍💻"}
	got := wrapCells(previewEmoji, 2)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("wrapCells(%q, 2) = %q, want %q", previewEmoji, got, want)
	}
	if got := wrapCells(previewEmoji, 1); strings.Join(got, "") != "" {
		t.Errorf("2-cell clusters cannot fit in 1 cell and should be dropped, got %q", got)
	}
}

func TestPreviewPane_NarrowEventKeepsTime(t *testing.T) {
	p := newTestPreview(checksItem(), 5, 100)
	p.Update(PreviewEventsMsg{TaskID: "impl-checks", Events: []tuiclient.Event{testEvent(12, "submit", "w", nil)}})
	out := viewString(p)
	assertWithinBounds(t, out, 5, 100)
	flat := strings.Join(strings.Fields(out), "")
	if !strings.Contains(flat, "12:12submitted") {
		t.Errorf("event time and text should both survive at width 5:\n%s", out)
	}
}

func TestPreviewPane_ZeroSize(t *testing.T) {
	p := newTestPreview(checksItem(), 0, 10)
	if out := p.Render(); out != "" {
		t.Errorf("zero width should render nothing, got %q", out)
	}
	p.SetSize(10, 0)
	if out := p.Render(); out != "" {
		t.Errorf("zero height should render nothing, got %q", out)
	}
}
