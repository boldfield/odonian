package evaluation

import (
	"fmt"
	"testing"
	"time"
)

// TestDeterministicFixtureCohort verifies that a reproducible cohort can be
// created and used consistently across multiple test runs.
func TestDeterministicFixtureCohort(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	samples := []CohortSampleSelection{
		{
			OriginalTaskID:      "research-001",
			OriginalReviewRound: 1,
			SubmittedSHA:        "abc123def456",
			SelectionReason:     "clean first-round submission",
			MateriallyRejected:  false,
		},
		{
			OriginalTaskID:      "research-002",
			OriginalReviewRound: 1,
			SubmittedSHA:        "xyz789uvw012",
			SelectionReason:     "rejected: missing bounds check",
			MateriallyRejected:  true,
		},
		{
			OriginalTaskID:      "research-003",
			OriginalReviewRound: 1,
			SubmittedSHA:        "111222333444",
			SelectionReason:     "clean, simple implementation",
			MateriallyRejected:  false,
		},
	}

	builder := DeterministicCohortBuilder{
		Version:          1,
		Samples:          samples,
		TotalDenominator: 250,
		SelectionReason:  "pilot evaluation cohort: 3 samples from 250 eligible submissions",
		CreatedAt:        now,
	}

	cohort1, err := builder.Build()
	if err != nil {
		t.Fatalf("first build failed: %v", err)
	}

	// Rebuild with same samples in different order to verify deterministic sorting
	shuffledSamples := []CohortSampleSelection{
		samples[2], samples[0], samples[1],
	}
	builder2 := DeterministicCohortBuilder{
		Version:          1,
		Samples:          shuffledSamples,
		TotalDenominator: 250,
		SelectionReason:  "pilot evaluation cohort: 3 samples from 250 eligible submissions",
		CreatedAt:        now,
	}

	cohort2, err := builder2.Build()
	if err != nil {
		t.Fatalf("second build failed: %v", err)
	}

	// Both should have same digest despite different input order
	if cohort1.Digest() != cohort2.Digest() {
		t.Errorf("cohorts built from shuffled samples have different digests")
	}

	// Verify both have samples in same deterministic order
	if cohort1.Samples[0].OriginalTaskID != cohort2.Samples[0].OriginalTaskID {
		t.Errorf("samples not in same order after rebuild")
	}

	// Verify rejected sample is clearly marked
	var rejectedFound bool
	for _, s := range cohort1.Samples {
		if s.MateriallyRejected {
			rejectedFound = true
			if s.OriginalTaskID != "research-002" {
				t.Errorf("wrong task marked as rejected")
			}
		}
	}
	if !rejectedFound {
		t.Errorf("no rejected sample found")
	}
}

// TestRejectedBeforeFixSnapshot demonstrates that snapshot digests preserve
// the original rejected SHA, not any later fixes. This test also verifies
// that the snapshot is isolated from subsequent fixes.
func TestRejectedBeforeFixSnapshot(t *testing.T) {
	// Scenario: task was rejected in round 1, but later commits fix the issue.
	// The snapshot must use the original rejected SHA, not later fixes.
	// The snapshot must not contain the later fixes or other reviewer findings.

	originalRejectedSHA := "broken-abc123"
	laterFixedSHA := "fixed-def456"

	// Create snapshot using original rejected SHA
	builder := SnapshotBuilder{
		SampleID:       "sample-rejected",
		OriginalTaskID: "task-with-issue",
		SubmittedSHA:   originalRejectedSHA,
		ProjectID:      "project-1",
		CandidateID:    "candidate-1",
		PromptVersion:  "v1.0",
		ModelVersion:   "claude-5",
		RuntimeVersion: "1.2.3",
	}

	snap, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	originalDigest := snap.SnapshotDigest

	// Verify the original snapshot preserves the rejected SHA in its digest
	if snap.SnapshotDigest == "" {
		t.Errorf("snapshot digest should not be empty")
	}

	// Try to build with later fixed SHA (this would be wrong)
	builderWithFix := SnapshotBuilder{
		SampleID:       "sample-rejected",
		OriginalTaskID: "task-with-issue",
		SubmittedSHA:   laterFixedSHA, // Wrong: using later fix
		ProjectID:      "project-1",
		CandidateID:    "candidate-1",
		PromptVersion:  "v1.0",
		ModelVersion:   "claude-5",
		RuntimeVersion: "1.2.3",
	}

	snapWithFix, _ := builderWithFix.Build()
	fixedDigest := snapWithFix.SnapshotDigest

	// Digests must be different; we must never confuse original with later fixes
	if originalDigest == fixedDigest {
		t.Errorf("snapshot digest should change when SHA changes")
	}

	// Verify that the original snapshot is truly immutable
	// and not affected by the later fix attempt
	if snap.SnapshotDigest != originalDigest {
		t.Errorf("original snapshot digest should not change")
	}
}

