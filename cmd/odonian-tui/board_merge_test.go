package main

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
)

// approvedTestBucketed returns a bucketed task map with one approved task.
// The PR link is only available in the TaskDetail (fetched when entering detail mode).
func approvedTestBucketed(approvedTaskID string) map[string][]tuiclient.Task {
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "review", "approved", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["approved"] = []tuiclient.Task{
		{
			ID:    approvedTaskID,
			Title: "Approved Task",
			State: "approved",
		},
	}
	return bucketed
}

// buildApprovedModel returns a board model set up in the approved column with one approved
// task selected. approvedClient is the mock client to inject.
func buildApprovedModel(t *testing.T, approvedClient *tuiclient.MockClient) *BoardModel {
	t.Helper()

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "reviewer-bot",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test", Repo: "https://github.com/boldfield/odonian"}

	model := NewBoardModel(approvedClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 200, Height: 24})
	model = m.(*BoardModel)

	// Load an approved task into the board.
	m, _ = model.Update(tasksFetchedMsg{tasks: approvedTestBucketed("approved-task-1")})
	model = m.(*BoardModel)

	// Navigate to the approved column. Starting from in_progress, press right once per
	// column in stateOrder: checks, review, then approved.
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	model = m.(*BoardModel)
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	model = m.(*BoardModel)
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	model = m.(*BoardModel)

	if model.selectedColumn != columnIndex(stateApproved) {
		t.Fatalf("buildApprovedModel: expected selectedColumn %d (approved), got %d", columnIndex(stateApproved), model.selectedColumn)
	}
	if model.selectedTaskID != "approved-task-1" {
		t.Fatalf("buildApprovedModel: expected selectedTaskID approved-task-1, got %q", model.selectedTaskID)
	}

	return model
}

// TestBoardModel_MergePRConfirmFlow tests the merge PR confirmation flow in detail view.
// When a task in approved state is selected and 'm' is pressed, it should show a merge confirmation prompt.
func TestBoardModel_MergePRConfirmFlow(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{
				ID:    "approved-task-1",
				Title: "Approved Task",
				State: "approved",
				Links: []tuiclient.TaskLink{
					{Kind: "pr", Value: "owner/repo#123"},
				},
				Spec: "Task spec",
			}, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return []tuiclient.Document{}, nil
		},
	}

	model := buildApprovedModel(t, mockClient)

	// Enter detail mode by pressing enter on the approved task.
	m, detailCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail after enter, got mode %d", model.mode)
	}

	// Execute the detail fetch command to load the task detail.
	model = executeReviewCmd(t, model, detailCmd)

	// Press 'm' to start merge flow.
	m, _ = model.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	model = m.(*BoardModel)

	if model.mode != modeMergeConfirm {
		t.Fatalf("Expected modeMergeConfirm after 'm', got mode %d", model.mode)
	}

	if model.pendingPRURL != "owner/repo#123" {
		t.Errorf("Expected pendingPRURL 'owner/repo#123', got %q", model.pendingPRURL)
	}

	// Verify the output contains the merge confirmation prompt.
	output := model.View().Content
	if !strings.Contains(output, "owner/repo#123") {
		t.Errorf("Expected PR URL in merge confirmation, got:\n%s", output)
	}
}

// TestBoardModel_MergePRNoPRLink tests that pressing 'm' on an approved task with no PR link
// tries to resolve from the branch and shows an error if not found.
func TestBoardModel_MergePRNoPRLink(t *testing.T) {
	// Save and mock the command functions
	oldCommandContextFunc := commandContextFunc
	oldLookPathFunc := lookPathFunc
	defer func() {
		commandContextFunc = oldCommandContextFunc
		lookPathFunc = oldLookPathFunc
	}()

	// Mock lookPathFunc to simulate gh being available
	lookPathFunc = func(file string) (string, error) {
		return "/usr/bin/gh", nil
	}

	// Mock commandContextFunc to return empty PR list
	commandContextFunc = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.Command("echo", `[]`)
	}

	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{
				ID:    "approved-task-1",
				Title: "Approved Task",
				State: "approved",
				Links: []tuiclient.TaskLink{}, // No PR link
				Spec:  "Task spec",
			}, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return []tuiclient.Document{}, nil
		},
	}

	model := buildApprovedModel(t, mockClient)

	// Enter detail mode and try to merge.
	m, detailCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Execute the detail fetch command to load the task detail.
	model = executeReviewCmd(t, model, detailCmd)

	m, resolveCmd := model.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	model = m.(*BoardModel)

	// Should return a command to resolve the PR.
	// Execute the resolve command (which will try to find the PR and fail).
	model = executeReviewCmd(t, model, resolveCmd)

	// Should stay in detail mode with an error message containing "no PR".
	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail (merge not initiated), got mode %d", model.mode)
	}

	if !strings.Contains(model.detailMessage, "no PR") {
		t.Errorf("Expected 'no PR' message in %q", model.detailMessage)
	}
}

