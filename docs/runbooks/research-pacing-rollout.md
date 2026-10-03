# Research Pacing Rollout and Operations Runbook

## Overview

This runbook guides operators through deploying and managing research pacing in production, from initial observation through enforcement and emergency rollback. Research pacing controls task admission based on subscription quota limits, protecting against overload while maintaining quality.

**Key principle:** Research pacing is **non-disruptive** — deferring tasks does not affect existing active sessions, reviews, or rework. Only new task launches are affected.

## Prerequisites

- Odonian cluster version with research pacing support
- Database: SQLite with WAL mode enabled (provides durable state across restarts)
- Current fleet agents updated to support preclaimed task IDs (`ODONIAN_PRECLAIMED_TASK_ID`)
- Subscription details: concurrent request limits, quota per subscription, and cost per model

## Phase 1: Baseline Observation (No Enforcement)

Start in observe mode to collect metrics without operational impact.

### Setup

```bash
# Set observe mode
export ODONIAN_RESEARCH_POLICY_MODE=observe

# Set initial pool configuration (illustrative; adjust based on your quota)
export ODONIAN_RESEARCH_POOLS='
{
  "default": {
    "account_id": "your-account",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.5,
    "burst_capacity": 3,
    "concurrent_dispatch_limit": 4,
    "completion_reserved": 2
  }
}
'

# Restart server
systemctl restart odonian
```

### Observing Dispatch Durations

Dispatch duration is the time from task launch to completion. This determines sustainable start_rate.

**Method 1: Permit-based observation (simplest)**

```bash
# Extract dispatch durations from research_attempt
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  task_id,
  datetime(started_at) as start_time,
  datetime(ended_at) as end_time,
  duration_ms / 1000.0 as duration_seconds,
  state
FROM research_attempt
WHERE state IN ('finalized', 'expired')
  AND started_at > datetime('now', '-1 day')
ORDER BY ended_at DESC;
SQL

# Calculate statistics by completion type
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  completion,
  COUNT(*) as completed_attempts,
  ROUND(AVG(duration_ms) / 1000.0, 1) as avg_duration_sec,
  ROUND(MAX(duration_ms) / 1000.0, 1) as max_duration_sec,
  ROUND(MIN(duration_ms) / 1000.0, 1) as min_duration_sec
FROM research_attempt
WHERE state IN ('finalized', 'expired')
  AND started_at > datetime('now', '-1 day')
GROUP BY completion;
SQL
```

### Observing Active Permits

In observe mode, the server evaluates policy but admits all tasks. Track active permits to understand load.

```bash
# Count active permits by account and completion type
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  account_id,
  SUM(CASE WHEN completion = 0 THEN 1 ELSE 0 END) as active_writer_attempts,
  SUM(CASE WHEN completion = 1 THEN 1 ELSE 0 END) as active_completion_attempts,
  COUNT(*) as total_active_attempts
FROM research_attempt
WHERE state = 'active'
GROUP BY account_id
ORDER BY total_active_attempts DESC;
SQL
```

### Observing Shared-Account Headroom

Headroom is the difference between your subscription's concurrent limit and your actual peak concurrent usage.

```bash
# Current active attempts (running dispatches)
sqlite3 $ODONIAN_DB <<'SQL'
SELECT account_id, COUNT(*) as active_attempts
FROM research_attempt
WHERE state = 'active'
GROUP BY account_id;
SQL

# Peak concurrent observed in the last 24 hours (by completion type)
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  account_id,
  MAX(CASE WHEN completion = 0 THEN active_writers ELSE 0 END) as peak_writers,
  MAX(CASE WHEN completion = 1 THEN active_completion ELSE 0 END) as peak_completion
FROM (
  SELECT 
    account_id,
    strftime('%Y-%m-%d %H:00:00', started_at) as hour,
    SUM(CASE WHEN completion = 0 THEN 1 ELSE 0 END) as active_writers,
    SUM(CASE WHEN completion = 1 THEN 1 ELSE 0 END) as active_completion
  FROM research_attempt
  WHERE state = 'active' AND started_at > datetime('now', '-1 day')
  GROUP BY account_id, hour
)
GROUP BY account_id;
SQL
```

**Interpreting headroom:**
- Your pool's concurrent_dispatch_limit defines the allowed concurrent slot count
- Peak concurrent usage is the maximum active_attempts observed
- Headroom = (limit - peak) / limit
- Rule of thumb: keep 30-50% headroom for variance and future growth
- Headroom of less than 20% indicates you're running tight

