package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
)

type reportFixture struct {
	*cohortFixture
	campaign EvaluationCampaign
	candA    EvaluationCandidate
	candB    EvaluationCandidate
	samples  map[string]EvaluationSample
}

// newReportFixture seeds the board with real tasks and reviews, then freezes
// three samples of them: "rejected" (round 1 rejected by opus with a P1, later
// fixed and re-reviewed with a different finding), "clean" and "minor" whose
// frozen SHA deliberately differs from the commit its round pinned.
func newReportFixture(t *testing.T) *reportFixture {
	t.Helper()
	f := newCohortFixture(t)
	f.seed()
	rej := f.tasks["rejected"]
	// A round 2 review of the corrected work; it must never reach the report.
	f.review(rej, 2, "opus", "reject",
		`[{"id":"late","severity":"P2","file":"research.md","line":5,"summary":"Only the corrected draft has this problem","in_changed_text":true,"status":"new"}]`)

	configureTestEvaluationPool(t, f.st, "pool1")
	campaign := newPoolTestCampaign(t, f.st, 50, f.proj)
	r := &reportFixture{cohortFixture: f, campaign: campaign, samples: map[string]EvaluationSample{}}
	r.candA = newPoolTestCandidateStored(t, f.st, campaign, "fakeA", "pool1", 10)
	r.candB = newPoolTestCandidateStored(t, f.st, campaign, "fakeB", "pool1", 10)
	for _, s := range []struct{ name, sha string }{{"rejected", sha("c")}, {"clean", sha("a")}, {"minor", sha("9")}} {
		sm, err := f.st.CreateEvaluationSample(f.ctx, EvaluationSample{
			ID: GenerateID(), CampaignID: campaign.ID, ProjectID: f.proj, OriginalTaskID: f.tasks[s.name], OriginalReviewRound: 1,
			SubmittedSHA: s.sha, SnapshotDigest: strp("snap-" + s.name), SourceDigest: strp("src-" + s.name),
			PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
		})
		if err != nil {
			t.Fatal(err)
		}
		r.samples[s.name] = sm
	}
	return r
}

func (r *reportFixture) stage(cand EvaluationCandidate, sample EvaluationSample, snapshot string) {
	r.t.Helper()
	snap := sample.SnapshotDigest
	if snapshot != "" {
		snap = &snapshot
	}
	// RecordEvaluationStaging only accepts the frozen digest, so a staging whose
	// snapshot differs is written directly, the way a buggy stager could.
	_, err := r.st.Conn().ExecContext(r.ctx,
		`INSERT INTO evaluation_staging (id, campaign_id, sample_id, candidate_id, candidate_config_digest, snapshot_digest, source_digest,
		   prompt_version, prompt_digest, limits_json, staged_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'v1', 'pd', '{}', '2026-01-01T00:00:00Z')`,
		GenerateID(), r.campaign.ID, sample.ID, cand.ID, cand.Digest(), *snap, *sample.SourceDigest)
	if err != nil {
		r.t.Fatal(err)
	}
}

// run claims and finalizes one attempt and returns its id.
func (r *reportFixture) run(cand EvaluationCandidate, sample EvaluationSample, exit EvaluationExitClass, dur *int, usage map[string]float64, fs ...evaluation.Finding) string {
	r.t.Helper()
	c, err := claimEval(r.st, sample, cand, time.Minute)
	if err != nil {
		r.t.Fatal(err)
	}
	status := evaluation.StatusCompleted
	if exit != EvalExitCompleted {
		status = evaluation.StatusFailed
	}
	if err := r.st.FinalizeEvaluationAttempt(r.ctx, EvaluationAttemptResult{
		AttemptID: c.Attempt.ID, FenceAttemptID: c.Attempt.ID, ExitClass: exit, Status: &status, DurationMs: dur, Findings: fs,
		Detail: &EvaluationAttemptDetail{CandidateDigest: cand.Digest(), Launched: true, Usage: usage},
	}); err != nil {
		r.t.Fatal(err)
	}
	return c.Attempt.ID
}

func (r *reportFixture) reviewTask(task, model string, round int) string {
	r.t.Helper()
	var id string
	if err := r.st.Conn().QueryRow(`SELECT id FROM task WHERE target_task_id = ? AND kind = 'review' AND review_round = ? AND model = ?`,
		r.tasks[task], round, model).Scan(&id); err != nil {
		r.t.Fatalf("review task %s/%s/%d: %v", task, model, round, err)
	}
	return id
}

