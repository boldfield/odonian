package evaluation

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ident(adapter, model string) CandidateIdentity {
	id := UnknownIdentity()
	id.AdapterName, id.ModelID, id.ModelRevision = adapter, model, "r1"
	id.RuntimeName, id.RuntimeVersion = "rt-"+adapter, "1.0"
	id.AccountPool = "pool-" + adapter
	id.Tools = KnownNames("grep")
	return id
}

func intp(v int) *int { return &v }

func attempt(id string, seq int, exit string, findings ...Finding) ReportAttempt {
	st := "finalized"
	return ReportAttempt{ID: id, Sequence: seq, State: st, ExitClass: exit, Findings: findings}
}

func fnd(id string, sev Severity, summary string) Finding {
	return Finding{ID: id, Severity: sev, Summary: summary}
}

func disp(id, sample, kind, source, cand, finding string, label FindingLabel, sev, claim string) Disposition {
	return Disposition{
		ID: id, SampleID: sample, SourceKind: kind, SourceID: source, CandidateID: cand, FindingID: finding,
		Label: label, Severity: sev, Claim: claim, Evidence: "checked " + finding, Actor: "ops", CreatedAt: "2026-01-01T00:00:00Z",
	}
}

func rowByKey(t *testing.T, r Report, key string) ReviewerReport {
	t.Helper()
	for _, rr := range r.Reviewers {
		if rr.Key == key {
			return rr
		}
	}
	t.Fatalf("no reviewer %q in %v", key, func() []string {
		var ks []string
		for _, rr := range r.Reviewers {
			ks = append(ks, rr.Key)
		}
		return ks
	}())
	return ReviewerReport{}
}

func groupWithClaim(t *testing.T, r Report, sample, claim string) IssueGroup {
	t.Helper()
	for _, g := range r.Groups {
		if g.SampleID == sample && claimKey(g.Claim) == claimKey(claim) {
			return g
		}
	}
	t.Fatalf("no group %q on %s", claim, sample)
	return IssueGroup{}
}

