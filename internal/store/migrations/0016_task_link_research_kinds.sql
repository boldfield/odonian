-- Add research-specific task_link kinds for deduplication, parent linking, and source finding.
-- Research follow-up tasks need three links: one for deduplication (research_finding_dedup)
-- to ensure idempotency across repeated aggregation, one for parent linking (parent),
-- and one for source finding reference (research_source_finding).

CREATE TABLE task_link_new (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('pr', 'branch', 'commit', 'ci', 'no_op', 'research_finding_dedup', 'parent', 'research_source_finding')),
  value TEXT NOT NULL,
  tombstoned_at TEXT,
  FOREIGN KEY (task_id) REFERENCES task(id)
);

INSERT INTO task_link_new
SELECT id, task_id, kind, value, tombstoned_at
FROM task_link;

DROP TABLE task_link;

ALTER TABLE task_link_new RENAME TO task_link;

CREATE INDEX idx_task_link_kind_value ON task_link(kind, value);
