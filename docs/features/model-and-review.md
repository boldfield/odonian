# Feature: Model assignment & review tasks

Status: feature spec, 2026-06-06
Kind: feature_spec (for project Odonian)

## What this is

Two coupled changes that turn Odonian from a single-pool work queue into a
**model-aware** one, and make code review a **first-class unit of work** instead of an
out-of-band human action:

1. **Model assignment.** Every task carries a `model` — a free-form string (e.g. `haiku`,
   `sonnet`, `opus`, `gpt-5`) **validated at create time against a per-deployment allowlist**
   (config, see below) — and a claim is **server-enforced** to match: a worker declares its
   model when claiming, and the atomic claim only succeeds if the task's model matches. No
   model can pick up another model's work. The allowlist keeps the field open to any provider
   (so a non-Claude reviewer is possible) while catching typos that would otherwise make a
   task unclaimable forever. This is the substrate for running a fixed fleet (e.g. 2 Haiku +
   1 Opus) where each worker drains only its own lane.

2. **Review as a task `kind`.** Review stops being "a human hits `/review` then `/transition`"
   and becomes real work (`kind = review`) that a reviewer worker **claims, performs, and
   completes** through the same claim/lease/event machinery as implementation work. When an
   implement task enters `review`, a review task is **auto-spawned per required reviewer**
   (the implement task's `review_models` list, default `["opus"]`) — all at once, immediately
   claimable by the matching model. The reviewers run **in parallel**, and the parent advances
   only when **all** of them have verdicted (**wait-for-all**, see below).

These are one feature because review tasks *are* model-assigned tasks — the review worker is
just an Opus worker draining review-kind work. Building model assignment without review-as-a-
task would leave the Opus reviewer with nothing model-matched to claim.

This feature is **server-only** (`internal/store` + `internal/api` + a migration). The
headless workers that drive this loop (Feature 2) and the TUI lanes that visualize it are
separate, downstream features. This feature ships behind the existing API with backward-
compatible defaults so the current single-pool flow keeps working until workers are pointed
at it.

## Why now / what it unlocks

- **Feature 2 (headless pull workers)** needs the `model` field and the model-matched claim
  to exist before model-pinned `claude -p` loops can self-serve their own lane.
- **Feature 3 (design+breakdown skill)** assigns a model per task during decomposition; it
  needs the field to assign to.
- It moves the strict two-pass review (Opus reviewer → human merge) from a manual ritual into
  enforced board state, while **preserving the human merge gate** (see the `approved` state
  below).

## The two task kinds

A new column `kind ∈ {implement, review}` (default `implement`) splits tasks into two
lifecycles. Everything else (claim, lease, heartbeat, events, links, deps, model-match) is
shared.

### implement tasks (the existing lifecycle, plus one new state)

```
backlog → ready → in_progress → review → approved → done
                                   │          │
                                   │ (reject) │ (human reject)
                                   ▼          ▼
                                 ready      ready
        (any active state) → blocked | failed
```

- Unchanged through `review`. The new state is **`approved`**, between `review` and `done`.
- A review worker's **approve** moves the parent `review → approved`. **`approved` is an
  explicit "ready to merge" lane the human drains** — the human merges the PR and transitions
  `approved → done`. This is the merge gate: **the Opus worker can approve but cannot merge.**
- A review worker's **reject** moves the parent `review → ready` (auto-bounce for rework, no
  human in the loop).
- The human may also **reject from `approved`** (`approved → ready`) if they disagree with the
  Opus approval — the final gate stays with the human.

### review tasks (reduced lifecycle, never self-review)

```
ready → in_progress → done
   (any active state) → blocked | failed
```

- `kind = review`, `target_task_id` → the implement task it reviews, `model` = one entry from
  that implement task's `review_models` list, `review_round` = the parent's current review
  round.
- Never enters `review` itself — no infinite regress.
- Auto-spawned in state `ready` (immediately claimable, no deps) when its target enters
  `review`. **One review task per entry in `review_models`, all spawned at once** (parallel
  reviewers). A single-element list (the common case) is just N=1.
