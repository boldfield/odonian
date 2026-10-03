package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/policy"
)

var evalT0 = time.Unix(1_800_000_000, 0).UTC()

// evalClock is a mutable injectable clock.
type evalClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *evalClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *evalClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func openEvalClockStore(t *testing.T, path string, clock *evalClock) Store {
	t.Helper()
	s, err := Open(path, defaultTestAllowedModels(), WithClock(clock.Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newPoolTestCandidate(campaignID, adapter, promptVersion, pool string, cap int) EvaluationCandidate {
	identity := evaluation.CandidateIdentity{
		AdapterName:        adapter,
		AdapterVersion:     "v1",
		ModelID:            adapter + "-model",
		ModelRevision:      "unknown",
		RuntimeName:        "test",
		RuntimeVersion:     "v1",
		ReasoningSettings:  evaluation.UnknownSettings(),
		GenerationSettings: evaluation.UnknownSettings(),
		PromptVersion:      promptVersion,
		Tools:              evaluation.UnknownNames(),
		Observers:          evaluation.UnknownNames(),
		AccountPool:        pool,
	}
	config, err := evaluation.NewCandidateConfig(identity)
	if err != nil {
		panic(err)
	}
	return EvaluationCandidate{ID: GenerateID(), CampaignID: campaignID, Config: config, PerCandidateCap: cap}
}

func newPoolTestCampaign(t *testing.T, st Store, attemptCap int, projects ...string) EvaluationCampaign {
	t.Helper()
	if len(projects) == 0 {
		projects = []string{"proj1"}
	}
	c, err := st.CreateEvaluationCampaign(context.Background(), EvaluationCampaign{
		ID: GenerateID(), Name: "pool-campaign", AllowedProjectIDs: projects, AllowedModelID: "m",
		CohortManifest: `{"samples": []}`, AttemptCap: attemptCap,
	})
	if err != nil {
		t.Fatalf("CreateEvaluationCampaign: %v", err)
	}
	return c
}

func newPoolTestCandidateStored(t *testing.T, st Store, c EvaluationCampaign, adapter, pool string, cap int) EvaluationCandidate {
	t.Helper()
	cand, err := st.CreateEvaluationCandidate(context.Background(), newPoolTestCandidate(c.ID, adapter, "v1", pool, cap))
	if err != nil {
		t.Fatalf("CreateEvaluationCandidate: %v", err)
	}
	return cand
}

func newPoolTestSamples(t *testing.T, st Store, c EvaluationCampaign, n int) []EvaluationSample {
	t.Helper()
	var out []EvaluationSample
	for i := 0; i < n; i++ {
		sha := fmt.Sprintf("sha%d", i)
		sm, err := st.CreateEvaluationSample(context.Background(), EvaluationSample{
			ID: GenerateID(), CampaignID: c.ID, ProjectID: "proj1", OriginalTaskID: fmt.Sprintf("task%d", i), OriginalReviewRound: 1,
			SubmittedSHA: sha, SnapshotDigest: &sha, PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
		})
		if err != nil {
			t.Fatalf("CreateEvaluationSample: %v", err)
		}
		out = append(out, sm)
	}
	return out
}

var evalReqSeq int
var evalReqMu sync.Mutex

func claimEval(st Store, sample EvaluationSample, cand EvaluationCandidate, lease time.Duration) (EvaluationJobClaimResult, error) {
	evalReqMu.Lock()
	evalReqSeq++
	id := fmt.Sprintf("pool-req-%d", evalReqSeq)
	evalReqMu.Unlock()
	return st.ClaimEvaluationJob(context.Background(), EvaluationJobClaim{
		SampleID: sample.ID, CandidateID: cand.ID, RequestID: id, LeaseExpires: lease,
	})
}

func evalCount(t *testing.T, st Store, table string) int {
	t.Helper()
	var n int
	if err := st.Conn().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestEvaluationPoolMissingConfigurationIsAnError(t *testing.T) {
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 5)
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "pool-never-configured", 5)
	sm := newPoolTestSamples(t, st, c, 1)[0]

	_, err := claimEval(st, sm, cand, time.Minute)
	if !errors.Is(err, ErrEvaluationPoolNotConfigured) {
		t.Fatalf("claim without pool = %v, want ErrEvaluationPoolNotConfigured", err)
	}
	if n := evalCount(t, st, "evaluation_job"); n != 0 {
		t.Errorf("jobs = %d after refused claim, want 0", n)
	}
	if n := evalCount(t, st, "evaluation_attempt"); n != 0 {
		t.Errorf("attempts = %d after refused claim, want 0", n)
	}
}

func TestEvaluationPoolConfigValidation(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()
	bad := []EvaluationPoolConfig{
		{ID: "", ConcurrencyOnly: true, ConcurrentLimit: 1},
		{ID: "p", ConcurrencyOnly: true, ConcurrentLimit: 0},
		{ID: "p", ConcurrencyOnly: true, ConcurrentLimit: 1, StartRate: 1},
		{ID: "p", ConcurrencyOnly: true, ConcurrentLimit: 1, BurstCapacity: 1},
		{ID: "p", StartRate: 0, BurstCapacity: 1, ConcurrentLimit: 1},
		{ID: "p", StartRate: 1, BurstCapacity: 0, ConcurrentLimit: 1},
		{ID: "p", StartRate: 1, BurstCapacity: 1, ConcurrentLimit: 0},
	}
	for i, cfg := range bad {
		if _, err := st.ConfigureEvaluationPool(ctx, cfg); !errors.Is(err, ErrEvaluationInvalidInput) {
			t.Errorf("config %d: err = %v, want ErrEvaluationInvalidInput", i, err)
		}
	}
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "mode", StartRate: 1, BurstCapacity: 1, ConcurrentLimit: 1}); err != nil {
		t.Fatalf("configure rate pool: %v", err)
	}
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "mode", ConcurrencyOnly: true, ConcurrentLimit: 1}); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Errorf("mode change err = %v, want ErrEvaluationInvalidInput", err)
	}
	if _, err := st.GetEvaluationPool(ctx, "absent"); !errors.Is(err, ErrEvaluationPoolNotConfigured) {
		t.Errorf("GetEvaluationPool(absent) = %v, want ErrEvaluationPoolNotConfigured", err)
	}
}

