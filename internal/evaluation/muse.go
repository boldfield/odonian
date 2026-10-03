package evaluation

// Muse Code adapter: an isolated candidate runtime for comparison reviews. It
// speaks the generic adapter contract (adapter.go) and keeps every Muse-specific
// detail (CLI invocation, auth routing, output normalization) in this file.
//
// What is and is not known about the runtime comes from the official Muse Code
// documentation (https://dev.meta.ai/docs/muse-code and its /extending, /auth
// and /subscriptions pages), which documents: `muse --version`; `muse exec`
// with --json (JSONL events on stdout), --prompt-file, --disable-approval and
// --max-model-steps; exit codes 0, 1, 2, 130 and 143; and the credential
// precedence META_API_KEY, then a stored key, then a stored browser session
// (only the browser session is covered by a Power subscription; any API key is
// billed pay-as-you-go). It does NOT document a model-selection flag, the JSONL
// event schema, or any way to ask which credential is active. The adapter
// therefore never assumes those: it probes `muse exec --help` for the flags it
// needs, collects results through its own marker protocol that does not depend
// on the event schema, and refuses to run unless the billing route is
// constrained: muse runs with a dedicated credential home (HOME is replaced,
// XDG_* dropped) that preflight checks, so an API key stored for some other
// purpose in the operator's own HOME can never outrank the browser session.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	MuseAdapterName    = "muse-code"
	MuseAdapterVersion = "1"
	MuseRuntimeName    = "muse-code-cli"
	// MusePinnedModel is the only model the adapter will run.
	MusePinnedModel   = "muse-spark-1.3"
	MusePromptVersion = "odonian-muse-review/v1"
	// MuseAuthRouteBrowserSession is the operator attestation that the
	// dedicated credential home holds only a browser session (the Power
	// subscription) and that `muse auth set` was never run against it.
	MuseAuthRouteBrowserSession = "browser-session"

	museAPIKeyEnv        = "META_API_KEY"
	museResultVersion    = 1
	museDefaultSteps     = 100
	museDefaultTimeout   = 30 * time.Minute
	museDefaultProbe     = 20 * time.Second
	museMaxOutput        = 16 << 20
	museMaxLine          = 4 << 20
	museProbeCapture     = 64 << 10
	museStderrTail       = 4 << 10
	museMessageSnippet   = 200
	museWaitDelay        = 2 * time.Second
	museBeginMarker      = "ODONIAN_RESULT_BEGIN:"
	museEndMarker        = "ODONIAN_RESULT_END:"
	museExitSignalINT    = 130
	museExitSignalTERM   = 143
	museExitUsage        = 2
	museMissingCapPrefix = "cli_flag:"
	museSettingsFile     = "settings.json"
	museHomeMaxEntries   = 5000
	museHomeMaxScanBytes = 1 << 20
)

// museKeyHint matches file names and contents that suggest a stored API key.
var museKeyHint = regexp.MustCompile(`(?i)api[_\-. ]?key`)

// museRequiredFlags are the `muse exec` flags the adapter passes. --model is
// not in the public documentation, so its presence is confirmed per install.
var museRequiredFlags = []string{"--json", "--prompt-file", "--model", "--disable-approval", "--max-model-steps"}

// museAllowedEnv is every variable (or prefix ending in *) the muse child
// inherits. Everything else, API keys above all, is dropped. HOME and XDG_* are
// deliberately absent: the child's HOME is always the dedicated credential home.
var museAllowedEnv = []string{
	"PATH", "USER", "LOGNAME", "LANG", "LC_*", "TERM", "TZ", "TMPDIR",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY",
	"https_proxy", "http_proxy", "no_proxy",
}

// PreflightOutcome classifies a preflight result. The names are specific on
// purpose; the candidate-response error classes are coarser and fixed by the
// adapter contract.
type PreflightOutcome string