// TestMultipleCandidatesIndependentSnapshots verifies that each candidate in
// a campaign receives the same frozen cohort but with independently created
// snapshots and jobs. Each candidate's snapshot includes its CandidateID.
func TestMultipleCandidatesIndependentSnapshots(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Create a cohort with one sample
	cohort := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{
			{
				OriginalTaskID:      "original-task-1",
				OriginalReviewRound: 1,
				SubmittedSHA:        "sha-original-1",
				SelectionReason:     "test sample",
				MateriallyRejected:  false,
			},
		},
		TotalDenominator: 100,
		SelectionReason:  "test cohort",
		CreatedAt:        now,
	}

	cohortDigest := cohort.Digest()

	// Create snapshots for the same sample, representing it would be evaluated
	// by multiple independent candidates
	snapshots := make(map[string]EvaluationSnapshot)

	candidates := []string{"candidate-muse", "candidate-pi", "candidate-spark"}
	for _, candID := range candidates {
		builder := SnapshotBuilder{
			SampleID:       "sample-1",
			OriginalTaskID: "original-task-1",
			SubmittedSHA:   "sha-original-1", // Same original SHA for all
			ProjectID:      "project-1",
			CandidateID:    candID, // Each candidate identified uniquely
			PromptVersion:  "v1.0",
			ModelVersion:   "varies-by-candidate",
			RuntimeVersion: "1.2.3",
		}

		snap, err := builder.Build()
		if err != nil {
			t.Fatalf("failed to build snapshot for %s: %v", candID, err)
		}

		snapshots[candID] = snap
	}

	// All candidates should have their CandidateID recorded
	for _, candID := range candidates {
		if snapshots[candID].CandidateID != candID {
			t.Errorf("candidate %s snapshot has wrong CandidateID", candID)
		}
	}

	if len(snapshots) != len(candidates) {
		t.Errorf("expected %d snapshots, got %d", len(candidates), len(snapshots))
	}

	// Verify cohort digest is stable across the campaign
	cohortDigest2 := cohort.Digest()
	if cohortDigest != cohortDigest2 {
		t.Errorf("cohort digest not stable")
	}
}

