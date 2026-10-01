---
name: review
description: Use to drive the human review gate from inside an interactive `claude` session — show the queue of tasks awaiting a decision, show the diff for one of them, and, on the human's explicit instruction, record their verdict by running `odonian approve`/`reject`. The skill NEVER decides on its own — the human supplies the judgment, the CLI does the mechanics. Triggers on requests like "what's waiting for review?", "show me the review queue", "let me review the board", "show me the diff for <task>", "approve <task>", or "reject <task> because …".
---

# Odonian review (human gate)

A conversational wrapper for the **human review gate**. The sandbox's only interface is an
interactive `claude` session, so this skill is how a human inspects what is waiting, reads a diff,
and then — once they have made the call — records that verdict on the board. It surfaces the queue
and the diff so the human can decide, then executes the decision they hand back.

## The one rule that overrides everything

**The human decides; this skill only records.** It never forms its own opinion of a diff and never
approves or rejects on its own initiative. It runs `odonian approve`/`reject` **only** when the
human gives an explicit instruction to do so ("approve it", "reject it because X"). The skill is
the steward of the board state, not the reviewer: the CLI does the mechanics (transition, freeze,
cleanup), and the human supplies the judgment. If you are unsure whether the human has actually
decided, ask — do not guess and do not act.

## When to use it

