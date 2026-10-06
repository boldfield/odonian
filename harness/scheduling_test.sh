#!/usr/bin/env bash
# scheduling_test.sh — behavioural tests for global priority scheduling in agent.sh. The REAL
# agent.sh (worker, reviewer and merger; SINGLE and MULTI) runs against a stateful fake `odonian`
# and a fake `claude`; assertions are made on their logs. The fake `next` mirrors the real CLI
# (server comparator, ODONIAN_SELECTED_TASK_ID pin) and the fake `claude` behaves like a prompt
# (it runs `odonian next --claim` unless the task was preclaimed), so a dispatch that picks a
# different task than the harness selected is visible. PATH is rebuilt from a fixed set of system
# tools: no real claude/codex/gh/odonian server is reachable, so no paid model is ever called.
#
# Comparator under test: priority DESC, created_at ASC, task ID ASC — at every value (including
# server-generated values above 1000), across all allowed projects, with no project shuffle.
set -uo pipefail

HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
AGENT_PIDS=()
stop_agents() {
  local pid i
  for pid in "${AGENT_PIDS[@]:-}"; do [ -n "$pid" ] && kill -TERM "$pid" 2>/dev/null; done
  for pid in "${AGENT_PIDS[@]:-}"; do
    [ -n "$pid" ] || continue
    for i in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep 0.1; done
    kill -KILL "$pid" 2>/dev/null; wait "$pid" 2>/dev/null
  done
  AGENT_PIDS=()
}
trap 'stop_agents; rm -rf "$TMP"' EXIT

for tool in jq git; do command -v "$tool" >/dev/null || { echo "SKIP: $tool not installed"; exit 0; }; done

SYSBIN="$TMP/sysbin"; mkdir -p "$SYSBIN"
for tool in bash env jq git date sed sleep mktemp tr head od hostname cat rm stat du cut sort seq awk basename \
            dirname wc touch grep readlink tail uname ln mkdir chmod mv cp ls find tee sh rmdir paste; do
  p="$(command -v "$tool" 2>/dev/null)"; [ -n "$p" ] && [ "${p#/}" != "$p" ] && ln -sf "$p" "$SYSBIN/$tool"
done
BIN="$TMP/bin"; mkdir -p "$BIN"
export PATH="$BIN:$SYSBIN"

BARE="$TMP/origin.git"
git init --quiet --bare "$BARE"
SEED="$TMP/seed"; git clone --quiet "$BARE" "$SEED" 2>/dev/null
git -C "$SEED" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$SEED" push --quiet origin HEAD:main 2>/dev/null
MAIN_REPO="$TMP/main-repo"; git clone --quiet "$BARE" "$MAIN_REPO" 2>/dev/null

