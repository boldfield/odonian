package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

const evalAuth = "Bearer test-token"

type evalAPI struct {
	t      *testing.T
	server *Server
	store  store.Store
	clock  *apiClock
}

func newEvalAPI(t *testing.T) *evalAPI {
	t.Helper()
	clock := &apiClock{t: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	s, err := store.Open(filepath.Join(t.TempDir(), "eval-api.db"), defaultTestAllowedModels(), store.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return &evalAPI{t: t, store: s, clock: clock, server: New(s, "test-token", 5*time.Minute, 5, nil, nil, 999999, false, 500, nil)}
}

// call performs an authenticated request and returns the status and decoded JSON object.
func (e *evalAPI) call(method, path string, body interface{}) (int, map[string]interface{}) {
	e.t.Helper()
	var rd *bytes.Reader
	switch b := body.(type) {
	case nil:
		rd = bytes.NewReader(nil)
	case string:
		rd = bytes.NewReader([]byte(b))
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rd)
	req.Header.Set("Authorization", evalAuth)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	e.server.mux.ServeHTTP(w, req)
	out := map[string]interface{}{}
	if w.Body.Len() > 0 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			e.t.Fatalf("%s %s: non-JSON body %q", method, path, w.Body.String())
		}
	}
	return w.Code, out
}

func (e *evalAPI) expect(method, path string, body interface{}, wantStatus int, wantCode string) map[string]interface{} {
	e.t.Helper()
	status, out := e.call(method, path, body)
	if status != wantStatus {
		e.t.Fatalf("%s %s: status %d, want %d; body %v", method, path, status, wantStatus, out)
	}
	if wantCode != "" {
		if got := errCode(out); got != wantCode {
			e.t.Fatalf("%s %s: error code %q, want %q; body %v", method, path, got, wantCode, out)
		}
	}
	return out
}

func errCode(out map[string]interface{}) string {
	if m, ok := out["error"].(map[string]interface{}); ok {
		s, _ := m["code"].(string)
		return s
	}
	return ""
}

func evalIdentity(model, pool string) evaluation.CandidateIdentity {
	return evaluation.CandidateIdentity{
		AdapterName: "fake", AdapterVersion: "v1", ModelID: model, ModelRevision: evaluation.Unknown,
		RuntimeName: "test", RuntimeVersion: "v1", ReasoningSettings: evaluation.UnknownSettings(),
		GenerationSettings: evaluation.UnknownSettings(), PromptVersion: "v1", Tools: evaluation.UnknownNames(),
		Observers: evaluation.UnknownNames(), AccountPool: pool,
	}
}

func (e *evalAPI) pool(id string, limit int) {
	e.t.Helper()
	e.expect("PUT", "/evaluation/pools/"+id, map[string]interface{}{"concurrency_only": true, "concurrent_limit": limit}, 200, "")
}

func (e *evalAPI) campaign(id string, cap int, projects ...string) {
	e.t.Helper()
	if len(projects) == 0 {
		projects = []string{"proj1"}
	}
	e.expect("POST", "/evaluation/campaigns", map[string]interface{}{
		"id": id, "name": "camp " + id, "allowed_project_ids": projects,
		"allowed_model_ids": []string{"model-a", "model-b"}, "cohort_manifest": `{"samples":[]}`, "attempt_cap": cap,
	}, 201, "")
}

func (e *evalAPI) candidate(campaign, id, model, pool string, cap int) {
	e.t.Helper()
	e.expect("POST", "/evaluation/campaigns/"+campaign+"/candidates", map[string]interface{}{
		"id": id, "per_candidate_cap": cap, "identity": evalIdentity(model, pool),
	}, 201, "")
}

func (e *evalAPI) sample(campaign, id, project, task string) {
	e.t.Helper()
	_, err := e.store.CreateEvaluationSample(e.t.Context(), store.EvaluationSample{
		ID: id, CampaignID: campaign, ProjectID: project, OriginalTaskID: task, OriginalReviewRound: 1,
		SubmittedSHA: "abc123", PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
	})
	if err != nil {
		e.t.Fatalf("seed sample: %v", err)
	}
}

type claimed struct{ jobID, attemptID string }

func (e *evalAPI) claim(sample, candidate, requestID string) claimed {
	e.t.Helper()
	out := e.expect("POST", "/evaluation/jobs/claim", map[string]interface{}{
		"sample_id": sample, "candidate_id": candidate, "request_id": requestID, "lease_ttl_ms": 60000,
	}, 200, "")
	job := out["job"].(map[string]interface{})
	att := out["attempt"].(map[string]interface{})
	return claimed{jobID: job["id"].(string), attemptID: att["id"].(string)}
}

func (c claimed) path(verb string) string {
	return "/evaluation/jobs/" + c.jobID + "/attempts/" + c.attemptID + "/" + verb
}

