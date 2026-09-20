package forge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestListUnaddressedFeedback_UnresolvedThreads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Response for review threads query (first call)
		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "thread-1",
              "isResolved": false,
              "path": "main.go",
              "line": 42,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-1",
                    "body": "This needs fixing",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-1",
                    "body": "This needs fixing",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              }
            },
            {
              "id": "thread-2",
              "isResolved": true,
              "path": "main.go",
              "line": 50,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-2",
                    "body": "Already fixed",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-2",
                    "body": "Already fixed",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			// Response for global comments query (second call) - return empty
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-test",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "bot", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	if len(items) != 1 {
		t.Errorf("ListUnaddressedFeedback() returned %d items, want 1", len(items))
	}

	if items[0].Kind != "inline" {
		t.Errorf("items[0].Kind = %q, want %q", items[0].Kind, "inline")
	}

	if items[0].ID != "thread-1" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "thread-1")
	}

	if items[0].Path != "main.go" {
		t.Errorf("items[0].Path = %q, want %q", items[0].Path, "main.go")
	}

	if items[0].Line != 42 {
		t.Errorf("items[0].Line = %d, want 42", items[0].Line)
	}

	if items[0].Author != "reviewer" {
		t.Errorf("items[0].Author = %q, want %q", items[0].Author, "reviewer")
	}

	if items[0].Body != "This needs fixing" {
		t.Errorf("items[0].Body = %q, want %q", items[0].Body, "This needs fixing")
	}
}

func TestListUnaddressedFeedback_GlobalCommentsBotExcluded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Response for review threads query (first call)
		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			// Response for global comments query
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-1",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "comment-1",
              "databaseId": 1,
              "body": "Human feedback",
              "createdAt": "2024-01-01T10:00:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-2",
              "databaseId": 2,
              "body": "haiku-worker: Bot response",
              "createdAt": "2024-01-01T10:00:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            }
          ]
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "human", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	if len(items) != 1 {
		t.Errorf("ListUnaddressedFeedback() returned %d items, want 1", len(items))
	}

	if items[0].Author != "human" {
		t.Errorf("items[0].Author = %q, want %q", items[0].Author, "human")
	}
}

func TestListUnaddressedFeedback_AcknowledgedCommentExcluded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Response for review threads query (first call)
		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			// Response for global comments query
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-2",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "comment-1",
              "databaseId": 1,
              "body": "Feedback needing ack",
              "createdAt": "2024-01-01T10:00:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-2",
              "databaseId": 2,
              "body": "haiku-worker: addressed in abc123 (see comment comment-1)",
              "createdAt": "2024-01-01T10:05:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-3",
              "databaseId": 3,
              "body": "Feedback with only a bot reaction",
              "createdAt": "2024-01-01T10:10:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": [
                {
                  "content": "THUMBS_UP",
                  "users": {
                    "nodes": [
                      {
                        "login": "bot"
                      }
                    ]
                  }
                }
              ]
            }
          ]
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "bot", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	// comment-1 is cleared by the exact-ID worker acknowledgment (comment-2), which
	// is itself worker status and not surfaced. comment-3 carries only a bot reaction,
	// which no longer acknowledges anything, so it remains outstanding.
	if len(items) != 1 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 1: %+v", len(items), items)
	}

	if items[0].ID != "comment-3" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-3")
	}
}

func TestListUnaddressedFeedback_EmptyCase(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        },
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(graphqlResp))
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "bot", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	if len(items) != 0 {
		t.Errorf("ListUnaddressedFeedback() returned %d items, want 0", len(items))
	}
}

func TestListUnaddressedFeedback_GraphQLQueryValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Validate that the query doesn't include 'replies' field (which doesn't exist on IssueComment)
		if strings.Contains(bodyStr, "comments") && strings.Contains(bodyStr, "replies") {
			// If 'replies' is in the query, return a GraphQL error like GitHub would
			graphqlResp := `{
  "errors": [
    {
      "message": "Field replies does not exist on type IssueComment",
      "locations": [{"line": 1, "column": 1}],
      "code": "undefinedField",
      "typeName": "IssueComment",
      "fieldName": "replies"
    }
  ]
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
			return
		}

		// Response for review threads query
		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			// Response for global comments query - should not have 'replies' field
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-test",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "bot", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	if len(items) != 0 {
		t.Errorf("ListUnaddressedFeedback() returned %d items, want 0", len(items))
	}
}

func TestListUnaddressedFeedback_GraphQLErrorHandling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Response for review threads query
		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			// Return a GraphQL error
			graphqlResp := `{
  "errors": [
    {
      "message": "Field replies does not exist on type IssueComment"
    }
  ]
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "bot", "token")

	if err == nil {
		t.Fatalf("ListUnaddressedFeedback() error = nil, want error for GraphQL errors")
	}

	if !strings.Contains(err.Error(), "graphql error") {
		t.Errorf("ListUnaddressedFeedback() error = %v, want error containing 'graphql error'", err)
	}
}

