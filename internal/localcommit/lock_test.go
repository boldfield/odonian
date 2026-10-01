package localcommit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func lockHeld(t *testing.T, repoDir, slug string) bool {
	t.Helper()
	_, err := git(repoDir, "rev-parse", "--verify", "--quiet", lockRefPrefix+slug)
	return err == nil
}

func TestLockBranch_ExcludesASecondApprove(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)

	heldLock, err := LockBranch(repo.dir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("LockBranch: %v", err)
	}
	if _, err := LockBranch(repo.dir, "shared", time.Minute); err == nil || !strings.Contains(err.Error(), "another approve is landing work on wi/shared") {
		t.Fatalf("a second lock on the same branch: expected a refusal, got %v", err)
	}
	// The lock ref points at a blob; ordinary history commands must not trip over it.
	runCmd(t, repo.dir, "git", "log", "--all", "--oneline")

	otherLock, err := LockBranch(repo.dir, "other", time.Minute)
	if err != nil {
		t.Fatalf("a different branch must lock independently: %v", err)
	}
	otherLock.Release()

	heldLock.Release()
	if lockHeld(t, repo.dir, "shared") {
		t.Fatal("release must remove the lock")
	}
	relock, err := LockBranch(repo.dir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("relock after release: %v", err)
	}
	relock.Release()
}

func TestLockBranch_TakesOverAnExpiredLock(t *testing.T) {
	repo := newSharedBranchRepo(t, passingMakefile)

	// A crashed approve's lock: already expired, never released.
	staleLock, err := LockBranch(repo.dir, "shared", -time.Second)
	if err != nil {
		t.Fatalf("LockBranch: %v", err)
	}
	heldLock, err := LockBranch(repo.dir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("an expired lock must be taken over: %v", err)
	}
	defer heldLock.Release()

	// If the crashed approve comes back and releases, it must not drop the new holder's lock.
	staleLock.Release()
	if _, err := LockBranch(repo.dir, "shared", time.Minute); err == nil {
		t.Fatal("a stale holder's release dropped the current holder's lock")
	}
}

