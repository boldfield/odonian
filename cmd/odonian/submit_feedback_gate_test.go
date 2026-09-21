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
	"sync/atomic"
	"testing"

	"github.com/boldfield/odonian/internal/forge"
)

// submitTracker records whether POST /tasks/{id}/submit was ever hit, so blocked and
// failed-lookup tests can assert the gate withheld the request rather than merely
// asserting that executeSubmit returned an error — a regression that fires the POST and
// then returns an error would otherwise still pass.
type submitTracker struct {
	hit atomic.Bool
}

// odonianTaskServer returns a mock odonian server serving GET /tasks/{id} with the given
// review round and pr link, and recording POST /tasks/{id}/submit hits into tracker.
func odonianTaskServer(t *testing.T, taskID string, reviewRound int, prLink string, tracker *submitTracker) *httptest.Server {
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
			if tracker != nil {
				tracker.hit.Store(true)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// failingGetOdonianTaskServer models a task-fetch failure: GET /tasks/{id} always 500s.
// POST hits are still recorded so tests can assert the submit never reached the server.
func failingGetOdonianTaskServer(t *testing.T, taskID string, tracker *submitTracker) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tasks/"+taskID:
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/tasks/"+taskID+"/submit":
			if tracker != nil {
				tracker.hit.Store(true)
			}
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

// githubGraphQLFailureServer models a feedback-lookup failure that is distinct from a
// missing token: /user resolves fine (so token resolution succeeds), but every /graphql
// call 500s, so the feedback list itself cannot be retrieved.
func githubGraphQLFailureServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"login": "test-bot"})
		case "/graphql":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

const (
	incidentSharedLogin    = "fleet-bot"
	incidentRejectionID    = "IC_reject1"
	incidentApprovalID     = "IC_approve1"
	incidentResolvedThread = "PRRT_thread1"
)

