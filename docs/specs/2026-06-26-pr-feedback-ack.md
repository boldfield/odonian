# Feature spec — workers address all unaddressed PR feedback + acknowledge it

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Implemented, with later semantics superseded.**
> The list/ack commands exist in [pr-feedback.go](../../cmd/odonian/pr-feedback.go). Use the [reviewer-feedback repair](2026-09-19-reviewer-feedback-repair.md) for the later actionable-feedback and exact-acknowledgment rules, and [AGENT-API.md](../../AGENT-API.md) for current usage. The original incident and proposal remain below.

**Status:** board milestone (Odonian project). **Date:** 2026-06-26.

## Problem

On a rework, a worker today reads **only the single most-recent global
conversation comment** on its PR. The `pull_request` build/design
implement prompts say: `gh pr view <pr-url> --comments` lists
oldest→newest, "take the LAST one; it **supersedes all earlier
comments**." This drops feedback three ways:

1. **Earlier-but-still-open feedback is ignored.** If round N's comment
   raised points the round N+1 comment didn't repeat, they vanish.
2. **Inline (diff-anchored) review comments are never read at all** —
   only conversation/issue comments are fetched. Humans routinely leave
   line-level comments that the worker never sees.
3. **Nothing is acknowledged.** There is no addressed/unaddressed state,
   so feedback can't be tracked across rework rounds, and a reviewer or
   human can't tell what the worker actually handled.

## Goal

A reworking worker reads **every unaddressed** piece of feedback on its
PR — both global conversation comments and inline review-thread comments
— addresses each, and **acknowledges** each when addressed. The
acknowledgment is also what marks an item "addressed" so the next round
skips it.

## Approach

This is GitHub forge logic and belongs in Go next to the existing forge
code (`internal/forge/`: `SquashMerge`, `pr_review.go`, `OwnerToken`),
exposed as `odonian` CLI subcommands the prompts call — not a parallel
shell+jq implementation. Go gives CI-gated unit tests (the harness shell
tests are not run in CI) and a mockable HTTP seam (forge already has
`GitHubBaseURL`).

- **List unaddressed feedback.** Given a PR, return the items that still
  need attention: inline review threads that are **not resolved**
  (GitHub GraphQL `reviewThreads`, `isResolved == false`), plus global
  conversation comments that are **not authored by the worker bot** and
  **not yet acknowledged**. The bot's own login is derived from the
  authenticated user so its comments and acks are excluded.

- **Acknowledge an addressed item.** For an inline thread: post a reply
  noting the fixing commit and **resolve the thread**
  (`resolveReviewThread`). For a global comment: post a reply noting the
  fixing commit and add a 👍 reaction (the reaction is the durable
  "addressed" marker, since conversation comments have no native
  resolved state).

- **CLI surface.** `odonian pr-feedback list <pr-url>` emits the
  normalized unaddressed items; `odonian pr-feedback ack <pr-url>
  <item-id> <sha>` marks one addressed. Token resolution reuses the
  existing per-owner path (`forge.OwnerToken` by repo owner).

- **Prompts.** The `pull_request` implement prompts call
  `pr-feedback list`, address **every** returned item (inline + global),
  and `pr-feedback ack` each after fixing — replacing the
  "only-the-most-recent-comment-supersedes" instruction. The reviewer
  prompts are aligned so reviewers may leave inline + global feedback,
  do not pre-resolve their own threads, and treat resolved/acked items
  as done on re-review.

## Scope / boundaries

- **In scope:** the two forge functions (list, ack), the
  `odonian pr-feedback` CLI subcommands, the implement-prompt rewrite,
  and the reviewer-prompt alignment — for **`pull_request` delivery
  mode only** (`local_commit` mode commits directly and has no PR).
- **Out of scope:** changing the review/escalation state machine; the
  merger; non-GitHub forges.
- **No code in this spec.** Each task carries its own exact,
  mechanically-checkable acceptance criteria.

## Task DAG

1. `forge-feedback-list` (deps: none) — list unaddressed feedback, Go +
   tests.
2. `forge-feedback-ack` (deps: 1) — resolve/reply/react ack, Go + tests.
3. `cli-pr-feedback` (deps: 1, 2) — `odonian pr-feedback list/ack`
   subcommands.
4. `prompt-implement-rework` (deps: 3) — rewrite the rework section of
   both `pull_request` implement prompts.
5. `prompt-reviewer-align` (deps: 3) — align both `pull_request` review
   prompts to the convention. Runs parallel to task 4.

## Done definition

All five merged; `odonian pr-feedback list/ack` work against a PR with
mixed resolved/unresolved inline threads and global comments; a
reworking worker addresses every unaddressed item and leaves resolved
threads + acked comments behind; re-review sees only genuinely
outstanding items.
