package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
)

func intPtr(v int64) *int64 {
	return &v
}

func strPtr(v string) *string {
	return &v
}

// priorityFixture is the shell over the given tasks with the first one selected.
func priorityFixture(t *testing.T, tasks ...tuiclient.Task) *boardFixture {
	t.Helper()
	f := newBoardFixture(t, 140, 40, tasks...)
	f.selectItem(t, tasks[0].ID)
	// A blinking cursor arms a timer command per keystroke, which the fixture runs
	// synchronously; a static cursor keeps typing instant.
	styles := f.model.reviewInput.Styles()
	styles.Cursor.Blink = false
	f.model.reviewInput.SetStyles(styles)
	return f
}

// priorityTestFlow drives a priority action through the keyboard: the start key, optional
// manual value, reason, and the final confirmation.
func priorityTestFlow(f *boardFixture, startKey, value, reason string) {
	f.t.Helper()
	f.press(startKey)
	if value != "" {
		f.press(value, "enter")
	}
	f.press(reason, "enter", "y")
}

// selectedPriority is the priority of the selected work item's implement task, 0 when unset.
// The shell's rows do not render a priority, so a refresh is checked in the model state.
func selectedPriority(f *boardFixture) int64 {
	item := f.model.shell.items.SelectedItem()
	if item == nil || item.Implement.Priority == nil {
		return 0
	}
	return *item.Implement.Priority
}

// enterDetail opens the detail view of the selected task.
func enterDetail(t *testing.T, f *boardFixture) {
	t.Helper()
	f.press("enter")
	if f.model.mode != modeDetail {
		t.Fatalf("expected modeDetail, got %d", f.model.mode)
	}
}

func TestBoardModel_SetPriorityFlow_ValidInput(t *testing.T) {
	var gotID string
	var gotReq tuiclient.SetTaskPriorityRequest
	f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement"})
	f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
		gotID, gotReq = id, req
		return tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: req.Priority, OldPriority: 500}, nil
	}

	f.press("i")
	if f.model.mode != modeSetPriorityInput {
		t.Fatalf("expected modeSetPriorityInput, got %d", f.model.mode)
	}
	if out := f.model.View().Content; !strings.Contains(out, "Set priority — enter a value (1-1000)") {
		t.Errorf("expected set priority prompt in view, got:\n%s", out)
	}
	f.press("750", "enter")
	if f.model.mode != modeSetPriorityReason {
		t.Fatalf("expected modeSetPriorityReason, got %d", f.model.mode)
	}
	if out := f.model.View().Content; !strings.Contains(out, "reason (required)") {
		t.Errorf("expected reason prompt in view, got:\n%s", out)
	}
	f.press("important feature", "enter")
	if f.model.mode != modeSetPriorityConfirm {
		t.Fatalf("expected modeSetPriorityConfirm, got %d", f.model.mode)
	}
	if out := f.model.View().Content; !strings.Contains(out, "Set priority to 750") {
		t.Errorf("expected priority value 750 in confirm view, got:\n%s", out)
	}
	f.press("y")
	if f.model.mode != modeNormal {
		t.Fatalf("expected modeNormal after confirm, got %d", f.model.mode)
	}

	if gotID != "task-1" {
		t.Errorf("expected task ID task-1, got %q", gotID)
	}
	if gotReq.Priority != 750 || gotReq.Reason != "important feature" {
		t.Errorf("expected priority 750 and reason 'important feature', got %+v", gotReq)
	}
	if gotReq.ActionKey == "" {
		t.Errorf("expected non-empty ActionKey")
	}
	if gotReq.Actor != "tui-worker" {
		t.Errorf("expected actor 'tui-worker', got %q", gotReq.Actor)
	}
	m := f.model
	if m.error != "" {
		t.Errorf("expected no error after success, got %q", m.error)
	}
	if !strings.Contains(m.statusMessage, "priority set to 750") {
		t.Errorf("expected status message containing 'priority set to 750', got %q", m.statusMessage)
	}
	if m.lastPriorityAttempt.key != "" {
		t.Errorf("expected remembered attempt cleared after success, got %q", m.lastPriorityAttempt.key)
	}
	if m.pendingPriority != 0 {
		t.Errorf("expected pendingPriority cleared after success, got %d", m.pendingPriority)
	}
}

