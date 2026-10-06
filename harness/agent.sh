#!/usr/bin/env bash
# agent.sh — the unified Odonian fleet engine. One loop, parameterized:
#   --model <tier>            the model this agent claims + runs (e.g. haiku, opus)
#   --kind  <implement|review|merge>  implement = worker, review = reviewer, merge = merger (non-LLM).
#                                      LLM prompts resolve as prompts/<delivery_mode>/<track>/<kind>.md.
#   [slot]                    stable slot name -> persistent agent id + dedicated worktree(s)
#
# PROJECT SCOPE (from $ODONIAN_PROJECT):
#   a project uuid  -> SINGLE-project mode: pinned to that board + $ODONIAN_REPO (back-compat).
#   "all" or empty  -> MULTI-project mode: poll GET /projects?claimable=&model=&kind= (v0.4.0+),
#                      shuffle, and drain every project that has matching work — cloning each
#                      project's repo on demand and standing up a per-(slot,repo) worktree (a
#                      worktree can't span repositories). Optional $ODONIAN_PROJECTS (comma-sep
#                      ids) restricts which projects multi-mode will touch.
#                      local_commit workers/reviewers reject multi-project mode; use a UUID.
#
# Run it straight from the repo's harness/ dir. Code + prompts live next to this script (the dir is
# resolved from this script's own path, and still works if invoked via a symlink); the prompt is read
# FRESH each dispatch. STATE — env, agent ids, repo clones, worktrees — lives under $ODONIAN_HOME
# (~/.odonian) and is NOT versioned. Ctrl-C can interrupt in-flight work; it does not guarantee a drain.
#
# NOTE: assumes each repo's default branch is `main` (matches the implement prompt). master-default repos
# need the prompt parameterized — not supported yet.
#
# NOTE: requires `odonian` CLI to be on PATH for board discovery and polling.
set -uo pipefail
set -m

