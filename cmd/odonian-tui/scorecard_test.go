package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
	tea "github.com/charmbracelet/bubbletea"
)

// TestBoardModel_ScorecardMode_Normal tests the normal scorecard flow: press 'c', loading state, then data display.
func TestBoardModel_ScorecardMode_Normal(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		GetResearchReviewerScorecardsFunc: func(ctx context.Context, projectID string) (tuiclient.ReviewerScorecards, error) {
			return tuiclient.ReviewerScorecards{
				Scorecards: []tuiclient.ReviewerScorecard{
					{
						Model: "opus",
						FindingsRaised: map[string]int{
							"p1": 5,
							"p2": 3,
						},
						FindingsHeld:       4,
						FindingsWithdrawn:  2,
						FindingsUnresolved: 1,
						TotalReviewRounds:  15,
						SampleSize:         10,
					},
				},
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

	// Press 'c' to enter scorecard mode
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model = m.(*BoardModel)

	// Check that we're in scorecard mode and showing loading state
	if model.mode != modeScorecards {
		t.Errorf("expected mode to be modeScorecards, got %d", model.mode)
	}

	view := model.View()
	if !strings.Contains(view, "Loading scorecards…") {
		t.Errorf("expected 'Loading scorecards…' in view during loading state, got:\n%s", view)
	}

	// Execute the fetch command to get the scorecardFetchedMsg
	if cmd == nil {
		t.Fatal("expected a cmd from pressing 'c'")
	}

	msg := cmd()
	scorecardMsg, ok := msg.(scorecardFetchedMsg)
	if !ok {
		t.Fatalf("expected scorecardFetchedMsg, got %T", msg)
	}

	// Process the fetched scorecards
	m, _ = model.Update(scorecardMsg)
	model = m.(*BoardModel)

	// Check that the view now shows the scorecard content
	view = model.View()
	if !strings.Contains(view, "opus") {
		t.Errorf("expected 'opus' in view after fetch, got:\n%s", view)
	}
	if !strings.Contains(view, "P1: 5") {
		t.Errorf("expected 'P1: 5' in view after fetch, got:\n%s", view)
	}
	if !strings.Contains(view, "Held: 4") {
		t.Errorf("expected 'Held: 4' in view after fetch, got:\n%s", view)
	}

	// Test pressing 'esc' to return to board
	m, _ = model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	model = m.(*BoardModel)

	if model.mode != modeNormal {
		t.Errorf("expected mode to be modeNormal after esc, got %d", model.mode)
	}

	// Test reopening scorecards after exiting - should show loading state
	m, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model = m.(*BoardModel)

	view = model.View()
	if !strings.Contains(view, "Loading scorecards…") {
		t.Errorf("expected 'Loading scorecards…' in view when reopening scorecards, got:\n%s", view)
	}
}

// TestBoardModel_ScorecardMode_Empty tests the empty scorecard case.
func TestBoardModel_ScorecardMode_Empty(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		GetResearchReviewerScorecardsFunc: func(ctx context.Context, projectID string) (tuiclient.ReviewerScorecards, error) {
			return tuiclient.ReviewerScorecards{
				Scorecards: []tuiclient.ReviewerScorecard{},
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

	// Press 'c' to enter scorecard mode
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model = m.(*BoardModel)

	if cmd == nil {
		t.Fatal("expected a cmd from pressing 'c'")
	}

	// Execute the fetch command
	msg := cmd()
	scorecardMsg, ok := msg.(scorecardFetchedMsg)
	if !ok {
		t.Fatalf("expected scorecardFetchedMsg, got %T", msg)
	}

	// Process the fetched empty scorecards
	m, _ = model.Update(scorecardMsg)
	model = m.(*BoardModel)

	view := model.View()
	if !strings.Contains(view, "No research tasks") {
		t.Errorf("expected 'No research tasks' in view for empty scorecards, got:\n%s", view)
	}
}

// TestBoardModel_ScorecardMode_Sparse tests the sparse data warning.
func TestBoardModel_ScorecardMode_Sparse(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		GetResearchReviewerScorecardsFunc: func(ctx context.Context, projectID string) (tuiclient.ReviewerScorecards, error) {
			return tuiclient.ReviewerScorecards{
				Scorecards: []tuiclient.ReviewerScorecard{
					{
						Model: "sonnet",
						FindingsRaised: map[string]int{
							"p1": 1,
						},
						FindingsHeld:       0,
						FindingsWithdrawn:  0,
						FindingsUnresolved: 1,
						TotalReviewRounds:  1,
						SampleSize:         1,
					},
				},
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

	// Press 'c' to enter scorecard mode
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model = m.(*BoardModel)

	if cmd == nil {
		t.Fatal("expected a cmd from pressing 'c'")
	}

	// Execute the fetch command
	msg := cmd()
	scorecardMsg, ok := msg.(scorecardFetchedMsg)
	if !ok {
		t.Fatalf("expected scorecardFetchedMsg, got %T", msg)
	}

	// Process the fetched sparse scorecards
	m, _ = model.Update(scorecardMsg)
	model = m.(*BoardModel)

	view := model.View()
	if !strings.Contains(view, "sonnet") {
		t.Errorf("expected 'sonnet' in view for sparse data, got:\n%s", view)
	}
	if !strings.Contains(view, "Small sample size") {
		t.Errorf("expected 'Small sample size' warning in view for sparse data, got:\n%s", view)
	}
	if !strings.Contains(view, "P1: 1") {
		t.Errorf("expected 'P1: 1' in view for sparse data, got:\n%s", view)
	}
}

// TestBoardModel_ScorecardMode_APIError tests the API error case.
func TestBoardModel_ScorecardMode_APIError(t *testing.T) {
	mockClient := &tuiclient.MockClient{
		GetResearchReviewerScorecardsFunc: func(ctx context.Context, projectID string) (tuiclient.ReviewerScorecards, error) {
			return tuiclient.ReviewerScorecards{}, fmt.Errorf("network timeout")
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

	// Press 'c' to enter scorecard mode
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model = m.(*BoardModel)

	if model.mode != modeScorecards {
		t.Errorf("expected mode to be modeScorecards, got %d", model.mode)
	}

	if cmd == nil {
		t.Fatal("expected a cmd from pressing 'c'")
	}

	// Execute the fetch command (which will error)
	msg := cmd()
	scorecardMsg, ok := msg.(scorecardFetchedMsg)
	if !ok {
		t.Fatalf("expected scorecardFetchedMsg, got %T", msg)
	}

	if scorecardMsg.err == nil {
		t.Fatal("expected an error in scorecardFetchedMsg")
	}

	// Process the error
	m, _ = model.Update(scorecardMsg)
	model = m.(*BoardModel)

	// Check that the view shows the error message
	view := model.View()
	if !strings.Contains(view, "Error:") {
		t.Errorf("expected 'Error:' in view after API error, got:\n%s", view)
	}
	if !strings.Contains(view, "network timeout") {
		t.Errorf("expected 'network timeout' in view after API error, got:\n%s", view)
	}

	// Mode should still be scorecard to show the error
	if model.mode != modeScorecards {
		t.Errorf("expected mode to be modeScorecards after error, got %d", model.mode)
	}
}

// TestBoardModel_ScorecardMode_Refresh tests the refresh functionality.
func TestBoardModel_ScorecardMode_Refresh(t *testing.T) {
	callCount := 0
	mockClient := &tuiclient.MockClient{
		GetResearchReviewerScorecardsFunc: func(ctx context.Context, projectID string) (tuiclient.ReviewerScorecards, error) {
			callCount++
			if callCount == 1 {
				// First call returns data
				return tuiclient.ReviewerScorecards{
					Scorecards: []tuiclient.ReviewerScorecard{
						{
							Model:              "opus",
							FindingsRaised:     map[string]int{"p1": 5},
							FindingsHeld:       3,
							FindingsWithdrawn:  1,
							FindingsUnresolved: 1,
							TotalReviewRounds:  10,
							SampleSize:         10,
						},
					},
				}, nil
			}
			// Second call (refresh) returns different data
			return tuiclient.ReviewerScorecards{
				Scorecards: []tuiclient.ReviewerScorecard{
					{
						Model:              "sonnet",
						FindingsRaised:     map[string]int{"p2": 7},
						FindingsHeld:       5,
						FindingsWithdrawn:  2,
						FindingsUnresolved: 0,
						TotalReviewRounds:  12,
						SampleSize:         12,
					},
				},
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

	// Press 'c' to enter scorecard mode
	m, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	model = m.(*BoardModel)

	// Execute the fetch command
	msg := cmd()
	scorecardMsg, ok := msg.(scorecardFetchedMsg)
	if !ok {
		t.Fatalf("expected scorecardFetchedMsg, got %T", msg)
	}

	// Process the initial fetch
	m, _ = model.Update(scorecardMsg)
	model = m.(*BoardModel)

	view := model.View()
	if !strings.Contains(view, "opus") {
		t.Errorf("expected 'opus' in view after initial fetch")
	}

	// Press 'r' to refresh
	m, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	model = m.(*BoardModel)

	// Check that we're showing loading state during refresh
	view = model.View()
	if !strings.Contains(view, "Loading scorecards…") {
		t.Errorf("expected 'Loading scorecards…' in view during refresh, got:\n%s", view)
	}

	// Execute the refresh command
	if cmd == nil {
		t.Fatal("expected a cmd from pressing 'r'")
	}

	msg = cmd()
	scorecardMsg, ok = msg.(scorecardFetchedMsg)
	if !ok {
		t.Fatalf("expected scorecardFetchedMsg, got %T", msg)
	}

	// Process the refresh
	m, _ = model.Update(scorecardMsg)
	model = m.(*BoardModel)

	view = model.View()
	if !strings.Contains(view, "sonnet") {
		t.Errorf("expected 'sonnet' in view after refresh")
	}
	if strings.Contains(view, "opus") {
		t.Errorf("unexpected 'opus' in view after refresh (should be replaced by sonnet)")
	}
}

func TestBuildScorecardContent_WithData(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 30,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{
			{
				Model: "opus",
				FindingsRaised: map[string]int{
					"p1": 5,
					"p2": 10,
					"p3": 15,
				},
				FindingsHeld:                            12,
				FindingsWithdrawn:                       8,
				FindingsUnresolved:                      10,
				ApprovalsWithLaterFixedBlockingFindings: 2,
				TotalReviewRounds:                       20,
				SampleSize:                              5,
			},
		},
	}

	content := m.buildScorecardContent(scorecards)

	if len(content) == 0 {
		t.Error("expected non-empty content")
	}

	if !strings.Contains(content, "opus") {
		t.Error("expected model name 'opus' in content")
	}

	if !strings.Contains(content, "P1: 5") {
		t.Error("expected 'P1: 5' in content")
	}

	if !strings.Contains(content, "P2: 10") {
		t.Error("expected 'P2: 10' in content")
	}

	if !strings.Contains(content, "Sample Size: 5") {
		t.Error("expected sample size in content")
	}

	if !strings.Contains(content, "Held: 12") {
		t.Error("expected 'Held: 12' in content")
	}

	if !strings.Contains(content, "Unresolved: 10") {
		t.Error("expected 'Unresolved: 10' in content")
	}
}

func TestBuildScorecardContent_Empty(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 30,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{},
	}

	content := m.buildScorecardContent(scorecards)

	if !strings.Contains(content, "No research tasks") {
		t.Error("expected empty message in content")
	}
}

func TestBuildScorecardContent_SparseData(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 30,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{
			{
				Model: "sonnet",
				FindingsRaised: map[string]int{
					"p1": 0,
				},
				FindingsHeld:       0,
				FindingsWithdrawn:  0,
				FindingsUnresolved: 1,
				TotalReviewRounds:  1,
				SampleSize:         1,
			},
		},
	}

	content := m.buildScorecardContent(scorecards)

	if !strings.Contains(content, "sonnet") {
		t.Error("expected model name in content")
	}

	if !strings.Contains(content, "Sample Size: 1") {
		t.Error("expected small sample size in content")
	}

	if !strings.Contains(content, "Small sample size") {
		t.Error("expected sparse data warning in content")
	}
}
