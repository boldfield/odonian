#!/usr/bin/env bash
# research_pacing_smoke_test.sh — end-to-end, deterministic smoke for automatic research pacing.
#
# A REAL `odonian server` (freshly built, temp SQLite DB, enforce mode, one shared account pool) is
# driven by the REAL harness/agent.sh, with a FAKE `claude` standing in for the model provider.
# Nothing here can reach a real model, subscription, GitHub, or any cluster: PATH is rebuilt from a
# fixed set of system tools plus the fakes, every service listens on 127.0.0.1, and all state lives
# under one temp dir. The pool numbers are illustrative test values, not production recommendations.
#
# Scenarios (each prints ✓/✗ lines; exit status is non-zero if any ✗):
#   1. concurrency limit shared across two projects; the denied task stays ready, no model launched
#   2. a review is admitted into the capacity reserved for completion work (a writer was denied it)
#   3. server restart: active permits and the spent allowance survive; no refill, no duplicate launch
#   4. build-track work is unaffected by a saturated research pool
#   5. the deferred task is admitted automatically once capacity is released (no manual promotion)
#   6. rework is completion work: it takes the reserved slot while first-pass writers are denied
#
# Usage: bash harness/research_pacing_smoke_test.sh        (needs go, jq, git, curl; SMOKE_VERBOSE=1 adds ports/ids/allowance detail)
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HARNESS_DIR/.." && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
info() { [ -z "${SMOKE_VERBOSE:-}" ] || echo "   $*"; }  # run-specific detail (ports, ids, allowance); kept out of the default output so it is reproducible
die() { echo "FATAL: $*" >&2; exit 2; }
ok() { # ok "<description>" '<shell expression>'  — evaluated now, in this shell; pass when it succeeds
  if eval "$2"; then pass "$1"; else fail "$1"; fi
}

