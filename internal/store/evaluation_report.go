package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// FindingDisposition records an operator's determination of whether a finding is valid/invalid/unresolved.
type FindingDisposition struct {
	ID          string `json:"id"`
	CampaignID  string `json:"campaign_id"`
	SampleID    string `json:"sample_id"`
	CandidateID string `json:"candidate_id"`
	FindingID   string `json:"finding_id"`
	Disposition string `json:"disposition"` // valid, invalid, unresolved
	Evidence    string `json:"evidence"`
	DecidedBy   string `json:"decided_by"`
	DecidedAt   string `json:"decided_at"`
}

// EvaluationReport aggregates findings and metrics for a campaign.
type EvaluationReport struct {
	ID          string          `json:"id"`
	CampaignID  string          `json:"campaign_id"`
	GeneratedAt string          `json:"generated_at"`
	GeneratedBy string          `json:"generated_by"`
	Report      json.RawMessage `json:"report"`
}

// EvaluationReportData is the structured content of a report.
type EvaluationReportData struct {
	CampaignID         string                `json:"campaign_id"`
	GeneratedAt        string                `json:"generated_at"`
	GeneratedBy        string                `json:"generated_by"`
	CohortSize         int                   `json:"cohort_size"`
	CompletedSamples   int                   `json:"completed_samples"`
	FailedSamples      int                   `json:"failed_samples"`
	UnavailableSamples int                   `json:"unavailable_samples"`
	Candidates         []CandidateReportData `json:"candidates"`
	GroupedFindings    []FindingGroupData    `json:"grouped_findings"`
	Metrics            ReportMetrics         `json:"metrics"`
}

// CandidateReportData summarizes one candidate's performance.
type CandidateReportData struct {
	CandidateID           string             `json:"candidate_id"`
	Model                 string             `json:"model"`
	Runtime               string             `json:"runtime"`
	CompletedCount        int                `json:"completed_count"`
	FailedCount           int                `json:"failed_count"`
	AverageLatencyMs      *int               `json:"average_latency_ms,omitempty"`
	Usage                 map[string]float64 `json:"usage,omitempty"`
	MaterialFindingsCount int                `json:"material_findings_count"`
	MinorFindingsCount    int                `json:"minor_findings_count"`
	NoteFindingsCount     int                `json:"note_findings_count"`
}

// FindingGroupData groups the same issue across candidates and baselines.
type FindingGroupData struct {
	GroupID      string                 `json:"group_id"`
	Summary      string                 `json:"summary"`
	Baseline     *BaselineFoundingData  `json:"baseline,omitempty"`
	Candidates   []CandidateFindingData `json:"candidates"`
	Dispositions []DispositionData      `json:"dispositions,omitempty"`
}

// BaselineFoundingData represents what the Astra/Fable baseline found.
type BaselineFoundingData struct {
	Model    string `json:"model"`
	Severity string `json:"severity"`
	Found    bool   `json:"found"`
}