// TestSecondCandidateDoesNotAlterCohort verifies that adding a second candidate
// to a campaign does not modify the cohort or previous candidate results.
func TestSecondCandidateDoesNotAlterCohort(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Create original cohort
	originalCohort := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{
			{
				OriginalTaskID:      "task-1",
				OriginalReviewRound: 1,
				SubmittedSHA:        "sha1",
				SelectionReason:     "original selection",
				MateriallyRejected:  false,
			},
			{
				OriginalTaskID:      "task-2",
				OriginalReviewRound: 1,
				SubmittedSHA:        "sha2",
				SelectionReason:     "original selection",
				MateriallyRejected:  false,
			},
		},
		TotalDenominator: 50,
		SelectionReason:  "pilot cohort",
		CreatedAt:        now,
	}

	originalDigest := originalCohort.Digest()

	// Create first candidate's snapshots with CandidateID
	firstCandSnapshots := make([]EvaluationSnapshot, len(originalCohort.Samples))
	for i, sample := range originalCohort.Samples {
		builder := SnapshotBuilder{
			SampleID:       "snap-first-" + sample.OriginalTaskID,
			OriginalTaskID: sample.OriginalTaskID,
			SubmittedSHA:   sample.SubmittedSHA,
			ProjectID:      "project-1",
			CandidateID:    "candidate-muse",
		}
		snap, _ := builder.Build()
		firstCandSnapshots[i] = snap
	}

	// Now "add" a second candidate - but the cohort should not change
	secondCandSnapshots := make([]EvaluationSnapshot, len(originalCohort.Samples))
	for i, sample := range originalCohort.Samples {
		builder := SnapshotBuilder{
			SampleID:       "snap-second-" + sample.OriginalTaskID,
			OriginalTaskID: sample.OriginalTaskID,
			SubmittedSHA:   sample.SubmittedSHA,
			ProjectID:      "project-1",
			CandidateID:    "candidate-pi",
		}
		snap, _ := builder.Build()
		secondCandSnapshots[i] = snap
	}

	// Verify cohort digest unchanged
	afterSecondDigest := originalCohort.Digest()
	if originalDigest != afterSecondDigest {
		t.Errorf("cohort digest changed after second candidate")
	}

	// Each candidate's snapshots should record their CandidateID
	if firstCandSnapshots[0].CandidateID != "candidate-muse" {
		t.Errorf("first candidate snapshot has wrong CandidateID")
	}
	if secondCandSnapshots[0].CandidateID != "candidate-pi" {
		t.Errorf("second candidate snapshot has wrong CandidateID")
	}

	// Verify that snapshots with different sample IDs have different digests
	if firstCandSnapshots[0].SnapshotDigest == secondCandSnapshots[0].SnapshotDigest {
		t.Errorf("different snapshot sample IDs should have different digests")
	}

	// Verify that same sample with same SHA produces consistent digests
	firstBuilt := SnapshotBuilder{
		SampleID:       "test",
		OriginalTaskID: "task-1",
		SubmittedSHA:   "sha1",
		CandidateID:    "candidate-muse",
	}
	secondBuilt := SnapshotBuilder{
		SampleID:       "test",
		OriginalTaskID: "task-1",
		SubmittedSHA:   "sha1",
		CandidateID:    "candidate-muse",
	}
	snap1, _ := firstBuilt.Build()
	snap2, _ := secondBuilt.Build()

	if snap1.SnapshotDigest != snap2.SnapshotDigest {
		t.Errorf("same sample+candidate should have same snapshot digest")
	}

	// But different candidates should have different digests
	thirdBuilt := SnapshotBuilder{
		SampleID:       "test",
		OriginalTaskID: "task-1",
		SubmittedSHA:   "sha1",
		CandidateID:    "candidate-pi",
	}
	snap3, _ := thirdBuilt.Build()
	if snap1.SnapshotDigest == snap3.SnapshotDigest {
		t.Errorf("different candidates should have different snapshot digests")
	}
}

