package evalrun

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/boldfield/odonian/internal/evalcohort"
	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

const fakeAdapterArg = "__fake_adapter__"

// The test binary doubles as the fake adapter, so every run goes through a real
// registered executable and the real host pipeline.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == fakeAdapterArg {
		os.Exit(evaluation.FakeMain(os.Args[2:], os.Stderr))
	}
	os.Exit(m.Run())
}

var adapterCaps = []string{evaluation.CapStructuredOutput, evaluation.CapSourceRetrieval}

func ident(adapter, model, pool string) evaluation.CandidateIdentity {
	return evaluation.CandidateIdentity{
		AdapterName: adapter, AdapterVersion: "1", ModelID: model, ModelRevision: evaluation.Unknown,
		RuntimeName: adapter + "-cli", RuntimeVersion: "2.0",
		ReasoningSettings:  evaluation.KnownSettings(map[string]string{"effort": "high"}),
		GenerationSettings: evaluation.KnownSettings(map[string]string{"temperature": "0"}),
		PromptVersion:      "p1", Tools: evaluation.KnownNames("web_fetch"), Observers: evaluation.KnownNames(),
		AccountPool: pool,
	}
}

func registerAdapter(t *testing.T, reg *evaluation.Registry, name, mode string, id evaluation.CandidateIdentity, mutate ...func(*evaluation.Runtime)) evaluation.CandidateConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := evaluation.NewCandidateConfig(id)
	if err != nil {
		t.Fatal(err)
	}
	idJSON, err := json.Marshal(id)
	if err != nil {
		t.Fatal(err)
	}
	rt := evaluation.Runtime{
		Name: name, Executable: exe, Timeout: 30 * time.Second, Capabilities: adapterCaps, Candidate: cfg,
		Args: []string{fakeAdapterArg, "--mode", mode, "--request", evaluation.PlaceholderRequestPath, "--identity", string(idJSON)},
	}
	for _, m := range mutate {
		m(&rt)
	}
	if err := reg.Register(rt); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func withUsage(usage string) func(*evaluation.Runtime) {
	return func(rt *evaluation.Runtime) { rt.Args = append(rt.Args, "--usage-json", usage) }
}

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) index(e string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, x := range l.events {
		if x == e {
			return i
		}
	}
	return -1
}

// harness is a real store, a registry of fake adapters and the fakes the
// runner is wired to. Nothing in it makes a network call or spends a token.
type harness struct {
	t        *testing.T
	st       store.Store
	dbPath   string
	campaign store.EvaluationCampaign
	samples  []store.EvaluationSample
	reg      *evaluation.Registry
	creds    evaluation.MapCredentials
	cands    map[string]store.EvaluationCandidate
	stager   *fakeStager
	log      *eventLog
}

