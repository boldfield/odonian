package forge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// TestListUnaddressedFeedback_BareReactionDoesNotAcknowledge covers the approved repair's
// correction that a bare reaction can never clear feedback: only an explicit, exact-ID worker
// reply does. Both comments below remain outstanding.
func TestListUnaddressedFeedback_BareReactionDoesNotAcknowledge(t *testing.T) {
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
              "body": "Feedback with bot reaction",
              "createdAt": "2024-01-01T10:00:00Z",
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

	// Neither comment has an explicit exact-ID worker acknowledgment, so both remain —
	// including comment-2, whose thumbs-up reaction is no longer treated as an ack.
	if len(items) != 2 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 2: %+v", len(items), items)
	}

	if items[0].ID != "comment-1" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-1")
	}

	if items[1].ID != "comment-2" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "comment-2")
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

// TestListUnaddressedFeedback_UnrelatedWorkerReplyDoesNotAcknowledge covers the approved
// repair's correction that "any later marker reply" is no longer sufficient to acknowledge
// earlier feedback: only an explicit worker reply naming the exact original comment's ID
// clears it. comment-2 is a worker reply but does not reference comment-1's ID, so comment-1
// remains outstanding.
func TestListUnaddressedFeedback_UnrelatedWorkerReplyDoesNotAcknowledge(t *testing.T) {
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

	// comment-2 is itself worker chatter (excluded), but it does not name comment-1's exact
	// ID, so comment-1 remains outstanding alongside comment-3.
	if len(items) != 2 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 2: %+v", len(items), items)
	}

	if items[0].ID != "comment-1" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-1")
	}

	if items[1].ID != "comment-3" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "comment-3")
	}
}

// TestListUnaddressedFeedback_SingleIdentity models the production deployment: the fleet
// and the human reviewer share one GitHub login, so classification must rely entirely on
// the marker grammar rather than Author.Login equality. It also covers the approved repair's
// correction that neither an unrelated worker reply nor a bare reaction can clear feedback —
// only an explicit, exact-ID worker acknowledgment can.
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
              "id": "comment-needs-nil-check",
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
              "body": "haiku-worker: addressed in abc123",
              "createdAt": "2024-01-01T10:05:00Z",
              "author": {
                "login": "human"
              },
              "reactionGroups": []
            },
            {
              "id": "comment-needs-naming-fix",
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

	// comment-marker-reply is itself worker chatter (excluded), but it does not name any
	// comment's exact ID, so it acknowledges nothing. comment-needs-naming-fix's thumbs-up
	// reaction is no longer treated as an ack either. All three human comments survive.
	if len(items) != 3 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 3: %+v", len(items), items)
	}

	wantIDs := []string{"comment-needs-nil-check", "comment-needs-naming-fix", "comment-unmarked"}
	for i, want := range wantIDs {
		if i >= len(items) {
			break
		}
		if items[i].ID != want {
			t.Errorf("items[%d].ID = %q, want %q", i, items[i].ID, want)
		}
	}
}

// TestListUnaddressedFeedback_SingleIdentityThreads models the production deployment for
// inline review threads: the fleet and the human reviewer share one GitHub login, so
// completion must rely on GitHub's thread-resolution state rather than Author.Login equality.
// It also covers the approved repair's correction that a marker-prefixed reply left in an
// unresolved thread does not itself resolve it — only thread.IsResolved does.
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
              "id": "thread-marked-reply-still-unresolved",
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

	// thread-unresolved-human and thread-marked-reply-still-unresolved both survive: neither
	// is resolved, and a marker-prefixed last reply does not resolve a thread by itself.
	// thread-resolved is excluded because it is resolved. All comments share one login, so
	// only isResolved distinguishes them.
	if len(items) != 2 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 2: %+v", len(items), items)
	}

	wantIDs := []string{"thread-unresolved-human", "thread-marked-reply-still-unresolved"}
	for i, want := range wantIDs {
		if i >= len(items) {
			break
		}
		if items[i].ID != want {
			t.Errorf("items[%d].ID = %q, want %q", i, items[i].ID, want)
		}
	}
}

// TestListUnaddressedFeedback_ThreadLastReplyBeyondFirstPage guards against the pagination
// boundary bug: fetching only the first N replies and treating the last of that page as the
// thread's last reply misclassifies long threads. The thread's earliest reply (what a
// first-N-only fetch would still see as "last" among the first page) is marker-prefixed, but
// the actual last reply — reachable only via comments(last: 1) — is an unmarked human reply.
// Classification must follow the true last reply and report the thread as unaddressed.
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

