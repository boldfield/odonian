package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// CohortSampleSelection represents a single sample in the cohort with its
// selection metadata. The sample pins the original first-round submitted SHA
// and records why it was selected. An empty SubmittedSHA indicates the
// original artifact is unavailable.
type CohortSampleSelection struct {
	OriginalTaskID      string `json:"original_task_id"`
	OriginalReviewRound int    `json:"original_review_round"`
	SubmittedSHA        string `json:"submitted_sha"` // empty means unavailable
	SelectionReason     string `json:"selection_reason"`
	MateriallyRejected  bool   `json:"materially_rejected"`
	Unavailable         bool   `json:"unavailable"` // true if original artifact cannot be reconstructed
}

// CohortManifest represents a finite, reproducible cohort of first-round
// submissions selected for evaluation. Samples are selected explicitly;
// selection uses sealed historical outcomes, never exposed to reviewers.
// An unavailable sample still counts toward the denominator.
type CohortManifest struct {
	Version          int                     `json:"version"`
	Samples          []CohortSampleSelection `json:"samples"`
	TotalDenominator int                     `json:"total_denominator"`
	SelectionReason  string                  `json:"selection_reason"`
	CreatedAt        string                  `json:"created_at"`
}

// Validate checks that the manifest is well-formed.
func (m *CohortManifest) Validate() error {
	if m == nil {
		return fmt.Errorf("cohort manifest cannot be nil")
	}
	if m.Version != 1 {
		return fmt.Errorf("unsupported cohort manifest version: %d", m.Version)
	}
	if len(m.Samples) == 0 {
		return fmt.Errorf("cohort must contain at least one sample")
	}
	if m.TotalDenominator < len(m.Samples) {
		return fmt.Errorf("denominator %d is less than sample count %d", m.TotalDenominator, len(m.Samples))
	}
	seen := make(map[string]bool)
	for _, s := range m.Samples {
		if s.OriginalTaskID == "" {
			return fmt.Errorf("sample missing original_task_id")
		}
		// Empty SHA is allowed only if Unavailable is true
		if s.SubmittedSHA == "" && !s.Unavailable {
			return fmt.Errorf("sample has empty SHA but not marked unavailable")
		}
		key := s.OriginalTaskID + ":" + fmt.Sprintf("%d", s.OriginalReviewRound)
		if seen[key] {
			return fmt.Errorf("duplicate sample in cohort: %s", key)
		}
		seen[key] = true
	}
	return nil
}

// Digest returns the immutable content digest of the cohort. Two cohorts
// with the same samples in the same order produce the same digest.
func (m *CohortManifest) Digest() string {
	h := sha256.New()
	// Include version, samples and denominator for complete reproducibility
	fmt.Fprintf(h, "v%d:", m.Version)
	for _, s := range m.Samples {
		fmt.Fprintf(h, "%s:%d:%s:%t:%t;", s.OriginalTaskID, s.OriginalReviewRound, s.SubmittedSHA, s.MateriallyRejected, s.Unavailable)
	}
	fmt.Fprintf(h, "denom:%d:", m.TotalDenominator)
	fmt.Fprintf(h, "reason:%s", m.SelectionReason)
	return hex.EncodeToString(h.Sum(nil))
}

// EvaluationSnapshot represents a frozen workspace for one sample and one
// candidate. It contains only the necessary artifacts and source context,
// with no repository history, PR discussions, or other reviewer findings.
type EvaluationSnapshot struct {
	SampleID       string   `json:"sample_id"`
	CandidateID    string   `json:"candidate_id"`
	SnapshotDigest string   `json:"snapshot_digest"` // content-addressed digest of staged workspace
	SourceDigest   string   `json:"source_digest"`   // digest of source context
	ManifestDigest string   `json:"manifest_digest"` // digest of manifest files
	PromptVersion  string   `json:"prompt_version"`
	ModelVersion   string   `json:"model_version"`
	RuntimeVersion string   `json:"runtime_version"`
	ArtifactPath   string   `json:"artifact_path"`   // path to staged artifact directory
	SourceContexts []string `json:"source_contexts"` // paths to source context files
	Unavailable    bool     `json:"unavailable"`     // true if original artifact unavailable
}

