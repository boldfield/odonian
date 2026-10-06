# Priority and Move-to-front rollout runbook

Design: [`docs/features/urgent-work-queue.md`](../features/urgent-work-queue.md). This runbook covers what the verification smoke proves, what it does not, and what an operator must do before and after deploying numeric priority. It sets no production values and performs no live action.

## Two kinds of number

| | Where it comes from | Allowed values |
| --- | --- | --- |
| **External input** | A person or client: task creation, `odonian priority --set N`, the TUI "Set priority" action, `--reset` (assigns 500) | integers 1..1000 only. Anything else (0, negatives, fractions, 1001 and above, non-numbers) is rejected with an error and **never clamped**. Omitted means 500. |
| **Generated / inherited value** | The server: `odonian priority --front` assigns `max(1000, max(P_queued)) + 1`; reviews, rework, adjudications, replacements, merges and continuations inherit the topic's value | any integer; always above 1000 for Front. These values are valid, are stored and displayed exactly, and are never passed through the manual-entry validator. |

Consequences an operator should expect:

- Front takes no number. Queue maximum 500 -> 1001, 730 -> 1001, 1000 -> 1001, 1042 -> 1043.
- Manual 505 or 1000 entered after a Front cannot overtake it (Front is at least 1001). Another Front can.
- The maximum is server-wide over every outstanding, non-archived topic (backlog, ready, held, blocked, dependency-blocked, in flight, in review), not the current project, model or filter. Wholly finished or archived topics do not count; a finished root counts only through an active descendant.
- Every Front permanently raises the ceiling by one, so a long-lived queue can hold values well above 1000. Reset (to 500) lowers a topic and, through it, the next Front's base.

## Removed behavior: the equal-priority project shuffle

Before this change the multi-project launcher (`harness/agent.sh`, `ODONIAN_PROJECT=all`) shuffled projects, so tasks of equal priority were dispatched in a random project order. That shuffle is gone. Every launcher (worker, reviewer, merger), `odonian next` and the TUI now use one comparator: **priority descending, `created_at` ascending, task ID ascending**. Operators will see a visible change even if nobody ever sets a priority: at the default 500 the **oldest** eligible task across all projects is dispatched first, so a project with a deep old backlog is served before a project with newer work. Raise a newer topic's priority to jump ahead.

Starvation is deliberate and unmitigated (no aging): a steady supply of higher-priority work keeps lower-priority work waiting. Use `--set`/`--reset` to correct it.

## What the smoke proves

`bash harness/priority_scheduling_smoke_test.sh` (also run by `go test ./harness`) builds `odonian` fresh into a temp dir, starts a real server on a random loopback port with a temp SQLite database per phase (two projects each), and drives the real `harness/agent.sh` in multi-project mode with a fake `claude`. It never calls a model, a subscription, GitHub or a cluster. Every expected number in it is a literal, not a recomputed formula. Passing means:

