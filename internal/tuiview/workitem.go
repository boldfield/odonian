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

// Status returns the glyph for a task's status.
// claimableIDs is a set of task IDs that are claimable (ready and not waiting on a dependency).
// A ready or backlog task not in the set is waiting on a dependency.
func Status(task *tuiclient.Task, claimableIDs map[string]bool) string {
	if task.Held {
		return "⏸"
	}

	switch task.State {
	case "backlog":
		if claimableIDs[task.ID] {
			return "○"
		}
		return "·"
	case "ready":
		if claimableIDs[task.ID] {
			return "○"
		}
		return "⛓"
	case "in_progress":
		return "●"
	case "checks":
		return "◷"
	case "review":
		return "◆"
	case "approved":
		return "✔"
	case "blocked":
		return "✖"
	case "failed":
		return "!"
	default:
		return "·"
	}
}

// IsHeld returns whether a task is held.
func IsHeld(task *tuiclient.Task) bool {
	return task.Held
}

// NeedsYou returns whether a task needs the operator.
// A task needs you when: blocked, failed, or approved with agent_merge=false.
func NeedsYou(task *tuiclient.Task) bool {
	switch task.State {
	case "blocked", "failed":
		return true
	case "approved":
		return !task.AgentMerge
	default:
		return false
	}
}

// Badge computes a project's badge from its work items and returns the badge glyph and count.
// Returns ("", 0) for projects with only queued work (no needs-you, no awaiting-merge, no in-flight items).
// Badge priority (highest first):
// 1. Needs you (blocked or failed): "⚠"
// 2. Awaiting merge (approved with agent_merge=false): "✔"
// 3. In flight (approved with agent_merge=true): "●"
func Badge(workItems []*WorkItem) (string, int) {
	var needsYouCount, awaitingMergeCount, inFlightCount int

	for _, wi := range workItems {
		impl := wi.Implement

		if impl.State == "blocked" || impl.State == "failed" {
			needsYouCount++
		} else if impl.State == "approved" {
			if impl.AgentMerge {
				inFlightCount++
			} else {
				awaitingMergeCount++
			}
		}
	}

	// Highest priority first
	if needsYouCount > 0 {
		return "⚠", needsYouCount
	}
	if awaitingMergeCount > 0 {
		return "✔", awaitingMergeCount
	}
	if inFlightCount > 0 {
		return "●", inFlightCount
	}

	return "", 0
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
