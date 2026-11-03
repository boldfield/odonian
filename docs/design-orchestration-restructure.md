# Design: Foreman Design-Orchestration Restructure

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Historical cross-repository design; implementation only partly verified here.**
> The Odonian [design prompt](../harness/prompts/pull_request/design/implement.md) contains the contract core described here, but this checkout does not establish the status of Foreman changes or the full cross-repository rollout. The original proposal and task plan remain below. Use [current documentation](README.md) for supported Odonian behavior.

**Status:** design — 2026-06-10. Spans two repos: `boldfield/odonian` (substrate + harness) and
`boldfield/foreman` (pipeline). All open calls **locked**: server-side merge primitive; scaffold
pushes Makefile/CI in the initial commit (repo created pre-design); Option B (generic extensible
prompt + per-consumer extension); per-repo fixture copy (chosen for task isolation); merge-task model
inherits the parent, claimed by `kind`. Pending: morning review, then decomposition.

## Why

P4.1/P4.2 wired "design = a board task" but never reconciled it with (a) Foreman's phase sequence —
the design task needs the project + repo that `setup` creates *afterward*, so it is unrunnable; or
(b) the harness design prompts — a different `DESIGN.md` schema and a JSON-header extraction that the
worker never emits. Root cause: the work was **decomposed without a binding shared contract**, so
each task author invented their own. This design fixes that by pinning every cross-task contract
FIRST, then carving tasks that each produce or consume one contract.

## Design rule (the whole point)

> Every seam that crosses a task boundary is a **contract** in §1. A task produces or consumes a §1
> contract and is **verifiable in isolation** against it (via a shared fixture), so tasks compose
> without their authors coordinating. No task may invent a cross-component shape; if it needs one
> that isn't in §1, §1 is amended first.

---

## 1. Contracts (the spine)

### 1.1 The `DESIGN.md` document contract

A `track=design` worker produces `DESIGN.md` at repo root. It has a **generic contract core** (owned
by the harness prompt, tool-agnostic) plus optional **consumer-extension** sections (supplied by the
task spec). Every consumer parses by **literal `## ` heading string** — those strings are load-bearing.

**Contract core (always present, in this order):**

| Heading (literal) | Content | Parsed by |
|---|---|---|
| `# <repo-slug>` | H1 + one-line summary | — |
| `## Charter` | one sentence: purpose + primary user + ONE headline use case | review |
| `## Command Surface` | every command, flag, argument | **decompose** |
| `## Output schema/format` | exact output shape (not assumed JSON) | review |
| `## Default no-flag behavior` | must demonstrate the headline; worked example | review |
| `## Canonical invocations` | 3–5 runnable examples | — |
| `## Acceptance criteria` | `- ` bullets, each bound to exactly one command/flag | **completion, decompose** |
| `## Coherence requirements` | the 4-check block, verbatim | review |

**Foreman consumer-extension (appended; supplied by the design task spec, NOT the harness prompt):**
`## Problem` · `## Goals / Non-goals` · `## Hermetic build constraints` (Makefile `check/test/build/run`
+ a named `make run` fixture) · `## Test expectations` (unit/integration/e2e).

