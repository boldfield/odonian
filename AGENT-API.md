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
Operators: the rollout runbook, calibration queries and smoke test are in
[`docs/runbooks/research-pacing-rollout.md`](docs/runbooks/research-pacing-rollout.md).
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
For an admitted research claim, prints the server's `research_admission` object as JSON, with
only the keys the server sent: `permit_id`, `attempt_id`, `request_id`, `account_id`, `expires_at`,
`replayed` (present and `true` when a `--request-id` retry recovered the original admission instead
of spending again) and `observed_denial` (observe mode only: the `outcome`, `reason`, `not_before`
and `retry_after_seconds` the pool would have denied with). A non-research claim prints nothing.

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
permit's identity. Prints the server's `attempt` object unchanged as JSON (`id`, `permit_id`, `task_id`, `state`,
`expires_at`), including the updated lease expiry time.

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
reported by the runtime. Prints the server's `attempt` object unchanged as JSON (`id`, `permit_id`, `task_id`, `state`,
`exit_class`). An explicit `--usage-tokens 0` is sent as zero, distinct from omitting the flag.

### Preclaimed task support

Research prompts (`harness/prompts/pull_request/research/implement.md` and
`harness/prompts/pull_request/research/review.md`) support operating on a preclaimed task ID when
`ODONIAN_PRECLAIMED_TASK_ID` is set in the environment. When this variable is set:

- The worker or reviewer skips the `odonian next` and `odonian claim` steps.
- The worker or reviewer uses the supplied task ID directly and validates ownership by reading
  the task (`odonian show --json <id>`): its `assignee` must equal `$AGENT_ID` and its `state` must
  be `in_progress`.
- The task must exist and be owned by the current agent; if not, the worker or reviewer stops
  without proceeding. Only `ODONIAN_PRECLAIMED_TASK_ID` may be worked; the prompts never select
  another task. This applies to regular research reviews and to adjudication tasks alike.