// SnapshotBuilder constructs a frozen snapshot for an evaluation sample.
// It ensures that only the original submitted artifact and necessary source
// context are included, with no later fixes or repository history.
type SnapshotBuilder struct {
	SampleID       string
	OriginalTaskID string
	SubmittedSHA   string // empty if unavailable
	ProjectID      string
	CandidateID    string
	PromptVersion  string
	ModelVersion   string
	RuntimeVersion string
	// Optional: functions to retrieve original artifact and source context
	// Used for staging the actual workspace files
	ArtifactFetcher func(sha string) ([]byte, error)
	SourceFetcher   func() (map[string][]byte, error)
}

// Build stages the snapshot workspace. It returns the snapshot record and
// an error if required fields are missing or I/O fails.
// If SubmittedSHA is empty, marks snapshot as unavailable.
func (b *SnapshotBuilder) Build() (EvaluationSnapshot, error) {
	if b.SampleID == "" || b.OriginalTaskID == "" {
		return EvaluationSnapshot{}, fmt.Errorf("snapshot builder missing required fields")
	}

	snap := EvaluationSnapshot{
		SampleID:       b.SampleID,
		CandidateID:    b.CandidateID,
		PromptVersion:  b.PromptVersion,
		ModelVersion:   b.ModelVersion,
		RuntimeVersion: b.RuntimeVersion,
	}

	// If SHA is empty, mark as unavailable but don't fail
	if b.SubmittedSHA == "" {
		snap.Unavailable = true
		h := sha256.New()
		fmt.Fprintf(h, "unavailable:%s:%s", b.SampleID, b.OriginalTaskID)
		snap.SnapshotDigest = hex.EncodeToString(h.Sum(nil))
		return snap, nil
	}

	// For available samples, compute content-addressed digest
	// This would normally include the actual staged files
	// For now, we create a digest that includes the SHA and sample info
	h := sha256.New()
	fmt.Fprintf(h, "snapshot:v1:%s:%s:%s:%s", b.SampleID, b.OriginalTaskID, b.SubmittedSHA, b.CandidateID)
	snap.SnapshotDigest = hex.EncodeToString(h.Sum(nil))

	// TODO: In full implementation, would stage actual files here:
	// - Create workspace directory
	// - Checkout original artifact from SubmittedSHA
	// - Copy source context without git history
	// - Compute actual content-addressed digest over staged files
	// - Exclude PR discussions, reviewer findings, etc.

	return snap, nil
}

// DeterministicCohortBuilder helps construct a reproducible cohort.
// It sorts samples deterministically and validates their consistency.
type DeterministicCohortBuilder struct {
	Version          int
	Samples          []CohortSampleSelection
	TotalDenominator int
	SelectionReason  string
	CreatedAt        string
}

// Build creates and validates the cohort manifest, ensuring deterministic
// sample ordering and consistency.
func (b *DeterministicCohortBuilder) Build() (CohortManifest, error) {
	if b.Version == 0 {
		b.Version = 1
	}
	if len(b.Samples) == 0 {
		return CohortManifest{}, fmt.Errorf("cannot build cohort with no samples")
	}

	// Sort samples deterministically by task ID and review round
	sortedSamples := make([]CohortSampleSelection, len(b.Samples))
	copy(sortedSamples, b.Samples)
	sort.Slice(sortedSamples, func(i, j int) bool {
		if sortedSamples[i].OriginalTaskID != sortedSamples[j].OriginalTaskID {
			return sortedSamples[i].OriginalTaskID < sortedSamples[j].OriginalTaskID
		}
		return sortedSamples[i].OriginalReviewRound < sortedSamples[j].OriginalReviewRound
	})

	manifest := CohortManifest{
		Version:          b.Version,
		Samples:          sortedSamples,
		TotalDenominator: b.TotalDenominator,
		SelectionReason:  b.SelectionReason,
		CreatedAt:        b.CreatedAt,
	}

	if err := manifest.Validate(); err != nil {
		return CohortManifest{}, err
	}

	return manifest, nil
}

// MarshalCohort serializes a cohort manifest to JSON.
func MarshalCohort(m *CohortManifest) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal cohort: %w", err)
	}
	return string(data), nil
}

// UnmarshalCohort deserializes a JSON cohort manifest.
func UnmarshalCohort(raw string) (*CohortManifest, error) {
	var m CohortManifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, fmt.Errorf("unmarshal cohort: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return &m, nil
}
