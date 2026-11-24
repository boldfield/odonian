package tuiview

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/charmbracelet/x/ansi"
)

// claimableSet constructs a set for Status() based on a single claimable flag.
func claimableSet(taskID string, claimable bool) map[string]bool {
	if claimable {
		return map[string]bool{taskID: true}
	}
	return map[string]bool{}
}

// RowTheme defines the styling for row rendering.
type RowTheme interface {
	GetStatusStyle(state string) lipgloss.Style
}

// statusTypeToString converts a computed StatusType to a string for theme styling.
func statusTypeToString(st StatusType) string {
	switch st {
	case StatusReadyClaimable:
		return "ready"
	case StatusReadyWaiting:
		return "ready"
	case StatusBacklogNotPromoted:
		return "backlog"
	case StatusBacklogWaiting:
		return "backlog"
	case StatusInProgress:
		return "in_progress"
	case StatusChecks:
		return "checks"
	case StatusReview:
		return "review"
	case StatusApproved:
		return "approved"
	case StatusBlocked:
		return "blocked"
	case StatusFailed:
		return "failed"
	case StatusDone:
		return "done"
	case StatusAbandoned:
		return "abandoned"
	default:
		return "unknown"
	}
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
//   - claimable: whether the task is claimable (ready and not waiting on a dependency)
func RenderRow(wi *WorkItem, width int, now time.Time, theme RowTheme, claimable bool) string {
	if wi == nil || wi.Implement == nil {
		return ""
	}

	impl := wi.Implement
	status := Status(impl, claimableSet(impl.ID, claimable))
	glyph := Glyph(status, impl.Held)

	// Apply colour to glyph from theme, using computed status for the color
	var coloredGlyph string
	if theme != nil {
		stateStyle := theme.GetStatusStyle(statusTypeToString(status))
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

	// Add priority if non-default (default is 500)
	var priorityStr string
	if impl.Priority != nil && *impl.Priority != 500 {
		priorityStr = fmt.Sprintf(" P:%d", int(*impl.Priority))
	}

	// Measure the parts we know the size of
	glyphWidth := lipgloss.Width(glyph)
	detailWidth := lipgloss.Width(detail)
	priorityWidth := lipgloss.Width(priorityStr)

	// Space for: glyph + " " + taskID + " " + title + priorityStr + "  " + detail
	// Minimum space: glyph (1) + space (1) + taskID (8) + space (1) + min-title (12) + space-sep (2) + detail
	minTitleWidth := 12
	fixedWidth := glyphWidth + 1 + len(taskID) + 1 + 2 + detailWidth + priorityWidth

	// Available width for title
	availableForTitle := width - fixedWidth
	if availableForTitle < minTitleWidth {
		// If not enough space, drop the detail
		availableForTitle = width - glyphWidth - 1 - len(taskID) - 1 - priorityWidth
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
		spaceBetween := width - glyphWidth - 1 - len(taskID) - 1 - titleWidth - priorityWidth - detailWidth
		if spaceBetween < 2 {
			spaceBetween = 2
		}
		row = fmt.Sprintf("%s %s %s%s%s%s", coloredGlyph, taskID, title, priorityStr, strings.Repeat(" ", spaceBetween), detail)
	} else {
		row = fmt.Sprintf("%s %s %s%s", coloredGlyph, taskID, title, priorityStr)
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
		holder := ""
		if task.Assignee != nil {
			holder = *task.Assignee
			if lipgloss.Width(holder) > 20 {
				holder = ansi.Truncate(holder, 17, "")
				holder = holder + "…"
			}
			holder = "@" + holder
		}
		// The API exposes no claim time (updated_at is rewritten by every lease
		// heartbeat), so elapsed time held cannot be shown honestly. Show the
		// lease time remaining instead, labelled as such.
		lease := "lease " + FormatLeaseCountdown(task.LeaseExpiresAt, now)
		if holder == "" {
			return fmt.Sprintf("%s %s", task.Model, lease)
		}
		return fmt.Sprintf("%s %s %s", task.Model, holder, lease)

	case "checks":
		return fmt.Sprintf("checks %s", FormatChecksDuration(task.ChecksStartedAt, now))

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
			// updated_at changes on every heartbeat, so no elapsed time is shown.
			verdictGlyph = "…"
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
// Uses ansi.Truncate to handle ANSI escape sequences properly without cutting through them.
func truncateWithEllipsis(s string, maxWidth int) string {
	if maxWidth < 1 {
		return ""
	}

	// Measure current width
	width := lipgloss.Width(s)
	if width <= maxWidth {
		return s
	}

	// Reserve 1 cell for ellipsis
	targetWidth := maxWidth - 1
	if targetWidth < 0 {
		targetWidth = 0
	}

	// Use ansi.Truncate to handle ANSI codes properly
	truncated := ansi.Truncate(s, targetWidth, "")
	return truncated + "…"
}
