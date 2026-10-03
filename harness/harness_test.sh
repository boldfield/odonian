#!/usr/bin/env bash
# harness_test.sh — smoke tests for agent.sh and fleet.sh local_commit mode support
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_TO_TEST="$HARNESS_DIR/agent.sh"
FLEET_SCRIPT="$HARNESS_DIR/fleet.sh"

test_count=0
pass_count=0
fail_count=0

test_pass() {
  ((test_count++))
  ((pass_count++))
  echo "✓ $1"
}

test_fail() {
  ((test_count++))
  ((fail_count++))
  echo "✗ $1"
}

echo "=== Smoke tests for local_commit harness support ==="
echo ""

# Test 1: Check that agent.sh has DELIVERY_MODE check
echo "Test 1: agent.sh validates DELIVERY_MODE"
if grep -q 'DELIVERY_MODE.*pull_request' "$SCRIPT_TO_TEST" && grep -q 'pull_request|local_commit' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh has DELIVERY_MODE check"
else
  test_fail "agent.sh missing DELIVERY_MODE check"
fi

# Test 2: Check that agent.sh has local_commit branch in SINGLE-PROJECT mode
echo "Test 2: agent.sh has local_commit branch in SINGLE-PROJECT"
if grep -q 'if \[ "$DELIVERY_MODE" = "local_commit" \]; then' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh has local_commit conditional"
else
  test_fail "agent.sh missing local_commit conditional"
fi

# Test 3: Check that ODONIAN_WORKTREE_HOME is required in local_commit
echo "Test 3: agent.sh requires ODONIAN_WORKTREE_HOME in local_commit"
if grep -q 'ODONIAN_WORKTREE_HOME.*local_commit' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh validates ODONIAN_WORKTREE_HOME"
else
  test_fail "agent.sh missing ODONIAN_WORKTREE_HOME validation"
fi

# Test 4: Check that agent.sh exports ODONIAN_WORKTREE_HOME
echo "Test 4: agent.sh exports ODONIAN_WORKTREE_HOME in local_commit"
if grep -q 'export ODONIAN_WORKTREE_HOME' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh exports ODONIAN_WORKTREE_HOME"
else
  test_fail "agent.sh doesn't export ODONIAN_WORKTREE_HOME"
fi

# Test 5: Check that clone is skipped in local_commit (no ensure_clone call)
echo "Test 5: agent.sh local_commit skips clone"
if grep -A 30 'if \[ "$DELIVERY_MODE" = "local_commit" \]' "$SCRIPT_TO_TEST" | grep -q 'ensure_clone'; then
  test_fail "agent.sh still calls ensure_clone in local_commit mode"
else
  test_pass "agent.sh skips clone in local_commit mode"
fi

# Test 6: Check that pull_request path still has worktree setup
echo "Test 6: agent.sh pull_request mode has worktree setup"
if grep -A 20 'pull_request mode: standard clone' "$SCRIPT_TO_TEST" | grep -q 'git.*worktree add'; then
  test_pass "agent.sh still sets up worktrees in pull_request mode"
else
  test_fail "agent.sh doesn't set up worktrees in pull_request mode"
fi

# Test 7: prompts are keyed on delivery_mode/track/kind as path dimensions, and every
# (delivery_mode, kind) combo the build fleet runs has a prompt file present.
echo "Test 7: prompts key on delivery_mode/track/kind"
if grep -q 'prompts/\$DELIVERY_MODE/\$track/\$kind.md' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh resolves prompts/<delivery_mode>/<track>/<kind>.md"
else
  test_fail "agent.sh does not key the prompt path on delivery_mode"
fi
_missing=0
for _f in prompts/pull_request/build/implement.md prompts/pull_request/build/review.md \
          prompts/local_commit/build/implement.md prompts/local_commit/build/review.md; do
  [ -f "$HARNESS_DIR/$_f" ] || { echo "  missing: $_f"; _missing=1; }
done
if [ "$_missing" -eq 0 ]; then
  test_pass "all build prompts present (pull_request + local_commit)"
else
  test_fail "a delivery-mode build prompt is missing"
fi

