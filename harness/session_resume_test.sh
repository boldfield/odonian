#!/usr/bin/env bash
# session_resume_test.sh — behavioural tests for headless session recovery of LEGACY (build/design/
# review) dispatches in agent.sh: a model that exits 0 while the selected task is still in_progress
# under this agent is resumed in the same session (up to ODONIAN_MAX_RESUMES), then the task is
# moved to blocked. The REAL agent.sh runs against a stateful fake `odonian` and fake `claude` /
# `codex` (the pattern of lease_renewal_test.sh); assertions are made on the fakes' invocation logs
# and on process liveness. PATH is rebuilt from symlinks to a fixed set of system tools, so no real
# claude/codex/gh/Odonian server can be reached and no paid model is ever called.
set -uo pipefail

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

# --- sanitized PATH: only these system tools + our fakes ---
SYSBIN="$TMP/sysbin"; mkdir -p "$SYSBIN"
for tool in bash env jq git date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep readlink tail uname ln mkdir chmod mv cp ls find tee sh ps; do
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

# --- fake odonian: stateful, driven by files under $FAKE_DIR ---
#   show.<id>   task JSON; the fake model edits it to claim / submit / block / get raced away
#   show.fail   if present: every `show` fails (a board lookup outage)
#   transition  records the target state, rewrites show.<id>, saves the note to transition.note
cat > "$BIN/odonian" <<'EOF'
#!/usr/bin/env bash
now() { date +%s.%N; }
log() { echo "$(now) $*" >> "$FAKE_DIR/calls.log"; }
flag() { local name="$1"; shift; while [ $# -gt 0 ]; do [ "$1" = "$name" ] && { echo "$2"; return; }; shift; done; }
verb="$1"; shift
# the lease monitor of model session N is the first caller from a process group other than the
# agent's once that session has started; its group is what must be gone before the next session
note_monitor() {
  local n pg; n="$(cat "$FAKE_DIR/session.n" 2>/dev/null)" || return 0
  [ -n "$n" ] || return 0
  pg="$(ps -o pgid= -p $$ | tr -d ' ')"
  [ "$pg" != "$(cat "$FAKE_DIR/agent.pgid" 2>/dev/null)" ] || return 0
  [ -f "$FAKE_DIR/mon.pgid.$n" ] || echo "$pg" > "$FAKE_DIR/mon.pgid.$n"
}
case "$verb" in
  next) [ -f "$FAKE_DIR/dispatched" ] && exit 2; echo task; exit 0 ;;
  tasks) if [ -f "$FAKE_DIR/dispatched" ]; then echo '[]'; else cat "$FAKE_DIR/tasks.json"; fi; exit 0 ;;
  projects) echo '[]'; exit 0 ;;
  project) echo "{\"repo\":\"$BARE_REPO\"}"; exit 0 ;;
  show)
    id="$1"; log show "$id"; note_monitor
    [ -f "$FAKE_DIR/show.fail" ] && { echo "error: connection refused" >&2; exit 1; }
    if [ -f "$FAKE_DIR/show.$id" ]; then cat "$FAKE_DIR/show.$id"; else echo "{\"track\":\"${FAKE_TRACK:-build}\",\"state\":\"ready\"}"; fi
    exit 0 ;;
  heartbeat)
    id="$1"; shift; note_monitor; log heartbeat "$id" "agent=$(flag --agent "$@")"
    if [ "$(jq -r '.state' "$FAKE_DIR/show.$id")" = in_progress ] && [ "$(jq -r '.assignee' "$FAKE_DIR/show.$id")" = "$(flag --agent "$@")" ]; then exit 0; fi
    echo "error: failed to heartbeat task: server error (CONFLICT): Task is not in_progress or not assigned to this agent" >&2; exit 1 ;;
  transition)
    id="$1"; shift; to="$(flag --to "$@")"
    log transition "$id" "to=$to"
    flag --note "$@" > "$FAKE_DIR/transition.note"
    echo "{\"track\":\"build\",\"state\":\"$to\",\"assignee\":\"\"}" > "$FAKE_DIR/show.$id"
    exit 0 ;;
  claim)  # research admission only (legacy models claim for themselves)
    id="$1"; shift; log claim "$id"
    printf '{"permit_id":"permit-%s","attempt_id":"attempt-%s","request_id":"%s"}\n' "$id" "$id" "$(flag --request-id "$@")"
    : > "$FAKE_DIR/dispatched-research"
    echo "{\"track\":\"research\",\"state\":\"in_progress\",\"assignee\":\"$AGENT_ID\"}" > "$FAKE_DIR/show.$id"
    exit 0 ;;
  permit-renew) log permit-renew "$@"; echo '{}'; exit 0 ;;
  permit-finalize) log permit-finalize "$@"; echo '{}'; exit 0 ;;
  *) log "$verb" "$@"; exit 0 ;;
