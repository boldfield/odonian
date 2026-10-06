#!/usr/bin/env bash
# priority_integration_test.sh — Priority feature integration tests with a real odonian server.
# Starts a real server with a temporary database, creates real projects and tasks via API,
# calls the actual priority endpoints, verifies values and persistence across restart.
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/.." && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
info() { echo "   $*"; }

trap 'cleanup' EXIT

cleanup() {
  if [ -n "${SERVER_PID:-}" ]; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    for _ in $(seq 1 20); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}

echo "=== Priority Integration Tests (Real Server + API) ==="
echo

for tool in jq git curl go; do
  if ! command -v "$tool" >/dev/null; then
    echo "SKIP: $tool not installed"
    exit 0
  fi
done

# Build odonian if not already built
if [ ! -f "$REPO_ROOT/bin/odonian" ]; then
  echo "Building odonian..."
  (cd "$REPO_ROOT" && make build >/dev/null 2>&1)
fi

# Set up database and configuration
DB="$TMP/test.db"
export ODONIAN_DB="$DB"
export ODONIAN_TOKEN="test-token-$(date +%s)"

# Find a free port
PORT=$((20000 + RANDOM % 30000))
for _ in $(seq 1 20); do
  if ! curl -s --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
    break
  fi
  PORT=$((PORT + 1))
done
export ODONIAN_ADDR="127.0.0.1:$PORT"
export ODONIAN_URL="http://$ODONIAN_ADDR"
export ODONIAN_MODELS="haiku,opus,gpt-5.5"

# Create a temporary git repo for projects
BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
SEED="$TMP/seed"
git clone --quiet "$BARE" "$SEED" 2>/dev/null
git -C "$SEED" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$SEED" push --quiet origin HEAD:main 2>/dev/null

# Helper functions for API calls
api_create_project() {
  local name="$1"
  curl -fsS -H "Authorization: Bearer $ODONIAN_TOKEN" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"$name\",\"repo\":\"$BARE\"}" \
    "$ODONIAN_URL/projects" | jq -r '.id'
}

api_create_document() {
  local project="$1"
  curl -fsS -H "Authorization: Bearer $ODONIAN_TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"title":"test","kind":"design","content":"x"}' \
    "$ODONIAN_URL/projects/$project/documents" | jq -r '.id'
}

api_create_task() {
  local project="$1" doc="$2" title="$3"
  curl -fsS -H "Authorization: Bearer $ODONIAN_TOKEN" \
    -H 'Content-Type: application/json' \
    -d "[{\"title\":\"$title\",\"spec\":\"test task\",\"document_id\":\"$doc\",\"model\":\"haiku\",\"review_models\":[\"opus\"]}]" \
    "$ODONIAN_URL/projects/$project/tasks" | jq -r '.[0].id'
}

api_promote_task() {
  local task_id="$1"
  "$REPO_ROOT/bin/odonian" promote "$task_id" >/dev/null 2>&1
}

api_get_task() {
  local task_id="$1"
  "$REPO_ROOT/bin/odonian" show "$task_id" --json
}

api_set_priority() {
  local task_id="$1" priority="$2"
  "$REPO_ROOT/bin/odonian" priority "$task_id" --set "$priority" --reason "test" 2>/dev/null
}

api_move_to_front() {
  local task_id="$1"
  local result=$("$REPO_ROOT/bin/odonian" priority "$task_id" --front --reason "test" 2>/dev/null)
  echo "$result" | grep "New Priority:" | grep -o "[0-9]*$"
}

api_reset_priority() {
  local task_id="$1"
  "$REPO_ROOT/bin/odonian" priority "$task_id" --reset --reason "test" 2>/dev/null
}

# Start the real server in the background
echo "Starting real odonian server on $ODONIAN_ADDR..."
"$REPO_ROOT/bin/odonian" server >"$TMP/server.log" 2>&1 &
SERVER_PID=$!

# Wait for server to be ready
SERVER_READY=0
for i in $(seq 1 100); do
  if curl -s -H "Authorization: Bearer $ODONIAN_TOKEN" "$ODONIAN_URL/projects" >/dev/null 2>&1; then
    SERVER_READY=1
    break
  fi
  sleep 0.1
