-- Add findings column to event table to store structured review findings (JSON)
ALTER TABLE event ADD COLUMN findings TEXT;
