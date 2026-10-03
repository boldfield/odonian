package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// AdapterVersion is a string identifier for the adapter protocol version.
const AdapterVersion = "1.0"

// Valid status values for CandidateResponse
const (
	StatusCompleted   = "completed"
	StatusUnsupported = "unsupported"
	StatusIncomplete  = "incomplete"
	StatusFailed      = "failed"
)

// CandidateRequest is a versioned evaluation request sent to a comparison reviewer adapter.
type CandidateRequest struct {
	// Version identifies the request protocol version.
	Version string `json:"version"`

	// RunID is a stable request identifier for idempotent retries.
	RunID string `json:"run_id"`

	// TaskID is the original task being evaluated.
	TaskID string `json:"task_id"`

	// ReviewRound is the review cycle number.
	ReviewRound int `json:"review_round"`

	// SubmittedCommit is the exact SHA of the code being reviewed.
	SubmittedCommit string `json:"submitted_commit"`

	// SnapshotPath is the path to a frozen artifact workspace containing only
	// the submitted code and necessary source context. No repository history.
	SnapshotPath string `json:"snapshot_path"`

	// BlindedPrompt is the evaluation prompt with all other reviewer feedback removed.
	BlindedPrompt string `json:"blinded_prompt"`

	// ToolAccess declares which tools/sources this review requires.
	ToolAccess ToolAccessRequirements `json:"tool_access"`

	// ResultPath is where the adapter should write its structured result.
	ResultPath string `json:"result_path"`
}

// Validate checks that the request is complete and valid.
func (r *CandidateRequest) Validate() error {
	if r.Version != AdapterVersion {
		return fmt.Errorf("unsupported request version: %s", r.Version)
	}
	if r.RunID == "" {
		return fmt.Errorf("RunID is required")
	}
	if r.SnapshotPath == "" {
		return fmt.Errorf("SnapshotPath is required")
	}
	if r.ResultPath == "" {
		return fmt.Errorf("ResultPath is required")
	}
	return nil
}

// ToolAccessRequirements declares what tools and source access the review needs.
type ToolAccessRequirements struct {
	RequireSourceRetrieval  bool     `json:"require_source_retrieval"`
	RequirePDFSupport       bool     `json:"require_pdf_support"`
	RequireStructuredOutput bool     `json:"require_structured_output"`
	DeclaredTools           []string `json:"declared_tools"` // e.g., ["bash", "python", "curl"]
}

// CandidateResponse is the normalized result returned by a comparison reviewer adapter.
type CandidateResponse struct {
	// Version identifies the response protocol version.
	Version string `json:"version"`

	// Status is the execution outcome: "completed", "unsupported", "incomplete", or "failed".
	Status string `json:"status"`

	// ErrorClass indicates the kind of failure (only for failed status).
	// Examples: "timeout", "auth_error", "output_malformed", "capability_unsupported".
	ErrorClass *string `json:"error_class,omitempty"`

	// ErrorMessage provides details about the failure (only for failed status).
	ErrorMessage *string `json:"error_message,omitempty"`

	// Findings are the structured review findings (only for completed status).
	Findings []Finding `json:"findings,omitempty"`

	// ReviewCompleted indicates whether a substantive review was performed.
	ReviewCompleted bool `json:"review_completed"`

	// EffectiveCandidate describes the exact model/runtime/configuration that produced this result.
	EffectiveCandidate CandidateIdentity `json:"effective_candidate"`

	// Timing records execution duration.
	Timing ResponseTiming `json:"timing"`

	// Usage records provider-reported usage if available.
	Usage *ResponseUsage `json:"usage,omitempty"`

	// RawOutput is the unprocessed adapter output for auditing (optional).
	RawOutput *string `json:"raw_output,omitempty"`
}

// Validate checks that the response is complete and consistent.
func (r *CandidateResponse) Validate() error {
	if r.Version != AdapterVersion {
		return fmt.Errorf("unsupported response version: %s", r.Version)
	}

	validStatuses := []string{StatusCompleted, StatusUnsupported, StatusIncomplete, StatusFailed}
	if !slices.Contains(validStatuses, r.Status) {
		return fmt.Errorf("invalid status: %s", r.Status)
	}

	switch r.Status {
	case StatusCompleted:
		if !r.ReviewCompleted {
			return fmt.Errorf("completed status requires ReviewCompleted=true")
		}
		if r.ErrorClass != nil || r.ErrorMessage != nil {
			return fmt.Errorf("completed status must not have error fields")
		}
	case StatusFailed, StatusUnsupported, StatusIncomplete:
		if r.ReviewCompleted && r.Status != StatusIncomplete {
			return fmt.Errorf("%s status must have ReviewCompleted=false", r.Status)
		}
		if r.ErrorClass == nil {
			return fmt.Errorf("%s status requires ErrorClass", r.Status)
		}
	}

	return nil
}

// Finding represents a single structured finding from the review.
type Finding struct {
	Severity string `json:"severity"`       // "critical", "major", "minor", "info"
	Category string `json:"category"`       // e.g., "correctness", "performance", "style"
	Summary  string `json:"summary"`        // brief one-liner
	Details  string `json:"details"`        // full description
	File     string `json:"file"`           // affected file path
	Line     *int   `json:"line,omitempty"` // line number if applicable
}

// ResponseTiming records execution timeline.
type ResponseTiming struct {
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt time.Time     `json:"completed_at"`
	Duration    time.Duration `json:"duration"`
}

// ResponseUsage records provider-reported usage metrics.
type ResponseUsage struct {
	// Provider-specific usage units as reported; not normalized or compared.
	// Examples: "12345 tokens", "45 muse_credits", "0.123 compute_hours".
	Units string `json:"units"`

	// Metadata provides additional context about usage measurement.
	Metadata map[string]interface{} `json:"metadata,omitempty"`
}

// Unknown is a marker value for explicitly unknown candidate identity fields.
const Unknown = "unknown"

// UnknownSettingsMap explicitly represents unknown settings.
// It distinguishes between nil (unknown), empty map (no settings), and populated map.
type UnknownSettingsMap map[string]interface{}

// MarshalJSON ensures nil, empty, and populated maps serialize distinctly.
func (u UnknownSettingsMap) MarshalJSON() ([]byte, error) {
	if u == nil {
		return []byte(`"` + Unknown + `"`), nil
	}
	return json.Marshal(map[string]interface{}(u))
}

// UnmarshalJSON reconstructs the map from JSON, handling the unknown marker.
func (u *UnknownSettingsMap) UnmarshalJSON(data []byte) error {
	if string(data) == `"`+Unknown+`"` {
		*u = nil
		return nil
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*u = UnknownSettingsMap(m)
	return nil
}

// UnknownTools explicitly represents unknown tool configuration.
// It distinguishes between nil (unknown), empty slice (no tools), and populated slice.
type UnknownTools []ToolConfig

// MarshalJSON ensures nil, empty, and populated slices serialize distinctly.
func (u UnknownTools) MarshalJSON() ([]byte, error) {
	if u == nil {
		return []byte(`"` + Unknown + `"`), nil
	}
	return json.Marshal([]ToolConfig(u))
}

// UnmarshalJSON reconstructs the slice from JSON, handling the unknown marker.
func (u *UnknownTools) UnmarshalJSON(data []byte) error {
	if string(data) == `"`+Unknown+`"` {
		*u = nil
		return nil
	}
	var t []ToolConfig
	if err := json.Unmarshal(data, &t); err != nil {
		return err
	}
	*u = UnknownTools(t)
	return nil
}

// CandidateIdentity uniquely identifies a reviewer configuration for reproducibility.
// Changing any field creates a new candidate version.
// String fields use the Unknown constant to represent unknown values.
// Settings and tool configuration use UnknownSettingsMap and UnknownTools to explicitly
// distinguish unknown from empty.
type CandidateIdentity struct {
	// AdapterName is the registered adapter identifier (e.g., "muse_code", "pi_spark").
	AdapterName string `json:"adapter_name"`

	// AdapterVersion is the adapter implementation version.
	AdapterVersion string `json:"adapter_version"`

	// ModelID is the immutable model identifier (e.g., "muse-spark-1.3", "pi-2024-q4").
	ModelID string `json:"model_id"`

	// ModelRevision is the model version if the provider reports one.
	// Use Unknown constant to represent unknown value.
	ModelRevision string `json:"model_revision"`

	// RuntimeName is the runtime environment (e.g., "muse_code_cli", "pi_api").
	RuntimeName string `json:"runtime_name"`

	// RuntimeVersion is the runtime implementation version.
	RuntimeVersion string `json:"runtime_version"`

	// ReasoningSettings are the configured reasoning parameters (if applicable).
	// nil = unknown, empty map = no settings, populated map = actual settings.
	ReasoningSettings UnknownSettingsMap `json:"reasoning_settings"`

	// GenerationSettings are the configured generation parameters.
	// nil = unknown, empty map = no settings, populated map = actual settings.
	GenerationSettings UnknownSettingsMap `json:"generation_settings"`

	// PromptVersion is the exact prompt version used.
	PromptVersion string `json:"prompt_version"`

	// ToolConfiguration describes what tools are available.
	// nil = unknown, empty slice = no tools, populated slice = actual tools.
	ToolConfiguration UnknownTools `json:"tool_configuration"`

	// AccountOrPool identifies the subscription/compute pool used.
	AccountOrPool string `json:"account_or_pool"`
}

// IsUnknown checks if a string field is explicitly unknown.
func IsUnknown(s string) bool {
	return s == Unknown
}

// ToolConfig describes a tool available to the reviewer.
type ToolConfig struct {
	Name    string                 `json:"name"`
	Version string                 `json:"version"`
	Config  map[string]interface{} `json:"config,omitempty"`
}

// CandidateDigest computes the immutable hash of a candidate configuration.
// Changing any effective configuration value changes the digest.
func (id CandidateIdentity) Digest() (string, error) {
	data, err := json.Marshal(id)
	if err != nil {
		return "", fmt.Errorf("marshal candidate identity: %w", err)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// CapabilityPreflight validates whether an adapter can fulfill a request
// without making a paid inference call.
type CapabilityPreflight struct {
	// Supported indicates whether all requested capabilities are available.
	Supported bool `json:"supported"`

	// Warnings lists non-critical issues found during preflight.
	Warnings []string `json:"warnings,omitempty"`

	// Error describes why the request is unsupported (only if Supported == false).
	Error *string `json:"error,omitempty"`

	// DeclaredCapabilities describes what this runtime can actually do.
	DeclaredCapabilities map[string]bool `json:"declared_capabilities"`
}

// CheckCapabilities validates whether the adapter can fulfill a request's tool access needs.
// Capability names: "source_retrieval", "pdf_parsing", "tool_execution", "structured_output".
func (cp *CapabilityPreflight) CheckCapabilities(req *CandidateRequest, adapterCaps []string) {
	cp.DeclaredCapabilities = make(map[string]bool)
	for _, cap := range adapterCaps {
		// Normalize pdf_support to pdf_parsing for consistency
		if cap == "pdf_support" {
			cap = "pdf_parsing"
		}
		cp.DeclaredCapabilities[cap] = true
	}

	cp.Supported = true
	var missing []string

	if req.ToolAccess.RequireSourceRetrieval && !cp.DeclaredCapabilities["source_retrieval"] {
		cp.Supported = false
		missing = append(missing, "source_retrieval")
	}
	if req.ToolAccess.RequirePDFSupport && !cp.DeclaredCapabilities["pdf_parsing"] {
		cp.Supported = false
		missing = append(missing, "pdf_parsing")
	}

	if len(req.ToolAccess.DeclaredTools) > 0 && !cp.DeclaredCapabilities["tool_execution"] {
		cp.Supported = false
		missing = append(missing, "tool_execution")
	}

	if req.ToolAccess.RequireStructuredOutput && !cp.DeclaredCapabilities["structured_output"] {
		cp.Supported = false
		missing = append(missing, "structured_output")
	}

	if !cp.Supported {
		errMsg := fmt.Sprintf("missing capabilities: %v", missing)
		cp.Error = &errMsg
	}
}

// RegistrationExecutable describes a trusted executable and its argument template.
type RegistrationExecutable struct {
	// Path is the absolute or relative path to the executable.
	Path string `json:"path"`

	// Args is the argument list template; may include placeholders like {request_path}, {result_path}.
	Args []string `json:"args"`

	// CredentialReferences are names of credentials this executable needs.
	// The host provides these at runtime without exposing them to the model.
	CredentialReferences []string `json:"credential_references"`

	// Timeout is the maximum execution duration.
	Timeout time.Duration `json:"timeout"`

	// WorkingDirectory is where the executable should run (optional).
	WorkingDirectory string `json:"working_directory,omitempty"`

	// Environment variables to set (optional; credential values are never exposed).
	Environment map[string]string `json:"environment,omitempty"`
}

// AdapterRuntime describes how to invoke a registered adapter.
type AdapterRuntime struct {
	// Name is the adapter identifier.
	Name string `json:"name"`

	// Version is the adapter implementation version.
	Version string `json:"version"`

	// Executable describes how to run the adapter.
	Executable RegistrationExecutable `json:"executable"`

	// DeclaredCapabilities lists what this adapter can do.
	// Standard capabilities: "source_retrieval", "pdf_parsing", "tool_execution", "structured_output".
	DeclaredCapabilities []string `json:"declared_capabilities"`

	// RequiredCredentials lists credentials this adapter needs.
	RequiredCredentials []string `json:"required_credentials"`
}

// AdapterRegistry holds registered adapters and provides resolution.
type AdapterRegistry struct {
	adapters map[string]*AdapterRuntime
}

// NewAdapterRegistry creates an empty registry.
func NewAdapterRegistry() *AdapterRegistry {
	return &AdapterRegistry{
		adapters: make(map[string]*AdapterRuntime),
	}
}

// Register adds a new adapter runtime to the registry.
// It returns an error if an adapter with the same name is already registered.
func (r *AdapterRegistry) Register(runtime *AdapterRuntime) error {
	if runtime.Name == "" {
		return fmt.Errorf("runtime name is required")
	}
	if _, exists := r.adapters[runtime.Name]; exists {
		return fmt.Errorf("adapter %q already registered", runtime.Name)
	}
	r.adapters[runtime.Name] = runtime
	return nil
}

// Resolve retrieves a registered adapter runtime by name.
// It returns nil if the adapter is not found.
func (r *AdapterRegistry) Resolve(name string) *AdapterRuntime {
	return r.adapters[name]
}

// RegisteredAdapters returns a list of all registered adapter names.
func (r *AdapterRegistry) RegisteredAdapters() []string {
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// ExecuteRegisteredAdapter runs a registered adapter with argv substitution and timeout.
// It replaces {request_path} and {result_path} placeholders in the Args list,
// sets up the working directory and environment, and runs the executable.
// No shell text execution is allowed; only the registered argv is used.
// Credentials are provided by the caller and never stored in the executable configuration.
func ExecuteRegisteredAdapter(ctx context.Context, registry *AdapterRegistry, adapterName string, requestPath string, resultPath string, credentials map[string]string) error {
	if registry == nil {
		return fmt.Errorf("registry is required")
	}

	runtime := registry.Resolve(adapterName)
	if runtime == nil {
		return fmt.Errorf("adapter %q not found in registry", adapterName)
	}

	exe := runtime.Executable
	if exe.Path == "" {
		return fmt.Errorf("executable path is required for adapter %q", adapterName)
	}

	// Substitute placeholders in args
	substitutedArgs := make([]string, len(exe.Args))
	for i, arg := range exe.Args {
		substituted := strings.ReplaceAll(arg, "{request_path}", requestPath)
		substituted = strings.ReplaceAll(substituted, "{result_path}", resultPath)
		substitutedArgs[i] = substituted
	}

	// Build the command with argv only (no shell)
	if exe.Timeout > 0 {
		// Create a new context with timeout if one wasn't provided or if we need a shorter timeout
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, exe.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, exe.Path, substitutedArgs...)

	// Set working directory if specified
	if exe.WorkingDirectory != "" {
		cmd.Dir = exe.WorkingDirectory
	}

	// Set up environment
	// Start with the current process environment
	cmd.Env = os.Environ()

	// Add credential environment variables (credentials are provided by the caller,
	// not stored in the RegistrationExecutable to ensure secret isolation)
	for _, credName := range exe.CredentialReferences {
		if credValue, ok := credentials[credName]; ok {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", credName, credValue))
		}
	}

	// Add other environment variables (non-credential ones only)
	for key, value := range exe.Environment {
		// Skip credential environment variables from the config; they must come from the credentials map
		if !slices.Contains(exe.CredentialReferences, key) {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", key, value))
		}
	}

	// Execute the adapter
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("adapter execution failed: %w", err)
	}

	return nil
}
