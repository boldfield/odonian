# Defects exposed by documenting the API and harness (PR #335 review)

Status: feature spec, 2026-09-12
Kind: feature_spec (for project Odonian)

## What this is

Writing the complete API reference in PR #335 surfaced behaviour that the docs could not honestly
describe. Each item below is a verified defect with the file and line that proves it, the behaviour
we want instead, and acceptance criteria. The docs state current behaviour until the fix lands;
each task updates the relevant reference section as part of its change.

Non-goals: no new endpoints beyond what is listed; no change to the state machine's transition
table except where an item says so; no harness prompt changes.

## 1. Supersede must preserve `track`

`supersedeTaskTx` (`internal/store/store.go`, the INSERT of the replacement) omits the `track`
column, so the replacement gets the schema default `build`. The circuit breaker uses the same
function, so an escalated `design` task is dispatched with build prompts.

Acceptance: the replacement's `track` equals the superseded task's `track`; a store test
supersedes a `design` task and asserts the copy is `design`; `docs/api.md` supersede section says
`track` is preserved.

## 2. Supersede refuses terminal tasks

`supersedeTaskTx` checks only existence. Superseding a `done`, `failed`, `abandoned`, or
`superseded` task repoints every dependent at a new `backlog` copy, making claimable tasks
unclaimable with no error. The handler already maps `ErrConflict` to 409 but never receives it.

Acceptance: supersede of a task in any terminal state returns `ErrConflict` (HTTP 409) and writes
nothing; store test covers all four terminal states plus one non-terminal success; `docs/api.md`
supersede section lists the 409.

## 3. Releasing a held task in `review` re-runs review aggregation

`SubmitTask` skips the verdict tally when the parent is held; `ReleaseTask` is a bare
`UPDATE held=0`; nothing else aggregates. A parent held through its last reviewer verdict stays
in `review` forever.

Done in two steps. (a) Extract the aggregation block of `SubmitTask` (all review tasks for the
parent done → compute approved / back-to-ready / breaker) into a helper that takes the tx and
parent id, with no behaviour change; existing tests pass unchanged. (b) `ReleaseTask` calls the
helper when the released task is in `review` and every review task targeting it is `done`.

Acceptance for (b): store test holds a parent in `review`, submits the final approving verdict,
asserts the parent is still `review`, releases it, asserts `approved`; a second test with a
rejecting verdict asserts `ready` with `review_round` incremented; `docs/api.md` hold/release
section describes the behaviour.

## 4. `archived_at` is returned by project reads

`GetProject` and `ListProjects` select `id, name, repo, created_at` only, so
`include_archived=true` cannot show which projects are archived.

Acceptance: both queries select `archived_at`; `GET /projects?include_archived=true` and
`GET /projects/{id}` return it (null for live projects); store test archives a project and asserts
both reads show a non-null `archived_at`; `docs/api.md` project sections show the field.

## 5. Supersede skips forge cleanup when there is no token

`closeSupersededPR` proceeds with an empty token (missing forge-tokens file or owner), making
anonymous GitHub calls that fail and log from a detached goroutine.

Acceptance: when `OwnerToken` returns an empty token, cleanup is skipped with one log line naming
the owner and the PR; no forge call is made (test with a recording fake forge asserts zero calls);
`docs/configuration.md` forge-tokens section says supersede cleanup is skipped without a token.

## 6. `track` is validated at task creation

`CreateTasks` accepts any string for `track`; only `build` and `design` have prompts. An unknown
track is never dispatched and blocks the queue (item 7).

Acceptance: `track` outside `{build, design}` returns 400 `UNKNOWN_TRACK` with the same error
shape as `UNKNOWN_MODEL`; empty still defaults to `build`; API test covers accept, default, and
reject; `docs/api.md` task creation says which values are valid.

## 7. A task with no prompt is blocked, not spun on

`harness/agent.sh` single-project loop: when the prompt file for
`<delivery_mode>/<track>/<kind>` is missing it logs `prompt not found` and `continue`s with no
sleep. `odonian next` is unclaimed, so the same task returns every iteration: a hot loop of three
API calls per spin that head-of-line blocks every other ready task of that kind.

Acceptance: on prompt-not-found the agent transitions the task to `blocked` with a note naming
the missing prompt path, logs once, and continues to the next poll after the normal sleep; a
`harness_test.sh` check greps for the transition call and the nap on that path; the multi-project
branch gets the same treatment. `docs/api.md` line about `track` says an unsupported track blocks
the task with a note.

## 8. `pending --json` emits `[]` when nothing is pending

`cmd/odonian/pending.go` marshals a nil slice as `null`, so the documented `jq '.[]'` pipeline
errors. Acceptance: empty result prints `[]`; unit test; same for any other list command that
can print `null` (check `tasks`, `projects`).

## 9. Task ids accept a unique prefix

Table output truncates ids to 8 characters and no command resolves a prefix, so every doc teaches
a `--json | jq` detour to recover full UUIDs.

Acceptance: `GetTask` (and therefore every `/tasks/{id}/...` route) resolves an id shorter than
36 characters as a prefix: exactly one match → that task; none → 404; several → 409 `AMBIGUOUS_ID`
listing the candidates. Minimum prefix length 8. Store and API tests cover the three cases; the CLI
needs no change; `docs/api.md` documents prefix resolution once, in the task-id conventions, and
the `--json | jq` detours in `docs/demo.md`, `docs/running.md`, and `site/index.html` are removed.
