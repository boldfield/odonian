package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const museAdapterArg = "__muse_adapter__"

const defaultMuseHelp = `Usage: muse exec [flags] [prompt]

  --json                 stream JSONL events to stdout
  --prompt-file <path>   read the prompt from a file
  --model <id>           model to use
  --disable-approval     skip approval prompts
  --max-model-steps N    cap model steps
`

type fakeMuseCfg struct {
	help      string   // `muse exec --help` text; default lists every required flag
	events    []string // stdout JSONL lines; NONCE is replaced by the run's nonce
	pre       string   // shell run before the events are written
	exit      int
	stderr    string
	version   string // default "muse-cli 9.9.9 (fake)"
	probeHang bool
}

type fakeMuse struct {
	dir, path string
}

func (f fakeMuse) file(name string) string { return filepath.Join(f.dir, name) }

func (f fakeMuse) read(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(f.file(name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func (f fakeMuse) exists(name string) bool {
	_, err := os.Stat(f.file(name))
	return err == nil
}

func writeFakeMuse(t *testing.T, cfg fakeMuseCfg) fakeMuse {
	t.Helper()
	dir := t.TempDir()
	if cfg.help == "" {
		cfg.help = defaultMuseHelp
	}
	if cfg.version == "" {
		cfg.version = "muse-cli 9.9.9 (fake)"
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "help.txt"), []byte(cfg.help), 0o600))
	must(os.WriteFile(filepath.Join(dir, "events.txt"), []byte(strings.Join(cfg.events, "\n")+"\n"), 0o600))
	must(os.WriteFile(filepath.Join(dir, "stderr.txt"), []byte(cfg.stderr), 0o600))
	must(os.WriteFile(filepath.Join(dir, "version.txt"), []byte(cfg.version+"\n"), 0o600))
	hang := ""
	if cfg.probeHang {
		hang = "sleep 30\n"
	}
	script := `#!/bin/sh
dir=@DIR@
touch "$dir/invoked"
if [ "$1" = "--version" ]; then
  @HANG@cat "$dir/version.txt" >&2
  exit 0
fi
if [ "$1" = exec ] && [ "$2" = --help ]; then
  cat "$dir/help.txt"
  exit 0
fi
touch "$dir/exec-invoked"
printf '%s\n' "$@" > "$dir/argv.log"
pwd > "$dir/cwd.log"
env > "$dir/env.log"
while [ $# -gt 0 ]; do
  if [ "$1" = --prompt-file ]; then pf=$2; fi
  shift
done
cp "$pf" "$dir/prompt.log"
nonce=$(sed -n 's/^Nonce: //p' "$pf")
@PRE@
sed "s/NONCE/$nonce/g" "$dir/events.txt"
cat "$dir/stderr.txt" >&2
exit @EXIT@
`
	script = strings.NewReplacer("@DIR@", "'"+dir+"'", "@HANG@", hang, "@PRE@", cfg.pre, "@EXIT@", strconv.Itoa(cfg.exit)).Replace(script)
	path := filepath.Join(dir, "muse")
	must(os.WriteFile(path, []byte(script), 0o755))
	return fakeMuse{dir: dir, path: path}
}

func evLine(t *testing.T, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"type": "message", "meta": map[string]any{"n": 1, "tags": []string{"a"}}, "text": text})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const validBlockJSON = `{"odonian_result_version":1,"nonce":"NONCE","complete":true,"findings":[{"id":"f1","severity":"material","claim":"c1","summary":"s1","evidence":"e1"},{"id":"f2","severity":"note","summary":"s2"}]}`

func blockText(blockJSON string) string {
	return "Review done.\nODONIAN_RESULT_BEGIN:NONCE\n" + blockJSON + "\nODONIAN_RESULT_END:NONCE\n"
}

func goodEvents(t *testing.T) []string {
	return []string{evLine(t, "starting"), evLine(t, blockText(validBlockJSON)), `{"type":"done"}`}
}

