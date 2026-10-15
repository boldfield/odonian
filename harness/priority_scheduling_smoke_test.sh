#!/usr/bin/env bash
# priority_scheduling_smoke_test.sh — end-to-end smoke for numeric queue priority and server-generated
# Move-to-front, driven through the REAL server, CLI and fleet launcher.
#
# A freshly built `odonian server` (temp SQLite DB, random loopback port, research pacing in enforce mode)
# is exercised over its REST API and the real `odonian` CLI; the REAL harness/agent.sh (MULTI-project
# mode) selects and launches work, with a FAKE `claude` standing in for the model provider. Nothing
# here can reach a real model, subscription, GitHub or cluster: PATH is rebuilt from a fixed set of
# system tools plus the fakes, every service listens on 127.0.0.1, and all state lives under one temp
# dir. No expected value is computed by re-implementing the priority formula: every number below is a
# literal that the server/launcher output is compared to.
#
# Contract under test: P defaults to 500; external assignment accepts only 1..1000 (rejected, never
# clamped); server-generated Move-to-front = max(1000, max(P_queued)) + 1 over every outstanding
# non-archived topic on the server, so it is always above 1000; inherited/generated values above 1000
# are valid; ONE comparator — P descending, created_at ascending, task id ascending — at every value.
#
#   1. bounds and defaults        manual 1..1000 + default 500; 0/1001/-1/non-integer rejected, nothing clamped
#   2. Front values, restart      Front at queue max 500/730/1000/1042; owner regression (manual 505/1000 cannot
#                                 overtake Front, a later Front can); parallel Front + replay; exact persistence
#   3. P_queued and lineages      backlog/ready/held/dependency-blocked/blocked/in-flight/review all count; a
#                                 terminal root counts only through an active descendant; review/rework inherit
#                                 P > 1000; reset to 500
#   4. real launcher, build       agent.sh picks one global order across two projects (oldest first at equal P);
#                                 held / dependency-blocked high-priority work falls through; a live task is not
#                                 interrupted; one start and one claim per task, pinned selection never drifts
#   5. real launcher, research    quota-unavailable high-priority work falls back to another pool with no debit;
#                                 completion-reserved capacity unchanged under Front; review/rework inherit P > 1000
#
# NOT covered here (needs a forge): continuation-child inheritance through a real approved continuation. That path
# is covered by the store tests (internal/store/priority_test.go) and is listed as a rollout gap in
# docs/runbooks/priority-rollout.md.
#
# Usage: bash harness/priority_scheduling_smoke_test.sh   (needs go, jq, git, curl; SMOKE_VERBOSE=1 adds ids/ports)
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/.." && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
info() { [ -z "${SMOKE_VERBOSE:-}" ] || echo "   $*"; }
die() { echo "FATAL: $*" >&2; kill -TERM $$ 2>/dev/null; exit 2; }
expect_eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (expected '$2', got '$3')"; fi; }
check() { local description="$1"; shift; if "$@"; then pass "$description"; else fail "$description"; fi; }

for tool in go jq git curl; do command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }; done

# A developer's own fleet environment must never leak into the fakes or the launcher.
unset ODONIAN_PROJECT ODONIAN_PROJECTS ODONIAN_REPO ODONIAN_MAIN_REPO ODONIAN_MODEL ODONIAN_PRECLAIMED_TASK_ID \
      ODONIAN_PRECLAIMED_ATTEMPT_ID ODONIAN_SELECTED_TASK_ID ODONIAN_DELIVERY_MODE ODONIAN_WORKTREE_HOME \
      AGENT_MODEL AGENT_SLOT AGENT_CODEX_MODELS AGENT_CODEX_FLAGS AGENT_CLAUDE_FLAGS FORGE_TOKENS GH_TOKEN

SERVER_PID=""
stop_server() {
  if [ -n "$SERVER_PID" ]; then
    kill -TERM "$SERVER_PID" 2>/dev/null
    for _ in $(seq 1 100); do kill -0 "$SERVER_PID" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$SERVER_PID" 2>/dev/null
    wait "$SERVER_PID" 2>/dev/null
    SERVER_PID=""
  fi
}
stop_agent() {
  local f="$TMP/pid.$1" pid
  [ -f "$f" ] || return 0
  pid="$(cat "$f")"; rm -f "$f"
  kill -TERM "$pid" 2>/dev/null
  for _ in $(seq 1 150); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
  kill -KILL "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
}
stop_all_agents() { local f; for f in "$TMP"/pid.*; do [ -e "$f" ] && stop_agent "${f##*/pid.}"; done; }
cleanup() { stop_all_agents; stop_server; rm -rf "$TMP"; }
trap cleanup EXIT

# --- always build the real binary fresh into the temp dir; sanitized PATH: system tools + fakes only ---
BIN="$TMP/bin"; SYSBIN="$TMP/sysbin"; mkdir -p "$BIN" "$SYSBIN"
(cd "$REPO_ROOT" && go build -o "$BIN/odonian" ./cmd/odonian) || die "go build failed"
for tool in bash env jq git curl date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep paste readlink tail uname ln mkdir chmod mv cp ls find tee sh; do
  p="$(command -v "$tool" 2>/dev/null)"; [ -n "$p" ] && [ "${p#/}" != "$p" ] && ln -sf "$p" "$SYSBIN/$tool"
done
export PATH="$BIN:$SYSBIN"
export HOME="$TMP/home"; mkdir -p "$HOME"
export GIT_CONFIG_GLOBAL="$TMP/gitconfig"; : > "$GIT_CONFIG_GLOBAL"
git config --global user.email smoke@example.invalid; git config --global user.name smoke
git config --global init.defaultBranch main

# --- one tiny local repo stands in for every project's repo (never a network remote) ---
BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
SEED="$TMP/seed"; git clone --quiet "$BARE" "$SEED" 2>/dev/null
git -C "$SEED" commit --quiet --allow-empty -m init
git -C "$SEED" push --quiet origin HEAD:main 2>/dev/null
REPO_SLUG="$(echo "$BARE" | sed -E 's#^(https://|git@)github\.com[:/]##; s#\.git$##' | tr '/' '-')"   # agent.sh's repo_slug

