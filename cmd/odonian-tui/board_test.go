package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
	"github.com/boldfield/odonian/internal/tuiview"
)

// TestBoardModel_SelectionStability tests that selection is preserved across refreshes.
func TestBoardModel_SelectionStability(t *testing.T) {
	f := newBoardFixture(t, 80, 24,
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "in_progress", Kind: "implement"},
		tuiclient.Task{ID: "task-2", Title: "Task 2", State: "in_progress", Kind: "implement"},
	)

	// Verify task-1 is selected
	if f.model.selectedTaskID != "task-1" {
		t.Errorf("Expected initial selection to be task-1, got %s", f.model.selectedTaskID)
	}

	// Move down to select task-2
	f.press("down")
	if f.model.selectedTaskID != "task-2" {
		t.Errorf("Expected selection to be task-2 after down, got %s", f.model.selectedTaskID)
	}

	// Simulate a refresh with reordered tasks: task-2 now outranks task-1.
	f.setTasks(
		tuiclient.Task{ID: "task-2", Title: "Task 2", State: "in_progress", Kind: "implement", Priority: intPtr(900)},
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "in_progress", Kind: "implement"},
	)
	f.send(tickMsg{})
	view := f.model.viewContent()
	if i2, i1 := strings.Index(view, "Task 2"), strings.Index(view, "Task 1"); i2 < 0 || i1 < 0 || i2 > i1 {
		t.Fatalf("refresh did not reorder the items:\n%s", view)
	}

	// Verify task-2 is still selected
	if f.model.selectedTaskID != "task-2" {
		t.Errorf("Selection not preserved: was task-2, became %s", f.model.selectedTaskID)
	}
	if got := f.model.shell.items.SelectedTaskID(); got != "task-2" {
		t.Errorf("Items pane selection not preserved: was task-2, became %s", got)
	}
}

// TestBoardModel_ErrorState tests that errors are rendered correctly.
func TestBoardModel_ErrorState(t *testing.T) {
	// First set a successful state
	f := newBoardFixture(t, 80, 24,
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "in_progress", Kind: "implement"},
	)

	// Now simulate an error on the next poll
	f.mu.Lock()
	f.listErr = errors.New("test error message")
	f.mu.Unlock()
	f.send(tickMsg{})

	output := f.model.viewContent()
	if !strings.Contains(output, "Error:") || !strings.Contains(output, "test error message") {
		t.Errorf("Expected error message in output, got:\n%s", output)
	}
	if !strings.Contains(output, "Press 'r' to retry.") {
		t.Errorf("Expected retry prompt in output, got:\n%s", output)
	}

	// 'r' retries: once the server answers again the error is gone.
	f.mu.Lock()
	f.listErr = nil
	f.mu.Unlock()
	f.resetCalls()
	f.press("r")
	if len(f.listCalls()) == 0 {
		t.Errorf("Expected 'r' to refetch the tasks, got no ListTasks call")
	}
	output = f.model.viewContent()
	if strings.Contains(output, "test error message") || strings.Contains(output, "to retry.") || f.model.error != "" {
		t.Errorf("Expected the error cleared after retry, got %q:\n%s", f.model.error, output)
	}
}

// TestBoardModel_DisappearedTaskSelection tests that when the selected task disappears,
// the cursor lands on the positionally-nearest remaining task, not the first.
func TestBoardModel_DisappearedTaskSelection(t *testing.T) {
	// First fetch with three tasks
	f := newBoardFixture(t, 80, 24,
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "in_progress", Kind: "implement"},
		tuiclient.Task{ID: "task-2", Title: "Task 2", State: "in_progress", Kind: "implement"},
		tuiclient.Task{ID: "task-3", Title: "Task 3", State: "in_progress", Kind: "implement"},
	)

	// Select task-2 (index 1)
	f.press("down")
	if f.model.selectedTaskID != "task-2" {
		t.Errorf("Expected selection to be task-2, got %s", f.model.selectedTaskID)
	}

	// Simulate refresh where task-2 is REMOVED (only task-1 and task-3 remain)
	f.setTasks(
		tuiclient.Task{ID: "task-1", Title: "Task 1", State: "in_progress", Kind: "implement"},
		tuiclient.Task{ID: "task-3", Title: "Task 3", State: "in_progress", Kind: "implement"},
	)
	f.send(tickMsg{})

	// Should have selected task-3 (at clamped index 1, which is now the second task)
	// not task-1 (the first task).
	if f.model.selectedTaskID != "task-3" {
		t.Errorf("Expected selection to move to task-3 (nearest), got %s", f.model.selectedTaskID)
	}
	if got := f.model.shell.items.indexOf(f.model.shell.items.SelectedTaskID()); got != 1 {
		t.Errorf("Expected selected index to be 1, got %d", got)
	}
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

// extractStateFromOptions extracts the State field a ListTasks call was made with, if any.
func extractStateFromOptions(options ...tuiclient.TaskListOption) string {
	opts := &tuiclient.TaskListOptions{}
	for _, opt := range options {
		opt(opts)
	}
	return opts.State
}

// filterTasksByState filters tasks to those matching state. An empty state returns all tasks.
func filterTasksByState(tasks []tuiclient.Task, state string) []tuiclient.Task {
	if state == "" {
		return tasks
	}
	var filtered []tuiclient.Task
	for _, task := range tasks {
		if task.State == state {
			filtered = append(filtered, task)
		}
	}
	return filtered
}