# --- resolve our REAL directory, even when invoked via a symlink in ~/.odonian ---
_src="${BASH_SOURCE[0]}"
while [ -h "$_src" ]; do
  _d="$(cd -P "$(dirname "$_src")" && pwd)"; _src="$(readlink "$_src")"; [[ $_src != /* ]] && _src="$_d/$_src"
done
HARNESS_DIR="$(cd -P "$(dirname "$_src")" && pwd)"

ODONIAN_HOME="${ODONIAN_HOME:-$HOME/.odonian}"
# Source the env file if present (local fleet). In k8s the config arrives via the container env and
# there is no file — so don't hard-fail when it's absent.
# shellcheck source=/dev/null
[ -f "$ODONIAN_HOME/env" ] && source "$ODONIAN_HOME/env"

# --- parse args ---
MODEL="" KIND="" SLOT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --model) MODEL="${2:?}"; shift 2 ;;
    --kind)  KIND="${2:?}";  shift 2 ;;
    -h|--help) echo "usage: agent.sh --model <tier> --kind <implement|review|merge> [slot]"; exit 0 ;;
    *) SLOT="$1"; shift ;;
  esac
done
case "${KIND:?--kind required}" in implement|review|merge) ;; *) echo "kind must be implement|review|merge" >&2; exit 1 ;; esac

export AGENT_MODEL="$MODEL"

# ============================== GLOBAL SCHEDULING ==============================
# One comparator orders every candidate the fleet can start, at every priority value: priority
# DESCENDING, task created_at ASCENDING, task ID ASCENDING. All allowed projects are compared as one
# set before anything is dispatched; there is no project shuffle and no per-project head-of-line pick.
# (The server's `next` and the TUI apply the same comparator within a project.)
#
# Candidate rows are US-separated (a non-whitespace separator, so an empty field never collapses):
#   <priority, 20-digit zero-padded><created_at, normalized><task id><model><project id><repo>
# The zero-padded priority and the fixed-width created_at make a byte-wise `LC_ALL=C sort` an exact
# integer / timestamp comparison, independent of locale and of sort's numeric parsing (priorities
# above 1000 — server-generated Move-to-front values — are ordinary integers, never special cased).
US=$'\037'
ALLOW="${ODONIAN_PROJECTS:-}"   # optional comma-separated project id allowlist (MULTI mode)
in_allow() { [ -z "$ALLOW" ] && return 0; case ",$ALLOW," in *",$1,"*) return 0 ;; *) return 1 ;; esac; }

# Sort candidate rows on stdin by the comparator above.
sort_candidates() { LC_ALL=C sort -t "$US" -k1,1r -k2,2 -k3,3; }

# Emit candidate rows for the claimable kind-$2 tasks of project $1 (repo $3 is carried through).
# The integer priority is quoted before jq sees it so a large value is never rounded through a
# float; a missing priority means the default 500. created_at is normalized to a fixed-width UTC
# form (fraction right-padded to 9 digits) because RFC 3339 trims trailing fractional zeros, which
# would otherwise misorder 12:00:00Z against 12:00:00.5Z; a stamp that isn't plain UTC is kept as is.
project_candidates() {
  local pid="$1" kind="$2" repo="${3:-}" tasks_json
  tasks_json="$(odonian tasks --project "$pid" --claimable --kind "$kind" --json 2>/dev/null)" || return 0
  printf '%s' "$tasks_json" | sed -E 's/"priority":[[:space:]]*([0-9]+)/"priority":"\1"/g' \
    | jq -r --arg pid "$pid" --arg repo "$repo" --arg us "$US" '
        def ts: . as $s
          | (capture("^(?<base>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\\.(?<frac>[0-9]+))?(Z|\\+00:00)$") // null) as $m
          | if $m == null then $s else $m.base + "." + ((($m.frac // "") + "000000000")[0:9]) + "Z" end;
        def pad: if test("^[0-9]{1,20}$") then ("00000000000000000000" + .)[-20:] else "00000000000000000500" end;
        .[]? | [ ((.priority // 500) | tostring | pad), ((.created_at // "") | ts), .id, (.model // ""), $pid, $repo ]
        | join($us)' 2>/dev/null
}

# Emit the globally sorted candidate rows of kind $1 across every allowed project that reports
# claimable work. $2 = "repo" restricts to projects with a repo (clone-based worker/reviewer MULTI).
global_candidates() {
  local kind="$1" need_repo="${2:-}" pid prepo
  while IFS="$US" read -r pid prepo; do
    [ -n "$pid" ] || continue
    in_allow "$pid" || continue
    project_candidates "$pid" "$kind" "$prepo"
  done < <(odonian projects --claimable --kind "$kind" --json 2>/dev/null \
      | jq -r --arg us "$US" --arg need "$need_repo" '.[]? | select($need != "repo" or ((.repo // "") != "")) | [.id, (.repo // "")] | join($us)' 2>/dev/null) \
    | sort_candidates
}

# ============================== MERGE-KIND (REPO-LESS) ==============================
# Merge tasks are handled via REST API (internal/forge) and need NO local repo, NO worktree.
# Run as a separate early loop before any clone/worktree setup, then exit.
if [ "$KIND" = "merge" ]; then
  ROLE="merger"
  DEFAULT_SLOT="merger-1"
  SLOT="${SLOT:-${AGENT_SLOT:-$DEFAULT_SLOT}}"

  ID_DIR="$ODONIAN_HOME/agents"; ID_FILE="$ID_DIR/$SLOT.id"
  mkdir -p "$ID_DIR"
  [ -s "$ID_FILE" ] || echo "$SLOT-$(hostname -s)-$(od -An -N3 -tx1 /dev/urandom | tr -d ' ')" > "$ID_FILE"
  export AGENT_ID="$(cat "$ID_FILE")"

  MULTI=0
  case "${ODONIAN_PROJECT:-}" in ""|all|ALL) MULTI=1 ;; esac

  STOP=0
  request_stop() {
    [ "$STOP" -eq 1 ] && return
    STOP=1
    echo "[$AGENT_ID] stop requested — exiting the $ROLE loop. Check PR and task state if a merge was interrupted."
    trap - INT TERM
  }
  trap request_stop INT TERM

  nap() { sleep "$1" & wait $! 2>/dev/null; }

  merge_one() {
    local task_id="$1"
    odonian merge "$task_id"
  }

  # ---- SINGLE-PROJECT MERGE MODE ----
  if [ "$MULTI" = 0 ]; then
    echo "[$AGENT_ID] merger (merge/SINGLE) @ project $ODONIAN_PROJECT; polling"
    while true; do
      [ "$STOP" -eq 1 ] && break
      task_id=$(odonian next --project "$ODONIAN_PROJECT" --kind merge --claim 2>/dev/null)
      if [ -n "$task_id" ]; then
        echo "[$AGENT_ID] $(date '+%H:%M:%S') merging…"
        merge_one "$task_id"
      else
        echo "[$AGENT_ID] $(date '+%H:%M:%S') nothing claimable (merge); sleeping 30s"; nap 30
      fi
    done
  else
    # ---- MULTI-PROJECT MERGE MODE ----
    echo "[$AGENT_ID] merger (merge/MULTI) @ $ODONIAN_URL${ALLOW:+ (allow: $ALLOW)}; discovering work across projects"
    while true; do
      [ "$STOP" -eq 1 ] && break
      # Compare every claimable merge task across all allowed projects as one set (priority desc,
      # created_at asc, id asc) and try them in that order. ODONIAN_SELECTED_TASK_ID pins `next` to
      # the chosen task, so a race or a newer task can never make it claim a different one: it
      # reports nothing claimable and we fall through to the next candidate.
      candidates=()
      while IFS= read -r _row; do candidates+=("$_row"); done < <(global_candidates merge)
      if [ "${#candidates[@]}" -eq 0 ]; then
        echo "[$AGENT_ID] $(date '+%H:%M:%S') no claimable merge work in any project; sleeping 30s"; nap 30; continue
      fi

      worked=0
      for _row in "${candidates[@]}"; do
        [ "$STOP" -eq 1 ] && break
        IFS="$US" read -r _ _ task_id _ pid _ <<< "$_row"
        [ -n "$task_id" ] || continue
        claimed_id=$(ODONIAN_SELECTED_TASK_ID="$task_id" odonian next --project "$pid" --kind merge --claim 2>/dev/null)
        if [ -n "$claimed_id" ]; then
          echo "[$AGENT_ID] $(date '+%H:%M:%S') merging on project [${pid:0:8}]…"
          merge_one "$claimed_id"
          worked=1
          break
        fi
      done
      [ "$STOP" -eq 1 ] && break
      [ "$worked" -eq 0 ] && { echo "[$AGENT_ID] $(date '+%H:%M:%S') candidate tasks raced away; sleeping 10s"; nap 10; }
    done
  fi
  exit 0
fi

# ============================== IMPLEMENT/REVIEW KIND (WORKTREE-DEPENDENT) ==============================
if [ "$KIND" = "review" ]; then
  ROLE="reviewer"
  if [ -n "$MODEL" ]; then
    DEFAULT_SLOT="${MODEL}-rev-1"
  else
    DEFAULT_SLOT="reviewer-1"
  fi
  SLOT="${SLOT:-${AGENT_SLOT:-$DEFAULT_SLOT}}"
else
  ROLE="worker"
  if [ -n "$MODEL" ]; then
    DEFAULT_SLOT="${MODEL}-1"
  else
    DEFAULT_SLOT="worker-1"
  fi
  SLOT="${SLOT:-${AGENT_SLOT:-$DEFAULT_SLOT}}"
fi

# Delivery mode check
DELIVERY_MODE="${ODONIAN_DELIVERY_MODE:-pull_request}"
case "$DELIVERY_MODE" in pull_request|local_commit) ;; *) echo "delivery mode must be pull_request or local_commit" >&2; exit 1 ;; esac

# The prompt is keyed on all three axes — delivery_mode, track, kind — as PATH dimensions:
#   prompts/<delivery_mode>/<track>/<kind>.md
# No special-casing: a new mode/track/kind is just a file. A combo with no prompt (e.g.
# local_commit + design) resolves to a missing path; the caller blocks the task with a note
# and sleeps before continuing, so it cannot keep selecting the same incompatible task.
get_prompt_file() {
  local track="${1:-build}"
  local kind="$2"
  echo "$HARNESS_DIR/prompts/$DELIVERY_MODE/$track/$kind.md"
}

ID_DIR="$ODONIAN_HOME/agents"; ID_FILE="$ID_DIR/$SLOT.id"
REPOS_DIR="$ODONIAN_HOME/repos"     # per-repo clones (multi-mode); LRU-pruned by prune_repos_cache() to stay under $ODONIAN_HOME's emptyDir cap
mkdir -p "$ID_DIR"
[ -s "$ID_FILE" ] || echo "$SLOT-$(hostname -s)-$(od -An -N3 -tx1 /dev/urandom | tr -d ' ')" > "$ID_FILE"
export AGENT_ID="$(cat "$ID_FILE")"

# Mode: single (a uuid) vs multi (all/empty).
MULTI=0
case "${ODONIAN_PROJECT:-}" in ""|all|ALL) MULTI=1 ;; esac

# --- graceful stop ---
STOP=0
CLAUDE_PID=""   # pid (== pgid, via `set -m`) of the in-flight `claude -p`, if any
MON_PID=""      # pid (== pgid) of the in-flight research renewal monitor, if any
request_stop() {
  [ "$STOP" -eq 1 ] && return
  STOP=1
  echo "[$AGENT_ID] stop requested — stopping the in-flight $ROLE task and exiting."
  # The in-flight claude runs in its OWN process group (`set -m`, to shield it from the terminal's
  # Ctrl-C), so a group-kill aimed at the fleet's group never reaches it. TERM its group here so it
  # winds down WITH us — otherwise a force-kill of this agent would orphan the claude.
  [ -n "$CLAUDE_PID" ] && kill -TERM "-$CLAUDE_PID" 2>/dev/null || true
  trap - INT TERM
}
trap request_stop INT TERM

# --- helpers ---
norm_repo() { echo "$1" | sed -E 's#^(https://|git@)github\.com[:/]##; s#\.git$##'; }   # -> owner/repo
repo_slug() { norm_repo "$1" | tr '/' '-'; }                                            # -> owner-repo

# --- per-owner GitHub auth ---
# ~/.odonian/forge-tokens optionally pairs a repo OWNER with a PAT (lines: "owner=token"; # comments
# ok). The worker uses the matching token to clone/push/gh for that owner's repos; with no entry it
# falls back to the operator's default gh auth. Git creds stay in the worker, never in Odonian.
# (chmod 600 it — it holds tokens.)
FORGE_TOKENS="${FORGE_TOKENS:-$ODONIAN_HOME/forge-tokens}"
token_for_owner() {
  [ -f "$FORGE_TOKENS" ] || return 0
  # case-insensitive owner match — GitHub owners are case-insensitive (fAIctory == faictory), and the
  # owner derived from the repo URL may differ in case from the forge-tokens entry.
  local val
  val=$(sed -E 's/[[:space:]]*#.*$//' "$FORGE_TOKENS" 2>/dev/null \
        | grep -iE "^[[:space:]]*$1[[:space:]]*=" | head -1 | sed -E 's/^[^=]*=[[:space:]]*//; s/[[:space:]]*$//')
  # tolerate a token wrapped in surrounding quotes ("ghp_…" or 'ghp_…') — a common copy-paste slip
  # that yields HTTP 401 Bad credentials otherwise.
  val="${val#[\"\']}"; val="${val%[\"\']}"
  printf '%s' "$val"
}
# Set GH_TOKEN for a repo owner from the map, or fall back to the operator's default gh auth.
apply_owner_token() {
  local tok; tok="$(token_for_owner "$1")"
  if [ -n "$tok" ]; then export GH_TOKEN="$tok"; else unset GH_TOKEN 2>/dev/null || true; fi
}

# Ensure a local clone of a repo (clone on first sight, fetch otherwise); echo the clone dir.
ensure_clone() {
  local repo="$1" owner_repo slug clone
  owner_repo="$(norm_repo "$repo")"; slug="$(repo_slug "$repo")"; clone="$REPOS_DIR/$slug"
  if [ ! -d "$clone/.git" ]; then
    mkdir -p "$REPOS_DIR"
    gh repo clone "$owner_repo" "$clone" -- --quiet 2>/dev/null \
      || git clone --quiet "https://github.com/$owner_repo.git" "$clone" 2>/dev/null \
      || { echo "[$AGENT_ID] clone failed: $owner_repo" >&2; return 1; }
  fi
  git -C "$clone" fetch origin --quiet 2>/dev/null || true
  # LRU key for prune_repos_cache() — a dedicated marker, NOT the clone dir's own mtime, which
  # routine git operations (fetch, worktree add/prune) touch and would make the ordering meaningless.
  touch "$clone/.odonian-last-used" 2>/dev/null || true
  # If a per-owner token is set, tokenize the remote so the worker's git push/fetch authenticate as
  # that owner (gh commands read GH_TOKEN from the env). No token -> leave the plain remote (default auth).
  if [ -n "${GH_TOKEN:-}" ]; then
    git -C "$clone" remote set-url origin "https://x-access-token:${GH_TOKEN}@github.com/$owner_repo.git" 2>/dev/null || true
  fi
  echo "$clone"
}

# Prune REPOS_DIR (the per-repo clone cache) so $ODONIAN_HOME stays under its emptyDir cap — the
# cache is otherwise unbounded and multi-project reviewers (ODONIAN_PROJECT=all) accumulate a
# clone of every repo they ever review, which is what filled the 20Gi emptyDir and got 37 pods
# evicted with "Usage of EmptyDir volume home exceeds the limit" on 2026-08-07.
#
# Cheap in the common case: one `du -sk` and an immediate return when usage is under the high-water
# mark. Only when over does it walk REPOS_DIR, oldest-.odonian-last-used-first, deleting clones
# one at a time (re-measuring after each) until usage is back at/under the low-water mark. $1 is
# the clone this dispatch is about to use — it is skipped so pruning can never corrupt a running
# task's worktree. Never raises: any failure here is logged and the dispatch proceeds regardless.
prune_repos_cache() {
  local keep="$1" high_kib low_kib usage_kib
  high_kib=$(( ${ODONIAN_REPOS_HIGH_GIB:-14} * 1024 * 1024 ))
  low_kib=$(( ${ODONIAN_REPOS_LOW_GIB:-8} * 1024 * 1024 ))

  usage_kib=$(du -sk "$ODONIAN_HOME" 2>/dev/null | cut -f1)
  [ -n "${usage_kib:-}" ] || return 0
  [ "$usage_kib" -le "$high_kib" ] && return 0
  [ -d "$REPOS_DIR" ] || return 0

  # Build "<lru-key>\t<dir>" rows, oldest first; a clone with no marker sorts oldest (key 0).
  local d key marker rows
  rows=""
  for d in "$REPOS_DIR"/*/; do
    d="${d%/}"
    [ -d "$d" ] || continue
    [ "$d" = "$keep" ] && continue
    marker="$d/.odonian-last-used"
    key=""
    [ -e "$marker" ] && key=$(stat -c %Y "$marker" 2>/dev/null || stat -f %m "$marker" 2>/dev/null)
    rows="$rows${key:-0}	$d
"
  done
  [ -n "$rows" ] || return 0

  local dir size_kib freed_mb
  while IFS=$'\t' read -r _ dir; do
    [ -n "$dir" ] || continue
    usage_kib=$(du -sk "$ODONIAN_HOME" 2>/dev/null | cut -f1)
    [ -n "${usage_kib:-}" ] || break
    [ "$usage_kib" -le "$low_kib" ] && break
    [ -d "$dir" ] || continue
    size_kib=$(du -sk "$dir" 2>/dev/null | cut -f1)
    freed_mb=$(( ${size_kib:-0} / 1024 ))
    rm -rf "$dir" 2>/dev/null || { echo "[$AGENT_ID] prune failed: $(basename "$dir")" >&2; continue; }
    echo "[$AGENT_ID] pruned clone: $(basename "$dir") (freed $freed_mb MB)"
  done < <(printf '%s' "$rows" | sort -n)
  return 0
}

# Ensure this slot's detached worktree for a given clone; echo the worktree dir.
ensure_worktree() {
  local clone="$1" wt
  wt="$ODONIAN_HOME/wt-$SLOT-$(basename "$clone")"
  git -C "$clone" worktree prune 2>/dev/null || true
  [ -e "$wt" ] && { git -C "$clone" worktree remove --force "$wt" 2>/dev/null || rm -rf "$wt"; }
  git -C "$clone" worktree add --detach "$wt" origin/main --quiet 2>/dev/null \
    || { echo "[$AGENT_ID] worktree add failed for $(basename "$clone")" >&2; return 1; }
  echo "$wt"
}

# --- per-model backend availability (bash 3.2: parallel indexed arrays, no assoc arrays) ---
# A dispatch failure (claude/codex exits non-zero — out of credits, auth, spend limit) is a
# property of the MODEL's backend, not of this pod or the task it was trying. Tracking it per
# model (instead of one pod-wide failure counter) means a failing Claude backend never delays
# codex-routed (or any other model's) work: only that model's tasks are skipped, for an
# escalating-but-capped window, until it is retried.
FAIL_MODEL_NAMES=()
FAIL_MODEL_UNTIL=()   # epoch seconds when the matching model's backoff window ends
FAIL_MODEL_COUNTS=()  # consecutive failures for that model, for escalating backoff

# Echo the index of $1 in FAIL_MODEL_NAMES, or nothing (rc=1) if not tracked.
_fail_model_index() {
  local i
  for i in "${!FAIL_MODEL_NAMES[@]}"; do
    [ "${FAIL_MODEL_NAMES[$i]}" = "$1" ] && { echo "$i"; return 0; }
  done
  return 1
}

_fail_model_drop() {
  local i="$1"
  unset 'FAIL_MODEL_NAMES[i]' 'FAIL_MODEL_UNTIL[i]' 'FAIL_MODEL_COUNTS[i]'
  FAIL_MODEL_NAMES=("${FAIL_MODEL_NAMES[@]}")
  FAIL_MODEL_UNTIL=("${FAIL_MODEL_UNTIL[@]}")
  FAIL_MODEL_COUNTS=("${FAIL_MODEL_COUNTS[@]}")
}

# True (rc=0) if model $1 is currently within a failure backoff window — the caller should skip
# claimable tasks pinned to it. Clears the record and logs a retry line once the window elapses,
# so the model becomes a normal candidate again on the very next check.
model_unavailable() {
  local m="$1" i now
  i="$(_fail_model_index "$m")" || return 1
  now=$(date +%s)
  if [ "$now" -lt "${FAIL_MODEL_UNTIL[$i]}" ]; then
    return 0
  fi
  echo "[$AGENT_ID] $(date '+%H:%M:%S') model $m backoff window elapsed; retrying" >&2
  _fail_model_drop "$i"
  return 1
}

# Record a dispatch failure attributable to model $1's backend; escalating capped backoff,
# mirroring the previous pod-wide CLAUDE_FAILS scheme but scoped to just this model.
mark_model_unavailable() {
  local m="$1" i count backoff now
  i="$(_fail_model_index "$m")"
  count=1
  [ -n "$i" ] && count=$((${FAIL_MODEL_COUNTS[$i]} + 1))
  backoff=$((count * 30)); [ "$backoff" -gt 300 ] && backoff=300
  now=$(date +%s)
  if [ -n "$i" ]; then
    FAIL_MODEL_UNTIL[$i]=$((now + backoff)); FAIL_MODEL_COUNTS[$i]=$count
  else
    FAIL_MODEL_NAMES+=("$m"); FAIL_MODEL_UNTIL+=("$((now + backoff))"); FAIL_MODEL_COUNTS+=("$count")
  fi
  echo "[$AGENT_ID] $(date '+%H:%M:%S') model $m backend unavailable (consecutive failures=$count); skipping its tasks for ${backoff}s" >&2
}

# Self-heal on a successful dispatch: this model's backend works again.
clear_model_failures() {
  local i
  i="$(_fail_model_index "$1")" || return 0
  _fail_model_drop "$i"
}

# --- research admission (P5) -------------------------------------------------------------------
# Research-track tasks are paced: the harness must be admitted for the EXACT task BEFORE any model
# process exists. Admission is `odonian claim <id>`, which atomically checks eligibility, debits a
# start from the pool, creates the durable permit/attempt, and claims the task. Only research-track
# tasks take this path; build/design/merge dispatch is untouched (the agent still claims for itself).
#
# State of the admission currently held (set by admit_research_task, cleared by clear_preclaim):
P_TASK="" P_ATTEMPT="" P_PERMIT="" P_REQUEST="" P_MODEL=""
SELECTED_TASK_ID=""   # task the loop selected for a legacy (non-preclaimed) dispatch; see dispatch()
ADM_STATE=""    # granted | deferred | busy | lost | ambiguous (outcome of the last admit_research_task)

clear_preclaim() { P_TASK="" P_ATTEMPT="" P_PERMIT="" P_REQUEST="" P_MODEL=""; }

# Deferred tasks: a denied task is skipped by pick_claimable_task until its retry time passes, so
# both discovery loops move on to other eligible tasks/projects/models instead of re-asking.
# Parallel indexed arrays (bash 3.2: no associative arrays).
DEFER_IDS=()
DEFER_UNTIL=()

defer_task() {
  local id="$1" secs="$2" expiry i
  expiry=$(( $(date +%s) + secs ))
  for ((i = 0; i < ${#DEFER_IDS[@]}; i++)); do
    if [ "${DEFER_IDS[$i]}" = "$id" ]; then DEFER_UNTIL[$i]=$expiry; return 0; fi
  done
  DEFER_IDS+=("$id"); DEFER_UNTIL+=("$expiry")
}

# True (rc=0) while task $1 is inside its deferral window. Read-only so it is safe in $(...).
task_deferred() {
  local id="$1" now i
  now=$(date +%s)
  for ((i = 0; i < ${#DEFER_IDS[@]}; i++)); do
    [ "${DEFER_IDS[$i]}" = "$id" ] && [ "${DEFER_UNTIL[$i]}" -gt "$now" ] && return 0
  done
  return 1
}

prune_deferred() {
  local now i ids=() untils=()
  now=$(date +%s)
  for ((i = 0; i < ${#DEFER_IDS[@]}; i++)); do
    if [ "${DEFER_UNTIL[$i]}" -gt "$now" ]; then ids+=("${DEFER_IDS[$i]}"); untils+=("${DEFER_UNTIL[$i]}"); fi
  done
  DEFER_IDS=(); DEFER_UNTIL=()
  for ((i = 0; i < ${#ids[@]}; i++)); do DEFER_IDS+=("${ids[$i]}"); DEFER_UNTIL+=("${untils[$i]}"); done
}

# Echo how long an idle loop should sleep: $1 (the usual nap) shortened to the earliest deferral
# expiry (at least 1s), so a deferred task is re-asked when it is allowed, not 30s later.
idle_nap() {
  local now i remaining best="$1"
  now=$(date +%s)
  for ((i = 0; i < ${#DEFER_IDS[@]}; i++)); do
    remaining=$(( ${DEFER_UNTIL[$i]} - now ))
    [ "$remaining" -lt 1 ] && remaining=1
    [ "$remaining" -lt "$best" ] && best=$remaining
  done
  echo "$best"
}

# Ambiguous admissions: when a claim ends without a definite answer (transport error, timeout) the
# server may or may not have admitted us. Remember that request id per task so the next attempt
# replays the SAME request (the server returns the original admission instead of spending another
# start). Cleared as soon as any definite answer arrives.
AMBIG_IDS=()
AMBIG_REQS=()
ambig_get() {
  local i
  for ((i = 0; i < ${#AMBIG_IDS[@]}; i++)); do
    [ "${AMBIG_IDS[$i]}" = "$1" ] && { echo "${AMBIG_REQS[$i]}"; return 0; }
  done
  return 0
}
ambig_clear() {
  local i ids=() reqs=()
  for ((i = 0; i < ${#AMBIG_IDS[@]}; i++)); do
    [ "${AMBIG_IDS[$i]}" = "$1" ] || { ids+=("${AMBIG_IDS[$i]}"); reqs+=("${AMBIG_REQS[$i]}"); }
  done
  AMBIG_IDS=(); AMBIG_REQS=()
  for ((i = 0; i < ${#ids[@]}; i++)); do AMBIG_IDS+=("${ids[$i]}"); AMBIG_REQS+=("${reqs[$i]}"); done
}
ambig_set() { ambig_clear "$1"; AMBIG_IDS+=("$1"); AMBIG_REQS+=("$2"); }

# RFC 3339 UTC timestamp -> epoch seconds (GNU date, then BSD date). Fails (rc=1) if unparseable.
rfc3339_epoch() {
  local t="$1" e
  e=$(date -u -d "$t" +%s 2>/dev/null) && [ -n "$e" ] && { echo "$e"; return 0; }
  t="${t%%.*}"; t="${t%Z}"
  e=$(date -u -j -f '%Y-%m-%dT%H:%M:%S' "$t" +%s 2>/dev/null) && [ -n "$e" ] && { echo "$e"; return 0; }
  return 1
}

# Seconds to wait after a denial. $1 is the CLI's stderr: `retry-after: N` (an active dispatch must
# finish first) and/or `not-before: <RFC 3339>` (time alone unblocks it). Honors the later of the
# two, clamped to [1, 900]; 30 when the server sent no hint.
deferral_secs() {
  local errf="$1" retry nb secs=0 now e
  retry=$(sed -n 's/^retry-after:[[:space:]]*//p' "$errf" | head -1 | tr -cd '0-9')
  nb=$(sed -n 's/^not-before:[[:space:]]*//p' "$errf" | head -1 | tr -d '[:space:]')
  [ -n "$retry" ] && secs=$retry
  if [ -n "$nb" ] && e=$(rfc3339_epoch "$nb"); then
    now=$(date +%s)
    [ $((e - now + 1)) -gt "$secs" ] && secs=$((e - now + 1))
  fi
  [ "$secs" -eq 0 ] && secs=30
  [ "$secs" -lt 1 ] && secs=1
  [ "$secs" -gt 900 ] && secs=900
  echo "$secs"
}

# Request admission for research task $1 on model $2. Sets ADM_STATE and, when granted, P_*:
#   granted    — task is claimed by us (P_PERMIT/P_ATTEMPT are set when the server issued a permit; they
#                are empty when the policy is disabled/not enforcing and the claim carried no permit).
#   deferred   — pool denied (exit 10): task untouched, deferred for the server's retry hint; no model.
#   busy       — conflict (exit 11, e.g. a live attempt still holds the task); deferred 30s.
#   lost       — another worker claimed it first (exit 3).
#   ambiguous  — no definite answer after retries; the request id is remembered and replayed later.
# Nothing here ever launches a model.
admit_research_task() {
  local task_id="$1" model="$2" req out rc tries=0 errf secs max_tries nap_secs
  max_tries="${ODONIAN_ADMISSION_TRIES:-3}"; nap_secs="${ODONIAN_ADMISSION_RETRY_NAP:-2}"
  ADM_STATE=""
  clear_preclaim
  req="$(ambig_get "$task_id")"
  [ -n "$req" ] || req="$AGENT_ID:$task_id:$(date +%s)-$$-$RANDOM"
  errf="$(mktemp "${TMPDIR:-/tmp}/odonian-claim.XXXXXX")"
  while :; do
    out="$(odonian claim "$task_id" --agent "$AGENT_ID" --model "$model" --request-id "$req" 2>"$errf")"; rc=$?
    case "$rc" in
      0)
        ambig_clear "$task_id"
        P_TASK="$task_id"; P_MODEL="$model"
        P_PERMIT="$(printf '%s' "$out" | jq -r '.permit_id // empty' 2>/dev/null)"
        P_ATTEMPT="$(printf '%s' "$out" | jq -r '.attempt_id // empty' 2>/dev/null)"
        P_REQUEST="$(printf '%s' "$out" | jq -r '.request_id // empty' 2>/dev/null)"
        [ -n "$P_REQUEST" ] || P_REQUEST="$req"
        if [ -z "$P_PERMIT" ] || [ -z "$P_ATTEMPT" ]; then P_PERMIT=""; P_ATTEMPT=""; fi
        ADM_STATE=granted; rm -f "$errf"; return 0 ;;
      10)
        secs="$(deferral_secs "$errf")"
        ambig_clear "$task_id"; defer_task "$task_id" "$secs"
        echo "[$AGENT_ID] $(date '+%H:%M:%S') research admission deferred for ${task_id:0:8} ($(tr '\n' ' ' <"$errf" | sed 's/ *$//')); retry in ${secs}s" >&2
        ADM_STATE=deferred; rm -f "$errf"; return 1 ;;
      3)
        ambig_clear "$task_id"; defer_task "$task_id" 5
        ADM_STATE=lost; rm -f "$errf"; return 1 ;;
      11)
        ambig_clear "$task_id"; defer_task "$task_id" 30
        echo "[$AGENT_ID] $(date '+%H:%M:%S') research admission conflict for ${task_id:0:8}: $(tr '\n' ' ' <"$errf" | sed 's/ *$//')" >&2
        ADM_STATE=busy; rm -f "$errf"; return 1 ;;
      *)
        tries=$((tries + 1))
        if [ "$tries" -ge "$max_tries" ] || [ "$STOP" -eq 1 ]; then
          ambig_set "$task_id" "$req"; defer_task "$task_id" "${ODONIAN_AMBIGUOUS_RETRY_SECS:-30}"
          echo "[$AGENT_ID] $(date '+%H:%M:%S') research admission for ${task_id:0:8} ambiguous (rc=$rc); will replay request $req" >&2
          ADM_STATE=ambiguous; rm -f "$errf"; return 1
        fi
        nap "$nap_secs" ;;
    esac
  done
}