func TestBoardModel_SetPriorityFlow_InvalidInputs(t *testing.T) {
	tests := []struct {
		input   string
		wantErr string
	}{
		{"", "priority value required"},
		{"0", "priority must be an integer between 1 and 1000"},
		{"1001", "priority must be an integer between 1 and 1000"},
		{"-5", "priority must be an integer between 1 and 1000"},
		{"abc", "priority must be an integer between 1 and 1000"},
		{"5.5", "priority must be an integer between 1 and 1000"},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("input_%s", tt.input), func(t *testing.T) {
			f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement"})
			f.press("i")
			if tt.input != "" {
				f.press(tt.input)
			}
			f.press("enter")
			if f.model.mode != modeSetPriorityInput {
				t.Errorf("expected to stay in modeSetPriorityInput, got %d", f.model.mode)
			}
			if f.model.inputHint != tt.wantErr {
				t.Errorf("expected hint %q, got %q", tt.wantErr, f.model.inputHint)
			}
			if out := f.model.View().Content; !strings.Contains(out, tt.wantErr) {
				t.Errorf("expected error %q in view output, got:\n%s", tt.wantErr, out)
			}
			if f.got() != "" {
				t.Errorf("invalid input made a client call: %s", f.got())
			}
		})
	}
}

func TestBoardModel_MoveToFrontFlow(t *testing.T) {
	var gotID string
	var gotReq tuiclient.MoveTaskToFrontRequest
	f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement"})
	f.mock().MoveTaskToFrontFunc = func(ctx context.Context, id string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
		gotID, gotReq = id, req
		return tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: 1500, OldPriority: 500}, nil
	}

	f.press("w")
	if f.model.mode != modeMoveToFrontReason {
		t.Fatalf("expected modeMoveToFrontReason, got %d", f.model.mode)
	}
	if out := f.model.View().Content; !strings.Contains(out, "Move to front — reason (required)") {
		t.Errorf("expected move to front prompt in view, got:\n%s", out)
	}
	f.press("blocking other tasks", "enter")
	if f.model.mode != modeMoveToFrontConfirm {
		t.Fatalf("expected modeMoveToFrontConfirm, got %d", f.model.mode)
	}
	if out := f.model.View().Content; !strings.Contains(out, "Move") || !strings.Contains(out, "to front?") {
		t.Errorf("expected move to front confirm prompt in view, got:\n%s", out)
	}
	f.press("y")
	if f.model.mode != modeNormal {
		t.Fatalf("expected modeNormal after confirm, got %d", f.model.mode)
	}

	if gotID != "task-1" {
		t.Errorf("expected task ID task-1, got %q", gotID)
	}
	if gotReq.Reason != "blocking other tasks" {
		t.Errorf("expected reason 'blocking other tasks', got %q", gotReq.Reason)
	}
	if gotReq.ActionKey == "" {
		t.Errorf("expected non-empty ActionKey")
	}
	if f.model.error != "" {
		t.Errorf("expected no error after success, got %q", f.model.error)
	}
	if !strings.Contains(f.model.statusMessage, "priority 1500") {
		t.Errorf("expected priority 1500 in success message, got status: %q", f.model.statusMessage)
	}
}

func TestBoardModel_ActionKeyPreservation(t *testing.T) {
	var keys []string
	f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement"})
	f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
		keys = append(keys, req.ActionKey)
		return tuiclient.PriorityChange{Priority: req.Priority, OldPriority: 500}, nil
	}
	priorityTestFlow(f, "i", "750", "test")
	if len(keys) == 0 {
		t.Fatalf("expected action key to be sent to API")
	}
	if keys[0] == "" {
		t.Errorf("expected non-empty action key to be sent to API")
	}
}

