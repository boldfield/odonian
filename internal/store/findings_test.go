package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestFindingsValidation_ValidSubmission tests that a valid findings array passes validation.
func TestFindingsValidation_ValidSubmission(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	implementTaskID := tasks[0].ID

	// Claim and submit the implement task
	if _, err := store.PromoteTask(ctx, implementTaskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	if _, err := store.ClaimTask(ctx, implementTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	_, err = store.SubmitTask(ctx, implementTaskID, "agent-1", "Implementation complete", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, nil, 5, nil)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find the spawned review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == implementTaskID && allTasks[i].State == "ready" {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Claim the review task and submit with valid findings
	if _, err := store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	priorID := "finding-0"
	inChangedTrue := true
	inChangedFalse := false
	findings := []Finding{
		{
			ID:            "finding-1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Missing error handling",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
		{
			ID:            "finding-2",
			Severity:      "P3",
			File:          "src/main.go",
			Line:          50,
			Summary:       "Typo in variable name",
			InChangedText: &inChangedFalse,
			Status:        "still_open",
			PriorID:       &priorID,
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Approved with minor notes", &approve, []LinkInput{}, &findings, 5, nil)
	if err != nil {
		t.Fatalf("failed to submit review task with findings: %v", err)
	}

	// Read back the events and verify findings are present and correctly unmarshaled
	events, err := store.ListEvents(ctx, implementTaskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	// Find the review event
	var reviewEvent *Event
	for i := range events {
		if events[i].Kind == "review" {
			reviewEvent = &events[i]
			break
		}
	}
	if reviewEvent == nil {
		t.Fatalf("review event not found")
	}

	if reviewEvent.Findings == nil {
		t.Fatalf("expected findings in review event, got nil")
	}

	if len(*reviewEvent.Findings) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(*reviewEvent.Findings))
	}

	f1 := (*reviewEvent.Findings)[0]
	if f1.ID != "finding-1" || f1.Severity != "P2" || f1.File != "src/main.go" || f1.Line != 42 {
		t.Errorf("first finding not correctly unmarshaled: %+v", f1)
	}

	f2 := (*reviewEvent.Findings)[1]
	if f2.ID != "finding-2" || f2.Status != "still_open" || f2.PriorID == nil || *f2.PriorID != "finding-0" {
		t.Errorf("second finding not correctly unmarshaled: %+v", f2)
	}
}

// TestFindingsValidation_InvalidID tests rejection for empty finding ID.
func TestFindingsValidation_InvalidID(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "", // Empty ID should fail
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for empty finding ID, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_InvalidSeverity tests rejection for invalid severity.
func TestFindingsValidation_InvalidSeverity(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P4", // Invalid severity
			File:          "src/main.go",
			Line:          42,
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for invalid severity, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_EmptyFile tests rejection for empty file path.
func TestFindingsValidation_EmptyFile(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "", // Empty file
			Line:          42,
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for empty file, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_NonPositiveLine tests rejection for non-positive line number.
func TestFindingsValidation_NonPositiveLine(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          0, // Non-positive
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for non-positive line, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_EmptySummary tests rejection for empty summary.
func TestFindingsValidation_EmptySummary(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "", // Empty summary
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for empty summary, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_InvalidStatus tests rejection for invalid status.
func TestFindingsValidation_InvalidStatus(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "invalid", // Invalid status
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for invalid status, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_MissingPriorID tests rejection when prior_id is required but missing.
func TestFindingsValidation_MissingPriorID(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "still_open", // Requires prior_id
			PriorID:       nil,          // Missing
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for missing prior_id, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_UnexpectedPriorID tests rejection when prior_id is present but shouldn't be.
func TestFindingsValidation_UnexpectedPriorID(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	priorID := "f0"
	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "new",    // Doesn't allow prior_id
			PriorID:       &priorID, // But provided anyway
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for unexpected prior_id, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_FindingsNotAllowedOnImplement tests rejection when findings provided on implement task.
func TestFindingsValidation_FindingsNotAllowedOnImplement(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	implementTaskID := tasks[0].ID

	if _, err := store.PromoteTask(ctx, implementTaskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	if _, err := store.ClaimTask(ctx, implementTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Some issue",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
	}

	// Try to submit with findings on implement task (should fail)
	_, err = store.SubmitTask(ctx, implementTaskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for findings on implement task, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "FINDINGS_NOT_ALLOWED" {
		t.Errorf("expected code FINDINGS_NOT_ALLOWED, got %s", valErr.Code)
	}
}

// TestFindingsValidation_UnchangedSubmissionWithoutFindings tests that submissions without findings work correctly.
func TestFindingsValidation_UnchangedSubmissionWithoutFindings(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	implementTaskID := tasks[0].ID

	if _, err := store.PromoteTask(ctx, implementTaskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	if _, err := store.ClaimTask(ctx, implementTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	if _, err := store.SubmitTask(ctx, implementTaskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, nil, 5, nil); err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == implementTaskID && allTasks[i].State == "ready" {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	if _, err := store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	// Submit WITHOUT findings (should work)
	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, nil, 5, nil)
	if err != nil {
		t.Fatalf("failed to submit review without findings: %v", err)
	}

	// Verify event exists with no findings
	events, err := store.ListEvents(ctx, implementTaskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var reviewEvent *Event
	for i := range events {
		if events[i].Kind == "review" {
			reviewEvent = &events[i]
			break
		}
	}
	if reviewEvent == nil {
		t.Fatalf("review event not found")
	}

	if reviewEvent.Findings != nil {
		t.Errorf("expected no findings, got %v", reviewEvent.Findings)
	}
}

// TestFindingsValidation_MissingInChangedText tests rejection when in_changed_text is missing.
func TestFindingsValidation_MissingInChangedText(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Some issue",
			InChangedText: nil, // Missing required field
			Status:        "new",
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for missing in_changed_text, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
}

// TestFindingsValidation_DuplicateID tests rejection for duplicate finding IDs.
func TestFindingsValidation_DuplicateID(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	_, _, reviewTaskID := setupFindingsTest(t, store, ctx)

	inChangedTrue := true
	findings := []Finding{
		{
			ID:            "f1",
			Severity:      "P2",
			File:          "src/main.go",
			Line:          42,
			Summary:       "Issue 1",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
		{
			ID:            "f1", // Duplicate ID
			Severity:      "P2",
			File:          "src/main.go",
			Line:          50,
			Summary:       "Issue 2",
			InChangedText: &inChangedTrue,
			Status:        "new",
		},
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Review", &approve, []LinkInput{}, &findings, 5, nil)
	if err == nil {
		t.Fatalf("expected error for duplicate ID, got nil")
	}

	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	if valErr.Code != "INVALID_FINDINGS" {
		t.Errorf("expected code INVALID_FINDINGS, got %s", valErr.Code)
	}
	// Verify the error message contains the index
	if !strings.Contains(valErr.Message, "findings[1]") {
		t.Errorf("expected error message to contain findings[1], got: %s", valErr.Message)
	}
}

// setupFindingsTest creates a project, document, implement task, and ready review task for testing findings.
func setupFindingsTest(t *testing.T, store Store, ctx context.Context) (string, string, string) {
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	implementTaskID := tasks[0].ID

	if _, err := store.PromoteTask(ctx, implementTaskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	if _, err := store.ClaimTask(ctx, implementTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	if _, err := store.SubmitTask(ctx, implementTaskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, nil, 5, nil); err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == implementTaskID && allTasks[i].State == "ready" {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	if _, err := store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	return proj.ID, doc.ID, reviewTask.ID
}
