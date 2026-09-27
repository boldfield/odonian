package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestGetResearchReviewerScorecards_EmptyProject(t *testing.T) {
	s, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	proj, err := s.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	scorecards, err := s.GetResearchReviewerScorecards(ctx, proj.ID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	if len(scorecards.Scorecards) != 0 {
		t.Errorf("expected 0 scorecards for empty project, got %d", len(scorecards.Scorecards))
	}
}

func TestGetResearchReviewerScorecards_SingleReviewerWithFinding(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	// Get the review task
	opusTask, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask == nil {
		t.Fatalf("expected opus review task")
	}

	// Submit review with finding
	findingsJSON := json.RawMessage(`[{"id":"f1","severity":"P1","file":"test.txt","line":1,"summary":"test finding","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusTask, "opus-reviewer", "reject", findingsJSON)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	if len(scorecards.Scorecards) != 1 {
		t.Errorf("expected 1 scorecard, got %d", len(scorecards.Scorecards))
	}

	sc := scorecards.Scorecards[0]
	if sc.Model != "opus" {
		t.Errorf("expected model opus, got %s", sc.Model)
	}
	if sc.FindingsRaised["p1"] != 1 {
		t.Errorf("expected 1 P1 finding raised, got %d", sc.FindingsRaised["p1"])
	}
	if sc.TotalReviewRounds != 1 {
		t.Errorf("expected 1 total review round, got %d", sc.TotalReviewRounds)
	}
	if sc.SampleSize != 1 {
		t.Errorf("expected sample size 1, got %d", sc.SampleSize)
	}
	if sc.FindingsUnresolved != 1 {
		t.Errorf("expected 1 unresolved finding, got %d", sc.FindingsUnresolved)
	}
}

func TestGetResearchReviewerScorecards_MixedReviewers(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "sonnet"})

	// Get the review tasks
	opusTask, sonnetTask := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask == nil || sonnetTask == nil {
		t.Fatalf("expected both opus and sonnet review tasks")
	}

	// Submit reviews with findings
	findingsJSON := json.RawMessage(`[{"id":"f1","severity":"P1","file":"test.txt","line":1,"summary":"test finding","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusTask, "opus-reviewer", "reject", findingsJSON)
	submitResearchReview(t, store, ctx, sonnetTask, "sonnet-reviewer", "reject", findingsJSON)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	if len(scorecards.Scorecards) != 2 {
		t.Errorf("expected 2 scorecards, got %d", len(scorecards.Scorecards))
	}

	// Verify both reviewers are present
	models := make(map[string]bool)
	for _, sc := range scorecards.Scorecards {
		models[sc.Model] = true
		if sc.FindingsRaised["p1"] != 1 {
			t.Errorf("expected 1 P1 for %s, got %d", sc.Model, sc.FindingsRaised["p1"])
		}
	}
	if !models["opus"] || !models["sonnet"] {
		t.Errorf("expected both reviewers in scorecards, got %v", models)
	}
}

func TestGetResearchReviewerScorecards_MultipleSeverities(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	opusTask, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask == nil {
		t.Fatalf("expected opus review task")
	}

	// Submit review with multiple severities
	findingsJSON := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"test.txt","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P2","file":"test.txt","line":2,"summary":"missing info","in_changed_text":true,"status":"new"},
		{"id":"f3","severity":"P3","file":"test.txt","line":3,"summary":"wrong page","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opusTask, "opus-reviewer", "reject", findingsJSON)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}

	sc := scorecards.Scorecards[0]
	if sc.FindingsRaised["p1"] != 1 {
		t.Errorf("expected 1 P1, got %d", sc.FindingsRaised["p1"])
	}
	if sc.FindingsRaised["p2"] != 1 {
		t.Errorf("expected 1 P2, got %d", sc.FindingsRaised["p2"])
	}
	if sc.FindingsRaised["p3"] != 1 {
		t.Errorf("expected 1 P3, got %d", sc.FindingsRaised["p3"])
	}
}

func TestGetResearchReviewerScorecards_BuildDesignExcluded(t *testing.T) {
	s, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	proj, err := s.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := s.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a build track task
	_, err = s.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Build task",
			Spec:       "Build spec",
			DocumentID: doc.ID,
			Model:      "haiku",
			Track:      "build",
		},
	})
	if err != nil {
		t.Fatalf("failed to create build task: %v", err)
	}

	// Get scorecards - should not include build track
	scorecards, err := s.GetResearchReviewerScorecards(ctx, proj.ID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}

	// Should have no scorecards since no research tasks
	if len(scorecards.Scorecards) != 0 {
		t.Errorf("expected 0 scorecards for non-research tasks, got %d", len(scorecards.Scorecards))
	}
}

// TestGetResearchReviewerScorecards_BuildTrackReviewDataUntouched verifies that a
// build-track task's own review activity, in the same project as a research task,
// never leaks into the research reviewer scorecards: only track='research' review
// data is aggregated.
func TestGetResearchReviewerScorecards_BuildTrackReviewDataUntouched(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	doc, err := store.CreateDocument(ctx, projID, "feature_spec", "build-doc", "build.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	buildTasks, err := store.CreateTasks(ctx, projID, []TaskInput{
		{
			Title:        "Build task",
			Spec:         "Build spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Track:        "build",
		},
	})
	if err != nil {
		t.Fatalf("failed to create build task: %v", err)
	}
	buildTaskID := buildTasks[0].ID
	if _, err := store.PromoteTask(ctx, buildTaskID); err != nil {
		t.Fatalf("failed to promote build task: %v", err)
	}
	if _, err := store.ClaimTask(ctx, buildTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim build task: %v", err)
	}
	if _, err := store.SubmitTask(ctx, buildTaskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#200"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit build task: %v", err)
	}
	allTasks, err := store.ListTasks(ctx, projID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var buildReviewTask *Task
	for i := range allTasks {
		tk := allTasks[i]
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == buildTaskID {
			buildReviewTask = &tk
			break
		}
	}
	if buildReviewTask == nil {
		t.Fatalf("expected a review task for the build task")
	}
	if _, err := store.ClaimTask(ctx, buildReviewTask.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim build review task: %v", err)
	}
	approve := "approve"
	if _, err := store.SubmitTask(ctx, buildReviewTask.ID, "opus-reviewer", "looks good", &approve, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit build review: %v", err)
	}

	// The research task's own opus review, with one finding.
	opusTask, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask == nil {
		t.Fatalf("expected opus review task")
	}
	findingsJSON := json.RawMessage(`[{"id":"f1","severity":"P1","file":"test.txt","line":1,"summary":"test finding","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusTask, "opus-reviewer", "reject", findingsJSON)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	if len(scorecards.Scorecards) != 1 {
		t.Fatalf("expected 1 scorecard, got %d", len(scorecards.Scorecards))
	}
	sc := scorecards.Scorecards[0]
	// The build task's approve-with-no-findings review round must not be counted
	// alongside the research task's single reject round.
	if sc.TotalReviewRounds != 1 {
		t.Errorf("expected 1 total review round (build track excluded), got %d", sc.TotalReviewRounds)
	}
	if sc.SampleSize != 1 {
		t.Errorf("expected sample size 1 (build track excluded), got %d", sc.SampleSize)
	}
	if sc.FindingsRaised["p1"] != 1 {
		t.Errorf("expected 1 P1 raised, got %d", sc.FindingsRaised["p1"])
	}
}

func TestGetResearchReviewerScorecards_MultipleRounds_ResolvedFindings(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	// Round 1: Opus raises a P2 finding
	opusTask1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil {
		t.Fatalf("expected opus review task for round 1")
	}
	findingsRound1 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", findingsRound1)

	// Resubmit parent to trigger round 2
	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Round 2: Opus reviews and marks f1 as resolved
	opusTask2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil {
		t.Fatalf("expected opus review task for round 2")
	}
	findingsRound2 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":false,"status":"resolved","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "approve", findingsRound2)

	// Check scorecards
	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	if len(scorecards.Scorecards) != 1 {
		t.Errorf("expected 1 scorecard, got %d", len(scorecards.Scorecards))
	}

	sc := scorecards.Scorecards[0]
	if sc.FindingsRaised["p2"] != 1 {
		t.Errorf("expected 1 P2 raised, got %d", sc.FindingsRaised["p2"])
	}
	if sc.FindingsHeld != 1 {
		t.Errorf("expected 1 held finding, got %d", sc.FindingsHeld)
	}
	if sc.FindingsUnresolved != 0 {
		t.Errorf("expected 0 unresolved findings, got %d", sc.FindingsUnresolved)
	}
	if sc.TotalReviewRounds != 2 {
		t.Errorf("expected 2 total review rounds, got %d", sc.TotalReviewRounds)
	}
}

func TestGetResearchReviewerScorecards_ApprovalsWithLaterFixedBlockingFindings(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "sonnet"})

	// Round 1: Opus approves, Sonnet rejects with P2 blocking finding
	opusTask1, sonnetTask1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil || sonnetTask1 == nil {
		t.Fatalf("expected both review tasks")
	}
	emptyFindings := json.RawMessage(`[]`)
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "approve", emptyFindings)
	blockingFindings := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, sonnetTask1, "sonnet-reviewer", "reject", blockingFindings)

	// Resubmit parent to trigger round 2
	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Round 2: Opus approves again, Sonnet marks f1 as resolved
	opusTask2, sonnetTask2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil || sonnetTask2 == nil {
		t.Fatalf("expected both round 2 review tasks")
	}
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "approve", emptyFindings)
	resolvedFindings := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"resolved","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, sonnetTask2, "sonnet-reviewer", "approve", resolvedFindings)

	// Check scorecards
	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}

	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	sonnetCard := findScorecardByModel(scorecards.Scorecards, "sonnet")

	if opusCard == nil || sonnetCard == nil {
		t.Fatalf("expected both reviewers in scorecards")
	}

	// Opus approved while sonnet had a blocking finding that was later fixed
	if opusCard.ApprovalsWithLaterFixedBlockingFindings != 1 {
		t.Errorf("expected opus 1 approval with later fixed, got %d", opusCard.ApprovalsWithLaterFixedBlockingFindings)
	}

	// Sonnet raised a P2 and later resolved it
	if sonnetCard.FindingsRaised["p2"] != 1 {
		t.Errorf("expected sonnet 1 P2 raised, got %d", sonnetCard.FindingsRaised["p2"])
	}
	if sonnetCard.FindingsHeld != 1 {
		t.Errorf("expected sonnet 1 held, got %d", sonnetCard.FindingsHeld)
	}
}

func findScorecardByModel(scorecards []ReviewerScorecard, model string) *ReviewerScorecard {
	for i := range scorecards {
		if scorecards[i].Model == model {
			return &scorecards[i]
		}
	}
	return nil
}

func TestGetResearchReviewerScorecards_TwoReviewersSharedFindingID(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "sonnet"})

	// Round 1: Both opus and sonnet raise finding with same ID but different models
	opusTask1, sonnetTask1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil || sonnetTask1 == nil {
		t.Fatalf("expected both review tasks")
	}

	// Both raise f1 with different severities
	opusFindings := json.RawMessage(`[{"id":"f1","severity":"P1","file":"test.txt","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"}]`)
	sonnetFindings := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":2,"summary":"missing info","in_changed_text":true,"status":"new"}]`)

	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", opusFindings)
	submitResearchReview(t, store, ctx, sonnetTask1, "sonnet-reviewer", "reject", sonnetFindings)

	// Check scorecards - each should count their own finding
	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}

	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	sonnetCard := findScorecardByModel(scorecards.Scorecards, "sonnet")

	if opusCard == nil || sonnetCard == nil {
		t.Fatalf("expected both reviewers")
	}

	// Opus should have 1 P1
	if opusCard.FindingsRaised["p1"] != 1 {
		t.Errorf("expected opus 1 P1, got %d", opusCard.FindingsRaised["p1"])
	}
	if opusCard.FindingsRaised["p2"] != 0 {
		t.Errorf("expected opus 0 P2, got %d", opusCard.FindingsRaised["p2"])
	}

	// Sonnet should have 1 P2
	if sonnetCard.FindingsRaised["p2"] != 1 {
		t.Errorf("expected sonnet 1 P2, got %d", sonnetCard.FindingsRaised["p2"])
	}
	if sonnetCard.FindingsRaised["p1"] != 0 {
		t.Errorf("expected sonnet 0 P1, got %d", sonnetCard.FindingsRaised["p1"])
	}
}

