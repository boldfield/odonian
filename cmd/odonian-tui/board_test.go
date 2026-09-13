package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
	tea "github.com/charmbracelet/bubbletea"
)

// TestBoardModel_RenderLayout tests that the board renders with all five columns and counts.
func TestBoardModel_RenderLayout(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-1", Title: "Task 1", State: "backlog"},
			{ID: "task-2", Title: "Task 2", State: "backlog"},
			{ID: "task-3", Title: "Task 3", State: "in_progress"},
			{ID: "task-4", Title: "Task 4", State: "done"},
			{ID: "task-5", Title: "Task 5", State: "done"},
			{ID: "task-6", Title: "Task 6", State: "done"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate the task fetch by constructing the bucketed tasks
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{{ID: "task-1", Title: "Task 1", State: "backlog"}, {ID: "task-2", Title: "Task 2", State: "backlog"}}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "task-3", Title: "Task 3", State: "in_progress"}}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{{ID: "task-4", Title: "Task 4", State: "done"}, {ID: "task-5", Title: "Task 5", State: "done"}, {ID: "task-6", Title: "Task 6", State: "done"}}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	output := model.View()

	// Verify the tabs are present with correct counts
	if !strings.Contains(output, "backlog(2)") {
		t.Errorf("Expected 'backlog(2)' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "ready(0)") {
		t.Errorf("Expected 'ready(0)' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "in_progress(1)") {
		t.Errorf("Expected 'in_progress(1)' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "review(0)") {
		t.Errorf("Expected 'review(0)' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "done(3)") {
		t.Errorf("Expected 'done(3)' in output, got:\n%s", output)
	}
}

// TestBoardModel_Navigation tests column and selection navigation.
func TestBoardModel_Navigation(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-1", Title: "Task 1", State: "backlog"},
			{ID: "task-2", Title: "Task 2", State: "ready"},
			{ID: "task-3", Title: "Task 3", State: "in_progress"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate the task fetch
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{{ID: "task-1", Title: "Task 1", State: "backlog"}}
	bucketed["ready"] = []tuiclient.Task{{ID: "task-2", Title: "Task 2", State: "ready"}}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "task-3", Title: "Task 3", State: "in_progress"}}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Initially in in_progress column (index 2)
	initialOutput := model.View()
	if !strings.Contains(initialOutput, "in_progress(1)") {
		t.Errorf("Expected active column to show in_progress(1), got:\n%s", initialOutput)
	}

	// Move left to ready
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)
	output := model.View()
	if !strings.Contains(output, "ready(1)") {
		t.Errorf("Expected active column to be ready after left, got:\n%s", output)
	}

	// Move left to backlog
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)
	output = model.View()
	if !strings.Contains(output, "backlog(1)") {
		t.Errorf("Expected active column to be backlog after left, got:\n%s", output)
	}
}

// TestBoardModel_SelectionStability tests that selection is preserved across refreshes.
func TestBoardModel_SelectionStability(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate first fetch
	bucketed1 := make(map[string][]tuiclient.Task)
	bucketed1["backlog"] = []tuiclient.Task{}
	bucketed1["ready"] = []tuiclient.Task{}
	bucketed1["in_progress"] = []tuiclient.Task{
		{ID: "task-1", Title: "Task 1", State: "in_progress"},
		{ID: "task-2", Title: "Task 2", State: "in_progress"},
	}
	bucketed1["review"] = []tuiclient.Task{}
	bucketed1["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed1})
	model = m.(*BoardModel)

	// Verify task-1 is selected
	if model.selectedTaskID != "task-1" {
		t.Errorf("Expected initial selection to be task-1, got %s", model.selectedTaskID)
	}

	// Move down to select task-2
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m.(*BoardModel)
	if model.selectedTaskID != "task-2" {
		t.Errorf("Expected selection to be task-2 after down, got %s", model.selectedTaskID)
	}

	// Simulate a refresh with reordered tasks
	bucketed2 := make(map[string][]tuiclient.Task)
	bucketed2["backlog"] = []tuiclient.Task{}
	bucketed2["ready"] = []tuiclient.Task{}
	bucketed2["in_progress"] = []tuiclient.Task{
		{ID: "task-2", Title: "Task 2", State: "in_progress"},
		{ID: "task-1", Title: "Task 1", State: "in_progress"},
	}
	bucketed2["review"] = []tuiclient.Task{}
	bucketed2["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed2})
	model = m.(*BoardModel)

	// Verify task-2 is still selected
	if model.selectedTaskID != "task-2" {
		t.Errorf("Selection not preserved: was task-2, became %s", model.selectedTaskID)
	}
}

// TestBoardModel_ErrorState tests that errors are rendered correctly.
func TestBoardModel_ErrorState(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// First set a successful state
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "review", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}
	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Now simulate an error
	m, _ = model.Update(tasksFetchedMsg{err: errors.New("test error message")})
	model = m.(*BoardModel)

	output := model.View()
	if !strings.Contains(output, "Error:") || !strings.Contains(output, "test error message") {
		t.Errorf("Expected error message in output, got:\n%s", output)
	}

	if !strings.Contains(output, "Press 'r' to retry") {
		t.Errorf("Expected retry hint in output, got:\n%s", output)
	}
}

// TestBoardModel_EmptyColumn tests that empty columns show the empty message.
func TestBoardModel_EmptyColumn(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-1", Title: "Task 1", State: "backlog"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate fetch
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{{ID: "task-1", Title: "Task 1", State: "backlog"}}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate from in_progress (index 2) to ready (index 1): one left
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)

	output := model.View()
	if !strings.Contains(output, "(empty)") {
		t.Errorf("Expected '(empty)' message in output, got:\n%s", output)
	}
}

// TestBoardModel_InProgressCardDetails tests that in_progress cards show assignee and lease info.
func TestBoardModel_InProgressCardDetails(t *testing.T) {
	expiresAt := time.Now().Add(5 * time.Minute).Format(time.RFC3339Nano)
	assignee := "alice"

	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate fetch
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{{
		ID:             "task-1",
		Title:          "Task 1",
		State:          "in_progress",
		Assignee:       &assignee,
		LeaseExpiresAt: &expiresAt,
	}}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	output := model.View()
	if !strings.Contains(output, "alice") {
		t.Errorf("Expected assignee 'alice' in output, got:\n%s", output)
	}
	if !strings.Contains(output, "lease") {
		t.Errorf("Expected lease info in output, got:\n%s", output)
	}
}

// TestBoardModel_AssigneeOnReviewApprovedDone tests that assignee is shown on review, approved, and done cards.
func TestBoardModel_AssigneeOnReviewApprovedDone(t *testing.T) {
	assigneeReview := "alice"
	assigneeApproved := "bob"
	assigneeDone := "charlie"

	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate fetch with assignees on review, approved, and done tasks
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{{
		ID:       "task-review",
		Title:    "Review Task",
		State:    "review",
		Assignee: &assigneeReview,
	}}
	bucketed["approved"] = []tuiclient.Task{{
		ID:       "task-approved",
		Title:    "Approved Task",
		State:    "approved",
		Assignee: &assigneeApproved,
	}}
	bucketed["done"] = []tuiclient.Task{{
		ID:       "task-done",
		Title:    "Done Task",
		State:    "done",
		Assignee: &assigneeDone,
	}}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to review column
	for model.selectedColumn < 3 {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model = m.(*BoardModel)
	}

	output := model.View()
	if !strings.Contains(output, "alice") {
		t.Errorf("Expected assignee 'alice' in review column output, got:\n%s", output)
	}

	// Navigate to approved column
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = m.(*BoardModel)

	output = model.View()
	if !strings.Contains(output, "bob") {
		t.Errorf("Expected assignee 'bob' in approved column output, got:\n%s", output)
	}

	// Navigate to done column
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = m.(*BoardModel)

	output = model.View()
	if !strings.Contains(output, "charlie") {
		t.Errorf("Expected assignee 'charlie' in done column output, got:\n%s", output)
	}
}

// TestBoardModel_NoAssigneeShown tests that tasks without assignee don't show anything.
func TestBoardModel_NoAssigneeShown(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate fetch with tasks that have no assignee
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{{
		ID:       "task-no-assignee",
		Title:    "No Assignee Task",
		State:    "in_progress",
		Assignee: nil,
	}}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	output := model.View()
	// Should not contain "null" or "(unassigned)" or "@"
	if strings.Contains(output, "null") {
		t.Errorf("Expected no 'null' in output for unassigned task, got:\n%s", output)
	}
	if strings.Contains(output, "(unassigned)") {
		t.Errorf("Expected no '(unassigned)' in output for unassigned task, got:\n%s", output)
	}
	if strings.Contains(output, "@") {
		t.Errorf("Expected no '@' in output for unassigned task, got:\n%s", output)
	}
	// Task title should still be there
	if !strings.Contains(output, "No Assignee Task") {
		t.Errorf("Expected task title in output, got:\n%s", output)
	}
}

// TestBoardModel_DisappearedTaskSelection tests that when the selected task disappears,
// the cursor lands on the positionally-nearest remaining task, not the first.
func TestBoardModel_DisappearedTaskSelection(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate first fetch with three tasks
	bucketed1 := make(map[string][]tuiclient.Task)
	bucketed1["backlog"] = []tuiclient.Task{}
	bucketed1["ready"] = []tuiclient.Task{}
	bucketed1["in_progress"] = []tuiclient.Task{
		{ID: "task-1", Title: "Task 1", State: "in_progress"},
		{ID: "task-2", Title: "Task 2", State: "in_progress"},
		{ID: "task-3", Title: "Task 3", State: "in_progress"},
	}
	bucketed1["review"] = []tuiclient.Task{}
	bucketed1["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed1})
	model = m.(*BoardModel)

	// Select task-2 (index 1)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m.(*BoardModel)
	if model.selectedTaskID != "task-2" {
		t.Errorf("Expected selection to be task-2, got %s", model.selectedTaskID)
	}

	// Simulate refresh where task-2 is REMOVED (only task-1 and task-3 remain)
	bucketed2 := make(map[string][]tuiclient.Task)
	bucketed2["backlog"] = []tuiclient.Task{}
	bucketed2["ready"] = []tuiclient.Task{}
	bucketed2["in_progress"] = []tuiclient.Task{
		{ID: "task-1", Title: "Task 1", State: "in_progress"},
		{ID: "task-3", Title: "Task 3", State: "in_progress"},
	}
	bucketed2["review"] = []tuiclient.Task{}
	bucketed2["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed2})
	model = m.(*BoardModel)

	// Should have selected task-3 (at clamped index 1, which is now the second task)
	// not task-1 (the first task).
	if model.selectedTaskID != "task-3" {
		t.Errorf("Expected selection to move to task-3 (nearest), got %s", model.selectedTaskID)
	}
	if model.selectedIndex != 1 {
		t.Errorf("Expected selectedIndex to be 1, got %d", model.selectedIndex)
	}
}

// tickSentinel is returned by the test tick command to identify that a tick was armed.
type tickSentinel struct{}

// countTickCmds installs a counting tick function on the model and returns a pointer to the counter.
// Each time the model arms a new tick (via newTickCmd), the counter increments by 1
// and returns a tea.Cmd that immediately sends tickSentinel{} when executed.
func countTickCmds(model *BoardModel) *int {
	count := 0
	model.newTickCmd = func() tea.Cmd {
		count++
		return func() tea.Msg { return tickSentinel{} }
	}
	return &count
}

// runCmd executes a tea.Cmd synchronously and collects all leaf messages it produces.
// tea.Batch returns a BatchMsg ([]tea.Cmd); this function recursively executes those sub-cmds
// and collects their results, so callers can inspect the full set of messages produced.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}

	resultChan := make(chan tea.Msg, 64)
	var execCmd func(tea.Cmd)
	execCmd = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			msg := c()
			if msg == nil {
				resultChan <- nil
				return
			}
			// tea.Batch returns a BatchMsg; recurse into each sub-cmd.
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, subCmd := range batch {
					execCmd(subCmd)
				}
			} else {
				resultChan <- msg
			}
		}()
	}

	execCmd(cmd)

	// Collect results with a timeout to handle the async tick sentinel.
	var messages []tea.Msg
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()

	// We need to know how many goroutines to expect. Since we don't track that precisely,
	// use a small fixed wait after the first message arrives.
	firstArrived := false
	drainTimer := time.NewTimer(24 * time.Hour) // starts stopped effectively
	defer drainTimer.Stop()

	for {
		select {
		case msg := <-resultChan:
			if msg != nil {
				messages = append(messages, msg)
			}
			if !firstArrived {
				firstArrived = true
				drainTimer.Reset(50 * time.Millisecond)
			}
		case <-drainTimer.C:
			return messages
		case <-timer.C:
			return messages
		}
	}
}

// extractStateFromOptions extracts the State field from TaskListOptions if set.
func extractStateFromOptions(options ...tuiclient.TaskListOption) string {
	opts := &tuiclient.TaskListOptions{}
	for _, opt := range options {
		opt(opts)
	}
	return opts.State
}

