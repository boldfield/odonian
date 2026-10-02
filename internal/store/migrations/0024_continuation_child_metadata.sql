-- Add new task_link kinds for storing continuation manifest child metadata.
-- SQLite doesn't support dropping inline CHECK constraints, so we recreate the table.
-- These kinds store JSON arrays of the child's metadata extracted from the reviewed manifest:
-- - continuation_child_claim_ids: the claim IDs this child task verifies
-- - continuation_child_source_start_points: starting references for sources
-- - continuation_child_file_scope: files this child touches
-- - continuation_child_acceptance_criteria: acceptance criteria as strings

CREATE TABLE task_link_new (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('pr', 'branch', 'commit', 'ci', 'no_op', 'research_finding_dedup', 'research_finding_source', 'research_parent', 'continuation_child_dedup', 'continuation_parent', 'continuation_child_claim_ids', 'continuation_child_source_start_points', 'continuation_child_file_scope', 'continuation_child_acceptance_criteria')),
  value TEXT NOT NULL,
  tombstoned_at TEXT,
  review_round INTEGER,
  FOREIGN KEY (task_id) REFERENCES task(id)
);

INSERT INTO task_link_new SELECT * FROM task_link;
DROP TABLE task_link;
ALTER TABLE task_link_new RENAME TO task_link;

CREATE INDEX idx_task_link_kind_value ON task_link(kind, value);
