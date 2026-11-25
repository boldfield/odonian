-- Add indexes to improve query performance on hot lookups

CREATE INDEX idx_event_task ON event(task_id, created_at);
CREATE INDEX idx_task_link_task ON task_link(task_id, kind);
CREATE INDEX idx_task_target ON task(target_task_id, kind, review_round) WHERE target_task_id IS NOT NULL;
CREATE INDEX idx_task_dep_depends_on ON task_dep(depends_on_id);
