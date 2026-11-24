package main

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/charmbracelet/x/ansi"
)

// PreviewEventsMsg delivers a work item's events to the preview pane once they
// arrive. Events for a task other than the one shown are ignored.
type PreviewEventsMsg struct {
	TaskID string
	Events []tuiclient.Event
}

// PreviewPane is a Bubble Tea model that shows a summary of one work item:
// title, status line, pull request and time in checks, the current round's
// reviews, and the most recent events, newest first, as many as fit.
// It does not fetch; the caller hands it the work item, the pull request link
// and, separately, the events once they arrive.
type PreviewPane struct {
	item   *tuiview.WorkItem
	prURL  string
	events []tuiclient.Event
	loaded bool // true once events have arrived, even when there are none
	width  int
	height int
	now    func() time.Time
	// focused styles the title; scroll is how many lines the view is scrolled down.
	focused bool
	scroll  int
	theme   *Theme
}

// NewPreviewPane creates a preview pane for item. History shows as loading
// until SetEvents is called or a PreviewEventsMsg arrives.
func NewPreviewPane(item *tuiview.WorkItem, width, height int) *PreviewPane {
	return &PreviewPane{item: item, width: width, height: height, now: time.Now}
}

// SetItem shows a different work item. If it is a different task, the pull
// request link and events are cleared and history shows as loading again.
func (p *PreviewPane) SetItem(item *tuiview.WorkItem) {
	if p.taskID() != previewTaskID(item) {
		p.prURL = ""
		p.events = nil
		p.loaded = false
		p.scroll = 0
	}
	p.item = item
}

// SetPRURL sets the pull request link to show; empty hides it.
func (p *PreviewPane) SetPRURL(url string) {
	p.prURL = url
}

// SetDetail takes the pull request link from the task's current-round links,
// so a superseded pull request from an earlier round is never shown.
func (p *PreviewPane) SetDetail(detail tuiclient.TaskDetail) {
	p.prURL = ""
	for _, link := range detail.CurrentRoundLinks {
		if link.Kind == "pr" && link.TombstonedAt == nil {
			p.prURL = link.Value
		}
	}
}

// SetEvents hands the pane the item's events, in the order the server returns
// them (oldest first), and marks history as loaded.
func (p *PreviewPane) SetEvents(events []tuiclient.Event) {
	p.events = events
	p.loaded = true
}

// SetFocused updates the focus state of the pane; the title is bold while focused.
func (p *PreviewPane) SetFocused(focused bool) {
	p.focused = focused
}

// SetTheme sets the theme used to style the focused title.
func (p *PreviewPane) SetTheme(theme *Theme) {
	p.theme = theme
}

// Scroll moves the view by delta lines; Render clamps it to the content.
func (p *PreviewPane) Scroll(delta int) {
	p.scroll += delta
	if p.scroll < 0 {
		p.scroll = 0
	}
}

// SetSize updates the pane dimensions.
func (p *PreviewPane) SetSize(width, height int) {
	p.width = width
	p.height = height
}

// Init implements tea.Model.
func (p *PreviewPane) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (p *PreviewPane) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case PreviewEventsMsg:
		if msg.TaskID == p.taskID() {
			p.SetEvents(msg.Events)
		}
	case tea.WindowSizeMsg:
		p.SetSize(msg.Width, msg.Height)
	}
	return p, nil
}

// View implements tea.Model.
func (p *PreviewPane) View() tea.View {
	return tea.NewView(p.Render())
}

// Render returns the pane content. No line is wider than the pane and there
// are never more lines than its height.
func (p *PreviewPane) Render() string {
	if p.width <= 0 || p.height <= 0 || p.item == nil || p.item.Implement == nil {
		return ""
	}
	task := p.item.Implement

	var lines []string
	add := func(prefix, text string) {
		lines = append(lines, previewWrap(prefix, text, p.width)...)
	}

	add("", task.Title)
	status := task.State + " · round " + fmt.Sprint(task.ReviewRound)
	if task.Model != "" {
		status += " · " + task.Model
	}
	add("", status)

	var extra []string
	if p.prURL != "" {
		extra = append(extra, previewPRLabel(p.prURL))
	}
	if task.State == stateChecks {
		extra = append(extra, "in checks "+tuiview.FormatChecksDuration(task.ChecksStartedAt, p.now()))
	}
	if len(extra) > 0 {
		add("", strings.Join(extra, " · "))
	}

	if reviews := p.reviewLines(); len(reviews) > 0 {
		lines = append(lines, "")
		add("", fmt.Sprintf("Reviews (round %d)", task.ReviewRound))
		for _, r := range reviews {
			add("  ", r)
		}
	}

	if len(lines) < p.height+p.scroll {
		lines = append(lines, "")
		add("", "History")
		switch {
		case !p.loaded:
			add("  ", "loading…")
		case len(p.events) == 0:
			add("  ", "no events yet")
		default:
			// Newest first; stop once the pane is full.
			for i := len(p.events) - 1; i >= 0 && len(lines) < p.height+p.scroll; i-- {
				e := p.events[i]
				add("  "+formatEventClock(e.CreatedAt)+" ", describeEvent(e))
			}
		}
	}

	if maxScroll := len(lines) - p.height; p.scroll > maxScroll {
		p.scroll = max(maxScroll, 0)
	}
	lines = lines[p.scroll:]
	if len(lines) > p.height {
		lines = lines[:p.height]
	}
	if p.focused && p.theme != nil && p.scroll == 0 && len(lines) > 0 {
		lines[0] = p.theme.PaneTitleFocused.Render(lines[0])
	}
	return strings.Join(lines, "\n")
}

