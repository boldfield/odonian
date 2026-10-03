package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
)

type cohortFixture struct {
	t     *testing.T
	ctx   context.Context
	st    Store
	proj  string
	doc   string
	spec  string
	tasks map[string]string
}

func newCohortFixture(t *testing.T) *cohortFixture {
	t.Helper()
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "cohort.db"), defaultTestAllowedModels())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	proj, err := st.CreateProject(ctx, "research", "https://example.com/org/repo")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := st.CreateDocument(ctx, proj.ID, "feature_spec", "doc", "doc.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &cohortFixture{t: t, ctx: ctx, st: st, proj: proj.ID, doc: doc.ID, tasks: map[string]string{}}
}

func (f *cohortFixture) create(name, track string, promote bool) string {
	f.t.Helper()
	tasks, err := f.st.CreateTasks(f.ctx, f.proj, []TaskInput{{
		Title: name, Spec: "Acceptance: " + name + " cites a primary source for every claim.", DocumentID: f.doc,
		Model: "haiku", ReviewModels: []string{"opus", "sonnet"}, Track: track,
	}})
	if err != nil {
		f.t.Fatal(err)
	}
	id := tasks[0].ID
	f.tasks[name] = id
	if promote {
		if _, err := f.st.PromoteTask(f.ctx, id); err != nil {
			f.t.Fatal(err)
		}
	}
	return id
}

func (f *cohortFixture) submit(id, note string, links ...LinkInput) {
	f.t.Helper()
	if _, err := f.st.ClaimTask(f.ctx, id, "agent-1", "haiku", 5*time.Minute); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.st.SubmitTask(f.ctx, id, "agent-1", note, nil, links, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		f.t.Fatal(err)
	}
}

func (f *cohortFixture) review(id string, round int, model, verdict, findings string) {
	f.t.Helper()
	opus, sonnet := findResearchReviewTasks(f.t, f.st, f.ctx, f.proj, id, round)
	rt := map[string]*Task{"opus": opus, "sonnet": sonnet}[model]
	if rt == nil {
		f.t.Fatalf("no ready %s review task in round %d", model, round)
	}
	submitResearchReview(f.t, f.st, f.ctx, rt, model+"-reviewer", verdict, json.RawMessage(findings))
}

const (
	noFindings = `[]`
	p1Finding  = `[{"id":"f1","severity":"P1","file":"research.md","line":3,"summary":"Claim 3 misstates the holding of the cited opinion","in_changed_text":true,"status":"new"}]`
	p3Finding  = `[{"id":"f1","severity":"P3","file":"research.md","line":9,"summary":"Citation format is inconsistent here","in_changed_text":true,"status":"new"}]`
)

func sha(c string) string { return strings.Repeat(c, 40) }

func (f *cohortFixture) seed() {
	f.t.Helper()
	// clean: both reviewers report nothing.
	clean := f.create("clean", "research", true)
	f.submit(clean, "first submission", LinkInput{Kind: "pr", Value: "https://github.com/org/repo/pull/101"}, LinkInput{Kind: "commit", Value: sha("a")})
	f.review(clean, 1, "opus", "approve", noFindings)
	f.review(clean, 1, "sonnet", "approve", noFindings)

	// P3 only is still clean.
	minor := f.create("minor", "research", true)
	f.submit(minor, "first submission", LinkInput{Kind: "commit", Value: sha("b")})
	f.review(minor, 1, "opus", "approve", p3Finding)
	f.review(minor, 1, "sonnet", "approve", noFindings)

	// rejected, then fixed in a later round.
	rej := f.create("rejected", "research", true)
	f.submit(rej, "first submission", LinkInput{Kind: "pr", Value: "https://github.com/org/repo/pull/102"}, LinkInput{Kind: "commit", Value: sha("c")})
	f.review(rej, 1, "opus", "reject", p1Finding)
	f.review(rej, 1, "sonnet", "approve", noFindings)
	f.submit(rej, "fixed claim three after reviewer feedback", LinkInput{Kind: "pr", Value: "https://github.com/org/repo/pull/102"}, LinkInput{Kind: "commit", Value: sha("d")})

	// one reviewer has not reported.
	part := f.create("partial", "research", true)
	f.submit(part, "first submission", LinkInput{Kind: "commit", Value: sha("e")})
	f.review(part, 1, "opus", "approve", noFindings)

	// never submitted.
	f.create("backlog", "research", false)

	// build track.
	f.create("build", "build", true)

	// submitted without any commit link.
	nocommit := f.create("nocommit", "research", true)
	f.submit(nocommit, "first submission", LinkInput{Kind: "pr", Value: "https://github.com/org/repo/pull/103"})
	f.review(nocommit, 1, "opus", "approve", noFindings)
	f.review(nocommit, 1, "sonnet", "approve", noFindings)

	// a replacement task carries the supersede compaction block.
	repl := f.create("replacement", "research", true)
	f.submit(repl, "first submission", LinkInput{Kind: "commit", Value: sha("f")})
	f.review(repl, 1, "opus", "approve", noFindings)
	f.review(repl, 1, "sonnet", "approve", noFindings)
	if _, err := f.st.Conn().ExecContext(f.ctx, `UPDATE task SET spec = spec || ? WHERE id = ?`, "\n\n"+researchHistorySentinel+"\n## Unresolved findings\n- earlier reviewer said X", repl); err != nil {
		f.t.Fatal(err)
	}
}