func TestGetResearchReviewerScorecards_DuplicateFindings_SameReviewer(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	// Round 1: Opus raises f1
	opusTask1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil {
		t.Fatalf("expected opus review task")
	}
	findingsRound1 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", findingsRound1)

	// Resubmit parent
	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Round 2: Opus raises f1 again as still_open
	opusTask2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil {
		t.Fatalf("expected opus review task for round 2")
	}
	findingsRound2 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"still_open","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "reject", findingsRound2)

	// Check scorecards - should count the finding only once
	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}

	sc := scorecards.Scorecards[0]
	if sc.FindingsRaised["p2"] != 1 {
		t.Errorf("expected 1 P2 raised (not doubled), got %d", sc.FindingsRaised["p2"])
	}
	if sc.FindingsUnresolved != 1 {
		t.Errorf("expected 1 unresolved, got %d", sc.FindingsUnresolved)
	}
}

func TestGetResearchReviewerScorecards_RenumberedFindings(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	// Round 1: Opus raises f1
	opusTask1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil {
		t.Fatalf("expected opus review task")
	}
	findingsRound1 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", findingsRound1)

	// Resubmit parent
	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Round 2: Opus renumbers f1 to f2 and marks as resolved
	// This is the key scenario: a finding renumbered with prior_id
	opusTask2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil {
		t.Fatalf("expected opus review task for round 2")
	}
	findingsRound2 := json.RawMessage(`[{"id":"f2","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"resolved","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "approve", findingsRound2)

	// Check scorecards - should count the finding only once (not doubled)
	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}

	if len(scorecards.Scorecards) != 1 {
		t.Errorf("expected 1 scorecard, got %d", len(scorecards.Scorecards))
	}

	sc := scorecards.Scorecards[0]
	// Should count as 1 raised P2 (not 2)
	if sc.FindingsRaised["p2"] != 1 {
		t.Errorf("expected 1 P2 raised (not doubled due to renumbering), got %d", sc.FindingsRaised["p2"])
	}
	// Should count as 1 held (resolved)
	if sc.FindingsHeld != 1 {
		t.Errorf("expected 1 held finding, got %d", sc.FindingsHeld)
	}
	// Should have 0 unresolved
	if sc.FindingsUnresolved != 0 {
		t.Errorf("expected 0 unresolved findings, got %d", sc.FindingsUnresolved)
	}
}

// TestGetResearchReviewerScorecards_SupersededChain_NoDoubleCount verifies that a
// finding still outstanding when a research task is superseded, and explicitly linked
// by the same reviewer lineage back to its carried id (buildResearchSupersessionSpec's
// "Structured findings (JSON)" block) via prior_id on the replacement, is counted as
// raised once, not twice, and correctly held once the replacement resolves it — even
// though the replacement's reviewer rewords it and reports it at a different line, as
// section 3 gives it free rein to do on every round. This is the section 8 "span
// superseded task chains without counting the same finding twice" requirement.
func TestGetResearchReviewerScorecards_SupersededChain_NoDoubleCount(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	opusA, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusA == nil {
		t.Fatalf("expected opus review task on the original task")
	}
	findingsA := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusA, "opus-reviewer", "reject", findingsA)

	successor, err := store.SupersedeTask(ctx, parentID, nil)
	if err != nil {
		t.Fatalf("failed to supersede: %v", err)
	}
	if _, err := store.PromoteTask(ctx, successor.ID); err != nil {
		t.Fatalf("failed to promote successor: %v", err)
	}
	if _, err := store.ClaimTask(ctx, successor.ID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim successor: %v", err)
	}
	if _, err := store.SubmitTask(ctx, successor.ID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit successor: %v", err)
	}

	// Round 1 on the replacement is a full review: opus rediscovers the same defect,
	// reworded and at a different line, but deliberately links it back to the carried
	// id ("f1") the predecessor's spec listed it under.
	opusB1, _ := findResearchReviewTasks(t, store, ctx, projID, successor.ID, 1)
	if opusB1 == nil {
		t.Fatalf("expected opus review task on the successor")
	}
	findingsB1 := json.RawMessage(`[{"id":"g7","severity":"P2","file":"test.txt","line":5,"summary":"still missing the info","in_changed_text":true,"status":"still_open","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusB1, "opus-reviewer", "reject", findingsB1)

	resubmitResearchImplementTask(t, store, ctx, successor.ID)

	opusB2, _ := findResearchReviewTasks(t, store, ctx, projID, successor.ID, 2)
	if opusB2 == nil {
		t.Fatalf("expected opus review task for successor round 2")
	}
	findingsB2 := json.RawMessage(`[{"id":"g8","severity":"P2","file":"test.txt","line":5,"summary":"still missing the info","in_changed_text":false,"status":"resolved","prior_id":"g7"}]`)
	submitResearchReview(t, store, ctx, opusB2, "opus-reviewer", "approve", findingsB2)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	if len(scorecards.Scorecards) != 1 {
		t.Fatalf("expected 1 scorecard, got %d", len(scorecards.Scorecards))
	}
	sc := scorecards.Scorecards[0]
	if sc.FindingsRaised["p2"] != 1 {
		t.Errorf("expected 1 P2 raised (not doubled across the supersede boundary), got %d", sc.FindingsRaised["p2"])
	}
	if sc.FindingsHeld != 1 {
		t.Errorf("expected 1 held finding, got %d", sc.FindingsHeld)
	}
	if sc.FindingsUnresolved != 0 {
		t.Errorf("expected 0 unresolved findings, got %d", sc.FindingsUnresolved)
	}
	if sc.SampleSize != 2 {
		t.Errorf("expected sample size 2 (both tasks in the chain), got %d", sc.SampleSize)
	}
	if sc.TotalReviewRounds != 3 {
		t.Errorf("expected 3 total review rounds across the chain, got %d", sc.TotalReviewRounds)
	}
}

// TestGetResearchReviewerScorecards_SupersededChain_UnlinkedFindingStaysUnresolved
// verifies that a carried finding never counts as fixed, and is never merged with an
// unrelated report, merely because the replacement's round 1 (a full review) happens
// to raise a similarly worded finding under a new id: without an explicit prior_id
// link back to the carried id, there is no reliable evidence the two are the same
// defect, or that the original was ever revisited at all, so it must stay unresolved.
// Section 8 requires accuracy is never inferred for unresolved findings.
func TestGetResearchReviewerScorecards_SupersededChain_UnlinkedFindingStaysUnresolved(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	opusA, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusA == nil {
		t.Fatalf("expected opus review task on the original task")
	}
	findingsA := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":10,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusA, "opus-reviewer", "reject", findingsA)

	successor, err := store.SupersedeTask(ctx, parentID, nil)
	if err != nil {
		t.Fatalf("failed to supersede: %v", err)
	}
	if _, err := store.PromoteTask(ctx, successor.ID); err != nil {
		t.Fatalf("failed to promote successor: %v", err)
	}
	if _, err := store.ClaimTask(ctx, successor.ID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim successor: %v", err)
	}
	if _, err := store.SubmitTask(ctx, successor.ID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit successor: %v", err)
	}

	// Round 1 on the replacement: opus reports a similarly worded finding under a
	// brand new id, status "new" (which cannot itself carry a prior_id), with no link
	// back to "f1" at all.
	opusB1, _ := findResearchReviewTasks(t, store, ctx, projID, successor.ID, 1)
	if opusB1 == nil {
		t.Fatalf("expected opus review task on the successor")
	}
	findingsB1 := json.RawMessage(`[{"id":"g1","severity":"P2","file":"test.txt","line":12,"summary":"still missing the info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusB1, "opus-reviewer", "reject", findingsB1)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	if len(scorecards.Scorecards) != 1 {
		t.Fatalf("expected 1 scorecard, got %d", len(scorecards.Scorecards))
	}
	sc := scorecards.Scorecards[0]
	// f1 (carried, never linked) and g1 (freshly raised) are each counted: 2 raised.
	if sc.FindingsRaised["p2"] != 2 {
		t.Errorf("expected 2 P2 raised (carried f1 plus fresh g1), got %d", sc.FindingsRaised["p2"])
	}
	// Neither is inferred fixed: f1 was never linked to, and g1 was never resolved.
	if sc.FindingsHeld != 0 {
		t.Errorf("expected 0 held findings (no fix inferred without a link), got %d", sc.FindingsHeld)
	}
	if sc.FindingsUnresolved != 2 {
		t.Errorf("expected 2 unresolved findings, got %d", sc.FindingsUnresolved)
	}
}

// TestGetResearchReviewerScorecards_NotRestatedInLaterRound_SettledWithinTask verifies
// that within one task (no supersession involved), a blocking finding a lineage
// raises and then does not restate in a later round it reviews is held, not
// unresolved: the same "not restated means settled" rule unresolvedResearchFindings
// applies when deciding what a replacement's spec still needs to carry.
func TestGetResearchReviewerScorecards_NotRestatedInLaterRound_SettledWithinTask(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	opusTask1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil {
		t.Fatalf("expected opus review task for round 1")
	}
	findingsRound1 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", findingsRound1)

	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Round 2: opus approves without restating f1 at all (no report, no prior_id).
	opusTask2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil {
		t.Fatalf("expected opus review task for round 2")
	}
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "approve", json.RawMessage(`[]`))

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	sc := scorecards.Scorecards[0]
	if sc.FindingsHeld != 1 {
		t.Errorf("expected 1 held finding (settled by non-restatement), got %d", sc.FindingsHeld)
	}
	if sc.FindingsUnresolved != 0 {
		t.Errorf("expected 0 unresolved findings, got %d", sc.FindingsUnresolved)
	}
}

// TestGetResearchReviewerScorecards_DisputeWithdrawnWithoutAdjudication verifies that
// a finding the worker disputes, which the reviewer then reports resolved in its next
// round without the dispute going to adjudication, is counted as withdrawn rather than
// held: docs/features/research-track.md section 5 says "if it withdraws the finding,
// the finding is resolved" — that resolution is the reviewer's withdrawal, not a fix
// the worker made, and section 8 counts these separately from fixes.
func TestGetResearchReviewerScorecards_DisputeWithdrawnWithoutAdjudication(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	opusTask1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil {
		t.Fatalf("expected opus review task for round 1")
	}
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"already covered elsewhere"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	// Round 2: opus re-evaluates against the evidence and withdraws the finding.
	opusTask2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil {
		t.Fatalf("expected opus review task for round 2")
	}
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "approve", json.RawMessage(`[{"id":"f2","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":false,"status":"resolved","prior_id":"f1"}]`))

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	sc := scorecards.Scorecards[0]
	if sc.FindingsWithdrawn != 1 {
		t.Errorf("expected 1 withdrawn (dispute settled without adjudication), got %d", sc.FindingsWithdrawn)
	}
	if sc.FindingsHeld != 0 {
		t.Errorf("expected 0 held, got %d", sc.FindingsHeld)
	}
	if sc.FindingsUnresolved != 0 {
		t.Errorf("expected 0 unresolved, got %d", sc.FindingsUnresolved)
	}
}

// TestGetResearchReviewerScorecards_WithdrawnFindingNotCountedAsLaterFix verifies that
// a blocking finding the worker disputes and its reviewer then withdraws does not
// credit another reviewer's approval of that round as one "whose blocking finding was
// subsequently fixed": the finding was withdrawn, so nothing was fixed and the
// approving reviewer was right.
func TestGetResearchReviewerScorecards_WithdrawnFindingNotCountedAsLaterFix(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "sonnet"})

	opusTask1, sonnetTask1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil || sonnetTask1 == nil {
		t.Fatalf("expected both round 1 review tasks")
	}
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnetTask1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"already covered elsewhere"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusTask2, sonnetTask2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil || sonnetTask2 == nil {
		t.Fatalf("expected both round 2 review tasks")
	}
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "approve", json.RawMessage(`[{"id":"f2","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":false,"status":"resolved","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetTask2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	sonnetCard := findScorecardByModel(scorecards.Scorecards, "sonnet")
	if opusCard == nil || sonnetCard == nil {
		t.Fatalf("expected both reviewers in scorecards")
	}
	if opusCard.FindingsWithdrawn != 1 || opusCard.FindingsHeld != 0 {
		t.Errorf("expected opus withdrawn=1 held=0, got withdrawn=%d held=%d", opusCard.FindingsWithdrawn, opusCard.FindingsHeld)
	}
	if sonnetCard.ApprovalsWithLaterFixedBlockingFindings != 0 {
		t.Errorf("expected sonnet 0 approvals with later fixed (finding was withdrawn), got %d", sonnetCard.ApprovalsWithLaterFixedBlockingFindings)
	}
}

// TestGetResearchReviewerScorecards_SupersededChain_ApprovalsWithLaterFix verifies
// that approvals-with-later-fix compares rounds by their position across the whole
// supersede chain, not by each chain member's own review_round (which restarts at 1
// on every replacement): an approval on the original task and a fix landing in round
// 1 of its replacement, explicitly linked back via prior_id, must still be recognized
// as "later", even though both are task-local round 1.
func TestGetResearchReviewerScorecards_SupersededChain_ApprovalsWithLaterFix(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "sonnet"})

	opusA, sonnetA := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusA == nil || sonnetA == nil {
		t.Fatalf("expected both review tasks on the original task")
	}
	emptyFindings := json.RawMessage(`[]`)
	submitResearchReview(t, store, ctx, opusA, "opus-reviewer", "approve", emptyFindings)
	blockingFindings := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, sonnetA, "sonnet-reviewer", "reject", blockingFindings)

	successor, err := store.SupersedeTask(ctx, parentID, nil)
	if err != nil {
		t.Fatalf("failed to supersede: %v", err)
	}
	if _, err := store.PromoteTask(ctx, successor.ID); err != nil {
		t.Fatalf("failed to promote successor: %v", err)
	}
	if _, err := store.ClaimTask(ctx, successor.ID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim successor: %v", err)
	}
	if _, err := store.SubmitTask(ctx, successor.ID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit successor: %v", err)
	}

	// Round 1 on the replacement (task-local round 1, same number as the original
	// task's own round 1): opus approves again, and sonnet explicitly links its
	// report back to the carried finding ("f1") and reports it resolved.
	opusB1, sonnetB1 := findResearchReviewTasks(t, store, ctx, projID, successor.ID, 1)
	if opusB1 == nil || sonnetB1 == nil {
		t.Fatalf("expected both review tasks on the successor")
	}
	submitResearchReview(t, store, ctx, opusB1, "opus-reviewer", "approve", emptyFindings)
	fixedFindings := json.RawMessage(`[{"id":"g1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":false,"status":"resolved","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, sonnetB1, "sonnet-reviewer", "approve", fixedFindings)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	if opusCard == nil {
		t.Fatalf("expected opus in scorecards")
	}
	if opusCard.ApprovalsWithLaterFixedBlockingFindings != 1 {
		t.Errorf("expected opus 1 approval with a later fix across the supersede boundary, got %d", opusCard.ApprovalsWithLaterFixedBlockingFindings)
	}
}

// TestGetResearchReviewerScorecards_ApprovalsWithLaterFix_ScopedToChain verifies
// that computeApprovalsWithLaterFix only matches an approval against a blocking
// finding raised on the same research chain: a reviewer who never touched a given
// chain must not be credited (or blamed) for what happened on an unrelated one in
// the same project.
func TestGetResearchReviewerScorecards_ApprovalsWithLaterFix_ScopedToChain(t *testing.T) {
	store, ctx, projID, taskAID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	// Task A: opus approves with no findings. Opus never reviews task B below.
	opusA, _ := findResearchReviewTasks(t, store, ctx, projID, taskAID, 1)
	if opusA == nil {
		t.Fatalf("expected opus review task on task A")
	}
	emptyFindings := json.RawMessage(`[]`)
	submitResearchReview(t, store, ctx, opusA, "opus-reviewer", "approve", emptyFindings)

	// Task B, a second and independent research chain in the same project: sonnet
	// rejects round 1 with a blocking P2, then resolves it in round 2.
	doc, err := store.CreateDocument(ctx, projID, "feature_spec", "test-doc-b", "test-b.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	escalate := false
	tasks, err := store.CreateTasks(ctx, projID, []TaskInput{
		{
			Title:        "Verify other claims",
			Spec:         "Verify the claims in the other doc",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"sonnet"},
			Track:        "research",
			Escalate:     &escalate,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task B: %v", err)
	}
	taskBID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskBID); err != nil {
		t.Fatalf("failed to promote task B: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskBID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task B: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskBID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#200"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit task B: %v", err)
	}

	_, sonnetB1 := findResearchReviewTasks(t, store, ctx, projID, taskBID, 1)
	if sonnetB1 == nil {
		t.Fatalf("expected sonnet review task on task B round 1")
	}
	blockingFindings := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, sonnetB1, "sonnet-reviewer", "reject", blockingFindings)

	resubmitResearchImplementTask(t, store, ctx, taskBID)

	_, sonnetB2 := findResearchReviewTasks(t, store, ctx, projID, taskBID, 2)
	if sonnetB2 == nil {
		t.Fatalf("expected sonnet review task on task B round 2")
	}
	resolvedFindings := json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"resolved","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, sonnetB2, "sonnet-reviewer", "approve", resolvedFindings)

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	if opusCard == nil {
		t.Fatalf("expected opus in scorecards")
	}
	if opusCard.ApprovalsWithLaterFixedBlockingFindings != 0 {
		t.Errorf("expected opus 0 approvals with later fix (opus never reviewed task B), got %d", opusCard.ApprovalsWithLaterFixedBlockingFindings)
	}

	sonnetCard := findScorecardByModel(scorecards.Scorecards, "sonnet")
	if sonnetCard == nil {
		t.Fatalf("expected sonnet in scorecards")
	}
	if sonnetCard.FindingsHeld != 1 {
		t.Errorf("expected sonnet 1 held finding, got %d", sonnetCard.FindingsHeld)
	}
}

// TestGetResearchReviewerScorecards_AdjudicationOverturned verifies that a finding
// overturned on adjudication (docs/features/research-track.md section 5) is counted
// as withdrawn, not held or unresolved, even though it was never independently
// resolved by the worker.
func TestGetResearchReviewerScorecards_AdjudicationOverturned(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"resolves it"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
	if len(adjTasks) != 1 {
		t.Fatalf("expected exactly one adjudication task, got %d", len(adjTasks))
	}
	submitAdjudication(t, store, ctx, &adjTasks[0], "approve") // overturns the finding

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	if opusCard == nil {
		t.Fatalf("expected opus in scorecards")
	}
	if opusCard.FindingsRaised["p2"] != 1 {
		t.Errorf("expected opus 1 P2 raised, got %d", opusCard.FindingsRaised["p2"])
	}
	if opusCard.FindingsWithdrawn != 1 {
		t.Errorf("expected opus 1 withdrawn (overturned), got %d", opusCard.FindingsWithdrawn)
	}
	if opusCard.FindingsHeld != 0 {
		t.Errorf("expected opus 0 held, got %d", opusCard.FindingsHeld)
	}
	if opusCard.FindingsUnresolved != 0 {
		t.Errorf("expected opus 0 unresolved (adjudication settles it), got %d", opusCard.FindingsUnresolved)
	}
	// The adjudicator itself must never be scored as a reviewer: its ruling doesn't
	// vote on the round and it never gets its own review task on this parent's chain.
	if findScorecardByModel(scorecards.Scorecards, "haiku") != nil {
		t.Errorf("expected the adjudicator model not to appear in reviewer scorecards")
	}
}

// TestGetResearchReviewerScorecards_AdjudicationUpheld verifies that a finding upheld
// on adjudication is counted as held, since it remains a valid finding despite the
// worker's evidence, exactly as docs/features/research-track.md section 8 requires
// ("findings that held up: fixed by the worker, or upheld on adjudication").
func TestGetResearchReviewerScorecards_AdjudicationUpheld(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"does not resolve it"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
	if len(adjTasks) != 1 {
		t.Fatalf("expected exactly one adjudication task, got %d", len(adjTasks))
	}
	submitAdjudication(t, store, ctx, &adjTasks[0], "reject") // upholds the finding

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	if opusCard == nil {
		t.Fatalf("expected opus in scorecards")
	}
	if opusCard.FindingsHeld != 1 {
		t.Errorf("expected opus 1 held (upheld on adjudication), got %d", opusCard.FindingsHeld)
	}
	if opusCard.FindingsWithdrawn != 0 {
		t.Errorf("expected opus 0 withdrawn, got %d", opusCard.FindingsWithdrawn)
	}
	if opusCard.FindingsUnresolved != 0 {
		t.Errorf("expected opus 0 unresolved, got %d", opusCard.FindingsUnresolved)
	}
}

// TestGetResearchReviewerScorecards_MaintainedDisputeLaterFixedIsHeld verifies that a
// disputed finding its reviewer maintains (reports still_open after the dispute) and
// that the worker then fixes, with no adjudication ruling, counts as held rather than
// withdrawn — and so still credits another reviewer's earlier approval as one whose
// blocking finding was subsequently fixed.
func TestGetResearchReviewerScorecards_MaintainedDisputeLaterFixedIsHeld(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "sonnet"})

	opusTask1, sonnetTask1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask1 == nil || sonnetTask1 == nil {
		t.Fatalf("expected both round 1 review tasks")
	}
	submitResearchReview(t, store, ctx, opusTask1, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnetTask1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"already covered elsewhere"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	// Round 2: opus maintains the finding; no adjudicator is configured, so it stays blocking.
	opusTask2, sonnetTask2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusTask2 == nil || sonnetTask2 == nil {
		t.Fatalf("expected both round 2 review tasks")
	}
	submitResearchReview(t, store, ctx, opusTask2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"test.txt","line":1,"summary":"still missing info","in_changed_text":true,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetTask2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	// Round 3: the worker fixes it and opus reports it resolved.
	resubmitResearchImplementTask(t, store, ctx, parentID)
	opusTask3, sonnetTask3 := findResearchReviewTasks(t, store, ctx, projID, parentID, 3)
	if opusTask3 == nil || sonnetTask3 == nil {
		t.Fatalf("expected both round 3 review tasks")
	}
	submitResearchReview(t, store, ctx, opusTask3, "opus-reviewer", "approve", json.RawMessage(`[{"id":"f3","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":false,"status":"resolved","prior_id":"f2"}]`))
	submitResearchReview(t, store, ctx, sonnetTask3, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	scorecards, err := store.GetResearchReviewerScorecards(ctx, projID)
	if err != nil {
		t.Fatalf("failed to get scorecards: %v", err)
	}
	opusCard := findScorecardByModel(scorecards.Scorecards, "opus")
	sonnetCard := findScorecardByModel(scorecards.Scorecards, "sonnet")
	if opusCard == nil || sonnetCard == nil {
		t.Fatalf("expected both reviewers in scorecards")
	}
	if opusCard.FindingsRaised["p2"] != 1 {
		t.Errorf("expected opus raised p2=1, got %d", opusCard.FindingsRaised["p2"])
	}
	if opusCard.FindingsHeld != 1 || opusCard.FindingsWithdrawn != 0 || opusCard.FindingsUnresolved != 0 {
		t.Errorf("expected opus held=1 withdrawn=0 unresolved=0, got held=%d withdrawn=%d unresolved=%d",
			opusCard.FindingsHeld, opusCard.FindingsWithdrawn, opusCard.FindingsUnresolved)
	}
	// Sonnet approved rounds 1 and 2 while opus's blocking finding was open; it was fixed in round 3.
	if sonnetCard.ApprovalsWithLaterFixedBlockingFindings != 2 {
		t.Errorf("expected sonnet 2 approvals with later fixed blocking findings, got %d", sonnetCard.ApprovalsWithLaterFixedBlockingFindings)
	}
}

// TestGetResearchReviewerScorecards_ConcurrentCallsDoNotExhaustReadPool verifies the
// aggregation reads everything through its own read transaction: more concurrent
// callers than readConn's pool size must all complete rather than each holding one
// connection while waiting on a second.
func TestGetResearchReviewerScorecards_ConcurrentCallsDoNotExhaustReadPool(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})

	opusTask, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opusTask == nil {
		t.Fatalf("expected opus review task")
	}
	submitResearchReview(t, store, ctx, opusTask, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"test.txt","line":1,"summary":"missing info","in_changed_text":true,"status":"new"}]`))

	const callers = 16
	timeoutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sc, err := store.GetResearchReviewerScorecards(timeoutCtx, projID)
			if err == nil && len(sc.Scorecards) != 1 {
				err = fmt.Errorf("expected 1 scorecard, got %d", len(sc.Scorecards))
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent GetResearchReviewerScorecards failed: %v", err)
		}
	}
}
