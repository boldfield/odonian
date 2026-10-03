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
	ErrEvaluationSampleNotFound    = errors.New("evaluation sample not found")
	ErrEvaluationCandidateNotFound = errors.New("evaluation candidate not found")
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
	EvalExitCompleted           = "completed"
	EvalExitFailed              = "failed"
	EvalExitCancelled           = "cancelled"
	EvalExitUnknown             = "unknown"
	EvalExitLeaseExpired        = "lease_expired"
	EvalExitTimeout             = "timeout"
	EvalExitUnavailableSnapshot = "unavailable_snapshot"
	EvalExitUnavailableSource   = "unavailable_source"
	EvalExitInvalidOutput       = "invalid_output"
	EvalExitIncompleteOutput    = "incomplete_output"
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
	ID                    string
	CampaignID            string
	AdapterName           string
	ModelIdentity         string
	ModelRevision         *string
	RuntimeVersion        string
	ReasoningConfig       *string
	GenerationConfig      *string
	PromptVersion         string
	ToolAccessConfig      *string
	SourceAccessConfig    *string
	AccountPoolID         string
	PerCandidateCap       int
	CandidateConfigDigest string
	CreatedAt             string
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
	CurrentAttemptID string
	CreatedAt        string
}

// EvaluationAttempt represents one attempt at evaluating a job.
type EvaluationAttempt struct {
	ID                string
	JobID             string
	RequestID         string
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
	// RetryHint is the RetryAfter returned for a concurrency denial; zero means
	// policy.DefaultConcurrencyRetry.
	RetryHint time.Duration
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

	now := s.Now().UTC().Format(timestampLayout)
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
		candidate.PromptVersion == "" || candidate.AccountPoolID == "" || candidate.PerCandidateCap < 1 ||
		candidate.CandidateConfigDigest == "" {
		return EvaluationCandidate{}, ErrEvaluationInvalidInput
	}

	now := s.Now().UTC().Format(timestampLayout)
	candidate.CreatedAt = now

	_, err := s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_candidate
		 (id, campaign_id, adapter_name, model_identity, model_revision, runtime_version,
		  reasoning_config, generation_config, prompt_version, tool_access_config,
		  source_access_config, account_pool_id, per_candidate_cap, candidate_config_digest, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ID, candidate.CampaignID, candidate.AdapterName, candidate.ModelIdentity,
		candidate.ModelRevision, candidate.RuntimeVersion, candidate.ReasoningConfig,
		candidate.GenerationConfig, candidate.PromptVersion, candidate.ToolAccessConfig,
		candidate.SourceAccessConfig, candidate.AccountPoolID, candidate.PerCandidateCap,
		candidate.CandidateConfigDigest, now,
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

	now := s.Now().UTC().Format(timestampLayout)
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

// ExpireEvaluationAttempts marks all overdue active attempts as expired.
func (s *sqliteStore) ExpireEvaluationAttempts(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var overdue []EvaluationAttempt
	rows, err := tx.QueryContext(ctx,
		`SELECT id, job_id, request_id, previous_attempt_id, sequence_number, state, started_at, expires_at, ended_at, exit_class, status, error_class, error_message, duration_ms, usage_tokens
		 FROM evaluation_attempt WHERE state = ? AND expires_at <= ?`,
		EvalAttemptActive, now.UTC().Format(timestampLayout))
	if err != nil {
		return 0, fmt.Errorf("find overdue attempts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		a := EvaluationAttempt{}
		var reqID string
		var endedAtStr *string
		var exitClass, status, errorClass, errorMsg *string
		var durationMs, usageTokens *int
		if err := rows.Scan(&a.ID, &a.JobID, &reqID, &a.PreviousAttemptID, &a.SequenceNumber, &a.State,
			&a.StartedAt, &a.ExpiresAt, &endedAtStr, &exitClass, &status, &errorClass, &errorMsg, &durationMs, &usageTokens); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan attempt: %w", err)
		}
		a.RequestID = reqID
		a.EndedAt = endedAtStr
		a.ExitClass = exitClass
		a.Status = status
		a.ErrorClass = errorClass
		a.ErrorMessage = errorMsg
		a.DurationMs = durationMs
		a.UsageTokens = usageTokens
		overdue = append(overdue, a)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	count := 0
	for _, a := range overdue {
		started, err := time.Parse(timestampLayout, a.StartedAt)
		if err != nil {
			return 0, fmt.Errorf("parse started_at: %w", err)
		}
		expires, err := time.Parse(timestampLayout, a.ExpiresAt)
		if err != nil {
			return 0, fmt.Errorf("parse expires_at: %w", err)
		}
		dur := expires.Sub(started).Milliseconds()
		if dur < 0 {
			dur = 0
		}

		_, err = tx.ExecContext(ctx,
			`UPDATE evaluation_attempt SET state = ?, ended_at = ?, exit_class = ?, duration_ms = ?
			 WHERE id = ? AND state = ?`,
			EvalAttemptExpired, expires.Format(timestampLayout), EvalExitLeaseExpired, dur,
			a.ID, EvalAttemptActive)
		if err != nil {
			return 0, fmt.Errorf("expire attempt: %w", err)
		}
		count++
	}

	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}

	return count, nil
}

// ClaimEvaluationJob atomically claims a job, checking capacity and creating a new attempt for retries.
// For a given request_id, it's idempotent: re-requesting the same request_id returns the same attempt.
// For a fresh request_id on the same sample/candidate, it creates a new attempt if capacity allows.
func (s *sqliteStore) ClaimEvaluationJob(ctx context.Context, req EvaluationJobClaim) (EvaluationJobClaimResult, error) {
	if req.SampleID == "" || req.CandidateID == "" || req.RequestID == "" {
		return EvaluationJobClaimResult{}, ErrEvaluationInvalidInput
	}

	now := s.Now().UTC()
	expiresAt := now.Add(req.LeaseExpires)
	nowStr := now.UTC().Format(timestampLayout)
	expiresAtStr := expiresAt.UTC().Format(timestampLayout)

	// First, expire any overdue attempts
	_, err := s.ExpireEvaluationAttempts(ctx, now)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("expire overdue attempts: %w", err)
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Check if request_id is already bound (idempotency check first)
	var boundAttemptID, boundJobID string
	err = tx.QueryRowContext(ctx,
		`SELECT id, job_id FROM evaluation_attempt WHERE request_id = ?`,
		req.RequestID,
	).Scan(&boundAttemptID, &boundJobID)
	if err == nil {
		// Request ID is already bound; verify it's for the same sample/candidate and return the existing attempt
		var sampleID, candidateID string
		err = tx.QueryRowContext(ctx,
			`SELECT ej.sample_id, ej.candidate_id FROM evaluation_job ej WHERE id = ?`,
			boundJobID,
		).Scan(&sampleID, &candidateID)
		if err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch job: %w", err)
		}

		if sampleID != req.SampleID || candidateID != req.CandidateID {
			return EvaluationJobClaimResult{}, ErrEvaluationInvalidInput
		}

		// Fetch the job and attempt
		job, err := s.queryEvaluationJob(ctx, tx, boundJobID)
		if err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch job: %w", err)
		}

		attempt, err := s.fetchEvaluationAttempt(ctx, tx, boundAttemptID)
		if err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch attempt: %w", err)
		}

		if err = tx.Commit(); err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("commit transaction: %w", err)
		}

		return EvaluationJobClaimResult{Job: job, Attempt: attempt}, nil
	}
	if err != sql.ErrNoRows {
		return EvaluationJobClaimResult{}, fmt.Errorf("check request binding: %w", err)
	}

	// Verify sample exists and get its campaign_id
	var sampleCampaignID string
	err = tx.QueryRowContext(ctx,
		`SELECT campaign_id FROM evaluation_sample WHERE id = ?`,
		req.SampleID,
	).Scan(&sampleCampaignID)
	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationJobClaimResult{}, ErrEvaluationSampleNotFound
		}
		return EvaluationJobClaimResult{}, fmt.Errorf("fetch sample: %w", err)
	}

	// Check if job already exists for this sample/candidate pair (fresh request_id, retry case)
	var jobID string
	var campaignID string
	err = tx.QueryRowContext(ctx,
		`SELECT ej.id, ecd.campaign_id FROM evaluation_job ej
		 JOIN evaluation_candidate ecd ON ej.candidate_id = ecd.id
		 WHERE ej.sample_id = ? AND ej.candidate_id = ?`,
		req.SampleID, req.CandidateID,
	).Scan(&jobID, &campaignID)
	if err == nil {
		// Job exists; verify campaign_id match
		if sampleCampaignID != campaignID {
			return EvaluationJobClaimResult{}, ErrEvaluationInvalidInput
		}

		// Attempt to create a new retry attempt
		// Check if the previous attempt is finalized/expired before allowing a new attempt
		var prevAttemptState, prevAttemptID, prevExpiresAtStr string
		err = tx.QueryRowContext(ctx,
			`SELECT state, id, expires_at FROM evaluation_attempt WHERE job_id = ? ORDER BY sequence_number DESC LIMIT 1`,
			jobID,
		).Scan(&prevAttemptState, &prevAttemptID, &prevExpiresAtStr)
		if err != nil && err != sql.ErrNoRows {
			return EvaluationJobClaimResult{}, fmt.Errorf("check previous attempt: %w", err)
		}

		// If there's an active attempt that hasn't expired, reject the retry
		if err == nil && prevAttemptState == EvalAttemptActive {
			prevExpires, err := time.Parse(timestampLayout, prevExpiresAtStr)
			if err != nil {
				return EvaluationJobClaimResult{}, fmt.Errorf("parse expires_at: %w", err)
			}
			if now.Before(prevExpires) {
				return EvaluationJobClaimResult{}, ErrEvaluationAttemptLive
			}
		}

		// Proceed to create new attempt
	} else if err != sql.ErrNoRows {
		return EvaluationJobClaimResult{}, fmt.Errorf("check existing job: %w", err)
	} else {
		// New job case: create the job
		// Verify campaign_id match
		var candidateCampaignID string
		err = tx.QueryRowContext(ctx,
			`SELECT campaign_id FROM evaluation_candidate WHERE id = ?`,
			req.CandidateID,
		).Scan(&candidateCampaignID)
		if err != nil {
			if err == sql.ErrNoRows {
				return EvaluationJobClaimResult{}, ErrEvaluationCandidateNotFound
			}
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch candidate: %w", err)
		}

		if sampleCampaignID != candidateCampaignID {
			return EvaluationJobClaimResult{}, ErrEvaluationInvalidInput
		}

		jobID = GenerateID()
		campaignID = candidateCampaignID

		_, err = tx.ExecContext(ctx,
			`INSERT INTO evaluation_job (id, sample_id, candidate_id, current_attempt_id, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			jobID, req.SampleID, req.CandidateID, "", nowStr,
		)
		if err != nil {
			return EvaluationJobClaimResult{}, fmt.Errorf("insert job: %w", err)
		}
	}

	// Check candidate capacity: per-candidate attempts (count all, including expired)
	var candidateAttempts int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_attempt ea
		 JOIN evaluation_job ej ON ea.job_id = ej.id
		 WHERE ej.candidate_id = ?`,
		req.CandidateID,
	).Scan(&candidateAttempts)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("check candidate capacity: %w", err)
	}

	// Fetch candidate cap and the pool this candidate draws from
	var candidateCap int
	var poolID string
	err = tx.QueryRowContext(ctx,
		`SELECT per_candidate_cap, account_pool_id FROM evaluation_candidate WHERE id = ?`,
		req.CandidateID,
	).Scan(&candidateCap, &poolID)
	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationJobClaimResult{}, ErrEvaluationCandidateNotFound
		}
		return EvaluationJobClaimResult{}, fmt.Errorf("fetch candidate cap: %w", err)
	}

	if candidateAttempts >= candidateCap {
		return EvaluationJobClaimResult{}, ErrEvaluationCapacityExhausted
	}

	// Check campaign capacity: overall attempts (count all, including expired)
	var campaignAttempts int
	var attemptCap int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*), ec.attempt_cap FROM evaluation_attempt ea
		 JOIN evaluation_job ej ON ea.job_id = ej.id
		 JOIN evaluation_candidate ecd ON ej.candidate_id = ecd.id
		 JOIN evaluation_campaign ec ON ecd.campaign_id = ec.id
		 WHERE ecd.campaign_id = ?
		 GROUP BY ec.id`,
		campaignID,
	).Scan(&campaignAttempts, &attemptCap)
	if err != sql.ErrNoRows && err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("check campaign capacity: %w", err)
	}

	if err == nil && campaignAttempts >= attemptCap {
		return EvaluationJobClaimResult{}, ErrEvaluationCapacityExhausted
	}

	// Debit the candidate's independent pool. A missing pool is an error and a
	// denial rolls back everything, so a refused start consumes no capacity.
	if err = admitEvaluationStart(ctx, tx, now, poolID, req.RetryHint); err != nil {
		return EvaluationJobClaimResult{}, err
	}

	// Fetch the job and get the next sequence number
	var maxSeqNum int
	err = tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(sequence_number), 0) FROM evaluation_attempt WHERE job_id = ?`,
		jobID,
	).Scan(&maxSeqNum)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("fetch max sequence: %w", err)
	}

	nextSeqNum := maxSeqNum + 1
	var prevAttemptID *string
	if maxSeqNum > 0 {
		var lastID string
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM evaluation_attempt WHERE job_id = ? AND sequence_number = ?`,
			jobID, maxSeqNum,
		).Scan(&lastID)
		if err != nil && err != sql.ErrNoRows {
			return EvaluationJobClaimResult{}, fmt.Errorf("fetch previous attempt id: %w", err)
		}
		if err == nil {
			prevAttemptID = &lastID
		}
	}

	// Create the new attempt
	attemptID := GenerateID()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO evaluation_attempt
		 (id, job_id, request_id, account_pool_id, previous_attempt_id, sequence_number, state, started_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attemptID, jobID, req.RequestID, poolID, prevAttemptID, nextSeqNum, EvalAttemptActive, nowStr, expiresAtStr,
	)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("insert attempt: %w", err)
	}

	attempt := EvaluationAttempt{
		ID:                attemptID,
		JobID:             jobID,
		RequestID:         req.RequestID,
		PreviousAttemptID: prevAttemptID,
		SequenceNumber:    nextSeqNum,
		State:             EvalAttemptActive,
		StartedAt:         nowStr,
		ExpiresAt:         expiresAtStr,
	}

	// Update job's current_attempt_id
	_, err = tx.ExecContext(ctx,
		`UPDATE evaluation_job SET current_attempt_id = ? WHERE id = ?`,
		attemptID, jobID,
	)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("update job attempt: %w", err)
	}

	// Fetch the created job for the response
	job, err := s.queryEvaluationJob(ctx, tx, jobID)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("fetch job: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("commit transaction: %w", err)
	}

	return EvaluationJobClaimResult{Job: job, Attempt: attempt}, nil
}

// RenewEvaluationAttempt extends the lease on an active attempt.
func (s *sqliteStore) RenewEvaluationAttempt(ctx context.Context, attemptID string, expiresAt time.Time) error {
	now := s.Now().UTC()

	// Check if attempt exists and is still active with a valid lease
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

	// Parse current expires_at
	currentExpires, err := time.Parse(timestampLayout, attempt.ExpiresAt)
	if err != nil {
		return fmt.Errorf("parse expires_at: %w", err)
	}

	// Reject if lease has already passed
	if now.After(currentExpires) {
		return ErrEvaluationAttemptExpired
	}

	expiresAtStr := expiresAt.UTC().Format(timestampLayout)
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
		return ErrEvaluationAttemptNotFound
	}

	return nil
}

// FinalizeEvaluationAttempt marks an attempt as finalized with result metadata.
// It enforces fencing: the attempt must be the job's current_attempt_id and its lease must not have expired.
func (s *sqliteStore) FinalizeEvaluationAttempt(ctx context.Context, attemptID, fenceAttemptID string, exitClass string, status *evaluation.Status, errorClass *evaluation.ErrorClass, errorMsg *string, durationMs, usageTokens *int) error {
	if attemptID != fenceAttemptID {
		return ErrEvaluationFenceMismatch
	}

	// Validate exitClass
	switch exitClass {
	case EvalExitCompleted, EvalExitFailed, EvalExitCancelled, EvalExitUnknown, EvalExitLeaseExpired,
		EvalExitTimeout, EvalExitUnavailableSnapshot, EvalExitUnavailableSource,
		EvalExitInvalidOutput, EvalExitIncompleteOutput:
		// Valid
	default:
		return fmt.Errorf("invalid exit_class: %s", exitClass)
	}

	// Validate status if provided
	if status != nil {
		switch *status {
		case evaluation.StatusCompleted, evaluation.StatusIncomplete, evaluation.StatusUnsupported, evaluation.StatusInterrupted, evaluation.StatusFailed:
			// Valid
		default:
			return fmt.Errorf("invalid status: %s", *status)
		}
	}

	// Validate error_class if provided
	if errorClass != nil {
		switch *errorClass {
		case evaluation.ErrClassCapabilityMissing, evaluation.ErrClassOutputTruncated, evaluation.ErrClassSourceUnavailable,
			evaluation.ErrClassBudgetExhausted, evaluation.ErrClassInterrupted, evaluation.ErrClassTimeout,
			evaluation.ErrClassRuntimeError, evaluation.ErrClassOutputMalformed, evaluation.ErrClassOutputMissing,
			evaluation.ErrClassAuthMissing, evaluation.ErrClassLaunchError:
			// Valid
		default:
			return fmt.Errorf("invalid error_class: %s", *errorClass)
		}
	}

	now := s.Now().UTC()
	nowStr := now.UTC().Format(timestampLayout)

	var statusVal interface{} = nil
	if status != nil {
		statusVal = string(*status)
	}

	var errorClassVal interface{} = nil
	if errorClass != nil {
		errorClassVal = string(*errorClass)
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Fetch the attempt to check fencing conditions
	var jobID, attemptState, expiresAtStr string
	err = tx.QueryRowContext(ctx,
		`SELECT job_id, state, expires_at FROM evaluation_attempt WHERE id = ?`,
		attemptID,
	).Scan(&jobID, &attemptState, &expiresAtStr)
	if err != nil {
		if err == sql.ErrNoRows {
			return ErrEvaluationAttemptNotFound
		}
		return fmt.Errorf("fetch attempt: %w", err)
	}

	// Check if attempt is already in a terminal state
	if attemptState != EvalAttemptActive {
		if attemptState == EvalAttemptFinalized {
			return ErrEvaluationAttemptFinalized
		}
		return ErrEvaluationAttemptExpired
	}

	// Check if this attempt is still the job's current attempt
	var jobCurrentAttemptID string
	err = tx.QueryRowContext(ctx,
		`SELECT current_attempt_id FROM evaluation_job WHERE id = ?`,
		jobID,
	).Scan(&jobCurrentAttemptID)
	if err != nil {
		return fmt.Errorf("fetch job: %w", err)
	}
	if jobCurrentAttemptID != attemptID {
		return ErrEvaluationFenceMismatch
	}

	// Check if the lease has expired
	expiresAt, err := time.Parse(timestampLayout, expiresAtStr)
	if err != nil {
		return fmt.Errorf("parse expires_at: %w", err)
	}
	if now.After(expiresAt) {
		// Lease has expired; mark the attempt as expired and return error
		_, err := tx.ExecContext(ctx,
			`UPDATE evaluation_attempt SET state = ? WHERE id = ?`,
			EvalAttemptExpired, attemptID,
		)
		if err != nil {
			return fmt.Errorf("mark attempt expired: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}
		return ErrEvaluationAttemptExpired
	}

	// Now safe to finalize
	result, err := tx.ExecContext(ctx,
		`UPDATE evaluation_attempt
		 SET state = ?, ended_at = ?, exit_class = ?, status = ?, error_class = ?, error_message = ?, duration_ms = ?, usage_tokens = ?
		 WHERE id = ?`,
		EvalAttemptFinalized, nowStr, exitClass, statusVal, errorClassVal, errorMsg, durationMs, usageTokens,
		attemptID,
	)
	if err != nil {
		return fmt.Errorf("finalize attempt: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rows == 0 {
		return ErrEvaluationAttemptNotFound
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// StoreEvaluationFinding persists a finding from a completed evaluation attempt.
// Findings can only be stored on finalized attempts with completed status and are immutable.
func (s *sqliteStore) StoreEvaluationFinding(ctx context.Context, attemptID string, sequenceNumber int, severity evaluation.Severity, file string, line int, summary, context *string) (string, error) {
	if attemptID == "" || severity == "" || file == "" || line < 0 || summary == nil {
		return "", ErrEvaluationInvalidInput
	}

	// Validate severity using evaluation package constants
	switch severity {
	case evaluation.SeverityMaterial, evaluation.SeverityMinor, evaluation.SeverityNote:
		// Valid severity
	default:
		return "", fmt.Errorf("invalid severity: %s", severity)
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Verify that the attempt exists and is finalized with completed exit_class
	var attemptState, exitClass string
	err = tx.QueryRowContext(ctx,
		`SELECT state, COALESCE(exit_class, '')
		 FROM evaluation_attempt WHERE id = ?`,
		attemptID,
	).Scan(&attemptState, &exitClass)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", ErrEvaluationAttemptNotFound
		}
		return "", fmt.Errorf("fetch attempt: %w", err)
	}

	if attemptState != EvalAttemptFinalized {
		return "", fmt.Errorf("findings can only be stored on finalized attempts, got state: %s", attemptState)
	}

	// Findings can only be stored if the exit_class is 'completed'
	if exitClass != EvalExitCompleted {
		return "", fmt.Errorf("findings can only be stored on completed attempts, got exit_class: %s", exitClass)
	}

	findingID := GenerateID()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO evaluation_finding (id, attempt_id, sequence_number, severity, file, line, summary, context)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		findingID, attemptID, sequenceNumber, string(severity), file, line, summary, context,
	)
	if err != nil {
		return "", fmt.Errorf("store finding: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return "", fmt.Errorf("commit transaction: %w", err)
	}

	return findingID, nil
}

// queryEvaluationJob is a helper to fetch a job by ID from either connection or transaction.
func (s *sqliteStore) queryEvaluationJob(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}, jobID string) (EvaluationJob, error) {
	var sampleID, candidateID, currentAttemptID, createdAt string

	err := querier.QueryRowContext(ctx,
		`SELECT id, sample_id, candidate_id, current_attempt_id, created_at FROM evaluation_job WHERE id = ?`,
		jobID,
	).Scan(&jobID, &sampleID, &candidateID, &currentAttemptID, &createdAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationJob{}, ErrEvaluationJobNotFound
		}
		return EvaluationJob{}, fmt.Errorf("fetch job: %w", err)
	}

	return EvaluationJob{
		ID:               jobID,
		SampleID:         sampleID,
		CandidateID:      candidateID,
		CurrentAttemptID: currentAttemptID,
		CreatedAt:        createdAt,
	}, nil
}

// fetchEvaluationAttempt is a helper to fetch an attempt by ID from either connection or transaction.
func (s *sqliteStore) fetchEvaluationAttempt(ctx context.Context, querier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}, attemptID string) (EvaluationAttempt, error) {
	var startedAtStr, expiresAtStr, requestID string
	var endedAtStr *string
	var exitClass, status, errorClass, errorMsg *string
	var durationMs, usageTokens *int

	attempt := EvaluationAttempt{}

	err := querier.QueryRowContext(ctx,
		`SELECT id, job_id, request_id, previous_attempt_id, sequence_number, state, started_at, expires_at, ended_at, exit_class, status, error_class, error_message, duration_ms, usage_tokens
		 FROM evaluation_attempt WHERE id = ?`,
		attemptID,
	).Scan(&attempt.ID, &attempt.JobID, &requestID, &attempt.PreviousAttemptID, &attempt.SequenceNumber, &attempt.State,
		&startedAtStr, &expiresAtStr, &endedAtStr, &exitClass, &status, &errorClass, &errorMsg, &durationMs, &usageTokens)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationAttempt{}, ErrEvaluationAttemptNotFound
		}
		return EvaluationAttempt{}, fmt.Errorf("fetch attempt: %w", err)
	}

	attempt.RequestID = requestID
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
	return s.queryEvaluationJob(ctx, s.conn, jobID)
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
			return EvaluationSample{}, ErrEvaluationSampleNotFound
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
		        source_access_config, account_pool_id, per_candidate_cap, candidate_config_digest, created_at
		 FROM evaluation_candidate WHERE id = ?`,
		candidateID,
	).Scan(&candidate.ID, &candidate.CampaignID, &candidate.AdapterName, &candidate.ModelIdentity,
		&candidate.ModelRevision, &candidate.RuntimeVersion, &candidate.ReasoningConfig,
		&candidate.GenerationConfig, &candidate.PromptVersion, &candidate.ToolAccessConfig,
		&candidate.SourceAccessConfig, &candidate.AccountPoolID, &candidate.PerCandidateCap,
		&candidate.CandidateConfigDigest, &candidate.CreatedAt)

	if err != nil {
		if err == sql.ErrNoRows {
			return EvaluationCandidate{}, ErrEvaluationCandidateNotFound
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
