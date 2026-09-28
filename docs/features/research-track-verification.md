# Research Track Verification

This document describes the end-to-end verification scenario for the research track feature, covering all acceptance criteria in `docs/features/research-track.md`.

## Overview

The research track is a third task track alongside `build` and `design` for verifying factual claims against primary sources. This verification document provides:
- Setup and deployment configuration required
- Commands to exercise the full end-to-end workflow
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

## Verification Scenario: Research Task End-to-End Workflow

This scenario exercises the complete research track workflow, covering all acceptance criteria.

### Phase 1: Task Setup and Track Validation

**Acceptance Criterion 1:** A research task with no Makefile passes review when its content is correct.

**Acceptance Criterion 8:** A research task created without a model gets `ODONIAN_RESEARCH_DEFAULT_MODEL`; one created with a model keeps it; build and design defaults are unchanged.

#### Commands:

```bash
# 1. Create a research task without specifying a model
TASK_ID=$(odonian task-create \
  --project "$PROJECT_ID" \
  --title "Verify Security Advisory Claims" \
  --spec "Review claims in security_review.md against primary sources" \
  --track research \
  --kind implement \
  --document-id "$DOC_ID" \
  --review-models opus,sonnet \
  --json | jq -r '.id')

echo "Created research task: $TASK_ID"

# 2. Verify task model is set to research default
odonian show "$TASK_ID" | jq '.model'
# Expected output: "claude-opus-5-5" (ODONIAN_RESEARCH_DEFAULT_MODEL)

# 3. Create a research task WITH explicit model
TASK_ID_EXPLICIT=$(odonian task-create \
  --project "$PROJECT_ID" \
  --title "Verify Claims with Explicit Model" \
  --spec "Review against sources" \
  --track research \
  --model haiku \
  --kind implement \
  --document-id "$DOC_ID" \
  --review-models opus,sonnet \
  --json | jq -r '.id')

# 4. Verify explicit model is preserved
odonian show "$TASK_ID_EXPLICIT" | jq '.model'
# Expected output: "haiku" (not changed to research default)
```

#### Expected Outcomes:
- ✓ Research task created with track="research"
- ✓ Task without explicit model gets `ODONIAN_RESEARCH_DEFAULT_MODEL`
- ✓ Task with explicit model keeps that model
- ✓ No Makefile required in repository

### Phase 2: Initial Review and Findings

**Acceptance Criterion 2:** A claim marked confirmed whose source the reviewer can't open fails the round. The same claim marked pending with an access record does not.

**Acceptance Criterion 3:** A round with only P3 findings passes and creates one follow-up task per finding.

#### Commands:

```bash
# 1. Promote research task to ready
odonian promote "$TASK_ID"

# 2. Claim the implement task
odonian claim "$TASK_ID" --agent-id "$AGENT_ID" --model haiku

# 3. Submit implementation with PR link
PR_ID=https://github.com/owner/repo/pull/100
odonian submit "$TASK_ID" \
  --result "Verified claims against sources" \
  --pr "$PR_ID" \
  --branch "mr/research-verification"

# 4. Find the two review tasks (opus and sonnet)
OPUS_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID,model=opus" | jq -r '.[0].id')
SONNET_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID,model=sonnet" | jq -r '.[0].id')

# 5. Claim and submit reviews with findings
# Scenario A: Claim marked "confirmed" with inaccessible source (should fail round)
FINDINGS_P1='[
  {
    "id": "finding-1",
    "severity": "P1",
    "file": "security.md",
    "line": 42,
    "summary": "Source claim1.pdf is cited but not accessible",
    "in_changed_text": true,
    "status": "new"
  }
]'

odonian claim "$OPUS_REVIEW" --agent-id "opus-reviewer" --model opus
odonian submit "$OPUS_REVIEW" \
  --verdict reject \
  --findings-file <(echo "$FINDINGS_P1") \
  --result "Found P1 finding: inaccessible source"
  
# 6. Submit sonnet review with P3 findings only
FINDINGS_P3='[
  {
    "id": "finding-p3-1",
    "severity": "P3",
    "file": "security.md",
    "line": 15,
    "summary": "Page number reference should be 'p. 45' not 'p.45'",
    "in_changed_text": false,
    "status": "new"
  },
  {
    "id": "finding-p3-2",
    "severity": "P3",
    "file": "security.md",
    "line": 28,
    "summary": "Footnote number mismatch: should be [3] not [2]",
    "in_changed_text": false,
    "status": "new"
  }
]'

odonian claim "$SONNET_REVIEW" --agent-id "sonnet-reviewer" --model sonnet
odonian submit "$SONNET_REVIEW" \
  --verdict approve \
  --findings-file <(echo "$FINDINGS_P3") \
  --result "P3 findings only, content is correct"

# 7. Check parent task state (should be rejected due to P1 blocking finding)
odonian show "$TASK_ID" | jq '.state'
# Expected output: "rejected"

# 8. Verify follow-up tasks created for P3 findings
FOLLOWUPS=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID,kind=follow_up" | jq length)
# Expected output: 2 (one per P3 finding)
```