func newHarness(t *testing.T, nSamples int, opts ...store.StoreOption) *harness {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "eval.db")
	st, err := store.Open(dbPath, []string{"sonnet"}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	campaign, err := st.CreateEvaluationCampaign(ctx, store.EvaluationCampaign{
		ID: store.GenerateID(), Name: "c", AllowedProjectIDs: []string{"proj1"},
		AllowedModelIDs: []string{"model-a", "model-b", "model-c"}, CohortManifest: `{"samples": []}`, AttemptCap: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t: t, st: st, dbPath: dbPath, campaign: campaign, reg: evaluation.NewRegistry(),
		creds: evaluation.MapCredentials{}, cands: map[string]store.EvaluationCandidate{}, log: &eventLog{},
		stager: &fakeStager{root: filepath.Join(dir, "work")},
	}
	for i := 0; i < nSamples; i++ {
		s, err := st.CreateEvaluationSample(ctx, store.EvaluationSample{
			ID: store.GenerateID(), CampaignID: campaign.ID, ProjectID: "proj1",
			OriginalTaskID: store.GenerateID(), OriginalReviewRound: 1, SubmittedSHA: "abc123", PromptVersion: "p1", ModelVersion: "m1", RuntimeVersion: "r1",
		})
		if err != nil {
			t.Fatal(err)
		}
		h.samples = append(h.samples, s)
	}
	return h
}

// pool configures a concurrency-only pool unless one already exists.
func (h *harness) pool(cfg store.EvaluationPoolConfig) {
	h.t.Helper()
	if _, err := h.st.GetEvaluationPool(context.Background(), cfg.ID); err == nil {
		return
	}
	if _, err := h.st.ConfigureEvaluationPool(context.Background(), cfg); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) openPool(id string) {
	h.pool(store.EvaluationPoolConfig{ID: id, ConcurrencyOnly: true, ConcurrentLimit: 1000})
}

// addCandidate registers an adapter and stores the matching candidate version.
func (h *harness) addCandidate(name, mode string, id evaluation.CandidateIdentity, cap int, mutate ...func(*evaluation.Runtime)) store.EvaluationCandidate {
	h.t.Helper()
	cfg := registerAdapter(h.t, h.reg, name, mode, id, mutate...)
	cand, err := h.st.CreateEvaluationCandidate(context.Background(), store.EvaluationCandidate{
		ID: store.GenerateID(), CampaignID: h.campaign.ID, Config: cfg, PerCandidateCap: cap,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.cands[name] = cand
	return cand
}

// storeOnly stores a candidate without registering any adapter for it.
func (h *harness) storeOnly(id evaluation.CandidateIdentity, cap int) store.EvaluationCandidate {
	h.t.Helper()
	cfg, err := evaluation.NewCandidateConfig(id)
	if err != nil {
		h.t.Fatal(err)
	}
	cand, err := h.st.CreateEvaluationCandidate(context.Background(), store.EvaluationCandidate{
		ID: store.GenerateID(), CampaignID: h.campaign.ID, Config: cfg, PerCandidateCap: cap,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return cand
}

func (h *harness) pipeline() *evaluation.Pipeline {
	return &evaluation.Pipeline{Registry: h.reg, Credentials: h.creds}
}

func (h *harness) job(sample int, cand store.EvaluationCandidate) Job {
	return Job{CampaignID: h.campaign.ID, SampleID: h.samples[sample].ID, CandidateID: cand.ID}
}

func (h *harness) attempt(id string) store.EvaluationAttempt {
	h.t.Helper()
	a, err := h.st.GetEvaluationAttempt(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func (h *harness) detail(id string) *store.EvaluationAttemptDetail {
	h.t.Helper()
	d, err := h.st.GetEvaluationAttemptDetail(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *harness) attemptsUsed() int {
	h.t.Helper()
	status, err := h.st.GetEvaluationCampaignStatus(context.Background(), h.campaign.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	return status.TotalAttemptsUsed
}

// runner builds a Runner over the harness with instant sleeps. mutate may
// replace any part of the wiring.
func (h *harness) runner(mutate ...func(*Config)) (*Runner, *recExec, *sleeper) {
	h.t.Helper()
	rx := &recExec{inner: h.pipeline(), log: h.log}
	sl := &sleeper{}
	cfg := Config{Backend: h.st, Registry: h.reg, Stager: h.stager, Executor: rx, Sleep: sl.Sleep}
	for _, m := range mutate {
		m(&cfg)
	}
	r, err := New(cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return r, rx, sl
}

// nonEvaluationRows counts the rows of every table outside the evaluation
// tables: the production board's state.
func nonEvaluationRows(t *testing.T, dbPath string) map[string]int {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'evaluation\_%' ESCAPE '\' AND name NOT LIKE 'sqlite\_%' ESCAPE '\'`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	sort.Strings(names)
	counts := map[string]int{}
	for _, n := range names {
		var c int
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + n + `"`).Scan(&c); err != nil {
			t.Fatalf("count %s: %v", n, err)
		}
		counts[n] = c
	}
	return counts
}

type sleeper struct {
	mu      sync.Mutex
	calls   []time.Duration
	advance func(d time.Duration)
	onSleep func(n int, d time.Duration)
}

func (s *sleeper) Sleep(ctx context.Context, d time.Duration) error {
	s.mu.Lock()
	s.calls = append(s.calls, d)
	n := len(s.calls)
	adv, hook := s.advance, s.onSleep
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if hook != nil {
		hook(n, d)
	}
	if adv != nil {
		adv(d)
	}
	return nil
}

func (s *sleeper) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.calls...)
}

// recExec records every request it is asked to run and delegates.
type recExec struct {
	inner Executor
	log   *eventLog
	hook  func(ctx context.Context, n int)

	mu    sync.Mutex
	reqs  []evaluation.CandidateRequest
	names []string
}

func (e *recExec) Execute(ctx context.Context, runtime string, req evaluation.CandidateRequest) (evaluation.Result, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	e.names = append(e.names, runtime)
	n := len(e.reqs)
	e.mu.Unlock()
	if e.hook != nil {
		e.hook(ctx, n)
	}
	res, err := e.inner.Execute(ctx, runtime, req)
	if e.log != nil {
		e.log.add("exec-return")
	}
	return res, err
}

func (e *recExec) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.reqs)
}

func (e *recExec) request(i int) evaluation.CandidateRequest {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reqs[i]
}

type funcExec func(ctx context.Context, runtime string, req evaluation.CandidateRequest) (evaluation.Result, error)

func (f funcExec) Execute(ctx context.Context, runtime string, req evaluation.CandidateRequest) (evaluation.Result, error) {
	return f(ctx, runtime, req)
}

// sequenceExec runs call i through pipes[i] (the last repeats), so one
// candidate can behave differently from one attempt to the next.
type sequenceExec struct {
	pipes []*evaluation.Pipeline
	mu    sync.Mutex
	n     int
}

func (s *sequenceExec) Execute(ctx context.Context, runtime string, req evaluation.CandidateRequest) (evaluation.Result, error) {
	s.mu.Lock()
	i := min(s.n, len(s.pipes)-1)
	s.n++
	s.mu.Unlock()
	return s.pipes[i].Execute(ctx, runtime, req)
}

// faultBackend wraps the real store and injects faults into the lease calls.
type faultBackend struct {
	store.Store
	log      *eventLog
	renew    func(n int) error
	finalize func(n int, res store.EvaluationAttemptResult) error

	mu          sync.Mutex
	renewN      int
	finalizeN   int
	offered     []store.EvaluationAttemptResult
	finalizeCtx []error
	claimHook   func(res *store.EvaluationJobClaimResult)
}

func (f *faultBackend) ClaimEvaluationJob(ctx context.Context, req store.EvaluationJobClaim) (store.EvaluationJobClaimResult, error) {
	res, err := f.Store.ClaimEvaluationJob(ctx, req)
	if err == nil {
		f.log.add("claim")
		if f.claimHook != nil {
			f.claimHook(&res)
		}
	}
	return res, err
}

func (f *faultBackend) RenewEvaluationAttempt(ctx context.Context, id string, expiresAt time.Time) error {
	f.mu.Lock()
	f.renewN++
	n := f.renewN
	f.mu.Unlock()
	if f.renew != nil {
		if err := f.renew(n); err != nil {
			f.log.add("renew-fail")
			return err
		}
	}
	return f.Store.RenewEvaluationAttempt(ctx, id, expiresAt)
}

func (f *faultBackend) FinalizeEvaluationAttempt(ctx context.Context, res store.EvaluationAttemptResult) error {
	f.mu.Lock()
	f.finalizeN++
	n := f.finalizeN
	f.offered = append(f.offered, res)
	f.finalizeCtx = append(f.finalizeCtx, ctx.Err())
	f.mu.Unlock()
	if f.finalize != nil {
		if err := f.finalize(n, res); err != nil {
			return err
		}
	}
	f.log.add("finalize")
	return f.Store.FinalizeEvaluationAttempt(ctx, res)
}

func (f *faultBackend) finalizeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finalizeN
}

// fakeStager builds real, digest-verified workspaces without a cohort manifest.
type fakeStager struct {
	root       string
	err        error
	afterStage func(*evalcohort.Staged)

	mu     sync.Mutex
	reqs   []evalcohort.StageRequest
	staged []*evalcohort.Staged
}

func (f *fakeStager) Stage(ctx context.Context, req evalcohort.StageRequest) (*evalcohort.Staged, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	parent := filepath.Join(f.root, req.CandidateID)
	ws, err := evaluation.StageWorkspace(parent, req.RunID, evaluation.WorkspaceSpec{
		Artifact:   []evaluation.WorkspaceFile{{Path: "artifact.md", Content: []byte("the artifact")}},
		Acceptance: "the acceptance criteria",
	})
	if err != nil {
		return nil, err
	}
	st := &evalcohort.Staged{
		Staging:   store.EvaluationStaging{SnapshotDigest: ws.SnapshotDigest, SourceDigest: ws.SourceDigest},
		Workspace: ws,
		Request: evaluation.CandidateRequest{
			Version: evaluation.ProtocolVersion, RunID: req.RunID, SnapshotPath: ws.Path,
			BlindedPrompt: "review the staged artifact", ToolAccess: req.ToolAccess,
			ResultPath: filepath.Join(parent, req.RunID+".result.json"),
		},
	}
	f.mu.Lock()
	f.staged = append(f.staged, st)
	f.mu.Unlock()
	if f.afterStage != nil {
		f.afterStage(st)
	}
	return st, nil
}

func (f *fakeStager) stagedAt(i int) *evalcohort.Staged {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.staged[i]
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
