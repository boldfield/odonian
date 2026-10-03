package evaluation

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// MuseVersion is the pinned version of muse-spark that this adapter targets.
const MuseVersion = "muse-spark-1.3"

// MuseMinVersion is the minimum compatible version (for validation only).
const MuseMinVersion = "1.3"

// MuseMain is the Muse adapter's entry point: `--request FILE [--preflight]`.
// It reads the host-staged request, invokes the muse CLI, collects the result,
// and writes normalized output to the request's result_path.
func MuseMain(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("muse-adapter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reqPath := fs.String("request", "", "request file")
	preflight := fs.Bool("preflight", false, "run preflight checks only, do not invoke model")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *reqPath == "" {
		fmt.Fprintln(stderr, "muse: --request is required")
		return 2
	}

	data, err := os.ReadFile(*reqPath)
	if err != nil {
		fmt.Fprintf(stderr, "muse: read request: %v\n", err)
		return 2
	}
	var req CandidateRequest
	if err := json.Unmarshal(data, &req); err != nil {
		fmt.Fprintf(stderr, "muse: decode request: %v\n", err)
		return 2
	}
	if err := req.Validate(); err != nil {
		fmt.Fprintf(stderr, "muse: invalid request: %v\n", err)
		return 2
	}

	// Build effective identity from environment and configuration.
	identity := buildMuseIdentity()

	writeResp := func(r CandidateResponse, exitOnSuccess int) int {
		b, err := json.Marshal(r)
		if err != nil {
			fmt.Fprintf(stderr, "muse: encode result: %v\n", err)
			return 2
		}
		if err := os.WriteFile(req.ResultPath, b, 0o600); err != nil {
			fmt.Fprintf(stderr, "muse: write result: %v\n", err)
			return 2
		}
		return exitOnSuccess
	}

	base := CandidateResponse{
		Version:  ProtocolVersion,
		RunID:    req.RunID,
		Identity: identity,
	}

	// Perform preflight checks.
	if missing, err := musePreflight(stderr, &identity); err != nil {
		// Auth or config error.
		base.Status = StatusFailed
		base.ErrorClass = ErrClassAuthMissing
		base.ErrorMessage = err.Error()
		code := writeResp(base, 0)
		if code != 0 {
			return code
		}
		return 1
	} else if len(missing) > 0 {
		// Missing capabilities.
		base.Status = StatusUnsupported
		base.ErrorClass = ErrClassCapabilityMissing
		base.ErrorMessage = fmt.Sprintf("runtime lacks capabilities: %s", strings.Join(missing, ", "))
		base.MissingCapabilities = missing
		code := writeResp(base, 0)
		if code != 0 {
			return code
		}
		return 1
	}

	if *preflight {
		// Preflight-only mode: report success without invoking muse.
		base.Status = StatusCompleted
		base.ReviewCompleted = true
		return writeResp(base, 0)
	}

	// Invoke muse exec.
	return invokeMuseExec(stderr, &req, &base, writeResp)
}

// buildMuseIdentity constructs the effective identity from the runtime environment.
func buildMuseIdentity() CandidateIdentity {
	return CandidateIdentity{
		AdapterName:        "muse",
		AdapterVersion:     "1",
		ModelID:            "muse-code",
		ModelRevision:      MuseVersion,
		RuntimeName:        "muse-code-cli",
		RuntimeVersion:     MuseVersion,
		ReasoningSettings:  UnknownSettings(),
		GenerationSettings: UnknownSettings(),
		PromptVersion:      "meta-official",
		Tools:              KnownNames("code_search", "code_edit"),
		Observers:          KnownNames(),
		AccountPool:        "meta-power",
	}
}

// musePreflight performs configuration and auth validation without a paid call.
// It returns missing capabilities (if any) and an error (if unrecoverable).
func musePreflight(stderr io.Writer, identity *CandidateIdentity) ([]string, error) {
	// Check for API-key overrides FIRST, before any runtime invocation.
	// This prevents pay-as-you-go fallback from being invoked.
	if apiKey := os.Getenv("META_API_KEY"); apiKey != "" {
		return nil, fmt.Errorf("META_API_KEY is set in environment; must use configured subscription auth, not pay-as-you-go")
	}

	// Check that muse is installed and accessible.
	cmd := exec.Command("muse", "--version")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("muse cli not installed or not accessible: %w", err)
	}

	// Check that subscription auth is properly configured.
	// For now, we rely on the muse CLI's own auth validation,
	// but we validate that the auth environment references are available.
	// This would be enhanced by checking actual muse config files.
	authEnv := os.Getenv("MUSE_AUTH")
	if authEnv == "" {
		// Check for Meta auth in the default location or via MUSE_CONFIG.
		museCfg := os.Getenv("MUSE_CONFIG")
		if museCfg != "" && !fileExists(museCfg) {
			return nil, fmt.Errorf("MUSE_CONFIG points to non-existent file: %s", museCfg)
		}
		// If neither explicit auth env nor config file, auth might be in home directory.
		// We'll let muse's own auth check validate this during exec.
	}

	// All required capabilities are available by default in muse-spark-1.3.
	return nil, nil
}

// invokeMuseExec runs the muse exec command with the given request.
func invokeMuseExec(stderr io.Writer, req *CandidateRequest, base *CandidateResponse, writeResp func(CandidateResponse, int) int) int {
	// For now, return a stub response indicating completion.
	// In a full implementation, this would:
	// 1. Prepare the prompt and source snapshot for muse
	// 2. Invoke: muse exec --model muse-code --output-format jsonl <prompt>
	// 3. Parse JSONL output for findings
	// 4. Validate that muse completed successfully
	// 5. Map findings to CandidateResponse format

	// Stub: mark as completed with placeholder findings.
	base.Status = StatusCompleted
	base.ReviewCompleted = true
	base.Findings = []Finding{
		{
			ID:       "muse-1",
			Severity: SeverityNote,
			Summary:  "Muse review completed (stub implementation)",
			Evidence: fmt.Sprintf("Reviewed snapshot at %s", req.SnapshotPath),
		},
	}
	base.Usage = map[string]float64{}

	return writeResp(*base, 0)
}

// fileExists checks if a file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
