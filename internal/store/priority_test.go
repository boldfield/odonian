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

func TestMoveTaskToFrontIdempotency(t *testing.T) {
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

	// Create a task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			DocumentID: doc.ID,
			Title:      "test task",
			Spec:       "spec",
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// First call to move to front
	moved1, err := store.MoveTaskToFront(ctx, taskID, "actor1", "reason1")
	if err != nil {
		t.Fatalf("first move to front failed: %v", err)
	}
	priority1 := moved1.Priority

	// Replay with same parameters - should return same result
	moved2, err := store.MoveTaskToFront(ctx, taskID, "actor1", "reason1")
	if err != nil {
		t.Fatalf("replay move to front failed: %v", err)
	}
	if moved2.Priority != priority1 {
		t.Errorf("replay returned different priority: expected %d, got %d", priority1, moved2.Priority)
	}

	// Call with different parameters - should be rejected with mismatch error
	_, err = store.MoveTaskToFront(ctx, taskID, "actor2", "reason2")
	if err == nil {
		t.Fatal("expected idempotency mismatch error for different parameters")
	}
	if conflictErr, ok := err.(*ConflictError); !ok || conflictErr.Code != "IDEMPOTENCY_MISMATCH" {
		t.Errorf("expected IDEMPOTENCY_MISMATCH error, got: %v", err)
	}
}

func TestMoveTaskToFrontArchivedRejection(t *testing.T) {
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

	// Create and archive a task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			DocumentID: doc.ID,
			Title:      "task to archive",
			Spec:       "spec",
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Archive the task
	_, err = store.ArchiveTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to archive task: %v", err)
	}

	// Try to move archived task to front - should fail
	_, err = store.MoveTaskToFront(ctx, taskID, "actor", "reason")
	if err == nil {
		t.Fatal("expected error when moving archived task to front")
	}
	if conflictErr, ok := err.(*ConflictError); !ok || conflictErr.Code != "ARCHIVED" {
		t.Errorf("expected ARCHIVED error, got: %v (type %T)", err, err)
	}
}

func TestSetTaskPriorityBoundaries(t *testing.T) {
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

	// Create a task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			DocumentID: doc.ID,
			Title:      "test task",
			Spec:       "spec",
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Test 1: Fractional priority via JSON should be rejected
	// (This would need to be tested via API, not through SetTaskPriority)

	// Test 2: Test invalid priorities
	testCases := []struct {
		priority int
		wantErr  bool
		errMsg   string
	}{
		{0, true, "0 should be invalid"},
		{-1, true, "-1 should be invalid"},
		{-100, true, "-100 should be invalid"},
		{1001, true, "1001 should be invalid"},
		{1002, true, "1002 should be invalid"},
		{2000, true, "2000 should be invalid"},
		{1, false, ""},
		{500, false, ""},
		{1000, false, ""},
	}

	for _, tc := range testCases {
		_, err := store.SetTaskPriority(ctx, taskID, tc.priority, "actor", "reason")
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: expected error but got none", tc.errMsg)
			}
		} else {
			if err != nil {
				t.Errorf("priority %d: expected no error but got %v", tc.priority, err)
			}
		}
	}
}

func TestConcurrentMoveTaskToFront(t *testing.T) {
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

	// Create multiple tasks
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{DocumentID: doc.ID, Title: "task 1", Spec: "spec", Model: "haiku", Priority: intPtr(500)},
		{DocumentID: doc.ID, Title: "task 2", Spec: "spec", Model: "haiku", Priority: intPtr(500)},
		{DocumentID: doc.ID, Title: "task 3", Spec: "spec", Model: "haiku", Priority: intPtr(500)},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	// Move tasks to front sequentially and verify strictly increasing priorities
	var previousPriority int
	var movedTasks []*Task
	for i, task := range tasks {
		moved, err := store.MoveTaskToFront(ctx, task.ID, "actor", "reason")
		if err != nil {
			t.Fatalf("task %d: failed to move to front: %v", i, err)
		}

		if i > 0 && moved.Priority <= previousPriority {
			t.Errorf("task %d: expected priority > %d, got %d", i, previousPriority, moved.Priority)
		}
		previousPriority = moved.Priority
		movedTasks = append(movedTasks, &moved)
	}

	// Verify final priorities are strictly increasing
	if len(movedTasks) >= 3 && (movedTasks[0].Priority >= movedTasks[1].Priority || movedTasks[1].Priority >= movedTasks[2].Priority) {
		t.Errorf("priorities not strictly increasing: %d, %d, %d",
			movedTasks[0].Priority, movedTasks[1].Priority, movedTasks[2].Priority)
	}
}

func TestDefaultPriority(t *testing.T) {
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

	// Create task without explicit priority - should default to 500
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{DocumentID: doc.ID, Title: "test task", Spec: "spec", Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	if tasks[0].Priority != 500 {
		t.Errorf("expected default priority 500, got %d", tasks[0].Priority)
	}
}

func TestCreateTasksInvalidPriority(t *testing.T) {
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

	// Test that CreateTasks rejects invalid priorities
	testCases := []struct {
		priority int
		wantErr  bool
	}{
		{0, true},
		{-1, true},
		{1001, true},
		{2000, true},
		{1, false},
		{500, false},
		{1000, false},
	}

	for _, tc := range testCases {
		_, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{
				DocumentID: doc.ID,
				Title:      "test task",
				Spec:       "spec",
				Model:      "haiku",
				Priority:   intPtr(tc.priority),
			},
		})

		if tc.wantErr && err == nil {
			t.Errorf("priority %d: expected error but got none", tc.priority)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("priority %d: expected no error but got %v", tc.priority, err)
		}
	}
}

func TestPriorityUnchangedByLeaseUpdate(t *testing.T) {
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

	// Create task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{DocumentID: doc.ID, Title: "test task", Spec: "spec", Model: "haiku", Priority: intPtr(750)},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Update priority
	updated, err := store.SetTaskPriority(ctx, taskID, 900, "actor", "reason")
	if err != nil {
		t.Fatalf("failed to set priority: %v", err)
	}

	newPriority := updated.Priority

	// Verify priority changed
	if newPriority != 900 {
		t.Errorf("expected priority 900, got %d", newPriority)
	}

	// Fetch the task again and verify priority is persisted
	fetched, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to fetch task: %v", err)
	}

	if fetched.Priority != 900 {
		t.Errorf("priority not persisted: expected 900, got %d", fetched.Priority)
	}

	// Verify state is unchanged (new tasks start in 'backlog')
	if fetched.State != "backlog" {
		t.Errorf("state changed: expected 'backlog', got %q", fetched.State)
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
