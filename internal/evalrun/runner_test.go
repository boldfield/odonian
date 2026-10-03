package evalrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evalcohort"
	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

func TestCompletedRunRecordsEverything(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5,
		withUsage(`{"fake_units": 12.5, "observer_units": 3}`))
	r, rx, _ := h.runner()

	rep := r.Run(context.Background(), h.job(0, cand))

	if rep.Kind != Kind(store.EvalExitCompleted) || len(rep.Attempts) != 1 || rep.RetriesExhausted {
		t.Fatalf("report = %+v", rep)
	}
	rec := rep.Attempts[0]
	if !rec.Recorded || rec.FinalizeErr != nil || rec.LeaseErr != nil || rec.CleanupErr != nil {
		t.Fatalf("record = %+v", rec)
	}
	att := h.attempt(rec.AttemptID)
	if att.State != store.EvalAttemptFinalized || att.ExitClass == nil || *att.ExitClass != store.EvalExitCompleted ||
		att.Status == nil || *att.Status != string(evaluation.StatusCompleted) || att.DurationMs == nil {
		t.Fatalf("attempt = %+v", att)
	}
	if att.UsageTokens != nil {
		t.Errorf("the core must not interpret provider usage as tokens, got %d", *att.UsageTokens)
	}
	findings, err := h.st.ListEvaluationFindings(context.Background(), rec.AttemptID)
	if err != nil || len(findings) != 2 {
		t.Fatalf("findings = %v, %v", findings, err)
	}

	req := rx.request(0)
	if !strings.Contains(req.BlindedPrompt, EvidenceStandard()) || !strings.HasPrefix(req.BlindedPrompt, "review the staged artifact") {
		t.Errorf("prompt does not carry the staged prompt followed by the evidence standard: %q", req.BlindedPrompt)
	}
	if req.RunID != rec.AttemptID || !req.ToolAccess.RequireSourceRetrieval {
		t.Errorf("request = %+v", req)
	}
	d := h.detail(rec.AttemptID)
	if d == nil {
		t.Fatal("no attempt detail")
	}
	if d.CandidateDigest != cand.Digest() || d.EffectiveDigest != cand.Digest() || d.EffectiveIdentity == nil ||
		d.EffectiveIdentity.ModelID != "model-a" {
		t.Errorf("identity in detail = %+v", d)
	}
	if d.PromptDigest != evaluation.PromptDigest(req.BlindedPrompt) || d.StandardDigest != evaluation.PromptDigest(EvidenceStandard()) {
		t.Errorf("digests in detail = %+v", d)
	}
	if !d.Launched || d.ExitCode == nil || *d.ExitCode != 0 {
		t.Errorf("launch in detail = %+v", d)
	}
	if want := map[string]float64{"fake_units": 12.5, "observer_units": 3}; !reflect.DeepEqual(d.Usage, want) {
		t.Errorf("usage = %v, want %v (provider units, unsummed)", d.Usage, want)
	}

	staged := h.stager.stagedAt(0)
	if exists(staged.Workspace.Path) || exists(staged.Request.ResultPath) {
		t.Error("workspace or result file left behind")
	}
	if got := h.stager.reqs[0]; got.RunID != rec.AttemptID || got.CandidateID != cand.ID || got.SampleID != h.samples[0].ID {
		t.Errorf("stage request = %+v", got)
	}
}

