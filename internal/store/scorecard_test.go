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
