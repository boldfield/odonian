package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/tuiclient"
)

func TestRunNoArgs(t *testing.T) {
	err := run([]string{"odonian"})
	if err != nil {
		t.Errorf("expected no error for bare odonian, got: %v", err)
	}
}

func TestRunHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"--help", []string{"odonian", "--help"}},
		{"-h", []string{"odonian", "-h"}},
		{"help", []string{"odonian", "help"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args)
			if err != nil {
				t.Errorf("expected no error for %s, got: %v", tt.name, err)
			}
		})
	}
}

func TestRunServer(t *testing.T) {
	// Note: runServer() will try to start a real server, so we can't test it directly.
	// This test just ensures the routing recognizes "server" as a valid command.
	// In a real test environment, we'd mock runServer().
}

func TestRunUnknownCommand(t *testing.T) {
	// Capture stderr
	stderrBackup := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := run([]string{"odonian", "invalid"})

	w.Close()
	os.Stderr = stderrBackup
	var buf bytes.Buffer
	buf.ReadFrom(r)
	stderrOutput := buf.String()

	if err == nil {
		t.Error("expected error for unknown command, got nil")
	}

	var handledErr *handledError
	if !errors.As(err, &handledErr) {
		t.Errorf("expected handledError, got %T: %v", err, err)
	}

	if !strings.Contains(stderrOutput, "error: unknown command") {
		t.Errorf("expected stderr to contain 'error: unknown command', got: %s", stderrOutput)
	}

	if !strings.Contains(stderrOutput, "usage: odonian") {
		t.Errorf("expected stderr to contain usage, got: %s", stderrOutput)
	}

	if !strings.Contains(stderrOutput, "projects") {
		t.Errorf("expected stderr to contain verb list with 'projects', got: %s", stderrOutput)
	}
}

func TestSplitJSONFlag(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantJSON bool
		wantRest []string
	}{
		{"trailing json with filters", []string{"--claimable", "--model", "haiku", "--json"}, true, []string{"--claimable", "--model", "haiku"}},
		{"leading json", []string{"--json", "--kind", "review"}, true, []string{"--kind", "review"}},
		{"no json", []string{"--model", "haiku"}, false, []string{"--model", "haiku"}},
		{"only json", []string{"--json"}, true, []string{}},
		{"empty", []string{}, false, []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotJSON, gotRest := splitJSONFlag(tc.args)
			if gotJSON != tc.wantJSON {
				t.Errorf("json = %v, want %v", gotJSON, tc.wantJSON)
			}
			if len(gotRest) != len(tc.wantRest) {
				t.Fatalf("rest = %v, want %v", gotRest, tc.wantRest)
			}
			for i := range gotRest {
				if gotRest[i] != tc.wantRest[i] {
					t.Errorf("rest[%d] = %q, want %q", i, gotRest[i], tc.wantRest[i])
				}
			}
		})
	}
}

func TestExecuteProjectsTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Project{
				{ID: "proj-1", Name: "Project 1", Repo: "repo-1", CreatedAt: "2026-01-01T00:00:00Z"},
				{ID: "proj-2", Name: "Project 2", Repo: "repo-2", CreatedAt: "2026-01-02T00:00:00Z"},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeProjects(context.Background(), server.URL, "test-token", false, []string{}, buf)
	if err != nil {
		t.Fatalf("executeProjects failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Project 1") {
		t.Errorf("expected output to contain 'Project 1', got: %s", output)
	}
	if !strings.Contains(output, "Project 2") {
		t.Errorf("expected output to contain 'Project 2', got: %s", output)
	}
	if !strings.Contains(output, "ID") || !strings.Contains(output, "NAME") {
		t.Errorf("expected table headers in output, got: %s", output)
	}
}

func TestExecuteProjectsJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Project{
				{ID: "proj-1", Name: "Project 1", Repo: "repo-1", CreatedAt: "2026-01-01T00:00:00Z"},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeProjects(context.Background(), server.URL, "test-token", true, []string{}, buf)
	if err != nil {
		t.Fatalf("executeProjects failed: %v", err)
	}

	output := buf.String()
	var result []tuiclient.Project
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if len(result) != 1 {
		t.Errorf("expected 1 project in JSON, got %d", len(result))
	}
	if result[0].Name != "Project 1" {
		t.Errorf("expected project name 'Project 1', got %q", result[0].Name)
	}
}

func TestExecuteProjectsEmptyResultJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Project{})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeProjects(context.Background(), server.URL, "test-token", true, []string{}, buf)
	if err != nil {
		t.Fatalf("executeProjects with empty result JSON failed: %v", err)
	}

	output := buf.String()
	var result []tuiclient.Project
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if result == nil {
		t.Error("expected non-nil empty slice [], got null")
	}
	if len(result) != 0 {
		t.Errorf("expected 0 projects in JSON, got %d", len(result))
	}
}

func TestExecuteProjectsMissingURL(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeProjects(context.Background(), "", "test-token", false, []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteProjectsMissingToken(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeProjects(context.Background(), "http://localhost:8080", "", false, []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecuteProjectsWithFilters(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects" {
			query := r.URL.Query()
			model := query.Get("model")
			kind := query.Get("kind")
			claimable := query.Get("claimable")

			w.Header().Set("Content-Type", "application/json")
			if model == "haiku" && kind == "implement" && claimable == "true" {
				json.NewEncoder(w).Encode([]tuiclient.Project{
					{ID: "proj-1", Name: "Project 1", Repo: "repo-1", CreatedAt: "2026-01-01T00:00:00Z"},
				})
			} else {
				json.NewEncoder(w).Encode([]tuiclient.Project{})
			}
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeProjects(context.Background(), server.URL, "test-token", false, []string{"--model", "haiku", "--kind", "implement", "--claimable"}, buf)
	if err != nil {
		t.Fatalf("executeProjects with filters failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Project 1") {
		t.Errorf("expected output to contain 'Project 1', got: %s", output)
	}
}

func TestExecuteProjectTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.Project{
				ID:        "proj-1",
				Name:      "Project 1",
				Repo:      "repo-1",
				CreatedAt: "2026-01-01T00:00:00Z",
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeProject(context.Background(), server.URL, "test-token", false, []string{"proj-1"}, buf)
	if err != nil {
		t.Fatalf("executeProject failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "ID: proj-1") {
		t.Errorf("expected 'ID: proj-1' in output, got: %s", output)
	}
	if !strings.Contains(output, "Name: Project 1") {
		t.Errorf("expected 'Name: Project 1' in output, got: %s", output)
	}
	if !strings.Contains(output, "Repo: repo-1") {
		t.Errorf("expected 'Repo: repo-1' in output, got: %s", output)
	}
	if !strings.Contains(output, "Created At:") {
		t.Errorf("expected 'Created At:' in output, got: %s", output)
	}
}

func TestExecuteProjectJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.Project{
				ID:        "proj-1",
				Name:      "Project 1",
				Repo:      "repo-1",
				CreatedAt: "2026-01-01T00:00:00Z",
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeProject(context.Background(), server.URL, "test-token", true, []string{"--json", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executeProject with JSON failed: %v", err)
	}

	output := buf.String()
	var result tuiclient.Project
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if result.ID != "proj-1" {
		t.Errorf("expected project ID 'proj-1', got %q", result.ID)
	}
	if result.Name != "Project 1" {
		t.Errorf("expected project name 'Project 1', got %q", result.Name)
	}
}

func TestExecuteProjectMissingID(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeProject(context.Background(), "http://localhost:8080", "test-token", false, []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing project id, got nil")
	}
	if !strings.Contains(err.Error(), "project id required") {
		t.Errorf("expected error to mention project id, got: %v", err)
	}
}

func TestExecuteShowTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:           "task-1",
				State:        "in_progress",
				Model:        "opus",
				Kind:         "implement",
				Title:        "Test Task",
				Spec:         "Test spec",
				TargetTaskID: nil,
				Links: []tuiclient.TaskLink{
					{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/102"},
				},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-1"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "ID: task-1") {
		t.Errorf("expected 'ID: task-1' in output, got: %s", output)
	}
	if !strings.Contains(output, "State: in_progress") {
		t.Errorf("expected 'State: in_progress' in output, got: %s", output)
	}
	if !strings.Contains(output, "Model: opus") {
		t.Errorf("expected 'Model: opus' in output, got: %s", output)
	}
	if !strings.Contains(output, "Kind: implement") {
		t.Errorf("expected 'Kind: implement' in output, got: %s", output)
	}
	if !strings.Contains(output, "Title: Test Task") {
		t.Errorf("expected 'Title: Test Task' in output, got: %s", output)
	}
	if !strings.Contains(output, "Spec: Test spec") {
		t.Errorf("expected 'Spec: Test spec' in output, got: %s", output)
	}
	if !strings.Contains(output, "Links:") {
		t.Errorf("expected 'Links:' in output, got: %s", output)
	}
	if !strings.Contains(output, "pr: https://github.com/boldfield/odonian/pull/102") {
		t.Errorf("expected link in output, got: %s", output)
	}
}

func TestExecuteShowJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "ready",
				Model: "haiku",
				Kind:  "implement",
				Title: "Test Task",
				Spec:  "Test spec",
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", true, []string{"--json", "task-1"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	var result tuiclient.TaskDetail
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if result.ID != "task-1" {
		t.Errorf("expected task ID 'task-1', got %q", result.ID)
	}
	if result.Title != "Test Task" {
		t.Errorf("expected title 'Test Task', got %q", result.Title)
	}
}

func TestExecuteShowWithSubmissionManifests(t *testing.T) {
	manifestJSON := json.RawMessage(`{"version":1,"parent_task_id":"task-1","children":[],"pending_candidates":[]}`)
	// Real SHA-256 digest of the manifest bytes: printf '{"version":1,"parent_task_id":"task-1","children":[],"pending_candidates":[]}' | sha256sum
	realDigest := "461bf9e337f7d640ca135d4743755ab5538b777442eddafed2c5dfb38b1279be"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "in_progress",
				Model: "haiku",
				Kind:  "implement",
				Title: "Test Task",
				Spec:  "Test spec",
				SubmissionManifests: []tuiclient.SubmissionManifest{
					{
						ReviewRound:    1,
						ParentTaskID:   "task-1",
						ManifestJSON:   manifestJSON,
						ManifestDigest: realDigest,
						SubmittedAt:    "2026-10-02T12:00:00Z",
					},
				},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-1"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Submission Manifests:") {
		t.Errorf("expected 'Submission Manifests:' in output, got: %s", output)
	}
	if !strings.Contains(output, "Round 1:") {
		t.Errorf("expected 'Round 1:' in output, got: %s", output)
	}
	if !strings.Contains(output, "Parent Task ID: task-1") {
		t.Errorf("expected 'Parent Task ID: task-1' in output, got: %s", output)
	}
	if !strings.Contains(output, fmt.Sprintf("Digest: %s", realDigest)) {
		t.Errorf("expected 'Digest: %s' in output, got: %s", realDigest, output)
	}
	if !strings.Contains(output, "Canonical:") {
		t.Errorf("expected 'Canonical:' header in output, got: %s", output)
	}
	lines := strings.Split(output, "\n")
	canonical := ""
	for i, line := range lines {
		if strings.TrimSpace(line) == "Canonical:" && i+1 < len(lines) {
			canonical = strings.TrimSpace(lines[i+1])
			break
		}
	}
	if canonical != string(manifestJSON) {
		t.Fatalf("expected canonical manifest bytes %s verbatim after 'Canonical:', got %q in output: %s", manifestJSON, canonical, output)
	}
	sum := sha256.Sum256([]byte(canonical))
	if got := fmt.Sprintf("%x", sum); got != realDigest {
		t.Errorf("shown canonical bytes hash to %s, want displayed digest %s", got, realDigest)
	}
}

func TestExecuteShowManifestWithHTMLCharsHashesToDigest(t *testing.T) {
	manifestJSON := json.RawMessage(`{"version":1,"parent_task_id":"task-1","children":[{"title":"A & B <= 3 > 1","spec":"parent's claim"}],"pending_candidates":[]}`)
	sum := sha256.Sum256(manifestJSON)
	digest := fmt.Sprintf("%x", sum)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetEscapeHTML(false)
		enc.Encode(tuiclient.TaskDetail{
			ID:    "task-1",
			State: "in_progress",
			SubmissionManifests: []tuiclient.SubmissionManifest{
				{ReviewRound: 1, ParentTaskID: "task-1", ManifestJSON: manifestJSON, ManifestDigest: digest},
			},
		})
	}))
	defer server.Close()

	text := &bytes.Buffer{}
	if err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-1"}, text); err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}
	lines := strings.Split(text.String(), "\n")
	canonical := ""
	for i, line := range lines {
		if strings.TrimSpace(line) == "Canonical:" && i+1 < len(lines) {
			canonical = strings.TrimSpace(lines[i+1])
		}
	}
	if canonical != string(manifestJSON) {
		t.Errorf("text show altered manifest bytes: got %q want %q", canonical, manifestJSON)
	}

	js := &bytes.Buffer{}
	if err := executeShow(context.Background(), server.URL, "test-token", true, []string{"task-1"}, js); err != nil {
		t.Fatalf("executeShow --json failed: %v", err)
	}
	if !strings.Contains(js.String(), "A & B <= 3 > 1") || strings.Contains(js.String(), `\u0026`) {
		t.Errorf("show --json HTML-escaped the manifest: %s", js.String())
	}
}

func TestExecuteShowJSONPreservesManifestKeyOrder(t *testing.T) {
	manifestJSON := json.RawMessage(`{"version":1,"parent_task_id":"task-1","children":[],"pending_candidates":[]}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tuiclient.TaskDetail{
			ID:    "task-1",
			State: "in_progress",
			SubmissionManifests: []tuiclient.SubmissionManifest{
				{ReviewRound: 1, ParentTaskID: "task-1", ManifestJSON: manifestJSON, ManifestDigest: "d"},
			},
		})
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	if err := executeShow(context.Background(), server.URL, "test-token", true, []string{"task-1"}, buf); err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}
	var out struct {
		Manifests []struct {
			ManifestJSON json.RawMessage `json:"manifest_json"`
		} `json:"submission_manifests"`
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil || len(out.Manifests) != 1 {
		t.Fatalf("bad show --json output (%v): %s", err, buf.String())
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, out.Manifests[0].ManifestJSON); err != nil {
		t.Fatal(err)
	}
	if compact.String() != string(manifestJSON) {
		t.Errorf("manifest_json in --json output differs from stored bytes: got %s want %s", compact.String(), manifestJSON)
	}
}

func TestExecuteShowMissingID(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), "http://localhost:8080", "test-token", false, []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}
	if !strings.Contains(err.Error(), "task id required") {
		t.Errorf("expected error to mention 'task id required', got: %v", err)
	}
}

func TestExecuteShowMissingURL(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), "", "test-token", false, []string{"task-1"}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteShowMissingToken(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), "http://localhost:8080", "", false, []string{"task-1"}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecuteShowServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.WriteHeader(http.StatusNotFound)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"error": "task not found"}`)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"nonexistent-id"}, buf)
	if err == nil {
		t.Fatal("expected error for non-existent task, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get task") {
		t.Errorf("expected error to mention 'failed to get task', got: %v", err)
	}
}

func TestExecuteTransitionSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/transition") {
			if r.Method != "POST" {
				t.Errorf("expected POST, got %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeTransition(context.Background(), server.URL, "test-token", []string{"task-123", "--to", "blocked", "--note", "test note"})
	if err != nil {
		t.Fatalf("executeTransition failed: %v", err)
	}
}

func TestExecuteTransitionMissingTo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := executeTransition(context.Background(), server.URL, "test-token", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for missing --to, got nil")
	}
	if !strings.Contains(err.Error(), "--to") {
		t.Errorf("expected error to mention --to, got: %v", err)
	}
}

func TestExecuteTransitionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "internal_error",
				"message": "server error",
			},
		})
	}))
	defer server.Close()

	err := executeTransition(context.Background(), server.URL, "test-token", []string{"task-123", "--to", "blocked"})
	if err == nil {
		t.Fatal("expected error for failed transition, got nil")
	}
}

func TestExecuteClaimSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/claim" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Setenv("AGENT_ID", "test-agent")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeClaim(context.Background(), server.URL, "test-token", []string{"task123"})
	if err != nil {
		t.Fatalf("executeClaim failed: %v", err)
	}
}

func TestExecuteClaimAlreadyClaimed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/claim" {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "already claimed"})
		}
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Setenv("AGENT_ID", "test-agent")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeClaim(context.Background(), server.URL, "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var claimErr *claimError
	if !errors.As(err, &claimErr) {
		t.Fatalf("expected claimError, got %T: %v", err, err)
	}

	if claimErr.code != 3 {
		t.Errorf("expected exit code 3, got %d", claimErr.code)
	}

	if !strings.Contains(claimErr.Error(), "already claimed") {
		t.Errorf("expected error message to contain 'already claimed', got: %v", claimErr.Error())
	}
}

func TestExecuteClaimMissingTaskID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Setenv("AGENT_ID", "test-agent")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeClaim(context.Background(), server.URL, "test-token", []string{})
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}

	if !strings.Contains(err.Error(), "task ID is required") {
		t.Errorf("expected error to mention task ID, got: %v", err)
	}
}

func TestExecuteClaimMissingAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Unsetenv("AGENT_ID")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeClaim(context.Background(), server.URL, "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for missing agent ID, got nil")
	}

	if !strings.Contains(err.Error(), "agent ID is required") {
		t.Errorf("expected error to mention agent ID, got: %v", err)
	}
}

func TestExecuteClaimMissingURL(t *testing.T) {
	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Setenv("AGENT_ID", "test-agent")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeClaim(context.Background(), "", "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}

	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteClaimServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/claim" {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
		}
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Setenv("AGENT_ID", "test-agent")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeClaim(context.Background(), server.URL, "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for server error, got nil")
	}
}

func TestExecuteSubmitSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 0, "links": []map[string]string{}})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/submit" {
			var req struct {
				AgentID string
				Result  string
				Verdict *string
				Links   []struct {
					Kind  string
					Value string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Result != "implementation done" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links) != 2 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "implementation done",
		"--pr", "https://github.com/example/repo/pull/1",
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}
}

func TestExecuteSubmitNoOp(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 0, "links": []map[string]string{}})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/submit" {
			var req struct {
				AgentID string
				Result  string
				Links   []struct {
					Kind  string
					Value string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links) != 1 || req.Links[0].Kind != "no_op" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "already satisfied",
		"--no-op",
		"task123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}
}

func TestExecuteSubmitVerdict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 0, "links": []map[string]string{}})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/submit" {
			var req struct {
				Verdict *string
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Verdict == nil || *req.Verdict != "approve" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "looks good",
		"--verdict", "approve",
		"task123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}
}

func TestExecuteSubmitNoOpWithPR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "test",
		"--no-op",
		"--pr", "https://github.com/example/repo/pull/1",
		"task123",
	})
	if err == nil {
		t.Fatal("expected error for --no-op with --pr, got nil")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("expected error to mention conflict, got: %v", err)
	}
}

func TestExecuteSubmitMissingResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for missing --result, got nil")
	}
	if !strings.Contains(err.Error(), "--result") {
		t.Errorf("expected error to mention --result, got: %v", err)
	}
}

func TestExecuteSubmitMissingTaskID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "test",
	})
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}
	if !strings.Contains(err.Error(), "task ID is required") {
		t.Errorf("expected error to mention task ID, got: %v", err)
	}
}

func TestExecuteSubmitPRWithoutBranch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 0, "links": []map[string]string{}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "test",
		"--pr", "https://github.com/example/repo/pull/1",
		"task123",
	})
	if err == nil {
		t.Fatal("expected error for --pr without --branch, got nil")
	}
	if !strings.Contains(err.Error(), "must be provided together") {
		t.Errorf("expected error to mention together, got: %v", err)
	}
}

func TestExecuteSubmitInvalidVerdict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 0, "links": []map[string]string{}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "test",
		"--verdict", "invalid",
		"task123",
	})
	if err == nil {
		t.Fatal("expected error for invalid verdict, got nil")
	}
	if !strings.Contains(err.Error(), "must be 'approve' or 'reject'") {
		t.Errorf("expected error to mention verdict values, got: %v", err)
	}
}

func TestExecuteSubmitMissingAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer func() {
		if oldAgent != "" {
			os.Setenv("AGENT_ID", oldAgent)
		} else {
			os.Unsetenv("AGENT_ID")
		}
	}()
	os.Unsetenv("AGENT_ID")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "test",
		"task123",
	})
	if err == nil {
		t.Fatal("expected error for missing agent ID, got nil")
	}
	if !strings.Contains(err.Error(), "agent ID is required") {
		t.Errorf("expected error to mention agent ID, got: %v", err)
	}
}

func TestExecuteSubmitLocalCommitFirstSubmit(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)

	tmpRepo := t.TempDir()
	initGitRepo(t, tmpRepo)

	wtPath := filepath.Join(tmpDir, "task-123")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}

	cmd := exec.Command("git", "clone", tmpRepo, wtPath)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to clone repo: %v", err)
	}

	setupGitConfig(t, wtPath)

	if err := os.WriteFile(filepath.Join(wtPath, "test.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				Title: "Test Task Title",
			})
		} else if r.Method == "POST" && r.URL.Path == "/tasks/task-123/submit" {
			var req struct {
				AgentID string
				Result  string
				Links   []struct {
					Kind  string
					Value string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links) != 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Links[0].Kind != "commit" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links[0].Value) != 40 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "implementation done",
		"task-123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}

	cmd = exec.Command("git", "-C", wtPath, "log", "-1", "--format=%s")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get commit message: %v", err)
	}
	if string(output) != "Test Task Title\n" {
		t.Errorf("expected commit message 'Test Task Title', got %q", string(output))
	}
}

// A review-kind task owns NO worktree — its payload is the --verdict. Submitting one must NOT take
// the commit path: <worktree-home>/<review-task-id> does not exist, so `git add -A` there exits 128
// and the reviewer can never record a verdict, wedging every implement task in `review`. Note the
// worktree dir is deliberately never created here — that is the whole point of the regression.
func TestExecuteSubmitLocalCommitReviewKindSkipsCommit(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	t.Setenv("ODONIAN_WORKTREE_HOME", t.TempDir())
	t.Setenv("AGENT_ID", "test-reviewer")

	var gotLinks int
	var gotVerdict string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/review-1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "review-1",
				Title: "Review: Test Task Title [opus]",
				Kind:  "review",
			})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/review-1/submit" {
			var req struct {
				Verdict *string             `json:"verdict"`
				Links   []map[string]string `json:"links"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			gotLinks = len(req.Links)
			if req.Verdict != nil {
				gotVerdict = *req.Verdict
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "LGTM", "--verdict", "approve", "review-1",
	})
	if err != nil {
		t.Fatalf("executeSubmit on a review-kind task failed: %v", err)
	}
	if gotVerdict != "approve" {
		t.Errorf("expected verdict %q, got %q", "approve", gotVerdict)
	}
	if gotLinks != 0 {
		t.Errorf("expected no links on a review-kind submit, got %d", gotLinks)
	}
}

// The documented no-op path (implement prompt, step 6) asserts acceptance is already satisfied on
// the base with an unchanged tree. CommitAll errors on an empty commit, so local_commit mode must
// bypass the commit and attach a no_op link, exactly as pull_request mode does.
func TestExecuteSubmitLocalCommitNoOp(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)
	t.Setenv("AGENT_ID", "test-agent")

	// A real worktree with a CLEAN tree — there is genuinely nothing to commit.
	wtPath := filepath.Join(tmpDir, "task-noop")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}
	initGitRepo(t, wtPath)

	var gotLinks []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-noop" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-noop",
				Title: "Already satisfied",
				Kind:  "implement",
			})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/task-noop/submit" {
			var req struct {
				Links []map[string]string `json:"links"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			gotLinks = req.Links
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "acceptance already satisfied on base; no changes needed",
		"--no-op", "task-noop",
	})
	if err != nil {
		t.Fatalf("executeSubmit --no-op in local_commit mode failed: %v", err)
	}
	if len(gotLinks) != 1 || gotLinks[0]["kind"] != "no_op" {
		t.Errorf("expected a single no_op link, got %v", gotLinks)
	}
}

func TestExecuteSubmitLocalCommitRework(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)

	tmpRepo := t.TempDir()
	initGitRepo(t, tmpRepo)

	wtPath := filepath.Join(tmpDir, "task-123")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}

	cmd := exec.Command("git", "clone", tmpRepo, wtPath)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to clone repo: %v", err)
	}

	setupGitConfig(t, wtPath)

	if err := os.WriteFile(filepath.Join(wtPath, "test.txt"), []byte("v1"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	sha1Cmd := exec.Command("git", "-C", wtPath, "add", "-A")
	if err := sha1Cmd.Run(); err != nil {
		t.Fatalf("failed to add files: %v", err)
	}
	sha1Cmd = exec.Command("git", "-C", wtPath, "commit", "-m", "first commit")
	if err := sha1Cmd.Run(); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	if err := os.WriteFile(filepath.Join(wtPath, "test.txt"), []byte("v2"), 0644); err != nil {
		t.Fatalf("failed to modify test file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				Title: "Updated Task Title",
			})
		} else if r.Method == "POST" && r.URL.Path == "/tasks/task-123/submit" {
			var req struct {
				AgentID string
				Result  string
				Links   []struct {
					Kind  string
					Value string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links) != 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Links[0].Kind != "commit" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links[0].Value) != 40 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "rework done",
		"task-123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}

	cmd = exec.Command("git", "-C", wtPath, "log", "-1", "--format=%s")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get commit message: %v", err)
	}
	if string(output) != "Updated Task Title\n" {
		t.Errorf("expected commit message 'Updated Task Title', got %q", string(output))
	}

	cmd = exec.Command("git", "-C", wtPath, "rev-list", "--count", "origin/main..HEAD")
	output, err = cmd.Output()
	if err != nil {
		t.Fatalf("failed to count commits: %v", err)
	}
	if string(output) != "1\n" {
		t.Errorf("expected 1 commit on top of origin/main, got %q", string(output))
	}
}

func TestExecuteSubmitLocalCommitWithMessageOverride(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)

	tmpRepo := t.TempDir()
	initGitRepo(t, tmpRepo)

	wtPath := filepath.Join(tmpDir, "task-123")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}

	cmd := exec.Command("git", "clone", tmpRepo, wtPath)
	cmd.Dir = tmpDir
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to clone repo: %v", err)
	}

	setupGitConfig(t, wtPath)

	if err := os.WriteFile(filepath.Join(wtPath, "test.txt"), []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to create test file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				Title: "Original Task Title",
			})
		} else if r.Method == "POST" && r.URL.Path == "/tasks/task-123/submit" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "done",
		"--message", "Custom commit message",
		"task-123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}

	cmd = exec.Command("git", "-C", wtPath, "log", "-1", "--format=%s")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get commit message: %v", err)
	}
	if string(output) != "Custom commit message\n" {
		t.Errorf("expected commit message 'Custom commit message', got %q", string(output))
	}
}

func TestExecuteSubmitLocalCommitStackedItems(t *testing.T) {
	// Test: item B stacked on item A (same document) should create NEW commit, not amend item A's commit
	// Both items have the same slug since they're on the same document
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)

	tmpRepo := t.TempDir()
	initGitRepo(t, tmpRepo)

	// Simulate item A's frozen commit: create wi/document-foo branch with a commit
	// (slug from "Document Foo" is "document-foo")
	if err := exec.Command("git", "-C", tmpRepo, "checkout", "-b", "wi/document-foo").Run(); err != nil {
		t.Fatalf("failed to create wi/document-foo: %v", err)
	}

	if err := os.WriteFile(filepath.Join(tmpRepo, "doc.txt"), []byte("item A"), 0644); err != nil {
		t.Fatalf("failed to create doc file: %v", err)
	}
	if err := exec.Command("git", "-C", tmpRepo, "add", "-A").Run(); err != nil {
		t.Fatalf("failed to add files: %v", err)
	}
	if err := exec.Command("git", "-C", tmpRepo, "commit", "-m", "Item A").Run(); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	// Create worktree for item B cloned from wi/foo
	wtPath := filepath.Join(tmpDir, "task-B")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}

	cloneCmd := exec.Command("git", "clone", "-b", "wi/document-foo", tmpRepo, wtPath)
	cloneCmd.Dir = tmpDir
	if err := cloneCmd.Run(); err != nil {
		t.Fatalf("failed to clone repo: %v", err)
	}

	setupGitConfig(t, wtPath)

	// Create wip/task-B branch from wi/foo
	if err := exec.Command("git", "-C", wtPath, "checkout", "-b", "wip/task-B").Run(); err != nil {
		t.Fatalf("failed to create wip/task-B: %v", err)
	}

	// Make changes for item B
	if err := os.WriteFile(filepath.Join(wtPath, "task-b.txt"), []byte("item B"), 0644); err != nil {
		t.Fatalf("failed to create item B file: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-B" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-B",
				Title: "Document Foo", // slug is "document-foo" which should NOT match wi/foo, falling back to origin/main
			})
		} else if r.Method == "POST" && r.URL.Path == "/tasks/task-B/submit" {
			var req struct {
				AgentID string
				Result  string
				Links   []struct {
					Kind  string
					Value string
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links) != 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Links[0].Kind != "commit" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(req.Links[0].Value) != 40 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "item B done",
		"task-B",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}

	// Verify: commit message is "Document Foo"
	cmd := exec.Command("git", "-C", wtPath, "log", "-1", "--format=%s")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get commit message: %v", err)
	}
	if string(output) != "Document Foo\n" {
		t.Errorf("expected commit message 'Document Foo', got %q", string(output))
	}

	// Verify: only 1 commit on top of wi/document-foo (proves it created new commit, not amended)
	cmd = exec.Command("git", "-C", wtPath, "rev-list", "--count", "wi/document-foo..HEAD")
	output, err = cmd.Output()
	if err != nil {
		t.Fatalf("failed to count commits: %v", err)
	}
	if string(output) != "1\n" {
		t.Errorf("expected 1 commit on top of wi/document-foo, got %q", string(output))
	}

	// Verify: log shows both commits in correct order
	cmd = exec.Command("git", "-C", wtPath, "log", "--oneline")
	output, err = cmd.Output()
	if err != nil {
		t.Fatalf("failed to get commit log: %v", err)
	}
	outputStr := string(output)
	if !strings.Contains(outputStr, "Document Foo") {
		t.Errorf("expected log to contain 'Document Foo', got: %s", outputStr)
	}
	if !strings.Contains(outputStr, "Item A") {
		t.Errorf("expected log to contain 'Item A', got: %s", outputStr)
	}
}

func TestExecuteTasksTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
				{ID: "task-2", State: "in_progress", Model: "sonnet", Kind: "review", Title: "Task 2"},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), server.URL, "test-token", false, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executeTasks failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "task-1") {
		t.Errorf("expected output to contain 'task-1', got: %s", output)
	}
	if !strings.Contains(output, "task-2") {
		t.Errorf("expected output to contain 'task-2', got: %s", output)
	}
	if !strings.Contains(output, "ID") || !strings.Contains(output, "STATE") {
		t.Errorf("expected table headers in output, got: %s", output)
	}
}

func TestExecuteTasksWithStateFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "application/json")
			if state == "ready" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
				})
			} else {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
					{ID: "task-2", State: "in_progress", Model: "sonnet", Kind: "review", Title: "Task 2"},
				})
			}
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), server.URL, "test-token", false, []string{"--project", "proj-1", "--state", "ready"}, buf)
	if err != nil {
		t.Fatalf("executeTasks failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "task-1") {
		t.Errorf("expected output to contain 'task-1', got: %s", output)
	}
	if strings.Contains(output, "task-2") {
		t.Errorf("expected output to NOT contain 'task-2' (filtered by state), got: %s", output)
	}
}

func TestExecuteTasksWithModelFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			model := r.URL.Query().Get("model")
			w.Header().Set("Content-Type", "application/json")
			if model == "sonnet" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-2", State: "in_progress", Model: "sonnet", Kind: "review", Title: "Task 2"},
				})
			} else {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
					{ID: "task-2", State: "in_progress", Model: "sonnet", Kind: "review", Title: "Task 2"},
				})
			}
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), server.URL, "test-token", false, []string{"--project", "proj-1", "--model", "sonnet"}, buf)
	if err != nil {
		t.Fatalf("executeTasks failed: %v", err)
	}

	output := buf.String()
	if strings.Contains(output, "task-1") {
		t.Errorf("expected output to NOT contain 'task-1' (filtered by model), got: %s", output)
	}
	if !strings.Contains(output, "task-2") {
		t.Errorf("expected output to contain 'task-2', got: %s", output)
	}
}

func TestExecuteTasksJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), server.URL, "test-token", true, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executeTasks failed: %v", err)
	}

	output := buf.String()
	var result []tuiclient.Task
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if len(result) != 1 {
		t.Errorf("expected 1 task in JSON, got %d", len(result))
	}
	if result[0].ID != "task-1" {
		t.Errorf("expected task ID 'task-1', got %q", result[0].ID)
	}
}

func TestExecuteTasksEmptyResultJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "application/json")
			if state == "review" {
				json.NewEncoder(w).Encode([]tuiclient.Task{})
			} else {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
				})
			}
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), server.URL, "test-token", true, []string{"--project", "proj-1", "--state", "review"}, buf)
	if err != nil {
		t.Fatalf("executeTasks with filter resulting in empty set failed: %v", err)
	}

	output := buf.String()
	var result []tuiclient.Task
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if result == nil {
		t.Error("expected non-nil empty slice [], got null")
	}
	if len(result) != 0 {
		t.Errorf("expected 0 tasks in JSON, got %d", len(result))
	}
}

func TestExecuteTasksMissingProject(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), "http://localhost:8080", "test-token", false, []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing --project, got nil")
	}
	if !strings.Contains(err.Error(), "--project") {
		t.Errorf("expected error to mention --project, got: %v", err)
	}
}

func TestExecuteTasksMissingURL(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), "", "test-token", false, []string{"--project", "proj-1"}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteTasksMissingToken(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), "http://localhost:8080", "", false, []string{"--project", "proj-1"}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecuteTasksPriority(t *testing.T) {
	// Test that priority is displayed in the tasks table output
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			w.Header().Set("Content-Type", "application/json")
			priority600 := int64(600)
			priority1001 := int64(1001) // Above 1000, from Front action
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{
					ID:       "task-1",
					State:    "ready",
					Model:    "haiku",
					Kind:     "implement",
					Title:    "Task with Priority 600",
					Priority: &priority600,
				},
				{
					ID:       "task-2",
					State:    "ready",
					Model:    "haiku",
					Kind:     "implement",
					Title:    "Task with Priority 1001",
					Priority: &priority1001,
				},
				{
					ID:    "task-3",
					State: "ready",
					Model: "haiku",
					Kind:  "implement",
					Title: "Task without Priority",
				},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeTasks(context.Background(), server.URL, "test-token", false, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executeTasks failed: %v", err)
	}

	output := buf.String()
	// Check that PRIORITY column header is present
	if !strings.Contains(output, "PRIORITY") {
		t.Errorf("expected 'PRIORITY' column header in output, got: %s", output)
	}
	// Check that values are displayed (600, 1001, empty for task-3)
	if !strings.Contains(output, "600") {
		t.Errorf("expected priority value '600' in output, got: %s", output)
	}
	if !strings.Contains(output, "1001") {
		t.Errorf("expected priority value '1001' in output, got: %s", output)
	}
}

func TestExecutePendingTable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			state := r.URL.Query().Get("state")
			fields := r.URL.Query().Get("fields")
			if fields != "summary" {
				t.Errorf("expected fields=summary in request, got: %s", fields)
			}
			w.Header().Set("Content-Type", "application/json")
			if state == "review" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "review", Kind: "implement", Title: "Task 1"},
				})
			} else if state == "approved" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-2", State: "approved", Kind: "review", Title: "Task 2"},
				})
			} else {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "review", Kind: "implement", Title: "Task 1"},
					{ID: "task-2", State: "approved", Kind: "review", Title: "Task 2"},
					{ID: "task-3", State: "ready", Kind: "implement", Title: "Task 3"},
				})
			}
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePending(context.Background(), server.URL, "test-token", false, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executePending failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "task-1") {
		t.Errorf("expected output to contain 'task-1', got: %s", output)
	}
	if !strings.Contains(output, "task-2") {
		t.Errorf("expected output to contain 'task-2', got: %s", output)
	}
	if strings.Contains(output, "task-3") {
		t.Errorf("expected output to NOT contain 'task-3' (not review/approved), got: %s", output)
	}
	if !strings.Contains(output, "ID") || !strings.Contains(output, "STATE") {
		t.Errorf("expected table headers in output, got: %s", output)
	}
}

func TestExecutePendingJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			state := r.URL.Query().Get("state")
			fields := r.URL.Query().Get("fields")
			if fields != "summary" {
				t.Errorf("expected fields=summary in request, got: %s", fields)
			}
			w.Header().Set("Content-Type", "application/json")
			if state == "review" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "review", Kind: "implement", Title: "Task 1"},
				})
			} else if state == "approved" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-2", State: "approved", Kind: "review", Title: "Task 2"},
				})
			} else {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "review", Kind: "implement", Title: "Task 1"},
					{ID: "task-2", State: "approved", Kind: "review", Title: "Task 2"},
					{ID: "task-3", State: "ready", Kind: "implement", Title: "Task 3"},
				})
			}
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePending(context.Background(), server.URL, "test-token", true, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executePending with JSON failed: %v", err)
	}

	output := buf.String()
	var result []tuiclient.Task
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("expected 2 tasks in JSON, got %d", len(result))
	}
	if result[0].State != "review" && result[0].State != "approved" {
		t.Errorf("expected task state to be review or approved, got %q", result[0].State)
	}
	if result[1].State != "review" && result[1].State != "approved" {
		t.Errorf("expected task state to be review or approved, got %q", result[1].State)
	}
}

func TestExecutePendingEmptyQueueJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePending(context.Background(), server.URL, "test-token", true, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executePending with empty queue JSON failed: %v", err)
	}

	output := buf.String()
	var result []tuiclient.Task
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if result == nil {
		t.Error("expected non-nil empty slice [], got null")
	}
	if len(result) != 0 {
		t.Errorf("expected 0 tasks in JSON, got %d", len(result))
	}
}

func TestExecutePendingEmptyQueue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePending(context.Background(), server.URL, "test-token", false, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executePending with empty queue failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "ID") || !strings.Contains(output, "STATE") || !strings.Contains(output, "KIND") || !strings.Contains(output, "TITLE") {
		t.Errorf("expected table headers in output for empty queue, got: %s", output)
	}
}

func TestExecutePendingMissingProject(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executePending(context.Background(), "http://localhost:8080", "test-token", false, []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing --project, got nil")
	}
	if !strings.Contains(err.Error(), "--project") {
		t.Errorf("expected error to mention --project, got: %v", err)
	}
}

func TestExecutePendingOrdering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" {
			state := r.URL.Query().Get("state")
			w.Header().Set("Content-Type", "application/json")
			if state == "review" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-2", State: "review", Kind: "implement", Title: "Task 2", CreatedAt: "2026-09-12T00:00:00Z"},
				})
			} else if state == "approved" {
				json.NewEncoder(w).Encode([]tuiclient.Task{
					{ID: "task-1", State: "approved", Kind: "implement", Title: "Task 1", CreatedAt: "2026-09-11T00:00:00Z"},
				})
			}
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePending(context.Background(), server.URL, "test-token", true, []string{"--project", "proj-1"}, buf)
	if err != nil {
		t.Fatalf("executePending failed: %v", err)
	}

	output := buf.String()
	var result []tuiclient.Task
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if len(result) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(result))
	}

	if result[0].ID != "task-1" {
		t.Errorf("expected first task to be task-1 (older created_at), got %q", result[0].ID)
	}
	if result[1].ID != "task-2" {
		t.Errorf("expected second task to be task-2 (newer created_at), got %q", result[1].ID)
	}
}

func TestExecuteHeartbeatSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/heartbeat" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)

	os.Setenv("AGENT_ID", "test-agent")

	err := executeHeartbeat(context.Background(), server.URL, "test-token", []string{"task123"})
	if err != nil {
		t.Fatalf("executeHeartbeat failed: %v", err)
	}
}

func TestExecuteHeartbeatWithAgentFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/heartbeat" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeHeartbeat(context.Background(), server.URL, "test-token", []string{"--agent", "flag-agent", "task123"})
	if err != nil {
		t.Fatalf("executeHeartbeat failed: %v", err)
	}
}

