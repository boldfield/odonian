-- Durable state for model-agnostic reviewer evaluation campaigns and jobs.
-- Campaigns are finite, bounded experiments with cohorts, attempt caps and pools.
-- Jobs are samples evaluated by candidates with immutable claims, leases and results.
-- No credentials are stored. Evaluation jobs cannot vote, reject, create follow-ups,
-- adjudicate disputes or affect production review scorecards.

-- Evaluation campaign: a finite experiment with allowed projects, a cohort
-- manifest and an overall attempt cap. Account pools belong to candidates (each
-- draws from its own evaluation_pool), so a campaign holds none itself.
CREATE TABLE evaluation_campaign (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT,
  cohort_manifest TEXT NOT NULL,
  attempt_cap INTEGER NOT NULL CHECK (attempt_cap >= 1),
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

-- Projects a campaign may sample from.
CREATE TABLE evaluation_campaign_project (
  campaign_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  PRIMARY KEY (campaign_id, project_id),
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id)
);

-- Models a campaign's candidates may use (a candidate's M1 ModelID must be in this set).
CREATE TABLE evaluation_campaign_model (
  campaign_id TEXT NOT NULL,
  model_id TEXT NOT NULL CHECK (length(model_id) > 0),
  PRIMARY KEY (campaign_id, model_id),
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id)
);

CREATE INDEX idx_evaluation_campaign_project_project ON evaluation_campaign_project(project_id);

-- Evaluation sample: original task/review round, submitted SHA, digests and manifests
CREATE TABLE evaluation_sample (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
  project_id TEXT NOT NULL,
  original_task_id TEXT NOT NULL,
  original_review_round INTEGER NOT NULL,
  submitted_sha TEXT NOT NULL,
  snapshot_digest TEXT,
  source_digest TEXT,
  manifest_digest TEXT,
  prompt_version TEXT NOT NULL,
  model_version TEXT NOT NULL,
  runtime_version TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id),
  UNIQUE (campaign_id, original_task_id, original_review_round)
);

CREATE INDEX idx_evaluation_sample_campaign ON evaluation_sample(campaign_id);
CREATE INDEX idx_evaluation_sample_task ON evaluation_sample(original_task_id);

-- A sample freezes the exact input it was drawn from; no column may change.
CREATE TRIGGER evaluation_sample_frozen BEFORE UPDATE ON evaluation_sample
BEGIN SELECT RAISE(ABORT, 'evaluation sample is immutable'); END;
CREATE TRIGGER evaluation_sample_no_delete BEFORE DELETE ON evaluation_sample
BEGIN SELECT RAISE(ABORT, 'evaluation sample is immutable'); END;

-- Evaluation candidate: an immutable candidate version. identity_json is the
-- canonical M1 CandidateIdentity and candidate_config_digest its digest; the
-- store recomputes the digest on write and verifies it on read. account_pool_id
-- is denormalized from the identity for admission and is only the pool name.
CREATE TABLE evaluation_candidate (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
  identity_json TEXT NOT NULL CHECK (json_valid(identity_json)),
  candidate_config_digest TEXT NOT NULL CHECK (length(candidate_config_digest) > 0),
  account_pool_id TEXT NOT NULL CHECK (length(account_pool_id) > 0),
  per_candidate_cap INTEGER NOT NULL CHECK (per_candidate_cap >= 1),
  created_at TEXT NOT NULL,
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id)
);

CREATE INDEX idx_evaluation_candidate_campaign ON evaluation_candidate(campaign_id);

CREATE TRIGGER evaluation_candidate_frozen BEFORE UPDATE ON evaluation_candidate
BEGIN SELECT RAISE(ABORT, 'evaluation candidate is immutable'); END;
CREATE TRIGGER evaluation_candidate_no_delete BEFORE DELETE ON evaluation_candidate
BEGIN SELECT RAISE(ABORT, 'evaluation candidate is immutable'); END;

-- Evaluation pool: an account/compute pool owned by evaluation, deliberately
-- separate from research_pool so evaluation starts can never spend or occupy
-- production research allowance. 'rate' pools carry a persisted token bucket
-- (so a restart never refills it) plus a concurrent-dispatch limit;
-- 'concurrency_only' pools (local compute) carry only the concurrent limit and
-- no bucket. A pool must be configured explicitly before any start is admitted.
CREATE TABLE evaluation_pool (
  id TEXT PRIMARY KEY,
  mode TEXT NOT NULL CHECK (mode IN ('rate', 'concurrency_only')),
  start_rate REAL,
  burst_capacity INTEGER,
  concurrent_limit INTEGER NOT NULL CHECK (concurrent_limit >= 1),
  tokens REAL NOT NULL CHECK (tokens >= 0),
  settled_at TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  CHECK (mode <> 'rate' OR (start_rate IS NOT NULL AND start_rate > 0 AND burst_capacity IS NOT NULL AND burst_capacity >= 1)),
  CHECK (mode <> 'concurrency_only' OR (start_rate IS NULL AND burst_capacity IS NULL))
);