func TestEvaluationRatePoolDebitsEveryFreshExecution(t *testing.T) {
	clock := &evalClock{now: evalT0}
	path := filepath.Join(t.TempDir(), "eval.db")
	st := openEvalClockStore(t, path, clock)
	ctx := context.Background()
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "meta", StartRate: 0.001, BurstCapacity: 2, ConcurrentLimit: 10}); err != nil {
		t.Fatal(err)
	}
	c := newPoolTestCampaign(t, st, 10)
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "meta", 10)
	samples := newPoolTestSamples(t, st, c, 3)

	first, err := claimEval(st, samples[0], cand, time.Minute)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	// A replay of the same request is idempotent and spends nothing.
	if _, err := st.ClaimEvaluationJob(ctx, EvaluationJobClaim{SampleID: samples[0].ID, CandidateID: cand.ID, RequestID: first.Attempt.RequestID, LeaseExpires: time.Minute}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if ps, _ := st.GetEvaluationPool(ctx, "meta"); ps.Tokens < 0.999 || ps.Tokens > 1.001 {
		t.Fatalf("tokens after one start and a replay = %v, want 1", ps.Tokens)
	}

	// The lease lapses; the retry is a fresh execution and spends a second start.
	clock.Advance(2 * time.Minute)
	retry, err := claimEval(st, samples[0], cand, time.Minute)
	if err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if retry.Attempt.SequenceNumber != 2 {
		t.Fatalf("retry sequence = %d, want 2", retry.Attempt.SequenceNumber)
	}

	// Pool exhausted: a different sample is deferred by rate, and nothing is consumed.
	attemptsBefore := evalCount(t, st, "evaluation_attempt")
	_, err = claimEval(st, samples[1], cand, time.Minute)
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("claim on exhausted pool = %v, want AdmissionDeniedError", err)
	}
	if denied.Outcome != policy.OutcomeDefer || denied.Reason != policy.ReasonRate || !denied.NotBefore.After(clock.Now()) {
		t.Errorf("denial = %+v, want a rate deferral with a future NotBefore", denied)
	}
	if n := evalCount(t, st, "evaluation_attempt"); n != attemptsBefore {
		t.Errorf("attempts changed %d -> %d on a denied claim", attemptsBefore, n)
	}
	if n := evalCount(t, st, "evaluation_job"); n != 1 {
		t.Errorf("jobs = %d, want 1 (denied claim must not persist a job)", n)
	}

	// Restart: the allowance is persisted, not refilled.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2 := openEvalClockStore(t, path, clock)
	if ps, err := st2.GetEvaluationPool(ctx, "meta"); err != nil || ps.Tokens >= 1 {
		t.Fatalf("pool after restart = %+v, %v; want tokens < 1", ps, err)
	}
	if _, err := claimEval(st2, samples[1], cand, time.Minute); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("claim after restart = %v, want still deferred", err)
	}
	// Persisted jobs and attempts survive too.
	if n := evalCount(t, st2, "evaluation_attempt"); n != 2 {
		t.Errorf("attempts after restart = %d, want 2", n)
	}

	// Time alone restores one start.
	clock.Advance(1000 * time.Second)
	if _, err := claimEval(st2, samples[1], cand, time.Minute); err != nil {
		t.Fatalf("claim after refill: %v", err)
	}
}