// filterTasksByState filters a list of tasks to only those matching the given state.
// If state is empty, returns all tasks.
func filterTasksByState(tasks []tuiclient.Task, state string) []tuiclient.Task {
	if state == "" {
		return tasks
	}
	var filtered []tuiclient.Task
	for _, t := range tasks {
		if t.State == state {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

// TestBoardModel_PromoteTask tests promoting a backlog task to ready.
func TestBoardModel_PromoteTask(t *testing.T) {
	promoteWasCalled := false
	var promoteTaskID string

	allTasks := []tuiclient.Task{
		{ID: "task-1", Title: "Task 1", State: "ready"},
		{ID: "task-2", Title: "Task 2", State: "backlog"},
	}

	mockClient := &tuiclient.MockClient{
		PromoteTaskFunc: func(ctx context.Context, id string) error {
			promoteWasCalled = true
			promoteTaskID = id
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			// On refetch after promotion, task-1 should be in ready
			state := extractStateFromOptions(options...)
			return filterTasksByState(allTasks, state), nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Set up initial backlog with two tasks
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{
		{ID: "task-1", Title: "Task 1", State: "backlog"},
		{ID: "task-2", Title: "Task 2", State: "backlog"},
	}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Move to backlog column (it starts at in_progress)
	for i := 0; i < 2; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
		model = m.(*BoardModel)
	}

	// Verify we're in backlog and task-1 is selected
	if model.selectedColumn != 0 {
		t.Errorf("Expected selectedColumn 0 (backlog), got %d", model.selectedColumn)
	}
	if model.selectedTaskID != "task-1" {
		t.Errorf("Expected selected task task-1, got %s", model.selectedTaskID)
	}

	// Press 'p' to promote
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	model = m.(*BoardModel)

	// Execute the promote command
	if cmd != nil {
		msg := cmd()
		m, _ = model.Update(msg)
		model = m.(*BoardModel)
	}

	// Verify promote was called on task-1
	if !promoteWasCalled {
		t.Errorf("Expected PromoteTask to be called")
	}
	if promoteTaskID != "task-1" {
		t.Errorf("Expected PromoteTask called on task-1, got %s", promoteTaskID)
	}

	// After refetch, task-1 should now be in ready column
	// and the board should show updated state
	output := model.View()
	if !strings.Contains(output, "ready(1)") {
		t.Errorf("Expected 'ready(1)' after promotion, got:\n%s", output)
	}
}

// promoteWithError is a helper that runs a promote action against a model
// already positioned in the backlog column with a task selected.
// It returns the model after the promote command has been executed.
func promoteWithError(t *testing.T, returnedErr error) *BoardModel {
	t.Helper()

	mockClient := &tuiclient.MockClient{
		PromoteTaskFunc: func(ctx context.Context, id string) error {
			return returnedErr
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{
		{ID: "task-1", Title: "Task 1", State: "backlog"},
	}
	for _, state := range []string{"ready", "in_progress", "review", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate from in_progress (index 2) to backlog (index 0): two lefts
	for i := 0; i < 2; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
		model = m.(*BoardModel)
	}

	// Press 'p' to promote
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	model = m.(*BoardModel)

	if cmd != nil {
		msg := cmd()
		m, _ = model.Update(msg)
		model = m.(*BoardModel)
	}

	return model
}

// TestBoardModel_PromoteTask409 verifies that a real *tuiclient.APIError with StatusCode 409
// produces the friendly "not in backlog (already moved?)" message, not the generic branch.
// This test exercises the typed-error detection path (errors.As + StatusCode check), so it
// WILL FAIL if the StatusCode detection is broken (e.g. if someone reverts to strings.Contains).
func TestBoardModel_PromoteTask409(t *testing.T) {
	// Use the exact error the real HTTPClient returns for a server 409 + structured body.
	conflictErr := &tuiclient.APIError{
		StatusCode: 409,
		Code:       "CONFLICT",
		Message:    "Task is not in backlog",
	}

	model := promoteWithError(t, conflictErr)

	// The FRIENDLY branch must be taken — not just "not in backlog" from the generic path.
	// The generic path would produce "promote failed: server error (CONFLICT): Task is not in backlog".
	// The friendly path produces exactly "not in backlog (already moved?)".
	const friendlyMsg = "not in backlog (already moved?)"
	if model.error != friendlyMsg {
		t.Errorf("Expected friendly 409 message %q, got: %q", friendlyMsg, model.error)
	}

	// Verify we didn't crash and the model is still usable
	output := model.View()
	if !strings.Contains(output, "backlog(1)") {
		t.Errorf("Expected board to still be functional, got:\n%s", output)
	}
}

// TestBoardModel_PromoteTask500 verifies that a non-409 APIError (e.g. 500) takes the
// generic branch and does NOT produce the friendly conflict message.
func TestBoardModel_PromoteTask500(t *testing.T) {
	serverErr := &tuiclient.APIError{
		StatusCode: 500,
		Code:       "INTERNAL_ERROR",
		Message:    "something went wrong",
	}

	model := promoteWithError(t, serverErr)

	// Must show the generic error, not the friendly conflict message.
	if model.error == "not in backlog (already moved?)" {
		t.Errorf("Expected generic error for 500, but got the friendly 409 message")
	}
	if !strings.Contains(model.error, "promote failed:") {
		t.Errorf("Expected 'promote failed:' prefix for generic error, got: %q", model.error)
	}
	if !strings.Contains(model.error, "something went wrong") {
		t.Errorf("Expected server message in generic error, got: %q", model.error)
	}
}

// TestBoardModel_PromoteOnNonBacklogIsNoOp tests that pressing 'p' on non-backlog column does nothing.
func TestBoardModel_PromoteOnNonBacklogIsNoOp(t *testing.T) {
	promoteWasCalled := false

	mockClient := &tuiclient.MockClient{
		PromoteTaskFunc: func(ctx context.Context, id string) error {
			promoteWasCalled = true
			return nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Set up initial state with ready column having a task
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{
		{ID: "task-2", Title: "Task 2", State: "ready"},
	}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Move to ready column (from in_progress at index 2)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)

	// Verify we're in ready column
	if model.selectedColumn != 1 {
		t.Errorf("Expected selectedColumn 1 (ready), got %d", model.selectedColumn)
	}

	// Press 'p' on ready column (should be no-op)
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'p'}})
	model = m.(*BoardModel)

	// Verify promote was NOT called
	if promoteWasCalled {
		t.Errorf("Expected PromoteTask NOT to be called when on ready column")
	}

	// Command should be nil
	if cmd != nil {
		t.Errorf("Expected nil command when promoting on non-backlog column")
	}
}

// TestBoardModel_TickGeneration verifies the no-fork invariant:
//   - Init arms exactly ONE tick.
//   - A tickMsg arms exactly ONE tick (the re-arm) and issues a fetch.
//   - 'r' (manual refresh) arms ZERO ticks (issues a fetch only).
//   - tasksFetchedMsg arms ZERO ticks.
//
// The test uses an injectable newTickCmd that counts arming calls instead of using real timers.
// This test WILL FAIL if a tick is re-armed from the 'r' handler or from tasksFetchedMsg.
func TestBoardModel_TickGeneration(t *testing.T) {
	mockClient := &tuiclient.MockClient{}
	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	// --- Init arms exactly one tick ---
	model := NewBoardModel(mockClient, config, project)
	tickCount := countTickCmds(model)

	_ = model.Init()
	if *tickCount != 1 {
		t.Errorf("Init: expected exactly 1 tick armed, got %d", *tickCount)
	}

	// --- tasksFetchedMsg arms ZERO ticks ---
	*tickCount = 0
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "review", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}
	m, fetchedCmd := model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	if *tickCount != 0 {
		t.Errorf("tasksFetchedMsg: expected 0 ticks armed, got %d (tick fork regression)", *tickCount)
	}

	// fetchedCmd should be nil (tasksFetchedMsg returns no command)
	if fetchedCmd != nil {
		// Run it and check that no tickSentinel is produced
		msgs := runCmd(fetchedCmd)
		for _, msg := range msgs {
			if _, isTickSentinel := msg.(tickSentinel); isTickSentinel {
				t.Errorf("tasksFetchedMsg returned a cmd that produced a tick (tick fork regression)")
			}
		}
	}

	// --- 'r' (manual refresh) arms ZERO ticks ---
	*tickCount = 0
	m, refreshCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model = m.(*BoardModel)

	if *tickCount != 0 {
		t.Errorf("'r' key: expected 0 ticks armed, got %d (tick fork regression)", *tickCount)
	}
	if refreshCmd == nil {
		t.Errorf("'r' key: expected a fetch cmd (non-nil), got nil")
	}

	// Execute the refreshCmd and confirm it produces tasksFetchedMsg, NOT a tickSentinel.
	refreshMsgs := runCmd(refreshCmd)
	hasTasksFetched := false
	hasTick := false
	for _, msg := range refreshMsgs {
		switch msg.(type) {
		case tasksFetchedMsg:
			hasTasksFetched = true
		case tickSentinel:
			hasTick = true
		}
	}
	if !hasTasksFetched {
		t.Errorf("'r' key cmd: expected a tasksFetchedMsg, got none (msgs: %v)", refreshMsgs)
	}
	if hasTick {
		t.Errorf("'r' key cmd: produced a tick (tick fork regression)")
	}

	// --- tickMsg arms exactly ONE tick and issues a fetch ---
	*tickCount = 0
	m, tickCmdResult := model.Update(tickMsg{})
	model = m.(*BoardModel)

	if *tickCount != 1 {
		t.Errorf("tickMsg: expected exactly 1 tick armed, got %d", *tickCount)
	}
	if tickCmdResult == nil {
		t.Errorf("tickMsg: expected a non-nil batch cmd (fetch + tick), got nil")
	}

	// Execute the batch from tickMsg and confirm it produces BOTH a tasksFetchedMsg AND a tickSentinel.
	tickMsgs := runCmd(tickCmdResult)
	hasTasksFetchedFromTick := false
	hasTickFromTick := false
	for _, msg := range tickMsgs {
		switch msg.(type) {
		case tasksFetchedMsg:
			hasTasksFetchedFromTick = true
		case tickSentinel:
			hasTickFromTick = true
		}
	}
	if !hasTasksFetchedFromTick {
		t.Errorf("tickMsg batch: expected a tasksFetchedMsg, got none (msgs: %v)", tickMsgs)
	}
	if !hasTickFromTick {
		t.Errorf("tickMsg batch: expected a tickSentinel (re-armed tick), got none (msgs: %v)", tickMsgs)
	}

	// --- tasksFetchedMsg with error arms ZERO ticks ---
	*tickCount = 0
	m, errFetchedCmd := model.Update(tasksFetchedMsg{err: errors.New("network error")})
	_ = m

	if *tickCount != 0 {
		t.Errorf("tasksFetchedMsg(error): expected 0 ticks armed, got %d (tick fork regression)", *tickCount)
	}
	if errFetchedCmd != nil {
		msgs := runCmd(errFetchedCmd)
		for _, msg := range msgs {
			if _, isTickSentinel := msg.(tickSentinel); isTickSentinel {
				t.Errorf("tasksFetchedMsg(error) returned a cmd that produced a tick (tick fork regression)")
			}
		}
	}
}

// --- TUI-4: Review action tests ---

// reviewTestBucketed returns a bucketed task map with one review task.
func reviewTestBucketed(reviewTaskID string) map[string][]tuiclient.Task {
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "review", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["review"] = []tuiclient.Task{
		{ID: reviewTaskID, Title: "Review Task", State: "review"},
	}
	return bucketed
}

// buildReviewModel returns a board model set up in the review column with one review task
// selected. reviewClient is the mock client to inject.
func buildReviewModel(t *testing.T, reviewClient *tuiclient.MockClient) *BoardModel {
	t.Helper()

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "reviewer-bot",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(reviewClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Load a review task into the board.
	m, _ = model.Update(tasksFetchedMsg{tasks: reviewTestBucketed("review-task-1")})
	model = m.(*BoardModel)

	// Navigate to the review column (index 3). Starting from in_progress (index 2), press right once.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = m.(*BoardModel)

	if model.selectedColumn != 3 {
		t.Fatalf("buildReviewModel: expected selectedColumn 3 (review), got %d", model.selectedColumn)
	}
	if model.selectedTaskID != "review-task-1" {
		t.Fatalf("buildReviewModel: expected selectedTaskID review-task-1, got %q", model.selectedTaskID)
	}

	return model
}

// executeReviewCmd synchronously runs a tea.Cmd produced by a review action and delivers
// the resulting message back into the model. It returns the updated model.
func executeReviewCmd(t *testing.T, model *BoardModel, cmd tea.Cmd) *BoardModel {
	t.Helper()
	if cmd == nil {
		t.Fatal("executeReviewCmd: expected non-nil cmd")
	}
	resultMsg := cmd()
	m, _ := model.Update(resultMsg)
	return m.(*BoardModel)
}

func executeCmd(t *testing.T, model *BoardModel, cmd tea.Cmd) *BoardModel {
	t.Helper()
	if cmd == nil {
		t.Fatal("executeCmd: expected non-nil cmd")
	}
	resultMsg := cmd()
	m, _ := model.Update(resultMsg)
	return m.(*BoardModel)
}

// TestBoardModel_ApproveFlow_ConfirmY tests the full approve flow with a non-empty note,
// confirmed with 'y'. Both ReviewTask(approve) and TransitionTask(done) must be called
// with the correct actor, verdict, and note — in that order. A refetch follows.
func TestBoardModel_ApproveFlow_ConfirmY(t *testing.T) {
	var callLog []string // ordered log of calls
	var capturedReviewID, capturedReviewActor, capturedReviewVerdict string
	var capturedReviewNote *string
	var capturedTransitionID, capturedTransitionTo string
	var capturedTransitionNote *string

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			callLog = append(callLog, "review")
			capturedReviewID = id
			capturedReviewActor = actor
			capturedReviewVerdict = verdict
			capturedReviewNote = note
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			callLog = append(callLog, "transition")
			capturedTransitionID = id
			capturedTransitionTo = to
			capturedTransitionNote = note
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			// After approve, the task should be in done.
			return []tuiclient.Task{
				{ID: "review-task-1", Title: "Review Task", State: "done"},
			}, nil
		},
	}

	model := buildReviewModel(t, mockClient)

	// Press 'a' to start approve flow.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)

	if model.mode != modeApproveNote {
		t.Fatalf("Expected modeApproveNote after 'a', got mode %d", model.mode)
	}

	// Type a note.
	for _, ch := range "LGTM" {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		model = m.(*BoardModel)
	}

	// Press enter to advance to confirm step.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeApproveConfirm {
		t.Fatalf("Expected modeApproveConfirm after enter, got mode %d", model.mode)
	}

	// Confirm the output contains the confirm prompt.
	output := model.View()
	if !strings.Contains(output, "→ done?") {
		t.Errorf("Expected confirm prompt in view, got:\n%s", output)
	}

	// Press 'y' to confirm.
	m, approveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after confirm, got mode %d", model.mode)
	}

	// Execute the approve command (runs ReviewTask then TransitionTask then refetch inline).
	model = executeReviewCmd(t, model, approveCmd)

	// Verify call order: review → transition → refetch.
	if len(callLog) != 3 {
		t.Fatalf("Expected 3 calls (review, transition, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "review" {
		t.Errorf("Expected first call to be 'review', got %q", callLog[0])
	}
	if callLog[1] != "transition" {
		t.Errorf("Expected second call to be 'transition', got %q", callLog[1])
	}
	if callLog[2] != "refetch" {
		t.Errorf("Expected third call to be 'refetch', got %q", callLog[2])
	}

	// Verify ReviewTask arguments.
	if capturedReviewID != "review-task-1" {
		t.Errorf("ReviewTask: expected task ID review-task-1, got %q", capturedReviewID)
	}
	if capturedReviewActor != "reviewer-bot" {
		t.Errorf("ReviewTask: expected actor reviewer-bot, got %q", capturedReviewActor)
	}
	if capturedReviewVerdict != "approve" {
		t.Errorf("ReviewTask: expected verdict approve, got %q", capturedReviewVerdict)
	}
	if capturedReviewNote == nil || *capturedReviewNote != "LGTM" {
		t.Errorf("ReviewTask: expected note 'LGTM', got %v", capturedReviewNote)
	}

	// Verify TransitionTask arguments.
	if capturedTransitionID != "review-task-1" {
		t.Errorf("TransitionTask: expected task ID review-task-1, got %q", capturedTransitionID)
	}
	if capturedTransitionTo != "done" {
		t.Errorf("TransitionTask: expected to=done, got %q", capturedTransitionTo)
	}
	if capturedTransitionNote == nil || *capturedTransitionNote != "LGTM" {
		t.Errorf("TransitionTask: expected note 'LGTM', got %v", capturedTransitionNote)
	}

	// Board should now show the task in done.
	output = model.View()
	if !strings.Contains(output, "done(1)") {
		t.Errorf("Expected done(1) after approve, got:\n%s", output)
	}
}

// TestBoardModel_ApproveFlow_EmptyNote tests that approving with an empty note passes nil
// (not a pointer to an empty string) to both API calls.
func TestBoardModel_ApproveFlow_EmptyNote(t *testing.T) {
	var capturedReviewNote *string
	var capturedTransitionNote *string

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			capturedReviewNote = note
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			capturedTransitionNote = note
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{}, nil
		},
	}

	model := buildReviewModel(t, mockClient)

	// Press 'a' → skip note (press enter immediately) → confirm 'y'.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)

	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	m, approveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	model = executeReviewCmd(t, model, approveCmd)

	// Both calls should receive nil (not an empty string pointer).
	if capturedReviewNote != nil {
		t.Errorf("ReviewTask: expected nil note for empty input, got %q", *capturedReviewNote)
	}
	if capturedTransitionNote != nil {
		t.Errorf("TransitionTask: expected nil note for empty input, got %q", *capturedTransitionNote)
	}
	_ = model
}

