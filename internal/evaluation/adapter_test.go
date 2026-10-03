package evaluation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCandidateIdentityDigestConsistency(t *testing.T) {
	identity := CandidateIdentity{
		AdapterName:    "fake_adapter",
		AdapterVersion: "1.0",
		ModelID:        "test-model-1.0",
		RuntimeName:    "test_runtime",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		AccountOrPool:  "test_pool",
	}

	digest1, err := identity.Digest()
	if err != nil {
		t.Fatalf("first digest failed: %v", err)
	}

	digest2, err := identity.Digest()
	if err != nil {
		t.Fatalf("second digest failed: %v", err)
	}

	if digest1 != digest2 {
		t.Errorf("digest not deterministic: %s vs %s", digest1, digest2)
	}
}

func TestCandidateIdentityDigestChange(t *testing.T) {
	baseIdentity := CandidateIdentity{
		AdapterName:    "fake_adapter",
		AdapterVersion: "1.0",
		ModelID:        "test-model-1.0",
		RuntimeName:    "test_runtime",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		AccountOrPool:  "test_pool",
	}

	digest1, err := baseIdentity.Digest()
	if err != nil {
		t.Fatalf("digest failed: %v", err)
	}

	// Change ModelID
	modifiedIdentity := baseIdentity
	modifiedIdentity.ModelID = "test-model-2.0"

	digest2, err := modifiedIdentity.Digest()
	if err != nil {
		t.Fatalf("digest failed: %v", err)
	}

	if digest1 == digest2 {
		t.Error("digest should change when identity changes")
	}
}

func TestCandidateIdentityDigestImmutable(t *testing.T) {
	identity := CandidateIdentity{
		AdapterName:    "test_adapter",
		AdapterVersion: "1.0",
		ModelID:        "model-v1",
		RuntimeName:    "runtime",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		AccountOrPool:  "pool",
		ReasoningSettings: map[string]interface{}{
			"temperature": 0.7,
		},
		GenerationSettings: map[string]interface{}{
			"max_tokens": 2048,
		},
	}

	digest1, _ := identity.Digest()
	digest2, _ := identity.Digest()

	if digest1 != digest2 {
		t.Error("digest must be deterministic across multiple calls")
	}

	// Verify digest is reproducible with the same data
	identityCopy := identity
	digest3, _ := identityCopy.Digest()
	if digest1 != digest3 {
		t.Error("digest must match for equivalent identity objects")
	}
}

func TestCandidateIdentityDigestWithSettingsChange(t *testing.T) {
	baseIdentity := CandidateIdentity{
		AdapterName:    "test_adapter",
		AdapterVersion: "1.0",
		ModelID:        "model-v1",
		RuntimeName:    "runtime",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		AccountOrPool:  "pool",
		ReasoningSettings: map[string]interface{}{
			"temperature": 0.7,
		},
	}

	digest1, err := baseIdentity.Digest()
	if err != nil {
		t.Fatalf("digest failed: %v", err)
	}

	// Change reasoning settings by creating a new map
	modifiedIdentity := CandidateIdentity{
		AdapterName:    "test_adapter",
		AdapterVersion: "1.0",
		ModelID:        "model-v1",
		RuntimeName:    "runtime",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		AccountOrPool:  "pool",
		ReasoningSettings: map[string]interface{}{
			"temperature": 0.9,
		},
	}

	digest2, err := modifiedIdentity.Digest()
	if err != nil {
		t.Fatalf("digest failed: %v", err)
	}

	if digest1 == digest2 {
		t.Error("digest should change when reasoning settings change")
	}
}

func TestCandidateIdentityDigestWithToolConfigChange(t *testing.T) {
	baseIdentity := CandidateIdentity{
		AdapterName:    "test_adapter",
		AdapterVersion: "1.0",
		ModelID:        "model-v1",
		RuntimeName:    "runtime",
		RuntimeVersion: "1.0",
		PromptVersion:  "v1",
		AccountOrPool:  "pool",
		ToolConfiguration: []ToolConfig{
			{Name: "bash", Version: "5.1"},
		},
	}

	digest1, _ := baseIdentity.Digest()

	// Change tool configuration
	modifiedIdentity := baseIdentity
	modifiedIdentity.ToolConfiguration = []ToolConfig{
		{Name: "bash", Version: "5.2"},
	}

	digest2, _ := modifiedIdentity.Digest()

	if digest1 == digest2 {
		t.Error("digest should change when tool configuration changes")
	}
}

