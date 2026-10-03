package evaluation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFakeAdapterSuccessMode(t *testing.T) {
	tmpDir := t.TempDir()
	requestPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:         AdapterVersion,
		RunID:           "test-success-1",
		TaskID:          "task-123",
		ReviewRound:     1,
		SubmittedCommit: "abc123",
		SnapshotPath:    "/tmp/snapshot",
		BlindedPrompt:   "Review this code",
		ToolAccess: ToolAccessRequirements{
			RequireSourceRetrieval: true,
			DeclaredTools:          []string{"bash"},
		},
		ResultPath: resultPath,
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request failed: %v", err)
	}
	if err := os.WriteFile(requestPath, reqData, 0644); err != nil {
		t.Fatalf("write request failed: %v", err)
	}

	cfg := FakeAdapterConfig{
		Mode:           FakeModeSuccess,
		CandidateName:  "test_adapter",
		ModelID:        "test-model-1.0",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		FindingCount:   2,
	}

	if err := ExecuteFakeAdapter(requestPath, resultPath, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	resultData, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result failed: %v", err)
	}

	var response CandidateResponse
	if err := json.Unmarshal(resultData, &response); err != nil {
		t.Fatalf("unmarshal result failed: %v", err)
	}

	if err := response.Validate(); err != nil {
		t.Fatalf("response validation failed: %v", err)
	}

	if response.Status != "completed" {
		t.Errorf("expected completed, got %s", response.Status)
	}
	if !response.ReviewCompleted {
		t.Error("review should be completed")
	}
	if len(response.Findings) != 2 {
		t.Errorf("expected 2 findings, got %d", len(response.Findings))
	}
	if response.EffectiveCandidate.ModelID != cfg.ModelID {
		t.Error("model ID not preserved")
	}
	if response.EffectiveCandidate.AdapterName != cfg.CandidateName {
		t.Error("adapter name not preserved")
	}
}

func TestFakeAdapterMalformedMode(t *testing.T) {
	tmpDir := t.TempDir()
	requestPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:      AdapterVersion,
		RunID:        "test-malformed-1",
		TaskID:       "task-123",
		ReviewRound:  1,
		SnapshotPath: "/tmp/snapshot",
		ResultPath:   resultPath,
	}

	reqData, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request failed: %v", err)
	}
	if err := os.WriteFile(requestPath, reqData, 0644); err != nil {
		t.Fatalf("write request failed: %v", err)
	}

	cfg := FakeAdapterConfig{
		Mode:            FakeModeMalformed,
		CandidateName:   "test_adapter",
		ModelID:         "test-model-1.0",
		RuntimeVersion:  "1.0",
		PromptVersion:   "v1",
		MalformedReason: "Adapter produced invalid JSON",
	}

	if err := ExecuteFakeAdapter(requestPath, resultPath, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	resultData, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result failed: %v", err)
	}

	// The result should be genuinely invalid JSON
	var response CandidateResponse
	if err := json.Unmarshal(resultData, &response); err == nil {
		t.Error("malformed output should not parse as valid JSON")
	}
}

func TestFakeAdapterUnsupportedMode(t *testing.T) {
	tmpDir := t.TempDir()
	requestPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:      AdapterVersion,
		RunID:        "test-unsupported-1",
		TaskID:       "task-123",
		ReviewRound:  1,
		SnapshotPath: "/tmp/snapshot",
		ToolAccess: ToolAccessRequirements{
			RequirePDFSupport: true,
		},
		ResultPath: resultPath,
	}

	reqData, _ := json.Marshal(req)
	os.WriteFile(requestPath, reqData, 0644)

	cfg := FakeAdapterConfig{
		Mode:           FakeModeUnsupported,
		CandidateName:  "test_adapter",
		ModelID:        "test-model-1.0",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		ErrorMessage:   "PDF parsing not supported in this runtime",
	}

	if err := ExecuteFakeAdapter(requestPath, resultPath, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	resultData, _ := os.ReadFile(resultPath)
	var response CandidateResponse
	json.Unmarshal(resultData, &response)

	if response.Status != "unsupported" {
		t.Errorf("expected unsupported status, got %s", response.Status)
	}
	if response.ReviewCompleted {
		t.Error("review should not be completed")
	}
	if response.ErrorClass == nil || *response.ErrorClass != "capability_unsupported" {
		t.Error("error class should be capability_unsupported")
	}
}

