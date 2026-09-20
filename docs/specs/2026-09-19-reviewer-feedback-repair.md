# Reliable Odonian reviewer feedback

Status: approved by the user on 2026-09-19. This specification supersedes the detection and acknowledgment rules in `2026-08-18-pr-feedback-single-identity.md` and the conflicting best-effort submission-gate behavior documented with that feature.

Target project: Odonian (`83c4152e-57dc-49b0-9244-d90ff1f9cf25`). Implementation stays in Odonian. Deployment changes belong to the manifests project.

## Correct the contract first

The approved `docs/specs/2026-08-18-pr-feedback-single-identity.md` explicitly specifies the faulty behavior: skip every marked agent comment, acknowledge a global comment after any later marked reply, and acknowledge an inline thread after any marked last reply. Its implementation and tests follow those rules. This repair must explicitly supersede those detection rules, not merely ask an implementer to make the existing tests pass.

The observed regression is Run03 Referee task `39a672b8-4062-43f2-88be-84aa24266e81`, PR https://github.com/boldfield/run03-referee/pull/34. In two rework sessions, marked global reviewer requests existed before the worker called `pr-feedback list`; the command returned nothing and normal `submit` accepted unchanged commit `c597d61f3ec0b73ec7a275cc1f962ebfdec5568f`. The four-round review limit then blocked the task. The outstanding rejection is recorded at https://github.com/boldfield/run03-referee/pull/34#issuecomment-5746765819 and again at https://github.com/boldfield/run03-referee/pull/34#issuecomment-5746791848. Native worker tool calls/results and Odonian events were also preserved by the operator; implementers can reproduce the failure using deterministic fixtures without access to those local logs.

## Required behavior

### Feedback and acknowledgment are different records

- Preserve actionable reviewer comments regardless of whether their author shares the worker's GitHub login. `gpt-5.5-reviewer: CHANGES REQUESTED` is feedback. The presence of an agent marker must never by itself mean a comment is addressed.
- Recognize canonical approval summaries as non-actionable status. An approval from one reviewer must not acknowledge another reviewer's requests. Unknown reviewer messages must not be silently discarded as worker chatter.
- Continue returning unmarked human feedback. Worker acknowledgment/status messages and merger/reconciler status messages are not new reviewer requests.
- Return unresolved inline threads. A marked reviewer comment or a worker reply in an unresolved thread does not itself resolve the thread. `pr-feedback ack` already posts a fixing-commit reply and resolves the exact thread; use the resolution state to determine completion.
- A global acknowledgment must be an explicit worker acknowledgment naming the exact original comment ID and fixing commit. The existing writer already emits `addressed in <sha> (see comment <id>)`, so recognize that existing format. An acknowledgment of B cannot clear A; later approvals, unrelated replies, and bare reactions cannot clear A either. The recorded acknowledgment documents the worker's claim of a fix; independent review still establishes whether the fix is sufficient.
- Existing resolved inline threads remain resolved. Existing global acknowledgments containing the exact ID remain valid. Historical items acknowledged only by a reaction or an unrelated reply can reappear; that is an intentional correction to the previous ambiguous policy.

### Rework receives the review record directly

- Odonian already stores reviewer verdicts and findings as task events. Show that record in the ordinary CLI path used by workers, rather than requiring GitHub mirroring to succeed for the worker to learn why the task was rejected.
- Plain `odonian show` should include the review round and latest completed round's verdicts, actors, and findings for a rework task. Earlier rejection notes should remain accessible as clearly labeled history, so an older request is not lost merely because a later reviewer did not repeat it. Do not silently truncate findings.
- Use the existing event API/client where possible; no new database or independent review-state system is required. If the review record cannot be retrieved, report the retrieval failure explicitly instead of presenting an apparently complete rework display.
- The implementation prompt must require reconciling recorded rejection findings, outstanding PR feedback, the task specification, and the current diff. An empty GitHub list alone does not establish completion. Human PR feedback remains additional input.

### Verify the normal submission path

- The standard rework submission gate must reject while actionable PR feedback remains. Tests must exercise the same lookup used by the CLI, including marked global findings and shared identities.
- As an additional hardening change, failure to retrieve task/feedback data during the gate must stop normal submission with a retryable error. An unsuccessful lookup must not be treated as evidence of zero outstanding items. This was not the cause of this incident, but the current gate explicitly allows it and its tests require it.
- Limit this policy to the appropriate implementation rework gate. Initial submissions, reviewer verdict submission, and supported delivery modes need explicit compatibility coverage. Preserve the documented explicit operator override and its warning; workers must not use it automatically. This remains a CLI safeguard, not a claim that raw API calls cannot bypass it.
- Do not introduce an arbitrary requirement for a new commit on every rework. A legitimate reviewer reconsideration can occur without a code change. Outstanding findings and review verdicts are the relevant checks.

## Approved Odonian tasks

All three use Haiku implementation, Opus and GPT-5.5 review, escalation disabled, and human merge (`agent_merge=false`). Publish the approved corrective specification at a pinned repository commit before task creation, and reference it explicitly so the old approved spec cannot override the new contract.

1. **F1 — Preserve reviewer feedback and match acknowledgments to their targets.** Implement the feedback/acknowledgment rules above in `internal/forge/feedback.go`, using `internal/forge/marker.go` only as an authorship/role parser rather than a completion rule. Update focused forge fixtures and the obsolete detection documentation. Cover shared and separate GitHub identities; global rejection followed by another reviewer's approval; two comments with only one acknowledged; marked unresolved inline threads; resolved threads; and existing exact-ID acknowledgment messages. Run appropriate forge tests and required repository checks. No dependencies.

2. **F2 — Deliver recorded review findings in the worker's rework workflow.** Update the plain task display at `cmd/odonian/main.go` and the applicable implementation prompts to expose and consume the review context described above. Reuse `internal/tuiclient.ListEvents`. Preserve the established JSON output contract unless an additive, documented change is necessary. Test initial tasks, mixed reviewer verdicts, multiple rounds, and event-retrieval failure. Include CLI/prompt documentation. Depends on F1 so it consumes the final corrected feedback contract.

3. **F3 — Reproduce the lost-feedback incident through the submission gate and stop on lookup failures.** Extend `cmd/odonian/submit_feedback_gate_test.go` with a deterministic fixture modeled on PR34: one acknowledged/resolved inline request, an outstanding marked global rejection, and another reviewer's approval, all under one login. Assert that listing retains the rejection and standard submit never posts the task submission until the specific request is acknowledged. Verify acknowledging an unrelated item does not unblock it. Change normal rework lookup errors to stop submission and cover the compatible initial/reviewer/delivery-mode cases and explicit operator override. Touch `cmd/odonian/pr-feedback.go` and the submit path in `cmd/odonian/main.go` as necessary. Depends on F2 to serialize CLI file changes.

## Deployment and N1 recovery

After the fixes pass review and merge, publish an immutable fleet image and update its reference through manifests/ArgoCD. Verify the running binary version and deployed prompt, then run a read-only feedback lookup against PR34: its unacknowledged global rejection findings must be visible alongside current unresolved inline requests. Do not test the gate by submitting the live ticket; use the isolated regression fixture.

Only after that verification, return the existing N1 replacement from blocked to ready with a diagnosis note, keeping its PR, acceptance criteria, dependencies, implementation model, reviewers, and review history. The worker must implement the missing regression coverage and pass normal review. The completed rejection history must remain intact; no reset of review history, replacement ticket, forced approval, or direct referee-code patch is part of this repair.