func TestEvaluationConcurrencyOnlyPool(t *testing.T) {
	clock := &evalClock{now: evalT0}
	st := openEvalClockStore(t, filepath.Join(t.TempDir(), "eval.db"), clock)
	ctx := context.Background()
	ps, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "local", ConcurrencyOnly: true, ConcurrentLimit: 1})
	if err != nil || !ps.ConcurrencyOnly || ps.Tokens != 0 {
		t.Fatalf("configure = %+v, %v", ps, err)
	}
	c := newPoolTestCampaign(t, st, 3)
	cand := newPoolTestCandidateStored(t, st, c, "local-llm", "local", 10)
	samples := newPoolTestSamples(t, st, c, 5)

	a, err := claimEval(st, samples[0], cand, time.Minute)
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	_, err = claimEval(st, samples[1], cand, time.Minute)
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || denied.Reason != policy.ReasonConcurrency || denied.Outcome != policy.OutcomeRetry {
		t.Fatalf("second concurrent claim = %v, want a concurrency retry", err)
	}
	// Finalizing releases the slot; there is no rate bucket to run out of.
	if err := st.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{AttemptID: a.Attempt.ID, FenceAttemptID: a.Attempt.ID, ExitClass: EvalExitCompleted}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	b, err := claimEval(st, samples[1], cand, time.Minute)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	// A lapsed lease frees the slot too.
	clock.Advance(2 * time.Minute)
	if _, err := claimEval(st, samples[2], cand, time.Minute); err != nil {
		t.Fatalf("claim after lease lapse: %v", err)
	}
	if err := st.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{AttemptID: b.Attempt.ID, FenceAttemptID: b.Attempt.ID, ExitClass: EvalExitCompleted}); !errors.Is(err, ErrEvaluationAttemptExpired) {
		t.Fatalf("late finalize = %v, want ErrEvaluationAttemptExpired", err)
	}

	// Concurrency-only pools still obey the finite campaign cap (3 attempts used).
	if _, err := claimEval(st, samples[3], cand, time.Minute); !errors.Is(err, ErrEvaluationCapacityExhausted) {
		t.Fatalf("claim past campaign cap = %v, want ErrEvaluationCapacityExhausted", err)
	}
}

