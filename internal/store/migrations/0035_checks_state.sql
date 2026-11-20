-- Rebuild task table to widen the state CHECK constraint to include 'checks' and add the
-- await_checks and checks_started_at columns (docs/features/checks-gate.md C1).
-- SQLite cannot alter CHECK constraints in place, so we rebuild the table.
-- await_checks opts a task into the checks gate; checks_started_at is set when a task enters
-- 'checks' and cleared when it leaves.

CREATE TABLE task_new (
  id TEXT PRIMARY KEY,
  project_id TEXT NOT NULL,
  document_id TEXT NOT NULL,
  title TEXT NOT NULL,
  spec TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('backlog', 'ready', 'in_progress', 'checks', 'review', 'approved', 'done', 'blocked', 'failed', 'superseded', 'abandoned')),
  assignee TEXT,
  lease_expires_at TEXT,
  result TEXT,
  model TEXT NOT NULL DEFAULT 'haiku',
  kind TEXT NOT NULL DEFAULT 'implement',
  review_models TEXT,
  review_round INTEGER NOT NULL DEFAULT 0,
  target_task_id TEXT REFERENCES task(id),
  verdict TEXT,
  agent_merge INTEGER NOT NULL DEFAULT 0,
  held INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  archived_at TEXT,
  superseded_by TEXT,
  escalate INTEGER NOT NULL DEFAULT 1,
  track TEXT NOT NULL DEFAULT 'build',
  adjudicate_finding_round INTEGER,
  adjudicate_finding_reviewer_model TEXT,
  adjudicate_finding_reviewer_slot INTEGER,
  adjudicate_finding_id TEXT,
  branch TEXT NOT NULL DEFAULT '',
  landing_round INTEGER,
  landing_commit TEXT,
  landing_attempt TEXT,
  priority INTEGER NOT NULL DEFAULT 500 CHECK (priority >= 1),
  replaces_review_task_id TEXT,
  await_checks INTEGER NOT NULL DEFAULT 0,
  checks_started_at TEXT,
  FOREIGN KEY (project_id) REFERENCES project(id),
  FOREIGN KEY (document_id) REFERENCES document(id)
);

INSERT INTO task_new (
  id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind,
  review_models, review_round, target_task_id, verdict, agent_merge, held, created_at, updated_at,
  archived_at, superseded_by, escalate, track, adjudicate_finding_round,
  adjudicate_finding_reviewer_model, adjudicate_finding_reviewer_slot, adjudicate_finding_id,
  branch, landing_round, landing_commit, landing_attempt, priority, replaces_review_task_id
)
SELECT
  id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind,
  review_models, review_round, target_task_id, verdict, agent_merge, held, created_at, updated_at,
  archived_at, superseded_by, escalate, track, adjudicate_finding_round,
  adjudicate_finding_reviewer_model, adjudicate_finding_reviewer_slot, adjudicate_finding_id,
  branch, landing_round, landing_commit, landing_attempt, priority, replaces_review_task_id
FROM task;

DROP TABLE task;

ALTER TABLE task_new RENAME TO task;

CREATE INDEX idx_task_project_state ON task(project_id, state);
CREATE INDEX idx_task_claimable ON task(project_id, state, model);
CREATE INDEX idx_task_archived_at ON task(archived_at);
CREATE INDEX idx_task_held ON task(held);
CREATE UNIQUE INDEX idx_task_adjudicate_unique
  ON task(target_task_id, adjudicate_finding_round, adjudicate_finding_reviewer_model, adjudicate_finding_reviewer_slot, adjudicate_finding_id)
  WHERE adjudicate_finding_id IS NOT NULL;
CREATE INDEX idx_task_superseded_by ON task(superseded_by) WHERE superseded_by IS NOT NULL;
