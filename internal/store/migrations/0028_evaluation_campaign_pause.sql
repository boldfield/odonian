-- Add pause support to evaluation campaigns.
-- Paused campaigns block new job admission and return paused-waiting status.

ALTER TABLE evaluation_campaign ADD COLUMN paused_at TEXT;
ALTER TABLE evaluation_campaign ADD COLUMN resumed_at TEXT;

CREATE INDEX idx_evaluation_campaign_paused ON evaluation_campaign(paused_at) WHERE paused_at IS NOT NULL AND resumed_at IS NULL;
