# Checks gate: wait for CI on the board, not in a worker

Status: Implemented as of 2026-11-20. C1-C9 (schema, field, submit, checks decision, reconciler, operator surface) are complete. C10 (harness prompts) and C11 (project default) remain optional.

## Problem

A worker that needs CI to pass before it submits waits for CI inside its session. The session
cannot do other work, so the pod is occupied for as long as CI runs. On a project whose CI is slow
this is most of the worker's day.

Measured on the OhAnd project (iPhone app; simulator workflows run 34 minutes at the median and 60
at p90), from task events and GitHub workflow runs, 2026-10-07 to 2026-11-20:

| Measure | Value |
|---|---|
| Worker claim→submit sessions | 539 |
| Time those sessions held a claim | 534 h |
| Of that, with CI in flight on the task branch | 275 h (51%) |
| Sessions that waited for the head commit's CI to finish | 198 |
| Time from CI finishing to `submit`, median | 0.9 min (185 of 198 within 5 min) |
| CI result at submit in those sessions | 190 green, 8 red |
| In-session fix pushes that followed a failed run | 110 |

The 275 h is an upper bound: it counts runner queue time, and a worker could in principle work
while CI runs. The 0.9-minute figure says they do not. Workers push, wait, and submit as soon as CI
ends. That is a gate after the work, and the server can evaluate it without a model.

Reviewers have the same instruction ("if checks are still pending, wait and re-check",
`harness/prompts/pull_request/build/review.md`). They wait little today only because the worker has
already absorbed the wait.

## Decision

Add a task state, `checks`, between `in_progress` and `review`. A submission with a pull request
enters `checks` when the task opts in. No agent can claim a task in `checks`. A server reconciler
watches the pull request's head commit and resolves the state:

```
in_progress --submit--> checks --green--> review   (review tasks spawn here)
                               --red----> ready | blocked | superseded   (circuit breaker)
                               --timeout-> blocked
```

A generic "parked" state was considered and rejected. It would wake the task to `ready` and need a
cold worker session to read the result, which on the evidence above is wasted about 96% of the time
(190 green against 8 red). CI is also the only wait condition we have evidence for.

### What this costs