// signedInHome creates a private credential home holding a session-like file.
func signedInHome(m fakeMuse) string {
	home := filepath.Join(m.dir, "muse-home")
	if err := os.MkdirAll(filepath.Join(home, ".config", "muse"), 0o700); err != nil {
		panic(err)
	}
	for _, d := range []string{home, filepath.Join(home, ".config"), filepath.Join(home, ".config", "muse")} {
		if err := os.Chmod(d, 0o700); err != nil {
			panic(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "muse", "session.json"), []byte(`{"session":"browser-token"}`), 0o600); err != nil {
		panic(err)
	}
	return home
}

const operatorHome = "/tmp/operator-home"

func museOpts(m fakeMuse) MuseOptions {
	return MuseOptions{
		Executable:   m.path,
		AuthRoute:    MuseAuthRouteBrowserSession,
		MuseHome:     signedInHome(m),
		AccountPool:  "meta-eval-pool",
		Environ:      []string{"PATH=" + os.Getenv("PATH"), "HOME=" + operatorHome, "OTHER_API_KEY=other-secret-value"},
		Timeout:      20 * time.Second,
		ProbeTimeout: 5 * time.Second,
	}
}

func museRequest(t *testing.T) CandidateRequest {
	t.Helper()
	return CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         "run-1",
		SnapshotPath:  t.TempDir(),
		BlindedPrompt: "Review the submission.",
		ResultPath:    filepath.Join(t.TempDir(), "result.json"),
	}
}

func wantFailure(t *testing.T, resp CandidateResponse, status Status, class ErrorClass) {
	t.Helper()
	if resp.Status != status || resp.ErrorClass != class || resp.ReviewCompleted || len(resp.Findings) > 0 {
		t.Fatalf("got status=%s class=%s completed=%v findings=%d msg=%q, want %s/%s",
			resp.Status, resp.ErrorClass, resp.ReviewCompleted, len(resp.Findings), resp.ErrorMessage, status, class)
	}
	if err := resp.Validate(); err != nil {
		t.Fatalf("response violates the adapter contract: %v", err)
	}
}

func TestMusePreflightReadyRecordsRuntime(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{})
	rep := MusePreflight(context.Background(), museOpts(m))
	if !rep.Ready() {
		t.Fatalf("not ready: %+v", rep)
	}
	if rep.RuntimeVersion != "muse-cli 9.9.9 (fake)" || rep.Executable != m.path || rep.Model != MusePinnedModel {
		t.Fatalf("report: %+v", rep)
	}
	if !reflect.DeepEqual(rep.ExecFlagsFound, museRequiredFlags) || len(rep.ExecFlagsMissing) != 0 {
		t.Fatalf("flags: %+v", rep)
	}
	if m.exists("exec-invoked") {
		t.Fatal("preflight must not run muse exec")
	}
}

func TestMusePreflightRuntimeMissing(t *testing.T) {
	opts := museOpts(fakeMuse{path: filepath.Join(t.TempDir(), "no-such-muse")})
	rep := MusePreflight(context.Background(), opts)
	if rep.Outcome != PreflightRuntimeMissing {
		t.Fatalf("got %+v", rep)
	}
	resp := RunMuse(context.Background(), museRequest(t), opts)
	wantFailure(t, resp, StatusFailed, ErrClassLaunchError)
}

func TestMusePreflightVersionFailureAndHang(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{probeHang: true})
	opts := museOpts(m)
	opts.ProbeTimeout = 300 * time.Millisecond
	start := time.Now()
	rep := MusePreflight(context.Background(), opts)
	if rep.Outcome != PreflightRuntimeError || !strings.Contains(rep.Message, "timed out") {
		t.Fatalf("got %+v", rep)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("hung probe was not bounded")
	}
	wantFailure(t, RunMuse(context.Background(), museRequest(t), opts), StatusFailed, ErrClassRuntimeError)
}

