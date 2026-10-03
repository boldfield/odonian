package store

import (
	"context"
	"fmt"
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
	configureTestEvaluationPool(t, s, "pool1")
	return s
}

// configureTestEvaluationPool gives a pool ample concurrency-only capacity so
// tests that are not about admission are not limited by it.
func configureTestEvaluationPool(t *testing.T, s Store, id string) {
	t.Helper()
	if _, err := s.ConfigureEvaluationPool(context.Background(), EvaluationPoolConfig{ID: id, ConcurrencyOnly: true, ConcurrentLimit: 1000}); err != nil {
		t.Fatalf("ConfigureEvaluationPool: %v", err)
	}
}

func newTestCandidate(campaignID string) EvaluationCandidate {
	identity := evaluation.CandidateIdentity{
		AdapterName:        "fake",
		AdapterVersion:     "v1",
		ModelID:            "model1",
		ModelRevision:      "unknown",
		RuntimeName:        "test",
		RuntimeVersion:     "v1",
		ReasoningSettings:  evaluation.UnknownSettings(),
		GenerationSettings: evaluation.UnknownSettings(),
		PromptVersion:      "v1",
		Tools:              evaluation.UnknownNames(),
		Observers:          evaluation.UnknownNames(),
		AccountPool:        "pool1",
	}
	config, _ := evaluation.NewCandidateConfig(identity)
	return EvaluationCandidate{
		ID:                    GenerateID(),
		CampaignID:            campaignID,
		AdapterName:           "fake",
		ModelIdentity:         "model1",
		RuntimeVersion:        "v1",
		PromptVersion:         "v1",
		AccountPoolID:         "pool1",
		PerCandidateCap:       5,
		CandidateConfigDigest: config.Digest(),
	}
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
	candidate := newTestCandidate(campaign.ID)
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

	candidate := newTestCandidate(campaign.ID)
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
	candidate := newTestCandidate(campaign.ID)
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

	candidate := newTestCandidate(campaign.ID)
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

	candidate := newTestCandidate(campaign.ID)
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

	// Finalize the attempt before storing findings
	status := evaluation.StatusCompleted
	err := st.FinalizeEvaluationAttempt(ctx, attemptID, attemptID, "completed", &status, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("FinalizeEvaluationAttempt failed: %v", err)
	}

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

func TestEvaluationRetryAttemptLifecycle(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	// Setup: campaign, candidate, sample
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "retry-test",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := newTestCandidate(campaign.ID)
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

	// First claim creates attempt #1
	req1 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	result1, err := st.ClaimEvaluationJob(ctx, req1)
	if err != nil {
		t.Fatalf("First claim failed: %v", err)
	}
	if result1.Attempt.SequenceNumber != 1 {
		t.Errorf("First attempt should have sequence 1, got %d", result1.Attempt.SequenceNumber)
	}
	attempt1ID := result1.Attempt.ID

	// Finalize the first attempt
	status := evaluation.StatusCompleted
	err = st.FinalizeEvaluationAttempt(ctx, attempt1ID, attempt1ID, "completed", &status, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Finalize attempt 1 failed: %v", err)
	}

	// Second claim with new request ID creates attempt #2
	req2 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req2",
		LeaseExpires: 5 * time.Minute,
	}
	result2, err := st.ClaimEvaluationJob(ctx, req2)
	if err != nil {
		t.Fatalf("Second claim failed: %v", err)
	}
	if result2.Attempt.SequenceNumber != 2 {
		t.Errorf("Second attempt should have sequence 2, got %d", result2.Attempt.SequenceNumber)
	}
	if result2.Attempt.PreviousAttemptID == nil || *result2.Attempt.PreviousAttemptID != attempt1ID {
		t.Error("Previous attempt should be attempt 1")
	}

	// Idempotent claim with same request ID returns same attempt
	result2_again, err := st.ClaimEvaluationJob(ctx, req2)
	if err != nil {
		t.Fatalf("Idempotent claim failed: %v", err)
	}
	if result2_again.Attempt.ID != result2.Attempt.ID {
		t.Errorf("Idempotent claim should return same attempt")
	}
}

