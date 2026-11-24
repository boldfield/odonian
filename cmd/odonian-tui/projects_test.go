package main

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
)

func TestProjectsPaneNavigationUp(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
		{ID: "p3", Name: "Project3"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)

	// Start at index 1
	pane.selectedIndex = 1

	// Move up
	pane.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if pane.selectedIndex != 0 {
		t.Errorf("expected selectedIndex=0, got %d", pane.selectedIndex)
	}

	// Move up again - should clamp
	pane.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if pane.selectedIndex != 0 {
		t.Errorf("expected selectedIndex to clamp at 0, got %d", pane.selectedIndex)
	}
}

func TestProjectsPaneNavigationDown(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
		{ID: "p3", Name: "Project3"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)

	// Start at index 1
	pane.selectedIndex = 1

	// Move down
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if pane.selectedIndex != 2 {
		t.Errorf("expected selectedIndex=2, got %d", pane.selectedIndex)
	}

	// Move down again - should clamp
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if pane.selectedIndex != 2 {
		t.Errorf("expected selectedIndex to clamp at 2, got %d", pane.selectedIndex)
	}
}

func TestProjectsPaneNavigationK(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)
	pane.selectedIndex = 1

	// k should move up
	pane.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if pane.selectedIndex != 0 {
		t.Errorf("expected selectedIndex=0 after k, got %d", pane.selectedIndex)
	}
}

func TestProjectsPaneNavigationJ(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)

	// j should move down
	pane.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if pane.selectedIndex != 1 {
		t.Errorf("expected selectedIndex=1 after j, got %d", pane.selectedIndex)
	}
}

func TestProjectsPaneSelectionMessage(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)
	pane.selectedIndex = 1

	// Press enter to emit selection message
	_, cmd := pane.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Error("expected command for selection message")
	}

	// Execute the command and check the message
	msg := cmd()
	selected, ok := msg.(ProjectSelected)
	if !ok {
		t.Errorf("expected ProjectSelected message, got %T", msg)
	}
	if selected.ProjectID != "p2" {
		t.Errorf("expected ProjectID=p2, got %s", selected.ProjectID)
	}
}

func TestProjectsPaneRenderFullWidth(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "ProjectOne"},
		{ID: "p2", Name: "ProjectTwo"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(tuiview.ProjectPaneWidth, 10) // Wide mode: pane width >= ProjectPaneWidth (17)

	view := pane.View()
	content := view.Content
	if !strings.Contains(content, "ProjectOne") {
		t.Error("expected full project name 'ProjectOne' in wide mode")
	}
	if !strings.Contains(content, "ProjectTwo") {
		t.Error("expected full project name 'ProjectTwo' in wide mode")
	}
}

func TestProjectsPaneRenderRailWidth(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "ProjectOne"},
		{ID: "p2", Name: "ProjectTwo"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(tuiview.ProjectRailWidth, 10) // Rail mode: pane width < ProjectPaneWidth (10)

	view := pane.View()
	content := view.Content
	// In rail mode, names should be abbreviated
	if !strings.Contains(content, "Pro") {
		t.Error("expected abbreviated project name 'Pro' in rail mode")
	}
	// Full names should not appear
	if strings.Contains(content, "ProjectOne") {
		t.Error("expected no full project names in rail mode")
	}
}

func TestProjectsPaneScrolling(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
		{ID: "p3", Name: "Project3"},
		{ID: "p4", Name: "Project4"},
		{ID: "p5", Name: "Project5"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 3) // Height of 3 (title + 2 project rows)

	// Selection at index 0 - should start at scrollOffset 0
	if pane.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0, got %d", pane.scrollOffset)
	}

	// Move down to index 1 - should not scroll yet (still visible)
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if pane.selectedIndex != 1 {
		t.Errorf("expected selectedIndex=1, got %d", pane.selectedIndex)
	}
	if pane.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0 (still visible), got %d", pane.scrollOffset)
	}

	// Move down to index 2 - should scroll to keep selection visible
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if pane.selectedIndex != 2 {
		t.Errorf("expected selectedIndex=2, got %d", pane.selectedIndex)
	}
	// Selection should be at bottom of visible area
	if pane.scrollOffset != 1 {
		t.Errorf("expected scrollOffset=1 to keep selection at bottom, got %d", pane.scrollOffset)
	}

	// Move to last item
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if pane.selectedIndex != 4 {
		t.Errorf("expected selectedIndex=4, got %d", pane.selectedIndex)
	}
	if pane.scrollOffset != 3 {
		t.Errorf("expected scrollOffset=3, got %d", pane.scrollOffset)
	}

	// Move back up - should keep selection visible
	pane.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if pane.selectedIndex != 3 {
		t.Errorf("expected selectedIndex=3, got %d", pane.selectedIndex)
	}
	// Selection should still be visible in the pane (visible rows = height - 1)
	visibleRows := pane.height - 1
	if pane.selectedIndex < pane.scrollOffset || pane.selectedIndex >= pane.scrollOffset+visibleRows {
		t.Errorf("selection not visible: selectedIndex=%d, scrollOffset=%d, visibleRows=%d", pane.selectedIndex, pane.scrollOffset, visibleRows)
	}
}

