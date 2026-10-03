package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
)

// Evaluation job lifecycle. A job is the durable evaluation of one sample
// by one candidate: it binds a sample, candidate, and the caller's request ID.
// Each start of the candidate is an attempt; the attempt ID is the fencing
// identity for renew and finalize. All helpers take the server time as an
// argument (an injectable clock).

var (
	ErrEvaluationJobNotFound           = errors.New("evaluation job not found")
	ErrEvaluationAttemptNotFound       = errors.New("evaluation attempt not found")
	ErrEvaluationCampaignNotFound      = errors.New("evaluation campaign not found")
	ErrEvaluationSampleNotFound        = errors.New("evaluation sample not found")
	ErrEvaluationCandidateNotFound     = errors.New("evaluation candidate not found")
	ErrEvaluationFenceMismatch         = errors.New("evaluation attempt identity does not match the job's current attempt")
	ErrEvaluationAttemptExpired        = errors.New("evaluation attempt lease expired")
	ErrEvaluationAttemptFinalized      = errors.New("evaluation attempt already finalized")
	ErrEvaluationAttemptLive           = errors.New("previous evaluation attempt is still live")
	ErrEvaluationCapacityExhausted     = errors.New("evaluation campaign or candidate has exhausted attempt capacity")
	ErrEvaluationCandidateCorrupt      = errors.New("stored evaluation candidate does not match its recorded digest")
	ErrEvaluationProjectNotAllowed     = errors.New("project is not allowed for this evaluation campaign")
	ErrEvaluationModelNotAllowed       = errors.New("model is not allowed for this evaluation campaign")
	ErrEvaluationCampaignPaused        = errors.New("evaluation campaign is paused")
	ErrEvaluationCampaignAlreadyPaused = errors.New("evaluation campaign is already paused")
	ErrEvaluationInvalidInput          = errors.New("invalid evaluation input")
	ErrEvaluationAlreadyExists         = errors.New("evaluation record already exists")
)

// EvaluationExitClass is the distinct terminal outcome of one attempt.
type EvaluationExitClass string

const (
	EvalExitCompleted           EvaluationExitClass = "completed"
	EvalExitFailed              EvaluationExitClass = "failed"
	EvalExitCancelled           EvaluationExitClass = "cancelled"
	EvalExitUnknown             EvaluationExitClass = "unknown"
	EvalExitLeaseExpired        EvaluationExitClass = "lease_expired"
	EvalExitTimeout             EvaluationExitClass = "timeout"
	EvalExitUnavailableSnapshot EvaluationExitClass = "unavailable_snapshot"
	EvalExitUnavailableSource   EvaluationExitClass = "unavailable_source"
	EvalExitInvalidOutput       EvaluationExitClass = "invalid_output"
	EvalExitIncompleteOutput    EvaluationExitClass = "incomplete_output"
)

func (c EvaluationExitClass) valid() bool {
	switch c {
	case EvalExitCompleted, EvalExitFailed, EvalExitCancelled, EvalExitUnknown, EvalExitLeaseExpired,
		EvalExitTimeout, EvalExitUnavailableSnapshot, EvalExitUnavailableSource,
		EvalExitInvalidOutput, EvalExitIncompleteOutput:
		return true
	}
	return false
}

// Attempt states.
const (
	EvalAttemptActive    = "active"
	EvalAttemptFinalized = "finalized"
	EvalAttemptExpired   = "expired"
)

// EvaluationCampaign represents a finite evaluation experiment. Samples may
// only come from AllowedProjectIDs and candidates may only use a model in
// AllowedModelIDs. Account pools belong to candidates.
type EvaluationCampaign struct {
	ID                string
	Name              string
	Description       *string
	AllowedProjectIDs []string
	AllowedModelIDs   []string
	CohortManifest    string
	AttemptCap        int
	CreatedAt         string
	UpdatedAt         string
	PausedAt          *string
	ResumedAt         *string
}

// EvaluationCandidate is an immutable candidate version. Its identity is the
// M1 evaluation.CandidateConfig; the store derives and verifies the digest from
// it, so a candidate can never be stored or read under a digest that is not its
// own. PerCandidateCap is this candidate's finite share of the campaign cap.
type EvaluationCandidate struct {
	ID              string
	CampaignID      string
	Config          evaluation.CandidateConfig
	PerCandidateCap int
	CreatedAt       string
}