func TestRunsLeaveTheProductionBoardAlone(t *testing.T) {
	h := newHarness(t, 2)
	h.openPool("pool-a")
	h.openPool("pool-b")
	h.addCandidate("ok", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
	h.addCandidate("bad", evaluation.FakeModeFailed, ident("beta", "model-b", "pool-b"), 5)
	before := nonEvaluationRows(t, h.dbPath)
	if len(before) == 0 {
		t.Fatal("no production tables found; the check would prove nothing")
	}
	r, _, _ := h.runner()

	reports, err := r.RunCampaign(context.Background(), h.campaign.ID)
	if err != nil || len(reports) != 4 {
		t.Fatalf("reports = %d, err = %v", len(reports), err)
	}
	if after := nonEvaluationRows(t, h.dbPath); !reflect.DeepEqual(before, after) {
		t.Fatalf("production rows changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestPrelaunchDeferralConcurrencyPool(t *testing.T) {
	h := newHarness(t, 2)
	h.pool(store.EvaluationPoolConfig{ID: "solo", ConcurrencyOnly: true, ConcurrentLimit: 1})
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "solo"), 5)
	ctx := context.Background()

	held, err := h.st.ClaimEvaluationJob(ctx, store.EvaluationJobClaim{
		SampleID: h.samples[0].ID, CandidateID: cand.ID, RequestID: "held", LeaseExpires: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("gives up within bounds without launching or consuming an attempt", func(t *testing.T) {
		r, rx, sl := h.runner(func(c *Config) { c.MaxDeferrals = 2 })
		rep := r.Run(ctx, h.job(1, cand))
		if rep.Kind != KindDeferred || rep.Deferrals != 2 || rep.RetryAfter <= 0 || len(rep.Attempts) != 0 {
			t.Fatalf("report = %+v", rep)
		}
		var denied *store.AdmissionDeniedError
		if !errors.As(rep.Err, &denied) {
			t.Fatalf("err = %v", rep.Err)
		}
		if rx.calls() != 0 || len(h.stager.reqs) != 0 {
			t.Fatalf("a deferred run staged %d or launched %d times", len(h.stager.reqs), rx.calls())
		}
		if h.attemptsUsed() != 1 {
			t.Fatalf("attempts used = %d, want only the held one", h.attemptsUsed())
		}
		if got := sl.durations(); len(got) != 1 || got[0] != rep.RetryAfter {
			t.Fatalf("sleeps = %v, want one wait of the store's hint %v", got, rep.RetryAfter)
		}
	})

	t.Run("launches once the slot is released", func(t *testing.T) {
		r, rx, sl := h.runner()
		sl.onSleep = func(int, time.Duration) {
			if err := h.st.FinalizeEvaluationAttempt(ctx, store.EvaluationAttemptResult{
				AttemptID: held.Attempt.ID, FenceAttemptID: held.Attempt.ID, ExitClass: store.EvalExitCancelled,
			}); err != nil {
				t.Errorf("release: %v", err)
			}
		}
		rep := r.Run(ctx, h.job(1, cand))
		if rep.Kind != Kind(store.EvalExitCompleted) || rep.Deferrals != 1 || rx.calls() != 1 {
			t.Fatalf("report = %+v, launches = %d", rep, rx.calls())
		}
		if len(sl.durations()) != 1 {
			t.Fatalf("sleeps = %v", sl.durations())
		}
	})
}

func TestPrelaunchDeferralRatePool(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	h := newHarness(t, 2, store.WithClock(clock))
	h.pool(store.EvaluationPoolConfig{ID: "metered", StartRate: 0.01, BurstCapacity: 1, ConcurrentLimit: 10})
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "metered"), 5)
	ctx := context.Background()

	r, rx, sl := h.runner(func(c *Config) { c.MaxDeferWait = time.Minute })
	if rep := r.Run(ctx, h.job(0, cand)); rep.Kind != Kind(store.EvalExitCompleted) {
		t.Fatalf("first run = %+v", rep)
	}

	rep := r.Run(ctx, h.job(1, cand))
	if rep.Kind != KindDeferred || rep.NotBefore.IsZero() || rx.calls() != 1 {
		t.Fatalf("second run = %+v, launches = %d", rep, rx.calls())
	}
	if h.attemptsUsed() != 1 {
		t.Fatalf("a refused start consumed capacity: %d", h.attemptsUsed())
	}

	r, rx, sl = h.runner(func(c *Config) { c.MaxDeferWait = 10 * time.Minute })
	sl.advance = func(d time.Duration) { now = now.Add(d) }
	rep = r.Run(ctx, h.job(1, cand))
	if rep.Kind != Kind(store.EvalExitCompleted) || rx.calls() != 1 {
		t.Fatalf("after waiting: %+v, launches = %d", rep, rx.calls())
	}
	if got := sl.durations(); len(got) != 1 || got[0] < time.Second || got[0] > 2*time.Minute {
		t.Fatalf("sleeps = %v, want one wait derived from the pool's NotBefore", got)
	}
}

func TestRetriesAreFinite(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeFailed, ident("alpha", "model-a", "pool-a"), 10)
	r, rx, sl := h.runner()

	rep := r.Run(context.Background(), h.job(0, cand))

	if rep.Kind != Kind(store.EvalExitFailed) || !rep.RetriesExhausted || len(rep.Attempts) != 3 || rx.calls() != 3 {
		t.Fatalf("report = %+v, launches = %d", rep, rx.calls())
	}
	if got, want := sl.durations(), []time.Duration{5 * time.Second, 10 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("backoff = %v, want %v", got, want)
	}
	for i, rec := range rep.Attempts {
		att := h.attempt(rec.AttemptID)
		if att.SequenceNumber != i+1 || att.ExitClass == nil || *att.ExitClass != store.EvalExitFailed {
			t.Errorf("attempt %d = %+v", i, att)
		}
	}
}