# Finalize the held permit with exit class $1 (completed|failed|cancelled|unknown). Idempotent: a
# permit that is already finalized/fenced/expired answers 409 (exit 11), which is final. A transport
# failure is retried; if it never lands the permit lapses at its lease expiry and the start stays
# debited (uncertain outcomes are never refunded). Clears the held admission either way.
finalize_permit() {
  local class="$1" i rc
  if [ -n "$P_PERMIT" ]; then
    for i in 1 2 3; do
      odonian permit-finalize "$P_PERMIT" --task-id "$P_TASK" --model "$P_MODEL" --agent-id "$AGENT_ID" \
        --request-id "$P_REQUEST" --attempt-id "$P_ATTEMPT" --exit-class "$class" >/dev/null 2>&1; rc=$?
      case "$rc" in 0|11) break ;; esac
      [ "$i" -lt 3 ] && nap 1
    done
    [ "$rc" -ne 0 ] && [ "$rc" -ne 11 ] && echo "[$AGENT_ID] $(date '+%H:%M:%S') could not finalize permit $P_PERMIT (rc=$rc); it will lapse at lease expiry" >&2
  fi
  clear_preclaim
  return 0
}

# Renewal + ownership monitor for one preclaimed research dispatch; runs as a background subshell
# next to the model process and ends when that process does. Every interval it
#   - heartbeats the task lease (fenced to the attempt) while this agent still OWNS the task
#     (in_progress and assigned to us). Once the model submits, ownership ends: heartbeats stop but
#     nothing else changes — the model keeps running and writing its final output;
#   - renews the dispatch permit regardless, so dispatch capacity stays held until the process exits
#     (finalization is the main shell's job, on process exit).
# Ownership is LOST — and the stale process fenced (TERM its group, KILL after a grace) — only on
# positive evidence: the attempt was fenced/expired/finalized under us (409 from heartbeat or permit
# renewal), or the task is in_progress under a different agent. Transient errors never kill work,
# and neither does rate exhaustion. NOTE the lease is a coordination bound, not proof the process
# stopped: a partitioned harness cannot fence what it cannot reach, but the server still rejects
# that process's heartbeat/submit by attempt id.
monitor_dispatch() {
  local pid="$1" ctl="$2" poll interval grace last now owned=1 err rc state assignee lost="" i
  trap - EXIT INT TERM
  poll="${ODONIAN_RENEW_POLL_SECS:-1}"; interval="${ODONIAN_RENEW_INTERVAL_SECS:-20}"; grace="${ODONIAN_FENCE_GRACE_SECS:-5}"
  last=$(date +%s)
  while kill -0 "$pid" 2>/dev/null; do
    sleep "$poll"
    kill -0 "$pid" 2>/dev/null || break
    now=$(date +%s)
    [ $((now - last)) -lt "$interval" ] && continue
    last=$now
    if [ "$owned" -eq 1 ]; then
      if err="$(odonian heartbeat "$P_TASK" --agent "$AGENT_ID" ${P_ATTEMPT:+--attempt "$P_ATTEMPT"} 2>&1 >/dev/null)"; then
        :
      else
        case "$err" in
          *ATTEMPT_FENCED*|*ATTEMPT_EXPIRED*|*ATTEMPT_FINALIZED*) lost="task attempt fenced ($err)" ;;
          *)
            if json="$(odonian show "$P_TASK" --json 2>/dev/null)" && [ -n "$json" ]; then
              state="$(printf '%s' "$json" | jq -r '.state // empty' 2>/dev/null)"
              assignee="$(printf '%s' "$json" | jq -r '.assignee // empty' 2>/dev/null)"
              if [ "$state" = "in_progress" ] && [ -n "$assignee" ] && [ "$assignee" != "$AGENT_ID" ]; then
                lost="task now owned by $assignee"
              elif [ "$state" = "in_progress" ]; then
                echo "[$AGENT_ID] heartbeat for ${P_TASK:0:8} failed transiently: $err" >&2
              elif [ -n "$state" ]; then
                owned=0   # submitted (or otherwise moved on): stop renewing the lease, keep the process
                echo "[$AGENT_ID] task ${P_TASK:0:8} no longer in_progress (state=$state); lease renewal stopped, permit renewal continues" >&2
              fi
            else
              echo "[$AGENT_ID] heartbeat for ${P_TASK:0:8} failed and task lookup failed: $err" >&2
            fi ;;
        esac
      fi
    fi
    if [ -z "$lost" ] && [ -n "$P_PERMIT" ]; then
      odonian permit-renew "$P_PERMIT" --task-id "$P_TASK" --model "$P_MODEL" --agent-id "$AGENT_ID" \
        --request-id "$P_REQUEST" --attempt-id "$P_ATTEMPT" >/dev/null 2>"$ctl/renew.err"; rc=$?
      if [ "$rc" -eq 11 ]; then
        lost="permit lost ($(tr '\n' ' ' <"$ctl/renew.err"))"
      elif [ "$rc" -ne 0 ]; then
        echo "[$AGENT_ID] permit renewal for ${P_PERMIT:0:8} failed transiently (rc=$rc)" >&2
      fi
    fi
    if [ -n "$lost" ]; then
      echo "[$AGENT_ID] $(date '+%H:%M:%S') ownership of ${P_TASK:0:8} lost: $lost — stopping the stale process" >&2
      : > "$ctl/fenced"
      kill -TERM "-$pid" 2>/dev/null
      for ((i = 0; i < grace * 10; i++)); do kill -0 "$pid" 2>/dev/null || exit 0; sleep 0.1; done
      kill -KILL "-$pid" 2>/dev/null
      exit 0
    fi
  done
  exit 0
}

