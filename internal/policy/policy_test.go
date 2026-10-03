package policy

import (
	"sync"
	"testing"
	"time"
)

var t0 = time.Unix(1_000_000, 0)

func at(sec float64) time.Time { return t0.Add(time.Duration(sec * float64(time.Second))) }

func pool(name, acct string, rate float64, burst, limit, reserved int, models ...string) Pool {
	return Pool{Name: name, AccountID: acct, Models: models, StartRate: rate,
		BurstCapacity: burst, ConcurrentDispatchLimit: limit, CompletionReserved: reserved}
}

func cfgOf(mode Mode, pools ...Pool) Config {
	return Config{Mode: mode, AllowedModels: []string{"haiku", "sonnet", "opus"}, Pools: pools}
}

func newEval(t *testing.T, now time.Time, cfg Config) *Evaluator {
	t.Helper()
	e, err := New(now, cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return e
}

func eval(t *testing.T, e *Evaluator, now time.Time, model string, class WorkClass) Decision {
	t.Helper()
	d, err := e.Evaluate(now, model, class)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return d
}

func allModelsPool(rate float64, burst, limit, reserved int) Pool {
	return pool("p", "acct", rate, burst, limit, reserved, "haiku", "sonnet", "opus")
}

func TestWorkClassTable(t *testing.T) {
	cases := []struct {
		class      WorkClass
		paced      bool
		completion bool
	}{
		{ResearchWrite, true, false},
		{ResearchReview, true, true},
		{ResearchRework, true, true},
		{ResearchAdjudication, true, true},
		{BuildWork, false, false},
		{DesignWork, false, false},
		{MergeWork, false, false},
	}
	for _, c := range cases {
		if c.class.Paced() != c.paced || c.class.Completion() != c.completion {
			t.Errorf("%s: paced=%v completion=%v, want %v %v", c.class, c.class.Paced(), c.class.Completion(), c.paced, c.completion)
		}
	}
}

// Every class in every mode, against a pool with exactly one start and one
// slot available. Paced classes consume it; unpaced classes never touch it.
func TestClassesByMode(t *testing.T) {
	classes := []WorkClass{ResearchWrite, ResearchReview, ResearchRework, ResearchAdjudication, BuildWork, DesignWork, MergeWork}
	for _, mode := range []Mode{ModeDisabled, ModeObserve, ModeEnforce} {
		for _, class := range classes {
			t.Run(string(mode)+"/"+string(class), func(t *testing.T) {
				e := newEval(t, t0, cfgOf(mode, allModelsPool(0.001, 1, 1, 0)))
				first := eval(t, e, t0, "opus", class)
				if first.Outcome != OutcomeAdmit || first.Shadow.Outcome != OutcomeAdmit {
					t.Fatalf("first start: %+v", first)
				}
				second := eval(t, e, t0, "opus", class)
				pacedActive := class.Paced() && mode != ModeDisabled

				if pacedActive {
					if first.Ticket == nil {
						t.Fatal("paced start should be tracked")
					}
					if second.Shadow.Outcome != OutcomeRetry || second.Shadow.Reason != ReasonConcurrency {
						t.Fatalf("shadow second = %+v, want concurrency retry", second.Shadow)
					}
					wantActual := OutcomeRetry
					if mode == ModeObserve {
						wantActual = OutcomeAdmit
					}
					if second.Outcome != wantActual {
						t.Fatalf("second outcome %s, want %s", second.Outcome, wantActual)
					}
				} else {
					if first.Ticket != nil || second.Outcome != OutcomeAdmit || second.Shadow.Outcome != OutcomeAdmit {
						t.Fatalf("unpaced/disabled must be untouched: first=%+v second=%+v", first, second)
					}
					// And it must not have consumed anything a paced start needs.
					if got := eval(t, e, t0, "opus", ResearchWrite); mode != ModeDisabled && got.Outcome != OutcomeAdmit {
						t.Fatalf("unpaced work consumed allowance: %+v", got)
					}
				}
			})
		}
	}
}

func TestUnknownWorkClass(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(1, 1, 1, 0)))
	if _, err := e.Evaluate(t0, "opus", WorkClass("bogus")); err == nil {
		t.Fatal("expected error for unknown class")
	}
}

