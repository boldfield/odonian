#!/usr/bin/env bash
# seed_demo_test.sh — functional test for harness/seed-demo.sh against a real, throwaway server.
# Builds the binary into a temp dir, starts `odonian server` on a free port with a fresh SQLite
# file, creates a project, runs seed-demo.sh twice (idempotency) and once with --again, and
# checks the board state after each run. Needs go, jq, curl. Run: bash harness/seed_demo_test.sh
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -P "$HARNESS_DIR/.." && pwd)"
TMP="$(mktemp -d)"
fail_count=0; pass_count=0
pass() { ((pass_count++)); echo "✓ $1"; }
fail() { ((fail_count++)); echo "✗ $1"; }
cleanup() { [ -n "${SERVER_PID:-}" ] && kill "$SERVER_PID" 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT

( cd "$REPO_ROOT" && go build -o "$TMP/odonian" ./cmd/odonian ) || { echo "build failed"; exit 1; }

# Pick a free port: bind 0 through a tiny helper is overkill; try a few high ports.
PORT=""
for candidate in 18811 18812 18813 18814 18815; do
  if ! (echo >"/dev/tcp/127.0.0.1/$candidate") 2>/dev/null; then PORT="$candidate"; break; fi
done
[ -n "$PORT" ] || { echo "no free port"; exit 1; }

export ODONIAN_URL="http://127.0.0.1:$PORT" ODONIAN_TOKEN="seed-test-token"
ODONIAN_DB="$TMP/test.db" ODONIAN_ADDR=":$PORT" ODONIAN_TOKEN="$ODONIAN_TOKEN" FORGE_TOKENS=/dev/null \
  "$TMP/odonian" server >"$TMP/server.log" 2>&1 &
SERVER_PID=$!
for _ in $(seq 1 50); do curl -fsS "$ODONIAN_URL/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
curl -fsS "$ODONIAN_URL/healthz" >/dev/null || { echo "server did not start"; cat "$TMP/server.log"; exit 1; }

AUTH=(-H "Authorization: Bearer $ODONIAN_TOKEN" -H "Content-Type: application/json")
PID="$(curl -fsS "${AUTH[@]}" -X POST "$ODONIAN_URL/projects" -d '{"name":"seed-test","repo":"/tmp/seed-test"}' | jq -r .id)"
tasks() { curl -fsS "${AUTH[@]}" "$ODONIAN_URL/projects/$PID/tasks"; }
docs()  { curl -fsS "${AUTH[@]}" "$ODONIAN_URL/projects/$PID/documents"; }

echo "Test 1: first run creates one document and one ready haiku task reviewed by opus, human-gated"
T1="$(bash "$HARNESS_DIR/seed-demo.sh" --project "$PID" 2>/dev/null)"
if [ "$(docs | jq 'length')" = "1" ]; then pass "one document"; else fail "expected 1 document, got $(docs | jq 'length')"; fi
if [ "$(tasks | jq 'length')" = "1" ]; then pass "one task"; else fail "expected 1 task, got $(tasks | jq 'length')"; fi
T1_JSON="$(tasks | jq -c --arg id "$T1" '.[] | select(.id == $id)')"
if [ "$(printf '%s' "$T1_JSON" | jq -r '.state')" = "ready" ]; then pass "task is ready"; else fail "task not ready: $T1_JSON"; fi
if [ "$(printf '%s' "$T1_JSON" | jq -r '.model')" = "haiku" ]; then pass "model is haiku"; else fail "model wrong: $T1_JSON"; fi
if [ "$(printf '%s' "$T1_JSON" | jq -c '.review_models')" = '["opus"]' ]; then pass "reviewer is opus"; else fail "reviewers wrong: $T1_JSON"; fi
if [ "$(printf '%s' "$T1_JSON" | jq -r '.agent_merge')" = "false" ]; then pass "agent_merge is false (human-gated)"; else fail "agent_merge not false: $T1_JSON"; fi

echo "Test 2: second run is a no-op and returns the same task id"
T2="$(bash "$HARNESS_DIR/seed-demo.sh" --project "$PID" 2>/dev/null)"
if [ "$T2" = "$T1" ]; then pass "same task id"; else fail "expected $T1, got $T2"; fi
if [ "$(tasks | jq 'length')" = "1" ] && [ "$(docs | jq 'length')" = "1" ]; then pass "still one task, one document"; else fail "duplicates created"; fi

echo "Test 3: --again posts a new, distinctly titled task, also ready, and reuses the document"
T3="$(bash "$HARNESS_DIR/seed-demo.sh" --project "$PID" --again 2>/dev/null)"
if [ -n "$T3" ] && [ "$T3" != "$T1" ]; then pass "new task id"; else fail "expected a new id, got '$T3'"; fi
if [ "$(tasks | jq 'length')" = "2" ] && [ "$(docs | jq 'length')" = "1" ]; then pass "two tasks, one document"; else fail "unexpected counts: tasks=$(tasks | jq 'length') docs=$(docs | jq 'length')"; fi
if [ "$(tasks | jq -r --arg id "$T3" '.[] | select(.id == $id) | .state')" = "ready" ]; then pass "fresh copy is ready"; else fail "fresh copy not ready"; fi
T3_TITLE="$(tasks | jq -r --arg id "$T3" '.[] | select(.id == $id) | .title')"
if [ "$T3_TITLE" = "Append greeting line #2 to GREETINGS.md" ]; then pass "distinct title: $T3_TITLE"; else fail "unexpected title: $T3_TITLE"; fi

echo; echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