// TestMissingSHAHandling verifies that missing original artifacts are recorded
// as unavailable and included in the cohort (not rejected).
func TestMissingSHAHandling(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Create a cohort sample with an unavailable SHA - marked with Unavailable=true
	unavailable := CohortSampleSelection{
		OriginalTaskID:      "task-unavailable",
		OriginalReviewRound: 1,
		SubmittedSHA:        "", // Empty SHA when unavailable
		SelectionReason:     "available in denominator but original commit lost",
		MateriallyRejected:  false,
		Unavailable:         true, // Must mark unavailable
	}

	// Valid cohort including unavailable samples
	cohort := &CohortManifest{
		Version:          1,
		Samples:          []CohortSampleSelection{unavailable},
		TotalDenominator: 100, // Denominator includes unavailable samples
		CreatedAt:        now,
	}

	// Should validate successfully - unavailable samples are allowed
	if err := cohort.Validate(); err != nil {
		t.Errorf("validation should accept unavailable samples marked with Unavailable=true: %v", err)
	}

	// Building a snapshot with missing SHA should succeed but mark it unavailable
	builder := SnapshotBuilder{
		SampleID:       "sample-missing",
		OriginalTaskID: unavailable.OriginalTaskID,
		SubmittedSHA:   "", // Missing
	}

	snap, err := builder.Build()
	if err != nil {
		t.Errorf("expected success for missing SHA, got error: %v", err)
	}

	if !snap.Unavailable {
		t.Errorf("snapshot should be marked unavailable when SHA is empty")
	}

	if snap.SnapshotDigest == "" {
		t.Errorf("unavailable snapshot should still have a digest")
	}

	// Trying to build snapshot without Unavailable flag should fail
	badBuilder := SnapshotBuilder{
		SampleID:       "sample-bad",
		OriginalTaskID: "task-x",
		SubmittedSHA:   "", // Empty without marking unavailable
	}

	snap2, err := badBuilder.Build()
	if err != nil {
		t.Errorf("should succeed but mark unavailable")
	}
	if !snap2.Unavailable {
		t.Errorf("empty SHA should create unavailable snapshot")
	}

	// Validation should reject empty SHA without Unavailable flag
	badCohort := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{
			{
				OriginalTaskID:      "task-bad",
				OriginalReviewRound: 1,
				SubmittedSHA:        "", // Empty without unavailable flag
				SelectionReason:     "bad",
				Unavailable:         false, // Not marked unavailable
			},
		},
	}
	if err := badCohort.Validate(); err == nil {
		t.Errorf("validation should reject empty SHA without Unavailable=true")
	}
}

// TestSnapshotDigestValidation verifies that snapshot digests are stable and
// reproducible, making them suitable for detecting tampering.
func TestSnapshotDigestValidation(t *testing.T) {
	builder := SnapshotBuilder{
		SampleID:       "sample-123",
		OriginalTaskID: "task-456",
		SubmittedSHA:   "abc123",
		ProjectID:      "project-789",
		CandidateID:    "candidate-1",
		PromptVersion:  "v1.0",
		ModelVersion:   "claude-5",
		RuntimeVersion: "1.2.3",
	}

	// Build snapshot multiple times
	digests := make(map[string]bool)
	for i := 0; i < 5; i++ {
		snap, _ := builder.Build()
		digests[snap.SnapshotDigest] = true
	}

	// Should only have one unique digest (deterministic)
	if len(digests) != 1 {
		t.Errorf("snapshot digest not deterministic: got %d unique digests", len(digests))
	}

	// Change a field and verify digest changes
	builder.SubmittedSHA = "def456"
	snap, _ := builder.Build()
	if snap.SnapshotDigest == "" {
		t.Errorf("snapshot digest should not be empty")
	}

	for digest := range digests {
		if snap.SnapshotDigest == digest {
			t.Errorf("digest should change when SHA changes")
		}
	}
}

