package evalrun

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/boldfield/odonian/internal/evalcohort"
	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

const maxMessage = 1024

// attemptRun is one claimed attempt from staging to finalization. Its result
// starts as an "aborted" one and is replaced only when the attempt really
// finishes, so a panic or an early return still leaves something truthful to
// finalize.
type attemptRun struct {
	r       *Runner
	job     Job
	cand    store.EvaluationCandidate
	runtime string
	att     store.EvaluationAttempt

	staged    *evalcohort.Staged
	res       store.EvaluationAttemptResult
	stopLease func() (renewals int, err error)
	rec       AttemptRecord
	committed bool
}

// run takes the claimed attempt to a recorded outcome. The lease is renewed
// from the claim until the candidate process has exited; the result is then
// written once, with a context that survives the caller's cancellation.
func (a *attemptRun) run(ctx context.Context) AttemptRecord {
	a.rec = AttemptRecord{AttemptID: a.att.ID, Sequence: a.att.SequenceNumber}
	a.res = a.result(store.EvalExitUnknown, "evaluation runner stopped before the attempt finished", nil)
	defer a.close(ctx)
	a.execute(ctx)
	a.commit(ctx)
	return a.rec
}

// close runs on every exit, including a panic: it finalizes an attempt that
// was not finalized yet and removes the staged workspace.
func (a *attemptRun) close(ctx context.Context) {
	if !a.committed {
		a.commit(ctx)
	}
	if a.staged != nil {
		a.rec.CleanupErr = a.staged.Cleanup()
	}
}

func (a *attemptRun) result(exit store.EvaluationExitClass, msg string, detail *store.EvaluationAttemptDetail) store.EvaluationAttemptResult {
	if detail == nil {
		detail = &store.EvaluationAttemptDetail{CandidateDigest: a.cand.Digest()}
	}
	m := truncate(msg)
	return store.EvaluationAttemptResult{
		AttemptID: a.att.ID, FenceAttemptID: a.att.ID, ExitClass: exit, ErrorMessage: &m, Detail: detail,
	}
}

func (a *attemptRun) abort(exit store.EvaluationExitClass, format string, args ...any) {
	a.res = a.result(exit, fmt.Sprintf(format, args...), nil)
}

func (a *attemptRun) execute(ctx context.Context) {
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	a.stopLease = a.r.keepLease(ctx, a.att.ID, cancel)

	if !runIDRE.MatchString(a.att.ID) {
		a.abort(store.EvalExitUnknown, "attempt id %q is not usable as a run id", a.att.ID)
		return
	}
	staged, err := a.r.cfg.Stager.Stage(runCtx, evalcohort.StageRequest{
		CampaignID: a.job.CampaignID, SampleID: a.job.SampleID, CandidateID: a.job.CandidateID,
		RunID: a.att.ID, ToolAccess: a.r.cfg.ToolAccess,
	})
	var unavailable *evalcohort.UnavailableError
	switch {
	case errors.As(err, &unavailable):
		a.abort(store.EvalExitUnavailableSnapshot, "%v", err)
		return
	case runCtx.Err() != nil:
		a.abort(store.EvalExitCancelled, "cancelled while staging: %v", context.Cause(runCtx))
		return
	case err != nil:
		a.abort(store.EvalExitUnknown, "could not stage the workspace: %v", err)
		return
	}
	a.staged = staged
	if err := staged.Validate(); err != nil {
		a.abort(store.EvalExitUnavailableSnapshot, "staged workspace no longer matches the frozen snapshot: %v", err)
		return
	}

	req := staged.Request
	req.BlindedPrompt += "\n\n" + evidenceStandard
	out, err := a.r.cfg.Executor.Execute(runCtx, a.runtime, req)
	detail := &store.EvaluationAttemptDetail{
		CandidateDigest: a.cand.Digest(), PromptDigest: evaluation.PromptDigest(req.BlindedPrompt),
		StandardDigest: evaluation.PromptDigest(evidenceStandard), Launched: out.Launched,
	}
	if out.Launched && out.ExitCode >= 0 {
		code := out.ExitCode
		detail.ExitCode = &code
	}
	if err != nil {
		a.res = a.result(store.EvalExitUnknown, "host could not run the candidate: "+err.Error(), detail)
		return
	}
	a.res = a.finalization(runCtx, out, detail)
}

