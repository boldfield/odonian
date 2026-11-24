package main

import (
	"charm.land/lipgloss/v2"
)

// Theme defines the Lip Gloss styles for the TUI.
type Theme struct {
	// StatusStyles maps task state to its style (glyph colour).
	StatusStyles map[string]lipgloss.Style

	// Shared pane styles
	PaneTitleFocused   lipgloss.Style
	PaneTitleUnfocused lipgloss.Style
	SelectedRow        lipgloss.Style
	DimmedText         lipgloss.Style
	PaneSeparator      lipgloss.Style
}

// NewTheme creates a new theme with status styles derived from stateOrder
// and colour assignments from ANSI 16-colour values.
func NewTheme() *Theme {
	// Status styles with ANSI 16-colour values and the glyph's information.
	// Colour is redundant with glyph; no information is carried by colour alone.
	statusStyles := make(map[string]lipgloss.Style)

	// ANSI 16-colour values:
	// 1=Red, 2=Green, 3=Yellow, 4=Blue, 5=Magenta, 6=Cyan, 8=Gray, 9=BrightRed, 11=BrightYellow
	colorMap := map[string]string{
		stateBacklog:    "8",  // Gray - not promoted
		stateReady:      "4",  // Blue - ready and claimable
		stateInProgress: "3",  // Yellow - in progress
		stateChecks:     "11", // Bright yellow - in checks
		stateReview:     "5",  // Magenta - in review
		stateApproved:   "6",  // Cyan - approved, awaiting merge
		stateDone:       "2",  // Green - done
		stateBlocked:    "1",  // Red - blocked
		stateFailed:     "9",  // Bright red - failed
		stateAbandoned:  "8",  // Gray - abandoned
	}

	for state, colorCode := range colorMap {
		statusStyles[state] = lipgloss.NewStyle().
			Foreground(lipgloss.Color(colorCode))
	}

	return &Theme{
		StatusStyles:       statusStyles,
		PaneTitleFocused:   lipgloss.NewStyle().Bold(true),
		PaneTitleUnfocused: lipgloss.NewStyle(),
		SelectedRow:        lipgloss.NewStyle().Reverse(true),
		DimmedText:         lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		PaneSeparator:      lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
	}
}