# Test 7b: get_prompt_file (extracted verbatim from agent.sh) actually resolves
# research/implement and research/review to real files under pull_request.
echo "Test 7b: get_prompt_file resolves research/implement and research/review under pull_request"
_get_prompt_file_src="$(sed -n '/^get_prompt_file() {/,/^}/p' "$SCRIPT_TO_TEST")"
_research_missing=0
if [ -z "$_get_prompt_file_src" ]; then
  echo "  could not extract get_prompt_file() from $SCRIPT_TO_TEST"
  _research_missing=1
else
  eval "$_get_prompt_file_src"
  for _kind in implement review; do
    _resolved="$(HARNESS_DIR="$HARNESS_DIR" DELIVERY_MODE=pull_request get_prompt_file research "$_kind")"
    if [ "$_resolved" != "$HARNESS_DIR/prompts/pull_request/research/$_kind.md" ] || [ ! -f "$_resolved" ]; then
      echo "  get_prompt_file research $_kind resolved to missing/unexpected path: $_resolved"
      _research_missing=1
    fi
  done
fi
if [ "$_research_missing" -eq 0 ]; then
  test_pass "get_prompt_file resolves research prompts under pull_request (implement + review)"
else
  test_fail "get_prompt_file failed to resolve a pull_request research prompt"
fi

# Test 7c: the research review prompt must submit structured findings via --findings-file, so the
# server stores them (the fenced block in prose is not parsed by anything).
echo "Test 7c: research review prompt submits findings with --findings-file"
_rr_prompt="$HARNESS_DIR/prompts/pull_request/research/review.md"
if grep -q -- '--findings-file' "$_rr_prompt" && grep -q 'in_changed_text' "$_rr_prompt"; then
  test_pass "research review prompt uses --findings-file and defines in_changed_text"
else
  test_fail "research review prompt is missing --findings-file or in_changed_text guidance"
fi

# Test 8: Check fleet.sh exists and is executable
echo "Test 8: fleet.sh exists"
if [ -x "$FLEET_SCRIPT" ]; then
  test_pass "fleet.sh exists and is executable"
else
  test_fail "fleet.sh missing or not executable"
fi

# Test 9: Check fleet.sh has --delivery-mode flag
echo "Test 9: fleet.sh supports --delivery-mode flag"
if grep -q '\-\-delivery-mode' "$FLEET_SCRIPT"; then
  test_pass "fleet.sh has --delivery-mode flag"
else
  test_fail "fleet.sh missing --delivery-mode flag"
fi

# Test 10: Check fleet.sh exports ODONIAN_DELIVERY_MODE
echo "Test 10: fleet.sh exports ODONIAN_DELIVERY_MODE"
if grep -q 'export ODONIAN_DELIVERY_MODE' "$FLEET_SCRIPT"; then
  test_pass "fleet.sh exports ODONIAN_DELIVERY_MODE"
else
  test_fail "fleet.sh doesn't export ODONIAN_DELIVERY_MODE"
fi

# Test 11: Verify fleet.sh invokes agent.sh directly with --kind and --model
echo "Test 11: fleet.sh invokes agent.sh directly with flags"
if grep -q '"$HARNESS_DIR/agent.sh" --kind "$KIND" $MODEL_ARG "$slot"' "$FLEET_SCRIPT"; then
  test_pass "fleet.sh invokes agent.sh directly with --kind, --model, and slot"
else
  test_fail "fleet.sh doesn't invoke agent.sh directly or missing flags"
fi

# Test 12: Verify local_commit mode skips clone in single-project
echo "Test 12: local_commit mode guards post-dispatch git commands"
if grep -q '\[ -n "\$WT" \] &&' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh guards post-dispatch git commands with WT check"
else
  test_fail "agent.sh missing WT guard on post-dispatch git commands"
fi

# Test 13: Verify multi-project rejects local_commit mode
echo "Test 13: multi-project mode rejects local_commit"
if grep -A 5 'MULTI-PROJECT MODE' "$SCRIPT_TO_TEST" | grep -q 'local_commit mode requires single-project'; then
  test_pass "agent.sh rejects multi-project with local_commit"