// finalization maps a validated response onto the store's distinct exit
// classes. Findings are carried only by a completed, validated review; a clean
// exit, an empty result file or a missing one is never read as completion.
func (a *attemptRun) finalization(runCtx context.Context, out evaluation.Result, detail *store.EvaluationAttemptDetail) store.EvaluationAttemptResult {
	resp := out.Response
	status, class, msg := resp.Status, resp.ErrorClass, resp.ErrorMessage
	exit := exitClass(resp, runCtx.Err() != nil)
	if exit == store.EvalExitCompleted && (resp.Validate() != nil || !resp.ReviewCompleted) {
		exit, status, class = store.EvalExitInvalidOutput, evaluation.StatusFailed, evaluation.ErrClassOutputMalformed
		msg = "response does not carry a validated, completed review"
	}
	if out.Launched {
		// A host-made response (malformed, missing, timeout, crash) carries the
		// all-unknown identity: the runtime never reported itself, so there is
		// no effective identity to record or compare to the configured one.
		if id := resp.Identity; id.Digest() != evaluation.UnknownIdentity().Digest() {
			detail.EffectiveIdentity, detail.EffectiveDigest = &id, id.Digest()
		}
		detail.Usage = resp.Usage
	}
	d := int(resp.Timing.FinishedAt.Sub(resp.Timing.StartedAt) / time.Millisecond)
	if cause := context.Cause(runCtx); exit != store.EvalExitCompleted && errors.Is(cause, errLeaseLost) {
		exit, msg = store.EvalExitCancelled, cause.Error()
	}
	res := store.EvaluationAttemptResult{
		AttemptID: a.att.ID, FenceAttemptID: a.att.ID, ExitClass: exit,
		Status: &status, DurationMs: &d, Detail: detail,
	}
	if class != "" {
		res.ErrorClass = &class
	}
	if msg != "" {
		m := truncate(msg)
		res.ErrorMessage = &m
	}
	if exit == store.EvalExitCompleted {
		res.Findings = resp.Findings
	}
	return res
}

func exitClass(resp evaluation.CandidateResponse, hostCancelled bool) store.EvaluationExitClass {
	switch resp.Status {
	case evaluation.StatusCompleted:
		return store.EvalExitCompleted
	case evaluation.StatusIncomplete:
		if resp.ErrorClass == evaluation.ErrClassSourceUnavailable {
			return store.EvalExitUnavailableSource
		}
		return store.EvalExitIncompleteOutput
	case evaluation.StatusInterrupted:
		switch {
		case resp.ErrorClass == evaluation.ErrClassTimeout:
			return store.EvalExitTimeout
		case hostCancelled:
			return store.EvalExitCancelled
		}
		return store.EvalExitUnknown
	case evaluation.StatusFailed:
		switch resp.ErrorClass {
		case evaluation.ErrClassOutputMalformed, evaluation.ErrClassOutputMissing:
			return store.EvalExitInvalidOutput
		case evaluation.ErrClassSourceUnavailable:
			return store.EvalExitUnavailableSource
		}
	}
	return store.EvalExitFailed
}

// commit stops lease renewal (so no renewal races the result), then records
// the result. It is the only place an attempt is finalized.
func (a *attemptRun) commit(ctx context.Context) {
	a.committed = true
	if a.stopLease != nil {
		a.rec.Renewals, a.rec.LeaseErr = a.stopLease()
		a.stopLease = nil
	}
	fctx := context.WithoutCancel(ctx)
	degraded := false
	for try := 0; ; try++ {
		err := a.callFinalize(fctx)
		switch {
		case err == nil:
			a.rec.Recorded, a.rec.Exit = true, a.res.ExitClass
		case errors.Is(err, store.ErrEvaluationAttemptExpired):
			// The store records an overdue attempt as lease_expired.
			a.rec.Recorded, a.rec.Exit, a.rec.FinalizeErr = true, store.EvalExitLeaseExpired, err
		case errors.Is(err, store.ErrEvaluationInvalidInput) && !degraded:
			// The store would not take the result; keep the slot from leaking by
			// recording that the result could not be recorded.
			degraded = true
			a.res = a.result(store.EvalExitUnknown, "result could not be recorded: "+err.Error(), nil)
			continue
		case errors.Is(err, store.ErrEvaluationFenceMismatch), errors.Is(err, store.ErrEvaluationAttemptFinalized),
			errors.Is(err, store.ErrEvaluationAttemptNotFound), errors.Is(err, store.ErrEvaluationInvalidInput),
			try >= a.r.cfg.FinalizeRetries:
			a.rec.FinalizeErr = err
		default:
			if a.r.cfg.Sleep(fctx, a.r.cfg.RetryBackoff<<try) == nil {
				continue
			}
			a.rec.FinalizeErr = err
		}
		break
	}
	a.rec.Result = a.res
}

func (a *attemptRun) callFinalize(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, a.r.cfg.CallTimeout)
	defer cancel()
	return a.r.cfg.Backend.FinalizeEvaluationAttempt(ctx, a.res)
}

// keepLease renews the attempt's lease on a ticker until the returned stop
// function is called. The first renewal failure ends renewal and cancels the
// run, so a lost lease stops the candidate instead of letting it run on
// unfenced; stop reports that failure rather than swallowing it.
func (r *Runner) keepLease(ctx context.Context, attemptID string, cancel context.CancelCauseFunc) func() (int, error) {
	stop, done := make(chan struct{}), make(chan struct{})
	var renewals int
	var leaseErr error
	go func() {
		defer close(done)
		tick := time.NewTicker(r.cfg.RenewEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			cctx, c := context.WithTimeout(context.WithoutCancel(ctx), r.cfg.CallTimeout)
			err := r.cfg.Backend.RenewEvaluationAttempt(cctx, attemptID, r.cfg.Backend.Now().Add(r.cfg.LeaseTTL))
			c()
			if err != nil {
				leaseErr = fmt.Errorf("%w: %v", errLeaseLost, err)
				cancel(leaseErr)
				return
			}
			renewals++
		}
	}()
	return func() (int, error) {
		close(stop)
		<-done
		return renewals, leaseErr
	}
}

func truncate(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	cut := maxMessage
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut]
}