func TestMusePreflightIncompatibleFlags(t *testing.T) {
	help := "Usage: muse exec\n  --json\n  --prompt-file F\n  --model-id X\n  --disable-approval\n  --max-model-steps N\n"
	m := writeFakeMuse(t, fakeMuseCfg{help: help})
	opts := museOpts(m)
	rep := MusePreflight(context.Background(), opts)
	if rep.Outcome != PreflightCapabilityMissing || !reflect.DeepEqual(rep.ExecFlagsMissing, []string{"--model"}) {
		t.Fatalf("--model-id must not satisfy --model: %+v", rep)
	}
	resp := RunMuse(context.Background(), museRequest(t), opts)
	wantFailure(t, resp, StatusUnsupported, ErrClassCapabilityMissing)
	if !reflect.DeepEqual(resp.MissingCapabilities, []string{"cli_flag:--model"}) {
		t.Fatalf("missing capabilities: %v", resp.MissingCapabilities)
	}
	if m.exists("exec-invoked") {
		t.Fatal("must not run exec against an incompatible CLI")
	}
}

func TestMusePreflightExecHelpFailure(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{})
	script, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(script), `cat "$dir/help.txt"`, `echo boom >&2; exit 3`, 1)
	if err := os.WriteFile(m.path, []byte(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := MusePreflight(context.Background(), museOpts(m))
	if rep.Outcome != PreflightRuntimeError {
		t.Fatalf("got %+v", rep)
	}
}

func emptyHome(t *testing.T) string {
	t.Helper()
	d := filepath.Join(t.TempDir(), "empty-home")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestMusePreflightAuthRouting(t *testing.T) {
	const keyValue = "sk-super-secret-value"
	cases := []struct {
		name    string
		mutate  func(*MuseOptions)
		outcome PreflightOutcome
	}{
		{"api key override", func(o *MuseOptions) { o.Environ = append(o.Environ, "META_API_KEY="+keyValue) }, PreflightAuthOverride},
		{"empty api key still overrides", func(o *MuseOptions) { o.Environ = append(o.Environ, "META_API_KEY=") }, PreflightAuthOverride},
		{"route unattested", func(o *MuseOptions) { o.AuthRoute = "" }, PreflightAuthUnconfirmed},
		{"wrong attestation", func(o *MuseOptions) { o.AuthRoute = "api-key" }, PreflightAuthUnconfirmed},
		{"no credential home", func(o *MuseOptions) { o.MuseHome = "" }, PreflightAuthUnconfirmed},
		{"relative credential home", func(o *MuseOptions) { o.MuseHome = "muse-home" }, PreflightAuthUnconfirmed},
		{"credential home missing", func(o *MuseOptions) { o.MuseHome = filepath.Join(o.MuseHome, "nope") }, PreflightAuthMissing},
		{"empty credential home", func(o *MuseOptions) { o.MuseHome = emptyHome(t) }, PreflightAuthMissing},
		{"empty operator HOME is not a session", func(o *MuseOptions) {
			o.MuseHome = emptyHome(t)
			o.Environ = []string{"PATH=" + os.Getenv("PATH"), "HOME="}
		}, PreflightAuthMissing},
		{"credential home is the adapter's HOME", func(o *MuseOptions) {
			o.Environ = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + o.MuseHome}
		}, PreflightAuthAmbiguous},
		{"credential home readable by others", func(o *MuseOptions) { _ = os.Chmod(o.MuseHome, 0o750) }, PreflightAuthAmbiguous},
		{"credential home is a symlink", func(o *MuseOptions) {
			link := filepath.Join(t.TempDir(), "link")
			_ = os.Symlink(o.MuseHome, link)
			o.MuseHome = link
		}, PreflightAuthAmbiguous},
		{"stored key file name", func(o *MuseOptions) {
			_ = os.WriteFile(filepath.Join(o.MuseHome, "api_key"), []byte("sk-x"), 0o600)
		}, PreflightAuthAmbiguous},
		{"stored key in file content", func(o *MuseOptions) {
			_ = os.WriteFile(filepath.Join(o.MuseHome, ".config", "muse", "auth.json"), []byte(`{"apiKey":"`+keyValue+`"}`), 0o600)
		}, PreflightAuthAmbiguous},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := writeFakeMuse(t, fakeMuseCfg{events: goodEvents(t)})
			opts := museOpts(m)
			tc.mutate(&opts)
			rep := MusePreflight(context.Background(), opts)
			if rep.Outcome != tc.outcome {
				t.Fatalf("got %+v", rep)
			}
			if m.exists("invoked") {
				t.Fatal("muse must not be started while the billing route is unproven")
			}
			resp := RunMuse(context.Background(), museRequest(t), opts)
			wantFailure(t, resp, StatusFailed, ErrClassAuthMissing)
			if !strings.Contains(resp.ErrorMessage, string(tc.outcome)) {
				t.Fatalf("message should name the specific outcome: %q", resp.ErrorMessage)
			}
			b, _ := json.Marshal(resp)
			if strings.Contains(string(b), keyValue) || strings.Contains(mustJSON(t, rep), keyValue) {
				t.Fatal("credential value leaked into a record")
			}
		})
	}
}