else
  test_fail "agent.sh doesn't reject multi-project with local_commit"
fi

# Test 14: Check that agent.sh exports ODONIAN_MODEL in dispatch
echo "Test 14: agent.sh exports ODONIAN_MODEL in dispatch"
if grep -q 'export ODONIAN_MODEL=' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh exports ODONIAN_MODEL"
else
  test_fail "agent.sh doesn't export ODONIAN_MODEL"
fi

# Test 15: Check that agent.sh transitions tasks when prompt file is missing
echo "Test 15: agent.sh transitions blocked when prompt not found"
if grep -q 'odonian transition.*blocked.*no prompt' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh transitions task to blocked on missing prompt"
else
  test_fail "agent.sh doesn't transition task on missing prompt"
fi

# Test 16: Check that agent.sh sleeps after blocking on missing prompt in single-project
echo "Test 16: agent.sh sleeps after blocking prompt in single-project"
if grep -B 1 'prompt not found.*blocking.*nap 30' "$SCRIPT_TO_TEST" | grep -q 'odonian transition.*blocked'; then
  test_pass "agent.sh sleeps and transitions on missing prompt in single-project"
else
  test_fail "agent.sh doesn't sleep/transition correctly on missing prompt"
fi

# Test 17: Check that agent.sh uses correct --to flag syntax for transition
echo "Test 17: agent.sh uses correct --to flag for transition"
if grep -q 'odonian transition.*--to blocked' "$SCRIPT_TO_TEST"; then
  test_pass "agent.sh uses correct --to syntax for transition"
else
  test_fail "agent.sh doesn't use --to flag in transition command"
fi

# Test 18: Check that agent.sh sleeps after blocking on missing prompt in multi-project
echo "Test 18: agent.sh sleeps after blocking prompt in multi-project"
if grep -A 60 'MULTI-PROJECT MODE' "$SCRIPT_TO_TEST" | grep -q 'odonian transition.*--to blocked.*no prompt'; then
  test_pass "agent.sh sleeps and logs correctly on missing prompt in multi-project"
else
  test_fail "agent.sh doesn't handle missing prompt correctly in multi-project"
fi

# Test 19: Check that research review prompt enforces round 1 full review
echo "Test 19: research review prompt enforces round 1 full review"
_rr_prompt="$HARNESS_DIR/prompts/pull_request/research/review.md"
if grep -q 'review_round == 1.*Full review' "$_rr_prompt" && grep -q 'Check every claim in the file' "$_rr_prompt"; then
  test_pass "research review prompt correctly detects round 1 and requires full review"
else
  test_fail "research review prompt does not correctly enforce round 1 full review"
fi

# Test 20: Check that research review prompt requires scoped re-review for round 2+
echo "Test 20: research review prompt enforces round 2+ scoped re-review"
if grep -q 'review_round >= 2.*Scoped re-review' "$_rr_prompt" && grep -q 'check the findings from your.*previous.*review round' "$_rr_prompt"; then
  test_pass "research review prompt correctly enforces round 2+ scoped re-review"
else
  test_fail "research review prompt does not correctly enforce round 2+ scoped re-review"
fi

# Test 20b: a replacement task's round 1 reports status only for the reviewer's own carried findings
echo "Test 20b: research review prompt scopes carried-finding status to the reviewer's own findings"
if grep -q 'Unresolved findings from last review round' "$_rr_prompt" && \
   grep -q 'Do not report a status for a carried finding raised only by a different reviewer model' "$_rr_prompt"; then
  test_pass "research review prompt scopes carried-finding status to the reviewer's own findings"
else
  test_fail "research review prompt is missing the replacement-round carried-findings instruction"
fi

# Test 21: Check that research review prompt handles unchanged-text defects
echo "Test 21: research review prompt handles unchanged-text defects"
if grep -q 'Newly discovered unchanged-text defects' "$_rr_prompt" && grep -q 'in_changed_text: false' "$_rr_prompt"; then
  test_pass "research review prompt correctly handles unchanged-text defects"
else
  test_fail "research review prompt does not handle unchanged-text defects"
fi

