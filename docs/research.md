# Research track guide

Verified against source baseline `b3da896` (tag `v0.24.0`), not against any production deployment.
Exact request bodies, CLI flags, exit codes and error codes live in [`api.md`](./api.md) and
[`../AGENT-API.md`](../AGENT-API.md); this guide explains what the research track is and how its
pieces fit, and links to those contracts rather than restating them.

## Tracks are not kinds

Every task has two independent labels.

- **`kind`** says what the task *does* in the workflow: `implement` (the work), `review`
  (auto-spawned, one per reviewer model, carries a verdict), or `merge` (auto-spawned for
  `agent_merge=true` with an active PR after approval, run by a non-LLM merger). There are no other kinds. An adjudication task
  (below) is a `review`-kind task scoped to one finding.
- **`track`** says what *sort of work* it is and selects the prompts and the review rules:
  `build`, `design` or `research`. A task's track is fixed at creation and survives supersession.

| Track | The worker produces | The reviewer checks | Repeated rejection |
|---|---|---|---|
| `build` | code on a branch or PR | the diff; a failing `make check`/`make test` is an automatic reject | per-model thresholds and the build escalation ladder |
| `design` | a `DESIGN.md` interface contract for one named tool | the contract | same ladder and thresholds as `build` |
| `research` | claims verified against primary sources, with evidence | that each confirmed claim's cited passage really says that | a separate research ladder (off by default) and a chain-wide round budget |

