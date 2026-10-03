// Package evaluation defines the model-agnostic contract between the Odonian
// host and comparison-reviewer runtimes (CLI wrappers or tool-capable API
// runtimes). The core has no provider-specific branches: a runtime is a
// registered executable plus argv, and it exchanges versioned JSON files with
// the host. The host owns admission, secret isolation and persistence.
package evaluation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ProtocolVersion is the only adapter protocol version this host speaks.
const ProtocolVersion = 1

// Unknown is the explicit marker for an effective value the runtime could not
// report. An empty string is never a valid identity value.
const Unknown = "unknown"

// Capability vocabulary shared by requests, registrations and preflight.
const (
	CapSourceRetrieval  = "source_retrieval"
	CapPDFAccess        = "pdf_access"
	CapStructuredOutput = "structured_output"
	// CapToolPrefix prefixes a named tool, e.g. "tool:web_fetch".
	CapToolPrefix = "tool:"
)

// Status is the normalized outcome of one candidate run.
type Status string

const (
	StatusCompleted   Status = "completed"
	StatusIncomplete  Status = "incomplete"
	StatusUnsupported Status = "unsupported"
	StatusInterrupted Status = "interrupted"
	StatusFailed      Status = "failed"
)

// ErrorClass classifies why a run did not complete.
type ErrorClass string

const (
	ErrClassCapabilityMissing ErrorClass = "capability_missing"
	ErrClassOutputTruncated   ErrorClass = "output_truncated"
	ErrClassSourceUnavailable ErrorClass = "source_unavailable"
	ErrClassBudgetExhausted   ErrorClass = "budget_exhausted"
	ErrClassInterrupted       ErrorClass = "interrupted"
	ErrClassTimeout           ErrorClass = "timeout"
	ErrClassRuntimeError      ErrorClass = "runtime_error"
	ErrClassOutputMalformed   ErrorClass = "output_malformed"
	ErrClassOutputMissing     ErrorClass = "output_missing"
	ErrClassAuthMissing       ErrorClass = "auth_missing"
	ErrClassLaunchError       ErrorClass = "launch_error"
)

var allowedClasses = map[Status]map[ErrorClass]bool{
	StatusUnsupported: {ErrClassCapabilityMissing: true},
	StatusIncomplete:  {ErrClassOutputTruncated: true, ErrClassSourceUnavailable: true, ErrClassBudgetExhausted: true},
	StatusInterrupted: {ErrClassInterrupted: true, ErrClassTimeout: true},
	StatusFailed: {
		ErrClassRuntimeError: true, ErrClassOutputMalformed: true, ErrClassOutputMissing: true,
		ErrClassAuthMissing: true, ErrClassLaunchError: true, ErrClassSourceUnavailable: true,
	},
}

// Severity of a finding as reported by the candidate.
type Severity string

const (
	SeverityMaterial Severity = "material"
	SeverityMinor    Severity = "minor"
	SeverityNote     Severity = "note"
)

const (
	maxErrorMessage = 1024
	maxFindings     = 500
)

var (
	ErrUnsupportedVersion = errors.New("unsupported protocol version")
	ErrInvalid            = errors.New("invalid")
)

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// ToolAccessRequirements declares what a review needs from the runtime.
// Structured result output is always required for a review and is not
// optional here.
type ToolAccessRequirements struct {
	RequireSourceRetrieval bool     `json:"require_source_retrieval"`
	RequirePDFAccess       bool     `json:"require_pdf_access"`
	Tools                  []string `json:"tools,omitempty"`
}

// RequiredCapabilities lists the capability names a runtime must declare.
func (t ToolAccessRequirements) RequiredCapabilities() []string {
	caps := []string{CapStructuredOutput}
	if t.RequireSourceRetrieval {
		caps = append(caps, CapSourceRetrieval)
	}
	if t.RequirePDFAccess {
		caps = append(caps, CapPDFAccess)
	}
	tools := append([]string(nil), t.Tools...)
	sort.Strings(tools)
	for _, name := range tools {
		caps = append(caps, CapToolPrefix+name)
	}
	return caps
}

// Validate checks the requirements are well formed.
func (t ToolAccessRequirements) Validate() error {
	seen := map[string]bool{}
	for _, name := range t.Tools {
		if strings.TrimSpace(name) == "" || name != strings.TrimSpace(name) {
			return invalid("tool name %q", name)
		}
		if seen[name] {
			return invalid("duplicate tool %q", name)
		}
		seen[name] = true
	}
	return nil
}