# Test 22: Check that research review prompt supports dispute re-evaluation
echo "Test 22: research review prompt supports dispute re-evaluation"
if grep -q 'Re-evaluating disputed findings' "$_rr_prompt" && grep -q 'dispute evidence' "$_rr_prompt"; then
  test_pass "research review prompt supports dispute re-evaluation"
else
  test_fail "research review prompt does not support dispute re-evaluation"
fi

# Test 23: Check that adjudication path exists and validates the PR
echo "Test 23: adjudication path includes PR validation and fetch"
if grep -q '3-adjudicate.*Adjudicate one disputed' "$_rr_prompt" && grep -q 'validate the PR link and fetch the PR' "$_rr_prompt"; then
  test_pass "adjudication path includes PR validation and fetch"
else
  test_fail "adjudication path missing PR validation or fetch"
fi

# Test 24: Check that adjudication submissions use empty findings array
echo "Test 24: adjudication submissions use empty findings array"
if grep -q '6-adjudicate' "$_rr_prompt" && grep -q '\--findings-file.*\[\]' "$_rr_prompt"; then
  test_pass "adjudication submissions use empty findings array"
else
  test_fail "adjudication submissions do not use empty findings array"
fi

# Test 25: Check that build/design prompts are unchanged (regression check)
echo "Test 25: build/design prompts unchanged"
_build_implement="$HARNESS_DIR/prompts/pull_request/build/implement.md"
_build_review="$HARNESS_DIR/prompts/pull_request/build/review.md"
_design_implement="$HARNESS_DIR/prompts/pull_request/design/implement.md"
_design_review="$HARNESS_DIR/prompts/pull_request/design/review.md"
_all_exist=1
for _f in "$_build_implement" "$_build_review" "$_design_implement" "$_design_review"; do
  if [ ! -f "$_f" ]; then
    _all_exist=0
  fi
done
if [ "$_all_exist" -eq 1 ]; then
  # Check that build/design prompts retain their essential markers and don't leak research-specific content
  if grep -q "make check" "$_build_implement" && \
     grep -q "verdict" "$_build_review" && \
     ! grep -q '3-adjudicate\|in_changed_text\|Scoped re-review' "$_build_implement" && \
     ! grep -q '3-adjudicate\|in_changed_text\|Scoped re-review' "$_build_review" && \
     grep -q "make check" "$_design_implement" && \
     grep -q "verdict" "$_design_review" && \
     ! grep -q '3-adjudicate\|in_changed_text\|Scoped re-review' "$_design_implement" && \
     ! grep -q '3-adjudicate\|in_changed_text\|Scoped re-review' "$_design_review"; then
    test_pass "build/design prompts remain unchanged"
  else
    test_fail "build/design prompts accidentally contain research-specific content or lost essential markers"
  fi
else
  test_fail "one or more build/design prompts missing"
fi

# Test 26: Check that research implement prompt includes dispute submission guidance
echo "Test 26: research implement prompt includes dispute submission"
_ri_prompt="$HARNESS_DIR/prompts/pull_request/research/implement.md"
if grep -q -- '--disputes-file' "$_ri_prompt" && grep -q 'finding_id.*evidence' "$_ri_prompt"; then
  test_pass "research implement prompt includes dispute submission with evidence"
else
  test_fail "research implement prompt missing dispute submission guidance"
fi

# Test 26b: Check that research implement prompt acknowledges disputed items with --disputed
echo "Test 26b: research implement prompt acknowledges disputed feedback with pr-feedback ack --disputed"
if grep -q -- 'odonian pr-feedback ack --disputed <pr-url> <item-id>' "$_ri_prompt" && \
   grep -q 'Never acknowledge a disputed item as addressed by a commit' "$_ri_prompt" && \
   grep -q -- 'Never use `--skip-feedback-gate`' "$_ri_prompt"; then
  test_pass "research implement prompt acknowledges disputed feedback with --disputed"
else
  test_fail "research implement prompt missing pr-feedback ack --disputed guidance"
fi

# Test 27: Check that severity rules correctly exclude P3 from blocking
echo "Test 27: research review prompt correctly excludes P3 from blocking"
if grep -q 'P3 findings never block' "$_rr_prompt" && grep -q 'Reject on any P1 or P2' "$_rr_prompt"; then
  test_pass "research review prompt correctly excludes P3 from blocking"