func TestCapExhaustionIsItsOwnOutcome(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeFailed, ident("alpha", "model-a", "pool-a"), 2)
	r, rx, _ := h.runner(func(c *Config) { c.MaxAttempts = 5 })

	rep := r.Run(context.Background(), h.job(0, cand))

	if rep.Kind != KindCapacityExhausted || len(rep.Attempts) != 2 || rx.calls() != 2 || rep.RetriesExhausted {
		t.Fatalf("report = %+v, launches = %d", rep, rx.calls())
	}
	if !errors.Is(rep.Err, store.ErrEvaluationCapacityExhausted) {
		t.Fatalf("err = %v", rep.Err)
	}
}

func TestRetryIsFreshAndCarriesNoFeedback(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	id := ident("alpha", "model-a", "pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeFailed, id, 5)
	okReg := evaluation.NewRegistry()
	registerAdapter(t, okReg, "alpha", evaluation.FakeModeSuccess, id)
	failing, succeeding := h.pipeline(), &evaluation.Pipeline{Registry: okReg}
	r, rx, _ := h.runner(func(c *Config) {
		c.Executor = &recExec{inner: &sequenceExec{pipes: []*evaluation.Pipeline{failing, succeeding}}}
	})
	rx = r.cfg.Executor.(*recExec)

	rep := r.Run(context.Background(), h.job(0, cand))

	if rep.Kind != Kind(store.EvalExitCompleted) || rep.RetriesExhausted || len(rep.Attempts) != 2 {
		t.Fatalf("report = %+v", rep)
	}
	first, second := rx.request(0), rx.request(1)
	if first.BlindedPrompt != second.BlindedPrompt {
		t.Error("the retry's prompt differs from the first attempt's")
	}
	if strings.Contains(second.BlindedPrompt, "fake: runtime failure") || strings.Contains(second.BlindedPrompt, rep.Attempts[0].AttemptID) {
		t.Error("the retry's prompt carries the failed attempt's feedback")
	}
	if first.RunID == second.RunID || first.SnapshotPath == second.SnapshotPath || first.ResultPath == second.ResultPath {
		t.Errorf("the retry reused the first attempt's run, workspace or result path: %+v %+v", first, second)
	}
	if got, _ := h.st.ListEvaluationFindings(context.Background(), rep.Attempts[0].AttemptID); len(got) != 0 {
		t.Errorf("the failed attempt recorded findings: %v", got)
	}
}

func TestLeaseIsRenewedWhileTheCandidateRuns(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
	fb := &faultBackend{Store: h.st, log: h.log}
	r, _, _ := h.runner(func(c *Config) {
		c.Backend = fb
		c.LeaseTTL, c.RenewEvery = 600*time.Millisecond, 100*time.Millisecond
		inner := c.Executor
		c.Executor = funcExec(func(ctx context.Context, rt string, req evaluation.CandidateRequest) (evaluation.Result, error) {
			time.Sleep(1500 * time.Millisecond)
			return inner.Execute(ctx, rt, req)
		})
	})

	rep := r.Run(context.Background(), h.job(0, cand))

	if rep.Kind != Kind(store.EvalExitCompleted) {
		t.Fatalf("a run longer than its lease must survive on renewals: %+v (finalize err %v)", rep, rep.Attempts[0].FinalizeErr)
	}
	if rec := rep.Attempts[0]; rec.Renewals < 5 || rec.LeaseErr != nil {
		t.Fatalf("renewals = %d, lease err = %v", rec.Renewals, rec.LeaseErr)
	}
	if fb.renewN < 5 {
		t.Fatalf("store saw %d renewals", fb.renewN)
	}
}

func TestRenewalFailureCancelsTheRunAndIsReported(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeHang, ident("alpha", "model-a", "pool-a"), 5)
	fb := &faultBackend{Store: h.st, log: h.log, renew: func(int) error { return errors.New("store unreachable") }}
	r, _, _ := h.runner(func(c *Config) {
		c.Backend = fb
		c.LeaseTTL, c.RenewEvery = time.Minute, 50*time.Millisecond
	})

	rep := r.Run(context.Background(), h.job(0, cand))

	if len(rep.Attempts) != 1 || rep.Kind != Kind(store.EvalExitCancelled) {
		t.Fatalf("report = %+v", rep)
	}
	rec := rep.Attempts[0]
	if !errors.Is(rec.LeaseErr, errLeaseLost) || !strings.Contains(rec.LeaseErr.Error(), "store unreachable") {
		t.Fatalf("renewal failure was swallowed: %v", rec.LeaseErr)
	}
	if rec.Result.ErrorMessage == nil || !strings.Contains(*rec.Result.ErrorMessage, "lease renewal failed") {
		t.Fatalf("result does not say why the run was cancelled: %+v", rec.Result)
	}
	renewFail, execReturn, fin := h.log.index("renew-fail"), h.log.index("exec-return"), h.log.index("finalize")
	if renewFail < 0 || !(renewFail < execReturn && execReturn < fin) {
		t.Fatalf("event order = %v; the result must be written after the process has exited", h.log.events)
	}
}

