package policy

import (
	"time"
)

// Mode controls whether policy enforcement is enabled.
type Mode string

const (
	ModeDisabled Mode = "disabled"
	ModeObserve  Mode = "observe"
	ModeEnforce  Mode = "enforce"
)

// DeferralReason explains why an admission was deferred.
type DeferralReason string

const (
	ReasonRateLimit     DeferralReason = "rate_limit"
	ReasonConcurrency   DeferralReason = "concurrency"
	ReasonCompletionCap DeferralReason = "completion_cap"
	ReasonUnmappedModel DeferralReason = "unmapped_model"
)

// AdmissionResult describes the outcome of an admission check.
type AdmissionResult struct {
	Admitted bool
	Reason   DeferralReason
	// NotBefore is set if the caller should retry after this time (rate limit case).
	NotBefore time.Time
	// RetryAfterMs is set if concurrency is the reason (bounded retry interval).
	RetryAfterMs int
}

// Pool represents a configured rate-limit pool for a set of models.
type Pool struct {
	// Name of the pool
	Name string
	// Models that use this pool
	Models map[string]bool
	// AccountID for billing/quota tracking
	AccountID string
	// StartRate is the sustained rate in starts per second
	StartRate float64
	// BurstCapacity is the maximum tokens in the bucket
	BurstCapacity int
	// ConcurrentDispatchLimit is the max concurrent active dispatches
	ConcurrentDispatchLimit int
	// CompletionReserved is the capacity reserved for completion work
	CompletionReserved int
}

// PolicyEvaluator holds the state and logic for evaluating admission.
type PolicyEvaluator struct {
	Mode  Mode
	Pools map[string]*Pool // pool name -> pool

	// In-memory state: model -> pool name
	modelToPool map[string]string

	// Token bucket state: pool name -> bucket state
	buckets map[string]*tokenBucket

	// Concurrency tracking: pool name -> active count
	concurrency map[string]int

	// Completion work tracking: pool name -> active count
	completionActive map[string]int

	// Clock is injected for testing
	clock func() time.Time
}

// tokenBucket tracks a rate-limit bucket with refill logic.
type tokenBucket struct {
	tokens         float64
	capacity       float64
	refillRate     float64
	lastRefill     time.Time
	lastRefillMono time.Time // monotonic high-water mark for clock rollback safety
}

// New creates a PolicyEvaluator with the given configuration.
func New(mode Mode, pools []*Pool) *PolicyEvaluator {
	pe := &PolicyEvaluator{
		Mode:             mode,
		Pools:            make(map[string]*Pool),
		modelToPool:      make(map[string]string),
		buckets:          make(map[string]*tokenBucket),
		concurrency:      make(map[string]int),
		completionActive: make(map[string]int),
		clock:            time.Now,
	}

	for _, pool := range pools {
		pe.Pools[pool.Name] = pool
		for model := range pool.Models {
			pe.modelToPool[model] = pool.Name
		}
		now := pe.clock()
		pe.buckets[pool.Name] = &tokenBucket{
			tokens:         float64(pool.BurstCapacity),
			capacity:       float64(pool.BurstCapacity),
			refillRate:     pool.StartRate,
			lastRefill:     now,
			lastRefillMono: now,
		}
	}

	return pe
}

// SetClock allows injection of a test clock.
func (pe *PolicyEvaluator) SetClock(clock func() time.Time) {
	pe.clock = clock
}

// CheckAdmission evaluates whether a new research task can be admitted.
// model is the model requested for the task.
// isCompletion indicates whether this is completion work (review/rework/adjudication).
// Returns an AdmissionResult with the decision and any deferral info.
func (pe *PolicyEvaluator) CheckAdmission(model string, isCompletion bool) AdmissionResult {
	if pe.Mode == ModeDisabled {
		return AdmissionResult{Admitted: true}
	}

	poolName, ok := pe.modelToPool[model]
	if !ok {
		// Unmapped model - only reject if enforcing
		if pe.Mode == ModeEnforce {
			return AdmissionResult{Admitted: false, Reason: ReasonUnmappedModel}
		}
		return AdmissionResult{Admitted: true}
	}

	pool := pe.Pools[poolName]

	// Check concurrency limit
	// For first-pass work: limited to (total - reserved) slots
	// For completion work: limited to total slots
	if isCompletion {
		// Completion work: check against total concurrent limit
		if pe.concurrency[poolName] >= pool.ConcurrentDispatchLimit {
			return AdmissionResult{
				Admitted:     false,
				Reason:       ReasonConcurrency,
				RetryAfterMs: 1000,
			}
		}
	} else {
		// First-pass work: limited to (total - reserved) slots
		reserved := pool.CompletionReserved
		firstPassLimit := pool.ConcurrentDispatchLimit - reserved
		activeFirstPass := pe.concurrency[poolName] - pe.completionActive[poolName]

		if activeFirstPass >= firstPassLimit {
			return AdmissionResult{
				Admitted:     false,
				Reason:       ReasonConcurrency,
				RetryAfterMs: 1000,
			}
		}
	}

	// Check rate limit using token bucket
	bucket := pe.buckets[poolName]
	now := pe.clock()
	bucket.refill(now)

	if bucket.tokens >= 1.0 {
		bucket.tokens -= 1.0
		bucket.lastRefillMono = now
		return AdmissionResult{Admitted: true}
	}

	// Rate limit exceeded - calculate when next token will be available
	// The bucket already has the refilled tokens, so we just need to know
	// when the current tokens + future refill will reach 1.0
	timeToNextToken := (1.0 - bucket.tokens) / bucket.refillRate
	if timeToNextToken < 0 {
		timeToNextToken = 0
	}

	return AdmissionResult{
		Admitted:  false,
		Reason:    ReasonRateLimit,
		NotBefore: now.Add(time.Duration(timeToNextToken * float64(time.Second))),
	}
}

// RecordAdmission records that an attempt was admitted (for concurrency tracking).
func (pe *PolicyEvaluator) RecordAdmission(model string, isCompletion bool) {
	poolName, ok := pe.modelToPool[model]
	if !ok {
		return
	}
	pe.concurrency[poolName]++
	if isCompletion {
		pe.completionActive[poolName]++
	}
}

// ReleaseAdmission records that an active attempt ended (for concurrency tracking).
func (pe *PolicyEvaluator) ReleaseAdmission(model string, isCompletion bool) {
	poolName, ok := pe.modelToPool[model]
	if !ok {
		return
	}
	if pe.concurrency[poolName] > 0 {
		pe.concurrency[poolName]--
	}
	if isCompletion && pe.completionActive[poolName] > 0 {
		pe.completionActive[poolName]--
	}
}

// refill updates the token bucket based on elapsed time.
// Uses a monotonic high-water mark to handle clock rollback safely.
func (b *tokenBucket) refill(now time.Time) {
	// Use monotonic high-water mark to handle clock rollback
	// If now is earlier than lastRefillMono, don't refill (clock rolled back)
	// If now is later, calculate elapsed from the last actual monotonic time
	if now.Before(b.lastRefillMono) {
		// Clock rolled back - don't refill, don't update lastRefill
		return
	}

	elapsed := now.Sub(b.lastRefillMono)
	if elapsed > 0 {
		tokensToAdd := elapsed.Seconds() * b.refillRate
		b.tokens = min(b.capacity, b.tokens+tokensToAdd)
		b.lastRefillMono = now
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