const (
	PreflightReady             PreflightOutcome = "ready"
	PreflightRuntimeMissing    PreflightOutcome = "runtime_missing"
	PreflightRuntimeError      PreflightOutcome = "runtime_error"
	PreflightCapabilityMissing PreflightOutcome = "capability_missing"
	// PreflightAuthOverride means META_API_KEY is set; Muse Code would prefer
	// it over the subscription session and bill pay-as-you-go.
	PreflightAuthOverride PreflightOutcome = "auth_override_present"
	// PreflightAuthUnconfirmed means nothing proves the run uses the
	// subscription session (a stored key cannot be detected from outside).
	PreflightAuthUnconfirmed PreflightOutcome = "auth_route_unconfirmed"
	// PreflightAuthMissing means the credential home holds no session at all.
	PreflightAuthMissing PreflightOutcome = "auth_missing"
	// PreflightAuthAmbiguous means the credential home is unsafe to trust: it
	// is shared, is the adapter's own HOME, or shows signs of a stored API key.
	PreflightAuthAmbiguous PreflightOutcome = "auth_ambiguous"
)

// PreflightReport is what a preflight observed. It never contains credentials.
type PreflightReport struct {
	Outcome          PreflightOutcome `json:"outcome"`
	Message          string           `json:"message,omitempty"`
	Executable       string           `json:"executable,omitempty"`
	RuntimeVersion   string           `json:"runtime_version,omitempty"`
	Model            string           `json:"model"`
	AuthRoute        string           `json:"auth_route"`
	MuseHome         string           `json:"muse_home,omitempty"`
	ExecFlagsFound   []string         `json:"exec_flags_found,omitempty"`
	ExecFlagsMissing []string         `json:"exec_flags_missing,omitempty"`
}

// Ready reports whether a run may proceed.
func (r PreflightReport) Ready() bool { return r.Outcome == PreflightReady }

// MuseOptions configures one adapter invocation.
type MuseOptions struct {
	Executable    string        // path or name of the muse binary; default "muse"
	AuthRoute     string        // operator attestation; see MuseAuthRouteBrowserSession
	MuseHome      string        // absolute dedicated credential home; becomes the child's HOME
	AccountPool   string        // recorded in the identity; Unknown when empty
	MaxModelSteps int           // default 100
	Timeout       time.Duration // whole exec run; default 30m
	ProbeTimeout  time.Duration // each preflight probe; default 20s
	Environ       []string      // the adapter's environment (KEY=VALUE)
	Now           func() time.Time
}

