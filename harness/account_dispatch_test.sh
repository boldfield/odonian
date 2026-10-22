#!/usr/bin/env bash
# account_dispatch_test.sh — end-to-end per-account backoff. Runs the real single-project loop in
# agent.sh against a stubbed `odonian` (recording every call) and fake `claude` / `codex` CLIs that
# print captured (synthetic) failure messages. Asserts: a Claude session limit stops every Claude
# model with no further dispatch or `odonian claim` for them; a Codex weekly limit stops every Codex
# model while a Claude task is still dispatched; a sign-in failure holds the account and logs the
# distinct line; an unparseable limit falls back to backoff; and a transcript that merely QUOTES a
# limit / 401 (tool output) does not hold the account.
set -uo pipefail
unset ODONIAN_DISPATCH_ID
HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }
AGENT_PID=""
stop_agent() {
  [ -n "$AGENT_PID" ] || return 0
  kill -TERM "$AGENT_PID" 2>/dev/null; sleep 0.5; kill -KILL "$AGENT_PID" 2>/dev/null
  wait "$AGENT_PID" 2>/dev/null; AGENT_PID=""
}
trap 'stop_agent; rm -rf "$TMP"' EXIT

BARE="$TMP/origin.git"; git init --quiet --bare "$BARE"
WORK="$TMP/seed"; git clone --quiet "$BARE" "$WORK"
git -C "$WORK" -c user.email=t@t.example -c user.name=test commit --quiet --allow-empty -m init
git -C "$WORK" push --quiet origin HEAD:main; rm -rf "$WORK"
MAIN_REPO="$TMP/main-repo"; git clone --quiet "$BARE" "$MAIN_REPO"

BIN="$TMP/bin"; mkdir -p "$BIN"
cat > "$BIN/odonian" <<EOF
#!/usr/bin/env bash
echo "\$*" >> "$TMP/calls"
case "\$1" in
  next) echo "task-claude-opus" ;;
  tasks) cat "$TMP/tasks.json" ;;
  show) echo "{\"track\":\"build\",\"state\":\"in_progress\",\"assignee\":\"\$AGENT_ID\"}" ;;
  project) echo '{"repo":"$BARE"}' ;;
esac
exit 0
EOF
# Fake CLIs: log the model, print the scenario's message (stdout for claude, stderr for codex) after a
# tool-output preamble, and exit with the scenario's status.
for cli in claude codex; do
cat > "$BIN/$cli" <<EOF
#!/usr/bin/env bash
model=""; prev=""
for a in "\$@"; do { [ "\$prev" = --model ] || [ "\$prev" = -m ]; } && model="\$a"; prev="\$a"; done
echo "\$(date +%s.%N) $cli \$model" >> "$TMP/dispatch"
msgf="$TMP/msg.$cli"; [ -s "$TMP/msg.$cli.\$model" ] && msgf="$TMP/msg.$cli.\$model"
if [ -s "\$msgf" ]; then
  if [ "$cli" = codex ]; then cat "\$msgf" >&2; else cat "\$msgf"; fi
  exit 1
fi
exit 0
EOF
chmod +x "$BIN/$cli"
done
chmod +x "$BIN/odonian"

export PATH="$BIN:$PATH" ODONIAN_HOME="$TMP/odonian-home"
export ODONIAN_URL="http://127.0.0.1:0" ODONIAN_TOKEN="test-token" ODONIAN_PROJECT="proj-test"
export ODONIAN_MAIN_REPO="$MAIN_REPO" ODONIAN_REPO="$MAIN_REPO"
export AGENT_CODEX_MODELS="gpt-5.5,gpt-5.6-sol,gpt-6-astra"
export ANTHROPIC_DEFAULT_FABLE_MODEL="claude-fable-5-1"

