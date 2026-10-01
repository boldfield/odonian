package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/tuiclient"
)

func roundPointer(round int) *int { return &round }

func TestReviewedCommitPicksTheCurrentRound(t *testing.T) {
	// The stale round's link comes first, as a random link id can order it.
	task := withCurrentRound(tuiclient.TaskDetail{ReviewRound: 2, Links: []tuiclient.TaskLink{
		{Kind: "commit", Value: "round-one", ReviewRound: roundPointer(1)},
		{Kind: "commit", Value: "round-two", ReviewRound: roundPointer(2)},
	}})
	if got, err := reviewedCommit(task); err != nil || got != "round-two" {
		t.Errorf("reviewedCommit = %q, %v; want round-two", got, err)
	}
	if label := linkRoundLabel(task, task.Links[0]); label != " (round 1, superseded)" {
		t.Errorf("label for round 1 = %q", label)
	}
	if label := linkRoundLabel(task, task.Links[1]); label != " (round 2, current)" {
		t.Errorf("label for round 2 = %q", label)
	}
}

func TestReviewedCommitCases(t *testing.T) {
	cases := []struct {
		name      string
		task      tuiclient.TaskDetail
		want      string
		wantError string
	}{
		{
			name: "current round is a no-op after an earlier commit",
			task: tuiclient.TaskDetail{ReviewRound: 2, Links: []tuiclient.TaskLink{
				{Kind: "commit", Value: "round-one", ReviewRound: roundPointer(1)},
				{Kind: "no_op", Value: "already satisfied", ReviewRound: roundPointer(2)},
			}},
			wantError: "review round 2 submitted no commit",
		},
		{
			name: "untagged single commit (submitted before links carried rounds)",
			task: tuiclient.TaskDetail{ReviewRound: 1, Links: []tuiclient.TaskLink{{Kind: "commit", Value: "only"}}},
			want: "only",
		},
		{
			name: "untagged commits from several rounds are ambiguous",
			task: tuiclient.TaskDetail{ReviewRound: 2, Links: []tuiclient.TaskLink{
				{Kind: "commit", Value: "a"}, {Kind: "commit", Value: "b"},
			}},
			wantError: "which one round 2 reviews is unknown",
		},
		{
			name:      "no links",
			task:      tuiclient.TaskDetail{ReviewRound: 1},
			wantError: "task has no commit link",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := reviewedCommit(withCurrentRound(testCase.task))
			if testCase.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("expected error containing %q, got %q, %v", testCase.wantError, got, err)
				}
				return
			}
			if err != nil || got != testCase.want {
				t.Fatalf("reviewedCommit = %q, %v; want %q", got, err, testCase.want)
			}
		})
	}
}

func TestExecuteDiffShowsTheCurrentRoundsCommit(t *testing.T) {
	repoDir, _ := sharedBranchSetup(t)
	firstRound := concurrentTask(t, repoDir, "first-round.txt", "first attempt\n")
	gitOutput(t, repoDir, "branch", "-D", "wip/task-2")
	secondRound := concurrentTask(t, repoDir, "second-round.txt", "rework\n")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(withCurrentRound(tuiclient.TaskDetail{ID: "task-2", State: "review", ReviewRound: 2, Links: []tuiclient.TaskLink{
			{Kind: "commit", Value: firstRound, ReviewRound: roundPointer(1)},
			{Kind: "commit", Value: secondRound, ReviewRound: roundPointer(2)},
		}}))
	}))
	defer server.Close()

	var output bytes.Buffer
	if err := executeDiff(context.Background(), server.URL, "test-token", []string{"task-2", "--repo", repoDir}, &output); err != nil {
		t.Fatalf("executeDiff: %v", err)
	}
	if !strings.Contains(output.String(), "second-round.txt") || strings.Contains(output.String(), "first-round.txt") {
		t.Errorf("diff should show round 2's commit only, got:\n%s", output.String())
	}
}