// CheckCapabilities returns the required capabilities the declared set lacks,
// sorted. It never makes a model call.
func CheckCapabilities(access ToolAccessRequirements, declared []string) []string {
	have := make(map[string]bool, len(declared))
	for _, c := range declared {
		have[c] = true
	}
	var missing []string
	for _, c := range access.RequiredCapabilities() {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	return missing
}

// CandidateRequest is the versioned input the host hands a runtime.
type CandidateRequest struct {
	Version       int                    `json:"version"`
	RunID         string                 `json:"run_id"`
	SnapshotPath  string                 `json:"snapshot_path"`
	BlindedPrompt string                 `json:"blinded_prompt"`
	ToolAccess    ToolAccessRequirements `json:"tool_access"`
	ResultPath    string                 `json:"result_path"`
}

// Validate checks the request is complete and speaks a supported version.
func (r CandidateRequest) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("%w: request version %d, host speaks %d", ErrUnsupportedVersion, r.Version, ProtocolVersion)
	}
	if strings.TrimSpace(r.RunID) == "" {
		return invalid("run_id is required")
	}
	if !filepath.IsAbs(r.SnapshotPath) {
		return invalid("snapshot_path must be absolute, got %q", r.SnapshotPath)
	}
	if !filepath.IsAbs(r.ResultPath) {
		return invalid("result_path must be absolute, got %q", r.ResultPath)
	}
	if strings.TrimSpace(r.BlindedPrompt) == "" {
		return invalid("blinded_prompt is required")
	}
	return r.ToolAccess.Validate()
}

// Settings is a set of reasoning or generation parameters. Known=false means
// the effective values are unknown, which differs from a known empty set.
type Settings struct {
	Known  bool              `json:"known"`
	Values map[string]string `json:"values,omitempty"`
}

// KnownSettings builds a known settings set (copying the input).
func KnownSettings(values map[string]string) Settings {
	s := Settings{Known: true}
	if len(values) > 0 {
		s.Values = make(map[string]string, len(values))
		for k, v := range values {
			s.Values[k] = v
		}
	}
	return s
}

// UnknownSettings is the explicit unknown marker.
func UnknownSettings() Settings { return Settings{} }

func (s Settings) validate(name string) error {
	if !s.Known && len(s.Values) > 0 {
		return invalid("%s: unknown settings must not carry values", name)
	}
	for k := range s.Values {
		if k == "" {
			return invalid("%s: empty key", name)
		}
	}
	return nil
}

func (s Settings) clone() Settings { return Settings{Known: s.Known, Values: copyMap(s.Values)} }

// NameSet is a set of tool or observer names. Known=false means unknown, which
// differs from a known empty set.
type NameSet struct {
	Known bool     `json:"known"`
	Names []string `json:"names,omitempty"`
}

// KnownNames builds a known name set (copying the input).
func KnownNames(names ...string) NameSet {
	return NameSet{Known: true, Names: append([]string(nil), names...)}
}

// UnknownNames is the explicit unknown marker.
func UnknownNames() NameSet { return NameSet{} }

func (n NameSet) validate(name string) error {
	if !n.Known && len(n.Names) > 0 {
		return invalid("%s: unknown set must not carry names", name)
	}
	seen := map[string]bool{}
	for _, v := range n.Names {
		if v == "" {
			return invalid("%s: empty name", name)
		}
		if seen[v] {
			return invalid("%s: duplicate %q", name, v)
		}
		seen[v] = true
	}
	return nil
}

func (n NameSet) clone() NameSet {
	return NameSet{Known: n.Known, Names: append([]string(nil), n.Names...)}
}

// CandidateIdentity is the reproducible identity of a candidate reviewer. Any
// change to an effective value is a different candidate version.
type CandidateIdentity struct {
	AdapterName        string   `json:"adapter_name"`
	AdapterVersion     string   `json:"adapter_version"`
	ModelID            string   `json:"model_id"`
	ModelRevision      string   `json:"model_revision"`
	RuntimeName        string   `json:"runtime_name"`
	RuntimeVersion     string   `json:"runtime_version"`
	ReasoningSettings  Settings `json:"reasoning_settings"`
	GenerationSettings Settings `json:"generation_settings"`
	PromptVersion      string   `json:"prompt_version"`
	Tools              NameSet  `json:"tools"`
	Observers          NameSet  `json:"observers"`
	AccountPool        string   `json:"account_pool"`
}

