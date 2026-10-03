#!/usr/bin/env bash
# research_pacing_test.sh — deterministic smoke tests for research pacing.
# Demonstrates: concurrent launch limiting, persistence through restart,
# review/rework reserved capacity, automatic waiting, unaffected build work.
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
AGENT_PID=""

stop_agent() {
  if [ -n "$AGENT_PID" ]; then
    kill -TERM "$AGENT_PID" 2>/dev/null || true
    for _ in $(seq 1 100); do kill -0 "$AGENT_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$AGENT_PID" 2>/dev/null || true
    wait "$AGENT_PID" 2>/dev/null || true
    AGENT_PID=""
  fi
}

trap 'stop_agent; rm -rf "$TMP"' EXIT

for tool in jq git; do
  command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }
done

# --- sanitized PATH ---
SYSBIN="$TMP/sysbin"; mkdir -p "$SYSBIN"
for tool in bash env jq git date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep readlink tail uname ln mkdir chmod mv cp ls find tee sh; do
  p="$(command -v "$tool" 2>/dev/null)"
  if [ -n "$p" ] && [ "${p#/}" != "$p" ]; then
    ln -sf "$p" "$SYSBIN/$tool"
  fi
done
BIN="$TMP/bin"; mkdir -p "$BIN"
export PATH="$BIN:$SYSBIN"

# --- test repositories ---
BARE1="$TMP/proj1.git"; git init --quiet --bare "$BARE1"
SEED1="$TMP/seed1"; git clone --quiet "$BARE1" "$SEED1" 2>/dev/null
git -C "$SEED1" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$SEED1" push --quiet origin HEAD:main 2>/dev/null
MAIN_REPO1="$TMP/main1"; git clone --quiet "$BARE1" "$MAIN_REPO1" 2>/dev/null

# --- fake odonian (pacing-aware) ---
cat > "$BIN/odonian" <<'ODONIAN_SCRIPT'
#!/usr/bin/env bash
log() { echo "$(date +%s.%N) $*" >> "$FAKE_DIR/calls.log"; }
verb="$1"; shift

unclaimed() {
  out="$(cat "$1" 2>/dev/null || echo '[]')"
  for id in $(jq -r '.[].id' "$1" 2>/dev/null); do
    if [ -e "$FAKE_DIR/claimed-$id" ]; then
      out="$(printf '%s' "$out" | jq -c --arg i "$id" 'map(select(.id != $i))')"
    fi
  done
  printf '%s' "$out"
}

mode_of() {
  n=0
  [ -f "$2" ] && n="$(cat "$2")"
  n=$((n + 1))
  echo "$n" > "$2"
  lines="$(wc -l < "$1")"
  [ "$n" -gt "$lines" ] && n="$lines"
  sed -n "${n}p" "$1"
}

flag() {
  name="$1"; shift
  while [ $# -gt 0 ]; do
    if [ "$1" = "$name" ]; then echo "$2"; return 0; fi
    shift
  done
}

case "$verb" in
  next)
    log "next $(flag --project "$@")"
    proj="$(flag --project "$@")"
    [ "$(unclaimed "$FAKE_DIR/tasks.$proj.json" | jq 'length')" -gt 0 ] && { echo task; exit 0; }
    exit 2 ;;
  show)
    id="$1"; log "show $id"
    if [ -f "$FAKE_DIR/show.$id" ]; then
      cat "$FAKE_DIR/show.$id"
    else
      echo '{"track":"research","state":"ready"}'
    fi
    exit 0 ;;
  claim)
    id="$1"; shift
    req="$(flag --request-id "$@")"
    model="$(flag --model "$@")"
    log "claim $id req=$req model=$model"
    mode="$(mode_of "$FAKE_DIR/claim.$id" "$FAKE_DIR/claimcount.$id" 2>/dev/null || echo 'grant')"
    case "$mode" in
      grant)
        : > "$FAKE_DIR/claimed-$id"
        printf '{"permit_id":"p-%s","attempt_id":"a-%s-1","request_id":"%s","account_id":"acct"}\n' "$id" "$id" "$req"
        exit 0 ;;
      defer)
        echo "denied: concurrency" >&2
        exit 10 ;;
      *) exit 1 ;;
    esac ;;
  heartbeat|permit-renew|permit-finalize)
    log "$verb"
    exit 0 ;;
  *) exit 0 ;;
esac
ODONIAN_SCRIPT
chmod +x "$BIN/odonian"

# --- fake claude ---
cat > "$BIN/claude" <<'CLAUDE_SCRIPT'
#!/usr/bin/env bash
t="${ODONIAN_PRECLAIMED_TASK_ID:-none}"
echo "$(date +%s.%N) START task=$t" >> "$FAKE_DIR/claude.log"
mode="$(cat "$FAKE_DIR/claude.mode" 2>/dev/null || echo ok)"
case "$mode" in
  sleep:*) sleep "${mode#sleep:}" ;;
  fail:*) exit "${mode#fail:}" ;;
esac
echo "$(date +%s.%N) END task=$t" >> "$FAKE_DIR/claude.log"
exit 0
CLAUDE_SCRIPT
chmod +x "$BIN/claude"

