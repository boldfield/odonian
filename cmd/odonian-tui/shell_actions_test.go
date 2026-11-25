package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
)

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
	f.resetCalls()
	_, cmd := f.model.Update(key("r"))
	if cmd == nil {
		t.Fatal("r in projects pane returned no refetch command")
	}
	f.send(f.run(cmd)...)
	var listed []string
	for _, c := range f.listCalls() {
		listed = append(listed, c.state)
	}
	states := strings.Join(listed, ",")
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
