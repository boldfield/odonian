# Odonian API Reference

## Overview

The Odonian API is a RESTful coordination substrate for managing a backlog of work claimed and executed by AI agents. All endpoints (except `/healthz`) require a bearer token authentication.

## Authentication

All endpoints except `GET /healthz` require the `Authorization: Bearer <token>` header.

**Server Configuration:**
- `ODONIAN_TOKEN` (required): The bearer token to authenticate requests
- `ODONIAN_DB` (required): SQLite database path (e.g., `/data/odonian.db`)
- `ODONIAN_ADDR` (optional, default `:8080`): Server address and port

**Example:**
```bash
curl -H "Authorization: Bearer your-secret-token" https://api.example.com/projects
```

**Error Responses:**
- `401 MISSING_AUTH`: Authorization header is missing
- `401 INVALID_AUTH_FORMAT`: Authorization header is not in the format `Bearer <token>`
- `401 INVALID_TOKEN`: The provided token does not match the server token

## Endpoints

### Health Check

#### `GET /healthz`

Check server health (no authentication required).

**Request:**
```bash
curl http://localhost:8080/healthz
```

**Response (200 OK):**
```json
{
  "status": "ok"
}
```

---

### Projects

#### `POST /projects`

Create a new project.

**Request:**
```json
{
  "name": "My Project",
  "repo": "https://github.com/user/my-project"
}
```

**Response (201 Created):**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "My Project",
  "repo": "https://github.com/user/my-project",
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "archived_at": null
}
```

**Status Codes:**
- `201 Created`: Project successfully created
- `400 EMPTY_NAME`: Project name cannot be empty
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `500 CREATE_ERROR`: Server error creating project

**Note:** The `id` field is a UUID that must be used in subsequent requests to reference this project.

---

#### `GET /projects/{id}`

Retrieve a project by ID.

**Request:**
```bash
curl -H "Authorization: Bearer token" \
  https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000
```

**Response (200 OK):**
```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "My Project",
  "repo": "https://github.com/user/my-project",
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "archived_at": null
}
```

**Status Codes:**
- `200 OK`: Project found
- `404 NOT_FOUND`: Project not found
- `500 GET_ERROR`: Server error retrieving project

**Note:** The `archived_at` field is `null` for active projects or contains the timestamp when the project was archived.

---

### Documents

#### `POST /projects/{id}/documents`

Register a design or feature specification document.

**Request:**
```json
{
  "kind": "design",
  "title": "DESIGN.md",
  "ref": "DESIGN.md",
  "commit": "abc123def456"
}
```

**Parameters:**
- `kind` (required): Either `"design"` or `"feature_spec"`
- `title` (required): Human-readable title of the document
- `ref` (required): Repository-relative path or URL to the document
- `commit` (optional): Specific commit hash if the document is pinned to a particular version

**Response (201 Created):**
```json
{
  "id": "660e8400-e29b-41d4-a716-446655440001",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "kind": "design",
  "title": "DESIGN.md",
  "ref": "DESIGN.md",
  "commit": null,
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "updated_at": "2026-06-05T21:00:00.000000000Z"
}
```

**Status Codes:**
- `201 Created`: Document successfully registered
- `400 INVALID_KIND`: `kind` must be `"design"` or `"feature_spec"`
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `404 NOT_FOUND`: Project not found
- `409 CONFLICT`: A design document already exists for this project (only one design per project allowed)
- `500 CREATE_ERROR`: Server error creating document

**Note:** Each project may have at most one `design` document, but multiple `feature_spec` documents.

---

#### `GET /projects/{id}/documents`

List all documents for a project.

**Request:**
```bash
# List all documents
curl -H "Authorization: Bearer token" \
  https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/documents

# Filter by kind
curl -H "Authorization: Bearer token" \
  "https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/documents?kind=design"
