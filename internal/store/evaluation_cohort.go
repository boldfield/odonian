package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/boldfield/odonian/internal/evaluation"
)

var (
	ErrEvaluationStagingNotFound = errors.New("evaluation staging not found")
	// ErrFirstRoundIneligible means a task is not an eligible first-round sample.
	ErrFirstRoundIneligible = errors.New("task is not an eligible first-round sample")
)

// EvaluationStaging records that one candidate staged its own fresh workspace
// for one sample, with the digests it was verified against and the
// candidate-specific source and tool limits (LimitsJSON). It holds no content.
type EvaluationStaging struct {
	ID                    string
	CampaignID            string
	SampleID              string
	CandidateID           string
	CandidateConfigDigest string
	SnapshotDigest        string
	SourceDigest          string
	PromptVersion         string
	PromptDigest          string
	LimitsJSON            string
	StagedAt              string
}

func (e EvaluationStaging) sameContent(o EvaluationStaging) bool {
	return e.CandidateConfigDigest == o.CandidateConfigDigest && e.SnapshotDigest == o.SnapshotDigest &&
		e.SourceDigest == o.SourceDigest && e.PromptVersion == o.PromptVersion &&
		e.PromptDigest == o.PromptDigest && e.LimitsJSON == o.LimitsJSON
}

const stagingColumns = `id, campaign_id, sample_id, candidate_id, candidate_config_digest, snapshot_digest,
	source_digest, prompt_version, prompt_digest, limits_json, staged_at`

func scanEvaluationStaging(row interface{ Scan(...any) error }) (EvaluationStaging, error) {
	var e EvaluationStaging
	err := row.Scan(&e.ID, &e.CampaignID, &e.SampleID, &e.CandidateID, &e.CandidateConfigDigest, &e.SnapshotDigest,
		&e.SourceDigest, &e.PromptVersion, &e.PromptDigest, &e.LimitsJSON, &e.StagedAt)
	return e, err
}

