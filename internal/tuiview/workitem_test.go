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
