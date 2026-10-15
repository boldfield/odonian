#!/usr/bin/env bash
# lease_renewal_test.sh — behavioural tests for harness-side lease renewal of LEGACY (build/design/
# review) dispatches in agent.sh. The REAL agent.sh runs against a stateful fake `odonian` and a fake
# `claude` that "claims" the harness-selected task by editing the fake's task file; assertions are
# made on the fakes' invocation logs and on process liveness (never on agent log text alone). PATH is
# rebuilt from symlinks to a fixed set of system tools, so no real claude/codex/gh/Odonian server can
# be reached and no paid model is ever called.
set -uo pipefail
# These fakes' models exit 0 while their task is still owned, which headless session recovery would
# resume or block; recovery has its own suite (session_resume_test.sh), so bypass it here.
export ODONIAN_SESSION_RECOVERY=off

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
AGENT_PID=""
stop_agent() {
  if [ -n "$AGENT_PID" ]; then
    kill -TERM "$AGENT_PID" 2>/dev/null
    for _ in $(seq 1 100); do kill -0 "$AGENT_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$AGENT_PID" 2>/dev/null
    wait "$AGENT_PID" 2>/dev/null
    AGENT_PID=""
  fi
}
trap 'stop_agent; rm -rf "$TMP"' EXIT

for tool in jq git ps; do command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }; done
REAL_GIT="$(command -v git)"

# --- sanitized PATH: only these system tools + our fakes ---
SYSBIN="$TMP/sysbin"; mkdir -p "$SYSBIN"
for tool in bash env jq date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep readlink tail uname ln mkdir chmod mv cp ls find tee sh ps; do
  p="$(command -v "$tool" 2>/dev/null)"; [ -n "$p" ] && [ "${p#/}" != "$p" ] && ln -sf "$p" "$SYSBIN/$tool"
done
BIN="$TMP/bin"; mkdir -p "$BIN"

# --- a tiny real repo stands in for the project's repo (set up with the real git before PATH is cut) ---
BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
SEED="$TMP/seed"; git clone --quiet "$BARE" "$SEED" 2>/dev/null
git -C "$SEED" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$SEED" push --quiet origin HEAD:main 2>/dev/null
MAIN_REPO="$TMP/main-repo"; git clone --quiet "$BARE" "$MAIN_REPO" 2>/dev/null

# git shim: before delegating to the real git, record whether any live process still belongs to the
# lease monitor's process group. agent.sh only runs git outside dispatch() (post-dispatch worktree
# reset), so a probe taken after the model exited samples the world right after dispatch() returned.
cat > "$BIN/git" <<EOF
#!/usr/bin/env bash
if [ -n "\${FAKE_DIR:-}" ] && [ -f "\$FAKE_DIR/mon.pgid" ]; then
  pg="\$(cat "\$FAKE_DIR/mon.pgid")"
  if ps -eo pgid=,stat= | awk -v g="\$pg" '\$1 == g && \$2 !~ /^Z/ {f=1} END{exit !f}'; then st=alive; else st=dead; fi
  echo "\$(date +%s.%N) \$st" >> "\$FAKE_DIR/probe.log"
fi
exec "$REAL_GIT" "\$@"
EOF
chmod +x "$BIN/git"
export PATH="$BIN:$SYSBIN"

