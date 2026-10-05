package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/boldfield/odonian/internal/evaluation"
)

// ErrEvaluationFindingNotFound means a disposition named a finding that no
// completed attempt or production review of the campaign recorded.
var ErrEvaluationFindingNotFound = errors.New("evaluation finding not found")

const (
	maxDispositionClaim    = 200
	maxDispositionEvidence = 4000
	maxDispositionActor    = 200
)

// EvaluationDispositionInput is one operator decision about one finding. Ref
// names the finding as the report prints it: candidate:<attempt id>:<finding
// id> or baseline:<review task id>:<finding id>. The sample and candidate come
// from the finding itself, so a label can never be attached to the wrong one.
type EvaluationDispositionInput struct {
	CampaignID string
	Ref        string
	Label      evaluation.FindingLabel
	Severity   string
	Claim      string
	Evidence   string
	Actor      string
}

func (in EvaluationDispositionInput) validate() (claim string, err error) {
	if in.CampaignID == "" {
		return "", fmt.Errorf("%w: campaign id is required", ErrEvaluationInvalidInput)
	}
	if !in.Label.Valid() {
		return "", fmt.Errorf("%w: label %q must be valid, invalid or unresolved", ErrEvaluationInvalidInput, in.Label)
	}
	if !evaluation.ValidAdjudicatedSeverity(in.Severity) {
		return "", fmt.Errorf("%w: severity %q must be P1, P2 or P3", ErrEvaluationInvalidInput, in.Severity)
	}
	claim = evaluation.NormalizeClaim(in.Claim)
	switch {
	case claim == "" || utf8.RuneCountInString(claim) > maxDispositionClaim:
		return "", fmt.Errorf("%w: claim is required (at most %d characters)", ErrEvaluationInvalidInput, maxDispositionClaim)
	case strings.TrimSpace(in.Evidence) == "" || utf8.RuneCountInString(in.Evidence) > maxDispositionEvidence:
		return "", fmt.Errorf("%w: evidence is required (at most %d characters)", ErrEvaluationInvalidInput, maxDispositionEvidence)
	case strings.TrimSpace(in.Actor) == "" || utf8.RuneCountInString(in.Actor) > maxDispositionActor:
		return "", fmt.Errorf("%w: actor is required (at most %d characters)", ErrEvaluationInvalidInput, maxDispositionActor)
	}
	return claim, nil
}

