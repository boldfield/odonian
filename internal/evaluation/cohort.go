package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// CohortVersion is the version of the persisted cohort manifest format.
const CohortVersion = 1

// MaxCohortCap bounds a cohort so a pilot is always finite.
const MaxCohortCap = 500

// Outcome is the sealed first-round outcome of a sample. It is host metadata
// used only to stratify selection; it never enters a reviewer prompt or
// workspace.
type Outcome string

const (
	// OutcomeClean means round 1 finished with no blocking (P1/P2) finding.
	OutcomeClean Outcome = "clean"
	// OutcomeRejectedMaterial means round 1 raised at least one blocking finding.
	OutcomeRejectedMaterial Outcome = "rejected_material"
)

// UnavailableReason says why a selected sample cannot be evaluated. An
// unavailable sample stays in the cohort and its denominators; it is neither a
// success nor a miss.
type UnavailableReason string

const (
	UnavailNoCommit            UnavailableReason = "no_submitted_commit"
	UnavailAmbiguousCommit     UnavailableReason = "ambiguous_submitted_commit"
	UnavailInvalidCommit       UnavailableReason = "invalid_submitted_commit"
	UnavailManifestCorrupt     UnavailableReason = "manifest_digest_mismatch"
	UnavailArtifactUnreachable UnavailableReason = "artifact_unreachable"
	UnavailContaminated        UnavailableReason = "sealed_content_in_snapshot"
	UnavailFrozenInputChanged  UnavailableReason = "frozen_input_changed"
)

// Exclusion buckets for tasks that were examined but are not eligible
// first-round samples. They are counted, never silently dropped.
const (
	ExcludedNotResearch        = "not_research_task"
	ExcludedNotSubmitted       = "never_submitted"
	ExcludedReplacement        = "replacement_task"
	ExcludedRoundOneIncomplete = "round_one_reviews_incomplete"
)

var commitSHARE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// RecordedLink is a task link as recorded on the board.
type RecordedLink struct {
	Kind  string
	Value string
}

// RecordedManifest is the continuation manifest stored with the first-round
// submission, exactly as stored.
type RecordedManifest struct {
	JSON   []byte
	Digest string
}

// Valid reports whether the stored digest is the SHA-256 of the stored JSON.
func (m RecordedManifest) Valid() bool {
	sum := sha256.Sum256(m.JSON)
	return hex.EncodeToString(sum[:]) == m.Digest
}

// FirstRoundRecord is one first-round submission read from the board's own
// records. Everything here is host-side: Sealed holds reviewer text, PR links
// and other material that must never reach a reviewer.
type FirstRoundRecord struct {
	ProjectID     string
	TaskID        string
	ReviewRound   int
	Spec          string
	Outcome       Outcome
	OutcomeReason string
	// RoundLinks are every link the submission recorded in review round 1,
	// including tombstoned ones. UntaggedLinks counts links that predate round
	// tagging and so cannot be attributed to a round.
	RoundLinks    []RecordedLink
	UntaggedLinks int
	Manifest      *RecordedManifest
	Sealed        []SealedString
}

// FirstRoundCensus is the complete result of examining a set of projects.
type FirstRoundCensus struct {
	Examined int
	Excluded map[string]int
	Records  []FirstRoundRecord
}

// ResolveSubmittedCommit pins the original first-round submitted commit from
// the links recorded in round 1. It never consults refs, branches or the
// current tree, so corrected or later work cannot be substituted. A missing or
// ambiguous commit yields an unavailable reason and detail.
func ResolveSubmittedCommit(links []RecordedLink, untagged int) (sha string, reason UnavailableReason, detail string) {
	seen := map[string]bool{}
	var commits []string
	for _, l := range links {
		if l.Kind != "commit" || seen[l.Value] {
			continue
		}
		seen[l.Value] = true
		commits = append(commits, l.Value)
	}
	switch len(commits) {
	case 0:
		d := "no commit link was recorded in review round 1"
		if untagged > 0 {
			d += fmt.Sprintf(" (%d link(s) predate round tagging and cannot be attributed to a round)", untagged)
		}
		return "", UnavailNoCommit, d
	case 1:
		if !commitSHARE.MatchString(commits[0]) {
			return "", UnavailInvalidCommit, "the round 1 commit link is not a full lowercase hex object id"
		}
		return commits[0], "", ""
	default:
		return "", UnavailAmbiguousCommit, fmt.Sprintf("%d different commit links were recorded in review round 1", len(commits))
	}
}

// CohortCriteria is the explicit, finite input to cohort construction.
type CohortCriteria struct {
	ProjectIDs []string `json:"project_ids"`
	SampleCap  int      `json:"sample_cap"`
	Seed       string   `json:"seed"`
	// ContextPaths are repository paths (files or directories) exported from the
	// pinned commit's own tree as source context. They are frozen with the
	// cohort so every candidate stages the same context.
	ContextPaths []string `json:"context_paths,omitempty"`
}