- **Fresh review round per review cycle.** On reject → rework → resubmit, the prior round's
  review tasks stay `done` (audit trail) and a *new* round (incremented `review_round`) of
  review tasks spawns. The review lane is the full verdict history across rounds.
- The review worker completes it by **submitting a verdict** (below), which transitions that
  review task to `done`. The parent moves only once **all** review tasks of the current round
  have verdicted (wait-for-all aggregation), all in the verdict-submitting transaction.

## Data model changes (migration 0003)

Four new columns on `task`, plus a widened `state` CHECK (the reason this is a table rebuild,
not `ADD COLUMN`):

| Column | Type | Notes |
|--------|------|-------|
| `model` | `TEXT NOT NULL DEFAULT 'haiku'` | Free-form; **no DB CHECK** — validated at create time against the deployment allowlist (the set is operational config, not schema). |
| `kind` | `TEXT NOT NULL DEFAULT 'implement'` | `CHECK (kind IN ('implement','review'))` |
| `review_models` | `TEXT` (nullable) | implement tasks only; JSON-encoded ordered list of model strings, e.g. `["opus","sonnet"]`. Default `["opus"]` applied at spawn if null/empty. Each entry validated against the allowlist at create time. |
| `review_round` | `INTEGER NOT NULL DEFAULT 0` | review tasks: the cycle they belong to. Implement tasks: their current/last review round (incremented each entry to `review`). |
| `target_task_id` | `TEXT` (nullable) | review tasks only; FK → `task(id)`. Implement tasks: null. |

- `state` CHECK widens to include `approved`:
  `CHECK (state IN ('backlog','ready','in_progress','review','approved','done','blocked','failed'))`.
