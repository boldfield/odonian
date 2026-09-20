# PR feedback under a single GitHub identity

**Status:** approved · **Date:** 2026-08-18 · **Supersedes behavior from:** `2026-06-26-pr-feedback-ack.md` (detection logic only; the CLI surface and prompt contract are unchanged)

> The detection and acknowledgment rules below are superseded by the approved [reviewer feedback repair](2026-09-19-reviewer-feedback-repair.md). Agent authorship alone does not make feedback addressed. This document retains the historical problem statement and original design for context.

## Problem

The rework-feedback loop (`odonian pr-feedback list|ack`) is partially blind in production.
`listUnacknowledgedGlobalComments` classifies comments as bot-authored via
`Author.Login == botLogin`, where `botLogin` is the login of the token's authenticated user.
The fleet acts on GitHub with the repo owner's PAT, so the human reviewer and the fleet share
one login: **global comments** from the human are filtered out as "the bot's own" and never
surface in `pr-feedback list` (first diagnosed 2026-08-04 on a trade-log rework). Inline
review threads are not author-filtered and do surface; they are included in this spec only so
one uniform marker rule governs both paths. The feature's tests model distinct identities; the
deployment has one.

A second GitHub identity is a known dead end: a machine-user PAT was tried on 2026-08-05 and
reverted after GitHub flagged the new account and throttled it to unauthenticated rate limits,
stalling the whole fleet. The fix must work under the shared identity.

## Fix: classify by marker, not by login

The fleet already has a self-identification convention born of the same shared-login problem:
comment prefixes like `opus-reviewer:`. Extend that convention into the classifier.

**Marker grammar.** A comment body is *agent-authored* iff, after leading whitespace, it
matches:

```
<token>-(worker|reviewer|merger|reconciler):
```

where `<token>` is one or more of `[a-z0-9.-]` (e.g. `haiku-worker:`, `gpt-5.5-reviewer:`).
Nothing else — login equality is removed from authorship classification entirely (it cannot
distinguish anything in a single-identity deployment, and a marker-only rule still behaves
correctly if distinct bot identities arrive later).

**Detection rules** (in `internal/forge/feedback.go` and the inline-thread path):

> ⚠️ **Superseded — do not implement these rules.** Every rule in this subsection is replaced
> by [reviewer feedback repair](2026-09-19-reviewer-feedback-repair.md). They are retained only
> as the historical record of the faulty behavior that caused the Run03 Referee lost-feedback
> incident. The marker grammar itself is still used, but *only as an authorship/role parser* —
> never as a completion/acknowledgment rule. Under the corrected contract:
>
> - *Skip-own* is replaced by **role-aware classification**: a marker identifies the author's
>   role, not that the comment is addressed. Reviewer requests (e.g. `gpt-5.5-reviewer: CHANGES
>   REQUESTED`) and unknown reviewer messages remain visible under shared and separate logins;
>   only worker acknowledgment/status, merger/reconciler status, and canonical reviewer
>   approvals are non-actionable.
> - *Reply-ack* is replaced by **exact-ID worker acknowledgment**: a global comment is addressed
>   only by a later worker comment naming the exact original comment ID and a fixing commit
>   (`addressed in <sha> (see comment <id>)`). An acknowledgment of B cannot clear A, and one
>   reviewer's approval cannot clear another's request.
> - *Thread-ack* is replaced by **resolution-only**: an inline thread is addressed iff it is
>   resolved. A marked reviewer comment or a worker reply that did not resolve the thread does
>   not clear it.
> - *Reaction-ack* is **removed**: a 👍 reaction no longer acknowledges anything. Items
>   previously cleared only by a reaction can reappear; that is an intentional correction.

- *Skip-own*: a comment is skipped as the fleet's own iff it matches the marker grammar.
- *Reply-ack*: a global comment counts as addressed iff a LATER reply in the conversation
  matches the marker grammar.
- *Thread-ack*: an inline review thread counts as addressed iff it is resolved OR its last
  reply matches the marker grammar.
- *Reaction-ack* (👍 by `botLogin`) is retained as-is: reactions cannot carry markers, and a
  human 👍-ing their own comment is an acceptable false-ack. Documented limitation.

**Ack stamping.** `odonian pr-feedback ack` must emit reply bodies that begin with a worker
marker so its own acks are recognized on the next `list`. Default prefix:
`${ODONIAN_MODEL:-fleet}-worker: ` with an optional `--marker` flag override.

## Non-goals

- No separate GitHub bot account or App identity (future option; marker rule already
  accommodates it).
- No change to the reviewer-side comment conventions, the CLI command surface, or the
  prompt-mandated rework gate itself.

## Verification

Unit fixtures must model the single-identity case explicitly (all authors share one login).
End-to-end: a human bounces a fleet PR with one inline thread and one global comment; the
rework worker's `pr-feedback list` shows both, its fixes and acks clear them, and a second
`list` returns nothing outstanding. Human-verified before this spec is considered done.
