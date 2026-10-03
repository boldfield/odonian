-- Latest research admission decision per task. Exactly one row per task is kept
-- and overwritten in place, so diagnostics stay bounded however often a deferred
-- task is polled, and no task event is written per poll. In observe mode the
-- row records the hypothetical decision (hypothetical = 1) while the claim is
-- still granted. task_id is deliberately not a foreign key, like research_attempt.

CREATE TABLE research_admission_diagnostic (
  task_id TEXT PRIMARY KEY,
  mode TEXT NOT NULL,
  work_class TEXT NOT NULL,
  model TEXT NOT NULL,
  account_id TEXT NOT NULL,
  outcome TEXT NOT NULL,
  reason TEXT,
  not_before TEXT,
  retry_after_ms INTEGER,
  hypothetical INTEGER NOT NULL CHECK (hypothetical IN (0, 1)),
  denial_count INTEGER NOT NULL DEFAULT 0 CHECK (denial_count >= 0),
  decided_at TEXT NOT NULL
);