# --- fake odonian. Scenario state under $FAKE_DIR:
#   tasks.json     [{id, project, kind, model, priority?, created_at, track?}] — every claimable task
#   projects.json  [{id, repo}]
#   claim.<id>     one mode per line, consumed per call (last repeats): grant | defer:N | taken
#   steal.<id>     present => another worker takes the task right after its first listing
#   claimed-<id>   (dir) task is claimed by someone; created atomically with mkdir
cat > "$BIN/odonian" <<'EOF'
#!/usr/bin/env bash
log() { echo "$*" >> "$FAKE_DIR/calls.log"; }
verb="$1"; shift
flag() { local name="$1"; shift; while [ $# -gt 0 ]; do [ "$1" = "$name" ] && { echo "$2"; return; }; shift; done; }
has() { local name="$1"; shift; for a in "$@"; do [ "$a" = "$name" ] && return 0; done; return 1; }
unclaimed() { # $1 project ("" = any) $2 kind -> JSON array, claimed tasks removed, server comparator applied
  local out id
  out="$(jq -c --arg p "$1" --arg k "$2" '[.[] | select(($p == "" or .project == $p) and .kind == $k)]' "$FAKE_DIR/tasks.json")"
  for id in $(printf '%s' "$out" | jq -r '.[].id'); do
    [ -e "$FAKE_DIR/claimed-$id" ] && out="$(printf '%s' "$out" | jq -c --arg i "$id" 'map(select(.id != $i))')"
  done
  printf '%s' "$out"
}
case "$verb" in
  projects)
    kind="$(flag --kind "$@")"; out="[]"
    for p in $(jq -r '.[].id' "$FAKE_DIR/projects.json"); do
      [ "$(unclaimed "$p" "$kind" | jq 'length')" -gt 0 ] && \
        out="$(printf '%s' "$out" | jq -c --argjson p "$(jq -c --arg p "$p" '.[] | select(.id==$p)' "$FAKE_DIR/projects.json")" '. + [$p]')"
    done
    printf '%s' "$out"; exit 0 ;;
  tasks)
    proj="$(flag --project "$@")"; kind="$(flag --kind "$@")"
    # the real listing is unordered as far as the harness is concerned: reverse it so a harness
    # that trusted listing order would pick the wrong task
    list="$(unclaimed "$proj" "$kind" | jq 'reverse')"
    printf '%s\n' "$list"
    for id in $(printf '%s' "$list" | jq -r '.[].id'); do
      [ -e "$FAKE_DIR/steal.$id" ] && { mkdir "$FAKE_DIR/claimed-$id" 2>/dev/null; rm -f "$FAKE_DIR/steal.$id"; }
    done
    exit 0 ;;
  next)
    proj="$(flag --project "$@")"; kind="$(flag --kind "$@")"; model="$(flag --model "$@")"
    list="$(unclaimed "$proj" "$kind" | jq -c --arg m "$model" 'map(select($m == "" or .model == $m)) | sort_by([-(.priority // 500), .created_at, .id])')"
    pin="${ODONIAN_SELECTED_TASK_ID:-}"
    [ -n "$pin" ] && list="$(printf '%s' "$list" | jq -c --arg i "$pin" 'map(select(.id == $i))')"
    id="$(printf '%s' "$list" | jq -r '.[0].id // empty')"
    if [ -z "$id" ]; then log "next project=$proj kind=$kind pin=$pin -> none"; exit 2; fi
    if has --claim "$@"; then
      mkdir "$FAKE_DIR/claimed-$id" 2>/dev/null || { log "next project=$proj kind=$kind pin=$pin -> raced $id"; exit 2; }
    fi
    log "next project=$proj kind=$kind pin=$pin -> $id"
    echo "$id"; exit 0 ;;
  project) echo "{\"repo\":\"BARE_REPO\"}"; exit 0 ;;
  show)
    id="$1"; track="$(jq -r --arg i "$id" '.[] | select(.id==$i) | .track // "build"' "$FAKE_DIR/tasks.json")"
    echo "{\"track\":\"${track:-build}\",\"state\":\"ready\"}"; exit 0 ;;
  claim)
    id="$1"; shift
    log "claim $id model=$(flag --model "$@")"
    n=0; [ -f "$FAKE_DIR/claimcount.$id" ] && n="$(cat "$FAKE_DIR/claimcount.$id")"; n=$((n + 1)); echo "$n" > "$FAKE_DIR/claimcount.$id"
    mode=grant
    if [ -f "$FAKE_DIR/claim.$id" ]; then
      lines="$(wc -l < "$FAKE_DIR/claim.$id")"; [ "$n" -gt "$lines" ] && n="$lines"; mode="$(sed -n "${n}p" "$FAKE_DIR/claim.$id")"
    fi
    case "$mode" in
      defer:*) echo "scheduling: denied" >&2; echo "retry-after: ${mode#defer:}" >&2; exit 10 ;;
      taken) echo "error: already claimed" >&2; exit 3 ;;
    esac
    mkdir "$FAKE_DIR/claimed-$id" 2>/dev/null || { echo "error: already claimed" >&2; exit 3; }
    g="$(cat "$FAKE_DIR/grantcount" 2>/dev/null || echo 0)"; echo $((g + 1)) > "$FAKE_DIR/grantcount"
    printf '{"permit_id":"permit-%s","attempt_id":"attempt-%s","request_id":"%s"}\n' "$id" "$id" "$(flag --request-id "$@")"
    exit 0 ;;
  merge) log "merge $1"; exit 0 ;;
  heartbeat) exit 0 ;;
  permit-renew|permit-finalize) log "$verb $1"; echo '{}'; exit 0 ;;
  transition) log "transition $*"; exit 0 ;;
  *) log "$verb $*"; exit 0 ;;
esac
EOF
chmod +x "$BIN/odonian"
sed -i "s|BARE_REPO|$BARE|" "$BIN/odonian"

