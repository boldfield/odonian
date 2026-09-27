package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ReviewerScorecard aggregates one reviewer model's findings across every research
// task in a project, per docs/features/research-track.md section 8.
type ReviewerScorecard struct {
	Model                                   string         `json:"model"`
	FindingsRaised                          map[string]int `json:"findings_raised"`     // by severity (p1, p2, p3)
	FindingsHeld                            int            `json:"findings_held"`       // fixed by the worker, or upheld on adjudication
	FindingsWithdrawn                       int            `json:"findings_withdrawn"`  // withdrawn, or overturned on adjudication
	FindingsUnresolved                      int            `json:"findings_unresolved"` // neither held nor withdrawn yet; accuracy is never inferred for these
	ApprovalsWithLaterFixedBlockingFindings int            `json:"approvals_with_later_fixed_blocking_findings"`
	TotalReviewRounds                       int            `json:"total_review_rounds"`
	SampleSize                              int            `json:"sample_size"` // number of distinct tasks reviewed
}

// ReviewerScorecards is the response for the reviewer scorecards endpoint.
type ReviewerScorecards struct {
	Scorecards []ReviewerScorecard `json:"reviewer_scorecards"`
}

// GetResearchReviewerScorecards aggregates reviewer findings across every research
// task in projectID, spanning each task's supersede chain per section 8 without
// counting a finding carried across a supersession twice. See processChain for how a
// finding's identity is tracked within and across a chain, and applyAdjudications for
// how a disputed finding's ruling (section 5) overrides its natural classification.
func (s *sqliteStore) GetResearchReviewerScorecards(ctx context.Context, projectID string) (ReviewerScorecards, error) {
	tx, err := s.readConn.BeginTx(ctx, nil)
	if err != nil {
		return ReviewerScorecards{}, fmt.Errorf("failed to begin read transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind,
		       review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track,
		       created_at, updated_at, archived_at, superseded_by, adjudicate_finding_id
		FROM task
		WHERE project_id = ? AND track = 'research'
		ORDER BY created_at, id
	`, projectID)
	if err != nil {
		return ReviewerScorecards{}, fmt.Errorf("failed to query research tasks: %w", err)
	}

	allTasks := make(map[string]Task)
	adjudicationTaskIDs := make(map[string]bool)
	var chainMembers []Task
	for rows.Next() {
		var t Task
		var reviewModelsJSON *string
		var adjFindingID sql.NullString
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee,
			&t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID,
			&t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt,
			&t.SupersededBy, &adjFindingID); err != nil {
			rows.Close()
			return ReviewerScorecards{}, fmt.Errorf("failed to scan task: %w", err)
		}
		allTasks[t.ID] = t
		if adjFindingID.Valid {
			adjudicationTaskIDs[t.ID] = true
		}
		if t.Kind == "implement" || t.Kind == "design" {
			chainMembers = append(chainMembers, t)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ReviewerScorecards{}, fmt.Errorf("failed to iterate research tasks: %w", err)
	}
	rows.Close()

	agg := newScorecardAggregation()
	for _, chain := range buildResearchChains(chainMembers) {
		if err := agg.processChain(ctx, tx, s, allTasks, adjudicationTaskIDs, chain); err != nil {
			return ReviewerScorecards{}, err
		}
	}
	if err := agg.applyAdjudications(ctx, tx, projectID); err != nil {
		return ReviewerScorecards{}, err
	}

	return ReviewerScorecards{Scorecards: agg.buildScorecards()}, nil
}

// buildResearchChains groups research implement/design tasks into their supersede
// chains (docs/features/research-track.md section 7), each ordered from the chain's
// original task through every replacement, oldest first. superseded_by already points
// forward (predecessor to successor), so walking it from each chain's root is O(n)
// total, unlike searching every task for a predecessor at every step.
func buildResearchChains(tasks []Task) [][]Task {
	byID := make(map[string]Task, len(tasks))
	hasPredecessor := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
		if t.SupersededBy != nil {
			hasPredecessor[*t.SupersededBy] = true
		}
	}

	var chains [][]Task
	for _, t := range tasks {
		if hasPredecessor[t.ID] {
			continue // reached below, while walking forward from its chain's root
		}
		var chain []Task
		seen := make(map[string]bool)
		cur := t
		for {
			if seen[cur.ID] {
				break // defensive: a cycle should never occur, but never loop forever
			}
			seen[cur.ID] = true
			chain = append(chain, cur)
			if cur.SupersededBy == nil {
				break
			}
			next, ok := byID[*cur.SupersededBy]
			if !ok {
				break
			}
			cur = next
		}
		chains = append(chains, chain)
	}
	return chains
}

const chainRoundOffset = 1_000_000

// globalRound gives a chain member's review_round (which restarts at 1 on every
// replacement) a position that increases monotonically across the whole chain, so
// findings and approvals from different chain members can be compared temporally.
func globalRound(taskIdx, localRound int) int {
	return taskIdx*chainRoundOffset + localRound
}

// findingThread is one logical finding raised by one reviewer lineage (model plus
// slot, researchReviewerLineage), followed across rounds within a task via prior_id
// (researchFindingChains) and across a supersede boundary the same way: the
// replacement's round 1 report must itself carry a prior_id, set to the id the
// predecessor's carried findings block (buildResearchSupersessionSpec) gave the
// finding, matched against lastKnownID — see findCarriedByPriorID and processChain.
// Unlike matching by severity/file/line/summary, this never depends on the
// replacement reviewer's wording, which section 3 gives them free rein to change on
// every round. A carried thread nothing in the replacement's own round links to stays
// unresolved rather than being inferred fixed, since a replacement's round 1 is a full
// review, not one scoped to the carried findings the way a same-task re-review is.
// Its classification (held, withdrawn or unresolved) is decided once, in
// buildScorecards, after every chain and every adjudication has been applied.
type findingThread struct {
	reviewerModel string
	chainRoot     string // the chain's root task id (chain[0].ID), so computeApprovalsWithLaterFix only compares within one chain
	lastKnownID   string // the id of the most recent report in this thread, for cross-boundary prior_id matching (findCarriedByPriorID)

	firstBlockingGlobalRound int   // 0 if never a blocking finding; else the earliest globalRound it was
	resolvedGlobalRounds     []int // every globalRound at which a report, or an implicit non-restatement, settled it
	finalOutstanding         bool  // per the latest information seen, whether it is still outstanding

	disputed    bool   // true if the worker disputed one of this thread's reports (event.disputes) and it later settled without adjudication
	adjudicated string // "", "held" or "withdrawn": set by applyAdjudications, overriding the natural classification
}

// scorecardApproval is one reviewer's approval of one review round, kept for
// computeApprovalsWithLaterFix.
type scorecardApproval struct {
	model       string
	chainRoot   string // the chain's root task id, so the approval only competes against blocking findings raised in the same chain
	globalRound int
}

// scorecardAggregation accumulates scorecard state across every research chain in a
// project. threadByReport lets applyAdjudications map a disputed finding (identified
// by the task, reviewer lineage, round and id it was disputed under) back to the
// thread it belongs to, however that finding has since been renumbered or carried
// across a supersession.
type scorecardAggregation struct {
	reviewers      map[string]*reviewerAggregator
	threads        []*findingThread
	threadByReport map[string]*findingThread
	approvals      []scorecardApproval
}

func newScorecardAggregation() *scorecardAggregation {
	return &scorecardAggregation{
		reviewers:      make(map[string]*reviewerAggregator),
		threadByReport: make(map[string]*findingThread),
	}
}

// reviewerAggregator accumulates the counters for one reviewer model.
type reviewerAggregator struct {
	model                 string
	findingsRaised        map[string]int
	held                  int
	withdrawn             int
	unresolved            int
	tasksReviewed         map[string]bool
	totalReviews          int
	approvalsWithLaterFix int
}

func (agg *scorecardAggregation) reviewerAgg(model string) *reviewerAggregator {
	r, ok := agg.reviewers[model]
	if !ok {
		r = &reviewerAggregator{model: model, findingsRaised: make(map[string]int), tasksReviewed: make(map[string]bool)}
		agg.reviewers[model] = r
	}
	return r
}

func reportKey(taskID, lineage string, round int, findingID string) string {
	return fmt.Sprintf("%s\x00%s\x00%d\x00%s", taskID, lineage, round, findingID)
}

// findCarriedByPriorID looks for an open thread, carried out of an earlier chain
// member, whose most recent report's id (lastKnownID) matches a chain member's own
// round-1 report's prior_id: the id buildResearchSupersessionSpec gave the finding,
// carried forward from the predecessor's last round, in its "Structured findings
// (JSON)" block. Unlike matching by severity, file, line and summary, this never
// depends on how the replacement's reviewer words the same defect on rediscovery,
// which section 3 gives them free rein to change every round: a replacement's round 1
// is always a full review, so a persisting defect is normally rediscovered fresh,
// reworded, and under a new id, unless the reviewer deliberately links back with
// prior_id against the id it was carried under.
func findCarriedByPriorID(candidates []*findingThread, priorID string) *findingThread {
	for _, c := range candidates {
		if c.lastKnownID == priorID {
			return c
		}
	}
	return nil
}

// groupResearchFindingsByChain buckets allFindings' indices by their local
// prior_id-chain root (researchFindingChains), preserving the order each root was
// first seen in, so a chain's own findings are processed oldest first.
func groupResearchFindingsByChain(allFindings []researchCollectedFinding, chainOf func(int) int) (groups map[int][]int, order []int) {
	groups = make(map[int][]int)
	for i := range allFindings {
		root := chainOf(i)
		if _, ok := groups[root]; !ok {
			order = append(order, root)
		}
		groups[root] = append(groups[root], i)
	}
	return groups, order
}

// processChain aggregates one supersede chain's findings and review rounds into agg.
// Within one task, a finding's identity across rounds is its prior_id lineage
// (researchFindingChains, reused from the same section-3 aggregation the review round
// itself uses), and a lineage that reviews a later round of the same task without
// restating an earlier, still-open report has settled it — the same "not restated
// means settled" rule unresolvedResearchFindings applies when deciding what a
// replacement's spec still needs to carry. Across a chain boundary, that same
// implicit rule does not apply: a replacement's round 1 is always a full review, so a
// reviewer simply rediscovers persisting defects fresh, under a new id and reworded,
// rather than deliberately restating each one. A still-outstanding thread only carries
// across the boundary as the same thread when the next chain member's own round-1
// report explicitly links back to it with prior_id (see findCarriedByPriorID); one a
// reviewer never links stays open, carried forward unchanged, however many further
// rounds or chain members pass, since nothing establishes it was ever revisited.
func (agg *scorecardAggregation) processChain(ctx context.Context, tx *sql.Tx, s *sqliteStore, allTasks map[string]Task, adjudicationTaskIDs map[string]bool, chain []Task) error {
	openByLineage := make(map[string][]*findingThread)
	chainRoot := chain[0].ID

	for taskIdx, task := range chain {
		var maxRound int
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(MAX(review_round), 0) FROM task WHERE target_task_id = ? AND kind = 'review'
		`, task.ID).Scan(&maxRound); err != nil {
			return fmt.Errorf("failed to find max review round for %s: %w", task.ID, err)
		}

		allFindings, latestSubmittedRound, err := s.collectResearchReviewReports(ctx, tx, task.ID, maxRound)
		if err != nil {
			return fmt.Errorf("failed to collect review findings for %s: %w", task.ID, err)
		}
		chainOf, isOutstanding := researchFindingChains(allFindings)
		groups, order := groupResearchFindingsByChain(allFindings, chainOf)

		openIn := openByLineage
		matchedIn := make(map[string]map[*findingThread]bool)
		openOut := make(map[string][]*findingThread)

		for _, root := range order {
			idxs := groups[root]
			first := allFindings[idxs[0]]
			last := allFindings[idxs[len(idxs)-1]]
			lineage := first.lineage
			severity := strings.ToLower(first.Severity)

			ragg := agg.reviewerAgg(first.reviewerModel)

			var thread *findingThread
			if first.round == 1 && first.PriorID != nil {
				thread = findCarriedByPriorID(openIn[lineage], *first.PriorID)
			}
			if thread != nil {
				if matchedIn[lineage] == nil {
					matchedIn[lineage] = make(map[*findingThread]bool)
				}
				matchedIn[lineage][thread] = true
			} else {
				thread = &findingThread{reviewerModel: first.reviewerModel, chainRoot: chainRoot}
				agg.threads = append(agg.threads, thread)
				ragg.findingsRaised[severity]++
			}

			for _, i := range idxs {
				f := allFindings[i]
				g := globalRound(taskIdx, f.round)
				if isBlockingResearchFinding(f.Finding, f.round) && (thread.firstBlockingGlobalRound == 0 || g < thread.firstBlockingGlobalRound) {
					thread.firstBlockingGlobalRound = g
				}
				agg.threadByReport[reportKey(task.ID, f.lineage, f.round, f.ID)] = thread
			}
			thread.lastKnownID = last.ID

			// A thread is settled either by an explicit resolved report (isOutstanding
			// false) or, within this task, by its lineage reviewing a later round
			// without restating it (last.round before the lineage's own latest
			// submitted round here). The round it actually settled is whichever of
			// those applied.
			settledRound := last.round
			outstanding := isOutstanding(idxs[len(idxs)-1])
			if outstanding && last.round != latestSubmittedRound[lineage] {
				outstanding = false
				settledRound = latestSubmittedRound[lineage]
			}
			thread.finalOutstanding = outstanding
			if thread.finalOutstanding {
				openOut[lineage] = append(openOut[lineage], thread)
			} else {
				thread.resolvedGlobalRounds = append(thread.resolvedGlobalRounds, globalRound(taskIdx, settledRound))
			}
		}

		for lineage, threads := range openIn {
			for _, thread := range threads {
				if matchedIn[lineage][thread] {
					continue // handled above: resolved there, or already carried into openOut
				}
				openOut[lineage] = append(openOut[lineage], thread) // never linked here; stays open, unchanged
			}
		}

		openByLineage = openOut

		if err := agg.collectVerdicts(ctx, s, allTasks, adjudicationTaskIDs, task.ID, taskIdx, chainRoot); err != nil {
			return err
		}
	}
	return nil
}

