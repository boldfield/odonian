package evaluation

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReportVersion is the shape version of Report.
const ReportVersion = 1

// ReportStandard is carried in every report so its numbers are never read as
// more than they are.
const ReportStandard = "Misses and recall are relative to the known adjudicated finding set: the findings an operator " +
	"has labeled valid on the compared samples. That set is not exhaustive ground truth. Labels come only from " +
	"explicit operator dispositions, never from agreement between reviewers or from a writer accepting an edit. " +
	"Failed, unavailable, incomplete, unfinished and excluded runs never count as clean. Usage is in each " +
	"provider's own units and is never converted to money."

// FindingLabel is an operator's judgment of one finding.
type FindingLabel string

const (
	LabelValid      FindingLabel = "valid"
	LabelInvalid    FindingLabel = "invalid"
	LabelUnresolved FindingLabel = "unresolved"
)

// Valid reports whether l is one of the three labels an operator may record.
func (l FindingLabel) Valid() bool {
	return l == LabelValid || l == LabelInvalid || l == LabelUnresolved
}

// Adjudicated severities, assigned by the operator with a label.
const (
	SeverityP1 = "P1"
	SeverityP2 = "P2"
	SeverityP3 = "P3"
)

// ValidAdjudicatedSeverity reports whether s is P1, P2 or P3.
func ValidAdjudicatedSeverity(s string) bool {
	return s == SeverityP1 || s == SeverityP2 || s == SeverityP3
}

func severityRank(s string) int {
	switch s {
	case SeverityP1:
		return 1
	case SeverityP2:
		return 2
	case SeverityP3:
		return 3
	}
	return 4
}

// Where a finding came from.
const (
	SourceCandidate = "candidate"
	SourceBaseline  = "baseline"
)

// Group labels beyond the three recordable ones: a group none of whose
// findings has a disposition, and a group whose findings carry contradicting
// valid and invalid labels.
const (
	GroupUnlabeled = "unlabeled"
	GroupDisputed  = "disputed"
)

// Outcomes of one reviewer on one sample.
const (
	OutcomeCompleted   = "completed"
	OutcomeFailed      = "failed"
	OutcomeUnavailable = "unavailable"
	OutcomeIncomplete  = "incomplete"
	OutcomeInProgress  = "in_progress"
	OutcomeNotRun      = "not_run"
	OutcomeExcluded    = "excluded"
)

// Disposition is one recorded operator decision about one finding. Source
// names the finding: for a candidate it is the attempt that produced it, for a
// baseline reviewer the production review task.
type Disposition struct {
	ID          string       `json:"id"`
	SampleID    string       `json:"sample_id"`
	SourceKind  string       `json:"source_kind"`
	SourceID    string       `json:"source_id"`
	CandidateID string       `json:"candidate_id,omitempty"`
	FindingID   string       `json:"finding_id"`
	Label       FindingLabel `json:"label"`
	Severity    string       `json:"severity"`
	Claim       string       `json:"claim"`
	Evidence    string       `json:"evidence"`
	Actor       string       `json:"actor"`
	CreatedAt   string       `json:"created_at"`
}

// NormalizeClaim collapses whitespace so one claim has one spelling.
func NormalizeClaim(s string) string { return strings.Join(strings.Fields(s), " ") }

func claimKey(s string) string { return strings.ToLower(NormalizeClaim(s)) }

// FindingRef is the stable reference an operator uses to address one finding.
func FindingRef(kind, sourceID, findingID string) string {
	return kind + ":" + sourceID + ":" + findingID
}

// ParseFindingRef splits a reference made by FindingRef. The finding id is
// everything after the second colon, so it may itself contain colons.
func ParseFindingRef(ref string) (kind, sourceID, findingID string, err error) {
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" ||
		(parts[0] != SourceCandidate && parts[0] != SourceBaseline) {
		return "", "", "", fmt.Errorf("finding ref %q must be candidate:<attempt id>:<finding id> or baseline:<review task id>:<finding id>", ref)
	}
	return parts[0], parts[1], parts[2], nil
}

