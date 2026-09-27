package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/tuiclient"
	tea "github.com/charmbracelet/bubbletea"
)

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

func TestUpdateScorecardMode_Escape(t *testing.T) {
	m := &BoardModel{
		mode: modeScorecards,
	}

	escMsg := tea.KeyMsg{
		Type: tea.KeyEsc,
	}

	updated, _ := m.updateScorecardMode(escMsg)

	if updated.mode != modeNormal {
		t.Errorf("expected mode to be modeNormal after esc, got %d", updated.mode)
	}
}

func TestInitScorecardViewport(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 24,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{
			{
				Model:              "opus",
				FindingsRaised:     map[string]int{"p1": 5},
				FindingsHeld:       3,
				FindingsWithdrawn:  2,
				FindingsUnresolved: 0,
				TotalReviewRounds:  5,
				SampleSize:         2,
			},
		},
	}

	m.initScorecardViewport(scorecards)

	if m.scorecardViewport.Width == 0 {
		t.Error("expected viewport width to be set")
	}

	if m.scorecardViewport.Height == 0 {
		t.Error("expected viewport height to be set")
	}

	content := m.scorecardViewport.View()
	if len(content) == 0 {
		t.Error("expected viewport to have content")
	}
}

func TestRenderScorecardView(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 24,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{
			{
				Model:          "opus",
				FindingsRaised: map[string]int{"p1": 5},
				SampleSize:     1,
			},
		},
	}

	m.initScorecardViewport(scorecards)
	rendered := m.renderScorecardView()

	if len(rendered) == 0 {
		t.Error("expected non-empty rendered content")
	}
}

func TestRenderScorecardHelpBar(t *testing.T) {
	m := &BoardModel{}
	helpBar := m.renderScorecardHelpBar()

	if !strings.Contains(helpBar, "esc back") {
		t.Error("expected 'esc back' in help bar")
	}

	if !strings.Contains(helpBar, "scroll") {
		t.Error("expected 'scroll' in help bar")
	}
}

// Test with all three severity levels
func TestBuildScorecardContent_AllSeverities(t *testing.T) {
	m := &BoardModel{
		width:  100,
		height: 30,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{
			{
				Model: "fable",
				FindingsRaised: map[string]int{
					"p1": 3,
					"p2": 7,
					"p3": 20,
				},
				FindingsHeld:                            15,
				FindingsWithdrawn:                       10,
				FindingsUnresolved:                      5,
				ApprovalsWithLaterFixedBlockingFindings: 1,
				TotalReviewRounds:                       25,
				SampleSize:                              10,
			},
		},
	}

	content := m.buildScorecardContent(scorecards)

	// Verify all severity levels are present
	if !strings.Contains(content, "P1: 3") {
		t.Error("expected 'P1: 3' in content")
	}
	if !strings.Contains(content, "P2: 7") {
		t.Error("expected 'P2: 7' in content")
	}
	if !strings.Contains(content, "P3: 20") {
		t.Error("expected 'P3: 20' in content")
	}

	// Verify outcome counts are present
	if !strings.Contains(content, "Held: 15") {
		t.Error("expected 'Held: 15' in content")
	}
	if !strings.Contains(content, "Withdrawn: 10") {
		t.Error("expected 'Withdrawn: 10' in content")
	}
}

// Test normal scorecard render with View()
func TestRenderScorecardView_Normal(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 24,
		mode:   modeScorecards,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{
			{
				Model:              "opus",
				FindingsRaised:     map[string]int{"p1": 5, "p2": 3},
				FindingsHeld:       4,
				FindingsWithdrawn:  2,
				FindingsUnresolved: 2,
				SampleSize:         10,
				TotalReviewRounds:  15,
			},
		},
	}
	m.initScorecardViewport(scorecards)

	view := m.renderScorecardView()
	if !strings.Contains(view, "opus") {
		t.Error("expected 'opus' in rendered view")
	}
	if !strings.Contains(view, "P1: 5") {
		t.Error("expected 'P1: 5' in rendered view")
	}
	if !strings.Contains(view, "Held: 4") {
		t.Error("expected 'Held: 4' in rendered view")
	}
}

// Test empty scorecard render
func TestRenderScorecardView_Empty(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 24,
		mode:   modeScorecards,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{},
	}
	m.initScorecardViewport(scorecards)

	view := m.renderScorecardView()
	if !strings.Contains(view, "No research tasks") {
		t.Error("expected 'No research tasks' in rendered view")
	}
}

// Test sparse data warning in rendered view
func TestRenderScorecardView_Sparse(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 24,
		mode:   modeScorecards,
	}

	scorecards := tuiclient.ReviewerScorecards{
		Scorecards: []tuiclient.ReviewerScorecard{
			{
				Model:              "sonnet",
				FindingsRaised:     map[string]int{"p1": 1},
				FindingsHeld:       0,
				FindingsWithdrawn:  0,
				FindingsUnresolved: 1,
				SampleSize:         1,
				TotalReviewRounds:  1,
			},
		},
	}
	m.initScorecardViewport(scorecards)

	view := m.renderScorecardView()
	if !strings.Contains(view, "sonnet") {
		t.Error("expected 'sonnet' in rendered view")
	}
	if !strings.Contains(view, "Small sample size") {
		t.Error("expected 'Small sample size' warning in rendered view")
	}
}

// Test API error in scorecard mode
func TestUpdateScorecardMode_APIError(t *testing.T) {
	m := &BoardModel{
		width:  80,
		height: 24,
		mode:   modeScorecards,
	}

	// Simulate fetching an empty scorecard with normal state
	scorecards := tuiclient.ReviewerScorecards{Scorecards: []tuiclient.ReviewerScorecard{}}
	m.initScorecardViewport(scorecards)

	// Process error message
	errMsg := scorecardFetchedMsg{err: fmt.Errorf("network error")}
	m.scorecardMessage = fmt.Sprintf("Error: %v", errMsg.err)

	view := m.renderScorecardView()
	if !strings.Contains(view, "Error:") {
		t.Error("expected error message in rendered view")
	}
	if !strings.Contains(view, "network error") {
		t.Error("expected error details in rendered view")
	}
}
