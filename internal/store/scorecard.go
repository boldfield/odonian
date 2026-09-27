package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ReviewerScorecard aggregates findings by reviewer model for research tasks.
type ReviewerScorecard struct {
	Model                                   string         `json:"model"`
	FindingsRaised                          map[string]int `json:"findings_raised"`     // by severity (p1, p2, p3)
	FindingsHeld                            int            `json:"findings_held"`       // fixed or upheld on adjudication
	FindingsWithdrawn                       int            `json:"findings_withdrawn"`  // withdrawn or overturned
	FindingsUnresolved                      int            `json:"findings_unresolved"` // still open
	ApprovalsWithLaterFixedBlockingFindings int            `json:"approvals_with_later_fixed_blocking_findings"`
	TotalReviewRounds                       int            `json:"total_review_rounds"`
	SampleSize                              int            `json:"sample_size"` // number of distinct tasks reviewed
}

// ReviewerScorecards is the response for the reviewer scorecards endpoint.
type ReviewerScorecards struct {
	Scorecards []ReviewerScorecard `json:"reviewer_scorecards"`
}

// GetResearchReviewerScorecards aggregates reviewer findings across all research tasks in a project,
// spanning superseded task chains without counting the same finding twice.
func (s *sqliteStore) GetResearchReviewerScorecards(ctx context.Context, projectID string) (ReviewerScorecards, error) {
	// Query all research track tasks (including review tasks to look up reviewer models)
	// Include adjudication columns to distinguish review tasks from adjudication tasks
	query := `SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, created_at, updated_at, archived_at, superseded_by, adjudicate_finding_id, adjudicate_finding_reviewer_model, adjudicate_finding_reviewer_slot
		FROM task
		WHERE project_id = ? AND track = 'research'
		ORDER BY created_at, id`

	rows, err := s.readConn.QueryContext(ctx, query, projectID)
	if err != nil {
		return ReviewerScorecards{}, fmt.Errorf("failed to query research tasks: %w", err)
	}
	defer rows.Close()

	tasksMap := make(map[string]Task)
	adjudicationTasks := make(map[string]bool) // track which tasks are adjudication tasks
	implementTasks := make(map[string]Task)    // only implement/design tasks for processing
	for rows.Next() {
		var t Task
		var reviewModelsJSON *string
		var adjFindingID, adjFindingReviewerModel, adjFindingReviewerSlot *string
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy, &adjFindingID, &adjFindingReviewerModel, &adjFindingReviewerSlot); err != nil {
			return ReviewerScorecards{}, fmt.Errorf("failed to scan task: %w", err)
		}
		t.ReviewModels = []string{}
		if reviewModelsJSON != nil {
			if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
				return ReviewerScorecards{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
			}
		}
		tasksMap[t.ID] = t
		if adjFindingID != nil {
			adjudicationTasks[t.ID] = true
		}
		// Only add implement/design tasks to be processed
		if t.Kind == "implement" || t.Kind == "design" {
			implementTasks[t.ID] = t
		}
	}

	if err := rows.Err(); err != nil {
		return ReviewerScorecards{}, fmt.Errorf("error iterating tasks: %w", err)
	}

	// Aggregate findings by reviewer model
	scorecards := aggregateScorecards(ctx, s, tasksMap, implementTasks, adjudicationTasks)

	return ReviewerScorecards{Scorecards: scorecards}, nil
}

