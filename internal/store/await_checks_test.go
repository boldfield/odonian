package store

import (
	"context"
	"errors"
	"testing"
)

func TestCreateTasksAwaitChecks(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Gated", Spec: "Spec", DocumentID: docID, Model: "haiku", AwaitChecks: true},
		{Title: "Plain", Spec: "Spec", DocumentID: docID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	for i, want := range []bool{true, false} {
		got, err := store.GetTask(ctx, tasks[i].ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.AwaitChecks != want || got.ChecksStartedAt != nil {
			t.Errorf("task %d: await_checks=%v checks_started_at=%v, want %v nil", i, got.AwaitChecks, got.ChecksStartedAt, want)
		}
	}
	list, err := store.ListTasks(ctx, projectID, TaskListFilter{})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	found := false
	for _, task := range list {
		if task.ID == tasks[0].ID {
			found = true
			if !task.AwaitChecks {
				t.Errorf("listed task await_checks = false, want true")
			}
		}
	}
	if !found {
		t.Errorf("gated task missing from list")
	}
}

func TestCreateTasksAwaitChecksResearchRejected(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	_, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Research", Spec: "Spec", DocumentID: docID, Model: "haiku", Track: "research", AwaitChecks: true},
	})
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("expected validation error, got %v", err)
	}
}
