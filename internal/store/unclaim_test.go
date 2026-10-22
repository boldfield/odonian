package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func unclaimFixture(t *testing.T) (Store, string) {
	t.Helper()
	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	proj, _ := st.CreateProject(ctx, "p", "https://github.com/example/repo")
	doc, _ := st.CreateDocument(ctx, proj.ID, "design", "D", "D.md", nil)
	tasks, err := st.CreateTasks(ctx, proj.ID, []TaskInput{{Title: "T", Spec: "s", DocumentID: doc.ID, Model: "haiku"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.PromoteTask(ctx, tasks[0].ID); err != nil {
		t.Fatalf("promote: %v", err)
	}
	return st, tasks[0].ID
}

func TestUnclaimTaskHandsClaimBack(t *testing.T) {
	st, id := unclaimFixture(t)
	ctx := context.Background()
	if _, err := st.ClaimTask(ctx, id, "agent-1", "haiku", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	task, err := st.UnclaimTask(ctx, id, "agent-1", "session limit")
	if err != nil {
		t.Fatalf("unclaim: %v", err)
	}
	if task.State != "ready" || task.Assignee != nil || task.LeaseExpiresAt != nil || task.ReviewRound != 0 {
		t.Fatalf("unexpected task after unclaim: %+v", task)
	}
	events, _ := st.ListEvents(ctx, id)
	last := events[len(events)-1]
	if last.Kind != "claim_released" || last.Actor != "agent-1" || last.Note == nil {
		t.Fatalf("expected claim_released event, got %+v", last)
	}
	// Claimable at once, by anyone.
	if _, err := st.ClaimTask(ctx, id, "agent-2", "haiku", time.Minute); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
}

func TestUnclaimTaskRefusals(t *testing.T) {
	st, id := unclaimFixture(t)
	ctx := context.Background()
	if _, err := st.UnclaimTask(ctx, id, "agent-1", "r"); !errors.Is(err, ErrConflict) {
		t.Fatalf("unclaim of ready task: want ErrConflict, got %v", err)
	}
	if _, err := st.UnclaimTask(ctx, "missing", "agent-1", "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := st.ClaimTask(ctx, id, "agent-1", "haiku", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.UnclaimTask(ctx, id, "agent-2", "r"); !errors.Is(err, ErrConflict) {
		t.Fatalf("non-owner: want ErrConflict, got %v", err)
	}
	if _, err := st.UnclaimTask(WithResearchAttempt(ctx, "stale"), id, "agent-1", "r"); conflictCode(err) != "ATTEMPT_FENCED" {
		t.Fatalf("stale attempt: want ATTEMPT_FENCED, got %v", err)
	}
	if got, _ := st.GetTask(ctx, id); got.State != "in_progress" {
		t.Fatalf("refused unclaim changed state to %q", got.State)
	}
	// A second unclaim after success is final.
	if _, err := st.UnclaimTask(ctx, id, "agent-1", "r"); err != nil {
		t.Fatalf("unclaim: %v", err)
	}
	if _, err := st.UnclaimTask(ctx, id, "agent-1", "r"); !errors.Is(err, ErrConflict) {
		t.Fatalf("repeat unclaim: want ErrConflict, got %v", err)
	}
}

func TestUnclaimTaskRefusedInReview(t *testing.T) {
	st, id := unclaimFixture(t)
	ctx := context.Background()
	if _, err := st.ClaimTask(ctx, id, "agent-1", "haiku", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := st.SubmitTask(ctx, id, "agent-1", "done", nil, []LinkInput{{Kind: "pr", Value: "https://github.com/example/repo/pull/1"}}, 3, nil, nil, 0); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, err := st.UnclaimTask(ctx, id, "agent-1", "r"); !errors.Is(err, ErrConflict) {
		t.Fatalf("task in review: want ErrConflict, got %v", err)
	}
}

func TestUnclaimReviewTask(t *testing.T) {
	st, id := unclaimFixture(t)
	ctx := context.Background()
	if _, err := st.Conn().ExecContext(ctx, `UPDATE task SET kind = 'review', review_round = 1, target_task_id = ? WHERE id = ?`, id, id); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if _, err := st.ClaimTask(ctx, id, "rev-1", "haiku", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	task, err := st.UnclaimTask(ctx, id, "rev-1", "session limit")
	if err != nil {
		t.Fatalf("unclaim: %v", err)
	}
	if task.State != "ready" || task.Assignee != nil || task.ReviewRound != 1 || task.TargetTaskID == nil || *task.TargetTaskID != id {
		t.Fatalf("unexpected review task after unclaim: %+v", task)
	}
}

func TestUnclaimMergeTaskWithoutRound(t *testing.T) {
	st, id := unclaimFixture(t)
	ctx := context.Background()
	if _, err := st.Conn().ExecContext(ctx, `UPDATE task SET kind = 'merge' WHERE id = ?`, id); err != nil {
		t.Fatalf("set kind: %v", err)
	}
	if _, err := st.ClaimTask(ctx, id, "m-1", "haiku", time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	task, err := st.UnclaimTask(ctx, id, "m-1", "r")
	if err != nil || task.State != "ready" {
		t.Fatalf("merge unclaim: %+v %v", task, err)
	}
}
