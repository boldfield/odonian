package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

// Evaluation endpoints expose the isolated reviewer-comparison lifecycle:
// campaigns, candidate versions, compute pools, job admission, lease renewal and
// result submission. They are separate from task and review submission: nothing
// here reads or writes a task, review task, verdict or PR link, so an
// experimental result can never vote on a research task.

const (
	// maxEvaluationLeaseTTLMs bounds every claim and renew lease request.
	maxEvaluationLeaseTTLMs = int64(time.Hour / time.Millisecond)
	// maxEvaluationCap bounds campaign and per-candidate attempt caps. Caps are
	// always finite; there is no unlimited value.
	maxEvaluationCap       = 100000
	maxEvaluationBodyBytes = 4 << 20
)

// decodeEvaluationJSON reads exactly one JSON object, rejecting unknown fields,
// trailing data and oversized bodies, and writes the 400 itself on failure.
func (s *Server) decodeEvaluationJSON(w http.ResponseWriter, r *http.Request, v interface{}) bool {
	if r.Body == nil {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "request body is required")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEvaluationBodyBytes))
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "failed to read request body")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "invalid JSON body: "+err.Error())
		return false
	}
	if dec.More() {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "request body must be a single JSON object")
		return false
	}
	return true
}

// writeEvaluationError maps a store error to its HTTP status and stable
// machine-readable code. Unrecognized errors are a generic 500 with no detail.
func (s *Server) writeEvaluationError(w http.ResponseWriter, err error) {
	var denied *store.AdmissionDeniedError
	if errors.As(err, &denied) {
		s.writeAdmissionDenied(w, denied)
		return
	}
	switch {
	case errors.Is(err, store.ErrEvaluationCampaignNotFound):
		s.errorResponse(w, http.StatusNotFound, "CAMPAIGN_NOT_FOUND", "Evaluation campaign not found")
	case errors.Is(err, store.ErrEvaluationSampleNotFound):
		s.errorResponse(w, http.StatusNotFound, "SAMPLE_NOT_FOUND", "Evaluation sample not found")
	case errors.Is(err, store.ErrEvaluationCandidateNotFound):
		s.errorResponse(w, http.StatusNotFound, "CANDIDATE_NOT_FOUND", "Evaluation candidate not found")
	case errors.Is(err, store.ErrEvaluationJobNotFound), errors.Is(err, store.ErrEvaluationAttemptNotFound):
		s.errorResponse(w, http.StatusNotFound, "ATTEMPT_NOT_FOUND", "Evaluation attempt not found")
	case errors.Is(err, store.ErrEvaluationPoolNotConfigured):
		s.errorResponse(w, http.StatusConflict, "POOL_NOT_CONFIGURED", "The candidate's evaluation pool is not configured")
	case errors.Is(err, store.ErrEvaluationAlreadyExists):
		s.errorResponse(w, http.StatusConflict, "ALREADY_EXISTS", "An evaluation record with that id already exists")
	case errors.Is(err, store.ErrEvaluationCapacityExhausted):
		s.errorResponse(w, http.StatusConflict, "CAPACITY_EXHAUSTED", "Campaign or candidate attempt cap is exhausted")
	case errors.Is(err, store.ErrEvaluationCampaignPaused):
		s.errorResponse(w, http.StatusConflict, "PAUSED_WAITING", "Evaluation campaign is paused; no new attempts are admitted")
	case errors.Is(err, store.ErrEvaluationCampaignAlreadyPaused):
		s.errorResponse(w, http.StatusConflict, "ALREADY_PAUSED", "Evaluation campaign is already paused")
	case errors.Is(err, store.ErrEvaluationAttemptLive):
		s.errorResponse(w, http.StatusConflict, "ATTEMPT_LIVE", "The previous attempt for this sample and candidate is still live")
	case errors.Is(err, store.ErrEvaluationFenceMismatch):
		s.errorResponse(w, http.StatusConflict, "FENCE_MISMATCH", "Attempt is not the job's current attempt")
	case errors.Is(err, store.ErrEvaluationAttemptExpired):
		s.errorResponse(w, http.StatusConflict, "ATTEMPT_EXPIRED", "Evaluation attempt lease has expired")
	case errors.Is(err, store.ErrEvaluationAttemptFinalized):
		s.errorResponse(w, http.StatusConflict, "ATTEMPT_FINALIZED", "Evaluation attempt is already finalized")
	case errors.Is(err, store.ErrEvaluationProjectNotAllowed):
		s.errorResponse(w, http.StatusBadRequest, "PROJECT_NOT_ALLOWED", err.Error())
	case errors.Is(err, store.ErrEvaluationModelNotAllowed):
		s.errorResponse(w, http.StatusBadRequest, "MODEL_NOT_ALLOWED", err.Error())
	case errors.Is(err, store.ErrEvaluationInvalidInput):
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", err.Error())
	case errors.Is(err, store.ErrEvaluationCandidateCorrupt):
		s.errorResponse(w, http.StatusInternalServerError, "CANDIDATE_CORRUPT", "Stored candidate failed digest verification")
	default:
		s.errorResponse(w, http.StatusInternalServerError, "INTERNAL_ERROR", "Evaluation request failed")
	}
}