// RecordEvaluationDisposition appends one decision. It checks that the finding
// exists, that it belongs to the campaign, and which sample and candidate it
// is on, then never edits or removes the row: a revised label is a new row and
// the latest one wins in the report. It changes no task, review or message.
func (s *sqliteStore) RecordEvaluationDisposition(ctx context.Context, in EvaluationDispositionInput) (evaluation.Disposition, error) {
	claim, err := in.validate()
	if err != nil {
		return evaluation.Disposition{}, err
	}
	kind, sourceID, findingID, err := evaluation.ParseFindingRef(in.Ref)
	if err != nil {
		return evaluation.Disposition{}, fmt.Errorf("%w: %v", ErrEvaluationInvalidInput, err)
	}
	if _, err := s.GetEvaluationCampaign(ctx, in.CampaignID); err != nil {
		return evaluation.Disposition{}, err
	}

	var sampleID, candidateID string
	if kind == evaluation.SourceCandidate {
		sampleID, candidateID, err = s.locateCandidateFinding(ctx, in.CampaignID, sourceID, findingID)
	} else {
		sampleID, err = s.locateBaselineFinding(ctx, in.CampaignID, sourceID, findingID)
	}
	if err != nil {
		return evaluation.Disposition{}, err
	}

	d := evaluation.Disposition{
		ID: GenerateID(), SampleID: sampleID, SourceKind: kind, SourceID: sourceID, CandidateID: candidateID,
		FindingID: findingID, Label: in.Label, Severity: in.Severity, Claim: claim, Evidence: in.Evidence,
		Actor: strings.TrimSpace(in.Actor), CreatedAt: formatTS(s.Now()),
	}
	_, err = s.conn.ExecContext(ctx,
		`INSERT INTO evaluation_finding_disposition
		   (id, campaign_id, sample_id, source_kind, source_id, candidate_id, finding_id, label, severity, claim, evidence, actor, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, in.CampaignID, d.SampleID, d.SourceKind, d.SourceID, nullableString(d.CandidateID), d.FindingID,
		string(d.Label), d.Severity, d.Claim, d.Evidence, d.Actor, d.CreatedAt)
	if err != nil {
		return evaluation.Disposition{}, fmt.Errorf("record disposition: %w", err)
	}
	return d, nil
}

func (s *sqliteStore) locateCandidateFinding(ctx context.Context, campaignID, attemptID, findingID string) (sampleID, candidateID string, err error) {
	var campaign, exit, state string
	err = s.conn.QueryRowContext(ctx,
		`SELECT es.campaign_id, es.id, ej.candidate_id, ea.state, COALESCE(ea.exit_class, '')
		 FROM evaluation_attempt ea
		 JOIN evaluation_job ej ON ej.id = ea.job_id
		 JOIN evaluation_sample es ON es.id = ej.sample_id
		 WHERE ea.id = ?`, attemptID).Scan(&campaign, &sampleID, &candidateID, &state, &exit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("%w: no attempt %q", ErrEvaluationFindingNotFound, attemptID)
	}
	if err != nil {
		return "", "", fmt.Errorf("locate attempt: %w", err)
	}
	if campaign != campaignID {
		return "", "", fmt.Errorf("%w: attempt %q belongs to another campaign", ErrEvaluationInvalidInput, attemptID)
	}
	var n int
	if err := s.conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_finding WHERE attempt_id = ? AND finding_id = ?`, attemptID, findingID).Scan(&n); err != nil {
		return "", "", fmt.Errorf("locate finding: %w", err)
	}
	if n == 0 {
		return "", "", fmt.Errorf("%w: attempt %q recorded no finding %q", ErrEvaluationFindingNotFound, attemptID, findingID)
	}
	return sampleID, candidateID, nil
}

func (s *sqliteStore) locateBaselineFinding(ctx context.Context, campaignID, reviewTaskID, findingID string) (string, error) {
	var target sql.NullString
	var round sql.NullInt64
	err := s.conn.QueryRowContext(ctx,
		`SELECT target_task_id, review_round FROM task WHERE id = ? AND kind = 'review' AND adjudicate_finding_id IS NULL`,
		reviewTaskID).Scan(&target, &round)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (!target.Valid || !round.Valid)) {
		return "", fmt.Errorf("%w: no production review task %q", ErrEvaluationFindingNotFound, reviewTaskID)
	}
	if err != nil {
		return "", fmt.Errorf("locate review task: %w", err)
	}
	var sampleID string
	err = s.conn.QueryRowContext(ctx,
		`SELECT id FROM evaluation_sample WHERE campaign_id = ? AND original_task_id = ? AND original_review_round = ?`,
		campaignID, target.String, round.Int64).Scan(&sampleID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: review task %q is not a review of any sample of this campaign", ErrEvaluationInvalidInput, reviewTaskID)
	}
	if err != nil {
		return "", fmt.Errorf("locate sample: %w", err)
	}
	reviews, err := s.baselineReviews(ctx, target.String, int(round.Int64))
	if err != nil {
		return "", err
	}
	for _, r := range reviews {
		if r.TaskID != reviewTaskID {
			continue
		}
		for _, f := range r.Findings {
			if f.ID == findingID {
				return sampleID, nil
			}
		}
	}
	return "", fmt.Errorf("%w: review task %q recorded no finding %q", ErrEvaluationFindingNotFound, reviewTaskID, findingID)
}