# --- the fake model provider ---
FAKE="$TMP/fake"; mkdir -p "$FAKE"; : > "$FAKE/starts.log"; : > "$FAKE/exits.log"
cat > "$BIN/claude" <<'EOF'
#!/usr/bin/env bash
# fake `claude -p`: records that a model process started and which task it owns, does the minimal board
# work the real prompt does, then either exits or (when a hold file names its task) stays running until
# released. It never calls any model.
trap 'echo "$(date +%s.%N) exit task=${task:-none} signal" >> "$FAKE_DIR/exits.log"; exit 143' TERM INT
task="${ODONIAN_PRECLAIMED_TASK_ID:-}"
pre=1
if [ -z "$task" ]; then   # legacy (build/review) flow: the model claims for itself, pinned to the launcher's selection
  pre=0
  task="$(odonian next --project "$ODONIAN_PROJECT" --model "$AGENT_MODEL" --kind "$FAKE_KIND" --claim 2>/dev/null)"
fi
echo "$(date +%s.%N) start task=${task:-none} selected=${ODONIAN_SELECTED_TASK_ID:-} model=$AGENT_MODEL preclaimed=$pre project=${ODONIAN_PROJECT:-}" >> "$FAKE_DIR/starts.log"
[ -n "$task" ] || exit 0
kind="$(odonian show "$task" --json 2>/dev/null | jq -r '.kind // empty')"
if [ "$pre" = 1 ] && [ "$kind" = implement ]; then   # a research writer ends its ownership by submitting
  odonian submit "$task" --attempt "${ODONIAN_PRECLAIMED_ATTEMPT_ID:-}" --result "fake provider output" --no-op >/dev/null 2>&1
fi
if [ -e "$FAKE_DIR/hold.$task" ] || [ -e "$FAKE_DIR/hold.all" ]; then
  for _ in $(seq 1 1800); do
    { [ -e "$FAKE_DIR/release.$task" ] || [ -e "$FAKE_DIR/release.all" ]; } && break
    sleep 0.1
  done
fi
echo "$(date +%s.%N) exit task=$task released" >> "$FAKE_DIR/exits.log"
exit 0
EOF
chmod +x "$BIN/claude"
export FAKE_DIR="$FAKE"

# --- server lifecycle: every world gets its own DB; the port is random and checked free ---
export ODONIAN_TOKEN="smoke-token" ODONIAN_STATE_DIR="$TMP/cli-state" AGENT_ID="smoke-operator"
export ODONIAN_MODELS="opus,fable,sonnet,haiku" ODONIAN_LEASE_TTL=30m
export ODONIAN_RESEARCH_POLICY_MODE="enforce"
# Illustrative test pools (not production recommendations). alpha: a single start of burst and a negligible
# refill, so one admitted start exhausts it ("quota unavailable"). beta: a roomy burst, two dispatch slots,
# one reserved for completion work (review/rework), refill too slow to blur the debit count.
export ODONIAN_RESEARCH_POOLS='{
  "alpha":{"account_id":"acct-alpha","models":["opus","haiku"],"start_rate":0.001,"burst_capacity":1,"concurrent_dispatch_limit":2,"completion_reserved":0},
  "beta":{"account_id":"acct-beta","models":["sonnet","fable"],"start_rate":0.001,"burst_capacity":10,"concurrent_dispatch_limit":2,"completion_reserved":1}}'
export ODONIAN_RENEW_INTERVAL_SECS=2 ODONIAN_RENEW_POLL_SECS=1 ODONIAN_AMBIGUOUS_RETRY_SECS=2 ODONIAN_ADMISSION_RETRY_NAP=1

start_server() { # (re)start the server on the current world's DB and port
  local attempt
  for attempt in 1 2 3 4 5; do
    if [ -z "${PORT:-}" ]; then
      for _ in $(seq 1 50); do
        PORT=$((20000 + RANDOM % 20000))
        curl -s --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 || break
      done
    fi
    export ODONIAN_URL="http://127.0.0.1:$PORT" ODONIAN_ADDR="127.0.0.1:$PORT"
    "$BIN/odonian" server >>"$TMP/server.log" 2>&1 &
    SERVER_PID=$!
    for _ in $(seq 1 100); do
      curl -fsS --max-time 1 "$ODONIAN_URL/healthz" >/dev/null 2>&1 && return 0
      kill -0 "$SERVER_PID" 2>/dev/null || break
      sleep 0.1
    done
    stop_server; PORT=""   # port taken or server died: pick another port
  done
  cat "$TMP/server.log" >&2; die "server did not become healthy"
}
restart_server() { stop_server; start_server; }

# --- board helpers (every helper fails loudly: no swallowed curl/HTTP errors) ---
api() { curl -fsS --max-time 15 -H "Authorization: Bearer $ODONIAN_TOKEN" -H 'Content-Type: application/json' "$@"; }
api_code() { # api_code METHOD PATH [BODY]: prints the HTTP status; the response body lands in $TMP/body.json
  curl -sS --max-time 15 -o "$TMP/body.json" -w '%{http_code}' -X "$1" -H "Authorization: Bearer $ODONIAN_TOKEN" \
    -H 'Content-Type: application/json' ${3:+-d "$3"} "$ODONIAN_URL$2"
}
error_code() { jq -r '.error.code // empty' "$TMP/body.json"; }
mk_project() { api -d "{\"name\":\"$1\",\"repo\":\"$BARE\"}" "$ODONIAN_URL/projects" | jq -er .id || die "create project $1"; }
mk_doc() { api -d '{"title":"smoke","kind":"design","content":"x"}' "$ODONIAN_URL/projects/$1/documents" | jq -er .id || die "create document"; }
new_task() { # new_task <project> <doc> <title> <track> <model> [extra JSON fields, e.g. ,"priority":730] -> backlog task id
  local project="$1" doc="$2" title="$3" track="$4" model="$5" extra="${6:-}"
  api -d "[{\"title\":\"$title\",\"spec\":\"smoke task\",\"document_id\":\"$doc\",\"model\":\"$model\",\"review_models\":[\"fable\"],\"track\":\"$track\"$extra}]" \
    "$ODONIAN_URL/projects/$project/tasks" | jq -er '.[0].id' || die "create task $title"
}
ready_task() { local id; id="$(new_task "$@")" || exit 2; odonian promote "$id" >/dev/null || die "promote $id"; echo "$id"; }
action_key() { echo "smoke-$$-$(date +%s%N)-$RANDOM"; }
front() { odonian priority "$1" --front --reason smoke --action-key "${2:-$(action_key)}" --json 2>/dev/null; }
setp() { odonian priority "$1" --set "$2" --reason smoke --action-key "${3:-$(action_key)}" --json 2>/dev/null; }
resetp() { odonian priority "$1" --reset --reason smoke --action-key "${2:-$(action_key)}" --json 2>/dev/null; }
change_of() { jq -r '"\(.priority)/\(.queue_max_priority)"'; }   # "<new priority>/<queue max the server saw>"
task_json() { odonian show "$1" --json; }
p_of() { task_json "$1" | jq -r '.priority'; }
task_state() { task_json "$1" | jq -r '.state'; }
task_assignee() { task_json "$1" | jq -r '.assignee // ""'; }
task_round() { task_json "$1" | jq -r '.review_round'; }
review_of() { # review_of <project> <task>: id of the review task targeting <task>
  local id
  for id in $(odonian tasks --project "$1" --json | jq -r '.[] | select(.kind=="review") | .id'); do
    [ "$(task_json "$id" | jq -r '.target_task_id // empty')" = "$2" ] && { echo "$id"; return 0; }
  done
  return 1
}
claim_pinned() { # claim_pinned <project> <model> <kind> <task>: the launcher's pinned `next --claim`
  ODONIAN_SELECTED_TASK_ID="$4" odonian next --project "$1" --model "$2" --kind "$3" --claim 2>/dev/null
}
pool_field() { odonian research-status --json | jq -r --arg a "$1" ".pools[] | select(.account_id==\$a) | .$2"; }
starts() { grep -c "task=$1 " "$FAKE/starts.log" 2>/dev/null || true; }
total_starts() { wc -l < "$FAKE/starts.log" | tr -d ' '; }
wait_for() { local t="$1" end; shift; end=$(( $(date +%s) + t )); while :; do "$@" && return 0; [ "$(date +%s)" -ge "$end" ] && return 1; sleep 0.3; done; }
agent_log_has() { grep -q "$2" "$TMP/agent-$1.log" 2>/dev/null; }
wait_expr() { local t="$1"; shift; wait_for "$t" eval "$@"; }
float_le() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a <= b) }'; }
float_ge() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a >= b) }'; }
start_agent() { # start_agent <name> <model> <kind>: the real agent.sh in MULTI-project mode (all projects)
  local name="$1" model="$2" kind="$3" home="$TMP/home-$1"
  mkdir -p "$home/repos"
  [ -d "$home/repos/$REPO_SLUG/.git" ] || git clone --quiet "$BARE" "$home/repos/$REPO_SLUG" 2>/dev/null || die "clone for $name"
  ( export ODONIAN_HOME="$home" ODONIAN_PROJECT=all FAKE_KIND="$kind"
    exec bash "$HARNESS_DIR/agent.sh" --model "$model" --kind "$kind" "$name" ) >"$TMP/agent-$name.log" 2>&1 &
  echo $! > "$TMP/pid.$name"
}
WORLD=0
new_world() { # a fresh DB, server, two projects and one design document each; clears the fake provider logs
  WORLD=$((WORLD + 1))
  stop_all_agents; stop_server
  export ODONIAN_DB="$TMP/world-$WORLD.db"
  rm -f "$FAKE"/hold.* "$FAKE"/release.*; : > "$FAKE/starts.log"; : > "$FAKE/exits.log"
  start_server
  PA="$(mk_project proj-a)"; PB="$(mk_project proj-b)"
  DA="$(mk_doc "$PA")"; DB="$(mk_doc "$PB")"
  info "world $WORLD: server $ODONIAN_URL  projects ${PA:0:8} ${PB:0:8}"
}
snapshot() { local p; for p in "$PA" "$PB"; do odonian tasks --project "$p" --json | jq -r '.[] | "\(.id) \(.priority) \(.state)"'; done | sort; }

