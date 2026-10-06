-- One row per accepted priority action (Set or Move to front). It is both the
-- idempotency record (action_key is the caller's stable key; request_hash binds it to
-- the exact request so reuse with a different payload is rejected) and the structured
-- audit trail: the original result is replayed from here, never recomputed.
CREATE TABLE priority_action (
  action_key TEXT PRIMARY KEY,
  action TEXT NOT NULL CHECK (action IN ('set', 'front')),
  task_id TEXT NOT NULL,
  topic_anchor_id TEXT NOT NULL,
  request_hash TEXT NOT NULL,
  old_priority INTEGER NOT NULL,
  new_priority INTEGER NOT NULL,
  queue_max_priority INTEGER,
  actor TEXT NOT NULL,
  reason TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY (task_id) REFERENCES task(id)
);

CREATE INDEX idx_priority_action_task ON priority_action(task_id, created_at);