type dispKey struct{ sample, kind, source, finding string }

// ReportSample is one frozen sample: the exact original task, review round and
// submitted commit every reviewer is compared on.
type ReportSample struct {
	ID             string
	TaskID         string
	Round          int
	SHA            string
	SnapshotDigest string
}

// ReportCandidate is one candidate version.
type ReportCandidate struct {
	ID       string
	Digest   string
	Identity CandidateIdentity
}

// ReportAttempt is the recorded fact of one attempt. State is "active" only
// while its lease is live; an attempt whose lease ran out is "expired". Usage
// nil means unknown, never zero.
type ReportAttempt struct {
	ID              string
	Sequence        int
	State           string
	ExitClass       string
	DurationMs      *int
	Usage           map[string]float64
	EffectiveDigest string
	Findings        []Finding
}

// ReportRun is everything recorded about one candidate on one sample: its
// attempts and the staging record that says which frozen artifact it ran on.
type ReportRun struct {
	CandidateID           string
	SampleID              string
	Attempts              []ReportAttempt
	Staged                bool
	StagedSnapshotDigest  string
	StagedCandidateDigest string
}

// BaselineFinding is one finding of a production reviewer (P1, P2 or P3).
type BaselineFinding struct {
	ID       string
	Severity string
	File     string
	Line     int
	Summary  string
}

// BaselineReview is one production review of the sample's round. Complete is
// false when the review recorded no valid findings payload.
type BaselineReview struct {
	TaskID   string
	Reviewer string
	Complete bool
	Findings []BaselineFinding
}

// BaselineSampleInput carries the production reviews of one sample's exact
// round and the commit that round pinned. PinProblem is set when the round
// does not pin exactly one commit.
type BaselineSampleInput struct {
	SampleID   string
	PinnedSHA  string
	PinProblem string
	Reviews    []BaselineReview
}

// ReportInput is everything BuildReport needs; it reads no store.
// Dispositions are in the order they were recorded, oldest first.
type ReportInput struct {
	CampaignID   string
	CampaignName string
	GeneratedAt  time.Time
	Samples      []ReportSample
	Candidates   []ReportCandidate
	Runs         []ReportRun
	Baselines    []BaselineSampleInput
	Dispositions []Disposition
}

// Coverage counts, against the campaign's whole sample set, what happened to
// one reviewer on each sample. Only Completed samples contribute findings or
// misses, and only Clean ones (completed with no finding) count as clean.
type Coverage struct {
	Samples     int `json:"samples"`
	Completed   int `json:"completed"`
	Clean       int `json:"clean"`
	Failed      int `json:"failed"`
	Unavailable int `json:"unavailable"`
	Incomplete  int `json:"incomplete"`
	InProgress  int `json:"in_progress"`
	NotRun      int `json:"not_run"`
	Excluded    int `json:"excluded"`
	Attempts    int `json:"attempts"`
}

// SampleOutcome is one reviewer's outcome on one sample.
type SampleOutcome struct {
	SampleID string `json:"sample_id"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail,omitempty"`
}

// Metrics are one reviewer's adjudicated numbers over its completed samples.
// Groups are deduplicated issues: a reviewer that reports the same claim twice
// on a sample counts once. Confirmed + Misses == KnownValid.
type Metrics struct {
	Findings       int      `json:"findings"`
	Groups         int      `json:"groups"`
	Confirmed      int      `json:"confirmed"`
	ConfirmedP1    int      `json:"confirmed_p1"`
	ConfirmedP2    int      `json:"confirmed_p2"`
	ConfirmedP3    int      `json:"confirmed_p3"`
	Unique         int      `json:"unique"`
	UniqueP1P2     int      `json:"unique_p1_p2"`
	FalsePositives int      `json:"false_positives"`
	Unresolved     int      `json:"unresolved"`
	KnownValid     int      `json:"known_valid"`
	Misses         int      `json:"misses"`
	MissesP1P2     int      `json:"misses_p1_p2"`
	Recall         *float64 `json:"recall_vs_known"`
}

