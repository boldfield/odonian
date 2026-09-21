package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/forge"
)

// odonianTaskServer returns a mock odonian server serving GET /tasks/{id} with the given
// review round and pr link, and accepting POST /tasks/{id}/submit unconditionally.
func odonianTaskServer(t *testing.T, taskID string, reviewRound int, prLink string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tasks/"+taskID:
			links := []map[string]string{}
			if prLink != "" {
				links = append(links, map[string]string{"kind": "pr", "value": prLink})
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":           taskID,
				"kind":         "implement",
				"title":        "test task",
				"review_round": reviewRound,
				"links":        links,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/tasks/"+taskID+"/submit":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// githubFeedbackServer returns a mock GitHub API serving /user and /graphql. If item is
// non-empty it is returned as a single unaddressed global comment; otherwise both queries
// return empty results.
func githubFeedbackServer(t *testing.T, itemBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"login": "test-bot"})
		case "/graphql":
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			query := body["query"]
			switch {
			case strings.Contains(query, "reviewThreads"):
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{"hasNextPage": false},
									"nodes":    []map[string]interface{}{},
								},
							},
						},
					},
				})
			case strings.Contains(query, "comments(first:"):
				nodes := []map[string]interface{}{}
				if itemBody != "" {
					nodes = append(nodes, map[string]interface{}{
						"id":             "global-comment-1",
						"databaseId":     123,
						"body":           itemBody,
						"createdAt":      "2026-01-01T00:00:00Z",
						"author":         map[string]string{"login": "reviewer"},
						"reactionGroups": []interface{}{},
					})
				}
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{"hasNextPage": false},
									"nodes":    nodes,
								},
							},
						},
					},
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// githubIncidentReplayServer models PR#34 with:
// - one resolved/acknowledged inline thread
// - an outstanding marked global rejection
// - another reviewer's approval (non-actionable)
func githubIncidentReplayServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"login": "test-bot"})
		case "/graphql":
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			query := body["query"]
			switch {
			case strings.Contains(query, "reviewThreads"):
				// One resolved inline thread (acknowledged)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{"hasNextPage": false},
									"nodes": []map[string]interface{}{
										{
											"id":         "thread-1",
											"isResolved": true,
											"path":       "file.go",
											"line":       42,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":     "comment-1",
														"body":   "please fix this",
														"author": map[string]string{"login": "reviewer"},
													},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":     "comment-1-reply",
														"body":   "haiku-worker: addressed in abc123 (see comment thread-1)",
														"author": map[string]string{"login": "test-bot"},
													},
												},
											},
										},
									},
								},
							},
						},
					},
				})
			case strings.Contains(query, "comments(first:"):
				// Outstanding marked global rejection + approval
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{"hasNextPage": false},
									"nodes": []map[string]interface{}{
										{
											"id":             "global-rejection-1",
											"databaseId":     123,
											"body":           "gpt-5.5-reviewer: CHANGES REQUESTED\n\nThis needs work.",
											"createdAt":      "2026-01-01T10:00:00Z",
											"author":         map[string]string{"login": "gpt-reviewer"},
											"reactionGroups": []interface{}{},
										},
										{
											"id":             "global-approval-1",
											"databaseId":     124,
											"body":           "opus-reviewer: APPROVED",
											"createdAt":      "2026-01-01T11:00:00Z",
											"author":         map[string]string{"login": "opus-reviewer"},
											"reactionGroups": []interface{}{},
										},
									},
								},
							},
						},
					},
				})
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestExecuteSubmitFeedbackGateBlocksWithRemainingItems(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"

	odonianServer := odonianTaskServer(t, "task123", 1, prURL)
	defer odonianServer.Close()

	ghServer := githubFeedbackServer(t, "please fix this")
	defer ghServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = ghServer.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"--pr", prURL,
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err == nil {
		t.Fatal("expected executeSubmit to be blocked by the pr-feedback gate, got nil error")
	}
	if !strings.Contains(err.Error(), "pr-feedback ack") {
		t.Errorf("expected error to point at pr-feedback ack, got: %v", err)
	}
}

