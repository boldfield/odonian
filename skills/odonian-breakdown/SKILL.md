---
name: odonian-breakdown
description: >-
  Use to turn a design into an executable Odonian board — decompose it into model-pinned, bite-size tasks and register the project/document/tasks via the Odonian API. The human brings the work: ALWAYS ask what to break down — never propose topics, ideas, or features to build. Works from an existing design/feature-spec document when the human has one (the common case — go straight to decomposing it); only brainstorms a design collaboratively when they don't. Proposes and takes positions on design choices and task boundaries but STOPS for the human's decision; never finalizes alone. Triggers on "break this down for the board", "decompose this doc into Odonian tasks", "put this feature on the board", "scaffold a project from this design".
---

# Odonian design + breakdown

Drive the collaborative workflow that turns a design into an executable Odonian board. The human
brings the intent — **often as a document they already have** — and you facilitate:
**(start from their doc, or shape one with them) → decompose into bite-size, model-pinned tasks →
register project/document/tasks via the API.** A model-pinned fleet then drains the board — Haiku
implements, Opus reviews, the human merges.

## The one rule that overrides everything

**Propose, take a position, then STOP for the human's decision.** At every design choice, every
task boundary, and every task's spec you put forward a concrete recommendation with a one-line
rationale — then you wait for the human to confirm or override. You **never** finalize the design,
the task list, or any task's content on your own. This is collaborative authorship, not autonomous
generation. When in doubt, surface the choice; do not absorb it.

**But you do NOT choose what to work on.** The human brings the problem — and usually a finished
design document. **Never open by proposing ideas, topics, or features to build** — that is theirs to
name, not yours to pick. Your proposing begins only *after* the human has said what we're building,
and only on the design choices, task boundaries, and specs *within* it. If you were triggered with
nothing specific, your first move is to **ask what to break down** — not to suggest one.

## Phase 0 — Start here: ask what, and from where

**Open by asking the human what they want to put on the board. Do NOT propose ideas or topics** —
the work is theirs to name. If the skill was triggered with nothing specific, ask *"what do you want
to break down?"* and wait.

