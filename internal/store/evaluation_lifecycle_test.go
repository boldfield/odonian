package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
)

type evalFixture struct {
	campaign EvaluationCampaign
	cand     EvaluationCandidate
	sample   EvaluationSample
}

func newEvalFixture(t *testing.T, st Store, candidateCap, campaignCap int) evalFixture {
	t.Helper()
	c := newPoolTestCampaign(t, st, campaignCap)
	return evalFixture{
		campaign: c,
		cand:     newPoolTestCandidateStored(t, st, c, "fakeA", "pool1", candidateCap),
		sample:   newPoolTestSamples(t, st, c, 1)[0],
	}
}

func newEvalClockFixture(t *testing.T, candidateCap, campaignCap int) (Store, *evalClock, evalFixture) {
	t.Helper()
	clock := &evalClock{now: evalT0}
	st := openEvalClockStore(t, filepath.Join(t.TempDir(), "eval.db"), clock)
	configureTestEvaluationPool(t, st, "pool1")
	return st, clock, newEvalFixture(t, st, candidateCap, campaignCap)
}

func finalizeEval(st Store, attemptID string, exit EvaluationExitClass, findings ...evaluation.Finding) error {
	return st.FinalizeEvaluationAttempt(context.Background(), EvaluationAttemptResult{
		AttemptID: attemptID, FenceAttemptID: attemptID, ExitClass: exit, Findings: findings,
	})
}

func TestEvaluationFindingsAreAtomicWithResultAndImmutable(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	f := newEvalFixture(t, st, 5, 10)
	r, err := claimEval(st, f.sample, f.cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	bad := [][]evaluation.Finding{
		{{ID: "a", Severity: "bogus", Summary: "x"}},
		{{ID: "", Severity: evaluation.SeverityNote, Summary: "x"}},
		{{ID: "a", Severity: evaluation.SeverityNote, Summary: " "}},
		{{ID: "a", Severity: evaluation.SeverityNote, Summary: "x"}, {ID: "a", Severity: evaluation.SeverityMinor, Summary: "y"}},
	}
	for i, fs := range bad {
		if err := finalizeEval(st, r.Attempt.ID, EvalExitCompleted, fs...); !errors.Is(err, ErrEvaluationInvalidInput) {
			t.Errorf("bad findings %d: err = %v, want ErrEvaluationInvalidInput", i, err)
		}
	}
	if err := finalizeEval(st, r.Attempt.ID, EvalExitFailed, evaluation.Finding{ID: "a", Severity: evaluation.SeverityNote, Summary: "x"}); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Errorf("findings on a failed attempt: err = %v, want ErrEvaluationInvalidInput", err)
	}
	if a, _ := st.GetEvaluationAttempt(ctx, r.Attempt.ID); a.State != EvalAttemptActive {
		t.Fatalf("a rejected result must leave the attempt active, got %s", a.State)
	}
	if n := evalCount(t, st, "evaluation_finding"); n != 0 {
		t.Fatalf("rejected results wrote %d findings", n)
	}

	want := []evaluation.Finding{
		{ID: "f1", Severity: evaluation.SeverityMaterial, Claim: "c", Summary: "first", Evidence: "e"},
		{ID: "f2", Severity: evaluation.SeverityNote, Summary: "second"},
	}
	if err := finalizeEval(st, r.Attempt.ID, EvalExitCompleted, want...); err != nil {
		t.Fatal(err)
	}
	got, err := st.ListEvaluationFindings(ctx, r.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %+v, want %+v", got, want)
	}

	if err := finalizeEval(st, r.Attempt.ID, EvalExitCompleted, evaluation.Finding{ID: "f3", Severity: evaluation.SeverityNote, Summary: "late"}); !errors.Is(err, ErrEvaluationAttemptFinalized) {
		t.Errorf("second result: err = %v, want ErrEvaluationAttemptFinalized", err)
	}
	for _, q := range []string{
		`UPDATE evaluation_finding SET summary = 'tampered'`,
		`DELETE FROM evaluation_finding`,
		`UPDATE evaluation_attempt SET exit_class = 'failed'`,
		`DELETE FROM evaluation_attempt`,
	} {
		if _, err := st.Conn().Exec(q); err == nil {
			t.Errorf("%q succeeded; recorded results must be immutable", q)
		}
	}
	if after, _ := st.ListEvaluationFindings(ctx, r.Attempt.ID); !reflect.DeepEqual(after, want) {
		t.Errorf("findings changed after finalize: %+v", after)
	}
}

