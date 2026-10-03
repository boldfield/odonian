package evalcohort

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

// ErrCohortMismatch means the campaign, its frozen manifest, a sample or a
// candidate do not agree, so nothing can be staged from them.
var ErrCohortMismatch = errors.New("campaign cohort does not match the frozen records")

// WebBlindingNotEnforced is recorded in every candidate's limits. The host
// stages a clean directory; it does not sandbox the candidate's network, and
// published material about a submission may be discoverable on the web.
const WebBlindingNotEnforced = "not_enforced"

// CandidateLimits are the source and tool limits of one candidate on one
// sample, persisted with its staging record. They are candidate-specific: they
// come from that candidate's own identity and requirements.
type CandidateLimits struct {
	ToolAccess          evaluation.ToolAccessRequirements `json:"tool_access"`
	CandidateTools      evaluation.NameSet                `json:"candidate_tools"`
	CandidateObservers  evaluation.NameSet                `json:"candidate_observers"`
	SourceScope         string                            `json:"source_scope"`
	ContextPaths        []string                          `json:"context_paths"`
	WebDiscoverability  string                            `json:"web_discoverability"`
	WorkspaceFilesBytes [2]int                            `json:"workspace_files_bytes"`
}

// StageRequest names one candidate on one frozen sample.
type StageRequest struct {
	CampaignID  string
	SampleID    string
	CandidateID string
	// RunID identifies this run to the adapter (typically the attempt id).
	RunID string
	// ToolAccess is what this candidate's review needs from its runtime.
	ToolAccess evaluation.ToolAccessRequirements
}

// Staged is one candidate's fresh workspace for one sample.
type Staged struct {
	Staging   store.EvaluationStaging
	Workspace *evaluation.Workspace
	Request   evaluation.CandidateRequest
	Limits    CandidateLimits
}

// Validate re-reads the workspace from disk and checks it against the frozen
// snapshot digest. Call it immediately before running the candidate; it
// detects modification, it does not prevent it.
func (s *Staged) Validate() error {
	return evaluation.ValidateWorkspace(s.Workspace.Path, s.Staging.SnapshotDigest)
}

// Cleanup removes the workspace and its result file. It is safe to call twice.
func (s *Staged) Cleanup() error {
	return errors.Join(s.Workspace.Remove(), removeIfExists(s.Request.ResultPath))
}

func removeIfExists(p string) error {
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Stager stages candidate workspaces under Root, one directory tree per
// candidate, so two candidates never share a file.
type Stager struct {
	Store   store.Store
	Sources Sources
	Root    string
}

func plainName(kind, name string) error {
	if name == "" || name != filepath.Base(name) || name[0] == '.' {
		return fmt.Errorf("%w: %s id %q is not a plain name", evaluation.ErrInvalid, kind, name)
	}
	return nil
}

// Stage rebuilds the sample's workspace from the pinned round-1 commit named in
// the campaign's frozen cohort manifest, verifies it reproduces the frozen
// digests exactly, scans it for sealed material, writes it to a fresh
// directory unique to this candidate, verifies the bytes on disk, and records
// the staging with the candidate's limits. A failure removes anything it
// created. An *UnavailableError is returned when the original can no longer be
// reproduced; callers should finalize the attempt as unavailable_snapshot.
func (st Stager) Stage(ctx context.Context, req StageRequest) (*Staged, error) {
	for kind, name := range map[string]string{"campaign": req.CampaignID, "sample": req.SampleID, "candidate": req.CandidateID} {
		if err := plainName(kind, name); err != nil {
			return nil, err
		}
	}
	if err := req.ToolAccess.Validate(); err != nil {
		return nil, err
	}
	root, err := filepath.Abs(st.Root)
	if err != nil {
		return nil, err
	}

	campaign, err := st.Store.GetEvaluationCampaign(ctx, req.CampaignID)
	if err != nil {
		return nil, err
	}
	manifest, err := evaluation.DecodeCohortManifest(campaign.CohortManifest)
	if err != nil {
		return nil, fmt.Errorf("%w: campaign %s has no verified cohort manifest: %v", ErrCohortMismatch, campaign.ID, err)
	}
	sample, err := st.Store.GetEvaluationSample(ctx, req.SampleID)
	if err != nil {
		return nil, err
	}
	candidate, err := st.Store.GetEvaluationCandidate(ctx, req.CandidateID)
	if err != nil {
		return nil, err
	}
	if sample.CampaignID != campaign.ID || candidate.CampaignID != campaign.ID {
		return nil, fmt.Errorf("%w: sample or candidate belongs to another campaign", ErrCohortMismatch)
	}
	cs, ok := manifest.Sample(evaluation.SampleKey(sample.ProjectID, sample.OriginalTaskID, sample.OriginalReviewRound))
	if !ok || !cs.Available || cs.SubmittedSHA != sample.SubmittedSHA ||
		sample.SnapshotDigest == nil || cs.SnapshotDigest != *sample.SnapshotDigest ||
		sample.SourceDigest == nil || cs.SourceDigest != *sample.SourceDigest {
		return nil, fmt.Errorf("%w: sample %s is not an available sample of the frozen manifest", ErrCohortMismatch, sample.ID)
	}

	rec, err := st.Store.GetFirstRoundRecord(ctx, sample.OriginalTaskID)
	if err != nil {
		return nil, unavailable(evaluation.UnavailFrozenInputChanged, "the original first-round record can no longer be read: %v", err)
	}
	sha, manifestDigest, err := resolve(rec)
	if err != nil {
		return nil, err
	}
	if sha != cs.SubmittedSHA || manifestDigest != cs.ManifestDigest {
		return nil, unavailable(evaluation.UnavailFrozenInputChanged, "the board's round 1 commit or manifest no longer matches the frozen cohort")
	}
	src, err := st.Sources.CommitSource(ctx, sample.ProjectID)
	if err != nil {
		return nil, unavailable(evaluation.UnavailArtifactUnreachable, "%v", err)
	}
	a, err := assemble(ctx, src, rec, sha, manifest.Criteria.ContextPaths)
	if err != nil {
		return nil, err
	}
	if a.snapshot != cs.SnapshotDigest || a.source != cs.SourceDigest {
		return nil, unavailable(evaluation.UnavailFrozenInputChanged, "rebuilt workspace digest differs from the frozen digest")
	}

	dir := filepath.Join(root, req.CampaignID, req.CandidateID)
	resultPath := filepath.Join(dir, "results", req.SampleID+".json")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		return nil, fmt.Errorf("create candidate directory: %w", err)
	}
	// A leftover workspace for this exact slot belongs to an earlier attempt of
	// this same candidate; replace it rather than reuse possibly modified files.
	if err := os.RemoveAll(filepath.Join(dir, "workspaces", req.SampleID)); err != nil {
		return nil, fmt.Errorf("remove earlier workspace: %w", err)
	}
	if err := removeIfExists(resultPath); err != nil {
		return nil, err
	}
	ws, err := evaluation.StageWorkspace(filepath.Join(dir, "workspaces"), req.SampleID, a.spec)
	if err != nil {
		return nil, err
	}
	staged, err := st.finish(ctx, req, candidate, sample, manifest.Criteria.ContextPaths, a, rec.Sealed, ws, resultPath)
	if err != nil {
		ws.Remove()
		return nil, err
	}
	return staged, nil
}