func (o MuseOptions) withDefaults() MuseOptions {
	if o.Executable == "" {
		o.Executable = "muse"
	}
	if o.MaxModelSteps <= 0 {
		o.MaxModelSteps = museDefaultSteps
	}
	if o.Timeout <= 0 {
		o.Timeout = museDefaultTimeout
	}
	if o.ProbeTimeout <= 0 {
		o.ProbeTimeout = museDefaultProbe
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

func envLookup(environ []string, name string) (string, bool) {
	for _, e := range environ {
		if k, v, ok := strings.Cut(e, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

func museChildEnv(environ []string, home string) []string {
	out := []string{"HOME=" + home}
	for _, e := range environ {
		k, _, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		for _, allowed := range museAllowedEnv {
			if prefix, wild := strings.CutSuffix(allowed, "*"); (wild && strings.HasPrefix(k, prefix)) || k == allowed {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// museCommand builds a child in its own process group whose whole group is
// killed on cancellation, so a runtime that spawns helpers cannot outlive it.
func museCommand(ctx context.Context, exe string, args, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = museWaitDelay
	return cmd
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncateTo(s, museMessageSnippet)
}

// probe runs a short, non-model muse command and returns its stdout and
// stderr (each bounded).
func (o MuseOptions) probe(ctx context.Context, exe string, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, o.ProbeTimeout)
	defer cancel()
	cmd := museCommand(ctx, exe, args, museChildEnv(o.Environ, o.MuseHome))
	out, errBuf := &capped{limit: museProbeCapture}, &capped{limit: museProbeCapture}
	cmd.Stdout, cmd.Stderr = out, errBuf
	err = cmd.Run()
	_ = killGroup(cmd)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("timed out after %s", o.ProbeTimeout)
	}
	return out.String(), errBuf.String(), err
}

func flagListed(help, flagName string) bool {
	re := regexp.MustCompile(`(?:^|[\s,|\[<(])` + regexp.QuoteMeta(flagName) + `(?:$|[\s=,|\]>)])`)
	return re.MatchString(help)
}

// checkMuseHome decides whether the dedicated credential home can be trusted
// to route billing through the browser session. Muse documents no way to ask
// which credential is active, so the adapter instead prevents the documented
// bypass: the child only ever sees this directory as HOME, and the directory
// must be private, separate from the adapter's own HOME, show Muse session
// state (a non-empty file besides settings.json under .config/muse) and free of anything that looks like a stored API key.
// File contents are inspected in memory only and never reported.
func checkMuseHome(opts MuseOptions) (PreflightOutcome, string) {
	home := opts.MuseHome
	if home == "" {
		return PreflightAuthUnconfirmed, "no dedicated credential home: pass --muse-home with a directory used only by this adapter (HOME=<dir> muse, sign in with the Power browser flow)"
	}
	if !filepath.IsAbs(home) {
		return PreflightAuthUnconfirmed, fmt.Sprintf("--muse-home %q must be an absolute path", home)
	}
	info, err := os.Lstat(home)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return PreflightAuthMissing, fmt.Sprintf("credential home %s does not exist; sign in with HOME=%s muse first", home, home)
	case err != nil:
		return PreflightAuthAmbiguous, fmt.Sprintf("credential home %s unreadable: %v", home, err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return PreflightAuthAmbiguous, fmt.Sprintf("credential home %s must be a real directory, not a symlink or file", home)
	case info.Mode().Perm()&0o077 != 0:
		return PreflightAuthAmbiguous, fmt.Sprintf("credential home %s is accessible to other users (mode %o); chmod 700", home, info.Mode().Perm())
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return PreflightAuthAmbiguous, fmt.Sprintf("credential home %s is not owned by the adapter's user", home)
	}
	if own, _ := envLookup(opts.Environ, "HOME"); own != "" {
		a, errA := filepath.EvalSymlinks(home)
		b, errB := filepath.EvalSymlinks(own)
		if errA == nil && errB == nil && a == b {
			return PreflightAuthAmbiguous, "credential home is the adapter's own HOME, which may hold a stored API key that outranks the subscription session; use a dedicated directory"
		}
	}
	files, hint, sessionFound := 0, "", false
	sessionDir := filepath.Join(home, ".config", "muse")
	walkErr := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == home {
			return nil
		}
		if files++; files > museHomeMaxEntries {
			return errors.New("too many entries to inspect")
		}
		rel, _ := filepath.Rel(home, path)
		if museKeyHint.MatchString(d.Name()) {
			hint = rel
			return filepath.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if filepath.Dir(path) == sessionDir && d.Name() != museSettingsFile {
			if fi, err := d.Info(); err == nil && fi.Size() > 0 {
				sessionFound = true
			}
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		buf, err := io.ReadAll(io.LimitReader(f, museHomeMaxScanBytes))
		if err != nil {
			return err
		}
		if museKeyHint.Match(buf) {
			hint = rel
			return filepath.SkipAll
		}
		return nil
	})
	switch {
	case hint != "":
		return PreflightAuthAmbiguous, fmt.Sprintf("credential home contains what looks like a stored API key (%s); run HOME=%s muse logout and sign in again with the browser flow", hint, home)
	case walkErr != nil:
		return PreflightAuthAmbiguous, fmt.Sprintf("credential home could not be fully inspected: %v", walkErr)
	case files == 0:
		return PreflightAuthMissing, fmt.Sprintf("credential home %s is empty; sign in with HOME=%s muse (Power browser flow) first", home, home)
	case !sessionFound:
		return PreflightAuthMissing, fmt.Sprintf("credential home %s holds no recognizable Muse session state (expected a non-empty file other than %s under .config/muse); sign in with HOME=%s muse (Power browser flow) first", home, museSettingsFile, home)
	}
	return PreflightReady, ""
}

// MusePreflight checks the installed runtime and the billing route without a
// model call. It runs only `muse --version` and `muse exec --help`, and it
// never starts muse at all while the billing route is ambiguous.
func MusePreflight(ctx context.Context, opts MuseOptions) PreflightReport {
	opts = opts.withDefaults()
	rep := PreflightReport{Model: MusePinnedModel, AuthRoute: opts.AuthRoute}
	fail := func(o PreflightOutcome, format string, a ...any) PreflightReport {
		rep.Outcome, rep.Message = o, fmt.Sprintf(format, a...)
		return rep
	}

	exe, err := exec.LookPath(opts.Executable)
	if err != nil {
		return fail(PreflightRuntimeMissing, "muse executable %q not found or not executable", opts.Executable)
	}
	if abs, err := filepath.Abs(exe); err == nil {
		exe = abs
	}
	rep.Executable = exe

	if _, set := envLookup(opts.Environ, museAPIKeyEnv); set {
		return fail(PreflightAuthOverride,
			"%s is set; Muse Code prefers it over the subscription session and bills pay-as-you-go. Unset it for this adapter", museAPIKeyEnv)
	}
	if opts.AuthRoute != MuseAuthRouteBrowserSession {
		return fail(PreflightAuthUnconfirmed,
			"billing route is not confirmed: pass --auth-route %s only after the dedicated credential home was signed in with the Power browser session and `muse auth set` was never run against it",
			MuseAuthRouteBrowserSession)
	}
	if o, msg := checkMuseHome(opts); o != PreflightReady {
		return fail(o, "%s", msg)
	}
	rep.MuseHome = opts.MuseHome

	stdout, stderr, err := opts.probe(ctx, exe, "--version")
	if err != nil {
		return fail(PreflightRuntimeError, "muse --version failed: %v", err)
	}
	version := snippet(stdout)
	if version == "" {
		version = snippet(stderr)
	}
	if version == "" {
		return fail(PreflightRuntimeError, "muse --version produced no output")
	}
	rep.RuntimeVersion = version

	stdout, stderr, err = opts.probe(ctx, exe, "exec", "--help")
	if err != nil {
		return fail(PreflightRuntimeError, "muse exec --help failed: %v", err)
	}
	help := stdout + "\n" + stderr
	for _, f := range museRequiredFlags {
		if flagListed(help, f) {
			rep.ExecFlagsFound = append(rep.ExecFlagsFound, f)
		} else {
			rep.ExecFlagsMissing = append(rep.ExecFlagsMissing, f)
		}
	}
	if len(rep.ExecFlagsMissing) > 0 {
		return fail(PreflightCapabilityMissing, "installed muse %q does not list required `muse exec` flags: %s",
			rep.RuntimeVersion, strings.Join(rep.ExecFlagsMissing, ", "))
	}
	rep.Outcome = PreflightReady
	return rep
}

func museIdentity(opts MuseOptions, rep PreflightReport, observed bool) CandidateIdentity {
	id := UnknownIdentity()
	id.AdapterName, id.AdapterVersion = MuseAdapterName, MuseAdapterVersion
	id.PromptVersion = MusePromptVersion
	if opts.AccountPool != "" {
		id.AccountPool = opts.AccountPool
	}
	if !observed {
		return id
	}
	id.ModelID = MusePinnedModel
	id.RuntimeName = MuseRuntimeName
	id.RuntimeVersion = rep.RuntimeVersion
	id.GenerationSettings = KnownSettings(map[string]string{"max_model_steps": strconv.Itoa(opts.MaxModelSteps)})
	return id
}

// RunMuse executes one comparison review and returns the normalized response.
// It never returns a completed response unless muse exited zero AND the output
// carried a valid, complete result block for this run.
func RunMuse(ctx context.Context, req CandidateRequest, opts MuseOptions) CandidateResponse {
	opts = opts.withDefaults()
	started := opts.Now()
	resp := CandidateResponse{Version: ProtocolVersion, RunID: req.RunID, Identity: museIdentity(opts, PreflightReport{}, false)}
	finish := func(status Status, class ErrorClass, format string, a ...any) CandidateResponse {
		resp.Status, resp.ErrorClass = status, class
		resp.ErrorMessage = truncate(fmt.Sprintf(format, a...))
		resp.Timing = Timing{StartedAt: started, FinishedAt: opts.Now()}
		return resp
	}

	if missing := CheckCapabilities(req.ToolAccess, []string{CapStructuredOutput}); len(missing) > 0 {
		resp.MissingCapabilities = missing
		return finish(StatusUnsupported, ErrClassCapabilityMissing, "the Muse adapter declares only %s", CapStructuredOutput)
	}

	rep := MusePreflight(ctx, opts)
	if !rep.Ready() && ctx.Err() != nil {
		return finish(StatusInterrupted, ErrClassInterrupted, "run interrupted during preflight")
	}
	switch rep.Outcome {
	case PreflightReady:
	case PreflightRuntimeMissing:
		return finish(StatusFailed, ErrClassLaunchError, "preflight %s: %s", rep.Outcome, rep.Message)
	case PreflightRuntimeError:
		return finish(StatusFailed, ErrClassRuntimeError, "preflight %s: %s", rep.Outcome, rep.Message)
	case PreflightCapabilityMissing:
		for _, f := range rep.ExecFlagsMissing {
			resp.MissingCapabilities = append(resp.MissingCapabilities, museMissingCapPrefix+f)
		}
		return finish(StatusUnsupported, ErrClassCapabilityMissing, "preflight %s: %s", rep.Outcome, rep.Message)
	default:
		return finish(StatusFailed, ErrClassAuthMissing, "preflight %s: %s", rep.Outcome, rep.Message)
	}
	resp.Identity = museIdentity(opts, rep, true)

	tmp, err := os.MkdirTemp("", "odonian-muse-*")
	if err != nil {
		return finish(StatusFailed, ErrClassLaunchError, "stage prompt: %v", err)
	}
	defer os.RemoveAll(tmp)
	nonce, err := newNonce()
	if err != nil {
		return finish(StatusFailed, ErrClassLaunchError, "nonce: %v", err)
	}
	promptPath := filepath.Join(tmp, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte(museReviewPrompt(req.BlindedPrompt, nonce)), 0o600); err != nil {
		return finish(StatusFailed, ErrClassLaunchError, "stage prompt: %v", err)
	}

	runCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	cmd := museCommand(runCtx, rep.Executable, []string{
		"exec", "--json", "--model", MusePinnedModel, "--disable-approval",
		"--max-model-steps", strconv.Itoa(opts.MaxModelSteps), "--prompt-file", promptPath,
	}, museChildEnv(opts.Environ, opts.MuseHome))
	cmd.Dir = req.SnapshotPath
	events := &museEvents{kill: cancel}
	stderr := &capped{limit: museStderrTail}
	cmd.Stdout, cmd.Stderr = events, stderr
	runErr := cmd.Run()
	_ = killGroup(cmd)
	events.flush()

	switch {
	case events.fatal != nil:
		return finish(StatusFailed, ErrClassOutputMalformed, "muse output rejected: %v", events.fatal)
	case runErr != nil && ctx.Err() != nil:
		return finish(StatusInterrupted, ErrClassInterrupted, "run interrupted")
	case runErr != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		return finish(StatusInterrupted, ErrClassTimeout, "muse exec exceeded %s", opts.Timeout)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return finish(StatusFailed, ErrClassLaunchError, "muse exec could not be launched: %v", runErr)
		}
		code := exitErr.ExitCode()
		switch code {
		case -1, museExitSignalINT, museExitSignalTERM:
			return finish(StatusInterrupted, ErrClassInterrupted, "muse exec terminated by signal (exit %d)", code)
		case museExitUsage:
			return finish(StatusFailed, ErrClassRuntimeError, "muse exec reported a usage error (exit 2), likely an incompatible flag: %s", snippet(stderr.String()))
		}
		return finish(StatusFailed, ErrClassRuntimeError, "muse exec exited with status %d: %s", code, snippet(stderr.String()))
	}

	if events.lines == 0 {
		return finish(StatusFailed, ErrClassOutputMissing, "muse exec exited zero but emitted no JSONL events")
	}
	findings, found, perr := extractMuseResult(events.groups, nonce)
	switch {
	case perr != nil:
		return finish(StatusFailed, ErrClassOutputMalformed, "muse result block rejected: %v", perr)
	case !found:
		return finish(StatusFailed, ErrClassOutputMissing, "muse exec exited zero but never emitted a complete result block")
	}
	resp.Status, resp.ReviewCompleted, resp.Findings = StatusCompleted, true, findings
	resp.Timing = Timing{StartedAt: started, FinishedAt: opts.Now()}
	if err := resp.Validate(); err != nil {
		resp.Status, resp.ReviewCompleted, resp.Findings = "", false, nil
		return finish(StatusFailed, ErrClassOutputMalformed, "normalized response invalid: %v", err)
	}
	return resp
}

func newNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// museReviewPrompt appends the output contract. The literal begin/end markers
// are never written next to the nonce here, so an echo of the prompt in the
// event stream cannot be mistaken for a result.
func museReviewPrompt(blinded, nonce string) string {
	return blinded + `

---
Output contract. When your review is finished, end your final message with exactly one result block:
a line made of the text ODONIAN_RESULT_BEGIN, a colon, and the nonce below with no spaces; then a single JSON object; then a line made of the text ODONIAN_RESULT_END, a colon, and the same nonce.
The JSON object has exactly these keys: "odonian_result_version" (the number 1), "nonce" (the nonce below), "complete" (true only if you finished the whole review), and "findings" (an array, empty if you found nothing).
Each finding has exactly these keys: "id" (a short unique string), "severity" (one of "material", "minor", "note"), "claim" (the statement under review, or an empty string), "summary" (non-empty), "evidence" (a string, possibly empty).
Do not emit the block until the review is finished.
Nonce: ` + nonce + "\n"
}

// museEvents collects the JSONL stdout stream. It always accepts writes so the
// child never blocks on a full pipe; on the first protocol violation it kills
// the run and discards the rest.
type museEvents struct {
	kill   func()
	buf    []byte
	total  int
	lines  int
	groups map[string][]string // string values by key path, in arrival order
	fatal  error
}

func (e *museEvents) fail(err error) {
	if e.fatal == nil {
		e.fatal = err
		e.kill()
	}
}

func (e *museEvents) Write(p []byte) (int, error) {
	if e.fatal != nil {
		return len(p), nil
	}
	e.total += len(p)
	if e.total > museMaxOutput {
		e.fail(fmt.Errorf("output exceeds %d bytes", museMaxOutput))
		return len(p), nil
	}
	e.buf = append(e.buf, p...)
	for e.fatal == nil {
		i := bytes.IndexByte(e.buf, '\n')
		if i < 0 {
			break
		}
		line := e.buf[:i]
		e.buf = e.buf[i+1:]
		e.line(line)
	}
	if e.fatal == nil && len(e.buf) > museMaxLine {
		e.fail(fmt.Errorf("event line exceeds %d bytes", museMaxLine))
	}
	return len(p), nil
}

func (e *museEvents) flush() {
	if e.fatal == nil && len(e.buf) > 0 {
		e.line(e.buf)
	}
	e.buf = nil
}

func (e *museEvents) line(raw []byte) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return
	}
	texts, err := jsonlEventStrings(raw)
	if err != nil {
		e.fail(fmt.Errorf("event %d is not a JSON object: %v", e.lines+1, err))
		return
	}
	e.lines++
	if e.groups == nil {
		e.groups = map[string][]string{}
	}
	for _, t := range texts {
		e.groups[t.path] = append(e.groups[t.path], t.value)
	}
}

// eventString is one string value of an event and the key path it sits at.
type eventString struct{ path, value string }

// jsonlEventStrings validates that line is one JSON object and returns its
// string values (not keys) in document order, each tagged with its key path.
func jsonlEventStrings(line []byte) ([]eventString, error) {
	if line[0] != '{' || !json.Valid(line) {
		return nil, errors.New("not a single JSON object")
	}
	type frame struct {
		object    bool
		key       string
		expectKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	var out []eventString
	var stack []frame
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].object {
			stack[n-1].expectKey = true
		}
	}
	pathOf := func() string {
		parts := make([]string, len(stack))
		for i, f := range stack {
			parts[i] = "[]"
			if f.object {
				parts[i] = f.key
			}
		}
		return strings.Join(parts, "/")
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{object: true, expectKey: true})
			case '[':
				stack = append(stack, frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		case string:
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				stack[n-1].key, stack[n-1].expectKey = t, false
				continue
			}
			out = append(out, eventString{pathOf(), t})
			valueDone()
		default:
			valueDone()
		}
	}
}

type museResultBlock struct {
	Version  int        `json:"odonian_result_version"`
	Nonce    string     `json:"nonce"`
	Complete *bool      `json:"complete"`
	Findings *[]Finding `json:"findings"`
}

// extractMuseResult scans the event strings for result blocks carrying this
// run's nonce. The event schema is undocumented, so each key path (for example
// a "text" or "delta" field) is treated as its own text stream. It returns found=false when no block exists at all, an error
// when blocks exist but none is a valid complete result (or valid blocks
// disagree), and the findings otherwise. Each stream is scanned both joined by
// newlines (whole messages) and joined directly (streamed deltas).
func extractMuseResult(groups map[string][]string, nonce string) (findings []Finding, found bool, err error) {
	begin, end := museBeginMarker+nonce, museEndMarker+nonce
	var candidates []string
	paths := make([]string, 0, len(groups))
	for p := range groups {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, sep := range []string{"\n", ""} {
		for _, path := range paths {
			candidates = append(candidates, blockCandidates(strings.Join(groups[path], sep), begin, end)...)
		}
	}
	if len(candidates) == 0 {
		return nil, false, nil
	}
	return pickMuseResult(candidates, nonce)
}

func blockCandidates(text, begin, end string) []string {
	var candidates []string
	seen := map[int]bool{}
	for from := 0; ; {
		ei := strings.Index(text[from:], end)
		if ei < 0 {
			return candidates
		}
		ei += from
		from = ei + len(end)
		bi := strings.LastIndex(text[:ei], begin)
		if bi < 0 || seen[bi] {
			continue
		}
		seen[bi] = true
		candidates = append(candidates, text[bi+len(begin):ei])
	}
}

func pickMuseResult(candidates []string, nonce string) (findings []Finding, found bool, err error) {
	var good []byte
	var firstErr error
	for _, c := range candidates {
		f, err := parseMuseBlock(c, nonce)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		canon, _ := json.Marshal(f)
		if good != nil && !bytes.Equal(good, canon) {
			return nil, false, errors.New("multiple differing result blocks")
		}
		if good == nil {
			good, findings = canon, f
		}
	}
	if good == nil {
		return nil, false, firstErr
	}
	return findings, true, nil
}

func parseMuseBlock(raw, nonce string) ([]Finding, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "```json"))
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "```"))
	raw = strings.TrimSpace(strings.TrimSuffix(raw, "```"))
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var b museResultBlock
	if err := dec.Decode(&b); err != nil {
		return nil, fmt.Errorf("decode: %v", err)
	}
	if dec.More() {
		return nil, errors.New("trailing content after the JSON object")
	}
	switch {
	case b.Version != museResultVersion:
		return nil, fmt.Errorf("odonian_result_version %d, want %d", b.Version, museResultVersion)
	case b.Nonce != nonce:
		return nil, errors.New("nonce mismatch")
	case b.Complete == nil:
		return nil, errors.New("complete is required")
	case !*b.Complete:
		return nil, errors.New("block reports complete=false")
	case b.Findings == nil:
		return nil, errors.New("findings is required (use [] for none)")
	}
	if err := validateFindings(*b.Findings); err != nil {
		return nil, err
	}
	return *b.Findings, nil
}

