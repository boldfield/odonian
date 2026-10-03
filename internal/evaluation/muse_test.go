package evaluation

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMuseIdentity(t *testing.T) {
	id := buildMuseIdentity()

	// Verify all required fields are set and not Unknown.
	if id.AdapterName != "muse" {
		t.Errorf("adapter_name = %q, want muse", id.AdapterName)
	}
	if id.AdapterVersion == "" || id.AdapterVersion == Unknown {
		t.Errorf("adapter_version = %q, want non-empty", id.AdapterVersion)
	}
	if id.ModelID != "muse-code" {
		t.Errorf("model_id = %q, want muse-code", id.ModelID)
	}
	if !strings.Contains(id.ModelRevision, "spark") {
		t.Errorf("model_revision = %q, want to contain spark", id.ModelRevision)
	}
	if id.RuntimeName == "" || id.RuntimeName == Unknown {
		t.Errorf("runtime_name = %q, want non-empty", id.RuntimeName)
	}
	if !strings.Contains(id.RuntimeVersion, "spark") {
		t.Errorf("runtime_version = %q, want to contain spark", id.RuntimeVersion)
	}
	if id.AccountPool != "meta-power" {
		t.Errorf("account_pool = %q, want meta-power", id.AccountPool)
	}

	// Identity must be valid.
	if err := id.Validate(); err != nil {
		t.Fatalf("identity validation failed: %v", err)
	}
}

func TestMusePreflightMissingMuse(t *testing.T) {
	id := buildMuseIdentity()
	stderr := &bytes.Buffer{}

	missing, err := musePreflight(stderr, &id)

	// If muse is not installed, we expect an error mentioning "not installed" or "executable file not found".
	// If muse is installed, we expect no error and no missing capabilities.
	if err != nil {
		if !strings.Contains(err.Error(), "not installed") && !strings.Contains(err.Error(), "executable file not found") {
			t.Errorf("unexpected error: %v", err)
		}
		// Expected behavior in test environments where muse is not installed.
		return
	}

	if missing != nil && len(missing) > 0 {
		t.Errorf("expected no missing capabilities, got %v", missing)
	}
}

func TestMusePreflightAPIKeyDetection(t *testing.T) {
	// Save original env.
	oldKey := os.Getenv("META_API_KEY")
	defer func() {
		if oldKey != "" {
			os.Setenv("META_API_KEY", oldKey)
		} else {
			os.Unsetenv("META_API_KEY")
		}
	}()

	// Test with API key set (simulating pay-as-you-go override).
	os.Setenv("META_API_KEY", "fake-key-12345")

	id := buildMuseIdentity()
	stderr := &bytes.Buffer{}
	missing, err := musePreflight(stderr, &id)

	// The preflight should ALWAYS reject an explicit META_API_KEY, even if muse is available.
	if err == nil {
		t.Error("expected error for META_API_KEY override, got none")
	}
	if !strings.Contains(err.Error(), "META_API_KEY") {
		t.Errorf("expected error to mention META_API_KEY, got: %v", err)
	}
	if missing != nil {
		t.Errorf("expected no capabilities returned on auth error, got %v", missing)
	}
}

func TestMuseMainPreflightMode(t *testing.T) {
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "request.json")
	resultPath := filepath.Join(dir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-muse-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review this code",
		ToolAccess:    ToolAccessRequirements{RequireSourceRetrieval: true},
		ResultPath:    resultPath,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := os.WriteFile(reqPath, data, 0o600); err != nil {
		t.Fatalf("write request: %v", err)
	}

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath, "--preflight"}, stderr)

	// Always read and validate the response.
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var resp CandidateResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("response validation failed: %v", err)
	}
	if resp.RunID != req.RunID {
		t.Errorf("response run_id = %q, want %q", resp.RunID, req.RunID)
	}

	if code == 0 {
		// muse is installed; check for success.
		if resp.Status != StatusCompleted {
			t.Errorf("response status = %q, want completed", resp.Status)
		}
		if !resp.ReviewCompleted {
			t.Errorf("response review_completed = %v, want true", resp.ReviewCompleted)
		}
	} else if code == 1 {
		// muse not installed is OK; check for failed status.
		if resp.Status != StatusFailed {
			t.Errorf("response status = %q, want failed", resp.Status)
		}
		if strings.Contains(resp.ErrorMessage, "not installed") || strings.Contains(resp.ErrorMessage, "executable file not found") {
			// Expected error message.
		} else {
			t.Errorf("expected 'not installed' in error message, got: %s", resp.ErrorMessage)
		}
	} else {
		t.Fatalf("unexpected exit code %d", code)
	}
}

func TestMuseMainMissingRequest(t *testing.T) {
	stderr := &bytes.Buffer{}
	code := MuseMain([]string{}, stderr)

	if code != 2 {
		t.Errorf("MuseMain returned %d, want 2 (usage error)", code)
	}
	if !strings.Contains(stderr.String(), "request") {
		t.Errorf("stderr should mention --request, got: %s", stderr)
	}
}

func TestMuseMainInvalidRequest(t *testing.T) {
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "request.json")

	// Write invalid JSON.
	if err := os.WriteFile(reqPath, []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("write invalid request: %v", err)
	}

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath}, stderr)

	if code != 2 {
		t.Errorf("MuseMain returned %d, want 2 (decode error)", code)
	}
	if !strings.Contains(stderr.String(), "decode") {
		t.Errorf("stderr should mention decode error, got: %s", stderr)
	}
}

func TestMuseMainMissingResultDir(t *testing.T) {
	dir := t.TempDir()
	reqPath := filepath.Join(dir, "request.json")
	// Result path in a non-existent directory that can't be created.
	resultPath := filepath.Join(dir, "subdir", "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-1",
		SnapshotPath:  dir,
		BlindedPrompt: "test",
		ToolAccess:    ToolAccessRequirements{},
		ResultPath:    resultPath,
	}

	data, _ := json.Marshal(req)
	os.WriteFile(reqPath, data, 0o600)

	// Create subdir so the write can succeed, but make it read-only after.
	subdir := filepath.Dir(resultPath)
	os.MkdirAll(subdir, 0o700)
	os.Chmod(subdir, 0o500) // Read-only

	defer os.Chmod(subdir, 0o700) // Restore for cleanup

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath}, stderr)

	// Should fail to write result.
	if code != 2 {
		t.Errorf("MuseMain returned %d, expected 2 (write error)", code)
	}
}

func TestMuseIdentityDigest(t *testing.T) {
	id := buildMuseIdentity()
	digest1 := id.Digest()

	// Same identity should have same digest.
	digest2 := id.Digest()
	if digest1 != digest2 {
		t.Errorf("digest mismatch for same identity: %s vs %s", digest1, digest2)
	}

	// Digest should be deterministic and non-empty.
	if digest1 == "" {
		t.Error("digest is empty")
	}
	if len(digest1) != 64 { // SHA256 hex.
		t.Errorf("digest length = %d, want 64", len(digest1))
	}
}

func TestMuseResponseValidation(t *testing.T) {
	id := buildMuseIdentity()
	base := CandidateResponse{
		Version:         ProtocolVersion,
		RunID:           "run-1",
		Identity:        id,
		Status:          StatusCompleted,
		ReviewCompleted: true,
		Findings: []Finding{
			{ID: "f1", Severity: SeverityMaterial, Summary: "test"},
		},
	}

	if err := base.Validate(); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
}
