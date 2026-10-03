-- Host-recorded detail of how one evaluation attempt ran: the declared and
-- effective candidate identity, the prompt and evidence-standard digests the
-- candidate received, launch diagnostics, and usage in the provider's own
-- units. usage_tokens on evaluation_attempt cannot carry units, so units are
-- kept here and are never summed or converted. The row is written in the same
-- transaction that finalizes the attempt and never changes afterwards.
CREATE TABLE evaluation_attempt_detail (
  attempt_id TEXT PRIMARY KEY,
  detail_json TEXT NOT NULL CHECK (json_valid(detail_json)),
  recorded_at TEXT NOT NULL,
  FOREIGN KEY (attempt_id) REFERENCES evaluation_attempt(id)
);

CREATE TRIGGER evaluation_attempt_detail_frozen BEFORE UPDATE ON evaluation_attempt_detail
BEGIN SELECT RAISE(ABORT, 'evaluation attempt detail is immutable'); END;
CREATE TRIGGER evaluation_attempt_detail_no_delete BEFORE DELETE ON evaluation_attempt_detail
BEGIN SELECT RAISE(ABORT, 'evaluation attempt detail is immutable'); END;
