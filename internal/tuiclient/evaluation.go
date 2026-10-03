package tuiclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// Evaluation sends one request to an /evaluation/... endpoint and returns the
// raw JSON response. A non-2xx response is an *APIError carrying the stable
// error code. The evaluation surface is deliberately a single generic call so
// the TUI Client interface, which has no use for it, stays unchanged.
func (c *HTTPClient) Evaluation(ctx context.Context, method, path string, body interface{}) (json.RawMessage, error) {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("response is not valid JSON")
	}
	return raw, nil
}
