package store

import (
	"context"
	"testing"
	"time"
)

// reviewRoundHarness drives one implement task through submit/review rounds.
type reviewRoundHarness struct {
	t         *testing.T
	store     Store
	projectID string
	taskID    string
}

func newReviewRoundHarness(t *testing.T) *reviewRoundHarness {
	t.Helper()
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", ReviewModels: []string{"opus"}, Branch: "shared"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	if _, err := store.PromoteTask(ctx, tasks[0].ID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	return &reviewRoundHarness{t: t, store: store, projectID: projectID, taskID: tasks[0].ID}
}

// submit claims the implement task and submits it with links, starting a new review round.
func (harness *reviewRoundHarness) submit(links ...LinkInput) {
	harness.t.Helper()
	ctx := context.Background()
	if _, err := harness.store.ClaimTask(ctx, harness.taskID, "worker", "haiku", 5*time.Minute); err != nil {
		harness.t.Fatalf("ClaimTask: %v", err)
	}
	if _, err := harness.store.SubmitTask(ctx, harness.taskID, "worker", "done", nil, links, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		harness.t.Fatalf("SubmitTask: %v", err)
	}
}

// review claims the round's review task and submits verdict ("approve" or "reject").
func (harness *reviewRoundHarness) review(verdict string) {
	harness.t.Helper()
	ctx := context.Background()
	reviewTasks, err := harness.store.ListTasks(ctx, harness.projectID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil || len(reviewTasks) != 1 {
		harness.t.Fatalf("expected one ready review task, got %d (%v)", len(reviewTasks), err)
	}
	if _, err := harness.store.ClaimTask(ctx, reviewTasks[0].ID, "reviewer", "opus", 5*time.Minute); err != nil {
		harness.t.Fatalf("claim review: %v", err)
	}
	if _, err := harness.store.SubmitTask(ctx, reviewTasks[0].ID, "reviewer", "verdict", &verdict, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		harness.t.Fatalf("submit review: %v", err)
	}
}

func (harness *reviewRoundHarness) task() TaskWithDepsAndLinks {
	harness.t.Helper()
	task, err := harness.store.GetTask(context.Background(), harness.taskID)
	if err != nil {
		harness.t.Fatalf("GetTask: %v", err)
	}
	return task
}

func linkRounds(task TaskWithDepsAndLinks, kind string) map[string]int {
	rounds := map[string]int{}
	for _, link := range task.Links {
		if link.Kind == kind && link.ReviewRound != nil {
			rounds[link.Value] = *link.ReviewRound
		}
	}
	return rounds
}

func TestSubmitTagsLinksWithTheirReviewRound(t *testing.T) {
	harness := newReviewRoundHarness(t)

	harness.submit(LinkInput{Kind: "commit", Value: "c1"})
	harness.review("reject")
	harness.submit(LinkInput{Kind: "commit", Value: "c2"})

	if got := linkRounds(harness.task(), "commit"); got["c1"] != 1 || got["c2"] != 2 {
		t.Fatalf("commit link rounds = %v, want c1:1 c2:2", got)
	}

	// Re-submitting an unchanged commit moves its link to the new round.
	harness.review("reject")
	harness.submit(LinkInput{Kind: "commit", Value: "c2"})
	if got := linkRounds(harness.task(), "commit"); got["c1"] != 1 || got["c2"] != 3 {
		t.Errorf("after an unchanged re-submit, rounds = %v, want c1:1 c2:3", got)
	}
}

func TestApprovedRoundIgnoresAnEarlierRoundsNoOp(t *testing.T) {
	harness := newReviewRoundHarness(t)

	// Round 1 claims a no-op; reviewers reject it. Round 2 submits real work and is approved.
	harness.submit(LinkInput{Kind: "no_op", Value: "already satisfied"})
	harness.review("reject")
	harness.submit(LinkInput{Kind: "commit", Value: "c2"})
	harness.review("approve")

	// Round 2's commit still has to be landed by `odonian approve`; finalising it straight to
	// done because of round 1's no_op would skip that and unblock dependents without the work.
	if state := harness.task().State; state != "approved" {
		t.Fatalf("task state = %q, want approved (round 1's no-op must not finalise round 2)", state)
	}
}

func TestApprovedRoundStillFinalisesACurrentNoOp(t *testing.T) {
	harness := newReviewRoundHarness(t)

	harness.submit(LinkInput{Kind: "commit", Value: "c1"})
	harness.review("reject")
	harness.submit(LinkInput{Kind: "no_op", Value: "already satisfied"})
	harness.review("approve")

	if state := harness.task().State; state != "done" {
		t.Fatalf("task state = %q, want done (the approved round is a no-op)", state)
	}
}

func TestBeginLandingOnlyLandsTheReviewedCommit(t *testing.T) {
	ctx := context.Background()
	harness := newReviewRoundHarness(t)
	harness.submit(LinkInput{Kind: "commit", Value: "c1"})
	harness.review("reject")
	harness.submit(LinkInput{Kind: "commit", Value: "c2"})
	harness.review("approve")

	if err := harness.store.BeginLanding(ctx, harness.taskID, 2, "c1", "attempt-1"); conflictCode(err) != "UNREVIEWED_COMMIT" {
		t.Errorf("landing round 1's commit for round 2: expected UNREVIEWED_COMMIT, got %v", err)
	}
	if err := harness.store.BeginLanding(ctx, harness.taskID, 2, "c-edited-after-submit", "attempt-1"); conflictCode(err) != "UNREVIEWED_COMMIT" {
		t.Errorf("landing an unsubmitted commit: expected UNREVIEWED_COMMIT, got %v", err)
	}
	if err := harness.store.BeginLanding(ctx, harness.taskID, 2, "c2", "attempt-1"); err != nil {
		t.Errorf("landing the reviewed commit: %v", err)
	}
}

func TestBeginLandingOnOlderTasksNeedsAnUnambiguousCommit(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 2)
	// A task submitted before links carried their round: its commit links are untagged.
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task_link SET review_round = NULL WHERE task_id = ?", taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Conn().ExecContext(ctx, "INSERT INTO task_link (id, task_id, kind, value) VALUES ('legacy-2', ?, 'commit', 'def')", taskID); err != nil {
		t.Fatal(err)
	}

	// A reworked legacy task has several untagged commits; any may be a rejected round's.
	for _, commit := range []string{"abc", "def", "zzz"} {
		if err := store.BeginLanding(ctx, taskID, 2, commit, "attempt-1"); conflictCode(err) != "UNREVIEWED_COMMIT" {
			t.Errorf("ambiguous legacy commits, landing %s: expected UNREVIEWED_COMMIT, got %v", commit, err)
		}
	}

	// With a single untagged commit, that one is the reviewed commit.
	if _, err := store.Conn().ExecContext(ctx, "DELETE FROM task_link WHERE id = 'legacy-2'"); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginLanding(ctx, taskID, 2, "def", "attempt-1"); conflictCode(err) != "UNREVIEWED_COMMIT" {
		t.Errorf("a commit the task never submitted: expected UNREVIEWED_COMMIT, got %v", err)
	}
	if err := store.BeginLanding(ctx, taskID, 2, "abc", "attempt-1"); err != nil {
		t.Errorf("the task's only commit: %v", err)
	}
}