- **Cold restarts on red.** Today a worker fixes a red run in the same session. Under the gate each
  red run is a rework claim by a fresh session. Estimated 117–192 extra sessions over the measured
  window, at 10–17 minutes each (today's rework sessions, claim to first push): 20–55 h of active
  model time to remove up to 275 h of idle pod time.
- **More model quota, not less.** Waiting costs almost no quota; a cold restart re-reads the spec,
  the diff and the failure. This spec is the right trade when pods are the constraint, which is the
  operator's stated position (2026-11-20). It is the wrong trade when vendor rate limits are.
- **More circuit-breaker trips.** A red run counts as a round (C4). 36 measured sessions pushed
  three or more times.

## Scope

- `implement`-kind tasks on the `build` and `design` tracks, `pull_request` delivery.
- Opt-in per task. A task that does not opt in behaves exactly as today.
- Out: the `research` track (its attempt, permit and findings rules need their own design),
  `local_commit` delivery (no pull request), and `no_op` submissions (nothing to check).

## Deliverables

Each deliverable is one or more small tasks. Dependencies are stated at the end of each.

### C1 — Schema

One migration, following the table-rebuild pattern of `0003_approved_state.sql`,
`0009_superseded_state.sql` and `0012_abandoned_state.sql`:

- widen the `task.state` CHECK to include `checks`;
- add `await_checks INTEGER NOT NULL DEFAULT 0`;
- add `checks_started_at TEXT` (nullable), set when a task enters `checks` and cleared when it
  leaves. `updated_at` cannot serve: `PATCH /tasks/{id}` moves it.

Acceptance: a migration test loads a database at migration 0034 with tasks in every state and with
every nullable column populated, migrates, and asserts row counts, every column value, and every
index are preserved. Existing rows have `await_checks = 0`.

### C2 — The `await_checks` task field

- `POST /projects/{id}/tasks` accepts `await_checks` (boolean, default false). It is rejected with
  a validation error on a `research`-track task. The server does not know the delivery mode (it is
  a harness setting), so `local_commit` needs no rule: its submissions carry no `pr` link and never
  enter `checks` (C3).
- `PATCH /tasks/{id}` can set it, under the existing partial-update rule (an omitted field is
  unchanged). A change made while the task is in `checks` does not move the task.
- `GET /tasks/{id}`, list responses (including `fields=summary`) and `odonian show` return it.
- A replacement created by supersede or escalation copies it.

Depends on C1.

### C3 — Submit enters `checks`; green opens the review round

- `submitTask` (`internal/store/store.go`): an `implement` submission moves to `checks` instead of
  `review` when `await_checks` is true and the submission carries a `pr` link. Otherwise nothing
  changes. `review_round` still increments at submit, so links and rounds line up as today. No
  review task is created. `checks_started_at` is set. The event is `submit`, as today.
- Extract the review-task creation block of `submitTask` into a function that both paths call.
- New store method, used by the reconciler: `PassChecks(taskID, headSHA, summary)`. In one
  transaction, conditional on `state = 'checks'`: state becomes `review`, the review tasks for the
  current round are created by the extracted function, `checks_started_at` is cleared, and a
  `checks_passed` event records the head commit and summary. A second call is a no-op conflict.
- `odonian submit` needs no flag. Its output reports the resulting state.

Acceptance: an opted-in submission with a PR link lands in `checks` with no review tasks; an
opted-in `no_op` submission and a non-opted-in submission land in `review` with review tasks, as
today; `PassChecks` creates exactly the review tasks a direct submit would have (same models, round,
priority, spec text); a task in `checks` is not returned by `claimable=true` and cannot be claimed.

Depends on C2.

### C4 — Red and timeout

New store method `FailChecks(taskID, headSHA, summary)`, conditional on `state = 'checks'`:

- records a `checks_failed` event with the head commit and the failed check names and URLs;
- clears `checks_started_at`;
- runs `applyRoundCircuitBreaker` with the task's current `review_round`, exactly as a rejected
  review round does. The task goes to `ready`, to `blocked`, or is superseded by an escalated copy.
- The replacement spec built on supersede carries the failure summary in the same "Prior attempt
  feedback" block that carries reviewer rejections.

New store method `TimeoutChecks(taskID, note)`: `checks → blocked` with a transition event. The
notifier already reports `blocked` tasks.

**Decision recorded here: a red run consumes a round from the same budget as a reviewer
rejection** (`ODONIAN_ESCALATION_THRESHOLDS`). The alternative is a separate failure budget. The
shared budget is simpler and an hour-long red run is at least as strong a signal as a rejection. Its
weakness is flaky CI: a flake spends a round the model did not earn. Revisit if flakes cause
escalations in practice.

Acceptance: a failure below the threshold returns the task to `ready`; a failure past it blocks, or
escalates when `escalate` is set and a higher tier exists; the escalated copy's spec contains the
failed check names; `review_round` is unchanged by the failure itself.

Depends on C3.

### C5 — Read the head commit's checks (forge)

Add `forge.GetPRChecks(ctx, owner, repo, prNumber, token)`: one GraphQL request returning the pull
request's state (open, closed, merged), head commit, the commit's `statusCheckRollup.state`, and its
check contexts (name, status, conclusion, URL), covering both check runs and commit statuses, paged
to completion. Rate-limit responses return the existing `*forge.RateLimitError`.

One request per waiting task per poll. The REST alternative needs two or three.

Acceptance: `httptest` fixtures for a green, a pending, a failed, a mixed, an empty (no checks) and
a rate-limited response.

Depends on nothing.

### C6 — The decision rule (pure function)

`Decide(checks, now, checksStartedAt, config) → Wait | Pass | Fail | Timeout | PRClosed`, with no I/O:

1. Pull request merged → `Wait`: PR-watch converges an externally merged pull request to `done`
   (C9). Pull request closed without merging → `PRClosed`.
2. Any context concluded `failure`, `timed_out`, `cancelled`, `action_required` or
   `startup_failure`, or a commit status in `failure`/`error` → `Fail`, **without waiting for the
   other contexts**. In a sample of OhAnd failures the failing job was known 13–43 minutes in,
   against 60 minutes at p90 for the whole workflow.
3. Rollup state `SUCCESS` and the task has been in `checks` for at least `CHECKS_SETTLE` → `Pass`.
   The settle window guards against a fast workflow finishing before a slow one has registered.
4. No contexts at all: `Wait` until `CHECKS_NO_RUNS_GRACE` has passed since `checks_started_at`,
   then `Pass` with the summary "no checks reported". A repository with no CI, or a path-filtered
   push that triggers nothing, must not hang.
5. In `checks` longer than `CHECKS_TIMEOUT` → `Timeout`.
6. Otherwise `Wait`.

The rule reads the pull request's **current** head commit on every poll. A push or a re-run after
submit therefore re-opens the wait with no extra bookkeeping.

Acceptance: a table test with one row per rule and per boundary.

Depends on C5 for the types only.

### C7 — The reconciler

A `checkwatch` reconciler on the existing runner (`internal/reconcile`):

- Each pass lists tasks in `checks` across unarchived projects, and for each: reads the `pr` link,
  calls `GetPRChecks`, applies `Decide`, and acts.
  - `Pass` → `PassChecks`.
  - `Fail` → post one PR comment, then `FailChecks`. The comment begins with the marker
    `odonian-checks:` and lists the head commit and each failed check with its URL. If the
    transition fails after the comment is posted, the next pass may post a second comment; that is
    accepted.
  - `Timeout` → `TimeoutChecks`.
  - `PRClosed` → `TimeoutChecks` with a note saying the pull request was closed unmerged. A human
    decides.
- **GitHub quota.** PR-watch's per-owner rate-limit backoff and quota floor
  (`internal/prwatch/reconciler.go`, `rateLimitBackoff`, `checkQuotaFloor`) move to a shared
  package, and both reconcilers use **one shared instance**, so a rate limit seen by either stops
  both. A missing owner token skips that owner and is logged when the skipped count changes, as
  PR-watch does.
- **Poll interval.** The reconciler keeps its own `CHECKS_POLL_INTERVAL` and skips a pass that
  comes sooner. The runner's 30-second default tick is too frequent for this.
- A held task is still reconciled. `held` governs claiming; the task that returns to `ready` stays
  held.

Configuration (server environment, documented in `docs/configuration.md`):

| Variable | Default | Meaning |
|---|---|---|
| `CHECKS_POLL_INTERVAL` | `2m` | Minimum time between passes. |
| `CHECKS_SETTLE` | `2m` | Minimum time in `checks` before a green result passes. |
| `CHECKS_NO_RUNS_GRACE` | `5m` | How long to wait for a first check to appear. |
| `CHECKS_TIMEOUT` | `3h` | Longest stay in `checks` before blocking. |

Acceptance: reconciler tests with a fake forge and store for each action; a rate limit from the
forge stops calls for that owner in both reconcilers until the reset; the timeout path produces a
`blocked` task that the notifier reports.

Depends on C3, C4, C5, C6.

### C8 — A failed check is feedback the worker must acknowledge

- Add the role `checks` to the agent marker (`internal/forge/marker.go`).
- `isActionableGlobalComment` treats an `odonian-checks:` comment as actionable, like a reviewer's
  change request. `odonian pr-feedback list` then returns it, and the existing submit gate refuses a
  rework submit until it is acknowledged.
- A fix is acknowledged with the fixing commit, as today. A re-run with no code change (a flake)
  is acknowledged as disputed, with the reason.

Acceptance: an `odonian-checks:` comment is listed until acknowledged; `odonian-reconciler:`
comments remain non-actionable.

Depends on nothing; C7 depends on it.

### C9 — Operator surface

- `TransitionTask`: `checks` joins the active states for `blocked`, `failed` and `superseded`. Add
  `checks → review` as the operator bypass; it does what `PassChecks` does and records the operator
  as the actor.
- PR-watch's convergence of an externally merged pull request (`ConvergeExternallyMergedTask`,
  and the state list in `internal/prwatch/reconciler.go`) covers `checks` as it covers `review`:
  a pull request merged by hand while the task waits takes the task to `done`.
