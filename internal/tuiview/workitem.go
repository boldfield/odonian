package tuiview

import (
	"github.com/boldfield/odonian/internal/tuiclient"
)

// WorkItem groups an implement task with its current-round review tasks and merge task.
type WorkItem struct {
	Implement *tuiclient.Task   // The implement task (never nil)
	Reviews   []*tuiclient.Task // Current-round review tasks (may be empty)
	Merge     *tuiclient.Task   // Merge task (may be nil)
}

// Result holds the grouped work items and any orphaned review/merge tasks.
type Result struct {
	WorkItems []*WorkItem
	Orphans   []*tuiclient.Task
}

// GroupWorkItems takes a flat list of tasks and groups them into work items,
// keeping reviews and merges attached to their target implement task.
// A review task belongs to the current round when its ReviewRound equals
// the implement task's ReviewRound; earlier rounds are dropped.
// Review or merge tasks whose target is not in the input are returned in Orphans.
// Output order is deterministic: the input order of implement tasks is preserved.
func GroupWorkItems(tasks []*tuiclient.Task) *Result {
	implementTasks := make([]*tuiclient.Task, 0)
	reviewTasks := make([]*tuiclient.Task, 0)
	mergeTasks := make([]*tuiclient.Task, 0)
	implementByID := make(map[string]*tuiclient.Task)

	// First pass: separate tasks by kind and build a map of implement tasks by ID
	for _, task := range tasks {
		switch task.Kind {
		case "implement":
			implementTasks = append(implementTasks, task)
			implementByID[task.ID] = task
		case "review":
			reviewTasks = append(reviewTasks, task)
		case "merge":
			mergeTasks = append(mergeTasks, task)
		}
	}

	workItems := make([]*WorkItem, len(implementTasks))
	orphans := make([]*tuiclient.Task, 0)

	// Second pass: build work items for each implement task
	for i, impl := range implementTasks {
		wi := &WorkItem{
			Implement: impl,
			Reviews:   make([]*tuiclient.Task, 0),
			Merge:     nil,
		}

		// Attach current-round reviews
		for _, review := range reviewTasks {
			if review.TargetTaskID != nil && *review.TargetTaskID == impl.ID &&
				review.ReviewRound == impl.ReviewRound {
				wi.Reviews = append(wi.Reviews, review)
			}
		}

		// Attach merge task
		for _, merge := range mergeTasks {
			if merge.TargetTaskID != nil && *merge.TargetTaskID == impl.ID {
				wi.Merge = merge
				break
			}
		}

		workItems[i] = wi
	}

	// Third pass: collect orphaned review and merge tasks
	for _, review := range reviewTasks {
		if review.TargetTaskID == nil || implementByID[*review.TargetTaskID] == nil {
			orphans = append(orphans, review)
		}
	}

	for _, merge := range mergeTasks {
		if merge.TargetTaskID == nil || implementByID[*merge.TargetTaskID] == nil {
			orphans = append(orphans, merge)
		}
	}

	return &Result{
		WorkItems: workItems,
		Orphans:   orphans,
	}
}