func completedResult(attemptID string, findings ...map[string]interface{}) map[string]interface{} {
	r := map[string]interface{}{"fence_attempt_id": attemptID, "exit_class": "completed", "status": "completed"}
	if findings != nil {
		r["findings"] = findings
	}
	return r
}

// basicSetup creates one pool, campaign, candidate and sample.
func (e *evalAPI) basicSetup(campaignCap, candidateCap, poolLimit int) {
	e.pool("pool1", poolLimit)
	e.campaign("camp1", campaignCap)
	e.candidate("camp1", "cand-a", "model-a", "pool1", candidateCap)
	e.sample("camp1", "s1", "proj1", "task-x")
}

func TestEvaluationAPIRequiresAuth(t *testing.T) {
	e := newEvalAPI(t)
	for _, route := range []struct{ method, path string }{
		{"POST", "/evaluation/campaigns"}, {"GET", "/evaluation/campaigns/c1"},
		{"GET", "/evaluation/campaigns/c1/status"}, {"POST", "/evaluation/campaigns/c1/pause"},
		{"PUT", "/evaluation/pools/p1"}, {"POST", "/evaluation/jobs/claim"},
	} {
		req := httptest.NewRequest(route.method, route.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		e.server.mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without auth: status %d, want 401", route.method, route.path, w.Code)
		}
	}
}

func TestEvaluationAPICampaignValidation(t *testing.T) {
	e := newEvalAPI(t)
	valid := func() map[string]interface{} {
		return map[string]interface{}{
			"id": "c1", "name": "n", "allowed_project_ids": []string{"p"}, "allowed_model_ids": []string{"m"},
			"cohort_manifest": "{}", "attempt_cap": 3,
		}
	}
	mutate := func(f func(m map[string]interface{})) map[string]interface{} {
		m := valid()
		f(m)
		return m
	}
	bad := map[string]interface{}{
		"zero cap":        mutate(func(m map[string]interface{}) { m["attempt_cap"] = 0 }),
		"negative cap":    mutate(func(m map[string]interface{}) { m["attempt_cap"] = -1 }),
		"missing cap":     mutate(func(m map[string]interface{}) { delete(m, "attempt_cap") }),
		"huge cap":        mutate(func(m map[string]interface{}) { m["attempt_cap"] = 100001 }),
		"no projects":     mutate(func(m map[string]interface{}) { m["allowed_project_ids"] = []string{} }),
		"no models":       mutate(func(m map[string]interface{}) { m["allowed_model_ids"] = []string{} }),
		"empty manifest":  mutate(func(m map[string]interface{}) { m["cohort_manifest"] = "" }),
		"missing id":      mutate(func(m map[string]interface{}) { delete(m, "id") }),
		"unknown field":   mutate(func(m map[string]interface{}) { m["unlimited"] = true }),
		"duplicate model": mutate(func(m map[string]interface{}) { m["allowed_model_ids"] = []string{"m", "m"} }),
		"string cap":      mutate(func(m map[string]interface{}) { m["attempt_cap"] = "5" }),
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) { e.expect("POST", "/evaluation/campaigns", body, 400, "INVALID_INPUT") })
	}
	e.expect("POST", "/evaluation/campaigns", "{not json", 400, "INVALID_INPUT")
	e.expect("POST", "/evaluation/campaigns", `{"id":"c1"} {"id":"c2"}`, 400, "INVALID_INPUT")
	e.expect("POST", "/evaluation/campaigns", "", 400, "INVALID_INPUT")

	out := e.expect("POST", "/evaluation/campaigns", valid(), 201, "")
	camp := out["campaign"].(map[string]interface{})
	if camp["id"] != "c1" || camp["attempt_cap"].(float64) != 3 || camp["paused_at"] != nil {
		t.Fatalf("unexpected campaign view: %v", camp)
	}
	e.expect("POST", "/evaluation/campaigns", valid(), 409, "ALREADY_EXISTS")

	got := e.expect("GET", "/evaluation/campaigns/c1", nil, 200, "")
	if !reflect.DeepEqual(got["campaign"], camp) {
		t.Fatalf("GET campaign = %v, want %v", got["campaign"], camp)
	}
	e.expect("GET", "/evaluation/campaigns/missing", nil, 404, "CAMPAIGN_NOT_FOUND")
	e.expect("GET", "/evaluation/campaigns/missing/status", nil, 404, "CAMPAIGN_NOT_FOUND")
}

func TestEvaluationAPINothingCreatedImplicitly(t *testing.T) {
	e := newEvalAPI(t)
	e.expect("GET", "/evaluation/campaigns/c1", nil, 404, "CAMPAIGN_NOT_FOUND")
	e.expect("GET", "/evaluation/pools/p1", nil, 409, "POOL_NOT_CONFIGURED")
	// Claiming against nothing creates nothing.
	e.expect("POST", "/evaluation/jobs/claim", map[string]interface{}{
		"sample_id": "s", "candidate_id": "c", "request_id": "r", "lease_ttl_ms": 1000,
	}, 404, "")
	e.expect("GET", "/evaluation/campaigns/c1", nil, 404, "CAMPAIGN_NOT_FOUND")
}