// aggregateScorecards aggregates findings by reviewer across research tasks
func aggregateScorecards(ctx context.Context, s *sqliteStore, allTasksMap map[string]Task, implementTasks map[string]Task, adjudicationTasks map[string]bool) []ReviewerScorecard {
	reviewerData := make(map[string]*reviewerAggregator)

	// Build chain roots map: for each task, find the root of its supersede chain
	chainRoots := make(map[string]string) // taskID -> chain root
	for taskID := range implementTasks {
		chainRoots[taskID] = findChainRoot(taskID, allTasksMap)
	}

	// Build prior_id chains: for each reviewer model on each chain root, track finding lineages
	// across all tasks in that chain. Key: chainRoot:reviewerModel, Value: map from current ID to lineage
	findingLineages := make(map[string]map[string]findingLineage)
	blockingFindingsByTask := make(map[string][]blockingFinding) // taskID -> list of blocking findings

	for taskID := range implementTasks {
		chainRoot := chainRoots[taskID]

		// Get events for this task
		events, err := s.ListEvents(ctx, taskID)
		if err != nil {
			continue
		}

		for _, event := range events {
			if event.Kind != "review" || event.SourceTaskID == nil {
				continue
			}

			// Get the reviewer model from the review task; skip adjudication tasks
			reviewTask, ok := allTasksMap[*event.SourceTaskID]
			if !ok {
				continue
			}
			if adjudicationTasks[*event.SourceTaskID] {
				continue
			}

			reviewerModel := reviewTask.Model

			// Initialize aggregator if needed
			if reviewerData[reviewerModel] == nil {
				reviewerData[reviewerModel] = &reviewerAggregator{
					model:            reviewerModel,
					findingsRaised:   make(map[string]int),
					findingsByStatus: make(map[string]int), // held, withdrawn, unresolved
					findingMap:       make(map[string]reviewingFinding),
					tasksReviewed:    make(map[string]bool),
					approvals:        make([]approvalRecord, 0), // track approvals for later-fixed logic
				}
			}

			agg := reviewerData[reviewerModel]
			agg.tasksReviewed[taskID] = true

			// Count total review rounds
			agg.totalReviews++

			// Track if this is an approval
			isApproval := event.Verdict != nil && *event.Verdict == "approve"
			if isApproval {
				agg.approvals = append(agg.approvals, approvalRecord{
					taskID:        taskID,
					round:         reviewTask.ReviewRound,
					chainRoot:     chainRoot,
					approvalRound: reviewTask.ReviewRound,
				})
			}

			// Parse findings
			var findings []Finding
			if event.Findings != nil {
				if err := json.Unmarshal(*event.Findings, &findings); err != nil {
					continue
				}
			}

			// Build lineage map for this reviewer on this entire chain root
			lineageKey := fmt.Sprintf("%s:%s", chainRoot, reviewerModel)
			if findingLineages[lineageKey] == nil {
				findingLineages[lineageKey] = make(map[string]findingLineage)
			}
			lineageMap := findingLineages[lineageKey]

			// Process each finding
			for _, finding := range findings {
				// Determine the root finding ID (follow prior_id chain backwards)
				rootID := finding.ID
				if finding.PriorID != nil {
					if prior, exists := lineageMap[*finding.PriorID]; exists {
						rootID = prior.rootID
					} else {
						// Prior ID not found yet, treat this as a new chain
						rootID = finding.ID
					}
				}

				// Get or initialize this lineage
				lineage := findingLineage{
					rootID:        rootID,
					firstRound:    reviewTask.ReviewRound,
					lastStatus:    finding.Status,
					lastSeverity:  strings.ToLower(finding.Severity),
					lastRound:     reviewTask.ReviewRound,
					isBlocking:    isBlockingFinding(finding),
					blockingRound: -1,
				}

				// Update if we've seen this before
				if existing, exists := lineageMap[finding.ID]; exists {
					lineage.rootID = existing.rootID
					lineage.firstRound = existing.firstRound
					lineage.blockingRound = existing.blockingRound
					if isBlockingFinding(finding) && lineage.blockingRound == -1 {
						lineage.blockingRound = reviewTask.ReviewRound
					}
				} else if isBlockingFinding(finding) {
					lineage.blockingRound = reviewTask.ReviewRound
				}

				lineageMap[finding.ID] = lineage

				// Use root ID + chain root + reviewer model as key for dedup
				key := fmt.Sprintf("%s:%s:%s", chainRoot, lineage.rootID, reviewerModel)

				// Track blocking findings for this task
				if isBlockingFinding(finding) {
					blockingFindingsByTask[taskID] = append(blockingFindingsByTask[taskID], blockingFinding{
						findingID:     finding.ID,
						rootFindingID: lineage.rootID,
						reviewerModel: reviewerModel,
						reviewerSlot:  reviewTask.ReviewRound,
						roundRaised:   lineage.firstRound,
						chainRoot:     chainRoot,
					})
				}

				// Count raised findings only on first appearance (new status)
				if _, exists := agg.findingMap[key]; !exists && finding.Status == "new" {
					severity := strings.ToLower(finding.Severity)
					agg.findingsRaised[severity]++
				}

				// Update finding status (keep latest by chain order)
				agg.findingMap[key] = reviewingFinding{
					id:            finding.ID,
					rootID:        lineage.rootID,
					chainRoot:     chainRoot,
					severity:      strings.ToLower(finding.Severity),
					status:        finding.Status,
					reviewerModel: reviewerModel,
					reviewerSlot:  reviewTask.ReviewRound,
					round:         reviewTask.ReviewRound,
				}
			}

			reviewerData[reviewerModel] = agg
		}
	}

	// Get project ID for adjudication query
	var projectID string
	for _, task := range implementTasks {
		projectID = task.ProjectID
		break
	}

	// Load and apply adjudication results
	adjQuery := `SELECT target_task_id, adjudicate_finding_id, adjudicate_finding_reviewer_model, adjudicate_finding_reviewer_slot, verdict
		FROM task
		WHERE project_id = ? AND track = 'research' AND kind = 'review' AND adjudicate_finding_id IS NOT NULL AND verdict IS NOT NULL`

	adjRows, err := s.readConn.QueryContext(ctx, adjQuery, projectID)
	if err != nil {
		adjRows = nil
	}

	adjResults := make(map[string]string) // key -> "upheld" or "overturned"
	if adjRows != nil {
		defer adjRows.Close()

		for adjRows.Next() {
			var targetTaskID, findingID, reviewerModel string
			var reviewerSlot int
			var verdict string
			if err := adjRows.Scan(&targetTaskID, &findingID, &reviewerModel, &reviewerSlot, &verdict); err != nil {
				continue
			}

			// Find the chain root for the target task
			chainRoot := chainRoots[targetTaskID]
			if chainRoot == "" {
				// Fall back to looking it up in allTasksMap
				if _, ok := allTasksMap[targetTaskID]; ok {
					chainRoot = findChainRoot(targetTaskID, allTasksMap)
					chainRoots[targetTaskID] = chainRoot
				} else {
					continue
				}
			}

			// Look up the lineage for this reviewer's finding
			lineageKey := fmt.Sprintf("%s:%s", chainRoot, reviewerModel)
			if lineageMap, ok := findingLineages[lineageKey]; ok {
				// Find the root ID for this finding through the lineage
				// The adjudicate_finding_id is the original disputed finding ID,
				// but it might have been renumbered, so we need to check the lineage
				rootID := findingID
				if lineage, exists := lineageMap[findingID]; exists {
					rootID = lineage.rootID
				}
				// Use root ID + chain root + reviewer model as key
				key := fmt.Sprintf("%s:%s:%s", chainRoot, rootID, reviewerModel)
				if verdict == "approve" {
					adjResults[key] = "overturned"
				} else {
					adjResults[key] = "upheld"
				}
			}
		}
	}

	// Apply status classification
	for _, agg := range reviewerData {
		for key, finding := range agg.findingMap {
			adjResult, hasAdj := adjResults[key]
			if hasAdj {
				if adjResult == "overturned" {
					agg.findingsByStatus["withdrawn"]++
					agg.findingMap[key] = reviewingFinding{
						id:            finding.id,
						rootID:        finding.rootID,
						chainRoot:     finding.chainRoot,
						severity:      finding.severity,
						status:        "withdrawn",
						reviewerModel: finding.reviewerModel,
						reviewerSlot:  finding.reviewerSlot,
						round:         finding.round,
					}
				} else { // upheld
					agg.findingsByStatus["held"]++
					agg.findingMap[key] = reviewingFinding{
						id:            finding.id,
						rootID:        finding.rootID,
						chainRoot:     finding.chainRoot,
						severity:      finding.severity,
						status:        "upheld",
						reviewerModel: finding.reviewerModel,
						reviewerSlot:  finding.reviewerSlot,
						round:         finding.round,
					}
				}
			} else if finding.status == "resolved" {
				agg.findingsByStatus["held"]++
			} else if finding.status == "still_open" {
				agg.findingsByStatus["unresolved"]++
			} else if finding.status == "new" {
				agg.findingsByStatus["unresolved"]++
			}
		}
	}

	// Compute approvals with later fixed blocking findings
	// For each approval, check if another reviewer's blocking finding was later fixed
	computeApprovalsWithLaterFix(reviewerData, blockingFindingsByTask)

	// Build final scorecards, sorted by model for determinism
	var scorecards []ReviewerScorecard
	var models []string
	for model := range reviewerData {
		models = append(models, model)
	}
	sort.Strings(models)

	for _, model := range models {
		agg := reviewerData[model]
		findingsRaised := agg.findingsRaised
		if findingsRaised == nil {
			findingsRaised = make(map[string]int)
		}
		// Ensure all keys are present
		if _, ok := findingsRaised["p1"]; !ok {
			findingsRaised["p1"] = 0
		}
		if _, ok := findingsRaised["p2"]; !ok {
			findingsRaised["p2"] = 0
		}
		if _, ok := findingsRaised["p3"]; !ok {
			findingsRaised["p3"] = 0
		}

		scorecard := ReviewerScorecard{
			Model:                                   model,
			FindingsRaised:                          findingsRaised,
			FindingsHeld:                            agg.findingsByStatus["held"],
			FindingsWithdrawn:                       agg.findingsByStatus["withdrawn"],
			FindingsUnresolved:                      agg.findingsByStatus["unresolved"],
			ApprovalsWithLaterFixedBlockingFindings: agg.approvalsWithLaterFix,
			TotalReviewRounds:                       agg.totalReviews,
			SampleSize:                              len(agg.tasksReviewed),
		}

		scorecards = append(scorecards, scorecard)
	}

	return scorecards
}

