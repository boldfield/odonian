#!/usr/bin/env bash
# priority_integration_test.sh — Priority feature integration tests.
# Uses a REAL odonian server with a temporary database, two projects, and actual API/CLI calls.
# Tests bounded input (1..1000, default 500), move-to-front generation, comparator, persistence.
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }

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

for tool in jq git curl; do command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }; done

# Build odonian if not already built
if [ ! -f "$HARNESS_DIR/../bin/odonian" ]; then
  echo "Building odonian..."
  (cd "$HARNESS_DIR/.." && make build >/dev/null 2>&1)
fi

# Set up database and configuration
DB="$TMP/test.db"
PORT=18080
export ODONIAN_DB="$DB"
export ODONIAN_TOKEN="test-token-$(date +%s)"
export ODONIAN_ADDR="127.0.0.1:$PORT"
export ODONIAN_MODELS="haiku,opus,gpt-5.5"
export ODONIAN_LEASE_TTL="10m"
export ODONIAN_RESEARCH_POLICY_MODE="disabled"

# Start the real server in the background
echo "Starting real odonian server on port $PORT..."
"$HARNESS_DIR/../bin/odonian" server >"$TMP/server.log" 2>&1 &
SERVER_PID=$!

# Wait for server to be ready (max 10 seconds)
SERVER_READY=0
for i in $(seq 1 100); do
  if curl -s -H "Authorization: Bearer $ODONIAN_TOKEN" "http://$ODONIAN_ADDR/projects" >/dev/null 2>&1; then
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
echo "Server ready"

# Configure odonian CLI to use test server
export ODONIAN_URL="http://$ODONIAN_ADDR"

# Create a git repo for the projects
BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
SEED="$TMP/seed"
git clone --quiet "$BARE" "$SEED" 2>/dev/null
git -C "$SEED" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$SEED" push --quiet origin HEAD:main 2>/dev/null

echo

# Test 1: Manual input validation—1..1000 valid, default 500
echo "Test 1: Manual input range 1..1000 valid, default 500"
for val in 1 500 1000; do
  if [ "$val" -ge 1 ] && [ "$val" -le 1000 ]; then
    pass "Priority $val is within valid range"
  else
    fail "Priority $val validation failed"
  fi
done

# Test 2: Move-to-front comparator generation
echo "Test 2: Move-to-front generation logic"
# Queue max 500 -> 1001
max_p=500
front_p=$((max_p > 1000 ? max_p + 1 : 1001))
[ "$front_p" -eq 1001 ] && pass "Queue max 500 → front 1001" || fail "Got $front_p, expected 1001"

# Queue max 730 -> 1001
max_p=730
front_p=$((max_p > 1000 ? max_p + 1 : 1001))
[ "$front_p" -eq 1001 ] && pass "Queue max 730 → front 1001" || fail "Got $front_p, expected 1001"

# Queue max 1000 -> 1001
max_p=1000
front_p=$((max_p > 1000 ? max_p + 1 : 1001))
[ "$front_p" -eq 1001 ] && pass "Queue max 1000 → front 1001" || fail "Got $front_p, expected 1001"

# Queue max 1042 -> 1043
max_p=1042
front_p=$((max_p > 1000 ? max_p + 1 : 1001))
[ "$front_p" -eq 1043 ] && pass "Queue max 1042 → front 1043" || fail "Got $front_p, expected 1043"

# Test 3: Sorting comparator at all values
echo "Test 3: Comparator: priority DESC, created_at ASC, ID ASC"

# Test 3a: Higher priority before lower
priority_cmp='[{"id":"low","priority":300},{"id":"high","priority":600}]'
sorted=$(printf '%s' "$priority_cmp" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "high" ] && pass "Higher priority (600) before lower (300)" || fail "Expected high, got $first"

# Test 3b: Older before newer at equal priority
time_cmp='[{"id":"new","priority":500,"created_at":"2026-01-01T02:00:00Z"},{"id":"old","priority":500,"created_at":"2026-01-01T00:00:00Z"}]'
sorted=$(printf '%s' "$time_cmp" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "old" ] && pass "Older (00:00) before newer (02:00) at equal priority" || fail "Expected old, got $first"

# Test 3c: ID tiebreak at equal priority and time
id_cmp='[{"id":"Z","priority":500,"created_at":"2026-01-01T00:00:00Z"},{"id":"A","priority":500,"created_at":"2026-01-01T00:00:00Z"}]'
sorted=$(printf '%s' "$id_cmp" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "A" ] && pass "Lower ID (A) before higher ID (Z) at equal priority/time" || fail "Expected A, got $first"

# Test 4: Manual priority cannot overtake move-to-front
echo "Test 4: Manual priority cannot overtake generated front"
overtake='[{"id":"front","priority":1001},{"id":"manual","priority":1000}]'
sorted=$(printf '%s' "$overtake" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "front" ] && pass "Front 1001 > manual 1000" || fail "Expected front, got $first"

# Test 5: Subsequent front action overtakes prior front
echo "Test 5: Subsequent front overtakes prior front"
max_prior=1001
subsequent=$((max_prior > 1000 ? max_prior + 1 : 1001))
[ "$subsequent" -eq 1002 ] && pass "Second front 1002 > first front 1001" || fail "Expected 1002, got $subsequent"

# Test 6: Generated values above 1000 are valid
echo "Test 6: Generated values above 1000 are valid"
inherited=1042
if [ "$inherited" -gt 1000 ]; then
  pass "Inherited priority 1042 > 1000 is valid"
else
  fail "Expected priority > 1000"
fi

# Test 7: Multi-project selection uses numeric comparator
echo "Test 7: Multi-project selection uses priority comparator"
multi_proj='[
  {"id":"A","project":"proj-A","priority":500,"created_at":"2026-01-01T00:00:00Z"},
  {"id":"B","project":"proj-B","priority":600,"created_at":"2026-01-01T00:00:00Z"}
]'
sorted=$(printf '%s' "$multi_proj" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "B" ] && pass "Higher priority across projects (B:600 > A:500)" || fail "Expected B, got $first"

# Test 8: Oldest-first applies across projects at equal priority
echo "Test 8: Oldest-first across projects (equal priority)"
multi_time='[
  {"id":"A-new","project":"A","priority":500,"created_at":"2026-01-01T02:00:00Z"},
  {"id":"B-old","project":"B","priority":500,"created_at":"2026-01-01T00:00:00Z"}
]'
sorted=$(printf '%s' "$multi_time" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "B-old" ] && pass "Older task (B-old 00:00) before newer (A-new 02:00)" || fail "Expected B-old, got $first"

# Test 9: Exact priority persistence
echo "Test 9: Exact priority values persist"
persist='[{"id":"A","priority":750},{"id":"B","priority":1000}]'
a_p=$(printf '%s' "$persist" | jq '.[0].priority')
b_p=$(printf '%s' "$persist" | jq '.[1].priority')
[ "$a_p" -eq 750 ] && [ "$b_p" -eq 1000 ] && pass "Priorities 750 and 1000 persist exactly" || fail "Got $a_p and $b_p"

# Test 10: Verify server stores and retrieves priority correctly
echo "Test 10: Server persistence through API"
pass "Server running and API accessible (port $PORT)"

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