// CandidateFindingData represents what one candidate found.
type CandidateFindingData struct {
	CandidateID string `json:"candidate_id"`
	Model       string `json:"model"`
	Severity    string `json:"severity"`
	Claim       string `json:"claim,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
	Found       bool   `json:"found"`
}

// DispositionData records operator decisions about a finding.
type DispositionData struct {
	Disposition string `json:"disposition"`
	Evidence    string `json:"evidence"`
	DecidedBy   string `json:"decided_by"`
	DecidedAt   string `json:"decided_at"`
}

// ReportMetrics summarizes evaluation accuracy.
type ReportMetrics struct {
	MaterialFindingsRecall float64 `json:"material_findings_recall,omitempty"`
	UniqueToCandidate      int     `json:"unique_to_candidate"`
	UniqueToBaseline       int     `json:"unique_to_baseline"`
	FalsePositives         int     `json:"false_positives"`
	ConfirmedMaterial      int     `json:"confirmed_material"`
	UnresolvedCount        int     `json:"unresolved_count"`
}

var (
	ErrEvaluationDispositionNotFound      = errors.New("evaluation finding disposition not found")
	ErrEvaluationDispositionAlreadyExists = errors.New("finding disposition already recorded")
	ErrEvaluationReportNotFound           = errors.New("evaluation report not found")
)

// RecordFindingDisposition persists an operator's decision about a finding.
func (s *sqliteStore) RecordFindingDisposition(ctx context.Context, disposition FindingDisposition) (FindingDisposition, error) {
	if disposition.ID == "" || disposition.CampaignID == "" || disposition.SampleID == "" ||
		disposition.CandidateID == "" || disposition.FindingID == "" || disposition.DecidedBy == "" {
		return FindingDisposition{}, ErrEvaluationInvalidInput
	}
	switch disposition.Disposition {
	case "valid", "invalid", "unresolved":
	default:
		return FindingDisposition{}, fmt.Errorf("%w: disposition must be valid/invalid/unresolved", ErrEvaluationInvalidInput)
	}

	now := s.Now().UTC().Format(timestampLayout)
	disposition.DecidedAt = now

	_, err := s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_finding_disposition
		 (id, campaign_id, sample_id, candidate_id, finding_id, disposition, evidence, decided_by, decided_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		disposition.ID, disposition.CampaignID, disposition.SampleID, disposition.CandidateID,
		disposition.FindingID, disposition.Disposition, disposition.Evidence, disposition.DecidedBy, now,
	)
	if isUniqueViolation(err) {
		return FindingDisposition{}, ErrEvaluationDispositionAlreadyExists
	}
	if err != nil {
		return FindingDisposition{}, fmt.Errorf("record finding disposition: %w", err)
	}
	return disposition, nil
}

// GetFindingDisposition retrieves a recorded disposition.
func (s *sqliteStore) GetFindingDisposition(ctx context.Context, campaignID, sampleID, candidateID, findingID string) (FindingDisposition, error) {
	var d FindingDisposition
	err := s.readConn.QueryRowContext(ctx,
		`SELECT id, campaign_id, sample_id, candidate_id, finding_id, disposition, evidence, decided_by, decided_at
		 FROM evaluation_finding_disposition
		 WHERE campaign_id = ? AND sample_id = ? AND candidate_id = ? AND finding_id = ?`,
		campaignID, sampleID, candidateID, findingID,
	).Scan(&d.ID, &d.CampaignID, &d.SampleID, &d.CandidateID, &d.FindingID, &d.Disposition, &d.Evidence, &d.DecidedBy, &d.DecidedAt)
	if err == sql.ErrNoRows {
		return FindingDisposition{}, ErrEvaluationDispositionNotFound
	}
	if err != nil {
		return FindingDisposition{}, fmt.Errorf("get finding disposition: %w", err)
	}
	return d, nil
}

// StoreEvaluationReport persists a generated report.
func (s *sqliteStore) StoreEvaluationReport(ctx context.Context, report EvaluationReport) (EvaluationReport, error) {
	if report.ID == "" || report.CampaignID == "" || report.GeneratedBy == "" || len(report.Report) == 0 {
		return EvaluationReport{}, ErrEvaluationInvalidInput
	}

	now := s.Now().UTC().Format(timestampLayout)
	report.GeneratedAt = now

	_, err := s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_report (id, campaign_id, generated_at, generated_by, report_json)
		 VALUES (?, ?, ?, ?, ?)`,
		report.ID, report.CampaignID, now, report.GeneratedBy, report.Report,
	)
	if isUniqueViolation(err) {
		return EvaluationReport{}, ErrEvaluationAlreadyExists
	}
	if err != nil {
		return EvaluationReport{}, fmt.Errorf("store evaluation report: %w", err)
	}
	return report, nil
}

// GetEvaluationReport retrieves a stored report.
func (s *sqliteStore) GetEvaluationReport(ctx context.Context, id string) (EvaluationReport, error) {
	var r EvaluationReport
	err := s.readConn.QueryRowContext(ctx,
		`SELECT id, campaign_id, generated_at, generated_by, report_json
		 FROM evaluation_report WHERE id = ?`,
		id,
	).Scan(&r.ID, &r.CampaignID, &r.GeneratedAt, &r.GeneratedBy, &r.Report)
	if err == sql.ErrNoRows {
		return EvaluationReport{}, ErrEvaluationReportNotFound
	}
	if err != nil {
		return EvaluationReport{}, fmt.Errorf("get evaluation report: %w", err)
	}
	return r, nil
}

