-- Audited finding disposition workflow: each finding from an evaluation attempt
-- can be assigned a disposition (valid/invalid/unresolved) along with evidence and
-- the actor/timestamp recording the decision. Dispositions are immutable once recorded.

CREATE TABLE evaluation_finding_disposition (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
  sample_id TEXT NOT NULL,
  candidate_id TEXT NOT NULL,
  finding_id TEXT NOT NULL,
  disposition TEXT NOT NULL CHECK (disposition IN ('valid', 'invalid', 'unresolved')),
  evidence TEXT NOT NULL,
  decided_by TEXT NOT NULL,
  decided_at TEXT NOT NULL,
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id),
  FOREIGN KEY (sample_id) REFERENCES evaluation_sample(id),
  FOREIGN KEY (candidate_id) REFERENCES evaluation_candidate(id),
  UNIQUE (campaign_id, sample_id, candidate_id, finding_id)
);

CREATE TRIGGER evaluation_finding_disposition_frozen BEFORE UPDATE ON evaluation_finding_disposition
BEGIN SELECT RAISE(ABORT, 'finding disposition is immutable'); END;
CREATE TRIGGER evaluation_finding_disposition_no_delete BEFORE DELETE ON evaluation_finding_disposition
BEGIN SELECT RAISE(ABORT, 'finding disposition is immutable'); END;

-- Evaluation report generation metadata: one report per campaign/version.
-- Contains aggregated metrics and findings summary.
CREATE TABLE evaluation_report (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
  generated_at TEXT NOT NULL,
  generated_by TEXT NOT NULL,
  report_json TEXT NOT NULL CHECK (json_valid(report_json)),
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id)
);

CREATE TRIGGER evaluation_report_frozen BEFORE UPDATE ON evaluation_report
BEGIN SELECT RAISE(ABORT, 'report is immutable'); END;
CREATE TRIGGER evaluation_report_no_delete BEFORE DELETE ON evaluation_report
BEGIN SELECT RAISE(ABORT, 'report is immutable'); END;