type evaluationCampaignView struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       *string  `json:"description"`
	AllowedProjectIDs []string `json:"allowed_project_ids"`
	AllowedModelIDs   []string `json:"allowed_model_ids"`
	CohortManifest    string   `json:"cohort_manifest"`
	AttemptCap        int      `json:"attempt_cap"`
	PausedAt          *string  `json:"paused_at"`
	CreatedAt         string   `json:"created_at"`
}

func campaignView(c store.EvaluationCampaign) evaluationCampaignView {
	return evaluationCampaignView{
		ID: c.ID, Name: c.Name, Description: c.Description,
		AllowedProjectIDs: nonNil(c.AllowedProjectIDs), AllowedModelIDs: nonNil(c.AllowedModelIDs),
		CohortManifest: c.CohortManifest, AttemptCap: c.AttemptCap,
		PausedAt: activePause(c), CreatedAt: c.CreatedAt,
	}
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// activePause returns the pause time only while the pause is in force.
func activePause(c store.EvaluationCampaign) *string {
	if c.PausedAt != nil && c.ResumedAt == nil {
		return c.PausedAt
	}
	return nil
}

type evaluationCandidateView struct {
	ID              string                       `json:"id"`
	CampaignID      string                       `json:"campaign_id"`
	ConfigDigest    string                       `json:"config_digest"`
	AccountPoolID   string                       `json:"account_pool_id"`
	PerCandidateCap int                          `json:"per_candidate_cap"`
	Identity        evaluation.CandidateIdentity `json:"identity"`
	CreatedAt       string                       `json:"created_at"`
}

func candidateView(c store.EvaluationCandidate) evaluationCandidateView {
	return evaluationCandidateView{
		ID: c.ID, CampaignID: c.CampaignID, ConfigDigest: c.Digest(), AccountPoolID: c.AccountPoolID(),
		PerCandidateCap: c.PerCandidateCap, Identity: c.Config.Identity(), CreatedAt: c.CreatedAt,
	}
}

type evaluationPoolView struct {
	ID              string  `json:"id"`
	Mode            string  `json:"mode"`
	ConcurrentLimit int     `json:"concurrent_limit"`
	Active          int     `json:"active"`
	StartRate       float64 `json:"start_rate,omitempty"`
	BurstCapacity   int     `json:"burst_capacity,omitempty"`
	Tokens          float64 `json:"tokens,omitempty"`
}

func poolView(p store.EvaluationPoolState) evaluationPoolView {
	mode := "rate"
	if p.ConcurrencyOnly {
		mode = "concurrency_only"
	}
	return evaluationPoolView{
		ID: p.ID, Mode: mode, ConcurrentLimit: p.ConcurrentLimit, Active: p.Active,
		StartRate: p.StartRate, BurstCapacity: p.BurstCapacity, Tokens: p.Tokens,
	}
}

func validEvaluationCap(n int) bool { return n >= 1 && n <= maxEvaluationCap }

// handleConfigureEvaluationPool handles PUT /evaluation/pools/{id}.
func (s *Server) handleConfigureEvaluationPool(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ConcurrencyOnly bool    `json:"concurrency_only"`
		StartRate       float64 `json:"start_rate"`
		BurstCapacity   int     `json:"burst_capacity"`
		ConcurrentLimit int     `json:"concurrent_limit"`
	}
	if !s.decodeEvaluationJSON(w, r, &payload) {
		return
	}
	pool, err := s.store.ConfigureEvaluationPool(r.Context(), store.EvaluationPoolConfig{
		ID: r.PathValue("id"), ConcurrencyOnly: payload.ConcurrencyOnly, StartRate: payload.StartRate,
		BurstCapacity: payload.BurstCapacity, ConcurrentLimit: payload.ConcurrentLimit,
	})
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{"pool": poolView(pool)})
}