```

**Query Parameters:**
- `kind` (optional): Filter by `"design"` or `"feature_spec"`

**Response (200 OK):**
```json
[
  {
    "id": "660e8400-e29b-41d4-a716-446655440001",
    "project_id": "550e8400-e29b-41d4-a716-446655440000",
    "kind": "design",
    "title": "DESIGN.md",
    "ref": "DESIGN.md",
    "commit": null,
    "created_at": "2026-06-05T21:00:00.000000000Z",
    "updated_at": "2026-06-05T21:00:00.000000000Z"
  }
]
```

**Status Codes:**
- `200 OK`: Documents retrieved
- `500 LIST_ERROR`: Server error listing documents

---

### Tasks

#### `POST /projects/{id}/tasks`

Bulk-create tasks for a project.

**Request:**
```json
[
  {
    "key": "task1",
    "title": "Implement authentication",
    "spec": "Add bearer token authentication to all endpoints",
    "document_id": "660e8400-e29b-41d4-a716-446655440001",
    "model": "haiku",
    "review_models": ["opus", "sonnet"]
  },
  {
    "key": "task2",
    "title": "Add task claiming",
    "spec": "Implement atomic task claiming with leases",
    "document_id": "660e8400-e29b-41d4-a716-446655440001",
    "model": "haiku",
    "depends_on": ["task1"]
  }
]
```

**Parameters (per task):**
- `key` (optional): Client-provided unique key for referencing this task within the batch (for `depends_on`)
- `title` (required): Task title
- `spec` (required): Task specification/description
- `document_id` (required): ID of the design or feature document this task is decomposed from
- `model` (optional): Assigned model (e.g., `haiku`, `sonnet`, `opus`); must be in the deployment allowlist if provided. If omitted or empty, defaults to the deployment default model.
- `review_models` (optional): List of reviewer models for this task (e.g., `["opus", "sonnet"]`); each must be in the allowlist. Default is `["opus"]` if unset/empty. Ignored for review tasks (auto-spawned only).
- `depends_on` (optional): Array of task IDs or keys (if using intra-batch references) that must be done before this task is claimable

**Response (201 Created):**
```json
[
  {
    "id": "770e8400-e29b-41d4-a716-446655440002",
    "project_id": "550e8400-e29b-41d4-a716-446655440000",
    "document_id": "660e8400-e29b-41d4-a716-446655440001",
    "title": "Implement authentication",
    "spec": "Add bearer token authentication to all endpoints",
    "state": "backlog",
    "kind": "implement",
    "model": "haiku",
    "review_models": ["opus", "sonnet"],
    "review_round": 0,
    "assignee": null,
    "lease_expires_at": null,
    "result": null,
    "created_at": "2026-06-05T21:00:00.000000000Z",
    "updated_at": "2026-06-05T21:00:00.000000000Z"
  },
  {
    "id": "880e8400-e29b-41d4-a716-446655440003",
    "project_id": "550e8400-e29b-41d4-a716-446655440000",
    "document_id": "660e8400-e29b-41d4-a716-446655440001",
    "title": "Add task claiming",
    "spec": "Implement atomic task claiming with leases",
    "state": "backlog",
    "kind": "implement",
    "model": "haiku",
    "review_models": ["opus"],
    "review_round": 0,
    "assignee": null,
    "lease_expires_at": null,
    "result": null,
    "created_at": "2026-06-05T21:00:00.000000000Z",
    "updated_at": "2026-06-05T21:00:00.000000000Z"
  }
]
```

**Status Codes:**
- `201 Created`: Tasks successfully created
- `400 INVALID_DOCUMENT_ID`: One or more document IDs do not exist
- `400 UNKNOWN_MODEL`: The `model` or a `review_models` entry is not in the deployment allowlist
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `400 <other validation errors>`: Client input validation errors
- `500 CREATE_ERROR`: Server error creating tasks

**Note:** All tasks begin in the `backlog` state and must be promoted to `ready` before they can be claimed. All tasks default to `kind: implement` if not auto-spawned as review tasks.

---

#### `GET /projects/{id}/tasks`

List tasks for a project with optional filters.

**Request:**
```bash
# List all tasks
curl -H "Authorization: Bearer token" \
  https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/tasks

# Filter by state
curl -H "Authorization: Bearer token" \
  "https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/tasks?state=in_progress"

# Filter by model
curl -H "Authorization: Bearer token" \
  "https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/tasks?model=haiku"

# Filter by kind
curl -H "Authorization: Bearer token" \
  "https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/tasks?kind=implement"

# Filter by assignee
curl -H "Authorization: Bearer token" \
  "https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/tasks?assignee=agent-1"

# Filter by claimable and model (worker polls for its own work)
curl -H "Authorization: Bearer token" \
  "https://api.example.com/projects/550e8400-e29b-41d4-a716-446655440000/tasks?claimable=true&model=haiku"
```

**Query Parameters:**
- `state` (optional): Filter by task state (`backlog`, `ready`, `in_progress`, `review`, `approved`, `done`, `blocked`, `failed`)
- `model` (optional): Filter by assigned model (e.g., `haiku`, `sonnet`, `opus`)
- `kind` (optional): Filter by task kind (`implement` or `review`)
- `assignee` (optional): Filter by agent ID
- `claimable` (optional): If `true`, only return tasks that can be claimed (in `ready` state with no live lease and all dependencies done)

**Response (200 OK):**
```json
[
  {
    "id": "770e8400-e29b-41d4-a716-446655440002",
    "project_id": "550e8400-e29b-41d4-a716-446655440000",
    "document_id": "660e8400-e29b-41d4-a716-446655440001",
    "title": "Implement authentication",
    "spec": "Add bearer token authentication to all endpoints",
    "state": "ready",
    "kind": "implement",
    "model": "haiku",
    "review_models": ["opus"],
    "review_round": 0,
    "assignee": null,
    "lease_expires_at": null,
    "result": null,
    "created_at": "2026-06-05T21:00:00.000000000Z",
    "updated_at": "2026-06-05T21:00:00.000000000Z"
  }
]
```

**Status Codes:**
- `200 OK`: Tasks retrieved
- `500 LIST_ERROR`: Server error listing tasks

**Note:** Response contains an empty array if no tasks match the filters. Tasks can only be claimed if they are in the `ready` state, have no active lease, and all their dependencies are `done`. All tasks have a `kind` (implement or review) and a `model`; review tasks additionally have a `target_task_id` pointing to their parent implement task.

---

#### `GET /tasks/{id}`

Retrieve a task with its dependencies and links.

**Request:**
```bash
curl -H "Authorization: Bearer token" \
  https://api.example.com/tasks/770e8400-e29b-41d4-a716-446655440002