# ============================================================================================
echo "== 1. bounds and defaults: manual entry is 1..1000 (default 500); anything else is rejected, never clamped"
new_world
T="$(new_task "$PA" "$DA" default build opus)"
expect_eq "a task created without a priority is stored at the default 500" 500 "$(p_of "$T")"
LOW="$(new_task "$PA" "$DA" low build opus ',"priority":1')"; HIGH="$(new_task "$PA" "$DA" high build opus ',"priority":1000')"
expect_eq "creating with priority 1 (the lower bound) is accepted and stored as 1" 1 "$(p_of "$LOW")"
expect_eq "creating with priority 1000 (the upper bound) is accepted and stored as 1000" 1000 "$(p_of "$HIGH")"
TASKS_BEFORE="$(odonian tasks --project "$PA" --json | jq length)"
for bad in 0 1001 -1 500.5 '"high"'; do
  code="$(api_code POST "/projects/$PA/tasks" "[{\"title\":\"bad\",\"spec\":\"s\",\"document_id\":\"$DA\",\"model\":\"opus\",\"priority\":$bad}]")"
  expect_eq "creating with priority $bad is rejected with HTTP 400" 400 "$code"
done
expect_eq "no rejected create left a task behind" "$TASKS_BEFORE" "$(odonian tasks --project "$PA" --json | jq length)"
SETBODY() { echo "{\"action_key\":\"$(action_key)\",\"priority\":$1,\"actor\":\"smoke\",\"reason\":\"smoke\"}"; }
expect_eq "server rejects a fractional manual priority (HTTP 400)" 400 "$(api_code POST "/tasks/$T/priority/set" "$(SETBODY 500.5)")"
for bad in 0 1001 -1 100000; do
  code="$(api_code POST "/tasks/$T/priority/set" "$(SETBODY "$bad")")"
  expect_eq "server rejects a manual priority of $bad (HTTP 400 INVALID_PRIORITY), not clamped" "400/INVALID_PRIORITY" "$code/$(error_code)"
done
expect_eq "a manual priority without a value is rejected" 400 "$(api_code POST "/tasks/$T/priority/set" "{\"action_key\":\"$(action_key)\",\"actor\":\"smoke\",\"reason\":\"smoke\"}")"
expect_eq "a priority change without a reason is rejected" 400 "$(api_code POST "/tasks/$T/priority/set" "{\"action_key\":\"$(action_key)\",\"priority\":600,\"actor\":\"smoke\"}")"
expect_eq "after every rejection the stored priority is still the default 500" 500 "$(p_of "$T")"
check "the CLI also refuses 0 and 1001 before contacting the server" \
  bash -c '! odonian priority "$0" --set 0 --reason smoke >/dev/null 2>&1 && ! odonian priority "$0" --set 1001 --reason smoke >/dev/null 2>&1' "$T"
expect_eq "manual 1 is accepted and stored" 1 "$(setp "$T" 1 | jq -r .priority)"
expect_eq "manual 1000 is accepted and stored" 1000 "$(setp "$T" 1000 | jq -r .priority)"
expect_eq "reset returns the topic to 500" "1000/500" "$(resetp "$T" | jq -r '"\(.old_priority)/\(.priority)"')"
expect_eq "the reset value is what the board reads back" 500 "$(p_of "$T")"

# ============================================================================================
echo "== 2. Front values, owner regression, parallel Front + replay, exact persistence across restart"
new_world
declare -a TA TB
for i in 1 2 3 4 5 6; do TA[$i]="$(ready_task "$PA" "$DA" "a$i" build opus)"; TB[$i]="$(ready_task "$PB" "$DB" "b$i" build opus)"; done
BACKLOG_ONLY="$(new_task "$PA" "$DA" backlog-only build opus)"
expect_eq "Front with the queue maximum at 500 gives 1001 (backlog work is counted too)" "1001/500" "$(front "${TA[1]}" | change_of)"
check "Front on a topic is topic-wide and readable back" test "$(p_of "${TA[1]}")" = 1001
echo "  -- owner regression: manual 505/1000 created later cannot overtake Front; a later Front can"
expect_eq "manual 505 on a newer task in the other project" 505 "$(setp "${TB[2]}" 505 | jq -r .priority)"
expect_eq "manual 1000 on a newer task in the first project" 1000 "$(setp "${TA[3]}" 1000 | jq -r .priority)"
expect_eq "the server's next task in project A is still the fronted one (1001 beats 1000)" "${TA[1]}" "$(odonian next --project "$PA" --model opus --kind implement)"
expect_eq "the next task in project B is the manual 505 one (above its 500 peers)" "${TB[2]}" "$(odonian next --project "$PB" --model opus --kind implement)"
expect_eq "a later Front sees the queue maximum 1001 and gives 1002" "1002/1001" "$(front "${TB[4]}" | change_of)"
expect_eq "a Front on the 1000 task gives 1003 (its own value counts in the maximum 1002)" "1003/1002" "$(front "${TA[3]}" | change_of)"
expect_eq "the later Front overtakes the earlier Front within project A" "${TA[3]}" "$(odonian next --project "$PA" --model opus --kind implement)"
expect_eq "and the project B Front outranks its manual 505" "${TB[4]}" "$(odonian next --project "$PB" --model opus --kind implement)"
echo "  -- Front below, at and above 1000"
for id in "${TA[1]}" "${TA[3]}" "${TB[2]}" "${TB[4]}"; do resetp "$id" >/dev/null; done
expect_eq "everything reset: Front at queue maximum 500 gives 1001" "1001/500" "$(front "${TA[5]}" | change_of)"
resetp "${TA[5]}" >/dev/null
setp "${TA[6]}" 730 >/dev/null
expect_eq "Front at queue maximum 730 (below 1000) gives 1001" "1001/730" "$(front "${TB[6]}" | change_of)"
resetp "${TB[6]}" >/dev/null; resetp "${TA[6]}" >/dev/null
setp "${TA[6]}" 1000 >/dev/null
expect_eq "Front at queue maximum 1000 (at the manual bound) gives 1001" "1001/1000" "$(front "${TB[6]}" | change_of)"
expect_eq "Front at queue maximum 1001 (above 1000) gives 1002" "1002/1001" "$(front "${TB[5]}" | change_of)"
FILL="$(api -d "[$(for i in $(seq 1 40); do printf '{"title":"fill%s","spec":"s","document_id":"%s","model":"opus"},' "$i" "$DA"; done | sed 's/,$//')]" "$ODONIAN_URL/projects/$PA/tasks" | jq -r '.[].id')"
EXPECT=1002; fill_ok=1
for id in $FILL; do
  EXPECT=$((EXPECT + 1))
  got="$(front "$id" | change_of)"
  [ "$got" = "$EXPECT/$((EXPECT - 1))" ] || { fill_ok=0; fail "40 consecutive Fronts: task expected $EXPECT/$((EXPECT - 1)), got $got"; }
done
[ "$fill_ok" = 1 ] && pass "40 consecutive Fronts each take queue maximum + 1 (1003 .. 1042)"
expect_eq "Front at queue maximum 1042 gives 1043" "1043/1042" "$(front "${TA[2]}" | change_of)"
echo "  -- concurrent Front serializes; replay is idempotent"
declare -a CONC
for i in 1 2 3 4 5 6 7 8; do CONC[$i]="$(new_task "$PB" "$DB" "conc$i" build opus)"; done
pids=()
for i in 1 2 3 4 5 6 7 8; do ( front "${CONC[$i]}" > "$TMP/par.$i" ) & pids+=($!); done
for pid in "${pids[@]}"; do wait "$pid"; done
expect_eq "8 parallel Fronts on 8 tasks produce 8 distinct consecutive values (no two share a maximum)" \
  "1044,1045,1046,1047,1048,1049,1050,1051" "$(for i in 1 2 3 4 5 6 7 8; do jq -r .priority "$TMP/par.$i"; done | sort -n | paste -sd, -)"
REPLAY_TASK="$(new_task "$PB" "$DB" replay build opus)"; REPLAY_KEY="smoke-replay-key-$$"
pids=()
for i in 1 2 3 4 5 6; do ( front "$REPLAY_TASK" "$REPLAY_KEY" > "$TMP/rep.$i" ) & pids+=($!); done
for pid in "${pids[@]}"; do wait "$pid"; done
expect_eq "6 parallel requests with one action key: exactly one applied, five replayed" "1/5" \
  "$(cat "$TMP"/rep.* | jq -r 'select(.replayed==false)' | jq -s length)/$(cat "$TMP"/rep.* | jq -r 'select(.replayed==true)' | jq -s length)"
expect_eq "every replay carries the original result (1052)" "1052" "$(cat "$TMP"/rep.* | jq -r .priority | sort -u | paste -sd, -)"
expect_eq "the replays did not move the queue: the next Front gets 1053" "1053/1052" "$(front "$(new_task "$PB" "$DB" after-replay build opus)" | change_of)"
expect_eq "reusing that key for a different task is a 409 IDEMPOTENCY_MISMATCH" "409/IDEMPOTENCY_MISMATCH" \
  "$(api_code POST "/tasks/${TA[4]}/priority/front" "{\"action_key\":\"$REPLAY_KEY\",\"actor\":\"smoke\",\"reason\":\"smoke\"}")/$(error_code)"
SET_KEY="smoke-set-key-$$"; setp "${TA[4]}" 700 "$SET_KEY" >/dev/null; setp "${TA[4]}" 800 >/dev/null
expect_eq "replaying an old Set key returns its original 700 result" "700/true" "$(setp "${TA[4]}" 700 "$SET_KEY" | jq -r '"\(.priority)/\(.replayed)"')"
expect_eq "and does not re-apply it (the topic is still at 800)" 800 "$(p_of "${TA[4]}")"
echo "  -- exact persistence across a real server restart"
SNAP_BEFORE="$(snapshot)"
restart_server
SNAP_AFTER="$(snapshot)"
expect_eq "every task's priority and state is identical after the restart ($(echo "$SNAP_BEFORE" | wc -l | tr -d ' ') tasks)" "$SNAP_BEFORE" "$SNAP_AFTER"
expect_eq "spot checks survive: 1043, 1000 and 800" "1043,1000,800" "$(p_of "${TA[2]}"),$(p_of "${TA[6]}"),$(p_of "${TA[4]}")"
expect_eq "a pre-restart Front key still replays to the original value" "1052/true" "$(front "$REPLAY_TASK" "$REPLAY_KEY" | jq -r '"\(.priority)/\(.replayed)"')"
expect_eq "Front after the restart continues from the persisted maximum (1053 -> 1054)" "1054/1053" "$(front "$(new_task "$PA" "$DA" after-restart build opus)" | change_of)"
expect_eq "the backlog-only task is unchanged and still at the default" 500 "$(p_of "$BACKLOG_ONLY")"

# ============================================================================================
echo "== 3. P_queued spans every outstanding state; terminal roots count only via active descendants; lineage inherits"
new_world
probe_state() { # probe_state <label> <task>: Front it (queue max 500 -> 1001), then show a fresh probe still sees it
  local label="$1" task="$2" probe
  expect_eq "$label: Front on an otherwise-500 queue gives 1001" "1001/500" "$(front "$task" | change_of)"
  probe="$(new_task "$PA" "$DA" "probe-$label" build haiku)"
  expect_eq "$label: the topic counts in P_queued (a probe Front sees 1001 and gives 1002)" "1002/1001" "$(front "$probe" | change_of)"
  resetp "$task" >/dev/null; resetp "$probe" >/dev/null
}
S="$(new_task "$PA" "$DA" st-backlog build haiku)";                       probe_state backlog "$S"
S="$(ready_task "$PA" "$DA" st-ready build haiku)";                        probe_state ready "$S"
S="$(ready_task "$PA" "$DA" st-held build haiku)"; api -X POST "$ODONIAN_URL/tasks/$S/hold" >/dev/null || die hold
probe_state "held (not claimable)" "$S"
BASE="$(ready_task "$PA" "$DA" st-dep-base build haiku)"
S="$(ready_task "$PA" "$DA" st-dep build haiku ",\"depends_on\":[\"$BASE\"]")"
check "the dependent task is not claimable while its dependency is unfinished" test "$(odonian tasks --project "$PA" --claimable --json | jq -r --arg id "$S" '[.[]|select(.id==$id)]|length')" = 0
probe_state "dependency-blocked" "$S"
S="$(ready_task "$PA" "$DA" st-blocked build haiku)"; odonian transition "$S" --to blocked --note smoke >/dev/null || die transition
probe_state blocked "$S"
S="$(ready_task "$PA" "$DA" st-inflight build haiku)"
expect_eq "the in-flight task is claimed through the pinned next" "$S" "$(claim_pinned "$PA" haiku implement "$S")"
OWNER_BEFORE="$(task_json "$S" | jq -c '[.state,.assignee,.lease_expires_at]')"
expect_eq "in-flight: Front on a claimed task gives 1001" "1001/500" "$(front "$S" | change_of)"
expect_eq "in-flight: Front did not touch the claim (same state, assignee and lease)" "$OWNER_BEFORE" "$(task_json "$S" | jq -c '[.state,.assignee,.lease_expires_at]')"
probe="$(new_task "$PA" "$DA" probe-inflight build haiku)"
expect_eq "in-flight: the claimed topic counts in P_queued" "1002/1001" "$(front "$probe" | change_of)"
echo "  -- lineage: a FUTURE review inherits the topic value; an EXISTING review is rewritten with it; rework keeps it"
odonian submit "$S" --result "fake provider output" --no-op >/dev/null || die "submit $S"
REV="$(review_of "$PA" "$S")" || die "no review task for $S"
expect_eq "future review: the review spawned after Front inherits 1001" 1001 "$(p_of "$REV")"
expect_eq "review state: the topic (implement in review + review ready) still counts" "1003/1002" "$(front "$(new_task "$PA" "$DA" probe-review build haiku)" | change_of)"
resetp "$S" >/dev/null; resetp "$probe" >/dev/null
for id in $(odonian tasks --project "$PA" --json | jq -r '.[] | select(.priority > 1000) | .id'); do resetp "$id" >/dev/null; done
expect_eq "reset to 500 is topic-wide: the review follows the implement task" "500/500" "$(p_of "$S")/$(p_of "$REV")"
EX="$(ready_task "$PA" "$DA" st-existing-review build haiku)"
claim_pinned "$PA" haiku implement "$EX" >/dev/null; odonian submit "$EX" --result "fake provider output" --no-op >/dev/null || die "submit $EX"
EXREV="$(review_of "$PA" "$EX")" || die "no review for $EX"
expect_eq "an existing review starts at the default 500" 500 "$(p_of "$EXREV")"
expect_eq "Front on the reviewed task" "1001/500" "$(front "$EX" | change_of)"
expect_eq "the existing review now carries 1001 (generated value, above the manual bound)" 1001 "$(p_of "$EXREV")"
check "the review is claimable at that priority and a reject sends the task back to rework" \
  test "$(claim_pinned "$PA" fable review "$EXREV")" = "$EXREV"
