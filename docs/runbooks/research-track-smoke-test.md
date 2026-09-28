# Research track live smoke test

Status: draft, September 27, 2026. Not yet run. Board: project `4ff592d7-da96-40b0-8753-db42be264a68`, document `a2001386-bf46-4b5d-8024-bb90c0e16da9`.

R16 verified the research track with store and API tests only. This runbook exercises the paths
unit tests can't: the real fleet worker and reviewer prompts, `odonian submit --findings-file` and
`--disputes-file` from inside the fleet image, the codex reviewer, adjudication-task pickup by the
reviewer fleet, and the scorecard endpoint against real data. Run it after each release that changes
research-track server code or research prompts.

## What it covers, and what it leaves to unit tests

| Path | Covered by |
|---|---|
| P3 finding on a passing round creates a backlog follow-up | Fixture B (fleet only) |
| Approved research task stays `approved`; no merge task | Fixtures A and B |
| P1 on a confirmed false claim blocks round 1 | Fixture A round 1 |
| Worker dispute with evidence reaches the raising reviewer | Fixture A round 2 (operator plays the worker) |
| Maintained dispute spawns one adjudication task on `ODONIAN_RESEARCH_ADJUDICATOR` | Fixture A round 2 |
| Adjudication ruling binds the one finding | Fixture A round 2 |
| Research spec compaction on manual supersede | Fixture A, after round 2 |
| Fleet worker addresses the upheld finding; round passes | Fixture A round 3 |
| Scorecard reflects raised, upheld and fixed findings | End |
| Chain-wide round budget, `decompose` block | Unit tests only: 6 rejected rounds of paid reviews is not worth it |
| Research escalation ladder | Not deployed (ladder is empty) |

The operator plays the worker in fixture A's first two rounds because the dispute path can't be
forced otherwise: an honest fleet worker fixes a false claim instead of disputing it, which is
correct behavior but leaves adjudication unexercised.

## Setup

1. The private sandbox repo `boldfield/odonian-research-smoke` holds a neutral README and the two
   fixture files below on `main`. Keep this runbook out of that repo: reviewers read the whole
   repo, and knowing the round-2 dispute is deliberately weak would bias them.
2. Its Odonian project, `odonian-research-smoke`, uses `delivery_mode` `pull_request`. One
   `feature_spec` document references the sandbox README, not this runbook (same reason), and the
   fixture tasks hang off it.
3. Reset the fixtures before a re-run: close the previous run's PRs unmerged, so `main` still
   carries the original rows.
4. Set `review_models` to `["gpt-6-astra","claude-fable-5-1"]` on both tasks, the production
   research pair. It must not include `ODONIAN_RESEARCH_ADJUDICATOR` (see "Reviewer pair" below).

All sources are the National Archives page on Amendments XI–XXVII,
`https://www.archives.gov/founding-docs/amendments-11-27`, which is stable, public and
text-rendered.

### Fixture A: `claims-a.md`

| # | Claim | Status | Source | Locator |
|---|---|---|---|---|
| 1 | The Nineteenth Amendment was ratified on August 18, 1920. | confirmed | archives.gov amendments 11–27 | Amendment XIX |
| 2 | The Twenty-Seventh Amendment was ratified in 1791 along with the Bill of Rights. | confirmed | archives.gov amendments 11–27 | Amendment XXVII |

Row 2 is deliberately false. The page says the amendment was proposed on September 25, 1789 and
ratified on May 7, 1992.

### Fixture B: `claims-b.md`

| # | Claim | Status | Source | Locator |
|---|---|---|---|---|
| 1 | The Twenty-Sixth Amendment was ratified on July 1, 1971. | confirmed | archives.gov amendments 11–27 | Amendment XXVI |
| 2 | The Twenty-Second Amendment was ratified on February 27, 1951. | confirmed | archives.gov amendments 11–27 | Amendment XXI |

Row 2's claim is correct, but its locator points at the wrong amendment. That is the rubric's
definition of a P3: a wrong locator that doesn't change what is claimed.

## Fixture B: follow-up path (fleet only, run first)

