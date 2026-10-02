package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/manifest"
)

// approvedTaskInRound creates one task, forces it to approved in the given review round, and
// records that round's submitted commit as "abc".
func approvedTaskInRound(t *testing.T, reviewRound int) (Store, string) {
	t.Helper()
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: docID, Model: "haiku", Branch: "shared"},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'approved', review_round = ? WHERE id = ?", reviewRound, tasks[0].ID); err != nil {
		t.Fatalf("force approved: %v", err)
	}
	// The round's submission: commit "abc".
	if _, err := store.Conn().ExecContext(ctx, "INSERT INTO task_link (id, task_id, kind, value, review_round) VALUES (?, ?, 'commit', 'abc', ?)", GenerateID(), tasks[0].ID, reviewRound); err != nil {
		t.Fatalf("insert commit link: %v", err)
	}
	return store, tasks[0].ID
}

func conflictCode(err error) string {
	var conflictErr *ConflictError
	if errors.As(err, &conflictErr) {
		return conflictErr.Code
	}
	return ""
}

func computeManifestDigest(manifestJSON []byte) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Re-encode to canonical form
	var m interface{}
	if err := json.Unmarshal(manifestJSON, &m); err != nil {
		return ""
	}
	if err := enc.Encode(m); err != nil {
		return ""
	}
	canonical := strings.TrimSuffix(buf.String(), "\n")
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func TestBeginLandingIsConditionalOnTheReviewRound(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 2)

	// An approve that prepared round 1 lost the race with a rework and re-approval.
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); conflictCode(err) != "STALE_REVIEW_ROUND" {
		t.Fatalf("expected STALE_REVIEW_ROUND, got %v", err)
	}
	if err := store.BeginLanding(ctx, taskID, 2, "abc", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding round 2: %v", err)
	}
	// A resumed approve re-reserving the same round and commit is a no-op; anything else is refused.
	if err := store.BeginLanding(ctx, taskID, 2, "abc", "attempt-1"); err != nil {
		t.Errorf("re-reserving the same landing: %v", err)
	}
	if err := store.BeginLanding(ctx, taskID, 2, "def", "attempt-1"); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("a different commit: expected LANDING_IN_PROGRESS, got %v", err)
	}

	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.LandingRound == nil || *task.LandingRound != 2 || task.LandingCommit == nil || *task.LandingCommit != "abc" {
		t.Errorf("GetTask landing = %v/%v, want 2/abc", task.LandingRound, task.LandingCommit)
	}
}

func TestBeginLandingRequiresApproved(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'review' WHERE id = ?", taskID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); conflictCode(err) != "NOT_APPROVED" {
		t.Errorf("expected NOT_APPROVED, got %v", err)
	}
	if err := store.BeginLanding(ctx, "no-such-task", 1, "abc", "attempt-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestLandingReservationOnlyAllowsDone(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}

	for _, target := range []string{"ready", "failed", "blocked", "abandoned", "superseded"} {
		if _, err := store.TransitionTask(ctx, taskID, target, nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
			t.Errorf("transition to %s while landing: expected LANDING_IN_PROGRESS, got %v", target, err)
		}
	}
	if _, err := store.SupersedeTask(ctx, taskID, nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("supersede while landing: expected LANDING_IN_PROGRESS, got %v", err)
	}

	// Not even to done by a plain transition: only the owning approve completes the landing.
	if _, err := store.TransitionTask(ctx, taskID, "done", nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("plain transition to done while landing: expected LANDING_IN_PROGRESS, got %v", err)
	}
	if _, err := store.CompleteLanding(ctx, taskID, "someone-else", nil); conflictCode(err) != "LANDING_ATTEMPT_MISMATCH" {
		t.Errorf("completing another attempt's landing: expected LANDING_ATTEMPT_MISMATCH, got %v", err)
	}
	if _, err := store.CompleteLanding(ctx, taskID, "attempt-1", nil); err != nil {
		t.Fatalf("the owning approve completes the landing: %v", err)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.LandingRound != nil || task.LandingCommit != nil {
		t.Errorf("done must clear the reservation, got %v/%v", task.LandingRound, task.LandingCommit)
	}
}

func TestCancelLandingAllowsRejectAgain(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}
	if err := store.CancelLanding(ctx, taskID, "attempt-1"); err != nil {
		t.Fatalf("CancelLanding: %v", err)
	}
	if _, err := store.TransitionTask(ctx, taskID, "ready", nil); err != nil {
		t.Errorf("reject after cancelling the landing: %v", err)
	}
	if err := store.CancelLanding(ctx, "no-such-task", "attempt-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestCancelLandingOnlyCancelsTheOwningAttempt(t *testing.T) {
	ctx := context.Background()
	store, taskID := approvedTaskInRound(t, 1)

	// Approve A reserves, then stalls; approve B resumes the same landing and takes it over.
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-A"); err != nil {
		t.Fatalf("A reserves: %v", err)
	}
	if err := store.BeginLanding(ctx, taskID, 1, "abc", "attempt-B"); err != nil {
		t.Fatalf("B takes over: %v", err)
	}

	// A comes back and tries to give the reservation back: it is B's now.
	if err := store.CancelLanding(ctx, taskID, "attempt-A"); conflictCode(err) != "LANDING_ATTEMPT_MISMATCH" {
		t.Fatalf("stale cancel: expected LANDING_ATTEMPT_MISMATCH, got %v", err)
	}
	if _, err := store.TransitionTask(ctx, taskID, "ready", nil); conflictCode(err) != "LANDING_IN_PROGRESS" {
		t.Errorf("after the stale cancel the task must still be unrejectable, got %v", err)
	}
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.LandingAttempt == nil || *task.LandingAttempt != "attempt-B" {
		t.Errorf("landing attempt = %v, want attempt-B", task.LandingAttempt)
	}

	if err := store.CancelLanding(ctx, taskID, "attempt-B"); err != nil {
		t.Errorf("the owner's cancel: %v", err)
	}
	if err := store.CancelLanding(ctx, taskID, "attempt-A"); err != nil {
		t.Errorf("cancelling an unreserved task is a no-op, got %v", err)
	}
}

