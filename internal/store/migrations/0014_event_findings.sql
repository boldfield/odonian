-- Add nullable findings column to event table for structured review findings (research track).
ALTER TABLE event ADD COLUMN findings TEXT;
