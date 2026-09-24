#!/usr/bin/env bash
# model_failover_test.sh — verifies agent.sh skips a model whose backend is failing instead of
# re-selecting its head task on every pass (see pick_claimable_task/mark_model_unavailable in
# agent.sh). Runs the real SINGLE-project loop against a stubbed `odonian` CLI and a stubbed
# `claude` dispatch: two claimable review tasks are offered, the first pinned to a model whose
# dispatch always fails, the second to a model whose dispatch always succeeds. Asserts the second
# task is dispatched on the very next pass, without waiting out the first model's backoff window.
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
LOG="$TMP/dispatch.log"
FAIL_MODEL="opus"
OK_MODEL="sonnet"

pass_count=0; fail_count=0
pass() { ((pass_count++)); echo "✓ $1"; }
fail() { ((fail_count++)); echo "✗ $1"; }
cleanup() {
  [ -n "${AGENT_PID:-}" ] && kill -TERM "$AGENT_PID" 2>/dev/null
  [ -n "${AGENT_PID:-}" ] && { sleep 1; kill -KILL "$AGENT_PID" 2>/dev/null; }
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "=== model_failover_test: skip an unavailable model's tasks instead of re-selecting them ==="

# --- a tiny real local repo stands in for the project's repo (real git, no need to stub it) ---
BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
WORK="$TMP/seed"
git clone --quiet "$BARE" "$WORK"
git -C "$WORK" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$WORK" push --quiet origin HEAD:main
rm -rf "$WORK"
MAIN_REPO="$TMP/main-repo"
git clone --quiet "$BARE" "$MAIN_REPO"

# --- stub bin dir, first on PATH ---
BIN="$TMP/bin"; mkdir -p "$BIN"

cat > "$BIN/odonian" <<EOF
#!/usr/bin/env bash
case "\$1" in
  next)
    # has_claimable_work only checks exit status; always report claimable work exists.
    echo "task-a"
    exit 0
    ;;
  tasks)
    cat <<JSON
[
  {"id":"task-a","model":"$FAIL_MODEL","kind":"review","state":"ready","track":"build"},
  {"id":"task-b","model":"$OK_MODEL","kind":"review","state":"ready","track":"build"}
]
JSON
    exit 0
    ;;
  show)
    echo '{"track":"build"}'
    exit 0
    ;;
  project)
    echo '{"repo":"$BARE"}'
    exit 0
    ;;
  transition)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
EOF
chmod +x "$BIN/odonian"

cat > "$BIN/claude" <<EOF
#!/usr/bin/env bash
model=""
while [ \$# -gt 0 ]; do
  [ "\$1" = "--model" ] && model="\$2"
  shift
done
echo "\$(date +%s.%N) \$model" >> "$LOG"
[ "\$model" = "$FAIL_MODEL" ] && exit 1
exit 0
EOF
chmod +x "$BIN/claude"

export PATH="$BIN:$PATH"
export ODONIAN_HOME="$TMP/odonian-home"
export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN="test-token"   # unused (odonian is stubbed); satisfies any env checks
export ODONIAN_PROJECT="proj-test"
export ODONIAN_MAIN_REPO="$MAIN_REPO"
export ODONIAN_REPO="$MAIN_REPO"

: > "$LOG"
"$HARNESS_DIR/agent.sh" --model x --kind review test-failover-slot > "$TMP/agent.log" 2>&1 &
AGENT_PID=$!

# Wait for two dispatch attempts (or time out).
for _ in $(seq 1 150); do
  [ "$(wc -l < "$LOG" 2>/dev/null || echo 0)" -ge 2 ] && break
  sleep 0.1
done

kill -TERM "$AGENT_PID" 2>/dev/null
wait "$AGENT_PID" 2>/dev/null
AGENT_PID=""

LINE_COUNT="$(wc -l < "$LOG" 2>/dev/null || echo 0)"
echo "Test 1: two dispatch attempts observed"
if [ "$LINE_COUNT" -ge 2 ]; then
  pass "saw $LINE_COUNT dispatch attempt(s)"
else
  fail "expected 2 dispatch attempts, saw $LINE_COUNT — agent.log:"
  sed 's/^/    /' "$TMP/agent.log" 2>/dev/null
fi

if [ "$LINE_COUNT" -ge 2 ]; then
  FIRST_MODEL="$(sed -n '1p' "$LOG" | awk '{print $2}')"
  SECOND_MODEL="$(sed -n '2p' "$LOG" | awk '{print $2}')"
  FIRST_TS="$(sed -n '1p' "$LOG" | awk '{print $1}')"
  SECOND_TS="$(sed -n '2p' "$LOG" | awk '{print $1}')"

  echo "Test 2: the failing model is tried first"
  if [ "$FIRST_MODEL" = "$FAIL_MODEL" ]; then
    pass "first dispatch was $FAIL_MODEL"
  else
    fail "expected first dispatch to be $FAIL_MODEL, got $FIRST_MODEL"
  fi

  echo "Test 3: the next pass dispatches the healthy model's task, not the failing one again"
  if [ "$SECOND_MODEL" = "$OK_MODEL" ]; then
    pass "second dispatch was $OK_MODEL"
  else
    fail "expected second dispatch to be $OK_MODEL, got $SECOND_MODEL"
  fi

  echo "Test 4: the second dispatch did not wait out the failing model's backoff window"
  ELAPSED="$(awk -v a="$FIRST_TS" -v b="$SECOND_TS" 'BEGIN { print (b - a) }')"
  if awk -v e="$ELAPSED" 'BEGIN { exit !(e < 15) }'; then
    pass "second dispatch followed the first after ${ELAPSED}s (< 15s, no backoff wait)"
  else
    fail "second dispatch took ${ELAPSED}s — looks like it waited out a backoff instead of skipping"
  fi
else
  fail "the failing model was tried first (skipped — not enough dispatch attempts)"
  fail "the healthy model's task was dispatched next (skipped — not enough dispatch attempts)"
  fail "no backoff wait before the second dispatch (skipped — not enough dispatch attempts)"
fi

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
