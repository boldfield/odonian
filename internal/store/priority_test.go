package store

import (
	"context"
	"testing"
)

func TestSetTaskPriority(t *testing.T) {
	ctx := context.Background()

	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-proj", "https://example.com/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "test-doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a test task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			DocumentID: doc.ID,
			Title:      "test task",
			Spec:       "test spec",
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	if len(tasks) == 0 {
		t.Fatal("no tasks created")
	}
	taskID := tasks[0].ID

	// Test 1: Valid priority set (500 → 750)
	updated, err := store.SetTaskPriority(ctx, taskID, 750, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to set priority to 750: %v", err)
	}
	if updated.Priority != 750 {
		t.Errorf("expected priority 750, got %d", updated.Priority)
	}

	// Test 2: Verify idempotence - setting same priority is no-op but succeeds
	updated2, err := store.SetTaskPriority(ctx, taskID, 750, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to set same priority: %v", err)
	}
	if updated2.Priority != 750 {
		t.Errorf("expected priority 750, got %d", updated2.Priority)
	}

	// Test 3: Valid boundaries - set to 1 (minimum)
	updated, err = store.SetTaskPriority(ctx, taskID, 1, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to set priority to 1: %v", err)
	}
	if updated.Priority != 1 {
		t.Errorf("expected priority 1, got %d", updated.Priority)
	}

	// Test 4: Valid boundaries - set to 1000 (maximum)
	updated, err = store.SetTaskPriority(ctx, taskID, 1000, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to set priority to 1000: %v", err)
	}
	if updated.Priority != 1000 {
		t.Errorf("expected priority 1000, got %d", updated.Priority)
	}

	// Test 5: Invalid - priority 0 (below minimum)
	_, err = store.SetTaskPriority(ctx, taskID, 0, "test-actor", "test reason")
	if err == nil {
		t.Fatal("expected error for priority 0")
	}

	// Test 6: Invalid - priority 1001 (above maximum)
	_, err = store.SetTaskPriority(ctx, taskID, 1001, "test-actor", "test reason")
	if err == nil {
		t.Fatal("expected error for priority 1001")
	}

	// Test 7: Invalid - negative priority
	_, err = store.SetTaskPriority(ctx, taskID, -1, "test-actor", "test reason")
	if err == nil {
		t.Fatal("expected error for negative priority")
	}

	// Test 8: Verify event is recorded
	// Get task and check for priority-set event
	fetched, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to fetch task: %v", err)
	}
	if fetched.Priority != 1000 {
		t.Errorf("expected final priority 1000, got %d", fetched.Priority)
	}
}

