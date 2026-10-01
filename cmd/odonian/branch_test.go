package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/localcommit"
	"github.com/boldfield/odonian/internal/tuiclient"
)

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func refExists(repoDir, ref string) bool {
	return exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref).Run() == nil
}

// commitFile writes file=content in dir and commits it.
func commitFile(t *testing.T, dir, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, dir, "add", "-A")
	gitOutput(t, dir, "commit", "-q", "-m", "write "+file)
}

// sharedBranchSetup is a local_commit repo (with passing make check/test) whose shared branch
// wi/shared already holds one frozen task's commit writing shared.txt.
func sharedBranchSetup(t *testing.T) (repoDir, sharedTip string) {
	t.Helper()
	repoDir = t.TempDir()
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	t.Setenv("ODONIAN_WORKTREE_HOME", t.TempDir())
	gitOutput(t, repoDir, "init", "-b", "main")
	gitOutput(t, repoDir, "config", "user.email", "test@example.com")
	gitOutput(t, repoDir, "config", "user.name", "Test User")
	commitFile(t, repoDir, "Makefile", ".PHONY: check test\ncheck:\n\t@true\ntest:\n\t@true\n")
	gitOutput(t, repoDir, "update-ref", "refs/remotes/origin/main", "HEAD")
	gitOutput(t, repoDir, "checkout", "-q", "-b", "wi/shared")
	commitFile(t, repoDir, "shared.txt", "first task\n")
	sharedTip = gitOutput(t, repoDir, "rev-parse", "HEAD")
	gitOutput(t, repoDir, "checkout", "-q", "main")
	return repoDir, sharedTip
}

// concurrentTask creates wip/task-2 from main (i.e. started before the first task froze onto
// wi/shared) with one commit writing file=content, and returns that commit.
func concurrentTask(t *testing.T, repoDir, file, content string) string {
	t.Helper()
	worktreeDir := filepath.Join(t.TempDir(), "task-2")
	gitOutput(t, repoDir, "worktree", "add", "-q", "-b", "wip/task-2", worktreeDir, "main")
	commitFile(t, worktreeDir, file, content)
	commit := gitOutput(t, worktreeDir, "rev-parse", "HEAD")
	gitOutput(t, repoDir, "worktree", "remove", worktreeDir)
	return commit
}

// gateRunsMakefile is a Makefile whose `make test` runs command, so a test can act in the middle
// of approve's merge gate.
func gateRunsMakefile(command string) string {
	return ".PHONY: check test\ncheck:\n\t@true\ntest:\n\t@" + command + "\n"
}

// fakeBoard is a stateful stand-in for the board serving task-2 (approved in review round 1).
type fakeBoard struct {
	mu             sync.Mutex
	repoDir        string
	state          string
	reviewRound    int
	landingRound   *int
	landingCommit  *string
	landingAttempt *string
	links          []tuiclient.TaskLink
	// afterLanding runs (under the board's lock) right after a successful reservation, to stage
	// what other processes do while this approve continues.
	afterLanding func(board *fakeBoard)
	// landingMode: "" reserves; "stale" refuses because the task was reworked and re-approved
	// meanwhile; "drop" reserves and then drops the connection.
	landingMode string
	// transitionMode: "" applies the transition; "fail" answers 500 without applying it; "drop"
	// applies it and then drops the connection, so the client never sees the answer.
	transitionMode string
	// failGets makes every GET after the first mutating call fail, so outcomes stay unknown.
	failGets bool

	mutations       int
	transitions     int
	landings        int
	cancelRequests  int
	cancellations   int
	branchTipAtDone string // wi/shared when the board recorded done
}

// newFakeBoard serves task-2 in review round 1, whose submission is wip/task-2's current commit.
func newFakeBoard(repoDir, state string) *fakeBoard {
	board := &fakeBoard{repoDir: repoDir, state: state, reviewRound: 1}
	if submitted, err := gitRef(repoDir, "refs/heads/wip/task-2"); err == nil {
		round := 1
		board.links = []tuiclient.TaskLink{{Kind: "commit", Value: submitted, ReviewRound: &round}}
	}
	return board
}

// boardSnapshot is a copy of what the fake board recorded, safe to read without its lock.
type boardSnapshot struct {
	state           string
	landingRound    *int
	landingAttempt  *string
	transitions     int
	cancelRequests  int
	landings        int
	cancellations   int
	branchTipAtDone string
}

