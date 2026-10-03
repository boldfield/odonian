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

A research-track parent can propose follow-on work as a **continuation manifest**. Reviewers
examine the exact proposal together with the parent's PR, and the children exist only after the
parent's verified human merge — nobody transcribes or promotes tickets by hand.

### Opt-in contract

- The parent must be `track: research`, `kind: implement`, and its spec must contain a line that
  reads exactly `## Continuation manifest` (case and surrounding whitespace ignored; a deeper
  heading such as `### Continuation manifest`, or prose mentioning the phrase, does not count).
- Use `agent_merge: false` on the parent. Children are created by the human `approved → done`
  transition (or `POST /tasks/{id}/landing/complete` for a `local_commit` task) and by nothing else.
- Parents that do not opt in behave exactly as before; a manifest sent to one is refused with
  `400 PARENT_NOT_OPTED_IN` (`400 MANIFEST_NOT_ALLOWED` on a non-research task).

### Lifecycle

1. **Submit.** The worker adds a `manifest` object to the normal submit body
   (`odonian submit --manifest-file <path>`). The server validates it, canonicalises it, and stores
   it with its SHA-256 digest for the review round the submission starts. A refused manifest
   (`400`) changes nothing: the task stays `in_progress`, no review round starts, and no manifest
   is stored. The worker fixes the manifest and submits again.
2. **Review.** Two independent reviewers (opus and sonnet by default) examine the manifest in
   `GET /tasks/{id}` → `submission_manifests` / `continuation.proposed_children` (each child
   `pending`). Each reviewer must judge the proposed split — claims covered, source-only scope,
   non-overlapping file scopes, dependencies — and **reject with a finding** if it is wrong.
   Nothing is created while the parent is `review` or `approved`.
3. **Approve.** When both approve, the parent moves to `approved`. Still nothing exists.
4. **Merge.** When the human merges and transitions the parent `approved → done`, the server, in
   the same transaction, loads the manifest of the approved round, re-validates it, checks its
   digest, and creates one task per child. The merge fails as a whole if any step fails.
5. **Work.** Research children start `ready`; generated build and design children start
   `backlog` and must be promoted explicitly. A child is claimable only when it is `ready`, not
   held, and every dependency (including a `parent` or sibling `child` dependency) is `done`; the
   `claimable`, `dependency_status` and `blocked_by` fields of `continuation.created_children`
   report this. The `GET /projects/{id}/tasks?claimable=true&model=<model>` list shows eligible
   research children with no manual promotion.

Minimal manifest (one child; up to 3 are allowed):

```json
{
  "manifest": {
    "version": 1,
    "parent_task_id": "<the submitted task id>",
    "children": [{
      "key": "source-a", "title": "Analyze source A", "spec": "Source-only analysis of A",
      "track": "research", "model": "haiku", "review_models": ["opus", "sonnet"],
      "agent_merge": false, "escalate": true, "claim_ids": ["claim-a"],
      "source_start_points": ["https://example.com/a"], "file_scope": ["docs/a.md"],
      "acceptance_criteria": ["source A verified"],
      "dependencies": [{"kind": "parent", "ref": "<the submitted task id>"}]
    }],
    "pending_candidates": [{"claim_id": "claim-a", "disposition": "assigned"}]
  }
}
```

`dependencies` entries have `kind` `parent` (ref: the parent id), `child` (ref: a sibling `key`)
or `task` (ref: an existing task id). `pending_candidates` maps every claim to `assigned`,
`carried_forward` (with `owner`) or `excluded` (with `reason`). Each child needs exactly two
distinct `review_models`; the full rules and error codes are in `docs/features/research-continuations.md`.

### Review duties

Both reviewers verify the **exact** manifest in the parent's current round, not a summary of it:
that every claim is assigned, carried forward or excluded; that children are source-only and
independent; that no two children write the same file; that dependencies are right; and that the
digest shown for the round is the one they reviewed. A reviewer who finds a problem rejects the
parent with a finding. The parent goes back to `ready`, and the next submission starts a new
round with its own manifest.

### Rejected and invalid manifests: how to recover

- **Invalid split at submit** (`400` with a validator code such as `OVERLAPPING_FILES`,
  `TOO_MANY_CHILDREN`, `UNKNOWN_CHILD_DEPENDENCY`, `INVALID_REVIEW_MODELS`): nothing was stored.
  Correct the manifest and submit again.
