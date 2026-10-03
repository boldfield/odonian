package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const fakeAdapterArg = "__fake_adapter__"

// The test binary doubles as the fake adapter executable, so the pipeline is
// exercised through a real registered argv without shell or network.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == fakeAdapterArg {
		os.Exit(FakeMain(os.Args[2:], os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == museAdapterArg {
		os.Exit(MuseMain(os.Args[2:], os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

var fixedClock = func() time.Time { return time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC) }

var fullCaps = []string{CapStructuredOutput, CapSourceRetrieval, CapPDFAccess, CapToolPrefix + "web_fetch"}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func registerFake(t *testing.T, reg *Registry, name, mode string, id CandidateIdentity, caps []string, mutate ...func(*Runtime)) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewCandidateConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	rt := Runtime{
		Name: name, Executable: exe, Timeout: 30 * time.Second, Capabilities: caps, Candidate: cfg,
		Args: []string{fakeAdapterArg, "--mode", mode, "--request", PlaceholderRequestPath, "--identity", mustJSON(t, id)},
	}
	for _, m := range mutate {
		m(&rt)
	}
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
}

func newPipeline(reg *Registry) *Pipeline {
	return &Pipeline{Registry: reg, Credentials: MapCredentials{}, Now: fixedClock}
}

func TestFakeFixturesThroughPipeline(t *testing.T) {
	cases := []struct {
		mode      string
		status    Status
		class     ErrorClass
		exit      int
		completed bool
	}{
		{FakeModeSuccess, StatusCompleted, "", 0, true},
		{FakeModeIncomplete, StatusIncomplete, ErrClassSourceUnavailable, 0, false},
		{FakeModeUnsupported, StatusUnsupported, ErrClassCapabilityMissing, 0, false},
		{FakeModeInterrupted, StatusInterrupted, ErrClassInterrupted, 130, false},
		{FakeModeFailed, StatusFailed, ErrClassRuntimeError, 1, false},
		{FakeModeMalformed, StatusFailed, ErrClassOutputMalformed, 0, false},
		{FakeModeInvalid, StatusFailed, ErrClassOutputMalformed, 0, false},
		{FakeModeWrongRun, StatusFailed, ErrClassOutputMalformed, 0, false},
		{FakeModeDirtyExit, StatusFailed, ErrClassRuntimeError, 1, false},
		{FakeModeCrash, StatusFailed, ErrClassRuntimeError, 3, false},
		{FakeModeSilent, StatusFailed, ErrClassOutputMissing, 0, false},
		{"no-such-mode", StatusFailed, ErrClassRuntimeError, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			reg := NewRegistry()
			registerFake(t, reg, "fake", tc.mode, baseIdentity(), fullCaps)
			res, err := newPipeline(reg).Execute(context.Background(), "fake", baseRequest(t))
			if err != nil {
				t.Fatal(err)
			}
			r := res.Response
			if r.Status != tc.status || r.ErrorClass != tc.class || r.ReviewCompleted != tc.completed {
				t.Fatalf("got status=%s class=%q completed=%v, want %s/%q/%v (%s)", r.Status, r.ErrorClass, r.ReviewCompleted, tc.status, tc.class, tc.completed, r.ErrorMessage)
			}
			if !res.Launched || res.ExitCode != tc.exit {
				t.Fatalf("launched=%v exit=%d, want true/%d", res.Launched, res.ExitCode, tc.exit)
			}
			if err := r.Validate(); err != nil {
				t.Fatalf("normalized response invalid: %v", err)
			}
			if r.Timing.StartedAt.IsZero() || r.Timing.FinishedAt.Before(r.Timing.StartedAt) {
				t.Fatalf("timing not recorded by host: %+v", r.Timing)
			}
			if res.CandidateDigest != baseIdentityDigest(t) {
				t.Fatal("result must carry the declared candidate digest")
			}
			switch tc.mode {
			case FakeModeMalformed, FakeModeInvalid, FakeModeWrongRun:
				if len(res.RawOutput) == 0 {
					t.Fatal("rejected output should be retained for diagnosis")
				}
				if r.Identity.ModelID != Unknown {
					t.Fatalf("a rejected result must not contribute an effective identity, got %q", r.Identity.ModelID)
				}
			case FakeModeSuccess:
				if r.Identity.ModelID != "model-a" || r.Identity.ModelRevision != Unknown {
					t.Fatalf("effective identity not carried through: %+v", r.Identity)
				}
				if r.Usage["fake_units"] == 0 {
					t.Fatal("provider-native usage should pass through")
				}
			}
		})
	}
}

func baseIdentityDigest(t *testing.T) string {
	t.Helper()
	cfg, err := NewCandidateConfig(baseIdentity())
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Digest()
}

func TestFakeExecutionDeterministic(t *testing.T) {
	run := func() Result {
		t.Helper()
		reg := NewRegistry()
		registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)
		req := baseRequest(t)
		req.SnapshotPath, req.ResultPath = "/tmp", filepath.Join(t.TempDir(), "result.json")
		res, err := newPipeline(reg).Execute(context.Background(), "fake", req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a, b := run(), run()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("runs differ:\n%+v\n%+v", a, b)
	}
	want := []Finding{
		{ID: "fake-6f1c0419-1", Severity: SeverityMaterial, Claim: "claim-1", Summary: "fake material finding 6f1c0419", Evidence: "snapshot /tmp"},
		{ID: "fake-6f1c0419-2", Severity: SeverityNote, Summary: "fake source note structured_output,source_retrieval,tool:web_fetch"},
	}
	if !reflect.DeepEqual(a.Response.Findings, want) {
		t.Fatalf("findings = %+v", a.Response.Findings)
	}
	if got, wantT := a.Response.Timing, (Timing{StartedAt: fixedClock(), FinishedAt: fixedClock()}); got != wantT {
		t.Fatalf("timing = %+v", got)
	}
}

func TestTwoCandidateConfigurationsShareOnePipeline(t *testing.T) {
	idA := baseIdentity()
	idB := baseIdentity()
	idB.ReasoningSettings = KnownSettings(map[string]string{"effort": "low"})
	idB.Tools = KnownNames("web_fetch")
	idB.ModelRevision = "rev-7"

	reg := NewRegistry()
	registerFake(t, reg, "cand-a", FakeModeSuccess, idA, fullCaps)
	registerFake(t, reg, "cand-b", FakeModeSuccess, idB, fullCaps)
	p := newPipeline(reg)

	req := baseRequest(t)
	resA, err := p.Execute(context.Background(), "cand-a", req)
	if err != nil {
		t.Fatal(err)
	}
	reqB := req
	reqB.ResultPath = filepath.Join(filepath.Dir(req.ResultPath), "b", "result.json")
	resB, err := p.Execute(context.Background(), "cand-b", reqB)
	if err != nil {
		t.Fatal(err)
	}

	if resA.CandidateDigest == resB.CandidateDigest {
		t.Fatal("candidates differing only in settings/tools/revision must have different digests")
	}
	for name, res := range map[string]Result{"a": resA, "b": resB} {
		if res.Response.Status != StatusCompleted || !res.Response.ReviewCompleted {
			t.Fatalf("%s: %+v", name, res.Response)
		}
	}
	if !reflect.DeepEqual(resA.Response.Identity, idA) || !reflect.DeepEqual(resB.Response.Identity, idB) {
		t.Fatal("each result must record its own effective identity")
	}
	if !reflect.DeepEqual(resA.Response.Findings, resB.Response.Findings) {
		t.Fatal("same request must yield the same normalized findings through the shared pipeline")
	}
}

func TestPreflightIsUnconditionalAndNeverLaunches(t *testing.T) {
	cases := map[string]struct {
		caps    []string
		access  ToolAccessRequirements
		missing []string
	}{
		"no pdf":    {[]string{CapStructuredOutput, CapSourceRetrieval}, ToolAccessRequirements{RequirePDFAccess: true}, []string{CapPDFAccess}},
		"no source": {[]string{CapStructuredOutput}, ToolAccessRequirements{RequireSourceRetrieval: true}, []string{CapSourceRetrieval}},
		"no output": {[]string{CapSourceRetrieval}, ToolAccessRequirements{}, []string{CapStructuredOutput}},
		"no tool":   {[]string{CapStructuredOutput}, ToolAccessRequirements{Tools: []string{"web_fetch"}}, []string{"tool:web_fetch"}},
		"several":   {nil, ToolAccessRequirements{RequirePDFAccess: true, RequireSourceRetrieval: true}, []string{CapPDFAccess, CapSourceRetrieval, CapStructuredOutput}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reg := NewRegistry()
			// A success-mode fake would report a clean review if it were launched.
			registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), tc.caps)
			req := baseRequest(t)
			req.ToolAccess = tc.access
			res, err := newPipeline(reg).Execute(context.Background(), "fake", req)
			if err != nil {
				t.Fatal(err)
			}
			r := res.Response
			if r.Status != StatusUnsupported || r.ReviewCompleted || r.ErrorClass != ErrClassCapabilityMissing {
				t.Fatalf("got %+v", r)
			}
			if !reflect.DeepEqual(r.MissingCapabilities, tc.missing) {
				t.Fatalf("missing = %v, want %v", r.MissingCapabilities, tc.missing)
			}
			if res.Launched {
				t.Fatal("unsupported request must not launch the runtime")
			}
			if _, err := os.Stat(RequestPathFor(req.ResultPath)); !os.IsNotExist(err) {
				t.Fatalf("request was staged for an unsupported run: %v", err)
			}
			if !reflect.DeepEqual(r.Identity, UnknownIdentity()) {
				t.Fatalf("host-synthesized identity must be all-unknown, got %+v", r.Identity)
			}
		})
	}
}