// handleGetEvaluationPool handles GET /evaluation/pools/{id}.
func (s *Server) handleGetEvaluationPool(w http.ResponseWriter, r *http.Request) {
	pool, err := s.store.GetEvaluationPool(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{"pool": poolView(pool)})
}

// handleCreateEvaluationCampaign handles POST /evaluation/campaigns.
func (s *Server) handleCreateEvaluationCampaign(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ID                string   `json:"id"`
		Name              string   `json:"name"`
		Description       *string  `json:"description"`
		AllowedProjectIDs []string `json:"allowed_project_ids"`
		AllowedModelIDs   []string `json:"allowed_model_ids"`
		CohortManifest    string   `json:"cohort_manifest"`
		AttemptCap        int      `json:"attempt_cap"`
	}
	if !s.decodeEvaluationJSON(w, r, &payload) {
		return
	}
	if !validEvaluationCap(payload.AttemptCap) {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "attempt_cap must be a finite integer between 1 and 100000")
		return
	}
	campaign, err := s.store.CreateEvaluationCampaign(r.Context(), store.EvaluationCampaign{
		ID: payload.ID, Name: payload.Name, Description: payload.Description,
		AllowedProjectIDs: payload.AllowedProjectIDs, AllowedModelIDs: payload.AllowedModelIDs,
		CohortManifest: payload.CohortManifest, AttemptCap: payload.AttemptCap,
	})
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusCreated, map[string]interface{}{"campaign": campaignView(campaign)})
}

// handleGetEvaluationCampaign handles GET /evaluation/campaigns/{id}.
func (s *Server) handleGetEvaluationCampaign(w http.ResponseWriter, r *http.Request) {
	campaign, err := s.store.GetEvaluationCampaign(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{"campaign": campaignView(campaign)})
}

// handleCreateEvaluationCandidate handles POST /evaluation/campaigns/{id}/candidates.
// The candidate is an immutable version: the server derives its digest from the
// validated identity, never from the caller.
func (s *Server) handleCreateEvaluationCandidate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ID              string                       `json:"id"`
		PerCandidateCap int                          `json:"per_candidate_cap"`
		Identity        evaluation.CandidateIdentity `json:"identity"`
	}
	if !s.decodeEvaluationJSON(w, r, &payload) {
		return
	}
	if !validEvaluationCap(payload.PerCandidateCap) {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "per_candidate_cap must be a finite integer between 1 and 100000")
		return
	}
	config, err := evaluation.NewCandidateConfig(payload.Identity)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", err.Error())
		return
	}
	candidate, err := s.store.CreateEvaluationCandidate(r.Context(), store.EvaluationCandidate{
		ID: payload.ID, CampaignID: r.PathValue("id"), PerCandidateCap: payload.PerCandidateCap, Config: config,
	})
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusCreated, map[string]interface{}{"candidate": candidateView(candidate)})
}

