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

// MuseModelID is the model identifier reported in the identity.
const MuseModelID = "muse-code"

// MuseExecModelFlag is the model flag to pass to muse exec.
const MuseExecModelFlag = "muse-spark-1.3"

// Muse JSONL output field names and result type identifiers.
const (
	jsonlFieldType      = "type"
	jsonlTypeResult     = "result"
	jsonlTypeCompletion = "completion"
	jsonlFieldID        = "id"
	jsonlFieldSeverity  = "severity"
	jsonlFieldSummary   = "summary"
	jsonlFieldClaim     = "claim"
	jsonlFieldEvidence  = "evidence"
)

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
		ModelID:            MuseModelID,
		ModelRevision:      MuseVersion,
		RuntimeName:        "muse-code-cli",
		RuntimeVersion:     runtimeVersion,
		ReasoningSettings:  UnknownSettings(),
		GenerationSettings: UnknownSettings(),
		PromptVersion:      Unknown,
		Tools:              UnknownNames(),
		Observers:          UnknownNames(),
		AccountPool:        Unknown,
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

	// Check subscription auth configuration and catch ambiguous routing.
	// A valid configuration uses exactly one of: MUSE_AUTH, MUSE_CONFIG, or ~/.muse/auth.
	authEnv := os.Getenv("MUSE_AUTH")
	museCfg := os.Getenv("MUSE_CONFIG")

	// Count configured auth sources.
	authSourceCount := 0
	if authEnv != "" {
		authSourceCount++
	}
	if museCfg != "" {
		authSourceCount++
	}

	// Check for ~/.muse/auth as a fallback.
	home, err := os.UserHomeDir()
	var defaultAuthExists bool
	if err == nil {
		defaultAuthPath := home + "/.muse/auth"
		defaultAuthExists = fileExists(defaultAuthPath)
		if defaultAuthExists && authSourceCount == 0 {
			authSourceCount++
		}
	}

	// Fail if multiple auth sources are configured (ambiguous routing).
	if authSourceCount > 1 {
		return nil, fmt.Errorf("ambiguous auth configuration: multiple auth sources set (MUSE_AUTH, MUSE_CONFIG, ~/.muse/auth); configure exactly one")
	}

	// Fail if no auth sources are configured.
	if authSourceCount == 0 {
		if err != nil {
			return nil, fmt.Errorf("no subscription auth configured: cannot determine home directory, and MUSE_AUTH/MUSE_CONFIG not set")
		}
		return nil, fmt.Errorf("no subscription auth configured: set MUSE_AUTH, MUSE_CONFIG, or configure ~/.muse/auth for subscription entitlement")
	}

	// Validate that specified files exist.
	if museCfg != "" && !fileExists(museCfg) {
		return nil, fmt.Errorf("MUSE_CONFIG points to non-existent file: %s", museCfg)
	}
	if authEnv != "" && !fileExists(authEnv) {
		return nil, fmt.Errorf("MUSE_AUTH points to non-existent file: %s", authEnv)
	}

	// All required capabilities are available by default in muse-spark-1.3.
	// We report no missing capabilities here.
	return nil, nil
}

// invokeMuseExec runs the muse exec command with the given request.
// It validates that the runtime emits an explicit completion signal in the JSONL stream,
// not just that the process exits successfully.
func invokeMuseExec(stderr io.Writer, req *CandidateRequest, base *CandidateResponse, writeResp func(CandidateResponse, int) int) int {
	startTime := time.Now()

	// Invoke: muse exec --model muse-spark-1.3 --output-format jsonl --workspace <snapshot> --input <prompt>
	cmd := exec.Command(
		"muse", "exec",
		"--model", MuseExecModelFlag,
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

	// Parse JSONL output and look for explicit completion signal.
	var findings []Finding
	var completionFound bool
	scanner := bufio.NewScanner(stdout)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}

		var jsonlEntry map[string]interface{}
		if err := json.Unmarshal(line, &jsonlEntry); err != nil {
			// Drain stdout to avoid deadlock, then kill the process.
			io.Copy(io.Discard, stdout)
			cmd.Wait()
			base.Status = StatusFailed
			base.ErrorClass = ErrClassOutputMalformed
			base.ErrorMessage = fmt.Sprintf("malformed JSONL output: %v", err)
			return writeResp(*base, 1)
		}

		// Check if this is a completion or result event.
		entryType := extractStringField(jsonlEntry, jsonlFieldType, "")
		if entryType == jsonlTypeCompletion || entryType == jsonlTypeResult {
			completionFound = true
		}

		// Try to extract a finding from the JSONL entry.
		// Findings should have id, severity, and summary fields.
		if id := extractStringField(jsonlEntry, jsonlFieldID, ""); id != "" {
			finding := Finding{
				ID:       id,
				Severity: Severity(extractStringField(jsonlEntry, jsonlFieldSeverity, string(SeverityNote))),
				Summary:  extractStringField(jsonlEntry, jsonlFieldSummary, ""),
				Claim:    extractStringField(jsonlEntry, jsonlFieldClaim, ""),
				Evidence: extractStringField(jsonlEntry, jsonlFieldEvidence, ""),
			}
			if finding.Summary != "" {
				findings = append(findings, finding)
			}
		}
	}

	if err := scanner.Err(); err != nil {
		// Drain stdout to avoid deadlock, then kill the process.
		io.Copy(io.Discard, stdout)
		cmd.Wait()
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

	// Exit zero is not sufficient for success; we must have received an explicit completion signal.
	if !completionFound {
		base.Status = StatusFailed
		base.ErrorClass = ErrClassOutputMissing
		base.ErrorMessage = "muse exec exited successfully but did not emit a completion/result event"
		return writeResp(*base, 1)
	}

	// Success: compile the response.
	base.Status = StatusCompleted
	base.ReviewCompleted = true
	base.Findings = findings
	base.Timing = Timing{
		StartedAt:  startTime,
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