// UnknownIdentity is an identity with every effective value unknown.
func UnknownIdentity() CandidateIdentity {
	return CandidateIdentity{
		AdapterName: Unknown, AdapterVersion: Unknown, ModelID: Unknown, ModelRevision: Unknown,
		RuntimeName: Unknown, RuntimeVersion: Unknown, ReasoningSettings: UnknownSettings(),
		GenerationSettings: UnknownSettings(), PromptVersion: Unknown, Tools: UnknownNames(),
		Observers: UnknownNames(), AccountPool: Unknown,
	}
}

func (c CandidateIdentity) strings() []struct{ name, val string } {
	return []struct{ name, val string }{
		{"adapter_name", c.AdapterName}, {"adapter_version", c.AdapterVersion},
		{"model_id", c.ModelID}, {"model_revision", c.ModelRevision},
		{"runtime_name", c.RuntimeName}, {"runtime_version", c.RuntimeVersion},
		{"prompt_version", c.PromptVersion}, {"account_pool", c.AccountPool},
	}
}

// Validate requires every value to be set or explicitly Unknown.
func (c CandidateIdentity) Validate() error {
	for _, f := range c.strings() {
		if strings.TrimSpace(f.val) == "" {
			return invalid("identity %s is empty; use %q when unknown", f.name, Unknown)
		}
	}
	for name, s := range map[string]Settings{"reasoning_settings": c.ReasoningSettings, "generation_settings": c.GenerationSettings} {
		if err := s.validate(name); err != nil {
			return err
		}
	}
	for name, n := range map[string]NameSet{"tools": c.Tools, "observers": c.Observers} {
		if err := n.validate(name); err != nil {
			return err
		}
	}
	return nil
}

func (c CandidateIdentity) clone() CandidateIdentity {
	c.ReasoningSettings = c.ReasoningSettings.clone()
	c.GenerationSettings = c.GenerationSettings.clone()
	c.Tools = c.Tools.clone()
	c.Observers = c.Observers.clone()
	return c
}

// Digest is a stable content digest of the identity. Known-empty and unknown
// settings/tool sets digest differently, and map/slice ordering is irrelevant.
func (c CandidateIdentity) Digest() string {
	h := sha256.New()
	put := func(s string) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(s)))
		h.Write(n[:])
		h.Write([]byte(s))
	}
	put("odonian-candidate-identity/v1")
	for _, f := range c.strings() {
		put(f.name)
		put(f.val)
	}
	putSettings := func(name string, s Settings) {
		put(name)
		put(fmt.Sprint(s.Known))
		keys := make([]string, 0, len(s.Values))
		for k := range s.Values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		put(fmt.Sprint(len(keys)))
		for _, k := range keys {
			put(k)
			put(s.Values[k])
		}
	}
	putNames := func(name string, n NameSet) {
		put(name)
		put(fmt.Sprint(n.Known))
		names := append([]string(nil), n.Names...)
		sort.Strings(names)
		put(fmt.Sprint(len(names)))
		for _, v := range names {
			put(v)
		}
	}
	putSettings("reasoning_settings", c.ReasoningSettings)
	putSettings("generation_settings", c.GenerationSettings)
	putNames("tools", c.Tools)
	putNames("observers", c.Observers)
	return hex.EncodeToString(h.Sum(nil))
}

// CandidateConfig is an immutable, validated, digested candidate identity.
// It deep-copies on the way in and out so later mutation of a caller's maps or
// slices cannot change a registered candidate.
type CandidateConfig struct {
	identity CandidateIdentity
	digest   string
}

// NewCandidateConfig validates and freezes an identity.
func NewCandidateConfig(id CandidateIdentity) (CandidateConfig, error) {
	if err := id.Validate(); err != nil {
		return CandidateConfig{}, err
	}
	frozen := id.clone()
	return CandidateConfig{identity: frozen, digest: frozen.Digest()}, nil
}

// Identity returns a copy of the identity.
func (c CandidateConfig) Identity() CandidateIdentity { return c.identity.clone() }