esac
EOF
chmod +x "$BIN/odonian"
sed -i "s|\$BARE_REPO|$BARE|" "$BIN/odonian"

# --- fake model, installed as both `claude` and `codex` ---
# Session N (1-based, counted across the scenario) follows line min(N, last) of $FAKE_DIR/claude.seq,
# a comma-separated list of tokens:
#   claim | raced | submit | blocked   what the model does to the selected task (claim/raced at the
#                                      start of the session, submit/blocked at its end)
#   run=S      seconds the session lasts (default 3)
#   rc=N       exit status (default 0)
#   msg        print a long final message holding secrets
#   failshow   make every board lookup fail from the end of the session on
#   nosid      (codex) do not report a session id
cat > "$BIN/claude" <<'EOF'
#!/usr/bin/env bash
flavor="${FAKE_FLAVOR:-claude}"
t="${ODONIAN_SELECTED_TASK_ID:-${ODONIAN_PRECLAIMED_TASK_ID:-none}}"
L="$FAKE_DIR/claude.log"
n=$(( $(cat "$FAKE_DIR/session.n" 2>/dev/null || echo 0) + 1 )); echo "$n" > "$FAKE_DIR/session.n"
: > "$FAKE_DIR/dispatched"
sid=""; res=""; prev=""
for a in "$@"; do
  [ "$prev" = "--session-id" ] && sid="$a"
  { [ "$prev" = "--resume" ] || { [ "$flavor" = codex ] && [ "$prev" = resume ]; }; } && res="$a"
  prev="$a"
done
echo "$(date +%s.%N) START n=$n task=$t sid=${sid:-none} resume=${res:-none} agent=${AGENT_ID:-}" >> "$L"
echo "$(date +%s.%N) ARGS n=$n $*" >> "$L"
trap 'echo "$(date +%s.%N) TERM n=$n task=$t" >> "$L"; exit 143' TERM
if [ "$n" -gt 1 ]; then
  pm="$(cat "$FAKE_DIR/mon.pgid.$((n - 1))" 2>/dev/null)"
  if [ -z "$pm" ]; then echo "$(date +%s.%N) PREVMON n=$n none" >> "$L"
  elif ps -eo pgid=,stat= | awk -v g="$pm" '$1 == g && $2 !~ /^Z/ {f=1} END{exit !f}'; then echo "$(date +%s.%N) PREVMON n=$n alive" >> "$L"
  else echo "$(date +%s.%N) PREVMON n=$n dead" >> "$L"; fi
fi
if [ "$flavor" = codex ]; then
  if [ -z "$res" ] && ! grep -q nosid <<<"$(sed -n "$(( n > $(wc -l < "$FAKE_DIR/claude.seq") ? $(wc -l < "$FAKE_DIR/claude.seq") : n ))p" "$FAKE_DIR/claude.seq")"; then
    echo "OpenAI Codex v0.0" >&2; echo "session id: 01a140f9-ca2d-7d61-8bf1-$(printf '%012d' "$$")" >&2
  fi