# --- fake odonian: stateful, driven by files under $FAKE_DIR ---
#   tasks.json   offered until a model has been launched (then the board looks empty)
#   show.<id>    task JSON; the fake claude edits it to claim / submit / get raced away
#   hb.fail      if present: heartbeat fails (transiently) that many times, then recovers
cat > "$BIN/odonian" <<'EOF'
#!/usr/bin/env bash
now() { date +%s.%N; }
log() { echo "$(now) $*" >> "$FAKE_DIR/calls.log"; }
flag() { local name="$1"; shift; while [ $# -gt 0 ]; do [ "$1" = "$name" ] && { echo "$2"; return; }; shift; done; }
verb="$1"; shift
# the first show/heartbeat after a launch is the lease monitor's (the loop reads the task only
# before launching); its process group is what must be gone once dispatch() returns
note_monitor() {
  [ -f "$FAKE_DIR/dispatched" ] && [ ! -f "$FAKE_DIR/mon.pgid" ] && ps -o pgid= -p $$ | tr -d ' ' > "$FAKE_DIR/mon.pgid"
}
case "$verb" in
  next) [ -f "$FAKE_DIR/dispatched" ] && exit 2; echo task; exit 0 ;;
  tasks) if [ -f "$FAKE_DIR/dispatched" ]; then echo '[]'; else cat "$FAKE_DIR/tasks.json"; fi; exit 0 ;;
  projects) echo '[]'; exit 0 ;;
  project) echo "{\"repo\":\"$BARE_REPO\"}"; exit 0 ;;
  show)
    id="$1"; log show "$id"; note_monitor
    if [ -f "$FAKE_DIR/show.$id" ]; then cat "$FAKE_DIR/show.$id"; else echo '{"track":"build","state":"ready"}'; fi
    exit 0 ;;
  heartbeat)
    id="$1"; shift; note_monitor; log heartbeat "$id" "attempt=$(flag --attempt "$@")" "agent=$(flag --agent "$@")"
    if [ -f "$FAKE_DIR/hb.fail" ]; then
      n="$(cat "$FAKE_DIR/hb.fail")"
      if [ "$n" -gt 0 ]; then echo $((n - 1)) > "$FAKE_DIR/hb.fail"; echo "error: failed to heartbeat task: connection reset" >&2; exit 1; fi
    fi
    # like the server: only the in_progress owner may renew
    if [ "$(jq -r '.state' "$FAKE_DIR/show.$id")" = in_progress ] && [ "$(jq -r '.assignee' "$FAKE_DIR/show.$id")" = "$(flag --agent "$@")" ]; then exit 0; fi
    echo "error: failed to heartbeat task: server error (CONFLICT): Task is not in_progress or not assigned to this agent" >&2; exit 1 ;;
  *) log "$verb" "$@"; exit 0 ;;
esac
EOF
chmod +x "$BIN/odonian"
sed -i "s|\$BARE_REPO|$BARE|" "$BIN/odonian"

# --- fake claude: behaviour from $FAKE_DIR/claude.mode ---
#   run:C:S:A   after C secs claim the selected task as this agent, run S more secs total, exit 0
#               after the claim; A = what happens at the claim step:
#                 claim    in_progress under $AGENT_ID
#                 never    no claim at all
#                 raced    in_progress under "other-agent"
#   Optional trailing fields: submit@T (move task to review T secs after the claim),
#   rc=N (exit status instead of 0).
cat > "$BIN/claude" <<'EOF'
#!/usr/bin/env bash
t="${ODONIAN_SELECTED_TASK_ID:-none}"
L="$FAKE_DIR/claude.log"
echo "$(date +%s.%N) START task=$t agent=${AGENT_ID:-}" >> "$L"
: > "$FAKE_DIR/dispatched"
trap 'echo "$(date +%s.%N) TERM task=$t" >> "$L"; echo "$(date +%s.%N) END rc=143 task=$t" >> "$L"; exit 143' TERM
IFS=: read -r _ pre run action extra <<<"$(cat "$FAKE_DIR/claude.mode")"
rc=0; submit_at=""
for f in ${extra//,/ }; do
  case "$f" in submit@*) submit_at="${f#submit@}" ;; rc=*) rc="${f#rc=}" ;; esac
