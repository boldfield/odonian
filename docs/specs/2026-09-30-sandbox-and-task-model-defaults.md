# Sandbox and new-task model defaults

**Date:** 2026-09-30  
**Status:** Approved  
**Task:** Update sandbox and new-task model defaults

## Requested behavior

Preserve the current implementation tracks and escalation order while selecting Haiku 4.5, Sonnet 5.5, and Opus 5.5 for their existing tier aliases. Use GPT-6.1 Sol instead of GPT-5.5 in active defaults for Codex reviews and new task templates.

| Existing tier or role | Requested provider model ID |
| --- | --- |
| haiku | claude-haiku-4-5-20251001 |
| sonnet | claude-sonnet-5-5 |
| opus | claude-opus-5-5 |
| Default Codex reviewer | gpt-6.1-sol |

Provider references verified on September 29, 2026:

- https://platform.claude.com/docs/en/models/overview
- https://developers.openai.com/api/docs/models/gpt-6.1-sol

## Existing deployment and constraints

The production server allowlists haiku, sonnet, opus, fable, gpt-5.5, claude-opus-5-5, gpt-5.6-sol, gpt-6-astra, and claude-fable-5-1. Its implementation escalation ladder is haiku, sonnet, opus, fable, with rejection thresholds 3, 2, 2, and 1 respectively. Research defaults and adjudication already use claude-opus-5-5; the current research reviewer pair is GPT-6 Astra and Claude Fable 5.1. These settings remain as configured.

Production worker and reviewer manifests already pin the opus and fable aliases. They do not yet pin haiku or sonnet. The production reviewer routes gpt-5.5, gpt-5.6-sol, and gpt-6-astra through Codex. The sandbox still advertises GPT-5.5 in its allowlist, routing configuration, auth verification, and generated board instructions.

Keep task model strings haiku, sonnet, opus, and fable intact so existing task claims, thresholds, and supersession continue to match. Pin Claude alias resolution with the established ANTHROPIC_DEFAULT_HAIKU_MODEL, ANTHROPIC_DEFAULT_SONNET_MODEL, and ANTHROPIC_DEFAULT_OPUS_MODEL settings. Keep GPT-6.1 Sol outside the implementation escalation ladder.

Retain existing allowlist and dispatch entries for older explicitly pinned tasks. Changing a default must not silently rewrite historical records or in-flight task assignments. Existing research choices, authentication mechanisms, independent reviews, and human merge gates remain intact. No server schema or generic model-routing framework is required.

The published fleet image currently packages Claude Code 2.1.281 and Codex 0.156.1. Verify that these exact runtimes accept the requested model IDs. Only change a runtime pin if a reproducible compatibility failure requires it; record the supported version and, if a new image is needed, make the release and image pin an explicit prerequisite to production rollout.

## Approved task

Update sandbox and new-task model defaults

Project: Odonian. Initial model: haiku. Escalation: enabled. Independent reviewers: opus and gpt-5.5. Human merge: required. May be implemented independently of task 1; production-created Sol review work starts only after the allowlist and reviewer route are deployed.

Intent: stop newly generated Odonian sandbox configuration and board instructions from selecting GPT-5.5, and pin the requested Claude variants consistently with production.

## Acceptance criteria

1. Sandbox-generated fleet configuration and child process environments resolve haiku, sonnet, and opus to the exact provider IDs above. Preserve intentional operator overrides where the existing interface supports them.

2. Allowlist and route gpt-6.1-sol through codex exec in sandbox mode. Retain routing support for gpt-5.5 so reused sandbox databases with explicitly pinned work keep functioning.

3. New-task instructions and active task-creation examples use the independent reviewer pair opus and gpt-6.1-sol. Generic server review fallback behavior remains unchanged; the pair is passed explicitly at task creation.

4. Codex auth verification invokes gpt-6.1-sol. Update directly related messages and current operator guidance. Preserve historical incidents, old task records, and unrelated model fixtures.

5. Preserve the sandbox implementation ladder, thresholds, task kinds/tracks, explicit research choices, authentication flow, Codex high reasoning effort, and human merge gate.

6. Use existing harness verification or a focused stubbed check to confirm the generated environment, reviewer pair, allowlist, and vendor dispatch agree. Run the required repository checks and record any environment-dependent smoke check limitations.

7. Open a PR with agent_merge=false. A fleet image release is only needed if model compatibility verification proves the existing runtime insufficient.
