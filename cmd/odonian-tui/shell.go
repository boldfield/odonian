package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/charmbracelet/x/ansi"
)

// previewDebounce is how long the selection must rest on a work item before its events
// are fetched for the preview, so holding an arrow key does not issue a request per row.
const previewDebounce = 150 * time.Millisecond

// shellPane identifies a focusable pane of the shell.
type shellPane int

const (
	paneItems shellPane = iota
	paneProjects
	panePreview
)

// shellState is the three-pane shell: it owns the panes, the focus and the toggles.
// A BoardModel with a nil shell renders the old tab layout (kept until a later task
// deletes it).
type shellState struct {
	theme         *Theme
	projects      *ProjectsPane
	items         *WorkItemsPane
	preview       *PreviewPane
	focus         shellPane
	previewHidden bool
	projectHidden bool
	// previewSeq numbers the selections so a debounced events fetch for a row the
	// selection has since left is dropped.
	previewSeq      int
	previewDebounce time.Duration
}

// shellTasksMsg carries one project's work for the shell: the tasks and the ids of the
// ready tasks that are claimable (the rest are waiting on a dependency).
type shellTasksMsg struct {
	projectID string
	tasks     []tuiclient.Task
	claimable map[string]bool
	err       error
}

// previewFetchMsg fires after the debounce; it fetches events only if seq is still current.
type previewFetchMsg struct {
	seq    int
	taskID string
}

// NewShellBoardModel creates a board model that renders the three-pane layout.
func NewShellBoardModel(client tuiclient.Client, config *tuiconfig.Config, project tuiclient.Project) *BoardModel {
	m := NewBoardModel(client, config, project)
	theme := NewTheme()
	m.shell = &shellState{
		theme:           theme,
		projects:        NewProjectsPane(theme, []tuiclient.Project{project}),
		items:           NewWorkItemsPane(theme),
		preview:         NewPreviewPane(nil, 0, 0),
		focus:           paneItems,
		previewDebounce: previewDebounce,
	}
	m.shell.preview.SetTheme(theme)
	m.shell.items.SetProjectName(project.Name)
	return m
}

// fetchShellTasks lists the project's open tasks with fields=summary, plus one
// claimable=true listing so ready tasks can be told apart from tasks waiting on a dependency.
func (m *BoardModel) fetchShellTasks() tea.Cmd {
	client := m.client
	projectID := m.project.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		var all []tuiclient.Task
		for _, state := range append(append([]string{}, activeStates...), stateFailed) {
			tasks, err := client.ListTasks(ctx, projectID, tuiclient.WithState(state), tuiclient.WithFields("summary"))
			if err != nil {
				return shellTasksMsg{projectID: projectID, err: err}
			}
			all = append(all, tasks...)
		}
		ready, err := client.ListTasks(ctx, projectID, tuiclient.WithClaimable(true), tuiclient.WithFields("summary"))
		if err != nil {
			return shellTasksMsg{projectID: projectID, err: err}
		}
		claimable := make(map[string]bool, len(ready))
		for _, t := range ready {
			claimable[t.ID] = true
		}
		return shellTasksMsg{projectID: projectID, tasks: all, claimable: claimable}
	}
}

// fetchPreviewEvents fetches the events of a task for the preview pane.
func (m *BoardModel) fetchPreviewEvents(taskID string) tea.Cmd {
	client := m.client
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		events, err := client.ListEvents(ctx, taskID)
		if err != nil {
			events = nil
		}
		return PreviewEventsMsg{TaskID: taskID, Events: events}
	}
}

// setShellItems groups tasks into work items and gives them to the items pane. The
// returned command emits WorkItemSelected when the selection changed.
func (m *BoardModel) setShellItems(tasks []tuiclient.Task, claimable map[string]bool) tea.Cmd {
	ptrs := make([]*tuiclient.Task, len(tasks))
	for i := range tasks {
		ptrs[i] = &tasks[i]
	}
	cmd := m.shell.items.SetItems(tuiview.GroupWorkItems(ptrs).WorkItems, claimable)
	m.syncShellSelection()
	return cmd
}

// rebuildShellItems regroups m.tasks after an action refetched them.
func (m *BoardModel) rebuildShellItems() tea.Cmd {
	var flat []tuiclient.Task
	for _, state := range stateOrder {
		flat = append(flat, m.tasks[state]...)
	}
	return m.setShellItems(flat, m.shell.items.claimable)
}

