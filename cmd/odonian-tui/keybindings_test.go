package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// squeeze collapses runs of spaces, which the full list uses to align its columns.
func squeeze(s string) string { return strings.Join(strings.Fields(s), " ") }

// lastLine is the bottom row of a rendered view: the help bar.
func lastLine(view string) string {
	lines := strings.Split(view, "\n")
	return lines[len(lines)-1]
}

func TestShellHelpBarReviewItemWidths(t *testing.T) {
	for _, w := range []int{60, 100, 160} {
		f := newActionFixture(t)
		f.send(tea.WindowSizeMsg{Width: w, Height: 40})
		f.selectItem(t, "t-review")
		bar := lastLine(f.model.View().Content)
		if got := ansi.StringWidth(bar); got > w {
			t.Errorf("width %d: bar is %d columns wide: %q", w, got, bar)
		}
		if !strings.HasPrefix(bar, "a approve • x reject") {
			t.Errorf("width %d: bar must start with approve then reject, got %q", w, bar)
		}
	}
}

func TestShellHelpBarTruncatesWithEllipsis(t *testing.T) {
	f := newActionFixture(t)
	f.send(tea.WindowSizeMsg{Width: 40, Height: 40})
	f.selectItem(t, "t-review")
	bar := lastLine(f.model.View().Content)
	if !strings.HasSuffix(bar, "…") || ansi.StringWidth(bar) > 40 {
		t.Errorf("narrow bar should end in an ellipsis within 40 columns, got %q", bar)
	}
}

func TestShellHelpBarBlockedItem(t *testing.T) {
	f := newActionFixture(t)
	f.selectItem(t, "t-blocked")
	bar := lastLine(f.model.View().Content)
	for _, want := range []string{"u unblock", "f fail"} {
		if !strings.Contains(bar, want) {
			t.Errorf("blocked bar lacks %q: %q", want, bar)
		}
	}
	if strings.Contains(bar, "approve") {
		t.Errorf("blocked bar must not offer approve: %q", bar)
	}
}

func TestShellHelpBarApprovedItemListsMergeFirst(t *testing.T) {
	f := newActionFixture(t)
	f.send(tea.WindowSizeMsg{Width: 60, Height: 40})
	f.selectItem(t, "t-approved")
	if bar := lastLine(f.model.View().Content); !strings.HasPrefix(bar, "m merge") {
		t.Errorf("approved bar must start with merge, got %q", bar)
	}
}

func TestShellHelpBarPerPane(t *testing.T) {
	f := newActionFixture(t)
	f.selectItem(t, "t-review")
	items := f.model.shellKeyHints()
	for _, want := range []string{"tab pane", "v preview", "V projects"} {
		if !strings.Contains(items, want) {
			t.Errorf("items bar lacks %q: %q", want, items)
		}
	}
	f.send(key("shift+tab"))
	projects := f.model.shellKeyHints()
	if !strings.Contains(projects, "enter open") || !strings.Contains(projects, "z archive project") || strings.Contains(projects, "approve") {
		t.Errorf("projects bar wrong: %q", projects)
	}
	f.press("tab", "tab")
	preview := f.model.shellKeyHints()
	if strings.Contains(preview, "approve") || strings.Contains(preview, "open") || !strings.Contains(preview, "↑/↓") {
		t.Errorf("preview bar wrong: %q", preview)
	}
}