// TestBoardModel_ApproveFlow_CancelWithEscAtNote tests that pressing esc at the note step
// cancels the action entirely without making any API calls.
func TestBoardModel_ApproveFlow_CancelWithEscAtNote(t *testing.T) {
	reviewCalled := false
	transitionCalled := false

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			reviewCalled = true
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			transitionCalled = true
			return nil
		},
	}

	model := buildReviewModel(t, mockClient)

	// Press 'a' then esc.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)

	if model.mode != modeApproveNote {
		t.Fatalf("Expected modeApproveNote, got %d", model.mode)
	}

	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after esc, got mode %d", model.mode)
	}
	if cmd != nil {
		t.Errorf("Expected nil cmd after esc cancel, got non-nil")
	}
	if reviewCalled {
		t.Errorf("ReviewTask was called but should not have been (action cancelled)")
	}
	if transitionCalled {
		t.Errorf("TransitionTask was called but should not have been (action cancelled)")
	}
}

// TestBoardModel_ApproveFlow_CancelWithN tests that pressing 'n' at the confirm step
// cancels without making any API calls.
func TestBoardModel_ApproveFlow_CancelWithN(t *testing.T) {
	reviewCalled := false
	transitionCalled := false

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			reviewCalled = true
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			transitionCalled = true
			return nil
		},
	}

	model := buildReviewModel(t, mockClient)

	// 'a' → enter (skip note) → 'n' (decline confirm).
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeApproveConfirm {
		t.Fatalf("Expected modeApproveConfirm, got %d", model.mode)
	}

	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after 'n', got mode %d", model.mode)
	}
	if cmd != nil {
		t.Errorf("Expected nil cmd after 'n' cancel, got non-nil")
	}
	if reviewCalled {
		t.Errorf("ReviewTask was called but should not have been (declined)")
	}
	if transitionCalled {
		t.Errorf("TransitionTask was called but should not have been (declined)")
	}
}

// TestBoardModel_RejectFlow_EmptyReasonBlocked tests that submitting an empty reason does
// not call any API and shows a hint to the user.
func TestBoardModel_RejectFlow_EmptyReasonBlocked(t *testing.T) {
	reviewCalled := false
	transitionCalled := false

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			reviewCalled = true
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			transitionCalled = true
			return nil
		},
	}

	model := buildReviewModel(t, mockClient)

	// Press 'x' to start reject flow.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model = m.(*BoardModel)

	if model.mode != modeRejectReason {
		t.Fatalf("Expected modeRejectReason after 'x', got %d", model.mode)
	}

	// Press enter immediately with no reason typed.
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	// Must still be in input mode.
	if model.mode != modeRejectReason {
		t.Errorf("Expected to stay in modeRejectReason on empty submit, got mode %d", model.mode)
	}
	// No API calls.
	if reviewCalled {
		t.Errorf("ReviewTask was called but should not have been (empty reason)")
	}
	if transitionCalled {
		t.Errorf("TransitionTask was called but should not have been (empty reason)")
	}
	// Should produce no command.
	if cmd != nil {
		t.Errorf("Expected nil cmd on empty reason submit, got non-nil")
	}

	// Hint should be visible in the view.
	output := model.View()
	if !strings.Contains(output, "reason is required") {
		t.Errorf("Expected 'reason is required' hint in view, got:\n%s", output)
	}
}

// TestBoardModel_RejectFlow_NonEmptyReason tests the full reject flow with a non-empty reason.
// Both ReviewTask(reject) and TransitionTask(ready) must be called with the correct args in order.
func TestBoardModel_RejectFlow_NonEmptyReason(t *testing.T) {
	var callLog []string
	var capturedReviewID, capturedReviewActor, capturedReviewVerdict string
	var capturedReviewNote *string
	var capturedTransitionID, capturedTransitionTo string
	var capturedTransitionNote *string

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			callLog = append(callLog, "review")
			capturedReviewID = id
			capturedReviewActor = actor
			capturedReviewVerdict = verdict
			capturedReviewNote = note
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			callLog = append(callLog, "transition")
			capturedTransitionID = id
			capturedTransitionTo = to
			capturedTransitionNote = note
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			// After reject, the task returns to ready.
			return []tuiclient.Task{
				{ID: "review-task-1", Title: "Review Task", State: "ready"},
			}, nil
		},
	}

	model := buildReviewModel(t, mockClient)

	// Press 'x' to start reject flow.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model = m.(*BoardModel)

	// Type a reason.
	for _, ch := range "needs tests" {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		model = m.(*BoardModel)
	}

	// Submit with enter.
	m, rejectCmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after submit, got mode %d", model.mode)
	}

	// Execute the reject command.
	model = executeReviewCmd(t, model, rejectCmd)

	// Verify call order.
	if len(callLog) != 3 {
		t.Fatalf("Expected 3 calls (review, transition, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "review" {
		t.Errorf("Expected first call 'review', got %q", callLog[0])
	}
	if callLog[1] != "transition" {
		t.Errorf("Expected second call 'transition', got %q", callLog[1])
	}
	if callLog[2] != "refetch" {
		t.Errorf("Expected third call 'refetch', got %q", callLog[2])
	}

	// Verify ReviewTask arguments.
	if capturedReviewID != "review-task-1" {
		t.Errorf("ReviewTask: expected task ID review-task-1, got %q", capturedReviewID)
	}
	if capturedReviewActor != "reviewer-bot" {
		t.Errorf("ReviewTask: expected actor reviewer-bot, got %q", capturedReviewActor)
	}
	if capturedReviewVerdict != "reject" {
		t.Errorf("ReviewTask: expected verdict reject, got %q", capturedReviewVerdict)
	}
	if capturedReviewNote == nil || *capturedReviewNote != "needs tests" {
		t.Errorf("ReviewTask: expected note 'needs tests', got %v", capturedReviewNote)
	}

	// Verify TransitionTask arguments.
	if capturedTransitionID != "review-task-1" {
		t.Errorf("TransitionTask: expected task ID review-task-1, got %q", capturedTransitionID)
	}
	if capturedTransitionTo != "ready" {
		t.Errorf("TransitionTask: expected to=ready, got %q", capturedTransitionTo)
	}
	if capturedTransitionNote == nil || *capturedTransitionNote != "needs tests" {
		t.Errorf("TransitionTask: expected note 'needs tests', got %v", capturedTransitionNote)
	}

	// Board should reflect the refetch.
	output := model.View()
	if !strings.Contains(output, "ready(1)") {
		t.Errorf("Expected ready(1) after reject refetch, got:\n%s", output)
	}
}

// TestBoardModel_ApproveTransition409 tests that a 409 from TransitionTask (after a successful
// ReviewTask) surfaces an error message in the model and triggers a refetch — it must not crash.
func TestBoardModel_ApproveTransition409(t *testing.T) {
	reviewCalled := false
	transitionCalled := false

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			reviewCalled = true
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			transitionCalled = true
			// Return a 409 as the real HTTPClient would.
			return &tuiclient.APIError{
				StatusCode: http.StatusConflict,
				Code:       "CONFLICT",
				Message:    "task already transitioned",
			}
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			// After the 409, refetch should still work.
			return []tuiclient.Task{
				{ID: "review-task-1", Title: "Review Task", State: "done"},
			}, nil
		},
	}

	model := buildReviewModel(t, mockClient)

	// Walk through the approve flow.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // skip note
	model = m.(*BoardModel)
	m, approveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}) // confirm
	model = m.(*BoardModel)

	// Execute the command.
	model = executeReviewCmd(t, model, approveCmd)

	// Both calls must have been made.
	if !reviewCalled {
		t.Errorf("ReviewTask was not called")
	}
	if !transitionCalled {
		t.Errorf("TransitionTask was not called")
	}

	// The model must surface the 409 via the error field but still render (no crash).
	if model.error == "" {
		t.Errorf("Expected non-empty error after 409 transition, got empty")
	}
	if !strings.Contains(model.error, "409") && !strings.Contains(model.error, "task already transitioned") {
		t.Errorf("Expected 409 message in error, got: %q", model.error)
	}

	// Model must still be usable — View must not panic.
	output := model.View()
	if output == "" {
		t.Errorf("Expected non-empty view after 409 error, got empty")
	}

	// The board was refreshed even after the 409.
	if !strings.Contains(output, "done(1)") {
		t.Errorf("Expected done(1) in view after refetch following 409, got:\n%s", output)
	}
}

// TestBoardModel_ReviewActionsNoopOutsideReviewColumn tests that pressing 'a' or 'x'
// when the active column is NOT review (index 3) does nothing — no API calls, no mode change.
func TestBoardModel_ReviewActionsNoopOutsideReviewColumn(t *testing.T) {
	reviewCalled := false
	transitionCalled := false

	mockClient := &tuiclient.MockClient{
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			reviewCalled = true
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			transitionCalled = true
			return nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "reviewer-bot",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Load a task in in_progress (default column, index 2) and one in backlog.
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{{ID: "task-b", Title: "Backlog Task", State: "backlog"}}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "task-ip", Title: "IP Task", State: "in_progress"}}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Sanity check: model starts in in_progress (index 2).
	if model.selectedColumn != 2 {
		t.Fatalf("Expected selectedColumn 2 (in_progress), got %d", model.selectedColumn)
	}

	// Press 'a' in in_progress — must be a no-op.
	m, cmdA := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)
	if model.mode != modeNormal {
		t.Errorf("'a' in in_progress: expected modeNormal, got %d", model.mode)
	}
	if cmdA != nil {
		t.Errorf("'a' in in_progress: expected nil cmd, got non-nil")
	}

	// Press 'x' in in_progress — must be a no-op.
	m, cmdX := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	model = m.(*BoardModel)
	if model.mode != modeNormal {
		t.Errorf("'x' in in_progress: expected modeNormal, got %d", model.mode)
	}
	if cmdX != nil {
		t.Errorf("'x' in in_progress: expected nil cmd, got non-nil")
	}

	// Navigate to backlog (two lefts), press 'a' — still a no-op.
	for i := 0; i < 2; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
		model = m.(*BoardModel)
	}
	m, cmdABacklog := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)
	if model.mode != modeNormal {
		t.Errorf("'a' in backlog: expected modeNormal, got %d", model.mode)
	}
	if cmdABacklog != nil {
		t.Errorf("'a' in backlog: expected nil cmd, got non-nil")
	}

	// None of the API calls should have fired.
	if reviewCalled {
		t.Errorf("ReviewTask was called but should not have been outside review column")
	}
	if transitionCalled {
		t.Errorf("TransitionTask was called but should not have been outside review column")
	}
}

// --- TUI-5: Detail view tests ---

// buildDetailModel constructs a board model that has the in_progress column populated with
// one task and the model is in modeDetail for that task.
// The provided task detail and documents are injected via a canned GetTask/ListDocuments.
func buildDetailModel(
	t *testing.T,
	taskDetail tuiclient.TaskDetail,
	documents []tuiclient.Document,
	boardTasks map[string][]tuiclient.Task,
) *BoardModel {
	t.Helper()

	var recordedURL string
	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
			return taskDetail, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return documents, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "reviewer-bot",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test", Repo: "https://github.com/example/repo"}

	model := NewBoardModel(mockClient, config, project)
	// Inject a no-op opener so tests don't launch a browser; the recorded URL is unused here.
	model.urlOpener = func(rawURL string) error {
		recordedURL = rawURL
		return nil
	}
	_ = recordedURL // used in individual tests

	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Load board tasks.
	if boardTasks == nil {
		boardTasks = make(map[string][]tuiclient.Task)
		for _, state := range []string{"backlog", "ready", "review", "done"} {
			boardTasks[state] = []tuiclient.Task{}
		}
		boardTasks["in_progress"] = []tuiclient.Task{
			{ID: taskDetail.ID, Title: taskDetail.Title, State: taskDetail.State},
		}
	}
	m, _ = model.Update(tasksFetchedMsg{tasks: boardTasks})
	model = m.(*BoardModel)

	// Trigger detail mode by pressing enter (with the task selected).
	model.selectedTaskID = taskDetail.ID
	m, detailCmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	// Execute the fetchDetailCmd.
	if detailCmd != nil {
		detailMsg := detailCmd()
		m, _ = model.Update(detailMsg)
		model = m.(*BoardModel)
	}

	return model
}

