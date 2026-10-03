// Package policy is the pure research admission policy: given explicit
// account-pool configuration and a caller-supplied server time it decides
// whether one research LLM start is admitted, deferred until a time, or must
// wait for an active dispatch to finish. It reads no clock, launches nothing
// and touches no tasks; wiring it into claim or dispatch is separate work.
package policy

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Mode selects how decisions are applied.
type Mode string

const (
	// ModeDisabled admits everything and tracks nothing.
	ModeDisabled Mode = "disabled"
	// ModeObserve always admits and reports the decision enforcement would
	// have made in Decision.Shadow.
	ModeObserve Mode = "observe"
	// ModeEnforce applies decisions.
	ModeEnforce Mode = "enforce"
)

func (m Mode) valid() bool {
	return m == ModeDisabled || m == ModeObserve || m == ModeEnforce
}

// WorkClass is the kind of work asking to start. Only research LLM work is
// paced; build, design and non-LLM merge work is always admitted untouched.
type WorkClass string

const (
	ResearchWrite        WorkClass = "research_write"
	ResearchReview       WorkClass = "research_review"
	ResearchRework       WorkClass = "research_rework"
	ResearchAdjudication WorkClass = "research_adjudication"
	BuildWork            WorkClass = "build"
	DesignWork           WorkClass = "design"
	MergeWork            WorkClass = "merge"
)

func (c WorkClass) known() bool {
	switch c {
	case ResearchWrite, ResearchReview, ResearchRework, ResearchAdjudication, BuildWork, DesignWork, MergeWork:
		return true
	}
	return false
}

// Paced reports whether the class consumes research allowance.
func (c WorkClass) Paced() bool {
	switch c {
	case ResearchWrite, ResearchReview, ResearchRework, ResearchAdjudication:
		return true
	}
	return false
}

// Completion reports whether the class is completion work (review, rework,
// adjudication), which may use reserved capacity. ResearchWrite is first-pass.
func (c WorkClass) Completion() bool {
	return c == ResearchReview || c == ResearchRework || c == ResearchAdjudication
}

// Outcome is the kind of answer to an admission request.
type Outcome string

const (
	// OutcomeAdmit: start now; one start was debited.
	OutcomeAdmit Outcome = "admit"
	// OutcomeDefer: time alone allows progress; retry at NotBefore.
	OutcomeDefer Outcome = "defer"
	// OutcomeRetry: an active dispatch must finish first; there is no finish
	// time, so retry after RetryAfter.
	OutcomeRetry Outcome = "retry"
	// OutcomeUnmapped: the model has no pool. Config validation makes this
	// impossible under enforcement for allowed models; it is reported
	// separately so it is never mistaken for a timed deferral.
	OutcomeUnmapped Outcome = "unmapped"
)

// Reason refines OutcomeDefer and OutcomeRetry.
type Reason string

const (
	ReasonRate             Reason = "rate"
	ReasonConcurrency      Reason = "concurrency"
	ReasonReservedCapacity Reason = "reserved_capacity"
)

// Verdict is one policy decision.
type Verdict struct {
	Outcome    Outcome
	Reason     Reason        // set for OutcomeDefer and OutcomeRetry
	NotBefore  time.Time     // set for OutcomeDefer
	RetryAfter time.Duration // set for OutcomeRetry
}

// Decision is the result of Evaluate. Verdict is what the caller must do.
// Shadow is what enforcement would decide; it equals Verdict except in
// ModeObserve, where Verdict always admits.
type Decision struct {
	Verdict
	Shadow Verdict
	// Ticket is non-nil when an active dispatch is now tracked. The caller
	// must Release it exactly when the dispatch ends.
	Ticket *Ticket
}

// Ticket attributes one tracked dispatch to the account that admitted it, so
// a later Reconfigure cannot make the release hit a different pool.
type Ticket struct {
	owner      *Evaluator
	account    string
	completion bool
	released   bool
}

// tokenEpsilon absorbs float rounding so a deferral's NotBefore is honored.
const tokenEpsilon = 1e-9

type account struct {
	tokens           float64
	last             time.Time // high-water mark of every time seen; never moves backward
	rate             float64
	burst            float64
	active           int
	activeCompletion int
}

// settle credits refill for time elapsed past the high-water mark, capped at
// burst. A time at or before the mark credits nothing and leaves it alone.
func (a *account) settle(now time.Time) {
	if !now.After(a.last) {
		return
	}
	a.tokens = math.Min(a.burst, a.tokens+now.Sub(a.last).Seconds()*a.rate)
	a.last = now
}

// Evaluator holds pool allowance and active-dispatch state. It is safe for
// concurrent use.
type Evaluator struct {
	mu        sync.Mutex
	cfg       Config
	modelPool map[string]Pool
	accounts  map[string]*account // by account ID; kept after a pool is removed
}