func TestEvaluationDistinctTerminalStates(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 100)
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "pool1", 100)
	samples := newPoolTestSamples(t, st, c, 9)
	exits := []EvaluationExitClass{
		EvalExitCompleted, EvalExitFailed, EvalExitCancelled, EvalExitUnknown, EvalExitTimeout,
		EvalExitUnavailableSnapshot, EvalExitUnavailableSource, EvalExitInvalidOutput, EvalExitIncompleteOutput,
	}
	for i, exit := range exits {
		r, err := claimEval(st, samples[i], cand, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if err := finalizeEval(st, r.Attempt.ID, exit); err != nil {
			t.Fatalf("%s: %v", exit, err)
		}
		a, err := st.GetEvaluationAttempt(ctx, r.Attempt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if a.State != EvalAttemptFinalized || a.ExitClass == nil || *a.ExitClass != exit {
			t.Errorf("%s: state=%s exit=%v", exit, a.State, a.ExitClass)
		}
		if a.Status != nil || a.ErrorClass != nil || a.UsageTokens != nil || a.DurationMs != nil {
			t.Errorf("%s: absent metadata must stay NULL, got status=%v class=%v usage=%v dur=%v", exit, a.Status, a.ErrorClass, a.UsageTokens, a.DurationMs)
		}
	}
	r, _ := claimEval(st, newPoolTestSamples2(t, st, c, "extra"), cand, time.Minute)
	for _, bad := range []EvaluationExitClass{"", "bogus", EvalExitLeaseExpired} {
		if err := finalizeEval(st, r.Attempt.ID, bad); !errors.Is(err, ErrEvaluationInvalidInput) {
			t.Errorf("exit %q: err = %v, want ErrEvaluationInvalidInput", bad, err)
		}
	}
	for _, q := range []string{
		`INSERT INTO evaluation_attempt (id, job_id, request_id, account_pool_id, sequence_number, state, started_at, expires_at, status) VALUES ('x','j','rx','p',1,'active','t','t','bogus')`,
		`INSERT INTO evaluation_attempt (id, job_id, request_id, account_pool_id, sequence_number, state, started_at, expires_at, error_class) VALUES ('x','j','rx','p',1,'active','t','t','bogus')`,
	} {
		if _, err := st.Conn().Exec(q); err == nil {
			t.Errorf("schema accepted an out-of-set status/error_class: %s", q)
		}
	}
}

func newPoolTestSamples2(t *testing.T, st Store, c EvaluationCampaign, task string) EvaluationSample {
	t.Helper()
	sm, err := st.CreateEvaluationSample(context.Background(), EvaluationSample{
		ID: GenerateID(), CampaignID: c.ID, ProjectID: "proj1", OriginalTaskID: task, OriginalReviewRound: 1,
		SubmittedSHA: "sha-" + task, PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return sm
}

func TestEvaluationUsageUnknownIsNotZero(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 10)
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "pool1", 10)
	samples := newPoolTestSamples(t, st, c, 2)
	zero := 0
	r1, _ := claimEval(st, samples[0], cand, time.Minute)
	r2, _ := claimEval(st, samples[1], cand, time.Minute)
	if err := finalizeEval(st, r1.Attempt.ID, EvalExitCompleted); err != nil {
		t.Fatal(err)
	}
	if err := st.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{AttemptID: r2.Attempt.ID, FenceAttemptID: r2.Attempt.ID, ExitClass: EvalExitCompleted, UsageTokens: &zero}); err != nil {
		t.Fatal(err)
	}
	a1, _ := st.GetEvaluationAttempt(ctx, r1.Attempt.ID)
	a2, _ := st.GetEvaluationAttempt(ctx, r2.Attempt.ID)
	if a1.UsageTokens != nil {
		t.Errorf("unknown usage read back as %d", *a1.UsageTokens)
	}
	if a2.UsageTokens == nil || *a2.UsageTokens != 0 {
		t.Errorf("reported zero usage read back as %v", a2.UsageTokens)
	}
}

