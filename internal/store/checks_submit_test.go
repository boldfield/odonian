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
