package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// Fake adapter fixture modes. The fake performs no inference and needs no
// credentials; it exists so the host pipeline can be exercised end to end
// through a real registered executable.
const (
	FakeModeSuccess     = "success"
	FakeModeIncomplete  = "incomplete"
	FakeModeUnsupported = "unsupported"
	FakeModeInterrupted = "interrupted"
	FakeModeFailed      = "failed"
	FakeModeMalformed   = "malformed" // truncated, undecodable JSON
	FakeModeInvalid     = "invalid"   // decodable JSON violating the protocol invariants
	FakeModeCrash       = "crash"     // non-zero exit, no result
	FakeModeSilent      = "silent"    // exit zero, no result
	FakeModeHang        = "hang"      // never finishes
	FakeModeProbe       = "probe"     // success plus a dump of argv/env/cwd next to the result
	FakeModeLeak        = "leak"      // prints its environment to stderr, then exits non-zero
	FakeModeWrongRun    = "wrongrun"  // success, but for a different run ID
	FakeModeDirtyExit   = "dirtyexit" // success result, but exits non-zero

	// Credential-leak fixtures: the adapter echoes the credential held in the
	// environment variable named by --secret-env into places the host records.
	FakeModeLeakMalformed = "leakmalformed" // undecodable result containing the secret
	FakeModeLeakField     = "leakfield"     // decodable result whose unknown field name is the secret
	FakeModeLeakValid     = "leakvalid"     // valid result carrying the secret in every free-text field
	FakeModeLeakBoundary  = "leakboundary"  // stderr where the secret straddles the host capture cap
)

// FakeProbePath is where probe mode records what the child observed.
func FakeProbePath(resultPath string) string { return resultPath + ".probe" }

// FakeProbe is what probe mode records.
type FakeProbe struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
	Cwd  string   `json:"cwd"`
}