// New validates cfg and returns an Evaluator whose pools start with a full
// burst as of now.
func New(now time.Time, cfg Config) (*Evaluator, error) {
	e := &Evaluator{accounts: make(map[string]*account)}
	if err := e.apply(now, cfg); err != nil {
		return nil, err
	}
	return e, nil
}

// Reconfigure atomically swaps in a new configuration, or returns an error
// and changes nothing. Existing accounts keep their allowance (settled up to
// now under the old rate, then clamped to the new burst), their high-water
// mark and their active dispatches; new rate/limits apply from now on. State
// of a removed account is retained so removing and re-adding a pool cannot
// refill it. An account never seen before starts with a full burst.
func (e *Evaluator) Reconfigure(now time.Time, cfg Config) error {
	return e.apply(now, cfg)
}

func (e *Evaluator) apply(now time.Time, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	cfg = cfg.clone()

	e.mu.Lock()
	defer e.mu.Unlock()

	modelPool := make(map[string]Pool)
	if cfg.Mode != ModeDisabled {
		for _, p := range cfg.Pools {
			for _, m := range p.Models {
				modelPool[m] = p
			}
			acct, ok := e.accounts[p.AccountID]
			if !ok {
				e.accounts[p.AccountID] = &account{
					tokens: float64(p.BurstCapacity),
					last:   now,
					rate:   p.StartRate,
					burst:  float64(p.BurstCapacity),
				}
				continue
			}
			acct.settle(now)
			acct.rate = p.StartRate
			acct.burst = float64(p.BurstCapacity)
			acct.tokens = math.Min(acct.tokens, acct.burst)
		}
	}
	e.cfg = cfg
	e.modelPool = modelPool
	return nil
}

// Evaluate decides one start request at server time now. Time earlier than
// any time already seen is treated as the latest time seen, so a clock
// rollback never refills allowance. An unknown WorkClass is an error.
func (e *Evaluator) Evaluate(now time.Time, model string, class WorkClass) (Decision, error) {
	if !class.known() {
		return Decision{}, fmt.Errorf("unknown work class %q", class)
	}
	admit := Verdict{Outcome: OutcomeAdmit}

	e.mu.Lock()
	defer e.mu.Unlock()

	if !class.Paced() || e.cfg.Mode == ModeDisabled {
		return Decision{Verdict: admit, Shadow: admit}, nil
	}

	pool, ok := e.modelPool[model]
	if !ok {
		v := Verdict{Outcome: OutcomeUnmapped}
		if e.cfg.Mode == ModeObserve {
			return Decision{Verdict: admit, Shadow: v}, nil
		}
		return Decision{Verdict: v, Shadow: v}, nil
	}

	acct := e.accounts[pool.AccountID]
	acct.settle(now)
	at := acct.last

	v := e.decide(pool, acct, class, at)
	if v.Outcome == OutcomeAdmit {
		acct.tokens = math.Max(0, acct.tokens-1)
	}

	d := Decision{Verdict: v, Shadow: v}
	if e.cfg.Mode == ModeObserve {
		d.Verdict = admit
	}
	// In observe mode the work really starts, so active is counted even when
	// the shadow verdict was a deferral/retry (which spent no token).
	if d.Verdict.Outcome == OutcomeAdmit {
		acct.active++
		if class.Completion() {
			acct.activeCompletion++
		}
		d.Ticket = &Ticket{owner: e, account: pool.AccountID, completion: class.Completion()}
	}
	return d, nil
}

// decide applies concurrency first (no token is spent while waiting on an
// active dispatch), then the start-rate bucket. It does not mutate the account.
func (e *Evaluator) decide(pool Pool, acct *account, class WorkClass, at time.Time) Verdict {
	limit := pool.ConcurrentDispatchLimit
	retry := func(r Reason) Verdict {
		return Verdict{Outcome: OutcomeRetry, Reason: r, RetryAfter: e.cfg.retryAfter()}
	}
	if acct.active >= limit {
		return retry(ReasonConcurrency)
	}
	if !class.Completion() && acct.active-acct.activeCompletion >= limit-pool.CompletionReserved {
		return retry(ReasonReservedCapacity)
	}

	if acct.tokens >= 1-tokenEpsilon {
		return Verdict{Outcome: OutcomeAdmit}
	}
	wait := (1 - acct.tokens) / acct.rate * float64(time.Second)
	d := time.Duration(math.MaxInt64 / 2)
	if wait < float64(d) {
		d = time.Duration(math.Ceil(wait))
	}
	return Verdict{Outcome: OutcomeDefer, Reason: ReasonRate, NotBefore: at.Add(d)}
}

// Release ends a tracked dispatch. It is idempotent, and a nil ticket or one
// from another Evaluator is ignored. It frees concurrency only; a spent start
// is never refunded.
func (e *Evaluator) Release(t *Ticket) {
	if t == nil || t.owner != e {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if t.released {
		return
	}
	t.released = true
	acct := e.accounts[t.account]
	acct.active--
	if t.completion {
		acct.activeCompletion--
	}
}