ALL_TASKS='[{"id":"task-claude-opus","model":"opus","kind":"review","state":"ready","track":"build"},{"id":"task-claude-sonnet","model":"sonnet","kind":"review","state":"ready","track":"build"},{"id":"task-claude-haiku","model":"haiku","kind":"review","state":"ready","track":"build"},{"id":"task-codex","model":"gpt-5.5","kind":"review","state":"ready","track":"build"}]'
CODEX_TASKS='[{"priority":900,"id":"task-codex","model":"gpt-5.5","kind":"review","state":"ready","track":"build"},{"priority":900,"id":"task-codex2","model":"gpt-6-astra","kind":"review","state":"ready","track":"build"},{"id":"task-claude-opus","model":"opus","kind":"review","state":"ready","track":"build"}]'

# run_scenario <claude-msg-file|-> <codex-msg-file|-> <seconds> [tasks-json]
run_scenario() {
  printf '%s' "${4:-$ALL_TASKS}" > "$TMP/tasks.json"
  : > "$TMP/dispatch"; : > "$TMP/calls"; rm -f "$TMP"/msg.claude* "$TMP"/msg.codex*
  [ "$1" = - ] || cp "$1" "$TMP/msg.claude"
  [ "$2" = - ] || cp "$2" "$TMP/msg.codex"
  [ -z "${5:-}" ] || cp "${5#*=}" "$TMP/msg.${6:-claude}.${5%%=*}"   # per-model failure: <model>=<file> [cli]
  "$HARNESS_DIR/agent.sh" --model x --kind review e2e-slot > "$TMP/agent.log" 2>&1 &
  AGENT_PID=$!
  sleep "$3"
  stop_agent; [ -n "${DEBUG:-}" ] && { cat "$TMP/dispatch"; cat "$TMP/agent.log"; }
}
n_dispatch() { grep -c " $1\$" "$TMP/dispatch" 2>/dev/null || true; }
claims_for() { grep -c "^claim $1" "$TMP/calls" 2>/dev/null || true; }

echo "=== account_dispatch_test ==="
printf "You've hit your session limit · resets 11:59pm (UTC)\n" > "$TMP/claude_limit"
printf "tool output line\nERROR: You've hit your usage limit. Upgrade or try again at Dec 28th, 2099 6:32 AM.\n" > "$TMP/codex_limit"
printf "Your refresh token has already been used to generate a new access token. Please try signing in again. (refresh_token_reused)\n" > "$TMP/codex_signin"
printf "You've hit your session limit · resets sometime soon\n" > "$TMP/unparseable"
printf '{"status":401,"error":"unauthorized"}\nERROR: stream disconnected\n' > "$TMP/quoted_401"
printf 'harness/x.sh:12:printf "You'"'"'ve hit your session limit · resets 7:50pm (UTC)\\n"\nERROR: stream disconnected\n' > "$TMP/quoted_limit"

echo "Claude session limit stops every Claude model"
run_scenario "$TMP/claude_limit" - 6
check "exactly one Claude dispatch (the probe that hit the limit)" '[ "$(grep -c " claude " "$TMP/dispatch")" = 1 ]'
check "no claim issued for the other Claude tasks" '[ "$(claims_for task-claude-sonnet)$(claims_for task-claude-haiku)" = 00 ]'
check "the Codex task is still dispatched" '[ "$(n_dispatch gpt-5.5)" -ge 1 ]'
check "log states the account and parsed reset" 'grep -q "account anthropic unavailable (usage limit; reset at" "$TMP/agent.log"'

printf "tool output: You've hit your limit · resets Dec 31, 11:00pm (UTC)\nx\nYou've hit your limit · resets 11:59pm (UTC)\n" > "$TMP/quoted_reset"
echo "A reset quoted earlier in the tail does not set the window"
run_scenario "$TMP/quoted_reset" - 4
check "reset is parsed from the last line only (within a day)" 'grep -q "account anthropic unavailable (usage limit; reset at" "$TMP/agent.log" && ! grep -q "reset at .*Dec 31\|reset at .*-12-31" "$TMP/agent.log"'

echo "Codex weekly limit stops every Codex model, Claude stays dispatchable"
run_scenario - "$TMP/codex_limit" 6 "$CODEX_TASKS"
check "exactly one Codex dispatch" '[ "$(grep -c " codex " "$TMP/dispatch")" = 1 ]'
check "Claude tasks are still dispatched" '[ "$(grep -c " claude " "$TMP/dispatch")" -ge 1 ]'
check "log states the openai account" 'grep -q "account openai unavailable (usage limit; reset at" "$TMP/agent.log"'

