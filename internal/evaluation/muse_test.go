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
	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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
	if id.AccountPool != Unknown {
		t.Errorf("account_pool = %q, want unknown", id.AccountPool)
	}

	// Identity must be valid.
	if err := id.Validate(); err != nil {
		t.Fatalf("identity validation failed: %v", err)
	}
}

func TestMusePreflightMissingMuse(t *testing.T) {
	// Set PATH to exclude muse.
	t.Setenv("PATH", "/dev/null")

	stderr := &bytes.Buffer{}
	_, errClass, errMsg := musePreflight(stderr)

	// muse should not be found, should report runtime error.
	if errClass != ErrClassRuntimeError {
		t.Errorf("expected runtime error for missing muse, got error_class=%q", errClass)
	}
	if !strings.Contains(errMsg, "not installed") && !strings.Contains(errMsg, "executable file not found") {
		t.Errorf("unexpected error message: %v", errMsg)
	}
}

func TestMusePreflightAPIKeyDetection(t *testing.T) {
	// Test with API key set (simulating pay-as-you-go override).
	t.Setenv("META_API_KEY", "fake-key-12345")

	stderr := &bytes.Buffer{}
	missing, errClass, errMsg := musePreflight(stderr)

	// The preflight should ALWAYS reject an explicit META_API_KEY.
	if errClass != ErrClassAuthMissing {
		t.Errorf("expected auth_missing error for META_API_KEY override, got error_class=%q", errClass)
	}
	if !strings.Contains(errMsg, "META_API_KEY") {
		t.Errorf("expected error to mention META_API_KEY, got: %v", errMsg)
	}
	if missing != nil {
		t.Errorf("expected no missing capabilities on auth error, got %v", missing)
	}
}