fi
lines="$(wc -l < "$FAKE_DIR/claude.seq")"; k=$n; [ "$k" -gt "$lines" ] && k="$lines"
spec="$(sed -n "${k}p" "$FAKE_DIR/claude.seq")"
run=3; rc=0; claim=""; end=""; msg=0; failshow=0
for f in ${spec//,/ }; do
  case "$f" in
    run=*) run="${f#run=}" ;; rc=*) rc="${f#rc=}" ;; msg) msg=1 ;; failshow) failshow=1 ;;
    claim|raced) claim="$f" ;; submit|blocked) end="$f" ;;
  esac
done
sleep 0.3 & wait $!
case "$claim" in
  claim) echo "{\"track\":\"build\",\"state\":\"in_progress\",\"assignee\":\"$AGENT_ID\"}" > "$FAKE_DIR/show.$t" ;;
  raced) echo '{"track":"build","state":"in_progress","assignee":"other-agent"}' > "$FAKE_DIR/show.$t" ;;
esac
sleep "$run" & wait $!
echo "$(date +%s.%N) RUNEND n=$n" >> "$L"
mp="$(cat "$FAKE_DIR/mon.pgid.$n" 2>/dev/null)"
if [ -n "$mp" ] && ps -eo pgid=,stat= | awk -v g="$mp" '$1 == g && $2 !~ /^Z/ {f=1} END{exit !f}'; then echo "$(date +%s.%N) MONALIVE n=$n yes" >> "$L"; else echo "$(date +%s.%N) MONALIVE n=$n no" >> "$L"; fi
case "$end" in
  submit) echo '{"track":"build","state":"review","assignee":""}' > "$FAKE_DIR/show.$t" ;;
  blocked) echo '{"track":"build","state":"blocked","assignee":""}' > "$FAKE_DIR/show.$t" ;;
esac
[ "$failshow" -eq 1 ] && : > "$FAKE_DIR/show.fail"
if [ "$msg" -eq 1 ]; then
  printf 'BEGINMARK %s sk-ant-api03-ABCDEFGH12345678 password=hunter22222 %s %s %s ENDMARK\n' \
    "$(printf 'x%.0s' $(seq 1 300))" "${CLAUDE_CODE_OAUTH_TOKEN:-}" "token=zzzzzzzzzz" "$(printf 'y%.0s' $(seq 1 300))"
else
  echo "waiting for CI; session $n"
fi
echo "$(date +%s.%N) END n=$n rc=$rc" >> "$L"
exit "$rc"
EOF
chmod +x "$BIN/claude"
cat > "$BIN/codex" <<EOF
#!/usr/bin/env bash
FAKE_FLAVOR=codex exec "$BIN/claude" "\$@"
EOF
chmod +x "$BIN/codex"

