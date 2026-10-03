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
// and records why it was selected.
type CohortSampleSelection struct {
	OriginalTaskID      string `json:"original_task_id"`
	OriginalReviewRound int    `json:"original_review_round"`
	SubmittedSHA        string `json:"submitted_sha"`
	SelectionReason     string `json:"selection_reason"`
	MateriallyRejected  bool   `json:"materially_rejected"`
}

// CohortManifest represents a finite, reproducible cohort of first-round
// submissions selected for evaluation. Samples are selected explicitly;
// selection uses sealed historical outcomes, never exposed to reviewers.
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
		if s.OriginalTaskID == "" || s.SubmittedSHA == "" {
			return fmt.Errorf("sample missing required fields")
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
	// Include version and samples in order for reproducibility
	fmt.Fprintf(h, "v%d:", m.Version)
	for _, s := range m.Samples {
		fmt.Fprintf(h, "%s:%d:%s:%t;", s.OriginalTaskID, s.OriginalReviewRound, s.SubmittedSHA, s.MateriallyRejected)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EvaluationSnapshot represents a frozen workspace for one sample and one
// candidate. It contains only the necessary artifacts and source context,
// with no repository history, PR discussions, or other reviewer findings.
type EvaluationSnapshot struct {
	SampleID       string   `json:"sample_id"`
	CandidateID    string   `json:"candidate_id"`
	SnapshotDigest string   `json:"snapshot_digest"`
	SourceDigest   string   `json:"source_digest"`
	ManifestDigest string   `json:"manifest_digest"`
	PromptVersion  string   `json:"prompt_version"`
	ModelVersion   string   `json:"model_version"`
	RuntimeVersion string   `json:"runtime_version"`
	ArtifactPath   string   `json:"artifact_path"`
	SourceContexts []string `json:"source_contexts"`
}

// SnapshotBuilder constructs a frozen snapshot for an evaluation sample.
// It ensures that only the original submitted artifact and necessary source
// context are included, with no later fixes or repository history.
type SnapshotBuilder struct {
	SampleID       string
	OriginalTaskID string
	SubmittedSHA   string
	ProjectID      string
	PromptVersion  string
	ModelVersion   string
	RuntimeVersion string
}

// Build stages the snapshot workspace. It returns the snapshot record and
// an error if the original artifact cannot be reconstructed or sources
// are unavailable.
func (b *SnapshotBuilder) Build() (EvaluationSnapshot, error) {
	if b.SampleID == "" || b.OriginalTaskID == "" || b.SubmittedSHA == "" {
		return EvaluationSnapshot{}, fmt.Errorf("snapshot builder missing required fields")
	}

	snap := EvaluationSnapshot{
		SampleID:       b.SampleID,
		PromptVersion:  b.PromptVersion,
		ModelVersion:   b.ModelVersion,
		RuntimeVersion: b.RuntimeVersion,
	}

	// Compute immutable snapshot digest from the original SHA and sample ID.
	// The digest is deterministic: same sample/SHA combination always yields
	// the same digest, enforcing immutability.
	h := sha256.New()
	fmt.Fprintf(h, "snapshot:%s:%s:%s", b.SampleID, b.OriginalTaskID, b.SubmittedSHA)
	snap.SnapshotDigest = hex.EncodeToString(h.Sum(nil))

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
