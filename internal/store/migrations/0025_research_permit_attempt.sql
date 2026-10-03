-- Add tables for research permit and attempt lifecycle tracking.
-- research_permit: main permit binding task/project/agent/model/pool and caller request.
-- research_attempt: individual attempt tracking with optional usage.
-- Stable retries recover the same permit; new attempts debit new starts.
-- Renewal and finalize are idempotent and fenced by attempt identity.

CREATE TABLE IF NOT EXISTS research_permit (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  model TEXT NOT NULL,
  account_pool TEXT NOT NULL,
  request_id TEXT NOT NULL,
  attempt_id TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('active', 'expired', 'finalized')),
  attempt_started_at TEXT NOT NULL,
  attempt_finalized_at TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (task_id) REFERENCES task(id),
  FOREIGN KEY (project_id) REFERENCES project(id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_research_permit_request_id ON research_permit(request_id);
CREATE INDEX IF NOT EXISTS idx_research_permit_task_id ON research_permit(task_id);
CREATE INDEX IF NOT EXISTS idx_research_permit_project_id ON research_permit(project_id);
CREATE INDEX IF NOT EXISTS idx_research_permit_account_pool ON research_permit(account_pool);
CREATE INDEX IF NOT EXISTS idx_research_permit_state ON research_permit(state);

CREATE TABLE IF NOT EXISTS research_attempt (
  id TEXT PRIMARY KEY,
  permit_id TEXT NOT NULL,
  sequence_number INTEGER NOT NULL,
  started_at TEXT NOT NULL,
  finalized_at TEXT,
  exit_class TEXT,
  usage_tokens TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  FOREIGN KEY (permit_id) REFERENCES research_permit(id)
);

CREATE INDEX IF NOT EXISTS idx_research_attempt_permit_id ON research_attempt(permit_id);
CREATE INDEX IF NOT EXISTS idx_research_attempt_state ON research_attempt(exit_class);