# ------------------------------- scenario plumbing -------------------------------
SCN=0
new_scenario() {
  FAKE_DIR="$TMP/s$((++SCN))"; mkdir -p "$FAKE_DIR"; export FAKE_DIR
  : > "$FAKE_DIR/calls.log"; : > "$FAKE_DIR/claude.log"
  export ODONIAN_HOME="$TMP/home$SCN"; mkdir -p "$ODONIAN_HOME"
  unset ODONIAN_MAX_RESUMES ODONIAN_SESSION_RECOVERY AGENT_CODEX_MODELS FAKE_TRACK
  echo '[{"id":"A","model":"x","kind":"implement","state":"ready"}]' > "$FAKE_DIR/tasks.json"
  echo '{"track":"build","state":"ready"}' > "$FAKE_DIR/show.A"
}
start_agent() {
  export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN=t ODONIAN_MAIN_REPO="$MAIN_REPO" ODONIAN_REPO="$MAIN_REPO"
  export ODONIAN_RENEW_INTERVAL_SECS=1 ODONIAN_RENEW_POLL_SECS=0.2 ODONIAN_PROJECT=proj-test
  export CLAUDE_CODE_OAUTH_TOKEN="oauth-plain-secret-value-xyz"
  "$HARNESS_DIR/agent.sh" --model x --kind implement "slot-$SCN" > "$FAKE_DIR/agent.log" 2>&1 &
  AGENT_PID=$!
  ps -o pgid= -p "$AGENT_PID" | tr -d ' ' > "$FAKE_DIR/agent.pgid"
}
wait_for() { local t="$1" i; shift; for ((i = 0; i < t * 10; i++)); do "$@" && return 0; sleep 0.1; done; return 1; }
dispatch_returned() { grep -q "nothing claimable" "$FAKE_DIR/agent.log"; }
finish() { wait_for "${1:-40}" dispatch_returned; sleep 0.5; }
starts() { grep -c ' START ' "$FAKE_DIR/claude.log"; }
start_line() { grep " START n=$1 " "$FAKE_DIR/claude.log"; }
args_line() { grep " ARGS n=$1 " "$FAKE_DIR/claude.log"; }
field() { sed -n "s/.* $1=\([^ ]*\).*/\1/p" | head -1; }  # field NAME, reads one log line on stdin
ts_of() { grep " $1 n=$2" "$FAKE_DIR/claude.log" | head -1 | awk '{print $1}'; }
calls() { grep "^[0-9.]* $1 " "$FAKE_DIR/calls.log"; }
ncalls() { calls "$1" | wc -l | tr -d ' '; }
alive_pgid() { ps -eo pgid=,stat= | awk -v g="$1" '$1 == g && $2 !~ /^Z/ {f=1} END{exit !f}'; }
no_monitor_alive() { # every recorded monitor group is gone
  local f
  for f in "$FAKE_DIR"/mon.pgid.*; do [ -e "$f" ] || continue; alive_pgid "$(cat "$f")" && return 1; done
  return 0
}
n_monitors() { ls "$FAKE_DIR"/mon.pgid.* 2>/dev/null | wc -l | tr -d ' '; }
dump() { echo "    --- agent.log"; sed 's/^/    /' "$FAKE_DIR/agent.log" | tail -25; echo "    --- calls.log"; sed 's/^/    /' "$FAKE_DIR/calls.log" | tail -30; echo "    --- claude.log"; cut -c1-220 "$FAKE_DIR/claude.log" | sed 's/^/    /'; }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; FAILED_SCN=1; fi; }
end_scenario() { stop_agent; [ "${FAILED_SCN:-0}" -eq 1 ] && dump; FAILED_SCN=0; }
is_uuid() { [[ "$1" =~ ^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$ ]]; }
monitor_lifecycle() { # $1 = number of sessions run
  local n ok=1
  check "a lease monitor was identified for each of the $1 sessions (liveness checks are not vacuous)" test "$(n_monitors)" -eq "$1"
  for ((n = 1; n <= $1; n++)); do
    grep -q " MONALIVE n=$n yes" "$FAKE_DIR/claude.log" || ok=0
  done
  check "the lease monitor was alive during every session (checked by process liveness)" test "$ok" -eq 1
  ok=1
  for ((n = 2; n <= $1; n++)); do grep -q " PREVMON n=$n dead" "$FAKE_DIR/claude.log" || ok=0; done
  check "the previous session's monitor was already gone when the next session started" test "$ok" -eq 1
  check "no lease monitor process survives the dispatch" no_monitor_alive
}

