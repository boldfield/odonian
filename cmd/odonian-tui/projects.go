package main

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
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
	railMode      bool // true when width < 120 (abbreviated names)
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
	p.railMode = width < 120
	p.ensureSelectionVisible()
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
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "up", "k":
			if p.selectedIndex > 0 {
				p.selectedIndex--
				p.ensureSelectionVisible()
			}
		case "down", "j":
			if p.selectedIndex < len(p.projects)-1 {
				p.selectedIndex++
				p.ensureSelectionVisible()
			}
		case "enter":
			return p, func() tea.Msg {
				return ProjectSelected{ProjectID: p.SelectedProjectID()}
			}
		}
	}
	return p, nil
}

// View renders the projects pane.
func (p *ProjectsPane) View() tea.View {
	var content string
	if len(p.projects) == 0 {
		content = "(no projects)"
	} else {
		var lines []string
		for i, proj := range p.projects {
			line := p.renderProjectLine(i, proj)
			lines = append(lines, line)
		}

		// Apply scroll offset and height
		startIdx := p.scrollOffset
		endIdx := startIdx + p.height
		if endIdx > len(lines) {
			endIdx = len(lines)
		}

		var result []string
		for i := startIdx; i < endIdx; i++ {
			result = append(result, lines[i])
		}

		content = strings.Join(result, "\n")
	}

	return tea.NewView(content)
}

// renderProjectLine renders a single project line.
func (p *ProjectsPane) renderProjectLine(index int, proj tuiclient.Project) string {
	cursor := "  "
	if index == p.selectedIndex {
		cursor = "▸ "
	}

	badge := p.badges[proj.ID]
	badgeStr := ""
	if badge.Symbol != "" {
		badgeStr = " " + badge.Symbol
	}

	var name string
	if p.railMode {
		name = p.abbreviateName(proj.Name)
	} else {
		name = proj.Name
	}

	line := fmt.Sprintf("%s%s%s", cursor, name, badgeStr)

	// Truncate to fit within pane width before styling
	line = truncateToWidth(line, p.width)

	// Apply styling if selected (after truncation so it doesn't affect width)
	if index == p.selectedIndex {
		line = p.theme.SelectedRow.Render(line)
	}

	return line
}

// abbreviateName shortens a project name to 3 characters for rail mode.
func (p *ProjectsPane) abbreviateName(name string) string {
	if len(name) <= 3 {
		return name
	}
	return name[:3]
}

// ensureSelectionVisible adjusts scroll offset to keep the selection in view.
func (p *ProjectsPane) ensureSelectionVisible() {
	if p.selectedIndex < p.scrollOffset {
		p.scrollOffset = p.selectedIndex
	}
	if p.selectedIndex >= p.scrollOffset+p.height {
		p.scrollOffset = p.selectedIndex - p.height + 1
	}
	if p.scrollOffset < 0 {
		p.scrollOffset = 0
	}
}

// truncateToWidth truncates a string to fit within the given width, adding "…" if needed.
func truncateToWidth(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	return s[:width-1] + "…"
}
