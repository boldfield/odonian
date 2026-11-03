# Feature spec — supersession closes the superseded attempt's pull request

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Implemented; historical incident and specification.**
> The [store](../../internal/store/store.go) contains best-effort superseded-PR cleanup, covered by [store tests](../../internal/store/store_test.go); the [PR watcher](../../internal/prwatch/reconciler.go) provides terminal-task cleanup. See the [later quota repair](2026-09-10-prwatch-quota-burn.md) and [operations](../operations.md). Source presence does not guarantee a live forge call succeeds.

**Status:** board milestone (Odonian project). **Date:** 2026-08-06.

## Problem

Escalation supersedes a task rather than reworking it in place.
`supersedeTaskTx` mints a **new task ID**, copies the title, spec, and
dependencies, resets `review_round` to 0, re-points dependents, and
retires the old task as `superseded`. Because the working branch is
derived from the task ID (`"mr/" + taskID[:8]`, see
`cmd/odonian-tui/board.go`), every escalation generation gets a fresh
branch and therefore a fresh pull request.

Nothing ever closes the previous generation's pull request.

Concretely, the sigil-programs task "surl: -c (save Set-Cookie to a jar
file)" escalated through four generations — branches `mr/119e7718`,
`mr/ab0ab876`, `mr/aa08a1fe`, `mr/daaa0540` — opening pull requests #48,
#49, #50, and #51 against `boldfield/sigil-programs`. Only the last was
current. The rest were left OPEN and abandoned. The same pattern left
#41 and #39 open after their successors #42 and #40 merged.

Three consequences, in descending order of how much they actually cost:

1. **Reviewers review the wrong PR.** Every generation carries an
   identical title, so the stale ones are indistinguishable from the
   live one. Reviewers of the current generation repeatedly surfaced an
   abandoned attempt — several review notes on the `-c` task state that
   "the actual implementation appears to live on PR #48" and then
   reject against defects that had already been fixed on the current
   branch. Multiple review rounds were consumed by this alone, and it
   is self-reinforcing: each wasted round escalates again, which opens
   another PR, which gives the next reviewer one more wrong candidate.
2. **Stale branches and PRs accumulate** in proportion to escalation
   depth, and are only ever cleaned up by hand.
3. **Create-and-abandon churn is an abuse signature.** The fleet's
   machine account was suspended on 2026-08-05; three pull requests
   opened within 80 minutes on this single task were part of the
   activity immediately preceding it. The trigger was never disclosed
   by GitHub, so this is correlation rather than established cause —
   but reducing needless PR creation lowers the exposure of whatever
   identity the fleet authenticates as, which is currently a human
   operator's personal token.

## Goal

When a task is superseded, its pull request is closed with a comment
pointing at the replacement task, and its head branch is cleaned up, so
that at most one open pull request exists per logical unit of work.

## Approach

Do this server-side in the supersede path. That is the only place that
knows both the retired task and the ID of its replacement.

`supersedeTaskTx` already retires the old task and emits a
`task_superseded` event. After the enclosing transaction **commits**,
resolve the retired task's `pr` link; if it is present and the pull
request is still open, post a short comment naming the replacement task
and close it, then delete the head branch. Deleting the branch is safe
because GitHub retains `refs/pull/N/head` for a closed pull request, so
the commits stay reachable for anyone auditing the attempt later.

Three constraints matter more than the mechanics:

- **Perform the close outside the store transaction.** A GitHub outage,
  a throttled API, or a revoked token must never roll back or block a
  supersession. The task state change is authoritative; closing the pull
  request is best-effort cleanup. Log the failure and carry on.
- **Be idempotent and never close a merged pull request.**
  `internal/forge/pr_state.go` already exposes `GetPRState`, which
  distinguishes merged, closed, and open — consult it and skip anything
  not open.
- **Reconcile, don't migrate.** `internal/prwatch/reconciler.go`
  already polls pull request state on a schedule. Extend it to close
  pull requests whose owning task sits in a terminal state
  (`superseded`, and by the same argument `abandoned`) while the pull
  request is still open. That drains the existing backlog of stale pull
  requests without a one-shot migration, and it covers any close that
  failed at supersession time.