// RecordEvaluationStaging persists a candidate's staging of a sample. The
// staging must agree with what is frozen: the sample must belong to the
// campaign and carry a snapshot digest equal to the staged one, and the
// candidate must belong to the campaign and carry the recorded config digest.
// Recording the identical staging again returns the existing row; a different
// one for the same (campaign, sample, candidate) is ErrEvaluationAlreadyExists.
func (s *sqliteStore) RecordEvaluationStaging(ctx context.Context, st EvaluationStaging) (EvaluationStaging, error) {
	if st.ID == "" || st.CampaignID == "" || st.SampleID == "" || st.CandidateID == "" ||
		st.CandidateConfigDigest == "" || st.SnapshotDigest == "" || st.SourceDigest == "" ||
		st.PromptVersion == "" || st.PromptDigest == "" || !json.Valid([]byte(st.LimitsJSON)) {
		return EvaluationStaging{}, ErrEvaluationInvalidInput
	}
	sample, err := s.GetEvaluationSample(ctx, st.SampleID)
	if err != nil {
		return EvaluationStaging{}, err
	}
	if sample.CampaignID != st.CampaignID {
		return EvaluationStaging{}, fmt.Errorf("%w: sample belongs to another campaign", ErrEvaluationInvalidInput)
	}
	if sample.SnapshotDigest == nil || *sample.SnapshotDigest != st.SnapshotDigest {
		return EvaluationStaging{}, fmt.Errorf("%w: staged snapshot digest differs from the frozen sample", ErrEvaluationInvalidInput)
	}
	if sample.SourceDigest == nil || *sample.SourceDigest != st.SourceDigest {
		return EvaluationStaging{}, fmt.Errorf("%w: staged source digest differs from the frozen sample", ErrEvaluationInvalidInput)
	}
	cand, err := s.GetEvaluationCandidate(ctx, st.CandidateID)
	if err != nil {
		return EvaluationStaging{}, err
	}
	if cand.CampaignID != st.CampaignID {
		return EvaluationStaging{}, fmt.Errorf("%w: candidate belongs to another campaign", ErrEvaluationInvalidInput)
	}
	if cand.Digest() != st.CandidateConfigDigest {
		return EvaluationStaging{}, fmt.Errorf("%w: candidate config digest differs from the stored candidate", ErrEvaluationInvalidInput)
	}

	st.StagedAt = s.Now().UTC().Format(timestampLayout)
	_, err = s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_staging (`+stagingColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		st.ID, st.CampaignID, st.SampleID, st.CandidateID, st.CandidateConfigDigest, st.SnapshotDigest,
		st.SourceDigest, st.PromptVersion, st.PromptDigest, st.LimitsJSON, st.StagedAt)
	if isUniqueViolation(err) {
		existing, gerr := s.GetEvaluationStaging(ctx, st.CampaignID, st.SampleID, st.CandidateID)
		if gerr == nil && existing.sameContent(st) {
			return existing, nil
		}
		return EvaluationStaging{}, ErrEvaluationAlreadyExists
	}
	if err != nil {
		return EvaluationStaging{}, fmt.Errorf("record evaluation staging: %w", err)
	}
	return st, nil
}

// GetEvaluationStaging returns one candidate's staging record for a sample.
func (s *sqliteStore) GetEvaluationStaging(ctx context.Context, campaignID, sampleID, candidateID string) (EvaluationStaging, error) {
	e, err := scanEvaluationStaging(s.conn.QueryRowContext(ctx,
		`SELECT `+stagingColumns+` FROM evaluation_staging WHERE campaign_id = ? AND sample_id = ? AND candidate_id = ?`,
		campaignID, sampleID, candidateID))
	if errors.Is(err, sql.ErrNoRows) {
		return EvaluationStaging{}, ErrEvaluationStagingNotFound
	}
	if err != nil {
		return EvaluationStaging{}, fmt.Errorf("get evaluation staging: %w", err)
	}
	return e, nil
}

// ListEvaluationStagings lists a campaign's staging records, oldest first.
func (s *sqliteStore) ListEvaluationStagings(ctx context.Context, campaignID string) ([]EvaluationStaging, error) {
	if _, err := s.GetEvaluationCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx,
		`SELECT `+stagingColumns+` FROM evaluation_staging WHERE campaign_id = ? ORDER BY staged_at, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list evaluation staging: %w", err)
	}
	defer rows.Close()
	var out []EvaluationStaging
	for rows.Next() {
		e, err := scanEvaluationStaging(rows)
		if err != nil {
			return nil, fmt.Errorf("scan evaluation staging: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEvaluationSamples lists a campaign's frozen samples in creation order.
func (s *sqliteStore) ListEvaluationSamples(ctx context.Context, campaignID string) ([]EvaluationSample, error) {
	if _, err := s.GetEvaluationCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx, `SELECT id FROM evaluation_sample WHERE campaign_id = ? ORDER BY created_at, id`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list evaluation samples: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan evaluation sample id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]EvaluationSample, 0, len(ids))
	for _, id := range ids {
		sm, err := s.GetEvaluationSample(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, nil
}

// FirstRoundCensus examines every implement task of the given projects and
// returns each eligible first-round submission with its outcome and sealed
// material, plus a count of every task that was examined but is not eligible.
// It reads only the board's own records.
func (s *sqliteStore) FirstRoundCensus(ctx context.Context, projectIDs []string) (evaluation.FirstRoundCensus, error) {
	census := evaluation.FirstRoundCensus{Excluded: map[string]int{}}
	if len(projectIDs) == 0 {
		return census, fmt.Errorf("%w: at least one project is required", ErrEvaluationInvalidInput)
	}
	for _, pid := range projectIDs {
		var n int
		if err := s.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM project WHERE id = ?`, pid).Scan(&n); err != nil {
			return census, fmt.Errorf("check project: %w", err)
		}
		if n == 0 {
			return census, fmt.Errorf("%w: project %q does not exist", ErrEvaluationInvalidInput, pid)
		}
		rows, err := s.conn.QueryContext(ctx, `SELECT id FROM task WHERE project_id = ? AND kind = 'implement' ORDER BY id`, pid)
		if err != nil {
			return census, fmt.Errorf("list tasks: %w", err)
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return census, fmt.Errorf("scan task id: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return census, err
		}
		for _, id := range ids {
			census.Examined++
			rec, bucket, err := s.firstRoundRecord(ctx, id)
			if err != nil {
				return census, err
			}
			if bucket != "" {
				census.Excluded[bucket]++
				continue
			}
			census.Records = append(census.Records, rec)
		}
	}
	return census, nil
}

// GetFirstRoundRecord re-reads one task's first-round record, for staging from
// a frozen cohort. A task that is not an eligible first-round sample returns
// ErrFirstRoundIneligible naming why.
func (s *sqliteStore) GetFirstRoundRecord(ctx context.Context, taskID string) (evaluation.FirstRoundRecord, error) {
	rec, bucket, err := s.firstRoundRecord(ctx, taskID)
	if err != nil {
		return evaluation.FirstRoundRecord{}, err
	}
	if bucket != "" {
		return evaluation.FirstRoundRecord{}, fmt.Errorf("%w: %s", ErrFirstRoundIneligible, bucket)
	}
	return rec, nil
}

func (s *sqliteStore) firstRoundRecord(ctx context.Context, taskID string) (evaluation.FirstRoundRecord, string, error) {
	var rec evaluation.FirstRoundRecord
	var track, kind string
	err := s.conn.QueryRowContext(ctx, `SELECT project_id, spec, track, kind FROM task WHERE id = ?`, taskID).
		Scan(&rec.ProjectID, &rec.Spec, &track, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return rec, "", ErrNotFound
	}
	if err != nil {
		return rec, "", fmt.Errorf("read task: %w", err)
	}
	rec.TaskID, rec.ReviewRound = taskID, 1
	switch {
	case kind != "implement" || track != "research":
		return rec, evaluation.ExcludedNotResearch, nil
	case strings.Contains(rec.Spec, researchHistorySentinel):
		return rec, evaluation.ExcludedReplacement, nil
	}

	reviewTasks, err := s.roundOneReviewTasks(ctx, taskID)
	if err != nil {
		return rec, "", err
	}
	if len(reviewTasks) == 0 {
		return rec, evaluation.ExcludedNotSubmitted, nil
	}

	events, err := s.reviewEventsByTask(ctx, taskID)
	if err != nil {
		return rec, "", err
	}
	var p1, p2 int
	for _, id := range reviewTasks {
		ev, ok := events[id]
		if !ok || !ev.findings.Valid {
			return rec, evaluation.ExcludedRoundOneIncomplete, nil
		}
		var findings []Finding
		if err := json.Unmarshal([]byte(ev.findings.String), &findings); err != nil {
			return rec, evaluation.ExcludedRoundOneIncomplete, nil
		}
		for _, f := range findings {
			switch f.Severity {
			case "P1":
				p1++
			case "P2":
				p2++
			}
		}
	}
	if p1+p2 > 0 {
		rec.Outcome = evaluation.OutcomeRejectedMaterial
		rec.OutcomeReason = fmt.Sprintf("%d P1 and %d P2 blocking finding(s) across %d round-1 reviewer(s)", p1, p2, len(reviewTasks))
	} else {
		rec.Outcome = evaluation.OutcomeClean
		rec.OutcomeReason = fmt.Sprintf("no P1/P2 finding across %d round-1 reviewer(s)", len(reviewTasks))
	}

	if err := s.collectSealed(ctx, taskID, &rec); err != nil {
		return rec, "", err
	}
	if err := s.collectRoundOneSubmission(ctx, taskID, &rec); err != nil {
		return rec, "", err
	}
	return rec, "", nil
}

func (s *sqliteStore) roundOneReviewTasks(ctx context.Context, taskID string) ([]string, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id FROM task
		WHERE target_task_id = ? AND kind = 'review' AND review_round = 1 AND adjudicate_finding_id IS NULL
		ORDER BY rowid`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list round-1 review tasks: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan review task: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

type reviewEvent struct {
	findings sql.NullString
	note     sql.NullString
}

// reviewEventsByTask keys review events by the review task that produced
// them; the first event per review task wins, as in the production
// aggregation.
func (s *sqliteStore) reviewEventsByTask(ctx context.Context, taskID string) (map[string]reviewEvent, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT source_task_id, findings, note FROM event
		WHERE task_id = ? AND kind = 'review' AND source_task_id IS NOT NULL
		ORDER BY rowid`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list review events: %w", err)
	}
	defer rows.Close()
	out := map[string]reviewEvent{}
	for rows.Next() {
		var src string
		var ev reviewEvent
		if err := rows.Scan(&src, &ev.findings, &ev.note); err != nil {
			return nil, fmt.Errorf("scan review event: %w", err)
		}
		if _, seen := out[src]; !seen {
			out[src] = ev
		}
	}
	return out, rows.Err()
}

// collectSealed gathers what the host holds that a reviewer must never see:
// every review event's note and findings, the notes of rework submissions
// (every submit event after the first), later-round commits and every PR link.
func (s *sqliteStore) collectSealed(ctx context.Context, taskID string, rec *evaluation.FirstRoundRecord) error {
	add := func(label, text string) {
		if strings.TrimSpace(text) != "" {
			rec.Sealed = append(rec.Sealed, evaluation.SealedString{Label: label, Text: text})
		}
	}
	// task.result holds only the most recent submission note; once the task is
	// past round 1 it describes the revised work, not the round-1 artifact.
	var result string
	var round int
	if err := s.conn.QueryRowContext(ctx, `SELECT COALESCE(result, ''), review_round FROM task WHERE id = ?`, taskID).Scan(&result, &round); err != nil {
		return fmt.Errorf("read task result: %w", err)
	}
	if round > 1 {
		add("later submission note", result)
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT kind, COALESCE(note, ''), COALESCE(findings, '') FROM event
		WHERE task_id = ? ORDER BY rowid`, taskID)
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()
	submits := 0
	for rows.Next() {
		var kind, note, findings string
		if err := rows.Scan(&kind, &note, &findings); err != nil {
			return fmt.Errorf("scan event: %w", err)
		}
		if kind == "submit" {
			if submits++; submits == 1 {
				continue
			}
		}
		add("event note ("+kind+")", note)
		if findings != "" {
			var fs []Finding
			if json.Unmarshal([]byte(findings), &fs) == nil {
				for _, f := range fs {
					add("reviewer finding", f.Summary)
				}
			}
		}
	}
	return rows.Err()
}

// collectRoundOneSubmission records the links and manifest of review round 1.
// Every link outside round 1, and every PR link, is sealed.
func (s *sqliteStore) collectRoundOneSubmission(ctx context.Context, taskID string, rec *evaluation.FirstRoundRecord) error {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT kind, value, review_round FROM task_link
		WHERE task_id = ? AND kind IN ('pr', 'branch', 'commit', 'ci') ORDER BY rowid`, taskID)
	if err != nil {
		return fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, value string
		var round sql.NullInt64
		if err := rows.Scan(&kind, &value, &round); err != nil {
			return fmt.Errorf("scan link: %w", err)
		}
		switch {
		case !round.Valid:
			rec.UntaggedLinks++
		case round.Int64 == 1:
			rec.RoundLinks = append(rec.RoundLinks, evaluation.RecordedLink{Kind: kind, Value: value})
		}
		if kind == "pr" || (round.Valid && round.Int64 != 1) {
			rec.Sealed = append(rec.Sealed, evaluation.SealedString{Label: "link (" + kind + ")", Text: value})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var m evaluation.RecordedManifest
	var body string
	err = s.conn.QueryRowContext(ctx,
		`SELECT manifest_json, manifest_digest FROM task_submission_manifest WHERE task_id = ? AND review_round = 1`, taskID).
		Scan(&body, &m.Digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("read manifest: %w", err)
	default:
		m.JSON = []byte(body)
		rec.Manifest = &m
	}
	return nil
}