# --- fake claude: behaves like a prompt. A preclaimed (research) dispatch works that task; otherwise
# it runs `odonian next --claim` exactly as the build/review prompts do and works whatever it gets.
cat > "$BIN/claude" <<'EOF'
#!/usr/bin/env bash
model=""; prev=""
for a in "$@"; do [ "$prev" = "--model" ] && model="$a"; prev="$a"; done
pre="${ODONIAN_PRECLAIMED_TASK_ID:-}"
if [ -f "$FAKE_DIR/claude.fail.$model" ]; then
  echo "START model=$model project=${ODONIAN_PROJECT:-} pin=${ODONIAN_SELECTED_TASK_ID:-} pre=$pre worked=FAILED" >> "$FAKE_DIR/claude.log"; exit 1
fi
if [ -n "$pre" ]; then worked="$pre"
else worked="$(odonian next --project "${ODONIAN_PROJECT:-}" --model "$model" --kind "${FAKE_KIND:-implement}" --claim 2>/dev/null)"; fi
echo "START model=$model project=${ODONIAN_PROJECT:-} pin=${ODONIAN_SELECTED_TASK_ID:-} pre=$pre worked=${worked:-none}" >> "$FAKE_DIR/claude.log"
sleep "${FAKE_CLAUDE_SECS:-0}"
exit 0
EOF
chmod +x "$BIN/claude"

# ------------------------------- scenario plumbing -------------------------------
SCN=0
new_scenario() {
  FAKE_DIR="$TMP/s$((++SCN))"; mkdir -p "$FAKE_DIR"; export FAKE_DIR
  : > "$FAKE_DIR/calls.log"; : > "$FAKE_DIR/claude.log"
  export ODONIAN_HOME="$TMP/home$SCN"; mkdir -p "$ODONIAN_HOME"
  export FAKE_KIND=implement FAKE_CLAUDE_SECS=0
  unset ODONIAN_PROJECTS
  echo '[{"id":"p1","repo":"own/one"},{"id":"p2","repo":"own/two"},{"id":"p3","repo":"own/three"}]' > "$FAKE_DIR/projects.json"
  echo '[]' > "$FAKE_DIR/tasks.json"
}
# add_task id project priority created_at [model] [kind] [track]; priority "-" = unset (default 500)
add_task() {
  local id="$1" proj="$2" prio="$3" created="$4" model="${5:-x}" kind="${6:-implement}" track="${7:-build}"
  jq -c --arg id "$id" --arg p "$proj" --arg prio "$prio" --arg c "$created" --arg m "$model" --arg k "$kind" --arg t "$track" \
    '. + [{id:$id, project:$p, kind:$k, model:$m, state:"ready", track:$t, created_at:$c}
          + (if $prio == "-" then {} else {priority: ($prio|tonumber)} end)]' "$FAKE_DIR/tasks.json" > "$FAKE_DIR/tasks.new"
  mv "$FAKE_DIR/tasks.new" "$FAKE_DIR/tasks.json"
}
# start_agent kind single|multi [slot]
start_agent() {
  local kind="$1" mode="$2" slot="${3:-slot-$SCN}" p
  export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN=t ODONIAN_MAIN_REPO="$MAIN_REPO" ODONIAN_REPO="$MAIN_REPO"
  if [ "$mode" = multi ]; then
    export ODONIAN_PROJECT=all
    for p in $(jq -r '.[] | "\(.id)|\(.repo)"' "$FAKE_DIR/projects.json"); do
      local repo="${p#*|}" slug; slug="$(echo "$repo" | tr '/' '-')"
      [ -d "$ODONIAN_HOME/repos/$slug/.git" ] || { mkdir -p "$ODONIAN_HOME/repos"; git clone --quiet "$BARE" "$ODONIAN_HOME/repos/$slug" 2>/dev/null; }
    done
  else
    export ODONIAN_PROJECT=p1
  fi
  "$HARNESS_DIR/agent.sh" --model x --kind "$kind" "$slot" > "$FAKE_DIR/agent-$slot.log" 2>&1 &
  AGENT_PIDS+=("$!")
}
wait_for() { local t="$1" i; shift; for ((i = 0; i < t * 10; i++)); do "$@" && return 0; sleep 0.1; done; return 1; }
claude_count() { grep -c " worked=" "$FAKE_DIR/claude.log"; }
claude_count_ge() { [ "$(claude_count)" -ge "$1" ]; }
worked_order() { sed -n 's/.* worked=\([^ ]*\).*/\1/p' "$FAKE_DIR/claude.log" | paste -sd, -; }
calls_count() { grep -c "$1" "$FAKE_DIR/calls.log"; }
dump() { echo "    --- agent.log"; tail -15 "$FAKE_DIR"/agent-*.log | sed 's/^/    /'; echo "    --- calls.log"; sed 's/^/    /' "$FAKE_DIR/calls.log" | tail -25; echo "    --- claude.log"; sed 's/^/    /' "$FAKE_DIR/claude.log"; }
check() { local d="$1"; shift; if "$@"; then pass "$d"; else fail "$d"; FAILED_SCN=1; fi; }
end_scenario() { stop_agents; [ "${FAILED_SCN:-0}" -eq 1 ] && dump; FAILED_SCN=0; }
order_is() { [ "$(worked_order)" = "$1" ]; }
# run a worker until $2 dispatches happened, then report the dispatch order
drain() { start_agent "$1" "$3"; wait_for 40 claude_count_ge "$2"; sleep 0.3; }