done
sleep "$pre" & wait $!
case "$action" in
  claim) echo "{\"track\":\"build\",\"state\":\"in_progress\",\"assignee\":\"$AGENT_ID\"}" > "$FAKE_DIR/show.$t"
         echo "$(date +%s.%N) CLAIMED task=$t" >> "$L" ;;
  raced) echo '{"track":"build","state":"in_progress","assignee":"other-agent"}' > "$FAKE_DIR/show.$t"
         echo "$(date +%s.%N) RACED task=$t" >> "$L" ;;
esac
if [ -n "$submit_at" ]; then
  sleep "$submit_at" & wait $!
  echo '{"track":"build","state":"review","assignee":""}' > "$FAKE_DIR/show.$t"
  echo "$(date +%s.%N) SUBMITTED task=$t" >> "$L"
  run=$((run - submit_at))
fi
sleep "$run" & wait $!
echo "$(date +%s.%N) END rc=$rc task=$t" >> "$L"
exit "$rc"
EOF
chmod +x "$BIN/claude"

# ------------------------------- scenario plumbing -------------------------------
SCN=0
new_scenario() {
  FAKE_DIR="$TMP/s$((++SCN))"; mkdir -p "$FAKE_DIR"; export FAKE_DIR
  : > "$FAKE_DIR/calls.log"; : > "$FAKE_DIR/claude.log"; : > "$FAKE_DIR/probe.log"
  export ODONIAN_HOME="$TMP/home$SCN"; mkdir -p "$ODONIAN_HOME"
  echo '[{"id":"A","model":"x","kind":"implement","state":"ready"}]' > "$FAKE_DIR/tasks.json"
  echo '{"track":"build","state":"ready"}' > "$FAKE_DIR/show.A"
}
start_agent() {
  export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN=t ODONIAN_MAIN_REPO="$MAIN_REPO" ODONIAN_REPO="$MAIN_REPO"
  export ODONIAN_RENEW_INTERVAL_SECS=1 ODONIAN_RENEW_POLL_SECS=0.2 ODONIAN_PROJECT=proj-test
  "$HARNESS_DIR/agent.sh" --model x --kind implement "slot-$SCN" > "$FAKE_DIR/agent.log" 2>&1 &
  AGENT_PID=$!
  ps -o pgid= -p "$AGENT_PID" | tr -d ' ' > "$FAKE_DIR/agent.pgid"
}
wait_for() { local t="$1" i; shift; for ((i = 0; i < t * 10; i++)); do "$@" && return 0; sleep 0.1; done; return 1; }
has_claude() { grep -q "$1" "$FAKE_DIR/claude.log"; }
claude_ts() { grep " $1 " "$FAKE_DIR/claude.log" | head -1 | awk '{print $1}'; }
calls() { grep "^[0-9.]* $1 " "$FAKE_DIR/calls.log"; }
ncalls() { calls "$1" | wc -l | tr -d ' '; }
ncalls_before() { calls "$1" | awk -v t="$2" '$1 < t' | wc -l | tr -d ' '; }
ncalls_after() { calls "$1" | awk -v t="$2" '$1 > t' | wc -l | tr -d ' '; }
probes_after() { awk -v t="$1" -v s="$2" '$1 > t && $2 == s' "$FAKE_DIR/probe.log" | wc -l | tr -d ' '; }
monitor_dead() { ! monitor_alive; }
monitor_alive() {
  local pg; pg="$(cat "$FAKE_DIR/mon.pgid" 2>/dev/null)" || return 1
  [ -n "$pg" ] || return 1
  ps -eo pgid=,stat= | awk -v g="$pg" '$1 == g && $2 !~ /^Z/ {f=1} END{exit !f}'
}
dispatch_returned() { grep -q "nothing claimable" "$FAKE_DIR/agent.log"; }
dump() { echo "    --- agent.log"; sed 's/^/    /' "$FAKE_DIR/agent.log" | tail -20; echo "    --- calls.log"; sed 's/^/    /' "$FAKE_DIR/calls.log" | tail -25; echo "    --- claude.log"; sed 's/^/    /' "$FAKE_DIR/claude.log"; echo "    --- probe.log"; sed 's/^/    /' "$FAKE_DIR/probe.log"; }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; FAILED_SCN=1; fi; }
end_scenario() { stop_agent; [ "${FAILED_SCN:-0}" -eq 1 ] && dump; FAILED_SCN=0; }
no_monitor_after_dispatch() { # $1 = ts the model process ended
  check "a post-dispatch probe ran and found the monitor process group dead" test "$(probes_after "$1" dead)" -ge 1
  check "no post-dispatch probe ever found a live monitor process" test "$(probes_after "$1" alive)" -eq 0
  check "the monitor's process group was identified (so the liveness checks are not vacuous)" test -s "$FAKE_DIR/mon.pgid"
  check "no process of the monitor's group remains (liveness, not log text)" monitor_dead
}