// Validate checks the criteria are explicit and finite.
func (c CohortCriteria) Validate() error {
	if len(c.ProjectIDs) == 0 {
		return invalid("at least one explicitly selected project is required")
	}
	seen := map[string]bool{}
	for _, p := range c.ProjectIDs {
		if strings.TrimSpace(p) == "" || seen[p] {
			return invalid("project id %q is empty or duplicated", p)
		}
		seen[p] = true
	}
	if c.SampleCap < 1 || c.SampleCap > MaxCohortCap {
		return invalid("sample cap must be between 1 and %d", MaxCohortCap)
	}
	if strings.TrimSpace(c.Seed) == "" {
		return invalid("a seed is required")
	}
	paths := map[string]bool{}
	for _, p := range c.ContextPaths {
		if err := ValidateWorkspacePath(p); err != nil {
			return err
		}
		if paths[p] {
			return invalid("context path %q is duplicated", p)
		}
		paths[p] = true
	}
	return nil
}

// CohortSample is one selected first-round submission. Outcome and
// SelectionReason are host metadata.
type CohortSample struct {
	Key               string            `json:"key"`
	ProjectID         string            `json:"project_id"`
	TaskID            string            `json:"task_id"`
	ReviewRound       int               `json:"review_round"`
	Outcome           Outcome           `json:"outcome"`
	SelectionReason   string            `json:"selection_reason"`
	SubmittedSHA      string            `json:"submitted_sha,omitempty"`
	ManifestDigest    string            `json:"manifest_digest,omitempty"`
	SnapshotDigest    string            `json:"snapshot_digest,omitempty"`
	SourceDigest      string            `json:"source_digest,omitempty"`
	Available         bool              `json:"available"`
	UnavailableReason UnavailableReason `json:"unavailable_reason,omitempty"`
	UnavailableDetail string            `json:"unavailable_detail,omitempty"`
}

// CohortDenominators accounts for every task examined and every sample chosen.
type CohortDenominators struct {
	Examined            int            `json:"examined"`
	Excluded            map[string]int `json:"excluded"`
	EligibleClean       int            `json:"eligible_clean"`
	EligibleRejected    int            `json:"eligible_rejected"`
	SelectedClean       int            `json:"selected_clean"`
	SelectedRejected    int            `json:"selected_rejected"`
	Available           int            `json:"available"`
	Unavailable         int            `json:"unavailable"`
	UnavailableByReason map[string]int `json:"unavailable_by_reason"`
}

// CohortManifest is the frozen, persisted cohort. Digest covers every other
// field, so a stored manifest that was edited fails Verify.
type CohortManifest struct {
	Version      int                `json:"version"`
	Criteria     CohortCriteria     `json:"criteria"`
	Denominators CohortDenominators `json:"denominators"`
	Samples      []CohortSample     `json:"samples"`
	Digest       string             `json:"digest"`
}

// SampleKey is the stable identity of a first-round sample.
func SampleKey(projectID, taskID string, round int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("sample\x00%s\x00%s\x00%d", projectID, taskID, round)))
	return hex.EncodeToString(sum[:8])
}

