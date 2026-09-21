package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/tuiclient"
)

func executePRFeedback(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("pr-feedback subcommand required (list or ack)")
	}

	subcommand := args[0]
	subArgs := args[1:]

	switch subcommand {
	case "list":
		return executePRFeedbackList(ctx, subArgs, out)
	case "ack":
		return executePRFeedbackAck(ctx, subArgs)
	case "--help", "-h":
		printPRFeedbackHelp(out)
		return nil
	default:
		return fmt.Errorf("unknown pr-feedback subcommand %q (use 'list' or 'ack')", subcommand)
	}
}

func printPRFeedbackHelp(w io.Writer) {
	fmt.Fprintf(w, `odonian pr-feedback - Manage PR feedback

Subcommands:
  list <pr-url>                   List unaddressed feedback items as JSON lines
  ack [--marker PREFIX] <pr-url> <item-id> <sha>    Acknowledge a feedback item

Flags (ack only):
  --marker PREFIX                 Override worker marker prefix (e.g., 'custom-worker: ')

Environment:
  GH_TOKEN                        GitHub token (fallback if not in forge-tokens)
  FORGE_TOKENS                    Path to forge tokens file (defaults to ~/.odonian/forge-tokens)
  ODONIAN_MODEL                   Model name for default marker prefix (default: fleet)
`)
}

// fetchUnaddressedFeedback resolves a PR URL to its unaddressed pr-feedback items,
// reusing the same token/bot-login resolution as `pr-feedback list`.
func fetchUnaddressedFeedback(ctx context.Context, prURL string) ([]forge.FeedbackItem, error) {
	owner, repo, prNumber, err := parsePRURL(prURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse PR URL: %w", err)
	}

	token, err := resolveGitHubToken(owner)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve GitHub token: %w", err)
	}

	if token == "" {
		return nil, fmt.Errorf("no GitHub token found (check FORGE_TOKENS file or GH_TOKEN env)")
	}

	botLogin, err := getBotLogin(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("failed to get bot login: %w", err)
	}

	return forge.ListUnaddressedFeedback(ctx, owner, repo, prNumber, botLogin, token)
}

func executePRFeedbackList(ctx context.Context, args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("pr-url required for pr-feedback list")
	}

	items, err := fetchUnaddressedFeedback(ctx, args[0])
	if err != nil {
		return fmt.Errorf("failed to list feedback: %w", err)
	}

	for _, item := range items {
		output := map[string]interface{}{
			"kind":   item.Kind,
			"id":     item.ID,
			"path":   item.Path,
			"line":   item.Line,
			"author": item.Author,
			"body":   item.Body,
		}
		jsonBytes, err := json.Marshal(output)
		if err != nil {
			return fmt.Errorf("failed to marshal feedback item: %w", err)
		}
		fmt.Fprintln(out, string(jsonBytes))
	}

	return nil
}

func executePRFeedbackAck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ack", flag.ContinueOnError)
	markerFlag := fs.String("marker", "", "optional marker prefix override (e.g., 'custom-worker: ')")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	positional := fs.Args()
	if len(positional) < 3 {
		return fmt.Errorf("pr-feedback ack requires pr-url, item-id, and sha")
	}

	prURL := positional[0]
	itemID := positional[1]
	sha := positional[2]

	owner, repo, prNumber, err := parsePRURL(prURL)
	if err != nil {
		return fmt.Errorf("failed to parse PR URL: %w", err)
	}

	token, err := resolveGitHubToken(owner)
	if err != nil {
		return fmt.Errorf("failed to resolve GitHub token: %w", err)
	}

	if token == "" {
		return fmt.Errorf("no GitHub token found (check FORGE_TOKENS file or GH_TOKEN env)")
	}

	botLogin, err := getBotLogin(ctx, token)
	if err != nil {
		return fmt.Errorf("failed to get bot login: %w", err)
	}

	items, err := forge.ListUnaddressedFeedback(ctx, owner, repo, prNumber, botLogin, token)
	if err != nil {
		return fmt.Errorf("failed to list feedback: %w", err)
	}

	var item *forge.FeedbackItem
	for i := range items {
		if items[i].ID == itemID {
			item = &items[i]
			break
		}
	}

	if item == nil {
		return fmt.Errorf("feedback item %q not found", itemID)
	}

	marker := *markerFlag
	if marker == "" {
		model := os.Getenv("ODONIAN_MODEL")
		if model == "" {
			model = "fleet"
		}
		marker = forge.MarkerPrefix(model, "worker")
	}

	if err := forge.AcknowledgeFeedbackItem(ctx, owner, repo, prNumber, token, *item, sha, marker); err != nil {
		return fmt.Errorf("failed to acknowledge feedback item: %w", err)
	}

	return nil
}

func resolveGitHubToken(owner string) (string, error) {
	token, err := forge.OwnerToken(owner)
	if err != nil {
		return "", fmt.Errorf("failed to read forge tokens: %w", err)
	}

	if token != "" {
		return token, nil
	}

	return os.Getenv("GH_TOKEN"), nil
}

func getBotLogin(ctx context.Context, token string) (string, error) {
	url := fmt.Sprintf("%s/user", forge.GitHubBaseURL)
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

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("failed to get user info (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		Login string `json:"login"`
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %w", err)
	}

	if result.Login == "" {
		return "", fmt.Errorf("no login found in user response")
	}

	return result.Login, nil
}

// enforceFeedbackGate is the mechanical rework gate: a submit for a task that already
// carries a `pr` link and has gone through at least one review round (review_round > 0)
// must have no unaddressed pr-feedback items outstanding. This mirrors `pr-feedback list`
// rather than trusting the agent to have run it, since a half-followed prompt (ack one of
// two items, submit anyway) was observed in production on PR #314.
//
// A check that cannot run (network/token error) stops the submit with a retryable error
// rather than proceeding: an unsuccessful lookup is not evidence of zero outstanding
// items. A run03-referee incident (PR #34) showed two rework sessions where a lookup
// returned nothing and the ordinary submit accepted the unchanged commit while a marked
// reviewer rejection was still outstanding. --skip-feedback-gate remains the only way
// past this, for explicit human/emergency use — workers must not reach for it themselves.
func enforceFeedbackGate(ctx context.Context, task tuiclient.TaskDetail, skip bool, errOut io.Writer) error {
	if skip {
		fmt.Fprintln(errOut, "WARNING: --skip-feedback-gate set; bypassing the PR feedback gate check")
		return nil
	}

	if task.ReviewRound <= 0 {
		return nil
	}

	var prURL string
	for _, link := range task.Links {
		if link.Kind == "pr" {
			prURL = link.Value
			break
		}
	}
	if prURL == "" {
		return nil
	}

	items, err := fetchUnaddressedFeedback(ctx, prURL)
	if err != nil {
		return fmt.Errorf("could not check pr-feedback gate on %s (%w); this is retryable — retry the submit once the lookup succeeds, or pass --skip-feedback-gate to bypass (humans/emergencies only)", prURL, err)
	}

	if len(items) == 0 {
		return nil
	}

	fmt.Fprintf(errOut, "submit blocked: %d unaddressed pr-feedback item(s) remain on %s\n", len(items), prURL)
	for _, item := range items {
		fmt.Fprintf(errOut, "  - [%s] %s: %s\n", item.Kind, item.ID, firstLine(item.Body))
	}

	return fmt.Errorf("fix the items above and run `odonian pr-feedback ack %s <item-id> <sha>` for each, then submit again (or pass --skip-feedback-gate to bypass)", prURL)
}

// firstLine returns the first line of s, for compact display of feedback item bodies.
func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