func TestFinalizeUsesACtxThatSurvivesCancellation(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
	fb := &faultBackend{Store: h.st, log: h.log}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, _, _ := h.runner(func(c *Config) {
		c.Backend = fb
		rx := &recExec{inner: h.pipeline(), log: h.log, hook: func(context.Context, int) { cancel() }}
		c.Executor = rx
	})

	rep := r.Run(ctx, h.job(0, cand))

	if len(rep.Attempts) != 1 || rep.Kind != Kind(store.EvalExitCancelled) {
		t.Fatalf("report = %+v", rep)
	}
	if !rep.Attempts[0].Recorded || fb.finalizeCalls() != 1 || fb.finalizeCtx[0] != nil {
		t.Fatalf("finalize was not recorded with a live ctx: recorded=%v calls=%d ctx=%v",
			rep.Attempts[0].Recorded, fb.finalizeCalls(), fb.finalizeCtx)
	}
	att := h.attempt(rep.Attempts[0].AttemptID)
	if att.State != store.EvalAttemptFinalized || *att.ExitClass != store.EvalExitCancelled {
		t.Fatalf("attempt = %+v", att)
	}
	if exists(h.stager.stagedAt(0).Workspace.Path) {
		t.Error("workspace left behind after cancellation")
	}
}

func TestAttemptIsFinalizedWhenTheExecutorPanics(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
	fb := &faultBackend{Store: h.st, log: h.log}
	r, _, _ := h.runner(func(c *Config) {
		c.Backend = fb
		c.Executor = funcExec(func(context.Context, string, evaluation.CandidateRequest) (evaluation.Result, error) {
			panic("executor exploded")
		})
	})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic was swallowed")
			}
		}()
		r.Run(context.Background(), h.job(0, cand))
	}()

	if fb.finalizeCalls() != 1 {
		t.Fatalf("finalize calls = %d", fb.finalizeCalls())
	}
	res := fb.offered[0]
	att := h.attempt(res.AttemptID)
	if att.State != store.EvalAttemptFinalized || *att.ExitClass != store.EvalExitUnknown {
		t.Fatalf("attempt = %+v", att)
	}
	if exists(h.stager.stagedAt(0).Workspace.Path) {
		t.Error("workspace left behind after a panic")
	}
}