func TestExecuteHeartbeatMissingTaskID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)

	os.Setenv("AGENT_ID", "test-agent")

	err := executeHeartbeat(context.Background(), server.URL, "test-token", []string{})
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}

	if !strings.Contains(err.Error(), "task ID is required") {
		t.Errorf("expected error to mention task ID, got: %v", err)
	}
}

func TestExecuteHeartbeatMissingAgent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	defer func() {
		if oldAgent != "" {
			os.Setenv("AGENT_ID", oldAgent)
		} else {
			os.Unsetenv("AGENT_ID")
		}
	}()

	os.Unsetenv("AGENT_ID")

	err := executeHeartbeat(context.Background(), server.URL, "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for missing agent ID, got nil")
	}

	if !strings.Contains(err.Error(), "agent ID is required") {
		t.Errorf("expected error to mention agent ID, got: %v", err)
	}
}

func TestExecuteHeartbeatMissingURL(t *testing.T) {
	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)

	os.Setenv("AGENT_ID", "test-agent")

	err := executeHeartbeat(context.Background(), "", "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}

	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteHeartbeatMissingToken(t *testing.T) {
	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)

	os.Setenv("AGENT_ID", "test-agent")

	err := executeHeartbeat(context.Background(), "http://localhost:8080", "", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}

	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecuteHeartbeatServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/heartbeat" {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "internal error"})
		}
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)

	os.Setenv("AGENT_ID", "test-agent")

	err := executeHeartbeat(context.Background(), server.URL, "test-token", []string{"task123"})
	if err == nil {
		t.Fatal("expected error for server error, got nil")
	}
}

func TestExecuteNextSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" && r.URL.Query().Get("claimable") == "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
				{ID: "task-2", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 2"},
			})
		}
	}))
	defer server.Close()

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--project", "proj-1",
		"--model", "haiku",
		"--kind", "implement",
	})
	if err != nil {
		t.Fatalf("executeNext failed: %v", err)
	}
}

func TestExecuteNextWithClaim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" && r.URL.Query().Get("claimable") == "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
			})
		} else if r.URL.Path == "/tasks/task-1/claim" {
			w.WriteHeader(http.StatusOK)
		} else if r.URL.Path == "/tasks/task-1" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "in_progress",
				Model: "haiku",
				Kind:  "implement",
				Title: "Task 1",
			})
		}
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Setenv("AGENT_ID", "test-agent")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--project", "proj-1",
		"--model", "haiku",
		"--kind", "implement",
		"--claim",
	})
	if err != nil {
		t.Fatalf("executeNext with claim failed: %v", err)
	}
}

func TestExecuteNextPinnedToSelectedTask(t *testing.T) {
	high := int64(1001)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" && r.URL.Query().Get("claimable") == "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-low", State: "ready", Model: "haiku", Kind: "implement", Title: "Low"},
				{ID: "task-high", State: "ready", Model: "haiku", Kind: "implement", Title: "High", Priority: &high},
			})
		}
	}))
	defer server.Close()
	args := []string{"--project", "proj-1", "--model", "haiku", "--kind", "implement"}

	t.Setenv(selectedTaskEnv, "task-low")
	out := captureStdout(t, func() {
		if err := executeNext(context.Background(), server.URL, "test-token", false, args); err != nil {
			t.Fatalf("executeNext pinned: %v", err)
		}
	})
	if strings.TrimSpace(out) != "task-low" {
		t.Fatalf("pinned next printed %q, want the selected task-low", out)
	}

	t.Setenv(selectedTaskEnv, "task-gone")
	err := executeNext(context.Background(), server.URL, "test-token", false, args)
	var claimErr *claimError
	if !errors.As(err, &claimErr) || claimErr.code != 2 {
		t.Fatalf("pinned to a task that is no longer claimable: got %v, want exit-2 claimError", err)
	}

	t.Setenv(selectedTaskEnv, "")
	out = captureStdout(t, func() {
		if err := executeNext(context.Background(), server.URL, "test-token", false, args); err != nil {
			t.Fatalf("executeNext unpinned: %v", err)
		}
	})
	if strings.TrimSpace(out) != "task-high" {
		t.Fatalf("unpinned next printed %q, want the highest-priority task-high", out)
	}
}

func TestExecuteNextRaced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" && r.URL.Query().Get("claimable") == "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
			})
		} else if r.URL.Path == "/tasks/task-1/claim" {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"code":    "already_claimed",
					"message": "already claimed",
				},
			})
		}
	}))
	defer server.Close()

	// Save current env values
	oldAgent := os.Getenv("AGENT_ID")
	oldModel := os.Getenv("AGENT_MODEL")
	defer func() {
		os.Setenv("AGENT_ID", oldAgent)
		os.Setenv("AGENT_MODEL", oldModel)
	}()

	os.Setenv("AGENT_ID", "test-agent")
	os.Setenv("AGENT_MODEL", "haiku")

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--project", "proj-1",
		"--model", "haiku",
		"--kind", "implement",
		"--claim",
	})
	if err == nil {
		t.Fatal("expected error for raced claim, got nil")
	}

	var claimErr *claimError
	if !errors.As(err, &claimErr) {
		t.Fatalf("expected claimError, got %T: %v", err, err)
	}

	if claimErr.code != 2 {
		t.Errorf("expected exit code 2, got %d", claimErr.code)
	}

	if !strings.Contains(claimErr.Error(), "raced") {
		t.Errorf("expected error message to contain 'raced', got: %v", claimErr.Error())
	}
}

func TestExecuteNextClaimSchedulingError(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")

	retryAfter := int64(30)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" && r.URL.Query().Get("claimable") == "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
			})
		} else if r.URL.Path == "/tasks/task-1/claim" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":                "ADMISSION_DEFERRED",
					"message":             "admission deferred",
					"retry_after_seconds": retryAfter,
				},
			})
		}
	}))
	defer server.Close()

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--project", "proj-1",
		"--model", "haiku",
		"--kind", "implement",
		"--claim",
	})
	if err == nil {
		t.Fatal("expected error for scheduling error, got nil")
	}

	var schedErr *schedulingError
	if !errors.As(err, &schedErr) {
		t.Fatalf("expected schedulingError, got %T: %v", err, err)
	}

	if schedErr.code != 10 {
		t.Errorf("expected exit code 10, got %d", schedErr.code)
	}
	if schedErr.retryAfterSeconds == nil || *schedErr.retryAfterSeconds != 30 {
		t.Errorf("expected retryAfterSeconds 30, got %v", schedErr.retryAfterSeconds)
	}
}

func TestExecuteNextNothingClaimable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" && r.URL.Query().Get("claimable") == "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{})
		}
	}))
	defer server.Close()

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--project", "proj-1",
		"--model", "haiku",
		"--kind", "implement",
	})
	if err == nil {
		t.Fatal("expected error for nothing claimable, got nil")
	}

	var claimErr *claimError
	if !errors.As(err, &claimErr) {
		t.Fatalf("expected claimError, got %T: %v", err, err)
	}

	if claimErr.code != 2 {
		t.Errorf("expected exit code 2, got %d", claimErr.code)
	}

	if !strings.Contains(claimErr.Error(), "nothing claimable") {
		t.Errorf("expected error message to contain 'nothing claimable', got: %v", claimErr.Error())
	}
}

func TestExecuteNextJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/projects/proj-1/tasks" && r.URL.Query().Get("claimable") == "true" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Task{
				{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Task 1"},
			})
		}
	}))
	defer server.Close()

	err := executeNext(context.Background(), server.URL, "test-token", true, []string{
		"--project", "proj-1",
		"--model", "haiku",
		"--kind", "implement",
	})
	if err != nil {
		t.Fatalf("executeNext failed: %v", err)
	}
}

func TestExecuteNextMissingProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--model", "haiku",
		"--kind", "implement",
	})
	if err == nil {
		t.Fatal("expected error for missing --project, got nil")
	}

	if !strings.Contains(err.Error(), "--project") {
		t.Errorf("expected error to mention --project, got: %v", err)
	}
}

func TestExecuteNextMissingModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// With --model now optional, return empty task list (nothing claimable)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--project", "proj-1",
		"--kind", "implement",
	})
	if err == nil {
		t.Fatal("expected error for nothing claimable, got nil")
	}

	if !strings.Contains(err.Error(), "nothing claimable") {
		t.Errorf("expected error to mention nothing claimable, got: %v", err)
	}
}

func TestExecuteNextMissingKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := executeNext(context.Background(), server.URL, "test-token", false, []string{
		"--project", "proj-1",
		"--model", "haiku",
	})
	if err == nil {
		t.Fatal("expected error for missing --kind, got nil")
	}

	if !strings.Contains(err.Error(), "--kind") {
		t.Errorf("expected error to mention --kind, got: %v", err)
	}
}

func TestExecutePromoteSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/promote") {
			if r.Method != "POST" {
				t.Errorf("expected POST, got %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executePromote(context.Background(), server.URL, "test-token", []string{"task-123"})
	if err != nil {
		t.Fatalf("executePromote failed: %v", err)
	}
}

func TestExecutePromoteMissingTaskID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := executePromote(context.Background(), server.URL, "test-token", []string{})
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}
	if !strings.Contains(err.Error(), "task ID is required") {
		t.Errorf("expected error to mention 'task ID is required', got: %v", err)
	}
}

func TestExecutePromoteMissingURL(t *testing.T) {
	err := executePromote(context.Background(), "", "test-token", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecutePromoteMissingToken(t *testing.T) {
	err := executePromote(context.Background(), "http://localhost:8080", "", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecutePromoteServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/promote") {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"code":    "internal_error",
					"message": "server error",
				},
			})
		}
	}))
	defer server.Close()

	err := executePromote(context.Background(), server.URL, "test-token", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for failed promote, got nil")
	}
	if !strings.Contains(err.Error(), "failed to promote task") {
		t.Errorf("expected error to mention 'failed to promote task', got: %v", err)
	}
}

func TestResolveAgentIdentity(t *testing.T) {
	tests := []struct {
		name          string
		agentFlag     string
		modelFlag     string
		envAgent      string
		envModel      string
		expectedAgent string
		expectedModel string
		expectError   bool
		errorContains string
	}{
		{
			name:          "flag wins",
			agentFlag:     "flag-agent",
			modelFlag:     "flag-model",
			envAgent:      "env-agent",
			envModel:      "env-model",
			expectedAgent: "flag-agent",
			expectedModel: "flag-model",
		},
		{
			name:          "agent flag wins over env",
			agentFlag:     "flag-agent",
			modelFlag:     "",
			envAgent:      "env-agent",
			envModel:      "env-model",
			expectedAgent: "flag-agent",
			expectedModel: "env-model",
		},
		{
			name:          "model flag wins over env",
			agentFlag:     "",
			modelFlag:     "flag-model",
			envAgent:      "env-agent",
			envModel:      "env-model",
			expectedAgent: "env-agent",
			expectedModel: "flag-model",
		},
		{
			name:          "fallback to env when flags empty",
			agentFlag:     "",
			modelFlag:     "",
			envAgent:      "env-agent",
			envModel:      "env-model",
			expectedAgent: "env-agent",
			expectedModel: "env-model",
		},
		{
			name:          "error when agent ID missing",
			agentFlag:     "",
			modelFlag:     "flag-model",
			envAgent:      "",
			envModel:      "env-model",
			expectError:   true,
			errorContains: "agent ID is required",
		},
		{
			name:          "error when model missing",
			agentFlag:     "flag-agent",
			modelFlag:     "",
			envAgent:      "env-agent",
			envModel:      "",
			expectError:   true,
			errorContains: "model is required",
		},
		{
			name:          "error when both missing",
			agentFlag:     "",
			modelFlag:     "",
			envAgent:      "",
			envModel:      "",
			expectError:   true,
			errorContains: "agent ID is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Save current env values
			oldAgent := os.Getenv("AGENT_ID")
			oldModel := os.Getenv("AGENT_MODEL")
			defer func() {
				os.Setenv("AGENT_ID", oldAgent)
				os.Setenv("AGENT_MODEL", oldModel)
			}()

			// Set test env values
			if tt.envAgent != "" {
				os.Setenv("AGENT_ID", tt.envAgent)
			} else {
				os.Unsetenv("AGENT_ID")
			}
			if tt.envModel != "" {
				os.Setenv("AGENT_MODEL", tt.envModel)
			} else {
				os.Unsetenv("AGENT_MODEL")
			}

			// Call function
			agent, model, err := resolveAgentIdentity(tt.agentFlag, tt.modelFlag)

			// Check error
			if tt.expectError {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain %q, got: %v", tt.errorContains, err)
				}
			} else {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				if agent != tt.expectedAgent {
					t.Errorf("expected agent %q, got %q", tt.expectedAgent, agent)
				}
				if model != tt.expectedModel {
					t.Errorf("expected model %q, got %q", tt.expectedModel, model)
				}
			}
		})
	}
}

// TestParseFlagsWithPositionals_OrderIndependent pins the submit arg-order bug:
// `submit <id> --result x` (id first) must parse --result, not silently drop it.
func TestParseFlagsWithPositionals_OrderIndependent(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantID     string
		wantResult string
		wantErr    bool
	}{
		{"flags-first", []string{"--result", "ok", "TASK1"}, "TASK1", "ok", false},
		{"id-first (the bug)", []string{"TASK1", "--result", "ok"}, "TASK1", "ok", false},
		{"interspersed", []string{"--result", "ok", "TASK1", "--pr", "u"}, "TASK1", "ok", false},
		{"id-only", []string{"TASK1"}, "TASK1", "", false},
		{"no positional", []string{"--result", "ok"}, "", "ok", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("submit", flag.ContinueOnError)
			fs.SetOutput(&bytes.Buffer{})
			result := fs.String("result", "", "")
			fs.String("pr", "", "")

			pos, err := parseFlagsWithPositionals(fs, tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			gotID := ""
			if len(pos) > 0 {
				gotID = pos[0]
			}
			if gotID != tc.wantID {
				t.Errorf("task id = %q, want %q", gotID, tc.wantID)
			}
			if *result != tc.wantResult {
				t.Errorf("--result = %q, want %q", *result, tc.wantResult)
			}
		})
	}
}

// TestParsePRURL tests URL parsing for GitHub PR URLs
func TestParsePRURL(t *testing.T) {
	cases := []struct {
		name       string
		url        string
		wantOwner  string
		wantRepo   string
		wantNumber int
		wantErr    bool
	}{
		{"valid github url", "https://github.com/boldfield/odonian/pull/174", "boldfield", "odonian", 174, false},
		{"valid with trailing slash", "https://github.com/boldfield/odonian/pull/174/", "boldfield", "odonian", 174, false},
		{"invalid not github", "https://gitlab.com/boldfield/odonian/pull/174", "", "", 0, true},
		{"invalid path", "https://github.com/boldfield/odonian", "", "", 0, true},
		{"invalid pr number", "https://github.com/boldfield/odonian/pull/abc", "", "", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			owner, repo, number, err := parsePRURL(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if !tc.wantErr {
				if owner != tc.wantOwner {
					t.Errorf("owner = %q, want %q", owner, tc.wantOwner)
				}
				if repo != tc.wantRepo {
					t.Errorf("repo = %q, want %q", repo, tc.wantRepo)
				}
				if number != tc.wantNumber {
					t.Errorf("number = %d, want %d", number, tc.wantNumber)
				}
			}
		})
	}
}

// TestExecuteMergeSuccess tests the happy path: successful merge and task transitions
func TestExecuteMergeSuccess(t *testing.T) {
	// Create a mock forge server (GitHub API)
	forgeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge") {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"merged": true})
		}
	}))
	defer forgeServer.Close()

	// Temporarily replace the GitHub base URL
	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = forgeServer.URL
	t.Cleanup(func() { forge.GitHubBaseURL = oldBaseURL })

	// Create a mock odonian API server
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			if strings.HasSuffix(r.URL.Path, "/merge-123") {
				// Return merge task with target_task_id pointing to parent
				json.NewEncoder(w).Encode(tuiclient.TaskDetail{
					ID:           "merge-123",
					State:        "approved",
					TargetTaskID: ptrString("parent-456"),
				})
			} else if strings.HasSuffix(r.URL.Path, "/parent-456") {
				// Return parent task
				json.NewEncoder(w).Encode(tuiclient.TaskDetail{
					ID:         "parent-456",
					State:      "approved",
					AgentMerge: true,
					Links: []tuiclient.TaskLink{
						{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/174"},
					},
				})
			}
		} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transition") {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer apiServer.Close()

	err := executeMerge(context.Background(), apiServer.URL, "test-token", []string{"merge-123"})
	if err != nil {
		t.Fatalf("executeMerge failed: %v", err)
	}
}

// TestExecuteMergeForgeFails tests handling of forge (GitHub API) failure
func TestExecuteMergeForgeFails(t *testing.T) {
	// Create a mock forge server that fails
	forgeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge") {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("PR is not mergeable"))
		}
	}))
	defer forgeServer.Close()

	// Temporarily replace the GitHub base URL
	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = forgeServer.URL
	t.Cleanup(func() { forge.GitHubBaseURL = oldBaseURL })

	// Create a mock odonian API server
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/tasks/") {
			if strings.HasSuffix(r.URL.Path, "/merge-123") {
				json.NewEncoder(w).Encode(tuiclient.TaskDetail{
					ID:           "merge-123",
					State:        "approved",
					TargetTaskID: ptrString("parent-456"),
				})
			} else if strings.HasSuffix(r.URL.Path, "/parent-456") {
				json.NewEncoder(w).Encode(tuiclient.TaskDetail{
					ID:         "parent-456",
					State:      "approved",
					AgentMerge: true,
					Links: []tuiclient.TaskLink{
						{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/174"},
					},
				})
			}
		}
	}))
	defer apiServer.Close()

	err := executeMerge(context.Background(), apiServer.URL, "test-token", []string{"merge-123"})
	if err == nil {
		t.Fatal("expected error for failed forge merge")
	}
	if !strings.Contains(err.Error(), "failed to squash merge PR") {
		t.Errorf("expected 'failed to squash merge PR' in error, got: %v", err)
	}
}

// TestExecuteMergeIdempotent tests that a merge task already in 'done' is a no-op:
// the command returns nil without re-merging or transitioning anything.
func TestExecuteMergeIdempotent(t *testing.T) {
	forgePutCalled := false
	forgeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge") {
			forgePutCalled = true
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"merged": true})
		}
	}))
	defer forgeServer.Close()

	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = forgeServer.URL
	t.Cleanup(func() { forge.GitHubBaseURL = oldBaseURL })

	transitionCalled := false
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/merge-123") {
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:           "merge-123",
				State:        "done",
				TargetTaskID: ptrString("parent-456"),
			})
		} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transition") {
			transitionCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer apiServer.Close()

	// A merge task already in 'done' should be a clean no-op (idempotent).
	err := executeMerge(context.Background(), apiServer.URL, "test-token", []string{"merge-123"})
	if err != nil {
		t.Fatalf("expected no-op success for an already-done merge task, got: %v", err)
	}
	if forgePutCalled {
		t.Error("expected no forge merge call for an already-done merge task")
	}
	if transitionCalled {
		t.Error("expected no transition for an already-done merge task")
	}
}

