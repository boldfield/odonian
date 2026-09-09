package forge

import (
	"fmt"
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