func baseInput() ReportInput {
	samples := []ReportSample{
		{ID: "s1", TaskID: "t1", Round: 1, SHA: "aaa", SnapshotDigest: "snap-1"},
		{ID: "s2", TaskID: "t2", Round: 1, SHA: "bbb", SnapshotDigest: "snap-2"},
		{ID: "s3", TaskID: "t3", Round: 1, SHA: "ccc", SnapshotDigest: "snap-3"},
	}
	candA := ReportCandidate{ID: "A", Digest: "dig-A", Identity: ident("alpha", "model-a")}
	candB := ReportCandidate{ID: "B", Digest: "dig-B", Identity: ident("beta", "model-b")}
	run := func(c ReportCandidate, s ReportSample, attempts ...ReportAttempt) ReportRun {
		return ReportRun{CandidateID: c.ID, SampleID: s.ID, Attempts: attempts, Staged: true,
			StagedSnapshotDigest: s.SnapshotDigest, StagedCandidateDigest: c.Digest}
	}
	a1 := attempt("A-s1", 1, "completed",
		fnd("f1", SeverityMaterial, "parser dereferences nil"),
		fnd("f2", SeverityMaterial, "nil pointer in the parser"), // duplicate of f1 by the same candidate
		fnd("f3", SeverityMinor, "unused variable"))
	a1.DurationMs, a1.Usage, a1.EffectiveDigest = intp(10), map[string]float64{"tokens": 1200}, "dig-A"
	a2 := attempt("A-s2", 1, "completed")
	a2.DurationMs, a2.Usage, a2.EffectiveDigest = intp(20), map[string]float64{"tokens": 800}, "dig-A"
	a3 := attempt("A-s3", 1, "unavailable_source")
	b1 := attempt("B-s1", 1, "completed", fnd("g1", SeverityMaterial, "cache is not locked"))
	b1.DurationMs, b1.Usage, b1.EffectiveDigest = intp(100), map[string]float64{"gpu_seconds": 4.5}, "dig-B"
	b2 := attempt("B-s2", 1, "timeout")
	b3 := attempt("B-s3", 1, "completed", fnd("g2", SeverityMinor, "typo in a comment"))
	b3.EffectiveDigest = "dig-B" // completed with no duration and no usage: unknown, not zero

	return ReportInput{
		CampaignID: "camp", CampaignName: "two fakes", GeneratedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Samples:    samples,
		Candidates: []ReportCandidate{candA, candB},
		Runs: []ReportRun{
			run(candA, samples[0], a1), run(candA, samples[1], a2), run(candA, samples[2], a3),
			run(candB, samples[0], b1), run(candB, samples[1], b2), run(candB, samples[2], b3),
		},
		Baselines: []BaselineSampleInput{
			{SampleID: "s1", PinnedSHA: "aaa", Reviews: []BaselineReview{
				{TaskID: "rv-astra-1", Reviewer: "astra", Complete: true, Findings: []BaselineFinding{{ID: "F1", Severity: "P1", File: "p.go", Line: 3, Summary: "null deref on parse"}}},
				{TaskID: "rv-fable-1", Reviewer: "fable", Complete: true, Findings: []BaselineFinding{{ID: "F1", Severity: "P1", File: "p.go", Line: 3, Summary: "possible crash when input empty"}}},
			}},
			{SampleID: "s2", PinnedSHA: "bbb", Reviews: []BaselineReview{
				{TaskID: "rv-astra-2", Reviewer: "astra", Complete: true},
				{TaskID: "rv-fable-2", Reviewer: "fable", Complete: true},
			}},
			{SampleID: "s3", PinnedSHA: "zzz-later-commit", Reviews: []BaselineReview{
				{TaskID: "rv-astra-3", Reviewer: "astra", Complete: true, Findings: []BaselineFinding{{ID: "F9", Severity: "P2", Summary: "only in the corrected work"}}},
			}},
		},
		Dispositions: []Disposition{
			disp("d1", "s1", SourceBaseline, "rv-astra-1", "", "F1", LabelValid, "P1", "Null deref in parser"),
			disp("d2", "s1", SourceBaseline, "rv-fable-1", "", "F1", LabelValid, "P1", "null  deref in   parser"),
			disp("d3", "s1", SourceCandidate, "A-s1", "A", "f1", LabelValid, "P1", "null deref in parser"),
			disp("d4", "s1", SourceCandidate, "A-s1", "A", "f2", LabelValid, "P1", "null deref in parser"),
			disp("d5", "s1", SourceCandidate, "A-s1", "A", "f3", LabelInvalid, "P3", "unused variable"),
			disp("d6", "s1", SourceCandidate, "B-s1", "B", "g1", LabelValid, "P2", "cache race"),
		},
	}
}