func TestListUnaddressedFeedback_Pagination(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Response for review threads query with pagination
		if strings.Contains(bodyStr, "reviewThreads") {
			if callCount == 1 {
				// First page has more
				graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": true,
            "endCursor": "cursor123"
          },
          "nodes": [
            {
              "id": "thread-1",
              "isResolved": false,
              "path": "file1.go",
              "line": 10,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-1",
                    "body": "Comment 1",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-1",
                    "body": "Comment 1",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(graphqlResp))
			} else if callCount == 2 {
				// Second page is last
				graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "thread-2",
              "isResolved": false,
              "path": "file2.go",
              "line": 20,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-2",
                    "body": "Comment 2",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-2",
                    "body": "Comment 2",
                    "author": {
                      "login": "reviewer"
                    }
                  }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(graphqlResp))
			}
		} else if strings.Contains(bodyStr, "comments") {
			// Return empty comments for global comments
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-test",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "bot", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	if len(items) != 2 {
		t.Errorf("ListUnaddressedFeedback() returned %d items, want 2", len(items))
	}

	if items[0].Path != "file1.go" {
		t.Errorf("items[0].Path = %q, want %q", items[0].Path, "file1.go")
	}

	if items[1].Path != "file2.go" {
		t.Errorf("items[1].Path = %q, want %q", items[1].Path, "file2.go")
	}
}

