package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
)

func newEvaluationStore(t *testing.T) Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "eval.db"), defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEvaluationJobClaim(t *testing.T) {
	st := newEvaluationStore(t)

	ctx := context.Background()

	// Create a campaign
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "test-campaign",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, err := st.CreateEvaluationCampaign(ctx, campaign)
	if err != nil {
		t.Fatalf("CreateEvaluationCampaign failed: %v", err)
	}

	// Create a candidate
	candidate := EvaluationCandidate{
		ID:              GenerateID(),
		CampaignID:      campaign.ID,
		AdapterName:     "fake",
		ModelIdentity:   "model1",
		RuntimeVersion:  "v1",
		PromptVersion:   "v1",
		AccountPoolID:   "pool1",
		PerCandidateCap: 5,
	}
	candidate, err = st.CreateEvaluationCandidate(ctx, candidate)
	if err != nil {
		t.Fatalf("CreateEvaluationCandidate failed: %v", err)
	}

	// Create a sample
	sample := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample, err = st.CreateEvaluationSample(ctx, sample)
	if err != nil {
		t.Fatalf("CreateEvaluationSample failed: %v", err)
	}

	// Claim a job
	req := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}

	result, err := st.ClaimEvaluationJob(ctx, req)
	if err != nil {
		t.Fatalf("ClaimEvaluationJob failed: %v", err)
	}

	if result.Job.ID == "" {
		t.Error("Job ID is empty")
	}
	if result.Job.SampleID != sample.ID {
		t.Errorf("Sample ID mismatch: got %s, want %s", result.Job.SampleID, sample.ID)
	}
	if result.Job.CandidateID != candidate.ID {
		t.Errorf("Candidate ID mismatch: got %s, want %s", result.Job.CandidateID, candidate.ID)
	}
	if result.Attempt.SequenceNumber != 1 {
		t.Errorf("First attempt should have sequence number 1, got %d", result.Attempt.SequenceNumber)
	}
}

func TestEvaluationJobClaimIdempotent(t *testing.T) {
	st := newEvaluationStore(t)

	ctx := context.Background()

	// Create campaign, candidate, sample
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "test-campaign",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := EvaluationCandidate{
		ID:              GenerateID(),
		CampaignID:      campaign.ID,
		AdapterName:     "fake",
		ModelIdentity:   "model1",
		RuntimeVersion:  "v1",
		PromptVersion:   "v1",
		AccountPoolID:   "pool1",
		PerCandidateCap: 5,
	}
	candidate, _ = st.CreateEvaluationCandidate(ctx, candidate)

	sample := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample, _ = st.CreateEvaluationSample(ctx, sample)

	req := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}

	// First claim
	result1, err := st.ClaimEvaluationJob(ctx, req)
	if err != nil {
		t.Fatalf("First claim failed: %v", err)
	}

	// Second claim with same request ID should return existing job and attempt
	result2, err := st.ClaimEvaluationJob(ctx, req)
	if err != nil {
		t.Fatalf("Second claim failed: %v", err)
	}

	if result1.Job.ID != result2.Job.ID {
		t.Errorf("Job ID changed on idempotent claim: %s != %s", result1.Job.ID, result2.Job.ID)
	}
	if result1.Attempt.ID != result2.Attempt.ID {
		t.Errorf("Attempt ID changed on idempotent claim: %s != %s", result1.Attempt.ID, result2.Attempt.ID)
	}
}

