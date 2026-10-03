#!/usr/bin/env bash
# research_admission_test.sh — behavioural tests for research admission in agent.sh (SINGLE and
# MULTI loops). The REAL agent.sh runs against a stateful fake `odonian` CLI and a fake `claude`;
# assertions are made on their invocation logs. PATH is rebuilt from symlinks to a fixed set of
# system tools, so neither a real `claude`/`codex`/`gh` nor a real Odonian server can be reached and
# no paid model is ever called.
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

for tool in jq git; do command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }; done

# --- sanitized PATH: only these system tools + our fakes ---
SYSBIN="$TMP/sysbin"; mkdir -p "$SYSBIN"
for tool in bash env jq git date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep readlink tail uname ln mkdir chmod mv cp ls find tee sh; do
  p="$(command -v "$tool" 2>/dev/null)"; [ -n "$p" ] && [ "${p#/}" != "$p" ] && ln -sf "$p" "$SYSBIN/$tool"
done
BIN="$TMP/bin"; mkdir -p "$BIN"
export PATH="$BIN:$SYSBIN"

# --- a tiny real repo stands in for every project's repo ---
BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
SEED="$TMP/seed"; git clone --quiet "$BARE" "$SEED" 2>/dev/null
git -C "$SEED" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$SEED" push --quiet origin HEAD:main 2>/dev/null
MAIN_REPO="$TMP/main-repo"; git clone --quiet "$BARE" "$MAIN_REPO" 2>/dev/null

