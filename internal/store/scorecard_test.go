package store

import (
	"context"
	"encoding/json"
	"testing"
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
