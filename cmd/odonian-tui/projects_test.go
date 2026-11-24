package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
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
	pane.SetSize(120, 10) // Wide mode: full names

	view := pane.View()
	if !strings.Contains(view.Content, "ProjectOne") {
		t.Error("expected full project name 'ProjectOne' in wide mode")
	}
	if !strings.Contains(view.Content, "ProjectTwo") {
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
	pane.SetSize(80, 10) // Rail mode: abbreviated names (< 120)

	view := pane.View()
	// In rail mode, names should be abbreviated to 3 chars
	if !strings.Contains(view.Content, "Pro") {
		t.Error("expected abbreviated project name 'Pro' in rail mode")
	}
	// Full names should not appear
	if strings.Contains(view.Content, "ProjectOne") {
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
	pane.SetSize(50, 2) // Height of 2, but 5 projects

	// Selection at index 0 - should start at scrollOffset 0
	if pane.scrollOffset != 0 {
		t.Errorf("expected scrollOffset=0, got %d", pane.scrollOffset)
	}

	// Move down to index 2 - should scroll
	pane.Update(tea.KeyPressMsg{Code: tea.KeyDown})
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
	// Selection should still be visible in the pane
	if pane.selectedIndex < pane.scrollOffset || pane.selectedIndex >= pane.scrollOffset+pane.height {
		t.Errorf("selection not visible: selectedIndex=%d, scrollOffset=%d, height=%d", pane.selectedIndex, pane.scrollOffset, pane.height)
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
	pane := NewProjectsPane(theme, projects)
	pane.SetSize(10, 10)

	view := pane.View()
	lines := strings.Split(view.Content, "\n")
	for _, line := range lines {
		// Remove ANSI escape codes to get visual width
		// ANSI codes start with \x1b and end with [a-mA-Z]
		cleanLine := strings.ReplaceAll(line, "\x1b[7m", "")
		cleanLine = strings.ReplaceAll(cleanLine, "\x1b[m", "")
		cleanLine = strings.ReplaceAll(cleanLine, "\x1b(B\x1b)0", "")

		if len(cleanLine) > 10 {
			t.Errorf("visual line exceeds pane width: %q (len=%d)", cleanLine, len(cleanLine))
		}
	}
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
	if view.Content != "(no projects)" {
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

	pane.SetSize(130, 10)
	if pane.railMode {
		t.Error("expected railMode=false for width=130")
	}

	pane.SetSize(100, 10)
	if !pane.railMode {
		t.Error("expected railMode=true for width=100")
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