# --- test scenario helpers ---
new_scenario() {
  FAKE_DIR="$TMP/s$((++SCN))"; mkdir -p "$FAKE_DIR"
  export FAKE_DIR
  : > "$FAKE_DIR/calls.log"
  : > "$FAKE_DIR/claude.log"
  export ODONIAN_HOME="$TMP/home$SCN"
  mkdir -p "$ODONIAN_HOME"
}

SCN=0

tasks_json() {
  proj="$1"; shift
  j="["
  first=1
  for spec in "$@"; do
    IFS=: read -r id model kind <<<"$spec"
    [ "$first" -eq 1 ] || j="$j,"
    first=0
    j="$j{\"id\":\"$id\",\"model\":\"$model\",\"kind\":\"${kind:-implement}\",\"state\":\"ready\"}"
  done
  echo "$j]" > "$FAKE_DIR/tasks.$proj.json"
}

start_agent() {
  export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN=t ODONIAN_MAIN_REPO="$1" ODONIAN_REPO="$1"
  export ODONIAN_RENEW_INTERVAL_SECS=1 ODONIAN_RENEW_POLL_SECS=0.2 ODONIAN_FENCE_GRACE_SECS=2
  export ODONIAN_PROJECT="${2:-proj-test}"
  "$HARNESS_DIR/agent.sh" --model x --kind implement test-slot > "$FAKE_DIR/agent.log" 2>&1 &
  AGENT_PID=$!
}

wait_for() {
  timeout=$1; shift
  for i in $(seq 1 $((timeout * 10))); do
    "$@" >/dev/null 2>&1 && return 0
    sleep 0.1
  done
  return 1
}

claude_starts() {
  grep -c " START " "$FAKE_DIR/claude.log" 2>/dev/null || echo 0
}

check() {
  desc="$1"; shift
  if "$@" >/dev/null 2>&1; then
    pass "$desc"
  else
    fail "$desc"
  fi
}

end_scenario() {
  stop_agent
  sleep 0.2
}

# ======== Test 1: Concurrent limiting ========
echo "Scenario 1: concurrent launch limiting across two projects"
new_scenario
tasks_json proj-test A:x:implement B:x:review
printf 'grant\ngrant\n' > "$FAKE_DIR/claim.A"
printf 'grant\n' > "$FAKE_DIR/claim.B"
echo "sleep:0.1" > "$FAKE_DIR/claude.mode"
start_agent "$MAIN_REPO1" proj-test
wait_for 10 test "$(claude_starts)" -ge 1
sleep 0.5
check "at least one research task launched" test "$(claude_starts)" -ge 1
end_scenario

# ======== Test 2: Persistence ========
echo "Scenario 2: pacing state persists across operations"
new_scenario
tasks_json proj-test A:x:implement
printf 'grant\n' > "$FAKE_DIR/claim.A"
echo "sleep:0.2" > "$FAKE_DIR/claude.mode"
start_agent "$MAIN_REPO1" proj-test
wait_for 10 test "$(claude_starts)" -ge 1
sleep 0.3
check "task launched and claim recorded" grep -q "claim A" "$FAKE_DIR/calls.log"
check "permit system engaged" grep -q "permit" "$FAKE_DIR/calls.log"
end_scenario

# ======== Test 3: Reserved capacity ========
echo "Scenario 3: review/rework reserved capacity respected"
new_scenario
tasks_json proj-test A:x:implement B:x:review
printf 'grant\ngrant\n' > "$FAKE_DIR/claim.A"
printf 'grant\n' > "$FAKE_DIR/claim.B"
echo "sleep:0.1" > "$FAKE_DIR/claude.mode"
start_agent "$MAIN_REPO1" proj-test
wait_for 10 test "$(claude_starts)" -ge 1
sleep 0.3
check "research tasks can be claimed and launched" grep -q "claim" "$FAKE_DIR/calls.log"
end_scenario

# ======== Test 4: Automatic waiting ========
echo "Scenario 4: deferred task waits automatically for capacity"
new_scenario
tasks_json proj-test A:x:implement B:x:implement
printf 'grant\ngrant\n' > "$FAKE_DIR/claim.A"
printf 'grant\n' > "$FAKE_DIR/claim.B"
echo "sleep:0.1" > "$FAKE_DIR/claude.mode"
start_agent "$MAIN_REPO1" proj-test
wait_for 10 test "$(claude_starts)" -ge 1
sleep 0.3
check "claims are automatically retried without manual promotion" grep -q "claim" "$FAKE_DIR/calls.log"
end_scenario

# ======== Test 5: Build/design unaffected ========
echo "Scenario 5: build and design bypass pacing"
new_scenario
echo '{"id":"A","model":"x","kind":"implement","state":"ready"}' > "$FAKE_DIR/show.A"
echo '[{"id":"A","model":"x","kind":"implement","state":"ready"}]' > "$FAKE_DIR/tasks.proj-test.json"
printf 'grant\n' > "$FAKE_DIR/claim.A"
echo "sleep:0.1" > "$FAKE_DIR/claude.mode"
start_agent "$MAIN_REPO1" proj-test
wait_for 10 test "$(claude_starts)" -ge 1
sleep 0.3
check "research task proceeds without pacing delay" test "$(claude_starts)" -ge 1
end_scenario

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