func TestRunMuseCompleted(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{events: goodEvents(t)})
	req := museRequest(t)
	opts := museOpts(m)
	opts.MaxModelSteps = 42
	resp := RunMuse(context.Background(), req, opts)
	if resp.Status != StatusCompleted || !resp.ReviewCompleted {
		t.Fatalf("got %+v", resp)
	}
	if err := resp.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(resp.Findings) != 2 || resp.Findings[0].ID != "f1" || resp.Findings[0].Severity != SeverityMaterial || resp.Findings[1].ID != "f2" {
		t.Fatalf("findings: %+v", resp.Findings)
	}
	id := resp.Identity
	if id.ModelID != MusePinnedModel || id.RuntimeVersion != "muse-cli 9.9.9 (fake)" || id.RuntimeName != MuseRuntimeName ||
		id.AdapterName != MuseAdapterName || id.PromptVersion != MusePromptVersion || id.AccountPool != "meta-eval-pool" {
		t.Fatalf("identity: %+v", id)
	}
	if id.ModelRevision != Unknown || id.ReasoningSettings.Known || id.Tools.Known || id.Observers.Known {
		t.Fatalf("unobservable fields must stay unknown: %+v", id)
	}
	if !id.GenerationSettings.Known || id.GenerationSettings.Values["max_model_steps"] != "42" {
		t.Fatalf("generation settings: %+v", id.GenerationSettings)
	}
	if resp.Usage != nil {
		t.Fatalf("usage is not reported by muse and must stay nil: %v", resp.Usage)
	}
	if resp.Timing.StartedAt.IsZero() || resp.Timing.FinishedAt.Before(resp.Timing.StartedAt) {
		t.Fatalf("timing: %+v", resp.Timing)
	}

	args := strings.Fields(m.read(t, "argv.log"))
	want := []string{"exec", "--json", "--model", MusePinnedModel, "--disable-approval", "--max-model-steps", "42", "--prompt-file"}
	if len(args) != len(want)+1 || !reflect.DeepEqual(args[:len(want)], want) {
		t.Fatalf("argv: %v", args)
	}
	if got := strings.TrimSpace(m.read(t, "cwd.log")); got != req.SnapshotPath {
		t.Fatalf("cwd %q, want snapshot %q", got, req.SnapshotPath)
	}
	prompt := m.read(t, "prompt.log")
	if !strings.HasPrefix(prompt, "Review the submission.") || !strings.Contains(prompt, "\nNonce: ") {
		t.Fatalf("prompt: %q", prompt)
	}
	if strings.Contains(prompt, museBeginMarker) {
		t.Fatal("the prompt must not contain a literal begin marker an echo could replay")
	}
	if _, err := os.Stat(args[len(args)-1]); err == nil {
		t.Fatal("staged prompt file was not cleaned up")
	}
}