func TestProjectsPaneScrollingKeepsSelectionVisible(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
		{ID: "p3", Name: "Project3"},
		{ID: "p4", Name: "Project4"},
		{ID: "p5", Name: "Project5"},
		{ID: "p6", Name: "Project6"},
		{ID: "p7", Name: "Project7"},
		{ID: "p8", Name: "Project8"},
		{ID: "p9", Name: "Project9"},
		{ID: "p10", Name: "Project10"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 3) // Height of 3, 10 projects

	// Select the last item directly
	pane.selectedIndex = 9
	pane.ensureSelectionVisible()

	// Selection should be visible within the scroll window
	if pane.selectedIndex < pane.scrollOffset {
		t.Errorf("selection %d is above scroll offset %d", pane.selectedIndex, pane.scrollOffset)
	}
	if pane.selectedIndex >= pane.scrollOffset+pane.height {
		t.Errorf("selection %d is below scroll window (offset=%d, height=%d)", pane.selectedIndex, pane.scrollOffset, pane.height)
	}
}

func TestProjectsPaneLineWidth(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "VeryLongProjectNameThatExceedsTheWidth"},
	}

	for _, width := range []int{1, 2, 3, 10, tuiview.ProjectRailWidth, tuiview.ProjectPaneWidth} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			pane := NewProjectsPane(theme, projects)
			pane.SetSize(width, 10)

			view := pane.View()
			lines := strings.Split(strings.TrimSpace(view.Content), "\n")
			for i, line := range lines {
				lineWidth := lipgloss.Width(line)
				if lineWidth > width {
					t.Errorf("line %d display width %d exceeds pane width %d: %q", i, lineWidth, width, line)
				}
			}
		})
	}
}

func TestProjectsPaneLineWidthWithBadges(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "VeryLongProjectNameThatExceedsTheWidth"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(10, 10)

	badges := map[string]ProjectBadge{
		"p1": {Symbol: "⚠"},
	}
	pane.SetBadges(badges)

	view := pane.View()
	lines := strings.Split(strings.TrimSpace(view.Content), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // Skip title
		}
		width := lipgloss.Width(line)
		if width > 10 {
			t.Errorf("line %d with badge exceeds pane width: %q (width=%d)", i, line, width)
		}
		if !strings.Contains(line, "⚠") {
			t.Error("expected badge to be present in line")
		}
	}
}

func TestProjectsPaneUTF8Truncation(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "日本語プロジェクト名"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(10, 10)

	badges := map[string]ProjectBadge{
		"p1": {Symbol: "⚠"},
	}
	pane.SetBadges(badges)

	view := pane.View()
	lines := strings.Split(strings.TrimSpace(view.Content), "\n")
	for i, line := range lines {
		if i == 0 {
			continue // Skip title
		}
		// Should not contain invalid UTF-8 sequences
		if !utf8.ValidString(line) {
			t.Errorf("line contains invalid UTF-8: %q", line)
		}
		width := lipgloss.Width(line)
		if width > 10 {
			t.Errorf("UTF-8 line exceeds width: %q (width=%d)", line, width)
		}
	}
}

func TestProjectsPaneNavigationSendsMessage(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
		{ID: "p3", Name: "Project3"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)
	pane.selectedIndex = 0

	_, cmd := pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd == nil {
		t.Error("expected command for down navigation")
	}
	if pane.selectedIndex != 1 {
		t.Errorf("expected selectedIndex=1 after down, got %d", pane.selectedIndex)
	}

	msg := cmd()
	selected, ok := msg.(ProjectSelected)
	if !ok {
		t.Errorf("expected ProjectSelected message, got %T", msg)
	}
	if selected.ProjectID != "p2" {
		t.Errorf("expected ProjectID=p2, got %s", selected.ProjectID)
	}

	_, cmd = pane.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if cmd == nil {
		t.Error("expected command for up navigation")
	}
	if pane.selectedIndex != 0 {
		t.Errorf("expected selectedIndex=0 after up, got %d", pane.selectedIndex)
	}

	msg = cmd()
	selected, ok = msg.(ProjectSelected)
	if !ok {
		t.Errorf("expected ProjectSelected message, got %T", msg)
	}
	if selected.ProjectID != "p1" {
		t.Errorf("expected ProjectID=p1, got %s", selected.ProjectID)
	}
}