# --- fake odonian: stateful, scenario-driven by files under $FAKE_DIR ---
cat > "$BIN/odonian" <<'EOF'
#!/usr/bin/env bash
# fake odonian. Scenario files in $FAKE_DIR:
#   tasks.<project>.json  claimable list; projects.json  [{id,repo}]; show.<task>  task JSON
#   claim.<task>          one mode per line, consumed per call (last repeats)
#   renew.mode / hb.mode / finalize.mode   ok | conflict | attempt-fenced | err
now() { date +%s.%N; }
log() { echo "$(now) $*" >> "$FAKE_DIR/calls.log"; }
verb="$1"; shift
unclaimed() { # $1 = tasks file -> JSON list without already-claimed ids
  local out="[]" id
  out="$(cat "$1" 2>/dev/null || echo '[]')"
  for id in $(jq -r '.[].id' "$1" 2>/dev/null); do
    [ -e "$FAKE_DIR/claimed-$id" ] && out="$(printf '%s' "$out" | jq -c --arg i "$id" 'map(select(.id != $i))')"
  done
  printf '%s' "$out"
}
mode_of() { # $1 file, $2 counter file
  local n=0 lines
  [ -f "$2" ] && n="$(cat "$2")"; n=$((n + 1)); echo "$n" > "$2"
  lines="$(wc -l < "$1")"; [ "$n" -gt "$lines" ] && n="$lines"
  sed -n "${n}p" "$1"
}
flag() { # flag NAME args... -> value
  local name="$1"; shift
  while [ $# -gt 0 ]; do [ "$1" = "$name" ] && { echo "$2"; return; }; shift; done
}
case "$verb" in
  next)
    log next "$@"
    proj="$(flag --project "$@")"
    [ "$(unclaimed "$FAKE_DIR/tasks.$proj.json" | jq 'length')" -gt 0 ] && { echo task; exit 0; }
    exit 2 ;;
  tasks)
    proj="$(flag --project "$@")"
    unclaimed "$FAKE_DIR/tasks.$proj.json"; exit 0 ;;
  projects)
    out="[]"
    for p in $(jq -r '.[].id' "$FAKE_DIR/projects.json"); do
      [ "$(unclaimed "$FAKE_DIR/tasks.$p.json" | jq 'length')" -gt 0 ] && \
        out="$(printf '%s' "$out" | jq -c --argjson p "$(jq -c --arg p "$p" '.[] | select(.id==$p)' "$FAKE_DIR/projects.json")" '. + [$p]')"
    done
    printf '%s' "$out"; exit 0 ;;
  project) echo "{\"repo\":\"$BARE_REPO\"}"; exit 0 ;;
  show)
    id="$1"; log show "$id"
    if [ -f "$FAKE_DIR/show.$id" ]; then cat "$FAKE_DIR/show.$id"; else echo '{"track":"research","state":"ready"}'; fi
    exit 0 ;;
  transition) log transition "$@"; exit 0 ;;
  claim)
    id="$1"; shift
    req="$(flag --request-id "$@")"
    log claim "$id" "req=$req" "model=$(flag --model "$@")" "agent=$(flag --agent "$@")"
    mode="$(mode_of "$FAKE_DIR/claim.$id" "$FAKE_DIR/claimcount.$id")"
    case "$mode" in
      grant)
        n="$(cat "$FAKE_DIR/grantcount" 2>/dev/null || echo 0)"; n=$((n + 1)); echo "$n" > "$FAKE_DIR/grantcount"
        : > "$FAKE_DIR/claimed-$id"
        printf '{"permit_id":"permit-%s","attempt_id":"attempt-%s-%s","request_id":"%s","account_id":"acct","expires_at":"2099-01-01T00:00:00Z"}\n' "$id" "$id" "$n" "$req"
        exit 0 ;;
      grant-bare) : > "$FAKE_DIR/claimed-$id"; exit 0 ;;
      defer:*) echo "scheduling: server error (ADMISSION_DENIED): denied" >&2; echo "outcome: defer" >&2; echo "reason: rate" >&2
               echo "retry-after: ${mode#defer:}" >&2; exit 10 ;;
      notbefore:*) echo "scheduling: denied" >&2
               echo "not-before: $(date -u -d "@$(( $(date +%s) + ${mode#notbefore:} ))" +%Y-%m-%dT%H:%M:%SZ)" >&2; exit 10 ;;
      err) echo "error: connection reset" >&2; exit 1 ;;
      conflict) echo "conflict: TASK_BUSY" >&2; exit 11 ;;
      taken) echo "error: already claimed" >&2; exit 3 ;;
    esac
    exit 1 ;;
  heartbeat)
    id="$1"; shift; log heartbeat "$id" "attempt=$(flag --attempt "$@")"
    case "$(cat "$FAKE_DIR/hb.mode" 2>/dev/null || echo ok)" in
      ok) exit 0 ;;
      attempt-fenced) echo "error: failed to heartbeat task: server error (ATTEMPT_FENCED): fenced" >&2; exit 1 ;;
      *) echo "error: failed to heartbeat task: server error (CONFLICT): Task is not in_progress or not assigned to this agent" >&2; exit 1 ;;
    esac ;;
  permit-renew)
    pid="$1"; shift
    log permit-renew "$pid" "task=$(flag --task-id "$@")" "model=$(flag --model "$@")" "agent=$(flag --agent-id "$@")" "req=$(flag --request-id "$@")" "attempt=$(flag --attempt-id "$@")"
    case "$(cat "$FAKE_DIR/renew.mode" 2>/dev/null || echo ok)" in
      ok) echo '{}'; exit 0 ;;
      conflict) echo "conflict: ATTEMPT_FENCED" >&2; exit 11 ;;
      *) echo "error: boom" >&2; exit 1 ;;
    esac ;;
  permit-finalize)
    pid="$1"; shift
    log permit-finalize "$pid" "task=$(flag --task-id "$@")" "model=$(flag --model "$@")" "agent=$(flag --agent-id "$@")" "req=$(flag --request-id "$@")" "attempt=$(flag --attempt-id "$@")" "class=$(flag --exit-class "$@")"
    echo '{}'; exit 0 ;;
  *) log "$verb" "$@"; exit 0 ;;
