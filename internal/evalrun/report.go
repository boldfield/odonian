package evalrun

import (
	"slices"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

// Kind is the distinct outcome of running one sample against one candidate.
// When an attempt was recorded, the kind is that attempt's exit class
// (completed, failed, timeout, unavailable_source, ...); the kinds below are
// the outcomes that happen before any attempt exists, or that no attempt can
// record.
type Kind string

const (
	// KindDeferred: the candidate's pool refused every start within the
	// runner's bounds. Nothing was launched and no attempt was consumed; the
	// report carries the store's retry hint.
	KindDeferred Kind = "deferred"
	// KindPaused: the campaign is paused.
	KindPaused Kind = "paused"
	// KindCapacityExhausted: the candidate or the campaign has spent its
	// finite attempt cap.
	KindCapacityExhausted Kind = "capacity_exhausted"
	// KindPoolNotConfigured: the candidate's account pool does not exist.
	KindPoolNotConfigured Kind = "pool_not_configured"
	// KindAttemptLive: an earlier attempt for this sample and candidate still
	// holds its lease.
	KindAttemptLive Kind = "attempt_live"
	// KindNotRegistered: no registered runtime has the candidate's identity
	// digest, so nothing could be launched.
	KindNotRegistered Kind = "not_registered"
	// KindUnrecorded: the attempt ran but the store would not record its
	// result; the outcome is in the attempt record, not in the store.
	KindUnrecorded Kind = "unrecorded"
	// KindHostError: the host could not proceed (store or configuration
	// fault) before any attempt was claimed.
	KindHostError Kind = "host_error"
)

// AttemptRecord is the host's account of one claimed attempt. Result is
// exactly what was written to the store (or offered to it).
type AttemptRecord struct {
	AttemptID string
	Sequence  int
	Result    store.EvaluationAttemptResult
	// Exit is the exit class the store holds. It differs from Result.ExitClass
	// only when the lease had run out and the store recorded lease_expired.
	Exit store.EvaluationExitClass
	// Recorded reports whether the store holds a terminal record of the attempt.
	Recorded    bool
	FinalizeErr error
	// LeaseErr is the first lease renewal failure, which also cancelled the run.
	LeaseErr   error
	CleanupErr error
	Renewals   int
}

// Report is the outcome of one sample on one candidate version. It is built
// from, and never merged with, the reports of other candidate versions.
type Report struct {
	CampaignID  string
	SampleID    string
	CandidateID string
	// CandidateDigest and Declared are the candidate version's identity as
	// configured; Runtime is the registered runtime that carries it.
	CandidateDigest string
	Declared        evaluation.CandidateIdentity
	Runtime         string

	Kind     Kind
	Attempts []AttemptRecord
	// RetriesExhausted is set when the last attempt failed in a way a retry
	// could fix but the run's attempt budget was spent.
	RetriesExhausted bool

	Deferrals  int
	NotBefore  time.Time     // last deferral hint, when the pool named a time
	RetryAfter time.Duration // last deferral hint, when it named a delay
	Err        error
}

// Last returns the final attempt record, if any attempt was claimed.
func (r Report) Last() (AttemptRecord, bool) {
	if len(r.Attempts) == 0 {
		return AttemptRecord{}, false
	}
	return r.Attempts[len(r.Attempts)-1], true
}

// CandidateSummary counts outcomes for exactly one candidate version.
type CandidateSummary struct {
	CandidateDigest string
	Declared        evaluation.CandidateIdentity
	Runtime         string
	Samples         int
	Attempts        int
	Kinds           map[Kind]int
	// EffectiveDigests lists, in first-seen order, the distinct identities the
	// runtime reported about itself. More than one entry, or one that differs
	// from CandidateDigest, means the runtime did not behave as configured.
	EffectiveDigests []string
}

// GroupByCandidate summarizes reports per candidate version, in first-seen
// order. Candidates are keyed by identity digest, so two versions of the same
// adapter (or two adapters) are never combined, and usage is left in the
// per-attempt records in the provider's own units rather than totalled.
func GroupByCandidate(reports []Report) []CandidateSummary {
	var out []CandidateSummary
	index := map[string]int{}
	for _, rep := range reports {
		i, ok := index[rep.CandidateDigest]
		if !ok {
			i = len(out)
			index[rep.CandidateDigest] = i
			out = append(out, CandidateSummary{
				CandidateDigest: rep.CandidateDigest, Declared: rep.Declared, Runtime: rep.Runtime,
				Kinds: map[Kind]int{},
			})
		}
		s := &out[i]
		s.Samples++
		s.Kinds[rep.Kind]++
		s.Attempts += len(rep.Attempts)
		for _, a := range rep.Attempts {
			d := a.Result.Detail
			if d == nil || d.EffectiveDigest == "" || slices.Contains(s.EffectiveDigests, d.EffectiveDigest) {
				continue
			}
			s.EffectiveDigests = append(s.EffectiveDigests, d.EffectiveDigest)
		}
	}
	return out
}
