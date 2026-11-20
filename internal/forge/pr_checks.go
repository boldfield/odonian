package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// CheckContext is one entry in a commit's check rollup: a check run or a
// commit status.
type CheckContext struct {
	Kind       string // "check_run" or "status"
	Name       string // check run name, or status context
	Status     string // check run status (e.g. COMPLETED, IN_PROGRESS); empty for statuses
	Conclusion string // check run conclusion (e.g. SUCCESS, FAILURE); for statuses, the state (SUCCESS, PENDING, FAILURE, ERROR, EXPECTED)
	URL        string
}

// PRChecks is the checks view of a pull request's head commit.
type PRChecks struct {
	State       string // "open", "closed" or "merged"
	HeadSHA     string
	RollupState string // statusCheckRollup.state; empty when the commit has no rollup
	Contexts    []CheckContext
}

const prChecksQuery = `query($owner: String!, $repo: String!, $number: Int!, $after: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $number) {
      state
      merged
      headRefOid
      commits(last: 1) {
        nodes {
          commit {
            oid
            statusCheckRollup {
              state
              contexts(first: 100, after: $after) {
                pageInfo {
                  hasNextPage
                  endCursor
                }
                nodes {
                  __typename
                  ... on CheckRun {
                    name
                    status
                    conclusion
                    detailsUrl
                  }
                  ... on StatusContext {
                    context
                    state
                    targetUrl
                  }
                }
              }
            }
          }
        }
      }
    }
  }
}`

type prChecksPage struct {
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
	Data struct {
		Repository *struct {
			PullRequest *struct {
				State      string `json:"state"`
				Merged     bool   `json:"merged"`
				HeadRefOid string `json:"headRefOid"`
				Commits    struct {
					Nodes []struct {
						Commit struct {
							Oid               string `json:"oid"`
							StatusCheckRollup *struct {
								State    string `json:"state"`
								Contexts struct {
									PageInfo struct {
										HasNextPage bool   `json:"hasNextPage"`
										EndCursor   string `json:"endCursor"`
									} `json:"pageInfo"`
									Nodes []struct {
										Typename   string `json:"__typename"`
										Name       string `json:"name"`
										Status     string `json:"status"`
										Conclusion string `json:"conclusion"`
										DetailsURL string `json:"detailsUrl"`
										Context    string `json:"context"`
										State      string `json:"state"`
										TargetURL  string `json:"targetUrl"`
									} `json:"nodes"`
								} `json:"contexts"`
							} `json:"statusCheckRollup"`
						} `json:"commit"`
					} `json:"nodes"`
				} `json:"commits"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

// GetPRChecks reads a pull request's state, head commit and the head commit's
// check rollup in one GraphQL request (more only when the contexts overflow a
// page). Contexts cover both check runs and commit statuses. A rate-limit
// response returns a *RateLimitError.
func GetPRChecks(ctx context.Context, owner, repo string, prNumber int, token string) (*PRChecks, error) {
	out := &PRChecks{}
	after := ""
	for {
		page, err := fetchPRChecksPage(ctx, owner, repo, prNumber, after, token)
		if err != nil {
			return nil, err
		}
		if page.Data.Repository == nil || page.Data.Repository.PullRequest == nil {
			return nil, fmt.Errorf("pull request %s/%s#%d not found", owner, repo, prNumber)
		}
		pr := page.Data.Repository.PullRequest
		switch {
		case pr.Merged || pr.State == "MERGED":
			out.State = "merged"
		case pr.State == "CLOSED":
			out.State = "closed"
		default:
			out.State = "open"
		}
		out.HeadSHA = pr.HeadRefOid

		if len(pr.Commits.Nodes) == 0 {
			return out, nil
		}
		commit := pr.Commits.Nodes[0].Commit
		if out.HeadSHA == "" {
			out.HeadSHA = commit.Oid
		}
		rollup := commit.StatusCheckRollup
		if rollup == nil {
			return out, nil
		}
		out.RollupState = rollup.State
		for _, n := range rollup.Contexts.Nodes {
			if n.Typename == "StatusContext" {
				out.Contexts = append(out.Contexts, CheckContext{
					Kind: "status", Name: n.Context, Conclusion: n.State, URL: n.TargetURL,
				})
				continue
			}
			out.Contexts = append(out.Contexts, CheckContext{
				Kind: "check_run", Name: n.Name, Status: n.Status, Conclusion: n.Conclusion, URL: n.DetailsURL,
			})
		}
		if !rollup.Contexts.PageInfo.HasNextPage || rollup.Contexts.PageInfo.EndCursor == "" {
			return out, nil
		}
		after = rollup.Contexts.PageInfo.EndCursor
	}
}

func fetchPRChecksPage(ctx context.Context, owner, repo string, prNumber int, after, token string) (*prChecksPage, error) {
	vars := map[string]any{"owner": owner, "repo": repo, "number": prNumber, "after": nil}
	if after != "" {
		vars["after"] = after
	}
	body, err := json.Marshal(map[string]any{"query": prChecksQuery, "variables": vars})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, GitHubBaseURL+"/graphql", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		if rle := rateLimitError(resp, respBody); rle != nil {
			return nil, rle
		}
		return nil, fmt.Errorf("graphql request failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var page prChecksPage
	if err := json.Unmarshal(respBody, &page); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	// GraphQL reports an exhausted quota as a 200 with a RATE_LIMITED error.
	for _, e := range page.Errors {
		if e.Type == "RATE_LIMITED" || strings.Contains(strings.ToLower(e.Message), "rate limit") {
			rle := &RateLimitError{StatusCode: resp.StatusCode, Body: string(respBody)}
			if hdr := rateLimitError(&http.Response{StatusCode: http.StatusTooManyRequests, Header: resp.Header}, nil); hdr != nil {
				rle.Reset = hdr.Reset
			}
			return nil, rle
		}
	}
	if len(page.Errors) > 0 {
		return nil, fmt.Errorf("graphql errors: %s", page.Errors[0].Message)
	}
	return &page, nil
}
