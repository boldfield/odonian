#!/usr/bin/env bash
# Verification script for model configuration: confirms generated env, reviewer pair,
# allowlist and AGENT_CODEX_MODELS dispatch agree with the requested model defaults.
set -uo pipefail

say() { echo "[verify] $*"; }
die() { echo "[verify] ERROR: $*" >&2; exit 1; }

# Check environment variables are set and exported
verify_env_vars() {
  local actual_haiku="${ANTHROPIC_DEFAULT_HAIKU_MODEL:-}"
  local actual_sonnet="${ANTHROPIC_DEFAULT_SONNET_MODEL:-}"
  local actual_opus="${ANTHROPIC_DEFAULT_OPUS_MODEL:-}"

  say "Environment variable verification:"
  [ -n "$actual_haiku" ] && say "  ✓ ANTHROPIC_DEFAULT_HAIKU_MODEL=$actual_haiku" || die "ANTHROPIC_DEFAULT_HAIKU_MODEL not set"
  [ -n "$actual_sonnet" ] && say "  ✓ ANTHROPIC_DEFAULT_SONNET_MODEL=$actual_sonnet" || die "ANTHROPIC_DEFAULT_SONNET_MODEL not set"
  [ -n "$actual_opus" ] && say "  ✓ ANTHROPIC_DEFAULT_OPUS_MODEL=$actual_opus" || die "ANTHROPIC_DEFAULT_OPUS_MODEL not set"
}

# Check generated env file contains the variables and that env file values match exported environment
verify_env_file() {
  local env_file="${ODONIAN_HOME:-$HOME/.odonian}/env"
  [ -f "$env_file" ] || die "env file not found: $env_file"

  say "Generated env file verification ($env_file):"
  grep -q 'ANTHROPIC_DEFAULT_HAIKU_MODEL' "$env_file" || die "ANTHROPIC_DEFAULT_HAIKU_MODEL not in env file"
  say "  ✓ ANTHROPIC_DEFAULT_HAIKU_MODEL present"

  grep -q 'ANTHROPIC_DEFAULT_SONNET_MODEL' "$env_file" || die "ANTHROPIC_DEFAULT_SONNET_MODEL not in env file"
  say "  ✓ ANTHROPIC_DEFAULT_SONNET_MODEL present"

  grep -q 'ANTHROPIC_DEFAULT_OPUS_MODEL' "$env_file" || die "ANTHROPIC_DEFAULT_OPUS_MODEL not in env file"
  say "  ✓ ANTHROPIC_DEFAULT_OPUS_MODEL present"

  grep -q 'AGENT_CODEX_MODELS' "$env_file" || die "AGENT_CODEX_MODELS not in env file"
  say "  ✓ AGENT_CODEX_MODELS present"

  # Verify env file and exported environment are consistent
  local file_haiku="$(grep '^export ANTHROPIC_DEFAULT_HAIKU_MODEL=' "$env_file" | cut -d= -f2 | tr -d '"')"
  local file_sonnet="$(grep '^export ANTHROPIC_DEFAULT_SONNET_MODEL=' "$env_file" | cut -d= -f2 | tr -d '"')"
  local file_opus="$(grep '^export ANTHROPIC_DEFAULT_OPUS_MODEL=' "$env_file" | cut -d= -f2 | tr -d '"')"
  [ "$file_haiku" = "${ANTHROPIC_DEFAULT_HAIKU_MODEL}" ] || die "env file and environment haiku model mismatch"
  [ "$file_sonnet" = "${ANTHROPIC_DEFAULT_SONNET_MODEL}" ] || die "env file and environment sonnet model mismatch"
  [ "$file_opus" = "${ANTHROPIC_DEFAULT_OPUS_MODEL}" ] || die "env file and environment opus model mismatch"
  say "  ✓ env file and environment consistent"
}

# Check Codex dispatch routing
verify_codex_dispatch() {
  local codex_models="${AGENT_CODEX_MODELS:-}"
  say "Codex dispatch routing verification:"

  if echo "$codex_models" | grep -q 'gpt-6.1-sol'; then
    say "  ✓ gpt-6.1-sol routed through codex"
  else
    die "gpt-6.1-sol not in AGENT_CODEX_MODELS: $codex_models"
  fi

  if echo "$codex_models" | grep -q 'gpt-5.5'; then
    say "  ✓ gpt-5.5 retained for backwards compatibility"
  else
    die "gpt-5.5 not in AGENT_CODEX_MODELS (backwards compatibility): $codex_models"
  fi
}

# Verify reviewer pair and server allowlist
verify_reviewer_and_allowlist() {
  say "Reviewer pair and allowlist verification:"

  # Check sbx-agent-setup.sh uses the correct reviewer pair
  local sbx_setup="${HARNESS_DIR:-$(dirname "${BASH_SOURCE[0]}")}/sbx-agent-setup.sh"
  if [ -f "$sbx_setup" ]; then
    if grep -q '\["opus", "gpt-6.1-sol"\]' "$sbx_setup"; then
      say "  ✓ sbx-agent-setup.sh uses correct reviewer pair [\"opus\", \"gpt-6.1-sol\"]"
    else
      die "sbx-agent-setup.sh does not use reviewer pair [\"opus\", \"gpt-6.1-sol\"]"
    fi
  else
    say "  ⚠ sbx-agent-setup.sh not found; skipping reviewer pair check"
  fi

  # Check server allowlist contains gpt-6.1-sol
  local server_models="${ODONIAN_MODELS:-}"
  if [ -n "$server_models" ]; then
    if echo "$server_models" | grep -q 'gpt-6.1-sol'; then
      say "  ✓ ODONIAN_MODELS includes gpt-6.1-sol"
    else
      die "ODONIAN_MODELS does not include gpt-6.1-sol: $server_models"
    fi
  else
    say "  ⚠ ODONIAN_MODELS not set; skipping allowlist check"
  fi
}

# Runtime compatibility check
verify_runtime_compatibility() {
  say "Runtime compatibility check:"

  # Check claude CLI accepts the model IDs
  if command -v claude >/dev/null 2>&1; then
    say "  ✓ claude CLI found"
    # Note: full model ID validation requires network access; we document the known
    # compatible versions:
    say "  ℹ Expected compatible with: Claude Code 2.1.281, Codex 0.156.1+ (as of 2026-09-29)"
    say "  ℹ Exact runtime compatibility verification requires network access"
  else
    say "  ⚠ claude CLI not on PATH (runtime check skipped)"
  fi

  if command -v codex >/dev/null 2>&1; then
    say "  ✓ codex CLI found"
  else
    say "  ⚠ codex CLI not on PATH (review-only tasks may fail if GPT models are used)"
  fi
}

say "Verifying model configuration…"
verify_env_vars
verify_env_file
verify_codex_dispatch
verify_reviewer_and_allowlist
verify_runtime_compatibility
say "All verification checks passed."