- `ODONIAN_PRECLAIMED_ATTEMPT_ID` carries the admitted permit attempt identity (the `attempt_id`
  from the claim's `research_admission` output). The harness must export it alongside the task ID;
  the prompts pass it as `--attempt "$ODONIAN_PRECLAIMED_ATTEMPT_ID"` on every `odonian heartbeat`
  and `odonian submit` so the work is fenced to that attempt (no claim ran in the worker process,
  so no attempt file was saved for it).
- When `ODONIAN_PRECLAIMED_TASK_ID` is not set, the prompts run the ordinary `odonian next` +
  `odonian claim` flow. That legacy flow is valid only while the research admission policy mode is
  not `enforce`; under `enforce`, an exit code 10 (scheduling denial) from `next`/`claim` means the
  worker reports the reason and retry hint and stops.
- Preserve all source checks, findings, adjudication, manifestation and submission requirements
  from the regular prompt flow.

This support allows the harness to pre-admit research tasks against the pacing policy (atomically
claiming and debiting a start in one call), then supply the admitted task ID to the worker/reviewer
process so no additional claims or discovery calls are needed.

## Evaluation campaign CLI commands

Evaluation campaigns compare candidate reviewer versions on frozen samples. They are isolated from
the board: these commands never read or write a task, review, verdict or PR link, and a finalized
evaluation attempt **cannot vote on a research task**. Nothing is created automatically, every cap is
a finite integer you pass, and there is no unlimited default. See `docs/api.md` ("Evaluation
Campaigns") for the HTTP interface each command wraps.

Every command needs `ODONIAN_URL` and `ODONIAN_TOKEN`, takes only the flags below (unknown flags and
extra arguments are errors) and prints the server's JSON response as **one compact line** on stdout.
Flags marked * are required. Candidates are always referenced by candidate-version ID; nothing names
a provider.

```bash
# Pools (a candidate's identity.account_pool must name a configured pool)
odonian evaluation-pool-set --id local --concurrency-only --concurrent-limit 2
odonian evaluation-pool-set --id acct --start-rate 0.5 --burst 3 --concurrent-limit 2
odonian evaluation-pool-get --id local

# Campaign
odonian evaluation-create-campaign --id c1 --name "Reviewer comparison" \
  --projects proj1,proj2 --models model-a,model-b --cohort "$(cat cohort.json)" --cap 100 \
  [--description "..."]
odonian evaluation-get-campaign --id c1
odonian evaluation-get-campaign-status --id c1
odonian evaluation-pause-campaign --id c1

# Candidate versions (identity JSON inline or from a file)
odonian evaluation-create-candidate --campaign c1 --id v1 --cap 50 --identity-file identity.json
odonian evaluation-list-candidates --campaign c1
odonian evaluation-get-sample --campaign c1 --sample s1

# Attempts
odonian evaluation-claim-job --sample s1 --candidate v1 --request-id run-1 --ttl-ms 600000
odonian evaluation-renew-attempt --job <job_id> --attempt <attempt_id> --ttl-ms 600000
odonian evaluation-finalize-attempt --job <job_id> --attempt <attempt_id> --exit-class completed \
  --result '{"status":"completed","duration_ms":4200,"findings":[{"id":"f1","severity":"minor","summary":"..."}]}'

# Report and operator dispositions (the only source of finding labels)
odonian evaluation-get-report --id c1
odonian evaluation-record-disposition --campaign c1 --ref candidate:<attempt_id>:f1 \
  --label valid --severity P1 --claim "claim 3 misstates the cited holding" \
  --evidence "the opinion at p. 4 holds the opposite" --actor alice
odonian evaluation-list-dispositions --campaign c1
```

| Command | Flags | HTTP |
|---|---|---|
| `evaluation-pool-set` | `--id`*, `--concurrent-limit`*, then either `--concurrency-only` or `--start-rate` and `--burst` | `PUT /evaluation/pools/{id}` |
| `evaluation-pool-get` | `--id`* | `GET /evaluation/pools/{id}` |
| `evaluation-create-campaign` | `--id`*, `--name`*, `--projects`* and `--models`* (comma-separated), `--cohort`*, `--cap`* (>= 1), `--description` | `POST /evaluation/campaigns` |
| `evaluation-get-campaign` | `--id`* | `GET /evaluation/campaigns/{id}` |
| `evaluation-get-campaign-status` | `--id`* | `GET /evaluation/campaigns/{id}/status` |
| `evaluation-pause-campaign` | `--id`* | `POST /evaluation/campaigns/{id}/pause` |
| `evaluation-create-candidate` | `--campaign`*, `--id`*, `--cap`* (>= 1), `--identity` or `--identity-file` (one of them)* | `POST /evaluation/campaigns/{id}/candidates` |
| `evaluation-list-candidates` | `--campaign`* | `GET /evaluation/campaigns/{id}/candidates` |
| `evaluation-get-sample` | `--campaign`*, `--sample`* | `GET /evaluation/campaigns/{campaign}/samples/{sample}` |
| `evaluation-claim-job` | `--sample`*, `--candidate`*, `--request-id`*, `--ttl-ms`* (1-3600000) | `POST /evaluation/jobs/claim` |
| `evaluation-renew-attempt` | `--job`*, `--attempt`*, `--ttl-ms`* (1-3600000) | `POST /evaluation/jobs/{job}/attempts/{attempt}/renew` |
| `evaluation-finalize-attempt` | `--job`*, `--attempt`*, `--exit-class`*, `--result` or `--result-file` | `POST /evaluation/jobs/{job}/attempts/{attempt}/finalize` |
| `evaluation-get-report` | `--id`* | `GET /evaluation/campaigns/{id}/report` |
| `evaluation-record-disposition` | `--campaign`*, `--ref`*, `--label`* (`valid`, `invalid`, `unresolved`), `--severity`* (`P1`-`P3`), `--claim`*, `--actor`*, `--evidence` or `--evidence-file` (one of them)* | `POST /evaluation/campaigns/{id}/dispositions` |
| `evaluation-list-dispositions` | `--campaign`* | `GET /evaluation/campaigns/{id}/dispositions` |

**Finalize payload.** `--result` (or `--result-file`) is a JSON object limited to `status`,
`error_class`, `error_message`, `duration_ms`, `usage_tokens` and `findings`; any other key (including
`fence_attempt_id`, `exit_class`, `task_id` or `verdict`) is rejected before anything is sent. The
fence is always the `--attempt` flag and the exit class always the `--exit-class` flag, so a result
file cannot override either.

**Status.** `evaluation-get-campaign-status` is the poll command. Read `state` (`active`, `paused` or
`exhausted`), `attempts_remaining`, and each candidate's `attempts_remaining`, `exhausted`,
`active_attempts` and `pool`.

**Exit codes.** Validation of flags exits 1 without contacting the server. A server refusal maps like
the research admission commands: `429 ADMISSION_DENIED` exits **10** (prints `scheduling:` and the
retry hints; wait and retry the claim), and every `409` exits **11** with `conflict: server error
(<CODE>): ...` on stderr, where `<CODE>` is one of `PAUSED_WAITING` (stop claiming; the campaign is
paused), `CAPACITY_EXHAUSTED` (this candidate or campaign is out of attempts), `ATTEMPT_LIVE`,
`FENCE_MISMATCH` (a newer attempt superseded yours), `ATTEMPT_EXPIRED` (your lease ran out before the
result arrived), `ATTEMPT_FINALIZED` (a repeat of an already recorded result; nothing changed),
`POOL_NOT_CONFIGURED` or `ALREADY_PAUSED`. None of these should be retried blindly. Other errors
(`400`, `404`, `5xx`) exit 1.

**Report.** `evaluation-get-report` compares the exact same frozen samples across every candidate
version and the production reviewers of that round, with per-reviewer coverage, adjudicated metrics,
latency, provider-native usage and a list of material candidate-only findings for a human to read.
It never sends anything or changes a task. Copy `ref` values from the report's `groups[].members[].ref`.
A label needs evidence, an actor, a claim and a severity; revising one records a new row and keeps the
old one. The server refuses a `ref` that is not part of the campaign (`404 FINDING_NOT_FOUND`, exit 1).
Operating procedure: `docs/runbooks/reviewer-evaluation-rollout.md`. See `docs/api.md` for the field
meanings.

**Replay.** Re-running `evaluation-claim-job` with the same `--request-id` returns the original
attempt without spending cap, so a worker may safely retry after a lost response. Re-running
`evaluation-finalize-attempt` after it succeeded exits 11 `ATTEMPT_FINALIZED` and records nothing
twice.

## Exit codes

Research admission commands (`claim`, `next --claim`, `permit-renew`, `permit-finalize`) use
distinct exit codes to signal scheduling vs. error conditions, allowing callers to distinguish
transient admission deferral (which may succeed on retry) from fatal conflicts:

- **Exit 0:** Command succeeded.
- **Exit 1:** Generic error (misconfiguration, network failure, server error, etc.). Retry strategy
  depends on the nature of the error; inspect stderr for details.
- **Exit 2:** `next` only — nothing claimable, or a `next --claim` race lost. Not a scheduling
  denial and not an error; the queue simply has nothing for this worker right now.
- **Exit 10:** Scheduling denial — the admission pool refused the start (429 Too Many Requests).
  The command prints `scheduling: <message>` to stderr followed by whichever hints the server sent,
  one per line: `outcome: defer|retry`, `reason: <reason>`, `not-before: <RFC 3339 time>` and
  `retry-after: <seconds>`. A rate/time deferral carries `not-before` only; `retry-after` is sent
  when an active dispatch must finish first (also read from the `Retry-After` header). Callers
  should wait until `not-before` / `retry-after` and retry; this exit applies to `claim`,
  `next --claim`, `permit-renew` and `permit-finalize`.
- **Exit 3:** Already claimed (claim-specific) — the task was claimed by another agent or worker
  between the `next` query and the claim attempt. A racing claim succeeded first. Retry by calling
  `next` again.
- **Exit 11:** Conflict error (409) — the permit, attempt or request identity is in an invalid
  state for the requested operation. The command prints `conflict: <reason>` to stderr. Possible reasons include
  `ATTEMPT_FENCED` (the attempt ID doesn't match the permit's current attempt), `ATTEMPT_EXPIRED`
  (the permit's lease has expired), `ATTEMPT_FINALIZED` (the attempt is already finalized), `PERMIT_IDENTITY_MISMATCH` (the supplied identity parameters don't match the permit's record),
  `REQUEST_ID_CONFLICT` (a `claim --request-id` already used for a different task, agent, model or
  pool) or `TASK_BUSY` (the task still has a live research attempt).
  These are not transient scheduling conditions; do not retry them blindly.

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

## Task priority management

Tasks are ordered in the execution queue by priority (higher first), then creation order, then ID.
Manual priority ranges from 1–1000 (default 500). The server can assign higher priorities (>1000)
to move tasks to the front and keep them ahead of all manual assignments.

### Set manual priority

Assign a manual priority (1–1000) to control task ordering. The priority is idempotent: replaying
the same `action_key` with the same priority returns the cached result.

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/priority/set" \
  -d '{"action_key":"key-12345","priority":750,"actor":"operator","reason":"urgent fix needed"}'
```

- `action_key` (required): Unique identifier for idempotency (max 200 chars)
- `priority` (required): Integer from 1 to 1000; `400 INVALID_PRIORITY` if outside range
- `actor` (required): Who is making the change
- `reason` (required): Why the priority is being changed

**Response:** `200 OK` with `{"priority": <int>, "topic_anchor_id": "<task_id>", "replayed": <bool>}`;
`409 IDEMPOTENCY_MISMATCH` if the same key is used with a different priority.

### Move task to front of queue

Assign a server-calculated priority exceeding all manual assignments (always >1000) to move a task
to the front of the queue. Successive calls return strictly increasing values, ensuring the task
remains ahead of any subsequent manual priority assignments.

```bash
curl -s "${A[@]}" -X POST "$ODONIAN_URL/tasks/$TASK_ID/priority/front" \
  -d '{"action_key":"front-key-xyz","actor":"operator","reason":"production critical"}'
```

- `action_key` (required): Unique identifier for idempotency (max 200 chars)
- `actor` (required): Who is making the change
- `reason` (required): Why the task is being moved

**Response:** `200 OK` with `{"priority": <int>, "topic_anchor_id": "<task_id>", "replayed": <bool>}`;
priority is always >1000 and strictly increases on subsequent calls with different keys.

### CLI: priority management

The `odonian priority` command provides CLI access to priority management.

```bash
odonian priority <task-id> --set <N> --reason "<reason>"          # Set priority 1-1000
odonian priority <task-id> --front --reason "<reason>"             # Move to front (>1000)
odonian priority <task-id> --reset --reason "<reason>"             # Reset to default (500)
odonian priority <task-id> --set <N> --action-key "<key>" --reason "<reason>"  # Use provided action key for retries
```

- `--set <N>`: Set priority to N (1–1000); returns `INVALID_PRIORITY` if outside range
- `--front`: Move task to front with server-calculated priority (>1000)
- `--reset`: Reset priority to default value (500)
- `--reason`: Why the priority is being changed (required)
- `--action-key`: Optional action key for idempotent retries; if omitted, generates a new UUID per invocation

**Output:** displays action key, old/new priority values, and `Replayed: true` if idempotently matched.

**Important:** Priority changes do not affect task eligibility or permissions. Filters in `odonian next`
do not change the global "front" maximum calculation—the server always assigns a value exceeding all
manually-assignable priorities. A task's `held` status and merge gates are preserved regardless of
priority changes.

Both priority endpoints:
- Return `404 NOT_FOUND` if the task does not exist
- Return `409 ARCHIVED` if the task has been archived
- Return `400 INVALID_ACTION_KEY` if `action_key` is empty or >200 characters
- Return `400 JSON_DECODE_ERROR` if the JSON is malformed or contains unknown fields
- Support required idempotency key (`action_key`); same key replayed returns the same result with `"replayed": true`
- Preserve the task's `held` status and all merge gates

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