func TestPlainDoneIsRefusedForWorkThatMustLand(t *testing.T) {
	ctx := context.Background()
	harness := newReviewRoundHarness(t)
	harness.submit(LinkInput{Kind: "commit", Value: "c1"})
	harness.review("approve")

	// A local_commit task reaches done only by landing its commit (odonian approve).
	if _, err := harness.store.TransitionTask(ctx, harness.taskID, "done", nil); conflictCode(err) != "LANDING_REQUIRED" {
		t.Fatalf("plain done for a reviewed commit: expected LANDING_REQUIRED, got %v", err)
	}
	if err := harness.store.BeginLanding(ctx, harness.taskID, 1, "c1", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}
	if _, err := harness.store.CompleteLanding(ctx, harness.taskID, "attempt-1", nil); err != nil {
		t.Fatalf("CompleteLanding: %v", err)
	}
	if state := harness.task().State; state != "done" {
		t.Errorf("state = %q, want done", state)
	}
}

func TestPlainDoneStillWorksForPullRequestTasks(t *testing.T) {
	ctx := context.Background()
	harness := newReviewRoundHarness(t)
	harness.submit(LinkInput{Kind: "pr", Value: "https://example.com/pr/1"}, LinkInput{Kind: "branch", Value: "mr/x"})
	harness.review("approve")

	if _, err := harness.store.TransitionTask(ctx, harness.taskID, "done", nil); err != nil {
		t.Fatalf("a PR task's human merge-and-done must still work: %v", err)
	}
}

func TestBeginLandingRefusesANoOpRound(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 2)
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task_link SET review_round = 1 WHERE task_id = ?", taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Conn().ExecContext(ctx, "INSERT INTO task_link (id, task_id, kind, value, review_round) VALUES ('noop-2', ?, 'no_op', 'already satisfied', 2)", taskID); err != nil {
		t.Fatal(err)
	}

	// Round 2 was a no-op: there is nothing reviewed to land, whatever commit is offered (here
	// round 1's, which was rejected).
	if err := store.BeginLanding(ctx, taskID, 2, "abc", "attempt-1"); conflictCode(err) != "UNREVIEWED_COMMIT" {
		t.Fatalf("landing for a no-op round: expected UNREVIEWED_COMMIT, got %v", err)
	}
}

func TestAgentMergeReworkWithoutResubmittedPRStillMerges(t *testing.T) {
	ctx := context.Background()
	harness := newReviewRoundHarness(t)
	if _, err := harness.store.Conn().ExecContext(ctx, "UPDATE task SET agent_merge = 1 WHERE id = ?", harness.taskID); err != nil {
		t.Fatal(err)
	}
	harness.submit(LinkInput{Kind: "pr", Value: "https://example.com/pr/1"}, LinkInput{Kind: "branch", Value: "mr/x"})
	harness.review("reject")
	// The rework pushes to the same PR and is submitted without re-passing it.
	harness.submit()
	harness.review("approve")

	mergeTasks, err := harness.store.ListTasks(ctx, harness.projectID, TaskListFilter{Kind: ptrStr("merge")})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(mergeTasks) != 1 {
		t.Fatalf("expected the task's PR to get a merge task, got %d", len(mergeTasks))
	}
}

func TestGetTaskReportsTheCurrentRoundsLinks(t *testing.T) {
	ctx := context.Background()
	harness := newReviewRoundHarness(t)
	harness.submit(LinkInput{Kind: "commit", Value: "c1"})
	harness.review("reject")
	harness.submit(LinkInput{Kind: "commit", Value: "c2"}, LinkInput{Kind: "ci", Value: "gone"})
	if _, err := harness.store.Conn().ExecContext(ctx, "UPDATE task_link SET tombstoned_at = '2026-01-01T00:00:00.000000000Z' WHERE value = 'gone'"); err != nil {
		t.Fatal(err)
	}

	current := harness.task().CurrentRoundLinks
	if len(current) != 1 || current[0].Value != "c2" {
		t.Errorf("current_round_links = %+v, want only c2 (round 1's c1 superseded, the tombstoned link dropped)", current)
	}
}
