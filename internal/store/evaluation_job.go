package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
)

// Evaluation job lifecycle. A job is the durable evaluation of one sample
// by one candidate: it binds a sample, candidate, and the caller's request ID.
// Each start of the candidate is an attempt; the attempt ID is the fencing
// identity for renew and finalize. All helpers take the server time as an
// argument (an injectable clock).

var (
	ErrEvaluationJobNotFound       = errors.New("evaluation job not found")
	ErrEvaluationAttemptNotFound   = errors.New("evaluation attempt not found")
	ErrEvaluationCampaignNotFound  = errors.New("evaluation campaign not found")
	ErrEvaluationFenceMismatch     = errors.New("evaluation attempt identity does not match the job's current attempt")
	ErrEvaluationAttemptExpired    = errors.New("evaluation attempt lease expired")
	ErrEvaluationAttemptFinalized  = errors.New("evaluation attempt already finalized")
	ErrEvaluationAttemptLive       = errors.New("previous evaluation attempt is still live")
	ErrEvaluationCapacityExhausted = errors.New("evaluation campaign or candidate has exhausted attempt capacity")
	ErrEvaluationDuplicateJob      = errors.New("evaluation job already exists for this sample and candidate")
	ErrEvaluationInvalidInput      = errors.New("invalid evaluation input")
)

// Exit classes for a finalized attempt.
const (
	EvalExitCompleted    = "completed"
	EvalExitFailed       = "failed"
	EvalExitCancelled    = "cancelled"
	EvalExitUnknown      = "unknown"
	EvalExitLeaseExpired = "lease_expired"
)

// Attempt states.
const (
	EvalAttemptActive    = "active"
	EvalAttemptFinalized = "finalized"
	EvalAttemptExpired   = "expired"
)

// EvaluationCampaign represents a finite evaluation experiment.
type EvaluationCampaign struct {
	ID             string
	Name           string
	Description    *string
	ProjectID      string
	AllowedModelID string
	CohortManifest string
	AttemptCap     int
	AccountPoolID  string
	CreatedAt      string
	UpdatedAt      string
}

// EvaluationCandidate represents an immutable model version for evaluation.
type EvaluationCandidate struct {
	ID                 string
	CampaignID         string
	AdapterName        string
	ModelIdentity      string
	ModelRevision      *string
	RuntimeVersion     string
	ReasoningConfig    *string
	GenerationConfig   *string
	PromptVersion      string
	ToolAccessConfig   *string
	SourceAccessConfig *string
	AccountPoolID      string
	PerCandidateCap    int
	CreatedAt          string
}

// EvaluationSample represents an original task/review bound for evaluation.
type EvaluationSample struct {
	ID                  string
	CampaignID          string
	OriginalTaskID      string
	OriginalReviewRound int
	SubmittedSHA        string
	SnapshotDigest      *string
	SourceDigest        *string
	ManifestDigest      *string
	PromptVersion       string
	ModelVersion        string
	RuntimeVersion      string
	CreatedAt           string
}

// EvaluationJob represents a job binding sample and candidate.
type EvaluationJob struct {
	ID               string
	SampleID         string
	CandidateID      string
	RequestID        string
	CurrentAttemptID string
	CreatedAt        string
}

// EvaluationAttempt represents one attempt at evaluating a job.
type EvaluationAttempt struct {
	ID                string
	JobID             string
	PreviousAttemptID *string
	SequenceNumber    int
	State             string
	StartedAt         string
	ExpiresAt         string
	EndedAt           *string
	ExitClass         *string
	Status            *string
	ErrorClass        *string
	ErrorMessage      *string
	DurationMs        *int
	UsageTokens       *int
}

// EvaluationJobClaim bundles the input for claiming an evaluation job.
type EvaluationJobClaim struct {
	SampleID     string
	CandidateID  string
	RequestID    string
	LeaseExpires time.Duration
}

// EvaluationJobClaimResult bundles the created job and its first attempt.
type EvaluationJobClaimResult struct {
	Job     EvaluationJob
	Attempt EvaluationAttempt
}

