package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTimeoutChecksSuccess(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create a task with await_checks=true
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID

	// Promote and submit to get to checks state
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "worker", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	submitted, err := store.SubmitTask(ctx, taskID, "worker", "done", nil, []LinkInput{
		{Kind: "pr", Value: "https://github.com/o/r/pull/1"},
	}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if submitted.State != "checks" {
		t.Fatalf("Expected state='checks', got %q", submitted.State)
	}

	// Timeout the checks
	note := "CI timeout after 1 hour"
	timedOut, err := store.TimeoutChecks(ctx, taskID, note)
	if err != nil {
		t.Fatalf("TimeoutChecks: %v", err)
	}

	if timedOut.State != "blocked" {
		t.Errorf("Expected state='blocked', got %q", timedOut.State)
	}
	if timedOut.ChecksStartedAt != nil {
		t.Errorf("Expected checks_started_at=nil, got %v", timedOut.ChecksStartedAt)
	}

	// Verify event is recorded
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "transition" && e.Note != nil && *e.Note == note {
			found = true
		}
	}
	if !found {
		t.Errorf("transition event with correct note not found")
	}
}

func TestTimeoutChecksNotInChecksState(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create a task in backlog state
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID

	// Try to timeout a task not in checks state
	note := "Should fail"
	_, err = store.TimeoutChecks(ctx, taskID, note)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("Expected ErrConflict, got %v", err)
	}
}

func TestTransitionChecksToBlocked(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create and submit a task to get to checks state
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID

	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "worker", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	submitted, err := store.SubmitTask(ctx, taskID, "worker", "done", nil, []LinkInput{
		{Kind: "pr", Value: "https://github.com/o/r/pull/1"},
	}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if submitted.ChecksStartedAt == nil {
		t.Fatalf("checks_started_at not set after submit")
	}

	// Transition to blocked
	note := "Manual block"
	blocked, err := store.TransitionTask(ctx, taskID, "blocked", &note)
	if err != nil {
		t.Fatalf("TransitionTask: %v", err)
	}

	if blocked.State != "blocked" {
		t.Errorf("Expected state='blocked', got %q", blocked.State)
	}
	if blocked.ChecksStartedAt != nil {
		t.Errorf("Expected checks_started_at=nil, got %v", blocked.ChecksStartedAt)
	}

	// Verify event is recorded
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "transition" && e.Note != nil && *e.Note == note {
			found = true
		}
	}
	if !found {
		t.Errorf("transition event with note not found")
	}
}

func TestTransitionChecksToFailed(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create and submit a task to get to checks state
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID

	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "worker", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "worker", "done", nil, []LinkInput{
		{Kind: "pr", Value: "https://github.com/o/r/pull/1"},
	}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// Transition to failed
	note := "Retired task"
	failed, err := store.TransitionTask(ctx, taskID, "failed", &note)
	if err != nil {
		t.Fatalf("TransitionTask: %v", err)
	}

	if failed.State != "failed" {
		t.Errorf("Expected state='failed', got %q", failed.State)
	}
	if failed.ChecksStartedAt != nil {
		t.Errorf("Expected checks_started_at=nil, got %v", failed.ChecksStartedAt)
	}
}

func TestTransitionChecksToSuperseded(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create and submit a task to get to checks state
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID

	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "worker", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "worker", "done", nil, []LinkInput{
		{Kind: "pr", Value: "https://github.com/o/r/pull/1"},
	}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}

	// Transition to superseded
	note := "Superseded by newer task"
	superseded, err := store.TransitionTask(ctx, taskID, "superseded", &note)
	if err != nil {
		t.Fatalf("TransitionTask: %v", err)
	}

	if superseded.State != "superseded" {
		t.Errorf("Expected state='superseded', got %q", superseded.State)
	}
	if superseded.ChecksStartedAt != nil {
		t.Errorf("Expected checks_started_at=nil, got %v", superseded.ChecksStartedAt)
	}
}

