# Dispatch failure handling: hand claims back, back off per account

Status: specification, 2026-10-21. D1–D3 implement the original design and merged as #502–#504. D4 and D5 were added on 2026-10-22, after the incident described in "What went wrong, again".

## What went wrong

On 2026-10-21 the fleet's Anthropic account hit its 5-hour session limit at 18:14 UTC ("You've hit your session limit · resets 7:50pm (UTC)"). The ChatGPT account had hit its weekly limit at 16:30 ("try again at Oct 27th, 2026 6:32 AM"). Over the next hour:

- Four workers each claimed a new legislation.guide research task every 15–20 seconds. 124 tasks sat `in_progress` with nobody working on them. Task `3e9c9193` was claimed at 18:20, 18:51 and 19:21 with no work in between.
- Reviews on other projects were orphaned the same way (an ohand review claimed at 18:14, a GPT review at 16:11). Each held its task until the 30-minute lease ran out.
- Recovery lags the reset by up to one lease. A task claimed just before the limit resets stays locked for 30 minutes afterwards.

Two defects combine.

**1. A failed dispatch never hands its claim back.** For research tasks, `admit_research_task` claims the task before any model process exists (`harness/agent.sh`, `odonian claim`). When the model exits non-zero, `dispatch()` finalizes the permit as `failed` and calls `clear_preclaim`. That only clears shell variables (`agent.sh:433`), so the task stays `in_progress` under the dead agent's lease. Build-track agents claim for themselves, and a session that dies after claiming leaves the same orphan. There is no way to hand a claim back: the documented transitions from `in_progress` are `review` (submit) and, for merge tasks, `done`. `POST /tasks/{id}/release` is the hold release, which does not unclaim.

**2. Backoff is per model name, but limits are per account.** `mark_model_unavailable` backs off one model name for 30 s, escalating by 30 s up to 300 s, and `pick_claimable_task` / the discovery loop skip only that name. `claude-opus-5-5`, `opus`, `sonnet`, `haiku` and `fable` share one Anthropic account. `gpt-5.5`, `gpt-5.6-sol` and `gpt-6-astra` share one ChatGPT account. After one failure, the next pass dispatches the next name on the same exhausted account. The reset time the CLI prints is ignored, so even the capped 300 s backoff retries an account that cannot recover for hours or days.

Sign-in failures belong with limits. Codex's `refresh_token_reused` ("Your refresh token has already been used… Please try signing in again") is an account-wide condition that never clears by itself.

## D1 — Hand a claim back (server and CLI)

Add `POST /tasks/{id}/unclaim` and `odonian unclaim <id> [--attempt <id>] --reason <text>`.

- Only the current owner may unclaim: the task is `in_progress`, `agent_id` matches the assignee, and for a research task the attempt matches the live attempt. Anything else answers 409 with the current state and changes nothing. The call is idempotent: a second unclaim by the same owner after success answers 409, which is final for the caller.
- Effect, in one transaction: state becomes `ready`, assignee and lease are cleared, and a `claim_released` event records agent, attempt and reason. Dependencies, review round, priority, `escalate` and `agent_merge` are unchanged. No review, rework or escalation counter moves. A claim handed back is not a failed attempt of the task.
- Research: an unclaim of a preclaimed task requires its permit to be finalized first (or finalizes it as `cancelled` in the same transaction; pick one and document it). The pool debit follows the existing rule that uncertain outcomes are never refunded.
- A task with an open PR, a submitted round or `review` state cannot be unclaimed (409).
- Tests: the owner unclaims and the task becomes claimable at once; a non-owner, a stale attempt, and a task in `review` are refused; the event is recorded. Document it in `docs/api.md` and `AGENT-API.md`, next to claim and heartbeat.

Source pointers: `internal/api/api.go:133-146` (claim, heartbeat and release routes), `handleClaimTask` (`:745`), `ClaimTask` (`internal/store/store.go:1925`).

## D2 — Back off per provider account, until the reset the CLI states (harness)

- **Accounts.** Map each dispatchable model to its provider account. Anthropic: every Claude model and alias, including tier aliases and pinned IDs. OpenAI: every Codex model. Keep the map in one place, derived from the same configuration that lists the models (`AGENT_CODEX_MODELS`, the `ANTHROPIC_DEFAULT_*_MODEL` pins), so a new model name cannot escape it. An unknown model is its own account.
- **Classification.** Classify a non-zero dispatch exit by the CLI's output:
  - a usage or session limit (the Claude "hit your session limit · resets …" and Codex "hit your usage limit … try again at …" forms);
  - a sign-in failure (401, `refresh_token_reused`, missing or expired credentials);
  - anything else, which keeps today's behaviour.

  Base the matching on fixtures captured from the real messages. Never log credential material.