func TestFirstRoundCensusClassifiesFromBoardRecords(t *testing.T) {
	f := newCohortFixture(t)
	f.seed()
	census, err := f.st.FirstRoundCensus(f.ctx, []string{f.proj})
	if err != nil {
		t.Fatal(err)
	}
	// 8 seeded tasks plus the follow-up the board opens for the non-blocking P3 finding.
	if census.Examined != 9 {
		t.Fatalf("examined = %d", census.Examined)
	}
	want := map[string]int{evaluation.ExcludedNotSubmitted: 2, evaluation.ExcludedNotResearch: 1, evaluation.ExcludedRoundOneIncomplete: 1, evaluation.ExcludedReplacement: 1}
	for k, v := range want {
		if census.Excluded[k] != v {
			t.Errorf("excluded[%s] = %d, want %d (%v)", k, census.Excluded[k], v, census.Excluded)
		}
	}
	got := map[string]evaluation.FirstRoundRecord{}
	for _, r := range census.Records {
		got[r.TaskID] = r
	}
	if len(got) != 4 {
		t.Fatalf("eligible records = %d, want 4", len(got))
	}
	outcomes := map[string]evaluation.Outcome{"clean": evaluation.OutcomeClean, "minor": evaluation.OutcomeClean, "rejected": evaluation.OutcomeRejectedMaterial, "nocommit": evaluation.OutcomeClean}
	for name, oc := range outcomes {
		if r := got[f.tasks[name]]; r.Outcome != oc || r.OutcomeReason == "" || r.ReviewRound != 1 || r.ProjectID != f.proj {
			t.Errorf("%s: %+v", name, r)
		}
	}

	rej := got[f.tasks["rejected"]]
	sha1, reason, _ := evaluation.ResolveSubmittedCommit(rej.RoundLinks, rej.UntaggedLinks)
	if sha1 != sha("c") || reason != "" {
		t.Fatalf("round 1 commit = %q (%s); the later fix %s must never be pinned", sha1, reason, sha("d"))
	}
	if !strings.Contains(rej.Spec, "Acceptance: rejected") || strings.Contains(rej.Spec, "reviewer") {
		t.Fatalf("spec = %q", rej.Spec)
	}
	sealed := ""
	for _, s := range rej.Sealed {
		sealed += s.Label + "=" + s.Text + "\n"
	}
	for _, must := range []string{"Claim 3 misstates the holding", "fixed claim three after reviewer feedback", sha("d"), "https://github.com/org/repo/pull/102"} {
		if !strings.Contains(sealed, must) {
			t.Errorf("sealed material lacks %q:\n%s", must, sealed)
		}
	}
	if strings.Contains(sealed, "first submission") { // never recoverable once overwritten, but must never be sealed either
		t.Error("the round-1 submission note is the artifact's own and must not be sealed")
	}

	nc := got[f.tasks["nocommit"]]
	if _, reason, _ := evaluation.ResolveSubmittedCommit(nc.RoundLinks, nc.UntaggedLinks); reason != evaluation.UnavailNoCommit {
		t.Fatalf("task without a commit link resolved: %q", reason)
	}
}