func TestFinalizeFaults(t *testing.T) {
	setup := func(t *testing.T, fail func(int, store.EvaluationAttemptResult) error) (*harness, *faultBackend, Report, *sleeper) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
		fb := &faultBackend{Store: h.st, log: h.log, finalize: fail}
		r, _, sl := h.runner(func(c *Config) { c.Backend = fb })
		return h, fb, r.Run(context.Background(), h.job(0, cand)), sl
	}

	t.Run("transient errors are retried", func(t *testing.T) {
		h, fb, rep, sl := setup(t, func(n int, _ store.EvaluationAttemptResult) error {
			if n <= 2 {
				return errors.New("database is locked")
			}
			return nil
		})
		if rep.Kind != Kind(store.EvalExitCompleted) || fb.finalizeCalls() != 3 || !rep.Attempts[0].Recorded {
			t.Fatalf("report = %+v, calls = %d", rep, fb.finalizeCalls())
		}
		if got := sl.durations(); len(got) != 2 {
			t.Fatalf("sleeps = %v", got)
		}
		if att := h.attempt(rep.Attempts[0].AttemptID); *att.ExitClass != store.EvalExitCompleted {
			t.Fatalf("attempt = %+v", att)
		}
	})

	t.Run("a persistent failure is reported as unrecorded", func(t *testing.T) {
		_, fb, rep, _ := setup(t, func(int, store.EvaluationAttemptResult) error { return errors.New("database is locked") })
		if rep.Kind != KindUnrecorded || fb.finalizeCalls() != 4 || rep.Attempts[0].Recorded || rep.Err == nil {
			t.Fatalf("report = %+v, calls = %d", rep, fb.finalizeCalls())
		}
	})

	t.Run("terminal store answers are not retried", func(t *testing.T) {
		_, fb, rep, _ := setup(t, func(int, store.EvaluationAttemptResult) error { return store.ErrEvaluationAttemptFinalized })
		if rep.Kind != KindUnrecorded || fb.finalizeCalls() != 1 || !errors.Is(rep.Err, store.ErrEvaluationAttemptFinalized) {
			t.Fatalf("report = %+v, calls = %d", rep, fb.finalizeCalls())
		}
	})

	t.Run("a refused result is replaced by an honest unknown", func(t *testing.T) {
		h, fb, rep, _ := setup(t, func(n int, _ store.EvaluationAttemptResult) error {
			if n == 1 {
				return store.ErrEvaluationInvalidInput
			}
			return nil
		})
		if rep.Kind != Kind(store.EvalExitUnknown) || fb.finalizeCalls() != 2 || !rep.Attempts[0].Recorded {
			t.Fatalf("report = %+v, calls = %d", rep, fb.finalizeCalls())
		}
		degraded := fb.offered[1]
		if degraded.ExitClass != store.EvalExitUnknown || len(degraded.Findings) != 0 || degraded.ErrorMessage == nil {
			t.Fatalf("degraded result = %+v", degraded)
		}
		if att := h.attempt(rep.Attempts[0].AttemptID); att.State != store.EvalAttemptFinalized {
			t.Fatalf("attempt = %+v", att)
		}
	})

	t.Run("a lease the store already expired is recorded as lease_expired and retried", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
		fb := &faultBackend{Store: h.st, log: h.log}
		fb.finalize = func(n int, _ store.EvaluationAttemptResult) error {
			if n > 1 {
				return nil
			}
			if _, err := h.st.ExpireEvaluationAttempts(context.Background(), time.Now().Add(time.Hour)); err != nil {
				t.Error(err)
			}
			return store.ErrEvaluationAttemptExpired
		}
		r, _, _ := h.runner(func(c *Config) { c.Backend = fb })
		rep := r.Run(context.Background(), h.job(0, cand))
		if len(rep.Attempts) != 2 || rep.Attempts[0].Exit != store.EvalExitLeaseExpired || !rep.Attempts[0].Recorded ||
			rep.Kind != Kind(store.EvalExitCompleted) {
			t.Fatalf("report = %+v", rep)
		}
		if att := h.attempt(rep.Attempts[0].AttemptID); *att.ExitClass != store.EvalExitLeaseExpired {
			t.Fatalf("attempt = %+v", att)
		}
	})
}

