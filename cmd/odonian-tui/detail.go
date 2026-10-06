package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

// commandContextFunc is the function used to create exec commands (made mockable for testing).
var commandContextFunc func(ctx context.Context, name string, args ...string) *exec.Cmd = exec.CommandContext

// lookPathFunc is the function used to look up executables (made mockable for testing).
var lookPathFunc func(file string) (string, error) = exec.LookPath

// userHomeDirFunc is the function used to get the home directory (made mockable for testing).
var userHomeDirFunc func() (string, error) = os.UserHomeDir

// detailFetchedMsg carries the result of a GetTask + ListDocuments + ListEvents call for the detail view.
type detailFetchedMsg struct {
	task      tuiclient.TaskDetail
	documents []tuiclient.Document
	events    []tuiclient.Event
	err       error
}

// openerResultMsg carries a brief status message after an open-in-browser action.
// An empty message means success (no banner shown).
type openerResultMsg struct {
	message string
}

// fetchDetailCmd creates a command that fetches full task detail, the project's document list, and task events.
func (m *BoardModel) fetchDetailCmd(taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		task, err := m.client.GetTask(ctx, taskID)
		if err != nil {
			return detailFetchedMsg{err: fmt.Errorf("get task failed: %w", err)}
		}

		docs, err := m.client.ListDocuments(ctx, m.project.ID)
		if err != nil {
			// Non-fatal: we still show the task; opener actions will fail gracefully.
			docs = nil
		}

		events, err := m.client.ListEvents(ctx, taskID)
		if err != nil {
			// Non-fatal: we still show the task; event timeline will be empty.
			events = nil
		}

		return detailFetchedMsg{task: task, documents: docs, events: events}
	}
}

// resolveDocURL builds a VCS browse URL from a Document ref using the project's repo.
// If ref is already a URL (has a scheme), it is returned directly.
// Otherwise: <repo>/blob/<commit-or-main>/<ref>.
// Returns ("", errMsg) when the URL cannot be built.
//
// Note: only https-form repos (e.g. "https://github.com/owner/repo") are supported.
// SSH-form repos (e.g. "git@github.com:owner/repo") are out of scope.
func resolveDocURL(ref string, doc tuiclient.Document, projectRepo string) (string, string) {
	if ref == "" {
		return "", "document has no ref"
	}

	// If ref already looks like a URL, open it directly.
	if parsed, err := url.Parse(ref); err == nil && parsed.Scheme != "" {
		return ref, ""
	}

	// Need a repo to build the URL.
	if projectRepo == "" {
		return "", "project has no repo configured"
	}

	// Normalize the repo: strip trailing slash then trailing ".git" to avoid
	// producing a path like "…/repo.git/blob/…" which results in a 404 on GitHub.
	normalizedRepo := strings.TrimRight(projectRepo, "/")
	normalizedRepo = strings.TrimSuffix(normalizedRepo, ".git")

	branch := "main"
	if doc.Commit != nil && *doc.Commit != "" {
		branch = *doc.Commit
	}

	builtURL := normalizedRepo + "/blob/" + branch + "/" + strings.TrimLeft(ref, "/")
	return builtURL, ""
}

// openPRCmd creates a command that opens the task's PR link in the browser.
func (m *BoardModel) openPRCmd(task tuiclient.TaskDetail) tea.Cmd {
	return func() tea.Msg {
		var prURL string
		for _, link := range task.Links {
			if link.Kind == "pr" {
				prURL = link.Value
				break
			}
		}

		if prURL == "" {
			return openerResultMsg{message: "no PR link on this task"}
		}

		if err := m.urlOpener(prURL); err != nil {
			return openerResultMsg{message: fmt.Sprintf("open failed: %v", err)}
		}
		return openerResultMsg{}
	}
}

// openSourceDocCmd creates a command that opens the task's source document in the browser.
func (m *BoardModel) openSourceDocCmd(task tuiclient.TaskDetail, documents []tuiclient.Document) tea.Cmd {
	return func() tea.Msg {
		if task.DocumentID == "" {
			return openerResultMsg{message: "task has no source document"}
		}

		var foundDoc *tuiclient.Document
		for i := range documents {
			if documents[i].ID == task.DocumentID {
				foundDoc = &documents[i]
				break
			}
		}

		if foundDoc == nil {
			return openerResultMsg{message: "source document not found in project"}
		}

		openURL, errMsg := resolveDocURL(foundDoc.Ref, *foundDoc, m.project.Repo)
		if errMsg != "" {
			return openerResultMsg{message: errMsg}
		}

		if err := m.urlOpener(openURL); err != nil {
			return openerResultMsg{message: fmt.Sprintf("open failed: %v", err)}
		}
		return openerResultMsg{}
	}
}