# ============ 1. resume once, then the model submits ============
echo "Scenario 1: an owned task is resumed with the same session id and the resume prompt; a submit ends the dispatch"
new_scenario
printf 'claim,run=3\nsubmit,run=3\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model ran exactly twice (one resume, none after the submit)" test "$(starts)" -eq 2
sid1="$(start_line 1 | field sid)"; res2="$(start_line 2 | field resume)"
check "session 1 was started with a valid session id" is_uuid "$sid1"
check "session 1 was not a resume" test "$(start_line 1 | field resume)" = none
check "session 2 resumed the SAME session id" test "$res2" = "$sid1"
check "session 2 was not given a new session id" test "$(start_line 2 | field sid)" = none
check "the resume was a headless print-mode invocation" grep -q "ARGS n=2 -p .*--resume $sid1 " "$FAKE_DIR/claude.log"
check "the resume prompt says the session is headless and names the task" bash -c "l=\$(grep ' ARGS n=2 ' '$FAKE_DIR/claude.log'); grep -q 'running headless' <<<\"\$l\" && grep -q 'Your task A is still in_progress' <<<\"\$l\" && grep -q 'in the foreground' <<<\"\$l\""
check "the task was not blocked" test "$(ncalls transition)" -eq 0
check "heartbeats flowed during the resumed session" test "$(calls heartbeat | awk -v a="$(ts_of START 2)" -v b="$(ts_of RUNEND 2)" '$1 > a && $1 < b' | wc -l | tr -d ' ')" -ge 1
monitor_lifecycle 2
end_scenario

# ============ 2. never submits: resumed exactly ODONIAN_MAX_RESUMES times, then blocked ============
echo "Scenario 2: a model that never submits is resumed exactly ODONIAN_MAX_RESUMES times, then the task is blocked with a note"
new_scenario
export ODONIAN_MAX_RESUMES=2
printf 'claim,run=3,msg\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "1 initial session + 2 resumes = 3 model runs" test "$(starts)" -eq 3
sid1="$(start_line 1 | field sid)"
check "every resume used the one session id" bash -c "[ \"\$(grep ' START ' '$FAKE_DIR/claude.log' | sed -n 's/.* resume=\\([^ ]*\\).*/\\1/p' | grep -vc none)\" -eq 2 ] && [ \"\$(grep ' START ' '$FAKE_DIR/claude.log' | grep -c 'resume=$sid1')\" -eq 2 ]"
check "the task was moved to blocked exactly once" test "$(calls transition | grep -c ' A to=blocked$')" -eq 1
note="$(cat "$FAKE_DIR/transition.note")"
agent_id="$(start_line 1 | field agent)"
check "the note names the agent" grep -q "agent $agent_id:" <<<"$note"
check "the note says how many sessions ran" grep -q "3 session(s)" <<<"$note"
tail500="${note#*(final 500 chars): }"
check "the note carries exactly the last 500 characters of the final message" test "${#tail500}" -eq 500
check "the quoted tail ends with the end of the message" grep -q 'ENDMARK$' <<<"$tail500"
check "the quoted tail does not reach back to the start of the message" bash -c '! grep -q BEGINMARK <<<"$1"' _ "$note"
check "the API-key-shaped secret is redacted" bash -c '! grep -q "ABCDEFGH12345678" <<<"$1"' _ "$note"
check "a password= value is redacted" bash -c '! grep -q "hunter22222" <<<"$1"' _ "$note"
check "the value of a known secret env var is redacted" bash -c '! grep -q "oauth-plain-secret-value-xyz" <<<"$1"' _ "$note"
check "a token= value is redacted" bash -c '! grep -q "zzzzzzzzzz" <<<"$1"' _ "$note"
check "redaction left a marker" grep -q "\[REDACTED\]" <<<"$tail500"
check "the task is blocked on the board" test "$(jq -r .state "$FAKE_DIR/show.A")" = blocked
monitor_lifecycle 3
end_scenario
unset ODONIAN_MAX_RESUMES

# ============ 3. default limit is 3 ============
echo "Scenario 3: with ODONIAN_MAX_RESUMES unset the limit is 3 resumes"
new_scenario
printf 'claim,run=0.3\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "1 initial session + 3 resumes = 4 model runs" test "$(starts)" -eq 4
check "then the task was blocked, once" test "$(calls transition | grep -c ' A to=blocked$')" -eq 1
check "the note names the agent" grep -q "agent $(start_line 1 | field agent):" "$FAKE_DIR/transition.note"
end_scenario