func TestMalformedAndPartialResultsAreNeverCleanReviews(t *testing.T) {
	cases := []struct {
		mode      string
		exit      store.EvaluationExitClass
		class     evaluation.ErrorClass
		retryable bool
	}{
		{evaluation.FakeModeMalformed, store.EvalExitInvalidOutput, evaluation.ErrClassOutputMalformed, true},
		{evaluation.FakeModeInvalid, store.EvalExitInvalidOutput, evaluation.ErrClassOutputMalformed, true},
		{evaluation.FakeModeWrongRun, store.EvalExitInvalidOutput, evaluation.ErrClassOutputMalformed, true},
		{evaluation.FakeModeSilent, store.EvalExitInvalidOutput, evaluation.ErrClassOutputMissing, true},
		{evaluation.FakeModeIncomplete, store.EvalExitUnavailableSource, evaluation.ErrClassSourceUnavailable, true},
		{evaluation.FakeModeFailed, store.EvalExitFailed, evaluation.ErrClassRuntimeError, true},
		{evaluation.FakeModeCrash, store.EvalExitFailed, evaluation.ErrClassRuntimeError, true},
		{evaluation.FakeModeDirtyExit, store.EvalExitFailed, evaluation.ErrClassRuntimeError, true},
		{evaluation.FakeModeUnsupported, store.EvalExitFailed, evaluation.ErrClassCapabilityMissing, false},
		{evaluation.FakeModeInterrupted, store.EvalExitUnknown, evaluation.ErrClassInterrupted, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			h := newHarness(t, 1)
			h.openPool("pool-a")
			cand := h.addCandidate("alpha", tc.mode, ident("alpha", "model-a", "pool-a"), 10)
			r, rx, _ := h.runner()

			rep := r.Run(context.Background(), h.job(0, cand))

			wantAttempts := 1
			if tc.retryable {
				wantAttempts = 3
			}
			if len(rep.Attempts) != wantAttempts || rx.calls() != wantAttempts || rep.Kind != Kind(tc.exit) ||
				rep.RetriesExhausted != tc.retryable {
				t.Fatalf("report = %+v (launches %d)", rep, rx.calls())
			}
			for _, rec := range rep.Attempts {
				att := h.attempt(rec.AttemptID)
				if *att.ExitClass != tc.exit || att.ErrorClass == nil || *att.ErrorClass != string(tc.class) {
					t.Fatalf("attempt = %+v (class %v)", att, att.ErrorClass)
				}
				if f, _ := h.st.ListEvaluationFindings(context.Background(), rec.AttemptID); len(f) != 0 {
					t.Fatalf("findings recorded for a %s attempt: %v", tc.mode, f)
				}
			}
		})
	}

	t.Run("timeout", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeHang, ident("alpha", "model-a", "pool-a"), 10,
			func(rt *evaluation.Runtime) { rt.Timeout = 300 * time.Millisecond })
		r, _, _ := h.runner(func(c *Config) { c.MaxAttempts = 1 })
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != Kind(store.EvalExitTimeout) {
			t.Fatalf("report = %+v", rep)
		}
	})
}

func TestCompletionNeedsAnExplicitValidatedReview(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 10)
	id := cand.Config.Identity()
	respond := func(completed bool, findings []evaluation.Finding) funcExec {
		return func(_ context.Context, _ string, req evaluation.CandidateRequest) (evaluation.Result, error) {
			start := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
			return evaluation.Result{
				Runtime: "alpha", CandidateDigest: cand.Digest(), Launched: true,
				Response: evaluation.CandidateResponse{
					Version: evaluation.ProtocolVersion, RunID: req.RunID, Status: evaluation.StatusCompleted,
					ReviewCompleted: completed, Findings: findings, Identity: id,
					Timing: evaluation.Timing{StartedAt: start, FinishedAt: start.Add(1500 * time.Millisecond)},
				},
			}, nil
		}
	}

	t.Run("a completed review with no findings is a clean review", func(t *testing.T) {
		r, _, _ := h.runner(func(c *Config) { c.Executor = respond(true, nil) })
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != Kind(store.EvalExitCompleted) {
			t.Fatalf("report = %+v (finalize err %v)", rep, rep.Attempts[0].FinalizeErr)
		}
		if att := h.attempt(rep.Attempts[0].AttemptID); att.DurationMs == nil || *att.DurationMs != 1500 {
			t.Fatalf("duration = %v", att.DurationMs)
		}
	})

	t.Run("a response that did not finish the review is not clean", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 10)
		r, _, _ := h.runner(func(c *Config) { c.Executor = respond(false, nil); c.MaxAttempts = 1 })
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != Kind(store.EvalExitInvalidOutput) || !rep.Attempts[0].Recorded {
			t.Fatalf("report = %+v (finalize err %v)", rep, rep.Attempts[0].FinalizeErr)
		}
	})

	t.Run("an executor error is a host fault, not a review", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 10)
		r, _, _ := h.runner(func(c *Config) {
			c.Executor = funcExec(func(context.Context, string, evaluation.CandidateRequest) (evaluation.Result, error) {
				return evaluation.Result{}, errors.New("disk full")
			})
		})
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != Kind(store.EvalExitUnknown) || len(rep.Attempts) != 1 {
			t.Fatalf("report = %+v", rep)
		}
		if msg := rep.Attempts[0].Result.ErrorMessage; msg == nil || !strings.Contains(*msg, "disk full") {
			t.Fatalf("message = %v", msg)
		}
	})
}

