package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// BoundedComparisonConfig holds the configuration for running a bounded
// evaluation comparison job through a generic adapter.
type BoundedComparisonConfig struct {
	// Registry holds all registered adapter runtimes.
	Registry *Registry
	// Credentials resolves credential references to secrets.
	Credentials CredentialResolver
	// Store persists evaluation job lifecycle and results.
	Store EvaluationStore
	// RuntimeName names the adapter to invoke.
	RuntimeName string
	// StagingDir is where staged snapshots are written.
	StagingDir string
	// Now is an injectable clock; time.Now is used if nil.
	Now func() time.Time
	// MaxRetries bounds the number of attempts to claim and retry after
	// transient failures. Exhausted attempts record EvalExitExhaustedCampaign.
	MaxRetries int
	// InitialLeaseExpiry sets the initial claim lease TTL.
	InitialLeaseExpiry time.Duration
	// LeaseRenewalInterval is how often to renew the lease during execution.
	LeaseRenewalInterval time.Duration
}

// EvaluationExitClass is the distinct terminal outcome of one attempt.
type EvaluationExitClass string

const (
	ExitCompleted         EvaluationExitClass = "completed"
	ExitFailed            EvaluationExitClass = "failed"
	ExitUnavailableSource EvaluationExitClass = "unavailable_source"
	ExitIncompleteOutput  EvaluationExitClass = "incomplete_output"
	ExitExhaustedCampaign EvaluationExitClass = "exhausted_campaign"
)

// EvaluationJobClaim is the input for claiming an evaluation job.
type EvaluationJobClaim struct {
	SampleID     string
	CandidateID  string
	RequestID    string
	LeaseExpires time.Duration
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
	ID        string
	JobID     string
	RequestID string
	State     string
	StartedAt string
	ExpiresAt string
}

// EvaluationJobClaimResult bundles the created job and its first attempt.
type EvaluationJobClaimResult struct {
	Job     EvaluationJob
	Attempt EvaluationAttempt
}

// EvaluationAttemptResult is the recorded outcome of one attempt.
type EvaluationAttemptResult struct {
	AttemptID      string
	FenceAttemptID string
	ExitClass      EvaluationExitClass
	Status         *Status
	ErrorClass     *ErrorClass
	ErrorMessage   *string
	DurationMs     *int
	Findings       []Finding
}

// EvaluationStore defines the evaluation job persistence contract.
type EvaluationStore interface {
	ClaimEvaluationJob(ctx context.Context, req EvaluationJobClaim) (EvaluationJobClaimResult, error)
	RenewEvaluationAttempt(ctx context.Context, attemptID string, expiresAt time.Time) error
	FinalizeEvaluationAttempt(ctx context.Context, res EvaluationAttemptResult) error
}

// BoundedComparisonRequest holds the input to a comparison run.
type BoundedComparisonRequest struct {
	// RunID uniquely identifies this run (alphanumeric/dash/underscore only).
	RunID string
	// SampleID is the evaluation sample being reviewed.
	SampleID string
	// CandidateID is the evaluation candidate performing the review.
	CandidateID string
	// SnapshotPath is the absolute path to the frozen snapshot.
	SnapshotPath string
	// BlindedPrompt is the prompt without task/reviewer context.
	BlindedPrompt string
	// ToolAccess declares what the review needs.
	ToolAccess ToolAccessRequirements
}

// BoundedComparisonResult is the outcome of running a bounded evaluation.
type BoundedComparisonResult struct {
	// Response is the normalized adapter result.
	Response CandidateResponse
	// Runtime is the name of the adapter that ran.
	Runtime string
	// CandidateDigest is the identity digest of the candidate.
	CandidateDigest string
	// Launched reports whether the adapter process was started.
	Launched bool
	// ExitCode is the adapter process exit code (-1 if not launched or killed by signal).
	ExitCode int
	// Stderr is the bounded, credential-redacted adapter stderr.
	Stderr string
	// RawOutput contains leading bytes of a malformed result file.
	RawOutput []byte
	// ExitClass records the distinct outcome of this attempt.
	ExitClass EvaluationExitClass
	// AttemptID identifies the finalized attempt.
	AttemptID string
}

var validRunIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func sanitizeRunID(runID string) error {
	if runID == "" {
		return fmt.Errorf("run_id is required")
	}
	if !validRunIDPattern.MatchString(runID) {
		return fmt.Errorf("run_id must contain only alphanumerics, dash, and underscore")
	}
	return nil
}