// TestBoardModel_TickGeneration verifies the no-fork invariant:
//   - Init arms exactly ONE tick.
//   - A tickMsg arms exactly ONE tick (the re-arm) and issues a fetch.
//   - 'r' (manual refresh) arms ZERO ticks (issues a fetch only).
//   - A fetch result, successful or failed, arms ZERO ticks.
//
// The fixture's newTickCmd counts arming calls instead of using real timers.
// This test WILL FAIL if a tick is re-armed from the 'r' handler or from a fetch result.
func TestBoardModel_TickGeneration(t *testing.T) {
	f := newBoardFixture(t, 80, 24)

	// --- Init arms exactly one tick ---
	f.resetCalls()
	initMsgs := f.run(f.model.Init())
	if f.ticks != 1 {
		t.Errorf("Init: expected exactly 1 tick armed, got %d", f.ticks)
	}

	// --- A fetch result arms ZERO ticks ---
	var fetched []tea.Msg
	for _, msg := range initMsgs {
		if _, ok := msg.(shellTasksMsg); ok {
			fetched = append(fetched, msg)
		}
	}
	if len(fetched) != 1 {
		t.Fatalf("Init: expected one task fetch, got %v", initMsgs)
	}
	f.resetCalls()
	for _, msg := range f.step(fetched[0]) {
		if _, isTick := msg.(tickArmedMsg); isTick {
			t.Errorf("fetch result returned a cmd that produced a tick (tick fork regression)")
		}
	}
	if f.ticks != 0 {
		t.Errorf("fetch result: expected 0 ticks armed, got %d (tick fork regression)", f.ticks)
	}

	// --- 'r' (manual refresh) arms ZERO ticks ---
	f.resetCalls()
	refreshMsgs := f.step(key("r"))
	if f.ticks != 0 {
		t.Errorf("'r' key: expected 0 ticks armed, got %d (tick fork regression)", f.ticks)
	}
	hasTasksFetched := false
	for _, msg := range refreshMsgs {
		switch msg.(type) {
		case tasksFetchedMsg:
			hasTasksFetched = true
		case tickArmedMsg:
			t.Errorf("'r' key cmd: produced a tick (tick fork regression)")
		}
	}
	if !hasTasksFetched {
		t.Errorf("'r' key cmd: expected a tasksFetchedMsg, got none (msgs: %v)", refreshMsgs)
	}
	f.send(refreshMsgs...)

	// --- tickMsg arms exactly ONE tick and issues a fetch ---
	f.resetCalls()
	tickMsgs := f.step(tickMsg{})
	if f.ticks != 1 {
		t.Errorf("tickMsg: expected exactly 1 tick armed, got %d", f.ticks)
	}
	hasFetchFromTick, hasTickFromTick := false, false
	for _, msg := range tickMsgs {
		switch msg.(type) {
		case shellTasksMsg:
			hasFetchFromTick = true
		case tickArmedMsg:
			hasTickFromTick = true
		}
	}
	if !hasFetchFromTick {
		t.Errorf("tickMsg batch: expected a shellTasksMsg, got none (msgs: %v)", tickMsgs)
	}
	if !hasTickFromTick {
		t.Errorf("tickMsg batch: expected a re-armed tick, got none (msgs: %v)", tickMsgs)
	}

	// --- A failed fetch arms ZERO ticks ---
	f.resetCalls()
	for _, msg := range []tea.Msg{
		shellTasksMsg{projectID: "p1", err: errors.New("network error")},
		tasksFetchedMsg{err: errors.New("network error")},
	} {
		for _, out := range f.step(msg) {
			if _, isTick := out.(tickArmedMsg); isTick {
				t.Errorf("%T(error) returned a cmd that produced a tick (tick fork regression)", msg)
			}
		}
	}
	if f.ticks != 0 {
		t.Errorf("failed fetch: expected 0 ticks armed, got %d (tick fork regression)", f.ticks)
	}
}

// extractFieldsFromOptions extracts the Fields field a ListTasks call was made with, if any.
func extractFieldsFromOptions(options ...tuiclient.TaskListOption) string {
	opts := &tuiclient.TaskListOptions{}
	for _, opt := range options {
		opt(opts)
	}
	return opts.Fields
}

// TestBoardModel_PerStatePollingWithSummaryFields verifies the per-state poll contract:
// a tick issues exactly one WithState(...)+fields=summary ListTasks call per active state
// (backlog, ready, in_progress, checks, review, approved, blocked) plus failed, and one
// claimable+fields=summary call, leaving done and abandoned unpolled; manual refresh
// issues one such call per state across all states.
func TestBoardModel_PerStatePollingWithSummaryFields(t *testing.T) {
	f := newBoardFixture(t, 80, 24,
		tuiclient.Task{ID: "task-active", Title: "Active", State: "in_progress", Kind: "implement"},
		tuiclient.Task{ID: "task-done", Title: "Done", State: "done", Kind: "implement"},
		tuiclient.Task{ID: "task-failed", Title: "Failed", State: "failed", Kind: "implement"},
		tuiclient.Task{ID: "task-abandoned", Title: "Abandoned", State: "abandoned", Kind: "implement"},
	)

	assertCalls := func(t *testing.T, calls []listCall, wantStates []string, wantClaimable int) {
		t.Helper()
		if len(calls) != len(wantStates)+wantClaimable {
			t.Fatalf("expected %d ListTasks calls, got %d: %v", len(wantStates)+wantClaimable, len(calls), calls)
		}
		seen := make(map[string]int)
		claimable := 0
		for _, call := range calls {
			if call.fields != "summary" {
				t.Errorf("expected fields=summary, got %q (state=%q)", call.fields, call.state)
			}
			if call.claimable {
				claimable++
				continue
			}
			seen[call.state]++
		}
		for _, state := range wantStates {
			if seen[state] != 1 {
				t.Errorf("expected exactly 1 call for state %q, got %d", state, seen[state])
			}
		}
		if claimable != wantClaimable {
			t.Errorf("expected %d claimable calls, got %d", wantClaimable, claimable)
		}
	}

	// --- Tick polls only the active states and failed ---
	f.resetCalls()
	f.send(tickMsg{})
	assertCalls(t, f.listCalls(), append(append([]string{}, activeStates...), stateFailed), 1)

	if len(f.model.tasks["failed"]) != 1 || f.model.tasks["failed"][0].ID != "task-failed" {
		t.Errorf("expected failed tasks polled with task-failed, got %v", f.model.tasks["failed"])
	}
	if view := f.model.viewContent(); !strings.Contains(view, "Active") || !strings.Contains(view, "Failed") {
		t.Errorf("expected the polled tasks in the work items:\n%s", view)
	}

	// --- Manual refresh ('r') fetches every state ---
	f.resetCalls()
	f.press("r")
	assertCalls(t, f.listCalls(), stateOrder, 0)
}

