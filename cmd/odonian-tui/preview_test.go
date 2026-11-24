package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/boldfield/odonian/internal/tuiclient"
)

func TestPreviewPaneRender_WorkItemInChecks(t *testing.T) {
	now := "2026-11-24T12:00:00Z"
	task := tuiclient.TaskDetail{
		ID:              "task-123",
		Title:           "Test task",
		State:           stateChecks,
		Model:           "sonnet",
		ReviewRound:     1,
		ChecksStartedAt: &now,
	}
	events := []tuiclient.Event{}

	pane := NewPreviewPane(task, events, 50, 10)
	output := pane.Render()

	lines := strings.Split(output, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}

	// Check title
	if !strings.Contains(output, "Test task") {
		t.Errorf("expected title in output, got: %s", output)
	}

	// Check status line
	if !strings.Contains(output, stateChecks) {
		t.Errorf("expected state '%s' in output", stateChecks)
	}
	if !strings.Contains(output, "round 1") {
		t.Errorf("expected 'round 1' in output")
	}
	if !strings.Contains(output, "sonnet") {
		t.Errorf("expected 'sonnet' in output")
	}

	// Check time in checks
	if !strings.Contains(output, "in checks") {
		t.Errorf("expected 'in checks' in output")
	}
}

func TestPreviewPaneRender_WithPRLink(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateReview,
		Model:       "opus",
		ReviewRound: 2,
		Links: []tuiclient.TaskLink{
			{
				Kind:  "pr",
				Value: "https://github.com/owner/repo/pull/123",
			},
		},
	}
	events := []tuiclient.Event{}

	pane := NewPreviewPane(task, events, 60, 10)
	output := pane.Render()

	if !strings.Contains(output, "PR ") {
		t.Errorf("expected PR link prefix in output")
	}
	if !strings.Contains(output, "pull/123") {
		t.Errorf("expected PR URL in output")
	}
}

func TestPreviewPaneRender_WithReviews(t *testing.T) {
	reviewRound := 1
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateReview,
		Model:       "sonnet",
		ReviewRound: 1,
		Links: []tuiclient.TaskLink{
			{
				Kind:        "review",
				Value:       "review-data",
				ReviewRound: &reviewRound,
			},
		},
	}
	events := []tuiclient.Event{}

	pane := NewPreviewPane(task, events, 60, 10)
	output := pane.Render()

	if !strings.Contains(output, "Reviews:") {
		t.Errorf("expected 'Reviews:' in output")
	}
}

func TestPreviewPaneRender_WithHistory(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}

	verdict := "approve"
	events := []tuiclient.Event{
		{
			ID:        "event-1",
			TaskID:    "task-123",
			Actor:     "sonnet",
			Kind:      "submitted",
			Verdict:   nil,
			CreatedAt: "2026-11-24T12:00:00Z",
		},
		{
			ID:        "event-2",
			TaskID:    "task-123",
			Actor:     "opus",
			Kind:      "review",
			Verdict:   &verdict,
			CreatedAt: "2026-11-24T12:05:00Z",
		},
	}

	pane := NewPreviewPane(task, events, 60, 10)
	output := pane.Render()

	// Should have history header
	if !strings.Contains(output, "History:") {
		t.Errorf("expected 'History:' in output")
	}

	// Should have events (newest first)
	if !strings.Contains(output, "review") {
		t.Errorf("expected 'review' event in output")
	}
	if !strings.Contains(output, "submitted") {
		t.Errorf("expected 'submitted' event in output")
	}

	// Should have verdict
	if !strings.Contains(output, "approve") {
		t.Errorf("expected 'approve' verdict in output")
	}
}

func TestPreviewPaneRender_HistoryLoadingWhenEmpty(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	events := []tuiclient.Event{}

	pane := NewPreviewPane(task, events, 60, 10)
	output := pane.Render()

	if !strings.Contains(output, "History: loading...") {
		t.Errorf("expected 'History: loading...' in output when events are empty")
	}
}

