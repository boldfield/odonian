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
	// Save original env and PATH.
	oldPath := os.Getenv("PATH")
	defer func() {
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
	}()

	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	os.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	id, err := buildMuseIdentity()
	if err != nil {
		t.Fatalf("buildMuseIdentity failed: %v", err)
	}

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
	if id.ModelRevision != MuseVersion {
		t.Errorf("model_revision = %q, want %q", id.ModelRevision, MuseVersion)
	}
	if id.RuntimeName == "" || id.RuntimeName == Unknown {
		t.Errorf("runtime_name = %q, want non-empty", id.RuntimeName)
	}
	if id.RuntimeVersion == "" || id.RuntimeVersion == Unknown {
		t.Errorf("runtime_version = %q, want non-empty", id.RuntimeVersion)
	}
	if id.PromptVersion != Unknown {
		t.Errorf("prompt_version = %q, want unknown", id.PromptVersion)
	}
	if id.Tools.Known {
		t.Errorf("tools should be unknown, got known=%v", id.Tools.Known)
	}
	if id.Observers.Known {
		t.Errorf("observers should be unknown, got known=%v", id.Observers.Known)
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
	// Save original PATH.
	oldPath := os.Getenv("PATH")
	defer func() {
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
	}()

	// Set PATH to exclude muse.
	os.Setenv("PATH", "/dev/null")

	stderr := &bytes.Buffer{}
	_, err := musePreflight(stderr)

	// muse should not be found.
	if err == nil {
		t.Error("expected error for missing muse, got none")
	}
	if !strings.Contains(err.Error(), "not installed") && !strings.Contains(err.Error(), "executable file not found") {
		t.Errorf("unexpected error: %v", err)
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

	stderr := &bytes.Buffer{}
	missing, err := musePreflight(stderr)

	// The preflight should ALWAYS reject an explicit META_API_KEY.
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
	// Save original PATH and MUSE_AUTH.
	oldPath := os.Getenv("PATH")
	oldAuth := os.Getenv("MUSE_AUTH")
	defer func() {
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
		if oldAuth != "" {
			os.Setenv("MUSE_AUTH", oldAuth)
		} else {
			os.Unsetenv("MUSE_AUTH")
		}
	}()

	// Set up a temp directory with a fake muse executable and auth file.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\nif [ \"$1\" = '--help' ]; then echo 'muse help'; fi\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}

	// Create a fake auth file.
	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, []byte("fake-auth-token"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	os.Setenv("MUSE_AUTH", authPath)
	os.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-muse-1",
		SnapshotPath:  tmpDir,
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

	// Preflight-only mode with successful checks should exit 0 and not write a response.
	if code != 0 {
		t.Errorf("MuseMain returned %d, want 0 (preflight success)", code)
	}

	// Result file should not exist for successful preflight.
	if _, err := os.Stat(resultPath); err == nil {
		t.Errorf("expected no result file for successful preflight, but file exists")
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
	// Save original PATH.
	oldPath := os.Getenv("PATH")
	defer func() {
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
	}()

	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	os.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	// Result path in a non-existent directory that can't be created.
	resultPath := filepath.Join(tmpDir, "subdir", "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-1",
		SnapshotPath:  tmpDir,
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

func TestMuseMainMalformedOutput(t *testing.T) {
	// Save original PATH and MUSE_AUTH.
	oldPath := os.Getenv("PATH")
	oldAuth := os.Getenv("MUSE_AUTH")
	defer func() {
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
		if oldAuth != "" {
			os.Setenv("MUSE_AUTH", oldAuth)
		} else {
			os.Unsetenv("MUSE_AUTH")
		}
	}()

	// Set up a temp directory with a fake muse executable that outputs malformed JSON.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	// Fake muse that outputs invalid JSON when called with exec
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "muse help"
elif [ "$1" = "exec" ]; then
  echo '{"invalid json'
else
  echo "muse-spark-1.3"
fi`
	if err := os.WriteFile(musePath, []byte(museSh), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}

	// Create a fake auth file.
	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, []byte("fake-auth-token"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	os.Setenv("MUSE_AUTH", authPath)
	os.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-malformed",
		SnapshotPath:  tmpDir,
		BlindedPrompt: "review this",
		ToolAccess:    ToolAccessRequirements{},
		ResultPath:    resultPath,
	}

	data, _ := json.Marshal(req)
	os.WriteFile(reqPath, data, 0o600)

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath}, stderr)

	if code != 1 {
		t.Errorf("MuseMain returned %d, want 1 (malformed output)", code)
	}

	// Check that a response was written with error status.
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var resp CandidateResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != StatusFailed {
		t.Errorf("response status = %q, want failed", resp.Status)
	}
	if resp.ErrorClass != ErrClassOutputMalformed {
		t.Errorf("error_class = %q, want output_malformed", resp.ErrorClass)
	}
}

func TestMuseMainAuthFailure(t *testing.T) {
	// Save original env.
	oldAuth := os.Getenv("MUSE_AUTH")
	oldConfig := os.Getenv("MUSE_CONFIG")
	oldPath := os.Getenv("PATH")
	oldHome := os.Getenv("HOME")

	defer func() {
		if oldAuth != "" {
			os.Setenv("MUSE_AUTH", oldAuth)
		} else {
			os.Unsetenv("MUSE_AUTH")
		}
		if oldConfig != "" {
			os.Setenv("MUSE_CONFIG", oldConfig)
		} else {
			os.Unsetenv("MUSE_CONFIG")
		}
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
		if oldHome != "" {
			os.Setenv("HOME", oldHome)
		}
	}()

	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	os.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// Clear auth env vars and set HOME to a directory without .muse/auth.
	os.Unsetenv("MUSE_AUTH")
	os.Unsetenv("MUSE_CONFIG")
	os.Setenv("HOME", dir) // dir has no .muse/auth subdirectory

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-auth",
		SnapshotPath:  tmpDir,
		BlindedPrompt: "review this",
		ToolAccess:    ToolAccessRequirements{},
		ResultPath:    resultPath,
	}

	data, _ := json.Marshal(req)
	os.WriteFile(reqPath, data, 0o600)

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath}, stderr)

	if code != 1 {
		t.Errorf("MuseMain returned %d, want 1 (auth failure)", code)
	}

	// Check that a response was written with auth error.
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var resp CandidateResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != StatusFailed {
		t.Errorf("response status = %q, want failed", resp.Status)
	}
	if resp.ErrorClass != ErrClassAuthMissing {
		t.Errorf("error_class = %q, want auth_missing", resp.ErrorClass)
	}
}

func TestMuseIdentityDigest(t *testing.T) {
	// Save original PATH.
	oldPath := os.Getenv("PATH")
	defer func() {
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
	}()

	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	os.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	id, err := buildMuseIdentity()
	if err != nil {
		t.Fatalf("buildMuseIdentity failed: %v", err)
	}

	digest1 := id.Digest()

	// Same identity should have same digest.
	id2, _ := buildMuseIdentity()
	digest2 := id2.Digest()
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
	// Save original PATH.
	oldPath := os.Getenv("PATH")
	defer func() {
		if oldPath != "" {
			os.Setenv("PATH", oldPath)
		}
	}()

	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	os.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	id, err := buildMuseIdentity()
	if err != nil {
		t.Fatalf("buildMuseIdentity failed: %v", err)
	}

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
