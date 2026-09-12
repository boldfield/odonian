#!/usr/bin/env bash
# seed-demo.sh — put ONE example task on a project's board so a fresh fleet has something to do.
#
# Idempotent: ensures a "Demo: greetings" feature_spec document exists on the project and that one
# implement task titled "Append a greeting line to GREETINGS.md" (model haiku, reviewer opus,
# agent_merge=false, i.e. human-gated) exists and is promoted to `ready`. Re-running after the
# task has been worked creates nothing new; pass --again to post a NEW, distinctly titled task
# ("Append greeting line #N"). A same-titled copy would be a no-op: in local_commit mode the CLI
# branches a task's worktree from the frozen `wi/<slug>` branch of the same title, so the line
# would already be there and the worker would (correctly) submit a no_op for review.
#
# Called by harness/sbx.sh --seed-demo after it creates the throwaway project; also usable on its
# own against any server:
#   ODONIAN_URL=http://localhost:8080 ODONIAN_TOKEN=... bash harness/seed-demo.sh --project <uuid>
#
# Prints the task id on stdout (one line) so callers can hand it to the human.
set -euo pipefail

PROJECT_ID="" AGAIN=0
while [ $# -gt 0 ]; do
  case "$1" in
    --project) PROJECT_ID="${2:?}"; shift 2 ;;
    --again)   AGAIN=1; shift ;;
    -h|--help) sed -n '2,13p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done
[ -n "$PROJECT_ID" ] || { echo "seed-demo: --project <uuid> is required" >&2; exit 1; }
: "${ODONIAN_URL:?seed-demo: ODONIAN_URL must be set}"
: "${ODONIAN_TOKEN:?seed-demo: ODONIAN_TOKEN must be set}"
command -v jq >/dev/null 2>&1 || { echo "seed-demo: jq is required" >&2; exit 1; }

DOC_TITLE="Demo: greetings"
TASK_TITLE="Append a greeting line to GREETINGS.md"
TASK_LINE="hello from the fleet"

AUTH=(-H "Authorization: Bearer $ODONIAN_TOKEN" -H "Content-Type: application/json")
api() { curl -fsS "${AUTH[@]}" "$@"; }
say() { echo "[seed-demo] $*" >&2; }

# 1. Document (tasks need a document_id). Idempotent by title.
DOC_ID="$(api "$ODONIAN_URL/projects/$PROJECT_ID/documents" \
  | jq -r --arg t "$DOC_TITLE" '.[] | select(.title == $t) | .id' | head -1)"
if [ -z "$DOC_ID" ] || [ "$DOC_ID" = "null" ]; then
  DOC_ID="$(api -X POST "$ODONIAN_URL/projects/$PROJECT_ID/documents" \
    -d "$(jq -n --arg t "$DOC_TITLE" '{kind:"feature_spec", title:$t, ref:"README.md"}')" | jq -r '.id')"
  say "created document $DOC_ID ($DOC_TITLE)"
else
  say "document exists: $DOC_ID ($DOC_TITLE)"
fi
[ -n "$DOC_ID" ] && [ "$DOC_ID" != "null" ] || { say "failed to resolve the demo document id"; exit 1; }

# 2. Task. Idempotent by title unless --again.
TASKS_JSON="$(api "$ODONIAN_URL/projects/$PROJECT_ID/tasks")"
EXISTING="$(printf '%s' "$TASKS_JSON" \
  | jq -r --arg t "$TASK_TITLE" '[.[] | select(.kind == "implement" and .title == $t)] | sort_by(.created_at) | last | "\(.id) \(.state)"' 2>/dev/null || true)"
if [ "$AGAIN" -eq 1 ]; then
  # Distinct title + distinct line per run, numbered after the tasks already on the board.
  N="$(( $(printf '%s' "$TASKS_JSON" | jq '[.[] | select(.kind == "implement" and (.title | startswith("Append")))] | length') + 1 ))"
  TASK_TITLE="Append greeting line #$N to GREETINGS.md"
  TASK_LINE="hello from the fleet (#$N)"
fi
TASK_SPEC="Append the line '$TASK_LINE' to GREETINGS.md in the repository root. Do not change any other file. \`make check\` and \`make test\` must still pass."
EXISTING_ID="${EXISTING%% *}"; EXISTING_STATE="${EXISTING#* }"
if [ -n "$EXISTING_ID" ] && [ "$EXISTING_ID" != "null" ] && [ "$AGAIN" -eq 0 ]; then
  say "task exists: $EXISTING_ID (state: $EXISTING_STATE); pass --again to post a new one"
  if [ "$EXISTING_STATE" = "backlog" ]; then
    api -X POST "$ODONIAN_URL/tasks/$EXISTING_ID/promote" >/dev/null
    say "promoted $EXISTING_ID: backlog -> ready"
  fi
  echo "$EXISTING_ID"
  exit 0
fi

TASK_ID="$(api -X POST "$ODONIAN_URL/projects/$PROJECT_ID/tasks" \
  -d "$(jq -n --arg d "$DOC_ID" --arg t "$TASK_TITLE" --arg s "$TASK_SPEC" \
        '[{title:$t, spec:$s, model:"haiku", review_models:["opus"], document_id:$d, agent_merge:false}]')" \
  | jq -r '.[0].id')"
[ -n "$TASK_ID" ] && [ "$TASK_ID" != "null" ] || { say "failed to create the demo task"; exit 1; }
api -X POST "$ODONIAN_URL/tasks/$TASK_ID/promote" >/dev/null
say "created task $TASK_ID ($TASK_TITLE) and promoted it to ready"
echo "$TASK_ID"