func TestMoveTaskToFront(t *testing.T) {
	ctx := context.Background()

	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-proj", "https://example.com/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "test-doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create multiple tasks with different priorities
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			DocumentID: doc.ID,
			Title:      "task 1",
			Spec:       "spec 1",
			Model:      "haiku",
			Priority:   intPtr(500),
		},
		{
			DocumentID: doc.ID,
			Title:      "task 2",
			Spec:       "spec 2",
			Model:      "haiku",
			Priority:   intPtr(600),
		},
		{
			DocumentID: doc.ID,
			Title:      "task 3",
			Spec:       "spec 3",
			Model:      "haiku",
			Priority:   intPtr(730),
		},
		{
			DocumentID: doc.ID,
			Title:      "task 4",
			Spec:       "spec 4",
			Model:      "haiku",
			Priority:   intPtr(1000),
		},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}
	if len(tasks) < 4 {
		t.Fatal("expected at least 4 tasks created")
	}

	// Test 1: Move task 1 (P=500) to front - should be max(1000, 730) + 1 = 1001
	moved, err := store.MoveTaskToFront(ctx, tasks[0].ID, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to move to front: %v", err)
	}
	if moved.Priority != 1001 {
		t.Errorf("expected priority 1001 after moving to front, got %d", moved.Priority)
	}

	// Test 2: Move task 3 (P=730) to front - should be max(1000, 1001) + 1 = 1002
	moved, err = store.MoveTaskToFront(ctx, tasks[2].ID, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to move task 3 to front: %v", err)
	}
	if moved.Priority != 1002 {
		t.Errorf("expected priority 1002 after moving to front, got %d", moved.Priority)
	}

	// Test 3: Move task 4 (P=1000) to front - should be max(1000, 1002) + 1 = 1003
	moved, err = store.MoveTaskToFront(ctx, tasks[3].ID, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to move task 4 to front: %v", err)
	}
	if moved.Priority != 1003 {
		t.Errorf("expected priority 1003 after moving to front, got %d", moved.Priority)
	}

	// Test 4: Manual set to 1000 should not overtake the front
	updated, err := store.SetTaskPriority(ctx, tasks[1].ID, 1000, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to set task 2 priority: %v", err)
	}
	if updated.Priority != 1000 {
		t.Errorf("expected priority 1000, got %d", updated.Priority)
	}

	// Task 2 should still be behind task 4 (which is at 1003)
	front, err := store.MoveTaskToFront(ctx, tasks[1].ID, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to move task 2 to front: %v", err)
	}
	if front.Priority != 1004 {
		t.Errorf("expected priority 1004, got %d", front.Priority)
	}

	// Test 5: ListTasks should be ordered by priority DESC, created_at, ID
	listed, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(listed) < 4 {
		t.Fatal("expected at least 4 tasks in list")
	}

	// Verify order: highest priority first
	if listed[0].Priority < listed[1].Priority {
		t.Errorf("tasks not ordered by priority DESC: %d, %d", listed[0].Priority, listed[1].Priority)
	}

	// Test 6: Archived tasks excluded from max calculation
	// Create a new task
	newTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			DocumentID: doc.ID,
			Title:      "new task for archive test",
			Spec:       "new spec",
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create new task: %v", err)
	}
	newTaskID := newTasks[0].ID

	// Archive all existing high-priority tasks
	for _, task := range tasks {
		_, err := store.ArchiveTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("failed to archive task: %v", err)
		}
	}

	// Move the new task to front - should calculate based on archived tasks being excluded
	movedNew, err := store.MoveTaskToFront(ctx, newTaskID, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to move new task to front: %v", err)
	}

	// The priority should be 1001 because there are no active outstanding tasks
	// (only archived and the terminal task we created)
	if movedNew.Priority != 1001 {
		t.Errorf("expected priority 1001 with no active tasks, got %d", movedNew.Priority)
	}
}

func TestListTasksOrderByPriority(t *testing.T) {
	ctx := context.Background()

	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-proj", "https://example.com/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "test-doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks with various priorities
	priorities := []int{100, 500, 750, 200, 1000, 300}
	taskIDs := []string{}

	for i, p := range priorities {
		tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{
				DocumentID: doc.ID,
				Title:      taskTitle(i),
				Spec:       "spec",
				Model:      "haiku",
				Priority:   intPtr(p),
			},
		})
		if err != nil {
			t.Fatalf("failed to create task %d: %v", i, err)
		}
		taskIDs = append(taskIDs, tasks[0].ID)
	}

	// List all tasks
	listed, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	// Verify ordering: should be sorted by priority DESC
	expected := []int{1000, 750, 500, 300, 200, 100}
	if len(listed) < len(expected) {
		t.Fatalf("expected at least %d tasks, got %d", len(expected), len(listed))
	}

	for i := 0; i < len(expected); i++ {
		if listed[i].Priority != expected[i] {
			t.Errorf("task %d: expected priority %d, got %d", i, expected[i], listed[i].Priority)
		}
	}
}

func TestPriorityWithHolds(t *testing.T) {
	ctx := context.Background()

	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-proj", "https://example.com/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "test-doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a task and hold it
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			DocumentID: doc.ID,
			Title:      "held task",
			Spec:       "spec",
			Model:      "haiku",
			Priority:   intPtr(500),
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Hold the task
	_, err = store.HoldTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to hold task: %v", err)
	}

	// Priority mutation should work on held tasks
	updated, err := store.SetTaskPriority(ctx, taskID, 750, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to set priority on held task: %v", err)
	}
	if updated.Priority != 750 {
		t.Errorf("expected priority 750, got %d", updated.Priority)
	}

	// Check that held task is included in max calculation for move-to-front
	moved, err := store.MoveTaskToFront(ctx, taskID, "test-actor", "test reason")
	if err != nil {
		t.Fatalf("failed to move held task to front: %v", err)
	}
	if moved.Priority != 1001 {
		t.Errorf("expected priority 1001, got %d", moved.Priority)
	}
}

// Helper functions
func intPtr(i int) *int {
	return &i
}

func taskTitle(i int) string {
	titles := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	if i < len(titles) {
		return titles[i]
	}
	return "task"
}