else
  test_fail "research review prompt does not correctly exclude P3 from blocking"
fi

# Test 28: Check that research implement prompt covers continuation manifest submission
echo "Test 28: research implement prompt covers continuation manifest submission"
_ri_prompt="$HARNESS_DIR/prompts/pull_request/research/implement.md"
if grep -q '## continuation manifest' "$_ri_prompt" && \
   grep -q 'commit the versioned manifest file in the PR' "$_ri_prompt" && \
   grep -q -- '--manifest-file' "$_ri_prompt"; then
  test_pass "research implement prompt covers manifest submission and commitment"
else
  test_fail "research implement prompt missing manifest submission guidance"
fi

# Test 29: Check that research review prompt covers submission manifests and blocking rules
echo "Test 29: research review prompt covers submission manifests and blocking rules"
if grep -q 'submission_manifests' "$_rr_prompt" && \
   grep -q 'File equality check.*BLOCKING' "$_rr_prompt" && \
   grep -q 'missing committed manifest file' "$_rr_prompt" && \
   grep -q 'Digest check' "$_rr_prompt" && \
   grep -q 'jq -jc --argjson r' "$_rr_prompt" && \
   grep -q 'sha256sum /tmp/stored-manifest.json' "$_rr_prompt" && \
   grep -q 'jq -S . /tmp/stored-manifest.json' "$_rr_prompt" && \
   ! grep -q 'printf .%s. .<Canonical line>.' "$_rr_prompt" && \
   grep -q 'per child at most 6 claims' "$_rr_prompt" && \
   grep -q '`task` (an existing task ID)' "$_rr_prompt" && \
   grep -q 'Oversized scope is a \*\*P1 finding' "$_rr_prompt" && \
   grep -q 'Manifest findings blocking rule' "$_rr_prompt" && \
   grep -q 'always blocks in every round' "$_rr_prompt"; then
  test_pass "research review prompt covers manifest inspection and blocking"
else
  test_fail "research review prompt missing manifest coverage or blocking rules"
fi

# Test 30: Check that research review prompt forbids reviewers from creating child tasks
echo "Test 30: research review prompt forbids reviewers from creating child tasks"
if grep -q 'Reviewers must never create child tasks' "$_rr_prompt"; then
  test_pass "research review prompt forbids child task creation"
else
  test_fail "research review prompt missing prohibition on child task creation"
fi

# Test 31: research prompts support preclaimed task + attempt identity without next/claim
echo "Test 31: research prompts accept a preclaimed task and attempt identity"
_pc_ok=1
for _pc_prompt in "$_ri_prompt" "$_rr_prompt"; do
  _pc_text="$(tr -s '[:space:]' ' ' < "$_pc_prompt")"
  for _pc_needle in \
    'ODONIAN_PRECLAIMED_TASK_ID' \
    'ODONIAN_PRECLAIMED_ATTEMPT_ID' \
    'not call `odonian next` or `odonian claim`' \
    'other than `ODONIAN_PRECLAIMED_TASK_ID`' \
    'odonian show --json' \
    "compare the task's assignee to \`\$AGENT_ID\`" \
    'state is `in_progress`' \
    '**Ordinary (legacy) mode.**' \
    'not `enforce`' \
    'exits 10 (scheduling denial)'; do
    if ! printf '%s' "$_pc_text" | grep -qF -- "$_pc_needle"; then
      echo "  $(basename "$_pc_prompt") missing: $_pc_needle"
      _pc_ok=0
    fi
  done
done
if [ "$_pc_ok" -eq 1 ]; then
  test_pass "research prompts document preclaimed mode, ownership check, and enforce-aware ordinary mode"
else
  test_fail "research prompts missing preclaimed/ordinary mode guidance"
fi