Where the harness looks for prompts: `harness/prompts/<delivery_mode>/<track>/<kind>.md`. At this
baseline the bundled prompts are `pull_request/{build,design,research}` and `local_commit/build`.
**There are no `local_commit` research prompts.** A research task claimed by a `local_commit` slot
has no prompt, so the harness blocks the task with a `no prompt for …` note rather than running
it; see the delivery-mode table in [`api.md`](./api.md#post-projectsidtasks). Research is a
pull-request track.

## What a research task delivers

The worker's output is evidence, not code: a set of claims, each marked with a status and tied to a
locator in a primary source the reviewer can open. A claim the worker could not verify is marked
pending with a record of the access attempt; it is never marked confirmed on the strength of a
hash, an export or a secondary source. Anything mechanical in the deliverable (enumerations,
search hits, line locators) should come from a tool the project names in the task contract, with
the exact command and its output included, because the reviewer re-runs those tools and treats a
difference as a P1 finding.

The sizing rules (how many claim rows and distinct sources one task may carry, and splitting
cross-file mapping by the domain the items come from) are enforced at breakdown time by the
`odonian-breakdown` skill, not by the server. Oversized research tasks can exhaust the round budget below.

## Review: structured findings

A research review verdict is still `approve` or `reject`, but it carries a list of structured
findings. Each finding has an id, a severity, a file and line, a summary, whether it is in text
changed since that reviewer's last look, and a status (`new`, `still_open`, `resolved`) with a
`prior_id` for the last two. The severity rubric:

- **P1**: a false or unsupported claim, a fabricated or wrong source, or attribution upgraded (an
  allegation presented as fact).
- **P2**: a claim that overstates its source, a missing qualification that changes meaning, or a
  known gap dropped.
- **P3**: a wrong locator, page or footnote that does not change what is claimed, or formatting.

The first round is a full review. From round two on, each reviewer re-reports every one of its own
earlier findings as `still_open` or `resolved`, checks everything changed since its last review, and
still reads the rest of the file.

Findings travel in the submit payload (`findings`, and `disputes` for the worker's side). Field
rules and validation errors are in [`api.md`](./api.md#post-tasksidsubmit); the submit flow a
reviewer follows is in [`../AGENT-API.md`](../AGENT-API.md).

### Blocking findings versus follow-ups

The round's outcome is decided by the findings, not by counting approvals. A finding **blocks** when
it is P1 or P2 and any of these hold: it is in changed text; it was raised in round 1; or it is
`still_open`. A `resolved` finding never blocks, and a P3 never blocks whatever its status. Any one
blocking finding from any reviewer fails the round, which is what makes a second reviewer worth
having: each is there to catch what the other misses. If the server cannot account for every
reviewer's findings (a missing or malformed payload), it fails closed and treats the round as
blocked rather than risk a silent approval.

**Non-blocking findings** (every P3, and new P1/P2 in unchanged text after round 1) do not fail the
round. When the round passes, the server creates one follow-up research task per outstanding
non-blocking finding, in `backlog`, linked to the parent, deduplicated so the same finding raised in
two rounds yields one task. Follow-ups never block the parent's merge; the owner decides when to run
them. `GET /tasks/{id}` lists them as `finding_follow_ups`.

### Disputes and adjudication

A worker may dispute a finding on resubmit by naming the finding id and the source evidence it
relies on. In the next round the reviewer that raised it re-evaluates against that evidence and
either resolves it or maintains it.

A maintained dispute goes to adjudication **only if** the deployment sets
`ODONIAN_RESEARCH_ADJUDICATOR` to an allowlisted model that differs from every one of the task's
reviewers (compared as exact strings, so `opus` and `claude-opus-5-5` are *not* treated as the same
model). The server then spawns one `review`-kind task scoped to that single finding; the
adjudicator's ruling binds that finding and never votes on the round.

Limitations to plan around:

- When the adjudicator is unset, or equals one of the reviewers, the maintained finding stays
  blocking and the server records a `research_adjudication_unavailable` event saying why. The
  dispute is not lost. The worker can still correct the claim and the reviewer can mark the
  finding resolved in a later round; an operator can also reconsider the task or configuration.
- Adjudication is per finding. A round with several maintained disputes spawns several
  adjudication tasks, each spending reviewer capacity (and, under pacing, a research start).
- Reviewers are chosen per task (`review_models`); the adjudicator is chosen per deployment. If you
  change a task's reviewers (`PATCH /tasks/{id}`, see [`api.md`](./api.md#patch-tasksid)), check the
  adjudicator still differs from every reviewer.

Configuration rows: [`configuration.md`](./configuration.md#server-odonian-server).

## Escalation, the round budget, and `decompose`

Research tasks do not use the build ladder.

- **Default model.** A research task created without a `model` gets
  `ODONIAN_RESEARCH_DEFAULT_MODEL` if set; otherwise the deployment's general default (`haiku` when
  allowlisted, else the first allowlisted model), which may not suit the task. Choose the default deliberately.
- **Escalation is off unless configured.** `ODONIAN_RESEARCH_ESCALATION_LADDER` is empty by
  default, so a rejected research task keeps its tier. With a ladder set,
  `ODONIAN_RESEARCH_ESCALATION_THRESHOLDS` says how many rejected rounds each tier gets, and the
  task's own `escalate=false` still opts it out. A model not on the research ladder falls back to
  `ODONIAN_MAX_REVIEW_ROUNDS`.
- **The chain-wide round budget always wins.** `ODONIAN_RESEARCH_ROUND_BUDGET` (default 6) counts
  rejected rounds across the task's entire supersede chain: automatic escalation and manual
  `supersede` both count and neither resets it. When the chain reaches the budget, the task is
  blocked with reason `decompose`, whatever its tier and even if the per-tier threshold has not
  been hit. The block note lists the blocking findings from every round, oldest first, so the owner
  can see whether they were shrinking or recurring.

`decompose` means what it says: the expected response is to split the task, not to unblock it. The
evidence behind this rule (tasks that escalated three tiers over eight rounds without converging
because the task needed more context than any tier has) is in the feature doc linked below.

Two research-only behaviours that follow from this: on supersession a research task's replacement
spec carries the original assignment plus only the findings still unresolved in the last round, in
structured form (build and design replacements append prior rejection feedback); and research-track
audit events are never pruned, because scorecards are computed from them.

## Continuations (opt-in)

A parent research task can propose child tasks in a versioned **continuation manifest** submitted
with its work. The manifest is reviewed by the configured reviewers alongside the parent (a reviewer can object
to the proposed breakdown as a finding on the parent), and the children are materialised on the **`approved → done` transition**, not at review
approval. In the intended human-gated PR workflow, PR-watch observes the human merge and records
completion. A manual board transition does not itself prove a PR was merged. The direct
`review → done` no-op path currently does not materialise continuation children.

The server accepts a manifest only when the task's spec opts in with a line matching
`## continuation manifest` (case-insensitive, ignoring surrounding whitespace), the manifest names the submitting task as its parent, and it fits the
fixed child envelope (at most three children, each fully specified with exactly two reviewers,
bounded claims and sources per child, non-overlapping file scopes). Accepted manifests are stored
per review round, canonicalised and digested, and returned on `GET /tasks/{id}` as
`submission_manifests`. Each materialised child records a dedup link keyed on the parent and the
manifest key, so repeated materialisation for the same parent does not create duplicates.

Contract and envelope limits: [`../AGENT-API.md`](../AGENT-API.md) ("Research continuation
manifests"). Design: [`features/research-continuations.md`](./features/research-continuations.md).

## Pacing: `disabled`, `observe`, `enforce`

Research work is the only work the server can pace. `ODONIAN_RESEARCH_POLICY_MODE` is read once at
startup:

- **`disabled`** (default): normal task eligibility still applies, but there is no pacing decision,
  permit or pool debit. `ODONIAN_RESEARCH_POOLS` is ignored.
- **`observe`**: otherwise eligible claims are admitted regardless of the policy decision. Mapped
  models create permits and attempts; hypothetical admits debit starts, while hypothetical denials
  are recorded without denying work. Unmapped models use plain claims without permits.
- **`enforce`**: every research claim is gated by its model's pool (a sustained start rate, a burst,
  a concurrent-dispatch limit, and a reserved share for completion work such as reviews, rework and
  adjudication). A denied claim leaves the existing state and lease unchanged, with no rejection, round
  increment or block; the fleet starts no model process for it and retries when the server's hint says to.
  Every allowlisted model must be mapped to a pool or the server refuses to start.

The harness side is the **preclaim** contract: for research tasks the harness requests admission and
claims the task *before* a model exists, then hands the dispatched agent the preclaimed task and, when admission created one, an
attempt id. It renews the lease and any permit in the background for the life of the process. Recognized
backend failures trigger a best-effort handback (`unclaim`) when ownership and submission
preconditions still permit it. Build, design and merge
work is never paced and keeps the ordinary self-claim path.

Admitted attempts are durable (`research_pool`, `research_permit`, `research_attempt`), so a server
restart neither loses nor refunds them. Starts are a proxy for subscription consumption, not a
billing meter: other activity on the same account is invisible to the policy.

Variable reference and the pool JSON schema: [`configuration.md`](./configuration.md#research-pacing-pools).
Enabling it safely, including reading observed data and draining pre-admission launchers first:
[`runbooks/research-pacing-rollout.md`](./runbooks/research-pacing-rollout.md). Status and permit
endpoints: [`api.md`](./api.md#research-admission).

## Scorecards and isolated reviewer evaluation

These are two different things and only the first touches live tasks.

**Reviewer scorecards** (`GET /projects/{id}/research/reviewers`; the `odonian-tui` has a scorecard
view) aggregate findings per reviewer model and severity across a project's research tasks,
spanning supersede chains without double-counting a finding carried across a supersession, and
recording which findings held up (fixed or upheld on adjudication) versus were withdrawn or
overturned. They are observational: they report what reviewers did, and are the input to a human
choosing a reviewer pair. Nothing reads them to route work. Build and design review data is never
included. See [`api.md`](./api.md#research-reviewer-scorecards).

**Evaluation campaigns** (`/evaluation/...`, `odonian evaluation-*`, `internal/evaluation`,
`internal/evalrun`, `cmd/muse-adapter`) compare candidate reviewer runtimes on frozen samples of
past review work. They are a separate surface with their own tables, pools, caps and report. At this
baseline:

- Evaluation cohort/report generation reads production tasks, events and links as baseline data.
  Evaluation results do not mutate production tasks, reviews, PR links or scorecards, and cannot
  vote on, reject, advance or change a research task.
- A candidate adapter is not in any task's `review_models` and nothing in the harness dispatches
  it. The bundled Muse Code adapter is documented as an isolated candidate runtime that changes no
  production reviewer.
- Nothing runs unless an operator creates a campaign, its candidates and its pools, with finite
  caps, and runs the evaluation process themselves. This repository ships the library and the
  adapter, not a launcher.

Operator procedure and how to read a report:
[`runbooks/reviewer-evaluation-rollout.md`](./runbooks/reviewer-evaluation-rollout.md); writing a new
adapter: [`runbooks/evaluation-adapter-guide.md`](./runbooks/evaluation-adapter-guide.md); the Muse
adapter: [`runbooks/muse-code-adapter.md`](./runbooks/muse-code-adapter.md); routes:
[`api.md`](./api.md#evaluation-campaigns).

## Where the rules come from

- Design and the evidence that motivated the track:
  [`features/research-track.md`](./features/research-track.md).
- Acceptance scenario and the automated tests that cover it:
  [`features/research-track-verification.md`](./features/research-track-verification.md),
  [`runbooks/research-track-smoke-test.md`](./runbooks/research-track-smoke-test.md).
- Continuations: [`features/research-continuations.md`](./features/research-continuations.md).
- Pacing and evaluation design:
  [`features/research-pacing-and-reviewer-evaluation.md`](./features/research-pacing-and-reviewer-evaluation.md).
- The hand-back and backoff behaviour that research dispatch depends on:
  [`features/dispatch-failure-handling.md`](./features/dispatch-failure-handling.md).
- Code: `internal/store/store.go` (`isBlockingResearchFinding`, `checkResearchBlockingFindings`,
  `createResearchFollowUpTasks`, `spawnAdjudicationTask`), `internal/store/research_claim.go`,
  `internal/store/research_permit.go`, `internal/store/scorecard.go`, `internal/policy/`,
  `internal/manifest/`, `internal/evaluation/`, `internal/evalrun/`.
