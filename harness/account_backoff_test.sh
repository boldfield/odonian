#!/usr/bin/env bash
# account_backoff_test.sh — per-account dispatch backoff in agent.sh. The account-state block is
# extracted from agent.sh and exercised against synthetic fixtures that reproduce the CLIs' real
# limit and sign-in messages (never credentials): classification, reset parsing (time zone, date,
# ordinal), per-account scope (a Codex limit leaves Claude dispatchable), the sign-in window, the
# unparseable-reset fallback, the window cap, probe-then-recover, and that model_unavailable (the
# filter in front of every claim) reports every model of an exhausted account.
set -uo pipefail
HARNESS_DIR="$(cd -P "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
pass_count=0; fail_count=0
pass() { pass_count=$((pass_count + 1)); echo "  ✓ $1"; }
fail() { fail_count=$((fail_count + 1)); echo "  ✗ $1"; }
check() { if eval "$2"; then pass "$1"; else fail "$1"; fi; }

AGENT_ID=test
export AGENT_CODEX_MODELS="gpt-5.5,gpt-5.6-sol,gpt-6-astra"
export ANTHROPIC_DEFAULT_OPUS_MODEL="claude-opus-5-5" ANTHROPIC_DEFAULT_HAIKU_MODEL="pinned-haiku-x" ANTHROPIC_DEFAULT_FABLE_MODEL="claude-fable-5-1" ANTHROPIC_DEFAULT_SONNET_MODEL="claude-sonnet-5-5"
sed -n '/^# --- per-account backend availability/,/^# --- research admission/p' "$HARNESS_DIR/agent.sh" > "$TMP/block.sh"
[ -s "$TMP/block.sh" ] || { echo "cannot extract block"; exit 1; }
# shellcheck disable=SC1091
source "$TMP/block.sh"

echo "=== account_backoff_test ==="
printf "You've hit your session limit · resets 7:50pm (UTC)\n" > "$TMP/claude_limit"
printf "Error: You've hit your usage limit. Upgrade or try again at Oct 27th, 2026 6:32 AM.\n" > "$TMP/codex_limit"
printf "Your refresh token has already been used to generate a new access token. Please try signing in again. (refresh_token_reused)\n" > "$TMP/codex_signin"
printf "Failed to authenticate. API Error: 401 authentication_error\n" > "$TMP/claude_signin"
printf "segfault in tool\n" > "$TMP/other"
printf "You've hit your session limit · resets sometime soon\n" > "$TMP/unparseable"

echo "Classification"
check "claude session limit" '[ "$(classify_dispatch_failure "$TMP/claude_limit")" = limit ]'
check "codex usage limit" '[ "$(classify_dispatch_failure "$TMP/codex_limit")" = limit ]'
check "refresh_token_reused is sign-in" '[ "$(classify_dispatch_failure "$TMP/codex_signin")" = signin ]'
check "401 is sign-in" '[ "$(classify_dispatch_failure "$TMP/claude_signin")" = signin ]'
check "anything else" '[ "$(classify_dispatch_failure "$TMP/other")" = other ]'

echo "Accounts"
for m in opus sonnet haiku fable claude-opus-5-5 claude-sonnet-5-5 pinned-haiku-x; do
  check "$m -> anthropic" '[ "$(account_of "$m")" = anthropic ]'
done
for m in gpt-5.5 gpt-5.6-sol gpt-6-astra; do check "$m -> openai" '[ "$(account_of "$m")" = openai ]'; done
check "unknown model is its own account" '[ "$(account_of mystery)" = model:mystery ]'

echo "Reset parsing"
now=$(date +%s)
ts="$(parse_reset_epoch "$TMP/claude_limit")"
check "claude reset is in the future, within a day" '[ -n "$ts" ] && [ "$ts" -gt "$now" ] && [ "$ts" -le $((now + 86400)) ]'
check "claude reset is 19:50 UTC" '[ "$(date -u -d "@$ts" +%H:%M)" = 19:50 ]'
printf "hit your session limit · resets Dec 31, 11pm (America/New_York)\n" > "$TMP/dated"
ts="$(parse_reset_epoch "$TMP/dated")"
check "dated claude reset honours the time zone" '[ -n "$ts" ] && [ "$(date -u -d "@$ts" +%H)" -eq "$(( (23 + 5) % 24 ))" ]'
fmon="$(date -d '+20 days' +%b)"; fday="$(date -d '+20 days' +%-d)"; fyr="$(date -d '+20 days' +%Y)"
case "$fday" in 1|21|31) ord=st ;; 2|22) ord=nd ;; 3|23) ord=rd ;; *) ord=th ;; esac
printf "Error: You've hit your usage limit. Upgrade or try again at %s %s%s, %s 6:32 AM.\n" "$fmon" "$fday" "$ord" "$fyr" > "$TMP/codex_future"
ts="$(parse_reset_epoch "$TMP/codex_future")"
check "codex reset parses date with ordinal" '[ -n "$ts" ] && [ "$ts" = "$(date -d "$fmon $fday $fyr 6:32 AM" +%s)" ]'
check "unparseable reset yields nothing" '! parse_reset_epoch "$TMP/unparseable" >/dev/null'