// Digest is the M1 identity digest of the candidate version.
func (c EvaluationCandidate) Digest() string { return c.Config.Digest() }

// AccountPoolID is the evaluation pool this candidate draws every start from.
func (c EvaluationCandidate) AccountPoolID() string { return c.Config.Identity().AccountPool }

// EvaluationSample represents an original task/review bound for evaluation.
type EvaluationSample struct {
	ID                  string
	CampaignID          string
	ProjectID           string
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
	ExitClass         *EvaluationExitClass
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

// CreateEvaluationCampaign persists a new campaign and its allowed projects.
func (s *sqliteStore) CreateEvaluationCampaign(ctx context.Context, campaign EvaluationCampaign) (EvaluationCampaign, error) {
	if campaign.ID == "" || campaign.Name == "" || len(campaign.AllowedProjectIDs) == 0 ||
		len(campaign.AllowedModelIDs) == 0 || campaign.CohortManifest == "" || campaign.AttemptCap < 1 {
		return EvaluationCampaign{}, ErrEvaluationInvalidInput
	}
	seen := map[string]bool{}
	for _, p := range campaign.AllowedProjectIDs {
		if p == "" || seen[p] {
			return EvaluationCampaign{}, ErrEvaluationInvalidInput
		}
		seen[p] = true
	}

	seenModels := map[string]bool{}
	for _, m := range campaign.AllowedModelIDs {
		if m == "" || seenModels[m] {
			return EvaluationCampaign{}, ErrEvaluationInvalidInput
		}
		seenModels[m] = true
	}

	now := s.Now().UTC().Format(timestampLayout)
	campaign.CreatedAt = now
	campaign.UpdatedAt = now
	campaign.AllowedProjectIDs = append([]string(nil), campaign.AllowedProjectIDs...)
	campaign.AllowedModelIDs = append([]string(nil), campaign.AllowedModelIDs...)

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return EvaluationCampaign{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx,
		`INSERT INTO evaluation_campaign
		 (id, name, description, cohort_manifest, attempt_cap, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		campaign.ID, campaign.Name, campaign.Description,
		campaign.CohortManifest, campaign.AttemptCap, now, now,
	)
	if isUniqueViolation(err) {
		return EvaluationCampaign{}, ErrEvaluationAlreadyExists
	}
	if err != nil {
		return EvaluationCampaign{}, fmt.Errorf("create evaluation campaign: %w", err)
	}
	for _, p := range campaign.AllowedProjectIDs {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO evaluation_campaign_project (campaign_id, project_id) VALUES (?, ?)`, campaign.ID, p); err != nil {
			return EvaluationCampaign{}, fmt.Errorf("create evaluation campaign project: %w", err)
		}
	}
	for _, m := range campaign.AllowedModelIDs {
		if _, err = tx.ExecContext(ctx,
			`INSERT INTO evaluation_campaign_model (campaign_id, model_id) VALUES (?, ?)`, campaign.ID, m); err != nil {
			return EvaluationCampaign{}, fmt.Errorf("create evaluation campaign model: %w", err)
		}
	}
	if err = tx.Commit(); err != nil {
		return EvaluationCampaign{}, fmt.Errorf("commit transaction: %w", err)
	}
	return campaign, nil
}

func (s *sqliteStore) PauseEvaluationCampaign(ctx context.Context, campaignID string) error {
	now := s.Now().UTC().Format(timestampLayout)

	// First check if campaign exists and its current pause state
	var exists, alreadyPaused int
	err := s.conn.QueryRowContext(ctx,
		`SELECT 1, CASE WHEN paused_at IS NOT NULL THEN 1 ELSE 0 END FROM evaluation_campaign WHERE id = ?`,
		campaignID).Scan(&exists, &alreadyPaused)
	if err != nil {
		if err == sql.ErrNoRows {
			return ErrEvaluationCampaignNotFound
		}
		return fmt.Errorf("check campaign: %w", err)
	}

	if alreadyPaused == 1 {
		return ErrEvaluationCampaignAlreadyPaused
	}

	result, err := s.conn.ExecContext(ctx,
		`UPDATE evaluation_campaign SET paused_at = ? WHERE id = ? AND paused_at IS NULL AND resumed_at IS NULL`,
		now, campaignID)
	if err != nil {
		return fmt.Errorf("pause campaign: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check pause result: %w", err)
	}
	if rowsAffected == 0 {
		return ErrEvaluationCampaignAlreadyPaused
	}

	return nil
}

