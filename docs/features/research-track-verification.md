# Research Track Verification

This document describes the end-to-end verification scenario for the research track feature, covering all acceptance criteria in `docs/features/research-track.md`.

## Overview

The research track is a third task track alongside `build` and `design` for verifying factual claims against primary sources. This verification document provides:
- Setup and deployment configuration required
- End-to-end verification through automated tests
- Expected outcomes for each acceptance criterion
- Regression tests for build and design tracks

## Deployment Configuration

Before running the verification scenario, ensure these environment variables are configured in your Odonian deployment:

```bash
# Required settings for research track
export ODONIAN_RESEARCH_DEFAULT_MODEL=claude-opus-5-5
export ODONIAN_RESEARCH_ADJUDICATOR=claude-fable-5-1  # For adjudication disputes
export ODONIAN_RESEARCH_ROUND_BUDGET=6  # Recommended: 6 rounds before decompose block
export ODONIAN_RESEARCH_ESCALATION_LADDER=""  # Empty by default: no escalation
export ODONIAN_RESEARCH_ESCALATION_THRESHOLDS=""  # Empty when ladder is empty

# Allowed models must include the research default and adjudicator
export ODONIAN_ALLOWED_MODELS="haiku,sonnet,opus,claude-opus-5-5,claude-fable-5-1"

# Optional: test with research escalation enabled
# export ODONIAN_RESEARCH_ESCALATION_LADDER=claude-opus-5-5,claude-fable-5-1
# export ODONIAN_RESEARCH_ESCALATION_THRESHOLDS=3,2
```

## Verification Approach

The research track verification uses automated test coverage in `internal/store/store_test.go:TestResearchTrack_EndToEndVerification`.
This test suite covers all acceptance criteria through programmatic scenario execution.

Run the complete end-to-end verification with:

```bash
go test -count=1 ./internal/store -run TestResearchTrack_EndToEndVerification -v
```

## Acceptance Criteria Verification

### AC1: Research task with no Makefile passes review when correct

**Test:** `acceptance_1_research_track_no_makefile`

Verified by: Creating a research task and confirming its track is "research", without requiring a Makefile.

Expected outcome: Task.Track == "research"

### AC2: Confirmed claim with inaccessible source fails; pending with access record does not

**Tests:**
- `acceptance_2_confirmed_source_inaccessible_fails`: P1 finding for inaccessible confirmed source blocks the round
- `acceptance_2_pending_source_with_access_record_passes`: Pending source with access record does not block

Verified by:
1. Round 1: P1 finding for inaccessible *confirmed* source blocks the round (causes rejection)
2. Round 2: the claim is downgraded to *pending* with an access attempt recorded. Per
   docs/features/research-track.md section 2, "inaccessibility alone is not a finding"
   once a claim is pending with an access record, so the reviewer raises no new finding
   for it and reports the round 1 finding as `resolved` (the downgrade fixed the
   confirmed-but-unverifiable violation)
3. Round passes with nothing outstanding, so no follow-up is created

Expected outcomes:
- Round 1 with inaccessible confirmed source: Parent returns to "ready" (P1 blocks)
- Round 2 with pending + access record and the prior finding resolved: Parent reaches
  "approved"
- No follow-up task is created (the resolved finding isn't outstanding, and no new
  finding was raised for the pending state itself)

### AC3: Round with only P3 findings passes and creates follow-up tasks

**Test:** `acceptance_3_p3_findings_create_followups`

Verified by: Submitting P3 findings (non-blocking), confirming round passes and follow-up tasks are created.