func TestEvaluationAPIPoolConfigure(t *testing.T) {
	e := newEvalAPI(t)
	for name, body := range map[string]interface{}{
		"no limit":                map[string]interface{}{"concurrency_only": true},
		"rate pool without rate":  map[string]interface{}{"burst_capacity": 2, "concurrent_limit": 1},
		"rate pool without burst": map[string]interface{}{"start_rate": 1.0, "concurrent_limit": 1},
		"compute pool with rate":  map[string]interface{}{"concurrency_only": true, "start_rate": 1.0, "concurrent_limit": 1},
		"unknown field":           map[string]interface{}{"concurrency_only": true, "concurrent_limit": 1, "unlimited": true},
	} {
		t.Run(name, func(t *testing.T) { e.expect("PUT", "/evaluation/pools/p1", body, 400, "INVALID_INPUT") })
	}
	e.expect("GET", "/evaluation/pools/p1", nil, 409, "POOL_NOT_CONFIGURED")

	out := e.expect("PUT", "/evaluation/pools/compute", map[string]interface{}{"concurrency_only": true, "concurrent_limit": 2}, 200, "")
	pool := out["pool"].(map[string]interface{})
	if pool["mode"] != "concurrency_only" || pool["concurrent_limit"].(float64) != 2 || pool["active"].(float64) != 0 {
		t.Fatalf("compute pool view: %v", pool)
	}
	out = e.expect("PUT", "/evaluation/pools/acct", map[string]interface{}{"start_rate": 0.5, "burst_capacity": 3, "concurrent_limit": 2}, 200, "")
	pool = out["pool"].(map[string]interface{})
	if pool["mode"] != "rate" || pool["burst_capacity"].(float64) != 3 || pool["tokens"].(float64) != 3 {
		t.Fatalf("rate pool view: %v", pool)
	}
	// A pool's mode cannot change.
	e.expect("PUT", "/evaluation/pools/acct", map[string]interface{}{"concurrency_only": true, "concurrent_limit": 2}, 400, "INVALID_INPUT")
	got := e.expect("GET", "/evaluation/pools/acct", nil, 200, "")
	if got["pool"].(map[string]interface{})["mode"] != "rate" {
		t.Fatalf("pool mode changed: %v", got)
	}
}

func TestEvaluationAPICandidates(t *testing.T) {
	e := newEvalAPI(t)
	e.pool("pool1", 5)
	e.campaign("camp1", 10)
	path := "/evaluation/campaigns/camp1/candidates"

	valid := func() map[string]interface{} {
		return map[string]interface{}{"id": "cand-a", "per_candidate_cap": 4, "identity": evalIdentity("model-a", "pool1")}
	}
	emptyIdentity := valid()
	emptyIdentity["identity"] = evaluation.CandidateIdentity{}
	zeroCap := valid()
	zeroCap["per_candidate_cap"] = 0
	hugeCap := valid()
	hugeCap["per_candidate_cap"] = 100001
	extra := valid()
	extra["config_digest"] = "forged"
	e.expect("POST", path, emptyIdentity, 400, "INVALID_INPUT")
	e.expect("POST", path, zeroCap, 400, "INVALID_INPUT")
	e.expect("POST", path, hugeCap, 400, "INVALID_INPUT")
	e.expect("POST", path, extra, 400, "INVALID_INPUT")
	// The model must be one the campaign allows.
	notAllowed := valid()
	notAllowed["identity"] = evalIdentity("model-z", "pool1")
	e.expect("POST", path, notAllowed, 400, "MODEL_NOT_ALLOWED")
	e.expect("POST", "/evaluation/campaigns/nope/candidates", valid(), 404, "CAMPAIGN_NOT_FOUND")

	out := e.expect("POST", path, valid(), 201, "")
	cand := out["candidate"].(map[string]interface{})
	want := evalIdentity("model-a", "pool1").Digest()
	if cand["id"] != "cand-a" || cand["config_digest"] != want || cand["account_pool_id"] != "pool1" || cand["campaign_id"] != "camp1" {
		t.Fatalf("candidate view: %v", cand)
	}
	e.expect("POST", path, valid(), 409, "ALREADY_EXISTS")

	// A second candidate with a different model and its own cap and pool.
	e.pool("pool2", 1)
	second := map[string]interface{}{"id": "cand-b", "per_candidate_cap": 2, "identity": evalIdentity("model-b", "pool2")}
	e.expect("POST", path, second, 201, "")

	list := e.expect("GET", path, nil, 200, "")["candidates"].([]interface{})
	if len(list) != 2 {
		t.Fatalf("listed %d candidates, want 2: %v", len(list), list)
	}
	ids := map[string]float64{}
	for _, c := range list {
		m := c.(map[string]interface{})
		ids[m["id"].(string)] = m["per_candidate_cap"].(float64)
	}
	if ids["cand-a"] != 4 || ids["cand-b"] != 2 {
		t.Fatalf("candidate caps: %v", ids)
	}
	e.expect("GET", "/evaluation/campaigns/nope/candidates", nil, 404, "CAMPAIGN_NOT_FOUND")
}

