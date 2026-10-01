package localcommit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// divergedRepo returns a repo where task-1 is frozen onto wi/shared and task-2 started before
// it, so integrating task-2 needs a merge (and so the gate). The Makefile is makefile.
func divergedRepo(t *testing.T, makefile string) sharedBranchRepo {
	t.Helper()
	repo := newSharedBranchRepo(t, makefile)
	repo.taskCommit(t, "task-1", "origin/main", "one.txt")
	repo.taskCommit(t, "task-2", "origin/main", "two.txt")
	if err := Freeze(repo.dir, "shared", "task-1"); err != nil {
		t.Fatalf("freeze task-1: %v", err)
	}
	return repo
}

func TestIntegrate_GateTimeoutKillsTheRun(t *testing.T) {
	repo := divergedRepo(t, ".PHONY: check test\ncheck:\n\t@true\ntest:\n\t@sleep 30\n")
	branchBefore := getBranch(t, repo.dir, "wi/shared")

	started := time.Now()
	options := hostGate
	options.GateTimeout = time.Second
	_, err := Integrate(repo.dir, "shared", "task-2", options)

	if err == nil || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("expected a gate timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "environment problem, not a verdict on the code") {
		t.Errorf("a timeout is not a test failure, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Errorf("the gate took %s to give up; the timed-out make should have been killed", elapsed)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != branchBefore {
		t.Errorf("wi/shared moved after a timeout")
	}
}

func TestIntegrate_GateEnvironmentIsScrubbed(t *testing.T) {
	// check fails if a credential from the approver's environment leaks into the gate.
	repo := divergedRepo(t, ".PHONY: check test\ncheck:\n\t@test -z \"$$ODONIAN_GATE_TEST_SECRET\" || (echo leaked; exit 1)\ntest:\n\t@true\n")
	t.Setenv("ODONIAN_GATE_TEST_SECRET", "hunter2")

	if _, err := Integrate(repo.dir, "shared", "task-2", hostGate); err != nil {
		t.Fatalf("the gate saw a variable outside its allowlist: %v", err)
	}
}

func TestIntegrate_MissingTargetIsAnEnvironmentProblem(t *testing.T) {
	repo := divergedRepo(t, ".PHONY: check\ncheck:\n\t@true\n")

	_, err := Integrate(repo.dir, "shared", "task-2", hostGate)
	if err == nil || !strings.Contains(err.Error(), "has no `make test` target") {
		t.Fatalf("expected a missing-target error, got %v", err)
	}
	if strings.Contains(err.Error(), "each passed review on their own") {
		t.Errorf("a missing target must not be blamed on the tasks: %v", err)
	}
}

func TestIntegrate_RefusesHostGateOutsideSandbox(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	t.Setenv("SANDBOX_NAME", "")

	_, err := Integrate(repo.dir, "shared", "task-2", IntegrateOptions{})
	if err == nil || !strings.Contains(err.Error(), "refusing to run that outside a sandbox") {
		t.Fatalf("expected the host guard to refuse, got %v", err)
	}

	t.Setenv("SANDBOX_NAME", "test-sandbox")
	if _, err := Integrate(repo.dir, "shared", "task-2", IntegrateOptions{}); err != nil {
		t.Fatalf("inside a sandbox the gate should run: %v", err)
	}
}

func TestIntegrate_CleansUpAMergeWorktreeLeftByAKilledRun(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	staleDir, err := mergeWorktreePath("task-2")
	if err != nil {
		t.Fatal(err)
	}
	runCmd(t, repo.dir, "git", "worktree", "add", "--detach", staleDir, "main")
	if err := os.WriteFile(filepath.Join(staleDir, "half-done.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Integrate(repo.dir, "shared", "task-2", hostGate); err != nil {
		t.Fatalf("integrate over a stale merge worktree: %v", err)
	}
	if repo.worktreeCount(t) != 1 {
		t.Errorf("merge worktree not cleaned up: %s", runCmd(t, repo.dir, "git", "worktree", "list"))
	}
}

func TestPublish_RefusesABranchCheckedOutMeanwhile(t *testing.T) {
	repo := divergedRepo(t, passingMakefile)
	previous := getBranch(t, repo.dir, "wi/shared")
	wipTip := getBranch(t, repo.dir, "wip/task-2")

	// Someone checks the branch out after Integrate's first check (e.g. during the gate).
	checkoutDir := filepath.Join(t.TempDir(), "checkout")
	runCmd(t, repo.dir, "git", "worktree", "add", checkoutDir, "wi/shared")

	heldLock, err := LockBranch(repo.dir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("LockBranch: %v", err)
	}
	defer heldLock.Release()
	err = Publish(repo.dir, Integration{Kind: "merge", Branch: "wi/shared", Commit: wipTip, Previous: previous}, heldLock)
	if err == nil || !strings.Contains(err.Error(), "is checked out at") {
		t.Fatalf("expected Publish to refuse a checked-out branch, got %v", err)
	}
	if got := getBranch(t, repo.dir, "wi/shared"); got != previous {
		t.Errorf("wi/shared moved under a checkout: %s", got)
	}
}

func TestIntegrationNote(t *testing.T) {
	note := Integration{Kind: "merge", Branch: "wi/shared", Commit: "abc123"}.Note()
	if !strings.Contains(note, "merged into wi/shared as abc123") {
		t.Errorf("unexpected note %q", note)
	}
	if note := (Integration{Kind: "fast-forward", Branch: "wi/shared", Commit: "abc123"}).Note(); !strings.Contains(note, "fast-forwarded wi/shared to abc123") {
		t.Errorf("unexpected note %q", note)
	}
}

func TestIntegrate_GateEnvironmentCanBeExtended(t *testing.T) {
	// check needs a variable outside the default allowlist.
	repo := divergedRepo(t, ".PHONY: check test\ncheck:\n\t@test \"$$ODONIAN_GATE_TEST_TOOL\" = present\ntest:\n\t@true\n")
	t.Setenv("ODONIAN_GATE_TEST_TOOL", "present")

	if _, err := Integrate(repo.dir, "shared", "task-2", hostGate); err == nil || !strings.Contains(err.Error(), "ODONIAN_GATE_ENV") {
		t.Fatalf("without ODONIAN_GATE_ENV the gate should fail and say how to fix it, got %v", err)
	}
	t.Setenv("ODONIAN_GATE_ENV", "ODONIAN_GATE_TEST_TOOL")
	if _, err := Integrate(repo.dir, "shared", "task-2", hostGate); err != nil {
		t.Fatalf("with ODONIAN_GATE_ENV naming it, the gate should pass: %v", err)
	}
}