func selectionRank(seed, key string) string {
	sum := sha256.Sum256([]byte("rank\x00" + seed + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// SelectCohort builds the cohort deterministically: the same census, criteria
// and seed always give the same manifest, whatever order the census arrives in.
// Selection is stratified: half the cap (rounded up) goes to clean submissions
// and the rest to materially rejected ones, each ranked by a seeded hash; a
// stratum that cannot fill its share yields the remainder to the other.
// Availability is resolved after the draw and never influences it, so missing
// originals cannot bias the sample. Digests are filled by the caller once
// snapshots exist; call Seal afterwards.
func SelectCohort(criteria CohortCriteria, census FirstRoundCensus) (CohortManifest, error) {
	if err := criteria.Validate(); err != nil {
		return CohortManifest{}, err
	}
	allowed := map[string]bool{}
	for _, p := range criteria.ProjectIDs {
		allowed[p] = true
	}
	type ranked struct {
		rec  FirstRoundRecord
		key  string
		rank string
	}
	strata := map[Outcome][]ranked{}
	for _, r := range census.Records {
		if !allowed[r.ProjectID] {
			return CohortManifest{}, invalid("record for task %s is from project %s, which was not selected", r.TaskID, r.ProjectID)
		}
		if r.ReviewRound != 1 {
			return CohortManifest{}, invalid("task %s: only first-round submissions are eligible, got round %d", r.TaskID, r.ReviewRound)
		}
		if r.Outcome != OutcomeClean && r.Outcome != OutcomeRejectedMaterial {
			return CohortManifest{}, invalid("task %s has outcome %q", r.TaskID, r.Outcome)
		}
		key := SampleKey(r.ProjectID, r.TaskID, r.ReviewRound)
		strata[r.Outcome] = append(strata[r.Outcome], ranked{rec: r, key: key, rank: selectionRank(criteria.Seed, key)})
	}
	for _, s := range strata {
		sort.Slice(s, func(i, j int) bool { return s[i].rank < s[j].rank })
	}

	clean, rejected := strata[OutcomeClean], strata[OutcomeRejectedMaterial]
	wantClean := (criteria.SampleCap + 1) / 2
	wantRejected := criteria.SampleCap - wantClean
	takeClean, takeRejected := min(wantClean, len(clean)), min(wantRejected, len(rejected))
	takeClean = min(len(clean), takeClean+(wantRejected-takeRejected))
	takeRejected = min(len(rejected), takeRejected+(wantClean-min(wantClean, len(clean))))

	m := CohortManifest{
		Version:  CohortVersion,
		Criteria: CohortCriteria{ProjectIDs: sortedCopy(criteria.ProjectIDs), SampleCap: criteria.SampleCap, Seed: criteria.Seed, ContextPaths: sortedCopy(criteria.ContextPaths)},
		Denominators: CohortDenominators{
			Examined:            census.Examined,
			Excluded:            copyCounts(census.Excluded),
			EligibleClean:       len(clean),
			EligibleRejected:    len(rejected),
			SelectedClean:       takeClean,
			SelectedRejected:    takeRejected,
			UnavailableByReason: map[string]int{},
		},
		Samples: []CohortSample{},
	}
	for outcome, group := range map[Outcome][]ranked{OutcomeClean: clean[:takeClean], OutcomeRejectedMaterial: rejected[:takeRejected]} {
		total := len(strata[outcome])
		for i, g := range group {
			m.Samples = append(m.Samples, CohortSample{
				Key: g.key, ProjectID: g.rec.ProjectID, TaskID: g.rec.TaskID, ReviewRound: g.rec.ReviewRound,
				Outcome: outcome,
				SelectionReason: fmt.Sprintf("stratum %s, seeded rank %d of %d eligible, cap %d; %s",
					outcome, i+1, total, criteria.SampleCap, g.rec.OutcomeReason),
				Available: true,
			})
		}
	}
	sort.Slice(m.Samples, func(i, j int) bool {
		a, b := m.Samples[i], m.Samples[j]
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		return a.TaskID < b.TaskID
	})
	return m, nil
}

// MarkUnavailable records a sample as unavailable and keeps the denominators
// consistent. The sample stays in the manifest.
func (m *CohortManifest) MarkUnavailable(key string, reason UnavailableReason, detail string) error {
	for i := range m.Samples {
		if m.Samples[i].Key != key {
			continue
		}
		s := &m.Samples[i]
		s.Available = false
		s.UnavailableReason, s.UnavailableDetail = reason, detail
		s.SnapshotDigest, s.SourceDigest = "", ""
		return nil
	}
	return invalid("no sample %q in the cohort", key)
}

// Seal recomputes the denominators' availability counts and the digest. It
// must be called after the last change to the manifest.
func (m *CohortManifest) Seal() error {
	m.Denominators.Available, m.Denominators.Unavailable = 0, 0
	m.Denominators.UnavailableByReason = map[string]int{}
	for _, s := range m.Samples {
		if s.Available {
			if s.SubmittedSHA == "" || s.SnapshotDigest == "" {
				return invalid("available sample %s lacks a pinned commit or snapshot digest", s.Key)
			}
			m.Denominators.Available++
			continue
		}
		if s.UnavailableReason == "" {
			return invalid("unavailable sample %s lacks a reason", s.Key)
		}
		m.Denominators.Unavailable++
		m.Denominators.UnavailableByReason[string(s.UnavailableReason)]++
	}
	d, err := m.computeDigest()
	if err != nil {
		return err
	}
	m.Digest = d
	return nil
}

func (m CohortManifest) computeDigest() (string, error) {
	m.Digest = ""
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode cohort manifest: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Verify checks the manifest has not been altered since it was sealed.
func (m CohortManifest) Verify() error {
	if m.Version != CohortVersion {
		return fmt.Errorf("%w: cohort manifest version %d", ErrUnsupportedVersion, m.Version)
	}
	d, err := m.computeDigest()
	if err != nil {
		return err
	}
	if m.Digest == "" || d != m.Digest {
		return invalid("cohort manifest digest does not match its content")
	}
	return nil
}

// Encode returns the persisted form of a sealed manifest.
func (m CohortManifest) Encode() (string, error) {
	if err := m.Verify(); err != nil {
		return "", err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode cohort manifest: %w", err)
	}
	return string(b), nil
}

// DecodeCohortManifest parses and verifies a persisted manifest.
func DecodeCohortManifest(s string) (CohortManifest, error) {
	var m CohortManifest
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return CohortManifest{}, invalid("decode cohort manifest: %v", err)
	}
	return m, m.Verify()
}

// Sample returns the sample with the given key.
func (m CohortManifest) Sample(key string) (CohortSample, bool) {
	for _, s := range m.Samples {
		if s.Key == key {
			return s, true
		}
	}
	return CohortSample{}, false
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func copyCounts(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