func TestEvaluationAPISampleRead(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	e.campaign("camp2", 5)
	sample := e.expect("GET", "/evaluation/campaigns/camp1/samples/s1", nil, 200, "")["sample"].(map[string]interface{})
	if sample["id"] != "s1" || sample["campaign_id"] != "camp1" || sample["submitted_sha"] != "abc123" || sample["original_task_id"] != "task-x" {
		t.Fatalf("sample view: %v", sample)
	}
	e.expect("GET", "/evaluation/campaigns/camp1/samples/nope", nil, 404, "SAMPLE_NOT_FOUND")
	// A sample is not visible through a different campaign.
	e.expect("GET", "/evaluation/campaigns/camp2/samples/s1", nil, 404, "SAMPLE_NOT_FOUND")
}

func TestEvaluationAPIClaimValidation(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	valid := func() map[string]interface{} {
		return map[string]interface{}{"sample_id": "s1", "candidate_id": "cand-a", "request_id": "r1", "lease_ttl_ms": 1000}
	}
	cases := map[string]func(m map[string]interface{}){
		"missing sample":    func(m map[string]interface{}) { delete(m, "sample_id") },
		"missing candidate": func(m map[string]interface{}) { delete(m, "candidate_id") },
		"missing request":   func(m map[string]interface{}) { delete(m, "request_id") },
		"missing ttl":       func(m map[string]interface{}) { delete(m, "lease_ttl_ms") },
		"zero ttl":          func(m map[string]interface{}) { m["lease_ttl_ms"] = 0 },
		"negative ttl":      func(m map[string]interface{}) { m["lease_ttl_ms"] = -5 },
		"over-long ttl":     func(m map[string]interface{}) { m["lease_ttl_ms"] = 3600001 },
		"unknown field":     func(m map[string]interface{}) { m["cap_override"] = 99 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := valid()
			mutate(m)
			e.expect("POST", "/evaluation/jobs/claim", m, 400, "INVALID_INPUT")
		})
	}
	m := valid()
	m["sample_id"] = "nope"
	e.expect("POST", "/evaluation/jobs/claim", m, 404, "SAMPLE_NOT_FOUND")
	m = valid()
	m["candidate_id"] = "nope"
	e.expect("POST", "/evaluation/jobs/claim", m, 404, "CANDIDATE_NOT_FOUND")

	// Nothing above spent any cap.
	st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
	if st["attempts_used"].(float64) != 0 {
		t.Fatalf("validation failures spent cap: %v", st)
	}
}

func TestEvaluationAPIClaimUnconfiguredPool(t *testing.T) {
	e := newEvalAPI(t)
	e.campaign("camp1", 5)
	e.candidate("camp1", "cand-a", "model-a", "ghost-pool", 5)
	e.sample("camp1", "s1", "proj1", "task-x")
	e.expect("POST", "/evaluation/jobs/claim", map[string]interface{}{
		"sample_id": "s1", "candidate_id": "cand-a", "request_id": "r1", "lease_ttl_ms": 1000,
	}, 409, "POOL_NOT_CONFIGURED")
	st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
	if st["attempts_used"].(float64) != 0 {
		t.Fatalf("unconfigured pool still spent cap: %v", st)
	}
}

func TestEvaluationAPIClaimReplaySafe(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	first := e.claim("s1", "cand-a", "r1")
	again := e.claim("s1", "cand-a", "r1")
	if first != again {
		t.Fatalf("replayed claim returned a different attempt: %v vs %v", first, again)
	}
	st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
	if st["attempts_used"].(float64) != 1 {
		t.Fatalf("replayed claim spent cap twice: %v", st)
	}
	// A different request for the same sample while the attempt is live is refused.
	e.expect("POST", "/evaluation/jobs/claim", map[string]interface{}{
		"sample_id": "s1", "candidate_id": "cand-a", "request_id": "r2", "lease_ttl_ms": 1000,
	}, 409, "ATTEMPT_LIVE")
}