# ============ 1. claim, then a run of several intervals: repeated heartbeats, none before the claim ============
echo "Scenario 1: the model claims the selected task and runs for several intervals -> repeated unfenced heartbeats after the claim only"
new_scenario
echo "run:1.5:6:claim" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 20 has_claude " END "; wait_for 5 dispatch_returned; sleep 0.5
claimed="$(claude_ts CLAIMED)"; end_t="$(claude_ts END)"
check "the model claimed the selected task" test -n "$claimed"
check "no heartbeat was sent before the claim" test "$(ncalls_before heartbeat "$claimed")" -eq 0
check "the monitor read the unclaimed task before the claim (to wait for ownership)" test "$(ncalls_before show "$claimed")" -ge 1
check "at least 3 heartbeats were sent while the model ran" test "$(calls heartbeat | awk -v t="$end_t" '$1 < t' | wc -l | tr -d ' ')" -ge 3
check "every heartbeat is for the selected task only" test "$(calls heartbeat | awk '$3 != "A"' | wc -l | tr -d ' ')" -eq 0
check "heartbeats are not fenced to an attempt (no --attempt)" test "$(calls heartbeat | grep -vc ' attempt= ')" -eq 0
agent_id="$(grep ' START ' "$FAKE_DIR/claude.log" | head -1 | sed 's/.* agent=//')"
check "heartbeats carry this agent's id" test "$(calls heartbeat | grep -c " agent=$agent_id\$")" -eq "$(ncalls heartbeat)"
check "once owned the monitor does not keep polling the task (no show after the claim)" test "$(ncalls_after show "$claimed")" -le 1
check "the model was not interrupted" bash -c "! grep -q TERM '$FAKE_DIR/claude.log' && grep -q 'END rc=0' '$FAKE_DIR/claude.log'"
no_monitor_after_dispatch "$end_t"
end_scenario

# ============ 2. never claimed ============
echo "Scenario 2: a task the model never claims gets no heartbeat, and the model is left alone"
new_scenario
echo "run:0:5:never" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 20 has_claude " END "; wait_for 5 dispatch_returned; sleep 0.5
end_t="$(claude_ts END)"
check "the monitor kept looking at the task while it was unclaimed (>=2 reads)" test "$(ncalls show)" -ge 3
check "zero heartbeats for a never-claimed task" test "$(ncalls heartbeat)" -eq 0
check "the model ran to completion untouched" bash -c "! grep -q TERM '$FAKE_DIR/claude.log' && grep -q 'END rc=0' '$FAKE_DIR/claude.log'"
no_monitor_after_dispatch "$end_t"
end_scenario