// Latency summarizes the duration of a reviewer's counted completed attempts.
// Unknown counts completed samples whose duration was not recorded.
type Latency struct {
	Count    int      `json:"count"`
	Unknown  int      `json:"unknown"`
	MeanMs   *float64 `json:"mean_ms"`
	MedianMs *float64 `json:"median_ms"`
	MaxMs    *int     `json:"max_ms"`
}

// UsageUnit is the total of one provider-native unit over the attempts that
// reported it. It is never converted to another unit or to money.
type UsageUnit struct {
	Sum      float64 `json:"sum"`
	Attempts int     `json:"attempts"`
}

// ReviewerReport is one reviewer's row: a candidate version or a production
// baseline reviewer.
type ReviewerReport struct {
	Key                  string               `json:"key"`
	Kind                 string               `json:"kind"`
	Label                string               `json:"label"`
	CandidateID          string               `json:"candidate_id,omitempty"`
	CandidateDigest      string               `json:"candidate_digest,omitempty"`
	Identity             *CandidateIdentity   `json:"identity,omitempty"`
	EffectiveDigests     []string             `json:"effective_digests,omitempty"`
	RuntimeDrift         bool                 `json:"runtime_drift"`
	Coverage             Coverage             `json:"coverage"`
	Outcomes             []SampleOutcome      `json:"outcomes"`
	Metrics              Metrics              `json:"metrics"`
	Latency              *Latency             `json:"latency,omitempty"`
	Usage                map[string]UsageUnit `json:"usage,omitempty"`
	UsageUnknownAttempts int                  `json:"usage_unknown_attempts"`
}

// CommonReviewer is a reviewer's metrics restricted to the common samples.
type CommonReviewer struct {
	Key     string  `json:"key"`
	Metrics Metrics `json:"metrics"`
}

// CommonReport restricts every reviewer to the samples all of them completed,
// so unequal coverage cannot flatter or punish anyone.
type CommonReport struct {
	SampleIDs []string         `json:"sample_ids"`
	Reviewers []CommonReviewer `json:"reviewers"`
}

// GroupMember is one finding inside an issue group with its provenance and its
// latest disposition.
type GroupMember struct {
	Reviewer            string `json:"reviewer"`
	Ref                 string `json:"ref"`
	FindingID           string `json:"finding_id"`
	ReportedSeverity    string `json:"reported_severity"`
	Summary             string `json:"summary"`
	File                string `json:"file,omitempty"`
	Line                int    `json:"line,omitempty"`
	Label               string `json:"label"`
	DispositionSeverity string `json:"disposition_severity,omitempty"`
	Claim               string `json:"claim,omitempty"`
	Actor               string `json:"actor,omitempty"`
	Evidence            string `json:"evidence,omitempty"`
	DispositionID       string `json:"disposition_id,omitempty"`
	Revisions           int    `json:"revisions"`
}

// IssueGroup is one issue on one sample. Findings join a group only through
// the claim an operator recorded; findings nobody has labeled stay alone.
// Disagreement is true when members carry different labels or severities; both
// stay visible on the members.
type IssueGroup struct {
	ID               string        `json:"id"`
	SampleID         string        `json:"sample_id"`
	TaskID           string        `json:"task_id"`
	Round            int           `json:"round"`
	SHA              string        `json:"sha"`
	Claim            string        `json:"claim,omitempty"`
	Label            string        `json:"label"`
	Severity         string        `json:"severity,omitempty"`
	Disagreement     bool          `json:"disagreement"`
	Reviewers        []string      `json:"reviewers"`
	BaselineReported bool          `json:"baseline_reported"`
	CandidateOnly    bool          `json:"candidate_only"`
	Members          []GroupMember `json:"members"`
}

