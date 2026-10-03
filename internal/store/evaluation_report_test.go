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

// TestListCampaignAttemptsWithUnequalCoverage tests report generation with unequal candidate coverage.
func TestListCampaignAttemptsWithUnequalCoverage(t *testing.T) {
	store := newEvaluationStore(t)
	ctx := context.Background()

	// Create campaign with two candidates
	campaign, err := store.CreateEvaluationCampaign(ctx, EvaluationCampaign{
		ID:                GenerateID(),
		Name:              "unequal coverage campaign",
		AllowedProjectIDs: []string{"proj1"},
		AllowedModelIDs:   []string{"model1", "model2"},
		CohortManifest:    "{}",
		AttemptCap:        100,
	})
	if err != nil {
		t.Fatalf("CreateEvaluationCampaign: %v", err)
	}

	// Create two samples
	sample1, _ := store.CreateEvaluationSample(ctx, EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		ProjectID:           "proj1",
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "sha1",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	})
	sample2, _ := store.CreateEvaluationSample(ctx, EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		ProjectID:           "proj1",
		OriginalTaskID:      "task2",
		OriginalReviewRound: 1,
		SubmittedSHA:        "sha2",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	})

	// Create two candidates
	cand1 := newTestCandidate(campaign.ID)
	cand1, _ = store.CreateEvaluationCandidate(ctx, cand1)
	cand2 := newTestCandidate(campaign.ID)
	cand2, _ = store.CreateEvaluationCandidate(ctx, cand2)

	// Create jobs: candidate1 has both samples, candidate2 has only sample1
	pool, _ := store.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: "pool1", ConcurrentLimit: 10})
	store.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: cand1.AccountPoolID(), ConcurrentLimit: 10})
	store.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{ID: cand2.AccountPoolID(), ConcurrentLimit: 10})

	// Candidate 1 completes both samples
	job1, _ := store.ClaimEvaluationJob(ctx, EvaluationJobClaim{
		SampleID:     sample1.ID,
		CandidateID:  cand1.ID,
		RequestID:    "req1",
		LeaseExpires: 1 * time.Hour,
	})
	store.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{
		AttemptID:      job1.Attempt.ID,
		FenceAttemptID: job1.Attempt.ID,
		ExitClass:      EvalExitCompleted,
		Status:         &completedStatus,
		Findings:       []evaluation.Finding{{ID: "f1", Summary: "Finding 1", Severity: "material"}},
	})

	job2, _ := store.ClaimEvaluationJob(ctx, EvaluationJobClaim{
		SampleID:     sample2.ID,
		CandidateID:  cand1.ID,
		RequestID:    "req2",
		LeaseExpires: 1 * time.Hour,
	})
	store.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{
		AttemptID:      job2.Attempt.ID,
		FenceAttemptID: job2.Attempt.ID,
		ExitClass:      EvalExitCompleted,
		Status:         &completedStatus,
		Findings:       []evaluation.Finding{},
	})

	// Candidate 2 completes only sample1
	job3, _ := store.ClaimEvaluationJob(ctx, EvaluationJobClaim{
		SampleID:     sample1.ID,
		CandidateID:  cand2.ID,
		RequestID:    "req3",
		LeaseExpires: 1 * time.Hour,
	})
	store.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{
		AttemptID:      job3.Attempt.ID,
		FenceAttemptID: job3.Attempt.ID,
		ExitClass:      EvalExitCompleted,
		Status:         &completedStatus,
		Findings:       []evaluation.Finding{{ID: "f1", Summary: "Finding 1", Severity: "material"}},
	})

	// Fetch campaign attempt stats
	stats, err := store.ListCampaignAttempts(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("ListCampaignAttempts: %v", err)
	}

	// Verify cohort size
	if stats.CohortSize != 2 {
		t.Errorf("CohortSize: got %d, want 2", stats.CohortSize)
	}

	// Verify candidate stats
	if len(stats.Candidates) != 2 {
		t.Errorf("Candidates: got %d, want 2", len(stats.Candidates))
	}

	c1 := stats.Candidates[cand1.ID]
	if c1 == nil || c1.CompletedAttempts != 2 {
		t.Errorf("Candidate1 CompletedAttempts: got %v, want 2", c1.CompletedAttempts)
	}

	c2 := stats.Candidates[cand2.ID]
	if c2 == nil || c2.CompletedAttempts != 1 {
		t.Errorf("Candidate2 CompletedAttempts: got %v, want 1", c2.CompletedAttempts)
	}
}

// TestListCampaignAttemptsWithMissingUsage tests report with findings that have no usage data.
func TestListCampaignAttemptsWithMissingUsage(t *testing.T) {
	store := newEvaluationStore(t)
	ctx := context.Background()

	campaign, _ := store.CreateEvaluationCampaign(ctx, EvaluationCampaign{
		ID:                GenerateID(),
		Name:              "missing usage campaign",
		AllowedProjectIDs: []string{"proj1"},
		AllowedModelIDs:   []string{"model1"},
		CohortManifest:    "{}",
		AttemptCap:        100,
	})

	sample, _ := store.CreateEvaluationSample(ctx, EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		ProjectID:           "proj1",
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "sha1",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	})

	candidate := newTestCandidate(campaign.ID)
	candidate, _ = store.CreateEvaluationCandidate(ctx, candidate)
	store.ConfigureEvaluationPool(ctx, EvaluationPoolConfig{
		ID:               candidate.AccountPoolID(),
		ConcurrentLimit:  10,
	})

	// Create attempt with no usage tokens
	job, _ := store.ClaimEvaluationJob(ctx, EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 1 * time.Hour,
	})

	// Finalize with no usage
	store.FinalizeEvaluationAttempt(ctx, EvaluationAttemptResult{
		AttemptID:      job.Attempt.ID,
		FenceAttemptID: job.Attempt.ID,
		ExitClass:      EvalExitCompleted,
		Status:         &completedStatus,
		DurationMs:     intPtr(5000),
		UsageTokens:    nil, // No usage reported
		Findings:       []evaluation.Finding{{ID: "f1", Summary: "Finding 1", Severity: "material"}},
	})

	stats, err := store.ListCampaignAttempts(ctx, campaign.ID)
	if err != nil {
		t.Fatalf("ListCampaignAttempts: %v", err)
	}

	// Verify that missing usage doesn't crash
	c := stats.Candidates[candidate.ID]
	if c.TotalUsageTokens != 0 {
		t.Errorf("TotalUsageTokens: got %d, want 0", c.TotalUsageTokens)
	}
}

func intPtr(i int) *int {
	return &i
}

var completedStatus = "completed"