func TestAcknowledgeFeedbackItem_InlineItem(t *testing.T) {
	mutationCalls := make(map[string]string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Handle resolveReviewThread mutation
		if strings.Contains(bodyStr, "resolveReviewThread") {
			mutationCalls["resolveReviewThread"] = bodyStr
			graphqlResp := `{
  "data": {
    "resolveReviewThread": {
      "thread": {
        "id": "thread-1"
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "addPullRequestReviewThreadReply") {
			// Handle addPullRequestReviewThreadReply mutation
			mutationCalls["addPullRequestReviewThreadReply"] = bodyStr
			// Fail if the request does not use the correct field name (AddPullRequestReviewThreadReplyInput)
			if !strings.Contains(bodyStr, "pullRequestReviewThreadId:") {
				graphqlResp := `{
  "errors": [
    {
      "message": "Field 'threadId' doesn't exist on type 'AddPullRequestReviewThreadReplyInput'"
    }
  ]
}`
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(graphqlResp))
				return
			}
			graphqlResp := `{
  "data": {
    "addPullRequestReviewThreadReply": {
      "comment": {
        "id": "comment-reply-1"
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	item := FeedbackItem{
		Kind:   "inline",
		ID:     "thread-1",
		Author: "reviewer",
		Body:   "This needs fixing",
	}

	fixingSha := "abc123def456"
	err := AcknowledgeFeedbackItem(ctx, "owner", "repo", 42, "token", item, fixingSha, "")

	if err != nil {
		t.Fatalf("AcknowledgeFeedbackItem() error = %v, want nil", err)
	}

	if _, ok := mutationCalls["addPullRequestReviewThreadReply"]; !ok {
		t.Errorf("addPullRequestReviewThreadReply mutation was not called")
	} else {
		if !strings.Contains(mutationCalls["addPullRequestReviewThreadReply"], fixingSha) {
			t.Errorf("addPullRequestReviewThreadReply mutation body does not contain fixing SHA %q", fixingSha)
		}
		if !strings.Contains(mutationCalls["addPullRequestReviewThreadReply"], "thread-1") {
			t.Errorf("addPullRequestReviewThreadReply mutation body does not contain thread ID 'thread-1'")
		}
	}

	if _, ok := mutationCalls["resolveReviewThread"]; !ok {
		t.Errorf("resolveReviewThread mutation was not called")
	} else {
		if !strings.Contains(mutationCalls["resolveReviewThread"], "thread-1") {
			t.Errorf("resolveReviewThread mutation body does not contain thread ID 'thread-1'")
		}
	}
}

func TestAcknowledgeFeedbackItem_GlobalItem(t *testing.T) {
	requestPaths := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPaths[r.Method+" "+r.URL.Path]++

		// Handle GraphQL mutations for global comment reply
		if r.Method == http.MethodPost && r.URL.Path == "/graphql" {
			body, _ := io.ReadAll(r.Body)
			bodyStr := string(body)

			if strings.Contains(bodyStr, "addComment") {
				graphqlResp := `{
  "data": {
    "addComment": {
      "commentEdge": {
        "node": {
          "id": "comment-reply-1"
        }
      }
    }
  }
}`
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(graphqlResp))
			}
		} else if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/reactions") {
			// Handle thumbsup reaction via REST
			reactionResp := `{
  "id": 1,
  "user": {
    "login": "bot"
  },
  "content": "+1",
  "created_at": "2024-01-01T10:00:00Z"
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(reactionResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	item := FeedbackItem{
		Kind:       "global",
		ID:         "comment-1",
		DatabaseID: "12345",
		PRID:       "PR-node-id",
		Author:     "human",
		Body:       "Global feedback",
	}

	err := AcknowledgeFeedbackItem(ctx, "owner", "repo", 42, "token", item, "abc123def456", "")

	if err != nil {
		t.Fatalf("AcknowledgeFeedbackItem() error = %v, want nil", err)
	}

	// Verify that both GraphQL and REST calls were made
	if requestPaths["POST /graphql"] == 0 {
		t.Errorf("Expected GraphQL mutation call, but got none")
	}
	if requestPaths["POST /repos/owner/repo/issues/comments/12345/reactions"] == 0 {
		t.Errorf("Expected reactions REST call, but got none")
	}
}

func TestAcknowledgeFeedbackItem_InlineItemGraphQLError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a GraphQL error for any mutation
		graphqlResp := `{
  "errors": [
    {
      "message": "Invalid thread ID"
    }
  ]
}`
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(graphqlResp))
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	item := FeedbackItem{
		Kind:   "inline",
		ID:     "invalid-thread",
		Author: "reviewer",
		Body:   "This needs fixing",
	}

	err := AcknowledgeFeedbackItem(ctx, "owner", "repo", 42, "token", item, "abc123def456", "")

	if err == nil {
		t.Fatalf("AcknowledgeFeedbackItem() error = nil, want error for GraphQL error")
	}

	if !strings.Contains(err.Error(), "graphql error") {
		t.Errorf("AcknowledgeFeedbackItem() error = %v, want error containing 'graphql error'", err)
	}
}

func TestAcknowledgeFeedbackItem_GlobalItemReactionError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/reactions") {
			// Return error for reaction endpoint
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message": "Not Found"}`))
		} else {
			// Return success for comment creation
			graphqlResp := `{
  "data": {
    "createIssueComment": {
      "commentEdge": {
        "node": {
          "id": "comment-reply-1"
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	item := FeedbackItem{
		Kind:       "global",
		ID:         "comment-1",
		DatabaseID: "12345",
		PRID:       "PR-node-id",
		Author:     "human",
		Body:       "Global feedback",
	}

	err := AcknowledgeFeedbackItem(ctx, "owner", "repo", 42, "token", item, "abc123def456", "")

	if err == nil {
		t.Fatalf("AcknowledgeFeedbackItem() error = nil, want error for reaction failure")
	}

	if !strings.Contains(err.Error(), "request failed") {
		t.Errorf("AcknowledgeFeedbackItem() error = %v, want error containing 'request failed'", err)
	}
}

func TestAcknowledgeFeedbackItem_UnknownKind(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	item := FeedbackItem{
		Kind:   "unknown",
		ID:     "item-1",
		Author: "someone",
		Body:   "Some feedback",
	}

	err := AcknowledgeFeedbackItem(ctx, "owner", "repo", 42, "token", item, "abc123def456", "")

	if err == nil {
		t.Fatalf("AcknowledgeFeedbackItem() error = nil, want error for unknown kind")
	}

	if !strings.Contains(err.Error(), "unknown feedback item kind") {
		t.Errorf("AcknowledgeFeedbackItem() error = %v, want error containing 'unknown feedback item kind'", err)
	}
}

func TestListUnaddressedFeedback_AcknowledgedByBotReply(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		// Response for review threads query (first call)
		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			// Response for global comments query
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-3",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "comment-1",
              "databaseId": 1,
              "body": "Human feedback without reaction",
              "createdAt": "2024-01-01T10:00:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-2",
              "databaseId": 2,
              "body": "haiku-worker: Bot reply acknowledging the feedback",
              "createdAt": "2024-01-01T10:05:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-3",
              "databaseId": 3,
              "body": "Another unacknowledged feedback",
              "createdAt": "2024-01-01T10:10:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            }
          ]
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "human", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	// comment-2 is a worker note WITHOUT the exact-ID/fixing-commit acknowledgment
	// format, so it neither surfaces as feedback (worker status) nor clears comment-1.
	// Both unmarked human comments therefore remain outstanding.
	if len(items) != 2 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 2: %+v", len(items), items)
	}

	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	if !ids["comment-1"] || !ids["comment-3"] {
		t.Errorf("expected comment-1 and comment-3 outstanding, got %+v", items)
	}
}

// TestListUnaddressedFeedback_SingleIdentity models the production deployment: the fleet
// and the human reviewer share one GitHub login, so classification must rely entirely on
// the marker grammar rather than Author.Login equality.
func TestListUnaddressedFeedback_SingleIdentity(t *testing.T) {
	const sharedLogin = "human"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-single-identity",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "comment-marker-acked",
              "databaseId": 1,
              "body": "This needs a nil check too",
              "createdAt": "2024-01-01T10:00:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-marker-reply",
              "databaseId": 2,
              "body": "haiku-worker: addressed in abc123 (see comment comment-marker-acked)",
              "createdAt": "2024-01-01T10:05:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-reaction-acked",
              "databaseId": 3,
              "body": "One more nit on naming",
              "createdAt": "2024-01-01T10:10:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": [
                {
                  "content": "THUMBS_UP",
                  "users": {
                    "nodes": [
                      {
                        "login": "human"
                      }
                    ]
                  }
                }
              ]
            },
            {
              "id": "comment-unmarked",
              "databaseId": 4,
              "body": "Please fix the off-by-one here",
              "createdAt": "2024-01-01T10:15:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            }
          ]
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, sharedLogin, "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	// Under the corrected contract two comments survive:
	//   - comment-marker-acked is cleared by comment-marker-reply, an exact-ID worker
	//     acknowledgment ("addressed in <sha> (see comment comment-marker-acked)").
	//   - comment-marker-reply is worker status and is not surfaced.
	//   - comment-reaction-acked has only a thumbs-up reaction, which NO LONGER
	//     acknowledges anything, so it remains outstanding.
	//   - comment-unmarked has no acknowledgment and remains outstanding.
	if len(items) != 2 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 2: %+v", len(items), items)
	}

	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	if !ids["comment-reaction-acked"] || !ids["comment-unmarked"] {
		t.Errorf("expected comment-reaction-acked and comment-unmarked outstanding, got %+v", items)
	}
}

// TestListUnaddressedFeedback_SingleIdentityThreads models the production deployment for
// inline review threads: the fleet and the human reviewer share one GitHub login, so
// thread-ack must rely on marker-prefixed last replies and thread resolution rather than
// Author.Login equality.
func TestListUnaddressedFeedback_SingleIdentityThreads(t *testing.T) {
	const sharedLogin = "human"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		if strings.Contains(bodyStr, "reviewThreads") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "thread-unresolved-human",
              "isResolved": false,
              "path": "main.go",
              "line": 10,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-thread-1a",
                    "body": "This needs fixing",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-thread-1b",
                    "body": "Still not fixed",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              }
            },
            {
              "id": "thread-marker-acked",
              "isResolved": false,
              "path": "main.go",
              "line": 20,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-thread-2a",
                    "body": "Please add a nil check",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-thread-2b",
                    "body": "haiku-worker: addressed in abc123",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              }
            },
            {
              "id": "thread-resolved",
              "isResolved": true,
              "path": "main.go",
              "line": 30,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-thread-3a",
                    "body": "Nit on naming",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-thread-3a",
                    "body": "Nit on naming",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-single-identity-threads",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, sharedLogin, "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	// Under the corrected contract completion is keyed solely on resolution state:
	//   - thread-unresolved-human survives (unresolved).
	//   - thread-marker-acked survives too: its last reply is a worker marker, but a
	//     marked/worker reply does NOT resolve a thread — only resolution does.
	//   - thread-resolved is excluded (resolved).
	if len(items) != 2 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 2: %+v", len(items), items)
	}

	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	if !ids["thread-unresolved-human"] || !ids["thread-marker-acked"] {
		t.Errorf("expected thread-unresolved-human and thread-marker-acked outstanding, got %+v", items)
	}
}

// TestListUnaddressedFeedback_ThreadLastReplyBeyondFirstPage keeps the pagination behavior
// intact: the reviewThreads query still fetches the thread's true last reply via
// comments(last: 1). Under the corrected contract completion is keyed solely on resolution
// state, so an unresolved thread surfaces regardless of who wrote its first or last reply —
// here the first reply carries a worker marker yet the unresolved thread must still be
// reported as outstanding.
func TestListUnaddressedFeedback_ThreadLastReplyBeyondFirstPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		if strings.Contains(bodyStr, "reviewThreads") {
			if !strings.Contains(bodyStr, "lastComments: comments(last: 1)") {
				t.Errorf("reviewThreads query does not request comments(last: 1) for the thread's true last reply: %s", bodyStr)
			}
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": [
            {
              "id": "thread-long",
              "isResolved": false,
              "path": "main.go",
              "line": 5,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-thread-long-first",
                    "body": "haiku-worker: addressed in abc123",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-thread-long-last",
                    "body": "Actually this regressed, please reopen",
                    "author": {
                      "login": "human"
                    }
                  }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		} else if strings.Contains(bodyStr, "comments") {
			graphqlResp := `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id-long-thread",
        "comments": {
          "pageInfo": {
            "hasNextPage": false,
            "endCursor": null
          },
          "nodes": []
        }
      }
    }
  }
}`
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(graphqlResp))
		}
	}))
	defer server.Close()

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "human", "token")

	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	if len(items) != 1 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 1: %+v", len(items), items)
	}

	if items[0].ID != "thread-long" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "thread-long")
	}
}

// listGlobalFeedback runs ListUnaddressedFeedback against a stub GitHub GraphQL
// server whose review-thread page is empty and whose global-comment page contains
// exactly the provided comment node JSON. It isolates the global-comment
// classification/acknowledgment rules for focused regression coverage.
func listGlobalFeedback(t *testing.T, botLogin, commentNodesJSON string) []FeedbackItem {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)

		if strings.Contains(bodyStr, "reviewThreads") {
			w.Write([]byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}}`))
			return
		}

		w.Write([]byte(`{"data":{"repository":{"pullRequest":{"id":"PR-node-id","comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[` + commentNodesJSON + `]}}}}}`))
	}))
	t.Cleanup(server.Close)

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	t.Cleanup(func() { GitHubBaseURL = oldBaseURL })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, botLogin, "token")
	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}
	return items
}