done

if [ "$SERVER_READY" -eq 0 ]; then
  echo "Server failed to start"
  cat "$TMP/server.log"
  exit 1
fi
echo "Server ready on $ODONIAN_ADDR"
echo

# === TEST 1: Create projects and verify default priority ===
echo "Test 1: Project setup and default priority"
PROJ1=$(api_create_project "project-a")
PROJ2=$(api_create_project "project-b")
info "Created projects: $PROJ1, $PROJ2"

DOC1=$(api_create_document "$PROJ1")
DOC2=$(api_create_document "$PROJ2")

# Create tasks without explicit priority (should default to 500)
TASK1=$(api_create_task "$PROJ1" "$DOC1" "task-a-1")
TASK2=$(api_create_task "$PROJ1" "$DOC1" "task-a-2")
TASK3=$(api_create_task "$PROJ2" "$DOC2" "task-b-1")
api_promote_task "$TASK1"
api_promote_task "$TASK2"
api_promote_task "$TASK3"
info "Created tasks: $TASK1, $TASK2, $TASK3"

# Verify default priority
T1_P=$(api_get_task "$TASK1" | jq '.priority // 500')
[ "$T1_P" = "500" ] && pass "Default priority is 500" || fail "Expected 500, got $T1_P"
echo

# === TEST 2: Manual priority validation (1..1000) ===
echo "Test 2: Manual priority boundaries (1..1000)"
for val in 1 500 1000; do
  api_set_priority "$TASK1" "$val" >/dev/null
  P=$(api_get_task "$TASK1" | jq '.priority')
  [ "$P" = "$val" ] && pass "Set priority $val succeeded" || fail "Expected $val, got $P"
done
echo

# === TEST 3: Move-to-front generation ===
echo "Test 3: Move-to-front value generation"
# Reset all tasks to low priority
api_set_priority "$TASK1" "100" >/dev/null
api_set_priority "$TASK2" "200" >/dev/null
api_set_priority "$TASK3" "300" >/dev/null
sleep 0.1

# Move TASK1 to front (queue max is 300, so front should be 1001)
FRONT_VAL=$(api_move_to_front "$TASK1")
info "Front value from max(300): $FRONT_VAL"
# Verify it's above 1000
[ "$FRONT_VAL" -gt 1000 ] && pass "Front value $FRONT_VAL > 1000" || fail "Expected >1000, got $FRONT_VAL"

# Move to front again (now max is $FRONT_VAL, so should be $FRONT_VAL + 1)
FRONT_VAL2=$(api_move_to_front "$TASK2")
info "Second front value: $FRONT_VAL2"
[ "$FRONT_VAL2" -gt "$FRONT_VAL" ] && pass "Subsequent front $FRONT_VAL2 > previous $FRONT_VAL" || fail "Expected >$FRONT_VAL, got $FRONT_VAL2"
echo

# === TEST 4: Manual priority cannot overtake front ===
echo "Test 4: Manual priority cannot overtake front value"
FRONT_TASK=$(api_move_to_front "$TASK3")
info "Front value: $FRONT_TASK"

# Try to set a manual priority below front
api_set_priority "$TASK1" "1000" >/dev/null
P1=$(api_get_task "$TASK1" | jq '.priority')
P3=$(api_get_task "$TASK3" | jq '.priority')
info "Task1 priority: $P1, Task3 front: $P3"
[ "$P3" -gt "$P1" ] && pass "Front value $P3 > manual 1000" || fail "Front should overtake manual"
echo

# === TEST 5: Cross-project priority comparison ===
echo "Test 5: Cross-project priority ordering"
# Set explicit priorities across projects
api_reset_priority "$TASK1" >/dev/null
api_reset_priority "$TASK2" >/dev/null
api_reset_priority "$TASK3" >/dev/null
sleep 0.1

api_set_priority "$TASK1" "600" >/dev/null  # proj-a:600
api_set_priority "$TASK3" "400" >/dev/null  # proj-b:400