```

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Implement authentication",
  "spec": "Add bearer token authentication to all endpoints",
  "state": "done",
  "kind": "implement",
  "model": "haiku",
  "review_models": ["opus"],
  "review_round": 1,
  "target_task_id": null,
  "assignee": "agent-1",
  "lease_expires_at": null,
  "result": "Completed successfully",
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "updated_at": "2026-06-05T21:05:00.000000000Z",
  "depends_on": [
    "770e8400-e29b-41d4-a716-446655440000"
  ],
  "links": [
    {
      "id": "990e8400-e29b-41d4-a716-446655440004",
      "task_id": "770e8400-e29b-41d4-a716-446655440002",
      "kind": "pr",
      "value": "#123"
    },
    {
      "id": "aa0e8400-e29b-41d4-a716-446655440005",
      "task_id": "770e8400-e29b-41d4-a716-446655440002",
      "kind": "commit",
      "value": "abc123def456"
    }
  ]
}
```

**Status Codes:**
- `200 OK`: Task retrieved
- `404 NOT_FOUND`: Task not found
- `409 AMBIGUOUS_ID`: Task ID prefix matches multiple tasks
- `500 GET_ERROR`: Server error retrieving task

**Task ID Resolution:**

The `{id}` parameter supports prefix resolution for convenience. When the provided ID is between 8 and 35 characters (shorter than a full UUID), it is treated as a prefix:
- If exactly one task ID matches the prefix, that task is returned
- If no tasks match the prefix, returns `404 NOT_FOUND`
- If multiple tasks match the prefix, returns `409 AMBIGUOUS_ID` with a list of candidate task IDs

For full UUID identifiers (36 characters), the standard exact-match lookup is used.

**Note:** This endpoint returns a rich response with field names in lowercase (unlike most other endpoints which use uppercase). It includes the full dependency list and all linked resources.

---

#### `POST /tasks/{id}/promote`

Promote a task from `backlog` to `ready` state.

**Request:**
```bash
curl -X POST -H "Authorization: Bearer token" \
  https://api.example.com/tasks/770e8400-e29b-41d4-a716-446655440002/promote
```

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Implement authentication",
  "spec": "Add bearer token authentication to all endpoints",
  "state": "ready",
  "kind": "implement",
  "model": "haiku",
  "review_models": ["opus"],
  "review_round": 0,
  "assignee": null,
  "lease_expires_at": null,
  "result": null,
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "updated_at": "2026-06-05T21:00:30.000000000Z"
}
```

**Status Codes:**
- `200 OK`: Task promoted successfully
- `404 NOT_FOUND`: Task not found
- `409 CONFLICT`: Task is not in backlog state
- `500 PROMOTE_ERROR`: Server error promoting task

**Note:** Promotion is the human's gate — only promoted tasks can be claimed by agents.

---

#### `POST /tasks/{id}/claim`

Claim a task and move it to `in_progress` state with a lease.

**Request:**
```json
{
  "agent_id": "agent-1",
  "model": "haiku"
}
```

**Parameters:**
- `agent_id` (required): The ID of the agent claiming the task (non-empty string)
- `model` (required): The model assigned to the claiming agent (e.g., `haiku`, `sonnet`, `opus`); must match the task's model

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Implement authentication",
  "spec": "Add bearer token authentication to all endpoints",
  "state": "in_progress",
  "kind": "implement",
  "model": "haiku",
  "review_models": ["opus"],
  "review_round": 0,
  "assignee": "agent-1",
  "lease_expires_at": "2026-06-05T21:05:00.000000000Z",
  "result": null,
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "updated_at": "2026-06-05T21:01:00.000000000Z"
}
```

**Status Codes:**
- `200 OK`: Task claimed successfully
- `400 EMPTY_AGENT_ID`: agent_id cannot be empty
- `400 EMPTY_MODEL`: model cannot be empty
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `404 NOT_FOUND`: Task not found
- `409 CONFLICT`: Task is not claimable (not in ready state, has an active lease, or has undone dependencies)
- `409 MODEL_MISMATCH`: The task's model does not match the declared model
- `500 CLAIM_ERROR`: Server error claiming task

