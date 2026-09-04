# Configuration reference

Everything is configured through environment variables. This page lists all of them, grouped by
the process that reads them. Defaults are what the code does when the variable is unset.

## Server (`odonian server`)

| Variable | Default | Meaning |
|---|---|---|
| `ODONIAN_TOKEN` | required | Bearer token. Every endpoint except `GET /healthz` requires `Authorization: Bearer <token>`. |
| `ODONIAN_DB` | `odonian.db` | SQLite database path, created on first run. Opened with `journal_mode=WAL`, `foreign_keys=ON`, and `busy_timeout=5000`. |
| `ODONIAN_ADDR` | `:8080` | Listen address. |
| `ODONIAN_MODELS` | `haiku,sonnet,opus` | Allowlist of model names that may be pinned to a task or named as a reviewer. |
| `ODONIAN_ESCALATION_LADDER` | same as `ODONIAN_MODELS` | Ordered tiers for the review circuit breaker's escalation path. Every entry must be in `ODONIAN_MODELS`. A model can be a valid reviewer without being on the ladder; `gpt-5.5` via Codex is the usual example. |
| `ODONIAN_ESCALATION_THRESHOLDS` | `haiku=8,sonnet=6,opus=4` | Per-model review-round threshold. A rejection that pushes a task past its threshold trips the circuit breaker. A malformed value logs a warning and falls back to the defaults. |
| `ODONIAN_MAX_REVIEW_ROUNDS` | `5` | Threshold for models with no entry in `ODONIAN_ESCALATION_THRESHOLDS`. |
| `ODONIAN_LEASE_TTL` | `5m` | Lease granted on claim and extended by each heartbeat. A task whose lease has lapsed is claimable again, so a session that outlives its lease loses the task to another worker. Kept generous in production because renewal is agent-driven. |
| `ODONIAN_EVENT_TERMINAL_RETENTION_DAYS` | `1` | At startup, audit events for tasks in terminal states older than this are pruned. Events for live tasks are never pruned. |
| `FORGE_TOKENS` | `~/.odonian/forge-tokens` | Path to the per-owner GitHub token file used by the PR-watch reconciler and by `odonian merge`. See [Forge tokens](#forge-tokens). |

The review circuit breaker, in full: when every review task for a parent is done and at least one
rejected, the parent's `review_round` is compared with its model's threshold. At or under the
threshold, the parent returns to `ready`. Over it, if the task has `escalate=true` (the default at
creation) and its model is not the top of the ladder, the task is superseded by a copy pinned to
the next tier and that copy is promoted to `ready`; otherwise the parent moves to `blocked`.
Unblocking (`blocked → ready`) clears the assignee and lease but does not yet reset the review
round, so one more rejection re-trips the breaker. Resetting it is specced in
[`docs/specs/2026-08-06-unblock-resets-review-round.md`](./specs/2026-08-06-unblock-resets-review-round.md).

## Notifier

The notifier is one of the level-triggered reconcilers on the server's reconcile runner. It is a
no-op when `NOTIFY_URL` is unset: nothing is sent and nothing is logged.

| Variable | Default | Meaning |
|---|---|---|
| `NOTIFY_URL` | unset | Webhook to POST notifications to. Unset disables the notifier. |
| `NOTIFY_TOKEN` | required if `NOTIFY_URL` is set | Sent as `Authorization: Bearer <token>` to the webhook. |
| `NOTIFY_INTERVAL` | `30s` | Tick interval for the reconcile runner. This is the cadence for every reconciler, PR-watch included, not only the notifier. |
| `NOTIFY_FAILED_WINDOW` | `1h` | A `failed` task is notified only if it failed within this window. |

Events the server publishes:

| Task state | Event | Priority | Notes |
|---|---|---|---|
| `approved` | `odonian-review` | P2 | Passed review; awaits the human merge decision. |
| `blocked` | `odonian-blocked` | P2 | Circuit breaker or operator; needs a human. |
| `failed` | `odonian-failed` | P3 | Recency-windowed by `NOTIFY_FAILED_WINDOW`. |
| PR merged | `odonian-merged` | | Published by PR-watch when a linked PR merges. |

The notifier has no dedup: a task that matches is re-published on every tick until it leaves the
matching state. Topics are dashed (`odonian-review`, not `odonian.review`) because ntfy rejects
dots in topic names.

```bash
export NOTIFY_URL="https://notifier.example.com/notify"
export NOTIFY_TOKEN="your-secret-token"
./bin/odonian server
```

## PR-watch reconciler

PR-watch runs on the same runner and needs no configuration beyond `FORGE_TOKENS`. It watches
`approved` tasks with `agent_merge=false` that carry a `pr` link, fetches PR state and review
decisions from GitHub, and applies:

| PR state | Action |
|---|---|
| merged | Task → `done`; publishes `odonian-merged`. |
| closed, unmerged | Task → `abandoned` (terminal). |
| open, "changes requested" newer than the approval | Task → `ready`; posts a marker comment on the PR explaining the bounce. |
| anything else | No action. |

### Forge tokens

Tokens are server-side and per GitHub owner. The file is one `owner=token` pair per line;
quoted tokens and `#` comments are allowed:

```
# ~/.odonian/forge-tokens (or $FORGE_TOKENS)
owner1=token_for_owner1
owner2="token_for_owner2"
```

If the file does not exist, GitHub calls are made unauthenticated. Public repos may work under
GitHub's 60 requests/hour unauthenticated limit; private repos fail with 401/404, logged every
tick. For a deployment with no GitHub integration, point `FORGE_TOKENS` at an empty file.

## CLI (`odonian <command>`)

| Variable | Default | Meaning |
|---|---|---|
| `ODONIAN_URL` | required | Base URL of the server. |
| `ODONIAN_TOKEN` | required | Bearer token. |
| `AGENT_ID` | | Agent identity sent on `claim`, `heartbeat`, and `submit`. The harness sets it per slot. |
| `AGENT_MODEL` | | Fallback model for `claim` when `--model` is not given. |
| `ODONIAN_MODEL` | `fleet` | Model name in the `<model>-<role>:` marker stamped on `pr-feedback ack` replies (`haiku-worker:`), so acknowledgements are attributable to a tier under the fleet's shared GitHub identity. |
| `GH_TOKEN` | | Fallback GitHub token for `pr-feedback` when no per-owner forge token applies. |
| `ODONIAN_DELIVERY_MODE` | `pull_request` | `pull_request` (branch + PR on a forge) or `local_commit` (the CLI commits into a local repo; no forge). |
| `ODONIAN_HOME` | `~/.odonian` | Root for harness state: agent ids, worktrees, repo clones, `env`, `forge-tokens`. |
| `ODONIAN_WORKTREE_HOME` | `$ODONIAN_HOME` | Per-task worktree root in `local_commit` mode. One of the two must be set in that mode, and the harness refuses a root under `/tmp` because bounced work must survive a reboot. |

## Harness (`harness/agent.sh` and its wrappers)

The harness sources `$ODONIAN_HOME/env` (copy `harness/env.example`). Every value can be
overridden per invocation.

| Variable | Default | Meaning |
|---|---|---|
| `ODONIAN_URL`, `ODONIAN_TOKEN` | required | As above. |
| `ODONIAN_PROJECT` | required | A project id to pin the slot to one board, or `all` to discover and drain every project with claimable work, cloning repos on demand. |
| `ODONIAN_PROJECTS` | unset | In `all` mode, a comma-separated allowlist of project ids. |
| `ODONIAN_REPO` | | Local checkout for single-project mode. Ignored in `all` mode. |
| `ODONIAN_MAIN_REPO` | `$ODONIAN_REPO` | The canonical clone that worktrees are detached from. |
| `ODONIAN_REPOS_HIGH_GIB`, `ODONIAN_REPOS_LOW_GIB` | `14`, `8` | Disk watermarks for the on-demand clone cache in `all` mode: when usage crosses the high mark, clones are evicted until it is under the low mark. |
| `AGENT_SLOT` | wrapper default | Slot name (`worker-1`, `reviewer-2`, …). Each slot gets a persistent agent id and its own worktree. |
| `AGENT_CLAUDE_FLAGS` | empty | Extra flags appended to every `claude -p` dispatch. `sbx.sh` uses it to pass the flag a nested `claude` needs inside a sandbox. |
| `AGENT_CODEX_MODELS` | unset | Comma-separated models to dispatch through `codex exec` instead of `claude -p`, e.g. `gpt-5.5`. Review-only in practice. |
| `AGENT_CODEX_FLAGS` | unset | Extra flags for `codex exec`, on top of the hardcoded `-c model_reasoning_effort=high`. |

Codex-routed reviewers authenticate with a `codex-auth` secret seeded from `~/.codex/auth.json`.
That credential rotates on every refresh and revokes its predecessor, so a snapshot copied into
several pods decays. [`deploy/fleet/README.md`](../deploy/fleet/README.md) covers the operational
consequences and the `make codex-auth` targets.
