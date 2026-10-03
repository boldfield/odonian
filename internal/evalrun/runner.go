// Package evalrun runs bounded comparison-reviewer evaluations. It drives the
// model-agnostic adapter contract (internal/evaluation), the evaluation job
// lifecycle in the store, and the frozen-workspace stager (internal/evalcohort)
// without knowing which model or CLI sits behind a candidate: a candidate is a
// registered runtime found by its identity digest, and everything the runner
// records is taken from the candidate's own identity and the adapter's own
// response. Adding a candidate is a registration plus an adapter; nothing here
// changes.
//
// The host does all of the work that matters. It claims a bounded attempt
// before anything is launched, keeps the lease renewed while the candidate
// runs, waits for the process to exit, validates what it wrote, and only then
// finalizes the attempt in the evaluation tables. Nothing it does touches the
// production board: it has no way to submit, comment, vote or create tasks.
package evalrun

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/boldfield/odonian/internal/evalcohort"
	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

//go:embed evidence_standard.md
var evidenceStandard string

// EvidenceStandard is the text appended to every candidate's blinded prompt:
// the research evidence-verification bar, stated without any board commands.
func EvidenceStandard() string { return evidenceStandard }

var (
	// ErrNotRegistered means no registered runtime carries a candidate's digest.
	ErrNotRegistered = errors.New("no registered runtime for the candidate's identity digest")
	// ErrAmbiguousRuntime means more than one registered runtime does.
	ErrAmbiguousRuntime = errors.New("more than one registered runtime for the candidate's identity digest")

	errLeaseLost = errors.New("lease renewal failed")
	runIDRE      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

// Backend is the part of the store the runner uses. store.Store satisfies it.
// It has no production-board verbs, so the runner cannot touch the board.
type Backend interface {
	Now() time.Time
	GetEvaluationCampaign(ctx context.Context, campaignID string) (store.EvaluationCampaign, error)
	GetEvaluationCandidate(ctx context.Context, candidateID string) (store.EvaluationCandidate, error)
	ListEvaluationCandidates(ctx context.Context, campaignID string) ([]store.EvaluationCandidate, error)
	ListEvaluationSamples(ctx context.Context, campaignID string) ([]store.EvaluationSample, error)
	ClaimEvaluationJob(ctx context.Context, req store.EvaluationJobClaim) (store.EvaluationJobClaimResult, error)
	RenewEvaluationAttempt(ctx context.Context, attemptID string, expiresAt time.Time) error
	FinalizeEvaluationAttempt(ctx context.Context, res store.EvaluationAttemptResult) error
}

// Stager stages one candidate's frozen workspace. evalcohort.Stager satisfies it.
type Stager interface {
	Stage(ctx context.Context, req evalcohort.StageRequest) (*evalcohort.Staged, error)
}

// Executor runs a request through a registered runtime. *evaluation.Pipeline
// satisfies it and is the only implementation outside tests.
type Executor interface {
	Execute(ctx context.Context, runtime string, req evaluation.CandidateRequest) (evaluation.Result, error)
}

var (
	_ Backend  = store.Store(nil)
	_ Stager   = evalcohort.Stager{}
	_ Executor = (*evaluation.Pipeline)(nil)
)

// Config assembles a Runner. Zero bounds take the defaults noted.
type Config struct {
	Backend  Backend
	Registry *evaluation.Registry
	Stager   Stager
	Executor Executor

	// ToolAccess is what every review needs from its runtime. The zero value
	// requires source retrieval, because the evidence standard makes the
	// candidate open every source itself.
	ToolAccess evaluation.ToolAccessRequirements

	LeaseTTL        time.Duration // lease of each attempt; default 10m
	RenewEvery      time.Duration // renewal cadence; default LeaseTTL/3
	CallTimeout     time.Duration // bound on each renew and finalize call; default 30s
	MaxAttempts     int           // attempts per run, retries included; default 3
	RetryBackoff    time.Duration // first retry delay, doubling each retry; default 5s
	MaxDeferrals    int           // refusals per run after which it is deferred (so N-1 waits); default 3
	MaxDeferWait    time.Duration // longest single wait for a hint; default 15m
	FinalizeRetries int           // extra finalize tries on a transient error; default 3

	// Sleep waits d or until ctx ends; tests replace it. NewRequestID makes the
	// request ID of each claim; both default to real implementations.
	Sleep        func(ctx context.Context, d time.Duration) error
	NewRequestID func() string
}

// Runner runs evaluation jobs. It is safe for concurrent use.
type Runner struct{ cfg Config }

// New validates cfg and returns a Runner.
func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Backend == nil, cfg.Registry == nil, cfg.Stager == nil, cfg.Executor == nil:
		return nil, errors.New("evalrun: Backend, Registry, Stager and Executor are required")
	case cfg.LeaseTTL < 0, cfg.RenewEvery < 0, cfg.CallTimeout < 0, cfg.MaxAttempts < 0, cfg.RetryBackoff < 0,
		cfg.MaxDeferrals < 0, cfg.MaxDeferWait < 0, cfg.FinalizeRetries < 0:
		return nil, errors.New("evalrun: bounds must not be negative")
	}
	def := func(v *time.Duration, d time.Duration) {
		if *v == 0 {
			*v = d
		}
	}
	defInt := func(v *int, d int) {
		if *v == 0 {
			*v = d
		}
	}
	def(&cfg.LeaseTTL, 10*time.Minute)
	def(&cfg.RenewEvery, cfg.LeaseTTL/3)
	def(&cfg.CallTimeout, 30*time.Second)
	def(&cfg.RetryBackoff, 5*time.Second)
	def(&cfg.MaxDeferWait, 15*time.Minute)
	defInt(&cfg.MaxAttempts, 3)
	defInt(&cfg.MaxDeferrals, 3)
	defInt(&cfg.FinalizeRetries, 3)
	if !cfg.ToolAccess.RequireSourceRetrieval && !cfg.ToolAccess.RequirePDFAccess && len(cfg.ToolAccess.Tools) == 0 {
		cfg.ToolAccess.RequireSourceRetrieval = true
	}
	if err := cfg.ToolAccess.Validate(); err != nil {
		return nil, err
	}
	if cfg.RenewEvery >= cfg.LeaseTTL {
		return nil, errors.New("evalrun: RenewEvery must be shorter than LeaseTTL")
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.NewRequestID == nil {
		cfg.NewRequestID = store.GenerateID
	}
	return &Runner{cfg: cfg}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Job names one sample on one candidate.
type Job struct {
	CampaignID  string
	SampleID    string
	CandidateID string
}

// Run evaluates one sample with one candidate within the runner's bounds and
// reports the outcome. It never launches before an attempt is claimed, never
// retries without bound, and finalizes every attempt it claims. A candidate is
// always run and recorded as its own configured version.
func (r *Runner) Run(ctx context.Context, job Job) Report {
	rep := Report{CampaignID: job.CampaignID, SampleID: job.SampleID, CandidateID: job.CandidateID}
	cand, err := r.cfg.Backend.GetEvaluationCandidate(ctx, job.CandidateID)
	if err == nil && cand.CampaignID != job.CampaignID {
		err = fmt.Errorf("candidate %s belongs to another campaign", cand.ID)
	}
	if err != nil {
		return rep.stop(KindHostError, err)
	}
	rep.CandidateDigest, rep.Declared = cand.Digest(), cand.Config.Identity()
	rt, err := r.runtimeFor(cand)
	if err != nil {
		return rep.stop(KindNotRegistered, err)
	}
	rep.Runtime = rt

	for n := 1; ; n++ {
		att, kind, err := r.claim(ctx, job, &rep)
		if kind != "" {
			return rep.stop(kind, err)
		}
		a := &attemptRun{r: r, job: job, cand: cand, runtime: rt, att: att}
		rec := a.run(ctx)
		rep.Attempts = append(rep.Attempts, rec)
		switch {
		case !rec.Recorded:
			return rep.stop(KindUnrecorded, rec.FinalizeErr)
		case ctx.Err() != nil || !retryable(rec):
			rep.Kind = Kind(rec.Exit)
			return rep
		case n >= r.cfg.MaxAttempts:
			rep.Kind, rep.RetriesExhausted = Kind(rec.Exit), true
			return rep
		}
		if err := r.cfg.Sleep(ctx, r.cfg.RetryBackoff<<(n-1)); err != nil {
			rep.Kind = Kind(rec.Exit)
			return rep
		}
	}
}

func (rep Report) stop(kind Kind, err error) Report {
	rep.Kind, rep.Err = kind, err
	return rep
}

// runtimeFor finds the registered runtime that carries the candidate version.
// Matching is by identity digest, so the stored candidate and the registered
// adapter cannot silently drift apart.
func (r *Runner) runtimeFor(cand store.EvaluationCandidate) (string, error) {
	var found []string
	for _, name := range r.cfg.Registry.Names() {
		rt, err := r.cfg.Registry.Get(name)
		if err == nil && rt.Candidate.Digest() == cand.Digest() {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w: candidate %s", ErrNotRegistered, cand.ID)
	case 1:
		return found[0], nil
	}
	sort.Strings(found)
	return "", fmt.Errorf("%w: %v", ErrAmbiguousRuntime, found)
}

// claim obtains an attempt before anything is launched. A refusal by the pool
// is waited out as the store's hint directs, within MaxDeferrals and
// MaxDeferWait; kind is non-empty when the run must stop instead.
func (r *Runner) claim(ctx context.Context, job Job, rep *Report) (att store.EvaluationAttempt, kind Kind, err error) {
	for {
		res, err := r.cfg.Backend.ClaimEvaluationJob(ctx, store.EvaluationJobClaim{
			SampleID: job.SampleID, CandidateID: job.CandidateID,
			RequestID: r.cfg.NewRequestID(), LeaseExpires: r.cfg.LeaseTTL,
		})
		var denied *store.AdmissionDeniedError
		switch {
		case err == nil:
			return res.Attempt, "", nil
		case ctx.Err() != nil:
			return att, Kind(store.EvalExitCancelled), ctx.Err()
		case errors.As(err, &denied):
			rep.Deferrals++
			rep.NotBefore, rep.RetryAfter = denied.NotBefore, denied.RetryAfter
			wait := denied.RetryAfter
			if !denied.NotBefore.IsZero() {
				wait = denied.NotBefore.Sub(r.cfg.Backend.Now())
			}
			if rep.Deferrals >= r.cfg.MaxDeferrals || wait > r.cfg.MaxDeferWait {
				return att, KindDeferred, err
			}
			if err := r.cfg.Sleep(ctx, max(wait, time.Millisecond)); err != nil {
				return att, Kind(store.EvalExitCancelled), err
			}
		case errors.Is(err, store.ErrEvaluationCapacityExhausted):
			return att, KindCapacityExhausted, err
		case errors.Is(err, store.ErrEvaluationCampaignPaused):
			return att, KindPaused, err
		case errors.Is(err, store.ErrEvaluationPoolNotConfigured):
			return att, KindPoolNotConfigured, err
		case errors.Is(err, store.ErrEvaluationAttemptLive):
			return att, KindAttemptLive, err
		default:
			return att, KindHostError, err
		}
	}
}

// retryable reports whether another attempt could change the outcome. A
// completed review, a host cancellation, a changed frozen input, a host fault
// and a deterministic configuration problem (missing capability or credential)
// are final; failed runs, timeouts, bad or truncated output, unreachable
// sources and expired leases are worth a bounded retry. A run cancelled
// because its renewal failed is final too: the store is the thing in trouble.
func retryable(rec AttemptRecord) bool {
	switch rec.Exit {
	case store.EvalExitCompleted, store.EvalExitCancelled, store.EvalExitUnavailableSnapshot, store.EvalExitUnknown:
		return false
	}
	if c := rec.Result.ErrorClass; c != nil && (*c == evaluation.ErrClassCapabilityMissing || *c == evaluation.ErrClassAuthMissing) {
		return false
	}
	return true
}
