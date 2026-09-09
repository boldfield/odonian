package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// GetReviewDecision queries GitHub's GraphQL API to get the review decision of a PR.
// It returns the normalized review decision ("approved", "changes_requested", or "pending")
// and the timestamp of the most recent review (zero time if none).
func GetReviewDecision(ctx context.Context, owner, repo string, prNumber int, token string) (decision string, latestReviewAt time.Time, err error) {
	const graphqlQuery = `query {
  repository(owner: "%s", name: "%s") {
    pullRequest(number: %d) {
      reviewDecision
      reviews(last: 1) {
        nodes {
          submittedAt
        }
      }
    }
  }
}`

	queryStr := fmt.Sprintf(graphqlQuery, owner, repo, prNumber)
	payload := map[string]string{"query": queryStr}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to marshal request body: %w", err)
	}

	url := fmt.Sprintf("%s/graphql", GitHubBaseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		if rle := rateLimitError(resp, respBody); rle != nil {
			return "", time.Time{}, rle
		}
		return "", time.Time{}, fmt.Errorf("graphql request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ReviewDecision string `json:"reviewDecision"`
					Reviews        struct {
						Nodes []struct {
							SubmittedAt time.Time `json:"submittedAt"`
						} `json:"nodes"`
					} `json:"reviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", time.Time{}, fmt.Errorf("failed to parse response: %w", err)
	}

	reviewDecision := result.Data.Repository.PullRequest.ReviewDecision
	switch reviewDecision {
	case "APPROVED":
		decision = "approved"
	case "CHANGES_REQUESTED":
		decision = "changes_requested"
	case "REVIEW_REQUIRED", "":
		decision = "pending"
	default:
		decision = "pending"
	}

	if len(result.Data.Repository.PullRequest.Reviews.Nodes) > 0 {
		latestReviewAt = result.Data.Repository.PullRequest.Reviews.Nodes[0].SubmittedAt
	}

	return decision, latestReviewAt, nil
}
