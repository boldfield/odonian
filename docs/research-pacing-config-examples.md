# Research Pacing Configuration Examples

This document provides concrete configuration examples for research pacing deployment, covering different quota scenarios, account pool strategies, and operational modes.

**⚠️ Illustrative Examples Only:** The numeric values in all examples below (start_rate, burst_capacity, concurrent_dispatch_limit, completion_reserved) are illustrative examples designed to demonstrate the configuration structure and policy patterns. They are NOT production recommendations and should not be used directly in production deployments. Each deployment must determine appropriate values based on its own:
- Subscription plan and quota limits
- Expected workload characteristics and peak concurrency
- Dispatch duration observations
- Account capacity and headroom requirements
- Team preferences for quality vs. throughput trade-offs

See the "Pool Sizing Guidelines" section below for methods to calibrate your own values.

## Basic Configuration Structure

All research pacing configuration uses environment variables:

```bash
# Enable/disable pacing
export ODONIAN_RESEARCH_POLICY_MODE=enforce  # or observe, disabled

# Define pool configurations as JSON
export ODONIAN_RESEARCH_POOLS='{ ... }'
```

**Validation:** The server validates all pools at startup:
- Every model in `ODONIAN_MODELS` must be mapped to exactly one pool
- No duplicate model mappings across pools
- All numeric values must be valid (positive, finite, appropriate type)
- No reservations exceeding concurrent_dispatch_limit

## Example 1: Single Account, All Models (Conservative)

**Use Case:** Small project, shared Anthropic account, limited quota

```bash
export ODONIAN_RESEARCH_POLICY_MODE=enforce
export ODONIAN_RESEARCH_POOLS='
{
  "anthropic": {
    "account_id": "anthropic-main",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.1,
    "burst_capacity": 1,
    "concurrent_dispatch_limit": 2,
    "completion_reserved": 1
  }
}
'
```

**Behavior:**
- Sustained rate: 1 start per 10 seconds (0.1/sec)
- Burst: 1 task can start immediately when idle
- Concurrency: max 2 active research dispatches
- Reserved: 1 slot for reviews/rework, 1 for writers
- Effect: Tight control, no starvation, slow steady flow

**Illustrative scenario:** This pattern might suit deployments with very tight quota constraints, but actual values should be determined from your own quota limits and dispatch duration observations.

## Example 2: Single Account, Moderate Load (Balanced)

**Use Case:** Medium project, adequate quota, balanced throughput

```bash
export ODONIAN_RESEARCH_POLICY_MODE=enforce
export ODONIAN_RESEARCH_POOLS='
{
  "anthropic": {
    "account_id": "anthropic-main",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.5,
    "burst_capacity": 3,
    "concurrent_dispatch_limit": 4,
    "completion_reserved": 2
  }
}
'
```

**Behavior:**
- Sustained rate: 2 starts per 4 seconds (0.5/sec)
- Burst: 3 tasks available immediately when idle
- Concurrency: max 4 active research dispatches
- Reserved: 2 slots for reviews/rework, 2 for writers
- Effect: Moderate pacing, reviews protected, good throughput

**Illustrative scenario:** This pattern demonstrates a mid-range configuration, but your deployment should calibrate start_rate and limits based on your actual subscription quota and dispatch durations.

## Example 3: Single Account, High Volume (Aggressive)

**Use Case:** Large project, generous quota, high throughput needed

```bash
export ODONIAN_RESEARCH_POLICY_MODE=enforce
export ODONIAN_RESEARCH_POOLS='
{
  "anthropic": {
    "account_id": "anthropic-main",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 1.0,
    "burst_capacity": 10,
    "concurrent_dispatch_limit": 8,
    "completion_reserved": 3
  }
}
'
```

**Behavior:**
- Sustained rate: 1 start per second (1.0/sec)
- Burst: 10 tasks available immediately when idle
- Concurrency: max 8 active research dispatches
- Reserved: 3 slots for reviews/rework, 5 for writers
- Effect: Aggressive pacing, high throughput, reviews still protected

**Illustrative scenario:** This pattern shows aggressive pacing, but only deployments with verified high quota capacity should use high start_rate values. Calibrate based on your subscription plan and monitoring.

