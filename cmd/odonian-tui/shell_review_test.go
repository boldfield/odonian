package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
)

// newReviewFixture is the shell with one review task ("review-task-1") selected.
func newReviewFixture(t *testing.T, extra ...tuiclient.Task) *boardFixture {
	t.Helper()
	tasks := append([]tuiclient.Task{
		{ID: "review-task-1", Title: "Review Task", State: "review", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
	}, extra...)
	f := newBoardFixture(t, 140, 40, tasks...)
	f.selectItem(t, "review-task-1")
	return f
}

// mock is the fixture's client, for overriding a call.
func (f *boardFixture) mock() *tuiclient.MockClient {
	return f.model.client.(*tuiclient.MockClient)
}

// reviewCapture records the arguments of the review and transition calls.
type reviewCapture struct {
	reviewID, reviewActor, reviewVerdict string
	reviewNote                           *string
	transitionID, transitionTo           string
	transitionNote                       *string
}

// capture wraps the fixture client's review and transition calls to record their arguments.
func (f *boardFixture) capture() *reviewCapture {
	c := &reviewCapture{}
	review, transition := f.mock().ReviewTaskFunc, f.mock().TransitionTaskFunc
	f.mock().ReviewTaskFunc = func(ctx context.Context, id, actor, verdict string, note *string) error {
		c.reviewID, c.reviewActor, c.reviewVerdict, c.reviewNote = id, actor, verdict, note
		return review(ctx, id, actor, verdict, note)
	}
	f.mock().TransitionTaskFunc = func(ctx context.Context, id, to string, note *string) error {
		c.transitionID, c.transitionTo, c.transitionNote = id, to, note
		return transition(ctx, id, to, note)
	}
	return c
}

func TestShellPromoteTask(t *testing.T) {
	f := newBoardFixture(t, 140, 40,
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "backlog", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
		tuiclient.Task{ID: "task-2", Title: "Task 2", State: "backlog", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"},
	)
	f.selectItem(t, "task-1")
	f.resetCalls()
	f.press("p")
	if got := f.got(); got != "promote task-1" {
		t.Errorf("calls = %q, want promote task-1", got)
	}
	if len(f.listCalls()) == 0 {
		t.Errorf("expected a refetch after promoting")
	}
}

func promoteWithError(t *testing.T, returnedErr error) *boardFixture {
	t.Helper()
	f := newBoardFixture(t, 140, 40,
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "backlog", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
	)
	f.mock().PromoteTaskFunc = func(ctx context.Context, id string) error { return returnedErr }
	f.selectItem(t, "task-1")
	f.press("p")
	return f
}

// A real *tuiclient.APIError with StatusCode 409 produces the friendly message, not the generic
// branch, so this fails if the typed-error detection is replaced by string matching.
func TestShellPromoteTask409(t *testing.T) {
	f := promoteWithError(t, &tuiclient.APIError{StatusCode: 409, Code: "CONFLICT", Message: "Task is not in backlog"})
	const friendlyMsg = "not in backlog (already moved?)"
	if f.model.error != friendlyMsg {
		t.Errorf("error = %q, want %q", f.model.error, friendlyMsg)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "Task 1") {
		t.Errorf("board not functional after the error:\n%s", view)
	}
}

func TestShellPromoteTask500(t *testing.T) {
	f := promoteWithError(t, &tuiclient.APIError{StatusCode: 500, Code: "INTERNAL_ERROR", Message: "something went wrong"})
	if f.model.error == "not in backlog (already moved?)" {
		t.Errorf("500 produced the friendly 409 message")
	}
	if !strings.Contains(f.model.error, "promote failed:") {
		t.Errorf("error %q lacks the 'promote failed:' prefix", f.model.error)
	}
	if !strings.Contains(f.model.error, "something went wrong") {
		t.Errorf("error %q lacks the server message", f.model.error)
	}
}

func TestShellPromoteOnNonBacklogIsNoOp(t *testing.T) {
	f := newBoardFixture(t, 140, 40,
		tuiclient.Task{ID: "task-2", Title: "Task 2", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
	)
	f.selectItem(t, "task-2")
	_, cmd := f.model.Update(key("p"))
	if cmd != nil {
		t.Errorf("p on a ready item returned a command")
	}
	if got := f.got(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

func TestShellApproveFlow_ConfirmY(t *testing.T) {
	f := newReviewFixture(t)
	c := f.capture()
	f.resetCalls()

	f.press("a")
	if f.model.mode != modeApproveNote {
		t.Fatalf("mode = %d after 'a', want modeApproveNote", f.model.mode)
	}
	f.press("LGTM", "enter")
	if f.model.mode != modeApproveConfirm {
		t.Fatalf("mode = %d after enter, want modeApproveConfirm", f.model.mode)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "→ done?") {
		t.Errorf("confirm prompt not in view:\n%s", view)
	}
	f.press("y")
	if f.model.mode != modeNormal {
		t.Errorf("mode = %d after confirm, want normal", f.model.mode)
	}

	if got := f.got(); got != "review approve review-task-1; transition review-task-1 done" {
		t.Errorf("calls = %q", got)
	}
	if len(f.listCalls()) == 0 {
		t.Errorf("expected a refetch after approving")
	}
	if c.reviewID != "review-task-1" || c.reviewActor != "u" || c.reviewVerdict != "approve" {
		t.Errorf("ReviewTask got id %q actor %q verdict %q", c.reviewID, c.reviewActor, c.reviewVerdict)
	}
	if c.reviewNote == nil || *c.reviewNote != "LGTM" {
		t.Errorf("ReviewTask note = %v, want LGTM", c.reviewNote)
	}
	if c.transitionID != "review-task-1" || c.transitionTo != "done" {
		t.Errorf("TransitionTask got id %q to %q", c.transitionID, c.transitionTo)
	}
	if c.transitionNote == nil || *c.transitionNote != "LGTM" {
		t.Errorf("TransitionTask note = %v, want LGTM", c.transitionNote)
	}
}

func TestShellApproveFlow_EmptyNote(t *testing.T) {
	f := newReviewFixture(t)
	c := f.capture()
	f.press("a", "enter", "y")
	if got := f.got(); got != "review approve review-task-1; transition review-task-1 done" {
		t.Fatalf("calls = %q", got)
	}
	if c.reviewNote != nil {
		t.Errorf("ReviewTask note = %q, want nil", *c.reviewNote)
	}
	if c.transitionNote != nil {
		t.Errorf("TransitionTask note = %q, want nil", *c.transitionNote)
	}
}

func TestShellApproveFlow_CancelWithEscAtNote(t *testing.T) {
	f := newReviewFixture(t)
	f.press("a")
	if f.model.mode != modeApproveNote {
		t.Fatalf("mode = %d, want modeApproveNote", f.model.mode)
	}
	_, cmd := f.model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if f.model.mode != modeNormal {
		t.Errorf("mode = %d after esc, want normal", f.model.mode)
	}
	if cmd != nil {
		t.Errorf("esc returned a command")
	}
	if got := f.got(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

func TestShellApproveFlow_CancelWithN(t *testing.T) {
	f := newReviewFixture(t)
	f.press("a", "enter")
	if f.model.mode != modeApproveConfirm {
		t.Fatalf("mode = %d, want modeApproveConfirm", f.model.mode)
	}
	_, cmd := f.model.Update(key("n"))
	if f.model.mode != modeNormal {
		t.Errorf("mode = %d after 'n', want normal", f.model.mode)
	}
	if cmd != nil {
		t.Errorf("'n' returned a command")
	}
	if got := f.got(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

// A bracketed paste is typed into the text input in a reason mode and ignored on the board.
func TestShellPasteGoesToTextInput(t *testing.T) {
	f := newReviewFixture(t)
	f.send(tea.PasteMsg{Content: "x"})
	if f.model.mode != modeNormal {
		t.Fatalf("mode = %d after paste on the board, want normal", f.model.mode)
	}
	f.press("x")
	if f.model.mode != modeRejectReason {
		t.Fatalf("mode = %d after 'x', want modeRejectReason", f.model.mode)
	}
	f.send(tea.PasteMsg{Content: "pasted reason"})
	if got := f.model.reviewInput.Value(); got != "pasted reason" {
		t.Errorf("input = %q, want the pasted text", got)
	}
}

func TestShellRejectFlow_EmptyReasonBlocked(t *testing.T) {
	f := newReviewFixture(t)
	f.press("x")
	if f.model.mode != modeRejectReason {
		t.Fatalf("mode = %d after 'x', want modeRejectReason", f.model.mode)
	}
	_, cmd := f.model.Update(key("enter"))
	if f.model.mode != modeRejectReason {
		t.Errorf("mode = %d on empty submit, want modeRejectReason", f.model.mode)
	}
	if cmd != nil {
		t.Errorf("empty reason submit returned a command")
	}
	if got := f.got(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "reason is required") {
		t.Errorf("'reason is required' hint not in view:\n%s", view)
	}
}

func TestShellRejectFlow_NonEmptyReason(t *testing.T) {
	f := newReviewFixture(t)
	c := f.capture()
	f.resetCalls()
	f.press("x", "needs tests")
	f.press("enter")
	if f.model.mode != modeNormal {
		t.Errorf("mode = %d after submit, want normal", f.model.mode)
	}
	if got := f.got(); got != "review reject review-task-1; transition review-task-1 ready" {
		t.Errorf("calls = %q", got)
	}
	if len(f.listCalls()) == 0 {
		t.Errorf("expected a refetch after rejecting")
	}
	if c.reviewID != "review-task-1" || c.reviewActor != "u" || c.reviewVerdict != "reject" {
		t.Errorf("ReviewTask got id %q actor %q verdict %q", c.reviewID, c.reviewActor, c.reviewVerdict)
	}
	if c.reviewNote == nil || *c.reviewNote != "needs tests" {
		t.Errorf("ReviewTask note = %v, want 'needs tests'", c.reviewNote)
	}
	if c.transitionID != "review-task-1" || c.transitionTo != "ready" {
		t.Errorf("TransitionTask got id %q to %q", c.transitionID, c.transitionTo)
	}
	if c.transitionNote == nil || *c.transitionNote != "needs tests" {
		t.Errorf("TransitionTask note = %v, want 'needs tests'", c.transitionNote)
	}
}

// A 409 from TransitionTask after a successful ReviewTask surfaces an error and still refetches.
func TestShellApproveTransition409(t *testing.T) {
	f := newReviewFixture(t)
	f.mock().TransitionTaskFunc = func(ctx context.Context, id, to string, note *string) error {
		f.setTasks(tuiclient.Task{ID: "review-task-1", Title: "Review Task", State: "done", Kind: "implement"})
		return &tuiclient.APIError{StatusCode: http.StatusConflict, Code: "CONFLICT", Message: "task already transitioned"}
	}
	f.resetCalls()
	f.press("a", "enter", "y")

	if got := f.got(); got != "review approve review-task-1" {
		t.Errorf("calls = %q, want only the review call", got)
	}
	if f.model.error == "" {
		t.Fatalf("expected an error after the 409 transition")
	}
	if !strings.Contains(f.model.error, "409") && !strings.Contains(f.model.error, "task already transitioned") {
		t.Errorf("error %q lacks the 409 message", f.model.error)
	}
	if len(f.listCalls()) == 0 {
		t.Errorf("expected a refetch after the 409")
	}
	for _, item := range f.model.shell.items.items {
		if item.Implement.State == "review" {
			t.Errorf("board not refreshed after the 409: %s still in review", item.Implement.ID)
		}
	}
	if view := f.model.viewContent(); view == "" {
		t.Errorf("empty view after the 409")
	}
}

// Pressing 'a' or 'x' on a selected item that is not in review does nothing.
func TestShellReviewActionsNoopOutsideReview(t *testing.T) {
	f := newBoardFixture(t, 140, 40,
		tuiclient.Task{ID: "task-b", Title: "Backlog Task", State: "backlog", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
		tuiclient.Task{ID: "task-ip", Title: "IP Task", State: "in_progress", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"},
	)
	for _, id := range []string{"task-ip", "task-b"} {
		f.selectItem(t, id)
		for _, k := range []string{"a", "x"} {
			_, cmd := f.model.Update(key(k))
			if f.model.mode != modeNormal {
				t.Errorf("%q on %s: mode %d, want normal", k, id, f.model.mode)
			}
			if cmd != nil {
				t.Errorf("%q on %s returned a command", k, id)
			}
		}
	}
	if got := f.got(); got != "" {
		t.Errorf("calls = %q, want none", got)
	}
}

// The help bar offers approve/reject only for a selected item in review.
func TestShellReviewHelpBar(t *testing.T) {
	f := newReviewFixture(t, tuiclient.Task{ID: "t-ip", Title: "IP", State: "in_progress", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"})
	bar := f.model.shellKeyHints()
	if !strings.Contains(bar, "a approve") || !strings.Contains(bar, "x reject") {
		t.Errorf("review item help %q lacks approve/reject", bar)
	}
	if view := f.model.viewContent(); !strings.Contains(view, "a approve") {
		t.Errorf("'a approve' not in the rendered shell:\n%s", view)
	}
	f.selectItem(t, "t-ip")
	bar = f.model.shellKeyHints()
	if strings.Contains(bar, "a approve") || strings.Contains(bar, "x reject") {
		t.Errorf("non-review item help offers approve/reject: %q", bar)
	}
	if view := f.model.viewContent(); strings.Contains(view, "a approve") || strings.Contains(view, "x reject") {
		t.Errorf("approve/reject in the rendered shell for a non-review item:\n%s", view)
	}
}

// In local_commit mode approving must land the work on the MR branch, which only `odonian
// approve` does, so the TUI must neither record the verdict nor transition the task to done.
func TestShellApproveRefusedInLocalCommitMode(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	f := newReviewFixture(t)
	f.resetCalls()

	msg, ok := f.model.reviewApprove("review-task-1", nil, false)().(reviewActionMsg)
	if !ok {
		t.Fatal("expected a reviewActionMsg")
	}
	if got := f.got(); got != "" {
		t.Errorf("local_commit approve from the TUI touched the task: %q", got)
	}
	if !strings.Contains(msg.err, "odonian approve review-task-1") {
		t.Errorf("message %q does not point at odonian approve", msg.err)
	}
}