func TestBoardModel_ResetPriorityFlow(t *testing.T) {
	var gotID string
	var gotReq tuiclient.SetTaskPriorityRequest
	f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(750)})
	f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
		gotID, gotReq = id, req
		f.setTasks(tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(500)})
		return tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: 500, OldPriority: 750}, nil
	}

	f.press("e")
	if f.model.mode != modeResetPriorityReason {
		t.Fatalf("expected modeResetPriorityReason, got %d", f.model.mode)
	}
	if out := f.model.View().Content; !strings.Contains(out, "Reset priority to 500 — reason (required)") {
		t.Errorf("expected reset priority reason prompt in view, got:\n%s", out)
	}

	// Submitting an empty reason keeps the prompt and shows a hint.
	f.press("enter")
	if f.model.mode != modeResetPriorityReason {
		t.Fatalf("expected to remain in modeResetPriorityReason on empty reason, got %d", f.model.mode)
	}
	if out := f.model.View().Content; !strings.Contains(out, "hint:") || !strings.Contains(out, "Reset priority to 500 — reason (required)") {
		t.Errorf("expected reset reason prompt with hint after empty reason, got:\n%s", out)
	}

	f.press("no longer urgent", "enter")
	if f.model.mode != modeResetPriorityConfirm {
		t.Fatalf("expected modeResetPriorityConfirm, got %d", f.model.mode)
	}
	out := f.model.View().Content
	if !strings.Contains(out, "Reset priority to 500 for task-1? [y/N]") {
		t.Errorf("expected reset priority confirm prompt in view, got:\n%s", out)
	}
	if !strings.Contains(out, "(y to confirm, n/esc to cancel)") {
		t.Errorf("expected confirm help in reset confirm view, got:\n%s", out)
	}
	f.press("y")
	if f.model.mode != modeNormal {
		t.Fatalf("expected modeNormal after confirm, got %d", f.model.mode)
	}
	if got := selectedPriority(f); got != 500 {
		t.Errorf("expected refreshed list to hold priority 500 after reset, got %d", got)
	}
	if gotID != "task-1" {
		t.Errorf("expected task ID task-1, got %q", gotID)
	}
	if gotReq.Priority != 500 || gotReq.Reason != "no longer urgent" {
		t.Errorf("expected priority 500 and reason 'no longer urgent', got %+v", gotReq)
	}
	if gotReq.ActionKey == "" {
		t.Errorf("expected non-empty ActionKey")
	}
}

func TestDetailView_ResetPriorityFlow(t *testing.T) {
	var gotReq tuiclient.SetTaskPriorityRequest
	f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(750)})
	f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
		gotReq = req
		return tuiclient.PriorityChange{Priority: 500, OldPriority: 750}, nil
	}
	enterDetail(t, f)
	f.press("e")
	if f.model.mode != modeResetPriorityReason {
		t.Fatalf("expected modeResetPriorityReason, got %d", f.model.mode)
	}
	f.press("test", "enter", "y")
	if gotReq.Priority != 500 {
		t.Errorf("expected priority 500, got %d", gotReq.Priority)
	}
}