Expected outcomes:
- Parent task reaches "approved" state (P3 findings don't block)
- One follow-up task created per P3 finding
- Follow-up tasks have track="research" and state="backlog"

### AC4: After round 1, P2 in unchanged text doesn't fail the round but creates follow-up

**Test:** `acceptance_4_p2_unchanged_creates_followup`

Verified by: Round 1 rejects (P2 in changed text), round 2 approves (P2 in unchanged text), follow-up is created.

Expected outcomes:
- Round 1: Blocks (P2 in changed text)
- Round 2: Passes (P2 in unchanged text doesn't block)
- Follow-up task created for the P2 in unchanged text

### AC5: P2 in changed text fails the round from either reviewer

**Tests:** `acceptance_5_p2_changed_text_blocks_opus_rejects`, `acceptance_5_p2_changed_text_blocks_sonnet_rejects`

Verified by: Confirming that P2 in changed text blocks regardless of which reviewer raises it.

Expected outcomes:
- P2 in changed text from Opus blocks despite Sonnet approval
- P2 in changed text from Sonnet blocks despite Opus approval

### AC6: Disputed finding with maintained status triggers adjudication; ruling decides the finding

**Test:** `acceptance_6_dispute_triggers_adjudication`

Verified by:
1. Worker disputes a P2 finding with evidence
2. Reviewer maintains the finding (rejects dispute)
3. Adjudicator task is spawned on configured model
4. Adjudicator submits ruling (upholds or overturns)
5. Ruling is binding for that finding

Expected outcomes:
- Adjudication task created with correct model
- When adjudicator upholds: parent blocked (finding still blocking)
- When adjudicator overturns: parent approved (finding no longer blocks)

### AC7: Round budget blocks task with "decompose" reason; model tier and budget handling

**Tests:**
- `acceptance_7_round_budget_blocks_with_decompose`: Empty-ladder case (no escalation)
- `acceptance_7_configured_ladder_shared_budget`: Configured-ladder case, tier escalation half
- `TestResearchBudget_MultipleSupersessions` (pre-existing, `internal/store/store_test.go`):
  configured-ladder case, chain-wide budget half

Verified by:
1. **Empty-ladder:** Budget exhaustion blocks (no escalation possible), model stays constant
2. **Configured-ladder, escalation half** (`acceptance_7_configured_ladder_shared_budget`):
   rejected rounds on one tier escalate the task to the next tier on the configured
   ladder. This subtest uses an unlimited research round budget, so it only proves the
   tier changes; it does not exercise the budget itself.
3. **Configured-ladder, chain-wide budget half** (`TestResearchBudget_MultipleSupersessions`):
   with a configured ladder and a chain-wide budget of 4, a task rejected on haiku
   auto-escalates to sonnet after its per-tier threshold, is then manually superseded
   again on sonnet, and blocks with reason `decompose` on chain-wide round 4 — the
   fourth rejected round counting across all three tasks in the chain, not the fourth
   round on the final task alone. The block note lists all 4 rounds, oldest first,
   numbered chain-wide.

Expected outcomes:
- **Empty-ladder:** Parent task reaches "blocked" state after budget exhaustion
- **Configured-ladder escalation:** Parent task escalates (superseded) to the next tier
  - Original parent: "superseded" state, SupersededBy points to escalated task
  - Escalated task: Next model tier, track remains "research"
- **Configured-ladder chain-wide budget:** the chain blocks with reason `decompose` once
  the cumulative round count across every tier and supersession reaches the budget,
  even though no single task in the chain reached that many rounds on its own
- All decompose blocks: reason recorded in task events

### AC8: Model assignment and defaults

**Tests:**
- `acceptance_8_explicit_model_preserved`: Explicit model is preserved
- `acceptance_8_research_default_model_applied`: Task without model gets ODONIAN_RESEARCH_DEFAULT_MODEL
- `acceptance_8_build_design_defaults_unchanged`: Build and design use their normal defaults

Expected outcomes:
- Research task with explicit model keeps that model
- Research task without explicit model gets research default (e.g., "opus")
- Build and design tasks use their standard defaults unchanged

### AC9: Superseded research spec contains original assignment and last round's unresolved findings

**Test:** `acceptance_9_supersede_spec_compaction`

Verified by:
1. Create research task with original spec
2. Round 1: Reject with multiple findings (f1, f2)
3. Round 2: Reject with some findings resolved (f2 resolved, f1 still_open)
4. Call SupersedeTask to create a new task from the rejected parent
5. Verify new task's spec compaction

Expected outcomes:
- Original parent reaches "superseded" state, SupersededBy points to new task
- New task spec contains original assignment ("Verify the claims in the doc")
- New task spec contains unresolved finding (f1 still_open)
- New task spec does NOT contain resolved finding (f2) or round 1-only feedback
- Compacted spec is shorter than original + all history combined

### AC10: Build and design tracks unchanged

**Test:** `acceptance_10_build_design_unchanged`

Verified by: Creating build task in parallel with research task, confirming it behaves as before.

Expected outcomes:
- Build task track="build" (unaffected)
- Build task behavior unchanged by research track addition

## Scorecard API and Reviewer Performance Tracking

**Test:** `acceptance_scorecard_api_and_tui_read`

Verified by:
1. Create research task with multiple review rounds
2. Call GetResearchReviewerScorecards() API
3. Verify scorecard contains expected fields for each reviewer model
4. Confirm findings_raised and other metrics are populated correctly

Expected outcomes:
- GetResearchReviewerScorecards(ctx, projID) returns scorecard for each reviewer model
- Scorecard contains ReviewerModel, FindingsRaised, TotalReviewRounds, and other metrics
- Metrics accurately reflect the reviews submitted in the scenario
- The TUI view is covered by existing scorecard tests (internal/cmd/odonian-tui/scorecard_test.go)

## Human Merge Gate Verification

**Test:** `acceptance_human_merge_gate`

Verified by:
1. Create research task and get it to "approved" state
2. Verify no merge-kind task is spawned (agent_merge=false for research)
3. Confirm approved task stays in "approved" state pending human decision

Expected outcome:
- Approved research task does not trigger automatic merge-kind task creation
- Task remains in "approved" state awaiting human review and merge decision
- This enforces the human merge gate: only humans can merge research findings

## Regression Testing

To verify build and design tracks are unaffected:

```bash
# Run all tests to verify no regressions
make test

# Or run store tests specifically to ensure build/design behavior is unchanged
go test -count=1 ./internal/store -v

# The end-to-end scenario includes acceptance_10_build_design_unchanged
# which verifies build task behavior is not affected by research track addition
```

All existing tests should pass unchanged. The research track verification test
includes explicit regression checks (acceptance_10_build_design_unchanged) to ensure
parallel build/design tasks operate independently.

## Test Execution Summary

Run the complete verification:

```bash
# Full test run including research track end-to-end
make test

# Just research track tests
go test -count=1 ./internal/store -run TestResearchTrack_EndToEndVerification -v

# Just research aggregation tests
go test -count=1 ./internal/store -run TestResearchAggregation -v

# Adjudication tests
go test -count=1 ./internal/store -run TestResearchAdjudication -v
```

## Completion Checklist

- [x] AC1: Research track created without Makefile requirement
- [x] AC2a: Confirmed claim with inaccessible source fails round
- [x] AC2b: Pending with access record doesn't fail (round 2 resolves the round-1
      finding and raises no new one; round passes with no follow-up)
- [x] AC3: P3 findings pass and create follow-ups
- [x] AC4: P2 in unchanged text creates follow-up (not blocking after round 1)
- [x] AC5: P2 in changed text blocks from either reviewer
- [x] AC6: Disputed findings trigger adjudication; ruling decides finding
- [x] AC7a: Round budget blocks with "decompose" (empty-ladder case)
- [x] AC7b: Configured-ladder tier escalation (`acceptance_7_configured_ladder_shared_budget`)
      and chain-wide shared budget across tiers (`TestResearchBudget_MultipleSupersessions`)
- [x] AC8a: Explicit model preserved
- [x] AC8b: Research default model applied
- [x] AC8c: Build/design defaults unchanged (verified with research default set)
- [x] AC9: Supersede spec compaction (SupersedeTask called, spec verified)
- [x] AC10: Build and design unchanged
- [x] Scorecard API read (GetResearchReviewerScorecards tested)
- [x] Human merge gate (approved task does not spawn merge-kind task)
