package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	maxResultBytes = 4 << 20
	maxCapture     = 8 << 10
	rawOutputKeep  = 4 << 10
	killWaitDelay  = 2 * time.Second
	// childPath is the only PATH a runtime sees unless it is passed through.
	childPath = "/usr/local/bin:/usr/bin:/bin"
)

// ErrUnknownRuntime means the requested runtime is not registered.
var ErrUnknownRuntime = errors.New("unknown runtime")

// CredentialResolver resolves a registered credential reference to a secret.
// Implementations live with the host; secrets never appear in registrations,
// requests, results or logs.
type CredentialResolver interface {
	Resolve(ref string) (secret string, ok bool)
}

// MapCredentials is a CredentialResolver over a fixed map.
type MapCredentials map[string]string

// Resolve implements CredentialResolver.
func (m MapCredentials) Resolve(ref string) (string, bool) {
	v, ok := m[ref]
	return v, ok && v != ""
}

// Result is the host's record of one run: the normalized response plus the
// declared candidate identity digest and launch diagnostics.
type Result struct {
	Runtime         string
	CandidateDigest string
	Response        CandidateResponse
	Launched        bool
	ExitCode        int    // -1 when not launched or killed by a signal
	RawOutput       []byte // leading bytes of the result file when it was malformed
	Stderr          string // bounded, credential-redacted
}

// Pipeline is the single path every candidate takes: validate request ->
// capability preflight -> resolve credentials -> invoke the registered argv ->
// read, validate and normalize the result. Failures of the candidate are
// normalized into the response; a returned error means the host itself could
// not proceed (invalid request, unknown runtime, local I/O).
type Pipeline struct {
	Registry    *Registry
	Credentials CredentialResolver
	Now         func() time.Time
}

func (p *Pipeline) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// RequestPathFor is where the host stages the request for a result path.
func RequestPathFor(resultPath string) string { return resultPath + ".request" }

// Execute runs one request through the named registered runtime.
func (p *Pipeline) Execute(ctx context.Context, runtimeName string, req CandidateRequest) (Result, error) {
	if err := req.Validate(); err != nil {
		return Result{}, err
	}
	rt, err := p.Registry.Get(runtimeName)
	if err != nil {
		return Result{}, err
	}
	res := Result{Runtime: rt.Name, CandidateDigest: rt.Candidate.Digest(), ExitCode: -1}
	started := p.now()

	if missing := CheckCapabilities(req.ToolAccess, rt.Capabilities); len(missing) > 0 {
		resp := p.synth(req, StatusUnsupported, ErrClassCapabilityMissing,
			"runtime lacks required capabilities", started)
		resp.MissingCapabilities = missing
		return p.finish(res, resp)
	}

	env, secrets, missingRef := p.childEnv(rt)
	rd := newRedactor(secrets)
	if missingRef != "" {
		resp := p.synth(req, StatusFailed, ErrClassAuthMissing,
			fmt.Sprintf("credential reference %q is not resolvable", missingRef), started)
		return p.finish(res, resp)
	}

	if err := os.MkdirAll(filepath.Dir(req.ResultPath), 0o700); err != nil {
		return res, fmt.Errorf("stage result dir: %w", err)
	}
	if err := os.Remove(req.ResultPath); err != nil && !os.IsNotExist(err) {
		return res, fmt.Errorf("clear stale result: %w", err)
	}
	reqPath := RequestPathFor(req.ResultPath)
	data, err := json.Marshal(req)
	if err != nil {
		return res, fmt.Errorf("encode request: %w", err)
	}
	if err := os.WriteFile(reqPath, data, 0o600); err != nil {
		return res, fmt.Errorf("stage request: %w", err)
	}

	res.Launched = true
	runErr, timedOut, stderr := p.launch(ctx, rt, req, reqPath, env, rd)
	res.Stderr = stderr
	finished := p.now()
	var exitErr *exec.ExitError
	isExit := errors.As(runErr, &exitErr)
	if runErr == nil {
		res.ExitCode = 0
	} else if isExit {
		res.ExitCode = exitErr.ExitCode()
	}
	fin := func(resp CandidateResponse) (Result, error) {
		resp = rd.response(resp)
		resp.Timing = Timing{StartedAt: started, FinishedAt: finished}
		if err := resp.Validate(); err != nil {
			resp = p.synth(req, StatusFailed, ErrClassOutputMalformed, "result rejected after redaction: "+rd.str(err.Error()), started)
			resp.Timing = Timing{StartedAt: started, FinishedAt: finished}
		}
		return p.finish(res, resp)
	}

	switch {
	case runErr != nil && timedOut:
		return fin(p.synth(req, StatusInterrupted, ErrClassTimeout, "runtime exceeded its timeout", started))
	case runErr != nil && ctx.Err() != nil:
		return fin(p.synth(req, StatusInterrupted, ErrClassInterrupted, "run cancelled by host", started))
	case runErr != nil && !isExit:
		return fin(p.synth(req, StatusFailed, ErrClassLaunchError, "runtime could not be launched", started))
	case runErr != nil && res.ExitCode == -1:
		return fin(p.synth(req, StatusInterrupted, ErrClassInterrupted, "runtime terminated by signal", started))
	}

	raw, readErr := readBounded(req.ResultPath)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		class, msg := ErrClassOutputMissing, "runtime exited without writing a result"
		if res.ExitCode != 0 {
			class, msg = ErrClassRuntimeError, fmt.Sprintf("runtime exited with status %d without a result", res.ExitCode)
		}
		return fin(p.synth(req, StatusFailed, class, msg, started))
	case readErr != nil && !errors.Is(readErr, errTooLarge):
		return res, fmt.Errorf("read result: %w", readErr)
	case readErr != nil:
		return fin(p.synth(req, StatusFailed, ErrClassOutputMalformed, readErr.Error(), started))
	}

	resp, decErr := DecodeResponse(raw)
	if decErr == nil && resp.RunID != req.RunID {
		decErr = invalid("response run_id %q does not match request %q", resp.RunID, req.RunID)
	}
	if decErr != nil {
		res.RawOutput = truncateBytes(rd.bytes(raw), rawOutputKeep)
		return fin(p.synth(req, StatusFailed, ErrClassOutputMalformed, "result rejected: "+rd.str(decErr.Error()), started))
	}
	if res.ExitCode != 0 && resp.Status == StatusCompleted {
		return fin(p.synth(req, StatusFailed, ErrClassRuntimeError,
			fmt.Sprintf("runtime exited with status %d but reported a completed review", res.ExitCode), started))
	}
	return fin(resp)
}

