package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/api"
	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

type recordedRequest struct {
	method, path, auth string
	body               map[string]interface{}
}

// fakeEvaluationServer answers every request with the given status and body and
// records what the CLI sent.
func fakeEvaluationServer(t *testing.T, status int, response string) (*httptest.Server, *recordedRequest) {
	t.Helper()
	rec := &recordedRequest{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.method, rec.path, rec.auth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		rec.body = nil
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &rec.body); err != nil {
				t.Errorf("CLI sent non-JSON body %q", raw)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, response)
	}))
	t.Cleanup(ts.Close)
	return ts, rec
}

func runEval(t *testing.T, url, verb string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := executeEvaluation(context.Background(), verb, url, "tok", args, &out)
	return out.String(), err
}

func TestEvaluationCLIRequests(t *testing.T) {
	identity := `{"adapter_name":"fake","model_id":"m"}`
	cases := []struct {
		name       string
		verb       string
		args       []string
		wantMethod string
		wantPath   string
		wantBody   map[string]interface{}
	}{
		{"pool-set compute", "evaluation-pool-set", []string{"--id", "p1", "--concurrency-only", "--concurrent-limit", "2"},
			"PUT", "/evaluation/pools/p1", map[string]interface{}{"concurrency_only": true, "concurrent_limit": 2.0}},
		{"pool-set rate", "evaluation-pool-set", []string{"--id", "p1", "--start-rate", "0.5", "--burst", "3", "--concurrent-limit", "2"},
			"PUT", "/evaluation/pools/p1", map[string]interface{}{"concurrency_only": false, "start_rate": 0.5, "burst_capacity": 3.0, "concurrent_limit": 2.0}},
		{"pool-get", "evaluation-pool-get", []string{"--id", "p1"}, "GET", "/evaluation/pools/p1", nil},
		{"create-campaign", "evaluation-create-campaign",
			[]string{"--id", "c1", "--name", "n", "--projects", "a, b", "--models", "m1,m2", "--cohort", "{}", "--cap", "7", "--description", "d"},
			"POST", "/evaluation/campaigns", map[string]interface{}{
				"id": "c1", "name": "n", "description": "d", "allowed_project_ids": []interface{}{"a", "b"},
				"allowed_model_ids": []interface{}{"m1", "m2"}, "cohort_manifest": "{}", "attempt_cap": 7.0}},
		{"get-campaign", "evaluation-get-campaign", []string{"--id", "c1"}, "GET", "/evaluation/campaigns/c1", nil},
		{"status", "evaluation-get-campaign-status", []string{"--id", "c1"}, "GET", "/evaluation/campaigns/c1/status", nil},
		{"pause", "evaluation-pause-campaign", []string{"--id", "c1"}, "POST", "/evaluation/campaigns/c1/pause", nil},
		{"create-candidate", "evaluation-create-candidate", []string{"--campaign", "c1", "--id", "v1", "--cap", "4", "--identity", identity},
			"POST", "/evaluation/campaigns/c1/candidates", map[string]interface{}{
				"id": "v1", "per_candidate_cap": 4.0, "identity": map[string]interface{}{"adapter_name": "fake", "model_id": "m"}}},
		{"list-candidates", "evaluation-list-candidates", []string{"--campaign", "c1"}, "GET", "/evaluation/campaigns/c1/candidates", nil},
		{"get-sample", "evaluation-get-sample", []string{"--campaign", "c1", "--sample", "s1"}, "GET", "/evaluation/campaigns/c1/samples/s1", nil},
		{"claim", "evaluation-claim-job", []string{"--sample", "s1", "--candidate", "v1", "--request-id", "r1", "--ttl-ms", "60000"},
			"POST", "/evaluation/jobs/claim", map[string]interface{}{"sample_id": "s1", "candidate_id": "v1", "request_id": "r1", "lease_ttl_ms": 60000.0}},
		{"renew", "evaluation-renew-attempt", []string{"--job", "j1", "--attempt", "a1", "--ttl-ms", "90000"},
			"POST", "/evaluation/jobs/j1/attempts/a1/renew", map[string]interface{}{"lease_ttl_ms": 90000.0}},
		{"finalize", "evaluation-finalize-attempt", []string{"--job", "j1", "--attempt", "a1", "--exit-class", "completed",
			"--result", `{"status":"completed","duration_ms":12,"findings":[{"id":"f1","severity":"minor","summary":"s"}]}`},
			"POST", "/evaluation/jobs/j1/attempts/a1/finalize", map[string]interface{}{
				"fence_attempt_id": "a1", "exit_class": "completed", "status": "completed", "duration_ms": 12.0,
				"findings": []interface{}{map[string]interface{}{"id": "f1", "severity": "minor", "summary": "s"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, rec := fakeEvaluationServer(t, 200, "{\n  \"ok\": true,\n  \"n\": 1\n}\n")
			out, err := runEval(t, ts.URL, tc.verb, tc.args...)
			if err != nil {
				t.Fatalf("%s: %v", tc.verb, err)
			}
			if rec.method != tc.wantMethod || rec.path != tc.wantPath {
				t.Fatalf("request %s %s, want %s %s", rec.method, rec.path, tc.wantMethod, tc.wantPath)
			}
			if rec.auth != "Bearer tok" {
				t.Fatalf("Authorization %q", rec.auth)
			}
			if !jsonEqual(rec.body, tc.wantBody) {
				t.Fatalf("body %v, want %v", rec.body, tc.wantBody)
			}
			if out != "{\"ok\":true,\"n\":1}\n" {
				t.Fatalf("output must be the server JSON on one compact line, got %q", out)
			}
		})
	}
}

func jsonEqual(a, b map[string]interface{}) bool {
	ra, _ := json.Marshal(a)
	rb, _ := json.Marshal(b)
	return string(ra) == string(rb)
}

func TestEvaluationCLIPathEscaping(t *testing.T) {
	ts, rec := fakeEvaluationServer(t, 200, "{}")
	if _, err := runEval(t, ts.URL, "evaluation-get-campaign", "--id", "a/b c"); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/evaluation/campaigns/a%2Fb%20c" {
		t.Fatalf("id must be path-escaped, got %q", rec.path)
	}
}

// --result can supply the findings and metrics but never the fence or exit class.
func TestEvaluationCLIFinalizeResultCannotOverrideFence(t *testing.T) {
	ts, rec := fakeEvaluationServer(t, 200, "{}")
	for _, result := range []string{
		`{"fence_attempt_id":"other"}`, `{"exit_class":"failed"}`, `{"task_id":"t","verdict":"approve"}`,
	} {
		_, err := runEval(t, ts.URL, "evaluation-finalize-attempt", "--job", "j", "--attempt", "a1", "--exit-class", "completed", "--result", result)
		if err == nil || !strings.Contains(err.Error(), "invalid --result") {
			t.Fatalf("--result %s: want invalid --result error, got %v", result, err)
		}
	}
	if rec.method != "" {
		t.Fatalf("a rejected --result must not reach the server, saw %s %s", rec.method, rec.path)
	}
	if _, err := runEval(t, ts.URL, "evaluation-finalize-attempt", "--job", "j", "--attempt", "a1", "--exit-class", "failed"); err != nil {
		t.Fatal(err)
	}
	if rec.body["fence_attempt_id"] != "a1" || rec.body["exit_class"] != "failed" {
		t.Fatalf("flags must define the fence and exit class: %v", rec.body)
	}
}

func TestEvaluationCLIResultAndIdentityFiles(t *testing.T) {
	dir := t.TempDir()
	resultFile := filepath.Join(dir, "result.json")
	identityFile := filepath.Join(dir, "identity.json")
	os.WriteFile(resultFile, []byte(`{"status":"failed","error_class":"timeout"}`), 0o600)
	os.WriteFile(identityFile, []byte(`{"adapter_name":"fake"}`), 0o600)
	ts, rec := fakeEvaluationServer(t, 200, "{}")
	if _, err := runEval(t, ts.URL, "evaluation-finalize-attempt", "--job", "j", "--attempt", "a", "--exit-class", "failed", "--result-file", resultFile); err != nil {
		t.Fatal(err)
	}
	if rec.body["status"] != "failed" || rec.body["error_class"] != "timeout" {
		t.Fatalf("result file not used: %v", rec.body)
	}
	if _, err := runEval(t, ts.URL, "evaluation-create-candidate", "--campaign", "c", "--id", "v", "--cap", "1", "--identity-file", identityFile); err != nil {
		t.Fatal(err)
	}
	if rec.body["identity"].(map[string]interface{})["adapter_name"] != "fake" {
		t.Fatalf("identity file not used: %v", rec.body)
	}
}

func TestEvaluationCLIFlagValidation(t *testing.T) {
	ts, rec := fakeEvaluationServer(t, 200, "{}")
	cases := []struct {
		verb string
		args []string
		want string
	}{
		{"evaluation-pool-set", []string{"--concurrent-limit", "1", "--concurrency-only"}, "--id required"},
		{"evaluation-pool-set", []string{"--id", "p"}, "--concurrent-limit"},
		{"evaluation-pool-set", []string{"--id", "p", "--concurrent-limit", "1"}, "rate pool needs"},
		{"evaluation-pool-set", []string{"--id", "p", "--concurrent-limit", "1", "--concurrency-only", "--burst", "2"}, "cannot be combined"},
		{"evaluation-pool-get", nil, "--id required"},
		{"evaluation-create-campaign", []string{"--id", "c", "--name", "n", "--projects", "p", "--models", "m", "--cohort", "{}"}, "--cap must be at least 1"},
		{"evaluation-create-campaign", []string{"--id", "c", "--cap", "1"}, "--name, --projects, --models, --cohort required"},
		{"evaluation-create-candidate", []string{"--campaign", "c", "--id", "v", "--identity", "{}"}, "--cap must be at least 1"},
		{"evaluation-create-candidate", []string{"--campaign", "c", "--id", "v", "--cap", "1"}, "--identity or --identity-file required"},
		{"evaluation-create-candidate", []string{"--campaign", "c", "--id", "v", "--cap", "1", "--identity", "{bad"}, "not valid JSON"},
		{"evaluation-create-candidate", []string{"--campaign", "c", "--id", "v", "--cap", "1", "--identity", "{}", "--identity-file", "x"}, "not both"},
		{"evaluation-claim-job", []string{"--sample", "s", "--candidate", "c", "--ttl-ms", "5"}, "--request-id required"},
		{"evaluation-claim-job", []string{"--sample", "s", "--candidate", "c", "--request-id", "r"}, "--ttl-ms must be at least 1"},
		{"evaluation-renew-attempt", []string{"--job", "j", "--attempt", "a"}, "--ttl-ms must be at least 1"},
		{"evaluation-renew-attempt", []string{"--attempt", "a", "--ttl-ms", "5"}, "--job required"},
		{"evaluation-finalize-attempt", []string{"--job", "j", "--attempt", "a"}, "--exit-class required"},
		{"evaluation-get-sample", []string{"--campaign", "c"}, "--sample required"},
		{"evaluation-pause-campaign", []string{"--id", "c", "extra"}, "unexpected argument"},
		{"evaluation-get-campaign-status", []string{"--bogus"}, "failed to parse flags"},
	}
	for _, tc := range cases {
		_, err := runEval(t, ts.URL, tc.verb, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %v: error %v, want containing %q", tc.verb, tc.args, err, tc.want)
		}
	}
	if rec.method != "" {
		t.Fatalf("invalid invocations must not call the server, saw %s %s", rec.method, rec.path)
	}
	if _, err := executeEvalNoURL(); err == nil {
		t.Fatal("missing ODONIAN_URL must fail")
	}
}

func executeEvalNoURL() (string, error) {
	var out bytes.Buffer
	err := executeEvaluation(context.Background(), "evaluation-pool-get", "", "tok", []string{"--id", "p"}, &out)
	return out.String(), err
}

func TestEvaluationCLIExitMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantSched  bool
		wantConfl  bool
		wantInText string
	}{
		{"paused", 409, `{"error":{"code":"PAUSED_WAITING","message":"paused"}}`, false, true, "PAUSED_WAITING"},
		{"exhausted", 409, `{"error":{"code":"CAPACITY_EXHAUSTED","message":"cap"}}`, false, true, "CAPACITY_EXHAUSTED"},
		{"stale", 409, `{"error":{"code":"FENCE_MISMATCH","message":"stale"}}`, false, true, "FENCE_MISMATCH"},
		{"finalized", 409, `{"error":{"code":"ATTEMPT_FINALIZED","message":"done"}}`, false, true, "ATTEMPT_FINALIZED"},
		{"pool denial", 429, `{"error":{"code":"ADMISSION_DENIED","message":"busy"}}`, true, false, ""},
		{"bad request", 400, `{"error":{"code":"INVALID_INPUT","message":"nope"}}`, false, false, "INVALID_INPUT"},
		{"not found", 404, `{"error":{"code":"CAMPAIGN_NOT_FOUND","message":"gone"}}`, false, false, "CAMPAIGN_NOT_FOUND"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := fakeEvaluationServer(t, tc.status, tc.body)
			_, err := runEval(t, ts.URL, "evaluation-claim-job", "--sample", "s", "--candidate", "c", "--request-id", "r", "--ttl-ms", "1000")
			if err == nil {
				t.Fatal("expected an error")
			}
			var sched *schedulingError
			var confl *conflictError
			if errors.As(err, &sched) != tc.wantSched || errors.As(err, &confl) != tc.wantConfl {
				t.Fatalf("error %T %v: scheduling=%v conflict=%v, want scheduling=%v conflict=%v",
					err, err, sched != nil, confl != nil, tc.wantSched, tc.wantConfl)
			}
			if tc.wantSched && sched.code != exitScheduling {
				t.Fatalf("scheduling exit code %d, want %d", sched.code, exitScheduling)
			}
			if tc.wantConfl && confl.code != exitConflict {
				t.Fatalf("conflict exit code %d, want %d", confl.code, exitConflict)
			}
			if !strings.Contains(err.Error(), tc.wantInText) {
				t.Fatalf("error %q should name %q", err, tc.wantInText)
			}
		})
	}
}