// CreateEvaluationCandidate persists a new immutable candidate version. The
// digest is taken from the M1 config, never from the caller, and the canonical
// identity is stored so the digest can be re-verified on every read. The
// campaign must exist.
func (s *sqliteStore) CreateEvaluationCandidate(ctx context.Context, candidate EvaluationCandidate) (EvaluationCandidate, error) {
	if candidate.ID == "" || candidate.CampaignID == "" || candidate.PerCandidateCap < 1 || candidate.Config.Digest() == "" {
		return EvaluationCandidate{}, ErrEvaluationInvalidInput
	}
	identity := candidate.Config.Identity()
	// Revalidate the identity so a zero or hand-built config cannot slip through.
	if _, err := evaluation.NewCandidateConfig(identity); err != nil {
		return EvaluationCandidate{}, fmt.Errorf("%w: %v", ErrEvaluationInvalidInput, err)
	}
	identityJSON, err := json.Marshal(identity)
	if err != nil {
		return EvaluationCandidate{}, fmt.Errorf("marshal candidate identity: %w", err)
	}

	var campaignExists, modelAllowed int
	if err := s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluation_campaign WHERE id = ?`, candidate.CampaignID).Scan(&campaignExists); err != nil {
		return EvaluationCandidate{}, fmt.Errorf("check campaign: %w", err)
	}
	if campaignExists == 0 {
		return EvaluationCandidate{}, ErrEvaluationCampaignNotFound
	}
	if err := s.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_campaign_model WHERE campaign_id = ? AND model_id = ?`,
		candidate.CampaignID, identity.ModelID).Scan(&modelAllowed); err != nil {
		return EvaluationCandidate{}, fmt.Errorf("check allowed model: %w", err)
	}
	if modelAllowed == 0 {
		return EvaluationCandidate{}, fmt.Errorf("%w: %q", ErrEvaluationModelNotAllowed, identity.ModelID)
	}

	candidate.CreatedAt = s.Now().UTC().Format(timestampLayout)
	_, err = s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_candidate
		 (id, campaign_id, identity_json, candidate_config_digest, account_pool_id, per_candidate_cap, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		candidate.ID, candidate.CampaignID, string(identityJSON), candidate.Config.Digest(),
		identity.AccountPool, candidate.PerCandidateCap, candidate.CreatedAt,
	)
	if isUniqueViolation(err) {
		return EvaluationCandidate{}, ErrEvaluationAlreadyExists
	}
	if err != nil {
		return EvaluationCandidate{}, fmt.Errorf("create evaluation candidate: %w", err)
	}
	return candidate, nil
}

