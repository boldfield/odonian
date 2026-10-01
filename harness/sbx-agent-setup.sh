#!/usr/bin/env bash
# sbx-agent-setup.sh — provision agent tooling + board config INSIDE an `sbx` sandbox container:
# installs the claude and codex CLIs (verifying each actually resolves and reports a version),
# symlinks the repo's four Odonian skills into ~/.claude/skills, and writes/merges board
# instructions + a permission allowlist for an in-container interactive claude session.
#
# This is Deliverable 2 of docs/features/sbx-agent-bootstrap.md — read that doc first, it governs
# every choice here. In short: the sandbox image is EXTERNAL (not built from this repo) and can
# change underneath us, so nothing may be assumed present — verify, never assume. Contrast with
# `make sbx-codex-auth` (Makefile), which runs on the HOST and seeds codex credentials INTO the
# sandbox from outside; this script runs the opposite direction (inside the sandbox) and cannot
# authenticate either CLI, only install + wire it up.
#
# Idempotent: re-running is a clean no-op. It NEVER touches an already-present CLI install or its
# stored credentials — the goal is a guaranteed-present CLI, not a guaranteed-fresh one.
#
# Usage:
#   bash harness/sbx-agent-setup.sh [--port P]   # P defaults to harness/sbx.sh's own default (8080)
#   sbx.sh also passes:
#     --board-env        write the ODONIAN_* board vars from ITS environment into settings.json `env`
#                        (only for a single-project local_commit fleet; see section 6). Never inferred
#                        from the ambient environment: a host shell's exported ODONIAN_TOKEN must not end
#                        up persisted in ~/.claude/settings.json.
#     --clear-board-env  remove those ODONIAN_* vars from settings.json `env` (a later boot of a fleet
#                        whose agents set them per task, which a fixed value there would override).
#     --codex-optional   warn instead of failing when codex cannot be installed (sbx.sh has already
#                        decided the board can run without it, e.g. the opus-reviewed demo).
#     --check-board-env  with --board-env or --clear-board-env: change nothing, just exit 0 if
#                        settings.json `env` already holds exactly the requested board vars (or none),
#                        else 1. sbx.sh runs it before starting the fleet, whose claude -p reads that file.
set -uo pipefail