- New index for the model-matched claimable query:
  `idx_task_claimable ON task(project_id, state, model)` (the existing
  `idx_task_project_state` can be dropped or kept; the claim predicate also filters `kind`
  via the spawned review task's own row, so `(project_id, state, model)` covers the hot path).
- **Migration mechanics:** The new `state` value `approved` requires widening the `state` CHECK,
  and SQLite cannot alter a CHECK in place — so it needs a **table rebuild** (new table → copy
  rows → drop old → rename → recreate indexes). The catch that blocked the first attempt: a
  rebuild dropping a table referenced by foreign keys needs FK enforcement OFF, and
  `PRAGMA foreign_keys` is a **no-op inside a transaction** — but the runner (`migrate()`,
  ~`store.go:103`) applies every migration inside one transaction with FKs forced ON by the DSN.
  **Resolution (re-scoped 2026-06-06):** task **MR-1a** changes the runner to disable FK
  enforcement around migration application (`foreign_keys=OFF` before the tx, `foreign_key_check`
  before commit, `ON` after); then the rebuild migration **MR-1b** follows the standard procedure
  with a round-trip test. The new columns are added separately by `ALTER TABLE ADD COLUMN`
  (no rebuild) in **MR-2**.

### Struct / input changes

- `Task`, `TaskWithDepsAndLinks`: add `Model`, `Kind`, `ReviewModels []string`, `ReviewRound
  int`, `TargetTaskID *string`. JSON fields `model`, `kind`, `review_models`, `review_round`,
  `target_task_id`. (`ReviewModels` marshals from the JSON `TEXT` column.)
- `TaskInput` (bulk create): add `Model` (required; validated against the allowlist) and
  `ReviewModels []string` (optional; each entry allowlist-validated; defaults to `["opus"]` at
  review-spawn time if unset/empty). Review tasks are **never** created via the public create
  endpoint — only auto-spawned — so `TaskInput` has no `kind`/`target_task_id`/`review_round`.
- `TaskListFilter`: add `Model *string`.

## Model-matched claim (server-enforced)

- The claim payload gains a required `model` field alongside `agent_id`.
- `claimableSQL` (`store.go:840`) gains `AND model = ?`. The claim's conditional `UPDATE`
  (`ClaimTask`, `store.go:912`) binds the worker's declared model; a mismatch yields
  `rowsAffected == 0`.
- The existing 0-rows cause-determination block (`store.go:950`) must distinguish **model
  mismatch** from a generic not-claimable conflict, so a worker gets an actionable error: if
  the task exists and is otherwise claimable but `model != declared`, return a typed conflict
  with code `MODEL_MISMATCH` and a message naming both models. Otherwise the existing
  `ErrConflict`. (The API maps both to 409; the body carries the code.)
- `ListTasks` claimable filter (`store.go:866`) also takes `model`, so a worker polls
  `GET /projects/{id}/tasks?claimable=true&model=haiku` and sees only its own claimable work,
  of either kind.

### Allowlist configuration

- The set of valid model strings is **deployment config**, not schema — e.g. an
  `ODONIAN_MODELS` env var / config key holding a comma-separated set
  (`haiku,sonnet,opus,gpt-5`). Sensible default if unset: `haiku,sonnet,opus`.
- **Validation happens at create time only**: `CreateTasks` rejects (400, `UNKNOWN_MODEL`) any
  task `model` or `review_models` entry not in the allowlist. The claim path does no allowlist
  check — it matches by equality, and since `task.model` was validated at creation, an
  allowlisted task is matchable and a typo never reaches the claim.
- Adding a provider later is a config change (extend the allowlist) + standing up a worker
  that declares that model — **no migration**.

## Review auto-spawn + verdict (the core new flow)

### Spawn (all reviewers at once)

`SubmitTask` (`store.go:1589`) becomes `kind`-aware. For an **implement** task, after the
existing `in_progress → review` update and link insertion, **in the same transaction**:

- Increment the parent's `review_round` (call it `R`).
- For **each** entry in `COALESCE(parent.review_models, ["opus"])`, insert a review task:
  `kind='review'`, `state='ready'`, `model = <that entry>`, `review_round = R`,
  `target_task_id = parent.id`, same `project_id`/`document_id`, `title = "Review: " +
  parent.title + " [" + model + "]"`, `spec =` a generated brief (the strict-review standing
  prompt + a pointer to the parent's PR link).
- Append a `spawn_review` event on the parent (note = the round + the model set).

All review tasks of round `R` are immediately claimable, each by its matching model; they
proceed concurrently.

### Verdict (wait-for-all aggregation)

A review worker completes its review task by submitting a verdict. For a **review** task,
`SubmitTask` instead does the following **in one transaction**:

- Validate: payload requires `verdict ∈ {approve, reject}` and an optional `note`/`result`
  (the review writeup; also posted by the worker as a PR comment out of band). For implement
  tasks `verdict` is forbidden; for review tasks it's required (`ValidationError` otherwise).
- Transition **this** review task `in_progress → done`, store the writeup in its `result`.
- Append a `review` event **on the parent** (`target_task_id`) with the verdict and the worker
  as actor — so the parent's audit trail shows every verdict (mirrors today's `AddReview`).
- **Aggregate the current round** (`aggregateReviewRound`, `store.go:1915`). Count the parent's review tasks where `review_round = parent.review_round`: `N` total, `done` count, `approve` count (via their review events or a
  verdict column — see note). Then:
  - **`done < N`** (siblings still reviewing) → leave the parent in `review`; do nothing else.
    This worker was not the last; another verdict tx will finish the round.
  - **`done == N` and all approve** → parent `review → approved` (awaits the human merge gate).
  - **`done == N` and ≥1 reject** → parent `review → ready` (rework; the implementer re-claims
    the *same* branch/PR). All reviewers' writeups are available as consolidated feedback.

Because every write goes through the single pooled connection (`SetMaxOpenConns(1)`) and the
aggregation count runs **inside** the verdict-writing transaction, the last verdict's tx sees
all siblings already committed — there is no stuck-parent race and no lost update. Reviewers
that crash mid-review just let their lease expire and become reclaimable by another same-model
worker (existing lease machinery; no new code).