**Claimability Rules:**
A task is claimable only if:
1. It is in the `ready` state
2. No active lease exists (or the lease has expired)
3. All tasks in its `depends_on` set are in the `done` state
4. The task's `model` matches the agent's declared model (server enforced)

The claim is atomic — implemented as a conditional UPDATE statement. If the claim succeeds, `rowsAffected == 1` and the task is guaranteed to be in `in_progress` with a fresh lease. If `rowsAffected == 0`, the client lost the race (another agent claimed it first) or the model didn't match, and should retry with a different task.

---

#### `POST /tasks/{id}/heartbeat`

Extend the lease on a task claimed by an agent.

**Request:**
```json
{
  "agent_id": "agent-1"
}
```

**Parameters:**
- `agent_id` (required): The ID of the agent (must match the task's assignee)

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Implement authentication",
  "spec": "Add bearer token authentication to all endpoints",
  "state": "in_progress",
  "kind": "implement",
  "model": "haiku",
  "review_models": ["opus"],
  "review_round": 0,
  "assignee": "agent-1",
  "lease_expires_at": "2026-06-05T21:10:00.000000000Z",
  "result": null,
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "updated_at": "2026-06-05T21:02:00.000000000Z"
}
```

**Status Codes:**
- `200 OK`: Heartbeat successful, lease extended
- `400 EMPTY_AGENT_ID`: agent_id cannot be empty
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `404 NOT_FOUND`: Task not found
- `409 CONFLICT`: Task is not in_progress or is not assigned to the provided agent_id
- `500 HEARTBEAT_ERROR`: Server error extending lease

**Note:** Agents should call this regularly (at least before the lease expires) to prevent the task from becoming claimable by another agent. The lease duration is configured on the server side.

---

#### `POST /tasks/{id}/submit`

Submit a task for review (implement tasks) or submit a verdict (review tasks). Behavior depends on task kind.

**For `kind: implement` tasks** — Submit for review with work links:

**Request:**
```json
{
  "agent_id": "agent-1",
  "result": "Task completed successfully. Implemented bearer token auth on all endpoints.",
  "links": [
    {
      "kind": "pr",
      "value": "#123"
    },
    {
      "kind": "commit",
      "value": "abc123def456"
    }
  ]
}
```

**Parameters:**
- `agent_id` (required): The ID of the agent submitting (must match the task's assignee)
- `result` (required): Summary of work completed
- `links` (optional): Array of external resource references
- `verdict` (forbidden): Must not be present for implement tasks

**Link Types:**
- `pr`: Pull request reference (e.g., `#123` or `owner/repo#123`)
- `commit`: Commit hash (e.g., `abc123def456`)
- `branch`: Branch name (e.g., `feature/auth`)
- `ci`: CI status/build URL (e.g., `https://ci.example.com/builds/123`)
- `no_op`: Review-verified no-op marker (e.g., `already-satisfied`). Used when the acceptance
  criteria are already satisfied on `main` with no diff: the worker submits with this marker and
  NO `pr` link, and the reviewer verifies the claim against `main` before approving.

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Implement authentication",
  "spec": "Add bearer token authentication to all endpoints",
  "state": "review",
  "kind": "implement",
  "model": "haiku",
  "review_models": ["opus"],
  "review_round": 1,
  "assignee": "agent-1",
  "lease_expires_at": null,
  "result": "Task completed successfully. Implemented bearer token auth on all endpoints.",
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "updated_at": "2026-06-05T21:03:00.000000000Z",
  "depends_on": [],
  "links": [
    {
      "id": "990e8400-e29b-41d4-a716-446655440004",
      "task_id": "770e8400-e29b-41d4-a716-446655440002",
      "kind": "pr",
      "value": "#123"
    },
    {
      "id": "aa0e8400-e29b-41d4-a716-446655440005",
      "task_id": "770e8400-e29b-41d4-a716-446655440002",
      "kind": "commit",
      "value": "abc123def456"
    }
  ]
}
```

---

**For `kind: review` tasks** — Submit a verdict:

**Request:**
```json
{
  "agent_id": "opus-reviewer-1",
  "verdict": "approve",
  "result": "Code review passed. Well-structured and thoroughly tested. One minor comment on error handling."
}
```

**Parameters:**
- `agent_id` (required): The ID of the reviewing agent (must match the task's assignee)
- `verdict` (required): Either `"approve"` or `"reject"`
- `result` (optional): Review writeup or detailed feedback

**Response (200 OK):**
```json
{
  "id": "aa0e8400-e29b-41d4-a716-446655440006",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Review: Implement authentication [opus]",
  "spec": "...",
  "state": "done",
  "kind": "review",
  "model": "opus",
  "review_models": null,
  "review_round": 1,
  "target_task_id": "770e8400-e29b-41d4-a716-446655440002",
  "assignee": "opus-reviewer-1",
  "lease_expires_at": null,
  "result": "Code review passed. Well-structured and thoroughly tested. One minor comment on error handling.",
  "created_at": "2026-06-05T21:03:00.000000000Z",
  "updated_at": "2026-06-05T21:04:30.000000000Z"
}
```

The response includes the review task's own `id` and the parent implement task's `id` in `target_task_id`. If this verdict completes the current review round (all reviewers have verdicted), the parent task will also be automatically transitioned:
- All approve → parent moves to `approved`
- Any reject → parent moves to `ready` for rework

---

**Status Codes (both kinds):**
- `200 OK`: Submit successful
- `400 EMPTY_AGENT_ID`: agent_id cannot be empty
- `400 INVALID_LINK_KIND`: One or more link kinds are invalid (must be pr, branch, commit, ci, or no_op)
- `400 INVALID_VERDICT`: verdict must be "approve" or "reject" (review only)
- `400 FORBIDDEN_VERDICT`: verdict must not be present for implement tasks
- `400 MISSING_VERDICT`: verdict is required for review tasks
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `404 NOT_FOUND`: Task not found
- `409 CONFLICT`: Task is not in_progress or is not assigned to the provided agent_id
- `500 SUBMIT_ERROR`: Server error submitting task

**Note:** Links are indexed on `(kind, value)` to enable reverse lookup. Review verdicts are recorded as events on the parent implement task for audit purposes.

---

#### `POST /tasks/{id}/review`

Post a review verdict on a task in the `review` state.

**Request:**
```json
{
  "actor": "reviewer@example.com",
  "verdict": "approve",
  "note": "Looks good! Code quality is solid."
}
```

**Parameters:**
- `actor` (required): Human-readable identifier of the reviewer (non-empty string)
- `verdict` (required): Either `"approve"` or `"reject"`
- `note` (optional): Free-form note or feedback

**Response (201 Created):**
```json
{
  "id": "bb0e8400-e29b-41d4-a716-446655440006",
  "task_id": "770e8400-e29b-41d4-a716-446655440002",
  "actor": "reviewer@example.com",
  "kind": "review",
  "verdict": "approve",
  "note": "Looks good! Code quality is solid.",
  "created_at": "2026-06-05T21:04:00.000000000Z"
}
```

**Status Codes:**
- `201 Created`: Review event recorded
- `400 EMPTY_ACTOR`: actor cannot be empty
- `400 INVALID_VERDICT`: verdict must be "approve" or "reject"
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `404 NOT_FOUND`: Task not found
- `409 CONFLICT`: Task is not in review state
- `500 REVIEW_ERROR`: Server error recording review

**Note:** Review events are immutable and append-only. They do not directly transition the task state — that is done via a separate `POST /tasks/{id}/transition` call. If verdict is `reject`, the task typically transitions back to `ready` for rework.

---

#### `POST /tasks/{id}/transition`

Transition a task to a new state (manual state machine operation). Typically used by humans
to drain the `approved` lane by merging and marking done, or to override an approval.

**Request:**
```json
{
  "to": "done",
  "note": "Reviewed and merged to main"
}
```

**Parameters:**
- `to` (required): Target state (`done`, `blocked`, `ready`, or `failed`)
- `note` (optional): Reason or context for the transition

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Implement authentication",
  "spec": "Add bearer token authentication to all endpoints",
  "state": "done",
  "kind": "implement",
  "model": "haiku",
  "review_models": ["opus"],
  "review_round": 1,
  "assignee": "agent-1",
  "lease_expires_at": null,
  "result": "Task completed successfully. Implemented bearer token auth on all endpoints.",
  "created_at": "2026-06-05T21:00:00.000000000Z",
  "updated_at": "2026-06-05T21:05:00.000000000Z"
}
```