func TestCandidateRequestStructure(t *testing.T) {
	req := CandidateRequest{
		Version:         AdapterVersion,
		RunID:           "test-run-1",
		TaskID:          "task-123",
		ReviewRound:     1,
		SubmittedCommit: "abc123def456",
		SnapshotPath:    "/tmp/snapshot",
		BlindedPrompt:   "Review this code",
		ToolAccess: ToolAccessRequirements{
			RequireSourceRetrieval: true,
			RequirePDFSupport:      false,
			DeclaredTools:          []string{"bash", "python"},
		},
		ResultPath: "/tmp/result.json",
	}

	// Verify it marshals to JSON.
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled CandidateRequest
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.RunID != req.RunID {
		t.Error("roundtrip failed")
	}
}

func TestCandidateResponseStructure(t *testing.T) {
	resp := CandidateResponse{
		Version:         AdapterVersion,
		Status:          "completed",
		ReviewCompleted: true,
		Findings: []Finding{
			{
				Severity: "major",
				Category: "correctness",
				Summary:  "Null pointer dereference",
				Details:  "Line 42 dereferences without null check",
				File:     "main.go",
				Line:     ptrInt(42),
			},
		},
		EffectiveCandidate: CandidateIdentity{
			AdapterName:    "test_adapter",
			AdapterVersion: "1.0",
			ModelID:        "model-1",
			RuntimeName:    "runtime",
			RuntimeVersion: "1.0",
			PromptVersion:  "v1",
			AccountOrPool:  "pool",
		},
		Timing: ResponseTiming{
			StartedAt:   time.Now().Add(-1 * time.Second),
			CompletedAt: time.Now(),
			Duration:    1 * time.Second,
		},
		Usage: &ResponseUsage{
			Units: "1000 tokens",
		},
	}

	// Verify it marshals to JSON.
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled CandidateResponse
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.Status != resp.Status {
		t.Error("roundtrip failed")
	}
	if len(unmarshaled.Findings) != 1 {
		t.Error("findings not preserved")
	}
}