# ===================== 1. two projects: higher priority wins over older work =====================
echo "Scenario 1: MULTI — a newer higher-priority task in project B beats older work in project A"
new_scenario
add_task A-old p1 500 2026-01-01T00:00:00Z
add_task B-hot p2 600 2026-03-01T00:00:00Z
add_task C-mid p3 550 2026-02-01T00:00:00Z
drain implement 3 multi
check "dispatch order is priority-descending across projects" order_is "B-hot,C-mid,A-old"
check "each dispatched agent was pinned to the task the harness selected" bash -c "grep -c 'pin=B-hot .*worked=B-hot' '$FAKE_DIR/claude.log' | grep -qx 1"
end_scenario

# ===================== 2. equal 500: global oldest-first, not per-project drain =====================
echo "Scenario 2: MULTI — equal default priority is oldest-first across projects (no project shuffle)"
new_scenario
add_task t3 p1 - 2026-01-03T00:00:00Z
add_task t1 p2 500 2026-01-01T00:00:00Z
add_task t2 p1 500 2026-01-02T00:00:00Z
add_task t4 p3 - 2026-01-04T00:00:00Z
drain implement 4 multi
check "order is strictly oldest-first interleaving projects" order_is "t1,t2,t3,t4"
end_scenario

echo "Scenario 2b: determinism — the same fixture is dispatched identically on repeated runs"
ok=1
for run in 1 2 3; do
  new_scenario
  add_task t3 p1 - 2026-01-03T00:00:00Z; add_task t1 p2 500 2026-01-01T00:00:00Z; add_task t2 p3 500 2026-01-02T00:00:00Z
  drain implement 3 multi
  order_is "t1,t2,t3" || ok=0
  stop_agents
done
check "three runs, same order every time (no shuffle)" test "$ok" -eq 1

# ===================== 3. equal 1001 and values above 1000: exact integers =====================
echo "Scenario 3: MULTI — equal 1001 is oldest-first; larger generated values and 1000/999 order as exact integers"
new_scenario
add_task f1001-new p1 1001 2026-01-02T00:00:00Z
add_task f1001-old p2 1001 2026-01-01T00:00:00Z
add_task f1043 p3 1043 2026-06-01T00:00:00Z
add_task m1000 p1 1000 2026-01-01T00:00:00Z
add_task m999 p2 999 2026-01-01T00:00:00Z
add_task m100 p3 100 2026-01-01T00:00:00Z
add_task big p1 90071992547409930 2026-12-01T00:00:00Z
drain implement 7 multi
check "order: big, 1043, 1001 oldest-first, 1000, 999, 100 (numeric, not lexical)" order_is "big,f1043,f1001-old,f1001-new,m1000,m999,m100"
end_scenario

echo "Scenario 3b: created_at fractions and equal timestamps break ties exactly; ID is the final tie-break"
new_scenario
add_task frac-late p1 500 2026-01-01T00:00:00.5Z
add_task whole p2 500 2026-01-01T00:00:00Z
add_task frac-mid p3 500 2026-01-01T00:00:00.25Z
add_task id-b p1 700 2026-02-01T00:00:00Z
add_task id-a p2 700 2026-02-01T00:00:00Z
drain implement 5 multi
check "same timestamp: lower task ID first; fractional seconds order after the whole second" order_is "id-a,id-b,whole,frac-mid,frac-late"
end_scenario