// CreateEvaluationSample persists a new sample. Its project must be one of the
// campaign's allowed projects and, if the original task exists, must be that
// task's project.
func (s *sqliteStore) CreateEvaluationSample(ctx context.Context, sample EvaluationSample) (EvaluationSample, error) {
	if sample.ID == "" || sample.CampaignID == "" || sample.ProjectID == "" || sample.OriginalTaskID == "" ||
		sample.SubmittedSHA == "" || sample.PromptVersion == "" ||
		sample.ModelVersion == "" || sample.RuntimeVersion == "" {
		return EvaluationSample{}, ErrEvaluationInvalidInput
	}

	var campaignExists, allowed int
	if err := s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM evaluation_campaign WHERE id = ?`, sample.CampaignID).Scan(&campaignExists); err != nil {
		return EvaluationSample{}, fmt.Errorf("check campaign: %w", err)
	}
	if campaignExists == 0 {
		return EvaluationSample{}, ErrEvaluationCampaignNotFound
	}
	if err := s.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_campaign_project WHERE campaign_id = ? AND project_id = ?`,
		sample.CampaignID, sample.ProjectID).Scan(&allowed); err != nil {
		return EvaluationSample{}, fmt.Errorf("check allowed project: %w", err)
	}
	if allowed == 0 {
		return EvaluationSample{}, ErrEvaluationProjectNotAllowed
	}
	var taskProject string
	err := s.conn.QueryRowContext(ctx, `SELECT project_id FROM task WHERE id = ?`, sample.OriginalTaskID).Scan(&taskProject)
	switch {
	case err == nil && taskProject != sample.ProjectID:
		return EvaluationSample{}, fmt.Errorf("%w: original task belongs to project %s", ErrEvaluationInvalidInput, taskProject)
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return EvaluationSample{}, fmt.Errorf("check original task: %w", err)
	}

	now := s.Now().UTC().Format(timestampLayout)
	sample.CreatedAt = now

	_, err = s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_sample
		 (id, campaign_id, project_id, original_task_id, original_review_round, submitted_sha,
		  snapshot_digest, source_digest, manifest_digest, prompt_version, model_version,
		  runtime_version, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sample.ID, sample.CampaignID, sample.ProjectID, sample.OriginalTaskID, sample.OriginalReviewRound,
		sample.SubmittedSHA, sample.SnapshotDigest, sample.SourceDigest, sample.ManifestDigest,
		sample.PromptVersion, sample.ModelVersion, sample.RuntimeVersion,
		now,
	)

	if err != nil {
		return EvaluationSample{}, fmt.Errorf("create evaluation sample: %w", err)
	}

	return sample, nil
}

// leaseExpired is the one boundary rule for a lease: an attempt is overdue at
// the instant its expires_at is reached. Sweep, renew, finalize and pool
// occupancy all agree on it.
func leaseExpired(now, expiresAt time.Time) bool { return !now.Before(expiresAt) }

// expireEvaluationAttempt records an overdue active attempt as expired with the
// distinct lease_expired outcome, ending it at its lease deadline. It is the
// single path by which a lease becomes an expired attempt, whether found by the
// sweep or by a late result.
func expireEvaluationAttempt(ctx context.Context, tx *sql.Tx, attemptID, startedAtStr, expiresAtStr string) error {
	started, err := time.Parse(timestampLayout, startedAtStr)
	if err != nil {
		return fmt.Errorf("parse started_at: %w", err)
	}
	expires, err := time.Parse(timestampLayout, expiresAtStr)
	if err != nil {
		return fmt.Errorf("parse expires_at: %w", err)
	}
	dur := expires.Sub(started).Milliseconds()
	if dur < 0 {
		dur = 0
	}
	_, err = tx.ExecContext(ctx,
		`UPDATE evaluation_attempt SET state = ?, ended_at = ?, exit_class = ?, duration_ms = ?
		 WHERE id = ? AND state = ?`,
		EvalAttemptExpired, expires.Format(timestampLayout), string(EvalExitLeaseExpired), dur,
		attemptID, EvalAttemptActive)
	if err != nil {
		return fmt.Errorf("expire attempt: %w", err)
	}
	return nil
}