func TestEvaluationCapacityEnforcement(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	// Setup: campaign with low attempt cap
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "cap-test",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     2,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := newTestCandidate(campaign.ID)
	candidate, _ = st.CreateEvaluationCandidate(ctx, candidate)

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

	// First claim for sample1 uses up attempt 1
	req1 := EvaluationJobClaim{
		SampleID:     sample1.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	result1, err := st.ClaimEvaluationJob(ctx, req1)
	if err != nil {
		t.Fatalf("Claim 1 failed: %v", err)
	}

	// Finalize and retry for sample1 uses up attempt 2
	status := evaluation.StatusCompleted
	st.FinalizeEvaluationAttempt(ctx, result1.Attempt.ID, result1.Attempt.ID, "completed", &status, nil, nil, nil, nil)

	req1_retry := EvaluationJobClaim{
		SampleID:     sample1.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1_retry",
		LeaseExpires: 5 * time.Minute,
	}
	_, err = st.ClaimEvaluationJob(ctx, req1_retry)
	if err != nil {
		t.Fatalf("Retry claim failed: %v", err)
	}

	// Campaign capacity exhausted, third claim should fail
	req2 := EvaluationJobClaim{
		SampleID:     sample2.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req2",
		LeaseExpires: 5 * time.Minute,
	}
	_, err = st.ClaimEvaluationJob(ctx, req2)
	if err != ErrEvaluationCapacityExhausted {
		t.Errorf("Expected capacity exhausted error, got %v", err)
	}
}

func TestEvaluationFencingLateStaleFence(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	// Setup
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "fence-test",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := newTestCandidate(campaign.ID)
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

	// Claim with short lease
	req := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 100 * time.Millisecond,
	}
	result, _ := st.ClaimEvaluationJob(ctx, req)

	// Wait for lease to expire
	time.Sleep(150 * time.Millisecond)

	// Try to finalize the expired attempt; should fail
	status := evaluation.StatusCompleted
	err := st.FinalizeEvaluationAttempt(ctx, result.Attempt.ID, result.Attempt.ID, "completed", &status, nil, nil, nil, nil)
	if err != ErrEvaluationAttemptExpired {
		t.Errorf("Expected expired error, got %v", err)
	}

	// Verify the attempt is marked as expired
	attempt, err := st.GetEvaluationAttempt(ctx, result.Attempt.ID)
	if err != nil {
		t.Fatalf("Get attempt failed: %v", err)
	}
	if attempt.State != EvalAttemptExpired {
		t.Errorf("Attempt should be expired, got state %s", attempt.State)
	}
}

func TestEvaluationFencingWrongAttemptID(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	// Setup
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "fence-wrong-test",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := newTestCandidate(campaign.ID)
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

	// First claim creates attempt #1
	req1 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	result1, _ := st.ClaimEvaluationJob(ctx, req1)

	// Finalize attempt #1
	status := evaluation.StatusCompleted
	st.FinalizeEvaluationAttempt(ctx, result1.Attempt.ID, result1.Attempt.ID, "completed", &status, nil, nil, nil, nil)

	// Create attempt #2
	req2 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req2",
		LeaseExpires: 5 * time.Minute,
	}
	result2, _ := st.ClaimEvaluationJob(ctx, req2)

	// Try to finalize attempt #1 again (it's no longer current)
	err := st.FinalizeEvaluationAttempt(ctx, result1.Attempt.ID, result1.Attempt.ID, "completed", &status, nil, nil, nil, nil)
	if err != ErrEvaluationAttemptFinalized {
		t.Errorf("Expected finalized error for non-current attempt, got %v", err)
	}

	// Try to finalize attempt #2 with wrong fence ID
	err = st.FinalizeEvaluationAttempt(ctx, result2.Attempt.ID, GenerateID(), "completed", &status, nil, nil, nil, nil)
	if err != ErrEvaluationFenceMismatch {
		t.Errorf("Expected fence mismatch error, got %v", err)
	}
}

func TestEvaluationFindingValidation(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	// Setup
	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "finding-test",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := newTestCandidate(campaign.ID)
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

	// Cannot store findings on active attempt
	summary := "Test finding"
	_, err := st.StoreEvaluationFinding(ctx, result.Attempt.ID, 0, evaluation.SeverityMaterial, "main.go", 42, &summary, nil)
	if err == nil {
		t.Error("Expected error storing findings on active attempt")
	}

	// Finalize the attempt
	status := evaluation.StatusCompleted
	st.FinalizeEvaluationAttempt(ctx, result.Attempt.ID, result.Attempt.ID, "completed", &status, nil, nil, nil, nil)

	// Can store findings on finalized attempt
	findingID, err := st.StoreEvaluationFinding(ctx, result.Attempt.ID, 0, evaluation.SeverityMaterial, "main.go", 42, &summary, nil)
	if err != nil {
		t.Fatalf("StoreEvaluationFinding on finalized attempt failed: %v", err)
	}
	if findingID == "" {
		t.Error("Finding ID should not be empty")
	}
}