// CreateEvaluationCampaign persists a new campaign.
func (s *sqliteStore) CreateEvaluationCampaign(ctx context.Context, campaign EvaluationCampaign) (EvaluationCampaign, error) {
	if campaign.ID == "" || campaign.Name == "" || campaign.ProjectID == "" ||
		campaign.AllowedModelID == "" || campaign.CohortManifest == "" ||
		campaign.AttemptCap < 1 || campaign.AccountPoolID == "" {
		return EvaluationCampaign{}, ErrEvaluationInvalidInput
	}

	now := s.Now().Format(timestampLayout)
	campaign.CreatedAt = now
	campaign.UpdatedAt = now

	_, err := s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_campaign
		 (id, name, description, project_id, allowed_model_id, cohort_manifest, attempt_cap, account_pool_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		campaign.ID, campaign.Name, campaign.Description, campaign.ProjectID,
		campaign.AllowedModelID, campaign.CohortManifest, campaign.AttemptCap,
		campaign.AccountPoolID, now, now,
	)

	if err != nil {
		return EvaluationCampaign{}, fmt.Errorf("create evaluation campaign: %w", err)
	}

	return campaign, nil
}

// CreateEvaluationCandidate persists a new immutable candidate.
func (s *sqliteStore) CreateEvaluationCandidate(ctx context.Context, candidate EvaluationCandidate) (EvaluationCandidate, error) {
	if candidate.ID == "" || candidate.CampaignID == "" || candidate.AdapterName == "" ||
		candidate.ModelIdentity == "" || candidate.RuntimeVersion == "" ||
		candidate.PromptVersion == "" || candidate.AccountPoolID == "" || candidate.PerCandidateCap < 1 {
		return EvaluationCandidate{}, ErrEvaluationInvalidInput
	}

	now := s.Now().Format(timestampLayout)
	candidate.CreatedAt = now

	_, err := s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_candidate
		 (id, campaign_id, adapter_name, model_identity, model_revision, runtime_version,
		  reasoning_config, generation_config, prompt_version, tool_access_config,
		  source_access_config, account_pool_id, per_candidate_cap, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ID, candidate.CampaignID, candidate.AdapterName, candidate.ModelIdentity,
		candidate.ModelRevision, candidate.RuntimeVersion, candidate.ReasoningConfig,
		candidate.GenerationConfig, candidate.PromptVersion, candidate.ToolAccessConfig,
		candidate.SourceAccessConfig, candidate.AccountPoolID, candidate.PerCandidateCap,
		now,
	)

	if err != nil {
		return EvaluationCandidate{}, fmt.Errorf("create evaluation candidate: %w", err)
	}

	return candidate, nil
}

// CreateEvaluationSample persists a new sample.
func (s *sqliteStore) CreateEvaluationSample(ctx context.Context, sample EvaluationSample) (EvaluationSample, error) {
	if sample.ID == "" || sample.CampaignID == "" || sample.OriginalTaskID == "" ||
		sample.SubmittedSHA == "" || sample.PromptVersion == "" ||
		sample.ModelVersion == "" || sample.RuntimeVersion == "" {
		return EvaluationSample{}, ErrEvaluationInvalidInput
	}

	now := s.Now().Format(timestampLayout)
	sample.CreatedAt = now

	_, err := s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_sample
		 (id, campaign_id, original_task_id, original_review_round, submitted_sha,
		  snapshot_digest, source_digest, manifest_digest, prompt_version, model_version,
		  runtime_version, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sample.ID, sample.CampaignID, sample.OriginalTaskID, sample.OriginalReviewRound,
		sample.SubmittedSHA, sample.SnapshotDigest, sample.SourceDigest, sample.ManifestDigest,
		sample.PromptVersion, sample.ModelVersion, sample.RuntimeVersion,
		now,
	)

	if err != nil {
		return EvaluationSample{}, fmt.Errorf("create evaluation sample: %w", err)
	}

	return sample, nil
}