func TestUnmappedModel(t *testing.T) {
	cfg := cfgOf(ModeEnforce, pool("p", "a", 1, 1, 1, 0, "opus"))
	cfg.AllowedModels = []string{"opus"}
	e := newEval(t, t0, cfg)
	d := eval(t, e, t0, "gpt", ResearchWrite)
	if d.Outcome != OutcomeUnmapped || !d.NotBefore.IsZero() || d.RetryAfter != 0 || d.Ticket != nil {
		t.Fatalf("enforce unmapped = %+v", d)
	}

	cfg.Mode = ModeObserve
	e = newEval(t, t0, cfg)
	d = eval(t, e, t0, "gpt", ResearchWrite)
	if d.Outcome != OutcomeAdmit || d.Shadow.Outcome != OutcomeUnmapped {
		t.Fatalf("observe unmapped = %+v", d)
	}
}

func TestEnforceBurstThenRate(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(0.5, 3, 100, 0)))
	for i := 0; i < 3; i++ {
		if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeAdmit {
			t.Fatalf("burst start %d: %+v", i, d)
		}
	}
	d := eval(t, e, t0, "opus", ResearchWrite)
	if d.Outcome != OutcomeDefer || d.Reason != ReasonRate || !d.NotBefore.Equal(at(2)) || d.Ticket != nil {
		t.Fatalf("after burst = %+v, want defer rate until +2s", d)
	}
	if d := eval(t, e, at(2).Add(-time.Millisecond), "opus", ResearchWrite); d.Outcome != OutcomeDefer {
		t.Fatalf("just before NotBefore: %+v", d)
	}
	if d := eval(t, e, d.NotBefore, "opus", ResearchWrite); d.Outcome != OutcomeAdmit {
		t.Fatalf("at NotBefore: %+v", d)
	}
}

func admitsOver(t *testing.T, e *Evaluator, from, to, step float64, class WorkClass) int {
	t.Helper()
	n := 0
	steps := int((to-from)/step + 0.5)
	for i := 0; i <= steps; i++ {
		d := eval(t, e, at(from+float64(i)*step), "opus", class)
		if d.Outcome == OutcomeAdmit {
			n++
			e.Release(d.Ticket)
		}
	}
	return n
}

func TestFractionalRefillExactCounts(t *testing.T) {
	cases := []struct {
		name      string
		rate      float64
		burst     int
		step, end float64
		want      int
	}{
		// Bursts of denied checks must not be credited the same time twice.
		{"0.1/s burst1 checked every 100ms for 10s", 0.1, 1, 0.1, 10, 2},
		{"0.1/s burst1 checked every 1s for 10s", 0.1, 1, 1, 10, 2},
		{"0.1/s burst1 checked every 1s for 9s", 0.1, 1, 1, 9, 1},
		{"0.25/s burst2 checked every 1s for 8s", 0.25, 2, 1, 8, 4},
		{"2/s burst1 checked every 0.25s for 3s", 2, 1, 0.25, 3, 7},
		{"0.001/s burst3 for 100s", 0.001, 3, 1, 100, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEval(t, at(0), cfgOf(ModeEnforce, allModelsPool(c.rate, c.burst, 100, 0)))
			if got := admitsOver(t, e, 0, c.end, c.step, ResearchWrite); got != c.want {
				t.Fatalf("admits = %d, want %d", got, c.want)
			}
		})
	}
}

func TestIdleBurstCeiling(t *testing.T) {
	e := newEval(t, at(0), cfgOf(ModeEnforce, allModelsPool(1, 4, 100, 0)))
	for i := 0; i < 4; i++ {
		eval(t, e, at(0), "opus", ResearchWrite)
	}
	// A very long idle period refills to the burst and no further.
	n := 0
	for i := 0; i < 20; i++ {
		if eval(t, e, at(86400), "opus", ResearchWrite).Outcome == OutcomeAdmit {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("admits after long idle = %d, want burst of 4", n)
	}
}

func TestClockRollback(t *testing.T) {
	for _, mode := range []Mode{ModeEnforce, ModeObserve} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEval(t, at(1000), cfgOf(mode, allModelsPool(0.1, 2, 100, 0)))
			count := func(now float64, n int) int {
				got := 0
				for i := 0; i < n; i++ {
					d := eval(t, e, at(now), "opus", ResearchWrite)
					if d.Shadow.Outcome == OutcomeAdmit {
						got++
					}
				}
				return got
			}
			total := count(1000, 1) // burst token 1
			total += count(900, 3)  // rolled back: burst token 2 only
			if total != 2 {
				t.Fatalf("admits before recovery = %d, want 2", total)
			}
			// Rolled-back time must not have moved the high-water mark:
			// 10s of real time past 1000 earns exactly one token.
			if got := count(1010, 3); got != 1 {
				t.Fatalf("admits after clock recovers = %d, want 1", got)
			}
		})
	}
}

