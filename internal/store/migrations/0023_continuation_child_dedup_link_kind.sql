-- Add new task_link kinds for continuation manifest child deduplication and parent link.
-- SQLite doesn't support dropping inline CHECK constraints, so we recreate the table.
-- The runner disables foreign key enforcement outside the transaction (store.go:236),
-- so constraint checks happen on commit as though they were deferred. We rebuild the index.

CREATE TABLE task_link_new (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('pr', 'branch', 'commit', 'ci', 'no_op', 'research_finding_dedup', 'research_finding_source', 'research_parent', 'continuation_child_dedup', 'continuation_parent')),
  value TEXT NOT NULL,
  tombstoned_at TEXT,
  review_round INTEGER,
  FOREIGN KEY (task_id) REFERENCES task(id)
);

INSERT INTO task_link_new SELECT * FROM task_link;
DROP TABLE task_link;
ALTER TABLE task_link_new RENAME TO task_link;

CREATE INDEX idx_task_link_kind_value ON task_link(kind, value);
