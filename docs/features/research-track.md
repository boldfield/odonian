# Feature: research track

Status: ready for owner review · September 25, 2026. Open questions have recommended defaults, marked below; the owner can override any of them before the milestone they affect.

## What this is

A third task track, `research`, alongside `build` and `design`. Research tasks verify factual claims against primary sources and produce evidence tables, not code. They need different review rules, different handling of repeated rejection, and different escalation behavior from build tasks. This spec defines those differences. The `build` and `design` tracks are unchanged.

## Why now: evidence from a research project

Between September 23 and 25, 2026, one project ran 59 research assignments on the build track. Counting superseded copies, that was 105 implement tasks and 551 review tasks. What went wrong, measured on that board:

- **The build gate rejected work for tooling, not content.** The build reviewer prompt treats a failing `make check` or `make test` as an automatic reject. The repository had no Makefile, and 25 of gpt-5.5's rejections were for that reason alone. The workaround was a placeholder Makefile.
- **Reviewers found real defects for many rounds.** One task, a correction to a single research file with 13 claim rows, went through 13 review rounds. 12 were rejected, and 11 of those had a real defect: P1 or P2 in 9 rounds, P3 only in 2, and one round rejected because the reviewer couldn't open a source. A P2 still turned up in round 12. Findings did not shrink round by round.
- **Most disagreement was one reviewer missing things.** On that task, claude-fable-5-1 approved about nine rounds in which gpt-6-astra found real P2 defects. Two reviewers rarely read the same evidence differently.
- **Escalating the model tier did not help.** A cross-file inventory task escalated from Sonnet to Opus to Fable over eight rounds without converging. Every rejection was a missed cross-file link in a manifest that spanned about fifty files. The task was too big for any tier. A bigger model does not fix a task that needs too much context.
- **At the top tier, every rejection became a manual step.** The Fable threshold is one failed round, so each rejection after that blocked the task until a human released it.
- **Superseding bloated specs.** Each escalation prepends all prior review feedback to the spec. The inventory task's spec grew to 83 KB: a 5.7 KB assignment and 79 KB of accumulated findings, some of them later reversed.

## Goals

1. Research review applies evidence rules, not build gates.
2. Findings carry a severity. Findings that block are separated from findings that are recorded and tracked.
3. Every non-blocking finding becomes a tracked task. None is dropped.
4. Disputes about a specific finding are resolved by adjudication, not by majority vote.
5. Repeated rejection is treated as a sign the task is too big, and it blocks the task for decomposition. Research starts on a strong default model and does not escalate unless a research ladder is configured.
6. Spec compaction on supersede, for research tasks only.
7. Each reviewer model's record is measured, so the reviewer pair can be chosen on evidence.

## Non-goals

- Any change to `build` or `design` track behavior.
- A human "accept with known issues" override. A research task finishes only when its reviews pass.
- Majority voting among reviewers.
- A shared store for retrieved evidence. Reviewers retrieve sources independently, and that independence is part of the point.
- Separate task kinds for different kinds of research. One track with the rules below covers source verification, inventories and definitions. Add kinds only if their prompts diverge.

## Behavior

### 1. The track and its prompts

`research` becomes a valid `track` value at task creation, next to `build` and `design`. The validation is at `internal/store/store.go` near line 1014. The harness already resolves prompts by path, `prompts/<delivery_mode>/<track>/<kind>.md`. The track adds `prompts/pull_request/research/implement.md` and `prompts/pull_request/research/review.md`. A task cannot change track after creation, so existing tasks are unaffected.

The research reviewer prompt is the dedicated research reviewer. A separate reviewer deployment is not required. Reviewers are model-dynamic and the fleet image already renders PDFs. An optional per-agent track filter can be added later if research reviewers need their own pool.

### 2. What a research reviewer checks

The research review prompt carries these rules. The project's own contract adds domain rules on top.

- **No build gate.** `make check` and `make test` are run if the repository defines them, and a failure is a finding like any other, with a severity. A missing target is not a finding.
- **The reviewer opens the sources.** A claim marked confirmed passes only if the reviewer retrieved and inspected the cited passage. A worker-supplied hash or export does not substitute, because a fabricated source can come with a fabricated hash.
- **Sources the reviewer can't open.** If a claim is marked confirmed and the reviewer can't open its source, that is a blocking finding. If the claim is already marked pending, with a record of the access attempt, inaccessibility alone is not a finding. A worker can therefore never pass an unverifiable citation as confirmed. It can only downgrade the claim honestly.
- **Round scope.** The first round is a full review. From the second round on, the reviewer checks every finding from earlier rounds and every change since its last review. It still reads the rest of the file. New findings in unchanged text are recorded, and are handled as described in section 4.
- **Severity rubric.**
  - **P1**: a false or unsupported claim, a fabricated or wrong source, or attribution upgraded, for example an allegation presented as fact.
  - **P2**: a claim that overstates its source, a missing qualification that changes meaning, or a known gap dropped.
  - **P3**: a locator, page or footnote reference that is wrong but doesn't change what is claimed, or formatting that doesn't affect meaning.

### 3. Structured verdicts and aggregation

