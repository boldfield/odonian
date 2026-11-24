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

// EventsLoadedMsg signals that events have arrived and should be displayed.
type EventsLoadedMsg struct {
	Events []tuiclient.Event
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
	switch msg := msg.(type) {
	case EventsLoadedMsg:
		p.SetEvents(msg.Events)
	case tea.WindowSizeMsg:
		p.width = msg.Width
		p.height = msg.Height
	}
	return p, nil
}

// SetSize updates the pane's width and height.
func (p *PreviewPane) SetSize(width, height int) {
	p.width = width
	p.height = height
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
	addLineWithWrap(&lines, p.workItem.Implement.Title, "", p.width, lineLimit)
	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// Status line: state, round, model
	statusLine := fmt.Sprintf("%s · round %d · %s", p.workItem.Implement.State, p.workItem.Implement.ReviewRound, p.workItem.Implement.Model)
	addLineWithWrap(&lines, statusLine, "", p.width, lineLimit)

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// PR link when known (from TaskDetail)
	if p.detail != nil {
		for _, link := range p.detail.Links {
			if link.Kind == "pr" {
				prefix := "PR "
				addLineWithWrap(&lines, link.Value, prefix, p.width, lineLimit)
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
		addLineWithWrap(&lines, checksLine, "", p.width, lineLimit)
	}

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// Reviews with verdicts for the current round
	if len(p.workItem.Reviews) > 0 {
		if len(lines) < lineLimit {
			lines = append(lines, previewTruncate(fmt.Sprintf("Reviews: (round %d)", p.workItem.Implement.ReviewRound), p.width))
		}
		if len(lines) >= lineLimit {
			return joinLines(lines, lineLimit)
		}

		for _, review := range p.workItem.Reviews {
			if len(lines) >= lineLimit {
				break
			}
			verdict := "pending"
			if review.Verdict != nil {
				verdict = *review.Verdict
			}
			reviewLine := fmt.Sprintf("  %s (%s)", review.Model, verdict)
			addLineWithWrap(&lines, reviewLine, "", p.width, lineLimit)
		}
	}

	if len(lines) >= lineLimit {
		return joinLines(lines, lineLimit)
	}

	// Event history: most recent first, as many as fit
	if !p.loaded {
		// Loading line if events have not yet been loaded
		if len(lines) < lineLimit {
			lines = append(lines, previewTruncate("History: loading...", p.width))
		}
	} else if len(p.events) > 0 {
		if len(lines) < lineLimit {
			lines = append(lines, previewTruncate("History:", p.width))
		}

		// Reverse events to show newest first
		for i := len(p.events) - 1; i >= 0 && len(lines) < lineLimit; i-- {
			event := p.events[i]
			timeStr := formatEventTime(event.CreatedAt)
			prefix := fmt.Sprintf("  %s ", timeStr)
			eventContent := event.Kind
			if event.Verdict != nil {
				eventContent += fmt.Sprintf(" (%s)", *event.Verdict)
			}
			addLineWithWrap(&lines, eventContent, prefix, p.width, lineLimit)
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

// addLineWithWrap wraps text, adds prefix only to the first line, indents continuation lines,
// and appends all lines to the output up to the line limit.
func addLineWithWrap(lines *[]string, text, prefix string, width, lineLimit int) {
	if len(*lines) >= lineLimit {
		return
	}

	prefixWidth := runewidth.StringWidth(prefix)
	if prefixWidth >= width {
		// The prefix would leave no room for content; drop it (and the
		// continuation indent) so the content gets the full pane width.
		prefix = ""
		prefixWidth = 0
	}
	contentWidth := width
	if contentWidth < 1 {
		contentWidth = 1
	}
	contentWidth -= prefixWidth

	// Wrap the content to the available width by display cells
	wrappedLines := wrapByDisplayWidth(text, contentWidth)

	for i, line := range wrappedLines {
		if len(*lines) >= lineLimit {
			break
		}

		var fullLine string
		if i == 0 {
			// First line gets the prefix
			fullLine = prefix + line
		} else {
			// Continuation lines get indented
			indent := strings.Repeat(" ", prefixWidth)
			fullLine = indent + line
		}

		// Ensure it fits in the pane width
		fullLine = previewTruncate(fullLine, width)
		*lines = append(*lines, fullLine)
	}
}

// wrapByDisplayWidth wraps s by display cells, returning a slice of wrapped lines.
// Accounts for wide characters (CJK, etc.) and wraps on word boundaries when possible.
func wrapByDisplayWidth(s string, width int) []string {
	if width <= 0 {
		return []string{s}
	}

	var result []string

	for _, line := range strings.Split(s, "\n") {
		if runewidth.StringWidth(line) <= width {
			result = append(result, line)
			continue
		}

		// Need to wrap this line
		var currentLine strings.Builder
		col := 0

		for _, word := range strings.Fields(line) {
			wordWidth := runewidth.StringWidth(word)

			// Check if we need to wrap
			if col > 0 && col+1+wordWidth > width {
				// Start a new line
				result = append(result, currentLine.String())
				currentLine.Reset()
				col = 0
			}

			// Add space if not at start of line
			if col > 0 {
				currentLine.WriteByte(' ')
				col++
			}

			// If word exceeds width, hard-break it
			if wordWidth > width {
				// Flush current line if not empty
				if col > 0 {
					result = append(result, currentLine.String())
					currentLine.Reset()
					col = 0
				}

				// Hard-break the long word
				for len(word) > 0 {
					chunk := ""
					chunkWidth := 0

					for len(word) > 0 {
						r, sz := utf8.DecodeRuneInString(word)
						rw := runewidth.RuneWidth(r)

						if chunkWidth+rw > width {
							break
						}

						chunk += string(r)
						chunkWidth += rw
						word = word[sz:]
					}

					if chunk == "" && len(word) > 0 {
						// Can't fit any rune in this width; skip the rune if width is too small
						r, sz := utf8.DecodeRuneInString(word)
						if runewidth.RuneWidth(r) > 1 && width < runewidth.RuneWidth(r) {
							// Wide rune at too-narrow width; replace with placeholder
							chunk = "?"
							word = word[sz:]
						} else {
							// Single-width rune; fit it anyway
							chunk = string(r)
							word = word[sz:]
						}
					}

					if chunk != "" {
						result = append(result, chunk)
						col = 0
					}
				}
			} else {
				// Word fits, add it
				currentLine.WriteString(word)
				col += wordWidth
			}
		}

		if currentLine.Len() > 0 {
			result = append(result, currentLine.String())
		}
	}

	return result
}

// previewTruncate truncates s to fit within the given display width.
func previewTruncate(s string, width int) string {
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
