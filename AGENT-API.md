# Execution agent runbook (live API)

You are an execution agent draining a project's backlog **through the live Odonian API**
(v0.2.0+). You claim, work, and submit over HTTP, not by moving files. (For the legacy text-file
board used to bootstrap the MVP, see `AGENT.md`.)

Two things are central in v0.2.0:

- **Tasks are model-pinned.** Every task has a `model` tier (`haiku` / `sonnet` / `opus` / any value
  in the deployment allowlist). You declare your model on claim and may only claim tasks whose
  `model` matches yours.
- **Review is a kind of task.** When an implement task is submitted to `review`, the server
  **auto-spawns a `review`-kind task per reviewer**. Reviewers are model-pinned workers (typically
  `opus`) that claim those review tasks and return a verdict. Most of this runbook is the implement
  loop; the **Review tasks** section covers the reviewer loop.

## Configuration

From the environment:

- `ODONIAN_URL` — base URL, e.g. `https://odonian.summercamp.eastharbor.casa`
- `ODONIAN_TOKEN` — bearer token (every request except `GET /healthz` needs
  `Authorization: Bearer $ODONIAN_TOKEN`)
- `PROJECT_ID` — the project you're draining (a UUID)
- `AGENT_ID` — a stable string identifying you (e.g. `haiku-3`); reported on claim/heartbeat/submit
- `AGENT_MODEL` — your model tier; you may only claim tasks whose `model` equals it

```bash
A=(-H "Authorization: Bearer $ODONIAN_TOKEN" -H "Content-Type: application/json")
```

## The implement loop

Work **one task at a time**, end to end. Don't start a second task until the current one is in
`review`, `blocked`, or `failed`.

### 1. Discover claimable work (your model)

```bash
curl -s "${A[@]}" "$ODONIAN_URL/projects/$PROJECT_ID/tasks?model=$AGENT_MODEL&claimable=true" | jq -r '.[].id'
```

`claimable=true` returns only tasks that are `ready`, have all dependencies `done`, and carry no
live lease; `model=$AGENT_MODEL` further restricts to your tier. Implementers can add `&kind=implement`
to filter for implement-only tasks; reviewers can add `&kind=review` for review-only tasks. Pick one id.
Empty list → nothing to do; stop.

### 2. Claim it (atomic, model-matched — you must win)

```bash
curl -s -o /tmp/claim.json -w '%{http_code}' "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/claim" \
  -d "{\"agent_id\":\"$AGENT_ID\",\"model\":\"$AGENT_MODEL\"}"
```

- `model` is **required** (`400 EMPTY_MODEL` if omitted) and must equal the task's `model`
  (`409`/`MODEL_MISMATCH` otherwise).
- `200` → you won; the task is `in_progress`, assigned to you, with a lease (`lease_expires_at`).
- `409` → someone else won, it's not your model, or it stopped being claimable. Pick another.
- `404` → the task id is gone; re-list.

Never work a task you did not win.

### 3. Read the full spec

```bash
curl -s "${A[@]}" "$ODONIAN_URL/tasks/$TASK_ID" | jq '{title, spec, model, review_models, agent_merge, depends_on, links}'
```

The `spec` is your contract — build exactly what it says, no more. Read the design/feature document
it derives from for context (`GET /projects/$PROJECT_ID/documents`).

### 4. Branch and implement

Branch from the remote (`git fetch origin && git checkout -b <slug> origin/main`); never commit to
`main`. Implement on the branch, scoped to this task, with the tests the acceptance criteria call
for.

### 5. Heartbeat on long work