// --- TUI-4: Review action tests ---

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
	m, _ = model.Update(tea.KeyPressMsg{Code: 'P', Text: "P"})
	model = m.(*BoardModel)

	if model.mode != modeProjectSwitch {
		t.Errorf("Expected mode to be modeProjectSwitch, got %d", model.mode)
	}

	// Navigate to second project
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = m.(*BoardModel)

	if model.projectSwitchIndex != 1 {
		t.Errorf("Expected projectSwitchIndex to be 1, got %d", model.projectSwitchIndex)
	}

	// Select the second project (press enter)
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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

	if model.selectedColumn != columnIndex(stateInProgress) {
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
	m, _ = model.Update(tea.KeyPressMsg{Code: 'P', Text: "P"})
	model = m.(*BoardModel)

	// Select the same project (first one is already selected)
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
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

// TestBoardModel_ProjectSwitchFetchesTerminalColumns verifies that switching projects issues
// a full refresh (all 9 states, not just the active-only poll set), so terminal columns
// (done, failed, abandoned) are populated for the newly selected project instead of staying
// blank until the next manual refresh.
func TestBoardModel_ProjectSwitchFetchesTerminalColumns(t *testing.T) {
	type listTasksCall struct {
		state  string
		fields string
	}
	var calls []listTasksCall

	project2Tasks := []tuiclient.Task{
		{ID: "task-active", Title: "Active", State: "in_progress"},
		{ID: "task-done", Title: "Done", State: "done"},
		{ID: "task-failed", Title: "Failed", State: "failed"},
		{ID: "task-abandoned", Title: "Abandoned", State: "abandoned"},
	}

	mockClient := &tuiclient.MockClient{
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			state := extractStateFromOptions(options...)
			calls = append(calls, listTasksCall{state: state, fields: extractFieldsFromOptions(options...)})
			return filterTasksByState(project2Tasks, state), nil
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

	m, _ := model.Update(projectsFetchedMsg{projects: []tuiclient.Project{project1, project2}})
	model = m.(*BoardModel)

	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range stateOrder {
		bucketed[state] = []tuiclient.Task{}
	}
	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Open the project switcher, move to project2, and select it.
	m, _ = model.Update(tea.KeyPressMsg{Code: 'P', Text: "P"})
	model = m.(*BoardModel)
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = m.(*BoardModel)
	calls = nil
	m, switchCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	if switchCmd == nil {
		t.Fatalf("expected a fetch command after switching projects, got nil")
	}
	for _, msg := range runCmd(switchCmd) {
		m, _ = model.Update(msg)
		model = m.(*BoardModel)
	}

	if len(calls) != len(stateOrder) {
		t.Fatalf("expected %d ListTasks calls (one per state) on project switch, got %d: %v", len(stateOrder), len(calls), calls)
	}
	for _, call := range calls {
		if call.fields != "summary" {
			t.Errorf("expected fields=summary on project-switch fetch, got %q (state=%q)", call.fields, call.state)
		}
	}

	if len(model.tasks["done"]) != 1 || model.tasks["done"][0].ID != "task-done" {
		t.Errorf("expected done column populated after project switch, got %v", model.tasks["done"])
	}
	if len(model.tasks["failed"]) != 1 || model.tasks["failed"][0].ID != "task-failed" {
		t.Errorf("expected failed column populated after project switch, got %v", model.tasks["failed"])
	}
	if len(model.tasks["abandoned"]) != 1 || model.tasks["abandoned"][0].ID != "task-abandoned" {
		t.Errorf("expected abandoned column populated after project switch, got %v", model.tasks["abandoned"])
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
	m, cmd := model.Update(tea.KeyPressMsg{Code: 'P', Text: "P"})
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
	output := model.View().Content
	if !strings.Contains(output, "Project 3") {
		t.Errorf("Expected 'Project 3' to appear in project switcher View(), got:\n%s", output)
	}
}

// TestBoardModel_HeldMarkerRendering verifies that [HELD] marker is rendered on board rows.
// findColumnIndex returns the column index for a given state name in stateOrder.
func findColumnIndex(state string) int {
	for i, s := range stateOrder {
		if s == state {
			return i
		}
	}
	return -1
}

// TestSetPriorityFlow tests the complete set priority flow from board mode.
func TestBoardModel_SetPriorityFlow_ValidInput(t *testing.T) {
	var capturedSetReq tuiclient.SetTaskPriorityRequest
	var capturedTaskID string

	mockClient := &tuiclient.MockClient{
		SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			capturedTaskID = taskID
			capturedSetReq = req
			return tuiclient.PriorityChange{
				TopicAnchorID: "topic-1",
				Priority:      req.Priority,
				OldPriority:   500,
			}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(750)},
			}, nil
		},
	}

	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Task", State: "ready"},
	})

	// Press 'i' to enter set priority input mode
	m, _ := model.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	model = m.(*BoardModel)

	if model.mode != modeSetPriorityInput {
		t.Fatalf("Expected modeSetPriorityInput, got %d", model.mode)
	}

	// Verify view shows input prompt
	output := model.View().Content
	if !strings.Contains(output, "Set priority — enter a value (1-1000)") {
		t.Errorf("Expected set priority prompt in view, got:\n%s", output)
	}

	// Type priority 750
	for _, ch := range "750" {
		m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		model = m.(*BoardModel)
	}

	// Press enter to move to reason input
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeSetPriorityReason {
		t.Fatalf("Expected modeSetPriorityReason, got %d", model.mode)
	}

	// Verify view shows reason prompt
	output = model.View().Content
	if !strings.Contains(output, "reason (required)") {
		t.Errorf("Expected reason prompt in view, got:\n%s", output)
	}

	// Type reason
	for _, ch := range "important feature" {
		m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		model = m.(*BoardModel)
	}

	// Press enter to move to confirmation
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeSetPriorityConfirm {
		t.Fatalf("Expected modeSetPriorityConfirm, got %d", model.mode)
	}

	// Verify confirm view shows the priority value
	output = model.View().Content
	if !strings.Contains(output, "Set priority to 750") {
		t.Errorf("Expected priority value 750 in confirm view, got:\n%s", output)
	}

	// Confirm with 'y'
	m, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Mode should be back to normal after confirmation
	if model.mode != modeNormal {
		t.Fatalf("Expected modeNormal after confirm, got %d", model.mode)
	}

	// Execute the command
	model = executeReviewCmd(t, model, cmd)

	// Verify API call arguments
	if capturedTaskID != "task-1" {
		t.Errorf("Expected task ID task-1, got %q", capturedTaskID)
	}
	if capturedSetReq.Priority != 750 {
		t.Errorf("Expected priority 750, got %d", capturedSetReq.Priority)
	}
	if capturedSetReq.Reason != "important feature" {
		t.Errorf("Expected reason 'important feature', got %q", capturedSetReq.Reason)
	}
	if capturedSetReq.ActionKey == "" {
		t.Errorf("Expected non-empty ActionKey, got empty string")
	}
	if capturedSetReq.Actor != "tui-worker" {
		t.Errorf("Expected actor 'tui-worker', got %q", capturedSetReq.Actor)
	}

	// Verify success message shows new priority
	if model.error != "" {
		t.Errorf("Expected no error after success, got %q", model.error)
	}
	if !strings.Contains(model.statusMessage, "priority set to 750") {
		t.Errorf("Expected status message containing 'priority set to 750', got %q", model.statusMessage)
	}

	// Verify priority state was cleared after success
	if model.lastPriorityAttempt.key != "" {
		t.Errorf("Expected remembered attempt cleared after success, got %q", model.lastPriorityAttempt.key)
	}
	if model.pendingPriority != 0 {
		t.Errorf("Expected pendingPriority cleared after success, got %d", model.pendingPriority)
	}
}