# ===================== 4. filtering: allowlist, kind, unavailable model =====================
echo "Scenario 4: project allowlist and task kind filter the candidate set before comparison"
new_scenario
add_task hot-blocked p3 900 2026-01-01T00:00:00Z
add_task hot-review p1 900 2026-01-01T00:00:00Z x review
add_task ok-2 p2 510 2026-01-02T00:00:00Z
add_task ok-1 p1 510 2026-01-01T00:00:00Z
export ODONIAN_PROJECTS="p1,p2"
drain implement 2 multi
sleep 1
check "a higher-priority task outside the allowlist, and a review-kind task, are never dispatched to an implementer" order_is "ok-1,ok-2"
end_scenario

echo "Scenario 4b: reviewers compare only review-kind work, with the same comparator"
new_scenario
add_task impl p1 900 2026-01-01T00:00:00Z
add_task rev-new p1 520 2026-01-02T00:00:00Z x review
add_task rev-hot p2 530 2026-03-01T00:00:00Z x review
export FAKE_KIND=review
drain review 2 multi
check "review dispatch order is priority-descending and skips implement-kind work" order_is "rev-hot,rev-new"
end_scenario

echo "Scenario 5: a task on an unavailable (failing) model is skipped; lower-priority work on a healthy model proceeds"
new_scenario
add_task down-hot p1 900 2026-01-01T00:00:00Z mdown
add_task up-low p2 100 2026-01-01T00:00:00Z mup
touch "$FAKE_DIR/claude.fail.mdown"
drain implement 2 multi
sleep 1
check "the failing model's head task was tried once, then the healthy lower-priority task was worked" bash -c "[ \"\$(grep -c 'worked=FAILED' '$FAKE_DIR/claude.log')\" = 1 ] && grep -q 'worked=up-low' '$FAKE_DIR/claude.log'"
check "the failing model is backed off: its task is not re-dispatched" test "$(grep -c 'worked=FAILED' "$FAKE_DIR/claude.log")" -eq 1
end_scenario

# ===================== 6. deferred research falls through, then is reconsidered =====================
echo "Scenario 6: a quota-denied high-priority research task is deferred; eligible work in another project progresses"
new_scenario
add_task R-hot p1 1001 2026-01-01T00:00:00Z x implement research
add_task B-low p2 500 2026-01-01T00:00:00Z
printf 'defer:60\n' > "$FAKE_DIR/claim.R-hot"
drain implement 1 multi
sleep 1
check "the build task ran" order_is "B-low"
check "no model process was started for the denied research task" bash -c "! grep -q 'R-hot' '$FAKE_DIR/claude.log'"
check "the denied task was asked about once, not re-asked inside its retry window" test "$(grep -c '^claim R-hot' "$FAKE_DIR/calls.log")" -eq 1
end_scenario

echo "Scenario 6b: a deferred head task is reconsidered once its retry window has passed"
new_scenario
add_task R-hot p1 1001 2026-01-01T00:00:00Z x implement research
printf 'defer:2\ngrant\n' > "$FAKE_DIR/claim.R-hot"
drain implement 1 multi
check "the task is admitted and launched after the window, preclaimed by the harness" bash -c "grep -q 'pre=R-hot worked=R-hot' '$FAKE_DIR/claude.log'"
end_scenario

# ===================== 7. claim races =====================
echo "Scenario 7: research claim race (exit 3) falls through to the next candidate; one process only"
new_scenario
add_task R-hot p1 900 2026-01-01T00:00:00Z x implement research
add_task B-low p2 100 2026-01-01T00:00:00Z
printf 'taken\n' > "$FAKE_DIR/claim.R-hot"
drain implement 1 multi
sleep 1
check "exactly one dispatch, for the lower-priority task, no process for the raced task" bash -c "[ \"\$(grep -c ' worked=' '$FAKE_DIR/claude.log')\" = 1 ] && grep -q 'worked=B-low' '$FAKE_DIR/claude.log'"
end_scenario

