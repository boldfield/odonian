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