## Example 4: Separate Providers (Meta Haiku vs. Anthropic Opus)

**Use Case:** Cost optimization, different subscriptions per model tier

```bash
export ODONIAN_RESEARCH_POLICY_MODE=enforce
export ODONIAN_RESEARCH_POOLS='
{
  "meta": {
    "account_id": "meta-power",
    "models": ["haiku"],
    "start_rate": 1.0,
    "burst_capacity": 5,
    "concurrent_dispatch_limit": 6,
    "completion_reserved": 2
  },
  "anthropic": {
    "account_id": "anthropic-main",
    "models": ["sonnet", "opus"],
    "start_rate": 0.2,
    "burst_capacity": 2,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 1
  }
}
'
```

**Behavior:**
- Haiku (cheaper): 1/sec sustained, 6 concurrent, more aggressive
- Sonnet/Opus (expensive): 0.2/sec sustained, 3 concurrent, conservative
- Each account tracked independently
- Model pool assignment is fixed; cannot move model between pools after config

**Illustrative scenario:** This demonstrates the multi-provider pattern with different rates per provider. Your actual rates should reflect your subscription limits and cost priorities.

## Example 5: Evaluate Reviews Separately

**Use Case:** Dedicated reviewer capacity, separate account for Muse evaluation

```bash
export ODONIAN_RESEARCH_POLICY_MODE=enforce
export ODONIAN_RESEARCH_POOLS='
{
  "writers": {
    "account_id": "anthropic-writers",
    "models": ["haiku", "sonnet"],
    "start_rate": 0.5,
    "burst_capacity": 3,
    "concurrent_dispatch_limit": 4,
    "completion_reserved": 0
  },
  "production-review": {
    "account_id": "anthropic-review",
    "models": ["opus"],
    "start_rate": 0.3,
    "burst_capacity": 2,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 3
  }
}
'
```

**Behavior:**
- Haiku/Sonnet (first-pass): independent pool, no reservation
- Opus (production review): independent pool, all slots reserved for completion
- Writers and reviewers consume separate quotas
- No cross-pool borrowing

**Illustrative scenario:** This demonstrates separate capacity pools for writers and reviewers. Determine your own values based on quota availability and observed review/rework concurrency.

## Example 6: Review-Only Pool (No New Writers)

**Use Case:** Pause new research, process backlog reviews only

```bash
export ODONIAN_RESEARCH_POLICY_MODE=enforce
export ODONIAN_RESEARCH_POOLS='
{
  "review-only": {
    "account_id": "review-only-acct",
    "models": ["opus"],
    "start_rate": 0.5,
    "burst_capacity": 2,
    "concurrent_dispatch_limit": 2,
    "completion_reserved": 2
  }
}
'
```

**Behavior:**
- concurrent_dispatch_limit (2) == completion_reserved (2)
- All slots reserved for reviews/rework/adjudication
- No capacity available for new first-pass writers
- New research tasks are deferred indefinitely

**Illustrative scenario:** This pattern demonstrates a review-only mode. Actual values should reflect your review queue depth and completion capacity.
- (Note: Haiku and Sonnet must still be in ODONIAN_MODELS but are unmapped; this fails validation unless removed)

## Example 7: Observe Mode (No Enforcement)

**Use Case:** Try pacing configuration before enforcement, measure impact

```bash
export ODONIAN_RESEARCH_POLICY_MODE=observe
export ODONIAN_RESEARCH_POOLS='
{
  "anthropic": {
    "account_id": "anthropic-main",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.5,
    "burst_capacity": 3,
    "concurrent_dispatch_limit": 4,
    "completion_reserved": 2
  }
}
'
```

**Behavior:**
- Everything is admitted (no actual limiting)
- Each decision is evaluated against the policy
- Hypothetical denials are logged and counted
- No impact on actual task flow
- Operator can monitor what would be deferred

**Illustrative scenario:** Observe mode with the same pool configuration, allowing safe collection of hypothetical denial data before enforcement.

## Example 8: Disabled (Default, No Pacing)

**Use Case:** Disable pacing, return to unlimited mode

```bash
export ODONIAN_RESEARCH_POLICY_MODE=disabled
# ODONIAN_RESEARCH_POOLS is ignored
```