> **Note — how the aggregation reads verdicts.** The count needs each review task's verdict.
> Two clean options, to settle in the breakdown: (a) read the `review` events on the parent for
> round `R` (no schema add, but couples aggregation to the event log), or (b) add a nullable
> `verdict` column on the review task set at `done` (one more column on the rebuild we're doing
> anyway; simpler aggregation `SELECT count, sum(verdict='approve') FROM task WHERE
> target_task_id=? AND review_round=? AND state='done'`). **Leaning (b)** — cheaper query, and
> the verdict is naturally an attribute of the review task.

### Human merge gate (transition rules)

`TransitionTask` (`store.go:2155`) gains the `approved` state:

- `approved → done`: allowed (the human merge — replaces today's "approve event required from
  review" check; the `approved` state itself is the gate, since reaching it already required a
  review-worker approve).
- `approved → ready`: allowed (human disagrees with the Opus approval, bounces for rework).
- `review → approved` and `review → ready` are **not** exposed as manual transitions — they
  are server-driven by the review-task verdict above (a human who wants to bypass review can
  still use `blocked`/`failed` or reject the eventual `approved`).
- `blocked`/`failed` unchanged (allowed from any active state, now including `approved`).

### Fate of the legacy `/review` endpoint

`AddReview` + the old `/review` → `/transition` dance is **superseded** for the agent loop by
the review-task verdict path. **Open sub-decision (flag for the joint breakdown):** keep
`/review` as a manual human override, or remove it. Leaning **keep but document as
human-only**, to avoid ripping out a working path mid-migration — the TUI's human actions move
to draining `approved` regardless. Not a blocker for this feature.

## API surface (summary of changes)

- `POST /projects/{id}/tasks` (bulk create): each task accepts `model` (required, allowlisted)
  + `review_models` (optional list, each allowlisted).
- `POST /tasks/{id}/claim`: payload adds required `model`; 409 `MODEL_MISMATCH` on mismatch.
- `POST /tasks/{id}/submit`: for review tasks, accepts `verdict` (+ note); transitions the
  review task and, on the last verdict of the round, drives the parent.
- `GET /projects/{id}/tasks`: add `model` query filter; responses include `kind`, `model`,
  `review_models`, `review_round`, `target_task_id`.
- `POST /tasks/{id}/transition`: `approved → done` and `approved → ready` now valid.
- `docs/api.md` updated for all of the above.

## Backward compatibility

- Existing tasks migrate to `kind='implement'`, `model='haiku'`, `review_models=NULL`,
  `review_round=0`, `target_task_id=NULL`. The current single Opus worker keeps working if it
  declares `model=haiku` on claim (or we backfill its in-flight tasks); the cutover to
  model-pinned workers is Feature 2's concern.
- The auto-spawn means **the moment 0003 ships, every implement task entering `review` spawns
  a review task** — so review-capable workers (or a human draining the review lane) must exist
  before, or review tasks pile up unclaimed. Sequencing note for the rollout, not a code
  dependency.

## Testing

- **Migration round-trip** (its own task): populate a DB with tasks in several states + deps +
  links + events, run 0003, assert every row preserved, defaults applied, FKs/indexes intact,
  and the widened CHECK accepts `approved` and rejects garbage.
- **Model-matched claim**: a `sonnet` task is unclaimable by a `haiku` declaration (409
  `MODEL_MISMATCH`), claimable by `sonnet`; the existing 20-goroutine concurrency test extends
  to mixed models (only the matching model wins; still exactly one winner).
- **Auto-spawn**: submitting an implement task with `review_models=["opus","sonnet"]` to
  `review` creates exactly two review tasks (right `model`/`target_task_id`/`document_id`/
  `review_round`); default `["opus"]` when unset creates one; resubmit after reject bumps the
  round and spawns a fresh set, leaving the prior round `done`.
- **Wait-for-all aggregation**: with two reviewers, the *first* approve leaves the parent in
  `review`; the second approve moves it to `approved`. With one reject + one approve (any
  order), once both are `done` the parent goes to `ready`. A verdict on a non-last sibling
  never moves the parent. Verdict required for review / forbidden for implement.