// AttentionItem is a material candidate-only finding for a human to look at.
// The report only lists it; nothing is sent and no task changes.
type AttentionItem struct {
	GroupID         string   `json:"group_id"`
	SampleID        string   `json:"sample_id"`
	TaskID          string   `json:"task_id"`
	Claim           string   `json:"claim,omitempty"`
	Label           string   `json:"label"`
	Severity        string   `json:"severity,omitempty"`
	Reviewers       []string `json:"reviewers"`
	Refs            []string `json:"refs"`
	Summary         string   `json:"summary"`
	BaselineCovered bool     `json:"baseline_covered"`
}

// DispositionTotals says how many recorded decisions the report used.
type DispositionTotals struct {
	Recorded int `json:"recorded"`
	Findings int `json:"findings_labeled"`
	Ignored  int `json:"ignored"`
}

// Report is the compact evaluation report.
type Report struct {
	Version           int               `json:"version"`
	CampaignID        string            `json:"campaign_id"`
	CampaignName      string            `json:"campaign_name"`
	GeneratedAt       string            `json:"generated_at"`
	Standard          string            `json:"standard"`
	SampleTotal       int               `json:"sample_total"`
	Reviewers         []ReviewerReport  `json:"reviewers"`
	Common            CommonReport      `json:"common"`
	Groups            []IssueGroup      `json:"groups"`
	Attention         []AttentionItem   `json:"attention"`
	UnlabeledFindings int               `json:"unlabeled_findings"`
	Dispositions      DispositionTotals `json:"dispositions"`
}

type runResult struct {
	outcome string
	detail  string
	counted *ReportAttempt
}

type reviewerState struct {
	report   ReviewerReport
	results  map[string]runResult
	findings map[string][]GroupMember
}

// BuildReport compares every reviewer on the exact same frozen samples. It is
// pure: it reads only its input and changes nothing.
func BuildReport(in ReportInput) Report {
	rep := Report{
		Version: ReportVersion, CampaignID: in.CampaignID, CampaignName: in.CampaignName,
		GeneratedAt: in.GeneratedAt.UTC().Format(time.RFC3339), Standard: ReportStandard,
		SampleTotal: len(in.Samples), Reviewers: []ReviewerReport{}, Groups: []IssueGroup{}, Attention: []AttentionItem{},
		Common: CommonReport{SampleIDs: []string{}, Reviewers: []CommonReviewer{}},
	}

	var states []*reviewerState
	runs := map[string]*ReportRun{}
	for i := range in.Runs {
		runs[in.Runs[i].CandidateID+"\x00"+in.Runs[i].SampleID] = &in.Runs[i]
	}
	for _, c := range in.Candidates {
		states = append(states, candidateState(c, in.Samples, runs))
	}
	states = append(states, baselineStates(in.Samples, in.Baselines)...)

	disps := map[dispKey][]Disposition{}
	for _, d := range in.Dispositions {
		k := dispKey{d.SampleID, d.SourceKind, d.SourceID, d.FindingID}
		disps[k] = append(disps[k], d)
	}
	used := map[dispKey]bool{}
	ignored := 0

	for _, s := range in.Samples {
		groups := groupSample(s, states, disps, used)
		rep.Groups = append(rep.Groups, groups...)
	}
	for _, d := range in.Dispositions {
		k := dispKey{d.SampleID, d.SourceKind, d.SourceID, d.FindingID}
		if !used[k] {
			ignored++
		}
	}
	for _, g := range rep.Groups {
		for _, m := range g.Members {
			if m.Label == GroupUnlabeled {
				rep.UnlabeledFindings++
			}
		}
	}
	rep.Dispositions = DispositionTotals{Recorded: len(in.Dispositions), Findings: len(used), Ignored: ignored}

	for _, st := range states {
		completed := map[string]bool{}
		for _, s := range in.Samples {
			if st.results[s.ID].outcome == OutcomeCompleted {
				completed[s.ID] = true
			}
		}
		st.report.Metrics = computeMetrics(st.report.Key, completed, rep.Groups)
		rep.Reviewers = append(rep.Reviewers, st.report)
	}

	rep.Common = commonReport(in.Samples, states, rep.Groups)
	rep.Attention = attention(in.Samples, states, rep.Groups)
	return rep
}

