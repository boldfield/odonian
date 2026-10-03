package evaluation

import (
	"errors"
	"testing"
	"time"
)

func validRuntime(t *testing.T) Runtime {
	t.Helper()
	cfg, err := NewCandidateConfig(baseIdentity())
	if err != nil {
		t.Fatal(err)
	}
	return Runtime{
		Name: "rt", Executable: "/usr/local/bin/reviewer", Args: []string{"--request", PlaceholderRequestPath},
		Credentials:  []CredentialRef{{EnvName: "API_KEY", Ref: "cred/api"}},
		Capabilities: []string{CapStructuredOutput, CapSourceRetrieval, CapToolPrefix + "web_fetch"},
		Timeout:      time.Minute, Candidate: cfg,
	}
}

func TestRegisterRejectsUnsafeRuntimes(t *testing.T) {
	cases := map[string]func(*Runtime){
		"no name":             func(r *Runtime) { r.Name = "" },
		"relative executable": func(r *Runtime) { r.Executable = "reviewer" },
		"unclean executable":  func(r *Runtime) { r.Executable = "/usr/local/../bin/reviewer" },
		"shell text":          func(r *Runtime) { r.Executable = "/bin/echo hi; rm -rf /" },
		"command substitution": func(r *Runtime) {
			r.Executable = "/usr/bin/$(whoami)"
		},
		"a shell":               func(r *Runtime) { r.Executable = "/bin/bash" },
		"sh":                    func(r *Runtime) { r.Executable = "/bin/sh" },
		"unknown placeholder":   func(r *Runtime) { r.Args = []string{"{prompt}"} },
		"no timeout":            func(r *Runtime) { r.Timeout = 0 },
		"bad env name":          func(r *Runtime) { r.Credentials[0].EnvName = "api-key" },
		"host env credential":   func(r *Runtime) { r.Credentials[0].EnvName = "ODONIAN_TOKEN" },
		"host env pass-through": func(r *Runtime) { r.PassThroughEnv = []string{"ODONIAN_URL"} },
		"duplicate env":         func(r *Runtime) { r.PassThroughEnv = []string{"API_KEY"} },
		"duplicate passthrough": func(r *Runtime) { r.PassThroughEnv = []string{"HOME", "HOME"} },
		"empty credential ref":  func(r *Runtime) { r.Credentials[0].Ref = " " },
		"unknown capability":    func(r *Runtime) { r.Capabilities = []string{"telepathy"} },
		"duplicate capability":  func(r *Runtime) { r.Capabilities = []string{CapPDFAccess, CapPDFAccess} },
		"empty tool capability": func(r *Runtime) { r.Capabilities = []string{CapToolPrefix} },
		"no candidate config":   func(r *Runtime) { r.Candidate = CandidateConfig{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rt := validRuntime(t)
			mutate(&rt)
			if err := NewRegistry().Register(rt); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v, want ErrInvalid", err)
			}
		})
	}
}

func TestRegistryDuplicateAndCopySemantics(t *testing.T) {
	reg := NewRegistry()
	rt := validRuntime(t)
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(rt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate registration: got %v", err)
	}

	rt.Args[1] = "mutated"
	rt.Credentials[0].Ref = "mutated"
	rt.Capabilities[0] = "mutated"
	got, err := reg.Get("rt")
	if err != nil {
		t.Fatal(err)
	}
	if got.Args[1] != PlaceholderRequestPath || got.Credentials[0].Ref != "cred/api" || got.Capabilities[0] != CapStructuredOutput {
		t.Fatalf("registration followed caller mutation: %+v", got)
	}
	got.Args[1] = "mutated"
	if again, _ := reg.Get("rt"); again.Args[1] != PlaceholderRequestPath {
		t.Fatal("registration followed mutation of a returned copy")
	}

	if _, err := reg.Get("nope"); !errors.Is(err, ErrUnknownRuntime) {
		t.Fatalf("got %v", err)
	}
	if names := reg.Names(); len(names) != 1 || names[0] != "rt" {
		t.Fatalf("names = %v", names)
	}
}
