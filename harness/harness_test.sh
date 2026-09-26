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