func TestCandidateEnvironmentIsIsolated(t *testing.T) {
	const secret = "s3cr3t-adapter-credential"
	t.Setenv("ODONIAN_TOKEN", "board-token")
	t.Setenv("ODONIAN_API_URL", "http://board.invalid")
	t.Setenv("GITHUB_TOKEN", "forge-token")
	t.Setenv("GH_TOKEN", "forge-token-2")
	t.Setenv("OTHER_REVIEWER_RESULT", "another reviewer's verdict")
	t.Setenv("HOME", t.TempDir())

	h := newHarness(t, 1)
	h.openPool("pool-a")
	h.creds["adapter-key"] = secret
	withCred := func(rt *evaluation.Runtime) {
		rt.Credentials = []evaluation.CredentialRef{{EnvName: "ADAPTER_API_KEY", Ref: "adapter-key"}}
		rt.PassThroughEnv = []string{"HOME"}
	}
	probe := h.addCandidate("probe", evaluation.FakeModeProbe, ident("alpha", "model-a", "pool-a"), 5, withCred)
	r, _, _ := h.runner()

	rep := r.Run(context.Background(), h.job(0, probe))
	if rep.Kind != Kind(store.EvalExitCompleted) {
		t.Fatalf("report = %+v", rep)
	}
	data, err := os.ReadFile(evaluation.FakeProbePath(h.stager.stagedAt(0).Request.ResultPath))
	if err != nil {
		t.Fatal(err)
	}
	var seen evaluation.FakeProbe
	if err := json.Unmarshal(data, &seen); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"PATH": true, "HOME": true, "ADAPTER_API_KEY": true, "ODONIAN_EVAL_PROTOCOL": true}
	got := map[string]string{}
	for _, kv := range seen.Env {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = value
		if !allowed[name] {
			t.Errorf("candidate saw %s", name)
		}
	}
	if got["ADAPTER_API_KEY"] != secret {
		t.Errorf("the configured credential is missing: %v", got)
	}

	t.Run("a credential echoed by the adapter is not recorded", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		h.creds["adapter-key"] = secret
		leak := h.addCandidate("leak", evaluation.FakeModeLeakValid, ident("alpha", "model-a", "pool-a"), 5, withCred,
			func(rt *evaluation.Runtime) { rt.Args = append(rt.Args, "--secret-env", "ADAPTER_API_KEY") })
		r, _, _ := h.runner(func(c *Config) { c.MaxAttempts = 1 })
		rep := r.Run(context.Background(), h.job(0, leak))
		if len(rep.Attempts) != 1 {
			t.Fatalf("report = %+v", rep)
		}
		att := h.attempt(rep.Attempts[0].AttemptID)
		blob, _ := json.Marshal([]any{att, h.detail(att.ID), rep.Attempts[0].Result})
		if strings.Contains(string(blob), secret) {
			t.Fatalf("the credential reached the recorded result: %s", blob)
		}
	})
}

func TestStagingFailures(t *testing.T) {
	t.Run("an unavailable original is recorded and not retried", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
		h.stager.err = &evalcohort.UnavailableError{Reason: evaluation.UnavailFrozenInputChanged, Detail: "round-1 record gone"}
		r, rx, _ := h.runner()
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != Kind(store.EvalExitUnavailableSnapshot) || len(rep.Attempts) != 1 || rx.calls() != 0 {
			t.Fatalf("report = %+v, launches = %d", rep, rx.calls())
		}
		att := h.attempt(rep.Attempts[0].AttemptID)
		if att.State != store.EvalAttemptFinalized || att.ErrorMessage == nil || !strings.Contains(*att.ErrorMessage, "round-1 record gone") {
			t.Fatalf("attempt = %+v", att)
		}
	})

	t.Run("a workspace changed after staging is not run", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
		h.stager.afterStage = func(s *evalcohort.Staged) {
			if err := os.WriteFile(filepath.Join(s.Workspace.Path, "planted.txt"), []byte("x"), 0o600); err != nil {
				t.Error(err)
			}
		}
		r, rx, _ := h.runner()
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != Kind(store.EvalExitUnavailableSnapshot) || rx.calls() != 0 {
			t.Fatalf("report = %+v, launches = %d", rep, rx.calls())
		}
		if exists(h.stager.stagedAt(0).Workspace.Path) {
			t.Error("tampered workspace left behind")
		}
	})

	t.Run("any other staging error is an unknown host fault", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
		h.stager.err = errors.New("no space left")
		r, rx, _ := h.runner()
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != Kind(store.EvalExitUnknown) || len(rep.Attempts) != 1 || rx.calls() != 0 {
			t.Fatalf("report = %+v", rep)
		}
	})
}

