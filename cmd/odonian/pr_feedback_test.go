package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/forge"
)

func TestExecutePRFeedbackListHelp(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executePRFeedback(context.Background(), []string{"--help"}, buf)
	if err != nil {
		t.Fatalf("executePRFeedback --help failed: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "list") {
		t.Errorf("expected output to contain 'list', got: %s", output)
	}
	if !strings.Contains(output, "ack") {
		t.Errorf("expected output to contain 'ack', got: %s", output)
	}
}

func TestExecutePRFeedbackList(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" && r.Header.Get("Authorization") == "token test-token" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"login": "test-bot",
			})
		} else if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			query := body["query"]
			if strings.Contains(query, "reviewThreads") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"id":         "thread-1",
											"isResolved": false,
											"path":       "main.go",
											"line":       42,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
													},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
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
			} else if strings.Contains(query, "comments(first:") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"id":         "global-comment-1",
											"databaseId": 123,
											"body":       "Fix this typo",
											"createdAt":  "2026-01-01T00:00:00Z",
											"author": map[string]string{
												"login": "commenter",
											},
											"reactionGroups": []interface{}{},
										},
									},
								},
							},
						},
					},
				})
			}
		}
	}))
	defer server.Close()

	forge.GitHubBaseURL = server.URL

	t.Setenv("GH_TOKEN", "test-token")

	buf := &bytes.Buffer{}
	err := executePRFeedbackList(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
	}, buf)
	if err != nil {
		t.Fatalf("executePRFeedbackList failed: %v", err)
	}

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 feedback items (1 inline, 1 global), got %d lines: %s", len(lines), output)
	}

	var items []map[string]interface{}
	for _, line := range lines {
		if line == "" {
			continue
		}
		var item map[string]interface{}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			t.Fatalf("failed to parse JSON line: %v", err)
		}
		items = append(items, item)
	}

	if len(items) < 1 {
		t.Errorf("expected at least 1 parsed item, got %d", len(items))
	}

	if len(items) >= 1 && items[0]["kind"] != "inline" {
		t.Errorf("expected first item kind 'inline', got %v", items[0]["kind"])
	}

	if len(items) >= 2 && items[1]["kind"] != "global" {
		t.Errorf("expected second item kind 'global', got %v", items[1]["kind"])
	}
}

func TestExecutePRFeedbackListNoToken(t *testing.T) {
	oldToken := os.Getenv("GH_TOKEN")
	defer func() {
		if oldToken != "" {
			os.Setenv("GH_TOKEN", oldToken)
		} else {
			os.Unsetenv("GH_TOKEN")
		}
	}()
	os.Unsetenv("GH_TOKEN")

	buf := &bytes.Buffer{}
	err := executePRFeedbackList(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
	}, buf)
	if err == nil {
		t.Fatal("expected error for no GitHub token, got nil")
	}
	if !strings.Contains(err.Error(), "GitHub token") {
		t.Errorf("expected error to mention GitHub token, got: %v", err)
	}
}

func TestExecutePRFeedbackListMissingPRURL(t *testing.T) {
	buf := &bytes.Buffer{}
	err := executePRFeedbackList(context.Background(), []string{}, buf)
	if err == nil {
		t.Fatal("expected error for missing pr-url, got nil")
	}
	if !strings.Contains(err.Error(), "pr-url") {
		t.Errorf("expected error to mention pr-url, got: %v", err)
	}
}

func TestExecutePRFeedbackAck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"login": "test-bot",
			})
		} else if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			query := body["query"]
			if strings.Contains(query, "reviewThreads") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"id":         "thread-1",
											"isResolved": false,
											"path":       "main.go",
											"line":       42,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
													},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
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
			} else if strings.Contains(query, "comments(first:") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{},
								},
							},
						},
					},
				})
			} else if strings.Contains(query, "addPullRequestReviewThreadReply") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"addPullRequestReviewThreadReply": map[string]interface{}{
							"comment": map[string]string{
								"id": "reply-1",
							},
						},
					},
				})
			} else if strings.Contains(query, "resolveReviewThread") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"resolveReviewThread": map[string]interface{}{
							"thread": map[string]string{
								"id": "thread-1",
							},
						},
					},
				})
			}
		}
	}))
	defer server.Close()

	forge.GitHubBaseURL = server.URL

	t.Setenv("GH_TOKEN", "test-token")

	err := executePRFeedbackAck(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
		"thread-1",
		"abc123def456",
	})
	if err != nil {
		t.Fatalf("executePRFeedbackAck failed: %v", err)
	}
}

func TestExecutePRFeedbackAckItemNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"login": "test-bot",
			})
		} else if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"repository": map[string]interface{}{
						"pullRequest": map[string]interface{}{
							"reviewThreads": map[string]interface{}{
								"pageInfo": map[string]interface{}{
									"hasNextPage": false,
								},
								"nodes": []map[string]interface{}{},
							},
							"id": "pr-node-1",
							"comments": map[string]interface{}{
								"pageInfo": map[string]interface{}{
									"hasNextPage": false,
								},
								"nodes": []map[string]interface{}{},
							},
						},
					},
				},
			})
		}
	}))
	defer server.Close()

	forge.GitHubBaseURL = server.URL

	t.Setenv("GH_TOKEN", "test-token")

	err := executePRFeedbackAck(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
		"nonexistent-item",
		"abc123def456",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent feedback item, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("expected error to mention 'not found', got: %v", err)
	}
}

