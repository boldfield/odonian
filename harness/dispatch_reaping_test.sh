#!/usr/bin/env bash
# dispatch_reaping_test.sh — behavioural tests for reaping the processes a dispatch started. The
# REAL agent.sh runs against a fake `odonian` and a fake `claude` that leaves background work behind
# (plain background jobs, a job in its own process group, a TERM-ignoring job in a new session via
# setsid). After dispatch() returns, no process carrying the dispatch's ODONIAN_DISPATCH_ID may be
# alive, while an unmarked process started by this test must be untouched. Every check is made on
# process liveness (kill -0 / ps), not on log text, and the model records that its background work
# was alive when it exited so "dead afterwards" cannot be vacuous. PATH is rebuilt from symlinks to a
# fixed set of system tools, so no real claude/codex/gh/Odonian server is reachable.
set -uo pipefail
unset ODONIAN_DISPATCH_ID   # an inherited marker would make the harness itself look like a reap target

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
AGENT_PID=""
CONTROL_PID=""
stop_agent() {
  if [ -n "$AGENT_PID" ]; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    for _ in $(seq 1 100); do kill -0 "$AGENT_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
    AGENT_PID=""
  fi
}
kill_leftovers() { # a failing test must not leak its loopers
  local f pid
  for f in "$TMP"/s*/bg.pids; do
    [ -f "$f" ] || continue
    while read -r pid; do kill -KILL "$pid" 2>/dev/null; done < "$f"
  done
  [ -n "$CONTROL_PID" ] && kill -KILL "$CONTROL_PID" 2>/dev/null
  return 0
}
trap 'stop_agent; kill_leftovers; rm -rf "$TMP"' EXIT

for tool in jq git ps; do command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }; done
HAVE_SETSID=0; command -v setsid >/dev/null && HAVE_SETSID=1

# --- sanitized PATH: only these system tools + our fakes ---
SYSBIN="$TMP/sysbin"; mkdir -p "$SYSBIN"
for tool in bash env jq git date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep readlink tail uname ln mkdir chmod mv cp ls find tee sh ps setsid; do
  p="$(command -v "$tool" 2>/dev/null)"; [ -n "$p" ] && [ "${p#/}" != "$p" ] && ln -sf "$p" "$SYSBIN/$tool"
done
BIN="$TMP/bin"; mkdir -p "$BIN"
export PATH="$BIN:$SYSBIN"

# --- a tiny real repo stands in for the project's repo ---
BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
SEED="$TMP/seed"; git clone --quiet "$BARE" "$SEED" 2>/dev/null
git -C "$SEED" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$SEED" push --quiet origin HEAD:main 2>/dev/null
MAIN_REPO="$TMP/main-repo"; git clone --quiet "$BARE" "$MAIN_REPO" 2>/dev/null

# --- fake odonian: stateful, driven by files under $FAKE_DIR (show.<id> holds the task JSON) ---
cat > "$BIN/odonian" <<'EOF'
#!/usr/bin/env bash
flag() { local name="$1"; shift; while [ $# -gt 0 ]; do [ "$1" = "$name" ] && { echo "$2"; return; }; shift; done; }
verb="$1"; shift
case "$verb" in
  next) [ -f "$FAKE_DIR/dispatched" ] && exit 2; echo task; exit 0 ;;
  tasks) if [ -f "$FAKE_DIR/dispatched" ]; then echo '[]'; else cat "$FAKE_DIR/tasks.json"; fi; exit 0 ;;
  projects) echo '[]'; exit 0 ;;
  project) echo "{\"repo\":\"$BARE_REPO\"}"; exit 0 ;;
  show)
    id="$1"
    if [ -f "$FAKE_DIR/show.$id" ]; then cat "$FAKE_DIR/show.$id"; else echo "{\"track\":\"${FAKE_TRACK:-build}\",\"state\":\"ready\"}"; fi
    exit 0 ;;
  heartbeat)
    id="$1"; shift
    if [ "$(jq -r '.state' "$FAKE_DIR/show.$id")" = in_progress ] && [ "$(jq -r '.assignee' "$FAKE_DIR/show.$id")" = "$(flag --agent "$@")" ]; then exit 0; fi
    exit 1 ;;
  transition)
    id="$1"; shift
    echo "{\"track\":\"build\",\"state\":\"$(flag --to "$@")\",\"assignee\":\"\"}" > "$FAKE_DIR/show.$id"
    exit 0 ;;
  claim)  # research admission only (legacy models claim for themselves)
    id="$1"; shift
    printf '{"permit_id":"permit-%s","attempt_id":"attempt-%s","request_id":"%s"}\n' "$id" "$id" "$(flag --request-id "$@")"
    echo "{\"track\":\"research\",\"state\":\"in_progress\",\"assignee\":\"$AGENT_ID\"}" > "$FAKE_DIR/show.$id"
    exit 0 ;;
  permit-renew|permit-finalize) echo '{}'; exit 0 ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$BIN/odonian"
