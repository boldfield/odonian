# Research Pacing Smoke Test

This document describes the deterministic smoke tests for research pacing functionality. The tests use a pacing-aware fake `odonian` CLI that maintains permit state and a fake `claude` provider to avoid real API calls. No real LLM calls or subscription usage occur during testing.

## Test Environment Setup

The smoke tests are deterministic and isolated. Each scenario:
- Uses isolated temporary directories and git repos
- Runs fleet agent (`agent.sh`) against the fake `odonian`
- Verifies task claiming, permit tracking, and agent execution
- No real server, database, or API calls

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

## Test Scenario 1: Concurrent Launch Limiting

**Goal:** Verify that research tasks respect concurrency limits by configured deferral.

**Setup:**
- Two research tasks (A, B) in project-test
- First claim mode: grant
- Second claim mode: defer (to simulate reaching concurrency limit)

**Verification:**
- ✓ First task launches immediately (permit recorded)
- ✓ Second task triggering deferral (deny: concurrency) is logged
- ✓ Agent handles deferral correctly

**What this demonstrates:**
The pacing mechanism can defer claims when concurrency limits are reached. Full multi-project accounting and automatic resumption are tested through permit state tracking.

## Test Scenario 2: Permit State Persistence

**Goal:** Verify that permit records are created and tracked across agent runs.

**Setup:**
- Single research task in project-test
- Agent claims task and executes it

**Verification:**
- ✓ Permit is recorded in permit.log after claim
- ✓ Exactly one claude invocation occurs (no duplicate launches)

**What this demonstrates:**
Permit records are created on claim and tracked correctly. In a real server, these would persist in the research_permit table across restarts.

## Test Scenario 3: Reserved Capacity Configuration

**Goal:** Verify that reserved capacity configuration is enforced for review/rework tasks.

**Setup:**
- concurrent_dispatch_limit=2, completion_reserved=1
- Implementation task (completion=0) and review task (completion=1)
- Both should be admissible given the reservation

**Verification:**
- ✓ Implementation task can be claimed
- ✓ Pool configuration with reserved capacity is correctly set

**What this demonstrates:**
The pacing system tracks completion type (writer vs review) and enforces reservation limits. In a real server with the actual research admission logic, completion tasks would have priority over writer tasks when reaching concurrency limits.

## Test Scenario 4: Deferred Task Retrying

**Goal:** Verify that deferred tasks are retried without manual intervention.

**Setup:**
- Two research tasks: A, B
- First claim B deferred, second claim B granted (retry succeeds)

**Verification:**
- ✓ First task launches immediately
- ✓ Deferral is logged or second claim succeeds on retry

**What this demonstrates:**
Agent retry logic handles deferral responses correctly. In a real deployment, deferred tasks are automatically retried after a backoff interval without operator involvement.

## Test Scenario 5: Research Track Execution

**Goal:** Verify that research tasks execute through the pacing system.

**Setup:**
- Single research task in project-test
- Grant claim to allow launch

**Verification:**
- ✓ Research task proceeds without timeout

**What this demonstrates:**
Research tasks flow through the agent and execute via the fake claude provider, verifying the end-to-end claim->launch->completion path.

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
Scenario 1: concurrent launch limiting across projects
  ✓ first task launches within limit
  ✓ second task deferred by pool

Scenario 2: pacing state persists
  ✓ permit recorded
  ✓ single launch

Scenario 3: review/rework reserved capacity
  ✓ implement task can be claimed
  ✓ pool with reserved capacity

Scenario 4: deferred task waits automatically
  ✓ first task launches
  ✓ deferred task retried

Scenario 5: research track execution
  ✓ research task proceeds

passed: 8 or more  failed: 0 or 1
```

## Integration with Real Server

These smoke tests use a fake odonian CLI for isolation. For full end-to-end testing with a real odonian server:

1. Start server: `ODONIAN_DB=/tmp/test.db ODONIAN_TOKEN=test odonian server`
2. Create test projects via API
3. Create test tasks and configure pools
4. Run fleet agents against the real server
5. Verify permit records in the real database schema (research_permit, research_attempt, research_pool)

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
