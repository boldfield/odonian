# Odonian — MVP Design

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Historical MVP design.**
> The approved June design is preserved below, including its original non-goals. It is not the current feature inventory: the repository now includes a [TUI](cmd/odonian-tui/), [research support](docs/research.md), automated review and configurable merge handling. Start with [current documentation](docs/README.md) and the [API reference](docs/api.md).

Status: MVP design, approved 2026-06-04
Owner: brian@oldfield.io

## 1. What this is

Odonian is an **API-only coordination substrate for a pool of AI agents draining a
backlog of work.** It is not a kanban product. "Kanban" is the human's mental model of
the underlying machinery: a work queue with a state machine, atomic task claiming, and
crash recovery. Agents do not look at a board; they call an API.

It exists to power this workflow:

1. Iterate on a design idea (with a strong model, e.g. Opus).
2. Formalize the idea into a design document that captures every salient, to-be-built aspect.
3. Decompose the design into **bite-size tasks** — each well-scoped enough that a senior
   engineer would hand it to a junior for execution.
4. Track those tasks on a per-project board with a fixed set of states.
5. A pool of execution agents (e.g. Haiku) claim tasks, move them through states, execute,
   and submit for review.

### Non-goals (MVP)

- No board UI / visualization / WIP limits / drag-drop. The pretty board is a later read-model.
- No document storage / CMS. Designs live in their project's git repo; Odonian stores refs.
- No repo provisioning. Creating repos is a human step, not an Odonian feature.
- No horizontal scaling. Single replica, SQLite. (See Deployment.)
- No multi-approval policy engine. Human is the review gate for the MVP.

## 2. Concepts / data model

```
Project    id, name, repo (target code repo URL), created_at
Document   id, project_id, kind (design | feature_spec), title,
           ref (repo-relative path or URL), commit (nullable pin), created_at, updated_at
Task       id, project_id, document_id, title, spec, state,
           assignee, lease_expires_at, result, created_at, updated_at
TaskDep    task_id, depends_on_id                         -- the DAG edges
TaskLink   task_id, kind (pr|branch|commit|ci), value     -- index on (kind, value)
Event      id, task_id, actor, kind, verdict, note, created_at  -- append-only audit/spine
```

- A `Project` maps to one code repo, known at registration (the repo already exists by then).
- A `Project` has **one** `design` document (the base) and **many** `feature_spec` documents.
- Design content lives in the project's repo (`DESIGN.md`, `docs/features/*.md`). Odonian
  stores only `ref`. The `Document` table is the **central index** across all projects —
  central discovery without central storage.
- `Task.document_id` ties a task back to the design/feature it was decomposed from.
- `Event` is the spine: every claim, heartbeat, transition, and review is an immutable event.
  Reviews are events, not a separate table.
- `TaskLink` is typed and indexed on `(kind, value)` to enable reverse lookup — e.g. a CI
  webhook arrives knowing a commit SHA and must find the task.

## 3. State machine

```
backlog ──promote──► ready ──claim──► in_progress ──submit──► review ──approve──► done
                       ▲                    │                     │
                       └──── lease expiry ──┘             reject──┘ (→ ready)

blocked / failed are off-ramps from any active state.
```

Two independent gates govern claimability (this is the "promotion gate + dependency DAG"
combination):

- **Promotion gate**: the task must be in `ready` — the planner/human has blessed it.
- **Dependency gate**: every task in its `depends_on` set must be `done`.

A task is **claimable** iff both gates hold AND no live lease exists.

## 4. Atomic claim — the only hard part

Everything else is CRUD. Claiming is one conditional, transactional `UPDATE`:

```sql
UPDATE task
SET state='in_progress', assignee=:agent, lease_expires_at=:lease, updated_at=:now
WHERE id=:id
  AND state='ready'
  AND (lease_expires_at IS NULL OR lease_expires_at < :now)        -- lazy lease expiry
  AND NOT EXISTS (                                                  -- dependency gate
        SELECT 1 FROM task_dep d JOIN task t2 ON t2.id = d.depends_on_id
        WHERE d.task_id = task.id AND t2.state != 'done');
```

`rowsAffected == 1` → claim won. `0` → lost the race or not actually claimable → `409`.
That single statement is the entire concurrency-control story: no locks, no broker, no
sweeper.

