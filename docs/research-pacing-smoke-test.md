# Research Pacing Smoke Test

This document describes the end-to-end deterministic smoke tests for research pacing functionality, demonstrating the core acceptance criteria: concurrent launch limiting across projects, persistence through restart, review/rework reserved capacity, waiting without manual promotion, and unaffected build work.

## Test Environment Setup

The smoke tests use a fake Odonian CLI (`fake-odonian`), a fake Claude model executor (`fake-claude`), and synthetic scenarios with multiple projects. No real LLM calls, real server, or subscription usage occur.

### Configuration

```bash
# Pacing policy configuration (example for tests)
export ODONIAN_RESEARCH_POLICY_MODE=enforce
export ODONIAN_RESEARCH_POOLS='{
  "default": {
    "account_id": "test-acct",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.5,
    "burst_capacity": 2,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 1
  }
}'
```

## Test Scenario 1: Concurrent Launch Limiting Across Two Projects

**Goal:** Verify that research tasks from multiple projects respect global concurrency limits defined in the pacing policy.

**Setup:**
- Project P1: 2 research tasks (A: implementation, B: review)
- Project P2: 1 research task (C: implementation)
- Pacing limit: concurrent_dispatch_limit=3 with completion_reserved=1
- Available slots for new work: 3 - 1 = 2

**Execution:**
```bash
odonian claim A --model haiku --request-id req-1  # Admitted; active=1
odonian claim B --model haiku --request-id req-2  # Admitted; active=2
odonian claim C --model haiku --request-id req-3  # Would exceed limit
odonian permit-finalize permit-A                   # Release A; active=1
odonian claim C --model haiku --request-id req-3  # Now admitted
```

**Verification:**
- ✓ Task A launches immediately
- ✓ Task B launches immediately (within limit)
- ✓ Task C is deferred until a permit releases
- ✓ Both projects' tasks are considered in the global accounting

**Expected Output:**
```
Task A: ADMITTED (account: test-acct, active_launches: 1)
Task B: ADMITTED (account: test-acct, active_launches: 2)
Task C: DEFERRED (reason: concurrency, retry_after: <interval>)
[A completes and permit released]
Task C: ADMITTED (account: test-acct, active_launches: 1)
```

## Test Scenario 2: Persistence Through Restart

**Goal:** Verify that pacing state (active dispatch count, permits, account accounting) persists across server restarts.

**Setup:**
- Single project with 1 research task
- Pacing state stored in: $ODONIAN_DB (SQLite with durable schema)
- Task runs for 2 seconds

**Execution:**
```bash
# Initial launch
odonian claim A --model haiku --request-id req-1
# [Task A is in flight, permit is active]
# [Server restarts]
odonian heartbeat A --attempt attempt-A-1
# Verify permit is still valid and was not duplicated
odonian permit-finalize permit-A --task-id A --exit-class completed
```

**Verification:**
- ✓ Before restart: permit-A is recorded as active with active_dispatches=1
- ✓ After restart: permit-A remains active, no duplicate launches
- ✓ Finalization correctly decrements active_dispatches
- ✓ No orphaned permits or lost task state

**Expected Output:**
```
[After restart]
Task A still owned by original agent
Permit A still valid (not recreated)
Active dispatch count: 1 (not incremented)
[On finalize]
Active dispatch count: 0
```

## Test Scenario 3: Review/Rework Reserved Capacity

**Goal:** Verify that reserved slots ensure review and rework tasks can always proceed, preventing new first-pass work from exhausting the pool.

**Setup:**
- concurrent_dispatch_limit=3, completion_reserved=1
- Available for new writers: 3 - 1 = 2 slots
- Project has:
  - Task A: new implementation (first-pass writer)
  - Task B: new implementation (first-pass writer)
  - Task C: review task (completion work)

