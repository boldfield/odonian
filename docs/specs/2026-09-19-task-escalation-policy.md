# Change an existing task's escalation policy

Status: implementation plan for the user's 2026-09-19 policy correction.

## User requirement

Size implementation tasks for Haiku and start them with Haiku. Automatic escalation is allowed by default (`escalate=true`) unless the user explicitly disables it. This is independent of the reviewer assignment and human merge gate.

The repair tasks F1 (`ea7658f0-5134-44fb-9e0a-3bf5f0e076fa`), F2 (`a6966887-7bad-4f14-94d6-63326710ea4e`), and F3 (`bf0df06f-837a-483b-b542-13ac782d96ae`) were incorrectly created with escalation disabled. Their task size and initial model were appropriate policy choices; prohibiting escalation was not. The user's correction supersedes the escalation-disabled prose in those existing task specifications and the original repair document.

## Current limitation

Task creation accepts `escalate`, but there is no supported operation to change it on an existing task. The current `PATCH /tasks/{id}` handler updates dependencies only. The supersede operation accepts a model override and copies the old escalation setting, so it cannot enable the policy. Direct database changes and ad hoc replacement tasks are not the repair path.

## Required behavior

Add an authenticated `PATCH /tasks/{id}/escalation` operation that accepts one required boolean field, `escalate`. Use a dedicated policy endpoint so changing this flag cannot accidentally clear dependencies through the existing dependency-only PATCH handler. Keep the existing dependency endpoint unchanged.

Allow setting the flag on nonterminal tasks, including blocked tasks. Reject changes to done, failed, abandoned, or superseded tasks. Updating the policy must preserve task ID, title/spec, model, reviewers, review round, state, result, assignment/lease, held/archive metadata, upstream/downstream dependencies, and PR/branch links. Only the escalation setting, update timestamp, and an appended audit event may change.

The store operation must update the flag and append the audit event atomically. Record the previous and new values so the operator decision is visible in task history. Repeating the current value should succeed without creating a duplicate policy-change event. A failed write must not leave a partial setting or event change.

Setting the flag must not resume a blocked task, escalate immediately, reset rounds, alter review verdicts, or merge code. The existing transition and review-aggregation mechanisms retain those responsibilities. On a subsequent qualifying rejection, the current escalation ladder should observe the new value and behave normally. Verify that behavior with an existing-style deterministic review-aggregation test, not a live task mutation.

For the API, require a JSON boolean; reject missing/null/string values and unknown fields. Reuse the established authentication and task-ID/prefix resolution. Return the updated task representation. Return the existing style of not-found, conflict, and validation errors for missing tasks, disallowed terminal states, and malformed requests. Document the operation and its separation from model selection and task resumption.

## Odonian task breakdown

Both tasks are Haiku-sized, start with `model=haiku`, have `escalate=true`, use independent Opus and GPT-5.5 reviewers, and retain `agent_merge=false`.

1. **EP1 — Store operation for an audited task escalation-policy update.** Add the narrow store operation and interface support. Reuse store transaction/event patterns; do not alter dependency mutation, review aggregation, supersession semantics, or task creation defaults. Test false-to-true and true-to-false, no-op idempotency, blocked/nonterminal support, terminal-state rejection, and preservation of task state, review history, links, dependencies, and lease metadata. Verify the existing escalation aggregator respects the updated flag when its normal review transition later runs. No dependencies.
2. **EP2 — Expose the escalation-policy update through the authenticated API.** Depends on EP1. Add the dedicated PATCH endpoint, route registration, input validation, documented response/error contract, and focused API tests. Test authentication, task ID and supported prefix lookup, true/false/missing/null/wrong-type/unknown-field input, missing/terminal tasks, and preservation of state/dependencies/history through the API. Reuse EP1; do not add a direct-database path or change the dependency-only PATCH route. Include API reference updates.

Run focused store/API tests and required repository checks after merging current main. Application implementation remains with the Odonian fleet; human merges each reviewed PR.

## Operator follow-up

After EP1 and EP2 merge, publish the Odonian server image and update its deployment through the manifests project. Verify the server supports the endpoint, then enable escalation on F1, F2, and F3 in place and read each task back to confirm the flag and preserved dependencies. This correction does not authorize automatically changing unrelated tasks.

Resume the blocked F1 only after its policy is enabled, using the normal audited transition. Keep the current PR and history unless Odonian's configured escalation mechanism later performs its normal model supersession. Do not confuse enabling the policy with immediately selecting a stronger model. F2 and F3 remain initially assigned to Haiku and dependency-gated.