esac
EOF
chmod +x "$BIN/odonian"
# the fake's `project` verb needs the repo path
sed -i "s|\$BARE_REPO|$BARE|" "$BIN/odonian"

# --- fake claude: behaviour from $FAKE_DIR/claude.mode (ok | fail:N | sleep:N | submit-sleep:N) ---
cat > "$BIN/claude" <<'EOF'
#!/usr/bin/env bash
model=""; prev=""
for a in "$@"; do [ "$prev" = "--model" ] && model="$a"; prev="$a"; done
t="${ODONIAN_PRECLAIMED_TASK_ID:-}"
echo "$(date +%s.%N) START model=$model task=${t:-none} attempt=${ODONIAN_PRECLAIMED_ATTEMPT_ID:-none}" >> "$FAKE_DIR/claude.log"
trap 'echo "$(date +%s.%N) TERM task=${t:-none}" >> "$FAKE_DIR/claude.log"; exit 143' TERM
mode="$(cat "$FAKE_DIR/claude.mode" 2>/dev/null || echo ok)"
case "$mode" in
  fail:*) echo "$(date +%s.%N) END rc=${mode#fail:} task=${t:-none}" >> "$FAKE_DIR/claude.log"; exit "${mode#fail:}" ;;
  sleep:*|submit-sleep:*)
    n="${mode#*:}"
    if [ "${mode%%:*}" = submit-sleep ]; then
      sleep 0.5; echo "{\"track\":\"research\",\"state\":\"review\"}" > "$FAKE_DIR/show.$t"
      echo "$(date +%s.%N) SUBMITTED task=$t" >> "$FAKE_DIR/claude.log"
    fi
    sleep "$n" & wait $!
    echo "$(date +%s.%N) END rc=0 task=${t:-none}" >> "$FAKE_DIR/claude.log"; exit 0 ;;
esac
echo "$(date +%s.%N) END rc=0 task=${t:-none}" >> "$FAKE_DIR/claude.log"
exit 0
EOF
chmod +x "$BIN/claude"

