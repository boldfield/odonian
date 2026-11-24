package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
)

// actionFixture is the three-pane shell over a recording client, one implement task per state.
type actionFixture struct {
	model  *BoardModel
	mu     sync.Mutex
	calls  []string
	states []string // states requested by ListTasks
}

func newActionFixture(t *testing.T) *actionFixture {
	t.Helper()
	f := &actionFixture{}
	tasks := []tuiclient.Task{
		{ID: "t-backlog", Title: "Backlog", State: "backlog", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "t-ready", Title: "Ready", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"},
		{ID: "t-review", Title: "Review", State: "review", Kind: "implement", CreatedAt: "2026-01-01T00:00:02Z"},
		{ID: "t-approved", Title: "Approved", State: "approved", Kind: "implement", CreatedAt: "2026-01-01T00:00:03Z"},
		{ID: "t-blocked", Title: "Blocked", State: "blocked", Kind: "implement", CreatedAt: "2026-01-01T00:00:04Z"},
		{ID: "t-held", Title: "Held", State: "ready", Held: true, Kind: "implement", CreatedAt: "2026-01-01T00:00:05Z"},
	}
	rec := func(s string) {
		f.mu.Lock()
		f.calls = append(f.calls, s)
		f.mu.Unlock()
	}
	client := &tuiclient.MockClient{
		ListProjectsFunc: func(ctx context.Context, o ...tuiclient.ProjectListOption) ([]tuiclient.Project, error) {
			return []tuiclient.Project{{ID: "p1", Name: "One"}}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, opts ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			var o tuiclient.TaskListOptions
			for _, opt := range opts {
				opt(&o)
			}
			f.mu.Lock()
			f.states = append(f.states, o.State)
			f.mu.Unlock()
			var out []tuiclient.Task
			for _, task := range tasks {
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
		GetTaskFunc: func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{Links: []tuiclient.TaskLink{{Kind: "pr", Value: "https://github.com/o/r/pull/9"}}}, nil
		},
		PromoteTaskFunc: func(ctx context.Context, id string) error { rec("promote " + id); return nil },
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			rec("review " + verdict + " " + id)
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			rec("transition " + id + " " + to)
			return nil
		},
		ArchiveTaskFunc:    func(ctx context.Context, id string) error { rec("archive " + id); return nil },
		ArchiveProjectFunc: func(ctx context.Context, id string) error { rec("archive-project " + id); return nil },
		ReleaseTaskFunc:    func(ctx context.Context, id string) error { rec("release " + id); return nil },
		HoldTaskFunc:       func(ctx context.Context, id string) error { rec("hold " + id); return nil },
		SetTaskPriorityFunc: func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			rec("priority " + id)
			return tuiclient.PriorityChange{Priority: req.Priority}, nil
		},
		MoveTaskToFrontFunc: func(ctx context.Context, id string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
			rec("front " + id)
			return tuiclient.PriorityChange{}, nil
		},
	}
	cfg := &tuiconfig.Config{URL: "http://test", Token: "t", Actor: "u", PollInterval: time.Hour}
	f.model = NewShellBoardModel(client, cfg, tuiclient.Project{ID: "p1", Name: "One"})
	f.model.shell.previewDebounce = 0
	f.model.newTickCmd = func() tea.Cmd { return nil }
	f.model.ghMerger = func(ctx context.Context, prURL string) error { rec("merge " + prURL); return nil }
	f.send(tea.WindowSizeMsg{Width: 140, Height: 40})
	f.send(projectsMsg())
	f.send(f.run(f.model.fetchShellTasks())...)
	return f
}

func (f *actionFixture) run(cmd tea.Cmd) []tea.Msg {
	return (&shellFixture{}).run(cmd)
}

func (f *actionFixture) send(msgs ...tea.Msg) {
	queue := append([]tea.Msg{}, msgs...)
	for len(queue) > 0 {
		msg := queue[0]
		queue = queue[1:]
		_, cmd := f.model.Update(msg)
		queue = append(queue, f.run(cmd)...)
	}
}

// press sends keys; each element is one key, except text, which is typed per character.
func (f *actionFixture) press(keys ...string) {
	for _, k := range keys {
		if len(k) > 1 && k != "enter" && k != "tab" && k != "down" && k != "up" {
			for _, ch := range k {
				f.send(key(string(ch)))
			}
			continue
		}
		f.send(key(k))
	}
}

