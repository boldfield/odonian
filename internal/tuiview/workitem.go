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

// StatusType represents a row in the glyph table.
type StatusType int

const (
	StatusReadyClaimable StatusType = iota
	StatusReadyWaiting
	StatusBacklogNotPromoted
	StatusBacklogWaiting
	StatusInProgress
	StatusChecks
	StatusReview
	StatusApproved
	StatusBlocked
	StatusFailed
	StatusDone
	StatusAbandoned
)

// Status returns the status classification for a task.
// claimableIDs is a set of task IDs that are claimable (ready and not waiting on a dependency).
// Ready tasks not in the set are waiting on a dependency; held tasks ignore the set.
// Backlog tasks have no dependency information, so they always return StatusBacklogNotPromoted.
func Status(task *tuiclient.Task, claimableIDs map[string]bool) StatusType {
	switch task.State {
	case "backlog":
		return StatusBacklogNotPromoted
	case "ready":
		if task.Held {
			return StatusReadyClaimable
		}
		if claimableIDs[task.ID] {
			return StatusReadyClaimable
		}
		return StatusReadyWaiting
	case "in_progress":
		return StatusInProgress
	case "checks":
		return StatusChecks
	case "review":
		return StatusReview
	case "approved":
		return StatusApproved
	case "blocked":
		return StatusBlocked
	case "failed":
		return StatusFailed
	case "done":
		return StatusDone
	case "abandoned":
		return StatusAbandoned
	default:
		return StatusAbandoned
	}
}

// Glyph returns the glyph string for a status, with optional held prefix.
func Glyph(status StatusType, held bool) string {
	var glyph string
	switch status {
	case StatusReadyClaimable:
		glyph = "○"
	case StatusReadyWaiting:
		glyph = "⛓"
	case StatusBacklogNotPromoted:
		glyph = "·"
	case StatusBacklogWaiting:
		glyph = "⛓"
	case StatusInProgress:
		glyph = "●"
	case StatusChecks:
		glyph = "◷"
	case StatusReview:
		glyph = "◆"
	case StatusApproved:
		glyph = "✔"
	case StatusBlocked:
		glyph = "✖"
	case StatusFailed:
		glyph = "!"
	case StatusDone:
		glyph = "✓"
	case StatusAbandoned:
		glyph = "⊗"
	default:
		glyph = "?"
	}

	if held {
		return "⏸" + glyph
	}
	return glyph
}

// IsHeld returns whether a task is held.
func IsHeld(task *tuiclient.Task) bool {
	return task.Held
}

// NeedsYou returns whether a task needs the operator.
// A task needs you when: blocked, or approved with agent_merge=false.
// A failed task is terminal, so there is no action to take on it.
func NeedsYou(task *tuiclient.Task) bool {
	switch task.State {
	case "blocked":
		return true
	case "approved":
		return !task.AgentMerge
	default:
		return false
	}
}

// Badge computes a project's badge from its work items and returns the badge glyph and count.
// Callers pass work items already filtered by FilterWorkItems, so badges count only
// tasks in visible states. Returns ("", 0) for projects with only queued work (no needs-you, no awaiting-merge, no in-flight items).
// Badge priority (highest first):
// 1. Needs you (blocked): "⚠"
// 2. Awaiting merge (approved with agent_merge=false): "✔"
// 3. In flight (in_progress, checks, review, or approved with agent_merge=true): "●"
func Badge(workItems []*WorkItem) (string, int) {
	var needsYouCount, awaitingMergeCount, inFlightCount int

	for _, wi := range workItems {
		impl := wi.Implement

		if NeedsYou(impl) {
			if impl.State == "approved" {
				// approved with agent_merge=false counts as awaiting merge
				awaitingMergeCount++
			} else {
				// blocked
				needsYouCount++
			}
		} else if impl.State == "in_progress" || impl.State == "checks" || impl.State == "review" {
			inFlightCount++
		} else if impl.State == "approved" && impl.AgentMerge {
			inFlightCount++
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