**Status Codes:**
- `200 OK`: Task transitioned successfully
- `400 INVALID_STATE`: Target state is invalid
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `404 NOT_FOUND`: Task not found
- `409 CONFLICT`: Transition is not allowed from the current state
- `500 TRANSITION_ERROR`: Server error transitioning task

**Valid Transitions:**
The state machine enforces these rules:
- `ready` → `in_progress` (via claim)
- `in_progress` → `review` (via submit for implement tasks)
- `review` → `approved` (when all reviewers approve; automatic, via verdict)
- `review` → `ready` (when any reviewer rejects; automatic, via verdict)
- `approved` → `done` (human merges PR)
- `approved` → `ready` (human disagrees with reviewers, requests rework)
- `blocked` → `ready` (human unblocks / retries; clears stale assignee and lease)
- `blocked` → `failed` (retire a dead blocked task without re-entering the queue)
- Any active state → `blocked` (off-ramp: external blocker)
- Any active state → `failed` (off-ramp: task cannot be done as specified)

Note: `review` → `done` is no longer a direct transition. All paths to `done` now go through
`approved`. The `approved` state is the human merge gate. `blocked` is recoverable via the
`blocked` → `ready` transition, unlike terminal states `done` and `failed`. Once a `blocked`
task is decided to be unrecoverable, use `blocked` → `failed` to retire it cleanly.