func TestFakeAdapterInterruptedMode(t *testing.T) {
	tmpDir := t.TempDir()
	requestPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:      AdapterVersion,
		RunID:        "test-interrupted-1",
		TaskID:       "task-123",
		ReviewRound:  1,
		SnapshotPath: "/tmp/snapshot",
		ResultPath:   resultPath,
	}

	reqData, _ := json.Marshal(req)
	os.WriteFile(requestPath, reqData, 0644)

	cfg := FakeAdapterConfig{
		Mode:           FakeModeInterrupted,
		CandidateName:  "test_adapter",
		ModelID:        "test-model-1.0",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
	}

	if err := ExecuteFakeAdapter(requestPath, resultPath, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	resultData, _ := os.ReadFile(resultPath)
	var response CandidateResponse
	json.Unmarshal(resultData, &response)

	if response.Status != "incomplete" {
		t.Errorf("expected incomplete status, got %s", response.Status)
	}
	if response.ErrorClass == nil || *response.ErrorClass != "timeout" {
		t.Error("error class should be timeout")
	}
}

func TestFakeAdapterFailedMode(t *testing.T) {
	tmpDir := t.TempDir()
	requestPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:      AdapterVersion,
		RunID:        "test-failed-1",
		TaskID:       "task-123",
		ReviewRound:  1,
		SnapshotPath: "/tmp/snapshot",
		ResultPath:   resultPath,
	}

	reqData, _ := json.Marshal(req)
	os.WriteFile(requestPath, reqData, 0644)

	cfg := FakeAdapterConfig{
		Mode:           FakeModeExecutionFail,
		CandidateName:  "test_adapter",
		ModelID:        "test-model-1.0",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		ErrorMessage:   "Authentication failed",
	}

	if err := ExecuteFakeAdapter(requestPath, resultPath, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	resultData, _ := os.ReadFile(resultPath)
	var response CandidateResponse
	json.Unmarshal(resultData, &response)

	if response.Status != "failed" {
		t.Errorf("expected failed status, got %s", response.Status)
	}
	if response.ErrorClass == nil || *response.ErrorClass != "execution_error" {
		t.Error("error class should be execution_error")
	}
}

func TestFakeAdapterDeterministicExecution(t *testing.T) {
	tmpDir := t.TempDir()

	req := CandidateRequest{
		Version:         AdapterVersion,
		RunID:           "test-deterministic-1",
		TaskID:          "task-123",
		ReviewRound:     1,
		SubmittedCommit: "abc123",
		SnapshotPath:    "/tmp/snapshot",
		BlindedPrompt:   "Review this code",
		ResultPath:      filepath.Join(tmpDir, "result1.json"),
	}

	cfg := FakeAdapterConfig{
		Mode:           FakeModeSuccess,
		CandidateName:  "test_adapter",
		ModelID:        "test-model-1.0",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		FindingCount:   2,
		ResponseDelay:  0,
	}

	// Run 1
	req1Data, _ := json.Marshal(req)
	req1Path := filepath.Join(tmpDir, "request1.json")
	result1Path := filepath.Join(tmpDir, "result1.json")
	os.WriteFile(req1Path, req1Data, 0644)

	if err := ExecuteFakeAdapter(req1Path, result1Path, cfg); err != nil {
		t.Fatalf("first execution failed: %v", err)
	}

	result1Data, _ := os.ReadFile(result1Path)
	var response1 CandidateResponse
	json.Unmarshal(result1Data, &response1)

	// Run 2 with same config
	req.RunID = "test-deterministic-1" // Same RunID
	req2Data, _ := json.Marshal(req)
	req2Path := filepath.Join(tmpDir, "request2.json")
	result2Path := filepath.Join(tmpDir, "result2.json")
	req.ResultPath = result2Path
	req2Data, _ = json.Marshal(req)
	os.WriteFile(req2Path, req2Data, 0644)

	if err := ExecuteFakeAdapter(req2Path, result2Path, cfg); err != nil {
		t.Fatalf("second execution failed: %v", err)
	}

	result2Data, _ := os.ReadFile(result2Path)
	var response2 CandidateResponse
	json.Unmarshal(result2Data, &response2)

	// Verify determinism: same findings count, status, etc.
	if response1.Status != response2.Status {
		t.Error("status should match across deterministic runs")
	}
	if len(response1.Findings) != len(response2.Findings) {
		t.Error("findings count should match across deterministic runs")
	}
	if response1.EffectiveCandidate.ModelID != response2.EffectiveCandidate.ModelID {
		t.Error("model ID should match across deterministic runs")
	}
}