// MuseMain is the adapter entry point.
//
//	muse-adapter --request FILE --muse-home DIR --auth-route browser-session [--muse-bin PATH] ...
//	muse-adapter --preflight --muse-home DIR --auth-route browser-session [--muse-bin PATH]
//
// Run mode reads the host-staged request and writes a CandidateResponse to the
// request's result_path; it exits 0 whenever a valid response was written and
// 2 when the adapter itself could not operate. Preflight mode prints a
// PreflightReport as JSON on stdout and exits 0 when ready, 1 when not.
func MuseMain(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return museMain(ctx, args, os.Environ(), stdout, stderr)
}

func museMain(ctx context.Context, args, environ []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("muse-adapter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reqPath := fs.String("request", "", "request file (run mode)")
	preflight := fs.Bool("preflight", false, "check runtime, flags and billing route without a model call")
	museBin := fs.String("muse-bin", "muse", "muse executable (path or name)")
	museHome := fs.String("muse-home", "", "absolute dedicated credential home; used as muse's HOME")
	authRoute := fs.String("auth-route", "", "operator attestation of the billing route; must be "+MuseAuthRouteBrowserSession)
	pool := fs.String("account-pool", "", "account pool name recorded in the identity")
	steps := fs.Int("max-model-steps", museDefaultSteps, "muse exec --max-model-steps")
	timeout := fs.Duration("timeout", museDefaultTimeout, "limit for one muse exec run")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts := MuseOptions{
		Executable: *museBin, AuthRoute: *authRoute, MuseHome: *museHome, AccountPool: *pool,
		MaxModelSteps: *steps, Timeout: *timeout, Environ: environ,
	}

	if *preflight {
		rep := MusePreflight(ctx, opts)
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintln(stderr, "muse: encode preflight:", err)
			return 2
		}
		if rep.Ready() {
			return 0
		}
		return 1
	}

	if *reqPath == "" {
		fmt.Fprintln(stderr, "muse: --request or --preflight is required")
		return 2
	}
	data, err := os.ReadFile(*reqPath)
	if err != nil {
		fmt.Fprintln(stderr, "muse: read request:", err)
		return 2
	}
	var req CandidateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintln(stderr, "muse: decode request:", err)
		return 2
	}
	if err := req.Validate(); err != nil {
		fmt.Fprintln(stderr, "muse: invalid request:", err)
		return 2
	}

	resp := RunMuse(ctx, req, opts)
	if err := resp.Validate(); err != nil {
		fmt.Fprintln(stderr, "muse: adapter produced an invalid response:", err)
		return 2
	}
	b, err := json.Marshal(resp)
	if err != nil {
		fmt.Fprintln(stderr, "muse: encode result:", err)
		return 2
	}
	if err := os.WriteFile(req.ResultPath, b, 0o600); err != nil {
		fmt.Fprintln(stderr, "muse: write result:", err)
		return 2
	}
	return 0
}
