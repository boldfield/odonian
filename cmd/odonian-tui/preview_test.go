package main

import (
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/mattn/go-runewidth"
)

func TestPreviewPaneRender_WorkItemInChecks(t *testing.T) {
	now := "2026-11-24T12:00:00Z"
	impl := &tuiclient.Task{
		ID:              "task-123",
		Title:           "Test task",
		State:           stateChecks,
		Model:           "sonnet",
		ReviewRound:     1,
		ChecksStartedAt: &now,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	mockClock := func() time.Time {
		t, _ := time.Parse(time.RFC3339Nano, "2026-11-24T12:00:00Z")
		return t
	}

	pane := NewPreviewPane(workItem, 50, 10)
	pane.clock = mockClock
	pane.SetEvents([]tuiclient.Event{})

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
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateReview,
		Model:       "opus",
		ReviewRound: 2,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	detail := &tuiclient.TaskDetail{
		ID:    "task-123",
		Title: "Test task",
		Links: []tuiclient.TaskLink{
			{
				Kind:  "pr",
				Value: "https://github.com/owner/repo/pull/123",
			},
		},
	}

	pane := NewPreviewPane(workItem, 60, 10)
	pane.WithDetail(detail)
	pane.SetEvents([]tuiclient.Event{})

	output := pane.Render()

	if !strings.Contains(output, "PR ") {
		t.Errorf("expected PR link prefix in output")
	}
	if !strings.Contains(output, "pull/123") {
		t.Errorf("expected PR URL in output")
	}
}

func TestPreviewPaneRender_WithOneApprovedOneRejectingReviewer(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateReview,
		Model:       "sonnet",
		ReviewRound: 1,
	}

	approved := "approve"
	reviews := []*tuiclient.Task{
		{
			ID:      "review-1",
			Kind:    "review",
			Model:   "opus",
			Verdict: &approved,
		},
		{
			ID:      "review-2",
			Kind:    "review",
			Model:   "haiku",
			Verdict: nil,
		},
	}

	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   reviews,
	}

	pane := NewPreviewPane(workItem, 60, 10)
	pane.SetEvents([]tuiclient.Event{})

	output := pane.Render()

	if !strings.Contains(output, "Reviews:") {
		t.Errorf("expected 'Reviews:' in output")
	}
	if !strings.Contains(output, "opus") {
		t.Errorf("expected 'opus' reviewer in output")
	}
	if !strings.Contains(output, "approve") {
		t.Errorf("expected 'approve' verdict in output")
	}
	if !strings.Contains(output, "haiku") {
		t.Errorf("expected 'haiku' reviewer in output")
	}
	if !strings.Contains(output, "pending") {
		t.Errorf("expected 'pending' verdict in output")
	}
}

func TestPreviewPaneRender_WithHistory(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
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

	pane := NewPreviewPane(workItem, 60, 10)
	pane.SetEvents(events)
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

func TestPreviewPaneRender_HistoryLoading(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 60, 10)
	// Don't call SetEvents - leave it in loading state
	output := pane.Render()

	if !strings.Contains(output, "History: loading...") {
		t.Errorf("expected 'History: loading...' in output when events not yet loaded")
	}
}

func TestPreviewPaneRender_HistoryLoadedEmpty(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 60, 10)
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()

	if strings.Contains(output, "History: loading...") {
		t.Errorf("expected no loading message when events are loaded but empty")
	}
	if strings.Contains(output, "History:") {
		t.Errorf("expected no History header when events are empty")
	}
}

func TestPreviewPaneRender_RespectHeight(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "A very long title that will wrap to multiple lines to test height limits on the preview pane render function",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
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

	pane := NewPreviewPane(workItem, 60, 5)
	pane.SetEvents(events)
	output := pane.Render()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 5 {
		t.Errorf("expected at most 5 lines (height), got %d lines:\n%v", len(lines), lines)
	}
}

func TestPreviewPaneRender_RespectWidth(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "A very long title that should wrap to fit within the width constraint of the preview pane renderer",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 30, 20)
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()

	lines := strings.Split(output, "\n")
	for i, line := range lines {
		width := runewidth.StringWidth(line)
		if width > 30 {
			t.Errorf("line %d exceeds width of 30: got %d display cells: %s", i, width, line)
		}
	}
}

func TestPreviewPaneRender_RespectWidthNarrow(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "sonnet",
		ReviewRound: 1,
		ChecksStartedAt: func() *string {
			s := "2026-11-24T12:00:00Z"
			return &s
		}(),
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	mockClock := func() time.Time {
		t, _ := time.Parse(time.RFC3339Nano, "2026-11-24T12:18:00Z")
		return t
	}

	pane := NewPreviewPane(workItem, 12, 20)
	pane.clock = mockClock
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()

	lines := strings.Split(output, "\n")
	for i, line := range lines {
		width := runewidth.StringWidth(line)
		if width > 12 {
			t.Errorf("line %d exceeds width of 12: got %d display cells: %s", i, width, line)
		}
	}
}

func TestPreviewPaneRender_EmptyWidthHeight(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
	}

	pane := NewPreviewPane(workItem, 0, 10)
	pane.SetEvents([]tuiclient.Event{})
	output := pane.Render()
	if output != "" {
		t.Errorf("expected empty output for zero width, got: %s", output)
	}

	pane = NewPreviewPane(workItem, 60, 0)
	pane.SetEvents([]tuiclient.Event{})
	output = pane.Render()
	if output != "" {
		t.Errorf("expected empty output for zero height, got: %s", output)
	}
}

func TestPreviewPaneRender_MostRecentEventsNewestFirst(t *testing.T) {
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
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

	pane := NewPreviewPane(workItem, 60, 20)
	pane.SetEvents(events)
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
	impl := &tuiclient.Task{
		ID:          "task-123",
		Title:       "Test task",
		State:       stateInProgress,
		Model:       "haiku",
		ReviewRound: 1,
	}
	workItem := &tuiview.WorkItem{
		Implement: impl,
		Reviews:   []*tuiclient.Task{},
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

	pane := NewPreviewPane(workItem, 60, 5)
	pane.SetEvents(events)
	output := pane.Render()

	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) > 5 {
		t.Errorf("expected at most 5 lines, got %d", len(lines))
	}

	// Check that newest events are kept, not oldest (should have History: header and at least one event line)
	if !strings.Contains(output, "History:") {
		t.Errorf("expected History header in output")
	}
	if !strings.Contains(output, "event") {
		t.Errorf("expected events in output")
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