// FakeMain is the fake adapter's entry point: `--mode M --request FILE
// [--identity JSON]`. It reads the host-staged request, writes its result to
// the request's result_path, and returns a process exit code.
func FakeMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("fake-adapter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	mode := fs.String("mode", "", "fixture mode")
	reqPath := fs.String("request", "", "request file")
	identityJSON := fs.String("identity", "", "effective identity as JSON")
	secretEnv := fs.String("secret-env", "", "environment variable holding a credential to leak (leak fixtures)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	data, err := os.ReadFile(*reqPath)
	if err != nil {
		fmt.Fprintln(stderr, "fake: read request:", err)
		return 2
	}
	var req CandidateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintln(stderr, "fake: decode request:", err)
		return 2
	}
	if err := req.Validate(); err != nil {
		fmt.Fprintln(stderr, "fake: invalid request:", err)
		return 2
	}
	identity := UnknownIdentity()
	if *identityJSON != "" {
		if err := json.Unmarshal([]byte(*identityJSON), &identity); err != nil {
			fmt.Fprintln(stderr, "fake: decode identity:", err)
			return 2
		}
	}
	write := func(b []byte) int {
		if err := os.WriteFile(req.ResultPath, b, 0o600); err != nil {
			fmt.Fprintln(stderr, "fake: write result:", err)
			return 2
		}
		return 0
	}
	writeResp := func(r CandidateResponse) int {
		b, err := json.Marshal(r)
		if err != nil {
			fmt.Fprintln(stderr, "fake: encode result:", err)
			return 2
		}
		return write(b)
	}
	base := CandidateResponse{Version: ProtocolVersion, RunID: req.RunID, Identity: identity}
	secret := os.Getenv(*secretEnv)

	switch *mode {
	case FakeModeLeakMalformed:
		return write([]byte(`{"version":1,"run_id":"` + req.RunID + `","status":"comple <` + secret + `>`))
	case FakeModeLeakField:
		return write([]byte(`{"version":1,"run_id":"` + req.RunID + `","` + secret + `":1}`))
	case FakeModeLeakValid:
		base.Status, base.ErrorClass, base.ErrorMessage = StatusFailed, ErrClassRuntimeError, "fake: auth failed for "+secret
		base.Identity.ModelRevision = "rev-" + secret
		return writeResp(base)
	case FakeModeLeakBoundary:
		// Units of growing padding put secret starts at every offset around the
		// host's capture limit, so some secret straddles it whatever the limit is.
		// Earlier secrets shrink under redaction, pulling a leftover fragment
		// from beyond the cap to inside it.
		fmt.Fprint(stderr, strings.Repeat(secret, 200))
		fmt.Fprint(stderr, strings.Repeat("x", maxCapture-204*len(secret)))
		for i := 0; i < 8*len(secret); i++ {
			fmt.Fprint(stderr, strings.Repeat("x", i)+secret)
		}
		return 3
	case FakeModeSuccess, FakeModeProbe:
		if *mode == FakeModeProbe {
			if code := writeProbe(req, stderr); code != 0 {
				return code
			}
		}
		base.Status, base.ReviewCompleted = StatusCompleted, true
		base.Findings = fakeFindings(req)
		base.Usage = map[string]float64{"fake_units": float64(len(req.BlindedPrompt))}
		return writeResp(base)
	case FakeModeWrongRun:
		base.RunID = req.RunID + "-other"
		base.Status, base.ReviewCompleted = StatusCompleted, true
		return writeResp(base)
	case FakeModeDirtyExit:
		base.Status, base.ReviewCompleted = StatusCompleted, true
		if code := writeResp(base); code != 0 {
			return code
		}
		return 1
	case FakeModeLeak:
		for _, e := range os.Environ() {
			fmt.Fprintln(stderr, e)
		}
		return 3
	case FakeModeIncomplete:
		base.Status, base.ErrorClass, base.ErrorMessage = StatusIncomplete, ErrClassSourceUnavailable, "fake: source could not be retrieved"
		return writeResp(base)
	case FakeModeUnsupported:
		base.Status, base.ErrorClass, base.ErrorMessage = StatusUnsupported, ErrClassCapabilityMissing, "fake: capability not available"
		base.MissingCapabilities = []string{CapPDFAccess}
		return writeResp(base)
	case FakeModeInterrupted:
		base.Status, base.ErrorClass, base.ErrorMessage = StatusInterrupted, ErrClassInterrupted, "fake: interrupted"
		if code := writeResp(base); code != 0 {
			return code
		}
		return 130
	case FakeModeFailed:
		base.Status, base.ErrorClass, base.ErrorMessage = StatusFailed, ErrClassRuntimeError, "fake: runtime failure"
		if code := writeResp(base); code != 0 {
			return code
		}
		return 1
	case FakeModeMalformed:
		return write([]byte(`{"version":1,"run_id":"` + req.RunID + `","status":"comple`))
	case FakeModeInvalid:
		base.Status, base.ReviewCompleted = StatusCompleted, false
		return writeResp(base)
	case FakeModeCrash:
		fmt.Fprintln(stderr, "fake: crashing")
		return 3
	case FakeModeSilent:
		return 0
	case FakeModeHang:
		time.Sleep(time.Minute)
		return 0
	default:
		fmt.Fprintf(stderr, "fake: unknown mode %q\n", *mode)
		return 2
	}
}

func writeProbe(req CandidateRequest, stderr io.Writer) int {
	cwd, _ := os.Getwd()
	env := os.Environ()
	sort.Strings(env)
	b, err := json.Marshal(FakeProbe{Args: os.Args[1:], Env: env, Cwd: cwd})
	if err == nil {
		err = os.WriteFile(FakeProbePath(req.ResultPath), b, 0o600)
	}
	if err != nil {
		fmt.Fprintln(stderr, "fake: write probe:", err)
		return 2
	}
	return 0
}

// fakeFindings derives deterministic findings from the request alone.
func fakeFindings(req CandidateRequest) []Finding {
	sum := sha256.Sum256([]byte(req.RunID + "\x00" + req.BlindedPrompt))
	tag := hex.EncodeToString(sum[:4])
	fs := []Finding{{
		ID: "fake-" + tag + "-1", Severity: SeverityMaterial,
		Claim: "claim-1", Summary: "fake material finding " + tag, Evidence: "snapshot " + req.SnapshotPath,
	}}
	if req.ToolAccess.RequireSourceRetrieval {
		fs = append(fs, Finding{ID: "fake-" + tag + "-2", Severity: SeverityNote, Summary: "fake source note " + strings.Join(req.ToolAccess.RequiredCapabilities(), ",")})
	}
	return fs
}
