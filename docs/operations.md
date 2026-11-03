# Operations guide

Source-checked 2026-11-03 against `b3da896` (v0.24.0); this is not a live deployment inventory.

How to tell why a board is not moving, and what an operator can safely change. Exact request
and response contracts are in [`docs/api.md`](api.md) and [`AGENT-API.md`](../AGENT-API.md);
configuration variables are in [`docs/configuration.md`](configuration.md). This page is the
diagnostic order, not a second copy of those contracts.

## First look

1. Look at the task itself (`odonian show <id>` / the TUI): state, assignee, lease, model,
   priority, dependencies, hold, and recent events. Most "stuck" reports are one of the states
   below, visible there.
2. Look at the slot logs for that kind/model. The harness logs why it skipped work: nothing
   claimable, admission deferred, account/model unavailable, last-model lines on a block.
3. Diagnose the cause before changing state. Some conditions clear when a lease or backoff
   expires; others require an operator decision.

## Why nothing is being dispatched

Scheduling order is global: priority descending, `created_at` ascending, task id ascending,
across all allowed projects. Ineligible head tasks are skipped; they do not block work behind
them. See [`harness/README.md`](../harness/README.md#scheduling).

| You see | It means | Operator action |
|---|---|---|
| Every slot logs "nothing claimable" | No eligible task of that kind at all — or every candidate is filtered out (next rows). Healthy workers log this on an empty board. | Check the rows below per task before restarting anything. |
| Task is `backlog` | Not promoted to `ready` yet. | Promote it, if it should run. Priority does not promote it. |
| Task is `ready` but never claimed: dependency-blocked | A task in `depends_on` is not finished. The dependent is claimable only when dependencies clear — and its own timestamps may not move when that happens. | Find and unstick the dependency. Raising the dependent's priority does not bypass the dependency and there is no priority donation to blockers. |
| Task is held (`hold`) | An operator/research hold excludes it from claimable discovery. | `POST /tasks/{id}/release` when the hold's reason is over. Release does not revoke a lease or unclaim anything. |
| Task is `in_progress`, lease in the future, nobody visibly working | The owner session died, or it is working quietly. The harness now hands claims back on backend failure (see below) and blocks legacy tasks whose session ended unfinished after bounded resumes — so check for those events first. Otherwise the task becomes claimable again when the lease lapses (`ODONIAN_LEASE_TTL`). | Wait for the lease, or have the *owner* unclaim. Do not unclaim someone else's task; the server refuses. |
| Research task stays `ready`; slot logs "admission deferred" | The pacing pool denied the preclaim (`rate`, `concurrency`, or `reserved_capacity`). No model was started and no start was spent on the denial; the harness retries at the server's hint while dispatching other work. | Usually none: it is the pool working. If it persists, read the [rollout runbook](runbooks/research-pacing-rollout.md); do not jump straight to quota conclusions — start-rate is a proxy, not billing. |
| All tasks of one model (or every Claude/Codex model) skipped; slot logs "account/model unavailable … skipping" | Local backoff in that harness process: `limit`/`signin` hold the provider account until the stated reset (+ margin) or a fixed window; `model_unavailable`/`other` hold one model. Other models/accounts keep dispatching. State is per-pod, so pods recover independently. | For `signin`, re-authenticate that provider (Codex fleet auth decays by design; see [`deploy/fleet/README.md`](../deploy/fleet/README.md)). For `limit`, wait for the logged reset. Changing board state does not repair provider access. Check whether a preceding failed dispatch left an owned claim before taking recovery action. |
| Task is `blocked` | Circuit breaker, an operator, a missing prompt, the research chain budget (`decompose`), or session recovery giving up (its note quotes the model's last message). | Read the block note/event, fix that cause, then unblock — see below. |

**Priority is not preemption.** Setting priority, or Move-to-front, never interrupts a running
session, never revokes a claim or lease, and never bypasses holds, readiness, dependencies,
pool admission, reviews, or merge gates. It changes the order at the *next* scheduling
decision, for the whole topic lineage. Contract: [topic priority](api.md) sections and
[`docs/runbooks/priority-rollout.md`](runbooks/priority-rollout.md).

## Handing a claim back (unclaim)

`odonian unclaim <id> --reason "<why>"` returns a claimed task to `ready` at once, clearing
assignee and lease and recording a `claim_released` event. It is ownership- and attempt-fenced:

- Only the current owner may unclaim: the task must be `in_progress` and `agent_id` must match
  the assignee. For research, `attempt_id` is required only when the task has a research
  permit for this agent — then it must be that permit's current attempt, else
  `409 ATTEMPT_FENCED` and nothing changes. A research claim made while the pacing policy was
  `disabled` (or by an unmapped model under `observe`) has no permit/attempt, so there is no
  attempt to supply. When an attempt id *is* supplied, it must be the current one even so.
  The CLI sends the attempt saved by `claim`.
- Implement, review, and merge tasks can all be unclaimed. A task with an active PR link, or a
  non-review task with a submitted round, cannot (`409`, with current state/assignee so you can
  tell "already handed back" from "someone else owns it" from "in review").
- A handback is not a failed attempt: dependencies, review round, priority, `escalate`, and
  `agent_merge` are unchanged; no review/rework counter moves.

The fleet harness attempts this automatically after a backend failure when the task is still its
own. A failed request or a submission precondition can leave the claim waiting for lease expiry.
Operators mainly need it for work claimed outside the harness. Full contract:
[`POST /tasks/{id}/unclaim`](api.md#post-tasksidunclaim).

## Review, rework, changing reviewers, unblocking

- **Build/design rounds are verdict-based.** A round is tallied only when every (non-abandoned,
  non-adjudication) review child for the current round is `done`. Any child that is done
  without an `approve` verdict fails the round; unanimous approval passes it. A failed round
  returns the parent to `ready` for rework unless the circuit breaker escalates it (superseded
  onto the next ladder tier) or blocks it. A passed round moves it to `approved` (or spawns a
  merge task, or finishes as a no-op; see below) — `approved` with `agent_merge=false` is a
  human gate, not completion.
- **Research rounds are finding-based, not verdict-based.** The same "every child done" gate
  applies, but the round then fails if any reviewer reported a *blocking finding* (see
  [`docs/research.md`](research.md) for which findings block), regardless of that reviewer's
  approve/reject verdict; a round with no blocking findings passes even on a `reject` verdict.
  A blocking finding that continues a worker-disputed finding is decided by adjudication:
  while an adjudication is pending the round is not decided at all — no state change, no
  rejected-round event, no circuit breaker. A failed research round follows the same
  rework/escalate/block flow, except the chain-wide research round budget blocks with reason
  `decompose` ahead of any escalation.
- **A held parent is never aggregated.** If the parent is `held`, aggregation is skipped
  entirely even when every review child is done: the parent stays where it is and no
  transition is recorded. Releasing the hold (`POST /tasks/{id}/release`) re-runs aggregation
  when the task is in `review` with all children done, so the deferred decision lands then.
- **Changing reviewers is supported.** `odonian update <id> --review-models a,b`
  (`PATCH /tasks/{id}`) replaces the whole list, validated like creation;
  `POST /tasks/{id}/supersede` accepts the same optional field for the replacement. In an
  open round, children for removed models are retired (a retired child's later submit is fenced off), replacements are spawned
  in the same round, and verdicts already recorded still count. This is the remedy when a
  reviewer model becomes unusable; editing `AGENT_CODEX_MODELS` is not — that only reroutes
  the dispatch to the other backend. See [`docs/api.md`](api.md) (PATCH/supersede) and
  [`AGENT-API.md`](../AGENT-API.md).
- **Unblocking does not reset review rounds.** `blocked → ready` clears assignee and lease and
  preserves round history, so one more rejection can re-trip the circuit breaker immediately.
  That is current behavior; a reset is specced, not built
  ([`docs/specs/2026-08-06-unblock-resets-review-round.md`](specs/2026-08-06-unblock-resets-review-round.md)).
  If you need a genuinely fresh round count today, supersede rather than unblock — and note
  the research chain-wide round budget still does not reset across supersession.

## Approved tasks, PR-watch, and the human gate

- For merge-state reconciliation, PR-watch watches `approved` tasks with `agent_merge=false` that carry a `pr` link:
  merged PR → task `done` (+ `odonian-merged`); closed unmerged → `abandoned`; "changes
  requested" newer than the approval → back to `ready` with a marker comment on the PR.
  It separately retries stale-PR cleanup for superseded and abandoned tasks. It skips owners
  without a forge token, even for public repos.
  Detail and token rules: [PR-watch](configuration.md#pr-watch-reconciler).
- `approved` with no PR movement is therefore normally a human decision outstanding, not a
  fleet fault. Check the PR before touching the board.
- **No-op auto completion is intentional.** When the round that just passed was submitted with
  an active (non-tombstoned) `no_op` link in *that round's* submission, and the task has no
  active `pr` link anywhere (task-wide, any round), the parent goes straight `review → done`
  during aggregation, regardless of `agent_merge`. A `no_op` marker left over from an earlier,
  rejected round does not count, and a task with any active `pr` link never takes this path.
  This only happens on a *passed* round, so it is still subject to the rules above: unanimous
  approval for build/design, and no blocking findings (after any adjudication) for research.
  There is nothing to merge — the acceptance claim is that the work is already satisfied, and
  reviewers verify that claim. This is an opted-in completion path, not a security bypass and
  not a missing merge: do not "fix" it by looking for a PR or reopening the task. See
  [`AGENT-API.md`](../AGENT-API.md) and [`docs/api.md`](api.md).

## What is not built: stall alerting

There is **no stall alert today**. The notifier publishes only `approved` / `blocked` /
recent-`failed` / merged events, so a board where claimable work exists but nothing changes
state looks exactly like an idle healthy board and pages nobody. The detection design exists —
[`docs/specs/2026-07-20-stall-detection.md`](specs/2026-07-20-stall-detection.md): claimable
work present *and* no non-terminal task in the project updated within a threshold — but no
`StallReconciler` is wired in, and `ODONIAN_STALL_THRESHOLD` / `ODONIAN_STALL_REALERT` in that
spec do nothing if set. Until it lands, "claimable work > 0 and `max(updated_at)` old" is a
manual check an operator (or an external cron against the API) has to make.

## Incident note: October 2026 claim orphaning

On 2026-10-21 both provider accounts hit limits within hours (Anthropic 5-hour session limit,
ChatGPT weekly limit). Failed dispatches never handed their claims back, and backoff was keyed
by model *name* while limits are per *account*, so the fleet kept claiming work it could not
run: 124 research tasks sat `in_progress` with nobody working on them, each locked until its
lease expired, and recovery lagged the provider reset by up to one lease. A day later,
single-model failures (one model's credits, one model unsupported on an account) showed that
pausing a whole account for a one-model fault is also wrong, and that reviewers pinned to a
dead model could not be redirected at all.

Current mitigations, each now in code:

- Owner/attempt-fenced unclaim, used automatically by the harness on backend failure — a failed
  dispatch avoids the remaining lease delay when handback succeeds. (Above; [`docs/api.md`](api.md#post-tasksidunclaim).)
- Per-account backoff for limits/sign-in until the provider-stated reset, per-model backoff
  for model-only failures, and no claiming at all while a window is open — an exhausted account
  now costs zero claims in that harness process.
  ([`harness/README.md`](../harness/README.md#dispatch-lifecycle),
  [`docs/configuration.md`](configuration.md#harness-harnessagentsh-and-its-wrappers).)
- Automatic lease/permit renewal beside every harness dispatch, so a long healthy session is
  not reaped mid-work; bounded session recovery, then `blocked`, for sessions that end with
  the task still owned.
- Reviewer lists are changeable at runtime, including inside an open round. (Above.)

Source record and design: [`docs/features/dispatch-failure-handling.md`](features/dispatch-failure-handling.md).

What this does **not** claim: universal deadlock prevention. Backoff state is per harness
process, not shared; a session killed outside the harness (or a harness SIGKILLed before
cleanup) can still leave a claim to expire with its lease; dependency chains can still stall
on one genuinely stuck task; and with stall alerting unbuilt, a novel standstill can still be
silent. These mechanisms shrink the known failure modes to bounded, visible delays; they do
not make "nothing is moving" impossible.

## After an interruption

Ctrl-C is not a drain anywhere in the fleet; see the interruption caveats in
[`harness/README.md`](../harness/README.md#one-engine). After one: check the task's state and
events on the board, and for merge interruption check the PR on GitHub too — a merge can have
landed there before the board recorded it. Reconcile those two views before retrying anything.

## References

- [`docs/api.md`](api.md) — HTTP contracts, error codes, state machine
- [`AGENT-API.md`](../AGENT-API.md) — the same contracts from an agent's seat, with `curl` flows
- [`docs/configuration.md`](configuration.md) — every variable, default, and which process reads it
- [`docs/research.md`](research.md) — research track lifecycle (admission, disputes, budgets)
- [`docs/runbooks/`](runbooks/) — priority rollout, research pacing rollout
- [`docs/README.md`](README.md) — documentation index