echo "Sign-in failure"
run_scenario - "$TMP/codex_signin" 6 "$CODEX_TASKS"
check "one Codex dispatch, then the account is held" '[ "$(grep -c " codex " "$TMP/dispatch")" = 1 ]'
check "distinct sign-in log line" 'grep -q "SIGN-IN FAILED for account openai.*re-authenticate" "$TMP/agent.log"'
check "logged as a sign-in failure, not a usage limit" '! grep -q "account openai unavailable (usage limit" "$TMP/agent.log"'

echo "Unparseable limit falls back to escalating backoff"
run_scenario "$TMP/unparseable" - 4
check "backoff window of tens of seconds, not a reset" 'grep -q "usage limit with no parseable reset" "$TMP/agent.log"'
printf 'You have hit your usage limit. Upgrade or try again at Dec 28th, 2099 6:32 AM.\nERROR: stream disconnected\n' > "$TMP/quoted_exact"

echo "Quoted text in the transcript is not a classification"
run_scenario - "$TMP/quoted_401" 4 "$CODEX_TASKS"
check "quoted 401 is not a sign-in failure" '! grep -q "SIGN-IN FAILED" "$TMP/agent.log" && grep -q "dispatch failure (consecutive" "$TMP/agent.log"'
run_scenario - "$TMP/quoted_limit" 4 "$CODEX_TASKS"
check "quoted limit line is not a usage limit" '! grep -q "usage limit" "$TMP/agent.log" && grep -q "dispatch failure (consecutive" "$TMP/agent.log"'
run_scenario - "$TMP/quoted_exact" 4 "$CODEX_TASKS"
check "verbatim limit text in tool output is not a usage limit" '! grep -q "account openai unavailable" "$TMP/agent.log" && grep -q "model gpt-5.5 unavailable (dispatch failure" "$TMP/agent.log"'

echo "Non-recover dispatch output reaches the log"
ODONIAN_SESSION_RECOVERY=off run_scenario "$TMP/claude_limit" - 3
check "the model's message is in the agent log" 'grep -q "hit your session limit" "$TMP/agent.log"'

echo "Model-specific failures hold one model, not the account"
FABLE_TASKS='[{"priority":900,"id":"task-fable","model":"fable","kind":"review","state":"ready","track":"build"},{"id":"task-claude-opus","model":"opus","kind":"review","state":"ready","track":"build"}]'
printf "You're out of usage credits. Switch to another model to continue.\n" > "$TMP/fable_credits"
run_scenario - - 6 "$FABLE_TASKS" fable="$TMP/fable_credits"
check "fable dispatched exactly once, then skipped" '[ "$(n_dispatch fable)" = 1 ]'
check "opus on the same account dispatches at once" '[ "$(n_dispatch opus)" -ge 1 ]'
check "log names the model scope" 'grep -q "model claude-fable-5-1 unavailable (model not available)" "$TMP/agent.log" && ! grep -q "account anthropic unavailable" "$TMP/agent.log"'
check "claim handed back as backend_unavailable: model:fable" 'grep -q "^unclaim task-fable .*backend_unavailable: model:claude-fable-5-1: model_unavailable" "$TMP/calls"'

printf 'ERROR: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The '"'"'gpt-5.5'"'"' model is not supported when using Codex with a ChatGPT account."}}\n' > "$TMP/gpt55_400"
SOL_TASKS='[{"priority":900,"id":"task-codex","model":"gpt-5.5","kind":"review","state":"ready","track":"build"},{"id":"task-sol","model":"gpt-5.6-sol","kind":"review","state":"ready","track":"build"}]'
run_scenario - - 6 "$SOL_TASKS" gpt-5.5="$TMP/gpt55_400" codex
check "gpt-5.5 dispatched once, gpt-5.6-sol dispatches at once" '[ "$(n_dispatch gpt-5.5)" = 1 ] && [ "$(n_dispatch gpt-5.6-sol)" -ge 1 ]'

echo
echo "$pass_count passed, $fail_count failed"
[ "$fail_count" -eq 0 ]
