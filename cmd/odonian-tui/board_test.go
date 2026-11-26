package main

import (
	"context"
	"errors"
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