func TestExecutePRFeedbackAckMissingArgs(t *testing.T) {
	err := executePRFeedbackAck(context.Background(), []string{})
	if err == nil {
		t.Fatal("expected error for missing args, got nil")
	}
	if !strings.Contains(err.Error(), "pr-url") || !strings.Contains(err.Error(), "item-id") || !strings.Contains(err.Error(), "sha") {
		t.Errorf("expected error to mention required args, got: %v", err)
	}
}

func TestExecutePRFeedbackAckDefaultMarker(t *testing.T) {
	var capturedMutation string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"login": "test-bot",
			})
		} else if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			query := body["query"]
			if strings.Contains(query, "reviewThreads") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"id":         "thread-1",
											"isResolved": false,
											"path":       "main.go",
											"line":       42,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
													},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
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
			} else if strings.Contains(query, "comments(first:") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{},
								},
							},
						},
					},
				})
			} else if strings.Contains(query, "addPullRequestReviewThreadReply") {
				capturedMutation = query
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"addPullRequestReviewThreadReply": map[string]interface{}{
							"comment": map[string]string{
								"id": "reply-1",
							},
						},
					},
				})
			} else if strings.Contains(query, "resolveReviewThread") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"resolveReviewThread": map[string]interface{}{
							"thread": map[string]string{
								"id": "thread-1",
							},
						},
					},
				})
			}
		}
	}))
	defer server.Close()

	forge.GitHubBaseURL = server.URL

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("ODONIAN_MODEL", "haiku")

	err := executePRFeedbackAck(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
		"thread-1",
		"abc123def456",
	})
	if err != nil {
		t.Fatalf("executePRFeedbackAck failed: %v", err)
	}

	if !strings.Contains(capturedMutation, "haiku-worker:") {
		t.Errorf("expected mutation to contain 'haiku-worker:', got: %s", capturedMutation)
	}
}

func TestExecutePRFeedbackAckWithMarkerOverride(t *testing.T) {
	var capturedMutation string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"login": "test-bot",
			})
		} else if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			query := body["query"]
			if strings.Contains(query, "reviewThreads") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"id":         "thread-1",
											"isResolved": false,
											"path":       "main.go",
											"line":       42,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
													},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
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
			} else if strings.Contains(query, "comments(first:") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{},
								},
							},
						},
					},
				})
			} else if strings.Contains(query, "addPullRequestReviewThreadReply") {
				capturedMutation = query
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"addPullRequestReviewThreadReply": map[string]interface{}{
							"comment": map[string]string{
								"id": "reply-1",
							},
						},
					},
				})
			} else if strings.Contains(query, "resolveReviewThread") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"resolveReviewThread": map[string]interface{}{
							"thread": map[string]string{
								"id": "thread-1",
							},
						},
					},
				})
			}
		}
	}))
	defer server.Close()

	forge.GitHubBaseURL = server.URL

	t.Setenv("GH_TOKEN", "test-token")

	err := executePRFeedbackAck(context.Background(), []string{
		"--marker", "custom-worker: ",
		"https://github.com/owner/repo/pull/42",
		"thread-1",
		"abc123def456",
	})
	if err != nil {
		t.Fatalf("executePRFeedbackAck failed: %v", err)
	}

	if !strings.Contains(capturedMutation, "custom-worker:") {
		t.Errorf("expected mutation to contain 'custom-worker:', got: %s", capturedMutation)
	}
}

func TestExecutePRFeedbackAckMarkerSatisfiesPredicate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"login": "test-bot",
			})
		} else if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			query := body["query"]
			if strings.Contains(query, "reviewThreads") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"id":         "thread-1",
											"isResolved": false,
											"path":       "main.go",
											"line":       42,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
													},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
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
			} else if strings.Contains(query, "comments(first:") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{},
								},
							},
						},
					},
				})
			} else if strings.Contains(query, "addPullRequestReviewThreadReply") {
				var extractedBody string
				if idx := strings.Index(query, "body: \""); idx != -1 {
					start := idx + len("body: \"")
					end := strings.Index(query[start:], "\"")
					if end != -1 {
						extractedBody = query[start : start+end]
					}
				}
				if extractedBody != "" && !forge.IsAgentAuthoredComment(extractedBody) {
					t.Errorf("extracted body %q does not satisfy marker predicate", extractedBody)
				}
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"addPullRequestReviewThreadReply": map[string]interface{}{
							"comment": map[string]string{
								"id": "reply-1",
							},
						},
					},
				})
			} else if strings.Contains(query, "resolveReviewThread") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"resolveReviewThread": map[string]interface{}{
							"thread": map[string]string{
								"id": "thread-1",
							},
						},
					},
				})
			}
		}
	}))
	defer server.Close()

	forge.GitHubBaseURL = server.URL

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("ODONIAN_MODEL", "opus")

	err := executePRFeedbackAck(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
		"thread-1",
		"abc123def456",
	})
	if err != nil {
		t.Fatalf("executePRFeedbackAck failed: %v", err)
	}
}

