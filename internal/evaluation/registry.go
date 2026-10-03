package evaluation

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Argv placeholders substituted by the host inside a single argument. The
// substituted argument is never re-split or interpreted by a shell.
const (
	PlaceholderRequestPath = "{request_path}"
	PlaceholderResultPath  = "{result_path}"
	PlaceholderRunID       = "{run_id}"
)

var (
	placeholderRE = regexp.MustCompile(`\{[a-z_]+\}`)
	envNameRE     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	shellNames    = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true, "csh": true, "tcsh": true, "ash": true}
)

// hostEnvPrefix names the host's own configuration/credentials; it can never
// be forwarded into or injected as a runtime's environment.
const hostEnvPrefix = "ODONIAN_"

// CredentialRef maps a host-resolved credential into one environment variable
// of the child process. Only the reference is registered, never the secret.
type CredentialRef struct {
	EnvName string `json:"env_name"`
	Ref     string `json:"ref"`
}

// Runtime is a trusted registration: an executable plus an argument list and
// credential references, never shell text taken from a task or campaign.
type Runtime struct {
	Name           string
	Executable     string // absolute path to the binary
	Args           []string
	PassThroughEnv []string // host env var names copied to the child (explicit allowlist)
	Credentials    []CredentialRef
	Capabilities   []string
	Timeout        time.Duration
	Candidate      CandidateConfig
}

func (r Runtime) clone() Runtime {
	r.Args = append([]string(nil), r.Args...)
	r.PassThroughEnv = append([]string(nil), r.PassThroughEnv...)
	r.Credentials = append([]CredentialRef(nil), r.Credentials...)
	r.Capabilities = append([]string(nil), r.Capabilities...)
	return r
}

func validEnvName(name string) error {
	if !envNameRE.MatchString(name) {
		return invalid("env name %q", name)
	}
	if strings.HasPrefix(name, hostEnvPrefix) {
		return invalid("env name %q is reserved for the host", name)
	}
	return nil
}

// Validate checks a registration is safe to admit.
func (r Runtime) Validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return invalid("runtime name is required")
	}
	if !filepath.IsAbs(r.Executable) || filepath.Clean(r.Executable) != r.Executable {
		return invalid("executable must be a clean absolute path, got %q", r.Executable)
	}
	if strings.ContainsAny(r.Executable, " \t\n;|&$`<>(){}*?\"'\\") {
		return invalid("executable %q looks like shell text", r.Executable)
	}
	if shellNames[filepath.Base(r.Executable)] {
		return invalid("executable %q is a shell; register the wrapper itself", r.Executable)
	}
	for _, a := range r.Args {
		for _, ph := range placeholderRE.FindAllString(a, -1) {
			switch ph {
			case PlaceholderRequestPath, PlaceholderResultPath, PlaceholderRunID:
			default:
				return invalid("unknown placeholder %s in argument %q", ph, a)
			}
		}
	}
	if r.Timeout <= 0 {
		return invalid("timeout must be positive")
	}
	seenEnv := map[string]bool{}
	for _, n := range r.PassThroughEnv {
		if err := validEnvName(n); err != nil {
			return err
		}
		if seenEnv[n] {
			return invalid("duplicate env name %q", n)
		}
		seenEnv[n] = true
	}
	for _, c := range r.Credentials {
		if err := validEnvName(c.EnvName); err != nil {
			return err
		}
		if seenEnv[c.EnvName] {
			return invalid("duplicate env name %q", c.EnvName)
		}
		seenEnv[c.EnvName] = true
		if strings.TrimSpace(c.Ref) == "" {
			return invalid("credential %s has an empty reference", c.EnvName)
		}
	}
	seenCap := map[string]bool{}
	for _, c := range r.Capabilities {
		if !validCapability(c) {
			return invalid("unknown capability %q", c)
		}
		if seenCap[c] {
			return invalid("duplicate capability %q", c)
		}
		seenCap[c] = true
	}
	if r.Candidate.Digest() == "" {
		return invalid("candidate configuration is required (use NewCandidateConfig)")
	}
	return nil
}

func validCapability(c string) bool {
	switch c {
	case CapSourceRetrieval, CapPDFAccess, CapStructuredOutput:
		return true
	}
	return strings.HasPrefix(c, CapToolPrefix) && len(c) > len(CapToolPrefix)
}

// Registry holds trusted runtime registrations. It is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	runtimes map[string]Runtime
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{runtimes: map[string]Runtime{}} }

// Register admits a runtime. Names are unique and registrations are never
// replaced; a changed configuration is registered under a new name.
func (r *Registry) Register(rt Runtime) error {
	if err := rt.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.runtimes[rt.Name]; dup {
		return invalid("runtime %q already registered", rt.Name)
	}
	r.runtimes[rt.Name] = rt.clone()
	return nil
}

// Get returns a copy of a registered runtime.
func (r *Registry) Get(name string) (Runtime, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.runtimes[name]
	if !ok {
		return Runtime{}, fmt.Errorf("%w: %q", ErrUnknownRuntime, name)
	}
	return rt.clone(), nil
}

// Names lists registered runtime names, sorted.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.runtimes))
	for n := range r.runtimes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