func TestMuseMainPreflightMode(t *testing.T) {
	// Set up a temp directory with a fake muse executable and auth file.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "muse help"
elif [ "$1" = "exec" ] && [ "$2" = "--help" ]; then
  echo "usage: muse exec [options]"
  echo "Options:"
  echo "  --model <model>"
  echo "  --output-format <format>"
  echo "  --workspace <path>"
  echo "  --input <prompt>"
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
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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

	// Preflight-only mode with successful checks should exit 0 and write a response.
	if code != 0 {
		t.Errorf("MuseMain returned %d, want 0 (preflight success)", code)
	}

	// Result file should exist and contain the effective identity.
	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("expected result file for successful preflight, but got error: %v", err)
	}
	var resp CandidateResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("decode preflight response: %v", err)
	}
	if resp.Status != StatusCompleted {
		t.Errorf("preflight response status = %q, want completed", resp.Status)
	}
	if resp.ReviewCompleted {
		t.Errorf("preflight response review_completed = %v, want false", resp.ReviewCompleted)
	}
	if resp.Identity.RuntimeVersion == "" || resp.Identity.RuntimeVersion == Unknown {
		t.Errorf("preflight should record runtime version, got %q", resp.Identity.RuntimeVersion)
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
	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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
	// Set up a temp directory with a fake muse executable that outputs malformed JSON.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	// Fake muse that outputs invalid JSON when called with exec
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "muse help"
elif [ "$1" = "exec" ] && [ "$2" = "--help" ]; then
  echo "usage: muse exec [options]"
  echo "Options: --model, --output-format, --workspace, --input"
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
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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
	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// Clear auth env vars and set HOME to a directory without .muse/auth.
	t.Setenv("MUSE_AUTH", "")
	t.Setenv("MUSE_CONFIG", "")
	t.Setenv("HOME", dir) // dir has no .muse/auth subdirectory

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
	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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
	// Set up a temp directory with a fake muse executable.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

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

func TestMuseMainExitZeroNoCompletion(t *testing.T) {
	// This is the critical blocker: a muse that exits 0 but never emits
	// a completion/result event should fail, not be treated as successful.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	// Fake muse that outputs valid JSON lines but no completion event, then exits 0
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "muse help"
elif [ "$1" = "exec" ] && [ "$2" = "--help" ]; then
  echo "usage: muse exec [options]"
  echo "Options: --model, --output-format, --workspace, --input"
elif [ "$1" = "exec" ]; then
  echo '{"id":"f1","severity":"material","summary":"test finding"}'
  exit 0
else
  echo "muse-spark-1.3"
fi`
	if err := os.WriteFile(musePath, []byte(museSh), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}

	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, []byte("fake-auth-token"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-no-completion",
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
		t.Errorf("MuseMain returned %d, want 1 (no completion signal)", code)
	}

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
	if resp.ErrorClass != ErrClassOutputMissing {
		t.Errorf("error_class = %q, want output_missing", resp.ErrorClass)
	}
	if !strings.Contains(resp.ErrorMessage, "completion") {
		t.Errorf("error message should mention completion, got: %s", resp.ErrorMessage)
	}
}

func TestMuseMainAmbiguousAuth(t *testing.T) {
	// Test that preflight fails closed when multiple auth sources are configured.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	if err := os.WriteFile(musePath, []byte("#!/bin/sh\necho 'muse-spark-1.3'"), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	// Create both MUSE_AUTH and MUSE_CONFIG files.
	authPath := filepath.Join(dir, "auth")
	configPath := filepath.Join(dir, "config")
	os.WriteFile(authPath, []byte("fake-auth"), 0o600)
	os.WriteFile(configPath, []byte("fake-config"), 0o600)
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("MUSE_CONFIG", configPath)

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-ambiguous",
		SnapshotPath:  tmpDir,
		BlindedPrompt: "test",
		ToolAccess:    ToolAccessRequirements{},
		ResultPath:    resultPath,
	}

	data, _ := json.Marshal(req)
	os.WriteFile(reqPath, data, 0o600)

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath}, stderr)

	if code != 1 {
		t.Errorf("MuseMain returned %d, want 1 (ambiguous auth)", code)
	}

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
	if !strings.Contains(resp.ErrorMessage, "ambiguous") {
		t.Errorf("error message should mention ambiguous auth, got: %s", resp.ErrorMessage)
	}
}

func TestMuseMainCompletionEvent(t *testing.T) {
	// Test that a completion event is properly recognized.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "muse help"
elif [ "$1" = "exec" ] && [ "$2" = "--help" ]; then
  echo "usage: muse exec [options]"
  echo "Options: --model, --output-format, --workspace, --input"
elif [ "$1" = "exec" ]; then
  echo '{"id":"f1","severity":"material","summary":"test finding"}'
  echo '{"type":"completion"}'
  exit 0
else
  echo "muse-spark-1.3"
fi`
	if err := os.WriteFile(musePath, []byte(museSh), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}

	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, []byte("fake-auth-token"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-with-completion",
		SnapshotPath:  tmpDir,
		BlindedPrompt: "review this",
		ToolAccess:    ToolAccessRequirements{},
		ResultPath:    resultPath,
	}

	data, _ := json.Marshal(req)
	os.WriteFile(reqPath, data, 0o600)

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath}, stderr)

	if code != 0 {
		t.Errorf("MuseMain returned %d, want 0 (success with completion)", code)
	}

	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var resp CandidateResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != StatusCompleted {
		t.Errorf("response status = %q, want completed", resp.Status)
	}
	if !resp.ReviewCompleted {
		t.Errorf("review_completed = %v, want true", resp.ReviewCompleted)
	}
	if len(resp.Findings) != 1 {
		t.Errorf("findings count = %d, want 1", len(resp.Findings))
	} else if resp.Findings[0].ID != "f1" {
		t.Errorf("finding id = %q, want f1", resp.Findings[0].ID)
	}
}