// Helper function to check if a finding is blocking
func isBlockingFinding(f Finding) bool {
	severity := strings.ToUpper(f.Severity)
	// P1 or P2 in changed text is blocking
	if (severity == "P1" || severity == "P2") && f.InChangedText {
		return true
	}
	// P1 or P2 in any text during round 1 (status new) is blocking
	if (severity == "P1" || severity == "P2") && f.Status == "new" {
		return true
	}
	// Still open findings are blocking
	if f.Status == "still_open" {
		return true
	}
	return false
}

// Helper to get any task from map for project ID extraction
func getAnyTask(taskMap map[string]Task) string {
	for id := range taskMap {
		return id
	}
	return ""
}

// Compute approvals where another reviewer's blocking finding was later fixed
func computeApprovalsWithLaterFix(reviewerData map[string]*reviewerAggregator, blockingFindingsByTask map[string][]blockingFinding) {
	// For each reviewer
	for _, agg := range reviewerData {
		// Track which tasks we've already counted for this reviewer
		countedTasks := make(map[string]bool)

		for _, approval := range agg.approvals {
			// Only count each task once per reviewer
			if countedTasks[approval.taskID] {
				continue
			}

			// Check if there were blocking findings in this task from other reviewers
			if blockings, ok := blockingFindingsByTask[approval.taskID]; ok {
				taskHasFixedBlockingFinding := false

				for _, blocking := range blockings {
					// Skip if from same reviewer model
					if blocking.reviewerModel == agg.model {
						continue
					}

					// Check if this blocking finding was later resolved/fixed (AFTER the approval)
					// by looking for it in the raising reviewer's findings marked as held/resolved
					for _, otherAgg := range reviewerData {
						if otherAgg.model != blocking.reviewerModel {
							continue
						}

						for _, finding := range otherAgg.findingMap {
							if finding.chainRoot == blocking.chainRoot &&
								finding.rootID == blocking.rootFindingID &&
								finding.round > approval.approvalRound &&
								(finding.status == "held" || finding.status == "upheld" || finding.status == "resolved") {
								taskHasFixedBlockingFinding = true
								break
							}
						}
						if taskHasFixedBlockingFinding {
							break
						}
					}

					if taskHasFixedBlockingFinding {
						break
					}
				}

				if taskHasFixedBlockingFinding {
					agg.approvalsWithLaterFix++
					countedTasks[approval.taskID] = true
				}
			}
		}
	}
}