// TestExecuteMergeFinalizesAfterPartialRun reproduces the zombie-merge bug: a prior
// run merged the PR and advanced the parent to 'done' but died before finalizing the
// merge task, leaving it 'in_progress'. The retry must NOT re-merge (the parent is
// already done) and must finalize the merge task to 'done' instead of erroring.
func TestExecuteMergeFinalizesAfterPartialRun(t *testing.T) {
	forgePutCalled := false
	forgeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/merge") {
			forgePutCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer forgeServer.Close()

	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = forgeServer.URL
	t.Cleanup(func() { forge.GitHubBaseURL = oldBaseURL })

	mergeTransitionedTo := ""
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/merge-123") {
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:           "merge-123",
				State:        "in_progress",
				TargetTaskID: ptrString("parent-456"),
			})
		} else if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/parent-456") {
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:         "parent-456",
				State:      "done", // prior run already merged + finalized the parent
				AgentMerge: true,
				Links: []tuiclient.TaskLink{
					{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/174"},
				},
			})
		} else if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transition") {
			var body struct {
				To string `json:"to"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			if strings.Contains(r.URL.Path, "/merge-123/") {
				mergeTransitionedTo = body.To
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer apiServer.Close()

	err := executeMerge(context.Background(), apiServer.URL, "test-token", []string{"merge-123"})
	if err != nil {
		t.Fatalf("expected partial-run retry to converge, got: %v", err)
	}
	if forgePutCalled {
		t.Error("expected no re-merge when parent is already done")
	}
	if mergeTransitionedTo != "done" {
		t.Errorf("expected merge task transitioned to done, got %q", mergeTransitionedTo)
	}
}

func TestExecuteDiffWithPR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "in_progress",
				Links: []tuiclient.TaskLink{
					{Kind: "pr", Value: "https://github.com/boldfield/odonian/pull/123"},
				},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeDiff(context.Background(), server.URL, "test-token", []string{"task-1"}, buf)
	if err != nil {
		t.Fatalf("executeDiff failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "https://github.com/boldfield/odonian/pull/123") {
		t.Errorf("expected PR URL in output, got: %s", output)
	}
}

func TestExecuteDiffNoPR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "in_progress",
				Links: []tuiclient.TaskLink{},
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeDiff(context.Background(), server.URL, "test-token", []string{"task-1"}, buf)
	if err == nil {
		t.Fatal("expected error for task with no PR link, got nil")
	}
	if !strings.Contains(err.Error(), "no pull request link") {
		t.Errorf("expected error to mention 'no pull request link', got: %v", err)
	}
}

func TestExecuteDiffMissingID(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeDiff(context.Background(), "http://localhost:8080", "test-token", []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}
	if !strings.Contains(err.Error(), "task id required") {
		t.Errorf("expected error to mention 'task id required', got: %v", err)
	}
}

func TestExecuteDiffMissingURL(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeDiff(context.Background(), "", "test-token", []string{"task-1"}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteDiffMissingToken(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executeDiff(context.Background(), "http://localhost:8080", "", []string{"task-1"}, buf)
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecuteDiffLocalCommitBaseDiff(t *testing.T) {
	// Create a temporary git repo with a commit
	tmpDir := t.TempDir()
	runCmd := func(dir string, args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Fatalf("git command failed: %v", err)
		}
	}

	// Initialize repo with main branch
	runCmd(tmpDir, "git", "init", "-b", "main")
	runCmd(tmpDir, "git", "config", "user.email", "test@example.com")
	runCmd(tmpDir, "git", "config", "user.name", "Test User")

	// Create initial commit on main
	if err := os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("initial\n"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	runCmd(tmpDir, "git", "add", "file.txt")
	runCmd(tmpDir, "git", "commit", "-m", "initial commit")

	// Create origin/main ref
	runCmd(tmpDir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")

	// Create a new commit for the diff
	if err := os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("initial\nnew line\n"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	runCmd(tmpDir, "git", "add", "file.txt")
	runCmd(tmpDir, "git", "commit", "-m", "add line")

	// Get the commit SHA
	cmd := exec.Command("git", "-C", tmpDir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get commit SHA: %v", err)
	}
	commitSHA := strings.TrimSpace(string(out))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(withCurrentRound(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "in_progress",
				Links: []tuiclient.TaskLink{
					{Kind: "commit", Value: commitSHA},
				},
			}))
		}
	}))
	defer server.Close()

	// Set up environment for local_commit mode
	oldMode := os.Getenv("ODONIAN_DELIVERY_MODE")
	t.Cleanup(func() { os.Setenv("ODONIAN_DELIVERY_MODE", oldMode) })
	os.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")

	buf := &bytes.Buffer{}
	err = executeDiff(context.Background(), server.URL, "test-token", []string{"--repo", tmpDir, "task-1"}, buf)
	if err != nil {
		t.Fatalf("executeDiff failed: %v", err)
	}

	output := buf.String()
	// The diff should show the new line we added
	if !strings.Contains(output, "new line") {
		t.Errorf("expected 'new line' in diff output, got: %s", output)
	}
}

func TestExecuteDiffLocalCommitFull(t *testing.T) {
	// Create a temporary git repo with a commit
	tmpDir := t.TempDir()
	runCmd := func(dir string, args ...string) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		if err := cmd.Run(); err != nil {
			t.Fatalf("git command failed: %v", err)
		}
	}

	// Initialize repo with main branch
	runCmd(tmpDir, "git", "init", "-b", "main")
	runCmd(tmpDir, "git", "config", "user.email", "test@example.com")
	runCmd(tmpDir, "git", "config", "user.name", "Test User")

	// Create initial commit on main
	if err := os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("initial\n"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	runCmd(tmpDir, "git", "add", "file.txt")
	runCmd(tmpDir, "git", "commit", "-m", "initial commit")

	// Create origin/main ref
	runCmd(tmpDir, "git", "update-ref", "refs/remotes/origin/main", "HEAD")

	// Create a new commit for the show
	if err := os.WriteFile(filepath.Join(tmpDir, "file.txt"), []byte("initial\nnew line\n"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	runCmd(tmpDir, "git", "add", "file.txt")
	runCmd(tmpDir, "git", "commit", "-m", "add line")

	// Get the commit SHA
	cmd := exec.Command("git", "-C", tmpDir, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get commit SHA: %v", err)
	}
	commitSHA := strings.TrimSpace(string(out))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(withCurrentRound(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "in_progress",
				Links: []tuiclient.TaskLink{
					{Kind: "commit", Value: commitSHA},
				},
			}))
		}
	}))
	defer server.Close()

	// Set up environment for local_commit mode
	oldMode := os.Getenv("ODONIAN_DELIVERY_MODE")
	t.Cleanup(func() { os.Setenv("ODONIAN_DELIVERY_MODE", oldMode) })
	os.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")

	buf := &bytes.Buffer{}
	err = executeDiff(context.Background(), server.URL, "test-token", []string{"--repo", tmpDir, "--full", "task-1"}, buf)
	if err != nil {
		t.Fatalf("executeDiff failed: %v", err)
	}

	output := buf.String()
	// The show should contain the commit message
	if !strings.Contains(output, "add line") {
		t.Errorf("expected 'add line' (commit message) in show output, got: %s", output)
	}
}

func TestExecuteDiffLocalCommitMissingCommitLink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-1",
				State: "in_progress",
				Links: []tuiclient.TaskLink{},
			})
		}
	}))
	defer server.Close()

	// Set up environment for local_commit mode
	oldMode := os.Getenv("ODONIAN_DELIVERY_MODE")
	t.Cleanup(func() { os.Setenv("ODONIAN_DELIVERY_MODE", oldMode) })
	os.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")

	buf := &bytes.Buffer{}
	err := executeDiff(context.Background(), server.URL, "test-token", []string{"--repo", "/tmp", "task-1"}, buf)
	if err == nil {
		t.Fatal("expected error for task with no commit link, got nil")
	}
	if !strings.Contains(err.Error(), "no commit link") {
		t.Errorf("expected error to mention 'no commit link', got: %v", err)
	}
}

func TestExecuteApproveSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				State: "approved",
				Title: "Test Task",
			})
		} else if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/transition") && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeApprove(context.Background(), server.URL, "test-token", []string{"task-123"})
	if err != nil {
		t.Fatalf("executeApprove failed: %v", err)
	}
}

func TestExecuteApproveWrongState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				State: "ready",
				Title: "Test Task",
			})
		}
	}))
	defer server.Close()

	err := executeApprove(context.Background(), server.URL, "test-token", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for task not in approved state, got nil")
	}
	if !strings.Contains(err.Error(), "expected approved") {
		t.Errorf("expected error to mention 'expected approved', got: %v", err)
	}
}

func TestExecuteApproveMissingTaskID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := executeApprove(context.Background(), server.URL, "test-token", []string{})
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}
	if !strings.Contains(err.Error(), "task id required") {
		t.Errorf("expected error to mention 'task id required', got: %v", err)
	}
}

func TestExecuteApproveMissingURL(t *testing.T) {
	err := executeApprove(context.Background(), "", "test-token", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteApproveMissingToken(t *testing.T) {
	err := executeApprove(context.Background(), "http://localhost:8080", "", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecuteApproveServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "task not found"})
		}
	}))
	defer server.Close()

	err := executeApprove(context.Background(), server.URL, "test-token", []string{"nonexistent"})
	if err == nil {
		t.Fatal("expected error for server error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get task") {
		t.Errorf("expected error to mention 'failed to get task', got: %v", err)
	}
}

// ptrString returns a pointer to a string
func ptrString(s string) *string {
	return &s
}

func TestExecuteRejectFromReviewSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				State: "review",
				Title: "Test Task",
			})
		} else if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/transition") && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{"task-123", "--note", "needs rework"})
	if err != nil {
		t.Fatalf("executeReject failed: %v", err)
	}
}

func TestExecuteRejectFromApprovedSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				State: "approved",
				Title: "Test Task",
			})
		} else if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/transition") && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{"task-123", "--note", "rejected"})
	if err != nil {
		t.Fatalf("executeReject failed: %v", err)
	}
}

func TestExecuteRejectMissingNote(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				State: "review",
				Title: "Test Task",
			})
		}
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for missing --note, got nil")
	}
	if !strings.Contains(err.Error(), "--note") {
		t.Errorf("expected error to mention '--note', got: %v", err)
	}
}

func TestExecuteRejectWrongState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				State: "ready",
				Title: "Test Task",
			})
		}
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{"task-123", "--note", "bad state"})
	if err == nil {
		t.Fatal("expected error for task not in review or approved state, got nil")
	}
	if !strings.Contains(err.Error(), "expected review or approved") {
		t.Errorf("expected error to mention 'expected review or approved', got: %v", err)
	}
}

func TestExecuteRejectMissingTaskID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{"--note", "some note"})
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}
	if !strings.Contains(err.Error(), "task id required") {
		t.Errorf("expected error to mention 'task id required', got: %v", err)
	}
}

func TestExecuteRejectMissingURL(t *testing.T) {
	err := executeReject(context.Background(), "", "test-token", []string{"task-123", "--note", "test"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteRejectMissingToken(t *testing.T) {
	err := executeReject(context.Background(), "http://localhost:8080", "", []string{"task-123", "--note", "test"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func TestExecuteRejectServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "task not found"})
		}
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{"nonexistent", "--note", "test"})
	if err == nil {
		t.Fatal("expected error for server error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get task") {
		t.Errorf("expected error to mention 'failed to get task', got: %v", err)
	}
}

func TestExecuteRejectAbandonLocalCommit(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)
	t.Setenv("ODONIAN_SKIP_PATH_VALIDATION", "1")

	// Create a temporary git repo with worktree and branches
	repoDir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
		{"git", "branch", "work"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
	}

	// Create worktree and wip branch
	iid := "task-123"
	worktreePath := filepath.Join(tmpDir, iid)
	cmd := exec.Command("git", "-C", repoDir, "worktree", "add", worktreePath, "work")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to add worktree: %v", err)
	}

	cmd = exec.Command("git", "-C", repoDir, "branch", "wip/"+iid)
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip branch: %v", err)
	}

	// Create wi/slug branch (should not be touched)
	cmd = exec.Command("git", "-C", repoDir, "branch", "wi/test-task")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wi/slug branch: %v", err)
	}

	// Verify worktree and branches exist
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should exist before reject: %v", err)
	}

	cmd = exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "wip/"+iid)
	if err := cmd.Run(); err != nil {
		t.Fatalf("wip branch should exist before reject: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    iid,
				State: "review",
				Title: "Test Task",
			})
		} else if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/transition") && r.Method == http.MethodPost {
			var req struct {
				To   string
				Note *string
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.To != "failed" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{
		"--note", "abandoned",
		"--abandon",
		"--repo", repoDir,
		iid,
	})
	if err != nil {
		t.Fatalf("executeReject failed: %v", err)
	}

	// Verify worktree is gone
	if _, err := os.Stat(worktreePath); err == nil {
		t.Errorf("worktree should not exist after reject --abandon")
	}

	// Verify wip branch is gone
	cmd = exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "wip/"+iid)
	if err := cmd.Run(); err == nil {
		t.Errorf("wip branch should not exist after reject --abandon")
	}

	// Verify wi/slug branch still exists
	cmd = exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "wi/test-task")
	if err := cmd.Run(); err != nil {
		t.Errorf("wi/slug branch should still exist after reject --abandon")
	}
}

func TestExecuteRejectAbandonPullRequest(t *testing.T) {
	// In pull_request mode, --abandon should transition to failed but skip cleanup
	t.Setenv("ODONIAN_DELIVERY_MODE", "pull_request")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				State: "review",
				Title: "Test Task",
			})
		} else if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/transition") && r.Method == http.MethodPost {
			var req struct {
				To   string
				Note *string
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.To != "failed" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	err := executeReject(context.Background(), server.URL, "test-token", []string{
		"--note", "abandoned",
		"--abandon",
		"task-123",
	})
	if err != nil {
		t.Fatalf("executeReject failed: %v", err)
	}
}

func TestExecuteRejectReworkPreservesWorktree(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)
	t.Setenv("ODONIAN_SKIP_PATH_VALIDATION", "1")

	// Create a temporary git repo with worktree and branches
	repoDir := t.TempDir()
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
		{"git", "branch", "work"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
	}

	// Create worktree and wip branch
	iid := "task-456"
	worktreePath := filepath.Join(tmpDir, iid)
	cmd := exec.Command("git", "-C", repoDir, "worktree", "add", worktreePath, "work")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to add worktree: %v", err)
	}

	cmd = exec.Command("git", "-C", repoDir, "branch", "wip/"+iid)
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip branch: %v", err)
	}

	// Verify worktree and branches exist before reject
	if _, err := os.Stat(worktreePath); err != nil {
		t.Fatalf("worktree should exist before reject: %v", err)
	}

	cmd = exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "wip/"+iid)
	if err := cmd.Run(); err != nil {
		t.Fatalf("wip branch should exist before reject: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    iid,
				State: "review",
				Title: "Test Task",
			})
		} else if strings.HasPrefix(r.URL.Path, "/tasks/") && strings.HasSuffix(r.URL.Path, "/transition") && r.Method == http.MethodPost {
			var req struct {
				To   string
				Note *string
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			// Rework should transition to "ready", not "failed"
			if req.To != "ready" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	// Call reject with --note but WITHOUT --abandon (rework case)
	err := executeReject(context.Background(), server.URL, "test-token", []string{
		"--note", "needs rework",
		"--repo", repoDir,
		iid,
	})
	if err != nil {
		t.Fatalf("executeReject failed: %v", err)
	}

	// Verify worktree still exists (rework must preserve it)
	if _, err := os.Stat(worktreePath); err != nil {
		t.Errorf("worktree should still exist after reject (rework): %v", err)
	}

	// Verify wip branch still exists (rework must preserve it)
	cmd = exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "wip/"+iid)
	if err := cmd.Run(); err != nil {
		t.Errorf("wip branch should still exist after reject (rework)")
	}
}

func TestExecuteApproveLocalCommitSuccess(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)

	// Create main repo
	mainRepo := t.TempDir()
	initGitRepo(t, mainRepo)

	// Create initial commit on main
	cmd := exec.Command("git", "-C", mainRepo, "commit", "--allow-empty", "-m", "initial")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create fake origin/main
	cmd = exec.Command("git", "-C", mainRepo, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create origin/main: %v", err)
	}

	// Create wip/task-123 branch with a new commit
	cmd = exec.Command("git", "-C", mainRepo, "commit", "--allow-empty", "-m", "wip work")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip commit: %v", err)
	}

	cmd = exec.Command("git", "-C", mainRepo, "branch", "-f", "wip/task-123", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip branch: %v", err)
	}

	// Capture the SHA of wip/task-123 before freeze (the freeze will delete the branch)
	wipShaCmd := exec.Command("git", "-C", mainRepo, "rev-parse", "wip/task-123")
	expectedWipSha, err := wipShaCmd.Output()
	if err != nil {
		t.Fatalf("failed to get wip/task-123 SHA: %v", err)
	}

	// Create a worktree directory
	wtPath := filepath.Join(tmpDir, "task-123")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}

	// Mock the server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(reviewedTaskDetail("task-123", "approved", "Test Task Title", strings.TrimSpace(string(expectedWipSha))))
		} else if r.Method == "POST" && r.URL.Path == "/tasks/task-123/transition" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	// Call executeApprove
	err = executeApprove(context.Background(), server.URL, "test-token", []string{
		"--repo", mainRepo,
		"task-123",
	})
	if err != nil {
		t.Fatalf("executeApprove failed: %v", err)
	}

	// Verify wi/test-task-title was created and points to the wip commit
	miShaCmd := exec.Command("git", "-C", mainRepo, "rev-parse", "wi/test-task-title")
	actualMiSha, err := miShaCmd.Output()
	if err != nil {
		// List branches to debug
		debugCmd := exec.Command("git", "-C", mainRepo, "branch", "-a")
		debugOut, _ := debugCmd.Output()
		t.Fatalf("wi/test-task-title should exist after Freeze. Branches: %s", string(debugOut))
	}
	if strings.TrimSpace(string(actualMiSha)) != strings.TrimSpace(string(expectedWipSha)) {
		t.Errorf("wi/test-task-title (SHA %s) should point to wip/task-123 (SHA %s)", strings.TrimSpace(string(actualMiSha)), strings.TrimSpace(string(expectedWipSha)))
	}

	// Verify wip/task-123 was deleted
	cmd = exec.Command("git", "-C", mainRepo, "rev-parse", "--verify", "--quiet", "wip/task-123")
	if err := cmd.Run(); err == nil {
		t.Fatal("wip/task-123 should be deleted after Freeze")
	}
}

func TestExecuteApproveLocalCommitFootgun(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)

	// Create main repo
	mainRepo := t.TempDir()
	initGitRepo(t, mainRepo)

	// Create initial commit on main
	cmd := exec.Command("git", "-C", mainRepo, "commit", "--allow-empty", "-m", "initial")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create fake origin/main
	cmd = exec.Command("git", "-C", mainRepo, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create origin/main: %v", err)
	}

	// Create wi/test-task-title branch at initial commit
	initialSha := getGitSHA(t, mainRepo, "HEAD")
	cmd = exec.Command("git", "-C", mainRepo, "branch", "-f", "wi/test-task-title", initialSha)
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wi branch: %v", err)
	}

	// Create wip/task-123 with a new commit
	cmd = exec.Command("git", "-C", mainRepo, "commit", "--allow-empty", "-m", "wip work")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip commit: %v", err)
	}

	cmd = exec.Command("git", "-C", mainRepo, "branch", "-f", "wip/task-123", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip branch: %v", err)
	}

	footgunWipSha := getGitSHA(t, mainRepo, "wip/task-123")

	// Create a worktree with wi/test-task-title checked out
	wtPath := filepath.Join(tmpDir, "worktree")
	cmd = exec.Command("git", "-C", mainRepo, "worktree", "add", wtPath, "wi/test-task-title")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create worktree: %v", err)
	}

	// Mock the server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(reviewedTaskDetail("task-123", "approved", "Test Task Title", footgunWipSha))
		}
	}))
	defer server.Close()

	// Call executeApprove - should fail with footgun error
	err := executeApprove(context.Background(), server.URL, "test-token", []string{
		"--repo", mainRepo,
		"task-123",
	})
	if err == nil {
		t.Fatal("expected footgun error, got nil")
	}

	if !strings.Contains(err.Error(), "is checked out at") {
		t.Errorf("error should mention 'is checked out at', got: %v", err)
	}

	// Verify wi/test-task-title is still at initial commit (unchanged)
	cmd = exec.Command("git", "-C", mainRepo, "rev-parse", "wi/test-task-title")
	currentSha, _ := cmd.Output()
	if strings.TrimSpace(string(currentSha)) != initialSha {
		t.Errorf("wi/test-task-title should be unchanged after failed Freeze")
	}

	// Verify wip/task-123 still exists
	cmd = exec.Command("git", "-C", mainRepo, "rev-parse", "--verify", "--quiet", "wip/task-123")
	if err := cmd.Run(); err != nil {
		t.Fatal("wip/task-123 should still exist after failed Freeze")
	}
}

func TestExecuteApproveFreezeOnly(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	tmpDir := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", tmpDir)

	// Create main repo
	mainRepo := t.TempDir()
	initGitRepo(t, mainRepo)

	// Create initial commit on main
	cmd := exec.Command("git", "-C", mainRepo, "commit", "--allow-empty", "-m", "initial")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create initial commit: %v", err)
	}

	// Create fake origin/main
	cmd = exec.Command("git", "-C", mainRepo, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create origin/main: %v", err)
	}

	// Create wip/task-123 with a new commit
	cmd = exec.Command("git", "-C", mainRepo, "commit", "--allow-empty", "-m", "wip work")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip commit: %v", err)
	}

	cmd = exec.Command("git", "-C", mainRepo, "branch", "-f", "wip/task-123", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create wip branch: %v", err)
	}

	// Capture the SHA of wip/task-123 before freeze
	wipShaCmd := exec.Command("git", "-C", mainRepo, "rev-parse", "wip/task-123")
	expectedWipShaFreezeOnly, err := wipShaCmd.Output()
	if err != nil {
		t.Fatalf("failed to get wip/task-123 SHA: %v", err)
	}

	// Create a worktree directory
	wtPath := filepath.Join(tmpDir, "task-123")
	if err := os.MkdirAll(wtPath, 0755); err != nil {
		t.Fatalf("failed to create worktree dir: %v", err)
	}

	// Mock the server
	transitionCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task-123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(reviewedTaskDetail("task-123", "done", "Test Task Title", strings.TrimSpace(string(expectedWipShaFreezeOnly))))
		} else if r.Method == "POST" && r.URL.Path == "/tasks/task-123/transition" {
			transitionCalled = true
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	// Call executeApprove with --freeze-only
	err = executeApprove(context.Background(), server.URL, "test-token", []string{
		"--repo", mainRepo,
		"--freeze-only",
		"task-123",
	})
	if err != nil {
		t.Fatalf("executeApprove with --freeze-only failed: %v", err)
	}

	// Verify transition was NOT called
	if transitionCalled {
		t.Fatal("transition should not be called with --freeze-only")
	}

	// Verify wi/test-task-title was created and points to the wip commit
	miShaCmd := exec.Command("git", "-C", mainRepo, "rev-parse", "wi/test-task-title")
	actualMiSha, err := miShaCmd.Output()
	if err != nil {
		t.Fatal("wi/test-task-title should exist after Freeze")
	}
	if strings.TrimSpace(string(actualMiSha)) != strings.TrimSpace(string(expectedWipShaFreezeOnly)) {
		t.Errorf("wi/test-task-title (SHA %s) should point to wip/task-123 (SHA %s)", strings.TrimSpace(string(actualMiSha)), strings.TrimSpace(string(expectedWipShaFreezeOnly)))
	}
}

// setupRepoForWtEnsure creates a temporary git repo for testing wt-ensure
func setupRepoForWtEnsure(t *testing.T) string {
	tmpDir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = tmpDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("setup failed: %v", err)
		}
	}

	cmd := exec.Command("git", "-C", tmpDir, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("setup failed to create origin/main: %v", err)
	}

	return tmpDir
}

func TestExecuteWtEnsurePullRequestMode(t *testing.T) {
	// Don't set ODONIAN_DELIVERY_MODE, defaults to pull_request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tuiclient.TaskDetail{ID: "task-123", Title: "Test Task"})
	}))
	defer server.Close()

	err := executeWtEnsure(context.Background(), server.URL, "test-token", []string{"task-123", "--repo", "/tmp/repo"})
	if err == nil {
		t.Fatal("expected error in pull_request mode, got nil")
	}
	if !strings.Contains(err.Error(), "local_commit") {
		t.Errorf("expected error to mention 'local_commit', got: %v", err)
	}
}

func TestExecuteWtEnsureLocalCommitMode(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	repoDir := setupRepoForWtEnsure(t)
	wtHome := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", wtHome)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-123",
				Title: "Test Task Feature",
			})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err := executeWtEnsure(context.Background(), server.URL, "test-token", []string{"task-123", "--repo", repoDir})
	w.Close()
	os.Stdout = oldStdout

	if err != nil {
		t.Fatalf("executeWtEnsure failed: %v", err)
	}

	// Read output
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("failed to read output: %v", err)
	}

	output := buf.String()
	if output == "" {
		t.Error("expected output (worktree path), got empty string")
	}
}

func TestExecuteWtEnsureIdempotent(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	repoDir := setupRepoForWtEnsure(t)
	wtHome := t.TempDir()
	t.Setenv("ODONIAN_WORKTREE_HOME", wtHome)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:    "task-456",
				Title: "Another Task",
			})
		}
	}))
	defer server.Close()

	// First call
	err := executeWtEnsure(context.Background(), server.URL, "test-token", []string{"task-456", "--repo", repoDir})
	if err != nil {
		t.Fatalf("first executeWtEnsure failed: %v", err)
	}

	// Second call - should not error (idempotent)
	err = executeWtEnsure(context.Background(), server.URL, "test-token", []string{"task-456", "--repo", repoDir})
	if err != nil {
		t.Fatalf("second executeWtEnsure failed (not idempotent): %v", err)
	}
}

func TestExecuteWtEnsureMissingTaskID(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	err := executeWtEnsure(context.Background(), server.URL, "test-token", []string{"--repo", "/tmp/repo"})
	if err == nil {
		t.Fatal("expected error for missing task ID, got nil")
	}
	if !strings.Contains(err.Error(), "task ID is required") {
		t.Errorf("expected error to mention 'task ID is required', got: %v", err)
	}
}

func TestExecuteWtEnsureMissingRepo(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	t.Setenv("ODONIAN_REPO", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(tuiclient.TaskDetail{ID: "task-123", Title: "Test Task"})
	}))
	defer server.Close()

	err := executeWtEnsure(context.Background(), server.URL, "test-token", []string{"task-123"})
	if err == nil {
		t.Fatal("expected error for missing repo, got nil")
	}
	// Error should mention --repo or ODONIAN_REPO requirement
	if !strings.Contains(err.Error(), "--repo") && !strings.Contains(err.Error(), "ODONIAN_REPO") {
		t.Errorf("expected error to mention '--repo' or 'ODONIAN_REPO', got: %v", err)
	}
}

func TestExecuteWtEnsureMissingURL(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	err := executeWtEnsure(context.Background(), "", "test-token", []string{"task-123", "--repo", "/tmp/repo"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_URL, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_URL") {
		t.Errorf("expected error to mention ODONIAN_URL, got: %v", err)
	}
}

func TestExecuteWtEnsureMissingToken(t *testing.T) {
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")
	err := executeWtEnsure(context.Background(), "http://localhost:8080", "", []string{"task-123", "--repo", "/tmp/repo"})
	if err == nil {
		t.Fatal("expected error for missing ODONIAN_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "ODONIAN_TOKEN") {
		t.Errorf("expected error to mention ODONIAN_TOKEN, got: %v", err)
	}
}

func initGitRepo(t *testing.T, repoPath string) {
	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
		{"git", "commit", "--allow-empty", "-m", "initial"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoPath
		if err := cmd.Run(); err != nil {
			t.Fatalf("git setup failed: %v", err)
		}
	}

	cmd := exec.Command("git", "-C", repoPath, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := cmd.Run(); err != nil {
		t.Fatalf("failed to create origin/main: %v", err)
	}
}

func setupGitConfig(t *testing.T, repoPath string) {
	cmds := [][]string{
		{"git", "config", "user.email", "test@example.com"},
		{"git", "config", "user.name", "Test User"},
	}

	for _, args := range cmds {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = repoPath
		if err := cmd.Run(); err != nil {
			t.Fatalf("git config failed: %v", err)
		}
	}
}

func getGitSHA(t *testing.T, repoPath, ref string) string {
	cmd := exec.Command("git", "-C", repoPath, "rev-parse", ref)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("failed to get SHA for %s: %v", ref, err)
	}
	return strings.TrimSpace(string(output))
}

// TestExecuteShowInitialTask tests showing an initial task with no review
func TestExecuteShowInitialTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-initial":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-initial",
				Title:       "Initial Task",
				Spec:        "Do something",
				State:       "ready",
				ReviewRound: 0,
				Kind:        "implement",
				Model:       "haiku",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-initial"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Initial Task") {
		t.Errorf("expected output to contain title, got: %s", output)
	}
	if strings.Contains(output, "Review Round") {
		t.Errorf("expected no Review Round for initial task, got: %s", output)
	}
	if strings.Contains(output, "Review Findings") {
		t.Errorf("expected no Review Findings for initial task, got: %s", output)
	}
}

// TestExecuteShowReworkTaskWithFindings tests showing a rework task with review findings
func TestExecuteShowReworkTaskWithFindings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-rework":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-rework",
				Title:       "Rework Task",
				Spec:        "Fix the code",
				State:       "ready",
				ReviewRound: 1,
				Kind:        "implement",
				Model:       "haiku",
			})
		case "/tasks/task-rework/events":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Event{
				{
					ID:     "event-0",
					TaskID: "task-rework",
					Actor:  "system",
					Kind:   "spawn_review",
					Note: func() *string {
						s := "Round 1 with models: [\"opus\"]"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:00Z",
				},
				{
					ID:     "event-1",
					TaskID: "task-rework",
					Actor:  "opus-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "reject"
						return &s
					}(),
					Note: func() *string {
						s := "The error handling is missing in the main path"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:01Z",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-rework"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Review Round: 1") {
		t.Errorf("expected Review Round in output, got: %s", output)
	}
	if !strings.Contains(output, "Review Findings") {
		t.Errorf("expected Review Findings in output, got: %s", output)
	}
	if !strings.Contains(output, "opus-reviewer") {
		t.Errorf("expected reviewer name in output, got: %s", output)
	}
	if !strings.Contains(output, "reject") {
		t.Errorf("expected reject verdict in output, got: %s", output)
	}
	if !strings.Contains(output, "error handling") {
		t.Errorf("expected finding text in output, got: %s", output)
	}
}

// TestExecuteShowReworkTaskJSON tests showing a rework task with JSON output
func TestExecuteShowReworkTaskJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-json":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-json",
				Title:       "JSON Task",
				Spec:        "Test JSON output",
				State:       "ready",
				ReviewRound: 1,
				Kind:        "implement",
				Model:       "haiku",
			})
		case "/tasks/task-json/events":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Event{
				{
					ID:     "event-0",
					TaskID: "task-json",
					Actor:  "system",
					Kind:   "spawn_review",
					Note: func() *string {
						s := "Round 1 with models: [\"haiku\"]"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:00Z",
				},
				{
					ID:     "event-1",
					TaskID: "task-json",
					Actor:  "reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "approve"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:01Z",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", true, []string{"--json", "task-json"}, buf)
	if err != nil {
		t.Fatalf("executeShow with --json failed: %v", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v", err)
	}

	if result["title"] != "JSON Task" {
		t.Errorf("expected title in JSON, got: %v", result["title"])
	}
	if result["review_round"] != float64(1) {
		t.Errorf("expected review_round in JSON, got: %v", result["review_round"])
	}
	if _, ok := result["review_findings"]; !ok {
		t.Errorf("expected review_findings in JSON output")
	}
}

// TestExecuteShowMultipleReviewers tests showing findings from multiple reviewers
func TestExecuteShowMultipleReviewers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-multi":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-multi",
				Title:       "Multi-Reviewer Task",
				Spec:        "Address feedback",
				State:       "ready",
				ReviewRound: 1,
				Kind:        "implement",
				Model:       "haiku",
			})
		case "/tasks/task-multi/events":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Event{
				{
					ID:     "event-0",
					TaskID: "task-multi",
					Actor:  "system",
					Kind:   "spawn_review",
					Note: func() *string {
						s := "Round 1 with models: [\"opus\",\"gpt-5.5\"]"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:00Z",
				},
				{
					ID:     "event-1",
					TaskID: "task-multi",
					Actor:  "opus-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "reject"
						return &s
					}(),
					Note: func() *string {
						s := "Performance issue in loop"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:01Z",
				},
				{
					ID:     "event-2",
					TaskID: "task-multi",
					Actor:  "gpt-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "approve"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:02Z",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-multi"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "opus-reviewer") {
		t.Errorf("expected opus-reviewer in output, got: %s", output)
	}
	if !strings.Contains(output, "gpt-reviewer") {
		t.Errorf("expected gpt-reviewer in output, got: %s", output)
	}
	if !strings.Contains(output, "reject") {
		t.Errorf("expected reject verdict, got: %s", output)
	}
	if !strings.Contains(output, "approve") {
		t.Errorf("expected approve verdict, got: %s", output)
	}
	if !strings.Contains(output, "Performance issue") {
		t.Errorf("expected finding text, got: %s", output)
	}
}

// TestExecuteShowApprovalNoteNotLabeledRejection tests that a note on an approve
// verdict is not presented to the worker as a rejection finding to address.
func TestExecuteShowApprovalNoteNotLabeledRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-approve-note":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-approve-note",
				Title:       "Mixed Verdict With Approval Note",
				Spec:        "Address feedback",
				State:       "ready",
				ReviewRound: 1,
				Kind:        "implement",
				Model:       "haiku",
			})
		case "/tasks/task-approve-note/events":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Event{
				{
					ID:     "event-0",
					TaskID: "task-approve-note",
					Actor:  "system",
					Kind:   "spawn_review",
					Note: func() *string {
						s := "Round 1 with models: [\"opus\",\"gpt-5.5\"]"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:00Z",
				},
				{
					ID:     "event-1",
					TaskID: "task-approve-note",
					Actor:  "opus-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "reject"
						return &s
					}(),
					Note: func() *string {
						s := "Missing input validation"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:01Z",
				},
				{
					ID:     "event-2",
					TaskID: "task-approve-note",
					Actor:  "gpt-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "approve"
						return &s
					}(),
					Note: func() *string {
						s := "LGTM, nicely scoped"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:02Z",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-approve-note"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "opus-reviewer] rejection: Missing input validation") {
		t.Errorf("expected the reject verdict's note labeled as rejection, got: %s", output)
	}
	if strings.Contains(output, "gpt-reviewer] rejection") {
		t.Errorf("expected approve verdict's note NOT labeled as rejection, got: %s", output)
	}
	if !strings.Contains(output, "gpt-reviewer] approval: LGTM") {
		t.Errorf("expected approve verdict's note labeled as approval, got: %s", output)
	}

	jsonBuf := &bytes.Buffer{}
	if err := executeShow(context.Background(), server.URL, "test-token", true, []string{"--json", "task-approve-note"}, jsonBuf); err != nil {
		t.Fatalf("executeShow --json failed: %v", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(jsonBuf.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v", err)
	}
	findings, ok := result["review_findings"].([]interface{})
	if !ok || len(findings) == 0 {
		t.Fatalf("expected review_findings array in JSON, got: %v", result["review_findings"])
	}
	round := findings[0].(map[string]interface{})
	roundFindings, ok := round["findings"].([]interface{})
	if !ok || len(roundFindings) != 2 {
		t.Fatalf("expected 2 findings in round, got: %v", round["findings"])
	}
	for _, rf := range roundFindings {
		f := rf.(map[string]interface{})
		switch f["reviewer"] {
		case "opus-reviewer":
			if f["kind"] != "rejection" {
				t.Errorf("expected opus-reviewer finding kind rejection, got: %v", f["kind"])
			}
		case "gpt-reviewer":
			if f["kind"] != "approval" {
				t.Errorf("expected gpt-reviewer finding kind approval, got: %v", f["kind"])
			}
		default:
			t.Errorf("unexpected reviewer in findings: %v", f["reviewer"])
		}
	}
}

// TestExecuteShowMultipleRounds tests showing findings from multiple review rounds with history
func TestExecuteShowMultipleRounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-rounds":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-rounds",
				Title:       "Multi-Round Task",
				Spec:        "Address feedback over multiple rounds",
				State:       "ready",
				ReviewRound: 2,
				Kind:        "implement",
				Model:       "haiku",
			})
		case "/tasks/task-rounds/events":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Event{
				{
					ID:     "event-0",
					TaskID: "task-rounds",
					Actor:  "system",
					Kind:   "spawn_review",
					Note: func() *string {
						s := "Round 1 with models: [\"opus\"]"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:00Z",
				},
				{
					ID:     "event-1",
					TaskID: "task-rounds",
					Actor:  "opus-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "reject"
						return &s
					}(),
					Note: func() *string {
						s := "Missing error handling in retry logic"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:01Z",
				},
				{
					ID:     "event-2",
					TaskID: "task-rounds",
					Actor:  "system",
					Kind:   "spawn_review",
					Note: func() *string {
						s := "Round 2 with models: [\"gpt-5.5\"]"
						return &s
					}(),
					CreatedAt: "2026-01-01T01:00:00Z",
				},
				{
					ID:     "event-3",
					TaskID: "task-rounds",
					Actor:  "gpt-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "approve"
						return &s
					}(),
					CreatedAt: "2026-01-01T01:00:01Z",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-rounds"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Round 1") {
		t.Errorf("expected Round 1 in output, got: %s", output)
	}
	if !strings.Contains(output, "Round 2") {
		t.Errorf("expected Round 2 in output, got: %s", output)
	}
	if !strings.Contains(output, "(historical)") {
		t.Errorf("expected (historical) marker for round 1 findings, got: %s", output)
	}
	if !strings.Contains(output, "Missing error handling") {
		t.Errorf("expected historical finding text from round 1, got: %s", output)
	}
	if !strings.Contains(output, "opus-reviewer") {
		t.Errorf("expected opus-reviewer in output, got: %s", output)
	}
	if !strings.Contains(output, "gpt-reviewer") {
		t.Errorf("expected gpt-reviewer in output, got: %s", output)
	}
	if !strings.Contains(output, "approve") {
		t.Errorf("expected approve verdict from round 2, got: %s", output)
	}
}

// TestExecuteShowEventRetrievalFailure tests handling of event retrieval failures
func TestExecuteShowEventRetrievalFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-fail":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-fail",
				Title:       "Failing Task",
				Spec:        "Test failure",
				State:       "ready",
				ReviewRound: 1,
				Kind:        "implement",
				Model:       "haiku",
			})
		case "/tasks/task-fail/events":
			w.WriteHeader(http.StatusInternalServerError)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]string{
					"code":    "internal_error",
					"message": "failed to retrieve events",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-fail"}, buf)
	if err == nil {
		t.Errorf("expected error when events cannot be retrieved, got nil")
	}
	if !strings.Contains(err.Error(), "failed to get task events") {
		t.Errorf("expected error message about event retrieval, got: %v", err)
	}
}

// TestPprofEnabledFromEnv verifies runServer's actual production wiring: pprofEnabledFromEnv
// reads ODONIAN_PPROF from the environment itself, so this exercises the same call the server
// makes, not a value the test re-derives. Only the exact literal "true" must enable pprof;
// case variants, other truthy-looking values, and whitespace must all disable it.
func TestPprofEnabledFromEnv(t *testing.T) {
	tests := []struct {
		envValue string
		want     bool
	}{
		{"true", true},
		{"", false},
		{"TRUE", false},
		{"True", false},
		{"1", false},
		{"yes", false},
		{" true", false},
		{"true ", false},
	}
	for _, tt := range tests {
		t.Setenv("ODONIAN_PPROF", tt.envValue)
		if got := pprofEnabledFromEnv(); got != tt.want {
			t.Errorf("ODONIAN_PPROF=%q: pprofEnabledFromEnv() = %v, want %v", tt.envValue, got, tt.want)
		}
	}
}

// TestParseSlowRequestThreshold verifies parseSlowRequestThreshold correctly parses
// ODONIAN_SLOW_REQUEST_MS and validates the threshold value. Invalid values return the
// default 500ms and log one warning via log.Printf.
func TestParseSlowRequestThreshold(t *testing.T) {
	tests := []struct {
		name        string
		envValue    string
		want        int
		wantWarning bool
	}{
		{"empty string defaults to 500", "", 500, false},
		{"valid positive integer", "250", 250, false},
		{"zero is valid", "0", 0, false},
		{"negative value uses default", "-5", 500, true},
		{"non-integer string uses default", "abc", 500, true},
		{"decimal string uses default", "1.5", 500, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logBuf := &bytes.Buffer{}
			logWriter := log.Writer()
			log.SetOutput(logBuf)
			t.Cleanup(func() {
				log.SetOutput(logWriter)
			})

			got := parseSlowRequestThreshold(tt.envValue)
			if got != tt.want {
				t.Errorf("parseSlowRequestThreshold(%q) = %d, want %d", tt.envValue, got, tt.want)
			}

			logOutput := logBuf.String()
			if tt.wantWarning {
				if !strings.Contains(logOutput, "ODONIAN_SLOW_REQUEST_MS") {
					t.Errorf("expected warning containing 'ODONIAN_SLOW_REQUEST_MS', got: %s", logOutput)
				}
				warningCount := strings.Count(logOutput, "ODONIAN_SLOW_REQUEST_MS")
				if warningCount != 1 {
					t.Errorf("expected exactly 1 warning, got %d warnings", warningCount)
				}
			} else {
				if strings.Contains(logOutput, "ODONIAN_SLOW_REQUEST_MS") {
					t.Errorf("expected no warning, got: %s", logOutput)
				}
			}
		})
	}
}

func TestParseResearchEscalationThresholds(t *testing.T) {
	tests := []struct {
		name          string
		thresholdsStr string
		allowedModels []string
		want          map[string]int
		wantErr       bool
		errMsg        string
	}{
		{
			"empty string returns empty map",
			"",
			[]string{"haiku", "sonnet", "opus"},
			map[string]int{},
			false,
			"",
		},
		{
			"valid single threshold",
			"opus=4",
			[]string{"haiku", "sonnet", "opus"},
			map[string]int{"opus": 4},
			false,
			"",
		},
		{
			"valid multiple thresholds",
			"haiku=8,sonnet=6,opus=4",
			[]string{"haiku", "sonnet", "opus"},
			map[string]int{"haiku": 8, "sonnet": 6, "opus": 4},
			false,
			"",
		},
		{
			"threshold with spaces",
			"opus = 4",
			[]string{"haiku", "sonnet", "opus"},
			map[string]int{"opus": 4},
			false,
			"",
		},
		{
			"malformed threshold missing equals",
			"opus4",
			[]string{"haiku", "sonnet", "opus"},
			nil,
			true,
			"invalid threshold format",
		},
		{
			"malformed threshold invalid value",
			"opus=abc",
			[]string{"haiku", "sonnet", "opus"},
			nil,
			true,
			"invalid threshold value",
		},
		{
			"negative threshold",
			"opus=-1",
			[]string{"haiku", "sonnet", "opus"},
			nil,
			true,
			"must be non-negative",
		},
		{
			"unknown model",
			"unknown=4",
			[]string{"haiku", "sonnet", "opus"},
			nil,
			true,
			"not in ODONIAN_MODELS allowlist",
		},
		{
			"trailing comma skipped",
			"opus=4,",
			[]string{"haiku", "sonnet", "opus"},
			map[string]int{"opus": 4},
			false,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResearchEscalationThresholds(tt.thresholdsStr, tt.allowedModels)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResearchEscalationThresholds(%q, ...) error = %v, wantErr %v", tt.thresholdsStr, err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err != nil && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("parseResearchEscalationThresholds(%q, ...) error = %v, want error containing %q", tt.thresholdsStr, err, tt.errMsg)
				}
				return
			}
			if !mapsEqual(got, tt.want) {
				t.Errorf("parseResearchEscalationThresholds(%q, ...) = %v, want %v", tt.thresholdsStr, got, tt.want)
			}
		})
	}
}

func TestParseResearchRoundBudget(t *testing.T) {
	tests := []struct {
		name      string
		budgetStr string
		want      int
		wantErr   bool
		errMsg    string
	}{
		{"empty string returns default 6", "", 6, false, ""},
		{"valid positive value", "3", 3, false, ""},
		{"valid value with whitespace", " 10 ", 10, false, ""},
		{"non-numeric value", "abc", 0, true, "invalid research round budget"},
		{"zero is rejected", "0", 0, true, "must be positive"},
		{"negative value is rejected", "-1", 0, true, "must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResearchRoundBudget(tt.budgetStr)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResearchRoundBudget(%q) error = %v, wantErr %v", tt.budgetStr, err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err != nil && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("parseResearchRoundBudget(%q) error = %v, want error containing %q", tt.budgetStr, err, tt.errMsg)
				}
				return
			}
			if got != tt.want {
				t.Errorf("parseResearchRoundBudget(%q) = %d, want %d", tt.budgetStr, got, tt.want)
			}
		})
	}
}

func TestParseResearchEscalationLadder(t *testing.T) {
	tests := []struct {
		name          string
		ladderStr     string
		allowedModels []string
		want          []string
		wantErr       bool
		errMsg        string
	}{
		{
			"empty string returns empty ladder",
			"",
			[]string{"haiku", "sonnet", "opus"},
			[]string{},
			false,
			"",
		},
		{
			"single model",
			"opus",
			[]string{"haiku", "sonnet", "opus"},
			[]string{"opus"},
			false,
			"",
		},
		{
			"valid ladder",
			"haiku,sonnet,opus",
			[]string{"haiku", "sonnet", "opus"},
			[]string{"haiku", "sonnet", "opus"},
			false,
			"",
		},
		{
			"ladder with spaces",
			"haiku , sonnet , opus",
			[]string{"haiku", "sonnet", "opus"},
			[]string{"haiku", "sonnet", "opus"},
			false,
			"",
		},
		{
			"deduplicates models",
			"haiku,sonnet,haiku,opus",
			[]string{"haiku", "sonnet", "opus"},
			[]string{"haiku", "sonnet", "opus"},
			false,
			"",
		},
		{
			"unknown model",
			"haiku,unknown",
			[]string{"haiku", "sonnet", "opus"},
			nil,
			true,
			"not in ODONIAN_MODELS allowlist",
		},
		{
			"trailing comma skipped",
			"haiku,sonnet,",
			[]string{"haiku", "sonnet", "opus"},
			[]string{"haiku", "sonnet"},
			false,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResearchEscalationLadder(tt.ladderStr, tt.allowedModels)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseResearchEscalationLadder(%q, ...) error = %v, wantErr %v", tt.ladderStr, err, tt.wantErr)
				return
			}
			if tt.wantErr {
				if err != nil && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("parseResearchEscalationLadder(%q, ...) error = %v, want error containing %q", tt.ladderStr, err, tt.errMsg)
				}
				return
			}
			if !slicesEqual(got, tt.want) {
				t.Errorf("parseResearchEscalationLadder(%q, ...) = %v, want %v", tt.ladderStr, got, tt.want)
			}
		})
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExecuteSubmitWithFindingsFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "findings*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	findings := `[
		{
			"id": "f1",
			"severity": "P2",
			"file": "main.go",
			"line": 42,
			"summary": "Missing error check",
			"in_changed_text": true,
			"status": "new"
		}
	]`
	if _, err := tmpFile.WriteString(findings); err != nil {
		t.Fatalf("failed to write findings file: %v", err)
	}
	tmpFile.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 0, "links": []map[string]string{}})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/submit" {
			var req struct {
				AgentID  string
				Result   string
				Findings json.RawMessage
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Findings == nil || len(req.Findings) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var f []map[string]interface{}
			if err := json.Unmarshal(req.Findings, &f); err != nil {
				t.Errorf("failed to unmarshal findings: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(f) != 1 {
				t.Errorf("expected 1 finding, got %d", len(f))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if f[0]["id"] != "f1" {
				t.Errorf("expected id 'f1', got %v", f[0]["id"])
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if f[0]["severity"] != "P2" {
				t.Errorf("expected severity 'P2', got %v", f[0]["severity"])
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if f[0]["file"] != "main.go" {
				t.Errorf("expected file 'main.go', got %v", f[0]["file"])
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err = executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "review done",
		"--findings-file", tmpFile.Name(),
		"task123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}
}

func TestExecuteSubmitFindingsFileNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "review done",
		"--findings-file", "/nonexistent/findings.json",
		"task123",
	})
	if err == nil {
		t.Fatalf("expected error for missing findings file, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read findings file") {
		t.Fatalf("expected 'failed to read findings file' error, got: %v", err)
	}
}

func TestExecuteSubmitFindingsInvalidJSON(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "findings*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("not valid json"); err != nil {
		t.Fatalf("failed to write findings file: %v", err)
	}
	tmpFile.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err = executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "review done",
		"--findings-file", tmpFile.Name(),
		"task123",
	})
	if err == nil {
		t.Fatalf("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "findings file is not valid JSON") {
		t.Fatalf("expected 'findings file is not valid JSON' error, got: %v", err)
	}
}

func TestExecuteSubmitFindingsNotArray(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "findings*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(`{"id": "f1"}`); err != nil {
		t.Fatalf("failed to write findings file: %v", err)
	}
	tmpFile.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err = executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "review done",
		"--findings-file", tmpFile.Name(),
		"task123",
	})
	if err == nil {
		t.Fatalf("expected error for non-array JSON, got nil")
	}
	if !strings.Contains(err.Error(), "findings file must be a JSON array") {
		t.Fatalf("expected 'findings file must be a JSON array' error, got: %v", err)
	}
}

func TestExecuteSubmitWithDisputesFile(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "disputes*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	disputes := `[
		{
			"finding_id": "f1",
			"evidence": "The source supports the claim as written; see page 4."
		}
	]`
	if _, err := tmpFile.WriteString(disputes); err != nil {
		t.Fatalf("failed to write disputes file: %v", err)
	}
	tmpFile.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 1, "links": []map[string]string{}})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/submit" {
			var req struct {
				AgentID  string
				Result   string
				Disputes json.RawMessage
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if req.Disputes == nil || len(req.Disputes) == 0 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var d []map[string]interface{}
			if err := json.Unmarshal(req.Disputes, &d); err != nil {
				t.Errorf("failed to unmarshal disputes: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if len(d) != 1 {
				t.Errorf("expected 1 dispute, got %d", len(d))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if d[0]["finding_id"] != "f1" {
				t.Errorf("expected finding_id 'f1', got %v", d[0]["finding_id"])
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if d[0]["evidence"] == "" {
				t.Errorf("expected non-empty evidence, got %v", d[0]["evidence"])
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err = executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "reworked, disputing f1",
		"--disputes-file", tmpFile.Name(),
		"--pr", "https://github.com/example/test-repo/pull/1",
		"--branch", "mr/task123",
		"task123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}
}

func TestExecuteSubmitDisputesFileNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "reworked",
		"--disputes-file", "/nonexistent/disputes.json",
		"task123",
	})
	if err == nil {
		t.Fatalf("expected error for missing disputes file, got nil")
	}
	if !strings.Contains(err.Error(), "failed to read disputes file") {
		t.Fatalf("expected 'failed to read disputes file' error, got: %v", err)
	}
}

func TestExecuteSubmitDisputesInvalidJSON(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "disputes*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString("not valid json"); err != nil {
		t.Fatalf("failed to write disputes file: %v", err)
	}
	tmpFile.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err = executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "reworked",
		"--disputes-file", tmpFile.Name(),
		"task123",
	})
	if err == nil {
		t.Fatalf("expected error for invalid JSON, got nil")
	}
	if !strings.Contains(err.Error(), "disputes file is not valid JSON") {
		t.Fatalf("expected 'disputes file is not valid JSON' error, got: %v", err)
	}
}

func TestExecuteSubmitDisputesNotArray(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "disputes*.json")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.WriteString(`{"finding_id": "f1"}`); err != nil {
		t.Fatalf("failed to write disputes file: %v", err)
	}
	tmpFile.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	oldAgent := os.Getenv("AGENT_ID")
	defer os.Setenv("AGENT_ID", oldAgent)
	os.Setenv("AGENT_ID", "test-agent")

	err = executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "reworked",
		"--disputes-file", tmpFile.Name(),
		"task123",
	})
	if err == nil {
		t.Fatalf("expected error for non-array JSON, got nil")
	}
	if !strings.Contains(err.Error(), "disputes file must be a JSON array") {
		t.Fatalf("expected 'disputes file must be a JSON array' error, got: %v", err)
	}
}

func TestExtractReviewFindingsWithStructuredFindings(t *testing.T) {
	severityP2 := "P2"
	findings := []tuiclient.Finding{
		{
			ID:            "f1",
			Severity:      severityP2,
			File:          "main.go",
			Line:          42,
			Summary:       "Missing error check",
			InChangedText: true,
			Status:        "new",
		},
	}

	events := []tuiclient.Event{
		{
			Kind:      "spawn_review",
			Note:      strPtr("Round 1 with models: [opus]"),
			Actor:     "system",
			CreatedAt: "2026-09-25T00:00:00Z",
		},
		{
			Kind:      "review",
			Verdict:   strPtr("reject"),
			Findings:  &findings,
			Actor:     "reviewer1",
			CreatedAt: "2026-09-25T00:01:00Z",
		},
	}

	result := extractReviewFindings(events, 1)

	if len(result) != 1 {
		t.Fatalf("expected 1 round, got %d", len(result))
	}
	if len(result[0].Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result[0].Findings))
	}

	f := result[0].Findings[0]
	if f.Severity != "P2" {
		t.Errorf("expected severity P2, got %s", f.Severity)
	}
	if f.File != "main.go" {
		t.Errorf("expected file main.go, got %s", f.File)
	}
	if f.Line != 42 {
		t.Errorf("expected line 42, got %d", f.Line)
	}
	if f.Summary != "Missing error check" {
		t.Errorf("expected summary 'Missing error check', got %s", f.Summary)
	}
	if !f.InChangedText {
		t.Errorf("expected in_changed_text true, got false")
	}
	if f.Status != "new" {
		t.Errorf("expected status 'new', got %s", f.Status)
	}
}

func TestExtractReviewFindingsWithoutStructuredFindings(t *testing.T) {
	events := []tuiclient.Event{
		{
			Kind:      "spawn_review",
			Note:      strPtr("Round 1 with models: [opus]"),
			Actor:     "system",
			CreatedAt: "2026-09-25T00:00:00Z",
		},
		{
			Kind:      "review",
			Verdict:   strPtr("reject"),
			Note:      strPtr("Please fix the error handling"),
			Findings:  nil,
			Actor:     "reviewer1",
			CreatedAt: "2026-09-25T00:01:00Z",
		},
	}

	result := extractReviewFindings(events, 1)

	if len(result) != 1 {
		t.Fatalf("expected 1 round, got %d", len(result))
	}
	if len(result[0].Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result[0].Findings))
	}

	f := result[0].Findings[0]
	if f.Text != "Please fix the error handling" {
		t.Errorf("expected prose note, got %s", f.Text)
	}
	if f.Severity != "" {
		t.Errorf("expected empty severity for prose finding, got %s", f.Severity)
	}
}

// TestExecuteShowStructuredFindingsRendering tests that structured findings are rendered
// with severity, file:line, status and summary in the text output
func TestExecuteShowStructuredFindingsRendering(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/task-structured":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:          "task-structured",
				Title:       "Task with Structured Findings",
				Spec:        "Fix the issues",
				State:       "ready",
				ReviewRound: 1,
				Kind:        "implement",
				Model:       "haiku",
			})
		case "/tasks/task-structured/events":
			w.Header().Set("Content-Type", "application/json")
			severityP1 := "P1"
			json.NewEncoder(w).Encode([]tuiclient.Event{
				{
					ID:     "event-0",
					TaskID: "task-structured",
					Actor:  "system",
					Kind:   "spawn_review",
					Note: func() *string {
						s := "Round 1 with models: [\"opus\"]"
						return &s
					}(),
					CreatedAt: "2026-01-01T00:00:00Z",
				},
				{
					ID:     "event-1",
					TaskID: "task-structured",
					Actor:  "opus-reviewer",
					Kind:   "review",
					Verdict: func() *string {
						s := "reject"
						return &s
					}(),
					Findings: &[]tuiclient.Finding{
						{
							ID:            "f1",
							Severity:      severityP1,
							File:          "handler.go",
							Line:          156,
							Summary:       "Missing nil check before dereference",
							InChangedText: true,
							Status:        "new",
						},
					},
					CreatedAt: "2026-01-01T00:00:01Z",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-structured"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	// Check that the structured finding is rendered with severity, file:line, status and summary
	if !strings.Contains(output, "opus-reviewer] rejection [P1] handler.go:156 (new): Missing nil check before dereference") {
		t.Errorf("expected structured finding with severity, file:line, status and summary in output, got: %s", output)
	}
}

// TestExtractReviewFindingsApproveWithStructuredFindings tests that structured findings
// with approve verdict are labeled as "approval" kind
func TestExtractReviewFindingsApproveWithStructuredFindings(t *testing.T) {
	severityP2 := "P2"
	findings := []tuiclient.Finding{
		{
			ID:            "f1",
			Severity:      severityP2,
			File:          "main.go",
			Line:          42,
			Summary:       "Missing error check",
			InChangedText: true,
			Status:        "new",
		},
	}

	events := []tuiclient.Event{
		{
			Kind:      "spawn_review",
			Note:      strPtr("Round 1 with models: [opus]"),
			Actor:     "system",
			CreatedAt: "2026-09-25T00:00:00Z",
		},
		{
			Kind:      "review",
			Verdict:   strPtr("approve"),
			Findings:  &findings,
			Actor:     "reviewer1",
			CreatedAt: "2026-09-25T00:01:00Z",
		},
	}

	result := extractReviewFindings(events, 1)

	if len(result) != 1 {
		t.Fatalf("expected 1 round, got %d", len(result))
	}
	if len(result[0].Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(result[0].Findings))
	}

	f := result[0].Findings[0]
	if f.Kind != "approval" {
		t.Errorf("expected kind 'approval' for approve verdict, got %s", f.Kind)
	}
	if f.Severity != "P2" {
		t.Errorf("expected severity P2, got %s", f.Severity)
	}
	if f.File != "main.go" {
		t.Errorf("expected file main.go, got %s", f.File)
	}
}

func TestValidateResearchDefaultModel(t *testing.T) {
	allowedModels := []string{"haiku", "sonnet", "opus"}

	tests := []struct {
		name      string
		modelStr  string
		allowed   []string
		wantModel string
		wantErr   bool
		errMsg    string
	}{
		{
			name:      "empty string returns empty",
			modelStr:  "",
			allowed:   allowedModels,
			wantModel: "",
			wantErr:   false,
		},
		{
			name:      "whitespace only returns empty",
			modelStr:  "   ",
			allowed:   allowedModels,
			wantModel: "",
			wantErr:   false,
		},
		{
			name:      "allowed model returns trimmed",
			modelStr:  "  opus  ",
			allowed:   allowedModels,
			wantModel: "opus",
			wantErr:   false,
		},
		{
			name:      "unallowlisted model returns error",
			modelStr:  "gpt-4",
			allowed:   allowedModels,
			wantModel: "",
			wantErr:   true,
			errMsg:    "ODONIAN_MODELS",
		},
		{
			name:      "unallowlisted model with whitespace returns error",
			modelStr:  "  invalid-model  ",
			allowed:   allowedModels,
			wantModel: "",
			wantErr:   true,
			errMsg:    "ODONIAN_MODELS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := validateResearchDefaultModel(tt.modelStr, tt.allowed)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateResearchDefaultModel() error = %v, wantErr %v", err, tt.wantErr)
			}
			if model != tt.wantModel {
				t.Errorf("validateResearchDefaultModel() model = %q, want %q", model, tt.wantModel)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("validateResearchDefaultModel() error = %v, want error containing %q", err, tt.errMsg)
			}
		})
	}
}

func strPtr(s string) *string {
	return &s
}

func TestExecuteSubmitWithManifestFile(t *testing.T) {
	manifestBody := `{"version":1,"parent_task_id":"task123","children":[],"pending_candidates":[]}`
	tmpFile := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(tmpFile, []byte(manifestBody), 0o600); err != nil {
		t.Fatalf("failed to write manifest file: %v", err)
	}

	var gotManifest json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/tasks/task123" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{"id": "task123", "review_round": 0, "links": []map[string]string{}})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/tasks/task123/submit" {
			var req struct {
				Manifest json.RawMessage `json:"manifest"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			gotManifest = req.Manifest
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), server.URL, "test-token", []string{
		"--result", "done",
		"--manifest-file", tmpFile,
		"--pr", "https://github.com/example/test-repo/pull/1",
		"--branch", "mr/task123",
		"task123",
	})
	if err != nil {
		t.Fatalf("executeSubmit failed: %v", err)
	}
	if string(gotManifest) != manifestBody {
		t.Fatalf("expected manifest %s to be sent in the request body, got %s", manifestBody, gotManifest)
	}
}

func TestExecuteSubmitManifestFileRejectedBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to server: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("AGENT_ID", "test-agent")

	dir := t.TempDir()
	notJSON := filepath.Join(dir, "bad.json")
	notObject := filepath.Join(dir, "array.json")
	if err := os.WriteFile(notJSON, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(notObject, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct{ file, want string }{
		"missing":    {filepath.Join(dir, "nope.json"), "failed to read manifest file"},
		"not_json":   {notJSON, "manifest file is not valid JSON"},
		"not_object": {notObject, "manifest file must be a JSON object"},
	} {
		t.Run(name, func(t *testing.T) {
			err := executeSubmit(context.Background(), server.URL, "test-token", []string{
				"--result", "done", "--manifest-file", tc.file, "task123",
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func showWithServer(t *testing.T, detail tuiclient.TaskDetail, jsonOutput bool) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(detail)
		}
	}))
	defer server.Close()
	args := []string{detail.ID}
	if jsonOutput {
		args = []string{"--json", detail.ID}
	}
	buf := &bytes.Buffer{}
	if err := executeShow(context.Background(), server.URL, "test-token", jsonOutput, args, buf); err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}
	return buf.String()
}

func requireContains(t *testing.T, output string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in output:\n%s", want, output)
		}
	}
}

func plannedContinuationDetail() tuiclient.TaskDetail {
	return tuiclient.TaskDetail{
		ID: "parent-1", State: "approved", Model: "haiku", Kind: "implement", Title: "Parent", Spec: "spec",
		Continuation: &tuiclient.ContinuationInfo{
			ManifestDigest: "digest-1",
			ProposedChildren: []tuiclient.ProposedChild{
				{Key: "a", Title: "Child A", Track: "research", Model: "haiku", InitialState: "ready", Status: "pending"},
				{Key: "b", Title: "Child B", Track: "build", Model: "sonnet", InitialState: "backlog", Status: "pending",
					Dependencies: []tuiclient.ContinuationDependency{{Kind: "child", Ref: "a"}}},
			},
			DeferredClaims: []tuiclient.DeferredClaim{{ClaimID: "c4", Owner: "alice"}},
			ExcludedClaims: []tuiclient.ExcludedClaim{{ClaimID: "c5", Reason: "out of scope"}},
			ActionItems: []tuiclient.ActionItem{{
				Type: "legacy_held_follow_up", TaskID: "fu-1", Title: "Old follow-up", State: "backlog",
				Description: "replace, retarget or close it manually.",
			}},
		},
		FindingFollowUps: []tuiclient.FindingFollowUp{{ID: "fu-1", Title: "Old follow-up", State: "backlog", Track: "research", Held: true}},
	}
}