func TestEvaluationJobCapacityEnforcement(t *testing.T) {
	st := newEvaluationStore(t)

	ctx := context.Background()

	// Create campaign with low capacity
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "test-campaign",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     1,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	// Create candidate with low per-candidate cap
	candidate := EvaluationCandidate{
		ID:              GenerateID(),
		CampaignID:      campaign.ID,
		AdapterName:     "fake",
		ModelIdentity:   "model1",
		RuntimeVersion:  "v1",
		PromptVersion:   "v1",
		AccountPoolID:   "pool1",
		PerCandidateCap: 1,
	}
	candidate, _ = st.CreateEvaluationCandidate(ctx, candidate)

	// Create two samples
	sample1 := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample1, _ = st.CreateEvaluationSample(ctx, sample1)

	sample2 := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		OriginalTaskID:      "task2",
		OriginalReviewRound: 1,
		SubmittedSHA:        "def456",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample2, _ = st.CreateEvaluationSample(ctx, sample2)

	// First claim should succeed
	req1 := EvaluationJobClaim{
		SampleID:     sample1.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	_, err := st.ClaimEvaluationJob(ctx, req1)
	if err != nil {
		t.Fatalf("First claim failed: %v", err)
	}

	// Second claim should fail due to candidate capacity
	req2 := EvaluationJobClaim{
		SampleID:     sample2.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req2",
		LeaseExpires: 5 * time.Minute,
	}
	_, err = st.ClaimEvaluationJob(ctx, req2)
	if err != ErrEvaluationCapacityExhausted {
		t.Errorf("Expected ErrEvaluationCapacityExhausted, got %v", err)
	}
}

func TestEvaluationAttemptRenewAndFinalize(t *testing.T) {
	st := newEvaluationStore(t)

	ctx := context.Background()

	// Create and claim a job
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "test",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := EvaluationCandidate{
		ID:              GenerateID(),
		CampaignID:      campaign.ID,
		AdapterName:     "fake",
		ModelIdentity:   "model1",
		RuntimeVersion:  "v1",
		PromptVersion:   "v1",
		AccountPoolID:   "pool1",
		PerCandidateCap: 5,
	}
	candidate, _ = st.CreateEvaluationCandidate(ctx, candidate)

	sample := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample, _ = st.CreateEvaluationSample(ctx, sample)

	req := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	result, _ := st.ClaimEvaluationJob(ctx, req)

	attemptID := result.Attempt.ID

	// Renew the attempt
	newExpires := st.Now().Add(10 * time.Minute)
	err := st.RenewEvaluationAttempt(ctx, attemptID, newExpires)
	if err != nil {
		t.Fatalf("RenewEvaluationAttempt failed: %v", err)
	}

	// Finalize the attempt
	status := evaluation.StatusCompleted
	durationMs := 1000
	usageTokens := 100

	err = st.FinalizeEvaluationAttempt(ctx, attemptID, attemptID, EvalExitCompleted, &status, nil, nil, &durationMs, &usageTokens)
	if err != nil {
		t.Fatalf("FinalizeEvaluationAttempt failed: %v", err)
	}

	// Verify attempt is finalized
	attempt, err := st.GetEvaluationAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("GetEvaluationAttempt failed: %v", err)
	}

	if attempt.State != EvalAttemptFinalized {
		t.Errorf("Attempt state should be finalized, got %s", attempt.State)
	}
	if attempt.ExitClass == nil || *attempt.ExitClass != EvalExitCompleted {
		t.Errorf("Exit class should be %s, got %v", EvalExitCompleted, attempt.ExitClass)
	}
	if attempt.Status == nil || *attempt.Status != string(status) {
		t.Errorf("Status should be %s, got %v", status, attempt.Status)
	}
}

func TestEvaluationFindingStorage(t *testing.T) {
	st := newEvaluationStore(t)

	ctx := context.Background()

	// Create and finalize an attempt
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "test",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := EvaluationCandidate{
		ID:              GenerateID(),
		CampaignID:      campaign.ID,
		AdapterName:     "fake",
		ModelIdentity:   "model1",
		RuntimeVersion:  "v1",
		PromptVersion:   "v1",
		AccountPoolID:   "pool1",
		PerCandidateCap: 5,
	}
	candidate, _ = st.CreateEvaluationCandidate(ctx, candidate)

	sample := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample, _ = st.CreateEvaluationSample(ctx, sample)

	req := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	result, _ := st.ClaimEvaluationJob(ctx, req)

	attemptID := result.Attempt.ID

	// Store a finding
	summary := "Test finding"
	findingID, err := st.StoreEvaluationFinding(ctx, attemptID, 0, evaluation.SeverityMaterial, "main.go", 42, &summary, nil)
	if err != nil {
		t.Fatalf("StoreEvaluationFinding failed: %v", err)
	}

	if findingID == "" {
		t.Error("Finding ID is empty")
	}
}