// handleListEvaluationCandidates handles GET /evaluation/campaigns/{id}/candidates.
func (s *Server) handleListEvaluationCandidates(w http.ResponseWriter, r *http.Request) {
	candidates, err := s.store.ListEvaluationCandidates(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	views := make([]evaluationCandidateView, 0, len(candidates))
	for _, c := range candidates {
		views = append(views, candidateView(c))
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{"candidates": views})
}

// handleGetEvaluationSample handles GET /evaluation/campaigns/{campaign_id}/samples/{sample_id}.
func (s *Server) handleGetEvaluationSample(w http.ResponseWriter, r *http.Request) {
	sample, err := s.store.GetEvaluationSample(r.Context(), r.PathValue("sample_id"))
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	if sample.CampaignID != r.PathValue("campaign_id") {
		// A sample outside the named campaign is indistinguishable from a missing one.
		s.writeEvaluationError(w, store.ErrEvaluationSampleNotFound)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{
		"sample": map[string]interface{}{
			"id":                    sample.ID,
			"campaign_id":           sample.CampaignID,
			"project_id":            sample.ProjectID,
			"original_task_id":      sample.OriginalTaskID,
			"original_review_round": sample.OriginalReviewRound,
			"submitted_sha":         sample.SubmittedSHA,
			"snapshot_digest":       sample.SnapshotDigest,
			"source_digest":         sample.SourceDigest,
			"manifest_digest":       sample.ManifestDigest,
			"prompt_version":        sample.PromptVersion,
			"model_version":         sample.ModelVersion,
			"runtime_version":       sample.RuntimeVersion,
			"created_at":            sample.CreatedAt,
		},
	})
}

type evaluationCandidateStatusView struct {
	CandidateID       string              `json:"candidate_id"`
	ConfigDigest      string              `json:"config_digest"`
	AdapterName       string              `json:"adapter_name"`
	ModelID           string              `json:"model_id"`
	ModelRevision     string              `json:"model_revision"`
	AccountPoolID     string              `json:"account_pool_id"`
	PerCandidateCap   int                 `json:"per_candidate_cap"`
	AttemptsUsed      int                 `json:"attempts_used"`
	AttemptsRemaining int                 `json:"attempts_remaining"`
	ActiveAttempts    int                 `json:"active_attempts"`
	Exhausted         bool                `json:"exhausted"`
	Pool              *evaluationPoolView `json:"pool"`
}

// handleGetEvaluationCampaignStatus handles GET /evaluation/campaigns/{id}/status.
// state is "paused" while an explicit pause is in force, otherwise "exhausted"
// when the campaign cap or every candidate cap is spent, otherwise "active".
func (s *Server) handleGetEvaluationCampaignStatus(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.GetEvaluationCampaignStatus(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	candidates := make([]evaluationCandidateStatusView, 0, len(st.Candidates))
	allExhausted := len(st.Candidates) > 0
	for _, c := range st.Candidates {
		id := c.Candidate.Config.Identity()
		remaining := max(c.Candidate.PerCandidateCap-c.AttemptsUsed, 0)
		view := evaluationCandidateStatusView{
			CandidateID: c.Candidate.ID, ConfigDigest: c.Candidate.Digest(), AdapterName: id.AdapterName,
			ModelID: id.ModelID, ModelRevision: id.ModelRevision, AccountPoolID: c.Candidate.AccountPoolID(),
			PerCandidateCap: c.Candidate.PerCandidateCap, AttemptsUsed: c.AttemptsUsed,
			AttemptsRemaining: remaining, ActiveAttempts: c.Active, Exhausted: remaining == 0,
		}
		if c.Pool != nil {
			pv := poolView(*c.Pool)
			view.Pool = &pv
		}
		allExhausted = allExhausted && view.Exhausted
		candidates = append(candidates, view)
	}
	campaign := st.Campaign
	remaining := max(campaign.AttemptCap-st.TotalAttemptsUsed, 0)
	state := "active"
	switch {
	case activePause(campaign) != nil:
		state = "paused"
	case remaining == 0 || allExhausted:
		state = "exhausted"
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{
		"campaign_id":        campaign.ID,
		"name":               campaign.Name,
		"state":              state,
		"paused_at":          activePause(campaign),
		"attempt_cap":        campaign.AttemptCap,
		"attempts_used":      st.TotalAttemptsUsed,
		"attempts_remaining": remaining,
		"candidates":         candidates,
	})
}

// handlePauseEvaluationCampaign handles POST /evaluation/campaigns/{id}/pause.
// A paused campaign admits no new attempts; attempts already live may still
// renew and finalize. Pausing an already paused campaign is a 409.
func (s *Server) handlePauseEvaluationCampaign(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.PauseEvaluationCampaign(r.Context(), id); err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	campaign, err := s.store.GetEvaluationCampaign(r.Context(), id)
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{
		"campaign_id": id,
		"state":       "paused",
		"paused_at":   campaign.PausedAt,
	})
}

type evaluationAttemptView struct {
	ID                string  `json:"id"`
	JobID             string  `json:"job_id"`
	RequestID         string  `json:"request_id"`
	SequenceNumber    int     `json:"sequence_number"`
	PreviousAttemptID *string `json:"previous_attempt_id"`
	State             string  `json:"state"`
	StartedAt         string  `json:"started_at"`
	ExpiresAt         string  `json:"expires_at"`
	EndedAt           *string `json:"ended_at"`
	ExitClass         *string `json:"exit_class"`
	Status            *string `json:"status"`
}

func attemptView(a store.EvaluationAttempt) evaluationAttemptView {
	v := evaluationAttemptView{
		ID: a.ID, JobID: a.JobID, RequestID: a.RequestID, SequenceNumber: a.SequenceNumber,
		PreviousAttemptID: a.PreviousAttemptID, State: a.State, StartedAt: a.StartedAt,
		ExpiresAt: a.ExpiresAt, EndedAt: a.EndedAt, Status: a.Status,
	}
	if a.ExitClass != nil {
		ec := string(*a.ExitClass)
		v.ExitClass = &ec
	}
	return v
}

// handleClaimEvaluationJob handles POST /evaluation/jobs/claim. A repeated
// request_id for the same sample and candidate returns the original attempt
// without spending cap or pool allowance again.
func (s *Server) handleClaimEvaluationJob(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		SampleID    string `json:"sample_id"`
		CandidateID string `json:"candidate_id"`
		RequestID   string `json:"request_id"`
		LeaseTTLMs  int64  `json:"lease_ttl_ms"`
	}
	if !s.decodeEvaluationJSON(w, r, &payload) {
		return
	}
	if payload.SampleID == "" || payload.CandidateID == "" || payload.RequestID == "" {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "sample_id, candidate_id and request_id are required")
		return
	}
	if payload.LeaseTTLMs < 1 || payload.LeaseTTLMs > maxEvaluationLeaseTTLMs {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "lease_ttl_ms must be between 1 and 3600000")
		return
	}
	result, err := s.store.ClaimEvaluationJob(r.Context(), store.EvaluationJobClaim{
		SampleID: payload.SampleID, CandidateID: payload.CandidateID, RequestID: payload.RequestID,
		LeaseExpires: time.Duration(payload.LeaseTTLMs) * time.Millisecond,
	})
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{
		"job": map[string]interface{}{
			"id":                 result.Job.ID,
			"sample_id":          result.Job.SampleID,
			"candidate_id":       result.Job.CandidateID,
			"current_attempt_id": result.Job.CurrentAttemptID,
			"created_at":         result.Job.CreatedAt,
		},
		"attempt": attemptView(result.Attempt),
	})
}