`internal/forge` already provides `OwnerToken`, `PostPRComment`,
`SquashMerge`, `PRMergeability`, and `GetPRState`. It has **no** close
helper — add one, following the shape of the existing `SquashMerge` and
`PostPRComment` functions for authentication and error handling rather
than inventing a new pattern.

## Acceptance

- Superseding a task that has an open `pr` link closes that pull
  request and leaves a comment naming the replacement task.
- Superseding a task whose pull request is already merged, or already
  closed, changes nothing and does not log an error.
- Superseding a task with no `pr` link is a clean no-op.
- A GitHub failure during the close does not fail the supersession: the
  old task still reaches `superseded`, the replacement task is still
  created, dependencies are still re-pointed, and the failure is logged.
- The reconciler closes still-open pull requests belonging to tasks
  already in a terminal state, so the existing stale set on
  `boldfield/sigil-programs` (#48, #41, #39) drains without manual
  intervention.
- After a full escalation cycle, at most one open pull request exists
  for the task chain.

## Testing (required)

- Unit coverage of the supersede path: open pull request → close is
  invoked; merged pull request → close is not invoked; no `pr` link →
  no-op; forge returns an error → supersession still succeeds and the
  replacement task exists.
- Reconciler coverage: a superseded task with an open pull request is
  closed on the next pass; a `done` task's merged pull request is left
  untouched.
- The existing supersession and escalation tests must still pass
  unchanged — in particular `review_round` resetting to 0 on the
  replacement, dependency copying, and dependent re-pointing. This
  change must not perturb any of that.

## Out of scope

- **The fresh-branch-per-escalation design itself.** Starting each
  escalated generation from `origin/main` is deliberate: the
  higher-tier model gets a clean slate with prior feedback already
  folded into its spec. This spec only stops the previous generation's
  pull request from being left open.
- **`blocked → ready` not resetting `review_round`.** A task unblocked
  after the circuit breaker trips will re-block on its very next
  rejection, because the transition clears `assignee` and
  `lease_expires_at` but never the round counter. That is a real
  defect and it is why an unblocked task effectively gets one attempt
  rather than a fresh budget — but it is independent of pull request
  cleanup and wants its own spec.

---

## Update, 2026-08-11: still unimplemented, and the cost is now measured

This spec was written on 2026-08-06 and never decomposed into a task. Other
parts of the supersession work were tasked and completed — carrying prior
review feedback into the replacement, auto-promoting the replacement to
`ready` — but pull request cleanup was not among them. `supersedeTaskTx`
contains no pull request handling, and nothing anywhere in the repository
closes a pull request. This is not a regression; it was never built.

Five days later, a sweep across every project on the board found **33 stale
pull requests**, all belonging to `superseded` tasks, all left OPEN:

| repository | stale PRs |
|---|---|
| `opencrr/communityrapidresponse.net` | 16 |
| `boldfield/email-triage` | 14 |
| `boldfield/odonian` | 1 |
| `boldfield/trade-log` | 1 |
| `boldfield/reala.gent` | 1 |

They were closed by hand. That is the second manual cleanup this pattern has
required, and the volume scales with escalation depth: email-triage
accumulated 14 in roughly a day of heavy tier-2 work, with one task alone
(the tier-2 alerter) contributing three generations.

### One consequence the original spec understates

The original framing leads with reviewer confusion, which is real. The sharper
risk is **merge hazard**. A stale generation holds an *earlier* attempt at work
that has since landed in corrected form. Because each generation branches from
`origin/main`, a stale PR will frequently still merge **cleanly** — so merging
one silently reintroduces superseded code on top of the fix that replaced it,
with no conflict to signal the mistake.

In `email-triage` this was concrete: `#31` was the pre-rework diagnostics
attempt, superseded by the version that merged as `#32`; `#29` was an earlier
recovery-drain attempt. Either would have reverted a live production fix. A
human merge gate is the only thing standing between a fleet-driven repository
and that outcome, and it is being asked to distinguish generations whose titles
are identical.

### Retrofit

Closing the current backlog is done. The fix should also handle pull requests
already open when it ships, or the accumulated set stays open forever — a
one-shot reconciliation over `superseded` tasks with recorded `pr` links is
enough, and it is the same code path the live case needs.
