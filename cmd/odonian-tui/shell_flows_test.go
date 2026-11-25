package main

import (
	"context"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/tuiclient"
)

// captureTransitions wraps the fixture client's TransitionTask to record the note it is given.
func captureTransitions(f *boardFixture) *[]*string {
	var notes []*string
	mc := f.model.client.(*tuiclient.MockClient)
	inner := mc.TransitionTaskFunc
	mc.TransitionTaskFunc = func(ctx context.Context, id, to string, note *string) error {
		notes = append(notes, note)
		return inner(ctx, id, to, note)
	}
	return &notes
}

func newBlockedFlowFixture(t *testing.T) *boardFixture {
	t.Helper()
	f := newBoardFixture(t, 140, 40,
		tuiclient.Task{ID: "blocked-task-1", Title: "Blocked Task", State: "blocked", Kind: "implement"},
	)
	f.selectItem(t, "blocked-task-1")
	return f
}

func newReadyFlowFixture(t *testing.T, held bool) *boardFixture {
	t.Helper()
	f := newBoardFixture(t, 140, 40,
		tuiclient.Task{ID: "ready-task-1", Title: "Ready Task", State: "ready", Kind: "implement", Held: held},
	)
	f.selectItem(t, "ready-task-1")
	return f
}

// taskHeld reports whether the board's model holds the task with Held set. The shell does
// not render a held marker, so the model state is what a hold or release changes.
func taskHeld(f *boardFixture, id string) bool {
	for _, list := range f.model.tasks {
		for _, task := range list {
			if task.ID == id {
				return task.Held
			}
		}
	}
	return false
}