// attemptForJob loads the path's attempt and requires it to belong to the
// path's job, so a request can never act on an attempt through the wrong job.
func (s *Server) attemptForJob(w http.ResponseWriter, r *http.Request) (store.EvaluationAttempt, bool) {
	attempt, err := s.store.GetEvaluationAttempt(r.Context(), r.PathValue("attempt_id"))
	if err != nil {
		s.writeEvaluationError(w, err)
		return store.EvaluationAttempt{}, false
	}
	if attempt.JobID != r.PathValue("job_id") {
		s.writeEvaluationError(w, store.ErrEvaluationAttemptNotFound)
		return store.EvaluationAttempt{}, false
	}
	return attempt, true
}

// handleRenewEvaluationAttempt handles POST /evaluation/jobs/{job_id}/attempts/{attempt_id}/renew.
// The new lease is lease_ttl_ms from the server's clock and must extend the
// current lease.
func (s *Server) handleRenewEvaluationAttempt(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		LeaseTTLMs int64 `json:"lease_ttl_ms"`
	}
	if !s.decodeEvaluationJSON(w, r, &payload) {
		return
	}
	if payload.LeaseTTLMs < 1 || payload.LeaseTTLMs > maxEvaluationLeaseTTLMs {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "lease_ttl_ms must be between 1 and 3600000")
		return
	}
	attempt, ok := s.attemptForJob(w, r)
	if !ok {
		return
	}
	expiresAt := s.store.Now().UTC().Add(time.Duration(payload.LeaseTTLMs) * time.Millisecond)
	if err := s.store.RenewEvaluationAttempt(r.Context(), attempt.ID, expiresAt); err != nil {
		if errors.Is(err, store.ErrEvaluationInvalidInput) {
			s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "lease_ttl_ms must extend the current lease")
			return
		}
		s.writeEvaluationError(w, err)
		return
	}
	renewed, err := s.store.GetEvaluationAttempt(r.Context(), attempt.ID)
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{"attempt": attemptView(renewed)})
}