func TestEvaluationAPIFinalizeValidation(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	c := e.claim("s1", "cand-a", "r1")
	finding := func(id, sev, summary string) map[string]interface{} {
		return map[string]interface{}{"id": id, "severity": sev, "summary": summary}
	}
	bad := map[string]map[string]interface{}{
		"missing fence":        {"exit_class": "completed", "status": "completed"},
		"missing exit class":   {"fence_attempt_id": c.attemptID},
		"unknown exit class":   {"fence_attempt_id": c.attemptID, "exit_class": "great"},
		"store-only exit":      {"fence_attempt_id": c.attemptID, "exit_class": "lease_expired"},
		"unknown status":       {"fence_attempt_id": c.attemptID, "exit_class": "failed", "status": "meh"},
		"exit contradicts":     {"fence_attempt_id": c.attemptID, "exit_class": "completed", "status": "failed"},
		"bad error class":      {"fence_attempt_id": c.attemptID, "exit_class": "failed", "error_class": "oops"},
		"negative duration":    {"fence_attempt_id": c.attemptID, "exit_class": "failed", "duration_ms": -1},
		"findings on failure":  {"fence_attempt_id": c.attemptID, "exit_class": "failed", "findings": []map[string]interface{}{finding("f1", "minor", "x")}},
		"bad severity":         {"fence_attempt_id": c.attemptID, "exit_class": "completed", "status": "completed", "findings": []map[string]interface{}{finding("f1", "huge", "x")}},
		"empty summary":        {"fence_attempt_id": c.attemptID, "exit_class": "completed", "status": "completed", "findings": []map[string]interface{}{finding("f1", "minor", " ")}},
		"duplicate finding id": {"fence_attempt_id": c.attemptID, "exit_class": "completed", "status": "completed", "findings": []map[string]interface{}{finding("f1", "minor", "x"), finding("f1", "note", "y")}},
		"unknown finding key":  {"fence_attempt_id": c.attemptID, "exit_class": "completed", "status": "completed", "findings": []map[string]interface{}{{"id": "f1", "severity": "minor", "summary": "x", "verdict": "approve"}}},
		"unknown top key":      {"fence_attempt_id": c.attemptID, "exit_class": "completed", "status": "completed", "verdict": "approve"},
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) { e.expect("POST", c.path("finalize"), body, 400, "INVALID_INPUT") })
	}
	// None of the rejected requests finalized the attempt.
	att, err := e.store.GetEvaluationAttempt(t.Context(), c.attemptID)
	if err != nil || att.State != store.EvalAttemptActive {
		t.Fatalf("attempt after rejected results: %+v, %v", att, err)
	}
	if fs, _ := e.store.ListEvaluationFindings(t.Context(), c.attemptID); len(fs) != 0 {
		t.Fatalf("rejected results recorded findings: %v", fs)
	}
}

func TestEvaluationAPIFinalizeRecordsAndRejectsReplay(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	c := e.claim("s1", "cand-a", "r1")
	res := completedResult(c.attemptID,
		map[string]interface{}{"id": "f1", "severity": "material", "claim": "c", "summary": "broken", "evidence": "line 3"},
		map[string]interface{}{"id": "f2", "severity": "note", "summary": "nit"})
	out := e.expect("POST", c.path("finalize"), res, 200, "")
	att := out["attempt"].(map[string]interface{})
	if att["state"] != "finalized" || att["exit_class"] != "completed" || att["status"] != "completed" || out["finding_count"].(float64) != 2 {
		t.Fatalf("finalize response: %v", out)
	}
	fs, err := e.store.ListEvaluationFindings(t.Context(), c.attemptID)
	if err != nil || len(fs) != 2 || fs[0].ID != "f1" || fs[1].ID != "f2" {
		t.Fatalf("recorded findings: %v, %v", fs, err)
	}
	// A repeat of the same result is refused and records nothing further.
	e.expect("POST", c.path("finalize"), res, 409, "ATTEMPT_FINALIZED")
	if fs, _ := e.store.ListEvaluationFindings(t.Context(), c.attemptID); len(fs) != 2 {
		t.Fatalf("replayed finalize duplicated findings: %v", fs)
	}
	// A finalized attempt cannot be renewed.
	e.expect("POST", c.path("renew"), map[string]interface{}{"lease_ttl_ms": 120000}, 409, "ATTEMPT_FINALIZED")
}