func TestReportTwoFakeCandidatesAgainstBaselines(t *testing.T) {
	r := BuildReport(baseInput())

	if r.SampleTotal != 3 || len(r.Reviewers) != 4 {
		t.Fatalf("reviewers = %d, samples = %d", len(r.Reviewers), r.SampleTotal)
	}
	a, b := rowByKey(t, r, "candidate:A"), rowByKey(t, r, "candidate:B")
	astra, fable := rowByKey(t, r, "baseline:astra"), rowByKey(t, r, "baseline:fable")

	// Unequal coverage: every row is measured against all three samples and a
	// failed, unavailable or excluded sample is never clean.
	wantA := Coverage{Samples: 3, Completed: 2, Clean: 1, Unavailable: 1, Attempts: 3}
	if a.Coverage != wantA {
		t.Errorf("A coverage = %+v, want %+v", a.Coverage, wantA)
	}
	wantB := Coverage{Samples: 3, Completed: 2, Failed: 1, Attempts: 3}
	if b.Coverage != wantB {
		t.Errorf("B coverage = %+v, want %+v", b.Coverage, wantB)
	}
	if astra.Coverage.Completed != 2 || astra.Coverage.Excluded != 1 || astra.Coverage.Clean != 1 {
		t.Errorf("astra coverage = %+v", astra.Coverage)
	}
	if fable.Coverage.Completed != 2 || fable.Coverage.NotRun != 1 {
		t.Errorf("fable coverage = %+v", fable.Coverage)
	}
	if got := a.Outcomes[2]; got.Outcome != OutcomeUnavailable || got.SampleID != "s3" {
		t.Errorf("A s3 outcome = %+v", got)
	}

	// Duplicate and differently worded findings that assert one claim form one
	// group per sample, with every reviewer's provenance kept.
	g := groupWithClaim(t, r, "s1", "Null deref in parser")
	if g.Label != "valid" || g.Severity != "P1" || len(g.Members) != 4 || g.Disagreement {
		t.Errorf("group = %+v", g)
	}
	if !reflect.DeepEqual(g.Reviewers, []string{"baseline:astra", "baseline:fable", "candidate:A"}) || !g.BaselineReported || g.CandidateOnly {
		t.Errorf("group reviewers = %v", g.Reviewers)
	}
	if g.Members[0].Ref != "baseline:rv-astra-1:F1" || g.Members[0].Actor != "ops" || g.Members[0].Evidence == "" {
		t.Errorf("member provenance = %+v", g.Members[0])
	}

	// A reported the null deref twice plus an invalid finding: 3 findings, 2 issues.
	wantAM := Metrics{Findings: 3, Groups: 2, Confirmed: 1, ConfirmedP1: 1, FalsePositives: 1, KnownValid: 2, Misses: 1, MissesP1P2: 1, Recall: ptr(0.5)}
	if !sameMetrics(a.Metrics, wantAM) {
		t.Errorf("A metrics = %+v, want %+v", a.Metrics, wantAM)
	}
	// B alone found the race, missed the null deref, and has one unlabeled finding.
	wantBM := Metrics{Findings: 2, Groups: 2, Confirmed: 1, ConfirmedP2: 1, Unique: 1, UniqueP1P2: 1, Unresolved: 1, KnownValid: 2, Misses: 1, MissesP1P2: 1, Recall: ptr(0.5)}
	if !sameMetrics(b.Metrics, wantBM) {
		t.Errorf("B metrics = %+v, want %+v", b.Metrics, wantBM)
	}
	if astra.Metrics.Confirmed != 1 || astra.Metrics.Misses != 1 || astra.Metrics.Unique != 0 {
		t.Errorf("astra metrics = %+v", astra.Metrics)
	}
	if r.UnlabeledFindings != 1 {
		t.Errorf("unlabeled = %d", r.UnlabeledFindings)
	}

	// Common samples: only s1 was completed by all four reviewers.
	if !reflect.DeepEqual(r.Common.SampleIDs, []string{"s1"}) || len(r.Common.Reviewers) != 4 {
		t.Fatalf("common = %+v", r.Common)
	}
	for _, c := range r.Common.Reviewers {
		if c.Key == "candidate:A" && (c.Metrics.Findings != 3 || c.Metrics.Misses != 1) {
			t.Errorf("A common = %+v", c.Metrics)
		}
		if c.Key == "candidate:B" && (c.Metrics.Findings != 1 || c.Metrics.Unresolved != 0) {
			t.Errorf("B common = %+v", c.Metrics)
		}
	}

	// The material candidate-only finding is surfaced; the invalid one is not.
	if len(r.Attention) != 1 || r.Attention[0].Claim != "cache race" || r.Attention[0].Severity != "P2" ||
		!reflect.DeepEqual(r.Attention[0].Reviewers, []string{"candidate:B"}) || !r.Attention[0].BaselineCovered ||
		r.Attention[0].Refs[0] != "candidate:B-s1:g1" {
		t.Errorf("attention = %+v", r.Attention)
	}
	if r.Dispositions.Recorded != 6 || r.Dispositions.Findings != 6 || r.Dispositions.Ignored != 0 {
		t.Errorf("dispositions = %+v", r.Dispositions)
	}
}

func ptr(f float64) *float64 { return &f }

func sameMetrics(a, b Metrics) bool {
	ar, br := a.Recall, b.Recall
	a.Recall, b.Recall = nil, nil
	if a != b || (ar == nil) != (br == nil) {
		return false
	}
	return ar == nil || math.Abs(*ar-*br) < 1e-9
}