// TestDetailView_PriorityFlowsRenderPrompts drives Set priority, Move to front and Reset
// priority from the detail view and asserts every step's rendered output: the detail help
// bar advertises the key, each input/confirm overlay is visible, cancelling returns to the
// rendered detail view, and a confirmed action lands on the refreshed board with status.
func TestDetailView_PriorityFlowsRenderPrompts(t *testing.T) {
	tests := []struct {
		name          string
		startKey      string
		helpLabel     string
		value         string
		valuePrompt   string
		reasonPrompt  string
		confirmPrompt string
		resultValue   int64
		wantStatus    string
	}{
		{
			name:          "set priority",
			startKey:      "i",
			helpLabel:     "i set priority",
			value:         "1000",
			valuePrompt:   "Set priority — enter a value (1-1000)",
			reasonPrompt:  "Set priority — reason (required)",
			confirmPrompt: "Set priority to 1000 for task-1? [y/N]",
			resultValue:   1000,
			wantStatus:    "priority set to 1000 for topic-1",
		},
		{
			name:          "move to front",
			startKey:      "w",
			helpLabel:     "w move to front",
			reasonPrompt:  "Move to front — reason (required)",
			confirmPrompt: "Move task-1 to front? [y/N]",
			resultValue:   1001,
			wantStatus:    "1001",
		},
		{
			name:          "reset priority",
			startKey:      "e",
			helpLabel:     "e reset priority",
			reasonPrompt:  "Reset priority to 500 — reason (required)",
			confirmPrompt: "Reset priority to 500 for task-1? [y/N]",
			resultValue:   500,
			wantStatus:    "500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			change := tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: tt.resultValue, OldPriority: 750}
			newFixture := func() *boardFixture {
				f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(750)})
				f.mock().GetTaskFunc = func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
					return tuiclient.TaskDetail{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(750)}, nil
				}
				f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
					f.setTasks(tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(tt.resultValue)})
					return change, nil
				}
				f.mock().MoveTaskToFrontFunc = func(ctx context.Context, id string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
					f.setTasks(tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(tt.resultValue)})
					return change, nil
				}
				enterDetail(t, f)
				return f
			}
			assertView := func(f *boardFixture, want string) {
				t.Helper()
				out := f.model.View().Content
				if strings.TrimSpace(out) == "" {
					t.Fatalf("expected non-empty view while expecting %q", want)
				}
				if !strings.Contains(out, want) {
					t.Errorf("expected %q in view, got:\n%s", want, out)
				}
			}
			start := func(f *boardFixture) {
				f.press(tt.startKey)
				if tt.valuePrompt != "" {
					if f.model.mode != modeSetPriorityInput {
						t.Fatalf("expected modeSetPriorityInput, got %d", f.model.mode)
					}
					assertView(f, tt.valuePrompt)
					f.press(tt.value, "enter")
				}
			}

			f := newFixture()
			assertView(f, tt.helpLabel)
			assertView(f, "Priority: 750")

			start(f)
			assertView(f, tt.reasonPrompt)
			f.press("operator request", "enter")
			assertView(f, tt.confirmPrompt)
			assertView(f, "(y to confirm, n/esc to cancel)")

			// Declining returns to the detail view with its content visible.
			f.press("n")
			if f.model.mode != modeDetail {
				t.Fatalf("expected modeDetail after declining, got %d", f.model.mode)
			}
			assertView(f, "Priority: 750")
			assertView(f, tt.helpLabel)

			// Run the flow again and confirm: the refreshed board shows status and the new value.
			f = newFixture()
			start(f)
			f.press("operator request", "enter", "y")
			if f.model.mode != modeNormal {
				t.Fatalf("expected modeNormal after confirmed detail action, got %d", f.model.mode)
			}
			assertView(f, tt.wantStatus)
			if got := selectedPriority(f); got != tt.resultValue {
				t.Errorf("expected refreshed list to hold priority %d, got %d", tt.resultValue, got)
			}
			if out := f.model.View().Content; strings.Contains(out, "Error:") {
				t.Errorf("expected no error screen after successful detail action, got:\n%s", out)
			}
		})
	}
}

func TestBoardModel_ManualInputErrors(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		errorHint string
	}{
		{"empty", "", "required"},
		{"zero", "0", "1-1000"},
		{"too high", "1001", "1-1000"},
		{"non-integer", "abc", "integer"},
		{"float", "5.5", "integer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement"})
			f.press("i")
			if tt.input != "" {
				f.press(tt.input)
			}
			f.press("enter")
			if f.model.mode != modeSetPriorityInput {
				t.Fatalf("expected to remain in modeSetPriorityInput for invalid input %q", tt.input)
			}
			if out := f.model.View().Content; !strings.Contains(out, tt.errorHint) {
				t.Errorf("expected error hint %q in output for input %q, got:\n%s", tt.errorHint, tt.input, out)
			}
		})
	}
}

func TestDetailView_HeldPlusPriority(t *testing.T) {
	f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Held Task", State: "ready", Kind: "implement", Held: true, Priority: intPtr(750)})
	f.mock().GetTaskFunc = func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
		return tuiclient.TaskDetail{ID: "task-1", Title: "Held Task", State: "ready", Held: true, Priority: intPtr(750)}, nil
	}
	enterDetail(t, f)
	out := f.model.View().Content
	if !strings.Contains(out, "Priority: 750") {
		t.Errorf("expected Priority: 750 in detail view:\n%s", out)
	}
	if !strings.Contains(out, "[HELD]") {
		t.Errorf("expected [HELD] marker in detail view:\n%s", out)
	}
}

// serverLikeIdempotency mimics the server: a key bound to one request replays that request and
// rejects (409 IDEMPOTENCY_MISMATCH) any different request using the same key.
type serverLikeIdempotency struct {
	seen map[string]tuiclient.SetTaskPriorityRequest
}

