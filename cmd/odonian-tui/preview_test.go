package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/mattn/go-runewidth"
)

func TestPreviewPaneRender_WorkItemInChecks(t *testing.T) {
	now := "2026-11-24T12:00:00Z"
	impl := &tuiclient.Task{
		ID:              "task-123",
		Title:           "Test task",
		State:           stateChecks,
		Model:           "sonnet",
		ReviewRound:     1,
		ChecksStartedAt: &now,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	mockClock := func() time.Time {
		t, _ := time.Parse(time.RFC3339Nano, "2026-11-24T12:00:00Z")
		return t
	}

	pane := NewPreviewPane(workItem, 50, 10)
	pane.clock = mockClock
	pane.SetEvents([]tuiclient.Event{})

	output := pane.Render()

	lines := strings.Split(output, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}

	// Check title
	if !strings.Contains(output, "Test task") {
		t.Errorf("expected title in output, got: %s", output)
	}

	// Check status line
	if !strings.Contains(output, stateChecks) {
		t.Errorf("expected state '%s' in output", stateChecks)
	}
	if !strings.Contains(output, "round 1") {
		t.Errorf("expected 'round 1' in output")
	}
	if !strings.Contains(output, "sonnet") {
		t.Errorf("expected 'sonnet' in output")
	}

	// Check time in checks
	if !strings.Contains(output, "in checks") {
		t.Errorf("expected 'in checks' in output")
	}
}

func TestPreviewPaneRender_WithPRLink(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateReview,
		Model:       "opus",
		ReviewRound: 2,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	detail := &tuiclient.TaskDetail{
		ID:    "task-123",
		Title: "Test task",
		CurrentRoundLinks: []tuiclient.TaskLink{
			{
				Kind:  "pr",
				Value: "https://github.com/owner/repo/pull/123",
			},
		},
	}

	pane := NewPreviewPane(workItem, 60, 10)
	pane.WithDetail(detail)
	pane.SetEvents([]tuiclient.Event{})

	output := pane.Render()

	if !strings.Contains(output, "PR ") {
		t.Errorf("expected PR link prefix in output")
	}
	if !strings.Contains(output, "pull/123") {
		t.Errorf("expected PR URL in output")
	}
}

func TestPreviewPaneRender_WithOneApprovedOneRejectingReviewer(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateReview,
		Model:       "sonnet",
		ReviewRound: 1,
	}

	approved := "approve"
	reviews := []*tuiclient.Task{
		{
			ID:      "review-1",
			Kind:    "review",
			Model:   "opus",
			Verdict: &approved,
		},
		{
			ID:      "review-2",
			Kind:    "review",
			Model:   "haiku",
			Verdict: nil,
		},
	}

	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   reviews,
	}

	pane := NewPreviewPane(workItem, 60, 10)
	pane.SetEvents([]tuiclient.Event{})

	output := pane.Render()

	if !strings.Contains(output, "Reviews:") {
		t.Errorf("expected 'Reviews:' in output")
	}
	if !strings.Contains(output, "opus") {
		t.Errorf("expected 'opus' reviewer in output")
	}
	if !strings.Contains(output, "approve") {
		t.Errorf("expected 'approve' verdict in output")
	}
	if !strings.Contains(output, "haiku") {
		t.Errorf("expected 'haiku' reviewer in output")
	}
	if !strings.Contains(output, "pending") {
		t.Errorf("expected 'pending' verdict in output")
	}
}

