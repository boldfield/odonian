package localcommit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// taskWorktree is a wip/<iid> worktree started from base, as wt-ensure would create it.
func taskWorktree(t *testing.T, repo sharedBranchRepo, iid, base string) string {
	t.Helper()
	worktreeDir := filepath.Join(t.TempDir(), iid)
	runCmd(t, repo.dir, "git", "worktree", "add", "-b", WIPBranch(iid), worktreeDir, base)
	return worktreeDir
}

func writeWorktreeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitsSince(t *testing.T, dir, start string) int {
	t.Helper()
	return len(strings.Fields(runCmd(t, dir, "git", "rev-list", start+"..HEAD")))
}

func TestCommitTask_FirstRoundThenRework(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	start := getBranch(t, repo.dir, "origin/main")
	worktreeDir := taskWorktree(t, repo, "task-1", "origin/main")

	writeWorktreeFile(t, worktreeDir, "one.txt", "first round\n")
	if _, squashed, err := CommitTask(worktreeDir, "origin/main", "task-1"); err != nil || squashed != 0 {
		t.Fatalf("first round: squashed=%d err=%v", squashed, err)
	}
	writeWorktreeFile(t, worktreeDir, "one.txt", "rework\n")
	sha, squashed, err := CommitTask(worktreeDir, "origin/main", "task-1")
	if err != nil || squashed != 0 {
		t.Fatalf("rework: squashed=%d err=%v", squashed, err)
	}

	if count := commitsSince(t, worktreeDir, start); count != 1 {
		t.Errorf("task should be one commit after rework, got %d", count)
	}
	if parent := runCmd(t, worktreeDir, "git", "rev-parse", sha+"^"); parent != start {
		t.Errorf("task commit's parent = %s, want the start point %s", parent, start)
	}
}

func TestCommitTask_SquashesHandMadeCommits(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	start := getBranch(t, repo.dir, "origin/main")
	worktreeDir := taskWorktree(t, repo, "task-1", "origin/main")

	// A worker broke the rules and committed twice by hand, then left more edits.
	writeWorktreeFile(t, worktreeDir, "one.txt", "a\n")
	runCmd(t, worktreeDir, "git", "add", "-A")
	runCmd(t, worktreeDir, "git", "commit", "-m", "hand commit 1")
	writeWorktreeFile(t, worktreeDir, "two.txt", "b\n")
	runCmd(t, worktreeDir, "git", "add", "-A")
	runCmd(t, worktreeDir, "git", "commit", "-m", "hand commit 2")
	writeWorktreeFile(t, worktreeDir, "three.txt", "c\n")

	sha, squashed, err := CommitTask(worktreeDir, "origin/main", "task-1")
	if err != nil {
		t.Fatalf("CommitTask: %v", err)
	}
	if squashed != 2 {
		t.Errorf("squashed = %d, want 2", squashed)
	}
	if count := commitsSince(t, worktreeDir, start); count != 1 {
		t.Errorf("task should be one commit after squashing, got %d", count)
	}
	// DiffBase (commit vs parent) must now show every change the task made.
	diff, err := DiffBase(repo.dir, "task-1", sha)
	if err != nil {
		t.Fatalf("DiffBase: %v", err)
	}
	for _, file := range []string{"one.txt", "two.txt", "three.txt"} {
		if !strings.Contains(diff, file) {
			t.Errorf("reviewers' diff is missing %s", file)
		}
	}
}

func TestCommitTask_SquashUsesTheTaskStartNotTheMovedBranch(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	start := getBranch(t, repo.dir, "origin/main")
	worktreeDir := taskWorktree(t, repo, "task-2", "origin/main")
	writeWorktreeFile(t, worktreeDir, "two.txt", "a\n")
	runCmd(t, worktreeDir, "git", "add", "-A")
	runCmd(t, worktreeDir, "git", "commit", "-m", "hand commit 1")
	writeWorktreeFile(t, worktreeDir, "two.txt", "b\n")
	runCmd(t, worktreeDir, "git", "add", "-A")
	runCmd(t, worktreeDir, "git", "commit", "-m", "hand commit 2")

	// Meanwhile another task lands on the shared branch, which is now the resolved tip.
	repo.taskCommit(t, "task-1", "origin/main", "one.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1: %v", err)
	}

	sha, _, err := CommitTask(worktreeDir, "wi/shared", "task-2")
	if err != nil {
		t.Fatalf("CommitTask: %v", err)
	}
	if parent := runCmd(t, worktreeDir, "git", "rev-parse", sha+"^"); parent != start {
		t.Errorf("squash landed on %s; it must stay on the task's own start %s, or it would silently revert task-1", parent, start)
	}
}

func TestCommitTask_RefusesWithoutAStartPoint(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	worktreeDir := taskWorktree(t, repo, "task-1", "origin/main")
	writeWorktreeFile(t, worktreeDir, "one.txt", "a\n")

	// A tip with no history in common with the worktree.
	unrelated := runCmd(t, repo.dir, "git", "commit-tree", "-m", "unrelated", runCmd(t, repo.dir, "git", "hash-object", "-t", "tree", "-w", "/dev/null"))
	runCmd(t, repo.dir, "git", "update-ref", "refs/heads/unrelated", unrelated)

	if _, _, err := CommitTask(worktreeDir, "unrelated", "task-1"); err == nil || !strings.Contains(err.Error(), "cannot find where the task started") {
		t.Fatalf("expected CommitTask to refuse, got %v", err)
	}
}

func TestDiffBase_ShowsEveryCommitOfAMultiCommitTask(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	worktreeDir := taskWorktree(t, repo, "task-1", "origin/main")
	// Submitted under older code: two commits, not squashed.
	writeWorktreeFile(t, worktreeDir, "one.txt", "a\n")
	runCmd(t, worktreeDir, "git", "add", "-A")
	runCmd(t, worktreeDir, "git", "commit", "-m", "first")
	writeWorktreeFile(t, worktreeDir, "two.txt", "b\n")
	runCmd(t, worktreeDir, "git", "add", "-A")
	runCmd(t, worktreeDir, "git", "commit", "-m", "second")
	sha := runCmd(t, worktreeDir, "git", "rev-parse", "HEAD")

	diff, err := DiffBase(repo.dir, "task-1", sha)
	if err != nil {
		t.Fatalf("DiffBase: %v", err)
	}
	if !strings.Contains(diff, "one.txt") || !strings.Contains(diff, "two.txt") {
		t.Errorf("the reviewers' diff must include every commit that would land, got:\n%s", diff)
	}
}