// TestSetPriorityFlow tests invalid priority inputs.
func TestBoardModel_SetPriorityFlow_InvalidInputs(t *testing.T) {
	mockClient := &tuiclient.MockClient{}
	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Task", State: "ready"},
	})

	tests := []struct {
		input   string
		wantErr string
	}{
		{"", "priority value required"},
		{"0", "priority must be an integer between 1 and 1000"},
		{"1001", "priority must be an integer between 1 and 1000"},
		{"-5", "priority must be an integer between 1 and 1000"},
		{"abc", "priority must be an integer between 1 and 1000"},
		{"5.5", "priority must be an integer between 1 and 1000"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("input_%s", tt.input), func(t *testing.T) {
			// Reset for each test
			model = buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready"},
			})

			// Press 'i' to enter set priority mode
			m, _ := model.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
			model = m.(*BoardModel)

			// Type the invalid input
			for _, ch := range tt.input {
				m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
				model = m.(*BoardModel)
			}

			// Press enter (should reject)
			m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			model = m.(*BoardModel)

			// Should still be in input mode with error hint
			if model.mode != modeSetPriorityInput {
				t.Errorf("Expected to stay in modeSetPriorityInput, got %d", model.mode)
			}
			if model.inputHint != tt.wantErr {
				t.Errorf("Expected hint %q, got %q", tt.wantErr, model.inputHint)
			}

			// Verify error is visible in view
			output := model.View().Content
			if !strings.Contains(output, tt.wantErr) {
				t.Errorf("Expected error %q in view output, got:\n%s", tt.wantErr, output)
			}
		})
	}
}

// TestMoveToFrontFlow tests the move to front flow.
func TestBoardModel_MoveToFrontFlow(t *testing.T) {
	var capturedReq tuiclient.MoveTaskToFrontRequest
	var capturedTaskID string

	mockClient := &tuiclient.MockClient{
		MoveTaskToFrontFunc: func(ctx context.Context, taskID string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
			capturedTaskID = taskID
			capturedReq = req
			return tuiclient.PriorityChange{
				TopicAnchorID: "topic-1",
				Priority:      1500, // Server assigns a value > 1000
				OldPriority:   500,
			}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(1500)},
			}, nil
		},
	}

	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Task", State: "ready"},
	})

	// Press 'w' to enter move to front reason mode
	m, _ := model.Update(tea.KeyPressMsg{Code: 'w', Text: "w"})
	model = m.(*BoardModel)

	if model.mode != modeMoveToFrontReason {
		t.Fatalf("Expected modeMoveToFrontReason, got %d", model.mode)
	}

	// Verify view shows reason prompt
	output := model.View().Content
	if !strings.Contains(output, "Move to front — reason (required)") {
		t.Errorf("Expected move to front prompt in view, got:\n%s", output)
	}

	// Type reason
	for _, ch := range "blocking other tasks" {
		m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		model = m.(*BoardModel)
	}

	// Press enter to move to confirmation
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeMoveToFrontConfirm {
		t.Fatalf("Expected modeMoveToFrontConfirm, got %d", model.mode)
	}

	// Verify confirm view
	output = model.View().Content
	if !strings.Contains(output, "Move") || !strings.Contains(output, "to front?") {
		t.Errorf("Expected move to front confirm prompt in view, got:\n%s", output)
	}

	// Confirm with 'y'
	m, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Mode should be back to normal
	if model.mode != modeNormal {
		t.Fatalf("Expected modeNormal after confirm, got %d", model.mode)
	}

	// Execute the command
	model = executeReviewCmd(t, model, cmd)

	// Verify API call
	if capturedTaskID != "task-1" {
		t.Errorf("Expected task ID task-1, got %q", capturedTaskID)
	}
	if capturedReq.Reason != "blocking other tasks" {
		t.Errorf("Expected reason 'blocking other tasks', got %q", capturedReq.Reason)
	}
	if capturedReq.ActionKey == "" {
		t.Errorf("Expected non-empty ActionKey, got empty string")
	}

	// Verify success shows priority > 1000
	if model.error != "" {
		t.Errorf("Expected no error after success, got %q", model.error)
	}
	if !strings.Contains(model.statusMessage, "priority 1500") {
		t.Errorf("Expected priority 1500 in success message, got status: %q", model.statusMessage)
	}
}

// TestActionKeyPreservation tests that action keys are preserved across retries.
func TestBoardModel_ActionKeyPreservation(t *testing.T) {
	var keys []string

	mockClient := &tuiclient.MockClient{
		SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			keys = append(keys, req.ActionKey)
			return tuiclient.PriorityChange{
				Priority:    req.Priority,
				OldPriority: 500,
			}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready"},
			}, nil
		},
	}

	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Task", State: "ready"},
	})

	// First attempt: press 'i' and go through the flow
	m, _ := model.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	model = m.(*BoardModel)

	// Type priority
	for _, ch := range "750" {
		m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		model = m.(*BoardModel)
	}

	// Move to reason
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Type reason and confirm
	for _, ch := range "test" {
		m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		model = m.(*BoardModel)
	}
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Confirm
	m, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Execute
	model = executeReviewCmd(t, model, cmd)

	// Verify key was used
	if len(keys) == 0 {
		t.Fatalf("Expected action key to be sent to API")
	}
	if keys[0] == "" {
		t.Errorf("Expected non-empty action key to be sent to API")
	}
}

