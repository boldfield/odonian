package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiview"
)

// WorkItemSelected is emitted when the selected work item changes.
type WorkItemSelected struct {
	TaskID string
}

// WorkItemsPane is a Bubble Tea model that lists work items, one row per implement task.
// It is a hand-written list rather than a Bubbles list: a row is one or two lines tall
// (the nested review line), and the Bubbles list delegate has a single fixed item height,
// so it cannot scroll or page variable-height rows correctly.
// The pane does not fetch; it is given items with SetItems.
type WorkItemsPane struct {
	theme       *Theme
	projectName string
	items       []*tuiview.WorkItem // in display order
	claimable   map[string]bool
	selectedID  string
	scroll      int // index of the first visible item
	width       int
	height      int
	focused     bool
	now         func() time.Time
}

// NewWorkItemsPane creates an empty WorkItemsPane.
func NewWorkItemsPane(theme *Theme) *WorkItemsPane {
	return &WorkItemsPane{
		theme:     theme,
		claimable: map[string]bool{},
		width:     60,
		height:    10,
		now:       time.Now,
	}
}

// themeRowStyles adapts Theme to tuiview.RowTheme.
type themeRowStyles struct{ theme *Theme }

func (t themeRowStyles) GetStatusStyle(state string) lipgloss.Style {
	if s, ok := t.theme.StatusStyles[state]; ok {
		return s
	}
	return lipgloss.NewStyle()
}

// SetSize updates the pane dimensions.
func (p *WorkItemsPane) SetSize(width, height int) {
	p.width = width
	p.height = height
	p.ensureSelectionVisible()
}

// SetFocused updates the focus state of the pane.
func (p *WorkItemsPane) SetFocused(focused bool) {
	p.focused = focused
}

// SetProjectName sets the name shown in the header.
func (p *WorkItemsPane) SetProjectName(name string) {
	p.projectName = name
}

// SelectedTaskID returns the id of the selected implement task, or "" when the list is empty.
func (p *WorkItemsPane) SelectedTaskID() string {
	if p.indexOf(p.selectedID) < 0 {
		return ""
	}
	return p.selectedID
}

// SetItems replaces the list. claimable is the set of ready task ids that can be claimed.
// The same task stays selected if it still exists; otherwise the row at the old index
// (clamped to the new list) is selected. The returned command emits WorkItemSelected
// when the selected task changed.
func (p *WorkItemsPane) SetItems(items []*tuiview.WorkItem, claimable map[string]bool) tea.Cmd {
	oldID := p.SelectedTaskID()
	oldIndex := p.indexOf(oldID)

	if claimable == nil {
		claimable = map[string]bool{}
	}
	p.claimable = claimable

	open := make([]*tuiview.WorkItem, 0, len(items))
	for _, wi := range items {
		if wi == nil || wi.Implement == nil {
			continue
		}
		switch tuiview.Status(wi.Implement, claimable) {
		case tuiview.StatusDone, tuiview.StatusAbandoned:
			continue
		}
		open = append(open, wi)
	}
	sort.SliceStable(open, func(i, j int) bool { return p.less(open[i], open[j]) })
	p.items = open

	switch {
	case len(open) == 0:
		p.selectedID = ""
		p.scroll = 0
	case p.indexOf(oldID) >= 0:
		// selection survives by id
	case oldIndex >= 0:
		if oldIndex >= len(open) {
			oldIndex = len(open) - 1
		}
		p.selectedID = open[oldIndex].Implement.ID
	default:
		p.selectedID = open[0].Implement.ID
	}
	p.ensureSelectionVisible()

	if newID := p.SelectedTaskID(); newID != oldID && newID != "" {
		return func() tea.Msg { return WorkItemSelected{TaskID: newID} }
	}
	return nil
}

// groupRank orders status groups: checks, in progress, review, approved,
// ready and waiting, backlog, blocked and failed.
func groupRank(s tuiview.StatusType) int {
	switch s {
	case tuiview.StatusChecks:
		return 0
	case tuiview.StatusInProgress:
		return 1
	case tuiview.StatusReview:
		return 2
	case tuiview.StatusApproved:
		return 3
	case tuiview.StatusReadyClaimable, tuiview.StatusReadyWaiting:
		return 4
	case tuiview.StatusBacklogNotPromoted, tuiview.StatusBacklogWaiting:
		return 5
	default:
		return 6
	}
}

// less orders by status group, then by today's board order: priority descending,
// created_at ascending, then id.
func (p *WorkItemsPane) less(a, b *tuiview.WorkItem) bool {
	ra := groupRank(tuiview.Status(a.Implement, p.claimable))
	rb := groupRank(tuiview.Status(b.Implement, p.claimable))
	if ra != rb {
		return ra < rb
	}
	priA, priB := int64(500), int64(500)
	if a.Implement.Priority != nil {
		priA = *a.Implement.Priority
	}
	if b.Implement.Priority != nil {
		priB = *b.Implement.Priority
	}
	if priA != priB {
		return priA > priB
	}
	if before, differs := compareCreatedAt(a.Implement.CreatedAt, b.Implement.CreatedAt); differs {
		return before
	}
	return a.Implement.ID < b.Implement.ID
}