# ============ 3. submit mid-run ============
echo "Scenario 3: renewal stops, without killing the model, once the task is submitted mid-run"
new_scenario
echo "run:1.5:9:claim:submit@3" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 25 has_claude " END "; wait_for 5 dispatch_returned; sleep 0.5
claimed="$(claude_ts CLAIMED)"; sub_t="$(claude_ts SUBMITTED)"; end_t="$(claude_ts END)"
check "heartbeats were sent between the claim and the submit" test "$(calls heartbeat | awk -v a="$claimed" -v b="$sub_t" '$1 > a && $1 < b' | wc -l | tr -d ' ')" -ge 1
check "at most one further (failed) heartbeat after the submit, then none for the remaining ~4 intervals" test "$(ncalls_after heartbeat "$sub_t")" -le 1
check "the monitor confirmed ownership loss by reading the task after the failed heartbeat" test "$(ncalls_after show "$sub_t")" -ge 1
check "the model kept running after the submit and exited normally (not killed)" bash -c "! grep -q TERM '$FAKE_DIR/claude.log' && grep -q 'END rc=0' '$FAKE_DIR/claude.log'"
no_monitor_after_dispatch "$end_t"
end_scenario

# ============ 4. raced away ============
echo "Scenario 4: a task that goes in_progress under another agent is never renewed, and the model is not killed"
new_scenario
echo "run:1.5:5:raced" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 20 has_claude " END "; wait_for 5 dispatch_returned; sleep 0.5
end_t="$(claude_ts END)"
check "zero heartbeats for a task owned by another agent" test "$(ncalls heartbeat)" -eq 0
check "the monitor stopped reading once it saw the other owner (reads stop, not one per interval)" test "$(ncalls_after show "$(claude_ts RACED)")" -le 2
check "the model was not killed" bash -c "! grep -q TERM '$FAKE_DIR/claude.log' && grep -q 'END rc=0' '$FAKE_DIR/claude.log'"
no_monitor_after_dispatch "$end_t"
end_scenario

# ============ 5. transient heartbeat failure while still ours ============
echo "Scenario 5: a transient heartbeat failure while the task is still ours is retried next interval"
new_scenario
echo 2 > "$FAKE_DIR/hb.fail"
echo "run:1.5:7:claim" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 25 has_claude " END "; wait_for 5 dispatch_returned; sleep 0.5
end_t="$(claude_ts END)"
check "the two failed heartbeats were followed by successful ones (>=4 attempts in total)" test "$(ncalls heartbeat)" -ge 4
check "the transient failure was logged, not treated as ownership loss" grep -q "failed transiently" "$FAKE_DIR/agent.log"
no_monitor_after_dispatch "$end_t"
end_scenario

# ============ 6. model failure ============
echo "Scenario 6: a failing model exit still leaves no monitor behind"
new_scenario
echo "run:1.5:4:claim:rc=7" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 20 has_claude " END "; wait_for 5 dispatch_returned; sleep 0.5
end_t="$(claude_ts END)"
check "the dispatch failure was handled normally (rc=7 preserved)" grep -q "dispatch exited rc=7" "$FAKE_DIR/agent.log"
check "heartbeats were sent while the model ran" test "$(calls heartbeat | awk -v t="$end_t" '$1 < t' | wc -l | tr -d ' ')" -ge 1
no_monitor_after_dispatch "$end_t"
end_scenario

# ============ 7. stop request ============
echo "Scenario 7: a stop request mid-run tears down the model and leaves no monitor behind"
new_scenario
echo "run:0.5:60:claim" > "$FAKE_DIR/claude.mode"
start_agent
hb2() { [ "$(ncalls heartbeat)" -ge 2 ]; }
wait_for 15 hb2
check "heartbeats were flowing before the stop" hb2
kill -TERM "$AGENT_PID"
for _ in $(seq 1 100); do kill -0 "$AGENT_PID" 2>/dev/null || break; sleep 0.1; done
check "the agent exited promptly on the stop request" bash -c "! kill -0 $AGENT_PID 2>/dev/null"
wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""
term_t="$(claude_ts TERM)"
check "the model was stopped by the harness" test -n "$term_t"
no_monitor_after_dispatch "$term_t"
n_hb="$(ncalls heartbeat)"; sleep 2
check "no heartbeats continue after the stop" test "$(ncalls heartbeat)" -eq "$n_hb"
end_scenario

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