// ClaimEvaluationJob atomically claims a job, checking capacity and creating the first attempt.
func (s *sqliteStore) ClaimEvaluationJob(ctx context.Context, req EvaluationJobClaim) (EvaluationJobClaimResult, error) {
	if req.SampleID == "" || req.CandidateID == "" || req.RequestID == "" {
		return EvaluationJobClaimResult{}, ErrEvaluationInvalidInput
	}

	now := s.Now()
	expiresAt := now.Add(req.LeaseExpires)
	nowStr := now.Format(timestampLayout)
	expiresAtStr := expiresAt.Format(timestampLayout)

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check if job already exists for this sample/candidate pair
	var existingJobID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM evaluation_job WHERE sample_id = ? AND candidate_id = ?`,
		req.SampleID, req.CandidateID,
	).Scan(&existingJobID)
	if err == nil {
		return EvaluationJobClaimResult{}, ErrEvaluationDuplicateJob
	}
	if err != sql.ErrNoRows {
		return EvaluationJobClaimResult{}, fmt.Errorf("check existing job: %w", err)
	}

	// Check if request_id is already bound to a different job
	var boundJobID string
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM evaluation_job WHERE request_id = ?`,
		req.RequestID,
	).Scan(&boundJobID)
	if err == nil {
		// Request ID is already bound; fetch and return the existing attempt
		var jobID, jobSampleID, jobCandidateID, jobCreatedAt string
		err = tx.QueryRowContext(ctx,
			`SELECT id, sample_id, candidate_id, created_at FROM evaluation_job WHERE request_id = ?`,
			req.RequestID,
		).Scan(&jobID, &jobSampleID, &jobCandidateID, &jobCreatedAt)
		if err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch existing job: %w", err)
		}

		job := EvaluationJob{
			ID:          jobID,
			SampleID:    jobSampleID,
			CandidateID: jobCandidateID,
			RequestID:   req.RequestID,
			CreatedAt:   jobCreatedAt,
		}

		// Fetch current attempt
		var attemptID string
		err = tx.QueryRowContext(ctx,
			`SELECT current_attempt_id FROM evaluation_job WHERE id = ?`,
			jobID,
		).Scan(&attemptID)
		if err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch current attempt id: %w", err)
		}
		job.CurrentAttemptID = attemptID

		attempt, err := s.fetchEvaluationAttempt(ctx, tx, attemptID)
		if err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch current attempt: %w", err)
		}

		if err = tx.Commit(); err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("commit transaction: %w", err)
		}

		return EvaluationJobClaimResult{Job: job, Attempt: attempt}, nil
	}
	if err != sql.ErrNoRows {
		return EvaluationJobClaimResult{}, fmt.Errorf("check request binding: %w", err)
	}

	// Check candidate capacity: per-candidate attempts
	var candidateAttempts int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_attempt ea
		 JOIN evaluation_job ej ON ea.job_id = ej.id
		 WHERE ej.candidate_id = ? AND ea.state IN ('active', 'finalized')`,
		req.CandidateID,
	).Scan(&candidateAttempts)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("check candidate capacity: %w", err)
	}

	// Fetch candidate cap
	var candidateCap int
	err = tx.QueryRowContext(ctx,
		`SELECT per_candidate_cap FROM evaluation_candidate WHERE id = ?`,
		req.CandidateID,
	).Scan(&candidateCap)
	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationJobClaimResult{}, ErrEvaluationInvalidInput
		}
		return EvaluationJobClaimResult{}, fmt.Errorf("fetch candidate cap: %w", err)
	}

	if candidateAttempts >= candidateCap {
		return EvaluationJobClaimResult{}, ErrEvaluationCapacityExhausted
	}

	// Check campaign capacity: overall attempts
	var campaignID string
	err = tx.QueryRowContext(ctx,
		`SELECT campaign_id FROM evaluation_candidate WHERE id = ?`,
		req.CandidateID,
	).Scan(&campaignID)
	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationJobClaimResult{}, ErrEvaluationInvalidInput
		}
		return EvaluationJobClaimResult{}, fmt.Errorf("fetch campaign id: %w", err)
	}

	var campaignAttempts int
	var attemptCap int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*), ec.attempt_cap FROM evaluation_attempt ea
		 JOIN evaluation_job ej ON ea.job_id = ej.id
		 JOIN evaluation_candidate ecd ON ej.candidate_id = ecd.id
		 JOIN evaluation_campaign ec ON ecd.campaign_id = ec.id
		 WHERE ecd.campaign_id = ? AND ea.state IN ('active', 'finalized')
		 GROUP BY ec.id`,
		campaignID,
	).Scan(&campaignAttempts, &attemptCap)
	if err != sql.ErrNoRows && err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("check campaign capacity: %w", err)
	}

	if err == nil && campaignAttempts >= attemptCap {
		return EvaluationJobClaimResult{}, ErrEvaluationCapacityExhausted
	}

	// Create the job
	jobID := GenerateID()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO evaluation_job (id, sample_id, candidate_id, request_id, current_attempt_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		jobID, req.SampleID, req.CandidateID, req.RequestID, "", nowStr,
	)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("insert job: %w", err)
	}

	job := EvaluationJob{
		ID:          jobID,
		SampleID:    req.SampleID,
		CandidateID: req.CandidateID,
		RequestID:   req.RequestID,
		CreatedAt:   nowStr,
	}

	// Create the first attempt
	attemptID := GenerateID()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO evaluation_attempt
		 (id, job_id, previous_attempt_id, sequence_number, state, started_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		attemptID, jobID, nil, 1, EvalAttemptActive, nowStr, expiresAtStr,
	)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("insert attempt: %w", err)
	}

	attempt := EvaluationAttempt{
		ID:             attemptID,
		JobID:          jobID,
		SequenceNumber: 1,
		State:          EvalAttemptActive,
		StartedAt:      nowStr,
		ExpiresAt:      expiresAtStr,
	}

	// Update job's current_attempt_id
	_, err = tx.ExecContext(ctx,
		`UPDATE evaluation_job SET current_attempt_id = ? WHERE id = ?`,
		attemptID, jobID,
	)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("update job attempt: %w", err)
	}

	job.CurrentAttemptID = attemptID

	if err = tx.Commit(); err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("commit transaction: %w", err)
	}

	return EvaluationJobClaimResult{Job: job, Attempt: attempt}, nil
}

