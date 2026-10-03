package store

import (
	"context"
	"testing"
)

func TestRecordAndGetFindingDisposition(t *testing.T) {
	store := newEvaluationStore(t)
	ctx := context.Background()

	// Create a campaign
	campaign, err := store.CreateEvaluationCampaign(ctx, EvaluationCampaign{
		ID:                GenerateID(),
		Name:              "test campaign",
		AllowedProjectIDs: []string{"proj1"},
		AllowedModelIDs:   []string{"model1"},
		CohortManifest:    "{}",
		AttemptCap:        5,
	})
	if err != nil {
		t.Fatalf("CreateEvaluationCampaign: %v", err)
	}

	// Create a sample
	sample, err := store.CreateEvaluationSample(ctx, EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		ProjectID:           "proj1",
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	})
	if err != nil {
		t.Fatalf("CreateEvaluationSample: %v", err)
	}

	// Create a candidate
	candidate := newTestCandidate(campaign.ID)
	candidate, err = store.CreateEvaluationCandidate(ctx, candidate)
	if err != nil {
		t.Fatalf("CreateEvaluationCandidate: %v", err)
	}

	disposition := FindingDisposition{
		ID:          "disp-1",
		CampaignID:  campaign.ID,
		SampleID:    sample.ID,
		CandidateID: candidate.ID,
		FindingID:   "finding-1",
		Disposition: "valid",
		Evidence:    "verified against source",
		DecidedBy:   "operator@example.com",
	}

	recorded, err := store.RecordFindingDisposition(ctx, disposition)
	if err != nil {
		t.Fatalf("RecordFindingDisposition failed: %v", err)
	}
	if recorded.ID != disposition.ID {
		t.Errorf("recorded ID mismatch: got %q, want %q", recorded.ID, disposition.ID)
	}
	if recorded.DecidedAt == "" {
		t.Errorf("DecidedAt should be set by the store")
	}

	retrieved, err := store.GetFindingDisposition(ctx, campaign.ID, sample.ID, candidate.ID, "finding-1")
	if err != nil {
		t.Fatalf("GetFindingDisposition failed: %v", err)
	}
	if retrieved.Disposition != "valid" {
		t.Errorf("disposition mismatch: got %q, want %q", retrieved.Disposition, "valid")
	}
}

func TestRecordFindingDispositionErrors(t *testing.T) {
	store := newEvaluationStore(t)
	ctx := context.Background()

	tests := []struct {
		name        string
		disposition FindingDisposition
		wantErr     bool
	}{
		{
			name: "missing ID",
			disposition: FindingDisposition{
				CampaignID:  "camp",
				SampleID:    "samp",
				CandidateID: "cand",
				FindingID:   "find",
				Disposition: "valid",
				DecidedBy:   "user",
			},
			wantErr: true,
		},
		{
			name: "invalid disposition",
			disposition: FindingDisposition{
				ID:          "d1",
				CampaignID:  "camp",
				SampleID:    "samp",
				CandidateID: "cand",
				FindingID:   "find",
				Disposition: "maybe",
				DecidedBy:   "user",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.RecordFindingDisposition(ctx, tt.disposition)
			if (err != nil) != tt.wantErr {
				t.Errorf("RecordFindingDisposition err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestListDispositionsForCampaignSample(t *testing.T) {
	store := newEvaluationStore(t)
	ctx := context.Background()

	// Create a campaign
	campaign, err := store.CreateEvaluationCampaign(ctx, EvaluationCampaign{
		ID:                GenerateID(),
		Name:              "test campaign",
		AllowedProjectIDs: []string{"proj1"},
		AllowedModelIDs:   []string{"model1"},
		CohortManifest:    "{}",
		AttemptCap:        5,
	})
	if err != nil {
		t.Fatalf("CreateEvaluationCampaign: %v", err)
	}

	// Create a sample
	sample, err := store.CreateEvaluationSample(ctx, EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		ProjectID:           "proj1",
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	})
	if err != nil {
		t.Fatalf("CreateEvaluationSample: %v", err)
	}

	// Create a candidate
	candidate := newTestCandidate(campaign.ID)
	candidate, err = store.CreateEvaluationCandidate(ctx, candidate)
	if err != nil {
		t.Fatalf("CreateEvaluationCandidate: %v", err)
	}

	dispositions := []FindingDisposition{
		{
			ID:          "d1",
			CampaignID:  campaign.ID,
			SampleID:    sample.ID,
			CandidateID: candidate.ID,
			FindingID:   "f1",
			Disposition: "valid",
			Evidence:    "evidence 1",
			DecidedBy:   "ops",
		},
		{
			ID:          "d2",
			CampaignID:  campaign.ID,
			SampleID:    sample.ID,
			CandidateID: candidate.ID,
			FindingID:   "f2",
			Disposition: "invalid",
			Evidence:    "evidence 2",
			DecidedBy:   "ops",
		},
	}

	for _, d := range dispositions {
		_, err := store.RecordFindingDisposition(ctx, d)
		if err != nil {
			t.Fatalf("RecordFindingDisposition failed: %v", err)
		}
	}

	retrieved, err := store.ListDispositionsForCampaignSample(ctx, campaign.ID, sample.ID)
	if err != nil {
		t.Fatalf("ListDispositionsForCampaignSample failed: %v", err)
	}
	if len(retrieved) != len(dispositions) {
		t.Errorf("count mismatch: got %d, want %d", len(retrieved), len(dispositions))
	}
}
