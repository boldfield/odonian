# Auto-finalize review-approved no-op tasks

A worker submitted foia.info task `84af4e1c-3d48-4390-b066-0ac1f18aa77a` with a `no_op` link and no PR link because its acceptance criteria were already on `main`. Both independent reviewers approved it on September 26, 2026. The task stayed `approved`, so its dependent work could not be claimed. An operator verified the result on `main` and manually transitioned it to `done`.

`AGENT-API.md` says an approved no-op goes straight to `done` because there is no PR to merge. The store's review aggregation at `internal/store/store.go` currently checks the `no_op` link only inside `if parentAgentMerge`, so tasks created with the normal human-merge setting (`agent_merge=false`) remain stuck at a merge gate with nothing to merge. This is a control-flow bug, not a reason to set `agent_merge=true` for work that may later produce a PR.

## Required behavior

After every required reviewer approves an implementation task with an active `no_op` link and no active `pr` link, move the parent directly from `review` to `done` in the same aggregation transaction, regardless of `agent_merge`. Append an accurate transition event. Create no merge task. A dependent ready task must become claimable immediately. One approval out of multiple required approvals must leave the parent in review. Any rejection follows the existing rework/escalation rules.

Keep the human merge gate for an ordinary `agent_merge=false` implementation with a PR. Keep the `agent_merge=true` PR merge-task path unchanged. Do not auto-complete a task that has both a no-op marker and a PR link, or one whose no-op marker has been tombstoned. Preserve held, blocked, failed, abandoned and superseded handling. Do not rewrite the historical foia.info task; its manual transition is already recorded.

## Verification

Add store-level tests for no-op with both `agent_merge` values, two reviewers, dependent task claimability, no merge task, and PR-linked controls. Add an API-level regression that submits a no-op, records two approvals, observes `done`, and confirms the dependent ready task becomes claimable. Update `AGENT-API.md` only if needed to make the no-op rule and manual recovery path unambiguous. Use synthetic projects/tasks, not the production foia.info case.

Implementation begins on Haiku with escalation enabled, independent Opus and GPT-5.5 review, and `agent_merge=false`; a human merges each implementation PR. The store fix and API regression are separate, dependency-ordered tasks so their shared test paths do not race.