// newGlobalCommentsServer returns a mock GitHub GraphQL server with no review threads and the
// given global-comment nodes (a JSON array literal of comment objects).
func newGlobalCommentsServer(t *testing.T, prNodeID, commentNodesJSON string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		if strings.Contains(bodyStr, "reviewThreads") {
			fmt.Fprint(w, `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": { "hasNextPage": false, "endCursor": null },
          "nodes": []
        }
      }
    }
  }
}`)
		} else if strings.Contains(bodyStr, "comments") {
			fmt.Fprintf(w, `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": %q,
        "comments": {
          "pageInfo": { "hasNextPage": false, "endCursor": null },
          "nodes": %s
        }
      }
    }
  }
}`, prNodeID, commentNodesJSON)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestListUnaddressedFeedback_GlobalReviewerRequestPreserved is the direct regression for the
// Run03 Referee incident: a marked global reviewer rejection ("gpt-5.5-reviewer: CHANGES
// REQUESTED") must remain visible, under both a shared and a separate GitHub login for the
// reviewer relative to the bot/worker identity.
func TestListUnaddressedFeedback_GlobalReviewerRequestPreserved(t *testing.T) {
	for _, tc := range []struct {
		name     string
		author   string
		botLogin string
	}{
		{name: "shared login", author: "human", botLogin: "human"},
		{name: "separate login", author: "human", botLogin: "gpt-5-5-bot-account"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			commentNodes := fmt.Sprintf(`[
  {
    "id": "comment-rejection",
    "databaseId": 1,
    "body": "gpt-5.5-reviewer: CHANGES REQUESTED - the nil check is still missing",
    "createdAt": "2024-01-01T10:00:00Z",
    "author": { "login": %q },
    "reactionGroups": []
  }
]`, tc.author)

			server := newGlobalCommentsServer(t, "PR-node-id", commentNodes)

			oldBaseURL := GitHubBaseURL
			GitHubBaseURL = server.URL
			defer func() { GitHubBaseURL = oldBaseURL }()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, tc.botLogin, "token")
			if err != nil {
				t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
			}

			if len(items) != 1 {
				t.Fatalf("ListUnaddressedFeedback() returned %d items, want 1: %+v", len(items), items)
			}
			if items[0].ID != "comment-rejection" {
				t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-rejection")
			}
		})
	}
}

// TestListUnaddressedFeedback_ReviewerApprovalDoesNotClearAnotherReviewersRejection covers the
// approved repair's requirement that a canonical reviewer approval is non-actionable status,
// but it must never clear a different reviewer's outstanding request.
func TestListUnaddressedFeedback_ReviewerApprovalDoesNotClearAnotherReviewersRejection(t *testing.T) {
	commentNodes := `[
  {
    "id": "comment-rejection",
    "databaseId": 1,
    "body": "gpt-5.5-reviewer: CHANGES REQUESTED - please add a test",
    "createdAt": "2024-01-01T10:00:00Z",
    "author": { "login": "human" },
    "reactionGroups": []
  },
  {
    "id": "comment-approval",
    "databaseId": 2,
    "body": "opus-reviewer: APPROVED",
    "createdAt": "2024-01-01T11:00:00Z",
    "author": { "login": "human" },
    "reactionGroups": []
  }
]`

	server := newGlobalCommentsServer(t, "PR-node-id", commentNodes)

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "human", "token")
	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	// comment-approval is non-actionable status (excluded); it must not clear comment-rejection.
	if len(items) != 1 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 1: %+v", len(items), items)
	}
	if items[0].ID != "comment-rejection" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-rejection")
	}
}

// TestListUnaddressedFeedback_ExactIDAcknowledgmentClearsOnlyItsTarget covers the approved
// repair's exact-ID acknowledgment rule: a worker reply in the existing writer's format
// ("addressed in <sha> (see comment <id>)") clears only the comment it names, and an old
// exact-ID acknowledgment message continues to work.
func TestListUnaddressedFeedback_ExactIDAcknowledgmentClearsOnlyItsTarget(t *testing.T) {
	commentNodes := `[
  {
    "id": "comment-a",
    "databaseId": 1,
    "body": "Fix the nil check here",
    "createdAt": "2024-01-01T10:00:00Z",
    "author": { "login": "human" },
    "reactionGroups": []
  },
  {
    "id": "comment-b",
    "databaseId": 2,
    "body": "Fix the off-by-one there",
    "createdAt": "2024-01-01T10:05:00Z",
    "author": { "login": "human" },
    "reactionGroups": []
  },
  {
    "id": "comment-ack",
    "databaseId": 3,
    "body": "haiku-worker: addressed in abc123def (see comment comment-a)",
    "createdAt": "2024-01-01T10:10:00Z",
    "author": { "login": "human" },
    "reactionGroups": []
  }
]`

	server := newGlobalCommentsServer(t, "PR-node-id", commentNodes)

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "human", "token")
	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	// comment-a is acknowledged by exact ID; comment-b is a different comment and remains.
	if len(items) != 1 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 1: %+v", len(items), items)
	}
	if items[0].ID != "comment-b" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-b")
	}
}