---

#### `POST /tasks/{id}/supersede`

Create a replacement task with the same specification and dependencies. The old task is marked
as superseded. This is typically used when a task needs to be retried with an escalated model
or when the original task should not be reused.

**Request:**
```json
{
  "model": "opus"
}
```

**Parameters:**
- `model` (optional): Override the model for the replacement task. If not provided, the replacement uses the original task's model.

**Response (200 OK):**
```json
{
  "id": "cc0e8400-e29b-41d4-a716-446655440007",
  "project_id": "550e8400-e29b-41d4-a716-446655440000",
  "document_id": "660e8400-e29b-41d4-a716-446655440001",
  "title": "Implement authentication",
  "spec": "Add bearer token authentication to all endpoints\n\n## Prior attempt feedback\n\n**opus-reviewer-1 (verdict: reject)**\nNeed better error handling\n\n",
  "state": "backlog",
  "kind": "implement",
  "model": "opus",
  "review_models": ["opus"],
  "review_round": 0,
  "assignee": null,
  "lease_expires_at": null,
  "result": null,
  "track": "design",
  "created_at": "2026-06-05T21:05:00.000000000Z",
  "updated_at": "2026-06-05T21:05:00.000000000Z"
}
```

**Status Codes:**
- `200 OK`: Task superseded successfully
- `400 UNKNOWN_MODEL`: The provided model is not in the deployment allowlist
- `400 JSON_DECODE_ERROR`: Invalid JSON in request body
- `404 NOT_FOUND`: Task not found
- `409 CONFLICT`: Task is in a terminal state (done, failed, abandoned, or superseded) and cannot be superseded
- `500 SUPERSEDE_ERROR`: Server error superseding task

**Behavior:**
- The replacement task inherits the title, spec, dependencies, and other task configuration from the original
- Prior rejection feedback is prepended to the replacement's spec
- The `track` field is preserved from the original task to the replacement
- All downstream dependencies (tasks that depend on the original) are re-pointed to the replacement
- The original task is marked with `state: superseded` and `superseded_by` set to the replacement task ID
- The replacement task starts in `backlog` state and must be promoted to `ready` before claiming

---

### Hold and Release

Hold/Release provides an orthogonal task lock independent of state transitions. A held task cannot be claimed. This is useful for pausing work without transitioning state (e.g., pausing a review task while waiting for external input) or for temporarily preventing auto-transitions during maintenance.

#### `POST /tasks/{id}/hold`

Hold a task, preventing it from being claimed and blocking automatic state transitions.

**Request:**
```bash
curl -X POST -H "Authorization: Bearer token" \
  https://api.example.com/tasks/770e8400-e29b-41d4-a716-446655440002/hold
```

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "state": "ready",
  "held": true,
  "...": "other task fields"
}
```

**Status Codes:**
- `200 OK`: Task held successfully
- `404 NOT_FOUND`: Task not found
- `500 HOLD_ERROR`: Server error holding task

**Behavior:**
- Hold works from any state (orthogonal lock)
- A held task cannot be claimed; `POST /tasks/{id}/claim` returns `409 CONFLICT`
- Automatic state transitions (e.g., from review aggregation) skip held tasks
- The `held` flag persists across state transitions until explicitly released

---

#### `POST /tasks/{id}/release`

Release a held task, restoring normal automated flow.

**Request:**
```bash
curl -X POST -H "Authorization: Bearer token" \
  https://api.example.com/tasks/770e8400-e29b-41d4-a716-446655440002/release
```

**Response (200 OK):**
```json
{
  "id": "770e8400-e29b-41d4-a716-446655440002",
  "state": "approved",
  "held": false,
  "...": "other task fields"
}
```

**Status Codes:**
- `200 OK`: Task released successfully
- `404 NOT_FOUND`: Task not found
- `500 RELEASE_ERROR`: Server error releasing task

**Behavior:**
- Release clears the `held` flag, allowing the task to be claimed again
- If a task is released while in `review` state and all review tasks targeting it are done, the review round is aggregated automatically in the same transaction. This allows a held parent task to complete review aggregation when released (e.g., if all reviewers submitted verdicts while the parent was held).
  - If all reviewers approved, the parent moves to `approved`
  - If any reviewer rejected (and under escalation threshold), the parent moves to `ready` with `review_round` incremented
  - If escalation threshold exceeded, the task may be escalated or blocked instead
- For non-review tasks or review tasks with pending review tasks, release is a simple unlock with no state change

---

## Full Lifecycle Walkthrough

Below is a copy-paste example of the complete task lifecycle using the modern model-assigned,
review-as-a-task flow. A Haiku worker implements a feature, Opus reviewers approve it
in parallel, and a human drains the `approved` lane to merge.

```bash
#!/bin/bash

