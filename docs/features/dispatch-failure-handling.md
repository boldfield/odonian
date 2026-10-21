# Dispatch failure handling: hand claims back, back off per account

Status: specification, 2026-10-21. Three tasks implement it (D1–D3 below).

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

## Out of scope

- The fleet sharing one Codex refresh token across pods. That is a deployment concern, recorded separately: give each pod its own sign-in or use an API key.
- Server-side lease reaping. Expired leases are already claimable.
