#!/usr/bin/env bash
# odonian.sh — thin CLI over the Odonian API for the odonian-breakdown skill.
# Keeps payloads correct (JSON built with jq, never hand-quoted) so the skill stops
# hand-rolling error-prone curl+jq for every call.
#
# Requires: ODONIAN_URL, ODONIAN_TOKEN in the environment; curl + jq on PATH.
# Every subcommand prints the raw JSON response on stdout; pipe to jq to extract ids.
set -uo pipefail

: "${ODONIAN_URL:?set ODONIAN_URL to the Odonian base URL}"
: "${ODONIAN_TOKEN:?set ODONIAN_TOKEN to the bearer token}"

_auth=(-H "Authorization: Bearer $ODONIAN_TOKEN" -H "Content-Type: application/json")
# --fail-with-body: non-zero exit on HTTP >=400 BUT still print the body, so the API's structured
# error (UNKNOWN_MODEL, UNKNOWN_DEPENDENCY, DOCUMENT_NOT_IN_PROJECT, ...) survives. Plain -f would
# discard it and reduce every failure to "error: 400" — defeating the point of the helper.
_api() { curl --fail-with-body -sS --max-time 30 "${_auth[@]}" "$@"; }

usage() {
  cat <<'USAGE'
Usage:
  odonian.sh create-project <name> <repo>                       -> project (.id)
  odonian.sh create-doc <project-id> <design|feature_spec> <title> <ref> [commit]
                                                                  -> document (.id)
  odonian.sh create-tasks <project-id> <tasks-json-file>        -> [tasks]  (file: JSON array)
  odonian.sh promote <task-id>                                  -> task (backlog->ready)
  odonian.sh transition <task-id> <to-state> [note]             -> task
  odonian.sh list <project-id> [query]                          -> [tasks]  (query e.g. 'state=ready&model=haiku')
  odonian.sh get <task-id>                                      -> task

Task object for the create-tasks file (a JSON array of these). depends_on entries are
either an intra-batch "key" or an existing task id, and must be ACYCLIC:
  { "key": "slug", "title": "...", "spec": "<prose, NO code>", "document_id": "<doc id>",
    "depends_on": ["other-key"], "model": "haiku", "review_models": ["opus", "gpt-6.1-sol"], "agent_merge": false }
  (review_models — pass the pair explicitly. Two independent reviewers: one Claude, one Codex.
   Server fallback when omitted is ["opus"]; this script shows the intended pairing. agent_merge defaults to false.)
USAGE
}

cmd="${1:-}"; shift || true
case "$cmd" in
  create-project)
    name="${1:?name}"; repo="${2:?repo}"
    jq -n --arg n "$name" --arg r "$repo" '{name:$n, repo:$r}' \
      | _api -X POST "$ODONIAN_URL/projects" -d @- ;;

  create-doc)
    pid="${1:?project-id}"; kind="${2:?kind (design|feature_spec)}"; title="${3:?title}"; ref="${4:?ref}"; commit="${5:-}"
    jq -n --arg k "$kind" --arg t "$title" --arg r "$ref" --arg c "$commit" \
      'if $c=="" then {kind:$k,title:$t,ref:$r} else {kind:$k,title:$t,ref:$r,commit:$c} end' \
      | _api -X POST "$ODONIAN_URL/projects/$pid/documents" -d @- ;;

  create-tasks)
    pid="${1:?project-id}"; file="${2:?tasks-json-file}"
    [ -f "$file" ] || { echo "no such file: $file" >&2; exit 1; }
    jq -e 'type=="array" and length>0' "$file" >/dev/null \
      || { echo "tasks file must be a non-empty JSON array" >&2; exit 1; }
    _api -X POST "$ODONIAN_URL/projects/$pid/tasks" -d @"$file" ;;

  promote)
    tid="${1:?task-id}"
    _api -X POST "$ODONIAN_URL/tasks/$tid/promote" ;;

  transition)
    tid="${1:?task-id}"; to="${2:?to-state}"; note="${3:-}"
    jq -n --arg t "$to" --arg n "$note" 'if $n=="" then {to:$t} else {to:$t,note:$n} end' \
      | _api -X POST "$ODONIAN_URL/tasks/$tid/transition" -d @- ;;

  list)
    pid="${1:?project-id}"; q="${2:-}"
    _api "$ODONIAN_URL/projects/$pid/tasks${q:+?$q}" ;;

  get)
    tid="${1:?task-id}"
    _api "$ODONIAN_URL/tasks/$tid" ;;

  ""|-h|--help|help) usage ;;
  *) echo "unknown command: $cmd" >&2; usage; exit 1 ;;
esac