func TestResultDestinationComesFromRequest(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)
	req := baseRequest(t)
	req.ResultPath = filepath.Join(t.TempDir(), "deep", "er", "result.json")
	res, err := newPipeline(reg).Execute(context.Background(), "fake", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Status != StatusCompleted {
		t.Fatalf("got %+v", res.Response)
	}
	if _, err := os.Stat(req.ResultPath); err != nil {
		t.Fatalf("adapter should write to the request's result path: %v", err)
	}
}

func TestStaleResultIsNotPickedUp(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "ok", FakeModeSuccess, baseIdentity(), fullCaps)
	registerFake(t, reg, "silent", FakeModeSilent, baseIdentity(), fullCaps)
	p := newPipeline(reg)
	req := baseRequest(t)
	if res, err := p.Execute(context.Background(), "ok", req); err != nil || res.Response.Status != StatusCompleted {
		t.Fatalf("setup run: %v %+v", err, res.Response)
	}
	res, err := p.Execute(context.Background(), "silent", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Status != StatusFailed || res.Response.ErrorClass != ErrClassOutputMissing {
		t.Fatalf("a previous run's result leaked into this one: %+v", res.Response)
	}
}

func TestEnvironmentIsolationArgvAndCredentials(t *testing.T) {
	t.Setenv("ODONIAN_TOKEN", "board-secret")
	t.Setenv("META_API_KEY", "paygo-secret")
	t.Setenv("ALLOWED_VAR", "allowed")

	const injected = "x; touch PWNED && echo {run_id}"
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeProbe, baseIdentity(), fullCaps, func(rt *Runtime) {
		rt.Args = append(rt.Args, injected)
		rt.PassThroughEnv = []string{"ALLOWED_VAR", "UNSET_VAR"}
		rt.Credentials = []CredentialRef{{EnvName: "REVIEW_KEY", Ref: "vault/review"}}
	})
	p := newPipeline(reg)
	p.Credentials = MapCredentials{"vault/review": "s3cret-value"}
	req := baseRequest(t)
	res, err := p.Execute(context.Background(), "fake", req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Status != StatusCompleted {
		t.Fatalf("got %+v (%s)", res.Response, res.Stderr)
	}

	raw, err := os.ReadFile(FakeProbePath(req.ResultPath))
	if err != nil {
		t.Fatal(err)
	}
	var probe FakeProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}

	env := map[string]string{}
	for _, e := range probe.Env {
		k, v, _ := strings.Cut(e, "=")
		env[k] = v
	}
	wantEnv := map[string]string{
		"PATH": childPath, "ODONIAN_EVAL_PROTOCOL": "1", "ALLOWED_VAR": "allowed", "REVIEW_KEY": "s3cret-value",
	}
	if !reflect.DeepEqual(env, wantEnv) {
		t.Fatalf("child environment = %v, want exactly %v", env, wantEnv)
	}
	if strings.Contains(string(raw), "board-secret") || strings.Contains(string(raw), "paygo-secret") {
		t.Fatal("host secrets leaked into the child")
	}

	if !containsArg(probe.Args, "x; touch PWNED && echo run-1") {
		t.Fatalf("argument should be substituted and passed literally, args = %v", probe.Args)
	}
	if !containsArg(probe.Args, RequestPathFor(req.ResultPath)) {
		t.Fatalf("request placeholder not substituted: %v", probe.Args)
	}
	if _, err := os.Stat(filepath.Join(req.SnapshotPath, "PWNED")); !os.IsNotExist(err) {
		t.Fatal("argument text was interpreted by a shell")
	}
	wantCwd, err := filepath.EvalSymlinks(req.SnapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotCwd, _ := filepath.EvalSymlinks(probe.Cwd); gotCwd != wantCwd {
		t.Fatalf("cwd = %q, want snapshot %q", probe.Cwd, wantCwd)
	}
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func TestUnresolvedCredentialFailsBeforeLaunch(t *testing.T) {
	for name, creds := range map[string]CredentialResolver{
		"missing":      MapCredentials{},
		"empty secret": MapCredentials{"vault/review": ""},
		"nil resolver": nil,
	} {
		t.Run(name, func(t *testing.T) {
			reg := NewRegistry()
			registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps, func(rt *Runtime) {
				rt.Credentials = []CredentialRef{{EnvName: "REVIEW_KEY", Ref: "vault/review"}}
			})
			p := newPipeline(reg)
			p.Credentials = creds
			req := baseRequest(t)
			res, err := p.Execute(context.Background(), "fake", req)
			if err != nil {
				t.Fatal(err)
			}
			if res.Launched || res.Response.Status != StatusFailed || res.Response.ErrorClass != ErrClassAuthMissing {
				t.Fatalf("got launched=%v %+v", res.Launched, res.Response)
			}
			if _, err := os.Stat(RequestPathFor(req.ResultPath)); !os.IsNotExist(err) {
				t.Fatal("request staged despite unresolved credential")
			}
		})
	}
}