func TestPreviewPaneRender_WithHistory(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	verdict := "approve"
	events := []tuiclient.Event{
		{
			ID:        "event-1",
			TaskID:    "task-123",
			Actor:     "sonnet",
			Kind:      "submitted",
			Verdict:   nil,
			CreatedAt: "2026-11-24T12:00:00Z",
		},
		{
			ID:        "event-2",
			TaskID:    "task-123",
			Actor:     "opus",
			Kind:      "review",
			Verdict:   &verdict,
			CreatedAt: "2026-11-24T12:05:00Z",
		},
	}

	pane := NewPreviewPane(workItem, 60, 10)
	pane.SetEvents(events)
	output := pane.Render()

	// Should have history header
	if !strings.Contains(output, "History:") {
		t.Errorf("expected 'History:' in output")
	}

	// Should have events (newest first)
	if !strings.Contains(output, "review") {
		t.Errorf("expected 'review' event in output")
	}
	if !strings.Contains(output, "submitted") {
		t.Errorf("expected 'submitted' event in output")
	}

	// Should have verdict
	if !strings.Contains(output, "approve") {
		t.Errorf("expected 'approve' verdict in output")
	}
}

func TestPreviewPaneRender_HistoryLoading(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 60, 10)
	// Don't call SetEvents - leave it in loading state
	output := pane.Render()

	if !strings.Contains(output, "History: loading...") {
		t.Errorf("expected 'History: loading...' in output when events not yet loaded")
	}
}

func TestPreviewPaneRender_HistoryLoadedEmpty(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 60, 10)
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()

	if strings.Contains(output, "History: loading...") {
		t.Errorf("expected no loading message when events are loaded but empty")
	}
	if strings.Contains(output, "History:") {
		t.Errorf("expected no History header when events are empty")
	}
}

func TestPreviewPaneRender_RespectHeight(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "A very long title that will wrap to multiple lines to test height limits on the preview pane render function",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	events := make([]tuiclient.Event, 20)
	for i := 0; i < 20; i++ {
		events[i] = tuiclient.Event{
			ID:        "event-" + string(rune(i)),
			TaskID:    "task-123",
			Actor:     "actor",
			Kind:      "event_kind",
			CreatedAt: "2026-11-24T12:00:00Z",
		}
	}

	pane := NewPreviewPane(workItem, 60, 5)
	pane.SetEvents(events)
	output := pane.Render()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 5 {
		t.Errorf("expected at most 5 lines (height), got %d lines:\n%v", len(lines), lines)
	}
}

func TestPreviewPaneRender_RespectWidth(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "A very long title that should wrap to fit within the width constraint of the preview pane renderer",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 30, 20)
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()

	lines := strings.Split(output, "\n")
	for i, line := range lines {
		width := runewidth.StringWidth(line)
		if width > 30 {
			t.Errorf("line %d exceeds width of 30: got %d display cells: %s", i, width, line)
		}
	}
}

func TestPreviewPaneRender_RespectWidthNarrow(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "sonnet",
		ReviewRound: 1,
		ChecksStartedAt: func() *string {
			s := "2026-11-24T12:00:00Z"
			return &s
		}(),
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	mockClock := func() time.Time {
		t, _ := time.Parse(time.RFC3339Nano, "2026-11-24T12:18:00Z")
		return t
	}

	pane := NewPreviewPane(workItem, 12, 20)
	pane.clock = mockClock
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()

	lines := strings.Split(output, "\n")
	for i, line := range lines {
		width := runewidth.StringWidth(line)
		if width > 12 {
			t.Errorf("line %d exceeds width of 12: got %d display cells: %s", i, width, line)
		}
	}
}

func TestPreviewPaneRender_EmptyWidthHeight(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 0, 10)
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()
	if output != "" {
		t.Errorf("expected empty output for zero width, got: %s", output)
	}

	pane = NewPreviewPane(workItem, 60, 0)
	pane.SetEvents([]tuiclient.Event{})
	output = pane.Render()
	if output != "" {
		t.Errorf("expected empty output for zero height, got: %s", output)
	}
}

