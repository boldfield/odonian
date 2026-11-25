package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
	"github.com/charmbracelet/x/ansi"
)

type shellFixture struct {
	model *BoardModel
	mu    sync.Mutex
	// listed holds the tasks per project; eventCalls records ListEvents task ids.
	listed     map[string][]tuiclient.Task
	eventCalls []string
}

func newShellFixture(t *testing.T, width, height int) *shellFixture {
	t.Helper()
	f := &shellFixture{listed: map[string][]tuiclient.Task{
		"p1": {
			{ID: "a1", Title: "Alpha", State: "in_progress", Kind: "implement", Model: "sonnet", CreatedAt: "2026-01-01T00:00:00Z"},
			{ID: "a2", Title: "Bravo", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"},
			{ID: "a3", Title: "Charlie", State: "blocked", Kind: "implement", CreatedAt: "2026-01-01T00:00:02Z"},
		},
		"p2": {
			{ID: "b1", Title: "Delta", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
		},
	}}
	client := &tuiclient.MockClient{
		ListTasksFunc: func(ctx context.Context, projectID string, opts ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			var o tuiclient.TaskListOptions
			for _, opt := range opts {
				opt(&o)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			var out []tuiclient.Task
			for _, task := range f.listed[projectID] {
				if o.State != "" && task.State != o.State {
					continue
				}
				if o.Claimable && task.State != "ready" {
					continue
				}
				out = append(out, task)
			}
			return out, nil
		},
		ListEventsFunc: func(ctx context.Context, taskID string) ([]tuiclient.Event, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.eventCalls = append(f.eventCalls, taskID)
			return nil, nil
		},
	}
	cfg := &tuiconfig.Config{URL: "http://test", Token: "t", Actor: "u", PollInterval: time.Hour}
	f.model = NewShellBoardModel(client, cfg, tuiclient.Project{ID: "p1", Name: "One"})
	f.model.shell.previewDebounce = 0
	f.model.newTickCmd = func() tea.Cmd { return nil }
	f.send(tea.WindowSizeMsg{Width: width, Height: height})
	f.send(projectsMsg())
	f.send(f.run(f.model.fetchShellTasks())...)
	return f
}

func projectsMsg() tea.Msg {
	return projectsFetchedMsg{projects: []tuiclient.Project{{ID: "p1", Name: "One"}, {ID: "p2", Name: "Two"}}}
}

// run executes a command and returns its message(s), flattening batches.
func (f *shellFixture) run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, f.run(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// send delivers messages and every message their commands produce, until quiet.
func (f *shellFixture) send(msgs ...tea.Msg) {
	queue := append([]tea.Msg{}, msgs...)
	for len(queue) > 0 {
		msg := queue[0]
		queue = queue[1:]
		_, cmd := f.model.Update(msg)
		queue = append(queue, f.run(cmd)...)
	}
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

func assertWidth(t *testing.T, out string, w int) {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if lipgloss.Width(l) > w {
			t.Errorf("line width %d > %d: %q", lipgloss.Width(l), w, l)
		}
	}
}

func TestShellLayoutByWidth(t *testing.T) {
	cases := []struct {
		width                         int
		wantProjects, wantPreview, ok bool
	}{
		{120, true, true, true},
		{100, true, false, true},
		{70, false, false, true},
	}
	for _, c := range cases {
		f := newShellFixture(t, c.width, 24)
		out := f.model.View().Content
		assertWidth(t, out, c.width)
		if got := strings.Contains(out, "PROJ"); got != c.wantProjects {
			t.Errorf("width %d: projects pane shown=%v want %v\n%s", c.width, got, c.wantProjects, out)
		}
		// The preview shows the selected item's status line; the items pane never does.
		if got := strings.Contains(out, "· round"); got != c.wantPreview {
			t.Errorf("width %d: preview shown=%v want %v\n%s", c.width, got, c.wantPreview, out)
		}
		if !strings.Contains(out, "Alpha") {
			t.Errorf("width %d: items missing\n%s", c.width, out)
		}
		if strings.Count(out, "│") == 0 && c.width >= 80 {
			t.Errorf("width %d: no separators", c.width)
		}
	}
}

func TestShellResizeChangesLayoutLive(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	if !strings.Contains(f.model.View().Content, "· round") {
		t.Fatal("expected preview at 120")
	}
	f.send(tea.WindowSizeMsg{Width: 70, Height: 24})
	out := f.model.View().Content
	if strings.Contains(out, "· round") || strings.Contains(out, "PROJ") {
		t.Errorf("expected items only at 70:\n%s", out)
	}
	assertWidth(t, out, 70)
}

func TestShellNoLineExceedsWidth(t *testing.T) {
	for _, w := range []int{40, 55, 79, 80, 99, 119, 120, 160} {
		f := newShellFixture(t, w, 20)
		out := f.model.View().Content
		assertWidth(t, out, w)
		if n := len(strings.Split(out, "\n")); n > 20 {
			t.Errorf("width %d: %d lines > 20", w, n)
		}
	}
}

func TestShellTabCyclesFocus(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	s := f.model.shell
	if s.focus != paneItems {
		t.Fatalf("initial focus = %v", s.focus)
	}
	want := []shellPane{panePreview, paneProjects, paneItems}
	for i, w := range want {
		f.send(key("tab"))
		if s.focus != w {
			t.Errorf("tab %d focus = %v, want %v", i+1, s.focus, w)
		}
	}
	f.send(key("shift+tab"))
	if s.focus != paneProjects {
		t.Errorf("after shift+tab focus = %v, want projects", s.focus)
	}

	// With only the items pane visible, tab has nowhere to go.
	f.send(tea.WindowSizeMsg{Width: 70, Height: 24})
	f.send(key("tab"))
	if got := f.model.visiblePanes(f.model.shellLayout()); len(got) != 1 {
		t.Errorf("visible panes at 70 = %v", got)
	}
	if s.focus != paneItems {
		t.Errorf("focus at 70 = %v, want items", s.focus)
	}
}

func TestShellPreviewFocusRendersAndTakesArrows(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	s := f.model.shell
	bold := func(text string) string { return s.preview.theme.PaneTitleFocused.Render(text) }
	if strings.Contains(f.model.View().Content, bold("Alpha")) {
		t.Fatal("preview title is bold before it has focus")
	}
	var events []tuiclient.Event
	for i := 0; i < 60; i++ {
		events = append(events, tuiclient.Event{ID: fmt.Sprint(i), TaskID: "a1", Kind: "note", CreatedAt: fmt.Sprintf("2026-01-01T00:%02d:00Z", i)})
	}
	s.preview.SetEvents(events)

	f.send(key("tab"))
	if s.focus != panePreview {
		t.Fatalf("focus = %v, want preview", s.focus)
	}
	atTop := f.model.View().Content
	if !strings.Contains(atTop, bold("Alpha")) {
		t.Error("focused preview title is not bold")
	}
	sel := s.items.SelectedTaskID()
	f.send(key("down"))
	f.send(key("down"))
	if s.items.SelectedTaskID() != sel {
		t.Error("down moved the item selection while the preview was focused")
	}
	if s.preview.scroll != 2 {
		t.Fatalf("scroll = %d, want 2", s.preview.scroll)
	}
	scrolled := f.model.View().Content
	if scrolled == atTop {
		t.Error("rendered preview did not change after scrolling down")
	}
	if !strings.Contains(scrolled, bold("Alpha")) {
		t.Error("preview focus marker scrolled away")
	}
	for i := 0; i < 200; i++ {
		f.send(key("down"))
	}
	f.model.View() // rendering clamps the scroll to the content
	end := s.preview.scroll
	if end <= 2 {
		t.Fatalf("scroll did not advance, got %d", end)
	}
	f.send(key("down"))
	f.model.View()
	if s.preview.scroll != end {
		t.Errorf("scroll not clamped: %d then %d", end, s.preview.scroll)
	}
	if !strings.Contains(f.model.View().Content, bold("Alpha")) {
		t.Error("preview focus marker missing at the end of the scroll")
	}
	for i := 0; i < 300; i++ {
		f.send(key("up"))
	}
	if s.preview.scroll != 0 {
		t.Errorf("scroll after up = %d, want 0", s.preview.scroll)
	}
	if f.model.View().Content != atTop {
		t.Error("view after scrolling back differs from the top view")
	}
	f.send(key("v"))
	if s.focus != paneItems {
		t.Errorf("hiding the preview left focus on %v", s.focus)
	}
}

func TestShellArrowsGoToFocusedPane(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	first := f.model.shell.items.SelectedTaskID()
	f.send(key("down"))
	second := f.model.shell.items.SelectedTaskID()
	if first == second {
		t.Fatal("down did not move the item selection")
	}
	if f.model.selectedTaskID != second {
		t.Errorf("selectedTaskID = %q want %q", f.model.selectedTaskID, second)
	}
	if f.model.project.ID != "p1" {
		t.Fatal("project changed")
	}

	f.send(key("shift+tab"))
	f.send(key("down"))
	if f.model.project.ID != "p2" {
		t.Errorf("project = %q, want p2", f.model.project.ID)
	}
}

func TestShellProjectChangeLoadsItems(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	f.send(key("shift+tab"), key("down"))
	if got := f.model.shell.items.SelectedTaskID(); got != "b1" {
		t.Errorf("selected = %q, want b1", got)
	}
	out := f.model.View().Content
	if !strings.Contains(out, "Delta") || strings.Contains(out, "Alpha") {
		t.Errorf("items not replaced:\n%s", out)
	}
}

func TestShellPollKeepsSelection(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	f.send(key("down"))
	want := f.model.shell.items.SelectedTaskID()

	// A new task appears ahead of the selection, then the poll fires.
	f.mu.Lock()
	f.listed["p1"] = append([]tuiclient.Task{{ID: "a0", Title: "Zulu", State: "checks", Kind: "implement", CreatedAt: "2025-01-01T00:00:00Z"}}, f.listed["p1"]...)
	f.mu.Unlock()
	f.send(tickMsg{})
	if got := f.model.shell.items.SelectedTaskID(); got != want {
		t.Errorf("selection after poll = %q, want %q", got, want)
	}
	if !strings.Contains(f.model.View().Content, "Zulu") {
		t.Error("poll did not refresh items")
	}
}

func TestShellQuickMovesFetchEventsForFinalItem(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	f.mu.Lock()
	f.eventCalls = nil
	f.mu.Unlock()

	// Three quick moves: collect each move's debounce command without running it yet.
	var pending []tea.Msg
	for i := 0; i < 2; i++ {
		_, cmd := f.model.Update(key("down"))
		for _, m := range f.run(cmd) { // WorkItemSelected
			_, c := f.model.Update(m)
			pending = append(pending, f.run(c)...) // previewFetchMsg
		}
	}
	final := f.model.shell.items.SelectedTaskID()
	var fetches []tea.Cmd
	for _, m := range pending {
		_, c := f.model.Update(m)
		fetches = append(fetches, c)
	}
	for _, c := range fetches {
		f.send(f.run(c)...)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.eventCalls) != 1 || f.eventCalls[0] != final {
		t.Errorf("event fetches = %v, want exactly [%s]", f.eventCalls, final)
	}
}

func TestShellEnterOpensDetail(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	want := f.model.shell.items.SelectedTaskID()
	_, cmd := f.model.Update(key("enter"))
	if f.model.mode != modeDetail || f.model.selectedTaskID != want || cmd == nil {
		t.Errorf("mode=%v selected=%q cmd=%v", f.model.mode, f.model.selectedTaskID, cmd)
	}
}

func TestShellToggles(t *testing.T) {
	f := newShellFixture(t, 120, 24)
	f.send(key("v"))
	if strings.Contains(f.model.View().Content, "· round") {
		t.Error("preview still shown after v")
	}
	f.send(key("shift+tab"), key("V"))
	out := f.model.View().Content
	if strings.Contains(out, "PROJECTS") {
		t.Error("project pane still shown after V")
	}
	if f.model.shell.focus != paneItems {
		t.Error("focus left on hidden project pane")
	}
	assertWidth(t, out, 120)
}

func TestShellErrorAtNarrowWidthPreservesHint(t *testing.T) {
	f := newShellFixture(t, 60, 24)

	// Simulate a long error message with retry hint
	errorMsg := "Error: This is a very long error message that would normally exceed the terminal width and needs to be truncated carefully"
	hintMsg := "Press 'r' to retry"
	f.model.shell.errorBase = errorMsg
	f.model.shell.retryHint = hintMsg
	f.model.error = errorMsg + ". " + hintMsg

	out := f.model.View().Content
	assertWidth(t, out, 60)

	// The hint should still be present even if error text is truncated
	bar := f.model.shellHelpBar()
	if !strings.Contains(bar, "Press 'r' to retry") {
		t.Errorf("retry hint missing from error bar at narrow width: %q", bar)
	}
	if ansi.StringWidth(bar) > 60 {
		t.Errorf("error bar exceeds width: %d > 60", ansi.StringWidth(bar))
	}
}