# ------------------------------- scenario plumbing -------------------------------
new_scenario() { # fresh FAKE_DIR + ODONIAN_HOME; echo path
  FAKE_DIR="$TMP/s$((++SCN))"; mkdir -p "$FAKE_DIR"; export FAKE_DIR
  : > "$FAKE_DIR/calls.log"; : > "$FAKE_DIR/claude.log"
  export ODONIAN_HOME="$TMP/home$SCN"
  mkdir -p "$ODONIAN_HOME"
}
SCN=0
tasks_json() { # project id:model:kind ...
  local proj="$1"; shift; local j="[" first=1 spec
  for spec in "$@"; do
    IFS=: read -r id model kind <<<"$spec"
    [ "$first" -eq 1 ] || j="$j,"; first=0
    j="$j{\"id\":\"$id\",\"model\":\"$model\",\"kind\":\"${kind:-implement}\",\"state\":\"ready\"}"
  done
  echo "$j]" > "$FAKE_DIR/tasks.$proj.json"
}
start_agent() { # [multi] -- env tuned for fast tests
  export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN=t ODONIAN_MAIN_REPO="$MAIN_REPO" ODONIAN_REPO="$MAIN_REPO"
  export ODONIAN_RENEW_INTERVAL_SECS=1 ODONIAN_RENEW_POLL_SECS=0.2 ODONIAN_FENCE_GRACE_SECS=2
  export ODONIAN_ADMISSION_RETRY_NAP=0 ODONIAN_AMBIGUOUS_RETRY_SECS=1
  if [ "${1:-}" = multi ]; then
    export ODONIAN_PROJECT=all
    for p in $(jq -r '.[] | "\(.id)|\(.repo)"' "$FAKE_DIR/projects.json"); do
      repo="${p#*|}"; slug="$(echo "$repo" | tr '/' '-')"
      [ -d "$ODONIAN_HOME/repos/$slug/.git" ] || { mkdir -p "$ODONIAN_HOME/repos"; git clone --quiet "$BARE" "$ODONIAN_HOME/repos/$slug" 2>/dev/null; }
    done
  else
    export ODONIAN_PROJECT=proj-test
  fi
  "$HARNESS_DIR/agent.sh" --model x --kind implement "slot-$SCN" > "$FAKE_DIR/agent.log" 2>&1 &
  AGENT_PID=$!
}
wait_for() { # timeout_secs cmd... (polls every 0.1s)
  local t="$1" i; shift
  for ((i = 0; i < t * 10; i++)); do "$@" && return 0; sleep 0.1; done
  return 1
}
calls() { grep "^[0-9.]* $1 " "$FAKE_DIR/calls.log"; }
ncalls() { if [ -n "${2:-}" ]; then grep -c "^[0-9.]* $1 $2\( \|\$\)" "$FAKE_DIR/calls.log"; else grep -c "^[0-9.]* $1\( \|\$\)" "$FAKE_DIR/calls.log"; fi; }
ncalls_ge() { [ "$(ncalls "$1" "${3:+$2}")" -ge "${3:-$2}" ]; }
has_claude() { grep -q "$1" "$FAKE_DIR/claude.log"; }
claude_starts() { grep -c " START " "$FAKE_DIR/claude.log"; }
ts_of() { head -1 | awk '{print $1}'; }
dump() { echo "    --- agent.log"; sed 's/^/    /' "$FAKE_DIR/agent.log" | tail -25; echo "    --- calls.log"; sed 's/^/    /' "$FAKE_DIR/calls.log" | tail -25; echo "    --- claude.log"; sed 's/^/    /' "$FAKE_DIR/claude.log"; }
check() { # description, cmd...
  local d="$1"; shift
  if "$@"; then pass "$d"; else fail "$d"; FAILED_SCN=1; fi
}
end_scenario() { stop_agent; [ "${FAILED_SCN:-0}" -eq 1 ] && dump; FAILED_SCN=0; }
lt() { awk -v a="$1" -v b="$2" 'BEGIN{exit !(a<b)}'; }

# =============================== 1. deferral ===============================
echo "Scenario 1: deferral invokes no model, honors retry-after, does not re-ask in a tight loop, then admits once"
new_scenario
tasks_json proj-test A:x
printf 'defer:3\ngrant\n' > "$FAKE_DIR/claim.A"
echo "sleep:0" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 5 ncalls_ge claim A 1
sleep 2
check "zero model invocations while deferred" test "$(claude_starts)" -eq 0
check "no tight re-claim loop during the 3s retry window (1 claim after 2s)" test "$(ncalls claim A)" -eq 1
check "task state untouched: no transition call" test "$(ncalls transition)" -eq 0
wait_for 8 has_claude " START "
sleep 1
check "after the window the task is admitted and the model launches exactly once" test "$(claude_starts)" -eq 1
check "exactly two claim requests (deferred, then granted) — no double launch" test "$(ncalls claim A)" -eq 2
r1="$(calls claim | sed -n '1p' | sed 's/.*req=\([^ ]*\).*/\1/')"; r2="$(calls claim | sed -n '2p' | sed 's/.*req=\([^ ]*\).*/\1/')"
check "the retry after a definite denial is a new admission request id" test -n "$r1" -a "$r1" != "$r2"
end_scenario

# ======================= 2. not-before hint and skip to another task =======================
echo "Scenario 2: a denied head task is skipped; the next eligible task is admitted and launched at once"
new_scenario
tasks_json proj-test A:x B:x
printf 'notbefore:60\n' > "$FAKE_DIR/claim.A"
printf 'grant\n' > "$FAKE_DIR/claim.B"
echo "sleep:0" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 8 has_claude "task=B"
sleep 1
check "model launched for B, the other eligible task" has_claude "START model=x task=B"
check "A was asked about exactly once (not-before: 60s honored, not re-asked)" test "$(ncalls claim A)" -eq 1
check "no model was ever launched for the deferred task A" test "$(grep -c 'task=A' "$FAKE_DIR/claude.log")" -eq 0
end_scenario