printf '%s' '[{"id":"f1","severity":"P2","file":"a.md","line":1,"summary":"smoke finding","in_changed_text":true,"status":"new"}]' > "$TMP/findings.json"
odonian submit "$EXREV" --verdict reject --findings-file "$TMP/findings.json" --result "smoke rejection" >/dev/null || die "reject review"
expect_eq "rework: the rejected task is ready again in review round 1 and keeps 1001" "ready/1/1001" "$(task_state "$EX")/$(task_round "$EX")/$(p_of "$EX")"
resetp "$EX" >/dev/null
expect_eq "reset to 500 on the reworked topic" 500 "$(p_of "$EX")"
echo "  -- terminal roots count only through an active descendant"
SUP="$(new_task "$PA" "$DA" st-superseded build haiku)"
expect_eq "Front on the task about to be superseded" "1001/500" "$(front "$SUP" | change_of)"
REPL="$(api -X POST -d '{}' "$ODONIAN_URL/tasks/$SUP/supersede" | jq -er .id)" || die supersede
expect_eq "the superseding replacement inherits 1001" 1001 "$(p_of "$REPL")"
expect_eq "the original is now terminal" superseded "$(task_state "$SUP")"
expect_eq "the terminal root counts through its active replacement (probe sees 1001)" "1002/1001" "$(front "$(new_task "$PA" "$DA" probe-terminal build haiku)" | change_of)"
expect_eq "archiving the replacement is accepted" 200 "$(api_code POST "/tasks/$REPL/archive")"
expect_eq "a terminal root with no active descendant no longer counts: only the 1002 probe remains, so Front sees 1002" "1003/1002" \
  "$(front "$(new_task "$PA" "$DA" probe-after-archive build haiku)" | change_of)"