for tool in go jq git curl; do command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }; done

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
stop_agent() { # stop_agent <name>: graceful TERM (agent.sh terminates its model process and finalizes its permit)
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

# --- build the real binary, then a sanitized PATH: fixed system tools + our fakes only ---
BIN="$TMP/bin"; SYSBIN="$TMP/sysbin"; mkdir -p "$BIN" "$SYSBIN"
(cd "$REPO_ROOT" && go build -o "$BIN/odonian" ./cmd/odonian) || die "go build failed"
for tool in bash env jq git curl date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep readlink tail uname ln mkdir chmod mv cp ls find tee sh; do
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

# --- the fake model provider ---
FAKE="$TMP/fake"; mkdir -p "$FAKE"; : > "$FAKE/starts.log"; : > "$FAKE/exits.log"
cat > "$BIN/claude" <<'EOF'
#!/usr/bin/env bash
# fake `claude -p`: records that a model process started, does the minimal board work a real
# research writer does (submit a no-op so the review task spawns), then holds the "dispatch" open
# until the smoke releases it. It never calls any model.
trap 'echo "$(date +%s.%N) exit task=${task:-none} signal" >> "$FAKE_DIR/exits.log"; exit 143' TERM INT
task="${ODONIAN_PRECLAIMED_TASK_ID:-}"
pre=1
if [ -z "$task" ]; then   # non-research (build) flow: the model claims for itself, as the real prompt does
  pre=0
  task="$(odonian next --project "$ODONIAN_PROJECT" --model "$AGENT_MODEL" --kind implement --claim 2>/dev/null)"
fi
echo "$(date +%s.%N) start task=$task model=$AGENT_MODEL preclaimed=$pre project=$ODONIAN_PROJECT" >> "$FAKE_DIR/starts.log"
kind="$(odonian show "$task" --json 2>/dev/null | jq -r '.kind // empty')"
if [ "$pre" = 1 ] && [ "$kind" = implement ]; then
  odonian submit "$task" --attempt "${ODONIAN_PRECLAIMED_ATTEMPT_ID:-}" --result "fake provider output" --no-op >/dev/null 2>&1
fi
for _ in $(seq 1 1200); do
  [ -e "$FAKE_DIR/release.$task" ] || [ -e "$FAKE_DIR/release.all" ] && break
  sleep 0.1
done
echo "$(date +%s.%N) exit task=$task released" >> "$FAKE_DIR/exits.log"
exit 0
EOF
chmod +x "$BIN/claude"
export FAKE_DIR="$FAKE"

# --- server lifecycle (real binary, temp DB, enforce mode, loopback only) ---
export ODONIAN_TOKEN="smoke-token" ODONIAN_DB="$TMP/odonian.db"
for _ in $(seq 1 50); do
  PORT=$((20000 + RANDOM % 20000))
  curl -s --max-time 1 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 || break
done
export ODONIAN_URL="http://127.0.0.1:$PORT" ODONIAN_ADDR="127.0.0.1:$PORT"
export ODONIAN_MODELS="opus,fable,sonnet"
export ODONIAN_RESEARCH_POLICY_MODE="enforce"
# Illustrative test pool: two dispatch slots, one reserved for completion work, so at most ONE
# first-pass writer runs at a time. A slow refill and a bounded burst make allowance visible.
RATE=0.05; BURST=10
export ODONIAN_RESEARCH_POOLS="{\"main\":{\"account_id\":\"acct-smoke\",\"models\":[\"opus\",\"fable\",\"sonnet\"],\"start_rate\":$RATE,\"burst_capacity\":$BURST,\"concurrent_dispatch_limit\":2,\"completion_reserved\":1}}"
export ODONIAN_STATE_DIR="$TMP/cli-state"
# Fast harness timers so the smoke finishes in about a minute.
export ODONIAN_RENEW_INTERVAL_SECS=2 ODONIAN_RENEW_POLL_SECS=1 ODONIAN_AMBIGUOUS_RETRY_SECS=2 ODONIAN_ADMISSION_RETRY_NAP=1

start_server() {
  "$BIN/odonian" server >>"$TMP/server.log" 2>&1 &
  SERVER_PID=$!
  for _ in $(seq 1 100); do
    curl -fsS --max-time 1 "$ODONIAN_URL/healthz" >/dev/null 2>&1 && return 0
    kill -0 "$SERVER_PID" 2>/dev/null || { cat "$TMP/server.log" >&2; die "server exited during startup"; }
    sleep 0.1
  done
  die "server did not become healthy"
}

# --- helpers (every board helper fails loudly: no swallowed curl/HTTP errors) ---
api() { curl -fsS --max-time 10 -H "Authorization: Bearer $ODONIAN_TOKEN" -H 'Content-Type: application/json' "$@"; }
mk_project() { api -d "{\"name\":\"$1\",\"repo\":\"$BARE\"}" "$ODONIAN_URL/projects" | jq -er .id || die "create project $1"; }
mk_doc() { api -d '{"title":"smoke","kind":"design","content":"x"}' "$ODONIAN_URL/projects/$1/documents" | jq -er .id || die "create document"; }
mk_task() { # mk_task <project> <doc> <title> <track> <model> -> promoted ready task id
  local id
  id="$(api -d "[{\"title\":\"$3\",\"spec\":\"smoke task\",\"document_id\":\"$2\",\"model\":\"$5\",\"review_models\":[\"fable\"],\"track\":\"$4\"}]" \
        "$ODONIAN_URL/projects/$1/tasks" | jq -er '.[0].id')" || die "create task $3"
  odonian promote "$id" >/dev/null || die "promote $id"
  echo "$id"
}
task_json() { odonian show "$1" --json; }
task_state() { task_json "$1" | jq -r '.state'; }
task_assignee() { task_json "$1" | jq -r '.assignee // ""'; }
task_round() { task_json "$1" | jq -r '.review_round'; }
pool() { odonian research-status --json | jq -r ".pools[0].$1"; }
starts() { grep -c "task=$1 " "$FAKE/starts.log" 2>/dev/null || true; }
total_starts() { wc -l < "$FAKE/starts.log" | tr -d ' '; }
wait_for() { local t="$1" end; shift; end=$(( $(date +%s) + t )); while :; do "$@" && return 0; [ "$(date +%s)" -ge "$end" ] && return 1; sleep 0.3; done; }
agent_log_has() { grep -q "$2" "$TMP/agent-$1.log" 2>/dev/null; }
start_agent() { # start_agent <name> <project> <model> <kind>
  local name="$1" project="$2" model="$3" kind="$4" home="$TMP/home-$1" clone="$TMP/clone-$1"
  mkdir -p "$home"; git clone --quiet "$BARE" "$clone" 2>/dev/null || die "clone for $name"
  ( export ODONIAN_HOME="$home" ODONIAN_PROJECT="$project" ODONIAN_REPO="$clone"
    exec bash "$HARNESS_DIR/agent.sh" --model "$model" --kind "$kind" "$name" ) >"$TMP/agent-$name.log" 2>&1 &
  echo $! > "$TMP/pid.$name"
}
float_le() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a <= b) }'; }