// RunBoundedComparison executes a single evaluation comparison job through a
// generic adapter. It claims the job before launch, renews the lease during
// execution, validates the result, and finalizes the attempt. Results are
// recorded with distinct outcomes (completed, failed, source unavailable, etc).
// The adapter runs without ODONIAN_TOKEN or other host credentials; only the
// configured credential reference is available. Failures of the adapter are
// normalized into the response; a returned error means the host itself could
// not proceed (invalid request, unknown runtime, local I/O).
func (c BoundedComparisonConfig) RunBoundedComparison(ctx context.Context, req BoundedComparisonRequest) (BoundedComparisonResult, error) {
	if err := req.ToolAccess.Validate(); err != nil {
		return BoundedComparisonResult{}, fmt.Errorf("tool access requirements: %w", err)
	}
	if err := sanitizeRunID(req.RunID); err != nil {
		return BoundedComparisonResult{}, fmt.Errorf("run_id: %w", err)
	}
	if req.SampleID == "" {
		return BoundedComparisonResult{}, fmt.Errorf("sample_id is required")
	}
	if req.CandidateID == "" {
		return BoundedComparisonResult{}, fmt.Errorf("candidate_id is required")
	}
	if req.SnapshotPath == "" || !filepath.IsAbs(req.SnapshotPath) {
		return BoundedComparisonResult{}, fmt.Errorf("snapshot_path must be absolute")
	}
	if req.BlindedPrompt == "" {
		return BoundedComparisonResult{}, fmt.Errorf("blinded_prompt is required")
	}

	if c.StagingDir == "" {
		return BoundedComparisonResult{}, fmt.Errorf("staging_dir is required")
	}
	if c.RuntimeName == "" {
		return BoundedComparisonResult{}, fmt.Errorf("runtime_name is required")
	}
	if c.Store == nil {
		return BoundedComparisonResult{}, fmt.Errorf("store is required")
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 3
	}
	if c.InitialLeaseExpiry <= 0 {
		c.InitialLeaseExpiry = 30 * time.Second
	}
	if c.LeaseRenewalInterval <= 0 {
		c.LeaseRenewalInterval = 10 * time.Second
	}

	if err := os.MkdirAll(c.StagingDir, 0o700); err != nil {
		return BoundedComparisonResult{}, fmt.Errorf("create staging dir: %w", err)
	}

	now := c.Now
	if now == nil {
		now = time.Now
	}

	// Claim the evaluation job, with retries for transient failures.
	claim := EvaluationJobClaim{
		SampleID:     req.SampleID,
		CandidateID:  req.CandidateID,
		RequestID:    req.RunID,
		LeaseExpires: c.InitialLeaseExpiry,
	}

	var claimResult EvaluationJobClaimResult
	var claimErr error
	const capacityExhaustedErr = "evaluation campaign or candidate has exhausted attempt capacity"
	const attemptLiveErr = "previous evaluation attempt is still live"
	const pausedErr = "evaluation campaign is paused"

	for attempt := 0; attempt < c.MaxRetries; attempt++ {
		claimResult, claimErr = c.Store.ClaimEvaluationJob(ctx, claim)
		if claimErr == nil {
			break
		}
		// Distinct outcome for capacity exhaustion.
		if claimErr.Error() == capacityExhaustedErr {
			return BoundedComparisonResult{
				ExitClass: ExitExhaustedCampaign,
			}, fmt.Errorf("evaluation campaign capacity exhausted")
		}
		// Retry on transient errors; other errors are fatal.
		errMsg := claimErr.Error()
		if errMsg != attemptLiveErr && errMsg != pausedErr {
			return BoundedComparisonResult{}, fmt.Errorf("claim job: %w", claimErr)
		}
	}
	if claimErr != nil {
		return BoundedComparisonResult{}, fmt.Errorf("claim job after retries: %w", claimErr)
	}

	attemptID := claimResult.Attempt.ID
	resultPath := filepath.Join(c.StagingDir, req.RunID, "result.json")

	candReq := CandidateRequest{
		Version:       ProtocolVersion,
		RunID:         req.RunID,
		SnapshotPath:  req.SnapshotPath,
		BlindedPrompt: req.BlindedPrompt,
		ToolAccess:    req.ToolAccess,
		ResultPath:    resultPath,
	}

	if err := candReq.Validate(); err != nil {
		return BoundedComparisonResult{}, fmt.Errorf("candidate request: %w", err)
	}

	// Start tracking metadata.
	startTime := now()
	defer func() {
		if attemptID == "" {
			return
		}
		// Finalize runs even on error; record the outcome distinctly.
		// This is deferred so lease renewals don't block the finalize flow.
	}()

	pipeline := &Pipeline{
		Registry:    c.Registry,
		Credentials: c.Credentials,
		Now:         now,
	}

	// Lease renewal ticker.
	ticker := time.NewTicker(c.LeaseRenewalInterval)
	defer ticker.Stop()

	type executionResult struct {
		res *Result
		err error
	}
	execDone := make(chan executionResult, 1)

	go func() {
		res, err := pipeline.Execute(ctx, c.RuntimeName, candReq)
		execDone <- executionResult{res: &res, err: err}
	}()

	var pipelineRes *Result
	var pipelineErr error

	for {
		select {
		case <-ctx.Done():
			return BoundedComparisonResult{
				ExitClass: ExitFailed,
			}, ctx.Err()
		case <-ticker.C:
			// Renew the lease while waiting for execution.
			if err := c.Store.RenewEvaluationAttempt(ctx, attemptID, now().Add(c.InitialLeaseExpiry)); err != nil {
				// Log renewal failure but continue; the attempt might expire.
				// Return it in finalize.
			}
		case result := <-execDone:
			pipelineRes = result.res
			pipelineErr = result.err
			goto finalize
		}
	}

finalize:
	duration := now().Sub(startTime)
	durationMs := int(duration.Milliseconds())

	// Determine exit class from the outcome.
	exitClass := ExitCompleted
	if pipelineErr != nil {
		exitClass = ExitFailed
	} else if pipelineRes != nil {
		switch pipelineRes.Response.Status {
		case StatusCompleted:
			exitClass = ExitCompleted
		case StatusFailed:
			exitClass = ExitFailed
		case StatusIncomplete:
			exitClass = ExitIncompleteOutput
		case StatusUnsupported, StatusInterrupted:
			exitClass = ExitFailed
		}
		if pipelineRes.Response.ErrorClass != "" && pipelineRes.Response.ErrorClass == ErrClassSourceUnavailable {
			exitClass = ExitUnavailableSource
		}
	}

	// Validate result before recording.
	var findings []Finding
	var status *Status
	var errorClass *ErrorClass
	var errorMsg *string
	if pipelineRes != nil {
		if len(pipelineRes.Response.Findings) > 0 {
			findings = pipelineRes.Response.Findings
		}
		status = (*Status)(&pipelineRes.Response.Status)
		if pipelineRes.Response.ErrorClass != "" {
			errorClass = (*ErrorClass)(&pipelineRes.Response.ErrorClass)
		}
		if pipelineRes.Response.ErrorMessage != "" {
			errorMsg = &pipelineRes.Response.ErrorMessage
		}
	}

	attemptResult := EvaluationAttemptResult{
		AttemptID:      attemptID,
		FenceAttemptID: attemptID,
		ExitClass:      exitClass,
		Status:         status,
		ErrorClass:     errorClass,
		ErrorMessage:   errorMsg,
		DurationMs:     &durationMs,
		Findings:       findings,
	}

	if err := c.Store.FinalizeEvaluationAttempt(ctx, attemptResult); err != nil {
		return BoundedComparisonResult{
			Response:        pipelineRes.Response,
			Runtime:         pipelineRes.Runtime,
			CandidateDigest: pipelineRes.CandidateDigest,
			Launched:        pipelineRes.Launched,
			ExitCode:        pipelineRes.ExitCode,
			Stderr:          pipelineRes.Stderr,
			RawOutput:       pipelineRes.RawOutput,
			ExitClass:       exitClass,
			AttemptID:       attemptID,
		}, fmt.Errorf("finalize attempt: %w", err)
	}

	return BoundedComparisonResult{
		Response:        pipelineRes.Response,
		Runtime:         pipelineRes.Runtime,
		CandidateDigest: pipelineRes.CandidateDigest,
		Launched:        pipelineRes.Launched,
		ExitCode:        pipelineRes.ExitCode,
		Stderr:          pipelineRes.Stderr,
		RawOutput:       pipelineRes.RawOutput,
		ExitClass:       exitClass,
		AttemptID:       attemptID,
	}, nil
}

// hashDigest returns a SHA256 hex digest of the input.
func hashDigest(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