// TestBoardModel_ResetPriorityFlow tests the full reset priority flow.
func TestBoardModel_ResetPriorityFlow(t *testing.T) {
	var capturedReq tuiclient.SetTaskPriorityRequest
	var capturedTaskID string

	mockClient := &tuiclient.MockClient{
		SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			capturedTaskID = taskID
			capturedReq = req
			return tuiclient.PriorityChange{
				TopicAnchorID: "topic-1",
				Priority:      500,
				OldPriority:   750,
			}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(500)},
			}, nil
		},
	}

	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(750)},
	})

	// Press 'e' to enter reset priority reason mode
	m, _ := model.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	model = m.(*BoardModel)

	if model.mode != modeResetPriorityReason {
		t.Fatalf("Expected modeResetPriorityReason, got %d", model.mode)
	}

	// Verify view shows reset reason prompt
	output := model.View().Content
	if !strings.Contains(output, "Reset priority to 500 — reason (required)") {
		t.Errorf("Expected reset priority reason prompt in view, got:\n%s", output)
	}

	// Submitting an empty reason keeps the prompt and shows a hint
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)
	if model.mode != modeResetPriorityReason {
		t.Fatalf("Expected to remain in modeResetPriorityReason on empty reason, got %d", model.mode)
	}
	output = model.View().Content
	if !strings.Contains(output, "hint:") || !strings.Contains(output, "Reset priority to 500 — reason (required)") {
		t.Errorf("Expected reset reason prompt with hint after empty reason, got:\n%s", output)
	}

	// Type reason
	for _, ch := range "no longer urgent" {
		m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		model = m.(*BoardModel)
	}

	// Press enter to move to confirmation
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	if model.mode != modeResetPriorityConfirm {
		t.Fatalf("Expected modeResetPriorityConfirm, got %d", model.mode)
	}

	// Verify confirm view
	output = model.View().Content
	if !strings.Contains(output, "Reset priority to 500 for task-1? [y/N]") {
		t.Errorf("Expected reset priority confirm prompt in view, got:\n%s", output)
	}
	if !strings.Contains(output, "(y to confirm, n/esc to cancel)") {
		t.Errorf("Expected confirm help in reset confirm view, got:\n%s", output)
	}

	// Confirm with 'y'
	m, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Mode should be back to normal
	if model.mode != modeNormal {
		t.Fatalf("Expected modeNormal after confirm, got %d", model.mode)
	}

	// Execute the command
	model = executeReviewCmd(t, model, cmd)

	// Verify the refreshed board shows the reset value
	output = model.View().Content
	if !strings.Contains(output, "P:500") {
		t.Errorf("Expected refreshed list to show P:500 after reset, got:\n%s", output)
	}

	// Verify API call
	if capturedTaskID != "task-1" {
		t.Errorf("Expected task ID task-1, got %q", capturedTaskID)
	}
	if capturedReq.Priority != 500 {
		t.Errorf("Expected priority 500, got %d", capturedReq.Priority)
	}
	if capturedReq.Reason != "no longer urgent" {
		t.Errorf("Expected reason 'no longer urgent', got %q", capturedReq.Reason)
	}
	if capturedReq.ActionKey == "" {
		t.Errorf("Expected non-empty ActionKey, got empty string")
	}
}

// TestDetailView_ResetPriorityFlow tests reset priority from detail view.
func TestDetailView_ResetPriorityFlow(t *testing.T) {
	var capturedReq tuiclient.SetTaskPriorityRequest

	mockClient := &tuiclient.MockClient{
		SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			capturedReq = req
			return tuiclient.PriorityChange{
				Priority:    500,
				OldPriority: 750,
			}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(500)},
			}, nil
		},
		GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{
				ID:    "task-1",
				Title: "Task",
				State: "ready",
			}, nil
		},
	}

	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(750)},
	})

	// Enter detail view
	m, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Press 'e' for reset priority from detail
	m, _ = model.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	model = m.(*BoardModel)

	if model.mode != modeResetPriorityReason {
		t.Fatalf("Expected modeResetPriorityReason, got %d", model.mode)
	}

	// Type reason
	for _, ch := range "test" {
		m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
		model = m.(*BoardModel)
	}

	// Confirm
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)
	m, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	model = m.(*BoardModel)

	// Execute
	model = executeReviewCmd(t, model, cmd)

	// Verify
	if capturedReq.Priority != 500 {
		t.Errorf("Expected priority 500, got %d", capturedReq.Priority)
	}
}

