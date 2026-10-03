package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
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
	RequireSourceRetrieval bool     `json:"require_source_retrieval"`
	RequirePDFSupport      bool     `json:"require_pdf_support"`
	DeclaredTools          []string `json:"declared_tools"` // e.g., ["bash", "python", "curl"]
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

// CandidateIdentity uniquely identifies a reviewer configuration for reproducibility.
// Changing any field creates a new candidate version.
// To represent unknown values explicitly, use the Unknown constant for string fields
// or a special map/slice that serializes differently from empty.
type CandidateIdentity struct {
	// AdapterName is the registered adapter identifier (e.g., "muse_code", "pi_spark").
	AdapterName string `json:"adapter_name"`

	// AdapterVersion is the adapter implementation version.
	AdapterVersion string `json:"adapter_version"`

	// ModelID is the immutable model identifier (e.g., "muse-spark-1.3", "pi-2024-q4").
	ModelID string `json:"model_id"`

	// ModelRevision is the model version if the provider reports one.
	// Empty string or Unknown means unknown.
	ModelRevision string `json:"model_revision,omitempty"`

	// RuntimeName is the runtime environment (e.g., "muse_code_cli", "pi_api").
	RuntimeName string `json:"runtime_name"`

	// RuntimeVersion is the runtime implementation version.
	RuntimeVersion string `json:"runtime_version"`

	// ReasoningSettings are the configured reasoning parameters (if applicable).
	// Use Unknown() helper to explicitly mark as unknown vs empty.
	ReasoningSettings map[string]interface{} `json:"reasoning_settings,omitempty"`

	// GenerationSettings are the configured generation parameters.
	// Use Unknown() helper to explicitly mark as unknown vs empty.
	GenerationSettings map[string]interface{} `json:"generation_settings,omitempty"`

	// PromptVersion is the exact prompt version used.
	PromptVersion string `json:"prompt_version"`

	// ToolConfiguration describes what tools are available.
	// Use Unknown() helper to explicitly mark as unknown vs empty.
	ToolConfiguration []ToolConfig `json:"tool_configuration,omitempty"`

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
func (cp *CapabilityPreflight) CheckCapabilities(req *CandidateRequest, adapterCaps []string) {
	cp.DeclaredCapabilities = make(map[string]bool)
	for _, cap := range adapterCaps {
		cp.DeclaredCapabilities[cap] = true
	}

	cp.Supported = true
	var missing []string

	if req.ToolAccess.RequireSourceRetrieval && !cp.DeclaredCapabilities["source_retrieval"] {
		cp.Supported = false
		missing = append(missing, "source_retrieval")
	}
	if req.ToolAccess.RequirePDFSupport && !cp.DeclaredCapabilities["pdf_support"] {
		cp.Supported = false
		missing = append(missing, "pdf_support")
	}

	if len(req.ToolAccess.DeclaredTools) > 0 && !cp.DeclaredCapabilities["tool_execution"] {
		cp.Supported = false
		missing = append(missing, "tool_execution")
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
	// Examples: "source_retrieval", "pdf_parsing", "tool_execution".
	DeclaredCapabilities []string `json:"declared_capabilities"`

	// RequiredCredentials lists credentials this adapter needs.
	RequiredCredentials []string `json:"required_credentials"`
}
