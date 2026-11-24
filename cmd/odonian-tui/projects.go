package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/charmbracelet/x/ansi"
)

// ProjectBadge represents the badge displayed next to a project name.
type ProjectBadge struct {
	Symbol string // e.g., "⚠", "!", "✔", "●", or empty
}

// ProjectSelected is emitted when the user changes the selected project.
type ProjectSelected struct {
	ProjectID string
}

// ProjectsPane is a Bubble Tea model that displays a list of projects with badges.
type ProjectsPane struct {
	projects      []tuiclient.Project
	badges        map[string]ProjectBadge // keyed by project ID
	selectedIndex int                     // index of currently selected project
	scrollOffset  int                     // vertical scroll offset
	width         int                     // pane width in characters
	height        int                     // pane height in lines
	theme         *Theme
	railMode      bool // true when width < ProjectPaneWidth (abbreviated names)
	focused       bool // true when the pane has keyboard focus
}

// NewProjectsPane creates a new ProjectsPane.
func NewProjectsPane(theme *Theme, projects []tuiclient.Project) *ProjectsPane {
	badges := make(map[string]ProjectBadge)
	for _, p := range projects {
		badges[p.ID] = ProjectBadge{}
	}

	return &ProjectsPane{
		projects:      projects,
		badges:        badges,
		selectedIndex: 0,
		scrollOffset:  0,
		width:         20,
		height:        10,
		theme:         theme,
		railMode:      false,
	}
}

// SetSize updates the pane dimensions and determines the render mode.
func (p *ProjectsPane) SetSize(width, height int) {
	p.width = width
	p.height = height
	p.railMode = width < tuiview.ProjectPaneWidth
	p.ensureSelectionVisible()
}

// SetFocused updates the focus state of the pane.
func (p *ProjectsPane) SetFocused(focused bool) {
	p.focused = focused
}

// SetBadges updates the badges for all projects.
func (p *ProjectsPane) SetBadges(badges map[string]ProjectBadge) {
	p.badges = badges
}

// SetProjects updates the list of projects. Resets selection to the first project.
func (p *ProjectsPane) SetProjects(projects []tuiclient.Project) {
	p.projects = projects
	p.selectedIndex = 0
	p.scrollOffset = 0

	// Update badges map to include all new projects, preserving existing badges
	newBadges := make(map[string]ProjectBadge)
	for _, proj := range projects {
		if badge, ok := p.badges[proj.ID]; ok {
			newBadges[proj.ID] = badge
		} else {
			newBadges[proj.ID] = ProjectBadge{}
		}
	}
	p.badges = newBadges
}

// SelectedProjectID returns the ID of the currently selected project.
func (p *ProjectsPane) SelectedProjectID() string {
	if p.selectedIndex >= 0 && p.selectedIndex < len(p.projects) {
		return p.projects[p.selectedIndex].ID
	}
	return ""
}

// Init returns the initial command for the projects pane (none needed).
func (p *ProjectsPane) Init() tea.Cmd {
	return nil
}

// Update handles user input for the projects pane.
func (p *ProjectsPane) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := msg.(tea.KeyPressMsg); ok {
		switch msg.String() {
		case "up", "k":
			return p, p.Move(-1)
		case "down", "j":
			return p, p.Move(1)
		case "enter":
			return p, p.Open()
		}
	}
	return p, nil
}

// Move shifts the selection by delta rows and reports the new selection.
func (p *ProjectsPane) Move(delta int) tea.Cmd {
	next := p.selectedIndex + delta
	if next < 0 || next >= len(p.projects) {
		return nil
	}
	p.selectedIndex = next
	p.ensureSelectionVisible()
	return p.Open()
}

// Open reports the selected project as chosen.
func (p *ProjectsPane) Open() tea.Cmd {
	selectedID := p.SelectedProjectID()
	return func() tea.Msg {
		return ProjectSelected{ProjectID: selectedID}
	}
}