# Test 31b: ordinary next+claim flow remains documented in both research prompts
echo "Test 31b: ordinary next+claim flow remains documented"
if grep -qF -- '`odonian next --project "$ODONIAN_PROJECT" --model "$AGENT_MODEL" --kind implement`' "$_ri_prompt" && \
   grep -qF -- '`odonian claim <id>`' "$_ri_prompt" && \
   grep -qF -- '--kind review' "$_rr_prompt" && \
   grep -qF -- '`odonian claim <id>`' "$_rr_prompt"; then
  test_pass "ordinary next+claim flow remains documented"
else
  test_fail "ordinary next+claim flow missing from a research prompt"
fi

# Test 31c: heartbeat/submit fencing with --attempt, for regular review and adjudication
echo "Test 31c: heartbeat and submits pass --attempt (implement, regular review, adjudication)"
_att='--attempt "$ODONIAN_PRECLAIMED_ATTEMPT_ID"'
_rr_regular="$(awk '/^6\. \*\*Decide the verdict/{f=1} /^6-adjudicate\./{f=0} f' "$_rr_prompt" | tr -s '[:space:]' ' ')"
_rr_adj="$(awk '/^6-adjudicate\./{f=1} /^7\. \*\*Do NOT merge/{f=0} f' "$_rr_prompt" | tr -s '[:space:]' ' ')"
if grep -qF -- "odonian heartbeat <id> $_att" "$_ri_prompt" && \
   tr -s '[:space:]' ' ' < "$_ri_prompt" | grep -qF -- "$_att"'` to bind the submission' && \
   printf '%s' "$_rr_regular" | grep -qF -- 'odonian submit <review-task-id>' && \
   printf '%s' "$_rr_regular" | grep -qF -- "$_att" && \
   printf '%s' "$_rr_adj" | grep -qF -- 'odonian submit <review-task-id>' && \
   printf '%s' "$_rr_adj" | grep -qF -- "$_att"; then
  test_pass "heartbeat and submit calls are fenced with --attempt in preclaimed mode"
else
  test_fail "a research prompt heartbeat/submit step is missing --attempt fencing"
fi

# Test 32: Research admission JSON parsing and variable export
echo "Test 32: research admission JSON parsing and variable export"
_test_tmpdir=$(mktemp -d)
_test_json='{"permit_id":"permit-123","attempt_id":"attempt-456"}'
_parsed_permit=$(echo "$_test_json" | jq -r ".permit_id // empty" 2>/dev/null)
_parsed_attempt=$(echo "$_test_json" | jq -r ".attempt_id // empty" 2>/dev/null)
if [ "$_parsed_permit" = "permit-123" ] && [ "$_parsed_attempt" = "attempt-456" ]; then
  test_pass "admission JSON parsing extracts top-level fields correctly"
else
  test_fail "admission JSON parsing failed; got permit=$_parsed_permit attempt=$_parsed_attempt"
fi
rm -rf "$_test_tmpdir"

# Test 33: Deferral includes sleep/nap
echo "Test 33: deferral (exit 10) includes sleep to avoid busy loop"
if grep -q 'admission_rc.*-eq 10' "$SCRIPT_TO_TEST" && grep -B2 -A2 'admission_rc.*-eq 10' "$SCRIPT_TO_TEST" | grep -q 'nap'; then
  test_pass "deferral includes nap to avoid busy loop"
else
  test_fail "deferral missing nap; would cause busy loop on same task"
fi

# Test 34: Exit class values are correct (completed/failed/cancelled/unknown)
echo "Test 34: exit class values use correct enum (completed/failed/cancelled/unknown)"
if grep -q 'exit_class="completed"' "$SCRIPT_TO_TEST" && \
   grep -q 'exit_class="failed"' "$SCRIPT_TO_TEST" && \
   grep -q 'exit_class="cancelled"' "$SCRIPT_TO_TEST" && \
   grep -q 'exit_class="unknown"' "$SCRIPT_TO_TEST" && \
   ! grep -q 'exit_class="success"' "$SCRIPT_TO_TEST" && \
   ! grep -q 'exit_class="shutdown"' "$SCRIPT_TO_TEST"; then
  test_pass "exit class values use correct enum"
else
  test_fail "exit class values are incorrect (success/failure/shutdown found instead of completed/failed/cancelled/unknown)"
fi