- **Transition gate**: `approved → done` and `approved → ready` allowed; `review → done`
  rejected (must go through `approved`).

## Task breakdown (final — 12 tasks, all Haiku)

Sized so a Haiku worker completes each in one pass with review catching only nits. **Specs
carry no code or SQL** — intent, constraints, the gotchas, pointers to existing patterns, and
acceptance criteria; the worker writes the implementation. All tasks touch `store.go`, so they
run **serially** even where the dependency graph allows branching. Settled sub-decisions baked
in: verdicts are stored in the `verdict` column (not re-derived from events); the legacy
`/review` endpoint stays as a human-only override (no task removes it).

> **Re-scope note (2026-06-06).** The original single MR-1 ("do the rebuild in migration SQL")
> was blocked: a SQLite table rebuild needs foreign keys OFF, and `PRAGMA foreign_keys` is a
> no-op inside a transaction — but the runner applies every migration inside one transaction
> with FKs forced on, so the rebuild cannot work from migration SQL alone. Re-scoped into
> **MR-1a** (runner change) + **MR-1b** (the rebuild). Downstream tasks re-point at MR-1b.

### MR-1a — Migration runner: disable foreign keys during migration application *(deps: none)*

**Intent.** Let migrations perform SQLite table rebuilds (which require foreign-key enforcement
off) without each migration managing it, and without leaving enforcement off afterward.

**Build.** Change the migration runner so foreign-key enforcement is turned OFF before the
migration transaction begins and restored to ON after it commits, with a foreign-key integrity
check before the commit that fails the run if any violation is found. (A `foreign_keys` pragma
issued inside a transaction is a no-op, and the runner currently applies all migrations inside
one transaction with FKs forced on by the DSN — which is why a rebuild that drops a referenced
table cannot work today.)

**Constraints / gotchas.** The runner is `migrate()` in `internal/store/store.go` (~`store.go:103`);
it opens a single transaction and execs each migration file. `PRAGMA foreign_keys` must be toggled
on the connection BEFORE `BEGIN`, not inside the tx. The store uses one connection
(`SetMaxOpenConns(1)`, `store.go:66`) with `foreign_keys` ON from the DSN — enforcement must end
ON for normal operation. Run SQLite's foreign-key integrity check just before commit and fail the
migration on any violation, so a bad rebuild can't silently corrupt referential integrity. This
task changes ONLY the runner — no migration files, no domain code.

**Acceptance.** All existing migrations still apply cleanly on a fresh and a populated DB. A test
shows foreign-key enforcement is OFF while migrations apply and ON again afterward, and that a
migration which would leave a dangling foreign-key reference is rejected by the integrity check
(the run fails) rather than committing corruption.

### MR-1b — Migration 0003: allow the `approved` state *(deps: MR-1a)*

**Intent.** Let a task's `state` hold the new value `approved` (the "review-approved, awaiting
human merge" lane).

**Build.** Add a migration that recreates the `task` table with a definition identical to its
current one, except the `state` CHECK constraint also permits `approved`. SQLite cannot alter a
CHECK in place, so this is a table rebuild: create the new table, copy every existing row, drop
the old table, rename the new one into place, recreate the table's indexes. All existing rows,
columns, and foreign keys must survive unchanged.

**Constraints / gotchas.** The full current table definition (and its two indexes) is in
`internal/store/migrations/0001_init.sql` — use it as the exact template and change only the
`state` CHECK. The rebuild relies on the MR-1a runner change (foreign-key enforcement is off
during migration application and integrity-checked before commit), so dropping/renaming a table
referenced by `task_dep`/`task_link`/`event` works — do NOT manage the `foreign_keys` pragma in
the migration itself. Match the existing migration file naming/format. No Go structs change.

**Acceptance.** Applies cleanly on an empty DB and one populated with tasks in several states
(with deps, links, events). After it runs, every prior row is intact, foreign keys still resolve,
and a task can be set to `approved`. A round-trip test proves row/FK preservation and that
`approved` is now accepted.