func TestEvaluationRatePoolConcurrentClaims(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "burst3", StartRate: 0.0001, BurstCapacity: 3, ConcurrentLimit: 100}); err != nil {
		t.Fatal(err)
	}
	c := newPoolTestCampaign(t, st, 100)
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "burst3", 100)
	samples := newPoolTestSamples(t, st, c, 12)

	var wg sync.WaitGroup
	errs := make([]error, len(samples))
	for i := range samples {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = claimEval(st, samples[i], cand, time.Minute)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInsufficientCapacity):
		default:
			t.Errorf("unexpected claim error: %v", err)
		}
	}
	if ok != 3 {
		t.Fatalf("successful concurrent claims = %d, want exactly the burst of 3", ok)
	}
	if n := evalCount(t, st, "evaluation_attempt"); n != 3 {
		t.Errorf("attempts = %d, want 3", n)
	}
}

// Two candidates from unrelated providers share one campaign. One exhausting
// its pool, or failing, must not move work to the other or change its results.
func TestEvaluationTwoProvidersAreIsolated(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "subscription-a", StartRate: 0.0001, BurstCapacity: 1, ConcurrentLimit: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "spark", ConcurrencyOnly: true, ConcurrentLimit: 5}); err != nil {
		t.Fatal(err)
	}
	c := newPoolTestCampaign(t, st, 20)
	candA := newPoolTestCandidateStored(t, st, c, "provider-a", "subscription-a", 5)
	candB := newPoolTestCandidateStored(t, st, c, "provider-b", "spark", 5)
	samples := newPoolTestSamples(t, st, c, 3)

	a1, err := claimEval(st, samples[0], candA, time.Minute)
	if err != nil {
		t.Fatalf("A claim 1: %v", err)
	}
	failed := evaluation.StatusFailed
	errClass := evaluation.ErrClassRuntimeError
	if err := st.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{AttemptID: a1.Attempt.ID, FenceAttemptID: a1.Attempt.ID, ExitClass: EvalExitFailed, Status: &failed, ErrorClass: &errClass}); err != nil {
		t.Fatalf("A finalize: %v", err)
	}
	// A is out of quota.
	if _, err := claimEval(st, samples[1], candA, time.Minute); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("A claim 2 = %v, want pool denial", err)
	}

	// B runs every sample independently and is unaffected by A's failure and exhaustion.
	var bAttempts []string
	for _, sm := range samples {
		r, err := claimEval(st, sm, candB, time.Minute)
		if err != nil {
			t.Fatalf("B claim: %v", err)
		}
		bAttempts = append(bAttempts, r.Attempt.ID)
		if r.Job.CandidateID != candB.ID {
			t.Errorf("B job bound to candidate %s", r.Job.CandidateID)
		}
	}
	bState, err := st.GetEvaluationAttempt(ctx, bAttempts[0])
	if err != nil || bState.State != EvalAttemptActive || bState.ExitClass != nil {
		t.Errorf("B attempt changed by A's outcome: %+v, %v", bState, err)
	}

	// Denominators are per candidate and are not equalised.
	perCandidate := func(id string) (jobs, attempts int) {
		_ = st.Conn().QueryRow(`SELECT COUNT(DISTINCT ej.id), COUNT(ea.id) FROM evaluation_job ej JOIN evaluation_attempt ea ON ea.job_id = ej.id WHERE ej.candidate_id = ?`, id).Scan(&jobs, &attempts)
		return
	}
	if j, a := perCandidate(candA.ID); j != 1 || a != 1 {
		t.Errorf("A jobs/attempts = %d/%d, want 1/1", j, a)
	}
	if j, a := perCandidate(candB.ID); j != 3 || a != 3 {
		t.Errorf("B jobs/attempts = %d/%d, want 3/3", j, a)
	}
	// A's refused sample was not rerouted: no job exists for (sample 2, A).
	var n int
	if err := st.Conn().QueryRow(`SELECT COUNT(*) FROM evaluation_job WHERE sample_id = ? AND candidate_id = ?`, samples[1].ID, candA.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("jobs for refused (sample, A) = %d, %v; want 0", n, err)
	}
}

