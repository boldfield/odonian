# Specification and incident history

Status checked against repository baseline `b3da896` (v0.24.0), 2026-11-03.
These dated documents preserve the problems, decisions and implementation plans that
led to changes. Their original present-tense descriptions, task IDs, deployment
inventories and rollout checklists are historical. The status banner on each document
separates that history from the checked source behavior.

Start with the [documentation index](../README.md), [agent workflow](../../AGENT-API.md),
[API reference](../api.md), [research guide](../research.md) or
[operations guide](../operations.md) for current use. “Implemented” below means source
and regression coverage exist; it does not mean tests were rerun for this inventory,
that a feature is enabled in production, or that an external provider accepts a model.

| Document | Status at this baseline | Evidence or successor |
| --- | --- | --- |
| [PR feedback acknowledgment](2026-06-26-pr-feedback-ack.md) | Commands implemented; later feedback semantics superseded | [CLI implementation](../../cmd/odonian/pr-feedback.go); [September repair](2026-09-19-reviewer-feedback-repair.md) |
| [Board stall detection](2026-07-20-stall-detection.md) | **Not implemented.** No project-level stalled-claimable-work alert | [Notification reconciler](../../internal/notify/reconciler.go) still watches approved/blocked/failed tasks |
| [Supersede closes stale PR](2026-08-06-supersede-closes-stale-pr.md) | Implemented, with best-effort forge cleanup | [Store tests](../../internal/store/store_test.go); [PR watcher](../../internal/prwatch/reconciler.go) |
| [Unblock resets review round](2026-08-06-unblock-resets-review-round.md) | **Not implemented.** Unblocking preserves review_round | TransitionTask in the [store](../../internal/store/store.go) |
| [PR feedback with one identity](2026-08-18-pr-feedback-single-identity.md) | Marker-only ignore/ack rules superseded | [September repair](2026-09-19-reviewer-feedback-repair.md); [feedback regression tests](../../internal/forge/feedback_test.go) |
| [PR-watch quota burn](2026-09-10-prwatch-quota-burn.md) | Settled-link tombstoning and quota floor implemented | [Reconciler](../../internal/prwatch/reconciler.go) and [tests](../../internal/prwatch/reconciler_test.go) |
| [Reviewer feedback repair](2026-09-19-reviewer-feedback-repair.md) | Implemented | [Feedback tests](../../internal/forge/feedback_test.go); [submission-gate tests](../../cmd/odonian/submit_feedback_gate_test.go) |
| [Task escalation policy](2026-09-19-task-escalation-policy.md) | Dedicated policy endpoint implemented | [API handler](../../internal/api/api.go) and [tests](../../internal/api/api_test.go) |
| [Approved no-op finalization](2026-09-26-approved-noop-finalization.md) | Current-round approved no-op finalizes without a merge task, independently of agent_merge | [Store](../../internal/store/store.go); [round-link regression tests](../../internal/store/link_round_test.go) |
| [Sandbox and task model defaults](2026-09-30-sandbox-and-task-model-defaults.md) | Repository defaults updated; live rollout/provider compatibility not checked | [Sandbox script](../../harness/sbx.sh); [task helper](../../skills/odonian-breakdown/scripts/odonian.sh) |

The older [MVP design](../../DESIGN.md), [bootstrap board](../../board/README.md),
[bootstrap execution runbook](../../AGENT.md),
[Foreman orchestration design](../design-orchestration-restructure.md) and
[rename cutover checklist](../runbooks/odonian-cutover.md) are also historical records.
Feature designs in [docs/features](../features/) carry their own status banners;
use their linked current guides before following an old operational instruction.