func globalComment(id string, dbID int, login, createdAt, body string) string {
	// body is embedded via %q so JSON string escaping is handled for us.
	return `{"id":"` + id + `","databaseId":` + itoa(dbID) + `,"body":` + quoteJSON(body) + `,"createdAt":"` + createdAt + `","author":{"login":"` + login + `"},"reactionGroups":[]}`
}

func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func idSet(items []FeedbackItem) map[string]bool {
	m := map[string]bool{}
	for _, it := range items {
		m[it.ID] = true
	}
	return m
}

// TestListUnaddressedFeedback_ReviewerRequestSharedLogin is the primary Run03 regression:
// a marked global reviewer rejection under the SHARED fleet/human login must remain visible.
// An agent marker signals authorship, never that the feedback is addressed.
func TestListUnaddressedFeedback_ReviewerRequestSharedLogin(t *testing.T) {
	nodes := globalComment("comment-req", 1, "human", "2024-01-01T10:00:00Z",
		"gpt-5.5-reviewer: CHANGES REQUESTED - fix the nil deref")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 1 {
		t.Fatalf("returned %d items, want 1 (reviewer rejection preserved): %+v", len(items), items)
	}
	if items[0].ID != "comment-req" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-req")
	}
}

// TestListUnaddressedFeedback_ReviewerRequestSeparateLogin is the same regression under a
// SEPARATE reviewer login: a marked reviewer rejection must remain visible regardless of login.
func TestListUnaddressedFeedback_ReviewerRequestSeparateLogin(t *testing.T) {
	nodes := globalComment("comment-req", 1, "gpt-reviewer-bot", "2024-01-01T10:00:00Z",
		"gpt-5.5-reviewer: CHANGES REQUESTED - fix the nil deref")

	// botLogin is the fleet's shared login "human", distinct from the reviewer's login.
	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 1 {
		t.Fatalf("returned %d items, want 1 (reviewer rejection preserved): %+v", len(items), items)
	}
	if items[0].ID != "comment-req" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-req")
	}
}

