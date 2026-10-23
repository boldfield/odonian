-- A review child spawned by a review_models change (UpdateTaskReviewModels) to stand in for
-- a retired child of the same model records that child's id here, so research slot
-- assignment gives it the retired child's slot and every other child of the round keeps its
-- own (docs/features/dispatch-failure-handling.md D5). NULL for every other task.
ALTER TABLE task ADD COLUMN replaces_review_task_id TEXT;