-- Evaluation job: claim/lease/attempt/result lifecycle
CREATE TABLE evaluation_job (
  id TEXT PRIMARY KEY,
  sample_id TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  current_attempt_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (sample_id) REFERENCES evaluation_sample(id),
  FOREIGN KEY (candidate_id) REFERENCES evaluation_candidate(id)
);

CREATE INDEX idx_evaluation_job_sample ON evaluation_job(sample_id);
CREATE INDEX idx_evaluation_job_candidate ON evaluation_job(candidate_id);

-- Evaluation attempt: immutable job execution lifecycle
CREATE TABLE evaluation_attempt (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  request_id TEXT NOT NULL UNIQUE,
  account_pool_id TEXT NOT NULL,
  previous_attempt_id TEXT,
  sequence_number INTEGER NOT NULL CHECK (sequence_number >= 1),
  state TEXT NOT NULL CHECK (state IN ('active', 'finalized', 'expired')),
  started_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  ended_at TEXT,
  exit_class TEXT CHECK (exit_class IN (
    'completed', 'failed', 'cancelled', 'unknown', 'lease_expired',
    'timeout', 'unavailable_snapshot', 'unavailable_source',
    'invalid_output', 'incomplete_output'
  )),
  status TEXT CHECK (status IS NULL OR status IN ('completed', 'incomplete', 'unsupported', 'interrupted', 'failed')),
  error_class TEXT CHECK (error_class IS NULL OR error_class IN (
    'capability_missing', 'output_truncated', 'source_unavailable', 'budget_exhausted',
    'interrupted', 'timeout', 'runtime_error', 'output_malformed', 'output_missing',
    'auth_missing', 'launch_error'
  )),
  error_message TEXT CHECK (error_message IS NULL OR length(error_message) <= 1024),
  duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0),
  usage_tokens INTEGER CHECK (usage_tokens IS NULL OR usage_tokens >= 0),
  UNIQUE (job_id, sequence_number),
  FOREIGN KEY (job_id) REFERENCES evaluation_job(id) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX idx_evaluation_attempt_live ON evaluation_attempt(expires_at) WHERE state = 'active';
CREATE INDEX idx_evaluation_attempt_live_pool ON evaluation_attempt(account_pool_id, expires_at) WHERE state = 'active';
CREATE INDEX idx_evaluation_attempt_job ON evaluation_attempt(job_id);

-- A finalized or expired attempt is a recorded result and never changes.
CREATE TRIGGER evaluation_attempt_terminal_frozen BEFORE UPDATE ON evaluation_attempt
WHEN OLD.state <> 'active'
BEGIN SELECT RAISE(ABORT, 'evaluation attempt result is immutable'); END;
CREATE TRIGGER evaluation_attempt_no_delete BEFORE DELETE ON evaluation_attempt
BEGIN SELECT RAISE(ABORT, 'evaluation attempt is immutable'); END;

-- Evaluation findings: the structured findings recorded atomically with a
-- completed attempt's result (evaluation.Finding). Immutable once written.
CREATE TABLE evaluation_finding (
  id TEXT PRIMARY KEY,
  attempt_id TEXT NOT NULL,
  sequence_number INTEGER NOT NULL CHECK (sequence_number >= 0),
  finding_id TEXT NOT NULL CHECK (length(finding_id) > 0),
  severity TEXT NOT NULL CHECK (severity IN ('material', 'minor', 'note')),
  claim TEXT,
  summary TEXT NOT NULL CHECK (length(summary) > 0),
  evidence TEXT,
  UNIQUE (attempt_id, sequence_number),
  UNIQUE (attempt_id, finding_id),
  FOREIGN KEY (attempt_id) REFERENCES evaluation_attempt(id)
);

CREATE INDEX idx_evaluation_finding_attempt ON evaluation_finding(attempt_id);

CREATE TRIGGER evaluation_finding_frozen BEFORE UPDATE ON evaluation_finding
BEGIN SELECT RAISE(ABORT, 'evaluation finding is immutable'); END;
CREATE TRIGGER evaluation_finding_no_delete BEFORE DELETE ON evaluation_finding
BEGIN SELECT RAISE(ABORT, 'evaluation finding is immutable'); END;
