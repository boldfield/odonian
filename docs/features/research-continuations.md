# Feature: research continuations

## Overview

Research continuation manifests enable controlled creation of child tasks from a parent research task. A parent task's worker proposes a set of child tasks in a versioned manifest carried in the parent's PR. Both reviewers examine the exact proposal, and only the parent's verified human merge can create the children. This prevents child tasks from being created without explicit human approval of the specific work breakdown.

## Behavior

### 1. Parent task and manifest

A parent research task may carry a continuation manifest in its PR. The manifest is:

- **Versioned**: Each manifest has an integer version, starting at 1. When a parent task is reworked, the manifest version increments if it changes.
- **Fully specified**: Every proposed child has a complete specification. No implicit defaults or inheriting field values from the parent.
- **Scoped to the parent**: Keys are stable within the parent task, not globally unique.
- **Proposed by the worker**: The worker proposes children alongside their implementation of the parent task.
- **Reviewed by both reviewers**: The manifest is examined alongside the parent's implementation. A reviewer can flag issues with the proposed children as findings on the parent task.
- **Gated by human merge**: Children are created only when a human merges the parent's PR, not when the parent's work is approved.

### 2. Child specification

Each proposed child has:

- **key**: A stable identifier within the parent task's manifest. Used for intra-manifest dependencies and references. Must be unique within the manifest.
- **title**: Short human-readable title for the child task.
- **spec**: Full prose specification of the child's work, complete and independent. Not derived from or dependent on the parent's implementation.
- **track**: One of `research`, `build`, or `design`. Same validation as parent task tracks.
- **model**: The model tier for the child's work. Must be an allowlisted model.
- **review_models**: A pair of reviewer models (1 or 2 entries). Each must be an allowlisted model and differ from the working model (if enforced).
- **agent_merge**: Whether the child's worker can merge without human approval.
- **escalate**: Whether the child allows model escalation on repeated rejection.
- **claim_ids**: The exact claims this child will verify. A list of claim identifiers from the parent's work or existing sources. Must have at least one; at most six per child.
- **source_start_points**: Starting references or sources the child will work from. At most four distinct primary sources per child.
- **file_scope**: List of files the child will touch or reference. Used to detect overlaps and scope the child's changes. Cannot overlap with other children's scopes.
- **acceptance_criteria**: What passes the child's review, stated as acceptance conditions. At least one per child.
- **dependencies**: References to other children in the same manifest or external task IDs. Enables children to depend on the parent's work or each other.

### 3. Child disposition

Pending candidate claims from the parent's work may be carried forward, assigned to a child, or excluded. Each pending candidate has:

- **claim_id**: The candidate claim's identifier.
- **disposition**: One of `assigned`, `carried_forward`, or `excluded`.
- **owner** (if `carried_forward`): The person or owner responsible for the carried-forward claim in a later task or phase.
- **reason** (if `excluded`): Why the claim was not included in any child task.

### 4. Validation

The manifest validator checks:

- **Completeness**: Every required field is present and non-empty.
- **Type correctness**: Models and tracks are in the allowlist. Dispositions are valid.
- **Uniqueness**: Child keys are unique within the manifest. Files don't overlap between children.
- **Sizing**: No more than six claims per child, no more than four primary sources per child. At most three children per manifest.
- **Dependencies**: No cycles within the intra-manifest dependency graph. Dependencies reference only other children or external task IDs.
- **Candidate consistency**: Carried-forward claims have an owner; excluded claims have a reason.

Validation is independent of task creation and existing review-finding follow-ups. It applies the rules above without knowledge of prior tasks or project-specific contracts.

### 5. Child creation workflow

When a parent task's PR is merged:

1. The parent task is marked as `approved` and a human reviews and merges the PR.
2. The server reads the manifest from the parent task.
3. For each child, the server creates a new task with the manifest's fields.
4. Research-track children start in the `ready` state and are immediately claimable.
5. Build and design-track children start in the `backlog` state and must be explicitly queued.
6. Each child has an automatic dependency on the parent task (the parent task must be `done` before the child can be claimed).
7. The parent task's final result event lists the created child task IDs.

### 6. Limits and ceilings

- **Children per manifest**: At most 3.
- **Claims per child**: At most 6. This is a sizing guidance: a child verifying more than 6 claims is too large for a single task.
- **Primary sources per child**: At most 4. Sourced from the `source_start_points` field.
- **File scope overlap**: Children cannot have overlapping file scopes. Each file belongs to at most one child.
- **Dependency cycles**: Intra-manifest dependencies cannot form a cycle.

### 7. Independent source-only children

The acceptance criteria emphasize "independent source-only children": children whose specifications are complete and whose work does not require merging code from the parent task or another child. Each child:

- Has a complete specification in its `spec` field.
- Cites source starting points in `source_start_points`.
- Touches only its own `file_scope`.
- Depends only on completed parent tasks or completed siblings (via `dependencies`).
- Does not inherit fields or defaults from the parent.

This enables each child to be worked in parallel (if dependencies allow) or sequenced, independent of the parent's implementation details.

## Data model

The manifest is defined in `internal/manifest/manifest.go` as:

```go
type Manifest struct {
	Version           int                `json:"version"`
	ParentTaskID      string             `json:"parent_task_id"`
	Children          []Child            `json:"children"`
	PendingCandidates []PendingCandidate `json:"pending_candidates"`
}

type Child struct {
	Key                string   `json:"key"`
	Title              string   `json:"title"`
	Spec               string   `json:"spec"`
	Track              string   `json:"track"`
	Model              string   `json:"model"`
	ReviewModels       []string `json:"review_models"`
	AgentMerge         bool     `json:"agent_merge"`
	Escalate           bool     `json:"escalate"`
	ClaimIDs           []string `json:"claim_ids"`
	SourceStartPoints  []string `json:"source_start_points"`
	FileScope          []string `json:"file_scope"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	Dependencies       []string `json:"dependencies"`
}

type PendingCandidate struct {
	ClaimID     string
	Disposition ChildDisposition
	Owner       *string
	Reason      *string
}
```

The validator in `internal/manifest/manifest.go` is used by later store and API work to validate manifests before child creation.

## Constraints

- **Do not auto-retarget held legacy tasks**: Old tasks held in backlog should not be automatically retargeted when children are created.
- **Complete a blocked or decompose parent before creating children**: If a parent is blocked for decomposition, the manifest is not applied.
- **Create children only after merge**: Children are created as part of the merge-completion action, not when the parent's work is approved.
- **Bypass publication gates only with explicit intent**: The merge gate ensures children are created only with human approval of the exact manifest.

## Future work

The manifest contract defined here supports future work:

- **Manifest persistence**: Storing manifests with parent tasks so they can be audited and traced.
- **Child creation implementation**: Extending the store's `CreateTasks` to populate child specifications from manifests.
- **Manifest versioning and updates**: Allowing parents to update manifests on rework (incrementing version) and tracking changes.
- **Claim traceability**: Linking created children back to the claims in the manifest and the parent's sources.

## Acceptance criteria

1. Valid independent source-only children pass validation.
2. Invalid manifests return specific, actionable error codes.
3. Validator tests cover all validation rules without creating board tasks.
4. The manifest package is independent of task creation and existing review-finding follow-ups.
