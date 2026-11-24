package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
)

// PreviewPane renders a summary of one work item at a given width and height.
// It shows: title, status line, PR link, time in checks, reviews with verdicts, and event history.
type PreviewPane struct {
	task   tuiclient.TaskDetail
	events []tuiclient.Event
	width  int
	height int
}

// NewPreviewPane creates a new preview pane for the given task, events, width, and height.
func NewPreviewPane(task tuiclient.TaskDetail, events []tuiclient.Event, width, height int) *PreviewPane {
	return &PreviewPane{
		task:   task,
		events: events,
		width:  width,
		height: height,
	}
}

// Render returns the rendered content of the preview pane, constrained to width and height.
// The pane renders:
// 1. Title
// 2. Status line: state, round, model
// 3. PR link when known
// 4. Time in checks when in checks
// 5. Reviews with verdicts for the current round
// 6. Most recent events, newest first
// 7. Loading line if events are not yet available
func (p *PreviewPane) Render() string {
	if p.width <= 0 || p.height <= 0 {
		return ""
	}

	var lines []string
	lineLimit := p.height

	// Title
	wrappedTitle := wrapText(p.task.Title, p.width)
	for _, line := range strings.Split(wrappedTitle, "\n") {
		if len(lines) >= lineLimit {
			break
		}
		lines = append(lines, line)
	}

	if len(lines) >= lineLimit {
		return strings.Join(lines, "\n")
	}

	// Status line: state, round, model
	statusLine := fmt.Sprintf("%s · round %d · %s", p.task.State, p.task.ReviewRound, p.task.Model)
	lines = append(lines, statusLine)

	if len(lines) >= lineLimit {
		return strings.Join(lines, "\n")
	}

	// PR link when known
	for _, link := range p.task.Links {
		if link.Kind == "pr" {
			prefix := "PR "
			wrappedLink := wrapText(link.Value, p.width-len(prefix))
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

	if len(lines) >= lineLimit {
		return strings.Join(lines, "\n")
	}

	// Time in checks when in checks
	if p.task.State == stateChecks && p.task.ChecksStartedAt != nil {
		duration := formatChecksDuration(p.task.ChecksStartedAt, time.Now())
		checksLine := fmt.Sprintf("in checks · %s", duration)
		lines = append(lines, checksLine)
	}

	if len(lines) >= lineLimit {
		return strings.Join(lines, "\n")
	}

	// Reviews with verdicts for the current round
	reviewsAdded := false
	for _, link := range p.task.Links {
		if link.ReviewRound != nil && *link.ReviewRound == p.task.ReviewRound && link.Kind == "review" {
			if !reviewsAdded {
				lines = append(lines, "Reviews:")
				reviewsAdded = true
				if len(lines) >= lineLimit {
					break
				}
			}

			verdict := "pending"
			if link.TombstonedAt != nil {
				verdict = "done"
			}
			reviewLine := fmt.Sprintf("  %s", verdict)
			lines = append(lines, reviewLine)
			if len(lines) >= lineLimit {
				break
			}
		}
	}

	if len(lines) >= lineLimit {
		return strings.Join(lines, "\n")
	}

	// Event history: most recent first, as many as fit
	if len(p.events) == 0 {
		// Loading line if events are not yet available
		lines = append(lines, "History: loading...")
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

			wrappedEvent := wrapText(eventLine, p.width-2)
			for _, line := range strings.Split(wrappedEvent, "\n") {
				if len(lines) >= lineLimit {
					break
				}
				lines = append(lines, line)
			}
		}
	}

	result := strings.Join(lines[:min(len(lines), lineLimit)], "\n")
	return result
}

// formatEventTime formats an event timestamp as a short time string (e.g., "12:41").
func formatEventTime(timestamp string) string {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "00:00"
	}
	return t.Format("15:04")
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// max returns the larger of a and b.
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