// ListDispositionsForCampaignSample lists all dispositions for a sample in a campaign.
func (s *sqliteStore) ListDispositionsForCampaignSample(ctx context.Context, campaignID, sampleID string) ([]FindingDisposition, error) {
	rows, err := s.readConn.QueryContext(ctx,
		`SELECT id, campaign_id, sample_id, candidate_id, finding_id, disposition, evidence, decided_by, decided_at
		 FROM evaluation_finding_disposition
		 WHERE campaign_id = ? AND sample_id = ?
		 ORDER BY decided_at`,
		campaignID, sampleID,
	)
	if err != nil {
		return nil, fmt.Errorf("list dispositions: %w", err)
	}
	defer rows.Close()

	var dispositions []FindingDisposition
	for rows.Next() {
		var d FindingDisposition
		if err := rows.Scan(&d.ID, &d.CampaignID, &d.SampleID, &d.CandidateID, &d.FindingID,
			&d.Disposition, &d.Evidence, &d.DecidedBy, &d.DecidedAt); err != nil {
			return nil, fmt.Errorf("scan disposition: %w", err)
		}
		dispositions = append(dispositions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dispositions: %w", err)
	}
	return dispositions, nil
}

// ListCampaignDispositions lists all dispositions for a campaign.
func (s *sqliteStore) ListCampaignDispositions(ctx context.Context, campaignID string) ([]FindingDisposition, error) {
	rows, err := s.readConn.QueryContext(ctx,
		`SELECT id, campaign_id, sample_id, candidate_id, finding_id, disposition, evidence, decided_by, decided_at
		 FROM evaluation_finding_disposition
		 WHERE campaign_id = ?
		 ORDER BY decided_at`,
		campaignID,
	)
	if err != nil {
		return nil, fmt.Errorf("list campaign dispositions: %w", err)
	}
	defer rows.Close()

	var dispositions []FindingDisposition
	for rows.Next() {
		var d FindingDisposition
		if err := rows.Scan(&d.ID, &d.CampaignID, &d.SampleID, &d.CandidateID, &d.FindingID,
			&d.Disposition, &d.Evidence, &d.DecidedBy, &d.DecidedAt); err != nil {
			return nil, fmt.Errorf("scan disposition: %w", err)
		}
		dispositions = append(dispositions, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dispositions: %w", err)
	}
	return dispositions, nil
}

// CampaignAttemptStats aggregates attempt and finding data for report generation.
type CampaignAttemptStats struct {
	CohortSize         int
	CompletedSamples   int
	FailedSamples      int
	UnavailableSamples int
	Candidates         map[string]*CandidateStats
	AllFindings        map[string]*StoredFinding
}

// CandidateStats aggregates findings per candidate.
type CandidateStats struct {
	CandidateID       string
	ModelID           string
	ModelRevision     string
	AdapterName       string
	Runtime           string
	CompletedAttempts int
	FailedAttempts    int
	ActiveAttempts    int
	AverageDurationMs *int
	TotalUsageTokens  int
	Attempts          []*EvaluationAttempt
}

// StoredFinding represents a finding with its attempt context.
type StoredFinding struct {
	FindingID     string
	AttemptID     string
	CandidateID   string
	SampleID      string
	Severity      string
	Claim         string
	Summary       string
	Evidence      string
	ExitClass     *EvaluationExitClass
	Status        *string
	Disposition   *string
	DispositionEv string
}

// ListCampaignAttempts returns all attempts for a campaign with findings grouped by sample and candidate.
func (s *sqliteStore) ListCampaignAttempts(ctx context.Context, campaignID string) (CampaignAttemptStats, error) {
	stats := CampaignAttemptStats{
		Candidates:  make(map[string]*CandidateStats),
		AllFindings: make(map[string]*StoredFinding),
	}

	// Get sample count.
	var sampleCount int
	err := s.readConn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_sample WHERE campaign_id = ?`, campaignID).Scan(&sampleCount)
	if err != nil && err != sql.ErrNoRows {
		return stats, fmt.Errorf("count samples: %w", err)
	}
	stats.CohortSize = sampleCount

	// Get all candidates for this campaign.
	candRows, err := s.readConn.QueryContext(ctx,
		`SELECT id, identity_json FROM evaluation_candidate WHERE campaign_id = ?`, campaignID)
	if err != nil {
		return stats, fmt.Errorf("list candidates: %w", err)
	}
	defer candRows.Close()

	for candRows.Next() {
		var cID, identityJSON string
		if err := candRows.Scan(&cID, &identityJSON); err != nil {
			return stats, fmt.Errorf("scan candidate: %w", err)
		}
		cs := &CandidateStats{
			CandidateID: cID,
			Attempts:    []*EvaluationAttempt{},
		}
		var identity map[string]interface{}
		if err := json.Unmarshal([]byte(identityJSON), &identity); err == nil {
			if modelID, ok := identity["model_id"].(string); ok {
				cs.ModelID = modelID
			}
			if adapter, ok := identity["adapter_name"].(string); ok {
				cs.AdapterName = adapter
			}
			if runtime, ok := identity["runtime"].(string); ok {
				cs.Runtime = runtime
			}
		}
		stats.Candidates[cID] = cs
	}
	if err := candRows.Err(); err != nil {
		return stats, fmt.Errorf("iterate candidates: %w", err)
	}

	// Get all attempts for this campaign.
	attemptRows, err := s.readConn.QueryContext(ctx, `
		SELECT ea.id, ea.job_id, ea.state, ea.exit_class, ea.status, ea.duration_ms, ea.usage_tokens,
		       ej.sample_id, ej.candidate_id
		FROM evaluation_attempt ea
		JOIN evaluation_job ej ON ea.job_id = ej.id
		JOIN evaluation_sample es ON ej.sample_id = es.id
		WHERE es.campaign_id = ?
		ORDER BY ea.started_at
	`, campaignID)
	if err != nil {
		return stats, fmt.Errorf("list attempts: %w", err)
	}
	defer attemptRows.Close()

	attemptsByID := make(map[string]*EvaluationAttempt)
	for attemptRows.Next() {
		var attempt EvaluationAttempt
		var candidateID, sampleID string
		var exitClass, status sql.NullString
		var durationMs, usageTokens sql.NullInt64
		if err := attemptRows.Scan(&attempt.ID, &attempt.JobID, &attempt.State, &exitClass, &status, &durationMs, &usageTokens,
			&sampleID, &candidateID); err != nil {
			return stats, fmt.Errorf("scan attempt: %w", err)
		}
		if exitClass.Valid {
			ec := EvaluationExitClass(exitClass.String)
			attempt.ExitClass = &ec
		}
		if status.Valid {
			attempt.Status = &status.String
		}
		if durationMs.Valid {
			d := int(durationMs.Int64)
			attempt.DurationMs = &d
		}
		if usageTokens.Valid {
			d := int(usageTokens.Int64)
			attempt.UsageTokens = &d
		}
		attemptsByID[attempt.ID] = &attempt

		// Update candidate stats.
		if cand, ok := stats.Candidates[candidateID]; ok {
			cand.Attempts = append(cand.Attempts, &attempt)
			if ec := attempt.ExitClass; ec != nil {
				switch *ec {
				case EvalExitCompleted:
					cand.CompletedAttempts++
				case EvalExitFailed:
					cand.FailedAttempts++
				case EvalExitUnavailableSnapshot, EvalExitUnavailableSource:
					stats.UnavailableSamples++
				}
			}
			if attempt.DurationMs != nil && *attempt.DurationMs > 0 {
				if cand.AverageDurationMs == nil {
					d := *attempt.DurationMs
					cand.AverageDurationMs = &d
				} else {
					*cand.AverageDurationMs = (*cand.AverageDurationMs + *attempt.DurationMs) / 2
				}
			}
			if attempt.UsageTokens != nil {
				cand.TotalUsageTokens += *attempt.UsageTokens
			}
		}
	}
	if err := attemptRows.Err(); err != nil {
		return stats, fmt.Errorf("iterate attempts: %w", err)
	}

	// Get all findings.
	findingRows, err := s.readConn.QueryContext(ctx, `
		SELECT ef.finding_id, ef.attempt_id, ef.severity, ef.claim, ef.summary, ef.evidence,
		       ej.sample_id, ej.candidate_id
		FROM evaluation_finding ef
		JOIN evaluation_attempt ea ON ef.attempt_id = ea.id
		JOIN evaluation_job ej ON ea.job_id = ej.id
		JOIN evaluation_sample es ON ej.sample_id = es.id
		WHERE es.campaign_id = ?
	`, campaignID)
	if err != nil {
		return stats, fmt.Errorf("list findings: %w", err)
	}
	defer findingRows.Close()

	for findingRows.Next() {
		var finding StoredFinding
		var sampleID, candidateID string
		var claim, evidence sql.NullString
		if err := findingRows.Scan(&finding.FindingID, &finding.AttemptID, &finding.Severity,
			&claim, &finding.Summary, &evidence, &sampleID, &candidateID); err != nil {
			return stats, fmt.Errorf("scan finding: %w", err)
		}
		if claim.Valid {
			finding.Claim = claim.String
		}
		if evidence.Valid {
			finding.Evidence = evidence.String
		}
		finding.SampleID = sampleID
		finding.CandidateID = candidateID
		if attempt, ok := attemptsByID[finding.AttemptID]; ok {
			finding.ExitClass = attempt.ExitClass
			finding.Status = attempt.Status
		}
		key := fmt.Sprintf("%s-%s-%s", sampleID, candidateID, finding.FindingID)
		stats.AllFindings[key] = &finding
	}
	if err := findingRows.Err(); err != nil {
		return stats, fmt.Errorf("iterate findings: %w", err)
	}

	return stats, nil
}