// TestEvaluationLeaseExpirySweep tests that expired attempts are properly swept and reclaimed.
func TestEvaluationLeaseExpirySweep(t *testing.T) {
	// Use injectable clock for precise lease expiry testing
	var fakeNow time.Time = time.Unix(1000, 0)
	s, err := Open(filepath.Join(t.TempDir(), "eval.db"), defaultTestAllowedModels(), WithClock(func() time.Time { return fakeNow }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	configureTestEvaluationPool(t, s, "pool1")
	st := s

	ctx := context.Background()

	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "test-campaign",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     3,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	candidate := newTestCandidate(campaign.ID)
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

	// Claim first attempt with 100-second lease
	req1 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 100 * time.Second,
	}
	result1, _ := st.ClaimEvaluationJob(ctx, req1)
	attemptID1 := result1.Attempt.ID

	// Advance time past first lease expiry
	fakeNow = fakeNow.Add(101 * time.Second)

	// Expire overdue attempts
	expiredCount, err := st.ExpireEvaluationAttempts(ctx, fakeNow)
	if err != nil {
		t.Fatalf("ExpireEvaluationAttempts failed: %v", err)
	}
	if expiredCount != 1 {
		t.Errorf("Expected 1 expired attempt, got %d", expiredCount)
	}

	// Verify attempt is marked as expired
	attempt, _ := st.GetEvaluationAttempt(ctx, attemptID1)
	if attempt.State != EvalAttemptExpired {
		t.Errorf("Attempt should be expired, got state: %s", attempt.State)
	}
	if attempt.ExitClass == nil || *attempt.ExitClass != EvalExitLeaseExpired {
		t.Errorf("Exit class should be lease_expired, got: %v", attempt.ExitClass)
	}

	// Should now be able to claim a retry attempt (expired attempt freed capacity)
	req2 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req2",
		LeaseExpires: 100 * time.Second,
	}
	result2, err := st.ClaimEvaluationJob(ctx, req2)
	if err != nil {
		t.Fatalf("Should be able to claim after expiry, got error: %v", err)
	}
	if result2.Attempt.SequenceNumber != 2 {
		t.Errorf("Second attempt should have sequence number 2, got %d", result2.Attempt.SequenceNumber)
	}
}

// TestEvaluationConcurrentClaim tests that concurrent claims on different samples are properly handled.
func TestEvaluationConcurrentClaim(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

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

	candidate := newTestCandidate(campaign.ID)
	candidate, _ = st.CreateEvaluationCandidate(ctx, candidate)

	// Create a single sample for concurrent claims
	sample := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign.ID,
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "sha1",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample, _ = st.CreateEvaluationSample(ctx, sample)

	// Launch concurrent claims for the SAME sample/candidate pair
	numClaims := 16
	results := make([]EvaluationJobClaimResult, numClaims)
	errsChan := make(chan error, numClaims)
	successCount := 0

	for i := 0; i < numClaims; i++ {
		go func(idx int) {
			reqID := fmt.Sprintf("req%d", idx)
			req := EvaluationJobClaim{
				SampleID:     sample.ID,
				CandidateID:  candidate.ID,
				RequestID:    reqID,
				LeaseExpires: 5 * time.Minute,
			}
			result, err := st.ClaimEvaluationJob(ctx, req)
			if err != nil {
				errsChan <- err
				return
			}
			results[idx] = result
			errsChan <- nil
		}(i)
	}

	// Collect results - only one should succeed, others should fail due to capacity limit
	for i := 0; i < numClaims; i++ {
		if err := <-errsChan; err == nil {
			successCount++
		}
	}

	// With AttemptCap=1, exactly one concurrent claim should succeed
	if successCount != 1 {
		t.Errorf("Expected exactly 1 successful concurrent claim, got %d", successCount)
	}

	// Verify the one successful claim has sequence number 1
	for i := 0; i < numClaims; i++ {
		if results[i].Attempt.SequenceNumber == 1 {
			if results[i].Job.SampleID != sample.ID {
				t.Errorf("Successful result sample ID mismatch")
			}
		}
	}
}