**Parsing rules (so consumers are fixture-testable):**
- A section = lines under its `## ` heading until the next `## ` (H3 `### ` does not close it).
- Acceptance bullets: `- `, `* `, or numbered (matches completion's `extractAcceptanceCriteria`, #108).
- **No JSON header anywhere.** `state.Design` is the raw merged markdown.

**Shared fixture:** `testdata/design-canonical.md` — a fully-conforming `DESIGN.md`. Lives in BOTH
repos (each carries its own copy derived from this table; the duplication is intentional, it keeps
every consumer task self-contained). Every consumer test asserts against this fixture, so a consumer
is verified **before any real worker exists**.

### 1.2 Merge-task contract (odonian substrate)

A `merge` task is to `approved → done` what a `review` task is to `review → approved`: a spawned,
claimable child that drives exactly one parent edge.

**Trigger (server, atomic).** Inside the review-aggregation transaction, when a parent transitions to
`approved` **and** `agent_merge=true` **and** the parent has a resolvable `pr` link:
`INSERT task {kind:"merge", project_id:parent.project_id, target_task_id:parent.id,
model:parent.model, state:"ready", title:"Merge: <parent.title>"}`.
- `agent_merge=false` → no merge task (human gate; parent waits at `approved`, unchanged).
- no PR / `no_op` link → existing auto-finalize takes `approved → done` directly; no merge task.

**Merge task invariants:** exactly one per parent; never reviewed; never spawns children; `target_task_id`
= parent. The parent state machine is **unchanged** (`review → approved → done`).

**CLI verb `odonian merge <merge-task-id>`** (mechanical, tested Go; all merge correctness lives
here — pinned signatures in §1.5). The merger claims a merge task, then acts on it *by its own id*:
1. `GetTask(merge-task-id)` → `target_task_id` is the parent; `GetTask(parent)`. Require parent
   `state==approved`, `agent_merge==true`, resolvable PR (link or deterministic branch `mr/<id8>`).
2. Squash-merge via the shared **`internal/forge`** helper (per-owner token + REST `PUT …/merge`) —
   extracted from the TUI by task **M0** so the verb and TUI share one implementation, not two.
3. `TransitionTask(parent, "done")`, then `TransitionTask(merge-task-id, "done")`.
- On ANY failure: non-zero exit; parent stays `approved`; the merge task stays claimable → **retryable**.

**Claim filter:** `odonian next --kind merge` returns `ready` merge tasks, matched on `kind` only
(model tier ignored — the merger is LLM-free).

**Harness:** `agent.sh --kind merge` is a NON-LLM loop — `next --kind merge` → `claim` →
`odonian merge <parent>` → stop. No `claude` dispatch. Runs as a dedicated slot or folded into
existing agents.

**`review.md` change:** delete the inline-merge step (today's step 5). Reviewers only vote.

### 1.3 Foreman `ProjectState` I/O contract

The inter-phase contract is the set of named `ProjectState` fields each phase reads/writes.

| Phase | Reads | Writes | Side effects |
|---|---|---|---|
| **scaffold** (NEW, pre-design) | candidate | `ProjectID`, `DocumentID`, `Metadata["repo_title"]` (= `slugify(candidate.Name)`), `Metadata["built_repo_path"]` | create repo `<owner>/<slug>` with **initial commit = README + canonical Makefile + CI** (no `DESIGN.md`); register Odonian project + design doc. **Use `repo_title` — the existing key every downstream phase reads (queue/tagrelease/abandon/discover); `repo_slug` is read by nothing.** |
| **design** | `ProjectID`, `DocumentID`, candidate | `Design` (merged `DESIGN.md` text), `Metadata[design_task_id]` | seed `track=design` task; poll to `done`; read merged `DESIGN.md` file |
| **design-gate** | `Design` | (gate verdict) | — |
| **decompose** | `Design`, `ProjectID` | (build tasks) | seed build tasks `depends_on` the contract |

- `Metadata["repo_title"] = slugify(candidate.Name)` (NEW mode) / `= target_repo` (EXTEND mode) — the repo-name key every downstream phase reads. `slugify` is the pinned deterministic helper (§1.5, F1).
- `Design` = merged `DESIGN.md` file content, **no JSON header**.

### 1.4 Harness prompt contract (Option B — generic core, layered extension)

- `implement.md`: contract-core template (§1.1) + "ALSO include any additional sections your task
  spec requires" (extensible). Tool-agnostic; no Foreman/pipeline knowledge.
- `review.md`: coherence rubric on the contract core; **tolerant of extra sections**; no merge (§1.2).
- `design-gen.md` (the Foreman design-task SPEC, not a competing template): inject the candidate JSON;
  "design this per your design-worker instructions"; **require** the Foreman extension sections (§1.1);
  no JSON header, no rival template.

### 1.5 Inter-module operation contracts

The §1.1–1.4 data contracts let a *parser* be tested in isolation; they do **not** pin the *calls*
across task boundaries. Below is every cross-boundary operation with its exact signature. **NEW** =
this design defines it; **EXISTING** = the current declaration, which the task author calls verbatim
(never re-invents). Pinning these surfaced three things the data contracts hid — noted inline.

**Odonian — merge primitive**

| Op (task) | Pinned signature | Producer → Consumer |
|---|---|---|
| Merge-task spawn (M1) | inside `SubmitTask` aggregation (`store.go` ~L1789): when `newParentState=="approved" && parentAgentMerge && hasPR`, `INSERT INTO task(… kind='merge', target_task_id=<parent>, state='ready', model=<parentModel>, title='Merge: '+title …)` in the **same tx** (mirror the spawn_review INSERT ~L969). Sits right beside the existing `hasNoOp && !hasPR → 'done'` branch. | M1 → M2,M3 |
| Claimable (M2) | `odonian next --kind merge` → `GET /projects?claimable=true&kind=merge`; returns `ready`, dep-free merge tasks; merger ignores model tier. exit 0 (id) / 2 (none). | M2 → M4 |
| **Forge helper (M0, NEW — the hidden seam)** | extract `internal/forge`: `OwnerToken(owner string) (string, error)` + `SquashMerge(ctx, owner, repo string, prNumber int, token string) error` (REST `PUT /repos/{o}/{r}/pulls/{n}/merge`). **This logic currently lives only inside `cmd/odonian-tui`** — without extracting it, the `merge` verb either can't merge or forks a second copy. M3 depends on it; the TUI refactors onto it. | M0 → M3, TUI |
| `merge` verb (M3) | `odonian merge <merge-task-id>`. Composes EXISTING `tuiclient.GetTask(ctx,id)(TaskDetail,error)` (→ `.TargetTaskID`), `tuiclient.TransitionTask(ctx,id,to string,note *string)error`, + M0's `forge.*`. exit 0 on merge+both-done; non-zero leaves both unchanged. | M3 ← M0; → M4 |
| Merger (M4) | `agent.sh --kind merge`: `next --kind merge` → `claim <id>` → `odonian merge <id>`; no `claude`. | M4 ← M2,M3 |

`TaskDetail.TargetTaskID *string` is already present (no client change). `review.md` (S3) deletes its inline-merge step.

**Foreman — restructure**

| Op (task) | Pinned signature | Notes |
|---|---|---|
| `slugify` (F1, NEW) | `func slugify(name string) string` in `internal/founder` — lowercase, hyphenate, strip non-alnum, collapse/trim dashes; deterministic. | F1 → F2 |
| Phase enum (F3, NEW) | add `PhaseScaffold Phase = "scaffold"` to `types.go`. | — |
| `scaffold.Execute` (F2) | implements EXISTING `PhaseHandler.Execute(ctx, *ProjectState) (Phase, error)`, returns `PhaseDesign`. Calls EXISTING `GHClientInterface.CreatePrivateRepo(ctx, owner, repo)`, `PushInitialCommit(ctx, owner, repo, repoPath, message)`; `OdonianClientInterface.CreateProject(ctx, name, repo)(*Project,error)`, `CreateDocument(ctx, projectID, kind, title, ref)(*Document,error)`. **Correction (twice over):** writes `state.Metadata["repo_title"] = repoName` + `["built_repo_path"]` — `ProjectState` has **no** struct fields for these (uses `Metadata`), AND the key is **`repo_title`**, the existing key `queue_phase.go:46` / `tagrelease_phase.go:62` / `abandon_phase.go` / `discover.go` all read (they `PhaseAbandon` if it's missing). `repo_slug` was an invented key nothing reads — using it hard-abandons the pipeline at tag-release and queue. | F2 ← F1 |
| Factory + transitions (F3) | factory `Create(PhaseScaffold)`→scaffold; `select` returns `PhaseScaffold` (was `PhaseDesign`); scaffold returns `PhaseDesign`. | F3 ← F2 |
| `readDesignFromTask` (F4) | EXISTING sig `(d *DesignPhase) readDesignFromTask(ctx, *ProjectState, *odonianclient.Task) error`; new body uses EXISTING `RepoCloner.CloneRepo(ctx, owner, repo, targetPath) error` → read `DESIGN.md` → `state.Design`; drop `task.Result`. | F4 ← S1f |
| decompose parse (F5) | replace `extractInterfaceContract` (`## Behavior`→`## Command Surface`), `validateSingleContract`, `extractDesignMarkdown` (drop JSON strip). Internal; tested vs §1.1 fixture. | F5 ← S1f |
| Remove setup (F6) | delete `PhaseSetup` + setup's creation/DESIGN.md-write (absorbed by scaffold); re-point `design-gate → decompose`. | F6 ← F2,F3 |

`odonianclient.CreateTaskRequest` (EXISTING) = `{ProjectID, DocumentID, Title, Spec, Key, DependsOn, Model, ReviewModels, AgentMerge, Track}` — the design phase already sets `Track:"design"`; decompose seeds build tasks `DependsOn` the contract.

**Cross-repo (foreman → odonian):** all via the EXISTING HTTP API (foreman's `odonianclient`): `CreateProject`, `CreateDocument`, `CreateTasks` (with `Track`), `GetTask`. The restructure adds **no** new cross-repo endpoint; the merge primitive is odonian-internal (consumed by the fleet, not foreman).

**What pinning the operations revealed (and the data contracts didn't):**
1. The `merge` verb takes the **merge-task id**, not the parent id (the merger claims a task, then acts on it) — changes the verb signature and the M4 loop.
2. `ProjectState` has **no** `RepoSlug`/`RepoOwner` — scaffold writes `Metadata`, not new struct fields.
3. A **hidden task M0**: the forge-merge helper lives only in the TUI; the verb needs it extracted to `internal/forge`, or it's a silent integration break.

---

## 2. Component changes (each keyed to a §1 contract)

**odonian**
- `internal/store`: spawn merge task in the aggregation tx (§1.2).
- `internal/store`/`internal/api`: `next --kind merge` claimable (§1.2).
- `cmd/odonian`: `merge` verb (§1.2).
- `harness/agent.sh`: `--kind merge` non-LLM branch (§1.2); `harness/merger.sh` slot.
- `harness/prompts/pull_request/design/implement.md`: extensible (§1.4); `review.md`: tolerant + drop merge (§1.2,§1.4).
- `testdata/design-canonical.md`: the fixture (§1.1).

**foreman**
- `internal/founder/scaffold.go` (NEW): split repo/project creation out of `setup`, pre-design (§1.3).
- `internal/founder/discover.go` (phase factory): re-sequence `select → scaffold → design` (§1.3).
- `internal/founder/setup.go`: remove DESIGN.md-write + repo/project creation (moved to scaffold).
- `internal/founder/design_phase.go` `readDesignFromTask`: read merged file, drop `task.Result` (§1.3).
- `internal/founder/decompose.go`: parse `## Command Surface` + `## Acceptance`; drop `## Behavior`
  gate + the JSON-header strip in `extractDesignMarkdown` (§1.1).
- slug helper + `prompts/design-gen.md` (§1.4); `testdata/design-canonical.md` (its own copy of §1.1).

---

## 3. Decomposition — isolated tasks

Each task lists: repo · the §1 contract it produces/consumes · isolated acceptance · deps. A task is
green when its acceptance holds against the §1 fixtures — no sibling task required.

**Group S — schema/contract (DEFINE FIRST; the seam → one author):**
- **S1** [odonian] add `testdata/design-canonical.md` (fixture) + heading constants. *Produces §1.1.*
  Acceptance: fixture conforms to the §1.1 table; a `schema_test` asserts each heading present. Deps: —.
- **S1f** [foreman] vendor a copy of the §1.1 fixture into `foreman/testdata/`. Deps: S1 (copy source).
- **S2** [odonian] `implement.md` extensible (§1.4). Acceptance: prompt emits the contract core +
  "include spec-required sections"; assumes nothing Foreman-specific. Deps: S1.
- **S3** [odonian] `review.md` tolerant of extra sections + remove merge step (§1.4, §1.2). Deps: S1.
- **S4** [foreman] `design-gen.md` → thin candidate + extension sections, no JSON header (§1.4). Deps: S1.

**Group M — merge primitive (odonian):**
- **M0a** [odonian] create `internal/forge` (`OwnerToken`, `SquashMerge`) by lifting the logic from
  the cited TUI source (§1.5); package unit-tested vs a mock forge. Deps: —. *(Surfaced only by the
  operation-contract pass.)*
- **M0b** [odonian] refactor `cmd/odonian-tui` to call `internal/forge`; delete the inline copies.
  Acceptance: TUI merge still works, no duplicate forge logic. Deps: M0a.
- **M1** [odonian] store: spawn merge task on `approved && agent_merge && pr` (§1.5). Acceptance:
  unit test — aggregation to `approved` w/ agent_merge spawns exactly one merge task; `agent_merge=false`
  and `no_op` spawn none. Deps: —.
- **M2** [odonian] `next --kind merge` claimable filter (§1.5). Acceptance: a `ready` merge task is
  returned by `next --kind merge`, not by `--kind review/implement`. Deps: M1 (chained — same store).
- **M3** [odonian] `odonian merge <merge-task-id>` verb (§1.5). Acceptance: against `internal/forge`
  (mock) + a hand-built merge task, resolves parent via `target_task_id` → merge → parent `done` →
  merge task `done`; failure leaves both unchanged. Deps: **M0a** (forge helper package).
- **M4** [odonian] `agent.sh --kind merge` non-LLM branch + `merger.sh` (§1.5). Deps: M2, M3.

**Group F — Foreman restructure:**
- **F1** [foreman] `slugify` helper + test. *Produces the slug contract.* Deps: —.
- **F2** [foreman] `scaffold.go`: create repo + initial commit (README/Makefile/CI) + Odonian
  project/doc; set `ProjectID/DocumentID/RepoSlug/RepoOwner` (§1.3). Deps: F1.
- **F3** [foreman] phase factory: `select → scaffold → design` re-sequence (§1.3). Deps: F2.
- **F4** [foreman] `readDesignFromTask` reads merged `DESIGN.md` file, drop `task.Result` (§1.3).
  Acceptance: given a fixture repo containing the §1.1 fixture, `state.Design` == file content. Deps: S1f.
- **F5** [foreman] `decompose` parses `## Command Surface` + `## Acceptance`; drop `## Behavior` + the
  JSON-strip (§1.1). Acceptance: against the §1.1 fixture, extracts the command surface + criteria;
  rejects a fixture missing `## Command Surface`. Deps: S1f.
- **F6** [foreman] `setup.go`: remove repo/project creation + DESIGN.md-write (now in scaffold). Deps: F2, F3.

**W1 — final validation gate (NOT a fleet/board task).** A `scaffold → design → decompose` dry-run on
a throwaway candidate, asserting a merged `DESIGN.md` and build tasks tracing to the contract. Judging
end-to-end correctness is judgment work — run by the assistant or the human after all tasks land, like
the standalone design-track validation. Deps: all.

**Model assignment:** every task above is **haiku** except the prompt-authoring tasks **S2, S3, S4
(opus)**. Standard coding tasks are sized for haiku — the §1.5 operation contracts moved the unknowns
into the spec, which is what makes them haiku-doable; the escalation ladder is the safety net if any
turns out harder. (F2 `scaffold` is the heaviest; split it if it bounces.)

## 4. Sequencing & the isolation guarantee

```
S1 ─┬─ S1f ─┬─ F4
    │       └─ F5
    ├─ S2
    ├─ S3
    └─ S4
M0a ─┬─ M0b
     └─ M3 ─┐
M1 ─ M2 ────┴─ M4
F1 ─ F2 ─ F3 ─ F6
                 └──────────────┐
S*, M*, F* ───────────────────► W1
```

- **S first** — it pins §1.1 and ships the fixture (S1/S1f) that unblocks the consumers (F4, F5).
  S2/S3/S4 are the schema seam → **one author**, not fan-out.
- **M** runs concurrently with S/F: M0a fans to M0b (TUI refactor) and M3 (the verb); M1→M2 (shared
  store) is a separate chain; M4 joins M2 + M3.
- **F** depends on S1f (F4, F5) and the slug (F1); F2→F3→F6 chained (phase wiring); F4 isolated.
- **W1** last.

**Why the pieces fit:** every consumer (F4, F5, review/decompose) is tested against the §1
contracts and the shared fixture, so each is complete and green *before* the producer (the real
design worker) exists. S, M, and F proceed in parallel once §1 + the fixture are pinned. The only
"one author" zones are the schema seam (S2/S3/S4) and the chained store/phase edits — everything else
is independently cuttable.

---

## 5. Codify the discipline in the design+breakdown skill

This document is a worked example of the decomposition pattern the planned **design+breakdown skill**
must automate, so the pattern — not just this instance — needs to live in the skill:

1. **Pin cross-task contracts first — both kinds.** Enumerate every seam that crosses a task boundary
   as an explicit contract, and pin **two kinds**: *data contracts* (shapes, file formats, state
   fields) AND *operation contracts* (the exact call signatures — function/method signatures, API
   endpoints, CLI args + exit codes). A shared data fixture only lets a *parser* be tested in
   isolation; two tasks on opposite sides of a *call* also need the signature pinned or they compile
   against guesses and don't fit. **Pin every contract name — signatures AND data-contract identifiers
   (metadata keys, section headings, struct fields) — from the real code via grep, never invent
   them.** This is the single most repeated failure: inventing `repo_slug` (the code reads
   `repo_title`), under-naming the `api.go` kind-allowlist, missing the `queue_phase` consumer.
   Doing it right corrected the merge verb's argument and a non-existent struct field, and surfaced
   the hidden forge-helper task; doing it wrong hard-abandoned the pipeline. **Grep all consumers of
   a name before pinning it, across every repo.** A task may only produce or consume a pinned contract;
   if it needs one that isn't pinned, the contract list is amended first.
2. **Ship a shared fixture per contract.** Each contract gets a concrete fixture so every *consumer*
   task is verifiable in isolation against the fixture, not against the producer.
3. **Mark the "one-author" seams.** Tasks that edit the same shared file, or that jointly define a
   contract, are chained or single-authored — never parallel fan-out. Everything else is parallelizable.
4. **The decomposition output records, per task:** the contract it produces/consumes, its isolated
   acceptance (vs the fixture), and its dependencies — exactly the columns in §3.
5. **Size every standard coding task for haiku.** Difficulty is controlled by decomposition, not by
   model tier — the escalation ladder is a safety net, not a sizing tool. If a coding task seems to
   need a stronger model, that's a signal to split it (or pin more of its contracts) until haiku can
   do it. Only judgment/authoring work (the prompts that instruct other agents) gets opus up front.
   Validation/integration milestones aren't fleet tasks at all — they're judgment, run by a human or
   the orchestrator.

The failure this prevents is the one that produced *this* restructure: tasks that were locally
well-defined but collectively incoherent because no artifact pinned the contract they all had to meet.
