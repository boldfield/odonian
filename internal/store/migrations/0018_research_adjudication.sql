-- Add adjudication support for disputed research findings
-- Adjudication tasks are review-kind tasks with:
-- - adjudicate_finding: the finding ID this task adjudicates
-- - adjudicated_by_reviewer: the reviewer model who raised the disputed finding
-- These enable tracking which finding is being adjudicated and by whom.

ALTER TABLE task ADD COLUMN adjudicate_finding TEXT;
ALTER TABLE task ADD COLUMN adjudicated_by_reviewer TEXT;
