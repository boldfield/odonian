package evaluation

import (
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
// the original rejected SHA, not any later fixes.
func TestRejectedBeforeFixSnapshot(t *testing.T) {
	// Scenario: task was rejected in round 1, but later commits fix the issue.
	// The snapshot must use the original rejected SHA, not later fixes.

	originalRejectedSHA := "broken-abc123"
	laterFixedSHA := "fixed-def456"

	// Create snapshot using original rejected SHA
	builder := SnapshotBuilder{
		SampleID:       "sample-rejected",
		OriginalTaskID: "task-with-issue",
		SubmittedSHA:   originalRejectedSHA,
		ProjectID:      "project-1",
		PromptVersion:  "v1.0",
		ModelVersion:   "claude-5",
		RuntimeVersion: "1.2.3",
	}

	snap, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	originalDigest := snap.SnapshotDigest

	// Try to build with later fixed SHA (this would be wrong)
	builderWithFix := SnapshotBuilder{
		SampleID:       "sample-rejected",
		OriginalTaskID: "task-with-issue",
		SubmittedSHA:   laterFixedSHA, // Wrong: using later fix
		ProjectID:      "project-1",
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

	// The original rejected snapshot should preserve the broken SHA
	if snap.SnapshotDigest == "" {
		t.Errorf("snapshot digest should not be empty")
	}
}

// TestMultipleCandidatesIndependentSnapshots verifies that each candidate in
// a campaign receives the same frozen cohort but with independently created
// snapshots and jobs.
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

	// All candidates should see the same snapshot digest for the same sample
	firstSnap := snapshots[candidates[0]]
	for _, candID := range candidates[1:] {
		if snapshots[candID].SnapshotDigest != firstSnap.SnapshotDigest {
			t.Errorf("candidate %s has different snapshot digest", candID)
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

	// Create first candidate's snapshots
	firstCandSnapshots := make([]EvaluationSnapshot, len(originalCohort.Samples))
	for i, sample := range originalCohort.Samples {
		builder := SnapshotBuilder{
			SampleID:       "snap-first-" + sample.OriginalTaskID,
			OriginalTaskID: sample.OriginalTaskID,
			SubmittedSHA:   sample.SubmittedSHA,
			ProjectID:      "project-1",
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
		}
		snap, _ := builder.Build()
		secondCandSnapshots[i] = snap
	}

	// Verify cohort digest unchanged
	afterSecondDigest := originalCohort.Digest()
	if originalDigest != afterSecondDigest {
		t.Errorf("cohort digest changed after second candidate")
	}

	// Verify that first candidate's snapshots are independent
	if firstCandSnapshots[0].SnapshotDigest == secondCandSnapshots[0].SnapshotDigest {
		t.Errorf("different snapshot builders should have different snapshot IDs")
	}

	// But all candidates see same snapshot digest for same sample
	// because snapshot digest is based on sample/task/sha, not candidate
	firstBuilt := SnapshotBuilder{
		SampleID:       "test",
		OriginalTaskID: "task-1",
		SubmittedSHA:   "sha1",
	}
	secondBuilt := SnapshotBuilder{
		SampleID:       "test",
		OriginalTaskID: "task-1",
		SubmittedSHA:   "sha1",
	}
	snap1, _ := firstBuilt.Build()
	snap2, _ := secondBuilt.Build()

	if snap1.SnapshotDigest != snap2.SnapshotDigest {
		t.Errorf("same sample should have same snapshot digest")
	}
}

// TestMissingSHAHandling verifies that missing original artifacts are recorded
// gracefully and not silently skipped or fabricated.
func TestMissingSHAHandling(t *testing.T) {
	// Create a cohort sample with an unavailable SHA
	unavailable := CohortSampleSelection{
		OriginalTaskID:      "task-unavailable",
		OriginalReviewRound: 1,
		SubmittedSHA:        "", // Empty SHA means unavailable
		SelectionReason:     "available in denominator but original commit lost",
		MateriallyRejected:  false,
	}

	// Attempting to build a snapshot with missing SHA should fail
	builder := SnapshotBuilder{
		SampleID:       "sample-missing",
		OriginalTaskID: unavailable.OriginalTaskID,
		SubmittedSHA:   "", // Missing
	}

	_, err := builder.Build()
	if err == nil {
		t.Errorf("expected error for missing SHA, got nil")
	}

	// Validation should catch cohorts with empty SHAs
	cohort := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{unavailable},
	}

	if err := cohort.Validate(); err == nil {
		t.Errorf("validation should reject samples with empty SHA")
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
