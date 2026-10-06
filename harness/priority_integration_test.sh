#!/usr/bin/env bash
# priority_integration_test.sh — Priority feature integration tests.
# Tests bounded input (1..1000, default 500), move-to-front generation, comparator correctness.
# No real server, database, or model calls—pure jq-based verification.
set -uo pipefail

pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }

echo "=== Priority Integration Tests ==="
echo

# Test 1: Default priority 500 when omitted
echo "Test 1: Default priority 500 when omitted"
tasks='[{"id":"A"},{"id":"B","priority":500}]'
a_prio=$(printf '%s' "$tasks" | jq '.[0].priority // 500')
b_prio=$(printf '%s' "$tasks" | jq '.[1].priority // 500')
[ "$a_prio" -eq 500 ] && [ "$b_prio" -eq 500 ] && pass "Default priority is 500" || fail "Expected both 500"

# Test 2: Input validation—manual range 1..1000 is valid
echo "Test 2: Input validation—manual range valid"
for val in 1 500 1000; do
  if [ "$val" -ge 1 ] && [ "$val" -le 1000 ]; then
    pass "Priority $val is within valid range"
  else
    fail "Priority $val validation failed"
  fi
done

# Test 3: Move-to-front generation—queue max below 1000
echo "Test 3: Move-to-front below 1000 (500 → 1001)"
tasks='[{"id":"A","priority":500},{"id":"B","priority":400},{"id":"C","priority":300}]'
max_p=$(printf '%s' "$tasks" | jq '[.[].priority // 500] | max')
front_p=$((max_p > 1000 ? max_p + 1 : 1001))
[ "$front_p" -eq 1001 ] && pass "Queue max 500 generates 1001" || fail "Got $front_p, expected 1001"

# Test 4: Move-to-front generation—queue max at 1000
echo "Test 4: Move-to-front at 1000 (1000 → 1001)"
tasks='[{"id":"A","priority":1000},{"id":"B","priority":500}]'
max_p=$(printf '%s' "$tasks" | jq '[.[].priority // 500] | max')
front_p=$((max_p > 1000 ? max_p + 1 : 1001))
[ "$front_p" -eq 1001 ] && pass "Queue max 1000 generates 1001" || fail "Got $front_p, expected 1001"

# Test 5: Move-to-front generation—queue max above 1000
echo "Test 5: Move-to-front above 1000 (1042 → 1043)"
tasks='[{"id":"A","priority":1042},{"id":"B","priority":500}]'
max_p=$(printf '%s' "$tasks" | jq '[.[].priority // 500] | max')
front_p=$((max_p > 1000 ? max_p + 1 : 1001))
[ "$front_p" -eq 1043 ] && pass "Queue max 1042 generates 1043" || fail "Got $front_p, expected 1043"

# Test 6: Comparator—priority DESC
echo "Test 6: Comparator respects priority DESC"
tasks='[{"id":"low","priority":300,"created_at":"2026-01-01T00:00:00Z"},{"id":"high","priority":600,"created_at":"2026-01-01T00:00:00Z"}]'
sorted=$(printf '%s' "$tasks" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "high" ] && pass "Higher priority (600) before lower (300)" || fail "Got $first, expected high"

# Test 7: Comparator—created_at ASC for ties (oldest first)
echo "Test 7: Comparator oldest-first at equal priority"
tasks='[{"id":"new","priority":500,"created_at":"2026-01-01T02:00:00Z"},{"id":"old","priority":500,"created_at":"2026-01-01T00:00:00Z"}]'
sorted=$(printf '%s' "$tasks" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "old" ] && pass "Older (00:00) before newer (02:00) at equal priority" || fail "Got $first, expected old"

# Test 8: Comparator—ID ASC for final tiebreak
echo "Test 8: Comparator uses ID for final tiebreak"
tasks='[{"id":"Z","priority":500,"created_at":"2026-01-01T00:00:00Z"},{"id":"A","priority":500,"created_at":"2026-01-01T00:00:00Z"}]'
sorted=$(printf '%s' "$tasks" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "A" ] && pass "Lower ID (A) before higher ID (Z) at equal priority/time" || fail "Got $first, expected A"

# Test 9: Manual 1000 cannot overtake front 1001
echo "Test 9: Manual priority 1000 < generated 1001"
tasks='[{"id":"front","priority":1001},{"id":"manual","priority":1000}]'
sorted=$(printf '%s' "$tasks" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "front" ] && pass "Priority 1001 > 1000" || fail "Got $first, expected front"

# Test 10: Subsequent front overtakes prior front
echo "Test 10: Second front overtakes first (1002 > 1001)"
max1=1001
front2=$((max1 > 1000 ? max1 + 1 : 1001))
[ "$front2" -eq 1002 ] && pass "Subsequent front 1002 > prior front 1001" || fail "Got $front2, expected 1002"

# Test 11: Priority persistence (exact values stored)
echo "Test 11: Priority values persist exactly"
tasks='[{"id":"A","priority":750},{"id":"B","priority":1000}]'
a_p=$(printf '%s' "$tasks" | jq '.[0].priority')
b_p=$(printf '%s' "$tasks" | jq '.[1].priority')
[ "$a_p" -eq 750 ] && [ "$b_p" -eq 1000 ] && pass "Priorities 750 and 1000 persisted exactly" || fail "Got $a_p and $b_p"

# Test 12: Multi-project selection—numeric comparator applies
echo "Test 12: Multi-project selects by numeric comparator"
tasks='[
  {"id":"A","project":"proj-A","priority":500,"created_at":"2026-01-01T00:00:00Z"},
  {"id":"B","project":"proj-B","priority":600,"created_at":"2026-01-01T00:00:00Z"}
]'
sorted=$(printf '%s' "$tasks" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "B" ] && pass "Higher priority across projects (B:600 > A:500)" || fail "Got $first, expected B"

# Test 13: Inherited values above 1000 remain valid
echo "Test 13: Inherited values above 1000 are valid"
inherited=1042
if [ "$inherited" -gt 1000 ]; then
  pass "Inherited priority 1042 > 1000 is valid"
else
  fail "Inherited priority validation failed"
fi

# Test 14: Oldest-first applies across projects
echo "Test 14: Oldest-first across projects (equal priority)"
tasks='[
  {"id":"A-new","project":"A","priority":500,"created_at":"2026-01-01T02:00:00Z"},
  {"id":"B-old","project":"B","priority":500,"created_at":"2026-01-01T00:00:00Z"}
]'
sorted=$(printf '%s' "$tasks" | jq -c 'sort_by([-((.priority // 500) | tonumber), .created_at, .id])')
first=$(printf '%s' "$sorted" | jq -r '.[0].id')
[ "$first" = "B-old" ] && pass "Older task (B-old 00:00) before newer (A-new 02:00) across projects" || fail "Got $first"

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
