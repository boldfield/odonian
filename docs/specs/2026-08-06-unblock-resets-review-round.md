# Feature spec — unblocking a task resets its review round counter

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Not implemented in this baseline.**
> TransitionTask in the [store](../../internal/store/store.go) clears assignment and lease on unblock but does not reset review_round. The reset below remains a proposal: blocked → ready does not grant a fresh round budget. Use [AGENT-API.md](../../AGENT-API.md) for supported recovery operations.

**Status:** board milestone (Odonian project). **Date:** 2026-08-06.

## Problem

`review_round` is incremented once per submit-into-review for every
`implement` task. When all spawned reviews finish and at least one
rejected, the circuit breaker compares the counter against
`thresholdFor(model, …)`: at or under the threshold the task returns to
`ready` for another attempt; over it, the task either escalates to the
next model tier via supersession, or — if escalation is disabled or the
model is already top of the ladder — transitions to `blocked` for human
attention.

`blocked → ready` is the documented unblock path, and
`TransitionTask` handles it specially: the UPDATE clears `assignee` and
`lease_expires_at` through conditional expressions so the task becomes
freshly claimable. It does **not** clear `review_round`.

So an unblocked task resumes with its counter still above the threshold
that blocked it. It gets exactly one more attempt: the worker claims it,
submits, the counter increments again, and the first rejection from any
reviewer re-trips the breaker immediately. Unless that single attempt is
approved unanimously, the task returns to `blocked` having consumed a
full implement-plus-review cycle.

The deployed thresholds make this sharp rather than theoretical
(`ODONIAN_ESCALATION_THRESHOLDS=haiku=6,sonnet=4,opus=2,fable=1`, with
`ODONIAN_ESCALATION_LADDER=haiku,sonnet,opus,fable`). A `fable` task
has a threshold of 1 and sits at the top of the ladder, so it cannot
escalate — it can only block. The sigil-programs task
`daaa0540` ("surl: -c") is in precisely this state: `model=fable`,
`review_round=2`, `state=blocked`. Unblocking it grants one attempt,
after which round 3 exceeds the threshold of 1 and it blocks again.

That is not what an operator means by unblocking. The counter's own
semantics say so: `supersedeTaskTx` resets `review_round` to 0 on the
replacement task, because an escalated generation is a fresh start. A
human unblocking a task is making the same statement — try this again —
but gets no corresponding budget.

## Goal

`blocked → ready` restores a full review-round budget, so an unblocked
task gets a real generation of attempts rather than a single one.

## Approach

Reset `review_round` to 0 as part of the same conditional UPDATE in
`TransitionTask` that already clears `assignee` and `lease_expires_at`,
gated on the **source** state being `blocked`. The existing statement
already carries that pattern for two other columns; this adds a third
alongside them rather than introducing a new mechanism or a second
write.

The gating matters and is not incidental. `to='ready'` is legal from
`approved` as well as from `blocked`, and that path is the ordinary
rejection of an approved task — still the same generation, so it must
keep counting. Only a transition whose source state is `blocked` may
reset.

Record the reset in the transition event note that the path already
appends, so the audit trail distinguishes an unblock that restored the
budget from one that did not.

No new configuration. The reset is unconditional on the blocked→ready
edge; an operator who does not want another full generation can retire
the task to `failed` or `abandoned` instead, which are already legal
targets from `blocked`.

## Acceptance

- A task blocked by the circuit breaker, then transitioned to `ready`,
  has `review_round` 0 and can complete a full budget of attempts before
  the breaker can trip again.
- A task transitioned `approved → ready` keeps its `review_round`
  unchanged — that path must not reset.
- The transition still clears `assignee` and `lease_expires_at` on
  blocked→ready exactly as it does today, and still leaves them intact
  on approved→ready.
- The appended transition event records that the counter was reset.
- Escalation behaviour is unchanged: supersession still creates the
  replacement task with `review_round` 0, and thresholds still resolve
  through `thresholdFor` in the same order (configured override, then
  built-in per-model defaults, then `maxReviewRounds`).

## Testing (required)

- A task at `review_round` above its threshold, blocked, then unblocked,
  reports 0 and survives a subsequent rejection without re-blocking.
- `approved → ready` leaves the counter untouched.
- Both edges still produce the correct `assignee` and
  `lease_expires_at` results, so the new column does not disturb the
  existing conditional expressions.
- Existing circuit-breaker, escalation, and supersession tests pass
  unchanged — in particular the threshold comparison itself, which this
  spec does not alter.

## Out of scope

- **Threshold values.** Whether `fable=1` is the right budget for a
  top-of-ladder model is a tuning question, separate from the counter
  never being cleared. This spec does not change any default or
  deployed threshold.
- **The blocked state's other causes.** `blocked` is also reachable by
  explicit transition from any active state; those tasks are covered by
  the same edge and get the same reset, which is consistent — but no
  special handling is proposed for them.
- **Closing stale pull requests on supersession.** Tracked separately
  in `2026-08-06-supersede-closes-stale-pr.md`.