// RenewEvaluationAttempt extends the lease on an active attempt.
func (s *sqliteStore) RenewEvaluationAttempt(ctx context.Context, attemptID string, expiresAt time.Time) error {
	expiresAtStr := expiresAt.Format(timestampLayout)
	result, err := s.conn.ExecContext(ctx,
		`UPDATE evaluation_attempt SET expires_at = ? WHERE id = ? AND state = ?`,
		expiresAtStr, attemptID, EvalAttemptActive,
	)
	if err != nil {
		return fmt.Errorf("renew attempt: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		// Check if the attempt exists and is not active
		attempt, err := s.fetchEvaluationAttempt(ctx, s.conn, attemptID)
		if err != nil {
			return err
		}
		if attempt.State != EvalAttemptActive {
			if attempt.State == EvalAttemptFinalized {
				return ErrEvaluationAttemptFinalized
			}
			return ErrEvaluationAttemptExpired
		}
		return ErrEvaluationAttemptNotFound
	}

	return nil
}

// FinalizeEvaluationAttempt marks an attempt as finalized with result metadata.
func (s *sqliteStore) FinalizeEvaluationAttempt(ctx context.Context, attemptID, fenceAttemptID string, exitClass string, status *evaluation.Status, errorClass *evaluation.ErrorClass, errorMsg *string, durationMs, usageTokens *int) error {
	if attemptID != fenceAttemptID {
		return ErrEvaluationFenceMismatch
	}

	now := s.Now().Format(timestampLayout)

	statusStr := ""
	if status != nil {
		statusStr = string(*status)
	}

	errorClassStr := ""
	if errorClass != nil {
		errorClassStr = string(*errorClass)
	}

	result, err := s.conn.ExecContext(ctx,
		`UPDATE evaluation_attempt
		 SET state = ?, ended_at = ?, exit_class = ?, status = ?, error_class = ?, error_message = ?, duration_ms = ?, usage_tokens = ?
		 WHERE id = ? AND state = ?`,
		EvalAttemptFinalized, now, exitClass, statusStr, errorClassStr, errorMsg, durationMs, usageTokens,
		attemptID, EvalAttemptActive,
	)
	if err != nil {
		return fmt.Errorf("finalize attempt: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		// Check if the attempt exists
		attempt, err := s.fetchEvaluationAttempt(ctx, s.conn, attemptID)
		if err != nil {
			return err
		}
		if attempt.State == EvalAttemptFinalized {
			return ErrEvaluationAttemptFinalized
		}
		if attempt.State == EvalAttemptExpired {
			return ErrEvaluationAttemptExpired
		}
		return ErrEvaluationAttemptNotFound
	}

	return nil
}

// StoreEvaluationFinding persists a finding from a completed evaluation attempt.
func (s *sqliteStore) StoreEvaluationFinding(ctx context.Context, attemptID string, sequenceNumber int, severity evaluation.Severity, file string, line int, summary, context *string) (string, error) {
	if attemptID == "" || severity == "" || file == "" || line < 0 || summary == nil {
		return "", ErrEvaluationInvalidInput
	}

	findingID := GenerateID()
	_, err := s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_finding (id, attempt_id, sequence_number, severity, file, line, summary, context)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		findingID, attemptID, sequenceNumber, string(severity), file, line, summary, context,
	)
	if err != nil {
		return "", fmt.Errorf("store finding: %w", err)
	}

	return findingID, nil
}

// fetchEvaluationAttempt is a helper to fetch an attempt by ID from either connection or transaction.
func (s *sqliteStore) fetchEvaluationAttempt(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}, attemptID string) (EvaluationAttempt, error) {
	var startedAtStr, expiresAtStr string
	var endedAtStr *string
	var exitClass, status, errorClass, errorMsg *string
	var durationMs, usageTokens *int

	attempt := EvaluationAttempt{}

	err := querier.QueryRowContext(ctx,
		`SELECT id, job_id, previous_attempt_id, sequence_number, state, started_at, expires_at, ended_at, exit_class, status, error_class, error_message, duration_ms, usage_tokens
		 FROM evaluation_attempt WHERE id = ?`,
		attemptID,
	).Scan(&attempt.ID, &attempt.JobID, &attempt.PreviousAttemptID, &attempt.SequenceNumber, &attempt.State,
		&startedAtStr, &expiresAtStr, &endedAtStr, &exitClass, &status, &errorClass, &errorMsg, &durationMs, &usageTokens)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationAttempt{}, ErrEvaluationAttemptNotFound
		}
		return EvaluationAttempt{}, fmt.Errorf("fetch attempt: %w", err)
	}

	attempt.StartedAt = startedAtStr
	attempt.ExpiresAt = expiresAtStr
	attempt.EndedAt = endedAtStr
	attempt.ExitClass = exitClass
	attempt.Status = status
	attempt.ErrorClass = errorClass
	attempt.ErrorMessage = errorMsg
	attempt.DurationMs = durationMs
	attempt.UsageTokens = usageTokens

	return attempt, nil
}