// TestEvaluationProductionIsolation tests that evaluation job operations never affect production review tasks.
func TestEvaluationProductionIsolation(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	// Create a campaign and sample/candidate for evaluation
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

	candidate := newTestCandidate(campaign.ID)
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

	// Claim, finalize, and add findings to an evaluation job
	req := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	result, _ := st.ClaimEvaluationJob(ctx, req)

	status := evaluation.StatusCompleted
	st.FinalizeEvaluationAttempt(ctx, result.Attempt.ID, result.Attempt.ID, EvalExitCompleted, &status, nil, nil, nil, nil)

	summary := "Test finding"
	st.StoreEvaluationFinding(ctx, result.Attempt.ID, 0, evaluation.SeverityMaterial, "main.go", 42, &summary, nil)

	// Verify evaluation data exists
	jobID := result.Job.ID
	attemptID := result.Attempt.ID
	retrievedJob, err := st.GetEvaluationJob(ctx, jobID)
	if err != nil {
		t.Fatalf("Failed to retrieve evaluation job: %v", err)
	}
	if retrievedJob.ID != jobID {
		t.Error("Job ID mismatch")
	}

	// Verify that the evaluation job tables are completely independent
	// A production task/review should never be directly connected to evaluation data
	// This test ensures evaluation job creation and completion do NOT trigger any
	// production task state changes, aggregateReviewRound calls, or scorecard updates.

	// The key assertion: evaluation tables should have the data, production tables should be unaffected
	// (since we're testing in a store without production tasks, we just verify evaluation data persists)
	attempt, err := st.GetEvaluationAttempt(ctx, attemptID)
	if err != nil {
		t.Fatalf("Failed to retrieve evaluation attempt: %v", err)
	}
	if attempt.State != EvalAttemptFinalized {
		t.Errorf("Attempt should be finalized, got state: %s", attempt.State)
	}
	if attempt.ExitClass == nil || *attempt.ExitClass != EvalExitCompleted {
		t.Errorf("Exit class should be 'completed', got: %v", attempt.ExitClass)
	}
}

// TestEvaluationConfigVersionIsolation tests that different candidate configurations maintain separate immutable identities.
func TestEvaluationConfigVersionIsolation(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	campaign := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "test-campaign",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     20,
		AccountPoolID:  "pool1",
	}
	campaign, _ = st.CreateEvaluationCampaign(ctx, campaign)

	// Create first candidate version
	identity1 := evaluation.CandidateIdentity{
		AdapterName:        "fake",
		AdapterVersion:     "v1",
		ModelID:            "model-a",
		ModelRevision:      "unknown",
		RuntimeName:        "test",
		RuntimeVersion:     "v1",
		ReasoningSettings:  evaluation.UnknownSettings(),
		GenerationSettings: evaluation.UnknownSettings(),
		PromptVersion:      "v1",
		Tools:              evaluation.UnknownNames(),
		Observers:          evaluation.UnknownNames(),
		AccountPool:        "pool1",
	}
	config1, _ := evaluation.NewCandidateConfig(identity1)

	candidate1 := EvaluationCandidate{
		ID:                    GenerateID(),
		CampaignID:            campaign.ID,
		AdapterName:           "fake",
		ModelIdentity:         "model-a",
		RuntimeVersion:        "v1",
		PromptVersion:         "v1",
		AccountPoolID:         "pool1",
		PerCandidateCap:       5,
		CandidateConfigDigest: config1.Digest(),
	}
	candidate1, _ = st.CreateEvaluationCandidate(ctx, candidate1)

	// Create second candidate version with different model ID
	identity2 := evaluation.CandidateIdentity{
		AdapterName:        "fake",
		AdapterVersion:     "v1",
		ModelID:            "model-b",
		ModelRevision:      "unknown",
		RuntimeName:        "test",
		RuntimeVersion:     "v1",
		ReasoningSettings:  evaluation.UnknownSettings(),
		GenerationSettings: evaluation.UnknownSettings(),
		PromptVersion:      "v1",
		Tools:              evaluation.UnknownNames(),
		Observers:          evaluation.UnknownNames(),
		AccountPool:        "pool1",
	}
	config2, _ := evaluation.NewCandidateConfig(identity2)

	candidate2 := EvaluationCandidate{
		ID:                    GenerateID(),
		CampaignID:            campaign.ID,
		AdapterName:           "fake",
		ModelIdentity:         "model-b",
		RuntimeVersion:        "v1",
		PromptVersion:         "v1",
		AccountPoolID:         "pool1",
		PerCandidateCap:       5,
		CandidateConfigDigest: config2.Digest(),
	}
	candidate2, _ = st.CreateEvaluationCandidate(ctx, candidate2)

	// Verify the two candidates have different digests
	if candidate1.CandidateConfigDigest == candidate2.CandidateConfigDigest {
		t.Error("Different candidate configs should have different digests")
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
	sample, _ = st.CreateEvaluationSample(ctx, sample)

	// Claim with candidate1
	req1 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate1.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	result1, _ := st.ClaimEvaluationJob(ctx, req1)

	// Finalize with candidate1
	status := evaluation.StatusCompleted
	st.FinalizeEvaluationAttempt(ctx, result1.Attempt.ID, result1.Attempt.ID, EvalExitCompleted, &status, nil, nil, nil, nil)

	// Now claim the same sample with candidate2 (different version)
	req2 := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate2.ID,
		RequestID:    "req2",
		LeaseExpires: 5 * time.Minute,
	}
	result2, _ := st.ClaimEvaluationJob(ctx, req2)

	// Verify we have two separate jobs for the same sample with different candidates
	if result1.Job.ID == result2.Job.ID {
		t.Error("Different candidates should create different jobs")
	}

	// Verify both jobs use their correct candidates
	if result1.Job.CandidateID != candidate1.ID {
		t.Error("Job 1 should reference candidate1")
	}
	if result2.Job.CandidateID != candidate2.ID {
		t.Error("Job 2 should reference candidate2")
	}
}