// findChainRoot finds the root of the supersede chain for a task
func findChainRoot(taskID string, tasksMap map[string]Task) string {
	if _, ok := tasksMap[taskID]; !ok {
		return taskID
	}

	// Follow the supersede chain backwards to find the root
	current := taskID
	seen := make(map[string]bool)
	for {
		if seen[current] {
			// Cycle detected, return current
			return current
		}
		seen[current] = true

		if _, ok := tasksMap[current]; !ok {
			return current
		}

		// Check if this task was superseded by another
		found := false
		for _, otherTask := range tasksMap {
			if otherTask.SupersededBy != nil && *otherTask.SupersededBy == current {
				current = otherTask.ID
				found = true
				break
			}
		}

		if !found {
			// This is the root
			return current
		}
	}
}

// reviewerAggregator accumulates finding data for a single reviewer
type reviewerAggregator struct {
	model                 string
	findingsRaised        map[string]int // severity -> count
	findingsByStatus      map[string]int // held, withdrawn, unresolved -> count
	findingMap            map[string]reviewingFinding
	tasksReviewed         map[string]bool
	approvals             []approvalRecord
	totalReviews          int
	approvalsWithLaterFix int
}

// reviewingFinding tracks a single finding
type reviewingFinding struct {
	id            string
	rootID        string
	chainRoot     string
	severity      string
	status        string
	reviewerModel string
	reviewerSlot  int
	round         int
}

// approvalRecord tracks a reviewer's approval on a task
type approvalRecord struct {
	taskID        string
	round         int
	chainRoot     string
	approvalRound int
}

// blockingFinding tracks blocking findings for later-fix logic
type blockingFinding struct {
	findingID     string
	rootFindingID string
	reviewerModel string
	reviewerSlot  int
	roundRaised   int
	chainRoot     string
}

// findingLineage tracks a finding's lineage through prior_id chains
type findingLineage struct {
	rootID        string
	firstRound    int
	lastStatus    string
	lastSeverity  string
	lastRound     int
	isBlocking    bool
	blockingRound int
}
