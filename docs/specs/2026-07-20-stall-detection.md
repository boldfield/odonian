# Feature spec — board stall detection (alert when claimable work stops moving)

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Not implemented in this baseline.**
> The [notification reconciler](../../internal/notify/reconciler.go) still checks approved/blocked/failed tasks; it does not implement the project-level claimable-work stall alert proposed below. Do not rely on ODONIAN_STALL_THRESHOLD or an automatic board-stall page. See [operations](../operations.md) for current monitoring and recovery.

**Status:** board milestone (Odonian project). **Date:** 2026-07-20.

## Problem

On 2026-07-09 the review queue deadlocked and **nothing alerted for 10 days.**

The trigger was a revoked codex refresh token: every `gpt-5.5` review dispatch
exited `rc=1`. The amplifier was that `harness/agent.sh` peeks the queue head
*without claiming it* (`odonian next`, no `--claim`), so the failing task stayed
`ready` and stayed the head forever. Four reviewer pods re-dispatched the same
doomed task ~3120 times each. An `opus` review sitting three minutes behind it in
the `ORDER BY created_at, id` queue was unreachable the entire time, and implement
tasks dep-blocked behind those reviews made healthy workers correctly log
"no claimable work in any project".

**Nothing entered `blocked` or `failed`.** Every task sat in `ready`. The existing
`NotifyReconciler` (`internal/notify/reconciler.go`) only fires on
`approved`/`blocked`/`failed`, so it had nothing to report. A fully deadlocked
board is byte-for-byte indistinguishable from an idle healthy one.

Fixing the head-of-line block (tracked separately) prevents *this* failure mode.
This spec is the safety net that catches the class: any future cause of "work
exists but nothing is moving" should page within hours, not days.

## Goal

When a project has claimable work but no task in it has changed state within a
threshold, publish a notification. Re-alert on a slow cadence rather than every
reconcile tick.

## Signal — and why the obvious one is wrong

The tempting signal is "age of the oldest claimable task", derived from
`created_at`. **It false-positives.** A task created ten days ago whose
dependencies only cleared this morning is legitimately claimable-for-one-minute
but looks ten days stale. `updated_at` does not rescue it: when a dependency
completes, the *dependent's* `updated_at` is never touched, so a task can become
claimable without any timestamp of its own moving.

The correct signal is board-level, and is a conjunction:

```
stalled(project) :=
      count(claimable tasks in project) > 0
  AND now - max(updated_at over all non-terminal tasks in project) > threshold
```

Both halves matter. Claimable-work-exists alone is just a backlog. No-recent-
progress alone is just an idle or finished board. Only together do they mean
"there is work to do and nobody is doing it" — which is exactly the incident.

Terminal states (`done`, `abandoned`, `superseded`) are excluded from the
`max(updated_at)` so a project that finished cleanly never alerts.

## Approach

No schema change and no store change. `ListTasks` already supports the
`Claimable` filter (`internal/store/store.go`, `claimableSQL`), and the existing
`taskSource` interface in `internal/notify/reconciler.go` already exposes
`ListProjects` + `ListTasks`. A new reconciler slots into the existing
`reconcile.Runner` alongside `NotifyReconciler`.

- **`buildStallNotification`** — pure function, mirroring `buildNotification` in
  `internal/notify/rules.go`. Takes the project, the stall duration, and the
  claimable count; returns a `Notification`. Priority P1 (this is an outage, and
  the P-convention maps to ntfy priority). Topic follows the established DASHED
  convention (ntfy 404s on dots).
- **`StallReconciler`** — implements `reconcile.Reconciler`. Per project:
  list claimable tasks; if none, skip. Otherwise list non-terminal tasks, take
  `max(updated_at)`, compare against the threshold, publish if exceeded. Clock
  injected as `now func() time.Time` for tests, matching `NewNotifyReconciler`.
- **Suppression.** `NotifyReconciler` has **no dedup today** — it republishes on
  every tick for every matching task. That is tolerable for `approved` (the task
  moves on) but unusable for a stall that persists for days. `StallReconciler`
  keeps an in-memory `map[projectID]lastAlertedAt` and re-alerts only once per
  `ODONIAN_STALL_REALERT`. In-memory is sufficient: the server is single-replica
  (`replicas: 1`, `Recreate`), and a restart re-alerting once is harmless.

## Config

| env | default | meaning |
|---|---|---|
| `ODONIAN_STALL_THRESHOLD` | `6h` | no progress for this long (with claimable work present) = stalled. `0` disables. |
| `ODONIAN_STALL_REALERT` | `24h` | minimum gap between repeat alerts for the same project. |

Both parse with `time.ParseDuration`, matching the existing duration env vars.
The reconciler registers only when `NOTIFY_URL` is set, matching how
`NotifyReconciler` is wired in `cmd/odonian/main.go`.

## Acceptance

- A project with claimable work and no state change for > threshold produces
  exactly one notification, then no more until `ODONIAN_STALL_REALERT` elapses.
- A project with claimable work that *is* progressing produces none.
- A project with no claimable work produces none, however old it is.
- A task that becomes claimable via a dependency completing does **not** trigger
  an alert on the strength of its own `created_at` — this is the regression test
  for the wrong-signal trap above.
- A fully `done` project never alerts.
- `ODONIAN_STALL_THRESHOLD=0` disables the reconciler entirely.

## Out of scope

- Fixing the head-of-line block itself (server-side dispatch-failure accounting:
  agent reports `rc=1` → per-task failure counter → auto-`blocked` after N).
  Decided, deferred to the next round.
- Backend preflight in `agent.sh` (dropped — largely subsumed once failure
  accounting lands).
- Per-kind or per-model stall granularity. Project-level is enough to page a
  human, who can then diagnose.