#### Expected Outcomes:
- ✓ Round fails when P1 finding for inaccessible confirmed source is raised
- ✓ Round aggregates findings: any blocking finding from either reviewer fails it
- ✓ Non-blocking findings (P3) create follow-up tasks
- ✓ Follow-up tasks are deduplicated by file, line, and summary

### Phase 3: Rework and Severity Aggregation

**Acceptance Criterion 4:** After round 1, a P2 finding in unchanged text doesn't fail the round, and it creates a follow-up task.

**Acceptance Criterion 5:** A P2 finding in changed text from either reviewer fails the round, even if the other reviewer approves.

#### Commands:

```bash
# 1. Rework the task after rejection
odonian claim "$TASK_ID" --agent-id "$AGENT_ID" --model haiku

# 2. Update PR with fixes (addressing the inaccessible source)
# Simulate rework by updating the branch
git fetch origin
git checkout mr/research-verification
# Make fixes to address the P1 finding...
git add .
git commit -m "Fix: Make source accessible or downgrade claim to pending"
git push origin HEAD:mr/research-verification

# 3. Submit rework
odonian submit "$TASK_ID" \
  --result "Addressed P1 finding: downgraded unverifiable claim to pending" \
  --pr "$PR_ID" \
  --branch "mr/research-verification"

# 4. Claim opus review again (new round)
OPUS_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID,model=opus" | jq -r '.[0].id')
SONNET_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID,model=sonnet" | jq -r '.[0].id')

# 5. Opus review: Report P2 finding in UNCHANGED text (should not fail)
FINDINGS_P2_UNCHANGED='[
  {
    "id": "finding-2",
    "severity": "P2",
    "file": "security.md",
    "line": 35,
    "summary": "Claim overstates source: says 'definitively proven' but source only suggests",
    "in_changed_text": false,
    "status": "new"
  }
]'

odonian claim "$OPUS_REVIEW" --agent-id "opus-reviewer" --model opus
odonian submit "$OPUS_REVIEW" \
  --verdict approve \
  --findings-file <(echo "$FINDINGS_P2_UNCHANGED") \
  --result "P2 in unchanged text, content otherwise correct"

# 6. Sonnet review: Approve with no findings
odonian claim "$SONNET_REVIEW" --agent-id "sonnet-reviewer" --model sonnet
odonian submit "$SONNET_REVIEW" \
  --verdict approve \
  --findings-file '[]' \
  --result "Round approved"

# 7. Check task state (should be approved despite P2 in unchanged text)
odonian show "$TASK_ID" | jq '.state'
# Expected output: "approved"

# 8. Verify follow-up created for P2 in unchanged text
FOLLOWUPS=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID,kind=follow_up" | jq length)
# Expected: at least one new follow-up for the P2 finding

# 9. NEW ROUND: Test P2 in CHANGED text (should fail)
# Create new rework with changes to claim on line 35
odonian claim "$TASK_ID" --agent-id "$AGENT_ID" --model haiku
# ... make changes to line 35 ...
git add .
git commit -m "Update security claim on line 35"
git push origin HEAD:mr/research-verification

odonian submit "$TASK_ID" \
  --result "Updated claim with new source evidence" \
  --pr "$PR_ID" \
  --branch "mr/research-verification"

# Opus review: Report P2 in CHANGED text
FINDINGS_P2_CHANGED='[
  {
    "id": "finding-3",
    "severity": "P2",
    "file": "security.md",
    "line": 35,
    "summary": "New wording still overstates the evidence",
    "in_changed_text": true,
    "status": "new"
  }
]'

odonian claim "$OPUS_REVIEW" --agent-id "opus-reviewer" --model opus
odonian submit "$OPUS_REVIEW" \
  --verdict reject \
  --findings-file <(echo "$FINDINGS_P2_CHANGED") \
  --result "P2 in changed text blocks the round"

# Sonnet review: Approve despite P2 (doesn't matter)
odonian claim "$SONNET_REVIEW" --agent-id "sonnet-reviewer" --model sonnet
odonian submit "$SONNET_REVIEW" \
  --verdict approve \
  --findings-file '[]' \
  --result "Looks good to me"

# 10. Check task state (should be rejected because P2 in changed text)
odonian show "$TASK_ID" | jq '.state'
# Expected output: "rejected"
```

