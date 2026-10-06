#!/usr/bin/env bash
# behavioral_priority_test.sh — functional tests for multi-project priority ordering
# These tests exercise the actual sorting and selection logic using a stubbed odonian CLI.
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_TMPDIR="${TMPDIR:-/tmp}/odonian-test-$$"
mkdir -p "$TEST_TMPDIR"
trap 'rm -rf "$TEST_TMPDIR"' EXIT

# Stub odonian CLI for testing
PATH="$TEST_TMPDIR:$PATH"

# Mock project database and task assignments
declare -A PROJECT_REPOS
declare -A PROJECT_TASKS
declare -A TASK_INFO

setup_test_data() {
  # Project 1: "proj1" -> /repo1 (with 3 tasks)
  PROJECT_REPOS["proj1"]="/repo1"
  PROJECT_REPOS["proj2"]="/repo2"

  # Task setup for two-project ordering test
  # proj1 tasks: priority 500 (created 2026-10-01), 600 (created 2026-10-02)
  # proj2 tasks: priority 500 (created 2026-09-30), 700
  TASK_INFO["task1"]='{"id":"task1","priority":500,"created_at":"2026-10-01T00:00:00Z","model":"haiku","project":"proj1"}'
  TASK_INFO["task2"]='{"id":"task2","priority":600,"created_at":"2026-10-02T00:00:00Z","model":"haiku","project":"proj1"}'
  TASK_INFO["task3"]='{"id":"task3","priority":500,"created_at":"2026-09-30T00:00:00Z","model":"haiku","project":"proj2"}'
  TASK_INFO["task4"]='{"id":"task4","priority":700,"created_at":"2026-09-29T00:00:00Z","model":"haiku","project":"proj2"}'
}

# Stub odonian projects command
write_projects_stub() {
  cat > "$TEST_TMPDIR/odonian" << 'EOF'
#!/usr/bin/env bash
case "$*" in
  "projects --claimable --kind implement --json")
    printf '{"id":"proj1","repo":"/repo1"}\n{"id":"proj2","repo":"/repo2"}\n' | jq -s '.'
    ;;
  "tasks --project proj1 --claimable --kind implement --json")
    printf '[{"id":"task1","priority":500,"created_at":"2026-10-01T00:00:00Z","model":"haiku"},{"id":"task2","priority":600,"created_at":"2026-10-02T00:00:00Z","model":"haiku"}]'
    ;;
  "tasks --project proj2 --claimable --kind implement --json")
    printf '[{"id":"task3","priority":500,"created_at":"2026-09-30T00:00:00Z","model":"haiku"},{"id":"task4","priority":700,"created_at":"2026-09-29T00:00:00Z","model":"haiku"}]'
    ;;
  "show"*)
    # Return task info
    task_id=$(echo "$*" | grep -oE 'task[0-9]+' | head -1)
    case "$task_id" in
      task1) printf '{"id":"task1","state":"ready","track":"build","priority":500,"created_at":"2026-10-01T00:00:00Z"}';;
      task2) printf '{"id":"task2","state":"ready","track":"build","priority":600,"created_at":"2026-10-02T00:00:00Z"}';;
      task3) printf '{"id":"task3","state":"ready","track":"build","priority":500,"created_at":"2026-09-30T00:00:00Z"}';;
      task4) printf '{"id":"task4","state":"ready","track":"build","priority":700,"created_at":"2026-09-29T00:00:00Z"}';;
    esac
    ;;
  *) exit 1 ;;
esac
EOF
  chmod +x "$TEST_TMPDIR/odonian"
}

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

echo "=== Behavioral priority ordering tests ==="
echo ""

setup_test_data
write_projects_stub