func (st Stager) finish(ctx context.Context, req StageRequest, candidate store.EvaluationCandidate, sample store.EvaluationSample,
	contextPaths []string, a assembly, sealed []evaluation.SealedString, ws *evaluation.Workspace, resultPath string) (*Staged, error) {
	if ws.SnapshotDigest != a.snapshot || ws.SourceDigest != a.source {
		return nil, unavailable(evaluation.UnavailFrozenInputChanged, "staged workspace digest differs from the frozen digest")
	}
	hits, err := evaluation.ScanWorkspaceDir(ws.Path, a.prompt, sealed)
	if err != nil {
		return nil, err
	}
	if len(hits) > 0 {
		return nil, unavailable(evaluation.UnavailContaminated, "%v", evaluation.LeakError(hits))
	}
	id := candidate.Config.Identity()
	limits := CandidateLimits{
		ToolAccess: req.ToolAccess, CandidateTools: id.Tools, CandidateObservers: id.Observers,
		SourceScope:         "artifact_and_pinned_context_paths_only",
		ContextPaths:        append([]string{}, contextPaths...),
		WebDiscoverability:  WebBlindingNotEnforced,
		WorkspaceFilesBytes: [2]int{ws.Files, ws.Bytes},
	}
	limitsJSON, err := json.Marshal(limits)
	if err != nil {
		return nil, err
	}
	staging, err := st.Store.RecordEvaluationStaging(ctx, store.EvaluationStaging{
		ID: store.GenerateID(), CampaignID: req.CampaignID, SampleID: req.SampleID, CandidateID: req.CandidateID,
		CandidateConfigDigest: candidate.Digest(), SnapshotDigest: ws.SnapshotDigest, SourceDigest: ws.SourceDigest,
		PromptVersion: evaluation.BlindedPromptVersion, PromptDigest: evaluation.PromptDigest(a.prompt),
		LimitsJSON: string(limitsJSON),
	})
	if err != nil {
		return nil, err
	}
	return &Staged{
		Staging: staging, Workspace: ws, Limits: limits,
		Request: evaluation.CandidateRequest{
			Version: evaluation.ProtocolVersion, RunID: req.RunID, SnapshotPath: ws.Path,
			BlindedPrompt: a.prompt, ToolAccess: req.ToolAccess, ResultPath: resultPath,
		},
	}, nil
}