func TestTwoCandidateConfigurationsSameRequestPipeline(t *testing.T) {
	tmpDir := t.TempDir()

	// Create shared request for both candidates
	req := CandidateRequest{
		Version:         AdapterVersion,
		RunID:           "shared-run-id",
		TaskID:          "task-shared",
		ReviewRound:     1,
		SubmittedCommit: "shared-commit-abc",
		SnapshotPath:    "/tmp/shared-snapshot",
		BlindedPrompt:   "Review this code",
		ToolAccess: ToolAccessRequirements{
			RequireSourceRetrieval: true,
			DeclaredTools:          []string{"bash", "python"},
		},
	}

	// Candidate 1: Muse model
	candidate1Config := FakeAdapterConfig{
		Mode:           FakeModeSuccess,
		CandidateName:  "muse_code",
		ModelID:        "muse-spark-1.3",
		RuntimeVersion: "1.0.0",
		PromptVersion:  "muse-v1",
		FindingCount:   3,
	}

	req1Path := filepath.Join(tmpDir, "request_candidate1.json")
	result1Path := filepath.Join(tmpDir, "result_candidate1.json")
	req.ResultPath = result1Path
	req1Data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request 1 failed: %v", err)
	}
	if err := os.WriteFile(req1Path, req1Data, 0644); err != nil {
		t.Fatalf("write request 1 failed: %v", err)
	}

	// Use the shared host pipeline for candidate 1
	pipeline1 := &HostRequestPipeline{
		RequestPath: req1Path,
		ResultPath:  result1Path,
		Config:      candidate1Config,
	}
	response1, err := pipeline1.Execute()
	if err != nil {
		t.Fatalf("candidate 1 pipeline failed: %v", err)
	}

	// Candidate 2: Pi model
	candidate2Config := FakeAdapterConfig{
		Mode:           FakeModeSuccess,
		CandidateName:  "pi_spark",
		ModelID:        "pi-2024-q4",
		RuntimeVersion: "2.0.0",
		PromptVersion:  "pi-v1",
		FindingCount:   3,
	}

	req2Path := filepath.Join(tmpDir, "request_candidate2.json")
	result2Path := filepath.Join(tmpDir, "result_candidate2.json")
	req.ResultPath = result2Path
	req2Data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request 2 failed: %v", err)
	}
	if err := os.WriteFile(req2Path, req2Data, 0644); err != nil {
		t.Fatalf("write request 2 failed: %v", err)
	}

	// Use the shared host pipeline for candidate 2
	pipeline2 := &HostRequestPipeline{
		RequestPath: req2Path,
		ResultPath:  result2Path,
		Config:      candidate2Config,
	}
	response2, err := pipeline2.Execute()
	if err != nil {
		t.Fatalf("candidate 2 pipeline failed: %v", err)
	}

	// Both should be completed
	if response1.Status != "completed" {
		t.Errorf("candidate 1 status: %s", response1.Status)
	}
	if response2.Status != "completed" {
		t.Errorf("candidate 2 status: %s", response2.Status)
	}

	// Both should process the same number of findings
	if len(response1.Findings) != len(response2.Findings) {
		t.Errorf("finding counts differ: %d vs %d", len(response1.Findings), len(response2.Findings))
	}

	// Both should validate successfully
	if err := response1.Validate(); err != nil {
		t.Errorf("candidate 1 validation failed: %v", err)
	}
	if err := response2.Validate(); err != nil {
		t.Errorf("candidate 2 validation failed: %v", err)
	}

	// But have different identities
	if response1.EffectiveCandidate.AdapterName == response2.EffectiveCandidate.AdapterName {
		t.Error("adapter names should differ for different candidates")
	}
	if response1.EffectiveCandidate.ModelID == response2.EffectiveCandidate.ModelID {
		t.Error("model IDs should differ for different candidates")
	}

	// Verify request was preserved in both
	if response1.EffectiveCandidate.PromptVersion == "" {
		t.Error("candidate 1 prompt version should be set")
	}
	if response2.EffectiveCandidate.PromptVersion == "" {
		t.Error("candidate 2 prompt version should be set")
	}
}