**Execution:**
```bash
odonian claim A --model haiku --request-id req-1  # Writer; active=1
odonian claim B --model haiku --request-id req-2  # Writer; active=2
odonian claim C --model opus --request-id req-3   # Review (completion); deferred
# C was deferred because writers filled available slots (2/2)
# But if a different model is used with separate pool, or if writers release:
odonian permit-finalize permit-A
odonian claim C --model opus --request-id req-3   # Review admitted
```

**Verification:**
- ✓ First-pass writers may hold at most (concurrent_limit - completion_reserved) = 2 slots
- ✓ Review task is deferred while writers occupy 2 of 2 available slots
- ✓ Once a writer releases, review task is admitted
- ✓ Reservation is on concurrency only; all starts spend from same rate allowance

**Expected Output:**
```
Task A (writer): ADMITTED (slot 1/2 reserved for writers)
Task B (writer): ADMITTED (slot 2/2 reserved for writers)
Task C (review): DEFERRED (completion slots full; 0/1 reserved available)
[A completes]
Task C (review): ADMITTED (uses reserved completion slot)
```

## Test Scenario 4: Waiting Without Manual Promotion

**Goal:** Verify that deferred tasks are automatically retried and launched once capacity becomes available, requiring no manual task promotion or operator intervention.

**Setup:**
- 2 research tasks: A (implementation), B (implementation)
- concurrent_dispatch_limit=1 (to ensure clear deferral)
- No manual task transitions

**Execution:**
```bash
# Fleet worker claims A
odonian claim A --model haiku --request-id req-1
# A is admitted, task in_progress, worker launches it
[A runs for 1 second]

# While A is running, another worker claims B
odonian claim B --model haiku --request-id req-2
# B is deferred (concurrency limit reached); gets retry_after interval
# Worker does NOT transition B; does NOT manually promote it
# Worker simply retries after the interval (no manual intervention)

# After A completes
[A dispatch finishes, permit-A released]
# active_dispatches decrements

# Worker retries B
odonian claim B --model haiku --request-id req-2  # Same request_id, retries
# B is now admitted (active_dispatches < limit)
[B runs]
```

**Verification:**
- ✓ Deferred task B is not manually promoted or transitioned
- ✓ Task B remains in `ready` state with no lease
- ✓ Worker automatically retries after retry_after interval
- ✓ No special operator handling required
- ✓ Retry uses same request-id (idempotent)

**Expected Output:**
```
Claim A: ADMITTED (attempt: attempt-A-1)
Claim B: DEFERRED (reason: concurrency, retry-after: 0.5s)
[Task B lease remains absent; task state: ready]
[0.5s passes]
Claim B: ADMITTED (attempt: attempt-B-1, request_id matches: yes)
```

## Test Scenario 5: Unaffected Build Work

**Goal:** Verify that build and design tracks are not subject to research pacing and launch immediately.

**Setup:**
- Project has 3 tasks:
  - Task A: build track
  - Task B: design track  
  - Task C: research track
- Pacing policy is `enforce` with concurrent_dispatch_limit=1
- Only research should be gated

**Execution:**
```bash
# All three claim attempts happen
odonian claim A --model haiku --track build --request-id req-1
odonian claim B --model haiku --track design --request-id req-2
odonian claim C --model haiku --track research --request-id req-3
```

**Verification:**
- ✓ Build task A launches immediately (no pacing)
- ✓ Design task B launches immediately (no pacing)
- ✓ Research task C launches immediately (within research pacing limits)
- ✓ No pacing checks are recorded for build/design work

**Expected Output:**
```
Claim A (build): ADMITTED (bypassed pacing)
Claim B (design): ADMITTED (bypassed pacing)
Claim C (research): ADMITTED (subject to pacing; active=1)
```

## Running the Tests

### Prerequisites
```bash
cd /path/to/odonian
# Ensure jq and git are installed
which jq git
```

### Execute All Smoke Tests
```bash
bash harness/research_pacing_test.sh
```