func TestNothingLaunchesWithoutAnAttempt(t *testing.T) {
	t.Run("paused campaign", func(t *testing.T) {
		h := newHarness(t, 2)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
		if err := h.st.PauseEvaluationCampaign(context.Background(), h.campaign.ID); err != nil {
			t.Fatal(err)
		}
		r, rx, _ := h.runner()
		reports, err := r.RunCampaign(context.Background(), h.campaign.ID)
		if err != nil || len(reports) != 1 || reports[0].Kind != KindPaused || rx.calls() != 0 || len(h.stager.reqs) != 0 {
			t.Fatalf("reports = %+v, err = %v", reports, err)
		}
		_ = cand
	})

	t.Run("candidate without a registered runtime", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.storeOnly(ident("ghost", "model-c", "pool-a"), 5)
		r, rx, _ := h.runner()
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != KindNotRegistered || !errors.Is(rep.Err, ErrNotRegistered) || rx.calls() != 0 || h.attemptsUsed() != 0 {
			t.Fatalf("report = %+v, attempts = %d", rep, h.attemptsUsed())
		}
	})

	t.Run("pool that was never configured", func(t *testing.T) {
		h := newHarness(t, 1)
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "no-such-pool"), 5)
		r, rx, _ := h.runner()
		rep := r.Run(context.Background(), h.job(0, cand))
		if rep.Kind != KindPoolNotConfigured || rx.calls() != 0 {
			t.Fatalf("report = %+v", rep)
		}
	})

	t.Run("candidate of another campaign", func(t *testing.T) {
		h := newHarness(t, 1)
		h.openPool("pool-a")
		cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
		r, rx, _ := h.runner()
		job := h.job(0, cand)
		job.CampaignID = "other"
		if rep := r.Run(context.Background(), job); rep.Kind != KindHostError || rx.calls() != 0 {
			t.Fatalf("report = %+v", rep)
		}
	})
}

func TestAttemptIDMustBeUsableAsARunID(t *testing.T) {
	h := newHarness(t, 1)
	h.openPool("pool-a")
	cand := h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
	fb := &faultBackend{Store: h.st, log: h.log, finalize: func(int, store.EvaluationAttemptResult) error { return errors.New("stop") }}
	fb.claimHook = func(res *store.EvaluationJobClaimResult) { res.Attempt.ID = "../escape" }
	r, rx, _ := h.runner(func(c *Config) { c.Backend = fb; c.FinalizeRetries = 1 })

	rep := r.Run(context.Background(), h.job(0, cand))

	if rx.calls() != 0 || len(h.stager.reqs) != 0 {
		t.Fatalf("a bad run id was staged or launched")
	}
	if len(fb.offered) == 0 || fb.offered[0].ExitClass != store.EvalExitUnknown {
		t.Fatalf("offered = %+v (report %+v)", fb.offered, rep)
	}
}

func TestNewValidatesItsBounds(t *testing.T) {
	h := newHarness(t, 0)
	base := Config{Backend: h.st, Registry: h.reg, Stager: h.stager, Executor: h.pipeline()}
	for name, mutate := range map[string]func(*Config){
		"no backend":      func(c *Config) { c.Backend = nil },
		"negative bound":  func(c *Config) { c.MaxAttempts = -1 },
		"slow renewal":    func(c *Config) { c.LeaseTTL, c.RenewEvery = time.Minute, time.Minute },
		"duplicate tools": func(c *Config) { c.ToolAccess = evaluation.ToolAccessRequirements{Tools: []string{"a", "a"}} },
	} {
		cfg := base
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	r, err := New(base)
	if err != nil || r.cfg.MaxAttempts != 3 || r.cfg.RenewEvery != r.cfg.LeaseTTL/3 || !r.cfg.ToolAccess.RequireSourceRetrieval {
		t.Fatalf("defaults = %+v, %v", r, err)
	}
}