func TestEvaluationCandidateIdentityIsBoundToM1Digest(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 10)

	if _, err := st.CreateEvaluationCandidate(ctx, EvaluationCandidate{ID: GenerateID(), CampaignID: c.ID, PerCandidateCap: 1}); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Errorf("zero-value config: err = %v, want ErrEvaluationInvalidInput", err)
	}
	if _, err := st.CreateEvaluationCandidate(ctx, newPoolTestCandidate("no-such-campaign", "fakeA", "v1", "pool1", 1)); !errors.Is(err, ErrEvaluationCampaignNotFound) {
		t.Errorf("unknown campaign: err = %v, want ErrEvaluationCampaignNotFound", err)
	}

	v1 := newPoolTestCandidate(c.ID, "fakeA", "prompt-v1", "pool1", 3)
	v2 := newPoolTestCandidate(c.ID, "fakeA", "prompt-v2", "pool1", 3)
	for _, cand := range []EvaluationCandidate{v1, v2} {
		if _, err := st.CreateEvaluationCandidate(ctx, cand); err != nil {
			t.Fatal(err)
		}
		var digest, pool string
		if err := st.Conn().QueryRow(`SELECT candidate_config_digest, account_pool_id FROM evaluation_candidate WHERE id = ?`, cand.ID).Scan(&digest, &pool); err != nil {
			t.Fatal(err)
		}
		if digest != cand.Config.Identity().Digest() || digest != cand.Digest() {
			t.Errorf("stored digest %q is not the M1 digest %q", digest, cand.Digest())
		}
		if pool != cand.AccountPoolID() {
			t.Errorf("stored pool %q, identity pool %q", pool, cand.AccountPoolID())
		}
	}

	// A stored candidate cannot be edited, and a row whose digest no longer
	// matches its identity is refused on read rather than trusted.
	if _, err := st.Conn().Exec(`UPDATE evaluation_candidate SET candidate_config_digest = ? WHERE id = ?`, v1.Digest(), v2.ID); err == nil {
		t.Fatal("candidate row was editable")
	}
	if _, err := st.Conn().Exec(`DROP TRIGGER evaluation_candidate_frozen`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Conn().Exec(`UPDATE evaluation_candidate SET candidate_config_digest = ? WHERE id = ?`, v1.Digest(), v2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetEvaluationCandidate(ctx, v2.ID); !errors.Is(err, ErrEvaluationCandidateCorrupt) {
		t.Errorf("v2 stored under v1's digest: err = %v, want ErrEvaluationCandidateCorrupt", err)
	}
	if got, err := st.GetEvaluationCandidate(ctx, v1.ID); err != nil || got.Digest() != v1.Digest() {
		t.Errorf("untouched candidate: %v %v", got.Digest(), err)
	}
}

func TestEvaluationSamplesRespectAllowedProjectsAndStayFrozen(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 10, "projA", "projB")
	got, err := st.GetEvaluationCampaign(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AllowedProjectIDs, []string{"projA", "projB"}) {
		t.Fatalf("allowed projects = %v", got.AllowedProjectIDs)
	}
	if _, err := st.CreateEvaluationCampaign(ctx, EvaluationCampaign{ID: GenerateID(), Name: "n", AllowedModelID: "m", CohortManifest: "{}", AttemptCap: 1}); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Errorf("campaign without projects: err = %v", err)
	}

	mk := func(project string) error {
		_, err := st.CreateEvaluationSample(ctx, EvaluationSample{
			ID: GenerateID(), CampaignID: c.ID, ProjectID: project, OriginalTaskID: "t-" + project, OriginalReviewRound: 1,
			SubmittedSHA: "s", PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
		})
		return err
	}
	if err := mk("projB"); err != nil {
		t.Errorf("allowed project: %v", err)
	}
	if err := mk("projZ"); !errors.Is(err, ErrEvaluationProjectNotAllowed) {
		t.Errorf("disallowed project: err = %v, want ErrEvaluationProjectNotAllowed", err)
	}

	snap, src, man := "snap", "src", "man"
	sm, err := st.CreateEvaluationSample(ctx, EvaluationSample{
		ID: GenerateID(), CampaignID: c.ID, ProjectID: "projA", OriginalTaskID: "frozen", OriginalReviewRound: 2,
		SubmittedSHA: "abc", SnapshotDigest: &snap, SourceDigest: &src, ManifestDigest: &man,
		PromptVersion: "p1", ModelVersion: "m1", RuntimeVersion: "r1",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE evaluation_sample SET submitted_sha = 'other'`,
		`UPDATE evaluation_sample SET snapshot_digest = 'other'`,
		`DELETE FROM evaluation_sample`,
	} {
		if _, err := st.Conn().Exec(q); err == nil {
			t.Errorf("%q succeeded; sample input identity must be frozen", q)
		}
	}
	read, err := st.GetEvaluationSample(ctx, sm.ID)
	if err != nil {
		t.Fatal(err)
	}
	sm.CreatedAt = read.CreatedAt
	if !reflect.DeepEqual(read, sm) {
		t.Errorf("sample identity changed:\n got %+v\nwant %+v", read, sm)
	}
}

func TestEvaluationLateResultsRecordLeaseExpiredConsistently(t *testing.T) {
	ctx := context.Background()
	st, clock, f := newEvalClockFixture(t, 5, 10)
	r1, err := claimEval(st, f.sample, f.cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	// The lease is over at the instant it is reached: renew refuses it.
	clock.Advance(time.Minute)
	if err := st.RenewEvaluationAttempt(ctx, r1.Attempt.ID, clock.Now().Add(time.Hour)); !errors.Is(err, ErrEvaluationAttemptExpired) {
		t.Errorf("renew at the lease boundary: err = %v, want ErrEvaluationAttemptExpired", err)
	}
	if err := finalizeEval(st, r1.Attempt.ID, EvalExitCompleted); !errors.Is(err, ErrEvaluationAttemptExpired) {
		t.Fatalf("late result: err = %v, want ErrEvaluationAttemptExpired", err)
	}
	late, err := st.GetEvaluationAttempt(ctx, r1.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if late.State != EvalAttemptExpired || late.ExitClass == nil || *late.ExitClass != EvalExitLeaseExpired || late.EndedAt == nil || late.DurationMs == nil {
		t.Fatalf("late result left an inconsistent row: %+v", late)
	}

	// The sweep records the identical outcome for an overdue attempt.
	r2, err := claimEval(st, f.sample, f.cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if n, err := st.ExpireEvaluationAttempts(ctx, clock.Now()); err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v", n, err)
	}
	swept, _ := st.GetEvaluationAttempt(ctx, r2.Attempt.ID)
	if swept.State != late.State || *swept.ExitClass != *late.ExitClass || swept.EndedAt == nil || swept.DurationMs == nil {
		t.Errorf("sweep and late result disagree: %+v vs %+v", swept, late)
	}
	// A late result for an expired attempt never turns it into a result.
	if err := finalizeEval(st, r2.Attempt.ID, EvalExitCompleted); !errors.Is(err, ErrEvaluationAttemptExpired) {
		t.Errorf("result after sweep: err = %v", err)
	}
	if n := evalCount(t, st, "evaluation_finding"); n != 0 {
		t.Errorf("late results wrote %d findings", n)
	}
}

func TestEvaluationStaleAttemptCannotOverwriteNewerRetry(t *testing.T) {
	ctx := context.Background()
	st, clock, f := newEvalClockFixture(t, 5, 10)
	r1, _ := claimEval(st, f.sample, f.cand, time.Minute)
	clock.Advance(2 * time.Minute)
	r2, err := claimEval(st, f.sample, f.cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Attempt.SequenceNumber != 2 || r2.Attempt.PreviousAttemptID == nil || *r2.Attempt.PreviousAttemptID != r1.Attempt.ID {
		t.Fatalf("retry attempt = %+v", r2.Attempt)
	}
	if err := finalizeEval(st, r1.Attempt.ID, EvalExitCompleted, evaluation.Finding{ID: "stale", Severity: evaluation.SeverityMaterial, Summary: "stale"}); err == nil {
		t.Fatal("a stale attempt's result was accepted")
	}
	if err := st.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{AttemptID: r2.Attempt.ID, FenceAttemptID: r1.Attempt.ID, ExitClass: EvalExitCompleted}); !errors.Is(err, ErrEvaluationFenceMismatch) {
		t.Errorf("wrong fence: err = %v, want ErrEvaluationFenceMismatch", err)
	}
	if err := finalizeEval(st, r2.Attempt.ID, EvalExitCompleted); err != nil {
		t.Fatalf("current attempt: %v", err)
	}
	if n := evalCount(t, st, "evaluation_finding"); n != 0 {
		t.Errorf("stale attempt wrote %d findings", n)
	}
}

func TestEvaluationConcurrentClaimsOnOneJobRespectCap(t *testing.T) {
	st := newEvaluationStore(t)
	f := newEvalFixture(t, st, 1, 10)

	const workers = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, other int
	var unexpected []error
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.ClaimEvaluationJob(context.Background(), EvaluationJobClaim{
				SampleID: f.sample.ID, CandidateID: f.cand.ID, RequestID: fmt.Sprintf("race-%d", i), LeaseExpires: time.Hour,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrEvaluationAttemptLive), errors.Is(err, ErrEvaluationCapacityExhausted):
				other++
			default:
				unexpected = append(unexpected, err)
			}
		}(i)
	}
	wg.Wait()
	if len(unexpected) > 0 {
		t.Fatalf("unexpected claim errors: %v", unexpected)
	}
	if ok != 1 || other != workers-1 {
		t.Fatalf("ok=%d refused=%d, want exactly one success", ok, other)
	}
	if n := evalCount(t, st, "evaluation_attempt"); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestEvaluationRetriesConsumeCampaignAndCandidateCapacity(t *testing.T) {
	st, clock, f := newEvalClockFixture(t, 2, 10)
	for i := 0; i < 2; i++ {
		r, err := claimEval(st, f.sample, f.cand, time.Minute)
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		if i == 0 {
			if err := finalizeEval(st, r.Attempt.ID, EvalExitFailed); err != nil {
				t.Fatal(err)
			}
		} else {
			clock.Advance(time.Minute) // the second execution times out
		}
	}
	if _, err := claimEval(st, f.sample, f.cand, time.Minute); !errors.Is(err, ErrEvaluationCapacityExhausted) {
		t.Fatalf("third execution: err = %v, want ErrEvaluationCapacityExhausted", err)
	}
	if n := evalCount(t, st, "evaluation_attempt"); n != 2 {
		t.Errorf("attempts = %d, want 2", n)
	}
}

func TestEvaluationRequestReplayIsIdempotentAndBoundToItsPair(t *testing.T) {
	ctx := context.Background()
	st := newEvaluationStore(t)
	f := newEvalFixture(t, st, 1, 1)
	other := newPoolTestCandidateStored(t, st, f.campaign, "fakeB", "pool1", 1)
	claim := EvaluationJobClaim{SampleID: f.sample.ID, CandidateID: f.cand.ID, RequestID: "same", LeaseExpires: time.Minute}
	a, err := st.ClaimEvaluationJob(ctx, claim)
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.ClaimEvaluationJob(ctx, claim)
	if err != nil {
		t.Fatalf("replay at cap: %v", err)
	}
	if a.Attempt.ID != b.Attempt.ID {
		t.Errorf("replay created attempt %s, want %s", b.Attempt.ID, a.Attempt.ID)
	}
	claim.CandidateID = other.ID
	if _, err := st.ClaimEvaluationJob(ctx, claim); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Errorf("request id reused for another pair: err = %v, want ErrEvaluationInvalidInput", err)
	}
	if n := evalCount(t, st, "evaluation_attempt"); n != 1 {
		t.Errorf("attempts = %d, want 1", n)
	}
}

func TestEvaluationStateSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "eval.db")
	clock := &evalClock{now: evalT0}

	st, err := Open(path, defaultTestAllowedModels(), WithClock(clock.Now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "pool1", ConcurrencyOnly: true, ConcurrentLimit: 1}); err != nil {
		t.Fatal(err)
	}
	f := newEvalFixture(t, st, 2, 5)
	done := newPoolTestSamples2(t, st, f.campaign, "done-task")
	r1, err := claimEval(st, done, f.cand, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	findings := []evaluation.Finding{{ID: "f1", Severity: evaluation.SeverityMinor, Summary: "kept"}}
	if err := finalizeEval(st, r1.Attempt.ID, EvalExitCompleted, findings...); err != nil {
		t.Fatal(err)
	}
	live, err := claimEval(st, f.sample, f.cand, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	clock.Advance(30 * time.Minute)
	st, err = Open(path, defaultTestAllowedModels(), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cand, err := st.GetEvaluationCandidate(ctx, f.cand.ID)
	if err != nil || cand.Digest() != f.cand.Digest() || !reflect.DeepEqual(cand.Config.Identity(), f.cand.Config.Identity()) {
		t.Fatalf("candidate after restart: %v, %v", cand.Digest(), err)
	}
	if got, err := st.ListEvaluationFindings(ctx, r1.Attempt.ID); err != nil || !reflect.DeepEqual(got, findings) {
		t.Fatalf("findings after restart: %+v, %v", got, err)
	}
	a, err := st.GetEvaluationAttempt(ctx, live.Attempt.ID)
	if err != nil || a.State != EvalAttemptActive || a.ExpiresAt != live.Attempt.ExpiresAt {
		t.Fatalf("live attempt after restart: %+v, %v", a, err)
	}
	// The lease and the finite cap both carry over: the live lease still blocks a
	// new start, and once it lapses the cap of 2 is already spent.
	if _, err := claimEval(st, f.sample, f.cand, time.Hour); !errors.Is(err, ErrEvaluationAttemptLive) {
		t.Errorf("claim during surviving lease: err = %v, want ErrEvaluationAttemptLive", err)
	}
	if err := finalizeEval(st, live.Attempt.ID, EvalExitCompleted); err != nil {
		t.Fatalf("finalize across restart: %v", err)
	}
	if _, err := claimEval(st, f.sample, f.cand, time.Hour); !errors.Is(err, ErrEvaluationCapacityExhausted) {
		t.Errorf("claim past cap after restart: err = %v, want ErrEvaluationCapacityExhausted", err)
	}
	if _, err := st.GetEvaluationPool(ctx, "pool1"); err != nil {
		t.Errorf("pool after restart: %v", err)
	}
}