### MR-2 — Migration 0004: add the new task columns *(deps: MR-1b)*

**Intent.** Add the columns the rest of the feature needs.

**Build.** Add six columns to `task`: a model tier (text, not null, default the Haiku tier); a
`kind` discriminator (text, not null, default the implement kind); a `review_models` list of
required reviewer models stored as JSON text (nullable); a `review_round` counter (integer, not
null, default 0); a `target_task_id` reference for review tasks (text, nullable, foreign key to
a task id); and a `verdict` field for review tasks (text, nullable). Add one index supporting
model-matched claimable lookups, keyed by project, then state, then model.

**Constraints / gotchas.** SQLite adds columns in place — **no table rebuild needed here**; just
add columns and the index. Existing rows must come through carrying the defaults (implement /
Haiku tier / round 0). Follow the migration style in the existing files. No Go structs change in
this task.

**Acceptance.** Migration applies on a populated DB; every existing task ends up `kind`
implement, the Haiku model tier, `review_round` 0, and the nullable columns empty. A test
confirms the columns and defaults.

### MR-3 — Struct & query plumbing *(deps: MR-2)*

**Intent.** Surface the new columns through the Go layer so every read returns them and every
write persists them.

**Build.** Add the new fields to the task structs (`Task`, `TaskWithDepsAndLinks`) and to the
create/list inputs (`TaskInput`, `TaskListFilter`), with sensible JSON names matching the column
intent. The review-models list marshals to/from its JSON text column. Then thread the fields
through **every** place that selects, inserts, or scans a task row.

**Constraints / gotchas.** Every task-returning query re-selects the full column set and scans
into a struct — miss one and it fails at runtime. The sites to update are all in `store.go`:
`GetTask`, `ListTasks`, `ClaimTask`, `HeartbeatTask`, `PromoteTask`, `SubmitTask`,
`TransitionTask`, and `CreateTasks`. This task only plumbs the fields through reads/writes with
their defaults — it does **not** add validation (MR-4), the claim match (MR-5), spawning (MR-7),
or any new behavior. Newly created tasks still default to implement/Haiku.

**Acceptance.** Creating a task and fetching/listing it returns the new fields (with defaults);
all existing store tests still pass; a test asserts the fields round-trip through create →
get/list.

### MR-4 — Allowlist + create-time model validation *(deps: MR-3)*

**Intent.** Make a task's model a free-form string constrained to a per-deployment allowlist, so
any provider is possible but typos are caught at creation.

**Build.** Read the set of valid model strings from deployment config (an `ODONIAN_MODELS`
setting; sensible default of the three Claude tiers when unset). In task creation, validate the
task's `model` and every entry of its `review_models` against that set; reject unknown values
with a clear client error (`UNKNOWN_MODEL`). Accept `model` and `review_models` on the create
request.

**Constraints / gotchas.** Validation is **create-time only** — the claim path does no allowlist
check (MR-5). Follow the existing config-loading pattern used elsewhere in the server, and the
existing client-error convention (the `ValidationError`/`invalid(...)` pattern in `store.go`,
mapped to 400 by the API). Default the allowlist sensibly so existing deployments keep working.

**Acceptance.** Creating a task with an allowlisted model succeeds; an off-allowlist `model` or
`review_models` entry returns 400 `UNKNOWN_MODEL`; with no config set, the Claude tiers are
accepted. Tests cover accept + reject + default.

### MR-5 — Model-matched claim *(deps: MR-3)*

**Intent.** A worker claims only tasks whose model matches the model it declares.

**Build.** The claim request gains a required `model` field. The atomic claim must succeed only
when the task's model equals the declared model, in addition to the existing claimable
conditions. When a claim fails specifically because the model didn't match (the task is
otherwise claimable), return a distinct conflict identifying it as a model mismatch
(`MODEL_MISMATCH`); other failures keep their current behavior.

