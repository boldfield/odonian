#!/usr/bin/env bash
# fleet.sh — spawn and manage multiple agents in parallel (workers, reviewers, or mergers).
# Each agent runs in its own slot and handles its own work in the background.
# Usage: fleet.sh --kind <implement|review|merge> --count <n> [--model <tier>] [--delivery-mode <pull_request|local_commit>]
#        fleet.sh (help/no args)
#
# Examples:
#   fleet.sh --kind implement --count 2              # 2 generic implement workers (pull_request mode)
#   fleet.sh --kind review --count 2 --model opus    # 2 opus reviewers (pull_request mode)
#   fleet.sh --kind merge --count 1                  # 1 merger (non-LLM)
#   fleet.sh --kind implement --count 2 --delivery-mode local_commit  # local_commit mode
#
# Agents are spawned in slots: worker-N, reviewer-N, merger-N (N = 1..count).
# Ctrl-C stops the fleet and can interrupt in-flight work; it does not guarantee a drain.
set -uo pipefail

# --- resolve our REAL directory, even when invoked via a symlink ---
_src="${BASH_SOURCE[0]}"
while [ -h "$_src" ]; do
  _d="$(cd -P "$(dirname "$_src")" && pwd)"; _src="$(readlink "$_src")"; [[ $_src != /* ]] && _src="$_d/$_src"
done
HARNESS_DIR="$(cd -P "$(dirname "$_src")" && pwd)"

# --- parse args ---
KIND="" COUNT="" MODEL="" DELIVERY_MODE="pull_request"
while [ $# -gt 0 ]; do
  case "$1" in
    --kind)            KIND="${2:?}";            shift 2 ;;
    --count)           COUNT="${2:?}";           shift 2 ;;
    --model)           MODEL="${2:?}";           shift 2 ;;
    --delivery-mode)   DELIVERY_MODE="${2:?}";   shift 2 ;;
    -h|--help) {
      echo "usage: fleet.sh --kind <implement|review|merge> --count <n> [--model <tier>] [--delivery-mode <pull_request|local_commit>]"
      echo ""
      echo "Examples:"
      echo "  fleet.sh --kind implement --count 2              # 2 generic implement workers"
      echo "  fleet.sh --kind review --count 2 --model opus    # 2 opus reviewers"
      echo "  fleet.sh --kind merge --count 1                  # 1 merger (non-LLM)"
      echo "  fleet.sh --kind implement --count 2 --delivery-mode local_commit  # local_commit mode"
      exit 0
    } ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

: "${KIND:?--kind required}"
: "${COUNT:?--count required}"

case "$DELIVERY_MODE" in pull_request|local_commit) ;; *) echo "delivery mode must be pull_request or local_commit" >&2; exit 1 ;; esac

case "$KIND" in
  implement) SLOT_PREFIX="worker" ;;
  review)    SLOT_PREFIX="reviewer" ;;
  merge)     SLOT_PREFIX="merger" ;;
  *) echo "kind must be implement|review|merge" >&2; exit 1 ;;
esac

# Export delivery mode for child agents
export ODONIAN_DELIVERY_MODE="$DELIVERY_MODE"

# --- stop ---
STOP=0
request_stop() {
  [ "$STOP" -eq 1 ] && return
  STOP=1
  echo "[fleet] stop requested — stopping agents; in-flight work may be interrupted. Ctrl-C again to force-quit."
  trap - INT TERM
  # Signal all child processes
  kill -TERM 0 2>/dev/null || true
}
trap request_stop INT TERM

# --- spawn agents ---
PIDS=()
echo "[fleet] spawning $COUNT $KIND agent(s)"

MODEL_ARG=""
[ -n "$MODEL" ] && MODEL_ARG="--model $MODEL"

for i in $(seq 1 "$COUNT"); do
  slot="${SLOT_PREFIX}-$i"
  echo "[fleet] starting slot $slot"
  # shellcheck disable=SC2086
  "$HARNESS_DIR/agent.sh" --kind "$KIND" $MODEL_ARG "$slot" &
  PIDS+=($!)
done

# --- wait for all agents to finish ---
for pid in "${PIDS[@]}"; do
  wait "$pid" 2>/dev/null || true
done

echo "[fleet] all agents exited"