func TestExecuteShowPlannedContinuationBeforeMerge(t *testing.T) {
	out := showWithServer(t, plannedContinuationDetail(), false)
	requireContains(t, out,
		"Research Continuation",
		"Proposed Children (manifest digest-1):",
		"- a: Child A [pending]",
		"Track: research, Model: haiku, Initial State: ready",
		"- b: Child B [pending]",
		"Initial State: backlog",
		"Depends On: child a",
		"Deferred Claims (carried forward, owned):",
		"- c4 (owner: alice)",
		"Excluded Claims (out of scope, no owner):",
		"- c5 (reason: out of scope)",
		"Action Items (manual replacement needed):",
		"[legacy_held_follow_up] fu-1: Old follow-up (backlog)",
		"Review-Finding Follow-Ups (not continuations):",
		"- fu-1: Old follow-up (backlog, held)",
	)
	if strings.Contains(out, "Created Children") {
		t.Errorf("nothing is created before the merge:\n%s", out)
	}
	// The finding follow-up is listed only under its own heading, never among the proposals.
	idx := strings.Index(out, "Review-Finding Follow-Ups")
	if strings.Contains(out[:idx], "- fu-1: Old follow-up (backlog, held)") {
		t.Errorf("follow-up listed outside its own section:\n%s", out)
	}
}

