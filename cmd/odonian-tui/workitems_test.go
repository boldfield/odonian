package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
)

func wiOf(id, state string) *tuiview.WorkItem {
	return &tuiview.WorkItem{Implement: &tuiclient.Task{
		ID: id, Title: "Title " + id, State: state, Kind: "implement",
		CreatedAt: "2026-01-01T00:00:00Z",
	}}
}

func reviewWI(id string) *tuiview.WorkItem {
	wi := wiOf(id, "review")
	wi.Implement.ReviewRound = 1
	verdict := "approve"
	wi.Reviews = []*tuiclient.Task{
		{ID: id + "-r1", Kind: "review", State: "approved", Model: "opus", ReviewRound: 1, Verdict: &verdict},
	}
	return wi
}

func newTestPane(w, h int) *WorkItemsPane {
	p := NewWorkItemsPane(NewTheme())
	p.now = func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) }
	p.SetSize(w, h)
	p.SetProjectName("proj")
	return p
}

func ids(p *WorkItemsPane) []string {
	var out []string
	for _, wi := range p.items {
		out = append(out, wi.Implement.ID)
	}
	return out
}

func assertBounds(t *testing.T, out string, w, h int) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) > h {
		t.Errorf("height %d > %d:\n%s", len(lines), h, out)
	}
	for _, l := range lines {
		if lipgloss.Width(l) > w {
			t.Errorf("line width %d > %d: %q", lipgloss.Width(l), w, l)
		}
	}
}

func TestWorkItemsOrdering(t *testing.T) {
	p := newTestPane(80, 30)
	items := []*tuiview.WorkItem{
		wiOf("failed1", "failed"),
		wiOf("backlog", "backlog"),
		wiOf("ready-waiting", "ready"),
		wiOf("approved", "approved"),
		wiOf("review", "review"),
		wiOf("progress", "in_progress"),
		wiOf("blocked", "blocked"),
		wiOf("ready-ok", "ready"),
		wiOf("checks", "checks"),
		wiOf("done", "done"),
	}
	p.SetItems(items, map[string]bool{"ready-ok": true})
	want := []string{"checks", "progress", "review", "approved", "ready-ok", "ready-waiting", "backlog", "blocked", "failed1"}
	// ready-ok and ready-waiting share a group: ties fall to id order.
	got := ids(p)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("order\n got %v\nwant %v", got, want)
	}
}

func TestWorkItemsPriorityWithinGroup(t *testing.T) {
	p := newTestPane(80, 30)
	hi, lo := int64(900), int64(100)
	a, b, c := wiOf("a", "ready"), wiOf("b", "ready"), wiOf("c", "ready")
	a.Implement.Priority = &lo
	b.Implement.Priority = &hi
	p.SetItems([]*tuiview.WorkItem{a, b, c}, map[string]bool{"a": true, "b": true, "c": true})
	if got := fmt.Sprint(ids(p)); got != "[b c a]" {
		t.Errorf("got %s", got)
	}
}

func TestWorkItemsSelectionKeptByID(t *testing.T) {
	p := newTestPane(80, 30)
	claim := map[string]bool{"a": true, "b": true, "c": true, "z": true}
	p.SetItems([]*tuiview.WorkItem{wiOf("a", "ready"), wiOf("b", "ready"), wiOf("c", "ready")}, claim)
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if p.SelectedTaskID() != "b" {
		t.Fatalf("selected %q", p.SelectedTaskID())
	}
	// A new item sorts ahead of b; b stays selected.
	cmd := p.SetItems([]*tuiview.WorkItem{wiOf("a", "ready"), wiOf("b", "ready"), wiOf("c", "ready"), wiOf("0", "checks")}, claim)
	if p.SelectedTaskID() != "b" {
		t.Errorf("selected %q, want b", p.SelectedTaskID())
	}
	if cmd != nil {
		t.Errorf("no selection change, expected no message")
	}
}