func TestFakeAdapterResponseDelay(t *testing.T) {
	tmpDir := t.TempDir()
	requestPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:      AdapterVersion,
		RunID:        "test-delay-1",
		TaskID:       "task-123",
		ReviewRound:  1,
		SnapshotPath: "/tmp/snapshot",
		ResultPath:   resultPath,
	}

	reqData, _ := json.Marshal(req)
	os.WriteFile(requestPath, reqData, 0644)

	cfg := FakeAdapterConfig{
		Mode:           FakeModeSuccess,
		CandidateName:  "test_adapter",
		ModelID:        "test-model-1.0",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		ResponseDelay:  100 * time.Millisecond,
	}

	start := time.Now()
	if err := ExecuteFakeAdapter(requestPath, resultPath, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}
	duration := time.Since(start)

	if duration < cfg.ResponseDelay {
		t.Errorf("expected at least %v delay, got %v", cfg.ResponseDelay, duration)
	}

	resultData, _ := os.ReadFile(resultPath)
	var response CandidateResponse
	json.Unmarshal(resultData, &response)

	// Verify timing was recorded
	if response.Timing.Duration <= 0 {
		t.Error("response timing should be positive")
	}
}

func TestFakeAdapterFindingDetails(t *testing.T) {
	tmpDir := t.TempDir()
	requestPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:      AdapterVersion,
		RunID:        "test-findings-1",
		TaskID:       "task-123",
		ReviewRound:  1,
		SnapshotPath: "/tmp/snapshot",
		ResultPath:   resultPath,
	}

	reqData, _ := json.Marshal(req)
	os.WriteFile(requestPath, reqData, 0644)

	cfg := FakeAdapterConfig{
		Mode:           FakeModeSuccess,
		CandidateName:  "test_adapter",
		ModelID:        "test-model-1.0",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		FindingCount:   3,
	}

	if err := ExecuteFakeAdapter(requestPath, resultPath, cfg); err != nil {
		t.Fatalf("execution failed: %v", err)
	}

	resultData, _ := os.ReadFile(resultPath)
	var response CandidateResponse
	json.Unmarshal(resultData, &response)

	for i, finding := range response.Findings {
		if finding.Severity == "" {
			t.Errorf("finding %d missing severity", i)
		}
		if finding.Category == "" {
			t.Errorf("finding %d missing category", i)
		}
		if finding.Summary == "" {
			t.Errorf("finding %d missing summary", i)
		}
		if finding.File == "" {
			t.Errorf("finding %d missing file", i)
		}
	}

	// First finding should be major severity
	if response.Findings[0].Severity != "major" {
		t.Error("first finding should be major severity")
	}

	// Others should be minor
	for i := 1; i < len(response.Findings); i++ {
		if response.Findings[i].Severity != "minor" {
			t.Errorf("finding %d should be minor severity", i)
		}
	}
}