expect_eq "a wholly finished topic cannot be moved to the front (409 TOPIC_NOT_OUTSTANDING)" "409/TOPIC_NOT_OUTSTANDING" \
  "$(api_code POST "/tasks/$SUP/priority/front" "{\"action_key\":\"$(action_key)\",\"actor\":\"smoke\",\"reason\":\"smoke\"}")/$(error_code)"
for id in $(odonian tasks --project "$PA" --json | jq -r '.[] | select(.priority > 1000) | .id'); do resetp "$id" >/dev/null; done
expect_eq "after resetting every generated value the queue maximum is back at 500 (Front gives 1001 again)" "1001/500" "$(front "$(new_task "$PA" "$DA" probe-final build haiku)" | change_of)"

# ============================================================================================
echo "== 4. real launcher, build work: one global order across two projects; held/blocked fall through; live task untouched"
new_world
declare -a U
order=(PA PB PA PB PA PB PB PA)
for i in 1 2 3 4 5 6 7 8; do
  if [ "${order[$((i - 1))]}" = PA ]; then U[$i]="$(ready_task "$PA" "$DA" "u$i" build haiku)"; else U[$i]="$(ready_task "$PB" "$DB" "u$i" build haiku)"; fi
done
expect_eq "Front on the 4th task (queue maximum 500)" "1001/500" "$(front "${U[4]}" | change_of)"
setp "${U[5]}" 1000 >/dev/null; setp "${U[6]}" 505 >/dev/null
expect_eq "a later Front (u3) takes 1002" "1002/1001" "$(front "${U[3]}" | change_of)"
SNAP_BEFORE="$(snapshot)"; restart_server
expect_eq "priorities are exact across a restart before any launcher runs" "$SNAP_BEFORE" "$(snapshot)"
start_agent builder haiku implement
all_started() { [ "$(total_starts)" -ge 8 ]; }
wait_for 150 all_started || fail "the launcher should have dispatched all 8 tasks"
sleep 2
WANT="${U[3]},${U[4]},${U[5]},${U[6]},${U[1]},${U[2]},${U[7]},${U[8]}"
expect_eq "launch order is the single comparator: u3(1002) u4(1001) u5(1000) u6(505), then 500s oldest-first across projects u1(A) u2(B) u7(B) u8(A)" \
  "$WANT" "$(sed -n 's/.* start task=\([^ ]*\) .*/\1/p' "$FAKE/starts.log" | paste -sd, -)"
expect_eq "exactly 8 model processes started (no duplicate launch)" 8 "$(total_starts)"
expect_eq "every fake model claimed exactly the task the launcher selected (pinned selection never drifts)" 8 \
  "$(sed -n 's/.* start task=\([^ ]*\) selected=\([^ ]*\) .*/\1 \2/p' "$FAKE/starts.log" | awk '$1 == $2 && $1 != ""' | wc -l | tr -d ' ')"
dups=0; for i in 1 2 3 4 5 6 7 8; do [ "$(starts "${U[$i]}")" = 1 ] || dups=1; done
expect_eq "every task has exactly one owner start (no duplicate ownership)" 0 "$dups"
bad=0; for i in 1 2 3 4 5 6 7 8; do [ "$(task_state "${U[$i]}")" = in_progress ] && [ -n "$(task_assignee "${U[$i]}")" ] || bad=1; done
expect_eq "every task ended up in_progress under one assignee" 0 "$bad"
stop_agent builder