**Constraints / gotchas.** The claimable condition is the shared predicate `claimableSQL`
(`store.go:840`), reused by both the claim and the claimable list — extend it consistently. The
atomic single-statement claim is `ClaimTask` (`store.go:901`); preserve its exactly-one-winner
guarantee — the model match is an added condition, not a second step. The post-failure
cause-determination block (`store.go:950`) is where the mismatch-vs-other-conflict distinction
goes. Keep matching as simple string equality.

**Acceptance.** A task with a given model is claimable by a worker declaring that model and not
by one declaring another (which gets 409 `MODEL_MISMATCH`); the existing concurrency test,
extended to mixed models, still yields exactly one winner and only among matching-model
claimants.

### MR-6 — List filter by model *(deps: MR-3)*

**Intent.** Let a worker poll for just its own claimable work.

**Build.** Add a `model` filter to the task list (a `model` query parameter on the list
endpoint) that restricts results to that model, composing with the existing state/claimable
filters.

**Constraints / gotchas.** Small, isolated change — mirror how the existing filters (`state`,
`assignee`, `claimable`) are handled in `ListTasks` (`store.go:850`) and parsed in the API
layer.

**Acceptance.** Listing with `model=` returns only that model's tasks; combined with
`claimable=true` it returns only that model's claimable tasks; a test covers the combination.

### MR-7 — Review auto-spawn on submit *(deps: MR-4)*

**Intent.** When an implement task goes to review, automatically create the review work for each
required reviewer.

**Build.** When an implement task is submitted into `review`, in the same transaction: bump the
implement task's `review_round`; then for each entry in its `review_models` list (defaulting to
a single Opus reviewer when the list is empty/unset), create a review task — `kind` review, state
`ready`, the entry as its model, the current round, its `target_task_id` pointing at the implement
task, the same project and document, a title marking it a review of the parent, and a spec that
is a strict-review brief pointing the reviewer at the parent's PR link. Record an event on the
implement task noting the review round was spawned.

**Constraints / gotchas.** This is the implement-kind branch of `SubmitTask` (`store.go:1121`) —
add the spawning to the existing submit flow without disturbing the current
in_progress→review transition and link handling; everything stays in the one transaction.
Review tasks are created here only, never via the public create endpoint. Don't implement the
reviewer's verdict here (MR-8).

**Acceptance.** Submitting an implement task with two required reviewers creates exactly two
ready, model-matched review tasks for the current round, each linked to the parent; with no
reviewers specified, exactly one Opus review task is created; resubmitting after a bounce
creates a fresh round and leaves the prior round's review tasks untouched. Tests cover the
multi-reviewer and default-single cases.

### MR-8 — Review verdict submission *(deps: MR-7)*

**Intent.** Let a reviewer worker complete its review task by recording a verdict.

**Build.** When a **review** task is submitted, it carries a verdict of approve or reject (plus an
optional written note/result). The submit, in one transaction: stores the verdict on the review
task and moves the review task to `done`; and records a review event on the **parent** (the
`target_task_id`) carrying the verdict and the reviewer as actor. Do not move the parent here —
that is the aggregation step (MR-9); for now the parent stays in `review`.

**Constraints / gotchas.** This is the review-kind branch of `SubmitTask` — review tasks have the
reduced lifecycle and go straight to `done` (they never enter `review`). A verdict is **required**
for review tasks and **forbidden** for implement tasks (reject with a clear client error
otherwise). Mirror the existing event-append pattern (`AddReview`, `store.go:1270`) for recording
the verdict event on the parent.

**Acceptance.** Submitting a review task with approve/reject moves that review task to `done`,
stores the verdict, and appends a verdict event on the parent; a verdict on an implement task is
rejected; a missing verdict on a review task is rejected; the parent remains in `review`. Tests
cover approve, reject, and both validation errors.

### MR-9 — Wait-for-all aggregation *(deps: MR-8)*

**Intent.** Move the parent only once all of the current round's reviewers have verdicted.