func TestMergedResearchParentCreatesContinuationChildren(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create a research task with continuation manifest opt-in
	spec := `Research task spec

## continuation manifest

Some description`
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Parent Task", Spec: spec, DocumentID: docID, Model: "haiku", Track: "research", ReviewModels: []string{"opus", "sonnet"}},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	parentID := tasks[0].ID

	// Create a manifest for this task
	falseVal := false
	trueVal := true
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			{
				Key:                "child1",
				Title:              "Child 1",
				Spec:               "Child 1 spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				AgentMerge:         &falseVal,
				Escalate:           &trueVal,
				ClaimIDs:           []string{"claim1"},
				SourceStartPoints:  []string{"source1"},
				FileScope:          []string{"file1.go"},
				AcceptanceCriteria: []string{"criterion1"},
			},
		},
		PendingCandidates: []manifest.PendingCandidate{
			{ClaimID: "claim1", Disposition: manifest.Assigned},
		},
	}

	// Validate manifest
	if err := m.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Store the manifest in the database by submitting the task
	manifestJSON, _ := json.Marshal(m)
	manifestDigest := computeManifestDigest(manifestJSON)

	// Manually insert the submission manifest since we can't call SubmitTaskWithManifest with correct parameters
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO task_submission_manifest (task_id, review_round, parent_task_id, manifest_json, manifest_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, parentID, 1, parentID, string(manifestJSON), manifestDigest, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert submission manifest: %v", err)
	}

	// Approve the parent task in review round 1
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'approved', review_round = 1 WHERE id = ?", parentID); err != nil {
		t.Fatalf("force approved: %v", err)
	}

	// Transition to done - this should create the child
	_, err = store.TransitionTask(ctx, parentID, "done", nil)
	if err != nil {
		t.Fatalf("TransitionTask to done: %v", err)
	}

	// Verify the parent is done
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask parent: %v", err)
	}
	if parent.State != "done" {
		t.Errorf("parent state = %q, want done", parent.State)
	}

	// Query for tasks with continuation_parent link pointing to this parent
	var childID string
	err = store.Conn().QueryRowContext(ctx, `
		SELECT task_id FROM task_link
		WHERE kind = 'continuation_parent' AND value = ?
		LIMIT 1
	`, parentID).Scan(&childID)
	if err != nil {
		t.Errorf("no child task found with continuation_parent link: %v", err)
		return
	}

	// Verify the child exists and is in ready state
	child, err := store.GetTask(ctx, childID)
	if err != nil {
		t.Fatalf("GetTask child: %v", err)
	}

	if child.State != "ready" {
		t.Errorf("child state = %q, want ready", child.State)
	}
	if child.Title != "Child 1" {
		t.Errorf("child title = %q, want Child 1", child.Title)
	}
	if child.Track != "research" {
		t.Errorf("child track = %q, want research", child.Track)
	}
}

func TestTransitioningApprovedToNotDoneDoesNotCreateChildren(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create a research task with continuation manifest opt-in
	spec := `Research task spec

## continuation manifest

Some description`
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Parent Task", Spec: spec, DocumentID: docID, Model: "haiku", Track: "research", ReviewModels: []string{"opus", "sonnet"}},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	parentID := tasks[0].ID

	// Create a manifest
	falseVal := false
	trueVal := true
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			{
				Key:                "child1",
				Title:              "Child 1",
				Spec:               "Child 1 spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				AgentMerge:         &falseVal,
				Escalate:           &trueVal,
				ClaimIDs:           []string{"claim1"},
				SourceStartPoints:  []string{"source1"},
				FileScope:          []string{"file1.go"},
				AcceptanceCriteria: []string{"criterion1"},
			},
		},
		PendingCandidates: []manifest.PendingCandidate{
			{ClaimID: "claim1", Disposition: manifest.Assigned},
		},
	}

	// Validate manifest
	if err := m.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Store the manifest
	manifestJSON, _ := json.Marshal(m)
	manifestDigest := computeManifestDigest(manifestJSON)
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO task_submission_manifest (task_id, review_round, parent_task_id, manifest_json, manifest_digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, parentID, 1, parentID, string(manifestJSON), manifestDigest, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert submission manifest: %v", err)
	}

	// Approve the parent
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'approved', review_round = 1 WHERE id = ?", parentID); err != nil {
		t.Fatalf("force approved: %v", err)
	}

	// Transition to ready (not done) - should not create children
	_, err = store.TransitionTask(ctx, parentID, "ready", nil)
	if err != nil {
		t.Fatalf("TransitionTask to ready: %v", err)
	}

	// Verify no child was created
	var childCount int
	err = store.Conn().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task_link
		WHERE kind = 'continuation_parent' AND value = ?
	`, parentID).Scan(&childCount)
	if err != nil {
		t.Fatalf("failed to count children: %v", err)
	}

	if childCount != 0 {
		t.Errorf("expected no children, but found %d", childCount)
	}
}