echo "== setup: real server (enforce), temp DB, fake claude"
start_server
info "server $ODONIAN_URL  db $ODONIAN_DB"
ok "server reports enforce mode with the configured pool" \
  '[ "$(odonian research-policy --json | jq -r .mode)" = enforce ] && [ "$(odonian research-policy --json | jq -r ".pools[0].concurrent_limit")" = 2 ]'
PA="$(mk_project proj-a)"; PB="$(mk_project proj-b)"; PC="$(mk_project proj-c-build)"
DA="$(mk_doc "$PA")"; DB="$(mk_doc "$PB")"; DC="$(mk_doc "$PC")"
TA="$(mk_task "$PA" "$DA" research-a research opus)"
TB="$(mk_task "$PB" "$DB" research-b research opus)"

echo "== 1. concurrency limit shared across two projects"
start_agent writer-a "$PA" opus implement
start_agent writer-b "$PB" opus implement
one_started() { [ "$(total_starts)" -ge 1 ]; }
wait_for 60 one_started || fail "a research writer should have launched"
sleep 4   # give the other project's writer time to be asked and denied
ok "exactly one research model process launched across both projects" '[ "$(total_starts)" = 1 ]'
if [ "$(starts "$TA")" = 1 ]; then WIN="$TA"; WINP="$PA"; WINAG=writer-a; LOSE="$TB"; LOSEAG=writer-b
else WIN="$TB"; WINP="$PB"; WINAG=writer-b; LOSE="$TA"; LOSEAG=writer-a; fi
info "winner ${WIN:0:8} (project ${WINP:0:8}); denied ${LOSE:0:8}"
ok "the denied task is untouched: still ready, unassigned, round 0, no model process" \
  '[ "$(task_state "$LOSE")" = ready ] && [ -z "$(task_assignee "$LOSE")" ] && [ "$(task_round "$LOSE")" = 0 ] && [ "$(starts "$LOSE")" = 0 ]'
ok "the denial was a reserved-capacity deferral (a slot was free but held for completion work)" \
  'agent_log_has "$LOSEAG" "reason: reserved_capacity"'
ok "pool shows 1 active dispatch and 1 deferred task" '[ "$(pool active)" = 1 ] && [ "$(pool deferred)" = 1 ]'

echo "== 2. review is admitted into the reserved completion slot"
rev_ready() { [ "$(task_state "$WIN")" = review ]; }
wait_for 30 rev_ready || fail "the winner should have submitted for review"
start_agent reviewer "$WINP" fable review
rev_started() { grep -q "model=fable preclaimed=1" "$FAKE/starts.log"; }
ok "a fable review launched while the second writer is still denied" 'wait_for 60 rev_started'
ok "pool shows 2 active dispatches, 1 of them completion work" '[ "$(pool active)" = 2 ] && [ "$(pool active_completion)" = 1 ]'
ok "the denied writer still has not launched" '[ "$(starts "$LOSE")" = 0 ]'
ok "the denial is now concurrency-bound (both slots busy)" 'wait_for 20 agent_log_has "$LOSEAG" "reason: concurrency"'