func TestRollbackDeferralIsRelativeToHighWaterMark(t *testing.T) {
	e := newEval(t, at(1000), cfgOf(ModeEnforce, allModelsPool(0.1, 1, 100, 0)))
	eval(t, e, at(1000), "opus", ResearchWrite)
	d := eval(t, e, at(500), "opus", ResearchWrite)
	if d.Outcome != OutcomeDefer || !d.NotBefore.Equal(at(1010)) {
		t.Fatalf("rolled-back deferral = %+v, want NotBefore +1010", d)
	}
}

func TestReservationSplit(t *testing.T) {
	type step struct {
		class WorkClass
		want  Outcome
		why   Reason
	}
	cases := []struct {
		name    string
		limit   int
		reserve int
		steps   []step
	}{
		{"writers stop at limit-reserved, leaving the reserve", 3, 1, []step{
			{ResearchWrite, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeRetry, ReasonReservedCapacity},
		}},
		{"completion may use the whole ceiling, not beyond", 3, 1, []step{
			{ResearchReview, OutcomeAdmit, ""},
			{ResearchRework, OutcomeAdmit, ""},
			{ResearchAdjudication, OutcomeAdmit, ""},
			{ResearchReview, OutcomeRetry, ReasonConcurrency},
		}},
		{"writers cannot borrow reserve while completion is idle", 2, 1, []step{
			{ResearchWrite, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeRetry, ReasonReservedCapacity},
			{ResearchReview, OutcomeAdmit, ""},
		}},
		{"completion at the ceiling also blocks a writer on the total", 2, 1, []step{
			{ResearchReview, OutcomeAdmit, ""},
			{ResearchReview, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeRetry, ReasonConcurrency},
		}},
		{"completion that spilled into writer slots blocks a writer", 2, 1, []step{
			{ResearchReview, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeRetry, ReasonConcurrency},
		}},
		{"reserve equal to limit makes a review-only pool", 2, 2, []step{
			{ResearchWrite, OutcomeRetry, ReasonReservedCapacity},
			{ResearchReview, OutcomeAdmit, ""},
			{ResearchReview, OutcomeAdmit, ""},
			{ResearchReview, OutcomeRetry, ReasonConcurrency},
		}},
		{"no reservation: writers use the full limit", 2, 0, []step{
			{ResearchWrite, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeAdmit, ""},
			{ResearchWrite, OutcomeRetry, ReasonConcurrency},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(1, 100, c.limit, c.reserve)))
			for i, s := range c.steps {
				d := eval(t, e, t0, "opus", s.class)
				if d.Outcome != s.want || d.Reason != s.why {
					t.Fatalf("step %d (%s): %s/%s, want %s/%s", i, s.class, d.Outcome, d.Reason, s.want, s.why)
				}
				if s.want == OutcomeRetry && d.RetryAfter != DefaultConcurrencyRetry {
					t.Fatalf("step %d RetryAfter = %v", i, d.RetryAfter)
				}
			}
		})
	}
}

func TestConcurrencyRetrySpendsNoToken(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(0.001, 2, 1, 0)))
	first := eval(t, e, t0, "opus", ResearchWrite)
	for i := 0; i < 5; i++ {
		if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeRetry {
			t.Fatalf("expected retry, got %+v", d)
		}
	}
	e.Release(first.Ticket)
	if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeAdmit {
		t.Fatalf("second burst token should remain: %+v", d)
	}
}

func TestConfiguredRetryAfter(t *testing.T) {
	cfg := cfgOf(ModeEnforce, allModelsPool(1, 5, 1, 0))
	cfg.ConcurrencyRetry = 7 * time.Second
	e := newEval(t, t0, cfg)
	eval(t, e, t0, "opus", ResearchWrite)
	if d := eval(t, e, t0, "opus", ResearchWrite); d.RetryAfter != 7*time.Second {
		t.Fatalf("RetryAfter = %v", d.RetryAfter)
	}
}

func TestReleaseIsIdempotentAndScoped(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(1, 100, 1, 0)))
	other := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(1, 100, 1, 0)))
	a := eval(t, e, t0, "opus", ResearchWrite)

	e.Release(nil)
	other.Release(a.Ticket) // wrong evaluator: ignored
	if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeRetry {
		t.Fatalf("foreign release freed a slot: %+v", d)
	}
	e.Release(a.Ticket)
	e.Release(a.Ticket) // double release must not free a second slot
	b := eval(t, e, t0, "opus", ResearchWrite)
	if b.Outcome != OutcomeAdmit {
		t.Fatalf("release did not free slot: %+v", b)
	}
	if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeRetry {
		t.Fatalf("double release over-freed: %+v", d)
	}
}

