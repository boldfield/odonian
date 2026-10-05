-- Operator dispositions: the only source of finding labels in a reviewer
-- evaluation report. One row is one decision by one named actor about one
-- finding (a candidate's finding on a frozen sample, or a production baseline
-- reviewer's finding on the same sample), with the evidence behind it, the
-- severity the operator assigns once the finding is judged, and the claim it
-- asserts. Findings that assert the same claim on the same sample are matched
-- through the claim. Rows are append-only: a revised label is a new row, the
-- latest row for a finding wins, and the earlier rows stay as the audit trail.
-- Nothing is inferred from agreement between reviewers or from a writer
-- accepting an edit, and nothing here touches a task, review or scorecard.
CREATE TABLE evaluation_finding_disposition (
  id TEXT PRIMARY KEY,
  campaign_id TEXT NOT NULL,
  sample_id TEXT NOT NULL,
  source_kind TEXT NOT NULL CHECK (source_kind IN ('candidate', 'baseline')),
  source_id TEXT NOT NULL CHECK (length(source_id) > 0),
  candidate_id TEXT,
  finding_id TEXT NOT NULL CHECK (length(finding_id) > 0),
  label TEXT NOT NULL CHECK (label IN ('valid', 'invalid', 'unresolved')),
  severity TEXT NOT NULL CHECK (severity IN ('P1', 'P2', 'P3')),
  claim TEXT NOT NULL CHECK (length(trim(claim)) > 0 AND length(claim) <= 200),
  evidence TEXT NOT NULL CHECK (length(trim(evidence)) > 0 AND length(evidence) <= 4000),
  actor TEXT NOT NULL CHECK (length(trim(actor)) > 0 AND length(actor) <= 200),
  created_at TEXT NOT NULL,
  CHECK ((source_kind = 'candidate') = (candidate_id IS NOT NULL)),
  FOREIGN KEY (campaign_id) REFERENCES evaluation_campaign(id),
  FOREIGN KEY (sample_id) REFERENCES evaluation_sample(id),
  FOREIGN KEY (candidate_id) REFERENCES evaluation_candidate(id)
);

CREATE INDEX idx_evaluation_disposition_campaign ON evaluation_finding_disposition(campaign_id);
CREATE INDEX idx_evaluation_disposition_finding ON evaluation_finding_disposition(source_kind, source_id, finding_id);

CREATE TRIGGER evaluation_finding_disposition_frozen BEFORE UPDATE ON evaluation_finding_disposition
BEGIN SELECT RAISE(ABORT, 'evaluation finding disposition is immutable'); END;
CREATE TRIGGER evaluation_finding_disposition_no_delete BEFORE DELETE ON evaluation_finding_disposition
BEGIN SELECT RAISE(ABORT, 'evaluation finding disposition is immutable'); END;