func TestPreviewPaneRender_MostRecentEventsNewestFirst(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	events := []tuiclient.Event{
		{
			ID:        "event-1",
			TaskID:    "task-123",
			Kind:      "first",
			CreatedAt: "2026-11-24T12:00:00Z",
		},
		{
			ID:        "event-2",
			TaskID:    "task-123",
			Kind:      "second",
			CreatedAt: "2026-11-24T12:05:00Z",
		},
		{
			ID:        "event-3",
			TaskID:    "task-123",
			Kind:      "third",
			CreatedAt: "2026-11-24T12:10:00Z",
		},
	}

	pane := NewPreviewPane(workItem, 60, 20)
	pane.SetEvents(events)
	output := pane.Render()

	// Find positions of events in output
	thirdPos := strings.Index(output, "third")
	secondPos := strings.Index(output, "second")
	firstPos := strings.Index(output, "first")

	if thirdPos == -1 || secondPos == -1 || firstPos == -1 {
		t.Fatalf("not all events found in output: %s", output)
	}

	// Newest (third) should appear before older (second) and (first)
	if thirdPos > secondPos || thirdPos > firstPos {
		t.Errorf("expected newest event (third) to appear first in output")
	}
	if secondPos > firstPos {
		t.Errorf("expected second newest event (second) to appear before oldest (first)")
	}
}

func TestPreviewPaneRender_PaneTooShortForAllEvents(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	events := make([]tuiclient.Event, 100)
	for i := 0; i < 100; i++ {
		events[i] = tuiclient.Event{
			ID:        fmt.Sprintf("event-%d", i),
			TaskID:    "task-123",
			Kind:      fmt.Sprintf("kind_%d", i),
			CreatedAt: "2026-11-24T12:00:00Z",
		}
	}

	pane := NewPreviewPane(workItem, 60, 5)
	pane.SetEvents(events)
	output := pane.Render()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 5 {
		t.Errorf("expected at most 5 lines, got %d", len(lines))
	}

	// Check that newest events are kept, not oldest
	if !strings.Contains(output, "History:") {
		t.Errorf("expected History header in output")
	}
	// Newest event (kind_99) should be present
	if !strings.Contains(output, "kind_99") {
		t.Errorf("expected newest event (kind_99) in output, got: %s", output)
	}
	// Oldest event (kind_0) should not be present due to height limit
	if strings.Contains(output, "kind_0") {
		t.Errorf("expected oldest event (kind_0) to be dropped from output due to height limit")
	}
}