func TestMuseMainMissingSummary(t *testing.T) {
	// Test that findings without a summary field are rejected.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "muse help"
elif [ "$1" = "exec" ] && [ "$2" = "--help" ]; then
  echo "usage: muse exec [options]"
  echo "Options: --model, --output-format, --workspace, --input"
elif [ "$1" = "exec" ]; then
  echo '{"id":"f1","severity":"material"}'
  echo '{"type":"completion"}'
  exit 0
else
  echo "muse-spark-1.3"
fi`
	if err := os.WriteFile(musePath, []byte(museSh), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}

	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, []byte("fake-auth-token"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-no-summary",
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
		t.Errorf("MuseMain returned %d, want 1 (missing summary)", code)
	}

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
	if !strings.Contains(resp.ErrorMessage, "summary") {
		t.Errorf("error message should mention summary, got: %s", resp.ErrorMessage)
	}
}

func TestMuseMainInvalidSeverity(t *testing.T) {
	// Test that findings with invalid severity values are rejected.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "muse help"
elif [ "$1" = "exec" ] && [ "$2" = "--help" ]; then
  echo "usage: muse exec [options]"
  echo "Options: --model, --output-format, --workspace, --input"
elif [ "$1" = "exec" ]; then
  echo '{"id":"f1","severity":"unknown-severity","summary":"test"}'
  echo '{"type":"completion"}'
  exit 0
else
  echo "muse-spark-1.3"
fi`
	if err := os.WriteFile(musePath, []byte(museSh), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}

	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, []byte("fake-auth-token"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-invalid-severity",
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
		t.Errorf("MuseMain returned %d, want 1 (invalid severity)", code)
	}

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
	if !strings.Contains(resp.ErrorMessage, "severity") {
		t.Errorf("error message should mention severity, got: %s", resp.ErrorMessage)
	}
}

func TestMuseMainIncompatibleFlags(t *testing.T) {
	// Test that preflight fails when muse exec doesn't support required flags.
	dir := t.TempDir()
	musePath := filepath.Join(dir, "muse")
	// Fake muse whose exec --help doesn't mention --model, --output-format, etc.
	museSh := `#!/bin/sh
if [ "$1" = "--help" ]; then
  echo "usage: muse [command]"
  echo "Commands: run"
elif [ "$1" = "exec" ] && [ "$2" = "--help" ]; then
  echo "usage: muse exec [options]"
  echo "Options: --timeout <seconds>"
else
  echo "muse-spark-1.3"
fi`
	if err := os.WriteFile(musePath, []byte(museSh), 0o755); err != nil {
		t.Fatalf("write fake muse: %v", err)
	}

	authPath := filepath.Join(dir, "auth")
	if err := os.WriteFile(authPath, []byte("fake-auth-token"), 0o600); err != nil {
		t.Fatalf("write auth file: %v", err)
	}
	t.Setenv("MUSE_AUTH", authPath)
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	tmpDir := t.TempDir()
	reqPath := filepath.Join(tmpDir, "request.json")
	resultPath := filepath.Join(tmpDir, "result.json")

	req := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-incompatible",
		SnapshotPath:  tmpDir,
		BlindedPrompt: "test",
		ToolAccess:    ToolAccessRequirements{},
		ResultPath:    resultPath,
	}

	data, _ := json.Marshal(req)
	os.WriteFile(reqPath, data, 0o600)

	stderr := &bytes.Buffer{}
	code := MuseMain([]string{"--request", reqPath}, stderr)

	if code != 1 {
		t.Errorf("MuseMain returned %d, want 1 (incompatible flags)", code)
	}

	result, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	var resp CandidateResponse
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Status != StatusUnsupported {
		t.Errorf("response status = %q, want unsupported", resp.Status)
	}
	if resp.ErrorClass != ErrClassCapabilityMissing {
		t.Errorf("error_class = %q, want capability_missing", resp.ErrorClass)
	}
	if len(resp.MissingCapabilities) == 0 {
		t.Errorf("expected missing capabilities to be listed")
	}
}
