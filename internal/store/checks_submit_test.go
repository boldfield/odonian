package store

import (
	"context"
	"errors"
	"strings"
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

	// Compare review task fields - should match exactly except for target task ID
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
	if task1.Title != task2.Title {
		t.Errorf("title mismatch: PassChecks=%q, direct submit=%q", task1.Title, task2.Title)
	}

	// Compare spec text, normalized for different parent task IDs
	spec1 := normalizeSpec(task1.Spec, h1.taskID, "PARENT_ID")
	spec2 := normalizeSpec(task2.Spec, h2.taskID, "PARENT_ID")
	if spec1 != spec2 {
		t.Errorf("spec mismatch (after normalization):\nPassChecks=%q\ndirect submit=%q", spec1, spec2)
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

	// Count events and review tasks after first call
	firstEvents, _ := h.store.ListEvents(ctx, h.taskID)
	firstReviewCount := reviewTaskCount(t, h)
	firstSpawnReviewCount := spawnReviewEvents(t, h)

	// Second call should return ErrConflict and not change state
	_, err = h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
	if !errors.Is(err, ErrConflict) {
		t.Errorf("second PassChecks returned %v, want ErrConflict", err)
	}

	// Verify state is still review
	task := h.task()
	if task.State != "review" {
		t.Errorf("state after second call = %q, want review", task.State)
	}

	// Verify no additional events or review tasks were created
	secondEvents, _ := h.store.ListEvents(ctx, h.taskID)
	if len(secondEvents) != len(firstEvents) {
		t.Errorf("events after second call = %d, want %d (no change)", len(secondEvents), len(firstEvents))
	}
	if n := reviewTaskCount(t, h); n != firstReviewCount {
		t.Errorf("review tasks after second call = %d, want %d (no change)", n, firstReviewCount)
	}
	if n := spawnReviewEvents(t, h); n != firstSpawnReviewCount {
		t.Errorf("spawn_review events after second call = %d, want %d (no change)", n, firstSpawnReviewCount)
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

		// Count initial events and review tasks
		initialEvents, _ := h.store.ListEvents(ctx, h.taskID)
		initialReviewTasks := reviewTaskCount(t, h)

		// PassChecks on ready state should return ErrConflict
		_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
		if !errors.Is(err, ErrConflict) {
			t.Errorf("PassChecks on ready state returned %v, want ErrConflict", err)
		}

		// Verify task state unchanged
		updatedTask := h.task()
		if updatedTask.State != "ready" {
			t.Errorf("state changed to %q after PassChecks conflict", updatedTask.State)
		}

		// Verify no review tasks or events were added
		finalEvents, _ := h.store.ListEvents(ctx, h.taskID)
		if len(finalEvents) != len(initialEvents) {
			t.Errorf("events changed from %d to %d", len(initialEvents), len(finalEvents))
		}
		if n := reviewTaskCount(t, h); n != initialReviewTasks {
			t.Errorf("review tasks changed from %d to %d", initialReviewTasks, n)
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

		// Count initial events and review tasks
		initialEvents, _ := h.store.ListEvents(ctx, h.taskID)
		initialReviewTasks := reviewTaskCount(t, h)

		// PassChecks on in_progress state should return ErrConflict
		_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
		if !errors.Is(err, ErrConflict) {
			t.Errorf("PassChecks on in_progress state returned %v, want ErrConflict", err)
		}

		// Verify task state unchanged
		updatedTask := h.task()
		if updatedTask.State != "in_progress" {
			t.Errorf("state changed to %q after PassChecks conflict", updatedTask.State)
		}

		// Verify no review tasks or events were added
		finalEvents, _ := h.store.ListEvents(ctx, h.taskID)
		if len(finalEvents) != len(initialEvents) {
			t.Errorf("events changed from %d to %d", len(initialEvents), len(finalEvents))
		}
		if n := reviewTaskCount(t, h); n != initialReviewTasks {
			t.Errorf("review tasks changed from %d to %d", initialReviewTasks, n)
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

		// Count initial events and review tasks
		initialEvents, _ := h.store.ListEvents(ctx, h.taskID)
		initialReviewTasks := reviewTaskCount(t, h)

		// PassChecks on review state should return ErrConflict
		_, err := h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
		if !errors.Is(err, ErrConflict) {
			t.Errorf("PassChecks on review state returned %v, want ErrConflict", err)
		}

		// Verify task state unchanged
		updatedTask := h.task()
		if updatedTask.State != "review" {
			t.Errorf("state changed to %q after PassChecks conflict", updatedTask.State)
		}

		// Verify no review tasks or events were added
		finalEvents, _ := h.store.ListEvents(ctx, h.taskID)
		if len(finalEvents) != len(initialEvents) {
			t.Errorf("events changed from %d to %d", len(initialEvents), len(finalEvents))
		}
		if n := reviewTaskCount(t, h); n != initialReviewTasks {
			t.Errorf("review tasks changed from %d to %d", initialReviewTasks, n)
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

	// Submit with PR link to enter checks
	currentPRLink := LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/42"}
	submitReturning(t, h, currentPRLink)

	// Get the task to access its current review_round
	task := h.task()
	currentRound := task.ReviewRound

	// Manually insert a stale link from an earlier round to test filtering
	stalePRLink := "https://github.com/o/r/pull/1"
	_, err := h.store.Conn().ExecContext(ctx, `
		INSERT INTO task_link (id, task_id, review_round, kind, value)
		VALUES (?, ?, ?, ?, ?)
	`, "stale-link-id", h.taskID, currentRound-1, "pr", stalePRLink)
	if err != nil {
		t.Fatalf("failed to insert stale link: %v", err)
	}

	// Call PassChecks
	_, err = h.store.PassChecks(ctx, h.taskID, "abc123", "All checks passed")
	if err != nil {
		t.Fatalf("PassChecks: %v", err)
	}

	// Get the review task and verify it has only the current round's PR link
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

	// Verify the review task spec includes the current PR link
	if !strings.Contains(reviewTasks[0].Spec, currentPRLink.Value) {
		t.Errorf("review task spec does not contain current PR link %q", currentPRLink.Value)
	}

	// Verify the review task spec does NOT include the stale PR link
	if strings.Contains(reviewTasks[0].Spec, stalePRLink) {
		t.Errorf("review task spec incorrectly contains stale PR link %q", stalePRLink)
	}
}

func normalizeSpec(spec, taskID, replacement string) string {
	// Replace the task ID with a placeholder so we can compare specs from different tasks
	return strings.ReplaceAll(spec, taskID, replacement)
}

func failChecksHarness(t *testing.T, escalate bool) *reviewRoundHarness {
	t.Helper()
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, AwaitChecks: true, Escalate: &escalate},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	if _, err := store.PromoteTask(ctx, tasks[0].ID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	h := &reviewRoundHarness{t: t, store: store, projectID: projectID, taskID: tasks[0].ID}
	submitReturning(t, h, LinkInput{Kind: "pr", Value: "https://github.com/o/r/pull/1"})
	if h.task().State != "checks" {
		t.Fatalf("state = %q, want checks", h.task().State)
	}
	return h
}

func TestFailChecksBelowThresholdReturnsToReady(t *testing.T) {
	ctx := context.Background()
	h := failChecksHarness(t, true)
	got, err := h.store.FailChecks(ctx, h.taskID, "abc123", "lint (https://ci/1)", 5, nil, nil)
	if err != nil {
		t.Fatalf("FailChecks: %v", err)
	}
	if got.State != "ready" || got.Assignee != nil || got.LeaseExpiresAt != nil || got.ChecksStartedAt != nil {
		t.Errorf("task = state %q assignee %v lease %v checks_started_at %v; want ready, unassigned, no lease, no checks_started_at", got.State, got.Assignee, got.LeaseExpiresAt, got.ChecksStartedAt)
	}
	if got.ReviewRound != 1 {
		t.Errorf("review_round = %d, want 1", got.ReviewRound)
	}
	events, err := h.store.ListEvents(ctx, h.taskID)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "checks_failed" {
			found = true
			if e.Note == nil || *e.Note != "head commit: abc123, summary: lint (https://ci/1)" {
				t.Errorf("checks_failed note = %v", e.Note)
			}
		}
	}
	if !found {
		t.Errorf("checks_failed event not found")
	}
}

func TestFailChecksPastThresholdBlocksWithoutEscalation(t *testing.T) {
	h := failChecksHarness(t, false)
	got, err := h.store.FailChecks(context.Background(), h.taskID, "abc123", "lint", 5, map[string]int{"haiku": 0}, nil)
	if err != nil {
		t.Fatalf("FailChecks: %v", err)
	}
	if got.State != "blocked" || got.ReviewRound != 1 {
		t.Errorf("state = %q review_round = %d, want blocked and 1", got.State, got.ReviewRound)
	}
}

func TestFailChecksPastThresholdEscalates(t *testing.T) {
	ctx := context.Background()
	h := failChecksHarness(t, true)
	got, err := h.store.FailChecks(ctx, h.taskID, "abc123", "lint (https://ci/1)", 5, map[string]int{"haiku": 0}, nil)
	if err != nil {
		t.Fatalf("FailChecks: %v", err)
	}
	if got.State != "superseded" || got.SupersededBy == nil {
		t.Fatalf("state = %q superseded_by = %v, want superseded with replacement", got.State, got.SupersededBy)
	}
	repl, err := h.store.GetTask(ctx, *got.SupersededBy)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if repl.State != "ready" || repl.Model != "sonnet" {
		t.Errorf("replacement state = %q model = %q, want ready sonnet", repl.State, repl.Model)
	}
	if !strings.Contains(repl.Spec, "Prior attempt feedback") || !strings.Contains(repl.Spec, "lint (https://ci/1)") {
		t.Errorf("replacement spec lacks failure summary: %q", repl.Spec)
	}
}

func TestFailChecksOutsideChecksConflicts(t *testing.T) {
	ctx := context.Background()
	h := newChecksSubmitHarness(t, true)
	if _, err := h.store.FailChecks(ctx, h.taskID, "abc123", "lint", 5, nil, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if h.task().State != "ready" {
		t.Errorf("state changed to %q", h.task().State)
	}
	events, _ := h.store.ListEvents(ctx, h.taskID)
	for _, e := range events {
		if e.Kind == "checks_failed" {
			t.Errorf("checks_failed event recorded on conflict")
		}
	}
}

func TestFailChecksEscalationIncludesHeadCommitInSpec(t *testing.T) {
	ctx := context.Background()
	h := failChecksHarness(t, true)
	got, err := h.store.FailChecks(ctx, h.taskID, "deadbeef1234", "lint failed (https://ci/1)", 5, map[string]int{"haiku": 0}, nil)
	if err != nil {
		t.Fatalf("FailChecks: %v", err)
	}
	if got.State != "superseded" || got.SupersededBy == nil {
		t.Fatalf("state = %q superseded_by = %v, want superseded with replacement", got.State, got.SupersededBy)
	}
	
	repl, err := h.store.GetTask(ctx, *got.SupersededBy)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	
	// Debug output
	t.Logf("Replacement spec:\n%s\n", repl.Spec)
	
	// Verify the replacement spec contains both the head commit and the summary
	if !strings.Contains(repl.Spec, "Prior attempt feedback") {
		t.Errorf("replacement spec lacks 'Prior attempt feedback' block")
	}
	if !strings.Contains(repl.Spec, "deadbeef1234") {
		t.Errorf("replacement spec lacks head commit 'deadbeef1234'")
	}
	if !strings.Contains(repl.Spec, "lint failed (https://ci/1)") {
		t.Errorf("replacement spec lacks failure summary 'lint failed (https://ci/1)'")
	}
	if !strings.Contains(repl.Spec, "checks failed") {
		t.Errorf("replacement spec lacks 'checks failed' marker")
	}
}