### Checking Durable State Across Restarts

Verify that research attempts persist across server restarts.

```bash
# Before restart: record active attempts
sqlite3 $ODONIAN_DB "SELECT task_id, permit_id FROM research_attempt WHERE state = 'active' LIMIT 5;"

# Restart server
systemctl restart odonian

# After restart: verify attempts still exist
sqlite3 $ODONIAN_DB "SELECT task_id, permit_id FROM research_attempt WHERE state = 'active' LIMIT 5;"

# Compare: same permit_ids should exist, no duplicates
```

### Duration of Phase 1

Run observe mode for **1-2 weeks** to collect:
- Full cycle of your workload patterns (weekday/weekend, peak/off-peak)
- At least 100-200 completed research tasks (for statistical confidence)
- Hypothetical denial patterns under your proposed policy

## Phase 2: Drain Legacy Agents (Before Enforcement)

Old agents that bypass admission must be drained before enforcement activates, otherwise they will be deferred immediately (they don't support preclaimed task IDs).

### Identify Legacy Agents

Legacy agents are those NOT updated to support `ODONIAN_PRECLAIMED_TASK_ID` environment variable.

```bash
# Check agent source version
git log -1 --format='%H %s' -- harness/agent.sh
# Look for commit that adds admit_research_task and preclaim support

# Check running agents
ps aux | grep -E 'agent\.sh.*--kind (implement|review)' | head -20
```

### Drain Procedure

```bash
# Step 1: Signal no new work to legacy agents
# (Option A: set empty task board, or Option B: empty ODONIAN_PROJECT env)
export ODONIAN_PROJECT=""

# Step 2: Wait for in-flight tasks to complete
# Monitor active attempts count
watch -n 10 'sqlite3 $ODONIAN_DB "SELECT COUNT(*) FROM research_attempt WHERE state = '\''active'\'';"'

# When count reaches 0: all permits have been finalized

# Step 3: Upgrade agents to new version
git pull origin main
systemctl restart fleet-agents

# Step 4: Verify new agents can claim preclaimed tasks
# (They will log "ODONIAN_PRECLAIMED_TASK_ID=..." when launching)
tail -f /var/log/odonian/agent.log | grep PRECLAIMED
```

**During drain:**
- Users can continue submitting research tasks; they'll queue in `backlog`
- In-flight tasks complete normally; their attempts release
- Review and rework tasks (if any) continue to launch
- No user-visible disruption

## Phase 3: Enable Enforcement

Once observe mode has run for 1-2 weeks and all agents are upgraded:

### Calculate Pool Parameters

Using observations from Phase 1, determine start_rate, burst_capacity, and concurrent_dispatch_limit.

```bash
# Example calculation (adjust based on your data):
# Average dispatch duration observed: 8 seconds
# Desired sustained rate: 1 start every 16 seconds = 0.0625/sec
# Peak concurrent observed: 6
# Subscription limit: 20 concurrent
# Headroom desired: 40% → safe concurrent limit: 20 * 0.6 = 12
# Peak reviews+rework observed: 3

export ODONIAN_RESEARCH_POOLS='
{
  "default": {
    "account_id": "your-account",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.0625,
    "burst_capacity": 4,
    "concurrent_dispatch_limit": 12,
    "completion_reserved": 4
  }
}
'
```

### Enable Enforcement

```bash
# Change mode to enforce
export ODONIAN_RESEARCH_POLICY_MODE=enforce

# Verify configuration loads
systemctl restart odonian
tail -20 /var/log/odonian/server.log | grep -i "policy\|pool\|research"

# Should see output like:
# "research policy mode: enforce"
# "loaded 1 pool: default"
```

### Monitor First 24 Hours

After enabling enforce mode, observe attempt completion patterns to validate your pool configuration:

```bash
# Check completions by exit class (success vs failure modes)
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  exit_class,
  completion,
  COUNT(*) as count
FROM research_attempt
WHERE ended_at > datetime('now', '-24 hours')
GROUP BY exit_class, completion
ORDER BY count DESC;
SQL

# Check attempt duration distribution (retries and waiting times)
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  sequence_number,
  COUNT(*) as attempts,
  ROUND(AVG(duration_ms) / 1000.0, 1) as avg_duration_sec
FROM research_attempt
WHERE started_at > datetime('now', '-24 hours')
GROUP BY sequence_number
ORDER BY sequence_number;
SQL

# If many multi-sequence attempts → deferral is happening; may need higher concurrent_dispatch_limit
# If completion attempts are slow → may need higher completion_reserved
# Monitor server logs for "denied: concurrency" messages to understand deferral patterns
```

## Phase 4: Inspection and Tuning

Monitor deferred status and automatic resumption.

### Understanding Deferral Reasons

**concurrency:** Pool is at concurrent_dispatch_limit with all slots occupied. Task will resume when a permit finalizes.

**rate:** Rate limiter has exhausted start_rate allowance. Task will resume when the next rate window opens (typically 1-30 seconds).

**reservation:** Insufficient unreserved slots; completion_reserved slots are preserved for rework/review. Task waits for a review task to finalize.

### Automatic Resumption

Tasks don't need manual promotion. The agent's retry logic automatically re-asks periodically:

```bash
# In agent logs, look for deferral and later resumption:
grep "research admission deferred" /var/log/odonian/agent.log | head -3
grep "research admission deferred.*A:B:C" /var/log/odonian/agent.log | head -3  # Same task

# Timestamps should show retry attempts at intervals
```

### Tuning Guidelines

**If deferring too much (tasks waiting >30 seconds):**
- Increase start_rate slightly (e.g., 0.0625 → 0.1)
- Increase burst_capacity (e.g., 4 → 6)
- Increase concurrent_dispatch_limit if subscription allows

**If not deferring enough (hypothetical denials in observe mode were higher):**
- Acceptable; your policy is conservative (good for quota safety)
- Or you might have upgraded agents since observe phase — actual workload may differ

**If reviews are starving (no slots available):**
- Increase concurrent_dispatch_limit
- Or increase completion_reserved
- Or reduce start_rate to create space

## Phase 5: Rollback (Emergency)

Research pacing is designed to **not disrupt active work** — rollback is safe and non-destructive.

### Why Rollback?

- Quota exhaustion (set start_rate too high)
- Policy miscalibration (concurrent_dispatch_limit too low, too much deferral)
- Subscription plan changed

### Rollback Steps

```bash
# Step 1: Switch to disabled mode (no enforcement)
export ODONIAN_RESEARCH_POLICY_MODE=disabled

# Step 2: Restart
systemctl restart odonian
tail -10 /var/log/odonian/server.log

# That's it. All future tasks will be admitted.
```

**What happens during rollback:**
- In-flight tasks (with active permits) continue normally
- Permits finalize as they complete (no change)
- Deferred tasks immediately resume and launch
- No task quality change, no interruption

### Verification After Rollback

```bash
# Confirm policy mode is disabled
grep "policy mode\|research.*mode" /var/log/odonian/server.log | tail -3
# Should show: "policy mode: disabled"

# Check that active attempts continue to completion (no stuck attempts)
sqlite3 $ODONIAN_DB <<'SQL'
SELECT COUNT(*) as stuck_active
FROM research_attempt
WHERE state = 'active' AND started_at < datetime('now', '-30 minutes');
SQL
# Should return 0 (old active attempts should have finalized)
```

## Phase 6: Re-enable After Root Cause Fix

Once you've fixed the underlying issue (increased quota, updated concurrency estimates):

```bash
# Update configuration with corrected values
export ODONIAN_RESEARCH_POOLS='
{
  "default": {
    "account_id": "your-account",
    "models": ["haiku", "sonnet", "opus"],
    "start_rate": 0.08,  # Increased from 0.0625
    "burst_capacity": 5,
    "concurrent_dispatch_limit": 15,  # Increased from 12
    "completion_reserved": 4
  }
}
'

# Switch back to enforce
export ODONIAN_RESEARCH_POLICY_MODE=enforce

# Restart and monitor
systemctl restart odonian
```

## Billing Telemetry and Observable Limits

### Unavailable Billing Telemetry

The Odonian server does **not** have direct access to Anthropic subscription limits or current usage. Pacing is based on your own quota declarations in ODONIAN_RESEARCH_POOLS.

**Why this design?**
- Account and subscription details are sensitive (not exposed to Odonian)
- Usage changes across multiple systems (Odonian is one of many consumers)
- Operator has visibility across all systems; Odonian does not

**Your responsibility:**
- Monitor your actual usage through Anthropic Dashboard or billing API
- Adjust ODONIAN_RESEARCH_POOLS start_rate and concurrent_dispatch_limit based on observed headroom
- Set conservative limits if shared with other systems

### Observable Limits

What Odonian **can** observe:
- Dispatch durations (time from claim to finalize)
- Active permit counts (current concurrent dispatches)
- Deferral patterns and reasons

What Odonian **cannot** observe:
- Actual API usage (tokens, requests) from Anthropic
- Remaining quota this month
- Usage from other systems sharing the account

### Monitoring External Usage

Regularly check Anthropic Dashboard:

```bash
# (Manual process; not automated in Odonian)
# 1. Log into Anthropic Dashboard
# 2. Check usage graph for your account
# 3. Compare to your configured start_rate

# If external usage is high:
# - Reduce ODONIAN_RESEARCH_POLICY_MODE=disabled or set start_rate=0
# - Coordinate with other systems using the same account
```

## Durable State Across Restarts

All research pacing state is stored in SQLite database (WAL-enabled).

### What Persists

```sql
-- Active attempts (in-flight tasks)
SELECT task_id, permit_id, state, started_at FROM research_attempt 
WHERE state = 'active';

-- Pool accounting (start_rate token bucket state)
SELECT account_id, tokens, settled_at FROM research_pool;

-- Permit records (no duplicate claims)
SELECT task_id, permit_id, current_attempt_id FROM research_permit;
```

### Restart Guarantees

- **No duplicate permits:** A task with a permit will not be re-admitted after restart
- **Permit attempts resume:** Active attempts continue with their original timeline
- **Pool state exact:** Rate window accounting is preserved across restarts

### Crash Safety

If the server crashes during an attempt operation:

```bash
# Attempt is in one of three states:
# 1. Fully committed (in research_attempt table with state='active') → survives crash
# 2. In-flight (not yet committed) → lost, but agent will retry and get new attempt
# 3. Finalized/expired → persisted, won't be reused

# Verify no orphaned attempts (attempts that expired during downtime):
sqlite3 $ODONIAN_DB <<'SQL'
SELECT COUNT(*) as expired_during_downtime
FROM research_attempt
WHERE state = 'expired' AND ended_at > datetime('now', '-30 minutes');
SQL

# These are expected after restart if the server was down
```

## Troubleshooting

### Tasks stuck deferred indefinitely

**Symptom:** Agent logs show repeated "research admission deferred" with no resume.

**Diagnosis:**
```bash
# Check policy mode
grep "policy mode" /var/log/odonian/server.log | tail -1

# Check pool configuration loaded
grep "pool" /var/log/odonian/server.log | grep -i "load\|error"

# Check start_rate value
sqlite3 $ODONIAN_DB "SELECT account_id, start_rate FROM research_pool;"
```

**Fix:**
- If start_rate = 0: increase it
- If mode = disabled/observe: change to enforce
- If no attempt finalization: check why completion_reserved is too high

### Attempt leak (active_attempts growing indefinitely)

**Symptom:** `SELECT COUNT(*) FROM research_attempt WHERE state = 'active';` keeps growing.

**Diagnosis:** Attempts are not finalizing (tasks not completing or agent not calling permit-finalize).

**Fix:**
```bash
# Check for stuck agents
ps aux | grep agent.sh

# Check agent logs for errors
tail -100 /var/log/odonian/agent.log | grep -i "permit\|error\|finalize"

# Identify old attempts that should have finalized
sqlite3 $ODONIAN_DB <<'SQL'
SELECT task_id, permit_id, started_at
FROM research_attempt
WHERE state = 'active' AND started_at < datetime('now', '-1 hour');
SQL
# If many: task execution is stalled or agent isn't calling permit-finalize
```

### Expires_at timing issues

**Symptom:** Attempts show expires_at in the past but state is still active.

**Diagnosis:** Server clock has jumped or is out of sync.

**Fix:**
- Use gradual NTP slew correction (not step): `ntpd`, `chrony`
- Avoid `date -s` (manual step setting)
- Monitor: `ntpq -p` to check sync status
- On restart: database will recompute expiry times based on current clock

## Contacts and Escalation

- **Policy questions:** Check `docs/features/research-pacing-and-reviewer-evaluation.md`
- **Operational issues:** Check server logs and `research_attempt`/`research_permit` table state
- **Emergency disable:** Set `ODONIAN_RESEARCH_POLICY_MODE=disabled` and restart

---

**Last updated:** October 2026  
**Version:** Research Pacing Milestone 1
