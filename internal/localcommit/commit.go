package localcommit

import (
	"bytes"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func CommitAll(wtPath, message string) (sha string, err error) {
	// Stage all changes
	cmd := exec.Command("git", "-C", wtPath, "add", "-A")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git add failed: %w", err)
	}

	// Try to commit
	cmd = exec.Command("git", "-C", wtPath, "commit", "-m", message)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Check if the error is "nothing to commit"
		if strings.Contains(stderr.String(), "nothing to commit") {
			return "", fmt.Errorf("nothing to commit")
		}
		return "", fmt.Errorf("git commit failed: %w", err)
	}

	// Get the new HEAD SHA
	cmd = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git rev-parse failed: %w", err)
	}

	sha = strings.TrimSpace(stdout.String())
	return sha, nil
}

func AmendAll(wtPath, message string) (sha string, err error) {
	// Stage all changes
	cmd := exec.Command("git", "-C", wtPath, "add", "-A")
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git add failed: %w", err)
	}

	// Amend the last commit (allowed to have no new changes)
	cmd = exec.Command("git", "-C", wtPath, "commit", "--amend", "-m", message)
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git commit --amend failed: %w", err)
	}

	// Get the new HEAD SHA
	cmd = exec.Command("git", "-C", wtPath, "rev-parse", "HEAD")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git rev-parse failed: %w", err)
	}

	sha = strings.TrimSpace(stdout.String())
	return sha, nil
}

// CommitTask records the task's work in its worktree as exactly one commit on top of the point
// the task started from (the merge-base of tip and HEAD), which is what DiffBase and the reviewers
// rely on: no commits yet → commit; one (a prior round) → amend it; more than one (someone
// committed by hand) → squash them all into one, reporting how many were folded in.
func CommitTask(wtPath, tip, message string) (sha string, squashedCommits int, err error) {
	startPoint, err := git(wtPath, "merge-base", tip, "HEAD")
	if err != nil {
		// No common history with the tip (should not happen for a wt-ensure worktree). A plain
		// commit here would add a second commit on a rework, breaking the one-commit task that
		// DiffBase and the reviewers rely on, so refuse instead.
		return "", 0, fmt.Errorf("cannot find where the task started (no common history between HEAD and %s), so it cannot be kept to one commit: %w", tip, err)
	}
	countOutput, err := git(wtPath, "rev-list", "--count", startPoint+"..HEAD")
	if err != nil {
		return "", 0, fmt.Errorf("git rev-list failed: %w", err)
	}

	switch countOutput {
	case "0":
		sha, err = CommitAll(wtPath, message)
		return sha, 0, err
	case "1":
		sha, err = AmendAll(wtPath, message)
		return sha, 0, err
	}
	commitCount, err := strconv.Atoi(countOutput)
	if err != nil {
		return "", 0, fmt.Errorf("unexpected commit count %q from git rev-list: %w", countOutput, err)
	}
	if _, err := git(wtPath, "reset", "--soft", startPoint); err != nil {
		return "", 0, fmt.Errorf("failed to squash %d commits: %w", commitCount, err)
	}
	sha, err = CommitAll(wtPath, message)
	return sha, commitCount, err
}
