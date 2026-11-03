# Converge a task whose PR was merged by a human from any non-terminal state

On November 1, 2026 the owner merged two Odonian pull requests by hand while their tasks sat in `blocked`: reala.gent task `484c7771` (PR boldfield/reala.gent#207, `agent_merge=true`) and manifests task `3688f2b0` (PR boldfield/manifests#206, `agent_merge=false`). Both tasks had been auto-blocked or worker-blocked because CI was red for infrastructure reasons, and the owner used the repository admin bypass to land the already-reviewed diffs. Neither task ever reached `done`.

What happened next cost four review rounds and produced no state change:

- The operator transitioned `484c7771` to `ready` with a note explaining the merge. The worker claimed it, followed the rework rule ("`origin/mr/<TASKID8>` exists, continue the existing PR"), resubmitted with the same `pr` link, and both reviewers rejected because the review prompt requires `gh pr view` to report an OPEN PR and forbids reviewing anything else. The worker then blocked the task again with a note saying no implementer can satisfy that.
- The operator transitioned `3688f2b0` to `ready` with a note saying the post-merge evidence had already been collected and posted on the PR. The worker never saw the note: `odonian show` renders review findings from the event log and drops operator transition notes. It re-blocked within one minute, repeating the previous round's objection.

The README's failure table promises "PR merged or closed outside the board: PR-watch converges the task to `done` or `abandoned`." That is true only for tasks in `approved`. `internal/prwatch/reconciler.go` lists approved tasks only and returns early for any `agent_merge=true` task, and `store.TransitionTask` refuses `done` from every state except `approved` (and `in_progress` for merge-kind tasks). A task in `blocked`, `review`, `in_progress` or `ready` whose PR a human merges is invisible to the reconciler, and no operator transition can finish it: `blocked` goes only to `ready` or `failed`, and `abandoned` means "PR closed unmerged" and is reachable only from `approved`. The board therefore has no truthful terminal state for merged work unless the task happened to be `approved` at the moment of the merge.

This spec has two independent tasks. Task A fixes convergence on the server. Task B fixes the worker's view and the prompts so that the next time an operator intervenes, the worker acts on the operator's instruction and takes the no-op path instead of burning a round.

## Task A: PR-watch converges a merged PR from any non-terminal state

### Required behavior

For every project, the PR-watch reconciler also examines tasks in `ready`, `in_progress`, `review` and `blocked` that carry an active (non-tombstoned) `pr` link. For those tasks only the merged outcome applies: when the forge reports the PR as merged, the task converges to `done`. The open-with-changes-requested bounce and the closed-unmerged abandon remain restricted to `approved`, exactly as today.

The merged outcome applies regardless of `agent_merge`. A PR merged by a human is merged; the merger's own path is already documented as idempotent on an already-merged PR, so converging here cannot race it into an error.

Add a store method for this path rather than widening `TransitionTask`. In one transaction it must:

- move the task from `ready`, `in_progress`, `review`, `blocked` or `approved` to `done`, clearing `assignee` and `lease_expires_at`;
- retire every review child of the current round that has no recorded verdict to `abandoned`, each with a transition event noting "retired: parent PR merged outside the board", following the pattern `reshapeOpenReviewRound` already uses; a reviewer's later verdict on a retired child is refused as it is today;
- append a `transition` event on the parent, actor `system`, whose note names the PR URL and that it was merged outside the board;
- leave dependents claimable on the next `claimable=true` listing, since the dependency is now `done`.

Do not materialize research continuation children on this path; that stays tied to the `approved` to `done` transition, and the spec for continuation manifests already says children exist only after the parent's verified human merge through that gate. Record in the event note that no continuation was materialized if the parent opted in.

The operator `POST /tasks/{id}/transition` endpoint keeps its current rules. This is a reconciler path, not a new operator verb; an operator who wants a blocked task finished merges its PR and lets the next tick converge it.

Update the README failure table so the promise matches the behavior, the transition list in `docs/api.md`, and the "Unblocking" section of `AGENT-API.md` to say that a blocked task whose PR has been merged needs no unblock: PR-watch finishes it.

### Verification

Store tests for the new method from each of the five source states, covering: assignee and lease cleared, open review children retired with their events while a child that already recorded a verdict is untouched, parent event note content, a dependent `ready` task becoming claimable, refusal from `done`, `failed`, `superseded` and `abandoned`, and no continuation children for an opted-in research parent. Reconciler tests in `internal/prwatch` for a `blocked` task with a merged PR and `agent_merge=true`, a `review` task with a merged PR and two in-flight review children, an `in_progress` task with a merged PR, and controls showing a `blocked` task with an OPEN or CLOSED PR is left alone. Reuse the existing fake forge in `reconciler_test.go`. `make check` and `make test` pass.

## Task B: the worker sees operator notes and takes the no-op path for a merged PR

### Required behavior

`odonian show <id>` currently fetches the event log for rework tasks and extracts review findings only. It must also render the notes of `transition` events that occurred after the task's most recent `submit` event (or since creation when there is none), oldest first, under a heading "Operator notes" in the text output and as an `operator_notes` array in the JSON output, each entry carrying the event timestamp and note. Notes with no text are omitted. This is where an operator's unblock explanation reaches the worker.

In `harness/prompts/pull_request/build/implement.md`, the rework branch of step 4 must begin by checking the recorded PR before continuing it: `odonian show` gives the `pr` link; `gh pr view <url> --json state,mergeCommit` gives its state. If the state is MERGED, the worker does not rework, does not push, and does not resubmit the `pr` link. It fetches `origin/main`, confirms the merge commit is an ancestor of `origin/main`, and goes straight to the no-op submit in step 9 with `--no-op`, a result that names the PR and the merge commit, and the merge commit as the no-op value. If the state is CLOSED and not merged, the worker blocks with a note naming the PR; it does not open a second PR for the same task. Step 7 gains "Operator notes" from `odonian show` as an additional source the worker must reconcile before acting, listed ahead of the recorded review findings because it is newer.

In `harness/prompts/pull_request/build/review.md`, the open-PR guard in step 3 currently treats any non-OPEN state as a fabricated or premature link. Change it so a MERGED PR is handled explicitly: verify the merge commit is on `origin/main`, review that commit's diff against the spec as the merged result, and approve or reject on the acceptance criteria. Only a PR that does not resolve at all, or a CLOSED unmerged PR, remains a defect. State in the prompt that once Task A of this spec is deployed, a merged PR will normally have been converged before any reviewer sees it, so this branch is a backstop.

Apply the same two prompt edits to the `design` and `research` variants under `harness/prompts/pull_request/` where the identical paragraphs exist, so the three prompt families do not drift. In `AGENT-API.md`, the rework rule ("continue the existing PR, don't open a new one") gets the exception for a merged PR, and the no-op section says explicitly that it covers the task's own PR having been merged by a human.

### Verification

CLI tests for `odonian show` text and JSON output with: a transition note after the last submit (rendered), a transition note before the last submit (not rendered), an empty note (omitted), and a task with no submit events (all notes rendered). Prompt changes are reviewed by reading; `harness/scripts_test.go` and any prompt lint in `make check` must still pass. `make check` and `make test` pass.

## Execution

Both tasks begin on Haiku with escalation enabled, independent Opus and GPT review, and `agent_merge=false`; a human merges each PR. They touch disjoint files (Task A: `internal/prwatch`, `internal/store`, README, `docs/api.md`, the Unblocking section of `AGENT-API.md`; Task B: `cmd/odonian`, `harness/prompts`, the rework and no-op sections of `AGENT-API.md`) and carry no dependency on each other. Both edit `AGENT-API.md` in different sections; whichever lands second syncs with `main` before submitting, per the standard rework step.
