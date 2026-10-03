package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/policy"
	"github.com/boldfield/odonian/internal/store"
	"github.com/boldfield/odonian/internal/tuiclient"
)

type apiClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *apiClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *apiClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type researchAPI struct {
	t      *testing.T
	server *Server
	clock  *apiClock
	proj   string
	doc    string
}

const researchAuth = "Bearer test-token"

func newResearchAPI(t *testing.T, mode policy.Mode, burst, limit int) *researchAPI {
	t.Helper()
	clock := &apiClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s, err := store.Open(filepath.Join(t.TempDir(), "api.db"), defaultTestAllowedModels(), store.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	cfg := policy.Config{Mode: mode, AllowedModels: defaultTestAllowedModels()}
	if mode != policy.ModeDisabled {
		cfg.Pools = []policy.Pool{{
			Name: "pool", AccountID: "acct", Models: defaultTestAllowedModels(),
			StartRate: 0.0001, BurstCapacity: burst, ConcurrentDispatchLimit: limit,
		}}
	}
	if err := s.SetResearchPolicy(t.Context(), clock.Now(), cfg); err != nil {
		t.Fatalf("SetResearchPolicy: %v", err)
	}
	server := New(s, "test-token", 5*time.Minute, 5, nil, nil, 999999, false, 500, nil)
	proj, err := s.CreateProject(t.Context(), "p", "https://github.com/test/repo")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(t.Context(), proj.ID, "feature_spec", "doc", "doc.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &researchAPI{t: t, server: server, clock: clock, proj: proj.ID, doc: doc.ID}
}

func (r *researchAPI) task(track string) string {
	r.t.Helper()
	tasks, err := r.server.store.CreateTasks(r.t.Context(), r.proj, []store.TaskInput{{
		Title: fmt.Sprintf("t-%d", time.Now().UnixNano()), Spec: "spec", DocumentID: r.doc, Model: "opus", Track: track,
	}})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.server.store.PromoteTask(r.t.Context(), tasks[0].ID); err != nil {
		r.t.Fatal(err)
	}
	return tasks[0].ID
}

func (r *researchAPI) post(path string, payload map[string]any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Authorization", researchAuth)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.server.mux.ServeHTTP(w, req)
	return w
}

func (r *researchAPI) claim(id string, extra map[string]any) *httptest.ResponseRecorder {
	payload := map[string]any{"agent_id": "agent", "model": "opus"}
	for k, v := range extra {
		payload[k] = v
	}
	return r.post("/tasks/"+id+"/claim", payload)
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
	return out
}

func admission(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("claim: status %d: %s", w.Code, w.Body.String())
	}
	a, ok := decodeBody(t, w)["research_admission"].(map[string]any)
	if !ok {
		t.Fatalf("no research_admission in %s", w.Body.String())
	}
	return a
}

func TestClaimResearchAPIDeniedIs429AndLeavesTaskReady(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 1, 5)
	a, b := r.task("research"), r.task("research")
	if att := admission(t, r.claim(a, nil)); att["attempt_id"] == "" || att["permit_id"] == "" {
		t.Fatalf("admission = %v", att)
	}

	w := r.claim(b, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	e := decodeBody(t, w)["error"].(map[string]any)
	if e["code"] != "ADMISSION_DENIED" || e["outcome"] != "defer" || e["reason"] != "rate" || e["not_before"] == nil {
		t.Fatalf("error body = %v", e)
	}
	if got := apiGetTask(t, r.server, researchAuth, b); got.State != "ready" || got.Assignee != nil {
		t.Fatalf("denied task = %s %v", got.State, got.Assignee)
	}
}

func TestClaimResearchAPIConcurrencyDenialSetsRetryAfter(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 10, 1)
	a, b := r.task("research"), r.task("research")
	admission(t, r.claim(a, nil))
	w := r.claim(b, nil)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d retry-after %q: %s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	e := decodeBody(t, w)["error"].(map[string]any)
	if e["outcome"] != "retry" || e["reason"] != "concurrency" || e["retry_after_seconds"] == nil {
		t.Fatalf("error body = %v", e)
	}
}