func TestBoardModel_UnblockFlow(t *testing.T) {
	f := newBlockedFlowFixture(t)
	notes := captureTransitions(f)

	f.press("u")
	if f.model.mode != modeUnblockNote {
		t.Fatalf("mode after 'u' = %d, want modeUnblockNote", f.model.mode)
	}
	f.press("Dependency resolved", "enter")
	if f.model.mode != modeUnblockConfirm {
		t.Fatalf("mode after enter = %d, want modeUnblockConfirm", f.model.mode)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "→ ready?") {
		t.Errorf("confirm prompt not in view:\n%s", view)
	}
	f.press("y")
	if f.model.mode != modeNormal {
		t.Errorf("mode after confirm = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "transition blocked-task-1 ready" {
		t.Errorf("calls = %q", got)
	}
	if len(*notes) != 1 || (*notes)[0] == nil || *(*notes)[0] != "Dependency resolved" {
		t.Errorf("note = %v, want 'Dependency resolved'", *notes)
	}
}

func TestBoardModel_FailFlow(t *testing.T) {
	f := newBlockedFlowFixture(t)
	notes := captureTransitions(f)

	f.press("f")
	if f.model.mode != modeFailNote {
		t.Fatalf("mode after 'f' = %d, want modeFailNote", f.model.mode)
	}
	f.press("Unresolvable", "enter")
	if f.model.mode != modeFailConfirm {
		t.Fatalf("mode after enter = %d, want modeFailConfirm", f.model.mode)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "→ failed?") {
		t.Errorf("confirm prompt not in view:\n%s", view)
	}
	f.press("y")
	if f.model.mode != modeNormal {
		t.Errorf("mode after confirm = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "transition blocked-task-1 failed" {
		t.Errorf("calls = %q", got)
	}
	if len(*notes) != 1 || (*notes)[0] == nil || *(*notes)[0] != "Unresolvable" {
		t.Errorf("note = %v, want 'Unresolvable'", *notes)
	}
}

func TestBoardModel_ArchiveTaskFlow_ConfirmY(t *testing.T) {
	f := newReadyFlowFixture(t, false)

	f.press("z")
	if f.model.mode != modeArchiveTaskConfirm {
		t.Fatalf("mode after 'z' = %d, want modeArchiveTaskConfirm", f.model.mode)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "Archive") {
		t.Errorf("archive confirm prompt not in view:\n%s", view)
	}
	// The archived task leaves the board on the refetch that follows.
	f.setTasks()
	f.press("y")
	if f.model.mode != modeNormal {
		t.Errorf("mode after confirm = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "archive ready-task-1" {
		t.Errorf("calls = %q", got)
	}
	if id := f.model.shell.items.SelectedTaskID(); id != "" {
		t.Errorf("archived task still selected: %q", id)
	}
}

func TestBoardModel_ArchiveTaskFlow_ConfirmN(t *testing.T) {
	f := newReadyFlowFixture(t, false)
	f.press("z")
	if f.model.mode != modeArchiveTaskConfirm {
		t.Fatalf("mode after 'z' = %d, want modeArchiveTaskConfirm", f.model.mode)
	}
	f.press("n")
	if f.model.mode != modeNormal {
		t.Errorf("mode after cancel = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "" {
		t.Errorf("calls after 'n' = %q, want none", got)
	}
}

func TestBoardModel_ArchiveTaskNoBareKeypress(t *testing.T) {
	f := newReadyFlowFixture(t, false)
	f.press("a")
	if f.model.mode == modeArchiveTaskConfirm {
		t.Error("mode is modeArchiveTaskConfirm after pressing 'a'")
	}
	if got := f.got(); got != "" {
		t.Errorf("calls without 'z' = %q, want none", got)
	}
}

func TestBoardModel_ArchiveProjectFlow_ConfirmY(t *testing.T) {
	f := newActionFixture(t)
	f.press("tab", "tab", "down")
	if got := f.model.shell.projects.SelectedProjectID(); got != "p2" {
		t.Fatalf("selected project = %q, want p2", got)
	}
	f.press("z")
	if f.model.mode != modeArchiveProjectConfirm {
		t.Fatalf("mode after 'z' = %d, want modeArchiveProjectConfirm", f.model.mode)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "Archive project?") {
		t.Errorf("'Archive project?' prompt not in view:\n%s", view)
	}
	f.press("y")
	if f.model.mode != modeNormal {
		t.Errorf("mode after confirm = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "archive-project p2" {
		t.Errorf("calls = %q", got)
	}
}

func TestBoardModel_ArchiveProjectFlow_CancelWithN(t *testing.T) {
	f := newActionFixture(t)
	f.press("tab", "tab", "down", "z")
	if f.model.mode != modeArchiveProjectConfirm {
		t.Fatalf("mode after 'z' = %d, want modeArchiveProjectConfirm", f.model.mode)
	}
	f.press("n")
	if f.model.mode != modeNormal {
		t.Errorf("mode after cancel = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "" {
		t.Errorf("calls after 'n' = %q, want none", got)
	}
}

func TestBoardModel_HoldTaskFlow_ConfirmY(t *testing.T) {
	f := newReadyFlowFixture(t, false)
	if taskHeld(f, "ready-task-1") {
		t.Error("task held before hold")
	}
	f.press("t")
	if f.model.mode != modeHoldTaskConfirm {
		t.Fatalf("mode after 't' = %d, want modeHoldTaskConfirm", f.model.mode)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "Hold") {
		t.Errorf("hold confirm prompt not in view:\n%s", view)
	}
	f.setTasks(tuiclient.Task{ID: "ready-task-1", Title: "Ready Task", State: "ready", Kind: "implement", Held: true})
	f.press("y")
	if f.model.mode != modeNormal {
		t.Errorf("mode after confirm = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "hold ready-task-1" {
		t.Errorf("calls = %q", got)
	}
	if !taskHeld(f, "ready-task-1") {
		t.Error("task not held after hold and refetch")
	}
}

func TestBoardModel_ReleaseTaskFlow_ConfirmY(t *testing.T) {
	f := newReadyFlowFixture(t, true)
	if !taskHeld(f, "ready-task-1") {
		t.Error("task not held before release")
	}
	f.press("t")
	if f.model.mode != modeReleaseTaskConfirm {
		t.Fatalf("mode after 't' on held task = %d, want modeReleaseTaskConfirm", f.model.mode)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "Release") {
		t.Errorf("release confirm prompt not in view:\n%s", view)
	}
	f.setTasks(tuiclient.Task{ID: "ready-task-1", Title: "Ready Task", State: "ready", Kind: "implement"})
	f.press("y")
	if f.model.mode != modeNormal {
		t.Errorf("mode after confirm = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "release ready-task-1" {
		t.Errorf("calls = %q", got)
	}
	if taskHeld(f, "ready-task-1") {
		t.Error("task still held after release and refetch")
	}
}

func TestBoardModel_HoldTaskFlow_ConfirmN(t *testing.T) {
	f := newReadyFlowFixture(t, false)
	f.press("t")
	if f.model.mode != modeHoldTaskConfirm {
		t.Fatalf("mode after 't' = %d, want modeHoldTaskConfirm", f.model.mode)
	}
	f.press("n")
	if f.model.mode != modeNormal {
		t.Errorf("mode after cancel = %d, want normal", f.model.mode)
	}
	if got := f.got(); got != "" {
		t.Errorf("calls after 'n' = %q, want none", got)
	}
}