- **Limits.** A limit marks the whole account unavailable until the stated reset plus a small margin. Parse both forms, including time zone and date. If the reset cannot be parsed, fall back to escalating backoff, but per account. Cap any single window (for example 7 days), and log the parsed reset once per window, not per pass.
- **Sign-in failures.** Mark the account unavailable for a fixed long window (for example 10 minutes), then probe once. Log a distinct, loud line ("sign-in failed for account …; re-authenticate") so it is not mistaken for credits.
- **Filters.** `model_unavailable` and both discovery filters check the account, not the name. No task is claimed or admitted for an unavailable account. That is the main fix: an exhausted account must cost zero claims.
- **Recovery.** When the window ends, one dispatch probes the account. Success clears it; failure re-arms it.
- **Scope.** Each pod keeps this state locally, as today. Sharing it across pods is out of scope.
- **Tests:** shell tests with a fake CLI that prints each captured message.
  - A Claude session limit stops dispatch of every Claude model until the stated reset.
  - A Codex weekly limit stops every Codex model and leaves Claude models dispatchable.
  - A sign-in failure gets the long window and the distinct log line.
  - An unparseable limit falls back to backoff.
  - While an account is unavailable, no `odonian claim` is issued for its models.

Source pointers: `harness/agent.sh` `mark_model_unavailable` / `model_unavailable` (about `:385-420`), `pick_claimable_task` (`:734`), the discovery loop filter (`:1208`), the failure branch in `dispatch()` (`:1064-1072`).

Depends on nothing. D3 depends on D2, because both edit `harness/agent.sh`.

## D3 — Hand back the claim on a failed dispatch (harness)

- **Hand it back.** When `dispatch()` returns non-zero for a backend reason (limit, sign-in failure, launch error rc 126/127, or any exit D2 attributes to the backend), and the selected or preclaimed task is still `in_progress` and assigned to this agent, hand it back with D1's `unclaim`. Use reason `backend_unavailable: <account>: <class>`. Research tasks are handed back after `finalize_permit`, in the order D1 specifies.
- **Leave it alone** when the task has moved on (submitted, blocked, superseded, or owned by someone else), when the stop was our own (`STOP`) or fenced, or when the model exited zero. A task that the model itself is still working on, for example after a headless resume, is never handed back.
- **Build-track agents** claim inside the session. Look up the task the loop selected (`SELECTED_TASK_ID`) and hand it back only if it is assigned to this agent.
- **Tests** (shell, fake CLI and fake server):
  - a research preclaim plus a limit exit ends with the task `ready` and claimable, and the permit finalized;
  - a build task the session claimed before dying is handed back;
  - a submitted task is left alone;
  - a fenced stop is left alone.

Depends on D1 (the endpoint) and D2 (the shared file and the classifier).

## What went wrong, again (2026-10-22)

Two failures that only affect one model ran for most of a day.

- **Fable ran out of credits** at about 00:18 UTC while Opus and Sonnet on the same Anthropic account kept working. Every Fable dispatch ended with `You're out of usage credits. Switch to another model to continue.`, about 300 failures an hour. Tasks escalated to Fable by the circuit breaker stalled.
- **gpt-5.5 stopped working on the fleet's ChatGPT sign-in.** Codex returns `ERROR: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."}}`. gpt-5.6-sol and gpt-6-astra on the same account work. That was another 300 failures an hour. manifests #203 removed gpt-5.5 from `AGENT_CODEX_MODELS` and the server allowlist. That list decides only whether a run uses Codex or Claude Code, so the reviewers kept claiming gpt-5.5 reviews and sent them to Claude Code. There they fail with `There's an issue with the selected model (gpt-5.5). It may not exist or you may not have access to it. Run --model to pick a different model.`

Two defects follow.

**3. D2 pauses the whole account for errors that affect only one model.** `classify_dispatch_failure` knows usage limits and sign-in failures. Everything else is `other`, and `mark_model_unavailable` records every class under `account_of(model)`. Released as it stands, one Fable or gpt-5.5 failure would pause every Claude or every Codex model on that pod for 30 s, rising to 300 s. Other boards always have Fable tasks ready, so each pod would keep failing on Fable and pausing Opus and Sonnet. With gpt-5.5 still in `AGENT_CODEX_MODELS`, gpt-5.6-sol would be paused too, and gpt-5.6-sol reviews every ohand task.

**4. A task's reviewers cannot be changed.** `review_models` is set at creation. `PATCH /tasks/{id}` accepts only `depends_on`, and `/supersede` copies the list. On 2026-10-22, about 86 open tasks on three boards still named gpt-5.5 as a reviewer. Every review they spawn fails, and nothing can redirect it.

## D4 — Back off a single model for model-specific failures (harness)

Pausing the whole account stays the rule for failures that really belong to the account: a stated usage or session limit, and a sign-in failure. Everything else backs off only the model that failed.

- **New class `model_unavailable`.** `classify_dispatch_failure` returns it when the CLI says that this model, not the account, cannot be served. At least these three verbatim tails must classify as `model_unavailable`:
  - Claude Code: `You're out of usage credits. Switch to another model to continue.`
  - Codex: `ERROR: {"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-5.5' model is not supported when using Codex with a ChatGPT account."}}`
  - Claude Code: `There's an issue with the selected model (gpt-5.5). It may not exist or you may not have access to it. Run --model to pick a different model.`

  The model error can come before trailing warning lines (`[claude-code:unrecognized_model] {...}`, `Claude Code doesn't recognize the model ...`), so it is not always on the last non-blank line. Keep D2's protection against quoted text: only match a line that starts with the error, optionally prefixed by `Error:`, `ERROR:` or `API Error:`. Search only the last few non-blank lines of output, and say in a comment how many and why.