func TestClaimResearchAPIRequestIDReplay(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 3, 5)
	id := r.task("research")
	first := admission(t, r.claim(id, map[string]any{"request_id": "req-1"}))
	again := admission(t, r.claim(id, map[string]any{"request_id": "req-1"}))
	if again["attempt_id"] != first["attempt_id"] || again["replayed"] != true {
		t.Fatalf("replay = %v, first = %v", again, first)
	}
	w := r.claim(id, map[string]any{"request_id": "req-1", "agent_id": "someone-else"})
	if w.Code != http.StatusConflict || apiErrorCode(t, w.Body.Bytes()) != "REQUEST_ID_CONFLICT" {
		t.Fatalf("reuse by another agent: %d %s", w.Code, w.Body.String())
	}
}

func TestClaimResearchAPIRejectsBadAssertions(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 3, 5)
	id := r.task("research")
	for name, extra := range map[string]map[string]any{
		"unknown work class": {"work_class": "bogus"},
		"unpaced work class": {"work_class": "build"},
		"wrong work class":   {"work_class": "research_review"},
		"wrong account":      {"account_id": "someone-else"},
	} {
		if w := r.claim(id, extra); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d: %s", name, w.Code, w.Body.String())
		}
	}
	if got := apiGetTask(t, r.server, researchAuth, id); got.State != "ready" {
		t.Fatalf("state = %s", got.State)
	}
	// Correct assertions are accepted.
	admission(t, r.claim(id, map[string]any{"work_class": "research_write", "account_id": "acct"}))
}

func TestClaimResearchAPIConcurrentRaceAdmitsAtMostBurst(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 2, 20)
	const n = 8
	ids := make([]string, n)
	for i := range ids {
		ids[i] = r.task("research")
	}
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = r.claim(ids[i], map[string]any{"agent_id": fmt.Sprintf("agent-%d", i)}).Code
		}(i)
	}
	close(start)
	wg.Wait()
	ok, denied := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			denied++
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	if ok != 2 || denied != n-2 {
		t.Fatalf("ok=%d denied=%d, want 2 and %d", ok, denied, n-2)
	}
}

func TestResearchAPIStaleAttemptFenced(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 5, 5)
	id := r.task("research")
	first := admission(t, r.claim(id, map[string]any{"request_id": "r1"}))
	r.clock.Advance(6 * time.Minute)
	second := admission(t, r.claim(id, map[string]any{"request_id": "r2"}))
	if second["attempt_id"] == first["attempt_id"] {
		t.Fatalf("reclaim reused the attempt")
	}

	w := r.post("/tasks/"+id+"/heartbeat", map[string]any{"agent_id": "agent", "attempt_id": first["attempt_id"]})
	if w.Code != http.StatusConflict || apiErrorCode(t, w.Body.Bytes()) != "ATTEMPT_FENCED" {
		t.Fatalf("stale heartbeat: %d %s", w.Code, w.Body.String())
	}
	code, body := apiSubmit(t, r.server, researchAuth, id, map[string]any{
		"agent_id": "agent", "attempt_id": first["attempt_id"], "result": "stale",
		"links": []map[string]string{{"kind": "pr", "value": "https://github.com/test/repo/pull/1"}},
	})
	if code != http.StatusConflict || apiErrorCode(t, body) != "ATTEMPT_FENCED" {
		t.Fatalf("stale submit: %d %s", code, body)
	}
	if got := apiGetTask(t, r.server, researchAuth, id); got.State != "in_progress" {
		t.Fatalf("replacement state = %s", got.State)
	}

	if w := r.post("/tasks/"+id+"/heartbeat", map[string]any{"agent_id": "agent", "attempt_id": second["attempt_id"]}); w.Code != http.StatusOK {
		t.Fatalf("current heartbeat: %d %s", w.Code, w.Body.String())
	}
}