// incidentReplayGitHubServer models the run03-referee PR #34 incident (task
// 39a672b8-4062-43f2-88be-84aa24266e81): one resolved inline review thread, an
// outstanding marked global reviewer rejection, and another reviewer's canonical
// approval, all posted under one shared GitHub login — the fleet's shared identity,
// which is exactly the condition under which the lost-feedback incident occurred (a
// marked comment under the same login as the worker's bot login must still be treated as
// live feedback, not agent chatter). ackReplyBody, if non-empty, is appended as an
// additional global comment so tests can vary which comment ID a worker acknowledgment
// names.
func incidentReplayGitHubServer(t *testing.T, ackReplyBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"login": incidentSharedLogin})
		case "/graphql":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			query := body["query"]
			w.Header().Set("Content-Type", "application/json")
			switch {
			case strings.Contains(query, "reviewThreads"):
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{"hasNextPage": false},
									"nodes": []map[string]interface{}{
										{
											"id":         incidentResolvedThread,
											"isResolved": true,
											"path":       "cmd/odonian/main.go",
											"line":       10,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{"id": "thread1-first", "body": "opus-reviewer: nil check missing here", "author": map[string]string{"login": incidentSharedLogin}},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{"id": "thread1-last", "body": "haiku-worker: addressed in cafefeed", "author": map[string]string{"login": incidentSharedLogin}},
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
				nodes := []map[string]interface{}{
					{
						"id":             incidentRejectionID,
						"databaseId":     501,
						"body":           "gpt-5.5-reviewer: CHANGES REQUESTED\n\nfix the auth check before merge",
						"createdAt":      "2026-01-01T00:00:00Z",
						"author":         map[string]string{"login": incidentSharedLogin},
						"reactionGroups": []interface{}{},
					},
					{
						"id":             incidentApprovalID,
						"databaseId":     502,
						"body":           "opus-reviewer: APPROVED — rest of the diff looks fine",
						"createdAt":      "2026-01-01T00:05:00Z",
						"author":         map[string]string{"login": incidentSharedLogin},
						"reactionGroups": []interface{}{},
					},
				}
				if ackReplyBody != "" {
					nodes = append(nodes, map[string]interface{}{
						"id":             "IC_ack1",
						"databaseId":     503,
						"body":           ackReplyBody,
						"createdAt":      "2026-01-01T00:10:00Z",
						"author":         map[string]string{"login": incidentSharedLogin},
						"reactionGroups": []interface{}{},
					})
				}
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "PR-node-incident",
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

func TestExecuteSubmitFeedbackGate_IncidentReplayBlocksOutstandingRejection(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
	defer odonianServer.Close()

	ghServer := incidentReplayGitHubServer(t, "")
	defer ghServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = ghServer.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")

	// pr-feedback list must retain the outstanding marked rejection.
	var listOut bytes.Buffer
	if err := executePRFeedbackList(context.Background(), []string{prURL}, &listOut); err != nil {
		t.Fatalf("executePRFeedbackList: %v", err)
	}
	if !strings.Contains(listOut.String(), incidentRejectionID) {
		t.Fatalf("expected pr-feedback list to retain the outstanding rejection %q, got: %s", incidentRejectionID, listOut.String())
	}

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
	if tracker.hit.Load() {
		t.Error("expected the gate to withhold POST /tasks/{id}/submit while the rejection remains outstanding, but it fired")
	}
}

func TestExecuteSubmitFeedbackGate_IncidentReplayExactAckPermits(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
	defer odonianServer.Close()

	ackReplyBody := "haiku-worker: addressed in cafef00d (see comment " + incidentRejectionID + ")"
	ghServer := incidentReplayGitHubServer(t, ackReplyBody)
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
		t.Fatalf("expected a specific acknowledgment of the outstanding rejection to permit submission, got: %v", err)
	}
	if !tracker.hit.Load() {
		t.Error("expected POST /tasks/{id}/submit to fire once the rejection is specifically acknowledged, but it did not")
	}
}

func TestExecuteSubmitFeedbackGate_IncidentReplayUnrelatedAckDoesNotPermit(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
	defer odonianServer.Close()

	// Acknowledges the approval comment, not the outstanding rejection.
	ackReplyBody := "haiku-worker: addressed in cafef00d (see comment " + incidentApprovalID + ")"
	ghServer := incidentReplayGitHubServer(t, ackReplyBody)
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
		t.Fatal("expected acknowledging an unrelated comment to leave the rejection outstanding and block submission, got nil error")
	}
	if !strings.Contains(err.Error(), "pr-feedback ack") {
		t.Errorf("expected error to point at pr-feedback ack, got: %v", err)
	}
	if tracker.hit.Load() {
		t.Error("expected the gate to withhold POST /tasks/{id}/submit when only an unrelated item was acknowledged, but it fired")
	}
}

func TestExecuteSubmitFeedbackGate_CleanPass(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
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
	if !tracker.hit.Load() {
		t.Error("expected POST /tasks/{id}/submit to fire once there is genuinely no outstanding feedback, but it did not")
	}
}

func TestExecuteSubmitFeedbackGate_BypassFlag(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
	defer odonianServer.Close()

	// A real GitHub mock with an outstanding item, and a working token: without
	// --skip-feedback-gate this setup blocks the submit (see
	// TestExecuteSubmitFeedbackGate_IncidentReplayBlocksOutstandingRejection). Passing
	// here proves the flag genuinely bypasses the check rather than coincidentally
	// hitting a fail-open path.
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
	if !tracker.hit.Load() {
		t.Error("expected POST /tasks/{id}/submit to fire once the gate is explicitly bypassed, but it did not")
	}
}

func TestExecuteSubmitFeedbackGate_TaskFetchFailureBlocks(t *testing.T) {
	tracker := &submitTracker{}
	odonianServer := failingGetOdonianTaskServer(t, "task123", tracker)
	defer odonianServer.Close()

	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"--pr", "https://github.com/owner/repo/pull/42",
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err == nil {
		t.Fatal("expected a task-fetch failure to stop submission with a retryable error, got nil error")
	}
	if !strings.Contains(err.Error(), "retryable") {
		t.Errorf("expected the error to explain the failure is retryable, got: %v", err)
	}
	if tracker.hit.Load() {
		t.Error("expected the gate to withhold POST /tasks/{id}/submit when task metadata could not be loaded, but it fired")
	}
}

func TestExecuteSubmitFeedbackGate_TaskFetchFailureBypassFlag(t *testing.T) {
	tracker := &submitTracker{}
	odonianServer := failingGetOdonianTaskServer(t, "task123", tracker)
	defer odonianServer.Close()

	t.Setenv("AGENT_ID", "test-agent")

	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatalf("failed to create pipe: %v", pipeErr)
	}
	oldStderr := os.Stderr
	os.Stderr = w

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"--pr", "https://github.com/owner/repo/pull/42",
		"--branch", "mr/a1b2c3d4",
		"--skip-feedback-gate",
		"task123",
	})

	os.Stderr = oldStderr
	w.Close()
	var stderr bytes.Buffer
	io.Copy(&stderr, r)

	if err != nil {
		t.Fatalf("expected --skip-feedback-gate to bypass a task-fetch failure and submit to succeed, got: %v", err)
	}
	if !strings.Contains(stderr.String(), "WARNING") {
		t.Errorf("expected a loud warning on bypass, got stderr: %q", stderr.String())
	}
	if !tracker.hit.Load() {
		t.Error("expected POST /tasks/{id}/submit to fire once the gate is explicitly bypassed, but it did not")
	}
}