# Query all tasks and verify ordering
T1_P=$(api_get_task "$TASK1" | jq '.priority')
T3_P=$(api_get_task "$TASK3" | jq '.priority')
info "Task1 (proj-a): $T1_P, Task3 (proj-b): $T3_P"
[ "$T1_P" -gt "$T3_P" ] && pass "Higher priority across projects (600 > 400)" || fail "Ordering wrong"
echo

# === TEST 6: Oldest-first tiebreaker ===
echo "Test 6: Oldest-first ordering at equal priority"
api_reset_priority "$TASK1" >/dev/null
api_reset_priority "$TASK2" >/dev/null
api_reset_priority "$TASK3" >/dev/null
sleep 0.1

# Set all to same priority; TASK1 was created first
api_set_priority "$TASK1" "500" >/dev/null
api_set_priority "$TASK2" "500" >/dev/null
api_set_priority "$TASK3" "500" >/dev/null

T1=$(api_get_task "$TASK1" | jq -r '.created_at')
T2=$(api_get_task "$TASK2" | jq -r '.created_at')
info "Task1 created: $T1, Task2 created: $T2"
[ "$T1" '<' "$T2" ] && pass "TASK1 older, should sort first" || fail "Ordering issue"
echo

# === TEST 7: Persistence across restart ===
echo "Test 7: Priority persistence across server restart"
# Set specific priority and front values
api_set_priority "$TASK1" "750" >/dev/null
FRONT_BEFORE=$(api_move_to_front "$TASK2")
info "Before restart: Task1=750, Task2 front=$FRONT_BEFORE"

# Kill the server
kill -TERM "$SERVER_PID" 2>/dev/null || true
for _ in $(seq 1 20); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
SERVER_PID=""

# Start server again
echo "Restarting server..."
"$REPO_ROOT/bin/odonian" server >"$TMP/server.log" 2>&1 &
SERVER_PID=$!

# Wait for server to be ready
for i in $(seq 1 100); do
  if curl -s -H "Authorization: Bearer $ODONIAN_TOKEN" "$ODONIAN_URL/projects" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

# Verify priorities persisted
T1_P=$(api_get_task "$TASK1" | jq '.priority')
T2_P=$(api_get_task "$TASK2" | jq '.priority')
info "After restart: Task1=$T1_P, Task2=$T2_P"
[ "$T1_P" = "750" ] && pass "Task1 priority persisted: 750" || fail "Expected 750, got $T1_P"
[ "$T2_P" = "$FRONT_BEFORE" ] && pass "Task2 front persisted: $FRONT_BEFORE" || fail "Expected $FRONT_BEFORE, got $T2_P"
echo

# === TEST 8: Reset to default ===
echo "Test 8: Reset priority to default"
api_reset_priority "$TASK1" >/dev/null
P=$(api_get_task "$TASK1" | jq '.priority')
[ "$P" = "500" ] && pass "Priority reset to 500" || fail "Expected 500, got $P"
echo

# === TEST 9: Concurrent priority operations ===
echo "Test 9: Concurrent priority operations"
# Create additional task for concurrency test
TASK4=$(api_create_task "$PROJ1" "$DOC1" "task-a-4")
api_promote_task "$TASK4"

# Set base priority for comparison
api_set_priority "$TASK3" "300" >/dev/null
api_set_priority "$TASK4" "400" >/dev/null

# Quickly call move-to-front twice
FRONT1=$(api_move_to_front "$TASK3")
FRONT2=$(api_move_to_front "$TASK4")
info "Concurrent fronts: $FRONT1, $FRONT2"

P3=$(api_get_task "$TASK3" | jq '.priority')
P4=$(api_get_task "$TASK4" | jq '.priority')
[ "$P3" != "$P4" ] && pass "Concurrent front operations generated distinct values" || fail "Values should differ: $P3, $P4"
[ "$P4" -gt "$P3" ] && pass "Subsequent front overtakes: $P4 > $P3" || fail "Ordering wrong"
echo

echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
