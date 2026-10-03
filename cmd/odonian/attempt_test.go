package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