echo "== 3. restart persistence"
TOK_BEFORE="$(pool tokens)"; T_BEFORE="$(date +%s)"; N_BEFORE="$(total_starts)"
stop_server; sleep 2; start_server
ELAPSED=$(( $(date +%s) - T_BEFORE ))
TOK_AFTER="$(pool tokens)"
LIMIT="$(awk -v t="$TOK_BEFORE" -v e="$ELAPSED" -v r="$RATE" 'BEGIN { printf "%.4f", t + e * r + 0.25 }')"
info "tokens before restart $TOK_BEFORE, after $TOK_AFTER (elapsed ${ELAPSED}s, refill drift bound $LIMIT, burst $BURST)"
ok "active permits survived the restart (2 active, 1 completion)" '[ "$(pool active)" = 2 ] && [ "$(pool active_completion)" = 1 ]'
ok "the restart did not refill the allowance (tokens within refill-rate drift of before)" 'float_le "$TOK_AFTER" "$LIMIT"'
sleep 6   # several 2s renewal intervals against the restarted server
ok "renewals after the restart kept both dispatches alive (still 2 active, no process fenced)" \
  '[ "$(pool active)" = 2 ] && ! grep -q signal "$FAKE/exits.log"'
ok "no model process was launched or duplicated by the restart" '[ "$(total_starts)" = "$N_BEFORE" ]'
ok "the denied task is still waiting after the restart" '[ "$(task_state "$LOSE")" = ready ] && [ "$(starts "$LOSE")" = 0 ]'

echo "== 4. build work is unaffected by the saturated research pool"
TC="$(mk_task "$PC" "$DC" build-c build opus)"
start_agent builder "$PC" opus implement
build_started() { grep -q "task=$TC .*preclaimed=0" "$FAKE/starts.log"; }
ok "the build task launched while the research pool was full" 'wait_for 60 build_started'
ok "the build task is in progress and took no research permit (pool still 2 active)" \
  '[ "$(task_state "$TC")" = in_progress ] && [ "$(pool active)" = 2 ]'
ok "the build agent never asked for research admission" '! grep -q "research admission" "$TMP/agent-builder.log"'

echo "== 5. deferred task resumes automatically when capacity frees"
ok "the denied task is still plain ready work (never promoted or edited by hand)" '[ "$(task_state "$LOSE")" = ready ]'
REVIEW_TASK="$(odonian tasks --project "$WINP" --json | jq -r '.[] | select(.kind=="review") | .id' | head -1)"
touch "$FAKE/release.$WIN" "$FAKE/release.$REVIEW_TASK"
lose_started() { [ "$(starts "$LOSE")" -ge 1 ]; }
ok "the denied task launched on its own after the release" 'wait_for 90 lose_started'
ok "its task is now owned by its own agent (launched, then submitted for review)" '[ "$(task_state "$LOSE")" != ready ] && [ -n "$(task_assignee "$LOSE")" ]'
ok "the released dispatches exited normally and finalized their permits" '[ "$(grep -c released "$FAKE/exits.log")" -ge 2 ]'
touch "$FAKE/release.all"

