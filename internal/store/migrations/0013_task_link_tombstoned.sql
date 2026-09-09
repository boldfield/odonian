-- Add tombstoned_at column to task_link to mark PRs that are permanently gone (404)
ALTER TABLE task_link ADD COLUMN tombstoned_at TEXT;