func TestEvaluationCLIDispatchedFromRun(t *testing.T) {
	ts, rec := fakeEvaluationServer(t, 200, `{"state":"active"}`)
	t.Setenv("ODONIAN_URL", ts.URL)
	t.Setenv("ODONIAN_TOKEN", "tok")
	if err := run([]string{"odonian", "evaluation-get-campaign-status", "--id", "c1"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if rec.path != "/evaluation/campaigns/c1/status" {
		t.Fatalf("run dispatched to %q", rec.path)
	}
}

// The whole lifecycle through the CLI against the real API and store.
func TestEvaluationCLILifecycleAgainstAPI(t *testing.T) {
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	s, err := store.Open(filepath.Join(t.TempDir(), "cli-eval.db"), []string{"haiku", "sonnet", "opus"}, store.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	srv := api.New(s, "tok", 5*time.Minute, 5, nil, nil, 999999, false, 500, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	call := func(verb string, args ...string) map[string]interface{} {
		t.Helper()
		out, err := runEval(t, ts.URL, verb, args...)
		if err != nil {
			t.Fatalf("%s %v: %v", verb, args, err)
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(out), &m); err != nil {
			t.Fatalf("%s output %q: %v", verb, out, err)
		}
		if strings.Count(strings.TrimRight(out, "\n"), "\n") != 0 {
			t.Fatalf("%s output is not one line: %q", verb, out)
		}
		return m
	}
	identity, _ := json.Marshal(evaluation.CandidateIdentity{
		AdapterName: "fake", AdapterVersion: "v1", ModelID: "m1", ModelRevision: evaluation.Unknown,
		RuntimeName: "test", RuntimeVersion: "v1", ReasoningSettings: evaluation.UnknownSettings(),
		GenerationSettings: evaluation.UnknownSettings(), PromptVersion: "v1", Tools: evaluation.UnknownNames(),
		Observers: evaluation.UnknownNames(), AccountPool: "pool1",
	})

	call("evaluation-pool-set", "--id", "pool1", "--concurrency-only", "--concurrent-limit", "2")
	call("evaluation-create-campaign", "--id", "c1", "--name", "n", "--projects", "proj1", "--models", "m1", "--cohort", "{}", "--cap", "2")
	call("evaluation-create-candidate", "--campaign", "c1", "--id", "v1", "--cap", "1", "--identity", string(identity))
	if _, err := s.CreateEvaluationSample(t.Context(), store.EvaluationSample{
		ID: "s1", CampaignID: "c1", ProjectID: "proj1", OriginalTaskID: "t1", OriginalReviewRound: 1,
		SubmittedSHA: "abc", PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
	}); err != nil {
		t.Fatal(err)
	}
	if got := call("evaluation-list-candidates", "--campaign", "c1")["candidates"].([]interface{}); len(got) != 1 {
		t.Fatalf("candidates: %v", got)
	}
	if got := call("evaluation-get-sample", "--campaign", "c1", "--sample", "s1")["sample"].(map[string]interface{}); got["submitted_sha"] != "abc" {
		t.Fatalf("sample: %v", got)
	}

	claim := call("evaluation-claim-job", "--sample", "s1", "--candidate", "v1", "--request-id", "r1", "--ttl-ms", "60000")
	jobID := claim["job"].(map[string]interface{})["id"].(string)
	attemptID := claim["attempt"].(map[string]interface{})["id"].(string)
	replay := call("evaluation-claim-job", "--sample", "s1", "--candidate", "v1", "--request-id", "r1", "--ttl-ms", "60000")
	if replay["attempt"].(map[string]interface{})["id"] != attemptID {
		t.Fatalf("replayed claim returned a different attempt")
	}
	call("evaluation-renew-attempt", "--job", jobID, "--attempt", attemptID, "--ttl-ms", "120000")

	// The candidate's cap of 1 is spent by the claim, so a second sample is refused.
	if _, err := s.CreateEvaluationSample(t.Context(), store.EvaluationSample{
		ID: "s2", CampaignID: "c1", ProjectID: "proj1", OriginalTaskID: "t2", OriginalReviewRound: 1,
		SubmittedSHA: "def", PromptVersion: "v1", ModelVersion: "v1", RuntimeVersion: "v1",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = runEval(t, ts.URL, "evaluation-claim-job", "--sample", "s2", "--candidate", "v1", "--request-id", "r2", "--ttl-ms", "1000")
	var confl *conflictError
	if !errors.As(err, &confl) || !strings.Contains(err.Error(), "CAPACITY_EXHAUSTED") {
		t.Fatalf("exhausted claim: %v", err)
	}

	call("evaluation-finalize-attempt", "--job", jobID, "--attempt", attemptID, "--exit-class", "completed",
		"--result", `{"status":"completed","findings":[{"id":"f1","severity":"minor","summary":"nit"}]}`)
	_, err = runEval(t, ts.URL, "evaluation-finalize-attempt", "--job", jobID, "--attempt", attemptID, "--exit-class", "completed")
	if !errors.As(err, &confl) || !strings.Contains(err.Error(), "ATTEMPT_FINALIZED") {
		t.Fatalf("repeated finalize: %v", err)
	}
	if fs, _ := s.ListEvaluationFindings(t.Context(), attemptID); len(fs) != 1 {
		t.Fatalf("findings after replay: %v", fs)
	}

	st := call("evaluation-get-campaign-status", "--id", "c1")
	if st["attempts_used"].(float64) != 1 || st["state"] != "exhausted" {
		t.Fatalf("status: %v", st)
	}
	paused := call("evaluation-pause-campaign", "--id", "c1")
	if paused["state"] != "paused" {
		t.Fatalf("pause: %v", paused)
	}
	_, err = runEval(t, ts.URL, "evaluation-pause-campaign", "--id", "c1")
	if !errors.As(err, &confl) || !strings.Contains(err.Error(), "ALREADY_PAUSED") {
		t.Fatalf("second pause: %v", err)
	}
	if got := call("evaluation-get-campaign-status", "--id", "c1")["state"]; got != "paused" {
		t.Fatalf("state after pause: %v", got)
	}
}