- **Scope by class.** `limit` and `signin` are still recorded under `account_of(model)`. `model_unavailable` and `other` are recorded under a model key. `model_unavailable` holds that model for `ODONIAN_MODEL_UNAVAILABLE_WINDOW` (default 1800 s) and then probes once. `other` keeps the escalating 30–300 s backoff, now per model. A model whose own window is open is skipped, and so is a model whose account's window is open. Success clears the model's record. A success on another model of the same account does not clear it.
- **Aliases share one key.** Use the canonical name for the model key: `fable`, `opus`, `sonnet` and `haiku` resolve through the `ANTHROPIC_DEFAULT_*_MODEL` pins, so `fable` and `claude-fable-5-1` share one window. A model without a pin is its own key.
- **Hand back.** `model_unavailable` is a backend reason for D3, so the claim is handed back with reason `backend_unavailable: model:<name>: model_unavailable`.
- **Log lines** name the scope: `model <name> unavailable (...)` or `account <name> unavailable (...)`.
- **Tests** (shell, fake CLI), each using the verbatim tails above:
  - after a Fable credits failure, the next Opus task on the same pod dispatches at once and Fable tasks are skipped for the window;
  - after the gpt-5.5 400 with gpt-5.5 still in `AGENT_CODEX_MODELS`, a gpt-5.6-sol review dispatches at once;
  - the Claude Code "issue with the selected model" output followed by the two warning lines classifies as `model_unavailable`;
  - an `other` failure on one model does not hold its account's other models;
  - `fable` and `claude-fable-5-1` share a window;
  - a session limit and a sign-in failure still hold every model of the account (D2 regression);
  - the error text quoted inside tool output earlier in the transcript does not classify.

Owned paths: `harness/agent.sh` and its tests. Depends on nothing; D2 and D3 have merged. Release v0.24.0 only after D4 lands.

## D5 — Change a task's reviewers (server, CLI)

- **PATCH.** `PATCH /tasks/{id}` also accepts `review_models`, a non-empty array that replaces the whole list. Validate it exactly like task creation, including the allowlist (`400 UNKNOWN_MODEL`). `null` and `[]` return `400 INVALID_REVIEW_MODELS`, so a list is never cleared to the defaults silently. It is accepted on tasks that carry reviewers (not on `review` or `merge` tasks: `400 FIELD_NOT_UPDATABLE`) in `backlog`, `ready`, `in_progress`, `blocked` and `review`. It returns `409` on `approved` (landing may be under way) and on terminal states. `depends_on` keeps its current behavior, and both fields may be sent together.
- **The open round.** When the task is in `review`, the current round must still be able to finish:
  - a review child of this round that has not recorded a verdict and whose model is no longer in the list is retired with a note naming the change;
  - a child that is claimed is retired too, and its agent's later submit is refused by the existing fence;
  - for each entry in the new list without a live or finished child in this round, a replacement child is spawned in the same round, with the same spec the round's other children received;
  - children that already recorded a verdict stay and count;
  - the round then aggregates over exactly the new list.

  Research dispute lineage is keyed by reviewer model and slot. A replaced reviewer starts with no lineage, so its disputes do not carry over. Document that limit; do not invent a mapping.
- **Event.** Append `review_models_changed` to the task, with the old list, the new list and the actor. Retired and spawned children get their usual events.
- **Supersede.** `POST /tasks/{id}/supersede` also takes an optional `review_models`, with the same validation. Without it the replacement copies the list as today.
- **CLI.** Expose it through the command that already wraps `PATCH /tasks/{id}`, or add `odonian update <id> --review-models a,b` if none exists.
- **Docs.** Update `docs/api.md` (PATCH and supersede) and `AGENT-API.md`.
- **Tests:**
  - PATCH on `backlog`, `ready` and `blocked` tasks changes the list, and the next round spawns the new models;
  - PATCH on a task in `review` with one verdict in and one open child for a removed model retires that child, spawns the replacement in the same round, and the round approves or rejects on the new pair;
  - a claimed child that is retired cannot submit;
  - `null`, `[]`, an unknown model, a `review` task, an `approved` task and a terminal task are each refused with the stated code, and nothing changes;
  - supersede with and without `review_models`;
  - the CLI round trip.

Owned paths: `internal/api/`, `internal/store/`, `cmd/odonian/` (the CLI command), `docs/api.md`, `AGENT-API.md` and their tests. Depends on nothing.

## Out of scope

- The fleet sharing one Codex refresh token across pods. That is a deployment concern, recorded separately: give each pod its own sign-in or use an API key.
- Server-side lease reaping. Expired leases are already claimable.
- A claim-side model filter in the reviewer, so it never claims a model it cannot run. With D4, an unsupported model costs one probe every 30 minutes per pod, which is not worth another switch.