func TestEvaluationAPIStaleResultsRejected(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	e.sample("camp1", "s2", "proj1", "task-y")

	t.Run("fence mismatch", func(t *testing.T) {
		c := e.claim("s1", "cand-a", "r1")
		other := e.claim("s2", "cand-a", "r-other")
		res := completedResult(other.attemptID)
		e.expect("POST", c.path("finalize"), res, 409, "FENCE_MISMATCH")
		att, _ := e.store.GetEvaluationAttempt(t.Context(), c.attemptID)
		if att.State != store.EvalAttemptActive {
			t.Fatalf("fence mismatch changed attempt state: %s", att.State)
		}
	})

	t.Run("wrong job in path", func(t *testing.T) {
		a := e.claim("s1", "cand-a", "r1")
		b := e.claim("s2", "cand-a", "r-other")
		crossed := "/evaluation/jobs/" + b.jobID + "/attempts/" + a.attemptID + "/finalize"
		e.expect("POST", crossed, completedResult(a.attemptID), 404, "ATTEMPT_NOT_FOUND")
		crossed = "/evaluation/jobs/" + b.jobID + "/attempts/" + a.attemptID + "/renew"
		e.expect("POST", crossed, map[string]interface{}{"lease_ttl_ms": 120000}, 404, "ATTEMPT_NOT_FOUND")
		e.expect("POST", "/evaluation/jobs/"+a.jobID+"/attempts/missing/finalize", completedResult("missing"), 404, "ATTEMPT_NOT_FOUND")
	})

	t.Run("expired lease", func(t *testing.T) {
		e2 := newEvalAPI(t)
		e2.basicSetup(5, 5, 5)
		c := e2.claim("s1", "cand-a", "r1")
		e2.clock.Advance(2 * time.Minute)
		e2.expect("POST", c.path("finalize"), completedResult(c.attemptID, map[string]interface{}{"id": "f1", "severity": "minor", "summary": "late"}), 409, "ATTEMPT_EXPIRED")
		att, _ := e2.store.GetEvaluationAttempt(t.Context(), c.attemptID)
		if att.State != store.EvalAttemptExpired || att.ExitClass == nil || *att.ExitClass != store.EvalExitLeaseExpired {
			t.Fatalf("late result should record lease_expired: %+v", att)
		}
		if fs, _ := e2.store.ListEvaluationFindings(t.Context(), c.attemptID); len(fs) != 0 {
			t.Fatalf("late result recorded findings: %v", fs)
		}
		e2.expect("POST", c.path("renew"), map[string]interface{}{"lease_ttl_ms": 120000}, 409, "ATTEMPT_EXPIRED")
	})

	t.Run("superseded attempt", func(t *testing.T) {
		e2 := newEvalAPI(t)
		e2.basicSetup(5, 5, 5)
		old := e2.claim("s1", "cand-a", "r1")
		e2.clock.Advance(2 * time.Minute)
		retry := e2.claim("s1", "cand-a", "r2")
		if retry.jobID != old.jobID || retry.attemptID == old.attemptID {
			t.Fatalf("retry should be a new attempt on the same job: %v vs %v", old, retry)
		}
		e2.expect("POST", old.path("finalize"), completedResult(old.attemptID), 409, "")
		e2.expect("POST", retry.path("finalize"), completedResult(retry.attemptID), 200, "")
	})
}

func TestEvaluationAPIRenew(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	c := e.claim("s1", "cand-a", "r1")
	for _, body := range []interface{}{
		map[string]interface{}{}, map[string]interface{}{"lease_ttl_ms": 0}, map[string]interface{}{"lease_ttl_ms": -1},
		map[string]interface{}{"lease_ttl_ms": 3600001}, map[string]interface{}{"lease_ttl_ms": 1000, "extra": 1},
	} {
		e.expect("POST", c.path("renew"), body, 400, "INVALID_INPUT")
	}
	// Shorter than the current 60s lease: must extend, not shorten.
	e.expect("POST", c.path("renew"), map[string]interface{}{"lease_ttl_ms": 1000}, 400, "INVALID_INPUT")

	before, _ := e.store.GetEvaluationAttempt(t.Context(), c.attemptID)
	out := e.expect("POST", c.path("renew"), map[string]interface{}{"lease_ttl_ms": 300000}, 200, "")
	after := out["attempt"].(map[string]interface{})
	if after["expires_at"] == before.ExpiresAt || after["state"] != "active" || after["id"] != c.attemptID {
		t.Fatalf("renew did not extend the lease: before %s, after %v", before.ExpiresAt, after)
	}
	want := e.clock.Now().Add(5 * time.Minute).UTC().Format("2006-01-02T15:04:05.000Z")
	if got := after["expires_at"].(string); !strings.HasPrefix(got, want[:19]) {
		t.Fatalf("lease should be ttl from the server clock: got %s, want prefix %s", got, want[:19])
	}
}

