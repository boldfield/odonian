-- Marks a research review task as adjudicating one specific worker-disputed finding
-- that its raising reviewer maintained, per docs/features/research-track.md section 5.
-- These columns together are the finding's dispute anchor: the (round, reviewer model,
-- reviewer slot, finding id) at which the worker first disputed it (see Dispute.Round /
-- Dispute.Lineage, resolved once at dispute time in submitTask). The reviewer model and
-- slot are stored as separate columns, rather than the single "model\x00slot" lineage
-- string researchReviewerLineage builds for in-memory comparisons, so this table never
-- has to hold a TEXT value with an embedded NUL byte. The anchor stays valid
-- identifying the same finding across however many further rounds the same reviewer
-- carries it forward under a new id, because researchFindingChains always resolves a
-- prior_id chain's root back to its earliest (round, lineage, id) member.
ALTER TABLE task ADD COLUMN adjudicate_finding_round INTEGER;
ALTER TABLE task ADD COLUMN adjudicate_finding_reviewer_model TEXT;
ALTER TABLE task ADD COLUMN adjudicate_finding_reviewer_slot INTEGER;
ALTER TABLE task ADD COLUMN adjudicate_finding_id TEXT;

-- Guards against spawning a second adjudication task for the same disputed finding on
-- retry, or if more than one code path notices the same maintained dispute needing
-- adjudication before the first spawn commits.
CREATE UNIQUE INDEX idx_task_adjudicate_unique
  ON task(target_task_id, adjudicate_finding_round, adjudicate_finding_reviewer_model, adjudicate_finding_reviewer_slot, adjudicate_finding_id)
  WHERE adjudicate_finding_id IS NOT NULL;
