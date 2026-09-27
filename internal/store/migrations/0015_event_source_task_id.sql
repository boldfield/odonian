-- Add nullable source_task_id column to event table. It records which review task
-- produced a review event on a parent, so research aggregation can key on the
-- current round's own review-task submissions instead of every review event on
-- the parent (which also include human/API AddReview events).
ALTER TABLE event ADD COLUMN source_task_id TEXT;