// TestDetail_EnterOpensDetail verifies that pressing enter opens the detail view and
// that spec, state, deps-as-titles, links, result, and meta are rendered.
func TestDetail_EnterOpensDetail(t *testing.T) {
	depID := "dep-task-1111"
	depTitle := "Dep Task One"
	result := "see PR #42"
	leaseAt := time.Now().Add(3 * time.Minute).Format(time.RFC3339Nano)
	assignee := "agent-x"

	taskDetail := tuiclient.TaskDetail{
		ID:             "task-detail-1",
		Title:          "My Feature Task",
		Spec:           "This is the full spec text for the task.",
		State:          "in_progress",
		Assignee:       &assignee,
		LeaseExpiresAt: &leaseAt,
		Result:         &result,
		CreatedAt:      "2026-01-01T00:00:00Z",
		UpdatedAt:      "2026-01-02T00:00:00Z",
		DependsOn:      []string{depID},
		Links: []tuiclient.TaskLink{
			{Kind: "pr", Value: "https://github.com/example/repo/pull/42"},
		},
	}

	// The board has the dep task so we can resolve its title.
	boardTasks := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "review", "done"} {
		boardTasks[state] = []tuiclient.Task{}
	}
	boardTasks["in_progress"] = []tuiclient.Task{
		{ID: "task-detail-1", Title: "My Feature Task", State: "in_progress"},
		{ID: depID, Title: depTitle, State: "in_progress"},
	}

	model := buildDetailModel(t, taskDetail, nil, boardTasks)

	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail after enter + fetch, got mode %d", model.mode)
	}

	output := model.View()

	// Title and state.
	if !strings.Contains(output, "My Feature Task") {
		t.Errorf("Expected task title in detail view, got:\n%s", output)
	}
	if !strings.Contains(output, "in_progress") {
		t.Errorf("Expected state in detail view, got:\n%s", output)
	}

	// Assignee.
	if !strings.Contains(output, "agent-x") {
		t.Errorf("Expected assignee in detail view, got:\n%s", output)
	}

	// Spec.
	if !strings.Contains(output, "This is the full spec text") {
		t.Errorf("Expected spec content in detail view, got:\n%s", output)
	}

	// Dep resolved to title.
	if !strings.Contains(output, depTitle) {
		t.Errorf("Expected dep title %q in detail view, got:\n%s", depTitle, output)
	}

	// Link.
	if !strings.Contains(output, "https://github.com/example/repo/pull/42") {
		t.Errorf("Expected PR link in detail view, got:\n%s", output)
	}

	// Result.
	if !strings.Contains(output, "see PR #42") {
		t.Errorf("Expected result in detail view, got:\n%s", output)
	}

	// Timestamps.
	if !strings.Contains(output, "2026-01-01") {
		t.Errorf("Expected created timestamp in detail view, got:\n%s", output)
	}
}

// TestDetail_EscReturnsToBoard verifies that pressing esc from detail returns to board (modeNormal).
func TestDetail_EscReturnsToBoard(t *testing.T) {
	taskDetail := tuiclient.TaskDetail{
		ID:        "task-esc-1",
		Title:     "Esc Test Task",
		Spec:      "spec",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	model := buildDetailModel(t, taskDetail, nil, nil)

	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail, got %d", model.mode)
	}

	// Press esc.
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after esc, got %d", model.mode)
	}
	if cmd != nil {
		t.Errorf("Expected nil cmd after esc in detail, got non-nil")
	}

	// Should render the board, not the detail view.
	output := model.View()
	if strings.Contains(output, "Task:") && strings.Contains(output, "State:") && !strings.Contains(output, "‹") {
		t.Errorf("Expected board view after esc, got something else:\n%s", output)
	}
	// Board view should show tabs.
	if !strings.Contains(output, "in_progress") {
		t.Errorf("Expected board column tabs after esc, got:\n%s", output)
	}
}

// TestDetail_BoardNavKeysIgnoredInDetail verifies that board navigation (arrow keys, h/l/j/k,
// column switching) does NOT fire when in detail mode.
func TestDetail_BoardNavKeysIgnoredInDetail(t *testing.T) {
	taskDetail := tuiclient.TaskDetail{
		ID:        "task-nav-isolation",
		Title:     "Nav Isolation",
		Spec:      "spec",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	model := buildDetailModel(t, taskDetail, nil, nil)

	originalColumn := model.selectedColumn

	// Press left/right — should NOT change the selected column.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)
	if model.selectedColumn != originalColumn {
		t.Errorf("Left key in detail changed column from %d to %d", originalColumn, model.selectedColumn)
	}
	if model.mode != modeDetail {
		t.Errorf("Expected to remain in modeDetail after left key, got %d", model.mode)
	}

	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = m.(*BoardModel)
	if model.selectedColumn != originalColumn {
		t.Errorf("Right key in detail changed column from %d to %d", originalColumn, model.selectedColumn)
	}
}

// TestDetail_OpenPRLink verifies that pressing 'o' in detail calls the opener with the PR URL.
func TestDetail_OpenPRLink(t *testing.T) {
	var openedURL string
	prURL := "https://github.com/example/repo/pull/99"

	taskDetail := tuiclient.TaskDetail{
		ID:        "task-pr-open",
		Title:     "PR Open Task",
		Spec:      "spec",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
		Links: []tuiclient.TaskLink{
			{Kind: "pr", Value: prURL},
		},
	}

	model := buildDetailModel(t, taskDetail, nil, nil)
	model.urlOpener = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	// Press 'o' to open PR.
	m, openCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	model = m.(*BoardModel)

	if openCmd == nil {
		t.Fatal("Expected non-nil cmd from 'o' key in detail")
	}
	resultMsg := openCmd()
	m, _ = model.Update(resultMsg)
	model = m.(*BoardModel)

	if openedURL != prURL {
		t.Errorf("Expected opener called with %q, got %q", prURL, openedURL)
	}

	// No error message shown.
	if model.detailMessage != "" {
		t.Errorf("Expected empty detailMessage on success, got %q", model.detailMessage)
	}
}

// TestDetail_OpenPRLink_NoPR verifies that 'o' when there's no PR link shows a message.
func TestDetail_OpenPRLink_NoPR(t *testing.T) {
	openerCalled := false

	taskDetail := tuiclient.TaskDetail{
		ID:        "task-no-pr",
		Title:     "No PR Task",
		Spec:      "spec",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
		Links:     []tuiclient.TaskLink{}, // no PR link
	}

	model := buildDetailModel(t, taskDetail, nil, nil)
	model.urlOpener = func(rawURL string) error {
		openerCalled = true
		return nil
	}

	m, openCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	model = m.(*BoardModel)

	if openCmd == nil {
		t.Fatal("Expected non-nil cmd from 'o'")
	}
	resultMsg := openCmd()
	m, _ = model.Update(resultMsg)
	model = m.(*BoardModel)

	if openerCalled {
		t.Errorf("Opener was called but should not have been (no PR link)")
	}
	if model.detailMessage == "" {
		t.Errorf("Expected a detailMessage (no PR link case), got empty")
	}
	if !strings.Contains(model.detailMessage, "no PR link") {
		t.Errorf("Expected 'no PR link' message, got %q", model.detailMessage)
	}
}

// TestDetail_OpenSourceDoc_RefPath verifies that 's' builds <repo>/blob/<branch>/<ref> for a path ref.
func TestDetail_OpenSourceDoc_RefPath(t *testing.T) {
	var openedURL string

	docID := "doc-source-1"
	taskDetail := tuiclient.TaskDetail{
		ID:         "task-src-path",
		Title:      "Source Path Task",
		Spec:       "spec",
		DocumentID: docID,
		State:      "in_progress",
		CreatedAt:  "2026-01-01T00:00:00Z",
		UpdatedAt:  "2026-01-01T00:00:00Z",
	}

	docs := []tuiclient.Document{
		{
			ID:        docID,
			ProjectID: "project-1",
			Kind:      "feature_spec",
			Title:     "Feature Doc",
			Ref:       "docs/features/my-feature.md",
			Commit:    nil, // no commit pin → use "main"
		},
	}

	model := buildDetailModel(t, taskDetail, docs, nil)
	model.urlOpener = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	m, srcCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	model = m.(*BoardModel)

	if srcCmd == nil {
		t.Fatal("Expected non-nil cmd from 's'")
	}
	resultMsg := srcCmd()
	m, _ = model.Update(resultMsg)
	model = m.(*BoardModel)

	expectedURL := "https://github.com/example/repo/blob/main/docs/features/my-feature.md"
	if openedURL != expectedURL {
		t.Errorf("Source doc URL: expected %q, got %q", expectedURL, openedURL)
	}
	if model.detailMessage != "" {
		t.Errorf("Expected no error message, got %q", model.detailMessage)
	}
}

// TestDetail_OpenSourceDoc_CommitPin verifies that 's' uses the commit hash when set.
func TestDetail_OpenSourceDoc_CommitPin(t *testing.T) {
	var openedURL string

	docID := "doc-source-2"
	commit := "abc1234def567890"
	taskDetail := tuiclient.TaskDetail{
		ID:         "task-src-commit",
		Title:      "Source Commit Task",
		Spec:       "spec",
		DocumentID: docID,
		State:      "in_progress",
		CreatedAt:  "2026-01-01T00:00:00Z",
		UpdatedAt:  "2026-01-01T00:00:00Z",
	}

	docs := []tuiclient.Document{
		{
			ID:        docID,
			ProjectID: "project-1",
			Kind:      "feature_spec",
			Title:     "Feature Doc",
			Ref:       "docs/features/pinned.md",
			Commit:    &commit,
		},
	}

	model := buildDetailModel(t, taskDetail, docs, nil)
	model.urlOpener = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	m, srcCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	model = m.(*BoardModel)

	resultMsg := srcCmd()
	m, _ = model.Update(resultMsg)
	model = m.(*BoardModel)

	expectedURL := "https://github.com/example/repo/blob/abc1234def567890/docs/features/pinned.md"
	if openedURL != expectedURL {
		t.Errorf("Commit-pinned source doc URL: expected %q, got %q", expectedURL, openedURL)
	}
}

// TestDetail_OpenSourceDoc_RefIsURL verifies that 's' opens a ref that is already a URL directly.
func TestDetail_OpenSourceDoc_RefIsURL(t *testing.T) {
	var openedURL string

	docID := "doc-source-url"
	refURL := "https://docs.example.com/feature-spec"
	taskDetail := tuiclient.TaskDetail{
		ID:         "task-src-url",
		Title:      "Source URL Task",
		Spec:       "spec",
		DocumentID: docID,
		State:      "in_progress",
		CreatedAt:  "2026-01-01T00:00:00Z",
		UpdatedAt:  "2026-01-01T00:00:00Z",
	}

	docs := []tuiclient.Document{
		{
			ID:        docID,
			ProjectID: "project-1",
			Kind:      "feature_spec",
			Title:     "External Doc",
			Ref:       refURL, // already a URL
		},
	}

	model := buildDetailModel(t, taskDetail, docs, nil)
	model.urlOpener = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	m, srcCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	model = m.(*BoardModel)

	resultMsg := srcCmd()
	m, _ = model.Update(resultMsg)
	model = m.(*BoardModel)

	// Must open the ref URL directly, not build a blob URL.
	if openedURL != refURL {
		t.Errorf("Ref-as-URL: expected opener called with %q, got %q", refURL, openedURL)
	}
}

// TestDetail_OpenDesignDoc verifies that 'd' finds the kind=design doc and builds the correct URL.
func TestDetail_OpenDesignDoc(t *testing.T) {
	var openedURL string

	taskDetail := tuiclient.TaskDetail{
		ID:        "task-design-open",
		Title:     "Design Doc Task",
		Spec:      "spec",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	docs := []tuiclient.Document{
		{
			ID:        "doc-spec-1",
			ProjectID: "project-1",
			Kind:      "feature_spec",
			Title:     "Some Feature",
			Ref:       "docs/features/some.md",
		},
		{
			ID:        "doc-design-1",
			ProjectID: "project-1",
			Kind:      "design",
			Title:     "Project Design",
			Ref:       "DESIGN.md",
		},
	}

	model := buildDetailModel(t, taskDetail, docs, nil)
	model.urlOpener = func(rawURL string) error {
		openedURL = rawURL
		return nil
	}

	m, designCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	model = m.(*BoardModel)

	resultMsg := designCmd()
	m, _ = model.Update(resultMsg)
	model = m.(*BoardModel)

	expectedURL := "https://github.com/example/repo/blob/main/DESIGN.md"
	if openedURL != expectedURL {
		t.Errorf("Design doc URL: expected %q, got %q", expectedURL, openedURL)
	}
}

// TestDetail_OpenDesignDoc_NoDesignDoc verifies that 'd' shows a message when no design doc exists.
func TestDetail_OpenDesignDoc_NoDesignDoc(t *testing.T) {
	openerCalled := false

	taskDetail := tuiclient.TaskDetail{
		ID:        "task-no-design",
		Title:     "No Design Task",
		Spec:      "spec",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	// Only a feature_spec doc, no design doc.
	docs := []tuiclient.Document{
		{
			ID:        "doc-feat-1",
			ProjectID: "project-1",
			Kind:      "feature_spec",
			Title:     "A Feature",
			Ref:       "docs/features/feat.md",
		},
	}

	model := buildDetailModel(t, taskDetail, docs, nil)
	model.urlOpener = func(rawURL string) error {
		openerCalled = true
		return nil
	}

	m, designCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	model = m.(*BoardModel)

	resultMsg := designCmd()
	m, _ = model.Update(resultMsg)
	model = m.(*BoardModel)

	if openerCalled {
		t.Errorf("Opener was called but should not (no design doc)")
	}
	if model.detailMessage == "" {
		t.Errorf("Expected detailMessage for no-design-doc case, got empty")
	}
	if !strings.Contains(model.detailMessage, "no design document") {
		t.Errorf("Expected 'no design document' message, got %q", model.detailMessage)
	}
}

// TestDetail_ReviewApproveFromDetail verifies that 'a' in detail mode for a review task
// initiates the approve flow and that it succeeds.
func TestDetail_ReviewApproveFromDetail(t *testing.T) {
	var callLog []string

	taskDetail := tuiclient.TaskDetail{
		ID:        "review-detail-task-1",
		Title:     "Detail Review Task",
		Spec:      "spec",
		State:     "review", // must be review to allow a/x
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	boardTasks := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "done"} {
		boardTasks[state] = []tuiclient.Task{}
	}
	boardTasks["review"] = []tuiclient.Task{
		{ID: "review-detail-task-1", Title: "Detail Review Task", State: "review"},
	}

	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
			return taskDetail, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return nil, nil
		},
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			callLog = append(callLog, "review:"+verdict)
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			callLog = append(callLog, "transition:"+to)
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			return []tuiclient.Task{
				{ID: "review-detail-task-1", Title: "Detail Review Task", State: "done"},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "reviewer-bot",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test", Repo: "https://github.com/example/repo"}

	model := NewBoardModel(mockClient, config, project)
	model.urlOpener = func(rawURL string) error { return nil }

	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	m, _ = model.Update(tasksFetchedMsg{tasks: boardTasks})
	model = m.(*BoardModel)

	// Navigate to review column (index 3): one right from in_progress (index 2).
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = m.(*BoardModel)

	model.selectedTaskID = "review-detail-task-1"

	// Press enter to open detail.
	m, detailCmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)
	if detailCmd != nil {
		m, _ = model.Update(detailCmd())
		model = m.(*BoardModel)
	}

	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail, got %d", model.mode)
	}

	// Press 'a' to start approve flow from detail.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)

	if model.mode != modeApproveNote {
		t.Fatalf("Expected modeApproveNote after 'a' in detail, got %d", model.mode)
	}
	if !model.reviewFromDetail {
		t.Errorf("Expected reviewFromDetail=true")
	}

	// Skip note (press enter).
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeApproveConfirm {
		t.Fatalf("Expected modeApproveConfirm, got %d", model.mode)
	}

	// Confirm with 'y'.
	m, approveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	// After cancel, should return to modeDetail.
	if model.mode != modeDetail {
		t.Errorf("Expected modeDetail after confirm (before cmd runs), got %d", model.mode)
	}

	// Execute the approve command.
	model = executeReviewCmd(t, model, approveCmd)

	// Verify approve was called.
	if len(callLog) < 2 {
		t.Fatalf("Expected at least review+transition calls, got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "review:approve" {
		t.Errorf("Expected first call review:approve, got %q", callLog[0])
	}
	if callLog[1] != "transition:done" {
		t.Errorf("Expected second call transition:done, got %q", callLog[1])
	}

	// After the command completes, the model must be back in modeNormal (the board),
	// not stuck in detail showing stale state.
	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after from-detail approve cmd, got %d", model.mode)
	}

	// The board should show the task in its new state (done(1)).
	output := model.View()
	if !strings.Contains(output, "done(1)") {
		t.Errorf("Expected done(1) in board view after from-detail approve, got:\n%s", output)
	}
}

// TestDetail_ReviewApproveFromDetail_409Visible verifies that when a 409 is returned during
// a from-detail approve, the error is visible in the board view (modeNormal), not swallowed
// in the detail view which does not render m.error.
func TestDetail_ReviewApproveFromDetail_409Visible(t *testing.T) {
	taskDetail := tuiclient.TaskDetail{
		ID:        "review-detail-409",
		Title:     "Detail 409 Task",
		Spec:      "spec",
		State:     "review",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	boardTasks := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "done"} {
		boardTasks[state] = []tuiclient.Task{}
	}
	boardTasks["review"] = []tuiclient.Task{
		{ID: "review-detail-409", Title: "Detail 409 Task", State: "review"},
	}

	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
			return taskDetail, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return nil, nil
		},
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			// Simulate a 409: task already transitioned (race).
			return &tuiclient.APIError{
				StatusCode: http.StatusConflict,
				Code:       "CONFLICT",
				Message:    "task already transitioned",
			}
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			// Refetch after 409: task is already done.
			return []tuiclient.Task{
				{ID: "review-detail-409", Title: "Detail 409 Task", State: "done"},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "reviewer-bot",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test", Repo: "https://github.com/example/repo"}

	model := NewBoardModel(mockClient, config, project)
	model.urlOpener = func(rawURL string) error { return nil }

	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	m, _ = model.Update(tasksFetchedMsg{tasks: boardTasks})
	model = m.(*BoardModel)

	// Navigate to review column and open detail.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = m.(*BoardModel)
	model.selectedTaskID = "review-detail-409"

	m, detailCmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)
	if detailCmd != nil {
		m, _ = model.Update(detailCmd())
		model = m.(*BoardModel)
	}
	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail, got %d", model.mode)
	}

	// Start approve flow from detail.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter}) // skip note
	model = m.(*BoardModel)
	m, approveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}}) // confirm
	model = m.(*BoardModel)

	// Execute the approve command (which triggers a 409 from TransitionTask).
	model = executeReviewCmd(t, model, approveCmd)

	// The model must be back in modeNormal so the error banner is visible.
	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after from-detail 409, got %d — error is invisible in detail mode", model.mode)
	}

	// The error must be non-empty and visible in the board view.
	if model.error == "" {
		t.Errorf("Expected non-empty error after 409 from-detail action, got empty")
	}

	output := model.View()
	if !strings.Contains(output, "409") && !strings.Contains(output, "task already transitioned") {
		t.Errorf("Expected 409 error in board view output, got:\n%s", output)
	}

	// Board was refreshed: the task should now show as done(1).
	if !strings.Contains(output, "done(1)") {
		t.Errorf("Expected done(1) in board view after refetch, got:\n%s", output)
	}
}

