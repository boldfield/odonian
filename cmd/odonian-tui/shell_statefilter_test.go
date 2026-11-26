package main

import (
	"github.com/charmbracelet/x/ansi"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/tuiview"
)

func TestStateFilterOverlayOpensWithNineStates(t *testing.T) {
	f := newBoardFixture(t, 60, 20, filterTasks()...)
	f.press("F")
	if f.model.mode != modeStateFilter {
		t.Fatalf("mode = %d, want modeStateFilter", f.model.mode)
	}
	view := f.model.viewContent()
	for _, s := range tuiview.FilterableStates {
		want := "[x] " + s
		if s == "failed" || s == "abandoned" {
			want = "[ ] " + s
		}
		if !strings.Contains(view, want) {
			t.Errorf("overlay lacks %q:\n%s", want, view)
		}
	}
	lines := strings.Split(view, "\n")
	if len(lines) > 20 {
		t.Errorf("view has %d rows, want at most 20", len(lines))
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 60 {
			t.Errorf("line wider than 60: %q", l)
		}
	}
}

func TestStateFilterToggleShowsFailed(t *testing.T) {
	f := newBoardFixture(t, 120, 20, filterTasks()...)
	f.press("F")
	for i := 0; i < 7; i++ {
		f.press("j")
	}
	f.resetCalls()
	f.press(" ")
	if !f.listedStates()["failed"] {
		t.Errorf("no state=failed request: %v", f.listedStates())
	}
	f.press("esc")
	if f.model.mode != modeNormal {
		t.Fatalf("mode = %d after esc", f.model.mode)
	}
	view := f.model.viewContent()
	if !strings.Contains(view, "Fail") {
		t.Errorf("failed task not rendered:\n%s", view)
	}
	last := lastLine(strings.TrimRight(f.model.shell.items.render(), "\n"))
	if !strings.Contains(last, "hidden: abandoned") || strings.Contains(last, "failed") {
		t.Errorf("footer = %q", last)
	}
}

func TestStateFilterToggleRoundTrip(t *testing.T) {
	f := newBoardFixture(t, 120, 20, filterTasks()...)
	before := f.model.viewContent()
	f.press("F")
	for i := 0; i < 7; i++ {
		f.press("j")
	}
	f.press(" ", "j", " ")
	f.press(" ", "k", " ", "F")
	if f.model.mode != modeNormal {
		t.Fatalf("mode = %d, want normal", f.model.mode)
	}
	if after := f.model.viewContent(); after != before {
		t.Errorf("rendering changed:\n%s\n---\n%s", before, after)
	}
}

func TestStateFilterOverlaySwallowsActionKeys(t *testing.T) {
	f := newBlockedFlowFixture(t)
	f.press("F")
	f.resetCalls()
	for _, k := range []string{"f", "p", "q", "u"} {
		_, cmd := f.model.Update(key(k))
		if cmd != nil {
			t.Errorf("%q returned a command", k)
		}
		if f.model.mode != modeStateFilter {
			t.Fatalf("%q left the overlay, mode %d", k, f.model.mode)
		}
	}
	if got := f.got(); got != "" {
		t.Errorf("calls = %q", got)
	}
}

func TestStateFilterLowerFStillFails(t *testing.T) {
	f := newBlockedFlowFixture(t)
	f.press("F", "F")
	f.press("f")
	if f.model.mode != modeFailNote {
		t.Errorf("mode = %d, want modeFailNote", f.model.mode)
	}
}

func TestStateFilterBindingInTable(t *testing.T) {
	f := newBoardFixture(t, 120, 20, filterTasks()...)
	b := f.model.keyBindings.Shell.binding(actStateFilter)
	if b.Help().Key != "F" {
		t.Errorf("binding help = %+v", b.Help())
	}
	found := false
	for _, h := range f.model.keyBindings.Shell.help(f.model.shellKeyCtx()) {
		if h.Help().Key == "F" {
			found = true
		}
	}
	if !found {
		t.Error("F is not in the help list")
	}
}