func TestReleaseDoesNotRefundStart(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(0.001, 1, 5, 0)))
	d := eval(t, e, t0, "opus", ResearchWrite)
	e.Release(d.Ticket)
	if got := eval(t, e, t0, "opus", ResearchWrite); got.Outcome != OutcomeDefer {
		t.Fatalf("release refunded a start: %+v", got)
	}
}

func TestAliasesShareOnePool(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeEnforce,
		pool("shared", "anthropic", 0.001, 2, 10, 0, "opus", "sonnet"),
		pool("other", "meta", 0.001, 1, 10, 0, "haiku")))
	eval(t, e, t0, "opus", ResearchWrite)
	eval(t, e, t0, "sonnet", ResearchWrite)
	if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeDefer {
		t.Fatalf("aliases did not share allowance: %+v", d)
	}
	if d := eval(t, e, t0, "haiku", ResearchWrite); d.Outcome != OutcomeAdmit {
		t.Fatalf("other account affected: %+v", d)
	}
}

func TestObserveNeverDeniesAndReportsShadow(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeObserve, allModelsPool(0.001, 1, 100, 0)))
	shadow := map[Outcome]int{}
	for i := 0; i < 5; i++ {
		d := eval(t, e, t0, "opus", ResearchWrite)
		if d.Outcome != OutcomeAdmit {
			t.Fatalf("observe denied: %+v", d)
		}
		shadow[d.Shadow.Outcome]++
		if d.Shadow.Outcome == OutcomeDefer && (d.Shadow.Reason != ReasonRate || d.Shadow.NotBefore.IsZero()) {
			t.Fatalf("bad shadow deferral: %+v", d.Shadow)
		}
	}
	if shadow[OutcomeAdmit] != 1 || shadow[OutcomeDefer] != 4 {
		t.Fatalf("shadow counts = %v, want 1 admit and 4 defer", shadow)
	}
}

func TestObserveShadowConcurrencyAndReservation(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeObserve, allModelsPool(1, 100, 2, 1)))
	w1 := eval(t, e, t0, "opus", ResearchWrite)
	w2 := eval(t, e, t0, "opus", ResearchWrite)
	if w1.Shadow.Outcome != OutcomeAdmit || w2.Shadow.Reason != ReasonReservedCapacity || w2.Outcome != OutcomeAdmit {
		t.Fatalf("w1=%+v w2=%+v", w1, w2)
	}
	c := eval(t, e, t0, "opus", ResearchReview)
	if c.Shadow.Reason != ReasonConcurrency {
		t.Fatalf("review shadow = %+v (two writers are running)", c.Shadow)
	}
}

func TestDisabledTracksNothing(t *testing.T) {
	e := newEval(t, t0, cfgOf(ModeDisabled, allModelsPool(0.001, 1, 1, 0)))
	for i := 0; i < 10; i++ {
		d := eval(t, e, t0, "opus", ResearchWrite)
		if d.Outcome != OutcomeAdmit || d.Ticket != nil {
			t.Fatalf("disabled: %+v", d)
		}
	}
}