// Digest returns the digest computed at construction.
func (c CandidateConfig) Digest() string { return c.digest }

// Finding is one structured review finding.
type Finding struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Claim    string   `json:"claim,omitempty"`
	Summary  string   `json:"summary"`
	Evidence string   `json:"evidence,omitempty"`
}

// Timing is recorded by the host from its own clock; adapter-supplied values
// are overwritten.
type Timing struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// CandidateResponse is the versioned, normalized result of one run.
// Usage holds provider-native units as supplied; nil means unknown.
type CandidateResponse struct {
	Version             int                `json:"version"`
	RunID               string             `json:"run_id"`
	Status              Status             `json:"status"`
	ReviewCompleted     bool               `json:"review_completed"`
	Findings            []Finding          `json:"findings,omitempty"`
	ErrorClass          ErrorClass         `json:"error_class,omitempty"`
	ErrorMessage        string             `json:"error_message,omitempty"`
	MissingCapabilities []string           `json:"missing_capabilities,omitempty"`
	Identity            CandidateIdentity  `json:"identity"`
	Timing              Timing             `json:"timing"`
	Usage               map[string]float64 `json:"usage,omitempty"`
}

// Validate enforces the status invariants: only a completed response claims a
// completed review or carries findings, and every other status names an error
// class that is legal for it.
func (r CandidateResponse) Validate() error {
	if r.Version != ProtocolVersion {
		return fmt.Errorf("%w: response version %d, host speaks %d", ErrUnsupportedVersion, r.Version, ProtocolVersion)
	}
	if strings.TrimSpace(r.RunID) == "" {
		return invalid("run_id is required")
	}
	if err := r.Identity.Validate(); err != nil {
		return err
	}
	if len(r.ErrorMessage) > maxErrorMessage {
		return invalid("error_message exceeds %d bytes", maxErrorMessage)
	}
	if !r.Timing.StartedAt.IsZero() && !r.Timing.FinishedAt.IsZero() && r.Timing.FinishedAt.Before(r.Timing.StartedAt) {
		return invalid("timing finishes before it starts")
	}
	for k, v := range r.Usage {
		if k == "" || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return invalid("usage %q=%v", k, v)
		}
	}
	if len(r.MissingCapabilities) > 0 && r.Status != StatusUnsupported {
		return invalid("missing_capabilities only valid for status %s", StatusUnsupported)
	}
	switch r.Status {
	case StatusCompleted:
		if !r.ReviewCompleted {
			return invalid("completed response must set review_completed")
		}
		if r.ErrorClass != "" || r.ErrorMessage != "" {
			return invalid("completed response must not carry an error")
		}
		return validateFindings(r.Findings)
	case StatusIncomplete, StatusUnsupported, StatusInterrupted, StatusFailed:
		if r.ReviewCompleted {
			return invalid("%s response must not set review_completed", r.Status)
		}
		if len(r.Findings) > 0 {
			return invalid("%s response must not carry findings", r.Status)
		}
		if !allowedClasses[r.Status][r.ErrorClass] {
			return invalid("error_class %q not valid for status %s", r.ErrorClass, r.Status)
		}
		if r.Status == StatusUnsupported && len(r.MissingCapabilities) == 0 {
			return invalid("unsupported response must list missing_capabilities")
		}
		return nil
	default:
		return invalid("unknown status %q", r.Status)
	}
}

// ValidateFindings applies the same finding rules a candidate response is held
// to: bounded count, unique non-empty ids, a known severity and a summary.
func ValidateFindings(fs []Finding) error { return validateFindings(fs) }

func validateFindings(fs []Finding) error {
	if len(fs) > maxFindings {
		return invalid("more than %d findings", maxFindings)
	}
	seen := map[string]bool{}
	for i, f := range fs {
		if strings.TrimSpace(f.ID) == "" {
			return invalid("finding %d: id is required", i)
		}
		if seen[f.ID] {
			return invalid("finding %d: duplicate id %q", i, f.ID)
		}
		seen[f.ID] = true
		switch f.Severity {
		case SeverityMaterial, SeverityMinor, SeverityNote:
		default:
			return invalid("finding %q: severity %q", f.ID, f.Severity)
		}
		if strings.TrimSpace(f.Summary) == "" {
			return invalid("finding %q: summary is required", f.ID)
		}
	}
	return nil
}

func copyMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
