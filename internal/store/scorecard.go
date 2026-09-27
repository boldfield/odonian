package store

import (
	"context"
	"encoding/json"
	"fmt"
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
	query := `SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, created_at, updated_at, archived_at, superseded_by
		FROM task
		WHERE project_id = ? AND track = 'research' AND archived_at IS NULL
		ORDER BY created_at, id`

	rows, err := s.readConn.QueryContext(ctx, query, projectID)
	if err != nil {
		return ReviewerScorecards{}, fmt.Errorf("failed to query research tasks: %w", err)
	}
	defer rows.Close()

	tasksMap := make(map[string]Task)
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
	}

	if err := rows.Err(); err != nil {
		return ReviewerScorecards{}, fmt.Errorf("error iterating tasks: %w", err)
	}

	// Aggregate findings by reviewer model
	scorecards := aggregateScorecards(ctx, s, tasksMap)

	return ReviewerScorecards{Scorecards: scorecards}, nil
}

// aggregateScorecards aggregates findings by reviewer across research tasks
func aggregateScorecards(ctx context.Context, s *sqliteStore, tasksMap map[string]Task) []ReviewerScorecard {
	reviewerData := make(map[string]*reviewerAggregator)

	// Collect all review tasks by their model
	reviewTasksByModel := make(map[string][]Task) // model -> list of review tasks

	for _, task := range tasksMap {
		if task.Kind == "review" {
			reviewTasksByModel[task.Model] = append(reviewTasksByModel[task.Model], task)
		}
	}

	// For each reviewer model, aggregate their findings
	for model, reviewTasks := range reviewTasksByModel {
		if reviewerData[model] == nil {
			reviewerData[model] = &reviewerAggregator{
				model:          model,
				findingsRaised: make(map[string]int),
				findingMap:     make(map[string]findingTracker),
				tasksReviewed:  make(map[string]bool),
			}
		}

		agg := reviewerData[model]

		// Process each review task for this reviewer
		for _, reviewTask := range reviewTasks {
			if reviewTask.TargetTaskID != nil {
				agg.tasksReviewed[*reviewTask.TargetTaskID] = true
			}

			// Get events from the review task
			events, err := s.ListEvents(ctx, reviewTask.ID)
			if err != nil {
				continue
			}

			// Count reviews and collect findings
			for _, event := range events {
				// Count verdicts
				if event.Kind == "review" && event.Verdict != nil && *event.Verdict == "approve" {
					agg.totalReviews++
				}

				// Process findings
				if event.Findings == nil || event.Kind != "review" {
					continue
				}

				var findings []Finding
				if err := json.Unmarshal(*event.Findings, &findings); err != nil {
					continue
				}

				// Process each finding
				for _, finding := range findings {
					key := finding.ID

					if existing, ok := agg.findingMap[key]; !ok {
						// New finding
						severity := strings.ToLower(finding.Severity)
						agg.findingsRaised[severity]++
						agg.findingMap[key] = findingTracker{
							id:       finding.ID,
							severity: finding.Severity,
							status:   finding.Status,
							rounds:   1,
						}
					} else {
						// Update existing finding status
						existing.rounds++
						existing.status = finding.Status
						agg.findingMap[key] = existing
					}
				}
			}
		}
	}

	// Build scorecards from aggregator data
	var scorecards []ReviewerScorecard
	for _, agg := range reviewerData {
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
			Model:              agg.model,
			FindingsRaised:     findingsRaised,
			FindingsHeld:       countHeldFindings(agg.findingMap),
			FindingsWithdrawn:  countWithdrawnFindings(agg.findingMap),
			FindingsUnresolved: countUnresolvedFindings(agg.findingMap),
			TotalReviewRounds:  agg.totalReviews,
			SampleSize:         len(agg.tasksReviewed),
		}

		scorecards = append(scorecards, scorecard)
	}

	return scorecards
}

// reviewerAggregator accumulates finding data for a single reviewer
type reviewerAggregator struct {
	model                 string
	findingsRaised        map[string]int
	findingMap            map[string]findingTracker
	tasksReviewed         map[string]bool
	totalReviews          int
	approvalsWithLaterFix int
}

// findingTracker tracks a single finding across rounds
type findingTracker struct {
	id       string
	severity string
	status   string
	rounds   int
}

// countHeldFindings counts findings that were fixed (resolved) or upheld (still_open but not withdrawn)
func countHeldFindings(findingMap map[string]findingTracker) int {
	count := 0
	for _, f := range findingMap {
		// A finding is held if it was resolved (fixed) or is still unresolved (upheld)
		if f.status == "resolved" || f.status == "still_open" {
			count++
		}
	}
	return count
}

// countWithdrawnFindings counts findings that were withdrawn (status is new and only appeared once)
// or overturned (not present in later rounds)
func countWithdrawnFindings(findingMap map[string]findingTracker) int {
	count := 0
	for _, f := range findingMap {
		// A finding is withdrawn if it only appeared once (new status, rounds == 1)
		if f.status == "new" && f.rounds == 1 {
			count++
		}
	}
	return count
}

// countUnresolvedFindings counts findings that are still open or new
func countUnresolvedFindings(findingMap map[string]findingTracker) int {
	count := 0
	for _, f := range findingMap {
		if f.status == "new" || f.status == "still_open" {
			count++
		}
	}
	return count
}
