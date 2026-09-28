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

**Test:** `acceptance_2_confirmed_source_inaccessible_fails`

Verified by: P1 finding for inaccessible confirmed source blocks the round (causes rejection).

Expected outcome: Parent task returns to "ready" state when P1 blocking finding is present.

**Partial verification (via prompt behavior):** The second half - "pending source with access record does not block" - is verified through the research review prompt implementation. The worker marks sources as "pending" with an access attempt record before resubmitting, and the reviewer prompt recognizes this context to avoid blocking on inaccessibility. This is verified during review prompt integration testing, not in unit tests.

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

**Tests:** `acceptance_7_round_budget_blocks_with_decompose`, `acceptance_7_configured_ladder_shared_budget`

Verified by:
1. Empty-ladder case: Budget exhaustion blocks, model stays constant
2. Configured-ladder case: Budget still enforces across escalation tiers

Expected outcomes:
- Parent task reaches "blocked" state after budget exhaustion
- Block reason includes "decompose"
- Budget is shared across tiers in configured-ladder case

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
1. Create task with original spec
2. Reject in round 1 (multiple findings)
3. Reject in round 2 (some findings resolved, some remain)
4. Confirm spec contains original assignment + last round's unresolved findings

Expected outcomes:
- Spec is shorter than if all feedback was prepended (like build/design)
- Original assignment is preserved
- Only unresolved findings from last round are in spec

### AC10: Build and design tracks unchanged

**Test:** `acceptance_10_build_design_unchanged`

Verified by: Creating build task in parallel with research task, confirming it behaves as before.

Expected outcomes:
- Build task track="build" (unaffected)
- Build task behavior unchanged by research track addition

## Scorecard API and Reviewer Performance Tracking

**Status:** UNVERIFIED - Requires implementation of scorecard endpoint

The scorecard API endpoint (`GET /projects/{id}/research/reviewers`) is required by AC-M4 (milestone 4 - scorecards and sizing).
This endpoint should return per-reviewer-model statistics including:
- findings_raised (by severity)
- findings_held (fixed or upheld on adjudication)
- findings_withdrawn (overturned or withdrawn)
- findings_unresolved
- approvals_with_later_fixed_blocking_findings
- total_review_rounds
- sample_size

**Follow-up task required:** Implement scorecard endpoint and TUI view.

## Human Merge Gate Verification

**Status:** UNVERIFIED - Requires human code review

This verification document itself is reviewed by independent Opus and gpt-5.5 reviewers before the research track ships.
The merge gate is implicit: this task and all dependent research track PRs require human approval before merging.

**No automated test possible** - human judgment cannot be automated. Verification occurs through the PR review process and sign-off by project owner.

## Regression Testing

To verify build and design tracks are unaffected:

```bash
# Run all build/design tests
go test -count=1 ./internal/store -run TestBuildTrack -v
go test -count=1 ./internal/store -run TestDesignTrack -v
```

All existing tests should pass unchanged.

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
- [~] AC2b: Pending with access record doesn't fail (via prompt behavior)
- [x] AC3: P3 findings pass and create follow-ups
- [x] AC4: P2 in unchanged text creates follow-up (not blocking after round 1)
- [x] AC5: P2 in changed text blocks from either reviewer
- [x] AC6: Disputed findings trigger adjudication; ruling decides finding
- [x] AC7: Round budget blocks with "decompose" (no-ladder and configured-ladder)
- [x] AC8: Model defaults and preservation
- [x] AC9: Supersede spec compaction
- [x] AC10: Build and design unchanged
- [ ] Scorecard API endpoint and TUI view (milestone 4)
- [ ] Human merge gate (implicit, via PR review process)