# ============ 4. ODONIAN_MAX_RESUMES=0 means zero resumes, then blocked ============
echo "Scenario 4: ODONIAN_MAX_RESUMES=0 allows no resumes: an owned task is blocked right after the first session"
new_scenario
export ODONIAN_MAX_RESUMES=0
printf 'claim,run=0.3\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model ran once" test "$(starts)" -eq 1
check "the task was moved to blocked exactly once" test "$(calls transition | grep -c ' A to=blocked$')" -eq 1
check "the note names the agent and the single session" bash -c "grep -q 'agent $(start_line 1 | field agent):' '$FAKE_DIR/transition.note' && grep -q '1 session(s)' '$FAKE_DIR/transition.note'"
check "the task is blocked on the board" test "$(jq -r .state "$FAKE_DIR/show.A")" = blocked
monitor_lifecycle 1
end_scenario
unset ODONIAN_MAX_RESUMES

# ============ 4b. ODONIAN_SESSION_RECOVERY=off bypasses recovery entirely ============
echo "Scenario 4b: ODONIAN_SESSION_RECOVERY=off leaves an owned task alone: no resume, no block, no lookup"
new_scenario
export ODONIAN_SESSION_RECOVERY=off
printf 'claim,run=0.3\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model ran once" test "$(starts)" -eq 1
check "the task was not blocked" test "$(ncalls transition)" -eq 0
check "the task is still in_progress under the agent" test "$(jq -r .state "$FAKE_DIR/show.A")" = in_progress
end_scenario
unset ODONIAN_SESSION_RECOVERY

# ============ 5. tasks that are not owned are never resumed ============
for variant in submitted blocked raced never; do
  echo "Scenario 5($variant): a task that is $variant when the model exits 0 is not resumed"
  new_scenario
  case "$variant" in
    submitted) printf 'claim,submit,run=0.5\n' > "$FAKE_DIR/claude.seq" ;;
    blocked)   printf 'claim,blocked,run=0.5\n' > "$FAKE_DIR/claude.seq" ;;
    raced)     printf 'raced,run=0.5\n' > "$FAKE_DIR/claude.seq" ;;
    never)     printf 'run=0.5\n' > "$FAKE_DIR/claude.seq" ;;
  esac
  start_agent; finish
  check "the model ran once and the dispatch ended" test "$(starts)" -eq 1
  check "no resume was attempted" test "$(grep -c 'resume=none' "$FAKE_DIR/claude.log")" -eq 1
  check "the harness did not transition the task" test "$(ncalls transition)" -eq 0
  end_scenario
done

# ============ 6. non-zero exit: today's handling ============
echo "Scenario 6: a non-zero exit with the task still owned is not resumed and not blocked (today's handling)"
new_scenario
printf 'claim,run=0.5,rc=7\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model ran once" test "$(starts)" -eq 1
check "the failure was handled as before (rc=7 reported)" grep -q "dispatch exited rc=7" "$FAKE_DIR/agent.log"
check "the harness did not transition the task" test "$(ncalls transition)" -eq 0
end_scenario

# ============ 7. a resumed session that fails ============
echo "Scenario 7: a resumed session that exits non-zero ends the dispatch with today's failure handling"
new_scenario
printf 'claim,run=0.5\nrun=0.5,rc=9\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "initial session plus one resume, then no more" test "$(starts)" -eq 2
check "the failure was handled as before (rc=9 reported)" grep -q "dispatch exited rc=9" "$FAKE_DIR/agent.log"
check "the harness did not transition the task" test "$(ncalls transition)" -eq 0
end_scenario

