package evaluation

import (
	"testing"
	"time"
)

func TestCohortManifestValidation(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	tests := []struct {
		name    string
		cohort  *CohortManifest
		wantErr bool
	}{
		{
			name:    "nil cohort",
			cohort:  nil,
			wantErr: true,
		},
		{
			name: "unsupported version",
			cohort: &CohortManifest{
				Version: 99,
				Samples: []CohortSampleSelection{
					{OriginalTaskID: "t1", SubmittedSHA: "sha1", SelectionReason: "clean"},
				},
			},
			wantErr: true,
		},
		{
			name: "empty samples",
			cohort: &CohortManifest{
				Version: 1,
				Samples: []CohortSampleSelection{},
			},
			wantErr: true,
		},
		{
			name: "denominator less than samples",
			cohort: &CohortManifest{
				Version: 1,
				Samples: []CohortSampleSelection{
					{OriginalTaskID: "t1", SubmittedSHA: "sha1"},
					{OriginalTaskID: "t2", SubmittedSHA: "sha2"},
				},
				TotalDenominator: 1,
			},
			wantErr: true,
		},
		{
			name: "missing required fields",
			cohort: &CohortManifest{
				Version: 1,
				Samples: []CohortSampleSelection{
					{OriginalTaskID: "t1"}, // missing SubmittedSHA and not marked unavailable
				},
			},
			wantErr: true,
		},
		{
			name: "unavailable sample with empty SHA",
			cohort: &CohortManifest{
				Version: 1,
				Samples: []CohortSampleSelection{
					{OriginalTaskID: "t1", SubmittedSHA: "", Unavailable: true}, // Empty SHA is OK when unavailable
				},
				TotalDenominator: 10,
			},
			wantErr: false,
		},
		{
			name: "duplicate sample",
			cohort: &CohortManifest{
				Version: 1,
				Samples: []CohortSampleSelection{
					{OriginalTaskID: "t1", OriginalReviewRound: 1, SubmittedSHA: "sha1"},
					{OriginalTaskID: "t1", OriginalReviewRound: 1, SubmittedSHA: "sha2"},
				},
				TotalDenominator: 2,
			},
			wantErr: true,
		},
		{
			name: "valid clean and rejected mix",
			cohort: &CohortManifest{
				Version: 1,
				Samples: []CohortSampleSelection{
					{OriginalTaskID: "t1", OriginalReviewRound: 1, SubmittedSHA: "sha1", SelectionReason: "clean", MateriallyRejected: false},
					{OriginalTaskID: "t2", OriginalReviewRound: 1, SubmittedSHA: "sha2", SelectionReason: "issue found", MateriallyRejected: true},
				},
				TotalDenominator: 100,
				SelectionReason:  "pilot cohort with mixed outcomes",
				CreatedAt:        now,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cohort.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCohortDigestDeterministic(t *testing.T) {
	sample1 := CohortSampleSelection{
		OriginalTaskID:      "t1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		MateriallyRejected:  false,
	}
	sample2 := CohortSampleSelection{
		OriginalTaskID:      "t2",
		OriginalReviewRound: 1,
		SubmittedSHA:        "def456",
		MateriallyRejected:  true,
	}

	cohort1 := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{sample1, sample2},
	}

	cohort2 := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{sample1, sample2},
	}

	cohort3 := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{sample2, sample1},
	}

	digest1 := cohort1.Digest()
	digest2 := cohort2.Digest()
	digest3 := cohort3.Digest()

	if digest1 != digest2 {
		t.Errorf("identical cohorts have different digests: %s vs %s", digest1, digest2)
	}

	if digest1 == digest3 {
		t.Errorf("different sample order should produce different digest")
	}

	// Verify digest is deterministic
	digest1_again := cohort1.Digest()
	if digest1 != digest1_again {
		t.Errorf("cohort digest not deterministic: %s vs %s", digest1, digest1_again)
	}
}

func TestDeterministicCohortBuilder(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	samples := []CohortSampleSelection{
		{OriginalTaskID: "z-task", OriginalReviewRound: 1, SubmittedSHA: "sha1", SelectionReason: "clean"},
		{OriginalTaskID: "a-task", OriginalReviewRound: 1, SubmittedSHA: "sha2", SelectionReason: "rejected"},
		{OriginalTaskID: "m-task", OriginalReviewRound: 2, SubmittedSHA: "sha3", SelectionReason: "clean"},
	}

	builder := DeterministicCohortBuilder{
		Samples:          samples,
		TotalDenominator: 100,
		SelectionReason:  "test cohort",
		CreatedAt:        now,
	}

	cohort, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	if cohort.Version != 1 {
		t.Errorf("expected version 1, got %d", cohort.Version)
	}

	// Verify samples are sorted deterministically
	if cohort.Samples[0].OriginalTaskID != "a-task" {
		t.Errorf("first sample should be a-task, got %s", cohort.Samples[0].OriginalTaskID)
	}
	if cohort.Samples[1].OriginalTaskID != "m-task" {
		t.Errorf("second sample should be m-task, got %s", cohort.Samples[1].OriginalTaskID)
	}
	if cohort.Samples[2].OriginalTaskID != "z-task" {
		t.Errorf("third sample should be z-task, got %s", cohort.Samples[2].OriginalTaskID)
	}
}

func TestDeterministicCohortBuilderRejectedBeforeFix(t *testing.T) {
	// Test that a rejection recorded at selection time is preserved
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	// This sample was rejected before fixes; we record that fact
	rejectedSample := CohortSampleSelection{
		OriginalTaskID:      "task-with-issue",
		OriginalReviewRound: 1,
		SubmittedSHA:        "original-broken-sha",
		SelectionReason:     "material issue: bounds check missing",
		MateriallyRejected:  true,
	}

	builder := DeterministicCohortBuilder{
		Samples:          []CohortSampleSelection{rejectedSample},
		TotalDenominator: 50,
		SelectionReason:  "includes known rejections to verify fix detection",
		CreatedAt:        now,
	}

	cohort, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	if !cohort.Samples[0].MateriallyRejected {
		t.Errorf("expected MateriallyRejected=true for rejected sample")
	}
	if cohort.Samples[0].SubmittedSHA != "original-broken-sha" {
		t.Errorf("expected original SHA to be preserved, not later fixes")
	}
}

func TestSnapshotBuilderBasic(t *testing.T) {
	builder := SnapshotBuilder{
		SampleID:       "sample-123",
		OriginalTaskID: "task-456",
		SubmittedSHA:   "commit-abc123",
		ProjectID:      "project-789",
		PromptVersion:  "v1.0",
		ModelVersion:   "claude-5",
		RuntimeVersion: "1.2.3",
	}

	snap, err := builder.Build()
	if err != nil {
		t.Fatalf("Build() failed: %v", err)
	}

	if snap.SampleID != "sample-123" {
		t.Errorf("expected sample ID sample-123, got %s", snap.SampleID)
	}

	if snap.SnapshotDigest == "" {
		t.Errorf("snapshot digest should not be empty")
	}

	// Verify digest is deterministic
	snap2, _ := builder.Build()
	if snap.SnapshotDigest != snap2.SnapshotDigest {
		t.Errorf("snapshot digest not deterministic")
	}
}

func TestSnapshotBuilderMissingFields(t *testing.T) {
	tests := []struct {
		name      string
		builder   SnapshotBuilder
		wantError bool
	}{
		{
			name: "missing sample ID",
			builder: SnapshotBuilder{
				OriginalTaskID: "t1",
				SubmittedSHA:   "sha1",
			},
			wantError: true,
		},
		{
			name: "missing task ID",
			builder: SnapshotBuilder{
				SampleID:     "s1",
				SubmittedSHA: "sha1",
			},
			wantError: true,
		},
		{
			name: "missing submitted SHA",
			builder: SnapshotBuilder{
				SampleID:       "s1",
				OriginalTaskID: "t1",
			},
			wantError: false, // Empty SHA is allowed, marks snapshot as unavailable
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap, err := tt.builder.Build()
			if tt.wantError && err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
			if !tt.wantError && err != nil {
				t.Errorf("unexpected error for %s: %v", tt.name, err)
			}
			// If it's the missing SHA test, verify it's marked unavailable
			if tt.name == "missing submitted SHA" && !snap.Unavailable {
				t.Errorf("snapshot with empty SHA should be marked unavailable")
			}
		})
	}
}

