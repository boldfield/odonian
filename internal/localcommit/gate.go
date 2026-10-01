package localcommit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultGateTimeout bounds the whole merge gate (make check + make test). It is kept under ten
// minutes so an approve run from a tool with a 600s command limit finishes or fails cleanly.
const DefaultGateTimeout = 8 * time.Minute

// gateEnvironmentAllowlist is the environment the merge gate's `make` sees by default: the basics
// plus the usual toolchain locations (Go, Rust, Python, Node, Java). The gate runs agent-written
// code, so credentials and tokens in the approver's environment are not passed through; variables
// that commonly embed credentials (package-index URLs, registry auth) are deliberately left out.
// A repository that needs more names them in ODONIAN_GATE_ENV. It does not isolate the filesystem
// (HOME stays readable), which is why the gate also refuses to run outside a sandbox unless
// explicitly allowed.
var gateEnvironmentAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TEMP", "TMP", "TERM", "TZ",
	"LANG", "LC_ALL", "LC_CTYPE", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"GOPATH", "GOROOT", "GOCACHE", "GOMODCACHE", "GOFLAGS", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB",
	"GONOSUMDB", "GOPRIVATE", "GONOPROXY", "CGO_ENABLED",
	"CARGO_HOME", "RUSTUP_HOME", "RUSTUP_TOOLCHAIN", "CARGO_TARGET_DIR",
	"VIRTUAL_ENV", "PYTHONPATH", "PYTHONHOME", "PYENV_ROOT", "POETRY_HOME", "UV_CACHE_DIR", "UV_PYTHON", "PIP_CACHE_DIR",
	"NODE_PATH", "NVM_DIR", "COREPACK_HOME",
	"JAVA_HOME", "GRADLE_USER_HOME", "MAVEN_HOME",
	"DOCKER_HOST",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
}

// gateEnvironmentNames is the allowlist plus the comma-separated names in ODONIAN_GATE_ENV.
func gateEnvironmentNames() []string {
	names := append([]string{}, gateEnvironmentAllowlist...)
	for _, name := range strings.Split(os.Getenv("ODONIAN_GATE_ENV"), ",") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// InSandbox reports whether this process runs inside a Docker sandbox (sbx sets SANDBOX_NAME),
// where running the gate's agent-written code is contained.
func InSandbox() bool {
	return os.Getenv("SANDBOX_NAME") != ""
}

func gateEnvironment() []string {
	var environment []string
	for _, name := range gateEnvironmentNames() {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	return environment
}

// gateError is a merge-gate failure, split by whether the merged code failed its checks
// (testFailure) or the gate could not run properly (make missing, no such target, timeout).
type gateError struct {
	target      string
	testFailure bool
	cause       string
	output      string
}

func (e *gateError) Error() string {
	if e.testFailure {
		return fmt.Sprintf("make %s failed on the merged result: %s (the gate runs with a reduced environment; if the failure is a missing tool or setting rather than the code, name the variables it needs in ODONIAN_GATE_ENV)\n%s", e.target, e.cause, e.output)
	}
	return fmt.Sprintf("the merge gate could not run make %s (an environment problem, not a verdict on the code): %s\n%s", e.target, e.cause, e.output)
}

// runGate runs `make check` then `make test` in dir with a scrubbed environment, streaming output
// to progress, killing the whole process group if the combined run exceeds timeout.
func runGate(dir string, timeout time.Duration, progress io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	for _, target := range []string{"check", "test"} {
		fmt.Fprintf(progress, "running make %s on the merged result (timeout %s)…\n", target, timeout)
		recentOutput := &tailBuffer{limit: 64 * 1024}
		cmd := exec.CommandContext(ctx, "make", target)
		cmd.Dir = dir
		cmd.Env = gateEnvironment()
		cmd.Stdout = io.MultiWriter(progress, recentOutput)
		cmd.Stderr = cmd.Stdout
		cmd.WaitDelay = 5 * time.Second
		killProcessGroupOnCancel(cmd)

		err := cmd.Run()
		if err == nil {
			continue
		}
		output := lastLines(recentOutput.String(), 30)
		var exitErr *exec.ExitError
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return &gateError{target: target, cause: fmt.Sprintf("timed out after %s (raise --gate-timeout if the suite is legitimately slow)", timeout), output: output}
		case !errors.As(err, &exitErr):
			return &gateError{target: target, cause: err.Error(), output: output}
		case strings.Contains(output, "No rule to make target"):
			return &gateError{target: target, cause: "the repository has no `make " + target + "` target", output: output}
		default:
			return &gateError{target: target, testFailure: true, cause: err.Error(), output: output}
		}
	}
	return nil
}

// tailBuffer keeps only the last limit bytes written to it.
type tailBuffer struct {
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if overflow := len(b.data) - b.limit; overflow > 0 {
		b.data = b.data[overflow:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	return string(b.data)
}

func lastLines(text string, count int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}
