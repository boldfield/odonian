#!/usr/bin/env bash
# multi_priority_test.sh — tests for multi-project priority ordering and comparator
# Tests the uniform comparator: P descending, created_at ascending, ID ascending
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPT_TO_TEST="$HARNESS_DIR/agent.sh"

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

echo "=== Multi-project priority ordering tests ==="
echo ""

# Test 1: Check that multi-project mode uses tab-separated rows for task collection
echo "Test 1: multi-project collects tasks as tab-separated rows"
if grep -q 'task_rows+=(' "$SCRIPT_TO_TEST" && \
   grep -q 'created_at.*task_id' "$SCRIPT_TO_TEST"; then
  test_pass "multi-project creates tab-separated task rows"
else
  test_fail "multi-project task row creation broken"
fi

# Test 2: Priority field not wrapped in parentheses in jq
echo "Test 2: priority field emitted without parentheses"
if grep -q 'jq.*\.priority.*// 500' "$SCRIPT_TO_TEST"; then
  test_pass "priority field is not wrapped in parentheses"
else
  test_fail "priority field has unwanted parentheses"
fi

# Test 3: Sort uses proper key ranges (-k2,2 -k3,3)
echo "Test 3: sort uses proper field key ranges"
if grep -q 'sort.*-k2,2' "$SCRIPT_TO_TEST" && \
   grep -q 'sort.*-k3,3' "$SCRIPT_TO_TEST"; then
  test_pass "sort uses -k2,2 and -k3,3 for field ranges"
else
  test_fail "sort uses incomplete key ranges"
fi

# Test 4: Multi-project uses while-read loop for parsing sorted tasks
echo "Test 4: multi-project uses while-read for task dispatch"
if grep -q 'while IFS.*read.*_priority.*_created_at.*task_id' "$SCRIPT_TO_TEST"; then
  test_pass "multi-project uses while-read loop for sorted task dispatch"
else
  test_fail "multi-project task dispatch structure incorrect"
fi

# Test 5: Sort order is numeric descending for priority
echo "Test 5: sort uses numeric descending for priority"
if grep -q 'sort.*-k1nr' "$SCRIPT_TO_TEST"; then
  test_pass "sort uses -k1nr for numeric descending priority"
else
  test_fail "sort does not use numeric descending for priority"
fi

# Test 6: Merger path does not use random project ordering
echo "Test 6: merger path uses deterministic project ordering"
_merger_section="$(sed -n '/MULTI-PROJECT MERGE MODE/,/^  fi$/p' "$SCRIPT_TO_TEST")"
if printf '%s' "$_merger_section" | grep -q 'jq.*\.id' && \
   ! printf '%s' "$_merger_section" | grep -q 'sort -R'; then
  test_pass "merger path does not randomize project ordering"
else
  test_fail "merger path still uses sort -R for projects"
fi

# Test 7: Both single and multi-project pass selected task for non-research
echo "Test 7: single and multi-project pass task for non-research dispatch"
_count=$(grep -c 'For non-research tasks, pass the selected task' "$SCRIPT_TO_TEST")
if [ "$_count" -eq 2 ] && grep -A 1 'For non-research tasks, pass the selected task' "$SCRIPT_TO_TEST" | grep -q 'P_TASK='; then
  test_pass "single and multi-project pass selected task for non-research dispatch"
else
  test_fail "single/multi-project does not pass task to non-research dispatch"
fi

# Test 9: Check all six fields of task row in sort input
echo "Test 9: task row includes all six fields"
_row_check=$(grep 'priority.*created_at.*task_id.*task_model.*pid.*prepo' "$SCRIPT_TO_TEST")
if [ -n "$_row_check" ]; then
  test_pass "task row includes all six fields"
else
  test_fail "task row missing fields"
fi

# Test 10: Parse statement shows correct field order
echo "Test 10: parse statement reads correct field order"
if grep -q 'IFS.*read.*_priority.*_created_at.*task_id.*task_model.*pid.*prepo' "$SCRIPT_TO_TEST"; then
  test_pass "parse statement reads all six fields in correct order"
else
  test_fail "parse statement has incorrect field order"
fi

echo ""
echo "=== Test Summary ==="
echo "Total: $test_count | Passed: $pass_count | Failed: $fail_count"

if [ "$fail_count" -eq 0 ]; then
  echo "✓ All multi-project priority tests passed"
  exit 0
else
  echo "✗ Some tests failed"
  exit 1
fi