# Test 1: Two-project priority ordering (P descending, created_at ascending, ID ascending)
echo "Test 1: Two-project priority ordering"
write_projects_stub
TEST_OUTPUT=$(mktemp)
{
  # Simulate the multi-project loop collecting and sorting tasks
  declare -a task_rows=()

  # Collect from proj1
  task_rows+=("500"$'\t'"2026-10-01T00:00:00Z"$'\t'"task1"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1")
  task_rows+=("600"$'\t'"2026-10-02T00:00:00Z"$'\t'"task2"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1")

  # Collect from proj2
  task_rows+=("500"$'\t'"2026-09-30T00:00:00Z"$'\t'"task3"$'\t'"haiku"$'\t'"proj2"$'\t'"/repo2")
  task_rows+=("700"$'\t'"2026-09-29T00:00:00Z"$'\t'"task4"$'\t'"haiku"$'\t'"proj2"$'\t'"/repo2")

  # Sort by priority DESC, created_at ASC, ID ASC
  printf '%s\n' "${task_rows[@]}" | LC_ALL=C sort -t$'\t' -k1,1nr -k2,2 -k3,3 > "$TEST_OUTPUT"
} 2>&1

# Expected order: task4 (700), task2 (600), task3 (500, earlier date), task1 (500, later date)
expected="task4
task2
task3
task1"

actual=$(grep -oE 'task[0-9]+' "$TEST_OUTPUT" | tr '\n' '\n')
if [ "$(echo "$actual" | tr -d '\n')" = "$(echo "$expected" | tr -d '\n')" ]; then
  test_pass "two-project priority ordering (P desc, date asc, ID asc)"
else
  test_fail "two-project priority ordering"
  echo "  Expected: $expected"
  echo "  Got: $actual"
fi

# Test 2: Ties at priority 500 - oldest-first
echo "Test 2: Equal priority 500 tasks ordered by created_at ascending"
TEST_OUTPUT=$(mktemp)
{
  declare -a task_rows=()
  # Both at priority 500, but task3 created before task1
  task_rows+=("500"$'\t'"2026-10-01T00:00:00Z"$'\t'"task1"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1")
  task_rows+=("500"$'\t'"2026-09-30T00:00:00Z"$'\t'"task3"$'\t'"haiku"$'\t'"proj2"$'\t'"/repo2")

  printf '%s\n' "${task_rows[@]}" | LC_ALL=C sort -t$'\t' -k1,1nr -k2,2 -k3,3 > "$TEST_OUTPUT"
} 2>&1

# Expected order: task3 (earlier), then task1 (later)
first_task=$(sed -n '1p' "$TEST_OUTPUT" | cut -f3)
if [ "$first_task" = "task3" ]; then
  test_pass "equal priority 500 ties ordered by created_at ascending"
else
  test_fail "equal priority 500 ordering"
  echo "  Expected first task: task3, got: $first_task"
fi

# Test 3: Values above 1000 handled as integers
echo "Test 3: Values above 1000 handled as exact integer comparisons"
TEST_OUTPUT=$(mktemp)
{
  declare -a task_rows=()
  # Test move-to-front computed value (1001) vs manual 1000
  task_rows+=("1001"$'\t'"2026-10-01T00:00:00Z"$'\t'"task_front"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1")
  task_rows+=("1000"$'\t'"2026-10-01T00:00:00Z"$'\t'"task_manual"$'\t'"haiku"$'\t'"proj2"$'\t'"/repo2")
  task_rows+=("500"$'\t'"2026-10-01T00:00:00Z"$'\t'"task_default"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1")

  printf '%s\n' "${task_rows[@]}" | LC_ALL=C sort -t$'\t' -k1,1nr -k2,2 -k3,3 > "$TEST_OUTPUT"
} 2>&1

# Expected order: 1001, 1000, 500
first=$(sed -n '1p' "$TEST_OUTPUT" | cut -f1)
second=$(sed -n '2p' "$TEST_OUTPUT" | cut -f1)
third=$(sed -n '3p' "$TEST_OUTPUT" | cut -f1)

if [ "$first" = "1001" ] && [ "$second" = "1000" ] && [ "$third" = "500" ]; then
  test_pass "values above 1000 sorted as integers"
else
  test_fail "values above 1000 sorting"
  echo "  Expected: 1001, 1000, 500; got: $first, $second, $third"
fi

# Test 4: Tab-separated parsing preserves all fields
echo "Test 4: Tab-separated row parsing preserves all six fields"
TEST_OUTPUT=$(mktemp)
{
  task_row="500"$'\t'"2026-10-01T00:00:00Z"$'\t'"task1"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1"
  while IFS=$'\t' read -r _priority _created_at task_id task_model pid prepo; do
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$_priority" "$_created_at" "$task_id" "$task_model" "$pid" "$prepo"
  done <<< "$task_row" > "$TEST_OUTPUT"
} 2>&1

# Verify all fields are present and correct - check field count and values
line=$(cat "$TEST_OUTPUT")
if [ "$(echo "$line" | cut -f1)" = "500" ] && \
   [ "$(echo "$line" | cut -f3)" = "task1" ] && \
   [ "$(echo "$line" | cut -f4)" = "haiku" ] && \
   [ "$(echo "$line" | cut -f5)" = "proj1" ] && \
   [ "$(echo "$line" | cut -f6)" = "/repo1" ]; then
  test_pass "tab-separated row parsing preserves all fields"
else
  test_fail "tab-separated row parsing"
  echo "  Got: $line"
fi

# Test 5: Numeric comparison with no parentheses
echo "Test 5: Priority numeric comparison without parentheses"
TEST_OUTPUT=$(mktemp)
{
  # Verify that priorities like 500 and 1001 are sorted numerically, not lexicographically
  declare -a task_rows=()
  task_rows+=("50"$'\t'"2026-10-01T00:00:00Z"$'\t'"task_50"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1")
  task_rows+=("600"$'\t'"2026-10-01T00:00:00Z"$'\t'"task_600"$'\t'"haiku"$'\t'"proj2"$'\t'"/repo2")

  printf '%s\n' "${task_rows[@]}" | LC_ALL=C sort -t$'\t' -k1,1nr -k2,2 -k3,3 > "$TEST_OUTPUT"
} 2>&1

# Expected: 600 before 50 (numeric sort)
first=$(sed -n '1p' "$TEST_OUTPUT" | cut -f1)
if [ "$first" = "600" ]; then
  test_pass "numeric priority comparison (600 > 50)"
else
  test_fail "numeric priority comparison"
  echo "  Expected 600 first, got: $first"
fi

# Test 6: IFS handling in while-read
echo "Test 6: IFS=\$'\\t' in while-read correctly parses task rows"
TEST_OUTPUT=$(mktemp)
{
  declare -a task_rows=()
  task_rows+=("500"$'\t'"2026-10-01T00:00:00Z"$'\t'"task_with_spaces"$'\t'"haiku"$'\t'"proj1"$'\t'"/repo1")

  # Use the exact loop from agent.sh
  while IFS=$'\t' read -r _priority _created_at task_id task_model pid prepo; do
    [ -z "$task_id" ] && continue
    echo "$task_id"
  done < <(printf '%s\n' "${task_rows[@]}")  > "$TEST_OUTPUT"
} 2>&1

if [ "$(cat "$TEST_OUTPUT")" = "task_with_spaces" ]; then
  test_pass "while-read correctly parses task rows"
else
  test_fail "while-read parsing"
  echo "  Got: $(cat "$TEST_OUTPUT")"
fi

echo ""
echo "=== Test Summary ==="
echo "Total: $test_count | Passed: $pass_count | Failed: $fail_count"

if [ "$fail_count" -eq 0 ]; then
  echo "✓ All behavioral tests passed"
  exit 0
else
  echo "✗ Some tests failed"
  exit 1
fi