// TestListUnaddressedFeedback_ApprovalDoesNotClearRejection: a canonical reviewer approval is
// non-actionable status and must NOT clear another reviewer's outstanding CHANGES REQUESTED.
func TestListUnaddressedFeedback_ApprovalDoesNotClearRejection(t *testing.T) {
	nodes := strings.Join([]string{
		globalComment("comment-req", 1, "human", "2024-01-01T10:00:00Z",
			"gpt-5.5-reviewer: CHANGES REQUESTED - fix the nil deref"),
		globalComment("comment-appr", 2, "human", "2024-01-01T11:00:00Z",
			"opus-reviewer: APPROVED - looks good to me"),
	}, ",")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 1 {
		t.Fatalf("returned %d items, want 1 (rejection preserved despite later approval): %+v", len(items), items)
	}
	if items[0].ID != "comment-req" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-req")
	}
}

// TestListUnaddressedFeedback_ReviewerApprovalNonActionable: a standalone canonical reviewer
// approval carries no outstanding request and must not surface as feedback.
func TestListUnaddressedFeedback_ReviewerApprovalNonActionable(t *testing.T) {
	nodes := globalComment("comment-appr", 1, "human", "2024-01-01T10:00:00Z",
		"opus-reviewer: APPROVED")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 0 {
		t.Fatalf("returned %d items, want 0 (approval is non-actionable status): %+v", len(items), items)
	}
}