echo "  -- held and dependency-blocked high-priority work falls through; a live task is not interrupted"
BASE="$(ready_task "$PA" "$DA" base build haiku)"; LO1="$(ready_task "$PB" "$DB" lo1 build haiku)"; LO2="$(ready_task "$PA" "$DA" lo2 build haiku)"
HELD="$(ready_task "$PA" "$DA" held build haiku)"; DEP="$(ready_task "$PA" "$DA" dep build haiku ",\"depends_on\":[\"$BASE\"]")"
api -X POST "$ODONIAN_URL/tasks/$HELD/hold" >/dev/null || die hold
touch "$FAKE/hold.$BASE"; : > "$FAKE/starts.log"; : > "$FAKE/exits.log"
start_agent builder haiku implement
check "the oldest claimable task (base) is launched first and stays running" wait_expr 60 '[ "$(starts "$BASE")" = 1 ]'
LEASE_BEFORE="$(task_json "$BASE" | jq -c '[.state,.assignee,.lease_expires_at]')"
expect_eq "Front on the held task counts in-flight u3 (1002) in the queue maximum" "1003/1002" "$(front "$HELD" | change_of)"
expect_eq "Front on the dependency-blocked task counts the held one" "1004/1003" "$(front "$DEP" | change_of)"
expect_eq "Front on a younger claimable task (lo2)" "1005/1004" "$(front "$LO2" | change_of)"
expect_eq "Front on the live, running task itself" "1006/1005" "$(front "$BASE" | change_of)"
expect_eq "the running task's state, assignee and lease are unchanged by repriorizing it" "$LEASE_BEFORE" "$(task_json "$BASE" | jq -c '[.state,.assignee,.lease_expires_at]')"
check "its model process was not signalled, restarted or duplicated" bash -c '! grep -q signal "$0" && test "$(grep -c "task=$1 " "$2")" = 1' "$FAKE/exits.log" "$BASE" "$FAKE/starts.log"
sleep 3
expect_eq "nothing else launched while the live task was running" 1 "$(total_starts)"
touch "$FAKE/release.$BASE"
all3() { [ "$(total_starts)" -ge 3 ]; }
wait_for 60 all3 || fail "after the release the launcher should pick the next two tasks"
sleep 4
expect_eq "after the release the newly fronted lo2 (1005) goes before lo1 (500); held (1003) and dependency-blocked (1004) are skipped" \
  "$BASE,$LO2,$LO1" "$(sed -n 's/.* start task=\([^ ]*\) .*/\1/p' "$FAKE/starts.log" | paste -sd, -)"