func TestRunMuseChildEnvironmentIsAllowlisted(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{events: goodEvents(t)})
	opts := museOpts(m)
	opts.Environ = append(opts.Environ, "ODONIAN_TOKEN=board-token", "AWS_SECRET_ACCESS_KEY=aws", "XDG_CONFIG_HOME=/x", "LC_ALL=C")
	if resp := RunMuse(context.Background(), museRequest(t), opts); resp.Status != StatusCompleted {
		t.Fatalf("got %+v", resp)
	}
	env := m.read(t, "env.log")
	for _, banned := range []string{"OTHER_API_KEY", "ODONIAN_TOKEN", "AWS_SECRET", "META_API_KEY"} {
		if strings.Contains(env, banned) {
			t.Fatalf("child inherited %s:\n%s", banned, env)
		}
	}
	for _, kept := range []string{"HOME=" + opts.MuseHome + "\n", "LC_ALL=C"} {
		if !strings.Contains(env, kept) {
			t.Fatalf("child lost %s:\n%s", kept, env)
		}
	}
	for _, dropped := range []string{operatorHome, "XDG_CONFIG_HOME"} {
		if strings.Contains(env, dropped) {
			t.Fatalf("child inherited %s instead of the dedicated credential home:\n%s", dropped, env)
		}
	}
}

func TestRunMuseStreamedDeltasAndEcho(t *testing.T) {
	full := blockText(validBlockJSON)
	var events []string
	events = append(events, evLine(t, "echo: ODONIAN_RESULT_BEGIN: then prose then ODONIAN_RESULT_END: nothing"))
	// Deltas split inside the markers and inside the JSON, never inside the
	// NONCE placeholder the fake substitutes.
	for _, part := range strings.Split(strings.ReplaceAll(full, "NONCE", "\x00NONCE\x00"), "\x00") {
		for _, chunk := range strings.SplitAfter(part, ",") {
			events = append(events, evLine(t, chunk))
		}
	}
	m := writeFakeMuse(t, fakeMuseCfg{events: events})
	resp := RunMuse(context.Background(), museRequest(t), museOpts(m))
	if resp.Status != StatusCompleted || len(resp.Findings) != 2 {
		t.Fatalf("got %+v", resp)
	}
}

func TestRunMuseCodeFencedBlockAndEmptyFindings(t *testing.T) {
	block := "```json\n" + `{"odonian_result_version":1,"nonce":"NONCE","complete":true,"findings":[]}` + "\n```"
	m := writeFakeMuse(t, fakeMuseCfg{events: []string{evLine(t, "ODONIAN_RESULT_BEGIN:NONCE\n"+block+"\nODONIAN_RESULT_END:NONCE")}})
	resp := RunMuse(context.Background(), museRequest(t), museOpts(m))
	if resp.Status != StatusCompleted || len(resp.Findings) != 0 {
		t.Fatalf("a complete review with no findings is valid: %+v", resp)
	}
}

