# Feature spec — PR-watch must stop re-polling settled pull requests and must leave the fleet GitHub headroom

**Status:** board milestone (Odonian project). **Date:** 2026-09-10.

## Problem

The PR-watch reconciler (`internal/prwatch/reconciler.go`) exhausts the
GitHub primary rate limit of the fleet's shared identity roughly 35 minutes
after every hourly reset, then sits in backoff for the remaining ~25 minutes.
Because worker and reviewer pods authenticate with the same
`odonian-forge-tokens` secret, they inherit the exhaustion: reviewers log
`API rate limit exceeded for user ID 441322` when running `gh pr diff`, and
workers cannot run `odonian pr-feedback list/ack`, so the rework gate
stalls. One worker put itself to sleep until the next reset. Every hour
since the current server pod started at 05:50 UTC on 2026-09-10 shows the
same cycle in the server log:
`entering rate limit backoff owner=boldfield status=403`.

The load comes almost entirely from `retrofitClosePRsForTerminalTasks`.
Every reconcile pass (the `NOTIFY_INTERVAL`, 30 seconds by default) it
lists every `superseded` and `abandoned` task in every project and calls
`GetPRState` once for each one that still has a non-tombstoned `pr` link.
The retrofit was written as a backlog drain plus a safety net for a failed
inline close, but nothing ever records that a pull request has been dealt
with: a link is tombstoned only when GitHub returns 404. A pull request
that is already merged or closed is therefore re-polled on every pass,
forever. The cost is proportional to every terminal task the fleet has ever
produced, and it can only grow.

Measured on 2026-09-10:

| | |
|---|---|
| Terminal tasks with a live `pr` link | 126 (106 under `boldfield`, 20 under `opencrr` skipped for lack of a token) |
| Pass cadence while the token has quota | ~52 s (30 s interval plus ~22 s of sequential GitHub calls) |
| REST calls per hour from the retrofit pass alone | ~7,300 against a 5,000/hour budget |
| Tasks in `approved` (the state the main pass watches) | 0 |

So today the retrofit pass is effectively 100% of the reconciler's GitHub
traffic, and it accomplishes nothing after the first pass because every one
of those pull requests is already closed.

A secondary, cheaper problem: the reconciler is the least valuable consumer
of the shared quota (it can tolerate minutes of staleness; a reviewer
mid-review cannot) but it has no notion of a budget. It calls until GitHub
says no. Any future regression of this kind, or any growth in the
`approved` backlog, will starve the fleet again in the same way.

A tertiary nuisance: the reconciler emits
`no forge token for owner owner=opencrr skipped_pr_checks=20` on every pass,
815 times in nine hours, which buries the lines that matter.

## Goals

1. Each terminal task's pull request costs at most a bounded number of
   GitHub calls over its lifetime, not one per pass forever. After this
   change the steady-state retrofit traffic is zero when nothing new has
   been superseded or abandoned.
2. The reconciler never drives the shared identity's remaining quota below
   a configurable floor. When the floor is reached it stops calling GitHub
   for that owner until the quota window resets, exactly as it already does
   after a 403.
3. The "no forge token" warning is emitted when the situation appears or
   changes, not on every pass.

## Non-goals

- Making the `approved`-state watch O(repositories) instead of O(pull
  requests) via per-repo listing and GraphQL batching. Worth doing, but it
  is not what is on fire; it gets its own spec.
- Decoupling the PR-watch cadence from `NOTIFY_INTERVAL`. Same reasoning.
- Conditional requests (ETag / `If-None-Match`). Only pays off for the
  `approved` watch, which is currently idle.
- Giving the reconciler its own GitHub identity. An ops decision, outside
  this codebase.
- Retroactively tombstoning the existing backlog with a migration. The
  reconciler's own first pass after deploy does it naturally: each of the
  126 links is checked once, found merged or closed, and marked. That is
  one pass of the current cost, then silence.

## Behavior

### Settled pull requests are marked and never re-polled

`task_link.tombstoned_at` already means "the reconciler must not look at
this link again". Its comment in `internal/store/store.go` and the
`0013_task_link_tombstoned.sql` migration describe it narrowly as "PR
returned 404", but nothing outside the reconciler reads the column, and
the reconciler's only use of it is the skip check. Broaden the meaning
rather than adding a second column: a link is tombstoned when the
reconciler has established that there is nothing left to do for it.

In the retrofit pass this means two new tombstone points, in addition to
the existing 404 case:

- When `GetPRState` reports the pull request is `merged` or `closed`,
  tombstone the link before returning. Today this path returns silently and
  the same call is repeated next pass.
- When the retrofit itself closes an open pull request, tombstone the link
  after the close succeeds (branch deletion failing must not prevent the
  tombstone; the pull request is closed, which is the thing that matters).
  If the close call fails, do not tombstone; the next pass retries, which is
  the existing safety-net behavior.

