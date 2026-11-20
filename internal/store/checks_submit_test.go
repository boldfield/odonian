package store

import (
	"context"
	"testing"
	"time"
)

func newChecksSubmitHarness(t *testing.T, awaitChecks bool) *reviewRoundHarness {
	t.Helper()
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: awaitChecks},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	if _, err := store.PromoteTask(ctx, tasks[0].ID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	return &reviewRoundHarness{t: t, store: store, projectID: projectID, taskID: tasks[0].ID}
}

func reviewTaskCount(t *testing.T, h *reviewRoundHarness) int {
	t.Helper()
	tasks, err := h.store.ListTasks(context.Background(), h.projectID, TaskListFilter{Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	return len(tasks)
}

// submitReturning is submit, but returns what SubmitTask reports.
func submitReturning(t *testing.T, h *reviewRoundHarness, links ...LinkInput) TaskWithDepsAndLinks {
	t.Helper()
	ctx := context.Background()
	if _, err := h.store.ClaimTask(ctx, h.taskID, "worker", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	got, err := h.store.SubmitTask(ctx, h.taskID, "worker", "done", nil, links, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	return got
}

func spawnReviewEvents(t *testing.T, h *reviewRoundHarness) int {
	t.Helper()
	events, err := h.store.ListEvents(context.Background(), h.taskID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == "spawn_review" {
			n++
		}
	}
	return n
}

func TestSubmitAwaitChecksWithPREntersChecks(t *testing.T) {
	ctx := context.Background()
	h := newChecksSubmitHarness(t, true)
	returned := submitReturning(t, h, LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/1"})
	if returned.State != "checks" || returned.ChecksStartedAt == nil {
		t.Errorf("returned state = %q, checks_started_at = %v; want checks and set", returned.State, returned.ChecksStartedAt)
	}
	if n := spawnReviewEvents(t, h); n != 0 {
		t.Errorf("spawn_review events = %d, want 0", n)
	}

	task := h.task()
	if task.State != "checks" {
		t.Fatalf("state = %q, want checks", task.State)
	}
	if task.ChecksStartedAt == nil {
		t.Errorf("checks_started_at not set")
	}
	if task.ReviewRound != 1 {
		t.Errorf("review_round = %d, want 1", task.ReviewRound)
	}
	if got := linkRounds(task, "pr"); got["https://github.com/o/r/pull/1"] != 1 {
		t.Errorf("pr link rounds = %v, want round 1", got)
	}
	if n := reviewTaskCount(t, h); n != 0 {
		t.Errorf("review tasks = %d, want 0", n)
	}

	// A task in checks is neither claimable nor claimed.
	ready, err := h.store.ListTasks(ctx, h.projectID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	for _, r := range ready {
		if r.ID == h.taskID {
			t.Errorf("task in checks returned by the claimable filter")
		}
	}
	if _, err := h.store.ClaimTask(ctx, h.taskID, "worker2", "haiku", time.Minute); err == nil {
		t.Errorf("claim on a task in checks succeeded")
	}
}

func TestSubmitAwaitChecksFallsBackToReview(t *testing.T) {
	cases := []struct {
		name        string
		awaitChecks bool
		links       []LinkInput
	}{
		{"opted-in no_op", true, []LinkInput{{Kind: "no_op", Value: "already satisfied"}}},
		{"opted-in no pr link", true, []LinkInput{{Kind: "commit", Value: "c1"}}},
		{"not opted in", false, []LinkInput{{Kind: "pr", Value: "https://github.com/o/r/pull/1"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newChecksSubmitHarness(t, tc.awaitChecks)
			returned := submitReturning(t, h, tc.links...)
			if returned.State != "review" {
				t.Errorf("returned state = %q, want review", returned.State)
			}
			if n := spawnReviewEvents(t, h); n != 1 {
				t.Errorf("spawn_review events = %d, want 1", n)
			}
			task := h.task()
			if task.State != "review" {
				t.Errorf("state = %q, want review", task.State)
			}
			if task.ChecksStartedAt != nil {
				t.Errorf("checks_started_at = %v, want nil", *task.ChecksStartedAt)
			}
			if n := reviewTaskCount(t, h); n != 1 {
				t.Errorf("review tasks = %d, want 1", n)
			}
		})
	}
}

func TestPassChecksCreatesReviewTasks(t *testing.T) {
	ctx := context.Background()
	h := newChecksSubmitHarness(t, true)

	// Submit with PR link to enter checks
	submitReturning(t, h, LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/1"})
	task := h.task()
	if task.State != "checks" {
		t.Fatalf("state = %q, want checks", task.State)
	}

	// Call PassChecks
	passedTask, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
	if err != nil {
		t.Fatalf("PassChecks: %v", err)
	}

	// Verify state transition
	if passedTask.State != "review" {
		t.Errorf("returned state = %q, want review", passedTask.State)
	}
	if passedTask.ChecksStartedAt != nil {
		t.Errorf("returned checks_started_at = %v, want nil", passedTask.ChecksStartedAt)
	}

	// Verify review round was not incremented again
	if passedTask.ReviewRound != 1 {
		t.Errorf("review_round = %d, want 1 (not incremented by PassChecks)", passedTask.ReviewRound)
	}

	// Verify review tasks were created
	if n := reviewTaskCount(t, h); n != 1 {
		t.Errorf("review tasks = %d, want 1", n)
	}

	// Verify checks_passed event was created with head SHA and summary
	events, err := h.store.ListEvents(ctx, h.taskID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	foundEvent := false
	for _, e := range events {
		if e.Kind == "checks_passed" {
			foundEvent = true
			if e.Note == nil || (*e.Note != "head commit: abc123, summary: All checks passed") {
				t.Errorf("checks_passed note = %v, want 'head commit: abc123, summary: All checks passed'", e.Note)
			}
		}
	}
	if !foundEvent {
		t.Errorf("checks_passed event not found")
	}

	// Verify spawn_review event was created
	if n := spawnReviewEvents(t, h); n != 1 {
		t.Errorf("spawn_review events = %d, want 1", n)
	}
}

func TestPassChecksReviewTasksMatchDirectSubmit(t *testing.T) {
	ctx := context.Background()

	// Create two identical tasks, one will go through checks, one will submit directly
	h1 := newChecksSubmitHarness(t, true)
	h2 := newChecksSubmitHarness(t, false)

	prLink := LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/42"}

	// h1: Submit with await_checks to enter checks, then PassChecks
	submitReturning(t, h1, prLink)
	_, err := h1.store.PassChecks(ctx, h1.taskID, "abc123", "All checks passed")
	if err != nil {
		t.Fatalf("PassChecks: %v", err)
	}

	// h2: Direct submit (no await_checks)
	submitReturning(t, h2, prLink)

	// Get review tasks from both
	getReviewTasks := func(projectID, taskID string, store Store) []Task {
		t.Helper()
		tasks, err := store.ListTasks(ctx, projectID, TaskListFilter{Kind: ptrStr("review")})
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		// Filter by parent task ID
		var result []Task
		for _, task := range tasks {
			if task.TargetTaskID != nil && *task.TargetTaskID == taskID {
				result = append(result, task)
			}
		}
		return result
	}

	reviewTasks1 := getReviewTasks(h1.projectID, h1.taskID, h1.store)
	reviewTasks2 := getReviewTasks(h2.projectID, h2.taskID, h2.store)

	if len(reviewTasks1) != len(reviewTasks2) {
		t.Fatalf("review task count mismatch: PassChecks=%d, direct submit=%d", len(reviewTasks1), len(reviewTasks2))
	}
	if len(reviewTasks1) == 0 {
		t.Fatalf("no review tasks created")
	}

	// Compare review task fields
	task1 := reviewTasks1[0]
	task2 := reviewTasks2[0]

	if task1.Model != task2.Model {
		t.Errorf("model mismatch: PassChecks=%q, direct submit=%q", task1.Model, task2.Model)
	}
	if task1.ReviewRound != task2.ReviewRound {
		t.Errorf("review_round mismatch: PassChecks=%d, direct submit=%d", task1.ReviewRound, task2.ReviewRound)
	}
	if task1.Priority != task2.Priority {
		t.Errorf("priority mismatch: PassChecks=%d, direct submit=%d", task1.Priority, task2.Priority)
	}
}

func TestPassChecksSecondCallReturnsConflict(t *testing.T) {
	ctx := context.Background()
	h := newChecksSubmitHarness(t, true)

	// Submit with PR link to enter checks
	submitReturning(t, h, LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/1"})

	// First call succeeds
	_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
	if err != nil {
		t.Fatalf("first PassChecks: %v", err)
	}

	// Second call should return ErrConflict
	_, err = h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
	if err != ErrConflict {
		t.Errorf("second PassChecks returned %v, want ErrConflict", err)
	}

	// Verify no additional events or review tasks were created
	if n := reviewTaskCount(t, h); n != 1 {
		t.Errorf("review tasks = %d, want 1", n)
	}
}

func TestPassChecksNonChecksStateReturnsConflict(t *testing.T) {
	ctx := context.Background()

	t.Run("ready", func(t *testing.T) {
		h := newChecksSubmitHarness(t, false)
		task := h.task()
		if task.State != "ready" {
			t.Fatalf("initial state = %q, want ready", task.State)
		}

		// PassChecks on ready state should return ErrConflict
		_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
		if err != ErrConflict {
			t.Errorf("PassChecks on ready state returned %v, want ErrConflict", err)
		}

		// Verify task state unchanged
		updatedTask := h.task()
		if updatedTask.State != "ready" {
			t.Errorf("state changed to %q after PassChecks conflict", updatedTask.State)
		}
	})

	t.Run("in_progress", func(t *testing.T) {
		h := newChecksSubmitHarness(t, false)
		// Claim task to move it to in_progress
		if _, err := h.store.ClaimTask(ctx, h.taskID, "worker", "haiku", 5*time.Minute); err != nil {
			t.Fatalf("ClaimTask: %v", err)
		}
		task := h.task()
		if task.State != "in_progress" {
			t.Fatalf("initial state = %q, want in_progress", task.State)
		}

		// PassChecks on in_progress state should return ErrConflict
		_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
		if err != ErrConflict {
			t.Errorf("PassChecks on in_progress state returned %v, want ErrConflict", err)
		}

		// Verify task state unchanged
		updatedTask := h.task()
		if updatedTask.State != "in_progress" {
			t.Errorf("state changed to %q after PassChecks conflict", updatedTask.State)
		}
	})

	t.Run("review", func(t *testing.T) {
		h := newChecksSubmitHarness(t, false)
		// Submit directly (without await_checks) to move to review
		submitReturning(t, h, LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/1"})
		task := h.task()
		if task.State != "review" {
			t.Fatalf("initial state = %q, want review", task.State)
		}

		// PassChecks on review state should return ErrConflict
		_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
		if err != ErrConflict {
			t.Errorf("PassChecks on review state returned %v, want ErrConflict", err)
		}

		// Verify task state unchanged
		updatedTask := h.task()
		if updatedTask.State != "review" {
			t.Errorf("state changed to %q after PassChecks conflict", updatedTask.State)
		}
	})
}

func TestPassChecksClearsChecksStartedAt(t *testing.T) {
	ctx := context.Background()
	h := newChecksSubmitHarness(t, true)

	// Submit with PR link to enter checks
	submitReturning(t, h, LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/1"})
	task := h.task()
	if task.ChecksStartedAt == nil {
		t.Fatalf("checks_started_at not set before PassChecks")
	}

	// Call PassChecks
	passedTask, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
	if err != nil {
		t.Fatalf("PassChecks: %v", err)
	}

	if passedTask.ChecksStartedAt != nil {
		t.Errorf("checks_started_at = %v, want nil", passedTask.ChecksStartedAt)
	}

	// Verify it's also nil in the database
	dbTask := h.task()
	if dbTask.ChecksStartedAt != nil {
		t.Errorf("checks_started_at in db = %v, want nil", dbTask.ChecksStartedAt)
	}
}

func TestPassChecksLinksRestrictedToCurrentRound(t *testing.T) {
	ctx := context.Background()
	h := newChecksSubmitHarness(t, true)

	oldPRLink := LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/1"}

	// Submit with old PR link to enter checks
	submitReturning(t, h, oldPRLink)

	// Call PassChecks
	_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
	if err != nil {
		t.Fatalf("PassChecks: %v", err)
	}

	// Get the review task and verify it has the correct PR link
	allReviewTasks, err := h.store.ListTasks(ctx, h.projectID, TaskListFilter{Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	var reviewTasks []Task
	for _, task := range allReviewTasks {
		if task.TargetTaskID != nil && *task.TargetTaskID == h.taskID {
			reviewTasks = append(reviewTasks, task)
		}
	}
	if len(reviewTasks) != 1 {
		t.Fatalf("expected 1 review task, got %d", len(reviewTasks))
	}

	// Verify the review task spec includes the correct PR link
	if !contains(reviewTasks[0].Spec, oldPRLink.Value) {
		t.Errorf("review task spec does not contain PR link %q", oldPRLink.Value)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