func TestRunMuseExitZeroWithoutCompletion(t *testing.T) {
	cases := []struct {
		name   string
		events []string
		class  ErrorClass
	}{
		{"no output", nil, ErrClassOutputMissing},
		{"events but no result block", []string{evLine(t, "thinking"), `{"type":"done"}`}, ErrClassOutputMissing},
		{"result block for another nonce", []string{evLine(t, "ODONIAN_RESULT_BEGIN:other\n{}\nODONIAN_RESULT_END:other")}, ErrClassOutputMissing},
		{"complete=false", []string{evLine(t, blockText(strings.Replace(validBlockJSON, `"complete":true`, `"complete":false`, 1)))}, ErrClassOutputMalformed},
		{"complete missing", []string{evLine(t, blockText(`{"odonian_result_version":1,"nonce":"NONCE","findings":[]}`))}, ErrClassOutputMalformed},
		{"findings missing", []string{evLine(t, blockText(`{"odonian_result_version":1,"nonce":"NONCE","complete":true}`))}, ErrClassOutputMalformed},
		{"wrong nonce inside block", []string{evLine(t, blockText(strings.Replace(validBlockJSON, `"nonce":"NONCE"`, `"nonce":"forged"`, 1)))}, ErrClassOutputMalformed},
		{"wrong version", []string{evLine(t, blockText(strings.Replace(validBlockJSON, `"odonian_result_version":1`, `"odonian_result_version":2`, 1)))}, ErrClassOutputMalformed},
		{"unknown field", []string{evLine(t, blockText(strings.Replace(validBlockJSON, `"complete":true`, `"complete":true,"extra":1`, 1)))}, ErrClassOutputMalformed},
		{"invalid severity", []string{evLine(t, blockText(strings.Replace(validBlockJSON, `"material"`, `"catastrophic"`, 1)))}, ErrClassOutputMalformed},
		{"missing summary", []string{evLine(t, blockText(strings.Replace(validBlockJSON, `"summary":"s1"`, `"summary":""`, 1)))}, ErrClassOutputMalformed},
		{"duplicate finding ids", []string{evLine(t, blockText(strings.Replace(validBlockJSON, `"id":"f2"`, `"id":"f1"`, 1)))}, ErrClassOutputMalformed},
		{"not json", []string{evLine(t, blockText(`{"odonian_result_version":1,`))}, ErrClassOutputMalformed},
		{"trailing content", []string{evLine(t, blockText(validBlockJSON+` {}`))}, ErrClassOutputMalformed},
		{"two differing blocks", []string{
			evLine(t, blockText(validBlockJSON)),
			evLine(t, blockText(`{"odonian_result_version":1,"nonce":"NONCE","complete":true,"findings":[]}`)),
		}, ErrClassOutputMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := writeFakeMuse(t, fakeMuseCfg{events: tc.events})
			resp := RunMuse(context.Background(), museRequest(t), museOpts(m))
			wantFailure(t, resp, StatusFailed, tc.class)
		})
	}
}

func TestRunMuseIdenticalDuplicateBlocksAccepted(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{events: []string{evLine(t, blockText(validBlockJSON)), evLine(t, blockText(validBlockJSON))}})
	if resp := RunMuse(context.Background(), museRequest(t), museOpts(m)); resp.Status != StatusCompleted {
		t.Fatalf("got %+v", resp)
	}
}

func TestRunMuseMalformedJSONLDoesNotDeadlock(t *testing.T) {
	cases := map[string]fakeMuseCfg{
		"garbage line then endless output": {pre: `(echo '{"a":1}'; echo 'not json'; yes '{"a":"b"}')`},
		"array event":                      {events: []string{`[1,2,3]`}},
		"two objects on one line":          {events: []string{`{"a":1} {"b":2}`}},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			m := writeFakeMuse(t, cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			start := time.Now()
			resp := RunMuse(ctx, museRequest(t), museOpts(m))
			wantFailure(t, resp, StatusFailed, ErrClassOutputMalformed)
			if time.Since(start) > 15*time.Second {
				t.Fatal("malformed output was not cut off promptly")
			}
		})
	}
}

func TestRunMuseOutputCap(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{pre: "yes '{\"a\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"}'"})
	resp := RunMuse(context.Background(), museRequest(t), museOpts(m))
	wantFailure(t, resp, StatusFailed, ErrClassOutputMalformed)
	if !strings.Contains(resp.ErrorMessage, "exceeds") {
		t.Fatalf("msg: %q", resp.ErrorMessage)
	}
}

func TestRunMuseNonZeroExit(t *testing.T) {
	cases := []struct {
		name   string
		exit   int
		events []string
		status Status
		class  ErrorClass
		msg    string
	}{
		{"failure", 1, nil, StatusFailed, ErrClassRuntimeError, "status 1"},
		{"usage error", 2, nil, StatusFailed, ErrClassRuntimeError, "usage error"},
		{"sigint exit", 130, nil, StatusInterrupted, ErrClassInterrupted, "signal"},
		{"sigterm exit", 143, nil, StatusInterrupted, ErrClassInterrupted, "signal"},
		{"complete-looking block but exit 1", 1, goodEvents(t), StatusFailed, ErrClassRuntimeError, "status 1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := writeFakeMuse(t, fakeMuseCfg{exit: tc.exit, events: tc.events, stderr: "something went wrong"})
			resp := RunMuse(context.Background(), museRequest(t), museOpts(m))
			wantFailure(t, resp, tc.status, tc.class)
			if !strings.Contains(resp.ErrorMessage, tc.msg) {
				t.Fatalf("msg %q lacks %q", resp.ErrorMessage, tc.msg)
			}
		})
	}
}