// View renders the projects pane.
func (p *ProjectsPane) View() tea.View {
	var title string
	if p.railMode {
		title = "PROJ"
	} else {
		title = "PROJECTS"
	}

	if p.focused {
		title = p.theme.PaneTitleFocused.Render(title)
	} else {
		title = p.theme.PaneTitleUnfocused.Render(title)
	}

	title = ensureLineWidth(title, p.width)

	var content string
	if len(p.projects) == 0 {
		content = ensureLineWidth("(no projects)", p.width)
	} else {
		var lines []string
		for i, proj := range p.projects {
			line := p.renderProjectLine(i, proj)
			lines = append(lines, line)
		}

		// Apply scroll offset and height (title takes 1 row, so projects get height-1 rows)
		rowsForProjects := p.height - 1
		if rowsForProjects < 0 {
			rowsForProjects = 0
		}

		startIdx := p.scrollOffset
		endIdx := startIdx + rowsForProjects
		if endIdx > len(lines) {
			endIdx = len(lines)
		}

		var result []string
		for i := startIdx; i < endIdx; i++ {
			result = append(result, lines[i])
		}

		content = strings.Join(result, "\n")
	}

	fullContent := title + "\n" + content
	return tea.NewView(fullContent)
}

// renderProjectLine renders a single project line.
func (p *ProjectsPane) renderProjectLine(index int, proj tuiclient.Project) string {
	cursor := "  "
	if index == p.selectedIndex {
		cursor = "▸ "
	}

	badge := p.badges[proj.ID]
	badgeStr := ""
	badgeWidth := 0
	if badge.Symbol != "" {
		badgeStr = " " + badge.Symbol
		badgeWidth = lipgloss.Width(badgeStr)
	}

	var name string
	if p.railMode {
		name = p.abbreviateName(proj.Name)
	} else {
		name = proj.Name
	}

	cursorWidth := lipgloss.Width(cursor)
	availableWidth := p.width - cursorWidth - badgeWidth
	if availableWidth < 1 {
		availableWidth = 1
	}

	name = truncateToDisplayWidth(name, availableWidth)

	line := fmt.Sprintf("%s%s%s", cursor, name, badgeStr)

	// Apply styling if selected (after truncation so it doesn't affect width)
	if index == p.selectedIndex {
		line = p.theme.SelectedRow.Render(line)
	}

	line = ensureLineWidth(line, p.width)

	return line
}

// abbreviateName shortens a project name to 3 display-width characters for rail mode.
func (p *ProjectsPane) abbreviateName(name string) string {
	const maxRailWidth = 3
	var result strings.Builder

	width := 0
	for _, r := range name {
		rStr := string(r)
		rWidth := lipgloss.Width(rStr)
		if width+rWidth > maxRailWidth {
			break
		}
		result.WriteRune(r)
		width += rWidth
	}

	return result.String()
}

// ensureSelectionVisible adjusts scroll offset to keep the selection in view.
func (p *ProjectsPane) ensureSelectionVisible() {
	rowsForProjects := p.height - 1
	if rowsForProjects < 0 {
		rowsForProjects = 0
	}

	if p.selectedIndex < p.scrollOffset {
		p.scrollOffset = p.selectedIndex
	}
	if p.selectedIndex >= p.scrollOffset+rowsForProjects {
		p.scrollOffset = p.selectedIndex - rowsForProjects + 1
	}
	if p.scrollOffset < 0 {
		p.scrollOffset = 0
	}
}

// truncateToDisplayWidth truncates a string to fit within the given display width, adding "…" if needed.
// It properly handles multi-byte UTF-8 characters and preserves rune boundaries.
func truncateToDisplayWidth(s string, maxWidth int) string {
	if maxWidth < 0 {
		return ""
	}

	currentWidth := lipgloss.Width(s)
	if currentWidth <= maxWidth {
		return s
	}

	ellipsisWidth := lipgloss.Width("…")
	targetWidth := maxWidth - ellipsisWidth

	if targetWidth < 0 {
		return "…"
	}

	var result strings.Builder
	for _, r := range s {
		rStr := string(r)
		rWidth := lipgloss.Width(rStr)
		if lipgloss.Width(result.String())+rWidth > targetWidth {
			break
		}
		result.WriteRune(r)
	}

	return result.String() + "…"
}

// ensureLineWidth ensures a line does not exceed the given display width by truncating if necessary.
// It preserves ANSI codes and rune boundaries using ansi.Truncate.
func ensureLineWidth(line string, maxWidth int) string {
	if maxWidth < 1 {
		maxWidth = 1
	}

	currentWidth := lipgloss.Width(line)
	if currentWidth <= maxWidth {
		return line
	}

	return ansi.Truncate(line, maxWidth, "")
}

// SelectProject moves the selection to the project with the given id, without emitting
// ProjectSelected. It does nothing when the id is not in the list.
func (p *ProjectsPane) SelectProject(id string) {
	for i, proj := range p.projects {
		if proj.ID == id {
			p.selectedIndex = i
			p.ensureSelectionVisible()
			return
		}
	}
}