func TestEvaluationAPICapExhaustion(t *testing.T) {
	t.Run("campaign cap", func(t *testing.T) {
		e := newEvalAPI(t)
		e.basicSetup(1, 5, 5)
		e.sample("camp1", "s2", "proj1", "task-y")
		c := e.claim("s1", "cand-a", "r1")
		e.expect("POST", c.path("finalize"), completedResult(c.attemptID), 200, "")
		e.expect("POST", "/evaluation/jobs/claim", map[string]interface{}{
			"sample_id": "s2", "candidate_id": "cand-a", "request_id": "r2", "lease_ttl_ms": 1000,
		}, 409, "CAPACITY_EXHAUSTED")
		st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
		if st["state"] != "exhausted" || st["attempts_remaining"].(float64) != 0 || st["attempts_used"].(float64) != 1 {
			t.Fatalf("status after exhaustion: %v", st)
		}
	})

	t.Run("per-candidate cap leaves other candidates available", func(t *testing.T) {
		e := newEvalAPI(t)
		e.basicSetup(10, 1, 5)
		e.candidate("camp1", "cand-b", "model-b", "pool1", 3)
		e.sample("camp1", "s2", "proj1", "task-y")
		c := e.claim("s1", "cand-a", "r1")
		e.expect("POST", c.path("finalize"), completedResult(c.attemptID), 200, "")
		e.expect("POST", "/evaluation/jobs/claim", map[string]interface{}{
			"sample_id": "s2", "candidate_id": "cand-a", "request_id": "r2", "lease_ttl_ms": 1000,
		}, 409, "CAPACITY_EXHAUSTED")
		e.claim("s2", "cand-b", "r3")

		st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
		if st["state"] != "active" || st["attempts_used"].(float64) != 2 {
			t.Fatalf("status: %v", st)
		}
		byID := map[string]map[string]interface{}{}
		for _, c := range st["candidates"].([]interface{}) {
			m := c.(map[string]interface{})
			byID[m["candidate_id"].(string)] = m
		}
		a, b := byID["cand-a"], byID["cand-b"]
		if a["exhausted"] != true || a["attempts_remaining"].(float64) != 0 || a["attempts_used"].(float64) != 1 || a["per_candidate_cap"].(float64) != 1 {
			t.Fatalf("candidate a status: %v", a)
		}
		if b["exhausted"] != false || b["attempts_remaining"].(float64) != 2 || b["active_attempts"].(float64) != 1 || b["model_id"] != "model-b" {
			t.Fatalf("candidate b status: %v", b)
		}
	})
}

func TestEvaluationAPIPoolAdmission(t *testing.T) {
	t.Run("compute pool concurrency", func(t *testing.T) {
		e := newEvalAPI(t)
		e.basicSetup(10, 10, 1)
		e.sample("camp1", "s2", "proj1", "task-y")
		c := e.claim("s1", "cand-a", "r1")
		status, out := e.call("POST", "/evaluation/jobs/claim", map[string]interface{}{
			"sample_id": "s2", "candidate_id": "cand-a", "request_id": "r2", "lease_ttl_ms": 1000,
		})
		if status != http.StatusTooManyRequests {
			t.Fatalf("status %d, want 429: %v", status, out)
		}
		// A denied start spends no cap.
		st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
		if st["attempts_used"].(float64) != 1 {
			t.Fatalf("denied claim spent cap: %v", st)
		}
		pool := st["candidates"].([]interface{})[0].(map[string]interface{})["pool"].(map[string]interface{})
		if pool["id"] != "pool1" || pool["mode"] != "concurrency_only" || pool["active"].(float64) != 1 || pool["concurrent_limit"].(float64) != 1 {
			t.Fatalf("pool in status: %v", pool)
		}
		// Finalizing releases occupancy.
		e.expect("POST", c.path("finalize"), completedResult(c.attemptID), 200, "")
		e.claim("s2", "cand-a", "r2")
	})

	t.Run("rate pool burst", func(t *testing.T) {
		e := newEvalAPI(t)
		e.expect("PUT", "/evaluation/pools/acct", map[string]interface{}{"start_rate": 0.001, "burst_capacity": 1, "concurrent_limit": 5}, 200, "")
		e.campaign("camp1", 10)
		e.candidate("camp1", "cand-a", "model-a", "acct", 10)
		e.sample("camp1", "s1", "proj1", "task-x")
		e.sample("camp1", "s2", "proj1", "task-y")
		c := e.claim("s1", "cand-a", "r1")
		e.expect("POST", c.path("finalize"), completedResult(c.attemptID), 200, "")
		e.expect("POST", "/evaluation/jobs/claim", map[string]interface{}{
			"sample_id": "s2", "candidate_id": "cand-a", "request_id": "r2", "lease_ttl_ms": 1000,
		}, http.StatusTooManyRequests, "")
	})
}

func TestEvaluationAPIPauseWaiting(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(5, 5, 5)
	e.sample("camp1", "s2", "proj1", "task-y")
	live := e.claim("s1", "cand-a", "r1")

	e.expect("POST", "/evaluation/campaigns/nope/pause", nil, 404, "CAMPAIGN_NOT_FOUND")
	out := e.expect("POST", "/evaluation/campaigns/camp1/pause", nil, 200, "")
	if out["state"] != "paused" || out["campaign_id"] != "camp1" || out["paused_at"] == nil {
		t.Fatalf("pause response: %v", out)
	}
	e.expect("POST", "/evaluation/campaigns/camp1/pause", nil, 409, "ALREADY_PAUSED")

	claimBody := map[string]interface{}{"sample_id": "s2", "candidate_id": "cand-a", "request_id": "r2", "lease_ttl_ms": 1000}
	e.expect("POST", "/evaluation/jobs/claim", claimBody, 409, "PAUSED_WAITING")

	st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
	if st["state"] != "paused" || st["paused_at"] == nil || st["attempts_used"].(float64) != 1 {
		t.Fatalf("paused status: %v", st)
	}
	camp := e.expect("GET", "/evaluation/campaigns/camp1", nil, 200, "")["campaign"].(map[string]interface{})
	if camp["paused_at"] == nil {
		t.Fatalf("campaign view should show the pause: %v", camp)
	}

	// Already-live attempts may still renew and finish.
	e.expect("POST", live.path("renew"), map[string]interface{}{"lease_ttl_ms": 300000}, 200, "")
	e.expect("POST", live.path("finalize"), completedResult(live.attemptID), 200, "")
	e.expect("POST", "/evaluation/jobs/claim", claimBody, 409, "PAUSED_WAITING")
}