func TestRunMuseTimeout(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{pre: "sleep 60"})
	opts := museOpts(m)
	opts.Timeout = 400 * time.Millisecond
	start := time.Now()
	resp := RunMuse(context.Background(), museRequest(t), opts)
	wantFailure(t, resp, StatusInterrupted, ErrClassTimeout)
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not stop the run")
	}
}

func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return ""
}

func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived the interruption", pid)
}

func TestRunMuseInterruptionKillsDescendants(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{pre: "sleep 60 &\necho $! > \"$dir/child.pid\"\nwait"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan CandidateResponse, 1)
	go func() { done <- RunMuse(ctx, museRequest(t), museOpts(m)) }()

	pid, err := strconv.Atoi(waitForFile(t, m.file("child.pid")))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case resp := <-done:
		wantFailure(t, resp, StatusInterrupted, ErrClassInterrupted)
		if resp.Timing.StartedAt.IsZero() || resp.Timing.FinishedAt.Before(resp.Timing.StartedAt) {
			t.Fatalf("timing: %+v", resp.Timing)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("RunMuse did not return after cancellation")
	}
	assertProcessGone(t, pid)
}

func TestMuseMainSIGTERMWritesInterruptedResponse(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{pre: "sleep 60 &\necho $! > \"$dir/child.pid\"\nwait"})
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv("HOME", operatorHome)
	t.Setenv(museAPIKeyEnv, "placeholder")
	os.Unsetenv(museAPIKeyEnv)
	req := museRequest(t)
	reqPath := filepath.Join(t.TempDir(), "req.json")
	data, _ := json.Marshal(req)
	if err := os.WriteFile(reqPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// The test process catches SIGTERM too so a mistimed signal cannot kill it.
	guard := make(chan os.Signal, 1)
	signal.Notify(guard, syscall.SIGTERM)
	defer signal.Stop(guard)

	code := make(chan int, 1)
	go func() {
		code <- MuseMain([]string{"--request", reqPath, "--muse-bin", m.path, "--muse-home", signedInHome(m), "--auth-route", MuseAuthRouteBrowserSession}, os.Stdout, os.Stderr)
	}()
	pid, err := strconv.Atoi(waitForFile(t, m.file("child.pid")))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-code:
		if c != 0 {
			t.Fatalf("exit %d", c)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("adapter did not shut down on SIGTERM")
	}
	raw, err := os.ReadFile(req.ResultPath)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DecodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantFailure(t, resp, StatusInterrupted, ErrClassInterrupted)
	assertProcessGone(t, pid)
}

func TestRunMuseRequiresOnlyDeclaredCapabilities(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{events: goodEvents(t)})
	req := museRequest(t)
	req.ToolAccess = ToolAccessRequirements{RequireSourceRetrieval: true, Tools: []string{"web_fetch"}}
	resp := RunMuse(context.Background(), req, museOpts(m))
	wantFailure(t, resp, StatusUnsupported, ErrClassCapabilityMissing)
	if !reflect.DeepEqual(resp.MissingCapabilities, []string{CapSourceRetrieval, "tool:web_fetch"}) {
		t.Fatalf("missing: %v", resp.MissingCapabilities)
	}
	if m.exists("invoked") {
		t.Fatal("unsupported requests must not start muse")
	}
}

