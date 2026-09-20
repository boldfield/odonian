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

**Detection rules — SUPERSEDED, kept for historical context only.** The rules below shipped
with this spec and are the ones that caused the Run03 Referee incident: they treat marker
authorship itself as proof a comment is addressed, which silently drops actionable marked
reviewer feedback (e.g. `gpt-5.5-reviewer: CHANGES REQUESTED`). Do not implement these; see
[the corrected rules](2026-09-19-reviewer-feedback-repair.md#required-behavior) instead, which
use the marker grammar above only as an authorship/role parser, not a completion rule.

- ~~*Skip-own*: a comment is skipped as the fleet's own iff it matches the marker grammar.~~
  Wrong: this also skips actionable marker-prefixed reviewer requests. The corrected rule
  classifies by *role*: worker/merger/reconciler markers are always non-actionable, but a
  reviewer marker is non-actionable only if it is a canonical approval; every other reviewer
  message (including unrecognized ones) remains visible feedback.
- ~~*Reply-ack*: a global comment counts as addressed iff a LATER reply in the conversation
  matches the marker grammar.~~ Wrong: any later marked comment, regardless of content, could
  clear every earlier comment — including a different reviewer's unrelated request. The
  corrected rule requires a later *worker* reply naming the *exact original comment ID* and a
  fixing commit, in the existing writer's `addressed in <sha> (see comment <id>)` format.
- ~~*Thread-ack*: an inline review thread counts as addressed iff it is resolved OR its last
  reply matches the marker grammar.~~ Wrong: a marked reviewer comment or worker reply left in
  an unresolved thread does not resolve it. The corrected rule relies solely on GitHub's
  `isResolved` state, which `pr-feedback ack` sets via `resolveReviewThread`.
- ~~*Reaction-ack* (👍 by `botLogin`) is retained as-is.~~ Wrong: a bare reaction is not an
  explicit acknowledgment. The corrected rule requires the exact-ID worker reply described
  above; reactions no longer acknowledge anything.

**Ack stamping.** `odonian pr-feedback ack` must emit reply bodies that begin with a worker
marker so its own acks are recognized on the next `list`. Default prefix:
`${ODONIAN_MODEL:-fleet}-worker: ` with an optional `--marker` flag override. This part is
unchanged by the repair; only the exact-ID requirement above is new.

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