The main pass over `approved` tasks needs no change: its terminal outcomes
(`Done`, `Abandon`) move the task out of `approved`, so it stops being
listed. A `Bounce` also transitions the task. Only `Noop` re-polls, and
that is correct because the pull request is genuinely still pending.

Update the column's doc comment on the `TaskLink` struct and on
`TombstoneLink` so the next reader learns the broadened meaning. Do not
touch the shipped migration file.

### Budget floor

Add a forge helper that reports the remaining primary-quota budget for a
token. GitHub's `GET /rate_limit` endpoint does not count against the
quota, so one call per owner per pass is free. The helper returns the
`core` resource's remaining count and reset time and returns the existing
`RateLimitError` shape on a 403/429 so the reconciler's error handling is
unchanged.

The reconciler consults it once per owner per pass, the first time it is
about to spend a call for that owner (both in the main pass and the
retrofit pass; whichever comes first). If remaining is below the floor it
enters the existing per-owner backoff with `not_before` set to the reported
reset time and logs one WARN naming the owner, the remaining count, and
the floor. Everything downstream (skipping the rest of that owner's tasks
this pass and on subsequent passes until `not_before`) already exists and
must be reused, not duplicated.

The floor is a constructor parameter on `PRWatchReconciler`, wired from an
environment variable `PRWATCH_RATE_LIMIT_FLOOR` in `cmd/odonian/main.go`
with a default of 1500. A value of 0 disables the check. Document it in
`docs/configuration.md` alongside the other reconciler settings.

The remaining-quota check is injectable on the reconciler struct in the
same way `getPRState` and `getReviewDecision` are, so tests can drive it
without an HTTP server.

### Quieter "no forge token" warning

Keep the per-pass `skippedPRsByOwner` tally. Remember, on the reconciler,
the count logged for each owner last time. Emit the WARN when an owner is
seen for the first time or when its count differs from the remembered
value; stay silent otherwise. When an owner that was previously reported
drops to zero skipped checks (a token was added), log one INFO saying so
and forget the owner.

## Acceptance criteria

1. A superseded task whose pull request GitHub reports as `merged` or
   `closed` has its `pr` link tombstoned after one reconcile pass, and a
   second pass makes no GitHub call for it. The existing test
   `TestRetrofitSkipsAlreadyClosedOrMergedSupersededPR` is extended to
   assert both.
2. A superseded task whose open pull request the retrofit closes has its
   link tombstoned in the same pass; a second pass makes no GitHub call for
   it. If the close call fails, the link is not tombstoned and the next pass
   retries.
3. With the remaining-quota check reporting a value below the floor, the
   reconciler makes zero pull-request calls for that owner in that pass,
   enters backoff until the reported reset, and the backoff persists across
   passes until then (reuse the shape of
   `TestRateLimitBackoffPersistsAcrossReconcilePasses`).
4. With the floor set to 0 the check is never called.
5. The "no forge token" WARN appears once when an owner first lacks a
   token, again only when its skipped count changes, and an INFO appears
   once when the owner gains a token.
6. `go test ./...` passes; `go vet ./...` is clean.
7. After deploy, the server log shows no
   `entering rate limit backoff` line for a full hour and the fleet's
   reviewers stop logging `API rate limit exceeded`.

## Constraints and gotchas

- Tombstone means "do not poll again", nothing more. It must never be read
  as "the pull request is gone" by any new code, and no code path may
  un-tombstone.
- The retrofit and the main pass share one `checkBackoff`/`enterBackoff`
  pair and one per-owner map. The budget floor must feed that mechanism,
  not add a parallel one.
- `GET /rate_limit` is free but it is still a network call. One per owner
  per pass, taken lazily on first use, never one per task.
- The reconcile runner is single-threaded and runs reconcilers in
  sequence, so per-pass state on the reconciler struct needs no locking.
- Tests for the retrofit path use a real SQLite store and an `httptest`
  server (see `newRetrofitTestServer` and
  `newRealTestStoreWithProject` in `reconciler_test.go`); follow that
  pattern rather than the `fakeTaskSource` used by the main-pass tests, so
  the tombstone write is exercised end to end.

## Rollout

Deploy the server. The first pass after deploy checks every existing
terminal link once and tombstones it; from then on the retrofit pass costs
nothing until the next supersession. Watch the server log for one full
hour for the absence of `entering rate limit backoff`, then confirm a
reviewer run completes `gh pr diff` without a 403.

Separately, and not part of this spec: the ArgoCD application `odonian`
is OutOfSync because `cp/odonian/deployment.yaml` in the manifests
repository still pins `v0.16.0` while the live pod runs `v0.16.2`. Bump the
pin before the next release so a manual sync cannot roll back the WAL
reader-pool fix.
