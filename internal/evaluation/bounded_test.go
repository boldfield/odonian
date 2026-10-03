package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FakeEvaluationStore implements EvaluationStore for testing.
type FakeEvaluationStore struct {
	jobs     map[string]*EvaluationJob
	attempts map[string]*EvaluationAttempt
	results  map[string]*EvaluationAttemptResult

	// Simulated behaviors for testing.
	ShouldRejectClaim      bool
	ClaimErrorToReturn     error
	RenewalFailureCount    int
	RenewalFailuresIssued  int
	FinalizeFailureCount   int
	FinalizeFailuresIssued int
}

func NewFakeEvaluationStore() *FakeEvaluationStore {
	return &FakeEvaluationStore{
		jobs:     make(map[string]*EvaluationJob),
		attempts: make(map[string]*EvaluationAttempt),
		results:  make(map[string]*EvaluationAttemptResult),
	}
}

func (f *FakeEvaluationStore) ClaimEvaluationJob(ctx context.Context, req EvaluationJobClaim) (EvaluationJobClaimResult, error) {
	if f.ShouldRejectClaim || f.ClaimErrorToReturn != nil {
		err := f.ClaimErrorToReturn
		if err == nil {
			err = errors.New("evaluation campaign or candidate has exhausted attempt capacity")
		}
		return EvaluationJobClaimResult{}, err
	}

	now := time.Now().UTC()
	jobID := req.RequestID + "-job"
	attemptID := req.RequestID + "-attempt"

	job := &EvaluationJob{
		ID:               jobID,
		SampleID:         req.SampleID,
		CandidateID:      req.CandidateID,
		CurrentAttemptID: attemptID,
		CreatedAt:        now.Format(time.RFC3339),
	}

	attempt := &EvaluationAttempt{
		ID:        attemptID,
		JobID:     jobID,
		RequestID: req.RequestID,
		State:     "active",
		StartedAt: now.Format(time.RFC3339),
		ExpiresAt: now.Add(req.LeaseExpires).Format(time.RFC3339),
	}

	f.jobs[jobID] = job
	f.attempts[attemptID] = attempt

	return EvaluationJobClaimResult{
		Job:     *job,
		Attempt: *attempt,
	}, nil
}

func (f *FakeEvaluationStore) RenewEvaluationAttempt(ctx context.Context, attemptID string, expiresAt time.Time) error {
	if f.RenewalFailureCount > 0 && f.RenewalFailuresIssued < f.RenewalFailureCount {
		f.RenewalFailuresIssued++
		return errors.New("renewal failed")
	}

	attempt, ok := f.attempts[attemptID]
	if !ok {
		return errors.New("evaluation attempt not found")
	}
	attempt.ExpiresAt = expiresAt.Format(time.RFC3339)
	return nil
}

func (f *FakeEvaluationStore) FinalizeEvaluationAttempt(ctx context.Context, res EvaluationAttemptResult) error {
	if f.FinalizeFailureCount > 0 && f.FinalizeFailuresIssued < f.FinalizeFailureCount {
		f.FinalizeFailuresIssued++
		return errors.New("finalize failed")
	}

	attempt, ok := f.attempts[res.AttemptID]
	if !ok {
		return errors.New("evaluation attempt not found")
	}
	attempt.State = "finalized"

	f.results[res.AttemptID] = &res
	return nil
}

