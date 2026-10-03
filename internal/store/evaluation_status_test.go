package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestEvaluationCampaignStatusCountsAttemptsAndPools(t *testing.T) {
	ctx := context.Background()
	st, clock, f := newEvalClockFixture(t, 3, 10)
	other := newPoolTestCandidateStored(t, st, f.campaign, "fakeB", "unconfigured-pool", 2)
	more := newPoolTestSamples2(t, st, f.campaign, "taskB")

	first, err := claimEval(st, f.sample, f.cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimEval(st, more, f.cand, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := finalizeEval(st, first.Attempt.ID, EvalExitCompleted); err != nil {
		t.Fatal(err)
	}
	// An expired attempt still counts against the caps but is no longer active.
	clock.Advance(2 * time.Minute)

	status, err := st.GetEvaluationCampaignStatus(ctx, f.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if status.TotalAttemptsUsed != 2 || len(status.Candidates) != 2 {
		t.Fatalf("status: %+v", status)
	}
	by := map[string]EvaluationCandidateStatus{}
	for _, c := range status.Candidates {
		by[c.Candidate.ID] = c
	}
	a, b := by[f.cand.ID], by[other.ID]
	if a.AttemptsUsed != 2 || a.Active != 0 || a.Pool == nil || a.Pool.ID != "pool1" {
		t.Fatalf("candidate a: %+v", a)
	}
	if b.AttemptsUsed != 0 || b.Pool != nil {
		t.Fatalf("a candidate with an unconfigured pool must report no pool, not unlimited: %+v", b)
	}
	if _, err := st.GetEvaluationCampaignStatus(ctx, "missing"); !errors.Is(err, ErrEvaluationCampaignNotFound) {
		t.Fatalf("missing campaign: %v", err)
	}
}

func TestEvaluationPauseBlocksAdmissionBeforeCapacityChecks(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 1)
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "pool1", 1)
	samples := newPoolTestSamples(t, st, c, 2)

	first, err := claimEval(st, samples[0], cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeEval(st, first.Attempt.ID, EvalExitCompleted); err != nil {
		t.Fatal(err)
	}
	// The cap is spent, but a paused campaign reports the pause, not exhaustion.
	if _, err := claimEval(st, samples[1], cand, time.Minute); !errors.Is(err, ErrEvaluationCapacityExhausted) {
		t.Fatalf("before pause: %v", err)
	}
	if err := st.PauseEvaluationCampaign(ctx, c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := claimEval(st, samples[1], cand, time.Minute); !errors.Is(err, ErrEvaluationCampaignPaused) {
		t.Fatalf("after pause: %v", err)
	}
	if err := st.PauseEvaluationCampaign(ctx, c.ID); !errors.Is(err, ErrEvaluationCampaignAlreadyPaused) {
		t.Fatalf("second pause: %v", err)
	}
	if err := st.PauseEvaluationCampaign(ctx, "missing"); !errors.Is(err, ErrEvaluationCampaignNotFound) {
		t.Fatalf("missing campaign: %v", err)
	}
}

func TestEvaluationDuplicateCreatesAreDistinguished(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 3)
	if _, err := st.CreateEvaluationCampaign(ctx, c); !errors.Is(err, ErrEvaluationAlreadyExists) {
		t.Fatalf("duplicate campaign: %v", err)
	}
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "pool1", 3)
	if _, err := st.CreateEvaluationCandidate(ctx, cand); !errors.Is(err, ErrEvaluationAlreadyExists) {
		t.Fatalf("duplicate candidate: %v", err)
	}
	list, err := st.ListEvaluationCandidates(ctx, c.ID)
	if err != nil || len(list) != 1 || list[0].ID != cand.ID || list[0].Digest() != cand.Digest() {
		t.Fatalf("list: %v, %v", list, err)
	}
	if _, err := st.ListEvaluationCandidates(ctx, "missing"); !errors.Is(err, ErrEvaluationCampaignNotFound) {
		t.Fatalf("list missing campaign: %v", err)
	}
}