func TestExecutePRFeedbackAckDisputed(t *testing.T) {
	var capturedMutation string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/user" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{
				"login": "test-bot",
			})
		} else if r.URL.Path == "/graphql" {
			w.Header().Set("Content-Type", "application/json")
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}

			query := body["query"]
			if strings.Contains(query, "reviewThreads") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"reviewThreads": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{
										{
											"id":         "thread-1",
											"isResolved": false,
											"path":       "main.go",
											"line":       42,
											"firstComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
													},
												},
											},
											"lastComments": map[string]interface{}{
												"nodes": []map[string]interface{}{
													{
														"id":   "comment-1",
														"body": "This looks wrong",
														"author": map[string]string{
															"login": "reviewer",
														},
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
			} else if strings.Contains(query, "comments(first:") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"repository": map[string]interface{}{
							"pullRequest": map[string]interface{}{
								"id": "pr-node-1",
								"comments": map[string]interface{}{
									"pageInfo": map[string]interface{}{
										"hasNextPage": false,
									},
									"nodes": []map[string]interface{}{},
								},
							},
						},
					},
				})
			} else if strings.Contains(query, "addPullRequestReviewThreadReply") {
				capturedMutation = query
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"addPullRequestReviewThreadReply": map[string]interface{}{
							"comment": map[string]string{
								"id": "reply-1",
							},
						},
					},
				})
			} else if strings.Contains(query, "resolveReviewThread") {
				json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{
						"resolveReviewThread": map[string]interface{}{
							"thread": map[string]string{
								"id": "thread-1",
							},
						},
					},
				})
			}
		}
	}))
	defer server.Close()

	forge.GitHubBaseURL = server.URL

	t.Setenv("GH_TOKEN", "test-token")
	t.Setenv("ODONIAN_MODEL", "haiku")

	err := executePRFeedbackAck(context.Background(), []string{
		"--disputed",
		"https://github.com/owner/repo/pull/42",
		"thread-1",
	})
	if err != nil {
		t.Fatalf("executePRFeedbackAck --disputed failed: %v", err)
	}

	if !strings.Contains(capturedMutation, "disputed (see comment thread-1): finding disputed with source evidence in this round's submission; not fixed") {
		t.Errorf("expected mutation to contain full disputed acknowledgment body, got: %s", capturedMutation)
	}
}

func TestExecutePRFeedbackAckDisputedWithShaIsError(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")

	err := executePRFeedbackAck(context.Background(), []string{
		"--disputed",
		"https://github.com/owner/repo/pull/42",
		"thread-1",
		"abc123def456",
	})
	if err == nil {
		t.Fatal("expected error when both --disputed and sha are specified")
	}
	if !strings.Contains(err.Error(), "both") {
		t.Errorf("expected error to mention 'both', got: %v", err)
	}
}

func TestExecutePRFeedbackAckMissingShaWithoutDisputed(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")

	err := executePRFeedbackAck(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
		"thread-1",
	})
	if err == nil {
		t.Fatal("expected error when sha is missing and --disputed is not set")
	}
	if !strings.Contains(err.Error(), "sha") || !strings.Contains(err.Error(), "disputed") {
		t.Errorf("expected error to mention sha/disputed requirement, got: %v", err)
	}
}

func TestExecutePRFeedbackAckDisputedAfterPositionals(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")

	// Test that --disputed after positional args is rejected (flag parsing stops at first positional)
	err := executePRFeedbackAck(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
		"thread-1",
		"--disputed",
	})
	if err == nil {
		t.Fatal("expected error when --disputed is placed after positional args")
	}
	if !strings.Contains(err.Error(), "cannot start with '-'") {
		t.Errorf("expected error about flag in sha position, got: %v", err)
	}
}

func TestExecutePRFeedbackAckShaWithTrailingDisputed(t *testing.T) {
	t.Setenv("GH_TOKEN", "test-token")

	// Test that sha followed by trailing --disputed is rejected without making HTTP calls.
	// Flag parsing stops at first positional, so [url, id, sha, "--disputed"] leaves
	// disputed=false and takes sha=positional[2]. The trailing --disputed becomes
	// positional[3] and should be rejected as an unexpected extra argument.
	err := executePRFeedbackAck(context.Background(), []string{
		"https://github.com/owner/repo/pull/42",
		"thread-1",
		"abc123def456",
		"--disputed",
	})
	if err == nil {
		t.Fatal("expected error when both sha and --disputed are specified in that order")
	}
	if !strings.Contains(err.Error(), "unexpected extra arguments") {
		t.Errorf("expected error about unexpected extra arguments, got: %v", err)
	}
}
