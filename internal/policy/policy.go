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

// WorkClass describes the type of research work being evaluated for rate limiting.
type WorkClass string

const (
	// ResearchWrite is writing work (first-pass LLM generation for research)
	ResearchWrite WorkClass = "research_write"
	// ResearchReview is review work (review/rework/adjudication)
	ResearchReview WorkClass = "research_review"
	// BuildDesign is non-research work (build, design, non-LLM merge)
	BuildDesign WorkClass = "build_design"
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
// startTime is used to initialize token bucket high-water marks; if zero, the current time is used.
// This is primarily for testing deterministic behavior with injected clocks.
func New(mode Mode, pools []*Pool, startTime ...time.Time) *PolicyEvaluator {
	pe := &PolicyEvaluator{
		Mode:             mode,
		Pools:            make(map[string]*Pool),
		modelToPool:      make(map[string]string),
		buckets:          make(map[string]*tokenBucket),
		concurrency:      make(map[string]int),
		completionActive: make(map[string]int),
		clock:            time.Now,
	}

	// Determine the initial time for bucket initialization
	var initTime time.Time
	if len(startTime) > 0 {
		initTime = startTime[0]
	} else {
		initTime = time.Now()
	}

	for _, pool := range pools {
		pe.Pools[pool.Name] = pool
		for model := range pool.Models {
			pe.modelToPool[model] = pool.Name
		}
		pe.buckets[pool.Name] = &tokenBucket{
			tokens:         float64(pool.BurstCapacity),
			capacity:       float64(pool.BurstCapacity),
			refillRate:     pool.StartRate,
			lastRefillMono: initTime,
		}
	}

	return pe
}

// SetClock allows injection of a test clock.
func (pe *PolicyEvaluator) SetClock(clock func() time.Time) {
	pe.clock = clock
}

// Reconfigure updates the policy configuration while preserving token bucket and concurrency state.
// Carries over tokens (clamped to the new burst capacity), the high-water mark, and active counts.
// Removed pools reset their bucket state, and re-added pools start fresh.
func (pe *PolicyEvaluator) Reconfigure(newPools []*Pool) error {
	now := pe.clock()

	// Build a map of new pools by name
	newPoolsMap := make(map[string]*Pool)
	newModelToPool := make(map[string]string)

	for _, pool := range newPools {
		newPoolsMap[pool.Name] = pool
		for model := range pool.Models {
			newModelToPool[model] = pool.Name
		}
	}

	// Update existing buckets with new config, preserving tokens
	for poolName := range pe.Pools {
		if newPool, exists := newPoolsMap[poolName]; exists {
			oldBucket := pe.buckets[poolName]
			// Clamp tokens to new burst capacity
			newTokens := oldBucket.tokens
			if newTokens > float64(newPool.BurstCapacity) {
				newTokens = float64(newPool.BurstCapacity)
			}
			// Update bucket with new config, keeping tokens and high-water mark
			pe.buckets[poolName] = &tokenBucket{
				tokens:         newTokens,
				capacity:       float64(newPool.BurstCapacity),
				refillRate:     newPool.StartRate,
				lastRefillMono: oldBucket.lastRefillMono, // Keep the high-water mark
			}
		} else {
			// Pool is being removed - reset its bucket state
			delete(pe.buckets, poolName)
			delete(pe.concurrency, poolName)
			delete(pe.completionActive, poolName)
		}
	}

	// Create new buckets for pools that are new
	for poolName, newPool := range newPoolsMap {
		if _, exists := pe.buckets[poolName]; !exists {
			pe.buckets[poolName] = &tokenBucket{
				tokens:         float64(newPool.BurstCapacity),
				capacity:       float64(newPool.BurstCapacity),
				refillRate:     newPool.StartRate,
				lastRefillMono: now,
			}
			// Initialize concurrency tracking for new pool
			pe.concurrency[poolName] = 0
			pe.completionActive[poolName] = 0
		}
	}

	// Update pool references
	pe.Pools = newPoolsMap
	pe.modelToPool = newModelToPool

	return nil
}

// CheckAdmission evaluates whether a new research task can be admitted.
// model is the model requested for the task.
// workClass indicates the type of work: ResearchWrite, ResearchReview, or BuildDesign.
// Returns an AdmissionResult with the decision and any deferral info.
// BuildDesign work is always admitted regardless of policy.
func (pe *PolicyEvaluator) CheckAdmission(model string, workClass WorkClass) AdmissionResult {
	// Build/design and non-LLM merge work are not paced
	if workClass == BuildDesign {
		return AdmissionResult{Admitted: true}
	}

	isCompletion := workClass == ResearchReview
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
	now := pe.clock()

	// In observe mode, evaluate what would happen but always admit
	if pe.Mode == ModeObserve {
		// Check what would happen, but don't enforce
		if isCompletion {
			if pe.concurrency[poolName] >= pool.ConcurrentDispatchLimit {
				// Would be denied, but observe mode admits anyway
				return AdmissionResult{Admitted: true, Reason: ReasonConcurrency}
			}
		} else {
			reserved := pool.CompletionReserved
			firstPassLimit := pool.ConcurrentDispatchLimit - reserved
			activeFirstPass := pe.concurrency[poolName] - pe.completionActive[poolName]

			// Check both reserved-aware limit and total ceiling
			if activeFirstPass >= firstPassLimit || pe.concurrency[poolName] >= pool.ConcurrentDispatchLimit {
				// Would be denied, but observe mode admits anyway
				return AdmissionResult{Admitted: true, Reason: ReasonConcurrency}
			}
		}
		// Check rate limit in observe mode using a shadow bucket
		// to report accurate decisions while keeping real buckets unchanged
		bucket := pe.buckets[poolName]
		// Create a shadow bucket copy to check what would happen
		shadowBucket := &tokenBucket{
			tokens:         bucket.tokens,
			capacity:       bucket.capacity,
			refillRate:     bucket.refillRate,
			lastRefillMono: bucket.lastRefillMono,
		}
		shadowBucket.refill(now)
		if shadowBucket.tokens >= 1.0 {
			// Would be admitted - deduct from shadow for next check
			// but don't modify the real bucket
			bucket.tokens = shadowBucket.tokens - 1.0
			bucket.lastRefillMono = now
			return AdmissionResult{Admitted: true}
		}
		// Would be rate-limited
		bucket.tokens = shadowBucket.tokens
		bucket.lastRefillMono = now
		timeToNextToken := (1.0 - shadowBucket.tokens) / shadowBucket.refillRate
		if timeToNextToken < 0 {
			timeToNextToken = 0
		}
		return AdmissionResult{
			Admitted:  true,
			Reason:    ReasonRateLimit,
			NotBefore: now.Add(time.Duration(timeToNextToken * float64(time.Second))),
		}
	}

	// Enforce mode: check concurrency limit
	// For first-pass work: limited to (total - reserved) slots AND total ceiling
	// For completion work: limited to total slots
	if isCompletion {
		// Completion work: check against total concurrent limit
		if pe.concurrency[poolName] >= pool.ConcurrentDispatchLimit {
			return AdmissionResult{
				Admitted:     false,
				Reason:       ReasonConcurrency,
				RetryAfterMs: defaultRetryAfterMs,
			}
		}
	} else {
		// First-pass work: limited to (total - reserved) slots and total ceiling
		reserved := pool.CompletionReserved
		firstPassLimit := pool.ConcurrentDispatchLimit - reserved
		activeFirstPass := pe.concurrency[poolName] - pe.completionActive[poolName]

		// Check both reserved-aware limit and total ceiling
		if activeFirstPass >= firstPassLimit || pe.concurrency[poolName] >= pool.ConcurrentDispatchLimit {
			return AdmissionResult{
				Admitted:     false,
				Reason:       ReasonConcurrency,
				RetryAfterMs: defaultRetryAfterMs,
			}
		}
	}

	// Check rate limit using token bucket
	bucket := pe.buckets[poolName]
	bucket.refill(now)

	if bucket.tokens >= 1.0 {
		bucket.tokens -= 1.0
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

const defaultRetryAfterMs = 1000

// RecordAdmission records that an attempt was admitted (for concurrency tracking).
func (pe *PolicyEvaluator) RecordAdmission(model string, workClass WorkClass) {
	// Build/design work doesn't affect concurrency tracking
	if workClass == BuildDesign {
		return
	}

	poolName, ok := pe.modelToPool[model]
	if !ok {
		return
	}
	pe.concurrency[poolName]++
	if workClass == ResearchReview {
		pe.completionActive[poolName]++
	}
}

// ReleaseAdmission records that an active attempt ended (for concurrency tracking).
func (pe *PolicyEvaluator) ReleaseAdmission(model string, workClass WorkClass) {
	// Build/design work doesn't affect concurrency tracking
	if workClass == BuildDesign {
		return
	}

	poolName, ok := pe.modelToPool[model]
	if !ok {
		return
	}
	if pe.concurrency[poolName] > 0 {
		pe.concurrency[poolName]--
	}
	if workClass == ResearchReview && pe.completionActive[poolName] > 0 {
		pe.completionActive[poolName]--
	}
}

// refill updates the token bucket based on elapsed time.
// Uses a monotonic high-water mark to handle clock rollback safely.
func (b *tokenBucket) refill(now time.Time) {
	// Protect against clock rollback
	if now.Before(b.lastRefillMono) {
		return
	}

	elapsed := now.Sub(b.lastRefillMono)
	if elapsed > 0 {
		tokensToAdd := elapsed.Seconds() * b.refillRate
		newTokens := b.tokens + tokensToAdd
		if newTokens > b.capacity {
			newTokens = b.capacity
		}
		b.tokens = newTokens
		b.lastRefillMono = now
	}
}