func TestReportLatencyIsTheMeanAndUsageKeepsProviderUnits(t *testing.T) {
	in := baseInput()
	// Durations 10, 20 and 100 across three completed samples: mean 43.33, not
	// the 57 a running pairwise average gives.
	in.Samples = append(in.Samples, ReportSample{ID: "s4", TaskID: "t4", Round: 1, SHA: "ddd", SnapshotDigest: "snap-4"})
	cand := in.Candidates[0]
	a4 := attempt("A-s4", 1, "completed")
	a4.DurationMs = intp(100)
	in.Runs = append(in.Runs, ReportRun{CandidateID: "A", SampleID: "s4", Attempts: []ReportAttempt{a4}, Staged: true,
		StagedSnapshotDigest: "snap-4", StagedCandidateDigest: cand.Digest})
	r := BuildReport(in)

	a, b := rowByKey(t, r, "candidate:A"), rowByKey(t, r, "candidate:B")
	if a.Latency == nil || a.Latency.Count != 3 || a.Latency.Unknown != 0 || math.Abs(*a.Latency.MeanMs-43.3333333) > 1e-4 ||
		*a.Latency.MedianMs != 20 || *a.Latency.MaxMs != 100 {
		t.Errorf("A latency = %+v", a.Latency)
	}
	if !reflect.DeepEqual(a.Usage, map[string]UsageUnit{"tokens": {Sum: 2000, Attempts: 2}}) {
		t.Errorf("A usage = %+v", a.Usage)
	}
	// B reports GPU seconds only; its units are never folded into tokens, and the
	// completed attempt that reported nothing is unknown, not zero.
	if !reflect.DeepEqual(b.Usage, map[string]UsageUnit{"gpu_seconds": {Sum: 4.5, Attempts: 1}}) {
		t.Errorf("B usage = %+v", b.Usage)
	}
	if b.UsageUnknownAttempts != 2 || b.Latency.Count != 1 || b.Latency.Unknown != 1 {
		t.Errorf("B unknowns = %d, latency %+v", b.UsageUnknownAttempts, b.Latency)
	}
	if a.UsageUnknownAttempts != 2 { // the unavailable attempt and s4 reported nothing
		t.Errorf("A unknown usage = %d", a.UsageUnknownAttempts)
	}
	raw, _ := json.Marshal(r)
	for _, bad := range []string{"cost", "dollar", "usd", "price"} {
		if strings.Contains(strings.ToLower(string(raw)), bad) {
			t.Errorf("report invents a monetary figure (%q): %s", bad, raw)
		}
	}
}

func TestReportMissingUsageAndLatencyStayUnknown(t *testing.T) {
	in := baseInput()
	in.Candidates = in.Candidates[1:]
	in.Runs = in.Runs[3:]
	for i := range in.Runs {
		for j := range in.Runs[i].Attempts {
			in.Runs[i].Attempts[j].Usage, in.Runs[i].Attempts[j].DurationMs = nil, nil
		}
	}
	b := rowByKey(t, BuildReport(in), "candidate:B")
	if len(b.Usage) != 0 || b.UsageUnknownAttempts != 3 || b.Latency.MeanMs != nil || b.Latency.Unknown != 2 {
		t.Errorf("usage = %+v unknown = %d latency = %+v", b.Usage, b.UsageUnknownAttempts, b.Latency)
	}
}