func TestEvaluationAPIStatusCompact(t *testing.T) {
	e := newEvalAPI(t)
	e.basicSetup(4, 3, 5)
	st := e.expect("GET", "/evaluation/campaigns/camp1/status", nil, 200, "")
	for _, key := range []string{"campaign_id", "name", "state", "paused_at", "attempt_cap", "attempts_used", "attempts_remaining", "candidates"} {
		if _, ok := st[key]; !ok {
			t.Errorf("status is missing %q: %v", key, st)
		}
	}
	if st["state"] != "active" || st["attempt_cap"].(float64) != 4 || st["attempts_remaining"].(float64) != 4 {
		t.Fatalf("fresh status: %v", st)
	}
	cand := st["candidates"].([]interface{})[0].(map[string]interface{})
	for _, key := range []string{"candidate_id", "config_digest", "adapter_name", "model_id", "account_pool_id", "per_candidate_cap", "attempts_used", "attempts_remaining", "active_attempts", "exhausted", "pool"} {
		if _, ok := cand[key]; !ok {
			t.Errorf("candidate status is missing %q: %v", key, cand)
		}
	}
	raw, _ := json.Marshal(st)
	if len(raw) > 2048 {
		t.Errorf("status is not compact: %d bytes", len(raw))
	}
	// No findings, prompts or samples leak into the compact status.
	for _, banned := range []string{"findings", "summary", "evidence", "submitted_sha"} {
		if strings.Contains(string(raw), banned) {
			t.Errorf("status contains %q", banned)
		}
	}
}

// An evaluation result is recorded only in evaluation tables: it never changes a
// task, spawns or alters a review task, or writes a verdict event, so it cannot
// vote on a research task.
func TestEvaluationResultCannotVoteOnTask(t *testing.T) {
	e := newEvalAPI(t)
	taskID := setupTaskInReview(t, e.server, evalAuth)
	task, err := e.store.GetTask(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	projectID := task.ProjectID

	e.pool("pool1", 5)
	e.campaign("camp1", 5, projectID)
	e.candidate("camp1", "cand-a", "model-a", "pool1", 5)
	e.sample("camp1", "s1", projectID, taskID)

	snapshot := func() string {
		var sb strings.Builder
		for _, q := range []string{
			`SELECT id, state, model, COALESCE(assignee,''), COALESCE(lease_expires_at,'') FROM task ORDER BY id`,
			`SELECT task_id, kind, COALESCE(actor,''), COALESCE(verdict,''), COALESCE(note,'') FROM event ORDER BY id`,
		} {
			rows, err := e.store.Conn().QueryContext(t.Context(), q)
			if err != nil {
				t.Fatalf("snapshot %q: %v", q, err)
			}
			cols, _ := rows.Columns()
			for rows.Next() {
				vals := make([]interface{}, len(cols))
				ptrs := make([]interface{}, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					t.Fatal(err)
				}
				for _, v := range vals {
					sb.WriteString(toStr(v))
					sb.WriteByte('|')
				}
				sb.WriteByte('\n')
			}
			rows.Close()
		}
		return sb.String()
	}
	before := snapshot()
	if before == "" {
		t.Fatal("empty snapshot")
	}

	c := e.claim("s1", "cand-a", "r1")
	e.expect("POST", c.path("renew"), map[string]interface{}{"lease_ttl_ms": 120000}, 200, "")
	e.expect("POST", c.path("finalize"), completedResult(c.attemptID,
		map[string]interface{}{"id": "f1", "severity": "material", "summary": "would block"}), 200, "")
	e.expect("POST", "/evaluation/campaigns/camp1/pause", nil, 200, "")

	if after := snapshot(); after != before {
		t.Fatalf("evaluation lifecycle changed task or review state:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	got, err := e.store.GetTask(t.Context(), taskID)
	if err != nil || got.State != task.State {
		t.Fatalf("task state changed: %v -> %v (%v)", task.State, got.State, err)
	}
	// The evaluation API has no route that takes a task id or a verdict.
	e.expect("POST", "/evaluation/jobs/"+c.jobID+"/attempts/"+c.attemptID+"/finalize",
		map[string]interface{}{"fence_attempt_id": c.attemptID, "exit_class": "completed", "task_id": taskID, "verdict": "approve"}, 400, "INVALID_INPUT")
}

func toStr(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}
