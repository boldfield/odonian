-- Add duration tracking, fix usage_tokens type, and add refill parameters.
-- Persist duration/exit class as required by spec.
-- Change usage_tokens to nullable INTEGER so unknown (NULL) is distinct from 0.
-- Add refill_rate and capacity_tokens for token-bucket refill logic.

ALTER TABLE research_attempt ADD COLUMN duration_ms INTEGER;
ALTER TABLE research_attempt ADD COLUMN usage_tokens_int INTEGER;
ALTER TABLE research_account_pool ADD COLUMN refill_rate REAL DEFAULT 0.0;
ALTER TABLE research_account_pool ADD COLUMN capacity_tokens INTEGER;