// syncShellSelection mirrors the items pane selection into the fields the existing
// actions and the detail view read.
func (m *BoardModel) syncShellSelection() {
	wi := m.shell.items.SelectedItem()
	if wi == nil {
		m.selectedTaskID = ""
		return
	}
	m.selectedTaskID = wi.Implement.ID
	if col := columnIndex(wi.Implement.State); col >= 0 {
		m.selectedColumn = col
	}
}

// visiblePanes lists the panes that take focus at the current layout, left to right.
func (m *BoardModel) visiblePanes(layout tuiview.Layout) []shellPane {
	var panes []shellPane
	if layout.ProjectWidth > 0 {
		panes = append(panes, paneProjects)
	}
	panes = append(panes, paneItems)
	if layout.PreviewWidth > 0 {
		panes = append(panes, panePreview)
	}
	return panes
}

func (m *BoardModel) shellLayout() tuiview.Layout {
	return tuiview.ChooseLayout(m.width, m.height, m.shell.items.SelectedTaskID() != "", m.shell.previewHidden, m.shell.projectHidden)
}

// cycleFocus moves focus to the next (or previous) visible pane.
func (m *BoardModel) cycleFocus(delta int) {
	panes := m.visiblePanes(m.shellLayout())
	cur := 0
	for i, p := range panes {
		if p == m.shell.focus {
			cur = i
		}
	}
	m.shell.focus = panes[(cur+delta+len(panes))%len(panes)]
}

// updateShellKey handles the keys the shell owns in normal mode. handled is false for
// keys it leaves to the existing actions.
func (m *BoardModel) updateShellKey(msg tea.KeyPressMsg) (model tea.Model, cmd tea.Cmd, handled bool) {
	s := m.shell
	switch msg.String() {
	case "q", "ctrl+c":
		return m, tea.Quit, true

	case "tab":
		m.cycleFocus(1)
		return m, nil, true
	case "shift+tab":
		m.cycleFocus(-1)
		return m, nil, true

	case "v":
		s.previewHidden = !s.previewHidden
		if s.previewHidden && s.focus == panePreview {
			s.focus = paneItems
		}
		return m, nil, true
	case "V":
		s.projectHidden = !s.projectHidden
		if s.projectHidden && s.focus == paneProjects {
			s.focus = paneItems
		}
		return m, nil, true

	case "up", "k", "down", "j":
		if s.focus == panePreview && m.shellLayout().PreviewWidth > 0 {
			if k := msg.String(); k == "up" || k == "k" {
				s.preview.Scroll(-1)
			} else {
				s.preview.Scroll(1)
			}
		} else if s.focus == paneProjects && s.projectsVisible(m) {
			_, cmd = s.projects.Update(msg)
		} else {
			_, cmd = s.items.Update(msg)
			m.syncShellSelection()
		}
		return m, cmd, true

	case "enter":
		if s.focus == paneProjects && s.projectsVisible(m) {
			_, cmd = s.projects.Update(msg)
			s.focus = paneItems
			return m, cmd, true
		}
		if m.selectedTaskID != "" {
			m.mode = modeDetail
			m.detailMessage = ""
			return m, m.fetchDetailCmd(m.selectedTaskID), true
		}
		return m, nil, true

	case "z":
		if m.effectiveFocus() == paneProjects {
			if id := s.projects.SelectedProjectID(); id != "" {
				m.pendingProjectID = id
				m.mode = modeArchiveProjectConfirm
			}
			return m, nil, true
		}
		if m.effectiveFocus() != paneItems {
			return m, nil, true
		}

	case "m":
		if m.effectiveFocus() != paneItems {
			return m, nil, true
		}
		if wi := s.items.SelectedItem(); wi != nil && wi.Implement.State == stateApproved {
			return m, m.mergeSelectedCmd(wi.Implement.ID), true
		}
		return m, nil, true

	// The remaining task actions apply only with the work-items pane focused.
	case "p", "a", "x", "b", "u", "f", "t", "i", "w", "e":
		if m.effectiveFocus() != paneItems {
			return m, nil, true
		}
		return m, nil, false

	// The tab layout's column and project-switcher keys have no meaning here.
	case "left", "right", "h", "l", "P":
		return m, nil, true
	}
	return m, nil, false
}