// GetEvaluationJob fetches a job by ID.
func (s *sqliteStore) GetEvaluationJob(ctx context.Context, jobID string) (EvaluationJob, error) {
	var sampleID, candidateID, requestID, currentAttemptID, createdAtStr string

	err := s.conn.QueryRowContext(ctx,
		`SELECT id, sample_id, candidate_id, request_id, current_attempt_id, created_at FROM evaluation_job WHERE id = ?`,
		jobID,
	).Scan(&jobID, &sampleID, &candidateID, &requestID, &currentAttemptID, &createdAtStr)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationJob{}, ErrEvaluationJobNotFound
		}
		return EvaluationJob{}, fmt.Errorf("get job: %w", err)
	}

	return EvaluationJob{
		ID:               jobID,
		SampleID:         sampleID,
		CandidateID:      candidateID,
		RequestID:        requestID,
		CurrentAttemptID: currentAttemptID,
		CreatedAt:        createdAtStr,
	}, nil
}

// GetEvaluationAttempt fetches an attempt by ID.
func (s *sqliteStore) GetEvaluationAttempt(ctx context.Context, attemptID string) (EvaluationAttempt, error) {
	return s.fetchEvaluationAttempt(ctx, s.conn, attemptID)
}

// GetEvaluationSample fetches a sample by ID.
func (s *sqliteStore) GetEvaluationSample(ctx context.Context, sampleID string) (EvaluationSample, error) {
	sample := EvaluationSample{}

	err := s.conn.QueryRowContext(ctx,
		`SELECT id, campaign_id, original_task_id, original_review_round, submitted_sha,
		        snapshot_digest, source_digest, manifest_digest, prompt_version, model_version,
		        runtime_version, created_at FROM evaluation_sample WHERE id = ?`,
		sampleID,
	).Scan(&sample.ID, &sample.CampaignID, &sample.OriginalTaskID, &sample.OriginalReviewRound,
		&sample.SubmittedSHA, &sample.SnapshotDigest, &sample.SourceDigest, &sample.ManifestDigest,
		&sample.PromptVersion, &sample.ModelVersion, &sample.RuntimeVersion, &sample.CreatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationSample{}, ErrEvaluationInvalidInput
		}
		return EvaluationSample{}, fmt.Errorf("get sample: %w", err)
	}

	return sample, nil
}