func TestProjectsPaneNavigationRenderedOutput(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)

	// Check initial render has cursor at first project
	view1 := pane.View()
	content1 := view1.Content
	lines1 := strings.Split(content1, "\n")
	if len(lines1) < 2 {
		t.Fatal("expected at least 2 lines (title + project)")
	}
	if !strings.Contains(lines1[1], "▸") {
		t.Error("expected cursor at first project in line 2")
	}
	if !strings.Contains(lines1[1], "Project1") {
		t.Error("expected Project1 in first project line")
	}

	// Navigate down
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	view2 := pane.View()
	content2 := view2.Content
	lines2 := strings.Split(content2, "\n")
	if len(lines2) < 3 {
		t.Fatal("expected at least 3 lines (title + 2 projects)")
	}

	// First project should not have cursor
	if strings.Contains(lines2[1], "▸ Project1") {
		t.Error("expected no cursor at Project1 after down navigation")
	}

	// Second project should have cursor
	if !strings.Contains(lines2[2], "▸") {
		t.Error("expected cursor at Project2 after down navigation")
	}
	if !strings.Contains(lines2[2], "Project2") {
		t.Error("expected Project2 in second project line")
	}
}

// Utility function for UTF-8 validation
func utf8Valid(s string) bool {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size == 1 {
			return false
		}
		s = s[size:]
	}
	return true
}

func TestProjectsPaneWithBadges(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)

	badges := map[string]ProjectBadge{
		"p1": {Symbol: "⚠"},
		"p2": {Symbol: "●"},
	}
	pane.SetBadges(badges)

	view := pane.View()
	if !strings.Contains(view.Content, "⚠") {
		t.Error("expected badge '⚠' in view")
	}
	if !strings.Contains(view.Content, "●") {
		t.Error("expected badge '●' in view")
	}
}

func TestProjectsPaneEmptyList(t *testing.T) {
	theme := NewTheme()
	pane := NewProjectsPane(theme, []tuiclient.Project{})
	pane.SetSize(50, 10)

	view := pane.View()
	if !strings.Contains(view.Content, "(no projects)") {
		t.Errorf("expected '(no projects)', got %q", view.Content)
	}
}

func TestProjectsPaneSelectedProjectID(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
		{ID: "p3", Name: "Project3"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)

	if pane.SelectedProjectID() != "p1" {
		t.Errorf("expected first project selected by default")
	}

	pane.selectedIndex = 2
	if pane.SelectedProjectID() != "p3" {
		t.Errorf("expected p3 to be selected, got %s", pane.SelectedProjectID())
	}
}

func TestProjectsPaneSetSize(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "ProjectOne"},
	}
	pane := NewProjectsPane(theme, projects)

	// Wide mode: width >= ProjectPaneWidth (17)
	pane.SetSize(20, 10)
	if pane.railMode {
		t.Error("expected railMode=false for width >= ProjectPaneWidth")
	}

	// Rail mode: width < ProjectPaneWidth (17)
	pane.SetSize(16, 10)
	if !pane.railMode {
		t.Error("expected railMode=true for width < ProjectPaneWidth")
	}
}

func TestProjectsPaneSetProjects(t *testing.T) {
	theme := NewTheme()
	projects1 := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
	}
	pane := NewProjectsPane(theme, projects1)
	pane.selectedIndex = 1

	projects2 := []tuiclient.Project{
		{ID: "p3", Name: "Project3"},
		{ID: "p4", Name: "Project4"},
		{ID: "p5", Name: "Project5"},
	}
	pane.SetProjects(projects2)

	if pane.selectedIndex != 0 {
		t.Errorf("expected selectedIndex reset to 0, got %d", pane.selectedIndex)
	}
	if len(pane.projects) != 3 {
		t.Errorf("expected 3 projects, got %d", len(pane.projects))
	}
	if pane.SelectedProjectID() != "p3" {
		t.Errorf("expected p3 to be selected, got %s", pane.SelectedProjectID())
	}
}

func TestProjectsPaneShortNames(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "AB"},
		{ID: "p2", Name: "XYZ"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10) // Wide mode

	view := pane.View()
	if !strings.Contains(view.Content, "AB") {
		t.Error("expected short name 'AB' to be unmodified")
	}
	if !strings.Contains(view.Content, "XYZ") {
		t.Error("expected short name 'XYZ' to be unmodified")
	}
}

func TestProjectsPaneNavigationWithOneProject(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)

	// Try to move up - should stay at 0
	pane.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	if pane.selectedIndex != 0 {
		t.Errorf("expected selectedIndex=0, got %d", pane.selectedIndex)
	}

	// Try to move down - should stay at 0
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if pane.selectedIndex != 0 {
		t.Errorf("expected selectedIndex=0, got %d", pane.selectedIndex)
	}
}