The lease expires (default 5m). Extend it **before** it lapses, or another agent may reclaim the
task:

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/heartbeat" -d "{\"agent_id\":\"$AGENT_ID\"}" | jq -r .lease_expires_at
```

Only the current assignee may heartbeat, only while `in_progress`.

### 6. Sync with main, then verify

Bring your branch up to date so it merges cleanly, then verify the **merged** result:

```bash
git fetch origin && git merge origin/main --no-edit   # resolve any conflicts, keeping both sides' intent
make check && make test                               # on the merged result; don't submit with failures
```

### 7. Open a PR

```bash
git push -u origin <slug>
gh pr create --fill --base main
```

The PR body should list which acceptance criteria are met and how you verified them.

### 8. Submit (moves the task to `review` and spawns review tasks)

For an **implement** task, submit with the PR/commit links and **no verdict**:

```bash
PR_URL=$(gh pr view --json url -q .url); SHA=$(git rev-parse HEAD)
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/submit" -d "$(jq -n \
  --arg a "$AGENT_ID" --arg pr "$PR_URL" --arg sha "$SHA" \
  '{agent_id:$a, result:"see PR", links:[{kind:"pr",value:$pr},{kind:"commit",value:$sha}]}')"
```

Submit clears your lease, moves the task to `review`, and **auto-spawns one `review`-kind task per
entry in `review_models`** (default `["opus"]`), each `ready` and pinned to that reviewer's model.
Only the assignee may submit, only from `in_progress`. Valid link kinds: `pr`, `branch`, `commit`,
`ci`, `no_op`. On rework (a rejected task bounced back to `ready`), continue the **existing** PR — don't open
a new one — and omit links you already attached.

### No-op submissions (acceptance already satisfied)

If the acceptance criteria are already satisfied (e.g., a fix merged to `main` before you claimed
the task, making implementation unnecessary), submit with a `no_op` link and no `pr` link:

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/submit" -d "$(jq -n \
  --arg a "$AGENT_ID" \
  '{agent_id:$a, result:"acceptance already satisfied on main at <commit>", links:[{kind:"no_op",value:"<commit>"}]}')"
```

A `no_op` link with no `pr` link signals that the task requires no merge and reviewers should verify
the no-op claim. Once **all** reviewers approve, the task goes straight to `done` (regardless of
`agent_merge`), with no merge task spawned. `agent_merge` remains immutable; it's the absence of a
`pr` link that triggers the no-op path.

### 9. Stop

Do **not** merge and do **not** transition the task yourself. Report the task id and PR URL, stop.

## Review tasks (the reviewer loop)

A reviewer is a model-pinned worker (e.g. `AGENT_MODEL=opus`) that drains `review`-kind tasks.

### Claim a review task

```bash
curl -s "${A[@]}" "$ODONIAN_URL/projects/$PROJECT_ID/tasks?model=$AGENT_MODEL&claimable=true" | jq -r '.[].id'
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$REVIEW_TASK_ID/claim" -d "{\"agent_id\":\"$AGENT_ID\",\"model\":\"$AGENT_MODEL\"}"
```

The review task's `spec` carries the **Implementation PR** URL and the **Parent task** id (also in
`target_task_id`). GET the parent task too — its `spec` is the acceptance criteria you review
against, and its `pr` link is the PR you review. Reviewers never merge — your output is the verdict.

### Review as merged with main

Check out the PR head **detached**, merge current main into it, and verify the merged result:

```bash
git fetch origin && git fetch origin "pull/<n>/head" && git checkout --detach FETCH_HEAD
git merge origin/main --no-edit    # CONFLICT → automatic reject
make check && make test            # on the merged result; failure → reject
```

A PR that conflicts with main, or whose merged result fails the build/tests, is never approvable.

### Submit a verdict

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$REVIEW_TASK_ID/submit" -d "$(jq -n \
  --arg a "$AGENT_ID" '{agent_id:$a, result:"<findings>", verdict:"approve"}')"   # or "reject"
