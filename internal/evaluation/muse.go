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
	"os/signal"
	"strings"
	"syscall"
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

	startTime := time.Now()

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
	missing, errClass, errMsg := musePreflight(stderr)
	if errClass != "" {
		// Runtime error, auth error, or ambiguous configuration.
		base.Status = StatusFailed
		base.ErrorClass = errClass
		base.ErrorMessage = errMsg
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
		// Write the response with effective identity (but no review/findings).
		base.Status = StatusCompleted
		base.ReviewCompleted = false
		base.Findings = []Finding{}
		base.Timing = Timing{
			StartedAt:  startTime,
			FinishedAt: time.Now(),
		}
		base.Usage = map[string]float64{}
		return writeResp(base, 0)
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
// It returns missing capabilities, error class (if any), and error message (if unrecoverable).
func musePreflight(stderr io.Writer) ([]string, ErrorClass, string) {
	// Check for pay-as-you-go API-key overrides FIRST, before any runtime invocation.
	if apiKey := os.Getenv("META_API_KEY"); apiKey != "" {
		return nil, ErrClassAuthMissing, "META_API_KEY is set in environment; must use configured subscription auth, not pay-as-you-go"
	}

	// Check that muse is installed and accessible.
	cmd := exec.Command("muse", "--help")
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return nil, ErrClassRuntimeError, fmt.Sprintf("muse cli not installed or not accessible: %v", err)
	}

	// Check subscription auth configuration and catch ambiguous routing.
	// A valid configuration uses exactly one of: MUSE_AUTH, MUSE_CONFIG, or ~/.muse/auth.
	authEnv := os.Getenv("MUSE_AUTH")
	museCfg := os.Getenv("MUSE_CONFIG")

	// Count configured auth sources.
	var authSources []string
	if authEnv != "" {
		authSources = append(authSources, "MUSE_AUTH")
	}
	if museCfg != "" {
		authSources = append(authSources, "MUSE_CONFIG")
	}

	// Check for ~/.muse/auth as a fallback.
	home, err := os.UserHomeDir()
	var defaultAuthExists bool
	if err == nil {
		defaultAuthPath := home + "/.muse/auth"
		defaultAuthExists = fileExists(defaultAuthPath)
		if defaultAuthExists {
			authSources = append(authSources, "~/.muse/auth")
		}
	}

	// Fail if multiple auth sources are configured (ambiguous routing).
	if len(authSources) > 1 {
		return nil, ErrClassAuthMissing, fmt.Sprintf("ambiguous auth configuration: multiple auth sources set (%s); configure exactly one", strings.Join(authSources, ", "))
	}

	// Fail if no auth sources are configured.
	if len(authSources) == 0 {
		if err != nil {
			return nil, ErrClassAuthMissing, fmt.Sprintf("no subscription auth configured: cannot determine home directory, and MUSE_AUTH/MUSE_CONFIG not set")
		}
		return nil, ErrClassAuthMissing, "no subscription auth configured: set MUSE_AUTH, MUSE_CONFIG, or configure ~/.muse/auth for subscription entitlement"
	}

	// Validate that specified files exist.
	if museCfg != "" && !fileExists(museCfg) {
		return nil, ErrClassAuthMissing, fmt.Sprintf("MUSE_CONFIG points to non-existent file: %s", museCfg)
	}
	if authEnv != "" && !fileExists(authEnv) {
		return nil, ErrClassAuthMissing, fmt.Sprintf("MUSE_AUTH points to non-existent file: %s", authEnv)
	}

	// Probe muse exec --help to verify required capabilities are supported.
	missing := checkMuseExecCapabilities(stderr)
	if len(missing) > 0 {
		return missing, "", ""
	}

	return nil, "", ""
}

// checkMuseExecCapabilities probes muse exec --help to verify that required flags are supported.
func checkMuseExecCapabilities(stderr io.Writer) []string {
	cmd := exec.Command("muse", "exec", "--help")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		// If we can't read the help, assume capabilities are missing.
		return []string{"muse exec --help", "--model", "--output-format", "--workspace", "--input"}
	}

	helpText := stdout.String()
	var missing []string

	requiredFlags := []string{"--model", "--output-format", "--workspace", "--input"}
	for _, flag := range requiredFlags {
		if !strings.Contains(helpText, flag) {
			missing = append(missing, flag)
		}
	}

	return missing
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

	// Set up signal handling to interrupt the process gracefully.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigChan)

	go func() {
		<-sigChan
		// Kill the muse process on signal.
		cmd.Process.Kill()
	}()

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
			// Kill the process on malformed output.
			cmd.Process.Kill()
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
		// Findings must have id and summary fields.
		if id := extractStringField(jsonlEntry, jsonlFieldID, ""); id != "" {
			summary := extractStringField(jsonlEntry, jsonlFieldSummary, "")
			if summary == "" {
				// Malformed finding: missing required summary field.
				cmd.Process.Kill()
				cmd.Wait()
				base.Status = StatusFailed
				base.ErrorClass = ErrClassOutputMalformed
				base.ErrorMessage = fmt.Sprintf("finding missing required 'summary' field (id=%s)", id)
				return writeResp(*base, 1)
			}

			severityStr := extractStringField(jsonlEntry, jsonlFieldSeverity, string(SeverityNote))
			// Validate severity is a known value.
			switch Severity(severityStr) {
			case SeverityNote, SeverityMinor, SeverityMaterial:
				// Valid severity.
			default:
				// Unknown severity value.
				cmd.Process.Kill()
				cmd.Wait()
				base.Status = StatusFailed
				base.ErrorClass = ErrClassOutputMalformed
				base.ErrorMessage = fmt.Sprintf("finding has unknown severity value: %q (id=%s)", severityStr, id)
				return writeResp(*base, 1)
			}

			finding := Finding{
				ID:       id,
				Severity: Severity(severityStr),
				Summary:  summary,
				Claim:    extractStringField(jsonlEntry, jsonlFieldClaim, ""),
				Evidence: extractStringField(jsonlEntry, jsonlFieldEvidence, ""),
			}
			findings = append(findings, finding)
		}
	}

	if err := scanner.Err(); err != nil {
		// Kill the process on scan error.
		cmd.Process.Kill()
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