func TestReconfigure(t *testing.T) {
	drain := func(e *Evaluator, now time.Time) int {
		n := 0
		for i := 0; i < 50; i++ {
			d := eval(t, e, now, "opus", ResearchWrite)
			if d.Outcome != OutcomeAdmit {
				break
			}
			e.Release(d.Ticket)
			n++
		}
		return n
	}
	one := func(rate float64, burst int) Config {
		return cfgOf(ModeEnforce, allModelsPool(rate, burst, 100, 0))
	}

	t.Run("raising burst does not refill", func(t *testing.T) {
		e := newEval(t, t0, one(0.001, 2))
		if drain(e, t0) != 2 {
			t.Fatal("setup")
		}
		if err := e.Reconfigure(t0, one(0.001, 10)); err != nil {
			t.Fatal(err)
		}
		if got := drain(e, t0); got != 0 {
			t.Fatalf("admits after raising burst = %d, want 0", got)
		}
		// Refill then proceeds toward the new, larger burst.
		if got := drain(e, at(3000)); got != 3 {
			t.Fatalf("admits after 3000s at 0.001/s = %d, want 3", got)
		}
	})

	t.Run("lowering burst clamps tokens", func(t *testing.T) {
		e := newEval(t, t0, one(0.001, 10))
		if err := e.Reconfigure(t0, one(0.001, 2)); err != nil {
			t.Fatal(err)
		}
		if got := drain(e, t0); got != 2 {
			t.Fatalf("admits = %d, want 2", got)
		}
	})

	t.Run("rate change settles old rate first", func(t *testing.T) {
		e := newEval(t, t0, one(1, 5))
		drain(e, t0)
		// 3s pass at the old rate of 1/s before the change.
		if err := e.Reconfigure(at(3), one(0.001, 5)); err != nil {
			t.Fatal(err)
		}
		if got := drain(e, at(3)); got != 3 {
			t.Fatalf("admits = %d, want 3 earned at the old rate", got)
		}
		if got := drain(e, at(13)); got != 0 {
			t.Fatalf("admits 10s later at the new rate = %d, want 0", got)
		}
	})

	t.Run("reconfigure at a rolled-back time mints nothing", func(t *testing.T) {
		e := newEval(t, at(100), one(1, 5))
		drain(e, at(100))
		if err := e.Reconfigure(at(50), one(1, 5)); err != nil {
			t.Fatal(err)
		}
		if got := drain(e, at(100)); got != 0 {
			t.Fatalf("admits = %d, want 0", got)
		}
	})

	t.Run("removing and re-adding a pool does not refill it", func(t *testing.T) {
		e := newEval(t, t0, one(0.001, 5))
		drain(e, t0)
		if err := e.Reconfigure(t0, cfgOf(ModeObserve)); err != nil {
			t.Fatal(err)
		}
		if err := e.Reconfigure(t0, one(0.001, 5)); err != nil {
			t.Fatal(err)
		}
		if got := drain(e, t0); got != 0 {
			t.Fatalf("admits = %d, want 0", got)
		}
	})

	t.Run("renaming a pool keeps its account state", func(t *testing.T) {
		e := newEval(t, t0, one(0.001, 5))
		drain(e, t0)
		renamed := cfgOf(ModeEnforce, pool("renamed", "acct", 0.001, 5, 100, 0, "haiku", "sonnet", "opus"))
		if err := e.Reconfigure(t0, renamed); err != nil {
			t.Fatal(err)
		}
		if got := drain(e, t0); got != 0 {
			t.Fatalf("admits = %d, want 0", got)
		}
	})

	t.Run("disabling then enforcing again does not refill", func(t *testing.T) {
		e := newEval(t, t0, one(0.001, 5))
		drain(e, t0)
		if err := e.Reconfigure(t0, cfgOf(ModeDisabled)); err != nil {
			t.Fatal(err)
		}
		if err := e.Reconfigure(t0, one(0.001, 5)); err != nil {
			t.Fatal(err)
		}
		if got := drain(e, t0); got != 0 {
			t.Fatalf("admits = %d, want 0", got)
		}
	})

	t.Run("a never-seen account starts full", func(t *testing.T) {
		e := newEval(t, t0, cfgOf(ModeObserve, pool("a", "a1", 0.001, 2, 100, 0, "opus")))
		cfg := cfgOf(ModeEnforce,
			pool("a", "a1", 0.001, 2, 100, 0, "opus"),
			pool("b", "b1", 0.001, 3, 100, 0, "haiku", "sonnet"))
		if err := e.Reconfigure(t0, cfg); err != nil {
			t.Fatal(err)
		}
		n := 0
		for i := 0; i < 10; i++ {
			if eval(t, e, t0, "haiku", ResearchWrite).Outcome == OutcomeAdmit {
				n++
			}
		}
		if n != 3 {
			t.Fatalf("new account admits = %d, want its burst of 3", n)
		}
	})

	t.Run("invalid config is rejected and state untouched", func(t *testing.T) {
		e := newEval(t, t0, one(0.001, 2))
		eval(t, e, t0, "opus", ResearchWrite)
		bad := []Config{
			cfgOf(ModeEnforce, allModelsPool(-1, 5, 5, 0)),
			cfgOf(ModeEnforce, allModelsPool(1, -3, 5, 0)),
			cfgOf(ModeEnforce, allModelsPool(1, 5, 2, 3)),
			cfgOf(ModeEnforce, pool("p", "a", 1, 1, 1, 0, "opus")),
			cfgOf(Mode("bogus")),
		}
		for i, cfg := range bad {
			if err := e.Reconfigure(t0, cfg); err == nil {
				t.Fatalf("bad config %d accepted", i)
			}
		}
		if got := drain(e, t0); got != 1 {
			t.Fatalf("state changed by rejected reconfigure: admits = %d, want 1", got)
		}
	})

	t.Run("active dispatches survive removal and re-adding", func(t *testing.T) {
		e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(1, 100, 1, 0)))
		first := eval(t, e, t0, "opus", ResearchWrite)
		if err := e.Reconfigure(t0, cfgOf(ModeObserve)); err != nil {
			t.Fatal(err)
		}
		if err := e.Reconfigure(t0, cfgOf(ModeEnforce, allModelsPool(1, 100, 1, 0))); err != nil {
			t.Fatal(err)
		}
		if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeRetry {
			t.Fatalf("in-flight dispatch forgotten: %+v", d)
		}
		e.Release(first.Ticket)
		if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeAdmit {
			t.Fatalf("release after reconfigure failed: %+v", d)
		}
	})

	t.Run("release is attributed to the admitting pool after a model moves", func(t *testing.T) {
		twoPools := func(haikuPool string) Config {
			a := pool("a", "acct-a", 100, 100, 1, 0, "sonnet")
			b := pool("b", "acct-b", 100, 100, 1, 0, "opus")
			if haikuPool == "a" {
				a.Models = append(a.Models, "haiku")
			} else {
				b.Models = append(b.Models, "haiku")
			}
			return cfgOf(ModeEnforce, a, b)
		}
		e := newEval(t, t0, twoPools("a"))
		haiku := eval(t, e, t0, "haiku", ResearchWrite)
		opus := eval(t, e, t0, "opus", ResearchWrite)
		if haiku.Outcome != OutcomeAdmit || opus.Outcome != OutcomeAdmit {
			t.Fatal("setup")
		}
		if err := e.Reconfigure(t0, twoPools("b")); err != nil {
			t.Fatal(err)
		}
		e.Release(haiku.Ticket) // frees pool a, not pool b
		if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeRetry {
			t.Fatalf("pool b ceiling exceeded after cross-pool release: %+v", d)
		}
		if d := eval(t, e, t0, "sonnet", ResearchWrite); d.Outcome != OutcomeAdmit {
			t.Fatalf("pool a slot not freed: %+v", d)
		}
	})

	t.Run("lowering the limit below active blocks without interrupting", func(t *testing.T) {
		e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(100, 100, 3, 0)))
		var tickets []*Ticket
		for i := 0; i < 3; i++ {
			tickets = append(tickets, eval(t, e, t0, "opus", ResearchWrite).Ticket)
		}
		if err := e.Reconfigure(t0, cfgOf(ModeEnforce, allModelsPool(100, 100, 1, 0))); err != nil {
			t.Fatal(err)
		}
		for i, tk := range tickets {
			if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeRetry {
				t.Fatalf("after %d releases still over limit: %+v", i, d)
			}
			e.Release(tk)
		}
		if d := eval(t, e, t0, "opus", ResearchWrite); d.Outcome != OutcomeAdmit {
			t.Fatalf("not admitted once drained: %+v", d)
		}
	})
}

