-- Durable state for research admission: per-account allowance (token bucket),
-- dispatch permits and their attempts. Occupancy is derived from attempt rows
-- (active and not past expires_at), never from a counter, so it cannot drift
-- and it is independent of the task's own state. No credentials or prompts are
-- stored. task_id is deliberately not a foreign key: attempt accounting must
-- outlive any later task archival.

CREATE TABLE research_pool (
  account_id TEXT PRIMARY KEY,
  start_rate REAL NOT NULL CHECK (start_rate > 0),
  burst_capacity INTEGER NOT NULL CHECK (burst_capacity >= 1),
  concurrent_limit INTEGER NOT NULL CHECK (concurrent_limit >= 1),
  completion_reserved INTEGER NOT NULL CHECK (completion_reserved >= 0 AND completion_reserved <= concurrent_limit),
  tokens REAL NOT NULL CHECK (tokens >= 0),
  settled_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE research_permit (
  id TEXT PRIMARY KEY,
  request_id TEXT NOT NULL UNIQUE,
  task_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  model TEXT NOT NULL,
  account_id TEXT NOT NULL,
  completion INTEGER NOT NULL CHECK (completion IN (0, 1)),
  current_attempt_id TEXT NOT NULL,
  created_at TEXT NOT NULL
);

CREATE INDEX idx_research_permit_task ON research_permit(task_id);

CREATE TABLE research_attempt (
  id TEXT PRIMARY KEY,
  permit_id TEXT NOT NULL,
  previous_attempt_id TEXT,
  sequence_number INTEGER NOT NULL CHECK (sequence_number >= 1),
  task_id TEXT NOT NULL,
  account_id TEXT NOT NULL,
  completion INTEGER NOT NULL CHECK (completion IN (0, 1)),
  state TEXT NOT NULL CHECK (state IN ('active', 'finalized', 'expired')),
  started_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  ended_at TEXT,
  exit_class TEXT,
  duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0),
  usage_tokens INTEGER CHECK (usage_tokens IS NULL OR usage_tokens >= 0),
  UNIQUE (permit_id, sequence_number),
  FOREIGN KEY (permit_id) REFERENCES research_permit(id) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX idx_research_attempt_live_pool ON research_attempt(account_id, expires_at) WHERE state = 'active';
CREATE INDEX idx_research_attempt_live_task ON research_attempt(task_id) WHERE state = 'active';