func (p *WorkItemsPane) indexOf(id string) int {
	if id == "" {
		return -1
	}
	for i, wi := range p.items {
		if wi.Implement.ID == id {
			return i
		}
	}
	return -1
}

// Init returns the initial command (none needed).
func (p *WorkItemsPane) Init() tea.Cmd { return nil }

// Update handles keyboard navigation.
func (p *WorkItemsPane) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return p, nil
	}
	idx := p.indexOf(p.selectedID)
	if idx < 0 {
		return p, nil
	}
	switch keyMsg.String() {
	case "up", "k":
		idx--
	case "down", "j":
		idx++
	default:
		return p, nil
	}
	if idx < 0 || idx >= len(p.items) {
		return p, nil
	}
	p.selectedID = p.items[idx].Implement.ID
	p.ensureSelectionVisible()
	id := p.selectedID
	return p, func() tea.Msg { return WorkItemSelected{TaskID: id} }
}

// View renders the pane.
func (p *WorkItemsPane) View() tea.View {
	return tea.NewView(p.render())
}

// bodyHeight is the number of lines available under the header.
func (p *WorkItemsPane) bodyHeight() int {
	if p.height < 1 {
		return 0
	}
	return p.height - 1
}

// itemHeight is the number of lines a work item renders to.
func (p *WorkItemsPane) itemHeight(wi *tuiview.WorkItem) int {
	return strings.Count(p.renderItem(wi), "\n") + 1
}

func (p *WorkItemsPane) renderItem(wi *tuiview.WorkItem) string {
	w := p.width - 2 // room for the cursor column
	if w < 1 {
		w = 1
	}
	return tuiview.RenderRow(wi, w, p.now(), themeRowStyles{p.theme}, p.claimable[wi.Implement.ID])
}

// visibleEnd returns the exclusive end index of the items that fit fully when
// starting at start. When items are cut off below, one line is reserved for "↓ N more".
func (p *WorkItemsPane) visibleEnd(start int) int {
	body := p.bodyHeight()
	end := p.fit(start, body)
	if end < len(p.items) {
		end = p.fit(start, body-1)
	}
	return end
}

func (p *WorkItemsPane) fit(start, lines int) int {
	end := start
	for end < len(p.items) {
		h := p.itemHeight(p.items[end])
		if h > lines {
			break
		}
		lines -= h
		end++
	}
	return end
}

// ensureSelectionVisible scrolls so the selected item is fully in view.
func (p *WorkItemsPane) ensureSelectionVisible() {
	idx := p.indexOf(p.selectedID)
	if idx < 0 {
		p.scroll = 0
		return
	}
	if p.scroll > idx {
		p.scroll = idx
	}
	if p.scroll < 0 {
		p.scroll = 0
	}
	for p.scroll < idx && p.visibleEnd(p.scroll) <= idx {
		p.scroll++
	}
}

func (p *WorkItemsPane) openCount() int { return len(p.items) }

func (p *WorkItemsPane) render() string {
	w := p.width
	if w < 1 {
		w = 1
	}
	title := fmt.Sprintf("%s · %d open", p.projectName, p.openCount())
	if p.projectName == "" {
		title = fmt.Sprintf("%d open", p.openCount())
	}
	if p.focused {
		title = p.theme.PaneTitleFocused.Render(truncateToDisplayWidth(title, w))
	} else {
		title = p.theme.PaneTitleUnfocused.Render(truncateToDisplayWidth(title, w))
	}
	lines := []string{ensureLineWidth(title, w)}

	if len(p.items) == 0 {
		lines = append(lines, ensureLineWidth(p.theme.DimmedText.Render(truncateToDisplayWidth("Nothing open in this project", w)), w))
		return p.clampLines(lines)
	}

	end := p.visibleEnd(p.scroll)
	if end == p.scroll {
		// The selected row alone is taller than the body; show what fits of it.
		end = p.scroll + 1
	}
	for i := p.scroll; i < end; i++ {
		wi := p.items[i]
		selected := wi.Implement.ID == p.selectedID
		for n, line := range strings.Split(p.renderItem(wi), "\n") {
			prefix := "  "
			if selected && n == 0 {
				prefix = "▸ "
			}
			line = prefix + line
			if selected {
				line = p.theme.SelectedRow.Render(ensureLineWidth(line, w))
			}
			lines = append(lines, ensureLineWidth(line, w))
		}
	}
	if end < len(p.items) {
		lines = append(lines, ensureLineWidth(p.theme.DimmedText.Render(fmt.Sprintf("↓ %d more", len(p.items)-end)), w))
	}
	return p.clampLines(lines)
}

// clampLines cuts the output to the pane height.
func (p *WorkItemsPane) clampLines(lines []string) string {
	h := p.height
	if h < 1 {
		h = 1
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}