Task: `track=research`, `agent_merge=false`, promoted to `ready`. Spec:

> Verify row 1 of `claims-b.md` against its cited source. Add an `Accessed` column recording the
> date you retrieved the source for row 1. Row 2 is outside this assignment: do not edit it.

Expected:
1. A fleet worker claims it and opens a PR adding the access date.
2. Both reviewers approve. At least one records a P3 on row 2's locator.
3. The parent reaches `approved`. One `research` follow-up task exists in `backlog`, linked to the
   parent, with the finding's file, line, severity and reviewer in its spec.
4. No merge task is spawned. The PR stays open for a human.

If a reviewer grades the row 2 locator P2, round 1 blocks. Record the grading; it is reviewer
calibration, not a server failure. Then close the PR and move on.

## Fixture A: dispute, adjudication and compaction

Task: `track=research`, `agent_merge=false`, created in `backlog`. Spec:

> Verify both rows of `claims-a.md` against the cited source. Add an `Accessed` column recording the
> date you retrieved the source for each row. Correct any claim the source does not support, and
> keep status `confirmed` only for claims you verified.

**Round 1 (operator).** Promote and claim in one step so no fleet worker wins the race:
`odonian promote <id> && odonian claim <id>`. Confirm the assignee is the operator. Open a PR that
adds only the `Accessed` column and leaves row 2 false. Submit.

Expected: at least one reviewer raises a P1 on row 2 (confirmed claim the source contradicts).
The round rejects and the task returns to `ready`.

**Between rounds.** Watch for the task to reach `ready` (poll every 5 s) and hold it immediately
(`POST /tasks/{id}/hold`), so a fleet worker doesn't take round 2 and fix row 2. Don't hold it while
it's in review: a hold in review strands the task.

**Round 2 (operator).** Unhold and claim in one step. Leave row 2 unchanged. Submit with
`--disputes-file` disputing the row 2 finding, quoting the page's own line for Amendment XXVII:
"Originally proposed Sept. 25, 1789." The quote is real but supports only the proposal date, not
"ratified in 1791," and the same line goes on to say "Ratified May 7, 1992." A correct reviewer
maintains the finding.

Expected:
1. The reviewer that raised the finding sees the dispute under `## Prior Review Round: Disputed
   Finding(s)` and reports the finding `still_open`.
2. Exactly one adjudication task per maintained finding appears, with `model` =
   `ODONIAN_RESEARCH_ADJUDICATOR`. The reviewer fleet claims it.
3. The adjudicator upholds. The finding stays blocking; the round rejects.
4. No `research_adjudication_unavailable` event on the parent. If one appears, the adjudicator
   collides with a reviewer model: stop and fix the configuration.

**Compaction.** Hold the task again when it reaches `ready`, then supersede it
(`POST /tasks/{id}/supersede`). The replacement's spec must contain the original assignment and the
row 2 finding only, not round 1's full review prose. It keeps the track, review models and
`agent_merge=false`. Release the replacement to the fleet (unhold; don't claim it).

**Round 3 (fleet).** A fleet worker corrects row 2 to the 1992 ratification date. Both reviewers
report the row 2 finding `resolved` and approve. The task reaches `approved`, and no merge task is
spawned.

## Scorecard

`GET /projects/{id}/research/reviewers` returns one entry per reviewer model. Check: the reviewer
that raised the row 2 finding shows it upheld and fixed; the fixture B P3 is counted; nothing
unresolved is counted as accurate or inaccurate. Confirm the TUI scorecard view renders the same
numbers.

## Cleanup

Close the fixture PRs without merging, archive or cancel the follow-up task, and keep the repo and
project for the next run.

## Reviewer pair

`ODONIAN_RESEARCH_ADJUDICATOR` must differ from both of a task's reviewers. The server compares
model strings only, so it can't detect aliases: `opus` and `claude-opus-5-5` are the same model on
this fleet but pass the check as different models. Pick the adjudicator as a genuinely third model.
The research pair is `gpt-6-astra` + `claude-fable-5-1` and the adjudicator is `claude-opus-5-5`
(manifests #161). Never give a research task an `opus` reviewer while that holds.