// effectiveFocus is the pane that has focus at the current layout (a hidden pane cannot).
func (m *BoardModel) effectiveFocus() shellPane {
	layout := m.shellLayout()
	focus := m.shell.focus
	if layout.ProjectWidth == 0 && focus == paneProjects {
		focus = paneItems
	}
	if layout.PreviewWidth == 0 && focus == panePreview {
		focus = paneItems
	}
	return focus
}

// mergeSelectedCmd resolves the PR of an approved task (its pr link, else the task's
// deterministic branch) and reports it as a prResolvedMsg, which opens the merge confirmation.
func (m *BoardModel) mergeSelectedCmd(taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if task, err := m.client.GetTask(ctx, taskID); err == nil {
			for _, link := range task.Links {
				if link.Kind == "pr" {
					return prResolvedMsg{taskID: taskID, prURL: link.Value, fromShell: true}
				}
			}
		}
		msg := m.resolvePRAndMerge(taskID)()
		if resolved, ok := msg.(prResolvedMsg); ok {
			resolved.fromShell = true
			return resolved
		}
		return msg
	}
}

func (s *shellState) projectsVisible(m *BoardModel) bool {
	return m.shellLayout().ProjectWidth > 0
}

// updateShell is the update of the three-pane layout. The shell handles its own messages
// and keys first; everything else goes through the existing handler, after which the work
// items are regrouped when an action or a refresh replaced the tasks.
func (m *BoardModel) updateShell(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, handled := m.handleShellMsg(msg); handled {
		return m, cmd
	}
	if key, ok := msg.(tea.KeyPressMsg); ok && m.mode == modeNormal {
		m.statusMessage = ""
		if model, cmd, handled := m.updateShellKey(key); handled {
			return model, cmd
		}
	}
	model, cmd := m.updateTabs(msg)
	switch msg.(type) {
	case reviewActionMsg, tasksFetchedMsg:
		rebuild := m.rebuildShellItems()
		m.shell.preview.SetItem(m.shell.items.SelectedItem())
		cmd = tea.Batch(cmd, rebuild)
	case projectsFetchedMsg:
		m.shell.projects.SetProjects(m.projects)
		m.shell.projects.SelectProject(m.project.ID)
	}
	return model, cmd
}

// handleShellMsg handles the messages only the shell produces. handled is false for the rest.
func (m *BoardModel) handleShellMsg(msg tea.Msg) (cmd tea.Cmd, handled bool) {
	s := m.shell
	switch msg := msg.(type) {
	case shellTasksMsg:
		if msg.projectID != m.project.ID {
			return nil, true // a response for a project the user has since left
		}
		if msg.err != nil {
			m.error = fmt.Sprintf("Error: %v", msg.err)
			return nil, true
		}
		m.loading = false
		m.error = ""
		m.tasks = bucketTasksByState(msg.tasks)
		m.lastRefresh = time.Now()
		cmd = m.setShellItems(msg.tasks, msg.claimable)
		s.preview.SetItem(s.items.SelectedItem())
		return cmd, true

	case projectArchiveMsg:
		if msg.err != "" {
			m.error = msg.err
			m.mode = modeNormal
			return nil, true
		}
		m.error = ""
		m.mode = modeNormal
		m.projects = msg.projects
		s.projects.SetProjects(m.projects)
		for _, p := range m.projects {
			if p.ID == m.project.ID {
				s.projects.SelectProject(p.ID)
				return nil, true
			}
		}
		// The current project was archived: move to the first remaining one, or empty the view.
		m.tasks = make(map[string][]tuiclient.Task)
		s.preview.SetItem(nil)
		if len(m.projects) == 0 {
			m.project = tuiclient.Project{}
			m.loading = false
			s.items.SetProjectName("")
			s.items.SetItems(nil, nil)
			m.syncShellSelection()
			return nil, true
		}
		m.project = m.projects[0]
		m.loading = true
		s.projects.SelectProject(m.project.ID)
		s.items.SetProjectName(m.project.Name)
		s.items.SetItems(nil, nil)
		m.syncShellSelection()
		return m.fetchShellTasks(), true

	case ProjectSelected:
		if msg.ProjectID == "" || msg.ProjectID == m.project.ID {
			return nil, true
		}
		for _, p := range m.projects {
			if p.ID == msg.ProjectID {
				m.project = p
			}
		}
		if m.project.ID != msg.ProjectID {
			return nil, true
		}
		m.tasks = make(map[string][]tuiclient.Task)
		m.loading = true
		m.error = ""
		s.items.SetProjectName(m.project.Name)
		s.items.SetItems(nil, nil)
		m.syncShellSelection()
		s.preview.SetItem(nil)
		return m.fetchShellTasks(), true

	case WorkItemSelected:
		wi := s.items.SelectedItem()
		s.preview.SetItem(wi)
		if wi == nil || wi.Implement.ID != msg.TaskID {
			return nil, true
		}
		s.previewSeq++
		fetch := previewFetchMsg{seq: s.previewSeq, taskID: msg.TaskID}
		return tea.Tick(s.previewDebounce, func(time.Time) tea.Msg { return fetch }), true

	case previewFetchMsg:
		if msg.seq != s.previewSeq {
			return nil, true
		}
		return m.fetchPreviewEvents(msg.taskID), true

	case PreviewEventsMsg:
		s.preview.Update(msg)
		return nil, true
	}
	return nil, false
}