func (s *serverLikeIdempotency) check(req tuiclient.SetTaskPriorityRequest) error {
	if s.seen == nil {
		s.seen = map[string]tuiclient.SetTaskPriorityRequest{}
	}
	if prior, ok := s.seen[req.ActionKey]; ok && (prior.Priority != req.Priority || prior.Reason != req.Reason) {
		return &tuiclient.APIError{StatusCode: http.StatusConflict, Code: "IDEMPOTENCY_MISMATCH", Message: "action key already used for a different priority request"}
	}
	s.seen[req.ActionKey] = req
	return nil
}

// TestBoardModel_ActionFailureRetrySendsSameKey drives a failed Set (lost response), then an
// identical retry through the UI, and asserts both calls carry the same non-empty key.
func TestBoardModel_ActionFailureRetrySendsSameKey(t *testing.T) {
	var requests []tuiclient.SetTaskPriorityRequest
	f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(500)})
	f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
		requests = append(requests, req)
		if len(requests) == 1 {
			return tuiclient.PriorityChange{}, fmt.Errorf("timeout")
		}
		return tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: 750, OldPriority: 500}, nil
	}

	priorityTestFlow(f, "i", "750", "urgent")
	if !strings.Contains(f.model.error, "set priority failed") {
		t.Fatalf("expected failure message, got %q", f.model.error)
	}
	if f.model.lastPriorityAttempt.key == "" {
		t.Fatalf("expected attempt kept after retryable failure")
	}
	if n := len(f.model.shell.items.items); n != 1 {
		t.Errorf("expected board state preserved after failure, got %d items", n)
	}

	priorityTestFlow(f, "i", "750", "urgent")
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	if requests[0].ActionKey == "" || requests[0].ActionKey != requests[1].ActionKey {
		t.Errorf("expected identical non-empty keys, got %q and %q", requests[0].ActionKey, requests[1].ActionKey)
	}
	if requests[0].Priority != requests[1].Priority || requests[0].Reason != requests[1].Reason {
		t.Errorf("expected identical requests, got %+v and %+v", requests[0], requests[1])
	}
	if f.model.lastPriorityAttempt.key != "" {
		t.Errorf("expected attempt cleared after success")
	}
	if f.model.error != "" || !strings.Contains(f.model.statusMessage, "priority set to 750") {
		t.Errorf("expected success status and no error, got status %q error %q", f.model.statusMessage, f.model.error)
	}
}

// TestBoardModel_ActionRetryWithChangedRequestMintsFreshKey checks a retry whose value or reason
// differs does not reuse the failed attempt's key (the server would answer IDEMPOTENCY_MISMATCH).
func TestBoardModel_ActionRetryWithChangedRequestMintsFreshKey(t *testing.T) {
	cases := []struct {
		name       string
		value      string
		reason     string
		wantNewKey bool
	}{
		{"changed value", "800", "urgent", true},
		{"changed reason", "750", "very urgent", true},
		{"identical", "750", "urgent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idempotency := &serverLikeIdempotency{}
			var requests []tuiclient.SetTaskPriorityRequest
			f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement"})
			f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
				requests = append(requests, req)
				if err := idempotency.check(req); err != nil {
					return tuiclient.PriorityChange{}, err
				}
				if len(requests) == 1 {
					return tuiclient.PriorityChange{}, fmt.Errorf("timeout after commit")
				}
				return tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: req.Priority}, nil
			}

			priorityTestFlow(f, "i", "750", "urgent")
			priorityTestFlow(f, "i", tc.value, tc.reason)

			if len(requests) != 2 {
				t.Fatalf("expected 2 requests, got %d", len(requests))
			}
			if gotNew := requests[0].ActionKey != requests[1].ActionKey; gotNew != tc.wantNewKey {
				t.Errorf("fresh key = %v, want %v", gotNew, tc.wantNewKey)
			}
			if strings.Contains(f.model.error, "409") {
				t.Errorf("retry hit idempotency mismatch: %q", f.model.error)
			}
		})
	}
}