func TestStderrIsRedacted(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeLeak, baseIdentity(), fullCaps, func(rt *Runtime) {
		rt.Credentials = []CredentialRef{{EnvName: "REVIEW_KEY", Ref: "vault/review"}}
	})
	p := newPipeline(reg)
	p.Credentials = MapCredentials{"vault/review": "s3cret-value"}
	res, err := p.Execute(context.Background(), "fake", baseRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Stderr, "s3cret-value") || !strings.Contains(res.Stderr, "REVIEW_KEY=[redacted]") {
		t.Fatalf("stderr not redacted: %q", res.Stderr)
	}
	if res.Response.Status != StatusFailed || res.Response.ErrorClass != ErrClassRuntimeError {
		t.Fatalf("got %+v", res.Response)
	}
}

func leakPipeline(t *testing.T, mode string) (*Pipeline, string) {
	t.Helper()
	reg := NewRegistry()
	registerFake(t, reg, "leak", mode, baseIdentity(), fullCaps, func(rt *Runtime) {
		rt.Credentials = []CredentialRef{{EnvName: "REVIEW_KEY", Ref: "vault/review"}}
		rt.Args = append(rt.Args, "--secret-env", "REVIEW_KEY")
	})
	p := newPipeline(reg)
	p.Credentials = MapCredentials{"vault/review": leakSecret}
	return p, leakSecret
}