# ============== 3. MULTI loop: continue to another project while one is deferred ==============
echo "Scenario 3: MULTI loop skips a deferred project's task and works another project"
new_scenario
echo '[{"id":"p1","repo":"own/one"},{"id":"p2","repo":"own/two"}]' > "$FAKE_DIR/projects.json"
tasks_json p1 A:x; tasks_json p2 B:x
printf 'defer:60\n' > "$FAKE_DIR/claim.A"; printf 'grant\n' > "$FAKE_DIR/claim.B"
echo "sleep:0" > "$FAKE_DIR/claude.mode"
start_agent multi
wait_for 20 has_claude "task=B"
sleep 1
check "project 2's task B was launched despite project 1's deferral" has_claude "START model=x task=B"
check "no model launched for the deferred project's task" test "$(grep -c 'task=A' "$FAKE_DIR/claude.log")" -eq 0
check "deferred task A not re-asked inside its retry window" test "$(ncalls claim A)" -le 1
end_scenario

# =============================== 4. granted: contract, renewal, finalize ===============================
echo "Scenario 4: granted admission passes the preclaimed contract, renews while alive, finalizes after exit"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"; echo "sleep:3" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 12 has_claude " END "
sleep 1
check "child got ODONIAN_PRECLAIMED_TASK_ID and ODONIAN_PRECLAIMED_ATTEMPT_ID from the claim" has_claude "START model=x task=A attempt=attempt-A-1"
check "exactly one launch" test "$(claude_starts)" -eq 1
check "task lease heartbeats are fenced with the admitted attempt" grep -q "heartbeat A attempt=attempt-A-1" "$FAKE_DIR/calls.log"
check "permit renewed with the full identity flags" grep -q "permit-renew permit-A task=A model=x agent=.* req=.* attempt=attempt-A-1" "$FAKE_DIR/calls.log"
end_t="$(grep ' END ' "$FAKE_DIR/claude.log" | ts_of)"
renew_before="$(calls permit-renew | awk -v e="$end_t" '$1 < e' | wc -l)"
check "permit renewed at least twice BEFORE the process exited" test "$renew_before" -ge 2
check "permit finalized exactly once, class completed, with identity flags" test "$(calls permit-finalize | grep -c 'permit-A task=A model=x agent=.* req=.* attempt=attempt-A-1 class=completed')" -eq 1
fin_t="$(calls permit-finalize | ts_of)"
check "finalization happened only after the process exit" lt "$end_t" "$fin_t"
check "request id on renew/finalize equals the admission request id" test "$(calls claim | sed 's/.*req=\([^ ]*\).*/\1/')" = "$(calls permit-finalize | sed 's/.* req=\([^ ]*\) .*/\1/')"
end_scenario

# ============== 5. model submits then keeps running: not killed, permit kept alive ==============
echo "Scenario 5: after the model submits, lease heartbeats stop but the process is not killed and the permit stays renewed"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"; echo "submit-sleep:4" > "$FAKE_DIR/claude.mode"; echo conflict > "$FAKE_DIR/hb.mode"
start_agent
wait_for 14 has_claude " END "
sleep 1
sub_t="$(grep SUBMITTED "$FAKE_DIR/claude.log" | ts_of)"; end_t="$(grep ' END ' "$FAKE_DIR/claude.log" | ts_of)"
check "the model ran to completion after submitting (not terminated)" bash -c "grep -q ' END rc=0' '$FAKE_DIR/claude.log' && ! grep -q TERM '$FAKE_DIR/claude.log'"
check "task heartbeats stopped once the task left in_progress (at most 2 attempts)" test "$(ncalls heartbeat)" -le 2
check "permit renewal continued after submission, until exit (>=2 renewals between submit and exit)" test "$(calls permit-renew | awk -v s="$sub_t" -v e="$end_t" '$1 > s && $1 < e' | wc -l)" -ge 2
check "permit finalized completed only after exit" test "$(calls permit-finalize | grep -c 'class=completed')" -eq 1
end_scenario