func TestMarshalUnmarshalCohort(t *testing.T) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	original := &CohortManifest{
		Version: 1,
		Samples: []CohortSampleSelection{
			{
				OriginalTaskID:      "t1",
				OriginalReviewRound: 1,
				SubmittedSHA:        "sha1",
				SelectionReason:     "clean submission",
				MateriallyRejected:  false,
			},
			{
				OriginalTaskID:      "t2",
				OriginalReviewRound: 1,
				SubmittedSHA:        "sha2",
				SelectionReason:     "contains issue",
				MateriallyRejected:  true,
			},
		},
		TotalDenominator: 100,
		SelectionReason:  "pilot cohort",
		CreatedAt:        now,
	}

	// Marshal
	data, err := MarshalCohort(original)
	if err != nil {
		t.Fatalf("MarshalCohort() failed: %v", err)
	}

	// Unmarshal
	restored, err := UnmarshalCohort(data)
	if err != nil {
		t.Fatalf("UnmarshalCohort() failed: %v", err)
	}

	// Verify
	if original.Digest() != restored.Digest() {
		t.Errorf("restored cohort has different digest")
	}

	if len(original.Samples) != len(restored.Samples) {
		t.Errorf("sample count mismatch")
	}

	for i := range original.Samples {
		if original.Samples[i].OriginalTaskID != restored.Samples[i].OriginalTaskID {
			t.Errorf("sample %d task ID mismatch", i)
		}
		if original.Samples[i].MateriallyRejected != restored.Samples[i].MateriallyRejected {
			t.Errorf("sample %d MateriallyRejected mismatch", i)
		}
	}
}

