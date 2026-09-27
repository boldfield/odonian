-- Add adjudication support for disputed research findings
-- Adjudication tasks are review-kind tasks with:
-- - adjudicate_finding: the finding ID this task adjudicates
-- - adjudicated_by_reviewer: the reviewer model who raised the disputed finding
-- These enable tracking which finding is being adjudicated and by whom.

ALTER TABLE task ADD COLUMN adjudicate_finding TEXT;
ALTER TABLE task ADD COLUMN adjudicated_by_reviewer TEXT;

-- Unique constraint to prevent duplicate adjudication tasks for the same finding.
-- Only applies to adjudication tasks (where adjudicate_finding IS NOT NULL).
-- SQLite syntax: create a unique index that includes the WHERE clause.
CREATE UNIQUE INDEX idx_task_adjudication_unique ON task(target_task_id, adjudicate_finding)
WHERE adjudicate_finding IS NOT NULL;