func TestEvaluationCandidateVersionsRoundTripAndStayIsolated(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()
	c := newPoolTestCampaign(t, st, 10)
	v1, err := st.CreateEvaluationCandidate(ctx, newPoolTestCandidate(c.ID, "fakeA", "prompt-v1", "pool1", 5))
	if err != nil {
		t.Fatal(err)
	}
	v2, err := st.CreateEvaluationCandidate(ctx, newPoolTestCandidate(c.ID, "fakeA", "prompt-v2", "pool1", 5))
	if err != nil {
		t.Fatal(err)
	}
	if v1.Digest() == v2.Digest() {
		t.Fatal("a changed prompt version must change the candidate digest")
	}
	for _, want := range []EvaluationCandidate{v1, v2} {
		got, err := st.GetEvaluationCandidate(ctx, want.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Digest() != want.Digest() || got.Digest() == "" {
			t.Errorf("digest round-trip = %q, want %q", got.Digest(), want.Digest())
		}
		if !reflect.DeepEqual(got.Config.Identity(), want.Config.Identity()) {
			t.Errorf("identity did not round-trip: %+v vs %+v", got.Config.Identity(), want.Config.Identity())
		}
	}
	sm := newPoolTestSamples(t, st, c, 1)[0]
	r1, err := claimEval(st, sm, v1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := claimEval(st, sm, v2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Job.ID == r2.Job.ID {
		t.Error("candidate versions must have separate jobs for the same sample")
	}
}

func TestEvaluationPoolIsIndependentOfResearchPool(t *testing.T) {
	clock := &evalClock{now: evalT0}
	st := openEvalClockStore(t, filepath.Join(t.TempDir(), "eval.db"), clock)
	ctx := context.Background()
	if _, err := st.ConfigureResearchPool(ctx, clock.Now(), ResearchPoolConfig{AccountID: "shared-name", StartRate: 0.001, BurstCapacity: 2, ConcurrentLimit: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "shared-name", StartRate: 0.001, BurstCapacity: 1, ConcurrentLimit: 1}); err != nil {
		t.Fatal(err)
	}
	c := newPoolTestCampaign(t, st, 5)
	cand := newPoolTestCandidateStored(t, st, c, "fakeA", "shared-name", 5)
	samples := newPoolTestSamples(t, st, c, 2)
	if _, err := claimEval(st, samples[0], cand, time.Minute); err != nil {
		t.Fatal(err)
	}
	rp, err := st.GetResearchPool(ctx, clock.Now(), "shared-name")
	if err != nil {
		t.Fatal(err)
	}
	if rp.Tokens != 2 || rp.Active != 0 {
		t.Errorf("research pool changed by an evaluation start: tokens=%v active=%d", rp.Tokens, rp.Active)
	}
}

func productionSnapshot(t *testing.T, st Store) map[string]string {
	t.Helper()
	conn := st.Conn()
	rows, err := conn.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'evaluation\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	snap := map[string]string{}
	for _, name := range names {
		r, err := conn.Query(`SELECT * FROM "` + name + `"`)
		if err != nil {
			t.Fatalf("dump %s: %v", name, err)
		}
		cols, _ := r.Columns()
		var lines []string
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, fmt.Sprint(vals...))
		}
		r.Close()
		snap[name] = strings.Join(lines, "\n")
	}
	if len(snap) < 5 {
		t.Fatalf("snapshot covers only %d production tables", len(snap))
	}
	return snap
}

// Evaluation results, whatever they say, must never act on the production
// review of the submission they evaluate: no verdicts or aggregation, no
// follow-ups, adjudications or continuations, no task state or round change,
// and no scorecard movement.
func TestEvaluationResultsNeverTouchProductionReview(t *testing.T) {
	st, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})
	opusTask, _ := findResearchReviewTasks(t, st, ctx, projID, parentID, 1)
	if opusTask == nil {
		t.Fatal("expected opus review task")
	}
	submitResearchReview(t, st, ctx, opusTask, "opus-reviewer", "reject",
		json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"}]`))

	parentBefore, err := st.GetTask(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	tasksBefore, err := st.ListTasks(ctx, projID, TaskListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	scoreBefore, err := st.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoreBefore.Scorecards) == 0 {
		t.Fatal("test setup: expected a populated scorecard")
	}
	followUpsBefore := findResearchFollowUps(t, st, ctx, projID, parentID)
	snapBefore := productionSnapshot(t, st)

	configureTestEvaluationPool(t, st, "iso-pool")
	campaign := newPoolTestCampaign(t, st, 5, projID)
	cand := newPoolTestCandidateStored(t, st, campaign, "fakeA", "iso-pool", 5)
	sample, err := st.CreateEvaluationSample(ctx, EvaluationSample{
		ID: GenerateID(), CampaignID: campaign.ID, ProjectID: projID, OriginalTaskID: parentID, OriginalReviewRound: parentBefore.ReviewRound,
		SubmittedSHA: "deadbeef", PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// An evaluation that contradicts production: approves with a material finding,
	// retried once after a failure.
	r1, err := claimEval(st, sample, cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	failed := evaluation.StatusFailed
	cls := evaluation.ErrClassRuntimeError
	if err := st.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{AttemptID: r1.Attempt.ID, FenceAttemptID: r1.Attempt.ID, ExitClass: EvalExitFailed, Status: &failed, ErrorClass: &cls}); err != nil {
		t.Fatal(err)
	}
	r2, err := claimEval(st, sample, cand, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	done := evaluation.StatusCompleted
	if err := st.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{AttemptID: r2.Attempt.ID, FenceAttemptID: r2.Attempt.ID, ExitClass: EvalExitCompleted, Status: &done,
		Findings: []evaluation.Finding{{ID: "f1", Severity: evaluation.SeverityMaterial, Summary: "evaluator disagrees"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExpireEvaluationAttempts(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	if n := evalCount(t, st, "evaluation_attempt"); n != 2 {
		t.Fatalf("evaluation attempts = %d, want 2", n)
	}
	parentAfter, err := st.GetTask(ctx, parentID)
	if err != nil {
		t.Fatal(err)
	}
	if parentAfter.State != parentBefore.State || parentAfter.ReviewRound != parentBefore.ReviewRound {
		t.Errorf("task state/round changed: %s/%d -> %s/%d", parentBefore.State, parentBefore.ReviewRound, parentAfter.State, parentAfter.ReviewRound)
	}
	tasksAfter, err := st.ListTasks(ctx, projID, TaskListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasksAfter) != len(tasksBefore) {
		t.Errorf("task count %d -> %d: evaluation created or removed tasks", len(tasksBefore), len(tasksAfter))
	}
	if got := findResearchFollowUps(t, st, ctx, projID, parentID); len(got) != len(followUpsBefore) {
		t.Errorf("follow-ups %d -> %d", len(followUpsBefore), len(got))
	}
	scoreAfter, err := st.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scoreBefore, scoreAfter) {
		t.Errorf("production reviewer scorecards changed:\nbefore %+v\nafter  %+v", scoreBefore, scoreAfter)
	}

	// Every production table (tasks, events, links, findings, verdicts, adjudications,
	// continuations, research pools and permits) is byte-for-byte unchanged.
	snapAfter := productionSnapshot(t, st)
	for name, before := range snapBefore {
		if snapAfter[name] != before {
			t.Errorf("production table %q changed during evaluation", name)
		}
	}
}