# Select the first claimable task of kind $2 in project $1 under the shared comparator (priority
# desc, created_at asc, id asc) whose model is not currently in a failure backoff window and which
# is not locally deferred, skipping past any head task(s) that are. Echoes "id<TAB>model" of the
# chosen task, or nothing if the project has no such task (the caller then falls back to its normal
# "nothing claimable" nap — the agent only backs off entirely in that case).
pick_claimable_task() {
  local project="$1" kind="$2" id model
  while IFS="$US" read -r _ _ id model _ _; do
    [ -n "$id" ] || continue
    model_unavailable "$model" && continue
    task_deferred "$id" && continue
    printf '%s\t%s\n' "$id" "$model"
    return 0
  done < <(project_candidates "$project" "$kind" | sort_candidates)
  return 0
}

# Run one claude task, shielded from Ctrl-C, waiting until it truly finishes. Captures claude's
# exit code: a non-zero exit means claude could not run (out of credits, auth, missing binary) —
# NOT that the task is bad. On failure this marks $AGENT_MODEL's backend unavailable (see above)
# rather than sleeping the whole pod, so a pass immediately following a failure can still
# dispatch a different, healthy model's claimable work instead of idling out the backoff.
dispatch() {
  # Substitute the __AGENT_MODEL__ placeholder with this dispatch's actual model so the agent
  # self-identifies correctly (e.g. review prompts sign PR comments "<model>-reviewer:" instead of
  # a hardcoded "opus-reviewer:"). sed (not envsubst) for portability; the token is unique so other
  # $VARS in the prompt are left for the agent's own shell to expand at runtime.
  local prompt; prompt="$(sed "s/__AGENT_MODEL__/$AGENT_MODEL/g" "$PROMPT_FILE")"

  # Export ODONIAN_MODEL for the dispatched agent to use (pr-feedback ack marker default is ${ODONIAN_MODEL:-fleet}-worker:)
  export ODONIAN_MODEL="$AGENT_MODEL"

  # Preclaimed contract (research): the harness already claimed the task, so the prompt must skip
  # next/claim and fence its heartbeat/submit to the admitted attempt. Always reset first — these
  # are exported and must never leak from one dispatch into the next (legacy) one.
  unset ODONIAN_PRECLAIMED_TASK_ID ODONIAN_PRECLAIMED_ATTEMPT_ID ODONIAN_SELECTED_TASK_ID
  if [ -n "$P_TASK" ]; then
    export ODONIAN_PRECLAIMED_TASK_ID="$P_TASK"
    [ -n "$P_ATTEMPT" ] && export ODONIAN_PRECLAIMED_ATTEMPT_ID="$P_ATTEMPT"
  elif [ -n "${SELECTED_TASK_ID:-}" ]; then
    # Legacy (build/design/review) dispatch: the agent still runs `odonian next` + `claim` itself,
    # but `next` is pinned to the task this harness selected under the global comparator. If that
    # task was raced away `next` reports nothing claimable; it never falls back to another task.
    export ODONIAN_SELECTED_TASK_ID="$SELECTED_TASK_ID"
  fi

  # Check if AGENT_MODEL is in AGENT_CODEX_MODELS (comma-separated list)
  local use_codex=0
  if [ -n "${AGENT_CODEX_MODELS:-}" ]; then
    case ",$AGENT_CODEX_MODELS," in
      *",$AGENT_MODEL,"*) use_codex=1 ;;
    esac
  fi

  if [ "$use_codex" -eq 1 ]; then
    # shellcheck disable=SC2086
    codex exec -m "$AGENT_MODEL" --sandbox danger-full-access -c model_reasoning_effort=high ${AGENT_CODEX_FLAGS:-} "$prompt" &
  else
    # >>> remove --dangerously-skip-permissions if you want interactive permission prompts <<<
    # AGENT_CLAUDE_FLAGS appends extra flags to the nested claude (default empty, so the normal fleet
    # is unchanged). sbx.sh sets it to --allow-dangerously-skip-permissions, which a NESTED `claude -p`
    # requires inside an sbx sandbox; it is unquoted on purpose so multiple flags word-split.
    # shellcheck disable=SC2086
    claude -p --dangerously-skip-permissions ${AGENT_CLAUDE_FLAGS:-} "$prompt" --model "$AGENT_MODEL" &
  fi

  CLAUDE_PID=$!   # tracked so request_stop()/cleanup() can tear down this claude's process group
  local pid=$CLAUDE_PID rc=0 ctl="" fenced=0
  # Preclaimed research: a sibling monitor renews the permit/task lease for the life of the process.
  if [ -n "$P_TASK" ]; then
    ctl="$(mktemp -d "${TMPDIR:-/tmp}/odonian-dispatch.XXXXXX")"
    monitor_dispatch "$pid" "$ctl" &
    MON_PID=$!
  fi
  # Wait unconditionally first: the child may already be gone, and wait still yields its status. The
  # loop re-waits when a trapped signal interrupts wait while the child is still alive.
  wait "$pid"; rc=$?
  while kill -0 "$pid" 2>/dev/null; do wait "$pid"; rc=$?; done
  CLAUDE_PID=""
  if [ -n "$MON_PID" ]; then
    kill -TERM "-$MON_PID" 2>/dev/null || true
    wait "$MON_PID" 2>/dev/null || true
    MON_PID=""
    [ -e "$ctl/fenced" ] && fenced=1
    rm -rf "$ctl"
  fi
  unset ODONIAN_PRECLAIMED_TASK_ID ODONIAN_PRECLAIMED_ATTEMPT_ID ODONIAN_SELECTED_TASK_ID
  # Finalize the research permit exactly once, now that the process is truly gone (never earlier:
  # a submit ends task ownership, not the dispatch). rc 126/127 = the model binary could not be
  # launched at all; that and any non-zero exit are `failed`, a fenced/stopped run `cancelled`.
  if [ -n "$P_TASK" ]; then
    local exit_class=failed
    if [ "$fenced" -eq 1 ]; then exit_class=cancelled
    elif [ "$rc" -eq 0 ]; then exit_class=completed
    elif [ "$STOP" -eq 1 ]; then exit_class=cancelled
    elif [ "$rc" -eq 126 ] || [ "$rc" -eq 127 ]; then echo "[$AGENT_ID] $(date '+%H:%M:%S') launch error (rc=$rc) for research task ${P_TASK:0:8}" >&2
    fi
    finalize_permit "$exit_class"
    clear_preclaim
  fi
  # If we're shutting down, the non-zero rc is our own TERM of claude — don't treat it as a credit
  # failure and don't back off; just unwind so the loop can exit promptly. Likewise a fenced
  # (stale-ownership) stop is our own kill, not a backend failure.
  [ "$STOP" -eq 1 ] && return "$rc"
  [ "$fenced" -eq 1 ] && return "$rc"
  if [ "$rc" -ne 0 ]; then
    echo "[$AGENT_ID] $(date '+%H:%M:%S') dispatch exited rc=$rc (likely out of credits/auth) for model $AGENT_MODEL" >&2
    mark_model_unavailable "$AGENT_MODEL"
  else
    clear_model_failures "$AGENT_MODEL"
  fi
  return "$rc"
}

