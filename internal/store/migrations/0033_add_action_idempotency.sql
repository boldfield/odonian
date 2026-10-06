-- Add action idempotency tracking for move-to-front and other atomic operations

CREATE TABLE IF NOT EXISTS action_idempotency (
  action_key TEXT NOT NULL,
  task_id TEXT NOT NULL,
  action TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  result_priority INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (action_key),
  UNIQUE (action_key, request_hash)
);

CREATE INDEX IF NOT EXISTS idx_action_idempotency_task_action
  ON action_idempotency(task_id, action);