// shellHelpBar is the bottom bar; it truncates with an ellipsis instead of overflowing.
func (m *BoardModel) shellHelpBar() string {
	var text string
	switch {
	case m.error != "":
		text = m.error
	case m.statusMessage != "":
		text = m.statusMessage
	default:
		text = m.shellKeyHints()
	}
	return ansi.Truncate(text, m.width, "…")
}

// shellKeyHints lists the keys valid now: the focused pane's, and for the work-items pane
// those the selected item's state allows.
func (m *BoardModel) shellKeyHints() string {
	const tail = "r refresh · c scorecards · q quit"
	switch m.effectiveFocus() {
	case paneProjects:
		return "tab pane · ↑↓ move · enter open · z archive project · " + tail
	case panePreview:
		return "tab pane · ↑↓ scroll · v preview · " + tail
	}
	parts := []string{"tab pane", "↑↓ move", "enter detail"}
	if wi := m.shell.items.SelectedItem(); wi != nil {
		switch wi.Implement.State {
		case stateBacklog:
			parts = append(parts, "p promote")
		case stateReview:
			parts = append(parts, "a approve", "x reject")
		case stateApproved:
			parts = append(parts, "b bounce", "m merge")
		case stateBlocked:
			parts = append(parts, "u unblock", "f fail")
		}
		parts = append(parts, "t hold/release", "i priority", "w front", "e reset", "z archive")
	}
	parts = append(parts, "v preview", "V projects", tail)
	return strings.Join(parts, " · ")
}

// shellView renders the three panes, joined, with a help bar.
func (m *BoardModel) shellView() string {
	s := m.shell
	layout := m.shellLayout()
	bodyHeight := m.height - 2
	// An active prompt takes the help bar's place and as many rows above it as it needs.
	prompt := ""
	if m.mode != modeNormal {
		prompt = strings.TrimRight(m.renderReviewOverlay(), "\n")
		bodyHeight -= strings.Count(prompt, "\n")
	}
	if bodyHeight < 1 {
		bodyHeight = 1
	}

	focus := s.focus
	if layout.ProjectWidth == 0 {
		focus = paneItems
	}
	if layout.PreviewWidth == 0 && focus == panePreview {
		focus = paneItems
	}
	s.preview.SetFocused(focus == panePreview)
	s.projects.SetFocused(focus == paneProjects)
	s.items.SetFocused(focus == paneItems)

	sep := s.theme.PaneSeparator.Render(strings.TrimSuffix(strings.Repeat("│\n", bodyHeight), "\n"))
	box := func(width int, content string) string {
		return lipgloss.NewStyle().Width(width).Height(bodyHeight).MaxWidth(width).MaxHeight(bodyHeight).Render(content)
	}

	var cols []string
	if layout.ProjectWidth > 0 {
		s.projects.SetSize(layout.ProjectWidth, bodyHeight)
		cols = append(cols, box(layout.ProjectWidth, s.projects.View().Content), sep)
	}
	s.items.SetSize(layout.ItemsWidth, bodyHeight)
	cols = append(cols, box(layout.ItemsWidth, s.items.View().Content))
	if layout.PreviewWidth > 0 {
		s.preview.SetSize(layout.PreviewWidth, bodyHeight)
		cols = append(cols, sep, box(layout.PreviewWidth, s.preview.Render()))
	}

	footer := m.shellHelpBar()
	if prompt != "" {
		footer = prompt
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, cols...) + "\n" +
		strings.Repeat("─", m.width) + "\n" + footer
}