// selectItem moves the items-pane selection to the task.
func (f *actionFixture) selectItem(t *testing.T, id string) {
	t.Helper()
	for i := 0; i < 10 && f.model.shell.items.SelectedTaskID() != id; i++ {
		f.press("down")
	}
	for i := 0; i < 10 && f.model.shell.items.SelectedTaskID() != id; i++ {
		f.press("up")
	}
	if got := f.model.shell.items.SelectedTaskID(); got != id {
		t.Fatalf("could not select %s, selected %q", id, got)
	}
}

func (f *actionFixture) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "; ")
}

func TestShellActions(t *testing.T) {
	tests := []struct {
		name string
		item string
		keys []string
		want string
	}{
		{"promote", "t-backlog", []string{"p"}, "promote t-backlog"},
		{"approve", "t-review", []string{"a", "enter", "y"}, "review approve t-review; transition t-review done"},
		{"reject", "t-review", []string{"x", "no good", "enter"}, "review reject t-review; transition t-review ready"},
		{"bounce", "t-approved", []string{"b", "enter", "y"}, "transition t-approved ready"},
		{"merge", "t-approved", []string{"m", "y"}, "merge https://github.com/o/r/pull/9; transition t-approved done"},
		{"unblock", "t-blocked", []string{"u", "enter", "y"}, "transition t-blocked ready"},
		{"fail", "t-blocked", []string{"f", "enter", "y"}, "transition t-blocked failed"},
		{"archive", "t-ready", []string{"z", "y"}, "archive t-ready"},
		{"hold", "t-ready", []string{"t", "y"}, "hold t-ready"},
		{"release", "t-held", []string{"t", "y"}, "release t-held"},
		{"set priority", "t-ready", []string{"i", "750", "enter", "why", "enter", "y"}, "priority t-ready"},
		{"move to front", "t-ready", []string{"w", "why", "enter", "y"}, "front t-ready"},
		{"reset priority", "t-ready", []string{"e", "why", "enter", "y"}, "priority t-ready"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newActionFixture(t)
			f.selectItem(t, tc.item)
			f.press(tc.keys...)
			if got := f.got(); got != tc.want {
				t.Errorf("calls = %q, want %q", got, tc.want)
			}
			if f.model.mode != modeNormal {
				t.Errorf("mode = %d, want normal", f.model.mode)
			}
		})
	}
}

func TestShellActionIgnoredInWrongState(t *testing.T) {
	tests := []struct {
		item string
		key  string
	}{
		{"t-ready", "p"}, {"t-ready", "a"}, {"t-ready", "x"}, {"t-ready", "b"},
		{"t-ready", "u"}, {"t-ready", "f"}, {"t-ready", "m"}, {"t-review", "m"}, {"t-backlog", "a"},
	}
	for _, tc := range tests {
		f := newActionFixture(t)
		f.selectItem(t, tc.item)
		f.press(tc.key)
		if f.model.mode != modeNormal || f.got() != "" {
			t.Errorf("%s on %s: mode %d calls %q, want ignored", tc.key, tc.item, f.model.mode, f.got())
		}
	}
}

func TestShellActionsIgnoredOutsideItemsPane(t *testing.T) {
	for _, pane := range []string{"projects", "preview"} {
		for _, k := range []string{"p", "a", "t", "i", "w", "e", "x", "m"} {
			f := newActionFixture(t)
			f.selectItem(t, "t-review")
			f.press("tab") // items -> preview
			if pane == "projects" {
				f.press("tab") // preview -> projects
			}
			f.press(k)
			if f.model.mode != modeNormal {
				t.Errorf("%s pane: %q entered mode %d", pane, k, f.model.mode)
			}
		}
	}
	f := newActionFixture(t)
	f.selectItem(t, "t-ready")
	f.press("tab", "z")
	if f.model.mode != modeNormal {
		t.Errorf("z in preview pane entered mode %d", f.model.mode)
	}
}

func TestShellProjectsPaneArchiveProject(t *testing.T) {
	f := newActionFixture(t)
	f.press("tab", "tab") // items -> preview -> projects
	if f.model.shell.focus != paneProjects {
		t.Fatalf("focus = %d, want projects", f.model.shell.focus)
	}
	f.press("z", "y")
	if got := f.got(); got != "archive-project p1" {
		t.Errorf("calls = %q", got)
	}
}

func TestShellProjectsPaneRefreshAndScorecards(t *testing.T) {
	f := newActionFixture(t)
	f.press("tab", "tab")
	f.press("c")
	if f.model.mode != modeScorecards {
		t.Errorf("c in projects pane: mode %d, want scorecards", f.model.mode)
	}
}