// TestDetailView_PriorityFlowsRenderPrompts drives Set priority, Move to front and Reset
// priority from the detail view and asserts every step's View() output: the detail help
// bar advertises the key, each input/confirm overlay is visible, cancelling returns to the
// rendered detail view, and a confirmed action lands on the refreshed board with status.
func TestDetailView_PriorityFlowsRenderPrompts(t *testing.T) {
	tests := []struct {
		name          string
		startKey      rune
		helpLabel     string
		value         string
		valuePrompt   string
		reasonPrompt  string
		confirmPrompt string
		resultValue   int64
		wantStatus    string
	}{
		{
			name:          "set priority",
			startKey:      'i',
			helpLabel:     "i set priority",
			value:         "1000",
			valuePrompt:   "Set priority — enter a value (1-1000)",
			reasonPrompt:  "Set priority — reason (required)",
			confirmPrompt: "Set priority to 1000 for task-1? [y/N]",
			resultValue:   1000,
			wantStatus:    "priority set to 1000 for topic-1",
		},
		{
			name:          "move to front",
			startKey:      'w',
			helpLabel:     "w move to front",
			reasonPrompt:  "Move to front — reason (required)",
			confirmPrompt: "Move task-1 to front? [y/N]",
			resultValue:   1001,
			wantStatus:    "1001",
		},
		{
			name:          "reset priority",
			startKey:      'e',
			helpLabel:     "e reset priority",
			reasonPrompt:  "Reset priority to 500 — reason (required)",
			confirmPrompt: "Reset priority to 500 for task-1? [y/N]",
			resultValue:   500,
			wantStatus:    "500",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priorityChange := tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: tt.resultValue, OldPriority: 750}
			mockClient := &tuiclient.MockClient{
				SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
					return priorityChange, nil
				},
				MoveTaskToFrontFunc: func(ctx context.Context, taskID string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
					return priorityChange, nil
				},
				ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
					return []tuiclient.Task{
						{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(tt.resultValue)},
					}, nil
				},
				GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
					return tuiclient.TaskDetail{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(750)}, nil
				},
			}

			enterDetail := func() *BoardModel {
				model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
					{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(750)},
				})
				m, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				model = m.(*BoardModel)
				if cmd != nil {
					model = executeCmd(t, model, cmd)
				}
				if model.mode != modeDetail {
					t.Fatalf("Expected modeDetail, got %d", model.mode)
				}
				return model
			}
			typeText := func(model *BoardModel, text string) *BoardModel {
				for _, ch := range text {
					m, _ := model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
					model = m.(*BoardModel)
				}
				m, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				return m.(*BoardModel)
			}
			assertView := func(model *BoardModel, want string) {
				t.Helper()
				output := model.View().Content
				if strings.TrimSpace(output) == "" {
					t.Fatalf("Expected non-empty view while expecting %q", want)
				}
				if !strings.Contains(output, want) {
					t.Errorf("Expected %q in view, got:\n%s", want, output)
				}
			}

			model := enterDetail()
			assertView(model, tt.helpLabel)
			assertView(model, "Priority: 750")

			// Walk the flow, asserting each overlay.
			m, _ := model.Update(tea.KeyPressMsg{Code: tt.startKey, Text: string(tt.startKey)})
			model = m.(*BoardModel)
			if tt.valuePrompt != "" {
				assertView(model, tt.valuePrompt)
				model = typeText(model, tt.value)
			}
			assertView(model, tt.reasonPrompt)
			model = typeText(model, "operator request")
			assertView(model, tt.confirmPrompt)
			assertView(model, "(y to confirm, n/esc to cancel)")

			// Declining returns to the detail view with its content visible.
			m, _ = model.Update(tea.KeyPressMsg{Code: 'n', Text: "n"})
			model = m.(*BoardModel)
			if model.mode != modeDetail {
				t.Fatalf("Expected modeDetail after declining, got %d", model.mode)
			}
			assertView(model, "Priority: 750")
			assertView(model, tt.helpLabel)

			// Run the flow again and confirm: the refreshed board shows status and the new value.
			model = enterDetail()
			m, _ = model.Update(tea.KeyPressMsg{Code: tt.startKey, Text: string(tt.startKey)})
			model = m.(*BoardModel)
			if tt.valuePrompt != "" {
				model = typeText(model, tt.value)
			}
			model = typeText(model, "operator request")
			m, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
			model = m.(*BoardModel)
			model = executeReviewCmd(t, model, cmd)
			if model.mode != modeNormal {
				t.Fatalf("Expected modeNormal after confirmed detail action, got %d", model.mode)
			}
			assertView(model, tt.wantStatus)
			assertView(model, fmt.Sprintf("P:%d", tt.resultValue))
			if strings.Contains(model.View().Content, "Error:") {
				t.Errorf("Expected no error screen after successful detail action, got:\n%s", model.View().Content)
			}
		})
	}
}

// TestBoardModel_ManualInputErrors tests validation of manual priority input.
func TestBoardModel_ManualInputErrors(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantError bool
		errorHint string
	}{
		{"empty", "", true, "required"},
		{"zero", "0", true, "1-1000"},
		{"too high", "1001", true, "1-1000"},
		{"non-integer", "abc", true, "integer"},
		{"float", "5.5", true, "integer"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockClient := &tuiclient.MockClient{}
			model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready"},
			})

			// Start set priority
			m, _ := model.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
			model = m.(*BoardModel)

			// Type invalid input
			for _, ch := range tt.input {
				m, _ = model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
				model = m.(*BoardModel)
			}

			// Try to proceed
			m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			model = m.(*BoardModel)

			// Should still be in input mode (not moved to reason)
			if model.mode != modeSetPriorityInput {
				t.Fatalf("Expected to remain in modeSetPriorityInput for invalid input %q", tt.input)
			}

			// Check error hint is shown
			output := model.View().Content
			if tt.wantError && !strings.Contains(output, tt.errorHint) {
				t.Errorf("Expected error hint %q in output for input %q, got:\n%s", tt.errorHint, tt.input, output)
			}
		})
	}
}

// TestDetailView_HeldPlusPriority tests display of [HELD] with priority in detail view.
func TestDetailView_HeldPlusPriority(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		GetTaskFunc: func(ctx context.Context, taskID string) (tuiclient.TaskDetail, error) {
			return tuiclient.TaskDetail{
				ID:       "task-1",
				Title:    "Held Task",
				State:    "ready",
				Held:     true,
				Priority: intPtr(750),
			}, nil
		},
	}

	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Held Task", State: "ready", Held: true, Priority: intPtr(750)},
	})

	// Enter detail view
	m, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = m.(*BoardModel)

	// Execute the fetch command
	if cmd != nil {
		model = executeCmd(t, model, cmd)
	}

	output := model.View().Content

	// Verify both priority and held status are shown
	if !strings.Contains(output, "Priority: 750") {
		t.Errorf("Expected Priority: 750 in detail view:\n%s", output)
	}
	if !strings.Contains(output, "[HELD]") {
		t.Errorf("Expected [HELD] marker in detail view:\n%s", output)
	}
}

// priorityTestFlow drives a priority action through the keyboard: the start key, optional
// manual value, reason, and the final confirmation. It returns the model and confirm command.
func priorityTestFlow(t *testing.T, model *BoardModel, startKey rune, value, reason string) (*BoardModel, tea.Cmd) {
	t.Helper()
	typeText := func(text string) {
		for _, ch := range text {
			m, _ := model.Update(tea.KeyPressMsg{Code: ch, Text: string(ch)})
			model = m.(*BoardModel)
		}
		m, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		model = m.(*BoardModel)
	}
	m, _ := model.Update(tea.KeyPressMsg{Code: startKey, Text: string(startKey)})
	model = m.(*BoardModel)
	if value != "" {
		typeText(value)
	}
	typeText(reason)
	m, cmd := model.Update(tea.KeyPressMsg{Code: 'y', Text: "y"})
	return m.(*BoardModel), cmd
}

// serverLikeIdempotency mimics the server: a key bound to one request replays that request and
// rejects (409 IDEMPOTENCY_MISMATCH) any different request using the same key.
type serverLikeIdempotency struct {
	seen map[string]tuiclient.SetTaskPriorityRequest
}