# --- resolve our REAL directory, even when invoked via a symlink (matches harness/sbx.sh) ---
_src="${BASH_SOURCE[0]}"
while [ -h "$_src" ]; do
  _d="$(cd -P "$(dirname "$_src")" && pwd)"; _src="$(readlink "$_src")"; [[ $_src != /* ]] && _src="$_d/$_src"
done
HARNESS_DIR="$(cd -P "$(dirname "$_src")" && pwd)"
REPO_ROOT="$(cd -P "$HARNESS_DIR/.." && pwd)"

# --- args ---
PORT=8080
BOARD_ENV=0 CLEAR_BOARD_ENV=0 CODEX_OPTIONAL=0 CHECK_BOARD_ENV=0
while [ $# -gt 0 ]; do
  case "$1" in
    --port) PORT="${2:?}"; shift 2 ;;
    --board-env) BOARD_ENV=1; shift ;;
    --clear-board-env) CLEAR_BOARD_ENV=1; shift ;;
    --codex-optional) CODEX_OPTIONAL=1; shift ;;
    --check-board-env) CHECK_BOARD_ENV=1; shift ;;
    -h|--help)
      sed -n '2,31p' "$_src"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

say() { echo "[sbx-agent-setup] $*"; }
die() { echo "[sbx-agent-setup] ERROR: $*" >&2; exit 1; }
warn() { echo "[sbx-agent-setup] WARNING: $*" >&2; }

[ -n "${HOME:-}" ] || die "\$HOME is not set"

# Must match harness/sbx.sh's LOCAL_TOKEN — this script only ever runs against sbx.sh's own local
# server, never the production cluster.
LOCAL_TOKEN="sbx-local-token"

CLAUDE_HOME="${CLAUDE_HOME:-$HOME/.claude}"
SKILLS_SRC="$REPO_ROOT/skills"
SKILLS_DST="$CLAUDE_HOME/skills"
INSTRUCTIONS_FILE="$CLAUDE_HOME/CLAUDE.md"       # global user memory: applies no matter what dir an
                                                  # interactive claude session is started from.
SETTINGS_FILE="$CLAUDE_HOME/settings.json"       # same reasoning: user-level, not project-level.
# The board vars this script manages in settings.json `env` (see section 6). harness/sbx.sh relies on
# --check-board-env (below) rather than its own copy of this list.
BOARD_ENV_KEYS='["ODONIAN_URL","ODONIAN_TOKEN","ODONIAN_PROJECT","ODONIAN_HOME","ODONIAN_DELIVERY_MODE","ODONIAN_REPO","ODONIAN_WORKTREE_HOME"]'

# board_env_matches set|clear: does settings.json `env` hold exactly the board vars requested — for
# `set`, those (non-empty) ODONIAN_* vars in this process's environment; for `clear`, none of them?
# Other keys in `env` are not considered. A missing settings.json holds none; an unreadable one fails.
board_env_matches() {
  local current expected='{}'
  command -v jq >/dev/null 2>&1 || return 1
  if [ -f "$SETTINGS_FILE" ]; then
    current="$(jq -cS --argjson keys "$BOARD_ENV_KEYS" '(.env // {}) | with_entries(select(.key as $k | $keys | index($k)))' "$SETTINGS_FILE" 2>/dev/null)" || return 1
  else
    current='{}'
  fi
  if [ "$1" = "set" ]; then
    expected="$(jq -cnS --argjson keys "$BOARD_ENV_KEYS" 'env | with_entries(select((.key as $k | $keys | index($k)) and (.value != "")))')" || return 1
  fi
  if [ "$current" != "$expected" ]; then
    echo "board vars in $SETTINGS_FILE: $current; expected: $expected" >&2
    return 1
  fi
}

if [ "$CHECK_BOARD_ENV" -eq 1 ]; then
  if [ "$BOARD_ENV" -eq 1 ]; then board_env_matches set
  elif [ "$CLEAR_BOARD_ENV" -eq 1 ]; then board_env_matches clear
  else die "--check-board-env needs --board-env or --clear-board-env"
  fi
  exit $?
fi

INSTALL_LOG="$(mktemp -t sbx-agent-setup-install.XXXXXX)"
trap 'rm -f "$INSTALL_LOG"' EXIT

mkdir -p "$CLAUDE_HOME"

# ============================== 1. claude CLI: install + verify ==============================
if command -v claude >/dev/null 2>&1; then
  say "claude: already present at $(command -v claude) — leaving install + credentials untouched"
else
  # The container's global npm prefix is root-owned; passwordless sudo is available (see
  # docs/features/sbx-agent-bootstrap.md), so a global install with sudo is the correct simple path
  # — no per-user prefix workaround.
  command -v npm  >/dev/null 2>&1 || die "npm not on PATH — cannot install the claude CLI"
  command -v sudo >/dev/null 2>&1 || die "sudo not on PATH — cannot write into the root-owned npm global prefix"
  say "claude: not found — installing (sudo npm install -g @anthropic-ai/claude-code)…"
  # -n: never prompt. Passwordless sudo is expected; without it, fail into the error below rather
  # than stall an unattended boot at a password prompt.
  sudo -n npm install -g @anthropic-ai/claude-code >>"$INSTALL_LOG" 2>&1 \
    || die "npm install of @anthropic-ai/claude-code failed (see $INSTALL_LOG): $(tail -20 "$INSTALL_LOG")"
fi
# Verify: presence on PATH AND a working version call. `npm install` exiting zero is not evidence
# the binary actually runs — this is the check this whole script exists to make real.
command -v claude >/dev/null 2>&1 || die "claude still not on PATH after install"
CLAUDE_VERSION="$(claude --version 2>&1)" || die "claude is on PATH but 'claude --version' failed: $CLAUDE_VERSION"
say "claude: $(command -v claude)  ($CLAUDE_VERSION)"

# ============================== 2. codex CLI: install + verify ==============================
# With --codex-optional (sbx.sh, which has already decided the board can run without codex), a
# missing or broken codex only warns, so the skills, instructions and settings below still get written.
codex_unavailable() {
  if [ "$CODEX_OPTIONAL" -eq 1 ]; then warn "$*; continuing without codex (--codex-optional)"; return 0; fi
  die "$*"
}
CODEX_OK=1
if command -v codex >/dev/null 2>&1; then
  say "codex: already present at $(command -v codex) — leaving install + credentials untouched"
elif ! command -v npm >/dev/null 2>&1; then
  codex_unavailable "npm not on PATH — cannot install the codex CLI"; CODEX_OK=0
elif ! command -v sudo >/dev/null 2>&1; then
  codex_unavailable "sudo not on PATH — cannot write into the root-owned npm global prefix"; CODEX_OK=0
else
  say "codex: not found — installing (sudo npm install -g @openai/codex)…"
  sudo -n npm install -g @openai/codex >>"$INSTALL_LOG" 2>&1 \
    || { codex_unavailable "npm install of @openai/codex failed (see $INSTALL_LOG): $(tail -20 "$INSTALL_LOG")"; CODEX_OK=0; }
fi
if [ "$CODEX_OK" -eq 1 ]; then
  if ! command -v codex >/dev/null 2>&1; then
    codex_unavailable "codex still not on PATH after install"; CODEX_OK=0
  elif ! CODEX_VERSION="$(codex --version 2>&1)"; then
    codex_unavailable "codex is on PATH but 'codex --version' failed: $CODEX_VERSION"; CODEX_OK=0
  else
    say "codex: $(command -v codex)  ($CODEX_VERSION)"
  fi
fi

# ============================== 3. report (never fail) auth state ==============================
# Presence is verified above; whether either CLI is actually AUTHENTICATED is a separate concern
# this script only REPORTS on — it must never block an idempotent provisioning run, and it must
# never spend quota doing it. harness/sbx.sh owns the loud, blocking preflight (a live `claude -p`
# call) right before the fleet starts; the checks here are local-only / no network, so they're safe
# to run on every invocation of this script.
#
# Deliberately NOT `claude auth status`: verified empirically that invoking it (in any output mode)
# rewrites ~/.claude/settings.json as a side effect, silently dropping fields it doesn't recognize —
# a real risk to the "existing claude settings are preserved" requirement below (§6), and one that
# would fire on EVERY run of this script, not just the first. Check the credential file's presence
# directly instead — the same shape-only signal `claude auth status` itself relies on, without the
# mutating read-modify-write. `claude --version` (used above) was verified to have no such effect.
if [ -s "$CLAUDE_HOME/.credentials.json" ] || [ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]; then
  say "claude: credentials present (harness/sbx.sh's live preflight is the authoritative check)"
else
  say "claude: NOT authenticated — no $CLAUDE_HOME/.credentials.json and no CLAUDE_CODE_OAUTH_TOKEN set."
  say "        Fix with: interactive 'claude auth login', or export CLAUDE_CODE_OAUTH_TOKEN (a"
  say "        'claude setup-token' token) before running harness/sbx.sh."
fi
if [ "$CODEX_OK" -eq 0 ]; then
  say "codex: not installed (see above)"
elif codex login status >/dev/null 2>&1; then
  say "codex: authenticated"
else
  say "codex: NOT authenticated — fix from the HOST with: make sbx-codex-auth SBX_NAME=<name>"
  say "       (codex login is an interactive browser flow that cannot run headless in the sandbox)"
fi

# ============================== 4. wire the repo's skills into ~/.claude/skills ==============================
# Symlink, never copy: editing a skill in the mounted repo must take effect immediately with no
# re-provisioning, mirroring the harness's split between versioned code in the repo and unversioned
# state elsewhere. SKILLS_SRC is resolved from THIS script's own location (never a hardcoded
# absolute path), so it's correct no matter where the repo happens to be mounted.
SKILLS=(odonian-board odonian-breakdown odonian-ops review)
mkdir -p "$SKILLS_DST"
for name in "${SKILLS[@]}"; do
  src="$SKILLS_SRC/$name"
  dst="$SKILLS_DST/$name"
  [ -d "$src" ] || die "skill '$name' not found at $src — repo layout mismatch"
  if [ -L "$dst" ]; then
    current="$(readlink "$dst")"
    if [ "$current" = "$src" ]; then
      say "skill '$name': already linked -> $src"
    else
      say "skill '$name': relinking (was -> $current)"
      rm -f "$dst"
      ln -s "$src" "$dst"
    fi
  elif [ -e "$dst" ]; then
    warn "skill '$name': $dst already exists and is not our symlink — leaving it alone"
  else
    ln -s "$src" "$dst"
    say "skill '$name': linked -> $src"
  fi
done

# ============================== 5. board instructions for the in-container claude session ==============================
# Written into the global CLAUDE.md (not the repo) since it describes THIS sandbox's local board,
# not the project — it must apply no matter which directory an interactive session is started from,
# and it must never be committed. Idempotent via a marked block: re-running replaces the block in
# place (so a changed --port is picked up) instead of appending duplicates.
BEGIN_MARK="<!-- BEGIN odonian-sbx-board (generated by harness/sbx-agent-setup.sh) -->"
END_MARK="<!-- END odonian-sbx-board -->"
touch "$INSTRUCTIONS_FILE"
if grep -qF "$BEGIN_MARK" "$INSTRUCTIONS_FILE"; then
  awk -v b="$BEGIN_MARK" -v e="$END_MARK" '
    $0==b {skip=1; next}
    $0==e {skip=0; next}
    !skip {print}
  ' "$INSTRUCTIONS_FILE" > "$INSTRUCTIONS_FILE.tmp" && mv "$INSTRUCTIONS_FILE.tmp" "$INSTRUCTIONS_FILE"
fi
# Strip trailing blank lines left by the removal above so re-runs don't grow the file.
printf '%s\n' "$(cat "$INSTRUCTIONS_FILE")" > "$INSTRUCTIONS_FILE"
{
  echo ""
  echo "$BEGIN_MARK"
  echo "## Odonian sandbox board"
  echo ""
  echo "This sandbox runs a THROWAWAY LOCAL Odonian board for development — it is explicitly NOT"
  echo "the production cluster:"
  echo ""
  echo "- Server: http://localhost:$PORT"
  echo "- Token: $LOCAL_TOKEN"
  echo ""
  echo "Every task you create on this board MUST set \`review_models\` to BOTH \`[\"opus\", \"gpt-6.1-sol\"]\`,"
  echo "unless a human explicitly tells you to override it. This pair is deliberate, not an arbitrary"
  echo "constant: it gives every task two INDEPENDENT reviewers — opus (dispatched via \`claude -p\`)"
  echo "and gpt-6.1-sol (dispatched via \`codex exec\`) — so no single model, or model family, grades its"
  echo "own or a sibling's work."
  if [ "$BOARD_ENV" -eq 1 ] && [ -n "${ODONIAN_PROJECT:-}" ]; then
    echo ""
    echo "- Project (the board the fleet drains): ${ODONIAN_PROJECT}"
    echo "- Delivery mode: ${ODONIAN_DELIVERY_MODE:-local_commit}${ODONIAN_REPO:+, repo ${ODONIAN_REPO}}"
    echo "- The ODONIAN_URL/ODONIAN_TOKEN/ODONIAN_PROJECT env vars are already set for this session"
    if ODONIAN_CLI="$(command -v odonian 2>/dev/null)"; then
      echo "- The \`odonian\` CLI is at ${ODONIAN_CLI}; if \`odonian\` is not found on your PATH, call it by that absolute path"
    fi
  fi
  echo "$END_MARK"
} >> "$INSTRUCTIONS_FILE"
say "board instructions written -> $INSTRUCTIONS_FILE"

# ============================== 6. claude settings: permission allowlist for routine board traffic ==============================
# Merge into any existing settings.json rather than overwriting it — an operator's own permissions
# (or anything else already in the file) must survive every re-run.
#
# Claude Code's Bash permission rules are prefix-style (an exact command, or a fixed prefix plus a
# trailing ":*" wildcard) — not a general glob — so a rule cannot precisely mean "any curl invocation
# that happens to target http://localhost:$PORT" (flags vary in position relative to the URL).
# "Bash(curl:*)" (any curl call) is the closest expressible rule; the risk is bounded because this
# settings file only ever applies inside a throwaway local sandbox (see the board instructions
# above), never production. WebFetch's rule syntax IS precisely domain-scoped, so that one is exact.
command -v jq >/dev/null 2>&1 || die "jq not on PATH — needed to merge $SETTINGS_FILE without clobbering it"
if [ -f "$SETTINGS_FILE" ]; then
  jq empty "$SETTINGS_FILE" >/dev/null 2>&1 || die "$SETTINGS_FILE exists but is not valid JSON — refusing to touch it"
  EXISTING="$(cat "$SETTINGS_FILE")"
else
  EXISTING='{}'
fi
# Board config -> the claude session's environment. Claude Code's settings.json `env` block is applied
# to every session AND to the Bash tool's subprocesses, so `odonian` just works without the operator
# exporting anything. It is also applied to the fleet's own `claude -p` dispatches, which is why sbx.sh
# asks for it (--board-env) only for a single-project local_commit fleet, whose agents use exactly
# these values. Agents of a pull_request or multi-project fleet set ODONIAN_PROJECT/ODONIAN_REPO per
# task (harness/agent.sh), and a fixed value here would override them, so for those sbx.sh passes
# --clear-board-env to drop any left over from an earlier boot. Without either flag the env block is
# left alone: ODONIAN_* vars are never copied from the ambient environment, where (e.g. on a host
# shell) ODONIAN_TOKEN can be a real credential. CLAUDE_CODE_OAUTH_TOKEN is never copied here.
if [ "$BOARD_ENV" -eq 1 ]; then ENV_ACTION="set"
elif [ "$CLEAR_BOARD_ENV" -eq 1 ]; then ENV_ACTION="clear"
else ENV_ACTION="keep"
fi
ODONIAN_CLI_ABS="$(command -v odonian 2>/dev/null || true)"
MERGED="$(jq --arg cli "$ODONIAN_CLI_ABS" --arg action "$ENV_ACTION" --argjson keys "$BOARD_ENV_KEYS" '
  .permissions.allow = ((.permissions.allow // []) + [
    "Bash(curl:*)",
    "WebFetch(domain:localhost)",
    "Bash(odonian:*)",
    "Bash(git:*)"
  ] + (if $cli != "" then ["Bash(" + $cli + ":*)"] else [] end) | unique)
  | if $action == "keep" then .
    else .env = ((.env // {}) | with_entries(select(.key as $k | $keys | index($k) | not)))
      + (if $action == "set"
         then (env | with_entries(select((.key as $k | $keys | index($k)) and (.value != ""))))
         else {} end)
    end
  | if .env == {} then del(.env) else . end
' <<<"$EXISTING")" || die "failed to merge $SETTINGS_FILE"
{ printf '%s\n' "$MERGED" > "$SETTINGS_FILE.tmp" && mv "$SETTINGS_FILE.tmp" "$SETTINGS_FILE"; } \
  || { rm -f "$SETTINGS_FILE.tmp"; die "failed to write $SETTINGS_FILE"; }
if [ "$ENV_ACTION" != "keep" ]; then
  board_env_matches "$ENV_ACTION" || die "$SETTINGS_FILE does not hold the requested board vars after writing it"
fi
say "settings merged -> $SETTINGS_FILE"

say "done."