# Configuration
BASE="http://localhost:8080"
TOKEN="your-secret-token"
AUTH="Authorization: Bearer $TOKEN"

echo "=== 1. Health check (no auth) ==="
curl -s "$BASE/healthz" | jq .

echo "=== 2. Create project ==="
PROJECT=$(curl -s -X POST "$BASE/projects" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{"name":"Example Project","repo":"https://github.com/user/repo"}')
echo "$PROJECT" | jq .
PROJECT_ID=$(echo "$PROJECT" | jq -r '.ID')

echo "=== 3. Register design document ==="
DOC=$(curl -s -X POST "$BASE/projects/$PROJECT_ID/documents" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{"kind":"design","title":"DESIGN.md","ref":"DESIGN.md"}')
echo "$DOC" | jq .
DOC_ID=$(echo "$DOC" | jq -r '.ID')

echo "=== 4. Bulk-create tasks with model assignment ==="
TASKS=$(curl -s -X POST "$BASE/projects/$PROJECT_ID/tasks" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d "[
    {
      \"key\":\"task1\",
      \"title\":\"Implement feature\",
      \"spec\":\"Add new functionality\",
      \"document_id\":\"$DOC_ID\",
      \"model\":\"haiku\",
      \"review_models\":[\"opus\"]
    },
    {
      \"key\":\"task2\",
      \"title\":\"Write tests\",
      \"spec\":\"Add test coverage\",
      \"document_id\":\"$DOC_ID\",
      \"model\":\"haiku\",
      \"depends_on\":[\"task1\"]
    }
  ]")
echo "$TASKS" | jq .
TASK_ID=$(echo "$TASKS" | jq -r '.[0].id')

echo "=== 5. List backlog tasks ==="
curl -s "$BASE/projects/$PROJECT_ID/tasks?state=backlog" -H "$AUTH" | jq .

echo "=== 6. Promote task to ready ==="
TASK=$(curl -s -X POST "$BASE/tasks/$TASK_ID/promote" -H "$AUTH")
echo "$TASK" | jq .

echo "=== 7. Claim task as Haiku worker (model-matched) ==="
TASK=$(curl -s -X POST "$BASE/tasks/$TASK_ID/claim" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{"agent_id":"haiku-1","model":"haiku"}')
echo "$TASK" | jq .

echo "=== 8. Send heartbeat to extend lease ==="
TASK=$(curl -s -X POST "$BASE/tasks/$TASK_ID/heartbeat" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{"agent_id":"agent-1"}')
echo "$TASK" | jq .

echo "=== 9. Submit implement task for review with PR links ==="
TASK=$(curl -s -X POST "$BASE/tasks/$TASK_ID/submit" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{
    "agent_id":"haiku-1",
    "result":"Feature implemented and tested",
    "links":[
      {"kind":"pr","value":"#123"},
      {"kind":"commit","value":"abc123def456"}
    ]
  }')
echo "$TASK" | jq .
echo "(Auto-spawned review tasks are now ready for Opus reviewers)"

echo "=== 10. List review tasks claimable by Opus ==="
REVIEW_TASKS=$(curl -s "$BASE/projects/$PROJECT_ID/tasks?claimable=true&model=opus" -H "$AUTH")
echo "$REVIEW_TASKS" | jq .
REVIEW_TASK_ID=$(echo "$REVIEW_TASKS" | jq -r '.[0].id')

echo "=== 11. Opus reviewer claims the review task ==="
REVIEW_TASK=$(curl -s -X POST "$BASE/tasks/$REVIEW_TASK_ID/claim" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{"agent_id":"opus-1","model":"opus"}')
echo "$REVIEW_TASK" | jq .

echo "=== 12. Opus reviewer submits verdict (approve) ==="
VERDICT=$(curl -s -X POST "$BASE/tasks/$REVIEW_TASK_ID/submit" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{
    "agent_id":"opus-1",
    "verdict":"approve",
    "result":"Code looks good. Well tested and documented."
  }')
echo "$VERDICT" | jq .
echo "(The parent task automatically moves to 'approved' since all reviewers approved)"

echo "=== 13. Check that parent task is now approved ==="
PARENT=$(curl -s "$BASE/tasks/$TASK_ID" -H "$AUTH")
echo "$PARENT" | jq '{state, kind}'

echo "=== 14. Human drains approved lane: merge the PR ==="
echo "(Human would run: git pull && git merge --ff-only pr/feature && git push)"

echo "=== 15. Human transitions approved task to done ==="
TASK=$(curl -s -X POST "$BASE/tasks/$TASK_ID/transition" \
  -H "Content-Type: application/json" \
  -H "$AUTH" \
  -d '{"to":"done","note":"Merged to main"}')
echo "$TASK" | jq .

echo "=== 16. Retrieve final task state ==="
curl -s "$BASE/tasks/$TASK_ID" -H "$AUTH" | jq .
```

**Key Points:**
1. Tasks are created with a `model` field; workers claim by declaring their model (e.g., `haiku`, `opus`)
2. Claiming is atomic and model-matched — if the model doesn't match, you get `409 MODEL_MISMATCH`
3. Implement tasks (the default `kind`) transition `in_progress` → `review` → `approved` → `done`
4. Submitting an implement task auto-spawns review tasks for each required reviewer (default: Opus)
5. Review tasks are claimed and completed by reviewers submitting verdicts (approve or reject)
6. When all reviewers of a round approve, the parent moves to `approved`; if any reject, it goes to `ready`
7. The human gates the final merge: tasks in `approved` are merged and transitioned to `done` by humans
8. Workers extend their lease via heartbeat to prevent task expiry
9. Dependencies are enforced at claim time — tasks with undone deps cannot be claimed
10. The second task `task2` cannot be claimed until `task1` is `done` (due to `depends_on`)

---

## Error Response Format

All error responses follow a consistent format:

```json
{
  "error": {
    "code": "ERROR_CODE",
    "message": "Human-readable error message"
  }
}
```

**Common Errors:**
- `MISSING_AUTH` (401): Authorization header missing
- `INVALID_AUTH_FORMAT` (401): Authorization header malformed
- `INVALID_TOKEN` (401): Token does not match server token
- `NOT_FOUND` (404): Resource not found
- `CONFLICT` (409): State transition or constraint violation (generic)
- `MODEL_MISMATCH` (409): Task's model doesn't match declared model on claim
- `UNKNOWN_MODEL` (400): Model is not in the deployment allowlist (create time)
- `JSON_DECODE_ERROR` (400): Invalid JSON in request body
- `EMPTY_<FIELD>` (400): Required field is empty
- `INVALID_<FIELD>` (400): Field value is invalid (e.g., verdict not "approve" or "reject")
- `FORBIDDEN_VERDICT` (400): Verdict provided for an implement task (forbidden)
- `MISSING_VERDICT` (400): Verdict missing for a review task (required)
- `CREATE_ERROR` (500): Server error during creation
- `GET_ERROR` (500): Server error retrieving resource
- `LIST_ERROR` (500): Server error listing resources
- `CLAIM_ERROR` (500): Server error during claim
- `SUBMIT_ERROR` (500): Server error during submit
- `HEARTBEAT_ERROR` (500): Server error during heartbeat
- `PROMOTE_ERROR` (500): Server error promoting task
- `REVIEW_ERROR` (500): Server error recording review (legacy endpoint)
- `TRANSITION_ERROR` (500): Server error transitioning task

---

## State Machine

Implement tasks follow this state machine:

```
backlog ──promote──► ready ──claim──► in_progress ──submit──► review ──┐
                       ▲                    │                        │
                       │                    │      all approve       ▼
                       │              lease expiry       ┌──────► approved ──human──► done
                       │                    │           │            │
                       └────────────────────┴──────────┴ any reject  │
                                                                     │
                                           human disagrees ◄────────┘

blocked / failed are off-ramps from any active state.
```

**For implement tasks:**
- `backlog`: Initial state; tasks are not yet ready for work
- `ready`: Promoted and ready to claim; dependencies met
- `in_progress`: Claimed by an agent; work is in progress; lease-based crash recovery
- `review`: Work submitted; reviewers are working; auto-spawned review tasks are claimable
- `approved`: All reviewers approved; awaiting human merge
- `done`: Approved and merged; task is complete
- `blocked`: Off-ramp; task cannot proceed (blocked on external dependency)
- `failed`: Off-ramp; task attempt failed; consider rework or cancellation

**For review tasks** (auto-spawned when parent enters `review`):
```
ready ──claim──► in_progress ──submit verdict──► done
           │                        │
           └────── lease expiry ────┘

blocked / failed are off-ramps.
```

- `ready`: Just spawned; claimable by the assigned reviewer model
- `in_progress`: Reviewer is conducting review; lease prevents concurrent reviews
- `done`: Verdict submitted; moving to `done` doesn't transition the parent (aggregation does)

---

## Concurrency & Leases

**Atomic Claiming:**
The claim operation is a single conditional UPDATE that fails atomically if any precondition is violated (wrong state, lease active, unmet dependencies). This eliminates races and avoids need for distributed locks.

**Lease-Based Crash Recovery:**
When a task is claimed, a lease expiration time is set (`lease_expires_at`). If an agent crashes or hangs, the lease eventually expires and the task becomes claimable again (checked lazily in the next claim attempt). Agents must heartbeat regularly to extend the lease.

**No Sweeper:**
The MVP does not run a background sweeper. Lease expiry is checked lazily inside the atomic claim query. For target concurrency of 2–5 agents, this is sufficient and keeps the system simple.