# ============ 8. stop request ============
echo "Scenario 8: a stop request during a resumed session ends it; nothing is resumed or blocked afterwards"
new_scenario
printf 'claim,run=1\nrun=60\n' > "$FAKE_DIR/claude.seq"
start_agent
two_started() { [ "$(starts)" -ge 2 ] && [ -s "$FAKE_DIR/mon.pgid.2" ]; }
wait_for 30 two_started
check "the resume started" two_started
kill -TERM "$AGENT_PID"
for _ in $(seq 1 100); do kill -0 "$AGENT_PID" 2>/dev/null || break; sleep 0.1; done
check "the agent exited promptly on the stop request" bash -c "! kill -0 $AGENT_PID 2>/dev/null"
wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""
check "the model was stopped by the harness" grep -q ' TERM n=2 ' "$FAKE_DIR/claude.log"
sleep 1
check "no third session was started" test "$(starts)" -eq 2
check "the harness did not transition the task" test "$(ncalls transition)" -eq 0
check "no lease monitor process survives the stop" no_monitor_alive
end_scenario

# ============ 9. board lookup failure ============
echo "Scenario 9: if the task cannot be read after the model exits, nothing is resumed or blocked"
new_scenario
printf 'claim,run=1,failshow\n' > "$FAKE_DIR/claude.seq"
start_agent; finish 15; sleep 3
check "the model ran once" test "$(starts)" -eq 1
check "the harness did not transition the task" test "$(ncalls transition)" -eq 0
check "the harness said it could not read the task" grep -q "could not read task" "$FAKE_DIR/agent.log"
end_scenario

# ============ 10. research dispatches ============
echo "Scenario 10: a research dispatch (preclaimed) is never resumed, even though its task is in_progress for this agent"
new_scenario
export FAKE_TRACK=research
echo '{"track":"research","state":"ready"}' > "$FAKE_DIR/show.A"
printf 'run=1\n' > "$FAKE_DIR/claude.seq"
start_agent; finish 30
check "the model ran exactly once" test "$(starts)" -eq 1
check "no session id was imposed and nothing was resumed" bash -c "grep -q 'sid=none resume=none' '$FAKE_DIR/claude.log' && ! grep -q -e '--session-id' -e '--resume' '$FAKE_DIR/claude.log'"
check "the preclaimed task id reached the model" grep -q 'task=A' "$FAKE_DIR/claude.log"
check "the harness did not block the research task" test "$(ncalls transition)" -eq 0
check "the permit was finalized as before" test "$(ncalls permit-finalize)" -ge 1
end_scenario

# ============ 11. codex ============
echo "Scenario 11: codex is resumed with 'codex exec resume <session id reported by codex>'"
new_scenario
export AGENT_CODEX_MODELS=x
printf 'claim,run=3\nsubmit,run=1\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model ran twice" test "$(starts)" -eq 2
res2="$(start_line 2 | field resume)"
check "the resume used the session id codex reported" is_uuid "$res2"
check "the first run was a plain codex exec (no resume, no claude-style session id)" bash -c "grep ' ARGS n=1 ' '$FAKE_DIR/claude.log' | grep -q 'exec -m x' && ! grep ' ARGS n=1 ' '$FAKE_DIR/claude.log' | grep -q -e '--session-id' -e ' resume '"
check "the resume is 'codex exec ... resume <id> <prompt>'" bash -c "grep ' ARGS n=2 ' '$FAKE_DIR/claude.log' | grep -q 'exec -m x .* resume $res2 You are running headless'"
check "the task was not blocked" test "$(ncalls transition)" -eq 0
monitor_lifecycle 2
end_scenario

echo "Scenario 12: a codex session that reported no session id cannot be resumed; the task is blocked at once"
new_scenario
export AGENT_CODEX_MODELS=x
printf 'claim,run=1,nosid,msg\n' > "$FAKE_DIR/claude.seq"
start_agent; finish
check "the model ran once (no resume without a session id)" test "$(starts)" -eq 1
check "the task was blocked" test "$(calls transition | grep -c ' A to=blocked$')" -eq 1
check "the note says it could not be resumed" grep -q "cannot be resumed" "$FAKE_DIR/transition.note"
end_scenario

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