func candidateState(c ReportCandidate, samples []ReportSample, runs map[string]*ReportRun) *reviewerState {
	id := c.Identity
	st := &reviewerState{
		results:  map[string]runResult{},
		findings: map[string][]GroupMember{},
		report: ReviewerReport{
			Key: SourceCandidate + ":" + c.ID, Kind: SourceCandidate, CandidateID: c.ID, CandidateDigest: c.Digest,
			Label: fmt.Sprintf("%s/%s@%s", id.AdapterName, id.ModelID, id.ModelRevision), Identity: &id,
			Outcomes: []SampleOutcome{},
		},
	}
	st.report.Coverage.Samples = len(samples)
	effective := map[string]bool{}
	usage := map[string]UsageUnit{}
	var durations []int
	for _, s := range samples {
		run := runs[c.ID+"\x00"+s.ID]
		res := classifyRun(run, s, c)
		st.results[s.ID] = res
		st.report.Outcomes = append(st.report.Outcomes, SampleOutcome{SampleID: s.ID, Outcome: res.outcome, Detail: res.detail})
		if run != nil {
			for _, a := range run.Attempts {
				st.report.Coverage.Attempts++
				if a.EffectiveDigest != "" {
					effective[a.EffectiveDigest] = true
				}
				if a.State == EvalAttemptActive {
					continue
				}
				if len(a.Usage) == 0 {
					st.report.UsageUnknownAttempts++
				}
				for unit, v := range a.Usage {
					u := usage[unit]
					u.Sum += v
					u.Attempts++
					usage[unit] = u
				}
			}
		}
		if res.outcome == OutcomeCompleted {
			if res.counted.DurationMs != nil {
				durations = append(durations, *res.counted.DurationMs)
			}
			for _, f := range res.counted.Findings {
				st.findings[s.ID] = append(st.findings[s.ID], GroupMember{
					Reviewer: st.report.Key, Ref: FindingRef(SourceCandidate, res.counted.ID, f.ID), FindingID: f.ID,
					ReportedSeverity: string(f.Severity), Summary: f.Summary,
				})
			}
		}
	}
	tally(&st.report.Coverage, st.results, st.findings)
	for d := range effective {
		st.report.EffectiveDigests = append(st.report.EffectiveDigests, d)
		if d != c.Digest {
			st.report.RuntimeDrift = true
		}
	}
	sort.Strings(st.report.EffectiveDigests)
	if len(usage) > 0 {
		st.report.Usage = usage
	}
	lat := Latency{Count: len(durations)}
	lat.Unknown = st.report.Coverage.Completed - len(durations)
	if len(durations) > 0 {
		sort.Ints(durations)
		sum := 0
		for _, d := range durations {
			sum += d
		}
		mean := float64(sum) / float64(len(durations))
		median := float64(durations[len(durations)/2])
		if len(durations)%2 == 0 {
			median = float64(durations[len(durations)/2-1]+durations[len(durations)/2]) / 2
		}
		maxMs := durations[len(durations)-1]
		lat.MeanMs, lat.MedianMs, lat.MaxMs = &mean, &median, &maxMs
	}
	st.report.Latency = &lat
	return st
}

// Attempt states as the report sees them.
const (
	EvalAttemptActive  = "active"
	EvalAttemptExpired = "expired"
)