// TestBoardModel_MergePRSuccess tests the full merge and transition flow when confirmed with 'y'.
// The merge must be called first, then the transition to done only if merge succeeds.
func TestBoardModel_MergePRSuccess(t *testing.T) {
	var callLog []string
	var capturedTransitionID, capturedTransitionTo string
	var capturedMergePRURL string

	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{
				ID:    "approved-task-1",
				Title: "Approved Task",
				State: "approved",
				Links: []tuiclient.TaskLink{
					{Kind: "pr", Value: "owner/repo#123"},
				},
				Spec: "Task spec",
			}, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return []tuiclient.Document{}, nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			callLog = append(callLog, "transition")
			capturedTransitionID = id
			capturedTransitionTo = to
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			return []tuiclient.Task{
				{ID: "approved-task-1", Title: "Approved Task", State: "done"},
			}, nil
		},
	}

	model := buildApprovedModel(t, mockClient)

	// Enter detail mode and initiate merge.
	m, detailCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Execute the detail fetch command to load the task detail.
	model = executeReviewCmd(t, model, detailCmd)

	m, _ = model.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	model = m.(*BoardModel)

	// Confirm with 'y'.
	m, mergeCmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// After confirming, mode goes back to modeDetail (via cancelReviewMode).
	// The merge will be executed next, and on success will transition to modeNormal.

	// Inject a mock ghMerger that records the PR URL.
	model.ghMerger = func(ctx context.Context, prURL string) error {
		callLog = append(callLog, "merge")
		capturedMergePRURL = prURL
		return nil
	}

	// Execute the merge command.
	model = executeReviewCmd(t, model, mergeCmd)

	// After the merge and transition, we should be back in modeNormal.
	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after merge execution, got mode %d", model.mode)
	}

	// Verify call order: merge → transition → refetch.
	if len(callLog) != 3 {
		t.Fatalf("Expected 3 calls (merge, transition, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "merge" {
		t.Errorf("Expected first call to be 'merge', got %q", callLog[0])
	}
	if callLog[1] != "transition" {
		t.Errorf("Expected second call to be 'transition', got %q", callLog[1])
	}
	if callLog[2] != "refetch" {
		t.Errorf("Expected third call to be 'refetch', got %q", callLog[2])
	}

	// Verify merge was called with the correct PR URL.
	if capturedMergePRURL != "owner/repo#123" {
		t.Errorf("Expected merge with PR 'owner/repo#123', got %q", capturedMergePRURL)
	}

	// Verify transition was called with the correct ID and target state.
	if capturedTransitionID != "approved-task-1" {
		t.Errorf("TransitionTask: expected task ID approved-task-1, got %q", capturedTransitionID)
	}
	if capturedTransitionTo != "done" {
		t.Errorf("TransitionTask: expected to=done, got %q", capturedTransitionTo)
	}
}

