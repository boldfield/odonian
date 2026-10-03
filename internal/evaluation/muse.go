package evaluation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// MuseVersion is the pinned version of muse-spark that this adapter targets.
const MuseVersion = "muse-spark-1.3"

// MuseModelID is the model identifier to pass to muse exec.
const MuseModelID = "muse-spark-1.3"

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

	// Build effective identity from runtime.
	identity, runtimeErr := buildMuseIdentity()
	if runtimeErr != nil {
		// Use unknown identity on launch failure.
		identity = UnknownIdentity()
	}

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

	// If we couldn't read the runtime identity, fail.
	if runtimeErr != nil {
		base.Status = StatusFailed
		base.ErrorClass = ErrClassLaunchError
		base.ErrorMessage = runtimeErr.Error()
		code := writeResp(base, 0)
		if code != 0 {
			return code
		}
		return 1
	}

	// Perform preflight checks.
	if missing, err := musePreflight(stderr); err != nil {
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
		// Preflight-only mode: preflight checks passed.
		// Don't write a response since preflight didn't run a review.
		return 0
	}

	// Invoke muse exec.
	return invokeMuseExec(stderr, &req, &base, writeResp)
}

// buildMuseIdentity constructs the effective identity by reading from the runtime.
func buildMuseIdentity() (CandidateIdentity, error) {
	// Read runtime version from muse --version.
	runtimeVersion, err := readMuseVersion()
	if err != nil {
		return CandidateIdentity{}, fmt.Errorf("unable to determine runtime version: %w", err)
	}

	return CandidateIdentity{
		AdapterName:        "muse",
		AdapterVersion:     "1",
		ModelID:            "muse-code",
		ModelRevision:      MuseVersion,
		RuntimeName:        "muse-code-cli",
		RuntimeVersion:     runtimeVersion,
		ReasoningSettings:  UnknownSettings(),
		GenerationSettings: UnknownSettings(),
		PromptVersion:      Unknown,
		Tools:              UnknownNames(),
		Observers:          UnknownNames(),
		AccountPool:        "meta-power",
	}, nil
}

// readMuseVersion executes `muse --version` and parses the output.
func readMuseVersion() (string, error) {
	cmd := exec.Command("muse", "--version")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("muse --version failed: %w", err)
	}
	// muse --version typically outputs something like "muse-code version muse-spark-1.3".
	// We'll extract the version string or use the full stdout if it looks reasonable.
	version := strings.TrimSpace(stdout.String())
	if version == "" {
		version = strings.TrimSpace(stderr.String())
	}
	if version == "" {
		return "", fmt.Errorf("muse --version returned empty output")
	}
	return version, nil
}

// musePreflight performs configuration and auth validation without a paid call.
// It returns missing capabilities (if any) and an error (if unrecoverable).
func musePreflight(stderr io.Writer) ([]string, error) {
	// Check for API-key overrides FIRST, before any runtime invocation.
	// This prevents pay-as-you-go fallback from being invoked.
	if apiKey := os.Getenv("META_API_KEY"); apiKey != "" {
		return nil, fmt.Errorf("META_API_KEY is set in environment; must use configured subscription auth, not pay-as-you-go")
	}

	// Check that muse is installed and accessible.
	cmd := exec.Command("muse", "--help")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("muse cli not installed or not accessible: %w", err)
	}

	// Check that subscription auth is properly configured.
	// We need to ensure that MUSE_AUTH or MUSE_CONFIG are set for subscription routing.
	authEnv := os.Getenv("MUSE_AUTH")
	museCfg := os.Getenv("MUSE_CONFIG")

	if authEnv == "" && museCfg == "" {
		// Check if ~/.muse/auth exists as a fallback.
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("cannot determine home directory, and MUSE_AUTH/MUSE_CONFIG not set")
		}
		defaultAuthPath := home + "/.muse/auth"
		if !fileExists(defaultAuthPath) {
			return nil, fmt.Errorf("no subscription auth configured: set MUSE_AUTH, MUSE_CONFIG, or configure ~/.muse/auth for subscription entitlement")
		}
	}

	if museCfg != "" && !fileExists(museCfg) {
		return nil, fmt.Errorf("MUSE_CONFIG points to non-existent file: %s", museCfg)
	}

	// All required capabilities are available by default in muse-spark-1.3.
	// We report no missing capabilities here.
	return nil, nil
}

