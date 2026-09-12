#!/usr/bin/env bash
# evidence.sh — recompute the numbers quoted in site/index.html (section "Evidence") from GitHub.
# The page is static on purpose: run this, then paste the numbers and the date into index.html.
#
# Counting rule: a merged PR is "fleet-authored" when its head branch starts with `mr/`, the
# deterministic branch name the harness derives from the task id (harness/prompts/pull_request/
# build/implement.md). Everything else (human branches, release branches, the pre-API bootstrap
# board's `agentask/` branches) counts as merged but not fleet-authored.
set -euo pipefail
REPO="${1:-boldfield/odonian}"
json="$(gh pr list -R "$REPO" --state merged --limit 2000 --json number,headRefName,mergedAt)"
total="$(printf '%s' "$json" | jq 'length')"
fleet="$(printf '%s' "$json" | jq '[.[] | select(.headRefName | startswith("mr/"))] | length')"
latest="$(printf '%s' "$json" | jq -r '[.[].mergedAt] | max | .[:10]')"
printf 'as of:            %s\n' "$(date -u +%Y-%m-%d)"
printf 'merged PRs:       %s\n' "$total"
printf 'fleet-authored:   %s  (head branch mr/*)\n' "$fleet"
printf 'share:            %s%%\n' "$(( fleet * 100 / total ))"
printf 'latest merge:     %s\n' "$latest"
printf 'verify totals:    https://github.com/%s/pulls?q=is%%3Apr+is%%3Amerged\n' "$REPO"
printf 'verify fleet:     https://github.com/%s/pulls?q=is%%3Apr+is%%3Amerged+head%%3Amr%%2F\n' "$REPO"
