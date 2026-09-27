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
	query := `SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, created_at, updated_at, archived_at, superseded_by
		FROM task
		WHERE project_id = ? AND track = 'research'
		ORDER BY created_at, id`

	rows, err := s.readConn.QueryContext(ctx, query, projectID)
	if err != nil {
		return ReviewerScorecards{}, fmt.Errorf("failed to query research tasks: %w", err)
	}
	defer rows.Close()

	tasksMap := make(map[string]Task)
	implementTasks := make(map[string]Task) // only implement/design tasks for processing
	for rows.Next() {
		var t Task
		var reviewModelsJSON *string
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy); err != nil {
			return ReviewerScorecards{}, fmt.Errorf("failed to scan task: %w", err)
		}
		t.ReviewModels = []string{}
		if reviewModelsJSON != nil {
			if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
				return ReviewerScorecards{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
			}
		}
		tasksMap[t.ID] = t
		// Only add implement/design tasks to be processed
		if t.Kind == "implement" || t.Kind == "design" {
			implementTasks[t.ID] = t
		}
	}

	if err := rows.Err(); err != nil {
		return ReviewerScorecards{}, fmt.Errorf("error iterating tasks: %w", err)
	}

	// Aggregate findings by reviewer model
	scorecards := aggregateScorecards(ctx, s, tasksMap, implementTasks)

	return ReviewerScorecards{Scorecards: scorecards}, nil
}

// aggregateScorecards aggregates findings by reviewer across research tasks
func aggregateScorecards(ctx context.Context, s *sqliteStore, allTasksMap map[string]Task, implementTasks map[string]Task) []ReviewerScorecard {
	reviewerData := make(map[string]*reviewerAggregator)

	// Build chain roots map: for each task, find the root of its supersede chain
	chainRoots := make(map[string]string) // taskID -> chain root
	for taskID := range implementTasks {
		chainRoots[taskID] = findChainRoot(taskID, allTasksMap)
	}

	// Process each task's findings (only implement/design tasks)
	for taskID := range implementTasks {
		chainRoot := chainRoots[taskID]

		// Get events for this task (where kind='review' and source_task_id IS NOT NULL)
		events, err := s.ListEvents(ctx, taskID)
		if err != nil {
			continue
		}

		for _, event := range events {
			if event.Kind != "review" || event.SourceTaskID == nil {
				continue
			}

			// Get the reviewer model from the review task
			reviewTask, ok := allTasksMap[*event.SourceTaskID]
			if !ok {
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
				}
			}

			agg := reviewerData[reviewerModel]
			agg.tasksReviewed[taskID] = true

			// Count total review rounds (both approve and reject)
			agg.totalReviews++

			// Parse findings
			var findings []Finding
			if event.Findings != nil {
				if err := json.Unmarshal(*event.Findings, &findings); err != nil {
					continue
				}
			}

			// Process each finding
			for _, finding := range findings {
				// Use chain root + finding id as key to avoid duplicates across supersede chain
				key := fmt.Sprintf("%s:%s", chainRoot, finding.ID)

				if _, exists := agg.findingMap[key]; !exists {
					// New finding
					severity := strings.ToLower(finding.Severity)
					agg.findingsRaised[severity]++
				}

				// Update finding status
				agg.findingMap[key] = reviewingFinding{
					id:            finding.ID,
					chainRoot:     chainRoot,
					severity:      strings.ToLower(finding.Severity),
					status:        finding.Status,
					reviewerModel: reviewerModel,
				}
			}

			reviewerData[reviewerModel] = agg
		}
	}

	// Check adjudication results to determine final finding status
	// Query for adjudication tasks and apply their verdicts
	adjQuery := `SELECT target_task_id, adjudicate_finding_id, verdict
		FROM task
		WHERE kind = 'review' AND adjudicate_finding_id IS NOT NULL AND verdict IS NOT NULL`

	adjRows, err := s.readConn.QueryContext(ctx, adjQuery)
	if err == nil {
		defer adjRows.Close()

		adjResults := make(map[string]string) // "chainRoot:findingId" -> "upheld" or "overturned"
		for adjRows.Next() {
			var targetTaskID, findingID, verdict string
			if err := adjRows.Scan(&targetTaskID, &findingID, &verdict); err != nil {
				continue
			}

			// Find the chain root for the target task
			if chainRoot, ok := findChainRootForAdjudication(targetTaskID, allTasksMap); ok {
				key := fmt.Sprintf("%s:%s", chainRoot, findingID)
				if verdict == "approve" {
					adjResults[key] = "overturned"
				} else {
					adjResults[key] = "upheld"
				}
			}
		}

		// Apply adjudication results
		for model, agg := range reviewerData {
			for key, finding := range agg.findingMap {
				adjResult, hasAdj := adjResults[key]
				if hasAdj {
					if adjResult == "overturned" {
						agg.findingsByStatus["withdrawn"]++
					} else { // upheld
						agg.findingsByStatus["held"]++
					}
				} else if finding.status == "resolved" {
					agg.findingsByStatus["held"]++
				} else if finding.status == "still_open" {
					agg.findingsByStatus["unresolved"]++
				} else if finding.status == "new" {
					agg.findingsByStatus["unresolved"]++
				}
			}
			reviewerData[model] = agg
		}
	}

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

// findChainRootForAdjudication finds the chain root for an adjudication target task
func findChainRootForAdjudication(targetTaskID string, tasksMap map[string]Task) (string, bool) {
	// For adjudication, we need to find the root of the chain that the target task belongs to
	if _, ok := tasksMap[targetTaskID]; !ok {
		return "", false
	}

	// Start from the target task and follow supersede chain
	current := targetTaskID
	seen := make(map[string]bool)
	for {
		if seen[current] {
			return current, true
		}
		seen[current] = true

		if _, ok := tasksMap[current]; !ok {
			return current, true
		}

		// Check if this task supersedes another (look for tasks that have current as SupersededBy)
		found := false
		for _, otherTask := range tasksMap {
			if otherTask.SupersededBy != nil && *otherTask.SupersededBy == current {
				current = otherTask.ID
				found = true
				break
			}
		}

		if !found {
			return current, true
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
	totalReviews          int
	approvalsWithLaterFix int
}

// reviewingFinding tracks a single finding
type reviewingFinding struct {
	id            string
	chainRoot     string
	severity      string
	status        string
	reviewerModel string
}
