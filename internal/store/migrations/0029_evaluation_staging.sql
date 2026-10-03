-- Per-candidate staging records for frozen evaluation workspaces. Each
-- candidate stages its own fresh workspace from the sample's pinned commit;
-- this row records what was staged and the candidate-specific source and tool
-- limits it ran under. A row is written once and never changes, so adding a
-- candidate to an existing campaign adds rows and never edits earlier ones or
-- the samples they reference. No workspace content or path is stored.
CREATE TABLE evaluation_staging (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
  sample_id TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  candidate_config_digest TEXT NOT NULL CHECK (length(candidate_config_digest) > 0),
  snapshot_digest TEXT NOT NULL CHECK (length(snapshot_digest) > 0),
  source_digest TEXT NOT NULL CHECK (length(source_digest) > 0),
  prompt_version TEXT NOT NULL CHECK (length(prompt_version) > 0),
  prompt_digest TEXT NOT NULL CHECK (length(prompt_digest) > 0),
  limits_json TEXT NOT NULL CHECK (json_valid(limits_json)),
  staged_at TEXT NOT NULL,
  UNIQUE (campaign_id, sample_id, candidate_id),
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id),
  FOREIGN KEY (sample_id) REFERENCES evaluation_sample(id),
  FOREIGN KEY (candidate_id) REFERENCES evaluation_candidate(id)
);

CREATE INDEX idx_evaluation_staging_campaign ON evaluation_staging(campaign_id);
CREATE INDEX idx_evaluation_staging_candidate ON evaluation_staging(candidate_id);

CREATE TRIGGER evaluation_staging_frozen BEFORE UPDATE ON evaluation_staging
BEGIN SELECT RAISE(ABORT, 'evaluation staging is immutable'); END;
CREATE TRIGGER evaluation_staging_no_delete BEFORE DELETE ON evaluation_staging
BEGIN SELECT RAISE(ABORT, 'evaluation staging is immutable'); END;