A research review verdict stays `approve` or `reject`. It also carries a list of findings. Each finding has:

- **id**: unique within the task, assigned by the reviewer.
- **severity**: P1, P2 or P3.
- **file** and **line**: where the defect is.
- **summary**: one or two sentences.
- **in_changed_text**: whether the defect is in text changed since this reviewer's last review. Always true in round 1.
- **status**: `new`, `still_open` or `resolved`. From round 2 on, a reviewer reports every one of its own earlier findings as `still_open` or `resolved`.
- **prior_id**: for `still_open` and `resolved`, the id of the earlier finding.

Aggregation for research tasks:

- **A finding blocks** if it is P1 or P2 and in changed text, if it is P1 or P2 in any text during round 1, or if its status is `still_open`.
- **The round passes** only if no reviewer raised a blocking finding. Any valid blocking finding from any reviewer fails the round. That keeps the value of two reviewers, since each is there to catch what the other misses.
- **Non-blocking findings** are P3 findings, and P1 or P2 findings in unchanged text after round 1. They don't fail the round. They become follow-up tasks.

Aggregation lives in the review-completion path in `internal/store/store.go`, around line 2040 on current main, which today requires every reviewer to approve.

### 4. Follow-up tasks for non-blocking findings

When a round passes with non-blocking findings, or when a task reaches `approved`, the server creates one follow-up task per non-blocking finding:

- Same project, `research` track, state `backlog`, linked to the parent task.
- The spec contains the finding text, file and line, severity and the raising reviewer. The follow-up is not rendered as a rewrite of the parent's whole assignment.
- Deduplicated by file, line and summary across rounds, so the same finding raised twice creates one task.
- Listed in the parent's final result and in a parent event.

Follow-ups don't block the parent's merge. Whether they block anything downstream is decided per project, for example by feeding a later inventory task or a human reconciliation step. They start in `backlog` so the owner decides when to run them.

### 5. Disputes and adjudication

A worker may dispute a finding when resubmitting, by listing the finding ID with the source evidence it relies on. Project contracts already invite this.

1. In the next round, the reviewer who raised the finding re-evaluates it against the worker's evidence. If it withdraws the finding, the finding is resolved.
2. If the reviewer maintains it, the finding goes to adjudication. The server spawns a review task scoped to that finding alone, assigned to a configured adjudicator model different from both reviewers.
3. The adjudicator's ruling is binding for that finding only. It does not vote on the round.

The adjudicator model is configured per deployment, for example `ODONIAN_RESEARCH_ADJUDICATOR`, and must be in the allowlist.

### 6. Default model, escalation and round budget

**Default model.** A research task created without a model gets the deployment's research default, `ODONIAN_RESEARCH_DEFAULT_MODEL`. The recommended value is `claude-opus-5-5`. The value must be in the allowlist; the server refuses to start otherwise. A model set explicitly at creation always wins. Without this setting, a task created without a model would fall back to the first allowlisted model, which is today's behavior for every track and is too weak for research.

**Escalation is off by default, and can be turned on by configuration.** Research tasks use their own ladder and thresholds, separate from the build ladder:

- `ODONIAN_RESEARCH_ESCALATION_LADDER`: ordered models, for example `claude-opus-5-5,claude-fable-5-1`. Empty or unset means research tasks never change tier on rejection. The default is empty.
- `ODONIAN_RESEARCH_ESCALATION_THRESHOLDS`: rejected rounds allowed at each tier before moving up, in the same format as the build thresholds.

Build and design tasks keep using the existing ladder and thresholds. A research task's `escalate` flag still applies: false disables escalation for that task even when a research ladder is configured.

**Round budget.** Each research task has a round budget, `ODONIAN_RESEARCH_ROUND_BUDGET`. The budget counts rejected review rounds across the task's entire supersede chain, not per tier, so escalation cannot reset it. When a task reaches the budget without passing, it is blocked with the reason `decompose`, whatever its tier. The block event lists the blocking findings from each round, so the owner can see whether they were shrinking or recurring. The expected response is to split the task, not to release it.

When escalation is enabled, a task escalates at its tier's threshold only if the budget still has rounds left. The budget always takes precedence.

Research tasks also need sizing rules at creation. That belongs in the `odonian-breakdown` skill, not the server: a cap on claim rows and distinct sources per task, and a rule that cross-file mapping is split by the domain the items originate in.

### 7. Spec compaction on supersede, research only

When a research task is superseded, the replacement keeps the original assignment and attaches only the findings still unresolved in the last round, in structured form. The full history stays in the old task's events. Build and design tasks keep today's behavior, which prepends all prior feedback. The prepending is at `internal/store/store.go` near line 2423.

### 8. Reviewer scorecards

For each reviewer model on the research track, record:
- findings raised, by severity
- findings that held up: fixed by the worker, or upheld on adjudication
- findings withdrawn or overturned
- rounds it approved in which another reviewer's blocking finding was later fixed

This goes through a read-only API endpoint and a view in the TUI. It is the basis for choosing the reviewer pair, which today is chosen on anecdote.

### 9. Evidence tooling

