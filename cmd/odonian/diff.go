package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/boldfield/odonian/internal/localcommit"
	"github.com/boldfield/odonian/internal/tuiclient"
)

func executeDiff(ctx context.Context, baseURL, token string, args []string, out io.Writer) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	// Parse flags
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fullFlag := fs.Bool("full", false, "show full commit (local_commit mode only)")
	repoFlag := fs.String("repo", "", "repository directory")
	positionals, err := parseFlagsWithPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	if len(positionals) < 1 {
		return fmt.Errorf("task id required")
	}
	taskID := positionals[0]

	client := tuiclient.NewHTTPClient(baseURL, token)
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}

	// Local commit mode
	if localcommit.IsLocalCommit() {
		// The commit under review is the current round's; earlier rounds' commit links remain.
		commitSHA, err := reviewedCommit(task)
		if err != nil {
			return err
		}

		// Resolve repoDir from --repo flag or ODONIAN_REPO
		repoDir := *repoFlag
		if repoDir == "" {
			repoDir = os.Getenv("ODONIAN_REPO")
		}
		if repoDir == "" {
			return fmt.Errorf("--repo flag or ODONIAN_REPO environment variable required")
		}

		var output string
		if *fullFlag {
			output, err = localcommit.ShowCommit(repoDir, commitSHA)
		} else {
			output, err = localcommit.DiffBase(repoDir, localcommit.BranchSlug(task.Branch, task.Title), commitSHA)
		}
		if err != nil {
			return fmt.Errorf("failed to get diff/show: %w", err)
		}

		fmt.Fprint(out, output)
		return nil
	}

	// PR mode
	var prURL string
	for _, link := range task.Links {
		if link.Kind == "pr" {
			prURL = link.Value
			break
		}
	}

	if prURL == "" {
		return fmt.Errorf("task has no pull request link")
	}

	fmt.Fprintln(out, prURL)

	// Check if gh is available and run gh pr diff if it is (best-effort)
	if _, err := exec.LookPath("gh"); err == nil {
		cmd := exec.CommandContext(ctx, "gh", "pr", "diff", prURL)
		cmd.Stdout = out
		cmd.Stderr = io.Discard
		_ = cmd.Run()
	}

	return nil
}
