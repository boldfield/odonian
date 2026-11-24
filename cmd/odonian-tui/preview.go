package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/mattn/go-runewidth"
)

// PreviewPane renders a summary of one work item at a given width and height.
// It shows: title, status line, PR link, time in checks, reviews with verdicts, and event history.
type PreviewPane struct {
	workItem *tuiview.WorkItem
	detail   *tuiclient.TaskDetail
	events   []tuiclient.Event
	loaded   bool
	width    int
	height   int
	clock    func() time.Time
}

// NewPreviewPane creates a new preview pane for the given work item and optional detail.
// Detail is used for PR links; if nil, PR links won't be shown.
// Events are initially nil; SetEvents must be called to load them.
func NewPreviewPane(workItem *tuiview.WorkItem, width, height int) *PreviewPane {
	return &PreviewPane{
		workItem: workItem,
		detail:   nil,
		events:   nil,
		loaded:   false,
		width:    width,
		height:   height,
		clock:    time.Now,
	}
}

// WithDetail sets the TaskDetail for PR link access.
func (p *PreviewPane) WithDetail(detail *tuiclient.TaskDetail) *PreviewPane {
	p.detail = detail
	return p
}

// SetEvents sets the events and marks them as loaded.
func (p *PreviewPane) SetEvents(events []tuiclient.Event) {
	p.events = events
	p.loaded = true
}

// Init implements tea.Model.
func (p *PreviewPane) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (p *PreviewPane) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return p, nil
}

// View implements tea.Model and renders the preview pane content.
func (p *PreviewPane) View() tea.View {
	return tea.NewView(p.Render())
}

// Render returns the rendered content of the preview pane, constrained to width and height.
func (p *PreviewPane) Render() string {
	if p.width <= 0 || p.height <= 0 {
		return ""
	}

	var lines []string
	lineLimit := p.height

	// Title
	wrappedTitle := wrapTextByDisplayWidth(p.workItem.Implement.Title, p.width)
	for _, line := range strings.Split(wrappedTitle, "\n") {
		if len(lines) >= lineLimit {
			break
		}
		lines = append(lines, line)
	}

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// Status line: state, round, model
	statusLine := fmt.Sprintf("%s · round %d · %s", p.workItem.Implement.State, p.workItem.Implement.ReviewRound, p.workItem.Implement.Model)
	if runewidth.StringWidth(statusLine) > p.width {
		statusLine = truncateToDisplayWidth(statusLine, p.width)
	}
	lines = append(lines, statusLine)

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// PR link when known (from TaskDetail)
	if p.detail != nil {
		for _, link := range p.detail.Links {
			if link.Kind == "pr" {
				prefix := "PR "
				wrappedLink := wrapTextByDisplayWidth(link.Value, p.width-len(prefix))
				linkLine := prefix + wrappedLink
				for _, line := range strings.Split(linkLine, "\n") {
					if len(lines) >= lineLimit {
						break
					}
					lines = append(lines, line)
				}
				break
			}
		}
	}

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// Time in checks when in checks
	if p.workItem.Implement.State == stateChecks && p.workItem.Implement.ChecksStartedAt != nil {
		duration := formatChecksDuration(p.workItem.Implement.ChecksStartedAt, p.clock())
		checksLine := fmt.Sprintf("in checks · %s", duration)
		lines = append(lines, checksLine)
	}

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// Reviews with verdicts for the current round
	if len(p.workItem.Reviews) > 0 {
		lines = append(lines, "Reviews:")
		if len(lines) >= lineLimit {
			return joinLines(lines, lineLimit)
		}

		for _, review := range p.workItem.Reviews {
			verdict := "pending"
			if review.Verdict != nil {
				verdict = *review.Verdict
			}
			reviewLine := fmt.Sprintf("  %s (%s)", review.Model, verdict)
			if runewidth.StringWidth(reviewLine) > p.width {
				reviewLine = truncateToDisplayWidth(reviewLine, p.width)
			}
			lines = append(lines, reviewLine)
			if len(lines) >= lineLimit {
				return joinLines(lines, lineLimit)
			}
		}
	}

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// Event history: most recent first, as many as fit
	if !p.loaded {
		// Loading line if events have not yet been loaded
		lines = append(lines, "History: loading...")
	} else if len(p.events) == 0 {
		// No history header if there are no events
	} else {
		if len(lines) < lineLimit {
			lines = append(lines, "History:")
		}

		// Reverse events to show newest first
		for i := len(p.events) - 1; i >= 0 && len(lines) < lineLimit; i-- {
			event := p.events[i]
			timeStr := formatEventTime(event.CreatedAt)
			eventLine := fmt.Sprintf("  %s %s", timeStr, event.Kind)
			if event.Verdict != nil {
				eventLine += fmt.Sprintf(" (%s)", *event.Verdict)
			}

			wrappedEvent := wrapTextByDisplayWidth(eventLine, p.width-2)
			for _, line := range strings.Split(wrappedEvent, "\n") {
				if len(lines) >= lineLimit {
					break
				}
				lines = append(lines, line)
			}
		}
	}

	return joinLines(lines, lineLimit)
}

// formatEventTime formats an event timestamp as a short time string (e.g., "12:41").
func formatEventTime(timestamp string) string {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "00:00"
	}
	return t.Format("15:04")
}

// wrapTextByDisplayWidth wraps s to the given display width on word boundaries,
// accounting for CJK and other wide characters using display cells.
// Preserves existing newlines and hard-breaks long words.
func wrapTextByDisplayWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		col := 0
		for j, word := range strings.Fields(line) {
			wwidth := runewidth.StringWidth(word)
			if j > 0 {
				if col+1+wwidth > width {
					out.WriteByte('\n')
					col = 0
				} else {
					out.WriteByte(' ')
					col++
				}
			}
			// If word exceeds width, hard-break it into chunks
			if wwidth > width {
				for col < width && len(word) > 0 {
					remaSpace := width - col
					var chunk string
					for len(word) > 0 && runewidth.StringWidth(chunk) < remaSpace {
						r, sz := utf8.DecodeRuneInString(word)
						chunk += string(r)
						word = word[sz:]
					}
					out.WriteString(chunk)
					col += runewidth.StringWidth(chunk)
					if len(word) > 0 {
						out.WriteByte('\n')
						col = 0
					}
				}
			} else {
				out.WriteString(word)
				col += wwidth
			}
		}
	}
	return out.String()
}

// truncateToDisplayWidth truncates s to fit within the given display width.
func truncateToDisplayWidth(s string, width int) string {
	if width < 0 {
		width = 0
	}
	var out strings.Builder
	w := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if w+rw > width {
			break
		}
		out.WriteRune(r)
		w += rw
	}
	return out.String()
}

// joinLines joins lines and truncates to lineLimit.
func joinLines(lines []string, lineLimit int) string {
	if len(lines) > lineLimit {
		lines = lines[:lineLimit]
	}
	return strings.Join(lines, "\n")
}