Anything mechanical in a research task should come from a tool, not be written by hand. Examples: listing every gap bullet and pending row in a set of files, or running a set of search terms across files and reporting the hits with line numbers. On one project, the most frequent review findings on cross-file inventory tasks were wrong line locators, missed items in an enumeration, and search results the worker reported but that did not exist. All three are mechanical.

The research prompts support project-provided tools without making Odonian responsible for them:

- A project's task contract may name tools, such as scripts in its repository, and the inputs to run them with.
- The research implement prompt requires the worker to run each named tool and include its output verbatim, with the exact command, in the deliverable or the PR.
- The research review prompt requires the reviewer to re-run each named tool on the merged result and treat any difference from the included output as a P1 finding.

Judgment stays with the worker: which hits are real links, and what disposition each item gets.

## Data model changes

- `research` added to the allowed `track` values.
- A findings list on review verdicts, stored with the review task.
- A link from a follow-up task to the parent task and finding it came from.
- A marker on a review task that it adjudicates one finding, and which finding.
- Round budget and adjudicator model as deployment configuration.

## Backward compatibility

Existing tasks keep their track and behavior. Build and design aggregation, escalation and supersede are unchanged. Review verdicts without findings remain valid on the build and design tracks.

## Acceptance criteria

1. A research task with no Makefile passes review when its content is correct.
2. A claim marked confirmed whose source the reviewer can't open fails the round. The same claim marked pending with an access record does not.
3. A round with only P3 findings passes and creates one follow-up task per finding.
4. After round 1, a P2 finding in unchanged text doesn't fail the round, and it creates a follow-up task.
5. A P2 finding in changed text from either reviewer fails the round, even if the other reviewer approves.
6. A disputed finding that its reviewer maintains spawns one adjudication task on the configured model, and its ruling decides that finding.
7. A research task that reaches its round budget is blocked with reason `decompose`. With no research ladder configured its model tier never changes; with one configured, rounds on every tier count toward the same budget.
8. A research task created without a model gets `ODONIAN_RESEARCH_DEFAULT_MODEL`; one created with a model keeps it; build and design defaults are unchanged.
9. A superseded research task's spec contains the original assignment and the last round's unresolved findings only.
10. Build and design tasks behave exactly as before, by their existing tests.

## Decisions and defaults

These were open questions. Each now has a recommended default. The owner can override any of them before the milestone it affects.

1. **Round budget: 6.** It is a size alarm, not a quality bar. A task that needs more than six rounds is treated as too large and blocked for decomposition. The earlier 13-round correction task would have been flagged at round 6.
2. **Adjudicator: configured per deployment, required for adjudication.** `ODONIAN_RESEARCH_ADJUDICATOR` names an allowlisted model that differs from both reviewers of the task. If it is unset, or equals either reviewer, a maintained dispute stays blocking and the event says why.
3. **Follow-up tasks start in `backlog`.** The owner decides when they run.
4. **Default model `claude-opus-5-5`; escalation off, but configurable.** The research ladder is empty by default, so research tasks keep their tier. Escalation to Fable can be enabled later by setting the research ladder, with no code change. The round budget counts across the whole supersede chain either way. A worker that crashes or never submits is already handled by lease expiry and stall detection.
5. **Delivery order** is in the task breakdown below. Until follow-up tasks ship in milestone 2, the research review prompt requires a full review every round.

## Task breakdown

Only milestone 1 is registered on the board now. Later milestones are registered after the owner reviews the previous one.

### Milestone 1: the track, prompts and structured findings

Research tasks can run on their own prompts, and reviewers record findings in structured form. Aggregation, escalation and supersede behavior are unchanged in this milestone, so the structured findings can be observed before they drive decisions.

- **R1. Accept `research` as a track.** Store validation, API documentation, tests. No other behavior change.
- **R2. Research prompts.** `prompts/pull_request/research/implement.md` and `review.md`, carrying the rules in sections 2 and 9. Full review every round. Findings written in the structured format that R3 defines, and also in prose.
- **R3a. Structured findings in the store and API.** A migration adding a findings field to review events, validation of the finding format, and acceptance on review submission. Optional, so build and design verdicts are unaffected.
- **R4. Research default model.** `ODONIAN_RESEARCH_DEFAULT_MODEL`, applied at creation when a research task has no model.
- **R3b. Structured findings in the CLI.** A way for reviewers to submit findings from a file, and inclusion of structured findings in the review context delivered to the worker on rework, alongside the existing prose findings.

Dependencies: R3a after R1, because both edit the store's task creation and the API documentation. R3b after R3a. R4 after R3b, because it edits the store's task creation and the server's configuration in cmd/odonian/main.go, which R1, R3a and R3b also touch. R2 is independent.

### Milestone 2: research aggregation

Blocking rules from section 3, follow-up tasks from section 4, the research ladder and round budget from section 6, and compaction from section 7.

### Milestone 3: disputes

Dispute submission, adjudication tasks and binding rulings from section 5. The review prompt switches to scoped re-review once follow-up tasks exist.

### Milestone 4: scorecards and sizing

The scorecard endpoint and TUI view from section 8, and sizing guidance in the `odonian-breakdown` skill.
