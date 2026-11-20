package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func checksResp(state, rollup string, hasNext bool, cursor string, nodes string) string {
	merged := "false"
	if state == "MERGED" {
		merged = "true"
	}
	rollupJSON := "null"
	if rollup != "" {
		rollupJSON = `{"state":"` + rollup + `","contexts":{"pageInfo":{"hasNextPage":` +
			map[bool]string{true: "true", false: "false"}[hasNext] + `,"endCursor":"` + cursor + `"},"nodes":[` + nodes + `]}}`
	}
	return `{"data":{"repository":{"pullRequest":{"state":"` + state + `","merged":` + merged +
		`,"headRefOid":"abc123","commits":{"nodes":[{"commit":{"oid":"abc123","statusCheckRollup":` + rollupJSON + `}}]}}}}}`
}

const (
	runOK      = `{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"http://x/1"}`
	runPending = `{"__typename":"CheckRun","name":"test","status":"IN_PROGRESS","conclusion":null,"detailsUrl":"http://x/2"}`
	runFailed  = `{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"http://x/3"}`
	statusCtx  = `{"__typename":"StatusContext","context":"ci/legacy","state":"PENDING","targetUrl":"http://x/4"}`
)

func serveChecks(t *testing.T, bodies ...string) (*int, func()) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(b, &req)
		if calls > 0 && req.Variables["after"] != "cur1" {
			t.Errorf("page %d: after = %v", calls, req.Variables["after"])
		}
		i := calls
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		calls++
		w.Write([]byte(bodies[i]))
	}))
	old := GitHubBaseURL
	GitHubBaseURL = srv.URL
	return &calls, func() { GitHubBaseURL = old; srv.Close() }
}

func TestGetPRChecks(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		state     string
		rollup    string
		contexts  int
		wantKinds []string
	}{
		{"green", checksResp("OPEN", "SUCCESS", false, "", runOK), "open", "SUCCESS", 1, nil},
		{"pending", checksResp("OPEN", "PENDING", false, "", runOK+","+runPending), "open", "PENDING", 2, nil},
		{"failed with pending", checksResp("OPEN", "FAILURE", false, "", runFailed+","+runPending), "open", "FAILURE", 2, nil},
		{"no checks", checksResp("OPEN", "", false, "", ""), "open", "", 0, nil},
		{"commit status", checksResp("OPEN", "PENDING", false, "", runOK+","+statusCtx), "open", "PENDING", 2, []string{"check_run", "status"}},
		{"merged", checksResp("MERGED", "SUCCESS", false, "", runOK), "merged", "SUCCESS", 1, nil},
		{"closed", checksResp("CLOSED", "SUCCESS", false, "", runOK), "closed", "SUCCESS", 1, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls, done := serveChecks(t, tt.body)
			defer done()
			got, err := GetPRChecks(context.Background(), "o", "r", 1, "tok")
			if err != nil {
				t.Fatal(err)
			}
			if *calls != 1 {
				t.Errorf("requests = %d, want 1", *calls)
			}
			if got.State != tt.state || got.RollupState != tt.rollup || len(got.Contexts) != tt.contexts || got.HeadSHA != "abc123" {
				t.Errorf("got %+v", got)
			}
			for i, k := range tt.wantKinds {
				if got.Contexts[i].Kind != k {
					t.Errorf("context %d kind = %s, want %s", i, got.Contexts[i].Kind, k)
				}
			}
		})
	}
}

func TestGetPRChecksFields(t *testing.T) {
	_, done := serveChecks(t, checksResp("OPEN", "FAILURE", false, "", runFailed+","+statusCtx))
	defer done()
	got, err := GetPRChecks(context.Background(), "o", "r", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	want := CheckContext{Kind: "check_run", Name: "lint", Status: "COMPLETED", Conclusion: "FAILURE", URL: "http://x/3"}
	if got.Contexts[0] != want {
		t.Errorf("got %+v, want %+v", got.Contexts[0], want)
	}
	want = CheckContext{Kind: "status", Name: "ci/legacy", Conclusion: "PENDING", URL: "http://x/4"}
	if got.Contexts[1] != want {
		t.Errorf("got %+v, want %+v", got.Contexts[1], want)
	}
}

func TestGetPRChecksSecondPage(t *testing.T) {
	calls, done := serveChecks(t,
		checksResp("OPEN", "PENDING", true, "cur1", runOK),
		checksResp("OPEN", "PENDING", false, "", runPending))
	defer done()
	got, err := GetPRChecks(context.Background(), "o", "r", 1, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || len(got.Contexts) != 2 {
		t.Errorf("calls=%d contexts=%d", *calls, len(got.Contexts))
	}
}

func TestGetPRChecksRateLimited(t *testing.T) {
	for name, handler := range map[string]http.HandlerFunc{
		"403": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-RateLimit-Reset", "1700000000")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"API rate limit exceeded"}`))
		},
		"graphql error": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded"}]}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(handler)
			defer srv.Close()
			old := GitHubBaseURL
			GitHubBaseURL = srv.URL
			defer func() { GitHubBaseURL = old }()
			_, err := GetPRChecks(context.Background(), "o", "r", 1, "secret-token")
			var rle *RateLimitError
			if !errors.As(err, &rle) {
				t.Fatalf("err = %v, want *RateLimitError", err)
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Error("error leaks token")
			}
		})
	}
}