echo "Limits hold the whole account"
reset=$((now + 3 * 3600))
mark_model_unavailable opus limit "$reset" 2>"$TMP/log"
for m in opus sonnet haiku fable claude-opus-5-5; do check "$m unavailable after a claude limit" 'model_unavailable "$m"'; done
check "codex models stay dispatchable" '! model_unavailable gpt-5.5 && ! model_unavailable gpt-6-astra'
check "window runs to reset plus margin" '[ "${FAIL_MODEL_UNTIL[0]}" -ge $((reset + ACCOUNT_RESET_MARGIN)) ] && [ "${FAIL_MODEL_UNTIL[0]}" -le $((reset + ACCOUNT_RESET_MARGIN + 5)) ]'
check "reset logged once" '[ "$(grep -c "usage limit; reset at" "$TMP/log")" = 1 ]'
clear_model_failures sonnet
check "success clears the account" '! model_unavailable opus'

echo "Codex weekly limit"
mark_model_unavailable gpt-5.5 limit "$((now + 6 * 86400))" 2>/dev/null
for m in gpt-5.5 gpt-5.6-sol gpt-6-astra; do check "$m unavailable" 'model_unavailable "$m"'; done
check "claude still dispatchable" '! model_unavailable opus && ! model_unavailable haiku'
clear_model_failures gpt-5.5

echo "Sign-in failure"
mark_model_unavailable gpt-5.6-sol signin 2>"$TMP/log"
check "distinct loud log line" 'grep -q "SIGN-IN FAILED for account openai.*re-authenticate" "$TMP/log"'
check "fixed long window" 'i=$(_fail_model_index openai); [ $((FAIL_MODEL_UNTIL[i] - $(date +%s))) -ge $((ACCOUNT_SIGNIN_WINDOW - 5)) ]'
check "all codex models held" 'model_unavailable gpt-5.5'
echo "Probe once, then re-arm"
i=$(_fail_model_index openai); FAIL_MODEL_UNTIL[$i]=$(( $(date +%s) - 1 ))
check "window end lets a probe through" '! model_unavailable gpt-5.5'
mark_model_unavailable gpt-5.5 signin 2>/dev/null
check "failed probe re-arms" 'model_unavailable gpt-6-astra'
clear_model_failures gpt-5.5

echo "Fallbacks and cap"
mark_model_unavailable opus limit "" 2>/dev/null
i=$(_fail_model_index anthropic)
check "unparseable limit uses escalating per-account backoff" '[ "${FAIL_MODEL_COUNTS[$i]}" = 1 ] && model_unavailable sonnet'
mark_model_unavailable sonnet limit "" 2>/dev/null
check "second failure on another name escalates the same account" '[ "${FAIL_MODEL_COUNTS[$i]}" = 2 ]'
clear_model_failures opus
mark_model_unavailable opus limit "$((now + 90 * 86400))" 2>/dev/null
i=$(_fail_model_index anthropic)
check "window capped at 7 days" '[ $((FAIL_MODEL_UNTIL[i] - $(date +%s))) -le $((ACCOUNT_MAX_WINDOW + 5)) ]'
clear_model_failures opus

echo "Unavailable account costs zero claims"
cat > "$TMP/pick.sh" <<'EOS'
claims=0
for m in opus sonnet haiku gpt-5.5; do model_unavailable "$m" && continue; claims=$((claims + 1)); done
echo "$claims"
EOS
mark_model_unavailable opus limit "$((now + 3600))" 2>/dev/null
check "only the other account's model is claimable" '[ "$(source "$TMP/pick.sh")" = 1 ]'

