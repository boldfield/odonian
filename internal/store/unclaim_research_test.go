package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/policy"
)

func TestUnclaimResearchStaleAttemptRefused(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	old := f.mustClaim("old", id, "agent-a")
	f.clock.Advance(claimTTL + time.Second)
	cur := f.mustClaim("new", id, "agent-a")

	ctx := WithResearchAttempt(f.ctx, old.Grant.Attempt.ID)
	_, err := f.s.UnclaimTask(ctx, id, "agent-a", "stale")
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Code != "ATTEMPT_FENCED" {
		t.Fatalf("stale attempt: %v, want ATTEMPT_FENCED", err)
	}
	if tk := f.get(id); tk.State != "in_progress" || tk.Assignee == nil {
		t.Fatalf("task changed: %+v", tk)
	}
	if n := f.count(`SELECT COUNT(*) FROM research_attempt WHERE id = ? AND state = 'active'`, cur.Grant.Attempt.ID); n != 1 {
		t.Fatalf("live attempt no longer active")
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE task_id = ? AND kind = 'claim_released'`, id); n != 0 {
		t.Fatalf("claim_released recorded on refusal")
	}
}

func TestUnclaimResearchFinalizesLiveAttempt(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 1, 0))
	id := f.research()
	res := f.mustClaim("r1", id, "agent-a")
	attemptID := res.Grant.Attempt.ID
	afterClaim := f.tokens()
	if f.active() != 1 {
		t.Fatalf("active = %d, want 1", f.active())
	}

	task, err := f.s.UnclaimTask(WithResearchAttempt(f.ctx, attemptID), id, "agent-a", "session limit")
	if err != nil {
		t.Fatalf("UnclaimTask: %v", err)
	}
	if task.State != "ready" || task.Assignee != nil || task.LeaseExpiresAt != nil {
		t.Fatalf("task not handed back: %+v", task)
	}
	if n := f.count(`SELECT COUNT(*) FROM research_attempt WHERE id = ? AND state = 'finalized' AND exit_class = ?`, attemptID, ExitCancelled); n != 1 {
		t.Fatalf("attempt not finalized as cancelled")
	}
	if f.active() != 0 {
		t.Fatalf("active = %d, want 0", f.active())
	}
	if got := f.tokens(); got > afterClaim+0.01 {
		t.Fatalf("tokens refunded: %v -> %v", afterClaim, got)
	}
	var note string
	if err := f.s.Conn().QueryRow(`SELECT note FROM event WHERE task_id = ? AND kind = 'claim_released'`, id).Scan(&note); err != nil {
		t.Fatalf("claim_released event: %v", err)
	}
	if !strings.Contains(note, attemptID) {
		t.Fatalf("note %q lacks attempt id %s", note, attemptID)
	}
	// the freed slot is claimable at once (concurrency limit is 1)
	f.mustClaim("r2", id, "agent-b")
}

func TestUnclaimResearchRequiresLiveAttemptID(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	res := f.mustClaim("r1", id, "agent-a")

	_, err := f.s.UnclaimTask(f.ctx, id, "agent-a", "no attempt")
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Code != "ATTEMPT_FENCED" {
		t.Fatalf("omitted attempt: %v, want ATTEMPT_FENCED", err)
	}
	_, err = f.s.UnclaimTask(WithResearchAttempt(f.ctx, "not-an-attempt"), id, "agent-a", "bogus")
	if !errors.As(err, &ce) || ce.Code != "ATTEMPT_FENCED" {
		t.Fatalf("non-live attempt: %v, want ATTEMPT_FENCED", err)
	}
	if tk := f.get(id); tk.State != "in_progress" || tk.Assignee == nil {
		t.Fatalf("task changed: %+v", tk)
	}
	if n := f.count(`SELECT COUNT(*) FROM research_attempt WHERE id = ? AND state = 'active'`, res.Grant.Attempt.ID); n != 1 {
		t.Fatalf("live attempt no longer active")
	}
}