// TestDetail_SpecViewportScrolls verifies that the spec viewport scrolls when up/down are pressed.
func TestDetail_SpecViewportScrolls(t *testing.T) {
	// Build a spec with many lines so there is room to scroll.
	var specLines []string
	for i := 0; i < 50; i++ {
		specLines = append(specLines, fmt.Sprintf("Spec line %d: some content here to fill space.", i))
	}
	longSpec := strings.Join(specLines, "\n")

	taskDetail := tuiclient.TaskDetail{
		ID:        "task-scroll",
		Title:     "Scroll Test Task",
		Spec:      longSpec,
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}

	model := buildDetailModel(t, taskDetail, nil, nil)

	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail, got %d", model.mode)
	}

	// Initial viewport should be at the top.
	if model.detailViewport.YOffset != 0 {
		t.Errorf("Expected initial YOffset=0, got %d", model.detailViewport.YOffset)
	}

	// Press down to scroll.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m.(*BoardModel)

	if model.detailViewport.YOffset != 1 {
		t.Errorf("Expected YOffset=1 after one down, got %d", model.detailViewport.YOffset)
	}

	// Press up to scroll back.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyUp})
	model = m.(*BoardModel)

	if model.detailViewport.YOffset != 0 {
		t.Errorf("Expected YOffset=0 after up, got %d", model.detailViewport.YOffset)
	}
}

// TestResolveDocURL_GitSuffix verifies that a project.repo ending in ".git" is normalized
// before building the blob URL, so the result is "…/repo/blob/main/<ref>" not "…/repo.git/blob/…".
func TestResolveDocURL_GitSuffix(t *testing.T) {
	doc := tuiclient.Document{
		ID:     "doc-git-suffix",
		Ref:    "docs/spec.md",
		Commit: nil, // no pin → defaults to "main"
	}

	tests := []struct {
		name        string
		projectRepo string
		expectedURL string
	}{
		{
			name:        "trailing .git stripped",
			projectRepo: "https://github.com/owner/repo.git",
			expectedURL: "https://github.com/owner/repo/blob/main/docs/spec.md",
		},
		{
			name:        "trailing slash then .git both stripped",
			projectRepo: "https://github.com/owner/repo.git/",
			expectedURL: "https://github.com/owner/repo/blob/main/docs/spec.md",
		},
		{
			name:        "clean repo unaffected",
			projectRepo: "https://github.com/owner/repo",
			expectedURL: "https://github.com/owner/repo/blob/main/docs/spec.md",
		},
		{
			name:        "trailing slash only stripped",
			projectRepo: "https://github.com/owner/repo/",
			expectedURL: "https://github.com/owner/repo/blob/main/docs/spec.md",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotURL, errMsg := resolveDocURL(doc.Ref, doc, tc.projectRepo)
			if errMsg != "" {
				t.Errorf("resolveDocURL returned error: %q", errMsg)
			}
			if gotURL != tc.expectedURL {
				t.Errorf("resolveDocURL: expected %q, got %q", tc.expectedURL, gotURL)
			}
		})
	}
}

// TestBoardModel_ReviewHelpBar verifies that the help bar shows approve/reject hints only
// when the active column is review, and omits them elsewhere.
func TestBoardModel_ReviewHelpBar(t *testing.T) {
	mockClient := &tuiclient.MockClient{}
	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "reviewer-bot",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	bucketed := make(map[string][]tuiclient.Task)
	for _, s := range []string{"backlog", "ready", "in_progress", "review", "approved", "done"} {
		bucketed[s] = []tuiclient.Task{}
	}
	bucketed["review"] = []tuiclient.Task{{ID: "r1", Title: "R1", State: "review"}}
	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to review column (index 3): one right from in_progress (index 2).
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
	model = m.(*BoardModel)

	if model.selectedColumn != 3 {
		t.Fatalf("Expected review column (3), got %d", model.selectedColumn)
	}

	reviewOutput := model.View()
	if !strings.Contains(reviewOutput, "a approve") {
		t.Errorf("Expected 'a approve' in help bar when in review column, got:\n%s", reviewOutput)
	}
	if !strings.Contains(reviewOutput, "x reject") {
		t.Errorf("Expected 'x reject' in help bar when in review column, got:\n%s", reviewOutput)
	}

	// Navigate away to in_progress.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)

	nonReviewOutput := model.View()
	if strings.Contains(nonReviewOutput, "a approve") {
		t.Errorf("Did not expect 'a approve' in help bar when NOT in review column, got:\n%s", nonReviewOutput)
	}
	if strings.Contains(nonReviewOutput, "x reject") {
		t.Errorf("Did not expect 'x reject' in help bar when NOT in review column, got:\n%s", nonReviewOutput)
	}
}

// TestBucketTasksByState tests that review-kind and merge-kind tasks are excluded from
// the done column.
func TestBucketTasksByState(t *testing.T) {
	// Create a mixed set of tasks with implement, review, and merge kinds
	tasks := []tuiclient.Task{
		{ID: "backlog-1", Title: "Task 1", State: "backlog", Kind: "implement"},
		{ID: "ready-1", Title: "Task 2", State: "ready", Kind: "implement"},
		{ID: "review-1", Title: "Review: Task 1", State: "in_progress", Kind: "review"},
		{ID: "impl-1", Title: "Implement Feature", State: "done", Kind: "implement"},
		{ID: "review-2", Title: "Review: Task 2", State: "done", Kind: "review"},
		{ID: "merge-1", Title: "Merge: Task 1", State: "done", Kind: "merge"},
		{ID: "impl-2", Title: "Fix Bug", State: "done", Kind: "implement"},
	}

	bucketed := bucketTasksByState(tasks)

	// Check that done column has only 2 tasks (the implement ones, not review or merge)
	if len(bucketed["done"]) != 2 {
		t.Errorf("Expected done column to have 2 tasks, got %d: %v", len(bucketed["done"]), bucketed["done"])
	}

	// Check that in_progress column has the review task
	if len(bucketed["in_progress"]) != 1 {
		t.Errorf("Expected in_progress column to have 1 task, got %d", len(bucketed["in_progress"]))
	}
	if bucketed["in_progress"][0].Kind != "review" {
		t.Errorf("Expected in_progress task to be review kind, got %s", bucketed["in_progress"][0].Kind)
	}

	// Verify the done column has the correct tasks
	doneTaskIDs := map[string]bool{}
	for _, task := range bucketed["done"] {
		doneTaskIDs[task.ID] = true
	}
	if !doneTaskIDs["impl-1"] {
		t.Errorf("Expected impl-1 in done column")
	}
	if !doneTaskIDs["impl-2"] {
		t.Errorf("Expected impl-2 in done column")
	}
	if doneTaskIDs["review-2"] {
		t.Errorf("Expected review-2 to be filtered out from done column")
	}
	if doneTaskIDs["merge-1"] {
		t.Errorf("Expected merge-1 to be filtered out from done column")
	}

	// Verify other columns are not affected
	if len(bucketed["backlog"]) != 1 {
		t.Errorf("Expected backlog to have 1 task, got %d", len(bucketed["backlog"]))
	}
	if len(bucketed["ready"]) != 1 {
		t.Errorf("Expected ready to have 1 task, got %d", len(bucketed["ready"]))
	}
}

// TestBoardModel_ProjectSwitch tests that the project switcher opens with 'P' key,
// allows navigation, and properly switches projects while resetting board state.
func TestBoardModel_ProjectSwitch(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-1", Title: "Task 1", State: "in_progress"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project1 := tuiclient.Project{ID: "project-1", Name: "Project 1"}
	project2 := tuiclient.Project{ID: "project-2", Name: "Project 2"}

	model := NewBoardModel(mockClient, config, project1)
	model.width = 80
	model.height = 24

	// Initialize with projects
	projects := []tuiclient.Project{project1, project2}
	m, _ := model.Update(projectsFetchedMsg{projects: projects})
	model = m.(*BoardModel)

	// Initialize with tasks
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range stateOrder {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "task-1", Title: "Task 1", State: "in_progress"}}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Select a task
	model.selectedTaskID = "task-1"
	model.selectedIndex = 0

	// Verify initial state
	if model.project.ID != project1.ID {
		t.Errorf("Expected initial project to be %s, got %s", project1.ID, model.project.ID)
	}
	if model.selectedTaskID != "task-1" {
		t.Errorf("Expected selectedTaskID to be 'task-1', got %s", model.selectedTaskID)
	}

	// Press 'P' to open project switcher
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'P'}})
	model = m.(*BoardModel)

	if model.mode != modeProjectSwitch {
		t.Errorf("Expected mode to be modeProjectSwitch, got %d", model.mode)
	}

	// Navigate to second project
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m.(*BoardModel)

	if model.projectSwitchIndex != 1 {
		t.Errorf("Expected projectSwitchIndex to be 1, got %d", model.projectSwitchIndex)
	}

	// Select the second project (press enter)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	// Verify project switched
	if model.project.ID != project2.ID {
		t.Errorf("Expected project to be %s, got %s", project2.ID, model.project.ID)
	}

	// Verify mode returned to normal
	if model.mode != modeNormal {
		t.Errorf("Expected mode to be modeNormal after switch, got %d", model.mode)
	}

	// Verify selection state was reset
	if model.selectedTaskID != "" {
		t.Errorf("Expected selectedTaskID to be empty after project switch, got %s", model.selectedTaskID)
	}

	if model.selectedIndex != 0 {
		t.Errorf("Expected selectedIndex to be 0 after project switch, got %d", model.selectedIndex)
	}

	if model.selectedColumn != 2 {
		t.Errorf("Expected selectedColumn to be 2 after project switch, got %d", model.selectedColumn)
	}

	// Verify tasks were cleared and fetch was triggered
	if len(model.tasks) != 0 || model.loading != true {
		t.Errorf("Expected tasks to be cleared and loading=true, got tasks=%v loading=%v", model.tasks, model.loading)
	}
}

// TestBoardModel_ProjectSwitchSameProject tests that selecting the current project is a no-op.
func TestBoardModel_ProjectSwitchSameProject(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-1", Title: "Task 1", State: "in_progress"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project1 := tuiclient.Project{ID: "project-1", Name: "Project 1"}
	project2 := tuiclient.Project{ID: "project-2", Name: "Project 2"}

	model := NewBoardModel(mockClient, config, project1)
	model.width = 80
	model.height = 24

	// Initialize with projects
	projects := []tuiclient.Project{project1, project2}
	m, _ := model.Update(projectsFetchedMsg{projects: projects})
	model = m.(*BoardModel)

	// Initialize with tasks
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range stateOrder {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "task-1", Title: "Task 1", State: "in_progress"}}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Select a task
	model.selectedTaskID = "task-1"
	model.selectedIndex = 0

	// Open project switcher
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'P'}})
	model = m.(*BoardModel)

	// Select the same project (first one is already selected)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	// Verify project is still project1
	if model.project.ID != project1.ID {
		t.Errorf("Expected project to be %s, got %s", project1.ID, model.project.ID)
	}

	// Verify mode returned to normal
	if model.mode != modeNormal {
		t.Errorf("Expected mode to be modeNormal, got %d", model.mode)
	}

	// Verify selection was NOT reset (same project is a no-op)
	if model.selectedTaskID != "task-1" {
		t.Errorf("Expected selectedTaskID to remain 'task-1', got %s", model.selectedTaskID)
	}
}