// handleFinalizeEvaluationAttempt handles POST /evaluation/jobs/{job_id}/attempts/{attempt_id}/finalize.
// It records the attempt's result and findings atomically in evaluation tables
// only. fence_attempt_id must equal the path attempt, which must be the job's
// current, still-live attempt; anything else is rejected without recording.
func (s *Server) handleFinalizeEvaluationAttempt(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		FenceAttemptID string                 `json:"fence_attempt_id"`
		ExitClass      string                 `json:"exit_class"`
		Status         *evaluation.Status     `json:"status"`
		ErrorClass     *evaluation.ErrorClass `json:"error_class"`
		ErrorMessage   *string                `json:"error_message"`
		DurationMs     *int                   `json:"duration_ms"`
		UsageTokens    *int                   `json:"usage_tokens"`
		Findings       []evaluation.Finding   `json:"findings"`

		Detail *store.EvaluationAttemptDetail `json:"detail"`
	}
	if !s.decodeEvaluationJSON(w, r, &payload) {
		return
	}
	if payload.FenceAttemptID == "" || payload.ExitClass == "" {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_INPUT", "fence_attempt_id and exit_class are required")
		return
	}
	attempt, ok := s.attemptForJob(w, r)
	if !ok {
		return
	}
	err := s.store.FinalizeEvaluationAttempt(r.Context(), store.EvaluationAttemptResult{
		AttemptID: attempt.ID, FenceAttemptID: payload.FenceAttemptID,
		ExitClass: store.EvaluationExitClass(payload.ExitClass), Status: payload.Status,
		ErrorClass: payload.ErrorClass, ErrorMessage: payload.ErrorMessage,
		DurationMs: payload.DurationMs, UsageTokens: payload.UsageTokens, Findings: payload.Findings, Detail: payload.Detail,
	})
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	final, err := s.store.GetEvaluationAttempt(r.Context(), attempt.ID)
	if err != nil {
		s.writeEvaluationError(w, err)
		return
	}
	s.encodeJSON(w, http.StatusOK, map[string]interface{}{
		"attempt":       attemptView(final),
		"finding_count": len(payload.Findings),
	})
}