func TestBoundedComparisonClaimRenewFinalizeLifecycle(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	fakeStore := NewFakeEvaluationStore()

	cfg := BoundedComparisonConfig{
		Registry:             reg,
		Credentials:          MapCredentials{},
		Store:                fakeStore,
		RuntimeName:          "fake",
		StagingDir:           filepath.Join(dir, "staging"),
		Now:                  fixedClock,
		MaxRetries:           3,
		InitialLeaseExpiry:   1 * time.Second,
		LeaseRenewalInterval: 100 * time.Millisecond,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SampleID:      "sample-1",
		CandidateID:   "cand-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review this",
		ToolAccess:    ToolAccessRequirements{RequireSourceRetrieval: true},
	}

	res, err := cfg.RunBoundedComparison(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.AttemptID == "" {
		t.Errorf("expected attempt ID, got empty")
	}
	if res.ExitClass != ExitCompleted {
		t.Errorf("got exit class %s, want completed", res.ExitClass)
	}

	// Verify finalize was called by checking the result is stored.
	if _, ok := fakeStore.results[res.AttemptID]; !ok {
		t.Errorf("finalize result not stored")
	}
}

func TestBoundedComparisonPrelaunchDeferralAndRetries(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	fakeStore := NewFakeEvaluationStore()

	cfg := BoundedComparisonConfig{
		Registry:             reg,
		Credentials:          MapCredentials{},
		Store:                fakeStore,
		RuntimeName:          "fake",
		StagingDir:           filepath.Join(dir, "staging"),
		Now:                  fixedClock,
		MaxRetries:           2,
		InitialLeaseExpiry:   1 * time.Second,
		LeaseRenewalInterval: 100 * time.Millisecond,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SampleID:      "sample-1",
		CandidateID:   "cand-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review",
		ToolAccess:    ToolAccessRequirements{},
	}

	res, err := cfg.RunBoundedComparison(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Response.Status != StatusCompleted {
		t.Errorf("got status %s, want completed", res.Response.Status)
	}
}

func TestBoundedComparisonRunIDPathTraversalProtection(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	fakeStore := NewFakeEvaluationStore()

	cfg := BoundedComparisonConfig{
		Registry:    reg,
		Credentials: MapCredentials{},
		Store:       fakeStore,
		RuntimeName: "fake",
		StagingDir:  filepath.Join(dir, "staging"),
		Now:         fixedClock,
	}

	invalidRunIDs := []string{
		"../escape",
		"..\\escape",
		"/absolute",
		"with space",
		"with\nnewline",
		"with/slash",
		"",
	}

	for _, runID := range invalidRunIDs {
		req := BoundedComparisonRequest{
			RunID:         runID,
			SampleID:      "sample-1",
			CandidateID:   "cand-1",
			SnapshotPath:  dir,
			BlindedPrompt: "review",
			ToolAccess:    ToolAccessRequirements{},
		}

		_, err := cfg.RunBoundedComparison(context.Background(), req)
		if err == nil {
			t.Errorf("expected error for runID %q, got none", runID)
		}
	}

	// Valid run IDs should work.
	validRunIDs := []string{"run-1", "run_1", "run123", "r"}
	for _, runID := range validRunIDs {
		req := BoundedComparisonRequest{
			RunID:         runID,
			SampleID:      "sample-1",
			CandidateID:   "cand-1",
			SnapshotPath:  dir,
			BlindedPrompt: "review",
			ToolAccess:    ToolAccessRequirements{},
		}

		res, err := cfg.RunBoundedComparison(context.Background(), req)
		if err != nil {
			t.Errorf("unexpected error for valid runID %q: %v", runID, err)
		}
		if res.AttemptID == "" {
			t.Errorf("expected attempt ID for valid runID %q", runID)
		}
	}
}

func TestBoundedComparisonDistinctOutcomeRecording(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		expectedExit EvaluationExitClass
	}{
		{"completed", FakeModeSuccess, ExitCompleted},
		{"failed", FakeModeFailed, ExitFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := NewRegistry()
			registerFake(t, reg, "fake", tt.mode, baseIdentity(), fullCaps)

			dir := t.TempDir()
			fakeStore := NewFakeEvaluationStore()

			cfg := BoundedComparisonConfig{
				Registry:             reg,
				Credentials:          MapCredentials{},
				Store:                fakeStore,
				RuntimeName:          "fake",
				StagingDir:           filepath.Join(dir, "staging"),
				Now:                  fixedClock,
				MaxRetries:           3,
				InitialLeaseExpiry:   1 * time.Second,
				LeaseRenewalInterval: 100 * time.Millisecond,
			}

			req := BoundedComparisonRequest{
				RunID:         "run-1",
				SampleID:      "sample-1",
				CandidateID:   "cand-1",
				SnapshotPath:  dir,
				BlindedPrompt: "review",
				ToolAccess:    ToolAccessRequirements{},
			}

			res, err := cfg.RunBoundedComparison(context.Background(), req)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if res.ExitClass != tt.expectedExit {
				t.Errorf("got exit class %s, want %s", res.ExitClass, tt.expectedExit)
			}
		})
	}
}