func TestFirstRoundCensusTreatsUnattributableLinksAsUnavailable(t *testing.T) {
	f := newCohortFixture(t)
	id := f.create("old", "research", true)
	f.submit(id, "first submission", LinkInput{Kind: "commit", Value: sha("a")})
	f.review(id, 1, "opus", "approve", noFindings)
	f.review(id, 1, "sonnet", "approve", noFindings)
	if _, err := f.st.Conn().ExecContext(f.ctx, `UPDATE task_link SET review_round = NULL WHERE task_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	rec, err := f.st.GetFirstRoundRecord(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, reason, detail := evaluation.ResolveSubmittedCommit(rec.RoundLinks, rec.UntaggedLinks); reason != evaluation.UnavailNoCommit || !strings.Contains(detail, "predate round tagging") {
		t.Fatalf("untagged link must not be guessed into round 1: %q %q", reason, detail)
	}
}

func TestFirstRoundCensusReadsStoredManifest(t *testing.T) {
	f := newCohortFixture(t)
	id := f.create("m", "research", true)
	f.submit(id, "first submission", LinkInput{Kind: "commit", Value: sha("a")})
	body := `{"children":[]}`
	if _, err := f.st.Conn().ExecContext(f.ctx,
		`INSERT INTO task_submission_manifest (id, task_id, review_round, parent_task_id, manifest_json, manifest_digest, created_at) VALUES (?, ?, 1, ?, ?, ?, ?)`,
		GenerateID(), id, id, body, "deadbeef", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	f.review(id, 1, "opus", "approve", noFindings)
	f.review(id, 1, "sonnet", "approve", noFindings)
	rec, err := f.st.GetFirstRoundRecord(f.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Manifest == nil || string(rec.Manifest.JSON) != body || rec.Manifest.Digest != "deadbeef" || rec.Manifest.Valid() {
		t.Fatalf("manifest = %+v (a stored digest that does not match its JSON must read as invalid)", rec.Manifest)
	}
}

func TestFirstRoundRecordErrors(t *testing.T) {
	f := newCohortFixture(t)
	f.seed()
	if _, err := f.st.GetFirstRoundRecord(f.ctx, f.tasks["backlog"]); !errors.Is(err, ErrFirstRoundIneligible) {
		t.Fatalf("backlog: %v", err)
	}
	if _, err := f.st.GetFirstRoundRecord(f.ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	if _, err := f.st.FirstRoundCensus(f.ctx, nil); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Fatalf("no projects: %v", err)
	}
	if _, err := f.st.FirstRoundCensus(f.ctx, []string{"missing"}); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Fatalf("missing project: %v", err)
	}
}

func strp(s string) *string { return &s }

type stagingFixture struct {
	st       Store
	campaign EvaluationCampaign
	sample   EvaluationSample
}

func newStagingFixture(t *testing.T) *stagingFixture {
	t.Helper()
	st := newEvaluationStore(t)
	ctx := context.Background()
	camp, err := st.CreateEvaluationCampaign(ctx, EvaluationCampaign{
		ID: GenerateID(), Name: "c", AllowedProjectIDs: []string{"p1"}, AllowedModelIDs: []string{"model1"},
		CohortManifest: `{}`, AttemptCap: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	sm, err := st.CreateEvaluationSample(ctx, EvaluationSample{
		ID: GenerateID(), CampaignID: camp.ID, ProjectID: "p1", OriginalTaskID: "t1", OriginalReviewRound: 1,
		SubmittedSHA: sha("a"), SnapshotDigest: strp("snap"), SourceDigest: strp("src"),
		PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &stagingFixture{st: st, campaign: camp, sample: sm}
}

func (f *stagingFixture) candidate(t *testing.T) EvaluationCandidate {
	t.Helper()
	c, err := f.st.CreateEvaluationCandidate(context.Background(), newTestCandidate(f.campaign.ID))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (f *stagingFixture) staging(c EvaluationCandidate) EvaluationStaging {
	return EvaluationStaging{
		ID: GenerateID(), CampaignID: f.campaign.ID, SampleID: f.sample.ID, CandidateID: c.ID,
		CandidateConfigDigest: c.Digest(), SnapshotDigest: "snap", SourceDigest: "src",
		PromptVersion: "blinded-v1", PromptDigest: "pd", LimitsJSON: `{"web":"not_enforced"}`,
	}
}

func TestRecordEvaluationStagingValidatesAgainstFrozenSample(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	c := f.candidate(t)

	bad := f.staging(c)
	bad.SnapshotDigest = "other"
	if _, err := f.st.RecordEvaluationStaging(ctx, bad); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Fatalf("snapshot digest mismatch: %v", err)
	}
	bad = f.staging(c)
	bad.SourceDigest = "other"
	if _, err := f.st.RecordEvaluationStaging(ctx, bad); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Fatalf("source digest mismatch: %v", err)
	}
	bad = f.staging(c)
	bad.CandidateConfigDigest = "other"
	if _, err := f.st.RecordEvaluationStaging(ctx, bad); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Fatalf("candidate digest mismatch: %v", err)
	}
	bad = f.staging(c)
	bad.LimitsJSON = `{not json`
	if _, err := f.st.RecordEvaluationStaging(ctx, bad); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Fatalf("bad limits: %v", err)
	}
	bad = f.staging(c)
	bad.PromptDigest = ""
	if _, err := f.st.RecordEvaluationStaging(ctx, bad); !errors.Is(err, ErrEvaluationInvalidInput) {
		t.Fatalf("missing prompt digest: %v", err)
	}

	other := newStagingFixture(t)
	foreign := other.staging(other.candidate(t))
	foreign.CampaignID = f.campaign.ID
	if _, err := f.st.RecordEvaluationStaging(ctx, foreign); err == nil {
		t.Fatal("a staging naming a sample and candidate from another store must be refused")
	}
	if got, _ := f.st.ListEvaluationStagings(ctx, f.campaign.ID); len(got) != 0 {
		t.Fatalf("rejected stagings left rows: %v", got)
	}
}

func TestRecordEvaluationStagingIdempotentAndImmutable(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	c := f.candidate(t)

	first, err := f.st.RecordEvaluationStaging(ctx, f.staging(c))
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.st.RecordEvaluationStaging(ctx, f.staging(c))
	if err != nil || again.ID != first.ID {
		t.Fatalf("identical staging must return the existing row: %v %+v", err, again)
	}
	different := f.staging(c)
	different.LimitsJSON = `{"web":"enforced"}`
	if _, err := f.st.RecordEvaluationStaging(ctx, different); !errors.Is(err, ErrEvaluationAlreadyExists) {
		t.Fatalf("changed limits for the same slot: %v", err)
	}
	for _, q := range []string{
		`UPDATE evaluation_staging SET snapshot_digest = 'x' WHERE id = ?`,
		`DELETE FROM evaluation_staging WHERE id = ?`,
	} {
		if _, err := f.st.Conn().ExecContext(ctx, q, first.ID); err == nil {
			t.Fatalf("%q must be refused by the frozen-row triggers", q)
		}
	}
	got, err := f.st.GetEvaluationStaging(ctx, f.campaign.ID, f.sample.ID, c.ID)
	if err != nil || got != first {
		t.Fatalf("get: %v %+v vs %+v", err, got, first)
	}
	if _, err := f.st.GetEvaluationStaging(ctx, f.campaign.ID, f.sample.ID, "nope"); !errors.Is(err, ErrEvaluationStagingNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestSecondCandidateStagingLeavesFirstUntouched(t *testing.T) {
	f := newStagingFixture(t)
	ctx := context.Background()
	c1 := f.candidate(t)
	first, err := f.st.RecordEvaluationStaging(ctx, f.staging(c1))
	if err != nil {
		t.Fatal(err)
	}
	samplesBefore, err := f.st.ListEvaluationSamples(ctx, f.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}

	c2 := f.candidate(t)
	second := f.staging(c2)
	second.LimitsJSON = `{"web":"not_enforced","tools":["none"]}`
	if _, err := f.st.RecordEvaluationStaging(ctx, second); err != nil {
		t.Fatal(err)
	}

	stagings, err := f.st.ListEvaluationStagings(ctx, f.campaign.ID)
	if err != nil || len(stagings) != 2 {
		t.Fatalf("stagings: %v %v", err, stagings)
	}
	if got, _ := f.st.GetEvaluationStaging(ctx, f.campaign.ID, f.sample.ID, c1.ID); got != first {
		t.Fatalf("first candidate's staging changed: %+v -> %+v", first, got)
	}
	samplesAfter, _ := f.st.ListEvaluationSamples(ctx, f.campaign.ID)
	if len(samplesAfter) != len(samplesBefore) || *samplesAfter[0].SnapshotDigest != *samplesBefore[0].SnapshotDigest || samplesAfter[0].ID != samplesBefore[0].ID {
		t.Fatalf("frozen sample changed: %+v -> %+v", samplesBefore, samplesAfter)
	}
	if _, err := f.st.ListEvaluationStagings(ctx, "missing"); !errors.Is(err, ErrEvaluationCampaignNotFound) {
		t.Fatalf("unknown campaign: %v", err)
	}
}