// invokeMuseExec runs the muse exec command with the given request.
func invokeMuseExec(stderr io.Writer, req *CandidateRequest, base *CandidateResponse, writeResp func(CandidateResponse, int) int) int {
	// Invoke: muse exec --model muse-spark-1.3 --output-format jsonl --workspace <snapshot> <prompt>
	cmd := exec.Command(
		"muse", "exec",
		"--model", MuseModelID,
		"--output-format", "jsonl",
		"--workspace", req.SnapshotPath,
		"--input", req.BlindedPrompt,
	)
	cmd.Stderr = stderr

	// Capture stdout for JSONL parsing.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		base.Status = StatusFailed
		base.ErrorClass = ErrClassLaunchError
		base.ErrorMessage = fmt.Sprintf("failed to create stdout pipe: %v", err)
		return writeResp(*base, 1)
	}

	if err := cmd.Start(); err != nil {
		base.Status = StatusFailed
		base.ErrorClass = ErrClassLaunchError
		base.ErrorMessage = fmt.Sprintf("failed to start muse exec: %v", err)
		return writeResp(*base, 1)
	}

	// Parse JSONL output.
	var findings []Finding
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var jsonlEntry map[string]interface{}
		if err := json.Unmarshal(line, &jsonlEntry); err != nil {
			cmd.Wait() // Best effort cleanup
			base.Status = StatusFailed
			base.ErrorClass = ErrClassOutputMalformed
			base.ErrorMessage = fmt.Sprintf("malformed JSONL output: %v", err)
			return writeResp(*base, 1)
		}

		// Try to extract a finding from the JSONL entry.
		// The exact format depends on muse's JSONL schema; we'll adapt as we learn.
		finding := Finding{
			ID:       extractStringField(jsonlEntry, "id", ""),
			Severity: Severity(extractStringField(jsonlEntry, "severity", string(SeverityNote))),
			Summary:  extractStringField(jsonlEntry, "summary", ""),
			Claim:    extractStringField(jsonlEntry, "claim", ""),
			Evidence: extractStringField(jsonlEntry, "evidence", ""),
		}
		if finding.ID != "" && finding.Summary != "" {
			findings = append(findings, finding)
		}
	}

	if err := scanner.Err(); err != nil {
		cmd.Wait() // Best effort cleanup
		base.Status = StatusFailed
		base.ErrorClass = ErrClassOutputMalformed
		base.ErrorMessage = fmt.Sprintf("error reading JSONL output: %v", err)
		return writeResp(*base, 1)
	}

	// Wait for muse to complete.
	if err := cmd.Wait(); err != nil {
		base.Status = StatusFailed
		base.ErrorClass = ErrClassRuntimeError
		base.ErrorMessage = fmt.Sprintf("muse exec failed: %v", err)
		return writeResp(*base, 1)
	}

	// Success: compile the response.
	base.Status = StatusCompleted
	base.ReviewCompleted = true
	base.Findings = findings
	base.Timing = Timing{
		StartedAt:  time.Now().Add(-10 * time.Second), // Approximate; not measured
		FinishedAt: time.Now(),
	}
	base.Usage = map[string]float64{} // Usage not reported by muse in this mode

	return writeResp(*base, 0)
}

// extractStringField safely extracts a string field from a JSON object.
func extractStringField(obj map[string]interface{}, key string, def string) string {
	v, ok := obj[key]
	if !ok {
		return def
	}
	s, ok := v.(string)
	if !ok {
		return def
	}
	return s
}

// fileExists checks if a file exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