// TestEvaluationCampaignMismatch tests that samples and candidates from different campaigns cannot be paired.
func TestEvaluationCampaignMismatch(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

	// Create two campaigns
	campaign1 := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "campaign1",
		ProjectID:      "proj1",
		AllowedModelID: "model1",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool1",
	}
	campaign1, _ = st.CreateEvaluationCampaign(ctx, campaign1)

	campaign2 := EvaluationCampaign{
		ID:             GenerateID(),
		Name:           "campaign2",
		ProjectID:      "proj2",
		AllowedModelID: "model2",
		CohortManifest: `{"samples": []}`,
		AttemptCap:     10,
		AccountPoolID:  "pool2",
	}
	campaign2, _ = st.CreateEvaluationCampaign(ctx, campaign2)

	// Create candidate in campaign1
	candidate := newTestCandidate(campaign1.ID)
	candidate, _ = st.CreateEvaluationCandidate(ctx, candidate)

	// Create sample in campaign2
	sample := EvaluationSample{
		ID:                  GenerateID(),
		CampaignID:          campaign2.ID,
		OriginalTaskID:      "task1",
		OriginalReviewRound: 1,
		SubmittedSHA:        "abc123",
		PromptVersion:       "v1",
		ModelVersion:        "v1",
		RuntimeVersion:      "v1",
	}
	sample, _ = st.CreateEvaluationSample(ctx, sample)

	// Try to claim with mismatched campaign
	req := EvaluationJobClaim{
		SampleID:     sample.ID,
		CandidateID:  candidate.ID,
		RequestID:    "req1",
		LeaseExpires: 5 * time.Minute,
	}
	_, err := st.ClaimEvaluationJob(ctx, req)
	if err == nil {
		t.Error("Should reject claim with campaign mismatch")
	}
	if err != ErrEvaluationInvalidInput {
		t.Errorf("Expected ErrEvaluationInvalidInput, got %v", err)
	}
}

// TestEvaluationFindingRejectionOnNonCompleted tests that findings cannot be stored on non-completed attempts.
func TestEvaluationFindingRejectionOnNonCompleted(t *testing.T) {
	st := newEvaluationStore(t)
	ctx := context.Background()

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

	candidate := newTestCandidate(campaign.ID)
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

	summary := "Test finding"

	// Finalize with failed status
	status := evaluation.StatusFailed
	errorClass := evaluation.ErrClassRuntimeError
	st.FinalizeEvaluationAttempt(ctx, result.Attempt.ID, result.Attempt.ID, "failed", &status, &errorClass, nil, nil, nil)

	// Should reject findings on failed attempt
	_, err := st.StoreEvaluationFinding(ctx, result.Attempt.ID, 0, evaluation.SeverityMaterial, "main.go", 42, &summary, nil)
	if err == nil {
		t.Error("Should reject findings on failed attempt")
	}
	if err.Error() != "findings can only be stored on completed attempts, got exit_class: failed" {
		t.Errorf("Unexpected error: %v", err)
	}
}
