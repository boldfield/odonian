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
	"time"

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

// helperCreateResearchTask creates, promotes, and claims a research-track task with continuation opt-in.
func helperCreateResearchTask(t *testing.T, store Store, ctx context.Context, projectID, docID string, title string) string {
	t.Helper()
	spec := `Research task spec

## continuation manifest

Some description`
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: title, Spec: spec, DocumentID: docID, Model: "haiku", Track: "research", ReviewModels: []string{"opus", "sonnet"}},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("PromoteTask: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*60*time.Second); err != nil {
		t.Fatalf("ClaimTask: %v", err)
	}
	return taskID
}

// helperCreateManifest creates a manifest for the given parent ID.
func helperCreateManifest(parentID string) *manifest.Manifest {
	falseVal := false
	trueVal := true
	return &manifest.Manifest{
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
}

// helperSubmitManifest submits a task with a manifest and returns the parent task in review state.
func helperSubmitManifest(t *testing.T, store Store, ctx context.Context, taskID string, m *manifest.Manifest) TaskWithDepsAndLinks {
	t.Helper()
	manifestJSON, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("failed to marshal manifest: %v", err)
	}
	links := []LinkInput{
		{Kind: "pr", Value: "#100"},
		{Kind: "commit", Value: "abc123"},
	}
	if _, err := store.SubmitTaskWithManifest(ctx, taskID, "agent-1", "Implemented", nil, links, 8, nil, nil, testUnlimitedResearchBudget, nil, nil, manifestJSON); err != nil {
		t.Fatalf("SubmitTaskWithManifest: %v", err)
	}
	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("GetTask after submit: %v", err)
	}
	return parent
}

// helperApproveAndTransitionToReady force-approves a task and transitions it to ready.
func helperApproveParent(t *testing.T, store Store, ctx context.Context, taskID string, reviewRound int) {
	t.Helper()
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'approved', review_round = ? WHERE id = ?", reviewRound, taskID); err != nil {
		t.Fatalf("force approved: %v", err)
	}
}

