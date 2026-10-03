package evaluation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BoundedComparisonConfig holds the configuration for running a bounded
// evaluation comparison job through a generic adapter.
type BoundedComparisonConfig struct {
	// Registry holds all registered adapter runtimes.
	Registry *Registry
	// Credentials resolves credential references to secrets.
	Credentials CredentialResolver
	// RuntimeName names the adapter to invoke.
	RuntimeName string
	// StagingDir is where staged snapshots are written.
	StagingDir string
	// Now is an injectable clock; time.Now is used if nil.
	Now func() time.Time
}

// BoundedComparisonRequest holds the input to a comparison run.
type BoundedComparisonRequest struct {
	// RunID uniquely identifies this run.
	RunID string
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
}

// RunBoundedComparison executes a single evaluation comparison job through a
// generic adapter. It stages the frozen snapshot, invokes the registered adapter
// through the pipeline, validates and returns the normalized result.
// The adapter runs without ODONIAN_TOKEN or other host credentials; only the
// configured credential reference is available. Failures of the adapter are
// normalized into the response; a returned error means the host itself could
// not proceed (invalid request, unknown runtime, local I/O).
func (c BoundedComparisonConfig) RunBoundedComparison(ctx context.Context, req BoundedComparisonRequest) (BoundedComparisonResult, error) {
	if err := req.ToolAccess.Validate(); err != nil {
		return BoundedComparisonResult{}, fmt.Errorf("tool access requirements: %w", err)
	}
	if req.RunID == "" {
		return BoundedComparisonResult{}, fmt.Errorf("run_id is required")
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

	if err := os.MkdirAll(c.StagingDir, 0o700); err != nil {
		return BoundedComparisonResult{}, fmt.Errorf("create staging dir: %w", err)
	}

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

	pipeline := &Pipeline{
		Registry:    c.Registry,
		Credentials: c.Credentials,
		Now:         c.Now,
	}

	res, err := pipeline.Execute(ctx, c.RuntimeName, candReq)
	if err != nil {
		return BoundedComparisonResult{
			Response:        res.Response,
			Runtime:         res.Runtime,
			CandidateDigest: res.CandidateDigest,
			Launched:        res.Launched,
			ExitCode:        res.ExitCode,
			Stderr:          res.Stderr,
			RawOutput:       res.RawOutput,
		}, err
	}

	return BoundedComparisonResult{
		Response:        res.Response,
		Runtime:         res.Runtime,
		CandidateDigest: res.CandidateDigest,
		Launched:        res.Launched,
		ExitCode:        res.ExitCode,
		Stderr:          res.Stderr,
		RawOutput:       res.RawOutput,
	}, nil
}