**Build.** Extend the review-verdict submission (same transaction as MR-8) so that, after
recording this verdict, it tallies the parent's review tasks for the current round: how many
exist, how many are done, and how many of those approved. Then: if not all are done yet, leave
the parent in `review`; if all are done and all approved, move the parent to `approved`; if all
are done and at least one rejected, move the parent to `ready` for rework (the prior round's
review tasks and their notes remain as the consolidated feedback record).

**Constraints / gotchas.** The tally must run **inside the same transaction** as the verdict
write — that, plus the single-writer connection (`SetMaxOpenConns(1)`, `store.go:66`), is what
makes the "last verdict wins" aggregation race-free; do not add locks or a separate pass. Scope
the tally to the parent and the current `review_round` only, so prior rounds don't count.

**Acceptance.** With two reviewers: the first approve leaves the parent in `review`, the second
approve moves it to `approved`. With any reject, once all are done the parent goes to `ready`. A
verdict that is not the round's last never moves the parent. Tests cover the partial round, the
all-approve completion, and the reject outcome.

### MR-10 — The `approved` lane transitions *(deps: MR-3)*

**Intent.** Make `approved` the human merge gate.

**Build.** In the task transition rules, allow `approved → done` (the human merges the PR, then
marks it done) and `approved → ready` (the human overrides an approval and sends it back for
rework). Reaching `done` now comes from `approved` rather than from `review`.

**Constraints / gotchas.** This changes the existing transition rules in `TransitionTask`
(`store.go:1322`): today `done` is reachable from `review` when an approve event exists — that
path is **replaced** by `approved → done` (being in `approved` already implies a passed review,
so no event re-check is needed). Keep the `blocked`/`failed` transitions working from active
states (now including `approved`). The legacy `/review` endpoint is left in place untouched as a
human override. Note: this transiently means a human can no longer mark a task done straight from
`review` — by design, the path is now via `approved`.

**Acceptance.** A task in `approved` can be transitioned to `done` and to `ready`; a task in
`review` can no longer be transitioned straight to `done`; `blocked`/`failed` still work. Tests
cover the new transitions and the removed one.

### MR-11 — Worker runbook + API docs *(deps: MR-5, MR-6, MR-9, MR-10)*

**Intent.** Document the new contract for the agents and humans who drive it.

**Build.** Update `AGENT-API.md` for: declaring a model when claiming, polling claimable work by
model, the implement-vs-review task kinds and how a worker branches on them, and how a reviewer
submits a verdict. Update `docs/api.md` for the changed request/response shapes (model and
review-models on create, model on claim, verdict on review submit, the `model` list filter, the
new `approved` transitions, and the new task fields in responses).

**Constraints / gotchas.** Docs only — no code. Keep the voice and structure of the existing
files. Describe behavior, not implementation.

**Acceptance.** A reader can, from the docs alone, run a model-pinned worker loop (claim by
model, branch on kind, implement-and-submit or review-and-verdict) and a human can drain the
`approved` lane. The API doc matches the shipped endpoints.

## Feature-level acceptance

- A task created with `model=sonnet` is claimable only by a worker declaring `sonnet`; a
  mismatched claim returns 409 `MODEL_MISMATCH`; a task created with an off-allowlist model
  is rejected at create with `UNKNOWN_MODEL`.
- Submitting an implement task with `review_models=["opus","sonnet"]` to `review` auto-spawns
  two model-matched review tasks (round 0); `["opus"]`/default spawns one.
- The parent reaches `approved` only after **all** current-round reviewers approve; the human
  transitions `approved → done`. If **any** reviewer rejects (once all are done), the parent
  goes to `ready`, and the next submit spawns a fresh round.
- Existing data migrates cleanly (round-trip test green); the current flow is unbroken with
  default `model=haiku` / `kind=implement`.
- Uses only `internal/store` + `internal/api`; no worker or TUI code in this feature.

## Out of scope (downstream features)

- **Headless model-pinned workers** (the `claude -p` loops) — Feature 2.
- **TUI review-queue lane + `approved` lane** — a TUI follow-on (the current TUI's `a`/`x`
  review actions will need to move to draining `approved`; until then the human drains via API).
- **Design+breakdown skill** assigning models — Feature 3.
