---
name: odonian-ops
description: Use to diagnose and safely operate a live Odonian board — figure out why a task is
  wedged and, only on the human's explicit instruction, remediate it via the odonian CLI
  (transition, promote) or archive API/TUI actions. Read to diagnose freely; mutate only when told, verify the
  precondition before any irreversible action, never touch tokens. Triggers on "why is <task>
  stuck?", "unstick <task>", "sweep for zombie merge tasks", "this lease expired but didn't release",
  "anything wedged in review?", "promote <task>", "move <task> to <state>". For just *viewing* the
  board, use odonian-board.
---

# Odonian ops (diagnose · operate)

The wrench for a live Odonian board: diagnose a wedged task and fix it. `odonian-board` is the
dashboard (read-only); this skill is what you reach for when something on it is broken. It reads to
diagnose, then — on the human's word — changes board state through the CLI or documented API/TUI actions.

## The one rule that overrides everything

**Read freely; the human gates every mutation.** Listing, inspecting, and diagnosing are always
fine. Anything that changes board state — `transition`, `promote`, `archive`, create — happens
**only on an explicit human instruction**, and only after you've stated what you're about to do and
why. Before any *irreversible or shared-state* action, **verify the precondition yourself and say
what you found**, then wait for the go-ahead. When unsure whether the human has actually decided,
ask — don't guess and don't act.

## Phase 0 — Configuration

- `ODONIAN_URL` + `ODONIAN_TOKEN` in the environment; the CLI reads them. If missing, ask.
- **Never print, echo, or log a token value** — not the bearer token, not a forge token. Reference
  secrets by env var; never `cat`/`cut` a secrets file into output.
- To *see* the board while diagnosing, use **`odonian-board`** (or its reads: `odonian projects`,
  `odonian tasks --project <id>`, `odonian show <id>`). How to read state for diagnosis:
  `in_progress` + a past `lease_expires_at` = a stuck-task candidate; `review` + a climbing
  `review_round` = a possible reject loop; a non-`done` `kind=merge` task = check it didn't zombie.

## Diagnose & remediate — playbooks

Each entry is **symptom → check → action → guardrail**. Never skip the check; never act past the
guardrail.

### Zombie merge task (merged PR, task stuck `in_progress`)

- **Symptom:** a `kind=merge` task sits `in_progress`; its parent/target task is already `done`.
- **Check:** confirm the PR is actually merged — find the parent's `pr` link, then
  `gh pr view <pr-url> --json state,mergedAt` must show it **MERGED**; confirm the parent is `done`.
- **Action:** `odonian transition <merge-task-id> --to done --note "PR already merged; finalizing zombie merge task"`.
- **Guardrail:** only after the PR is confirmed merged. Never force a merge task to `done` on
  assumption — if the PR isn't merged, the merge genuinely didn't happen.

### Sweep for stuck merge tasks (board-wide)

- **Symptom:** "are any merges wedged?" / a recurrence check after a fleet incident.
- **Check:** list every non-`done` `kind=merge` task across the relevant projects. For each, run the
  zombie check above against its PR before touching it.
- **Action:** finalize only the confirmed-merged zombies; **report — don't touch — the rest**.
- **Guardrail:** **log what you skipped and why.** Never silently "fix all" — a non-merged merge task
  may be legitimately in progress.

### Expired lease not reclaimed

- **Symptom:** a task is `in_progress` with `lease_expires_at` in the past and no recent `updated_at`
  — stranded, no agent finishing it.
- **Check:** confirm the lease is genuinely expired and that no agent is actively working it (state
  hasn't advanced, no fresh heartbeat).
- **Action:** no state transition is needed: expired `in_progress` tasks are already eligible for
  reclaim once dependencies are done and the task is not held. Check claimable discovery, the
  task's model, worker availability, and (for research) admission status to explain why no worker
  has picked it up. A direct `in_progress` → `ready` transition is not supported.
- **Guardrail:** don't yank a task whose lease is still live — you'd race a working agent and double
  up the work. The bundled harness renews owned leases in the background; an expired lease alone
  does not prove the worker has exited. Diagnose before proposing any mutation.

### Reject loop (task bouncing review → ready)

- **Symptom:** `review_round` keeps climbing with no convergence; the same task re-enters review
  repeatedly.
- **Check:** read the latest reviewer notes against the task spec. Is the spec demanding more than
  the reviewer will ever pass? Is there a spec ↔ reviewer mismatch, or a genuinely failing build?
- **Action:** surface the mismatch to the human. The real fix is a **spec edit** or a **model-tier
  decision** — both the human's call. Do not silently keep re-dispatching.
- **Guardrail:** never "resolve" a reject loop by approving around the reviewer or forcing the task
  past review. The reviewer's verdict is not yours to override.

### Task wedged in an unexpected state

- **Symptom:** a task sits somewhere it shouldn't (e.g. `in_progress` long after its work merged, or
  `approved` that should have shipped).
- **Check:** `odonian show <id>` — read its links, deps, and history; reconcile against reality
  (the PR, the repo) before concluding it's stuck.
- **Action:** propose the specific transition that reflects reality; execute only on instruction.
- **Guardrail:** verify against the *real* artifact (PR/repo), not the board's stale view, before
  moving anything.

### Archiving

There is no `odonian archive` CLI verb. After the human explicitly authorizes archiving a verified
dead item, use `POST /projects/{id}/archive` or `POST /tasks/{id}/archive`, or the matching TUI
archive action. The corresponding `/unarchive` endpoints reverse it. See the
[API reference](../../docs/api.md). Archiving changes default visibility; it is not a worker stop
or lease-revocation mechanism.

## Hard guardrails (the do-not list)

- **Never archive a project or task you didn't create or can't confirm is dead.** Archive reads as
  destructive even though it's soft server-side; **show what it is and confirm first**. (Note:
  archive is *soft* — an archived project is still claimable by id, so archiving does **not** stop a
  fleet from draining it. If the goal is "stop work," that's a different action.)
- **The human is the merge gate.** Never merge or drive a task to `done` except the
  confirmed-zombie-merge finalize above.
- **Verify before you mutate.** Re-`show` the task immediately before any transition and act on its
  *current* state — board reads go stale fast under a live fleet.
- **One thing at a time, stated out loud.** Announce each mutation and its reason before running it;
  no batch "cleanups" that the human didn't see item by item.
- **Never print tokens.**