// openDesignDocCmd creates a command that opens the project's base design document in the browser.
func (m *BoardModel) openDesignDocCmd(documents []tuiclient.Document) tea.Cmd {
	return func() tea.Msg {
		var foundDoc *tuiclient.Document
		for i := range documents {
			if documents[i].Kind == "design" {
				foundDoc = &documents[i]
				break
			}
		}

		if foundDoc == nil {
			return openerResultMsg{message: "no design document found for this project"}
		}

		openURL, errMsg := resolveDocURL(foundDoc.Ref, *foundDoc, m.project.Repo)
		if errMsg != "" {
			return openerResultMsg{message: errMsg}
		}

		if err := m.urlOpener(openURL); err != nil {
			return openerResultMsg{message: fmt.Sprintf("open failed: %v", err)}
		}
		return openerResultMsg{}
	}
}

// buildDetailContent builds the complete scrollable content for the detail view.
// It composes the header, metadata, events, result, and spec into a single string.
func (m *BoardModel) buildDetailContent(task tuiclient.TaskDetail) string {
	var b strings.Builder

	// Header: title, state, and project
	if m.project.Name != "" {
		wrappedProject := wrapText(m.project.Name, m.width-9)
		b.WriteString(fmt.Sprintf("Project: %s\n", wrappedProject))
	}
	wrappedTitle := wrapText(task.Title, m.width-6)
	b.WriteString(fmt.Sprintf("Task: %s\n", wrappedTitle))
	stateStr := fmt.Sprintf("State: %s", task.State)
	if task.Held {
		stateStr += "  [HELD]"
	}
	if task.Assignee != nil {
		stateStr += fmt.Sprintf("  Assignee: %s", *task.Assignee)
	}
	b.WriteString(stateStr + "\n")
	if task.Model != "" {
		b.WriteString(fmt.Sprintf("Model: %s\n", task.Model))
	}

	// Lease countdown
	if task.LeaseExpiresAt != nil {
		b.WriteString(fmt.Sprintf("Lease: %s\n", m.formatLeaseCountdown(*task.LeaseExpiresAt)))
	}

	// Priority
	priorityStr := "500"
	if task.Priority != nil {
		priorityStr = fmt.Sprintf("%d", *task.Priority)
	}
	if task.Held {
		b.WriteString(fmt.Sprintf("Priority: %s [HELD]\n", priorityStr))
	} else {
		b.WriteString(fmt.Sprintf("Priority: %s\n", priorityStr))
	}

	// Topic anchor
	if task.TopicAnchorID != nil {
		b.WriteString(fmt.Sprintf("Topic Anchor: %s\n", *task.TopicAnchorID))
	}

	// Timestamps
	b.WriteString(fmt.Sprintf("Created: %s\n", m.formatAbsTime(task.CreatedAt)))
	b.WriteString(fmt.Sprintf("Updated: %s\n", m.formatAbsTime(task.UpdatedAt)))

	// Dependencies resolved to titles
	if len(task.DependsOn) > 0 {
		b.WriteString("Depends on:\n")
		for _, depID := range task.DependsOn {
			title := m.resolveTaskTitle(depID)
			shortID := depID
			if len(shortID) > 8 {
				shortID = shortID[:8]
			}
			// Account for "  - " prefix (4 chars) and " (shortID)" suffix (max 11 chars)
			wrappedTitle := wrapText(title, m.width-15)
			b.WriteString(fmt.Sprintf("  - %s (%s)\n", wrappedTitle, shortID))
		}
	}

	// Links
	if len(task.Links) > 0 {
		b.WriteString("Links:\n")
		for _, link := range task.Links {
			prefix := fmt.Sprintf("  [%s] ", link.Kind)
			wrappedValue := wrapText(link.Value, m.width-len(prefix))
			formattedLink := prefix + wrappedValue
			b.WriteString(formattedLink + "\n")
		}
	}

	// Events timeline
	if len(m.detailEvents) > 0 {
		b.WriteString("Events:\n")
		for _, event := range m.detailEvents {
			timeStr := m.formatAbsTime(event.CreatedAt)
			b.WriteString(fmt.Sprintf("  [%s] %s: %s", timeStr, event.Actor, event.Kind))
			if event.Verdict != nil {
				b.WriteString(fmt.Sprintf(" (%s)", *event.Verdict))
			}
			b.WriteString("\n")
			if event.Note != nil && *event.Note != "" {
				b.WriteString(fmt.Sprintf("    %s\n", wrapText(*event.Note, m.width-4)))
			}
		}
	}

	// Result (wrapped to the view width so long summaries are fully readable, not truncated)
	if task.Result != nil && *task.Result != "" {
		b.WriteString("Result:\n")
		b.WriteString(wrapText(*task.Result, m.width))
		b.WriteString("\n")
	}

	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	// Spec content (wrapped to avoid horizontal overflow)
	b.WriteString(wrapText(task.Spec, m.width))

	return b.String()
}

// renderDetailView renders the full-screen detail view for the current task.
// The entire detail body is now in the scrollable viewport; this renders the viewport.
func (m *BoardModel) renderDetailView() string {
	var b strings.Builder

	// Status/opener message
	if m.detailMessage != "" {
		b.WriteString(fmt.Sprintf("» %s\n", m.detailMessage))
	}

	// The viewport holds the entire scrollable detail content
	b.WriteString(m.detailViewport.View())

	return b.String()
}