// helperCountChildren counts continuation_parent links for a given parent.
func helperCountChildren(t *testing.T, store Store, ctx context.Context, parentID string) int {
	t.Helper()
	var count int
	err := store.Conn().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task_link
		WHERE kind = 'continuation_parent' AND value = ?
	`, parentID).Scan(&count)
	if err != nil {
		t.Fatalf("failed to count children: %v", err)
	}
	return count
}

func TestMergedResearchParentCreatesContinuationChildren(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	parentID := helperCreateResearchTask(t, store, ctx, projectID, docID, "Parent Task")
	m := helperCreateManifest(parentID)
	if err := m.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}

	parent := helperSubmitManifest(t, store, ctx, parentID, m)
	reviewRound := parent.ReviewRound

	helperApproveParent(t, store, ctx, parentID, reviewRound)

	// Use BeginLanding/CompleteLanding to properly transition to done
	if err := store.BeginLanding(ctx, parentID, reviewRound, "abc123", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}

	if _, err := store.CompleteLanding(ctx, parentID, "attempt-1", nil); err != nil {
		t.Fatalf("CompleteLanding: %v", err)
	}

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask after transition: %v", err)
	}
	if parent.State != "done" {
		t.Errorf("parent state = %q, want done", parent.State)
	}

	childCount := helperCountChildren(t, store, ctx, parentID)
	if childCount != 1 {
		t.Errorf("expected 1 child, got %d", childCount)
	}
}

func TestTransitioningApprovedToNotDoneDoesNotCreateChildren(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	parentID := helperCreateResearchTask(t, store, ctx, projectID, docID, "Parent Task")
	m := helperCreateManifest(parentID)
	if err := m.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}

	parent := helperSubmitManifest(t, store, ctx, parentID, m)
	helperApproveParent(t, store, ctx, parentID, parent.ReviewRound)

	// Reject to ready instead of completing the landing
	if _, err := store.TransitionTask(ctx, parentID, "ready", nil); err != nil {
		t.Fatalf("TransitionTask to ready: %v", err)
	}

	// Verify no children were created
	childCount := helperCountChildren(t, store, ctx, parentID)
	if childCount != 0 {
		t.Errorf("expected no children, got %d", childCount)
	}
}

func TestDigestMismatchPreventsTransition(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	parentID := helperCreateResearchTask(t, store, ctx, projectID, docID, "Parent Task")
	m := helperCreateManifest(parentID)
	if err := m.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}

	parent := helperSubmitManifest(t, store, ctx, parentID, m)
	reviewRound := parent.ReviewRound
	helperApproveParent(t, store, ctx, parentID, reviewRound)

	// Corrupt the manifest digest in the database
	if _, err := store.Conn().ExecContext(ctx, `
		UPDATE task_submission_manifest SET manifest_digest = 'corrupted-digest'
		WHERE task_id = ? AND review_round = ?
	`, parentID, reviewRound); err != nil {
		t.Fatalf("corrupt digest: %v", err)
	}

	// BeginLanding should succeed
	if err := store.BeginLanding(ctx, parentID, reviewRound, "abc123", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}

	// CompleteLanding should fail due to digest mismatch in the child materialization
	_, err := store.CompleteLanding(ctx, parentID, "attempt-1", nil)
	if err == nil {
		t.Errorf("expected error due to digest mismatch, but CompleteLanding succeeded")
		return
	}
	if !strings.Contains(err.Error(), "digest mismatch") {
		t.Errorf("expected 'digest mismatch' in error, got: %v", err)
	}

	// Parent should still be in approved state (transaction rolled back)
	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if parent.State != "approved" {
		t.Errorf("parent state = %q, want approved (should be recoverable)", parent.State)
	}

	// No children should have been created
	childCount := helperCountChildren(t, store, ctx, parentID)
	if childCount != 0 {
		t.Errorf("expected no children after failed transition, got %d", childCount)
	}
}

func TestRetryAndIdempotency(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	parentID := helperCreateResearchTask(t, store, ctx, projectID, docID, "Parent Task")
	m := helperCreateManifest(parentID)
	if err := m.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}

	parent := helperSubmitManifest(t, store, ctx, parentID, m)
	reviewRound := parent.ReviewRound
	helperApproveParent(t, store, ctx, parentID, reviewRound)

	// First landing attempt
	if err := store.BeginLanding(ctx, parentID, reviewRound, "abc123", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}

	if _, err := store.CompleteLanding(ctx, parentID, "attempt-1", nil); err != nil {
		t.Fatalf("first CompleteLanding: %v", err)
	}

	childCount := helperCountChildren(t, store, ctx, parentID)
	if childCount != 1 {
		t.Fatalf("after first landing, expected 1 child, got %d", childCount)
	}

	// Verify the task is now done
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if parent.State != "done" {
		t.Fatalf("parent should be done, got %q", parent.State)
	}

	// Second idempotent attempt - task is already done, should not create duplicates
	childCount = helperCountChildren(t, store, ctx, parentID)
	if childCount != 1 {
		t.Errorf("after idempotent check, expected 1 child, got %d", childCount)
	}
}

func TestRoundBindingCreatesOnlyApprovedRoundManifest(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	parentID := helperCreateResearchTask(t, store, ctx, projectID, docID, "Parent Task")

	// Submit manifest for round 1
	m1 := helperCreateManifest(parentID)
	if err := m1.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation round 1: %v", err)
	}
	parent1 := helperSubmitManifest(t, store, ctx, parentID, m1)
	round1 := parent1.ReviewRound

	// Force-approve for round 1 and then transition back to in_progress
	// to simulate rejection and resubmission for round 2
	helperApproveParent(t, store, ctx, parentID, round1)
	if _, err := store.TransitionTask(ctx, parentID, "ready", nil); err != nil {
		t.Fatalf("transition to ready: %v", err)
	}

	// Manually move to in_progress to allow re-submission
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'in_progress', assignee = 'agent-1' WHERE id = ?", parentID); err != nil {
		t.Fatalf("force in_progress: %v", err)
	}

	// Re-submit with a new manifest for round 2
	m2 := helperCreateManifest(parentID)
	if err := m2.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation round 2: %v", err)
	}
	parent2 := helperSubmitManifest(t, store, ctx, parentID, m2)
	round2 := parent2.ReviewRound

	if round2 <= round1 {
		t.Fatalf("round 2 = %d should be > round 1 = %d", round2, round1)
	}

	// Approve for round 2
	helperApproveParent(t, store, ctx, parentID, round2)

	// Complete landing for round 2 - should use round 2 manifest only
	if err := store.BeginLanding(ctx, parentID, round2, "abc123", "attempt-2"); err != nil {
		t.Fatalf("BeginLanding round 2: %v", err)
	}

	if _, err := store.CompleteLanding(ctx, parentID, "attempt-2", nil); err != nil {
		t.Fatalf("CompleteLanding round 2: %v", err)
	}

	// Verify children were created (from round 2 manifest only)
	childCount := helperCountChildren(t, store, ctx, parentID)
	if childCount != 1 {
		t.Errorf("expected 1 child from round 2 manifest, got %d", childCount)
	}
}

func TestNonOptedInResearchTaskDoesNotCreateChildren(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create a research task WITHOUT continuation opt-in
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Non-opted Parent", Spec: "Just a research task", DocumentID: docID, Model: "haiku", Track: "research", ReviewModels: []string{"opus", "sonnet"}},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	parentID := tasks[0].ID

	// Try to submit with manifest (should be rejected because spec doesn't opt in)
	m := helperCreateManifest(parentID)
	manifestJSON, _ := json.Marshal(m)
	_, err = store.SubmitTaskWithManifest(ctx, parentID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget, nil, nil, manifestJSON)
	if err == nil {
		t.Fatalf("SubmitTaskWithManifest should have failed for non-opted-in task")
	}
	// The task should remain in backlog after the failed submission attempt
}

func TestBuildTrackDoesNotCreateChildren(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	// Create a build-track task with continuation opt-in spec (but it's build, not research)
	spec := `Build task

## continuation manifest

Children spec`
	tasks, err := store.CreateTasks(ctx, projectID, []TaskInput{
		{Title: "Build Task", Spec: spec, DocumentID: docID, Model: "haiku", Track: "build", ReviewModels: []string{"opus", "sonnet"}},
	})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	parentID := tasks[0].ID

	// Try to submit with manifest (should be rejected because it's build track)
	m := helperCreateManifest(parentID)
	manifestJSON, _ := json.Marshal(m)
	_, err = store.SubmitTaskWithManifest(ctx, parentID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget, nil, nil, manifestJSON)
	if err == nil {
		t.Fatalf("SubmitTaskWithManifest should have failed for build-track task")
	}
}

func TestCompleteLandingWithApprovedParentCreatesChildren(t *testing.T) {
	ctx := context.Background()
	store, projectID, docID := newBranchTestStore(t)

	parentID := helperCreateResearchTask(t, store, ctx, projectID, docID, "Parent Task")
	m := helperCreateManifest(parentID)
	if err := m.Validate(map[string]bool{"haiku": true, "opus": true, "sonnet": true}, validTracks); err != nil {
		t.Fatalf("manifest validation: %v", err)
	}

	parent := helperSubmitManifest(t, store, ctx, parentID, m)
	helperApproveParent(t, store, ctx, parentID, parent.ReviewRound)

	// Begin landing - must use the same commit that was submitted (abc123)
	if err := store.BeginLanding(ctx, parentID, parent.ReviewRound, "abc123", "attempt-1"); err != nil {
		t.Fatalf("BeginLanding: %v", err)
	}

	// Complete landing - this should create children
	if _, err := store.CompleteLanding(ctx, parentID, "attempt-1", nil); err != nil {
		t.Fatalf("CompleteLanding: %v", err)
	}

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask after completion: %v", err)
	}
	if parent.State != "done" {
		t.Errorf("parent state = %q, want done", parent.State)
	}

	childCount := helperCountChildren(t, store, ctx, parentID)
	if childCount != 1 {
		t.Errorf("expected 1 child after landing completion, got %d", childCount)
	}
}