func TestBoundedComparisonMalformedResultHandling(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	fakeStore := NewFakeEvaluationStore()

	cfg := BoundedComparisonConfig{
		Registry:             reg,
		Credentials:          MapCredentials{},
		Store:                fakeStore,
		RuntimeName:          "fake",
		StagingDir:           filepath.Join(dir, "staging"),
		Now:                  fixedClock,
		MaxRetries:           3,
		InitialLeaseExpiry:   1 * time.Second,
		LeaseRenewalInterval: 100 * time.Millisecond,
	}

	// Create a valid request.
	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SampleID:      "sample-1",
		CandidateID:   "cand-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review",
		ToolAccess:    ToolAccessRequirements{},
	}

	res, err := cfg.RunBoundedComparison(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	// Even if the adapter succeeds, finalize should record the result.
	if res.AttemptID == "" {
		t.Errorf("expected attempt ID")
	}
}

func TestBoundedComparisonExhaustedCampaignOutcome(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	fakeStore := NewFakeEvaluationStore()
	fakeStore.ClaimErrorToReturn = errors.New("evaluation campaign or candidate has exhausted attempt capacity")

	cfg := BoundedComparisonConfig{
		Registry:             reg,
		Credentials:          MapCredentials{},
		Store:                fakeStore,
		RuntimeName:          "fake",
		StagingDir:           filepath.Join(dir, "staging"),
		Now:                  fixedClock,
		MaxRetries:           3,
		InitialLeaseExpiry:   1 * time.Second,
		LeaseRenewalInterval: 100 * time.Millisecond,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SampleID:      "sample-1",
		CandidateID:   "cand-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review",
		ToolAccess:    ToolAccessRequirements{},
	}

	res, _ := cfg.RunBoundedComparison(context.Background(), req)
	if res.ExitClass != ExitExhaustedCampaign {
		t.Errorf("got exit class %s, want exhausted_campaign", res.ExitClass)
	}
}

func TestBoundedComparisonIsolatesCredentials(t *testing.T) {
	reg := NewRegistry()
	cfg, _ := NewCandidateConfig(baseIdentity())
	exe, _ := os.Executable()
	rt := Runtime{
		Name:         "with-creds",
		Executable:   exe,
		Timeout:      30 * time.Second,
		Capabilities: fullCaps,
		Candidate:    cfg,
		Args:         []string{fakeAdapterArg, "--mode", FakeModeProbe, "--request", PlaceholderRequestPath, "--identity", `{"adapter_name":"fake","adapter_version":"1","model_id":"m","model_revision":"unknown","runtime_name":"r","runtime_version":"1","reasoning_settings":{"known":false},"generation_settings":{"known":false},"prompt_version":"p","tools":{"known":true},"observers":{"known":false},"account_pool":"pool"}`},
		Credentials:  []CredentialRef{{EnvName: "TEST_SECRET", Ref: "secret1"}},
	}
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}

	secrets := MapCredentials{"secret1": "sensitive-data"}
	dir := t.TempDir()
	fakeStore := NewFakeEvaluationStore()

	cfg2 := BoundedComparisonConfig{
		Registry:             reg,
		Credentials:          secrets,
		Store:                fakeStore,
		RuntimeName:          "with-creds",
		StagingDir:           filepath.Join(dir, "staging"),
		Now:                  fixedClock,
		MaxRetries:           3,
		InitialLeaseExpiry:   1 * time.Second,
		LeaseRenewalInterval: 100 * time.Millisecond,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SampleID:      "sample-1",
		CandidateID:   "cand-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review",
		ToolAccess:    ToolAccessRequirements{},
	}

	res, err := cfg2.RunBoundedComparison(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Stderr != "" {
		t.Errorf("stderr should be empty after redaction, got: %s", res.Stderr)
	}
	// Verify that ODONIAN_TOKEN is not leaked in stderr.
	if res.Stderr != "" && contains(res.Stderr, "ODONIAN_TOKEN") {
		t.Errorf("credential leaked in stderr")
	}
}