func TestEvaluatorDoesNotAliasCallerConfig(t *testing.T) {
	cfg := cfgOf(ModeEnforce, allModelsPool(0.001, 1, 5, 0))
	e := newEval(t, t0, cfg)
	cfg.Pools[0].Models[0] = "mutated"
	cfg.Pools[0].BurstCapacity = 99
	if d := eval(t, e, t0, "haiku", ResearchWrite); d.Outcome != OutcomeAdmit {
		t.Fatalf("haiku should still be mapped: %+v", d)
	}
	if d := eval(t, e, t0, "haiku", ResearchWrite); d.Outcome != OutcomeDefer {
		t.Fatalf("mutating caller config changed evaluator: %+v", d)
	}
}

func TestConcurrentEvaluateNeverOverspends(t *testing.T) {
	const burst = 7
	e := newEval(t, t0, cfgOf(ModeEnforce, allModelsPool(0.0001, burst, 100, 0)))
	var wg sync.WaitGroup
	var mu sync.Mutex
	admits := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := e.Evaluate(t0, "opus", ResearchWrite)
			if err != nil {
				t.Error(err)
				return
			}
			if d.Outcome == OutcomeAdmit {
				mu.Lock()
				admits++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if admits != burst {
		t.Fatalf("admits = %d, want %d", admits, burst)
	}
}