// TestBoardModel_RejectedActionDropsKey checks a definitive 4xx rejection clears the remembered
// key so the operator is not stuck, and a timeout (408) keeps it.
func TestBoardModel_RejectedActionDropsKey(t *testing.T) {
	cases := []struct {
		status   int
		wantKept bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusConflict, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement"})
			f.mock().MoveTaskToFrontFunc = func(ctx context.Context, id string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
				return tuiclient.PriorityChange{}, &tuiclient.APIError{StatusCode: tc.status, Message: "nope"}
			}
			priorityTestFlow(f, "w", "", "because")

			if !strings.Contains(f.model.error, fmt.Sprintf("move to front %d", tc.status)) {
				t.Errorf("expected real status in error, got %q", f.model.error)
			}
			if kept := f.model.lastPriorityAttempt.key != ""; kept != tc.wantKept {
				t.Errorf("key kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

// TestBoardModel_ActionKeyTiedToRequest checks keys are never shared across tasks or actions and
// that an explicit cancel drops the remembered attempt.
func TestBoardModel_ActionKeyTiedToRequest(t *testing.T) {
	var keys []string
	f := priorityFixture(t,
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
		tuiclient.Task{ID: "task-2", Title: "Task 2", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"},
	)
	f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
		keys = append(keys, req.ActionKey)
		return tuiclient.PriorityChange{}, fmt.Errorf("timeout")
	}
	f.mock().MoveTaskToFrontFunc = func(ctx context.Context, id string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
		keys = append(keys, req.ActionKey)
		return tuiclient.PriorityChange{}, fmt.Errorf("timeout")
	}

	priorityTestFlow(f, "i", "750", "urgent")
	// Different action on the same task, then the same action on a different task.
	priorityTestFlow(f, "w", "", "urgent")
	first := f.model.shell.items.SelectedTaskID()
	f.press("down")
	if f.model.shell.items.SelectedTaskID() == first {
		t.Fatalf("down did not move the selection off %s", first)
	}
	priorityTestFlow(f, "w", "", "urgent")

	if len(keys) != 3 {
		t.Fatalf("expected 3 requests, got %d", len(keys))
	}
	if keys[0] == keys[1] || keys[1] == keys[2] || keys[0] == keys[2] {
		t.Errorf("expected distinct keys across action/task changes, got %v", keys)
	}

	// Explicit cancel clears the remembered attempt.
	f.press("i")
	f.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	m := f.model
	if m.lastPriorityAttempt.key != "" || m.pendingPriority != 0 || m.pendingPriorityReason != "" {
		t.Errorf("expected priority state cleared on cancel, got %+v", m.lastPriorityAttempt)
	}
}

// After each successful priority action the view shows the resolved topic/value as a status
// line and the refreshed task list, and never the error screen.
func TestBoardModel_SuccessfulPriorityActionKeepsRefreshedListVisible(t *testing.T) {
	cases := []struct {
		name         string
		startKey     string
		value        string
		wantPriority int64
		wantStatus   string
	}{
		{"set", "i", "750", 750, "priority set to 750 for topic-1"},
		{"front", "w", "", 1001, "moved to front with priority 1001 for topic-1"},
		{"reset", "e", "", 500, "priority reset to 500 for topic-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			change := tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: tc.wantPriority}
			f := priorityFixture(t, tuiclient.Task{ID: "task-1", Title: "Task", State: "ready", Kind: "implement", Priority: intPtr(300)})
			refresh := func() {
				f.setTasks(tuiclient.Task{ID: "task-1", Title: "Refreshed task", State: "ready", Kind: "implement", Priority: intPtr(tc.wantPriority)})
			}
			f.mock().SetTaskPriorityFunc = func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
				refresh()
				return change, nil
			}
			f.mock().MoveTaskToFrontFunc = func(ctx context.Context, id string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
				refresh()
				return change, nil
			}
			priorityTestFlow(f, tc.startKey, tc.value, "because")

			out := f.model.View().Content
			if strings.Contains(out, "Error:") || strings.Contains(out, "Press 'r' to retry") {
				t.Errorf("success rendered as an error:\n%s", out)
			}
			if !strings.Contains(out, tc.wantStatus) {
				t.Errorf("expected status %q in view:\n%s", tc.wantStatus, out)
			}
			if !strings.Contains(out, "Refreshed task") {
				t.Errorf("expected refreshed list row in view:\n%s", out)
			}
			if got := selectedPriority(f); got != tc.wantPriority {
				t.Errorf("expected refreshed list to hold priority %d, got %d", tc.wantPriority, got)
			}

			f.press("down")
			if strings.Contains(f.model.View().Content, tc.wantStatus) {
				t.Errorf("status should clear on next keypress")
			}
		})
	}
}
