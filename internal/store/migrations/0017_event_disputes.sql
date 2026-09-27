-- Add nullable disputes column to event table for worker finding disputes (research track).
ALTER TABLE event ADD COLUMN disputes TEXT;
