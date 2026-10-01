package localcommit

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostGate lets tests run the merge gate without a sandbox.
var hostGate = IntegrateOptions{Progress: io.Discard, AllowHostGate: true}

func TestBranchSlug(t *testing.T) {
	if got := BranchSlug("event-platform", "E2: Add bootstrap"); got != "event-platform" {
		t.Errorf("explicit branch: got %q, want event-platform", got)
	}
	if got := BranchSlug("", "E2: Add bootstrap"); got != "e2-add-bootstrap" {
		t.Errorf("no branch: got %q, want the title slug e2-add-bootstrap", got)
	}
}

const passingMakefile = ".PHONY: check test\ncheck:\n\t@true\ntest:\n\t@true\n"

// clashingMakefile passes when either task's file is present alone, and fails when both are:
// two changes that are each fine but break each other when combined.
const clashingMakefile = ".PHONY: check test\ncheck:\n\t@true\ntest:\n\t@if [ -f one.txt ] && [ -f two.txt ]; then echo 'one and two clash'; exit 1; fi\n"

// sharedBranchRepo is a repo whose main has one commit carrying a Makefile, plus a helper to
// commit a task's work onto a wip/<iid> branch started from a given base.
type sharedBranchRepo struct {
	dir string
}

func newSharedBranchRepo(t *testing.T, makefile string) sharedBranchRepo {
	t.Helper()
	repoDir := t.TempDir()
	runCmd(t, repoDir, "git", "init", "-b", "main")
	runCmd(t, repoDir, "git", "config", "user.email", "test@example.com")
	runCmd(t, repoDir, "git", "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repoDir, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, repoDir, "git", "add", "-A")
	runCmd(t, repoDir, "git", "commit", "-m", "initial")
	runCmd(t, repoDir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")
	t.Setenv("ODONIAN_WORKTREE_HOME", t.TempDir())
	return sharedBranchRepo{dir: repoDir}
}

// taskCommit creates wip/<iid> from base with one commit writing file (content = iid), as a
// task would.
func (repo sharedBranchRepo) taskCommit(t *testing.T, iid, base, file string) string {
	t.Helper()
	worktreeDir := filepath.Join(t.TempDir(), iid)
	runCmd(t, repo.dir, "git", "worktree", "add", "-b", WIPBranch(iid), worktreeDir, base)
	if err := os.WriteFile(filepath.Join(worktreeDir, file), []byte(iid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCmd(t, worktreeDir, "git", "add", "-A")
	runCmd(t, worktreeDir, "git", "commit", "-m", iid)
	sha := runCmd(t, worktreeDir, "git", "rev-parse", "HEAD")
	runCmd(t, repo.dir, "git", "worktree", "remove", worktreeDir)
	return sha
}

func (repo sharedBranchRepo) worktreeCount(t *testing.T) int {
	t.Helper()
	return strings.Count(runCmd(t, repo.dir, "git", "worktree", "list", "--porcelain"), "worktree ")
}

func TestFreeze_SharedBranchStacksTasks(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)

	firstCommit := repo.taskCommit(t, "task-1", "origin/main", "one.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1: %v", err)
	}

	// The dependent task starts from the shared branch, as wt-ensure's ResolveTip would pick.
	if tip, _ := ResolveTip(repo.dir, "shared"); tip != "wi/shared" {
		t.Fatalf("ResolveTip = %q, want wi/shared", tip)
	}
	secondCommit := repo.taskCommit(t, "task-2", "wi/shared", "two.txt")
	if err := Freeze(repo.dir, "shared", "task-2"); err != nil {
		t.Fatalf("freeze task-2: %v", err)
	}

	if got := getBranch(t, repo.dir, "wi/shared"); got != secondCommit {
		t.Errorf("wi/shared = %s, want a fast-forward to task-2's commit %s", got, secondCommit)
	}
	if !strings.Contains(runCmd(t, repo.dir, "git", "rev-list", "wi/shared"), firstCommit) {
		t.Error("task-1's commit is no longer on wi/shared")
	}
	if got := getBranch(t, repo.dir, "refs/heads/main"); got == secondCommit || got == firstCommit {
		t.Error("main must not move when tasks freeze onto a shared branch")
	}
}

func TestFreeze_ConcurrentTasksMergeIntoSharedBranch(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)

	// Two tasks start in parallel from the same base; the first fast-forwards the branch.
	firstCommit := repo.taskCommit(t, "task-1", "origin/main", "one.txt")
	secondCommit := repo.taskCommit(t, "task-2", "origin/main", "two.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1: %v", err)
	}

	var progress bytes.Buffer
	integration, err := Integrate(repo.dir, "shared", "task-2", IntegrateOptions{Progress: &progress, AllowHostGate: true})
	if err != nil {
		t.Fatalf("integrate task-2: %v", err)
	}
	if integration.Kind != "merge" || integration.Previous != firstCommit {
		t.Errorf("integration = %+v, want a merge on top of %s", integration, firstCommit)
	}

	merged := getBranch(t, repo.dir, "wi/shared")
	parents := strings.Fields(runCmd(t, repo.dir, "git", "rev-list", "--parents", "-n", "1", merged))
	if len(parents) != 3 || parents[1] != firstCommit || parents[2] != secondCommit {
		t.Fatalf("wi/shared should be a merge of task-1 %s and task-2 %s, got parents %v", firstCommit, secondCommit, parents[1:])
	}
	files := runCmd(t, repo.dir, "git", "ls-tree", "--name-only", merged)
	if !strings.Contains(files, "one.txt") || !strings.Contains(files, "two.txt") {
		t.Errorf("merged tree should hold both tasks' files, got %s", files)
	}
	if !strings.Contains(progress.String(), "running make test on the merged result") {
		t.Errorf("the merge gate should report progress, got %q", progress.String())
	}
	if count := repo.worktreeCount(t); count != 1 {
		t.Errorf("throwaway merge worktree left behind: %d worktrees", count)
	}
}