func TestReportFailedAndUnfinishedRunsNeverCountAsClean(t *testing.T) {
	in := baseInput()
	in.Baselines, in.Dispositions = nil, nil
	in.Candidates = in.Candidates[:1]
	in.Runs = in.Runs[:3]
	for i, exit := range []string{"failed", "timeout", "cancelled", "unknown", "invalid_output", "incomplete_output", "unavailable_snapshot"} {
		id := string(rune('a' + i))
		in.Samples = append(in.Samples, ReportSample{ID: "x" + id, TaskID: "t" + id, Round: 1, SHA: id, SnapshotDigest: "d" + id})
		in.Runs = append(in.Runs, ReportRun{CandidateID: "A", SampleID: "x" + id, Staged: true, StagedSnapshotDigest: "d" + id,
			StagedCandidateDigest: "dig-A", Attempts: []ReportAttempt{attempt("at"+id, 1, exit)}})
	}
	// An attempt whose lease ran out, one still running and one never started.
	in.Samples = append(in.Samples, ReportSample{ID: "y1"}, ReportSample{ID: "y2"}, ReportSample{ID: "y3"})
	in.Runs = append(in.Runs,
		ReportRun{CandidateID: "A", SampleID: "y1", Attempts: []ReportAttempt{{ID: "aty1", Sequence: 1, State: "expired", ExitClass: "lease_expired"}}},
		ReportRun{CandidateID: "A", SampleID: "y2", Attempts: []ReportAttempt{{ID: "aty2", Sequence: 1, State: "active"}}})
	a := rowByKey(t, BuildReport(in), "candidate:A")
	want := Coverage{Samples: 13, Completed: 2, Clean: 1, Failed: 5, Unavailable: 2, Incomplete: 2, InProgress: 1, NotRun: 1, Attempts: 12}
	if a.Coverage != want {
		t.Errorf("coverage = %+v, want %+v", a.Coverage, want)
	}
	for _, o := range a.Outcomes {
		if (strings.HasPrefix(o.SampleID, "x") || strings.HasPrefix(o.SampleID, "y")) && o.Outcome == OutcomeCompleted {
			t.Errorf("sample %s counted as completed: %+v", o.SampleID, o)
		}
	}
}

func TestReportLaterCorrectedArtifactsAreExcluded(t *testing.T) {
	in := baseInput()
	// B's staging for s1 recorded a different snapshot, and A's s2 run has no
	// staging at all: neither can be shown to have reviewed the frozen artifact.
	for i := range in.Runs {
		switch {
		case in.Runs[i].CandidateID == "B" && in.Runs[i].SampleID == "s1":
			in.Runs[i].StagedSnapshotDigest = "snap-1-corrected"
		case in.Runs[i].CandidateID == "A" && in.Runs[i].SampleID == "s2":
			in.Runs[i].Staged = false
		}
	}
	r := BuildReport(in)
	b, a := rowByKey(t, r, "candidate:B"), rowByKey(t, r, "candidate:A")
	if b.Outcomes[0].Outcome != OutcomeExcluded || !strings.Contains(b.Outcomes[0].Detail, "corrected") {
		t.Errorf("B s1 = %+v", b.Outcomes[0])
	}
	if a.Outcomes[1].Outcome != OutcomeExcluded || a.Coverage.Excluded != 1 || a.Coverage.Clean != 0 {
		t.Errorf("A s2 = %+v, coverage %+v", a.Outcomes[1], a.Coverage)
	}
	for _, g := range r.Groups {
		for _, m := range g.Members {
			if m.Reviewer == "candidate:B" && strings.HasPrefix(m.Ref, "candidate:B-s1") {
				t.Errorf("excluded run leaked a finding: %+v", m)
			}
		}
	}
	// The baseline review of s3 was of a later commit than the frozen one.
	astra := rowByKey(t, r, "baseline:astra")
	if astra.Outcomes[2].Outcome != OutcomeExcluded || !strings.Contains(astra.Outcomes[2].Detail, "differs") {
		t.Errorf("astra s3 = %+v", astra.Outcomes[2])
	}
	for _, g := range r.Groups {
		if g.SampleID == "s3" {
			for _, m := range g.Members {
				if m.Reviewer == "baseline:astra" {
					t.Errorf("a later artifact's baseline finding was compared: %+v", m)
				}
			}
		}
	}
	// A candidate whose run staged for another configuration is excluded too.
	in = baseInput()
	in.Runs[0].StagedCandidateDigest = "someone-else"
	if o := rowByKey(t, BuildReport(in), "candidate:A").Outcomes[0]; o.Outcome != OutcomeExcluded {
		t.Errorf("wrong-config staging = %+v", o)
	}
	// A round whose pinned commit is ambiguous cannot be compared.
	in = baseInput()
	in.Baselines[0].PinProblem = "2 different commit links"
	if o := rowByKey(t, BuildReport(in), "baseline:astra").Outcomes[0]; o.Outcome != OutcomeExcluded || !strings.Contains(o.Detail, "2 different") {
		t.Errorf("unpinned baseline = %+v", o)
	}
}