func TestExecuteSubmitFeedbackGateCleanPass(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"

	odonianServer := odonianTaskServer(t, "task123", 1, prURL)
	defer odonianServer.Close()

	ghServer := githubFeedbackServer(t, "")
	defer ghServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = ghServer.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"--pr", prURL,
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err != nil {
		t.Fatalf("expected executeSubmit to pass with no unaddressed feedback, got: %v", err)
	}
}

func TestExecuteSubmitFeedbackGateBypassFlag(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"

	odonianServer := odonianTaskServer(t, "task123", 1, prURL)
	defer odonianServer.Close()

	// A real GitHub mock with an outstanding item, and a working token: without
	// --skip-feedback-gate this setup blocks the submit (see
	// TestExecuteSubmitFeedbackGateBlocksWithRemainingItems). Passing here proves the flag
	// genuinely bypasses the check rather than coincidentally hitting the
	// check-error-proceeds path.
	ghServer := githubFeedbackServer(t, "please fix this")
	defer ghServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = ghServer.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")

	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("failed to create pipe: %v", pipeErr)
	}
	oldStderr := os.Stderr
	os.Stderr = w

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"--pr", prURL,
		"--branch", "mr/a1b2c3d4",
		"--skip-feedback-gate",
		"task123",
	})

	os.Stderr = oldStderr
	w.Close()
	var stderr bytes.Buffer
	io.Copy(&stderr, r)

	if err != nil {
		t.Fatalf("expected --skip-feedback-gate to bypass the check and submit to succeed, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("expected a loud warning on bypass, got stderr: %q", stderr.String())
	}
}

func TestExecuteSubmitFeedbackGateLookupFailureBlocks(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"

	odonianServer := odonianTaskServer(t, "task123", 1, prURL)
	defer odonianServer.Close()

	// No GH token available anywhere, so the feedback check itself fails (not a "found
	// items" result) — for a rework with review_round > 0, this must block the submit
	// with an explicit error, not warn and proceed.
	oldToken := os.Getenv("GH_TOKEN")
	os.Unsetenv("GH_TOKEN")
	defer func() {
		if oldToken != "" {
			os.Setenv("GH_TOKEN", oldToken)
		}
	}()
	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"--pr", prURL,
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err == nil {
		t.Fatal("expected executeSubmit to be blocked by lookup failure on a rework, got nil error")
	}
	if !strings.Contains(err.Error(), "could not retrieve") && !strings.Contains(err.Error(), "retry") {
		t.Errorf("expected error to indicate retrieval failure requiring retry, got: %v", err)
	}
}

func TestExecuteSubmitIncidentReplayOutstandingRejectionBlocks(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"

	odonianServer := odonianTaskServer(t, "task123", 1, prURL)
	defer odonianServer.Close()

	ghServer := githubIncidentReplayServer(t)
	defer ghServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = ghServer.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"--pr", prURL,
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err == nil {
		t.Fatal("expected executeSubmit to be blocked by the unacknowledged rejection")
	}
	// The error message should contain the pr-feedback ack instruction
	if !strings.Contains(err.Error(), "pr-feedback ack") {
		t.Errorf("expected error to mention pr-feedback ack, got: %v", err)
	}
}

func TestExecuteSubmitLookupFailureAllowsInitialSubmission(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"

	odonianServer := odonianTaskServer(t, "task123", 0, prURL) // review_round = 0 (initial)
	defer odonianServer.Close()

	// No GH token available, so lookup fails — but review_round = 0, so it's an initial
	// submission and the gate should not apply. Submit should proceed.
	oldToken := os.Getenv("GH_TOKEN")
	os.Unsetenv("GH_TOKEN")
	defer func() {
		if oldToken != "" {
			os.Setenv("GH_TOKEN", oldToken)
		}
	}()
	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "implemented",
		"--pr", prURL,
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err != nil {
		t.Fatalf("expected executeSubmit to allow initial submission (review_round=0) despite lookup failure, got: %v", err)
	}
}