// TestBoardModel_MergePRFailure tests that when the gh merge fails, the error is surfaced
// and the task is not transitioned to done.
func TestBoardModel_MergePRFailure(t *testing.T) {
	var callLog []string
	var transitionCalled bool

	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{
				ID:    "approved-task-1",
				Title: "Approved Task",
				State: "approved",
				Links: []tuiclient.TaskLink{
					{Kind: "pr", Value: "owner/repo#123"},
				},
				Spec: "Task spec",
			}, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return []tuiclient.Document{}, nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			transitionCalled = true
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			return []tuiclient.Task{
				{ID: "approved-task-1", Title: "Approved Task", State: "approved"},
			}, nil
		},
	}

	model := buildApprovedModel(t, mockClient)

	// Enter detail mode and initiate merge.
	m, detailCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Execute the detail fetch command to load the task detail.
	model = executeReviewCmd(t, model, detailCmd)

	m, _ = model.Update(tea.KeyPressMsg{Code: 'm', Text: "m"})
	model = m.(*BoardModel)

	// Confirm with 'y'.
	m, mergeCmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Inject a failing ghMerger.
	model.ghMerger = func(ctx context.Context, prURL string) error {
		callLog = append(callLog, "merge")
		return fmt.Errorf("merge rejected: PR has conflicts")
	}

	// Execute the merge command.
	model = executeReviewCmd(t, model, mergeCmd)

	// Verify merge was called but transition was not.
	if len(callLog) == 0 || callLog[0] != "merge" {
		t.Errorf("Expected merge to be called first, got %v", callLog)
	}

	if transitionCalled {
		t.Errorf("TransitionTask should not have been called after merge failure")
	}

	// Verify error message contains merge failure.
	if !strings.Contains(model.error, "merge rejected") {
		t.Errorf("Expected error to contain merge failure, got %q", model.error)
	}

	// Verify the task is still in approved state (refetch happened but no transition).
	output := model.View().Content
	if !strings.Contains(output, "approved(1)") {
		t.Errorf("Expected task to remain in approved state, got:\n%s", output)
	}
}

// TestBoardModel_ApprovedHelpBar tests that the help bar shows 'm merge & complete' when
// viewing an approved task in detail, and hides it elsewhere.
func TestBoardModel_ApprovedHelpBar(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{
				ID:    "approved-task-1",
				Title: "Approved Task",
				State: "approved",
				Links: []tuiclient.TaskLink{
					{Kind: "pr", Value: "owner/repo#123"},
				},
				Spec: "Task spec",
			}, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return []tuiclient.Document{}, nil
		},
	}

	model := buildApprovedModel(t, mockClient)

	// Enter detail mode on approved task.
	m, detailCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Execute the detail fetch command to load the task detail.
	model = executeReviewCmd(t, model, detailCmd)

	approvedOutput := model.View().Content
	if !strings.Contains(approvedOutput, "merge") {
		t.Errorf("Expected 'merge' in help bar for approved task, got:\n%s", approvedOutput)
	}

	// Exit detail mode and return to board.
	m, _ = model.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	model = m.(*BoardModel)

	// The help bar on the board should not show the merge action.
	boardOutput := model.View().Content
	if strings.Contains(boardOutput, "m merge") {
		t.Errorf("Did not expect 'm merge' in board help bar, got:\n%s", boardOutput)
	}
}

// TestBoardModel_ApprovedSendBackSuccess tests the approved→ready send-back flow.
// Pressing 'b' on an approved task enters the send-back flow with optional note input,
// then confirmation. On confirm with 'y', TransitionTask is called with "ready" (not ReviewTask).
// After success, the task moves to the ready column on refresh.
func TestBoardModel_ApprovedSendBackSuccess(t *testing.T) {
	var callLog []string
	var capturedTransitionID, capturedTransitionTo string
	var capturedTransitionNote *string

	mockClient := &tuiclient.MockClient{
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			callLog = append(callLog, "transition")
			capturedTransitionID = id
			capturedTransitionTo = to
			capturedTransitionNote = note
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			return []tuiclient.Task{
				{ID: "approved-task-1", Title: "Approved Task", State: "ready"},
			}, nil
		},
	}

	model := buildApprovedModel(t, mockClient)

	// Press 'b' to start the send-back flow.
	m, _ := model.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	model = m.(*BoardModel)

	if model.mode != modeApprovedRejectNote {
		t.Fatalf("Expected modeApprovedRejectNote after 'b', got mode %d", model.mode)
	}

	if model.pendingTaskID != "approved-task-1" {
		t.Errorf("Expected pendingTaskID approved-task-1, got %q", model.pendingTaskID)
	}

	// Enter a note and press enter to move to confirmation.
	model.reviewInput.SetValue("Conflict with main")
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeApprovedRejectConfirm {
		t.Fatalf("Expected modeApprovedRejectConfirm after enter, got mode %d", model.mode)
	}

	if model.pendingNote == nil || *model.pendingNote != "Conflict with main" {
		t.Errorf("Expected pendingNote 'Conflict with main', got %v", model.pendingNote)
	}

	// Confirm with 'y'.
	m, bounceCmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Execute the bounce command.
	model = executeReviewCmd(t, model, bounceCmd)

	// After the transition and refetch, we should be back in modeNormal.
	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after bounce execution, got mode %d", model.mode)
	}

	// Verify call order: transition → refetch (no ReviewTask).
	if len(callLog) != 2 {
		t.Fatalf("Expected 2 calls (transition, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "transition" {
		t.Errorf("Expected first call to be 'transition', got %q", callLog[0])
	}
	if callLog[1] != "refetch" {
		t.Errorf("Expected second call to be 'refetch', got %q", callLog[1])
	}

	// Verify transition was called with the correct ID, target state, and note.
	if capturedTransitionID != "approved-task-1" {
		t.Errorf("TransitionTask: expected task ID approved-task-1, got %q", capturedTransitionID)
	}
	if capturedTransitionTo != "ready" {
		t.Errorf("TransitionTask: expected to=ready, got %q", capturedTransitionTo)
	}
	if capturedTransitionNote == nil || *capturedTransitionNote != "Conflict with main" {
		t.Errorf("TransitionTask: expected note 'Conflict with main', got %v", capturedTransitionNote)
	}

	// Verify the task is now in the ready column.
	output := model.View().Content
	if !strings.Contains(output, "ready(1)") {
		t.Errorf("Expected task to be in ready column, got:\n%s", output)
	}
}