func TestPreviewPaneRender_RespectHeight(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "A very long title that will wrap to multiple lines to test height limits on the preview pane render function",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}

	events := make([]tuiclient.Event, 20)
	for i := 0; i < 20; i++ {
		events[i] = tuiclient.Event{
			ID:        "event-" + string(rune(i)),
			TaskID:    "task-123",
			Actor:     "actor",
			Kind:      "event_kind",
			CreatedAt: "2026-11-24T12:00:00Z",
		}
	}

	pane := NewPreviewPane(task, events, 60, 5)
	output := pane.Render()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 5 {
		t.Errorf("expected at most 5 lines (height), got %d lines:\n%v", len(lines), lines)
	}
}

func TestPreviewPaneRender_RespectWidth(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "A very long title that should wrap to fit within the width constraint of the preview pane renderer",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	events := []tuiclient.Event{}

	pane := NewPreviewPane(task, events, 30, 20)
	output := pane.Render()

	lines := strings.Split(output, "\n")
	for i, line := range lines {
		width := utf8.RuneCountInString(line)
		if width > 30 {
			t.Errorf("line %d exceeds width of 30: got %d runes: %s", i, width, line)
		}
	}
}

func TestPreviewPaneRender_EmptyWidthHeight(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	events := []tuiclient.Event{}

	pane := NewPreviewPane(task, events, 0, 10)
	output := pane.Render()
	if output != "" {
		t.Errorf("expected empty output for zero width, got: %s", output)
	}

	pane = NewPreviewPane(task, events, 60, 0)
	output = pane.Render()
	if output != "" {
		t.Errorf("expected empty output for zero height, got: %s", output)
	}
}

func TestPreviewPaneRender_MostRecentEventsNewestFirst(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}

	events := []tuiclient.Event{
		{
			ID:        "event-1",
			TaskID:    "task-123",
			Kind:      "first",
			CreatedAt: "2026-11-24T12:00:00Z",
		},
		{
			ID:        "event-2",
			TaskID:    "task-123",
			Kind:      "second",
			CreatedAt: "2026-11-24T12:05:00Z",
		},
		{
			ID:        "event-3",
			TaskID:    "task-123",
			Kind:      "third",
			CreatedAt: "2026-11-24T12:10:00Z",
		},
	}

	pane := NewPreviewPane(task, events, 60, 20)
	output := pane.Render()

	// Find positions of events in output
	thirdPos := strings.Index(output, "third")
	secondPos := strings.Index(output, "second")
	firstPos := strings.Index(output, "first")

	if thirdPos == -1 || secondPos == -1 || firstPos == -1 {
		t.Fatalf("not all events found in output: %s", output)
	}

	// Newest (third) should appear before older (second) and (first)
	if thirdPos > secondPos || thirdPos > firstPos {
		t.Errorf("expected newest event (third) to appear first in output")
	}
	if secondPos > firstPos {
		t.Errorf("expected second newest event (second) to appear before oldest (first)")
	}
}

func TestPreviewPaneRender_PaneTooShortForAllEvents(t *testing.T) {
	task := tuiclient.TaskDetail{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}

	events := make([]tuiclient.Event, 100)
	for i := 0; i < 100; i++ {
		events[i] = tuiclient.Event{
			ID:        "event-" + string(rune(i)),
			TaskID:    "task-123",
			Kind:      "event",
			CreatedAt: "2026-11-24T12:00:00Z",
		}
	}

	pane := NewPreviewPane(task, events, 60, 3)
	output := pane.Render()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 3 {
		t.Errorf("expected at most 3 lines, got %d", len(lines))
	}
}

func TestFormatEventTime(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"2026-11-24T12:34:56Z", "12:34"},
		{"2026-11-24T00:00:00Z", "00:00"},
		{"invalid", "00:00"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := formatEventTime(tt.input)
			if result != tt.expected {
				t.Errorf("formatEventTime(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}