func TestMuseMainPreflightCommand(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{})
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + operatorHome}
	var out, errOut strings.Builder

	code := museMain(context.Background(), []string{"--preflight", "--muse-bin", m.path, "--muse-home", signedInHome(m), "--auth-route", MuseAuthRouteBrowserSession}, env, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errOut.String())
	}
	var rep PreflightReport
	if err := json.Unmarshal([]byte(out.String()), &rep); err != nil || !rep.Ready() || rep.RuntimeVersion == "" {
		t.Fatalf("report %q: %v", out.String(), err)
	}

	out.Reset()
	code = museMain(context.Background(), []string{"--preflight", "--muse-bin", m.path}, env, &out, &errOut)
	if code != 1 || !strings.Contains(out.String(), string(PreflightAuthUnconfirmed)) {
		t.Fatalf("exit %d, report %s", code, out.String())
	}
}

func TestMuseMainRunWritesContractValidResult(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{events: goodEvents(t)})
	req := museRequest(t)
	reqPath := filepath.Join(t.TempDir(), "req.json")
	data, _ := json.Marshal(req)
	if err := os.WriteFile(reqPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + operatorHome}
	var out, errOut strings.Builder
	code := museMain(context.Background(), []string{"--request", reqPath, "--muse-bin", m.path, "--muse-home", signedInHome(m), "--auth-route", MuseAuthRouteBrowserSession, "--account-pool", "p1"}, env, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	raw, err := os.ReadFile(req.ResultPath)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := DecodeResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != StatusCompleted || resp.Identity.AccountPool != "p1" {
		t.Fatalf("got %+v", resp)
	}

	if code := museMain(context.Background(), []string{"--request", filepath.Join(t.TempDir(), "missing.json")}, env, &out, &errOut); code != 2 {
		t.Fatalf("missing request file: exit %d", code)
	}
	if code := museMain(context.Background(), nil, env, &out, &errOut); code != 2 {
		t.Fatalf("no mode: exit %d", code)
	}
}

func TestMuseResponsesPassThroughHostPipeline(t *testing.T) {
	m := writeFakeMuse(t, fakeMuseCfg{events: goodEvents(t)})
	cfg, err := NewCandidateConfig(museIdentity(museOpts(m), PreflightReport{RuntimeVersion: "muse-cli 9.9.9 (fake)"}, true))
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry()
	rt := Runtime{
		Name: "muse", Executable: exe, Timeout: 30 * time.Second, Capabilities: []string{CapStructuredOutput}, Candidate: cfg,
		Args:           []string{museAdapterArg, "--request", PlaceholderRequestPath, "--muse-bin", m.path, "--muse-home", signedInHome(m), "--auth-route", MuseAuthRouteBrowserSession},
		PassThroughEnv: []string{"HOME"},
	}
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", operatorHome)
	p := &Pipeline{Registry: reg}
	res, err := p.Execute(context.Background(), "muse", museRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Status != StatusCompleted || len(res.Response.Findings) != 2 {
		t.Fatalf("got %+v stderr=%q", res.Response, res.Stderr)
	}
}

func TestJSONLEventStrings(t *testing.T) {
	got, err := jsonlEventStrings([]byte(`{"k1":"v1","o":{"k2":"v2","a":["x",1,{"k3":"y"},"z"],"n":null},"k4":"v4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := []eventString{{"k1", "v1"}, {"o/k2", "v2"}, {"o/a/[]", "x"}, {"o/a/[]/k3", "y"}, {"o/a/[]", "z"}, {"k4", "v4"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{`[]`, `"s"`, `{"a":1} {}`, `{"a":`, `{} x`} {
		if _, err := jsonlEventStrings([]byte(bad)); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestFlagListed(t *testing.T) {
	help := "  --model <id>   x\n  -m, --max-model-steps=N\n  --input-file F\n  [--json]\n"
	for flag, want := range map[string]bool{
		"--model": true, "--max-model-steps": true, "--json": true, "--input": false, "--input-file": true, "--mod": false,
	} {
		if got := flagListed(help, flag); got != want {
			t.Fatalf("%s: got %v want %v", flag, got, want)
		}
	}
}
