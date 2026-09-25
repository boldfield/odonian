# Feature: research track

Status: draft for owner review · September 25, 2026

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
5. Repeated rejection is treated as a sign the task is too big, and it blocks the task for decomposition instead of escalating the model tier.
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

A research review verdict stays `approve` or `reject`. It also carries a list of findings. Each finding has an ID, a severity, a file and line, a summary, and whether it is in text changed during this round.

Aggregation for research tasks:

- **A finding blocks** if it is P1 or P2 and in changed text, if it is P1 or P2 in any text during round 1, or if it is any finding from an earlier round that is still unresolved.
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

### 6. Round budget instead of escalation

Research tasks do not change model tier on rejection. The `escalate` flag is ignored for the research track.

Each research task has a round budget, configured per deployment, for example `ODONIAN_RESEARCH_ROUND_BUDGET`. When a task reaches the budget without passing, it is blocked with the reason `decompose`. The block event lists the blocking findings from each round, so the owner can see whether they were shrinking or recurring. The expected response is to split the task, not to release it.

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
7. A research task that reaches its round budget is blocked with reason `decompose`, and its model tier doesn't change.
8. A superseded research task's spec contains the original assignment and the last round's unresolved findings only.
9. Build and design tasks behave exactly as before, by their existing tests.

## Open questions for the owner

1. **Round budget.** The 13-round correction task above was productive throughout. If the budget is meant as a size alarm, 6 would have flagged it for splitting. Is that the intent?
2. **Adjudicator model.** It must differ from both reviewers. Which model?
3. **Follow-up tasks.** Should they start in `backlog` as proposed, or promote automatically?
4. **Escalation.** Should research keep one escalation step, for a worker that crashes or produces nothing, or never escalate at all?
5. **Order of delivery.** Proposed order: the track and prompts, then structured findings with follow-up tasks, then adjudication, then the round budget, then compaction, then scorecards. The round-scope rule in section 2 depends on follow-up tasks. Until those ship, the research prompt should require full review every round.