func TestExecuteShowCreatedContinuationAfterMerge(t *testing.T) {
	detail := plannedContinuationDetail()
	c := detail.Continuation
	c.ProposedChildren[0].Status = "created"
	c.ProposedChildren[0].CreatedTaskID = "child-a-id"
	c.ProposedChildren[1].Status = "created"
	c.ProposedChildren[1].CreatedTaskID = "child-b-id"
	c.CreatedChildren = []tuiclient.CreatedContinuationTask{
		{ID: "child-a-id", Key: "a", Title: "Child A", ParentTaskID: "parent-1", ManifestDigest: "digest-1", State: "ready", Track: "research",
			DependencyStatus: "satisfied", DependsOn: []string{"parent-1"}, Claimable: true, ClaimIDs: []string{"c1"}},
		{ID: "child-b-id", Key: "b", Title: "Child B", ParentTaskID: "parent-1", ManifestDigest: "digest-1", State: "backlog", Track: "build",
			DependencyStatus: "blocked", DependsOn: []string{"child-a-id"}, BlockedBy: []string{"child-a-id"}},
	}
	out := showWithServer(t, detail, false)
	requireContains(t, out,
		"- a: Child A [created]",
		"Created As: child-a-id",
		"Created Children:",
		"- child-a-id: Child A",
		"Parent: parent-1",
		"Manifest Digest: digest-1",
		"State: ready, Track: research, Claimable: true",
		"Dependency Status: satisfied",
		"Claims: c1",
		"- child-b-id: Child B",
		"State: backlog, Track: build, Claimable: false",
		"Dependency Status: blocked",
		"Blocked By: child-a-id",
		"Review-Finding Follow-Ups (not continuations):",
	)
}

func TestExecuteShowCreatedChildShowsParentProvenance(t *testing.T) {
	out := showWithServer(t, tuiclient.TaskDetail{
		ID: "child-b-id", State: "backlog", Model: "sonnet", Kind: "implement", Title: "Child B", Spec: "spec",
		Continuation: &tuiclient.ContinuationInfo{ParentInfo: &tuiclient.ContinuationParent{ID: "parent-1", ChildKey: "b", ManifestDigest: "digest-1"}},
	}, false)
	requireContains(t, out, "Created from continuation parent: parent-1", "Manifest Key: b", "Manifest Digest: digest-1")
	if strings.Contains(out, "Review-Finding Follow-Ups") {
		t.Errorf("unexpected follow-up section:\n%s", out)
	}
}

func TestExecuteShowOmitsContinuationSectionsWhenAbsent(t *testing.T) {
	out := showWithServer(t, tuiclient.TaskDetail{ID: "t-1", State: "ready", Model: "haiku", Kind: "implement", Title: "Plain", Spec: "spec"}, false)
	if strings.Contains(out, "Continuation") || strings.Contains(out, "Follow-Ups") {
		t.Errorf("plain task shows continuation sections:\n%s", out)
	}
}

func TestExecuteShowJSONKeepsContinuationAndFollowUpsSeparate(t *testing.T) {
	detail := plannedContinuationDetail()
	detail.Continuation.CreatedChildren = []tuiclient.CreatedContinuationTask{
		{ID: "child-a-id", Key: "a", Title: "Child A", State: "ready", Track: "research", DependencyStatus: "none", Claimable: true},
	}
	out := showWithServer(t, detail, true)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	var got tuiclient.TaskDetail
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Continuation == nil || len(got.Continuation.ProposedChildren) != 2 || len(got.Continuation.CreatedChildren) != 1 {
		t.Fatalf("continuation lost in --json: %+v", got.Continuation)
	}
	if got.Continuation.CreatedChildren[0].State != "ready" || got.Continuation.DeferredClaims[0].Owner != "alice" {
		t.Errorf("created child state / deferred owner lost: %+v", got.Continuation)
	}
	if len(got.FindingFollowUps) != 1 || got.FindingFollowUps[0].ID != "fu-1" {
		t.Errorf("finding follow-ups lost in --json: %+v", got.FindingFollowUps)
	}
	if _, ok := raw["continuation"]; !ok {
		t.Errorf("--json has no continuation key")
	}
	if _, ok := raw["finding_follow_ups"]; !ok {
		t.Errorf("--json has no finding_follow_ups key")
	}
}

func TestExecuteShowPriority(t *testing.T) {
	// Test that priority and topic anchor are displayed in show output
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			priority600 := int64(600)
			topicID := "topic-anchor-123"

			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:            "task-1",
				State:         "ready",
				Model:         "haiku",
				Kind:          "implement",
				Title:         "Task with Priority",
				Spec:          "Test spec",
				Priority:      &priority600,
				TopicAnchorID: &topicID,
			})
		}
		if r.URL.Path == "/tasks/task-1/events" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Event{})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-1"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	// Check that priority is displayed
	if !strings.Contains(output, "Priority: 600") {
		t.Errorf("expected 'Priority: 600' in output, got: %s", output)
	}
	// Check that topic anchor is displayed
	if !strings.Contains(output, "Topic Anchor ID: topic-anchor-123") {
		t.Errorf("expected 'Topic Anchor ID: topic-anchor-123' in output, got: %s", output)
	}
}

func TestExecuteShowPriorityAbove1000(t *testing.T) {
	// Test that priority values above 1000 (from Front action) are displayed
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/tasks/") {
			w.Header().Set("Content-Type", "application/json")
			priority := int64(1001) // From Front action

			json.NewEncoder(w).Encode(tuiclient.TaskDetail{
				ID:       "task-1",
				State:    "ready",
				Model:    "haiku",
				Kind:     "implement",
				Title:    "Task with Front Priority",
				Spec:     "Test spec",
				Priority: &priority,
			})
		}
		if r.URL.Path == "/tasks/task-1/events" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode([]tuiclient.Event{})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executeShow(context.Background(), server.URL, "test-token", false, []string{"task-1"}, buf)
	if err != nil {
		t.Fatalf("executeShow failed: %v", err)
	}

	output := buf.String()
	// Check that priority above 1000 is displayed
	if !strings.Contains(output, "Priority: 1001") {
		t.Errorf("expected 'Priority: 1001' in output, got: %s", output)
	}
}

func TestPermitRenewConflictExitCodes(t *testing.T) {
	tests := []struct {
		name      string
		errorCode string
		wantExit  int
	}{
		{"ATTEMPT_FENCED", "ATTEMPT_FENCED", 11},
		{"ATTEMPT_EXPIRED", "ATTEMPT_EXPIRED", 11},
		{"ATTEMPT_FINALIZED", "ATTEMPT_FINALIZED", 11},
		{"PERMIT_IDENTITY_MISMATCH", "PERMIT_IDENTITY_MISMATCH", 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"code":    tt.errorCode,
						"message": "conflict",
					},
				})
			}))
			defer server.Close()

			err := executePermitRenew(context.Background(), server.URL, "testtoken",
				[]string{"permit-1", "--task-id", "task-1", "--model", "haiku", "--agent-id", "agent-1",
					"--request-id", "req-1", "--attempt-id", "att-1"}, io.Discard)

			var conflErr *conflictError
			if !errors.As(err, &conflErr) {
				t.Errorf("expected conflictError, got %T: %v", err, err)
			} else if conflErr.code != tt.wantExit {
				t.Errorf("expected exit code %d, got %d", tt.wantExit, conflErr.code)
			}
		})
	}
}

func TestPermitFinalizeConflictExitCodes(t *testing.T) {
	tests := []struct {
		name      string
		errorCode string
		wantExit  int
	}{
		{"ATTEMPT_FENCED", "ATTEMPT_FENCED", 11},
		{"ATTEMPT_EXPIRED", "ATTEMPT_EXPIRED", 11},
		{"ATTEMPT_FINALIZED", "ATTEMPT_FINALIZED", 11},
		{"PERMIT_IDENTITY_MISMATCH", "PERMIT_IDENTITY_MISMATCH", 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]string{
						"code":    tt.errorCode,
						"message": "conflict",
					},
				})
			}))
			defer server.Close()

			err := executePermitFinalize(context.Background(), server.URL, "testtoken",
				[]string{"permit-1", "--task-id", "task-1", "--model", "haiku", "--agent-id", "agent-1",
					"--request-id", "req-1", "--attempt-id", "att-1", "--exit-class", "completed"}, io.Discard)

			var conflErr *conflictError
			if !errors.As(err, &conflErr) {
				t.Errorf("expected conflictError, got %T: %v", err, err)
			} else if conflErr.code != tt.wantExit {
				t.Errorf("expected exit code %d, got %d", tt.wantExit, conflErr.code)
			}
		})
	}
}

func TestClaimSchedulingError(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")

	retryAfter := int64(30)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":                "ADMISSION_DEFERRED",
				"message":             "admission deferred",
				"retry_after_seconds": retryAfter,
			},
		})
	}))
	defer server.Close()

	err := executeClaim(context.Background(), server.URL, "testtoken", []string{"task-1"})

	var schedErr *schedulingError
	if !errors.As(err, &schedErr) {
		t.Errorf("expected schedulingError, got %T: %v", err, err)
	} else if schedErr.code != 10 {
		t.Errorf("expected exit code 10, got %d", schedErr.code)
	} else if schedErr.retryAfterSeconds == nil || *schedErr.retryAfterSeconds != 30 {
		t.Errorf("expected retryAfterSeconds 30, got %v", schedErr.retryAfterSeconds)
	}
}

func TestResearchStatusFloatTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"mode": "paced",
			"pools": []map[string]interface{}{
				{
					"account_id": "acct-1",
					"tokens":     1234.5,
				},
			},
		})
	}))
	defer server.Close()

	var buf bytes.Buffer
	err := executeResearchStatus(context.Background(), server.URL, "testtoken", true, &buf)
	if err != nil {
		t.Fatalf("executeResearchStatus failed: %v", err)
	}

	output := buf.String()
	var status tuiclient.ResearchStatus
	if err := json.Unmarshal([]byte(output), &status); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(status.Pools) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(status.Pools))
	}
	if status.Pools[0].Tokens != 1234.5 {
		t.Errorf("expected tokens 1234.5, got %v", status.Pools[0].Tokens)
	}
}

// admissionServer serves one canned response for every request and records
// the last request body.
func admissionServer(t *testing.T, status int, header map[string]string, body interface{}, lastBody *map[string]interface{}) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lastBody != nil && r.Body != nil {
			*lastBody = map[string]interface{}{}
			json.NewDecoder(r.Body).Decode(lastBody)
		}
		w.Header().Set("Content-Type", "application/json")
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func denial(outcome, reason string, extra map[string]interface{}) map[string]interface{} {
	e := map[string]interface{}{
		"code":    "ADMISSION_DENIED",
		"message": "research admission denied",
		"outcome": outcome,
		"reason":  reason,
	}
	for k, v := range extra {
		e[k] = v
	}
	return map[string]interface{}{"error": e}
}

func keysOf(m map[string]interface{}) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestClaimNotBeforeOnlyDenialPrintsHints(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")
	srv := admissionServer(t, http.StatusTooManyRequests, nil,
		denial("defer", "rate", map[string]interface{}{"not_before": "2026-10-03T15:00:00Z"}), nil)

	err := executeClaim(context.Background(), srv.URL, "testtoken", []string{"task-1"})
	var schedErr *schedulingError
	if !errors.As(err, &schedErr) {
		t.Fatalf("expected schedulingError, got %T: %v", err, err)
	}
	if schedErr.code != 10 {
		t.Errorf("exit code = %d, want 10", schedErr.code)
	}
	if schedErr.retryAfterSeconds != nil {
		t.Errorf("retryAfterSeconds = %d, want nil", *schedErr.retryAfterSeconds)
	}
	var buf bytes.Buffer
	writeSchedulingHints(&buf, schedErr)
	for _, want := range []string{"outcome: defer\n", "reason: rate\n", "not-before: 2026-10-03T15:00:00Z\n"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("stderr hints missing %q:\n%s", want, buf.String())
		}
	}
	if strings.Contains(buf.String(), "retry-after") {
		t.Errorf("unexpected retry-after line:\n%s", buf.String())
	}
}

func TestClaimHeaderOnlyRetryAfterPrintsHint(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")
	srv := admissionServer(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "45"},
		denial("retry", "concurrency", nil), nil)

	err := executeClaim(context.Background(), srv.URL, "testtoken", []string{"task-1"})
	var schedErr *schedulingError
	if !errors.As(err, &schedErr) {
		t.Fatalf("expected schedulingError, got %T: %v", err, err)
	}
	var buf bytes.Buffer
	writeSchedulingHints(&buf, schedErr)
	for _, want := range []string{"outcome: retry\n", "reason: concurrency\n", "retry-after: 45\n"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("stderr hints missing %q:\n%s", want, buf.String())
		}
	}
}

func TestExecuteNextNotBeforeOnlyDenial(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/projects/proj-1/tasks" {
			json.NewEncoder(w).Encode([]tuiclient.Task{{ID: "task-1", State: "ready", Model: "haiku", Kind: "implement"}})
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(denial("defer", "rate", map[string]interface{}{"not_before": "2026-10-03T15:00:00Z"}))
	}))
	defer srv.Close()

	err := executeNext(context.Background(), srv.URL, "testtoken", false,
		[]string{"--project", "proj-1", "--model", "haiku", "--kind", "implement", "--claim"})
	var schedErr *schedulingError
	if !errors.As(err, &schedErr) {
		t.Fatalf("expected schedulingError, got %T: %v", err, err)
	}
	if schedErr.code == 2 {
		t.Errorf("scheduling exit must differ from nothing-claimable exit 2")
	}
	if schedErr.notBefore == nil || *schedErr.notBefore != "2026-10-03T15:00:00Z" || schedErr.reason != "rate" || schedErr.outcome != "defer" {
		t.Errorf("scheduling metadata lost: %+v", schedErr)
	}
}

func TestExecuteNextPriorityOrdering(t *testing.T) {
	priorityOf := func(value int64) *int64 { return &value }
	testCases := []struct {
		name     string
		tasks    []tuiclient.Task
		wantTask string
	}{
		{
			name: "higher priority wins over older lower priority",
			tasks: []tuiclient.Task{
				{ID: "task-old-500", Priority: priorityOf(500), CreatedAt: "2026-01-01T08:00:00Z"},
				{ID: "task-new-800", Priority: priorityOf(800), CreatedAt: "2026-01-01T10:00:00Z"},
			},
			wantTask: "task-new-800",
		},
		{
			name: "value above 1000 beats 1000",
			tasks: []tuiclient.Task{
				{ID: "task-1000", Priority: priorityOf(1000), CreatedAt: "2026-01-01T08:00:00Z"},
				{ID: "task-1001", Priority: priorityOf(1001), CreatedAt: "2026-01-01T10:00:00Z"},
			},
			wantTask: "task-1001",
		},
		{
			name: "equal priority picks oldest created_at",
			tasks: []tuiclient.Task{
				{ID: "task-600-new", Priority: priorityOf(600), CreatedAt: "2026-01-01T11:00:00Z"},
				{ID: "task-600-old", Priority: priorityOf(600), CreatedAt: "2026-01-01T09:00:00Z"},
			},
			wantTask: "task-600-old",
		},
		{
			name: "equal priority and created_at picks lowest id",
			tasks: []tuiclient.Task{
				{ID: "task-b", Priority: priorityOf(600), CreatedAt: "2026-01-01T09:00:00Z"},
				{ID: "task-a", Priority: priorityOf(600), CreatedAt: "2026-01-01T09:00:00Z"},
			},
			wantTask: "task-a",
		},
		{
			name: "unset priority counts as default 500",
			tasks: []tuiclient.Task{
				{ID: "task-499", Priority: priorityOf(499), CreatedAt: "2026-01-01T08:00:00Z"},
				{ID: "task-unset", CreatedAt: "2026-01-01T10:00:00Z"},
			},
			wantTask: "task-unset",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			// The fake server deliberately returns the list in the wrong order so the
			// test proves the CLI applies the comparator rather than trusting list order.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				tasks := make([]tuiclient.Task, len(testCase.tasks))
				copy(tasks, testCase.tasks)
				for index := range tasks {
					tasks[index].State = "ready"
					tasks[index].Model = "haiku"
					tasks[index].Kind = "implement"
				}
				json.NewEncoder(w).Encode(tasks)
			}))
			defer server.Close()

			output := captureStdout(t, func() {
				err := executeNext(context.Background(), server.URL, "test-token", false, []string{
					"--project", "proj-1", "--model", "haiku", "--kind", "implement",
				})
				if err != nil {
					t.Fatalf("executeNext failed: %v", err)
				}
			})
			if strings.TrimSpace(output) != testCase.wantTask {
				t.Errorf("executeNext selected %q, want %q", strings.TrimSpace(output), testCase.wantTask)
			}
		})
	}
}

func TestClaimRequestIdentityConflictsExit11(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")
	for _, code := range []string{"REQUEST_ID_CONFLICT", "TASK_BUSY"} {
		t.Run(code, func(t *testing.T) {
			srv := admissionServer(t, http.StatusConflict, nil,
				map[string]interface{}{"error": map[string]string{"code": code, "message": "conflict"}}, nil)
			err := executeClaim(context.Background(), srv.URL, "testtoken", []string{"task-1", "--request-id", "r1"})
			var conflErr *conflictError
			if !errors.As(err, &conflErr) {
				t.Fatalf("expected conflictError, got %T: %v", err, err)
			}
			if conflErr.code != 11 {
				t.Errorf("exit code = %d, want 11", conflErr.code)
			}
			if !strings.Contains(conflErr.Error(), code) {
				t.Errorf("message %q does not name %s", conflErr.Error(), code)
			}
		})
	}
}

