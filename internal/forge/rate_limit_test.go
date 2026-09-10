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

func TestGetRemainingQuota200WithRealisticBody(t *testing.T) {
	resetAt := time.Unix(1893456000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		if r.URL.Path != "/rate_limit" {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		if r.Header.Get("Accept") != "application/vnd.github.v3+json" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if r.Header.Get("Authorization") != "token token123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"resources": {"core": {"remaining": 4500, "reset": %d}}}`, resetAt.Unix())
	}))
	defer server.Close()
	withGitHubBaseURL(t, server.URL)

	quota, err := GetRemainingQuota(context.Background(), "token123")
	if err != nil {
		t.Fatalf("GetRemainingQuota failed: %v", err)
	}
	if quota.Remaining != 4500 {
		t.Errorf("expected remaining 4500, got %d", quota.Remaining)
	}
	if !quota.Reset.Equal(resetAt) {
		t.Errorf("expected reset %v, got %v", resetAt, quota.Reset)
	}
}

func TestGetRemainingQuota403WithRateLimitBody(t *testing.T) {
	resetAt := time.Unix(1893456000, 0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message": "API rate limit exceeded for 12.34.56.78."}`)
	}))
	defer server.Close()
	withGitHubBaseURL(t, server.URL)

	_, err := GetRemainingQuota(context.Background(), "token")
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

func TestGetRemainingQuota500Error(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message": "Server error"}`)
	}))
	defer server.Close()
	withGitHubBaseURL(t, server.URL)

	_, err := GetRemainingQuota(context.Background(), "token")
	if err == nil {
		t.Fatal("expected error")
	}

	var rle *RateLimitError
	if errors.As(err, &rle) {
		t.Fatalf("expected a plain error, not *RateLimitError, got %v", rle)
	}
}