func classifyRun(run *ReportRun, s ReportSample, c ReportCandidate) runResult {
	if run == nil || len(run.Attempts) == 0 {
		return runResult{outcome: OutcomeNotRun, detail: "no attempt was started"}
	}
	attempts := append([]ReportAttempt(nil), run.Attempts...)
	sort.Slice(attempts, func(i, j int) bool { return attempts[i].Sequence < attempts[j].Sequence })

	var completed *ReportAttempt
	live := false
	for i := range attempts {
		a := &attempts[i]
		if a.State == EvalAttemptActive {
			live = true
		}
		if a.State != EvalAttemptActive && a.State != EvalAttemptExpired && a.ExitClass == "completed" {
			completed = a
		}
	}
	if completed != nil {
		switch {
		case !run.Staged:
			return runResult{outcome: OutcomeExcluded, detail: "no staging record: the frozen artifact the review ran on is unverified"}
		case s.SnapshotDigest == "" || run.StagedSnapshotDigest != s.SnapshotDigest:
			return runResult{outcome: OutcomeExcluded, detail: "staged artifact differs from the frozen sample (a later or corrected artifact)"}
		case run.StagedCandidateDigest != c.Digest:
			return runResult{outcome: OutcomeExcluded, detail: "staged for a different candidate configuration"}
		}
		return runResult{outcome: OutcomeCompleted, counted: completed}
	}
	if live {
		return runResult{outcome: OutcomeInProgress, detail: "an attempt is still running"}
	}
	last := attempts[len(attempts)-1]
	exit := last.ExitClass
	if last.State == EvalAttemptExpired && exit == "" {
		exit = "lease_expired"
	}
	switch exit {
	case "unavailable_snapshot", "unavailable_source":
		return runResult{outcome: OutcomeUnavailable, detail: exit}
	case "incomplete_output", "invalid_output":
		return runResult{outcome: OutcomeIncomplete, detail: exit}
	}
	return runResult{outcome: OutcomeFailed, detail: exit}
}

func tally(c *Coverage, results map[string]runResult, findings map[string][]GroupMember) {
	for id, r := range results {
		switch r.outcome {
		case OutcomeCompleted:
			c.Completed++
			if len(findings[id]) == 0 {
				c.Clean++
			}
		case OutcomeFailed:
			c.Failed++
		case OutcomeUnavailable:
			c.Unavailable++
		case OutcomeIncomplete:
			c.Incomplete++
		case OutcomeInProgress:
			c.InProgress++
		case OutcomeNotRun:
			c.NotRun++
		case OutcomeExcluded:
			c.Excluded++
		}
	}
}