func TestClaimResearchAPIDisabledReturnsBareTask(t *testing.T) {
	r := newResearchAPI(t, policy.ModeDisabled, 0, 0)
	for i := 0; i < 3; i++ {
		w := r.claim(r.task("research"), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("claim %d: %d %s", i, w.Code, w.Body.String())
		}
		if _, ok := decodeBody(t, w)["research_admission"]; ok {
			t.Fatalf("disabled mode returned an admission: %s", w.Body.String())
		}
	}
}

func TestClaimResearchAPIObserveGrantsAndReportsHypotheticalDenial(t *testing.T) {
	r := newResearchAPI(t, policy.ModeObserve, 1, 5)
	admission(t, r.claim(r.task("research"), nil))
	second := admission(t, r.claim(r.task("research"), nil))
	obs, ok := second["observed_denial"].(map[string]any)
	if !ok || obs["reason"] != "rate" {
		t.Fatalf("admission = %v", second)
	}
}

func TestClaimNonResearchAPIUnaffectedByPolicy(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 1, 1)
	admission(t, r.claim(r.task("research"), nil))
	for i := 0; i < 3; i++ {
		w := r.claim(r.task("build"), map[string]any{"agent_id": fmt.Sprintf("a%d", i)})
		if w.Code != http.StatusOK {
			t.Fatalf("build claim %d: %d %s", i, w.Code, w.Body.String())
		}
		if _, ok := decodeBody(t, w)["research_admission"]; ok {
			t.Fatalf("build claim returned an admission")
		}
	}
}

func TestResearchClientPathFencesStaleSameAgentAfterReplacement(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 5, 5)
	ts := httptest.NewServer(r.server.Handler())
	t.Cleanup(ts.Close)
	ctx := t.Context()
	id := r.task("research")

	// Two processes of the same agent identity, as after a lease expiry and reclaim.
	oldWorker := tuiclient.NewHTTPClient(ts.URL, "test-token")
	if err := oldWorker.ClaimTask(ctx, id, "agent", "opus"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if oldWorker.AttemptID(id) == "" {
		t.Fatalf("client did not keep the research attempt from the claim")
	}
	r.clock.Advance(6 * time.Minute)
	newWorker := tuiclient.NewHTTPClient(ts.URL, "test-token")
	if err := newWorker.ClaimTask(ctx, id, "agent", "opus"); err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if newWorker.AttemptID(id) == oldWorker.AttemptID(id) {
		t.Fatalf("reclaim reused the attempt")
	}

	var apiErr *tuiclient.APIError
	err := oldWorker.HeartbeatTask(ctx, id, "agent")
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != "ATTEMPT_FENCED" {
		t.Fatalf("stale heartbeat = %v", err)
	}
	links := []tuiclient.LinkInput{{Kind: "pr", Value: "https://github.com/test/repo/pull/1"}}
	err = oldWorker.SubmitTask(ctx, id, "agent", "stale", nil, links)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != "ATTEMPT_FENCED" {
		t.Fatalf("stale submit = %v", err)
	}
	if got := apiGetTask(t, r.server, researchAuth, id); got.State != "in_progress" || got.Result != nil {
		t.Fatalf("stale client changed the replacement: state=%s result=%v", got.State, got.Result)
	}

	if err := newWorker.HeartbeatTask(ctx, id, "agent"); err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if err := newWorker.SubmitTask(ctx, id, "agent", "real", nil, links); err != nil {
		t.Fatalf("current submit: %v", err)
	}
}

func TestResearchClientAttemptSetExplicitly(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 5, 5)
	ts := httptest.NewServer(r.server.Handler())
	t.Cleanup(ts.Close)
	ctx := t.Context()
	id := r.task("research")

	claimer := tuiclient.NewHTTPClient(ts.URL, "test-token")
	if err := claimer.ClaimTask(ctx, id, "agent", "opus"); err != nil {
		t.Fatal(err)
	}
	// A later process (the CLI) is handed the attempt ID.
	later := tuiclient.NewHTTPClient(ts.URL, "test-token")
	later.SetAttemptID(id, claimer.AttemptID(id))
	if err := later.HeartbeatTask(ctx, id, "agent"); err != nil {
		t.Fatalf("heartbeat with set attempt: %v", err)
	}
	later.SetAttemptID(id, "not-the-attempt")
	var apiErr *tuiclient.APIError
	if err := later.HeartbeatTask(ctx, id, "agent"); !errors.As(err, &apiErr) || apiErr.Code != "ATTEMPT_FENCED" {
		t.Fatalf("wrong attempt heartbeat = %v", err)
	}

	// A non-research claim carries no attempt.
	plain := r.task("build")
	if err := claimer.ClaimTask(ctx, plain, "agent", "opus"); err != nil {
		t.Fatal(err)
	}
	if claimer.AttemptID(plain) != "" {
		t.Fatalf("non-research claim recorded an attempt")
	}
}