// wrapText wraps s to the given width on word boundaries, preserving existing newlines.
// Long words that exceed the width are hard-broken into width-sized chunks.
// Used for free-text fields (like a task result) that would otherwise overrun the terminal.
func wrapText(s string, width int) string {
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
			wlen := utf8.RuneCountInString(word)
			if j > 0 {
				if col+1+wlen > width {
					out.WriteByte('\n')
					col = 0
				} else {
					out.WriteByte(' ')
					col++
				}
			}
			// If word exceeds width, hard-break it into chunks
			if wlen > width {
				for col < width && len(word) > 0 {
					// Take up to (width - col) runes from word
					remaSpace := width - col
					var chunk string
					for len(word) > 0 && utf8.RuneCountInString(chunk) < remaSpace {
						r, sz := utf8.DecodeRuneInString(word)
						chunk += string(r)
						word = word[sz:]
					}
					out.WriteString(chunk)
					col += utf8.RuneCountInString(chunk)
					if len(word) > 0 {
						out.WriteByte('\n')
						col = 0
					}
				}
			} else {
				out.WriteString(word)
				col += wlen
			}
		}
	}
	return out.String()
}

// resolveTaskTitle looks up a task ID in the board's full task list and returns the title.
// Falls back to the truncated ID if not found.
func (m *BoardModel) resolveTaskTitle(taskID string) string {
	for _, tasks := range m.tasks {
		for _, t := range tasks {
			if t.ID == taskID {
				return t.Title
			}
		}
	}
	return taskID
}

// formatAbsTime formats a timestamp as a human-readable absolute time.
func (m *BoardModel) formatAbsTime(timestamp string) string {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return timestamp
	}
	return t.Format("2006-01-02 15:04:05 UTC")
}

// initDetailViewport initialises the full-content viewport for the detail view.
// It builds the complete scrollable content (header, events, result, spec) and seeds the viewport.
// Call this whenever the detail mode is entered or the window is resized.
func (m *BoardModel) initDetailViewport(task tuiclient.TaskDetail) {
	content := m.buildDetailContent(task)

	// Reserve lines only for the help bar (no longer for the header, which is now scrollable).
	// The help bar is 3 lines: separator + bar + nothing else.
	const reservedLines = 3
	vpHeight := m.height - reservedLines
	if vpHeight < 3 {
		vpHeight = 3
	}

	vp := viewport.New(m.width, vpHeight)
	vp.SetContent(content)
	// Preserve scroll position on resize, but clamp to the new content's valid range.
	if m.detailViewport.YOffset > 0 {
		maxOffset := max(0, vp.TotalLineCount()-vp.VisibleLineCount())
		clampedOffset := min(m.detailViewport.YOffset, maxOffset)
		vp.SetYOffset(clampedOffset)
	}
	m.detailViewport = vp
}

// renderDetailHelpBar returns the help bar text appropriate for detail view.
func (m *BoardModel) renderDetailHelpBar() string {
	base := "esc back   ↑/↓/pgup/pgdn scroll spec   o open PR   s source doc   d design doc   P switch project   i set priority   w move to front   e reset priority"
	if m.detailTask.State == stateReview {
		base += "   a approve   x reject"
	}
	if m.detailTask.State == stateApproved {
		base += "   m merge & complete"
	}
	return base
}

// defaultURLOpener is the production opener. It tries $BROWSER, then platform open commands.
func defaultURLOpener(rawURL string) error {
	// Try $BROWSER first.
	if browserEnv := os.Getenv("BROWSER"); browserEnv != "" {
		return exec.Command(browserEnv, rawURL).Start()
	}

	// macOS: "open" command.
	if _, err := exec.LookPath("open"); err == nil {
		return exec.Command("open", rawURL).Start()
	}

	// Linux: xdg-open.
	if _, err := exec.LookPath("xdg-open"); err == nil {
		return exec.Command("xdg-open", rawURL).Start()
	}

	return fmt.Errorf("no browser found: set $BROWSER or install xdg-open")
}

// defaultGHMerger merges a PR via the GitHub REST API with per-owner token support.
// Returns an error if parsing fails or the merge fails.
func defaultGHMerger(ctx context.Context, prURL string) error {
	// Parse the PR URL to extract owner, repo, and number.
	owner, repo, number, err := parsePRURL(prURL)
	if err != nil {
		return fmt.Errorf("merge failed: %w", err)
	}

	// Get the per-owner token from forge-tokens file.
	token, err := forge.OwnerToken(owner)
	if err != nil {
		return fmt.Errorf("merge failed: %w", err)
	}

	// Use the forge package to perform the squash merge.
	if err := forge.SquashMerge(ctx, owner, repo, number, token); err != nil {
		return fmt.Errorf("merge failed: %w", err)
	}
	return nil
}
