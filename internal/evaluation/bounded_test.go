package evaluation

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBoundedComparisonAcceptsValidRequests(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake1", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	cfg := BoundedComparisonConfig{
		Registry:    reg,
		Credentials: MapCredentials{},
		RuntimeName: "fake1",
		StagingDir:  filepath.Join(dir, "staging"),
		Now:         fixedClock,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review this",
		ToolAccess:    ToolAccessRequirements{RequireSourceRetrieval: true},
	}

	res, err := cfg.RunBoundedComparison(context.Background(), req)
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if res.Response.Status != StatusCompleted || !res.Response.ReviewCompleted {
		t.Errorf("got status=%s completed=%v, want completed/true", res.Response.Status, res.Response.ReviewCompleted)
	}
	if len(res.Response.Findings) == 0 {
		t.Errorf("expected findings, got none")
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
	cfg2 := BoundedComparisonConfig{
		Registry:    reg,
		Credentials: secrets,
		RuntimeName: "with-creds",
		StagingDir:  filepath.Join(dir, "staging"),
		Now:         fixedClock,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
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
}

func TestBoundedComparisonRejectsInvalidInput(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)

	dir := t.TempDir()
	cfg := BoundedComparisonConfig{
		Registry:    reg,
		Credentials: MapCredentials{},
		RuntimeName: "fake",
		StagingDir:  filepath.Join(dir, "staging"),
	}

	cases := map[string]BoundedComparisonRequest{
		"missing run id":    {RunID: "", SnapshotPath: dir, BlindedPrompt: "p"},
		"relative snapshot": {RunID: "r1", SnapshotPath: "relative", BlindedPrompt: "p"},
		"empty prompt":      {RunID: "r1", SnapshotPath: dir, BlindedPrompt: ""},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := cfg.RunBoundedComparison(context.Background(), req)
			if err == nil {
				t.Fatalf("expected error, got none")
			}
		})
	}
}

func TestBoundedComparisonHandlesRuntimeErrors(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeFailed, baseIdentity(), fullCaps)

	dir := t.TempDir()
	cfg := BoundedComparisonConfig{
		Registry:    reg,
		Credentials: MapCredentials{},
		RuntimeName: "fake",
		StagingDir:  filepath.Join(dir, "staging"),
		Now:         fixedClock,
	}

	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review",
		ToolAccess:    ToolAccessRequirements{},
	}

	res, err := cfg.RunBoundedComparison(context.Background(), req)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if res.Response.Status != StatusFailed {
		t.Errorf("got status %s, want failed", res.Response.Status)
	}
}

// TestSecondFakeAdapterWithComputePool demonstrates extensibility with a
// second fake adapter using different usage units and compute pool.
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
	req := BoundedComparisonRequest{
		RunID:         "run-1",
		SnapshotPath:  dir,
		BlindedPrompt: "review",
		ToolAccess:    ToolAccessRequirements{},
	}

	// Run both adapters independently, proving they don't interfere.
	for name, poolID := range map[string]string{
		"fake-sub":     "subscription-pool",
		"fake-compute": "compute-pool",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := BoundedComparisonConfig{
				Registry:    reg,
				Credentials: MapCredentials{},
				RuntimeName: name,
				StagingDir:  filepath.Join(dir, "staging", name),
				Now:         fixedClock,
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