- `GET /projects/{id}/tasks?state=checks` works. `docs/api.md` and `AGENT-API.md` document the
  state, the field and the new events.
- TUI: a `checks` state between `in_progress` and `review`, with time in state.

Depends on C3.

### C10 — Prompts

- `implement.md`: when the task shows `await_checks: true`, push, open the pull request and submit.
  Do not wait for CI. On rework, a failed check appears in `odonian pr-feedback list`; read the
  failing job's log (`gh run view --log-failed`), fix or re-run, acknowledge, submit.
- `review.md` (build and design): for such a task, checks were green when the review was created.
  Still run `gh pr checks`. If the head commit's checks are not green now, reject and say the head
  changed after the gate. Never wait.

Acceptance: a test asserts that both prompts carry the instruction.

Depends on C7 being deployed; merge last.

### C11 — Project default (optional, after the rest)

A project-level default so new tasks on a slow-CI project opt in without the breakdown remembering
to: a `await_checks_default` column on `project`, `PATCH /projects/{id}` to set it, and task
creation inheriting it when `await_checks` is omitted. Until this exists, set the field per task
and `PATCH` existing `ready` tasks.

Depends on C2.

## Rollout

1. Release the server with C1–C9. With no task opted in, behaviour is unchanged.
2. Opt in one OhAnd task. Verify green → `review`, a forced red → `ready` with the feedback item,
   and the GitHub quota drawn per hour.
3. Release the fleet image with C10, then opt in OhAnd's remaining tasks.

## Out of scope

- **Resuming the same session** after CI, to avoid the cold restart. It needs session files shared
  across pods, the prompt cache will have expired, and the cold path must exist anyway for pod
  restarts. Consider it only if cold restarts prove expensive.
- **A fast compile-only CI job in OhAnd.** Several sampled red runs were one-to-three-line compile
  errors (a missing import, a missing `try`, a non-exhaustive switch) that cost 17–35 minutes of CI
  to discover, because Linux workers cannot compile Swift. A job that fails in minutes would cut
  both the wait and the cold restarts. It belongs to the OhAnd repository.
- **Webhooks.** The server is not reachable from GitHub; this polls.
- **The research track.**
- **Batching several pull requests into one GraphQL request.** An optimisation if quota needs it.

## Open questions

- **Which checks count.** This spec waits for the rollup of every check on the head commit. The
  alternative is only the checks branch protection requires. Every check is stricter and needs no
  branch-protection read.
- **`cancelled` as failure.** A cancelled check on the head commit is treated as red, so it
  surfaces instead of hanging until the timeout. A human cancelling a run on purpose will bounce the
  task.