func TestReportDisagreementsAndLabelRevisionsArePreserved(t *testing.T) {
	in := baseInput()
	in.Dispositions = []Disposition{
		disp("d1", "s1", SourceBaseline, "rv-astra-1", "", "F1", LabelValid, "P1", "null deref"),
		disp("d2", "s1", SourceCandidate, "A-s1", "A", "f1", LabelInvalid, "P2", "null deref"),
		disp("d3", "s1", SourceCandidate, "A-s1", "A", "f3", LabelUnresolved, "P3", "style"),
		disp("d4", "s1", SourceCandidate, "A-s1", "A", "f3", LabelUnresolved, "P3", "style"),
		disp("d5", "s1", SourceCandidate, "A-s1", "A", "f3", LabelInvalid, "P3", "style"),
	}
	r := BuildReport(in)
	g := groupWithClaim(t, r, "s1", "null deref")
	if g.Label != GroupDisputed || !g.Disagreement || g.Severity != "P1" {
		t.Errorf("disputed group = %+v", g)
	}
	labels := map[string]string{}
	for _, m := range g.Members {
		labels[m.Reviewer] = m.Label + "/" + m.DispositionSeverity
	}
	if labels["baseline:astra"] != "valid/P1" || labels["candidate:A"] != "invalid/P2" {
		t.Errorf("disagreement not preserved on members: %v", labels)
	}
	// A disputed issue is neither a confirmed hit nor a false positive; f2 has no label.
	am := rowByKey(t, r, "candidate:A").Metrics
	if am.Confirmed != 0 || am.Unresolved != 2 || am.KnownValid != 0 || am.Recall != nil {
		t.Errorf("A metrics = %+v", am)
	}
	// Latest label wins and the count of revisions is kept.
	style := groupWithClaim(t, r, "s1", "style")
	if style.Label != "invalid" || style.Members[0].Revisions != 3 || style.Members[0].DispositionID != "d5" {
		t.Errorf("revised group = %+v", style)
	}
	if rowByKey(t, r, "candidate:A").Metrics.FalsePositives != 1 {
		t.Errorf("false positives = %d", rowByKey(t, r, "candidate:A").Metrics.FalsePositives)
	}
	// Findings nobody labeled never join a group on summary text alone.
	in = baseInput()
	in.Dispositions = nil
	r = BuildReport(in)
	for _, g := range r.Groups {
		if g.Label != GroupUnlabeled || len(g.Members) != 1 {
			t.Fatalf("unlabeled finding grouped: %+v", g)
		}
	}
	if m := rowByKey(t, r, "candidate:A").Metrics; m.Confirmed != 0 || m.FalsePositives != 0 || m.KnownValid != 0 || m.Unresolved != 3 {
		t.Errorf("unlabeled findings were not left unresolved: %+v", m)
	}
}

func TestReportClaimsNeverMatchAcrossSamples(t *testing.T) {
	in := baseInput()
	in.Dispositions = []Disposition{
		disp("d1", "s1", SourceCandidate, "A-s1", "A", "f1", LabelValid, "P1", "same words"),
		disp("d2", "s3", SourceCandidate, "B-s3", "B", "g2", LabelValid, "P3", "same words"),
	}
	r := BuildReport(in)
	n := 0
	for _, g := range r.Groups {
		if g.Claim == "same words" {
			n++
			if len(g.Members) != 1 || len(g.Reviewers) != 1 {
				t.Errorf("group = %+v", g)
			}
		}
	}
	if n != 2 {
		t.Errorf("same claim on two samples gave %d groups, want 2", n)
	}
}

func TestReportIgnoresDispositionsForFindingsItDoesNotCompare(t *testing.T) {
	in := baseInput()
	in.Dispositions = append(in.Dispositions,
		disp("x1", "s1", SourceCandidate, "no-such-attempt", "A", "f1", LabelValid, "P1", "ghost"),
		disp("x2", "s2", SourceCandidate, "A-s1", "A", "f1", LabelValid, "P1", "wrong sample"))
	r := BuildReport(in)
	if r.Dispositions.Ignored != 2 || r.Dispositions.Recorded != 8 || r.Dispositions.Findings != 6 {
		t.Errorf("dispositions = %+v", r.Dispositions)
	}
	// The stray decisions label nothing and move no number.
	if m := rowByKey(t, r, "candidate:A").Metrics; m.Confirmed != 1 || m.FalsePositives != 1 {
		t.Errorf("A metrics = %+v", m)
	}
}