```

The server records the verdict on the parent and drives it: **reject → parent back to `ready`**
(rework); **approve →** once *all* of this round's reviewers approve (wait-for-all), the parent
moves to `approved`. A fresh review round spawns each time the parent re-enters `review`.

## The `approved` state and the merge gate

A task in `approved` passed review and awaits merge:

- **`agent_merge` is `false` (default):** the **human** merges the PR and transitions
  `approved → done`.
- **`agent_merge` is `true`:** the server auto-completes the merge (next section). Reviewers never
  merge.

`review → approved` and `review → ready` are server-driven by review verdicts — not manual
transitions. `approved → done` / `approved → ready` are the merge gate.

## Agent merge (`agent_merge`)

`agent_merge` is an **immutable** per-task boolean (set at creation, default `false`). When it's
`true`, the **server** completes the merge once all reviewers approve — reviewers never merge:

- **Parent has a `pr` link:** in the same aggregation that moves the parent to `approved`, the server
  spawns a `merge`-kind task (state `ready`). A dedicated **merger** claims it and squash-merges the
  PR via `odonian merge <merge-task-id>` (REST `PUT .../merge`, per-owner forge token), then
  transitions both the parent and the merge task to `done`. `odonian merge` is idempotent — a
  retried merge job on an already-merged PR converges instead of erroring.
- **Parent is a verified no-op (a `no_op` link, no `pr` link):** the server drives the parent
  straight to `done` in the aggregation — no merge task, nothing to merge.

When `agent_merge` is `false`, the parent waits in `approved` for the **human** merge gate. In no
case does a reviewer run `gh pr merge` or transition the parent.

## Research continuation manifests (opt-in)

Research-track implement tasks may carry a **continuation manifest** in their PR, proposing a set of child tasks to be created when the parent is merged. This allows controlled, reviewed task breakdown.

### Opt-in contract

A research task's spec must contain a line that reads exactly `## Continuation manifest` (trimmed, case-insensitive). The line must be a standalone heading (not mid-line), not nested (no `###`), and not just prose mentioning the phrase. Only tasks with this opt-in line may submit a manifest.

### Worker submission

When submitting a research-track task, include a `manifest` field in the submission body. The manifest is a JSON object (defined in `internal/manifest/manifest.go`) with:

- **version**: Always 1 (the schema version).
- **parent_task_id**: Must match the submitted task's ID.
- **children**: Array of up to 3 child specifications. Each child has `key`, `title`, `spec`, `track` ("research", "build", or "design"), `model`, `review_models` (exactly 2 distinct models), `agent_merge` (boolean), `escalate` (boolean), `claim_ids` (1–6 claims), `source_start_points` (up to 4 sources), `file_scope` (non-overlapping file paths), `acceptance_criteria` (1+ criteria), and `dependencies` (other children or the parent).
- **pending_candidates**: Optional. Maps each claim to a disposition: `assigned`, `carried_forward` (with owner), or `excluded` (with reason).

The server rejects the submission (`400` with an error code) if:
- The parent spec does not opt in.
- The manifest names a different parent task ID.
- The manifest fails validation (unknown models, overlapping files, circular dependencies, etc.).

On rejection, the task remains in `in_progress`, and no state changes occur.

### Review duties

Both reviewers examine the **exact manifest** alongside the parent's implementation. A reviewer can flag issues with the proposed children as review findings on the parent task. The manifest is shown in the `GET /tasks/{id}` response under `submission_manifests` for each review round, including the exact JSON and a SHA-256 digest.

### Merge-time creation

When a human transitions an `approved` research parent to `done`:

1. The server checks if the parent's spec opted in.
2. The server loads the manifest from the approved review round (the round the task passed review in).
3. For each child, the server creates a new task with the manifest's fields.
4. Research-track children start in the `ready` state and are claimable if all their dependencies (if any) are satisfied.
5. Build and design-track children start in the `backlog` state and must be explicitly queued.
6. Created children are linked to the parent task.

The `GET /tasks/{id}` response shows both proposed and created children in the `continuation` view, with status (`pending` or `created`) and manifest digest.

### Recovery from invalid manifests

If the stored manifest fails validation when the parent reaches `done`, the merge transition fails and rolls back. The parent remains in `approved`. Validation failures that trigger rollback include:

- **Digest mismatch**: The stored manifest digest does not match the canonical digest of the persisted manifest data.
- **Canonicalization errors**: Re-parsing and re-encoding the stored manifest produces a different canonical form.
- **Validation rule violations**: The manifest violates any of the validation rules (e.g., overlapping files, unknown models, circular dependencies) when rechecked at merge time.

To recover:

1. The parent's worker or a human operator rejects the parent back to `ready` (via a manual transition or an operator action).
2. The parent re-enters the claim queue and the worker claims it for a new review round.
3. The worker submits a corrected manifest in the new review round.
4. Both reviewers examine and approve the corrected manifest.
5. Only then can a human merge the parent and create children with the new manifest.

This path ensures every submitted manifest is reviewed and approved before child creation. There is no operator procedure to edit a stored digest or manifest in place.

### Idempotency

Child creation is idempotent within a single approved-to-done transition. When a human first transitions an `approved` parent to `done`, children are created once. If the same POST request to `/tasks/{id}/transition {"to":"done"}` is retried before the parent reaches a new state, the retry will encounter the parent already in `done` state and fail with a state-transition error. Thus, true retry-safety is enforced at the HTTP level: the first call succeeds and mutates state; retries fail because the parent is no longer `approved`.

Within a single successful approved-to-done transition, each child's identity within the parent is determined by its manifest key; the parent ID and manifest digest together uniquely identify the complete child set (not individual children). Separate review rounds (after rework) produce separate manifest versions and separate child sets, each with its own digest.

## Task creation

```json
{ "title": "...", "spec": "...", "document_id": "...",
  "model": "haiku", "review_models": ["opus"], "agent_merge": false }
```

- `model` (required) and each `review_models` entry must be in the deployment allowlist
  (`ODONIAN_MODELS`) — else `400 UNKNOWN_MODEL`. `review_models` defaults to `["opus"]`.
- `agent_merge` defaults to `false` and is immutable.
- Review tasks are auto-spawned only — never create them directly.

## Blocked or failed

If the spec is ambiguous/wrong, a dependency is broken, or the task can't be done as specified,
surface it instead of guessing:

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/transition" -d '{"to":"blocked","note":"<what you need>"}'
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/transition" -d '{"to":"failed","note":"<what you tried>"}'
```

## Unblocking

Once a blocker is cleared, the task can be recovered from `blocked` state (unlike terminal states
`done` and `failed`). A human operator can unblock and retry a blocked task:

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/transition" -d '{"to":"ready","note":"blocker cleared"}'
```

This transitions the task back to `ready`, clears any stale assignee and lease, and allows a worker
to claim it again.

Alternatively, if a blocked task has become unrecoverable and should not be retried, transition it
directly to `failed`:

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/transition" -d '{"to":"failed","note":"dead-end blocker; retiring without retry"}'
```

This retires the blocked task to terminal state without re-entering the ready queue.

## Rules

- **One task at a time.** Finish, block, or fail it before touching another.
- **Only work a task you won** (claim `200`). A `409` means it's not yours or not your model.
- **Declare your `model`** on every claim; it must match the task.
- **Heartbeat** before the lease expires on long work.
- **Review the merged-with-main result**, never the branch alone.
- **Never self-merge** unless the task is `agent_merge` and you merged it via the contract above.
  Otherwise your terminal state is `review` (implement) or a submitted verdict (review).

## Status / HTTP code reference

| Code | Meaning |
|------|---------|
| 200 / 201 | success |
| 400 | bad input — `EMPTY_AGENT_ID`, `EMPTY_MODEL`, `UNKNOWN_MODEL`, invalid link kind |
| 401 | missing/invalid bearer token |
| 404 | unknown task/project id |
| 409 | not claimable / not your task / model mismatch / illegal transition |

## State machine

```
backlog ─promote→ ready ─claim→ in_progress ─submit→ review ─(all reviewers approve)→ approved ─merge→ done
                  ▲ ▲                              │                                       │
                  │ └────── lease expiry ──────────┘            reject ─→ ready    human/agent_merge gate
                  │
                  └─ blocked ─→ ready (unblock / retry; clears stale assignee/lease)
                        │
                        └─→ failed (retire without retry; dead-end blocker)
                  
blocked / failed are off-ramps from any active state. blocked is recoverable via → ready; done/failed are terminal.
blocked → failed retires a dead-end blocked task cleanly without re-entering the queue.
```
