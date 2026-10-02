# Feature: research continuations

## Overview

Research continuation manifests enable controlled creation of child tasks from a parent research task. A parent task's worker proposes a set of child tasks in a versioned manifest carried in the parent's PR. Both reviewers examine the exact proposal, and only the parent's verified human merge can create the children. This prevents child tasks from being created without explicit human approval of the specific work breakdown.

## Behavior

### 1. Parent task and manifest

A parent research task may carry a continuation manifest in its PR. The manifest is:

- **Versioned**: Each manifest has an integer version, equal to the schema version (currently 1). The version enables future schema changes.
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
- **review_models**: Exactly 2 distinct reviewer models. Each must be an allowlisted model.
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
- **Dependencies**: No cycles within the intra-manifest dependency graph. Every dependency is a `parent` (the manifest's parent task), `child` (a sibling key in this manifest), or `task` (an existing task ID) reference, and a child may not list the same dependency twice.
- **Candidate consistency**: Carried-forward claims have an owner; excluded claims have a reason.

Validation is independent of task creation and existing review-finding follow-ups. It applies the rules above without knowledge of prior tasks or project-specific contracts.

### 5. Child creation workflow

When a parent task's PR is merged:

1. The parent task is marked as `approved` and a human reviews and merges the PR.
2. The server reads the manifest from the parent task.
3. For each child, the server creates a new task with the manifest's fields.
4. Research-track children start in the `ready` state and are immediately claimable.
5. Build and design-track children start in the `backlog` state and must be explicitly queued.
6. Explicit parent dependencies may be specified in the manifest's dependencies field if needed.
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

type Dependency struct {
	Kind DependencyKind `json:"kind"` // parent, child, or task
	Ref  string         `json:"ref"`  // reference (key or task ID)
}

type Child struct {
	Key                string       `json:"key"`
	Title              string       `json:"title"`
	Spec               string       `json:"spec"`
	Track              string       `json:"track"`
	Model              string       `json:"model"`
	ReviewModels       []string     `json:"review_models"`     // exactly 2 distinct models
	AgentMerge         *bool        `json:"agent_merge"`       // presence-aware
	Escalate           *bool        `json:"escalate"`          // presence-aware
	ClaimIDs           []string     `json:"claim_ids"`
	SourceStartPoints  []string     `json:"source_start_points"`
	FileScope          []string     `json:"file_scope"`
	AcceptanceCriteria []string     `json:"acceptance_criteria"`
	Dependencies       []Dependency `json:"dependencies"`
}

type PendingCandidate struct {
	ClaimID     string      `json:"claim_id"`
	Disposition Disposition `json:"disposition"` // assigned, carried_forward, or excluded
	Owner       *string     `json:"owner,omitempty"`
	Reason      *string     `json:"reason,omitempty"`
}
```

The validator in `internal/manifest/manifest.go` is used by later store and API work to validate manifests before child creation.

### Dependency kinds

Dependencies are typed with one of three kinds:

- **parent**: A reference to the parent task. Any number of children may depend on the parent, but a single child may list it at most once. The `ref` field must match the manifest's `parent_task_id`.
- **child**: A reference to another child in the same manifest, by key. Used for intra-manifest dependencies. The `ref` field must be an existing child key.
- **task**: A reference to an external task by ID. The `ref` field must be a valid UUID-format task ID.

### Manifest versioning

The manifest contract is versioned. `CurrentVersion` is 1. Manifests with any other version are rejected. The version is the schema version, not a per-manifest revision counter.

### Validation error codes

The validator returns specific error codes for different validation failures. Code values include:

- **INVALID_VERSION**: manifest version is not supported
- **DUPLICATE_CLAIM_ASSIGNMENT**: a claim ID appears in multiple children
- **UNASSIGNED_CANDIDATE**: an assigned candidate does not appear in any child
- **CONTRADICTORY_DISPOSITION**: a non-assigned candidate (carried_forward, excluded) appears in a child
- **OVERLAPPING_FILES**: children have overlapping or nested file scopes
- **INVALID_TASK_ID**: an external task dependency is not a valid UUID
- **MISMATCHED_PARENT_DEPENDENCY**: a parent dependency ref does not match the manifest's parent task ID
- **MISSING_OWNER**: a carried_forward candidate lacks an owner
- **MISSING_REASON**: an excluded candidate lacks a reason
- **BLANK_CRITERION**: an acceptance criterion is blank or whitespace-only
- **MULTIPLE_PARENT_DEPENDENCIES**: a single child lists the parent dependency more than once
- **DUPLICATE_DEPENDENCY**: a single child lists the same `child` or `task` dependency more than once
- **INVALID_FILE_SCOPE**: a `file_scope` entry is absolute, escapes the repository via `..`, or is `.` (the whole tree)
- **UNMATCHED_CHILD_CLAIM**: a child's claim has no assigned candidate entry
- **UNKNOWN_DEPENDENCY_KIND**: a dependency has an unknown or invalid kind

## Constraints

- **Do not auto-retarget held legacy tasks**: Old tasks held in backlog should not be automatically retargeted when children are created.
- **Do not create children from a blocked or decomposed parent**: If a parent is blocked for decomposition, the manifest is not applied; decompose the parent task first.
- **Create children only after human merge**: Children are created as part of the merge-completion action, not when the parent's work is approved. Only the human's merge gate can trigger child creation.
- **Do not bypass publication gates**: The merge gate is the only entry point for child creation; children cannot be created through other paths.

## Future work

The manifest contract defined here supports future work:

- **Manifest persistence**: Storing manifests with parent tasks so they can be audited and traced.
- **Child creation implementation**: Extending the store's `CreateTasks` to populate child specifications from manifests.
- **Manifest revisions**: Allowing parents to update manifests on rework and tracking changes, via a separate revision field (the schema `version` stays fixed).
- **Claim traceability**: Linking created children back to the claims in the manifest and the parent's sources.

## Acceptance criteria

1. Valid independent source-only children pass validation.
2. Invalid manifests return specific, actionable error codes.
3. Validator tests cover all validation rules without creating board tasks.
4. The manifest package is independent of task creation and existing review-finding follow-ups.