func (p *Pipeline) finish(res Result, resp CandidateResponse) (Result, error) {
	if err := resp.Validate(); err != nil {
		return res, fmt.Errorf("host produced an invalid normalized response: %w", err)
	}
	res.Response = resp
	return res, nil
}

// synth builds a host-originated response. The runtime's effective identity
// was never observed, so every effective value is explicitly unknown.
func (p *Pipeline) synth(req CandidateRequest, status Status, class ErrorClass, msg string, started time.Time) CandidateResponse {
	return CandidateResponse{
		Version:      ProtocolVersion,
		RunID:        req.RunID,
		Status:       status,
		ErrorClass:   class,
		ErrorMessage: truncate(msg),
		Identity:     UnknownIdentity(),
		Timing:       Timing{StartedAt: started, FinishedAt: p.now()},
	}
}

// childEnv builds the child's entire environment: a fixed PATH, explicitly
// passed-through host variables and resolved credentials. The host's own
// environment (board tokens, API keys) is never inherited.
func (p *Pipeline) childEnv(rt Runtime) (env, secrets []string, missingRef string) {
	env = []string{"PATH=" + childPath, "ODONIAN_EVAL_PROTOCOL=1"}
	for _, name := range rt.PassThroughEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	for _, c := range rt.Credentials {
		var secret string
		var ok bool
		if p.Credentials != nil {
			secret, ok = p.Credentials.Resolve(c.Ref)
		}
		if !ok {
			return nil, nil, c.Ref
		}
		env = append(env, c.EnvName+"="+secret)
		secrets = append(secrets, secret)
	}
	return env, secrets, ""
}

func (p *Pipeline) launch(ctx context.Context, rt Runtime, req CandidateRequest, reqPath string, env []string, rd *redactor) (runErr error, timedOut bool, stderr string) {
	args := make([]string, len(rt.Args))
	repl := strings.NewReplacer(
		PlaceholderRequestPath, reqPath,
		PlaceholderResultPath, req.ResultPath,
		PlaceholderRunID, req.RunID,
	)
	for i, a := range rt.Args {
		args[i] = repl.Replace(a)
	}
	tctx, cancel := context.WithTimeout(ctx, rt.Timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, rt.Executable, args...)
	cmd.Dir = req.SnapshotPath
	cmd.Env = env
	cmd.WaitDelay = killWaitDelay
	// Capture past the cap by the longest secret so redaction sees any secret
	// that starts inside the cap whole; the cap is applied after redaction.
	out, errBuf := &capped{limit: maxCapture}, &capped{limit: maxCapture + rd.longest}
	cmd.Stdout, cmd.Stderr = out, errBuf
	runErr = cmd.Run()
	timedOut = errors.Is(tctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
	s := rd.str(errBuf.String())
	if errBuf.truncated {
		s = rd.dropPartialTail(s)
	}
	return runErr, timedOut, truncateTo(s, maxCapture)
}

var errTooLarge = errors.New("result exceeds size limit")

func readBounded(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxResultBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResultBytes {
		return nil, errTooLarge
	}
	return data, nil
}

// DecodeResponse strictly decodes and validates adapter output. Unknown
// fields, trailing data and invariant violations are all errors.
func DecodeResponse(data []byte) (CandidateResponse, error) {
	var resp CandidateResponse
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return CandidateResponse{}, invalid("decode: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return CandidateResponse{}, invalid("trailing data after response")
	}
	if err := resp.Validate(); err != nil {
		return CandidateResponse{}, err
	}
	return resp, nil
}

func truncate(s string) string { return truncateTo(s, maxErrorMessage) }

func truncateTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func truncateBytes(b []byte, n int) []byte { return []byte(truncateTo(string(b), n)) }

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

type capped struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := max(c.limit-c.buf.Len(), 0)
	if len(p) > room {
		c.truncated = true
	}
	c.buf.Write(p[:min(room, len(p))])
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }
