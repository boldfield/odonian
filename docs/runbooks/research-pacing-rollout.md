# Research pacing rollout runbook (milestone 1)

Audience: the operator turning on automatic research pacing for a deployment. Design background is in
[`docs/features/research-pacing-and-reviewer-evaluation.md`](../features/research-pacing-and-reviewer-evaluation.md);
variable reference is [`docs/configuration.md`](../configuration.md#research-pacing-pools); the CLI verbs are in
[`AGENT-API.md`](../../AGENT-API.md#research-admission-cli-commands).

This runbook changes no rate. It contains no production numbers, and every number in it is an illustrative
placeholder you replace with values you measured (see [Observe and calibrate](#observe-and-calibrate)). It
does not touch the manifests repository or any cluster: the steps that change a deployment say so and are
yours to perform there.

## What pacing does, in one paragraph

Research LLM work (first-pass writing, review, rework, adjudication) must be *admitted* before a model
process is launched. Admission is one atomic server operation (`odonian claim <task>` on a research task):
it checks the task is claimable, checks the account pool, spends one start, records a durable permit and
attempt, and claims the task. A denial leaves the task `ready` and untouched (no lease, round, failure or
blocked state) and no model starts; the launcher skips it until the retry time and works on other eligible
tasks, so a deferred task resumes by itself. Build, design and merge work is never paced. Active research is
never interrupted by rate exhaustion.

## Configuration examples (illustrative only)

These are shape examples, not production recommendations. The values are chosen to be easy to read, not
derived from any measurement. Do not copy them into a live deployment.

Every model in `ODONIAN_MODELS` must belong to exactly one pool under `enforce`. `start_rate` is starts per
second, so `0.01` is one start every 100 seconds.

**One shared account, all research models (the smallest valid shape):**

```bash
export ODONIAN_MODELS="opus,fable,sonnet"
export ODONIAN_RESEARCH_POLICY_MODE=observe
export ODONIAN_RESEARCH_POOLS='{
  "main": {
    "account_id": "acct-example-1",
    "models": ["opus", "fable", "sonnet"],
    "start_rate": 0.01,
    "burst_capacity": 3,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 1
  }
}'
```

**Two accounts, so one provider's usage cannot starve the other's:**

```bash
export ODONIAN_MODELS="opus,fable,haiku"
export ODONIAN_RESEARCH_POLICY_MODE=observe
export ODONIAN_RESEARCH_POOLS='{
  "writers": {
    "account_id": "acct-example-1",
    "models": ["opus", "haiku"],
    "start_rate": 0.01,
    "burst_capacity": 3,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 1
  },
  "reviewers": {
    "account_id": "acct-example-2",
    "models": ["fable"],
    "start_rate": 0.02,
    "burst_capacity": 4,
    "concurrent_dispatch_limit": 2,
    "completion_reserved": 2
  }
}'
```

The `reviewers` pool reserves its whole limit for completion work (a review-only pool: a reservation equal to
the limit means first-pass writers can never take a slot in it).

What each field means for operators:

| Field | Effect | Notes |
|---|---|---|
| `account_id` | Identity of the external subscription. State is keyed by it and shared by every project and worker. | Renaming it creates a new, never-seen account with a full burst and no active dispatches. Treat a rename as resetting the allowance. |
| `models` | Models drawing from the account. | One pool per model; every alias on one account belongs in one pool. |
| `start_rate` | Sustained starts per second. | A proxy for consumption, not a billing figure. |
| `burst_capacity` | Starts available at once when idle. | Allowance never exceeds it; a restart does not refill it. |
| `concurrent_dispatch_limit` | Most dispatches active at once, completion work included. | A dispatch stays counted until its process exits and the harness finalizes the permit (or the lease lapses), not when the task is submitted. |
| `completion_reserved` | Slots only review/rework/adjudication may use. | First-pass writers hold at most `limit - completion_reserved` slots and never borrow reserved ones. The reservation is on concurrency only; every paced start spends from the same start-rate allowance. |

The policy is read from the environment when the server starts. Changing it means restarting the server (see
[Durable state](#durable-state-across-restarts)). Inspect what the running server loaded:

```bash
odonian research-policy --json   # mode and pools, no credentials
odonian research-status --json   # per pool: active, active_completion, deferred, tokens, settled_at
```

## Observe and calibrate

Choose the numbers from evidence you collect first, with the policy in `observe` mode. In `observe` mode every
claim is admitted exactly as if pacing were off, while the server records each attempt (start, end, exit class,
duration) and, for would-be denials, a hypothetical diagnostic. Nothing is limited, so `observe` says what
`enforce` *would* have done; it does not protect the subscription.

The server keeps the data in SQLite (the file named by `ODONIAN_DB`). The queries below are read-only; open the
database with `sqlite3 -readonly "$ODONIAN_DB"` (or against a copy). Timestamps are UTC strings such as
`2026-10-03T16:19:38.674659253Z`, so string comparison orders them.

Tables used (all created by migrations 0025 and 0026):

| Table | Holds |
|---|---|
| `research_pool` | One row per `account_id`: configured rate, burst, limit, reservation, current `tokens` and `settled_at`. |
| `research_permit` | One row per admission request: task, project, agent, model, account, whether it was completion work. |
| `research_attempt` | One row per dispatch attempt: `state` (`active`, `finalized`, `expired`), `started_at`, `expires_at`, `ended_at`, `exit_class`, `duration_ms`, `usage_tokens`. |
| `research_admission_diagnostic` | The latest admission decision per task, overwritten in place (bounded; no per-poll growth): `outcome`, `reason`, `hypothetical`, `denial_count`, `decided_at`. |

**How long dispatches really run** (finalized attempts only; an `expired` attempt's `duration_ms` is just the
lease length, not a measurement, so it is excluded):

```sql
SELECT account_id,
       CASE completion WHEN 1 THEN 'completion' ELSE 'write' END AS class,
       COUNT(*)                                   AS attempts,
       ROUND(AVG(duration_ms) / 60000.0, 1)       AS avg_minutes,
       ROUND(MAX(duration_ms) / 60000.0, 1)       AS max_minutes
FROM research_attempt
WHERE state = 'finalized'
  AND ended_at >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-7 days')
GROUP BY account_id, completion
ORDER BY account_id, completion;
```

Median duration for one account (replace `acct-example-1`):

```sql
SELECT ROUND(duration_ms / 60000.0, 1) AS median_minutes
FROM research_attempt
WHERE state = 'finalized' AND account_id = 'acct-example-1'
ORDER BY duration_ms
LIMIT 1 OFFSET (SELECT COUNT(*) FROM research_attempt
                WHERE state = 'finalized' AND account_id = 'acct-example-1') / 2;
```

**How many starts per hour the work actually generates** (this is the demand your `start_rate` and
`burst_capacity` would be pacing):

```sql
SELECT account_id,
       substr(started_at, 1, 13) AS hour_utc,
       COUNT(*)                  AS starts
FROM research_attempt
WHERE started_at >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-7 days')
GROUP BY account_id, hour_utc
ORDER BY account_id, hour_utc;
```

**Peak concurrent dispatches** (the demand your `concurrent_dispatch_limit` would be capping; an attempt with
no `ended_at` is counted until its lease expiry):

```sql
SELECT a.account_id,
       MAX((SELECT COUNT(*) FROM research_attempt b
            WHERE b.account_id = a.account_id
              AND b.started_at <= a.started_at
              AND COALESCE(b.ended_at, b.expires_at) > a.started_at)) AS peak_concurrent
FROM research_attempt a
WHERE a.started_at >= strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-7 days')
GROUP BY a.account_id;
```

**What `enforce` would have deferred** (observe mode only; `hypothetical = 1` means the claim was granted anyway):

```sql
SELECT account_id, work_class, outcome, reason,
       COUNT(*)           AS tasks,
       SUM(denial_count)  AS would_be_denials
FROM research_admission_diagnostic
WHERE hypothetical = 1
GROUP BY account_id, work_class, outcome, reason
ORDER BY account_id, work_class, reason;
```

A row's `denial_count` accumulates across polls of that one task, so it grows while a task is repeatedly asked
about; compare `tasks` for how many distinct tasks were affected.

**Reading the evidence.** Pick a starting `concurrent_dispatch_limit` below the account's tolerance and at or
above what you actually need at peak; a `start_rate` whose hourly product (`start_rate * 3600`) covers the
observed hourly starts with room left over for activity you cannot see (next section); and a
`completion_reserved` large enough that reviews of already-written work can always run. Then re-run the queries
after a week under `enforce` and adjust. Raising or lowering any value is a restart (below) and never refills
allowance beyond the new burst.

### Shared-account headroom

Pacing sees only what Odonian launches. Anything else using the same external account (people at a terminal,
other tools, another deployment, another environment) consumes the same quota and is invisible to these
tables. Pool values therefore have to leave headroom for that traffic, and the headroom is a judgment you make
from the account's own usage page, not something the pool can measure. If two Odonian deployments share one
subscription, they do not share a pool: each server paces only itself, so give each a share of the account,
not the whole of it.

### Billing telemetry is unavailable

`start_rate`, `burst_capacity` and `concurrent_dispatch_limit` count task starts and active dispatches. They
are proxies for consumption. A single start can use very little quota or a great deal, and nothing here
converts starts into a percentage of a subscription or a price.

`research_attempt.usage_tokens` exists so a runtime can report real usage, but the shipped harness does not
supply it: `permit-finalize` is called without `--usage-tokens`, so the column stays `NULL` (unknown, not zero).
Check that this is still true for your fleet:

```sql
SELECT COUNT(*) AS finalized, COUNT(usage_tokens) AS with_usage_tokens, SUM(usage_tokens) AS total_usage_tokens
FROM research_attempt WHERE state = 'finalized';
```

`with_usage_tokens = 0` means every figure you derive is a proxy. Calibrate against the account's own usage
page over the same window as the observed starts, and treat the result as an estimate.

## Durable state across restarts

Pool allowance (`research_pool.tokens` and `settled_at`), permits and attempts live in the same SQLite file as
the tasks. Restarting the server therefore:

- does **not** refill any bucket, and applying a changed pool configuration first settles allowance at the old
  rate, then clamps it to the new `burst_capacity`; a new `burst_capacity` can never mint extra starts;
- keeps active attempts, so concurrency is still counted after the restart and the harness's renewals simply
  continue (the same server URL is all the launchers need);
- never launches or duplicates a model process (the server launches nothing);
- leaves `ready` tasks that were deferred as they were; they are asked again when their retry time passes.

Occupancy is always derived from attempt rows that are still active and not past `expires_at`, never from an
in-memory counter, so it cannot drift from the database.

A pool whose `account_id` is new to the database starts with a full burst. A pool removed from the
configuration keeps its row, so removing and re-adding it does not refill it. Changing `ODONIAN_DB` to a fresh
file does reset everything.

## Safe rollout

Each step is reversible by the rollback section. Do not start step 5 until steps 1-4 are done and verified.

1. **Verify the build.** On the checkout you are about to deploy (merged with `main`), run the
   [smoke command](#smoke-test). It must exit 0 with `failed: 0`. It uses a temporary database, a fake
   `claude` and loopback networking, so it spends no subscription usage and touches no production state.

2. **Upgrade the server with pacing disabled.** `ODONIAN_RESEARCH_POLICY_MODE` unset (or `disabled`) admits
   everything and writes nothing. Confirm with `odonian research-policy --json` that `mode` is `disabled`.
   Behaviour is unchanged.

3. **Upgrade the launchers.** Roll out an image whose `harness/agent.sh` includes research admission
   (preclaimed task id, permit renewal, finalization on exit). This is a deployment change in the manifests
   repository; do it there. With the policy disabled the new launcher claims without a permit and works as
   before, so this step is safe to take first.

4. **Drain every legacy research launcher.** A launcher built before admission starts the model first and lets
   the model claim a task. Under `enforce` that is the wasteful path: the model process is already running (and
   spending usage) when the claim is denied with exit 10. Under `observe` such launchers are merely invisible
   to the permit tables, so their attempts may be missing from or expire in your calibration data. Scale legacy
   research workers and reviewers to zero and confirm none remain (the pod image tag on every research
   worker/reviewer). Then let their in-flight sessions finish. List research tasks still in progress in each
   project and wait for them to leave `in_progress`:

   ```bash
   odonian tasks --project <project-id> --state in_progress --json | jq -r '.[] | select(.track == "research") | "\(.id) \(.assignee)"'
   ```

5. **Observe.** Set `ODONIAN_RESEARCH_POLICY_MODE=observe` and the pool JSON, restart the server, and confirm
   with `odonian research-policy --json`. Run the research workload normally for long enough to be
   representative, then run the [calibration queries](#observe-and-calibrate). Under `enforce` the server
   refuses to start unless every model in `ODONIAN_MODELS` is mapped to a pool, so settle the model-to-pool
   mapping here, before enforcing.

6. **Enforce.** Set the mode to `enforce` with values you chose from step 5, restart the server, and watch
   `odonian research-status --json`. Expect `deferred` to rise when work arrives faster than the pool allows,
   then fall as capacity frees. Inspect why a task is waiting:

   ```sql
   SELECT d.task_id, d.work_class, d.model, d.account_id, d.outcome, d.reason, d.denial_count, d.decided_at,
          d.not_before, d.retry_after_ms
   FROM research_admission_diagnostic d
   JOIN task t ON t.id = d.task_id
   WHERE t.state = 'ready' AND d.hypothetical = 0 AND d.outcome IN ('defer', 'retry')
   ORDER BY d.decided_at;
   ```

   - `reason = rate`: the bucket is empty; `not_before` is when allowance allows a start.
   - `reason = concurrency`: every slot is busy; `retry_after_ms` is a retry hint, not a prediction.
   - `reason = reserved_capacity`: a slot is free but held for completion work; a first-pass writer waits.

   No manual action is needed. The launcher skips a deferred task until its retry time and then asks again, so
   the task starts by itself once capacity exists (single-project launchers also shorten their idle sleep to the
   next retry time). Never `promote`, edit or re-queue a deferred task to push it along; it is already `ready`.
   A task that stays deferred for a long time with `active = 0` is a pool-configuration or launcher problem, not
   something to fix on the task.

7. **Confirm quality is unchanged.** Deferral changes *when* a task starts, never what the reviewers require.
   Acceptance rules, source checks, round budgets, escalation and the human merge gate are untouched, and a
   deferral costs no review round. Nothing in this rollout routes work to a cheaper model.

### Rollback

Rolling back is a policy change, not a data repair:

1. Set `ODONIAN_RESEARCH_POLICY_MODE=disabled` (or `observe` to keep recording) and restart the server.
2. Active sessions keep running: restarting the server interrupts nothing and the launchers keep renewing.
   Their permits are simply no longer enforced; the attempts finish and finalize normally or lapse at lease
   expiry.
3. Waiting tasks are ordinary `ready` tasks and are claimed on the next poll. No task, round, lease or review
   state needs repair, and no task content changed.
4. Pool rows and attempt history are kept for the next attempt. Keep `ODONIAN_DB`; discarding it resets
   allowance.

You do not need to roll back the launchers: with the policy disabled the upgraded launcher behaves as before.

## Partitions, leases and honest limits

- **A lease is a coordination bound, not proof a process stopped.** Each dispatch holds an expiring permit
  (`ODONIAN_LEASE_TTL`, five minutes unless changed) that the harness renews while the process lives. If the
  harness loses the server, the server cannot signal it. After the lease lapses the attempt is marked `expired`
  and its slot is freed, even if the remote model process is still running and consuming usage. During that
  window the pool can be over-committed by the partitioned dispatches, for at most about one lease per such
  dispatch.
- **Fencing is by identity.** The stale process cannot affect the replacement: the server rejects its
  heartbeat, permit renewal and submit by attempt id (`ATTEMPT_FENCED`, `ATTEMPT_EXPIRED`,
  `ATTEMPT_FINALIZED`; exit code 11), and the harness stops its own process group on such positive evidence. A
  partitioned harness that cannot reach the server cannot do either, which is the limit described above.
- **Transient errors never kill work.** A failed renewal or heartbeat that is not an ownership conflict is
  logged and retried; rate exhaustion never stops an active session.
- **A submit does not end the dispatch.** Submission ends task ownership, but the permit stays active until the
  process exits and is finalized, so the slot stays counted. A rework claim on the same task is refused as
  `TASK_BUSY` (exit 11) until then.
- **No refunds.** A start whose launch outcome is uncertain stays debited. Accounting errs toward
  under-spending.
- **One server.** The pool is enforced by the single server and its SQLite file. Two servers on different
  databases do not share a pool.
- **Server time only.** A clock moving backwards never refills allowance.

## Smoke test

One command checks the milestone-1 behaviour end to end. It builds the server from the checkout, starts it on a
temporary database in `enforce` mode with a single account pool, and drives it with the real `harness/agent.sh`
and a fake `claude`. It can reach no real model, subscription, GitHub or cluster, and activates no production
limit.

```bash
bash harness/research_pacing_smoke_test.sh
```

It needs `go`, `git`, `jq` and `curl`, and takes under a minute. Exit status 0 means every check passed; any
failed check, or a failed helper step, makes it exit non-zero (there is no tolerated failure count). Set
`SMOKE_VERBOSE=1` for run-specific detail (port, task ids, allowance); the default output below is identical
from run to run.

What it proves, by behaviour:

| Scenario | Proof |
|---|---|
| 1. Concurrent launch limiting across two projects | Two projects each have a ready research writer for a pool whose test limit is 2 with 1 reserved. Exactly one model process starts; the other task stays `ready` with no assignee and round 0 and the server says `reserved_capacity`. |
| 2. Reserved review capacity | A review launches into the reserved slot while the writer is still denied; the pool shows 2 active, 1 completion; the writer's denial becomes `concurrency`. |
| 3. Persistence through restart | The server is stopped and restarted. The pool still shows the 2 active attempts, allowance is not refilled (within refill drift), renewals keep both dispatches alive, no process is relaunched, and the denied task is still waiting. |
| 4. Build work unaffected | With the research pool full, a build-track task launches immediately, takes no permit, and its launcher never asks for research admission. |
| 5. Waiting and automatic resume | After the running dispatches exit, the denied writer launches by itself with no promotion or edit. |
| 6. Rework reservation | A task rejected by a reviewer returns to `ready` at round 1; as completion work its rework claim is admitted into the reserved slot while a first-pass writer is denied `reserved_capacity`, then `concurrency` once slots are full, then admitted after a permit is finalized. |

The pool numbers inside the script are test values chosen to make the limits observable in seconds. They are
not recommendations for a deployment.

Command and exact output, from a run on the merged tree:

```text
OUTPUT_PLACEHOLDER
```