nap() { sleep "$1" & wait $! 2>/dev/null; }

# Cleanup: drop ALL of this slot's worktrees (single wt-$SLOT and multi wt-$SLOT-*), prune clones.
cleanup() {
  # Backstop: if a claude dispatch is still tracked when we exit (force-quit, error), KILL its
  # process group so it can't outlive us as an orphan. (Cannot run if WE are SIGKILLed — that's why
  # request_stop TERMs it on the graceful path.)
  [ -n "$CLAUDE_PID" ] && kill -KILL "-$CLAUDE_PID" 2>/dev/null || true
  [ -n "$MON_PID" ] && kill -KILL "-$MON_PID" 2>/dev/null || true
  # Same backstop for an admitted research permit that never reached its normal finalization (an exit
  # between admission and dispatch, or mid-dispatch): the process is gone/killed, so release the slot.
  [ -n "$P_TASK" ] && finalize_permit cancelled
  echo "[$AGENT_ID] cleaning up worktrees for slot $SLOT"
  for wt in "$ODONIAN_HOME/wt-$SLOT" "$ODONIAN_HOME"/wt-"$SLOT"-*; do
    [ -e "$wt" ] && rm -rf "$wt"
  done
  for clone in "$REPOS_DIR"/* "${ODONIAN_MAIN_REPO:-${ODONIAN_REPO:-/nonexistent}}"; do
    [ -d "$clone/.git" ] && git -C "$clone" worktree prune 2>/dev/null || true
  done
}
trap cleanup EXIT

# ============================== SINGLE-PROJECT MODE ==============================
if [ "$MULTI" = 0 ]; then
  WT=""  # Initialize WT; set only in pull_request mode
  # local_commit mode: use ODONIAN_REPO directly (CLI-managed worktree), skip clone
  if [ "$DELIVERY_MODE" = "local_commit" ]; then
    : "${ODONIAN_WORKTREE_HOME:?ODONIAN_WORKTREE_HOME required for local_commit mode}"
    MAIN_REPO="$ODONIAN_REPO"
    export ODONIAN_WORKTREE_HOME
    cd "$MAIN_REPO" || { echo "[$AGENT_ID] failed to cd to ODONIAN_REPO ($MAIN_REPO)" >&2; exit 1; }
    AGENT_MODEL_STR="${MODEL:+$MODEL/}$KIND"
    echo "[$AGENT_ID] $ROLE ($AGENT_MODEL_STR) SINGLE (local_commit) @ project $ODONIAN_PROJECT @ $MAIN_REPO; polling"
  else
    # pull_request mode: standard clone + worktree setup
    MAIN_REPO="${ODONIAN_MAIN_REPO:-$ODONIAN_REPO}"
    WT="$ODONIAN_HOME/wt-$SLOT"
    # Guard: refuse if MAIN_REPO doesn't match the pinned project's repo.
    _proj_repo=$(odonian project "$ODONIAN_PROJECT" --json | jq -r '.repo // ""')
    _origin=$(git -C "$MAIN_REPO" remote get-url origin 2>/dev/null || echo "")
    if [ -n "$_proj_repo" ] && [ "$(norm_repo "$_proj_repo")" != "$(norm_repo "$_origin")" ]; then
      echo "[$AGENT_ID] REFUSING: project $ODONIAN_PROJECT repo is '$(norm_repo "$_proj_repo")' but ODONIAN_REPO ($MAIN_REPO) points at '$(norm_repo "$_origin")'." >&2
      exit 1
    fi
    [ -n "$_proj_repo" ] && apply_owner_token "$(norm_repo "$_proj_repo" | cut -d/ -f1)"   # gh auth for the pinned project's owner
    git -C "$MAIN_REPO" fetch origin --quiet || true
    git -C "$MAIN_REPO" worktree prune
    [ -e "$WT" ] && { git -C "$MAIN_REPO" worktree remove --force "$WT" 2>/dev/null || rm -rf "$WT"; }
    git -C "$MAIN_REPO" worktree add --detach "$WT" origin/main
    export ODONIAN_REPO="$WT"; cd "$WT" || { echo "worktree cd failed"; exit 1; }
    AGENT_MODEL_STR="${MODEL:+$MODEL/}$KIND"
    echo "[$AGENT_ID] $ROLE ($AGENT_MODEL_STR) SINGLE @ project $ODONIAN_PROJECT @ $WT; polling"
  fi
  while true; do
    [ "$STOP" -eq 1 ] && break
    prune_deferred
    # Pick the first claimable task under the shared comparator (priority desc, created_at asc, id
    # asc) whose model isn't in a failure backoff window and that isn't deferred (see pick_claimable_task).
    sel=$(pick_claimable_task "$ODONIAN_PROJECT" "$KIND")
    if [ -z "$sel" ]; then
      _idle="$(idle_nap 30)"
      echo "[$AGENT_ID] $(date '+%H:%M:%S') nothing claimable ($KIND) with an available model; sleeping ${_idle}s"; nap "$_idle"; continue
    fi
    task_id="${sel%%$'\t'*}"; task_model="${sel#*$'\t'}"
    # Read track from task, default to 'build' if absent
    task_track=$(odonian show "$task_id" --json 2>/dev/null | jq -r '.track // "build"')
    # An unreadable task must not default to a build prompt: a research task would then reach a
    # model without admission. Treat it as transient and look again shortly.
    if [ -z "$task_track" ]; then echo "[$AGENT_ID] $(date '+%H:%M:%S') could not read task $task_id; retrying shortly" >&2; nap 5; continue; fi
    PROMPT_FILE="$(get_prompt_file "$task_track" "$KIND")"
    if [ ! -f "$PROMPT_FILE" ]; then
      odonian transition "$task_id" --to blocked --note "no prompt for $DELIVERY_MODE/$task_track/$KIND: $PROMPT_FILE"
      echo "[$AGENT_ID] $(date '+%H:%M:%S') prompt not found: $PROMPT_FILE; blocking task $task_id"; nap 30; continue
    fi
    # Research is paced: get admitted for THIS task before any model process exists. A denial
    # leaves the task untouched, launches nothing, and defers it (honoring the server's retry hint)
    # so the next pass picks other eligible work instead of re-asking.
    if [ "$task_track" = "research" ]; then
      if ! admit_research_task "$task_id" "$task_model"; then
        [ "$STOP" -eq 1 ] && break
        continue
      fi
    fi
    echo "[$AGENT_ID] $(date '+%H:%M:%S') claimable $KIND; dispatching ($task_model/$task_track)…"
    export AGENT_MODEL="$task_model"
    SELECTED_TASK_ID="$task_id"
    dispatch
    SELECTED_TASK_ID=""
    [ -n "$MODEL" ] && export AGENT_MODEL="$MODEL"   # restore original model for slot identity (if initially provided)
    [ -n "$WT" ] && git -C "$WT" fetch origin --quiet 2>/dev/null || true
    [ -n "$WT" ] && git -C "$WT" checkout --detach --force origin/main --quiet 2>/dev/null || true
    [ "$STOP" -eq 1 ] && break
  done
  exit 0
fi

# ============================== MULTI-PROJECT MODE ==============================
# local_commit mode requires single-project (works with a pre-set worktree); multi-project clones multiple repos.
if [ "$DELIVERY_MODE" = "local_commit" ]; then
  echo "[$AGENT_ID] local_commit mode requires SINGLE-project mode; ODONIAN_PROJECT must be set" >&2
  exit 1
fi

AGENT_MODEL_STR="${MODEL:+$MODEL/}$KIND"
echo "[$AGENT_ID] $ROLE ($AGENT_MODEL_STR) MULTI @ $ODONIAN_URL${ALLOW:+ (allow: $ALLOW)}; discovering work across projects"
while true; do
  [ "$STOP" -eq 1 ] && break
  # Collect EVERY claimable task of my kind (any model) from all allowed projects and order the
  # whole set by the shared comparator (priority desc, created_at asc, id asc) before dispatching
  # anything: the best task wins regardless of which project it is in, including when every task has
  # the default priority (oldest first). No project shuffle. (while-read, not mapfile: macOS ships
  # bash 3.2.)
  candidates=()
  while IFS= read -r _row; do candidates+=("$_row"); done < <(global_candidates "$KIND" repo)
  if [ "${#candidates[@]}" -eq 0 ]; then
    echo "[$AGENT_ID] $(date '+%H:%M:%S') no claimable $KIND work in any project; sleeping 30s"; nap 30; continue
  fi

  worked=0
  deferred_this_pass=0
  failed_projects=" "
  prune_deferred
  for _row in "${candidates[@]}"; do
    [ "$STOP" -eq 1 ] && break
    IFS="$US" read -r _ _ task_id task_model pid prepo <<< "$_row"
    [ -n "$task_id" ] && [ -n "$pid" ] || continue
    # Local filters: a task pinned to a backend in a failure backoff window, or one still inside
    # its admission-denial/claim-race deferral, is passed over so lower candidates (other models,
    # other projects) still make progress. Done before cloning so an ineligible task costs nothing.
    model_unavailable "$task_model" && continue
    task_deferred "$task_id" && continue
    case "$failed_projects" in *" $pid "*) continue ;; esac   # clone/worktree setup already failed this pass
    apply_owner_token "$(norm_repo "$prepo" | cut -d/ -f1)"   # auth as the repo's owner (default auth if unmapped)
    prune_repos_cache "$REPOS_DIR/$(repo_slug "$prepo")"
    clone="$(ensure_clone "$prepo")" || { failed_projects="$failed_projects$pid "; continue; }
    wt="$(ensure_worktree "$clone")" || { failed_projects="$failed_projects$pid "; continue; }
    export ODONIAN_PROJECT="$pid" ODONIAN_REPO="$wt"
    cd "$wt" || continue
    # Read track from task, default to 'build' if absent
    task_track=$(odonian show "$task_id" --json 2>/dev/null | jq -r '.track // "build"')
    if [ -z "$task_track" ]; then echo "[$AGENT_ID] $(date '+%H:%M:%S') could not read task $task_id; skipping" >&2; defer_task "$task_id" 5; continue; fi
    PROMPT_FILE="$(get_prompt_file "$task_track" "$KIND")"
    if [ ! -f "$PROMPT_FILE" ]; then
      odonian transition "$task_id" --to blocked --note "no prompt for $DELIVERY_MODE/$task_track/$KIND: $PROMPT_FILE"
      echo "[$AGENT_ID] $(date '+%H:%M:%S') prompt not found: $PROMPT_FILE; blocking task $task_id"; nap 30; continue
    fi
    # Research is paced: get admitted for THIS task before any model process exists. A denial leaves
    # the task untouched, launches nothing, and defers it; move on to the next candidate.
    if [ "$task_track" = "research" ]; then
      if ! admit_research_task "$task_id" "$task_model"; then
        [ "$STOP" -eq 1 ] && break
        deferred_this_pass=1
        continue
      fi
    fi
    echo "[$AGENT_ID] $(date '+%H:%M:%S') dispatching ($task_model/$task_track/$KIND) on $(norm_repo "$prepo") [${pid:0:8}]…"
    export AGENT_MODEL="$task_model"
    SELECTED_TASK_ID="$task_id"
    dispatch
    SELECTED_TASK_ID=""
    [ -n "$MODEL" ] && export AGENT_MODEL="$MODEL"   # restore original model for slot identity (if initially provided)
    git -C "$wt" checkout --detach --force origin/main --quiet 2>/dev/null || true
    worked=1
    break   # one task per discovery pass, then re-list fresh so the next pick sees current priorities
  done
  [ "$STOP" -eq 1 ] && break
  # candidates existed but every one raced away / was ineligible / failed setup — brief sleep, then
  # re-list. A research deferral this pass means other candidates may still be eligible: re-list
  # straight away, skipping the deferred task. Otherwise wait, but no longer than the earliest
  # deferral expiry.
  if [ "$worked" -eq 0 ]; then
    if [ "$deferred_this_pass" -eq 1 ]; then _idle=1; else _idle="$(idle_nap 10)"; fi
    echo "[$AGENT_ID] $(date '+%H:%M:%S') no dispatchable candidate this pass; sleeping ${_idle}s"; nap "$_idle"
  fi
done