func TestFormatEventTime(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"2026-11-24T12:34:56Z", "12:34"},
		{"2026-11-24T00:00:00Z", "00:00"},
		{"invalid", "00:00"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := formatEventTime(tt.input)
			if result != tt.expected {
				t.Errorf("formatEventTime(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestPreviewPaneRender_AllWidthsRespected(t *testing.T) {
	tests := []struct {
		name              string
		scenario          func() (*tuiview.WorkItem, *tuiclient.TaskDetail, []tuiclient.Event)
		widths            []int
		contentChecksWide []string // required content at all widths
		contentChecksNorm []string // required content at normal widths (12+)
	}{
		{
			name: "checks scenario with narrow widths",
			scenario: func() (*tuiview.WorkItem, *tuiclient.TaskDetail, []tuiclient.Event) {
				now := "2026-11-24T12:00:00Z"
				impl := &tuiclient.Task{
					ID:              "task-1",
					Title:           "修复中文标题的预览窗格宽度问题",
					State:           stateChecks,
					Model:           "gpt-5.6-sol",
					ReviewRound:     12,
					ChecksStartedAt: &now,
				}
				return &tuiview.WorkItem{Implement: impl, Reviews: []*tuiclient.Task{}}, nil, []tuiclient.Event{}
			},
			widths:            []int{12, 20},
			contentChecksWide: []string{"in checks", "round 12"}, // always present
			contentChecksNorm: []string{"18m"},                   // present at width 12+
		},
		{
			name: "review scenario with verdicts",
			scenario: func() (*tuiview.WorkItem, *tuiclient.TaskDetail, []tuiclient.Event) {
				impl := &tuiclient.Task{
					ID:          "task-2",
					Title:       "Review task title",
					State:       stateReview,
					Model:       "sonnet",
					ReviewRound: 1,
				}
				approved := "approve"
				reviews := []*tuiclient.Task{
					{ID: "r1", Model: "opus", Verdict: &approved},
					{ID: "r2", Model: "haiku", Verdict: nil},
				}
				return &tuiview.WorkItem{Implement: impl, Reviews: reviews}, nil, []tuiclient.Event{}
			},
			widths:            []int{5, 12, 30},
			contentChecksWide: []string{"opus", "haiku"},      // models always present
			contentChecksNorm: []string{"approve", "pending"}, // verdicts at width 12+
		},
		{
			name: "with PR link",
			scenario: func() (*tuiview.WorkItem, *tuiclient.TaskDetail, []tuiclient.Event) {
				impl := &tuiclient.Task{
					ID:          "task-3",
					Title:       "PR link test",
					State:       stateInProgress,
					Model:       "haiku",
					ReviewRound: 1,
				}
				detail := &tuiclient.TaskDetail{
					ID:    "task-3",
					Title: "PR link test",
					CurrentRoundLinks: []tuiclient.TaskLink{
						{Kind: "pr", Value: "https://github.com/owner/repo/pull/123456789"},
					},
				}
				return &tuiview.WorkItem{Implement: impl, Reviews: []*tuiclient.Task{}}, detail, []tuiclient.Event{}
			},
			widths:            []int{12, 30},
			contentChecksWide: []string{"PR"}, // prefix always present
			contentChecksNorm: []string{"pull"},
		},
		{
			name: "with events loading and loaded",
			scenario: func() (*tuiview.WorkItem, *tuiclient.TaskDetail, []tuiclient.Event) {
				impl := &tuiclient.Task{
					ID:          "task-4",
					Title:       "Events test",
					State:       stateInProgress,
					Model:       "sonnet",
					ReviewRound: 1,
				}
				events := []tuiclient.Event{
					{ID: "e1", Kind: "submitted, PR opened, entered checks", CreatedAt: "2026-11-24T12:00:00Z"},
					{ID: "e2", Kind: "claimed by @c4wqd", CreatedAt: "2026-11-24T12:05:00Z"},
				}
				return &tuiview.WorkItem{Implement: impl, Reviews: []*tuiclient.Task{}}, nil, events
			},
			widths:            []int{30, 60},
			contentChecksWide: []string{"History"}, // header always present
			contentChecksNorm: []string{"submitted"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workItem, detail, events := tt.scenario()
			mockClock := func() time.Time {
				t, _ := time.Parse(time.RFC3339Nano, "2026-11-24T12:18:00Z")
				return t
			}

			for _, width := range tt.widths {
				t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
					pane := NewPreviewPane(workItem, width, 20)
					pane.clock = mockClock
					if detail != nil {
						pane.WithDetail(detail)
					}
					if events != nil {
						pane.SetEvents(events)
					}

					output := pane.Render()
					lines := strings.Split(output, "\n")

					// Check all lines respect width
					for i, line := range lines {
						displayWidth := runewidth.StringWidth(line)
						if displayWidth > width {
							t.Errorf("line %d exceeds width %d: got %d cells: %q", i, width, displayWidth, line)
						}
						// Check that lines don't contain invalid runes (mojibake)
						for _, r := range line {
							if r == utf8.RuneError {
								t.Errorf("line %d contains invalid rune at width %d: %q", i, width, line)
							}
						}
					}

					// Check always-present content
					for _, required := range tt.contentChecksWide {
						if !strings.Contains(output, required) {
							t.Errorf("at width %d, expected required content %q not found in output:\n%s", width, required, output)
						}
					}

					// Check content at normal widths (only if there are normal-width checks defined)
					if len(tt.contentChecksNorm) > 0 && width >= 30 {
						for _, required := range tt.contentChecksNorm {
							if !strings.Contains(output, required) {
								t.Errorf("at width %d (normal), expected content %q not found in output:\n%s", width, required, output)
							}
						}
					}
				})
			}
		})
	}
}