// reviewLines returns one line per current-round reviewer, or a placeholder
// while the round has no reviewers yet. It returns nil when there is nothing
// to say about reviews in the item's state.
func (p *PreviewPane) reviewLines() []string {
	task := p.item.Implement
	if len(p.item.Reviews) == 0 {
		switch task.State {
		case stateChecks:
			return []string{"none yet: waiting for CI"}
		case stateReview:
			return []string{"none yet"}
		}
		return nil
	}
	lines := make([]string, 0, len(p.item.Reviews))
	for _, r := range p.item.Reviews {
		lines = append(lines, r.Model+" "+formatVerdict(r.Verdict))
	}
	return lines
}

func (p *PreviewPane) taskID() string {
	return previewTaskID(p.item)
}

func previewTaskID(item *tuiview.WorkItem) string {
	if item == nil || item.Implement == nil {
		return ""
	}
	return item.Implement.ID
}

// formatVerdict renders a review verdict, or "pending" when there is none yet.
func formatVerdict(verdict *string) string {
	if verdict == nil || *verdict == "" {
		return "… pending"
	}
	switch *verdict {
	case "approve":
		return "✔ approve"
	case "reject":
		return "✖ reject"
	}
	return *verdict
}

// previewPRLabel shortens a GitHub pull request URL to "PR #N"; any other
// link is shown as is.
func previewPRLabel(url string) string {
	if _, _, number, err := parsePRURL(url); err == nil {
		return fmt.Sprintf("PR #%d", number)
	}
	return "PR " + url
}

// formatEventClock formats an event timestamp as a wall-clock "15:04".
func formatEventClock(timestamp string) string {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "--:--"
	}
	return t.Format("15:04")
}

// describeEvent renders an event as a short readable phrase.
func describeEvent(e tuiclient.Event) string {
	note := ""
	if e.Note != nil {
		note = strings.TrimSpace(*e.Note)
	}
	var s string
	switch e.Kind {
	case "claim":
		s = "claimed by @" + shortActor(e.Actor)
	case "claim_released":
		s = "claim released by @" + shortActor(e.Actor)
	case "submit":
		s = "submitted"
	case "review":
		verdict := "reviewed"
		if e.Verdict != nil && *e.Verdict != "" {
			verdict = *e.Verdict
		}
		return verdict + " by @" + shortActor(e.Actor)
	case "checks_failed":
		s = "checks failed"
	case "transition":
		if note != "" {
			return note
		}
		s = "transition"
	default:
		s = strings.ReplaceAll(e.Kind, "_", " ")
	}
	if note != "" {
		s += ": " + note
	}
	return s
}

// shortActor keeps the last dash-separated part of an agent id, the part that
// tells workers apart ("worker-…-c4wqd" becomes "c4wqd").
func shortActor(actor string) string {
	if i := strings.LastIndex(actor, "-"); i >= 0 && i < len(actor)-1 {
		return actor[i+1:]
	}
	return actor
}

// previewWrap wraps text to width display cells. The first line starts with
// prefix and continuation lines are indented to match it. When the prefix
// leaves no room for content, it becomes part of the text instead, so nothing
// is lost. Every returned line is at most width cells wide.
func previewWrap(prefix, text string, width int) []string {
	indentWidth := ansi.StringWidth(prefix)
	if indentWidth >= width {
		text = strings.TrimSpace(prefix + text)
		prefix = ""
		indentWidth = 0
	}
	indent := strings.Repeat(" ", indentWidth)

	lines := wrapCells(text, width-indentWidth)
	for i, line := range lines {
		lead := indent
		if i == 0 {
			lead = prefix
		}
		lines[i] = lead + line
	}
	return lines
}

// wrapCells word-wraps text to limit display cells (limit >= 1), breaking a
// word that is wider than limit between grapheme clusters. A cluster wider
// than limit cannot be shown at all and is dropped.
func wrapCells(text string, limit int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		var line strings.Builder
		lineWidth := 0
		flush := func() {
			out = append(out, line.String())
			line.Reset()
			lineWidth = 0
		}
		for _, word := range strings.Fields(para) {
			w := ansi.StringWidth(word)
			switch {
			case lineWidth > 0 && lineWidth+1+w <= limit:
				line.WriteByte(' ')
				line.WriteString(word)
				lineWidth += 1 + w
				continue
			case lineWidth > 0:
				flush()
			}
			if w <= limit {
				line.WriteString(word)
				lineWidth = w
				continue
			}
			for rest := word; rest != ""; {
				g, gw := ansi.FirstGraphemeCluster(rest, ansi.GraphemeWidth)
				rest = rest[len(g):]
				if gw > limit {
					continue
				}
				if lineWidth+gw > limit {
					flush()
				}
				line.WriteString(g)
				lineWidth += gw
			}
		}
		flush()
	}
	return out
}
