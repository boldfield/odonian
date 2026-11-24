package tuiview

import (
	"testing"

	"github.com/boldfield/odonian/internal/tuiclient"
)

func TestGroupWorkItems(t *testing.T) {
	tests := []struct {
		name            string
		tasks           []*tuiclient.Task
		wantWorkItemLen int
		wantOrphansLen  int
		check           func(t *testing.T, result *Result)
	}{
		{
			name: "implement task with no children",
			tasks: []*tuiclient.Task{
				{
					ID:   "impl-1",
					Kind: "implement",
				},
			},
			wantWorkItemLen: 1,
			wantOrphansLen:  0,
			check: func(t *testing.T, result *Result) {
				wi := result.WorkItems[0]
				if wi.Implement.ID != "impl-1" {
					t.Errorf("Implement ID: got %s, want impl-1", wi.Implement.ID)
				}
				if len(wi.Reviews) != 0 {
					t.Errorf("Reviews length: got %d, want 0", len(wi.Reviews))
				}
				if wi.Merge != nil {
					t.Errorf("Merge: got %v, want nil", wi.Merge)
				}
			},
		},
		{
			name: "implement with two current-round reviews",
			tasks: []*tuiclient.Task{
				{
					ID:          "impl-2",
					Kind:        "implement",
					ReviewRound: 1,
				},
				{
					ID:           "review-1",
					Kind:         "review",
					TargetTaskID: strPtr("impl-2"),
					ReviewRound:  1,
				},
				{
					ID:           "review-2",
					Kind:         "review",
					TargetTaskID: strPtr("impl-2"),
					ReviewRound:  1,
				},
			},
			wantWorkItemLen: 1,
			wantOrphansLen:  0,
			check: func(t *testing.T, result *Result) {
				wi := result.WorkItems[0]
				if wi.Implement.ID != "impl-2" {
					t.Errorf("Implement ID: got %s, want impl-2", wi.Implement.ID)
				}
				if len(wi.Reviews) != 2 {
					t.Errorf("Reviews length: got %d, want 2", len(wi.Reviews))
				}
				if wi.Merge != nil {
					t.Errorf("Merge: got %v, want nil", wi.Merge)
				}
				// Check review IDs
				if wi.Reviews[0].ID != "review-1" {
					t.Errorf("Review 0 ID: got %s, want review-1", wi.Reviews[0].ID)
				}
				if wi.Reviews[1].ID != "review-2" {
					t.Errorf("Review 1 ID: got %s, want review-2", wi.Reviews[1].ID)
				}
			},
		},
		{
			name: "implement with reviews from two rounds (only current round attached)",
			tasks: []*tuiclient.Task{
				{
					ID:          "impl-3",
					Kind:        "implement",
					ReviewRound: 2,
				},
				{
					ID:           "review-round1",
					Kind:         "review",
					TargetTaskID: strPtr("impl-3"),
					ReviewRound:  1,
				},
				{
					ID:           "review-round2",
					Kind:         "review",
					TargetTaskID: strPtr("impl-3"),
					ReviewRound:  2,
				},
			},
			wantWorkItemLen: 1,
			wantOrphansLen:  0,
			check: func(t *testing.T, result *Result) {
				wi := result.WorkItems[0]
				if wi.Implement.ID != "impl-3" {
					t.Errorf("Implement ID: got %s, want impl-3", wi.Implement.ID)
				}
				if len(wi.Reviews) != 1 {
					t.Errorf("Reviews length: got %d, want 1", len(wi.Reviews))
				}
				if wi.Reviews[0].ID != "review-round2" {
					t.Errorf("Review ID: got %s, want review-round2", wi.Reviews[0].ID)
				}
			},
		},
		{
			name: "implement with merge task",
			tasks: []*tuiclient.Task{
				{
					ID:   "impl-4",
					Kind: "implement",
				},
				{
					ID:           "merge-1",
					Kind:         "merge",
					TargetTaskID: strPtr("impl-4"),
				},
			},
			wantWorkItemLen: 1,
			wantOrphansLen:  0,
			check: func(t *testing.T, result *Result) {
				wi := result.WorkItems[0]
				if wi.Implement.ID != "impl-4" {
					t.Errorf("Implement ID: got %s, want impl-4", wi.Implement.ID)
				}
				if wi.Merge == nil {
					t.Errorf("Merge: got nil, want merge-1")
				} else if wi.Merge.ID != "merge-1" {
					t.Errorf("Merge ID: got %s, want merge-1", wi.Merge.ID)
				}
			},
		},
		{
			name: "orphan review with no target",
			tasks: []*tuiclient.Task{
				{
					ID:   "impl-5",
					Kind: "implement",
				},
				{
					ID:   "orphan-review",
					Kind: "review",
					// TargetTaskID is nil
				},
			},
			wantWorkItemLen: 1,
			wantOrphansLen:  1,
			check: func(t *testing.T, result *Result) {
				if result.Orphans[0].ID != "orphan-review" {
					t.Errorf("Orphan ID: got %s, want orphan-review", result.Orphans[0].ID)
				}
			},
		},
		{
			name: "orphan review with non-existent target",
			tasks: []*tuiclient.Task{
				{
					ID:   "impl-6",
					Kind: "implement",
				},
				{
					ID:           "orphan-review-2",
					Kind:         "review",
					TargetTaskID: strPtr("non-existent"),
				},
			},
			wantWorkItemLen: 1,
			wantOrphansLen:  1,
			check: func(t *testing.T, result *Result) {
				if result.Orphans[0].ID != "orphan-review-2" {
					t.Errorf("Orphan ID: got %s, want orphan-review-2", result.Orphans[0].ID)
				}
			},
		},
		{
			name: "complex scenario with multiple work items and orphans",
			tasks: []*tuiclient.Task{
				// First implement task with reviews and merge
				{
					ID:          "impl-a",
					Kind:        "implement",
					ReviewRound: 1,
				},
				{
					ID:           "review-a1",
					Kind:         "review",
					TargetTaskID: strPtr("impl-a"),
					ReviewRound:  1,
				},
				{
					ID:           "review-a2",
					Kind:         "review",
					TargetTaskID: strPtr("impl-a"),
					ReviewRound:  1,
				},
				{
					ID:           "merge-a",
					Kind:         "merge",
					TargetTaskID: strPtr("impl-a"),
				},
				// Second implement task with no children
				{
					ID:          "impl-b",
					Kind:        "implement",
					ReviewRound: 1,
				},
				// Orphan review
				{
					ID:   "orphan-review-x",
					Kind: "review",
				},
				// Orphan merge
				{
					ID:           "orphan-merge-y",
					Kind:         "merge",
					TargetTaskID: strPtr("non-existent-impl"),
				},
			},
			wantWorkItemLen: 2,
			wantOrphansLen:  2,
			check: func(t *testing.T, result *Result) {
				// First work item
				wi0 := result.WorkItems[0]
				if wi0.Implement.ID != "impl-a" {
					t.Errorf("WI[0] Implement ID: got %s, want impl-a", wi0.Implement.ID)
				}
				if len(wi0.Reviews) != 2 {
					t.Errorf("WI[0] Reviews length: got %d, want 2", len(wi0.Reviews))
				}
				if wi0.Merge == nil || wi0.Merge.ID != "merge-a" {
					t.Errorf("WI[0] Merge ID: got %v, want merge-a", wi0.Merge)
				}

				// Second work item
				wi1 := result.WorkItems[1]
				if wi1.Implement.ID != "impl-b" {
					t.Errorf("WI[1] Implement ID: got %s, want impl-b", wi1.Implement.ID)
				}
				if len(wi1.Reviews) != 0 {
					t.Errorf("WI[1] Reviews length: got %d, want 0", len(wi1.Reviews))
				}
				if wi1.Merge != nil {
					t.Errorf("WI[1] Merge: got %v, want nil", wi1.Merge)
				}

				// Orphans
				if len(result.Orphans) != 2 {
					t.Errorf("Orphans length: got %d, want 2", len(result.Orphans))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GroupWorkItems(tt.tasks)

			if len(result.WorkItems) != tt.wantWorkItemLen {
				t.Errorf("WorkItems length: got %d, want %d", len(result.WorkItems), tt.wantWorkItemLen)
			}

			if len(result.Orphans) != tt.wantOrphansLen {
				t.Errorf("Orphans length: got %d, want %d", len(result.Orphans), tt.wantOrphansLen)
			}

			if tt.check != nil {
				tt.check(t, result)
			}
		})
	}
}

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name         string
		task         *tuiclient.Task
		claimableIDs map[string]bool
		want         StatusType
	}{
		// Backlog tests
		{
			name:         "backlog not promoted (in claimableIDs)",
			task:         &tuiclient.Task{ID: "t1", State: "backlog", Held: false},
			claimableIDs: map[string]bool{"t1": true},
			want:         StatusBacklogNotPromoted,
		},
		{
			name:         "backlog waiting on dependency",
			task:         &tuiclient.Task{ID: "t2", State: "backlog", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusBacklogWaiting,
		},

		// Ready tests
		{
			name:         "ready claimable",
			task:         &tuiclient.Task{ID: "t3", State: "ready", Held: false},
			claimableIDs: map[string]bool{"t3": true},
			want:         StatusReadyClaimable,
		},
		{
			name:         "ready waiting on dependency",
			task:         &tuiclient.Task{ID: "t4", State: "ready", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusReadyWaiting,
		},

		// In progress
		{
			name:         "in_progress",
			task:         &tuiclient.Task{ID: "t5", State: "in_progress", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusInProgress,
		},

		// In checks
		{
			name:         "checks",
			task:         &tuiclient.Task{ID: "t6", State: "checks", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusChecks,
		},

		// In review
		{
			name:         "review",
			task:         &tuiclient.Task{ID: "t7", State: "review", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusReview,
		},

		// Approved
		{
			name:         "approved",
			task:         &tuiclient.Task{ID: "t8", State: "approved", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusApproved,
		},

		// Blocked
		{
			name:         "blocked",
			task:         &tuiclient.Task{ID: "t9", State: "blocked", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusBlocked,
		},

		// Failed
		{
			name:         "failed",
			task:         &tuiclient.Task{ID: "t10", State: "failed", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusFailed,
		},

		// Done and abandoned (should default to StatusBacklogNotPromoted)
		{
			name:         "done",
			task:         &tuiclient.Task{ID: "t16", State: "done", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusBacklogNotPromoted,
		},
		{
			name:         "abandoned",
			task:         &tuiclient.Task{ID: "t17", State: "abandoned", Held: false},
			claimableIDs: map[string]bool{},
			want:         StatusBacklogNotPromoted,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Status(tt.task, tt.claimableIDs)
			if got != tt.want {
				t.Errorf("Status() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGlyph(t *testing.T) {
	tests := []struct {
		name   string
		status StatusType
		held   bool
		want   string
	}{
		// Glyph table: all rows without held
		{name: "ready claimable", status: StatusReadyClaimable, held: false, want: "○"},
		{name: "ready waiting", status: StatusReadyWaiting, held: false, want: "⛓"},
		{name: "backlog not promoted", status: StatusBacklogNotPromoted, held: false, want: "·"},
		{name: "backlog waiting", status: StatusBacklogWaiting, held: false, want: "⛓"},
		{name: "in_progress", status: StatusInProgress, held: false, want: "●"},
		{name: "checks", status: StatusChecks, held: false, want: "◷"},
		{name: "review", status: StatusReview, held: false, want: "◆"},
		{name: "approved", status: StatusApproved, held: false, want: "✔"},
		{name: "blocked", status: StatusBlocked, held: false, want: "✖"},
		{name: "failed", status: StatusFailed, held: false, want: "!"},

		// Held prefix on each state
		{name: "held ready claimable", status: StatusReadyClaimable, held: true, want: "⏸○"},
		{name: "held ready waiting", status: StatusReadyWaiting, held: true, want: "⏸⛓"},
		{name: "held backlog not promoted", status: StatusBacklogNotPromoted, held: true, want: "⏸·"},
		{name: "held backlog waiting", status: StatusBacklogWaiting, held: true, want: "⏸⛓"},
		{name: "held in_progress", status: StatusInProgress, held: true, want: "⏸●"},
		{name: "held checks", status: StatusChecks, held: true, want: "⏸◷"},
		{name: "held review", status: StatusReview, held: true, want: "⏸◆"},
		{name: "held approved", status: StatusApproved, held: true, want: "⏸✔"},
		{name: "held blocked", status: StatusBlocked, held: true, want: "⏸✖"},
		{name: "held failed", status: StatusFailed, held: true, want: "⏸!"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Glyph(tt.status, tt.held)
			if got != tt.want {
				t.Errorf("Glyph() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestIsHeld(t *testing.T) {
	tests := []struct {
		name string
		task *tuiclient.Task
		want bool
	}{
		{
			name: "held task",
			task: &tuiclient.Task{Held: true},
			want: true,
		},
		{
			name: "not held task",
			task: &tuiclient.Task{Held: false},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsHeld(tt.task)
			if got != tt.want {
				t.Errorf("IsHeld() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNeedsYou(t *testing.T) {
	tests := []struct {
		name string
		task *tuiclient.Task
		want bool
	}{
		// Blocked always needs you
		{
			name: "blocked",
			task: &tuiclient.Task{State: "blocked"},
			want: true,
		},

		// Failed always needs you
		{
			name: "failed",
			task: &tuiclient.Task{State: "failed"},
			want: true,
		},

		// Approved with agent_merge=false needs you
		{
			name: "approved with agent_merge=false",
			task: &tuiclient.Task{State: "approved", AgentMerge: false},
			want: true,
		},

		// Approved with agent_merge=true does not need you (in flight)
		{
			name: "approved with agent_merge=true",
			task: &tuiclient.Task{State: "approved", AgentMerge: true},
			want: false,
		},

		// Other states don't need you
		{
			name: "backlog",
			task: &tuiclient.Task{State: "backlog"},
			want: false,
		},
		{
			name: "ready",
			task: &tuiclient.Task{State: "ready"},
			want: false,
		},
		{
			name: "in_progress",
			task: &tuiclient.Task{State: "in_progress"},
			want: false,
		},
		{
			name: "checks",
			task: &tuiclient.Task{State: "checks"},
			want: false,
		},
		{
			name: "review",
			task: &tuiclient.Task{State: "review"},
			want: false,
		},
		{
			name: "done",
			task: &tuiclient.Task{State: "done"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NeedsYou(tt.task)
			if got != tt.want {
				t.Errorf("NeedsYou() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBadge(t *testing.T) {
	tests := []struct {
		name      string
		workItems []*WorkItem
		wantGlyph string
		wantCount int
	}{
		// No work items
		{
			name:      "empty",
			workItems: []*WorkItem{},
			wantGlyph: "",
			wantCount: 0,
		},

		// Only queued work (backlog, ready)
		{
			name: "only queued work",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "backlog"}},
				{Implement: &tuiclient.Task{State: "ready"}},
			},
			wantGlyph: "",
			wantCount: 0,
		},

		// Blocked (needs you)
		{
			name: "single blocked",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "blocked"}},
			},
			wantGlyph: "⚠",
			wantCount: 1,
		},

		// Failed (needs you)
		{
			name: "single failed",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "failed"}},
			},
			wantGlyph: "⚠",
			wantCount: 1,
		},

		// Mixed blocked and failed (needs you)
		{
			name: "blocked and failed",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "blocked"}},
				{Implement: &tuiclient.Task{State: "failed"}},
			},
			wantGlyph: "⚠",
			wantCount: 2,
		},

		// Approved with agent_merge=false (awaiting merge)
		{
			name: "single awaiting merge",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: false}},
			},
			wantGlyph: "✔",
			wantCount: 1,
		},

		// Approved with agent_merge=true (in flight)
		{
			name: "single approved in flight",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
			},
			wantGlyph: "●",
			wantCount: 1,
		},

		// In progress only (in flight)
		{
			name: "in_progress only",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "in_progress"}},
			},
			wantGlyph: "●",
			wantCount: 1,
		},

		// Checks only (in flight)
		{
			name: "checks only",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "checks"}},
			},
			wantGlyph: "●",
			wantCount: 1,
		},

		// Review only (in flight)
		{
			name: "review only",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "review"}},
			},
			wantGlyph: "●",
			wantCount: 1,
		},

		// Needs you takes priority over awaiting merge
		{
			name: "blocked and awaiting merge",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "blocked"}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: false}},
			},
			wantGlyph: "⚠",
			wantCount: 1,
		},

		// Needs you takes priority over in flight
		{
			name: "failed and in flight",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "failed"}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
			},
			wantGlyph: "⚠",
			wantCount: 1,
		},

		// Awaiting merge takes priority over in flight
		{
			name: "awaiting merge and in flight",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: false}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
			},
			wantGlyph: "✔",
			wantCount: 1,
		},

		// All priorities mixed
		{
			name: "all priorities",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "blocked"}},
				{Implement: &tuiclient.Task{State: "blocked"}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: false}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
				{Implement: &tuiclient.Task{State: "in_progress"}},
			},
			wantGlyph: "⚠",
			wantCount: 2,
		},

		// Multiple awaiting merge (no needs you)
		{
			name: "multiple awaiting merge",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: false}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: false}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
			},
			wantGlyph: "✔",
			wantCount: 2,
		},

		// Multiple in flight (no higher priority)
		{
			name: "multiple in flight",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
			},
			wantGlyph: "●",
			wantCount: 2,
		},

		// Mixed in-flight states
		{
			name: "mixed in-flight states",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "in_progress"}},
				{Implement: &tuiclient.Task{State: "checks"}},
				{Implement: &tuiclient.Task{State: "review"}},
				{Implement: &tuiclient.Task{State: "approved", AgentMerge: true}},
			},
			wantGlyph: "●",
			wantCount: 4,
		},

		// In flight takes priority over queued
		{
			name: "in-flight and queued",
			workItems: []*WorkItem{
				{Implement: &tuiclient.Task{State: "in_progress"}},
				{Implement: &tuiclient.Task{State: "backlog"}},
				{Implement: &tuiclient.Task{State: "ready"}},
			},
			wantGlyph: "●",
			wantCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotGlyph, gotCount := Badge(tt.workItems)
			if gotGlyph != tt.wantGlyph || gotCount != tt.wantCount {
				t.Errorf("Badge() = (%s, %d), want (%s, %d)", gotGlyph, gotCount, tt.wantGlyph, tt.wantCount)
			}
		})
	}
}
