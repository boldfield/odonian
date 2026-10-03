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
| `ODONIAN_RESEARCH_DEFAULT_MODEL` | unset | Default model for research tasks created without an explicit model. Must be in `ODONIAN_MODELS` if set. Recommended: `claude-opus-5-5`. When unset, research tasks without an explicit model use the default fallback (prefers `haiku`). |
| `ODONIAN_ESCALATION_LADDER` | same as `ODONIAN_MODELS` | Ordered tiers for the review circuit breaker's escalation path. Every entry must be in `ODONIAN_MODELS`. A model can be a valid reviewer without being on the ladder; `gpt-6.1-sol` via Codex is the current active example, with `gpt-5.5` retained for backwards compatibility. |
| `ODONIAN_ESCALATION_THRESHOLDS` | `haiku=8,sonnet=6,opus=4` | Per-model review-round threshold for build and design tasks. A rejection that pushes a task past its threshold trips the circuit breaker. A malformed value logs a warning and falls back to the defaults. |
| `ODONIAN_RESEARCH_ESCALATION_LADDER` | unset | Ordered tiers for the review circuit breaker's escalation path for research tasks. Empty or unset means research tasks never change tier on rejection. Every entry must be in `ODONIAN_MODELS`. A configured research ladder operates independently of the build/design ladder. |
| `ODONIAN_RESEARCH_ESCALATION_THRESHOLDS` | unset (treated as empty map) | Per-model review-round threshold for research tasks. Only applies if `ODONIAN_RESEARCH_ESCALATION_LADDER` is configured. When a research task's model is not on the research ladder, falls back to `ODONIAN_MAX_REVIEW_ROUNDS`. Build and design tasks use `ODONIAN_ESCALATION_THRESHOLDS` exclusively. |
| `ODONIAN_RESEARCH_ROUND_BUDGET` | `6` | Chain-wide rejected-review-round budget for research tasks (docs/features/research-track.md section 6). Counts rejected rounds across a research task's entire supersede chain — automatic escalation and manual `SupersedeTask` both count, and neither resets it. When the chain-wide count reaches the budget, the task is blocked with reason `decompose` instead of continuing or escalating, whatever its tier. The budget always takes precedence over research escalation: a rejection that would otherwise trip the per-tier circuit breaker blocks instead of escalating if it also reaches the budget. Must be a positive integer; the server refuses to start otherwise. Never applies to build or design tasks. |
| `ODONIAN_RESEARCH_ADJUDICATOR` | unset | Model assigned to finding-scoped adjudication review tasks when a reviewer maintains a worker-disputed research finding (docs/features/research-track.md section 5). Must be in `ODONIAN_MODELS`, or the server refuses to start. Must differ from both of the research task's review_models, checked per task at aggregation time by exact model-name string comparison (e.g., `opus` and `claude-opus-5-5` are not detected as the same). When unset or equal to one of the task's reviewers, a maintained dispute stays blocking with a `research_adjudication_unavailable` event explaining why. The ruling binds only the disputed finding and never votes on the round. |
| `ODONIAN_MAX_REVIEW_ROUNDS` | `5` | Threshold fallback for models with no entry in their respective escalation thresholds. |
| `ODONIAN_LEASE_TTL` | `5m` | Lease granted on claim and extended by each heartbeat. A task whose lease has lapsed is claimable again, so a session that outlives its lease loses the task to another worker. Kept generous in production because renewal is agent-driven. |
| `ODONIAN_EVENT_TERMINAL_RETENTION_DAYS` | `1` | At startup, audit events for tasks in terminal states older than this are pruned. Events for live tasks are never pruned. Research-track task events are kept indefinitely because the reviewer scorecard is computed from them. |
| `ODONIAN_PPROF` | unset | Enable Go runtime profiling on `/debug/pprof/` when set to exactly `true`. All pprof endpoints require the same bearer-token auth as every other protected route. When unset or any other value, `/debug/pprof/` returns 404. See [Runtime profiling with pprof](#runtime-profiling-with-pprof). |
| `ODONIAN_SLOW_REQUEST_MS` | `500` | Per-request latency logging threshold in milliseconds. Requests at or above this threshold log at INFO level; below it log at DEBUG. `/healthz` is never logged. A non-integer or negative value logs one warning at startup and falls back to the default. |
| `FORGE_TOKENS` | `~/.odonian/forge-tokens` | Path to the per-owner GitHub token file used by PR-watch, supersession PR cleanup, and `odonian merge`. See [Forge tokens](#forge-tokens). |
| `ODONIAN_RESEARCH_POLICY_MODE` | `disabled` | Research admission policy mode: `disabled`, `observe` or `enforce`. Validated at startup; an invalid value or pool configuration stops the server. This release only defines and validates the policy — it does not yet gate claims or dispatch. See [Research pacing pools](#research-pacing-pools). |
| `ODONIAN_RESEARCH_POOLS` | unset | JSON object mapping pool names to account pools for research LLM work; ignored when the mode is `disabled`. See [Research pacing pools](#research-pacing-pools). |

### Research tasks and escalation

Research tasks use a dedicated escalation ladder and thresholds, separate from the build/design path:

- **No escalation by default.** `ODONIAN_RESEARCH_ESCALATION_LADDER` is empty by default, so research
  tasks keep their assigned model tier. Escalation can be enabled by setting the research ladder.
- **Independent configuration.** A configured research ladder and thresholds operate independently of
  the build/design `ODONIAN_ESCALATION_LADDER` and `ODONIAN_ESCALATION_THRESHOLDS`.
- **Explicit task opt-out still applies.** A research task with `escalate=false` never escalates,
  even when a research ladder is configured.
- **Round budget via threshold fallback.** When no research ladder is configured, or a research task's
  model is not on the configured research ladder, its threshold falls back to `ODONIAN_MAX_REVIEW_ROUNDS`
  (default `5`). This differs from build/design tasks, whose fallback (for `haiku`, `sonnet`, and `opus`)
  is the built-in `ODONIAN_ESCALATION_THRESHOLDS` default of `haiku=8,sonnet=6,opus=4`, not
  `ODONIAN_MAX_REVIEW_ROUNDS`.
- **Chain-wide round budget takes precedence.** Independent of the per-tier threshold above,
  `ODONIAN_RESEARCH_ROUND_BUDGET` (default `6`) counts rejected review rounds across a research task's
  entire supersede chain — every escalation and every manual supersession — and never resets. Once the
  chain-wide count reaches the budget, the task blocks with reason `decompose` instead of escalating or
  continuing, even if the per-tier threshold hasn't been reached yet. The block event lists the blocking
  findings from every round in the chain, oldest first, so the owner can see whether they were shrinking
  or recurring.
- **Disputes are adjudicated per finding.** When a reviewer maintains a worker's dispute of a finding,
  `ODONIAN_RESEARCH_ADJUDICATOR` names a configured model that differs from both reviewers. The server
  spawns an adjudication task scoped to that finding alone; the adjudicator's ruling binds only that
  finding and never votes on the round. See the `ODONIAN_RESEARCH_ADJUDICATOR` variable above.

### Research pacing pools

The research pacing policy (`internal/policy`) decides, for one research LLM start, whether to admit it,
defer it until a time, or ask the caller to retry after an active dispatch finishes. It is a pure
module: the server parses and validates its configuration at startup, but nothing evaluates it yet, so
setting these variables launches no model and changes no task, claim or dispatch. No production rate is
assumed; with the defaults the policy is `disabled`.

**Modes** (`ODONIAN_RESEARCH_POLICY_MODE`):

- `disabled` (default): everything is admitted and nothing is tracked. `ODONIAN_RESEARCH_POOLS` is ignored.
- `observe`: everything is admitted; each decision also reports what `enforce` would have decided, so
  hypothetical denials can be counted. Observe does not limit spending.
- `enforce`: decisions are applied. Every model in `ODONIAN_MODELS` must be mapped to a pool, otherwise
  startup fails.

**Pools** (`ODONIAN_RESEARCH_POOLS`) is a JSON object keyed by pool name. Unknown fields are rejected.

```json
{
  "pool_name": {
    "account_id": "string",
    "models": ["model1", "model2"],
    "start_rate": 0.5,
    "burst_capacity": 5,
    "concurrent_dispatch_limit": 3,
    "completion_reserved": 1
  }
}
```

- `account_id`: the external subscription/account. Pool state is keyed by it and shared across all
  projects and workers, so every alias on one account belongs in one pool; two pools with the same
  account are rejected. Renaming an `account_id` (even to fix a typo) makes it a new, never-seen
  account that starts with a full burst and zero active dispatches, so treat it as resetting that
  pool's allowance. Repeated pool names in the JSON object are rejected.
- `models`: non-empty list from `ODONIAN_MODELS`. A model may appear in one pool only.
- `start_rate`: sustained starts per second; finite and greater than zero.
- `burst_capacity`: integer of at least 1; starts available at once when idle. Allowance never exceeds it.
- `concurrent_dispatch_limit`: integer of at least 1; total active dispatches, completion work included.
- `completion_reserved`: integer from 0 to `concurrent_dispatch_limit`. Slots only completion work may
  use. A reservation equal to the limit makes a review-only pool.

Missing, non-numeric, nonfinite, negative or fractional-where-integer values, duplicate model
mappings and reservations above the limit are all rejected.

**Which work is paced.** Only research LLM work: first-pass writing, reviews, rework and adjudication each
spend one start. Build, design and non-LLM merge work is always admitted and uses no allowance.

**Reservation.** First-pass writers may hold at most `concurrent_dispatch_limit - completion_reserved`
slots and can never take a reserved slot, even when none is in use; there is no automatic borrowing.
Completion work may use any free slot up to the total limit. The reservation is on concurrency only; every
paced start spends from the same start-rate allowance.

**Outcomes.**

- Admit: start now; one start is debited and a ticket tracks the active dispatch until released.
- Timed deferral (`rate`): the bucket is empty; retry at the returned not-before time.
- Concurrency-dependent retry (`concurrency` or `reserved_capacity`): an active dispatch must finish
  first. No finish time is invented; a bounded retry interval is returned and no start is spent.
- Unmapped: the model has no pool. Validation prevents this under `enforce` for allowed models; it is
  reported separately so it is never read as a timed deferral.

**Time and changes never mint allowance.** The caller supplies server time. Time earlier than any time
already seen is treated as the latest time seen, so a clock rollback refills nothing and deferral times are
measured from that high-water mark. Refill is fractional and capped at `burst_capacity`. On a policy
change, allowance is first settled at the old rate, then clamped to the new burst; active dispatches are
kept and never interrupted, and a lowered limit simply blocks new starts until the count drains. Removing
and re-adding an account does not refill it, and a ticket releases against the account that admitted it
even if its model has since moved pools. Only an account never seen before starts with a full burst.
Released dispatches free concurrency but do not refund starts.

**Start-rate proxy versus billing.** `start_rate`, `burst_capacity` and the concurrency limit count task
starts and active dispatches. They are proxies for subscription consumption, not exact billing: a start
can use very little or a great deal of quota, other activity on the same external account is invisible,
and nothing here translates starts into a percentage of a subscription. Choose values from observed
dispatch durations and quota trends with headroom.

**Example:**

```bash
export ODONIAN_RESEARCH_POLICY_MODE=observe
export ODONIAN_RESEARCH_POOLS='
{
  "meta": {
    "account_id": "meta-power",
    "models": ["haiku"],
    "start_rate": 0.1,
    "burst_capacity": 2,
    "concurrent_dispatch_limit": 2,
    "completion_reserved": 1
  },
  "anthropic": {
    "account_id": "anthropic-main",
    "models": ["opus", "sonnet"],
    "start_rate": 0.2,
    "burst_capacity": 4,
    "concurrent_dispatch_limit": 4,
    "completion_reserved": 2
  }
}
'
```

The numbers above are illustrative only, not recommendations.

See `docs/features/research-pacing-and-reviewer-evaluation.md` for the full design.

### Runtime profiling with pprof

When `ODONIAN_PPROF=true`, the server registers Go's standard `net/http/pprof` handlers under
`/debug/pprof/` on its own mux, each wrapped in the same bearer-token auth middleware as every
other protected route. Capture a 30-second CPU profile with `curl`, then open it with `go tool
pprof`:

```bash
curl -sf -H "Authorization: Bearer $ODONIAN_TOKEN" \
  -o cpu.pprof \
  "http://localhost:8080/debug/pprof/profile?seconds=30"
go tool pprof -http=:8081 cpu.pprof
```

Other named profiles (`heap`, `goroutine`, `block`, `mutex`, ...) are available the same way, e.g.
`curl -sf -H "Authorization: Bearer $ODONIAN_TOKEN" http://localhost:8080/debug/pprof/heap`.

The review circuit breaker, in full: when every review task for a parent is done and at least one
rejected, the parent's `review_round` is compared with its model's threshold. At or under the
threshold, the parent returns to `ready`. Over it, if the task has `escalate=true` (the default at
creation) and its model is not the top of the ladder, the task is superseded by a copy pinned to
the next tier and that copy is promoted to `ready`; otherwise the parent moves to `blocked`.
The replacement preserves the original `track`, including for design work; see
[supersession](./api.md#post-tasksidsupersede).
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
| `PRWATCH_RATE_LIMIT_FLOOR` | `1500` | Minimum remaining GitHub primary-quota budget PR-watch will leave for an owner's token. Once per owner per pass, before its first GitHub call, PR-watch checks the token's remaining quota (a free call); if it's at or below this floor, PR-watch enters the same per-owner backoff it uses after a 403 and skips the rest of that owner's checks until the quota window resets. `0` disables the check. The default of 1500 leaves headroom in the shared `odonian-forge-tokens` identity for the fleet's workers and reviewers, which need the quota more urgently than the reconciler does. |

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

The server reads a per-owner token file for PR-watch and stale-PR cleanup. The fleet and merger
need credentials in their own runtime environments as well; configuring only the server does
not authenticate workers. Each file contains one `owner=token` pair per line; quoted tokens and
`#` comments are allowed:

```
# ~/.odonian/forge-tokens (or $FORGE_TOKENS)
owner1=token_for_owner1
owner2="token_for_owner2"
```

PR-watch skips an owner when its token is missing, even for public repositories. It logs
`no forge token for owner` when the skipped count first appears or changes, and logs when the
owner is no longer skipped. Add the matching token to the server's file to enable checks;
the file is read again on later passes. An empty file disables PR-watch's checks, including its
stale-PR cleanup. Cleanup triggered by `/supersede` also skips GitHub requests when the matching
token is missing or empty, logging the owner and PR once for that cleanup attempt. The board
supersession still commits successfully.

The worker/reviewer harness has a separate fallback to its local `gh` authentication; that does
not authenticate the server or merger. The merger reads only its per-owner token file.
It does not fall back to `gh` authentication or `GH_TOKEN`; a
missing owner entry results in an unauthenticated merge request, which cannot merge the PR.

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
| `ODONIAN_WORKTREE_HOME` | `$ODONIAN_HOME` in the CLI | Per-task worktree root in `local_commit` mode. The CLI requires one of these variables; the worker harness requires `ODONIAN_WORKTREE_HOME` explicitly. Use persistent storage for work that must survive a reboot. The sandbox demo deliberately uses `/tmp/odonian/worktrees`. |

## Harness (`harness/agent.sh` and its wrappers)

The harness sources `$ODONIAN_HOME/env` (copy `harness/env.example`). Every value can be
overridden per invocation.

| Variable | Default | Meaning |
|---|---|---|
| `ODONIAN_URL`, `ODONIAN_TOKEN` | required | As above. |
| `ODONIAN_PROJECT` | depends on the env file; see below | Set a full project UUID for one board. In `pull_request` mode, literal `all` discovers and drains projects with claimable work, cloning repos on demand. `local_commit` workers/reviewers require a UUID. |
| `ODONIAN_PROJECTS` | unset | In `all` mode, a comma-separated allowlist of project ids. |
| `ODONIAN_REPO` | | Local checkout for single-project mode. Ignored in `all` mode. |
| `ODONIAN_MAIN_REPO` | `$ODONIAN_REPO` | The canonical clone that worktrees are detached from. |
| `ODONIAN_REPOS_HIGH_GIB`, `ODONIAN_REPOS_LOW_GIB` | `14`, `8` | Disk watermarks for the on-demand clone cache in `all` mode: when usage crosses the high mark, clones are evicted until it is under the low mark. |
| `AGENT_SLOT` | wrapper default | Slot name (`worker-1`, `reviewer-2`, …). Each slot gets a persistent agent id and its own worktree. |
| `AGENT_CLAUDE_FLAGS` | empty | Extra flags appended to every `claude -p` dispatch. `sbx.sh` uses it to pass the flag a nested `claude` needs inside a sandbox. |
| `AGENT_CODEX_MODELS` | unset | Comma-separated models to dispatch through `codex exec` instead of `claude -p`, e.g. `gpt-6.1-sol` or `gpt-5.5` (for backwards compatibility). Review-only in practice. |
| `AGENT_CODEX_FLAGS` | unset | Extra flags for `codex exec`, on top of the hardcoded `-c model_reasoning_effort=high`. |

Project selection is evaluated after sourcing `$ODONIAN_HOME/env`. The example file supplies
the placeholder `<project-uuid-or-all>` when the variable was unset or empty; replace it before
starting the fleet. That placeholder is treated as a project ID and can leave the slot polling
an empty queue indefinitely. With a different env file, or none, an unset or empty value after
configuration selects **all projects visible to the shared board token** in `pull_request`
mode. `local_commit` workers and reviewers reject this multi-project mode and exit; set a full
UUID for them, as the demo does. Choose an explicit UUID or, for `pull_request`, literal `all`
instead of relying on implicit scope. The repo-less merger uses all-project discovery for an
unset/empty value regardless of delivery mode.

Codex-routed reviewers authenticate with a `codex-auth` secret seeded from `~/.codex/auth.json`.
That credential rotates on every refresh and revokes its predecessor, so a snapshot copied into
several pods decays. [`deploy/fleet/README.md`](../deploy/fleet/README.md) covers the operational
consequences and the `make codex-auth` targets.