func TestExecuteApproveRefusesAWipBranchThatIsNotTheReviewedCommit(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	reviewed := concurrentTask(t, repoDir, "two.txt", "second task\n")

	// After submitting, the wip branch was changed by hand.
	edited := gitOutput(t, repoDir, "commit-tree", "-p", reviewed, "-m", "edited after submit", gitOutput(t, repoDir, "rev-parse", reviewed+"^{tree}"))
	gitOutput(t, repoDir, "update-ref", "refs/heads/wip/task-2", edited)

	board := newFakeBoard(repoDir, "approved")
	board.links = []tuiclient.TaskLink{{Kind: "commit", Value: reviewed, ReviewRound: roundPointer(1)}}
	err := approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "but review round 1 reviewed "+reviewed) {
		t.Fatalf("expected approve to refuse the unreviewed wip commit, got %v", err)
	}
	if snapshot := board.snapshot(); snapshot.landings != 0 || snapshot.transitions != 0 {
		t.Errorf("expected the board untouched, got %+v", snapshot)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("wi/shared moved to %s", got)
	}
}

// reviewedTaskDetail is a task in review round 1 whose round-1 submission is commit.
func reviewedTaskDetail(id, state, title, commit string) tuiclient.TaskDetail {
	return withCurrentRound(tuiclient.TaskDetail{ID: id, State: state, Title: title, ReviewRound: 1,
		Links: []tuiclient.TaskLink{{Kind: "commit", Value: commit, ReviewRound: roundPointer(1)}}})
}

func TestExecuteApproveDoneNoOpNeverLandsLeftoverWork(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	// Round 1 submitted this commit and was rejected; round 2 was a no-op that reached done.
	rejected := concurrentTask(t, repoDir, "two.txt", "rejected attempt\n")

	board := newFakeBoard(repoDir, "done")
	board.reviewRound = 2
	board.links = []tuiclient.TaskLink{
		{Kind: "commit", Value: rejected, ReviewRound: roundPointer(1)},
		{Kind: "no_op", Value: "already satisfied", ReviewRound: roundPointer(2)},
	}
	if err := approveTask2(board.serve(t), repoDir); err != nil {
		t.Fatalf("executeApprove: %v", err)
	}

	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("the rejected round's commit was published: wi/shared = %s", got)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wip/task-2"); got != rejected {
		t.Errorf("wip/task-2 = %s; it must be left for the human, not landed or deleted", got)
	}
}

func TestExecuteApproveDoneTaskLandsOnlyTheApprovedCommit(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	approved := concurrentTask(t, repoDir, "two.txt", "second task\n")
	edited := gitOutput(t, repoDir, "commit-tree", "-p", approved, "-m", "edited after approval", gitOutput(t, repoDir, "rev-parse", approved+"^{tree}"))
	gitOutput(t, repoDir, "update-ref", "refs/heads/wip/task-2", edited)

	board := newFakeBoard(repoDir, "done")
	board.links = []tuiclient.TaskLink{{Kind: "commit", Value: approved, ReviewRound: roundPointer(1)}}
	err := approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "nothing was landed") || !strings.Contains(err.Error(), "reviewed "+approved) {
		t.Fatalf("expected recovery to refuse an unapproved wip commit, got %v", err)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("wi/shared moved to %s", got)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wip/task-2"); got != edited {
		t.Errorf("wip/task-2 = %s, want it left at %s", got, edited)
	}
}

func TestExecuteApproveRefusesAmbiguousLegacyCommits(t *testing.T) {
	repoDir, sharedTip := sharedBranchSetup(t)
	current := concurrentTask(t, repoDir, "two.txt", "second task\n")

	// Reworked before links recorded their round: two untagged commit links, either of which
	// could be the rejected round's.
	board := newFakeBoard(repoDir, "approved")
	board.reviewRound = 2
	board.links = []tuiclient.TaskLink{{Kind: "commit", Value: "0123456789abcdef0123456789abcdef01234567"}, {Kind: "commit", Value: current}}
	err := approveTask2(board.serve(t), repoDir)

	if err == nil || !strings.Contains(err.Error(), "which one round 2 reviews is unknown") {
		t.Fatalf("expected approve to refuse ambiguous legacy commits, got %v", err)
	}
	if snapshot := board.snapshot(); snapshot.landings != 0 || snapshot.transitions != 0 {
		t.Errorf("expected the board untouched, got %+v", snapshot)
	}
	if got := gitOutput(t, repoDir, "rev-parse", "wi/shared"); got != sharedTip {
		t.Errorf("wi/shared moved to %s", got)
	}
}

// withCurrentRound fills in current_round_links as the server would report them.
func withCurrentRound(task tuiclient.TaskDetail) tuiclient.TaskDetail {
	task.CurrentRoundLinks = serverCurrentRoundLinks(task.Links, task.ReviewRound)
	return task
}
