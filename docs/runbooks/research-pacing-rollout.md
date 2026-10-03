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

**Method 1: Log-based observation (simplest)**

```bash
# Extract dispatch durations from server logs
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  task_id,
  model,
  datetime(started_at) as start_time,
  datetime(completed_at) as end_time,
  CAST((julianday(completed_at) - julianday(started_at)) * 86400 AS INTEGER) as duration_seconds
FROM research_attempts
WHERE completed_at IS NOT NULL
  AND started_at > datetime('now', '-1 day')
ORDER BY completed_at DESC;
SQL

# Calculate statistics
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  model,
  COUNT(*) as completed_tasks,
  ROUND(AVG(CAST((julianday(completed_at) - julianday(started_at)) * 86400 AS INTEGER)), 1) as avg_duration_sec,
  ROUND(MAX(CAST((julianday(completed_at) - julianday(started_at)) * 86400 AS INTEGER)), 1) as max_duration_sec,
  ROUND(MIN(CAST((julianday(completed_at) - julianday(started_at)) * 86400 AS INTEGER)), 1) as min_duration_sec
FROM research_attempts
WHERE completed_at IS NOT NULL
  AND started_at > datetime('now', '-1 day')
GROUP BY model;
SQL
```

### Observing Hypothetical Deferrals

In observe mode, the server evaluates policy but admits all tasks. Check what would have been deferred.

```bash
# Count hypothetical deferrals by reason
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  account_id,
  denial_reason,
  COUNT(*) as hypothetical_denials
FROM research_hypothetical_denials
WHERE evaluated_at > datetime('now', '-1 day')
GROUP BY account_id, denial_reason
ORDER BY hypothetical_denials DESC;
SQL
```

### Observing Shared-Account Headroom

Headroom is the difference between your subscription's concurrent limit and your actual peak concurrent usage.

```bash
# Current active permits (running dispatches)
sqlite3 $ODONIAN_DB <<'SQL'
SELECT account_id, COUNT(*) as active_permits
FROM research_permits
WHERE finalized_at IS NULL
GROUP BY account_id;
SQL

# Peak concurrent observed in the last 24 hours
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  account_id,
  MAX(active_at_time) as peak_concurrent,
  datetime(sample_time) as when_peak_occurred
FROM (
  SELECT 
    account_id,
    strftime('%Y-%m-%d %H:00:00', started_at) as sample_time,
    COUNT(*) as active_at_time
  FROM research_attempts
  WHERE started_at > datetime('now', '-1 day')
  GROUP BY account_id, sample_time
)
GROUP BY account_id;
SQL
```

**Interpreting headroom:**
- If your subscription allows 10 concurrent and peak observed is 4, headroom = 60%
- Rule of thumb: keep 30-50% headroom for variance and future growth
- Headroom of less than 20% indicates you're running tight

### Checking Durable State Across Restarts

Verify that research permits persist across server restarts.

```bash
# Before restart: record active permits
sqlite3 $ODONIAN_DB "SELECT task_id, permit_id FROM research_permits WHERE finalized_at IS NULL LIMIT 5;"

# Restart server
systemctl restart odonian

# After restart: verify permits still exist
sqlite3 $ODONIAN_DB "SELECT task_id, permit_id FROM research_permits WHERE finalized_at IS NULL LIMIT 5;"

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
# Monitor active_permits count
watch -n 10 'sqlite3 $ODONIAN_DB "SELECT COUNT(*) FROM research_permits WHERE finalized_at IS NULL;"'

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
- In-flight tasks complete normally; their permits release
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

After enabling enforce mode:

```bash
# Check deferral distribution by reason
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  denial_reason,
  COUNT(*) as deferrals
FROM research_deferrals
WHERE deferred_at > datetime('now', '-24 hours')
GROUP BY denial_reason
ORDER BY deferrals DESC;
SQL

# Check active dispatch count trend
sqlite3 $ODONIAN_DB <<'SQL'
SELECT 
  strftime('%Y-%m-%d %H:00', finalized_at) as hour,
  COUNT(*) as completions