- The human wants to see what work is awaiting a decision ("what's in the review queue?").
- The human wants to read the diff for a specific pending task before deciding.
- The human has decided and wants the verdict recorded ("approve it", "reject it, the tests are
  flaky").
- As the human-facing companion to the model-pinned fleet: Haiku implements, the Opus reviewer
  worker verdicts review-kind tasks, and the human drains the `approved` lane. This skill is how
  the human looks before they leap **and** how they record the leap once they take it.

## Phase 0 — Configuration

- `ODONIAN_URL` and `ODONIAN_TOKEN` must be set (the running Odonian instance + bearer token);
  the CLI reads them from the environment. If either is missing, ask the human.
- You need the **project id**. Use `$ODONIAN_PROJECT` if it is set; otherwise ask the human, or
  run `odonian projects` to list them and let the human pick.
- In `local_commit` mode (`ODONIAN_DELIVERY_MODE=local_commit`) the approve/reject mechanics touch
  the git repo, so the repo directory must be resolvable — `$ODONIAN_REPO` (or a `--repo <dir>`
  the human supplies). If it is unset when an action needs it, the CLI errors; ask the human.

## Phase 1 — Show the queue

Run:

```
odonian pending --project <project-id>
```

This lists every task in the **`review`** or **`approved`** state for that project — the work
waiting on a decision. The output is a table of `ID  STATE  KIND  TITLE` (truncated 8-char ids);
add `--json` for the full records. `review` rows are awaiting a reviewer verdict; `approved` rows
have passed review and await the human's merge.

Present this queue to the human and ask which task they want to inspect.

## Phase 2 — Show the diff

For the task the human chose, run:

```
odonian diff <task-id>
```

What it shows depends on the delivery mode:

- **`pull_request` mode** (default): prints the PR URL, then — if `gh` is on PATH — the PR diff
  (`gh pr diff <url>`), best-effort.
- **`local_commit` mode** (`ODONIAN_DELIVERY_MODE=local_commit`): shows the diff of the task's
  `commit` link against its parent — the tip the task started from — so a task sharing a branch
  shows only its own change, not earlier tasks' already-approved work. Add `--full` to show the
  entire commit instead of just the diff. The repo directory comes from `--repo <dir>` or, if unset,
  `$ODONIAN_REPO`.

Useful flags:

- `--full` — full commit rather than diff-against-base (local_commit mode).
- `--repo <dir>` — repository directory (local_commit mode; defaults to `$ODONIAN_REPO`).

**Show the RAW diff, verbatim — never summarize, analyze, or judge it.** Output exactly what
`odonian diff` prints, so the human reads the actual changes. Do **NOT** replace the diff with a
bulleted summary, a "what this adds" checklist, or any assessment ("looks good", "the defect is
fixed", "this is the clean scaffold the reviewers wanted") — that is forming an opinion of the diff,
which this skill never does. The human reads the diff and decides; your job is to put the real bytes
in front of them, not a précis of them. If the diff is long, say so and offer to page it, but still
show it. The only acceptable framing is neutral orientation the human asked for (e.g. "commit
`<sha>`, N files") — never an evaluation of the change.

## Phase 3 — Record the human's decision

This is the action step. Run it **only** on the human's explicit instruction, and run **exactly**
the verb their instruction maps to — never substitute your own judgment for theirs.

### Approve

When the human says to approve a task (one in the **`approved`** state — it has passed reviewer
verdict and is on the human's merge lane):

```
odonian approve <task-id>
```

- The task must be in `approved`; `approve` errors otherwise (except a `done` task in
  `local_commit` mode, below).
- **`pull_request` mode**: transitions `approved → done`. (The human still does the actual PR merge
  out of band; the skill only records the state.)
- **`local_commit` mode**: prepares the task's landing on its MR branch `wi/<slug>`, reserves the
  task for landing (only if it is still approved in the review round it prepared), moves the
  branch, and only then transitions `approved → done`; then it removes the item's worktree and WIP
  branch `wip/<id>`. In this mode, **approve = freeze**: there is no separate merge step, and
  nothing lands on `main`. `<slug>` is the task's `branch` when set (shared across tasks), else its
  title's slug. Approve holds a per-branch lock throughout, so a second approve on the same branch
  refuses with `another approve is landing work on wi/<slug>`; wait for the first to finish, then
  re-run it. If the task was reworked and re-approved while the gate ran, approve refuses with
  `STALE_REVIEW_ROUND`; re-run it to land the new version.
  - If nothing else landed on the branch since the task started, it **fast-forwards** (exactly the
    reviewed commit; quick).
  - Otherwise it **merges** the task in and first runs the repo's `make check` and `make test` on
    the merged result — this can take minutes, so say so, and **run the command with a 600-second
    tool timeout** (the gate's own default limit is 8 minutes; `--gate-timeout` changes it). The
    gate runs the tasks' code, so it gets a scrubbed environment and **refuses to run outside a
    sandbox**; `--allow-host-gate` overrides that. Never add `--allow-host-gate` on your own —
    only when the human explicitly says to run the gate on their machine.
  - A merge conflict, a failing gate, or a gate that cannot run (missing target, timeout, no
    sandbox) refuses the approve **before** transitioning: the task stays `approved` and the
    branch is unchanged. So does a reject that lands while the gate runs. The error says which it was. Relay it to the human verbatim; the recovery
    is their call (for a conflict or a real failure, typically `reject --abandon` and re-running
    the task from the current branch — a plain `reject` would rework on the same stale base).
- The TUI cannot approve in `local_commit` mode (it cannot land the work); approvals go through
  `odonian approve`. The board enforces this: a plain `transition <id> done` on such a task is
  refused with `LANDING_REQUIRED`.
- If the merge gate fails because the sandbox's toolchain needs an environment variable the gate
  does not pass through (it keeps the common Go/Rust/Python/Node/Java ones), name it in
  `ODONIAN_GATE_ENV` and re-run approve.

### Reject

When the human says to reject a task (one in **`review`** or **`approved`**), you must have a
reason from them — `--note` is required:

```
odonian reject <task-id> --note "<the human's reason>"
```

- Without `--abandon`, this transitions the task to **`ready`** for rework (the implementer
  re-claims it; a fresh review round spawns on resubmit).
- With `--abandon`, it transitions the task to **`failed`** instead, and in `local_commit` mode
  also cleans up the item's worktree and `wip/<id>` branch (the `wi/<slug>` MR branch is left
  intact). Use this only when the human wants to drop the work entirely, not send it back:

  ```
  odonian reject <task-id> --note "<reason>" --abandon
  ```

## The approve = freeze footgun (local_commit mode)

`approve` moves the MR branch only after reserving the task, and marks it `done` only after
moving the branch. A failure before the branch moves — the MR branch checked out somewhere, a
merge conflict, a failing merge gate, a reject or rework during the gate (`STALE_REVIEW_ROUND`) —
leaves the task `approved` and the branch untouched; fix the cause the message names and re-run
plain `odonian approve <task-id>`. The checked-out case surfaces as:

```
MR branch wi/<slug> is checked out at <path>; cd out or run 'git checkout --detach' there, then re-approve
```

Once approve has reserved the task for landing, the task can only become `done`: it cannot be
rejected, and its dependents stay blocked until the work is on the branch. If approve is
interrupted after that point (a crash, a lost response, a failed done transition), the error says
so and the task stays reserved. **Recovery — re-run plain `odonian approve <task-id>`**: it resumes
(landing is a no-op once the work is on the branch) and marks the task done. If the human instead
wants to drop the approval, `odonian approve <task-id> --cancel-landing` releases the reservation,
but only if the work never reached the branch; a `reject` of a reserved task fails with
`LANDING_IN_PROGRESS` until then.

Plain `approve` on a task that is already `done` but still has its `wip/<id>` branch (its cleanup
failed) finishes the job, landing only the commit the final review round approved: it refuses if
`wip/<id>` has moved off that commit, and if the final round was a **no-op** it lands nothing and
leaves `wip/<id>` alone (it can only hold work that was never approved; tell the human it is there).
With nothing left it says so and succeeds. (`--freeze-only` does the same but refuses a task that
is not yet `done`.)

A task reworked **before** links recorded their review round has several commit links and no way
to tell which one was reviewed; `approve` and `diff` refuse it. The fix is a fresh submission:
`reject` it (with a note saying why) so the implementer re-submits and the round is recorded.

## The gates you do not cross

- **When asked to show a diff, you show the diff — raw and verbatim.** You never summarize it,
  characterize its quality, list what it "adds", or say whether it passes. Presenting is not
  assessing; "show me the diff" means show the diff, not your reading of it.
- You never form or impose your own verdict. You run `approve`/`reject` only to record a decision
  the human has explicitly stated, and you never invent a `--note` reason — it comes from the human.
- In `pull_request` mode you never run the actual PR merge yourself; `approve` records the board
  state, the human merges the PR.
- You touch the repo only through the `odonian` verbs (`diff`, `approve`, `reject`); you never
  hand-edit branches, worktrees, or commits.