func TestPreviewPaneRender_ChecksDurationShown(t *testing.T) {
	now := "2026-11-24T12:00:00Z"
	impl := &tuiclient.Task{
		ID:              "task-1",
		Title:           "Test task",
		State:           stateChecks,
		Model:           "sonnet",
		ReviewRound:     1,
		ChecksStartedAt: &now,
	}
	workItem := &tuiview.WorkItem{Implement: impl, Reviews: []*tuiclient.Task{}}

	mockClock := func() time.Time {
		t, _ := time.Parse(time.RFC3339Nano, "2026-11-24T12:18:00Z")
		return t
	}

	pane := NewPreviewPane(workItem, 50, 10)
	pane.clock = mockClock
	pane.SetEvents([]tuiclient.Event{})

	output := pane.Render()

	if !strings.Contains(output, "in checks") {
		t.Errorf("expected 'in checks' in output")
	}
	if !strings.Contains(output, "18m") {
		t.Errorf("expected '18m' (duration) in output, got: %s", output)
	}
}

func TestPreviewPaneRender_NewestEventsKeptWhenTruncating(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-1",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{Implement: impl, Reviews: []*tuiclient.Task{}}

	events := make([]tuiclient.Event, 10)
	for i := 0; i < 10; i++ {
		events[i] = tuiclient.Event{
			ID:        fmt.Sprintf("event-%d", i),
			Kind:      fmt.Sprintf("kind_%d", i),
			CreatedAt: "2026-11-24T12:00:00Z",
		}
	}

	pane := NewPreviewPane(workItem, 50, 4)
	pane.SetEvents(events)
	output := pane.Render()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 4 {
		t.Errorf("expected at most 4 lines, got %d", len(lines))
	}

	if !strings.Contains(output, "kind_9") {
		t.Errorf("expected newest event (kind_9) in truncated output")
	}
	if !strings.Contains(output, "History:") {
		t.Errorf("expected History header in output")
	}
}

func TestPreviewPaneView_ContentVisibleBelowPrefixWidth(t *testing.T) {
	impl := &tuiclient.Task{ID: "t", Title: "T", State: stateReview, Model: "opus", ReviewRound: 1}
	workItem := &tuiview.WorkItem{Implement: impl}
	detail := &tuiclient.TaskDetail{CurrentRoundLinks: []tuiclient.TaskLink{{Kind: "pr", Value: "https://x.io/pull/9"}}}

	for _, width := range []int{1, 2, 3, 5, 7} {
		pane := NewPreviewPane(workItem, width, 60).WithDetail(detail)
		model, _ := pane.Update(EventsLoadedMsg{Events: []tuiclient.Event{
			{Kind: "claimed", CreatedAt: "2026-11-24T12:00:00Z"},
		}})
		out := model.(*PreviewPane).View().Content

		for _, line := range strings.Split(out, "\n") {
			if w := runewidth.StringWidth(line); w > width {
				t.Errorf("width %d: line %q is %d cells", width, line, w)
			}
		}
		// Content must survive wrapping, ignoring line breaks and padding.
		squashed := strings.ReplaceAll(strings.ReplaceAll(out, "\n", ""), " ", "")
		for _, want := range []string{"https://x.io/pull/9", "claimed"} {
			if !strings.Contains(squashed, want) {
				t.Errorf("width %d: %q missing from %q", width, want, squashed)
			}
		}
	}
}

func TestPreviewPane_ShowsOnlyCurrentRoundPR(t *testing.T) {
	impl := &tuiclient.Task{ID: "t", Title: "T", State: stateReview, Model: "opus", ReviewRound: 2}
	workItem := &tuiview.WorkItem{Implement: impl}
	old := tuiclient.TaskLink{Kind: "pr", Value: "https://github.com/o/r/pull/545"}
	cur := tuiclient.TaskLink{Kind: "pr", Value: "https://github.com/o/r/pull/547"}
	detail := &tuiclient.TaskDetail{
		Links:             []tuiclient.TaskLink{old, cur},
		CurrentRoundLinks: []tuiclient.TaskLink{cur},
	}
	out := NewPreviewPane(workItem, 80, 20).WithDetail(detail).Render()
	if strings.Contains(out, "pull/545") || !strings.Contains(out, "pull/547") {
		t.Fatalf("expected only current PR, got:\n%s", out)
	}
}