// ExpireEvaluationAttempts marks all overdue active attempts as expired.
func (s *sqliteStore) ExpireEvaluationAttempts(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	type overdue struct{ id, started, expires string }
	var found []overdue
	rows, err := tx.QueryContext(ctx,
		`SELECT id, started_at, expires_at FROM evaluation_attempt WHERE state = ? AND expires_at <= ?`,
		EvalAttemptActive, now.UTC().Format(timestampLayout))
	if err != nil {
		return 0, fmt.Errorf("find overdue attempts: %w", err)
	}
	for rows.Next() {
		var o overdue
		if err := rows.Scan(&o.id, &o.started, &o.expires); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan attempt: %w", err)
		}
		found = append(found, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	for _, o := range found {
		if err := expireEvaluationAttempt(ctx, tx, o.id, o.started, o.expires); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return len(found), nil
}

// ClaimEvaluationJob atomically claims a job, checking capacity and creating a new attempt for retries.
// For a given request_id, it's idempotent: re-requesting the same request_id returns the same attempt.
// For a fresh request_id on the same sample/candidate, it creates a new attempt if capacity allows.
func (s *sqliteStore) ClaimEvaluationJob(ctx context.Context, req EvaluationJobClaim) (EvaluationJobClaimResult, error) {
	if req.SampleID == "" || req.CandidateID == "" || req.RequestID == "" || req.LeaseExpires <= 0 {
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
			if !leaseExpired(now, prevExpires) {
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

	// A paused campaign admits nothing; this outranks the capacity checks so a
	// waiting runner sees the pause rather than a misleading exhaustion.
	var pausedAt, resumedAt *string
	err = tx.QueryRowContext(ctx,
		`SELECT paused_at, resumed_at FROM evaluation_campaign WHERE id = ?`,
		campaignID,
	).Scan(&pausedAt, &resumedAt)
	if err != nil {
		return EvaluationJobClaimResult{}, fmt.Errorf("check campaign pause state: %w", err)
	}
	if pausedAt != nil && resumedAt == nil {
		return EvaluationJobClaimResult{}, ErrEvaluationCampaignPaused
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
	if leaseExpired(now, currentExpires) {
		return ErrEvaluationAttemptExpired
	}

	// A renewal must move the lease forward, past now.
	if !expiresAt.After(now) || !expiresAt.After(currentExpires) {
		return ErrEvaluationInvalidInput
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

// EvaluationAttemptResult is the recorded outcome of one attempt. Findings are
// part of the result: they are validated and written atomically with it and
// never change afterwards. Nil DurationMs/UsageTokens mean unknown, not zero.
type EvaluationAttemptResult struct {
	AttemptID      string
	FenceAttemptID string
	ExitClass      EvaluationExitClass
	Status         *evaluation.Status
	ErrorClass     *evaluation.ErrorClass
	ErrorMessage   *string
	DurationMs     *int
	UsageTokens    *int
	Findings       []evaluation.Finding
}

func (r EvaluationAttemptResult) validate() error {
	if r.AttemptID == "" {
		return ErrEvaluationInvalidInput
	}
	if !r.ExitClass.valid() {
		return fmt.Errorf("%w: exit_class %q", ErrEvaluationInvalidInput, r.ExitClass)
	}
	if r.ExitClass == EvalExitLeaseExpired {
		return fmt.Errorf("%w: lease_expired is recorded by the store, not by a result", ErrEvaluationInvalidInput)
	}
	if r.Status != nil {
		switch *r.Status {
		case evaluation.StatusCompleted, evaluation.StatusIncomplete, evaluation.StatusUnsupported, evaluation.StatusInterrupted, evaluation.StatusFailed:
		default:
			return fmt.Errorf("%w: status %q", ErrEvaluationInvalidInput, *r.Status)
		}
	}
	if r.Status != nil && (r.ExitClass == EvalExitCompleted) != (*r.Status == evaluation.StatusCompleted) &&
		(r.ExitClass == EvalExitCompleted || *r.Status == evaluation.StatusCompleted) {
		return fmt.Errorf("%w: exit_class %q contradicts status %q", ErrEvaluationInvalidInput, r.ExitClass, *r.Status)
	}
	if r.ErrorClass != nil {
		switch *r.ErrorClass {
		case evaluation.ErrClassCapabilityMissing, evaluation.ErrClassOutputTruncated, evaluation.ErrClassSourceUnavailable,
			evaluation.ErrClassBudgetExhausted, evaluation.ErrClassInterrupted, evaluation.ErrClassTimeout,
			evaluation.ErrClassRuntimeError, evaluation.ErrClassOutputMalformed, evaluation.ErrClassOutputMissing,
			evaluation.ErrClassAuthMissing, evaluation.ErrClassLaunchError:
		default:
			return fmt.Errorf("%w: error_class %q", ErrEvaluationInvalidInput, *r.ErrorClass)
		}
	}
	if r.ErrorMessage != nil && len(*r.ErrorMessage) > 1024 {
		return fmt.Errorf("%w: error_message exceeds 1024 bytes", ErrEvaluationInvalidInput)
	}
	if (r.DurationMs != nil && *r.DurationMs < 0) || (r.UsageTokens != nil && *r.UsageTokens < 0) {
		return fmt.Errorf("%w: negative duration or usage", ErrEvaluationInvalidInput)
	}
	if len(r.Findings) > 0 && r.ExitClass != EvalExitCompleted {
		return fmt.Errorf("%w: findings are only valid on a completed attempt", ErrEvaluationInvalidInput)
	}
	if err := evaluation.ValidateFindings(r.Findings); err != nil {
		return fmt.Errorf("%w: %v", ErrEvaluationInvalidInput, err)
	}
	return nil
}

// FinalizeEvaluationAttempt records an attempt's result and findings atomically.
// It fences: the attempt must be the job's current attempt, still active, and
// its lease must not have been reached. A late result instead records the
// attempt as expired (lease_expired) and returns ErrEvaluationAttemptExpired; a
// stale result for a superseded attempt returns ErrEvaluationFenceMismatch.
func (s *sqliteStore) FinalizeEvaluationAttempt(ctx context.Context, res EvaluationAttemptResult) error {
	if res.AttemptID != res.FenceAttemptID {
		return ErrEvaluationFenceMismatch
	}
	if err := res.validate(); err != nil {
		return err
	}

	now := s.Now().UTC()
	nowStr := now.Format(timestampLayout)

	var statusVal, errorClassVal any
	if res.Status != nil {
		statusVal = string(*res.Status)
	}
	if res.ErrorClass != nil {
		errorClassVal = string(*res.ErrorClass)
	}

	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var jobID, attemptState, startedAtStr, expiresAtStr string
	err = tx.QueryRowContext(ctx,
		`SELECT job_id, state, started_at, expires_at FROM evaluation_attempt WHERE id = ?`,
		res.AttemptID,
	).Scan(&jobID, &attemptState, &startedAtStr, &expiresAtStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrEvaluationAttemptNotFound
		}
		return fmt.Errorf("fetch attempt: %w", err)
	}

	if attemptState == EvalAttemptFinalized {
		return ErrEvaluationAttemptFinalized
	}
	if attemptState != EvalAttemptActive {
		return ErrEvaluationAttemptExpired
	}

	var jobCurrentAttemptID string
	if err = tx.QueryRowContext(ctx,
		`SELECT current_attempt_id FROM evaluation_job WHERE id = ?`, jobID,
	).Scan(&jobCurrentAttemptID); err != nil {
		return fmt.Errorf("fetch job: %w", err)
	}
	if jobCurrentAttemptID != res.AttemptID {
		return ErrEvaluationFenceMismatch
	}

	expiresAt, err := time.Parse(timestampLayout, expiresAtStr)
	if err != nil {
		return fmt.Errorf("parse expires_at: %w", err)
	}
	if leaseExpired(now, expiresAt) {
		if err := expireEvaluationAttempt(ctx, tx, res.AttemptID, startedAtStr, expiresAtStr); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("commit transaction: %w", err)
		}
		return ErrEvaluationAttemptExpired
	}

	_, err = tx.ExecContext(ctx,
		`UPDATE evaluation_attempt
		 SET state = ?, ended_at = ?, exit_class = ?, status = ?, error_class = ?, error_message = ?, duration_ms = ?, usage_tokens = ?
		 WHERE id = ? AND state = ?`,
		EvalAttemptFinalized, nowStr, string(res.ExitClass), statusVal, errorClassVal, res.ErrorMessage, res.DurationMs, res.UsageTokens,
		res.AttemptID, EvalAttemptActive,
	)
	if err != nil {
		return fmt.Errorf("finalize attempt: %w", err)
	}

	for i, f := range res.Findings {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO evaluation_finding (id, attempt_id, sequence_number, finding_id, severity, claim, summary, evidence)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			GenerateID(), res.AttemptID, i, f.ID, string(f.Severity), nullableString(f.Claim), f.Summary, nullableString(f.Evidence))
		if err != nil {
			return fmt.Errorf("store finding: %w", err)
		}
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "PRIMARY KEY"))
}

func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// ListEvaluationFindings returns an attempt's findings in recorded order.
func (s *sqliteStore) ListEvaluationFindings(ctx context.Context, attemptID string) ([]evaluation.Finding, error) {
	if _, err := s.fetchEvaluationAttempt(ctx, s.conn, attemptID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx,
		`SELECT finding_id, severity, COALESCE(claim, ''), summary, COALESCE(evidence, '')
		 FROM evaluation_finding WHERE attempt_id = ? ORDER BY sequence_number`, attemptID)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close()
	var out []evaluation.Finding
	for rows.Next() {
		var f evaluation.Finding
		var sev string
		if err := rows.Scan(&f.ID, &sev, &f.Claim, &f.Summary, &f.Evidence); err != nil {
			return nil, fmt.Errorf("scan finding: %w", err)
		}
		f.Severity = evaluation.Severity(sev)
		out = append(out, f)
	}
	return out, rows.Err()
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
	if exitClass != nil {
		ec := EvaluationExitClass(*exitClass)
		attempt.ExitClass = &ec
	}
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
		`SELECT id, campaign_id, project_id, original_task_id, original_review_round, submitted_sha,
		        snapshot_digest, source_digest, manifest_digest, prompt_version, model_version,
		        runtime_version, created_at FROM evaluation_sample WHERE id = ?`,
		sampleID,
	).Scan(&sample.ID, &sample.CampaignID, &sample.ProjectID, &sample.OriginalTaskID, &sample.OriginalReviewRound,
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

// GetEvaluationCandidate fetches a candidate by ID, rebuilding its M1 config
// from the stored canonical identity and verifying it against the stored digest.
func (s *sqliteStore) GetEvaluationCandidate(ctx context.Context, candidateID string) (EvaluationCandidate, error) {
	row := s.conn.QueryRowContext(ctx,
		`SELECT id, campaign_id, identity_json, candidate_config_digest, account_pool_id, per_candidate_cap, created_at
		 FROM evaluation_candidate WHERE id = ?`,
		candidateID,
	)
	candidate, err := scanEvaluationCandidate(row)
	if errors.Is(err, sql.ErrNoRows) {
		return EvaluationCandidate{}, ErrEvaluationCandidateNotFound
	}
	return candidate, err
}

// scanEvaluationCandidate reads one candidate row and verifies the stored
// identity against its recorded digest and pool.
func scanEvaluationCandidate(row interface{ Scan(...any) error }) (EvaluationCandidate, error) {
	candidate := EvaluationCandidate{}
	var identityJSON, digest, pool string
	if err := row.Scan(&candidate.ID, &candidate.CampaignID, &identityJSON, &digest, &pool, &candidate.PerCandidateCap, &candidate.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EvaluationCandidate{}, err
		}
		return EvaluationCandidate{}, fmt.Errorf("scan candidate: %w", err)
	}
	var identity evaluation.CandidateIdentity
	if err := json.Unmarshal([]byte(identityJSON), &identity); err != nil {
		return EvaluationCandidate{}, fmt.Errorf("%w: %v", ErrEvaluationCandidateCorrupt, err)
	}
	config, err := evaluation.NewCandidateConfig(identity)
	if err != nil {
		return EvaluationCandidate{}, fmt.Errorf("%w: %v", ErrEvaluationCandidateCorrupt, err)
	}
	if config.Digest() != digest || identity.AccountPool != pool {
		return EvaluationCandidate{}, ErrEvaluationCandidateCorrupt
	}
	candidate.Config = config
	return candidate, nil
}

// GetEvaluationCampaign fetches a campaign by ID.
func (s *sqliteStore) GetEvaluationCampaign(ctx context.Context, campaignID string) (EvaluationCampaign, error) {
	campaign := EvaluationCampaign{}

	err := s.conn.QueryRowContext(ctx,
		`SELECT id, name, description, cohort_manifest, attempt_cap, created_at, updated_at, paused_at, resumed_at
		 FROM evaluation_campaign WHERE id = ?`,
		campaignID,
	).Scan(&campaign.ID, &campaign.Name, &campaign.Description,
		&campaign.CohortManifest, &campaign.AttemptCap,
		&campaign.CreatedAt, &campaign.UpdatedAt, &campaign.PausedAt, &campaign.ResumedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return EvaluationCampaign{}, ErrEvaluationCampaignNotFound
		}
		return EvaluationCampaign{}, fmt.Errorf("get campaign: %w", err)
	}

	rows, err := s.conn.QueryContext(ctx,
		`SELECT project_id FROM evaluation_campaign_project WHERE campaign_id = ? ORDER BY project_id`, campaignID)
	if err != nil {
		return EvaluationCampaign{}, fmt.Errorf("get campaign projects: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return EvaluationCampaign{}, fmt.Errorf("scan campaign project: %w", err)
		}
		campaign.AllowedProjectIDs = append(campaign.AllowedProjectIDs, p)
	}
	if err := rows.Err(); err != nil {
		return EvaluationCampaign{}, err
	}

	modelRows, err := s.conn.QueryContext(ctx,
		`SELECT model_id FROM evaluation_campaign_model WHERE campaign_id = ? ORDER BY model_id`, campaignID)
	if err != nil {
		return EvaluationCampaign{}, fmt.Errorf("get campaign models: %w", err)
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var m string
		if err := modelRows.Scan(&m); err != nil {
			return EvaluationCampaign{}, fmt.Errorf("scan campaign model: %w", err)
		}
		campaign.AllowedModelIDs = append(campaign.AllowedModelIDs, m)
	}
	if err := modelRows.Err(); err != nil {
		return EvaluationCampaign{}, err
	}
	return campaign, nil
}

// ListEvaluationCandidates fetches all candidates for a campaign in creation
// order, each verified against its recorded digest.
func (s *sqliteStore) ListEvaluationCandidates(ctx context.Context, campaignID string) ([]EvaluationCandidate, error) {
	if _, err := s.GetEvaluationCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx,
		`SELECT id, campaign_id, identity_json, candidate_config_digest, account_pool_id, per_candidate_cap, created_at
		 FROM evaluation_candidate WHERE campaign_id = ? ORDER BY created_at, id`,
		campaignID)
	if err != nil {
		return nil, fmt.Errorf("query candidates: %w", err)
	}
	defer rows.Close()

	var candidates []EvaluationCandidate
	for rows.Next() {
		c, err := scanEvaluationCandidate(rows)
		if err != nil {
			return nil, err
		}
		candidates = append(candidates, c)
	}
	return candidates, rows.Err()
}

// EvaluationCandidateStatus is one candidate's cap usage. Every started attempt
// counts against the caps, including expired ones; Active counts only attempts
// whose lease is still live.
type EvaluationCandidateStatus struct {
	Candidate    EvaluationCandidate
	AttemptsUsed int
	Active       int
	// Pool is nil when the candidate's pool has not been configured, which
	// blocks admission rather than meaning unlimited capacity.
	Pool *EvaluationPoolState
}

// EvaluationCampaignStatus is the campaign with per-candidate usage.
type EvaluationCampaignStatus struct {
	Campaign          EvaluationCampaign
	TotalAttemptsUsed int
	Candidates        []EvaluationCandidateStatus
}

// GetEvaluationCampaignStatus reads the campaign, its candidates and their
// attempt and pool usage without changing anything.
func (s *sqliteStore) GetEvaluationCampaignStatus(ctx context.Context, campaignID string) (EvaluationCampaignStatus, error) {
	campaign, err := s.GetEvaluationCampaign(ctx, campaignID)
	if err != nil {
		return EvaluationCampaignStatus{}, err
	}
	candidates, err := s.ListEvaluationCandidates(ctx, campaignID)
	if err != nil {
		return EvaluationCampaignStatus{}, err
	}
	now := formatTS(s.Now())
	status := EvaluationCampaignStatus{Campaign: campaign, Candidates: make([]EvaluationCandidateStatus, 0, len(candidates))}
	for _, c := range candidates {
		cs := EvaluationCandidateStatus{Candidate: c}
		err := s.conn.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(CASE WHEN ea.state = 'active' AND ea.expires_at > ? THEN 1 ELSE 0 END), 0)
			 FROM evaluation_attempt ea JOIN evaluation_job ej ON ea.job_id = ej.id
			 WHERE ej.candidate_id = ?`, now, c.ID).Scan(&cs.AttemptsUsed, &cs.Active)
		if err != nil {
			return EvaluationCampaignStatus{}, fmt.Errorf("count attempts: %w", err)
		}
		pool, err := s.GetEvaluationPool(ctx, c.AccountPoolID())
		switch {
		case err == nil:
			cs.Pool = &pool
		case !errors.Is(err, ErrEvaluationPoolNotConfigured):
			return EvaluationCampaignStatus{}, err
		}
		status.TotalAttemptsUsed += cs.AttemptsUsed
		status.Candidates = append(status.Candidates, cs)
	}
	return status, nil
}