func (s *serverLikeIdempotency) check(req tuiclient.SetTaskPriorityRequest) error {
	if s.seen == nil {
		s.seen = map[string]tuiclient.SetTaskPriorityRequest{}
	}
	if prior, ok := s.seen[req.ActionKey]; ok && (prior.Priority != req.Priority || prior.Reason != req.Reason) {
		return &tuiclient.APIError{StatusCode: http.StatusConflict, Code: "IDEMPOTENCY_MISMATCH", Message: "action key already used for a different priority request"}
	}
	s.seen[req.ActionKey] = req
	return nil
}

// TestBoardModel_ActionFailureRetrySendsSameKey drives a failed Set (lost response), then an
// identical retry through the UI, and asserts both calls carry the same non-empty key.
func TestBoardModel_ActionFailureRetrySendsSameKey(t *testing.T) {
	var requests []tuiclient.SetTaskPriorityRequest
	mockClient := &tuiclient.MockClient{
		SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			requests = append(requests, req)
			if len(requests) == 1 {
				return tuiclient.PriorityChange{}, fmt.Errorf("timeout")
			}
			return tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: 750, OldPriority: 500}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(500)}}, nil
		},
	}
	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{{ID: "task-1", Title: "Task", State: "ready"}})

	model, cmd := priorityTestFlow(t, model, 'i', "750", "urgent")
	model = executeReviewCmd(t, model, cmd)
	if !strings.Contains(model.error, "set priority failed") {
		t.Fatalf("Expected failure message, got %q", model.error)
	}
	if model.lastPriorityAttempt.key == "" {
		t.Fatalf("Expected attempt kept after retryable failure")
	}
	if len(model.tasks["ready"]) != 1 {
		t.Errorf("Expected board state preserved after failure, got %v", model.tasks["ready"])
	}

	model, cmd = priorityTestFlow(t, model, 'i', "750", "urgent")
	model = executeReviewCmd(t, model, cmd)

	if len(requests) != 2 {
		t.Fatalf("Expected 2 requests, got %d", len(requests))
	}
	if requests[0].ActionKey == "" || requests[0].ActionKey != requests[1].ActionKey {
		t.Errorf("Expected identical non-empty keys, got %q and %q", requests[0].ActionKey, requests[1].ActionKey)
	}
	if requests[0].Priority != requests[1].Priority || requests[0].Reason != requests[1].Reason {
		t.Errorf("Expected identical requests, got %+v and %+v", requests[0], requests[1])
	}
	if model.lastPriorityAttempt.key != "" {
		t.Errorf("Expected attempt cleared after success")
	}
	if model.error != "" || !strings.Contains(model.statusMessage, "priority set to 750") {
		t.Errorf("Expected success status and no error, got status %q error %q", model.statusMessage, model.error)
	}
}

// TestBoardModel_ActionRetryWithChangedRequestMintsFreshKey checks a retry whose value or reason
// differs does not reuse the failed attempt's key (the server would answer IDEMPOTENCY_MISMATCH).
func TestBoardModel_ActionRetryWithChangedRequestMintsFreshKey(t *testing.T) {
	cases := []struct {
		name       string
		value      string
		reason     string
		wantNewKey bool
	}{
		{"changed value", "800", "urgent", true},
		{"changed reason", "750", "very urgent", true},
		{"identical", "750", "urgent", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idempotency := &serverLikeIdempotency{}
			var requests []tuiclient.SetTaskPriorityRequest
			mockClient := &tuiclient.MockClient{
				SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
					requests = append(requests, req)
					if err := idempotency.check(req); err != nil {
						return tuiclient.PriorityChange{}, err
					}
					if len(requests) == 1 {
						return tuiclient.PriorityChange{}, fmt.Errorf("timeout after commit")
					}
					return tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: req.Priority}, nil
				},
				ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
					return []tuiclient.Task{{ID: "task-1", Title: "Task", State: "ready"}}, nil
				},
			}
			model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{{ID: "task-1", Title: "Task", State: "ready"}})

			model, cmd := priorityTestFlow(t, model, 'i', "750", "urgent")
			model = executeReviewCmd(t, model, cmd)
			model, cmd = priorityTestFlow(t, model, 'i', tc.value, tc.reason)
			model = executeReviewCmd(t, model, cmd)

			if len(requests) != 2 {
				t.Fatalf("Expected 2 requests, got %d", len(requests))
			}
			if gotNew := requests[0].ActionKey != requests[1].ActionKey; gotNew != tc.wantNewKey {
				t.Errorf("fresh key = %v, want %v", gotNew, tc.wantNewKey)
			}
			if strings.Contains(model.error, "409") {
				t.Errorf("Retry hit idempotency mismatch: %q", model.error)
			}
		})
	}
}