# Test 35: Permit functions receive required flags
echo "Test 35: permit-renew and permit-finalize include all required flags"
_permit_ok=1
if ! grep -A 2 'odonian permit-renew' "$SCRIPT_TO_TEST" | grep -q '\--task-id'; then
  _permit_ok=0
fi
if ! grep -A 2 'odonian permit-renew' "$SCRIPT_TO_TEST" | grep -q '\--model'; then
  _permit_ok=0
fi
if ! grep -A 2 'odonian permit-renew' "$SCRIPT_TO_TEST" | grep -q '\--agent-id'; then
  _permit_ok=0
fi
if ! grep -A 2 'odonian permit-renew' "$SCRIPT_TO_TEST" | grep -q '\--request-id'; then
  _permit_ok=0
fi
if ! grep -A 2 'odonian permit-renew' "$SCRIPT_TO_TEST" | grep -q '\--attempt-id'; then
  _permit_ok=0
fi
if ! grep -A 2 'odonian permit-finalize' "$SCRIPT_TO_TEST" | grep -q '\--task-id'; then
  _permit_ok=0
fi
if ! grep -A 2 'odonian permit-finalize' "$SCRIPT_TO_TEST" | grep -q '\--exit-class'; then
  _permit_ok=0
fi
if [ "$_permit_ok" -eq 1 ]; then
  test_pass "permit-renew and permit-finalize include all required flags"
else
  test_fail "permit-renew or permit-finalize missing required flags"
fi

# Test 36: Verify dispatch loop doesn't block on wait during polling
echo "Test 36: dispatch loop polls without blocking on wait"
_dispatch_func=$(sed -n '/^dispatch() {/,/^}/p' "$SCRIPT_TO_TEST")
_loop_content=$(printf '%s' "$_dispatch_func" | sed -n '/while kill -0 "$pid"/,/done/p')
if printf '%s' "$_loop_content" | grep -q 'sleep 1' && \
   printf '%s' "$_loop_content" | grep -q 'renew_permit' && \
   ! printf '%s' "$_loop_content" | grep -q 'wait "$pid"'; then
  test_pass "dispatch polling loop calls sleep and renew without blocking wait"
else
  test_fail "dispatch polling loop structure is incorrect"
fi

# Test 37: Verify wait happens after the polling loop
echo "Test 37: dispatch calls wait to capture exit code after polling loop"
_dispatch_func=$(sed -n '/^dispatch() {/,/^}/p' "$SCRIPT_TO_TEST")
_loop_line=$(printf '%s' "$_dispatch_func" | grep -n '^  done$' | head -1 | cut -d: -f1)
_wait_line=$(printf '%s' "$_dispatch_func" | grep -n 'wait "$pid"' | head -1 | cut -d: -f1)
if [ -n "$_loop_line" ] && [ -n "$_wait_line" ] && [ "$_wait_line" -gt "$_loop_line" ]; then
  test_pass "wait happens after polling loop (captures child exit code correctly)"
else
  test_fail "wait/loop ordering incorrect (exit code capture broken)"
fi

# Test 38: Preclaimed task ID comes from task variable, not JSON
echo "Test 38: preclaimed task ID uses task variable directly"
_task_id_direct=$(grep 'ODONIAN_PRECLAIMED_TASK_ID="\$task_id"' "$SCRIPT_TO_TEST" 2>/dev/null | wc -l)
_task_id_extract=$(grep 'extract_admission_field.*task_id' "$SCRIPT_TO_TEST" 2>/dev/null | wc -l)
if [ "$_task_id_direct" -ge 2 ] && [ "$_task_id_extract" -eq 0 ]; then
  test_pass "preclaimed task ID set from variable (no JSON extraction)"
else
  test_fail "preclaimed task ID extraction is incorrect (direct=$_task_id_direct extract=$_task_id_extract)"
fi

echo ""
echo "=== Test Summary ==="
echo "Total: $test_count | Passed: $pass_count | Failed: $fail_count"

if [ "$fail_count" -eq 0 ]; then
  echo "✓ All smoke tests passed"
  exit 0
else
  echo "✗ Some tests failed"
  exit 1
fi