func (r *reportFixture) label(ref string, label evaluation.FindingLabel, sev, claim string) evaluation.Disposition {
	r.t.Helper()
	d, err := r.st.RecordEvaluationDisposition(r.ctx, EvaluationDispositionInput{
		CampaignID: r.campaign.ID, Ref: ref, Label: label, Severity: sev, Claim: claim, Evidence: "read the source at the pinned commit", Actor: "operator-1",
	})
	if err != nil {
		r.t.Fatalf("label %s: %v", ref, err)
	}
	return d
}

func reviewerRow(t *testing.T, rep evaluation.Report, key string) evaluation.ReviewerReport {
	t.Helper()
	for _, rr := range rep.Reviewers {
		if rr.Key == key {
			return rr
		}
	}
	t.Fatalf("no reviewer %q", key)
	return evaluation.ReviewerReport{}
}

func TestEvaluationReportFromBoardAndEvaluationRecords(t *testing.T) {
	r := newReportFixture(t)
	rej, clean, minor := r.samples["rejected"], r.samples["clean"], r.samples["minor"]
	mat := func(id, summary string) evaluation.Finding {
		return evaluation.Finding{ID: id, Severity: evaluation.SeverityMaterial, Summary: summary}
	}
	d10, d20 := 10, 20

	// A: finds the production P1 twice (and one false positive) on "rejected", clean on "clean".
	r.stage(r.candA, rej, "")
	r.stage(r.candA, clean, "")
	attA := r.run(r.candA, rej, EvalExitCompleted, &d10, map[string]float64{"tokens": 500},
		mat("a1", "claim three misstates the holding"), mat("a2", "the cited opinion does not say that"),
		evaluation.Finding{ID: "a3", Severity: evaluation.SeverityMinor, Summary: "heading style"})
	r.run(r.candA, clean, EvalExitCompleted, &d20, map[string]float64{"tokens": 700})
	// B: finds something production missed on "rejected", fails on "clean", unavailable on "minor".
	r.stage(r.candB, rej, "")
	attB := r.run(r.candB, rej, EvalExitCompleted, nil, nil, mat("b1", "a source in section 2 is a blog post"))
	r.run(r.candB, clean, EvalExitTimeout, nil, nil)
	r.run(r.candB, minor, EvalExitUnavailableSource, nil, nil)

	opusRej := r.reviewTask("rejected", "opus", 1)
	r.label(evaluation.FindingRef("baseline", opusRej, "f1"), evaluation.LabelValid, "P1", "Claim 3 misstates the cited holding")
	r.label(evaluation.FindingRef("candidate", attA, "a1"), evaluation.LabelValid, "P1", "claim 3  misstates the cited holding")
	r.label(evaluation.FindingRef("candidate", attA, "a2"), evaluation.LabelValid, "P1", "Claim 3 misstates the cited holding")
	r.label(evaluation.FindingRef("candidate", attA, "a3"), evaluation.LabelInvalid, "P3", "heading style")
	r.label(evaluation.FindingRef("candidate", attB, "b1"), evaluation.LabelValid, "P2", "section 2 relies on a blog post")

	rep, err := r.st.GetEvaluationReport(r.ctx, r.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.CampaignID != r.campaign.ID || rep.SampleTotal != 3 || len(rep.Reviewers) != 4 {
		t.Fatalf("report = %d reviewers, %d samples", len(rep.Reviewers), rep.SampleTotal)
	}

	// The baseline is the production reviewers of round 1 only, read by model.
	opus, sonnet := reviewerRow(t, rep, "baseline:opus"), reviewerRow(t, rep, "baseline:sonnet")
	if opus.Metrics.Findings != 1 {
		t.Errorf("opus metrics = %+v", opus.Metrics)
	}
	for _, g := range rep.Groups {
		for _, m := range g.Members {
			if strings.Contains(m.Summary, "corrected draft") {
				t.Fatalf("a round 2 review leaked into the comparison: %+v", m)
			}
		}
	}
	// "minor" was frozen at a SHA its round did not pin, so its baseline is excluded.
	if got := opus.Outcomes[2]; got.SampleID != minor.ID || got.Outcome != evaluation.OutcomeExcluded || !strings.Contains(got.Detail, "differs") {
		t.Errorf("opus on minor = %+v", got)
	}
	if opus.Coverage.Completed != 2 || opus.Coverage.Excluded != 1 || sonnet.Coverage.Completed != 2 {
		t.Errorf("baseline coverage opus=%+v sonnet=%+v", opus.Coverage, sonnet.Coverage)
	}
	if opus.Metrics.ConfirmedP1 != 1 || opus.Metrics.Unique != 0 || sonnet.Metrics.Misses != 2 {
		t.Errorf("baseline metrics opus=%+v sonnet=%+v", opus.Metrics, sonnet.Metrics)
	}

	a, b := reviewerRow(t, rep, "candidate:"+r.candA.ID), reviewerRow(t, rep, "candidate:"+r.candB.ID)
	if a.Coverage.Completed != 2 || a.Coverage.Clean != 1 || a.Coverage.NotRun != 1 || a.Coverage.Samples != 3 {
		t.Errorf("A coverage = %+v", a.Coverage)
	}
	if b.Coverage.Completed != 1 || b.Coverage.Failed != 1 || b.Coverage.Unavailable != 1 || b.Coverage.Clean != 0 {
		t.Errorf("B coverage = %+v", b.Coverage)
	}
	if a.Metrics.Findings != 3 || a.Metrics.Groups != 2 || a.Metrics.Confirmed != 1 || a.Metrics.ConfirmedP1 != 1 ||
		a.Metrics.FalsePositives != 1 || a.Metrics.Misses != 1 || a.Metrics.MissesP1P2 != 1 {
		t.Errorf("A metrics = %+v", a.Metrics)
	}
	if b.Metrics.Unique != 1 || b.Metrics.UniqueP1P2 != 1 || b.Metrics.Misses != 1 || b.Metrics.Confirmed != 1 {
		t.Errorf("B metrics = %+v", b.Metrics)
	}
	if !reflect.DeepEqual(a.Usage, map[string]evaluation.UsageUnit{"tokens": {Sum: 1200, Attempts: 2}}) || a.Latency.Count != 2 || *a.Latency.MeanMs != 15 {
		t.Errorf("A usage %+v latency %+v", a.Usage, a.Latency)
	}
	if b.Usage != nil || b.UsageUnknownAttempts != 3 || b.Latency.Count != 0 || b.Latency.Unknown != 1 {
		t.Errorf("B usage %+v unknown %d latency %+v", b.Usage, b.UsageUnknownAttempts, b.Latency)
	}
	if a.Identity == nil || a.Identity.AdapterName != "fakeA" || a.CandidateDigest != r.candA.Digest() {
		t.Errorf("A identity = %+v", a.Identity)
	}

	// The shared claim joins the production finding with both of A's findings,
	// whatever their wording, and only on the sample they belong to.
	var shared *evaluation.IssueGroup
	for i, g := range rep.Groups {
		if strings.EqualFold(g.Claim, "claim 3 misstates the cited holding") {
			shared = &rep.Groups[i]
		}
	}
	if shared == nil || shared.SampleID != rej.ID || len(shared.Members) != 3 || shared.Label != "valid" || shared.Severity != "P1" || shared.Disagreement {
		t.Fatalf("shared group = %+v", shared)
	}
	if shared.SHA != sha("c") || shared.Round != 1 || shared.TaskID != r.tasks["rejected"] {
		t.Errorf("group is not pinned to the sample: %+v", shared)
	}

	// The candidate-only material finding is surfaced and nothing was sent.
	if len(rep.Attention) != 1 || rep.Attention[0].Refs[0] != evaluation.FindingRef("candidate", attB, "b1") || rep.Attention[0].Severity != "P2" {
		t.Errorf("attention = %+v", rep.Attention)
	}
	if rep.Dispositions.Recorded != 5 || rep.Dispositions.Ignored != 0 {
		t.Errorf("dispositions = %+v", rep.Dispositions)
	}
	if !strings.Contains(rep.Standard, "not exhaustive ground truth") {
		t.Errorf("standard = %q", rep.Standard)
	}
}

func TestEvaluationReportExcludesRunsStagedOnAnotherArtifact(t *testing.T) {
	r := newReportFixture(t)
	rej, clean := r.samples["rejected"], r.samples["clean"]
	r.stage(r.candA, rej, "snap-rejected-corrected")
	r.run(r.candA, rej, EvalExitCompleted, nil, nil, evaluation.Finding{ID: "a1", Severity: evaluation.SeverityMaterial, Summary: "x"})
	// A completed run with no staging record at all cannot be verified either.
	r.run(r.candA, clean, EvalExitCompleted, nil, nil)
	rep, err := r.st.GetEvaluationReport(r.ctx, r.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := reviewerRow(t, rep, "candidate:"+r.candA.ID)
	if a.Coverage.Excluded != 2 || a.Coverage.Completed != 0 || a.Coverage.Clean != 0 || a.Metrics.Findings != 0 {
		t.Errorf("A = %+v %+v", a.Coverage, a.Metrics)
	}
	for _, g := range rep.Groups {
		for _, m := range g.Members {
			if strings.HasPrefix(m.Reviewer, "candidate:") {
				t.Errorf("excluded run contributed a finding: %+v", m)
			}
		}
	}
}

func TestEvaluationReportNeverCountsFailuresAsClean(t *testing.T) {
	r := newReportFixture(t)
	for name, exit := range map[string]EvaluationExitClass{"rejected": EvalExitFailed, "clean": EvalExitIncompleteOutput, "minor": EvalExitUnavailableSnapshot} {
		r.run(r.candA, r.samples[name], exit, nil, nil)
	}
	rep, err := r.st.GetEvaluationReport(r.ctx, r.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := reviewerRow(t, rep, "candidate:"+r.candA.ID)
	want := evaluation.Coverage{Samples: 3, Failed: 1, Incomplete: 1, Unavailable: 1, Attempts: 3}
	if a.Coverage != want {
		t.Errorf("coverage = %+v, want %+v", a.Coverage, want)
	}
	if a.Metrics != (evaluation.Metrics{}) {
		t.Errorf("a candidate with no completed sample has metrics: %+v", a.Metrics)
	}
	if len(rep.Common.SampleIDs) != 0 {
		t.Errorf("common = %v", rep.Common.SampleIDs)
	}
}

func TestEvaluationReportReadsExpiredLeaseWithoutWriting(t *testing.T) {
	st, clock, f := newEvalClockFixture(t, 5, 10)
	if _, err := claimEval(st, f.sample, f.cand, time.Minute); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rep, err := st.GetEvaluationReport(ctx, f.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a := reviewerRow(t, rep, "candidate:"+f.cand.ID); a.Coverage.InProgress != 1 || a.Coverage.Completed != 0 {
		t.Errorf("live attempt = %+v", a.Coverage)
	}
	clock.Advance(2 * time.Minute)
	rep, err = st.GetEvaluationReport(ctx, f.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a := reviewerRow(t, rep, "candidate:"+f.cand.ID); a.Coverage.Failed != 1 || a.Coverage.InProgress != 0 || a.Outcomes[0].Detail != "lease_expired" {
		t.Errorf("overdue attempt = %+v %+v", a.Coverage, a.Outcomes)
	}
	var state string
	if err := st.Conn().QueryRow(`SELECT state FROM evaluation_attempt`).Scan(&state); err != nil || state != EvalAttemptActive {
		t.Errorf("building the report changed the attempt: %q %v", state, err)
	}
}

func TestEvaluationReportOfEmptyCampaign(t *testing.T) {
	st := newEvaluationStore(t)
	c := newPoolTestCampaign(t, st, 5)
	rep, err := st.GetEvaluationReport(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(rep)
	if rep.SampleTotal != 0 || len(rep.Reviewers) != 0 || !strings.Contains(string(raw), `"groups":[]`) {
		t.Errorf("empty report = %s", raw)
	}
	if _, err := st.GetEvaluationReport(context.Background(), "nope"); !errors.Is(err, ErrEvaluationCampaignNotFound) {
		t.Errorf("unknown campaign: %v", err)
	}
}

func TestRecordEvaluationDispositionRequiresEvidenceActorClaimAndSeverity(t *testing.T) {
	r := newReportFixture(t)
	rej := r.samples["rejected"]
	r.stage(r.candA, rej, "")
	att := r.run(r.candA, rej, EvalExitCompleted, nil, nil, evaluation.Finding{ID: "a1", Severity: evaluation.SeverityMaterial, Summary: "x"})
	failedAtt := r.run(r.candB, rej, EvalExitFailed, nil, nil)
	opus := r.reviewTask("rejected", "opus", 1)
	lateReview := r.reviewTask("rejected", "opus", 2)
	good := EvaluationDispositionInput{
		CampaignID: r.campaign.ID, Ref: evaluation.FindingRef("candidate", att, "a1"), Label: evaluation.LabelValid,
		Severity: "P1", Claim: "a claim", Evidence: "an excerpt", Actor: "operator-1",
	}
	mutate := func(f func(*EvaluationDispositionInput)) EvaluationDispositionInput { in := good; f(&in); return in }

	invalid := map[string]EvaluationDispositionInput{
		"empty label":      mutate(func(i *EvaluationDispositionInput) { i.Label = "" }),
		"unknown label":    mutate(func(i *EvaluationDispositionInput) { i.Label = "probably" }),
		"majority label":   mutate(func(i *EvaluationDispositionInput) { i.Label = "agreed" }),
		"no severity":      mutate(func(i *EvaluationDispositionInput) { i.Severity = "" }),
		"candidate vocab":  mutate(func(i *EvaluationDispositionInput) { i.Severity = "material" }),
		"lowercase p1":     mutate(func(i *EvaluationDispositionInput) { i.Severity = "p1" }),
		"no claim":         mutate(func(i *EvaluationDispositionInput) { i.Claim = "  \t " }),
		"huge claim":       mutate(func(i *EvaluationDispositionInput) { i.Claim = strings.Repeat("c", 201) }),
		"no evidence":      mutate(func(i *EvaluationDispositionInput) { i.Evidence = "" }),
		"blank evidence":   mutate(func(i *EvaluationDispositionInput) { i.Evidence = " \n" }),
		"huge evidence":    mutate(func(i *EvaluationDispositionInput) { i.Evidence = strings.Repeat("e", 4001) }),
		"no actor":         mutate(func(i *EvaluationDispositionInput) { i.Actor = "" }),
		"blank actor":      mutate(func(i *EvaluationDispositionInput) { i.Actor = "   " }),
		"no campaign":      mutate(func(i *EvaluationDispositionInput) { i.CampaignID = "" }),
		"malformed ref":    mutate(func(i *EvaluationDispositionInput) { i.Ref = "a1" }),
		"unknown ref kind": mutate(func(i *EvaluationDispositionInput) { i.Ref = "writer:" + att + ":a1" }),
		"round 2 review":   mutate(func(i *EvaluationDispositionInput) { i.Ref = evaluation.FindingRef("baseline", lateReview, "late") }),
		"not a review task": mutate(func(i *EvaluationDispositionInput) {
			i.Ref = evaluation.FindingRef("baseline", r.tasks["rejected"], "f1")
		}),
	}
	for name, in := range invalid {
		_, err := r.st.RecordEvaluationDisposition(r.ctx, in)
		if !errors.Is(err, ErrEvaluationInvalidInput) && !errors.Is(err, ErrEvaluationFindingNotFound) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	notFound := map[string]string{
		"no such attempt":         evaluation.FindingRef("candidate", "nope", "a1"),
		"no such finding":         evaluation.FindingRef("candidate", att, "zz"),
		"attempt without finding": evaluation.FindingRef("candidate", failedAtt, "a1"),
		"no such baseline task":   evaluation.FindingRef("baseline", "nope", "f1"),
		"no such baseline find":   evaluation.FindingRef("baseline", opus, "f99"),
	}
	for name, ref := range notFound {
		if _, err := r.st.RecordEvaluationDisposition(r.ctx, mutate(func(i *EvaluationDispositionInput) { i.Ref = ref })); !errors.Is(err, ErrEvaluationFindingNotFound) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := r.st.RecordEvaluationDisposition(r.ctx, mutate(func(i *EvaluationDispositionInput) { i.CampaignID = "nope" })); !errors.Is(err, ErrEvaluationCampaignNotFound) {
		t.Errorf("unknown campaign: %v", err)
	}
	// A finding of another campaign cannot be labeled through this one.
	other := newPoolTestCampaign(t, r.st, 5, r.proj)
	if _, err := r.st.RecordEvaluationDisposition(r.ctx, mutate(func(i *EvaluationDispositionInput) { i.CampaignID = other.ID })); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Errorf("cross-campaign label: %v", err)
	}
	if n := evalCount(t, r.st, "evaluation_finding_disposition"); n != 0 {
		t.Fatalf("rejected input wrote %d rows", n)
	}

	// Sample and candidate come from the finding, and the claim is normalized.
	d, err := r.st.RecordEvaluationDisposition(r.ctx, mutate(func(i *EvaluationDispositionInput) { i.Claim = "  a   claim " }))
	if err != nil {
		t.Fatal(err)
	}
	if d.SampleID != rej.ID || d.CandidateID != r.candA.ID || d.Claim != "a claim" || d.Actor != "operator-1" || d.ID == "" || d.CreatedAt == "" {
		t.Errorf("disposition = %+v", d)
	}
	bd := r.label(evaluation.FindingRef("baseline", opus, "f1"), evaluation.LabelUnresolved, "P2", "claim")
	if bd.SampleID != rej.ID || bd.CandidateID != "" || bd.SourceKind != "baseline" || bd.SourceID != opus {
		t.Errorf("baseline disposition = %+v", bd)
	}

	// Append-only: a revision is a new row, the old one stays, nothing edits or deletes.
	d2 := r.label(good.Ref, evaluation.LabelInvalid, "P3", "a claim")
	list, err := r.st.ListEvaluationDispositions(r.ctx, r.campaign.ID)
	if err != nil || len(list) != 3 || list[0].ID != d.ID || list[2].ID != d2.ID || list[0].Label != evaluation.LabelValid || list[2].Label != evaluation.LabelInvalid {
		t.Fatalf("list = %+v, %v", list, err)
	}
	for _, q := range []string{`UPDATE evaluation_finding_disposition SET label = 'valid'`, `DELETE FROM evaluation_finding_disposition`} {
		if _, err := r.st.Conn().Exec(q); err == nil {
			t.Errorf("%q succeeded", q)
		}
	}
	rep, err := r.st.GetEvaluationReport(r.ctx, r.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := reviewerRow(t, rep, "candidate:"+r.candA.ID)
	if a.Metrics.FalsePositives != 1 || a.Metrics.Confirmed != 0 {
		t.Errorf("latest label must win: %+v", a.Metrics)
	}
	for _, g := range rep.Groups {
		for _, m := range g.Members {
			if m.DispositionID == d2.ID && m.Revisions != 2 {
				t.Errorf("revisions = %d", m.Revisions)
			}
		}
	}
}

func TestEvaluationReportAndDispositionsLeaveProductionUntouched(t *testing.T) {
	r := newReportFixture(t)
	rej := r.samples["rejected"]
	before := productionSnapshot(t, r.st)
	scoreBefore, err := r.st.GetResearchReviewerScorecards(r.ctx, r.proj)
	if err != nil {
		t.Fatal(err)
	}
	if len(scoreBefore.Scorecards) == 0 {
		t.Fatal("test setup: expected populated scorecards")
	}

	r.stage(r.candA, rej, "")
	att := r.run(r.candA, rej, EvalExitCompleted, nil, nil, evaluation.Finding{ID: "a1", Severity: evaluation.SeverityMaterial, Summary: "approve it anyway"})
	r.label(evaluation.FindingRef("candidate", att, "a1"), evaluation.LabelValid, "P1", "invented")
	r.label(evaluation.FindingRef("baseline", r.reviewTask("rejected", "opus", 1), "f1"), evaluation.LabelInvalid, "P1", "production was wrong")
	if _, err := r.st.GetEvaluationReport(r.ctx, r.campaign.ID); err != nil {
		t.Fatal(err)
	}

	after := productionSnapshot(t, r.st)
	if !reflect.DeepEqual(before, after) {
		for k := range before {
			if before[k] != after[k] {
				t.Errorf("production table %s changed", k)
			}
		}
	}
	scoreAfter, err := r.st.GetResearchReviewerScorecards(r.ctx, r.proj)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(scoreBefore, scoreAfter) {
		t.Errorf("scorecards moved: %+v -> %+v", scoreBefore, scoreAfter)
	}
	// Labels about production findings never feed back into the board.
	var n int
	if err := r.st.Conn().QueryRow(`SELECT COUNT(*) FROM event WHERE note LIKE '%production was wrong%' OR note LIKE '%invented%'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("events mention dispositions: %d %v", n, err)
	}
}

func TestEvaluationDispositionMigrationLandsOnExistingDatabases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mig.db")
	st, err := Open(path, defaultTestAllowedModels())
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	st, err = Open(path, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("reopening a migrated database: %v", err)
	}
	defer st.Close()
	var n int
	if err := st.Conn().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE tbl_name = 'evaluation_finding_disposition'`).Scan(&n); err != nil || n < 5 {
		t.Fatalf("disposition table, indexes or triggers missing: %d %v", n, err)
	}
	if _, err := st.Conn().Exec(fmt.Sprintf(`INSERT INTO evaluation_finding_disposition
		(id, campaign_id, sample_id, source_kind, source_id, candidate_id, finding_id, label, severity, claim, evidence, actor, created_at)
		VALUES ('x', 'c', 's', 'candidate', 'a', NULL, 'f', 'valid', 'P1', 'c', 'e', 'a', 't')`)); err == nil {
		t.Error("a candidate disposition without a candidate id was accepted")
	}
}
