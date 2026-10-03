package evalcohort

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

const fakeAdapterArg = "__fake_adapter__"

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == fakeAdapterArg {
		os.Exit(evaluation.FakeMain(os.Args[2:], os.Stderr))
	}
	os.Exit(m.Run())
}

const (
	findingText = "Claim 3 misstates the holding of the cited opinion"
	fixNoteText = "fixed claim three after reviewer feedback"
	fixFileText = "FIXED-LATER: claim three now quotes the holding"
	prURL       = "https://github.com/org/repo/pull/102"
	contextText = "primary source excerpt for the opinion"
)

var p1Finding = `[{"id":"f1","severity":"P1","file":"research.md","line":3,"summary":"` + findingText + `","in_changed_text":true,"status":"new"}]`

type env struct {
	t     *testing.T
	ctx   context.Context
	st    store.Store
	repo  string
	proj  string
	proj2 string
	doc   map[string]string
	root  string
	tasks map[string]string
	shas  map[string]string
}

func (e *env) git(args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", e.repo}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@example.com", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@example.com",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) commit(name, content string) string {
	e.t.Helper()
	p := filepath.Join(e.repo, "research", name+".md")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	e.git("add", "-A")
	e.git("commit", "-q", "-m", "add "+name)
	return e.git("rev-parse", "HEAD")
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), repo: t.TempDir(), root: filepath.Join(t.TempDir(), "workspaces"),
		doc: map[string]string{}, tasks: map[string]string{}, shas: map[string]string{}}
	st, err := store.Open(filepath.Join(t.TempDir(), "cohort.db"), []string{"haiku", "sonnet", "opus"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	for _, p := range []*string{&e.proj, &e.proj2} {
		proj, err := st.CreateProject(e.ctx, "research", "https://example.com/org/repo")
		if err != nil {
			t.Fatal(err)
		}
		*p = proj.ID
		doc, err := st.CreateDocument(e.ctx, proj.ID, "feature_spec", "doc", "doc.md", nil)
		if err != nil {
			t.Fatal(err)
		}
		e.doc[proj.ID] = doc.ID
	}
	e.git("init", "-q")
	if err := os.MkdirAll(filepath.Join(e.repo, "sources"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.repo, "sources", "opinion.txt"), []byte(contextText), 0o644); err != nil {
		t.Fatal(err)
	}
	e.git("add", "-A")
	e.git("commit", "-q", "-m", "sources")
	return e
}

func (e *env) sources() Sources {
	return SourceMap{e.proj: evaluation.GitSource{Dir: e.repo}, e.proj2: evaluation.GitSource{Dir: e.repo}}
}

func (e *env) newTask(proj, name string, links []store.LinkInput, p1 bool) string {
	e.t.Helper()
	tasks, err := e.st.CreateTasks(e.ctx, proj, []store.TaskInput{{
		Title: name, Spec: "Acceptance: " + name + " cites a primary source for every claim.", DocumentID: e.doc[proj],
		Model: "haiku", ReviewModels: []string{"opus", "sonnet"}, Track: "research",
	}})
	if err != nil {
		e.t.Fatal(err)
	}
	id := tasks[0].ID
	e.tasks[name] = id
	if _, err := e.st.PromoteTask(e.ctx, id); err != nil {
		e.t.Fatal(err)
	}
	e.submit(id, "first submission", links)
	findings := map[string]string{"opus": `[]`, "sonnet": `[]`}
	if p1 {
		findings["opus"] = p1Finding
	}
	for _, model := range []string{"opus", "sonnet"} {
		e.review(proj, id, 1, model, findings[model])
	}
	return id
}

func (e *env) submit(id, note string, links []store.LinkInput) {
	e.t.Helper()
	if _, err := e.st.ClaimTask(e.ctx, id, "agent-1", "haiku", 5*time.Minute); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.st.SubmitTask(e.ctx, id, "agent-1", note, nil, links, 8, nil, nil, 999999); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) review(proj, id string, round int, model, findings string) {
	e.t.Helper()
	tasks, err := e.st.ListTasks(e.ctx, proj, store.TaskListFilter{})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, tk := range tasks {
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == id && tk.ReviewRound == round && tk.State == "ready" && tk.Model == model {
			if _, err := e.st.ClaimTask(e.ctx, tk.ID, model+"-agent", model, 5*time.Minute); err != nil {
				e.t.Fatal(err)
			}
			verdict := "approve"
			if findings != "[]" {
				verdict = "reject"
			}
			if _, err := e.st.SubmitTask(e.ctx, tk.ID, model+"-agent", "review notes", &verdict, []store.LinkInput{}, 8, nil, nil, 999999, json.RawMessage(findings)); err != nil {
				e.t.Fatal(err)
			}
			return
		}
	}
	e.t.Fatalf("no ready %s review task in round %d for %s", model, round, id)
}

func commitLink(sha string) []store.LinkInput {
	return []store.LinkInput{{Kind: "pr", Value: prURL}, {Kind: "commit", Value: sha}}
}

// seed creates the board and repository history used by most tests. The
// rejected submission is fixed in a later commit with a later note.
func (e *env) seed() {
	e.t.Helper()
	e.shas["clean1"] = e.commit("clean1", "Claim one cites the opinion.\n")
	e.shas["clean2"] = e.commit("clean2", "Claim two cites the statute.\n")
	e.shas["rejected"] = e.commit("rejected", "Claim three cites the opinion for a holding it does not contain.\n")
	e.newTask(e.proj, "clean1", commitLink(e.shas["clean1"]), false)
	e.newTask(e.proj, "clean2", []store.LinkInput{{Kind: "commit", Value: e.shas["clean2"]}}, false)
	rej := e.newTask(e.proj, "rejected", commitLink(e.shas["rejected"]), true)
	e.shas["fix"] = e.commit("rejected", "Claim three cites the opinion for a holding it does not contain.\n"+fixFileText+"\n")
	e.submit(rej, fixNoteText, []store.LinkInput{{Kind: "pr", Value: prURL}, {Kind: "commit", Value: e.shas["fix"]}})

	e.newTask(e.proj, "missing", []store.LinkInput{{Kind: "commit", Value: strings.Repeat("e", 40)}}, false)
	e.newTask(e.proj, "nocommit", []store.LinkInput{{Kind: "pr", Value: "https://github.com/org/repo/pull/103"}}, false)
	e.shas["tainted"] = e.commit("tainted", "See also "+"https://github.com/org/repo/pull/104"+" for discussion.\n")
	e.newTask(e.proj, "tainted", []store.LinkInput{{Kind: "commit", Value: e.shas["tainted"]}}, false)

	// A snapshot that quotes a sealed reviewer finding (not a link) is also contaminated.
	e.shas["echoed"] = e.commit("echoed", "Reviewers said: "+findingText+".\n")
	e.newTask(e.proj, "echoed", []store.LinkInput{{Kind: "commit", Value: e.shas["echoed"]}}, true)

	other := e.commit("otherproject", "Another project's work.\n")
	e.newTask(e.proj2, "otherproject", []store.LinkInput{{Kind: "commit", Value: other}}, false)
}

func (e *env) criteria() evaluation.CohortCriteria {
	return evaluation.CohortCriteria{ProjectIDs: []string{e.proj}, SampleCap: 12, Seed: "round-one-v1", ContextPaths: []string{"sources"}}
}

func (e *env) builder() Builder { return Builder{Store: e.st, Sources: e.sources()} }

func (e *env) build() evaluation.CohortManifest {
	e.t.Helper()
	m, err := e.builder().Build(e.ctx, e.criteria())
	if err != nil {
		e.t.Fatal(err)
	}
	return m
}

func sampleFor(t *testing.T, e *env, m evaluation.CohortManifest, name string) evaluation.CohortSample {
	t.Helper()
	s, ok := m.Sample(evaluation.SampleKey(e.proj, e.tasks[name], 1))
	if !ok {
		t.Fatalf("no sample for %s in %+v", name, m.Samples)
	}
	return s
}

func TestBuildPinsOriginalRoundOneSubmission(t *testing.T) {
	e := newEnv(t)
	e.seed()
	m := e.build()

	rej := sampleFor(t, e, m, "rejected")
	if rej.SubmittedSHA != e.shas["rejected"] || rej.SubmittedSHA == e.shas["fix"] {
		t.Fatalf("pinned %s, want the round 1 commit %s and never the fix %s", rej.SubmittedSHA, e.shas["rejected"], e.shas["fix"])
	}
	if rej.Outcome != evaluation.OutcomeRejectedMaterial || !rej.Available || rej.SelectionReason == "" {
		t.Fatalf("rejected sample: %+v", rej)
	}
	if c := sampleFor(t, e, m, "clean1"); c.Outcome != evaluation.OutcomeClean || c.SubmittedSHA != e.shas["clean1"] {
		t.Fatalf("clean sample: %+v", c)
	}

	// The frozen digest is the digest of the pre-fix workspace, not the fixed one.
	src := evaluation.GitSource{Dir: e.repo}
	digestAt := func(sha string) string {
		files, err := src.ChangedFiles(e.ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		ctxFiles, err := src.TreeFiles(e.ctx, sha, []string{"sources"})
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := evaluation.WorkspaceSpec{Artifact: files, Context: ctxFiles, Acceptance: "Acceptance: rejected cites a primary source for every claim."}.Digests()
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	if rej.SnapshotDigest != digestAt(e.shas["rejected"]) {
		t.Fatal("frozen digest is not the digest of the round 1 content")
	}
	if rej.SnapshotDigest == digestAt(e.shas["fix"]) {
		t.Fatal("frozen digest equals the fixed content's digest")
	}
	if _, ok := m.Sample(evaluation.SampleKey(e.proj2, e.tasks["otherproject"], 1)); ok {
		t.Fatal("a project that was not selected contributed a sample")
	}
}

func TestBuildIsDeterministicAndRecordsDenominators(t *testing.T) {
	e := newEnv(t)
	e.seed()
	a, b := e.build(), e.build()
	ea, _ := a.Encode()
	eb, _ := b.Encode()
	if ea != eb || a.Digest != b.Digest {
		t.Fatal("two builds from the same board, repository, criteria and seed differ")
	}
	if err := a.Verify(); err != nil {
		t.Fatal(err)
	}
	d := a.Denominators
	if d.EligibleClean != 5 || d.EligibleRejected != 2 || d.Available != 3 || d.Unavailable != 4 || len(a.Samples) != 7 {
		t.Fatalf("denominators: %+v", d)
	}
	want := map[string]int{
		string(evaluation.UnavailArtifactUnreachable): 1, // commit not in the repository
		string(evaluation.UnavailNoCommit):            1,
		string(evaluation.UnavailContaminated):        2,
	}
	if !reflect.DeepEqual(d.UnavailableByReason, want) {
		t.Fatalf("unavailable by reason: %v", d.UnavailableByReason)
	}
	for _, name := range []string{"missing", "nocommit", "tainted", "echoed"} {
		s := sampleFor(t, e, a, name)
		if s.Available || s.UnavailableReason == "" || s.UnavailableDetail == "" || s.SnapshotDigest != "" {
			t.Fatalf("%s must be visibly unavailable: %+v", name, s)
		}
	}
	if s := sampleFor(t, e, a, "tainted"); strings.Contains(s.UnavailableDetail, "https://") {
		t.Fatalf("unavailable detail must name where, not repeat the leaked text: %q", s.UnavailableDetail)
	}
	if s := sampleFor(t, e, a, "echoed"); s.UnavailableReason != evaluation.UnavailContaminated || strings.Contains(s.UnavailableDetail, findingText) {
		t.Fatalf("a quoted reviewer finding must contaminate without being repeated: %+v", s)
	}
}

func TestBuildSeedAndCapChangeSelection(t *testing.T) {
	e := newEnv(t)
	e.seed()
	crit := e.criteria()
	crit.SampleCap = 2
	m, err := e.builder().Build(e.ctx, crit)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Samples) != 2 || m.Denominators.SelectedClean != 1 || m.Denominators.SelectedRejected != 1 {
		t.Fatalf("cap 2 selected %+v", m.Denominators)
	}
	crit.Seed = ""
	if _, err := e.builder().Build(e.ctx, crit); err == nil {
		t.Fatal("a cohort without a seed must be refused")
	}
	crit = e.criteria()
	crit.ProjectIDs = nil
	if _, err := e.builder().Build(e.ctx, crit); err == nil {
		t.Fatal("a cohort without explicitly selected projects must be refused")
	}
}

func TestBuildRejectsTamperedManifestDigest(t *testing.T) {
	e := newEnv(t)
	e.seed()
	id := e.tasks["clean1"]
	body := `{"children":[]}`
	if _, err := e.st.Conn().ExecContext(e.ctx,
		`INSERT INTO task_submission_manifest (id, task_id, review_round, parent_task_id, manifest_json, manifest_digest, created_at) VALUES (?, ?, 1, ?, ?, 'not-the-digest', '2026-01-01T00:00:00Z')`,
		store.GenerateID(), id, id, body); err != nil {
		t.Fatal(err)
	}
	s := sampleFor(t, e, e.build(), "clean1")
	if s.Available || s.UnavailableReason != evaluation.UnavailManifestCorrupt {
		t.Fatalf("a manifest that does not match its digest must make the sample unavailable: %+v", s)
	}
}

func registerCohort(t *testing.T, e *env) (Registered, evaluation.CohortManifest) {
	t.Helper()
	m := e.build()
	reg, err := e.builder().Register(e.ctx, CampaignSpec{Name: "round-one", AllowedModelIDs: []string{"model-a", "model-b"}, AttemptCap: 50}, m)
	if err != nil {
		t.Fatal(err)
	}
	return reg, m
}

func TestRegisterFreezesCohortIntoCampaign(t *testing.T) {
	e := newEnv(t)
	e.seed()
	reg, m := registerCohort(t, e)

	got, err := evaluation.DecodeCohortManifest(reg.Campaign.CohortManifest)
	if err != nil || got.Digest != m.Digest {
		t.Fatalf("campaign manifest: %v", err)
	}
	if len(reg.Samples) != 3 {
		t.Fatalf("frozen samples = %d, want only the 3 available ones", len(reg.Samples))
	}
	stored, err := e.st.ListEvaluationSamples(e.ctx, reg.Campaign.ID)
	if err != nil || len(stored) != 3 {
		t.Fatalf("stored samples: %v %v", err, stored)
	}
	for _, s := range stored {
		cs, ok := m.Sample(evaluation.SampleKey(s.ProjectID, s.OriginalTaskID, s.OriginalReviewRound))
		if !ok || s.SubmittedSHA != cs.SubmittedSHA || s.SnapshotDigest == nil || *s.SnapshotDigest != cs.SnapshotDigest ||
			s.PromptVersion != evaluation.BlindedPromptVersion {
			t.Fatalf("stored sample %+v disagrees with cohort sample %+v", s, cs)
		}
	}
	unsealed := m
	unsealed.Digest = ""
	if _, err := e.builder().Register(e.ctx, CampaignSpec{Name: "x", AllowedModelIDs: []string{"model-a"}, AttemptCap: 1}, unsealed); err == nil {
		t.Fatal("an unsealed manifest must not be registered")
	}
}

type runner struct {
	e   *env
	reg *evaluation.Registry
	p   *evaluation.Pipeline
}

func newRunner(e *env) *runner {
	if _, err := e.st.ConfigureEvaluationPool(e.ctx, store.EvaluationPoolConfig{ID: "pool1", ConcurrencyOnly: true, ConcurrentLimit: 100}); err != nil {
		e.t.Fatal(err)
	}
	reg := evaluation.NewRegistry()
	return &runner{e: e, reg: reg, p: &evaluation.Pipeline{Registry: reg, Credentials: evaluation.MapCredentials{}}}
}

func (r *runner) addCandidate(campaignID, model string, tools evaluation.NameSet) (store.EvaluationCandidate, string) {
	t := r.e.t
	t.Helper()
	id := evaluation.CandidateIdentity{
		AdapterName: "fake", AdapterVersion: "v1", ModelID: model, ModelRevision: "unknown", RuntimeName: "rt-" + model,
		RuntimeVersion: "v1", ReasoningSettings: evaluation.UnknownSettings(), GenerationSettings: evaluation.UnknownSettings(),
		PromptVersion: evaluation.BlindedPromptVersion, Tools: tools, Observers: evaluation.UnknownNames(), AccountPool: "pool1",
	}
	cfg, err := evaluation.NewCandidateConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	idJSON, _ := json.Marshal(id)
	if err := r.reg.Register(evaluation.Runtime{
		Name: id.RuntimeName, Executable: exe, Timeout: 30 * time.Second, Candidate: cfg,
		Capabilities: []string{evaluation.CapStructuredOutput},
		Args:         []string{fakeAdapterArg, "--mode", "success", "--request", evaluation.PlaceholderRequestPath, "--identity", string(idJSON)},
	}); err != nil {
		t.Fatal(err)
	}
	cand, err := r.e.st.CreateEvaluationCandidate(r.e.ctx, store.EvaluationCandidate{ID: store.GenerateID(), CampaignID: campaignID, Config: cfg, PerCandidateCap: 10})
	if err != nil {
		t.Fatal(err)
	}
	return cand, id.RuntimeName
}

func (e *env) stager() Stager { return Stager{Store: e.st, Sources: e.sources(), Root: e.root} }

// run claims a job, stages the sample for the candidate, runs it through the
// M1 pipeline and finalizes the attempt.
func (r *runner) run(reg Registered, key string, cand store.EvaluationCandidate, runtime string) (*Staged, store.EvaluationAttempt) {
	t, e := r.e.t, r.e
	t.Helper()
	sample := reg.Samples[key]
	claim, err := e.st.ClaimEvaluationJob(e.ctx, store.EvaluationJobClaim{SampleID: sample.ID, CandidateID: cand.ID, RequestID: "req-" + store.GenerateID(), LeaseExpires: 5 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	staged, err := e.stager().Stage(e.ctx, StageRequest{CampaignID: reg.Campaign.ID, SampleID: sample.ID, CandidateID: cand.ID, RunID: claim.Attempt.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.Validate(); err != nil {
		t.Fatal(err)
	}
	res, err := r.p.Execute(e.ctx, runtime, staged.Request)
	if err != nil {
		t.Fatal(err)
	}
	status := res.Response.Status
	if err := e.st.FinalizeEvaluationAttempt(e.ctx, store.EvaluationAttemptResult{
		AttemptID: claim.Attempt.ID, FenceAttemptID: claim.Attempt.ID, ExitClass: store.EvalExitCompleted, Status: &status, Findings: res.Response.Findings,
	}); err != nil {
		t.Fatal(err)
	}
	attempt, err := e.st.GetEvaluationAttempt(e.ctx, claim.Attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	return staged, attempt
}

func walk(t *testing.T, dir string, fn func(rel string, content string)) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			fn(rel+"/", "")
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fn(rel, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStagedWorkspaceHoldsNoHistoryFixesOrFeedback(t *testing.T) {
	e := newEnv(t)
	e.seed()
	reg, _ := registerCohort(t, e)
	r := newRunner(e)
	cand, runtime := r.addCandidate(reg.Campaign.ID, "model-a", evaluation.UnknownNames())
	key := evaluation.SampleKey(e.proj, e.tasks["rejected"], 1)
	staged, attempt := r.run(reg, key, cand, runtime)
	if attempt.State != store.EvalAttemptFinalized {
		t.Fatalf("attempt: %+v", attempt)
	}

	forbidden := []string{findingText, fixNoteText, fixFileText, prURL, "pull/", e.shas["fix"], "review notes", "reviewer"}
	var seen []string
	walk(t, staged.Workspace.Path, func(rel, content string) {
		seen = append(seen, rel)
		if strings.Contains(rel, ".git") {
			t.Errorf("git metadata staged: %s", rel)
		}
		for _, f := range forbidden {
			if strings.Contains(content, f) || strings.Contains(rel, f) {
				t.Errorf("%s contains %q", rel, f)
			}
		}
	})
	for _, f := range forbidden {
		if strings.Contains(staged.Request.BlindedPrompt, f) {
			t.Errorf("prompt contains %q", f)
		}
	}
	for _, s := range []string{string(evaluation.OutcomeRejectedMaterial), "rejected_material", "round 1", "first-round"} {
		if strings.Contains(strings.ToLower(staged.Request.BlindedPrompt), strings.ToLower(s)) {
			t.Errorf("prompt reveals %q", s)
		}
	}
	art, err := os.ReadFile(filepath.Join(staged.Workspace.Path, "artifact", "research", "rejected.md"))
	if err != nil || strings.Contains(string(art), "FIXED-LATER") || !strings.Contains(string(art), "Claim three") {
		t.Fatalf("artifact must be the pre-fix content: %v %q", err, art)
	}
	ctxFile, err := os.ReadFile(filepath.Join(staged.Workspace.Path, "context", "sources", "opinion.txt"))
	if err != nil || string(ctxFile) != contextText {
		t.Fatalf("context: %v %q", err, ctxFile)
	}
	acc, err := os.ReadFile(filepath.Join(staged.Workspace.Path, "ACCEPTANCE.md"))
	if err != nil || string(acc) != "Acceptance: rejected cites a primary source for every claim." {
		t.Fatalf("acceptance criteria must be complete: %v %q", err, acc)
	}
	if !strings.Contains(staged.Request.BlindedPrompt, string(acc)) {
		t.Fatal("prompt must carry the full acceptance criteria")
	}
	if filepath.IsAbs(staged.Request.ResultPath) == false || strings.HasPrefix(staged.Request.ResultPath, staged.Workspace.Path) {
		t.Fatalf("result path must be absolute and outside the workspace: %s", staged.Request.ResultPath)
	}

	// Recorded digests and limits.
	sm := reg.Samples[key]
	if staged.Staging.SnapshotDigest != *sm.SnapshotDigest || staged.Staging.SourceDigest != *sm.SourceDigest {
		t.Fatal("staging digests differ from the frozen sample")
	}
	var limits CandidateLimits
	if err := json.Unmarshal([]byte(staged.Staging.LimitsJSON), &limits); err != nil {
		t.Fatal(err)
	}
	if limits.WebDiscoverability != WebBlindingNotEnforced || limits.SourceScope == "" || !reflect.DeepEqual(limits.ContextPaths, []string{"sources"}) {
		t.Fatalf("limits: %+v", limits)
	}

	// Digest validation catches tampering.
	if err := os.WriteFile(filepath.Join(staged.Workspace.Path, "artifact", "research", "rejected.md"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := staged.Validate(); !errors.Is(err, evaluation.ErrDigestMismatch) {
		t.Fatalf("tampered workspace validated: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staged.Workspace.Path, "artifact", "extra.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := staged.Validate(); err == nil {
		t.Fatal("an added file must fail validation")
	}
}

func TestSecondCandidateOnFrozenCohortLeavesFirstUntouched(t *testing.T) {
	e := newEnv(t)
	e.seed()
	reg, m := registerCohort(t, e)
	r := newRunner(e)
	key := evaluation.SampleKey(e.proj, e.tasks["rejected"], 1)

	candA, rtA := r.addCandidate(reg.Campaign.ID, "model-a", evaluation.UnknownNames())
	stagedA, attemptA := r.run(reg, key, candA, rtA)
	findingsA, err := e.st.ListEvaluationFindings(e.ctx, attemptA.ID)
	if err != nil {
		t.Fatal(err)
	}
	samplesBefore, _ := e.st.ListEvaluationSamples(e.ctx, reg.Campaign.ID)
	stagingA, _ := e.st.GetEvaluationStaging(e.ctx, reg.Campaign.ID, reg.Samples[key].ID, candA.ID)
	campaignBefore, _ := e.st.GetEvaluationCampaign(e.ctx, reg.Campaign.ID)
	var filesA []string
	var bytesA []string
	walk(t, stagedA.Workspace.Path, func(rel, content string) { filesA = append(filesA, rel); bytesA = append(bytesA, content) })

	candB, rtB := r.addCandidate(reg.Campaign.ID, "model-b", evaluation.KnownNames("web_fetch"))
	stagedB, attemptB := r.run(reg, key, candB, rtB)

	if stagedA.Workspace.Path == stagedB.Workspace.Path || strings.HasPrefix(stagedB.Workspace.Path, filepath.Dir(stagedA.Workspace.Path)+string(filepath.Separator)) && filepath.Dir(stagedA.Workspace.Path) == filepath.Dir(stagedB.Workspace.Path) {
		t.Fatalf("candidates share a workspace tree: %s %s", stagedA.Workspace.Path, stagedB.Workspace.Path)
	}
	if stagedA.Request.ResultPath == stagedB.Request.ResultPath {
		t.Fatal("candidates share a result path")
	}
	if stagedA.Staging.SnapshotDigest != stagedB.Staging.SnapshotDigest {
		t.Fatal("both candidates must see the same frozen snapshot")
	}
	if stagedA.Staging.LimitsJSON == stagedB.Staging.LimitsJSON || stagedA.Staging.CandidateConfigDigest == stagedB.Staging.CandidateConfigDigest {
		t.Fatal("limits and candidate identity must be candidate-specific")
	}
	if attemptB.ID == attemptA.ID {
		t.Fatal("candidates share an attempt")
	}

	// Nothing about the first candidate or the frozen cohort changed.
	if got, _ := e.st.GetEvaluationAttempt(e.ctx, attemptA.ID); !reflect.DeepEqual(got, attemptA) {
		t.Fatalf("first attempt changed: %+v -> %+v", attemptA, got)
	}
	if got, _ := e.st.ListEvaluationFindings(e.ctx, attemptA.ID); !reflect.DeepEqual(got, findingsA) {
		t.Fatal("first candidate's findings changed")
	}
	if got, _ := e.st.ListEvaluationSamples(e.ctx, reg.Campaign.ID); !reflect.DeepEqual(got, samplesBefore) {
		t.Fatal("frozen samples changed")
	}
	if got, _ := e.st.GetEvaluationStaging(e.ctx, reg.Campaign.ID, reg.Samples[key].ID, candA.ID); got != stagingA {
		t.Fatalf("first staging record changed: %+v -> %+v", stagingA, got)
	}
	if got, _ := e.st.GetEvaluationCampaign(e.ctx, reg.Campaign.ID); got.CohortManifest != campaignBefore.CohortManifest {
		t.Fatal("campaign cohort manifest changed")
	}
	if dec, err := evaluation.DecodeCohortManifest(campaignBefore.CohortManifest); err != nil || dec.Digest != m.Digest {
		t.Fatal("cohort digest changed")
	}
	var filesA2, bytesA2 []string
	walk(t, stagedA.Workspace.Path, func(rel, content string) { filesA2 = append(filesA2, rel); bytesA2 = append(bytesA2, content) })
	if !reflect.DeepEqual(filesA, filesA2) || !reflect.DeepEqual(bytesA, bytesA2) {
		t.Fatal("first candidate's workspace changed")
	}
	if err := stagedA.Validate(); err != nil {
		t.Fatal(err)
	}
	stagings, _ := e.st.ListEvaluationStagings(e.ctx, reg.Campaign.ID)
	if len(stagings) != 2 {
		t.Fatalf("stagings = %d", len(stagings))
	}
}

func TestStageReportsUnavailableAndLeavesNothingBehind(t *testing.T) {
	e := newEnv(t)
	e.seed()
	reg, _ := registerCohort(t, e)
	r := newRunner(e)
	cand, _ := r.addCandidate(reg.Campaign.ID, "model-a", evaluation.UnknownNames())
	sm := reg.Samples[evaluation.SampleKey(e.proj, e.tasks["rejected"], 1)]
	req := StageRequest{CampaignID: reg.Campaign.ID, SampleID: sm.ID, CandidateID: cand.ID, RunID: "run-1"}

	assertClean := func() {
		t.Helper()
		if _, err := os.Stat(e.root); err == nil {
			walk(t, e.root, func(rel, _ string) {
				if strings.Contains(rel, ".stage-") || strings.HasPrefix(rel, filepath.Join(reg.Campaign.ID, cand.ID, "workspaces", sm.ID)) {
					t.Errorf("orphaned staging artifact: %s", rel)
				}
			})
		}
		if _, err := e.st.GetEvaluationStaging(e.ctx, reg.Campaign.ID, sm.ID, cand.ID); !errors.Is(err, store.ErrEvaluationStagingNotFound) {
			t.Errorf("a failed staging must not be recorded: %v", err)
		}
	}

	t.Run("original commit gone from the repository", func(t *testing.T) {
		empty := t.TempDir()
		cmd := exec.Command("git", "-C", empty, "init", "-q")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		st := Stager{Store: e.st, Sources: SourceMap{e.proj: evaluation.GitSource{Dir: empty}}, Root: e.root}
		_, err := st.Stage(e.ctx, req)
		var u *UnavailableError
		if !errors.As(err, &u) || u.Reason != evaluation.UnavailArtifactUnreachable {
			t.Fatalf("err = %v", err)
		}
		assertClean()
	})

	t.Run("board's round 1 commit changed after freezing", func(t *testing.T) {
		if _, err := e.st.Conn().ExecContext(e.ctx, `UPDATE task_link SET value = ? WHERE task_id = ? AND kind = 'commit' AND review_round = 1`, e.shas["fix"], e.tasks["rejected"]); err != nil {
			t.Fatal(err)
		}
		_, err := e.stager().Stage(e.ctx, req)
		var u *UnavailableError
		if !errors.As(err, &u) || u.Reason != evaluation.UnavailFrozenInputChanged {
			t.Fatalf("err = %v", err)
		}
		assertClean()
	})
}

func TestStageRefusesMismatchedRecords(t *testing.T) {
	e := newEnv(t)
	e.seed()
	reg, _ := registerCohort(t, e)
	r := newRunner(e)
	cand, _ := r.addCandidate(reg.Campaign.ID, "model-a", evaluation.UnknownNames())
	sm := reg.Samples[evaluation.SampleKey(e.proj, e.tasks["clean1"], 1)]

	for name, req := range map[string]StageRequest{
		"traversing sample id":    {CampaignID: reg.Campaign.ID, SampleID: "../" + sm.ID, CandidateID: cand.ID, RunID: "r"},
		"traversing candidate id": {CampaignID: reg.Campaign.ID, SampleID: sm.ID, CandidateID: "../x", RunID: "r"},
		"hidden campaign id":      {CampaignID: ".hidden", SampleID: sm.ID, CandidateID: cand.ID, RunID: "r"},
		"unknown sample":          {CampaignID: reg.Campaign.ID, SampleID: "nope", CandidateID: cand.ID, RunID: "r"},
		"bad tool access":         {CampaignID: reg.Campaign.ID, SampleID: sm.ID, CandidateID: cand.ID, RunID: "r", ToolAccess: evaluation.ToolAccessRequirements{Tools: []string{"a", "a"}}},
	} {
		if _, err := e.stager().Stage(e.ctx, req); err == nil {
			t.Errorf("%s: staging must be refused", name)
		}
	}
	if _, err := os.Stat(e.root); err == nil {
		walk(t, e.root, func(rel, _ string) {
			if rel != "." && rel != "./" {
				t.Errorf("refused staging left %s behind", rel)
			}
		})
	}

	// A legacy campaign whose cohort manifest is not a verified cohort cannot stage.
	legacy, err := e.st.CreateEvaluationCampaign(e.ctx, store.EvaluationCampaign{
		ID: store.GenerateID(), Name: "legacy", AllowedProjectIDs: []string{e.proj}, AllowedModelIDs: []string{"model-a"}, CohortManifest: `{}`, AttemptCap: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.stager().Stage(e.ctx, StageRequest{CampaignID: legacy.ID, SampleID: sm.ID, CandidateID: cand.ID, RunID: "r"}); !errors.Is(err, ErrCohortMismatch) {
		t.Fatalf("legacy campaign: %v", err)
	}
	// A sample from one campaign cannot be staged through another.
	if _, err := e.stager().Stage(e.ctx, StageRequest{CampaignID: reg.Campaign.ID, SampleID: sm.ID, CandidateID: "missing", RunID: "r"}); err == nil {
		t.Fatal("unknown candidate must be refused")
	}
}

func TestRestagingReplacesAnEarlierWorkspaceOfTheSameSlot(t *testing.T) {
	e := newEnv(t)
	e.seed()
	reg, _ := registerCohort(t, e)
	r := newRunner(e)
	cand, _ := r.addCandidate(reg.Campaign.ID, "model-a", evaluation.UnknownNames())
	sm := reg.Samples[evaluation.SampleKey(e.proj, e.tasks["clean1"], 1)]
	req := StageRequest{CampaignID: reg.Campaign.ID, SampleID: sm.ID, CandidateID: cand.ID, RunID: "run-1"}
	first, err := e.stager().Stage(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first.Workspace.Path, "artifact", "scribble.md"), []byte("left by a crashed run"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := e.stager().Stage(e.ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if second.Staging.ID != first.Staging.ID {
		t.Fatal("identical re-staging must reuse the recorded staging")
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("a re-staged workspace must be clean: %v", err)
	}
	if err := second.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(second.Workspace.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanup left the workspace: %v", err)
	}
}