**Behavior:**
- All research tasks admitted immediately
- No permits issued
- No concurrency tracking
- No rate limiting
- No pool configuration needed

**Illustrative scenario:** No pacing configuration or enforcement; all research tasks admitted without concurrency limits.

## Transition Examples

### Transitioning from Disabled to Observe

```bash
# Step 1: Apply observe configuration
export ODONIAN_RESEARCH_POLICY_MODE=observe
export ODONIAN_RESEARCH_POOLS='{ ... }'
# Restart server; no operational impact

# Step 2: Monitor for 1-2 weeks
# Check logs for hypothetical denial rates
# Validate pool size choices match workload
```

### Transitioning from Observe to Enforce

```bash
# Prerequisite: Update fleet agents to support preclaimed task IDs
# (Old agents that bypass admission must be drained)

# Step 1: Enable enforcement
export ODONIAN_RESEARCH_POLICY_MODE=enforce
# Restart server; pacing now applies

# Step 2: Monitor task flow
# Watch for deferred reason distribution
# Adjust start_rate and burst_capacity based on observations
```

### Adjusting Pools Without Losing State

```bash
# Old configuration
export ODONIAN_RESEARCH_POOLS='
{
  "pool": {
    "account_id": "acct-v1",
    "start_rate": 0.2,
    "concurrent_dispatch_limit": 2
  }
}
'

# Want to increase rate
# Option A: Modify in-place (changes effective immediately)
export ODONIAN_RESEARCH_POOLS='
{
  "pool": {
    "account_id": "acct-v1",
    "start_rate": 0.5,
    "concurrent_dispatch_limit": 4
  }
}
'
# Effect: start_rate and concurrent limits updated; existing state preserved

# Option B: Never change account_id for the same account
# (Changing it creates a new pool with full burst, looks like account reset)
```

## Pool Sizing Guidelines

### Determining start_rate

**Observation method:**
1. Monitor dispatch durations (from logs or metrics)
2. Calculate average dispatch time (T_avg)
3. Set start_rate = 1.0 / T_avg (one start per average dispatch duration)
4. Add headroom: start_rate = (1.0 / T_avg) * 0.5 (50% headroom for variance)

**Example (illustrative):**
- Average dispatch takes 5 seconds
- Base rate = 1/5 = 0.2 starts/sec
- With 50% headroom: 0.2 * 0.5 = 0.1 starts/sec (illustrative; use your own observations)

### Determining burst_capacity

**Observation method:**
1. Count tasks that queue up during peak periods
2. Set burst_capacity = expected_peak_queue_size + 1
3. Typical range: 1-10 (rarely higher)

**Example (illustrative):**
- Peak queue observed: 3-5 tasks
- Set burst_capacity = 5 (illustrative; use your own peak observations)

### Determining concurrent_dispatch_limit

**Observation method:**
1. Monitor concurrent dispatch count (active permits)
2. Set concurrent_dispatch_limit = observed_peak + 1
3. Consider subscription plan's concurrent request limit
4. Typical range: 2-10

**Example (illustrative):**
- Observed peak concurrent: 4 tasks
- Subscription allows 10 concurrent: set limit = 6 (illustrative; use your actual quota and headroom)

### Determining completion_reserved

**Observation method:**
1. Count concurrent completion tasks (reviews, rework) at peak
2. Set completion_reserved ≥ peak_completion_count
3. Typical: 1-3 slots per pool

**Example (illustrative):**
- Peak concurrent reviews: 2
- Set completion_reserved = 2 (illustrative; use your actual review concurrency)

## Validation Examples

### Valid Configuration
```bash
export ODONIAN_RESEARCH_POOLS='
{
  "pool1": {
    "account_id": "acct1",
    "models": ["haiku"],
    "start_rate": 0.5,
    "burst_capacity": 2,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 1
  }
}
'
# ✓ All fields present
# ✓ All models mapped: haiku ✓
# ✓ No duplicate models
# ✓ No duplicate account_ids
# ✓ All numbers valid (positive, finite, types correct)
# ✓ completion_reserved (1) ≤ concurrent_dispatch_limit (3)
```

