-- Store research continuation manifests submitted with research implement tasks.
-- Each manifest is stored once per task per review round, identified by the digest.
-- A rework submission replaces the manifest for the new round without mutating earlier evidence.
CREATE TABLE IF NOT EXISTS task_submission_manifest (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL,
  review_round INTEGER NOT NULL,
  manifest_json TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  parent_task_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (task_id, review_round),
  FOREIGN KEY (task_id) REFERENCES task(id),
  FOREIGN KEY (parent_task_id) REFERENCES task(id)
);

-- Index for looking up manifests by task and round
CREATE INDEX IF NOT EXISTS idx_task_submission_manifest_task_round ON task_submission_manifest(task_id, review_round);

-- Index for looking up manifests by parent task
CREATE INDEX IF NOT EXISTS idx_task_submission_manifest_parent ON task_submission_manifest(parent_task_id);
