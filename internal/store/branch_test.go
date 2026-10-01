package store

import (
	"context"
	"errors"
	"testing"
)

func newBranchTestStore(t *testing.T) (Store, string, string) {
	t.Helper()
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	proj, err := store.CreateProject(ctx, "Branch Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Branch Doc", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	return store, proj.ID, doc.ID
}

func TestCreateTasksBranch(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Shared one", Spec: "Spec", DocumentID: docID, Model: "haiku", Branch: "event-platform"},
		{Title: "Shared two", Spec: "Spec", DocumentID: docID, Model: "haiku", Branch: "event-platform"},
		{Title: "Own branch", Spec: "Spec", DocumentID: docID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}

	for index, wantBranch := range []string{"event-platform", "event-platform", ""} {
		if tasks[index].Branch != wantBranch {
			t.Errorf("created task %d branch = %q, want %q", index, tasks[index].Branch, wantBranch)
		}
		stored, err := store.GetTask(ctx, tasks[index].ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if stored.Branch != wantBranch {
			t.Errorf("stored task %d branch = %q, want %q", index, stored.Branch, wantBranch)
		}
	}
}

func TestCreateTasksInvalidBranch(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	for _, badBranch := range []string{"Event-Platform", "event platform", "wi/event", "-event", "event-", "event--platform", "../main"} {
		_, err := store.CreateTasks(ctx, projectID, []TaskInput{
			{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", Branch: badBranch},
		})
		var validationErr *ValidationError
		if !errors.As(err, &validationErr) || validationErr.Code != "INVALID_BRANCH" {
			t.Errorf("branch %q: expected INVALID_BRANCH, got %v", badBranch, err)
		}
	}
}

func TestSupersedeKeepsBranch(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Shared", Spec: "Spec", DocumentID: docID, Model: "haiku", Branch: "event-platform"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}

	// Escalation supersedes the task onto a stronger model; the replacement must keep
	// targeting the same shared branch.
	escalatedModel := "sonnet"
	replacement, err := store.SupersedeTask(ctx, tasks[0].ID, &escalatedModel)
	if err != nil {
		t.Fatalf("SupersedeTask: %v", err)
	}
	if replacement.Branch != "event-platform" {
		t.Errorf("replacement branch = %q, want event-platform", replacement.Branch)
	}
}