// TestListUnaddressedFeedback_UnknownReviewerMessageVisible: an unrecognized reviewer message
// (not a canonical approval) must remain visible rather than be discarded as fleet chatter.
func TestListUnaddressedFeedback_UnknownReviewerMessageVisible(t *testing.T) {
	nodes := globalComment("comment-unknown", 1, "human", "2024-01-01T10:00:00Z",
		"sonnet-reviewer: deferring on this until the migration lands")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 1 {
		t.Fatalf("returned %d items, want 1 (unknown reviewer message preserved): %+v", len(items), items)
	}
	if items[0].ID != "comment-unknown" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-unknown")
	}
}

// TestListUnaddressedFeedback_OneAckDoesNotClearOther: two outstanding global comments with a
// single exact-ID acknowledgment must retain the unacknowledged one; an ack of B cannot clear A.
func TestListUnaddressedFeedback_OneAckDoesNotClearOther(t *testing.T) {
	nodes := strings.Join([]string{
		globalComment("comment-a", 1, "human", "2024-01-01T10:00:00Z", "Issue A: needs fixing"),
		globalComment("comment-b", 2, "human", "2024-01-01T10:05:00Z", "Issue B: also needs fixing"),
		globalComment("comment-ack-a", 3, "human", "2024-01-01T10:10:00Z",
			"haiku-worker: addressed in abc123 (see comment comment-a)"),
	}, ",")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 1 {
		t.Fatalf("returned %d items, want 1 (only comment-a acknowledged): %+v", len(items), items)
	}
	if items[0].ID != "comment-b" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-b")
	}
}

