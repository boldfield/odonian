-- Rebuild task_link table to widen the kind CHECK constraint for research follow-up
-- tasks: 'research_finding_dedup' is the structural dedup key (parent + file + line +
-- summary, JSON-encoded) that makes follow-up creation idempotent across reviewers,
-- rounds and repeated aggregation; 'research_finding_source' links a follow-up to the
-- specific review task and finding id it was raised on. SQLite cannot alter a CHECK
-- constraint in place, so we rebuild the table. Foreign-key enforcement is disabled by
-- the migration runner. The follow-up's link to its parent task uses the existing
-- target_task_id column on task, not a task_link row.

CREATE TABLE task_link_new (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('pr', 'branch', 'commit', 'ci', 'no_op', 'research_finding_dedup', 'research_finding_source')),
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
