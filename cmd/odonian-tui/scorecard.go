package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// scorecardFetchedMsg carries the result of a GetResearchReviewerScorecards call.
type scorecardFetchedMsg struct {
	scorecards tuiclient.ReviewerScorecards
	err        error
}

// fetchScorecardCmd creates a command that fetches research reviewer scorecards.
func (m *BoardModel) fetchScorecardCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		scorecards, err := m.client.GetResearchReviewerScorecards(ctx, m.project.ID)
		if err != nil {
			return scorecardFetchedMsg{err: fmt.Errorf("scorecard fetch failed: %w", err)}
		}

		return scorecardFetchedMsg{scorecards: scorecards}
	}
}

// buildScorecardContent builds the scrollable content for the scorecard view.
func (m *BoardModel) buildScorecardContent(scorecards tuiclient.ReviewerScorecards) string {
	var b strings.Builder

	b.WriteString("Research Reviewer Scorecards\n")
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n\n")

	if len(scorecards.Scorecards) == 0 {
		b.WriteString("No research tasks with reviewer scorecards found for this project.\n")
		return b.String()
	}

	for i, sc := range scorecards.Scorecards {
		if i > 0 {
			b.WriteString("\n")
		}

		b.WriteString(fmt.Sprintf("Reviewer: %s\n", sc.Model))
		b.WriteString(fmt.Sprintf("Sample Size: %d distinct tasks reviewed\n", sc.SampleSize))
		if sc.SampleSize < 5 {
			b.WriteString("⚠ Small sample size — not a reliable comparison\n")
		}
		b.WriteString(fmt.Sprintf("Total Review Rounds: %d\n", sc.TotalReviewRounds))
		b.WriteString("\n")

		// Findings raised by severity
		b.WriteString("Findings Raised:\n")
		if len(sc.FindingsRaised) > 0 {
			for _, severity := range []string{"p1", "p2", "p3"} {
				if count, ok := sc.FindingsRaised[severity]; ok {
					b.WriteString(fmt.Sprintf("  %s: %d\n", strings.ToUpper(severity), count))
				}
			}
		} else {
			b.WriteString("  (none)\n")
		}

		b.WriteString("\n")

		// Finding outcomes
		b.WriteString("Finding Outcomes:\n")
		b.WriteString(fmt.Sprintf("  Held: %d (fixed by worker or upheld on adjudication)\n", sc.FindingsHeld))
		b.WriteString(fmt.Sprintf("  Withdrawn: %d (withdrawn or overturned on adjudication)\n", sc.FindingsWithdrawn))
		b.WriteString(fmt.Sprintf("  Unresolved: %d (neither held nor withdrawn yet)\n", sc.FindingsUnresolved))

		b.WriteString("\n")

		// Approval quality
		b.WriteString(fmt.Sprintf("Rounds Approved With Later Fixed Blocking Findings: %d\n", sc.ApprovalsWithLaterFixedBlockingFindings))

		b.WriteString("\n")
		b.WriteString(strings.Repeat("─", m.width))
	}

	return b.String()
}

// initScorecardViewport initializes the scrollable viewport for the scorecard view.
func (m *BoardModel) initScorecardViewport(scorecards tuiclient.ReviewerScorecards) {
	m.lastScorecards = &scorecards
	content := m.buildScorecardContent(scorecards)

	const reservedLines = 3
	vpHeight := m.height - reservedLines
	if vpHeight < 3 {
		vpHeight = 3
	}

	vp := viewport.New(m.width, vpHeight)
	vp.SetContent(content)
	m.scorecardViewport = vp
}

// renderScorecardView renders the scorecard view.
func (m *BoardModel) renderScorecardView() string {
	var b strings.Builder

	if m.scorecardMessage != "" {
		b.WriteString(fmt.Sprintf("» %s\n", m.scorecardMessage))
	}

	if m.scorecardLoading && m.scorecardMessage == "" {
		b.WriteString("Loading scorecards…")
	} else {
		b.WriteString(m.scorecardViewport.View())
	}

	return b.String()
}

// renderScorecardHelpBar returns the help bar text for scorecard view.
func (m *BoardModel) renderScorecardHelpBar() string {
	return "esc back   ↑/↓/pgup/pgdn scroll   P switch project   r refresh   q quit"
}

// updateScorecardMode handles key presses while in scorecard view.
func (m *BoardModel) updateScorecardMode(msg tea.KeyMsg) (*BoardModel, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		return m, nil

	case "up", "k":
		m.scorecardViewport.LineUp(1)

	case "down", "j":
		m.scorecardViewport.LineDown(1)

	case "pgup":
		m.scorecardViewport.PageUp()

	case "pgdn":
		m.scorecardViewport.PageDown()

	case "r":
		m.scorecardMessage = ""
		m.scorecardViewport.SetContent("")
		m.lastScorecards = nil
		m.scorecardLoading = true
		return m, m.fetchScorecardCmd()

	case "P":
		m.mode = modeProjectSwitch
		m.projectSwitchIndex = 0
		for i, proj := range m.projects {
			if proj.ID == m.project.ID {
				m.projectSwitchIndex = i
				break
			}
		}
		m.scorecardViewport.SetContent("")
		m.lastScorecards = nil
		m.scorecardLoading = false
		return m, m.fetchProjects()

	case "q", "ctrl+c":
		return m, tea.Quit
	}

	return m, nil
}