func TestProjectsPaneHeightConstraint(t *testing.T) {
	theme := NewTheme()
	projects := make([]tuiclient.Project, 10)
	for i := 0; i < 10; i++ {
		projects[i] = tuiclient.Project{ID: fmt.Sprintf("p%d", i), Name: fmt.Sprintf("Project%d", i)}
	}
	pane := NewProjectsPane(theme, projects)

	for _, height := range []int{1, 2, 3, 5} {
		t.Run(fmt.Sprintf("height_%d", height), func(t *testing.T) {
			pane.SetSize(50, height)
			pane.selectedIndex = height - 1 // Select the last row to stress the constraint

			view := pane.View()
			lines := strings.Split(strings.TrimSuffix(view.Content, "\n"), "\n")
			if len(lines) > height {
				t.Errorf("view has %d lines but height is %d: %q", len(lines), height, view.Content)
			}
		})
	}
}

func TestProjectsPaneNavigationCommandDataRace(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "Project1"},
		{ID: "p2", Name: "Project2"},
		{ID: "p3", Name: "Project3"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(50, 10)
	pane.selectedIndex = 0

	// Press down to move to p2 and get the command
	_, cmd1 := pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd1 == nil {
		t.Fatal("expected command for first down press")
	}

	// Press down again to move to p3
	_, cmd2 := pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if cmd2 == nil {
		t.Fatal("expected command for second down press")
	}

	// Execute the first command - should return p2, not p3
	msg1 := cmd1()
	selected1, ok := msg1.(ProjectSelected)
	if !ok {
		t.Fatalf("expected ProjectSelected message, got %T", msg1)
	}
	if selected1.ProjectID != "p2" {
		t.Errorf("first command should return p2, got %s", selected1.ProjectID)
	}

	// Execute the second command - should return p3
	msg2 := cmd2()
	selected2, ok := msg2.(ProjectSelected)
	if !ok {
		t.Fatalf("expected ProjectSelected message, got %T", msg2)
	}
	if selected2.ProjectID != "p3" {
		t.Errorf("second command should return p3, got %s", selected2.ProjectID)
	}
}

func TestProjectsPaneEmptyListRailWidth(t *testing.T) {
	theme := NewTheme()
	pane := NewProjectsPane(theme, []tuiclient.Project{})
	pane.SetSize(tuiview.ProjectRailWidth, 10)

	view := pane.View()
	lines := strings.Split(strings.TrimSpace(view.Content), "\n")
	for i, line := range lines {
		lineWidth := lipgloss.Width(line)
		if lineWidth > tuiview.ProjectRailWidth {
			t.Errorf("empty list line %d exceeds rail width: width=%d (expected <= %d): %q", i, lineWidth, tuiview.ProjectRailWidth, line)
		}
	}
}

func TestProjectsPaneStyleBleeding(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "VeryLongProjectNameThatExceedsTheWidth"},
	}
	pane := NewProjectsPane(theme, projects)

	// Test at widths where truncation happens
	for _, width := range []int{2, 3, 5} {
		t.Run(fmt.Sprintf("width_%d", width), func(t *testing.T) {
			pane.SetSize(width, 5)
			view := pane.View()
			lines := strings.Split(view.Content, "\n")
			for _, line := range lines {
				if line == "" {
					continue
				}

				// Verify that all lines respect the width constraint
				lineWidth := lipgloss.Width(line)
				if lineWidth > width {
					t.Errorf("line exceeds width at truncation: width=%d (expected <= %d): %q", lineWidth, width, line)
				}
			}
		})
	}
}

func TestProjectsPaneSelectedRowStyledLineWidth(t *testing.T) {
	theme := NewTheme()
	projects := []tuiclient.Project{
		{ID: "p1", Name: "VeryLongProjectNameThatExceedsTheWidth"},
	}
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(10, 5)
	pane.SetFocused(true)

	view := pane.View()
	lines := strings.Split(strings.TrimSpace(view.Content), "\n")

	// Check the title line (should be styled as focused)
	if len(lines) > 0 {
		titleLine := lines[0]
		titleWidth := lipgloss.Width(titleLine)
		if titleWidth > 10 {
			t.Errorf("focused title line exceeds width: width=%d (expected <= 10): %q", titleWidth, titleLine)
		}
	}

	// Check project lines (selected one should be styled)
	if len(lines) > 1 {
		projectLine := lines[1]
		projectWidth := lipgloss.Width(projectLine)
		if projectWidth > 10 {
			t.Errorf("project line exceeds width: width=%d (expected <= 10): %q", projectWidth, projectLine)
		}
	}
}