# ============== 6. lease/permit actually lost: fence, stop, clean up ==============
echo "Scenario 6: lost ownership fences and stops the stale process, then finalizes cancelled"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"; echo "sleep:30" > "$FAKE_DIR/claude.mode"; echo conflict > "$FAKE_DIR/renew.mode"
start_agent
wait_for 10 has_claude " TERM "
sleep 1
check "the stale process was terminated well before its 30s run" has_claude " TERM task=A"
check "permit finalized with exit class cancelled" test "$(calls permit-finalize | grep -c 'class=cancelled')" -eq 1
check "a fenced stop is not recorded as a model backend failure" bash -c "! grep -q 'backend unavailable' '$FAKE_DIR/agent.log'"
end_scenario

echo "Scenario 6b: heartbeat ATTEMPT_FENCED while the task is still in_progress for us also fences"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"; echo "sleep:30" > "$FAKE_DIR/claude.mode"; echo attempt-fenced > "$FAKE_DIR/hb.mode"
start_agent
wait_for 10 has_claude " TERM "
sleep 1
check "stale process stopped on ATTEMPT_FENCED" has_claude " TERM task=A"
check "permit finalized cancelled" test "$(calls permit-finalize | grep -c 'class=cancelled')" -eq 1
end_scenario

echo "Scenario 6c: task reassigned to another agent fences; a bare CONFLICT from submit does not"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"; echo "sleep:30" > "$FAKE_DIR/claude.mode"; echo conflict > "$FAKE_DIR/hb.mode"
echo '{"track":"research","state":"in_progress","assignee":"some-other-agent"}' > "$FAKE_DIR/show.A"
start_agent
wait_for 10 has_claude " TERM "
sleep 1
check "in_progress under a different agent => stale process stopped" has_claude " TERM task=A"
end_scenario

# =============================== 7. failure / launch error / shutdown ===============================
echo "Scenario 7: child failure is finalized failed and backs the model off (exit status preserved)"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"; echo "fail:7" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 10 ncalls_ge permit-finalize 1
sleep 0.5
check "finalized with exit class failed" test "$(calls permit-finalize | grep -c 'class=failed')" -eq 1
check "non-zero exit marks the model backend unavailable (rc preserved)" grep -q "dispatch exited rc=7" "$FAKE_DIR/agent.log"
end_scenario

echo "Scenario 8: launch error (model binary not executable) is finalized failed, no leaked permit"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"
mv "$BIN/claude" "$BIN/claude.real"; : > "$BIN/claude"; chmod -x "$BIN/claude"
start_agent
wait_for 10 ncalls_ge permit-finalize 1
sleep 0.5
check "launch error finalized with class failed" test "$(calls permit-finalize | grep -c 'class=failed')" -eq 1
check "launch error logged" grep -q "launch error" "$FAKE_DIR/agent.log"
end_scenario
rm -f "$BIN/claude"; mv "$BIN/claude.real" "$BIN/claude"

echo "Scenario 9: shutdown while a research process runs stops it and finalizes cancelled"
new_scenario
tasks_json proj-test A:x
echo grant > "$FAKE_DIR/claim.A"; echo "sleep:30" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 10 has_claude " START "
sleep 1
kill -TERM "$AGENT_PID"
wait_for 10 ncalls_ge permit-finalize 1
check "the in-flight model was stopped" has_claude " TERM task=A"
check "permit finalized cancelled on shutdown" test "$(calls permit-finalize | grep -c 'class=cancelled')" -eq 1
end_scenario

