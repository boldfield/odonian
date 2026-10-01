package store

import (
	"context"
	"errors"
	"testing"
)

// approvedTaskInRound creates one task, forces it to approved in the given review round, and
// records that round's submitted commit as "abc".
func approvedTaskInRound(t *testing.T, reviewRound int) (Store, string) {
	t.Helper()
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", Branch: "shared"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'approved', review_round = ? WHERE id = ?", reviewRound, tasks[0].ID); err != nil {
		t.Fatalf("force approved: %v", err)
	}
	// The round's submission: commit "abc".
	if _, err := store.Conn().ExecContext(ctx, "INSERT INTO task_link (id, task_id, kind, value, review_round) VALUES (?, ?, 'commit', 'abc', ?)", GenerateID(), tasks[0].ID, reviewRound); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}
	return store, tasks[0].ID
}

func conflictCode(err error) string {
	var conflictErr *ConflictError
	if errors.As(err, &conflictErr) {
		return conflictErr.Code
	}
	return ""
}

func TestBeginLandingIsConditionalOnTheReviewRound(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 2)

	// An approve that prepared round 1 lost the race with a rework and re-approval.
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); conflictCode(err) != "STALE_REVIEW_ROUND" {
		t.Fatalf("expected STALE_REVIEW_ROUND, got %v", err)
	}
	if err := store.BeginLanding(ctx, taskID, 2, "abc", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding round 2: %v", err)
	}
	// A resumed approve re-reserving the same round and commit is a no-op; anything else is refused.
	if err := store.BeginLanding(ctx, taskID, 2, "abc", "attempt-1"); err != nil {
		t.Errorf("re-reserving the same landing: %v", err)
	}
	if err := store.BeginLanding(ctx, taskID, 2, "def", "attempt-1"); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("a different commit: expected LANDING_IN_PROGRESS, got %v", err)
	}

	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.LandingRound == nil || *task.LandingRound != 2 || task.LandingCommit == nil || *task.LandingCommit != "abc" {
		t.Errorf("GetTask landing = %v/%v, want 2/abc", task.LandingRound, task.LandingCommit)
	}
}

func TestBeginLandingRequiresApproved(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'review' WHERE id = ?", taskID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); conflictCode(err) != "NOT_APPROVED" {
		t.Errorf("expected NOT_APPROVED, got %v", err)
	}
	if err := store.BeginLanding(ctx, "no-such-task", 1, "abc", "attempt-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestLandingReservationOnlyAllowsDone(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}

	for _, target := range []string{"ready", "failed", "blocked", "abandoned", "superseded"} {
		if _, err := store.TransitionTask(ctx, taskID, target, nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
			t.Errorf("transition to %s while landing: expected LANDING_IN_PROGRESS, got %v", target, err)
		}
	}
	if _, err := store.SupersedeTask(ctx, taskID, nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("supersede while landing: expected LANDING_IN_PROGRESS, got %v", err)
	}

	// Not even to done by a plain transition: only the owning approve completes the landing.
	if _, err := store.TransitionTask(ctx, taskID, "done", nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("plain transition to done while landing: expected LANDING_IN_PROGRESS, got %v", err)
	}
	if _, err := store.CompleteLanding(ctx, taskID, "someone-else", nil); conflictCode(err) != "LANDING_ATTEMPT_MISMATCH" {
		t.Errorf("completing another attempt's landing: expected LANDING_ATTEMPT_MISMATCH, got %v", err)
	}
	if _, err := store.CompleteLanding(ctx, taskID, "attempt-1", nil); err != nil {
		t.Fatalf("the owning approve completes the landing: %v", err)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.LandingRound != nil || task.LandingCommit != nil {
		t.Errorf("done must clear the reservation, got %v/%v", task.LandingRound, task.LandingCommit)
	}
}

func TestCancelLandingAllowsRejectAgain(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}
	if err := store.CancelLanding(ctx, taskID, "attempt-1"); err != nil {
		t.Fatalf("CancelLanding: %v", err)
	}
	if _, err := store.TransitionTask(ctx, taskID, "ready", nil); err != nil {
		t.Errorf("reject after cancelling the landing: %v", err)
	}
	if err := store.CancelLanding(ctx, "no-such-task", "attempt-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCancelLandingOnlyCancelsTheOwningAttempt(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)

	// Approve A reserves, then stalls; approve B resumes the same landing and takes it over.
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-A"); err != nil {
		t.Fatalf("A reserves: %v", err)
	}
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-B"); err != nil {
		t.Fatalf("B takes over: %v", err)
	}

	// A comes back and tries to give the reservation back: it is B's now.
	if err := store.CancelLanding(ctx, taskID, "attempt-A"); conflictCode(err) != "LANDING_ATTEMPT_MISMATCH" {
		t.Fatalf("stale cancel: expected LANDING_ATTEMPT_MISMATCH, got %v", err)
	}
	if _, err := store.TransitionTask(ctx, taskID, "ready", nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("after the stale cancel the task must still be unrejectable, got %v", err)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.LandingAttempt == nil || *task.LandingAttempt != "attempt-B" {
		t.Errorf("landing attempt = %v, want attempt-B", task.LandingAttempt)
	}

	if err := store.CancelLanding(ctx, taskID, "attempt-B"); err != nil {
		t.Errorf("the owner's cancel: %v", err)
	}
	if err := store.CancelLanding(ctx, taskID, "attempt-A"); err != nil {
		t.Errorf("cancelling an unreserved task is a no-op, got %v", err)
	}
}