const leakSecret = `s3cret-"value"<&>`

func TestCredentialsAreRedactedFromRecordedResults(t *testing.T) {
	for _, mode := range []string{FakeModeLeakMalformed, FakeModeLeakField, FakeModeLeakValid} {
		t.Run(mode, func(t *testing.T) {
			p, secret := leakPipeline(t, mode)
			res, err := p.Execute(context.Background(), "leak", baseRequest(t))
			if err != nil {
				t.Fatal(err)
			}
			recorded := mustJSON(t, res) + string(res.RawOutput) + res.Stderr + res.Response.ErrorMessage
			for _, form := range []string{secret, "s3cret-", jsonEscaped(secret, true), jsonEscaped(secret, false)} {
				if strings.Contains(recorded, form) {
					t.Fatalf("%q leaked into result: %s", form, recorded)
				}
			}
			switch mode {
			case FakeModeLeakMalformed:
				if res.Response.ErrorClass != ErrClassOutputMalformed || !strings.Contains(string(res.RawOutput), redactedMarker) {
					t.Fatalf("got %+v raw=%q", res.Response, res.RawOutput)
				}
			case FakeModeLeakField:
				if res.Response.ErrorClass != ErrClassOutputMalformed {
					t.Fatalf("got %+v", res.Response)
				}
			case FakeModeLeakValid:
				if res.Response.ErrorClass != ErrClassRuntimeError || !strings.Contains(res.Response.ErrorMessage, redactedMarker) ||
					res.Response.Identity.ModelRevision != "rev-"+redactedMarker {
					t.Fatalf("got %+v", res.Response)
				}
			}
		})
	}
}