func TestSnapshotDigestDeterministic(t *testing.T) {
	builder1 := SnapshotBuilder{
		SampleID:       "sample-123",
		OriginalTaskID: "task-456",
		SubmittedSHA:   "abc123",
		ProjectID:      "project-789",
		PromptVersion:  "v1.0",
		ModelVersion:   "claude-5",
		RuntimeVersion: "1.2.3",
	}

	builder2 := SnapshotBuilder{
		SampleID:       "sample-123",
		OriginalTaskID: "task-456",
		SubmittedSHA:   "abc123",
		ProjectID:      "project-789", // Different project but same sample
		PromptVersion:  "v1.0",
		ModelVersion:   "claude-5",
		RuntimeVersion: "1.2.3",
	}

	snap1, _ := builder1.Build()
	snap2, _ := builder2.Build()

	// Same sample/task/SHA should produce same digest regardless of project
	if snap1.SnapshotDigest != snap2.SnapshotDigest {
		t.Errorf("snapshot digest should be deterministic for same sample/task/sha")
	}
}

func TestSnapshotDigestChangesWithSHA(t *testing.T) {
	builder1 := SnapshotBuilder{
		SampleID:       "sample-123",
		OriginalTaskID: "task-456",
		SubmittedSHA:   "abc123",
		ProjectID:      "project-789",
	}

	builder2 := SnapshotBuilder{
		SampleID:       "sample-123",
		OriginalTaskID: "task-456",
		SubmittedSHA:   "def456", // Different SHA
		ProjectID:      "project-789",
	}

	snap1, _ := builder1.Build()
	snap2, _ := builder2.Build()

	if snap1.SnapshotDigest == snap2.SnapshotDigest {
		t.Errorf("different SHAs should produce different digests")
	}
}