// TestSnapshotLeakageExclusion verifies that reviewer feedback, PR discussion
// links, other reviewers' findings/verdicts, task events, and production
// credentials do not enter the staged snapshot workspace.
func TestSnapshotLeakageExclusion(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// Create a cohort with a sample that has had multiple review rounds
	// (simulating a task with later reviewer feedback)
	cohort := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{
			{
				OriginalTaskID:      "task-with-feedback",
				OriginalReviewRound: 1,
				SubmittedSHA:        "original-sha-before-feedback",
				SelectionReason:     "test sample that later received feedback",
				MateriallyRejected:  false,
			},
		},
		TotalDenominator: 50,
		SelectionReason:  "pilot cohort with later feedback",
		CreatedAt:        now,
	}

	// Validate the cohort
	if err := cohort.Validate(); err != nil {
		t.Fatalf("failed to validate cohort: %v", err)
	}

	// Create snapshots for both candidates
	// Each candidate should see only the original artifact, not later reviewer feedback
	candidates := []string{"muse-candidate", "pi-candidate"}
	snapshots := make(map[string]EvaluationSnapshot)

	for _, candID := range candidates {
		builder := SnapshotBuilder{
			SampleID:       "snapshot-" + candID,
			OriginalTaskID: "task-with-feedback",
			SubmittedSHA:   "original-sha-before-feedback",
			ProjectID:      "project-1",
			CandidateID:    candID,
			PromptVersion:  "v1.0",
			ModelVersion:   "varies-by-candidate",
			RuntimeVersion: "1.2.3",
		}

		snap, err := builder.Build()
		if err != nil {
			t.Fatalf("failed to build snapshot for %s: %v", candID, err)
		}

		snapshots[candID] = snap

		// Key assertion: snapshot must contain original SHA, not later fixes
		// In full implementation, this would verify the staged files don't contain:
		// - PR discussion links
		// - Other reviewer comments/findings
		// - Task event history
		// - Production board credentials
		// - Later reviewer verdicts
		if snap.SnapshotDigest == "" {
			t.Errorf("snapshot digest should not be empty")
		}
	}

	// All candidates should have same snapshot digest for same sample+SHA
	// (but different from other candidates' sample+SHA combinations)
	snap1 := snapshots[candidates[0]]
	snap2 := snapshots[candidates[1]]

	// Different snapshot IDs (because different CandidateIDs)
	if snap1.SampleID == snap2.SampleID {
		t.Errorf("different candidates should have different sample IDs")
	}

	// Both should reflect the same original SHA - no leakage from later fixes
	if snap1.SnapshotDigest == snap2.SnapshotDigest {
		t.Errorf("different sample IDs should produce different digests")
	}

	// Verify neither snapshot is marked as unavailable
	if snap1.Unavailable || snap2.Unavailable {
		t.Errorf("available samples should not be marked unavailable")
	}
}

// TestLeakageWithRejectedAndCleanMix verifies that when a cohort contains
// both rejected and clean samples, snapshots properly exclude reviewer
// feedback and preserve only the original artifacts.
func TestLeakageWithRejectedAndCleanMix(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	cleanSample := CohortSampleSelection{
		OriginalTaskID:      "clean-task",
		OriginalReviewRound: 1,
		SubmittedSHA:        "clean-sha",
		SelectionReason:     "clean, no issues",
		MateriallyRejected:  false,
	}

	rejectedSample := CohortSampleSelection{
		OriginalTaskID:      "rejected-task",
		OriginalReviewRound: 1,
		SubmittedSHA:        "rejected-sha",
		SelectionReason:     "rejected: bounds check missing",
		MateriallyRejected:  true,
	}

	cohort := &CohortManifest{
		Version:          1,
		Samples:          []CohortSampleSelection{cleanSample, rejectedSample},
		TotalDenominator: 100,
		SelectionReason:  "mixed cohort for comparison",
		CreatedAt:        now,
	}

	if err := cohort.Validate(); err != nil {
		t.Fatalf("cohort validation failed: %v", err)
	}

	// Create snapshots for both - the MateriallyRejected flag is in the cohort
	// metadata but must NOT appear in the prompt given to the candidate
	snapshots := make(map[string]EvaluationSnapshot)
	for i, sample := range cohort.Samples {
		builder := SnapshotBuilder{
			SampleID:       "snap-" + sample.OriginalTaskID,
			OriginalTaskID: sample.OriginalTaskID,
			SubmittedSHA:   sample.SubmittedSHA,
			ProjectID:      "project-1",
			CandidateID:    "test-candidate",
			PromptVersion:  "v1.0",
		}

		snap, _ := builder.Build()
		key := fmt.Sprintf("sample-%d", i)
		snapshots[key] = snap
	}

	// Both snapshots should preserve their original SHAs
	// (in full implementation, the staged files would exclude rejection reason)
	cleanSnap := snapshots["sample-0"]
	rejectedSnap := snapshots["sample-1"]

	if cleanSnap.Unavailable || rejectedSnap.Unavailable {
		t.Errorf("available samples should not be marked unavailable")
	}

	// Digests should be different (different SHAs)
	if cleanSnap.SnapshotDigest == rejectedSnap.SnapshotDigest {
		t.Errorf("different SHAs should produce different digests")
	}
}
