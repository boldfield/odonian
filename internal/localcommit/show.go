package localcommit

import (
	"bytes"
	"os/exec"
)

func ShowCommit(repoDir, sha string) (string, error) {
	cmd := exec.Command("git", "-C", repoDir, "show", sha)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// DiffBase shows everything task commit sha changes relative to where the task started: its
// merge-base with the tip the task branched from (wi/<slug> if it exists, else origin/main), the
// start point CommitTask keeps a task's single commit on. A task is normally one commit there, so
// this is that commit's change; one submitted under older code can span several commits, and all
// of them are shown, since all of them would land. Diffing against origin/main instead would, on
// a branch shared across tasks, also show earlier tasks' approved work. Once the task has landed
// (sha is on the tip), it falls back to the commit against its parent.
func DiffBase(repoDir, slug, sha string) (string, error) {
	tip, _ := ResolveTip(repoDir, slug)
	start, err := git(repoDir, "merge-base", tip, sha)
	if err != nil || start == sha {
		start = sha + "^"
	}
	cmd := exec.Command("git", "-C", repoDir, "diff", start, sha)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return out.String(), nil
}