echo "== 6. rework is completion work: reserved slot while first-pass writers are denied"
stop_all_agents    # the remaining checks drive the same real admission path directly with the CLI
settled() { [ "$(pool active)" = 0 ]; }
ok "all permits are released once the agents stop" 'wait_for 30 settled'
claim_rc() { odonian claim "$1" --agent "$2" --model "$3" --work-class "$4" >"$TMP/claim.out" 2>"$TMP/claim.err"; echo $?; }
finalize_last() { # finalize_last <task> <agent> <model> [claim-output]: end an admitted attempt, as the harness does on process exit
  local f="${4:-$TMP/claim.out}"
  odonian permit-finalize "$(jq -r .permit_id "$f")" --task-id "$1" --model "$3" --agent-id "$2" \
    --request-id "$(jq -r .request_id "$f")" --attempt-id "$(jq -r .attempt_id "$f")" --exit-class completed >/dev/null
}
# Produce a task awaiting rework through the real flow: write -> submit -> review rejects -> ready, round 1.
W3="$(mk_task "$PA" "$DA" rework-candidate research opus)"
ok "writer W3 is admitted" '[ "$(claim_rc "$W3" direct-w3 opus research_write)" = 0 ]'
odonian submit "$W3" --agent direct-w3 --result "fake provider output" --no-op >/dev/null || fail "submit W3"
finalize_last "$W3" direct-w3 opus || fail "finalize W3"
review_of() { # review_of <task>: id of the review task that targets <task>
  local id
  for id in $(odonian tasks --project "$PA" --json | jq -r '.[] | select(.kind=="review") | .id'); do
    [ "$(odonian show "$id" --json | jq -r '.target_task_id // empty')" = "$1" ] && { echo "$id"; return 0; }
  done
  return 1
}
have_review() { R3="$(review_of "$W3")"; [ -n "$R3" ]; }
ok "the no-op submission spawned a review task targeting W3" 'wait_for 15 have_review'
ok "W3's review is admitted as completion work" '[ "$(claim_rc "$R3" direct-rv fable research_review)" = 0 ]'
printf '%s' '[{"id":"f1","severity":"P2","file":"claims.md","line":1,"summary":"smoke finding","in_changed_text":true,"status":"new"}]' > "$TMP/findings.json"
odonian submit "$R3" --agent direct-rv --verdict reject --findings-file "$TMP/findings.json" --result "smoke rejection" >/dev/null || fail "submit rejection"
finalize_last "$R3" direct-rv fable || fail "finalize review"
ok "W3 is back in ready with review_round >= 1 (rework), nothing active" \
  '[ "$(task_state "$W3")" = ready ] && [ "$(task_round "$W3")" -ge 1 ] && [ "$(pool active)" = 0 ]'
WX="$(mk_task "$PA" "$DA" writer-x research opus)"; WY="$(mk_task "$PA" "$DA" writer-y research opus)"
ok "first-pass writer X is admitted (takes the writer slot)" '[ "$(claim_rc "$WX" direct-x opus research_write)" = 0 ]'
cp "$TMP/claim.out" "$TMP/claim.x.out"
ok "second first-pass writer Y is denied although a slot is free (reserved_capacity)" \
  '[ "$(claim_rc "$WY" direct-y opus research_write)" = 10 ] && grep -q "reason: reserved_capacity" "$TMP/claim.err"'
ok "the rework claim is admitted into the reserved slot (server derived work class research_rework)" \
  '[ "$(claim_rc "$W3" direct-r opus research_rework)" = 0 ]'
ok "pool shows 2 active, 1 completion" '[ "$(pool active)" = 2 ] && [ "$(pool active_completion)" = 1 ]'
ok "writer Y is now denied for concurrency (all slots busy)" \
  '[ "$(claim_rc "$WY" direct-y opus research_write)" = 10 ] && grep -q "reason: concurrency" "$TMP/claim.err"'
finalize_last "$WX" direct-x opus "$TMP/claim.x.out" || fail "finalize X"
ok "after X's permit is finalized, writer Y is admitted" '[ "$(claim_rc "$WY" direct-y opus research_write)" = 0 ]'

echo
echo "passed: $pass_count  failed: $fail_count"
if [ "$fail_count" -gt 0 ]; then
  echo "--- server log (tail) ---"; tail -20 "$TMP/server.log"
  for f in "$TMP"/agent-*.log; do echo "--- ${f##*/} (tail) ---"; tail -12 "$f"; done
  echo "--- fake provider starts/exits ---"; cat "$FAKE/starts.log" "$FAKE/exits.log"
  exit 1
fi
exit 0
