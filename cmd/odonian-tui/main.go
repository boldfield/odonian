package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
)

func main() {
	// Parse flags
	var flagURL, flagToken, flagActor string
	flag.StringVar(&flagURL, "url", "", "Odonian URL")
	flag.StringVar(&flagToken, "token", "", "Odonian token")
	flag.StringVar(&flagActor, "actor", "", "Actor name for reviews (defaults to $USER)")
	flag.Parse()

	// Load configuration
	cfg, err := tuiconfig.LoadConfig(flagURL, flagToken, flagActor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	// Create API client
	client := tuiclient.NewHTTPClient(cfg.URL, cfg.Token)

	// List projects to see what we have
	ctx := context.Background()
	projects, err := client.ListProjects(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to list projects: %v\n", err)
		os.Exit(1)
	}

	if len(projects) == 0 {
		fmt.Fprintf(os.Stderr, "No projects available\n")
		os.Exit(0)
	}

	// Auto-select if exactly one project or a default is configured
	var selectedProject tuiclient.Project
	if len(projects) == 1 {
		selectedProject = projects[0]
	} else if cfg.DefaultProject != "" {
		found := false
		for _, p := range projects {
			if p.ID == cfg.DefaultProject {
				selectedProject = p
				found = true
				break
			}
		}
		if !found {
			fmt.Fprintf(os.Stderr, "Default project not found: %s\n", cfg.DefaultProject)
			os.Exit(1)
		}
	} else {
		// Multiple projects, no default: show picker
		m := NewProjectPickerModel(projects)
		p := tea.NewProgram(m)
		finalModel, err := p.Run()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
			os.Exit(1)
		}

		pickerModel := finalModel.(ProjectPickerModel)
		if pickerModel.Quit {
			os.Exit(0)
		}
		selectedProject = projects[pickerModel.Selected]
	}

	// Create and run the board model for the selected project
	boardModel := NewShellBoardModel(client, cfg, selectedProject)
	p := tea.NewProgram(boardModel)
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running board: %v\n", err)
		os.Exit(1)
	}
}

// ProjectPickerModel is the Bubble Tea model for selecting a project.
type ProjectPickerModel struct {
	Projects []tuiclient.Project
	Selected int
	Quit     bool
}

func NewProjectPickerModel(projects []tuiclient.Project) ProjectPickerModel {
	return ProjectPickerModel{
		Projects: projects,
		Selected: 0,
		Quit:     false,
	}
}

func (m ProjectPickerModel) Init() tea.Cmd {
	return nil
}

func (m ProjectPickerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.Quit = true
			return m, tea.Quit
		case "up", "k":
			if m.Selected > 0 {
				m.Selected--
			}
		case "down", "j":
			if m.Selected < len(m.Projects)-1 {
				m.Selected++
			}
		case "enter":
			return m, tea.Quit
		}
	case tea.WindowSizeMsg:
		// Handle window resizing
	}
	return m, nil
}

func (m ProjectPickerModel) View() tea.View {
	s := "Select a project (arrow keys or hjkl, enter to select, q to quit):\n\n"
	for i, p := range m.Projects {
		cursor := "  "
		if i == m.Selected {
			cursor = "> "
		}
		s += fmt.Sprintf("%s%s\n", cursor, p.Name)
	}
	return tea.NewView(s)
}