func TestIntegrate_RefusesWhileTheBranchIsLocked(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	branchBefore := getBranch(t, repo.dir, "wi/shared")
	heldLock, err := LockBranch(repo.dir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("LockBranch: %v", err)
	}
	defer heldLock.Release()

	if _, err := Integrate(repo.dir, "shared", "task-2", hostGate); err == nil || !strings.Contains(err.Error(), "another approve") {
		t.Fatalf("expected Integrate to wait its turn, got %v", err)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != branchBefore {
		t.Errorf("wi/shared moved while locked: %s", got)
	}
}

func TestPrepare_BuildsTheMergeWithoutMovingTheBranch(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	branchBefore := getBranch(t, repo.dir, "wi/shared")

	integration, err := Prepare(repo.dir, "shared", "task-2", hostGate)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if integration.Kind != "merge" || integration.Previous != branchBefore {
		t.Fatalf("integration = %+v, want a merge on top of %s", integration, branchBefore)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != branchBefore {
		t.Fatalf("Prepare moved wi/shared to %s; only Publish may", got)
	}
	// The merge commit outlives the scratch worktree it was built in.
	if objectType := runCmd(t, repo.dir, "git", "cat-file", "-t", integration.Commit); objectType != "commit" {
		t.Fatalf("prepared merge %s is a %q, want a commit", integration.Commit, objectType)
	}

	heldLock, err := LockBranch(repo.dir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("LockBranch: %v", err)
	}
	defer heldLock.Release()
	if err := Publish(repo.dir, integration, heldLock); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != integration.Commit {
		t.Errorf("wi/shared = %s after Publish, want %s", got, integration.Commit)
	}
}

func TestCleanup_KeepsAWipBranchThatMovedPastTheLandedCommit(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	landed := getBranch(t, repo.dir, "wip/task-2")

	// A rework moved wip/task-2 after the landed commit was prepared.
	rework := runCmd(t, repo.dir, "git", "commit-tree", "-p", landed, "-m", "rework", runCmd(t, repo.dir, "git", "rev-parse", landed+"^{tree}"))
	runCmd(t, repo.dir, "git", "update-ref", "refs/heads/wip/task-2", rework)

	if err := Cleanup(repo.dir, "task-2", landed); err == nil || !strings.Contains(err.Error(), "holds newer work") {
		t.Fatalf("expected Cleanup to refuse, got %v", err)
	}
	if got := getBranch(t, repo.dir, "wip/task-2"); got != rework {
		t.Errorf("wip/task-2 = %s, want the rework %s kept", got, rework)
	}
	if err := Cleanup(repo.dir, "task-2", rework); err != nil {
		t.Fatalf("Cleanup of the current commit: %v", err)
	}
	if branchExists(t, repo.dir, "wip/task-2") {
		t.Error("wip/task-2 should be deleted")
	}
}

func TestPublish_RefusesAfterTheLockWasTakenOver(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	branchBefore := getBranch(t, repo.dir, "wi/shared")

	// An approve prepares its landing, then stalls past its lock's expiry...
	stalledLock, err := LockBranch(repo.dir, "shared", -time.Second)
	if err != nil {
		t.Fatalf("LockBranch: %v", err)
	}
	integration, err := Prepare(repo.dir, "shared", "task-2", hostGate)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// ...meanwhile --cancel-landing takes the expired lock over (and the task may be rejected).
	cancelLock, err := LockBranch(repo.dir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("taking over the expired lock: %v", err)
	}
	defer cancelLock.Release()

	// When the stalled approve resumes, it must publish nothing.
	if err := Publish(repo.dir, integration, stalledLock); err == nil || !strings.Contains(err.Error(), "expired and was taken over") {
		t.Fatalf("expected Publish to refuse without the lock, got %v", err)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != branchBefore {
		t.Errorf("a stale approve published: wi/shared = %s", got)
	}
}

func TestCleanup_KeepsTheWipBranchWhenTheWorktreeCannotBeRemoved(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	landed := getBranch(t, repo.dir, "wip/task-2")
	home, err := WorktreeHome()
	if err != nil {
		t.Fatal(err)
	}
	worktreeDir := filepath.Join(home, "task-2")
	runCmd(t, repo.dir, "git", "worktree", "add", worktreeDir, "wip/task-2")
	runCmd(t, repo.dir, "git", "worktree", "lock", worktreeDir)

	if err := Cleanup(repo.dir, "task-2", landed); err == nil || !strings.Contains(err.Error(), "could not remove worktree") {
		t.Fatalf("expected Cleanup to report the locked worktree, got %v", err)
	}
	if !branchExists(t, repo.dir, "wip/task-2") {
		t.Fatal("wip/task-2 must be kept while its worktree is still there")
	}
	if got := runCmd(t, worktreeDir, "git", "rev-parse", "HEAD"); got != landed {
		t.Errorf("the worktree's HEAD = %s, want it still resolving to %s", got, landed)
	}

	// Once the worktree can go, a re-run cleans up.
	runCmd(t, repo.dir, "git", "worktree", "unlock", worktreeDir)
	if err := Cleanup(repo.dir, "task-2", landed); err != nil {
		t.Fatalf("Cleanup after unlocking: %v", err)
	}
	if branchExists(t, repo.dir, "wip/task-2") {
		t.Error("wip/task-2 should be deleted")
	}
}

func TestCleanup_KeepsAWipBranchCheckedOutElsewhere(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	landed := getBranch(t, repo.dir, "wip/task-2")
	elsewhere := filepath.Join(t.TempDir(), "elsewhere")
	runCmd(t, repo.dir, "git", "worktree", "add", elsewhere, "wip/task-2")

	if err := Cleanup(repo.dir, "task-2", landed); err == nil || !strings.Contains(err.Error(), "is checked out at") {
		t.Fatalf("expected Cleanup to refuse, got %v", err)
	}
	if !branchExists(t, repo.dir, "wip/task-2") {
		t.Fatal("wip/task-2 must be kept while a worktree has it checked out")
	}
}

func TestCleanup_ToleratesAMissingWorktreeAndAPlainDirectory(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	landed := getBranch(t, repo.dir, "wip/task-2")
	home, err := WorktreeHome()
	if err != nil {
		t.Fatal(err)
	}
	// A leftover plain directory (not a worktree) at the worktree path does not block cleanup.
	if err := os.MkdirAll(filepath.Join(home, "task-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Cleanup(repo.dir, "task-2", landed); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if branchExists(t, repo.dir, "wip/task-2") {
		t.Error("wip/task-2 should be deleted")
	}
}