func TestShellFullHelpToggle(t *testing.T) {
	f := newActionFixture(t)
	f.selectItem(t, "t-review")
	before := f.model.View().Content

	f.press("?")
	raw := f.model.View().Content
	open := squeeze(raw)
	for _, want := range []string{"a approve", "x reject", "tab pane", "v preview", "V projects", "q quit", "? help"} {
		if !strings.Contains(open, want) {
			t.Errorf("full list lacks %q:\n%s", want, open)
		}
	}
	if strings.Count(raw, "\n") < 5 {
		t.Errorf("full list should span several rows:\n%s", open)
	}
	if strings.Contains(open, "u unblock") || strings.Contains(open, "left") {
		t.Errorf("full list shows keys that do nothing here:\n%s", open)
	}

	// Other keys are swallowed while the list is open.
	f.press("a", "x", "z", "down", "v", "tab")
	if f.model.mode != modeNormal || f.model.shell.previewHidden || f.model.shell.focus != paneItems {
		t.Errorf("keys leaked through the open list: mode %d previewHidden %v focus %d", f.model.mode, f.model.shell.previewHidden, f.model.shell.focus)
	}
	if f.model.shell.items.SelectedTaskID() != "t-review" {
		t.Errorf("selection moved behind the list: %s", f.model.shell.items.SelectedTaskID())
	}

	f.press("?")
	if after := f.model.View().Content; after != before {
		t.Errorf("second ? should restore the shell view\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestShellFullHelpFollowsFocusedPane(t *testing.T) {
	f := newActionFixture(t)
	f.send(key("shift+tab"))
	f.press("?")
	out := squeeze(f.model.View().Content)
	if !strings.Contains(out, "enter open") || !strings.Contains(out, "z archive project") {
		t.Errorf("projects-pane list lacks project keys:\n%s", out)
	}
	if strings.Contains(out, "approve") {
		t.Errorf("projects-pane list shows item keys:\n%s", out)
	}
}

func TestShellQuitStillWorksUnderFullHelp(t *testing.T) {
	f := newActionFixture(t)
	f.press("?")
	_, cmd := f.model.Update(key("q"))
	if cmd == nil {
		t.Fatal("q under the full list should quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q under the full list did not produce a quit")
	}
}

func TestShellKeysComeFromBindings(t *testing.T) {
	f := newActionFixture(t)
	// Rebind approve: the new key acts and the help shows it; the old key does nothing.
	for i := range f.model.keyBindings.Shell {
		e := &f.model.keyBindings.Shell[i]
		if e.act == actApprove {
			e.bind.SetKeys("y")
			e.bind.SetHelp("y", "approve")
		}
	}
	f.selectItem(t, "t-review")
	if bar := lastLine(f.model.View().Content); !strings.HasPrefix(bar, "y approve") {
		t.Errorf("help should show the rebound key: %q", bar)
	}
	f.press("a")
	if f.model.mode != modeNormal {
		t.Error("the old key still approves after rebinding")
	}
	f.press("y")
	if f.model.mode != modeApproveNote {
		t.Errorf("the rebound key did not start an approval, mode %d", f.model.mode)
	}
}

func TestShellDoesNotOpenProjectSwitcherOrColumns(t *testing.T) {
	f := newActionFixture(t)
	for _, k := range []string{"P", "left", "right", "h", "l"} {
		f.send(key(k))
		if f.model.mode != modeNormal {
			t.Errorf("%s changed mode to %d", k, f.model.mode)
		}
	}
}

func TestShellKeepsVimAliases(t *testing.T) {
	f := newActionFixture(t)
	first := f.model.shell.items.SelectedTaskID()
	f.press("j")
	if f.model.shell.items.SelectedTaskID() == first {
		t.Error("j should move the selection down")
	}
	f.press("k")
	if f.model.shell.items.SelectedTaskID() != first {
		t.Error("k should move the selection back up")
	}
}

func detailFixture(t *testing.T, id string) *boardFixture {
	f := newActionFixture(t)
	f.selectItem(t, id)
	f.press("enter")
	if f.model.mode != modeDetail {
		t.Fatalf("enter did not open the detail view, mode %d", f.model.mode)
	}
	return f
}

func TestDetailFullHelpToggle(t *testing.T) {
	f := detailFixture(t, "t-review")
	before := f.model.View().Content
	f.press("?")
	open := squeeze(f.model.View().Content)
	for _, want := range []string{"a approve", "x reject", "esc back", "i set priority", "o open PR"} {
		if !strings.Contains(open, want) {
			t.Errorf("detail full list lacks %q:\n%s", want, open)
		}
	}
	if strings.Contains(open, "switch project") {
		t.Errorf("the shell has no project switcher:\n%s", open)
	}
	f.press("a", "esc")
	if f.model.mode != modeDetail {
		t.Errorf("keys leaked through the open list, mode %d", f.model.mode)
	}
	f.press("?")
	if after := f.model.View().Content; after != before {
		t.Errorf("second ? should restore the detail view\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDetailHelpBarFollowsState(t *testing.T) {
	f := detailFixture(t, "t-review")
	bar := lastLine(f.model.View().Content)
	if !strings.HasPrefix(bar, "a approve • x reject") {
		t.Errorf("review detail bar must start with approve, reject: %q", bar)
	}
	f = detailFixture(t, "t-blocked")
	if bar := lastLine(f.model.View().Content); strings.Contains(bar, "approve") || strings.Contains(bar, "merge") {
		t.Errorf("blocked detail bar offers other states' keys: %q", bar)
	}
}

// Every binding a context lists must resolve, in that context, to its own action.
func TestListedBindingsResolveToTheirActions(t *testing.T) {
	kb := DefaultKeyBindings()
	cases := map[string][]keyCtx{"Detail": {{state: stateReview}, {state: stateApproved}}, "Shell": {}}
	for _, p := range []shellPane{paneItems, paneProjects, panePreview} {
		for _, s := range []string{stateBacklog, stateReview, stateApproved, stateBlocked, "ready", ""} {
			cases["Shell"] = append(cases["Shell"], keyCtx{pane: p, state: s})
		}
	}
	maps := map[string]keyMap{"Detail": kb.Detail, "Shell": kb.Shell}
	for name, ctxs := range cases {
		for _, c := range ctxs {
			for _, e := range maps[name] {
				if !e.valid(c) || e.bind.Help().Key == "" {
					continue
				}
				for _, k := range e.bind.Keys() {
					got, ok := maps[name].lookup(key2msg(k), c)
					if !ok || got != e.act {
						t.Errorf("%s %+v: key %q lists %s but resolves to %q (ok=%v)", name, c, k, e.act, got, ok)
					}
				}
			}
		}
	}
}

func key2msg(s string) tea.KeyPressMsg {
	switch s {
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "pgup":
		return tea.KeyPressMsg{Code: tea.KeyPgUp}
	case "pgdown":
		return tea.KeyPressMsg{Code: tea.KeyPgDown}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	case "left":
		return tea.KeyPressMsg{Code: tea.KeyLeft}
	case "right":
		return tea.KeyPressMsg{Code: tea.KeyRight}
	}
	return key(s)
}

func TestShellNavigationComesFromBindings(t *testing.T) {
	f := newActionFixture(t)
	for i := range f.model.keyBindings.Shell {
		e := &f.model.keyBindings.Shell[i]
		if e.act == actDown {
			e.bind.SetKeys("y")
		}
	}
	first := f.model.shell.items.SelectedTaskID()
	f.press("j")
	if f.model.shell.items.SelectedTaskID() != first {
		t.Error("the old key still moves after rebinding")
	}
	f.press("y")
	if f.model.shell.items.SelectedTaskID() == first {
		t.Error("the rebound key did not move the selection")
	}
}

func TestShellEnterOpensDetailFromPreviewPane(t *testing.T) {
	f := newActionFixture(t)
	f.selectItem(t, "t-review")
	f.press("tab")
	if f.model.shell.focus != panePreview {
		t.Fatalf("focus = %v, want preview", f.model.shell.focus)
	}
	f.press("enter")
	if f.model.mode != modeDetail {
		t.Errorf("mode = %d, want detail", f.model.mode)
	}
}