echo "Model-scoped failures (model_unavailable, other)"
clear_model_failures opus
printf "You're out of usage credits. Switch to another model to continue.\n" > "$TMP/fable_credits"
printf 'ERROR: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The '"'"'gpt-5.5'"'"' model is not supported when using Codex with a ChatGPT account."}}\n' > "$TMP/gpt55"
printf "There's an issue with the selected model (gpt-5.5). It may not exist or you may not have access to it. Run --model to pick a different model.\n[claude-code:unrecognized_model] {\"model\":\"gpt-5.5\"}\nClaude Code doesn't recognize the model gpt-5.5.\n" > "$TMP/selected_model"
printf 'grep output:\nYou'"'"'re out of usage credits. Switch to another model to continue.\na\nb\nc\nd\ne\nERROR: stream disconnected\n' > "$TMP/quoted_credits"
check "fable out of credits" '[ "$(classify_dispatch_failure "$TMP/fable_credits")" = model_unavailable ]'
check "codex gpt-5.5 unsupported on ChatGPT account" '[ "$(classify_dispatch_failure "$TMP/gpt55")" = model_unavailable ]'
check "selected-model issue followed by two warning lines" '[ "$(classify_dispatch_failure "$TMP/selected_model")" = model_unavailable ]'
check "credits text quoted earlier in the transcript" '[ "$(classify_dispatch_failure "$TMP/quoted_credits")" = other ]'
printf 'tool output quoted: {"message":"The '"'"'gpt-5.5'"'"' model is not supported when using Codex with a ChatGPT account."}\nERROR: stream disconnected\n' > "$TMP/quoted_codex"
check "codex error quoted within the last five lines" '[ "$(classify_dispatch_failure "$TMP/quoted_codex")" = other ]'
check "limit text before a model error still not a limit" '[ "$(classify_dispatch_failure "$TMP/fable_credits")" != limit ]'

mark_model_unavailable fable model_unavailable 2>"$TMP/log"
check "log names the model scope" 'grep -q "model claude-fable-5-1 unavailable (model not available)" "$TMP/log"'
check "fable held" 'model_unavailable fable'
check "alias and pinned name share one window" 'model_unavailable claude-fable-5-1 && [ "${#FAIL_MODEL_NAMES[@]}" = 1 ]'
check "opus and sonnet on the same account stay dispatchable" '! model_unavailable opus && ! model_unavailable sonnet && ! model_unavailable claude-opus-5-5'
check "window is the model_unavailable window" 'i=$(_fail_model_index model@claude-fable-5-1); [ $((FAIL_MODEL_UNTIL[i] - $(date +%s))) -ge $((MODEL_UNAVAILABLE_WINDOW - 5)) ]'
clear_model_failures opus
check "success on another model does not clear it" 'model_unavailable fable'
i=$(_fail_model_index model@claude-fable-5-1); FAIL_MODEL_UNTIL[$i]=$(( $(date +%s) - 1 ))
check "window end lets one probe through" '! model_unavailable fable'
clear_model_failures claude-fable-5-1
check "success clears the model's record" '[ "${#FAIL_MODEL_NAMES[@]}" = 0 ]'

mark_model_unavailable gpt-5.5 model_unavailable 2>/dev/null
check "gpt-5.5 held, gpt-5.6-sol not" 'model_unavailable gpt-5.5 && ! model_unavailable gpt-5.6-sol'
clear_model_failures gpt-5.5

mark_model_unavailable sonnet other 2>"$TMP/log"
check "other failure holds only that model" 'model_unavailable sonnet && ! model_unavailable opus && ! model_unavailable fable'
check "other failure log names the model" 'grep -q "model claude-sonnet-5-5 unavailable (dispatch failure" "$TMP/log"'
mark_model_unavailable sonnet other 2>/dev/null
i=$(_fail_model_index model@claude-sonnet-5-5)
check "other escalates per model" '[ "${FAIL_MODEL_COUNTS[$i]}" = 2 ]'
clear_model_failures sonnet

mark_model_unavailable fable signin 2>/dev/null
check "sign-in still holds every model of the account" 'model_unavailable opus && model_unavailable sonnet && model_unavailable haiku && ! model_unavailable gpt-5.5'
clear_model_failures fable
mark_model_unavailable fable limit "$((now + 3600))" 2>/dev/null
check "limit still holds every model of the account" 'model_unavailable opus && model_unavailable sonnet && model_unavailable fable && ! model_unavailable gpt-5.5'
clear_model_failures opus

echo
echo "$pass_count passed, $fail_count failed"
[ "$fail_count" -eq 0 ]
