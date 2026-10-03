// Package evalcohort builds reproducible first-round review cohorts from the
// board's own records, freezes them into evaluation campaigns, and stages an
// independent, history-free workspace for each candidate. It joins the store
// and the evaluation package, which cannot import each other.
package evalcohort

import (
	"context"
	"errors"
	"fmt"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

// Placeholders recorded on frozen samples. The model and runtime are properties
// of each candidate, not of the cohort.
const (
	SampleModelVersion   = "per-candidate"
	SampleRuntimeVersion = "per-candidate"
)

// Sources resolves the repository a project's submissions were committed to.
type Sources interface {
	CommitSource(ctx context.Context, projectID string) (evaluation.CommitSource, error)
}

// SourceMap is a Sources backed by a fixed project-to-repository table.
type SourceMap map[string]evaluation.CommitSource

func (m SourceMap) CommitSource(_ context.Context, projectID string) (evaluation.CommitSource, error) {
	if src, ok := m[projectID]; ok && src != nil {
		return src, nil
	}
	return nil, fmt.Errorf("no repository is registered for project %q", projectID)
}

// UnavailableError means a sample's original cannot be reproduced. It is a
// recorded, visible outcome and not a build failure.
type UnavailableError struct {
	Reason evaluation.UnavailableReason
	Detail string
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("sample unavailable (%s): %s", e.Reason, e.Detail)
}

func unavailable(reason evaluation.UnavailableReason, format string, a ...any) error {
	return &UnavailableError{Reason: reason, Detail: fmt.Sprintf(format, a...)}
}

// assembly is everything pinned for one sample.
type assembly struct {
	spec           evaluation.WorkspaceSpec
	sha            string
	manifestDigest string
	snapshot       string
	source         string
	prompt         string
}

// resolve pins the original round-1 commit and manifest from the record alone.
func resolve(rec evaluation.FirstRoundRecord) (sha, manifestDigest string, err error) {
	sha, reason, detail := evaluation.ResolveSubmittedCommit(rec.RoundLinks, rec.UntaggedLinks)
	if reason != "" {
		return "", "", unavailable(reason, "%s", detail)
	}
	if rec.Manifest != nil {
		if !rec.Manifest.Valid() {
			return sha, "", unavailable(evaluation.UnavailManifestCorrupt, "the stored round 1 manifest does not match its recorded digest")
		}
		manifestDigest = rec.Manifest.Digest
	}
	return sha, manifestDigest, nil
}

// assemble reads the artifact and source context from the pinned commit only,
// verifies nothing sealed is present, and computes the workspace digests.
func assemble(ctx context.Context, src evaluation.CommitSource, rec evaluation.FirstRoundRecord, sha string, contextPaths []string) (assembly, error) {
	a := assembly{sha: sha}
	fail := func(err error) (assembly, error) {
		switch {
		case ctx.Err() != nil:
			return a, ctx.Err()
		case errors.Is(err, evaluation.ErrCommitUnavailable), errors.Is(err, evaluation.ErrUnsupportedEntry),
			errors.Is(err, evaluation.ErrUnsafeWorkspace), errors.Is(err, evaluation.ErrWorkspaceTooLarge),
			errors.Is(err, evaluation.ErrInvalid):
			return a, unavailable(evaluation.UnavailArtifactUnreachable, "%v", err)
		}
		return a, err
	}
	if err := src.CommitExists(ctx, sha); err != nil {
		return fail(err)
	}
	artifact, err := src.ChangedFiles(ctx, sha)
	if err != nil {
		return fail(err)
	}
	if len(artifact) == 0 {
		return a, unavailable(evaluation.UnavailArtifactUnreachable, "commit %s changed no files", sha)
	}
	var sourceContext []evaluation.WorkspaceFile
	if len(contextPaths) > 0 {
		if sourceContext, err = src.TreeFiles(ctx, sha, contextPaths); err != nil {
			return fail(err)
		}
	}
	a.spec = evaluation.WorkspaceSpec{Artifact: artifact, Context: sourceContext, Acceptance: rec.Spec}
	if a.snapshot, a.source, err = a.spec.Digests(); err != nil {
		return fail(err)
	}
	a.prompt = evaluation.BuildBlindedPrompt(rec.Spec)
	hits, err := evaluation.ScanWorkspace(a.spec, a.prompt, rec.Sealed)
	if err != nil {
		return fail(err)
	}
	if len(hits) > 0 {
		return a, unavailable(evaluation.UnavailContaminated, "%v", evaluation.LeakError(hits))
	}
	return a, nil
}

// Builder constructs cohorts from the board.
type Builder struct {
	Store   store.Store
	Sources Sources
}

// Build selects a cohort from the explicitly named projects and freezes each
// selected sample: it derives the round-1 commit from the board's own links,
// exports that commit's artifact and context, scans for leakage, and records
// the digests. Samples whose original cannot be reproduced stay in the
// manifest, marked unavailable with a reason. Build writes nothing to the
// store and creates no files.
func (b Builder) Build(ctx context.Context, criteria evaluation.CohortCriteria) (evaluation.CohortManifest, error) {
	census, err := b.Store.FirstRoundCensus(ctx, criteria.ProjectIDs)
	if err != nil {
		return evaluation.CohortManifest{}, err
	}
	manifest, err := evaluation.SelectCohort(criteria, census)
	if err != nil {
		return evaluation.CohortManifest{}, err
	}
	records := make(map[string]evaluation.FirstRoundRecord, len(census.Records))
	for _, r := range census.Records {
		records[evaluation.SampleKey(r.ProjectID, r.TaskID, r.ReviewRound)] = r
	}
	for i := range manifest.Samples {
		s := &manifest.Samples[i]
		err := b.freeze(ctx, s, records[s.Key], manifest.Criteria.ContextPaths)
		var u *UnavailableError
		switch {
		case errors.As(err, &u):
			if err := manifest.MarkUnavailable(s.Key, u.Reason, u.Detail); err != nil {
				return evaluation.CohortManifest{}, err
			}
		case err != nil:
			return evaluation.CohortManifest{}, fmt.Errorf("freeze sample %s: %w", s.Key, err)
		}
	}
	if err := manifest.Seal(); err != nil {
		return evaluation.CohortManifest{}, err
	}
	return manifest, nil
}

func (b Builder) freeze(ctx context.Context, s *evaluation.CohortSample, rec evaluation.FirstRoundRecord, contextPaths []string) error {
	sha, manifestDigest, err := resolve(rec)
	s.SubmittedSHA = sha
	if err != nil {
		return err
	}
	s.ManifestDigest = manifestDigest
	src, err := b.Sources.CommitSource(ctx, s.ProjectID)
	if err != nil {
		return unavailable(evaluation.UnavailArtifactUnreachable, "%v", err)
	}
	a, err := assemble(ctx, src, rec, sha, contextPaths)
	if err != nil {
		return err
	}
	s.SnapshotDigest, s.SourceDigest = a.snapshot, a.source
	return nil
}

// CampaignSpec is the campaign-level configuration around a cohort.
type CampaignSpec struct {
	Name            string
	Description     *string
	AllowedModelIDs []string
	AttemptCap      int
}

// Registered is a campaign with its frozen samples, keyed by cohort sample key.
type Registered struct {
	Campaign store.EvaluationCampaign
	Samples  map[string]store.EvaluationSample
}

// Register persists a sealed cohort as an evaluation campaign: the encoded
// manifest (including unavailable samples and denominators) is the campaign's
// cohort manifest, and every available sample is frozen as an immutable
// evaluation sample carrying its pinned commit and digests. Candidates are
// added to the returned campaign afterwards and never alter these rows. If
// registration fails part way the campaign is paused so it cannot run
// against a partial cohort.
func (b Builder) Register(ctx context.Context, spec CampaignSpec, manifest evaluation.CohortManifest) (Registered, error) {
	if err := manifest.Verify(); err != nil {
		return Registered{}, err
	}
	if manifest.Digest == "" || manifest.Denominators.Available == 0 {
		return Registered{}, fmt.Errorf("%w: a sealed cohort with at least one available sample is required", evaluation.ErrInvalid)
	}
	encoded, err := manifest.Encode()
	if err != nil {
		return Registered{}, err
	}
	campaign, err := b.Store.CreateEvaluationCampaign(ctx, store.EvaluationCampaign{
		ID: store.GenerateID(), Name: spec.Name, Description: spec.Description,
		AllowedProjectIDs: manifest.Criteria.ProjectIDs, AllowedModelIDs: spec.AllowedModelIDs,
		CohortManifest: encoded, AttemptCap: spec.AttemptCap,
	})
	if err != nil {
		return Registered{}, err
	}
	out := Registered{Campaign: campaign, Samples: map[string]store.EvaluationSample{}}
	for _, s := range manifest.Samples {
		if !s.Available {
			continue
		}
		sample := store.EvaluationSample{
			ID: store.GenerateID(), CampaignID: campaign.ID, ProjectID: s.ProjectID, OriginalTaskID: s.TaskID,
			OriginalReviewRound: s.ReviewRound, SubmittedSHA: s.SubmittedSHA,
			SnapshotDigest: &s.SnapshotDigest, SourceDigest: &s.SourceDigest,
			PromptVersion: evaluation.BlindedPromptVersion, ModelVersion: SampleModelVersion, RuntimeVersion: SampleRuntimeVersion,
		}
		if s.ManifestDigest != "" {
			sample.ManifestDigest = &s.ManifestDigest
		}
		created, err := b.Store.CreateEvaluationSample(ctx, sample)
		if err != nil {
			perr := b.Store.PauseEvaluationCampaign(ctx, campaign.ID)
			return Registered{}, fmt.Errorf("freeze sample %s into campaign %s (campaign paused: %v): %w", s.Key, campaign.ID, perr == nil, err)
		}
		out.Samples[s.Key] = created
	}
	return out, nil
}