func baselineStates(samples []ReportSample, inputs []BaselineSampleInput) []*reviewerState {
	bySample := map[string]BaselineSampleInput{}
	names := map[string]bool{}
	for _, b := range inputs {
		bySample[b.SampleID] = b
		for _, r := range b.Reviews {
			names[r.Reviewer] = true
		}
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var out []*reviewerState
	for _, name := range sorted {
		st := &reviewerState{
			results:  map[string]runResult{},
			findings: map[string][]GroupMember{},
			report: ReviewerReport{
				Key: SourceBaseline + ":" + name, Kind: SourceBaseline, Label: name, Outcomes: []SampleOutcome{},
			},
		}
		st.report.Coverage.Samples = len(samples)
		for _, s := range samples {
			b, ok := bySample[s.ID]
			var mine []BaselineReview
			for _, r := range b.Reviews {
				if r.Reviewer == name {
					mine = append(mine, r)
				}
			}
			st.report.Coverage.Attempts += len(mine)
			res := runResult{outcome: OutcomeNotRun, detail: "no production review of this round"}
			switch {
			case !ok || len(mine) == 0:
			case b.PinProblem != "":
				res = runResult{outcome: OutcomeExcluded, detail: "round commit is not pinned: " + b.PinProblem}
			case b.PinnedSHA != s.SHA:
				res = runResult{outcome: OutcomeExcluded, detail: "reviewed commit differs from the frozen sample (a later or corrected artifact)"}
			default:
				res = runResult{outcome: OutcomeCompleted}
				for _, r := range mine {
					if !r.Complete {
						res = runResult{outcome: OutcomeIncomplete, detail: "a production review recorded no valid findings"}
					}
				}
			}
			st.results[s.ID] = res
			st.report.Outcomes = append(st.report.Outcomes, SampleOutcome{SampleID: s.ID, Outcome: res.outcome, Detail: res.detail})
			if res.outcome != OutcomeCompleted {
				continue
			}
			for _, r := range mine {
				for _, f := range r.Findings {
					st.findings[s.ID] = append(st.findings[s.ID], GroupMember{
						Reviewer: st.report.Key, Ref: FindingRef(SourceBaseline, r.TaskID, f.ID), FindingID: f.ID,
						ReportedSeverity: f.Severity, Summary: f.Summary, File: f.File, Line: f.Line,
					})
				}
			}
		}
		tally(&st.report.Coverage, st.results, st.findings)
		out = append(out, st)
	}
	return out
}

func groupSample(s ReportSample, states []*reviewerState, disps map[dispKey][]Disposition, used map[dispKey]bool) []IssueGroup {
	byClaim := map[string]*IssueGroup{}
	var order []*IssueGroup
	for _, st := range states {
		for _, m := range st.findings[s.ID] {
			kind, source, finding, _ := ParseFindingRef(m.Ref)
			k := dispKey{s.ID, kind, source, finding}
			var latest *Disposition
			if hist := disps[k]; len(hist) > 0 {
				latest = &hist[len(hist)-1]
				used[k] = true
				m.Revisions = len(hist)
			}
			var g *IssueGroup
			if latest == nil {
				m.Label = GroupUnlabeled
				g = &IssueGroup{ID: s.ID + "#unlabeled:" + m.Ref}
				order = append(order, g)
			} else {
				m.Label, m.DispositionSeverity = string(latest.Label), latest.Severity
				m.Claim, m.Actor, m.Evidence, m.DispositionID = NormalizeClaim(latest.Claim), latest.Actor, latest.Evidence, latest.ID
				key := claimKey(latest.Claim)
				if g = byClaim[key]; g == nil {
					g = &IssueGroup{ID: s.ID + "#" + key, Claim: m.Claim}
					byClaim[key] = g
					order = append(order, g)
				}
			}
			g.SampleID, g.TaskID, g.Round, g.SHA = s.ID, s.TaskID, s.Round, s.SHA
			g.Members = append(g.Members, m)
		}
	}
	out := make([]IssueGroup, 0, len(order))
	for _, g := range order {
		finishGroup(g)
		out = append(out, *g)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func finishGroup(g *IssueGroup) {
	labels := map[string]bool{}
	sevs := map[string]bool{}
	reviewers := map[string]bool{}
	for _, m := range g.Members {
		reviewers[m.Reviewer] = true
		if strings.HasPrefix(m.Reviewer, SourceBaseline+":") {
			g.BaselineReported = true
		}
		if m.Label != GroupUnlabeled {
			labels[m.Label] = true
			sevs[m.DispositionSeverity] = true
		}
	}
	g.CandidateOnly = !g.BaselineReported
	for r := range reviewers {
		g.Reviewers = append(g.Reviewers, r)
	}
	sort.Strings(g.Reviewers)
	for s := range sevs {
		if g.Severity == "" || severityRank(s) < severityRank(g.Severity) {
			g.Severity = s
		}
	}
	g.Disagreement = len(labels) > 1 || len(sevs) > 1
	switch {
	case len(labels) == 0:
		g.Label = GroupUnlabeled
	case labels[string(LabelValid)] && labels[string(LabelInvalid)]:
		g.Label = GroupDisputed
	case labels[string(LabelValid)]:
		g.Label = string(LabelValid)
	case labels[string(LabelInvalid)]:
		g.Label = string(LabelInvalid)
	default:
		g.Label = string(LabelUnresolved)
	}
	sort.SliceStable(g.Members, func(i, j int) bool {
		if g.Members[i].Reviewer != g.Members[j].Reviewer {
			return g.Members[i].Reviewer < g.Members[j].Reviewer
		}
		return g.Members[i].Ref < g.Members[j].Ref
	})
}

func computeMetrics(key string, samples map[string]bool, groups []IssueGroup) Metrics {
	var m Metrics
	for _, g := range groups {
		if !samples[g.SampleID] {
			continue
		}
		reported := false
		for _, mem := range g.Members {
			if mem.Reviewer == key {
				m.Findings++
				reported = true
			}
		}
		p1p2 := g.Severity == SeverityP1 || g.Severity == SeverityP2
		valid := g.Label == string(LabelValid)
		if valid {
			m.KnownValid++
		}
		if !reported {
			if valid {
				m.Misses++
				if p1p2 {
					m.MissesP1P2++
				}
			}
			continue
		}
		m.Groups++
		switch g.Label {
		case string(LabelValid):
			m.Confirmed++
			switch g.Severity {
			case SeverityP1:
				m.ConfirmedP1++
			case SeverityP2:
				m.ConfirmedP2++
			default:
				m.ConfirmedP3++
			}
			if len(g.Reviewers) == 1 {
				m.Unique++
				if p1p2 {
					m.UniqueP1P2++
				}
			}
		case string(LabelInvalid):
			m.FalsePositives++
		default:
			m.Unresolved++
		}
	}
	if m.KnownValid > 0 {
		r := float64(m.Confirmed) / float64(m.KnownValid)
		m.Recall = &r
	}
	return m
}

func commonReport(samples []ReportSample, states []*reviewerState, groups []IssueGroup) CommonReport {
	out := CommonReport{SampleIDs: []string{}, Reviewers: []CommonReviewer{}}
	if len(states) == 0 {
		return out
	}
	common := map[string]bool{}
	for _, s := range samples {
		all := true
		for _, st := range states {
			if st.results[s.ID].outcome != OutcomeCompleted {
				all = false
				break
			}
		}
		if all {
			common[s.ID] = true
			out.SampleIDs = append(out.SampleIDs, s.ID)
		}
	}
	for _, st := range states {
		out.Reviewers = append(out.Reviewers, CommonReviewer{Key: st.report.Key, Metrics: computeMetrics(st.report.Key, common, groups)})
	}
	return out
}

func attention(samples []ReportSample, states []*reviewerState, groups []IssueGroup) []AttentionItem {
	baselineCovered := map[string]bool{}
	for _, st := range states {
		if st.report.Kind != SourceBaseline {
			continue
		}
		for _, s := range samples {
			if st.results[s.ID].outcome == OutcomeCompleted {
				baselineCovered[s.ID] = true
			}
		}
	}
	out := []AttentionItem{}
	for _, g := range groups {
		if !g.CandidateOnly || g.Label == string(LabelInvalid) {
			continue
		}
		material := g.Severity == SeverityP1 || g.Severity == SeverityP2
		var refs []string
		summary := ""
		for _, m := range g.Members {
			refs = append(refs, m.Ref)
			if m.ReportedSeverity == string(SeverityMaterial) {
				material = true
			}
			if summary == "" {
				summary = m.Summary
			}
		}
		if !material {
			continue
		}
		out = append(out, AttentionItem{
			GroupID: g.ID, SampleID: g.SampleID, TaskID: g.TaskID, Claim: g.Claim, Label: g.Label, Severity: g.Severity,
			Reviewers: g.Reviewers, Refs: refs, Summary: summary, BaselineCovered: baselineCovered[g.SampleID],
		})
	}
	return out
}
