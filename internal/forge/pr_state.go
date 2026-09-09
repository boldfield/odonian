package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// GetPRState returns the state of a GitHub PR: "merged", "closed", or "open".
// It queries the GitHub API endpoint GET /repos/{owner}/{repo}/pulls/{prNumber}.
func GetPRState(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", GitHubBaseURL, owner, repo, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		if rle := rateLimitError(resp, respBody); rle != nil {
			return "", rle
		}
		return "", fmt.Errorf("API request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var prData struct {
		MergedAt *string `json:"merged_at"`
		State    string  `json:"state"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&prData); err != nil {
		return "", fmt.Errorf("failed to decode response: %w", err)
	}

	if prData.MergedAt != nil {
		return "merged", nil
	}
	if prData.State == "closed" {
		return "closed", nil
	}
	return "open", nil
}