func (r *researchAPI) deferredCount() (int, int) {
	r.t.Helper()
	req := httptest.NewRequest("GET", "/research/status", nil)
	req.Header.Set("Authorization", researchAuth)
	w := httptest.NewRecorder()
	r.server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		r.t.Fatalf("status: %d: %s", w.Code, w.Body.String())
	}
	pools, _ := decodeBody(r.t, w)["pools"].([]any)
	if len(pools) != 1 {
		r.t.Fatalf("pools = %v", pools)
	}
	pool := pools[0].(map[string]any)
	return int(pool["deferred"].(float64)), int(pool["active"].(float64))
}

func TestResearchStatusDeferredCountsRateAndConcurrencyDenials(t *testing.T) {
	t.Run("concurrency retry", func(t *testing.T) {
		r := newResearchAPI(t, policy.ModeEnforce, 5, 1)
		admission(t, r.claim(r.task("research"), nil))
		if w := r.claim(r.task("research"), nil); w.Code != http.StatusTooManyRequests {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
		if d, a := r.deferredCount(); d != 1 || a != 1 {
			t.Fatalf("deferred=%d active=%d, want 1/1", d, a)
		}
	})
	t.Run("rate defer", func(t *testing.T) {
		r := newResearchAPI(t, policy.ModeEnforce, 1, 5)
		admission(t, r.claim(r.task("research"), nil))
		if w := r.claim(r.task("research"), nil); w.Code != http.StatusTooManyRequests {
			t.Fatalf("claim: %d %s", w.Code, w.Body.String())
		}
		if d, _ := r.deferredCount(); d != 1 {
			t.Fatalf("deferred=%d, want 1", d)
		}
	})
}

func TestResearchStatusDeferredIgnoresObserveModeHypotheticalDenials(t *testing.T) {
	r := newResearchAPI(t, policy.ModeObserve, 1, 1)
	admission(t, r.claim(r.task("research"), nil))
	b := r.task("research")
	admission(t, r.claim(b, nil))
	if got := apiGetTask(t, r.server, researchAuth, b); got.State != "in_progress" {
		t.Fatalf("observe mode must grant the claim, state = %s", got.State)
	}
	d, err := r.server.store.GetResearchAdmissionDiagnostic(t.Context(), b)
	if err != nil || !d.Hypothetical || (d.Outcome != policy.OutcomeRetry && d.Outcome != policy.OutcomeDefer) {
		t.Fatalf("expected a hypothetical denial diagnostic, got %+v err=%v", d, err)
	}
	if got, _ := r.deferredCount(); got != 0 {
		t.Fatalf("deferred=%d, want 0 for hypothetical denials", got)
	}
}

func TestResearchStatusDeferredDropsTasksThatLeaveReady(t *testing.T) {
	r := newResearchAPI(t, policy.ModeEnforce, 5, 1)
	admission(t, r.claim(r.task("research"), nil))
	b := r.task("research")
	if w := r.claim(b, nil); w.Code != http.StatusTooManyRequests {
		t.Fatalf("claim: %d %s", w.Code, w.Body.String())
	}
	if got, _ := r.deferredCount(); got != 1 {
		t.Fatalf("deferred=%d, want 1 while ready", got)
	}
	if _, err := r.server.store.TransitionTask(t.Context(), b, "blocked", nil); err != nil {
		t.Fatalf("block: %v", err)
	}
	if got, _ := r.deferredCount(); got != 0 {
		t.Fatalf("deferred=%d, want 0 after task left ready", got)
	}
}
