package tuiview

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
)

// RowTheme defines the styling for row rendering.
type RowTheme interface {
	GetStatusStyle(state string) lipgloss.Style
}

// RenderRow renders a work item as a single row fitted to a given width.
// The row contains: glyph, title, then right-aligned status detail.
// For review status, adds an indented line listing each reviewer with their verdict.
// The row never exceeds the given width; title is truncated with ellipsis if needed.
// Right-hand detail is dropped before truncating the title below 12 cells.
// Priority appears only when it differs from the default (500).
//
// Parameters:
//   - wi: work item to render (implement task with optional reviews and merge)
//   - width: target width in display cells
//   - now: current time for duration calculations
//   - theme: provides Lip Gloss styles for colouring glyphs by status
func RenderRow(wi *WorkItem, width int, now time.Time, theme RowTheme) string {
	if wi == nil || wi.Implement == nil {
		return ""
	}

	impl := wi.Implement
	status := Status(impl, nil)
	glyph := Glyph(status, impl.Held)

	// Apply colour to glyph from theme
	var coloredGlyph string
	if theme != nil {
		stateStyle := theme.GetStatusStyle(impl.State)
		coloredGlyph = stateStyle.Render(glyph)
	} else {
		coloredGlyph = glyph
	}

	// Build the detail string based on state
	detail := buildDetailString(impl, now)

	// Build task ID display (first 8 chars)
	taskID := impl.ID
	if len(taskID) > 8 {
		taskID = taskID[:8]
	}

	// Measure the parts we know the size of
	glyphWidth := lipgloss.Width(glyph)
	detailWidth := lipgloss.Width(detail)

	// Space for: glyph + " " + taskID + " " + title + "  " + detail
	// Minimum space: glyph (1) + space (1) + taskID (8) + space (1) + min-title (12) + space-sep (2) + detail
	minTitleWidth := 12
	fixedWidth := glyphWidth + 1 + len(taskID) + 1 + 2 + detailWidth

	// Available width for title
	availableForTitle := width - fixedWidth
	if availableForTitle < minTitleWidth {
		// If not enough space, drop the detail
		availableForTitle = width - glyphWidth - 1 - len(taskID) - 1
		detail = ""
		detailWidth = 0
	}

	// Truncate title if necessary
	title := impl.Title
	titleWidth := lipgloss.Width(title)
	if titleWidth > availableForTitle && availableForTitle > 0 {
		title = truncateWithEllipsis(title, availableForTitle)
	}

	// Build the main row
	var row string
	if detail != "" {
		// Right-align the detail
		titleWidth = lipgloss.Width(title)
		spaceBetween := width - glyphWidth - 1 - len(taskID) - 1 - titleWidth - detailWidth
		if spaceBetween < 2 {
			spaceBetween = 2
		}
		row = fmt.Sprintf("%s %s %s%s%s", coloredGlyph, taskID, title, strings.Repeat(" ", spaceBetween), detail)
	} else {
		row = fmt.Sprintf("%s %s %s", coloredGlyph, taskID, title)
	}

	// Ensure row doesn't exceed width
	if lipgloss.Width(row) > width {
		row = truncateWithEllipsis(row, width)
	}

	// Add reviewer line if in review status
	var result string
	result = row
	if impl.State == "review" && len(wi.Reviews) > 0 {
		reviewerLine := buildReviewerLine(wi.Reviews, now, width)
		result = result + "\n" + reviewerLine
	}

	return result
}

// buildDetailString returns the right-aligned detail for a task based on its state.
func buildDetailString(task *tuiclient.Task, now time.Time) string {
	switch task.State {
	case "ready":
		return "ready"
	case "in_progress":
		if task.Assignee != nil {
			assignee := *task.Assignee
			if len(assignee) > 20 {
				assignee = assignee[:17] + "…"
			}
			duration := formatLeaseCountdown(task.LeaseExpiresAt, now)
			return fmt.Sprintf("%s %s", task.Model, duration)
		}
		return task.Model

	case "checks":
		return fmt.Sprintf("checks %s", formatChecksDuration(task.ChecksStartedAt, now))

	case "review":
		return fmt.Sprintf("review r%d", task.ReviewRound)

	case "approved":
		return "approved"

	case "blocked":
		return "blocked"

	case "failed":
		return "failed"

	case "done":
		return "done"

	case "backlog":
		return ""

	default:
		return task.State
	}
}

// formatLeaseCountdown formats the lease expiration time as a countdown.
func formatLeaseCountdown(expiresAt *string, now time.Time) string {
	if expiresAt == nil {
		return "no lease"
	}

	t, err := time.Parse(time.RFC3339Nano, *expiresAt)
	if err != nil {
		return "invalid"
	}

	if now.After(t) {
		return "EXPIRED"
	}

	remaining := t.Sub(now)
	minutes := int(remaining.Minutes())
	hours := int(remaining.Hours())
	secs := int(remaining.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%dm", hours, minutes%60)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm", minutes)
	}
	return fmt.Sprintf("%ds", secs)
}

// formatChecksDuration renders how long a task has been in checks, from checks_started_at.
func formatChecksDuration(startedAt *string, now time.Time) string {
	if startedAt == nil {
		return "unknown"
	}

	t, err := time.Parse(time.RFC3339Nano, *startedAt)
	if err != nil {
		return "unknown"
	}

	d := now.Sub(t)
	if d < 0 {
		d = 0
	}

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// buildReviewerLine builds an indented line listing reviewers and their verdicts.
func buildReviewerLine(reviews []*tuiclient.Task, now time.Time, width int) string {
	if len(reviews) == 0 {
		return ""
	}

	// Build reviewer strings
	var reviewerParts []string
	for _, review := range reviews {
		if review.Model == "" {
			continue
		}

		verdict := "pending"
		if review.Verdict != nil {
			verdict = *review.Verdict
		}

		var verdictGlyph string
		switch verdict {
		case "approve":
			verdictGlyph = "✔"
		case "reject":
			verdictGlyph = "✖"
		default:
			verdictGlyph = "·"
		}

		reviewerParts = append(reviewerParts, fmt.Sprintf("%s %s", review.Model, verdictGlyph))
	}

	if len(reviewerParts) == 0 {
		return ""
	}

	// Indent with 4 spaces
	reviewLine := "    " + strings.Join(reviewerParts, " · ")

	// Truncate if necessary
	if lipgloss.Width(reviewLine) > width {
		reviewLine = truncateWithEllipsis(reviewLine, width)
	}

	return reviewLine
}

// truncateWithEllipsis truncates a string to fit within the given display width, adding an ellipsis.
// Uses lipgloss.Width to measure display cells correctly (handles ANSI codes, unicode, etc).
func truncateWithEllipsis(s string, maxWidth int) string {
	if maxWidth < 1 {
		return ""
	}

	// Measure current width
	width := lipgloss.Width(s)
	if width <= maxWidth {
		return s
	}

	// Need to fit ellipsis (3 chars wide)
	targetWidth := maxWidth - 3
	if targetWidth < 0 {
		targetWidth = 0
	}

	// Iterate through the string, measuring display width as we go
	var result strings.Builder
	var currentWidth int
	for _, r := range s {
		charWidth := lipgloss.Width(string(r))
		if currentWidth+charWidth > targetWidth {
			break
		}
		result.WriteRune(r)
		currentWidth += charWidth
	}

	result.WriteString("…")
	return result.String()
}