### Leases / crash recovery

An execution agent will die mid-task. `in_progress` tasks carry `lease_expires_at`; agents
heartbeat to extend it. Expiry is checked **lazily** inside the claim query above — a dead
agent's task silently becomes claimable again. No background sweeper for the MVP (target
concurrency is 2–5 agents).

## 5. Review

The MVP enforces a **human gate**: a person makes the `review → done` call. An Opus reviewer
agent (and later CI) participate by posting verdicts, but the human decides.

Every reviewer — human, agent, CI — posts the **same shape**: an `Event` with
`kind='review', verdict='approve'|'reject'`. This generalizes cleanly:

- Future: flip the gate rule from "human approves" to "agent approves AND a CI `approve`
  event exists." Same event stream, different rule — config, not architecture.
- A CI webhook (`POST /webhooks/ci {commit, status}`) resolves `commit → task` via the
  `task_link` index, then posts an `approve`/`reject` event as `actor='ci'`.

Reject → task returns to `ready`.

Reviewers **discover** work by listing: `GET /projects/{id}/tasks?state=review`. The MVP
does not make reviewers *claim* review tasks (one Opus reviewer + a human gate → double
review is cheap, not incorrect). A reviewer pool can reuse the claim machinery later.

## 6. API surface

```
POST /projects                                 create project (name, repo)
GET  /projects/{id}
POST /projects/{id}/documents                  register a design/feature_spec ref
GET  /projects/{id}/documents
POST /projects/{id}/tasks                       bulk create tasks (+ depends_on, document_id)
GET  /projects/{id}/tasks?state=&assignee=&claimable=
GET  /tasks/{id}
POST /tasks/{id}/promote                        backlog → ready
POST /tasks/{id}/claim          {agent_id}      atomic; 409 on loss
POST /tasks/{id}/heartbeat                       extend lease
POST /tasks/{id}/submit         {result, links[]}   in_progress → review
POST /tasks/{id}/review         {verdict, note} post a review verdict event
POST /tasks/{id}/transition     {to, note}      generic: done / blocked / failed
POST /webhooks/ci               {commit,status} (future) CI verdict
```

Auth: a single service bearer token. Agents self-report `agent_id` (a string) in the claim
body — no per-agent identity for the MVP.

## 7. Storage & deployment

- **Language:** Go 1.25. Stdlib `net/http` with Go 1.22+ routing feature; minimal deps.
- **Store:** SQLite via `modernc.org/sqlite` (pure Go, no cgo) so the container is a static
  binary on scratch/distroless. WAL mode.
- **k8s:** single-replica `Deployment` (or `StatefulSet`), PVC on `local-path-storage`.
  SQLite is single-writer and the PV is node-local, so **`replicas: 1` is mandatory** —
  two replicas would corrupt the DB. Horizontal scaling is explicitly out of scope until
  the storage layer is swapped (kept behind an interface to make that cheap).

## 8. The workflow this enables

**Greenfield project**
1. Human creates a project dir.
2. Brainstorm the design → write it down, or ditch it.
3. Human creates the repo (local + remote).
4. Commit `DESIGN.md` in the repo.
5. Decompose → register Project (with repo), Document (`kind=design`), and Tasks.
6. Agents drain the board.

**New feature on an existing project** — identical, minus repo creation:
1. Brainstorm the feature → `docs/features/<feature>.md`, or ditch.
2. Commit it.
3. Decompose → register a Document (`kind=feature_spec`, same `project_id`) + Tasks.
4. Agents drain the board.

## 9. Bootstrap plan

Odonian is built using a minimalist, text-file kanban (see `board/`) so we dogfood the
exact mental model before the system exists. During bootstrap there is no real concurrency
(one human + one assistant), so the atomic-claim problem does not yet exist — which is why
a text-file board is a fine bootstrap and a deliberately bad final design.

- `board/{backlog,ready,in_progress,review,done}/` — one markdown file per task.
- Task frontmatter: `id, title, state, document, depends_on`. Body: spec + acceptance criteria.
- "Moving" a task = `git mv` between state dirs.
- `DESIGN.md` (this file) is the base design document.

Once the MVP runs, we register Odonian itself as a Project in Odonian and migrate the
remaining work onto the real board.