func TestTransitionChecksToReviewOperatorBypass(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create and submit a task to get to checks state
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID

	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "worker", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	submitted, err := store.SubmitTask(ctx, taskID, "worker", "done", nil, []LinkInput{
		{Kind: "pr", Value: "https://github.com/o/r/pull/1"},
	}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	if submitted.State != "checks" {
		t.Fatalf("Expected state='checks', got %q", submitted.State)
	}

	// Transition to review via operator bypass
	note := "Operator bypass"
	reviewed, err := store.TransitionTask(ctx, taskID, "review", &note)
	if err != nil {
		t.Fatalf("TransitionTask: %v", err)
	}

	if reviewed.State != "review" {
		t.Errorf("Expected state='review', got %q", reviewed.State)
	}
	if reviewed.ChecksStartedAt != nil {
		t.Errorf("Expected checks_started_at=nil, got %v", reviewed.ChecksStartedAt)
	}

	// Verify review tasks were created
	reviewTasks, err := store.ListTasks(ctx, projectID, TaskListFilter{Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(reviewTasks) == 0 {
		t.Errorf("Expected review tasks to be created")
	}

	// Verify event is recorded with "operator" actor
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "operator_bypass" && e.Actor == "operator" {
			found = true
			if e.Note == nil || *e.Note != note {
				t.Errorf("Event note = %v, want %q", e.Note, note)
			}
		}
	}
	if !found {
		t.Errorf("operator_bypass event with actor='operator' not found")
	}
}

func TestTransitionToReviewOnlyFromChecks(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create a task in backlog state
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID

	// Try to transition to review from backlog (should fail)
	_, err = store.TransitionTask(ctx, taskID, "review", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("Expected ErrConflict for review transition from backlog, got %v", err)
	}

	// Try from in_progress state
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "worker", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	_, err = store.TransitionTask(ctx, taskID, "review", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("Expected ErrConflict for review transition from in_progress, got %v", err)
	}
}

func TestOperatorBypassReviewTasksMatchPassChecks(t *testing.T) {
	ctx := context.Background()

	// Test PassChecks path
	store1, projectID1, docID1 := newBranchTestStore(t)
	tasks1, _ := store1.CreateTasks(ctx, projectID1, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID1, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true},
	})
	taskID1 := tasks1[0].ID
	store1.PromoteTask(ctx, taskID1)
	store1.ClaimTask(ctx, taskID1, "worker", "haiku", 5*time.Minute)
	store1.SubmitTask(ctx, taskID1, "worker", "done", nil, []LinkInput{
		{Kind: "pr", Value: "https://github.com/o/r/pull/1"},
	}, 5, nil, nil, testUnlimitedResearchBudget)
	_, _ = store1.PassChecks(ctx, taskID1, "abc123", "All checks passed")

	reviewTasks1, _ := store1.ListTasks(ctx, projectID1, TaskListFilter{Kind: ptrStr("review")})

	// Test operator bypass path
	store2, projectID2, docID2 := newBranchTestStore(t)
	tasks2, _ := store2.CreateTasks(ctx, projectID2, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID2, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true},
	})
	taskID2 := tasks2[0].ID
	store2.PromoteTask(ctx, taskID2)
	store2.ClaimTask(ctx, taskID2, "worker", "haiku", 5*time.Minute)
	store2.SubmitTask(ctx, taskID2, "worker", "done", nil, []LinkInput{
		{Kind: "pr", Value: "https://github.com/o/r/pull/1"},
	}, 5, nil, nil, testUnlimitedResearchBudget)
	_, _ = store2.TransitionTask(ctx, taskID2, "review", nil)

	reviewTasks2, _ := store2.ListTasks(ctx, projectID2, TaskListFilter{Kind: ptrStr("review")})

	// Both should create the same number of review tasks
	if len(reviewTasks1) != len(reviewTasks2) {
		t.Errorf("PassChecks created %d review tasks, operator bypass created %d", len(reviewTasks1), len(reviewTasks2))
	}

	// All review tasks should have same review_round as the parent
	for _, rt := range reviewTasks1 {
		if rt.ReviewRound != 1 {
			t.Errorf("PassChecks review task has review_round=%d, want 1", rt.ReviewRound)
		}
	}
	for _, rt := range reviewTasks2 {
		if rt.ReviewRound != 1 {
			t.Errorf("Operator bypass review task has review_round=%d, want 1", rt.ReviewRound)
		}
	}
}