// TestBoardModel_RejectedActionDropsKey checks a definitive 4xx rejection clears the remembered
// key so the operator is not stuck, and a timeout (408) keeps it.
func TestBoardModel_RejectedActionDropsKey(t *testing.T) {
	cases := []struct {
		status   int
		wantKept bool
	}{
		{http.StatusBadRequest, false},
		{http.StatusConflict, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("status_%d", tc.status), func(t *testing.T) {
			mockClient := &tuiclient.MockClient{
				MoveTaskToFrontFunc: func(ctx context.Context, taskID string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
					return tuiclient.PriorityChange{}, &tuiclient.APIError{StatusCode: tc.status, Message: "nope"}
				},
				ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
					return []tuiclient.Task{{ID: "task-1", Title: "Task", State: "ready"}}, nil
				},
			}
			model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{{ID: "task-1", Title: "Task", State: "ready"}})
			model, cmd := priorityTestFlow(t, model, 'w', "", "because")
			model = executeReviewCmd(t, model, cmd)

			if !strings.Contains(model.error, fmt.Sprintf("move to front %d", tc.status)) {
				t.Errorf("Expected real status in error, got %q", model.error)
			}
			if kept := model.lastPriorityAttempt.key != ""; kept != tc.wantKept {
				t.Errorf("key kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

// TestBoardModel_ActionKeyTiedToRequest checks keys are never shared across tasks or actions and
// that an explicit cancel drops the remembered attempt.
func TestBoardModel_ActionKeyTiedToRequest(t *testing.T) {
	var keys []string
	mockClient := &tuiclient.MockClient{
		SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			keys = append(keys, req.ActionKey)
			return tuiclient.PriorityChange{}, fmt.Errorf("timeout")
		},
		MoveTaskToFrontFunc: func(ctx context.Context, taskID string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
			keys = append(keys, req.ActionKey)
			return tuiclient.PriorityChange{}, fmt.Errorf("timeout")
		},
		ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			return []tuiclient.Task{
				{ID: "task-1", Title: "Task 1", State: "ready"},
				{ID: "task-2", Title: "Task 2", State: "ready"},
			}, nil
		},
	}
	model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
		{ID: "task-1", Title: "Task 1", State: "ready"},
		{ID: "task-2", Title: "Task 2", State: "ready"},
	})

	model, cmd := priorityTestFlow(t, model, 'i', "750", "urgent")
	model = executeReviewCmd(t, model, cmd)

	// Different action on the same task, then the same action on a different task.
	model, cmd = priorityTestFlow(t, model, 'w', "", "urgent")
	model = executeReviewCmd(t, model, cmd)
	m, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = m.(*BoardModel)
	model, cmd = priorityTestFlow(t, model, 'w', "", "urgent")
	model = executeReviewCmd(t, model, cmd)

	if len(keys) != 3 {
		t.Fatalf("Expected 3 requests, got %d", len(keys))
	}
	if keys[0] == keys[1] || keys[1] == keys[2] || keys[0] == keys[2] {
		t.Errorf("Expected distinct keys across action/task changes, got %v", keys)
	}

	// Explicit cancel clears the remembered attempt.
	m, _ = model.Update(tea.KeyPressMsg{Code: 'i', Text: "i"})
	model = m.(*BoardModel)
	m, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	model = m.(*BoardModel)
	if model.lastPriorityAttempt.key != "" || model.pendingPriority != 0 || model.pendingPriorityReason != "" {
		t.Errorf("Expected priority state cleared on cancel, got %+v", model.lastPriorityAttempt)
	}
}

// Helper function to build a board model with specific tasks
func buildBoardModelWithTasks(t *testing.T, client *tuiclient.MockClient, tasks []tuiclient.Task) *BoardModel {
	t.Helper()

	config := &tuiconfig.Config{
		URL:          "http://test",
		Token:        "test",
		Actor:        "test-actor",
		PollInterval: 100 * time.Millisecond,
	}
	project := tuiclient.Project{ID: "project-1", Name: "Test"}

	model := NewBoardModel(client, config, project)
	m, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = m.(*BoardModel)

	// Bucket tasks by state
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range []string{"backlog", "ready", "in_progress", "review", "approved", "done", "blocked", "failed", "abandoned"} {
		bucketed[state] = []tuiclient.Task{}
	}

	for _, task := range tasks {
		bucketed[task.State] = append(bucketed[task.State], task)
	}

	m, _ = model.Update(tasksFetchedMsg{tasks: bucketed})
	model = m.(*BoardModel)

	// Select the first available task
	for _, state := range []string{"backlog", "ready", "in_progress", "review"} {
		if len(bucketed[state]) > 0 {
			model.selectedTaskID = bucketed[state][0].ID
			model.selectedColumn = findColumnIndex(state)
			break
		}
	}

	return model
}

// intPtr returns a pointer to an int64
func intPtr(v int64) *int64 {
	return &v
}

// strPtr returns a pointer to a string
func strPtr(v string) *string {
	return &v
}

// TestBoardModel_SuccessfulPriorityActionKeepsRefreshedListVisible checks that after each
// successful priority action View() shows the resolved topic/value as a status line and the
// refreshed task list, and never the error screen.
func TestBoardModel_SuccessfulPriorityActionKeepsRefreshedListVisible(t *testing.T) {
	cases := []struct {
		name       string
		startKey   rune
		value      string
		wantStatus string
		wantRow    string
	}{
		{"set", 'i', "750", "priority set to 750 for topic-1", "P:750"},
		{"front", 'w', "", "moved to front with priority 1001 for topic-1", "P:1001"},
		{"reset", 'e', "", "priority reset to 500 for topic-1", "P:500"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wantPriority int64
			switch tc.startKey {
			case 'i':
				wantPriority = 750
			case 'w':
				wantPriority = 1001
			default:
				wantPriority = 500
			}
			change := tuiclient.PriorityChange{TopicAnchorID: "topic-1", Priority: wantPriority}
			mockClient := &tuiclient.MockClient{
				SetTaskPriorityFunc: func(ctx context.Context, taskID string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
					return change, nil
				},
				MoveTaskToFrontFunc: func(ctx context.Context, taskID string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
					return change, nil
				},
				ListTasksFunc: func(ctx context.Context, projectID string, options ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
					return []tuiclient.Task{
						{ID: "task-1", Title: "Refreshed task", State: "ready", Priority: intPtr(wantPriority)},
					}, nil
				},
			}
			model := buildBoardModelWithTasks(t, mockClient, []tuiclient.Task{
				{ID: "task-1", Title: "Task", State: "ready", Priority: intPtr(300)},
			})

			model, cmd := priorityTestFlow(t, model, tc.startKey, tc.value, "because")
			model = executeReviewCmd(t, model, cmd)

			output := model.View().Content
			if strings.Contains(output, "Error:") || strings.Contains(output, "Press 'r' to retry") {
				t.Errorf("Success rendered as an error:\n%s", output)
			}
			if !strings.Contains(output, tc.wantStatus) {
				t.Errorf("Expected status %q in view:\n%s", tc.wantStatus, output)
			}
			if !strings.Contains(output, "Refreshed task") || !strings.Contains(output, tc.wantRow) {
				t.Errorf("Expected refreshed list row with %q in view:\n%s", tc.wantRow, output)
			}

			m, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
			if strings.Contains(m.(*BoardModel).View().Content, tc.wantStatus) {
				t.Errorf("Status should clear on next keypress")
			}
		})
	}
}

func TestFormatChecksDuration(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) *string { s := now.Add(-d).Format(time.RFC3339Nano); return &s }
	bad := "nope"
	cases := []struct {
		in   *string
		want string
	}{
		{nil, "unknown"},
		{&bad, "unknown"},
		{ts(30 * time.Second), "30s"},
		{ts(5 * time.Minute), "5m"},
		{ts(90 * time.Minute), "1h30m"},
		{ts(-time.Minute), "0s"},
	}
	for _, c := range cases {
		if got := tuiview.FormatChecksDuration(c.in, now); got != c.want {
			t.Errorf("got %q want %q", got, c.want)
		}
	}
}
