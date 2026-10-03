package evaluation

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func baseIdentity() CandidateIdentity {
	return CandidateIdentity{
		AdapterName: "fake", AdapterVersion: "1", ModelID: "model-a", ModelRevision: Unknown,
		RuntimeName: "fake-cli", RuntimeVersion: "2.0",
		ReasoningSettings:  KnownSettings(map[string]string{"effort": "high"}),
		GenerationSettings: KnownSettings(map[string]string{"temperature": "0"}),
		PromptVersion:      "p1", Tools: KnownNames("web_fetch", "pdf_reader"), Observers: KnownNames(),
		AccountPool: "pool-a",
	}
}

func baseRequest(t *testing.T) CandidateRequest {
	t.Helper()
	dir := t.TempDir()
	return CandidateRequest{
		Version: ProtocolVersion, RunID: "run-1", SnapshotPath: dir,
		BlindedPrompt: "review this artifact",
		ToolAccess:    ToolAccessRequirements{RequireSourceRetrieval: true, Tools: []string{"web_fetch"}},
		ResultPath:    filepath.Join(dir, "out", "result.json"),
	}
}

func TestRequestValidate(t *testing.T) {
	if err := baseRequest(t).Validate(); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	cases := map[string]struct {
		mutate  func(*CandidateRequest)
		wantErr error
	}{
		"future version":    {func(r *CandidateRequest) { r.Version = 2 }, ErrUnsupportedVersion},
		"zero version":      {func(r *CandidateRequest) { r.Version = 0 }, ErrUnsupportedVersion},
		"no run id":         {func(r *CandidateRequest) { r.RunID = " " }, ErrInvalid},
		"relative snapshot": {func(r *CandidateRequest) { r.SnapshotPath = "snap" }, ErrInvalid},
		"relative result":   {func(r *CandidateRequest) { r.ResultPath = "result.json" }, ErrInvalid},
		"empty prompt":      {func(r *CandidateRequest) { r.BlindedPrompt = "" }, ErrInvalid},
		"duplicate tool":    {func(r *CandidateRequest) { r.ToolAccess.Tools = []string{"a", "a"} }, ErrInvalid},
		"blank tool":        {func(r *CandidateRequest) { r.ToolAccess.Tools = []string{" "} }, ErrInvalid},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := baseRequest(t)
			tc.mutate(&r)
			if err := r.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func completedResponse() CandidateResponse {
	return CandidateResponse{
		Version: ProtocolVersion, RunID: "run-1", Status: StatusCompleted, ReviewCompleted: true,
		Identity: baseIdentity(),
		Findings: []Finding{{ID: "f1", Severity: SeverityMaterial, Summary: "s"}},
		Usage:    map[string]float64{"tokens": 10},
	}
}

func failedResponse(status Status, class ErrorClass) CandidateResponse {
	return CandidateResponse{Version: ProtocolVersion, RunID: "run-1", Status: status, ErrorClass: class, Identity: baseIdentity()}
}

func TestResponseValidate(t *testing.T) {
	ok := map[string]CandidateResponse{
		"completed":          completedResponse(),
		"completed no finds": func() CandidateResponse { r := completedResponse(); r.Findings = nil; return r }(),
		"incomplete":         failedResponse(StatusIncomplete, ErrClassOutputTruncated),
		"interrupted":        failedResponse(StatusInterrupted, ErrClassTimeout),
		"failed":             failedResponse(StatusFailed, ErrClassRuntimeError),
		"unsupported": func() CandidateResponse {
			r := failedResponse(StatusUnsupported, ErrClassCapabilityMissing)
			r.MissingCapabilities = []string{CapPDFAccess}
			return r
		}(),
	}
	for name, r := range ok {
		if err := r.Validate(); err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
		}
	}

	bad := map[string]func(*CandidateResponse){
		"version":                 func(r *CandidateResponse) { r.Version = 9 },
		"no run id":               func(r *CandidateResponse) { r.RunID = "" },
		"unknown status":          func(r *CandidateResponse) { r.Status = "done" },
		"completed not completed": func(r *CandidateResponse) { r.ReviewCompleted = false },
		"completed with error":    func(r *CandidateResponse) { r.ErrorClass = ErrClassRuntimeError },
		"finding no id":           func(r *CandidateResponse) { r.Findings[0].ID = "" },
		"finding bad severity":    func(r *CandidateResponse) { r.Findings[0].Severity = "huge" },
		"finding no summary":      func(r *CandidateResponse) { r.Findings[0].Summary = "" },
		"duplicate finding":       func(r *CandidateResponse) { r.Findings = append(r.Findings, r.Findings[0]) },
		"empty identity value":    func(r *CandidateResponse) { r.Identity.ModelID = "" },
		"unknown set with values": func(r *CandidateResponse) { r.Identity.Tools = NameSet{Known: false, Names: []string{"x"}} },
		"negative usage":          func(r *CandidateResponse) { r.Usage["tokens"] = -1 },
		"timing reversed": func(r *CandidateResponse) {
			r.Timing = Timing{StartedAt: time.Unix(10, 0), FinishedAt: time.Unix(5, 0)}
		},
		"long message": func(r *CandidateResponse) { r.ErrorMessage = strings.Repeat("x", maxErrorMessage+1) },
		"missing caps on completed": func(r *CandidateResponse) {
			r.MissingCapabilities = []string{CapPDFAccess}
		},
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			r := completedResponse()
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	nonCompleted := map[string]func(*CandidateResponse){
		"claims review completed": func(r *CandidateResponse) { r.ReviewCompleted = true },
		"carries findings":        func(r *CandidateResponse) { r.Findings = completedResponse().Findings },
		"no error class":          func(r *CandidateResponse) { r.ErrorClass = "" },
		"class of another status": func(r *CandidateResponse) { r.ErrorClass = ErrClassTimeout },
	}
	for name, mutate := range nonCompleted {
		t.Run("failed "+name, func(t *testing.T) {
			r := failedResponse(StatusFailed, ErrClassRuntimeError)
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}

	t.Run("unsupported must list capabilities", func(t *testing.T) {
		if err := failedResponse(StatusUnsupported, ErrClassCapabilityMissing).Validate(); err == nil {
			t.Fatal("expected validation error")
		}
	})
}

func TestDecodeResponseStrict(t *testing.T) {
	good, err := json.Marshal(completedResponse())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeResponse(good); err != nil {
		t.Fatalf("good response rejected: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(good, &m); err != nil {
		t.Fatal(err)
	}
	m["version"] = 2
	future, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	m["version"] = 1
	m["surprise"] = true
	extra, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		data    []byte
		wantErr error
	}{
		"future version": {future, ErrUnsupportedVersion},
		"unknown field":  {extra, ErrInvalid},
		"truncated":      {good[:len(good)/2], ErrInvalid},
		"trailing data":  {append(append([]byte{}, good...), []byte(`{}`)...), ErrInvalid},
		"empty":          {nil, ErrInvalid},
		"not an object":  {[]byte(`[1]`), ErrInvalid},
	}
	for name, tc := range cases {
		if _, err := DecodeResponse(tc.data); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: got %v, want %v", name, err, tc.wantErr)
		}
	}
}

func TestIdentityValidateRequiresExplicitUnknown(t *testing.T) {
	if err := baseIdentity().Validate(); err != nil {
		t.Fatal(err)
	}
	if err := UnknownIdentity().Validate(); err != nil {
		t.Fatalf("all-unknown identity must be valid: %v", err)
	}
	id := baseIdentity()
	id.ModelRevision = ""
	if err := id.Validate(); err == nil {
		t.Fatal("empty string must not stand in for unknown")
	}
}

func TestIdentityDigest(t *testing.T) {
	base := baseIdentity().Digest()
	if base != baseIdentity().Digest() {
		t.Fatal("digest not stable")
	}

	reordered := baseIdentity()
	reordered.Tools = KnownNames("pdf_reader", "web_fetch")
	if reordered.Digest() != base {
		t.Error("tool order must not change the digest")
	}

	changes := map[string]func(*CandidateIdentity){
		"adapter name":    func(c *CandidateIdentity) { c.AdapterName = "other" },
		"adapter version": func(c *CandidateIdentity) { c.AdapterVersion = "2" },
		"model id":        func(c *CandidateIdentity) { c.ModelID = "model-b" },
		"model revision":  func(c *CandidateIdentity) { c.ModelRevision = "r2" },
		"runtime name":    func(c *CandidateIdentity) { c.RuntimeName = "other" },
		"runtime version": func(c *CandidateIdentity) { c.RuntimeVersion = "3" },
		"reasoning value": func(c *CandidateIdentity) {
			c.ReasoningSettings = KnownSettings(map[string]string{"effort": "low"})
		},
		"reasoning key": func(c *CandidateIdentity) {
			c.ReasoningSettings = KnownSettings(map[string]string{"budget": "high"})
		},
		"generation": func(c *CandidateIdentity) {
			c.GenerationSettings = KnownSettings(map[string]string{"temperature": "1"})
		},
		"prompt version": func(c *CandidateIdentity) { c.PromptVersion = "p2" },
		"tools":          func(c *CandidateIdentity) { c.Tools = KnownNames("web_fetch") },
		"observers":      func(c *CandidateIdentity) { c.Observers = KnownNames("watcher") },
		"account pool":   func(c *CandidateIdentity) { c.AccountPool = "pool-b" },
	}
	seen := map[string]string{base: "base"}
	for name, mutate := range changes {
		id := baseIdentity()
		mutate(&id)
		d := id.Digest()
		if prior, dup := seen[d]; dup {
			t.Errorf("%s digest collides with %s", name, prior)
		}
		seen[d] = name
	}

	t.Run("unknown differs from known empty", func(t *testing.T) {
		for name, pair := range map[string][2]func(*CandidateIdentity){
			"reasoning": {
				func(c *CandidateIdentity) { c.ReasoningSettings = UnknownSettings() },
				func(c *CandidateIdentity) { c.ReasoningSettings = KnownSettings(nil) },
			},
			"generation": {
				func(c *CandidateIdentity) { c.GenerationSettings = UnknownSettings() },
				func(c *CandidateIdentity) { c.GenerationSettings = KnownSettings(map[string]string{}) },
			},
			"tools": {
				func(c *CandidateIdentity) { c.Tools = UnknownNames() },
				func(c *CandidateIdentity) { c.Tools = KnownNames() },
			},
			"observers": {
				func(c *CandidateIdentity) { c.Observers = UnknownNames() },
				func(c *CandidateIdentity) { c.Observers = KnownNames() },
			},
		} {
			a, b := baseIdentity(), baseIdentity()
			pair[0](&a)
			pair[1](&b)
			if a.Digest() == b.Digest() {
				t.Errorf("%s: unknown and known-empty share a digest", name)
			}
		}
	})

	t.Run("unknown model revision differs from a stated one", func(t *testing.T) {
		a, b := baseIdentity(), baseIdentity()
		a.ModelRevision, b.ModelRevision = Unknown, "2026-01"
		if a.Digest() == b.Digest() {
			t.Fatal("digests must differ")
		}
	})
}

func TestCandidateConfigImmutable(t *testing.T) {
	reasoning := map[string]string{"effort": "high"}
	tools := []string{"web_fetch"}
	id := baseIdentity()
	id.ReasoningSettings = Settings{Known: true, Values: reasoning}
	id.Tools = NameSet{Known: true, Names: tools}

	cfg, err := NewCandidateConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	digest := cfg.Digest()
	if digest != id.Digest() {
		t.Fatal("config digest must equal the identity digest")
	}

	reasoning["effort"] = "low"
	tools[0] = "changed"
	if got := cfg.Identity(); got.ReasoningSettings.Values["effort"] != "high" || got.Tools.Names[0] != "web_fetch" {
		t.Fatalf("config followed caller mutation: %+v", got)
	}

	out := cfg.Identity()
	out.ReasoningSettings.Values["effort"] = "mutated"
	out.Tools.Names[0] = "mutated"
	again := cfg.Identity()
	if again.ReasoningSettings.Values["effort"] != "high" || again.Tools.Names[0] != "web_fetch" {
		t.Fatalf("config followed mutation of a returned copy: %+v", again)
	}
	if cfg.Digest() != digest || again.Digest() != digest {
		t.Fatal("digest changed after mutation attempts")
	}
}

func TestNewCandidateConfigRejectsInvalid(t *testing.T) {
	id := baseIdentity()
	id.PromptVersion = ""
	if _, err := NewCandidateConfig(id); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestCheckCapabilities(t *testing.T) {
	full := []string{CapStructuredOutput, CapSourceRetrieval, CapPDFAccess, CapToolPrefix + "web_fetch"}
	cases := map[string]struct {
		access   ToolAccessRequirements
		declared []string
		want     []string
	}{
		"nothing beyond output": {ToolAccessRequirements{}, []string{CapStructuredOutput}, nil},
		"everything":            {ToolAccessRequirements{RequireSourceRetrieval: true, RequirePDFAccess: true, Tools: []string{"web_fetch"}}, full, nil},
		"output always needed":  {ToolAccessRequirements{}, nil, []string{CapStructuredOutput}},
		"no source":             {ToolAccessRequirements{RequireSourceRetrieval: true}, []string{CapStructuredOutput}, []string{CapSourceRetrieval}},
		"no pdf":                {ToolAccessRequirements{RequirePDFAccess: true}, []string{CapStructuredOutput, CapSourceRetrieval}, []string{CapPDFAccess}},
		"missing tools sorted":  {ToolAccessRequirements{Tools: []string{"b", "a"}}, []string{CapStructuredOutput}, []string{"tool:a", "tool:b"}},
		"extra caps ignored":    {ToolAccessRequirements{}, full, nil},
	}
	for name, tc := range cases {
		if got := CheckCapabilities(tc.access, tc.declared); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