// TestBoardModel_ProjectNameInHeader verifies that the active project name is shown in the board header.
func TestBoardModel_ProjectNameInHeader(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-1", Title: "Task 1", State: "backlog"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "My Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate task fetch
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{{ID: "task-1", Title: "Task 1", State: "backlog"}}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	output := model.View()

	// Verify project name is in the header
	if !strings.Contains(output, "My Test Project") {
		t.Errorf("Expected project name 'My Test Project' in header, got:\n%s", output)
	}

	// Verify project name appears before tabs
	projIndex := strings.Index(output, "My Test Project")
	tabsIndex := strings.Index(output, "backlog")
	if projIndex > tabsIndex {
		t.Errorf("Expected project name to appear before tabs in output")
	}
}

// TestBoardModel_ProjectNameTruncation verifies that long project names are truncated gracefully.
func TestBoardModel_ProjectNameTruncation(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	// Very long project name
	longName := "This Is A Very Long Project Name That Should Be Truncated"
	project := tuiclient.Project{ID: "project-1", Name: longName}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 40, Height: 24}) // moderately narrow terminal
	model = m.(*BoardModel)

	// Simulate task fetch
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "review", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	output := model.View()

	// Verify the full name is truncated (should have ellipsis)
	if strings.Contains(output, longName) {
		t.Errorf("Expected long project name to be truncated, but found full name in output:\n%s", output)
	}

	// Verify truncation marker (ellipsis) is present
	if !strings.Contains(output, "…") {
		t.Errorf("Expected truncation marker '…' in output:\n%s", output)
	}

	// Verify the first part of the name is still visible
	if !strings.Contains(output, "This Is A Very") {
		t.Errorf("Expected start of project name in truncated output:\n%s", output)
	}
}

// TestBoardModel_ProjectNameInDetailView verifies that the project name is also shown in the detail view.
func TestBoardModel_ProjectNameInDetailView(t *testing.T) {
	taskDetail := tuiclient.TaskDetail{
		ID:        "task-detail-1",
		Title:     "My Feature Task",
		Spec:      "This is the spec.",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-02T00:00:00Z",
	}

	project := tuiclient.Project{ID: "project-1", Name: "Detail Test Project", Repo: "https://github.com/example/repo"}

	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
			return taskDetail, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return nil, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Load board tasks
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "review", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["in_progress"] = []tuiclient.Task{
		{ID: "task-detail-1", Title: "My Feature Task", State: "in_progress"},
	}
	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Open detail view
	model.selectedTaskID = "task-detail-1"
	m, detailCmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	if detailCmd != nil {
		detailMsg := detailCmd()
		m, _ = model.Update(detailMsg)
		model = m.(*BoardModel)
	}

	output := model.View()

	// Verify project name is in the detail view
	if !strings.Contains(output, "Detail Test Project") {
		t.Errorf("Expected project name 'Detail Test Project' in detail view, got:\n%s", output)
	}
}

// buildBlockedModel returns a board model set up in the blocked column with one blocked task selected.
func buildBlockedModel(t *testing.T, blockedClient *tuiclient.MockClient) *BoardModel {
	t.Helper()

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "tester",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(blockedClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Load a blocked task into the board.
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range stateOrder {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["blocked"] = []tuiclient.Task{
		{ID: "blocked-task-1", Title: "Blocked Task", State: "blocked"},
	}
	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to the blocked column (index 6).
	// Starting from in_progress (index 2), press right 4 times.
	for i := 0; i < 4; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model = m.(*BoardModel)
	}

	if model.selectedColumn != 6 {
		t.Fatalf("buildBlockedModel: expected selectedColumn 6 (blocked), got %d", model.selectedColumn)
	}
	if model.selectedTaskID != "blocked-task-1" {
		t.Fatalf("buildBlockedModel: expected selectedTaskID blocked-task-1, got %q", model.selectedTaskID)
	}

	return model
}

// TestBoardModel_UnblockFlow tests the full unblock flow with an optional note.
func TestBoardModel_UnblockFlow(t *testing.T) {
	var callLog []string
	var capturedTransitionID, capturedTransitionTo string
	var capturedTransitionNote *string

	mockClient := &tuiclient.MockClient{
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			callLog = append(callLog, "transition")
			capturedTransitionID = id
			capturedTransitionTo = to
			capturedTransitionNote = note
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			// After unblock, the task should be in ready.
			return []tuiclient.Task{
				{ID: "blocked-task-1", Title: "Blocked Task", State: "ready"},
			}, nil
		},
	}

	model := buildBlockedModel(t, mockClient)

	// Press 'u' to start unblock flow.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	model = m.(*BoardModel)

	if model.mode != modeUnblockNote {
		t.Fatalf("Expected modeUnblockNote after 'u', got mode %d", model.mode)
	}

	// Type a note.
	for _, ch := range "Dependency resolved" {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		model = m.(*BoardModel)
	}

	// Press enter to advance to confirm step.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeUnblockConfirm {
		t.Fatalf("Expected modeUnblockConfirm after enter, got mode %d", model.mode)
	}

	// Verify the render output contains the confirm prompt (non-empty).
	output := model.View()
	if output == "" {
		t.Errorf("Expected non-empty render output in modeUnblockConfirm, got blank")
	}
	if !strings.Contains(output, "→ ready?") {
		t.Errorf("Expected confirm prompt in view, got:\n%s", output)
	}

	// Press 'y' to confirm.
	m, unblockCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after confirm, got mode %d", model.mode)
	}

	// Execute the unblock command (runs TransitionTask then refetch inline).
	model = executeReviewCmd(t, model, unblockCmd)

	// Verify call order: transition → refetch.
	if len(callLog) != 2 {
		t.Fatalf("Expected 2 calls (transition, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "transition" {
		t.Errorf("Expected first call to be 'transition', got %q", callLog[0])
	}
	if callLog[1] != "refetch" {
		t.Errorf("Expected second call to be 'refetch', got %q", callLog[1])
	}

	// Verify TransitionTask arguments.
	if capturedTransitionID != "blocked-task-1" {
		t.Errorf("TransitionTask: expected task ID blocked-task-1, got %q", capturedTransitionID)
	}
	if capturedTransitionTo != "ready" {
		t.Errorf("TransitionTask: expected to=ready, got %q", capturedTransitionTo)
	}
	if capturedTransitionNote == nil || *capturedTransitionNote != "Dependency resolved" {
		t.Errorf("TransitionTask: expected note 'Dependency resolved', got %v", capturedTransitionNote)
	}

	// Board should now show the task in ready.
	output = model.View()
	if !strings.Contains(output, "ready(1)") {
		t.Errorf("Expected ready(1) after unblock, got:\n%s", output)
	}
}

// TestBoardModel_FailFlow tests the full fail flow with an optional note.
func TestBoardModel_FailFlow(t *testing.T) {
	var callLog []string
	var capturedTransitionID, capturedTransitionTo string
	var capturedTransitionNote *string

	mockClient := &tuiclient.MockClient{
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			callLog = append(callLog, "transition")
			capturedTransitionID = id
			capturedTransitionTo = to
			capturedTransitionNote = note
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			// After fail, the task should be in failed.
			return []tuiclient.Task{
				{ID: "blocked-task-1", Title: "Blocked Task", State: "failed"},
			}, nil
		},
	}

	model := buildBlockedModel(t, mockClient)

	// Press 'f' to start fail flow.
	m, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	model = m.(*BoardModel)

	if model.mode != modeFailNote {
		t.Fatalf("Expected modeFailNote after 'f', got mode %d", model.mode)
	}

	// Type a note.
	for _, ch := range "Unresolvable" {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}})
		model = m.(*BoardModel)
	}

	// Press enter to advance to confirm step.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeFailConfirm {
		t.Fatalf("Expected modeFailConfirm after enter, got mode %d", model.mode)
	}

	// Verify the render output contains the confirm prompt (non-empty).
	output := model.View()
	if output == "" {
		t.Errorf("Expected non-empty render output in modeFailConfirm, got blank")
	}
	if !strings.Contains(output, "→ failed?") {
		t.Errorf("Expected confirm prompt in view, got:\n%s", output)
	}

	// Press 'y' to confirm.
	m, failCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after confirm, got mode %d", model.mode)
	}

	// Execute the fail command (runs TransitionTask then refetch inline).
	model = executeReviewCmd(t, model, failCmd)

	// Verify call order: transition → refetch.
	if len(callLog) != 2 {
		t.Fatalf("Expected 2 calls (transition, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "transition" {
		t.Errorf("Expected first call to be 'transition', got %q", callLog[0])
	}
	if callLog[1] != "refetch" {
		t.Errorf("Expected second call to be 'refetch', got %q", callLog[1])
	}

	// Verify TransitionTask arguments.
	if capturedTransitionID != "blocked-task-1" {
		t.Errorf("TransitionTask: expected task ID blocked-task-1, got %q", capturedTransitionID)
	}
	if capturedTransitionTo != "failed" {
		t.Errorf("TransitionTask: expected to=failed, got %q", capturedTransitionTo)
	}
	if capturedTransitionNote == nil || *capturedTransitionNote != "Unresolvable" {
		t.Errorf("TransitionTask: expected note 'Unresolvable', got %v", capturedTransitionNote)
	}

	// Board should now show the task in failed (not visible in blocked column anymore).
	output = model.View()
	if !strings.Contains(output, "blocked(0)") {
		t.Errorf("Expected blocked(0) after fail, got:\n%s", output)
	}
	if !strings.Contains(output, "failed(1)") {
		t.Errorf("Expected failed(1) after fail, got:\n%s", output)
	}
}

// TestBoardModel_AbandonedTaskVisible verifies that a task in abandoned state is visible on the board
// and renders the abandoned(N) column with the correct count.
func TestBoardModel_AbandonedTaskVisible(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate fetch with one abandoned task
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range stateOrder {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["abandoned"] = []tuiclient.Task{
		{ID: "abandoned-task-1", Title: "Abandoned Task", State: "abandoned"},
	}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to the abandoned column (index 8).
	// Starting from in_progress (index 2), press right 6 times.
	for i := 0; i < 6; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model = m.(*BoardModel)
	}

	output := model.View()

	// Verify the abandoned column is visible with the correct count
	if !strings.Contains(output, "abandoned(1)") {
		t.Errorf("Expected 'abandoned(1)' in output, got:\n%s", output)
	}

	// Verify the abandoned task is rendered
	if !strings.Contains(output, "Abandoned Task") {
		t.Errorf("Expected abandoned task title in output, got:\n%s", output)
	}
}

// TestDetail_ViewportOffsetClamping verifies that initDetailViewport clamps the carried-over
// YOffset to the new content's valid range, preventing panics when opening a shorter task
// after scrolling through a longer task.
func TestDetail_ViewportOffsetClamping(t *testing.T) {
	shortContent := "Short\nTask"
	longContent := strings.Repeat("Line\n", 100) // 100-line content

	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
			if id == "long-task" {
				return tuiclient.TaskDetail{
					ID:        "long-task",
					Title:     "Long Task",
					Spec:      longContent,
					State:     "in_progress",
					CreatedAt: "2026-01-01T00:00:00Z",
					UpdatedAt: "2026-01-01T00:00:00Z",
				}, nil
			}
			return tuiclient.TaskDetail{
				ID:        "short-task",
				Title:     "Short Task",
				Spec:      shortContent,
				State:     "in_progress",
				CreatedAt: "2026-01-01T00:00:00Z",
				UpdatedAt: "2026-01-01T00:00:00Z",
			}, nil
		},
		ListDocumentsFunc: func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
			return nil, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "test-user",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Set up board tasks.
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "review", "done"} {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["in_progress"] = []tuiclient.Task{
		{ID: "long-task", Title: "Long Task", State: "in_progress"},
		{ID: "short-task", Title: "Short Task", State: "in_progress"},
	}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Open the long task detail.
	model.selectedTaskID = "long-task"
	m, detailCmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)
	if detailCmd != nil {
		msg := detailCmd()
		m, _ = model.Update(msg)
		model = m.(*BoardModel)
	}

	// Verify we're in detail mode.
	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail, got %d", model.mode)
	}

	// Scroll down in the long task to set a large YOffset.
	for i := 0; i < 10; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = m.(*BoardModel)
	}
	largeOffset := model.detailViewport.YOffset
	if largeOffset == 0 {
		t.Fatalf("Expected YOffset > 0 after scrolling, got 0")
	}
	t.Logf("Scrolled to offset %d in long task", largeOffset)

	// Go back to board.
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = m.(*BoardModel)

	// Open the short task detail. This should clamp the carried-over YOffset.
	model.selectedTaskID = "short-task"
	m, detailCmd = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)
	if detailCmd != nil {
		msg := detailCmd()
		m, _ = model.Update(msg)
		model = m.(*BoardModel)
	}

	// Verify we're in detail mode with the short task.
	if model.mode != modeDetail {
		t.Fatalf("Expected modeDetail for short task, got %d", model.mode)
	}

	// The critical test: verify that the offset is clamped and doesn't exceed the valid range.
	clampedOffset := model.detailViewport.YOffset
	maxValidOffset := max(0, model.detailViewport.TotalLineCount()-model.detailViewport.VisibleLineCount())
	if clampedOffset > maxValidOffset {
		t.Errorf("YOffset %d exceeds max valid %d for content with %d lines and viewport height %d",
			clampedOffset, maxValidOffset, model.detailViewport.TotalLineCount(), model.detailViewport.VisibleLineCount())
	}
	t.Logf("Clamped offset to %d (max valid: %d)", clampedOffset, maxValidOffset)

	// The final test: render the view without panicking.
	// This will panic if the slice bounds are out of range.
	output := model.View()
	if output == "" {
		t.Errorf("Expected non-empty view output, got empty")
	}
}