| Area | Evidence |
| --- | --- |
| Bounds | create and set accept 1, 1000 and the default 500; 0, 1001, -1, 100000, fractions and strings are rejected (HTTP 400, `INVALID_PRIORITY` for out-of-range integers) and leave the stored value untouched; the CLI refuses 0 and 1001 locally. |
| Front values | 500->1001, 730->1001, 1000->1001, 1001->1002, 1042->1043, and 40 consecutive Fronts (1003..1042). Owner regression: after Front from 500, later manual 505 and 1000 do not overtake it (`odonian next` still returns the fronted task) and a later Front does. |
| Concurrency and replay | 8 parallel Fronts on 8 tasks receive 1044..1051 with no duplicates; 6 parallel requests with one action key apply once and replay five times with the original 1052; the replays do not move the next Front (1053); the same key on another task is `409 IDEMPOTENCY_MISMATCH`; replaying an old Set key does not re-apply it. |
| Persistence | after a real server restart all 63 tasks have identical priorities and states, a pre-restart action key still replays to its original result, and the next Front continues from the persisted maximum. |
| `P_queued` scope | backlog, ready, held, dependency-blocked, blocked, in-flight and in-review topics each raise the maximum seen by a later Front; a superseded root counts only through its active replacement; once that replacement is archived the maximum drops; Front on a wholly finished topic is `409 TOPIC_NOT_OUTSTANDING`. |
| Lineage | a review created after Front inherits 1001; an existing review is rewritten by a later Front; rework after a rejection keeps the value; reset to 500 is topic-wide; a superseding replacement inherits. |
| Real launcher, build | with 8 tasks over two projects (Front 1001 and 1002, manual 1000 and 505, four at 500) `agent.sh` launches exactly 8 model processes in the single-comparator order, the four default-priority tasks oldest first across projects; each fake model claims exactly the task the launcher selected (the pinned `ODONIAN_SELECTED_TASK_ID`); each task has one owner start. |
| Real launcher, fall-through and no interruption | a held and a dependency-blocked task with the highest priority are skipped and left untouched; Fronting and resetting the running task itself changes no state, assignee, lease or process (no signal, no second start); a newly Fronted younger task is picked before older work at the next dispatch. |
| Real launcher, research | with pool `alpha` (opus) out of quota, a Fronted opus task (1002) is deferred `rate` and the launcher dispatches the lower-priority sonnet task (1001) from pool `beta`; the denial debits nothing and starts no process; a Front issued mid-dispatch does not signal, replace or duplicate the live model process; the highest-priority first-pass writer is denied `reserved_capacity` while a slot is free and the inherited-1003 review is admitted into it; beta is debited exactly two starts and alpha nothing beyond its one setup start; reset of the live topic leaves both processes untouched; rework (inherited 1001) is admitted into the reserved slot while a higher-priority first-pass writer (1002) is denied. |

## What the smoke does not prove (remaining gaps)

- **Continuation-child inheritance and dependency release.** Creating an approved continuation and a dependency becoming `done` need a forge and the merge flow, which a no-network smoke cannot run. These are covered by the store tests only (`internal/store/priority_inheritance_test.go`: continuation generations, completed anchor, merge reads the topic, adjudication spawn). A live check after deployment is listed below.
- **Merger launcher path and TUI.** The merger MULTI branch and the Bubble Tea actions are covered by `harness/scheduling_test.sh` (scenarios 9, 9b) and `cmd/odonian-tui` unit tests, not by this smoke.
- **Production configuration, real providers and clock-scale behavior.** Pool numbers in the smoke are illustrative and tiny; there are no real model starts, no real quota windows and no multi-hour refill.
- **Multiple server replicas.** The smoke uses one server process; Front serialization relies on the single SQLite write connection.

## Rollout requirements

1. Deploy server, CLI, TUI and fleet launcher image together; an old launcher still shuffles projects. Check `odonian version` on every runner and confirm no worker is on a pre-priority `agent.sh`.
2. Confirm the new server started cleanly on the production database and that `odonian tasks --json` now reports `priority` before announcing the feature.
3. Announce the equal-priority change: at default priority, dispatch becomes oldest-first across projects.
4. Do not reprioritize live work or release research holds as part of rollout; priority never releases a hold or promotes backlog work.
5. After deploy, with the operator's own judgment and on non-urgent work: Front one topic, confirm `odonian next` and a launcher pick it up at the next refresh (no in-flight task is interrupted), then Reset it. Confirm one approved continuation inherits the topic priority.
6. Rollback means redeploying the previous binaries. The smoke does not exercise an old binary against a database that has been through the priority migration, so rehearse that on a copy of the database before relying on it.

## Re-running

```bash
bash harness/priority_scheduling_smoke_test.sh        # about a minute; SMOKE_VERBOSE=1 prints ports and ids
go test ./harness -run TestHarnessScripts/priority_scheduling_smoke_test.sh
```
