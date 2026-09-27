-- Add nullable disputes column to event table for research task dispute submissions.
-- Disputes allow workers to contest specific findings during rework.
ALTER TABLE event ADD COLUMN disputes TEXT;
