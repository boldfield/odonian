package tuiview

import (
	"testing"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
)

// mockTheme implements RowTheme for testing.
type mockTheme struct {
	styles map[string]lipgloss.Style
}

func (m *mockTheme) GetStatusStyle(state string) lipgloss.Style {
	if style, ok := m.styles[state]; ok {
		return style
	}
	return lipgloss.NewStyle()
}

func newMockTheme() RowTheme {
	return &mockTheme{
		styles: map[string]lipgloss.Style{
			"ready":       lipgloss.NewStyle(),
			"in_progress": lipgloss.NewStyle(),
			"checks":      lipgloss.NewStyle(),
			"review":      lipgloss.NewStyle(),
			"approved":    lipgloss.NewStyle(),
			"blocked":     lipgloss.NewStyle(),
			"failed":      lipgloss.NewStyle(),
			"done":        lipgloss.NewStyle(),
			"backlog":     lipgloss.NewStyle(),
			"abandoned":   lipgloss.NewStyle(),
		},
	}
}

func TestRenderRowWidth(t *testing.T) {
	theme := newMockTheme()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	testWidths := []int{46, 60, 100}

	tests := []struct {
		name  string
		state string
		build func() *WorkItem
	}{
		{
			name:  "ready",
			state: "ready",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:    "12345678-abcd-efgh-ijkl",
						Title: "Test task title",
						State: "ready",
						Model: "haiku",
						Kind:  "implement",
						Held:  false,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
		{
			name:  "in_progress",
			state: "in_progress",
			build: func() *WorkItem {
				assignee := "worker-12345678"
				leaseExpires := now.Add(5 * time.Minute).Format(time.RFC3339Nano)
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:             "87654321-dcba-hgfe-lkji",
						Title:          "In progress task",
						State:          "in_progress",
						Model:          "opus",
						Kind:           "implement",
						Held:           false,
						Assignee:       &assignee,
						LeaseExpiresAt: &leaseExpires,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
		{
			name:  "checks",
			state: "checks",
			build: func() *WorkItem {
				assignee := "worker-abcdefgh"
				checksStarted := now.Add(-15 * time.Minute).Format(time.RFC3339Nano)
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:              "aaaaaaaa-bbbb-cccc-dddd",
						Title:           "In checks task",
						State:           "checks",
						Model:           "sonnet",
						Kind:            "implement",
						Held:            false,
						Assignee:        &assignee,
						ChecksStartedAt: &checksStarted,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
		{
			name:  "review",
			state: "review",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:          "eeeeeeee-ffff-gggg-hhhh",
						Title:       "Under review task",
						State:       "review",
						Model:       "opus",
						Kind:        "implement",
						Held:        false,
						ReviewRound: 1,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
		{
			name:  "approved",
			state: "approved",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:    "iiiiiiii-jjjj-kkkk-llll",
						Title: "Approved task",
						State: "approved",
						Model: "haiku",
						Kind:  "implement",
						Held:  false,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
		{
			name:  "blocked",
			state: "blocked",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:    "mmmmmmmm-nnnn-oooo-pppp",
						Title: "Blocked task",
						State: "blocked",
						Model: "sonnet",
						Kind:  "implement",
						Held:  false,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
		{
			name:  "failed",
			state: "failed",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:    "qqqqqqqq-rrrr-ssss-tttt",
						Title: "Failed task",
						State: "failed",
						Model: "opus",
						Kind:  "implement",
						Held:  false,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
		{
			name:  "done",
			state: "done",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:    "uuuuuuuu-vvvv-wwww-xxxx",
						Title: "Done task",
						State: "done",
						Model: "haiku",
						Kind:  "implement",
						Held:  false,
					},
					Reviews: []*tuiclient.Task{},
					Merge:   nil,
				}
			},
		},
	}

	for _, tt := range tests {
		for _, width := range testWidths {
			t.Run(tt.name+"_width_"+string(rune(width)), func(t *testing.T) {
				wi := tt.build()
				row := RenderRow(wi, width, now, theme)

				// Split lines (may be multiple if review)
				lines := splitLines(row)
				for _, line := range lines {
					lineWidth := lipgloss.Width(line)
					if lineWidth > width {
						t.Errorf("row width %d exceeds limit %d for state %s: %q", lineWidth, width, tt.state, line)
					}
				}
			})
		}
	}
}

func TestRenderRowReviewersWithVerdicts(t *testing.T) {
	theme := newMockTheme()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	// Create a work item in review with one approved reviewer and one pending
	approve := "approve"
	wi := &WorkItem{
		Implement: &tuiclient.Task{
			ID:          "12345678-abcd-efgh-ijkl",
			Title:       "Task under review",
			State:       "review",
			Model:       "opus",
			Kind:        "implement",
			Held:        false,
			ReviewRound: 1,
		},
		Reviews: []*tuiclient.Task{
			{
				ID:          "aaaa-1111",
				Model:       "sonnet",
				State:       "review",
				Kind:        "review",
				Verdict:     &approve,
				ReviewRound: 1,
			},
			{
				ID:          "bbbb-2222",
				Model:       "haiku",
				State:       "review",
				Kind:        "review",
				Verdict:     nil, // pending
				ReviewRound: 1,
			},
		},
		Merge: nil,
	}

	row := RenderRow(wi, 100, now, theme)

	// Should have multiple lines
	lines := splitLines(row)
	if len(lines) != 2 {
		t.Errorf("expected 2 lines (main + reviewers), got %d", len(lines))
	}

	// Check main row
	if !hasSubstring(lines[0], "12345678") || !hasSubstring(lines[0], "Task under review") {
		t.Errorf("main row missing expected content: %q", lines[0])
	}

	// Check reviewer line contains both reviewers and verdicts
	reviewerLine := lines[1]
	if !hasSubstring(reviewerLine, "sonnet") || !hasSubstring(reviewerLine, "haiku") {
		t.Errorf("reviewer line missing model names: %q", reviewerLine)
	}
	if !hasSubstring(reviewerLine, "✔") {
		t.Errorf("reviewer line missing approved verdict: %q", reviewerLine)
	}
}

func TestRenderRowTitleTruncation(t *testing.T) {
	theme := newMockTheme()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	// Very long title that needs truncation
	longTitle := "This is a very long task title that should definitely be truncated when rendering to a narrow width"

	wi := &WorkItem{
		Implement: &tuiclient.Task{
			ID:    "12345678-abcd-efgh-ijkl",
			Title: longTitle,
			State: "ready",
			Model: "haiku",
			Kind:  "implement",
			Held:  false,
		},
		Reviews: []*tuiclient.Task{},
		Merge:   nil,
	}

	// Test at a narrow width
	width := 46
	row := RenderRow(wi, width, now, theme)

	lineWidth := lipgloss.Width(row)
	if lineWidth > width {
		t.Errorf("truncated row width %d exceeds limit %d: %q", lineWidth, width, row)
	}

	// Should contain ellipsis
	if !hasSubstring(row, "…") {
		t.Errorf("truncated title should contain ellipsis: %q", row)
	}
}

func TestRenderRowMinimumTitle(t *testing.T) {
	theme := newMockTheme()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	assignee := "very-long-worker-name-that-is-excessive"
	leaseExpires := now.Add(5 * time.Minute).Format(time.RFC3339Nano)

	wi := &WorkItem{
		Implement: &tuiclient.Task{
			ID:             "12345678-abcd-efgh-ijkl",
			Title:          "Task title",
			State:          "in_progress",
			Model:          "opus",
			Kind:           "implement",
			Held:           false,
			Assignee:       &assignee,
			LeaseExpiresAt: &leaseExpires,
		},
		Reviews: []*tuiclient.Task{},
		Merge:   nil,
	}

	// Test at very narrow width where detail should be dropped
	width := 40
	row := RenderRow(wi, width, now, theme)

	lineWidth := lipgloss.Width(row)
	if lineWidth > width {
		t.Errorf("row width %d exceeds limit %d: %q", lineWidth, width, row)
	}
}

func TestRenderRowHeld(t *testing.T) {
	theme := newMockTheme()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	wi := &WorkItem{
		Implement: &tuiclient.Task{
			ID:    "12345678-abcd-efgh-ijkl",
			Title: "Held task",
			State: "ready",
			Model: "haiku",
			Kind:  "implement",
			Held:  true, // This task is held
		},
		Reviews: []*tuiclient.Task{},
		Merge:   nil,
	}

	row := RenderRow(wi, 100, now, theme)

	// Should contain held prefix glyph ⏸
	if !hasSubstring(row, "⏸") {
		t.Errorf("held task should contain held prefix: %q", row)
	}
}

// Helper functions
func splitLines(s string) []string {
	return splitRunes([]rune(s), '\n')
}

func splitRunes(runes []rune, sep rune) []string {
	var result []string
	var current []rune
	for _, r := range runes {
		if r == sep {
			result = append(result, string(current))
			current = []rune{}
		} else {
			current = append(current, r)
		}
	}
	if len(current) > 0 {
		result = append(result, string(current))
	}
	return result
}

func hasSubstring(s string, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