func TestShellHelpBarFollowsSelectedState(t *testing.T) {
	f := newActionFixture(t)
	for item, want := range map[string]string{
		"t-backlog":  "p promote",
		"t-review":   "a approve",
		"t-approved": "m merge",
		"t-blocked":  "u unblock",
	} {
		f.selectItem(t, item)
		if bar := f.model.shellKeyHints(); !strings.Contains(bar, want) {
			t.Errorf("%s: help %q lacks %q", item, bar, want)
		}
	}
	f.selectItem(t, "t-ready")
	if bar := f.model.shellKeyHints(); strings.Contains(bar, "promote") || strings.Contains(bar, "approve") {
		t.Errorf("ready item help offers other states' keys: %q", bar)
	}
}

func TestShellPromptsRenderInsideShell(t *testing.T) {
	f := newActionFixture(t)
	f.selectItem(t, "t-approved")
	f.press("m")
	view := f.model.viewContent()
	if !strings.Contains(view, "merge https://github.com/o/r/pull/9") {
		t.Errorf("merge confirm not visible:\n%s", view)
	}
	if !strings.Contains(view, "Approved") || !strings.Contains(view, "│") {
		t.Errorf("shell panes not visible during merge confirm:\n%s", view)
	}
	f.press("n")

	f.press("tab", "tab", "z")
	view = f.model.viewContent()
	if f.model.mode != modeArchiveProjectConfirm || !strings.Contains(view, "Archive project? [y/N]") || !strings.Contains(view, "│") {
		t.Errorf("archive-project confirm not visible in shell:\n%s", view)
	}
}

func TestShellProjectsPaneRefreshAndQuit(t *testing.T) {
	f := newActionFixture(t)
	f.press("tab", "tab")
	f.mu.Lock()
	f.states = nil
	f.mu.Unlock()
	_, cmd := f.model.Update(key("r"))
	if cmd == nil {
		t.Fatal("r in projects pane returned no refetch command")
	}
	f.send(f.run(cmd)...)
	f.mu.Lock()
	states := strings.Join(f.states, ",")
	f.mu.Unlock()
	for _, want := range []string{"backlog", "ready", "review", "approved", "blocked", "done", "failed"} {
		if !strings.Contains(states, want) {
			t.Errorf("refresh did not list state %q: %s", want, states)
		}
	}
	_, cmd = f.model.Update(key("q"))
	if cmd == nil {
		t.Fatal("q returned no command")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Errorf("q did not quit")
	}
}

func TestShellStaleMergeAfterDetailNavigation(t *testing.T) {
	f := newActionFixture(t)
	f.selectItem(t, "t-approved")
	f.selectItem(t, "t-review")
	f.model.mode = modeDetail
	f.model.detailTask = tuiclient.TaskDetail{ID: "t-review"}
	f.send(prResolvedMsg{taskID: "t-approved", prURL: "https://github.com/o/r/pull/9", fromShell: true})
	if f.model.mode != modeDetail {
		t.Errorf("stale shell resolution over detail changed mode to %d", f.model.mode)
	}
}

func TestShellArchiveCurrentProjectSyncs(t *testing.T) {
	f := newActionFixture(t)
	other := tuiclient.Project{ID: "p2", Name: "Two"}
	f.send(projectArchiveMsg{projects: []tuiclient.Project{other}})
	if f.model.project.ID != "p2" {
		t.Fatalf("project = %q, want p2", f.model.project.ID)
	}
	if got := f.model.shell.projects.SelectedProjectID(); got != "p2" {
		t.Errorf("projects pane selects %q", got)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "Two") {
		t.Errorf("view does not show the new project:\n%s", view)
	}
	f.send(projectArchiveMsg{projects: nil})
	if f.model.shell.items.SelectedTaskID() != "" || f.model.mode != modeNormal {
		t.Errorf("empty project list left selection %q mode %d", f.model.shell.items.SelectedTaskID(), f.model.mode)
	}
}

func TestShellStaleMergeResolutionIgnored(t *testing.T) {
	f := newActionFixture(t)
	f.selectItem(t, "t-approved")
	f.selectItem(t, "t-ready")
	f.send(prResolvedMsg{taskID: "t-approved", prURL: "https://github.com/o/r/pull/9", fromShell: true})
	if f.model.mode != modeNormal {
		t.Errorf("stale resolution after selection change: mode %d", f.model.mode)
	}
	f.press("i")
	prompt := f.model.mode
	f.send(prResolvedMsg{taskID: "t-ready", prURL: "https://github.com/o/r/pull/9", fromShell: true})
	if f.model.mode != prompt {
		t.Errorf("resolution during another prompt changed mode to %d", f.model.mode)
	}
}