// TestListUnaddressedFeedback_UnknownReviewerMessagePreserved covers the approved repair's
// requirement that an unrecognized reviewer message (neither a canonical approval nor an
// obvious rejection) is preserved rather than discarded as fleet chatter, and that it does not
// acknowledge an unrelated comment.
func TestListUnaddressedFeedback_UnknownReviewerMessagePreserved(t *testing.T) {
	commentNodes := `[
  {
    "id": "comment-request",
    "databaseId": 1,
    "body": "Fix the nil check here",
    "createdAt": "2024-01-01T10:00:00Z",
    "author": { "login": "human" },
    "reactionGroups": []
  },
  {
    "id": "comment-unknown-reviewer",
    "databaseId": 2,
    "body": "gpt-5.5-reviewer: Looks mostly fine, one thing to consider next time",
    "createdAt": "2024-01-01T10:05:00Z",
    "author": { "login": "human" },
    "reactionGroups": []
  }
]`

	server := newGlobalCommentsServer(t, "PR-node-id", commentNodes)

	oldBaseURL := GitHubBaseURL
	GitHubBaseURL = server.URL
	defer func() { GitHubBaseURL = oldBaseURL }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	items, err := ListUnaddressedFeedback(ctx, "owner", "repo", 42, "human", "token")
	if err != nil {
		t.Fatalf("ListUnaddressedFeedback() error = %v, want nil", err)
	}

	// Neither comment is a canonical approval or a worker/merger/reconciler status message,
	// and comment-unknown-reviewer does not reference comment-request's exact ID, so both
	// remain outstanding.
	if len(items) != 2 {
		t.Fatalf("ListUnaddressedFeedback() returned %d items, want 2: %+v", len(items), items)
	}
	if items[0].ID != "comment-request" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "comment-request")
	}
	if items[1].ID != "comment-unknown-reviewer" {
		t.Errorf("items[1].ID = %q, want %q", items[1].ID, "comment-unknown-reviewer")
	}
}

// TestListUnaddressedFeedback_MarkedUnresolvedThreadRemainsVisible covers the approved
// repair's requirement that a marked reviewer comment opening an unresolved inline thread
// remains visible — resolution state, not the marker, governs completion.
func TestListUnaddressedFeedback_MarkedUnresolvedThreadRemainsVisible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyStr := string(body)

		if strings.Contains(bodyStr, "reviewThreads") {
			fmt.Fprint(w, `{
  "data": {
    "repository": {
      "pullRequest": {
        "reviewThreads": {
          "pageInfo": { "hasNextPage": false, "endCursor": null },
          "nodes": [
            {
              "id": "thread-marked-request",
              "isResolved": false,
              "path": "main.go",
              "line": 7,
              "firstComments": {
                "nodes": [
                  {
                    "id": "comment-thread-marked",
                    "body": "gpt-5.5-reviewer: CHANGES REQUESTED - nil check missing here",
                    "author": { "login": "human" }
                  }
                ]
              },
              "lastComments": {
                "nodes": [
                  {
                    "id": "comment-thread-marked",
                    "body": "gpt-5.5-reviewer: CHANGES REQUESTED - nil check missing here",
                    "author": { "login": "human" }
                  }
                ]
              }
            }
          ]
        }
      }
    }
  }
}`)
		} else if strings.Contains(bodyStr, "comments") {
			fmt.Fprint(w, `{
  "data": {
    "repository": {
      "pullRequest": {
        "id": "PR-node-id",
        "comments": {
          "pageInfo": { "hasNextPage": false, "endCursor": null },
          "nodes": []
        }
      }
    }
  }
}`)
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
	if items[0].ID != "thread-marked-request" {
		t.Errorf("items[0].ID = %q, want %q", items[0].ID, "thread-marked-request")
	}
}

// TestIsNonActionableGlobalComment covers the classification rule directly: unmarked comments
// and reviewer messages other than a canonical approval are always actionable; worker,
// merger, and reconciler markers are always non-actionable; and a reviewer approval must be
// the literal word "APPROVED" at the start of the message, not a substring match.
func TestIsNonActionableGlobalComment(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "unmarked human comment", body: "Please fix this", want: false},
		{name: "worker status", body: "haiku-worker: addressed in abc123", want: true},
		{name: "merger status", body: "fable-merger: Merging now", want: true},
		{name: "reconciler status", body: "odonian-reconciler: bouncing back", want: true},
		{name: "reviewer rejection", body: "gpt-5.5-reviewer: CHANGES REQUESTED", want: false},
		{name: "reviewer unknown message", body: "gpt-5.5-reviewer: seems fine overall", want: false},
		{name: "reviewer canonical approval", body: "gpt-5.5-reviewer: APPROVED", want: true},
		{name: "reviewer approval lowercase", body: "gpt-5.5-reviewer: approved, nice work", want: true},
		{name: "reviewer approval-prefixed word is not canonical", body: "gpt-5.5-reviewer: APPROVEDLY not a real verdict", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isNonActionableGlobalComment(tt.body)
			if got != tt.want {
				t.Errorf("isNonActionableGlobalComment(%q) = %v, want %v", tt.body, got, tt.want)
			}
		})
	}
}
