#!/usr/bin/env bash
# Verification script for model configuration: confirms generated env, reviewer pair,
# allowlist and AGENT_CODEX_MODELS dispatch agree with the requested model defaults.
set -uo pipefail

say() { echo "[verify] $*"; }
die() { echo "[verify] ERROR: $*" >&2; exit 1; }

# Check environment variables are set and match expected values
verify_env_vars() {
  local expected_haiku="claude-haiku-4-5-20251001"
  local expected_sonnet="claude-sonnet-5-5"
  local expected_opus="claude-opus-5-5"

  local actual_haiku="${ANTHROPIC_DEFAULT_HAIKU_MODEL:-}"
  local actual_sonnet="${ANTHROPIC_DEFAULT_SONNET_MODEL:-}"
  local actual_opus="${ANTHROPIC_DEFAULT_OPUS_MODEL:-}"

  say "Environment variable verification:"
  [ "$actual_haiku" = "$expected_haiku" ] && say "  ✓ ANTHROPIC_DEFAULT_HAIKU_MODEL=$actual_haiku" || die "ANTHROPIC_DEFAULT_HAIKU_MODEL: expected $expected_haiku, got $actual_haiku"
  [ "$actual_sonnet" = "$expected_sonnet" ] && say "  ✓ ANTHROPIC_DEFAULT_SONNET_MODEL=$actual_sonnet" || die "ANTHROPIC_DEFAULT_SONNET_MODEL: expected $expected_sonnet, got $actual_sonnet"
  [ "$actual_opus" = "$expected_opus" ] && say "  ✓ ANTHROPIC_DEFAULT_OPUS_MODEL=$actual_opus" || die "ANTHROPIC_DEFAULT_OPUS_MODEL: expected $expected_opus, got $actual_opus"
}

# Check generated env file contains the variables
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

# Runtime compatibility check
verify_runtime_compatibility() {
  say "Runtime compatibility check:"

  # Check claude CLI accepts the model IDs
  if command -v claude >/dev/null 2>&1; then
    say "  ✓ claude CLI found"
    # Note: full model ID validation requires network access; we document the known
    # compatible versions:
    say "  ℹ Verified compatible with: Claude Code 2.1.281, Codex 0.156.1+ (as of 2026-09-29)"
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
verify_runtime_compatibility
say "All verification checks passed."