Then settle two things (ask whichever isn't already obvious from what they gave you):

**Starting point — is there a design already?**
- **From an existing document (the common case).** The human brings a design or feature-spec — a
  file in the repo, a PR, a path, or pasted prose. **Read it**, reflect back your understanding of
  the problem, the scope **and non-scope**, and the acceptance criteria, and get the human's
  confirm/correct. Then go **straight to Phase 4 (Decompose)** — skip Phases 1–2; the design exists.
  Only fill a genuine gap (e.g. missing acceptance criteria) by asking, not by inventing.
- **From an idea, no document yet.** Run Phase 1 (brainstorm) → Phase 2 (formalize a doc) → decompose.

**Target — where do the tasks land?**
- **Greenfield** — a new project + new repo + a `design` document.
- **Feature-on-existing** — a new capability on an existing Odonian project; a `feature_spec`
  document, no new repo. (Locate the existing project + repo.)

**Config:** `ODONIAN_URL` and `ODONIAN_TOKEN` must be set (the running Odonian instance + bearer
token). Confirm both; if missing, ask. Every API call sends `Authorization: Bearer $ODONIAN_TOKEN`.

**Endpoint shapes** (get these right): list/create under a project at
`$ODONIAN_URL/projects/$PROJECT/tasks` and `.../documents`; every per-task call is at the ROOT,
`$ODONIAN_URL/tasks/<id>/{promote,transition}`. Prefer `scripts/odonian.sh` over hand-built
curl — inline `jq` quoting is error-prone, and the script keeps payloads correct.

## Phase 1 — Brainstorm the design (collaborative)

> **Skip this and Phase 2 entirely when the human already has a design document** — you confirmed it
> in Phase 0; go to Phase 4. Run them only when there is no doc yet.

Iterate the problem and the shape of the solution *with* the human. Challenge assumptions, name
failure modes, rank approaches and state the trade-offs — take positions. But settle nothing
alone: each design choice is the human's call. **Write no code here** — this is intent and shape.

Output: shared agreement on the problem, the solution approach, the scope **and explicit
non-scope**, and the acceptance criteria that will define "done."

## Phase 2 — Formalize the design document

> Only when Phase 1 ran (no pre-existing doc). If the human brought a document, it already plays
> this role — register it as-is in Phase 5.

Turn the agreed design into a prose document — greenfield → `DESIGN.md`; feature-on-existing → a
feature-spec doc. It must contain:

- Problem / motivation.
- Goals and **non-goals** (the scope boundary).
- The behavior — what it does.
- **Acceptance criteria = the stopping condition**, concrete and testable.
- Constraints and gotchas.
- Test expectations (unit / integration / e2e, as the work warrants).

**Prose only, no code.** The human approves the document before anything is created.

## Phase 3 — Repo (greenfield only)

Behind a pluggable forge seam, **GitHub/`gh` is first-class.** `scripts/create-repo.sh` handles
`git init` → create the remote → commit the document. For feature-on-existing, skip this — locate
the existing repo and its Odonian project instead.

## Phase 4 — Decompose (the heart, collaborative)

Break the design into tasks. For **each** task, propose and STOP for the human:

- **Title** + a **prose spec** carrying: intent, constraints/gotchas, **pattern pointers**
  (`file:line` into existing code), and acceptance criteria.
- **Dependencies** on other tasks (by key).
- A proposed **model**, with a one-line rationale.
- An **`agent_merge`** suggestion (default `false`).

Non-negotiable decomposition rules:

- **NO code in a spec.** The spec says *what* and *why*; the implementer writes the *how*. A spec
  that contains code reduces the implementer to a paste buffer — that is contrary to the job.
- **Decompose-to-executor: every coding task is Haiku-sized — landable in one pass.** If a task is
  too big for Haiku, decompose it **finer**. Use Haiku as the initial implementation model and
  allow Odonian's configured escalation policy to move a stalled task to a stronger model.
  Set `escalate=true` when creating tasks unless the user explicitly disables escalation.
  Task sizing and initial model choice do not prohibit escalation. Keep review and merge gates
  independent of the implementation model.
- **Dependency-order to serialize file overlap.** Two tasks that touch the same file must be
  ordered by a dependency, never left concurrent — that is the merge-conflict trap. Tasks can have
  multiple dependencies, but the graph must be a **DAG**: a cycle (A→B, B→A) leaves both tasks
  permanently unclaimable, and the server rejects only self-deps, not cycles — so catch them here.
- Keep each spec to one change; prefer a `file:line` pattern-pointer over prose where it says more.

## Phase 5 — Register + hand off

Using `scripts/odonian.sh`:

1. Create the project (greenfield) or confirm the existing one.
2. Register the document (`kind` = `design` | `feature_spec`, pointing at its repo ref) — or, if the
   human's document is already registered on the board, just locate and reuse it. Don't duplicate.
3. Bulk-create the tasks — dependencies by intra-batch `key`, plus `model` and `agent_merge`.
4. **Promote milestone-by-milestone — NOT the whole backlog at once.** There is no `ready→backlog`
   demote; to halt a promoted task, transition it to `blocked` (reversible — `blocked→ready`
   re-readies it and clears its stale lease) or `failed` (terminal). Promote the first milestone,
   then the next once its predecessor has merged — this is a **human checkpoint** between milestones
   (review milestone 1's approach before milestone 2's leaves auto-flow), NOT a dependency-safety
   measure: the `claimable` filter already gates every task on its deps being `done`, regardless of
   what's been promoted.

Report the project id, document id, and task ids, and tell the human how to point the fleet at the
board (`ODONIAN_PROJECT=<id>` + the worker/reviewer loops).

## The gates you do not cross

- The human owns the **merge gate** — tasks default `agent_merge=false`; review workers approve but
  never merge.
- You never finalize a design choice, a task boundary, or a task's spec alone.
- Tasks are Haiku-sized and start on Haiku; escalation is allowed. Specs carry no code; same-file tasks are dependency-ordered.