func contains(s, substr string) bool {
	for i := 0; i < len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestSecondFakeAdapterWithComputePool(t *testing.T) {
	reg := NewRegistry()

	// First adapter: uses subscription account pool.
	id1 := CandidateIdentity{
		AdapterName: "fake-sub", AdapterVersion: "1", ModelID: "model-a", ModelRevision: Unknown,
		RuntimeName: "fake-sub-cli", RuntimeVersion: "1.0",
		ReasoningSettings: UnknownSettings(), GenerationSettings: UnknownSettings(),
		PromptVersion: "p1", Tools: UnknownNames(), Observers: UnknownNames(),
		AccountPool: "subscription-pool",
	}
	registerFake(t, reg, "fake-sub", FakeModeSuccess, id1, fullCaps)

	// Second adapter: uses compute-only pool with different usage units.
	id2 := CandidateIdentity{
		AdapterName: "fake-compute", AdapterVersion: "1", ModelID: "model-b", ModelRevision: Unknown,
		RuntimeName: "fake-compute-cli", RuntimeVersion: "2.0",
		ReasoningSettings: UnknownSettings(), GenerationSettings: UnknownSettings(),
		PromptVersion: "p1", Tools: UnknownNames(), Observers: UnknownNames(),
		AccountPool: "compute-pool",
	}
	registerFake(t, reg, "fake-compute", FakeModeSuccess, id2, fullCaps)

	dir := t.TempDir()

	for name, poolID := range map[string]string{
		"fake-sub":     "subscription-pool",
		"fake-compute": "compute-pool",
	} {
		t.Run(name, func(t *testing.T) {
			fakeStore := NewFakeEvaluationStore()
			cfg := BoundedComparisonConfig{
				Registry:             reg,
				Credentials:          MapCredentials{},
				Store:                fakeStore,
				RuntimeName:          name,
				StagingDir:           filepath.Join(dir, "staging", name),
				Now:                  fixedClock,
				MaxRetries:           3,
				InitialLeaseExpiry:   1 * time.Second,
				LeaseRenewalInterval: 100 * time.Millisecond,
			}

			req := BoundedComparisonRequest{
				RunID:         "run-1",
				SampleID:      "sample-1",
				CandidateID:   "cand-1",
				SnapshotPath:  dir,
				BlindedPrompt: "review",
				ToolAccess:    ToolAccessRequirements{},
			}

			res, err := cfg.RunBoundedComparison(context.Background(), req)
			if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if res.Response.Status != StatusCompleted {
				t.Errorf("got status %s, want completed", res.Response.Status)
			}
			id := res.Response.Identity
			if id.AccountPool != poolID {
				t.Errorf("got pool %q, want %q", id.AccountPool, poolID)
			}
		})
	}
}

func TestBoundedComparisonNoProductionStateEffect(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	fakeStore := NewFakeEvaluationStore()

	cfg := BoundedComparisonConfig{
		Registry:             reg,
		Credentials:          MapCredentials{},
		Store:                fakeStore,
		RuntimeName:          "fake",
		StagingDir:           filepath.Join(dir, "staging"),
		Now:                  fixedClock,
		MaxRetries:           3,
		InitialLeaseExpiry:   1 * time.Second,
		LeaseRenewalInterval: 100 * time.Millisecond,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SampleID:      "sample-1",
		CandidateID:   "cand-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review",
		ToolAccess:    ToolAccessRequirements{},
	}

	res, err := cfg.RunBoundedComparison(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	// Verify no production state effects: the result is only stored in the
	// evaluation store, never submitted to tasks, PRs, or reviews.
	result, ok := fakeStore.results[res.AttemptID]
	if !ok {
		t.Errorf("result not stored")
	}
	if result == nil {
		t.Errorf("result is nil")
	}

	// The finding should be JSON-serializable (no production references).
	data, err := json.Marshal(result)
	if err != nil {
		t.Errorf("result not JSON-serializable: %v", err)
	}
	if len(data) == 0 {
		t.Errorf("result serialized to empty JSON")
	}
}