expect_eq "the held task is untouched: still ready, held, unassigned" "ready/true/" "$(task_json "$HELD" | jq -r '"\(.state)/\(.held)/\(.assignee // "")"')"
expect_eq "the dependency-blocked task is untouched: ready, unassigned, never started" "ready//0" "$(task_state "$DEP")/$(task_assignee "$DEP")/$(starts "$DEP")"
stop_agent builder

# ============================================================================================
echo "== 5. real launcher, research: quota-unavailable fallback to another pool; reserved capacity unchanged; inherited P > 1000"
new_world
touch "$FAKE/hold.all"
RX="$(ready_task "$PA" "$DA" rx research opus)"; R_OPUS="$(ready_task "$PB" "$DB" r-opus research opus)"
R_SON="$(ready_task "$PA" "$DA" r-sonnet research sonnet)"; W2="$(ready_task "$PB" "$DB" w2 research sonnet)"
ALPHA_BURST="$(pool_field acct-alpha tokens)"
check "alpha starts with its single-start burst" float_ge "$ALPHA_BURST" 0.99
claim_rc() { odonian claim "$1" --agent "$2" --model "$3" --work-class "$4" >"$TMP/claim.out" 2>"$TMP/claim.err"; echo $?; }
finalize_last() { # finalize_last <task> <agent> <model> [claim-output]
  local f="${4:-$TMP/claim.out}"
  odonian permit-finalize "$(jq -r .permit_id "$f")" --task-id "$1" --model "$3" --agent-id "$2" \
    --request-id "$(jq -r .request_id "$f")" --attempt-id "$(jq -r .attempt_id "$f")" --exit-class completed >/dev/null
}
expect_eq "one opus research start is admitted directly and exhausts alpha" 0 "$(claim_rc "$RX" direct-x opus research_write)"
finalize_last "$RX" direct-x opus || fail "finalize rx"
ALPHA_SPENT="$(pool_field acct-alpha tokens)"
check "alpha now has less than one start of quota" float_le "$ALPHA_SPENT" 0.5
expect_eq "Front on the sonnet task (queue maximum 500)" "1001/500" "$(front "$R_SON" | change_of)"
expect_eq "Front on the opus task afterwards: it is now the highest-priority work on the server" "1002/1001" "$(front "$R_OPUS" | change_of)"
start_agent writer-1 sonnet implement
check "the launcher falls past quota-blocked R_OPUS (1002) and launches R_SON (1001) from the other pool" wait_expr 60 '[ "$(starts "$R_SON")" = 1 ]'
expect_eq "the quota-blocked high-priority task never started: still ready, unassigned, round 0" "ready//0/0" \
  "$(task_state "$R_OPUS")/$(task_assignee "$R_OPUS")/$(task_round "$R_OPUS")/$(starts "$R_OPUS")"
check "the launcher logged a rate (quota) deferral for it and launched no model" agent_log_has writer-1 "reason: rate"
check "the fallback was admitted with a permit: beta has 1 active dispatch" test "$(pool_field acct-beta active)" = 1
check "the denial debited nothing: alpha is still below one start and has no active dispatch" bash -c \
  'awk -v t="$0" "BEGIN { exit !(t <= 0.5) }" && test "$1" = 0' "$(pool_field acct-alpha tokens)" "$(pool_field acct-alpha active)"
check "the writer submitted for review through the preclaimed attempt" wait_expr 20 '[ "$(task_state "$R_SON")" = review ]'
REV="$(review_of "$PA" "$R_SON")" || die "no review for R_SON"
expect_eq "the review spawned after Front inherits 1001 (generated, above the manual bound)" 1001 "$(p_of "$REV")"
echo "  -- Front while a dispatch is live does not interrupt it"
STARTS_BEFORE="$(total_starts)"
expect_eq "Front on the live writer's topic again (queue maximum is R_OPUS 1002)" "1003/1002" "$(front "$R_SON" | change_of)"
expect_eq "the review follows the topic to 1003" 1003 "$(p_of "$REV")"
expect_eq "Front on the second sonnet writer W2" "1004/1003" "$(front "$W2" | change_of)"
sleep 3
check "no model process was signalled, replaced or duplicated, and the permit is still held" bash -c \
  '! grep -q signal "$0" && test "$(grep -c "task=$1 " "$2")" = 1 && test "$3" = 1' "$FAKE/exits.log" "$R_SON" "$FAKE/starts.log" "$(pool_field acct-beta active)"
expect_eq "Front launched nothing by itself" "$STARTS_BEFORE" "$(total_starts)"
echo "  -- completion-reserved capacity is unchanged: the highest-priority writer cannot take it, the review can"
start_agent writer-2 sonnet implement
check "W2 (1004, the highest priority) is denied for reserved capacity although a slot is free" wait_for 40 agent_log_has writer-2 "reason: reserved_capacity"
check "R_OPUS is denied by quota for this launcher too" wait_for 40 agent_log_has writer-2 "reason: rate"
start_agent reviewer-1 fable review
check "the review (1003) is admitted into the reserved slot and its model starts" wait_expr 60 '[ "$(starts "$REV")" = 1 ]'
expect_eq "beta shows 2 active dispatches, 1 of them completion work" "2/1" "$(pool_field acct-beta active)/$(pool_field acct-beta active_completion)"
expect_eq "W2 and R_OPUS still have not launched" "0/0" "$(starts "$W2")/$(starts "$R_OPUS")"
expect_eq "no duplicate ownership: R_SON and its review each started once, nothing else did" "1/1/2" "$(starts "$R_SON")/$(starts "$REV")/$(total_starts)"
BETA_TOKENS="$(pool_field acct-beta tokens)"
check "debits match admissions: beta spent exactly 2 starts (writer + review), denied asks cost nothing" bash -c \
  'awk -v t="$0" "BEGIN { exit !(t >= 7.8 && t <= 8.3) }"' "$BETA_TOKENS"
check "alpha was still not debited by any denied ask" float_le "$(pool_field acct-alpha tokens)" 0.5
expect_eq "reset on the live topic: R_SON and its running review drop to 500" "500/500" "$(resetp "$R_SON" >/dev/null; p_of "$R_SON")/$(p_of "$REV")"
check "resetting a running dispatch's priority still did not signal or relaunch anything" bash -c '! grep -q signal "$0" && test "$(grep -c " start " "$1")" = 2' "$FAKE/exits.log" "$FAKE/starts.log"
touch "$FAKE/release.all"
check "the running writer and review finish normally (both exits recorded as released)" wait_expr 30 '[ "$(grep -c released "$FAKE/exits.log")" -ge 2 ]'
stop_all_agents
check "all permits are released once the launchers stop" wait_expr 30 '[ "$(pool_field acct-beta active)" = 0 ]'
rm -f "$FAKE/hold.all"

resetp "$W2" >/dev/null; resetp "$R_OPUS" >/dev/null
echo "  -- rework inherits P > 1000 and takes the reserved slot while an even higher first-pass writer is denied"
finalize_claim() { finalize_last "$1" "$2" "$3" "$4" || fail "finalize $1"; }
RW="$(ready_task "$PA" "$DA" rework-candidate research sonnet)"
expect_eq "Front on the future rework task" "1001/500" "$(front "$RW" | change_of)"
expect_eq "its writer is admitted" 0 "$(claim_rc "$RW" direct-w sonnet research_write)"
cp "$TMP/claim.out" "$TMP/claim.w.out"
odonian submit "$RW" --agent direct-w --result "fake provider output" --no-op >/dev/null || fail "submit RW"
finalize_claim "$RW" direct-w sonnet "$TMP/claim.w.out"
have_review() { RWREV="$(review_of "$PA" "$RW")"; [ -n "$RWREV" ]; }
check "the no-op submission spawned a review that inherited 1001" wait_for 15 have_review
expect_eq "the RW review carries 1001" 1001 "$(p_of "$RWREV")"
expect_eq "the RW review is admitted as completion work" 0 "$(claim_rc "$RWREV" direct-rv fable research_review)"
cp "$TMP/claim.out" "$TMP/claim.rv.out"
printf '%s' '[{"id":"f1","severity":"P2","file":"claims.md","line":1,"summary":"smoke finding","in_changed_text":true,"status":"new"}]' > "$TMP/findings.json"
odonian submit "$RWREV" --agent direct-rv --verdict reject --findings-file "$TMP/findings.json" --result "smoke rejection" >/dev/null || fail "reject RW review"
finalize_claim "$RWREV" direct-rv fable "$TMP/claim.rv.out"
expect_eq "rework: RW is ready again, review round 1, still 1001" "ready/1/1001" "$(task_state "$RW")/$(task_round "$RW")/$(p_of "$RW")"
WX="$(ready_task "$PB" "$DB" writer-x research sonnet)"; WY="$(ready_task "$PB" "$DB" writer-y research sonnet)"
expect_eq "Front on writer Y makes it the highest-priority first-pass task (1002)" "1002/1001" "$(front "$WY" | change_of)"
expect_eq "first-pass writer X (500) is admitted into the writer slot" 0 "$(claim_rc "$WX" direct-x sonnet research_write)"
cp "$TMP/claim.out" "$TMP/claim.x.out"
expect_eq "writer Y (1002) is denied reserved_capacity although a slot is free" "10/1" \
  "$(claim_rc "$WY" direct-y sonnet research_write)/$(grep -c 'reason: reserved_capacity' "$TMP/claim.err")"
expect_eq "the rework claim (1001, lower than Y) is admitted into the reserved slot" 0 "$(claim_rc "$RW" direct-r sonnet research_rework)"
cp "$TMP/claim.out" "$TMP/claim.r.out"
expect_eq "pool shows 2 active, 1 completion" "2/1" "$(pool_field acct-beta active)/$(pool_field acct-beta active_completion)"
expect_eq "Y is now denied for concurrency" "10/1" "$(claim_rc "$WY" direct-y sonnet research_write)/$(grep -c 'reason: concurrency' "$TMP/claim.err")"
expect_eq "the denied asks never claimed ownership of Y" "ready/" "$(task_state "$WY")/$(task_assignee "$WY")"
finalize_claim "$WX" direct-x sonnet "$TMP/claim.x.out"
expect_eq "once X's permit is finalized, Y (highest priority) is admitted" 0 "$(claim_rc "$WY" direct-y sonnet research_write)"

echo
echo "passed: $pass_count  failed: $fail_count"
if [ "$fail_count" -gt 0 ]; then
  echo "--- server log (tail) ---"; tail -20 "$TMP/server.log"
  for f in "$TMP"/agent-*.log; do [ -e "$f" ] && { echo "--- ${f##*/} (tail) ---"; tail -15 "$f"; }; done
  echo "--- fake provider starts/exits ---"; cat "$FAKE/starts.log" "$FAKE/exits.log"
  exit 1
fi
exit 0
