-- Durable state for model-agnostic reviewer evaluation campaigns and jobs.
-- Campaigns are finite, bounded experiments with cohorts, attempt caps and pools.
-- Jobs are samples evaluated by candidates with immutable claims, leases and results.
-- No credentials are stored. Evaluation jobs cannot vote, reject, create follow-ups,
-- adjudicate disputes or affect production review scorecards.

-- Evaluation campaign: finite experiment config with allowed projects, model,
-- cohort manifest, attempt cap and account pool.
CREATE TABLE evaluation_campaign (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  description TEXT,
  project_id TEXT NOT NULL,
  allowed_model_id TEXT NOT NULL,
  cohort_manifest TEXT NOT NULL,
  attempt_cap INTEGER NOT NULL CHECK (attempt_cap >= 1),
  account_pool_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE INDEX idx_evaluation_campaign_project ON evaluation_campaign(project_id);

-- Evaluation sample: original task/review round, submitted SHA, digests and manifests
CREATE TABLE evaluation_sample (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
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

-- Evaluation candidate: immutable model version, adapter, runtime config
CREATE TABLE evaluation_candidate (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
  adapter_name TEXT NOT NULL,
  model_identity TEXT NOT NULL,
  model_revision TEXT,
  runtime_version TEXT NOT NULL,
  reasoning_config TEXT,
  generation_config TEXT,
  prompt_version TEXT NOT NULL,
  tool_access_config TEXT,
  source_access_config TEXT,
  account_pool_id TEXT NOT NULL,
  per_candidate_cap INTEGER NOT NULL CHECK (per_candidate_cap >= 1),
  created_at TEXT NOT NULL,
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id)
);

CREATE INDEX idx_evaluation_candidate_campaign ON evaluation_candidate(campaign_id);

-- Evaluation job: claim/lease/attempt/result lifecycle
CREATE TABLE evaluation_job (
  id TEXT PRIMARY KEY,
  sample_id TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  request_id TEXT NOT NULL UNIQUE,
  current_attempt_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (sample_id) REFERENCES evaluation_sample(id),
  FOREIGN KEY (candidate_id) REFERENCES evaluation_candidate(id),
  UNIQUE (sample_id, candidate_id)
);

CREATE INDEX idx_evaluation_job_sample ON evaluation_job(sample_id);
CREATE INDEX idx_evaluation_job_candidate ON evaluation_job(candidate_id);

-- Evaluation attempt: immutable job execution lifecycle
CREATE TABLE evaluation_attempt (
  id TEXT PRIMARY KEY,
  job_id TEXT NOT NULL,
  previous_attempt_id TEXT,
  sequence_number INTEGER NOT NULL CHECK (sequence_number >= 1),
  state TEXT NOT NULL CHECK (state IN ('active', 'finalized', 'expired')),
  started_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  ended_at TEXT,
  exit_class TEXT,
  status TEXT,
  error_class TEXT,
  error_message TEXT,
  duration_ms INTEGER CHECK (duration_ms IS NULL OR duration_ms >= 0),
  usage_tokens INTEGER CHECK (usage_tokens IS NULL OR usage_tokens >= 0),
  UNIQUE (job_id, sequence_number),
  FOREIGN KEY (job_id) REFERENCES evaluation_job(id) DEFERRABLE INITIALLY DEFERRED
);

CREATE INDEX idx_evaluation_attempt_live ON evaluation_attempt(expires_at) WHERE state = 'active';
CREATE INDEX idx_evaluation_attempt_job ON evaluation_attempt(job_id);

-- Evaluation findings: immutable structured findings from a completed attempt
CREATE TABLE evaluation_finding (
  id TEXT PRIMARY KEY,
  attempt_id TEXT NOT NULL,
  sequence_number INTEGER NOT NULL CHECK (sequence_number >= 0),
  severity TEXT NOT NULL,
  file TEXT NOT NULL,
  line INTEGER NOT NULL,
  summary TEXT NOT NULL,
  context TEXT,
  UNIQUE (attempt_id, sequence_number),
  FOREIGN KEY (attempt_id) REFERENCES evaluation_attempt(id)
);

CREATE INDEX idx_evaluation_finding_attempt ON evaluation_finding(attempt_id);
