package forge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func withGitHubBaseURL(t *testing.T, url string) {
	t.Helper()
	old := GitHubBaseURL
	GitHubBaseURL = url
	t.Cleanup(func() { GitHubBaseURL = old })
}

func TestGetPRStateRateLimit403WithResetHeader(t *testing.T) {
	resetAt := time.Unix(1893456000, 0) // arbitrary future unix time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message": "API rate limit exceeded for 12.34.56.78."}`)
	}))
	defer server.Close()
	withGitHubBaseURL(t, server.URL)

	_, err := GetPRState(context.Background(), "owner", "repo", 1, "token")
	if err == nil {
		t.Fatal("expected error")
	}

	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	if rle.StatusCode != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rle.StatusCode)
	}
	if !rle.Reset.Equal(resetAt) {
		t.Errorf("expected reset %v, got %v", resetAt, rle.Reset)
	}
}

func TestGetPRStateRateLimit429WithoutResetHeader(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"message": "too many requests"}`)
	}))
	defer server.Close()
	withGitHubBaseURL(t, server.URL)

	_, err := GetPRState(context.Background(), "owner", "repo", 1, "token")
	if err == nil {
		t.Fatal("expected error")
	}

	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	if rle.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", rle.StatusCode)
	}
	if !rle.Reset.IsZero() {
		t.Errorf("expected zero Reset when header absent, got %v", rle.Reset)
	}
}

func TestGetPRStatePlain403IsNotRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message": "Must have admin rights to Repository."}`)
	}))
	defer server.Close()
	withGitHubBaseURL(t, server.URL)

	_, err := GetPRState(context.Background(), "owner", "repo", 1, "token")
	if err == nil {
		t.Fatal("expected error")
	}

	var rle *RateLimitError
	if errors.As(err, &rle) {
		t.Fatalf("expected a plain error, not *RateLimitError, got %v", rle)
	}
}

func TestGetReviewDecisionRateLimit403WithResetHeader(t *testing.T) {
	resetAt := time.Unix(1893456000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message": "API rate limit exceeded for 12.34.56.78."}`)
	}))
	defer server.Close()
	withGitHubBaseURL(t, server.URL)

	_, _, err := GetReviewDecision(context.Background(), "owner", "repo", 1, "token")
	if err == nil {
		t.Fatal("expected error")
	}

	var rle *RateLimitError
	if !errors.As(err, &rle) {
		t.Fatalf("expected *RateLimitError, got %T: %v", err, err)
	}
	if !rle.Reset.Equal(resetAt) {
		t.Errorf("expected reset %v, got %v", resetAt, rle.Reset)
	}
}
