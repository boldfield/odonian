# Odonian fleet harness

The headless pull-worker fleet that drains an Odonian board: model-pinned agents claim work,
run it via `claude -p` or `codex exec`, and submit for review — the human merges (unless a task
opts into `agent_merge`). Configuration lives in [`docs/configuration.md`](../docs/configuration.md);
day-to-day diagnosis in [`docs/operations.md`](../docs/operations.md).

## Requirements

- `odonian` CLI on `PATH` for board polling and work discovery
- An authenticated `claude` CLI for Claude-routed dispatches; run in a disposable sandbox or
  container because permission prompts are disabled (`--dangerously-skip-permissions`)
- An authenticated `codex` CLI only if `AGENT_CODEX_MODELS` names any model (see Routing)
- `gh` (GitHub CLI) for repo cloning and git authentication
- `jq` for JSON processing
- Bash 3.2+ (macOS ships 3.2; Linux and others typically have 4+)

For the complete build, configuration, authentication, and launch sequence, see
[`docs/running.md`](../docs/running.md#the-fleet-in-your-development-environment).

## One engine

`agent.sh` is the whole loop, parameterized by `--kind` (`implement|review|merge`) and an
optional `--model`. The wrappers are one-liners over it:

| Wrapper | Role |
|---|---|
| `worker.sh [slot]` | Generic implementer — claims `kind=implement`; model comes from the task |
| `reviewer.sh [slot]` | Generic reviewer — claims `kind=review`; model comes from the task |
| `merger.sh [slot]` | Non-LLM merger — claims `kind=merge`, merges via the API; no repo, no worktree, no model process |
| `fleet.sh --kind <k> --count <n> [--model <m>] [--delivery-mode <m>]` | Spawns `n` slots (`worker-N`/`reviewer-N`/`merger-N`) of one kind in one terminal |

Run slots with **distinct names**; each takes a persistent agent id per slot:

    ODONIAN_PROJECT=<id> ODONIAN_REPO=~/projects/<repo> ./worker.sh worker-1

One dispatch per discovery pass, then the loop re-lists. Ctrl-C interrupts the active
worker/reviewer session and exits. Pull-request slot worktrees are removed during cleanup,
including unpushed changes; wait for submission first if you want the task to finish. `fleet.sh`
signals its own process group and can exit on the first Ctrl-C without waiting for child
cleanup; there is no guaranteed second-interrupt phase or final exit message. The non-LLM
merger has no guaranteed drain either: Ctrl-C can interrupt its foreground merge command after
GitHub merges the PR but before the board records completion. Check both states after an
interruption.

## Scheduling

One comparator orders every candidate, at every priority value, across all allowed projects as
a single set: **priority descending, `created_at` ascending, task id ascending**. There is no
project shuffle and no per-project head-of-line pick; at the default priority (500) the oldest
eligible task wins. The server's `next` and the TUI use the same comparator within a project.

Eligibility still filters first: kind, project allowlist, readiness, dependencies, holds, a
model/account in a backoff window, and local admission deferrals. Ineligible head tasks are
skipped, not queued behind. Priority is not preemption: it never interrupts or revokes a live
claim; it applies at the next scheduling decision. See
[`docs/runbooks/priority-rollout.md`](../docs/runbooks/priority-rollout.md).

To keep the selected task stable across discovery and dispatch, the harness pins its choice:
`ODONIAN_SELECTED_TASK_ID` is exported so the model's in-session `odonian next` returns exactly
that task or nothing — never a fallback task. These pin variables are internal, set per
dispatch and cleared after it; see
[internal injected variables](../docs/configuration.md#internal-injected-variables-not-operator-tunables).

## Routing: Claude vs. Codex

The task's model decides the backend dynamically per dispatch. If the model is listed in
`AGENT_CODEX_MODELS` (comma-separated), the dispatch runs `codex exec` (with
`-c model_reasoning_effort=high` plus any `AGENT_CODEX_FLAGS`); otherwise it runs `claude -p`
(with any `AGENT_CLAUDE_FLAGS`). A slot started without `--model` serves every model its kind
claims, so one reviewer slot can alternate backends task by task. Removing a model from
`AGENT_CODEX_MODELS` does not stop its tasks being claimed — it reroutes them to `claude -p`,
which will fail if Claude cannot serve that name (that failure then backs off per model; see
below).

## Dispatch lifecycle

Two claim paths, both lease-monitored automatically by the harness. Standalone agents that
claim outside this harness get **no** automatic renewal — they must heartbeat themselves.

- **Legacy (build/design, and non-research reviews).** The harness selects the task, exports
  the pin, and the model claims it inside its own session (`next` + `claim`).
  `monitor_legacy_lease` runs beside the model process: it waits until the task is
  `in_progress` under this agent, then heartbeats it each renewal interval. It stops renewing
  when the task is submitted, raced away, or otherwise no longer ours. It never kills the model.
- **Research (preclaim).** The harness claims the exact task *before any model process
  exists*, in every pacing-policy mode; only what the claim does differs by mode
  (`ODONIAN_RESEARCH_POLICY_MODE`, default `disabled`). Under `enforce`, `odonian claim`
  atomically checks eligibility and pool capacity: an admitted claim debits a pool start and
  creates the durable permit/attempt, a denied one starts no model (below). Under `observe`
  with the model mapped to a pool, the claim is always granted but still creates a
  permit/attempt (debiting a start when the hypothetical decision is admit) and records what
  `enforce` would have decided. Under `disabled` — or `observe` with an unmapped model — the
  claim is a plain claim: no pool check, no debit, no permit/attempt. Only after a granted
  claim is the model launched, with `ODONIAN_PRECLAIMED_TASK_ID` exported (plus
  `ODONIAN_PRECLAIMED_ATTEMPT_ID` when an attempt exists); the prompt skips `next`/`claim` and
  fences every heartbeat/submit to that attempt when there is one. `monitor_dispatch` renews
  the task lease (attempt-fenced when applicable) and the permit, if any, for the life of the
  process. On positive evidence of lost ownership — attempt fenced/expired/finalized, or the
  task owned by another agent — it stops the stale process (TERM its group, KILL after a
  grace). Transient errors never kill work. Permit finalization after the process exits is
  best-effort, not guaranteed exactly-once delivery: the finalize call is retried a few times
  and is idempotent server-side (an already-finalized/fenced/expired permit answers as final),
  but if it never lands the harness logs it, clears its held admission, and the permit simply
  lapses at lease expiry with the start left debited. A submit ends task ownership, not the
  dispatch. See [`docs/research.md`](../docs/research.md) and
  [research pacing pools](../docs/configuration.md#research-pacing-pools).

Admission denials (exit 10 deferred, 11 busy/conflict, 3 lost) start no model and leave the
task untouched; the task is locally deferred for the server's retry hint and the loop moves to
other candidates. An ambiguous admission (transport error after retries) remembers its request
id and replays the same request later, so the server returns the original admission instead of
spending another start.

**Failure handling.** A non-zero dispatch exit is classified from the CLI's terminal message
only (never from text quoted earlier in the transcript):

- `limit` / `signin` are properties of the provider **account** (all Claude models share one
  Anthropic account; all Codex models one OpenAI account). A limit holds the account until the
  reset the CLI stated, plus a margin; an unparseable limit falls back to escalating backoff
  (30s steps, capped 300s). A sign-in failure holds the account for a fixed window and logs a
  distinct "re-authenticate" line.
- `model_unavailable` / `other` are properties of the **model** alone (aliases resolve through
  the `ANTHROPIC_DEFAULT_*_MODEL` pins, so an alias and its pinned id share one window). Other
  models on the same account keep dispatching.

While a window is open, no task for that account/model is claimed or admitted. When it elapses,
one dispatch probes; success clears the record, failure re-arms it. Windows and margins are
tunables in [`docs/configuration.md`](../docs/configuration.md#harness-harnessagentsh-and-its-wrappers).
Backoff state is local to each harness process (pod); it is not shared.

**Owner-fenced handback.** After a backend failure, if the selected/preclaimed task is still
`in_progress` under this agent, the harness attempts to hand it back with `odonian unclaim` (research:
after permit finalization, fenced to the attempt when the claim carried one) so it is
claimable at once if the request succeeds. A failed handback is logged and the claim can remain
until lease expiry. Submitted, blocked, superseded, fenced, or other-owned tasks are left alone.
Contract: [`POST /tasks/{id}/unclaim`](../docs/api.md#post-tasksidunclaim).

**Bounded session recovery (legacy only).** If a model exits cleanly while its task is still
`in_progress` under this agent (typically "waiting for a notification" that never reaches a
headless session), the harness resumes the same session with a "finish or block it" prompt, up
to `ODONIAN_MAX_RESUMES` times (default 3; `0` means none). Still unfinished after that — or if
the session cannot be resumed — the task is moved to `blocked` with a note quoting the model's
final message (secrets redacted). `ODONIAN_SESSION_RECOVERY=off` disables the mechanism.
Research dispatches are not resumed this way.

**Process cleanup.** Each dispatch stamps the model's environment with a unique
`ODONIAN_DISPATCH_ID`; descendants inherit it whatever their process group. After the model
(and any resumes) exit — success, failure, or stop — the harness reaps exactly the processes
carrying that marker (TERM, short grace, KILL) and logs what it reaped. Unmarked processes,
the harness, and its monitors are never signalled. On exit, cleanup also kills a tracked
model/monitor group, best-effort finalizes an admitted-but-undispatched research permit as
`cancelled` (same idempotent, may-lapse semantics as above), and removes the slot's worktrees. None of this runs if the harness itself is SIGKILLed.

## Code vs. state

The engine keeps **code** and **state** in separate trees, so they never mix:

- **Code + prompts** (`agent.sh`, the wrappers, `prompts/<delivery_mode>/<track>/<kind>.md` — e.g. `prompts/pull_request/build/review.md`) — versioned
  *here*, in the repo. The engine finds them via its own location, and reads the prompt **fresh each
  dispatch**, so editing it applies to the next task with no restart.
- **State + config** — lives under `$ODONIAN_HOME` (default `~/.odonian`), un-versioned: `env`
  (URL / token / project), `agents/<slot>.id` (persistent ids), `wt-*` (worktrees), `repos/`
  (on-demand repo clones in multi-project mode), and optionally `forge-tokens` (per-owner PATs).
  Create the directory with `mkdir -p ~/.odonian`, copy `env.example` → `~/.odonian/env` if no
  configuration exists, and fill in the URL, token, full project UUID, and repository path.

## Per-owner GitHub auth (`forge-tokens`)

A multi-project fleet may span repos owned by **different GitHub identities** (e.g. `boldfield/*`
and the dynamically-created `fAIctory/*`). A single `gh` login can't push to both, and you can't
pre-assign a "fAIctory fleet" because those repos appear at runtime — so the worker resolves the
right token **from the repo owner** (which Odonian already exposes via each project's `repo`).

Optional `~/.odonian/forge-tokens` pairs an owner with a PAT (`owner=token` per line; `#` comments
ok). The worker derives the owner from the project's repo URL, exports that owner's token as
`GH_TOKEN` for the clone + the dispatched worker's `git push`/`gh`, and **falls back to your default
`gh` auth** when an owner has no entry. The server separately reads its own `FORGE_TOKENS` file.
PR-watch, including its stale-PR cleanup, skips owners without a matching token, even for public
repositories. Cleanup triggered directly by `POST /tasks/{id}/supersede` also skips GitHub requests without
a matching token, logging the owner and PR. The merger reads its token only from `FORGE_TOKENS`,
defaulting to `~/.odonian/forge-tokens`; it has **no fallback to `gh` authentication or `GH_TOKEN`**. A missing
owner entry makes its merge request unauthenticated and unable to merge the PR. Tokens are not
stored in the board database or returned by the API.

    cp harness/forge-tokens.example ~/.odonian/forge-tokens
    # add e.g.  fAIctory=ghp_…   then:
    chmod 600 ~/.odonian/forge-tokens

Codex-routed reviewers authenticate separately: the `codex` CLI's own sign-in (in the fleet, a
`codex-auth` secret seeded from `~/.codex/auth.json`). That credential rotates on every refresh
and revokes its predecessor, so a snapshot copied into several pods decays; see
[`deploy/fleet/README.md`](../deploy/fleet/README.md). A decayed Codex sign-in surfaces here as
the `signin` backoff class, not as a task failure.

**Run the scripts straight from the repo — no symlinks into state.** Because the two trees are
independent, you invoke the code from `harness/` and it uses `~/.odonian` purely for state:

    cd ~/projects/odonian/harness
    ODONIAN_PROJECT=all ./worker.sh worker-1

(Symlinking the wrappers *into* `~/.odonian` would drop code into the state tree next to `repos/`
and `wt-*` — don't. If you want to invoke from anywhere, add `harness/` to `PATH` or alias the
wrappers; `$ODONIAN_HOME` still points the engine at your state. The engine itself resolves its
real directory through symlinks, so a `PATH` symlink *to* the repo copy is fine.)

## Project scope: single vs. multi

Set by `ODONIAN_PROJECT`:

- **A project uuid → single-project** (back-compat). The slot is pinned to that board and
  `ODONIAN_REPO`, with one worktree, including the repo-guard (refuse on repo mismatch).
- **`all` (or empty) → multi-project** (`pull_request` mode). The slot discovers projects with
  claimable work of its kind, collects *every* claimable candidate across allowed projects, and
  dispatches the best under the global comparator — cloning each project's repo on demand into
  `~/.odonian/repos/<owner-repo>/` and standing up a per-`(slot, repo)` worktree
  `wt-<slot>-<owner-repo>` (a worktree can't span repositories). One task per pass, then a
  fresh listing, so the next pick sees current priorities. `ODONIAN_PROJECTS=<id,id,…>`
  optionally restricts which projects multi-mode will touch. `local_commit` workers/reviewers
  reject multi-project mode and exit; give them a UUID.

      ODONIAN_PROJECT=all ./worker.sh worker-1     # one slot, all boards

Assumes each repo's default branch is `main` (master-default repos need the prompt
parameterized — not yet supported). Project-selection edge cases (env-file placeholder, empty
value, merger scope) are in [`docs/configuration.md`](../docs/configuration.md#harness-harnessagentsh-and-its-wrappers).

## Executable behavior specs

These shell tests are the runnable specification of the behaviors above. They use fake CLIs and
fake/temporary servers; run them directly, e.g. `bash harness/scheduling_test.sh`. Their
existence is not a claim they were run for any given change — run the ones covering your edit.

| Script | Covers |
|---|---|
| `harness_test.sh` | General engine/prompt contract |
| `scheduling_test.sh` | Global comparator (priority desc, `created_at` asc, id asc), no shuffle, selected-task pin |
| `priority_scheduling_smoke_test.sh` | End-to-end priority scheduling against a temporary real server |
| `lease_renewal_test.sh` | Legacy in-session claim + automatic lease renewal monitor |
| `research_admission_test.sh` | Research preclaim, permit renewal/fencing, deferral and ambiguous-claim replay |
| `research_pacing_smoke_test.sh` | Research pacing pools against a temporary real server |
| `account_backoff_test.sh` | Account vs. model backoff classification and windows |
| `account_dispatch_test.sh` | Dispatch behavior while an account/model window is open |
| `model_failover_test.sh` | Falling through to a healthy model's work after a failure |
| `session_resume_test.sh` | Bounded headless session recovery, then block |
| `dispatch_reaping_test.sh` | `ODONIAN_DISPATCH_ID`-marked descendant cleanup |
| `seed_demo_test.sh` | Demo seeding (`seed-demo.sh`) |

Also in this directory: `sbx.sh` / `sbx-agent-setup.sh` (sandbox demo launcher and in-sandbox
agent bootstrap; see [`docs/running.md`](../docs/running.md)) and `verify-model-config.sh`
(checks the `ANTHROPIC_DEFAULT_*_MODEL` pins agree between environment and env file).

## See also

- [`docs/configuration.md`](../docs/configuration.md) — configuration variables and defaults
- [`docs/operations.md`](../docs/operations.md) — diagnosing idle, blocked, held, deferred, backed-off boards
- [`docs/research.md`](../docs/research.md) — the research track end to end
- [`docs/README.md`](../docs/README.md) — documentation index
- [`docs/features/dispatch-failure-handling.md`](../docs/features/dispatch-failure-handling.md) — why handback and per-account backoff exist (October 2026 incident)