func TestCandidateResponseWithoutOptionalFields(t *testing.T) {
	// Test that response can be created with minimal required fields.
	resp := CandidateResponse{
		Version:         AdapterVersion,
		Status:          "completed",
		ReviewCompleted: true,
		Findings:        []Finding{},
		EffectiveCandidate: CandidateIdentity{
			AdapterName:    "adapter",
			AdapterVersion: "1.0",
			ModelID:        "model",
			RuntimeName:    "runtime",
			RuntimeVersion: "1.0",
			PromptVersion:  "v1",
			AccountOrPool:  "pool",
		},
		Timing: ResponseTiming{
			StartedAt:   time.Now(),
			CompletedAt: time.Now(),
			Duration:    0,
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled CandidateResponse
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.Usage != nil {
		t.Error("optional Usage should be nil")
	}
	if unmarshaled.ErrorMessage != nil {
		t.Error("optional ErrorMessage should be nil")
	}
}

func TestRegistrationExecutableStructure(t *testing.T) {
	exe := RegistrationExecutable{
		Path:                 "/usr/bin/fake-adapter",
		Args:                 []string{"--request", "{request_path}", "--result", "{result_path}"},
		CredentialReferences: []string{"META_API_KEY"},
		Timeout:              5 * time.Minute,
		WorkingDirectory:     "/tmp",
		Environment:          map[string]string{"DEBUG": "false"},
	}

	data, err := json.Marshal(exe)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled RegistrationExecutable
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.Path != exe.Path {
		t.Error("path not preserved")
	}
	if len(unmarshaled.CredentialReferences) != 1 {
		t.Error("credentials not preserved")
	}
}

func TestAdapterRuntimeStructure(t *testing.T) {
	runtime := AdapterRuntime{
		Name:    "test_adapter",
		Version: "1.0",
		Executable: RegistrationExecutable{
			Path:                 "/usr/bin/test",
			Args:                 []string{"--request", "{request_path}"},
			CredentialReferences: []string{"TEST_KEY"},
			Timeout:              5 * time.Minute,
		},
		DeclaredCapabilities: []string{"source_retrieval", "tool_execution"},
		RequiredCredentials:  []string{"TEST_KEY"},
	}

	data, err := json.Marshal(runtime)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled AdapterRuntime
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.Name != runtime.Name {
		t.Error("name not preserved")
	}
	if len(unmarshaled.DeclaredCapabilities) != 2 {
		t.Error("capabilities not preserved")
	}
}

func TestFindingStructure(t *testing.T) {
	finding := Finding{
		Severity: "critical",
		Category: "security",
		Summary:  "SQL injection vulnerability",
		Details:  "User input is concatenated into SQL query without escaping",
		File:     "database.go",
		Line:     ptrInt(127),
	}

	data, err := json.Marshal(finding)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled Finding
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.Severity != finding.Severity {
		t.Error("severity not preserved")
	}
	if *unmarshaled.Line != 127 {
		t.Error("line number not preserved")
	}
}

func TestCapabilityPreflightStructure(t *testing.T) {
	preflight := CapabilityPreflight{
		Supported: true,
		Warnings:  []string{"Token limit approaching"},
		DeclaredCapabilities: map[string]bool{
			"source_retrieval": true,
			"pdf_parsing":      false,
		},
	}

	data, err := json.Marshal(preflight)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled CapabilityPreflight
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if !unmarshaled.Supported {
		t.Error("supported flag not preserved")
	}
	if len(unmarshaled.DeclaredCapabilities) != 2 {
		t.Error("capabilities not preserved")
	}
}

func TestToolConfigStructure(t *testing.T) {
	toolConfig := ToolConfig{
		Name:    "bash",
		Version: "5.1",
		Config: map[string]interface{}{
			"sandbox": true,
			"timeout": 30,
		},
	}

	data, err := json.Marshal(toolConfig)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var unmarshaled ToolConfig
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if unmarshaled.Name != "bash" {
		t.Error("tool name not preserved")
	}
}

func TestCandidateRequestValidation(t *testing.T) {
	validReq := CandidateRequest{
		Version:      AdapterVersion,
		RunID:        "test-run-1",
		SnapshotPath: "/tmp/snapshot",
		ResultPath:   "/tmp/result.json",
	}

	if err := validReq.Validate(); err != nil {
		t.Errorf("valid request should not error: %v", err)
	}

	// Missing RunID
	invalidReq := validReq
	invalidReq.RunID = ""
	if err := invalidReq.Validate(); err == nil {
		t.Error("request without RunID should fail validation")
	}

	// Missing SnapshotPath
	invalidReq = validReq
	invalidReq.SnapshotPath = ""
	if err := invalidReq.Validate(); err == nil {
		t.Error("request without SnapshotPath should fail validation")
	}

	// Missing ResultPath
	invalidReq = validReq
	invalidReq.ResultPath = ""
	if err := invalidReq.Validate(); err == nil {
		t.Error("request without ResultPath should fail validation")
	}

	// Wrong version
	invalidReq = validReq
	invalidReq.Version = "2.0"
	if err := invalidReq.Validate(); err == nil {
		t.Error("request with wrong version should fail validation")
	}
}

func TestCandidateResponseValidation(t *testing.T) {
	validResp := CandidateResponse{
		Version:         AdapterVersion,
		Status:          "completed",
		ReviewCompleted: true,
		Findings:        []Finding{},
		EffectiveCandidate: CandidateIdentity{
			AdapterName:    "test",
			AdapterVersion: "1.0",
			ModelID:        "model",
			RuntimeName:    "runtime",
			RuntimeVersion: "1.0",
			PromptVersion:  "v1",
			AccountOrPool:  "pool",
		},
		Timing: ResponseTiming{
			StartedAt:   time.Now(),
			CompletedAt: time.Now(),
			Duration:    0,
		},
	}

	if err := validResp.Validate(); err != nil {
		t.Errorf("valid response should not error: %v", err)
	}

	// Completed without ReviewCompleted=true
	invalidResp := validResp
	invalidResp.ReviewCompleted = false
	if err := invalidResp.Validate(); err == nil {
		t.Error("completed status requires ReviewCompleted=true")
	}

	// Completed with error fields
	invalidResp = validResp
	invalidResp.ErrorClass = ptrString("test_error")
	if err := invalidResp.Validate(); err == nil {
		t.Error("completed status must not have error fields")
	}

	// Failed without ErrorClass
	invalidResp = validResp
	invalidResp.Status = "failed"
	invalidResp.ReviewCompleted = false
	invalidResp.ErrorClass = nil
	if err := invalidResp.Validate(); err == nil {
		t.Error("failed status requires ErrorClass")
	}

	// Invalid status
	invalidResp = validResp
	invalidResp.Status = "unknown_status"
	if err := invalidResp.Validate(); err == nil {
		t.Error("invalid status should fail validation")
	}
}

func TestCapabilityPreflightCheck(t *testing.T) {
	req := CandidateRequest{
		Version: AdapterVersion,
		ToolAccess: ToolAccessRequirements{
			RequireSourceRetrieval: true,
			RequirePDFSupport:      false,
		},
	}

	// Adapter with required capability
	preflight := &CapabilityPreflight{}
	preflight.CheckCapabilities(&req, []string{"source_retrieval"})
	if !preflight.Supported {
		t.Error("preflight should support request with source_retrieval")
	}

	// Adapter without required capability
	preflight = &CapabilityPreflight{}
	preflight.CheckCapabilities(&req, []string{"other_capability"})
	if preflight.Supported {
		t.Error("preflight should not support request without source_retrieval")
	}
	if preflight.Error == nil {
		t.Error("preflight error should be set")
	}

	// Request requiring PDF support (pdf_parsing is the standard name)
	req.ToolAccess.RequireSourceRetrieval = false
	req.ToolAccess.RequirePDFSupport = true
	preflight = &CapabilityPreflight{}
	preflight.CheckCapabilities(&req, []string{"other_capability"})
	if preflight.Supported {
		t.Error("preflight should not support request without pdf_parsing")
	}
}

func TestCapabilityPreflightStructuredOutput(t *testing.T) {
	req := CandidateRequest{
		Version: AdapterVersion,
		ToolAccess: ToolAccessRequirements{
			RequireStructuredOutput: true,
		},
	}

	// Adapter with structured output support
	preflight := &CapabilityPreflight{}
	preflight.CheckCapabilities(&req, []string{"structured_output"})
	if !preflight.Supported {
		t.Error("preflight should support structured_output")
	}

	// Adapter without structured output support
	preflight = &CapabilityPreflight{}
	preflight.CheckCapabilities(&req, []string{"source_retrieval"})
	if preflight.Supported {
		t.Error("preflight should not support request without structured_output")
	}
}

func TestAdapterRegistry(t *testing.T) {
	registry := NewAdapterRegistry()

	// Register an adapter
	runtime := &AdapterRuntime{
		Name:    "test_adapter",
		Version: "1.0",
		Executable: RegistrationExecutable{
			Path: "/usr/bin/test",
			Args: []string{"--request", "{request_path}"},
		},
		DeclaredCapabilities: []string{"source_retrieval"},
	}

	if err := registry.Register(runtime); err != nil {
		t.Fatalf("registration failed: %v", err)
	}

	// Resolve the adapter
	resolved := registry.Resolve("test_adapter")
	if resolved == nil {
		t.Error("adapter not found")
	}
	if resolved.Name != "test_adapter" {
		t.Error("adapter name mismatch")
	}

	// Try to register duplicate
	if err := registry.Register(runtime); err == nil {
		t.Error("should not allow duplicate registration")
	}

	// List registered adapters
	adapters := registry.RegisteredAdapters()
	if len(adapters) != 1 || adapters[0] != "test_adapter" {
		t.Error("adapter list incorrect")
	}
}

func TestUnknownSettingsMapSerialization(t *testing.T) {
	// nil = unknown
	var nilSettings UnknownSettingsMap
	nilData, err := json.Marshal(nilSettings)
	if err != nil {
		t.Fatalf("marshal nil settings failed: %v", err)
	}

	// empty map = no settings
	emptySettings := UnknownSettingsMap{}
	emptyData, err := json.Marshal(emptySettings)
	if err != nil {
		t.Fatalf("marshal empty settings failed: %v", err)
	}

	// Verify they serialize differently
	if string(nilData) == string(emptyData) {
		t.Error("nil and empty settings should serialize differently")
	}

	// nil should be the string "unknown"
	if !strings.Contains(string(nilData), "unknown") {
		t.Errorf("nil settings should contain 'unknown', got: %s", string(nilData))
	}

	// empty should be an empty object
	if !strings.Contains(string(emptyData), "{}") {
		t.Errorf("empty settings should contain '{{}}', got: %s", string(emptyData))
	}
}

func TestCandidateIdentityUnknownVsEmpty(t *testing.T) {
	// Identity with unknown reasoning settings
	identity1 := CandidateIdentity{
		AdapterName:       "test",
		AdapterVersion:    "1.0",
		ModelID:           "model1",
		RuntimeName:       "runtime",
		RuntimeVersion:    "1.0",
		PromptVersion:     "v1",
		AccountOrPool:     "pool",
		ReasoningSettings: nil, // unknown
	}

	// Identity with empty reasoning settings
	identity2 := CandidateIdentity{
		AdapterName:       "test",
		AdapterVersion:    "1.0",
		ModelID:           "model1",
		RuntimeName:       "runtime",
		RuntimeVersion:    "1.0",
		PromptVersion:     "v1",
		AccountOrPool:     "pool",
		ReasoningSettings: UnknownSettingsMap{}, // empty
	}

	digest1, err := identity1.Digest()
	if err != nil {
		t.Fatalf("digest 1 failed: %v", err)
	}

	digest2, err := identity2.Digest()
	if err != nil {
		t.Fatalf("digest 2 failed: %v", err)
	}

	if digest1 == digest2 {
		t.Error("unknown and empty settings should produce different digests")
	}
}