// TestBoardModel_ColumnScrolling tests that a column with many tasks scrolls correctly.
// Verify that navigating down scrolls the selected task into view, and that
// the selected row is always visible in the viewport.
func TestBoardModel_ColumnScrolling(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	// Set a small height to force scrolling with fewer tasks
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	model = m.(*BoardModel)

	// Create many tasks in the done column to force scrolling
	var doneTasks []tuiclient.Task
	for i := 0; i < 20; i++ {
		doneTasks = append(doneTasks, tuiclient.Task{
			ID:    fmt.Sprintf("task-done-%d", i),
			Title: fmt.Sprintf("Done Task %d", i),
			State: "done",
		})
	}

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = doneTasks
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to done column (5 rights from in_progress at index 2)
	for i := 0; i < 3; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model = m.(*BoardModel)
	}

	if model.selectedColumn != 5 {
		t.Fatalf("Expected selectedColumn 5 (done), got %d", model.selectedColumn)
	}

	// Check initial state: should be at first task with scrollOffset 0
	if model.selectedIndex != 0 {
		t.Errorf("Expected selectedIndex 0, got %d", model.selectedIndex)
	}
	if model.scrollOffset != 0 {
		t.Errorf("Expected scrollOffset 0, got %d", model.scrollOffset)
	}

	// Navigate down several times
	for i := 0; i < 8; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = m.(*BoardModel)
	}

	// After navigating down, we should have scrolled
	if model.selectedIndex != 8 {
		t.Errorf("Expected selectedIndex 8, got %d", model.selectedIndex)
	}
	// scrollOffset should be > 0 because we scrolled
	if model.scrollOffset <= 0 {
		t.Errorf("Expected scrollOffset > 0 after navigating down, got %d", model.scrollOffset)
	}

	// Verify the selected task is visible in the rendered output
	output := model.View()
	if !strings.Contains(output, "Done Task 8") {
		t.Errorf("Expected 'Done Task 8' (selected task) in view output, got:\n%s", output)
	}
}

// TestBoardModel_DoneTabScrollingRendering tests that scrolling in the done tab changes the rendered visible window.
// This verifies that items initially off-screen become visible after scrolling.
func TestBoardModel_DoneTabScrollingRendering(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	// Set height=10: line budget = 10 - 5 = 5 lines.
	// Done tasks render 2 lines each (task + assignee), so visibleHeight = 5 / 2 = 2 tasks.
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	model = m.(*BoardModel)

	// Create many done tasks (with assignees so they render 2 lines each) to force scrolling
	assignee := "agent-1"
	var doneTasks []tuiclient.Task
	for i := 0; i < 15; i++ {
		doneTasks = append(doneTasks, tuiclient.Task{
			ID:       fmt.Sprintf("task-done-%d", i),
			Title:    fmt.Sprintf("Done Task %d", i),
			State:    "done",
			Assignee: &assignee,
		})
	}

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = doneTasks
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to done column (3 rights from in_progress at index 2)
	for i := 0; i < 3; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model = m.(*BoardModel)
	}

	// Get initial rendering: should show tasks 0-1 (2 tasks visible with height=10)
	initialOutput := model.View()

	// Verify initial state shows tasks 0-1
	if !strings.Contains(initialOutput, "Done Task 0") {
		t.Errorf("Expected 'Done Task 0' in initial view, got:\n%s", initialOutput)
	}
	if !strings.Contains(initialOutput, "Done Task 1") {
		t.Errorf("Expected 'Done Task 1' in initial view, got:\n%s", initialOutput)
	}

	// Verify Done Task 2 is NOT visible initially (it's below the fold)
	if strings.Contains(initialOutput, "Done Task 2") {
		t.Errorf("Expected 'Done Task 2' NOT in initial view (should be off-screen), got:\n%s", initialOutput)
	}

	// Now scroll down by selecting a task further down (move down 2 times to reach task-done-2)
	for i := 0; i < 2; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = m.(*BoardModel)
	}

	// Get rendering after scrolling
	scrolledOutput := model.View()

	// After scrolling to task 2, the window should show tasks 1-2
	if !strings.Contains(scrolledOutput, "Done Task 1") {
		t.Errorf("Expected 'Done Task 1' in scrolled view, got:\n%s", scrolledOutput)
	}
	if !strings.Contains(scrolledOutput, "Done Task 2") {
		t.Errorf("Expected 'Done Task 2' in scrolled view, got:\n%s", scrolledOutput)
	}

	// But Done Task 0 should no longer be visible (scrolled out of view)
	if strings.Contains(scrolledOutput, "Done Task 0") {
		t.Errorf("Expected 'Done Task 0' NOT in scrolled view (should be scrolled out), got:\n%s", scrolledOutput)
	}
}

// TestBoardModel_ScrollResetOnColumnChange tests that switching columns resets scroll to top.
// Verify that when changing to a different column, both scrollOffset and selectedIndex reset to 0.
func TestBoardModel_ScrollResetOnColumnChange(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	// Set a small height to force scrolling
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 10})
	model = m.(*BoardModel)

	// Create many tasks in done column
	var doneTasks []tuiclient.Task
	for i := 0; i < 15; i++ {
		doneTasks = append(doneTasks, tuiclient.Task{
			ID:    fmt.Sprintf("task-done-%d", i),
			Title: fmt.Sprintf("Done Task %d", i),
			State: "done",
		})
	}

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = doneTasks
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to done column
	for i := 0; i < 3; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model = m.(*BoardModel)
	}

	// Scroll down several times
	for i := 0; i < 6; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = m.(*BoardModel)
	}

	// Verify we've scrolled
	if model.selectedIndex == 0 {
		t.Fatalf("Expected selectedIndex > 0 after scrolling, got 0")
	}
	if model.scrollOffset == 0 {
		t.Fatalf("Expected scrollOffset > 0 after scrolling, got 0")
	}

	// Now navigate left (back to review or approved column)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)

	// Both scrollOffset and selectedIndex should be reset to 0
	if model.scrollOffset != 0 {
		t.Errorf("Expected scrollOffset to reset to 0 on column change, got %d", model.scrollOffset)
	}
	if model.selectedIndex != 0 {
		t.Errorf("Expected selectedIndex to reset to 0 on column change, got %d", model.selectedIndex)
	}
}

// TestBoardModel_ArchiveTaskFlow_ConfirmY tests the full archive task flow with 'y' confirmation.
func TestBoardModel_ArchiveTaskFlow_ConfirmY(t *testing.T) {
	var callLog []string
	var capturedArchiveID string

	mockClient := &tuiclient.MockClient{
		ArchiveTaskFunc: func(ctx context.Context, id string) error {
			callLog = append(callLog, "archive")
			capturedArchiveID = id
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			// After archive, the task should be gone from its column
			return []tuiclient.Task{
				{ID: "backlog-task-1", Title: "Backlog Task", State: "backlog"},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Set up board with tasks in different columns
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{{ID: "backlog-task-1", Title: "Backlog Task", State: "backlog"}}
	bucketed["ready"] = []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready"}}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "ip-task-1", Title: "IP Task", State: "in_progress"}}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Select the ready task
	model.selectedTaskID = "ready-task-1"
	model.selectedColumn = 1 // ready column

	// Press 'z' to start archive
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	model = m.(*BoardModel)

	if model.mode != modeArchiveTaskConfirm {
		t.Fatalf("Expected modeArchiveTaskConfirm after 'z', got mode %d", model.mode)
	}

	// Verify the confirm prompt is shown
	output := model.View()
	if !strings.Contains(output, "Archive") {
		t.Errorf("Expected archive confirm prompt in view, got:\n%s", output)
	}

	// Press 'y' to confirm
	m, archiveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after confirm, got mode %d", model.mode)
	}

	// Execute the archive command
	model = executeReviewCmd(t, model, archiveCmd)

	// Verify call order: archive → refetch
	if len(callLog) != 2 {
		t.Fatalf("Expected 2 calls (archive, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "archive" {
		t.Errorf("Expected first call to be 'archive', got %q", callLog[0])
	}
	if callLog[1] != "refetch" {
		t.Errorf("Expected second call to be 'refetch', got %q", callLog[1])
	}

	// Verify archive was called with correct task ID
	if capturedArchiveID != "ready-task-1" {
		t.Errorf("ArchiveTask: expected task ID ready-task-1, got %q", capturedArchiveID)
	}

	// Verify the task is no longer in the ready column
	if len(model.tasks["ready"]) != 0 {
		t.Errorf("Expected ready column to be empty after archive, got %d tasks", len(model.tasks["ready"]))
	}
}

// TestBoardModel_ArchiveTaskFlow_ConfirmN tests that pressing 'n' cancels archive.
func TestBoardModel_ArchiveTaskFlow_ConfirmN(t *testing.T) {
	var archiveCalled bool

	mockClient := &tuiclient.MockClient{
		ArchiveTaskFunc: func(ctx context.Context, id string) error {
			archiveCalled = true
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready"}}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready"}}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	model.selectedTaskID = "ready-task-1"
	model.selectedColumn = 1

	// Press 'z' to start archive
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	model = m.(*BoardModel)

	if model.mode != modeArchiveTaskConfirm {
		t.Fatalf("Expected modeArchiveTaskConfirm, got mode %d", model.mode)
	}

	// Press 'n' to cancel
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after cancel, got mode %d", model.mode)
	}

	if archiveCalled {
		t.Error("Expected ArchiveTask not to be called after 'n'")
	}
}

// TestBoardModel_ArchiveTaskNoBareKeypress tests that archive requires pressing 'z' then confirming.
func TestBoardModel_ArchiveTaskNoBareKeypress(t *testing.T) {
	var archiveCalled bool

	mockClient := &tuiclient.MockClient{
		ArchiveTaskFunc: func(ctx context.Context, id string) error {
			archiveCalled = true
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready"}}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready"}}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	model.selectedTaskID = "ready-task-1"
	model.selectedColumn = 1

	// Pressing any key other than 'z' should not trigger archive
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	model = m.(*BoardModel)

	if model.mode == modeArchiveTaskConfirm {
		t.Error("Expected mode to not be modeArchiveTaskConfirm after pressing 'a'")
	}

	if archiveCalled {
		t.Error("Expected ArchiveTask not to be called without explicit 'z' press")
	}
}

// TestBoardModel_ArchiveProjectFlow_ConfirmY tests the full archive project flow with 'y' confirmation.
func TestBoardModel_ArchiveProjectFlow_ConfirmY(t *testing.T) {
	var callLog []string
	var capturedArchiveProjectID string

	mockClient := &tuiclient.MockClient{
		ArchiveProjectFunc: func(ctx context.Context, id string) error {
			callLog = append(callLog, "archive")
			capturedArchiveProjectID = id
			return nil
		},
		ListProjectsFunc: func(ctx context.Context, options ...tuiclient.ProjectListOption) ([]tuiclient.Project, error) {
			callLog = append(callLog, "refetch-projects")
			// After archive, project-2 is gone
			return []tuiclient.Project{
				{ID: "project-1", Name: "Project 1"},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project1 := tuiclient.Project{ID: "project-1", Name: "Project 1"}
	project2 := tuiclient.Project{ID: "project-2", Name: "Project 2"}

	model := NewBoardModel(mockClient, config, project1)
	model.width = 80
	model.height = 24

	// Initialize with projects
	projects := []tuiclient.Project{project1, project2}
	m, _ := model.Update(projectsFetchedMsg{projects: projects})
	model = m.(*BoardModel)

	// Open project switcher with 'P'
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'P'}})
	model = m.(*BoardModel)

	if model.mode != modeProjectSwitch {
		t.Fatalf("Expected modeProjectSwitch after 'P', got mode %d", model.mode)
	}

	// Select project 2 with down arrow
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m.(*BoardModel)

	// Press 'z' to start archive
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	model = m.(*BoardModel)

	if model.mode != modeArchiveProjectConfirm {
		t.Fatalf("Expected modeArchiveProjectConfirm after 'z', got mode %d", model.mode)
	}

	// Verify the confirm prompt is shown
	output := model.View()
	if !strings.Contains(output, "Archive project?") {
		t.Errorf("Expected 'Archive project?' prompt in view, got:\n%s", output)
	}

	// Press 'y' to confirm
	m, archiveCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after confirm, got mode %d", model.mode)
	}

	// Execute the archive command
	if archiveCmd != nil {
		resultMsg := archiveCmd()
		m, _ = model.Update(resultMsg)
		model = m.(*BoardModel)
	}

	// Verify archive was called with correct project ID
	if capturedArchiveProjectID != "project-2" {
		t.Errorf("ArchiveProject: expected project ID project-2, got %q", capturedArchiveProjectID)
	}
}

// TestBoardModel_ArchiveProjectFlow_CancelWithN tests that pressing 'n' cancels project archive.
func TestBoardModel_ArchiveProjectFlow_CancelWithN(t *testing.T) {
	var archiveCalled bool

	mockClient := &tuiclient.MockClient{
		ArchiveProjectFunc: func(ctx context.Context, id string) error {
			archiveCalled = true
			return nil
		},
		ListProjectsFunc: func(ctx context.Context, options ...tuiclient.ProjectListOption) ([]tuiclient.Project, error) {
			return []tuiclient.Project{
				{ID: "project-1", Name: "Project 1"},
				{ID: "project-2", Name: "Project 2"},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project1 := tuiclient.Project{ID: "project-1", Name: "Project 1"}
	project2 := tuiclient.Project{ID: "project-2", Name: "Project 2"}

	model := NewBoardModel(mockClient, config, project1)
	model.width = 80
	model.height = 24

	// Initialize with projects
	projects := []tuiclient.Project{project1, project2}
	m, _ := model.Update(projectsFetchedMsg{projects: projects})
	model = m.(*BoardModel)

	// Open project switcher
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'P'}})
	model = m.(*BoardModel)

	// Move to second project
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m.(*BoardModel)

	// Press 'z' to start archive
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	model = m.(*BoardModel)

	if model.mode != modeArchiveProjectConfirm {
		t.Fatalf("Expected modeArchiveProjectConfirm, got mode %d", model.mode)
	}

	// Press 'n' to cancel
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after cancel, got mode %d", model.mode)
	}

	if archiveCalled {
		t.Error("Expected ArchiveProject not to be called after 'n'")
	}
}