// ListEvaluationDispositions returns every recorded decision of a campaign,
// oldest first, so an auditor sees each revision.
func (s *sqliteStore) ListEvaluationDispositions(ctx context.Context, campaignID string) ([]evaluation.Disposition, error) {
	if _, err := s.GetEvaluationCampaign(ctx, campaignID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx,
		`SELECT id, sample_id, source_kind, source_id, COALESCE(candidate_id, ''), finding_id, label, severity, claim, evidence, actor, created_at
		 FROM evaluation_finding_disposition WHERE campaign_id = ? ORDER BY rowid`, campaignID)
	if err != nil {
		return nil, fmt.Errorf("list dispositions: %w", err)
	}
	defer rows.Close()
	out := []evaluation.Disposition{}
	for rows.Next() {
		var d evaluation.Disposition
		var label string
		if err := rows.Scan(&d.ID, &d.SampleID, &d.SourceKind, &d.SourceID, &d.CandidateID, &d.FindingID,
			&label, &d.Severity, &d.Claim, &d.Evidence, &d.Actor, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan disposition: %w", err)
		}
		d.Label = evaluation.FindingLabel(label)
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetEvaluationReport assembles the campaign's evaluation report from the
// frozen samples, the candidates' recorded attempts, the production review
// findings of each sample's exact round and the operator dispositions. It only
// reads: no task, review, message or evaluation record changes.
func (s *sqliteStore) GetEvaluationReport(ctx context.Context, campaignID string) (evaluation.Report, error) {
	campaign, err := s.GetEvaluationCampaign(ctx, campaignID)
	if err != nil {
		return evaluation.Report{}, err
	}
	samples, err := s.ListEvaluationSamples(ctx, campaignID)
	if err != nil {
		return evaluation.Report{}, err
	}
	candidates, err := s.ListEvaluationCandidates(ctx, campaignID)
	if err != nil {
		return evaluation.Report{}, err
	}
	stagings, err := s.ListEvaluationStagings(ctx, campaignID)
	if err != nil {
		return evaluation.Report{}, err
	}
	staged := map[string]EvaluationStaging{}
	for _, st := range stagings {
		staged[st.CandidateID+"\x00"+st.SampleID] = st
	}

	in := evaluation.ReportInput{CampaignID: campaign.ID, CampaignName: campaign.Name, GeneratedAt: s.Now()}
	for _, sm := range samples {
		in.Samples = append(in.Samples, evaluation.ReportSample{
			ID: sm.ID, TaskID: sm.OriginalTaskID, Round: sm.OriginalReviewRound, SHA: sm.SubmittedSHA,
			SnapshotDigest: derefString(sm.SnapshotDigest),
		})
	}
	for _, c := range candidates {
		in.Candidates = append(in.Candidates, evaluation.ReportCandidate{ID: c.ID, Digest: c.Digest(), Identity: c.Config.Identity()})
		for _, sm := range samples {
			run, err := s.reportRun(ctx, c.ID, sm.ID, staged[c.ID+"\x00"+sm.ID])
			if err != nil {
				return evaluation.Report{}, err
			}
			if len(run.Attempts) > 0 {
				in.Runs = append(in.Runs, run)
			}
		}
	}
	for _, sm := range samples {
		b, err := s.baselineForSample(ctx, sm)
		if err != nil {
			return evaluation.Report{}, err
		}
		if len(b.Reviews) > 0 {
			in.Baselines = append(in.Baselines, b)
		}
	}
	if in.Dispositions, err = s.ListEvaluationDispositions(ctx, campaignID); err != nil {
		return evaluation.Report{}, err
	}
	return evaluation.BuildReport(in), nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// reportRun reads every attempt of one candidate on one sample. An attempt
// whose lease has run out but was never finalized is reported as expired, the
// same as the store would record it, without writing anything.
func (s *sqliteStore) reportRun(ctx context.Context, candidateID, sampleID string, staging EvaluationStaging) (evaluation.ReportRun, error) {
	run := evaluation.ReportRun{CandidateID: candidateID, SampleID: sampleID}
	if staging.ID != "" {
		run.Staged = true
		run.StagedSnapshotDigest, run.StagedCandidateDigest = staging.SnapshotDigest, staging.CandidateConfigDigest
	}
	rows, err := s.conn.QueryContext(ctx,
		`SELECT ea.id FROM evaluation_attempt ea JOIN evaluation_job ej ON ej.id = ea.job_id
		 WHERE ej.sample_id = ? AND ej.candidate_id = ? ORDER BY ea.sequence_number, ea.started_at`, sampleID, candidateID)
	if err != nil {
		return run, fmt.Errorf("list attempts: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return run, fmt.Errorf("scan attempt id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return run, err
	}

	now := s.Now().UTC()
	for _, id := range ids {
		a, err := s.fetchEvaluationAttempt(ctx, s.conn, id)
		if err != nil {
			return run, err
		}
		ra := evaluation.ReportAttempt{ID: a.ID, Sequence: a.SequenceNumber, State: a.State, DurationMs: a.DurationMs}
		if a.ExitClass != nil {
			ra.ExitClass = string(*a.ExitClass)
		}
		if a.State == EvalAttemptActive {
			expires, err := parseTS(a.ExpiresAt)
			if err != nil {
				return run, fmt.Errorf("attempt %s lease: %w", a.ID, err)
			}
			if leaseExpired(now, expires) {
				ra.State, ra.ExitClass = evaluation.EvalAttemptExpired, string(EvalExitLeaseExpired)
			}
		}
		detail, err := s.GetEvaluationAttemptDetail(ctx, id)
		if err != nil {
			return run, err
		}
		if detail != nil {
			ra.Usage, ra.EffectiveDigest = detail.Usage, detail.EffectiveDigest
		}
		if a.State == EvalAttemptFinalized && ra.ExitClass == string(EvalExitCompleted) {
			if ra.Findings, err = s.ListEvaluationFindings(ctx, id); err != nil {
				return run, err
			}
		}
		run.Attempts = append(run.Attempts, ra)
	}
	return run, nil
}

// baselineReviews reads the production reviews of exactly one round of a task:
// each review task of that round, its reviewer model, and the findings it
// recorded. Later rounds' reviews are never read.
func (s *sqliteStore) baselineReviews(ctx context.Context, taskID string, round int) ([]evaluation.BaselineReview, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id, COALESCE(model, '') FROM task
		WHERE target_task_id = ? AND kind = 'review' AND review_round = ? AND adjudicate_finding_id IS NULL
		ORDER BY rowid`, taskID, round)
	if err != nil {
		return nil, fmt.Errorf("list review tasks: %w", err)
	}
	type rt struct{ id, model string }
	var tasks []rt
	for rows.Next() {
		var t rt
		if err := rows.Scan(&t.id, &t.model); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan review task: %w", err)
		}
		tasks = append(tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	events, err := s.reviewEventsByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var out []evaluation.BaselineReview
	for _, t := range tasks {
		r := evaluation.BaselineReview{TaskID: t.id, Reviewer: t.model}
		if r.Reviewer == "" {
			r.Reviewer = evaluation.Unknown
		}
		if ev, ok := events[t.id]; ok && ev.findings.Valid {
			var findings []Finding
			if json.Unmarshal([]byte(ev.findings.String), &findings) == nil {
				r.Complete = true
				for _, f := range findings {
					r.Findings = append(r.Findings, evaluation.BaselineFinding{
						ID: f.ID, Severity: f.Severity, File: f.File, Line: f.Line, Summary: f.Summary,
					})
				}
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// baselineForSample reads the production reviews of the sample's exact round
// and the one commit that round pinned. A later round's reviews and commits are
// never read, so corrected work cannot stand in for the frozen artifact.
func (s *sqliteStore) baselineForSample(ctx context.Context, sm EvaluationSample) (evaluation.BaselineSampleInput, error) {
	b := evaluation.BaselineSampleInput{SampleID: sm.ID}
	reviews, err := s.baselineReviews(ctx, sm.OriginalTaskID, sm.OriginalReviewRound)
	if err != nil || len(reviews) == 0 {
		return b, err
	}
	b.Reviews = reviews

	rows, err := s.conn.QueryContext(ctx, `
		SELECT kind, value, review_round FROM task_link
		WHERE task_id = ? AND kind IN ('pr', 'branch', 'commit', 'ci') ORDER BY rowid`, sm.OriginalTaskID)
	if err != nil {
		return b, fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()
	var links []evaluation.RecordedLink
	untagged := 0
	for rows.Next() {
		var kind, value string
		var round sql.NullInt64
		if err := rows.Scan(&kind, &value, &round); err != nil {
			return b, fmt.Errorf("scan link: %w", err)
		}
		switch {
		case !round.Valid:
			untagged++
		case int(round.Int64) == sm.OriginalReviewRound:
			links = append(links, evaluation.RecordedLink{Kind: kind, Value: value})
		}
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	sha, _, detail := evaluation.ResolveSubmittedCommit(links, untagged)
	b.PinnedSHA, b.PinProblem = sha, detail
	return b, nil
}