func (board *fakeBoard) snapshot() boardSnapshot {
	board.mu.Lock()
	defer board.mu.Unlock()
	return boardSnapshot{state: board.state, landingRound: board.landingRound, landingAttempt: board.landingAttempt, transitions: board.transitions, cancelRequests: board.cancelRequests,
		landings: board.landings, cancellations: board.cancellations, branchTipAtDone: board.branchTipAtDone}
}

func writeConflict(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": code}})
}

func (board *fakeBoard) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		board.mu.Lock()
		defer board.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/landing") && r.Method == http.MethodPost:
			board.mutations++
			board.landings++
			var payload struct {
				ReviewRound int    `json:"review_round"`
				Commit      string `json:"commit"`
				Attempt     string `json:"attempt"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if board.landingMode == "stale" {
				board.reviewRound++ // reworked and re-approved while the gate ran
			}
			switch {
			case board.state != "approved":
				writeConflict(w, http.StatusConflict, "NOT_APPROVED")
				return
			case payload.ReviewRound != board.reviewRound:
				writeConflict(w, http.StatusConflict, "STALE_REVIEW_ROUND")
				return
			case board.landingRound != nil && (*board.landingRound != payload.ReviewRound || *board.landingCommit != payload.Commit):
				writeConflict(w, http.StatusConflict, "LANDING_IN_PROGRESS")
				return
			}
			board.landingRound, board.landingCommit, board.landingAttempt = &payload.ReviewRound, &payload.Commit, &payload.Attempt
			if board.afterLanding != nil {
				board.afterLanding(board)
			}
			if board.landingMode == "drop" {
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/landing") && r.Method == http.MethodDelete:
			board.mutations++
			board.cancelRequests++
			if board.landingRound != nil && (board.landingAttempt == nil || *board.landingAttempt != r.URL.Query().Get("attempt")) {
				writeConflict(w, http.StatusConflict, "LANDING_ATTEMPT_MISMATCH")
				return
			}
			board.cancellations++
			board.landingRound, board.landingCommit, board.landingAttempt = nil, nil, nil
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/landing/complete") && r.Method == http.MethodPost:
			board.mutations++
			board.transitions++
			var payload struct {
				Attempt string `json:"attempt"`
			}
			json.NewDecoder(r.Body).Decode(&payload)
			if board.landingAttempt == nil || *board.landingAttempt != payload.Attempt {
				writeConflict(w, http.StatusConflict, "LANDING_ATTEMPT_MISMATCH")
				return
			}
			if board.transitionMode == "fail" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			board.state = "done"
			board.landingRound, board.landingCommit, board.landingAttempt = nil, nil, nil
			board.branchTipAtDone, _ = gitRef(board.repoDir, "wi/shared")
			if board.transitionMode == "drop" {
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/transition") && r.Method == http.MethodPost:
			board.mutations++
			board.transitions++
			if board.transitionMode == "fail" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			board.state = "done"
			board.landingRound, board.landingCommit, board.landingAttempt = nil, nil, nil
			board.branchTipAtDone, _ = gitRef(board.repoDir, "wi/shared")
			if board.transitionMode == "drop" {
				panic(http.ErrAbortHandler)
			}
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet:
			if board.failGets && board.mutations > 0 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{ID: "task-2", State: board.state, Title: "Second task", Branch: "shared",
				ReviewRound: board.reviewRound, LandingRound: board.landingRound, LandingCommit: board.landingCommit, LandingAttempt: board.landingAttempt, Links: board.links, CurrentRoundLinks: serverCurrentRoundLinks(board.links, board.reviewRound)})
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// serverCurrentRoundLinks is what the server reports as current_round_links: active links tagged
// with the current round, or, for a task none of whose links are tagged, all of them.
func serverCurrentRoundLinks(links []tuiclient.TaskLink, reviewRound int) []tuiclient.TaskLink {
	var roundLinks, untagged []tuiclient.TaskLink
	anyTagged := false
	for _, link := range links {
		if link.TombstonedAt != nil {
			continue
		}
		switch {
		case link.ReviewRound == nil:
			untagged = append(untagged, link)
		case *link.ReviewRound == reviewRound:
			anyTagged = true
			roundLinks = append(roundLinks, link)
		default:
			anyTagged = true
		}
	}
	if anyTagged {
		return roundLinks
	}
	return untagged
}

func gitRef(repoDir, ref string) (string, error) {
	output, err := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", ref).Output()
	return strings.TrimSpace(string(output)), err
}

func approveTask2(server *httptest.Server, repoDir string, extraArgs ...string) error {
	args := append([]string{"task-2", "--repo", repoDir, "--allow-host-gate"}, extraArgs...)
	return executeApprove(context.Background(), server.URL, "test-token", args)
}

// assertNothingLanded checks that an approve that did not complete left wi/shared at sharedTip,
// kept wip/task-2, and gave back the branch lock.
func assertNothingLanded(t *testing.T, repoDir, sharedTip string) {
	t.Helper()
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("wi/shared moved to %s, want it left at %s", got, sharedTip)
	}
	if !refExists(repoDir, "refs/heads/wip/task-2") {
		t.Error("wip/task-2 must be kept until its work has landed")
	}
	if refExists(repoDir, "refs/odonian/locks/shared") {
		t.Error("the branch lock must be released")
	}
}

// assertTask2Landed checks that task-2's work is on wi/shared as a merge on top of sharedTip,
// that the board recorded done only once that merge was on the branch, and that its wip branch
// and the branch lock are gone.
func assertTask2Landed(t *testing.T, repoDir, sharedTip string, board *fakeBoard) {
	t.Helper()
	merged := gitOutput(t, repoDir, "rev-parse", "wi/shared")
	if parents := strings.Fields(gitOutput(t, repoDir, "rev-list", "--parents", "-n", "1", merged)); len(parents) != 3 || parents[1] != sharedTip {
		t.Errorf("wi/shared should be a merge on top of the first task %s, got %v", sharedTip, parents)
	}
	files := gitOutput(t, repoDir, "ls-tree", "--name-only", merged)
	if !strings.Contains(files, "shared.txt") || !strings.Contains(files, "two.txt") {
		t.Errorf("merged branch should hold both tasks' work, got %s", files)
	}
	snapshot := board.snapshot()
	if snapshot.state != "done" {
		t.Errorf("board state = %q, want done", snapshot.state)
	}
	if snapshot.branchTipAtDone != merged {
		t.Errorf("the board recorded done while wi/shared was at %s, before the work landed at %s: dependents could start without it", snapshot.branchTipAtDone, merged)
	}
	if refExists(repoDir, "refs/heads/wip/task-2") {
		t.Error("wip/task-2 should be cleaned up once its work has landed")
	}
	if refExists(repoDir, "refs/odonian/locks/shared") {
		t.Error("the branch lock must be released")
	}
}

func TestExecuteWtEnsureStartsFromSharedBranch(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	server := newFakeBoard(repoDir, "in_progress").serve(t)

	oldStdout := os.Stdout
	readPipe, writePipe, _ := os.Pipe()
	os.Stdout = writePipe
	err := executeWtEnsure(context.Background(), server.URL, "test-token", []string{"task-2", "--repo", repoDir})
	writePipe.Close()
	os.Stdout = oldStdout
	if err != nil {
		t.Fatalf("executeWtEnsure: %v", err)
	}
	output := make([]byte, 4096)
	count, _ := readPipe.Read(output)
	worktreePath := strings.TrimSpace(string(output[:count]))

	if got := gitOutput(t, worktreePath, "rev-parse", "HEAD"); got != sharedTip {
		t.Errorf("task-2 worktree starts at %s, want the shared branch tip %s", got, sharedTip)
	}
}

func TestExecuteApproveMergesConcurrentTaskIntoSharedBranch(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")
	board := newFakeBoard(repoDir, "approved")

	if err := approveTask2(board.serve(t), repoDir); err != nil {
		t.Fatalf("executeApprove: %v", err)
	}

	if snapshot := board.snapshot(); snapshot.transitions != 1 || snapshot.landings != 1 {
		t.Errorf("expected 1 landing reservation and 1 transition, got %d and %d", snapshot.landings, snapshot.transitions)
	}
	assertTask2Landed(t, repoDir, sharedTip, board)
}

func TestExecuteApproveRefusesConflictBeforeTouchingTheBoard(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "shared.txt", "second task\n")
	board := newFakeBoard(repoDir, "approved")

	err := approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "not approved, task left in approved") || !strings.Contains(err.Error(), "conflicts with wi/shared") {
		t.Fatalf("expected a conflict refusal, got %v", err)
	}
	if snapshot := board.snapshot(); snapshot.landings != 0 || snapshot.transitions != 0 {
		t.Errorf("a refused landing must not touch the board, got %d reservations and %d transitions", snapshot.landings, snapshot.transitions)
	}
	assertNothingLanded(t, repoDir, sharedTip)
}

func TestExecuteApproveNeverLandsAVersionReworkedDuringTheGate(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)

	// While the merge gate runs, the task is rejected, reworked and re-approved: the worker's
	// rework moves wip/task-2 to a new commit, and the board moves to review round 2.
	reworkCommit := gitOutput(t, repoDir, "commit-tree", "-p", "main", "-m", "rework", gitOutput(t, repoDir, "rev-parse", "main^{tree}"))
	concurrentTask(t, repoDir, "Makefile", gateRunsMakefile(fmt.Sprintf("git -C %s update-ref refs/heads/wip/task-2 %s", repoDir, reworkCommit)))
	board := newFakeBoard(repoDir, "approved")
	board.landingMode = "stale"

	err := approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "STALE_REVIEW_ROUND") || !strings.Contains(err.Error(), "nothing was put on wi/shared") {
		t.Fatalf("expected the stale review round to stop the approve, got %v", err)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("the obsolete version was published: wi/shared = %s", got)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wip/task-2"); got != reworkCommit {
		t.Errorf("wip/task-2 = %s; the newly reviewed rework %s must be kept", got, reworkCommit)
	}
	if snapshot := board.snapshot(); snapshot.transitions != 0 {
		t.Errorf("expected no transition, got %d", snapshot.transitions)
	}
}

func TestExecuteApproveGivesTheReservationBackWhenPublishIsRefused(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)

	// Someone checks out wi/shared while the gate runs, so the branch cannot be moved.
	checkoutDir := filepath.Join(t.TempDir(), "checkout")
	concurrentTask(t, repoDir, "Makefile", gateRunsMakefile(fmt.Sprintf("git -C %s worktree add -q %s wi/shared", repoDir, checkoutDir)))
	board := newFakeBoard(repoDir, "approved")

	err := approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "nothing was put on wi/shared") || !strings.Contains(err.Error(), "is checked out at") {
		t.Fatalf("expected publishing to be refused, got %v", err)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("wi/shared moved under a checkout: %s", got)
	}
	if snapshot := board.snapshot(); snapshot.cancellations != 1 || snapshot.landingRound != nil || snapshot.transitions != 0 || snapshot.state != "approved" {
		t.Errorf("expected the reservation given back and the task left approved, got %+v", snapshot)
	}
}

func TestExecuteApproveLandsWorkWhenTheDoneResponseIsLost(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")

	// The board applies done but the response never arrives: approve must check, find the task
	// done, and finish rather than report a failure.
	board := newFakeBoard(repoDir, "approved")
	board.transitionMode = "drop"
	if err := approveTask2(board.serve(t), repoDir); err != nil {
		t.Fatalf("executeApprove: %v", err)
	}
	assertTask2Landed(t, repoDir, sharedTip, board)
}

func TestExecuteApproveFailedDoneTransitionKeepsTheTaskReservedUntilRerun(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")
	board := newFakeBoard(repoDir, "approved")
	board.transitionMode = "fail"
	server := board.serve(t)

	err := approveTask2(server, repoDir)

	if err == nil || !strings.Contains(err.Error(), "could not be marked done") || !strings.Contains(err.Error(), "Re-run `odonian approve task-2`") {
		t.Fatalf("expected a failed done transition, got %v", err)
	}
	if snapshot := board.snapshot(); snapshot.state != "approved" || snapshot.landingRound == nil {
		t.Fatalf("the task must stay approved and reserved (so it cannot be rejected and dependents stay blocked), got %+v", snapshot)
	}
	if !refExists(repoDir, "refs/heads/wip/task-2") {
		t.Error("wip/task-2 must be kept until the task is done")
	}

	board.mu.Lock()
	board.transitionMode = ""
	board.mu.Unlock()
	if err := approveTask2(server, repoDir); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	assertTask2Landed(t, repoDir, sharedTip, board)
}

func TestExecuteApproveLostReservationResponseStillLands(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")
	board := newFakeBoard(repoDir, "approved")
	board.landingMode = "drop"

	if err := approveTask2(board.serve(t), repoDir); err != nil {
		t.Fatalf("executeApprove: %v", err)
	}
	assertTask2Landed(t, repoDir, sharedTip, board)
}

func TestExecuteApproveUnknownReservationOutcomePublishesNothingUntilRerun(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")

	// The reservation's response is lost and the board cannot be asked afterwards either.
	board := newFakeBoard(repoDir, "approved")
	board.landingMode = "drop"
	board.failGets = true
	server := board.serve(t)
	err := approveTask2(server, repoDir)

	if err == nil || !strings.Contains(err.Error(), "could not tell whether the task was reserved for landing") {
		t.Fatalf("expected an unknown-outcome error, got %v", err)
	}
	assertNothingLanded(t, repoDir, sharedTip)

	// The board answers again: the task turns out to be reserved, and re-running approve resumes.
	board.mu.Lock()
	board.failGets = false
	board.landingMode = ""
	board.mu.Unlock()
	if err := approveTask2(server, repoDir); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	assertTask2Landed(t, repoDir, sharedTip, board)
}

func TestExecuteApproveRefusesWhileAnotherApproveHoldsTheBranch(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")
	heldLock, err := localcommit.LockBranch(repoDir, "shared", time.Minute)
	if err != nil {
		t.Fatalf("LockBranch: %v", err)
	}
	defer heldLock.Release()

	board := newFakeBoard(repoDir, "approved")
	err = approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "another approve is landing work on wi/shared") {
		t.Fatalf("expected the branch lock to refuse, got %v", err)
	}
	if snapshot := board.snapshot(); snapshot.landings != 0 || snapshot.transitions != 0 {
		t.Errorf("expected the board untouched, got %+v", snapshot)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("wi/shared moved to %s while another approve held it", got)
	}
}

func TestExecuteApproveCancelLanding(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	wipCommit := concurrentTask(t, repoDir, "two.txt", "second task\n")
	reservedRound := 1

	// An approve reserved the task and died before publishing: the reservation can be dropped.
	deadAttempt := "attempt-of-a-dead-approve"
	board := newFakeBoard(repoDir, "approved")
	board.landingRound, board.landingCommit, board.landingAttempt = &reservedRound, &wipCommit, &deadAttempt
	server := board.serve(t)
	if err := approveTask2(server, repoDir, "--cancel-landing"); err != nil {
		t.Fatalf("--cancel-landing: %v", err)
	}
	if snapshot := board.snapshot(); snapshot.landingRound != nil || snapshot.state != "approved" {
		t.Errorf("expected the reservation dropped and the task approved, got %+v", snapshot)
	}
	assertNothingLanded(t, repoDir, sharedTip)

	// Once the reserved work is on the branch, dropping the reservation would let the task be
	// rejected with its work left there, so it is refused.
	gitOutput(t, repoDir, "update-ref", "refs/heads/wi/shared", wipCommit)
	board.mu.Lock()
	board.landingRound, board.landingCommit, board.landingAttempt = &reservedRound, &wipCommit, &deadAttempt
	board.mu.Unlock()
	if err := approveTask2(server, repoDir, "--cancel-landing"); err == nil || !strings.Contains(err.Error(), "already on wi/shared") {
		t.Fatalf("expected --cancel-landing to refuse published work, got %v", err)
	}
	if snapshot := board.snapshot(); snapshot.landingRound == nil {
		t.Error("the reservation must stay while its work is on the branch")
	}
}

func TestExecuteApproveFinishesADoneTaskLeftHalfway(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")

	// The task is done but its wip branch is still here (e.g. marked done by hand).
	board := newFakeBoard(repoDir, "done")
	if err := approveTask2(board.serve(t), repoDir); err != nil {
		t.Fatalf("executeApprove: %v", err)
	}

	merged := gitOutput(t, repoDir, "rev-parse", "wi/shared")
	if parents := strings.Fields(gitOutput(t, repoDir, "rev-list", "--parents", "-n", "1", merged)); len(parents) != 3 || parents[1] != sharedTip {
		t.Errorf("wi/shared should be a merge on top of %s, got %v", sharedTip, parents)
	}
	if refExists(repoDir, "refs/heads/wip/task-2") {
		t.Error("wip/task-2 should be cleaned up")
	}
	if snapshot := board.snapshot(); snapshot.transitions != 0 {
		t.Errorf("a done task must not be transitioned again, got %d", snapshot.transitions)
	}
}

func TestExecuteApproveRejectsNonPositiveGateTimeout(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")
	board := newFakeBoard(repoDir, "approved")

	err := approveTask2(board.serve(t), repoDir, "--gate-timeout", "0s")

	if err == nil || !strings.Contains(err.Error(), "--gate-timeout must be positive") {
		t.Fatalf("expected a gate-timeout validation error, got %v", err)
	}
	assertNothingLanded(t, repoDir, sharedTip)
}

func TestExecuteApproveFreezeOnlyRequiresDone(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")

	board := newFakeBoard(repoDir, "approved")
	err := approveTask2(board.serve(t), repoDir, "--freeze-only")

	if err == nil || !strings.Contains(err.Error(), "--freeze-only is for a task that is already done") {
		t.Fatalf("expected --freeze-only to refuse an approved task, got %v", err)
	}
	assertNothingLanded(t, repoDir, sharedTip)
}

func TestExecuteApproveFreezeOnlyAfterCleanupIsANoOp(t *testing.T) {
	repoDir, _ := sharedBranchSetup(t)

	// The task is done and wip/task-2 is already gone: nothing left to land.
	board := newFakeBoard(repoDir, "done")
	if err := executeApprove(context.Background(), board.serve(t).URL, "test-token", []string{"task-2", "--repo", repoDir, "--freeze-only"}); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestExecuteApproveThatLostItsLockLeavesTheReplacementsReservation(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")

	// Approve A reserves the task, then stalls. Meanwhile approve B takes over A's expired lock,
	// resumes the landing (re-reserving it, so it owns the reservation), publishes, and fails
	// to mark the task done, leaving it reserved with its work on wi/shared.
	board := newFakeBoard(repoDir, "approved")
	board.afterLanding = func(board *fakeBoard) {
		replacementAttempt := "attempt-B"
		board.landingAttempt = &replacementAttempt
		foreignLock, _ := exec.Command("sh", "-c", "printf '{}' | git -C "+repoDir+" hash-object -w --stdin").Output()
		exec.Command("git", "-C", repoDir, "update-ref", "refs/odonian/locks/shared", strings.TrimSpace(string(foreignLock))).Run()
		board.afterLanding = nil
	}

	err := approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "not landed by this approve") || !strings.Contains(err.Error(), "expired and was taken over") {
		t.Fatalf("expected A to stop after losing its lock, got %v", err)
	}
	snapshot := board.snapshot()
	if snapshot.cancelRequests != 0 {
		t.Errorf("A must not try to give back a reservation it may no longer own, got %d cancel requests", snapshot.cancelRequests)
	}
	if snapshot.landingRound == nil || snapshot.landingAttempt == nil || *snapshot.landingAttempt != "attempt-B" {
		t.Errorf("B's reservation must survive, got round %v attempt %v", snapshot.landingRound, snapshot.landingAttempt)
	}
	if snapshot.transitions != 0 {
		t.Errorf("expected no transition from A, got %d", snapshot.transitions)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("A published without its lock: wi/shared = %s", got)
	}
}

func TestExecuteApproveRepairsAFailedCleanupOnRerun(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	concurrentTask(t, repoDir, "two.txt", "second task\n")
	worktreeDir := filepath.Join(os.Getenv("ODONIAN_WORKTREE_HOME"), "task-2")
	gitOutput(t, repoDir, "worktree", "add", "-q", worktreeDir, "wip/task-2")
	gitOutput(t, repoDir, "worktree", "lock", worktreeDir)
	board := newFakeBoard(repoDir, "approved")
	server := board.serve(t)

	err := approveTask2(server, repoDir)

	if err == nil || !strings.Contains(err.Error(), "cleanup failed") || !strings.Contains(err.Error(), "could not remove worktree") {
		t.Fatalf("expected approve to report the failed cleanup, got %v", err)
	}
	if snapshot := board.snapshot(); snapshot.state != "done" {
		t.Fatalf("the work landed, so the task should be done, got %q", snapshot.state)
	}
	if !refExists(repoDir, "refs/heads/wip/task-2") {
		t.Fatal("wip/task-2 must be kept while its worktree could not be removed")
	}
	gitOutput(t, worktreeDir, "rev-parse", "HEAD") // the worktree must still be usable

	// Once the worktree can be removed, re-running approve finishes the cleanup.
	gitOutput(t, repoDir, "worktree", "unlock", worktreeDir)
	if err := approveTask2(server, repoDir); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	assertTask2Landed(t, repoDir, sharedTip, board)
	if _, err := os.Stat(worktreeDir); !os.IsNotExist(err) {
		t.Errorf("the worktree should be removed, stat err = %v", err)
	}
}