// TestBoardModel_HoldTaskFlow_ConfirmY tests that pressing 'y' in hold confirm mode holds a task.
func TestBoardModel_HoldTaskFlow_ConfirmY(t *testing.T) {
	var callLog []string
	var capturedHoldID string

	mockClient := &tuiclient.MockClient{
		HoldTaskFunc: func(ctx context.Context, id string) error {
			callLog = append(callLog, "hold")
			capturedHoldID = id
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			// After hold, refetch returns the same tasks (no state change for held)
			return []tuiclient.Task{
				{ID: "ready-task-1", Title: "Ready Task", State: "ready", Held: true},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Set up board with a task in ready state
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready", Held: false}}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Select the ready task
	model.selectedTaskID = "ready-task-1"
	model.selectedColumn = 1 // ready column

	// Press 't' to start hold
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	model = m.(*BoardModel)

	if model.mode != modeHoldTaskConfirm {
		t.Fatalf("Expected modeHoldTaskConfirm after 't', got mode %d", model.mode)
	}

	// Verify the confirm prompt is shown
	output := model.View()
	if !strings.Contains(output, "Hold") {
		t.Errorf("Expected hold confirm prompt in view, got:\n%s", output)
	}

	// Press 'y' to confirm
	m, holdCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after confirm, got mode %d", model.mode)
	}

	// Execute the hold command
	model = executeReviewCmd(t, model, holdCmd)

	// Verify call order: hold → refetch
	if len(callLog) != 2 {
		t.Fatalf("Expected 2 calls (hold, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "hold" {
		t.Errorf("Expected first call to be 'hold', got %q", callLog[0])
	}
	if callLog[1] != "refetch" {
		t.Errorf("Expected second call to be 'refetch', got %q", callLog[1])
	}

	// Verify hold was called with correct task ID
	if capturedHoldID != "ready-task-1" {
		t.Errorf("HoldTask: expected task ID ready-task-1, got %q", capturedHoldID)
	}
}

// TestBoardModel_ReleaseTaskFlow_ConfirmY tests that pressing 'y' in release confirm mode releases a held task.
func TestBoardModel_ReleaseTaskFlow_ConfirmY(t *testing.T) {
	var callLog []string
	var capturedReleaseID string

	mockClient := &tuiclient.MockClient{
		ReleaseTaskFunc: func(ctx context.Context, id string) error {
			callLog = append(callLog, "release")
			capturedReleaseID = id
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			callLog = append(callLog, "refetch")
			// After release, the task is no longer held
			return []tuiclient.Task{
				{ID: "ready-task-1", Title: "Ready Task", State: "ready", Held: false},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Set up board with a held task in ready state
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready", Held: true}}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Select the held task
	model.selectedTaskID = "ready-task-1"
	model.selectedColumn = 1 // ready column

	// Press 't' to start release (since task is held)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	model = m.(*BoardModel)

	if model.mode != modeReleaseTaskConfirm {
		t.Fatalf("Expected modeReleaseTaskConfirm after 't' on held task, got mode %d", model.mode)
	}

	// Verify the confirm prompt is shown
	output := model.View()
	if !strings.Contains(output, "Release") {
		t.Errorf("Expected release confirm prompt in view, got:\n%s", output)
	}

	// Press 'y' to confirm
	m, releaseCmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after confirm, got mode %d", model.mode)
	}

	// Execute the release command
	model = executeReviewCmd(t, model, releaseCmd)

	// Verify call order: release → refetch
	if len(callLog) != 2 {
		t.Fatalf("Expected 2 calls (release, refetch), got %d: %v", len(callLog), callLog)
	}
	if callLog[0] != "release" {
		t.Errorf("Expected first call to be 'release', got %q", callLog[0])
	}
	if callLog[1] != "refetch" {
		t.Errorf("Expected second call to be 'refetch', got %q", callLog[1])
	}

	// Verify release was called with correct task ID
	if capturedReleaseID != "ready-task-1" {
		t.Errorf("ReleaseTask: expected task ID ready-task-1, got %q", capturedReleaseID)
	}
}

// TestBoardModel_HoldTaskFlow_ConfirmN tests that pressing 'n' cancels hold.
func TestBoardModel_HoldTaskFlow_ConfirmN(t *testing.T) {
	var holdCalled bool

	mockClient := &tuiclient.MockClient{
		HoldTaskFunc: func(ctx context.Context, id string) error {
			holdCalled = true
			return nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "ready-task-1", Title: "Ready Task", State: "ready"},
			}, nil
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test Project"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{{ID: "ready-task-1", Title: "Ready Task", State: "ready", Held: false}}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	model.selectedTaskID = "ready-task-1"
	model.selectedColumn = 1

	// Press 't' to start hold
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'t'}})
	model = m.(*BoardModel)

	if model.mode != modeHoldTaskConfirm {
		t.Fatalf("Expected modeHoldTaskConfirm after 't', got mode %d", model.mode)
	}

	// Press 'n' to cancel
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("Expected modeNormal after cancel, got mode %d", model.mode)
	}

	if holdCalled {
		t.Error("Expected HoldTask not to be called after 'n'")
	}
}

// TestBoardModel_ModelVisibilityInList tests that the model is visible in task list rows.
func TestBoardModel_ModelVisibilityInList(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-opus", Title: "Opus Task", State: "ready", Model: "opus"},
			{ID: "task-sonnet", Title: "Sonnet Task", State: "ready", Model: "sonnet"},
			{ID: "task-haiku", Title: "Haiku Task", State: "ready", Model: "haiku"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Simulate the task fetch
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{
		{ID: "task-opus", Title: "Opus Task", State: "ready", Model: "opus"},
		{ID: "task-sonnet", Title: "Sonnet Task", State: "ready", Model: "sonnet"},
		{ID: "task-haiku", Title: "Haiku Task", State: "ready", Model: "haiku"},
	}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to ready column
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyLeft})
	model = m.(*BoardModel)

	output := model.View()

	// Verify that model badges are visible in the list
	if !strings.Contains(output, "[opus]") {
		t.Errorf("Expected '[opus]' badge in output, got:\n%s", output)
	}
	if !strings.Contains(output, "[sonnet]") {
		t.Errorf("Expected '[sonnet]' badge in output, got:\n%s", output)
	}
	if !strings.Contains(output, "[haiku]") {
		t.Errorf("Expected '[haiku]' badge in output, got:\n%s", output)
	}
}

// TestDetailView_ModelVisibility tests that the model is visible in the task detail view.
func TestDetailView_ModelVisibility(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Set up tasks in the board first
	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "task-1", Title: "Test Task", State: "in_progress", Model: "opus"}}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = []tuiclient.Task{}
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Press enter to enter detail view mode (should set mode to modeDetail)
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = m.(*BoardModel)

	// Now simulate task detail fetch
	m, _ = model.Update(detailFetchedMsg{
		task: tuiclient.TaskDetail{
			ID:    "task-1",
			Title: "Test Task",
			State: "in_progress",
			Model: "opus",
			Spec:  "This is a test spec",
		},
		documents: []tuiclient.Document{},
		events:    []tuiclient.Event{},
	})
	model = m.(*BoardModel)

	output := model.View()

	// Verify that model is visible in the detail view
	if !strings.Contains(output, "Model: opus") {
		t.Errorf("Expected 'Model: opus' in detail output, got:\n%s", output)
	}
}

// TestBoardModel_ProjectSwitcher_ShowsNewProjects tests that opening the project
// switcher fetches a fresh project list and displays newly-created projects without restart.
func TestBoardModel_ProjectSwitcher_ShowsNewProjects(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		Tasks: []tuiclient.Task{
			{ID: "task-1", Title: "Task 1", State: "in_progress"},
		},
	}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project1 := tuiclient.Project{ID: "project-1", Name: "Project 1"}
	project2 := tuiclient.Project{ID: "project-2", Name: "Project 2"}
	project3 := tuiclient.Project{ID: "project-3", Name: "Project 3"}

	model := NewBoardModel(mockClient, config, project1)
	model.width = 80
	model.height = 24

	// Initialize with initial projects (project1 and project2)
	initialProjects := []tuiclient.Project{project1, project2}
	m, _ := model.Update(projectsFetchedMsg{projects: initialProjects})
	model = m.(*BoardModel)

	// Initialize with tasks
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range stateOrder {
		bucketed[state] = []tuiclient.Task{}
	}
	bucketed["in_progress"] = []tuiclient.Task{{ID: "task-1", Title: "Task 1", State: "in_progress"}}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Verify initial state: only 2 projects
	if len(model.projects) != 2 {
		t.Errorf("Expected initial 2 projects, got %d", len(model.projects))
	}

	// Press 'P' to open project switcher — this should issue a fetchProjects
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'P'}})
	model = m.(*BoardModel)

	if model.mode != modeProjectSwitch {
		t.Errorf("Expected mode to be modeProjectSwitch, got %d", model.mode)
	}

	// Verify that a command was returned (fetchProjects)
	if cmd == nil {
		t.Errorf("Expected fetchProjects command to be returned, got nil")
	}

	// Simulate the fresh project list from server with a new project (project3)
	newProjects := []tuiclient.Project{project1, project2, project3}
	m, _ = model.Update(projectsFetchedMsg{projects: newProjects})
	model = m.(*BoardModel)

	// Verify that the projects list was updated
	if len(model.projects) != 3 {
		t.Errorf("Expected 3 projects after fetch, got %d", len(model.projects))
	}

	if model.projects[2].ID != project3.ID {
		t.Errorf("Expected third project to be %s, got %s", project3.ID, model.projects[2].ID)
	}

	// Verify the current project selection is preserved (should be project1)
	if model.projectSwitchIndex != 0 {
		t.Errorf("Expected projectSwitchIndex to be 0 (project1), got %d", model.projectSwitchIndex)
	}

	// Verify the rendered View contains the new project
	output := model.View()
	if !strings.Contains(output, "Project 3") {
		t.Errorf("Expected 'Project 3' to appear in project switcher View(), got:\n%s", output)
	}
}

// TestBoardModel_RenderWindowingAndScroll tests that getVisibleTaskHeight correctly returns
// a task count (not a line count), and that scrolling advances scrollOffset when navigating
// past the visible window, revealing later tasks without overflow.
func TestBoardModel_RenderWindowingAndScroll(t *testing.T) {
	mockClient := &tuiclient.MockClient{}

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "testuser",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(mockClient, config, project)

	// Create a tall column with many tasks and assignees (2 lines each when rendered).
	// Height=14 gives us (14-5)/2 = 4 tasks visible.
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 14})
	model = m.(*BoardModel)

	// Create 10 tasks in the done column; each will render 2 lines (task + assignee).
	assignees := make([]*string, 10)
	tasks := make([]tuiclient.Task, 10)
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("agent-%d", i)
		assignees[i] = &name
		tasks[i] = tuiclient.Task{
			ID:       fmt.Sprintf("task-%d", i),
			Title:    fmt.Sprintf("Task %d", i),
			State:    stateDone,
			Assignee: assignees[i],
		}
	}

	bucketed := make(map[string][]tuiclient.Task)
	bucketed["backlog"] = []tuiclient.Task{}
	bucketed["ready"] = []tuiclient.Task{}
	bucketed["in_progress"] = []tuiclient.Task{}
	bucketed["review"] = []tuiclient.Task{}
	bucketed["approved"] = []tuiclient.Task{}
	bucketed["done"] = tasks
	bucketed["blocked"] = []tuiclient.Task{}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Navigate to done column (5 right presses from in_progress at index 2)
	for i := 0; i < 3; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyRight})
		model = m.(*BoardModel)
	}

	// Verify we're in the done column
	if model.selectedColumn != 5 {
		t.Fatalf("Expected selectedColumn 5 (done), got %d", model.selectedColumn)
	}

	// Verify getVisibleTaskHeight returns 4 (not 9).
	visibleHeight := model.getVisibleTaskHeight()
	if visibleHeight != 4 {
		t.Errorf("Expected visibleHeight=4 (9 lines / 2 per task), got %d", visibleHeight)
	}

	// Verify initial selection is task-0 with scrollOffset=0
	if model.selectedTaskID != "task-0" {
		t.Errorf("Expected initial selection task-0, got %s", model.selectedTaskID)
	}
	if model.scrollOffset != 0 {
		t.Errorf("Expected initial scrollOffset=0, got %d", model.scrollOffset)
	}

	// Verify initial render shows only tasks 0-3 (4 tasks, no overflow).
	output := model.View()
	if !strings.Contains(output, "task-0") {
		t.Errorf("Expected task-0 in initial render, got:\n%s", output)
	}
	if !strings.Contains(output, "task-3") {
		t.Errorf("Expected task-3 in initial render, got:\n%s", output)
	}
	// task-4 should NOT be in the render (it's below the fold).
	if strings.Contains(output, "task-4") {
		t.Errorf("Expected task-4 NOT in initial render (below fold), got:\n%s", output)
	}

	// Navigate down to task-1 (no scroll yet, still in the window).
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
	model = m.(*BoardModel)
	if model.selectedTaskID != "task-1" {
		t.Errorf("Expected selection task-1, got %s", model.selectedTaskID)
	}
	if model.scrollOffset != 0 {
		t.Errorf("Expected scrollOffset=0 (task-1 in window), got %d", model.scrollOffset)
	}

	// Navigate down to task-4 (4 downs from task-0).
	for i := 0; i < 3; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = m.(*BoardModel)
	}
	if model.selectedTaskID != "task-4" {
		t.Errorf("Expected selection task-4, got %s", model.selectedTaskID)
	}

	// Now scrollOffset should have advanced to show task-4 in the window.
	// With visibleHeight=4 and selectedIndex=4, scrollOffset = 4 - 4 + 1 = 1.
	if model.scrollOffset != 1 {
		t.Errorf("Expected scrollOffset=1 (to show task-4), got %d", model.scrollOffset)
	}

	// Verify render now shows tasks 1-4 (window starts at scrollOffset=1).
	output = model.View()
	if !strings.Contains(output, "task-1") {
		t.Errorf("Expected task-1 in scrolled render, got:\n%s", output)
	}
	if !strings.Contains(output, "task-4") {
		t.Errorf("Expected task-4 in scrolled render, got:\n%s", output)
	}
	// task-0 should NOT be visible anymore (scrolled out of window).
	if strings.Contains(output, "task-0") {
		t.Errorf("Expected task-0 NOT in scrolled render (scrolled out), got:\n%s", output)
	}
	// task-5 should NOT be visible (still below the fold).
	if strings.Contains(output, "task-5") {
		t.Errorf("Expected task-5 NOT in scrolled render (below fold), got:\n%s", output)
	}

	// Continue navigating down to task-9 (the last task).
	for i := 0; i < 5; i++ {
		m, _ = model.Update(tea.KeyMsg{Type: tea.KeyDown})
		model = m.(*BoardModel)
	}
	if model.selectedTaskID != "task-9" {
		t.Errorf("Expected selection task-9, got %s", model.selectedTaskID)
	}

	// Verify the final scroll position shows the tail of the list (tasks 6-9).
	// With 10 total tasks and visibleHeight=4, max scrollOffset = 10 - 4 = 6.
	expectedScrollOffset := 6
	if model.scrollOffset != expectedScrollOffset {
		t.Errorf("Expected scrollOffset=%d at task-9, got %d", expectedScrollOffset, model.scrollOffset)
	}

	output = model.View()
	if !strings.Contains(output, "task-6") {
		t.Errorf("Expected task-6 in final render, got:\n%s", output)
	}
	if !strings.Contains(output, "task-9") {
		t.Errorf("Expected task-9 in final render, got:\n%s", output)
	}
	// Earlier tasks should be out of view.
	if strings.Contains(output, "task-5") {
		t.Errorf("Expected task-5 NOT in final render (scrolled out), got:\n%s", output)
	}
}