func TestReportRuntimeDriftAndConfigurationAreReported(t *testing.T) {
	in := baseInput()
	in.Runs[0].Attempts[0].EffectiveDigest = "dig-other"
	r := BuildReport(in)
	a := rowByKey(t, r, "candidate:A")
	if !a.RuntimeDrift || !reflect.DeepEqual(a.EffectiveDigests, []string{"dig-A", "dig-other"}) {
		t.Errorf("drift = %v %v", a.RuntimeDrift, a.EffectiveDigests)
	}
	if rowByKey(t, r, "candidate:B").RuntimeDrift {
		t.Error("B drifted")
	}
	if a.Identity == nil || a.Identity.RuntimeVersion != "1.0" || len(a.Identity.Tools.Names) != 1 || a.Label != "alpha/model-a@r1" {
		t.Errorf("identity = %+v", a.Identity)
	}
}

func TestReportHasNoFixedColumnsPerCandidate(t *testing.T) {
	in := baseInput()
	for _, n := range []string{"C", "D", "E"} {
		in.Candidates = append(in.Candidates, ReportCandidate{ID: n, Digest: "dig-" + n, Identity: ident("adapter-"+n, "m-"+n)})
	}
	r := BuildReport(in)
	if len(r.Reviewers) != 7 {
		t.Fatalf("reviewers = %d", len(r.Reviewers))
	}
	e := rowByKey(t, r, "candidate:E")
	if e.Coverage.NotRun != 3 || e.Coverage.Samples != 3 || e.Coverage.Completed != 0 || e.Metrics.Misses != 0 {
		t.Errorf("a candidate that never ran = %+v", e)
	}
	if len(r.Common.SampleIDs) != 0 {
		t.Errorf("common samples = %v with candidates that never ran", r.Common.SampleIDs)
	}
}

func TestReportIsDeterministicAndStatesItsStandard(t *testing.T) {
	a, _ := json.Marshal(BuildReport(baseInput()))
	b, _ := json.Marshal(BuildReport(baseInput()))
	if string(a) != string(b) {
		t.Error("two builds of the same input differ")
	}
	s := strings.ToLower(BuildReport(baseInput()).Standard)
	for _, want := range []string{"not exhaustive ground truth", "never count as clean", "never from agreement"} {
		if !strings.Contains(s, want) {
			t.Errorf("standard lacks %q", want)
		}
	}
}

func TestReportWithNothingRecorded(t *testing.T) {
	r := BuildReport(ReportInput{CampaignID: "c", GeneratedAt: time.Unix(0, 0)})
	raw, _ := json.Marshal(r)
	for _, want := range []string{`"reviewers":[]`, `"groups":[]`, `"attention":[]`, `"sample_ids":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("empty report lacks %s: %s", want, raw)
		}
	}
}

func TestFindingRefRoundTripAndLabels(t *testing.T) {
	ref := FindingRef(SourceCandidate, "att-1", "a:b:c")
	k, s, f, err := ParseFindingRef(ref)
	if err != nil || k != SourceCandidate || s != "att-1" || f != "a:b:c" {
		t.Errorf("parse = %q %q %q %v", k, s, f, err)
	}
	for _, bad := range []string{"", "candidate", "candidate:x", "other:x:y", "candidate::y", "baseline:x:"} {
		if _, _, _, err := ParseFindingRef(bad); err == nil {
			t.Errorf("ref %q accepted", bad)
		}
	}
	if !LabelValid.Valid() || !LabelInvalid.Valid() || !LabelUnresolved.Valid() || FindingLabel("maybe").Valid() || FindingLabel("").Valid() {
		t.Error("label validity wrong")
	}
	if !ValidAdjudicatedSeverity("P1") || ValidAdjudicatedSeverity("p1") || ValidAdjudicatedSeverity("material") {
		t.Error("severity validity wrong")
	}
	if NormalizeClaim("  a \t b\n c ") != "a b c" {
		t.Error("claim not normalized")
	}
}