### Invalid: Duplicate Models
```bash
export ODONIAN_RESEARCH_POOLS='
{
  "pool1": { "models": ["haiku"] },
  "pool2": { "models": ["haiku"] }
}
'
# ✗ haiku appears in both pools — rejected at startup
```

### Invalid: Duplicate Account IDs
```bash
export ODONIAN_RESEARCH_POOLS='
{
  "pool1": { "account_id": "acct1", "models": ["haiku"] },
  "pool2": { "account_id": "acct1", "models": ["sonnet"] }
}
'
# ✗ Same account_id in two pools — rejected at startup
```

### Invalid: Reservation Exceeds Limit
```bash
export ODONIAN_RESEARCH_POOLS='
{
  "pool": {
    "account_id": "acct",
    "models": ["haiku"],
    "concurrent_dispatch_limit": 2,
    "completion_reserved": 3
  }
}
'
# ✗ completion_reserved (3) > concurrent_dispatch_limit (2) — rejected
```

### Invalid: Negative Rate
```bash
export ODONIAN_RESEARCH_POOLS='
{
  "pool": {
    "start_rate": -0.5
  }
}
'
# ✗ Negative rate — rejected at startup
```

### Invalid: Non-Numeric Value
```bash
export ODONIAN_RESEARCH_POOLS='
{
  "pool": {
    "burst_capacity": "two"
  }
}
'
# ✗ "two" is not a number — rejected at startup
```

## Monitoring Configuration

### Check Active Configuration
```bash
# Server logs (startup)
# Look for: "loaded research policy: mode=enforce"
# Look for: "pool: <name>, account: <id>, rate: <rate>/sec, limit: <limit>"

# CLI (runtime)
# Future: odonian show-config --research-policy
# (Not implemented in initial release)
```

### Check Active Permits
```bash
# Database query (SQLite)
sqlite3 $ODONIAN_DB <<SQL
SELECT account_id, COUNT(*) as active_dispatches
FROM research_permits
WHERE finalized_at IS NULL
GROUP BY account_id;
SQL
```

### Check Deferral Reasons
```bash
# Database query
sqlite3 $ODONIAN_DB <<SQL
SELECT account_id, reason, COUNT(*) as count
FROM research_deferrals
WHERE deferred_at > datetime('now', '-1 hour')
GROUP BY account_id, reason;
SQL
```

## Performance Characteristics

### Admission Overhead
- Per-claim: ~1-5ms (policy evaluation + permit creation)
- No external calls, fully local decision
- Suitable for high-frequency admission checks

### State Durability
- All state in SQLite database with WAL mode
- Durable across crashes/restarts
- No data loss on orderly shutdown

### Scalability
- Per-account state: ~100 bytes base + 50 bytes per active permit
- Typical: < 1MB for 100 concurrent permits across 10 accounts
- Lookup: O(1) by account_id, O(n) for scans

## Troubleshooting

### Configuration won't load
```
Error: duplicate model mapping
```
Check for the same model in multiple pools.

```
Error: reservation exceeds limit
```
Ensure `completion_reserved ≤ concurrent_dispatch_limit`.

```
Error: unmapped model (enforce mode)
```
All models in ODONIAN_MODELS must be in a pool when mode=enforce.

### Tasks stuck deferred
- Check pool's start_rate (may be too low)
- Check current active dispatch count (may be at limit)
- Switch to observe mode to see hypothetical policies
- Verify pool account_id hasn't changed (reset)

### Clock issues
- Avoid NTP step corrections; use gradual slew
- Time rollback does not refill allowance (conservative)
- If clock jumps, restart server to resync

## Next Steps

1. **Choose a configuration** from examples above
2. **Set environment variables** (see Configuration Structure)
3. **Start server** with ODONIAN_RESEARCH_POLICY_MODE=observe
4. **Monitor for 1-2 weeks** and collect metrics
5. **Adjust pool parameters** based on observations
6. **Update fleet agents** to support preclaimed task IDs
7. **Switch to enforce mode** after validation

For more information, see:
- `docs/features/research-pacing-and-reviewer-evaluation.md` — full design
- `docs/configuration.md` — complete configuration reference
- `docs/research-pacing-smoke-test.md` — testing and validation
