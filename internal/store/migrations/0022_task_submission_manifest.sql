-- The continuation manifest a research implement task submitted in a review round
-- (docs/features/research-continuations.md). One immutable row per task per round: a rework
-- submission adds a row for its new round and never rewrites an earlier reviewed round.
CREATE TABLE task_submission_manifest (
  id TEXT PRIMARY KEY,
  task_id TEXT NOT NULL REFERENCES task(id),
  review_round INTEGER NOT NULL,
  parent_task_id TEXT NOT NULL REFERENCES task(id),
  manifest_json TEXT NOT NULL,
  manifest_digest TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE (task_id, review_round)
);