// TestListUnaddressedFeedback_ExactIDAcknowledgmentClears: the existing writer format
// "addressed in <sha> (see comment <id>)" from a worker still acknowledges its exact target.
func TestListUnaddressedFeedback_ExactIDAcknowledgmentClears(t *testing.T) {
	nodes := strings.Join([]string{
		globalComment("comment-a", 1, "human", "2024-01-01T10:00:00Z", "Issue A: needs fixing"),
		globalComment("comment-ack-a", 2, "human", "2024-01-01T10:10:00Z",
			"haiku-worker: addressed in abc123 (see comment comment-a)"),
	}, ",")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 0 {
		t.Fatalf("returned %d items, want 0 (exact-ID acknowledgment clears comment-a): %+v", len(items), items)
	}
}

// TestListUnaddressedFeedback_WorkerNoteWithoutFixingCommitDoesNotAck: a worker reply that
// references the exact comment ID but claims NO fixing commit must not clear the feedback.
func TestListUnaddressedFeedback_WorkerNoteWithoutFixingCommitDoesNotAck(t *testing.T) {
	nodes := strings.Join([]string{
		globalComment("comment-a", 1, "human", "2024-01-01T10:00:00Z", "Issue A: needs fixing"),
		globalComment("comment-note", 2, "human", "2024-01-01T10:10:00Z",
			"haiku-worker: noted for follow-up (see comment comment-a)"),
	}, ",")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 1 {
		t.Fatalf("returned %d items, want 1 (note without fixing commit does not ack): %+v", len(items), items)
	}
	if items[0].ID != "comment-a" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-a")
	}
}

// TestListUnaddressedFeedback_NonWorkerAckDoesNotClear: only a worker acknowledgment clears
// global feedback; a reviewer/merger comment in the ack format must not.
func TestListUnaddressedFeedback_NonWorkerAckDoesNotClear(t *testing.T) {
	nodes := strings.Join([]string{
		globalComment("comment-a", 1, "human", "2024-01-01T10:00:00Z", "Issue A: needs fixing"),
		globalComment("comment-rev", 2, "human", "2024-01-01T10:10:00Z",
			"opus-reviewer: addressed in abc123 (see comment comment-a)"),
	}, ",")

	items := listGlobalFeedback(t, "human", nodes)

	// The reviewer's ack-format comment does NOT clear comment-a (only worker
	// acknowledgments do), so comment-a stays outstanding. The reviewer comment is
	// itself not a canonical approval, so it also remains visible as a reviewer
	// message — two items total.
	if len(items) != 2 {
		t.Fatalf("returned %d items, want 2: %+v", len(items), items)
	}
	ids := idSet(items)
	if !ids["comment-a"] {
		t.Errorf("comment-a must remain outstanding (reviewer comment must not acknowledge it), got %+v", items)
	}
}

// TestListUnaddressedFeedback_BareReactionDoesNotClear: a thumbs-up reaction by the fleet
// login no longer acknowledges a comment under the corrected contract.
func TestListUnaddressedFeedback_BareReactionDoesNotClear(t *testing.T) {
	nodes := `{"id":"comment-a","databaseId":1,"body":"Issue A: needs fixing","createdAt":"2024-01-01T10:00:00Z","author":{"login":"human"},"reactionGroups":[{"content":"THUMBS_UP","users":{"nodes":[{"login":"human"}]}}]}`

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 1 {
		t.Fatalf("returned %d items, want 1 (bare reaction does not acknowledge): %+v", len(items), items)
	}
	if items[0].ID != "comment-a" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-a")
	}
}

// TestListUnaddressedFeedback_WorkerAndReconcilerStatusNotFeedback: worker/merger/reconciler
// status messages are not new reviewer requests and must not surface as feedback.
func TestListUnaddressedFeedback_WorkerAndReconcilerStatusNotFeedback(t *testing.T) {
	nodes := strings.Join([]string{
		globalComment("comment-worker", 1, "human", "2024-01-01T10:00:00Z",
			"haiku-worker: pushed a rework, taking another look"),
		globalComment("comment-merger", 2, "human", "2024-01-01T10:05:00Z",
			"fable-merger: merged to main"),
		globalComment("comment-reconciler", 3, "human", "2024-01-01T10:10:00Z",
			"odonian-reconciler: bounced back to ready"),
	}, ",")

	items := listGlobalFeedback(t, "human", nodes)

	if len(items) != 0 {
		t.Fatalf("returned %d items, want 0 (fleet status is not feedback): %+v", len(items), items)
	}
}
