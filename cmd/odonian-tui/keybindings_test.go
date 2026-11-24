package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
)

// TestHelpBarReviewState tests that the help bar for a review state includes approve and reject.
func TestHelpBarReviewState(t *testing.T) {
	config := &tuiconfig.Config{
		PollInterval: 100 * time.Millisecond,
	}
	m := NewBoardModel(nil, config, tuiclient.Project{ID: "test", Name: "test"})

	// Set up a task in review state
	m.tasks[stateReview] = []tuiclient.Task{
		{ID: "task1", State: stateReview, Title: "Test task"},
	}
	m.selectedColumn = columnIndex(stateReview)
	m.selectedTaskID = "task1"
	m.width = 200 // Use a wide width to ensure all bindings are shown
	m.helpModel.SetWidth(m.width)

	helpBar := m.renderHelpBar()

	// Assert that approve and reject are in the help bar (they're the main actions for review state)
	if !strings.Contains(helpBar, "approve") {
		t.Errorf("approve action not in help bar: %s", helpBar)
	}
	if !strings.Contains(helpBar, "reject") {
		t.Errorf("reject action not in help bar: %s", helpBar)
	}
}

// TestHelpBarBlockedState tests that the help bar for a blocked item shows unblock and fail, not approve.
func TestHelpBarBlockedState(t *testing.T) {
	config := &tuiconfig.Config{
		PollInterval: 100 * time.Millisecond,
	}
	m := NewBoardModel(nil, config, tuiclient.Project{ID: "test", Name: "test"})

	// Set up a task in blocked state
	m.tasks[stateBlocked] = []tuiclient.Task{
		{ID: "blocked-task", State: stateBlocked, Title: "Blocked task"},
	}
	m.selectedColumn = columnIndex(stateBlocked)
	m.selectedTaskID = "blocked-task"
	m.width = 200
	m.helpModel.SetWidth(m.width)

	helpBar := m.renderHelpBar()

	// Assert unblock and fail are in the help bar
	if !strings.Contains(helpBar, "u") || !strings.Contains(helpBar, "unblock") {
		t.Errorf("unblock action not in help bar: %s", helpBar)
	}
	if !strings.Contains(helpBar, "f") || !strings.Contains(helpBar, "fail") {
		t.Errorf("fail action not in help bar: %s", helpBar)
	}

	// Assert approve is NOT in the help bar
	if strings.Contains(helpBar, "approve") {
		t.Errorf("approve action should not be in help bar for blocked state: %s", helpBar)
	}
}

// TestHelpToggleFullHelp tests that pressing ? toggles the full help view on and off.
func TestHelpToggleFullHelp(t *testing.T) {
	config := &tuiconfig.Config{
		PollInterval: 100 * time.Millisecond,
	}
	m := NewBoardModel(nil, config, tuiclient.Project{ID: "test", Name: "test"})
	m.width = 100
	m.height = 30
	m.helpModel.SetWidth(m.width)

	// Set up a task in review state
	m.tasks[stateReview] = []tuiclient.Task{
		{ID: "task1", State: stateReview, Title: "Test task"},
	}
	m.selectedColumn = columnIndex(stateReview)
	m.selectedTaskID = "task1"

	// Initial state: full help should be false
	if m.showFullHelp {
		t.Errorf("showFullHelp should be false initially, got true")
	}

	// Press ?
	msg := tea.KeyPressMsg{
		Code: '?',
		Text: "?",
	}

	_, _ = m.updateTabs(msg)

	// After first ?, full help should be true
	if !m.showFullHelp {
		t.Errorf("showFullHelp should be true after first ?, got false")
	}

	// Press ? again
	_, _ = m.updateTabs(msg)

	// After second ?, full help should be false
	if m.showFullHelp {
		t.Errorf("showFullHelp should be false after second ?, got true")
	}
}

// TestHelpViewContent tests that the help view is rendered when showFullHelp is true.
func TestHelpViewContent(t *testing.T) {
	config := &tuiconfig.Config{
		PollInterval: 100 * time.Millisecond,
	}
	m := NewBoardModel(nil, config, tuiclient.Project{ID: "test", Name: "test"})
	m.width = 100
	m.height = 30
	m.helpModel.SetWidth(m.width)

	// Set up a task in review state
	m.tasks[stateReview] = []tuiclient.Task{
		{ID: "task1", State: stateReview, Title: "Test task"},
	}
	m.selectedColumn = columnIndex(stateReview)
	m.selectedTaskID = "task1"

	// Toggle to show full help
	m.showFullHelp = true

	content := m.viewContent()

	// Assert that the content contains the help marker
	if !strings.Contains(content, "?") || !strings.Contains(content, "close") {
		t.Logf("Help view content: %s", content)
		t.Errorf("Help view should contain '? to close' or similar")
	}

	// Should not show the normal board content
	if strings.Contains(content, m.renderTabs()) {
		t.Errorf("Help view should not show tabs")
	}
}

// TestKeyBindingsAvailableForState tests that the right bindings are available for each state.
func TestKeyBindingsAvailableForState(t *testing.T) {
	kb := DefaultKeyBindings()

	testCases := []struct {
		state         string
		expectedCount int // approximate expected number of bindings
	}{
		{state: stateBacklog, expectedCount: 15},  // promote + common bindings
		{state: stateReview, expectedCount: 16},   // approve + reject + common bindings
		{state: stateApproved, expectedCount: 15}, // bounce + common bindings
		{state: stateBlocked, expectedCount: 16},  // unblock + fail + common bindings
	}

	for _, tc := range testCases {
		t.Run(tc.state, func(t *testing.T) {
			bindings := kb.getBoardBindingsForState(tc.state)
			if len(bindings) == 0 {
				t.Errorf("expected bindings for state %s, got none", tc.state)
			}
			// Check approximate count (allowing some variance)
			if len(bindings) < tc.expectedCount-2 || len(bindings) > tc.expectedCount+2 {
				t.Logf("warning: state %s has %d bindings, expected around %d", tc.state, len(bindings), tc.expectedCount)
			}
		})
	}
}

// TestDetailModeFullHelpToggle tests that ? in detail mode toggles full help.
func TestDetailModeFullHelpToggle(t *testing.T) {
	config := &tuiconfig.Config{
		PollInterval: 100 * time.Millisecond,
	}
	m := NewBoardModel(nil, config, tuiclient.Project{ID: "test", Name: "test"})
	m.width = 100
	m.height = 30
	m.helpModel.SetWidth(m.width)

	// Set up a task in review state and enter detail mode
	reviewTask := tuiclient.TaskDetail{
		ID:    "task1",
		Title: "Test task",
		State: stateReview,
	}
	m.mode = modeDetail
	m.detailTask = reviewTask

	// Initial state: full help should be false
	if m.showFullHelp {
		t.Errorf("showFullHelp should be false initially, got true")
	}

	// Press ?
	msg := tea.KeyPressMsg{Code: '?', Text: "?"}
	_, _ = m.updateDetailMode(msg)

	// After first ?, full help should be true
	if !m.showFullHelp {
		t.Errorf("showFullHelp should be true after first ?, got false")
	}

	// Press ? again
	_, _ = m.updateDetailMode(msg)

	// After second ?, full help should be false
	if m.showFullHelp {
		t.Errorf("showFullHelp should be false after second ?, got true")
	}
}