// collectVerdicts records each of taskID's own review events (sample size, total
// review rounds and approvals) by reviewer model, and each of its own submit events'
// disputes. This is separate from the findings walked in processChain because
// collectResearchReviewReports is findings-only: a reviewer who approves with no
// findings at all still reviewed the task and must still count toward sample size and
// total rounds. Disputes are collected here, rather than in processChain, because a
// dispute must be matched against threadByReport, which processChain has already
// fully populated for this task by the time it calls this method.
func (agg *scorecardAggregation) collectVerdicts(ctx context.Context, s *sqliteStore, allTasks map[string]Task, adjudicationTaskIDs map[string]bool, taskID string, taskIdx int, chainRoot string) error {
	events, err := s.ListEvents(ctx, taskID)
	if err != nil {
		return fmt.Errorf("failed to list events for %s: %w", taskID, err)
	}
	for _, event := range events {
		switch event.Kind {
		case "review":
			if event.SourceTaskID == nil {
				continue
			}
			if adjudicationTaskIDs[*event.SourceTaskID] {
				continue // an adjudicator's ruling is binding for one finding only; it never votes on a round
			}
			reviewTask, ok := allTasks[*event.SourceTaskID]
			if !ok {
				continue
			}
			ragg := agg.reviewerAgg(reviewTask.Model)
			ragg.tasksReviewed[taskID] = true
			ragg.totalReviews++
			if event.Verdict != nil && *event.Verdict == "approve" {
				agg.approvals = append(agg.approvals, scorecardApproval{model: reviewTask.Model, chainRoot: chainRoot, globalRound: globalRound(taskIdx, reviewTask.ReviewRound)})
			}
		case "submit":
			if err := agg.recordDisputes(taskID, event); err != nil {
				return err
			}
		}
	}
	return nil
}

// recordDisputes marks every thread named by a submit event's disputes (R10) as
// disputed: docs/features/research-track.md section 5 says a disputed finding the
// reviewer subsequently reports resolved, without going to adjudication, was
// withdrawn, not fixed by the worker. applyAdjudications overrides this when the
// reviewer instead maintains the finding and it goes to adjudication.
func (agg *scorecardAggregation) recordDisputes(taskID string, event Event) error {
	if event.Disputes == nil {
		return nil
	}
	var disputes []Dispute
	if err := json.Unmarshal(*event.Disputes, &disputes); err != nil {
		return fmt.Errorf("failed to unmarshal disputes for %s: %w", taskID, err)
	}
	for _, d := range disputes {
		if thread, ok := agg.threadByReport[reportKey(taskID, d.Lineage, d.Round, d.FindingID)]; ok {
			thread.disputed = true
		}
	}
	return nil
}

// applyAdjudications overrides a disputed finding's natural classification with the
// adjudicator's binding ruling (docs/features/research-track.md section 5): approve
// overturns it (withdrawn), reject upholds it (held) — regardless of whether the
// finding was independently restated or resolved afterward. The disputed finding is
// identified by the task, reviewer lineage and round it was disputed under, plus the
// id it carried in that round; threadByReport resolves that back to its thread
// however the finding has since been renumbered or carried across a supersession.
func (agg *scorecardAggregation) applyAdjudications(ctx context.Context, tx *sql.Tx, projectID string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT target_task_id, adjudicate_finding_round, adjudicate_finding_reviewer_model,
		       adjudicate_finding_reviewer_slot, adjudicate_finding_id, verdict
		FROM task
		WHERE project_id = ? AND track = 'research' AND kind = 'review'
		  AND adjudicate_finding_id IS NOT NULL AND verdict IS NOT NULL
	`, projectID)
	if err != nil {
		return fmt.Errorf("failed to query adjudications: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var targetTaskID, model, findingID, verdict string
		var round, slot int
		if err := rows.Scan(&targetTaskID, &round, &model, &slot, &findingID, &verdict); err != nil {
			return fmt.Errorf("failed to scan adjudication: %w", err)
		}
		lineage := researchReviewerLineage(model, slot)
		thread, ok := agg.threadByReport[reportKey(targetTaskID, lineage, round, findingID)]
		if !ok {
			continue // the disputed finding's own report is out of the aggregation window
		}
		if verdict == "approve" {
			thread.adjudicated = "withdrawn"
		} else {
			thread.adjudicated = "held"
		}
	}
	return rows.Err()
}

// computeApprovalsWithLaterFix implements the fourth section 8 metric: for each
// reviewer's approval of a round, whether another reviewer's blocking finding —
// already raised as of that round — was fixed in a later round, anywhere in the same
// chain. "Fixed" here means naturally resolved, not merely adjudicated: an
// overturned finding was never valid, so nothing was fixed, and an upheld finding
// still blocks, so it was not fixed either.
func (agg *scorecardAggregation) computeApprovalsWithLaterFix() {
	for _, appr := range agg.approvals {
		ragg := agg.reviewerAgg(appr.model)
		for _, thread := range agg.threads {
			if thread.chainRoot != appr.chainRoot {
				continue // a blocking finding on an unrelated chain was never relevant to this approval
			}
			if thread.reviewerModel == appr.model {
				continue
			}
			if thread.firstBlockingGlobalRound == 0 || thread.firstBlockingGlobalRound > appr.globalRound {
				continue
			}
			fixedLater := false
			for _, r := range thread.resolvedGlobalRounds {
				if r > appr.globalRound {
					fixedLater = true
					break
				}
			}
			if fixedLater {
				ragg.approvalsWithLaterFix++
				break
			}
		}
	}
}

// buildScorecards decides each thread's final classification — an adjudication
// ruling wins if one applies; otherwise a thread that ended outstanding is unresolved,
// and one that settled naturally (per processChain) is held, unless the worker
// disputed it and the reviewer never maintained it into adjudication, per section 5
// ("if it withdraws the finding, the finding is resolved") — that settlement is a
// withdrawal, not a fix — then renders every reviewer's counters, sorted by model for
// a deterministic response.
func (agg *scorecardAggregation) buildScorecards() []ReviewerScorecard {
	for _, thread := range agg.threads {
		ragg := agg.reviewerAgg(thread.reviewerModel)
		switch {
		case thread.adjudicated == "withdrawn":
			ragg.withdrawn++
		case thread.adjudicated == "held":
			ragg.held++
		case !thread.finalOutstanding && thread.disputed:
			ragg.withdrawn++
		case !thread.finalOutstanding:
			ragg.held++
		default:
			ragg.unresolved++
		}
	}

	agg.computeApprovalsWithLaterFix()

	var models []string
	for model := range agg.reviewers {
		models = append(models, model)
	}
	sort.Strings(models)

	scorecards := make([]ReviewerScorecard, 0, len(models))
	for _, model := range models {
		ragg := agg.reviewers[model]
		for _, sev := range []string{"p1", "p2", "p3"} {
			if _, ok := ragg.findingsRaised[sev]; !ok {
				ragg.findingsRaised[sev] = 0
			}
		}
		scorecards = append(scorecards, ReviewerScorecard{
			Model:                                   model,
			FindingsRaised:                          ragg.findingsRaised,
			FindingsHeld:                            ragg.held,
			FindingsWithdrawn:                       ragg.withdrawn,
			FindingsUnresolved:                      ragg.unresolved,
			ApprovalsWithLaterFixedBlockingFindings: ragg.approvalsWithLaterFix,
			TotalReviewRounds:                       ragg.totalReviews,
			SampleSize:                              len(ragg.tasksReviewed),
		})
	}
	return scorecards
}
