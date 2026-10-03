package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/api"
	"github.com/boldfield/odonian/internal/policy"
	"github.com/boldfield/odonian/internal/store"
	"github.com/boldfield/odonian/internal/tuiclient"
)

func TestAttemptCarriedFromClaimToHeartbeatAndSubmit(t *testing.T) {
	t.Setenv("ODONIAN_STATE_DIR", t.TempDir())
	var heartbeatBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/tasks/task-1/claim":
			w.Write([]byte(`{"id":"task-1","research_admission":{"attempt_id":"att-1"}}`))
		case "/tasks/task-1/heartbeat":
			b := make([]byte, 512)
			n, _ := r.Body.Read(b)
			heartbeatBody = string(b[:n])
			w.Write([]byte(`{"id":"task-1"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(ts.Close)

	claimer := tuiclient.NewHTTPClient(ts.URL, "tok")
	if err := claimer.ClaimTask(t.Context(), "task-1", "agent", "opus"); err != nil {
		t.Fatal(err)
	}
	saveAttempt(claimer, "task-1")

	// A separate process: nothing in memory, only the saved file.
	later := tuiclient.NewHTTPClient(ts.URL, "tok")
	loadAttempt(later, "task-1", "")
	if got := later.AttemptID("task-1"); got != "att-1" {
		t.Fatalf("loaded attempt = %q, want att-1", got)
	}
	if err := later.HeartbeatTask(t.Context(), "task-1", "agent"); err != nil {
		t.Fatal(err)
	}
	if want := `"attempt_id":"att-1"`; !strings.Contains(heartbeatBody, want) {
		t.Fatalf("heartbeat body %q lacks %s", heartbeatBody, want)
	}

	override := tuiclient.NewHTTPClient(ts.URL, "tok")
	loadAttempt(override, "task-1", "att-2")
	if got := override.AttemptID("task-1"); got != "att-2" {
		t.Fatalf("override = %q", got)
	}

	clearAttempt("task-1")
	cleared := tuiclient.NewHTTPClient(ts.URL, "tok")
	loadAttempt(cleared, "task-1", "")
	if got := cleared.AttemptID("task-1"); got != "" {
		t.Fatalf("attempt after clear = %q", got)
	}
}

func TestSaveAttemptClearsStaleAndRejectsUnsafeIDs(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ODONIAN_STATE_DIR", dir)
	c := tuiclient.NewHTTPClient("http://unused", "tok")
	c.SetAttemptID("task-1", "att-1")
	saveAttempt(c, "task-1")
	// A later claim of a non-research task has no attempt and must drop the old file.
	c.SetAttemptID("task-1", "")
	saveAttempt(c, "task-1")
	if p, _ := attemptPath("task-1"); fileExists(p) {
		t.Fatalf("stale attempt file survived")
	}
	if _, ok := attemptPath("../escape"); ok {
		t.Fatalf("path traversal task ID accepted")
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func newResearchServer(t *testing.T) (url string, advance func(time.Duration), taskID string) {
	t.Helper()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	models := []string{"haiku", "sonnet", "opus"}
	s, err := store.Open(filepath.Join(t.TempDir(), "r.db"), models, store.WithClock(clock))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	cfg := policy.Config{Mode: policy.ModeEnforce, AllowedModels: models, Pools: []policy.Pool{{
		Name: "pool", AccountID: "acct", Models: models,
		StartRate: 0.0001, BurstCapacity: 5, ConcurrentDispatchLimit: 5,
	}}}
	if err := s.SetResearchPolicy(t.Context(), clock(), cfg); err != nil {
		t.Fatal(err)
	}
	proj, err := s.CreateProject(t.Context(), "p", "https://github.com/test/repo")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(t.Context(), proj.ID, "feature_spec", "doc", "doc.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.CreateTasks(t.Context(), proj.ID, []store.TaskInput{{
		Title: "t", Spec: "spec", DocumentID: doc.ID, Model: "opus", Track: "research",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PromoteTask(t.Context(), tasks[0].ID); err != nil {
		t.Fatal(err)
	}
	srv := api.New(s, "tok", 5*time.Minute, 5, nil, nil, 999999, false, 500, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }, tasks[0].ID
}

// Each worker session is its own process environment sharing one state dir, as two
// sessions of the same agent on one host would.
func TestCLIFencesStaleSameAgentSessionAfterReclaim(t *testing.T) {
	t.Setenv("ODONIAN_STATE_DIR", t.TempDir())
	t.Setenv("ODONIAN_SESSION_ID", "")
	url, advance, id := newResearchServer(t)
	ctx := t.Context()
	agent := []string{"--agent", "agent", "--model", "opus"}
	prArgs := []string{"--agent", "agent", "--pr", "https://github.com/test/repo/pull/1", "--branch", "mr/x"}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-old")
	if err := executeClaim(ctx, url, "tok", append([]string{id}, agent...)); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	advance(6 * time.Minute)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-new")
	if err := executeClaim(ctx, url, "tok", append([]string{id}, agent...)); err != nil {
		t.Fatalf("reclaim: %v", err)
	}

	fenced := func(err error) bool {
		var apiErr *tuiclient.APIError
		return errors.As(err, &apiErr) && apiErr.Code == "ATTEMPT_FENCED"
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-old")
	if err := executeHeartbeat(ctx, url, "tok", []string{id, "--agent", "agent"}); !fenced(err) {
		t.Fatalf("stale heartbeat = %v, want ATTEMPT_FENCED", err)
	}
	if err := executeSubmit(ctx, url, "tok", append([]string{id, "--result", "STALE"}, prArgs...)); !fenced(err) {
		t.Fatalf("stale submit = %v, want ATTEMPT_FENCED", err)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-new")
	if err := executeHeartbeat(ctx, url, "tok", []string{id, "--agent", "agent"}); err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if err := executeSubmit(ctx, url, "tok", append([]string{id, "--result", "real"}, prArgs...)); err != nil {
		t.Fatalf("current submit: %v", err)
	}
}

func TestAttemptFileIsPerSession(t *testing.T) {
	t.Setenv("ODONIAN_STATE_DIR", t.TempDir())
	t.Setenv("ODONIAN_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s1")
	p1, _ := attemptPath("task-1")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "s2")
	p2, _ := attemptPath("task-1")
	t.Setenv("ODONIAN_SESSION_ID", "explicit")
	p3, _ := attemptPath("task-1")
	if p1 == p2 || p1 == p3 || p2 == p3 {
		t.Fatalf("sessions share an attempt file: %s %s %s", p1, p2, p3)
	}
}