// TestBoardModel_ApprovedSendBackNoNote tests the send-back flow with no optional note.
func TestBoardModel_ApprovedSendBackNoNote(t *testing.T) {
	var capturedTransitionNote *string

	mockClient := &tuiclient.MockClient{
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			capturedTransitionNote = note
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "approved-task-1", Title: "Approved Task", State: "ready"},
			}, nil
		},
	}

	model := buildApprovedModel(t, mockClient)

	// Press 'b' to start the send-back flow.
	m, _ := model.Update(tea.KeyPressMsg{Code: 'b', Text: "b"})
	model = m.(*BoardModel)

	// Press enter without entering a note (empty string).
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// pendingNote should be nil when empty.
	if model.pendingNote != nil {
		t.Errorf("Expected pendingNote to be nil for empty input, got %v", model.pendingNote)
	}

	// Confirm with 'y'.
	m, bounceCmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Execute the bounce command.
	model = executeReviewCmd(t, model, bounceCmd)

	// Verify transition was called with nil note.
	if capturedTransitionNote != nil {
		t.Errorf("TransitionTask: expected note to be nil, got %v", capturedTransitionNote)
	}
}

// TestBoardModel_BounceOverlayRender tests that renderReviewOverlay returns non-empty,
// properly formatted output for both modeApprovedRejectNote and modeApprovedRejectConfirm.
func TestBoardModel_BounceOverlayRender(t *testing.T) {
	mockClient := &tuiclient.MockClient{}
	model := buildApprovedModel(t, mockClient)

	// Test modeApprovedRejectNote
	model.mode = modeApprovedRejectNote
	model.reviewInput.SetValue("Test note")
	overlayOutput := model.renderReviewOverlay()

	if overlayOutput == "" {
		t.Errorf("renderReviewOverlay returned empty string for modeApprovedRejectNote")
	}
	if !strings.Contains(overlayOutput, "Bounce back to ready") {
		t.Errorf("Expected 'Bounce back to ready' in modeApprovedRejectNote output, got:\n%s", overlayOutput)
	}
	if !strings.Contains(overlayOutput, "(enter to continue, esc to cancel)") {
		t.Errorf("Expected hint text in modeApprovedRejectNote output, got:\n%s", overlayOutput)
	}

	// Test modeApprovedRejectConfirm
	model.mode = modeApprovedRejectConfirm
	model.pendingNote = nil
	overlayOutput = model.renderReviewOverlay()

	if overlayOutput == "" {
		t.Errorf("renderReviewOverlay returned empty string for modeApprovedRejectConfirm")
	}
	if !strings.Contains(overlayOutput, "Send back to ready? [y/N]") {
		t.Errorf("Expected 'Send back to ready? [y/N]' in modeApprovedRejectConfirm output, got:\n%s", overlayOutput)
	}
	if !strings.Contains(overlayOutput, "(y to confirm, n/esc to cancel)") {
		t.Errorf("Expected hint text in modeApprovedRejectConfirm output, got:\n%s", overlayOutput)
	}
}