func TestIntegrate_ConflictLeavesBranchUntouched(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	repo.taskCommit(t, "task-1", "origin/main", "shared.txt")
	repo.taskCommit(t, "task-2", "origin/main", "shared.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1: %v", err)
	}
	branchBefore := getBranch(t, repo.dir, "wi/shared")

	_, err := Integrate(repo.dir, "shared", "task-2", hostGate)
	if err == nil || !strings.Contains(err.Error(), "wip/task-2 conflicts with wi/shared in: shared.txt") {
		t.Fatalf("expected a conflict refusal, got %v", err)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != branchBefore {
		t.Errorf("wi/shared moved on conflict: %s, want %s", got, branchBefore)
	}
	if !branchExists(t, repo.dir, "wip/task-2") {
		t.Error("a refused integration must leave wip/task-2 in place")
	}
	if count := repo.worktreeCount(t); count != 1 {
		t.Errorf("throwaway merge worktree left behind: %d worktrees", count)
	}
}

func TestIntegrate_FailingMergeGateLeavesBranchUntouched(t *testing.T) {
	repo := newSharedBranchRepo(t, clashingMakefile)
	repo.taskCommit(t, "task-1", "origin/main", "one.txt")
	repo.taskCommit(t, "task-2", "origin/main", "two.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1 (fast-forward, no gate): %v", err)
	}
	branchBefore := getBranch(t, repo.dir, "wi/shared")

	_, err := Integrate(repo.dir, "shared", "task-2", hostGate)
	if err == nil || !strings.Contains(err.Error(), "either they break when combined") || !strings.Contains(err.Error(), "make test failed on the merged result") {
		t.Fatalf("expected the merge gate to refuse, got %v", err)
	}
	if !strings.Contains(err.Error(), "one and two clash") {
		t.Errorf("the refusal should carry the failing output, got %v", err)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != branchBefore {
		t.Errorf("wi/shared moved despite a failing gate: %s, want %s", got, branchBefore)
	}
	if count := repo.worktreeCount(t); count != 1 {
		t.Errorf("throwaway merge worktree left behind: %d worktrees", count)
	}
}

func TestIntegrate_RerunIsANoOp(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	repo.taskCommit(t, "task-1", "origin/main", "one.txt")
	repo.taskCommit(t, "task-2", "origin/main", "two.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1: %v", err)
	}
	if _, err := Integrate(repo.dir, "shared", "task-2", hostGate); err != nil {
		t.Fatalf("first integrate: %v", err)
	}
	merged := getBranch(t, repo.dir, "wi/shared")

	// approve re-runs Integrate if the transition or cleanup after it failed.
	if _, err := Integrate(repo.dir, "shared", "task-2", hostGate); err != nil {
		t.Fatalf("second integrate: %v", err)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != merged {
		t.Errorf("re-running Integrate moved wi/shared: %s, want %s (no second merge)", got, merged)
	}
}

func TestDiffBase_SharedBranchShowsOnlyTheTask(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)
	repo.taskCommit(t, "task-1", "origin/main", "one.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1: %v", err)
	}
	secondCommit := repo.taskCommit(t, "task-2", "wi/shared", "two.txt")

	diff, err := DiffBase(repo.dir, "shared", secondCommit)
	if err != nil {
		t.Fatalf("DiffBase: %v", err)
	}
	if !strings.Contains(diff, "two.txt") {
		t.Errorf("diff should show task-2's change, got:\n%s", diff)
	}
	if strings.Contains(diff, "one.txt") {
		t.Errorf("diff should not include task-1's already-approved change, got:\n%s", diff)
	}
}
