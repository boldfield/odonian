package tuiview

import (
	"fmt"
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

// newStyledMockTheme returns a theme with colors to test ANSI-aware width handling.
func newStyledMockTheme() RowTheme {
	return &mockTheme{
		styles: map[string]lipgloss.Style{
			"ready":       lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
			"in_progress": lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
			"checks":      lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
			"review":      lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
			"approved":    lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
			"blocked":     lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
			"failed":      lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
			"done":        lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
			"backlog":     lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
			"abandoned":   lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
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
		{
			name:  "backlog",
			state: "backlog",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:    "yyyyyyyy-zzzz-aaaa-bbbb",
						Title: "Backlog task",
						State: "backlog",
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
			name:  "abandoned",
			state: "abandoned",
			build: func() *WorkItem {
				return &WorkItem{
					Implement: &tuiclient.Task{
						ID:    "cccccccc-dddd-eeee-ffff",
						Title: "Abandoned task",
						State: "abandoned",
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
			t.Run(tt.name+"_width_"+fmt.Sprint(width), func(t *testing.T) {
				wi := tt.build()
				claimable := tt.state == "ready" // ready tasks are claimable by default in tests
				row := RenderRow(wi, width, now, theme, claimable)

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

	row := RenderRow(wi, 100, now, theme, false)

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
	row := RenderRow(wi, width, now, theme, true)

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
	row := RenderRow(wi, width, now, theme, false)

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

	row := RenderRow(wi, 100, now, theme, true)

	// Should contain held prefix glyph ⏸
	if !hasSubstring(row, "⏸") {
		t.Errorf("held task should contain held prefix: %q", row)
	}
}

func TestRenderRowPriority(t *testing.T) {
	theme := newMockTheme()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		priority   *int64
		shouldShow bool
	}{
		{
			name:       "priority_900",
			priority:   ptrInt64(900),
			shouldShow: true,
		},
		{
			name:       "priority_500",
			priority:   ptrInt64(500),
			shouldShow: false, // default, should be hidden
		},
		{
			name:       "priority_nil",
			priority:   nil,
			shouldShow: false, // nil is treated as default
		},
		{
			name:       "priority_100",
			priority:   ptrInt64(100),
			shouldShow: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wi := &WorkItem{
				Implement: &tuiclient.Task{
					ID:       "12345678-abcd-efgh-ijkl",
					Title:    "Task with priority",
					State:    "ready",
					Model:    "haiku",
					Kind:     "implement",
					Held:     false,
					Priority: tt.priority,
				},
				Reviews: []*tuiclient.Task{},
				Merge:   nil,
			}

			row := RenderRow(wi, 100, now, theme, true)

			hasPriority := hasSubstring(row, "P:")
			if hasPriority != tt.shouldShow {
				if tt.shouldShow {
					t.Errorf("priority should be shown but was not: %q", row)
				} else {
					t.Errorf("priority should be hidden but was shown: %q", row)
				}
			}
		})
	}
}

func TestRenderRowStyledTheme(t *testing.T) {
	theme := newStyledMockTheme()

	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	testWidths := []int{46, 60, 100}

	wi := &WorkItem{
		Implement: &tuiclient.Task{
			ID:    "12345678-abcd-efgh-ijkl",
			Title: "Task with styled theme",
			State: "ready",
			Model: "haiku",
			Kind:  "implement",
			Held:  false,
		},
		Reviews: []*tuiclient.Task{},
		Merge:   nil,
	}

	for _, width := range testWidths {
		t.Run(fmt.Sprintf("styled_width_%d", width), func(t *testing.T) {
			row := RenderRow(wi, width, now, theme, true)
			lines := splitLines(row)
			for _, line := range lines {
				lineWidth := lipgloss.Width(line)
				if lineWidth > width {
					t.Errorf("row width %d exceeds limit %d: %q", lineWidth, width, line)
				}
			}
		})
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

func ptrInt64(i int64) *int64 {
	return &i
}

func ptrInt(i int) *int {
	return &i
}

func TestRenderRowReadyClaimableGlyph(t *testing.T) {
	theme := newMockTheme()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

	wi := &WorkItem{
		Implement: &tuiclient.Task{
			ID:    "12345678-abcd-efgh-ijkl",
			Title: "Ready task",
			State: "ready",
			Model: "haiku",
			Kind:  "implement",
			Held:  false,
		},
		Reviews: []*tuiclient.Task{},
		Merge:   nil,
	}

	tests := []struct {
		name          string
		claimable     bool
		expectedGlyph string
	}{
		{"ready_claimable", true, "○"},
		{"ready_waiting", false, "⛓"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			row := RenderRow(wi, 100, now, theme, tt.claimable)
			if !hasSubstring(row, tt.expectedGlyph) {
				t.Errorf("expected glyph %q for claimable=%v, got: %q", tt.expectedGlyph, tt.claimable, row)
			}
		})
	}
}

func TestRenderRowInProgressDetail(t *testing.T) {
	theme := newMockTheme()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	updatedAt := now.Add(-15 * time.Minute).Format(time.RFC3339Nano)
	assignee := "worker-example"

	wi := &WorkItem{
		Implement: &tuiclient.Task{
			ID:             "12345678-abcd-efgh-ijkl",
			Title:          "In progress task",
			State:          "in_progress",
			Model:          "haiku",
			Kind:           "implement",
			Held:           false,
			Assignee:       &assignee,
			UpdatedAt:      updatedAt,
			LeaseExpiresAt: ptrString(now.Add(10 * time.Minute).Format(time.RFC3339Nano)),
		},
		Reviews: []*tuiclient.Task{},
		Merge:   nil,
	}

	row := RenderRow(wi, 100, now, theme, false)

	// Should show model, holder, and time held (not lease countdown)
	if !hasSubstring(row, "haiku") {
		t.Errorf("missing model in row: %q", row)
	}
	if !hasSubstring(row, "@worker-example") {
		t.Errorf("missing holder in row: %q", row)
	}
	if !hasSubstring(row, "15m") {
		t.Errorf("missing time held (15m) in row: %q", row)
	}
	// Should NOT show 10m (the lease countdown)
	if hasSubstring(row, "10m") {
		t.Errorf("should not show lease countdown in row: %q", row)
	}
}

func TestRenderRowPendingReviewerWithTime(t *testing.T) {
	theme := newMockTheme()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	reviewUpdatedAt := now.Add(-8 * time.Minute).Format(time.RFC3339Nano)

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
				UpdatedAt:   reviewUpdatedAt,
				ReviewRound: 1,
			},
		},
		Merge: nil,
	}

	row := RenderRow(wi, 100, now, theme, false)
	lines := splitLines(row)

	// Check reviewer line contains time for pending reviewer
	if len(lines) >= 2 {
		reviewerLine := lines[1]
		if !hasSubstring(reviewerLine, "8m") {
			t.Errorf("pending reviewer should show elapsed time, got: %q", reviewerLine)
		}
	}
}

func ptrString(s string) *string {
	return &s
}