FROM research_permits
WHERE finalized_at > datetime('now', '-24 hours')
GROUP BY hour
ORDER BY hour DESC;
SQL

# If 'concurrency' is the top deferral reason → your concurrent_dispatch_limit is too low
# If 'rate' is the top reason → your start_rate is too conservative
# Adjust and restart as needed
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
# Check that no new deferrals are recorded
sqlite3 $ODONIAN_DB <<'SQL'
SELECT COUNT(*) FROM research_deferrals
WHERE deferred_at > datetime('now', '-5 minutes');
SQL

# Should return 0

# Check server logs for mode
grep "policy mode" /var/log/odonian/server.log | tail -1
# Should show: "policy mode: disabled"
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
-- Active permits (in-flight tasks)
SELECT task_id, permit_id, started_at FROM research_permits 
WHERE finalized_at IS NULL;

-- Permit accounting (how many starts used this hour)
SELECT account_id, starts_used_this_window FROM research_accounts;

-- Task assignment to agent (prevents duplicate claims)
SELECT task_id, agent_id, claimed_at FROM research_claims;
```

### Restart Guarantees

- **No duplicate permits:** A task with a permit will not be re-admitted after restart
- **State preserved:** Permit leases are not reset; they continue their original timeline
- **No lost starts:** Rate window accounting is exact across restarts

### Crash Safety

If the server crashes during a permit operation:

```bash
# Permit is in one of three states:
# 1. Fully committed (record in research_permits table) → survives crash
# 2. In-flight (not yet committed) → lost, but agent will retry and get new permit
# 3. Finalized → persisted, won't be reused

# Verify no orphaned permits:
sqlite3 $ODONIAN_DB <<'SQL'
SELECT COUNT(*) as orphaned
FROM research_permits
WHERE finalized_at IS NULL
  AND started_at < datetime('now', '-1 hour')
  AND lease_expires_at < datetime('now');
SQL

# If orphaned > 0: permits have expired and are no longer active
```

## Troubleshooting

### Tasks stuck deferred indefinitely

**Symptom:** Agent logs show repeated "research admission deferred" with no resume.

**Diagnosis:**
```bash
# Check policy mode
systemctl status odonian | grep policy

# Check pool configuration for errors
systemctl status odonian | grep -i "pool\|error\|invalid"

# Check start_rate value
sqlite3 $ODONIAN_DB "SELECT account_id, start_rate FROM research_pool_config;"
```

**Fix:**
- If start_rate = 0: increase it
- If mode = disabled/observe: change to enforce
- If no permit finalization: check why completion_reserved is too high

### Permit leak (active_permits growing)

**Symptom:** `SELECT COUNT(*) FROM research_permits WHERE finalized_at IS NULL;` keeps increasing.

**Diagnosis:** Permits are not being finalized (tasks not completing or agent not calling permit-finalize).

**Fix:**
```bash
# Check for stuck agents
ps aux | grep agent.sh

# Check agent logs for errors
tail -100 /var/log/odonian/agent.log | grep -i "permit\|error\|finalize"

# Force finalize old permits (⚠️ only if > 1 hour old and no agent touching them)
sqlite3 $ODONIAN_DB <<'SQL'
UPDATE research_permits
SET finalized_at = datetime('now'),
    exit_class = 'unknown'
WHERE finalized_at IS NULL
  AND started_at < datetime('now', '-1 hour')
  AND lease_expires_at < datetime('now');
SQL
```

### Clock skew issues

**Symptom:** Permits expire prematurely or never expire; lease_expires_at is in the past.

**Diagnosis:** Server clock has jumped or is out of sync.

**Fix:**
- Use gradual NTP slew correction (not step): `ntpd`, `chrony`
- Avoid `date -s` (manual step setting)
- On restart: database will recompute lease times based on current clock

## Contacts and Escalation

- **Policy questions:** Check `docs/features/research-pacing-and-reviewer-evaluation.md`
- **Operational issues:** Check server logs and `research_permits` table state
- **Emergency disable:** Set `ODONIAN_RESEARCH_POLICY_MODE=disabled` and restart

---

**Last updated:** October 2026  
**Version:** Research Pacing Milestone 1