sed -i "s|\$BARE_REPO|$BARE|" "$BIN/odonian"

# --- fake model ---
# Session N (1-based) follows line min(N, last) of $FAKE_DIR/claude.seq, comma-separated tokens:
#   claim | submit   claim the selected task at the start / submit it at the end
#   run=S            seconds the session lasts after starting its background work (default 1)
#   rc=N             exit status (default 0)
# Every session leaves behind: a plain background job, a job in its own process group (job control),
# and — when setsid exists — a TERM-ignoring looper in a new session. Their pids go to $FAKE_DIR/bg.pids.
cat > "$BIN/claude" <<'EOF'
#!/usr/bin/env bash
t="${ODONIAN_SELECTED_TASK_ID:-${ODONIAN_PRECLAIMED_TASK_ID:-none}}"
n=$(( $(cat "$FAKE_DIR/session.n" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FAKE_DIR/session.n"
: > "$FAKE_DIR/dispatched"
echo "$n ${ODONIAN_DISPATCH_ID:-}" >> "$FAKE_DIR/markers"
trap 'exit 143' TERM
lines="$(wc -l < "$FAKE_DIR/claude.seq")"; k=$n; [ "$k" -gt "$lines" ] && k="$lines"
spec="$(sed -n "${k}p" "$FAKE_DIR/claude.seq")"
run=1; rc=0; claim=0; submit=0
for f in ${spec//,/ }; do
  case "$f" in run=*) run="${f#run=}" ;; rc=*) rc="${f#rc=}" ;; claim) claim=1 ;; submit) submit=1 ;; esac
done
[ "$claim" -eq 1 ] && echo "{\"track\":\"build\",\"state\":\"in_progress\",\"assignee\":\"$AGENT_ID\"}" > "$FAKE_DIR/show.$t"
export PIDS="$FAKE_DIR/bg.pids"
LOOP='echo $$ >> "$PIDS"; while :; do sleep 1; done'
bash -c "$LOOP" >/dev/null 2>&1 &
set -m
bash -c "$LOOP" >/dev/null 2>&1 &
set +m
if [ "${FAKE_HAVE_SETSID:-0}" -eq 1 ]; then
  setsid bash -c "trap '' TERM; $LOOP" >/dev/null 2>&1 &
fi
for _ in $(seq 1 50); do [ "$(wc -l < "$PIDS")" -ge $((n * ${FAKE_BG_PER_SESSION:-3})) ] && break; sleep 0.1; done
sleep "$run" & wait $!
alive=0
while read -r pid; do
  st="$(ps -o stat= -p "$pid" 2>/dev/null | tr -d ' ')"
  [ -n "$st" ] && [ "${st#Z}" = "$st" ] && alive=$((alive + 1))
done < "$PIDS"
echo "$n $alive" >> "$FAKE_DIR/alive.at.exit"
[ "$submit" -eq 1 ] && echo '{"track":"build","state":"review","assignee":""}' > "$FAKE_DIR/show.$t"
echo "session $n done"
exit "$rc"
EOF
chmod +x "$BIN/claude"