func TestWorkItemsNearestRowFallback(t *testing.T) {
	p := newTestPane(80, 30)
	claim := map[string]bool{"a": true, "b": true, "c": true}
	p.SetItems([]*tuiview.WorkItem{wiOf("a", "ready"), wiOf("b", "ready"), wiOf("c", "ready")}, claim)
	p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	cmd := p.SetItems([]*tuiview.WorkItem{wiOf("a", "ready"), wiOf("c", "ready")}, claim)
	if p.SelectedTaskID() != "c" {
		t.Errorf("selected %q, want c (same index)", p.SelectedTaskID())
	}
	if cmd == nil {
		t.Fatal("expected selection message")
	}
	if msg, ok := cmd().(WorkItemSelected); !ok || msg.TaskID != "c" {
		t.Errorf("msg %v", msg)
	}
	// Selected last row disappears: clamp to new last.
	p.SetItems([]*tuiview.WorkItem{wiOf("a", "ready")}, claim)
	if p.SelectedTaskID() != "a" {
		t.Errorf("selected %q, want a", p.SelectedTaskID())
	}
}

func TestWorkItemsEmptyState(t *testing.T) {
	p := newTestPane(40, 5)
	p.SetItems(nil, nil)
	out := p.render()
	if !strings.Contains(out, "proj · 0 open") || !strings.Contains(out, "Nothing open") {
		t.Errorf("empty state:\n%s", out)
	}
	if p.SelectedTaskID() != "" {
		t.Errorf("selected %q", p.SelectedTaskID())
	}
	assertBounds(t, out, 40, 5)
}

func TestWorkItemsMoreLine(t *testing.T) {
	p := newTestPane(60, 4) // header + 3 body lines
	var items []*tuiview.WorkItem
	for i := 0; i < 6; i++ {
		items = append(items, wiOf(fmt.Sprintf("t%d", i), "backlog"))
	}
	p.SetItems(items, nil)
	out := p.render()
	// 2 rows + the more line
	if !strings.Contains(out, "↓ 4 more") {
		t.Errorf("missing more line:\n%s", out)
	}
	assertBounds(t, out, 60, 4)
	if !strings.Contains(out, "proj · 6 open") {
		t.Errorf("header:\n%s", out)
	}
}

func TestWorkItemsScrollWithTwoLineRows(t *testing.T) {
	p := newTestPane(70, 6) // header + 5 body lines
	items := []*tuiview.WorkItem{reviewWI("r1"), reviewWI("r2"), reviewWI("r3"), wiOf("b1", "backlog")}
	p.SetItems(items, nil)
	assertBounds(t, p.render(), 70, 6)
	// Body 5 lines: r1 (2) + r2 (2) fit, then more-line reserves one: 2+2+1.
	if !strings.Contains(p.render(), "↓ 2 more") {
		t.Errorf("expected 2 more:\n%s", p.render())
	}
	for i := 0; i < 2; i++ {
		p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if p.SelectedTaskID() != "r3" {
		t.Fatalf("selected %q", p.SelectedTaskID())
	}
	out := p.render()
	assertBounds(t, out, 70, 6)
	if !strings.Contains(out, "r3") {
		t.Errorf("selected row not visible:\n%s", out)
	}
	if !strings.Contains(out, "opus") {
		t.Errorf("review line of selected row cut off:\n%s", out)
	}
	if p.scroll == 0 {
		t.Errorf("expected scroll > 0")
	}
	// Scroll back up.
	for i := 0; i < 2; i++ {
		p.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if p.scroll != 0 {
		t.Errorf("scroll %d, want 0", p.scroll)
	}
}

func TestWorkItemsBoundsSweep(t *testing.T) {
	items := []*tuiview.WorkItem{reviewWI("r1"), wiOf("b1", "backlog"), reviewWI("r2"), wiOf("x", "blocked")}
	for w := 1; w <= 50; w += 7 {
		for h := 1; h <= 9; h++ {
			p := newTestPane(w, h)
			p.SetItems(items, nil)
			for i := 0; i < 4; i++ {
				assertBounds(t, p.render(), w, h)
				p.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			}
		}
	}
}

func TestWorkItemsEmptyRefreshEmitsClearedSelection(t *testing.T) {
	p := newTestPane(80, 10)
	p.SetItems([]*tuiview.WorkItem{wiOf("a", "ready")}, map[string]bool{"a": true})
	cmd := p.SetItems(nil, nil)
	if cmd == nil {
		t.Fatal("expected a selection message when the last item disappears")
	}
	msg, ok := cmd().(WorkItemSelected)
	if !ok || msg.TaskID != "" {
		t.Errorf("got %#v, want WorkItemSelected with empty TaskID", msg)
	}
}