- **Reviewer rejects the split:** the parent returns to `ready`; no children exist. Re-claim it
  and submit a corrected manifest. That starts a new review round: `submission_manifests` then
  lists both rounds, reviewers examine the new one, and only the approved round's manifest can
  create children (the earlier round's children never exist).
- **Human disagrees after approval:** `POST /tasks/{id}/transition {"to":"ready"}` from
  `approved` bounces the parent for rework exactly as above. `blocked` and `failed` also create
  nothing.
- **Merge-time failure:** if the stored manifest fails re-validation or its digest does not match
  at `approved → done` (for example because it was altered after review), the transition returns
  `500 TRANSITION_ERROR`, is rolled back, and the parent stays `approved` with no children. The
  API has no way to edit a stored manifest or digest, so a retry only helps if the failure was
  transient. Otherwise bounce the parent with `{"to":"ready"}` and resubmit a corrected manifest
  for a fresh review round.
- **Unmerged work:** a parent that is not `approved` cannot be moved to `done` (`409 CONFLICT`),
  and a `local_commit` parent whose reviewed commit has not landed is refused with
  `409 LANDING_REQUIRED`. In both cases no children are created.

### Retries and duplicates

Children are created once, by the transition that moves the parent to `done`. Sending
`{"to":"done"}` a second time is refused with `409 CONFLICT` (the parent is no longer
`approved`) and creates nothing; `GET /tasks/{id}` keeps showing the same `created_children`.
If the first attempt failed and rolled back, the retry creates the full child set exactly once.
Each created child records the parent, its manifest `key` and the manifest digest, so a child is
identified by `(parent, key, digest)`; a corrected manifest from a later round has a different
digest and therefore creates a separate set.

### What stays separate

- **Review-finding follow-ups.** Non-blocking research findings still create backlog follow-up
  tasks when a round passes. They are listed in `finding_follow_ups`, never in `created_children`,
  and are not changed by the merge.
- **Held legacy tasks.** A held task, or a held task that depends on the parent, is never
  retargeted, repointed at the children, or released. `continuation.action_items`
  (`legacy_held_follow_up`, `held_dependent`) lists them so an operator can replace or close them by
  hand.

## Research admission CLI commands

Research tasks are admitted against a research pacing policy that manages consumption automatically.
The following CLI commands expose research admission operations and status.

### Get research pacing policy configuration

```bash
odonian research-policy [--json]
```

Returns the configured research pacing policy: mode (`enforce`, `observe`, or `disabled`), and
configured account/quota pools without credentials. With `--json`, output is JSON; otherwise it's
formatted as text.

### Get current research pool status

```bash
odonian research-status [--json]
```

Returns the current effective state of each research pool: active attempts, deferred tasks, tokens,
and time of last settlement. With `--json`, output is JSON; otherwise formatted as text.

### Claim a research task

```bash
odonian claim <task-id> \
  [--agent <agent-id>] \
  [--model <model>] \
  [--request-id <request-id>] \
  [--account-id <account-id>] \
  [--work-class <work-class>]
```

Claims a task as `in_progress` for the given agent and model. `agent-id` and `model` default to
the `AGENT_ID` and `AGENT_MODEL` environment variables if not provided. Optional flags support
stable admission request identity for transport retry recovery: `request-id` (idempotency key),
`account-id` (account assertion), and `work-class` (work type assertion). These optional fields
allow a caller to safely retry an ambiguous admission/claim in case of transport failure, as a
repeated claim with matching identity returns the original admission instead of spending again.
Returns the research admission record (permit ID, attempt ID, request ID, expires_at) as JSON.

### Renew a research permit

```bash
odonian permit-renew <permit_id> \
  --task-id <task-id> \
  --model <model> \
  --agent-id <agent-id> \
  --request-id <request-id> \
  --attempt-id <attempt-id>
```

Extends the lease on an active research attempt. All parameters are required and must match the
permit's identity. Returns the renewed attempt with its updated lease expiry time as JSON.

### Finalize a research permit

```bash
odonian permit-finalize <permit_id> \
  --task-id <task-id> \
  --model <model> \
  --agent-id <agent-id> \
  --request-id <request-id> \
  --attempt-id <attempt-id> \
  --exit-class <exit-class> \
  [--usage-tokens <tokens>]
```

Ends an active research attempt and records the outcome. `exit_class` must be one of: `completed`,
`failed`, `cancelled`, `unknown`. `usage_tokens` is optional and specifies the token usage if
reported by the runtime. Returns the finalized attempt with its exit status as JSON.

### Preclaimed task support

Research prompts (`harness/prompts/pull_request/research/implement.md` and
`harness/prompts/pull_request/research/review.md`) support operating on a preclaimed task ID when
`ODONIAN_PRECLAIMED_TASK_ID` is set in the environment. When this variable is set:

- The worker or reviewer skips the `odonian next` and `odonian claim` steps.
- The worker or reviewer uses the supplied task ID directly and validates ownership by reading
  the task (`odonian show <id>`).
- The task must exist and be owned by the current agent; if not, the worker or reviewer stops
  without proceeding.
- Preserve all source checks, findings, adjudication, manifestation and submission requirements
  from the regular prompt flow.

This support allows the harness to pre-admit research tasks against the pacing policy (atomically
claiming and debiting a start in one call), then supply the admitted task ID to the worker/reviewer
process so no additional claims or discovery calls are needed.

## Exit codes

Research admission commands (`claim`, `next --claim`, `permit-renew`, `permit-finalize`) use
distinct exit codes to signal scheduling vs. error conditions, allowing callers to distinguish
transient admission deferral (which may succeed on retry) from fatal conflicts:

- **Exit 0:** Command succeeded.
- **Exit 1:** Generic error (misconfiguration, network failure, server error, etc.). Retry strategy
  depends on the nature of the error; inspect stderr for details.
- **Exit 2:** Scheduling error — the task/permit encountered admission scheduling constraints
  (e.g., 429 Too Many Requests due to quota exhaustion or deferral). The command prints
  `scheduling: <reason>` to stderr. If available, `retry-after: <seconds>` is also printed,
  indicating how long to wait before retrying. Callers should back off and retry after the
  specified delay.
- **Exit 3:** Already claimed (claim-specific) — the task was claimed by another agent or worker
  between the `next` query and the claim attempt. A racing claim succeeded first. Retry by calling
  `next` again.
- **Exit 11:** Conflict error — the permit or attempt is in an invalid state for the requested
  operation. The command prints `conflict: <reason>` to stderr. Possible reasons include
  `ATTEMPT_FENCED` (the attempt ID doesn't match the permit's current attempt), `ATTEMPT_EXPIRED`
  (the permit's lease has expired), `ATTEMPT_FINALIZED` (the attempt is already finalized), or
  `PERMIT_IDENTITY_MISMATCH` (the supplied identity parameters don't match the permit's record).
  These are fatal conditions and do not benefit from retry.

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