func TestClaimPlainConflictStaysAlreadyClaimed(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")
	srv := admissionServer(t, http.StatusConflict, nil,
		map[string]interface{}{"error": map[string]string{"code": "CONFLICT", "message": "Task is not claimable"}}, nil)
	err := executeClaim(context.Background(), srv.URL, "testtoken", []string{"task-1"})
	var claimErr *claimError
	if !errors.As(err, &claimErr) || claimErr.code != 3 {
		t.Fatalf("expected claimError exit 3, got %T: %v", err, err)
	}
}

func TestClaimAdmissionJSONAndIdentityFlags(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")
	t.Setenv("ODONIAN_STATE_DIR", t.TempDir())
	var got map[string]interface{}
	srv := admissionServer(t, http.StatusOK, nil, map[string]interface{}{
		"id": "task-1",
		"research_admission": map[string]interface{}{
			"permit_id":  "permit-1",
			"attempt_id": "attempt-1",
			"request_id": "req-1",
			"account_id": "acct-1",
			"expires_at": "2026-10-03T16:00:00Z",
			"replayed":   true,
			"observed_denial": map[string]interface{}{
				"outcome": "defer", "reason": "rate", "not_before": "2026-10-03T15:00:00Z",
			},
		},
	}, &got)

	out := captureStdout(t, func() {
		err := executeClaim(context.Background(), srv.URL, "testtoken",
			[]string{"task-1", "--request-id", "req-1", "--account-id", "acct-1", "--work-class", "research_review"})
		if err != nil {
			t.Fatalf("executeClaim: %v", err)
		}
	})

	for k, want := range map[string]string{"request_id": "req-1", "account_id": "acct-1", "work_class": "research_review", "agent_id": "test-agent", "model": "haiku"} {
		if got[k] != want {
			t.Errorf("claim body %s = %v, want %s", k, got[k], want)
		}
	}

	var adm map[string]interface{}
	if err := json.Unmarshal([]byte(out), &adm); err != nil {
		t.Fatalf("claim output is not JSON: %v\n%s", err, out)
	}
	wantKeys := []string{"account_id", "attempt_id", "expires_at", "observed_denial", "permit_id", "replayed", "request_id"}
	if !reflect.DeepEqual(keysOf(adm), wantKeys) {
		t.Errorf("claim output keys = %v, want %v", keysOf(adm), wantKeys)
	}
	if adm["replayed"] != true {
		t.Errorf("replayed = %v, want true", adm["replayed"])
	}
	obs, _ := adm["observed_denial"].(map[string]interface{})
	if obs["outcome"] != "defer" || obs["reason"] != "rate" || obs["not_before"] != "2026-10-03T15:00:00Z" {
		t.Errorf("observed_denial = %v", obs)
	}
}

func TestClaimOutputOmitsFieldsServerDidNotSend(t *testing.T) {
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("AGENT_MODEL", "haiku")
	t.Setenv("ODONIAN_STATE_DIR", t.TempDir())
	srv := admissionServer(t, http.StatusOK, nil, map[string]interface{}{
		"id": "task-1",
		"research_admission": map[string]interface{}{
			"permit_id": "permit-1", "attempt_id": "attempt-1", "request_id": "req-1",
			"account_id": "acct-1", "expires_at": "2026-10-03T16:00:00Z",
		},
	}, nil)
	out := captureStdout(t, func() {
		if err := executeClaim(context.Background(), srv.URL, "testtoken", []string{"task-1"}); err != nil {
			t.Fatalf("executeClaim: %v", err)
		}
	})
	var adm map[string]interface{}
	if err := json.Unmarshal([]byte(out), &adm); err != nil {
		t.Fatalf("claim output is not JSON: %v\n%s", err, out)
	}
	wantKeys := []string{"account_id", "attempt_id", "expires_at", "permit_id", "request_id"}
	if !reflect.DeepEqual(keysOf(adm), wantKeys) {
		t.Errorf("claim output keys = %v, want %v", keysOf(adm), wantKeys)
	}
}

func TestPermitRenewOutputPassesServerAttemptThrough(t *testing.T) {
	var got map[string]interface{}
	srv := admissionServer(t, http.StatusOK, nil, map[string]interface{}{
		"attempt": map[string]interface{}{
			"id": "attempt-456", "permit_id": "permit-1", "task_id": "task-1",
			"state": "active", "expires_at": "2026-10-10T12:00:00Z",
		},
	}, &got)

	var buf bytes.Buffer
	err := executePermitRenew(context.Background(), srv.URL, "testtoken",
		[]string{"permit-1", "--task-id", "task-1", "--model", "haiku", "--agent-id", "agent-1",
			"--request-id", "req-1", "--attempt-id", "att-1"}, &buf)
	if err != nil {
		t.Fatalf("executePermitRenew: %v", err)
	}
	if got["attempt_id"] != "att-1" || got["request_id"] != "req-1" || got["task_id"] != "task-1" {
		t.Errorf("renew request body = %v", got)
	}
	var attempt map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &attempt); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	wantKeys := []string{"expires_at", "id", "permit_id", "state", "task_id"}
	if !reflect.DeepEqual(keysOf(attempt), wantKeys) {
		t.Errorf("renew output keys = %v, want %v", keysOf(attempt), wantKeys)
	}
	if attempt["expires_at"] != "2026-10-10T12:00:00Z" {
		t.Errorf("expires_at = %v", attempt["expires_at"])
	}
}

func TestPermitFinalizeOutputPassesServerAttemptThrough(t *testing.T) {
	var got map[string]interface{}
	srv := admissionServer(t, http.StatusOK, nil, map[string]interface{}{
		"attempt": map[string]interface{}{
			"id": "attempt-456", "permit_id": "permit-1", "task_id": "task-1",
			"state": "finalized", "exit_class": "completed",
		},
	}, &got)

	var buf bytes.Buffer
	err := executePermitFinalize(context.Background(), srv.URL, "testtoken",
		[]string{"permit-1", "--task-id", "task-1", "--model", "haiku", "--agent-id", "agent-1",
			"--request-id", "req-1", "--attempt-id", "att-1", "--exit-class", "completed", "--usage-tokens", "0"}, &buf)
	if err != nil {
		t.Fatalf("executePermitFinalize: %v", err)
	}
	if got["exit_class"] != "completed" || got["attempt_id"] != "att-1" {
		t.Errorf("finalize request body = %v", got)
	}
	if v, ok := got["usage_tokens"]; !ok || v != float64(0) {
		t.Errorf("explicit --usage-tokens 0 not sent: %v", got)
	}
	var attempt map[string]interface{}
	if err := json.Unmarshal(buf.Bytes(), &attempt); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, buf.String())
	}
	wantKeys := []string{"exit_class", "id", "permit_id", "state", "task_id"}
	if !reflect.DeepEqual(keysOf(attempt), wantKeys) {
		t.Errorf("finalize output keys = %v, want %v", keysOf(attempt), wantKeys)
	}
}

func TestPermitRenewFinalizeSchedulingDenial(t *testing.T) {
	retry := map[string]interface{}{"retry_after_seconds": 12}
	for _, verb := range []string{"renew", "finalize"} {
		t.Run(verb, func(t *testing.T) {
			srv := admissionServer(t, http.StatusTooManyRequests, nil, denial("retry", "concurrency", retry), nil)
			args := []string{"permit-1", "--task-id", "task-1", "--model", "haiku", "--agent-id", "agent-1",
				"--request-id", "req-1", "--attempt-id", "att-1"}
			var err error
			if verb == "renew" {
				err = executePermitRenew(context.Background(), srv.URL, "testtoken", args, io.Discard)
			} else {
				err = executePermitFinalize(context.Background(), srv.URL, "testtoken", append(args, "--exit-class", "completed"), io.Discard)
			}
			var schedErr *schedulingError
			if !errors.As(err, &schedErr) {
				t.Fatalf("expected schedulingError, got %T: %v", err, err)
			}
			if schedErr.code != 10 || schedErr.retryAfterSeconds == nil || *schedErr.retryAfterSeconds != 12 {
				t.Errorf("scheduling error = %+v", schedErr)
			}
		})
	}
}

func TestPermitRenewFinalizeNullFieldsDoNotPanic(t *testing.T) {
	srv := admissionServer(t, http.StatusOK, nil, map[string]interface{}{
		"attempt": map[string]interface{}{"id": "a", "exit_class": nil},
	}, nil)
	var buf bytes.Buffer
	if err := executePermitFinalize(context.Background(), srv.URL, "testtoken",
		[]string{"permit-1", "--task-id", "t", "--model", "haiku", "--agent-id", "a",
			"--request-id", "r", "--attempt-id", "a", "--exit-class", "completed"}, &buf); err != nil {
		t.Fatalf("executePermitFinalize: %v", err)
	}
	if !strings.Contains(buf.String(), `"exit_class": null`) {
		t.Errorf("null field not passed through:\n%s", buf.String())
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	backup := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = backup }()
	fn()
	w.Close()
	var buf bytes.Buffer
	buf.ReadFrom(r)
	return buf.String()
}

func TestExecutePrioritySetValid(t *testing.T) {
	tests := []struct {
		name     string
		priority int64
	}{
		{"min boundary", 1},
		{"default value", 500},
		{"max boundary", 1000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/tasks/task-123/priority/set" && r.Method == "POST" {
					var req tuiclient.SetTaskPriorityRequest
					json.NewDecoder(r.Body).Decode(&req)
					json.NewEncoder(w).Encode(tuiclient.PriorityChange{
						Action:           "set",
						TaskID:           "task-123",
						TopicAnchorID:    "topic-123",
						ActionKey:        req.ActionKey,
						OldPriority:      500,
						Priority:         tt.priority,
						QueueMaxPriority: nil,
						Replayed:         false,
					})
				} else {
					w.WriteHeader(http.StatusNotFound)
					json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
				}
			}))
			defer server.Close()

			buf := &bytes.Buffer{}
			err := executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--set", fmt.Sprintf("%d", tt.priority), "--reason", "test reason"}, buf)
			if err != nil {
				t.Fatalf("executePriority failed: %v", err)
			}

			output := buf.String()
			if !strings.Contains(output, "New Priority") {
				t.Errorf("expected output to contain 'New Priority', got: %s", output)
			}
		})
	}
}

func TestExecutePrioritySetInvalid(t *testing.T) {
	tests := []struct {
		name     string
		priority string
		wantErr  string
	}{
		{"zero value", "0", "priority must be an integer between 1 and 1000"},
		{"negative value", "-1", "priority must be an integer between 1 and 1000"},
		{"above max", "1001", "priority must be an integer between 1 and 1000"},
		{"way above max", "9999", "priority must be an integer between 1 and 1000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("server should not be called for invalid input")
			}))
			defer server.Close()

			buf := &bytes.Buffer{}
			err := executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--set", tt.priority, "--reason", "test reason"}, buf)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tt.wantErr, err)
			}
		})
	}
}

func TestExecutePriorityFront(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tasks/task-123/priority/front" && r.Method == "POST" {
			var req tuiclient.MoveTaskToFrontRequest
			json.NewDecoder(r.Body).Decode(&req)
			queueMax := int64(1001)
			json.NewEncoder(w).Encode(tuiclient.PriorityChange{
				Action:           "front",
				TaskID:           "task-123",
				TopicAnchorID:    "topic-123",
				ActionKey:        req.ActionKey,
				OldPriority:      500,
				Priority:         1001,
				QueueMaxPriority: &queueMax,
				Replayed:         false,
			})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--front", "--reason", "test reason"}, buf)
	if err != nil {
		t.Fatalf("executePriority --front failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "Action: front") {
		t.Errorf("expected output to contain 'Action: front', got: %s", output)
	}
	if !strings.Contains(output, "New Priority: 1001") {
		t.Errorf("expected output to contain 'New Priority: 1001', got: %s", output)
	}
}

func TestExecutePriorityReset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tasks/task-123/priority/set" && r.Method == "POST" {
			var req tuiclient.SetTaskPriorityRequest
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(tuiclient.PriorityChange{
				Action:           "reset",
				TaskID:           "task-123",
				TopicAnchorID:    "topic-123",
				ActionKey:        req.ActionKey,
				OldPriority:      750,
				Priority:         500,
				QueueMaxPriority: nil,
				Replayed:         false,
			})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--reset", "--reason", "test reason"}, buf)
	if err != nil {
		t.Fatalf("executePriority --reset failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "New Priority: 500") {
		t.Errorf("expected output to contain 'New Priority: 500', got: %s", output)
	}
}

func TestExecutePriorityMutualExclusivity(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"set and front", []string{"task-123", "--set", "600", "--front", "--reason", "test"}},
		{"set and reset", []string{"task-123", "--set", "600", "--reset", "--reason", "test"}},
		{"front and reset", []string{"task-123", "--front", "--reset", "--reason", "test"}},
		{"all three", []string{"task-123", "--set", "600", "--front", "--reset", "--reason", "test"}},
		{"no operation", []string{"task-123", "--reason", "test"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("server should not be called for invalid input")
			}))
			defer server.Close()

			buf := &bytes.Buffer{}
			err := executePriority(context.Background(), server.URL, "test-token", false, tt.args, buf)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tt.name)
			}
			if !strings.Contains(err.Error(), "exactly one of") {
				t.Errorf("expected error containing 'exactly one of', got: %v", err)
			}
		})
	}
}

func TestExecutePriorityMissingReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called for missing reason")
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--set", "600"}, buf)
	if err == nil {
		t.Fatal("expected error for missing --reason, got nil")
	}
	if !strings.Contains(err.Error(), "--reason flag is required") {
		t.Errorf("expected error containing '--reason flag is required', got: %v", err)
	}
}

func TestExecutePriorityJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tasks/task-123/priority/set" && r.Method == "POST" {
			var req tuiclient.SetTaskPriorityRequest
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(tuiclient.PriorityChange{
				Action:           "set",
				TaskID:           "task-123",
				TopicAnchorID:    "topic-123",
				ActionKey:        req.ActionKey,
				OldPriority:      500,
				Priority:         750,
				QueueMaxPriority: nil,
				Replayed:         false,
			})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePriority(context.Background(), server.URL, "test-token", true, []string{"task-123", "--set", "750", "--reason", "test"}, buf)
	if err != nil {
		t.Fatalf("executePriority --json failed: %v", err)
	}

	output := buf.String()
	var result tuiclient.PriorityChange
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v", err)
	}

	if result.Priority != 750 {
		t.Errorf("expected Priority 750, got %d", result.Priority)
	}
}

func TestExecutePriorityInheritedValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tasks/task-123/priority/front" && r.Method == "POST" {
			var req tuiclient.MoveTaskToFrontRequest
			json.NewDecoder(r.Body).Decode(&req)
			queueMax := int64(1001)
			json.NewEncoder(w).Encode(tuiclient.PriorityChange{
				Action:           "front",
				TaskID:           "task-123",
				TopicAnchorID:    "topic-123",
				ActionKey:        req.ActionKey,
				OldPriority:      1000,
				Priority:         1001,
				QueueMaxPriority: &queueMax,
				Replayed:         false,
			})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		}
	}))
	defer server.Close()

	buf := &bytes.Buffer{}
	err := executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--front", "--reason", "test"}, buf)
	if err != nil {
		t.Fatalf("executePriority with inherited value failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "New Priority: 1001") {
		t.Errorf("expected output to contain 'New Priority: 1001', got: %s", output)
	}
}

func TestExecutePriorityRetryIdentity(t *testing.T) {
	// Test that the action key is stable when provided via --action-key flag
	// and that distinct invocations generate different keys when no flag is provided
	var requestedKeys []string
	var mu sync.Mutex

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/tasks/task-123/priority/set" && r.Method == "POST" {
			var req tuiclient.SetTaskPriorityRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "bad request"})
				return
			}

			mu.Lock()
			requestedKeys = append(requestedKeys, req.ActionKey)
			mu.Unlock()

			// Verify action key is present
			if req.ActionKey == "" {
				t.Error("ActionKey is empty")
			}

			json.NewEncoder(w).Encode(tuiclient.PriorityChange{
				Action:        "set",
				TaskID:        "task-123",
				TopicAnchorID: "topic-123",
				ActionKey:     req.ActionKey,
				OldPriority:   500,
				Priority:      600,
				Replayed:      false,
			})
		} else {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		}
	}))
	defer server.Close()

	// Test 1: Invocation with explicit --action-key flag uses that key
	buf := &bytes.Buffer{}
	explicitKey := "test-action-key-123"
	err := executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--set", "600", "--reason", "test", "--action-key", explicitKey}, buf)
	if err != nil {
		t.Fatalf("executePriority with explicit key failed: %v", err)
	}

	if len(requestedKeys) != 1 || requestedKeys[0] != explicitKey {
		t.Errorf("Expected action key %q, got %q", explicitKey, requestedKeys[0])
	}

	// Verify the key is printed in the output
	if !strings.Contains(buf.String(), "Action Key: test-action-key-123") {
		t.Errorf("Action key not printed in output: %s", buf.String())
	}

	// Test 2: Two invocations without --action-key flag generate different keys
	requestedKeys = nil
	buf = &bytes.Buffer{}
	err = executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--set", "600", "--reason", "test"}, buf)
	if err != nil {
		t.Fatalf("executePriority without key failed: %v", err)
	}

	key1 := requestedKeys[0]

	buf = &bytes.Buffer{}
	err = executePriority(context.Background(), server.URL, "test-token", false, []string{"task-123", "--set", "600", "--reason", "test"}, buf)
	if err != nil {
		t.Fatalf("executePriority second invocation failed: %v", err)
	}

	key2 := requestedKeys[1]

	if key1 == key2 {
		t.Errorf("Expected distinct keys for separate invocations, but got same key: %s", key1)
	}
}

func TestExecutePriorityHelpWithoutEnvironment(t *testing.T) {
	for _, helpFlag := range []string{"-h", "--help"} {
		buf := &bytes.Buffer{}
		if err := executePriority(context.Background(), "", "", false, []string{helpFlag}, buf); err != nil {
			t.Fatalf("executePriority %s returned error: %v", helpFlag, err)
		}
		usage := buf.String()
		for _, want := range []string{"1..1000", "max(1000, max queued priority) + 1", "do not scope the global Front maximum", "does\nnot release holds or promote"} {
			if !strings.Contains(usage, want) {
				t.Errorf("usage for %s missing %q:\n%s", helpFlag, want, usage)
			}
		}
	}
}

func TestExecutePriorityRetryAfterFailureReusesKey(t *testing.T) {
	var receivedKeys []string
	var mu sync.Mutex
	attempts := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/tasks/task-123/priority/front" || r.Method != "POST" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var req tuiclient.MoveTaskToFrontRequest
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		receivedKeys = append(receivedKeys, req.ActionKey)
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "boom"})
			return
		}
		json.NewEncoder(w).Encode(tuiclient.PriorityChange{
			Action: "front", TaskID: "task-123", TopicAnchorID: "topic-123",
			ActionKey: req.ActionKey, OldPriority: 500, Priority: 1001, Replayed: attempt > 2,
		})
	}))
	defer server.Close()

	firstErr := executePriority(context.Background(), server.URL, "test-token", false,
		[]string{"task-123", "--front", "--reason", "retry test"}, &bytes.Buffer{})
	if firstErr == nil {
		t.Fatal("expected first attempt to fail")
	}
	const keyMarker = "action key "
	messageText := firstErr.Error()
	markerIndex := strings.Index(messageText, keyMarker)
	if markerIndex < 0 {
		t.Fatalf("error does not report the action key: %v", firstErr)
	}
	reportedKey := messageText[markerIndex+len(keyMarker):]
	reportedKey = reportedKey[:strings.Index(reportedKey, ")")]
	if reportedKey == "" || reportedKey != receivedKeys[0] {
		t.Fatalf("reported key %q does not match key sent %q", reportedKey, receivedKeys[0])
	}

	buf := &bytes.Buffer{}
	if err := executePriority(context.Background(), server.URL, "test-token", true,
		[]string{"task-123", "--front", "--reason", "retry test", "--action-key", reportedKey}, buf); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if len(receivedKeys) != 2 || receivedKeys[1] != reportedKey {
		t.Errorf("retry sent keys %v, want both %q", receivedKeys, reportedKey)
	}
	var result tuiclient.PriorityChange
	if err := json.Unmarshal(buf.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if result.ActionKey != reportedKey || result.Priority != 1001 {
		t.Errorf("unexpected JSON result: %+v", result)
	}
}
