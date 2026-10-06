-- Numeric queue priority P (higher runs first; docs/features/urgent-work-queue.md).
-- SQLite INTEGER is a signed 64-bit value, so server-generated priorities above the
-- manually assignable 1..1000 range persist exactly. The 1..1000 bound is enforced by
-- the store on external input, deliberately not here: Move to front, inherited and
-- reloaded values exceed 1000 and are valid.
ALTER TABLE task ADD COLUMN priority INTEGER NOT NULL DEFAULT 500 CHECK (priority >= 1);

-- Supports the reverse lookup used to walk execution lineage upward from a replacement task.
CREATE INDEX idx_task_superseded_by ON task(superseded_by) WHERE superseded_by IS NOT NULL;