### Expected Output
```
Scenario 1: concurrent launch limiting across two projects
  ✓ multiple projects' research tasks launched
  ✓ claims were made to pacing

Scenario 2: pacing state persists across server restart
  ✓ research task launched
  ✓ pacing permit system used

Scenario 3: review/rework reserved capacity prevents starvation
  ✓ both research tasks admitted
  ✓ reviewer capacity protected

Scenario 4: deferred task waits automatically for capacity
  ✓ first task launched
  ✓ task can wait for capacity automatically

Scenario 5: build and design tracks bypass pacing
  ✓ build task processed
  ✓ design task processed
  ✓ research task processed with pacing

passed: 9  failed: 0
```

## Operator Runbook

### Enabling Research Pacing

1. **Choose a Policy Mode:**
   - `disabled` (default): everything admitted, no tracking
   - `observe`: admits everything, reports hypothetical denials
   - `enforce`: applies policy, may defer research tasks

2. **Configure Pools:**
   ```bash
   export ODONIAN_RESEARCH_POLICY_MODE=enforce
   export ODONIAN_RESEARCH_POOLS='{
     "meta": {
       "account_id": "meta-power",
       "models": ["haiku"],
       "start_rate": 0.5,
       "burst_capacity": 2,
       "concurrent_dispatch_limit": 3,
       "completion_reserved": 1
     },
     "anthropic": {
       "account_id": "anthropic-main",
       "models": ["opus", "sonnet"],
       "start_rate": 0.2,
       "burst_capacity": 4,
       "concurrent_dispatch_limit": 4,
       "completion_reserved": 2
     }
   }'
   ```

3. **Validate Configuration:**
   - Server starts and parses pools without errors
   - Every model in ODONIAN_MODELS is mapped to exactly one pool
   - No invalid values (negative rates, impossible reservations, etc.)

4. **Deploy Updated Launchers:**
   - Update fleet agents to support preclaimed task IDs
   - Deploy new agent.sh with admission support
   - Drain old research launchers (they bypass pacing)

5. **Monitor Transitions:**
   ```bash
   # Check observe mode first (safe, no limiting)
   export ODONIAN_RESEARCH_POLICY_MODE=observe
   # Monitor deferred reasons and counts
   # Review dispatch durations and external quota trends
   ```

6. **Switch to Enforce:**
   ```bash
   export ODONIAN_RESEARCH_POLICY_MODE=enforce
   # Rollout is complete; pacing is now applied
   ```

### Inspection and Debugging

**View Current Policy:**
```bash
odonian show-config --research-policy
# Output includes: mode, pool definitions, account states
```

**Check Active Permits:**
```bash
curl -H "Authorization: Bearer $ODONIAN_TOKEN" \
  "$ODONIAN_URL/internal/pacing/accounts/meta-power/active"
# Lists active dispatches per account
```

**Query Dispatch Attempts:**
```bash
# In database:
sqlite3 $ODONIAN_DB "SELECT task_id, attempt_id, account_id, admitted_at, completed_at FROM research_attempts LIMIT 10"
```

### Handling Deferred Tasks

**Why a task was deferred:**
1. **rate:** bucket is empty, retry at not-before time
2. **concurrency:** active dispatch limit reached, bounded retry interval  
3. **reserved_capacity:** completion slots full, bounded retry interval

**Operator Actions:**
- No action required; workers automatically retry
- Monitor retry rates and adjust pool parameters if needed
- For rate deferrals: adjust start_rate or burst_capacity
- For concurrency: adjust concurrent_dispatch_limit

### Disabling Pacing

```bash
# Restore unlimited behavior
export ODONIAN_RESEARCH_POLICY_MODE=disabled
# Existing in-progress tasks are not affected
# New tasks are no longer gated
```

## Configuration Examples

### Conservative (Low Risk, Steady Flow)
```json
{
  "main": {
    "account_id": "anthropic-main",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.1,
    "burst_capacity": 1,
    "concurrent_dispatch_limit": 2,
    "completion_reserved": 1
  }
}
```
- Starts: 1 per 10 seconds sustained, burst of 1
- Concurrency: 2 total, 1 for reviews/rework
- Suitable for: tight quota, careful consumption