// GetEvaluationCandidate fetches a candidate by ID.
func (s *sqliteStore) GetEvaluationCandidate(ctx context.Context, candidateID string) (EvaluationCandidate, error) {
	candidate := EvaluationCandidate{}

	err := s.conn.QueryRowContext(ctx,
		`SELECT id, campaign_id, adapter_name, model_identity, model_revision, runtime_version,
		        reasoning_config, generation_config, prompt_version, tool_access_config,
		        source_access_config, account_pool_id, per_candidate_cap, created_at
		 FROM evaluation_candidate WHERE id = ?`,
		candidateID,
	).Scan(&candidate.ID, &candidate.CampaignID, &candidate.AdapterName, &candidate.ModelIdentity,
		&candidate.ModelRevision, &candidate.RuntimeVersion, &candidate.ReasoningConfig,
		&candidate.GenerationConfig, &candidate.PromptVersion, &candidate.ToolAccessConfig,
		&candidate.SourceAccessConfig, &candidate.AccountPoolID, &candidate.PerCandidateCap,
		&candidate.CreatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationCandidate{}, ErrEvaluationCampaignNotFound
		}
		return EvaluationCandidate{}, fmt.Errorf("get candidate: %w", err)
	}

	return candidate, nil
}

// GetEvaluationCampaign fetches a campaign by ID.
func (s *sqliteStore) GetEvaluationCampaign(ctx context.Context, campaignID string) (EvaluationCampaign, error) {
	campaign := EvaluationCampaign{}

	err := s.conn.QueryRowContext(ctx,
		`SELECT id, name, description, project_id, allowed_model_id, cohort_manifest, attempt_cap, account_pool_id, created_at, updated_at
		 FROM evaluation_campaign WHERE id = ?`,
		campaignID,
	).Scan(&campaign.ID, &campaign.Name, &campaign.Description, &campaign.ProjectID,
		&campaign.AllowedModelID, &campaign.CohortManifest, &campaign.AttemptCap,
		&campaign.AccountPoolID, &campaign.CreatedAt, &campaign.UpdatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationCampaign{}, ErrEvaluationCampaignNotFound
		}
		return EvaluationCampaign{}, fmt.Errorf("get campaign: %w", err)
	}

	return campaign, nil
}
