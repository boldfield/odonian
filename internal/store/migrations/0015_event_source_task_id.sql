-- Add source_task_id column to event table for tracking which review task created this event.
-- This allows research aggregation to correctly identify review events from the current round
-- without relying on date ordering or LIMIT heuristics.
ALTER TABLE event ADD COLUMN source_task_id TEXT;