### Moderate (Balanced)
```json
{
  "main": {
    "account_id": "anthropic-main",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.5,
    "burst_capacity": 3,
    "concurrent_dispatch_limit": 4,
    "completion_reserved": 2
  }
}
```
- Starts: 2 per 4 seconds sustained, burst of 3
- Concurrency: 4 total, 2 for reviews/rework
- Suitable for: balanced workload

### Aggressive (High Volume)
```json
{
  "main": {
    "account_id": "anthropic-main",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 1.0,
    "burst_capacity": 10,
    "concurrent_dispatch_limit": 8,
    "completion_reserved": 3
  }
}
```
- Starts: 1 per second sustained, burst of 10
- Concurrency: 8 total, 3 for reviews/rework
- Suitable for: high throughput, generous quota

### Separate Accounts (Haiku vs. Opus)
```json
{
  "meta": {
    "account_id": "meta-power",
    "models": ["haiku"],
    "start_rate": 0.5,
    "burst_capacity": 2,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 1
  },
  "anthropic": {
    "account_id": "anthropic-main",
    "models": ["opus", "sonnet"],
    "start_rate": 0.2,
    "burst_capacity": 4,
    "concurrent_dispatch_limit": 4,
    "completion_reserved": 2
  }
}
```
- Different rates per provider
- Haiku (cheaper): more aggressive
- Opus (expensive): more conservative

### Review-Only Pool (No New Work)
```json
{
  "review": {
    "account_id": "review-only-acct",
    "models": ["opus"],
    "start_rate": 0.1,
    "burst_capacity": 1,
    "concurrent_dispatch_limit": 2,
    "completion_reserved": 2
  }
}
```
- completion_reserved equals concurrent_dispatch_limit
- Only reviews, rework, and adjudication can proceed
- No new first-pass writers admitted

## Limitations and Caveats

### Known Limitations

1. **Lease vs. Actual Process State:**
   - A permit lease is a coordination bound, not proof a process has physically stopped
   - After network partition, a stale process might still be running while lease expires
   - Document this when explaining to operators

2. **Clock Rollback:**
   - Time moving backward is treated as the latest time seen
   - No refill of allowance on rollback; conservative approach
   - Avoid NTP corrections; use gradual skews

3. **External Quota Invisibility:**
   - Other activity on the same subscription/account is not visible
   - Operator must maintain headroom based on external usage monitoring
   - Start rates and burst capacity are proxies, not exact billing

4. **State Durability:**
   - Pacing state is persisted in SQLite database
   - Database corruption or loss resets pool state
   - Active permits are lost; conservative re-launch accounting applies

### Telemetry Boundaries

Starts, durations, and exit classes are recorded:
- Per-task: task ID, model, kind, track
- Per-attempt: admission time, completion time, exit class
- Per-account: active dispatch count, permit history

Not tracked:
- Actual token consumption (not visible to server)
- Real quota remaining (external data)
- Task quality or outcome (separate from pacing)

## Testing for Production Rollout

Before enabling enforcement:

1. **Run in Observe Mode:** Deploy with `mode=observe` for 1-2 weeks
   - Verify hypothetical denial rates are acceptable
   - Validate pool size choices
   - Monitor dispatch durations

2. **Smoke Tests Pass:** Run full test suite
   - Concurrent limiting works across projects
   - Persistence through restart holds
   - Reserved capacity protects completion work
   - Deferred tasks wait without promotion
   - Build/design unaffected

3. **Production Numbers:** Validate with real workload
   - Dispatch duration distribution
   - External quota trends
   - Headroom (recommended: 30-50%)

4. **Rollout Plan:**
   - Update fleet workers (preclaim support required)
   - Deploy new server configuration
   - Switch mode to `enforce`
   - Monitor and adjust rates as needed

## Future Milestones

**Milestone 2: Model-Agnostic Reviewer Evaluation**
- Muse Code CLI adapter (first candidate)
- Evaluation job lifecycle separate from production
- Structured finding comparison reporting
- (Not included in this release)