func TestSecretStraddlingStderrCapIsNotLeaked(t *testing.T) {
	p, secret := leakPipeline(t, FakeModeLeakBoundary)
	res, err := p.Execute(context.Background(), "leak", baseRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{secret, jsonEscaped(secret, true), jsonEscaped(secret, false)} {
		for k := 1; k < len(form); k++ {
			if strings.HasSuffix(res.Stderr, form[:k]) {
				t.Fatalf("stderr ends with secret fragment %q", form[:k])
			}
		}
	}
	if len(res.Stderr) > maxCapture {
		t.Fatalf("stderr exceeds cap: %d", len(res.Stderr))
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "slow", FakeModeHang, baseIdentity(), fullCaps, func(rt *Runtime) { rt.Timeout = 300 * time.Millisecond })
	registerFake(t, reg, "hang", FakeModeHang, baseIdentity(), fullCaps)
	p := newPipeline(reg)

	start := time.Now()
	res, err := p.Execute(context.Background(), "slow", baseRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Status != StatusInterrupted || res.Response.ErrorClass != ErrClassTimeout || res.Response.ReviewCompleted {
		t.Fatalf("got %+v", res.Response)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not stop the runtime promptly")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err = p.Execute(ctx, "hang", baseRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Status != StatusInterrupted || res.Response.ErrorClass != ErrClassInterrupted {
		t.Fatalf("got %+v", res.Response)
	}
}

func TestLaunchFailureIsNormalized(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "gone", FakeModeSuccess, baseIdentity(), fullCaps, func(rt *Runtime) {
		rt.Executable = filepath.Join(t.TempDir(), "does-not-exist")
	})
	res, err := newPipeline(reg).Execute(context.Background(), "gone", baseRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Status != StatusFailed || res.Response.ErrorClass != ErrClassLaunchError {
		t.Fatalf("got %+v", res.Response)
	}
}

func TestHostErrors(t *testing.T) {
	reg := NewRegistry()
	registerFake(t, reg, "fake", FakeModeSuccess, baseIdentity(), fullCaps)
	p := newPipeline(reg)

	req := baseRequest(t)
	if _, err := p.Execute(context.Background(), "nope", req); !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("unknown runtime: %v", err)
	}
	bad := req
	bad.Version = 2
	if _, err := p.Execute(context.Background(), "fake", bad); !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("version: %v", err)
	}
	bad = req
	bad.RunID = ""
	if _, err := p.Execute(context.Background(), "fake", bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid: %v", err)
	}
}