# ------------------------------- scenario plumbing -------------------------------
SCN=0
new_scenario() {
  FAKE_DIR="$TMP/s$((++SCN))"; mkdir -p "$FAKE_DIR"; export FAKE_DIR
  : > "$FAKE_DIR/bg.pids"
  export ODONIAN_HOME="$TMP/home$SCN"; mkdir -p "$ODONIAN_HOME"
  unset ODONIAN_MAX_RESUMES ODONIAN_SESSION_RECOVERY AGENT_CODEX_MODELS FAKE_TRACK
  export FAKE_HAVE_SETSID="$HAVE_SETSID"
  if [ "$HAVE_SETSID" -eq 1 ]; then export FAKE_BG_PER_SESSION=3; else export FAKE_BG_PER_SESSION=2; fi
  echo '[{"id":"A","model":"x","kind":"implement","state":"ready"}]' > "$FAKE_DIR/tasks.json"
  echo '{"track":"build","state":"ready"}' > "$FAKE_DIR/show.A"
  # an unmarked process the harness must never touch: not a descendant of any dispatch
  env -u ODONIAN_DISPATCH_ID sleep 600 &
  CONTROL_PID=$!
}
start_agent() {
  export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN=t ODONIAN_MAIN_REPO="$MAIN_REPO" ODONIAN_REPO="$MAIN_REPO"
  export ODONIAN_RENEW_INTERVAL_SECS=1 ODONIAN_RENEW_POLL_SECS=0.2 ODONIAN_PROJECT=proj-test
  "$HARNESS_DIR/agent.sh" --model x --kind implement "slot-$SCN" > "$FAKE_DIR/agent.log" 2>&1 &
  AGENT_PID=$!
}
wait_for() { local t="$1" i; shift; for ((i = 0; i < t * 10; i++)); do "$@" && return 0; sleep 0.1; done; return 1; }
dispatch_returned() { grep -q "nothing claimable" "$FAKE_DIR/agent.log" 2>/dev/null; }
finish() { wait_for "${1:-40}" dispatch_returned; sleep 0.3; }
is_alive() { # a live, non-zombie process
  local st; st="$(ps -o stat= -p "$1" 2>/dev/null | tr -d ' ')"
  [ -n "$st" ] && [ "${st#Z}" = "$st" ]
}
n_alive_bg() { local pid c=0; while read -r pid; do is_alive "$pid" && c=$((c + 1)); done < "$FAKE_DIR/bg.pids"; echo "$c"; }
n_bg() { wc -l < "$FAKE_DIR/bg.pids" | tr -d ' '; }
dump() { echo "    --- agent.log"; sed 's/^/    /' "$FAKE_DIR/agent.log" | tail -25; }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; FAILED_SCN=1; fi; }
end_scenario() {
  stop_agent
  if [ -n "$CONTROL_PID" ]; then { kill -KILL "$CONTROL_PID"; wait "$CONTROL_PID"; } 2>/dev/null; fi
  CONTROL_PID=""
  kill_leftovers
  [ "${FAILED_SCN:-0}" -eq 1 ] && dump; FAILED_SCN=0
}
control_alive() { is_alive "$CONTROL_PID"; }
# $1 = number of model sessions that ran
bg_reaped() {
  local per=$FAKE_BG_PER_SESSION
  check "the model left $(( $1 * per )) background processes behind in total" test "$(n_bg)" -eq $(( $1 * per ))
  check "they were all alive when the model exited (so 'dead afterwards' is not vacuous)" \
    bash -c "[ \"\$(wc -l < '$FAKE_DIR/alive.at.exit')\" -eq $1 ] && ! awk -v p=$per '\$2 != p * \$1 { bad=1 } END { exit bad }' '$FAKE_DIR/alive.at.exit' | grep -q ."
  check "none of them is alive after dispatch() returned (process liveness)" test "$(n_alive_bg)" -eq 0
  check "the unmarked control process was not touched" control_alive
}
marker_ok() { # every session had a distinct-looking marker, identical within the dispatch
  local m; m="$(awk '{print $2}' "$FAKE_DIR/markers" | sort -u)"
  [ "$(echo "$m" | wc -l | tr -d ' ')" -eq 1 ] && [[ "$m" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]
}

# ============ 1. successful exit ============
echo "Scenario 1: a model that exits 0 after submitting leaves no live process behind"
new_scenario
printf 'claim,submit,run=1\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model got a unique ODONIAN_DISPATCH_ID marker (a uuid)" marker_ok
bg_reaped 1
check "the reaping was logged with a count" grep -q "reaping [0-9]* process(es)" "$FAKE_DIR/agent.log"
end_scenario

# ============ 2. non-zero exit ============
echo "Scenario 2: a model that exits non-zero leaves no live process behind"
new_scenario
printf 'claim,run=1,rc=7\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the failure was handled as before (rc=7 reported)" grep -q "dispatch exited rc=7" "$FAKE_DIR/agent.log"
bg_reaped 1
end_scenario

# ============ 3. after resumes ============
echo "Scenario 3: processes left by every session of a resumed dispatch are gone when it ends"
new_scenario
export ODONIAN_MAX_RESUMES=2
printf 'claim,run=1\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model ran 3 sessions (1 + 2 resumes)" test "$(wc -l < "$FAKE_DIR/session.n" >/dev/null; cat "$FAKE_DIR/session.n")" -eq 3
check "all sessions shared one marker" marker_ok
check "the earlier sessions' background work was still alive when the next session ran (reaping happens once, at the end)" \
  bash -c "[ \"\$(awk '\$1 == 3 {print \$2}' '$FAKE_DIR/alive.at.exit')\" -eq $((3 * FAKE_BG_PER_SESSION)) ]"
bg_reaped 3
end_scenario
unset ODONIAN_MAX_RESUMES

# ============ 4. session recovery off ============
echo "Scenario 4: with ODONIAN_SESSION_RECOVERY=off the dispatch is still reaped"
new_scenario
export ODONIAN_SESSION_RECOVERY=off
printf 'claim,run=1\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
bg_reaped 1
end_scenario
unset ODONIAN_SESSION_RECOVERY

# ============ 5. stop request ============
echo "Scenario 5: a stop request mid-dispatch leaves no live process behind"
new_scenario
printf 'claim,run=60\n' > "$FAKE_DIR/claude.seq"
start_agent
all_started() { [ "$(n_bg)" -ge "$FAKE_BG_PER_SESSION" ]; }
wait_for 30 all_started
check "the model started its background work" all_started
sleep 0.5
if [ -r /proc/self/environ ]; then
  check "the harness itself (still running) carries no marker, so it can never be a reap target" \
    bash -c "[ -r /proc/$AGENT_PID/environ ] && ! tr '\\0' '\\n' < /proc/$AGENT_PID/environ | grep -q ODONIAN_DISPATCH_ID"
fi
check "the background work is alive before the stop" test "$(n_alive_bg)" -eq "$FAKE_BG_PER_SESSION"
kill -TERM "$AGENT_PID"
for _ in $(seq 1 150); do kill -0 "$AGENT_PID" 2>/dev/null || break; sleep 0.1; done
check "the agent exited on the stop request" bash -c "! kill -0 $AGENT_PID 2>/dev/null"
wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""
check "none of the background processes is alive after the agent exited (process liveness)" test "$(n_alive_bg)" -eq 0
check "the unmarked control process was not touched" control_alive
end_scenario

# ============ 6. research dispatch ============
echo "Scenario 6: a research (preclaimed) dispatch is reaped too"
new_scenario
export FAKE_TRACK=research
echo '{"track":"research","state":"ready"}' > "$FAKE_DIR/show.A"
printf 'run=1\n' > "$FAKE_DIR/claude.seq"
start_agent; finish 30
check "the research model ran once, carrying a marker" test "$(grep -c " ." "$FAKE_DIR/markers")" -eq 1
bg_reaped 1
end_scenario

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
