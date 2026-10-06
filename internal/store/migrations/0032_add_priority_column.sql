-- Add priority column for work queue ordering

ALTER TABLE task ADD COLUMN priority INTEGER NOT NULL DEFAULT 500;