# =============================== 10. no grant / observe ===============================
echo "Scenario 10: claim succeeds without a permit (disabled/observe) — no orphaned claim, no permit calls"
new_scenario
tasks_json proj-test A:x
echo grant-bare > "$FAKE_DIR/claim.A"; echo "sleep:2" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 10 has_claude " END "
sleep 1
check "model launched for the claimed task with the preclaimed task id, no attempt" has_claude "START model=x task=A attempt=none"
check "no permit renew/finalize without a permit" test "$(ncalls permit-renew)$(ncalls permit-finalize)" = "00"
check "task lease still heartbeated while owned" test "$(ncalls heartbeat)" -ge 1
end_scenario

# =============================== 11. ambiguous admission ===============================
echo "Scenario 11: ambiguous responses are retried with the SAME request id (no double spend)"
new_scenario
tasks_json proj-test A:x
printf 'err\nerr\ngrant\n' > "$FAKE_DIR/claim.A"; echo "sleep:0" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 10 has_claude " START "
sleep 1
check "three claims, one launch" bash -c "[ $(ncalls claim A) -eq 3 ] && [ $(claude_starts) -eq 1 ]"
check "all three claims carried one request id" test "$(calls claim | sed 's/.*req=\([^ ]*\).*/\1/' | sort -u | wc -l)" -eq 1
end_scenario

echo "Scenario 11b: still ambiguous after retries — nothing launched; the next pass replays the same request id"
new_scenario
tasks_json proj-test A:x
printf 'err\nerr\nerr\ngrant\n' > "$FAKE_DIR/claim.A"; echo "sleep:0" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 10 ncalls_ge claim A 3
check "no model launched while ambiguous" test "$(claude_starts)" -eq 0
check "no permit finalize for an unknown admission (the start stays debited)" test "$(ncalls permit-finalize)" -eq 0
wait_for 15 has_claude " START "
sleep 1
check "the later replay used the original request id and launched once" bash -c "[ $(claude_starts) -eq 1 ] && [ \$(grep '^[0-9.]* claim ' '$FAKE_DIR/calls.log' | sed 's/.*req=\([^ ]*\).*/\1/' | sort -u | wc -l) -eq 1 ]"
end_scenario

echo "Scenario 11c: conflict (task busy) launches nothing and defers the task"
new_scenario
tasks_json proj-test A:x
printf 'conflict\n' > "$FAKE_DIR/claim.A"
start_agent
wait_for 5 ncalls_ge claim A 1
sleep 2
check "no model, single claim" bash -c "[ $(claude_starts) -eq 0 ] && [ $(ncalls claim A) -eq 1 ]"
end_scenario

# =============================== 12. build/design unchanged ===============================
echo "Scenario 12: build track dispatch is unchanged — no harness claim/permit, no preclaimed env, rc preserved"
new_scenario
tasks_json proj-test A:x
echo '{"track":"build","state":"ready"}' > "$FAKE_DIR/show.A"; echo "fail:5" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 10 has_claude " END "
sleep 1
check "launched with no preclaimed env" has_claude "START model=x task=none attempt=none"
check "harness made no claim and no permit/heartbeat calls" test "$(ncalls claim)$(ncalls permit-renew)$(ncalls permit-finalize)$(ncalls heartbeat)" = "0000"
check "failing build dispatch backs the model off (exit status preserved)" grep -q "dispatch exited rc=5" "$FAKE_DIR/agent.log"
end_scenario

echo "Scenario 12b: design track also bypasses admission"
new_scenario
tasks_json proj-test A:x
echo '{"track":"design","state":"ready"}' > "$FAKE_DIR/show.A"; echo "sleep:0" > "$FAKE_DIR/claude.mode"
start_agent
wait_for 10 has_claude " END "
sleep 1
check "design launched with no admission" bash -c "has() { grep -q 'START model=x task=none' '$FAKE_DIR/claude.log'; }; has && [ $(ncalls claim) -eq 0 ]"
end_scenario

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