#### Expected Outcomes:
- ✓ P2 in unchanged text after round 1 creates follow-up but doesn't fail
- ✓ P2 in changed text fails the round regardless of other reviewer's verdict
- ✓ Aggregation rules properly distinguish blocking vs. non-blocking findings

### Phase 4: Round Budget and Decomposition Block

**Acceptance Criterion 7:** A research task that reaches its round budget is blocked with reason `decompose`. With no research ladder configured its model tier never changes; with one configured, rounds on every tier count toward the same budget.

#### Commands:

```bash
# 1. Create a new research task that will hit the round budget
TASK_ID_BUDGET=$(odonian task-create \
  --project "$PROJECT_ID" \
  --title "Complex Verification (Budget Test)" \
  --spec "Verify complex cross-file mapping" \
  --track research \
  --kind implement \
  --document-id "$DOC_ID" \
  --review-models opus,sonnet \
  --escalate false \
  --json | jq -r '.id')

# 2. Promote and proceed through rounds, rejecting with findings each time
odonian promote "$TASK_ID_BUDGET"

for round in {1..6}; do
  echo "Round $round..."
  
  # Claim and submit implement task
  odonian claim "$TASK_ID_BUDGET" --agent-id "agent-$round" --model haiku
  odonian submit "$TASK_ID_BUDGET" \
    --result "Round $round attempt" \
    --pr "$PR_ID"
  
  # Get review tasks
  OPUS_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_BUDGET,model=opus,state=ready" | jq -r '.[0].id')
  SONNET_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_BUDGET,model=sonnet,state=ready" | jq -r '.[0].id')
  
  # Both reviewers reject with P2 findings in changed text
  FINDING="[{\"id\": \"f-$round\", \"severity\": \"P2\", \"file\": \"test.md\", \"line\": $((10+round)), \"summary\": \"Issue round $round\", \"in_changed_text\": true, \"status\": \"new\"}]"
  
  odonian claim "$OPUS_REVIEW" --agent-id "opus-reviewer" --model opus
  odonian submit "$OPUS_REVIEW" --verdict reject --findings-file <(echo "$FINDING")
  
  odonian claim "$SONNET_REVIEW" --agent-id "sonnet-reviewer" --model sonnet
  odonian submit "$SONNET_REVIEW" --verdict reject --findings-file <(echo "$FINDING")
done

# 3. Check final state after 6 rejections (hitting budget)
STATUS=$(odonian show "$TASK_ID_BUDGET" | jq '.state,.block_reason')
# Expected output: "blocked" and "decompose"

# 4. Verify model tier didn't change (no escalation with empty ladder)
TASK_MODEL=$(odonian show "$TASK_ID_BUDGET" | jq '.model')
# Expected output: "claude-opus-5-5" (same as start)
```

#### Expected Outcomes:
- ✓ Task is blocked with reason "decompose" when round budget is exhausted
- ✓ Without escalation ladder configured, model tier stays constant
- ✓ Budget counts rejected rounds across entire supersede chain

### Phase 5: Disputes and Adjudication

**Acceptance Criterion 6:** A disputed finding that its reviewer maintains spawns one adjudication task on the configured model, and its ruling decides that finding.

#### Commands:

```bash
# 1. Create a task to test dispute workflow
TASK_ID_DISPUTE=$(odonian task-create \
  --project "$PROJECT_ID" \
  --title "Dispute Resolution Test" \
  --spec "Claims about disputed sources" \
  --track research \
  --kind implement \
  --document-id "$DOC_ID" \
  --review-models opus,sonnet \
  --json | jq -r '.id')

odonian promote "$TASK_ID_DISPUTE"
odonian claim "$TASK_ID_DISPUTE" --agent-id "agent-dispute" --model haiku
odonian submit "$TASK_ID_DISPUTE" \
  --result "Initial implementation" \
  --pr "$PR_ID"

# 2. First round: Opus raises finding, worker disputes it on rework
OPUS_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_DISPUTE,model=opus" | jq -r '.[0].id')
SONNET_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_DISPUTE,model=sonnet" | jq -r '.[0].id')

DISPUTED_FINDING='[{
  "id": "f-dispute",
  "severity": "P2",
  "file": "claims.md",
  "line": 10,
  "summary": "Source interpretation is debatable",
  "in_changed_text": true,
  "status": "new"
}]'

odonian claim "$OPUS_REVIEW" --agent-id "opus-reviewer" --model opus
odonian submit "$OPUS_REVIEW" \
  --verdict reject \
  --findings-file <(echo "$DISPUTED_FINDING") \
  --result "Found P2 issue"

odonian claim "$SONNET_REVIEW" --agent-id "sonnet-reviewer" --model sonnet
odonian submit "$SONNET_REVIEW" \
  --verdict approve \
  --findings-file '[]'

# 3. Rework: Worker disputes the finding with evidence
odonian claim "$TASK_ID_DISPUTE" --agent-id "agent-dispute" --model haiku
odonian submit "$TASK_ID_DISPUTE" \
  --result "Rework with dispute evidence" \
  --pr "$PR_ID" \
  --dispute '{
    "finding_id": "f-dispute",
    "evidence": "The source clearly states X, supporting our interpretation. See page 23."
  }'

# 4. Second round: Opus maintains the finding (dispute should trigger adjudication)
OPUS_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_DISPUTE,model=opus,round=2" | jq -r '.[0].id')

MAINTAINED_FINDING='[{
  "id": "f-dispute",
  "severity": "P2",
  "file": "claims.md",
  "line": 10,
  "summary": "Source interpretation is debatable",
  "in_changed_text": true,
  "status": "still_open",
  "prior_id": "f-dispute"
}]'

odonian claim "$OPUS_REVIEW" --agent-id "opus-reviewer" --model opus
odonian submit "$OPUS_REVIEW" \
  --verdict reject \
  --findings-file <(echo "$MAINTAINED_FINDING") \
  --result "Finding still applies"

# 5. Verify adjudication task created
ADJ_TASKS=$(odonian list --project "$PROJECT_ID" --filter "type=adjudication,parent=$TASK_ID_DISPUTE")
echo "Adjudication tasks: $(echo "$ADJ_TASKS" | jq length)"
# Expected: 1

# 6. Check adjudication task is assigned to configured adjudicator model
ADJ_TASK=$(echo "$ADJ_TASKS" | jq -r '.[0]')
ADJ_MODEL=$(echo "$ADJ_TASK" | jq '.model')
# Expected output: "claude-fable-5-1" (ODONIAN_RESEARCH_ADJUDICATOR)

# 7. Submit adjudication: Fable overturns the finding
ADJ_ID=$(echo "$ADJ_TASK" | jq -r '.id')
odonian claim "$ADJ_ID" --agent-id "adjudicator" --model claude-fable-5-1
odonian submit "$ADJ_ID" \
  --verdict approve \
  --result "Worker's evidence is convincing; finding is overturned" \
  --finding-ruling '{
    "finding_id": "f-dispute",
    "ruling": "overturned",
    "rationale": "The evidence provided shows the interpretation is supported"
  }'

# 8. Verify finding is resolved in parent task
PARENT=$(odonian show "$TASK_ID_DISPUTE")
# Check that the finding is marked as "resolved" in the context
# This may require checking task events or a detailed findings listing
```

#### Expected Outcomes:
- ✓ Disputed finding that reviewer maintains triggers adjudication
- ✓ Adjudication task created on configured ODONIAN_RESEARCH_ADJUDICATOR model
- ✓ Adjudicator's ruling is binding for that finding
- ✓ Finding status updated to reflect adjudication result

### Phase 6: Spec Compaction on Supersede

**Acceptance Criterion 9:** A superseded research task's spec contains the original assignment and the last round's unresolved findings only.

#### Commands:

```bash
# 1. Create task for supersede testing
TASK_ID_SUPER=$(odonian task-create \
  --project "$PROJECT_ID" \
  --title "Spec Compaction Test" \
  --spec "Original detailed assignment with full context and instructions" \
  --track research \
  --kind implement \
  --document-id "$DOC_ID" \
  --review-models opus,sonnet \
  --json | jq -r '.id')

odonian promote "$TASK_ID_SUPER"

# 2. Go through multiple rounds, accumulating findings
for round in {1..3}; do
  # Claim and submit
  odonian claim "$TASK_ID_SUPER" --agent-id "agent-$round" --model haiku
  odonian submit "$TASK_ID_SUPER" \
    --result "Round $round" \
    --pr "$PR_ID"
  
  # Get review tasks and submit with findings
  OPUS_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_SUPER,model=opus" | jq -r '.[-1].id')
  SONNET_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_SUPER,model=sonnet" | jq -r '.[-1].id')
  
  FINDINGS="[{\"id\": \"f-$round\", \"severity\": \"P2\", \"file\": \"doc.md\", \"line\": $((10+round)), \"summary\": \"Finding in round $round\", \"in_changed_text\": true, \"status\": \"new\"}]"
  
  odonian claim "$OPUS_REVIEW" --agent-id "opus" --model opus
  odonian submit "$OPUS_REVIEW" --verdict reject --findings-file <(echo "$FINDINGS")
  
  odonian claim "$SONNET_REVIEW" --agent-id "sonnet" --model sonnet
  odonian submit "$SONNET_REVIEW" --verdict reject --findings-file <(echo "$FINDINGS")
done

# 3. After round 3, if some findings are resolved, supersede
# Get the last round's review verdict to check unresolved findings
LAST_OPUS_REVIEW=$(odonian list --project "$PROJECT_ID" --filter "parent=$TASK_ID_SUPER,model=opus" | jq -r '.[-1]')
UNRESOLVED_FINDINGS=$(echo "$LAST_OPUS_REVIEW" | jq '.findings[] | select(.status != "resolved")')

# 4. Check superseded task spec
odonian release "$TASK_ID_SUPER" --for-supersede
SUPERSEDED_TASK=$(odonian list --project "$PROJECT_ID" --filter "supersedes=$TASK_ID_SUPER" | jq -r '.[0]')
SUPERSEDE_ID=$(echo "$SUPERSEDED_TASK" | jq -r '.id')

SUPERSEDED_SPEC=$(echo "$SUPERSEDED_TASK" | jq '.spec')
echo "Superseded spec length: $(echo "$SUPERSEDED_SPEC" | wc -c)"
# Expected: Much shorter than accumulated history
# Should contain original assignment + only unresolved findings from last round
```

#### Expected Outcomes:
- ✓ Superseded research task spec is compacted
- ✓ Contains original assignment and only unresolved findings
- ✓ Full history is preserved in task events

### Phase 7: Scorecard API and TUI

**Phase 7 is covered by existing scorecard tests.**

Research track includes reviewer scorecard tracking. Verify via:

```bash
# 1. After running reviews, check scorecard API
curl -H "Authorization: Bearer $ODONIAN_TOKEN" \
  "$ODONIAN_URL/api/v1/projects/$PROJECT_ID/research/scorecards"

# Expected response includes per-model statistics:
# {
#   "scorecards": [
#     {
#       "model": "opus",
#       "findings_by_severity": {"P1": 2, "P2": 5, "P3": 1},
#       "findings_upheld": 6,
#       "findings_withdrawn": 1,
#       "rounds_approved_with_hidden_issues": 0
#     },
#     ...
#   ]
# }

# 2. Check TUI scorecard view
odonian tui
# In TUI, navigate to research view to see reviewer scorecards
# Verify opus/sonnet statistics are displayed
```

### Phase 8: Build and Design Track Regression Tests

**Acceptance Criterion 10:** Build and design tasks behave exactly as before, by their existing tests.

#### Commands:

```bash
# 1. Create a build track task (unchanged behavior)
BUILD_TASK=$(odonian task-create \
  --project "$PROJECT_ID" \
  --title "Build Task Regression Test" \
  --spec "Add feature X" \
  --track build \
  --kind implement \
  --document-id "$DOC_ID" \
  --review-models opus,sonnet \
  --json | jq -r '.id')

odonian promote "$BUILD_TASK"
odonian claim "$BUILD_TASK" --agent-id "builder" --model haiku
odonian submit "$BUILD_TASK" \
  --result "Implemented feature" \
  --pr "$PR_ID"

# 2. Verify build track review behavior (should require both approvals)
BUILD_OPUS=$(odonian list --project "$PROJECT_ID" --filter "parent=$BUILD_TASK,model=opus" | jq -r '.[0].id')
BUILD_SONNET=$(odonian list --project "$PROJECT_ID" --filter "parent=$BUILD_TASK,model=sonnet" | jq -r '.[0].id')

# Submit opus approval
odonian claim "$BUILD_OPUS" --agent-id "opus" --model opus
odonian submit "$BUILD_OPUS" --verdict approve

# Submit sonnet approval
odonian claim "$BUILD_SONNET" --agent-id "sonnet" --model sonnet
odonian submit "$BUILD_SONNET" --verdict approve

# 3. Verify task is approved (requires both)
BUILD_STATUS=$(odonian show "$BUILD_TASK" | jq '.state')
# Expected: "approved"

# 4. Create a design track task (unchanged behavior)
DESIGN_TASK=$(odonian task-create \
  --project "$PROJECT_ID" \
  --title "Design Task Regression Test" \
  --spec "Design proposal for X" \
  --track design \
  --kind implement \
  --document-id "$DOC_ID" \
  --review-models opus,sonnet \
  --json | jq -r '.id')

odonian promote "$DESIGN_TASK"
odonian claim "$DESIGN_TASK" --agent-id "designer" --model haiku
odonian submit "$DESIGN_TASK" \
  --result "Design complete" \
  --pr "$PR_ID"

# 5. Verify design track behavior (should behave like build, unchanged)
DESIGN_STATUS=$(odonian show "$DESIGN_TASK" | jq '.state')
# Expected: Follows build track rules (both reviewers must approve)
```

#### Expected Outcomes:
- ✓ Build track behavior unchanged
- ✓ Design track behavior unchanged
- ✓ Research track does not affect other tracks

## Verification Checklist

Run this checklist to verify all acceptance criteria:

- [ ] **AC1**: Research task with no Makefile passes when content is correct
- [ ] **AC2**: Confirmed claim with inaccessible source fails; pending with access record does not
- [ ] **AC3**: Round with only P3 findings passes and creates follow-up tasks
- [ ] **AC4**: P2 in unchanged text after round 1 doesn't fail, creates follow-up
- [ ] **AC5**: P2 in changed text fails round from either reviewer
- [ ] **AC6**: Disputed finding maintained triggers adjudication; adjudicator ruling binds
- [ ] **AC7**: Round budget blocks with `decompose`; no escalation without ladder configured
- [ ] **AC8**: Task without model gets research default; with model keeps it
- [ ] **AC9**: Superseded research spec contains original + last round's unresolved only
- [ ] **AC10**: Build and design tasks behave exactly as before

## Deployment and Testing Notes

### Running Locally

To run the full verification scenario locally:

```bash
# Start the Odonian server with test configuration
export ODONIAN_DATABASE=test.db
export ODONIAN_RESEARCH_DEFAULT_MODEL=claude-opus-5-5
export ODONIAN_RESEARCH_ADJUDICATOR=claude-fable-5-1
export ODONIAN_ALLOWED_MODELS=haiku,sonnet,opus,claude-opus-5-5,claude-fable-5-1

# Run the server
odonian server

# In another terminal, run verification commands
export ODONIAN_URL=http://localhost:8080
export ODONIAN_TOKEN=test-token
export PROJECT_ID=<created-project-id>
export DOC_ID=<created-document-id>

# Execute the phases described above
```

### Key Observations

1. **Evidence Verification**: The research review prompt checks that confirmed claims' sources are actually retrievable. This is core to the research track's value over build reviews.

2. **Finding Severity**: P1/P2/P3 distinction is critical to the aggregation logic:
   - P1 and P2 in changed text → blocks
   - P1 and P2 in unchanged text after round 1 → follow-up only
   - P3 always → follow-up only

3. **Round Budget**: The default budget of 6 prevents tasks from cycling endlessly. Reaching it should trigger decomposition, not escalation.

4. **Adjudication**: Requires ODONIAN_RESEARCH_ADJUDICATOR to be configured and different from both reviewers. If unset, disputes stay blocking.

5. **Spec Compaction**: Prevents spec bloat during reworks. Research tasks supersede with clean specs; build/design are unchanged.

## Known Limitations and Future Work

- The current verification uses synthetic examples. Real research workflows may expose additional edge cases.
- Reviewer scorecards are tracked but scoring methodology may be refined based on usage.
- Research task sizing guidance is available in the `odonian-breakdown` skill but may need field validation.
- Evidence tooling (section 9 in research-track.md) is designed but not yet implemented; workflows use manual verification.

## Related Documentation

- `docs/features/research-track.md` — Full specification
- `internal/store/store_test.go` — Existing test coverage for research track features
- `prompts/pull_request/research/` — Research track prompts (implement and review)
- `docs/api.md` — API documentation for research task operations
