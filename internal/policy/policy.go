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
	tokens     float64
	capacity   float64
	refillRate float64
	lastRefill time.Time
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
		pe.buckets[pool.Name] = &tokenBucket{
			tokens:     float64(pool.BurstCapacity),
			capacity:   float64(pool.BurstCapacity),
			refillRate: pool.StartRate,
			lastRefill: pe.clock(),
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
			return AdmissionResult{Admitted: false, Reason: ReasonRateLimit}
		}
		return AdmissionResult{Admitted: true}
	}

	pool := pe.Pools[poolName]

	// Check concurrency limit
	if pe.concurrency[poolName] >= pool.ConcurrentDispatchLimit {
		return AdmissionResult{
			Admitted:     false,
			Reason:       ReasonConcurrency,
			RetryAfterMs: 1000, // Suggest 1 second retry
		}
	}

	// Check completion capacity if applicable
	if isCompletion {
		reserved := pool.CompletionReserved
		activeCompletion := pe.completionActive[poolName]
		availableCompletion := reserved - activeCompletion

		if availableCompletion <= 0 {
			return AdmissionResult{
				Admitted:     false,
				Reason:       ReasonCompletionCap,
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
		bucket.lastRefill = now
		return AdmissionResult{Admitted: true}
	}

	// Rate limit exceeded - calculate when next token will be available
	timeSinceRefill := now.Sub(bucket.lastRefill).Seconds()
	tokensEarned := timeSinceRefill * bucket.refillRate
	timeToNextToken := (1.0 - bucket.tokens - tokensEarned) / bucket.refillRate
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
// Time moving backward (clock rollback) does not refill.
func (b *tokenBucket) refill(now time.Time) {
	elapsed := now.Sub(b.lastRefill)
	if elapsed > 0 {
		tokensToAdd := elapsed.Seconds() * b.refillRate
		b.tokens = min(b.capacity, b.tokens+tokensToAdd)
	}
}

func min(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
