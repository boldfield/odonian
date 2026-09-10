package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RateLimitError signals that a GitHub API call failed because of rate limiting.
// GitHub reports both the primary hourly quota and the secondary abuse-detection
// limit as 403s (only some endpoints use 429), so detection has to look at the
// body rather than the status code alone. Reset carries the value of the
// X-RateLimit-Reset response header when GitHub sent one; it is the zero Time
// when absent, in which case the caller should fall back to its own cool-off.
type RateLimitError struct {
	StatusCode int
	Reset      time.Time
	Body       string
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("API request failed with status %d: %s", e.StatusCode, e.Body)
}

// QuotaInfo holds information about a GitHub API token's remaining quota.
type QuotaInfo struct {
	Remaining int
	Reset     time.Time
}

// GetRemainingQuota returns the remaining core request count and reset time for a GitHub token
// without consuming any quota (GET /rate_limit is a free endpoint).
func GetRemainingQuota(ctx context.Context, token string) (*QuotaInfo, error) {
	url := fmt.Sprintf("%s/rate_limit", GitHubBaseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		if rle := rateLimitError(resp, respBody); rle != nil {
			return nil, rle
		}
		return nil, fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var data struct {
		Resources struct {
			Core struct {
				Remaining int `json:"remaining"`
				Reset     int `json:"reset"`
			} `json:"core"`
		} `json:"resources"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return &QuotaInfo{
		Remaining: data.Resources.Core.Remaining,
		Reset:     time.Unix(int64(data.Resources.Core.Reset), 0),
	}, nil
}

// rateLimitError inspects a non-2xx GitHub response and returns a *RateLimitError
// when it identifies a rate-limit response: any 429, or a 403 whose body mentions
// "rate limit" (GitHub also returns plain permission-denied 403s for private repos
// and the like, which must not be treated as rate limiting). Returns nil otherwise.
func rateLimitError(resp *http.Response, body []byte) *RateLimitError {
	isRateLimit := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && strings.Contains(strings.ToLower(string(body)), "rate limit"))
	if !isRateLimit {
		return nil
	}

	rle := &RateLimitError{StatusCode: resp.StatusCode, Body: string(body)}
	if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
		if unixSec, err := strconv.ParseInt(reset, 10, 64); err == nil {
			rle.Reset = time.Unix(unixSec, 0)
		}
	}
	return rle
}