func TestExecuteSubmitFeedbackGate_FeedbackLookupFailureBlocks(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
	defer odonianServer.Close()

	ghServer := githubGraphQLFailureServer(t)
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
		t.Fatal("expected a feedback-lookup failure to stop submission with a retryable error, got nil error")
	}
	if !strings.Contains(err.Error(), "retryable") {
		t.Errorf("expected the error to explain the failure is retryable, got: %v", err)
	}
	if tracker.hit.Load() {
		t.Error("expected the gate to withhold POST /tasks/{id}/submit when the feedback lookup failed, but it fired")
	}
}

func TestExecuteSubmitFeedbackGate_NoTokenLookupFailureBlocks(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
	defer odonianServer.Close()

	// No GH token available anywhere, so the feedback check itself cannot even run. This
	// is the case the superseded CheckErrorProceeds test required to fail open; the
	// approved repair requires it to block instead, since an unsuccessful lookup is not
	// evidence of zero outstanding items.
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
		t.Fatal("expected a missing-token lookup failure to stop submission, got nil error")
	}
	if tracker.hit.Load() {
		t.Error("expected the gate to withhold POST /tasks/{id}/submit when the feedback lookup could not run, but it fired")
	}
}

func TestExecuteSubmitFeedbackGate_InitialSubmissionSkipsGate(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	// review_round 0: an initial submission. The gate must short-circuit before ever
	// contacting GitHub, so point at a closed server — any attempted call fails fast and
	// would turn this into a false pass/false block rather than a silent skip.
	poisoned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	poisoned.Close()

	odonianServer := odonianTaskServer(t, "task123", 0, prURL, tracker)
	defer odonianServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = poisoned.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "initial submission",
		"--pr", prURL,
		"--branch", "mr/a1b2c3d4",
		"task123",
	})
	if err != nil {
		t.Fatalf("expected an initial submission (review_round 0) to skip the gate entirely, got: %v", err)
	}
	if !tracker.hit.Load() {
		t.Error("expected POST /tasks/{id}/submit to fire for a gate-exempt initial submission, but it did not")
	}
}

func TestExecuteSubmitFeedbackGate_ReviewerVerdictSubmissionSkipsGate(t *testing.T) {
	tracker := &submitTracker{}

	// A review-kind task's own record carries no `pr` link (that link lives on the
	// parent implement task), so the gate must skip it regardless of review round. Point
	// at a closed GitHub server to prove it never gets contacted.
	poisoned := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	poisoned.Close()

	odonianServer := odonianTaskServer(t, "review-task-1", 1, "", tracker)
	defer odonianServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = poisoned.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "looks good",
		"--verdict", "approve",
		"review-task-1",
	})
	if err != nil {
		t.Fatalf("expected a reviewer verdict submission to skip the pr-feedback gate, got: %v", err)
	}
	if !tracker.hit.Load() {
		t.Error("expected POST /tasks/{id}/submit to fire for a gate-exempt reviewer verdict, but it did not")
	}
}

func TestExecuteSubmitFeedbackGate_LocalCommitDeliveryModeGateApplies(t *testing.T) {
	prURL := "https://github.com/owner/repo/pull/42"
	tracker := &submitTracker{}

	odonianServer := odonianTaskServer(t, "task123", 1, prURL, tracker)
	defer odonianServer.Close()

	ghServer := incidentReplayGitHubServer(t, "")
	defer ghServer.Close()

	oldBase := forge.GitHubBaseURL
	forge.GitHubBaseURL = ghServer.URL
	defer func() { forge.GitHubBaseURL = oldBase }()

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("AGENT_ID", "test-agent")
	t.Setenv("ODONIAN_DELIVERY_MODE", "local_commit")

	err := executeSubmit(context.Background(), odonianServer.URL, "test-token", []string{
		"--result", "reworked",
		"task123",
	})
	if err == nil {
		t.Fatal("expected the pr-feedback gate to apply under local_commit delivery mode too, got nil error")
	}
	if !strings.Contains(err.Error(), "pr-feedback ack") {
		t.Errorf("expected error to point at pr-feedback ack, got: %v", err)
	}
	if tracker.hit.Load() {
		t.Error("expected the gate to withhold POST /tasks/{id}/submit under local_commit delivery mode, but it fired")
	}
}
