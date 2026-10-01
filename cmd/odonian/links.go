package main

import (
	"fmt"
	"strings"

	"github.com/boldfield/odonian/internal/tuiclient"
)

// currentRoundLinks returns the submission under review in the task's current round. The server
// decides which links those are (GET /tasks/{id} current_round_links), so the rule lives in one
// place.
func currentRoundLinks(task tuiclient.TaskDetail) []tuiclient.TaskLink {
	return task.CurrentRoundLinks
}

// currentRoundIsNoOp reports whether the task's current review round was a no-op submission.
func currentRoundIsNoOp(task tuiclient.TaskDetail) bool {
	hasNoOp, hasCommit := false, false
	for _, link := range currentRoundLinks(task) {
		switch link.Kind {
		case "no_op":
			hasNoOp = true
		case "commit":
			hasCommit = true
		}
	}
	return hasNoOp && !hasCommit
}

// reviewedCommit returns the commit under review in the task's current round. Earlier rounds'
// commit links stay on the task, so "the first commit link" can be a stale round's.
func reviewedCommit(task tuiclient.TaskDetail) (string, error) {
	var commits []string
	for _, link := range currentRoundLinks(task) {
		if link.Kind == "commit" {
			commits = append(commits, link.Value)
		}
	}
	switch {
	case len(commits) == 1:
		return commits[0], nil
	case len(commits) > 1:
		return "", fmt.Errorf("task has %d candidate commits for review round %d (%s), recorded before links carried their review round, so which one round %d reviews is unknown; re-submit the task to record it",
			len(commits), task.ReviewRound, strings.Join(commits, ", "), task.ReviewRound)
	case len(currentRoundLinks(task)) > 0:
		return "", fmt.Errorf("review round %d submitted no commit (a no-op submission has nothing to diff)", task.ReviewRound)
	default:
		return "", fmt.Errorf("task has no commit link for review round %d", task.ReviewRound)
	}
}

// linkRoundLabel says which review round a link belongs to, for `show`.
func linkRoundLabel(task tuiclient.TaskDetail, link tuiclient.TaskLink) string {
	if link.TombstonedAt != nil {
		return " (tombstoned)"
	}
	if link.ReviewRound == nil {
		return ""
	}
	if *link.ReviewRound == task.ReviewRound {
		return fmt.Sprintf(" (round %d, current)", *link.ReviewRound)
	}
	return fmt.Sprintf(" (round %d, superseded)", *link.ReviewRound)
}