echo "Scenario 7b: non-research task stolen between listing and dispatch — the pinned agent claims nothing else"
new_scenario
add_task H p1 900 2026-01-01T00:00:00Z
add_task L p2 100 2026-01-01T00:00:00Z
: > "$FAKE_DIR/steal.H"
drain implement 2 multi
check "first dispatch was pinned to H and found nothing (it did not fall back to lower-priority L)" bash -c "sed -n 1p '$FAKE_DIR/claude.log' | grep -q 'pin=H .*worked=none'"
check "the refreshed pass then dispatched L" bash -c "sed -n 2p '$FAKE_DIR/claude.log' | grep -q 'pin=L .*worked=L'"
end_scenario

echo "Scenario 7c: two competing agents, one research task — exactly one permit and one process"
new_scenario
add_task R p1 900 2026-01-01T00:00:00Z x implement research
export FAKE_CLAUDE_SECS=1
start_agent implement multi agent-a; start_agent implement multi agent-b
wait_for 20 claude_count_ge 1; sleep 2.5
check "one permit granted" test "$(cat "$FAKE_DIR/grantcount" 2>/dev/null || echo 0)" -eq 1
check "one model process for the task" test "$(claude_count)" -eq 1
end_scenario

# ===================== 8. SINGLE mode =====================
echo "Scenario 8: SINGLE project — highest priority first regardless of listing order; pin is set and cleared per dispatch"
new_scenario
add_task a p1 500 2026-01-01T00:00:00Z
add_task b p1 1001 2026-05-01T00:00:00Z
add_task c p1 1000 2026-01-01T00:00:00Z
add_task other-project p2 2000 2026-01-01T00:00:00Z
drain implement 3 single
check "pinned project only; order 1001, 1000, 500" order_is "b,c,a"
check "each dispatch carried its own selected-task pin" bash -c "grep -q 'pin=b ' '$FAKE_DIR/claude.log' && grep -q 'pin=c ' '$FAKE_DIR/claude.log' && grep -q 'pin=a ' '$FAKE_DIR/claude.log'"
end_scenario

echo "Scenario 8b: SINGLE — equal 500 is oldest-first with ID tie-break; a research dispatch has no stale pin"
new_scenario
add_task zz p1 - 2026-01-01T00:00:00Z
add_task aa p1 - 2026-01-01T00:00:00Z
add_task old p1 - 2025-12-01T00:00:00Z x implement research
drain implement 3 single
check "order old (research, preclaimed), aa, zz" order_is "old,aa,zz"
check "the preclaimed research dispatch was not given a selected-task pin" bash -c "grep -q 'pin= pre=old worked=old' '$FAKE_DIR/claude.log'"
end_scenario

# ===================== 9. merger =====================
echo "Scenario 9: merger MULTI compares merge tasks globally; a raced head falls through in the same pass"
new_scenario
add_task M-old p1 500 2026-01-01T00:00:00Z x merge
add_task M-hot p2 700 2026-03-01T00:00:00Z x merge
add_task M-mid p3 500 2026-02-01T00:00:00Z x merge
: > "$FAKE_DIR/steal.M-hot"
start_agent merge multi merger
wait_for 20 bash -c "[ \$(grep -c '^merge ' '$FAKE_DIR/calls.log') -ge 2 ]"
sleep 0.5
merged="$(grep '^merge ' "$FAKE_DIR/calls.log" | awk '{print $2}' | paste -sd, -)"
check "M-hot was taken by another worker; the merger moved on to the oldest equal-priority tasks" test "$merged" = "M-old,M-mid"
check "no merge task was merged twice or skipped" bash -c "[ \"\$(grep -c '^merge ' '$FAKE_DIR/calls.log')\" = 2 ]"
end_scenario

echo "Scenario 9b: merger MULTI picks the highest-priority merge task across projects first"
new_scenario
add_task M-old p1 500 2026-01-01T00:00:00Z x merge
add_task M-hot p2 1001 2026-03-01T00:00:00Z x merge
start_agent merge multi merger
wait_for 20 bash -c "[ \$(grep -c '^merge ' '$FAKE_DIR/calls.log') -ge 2 ]"
sleep 0.5
check "1001 merges before older 500 work" test "$(grep '^merge ' "$FAKE_DIR/calls.log" | awk '{print $2}' | paste -sd, -)" = "M-hot,M-old"
end_scenario

echo
echo "passed: $pass_count  failed: $fail_count"
[ "$fail_count" -eq 0 ]
