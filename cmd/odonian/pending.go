package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"text/tabwriter"

	"github.com/boldfield/odonian/internal/tuiclient"
)

func executePending(ctx context.Context, baseURL, token string, jsonOutput bool, args []string, out io.Writer) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	fs := flag.NewFlagSet("pending", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	projectFlag := fs.String("project", "", "project ID")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	if *projectFlag == "" {
		return fmt.Errorf("--project flag is required")
	}

	client := tuiclient.NewHTTPClient(baseURL, token)

	// Fetch tasks in review state
	reviewTasks, err := client.ListTasks(ctx, *projectFlag, tuiclient.WithState("review"), tuiclient.WithFields("summary"))
	if err != nil {
		return fmt.Errorf("failed to list tasks: %w", err)
	}

	// Fetch tasks in approved state
	approvedTasks, err := client.ListTasks(ctx, *projectFlag, tuiclient.WithState("approved"), tuiclient.WithFields("summary"))
	if err != nil {
		return fmt.Errorf("failed to list tasks: %w", err)
	}

	// Combine results and sort by created_at then id to preserve chronological order
	filtered := append(reviewTasks, approvedTasks...)
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].CreatedAt != filtered[j].CreatedAt {
			return filtered[i].CreatedAt < filtered[j].CreatedAt
		}
		return filtered[i].ID < filtered[j].ID
	})

	// Output results
	if jsonOutput {
		output, err := json.MarshalIndent(filtered, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Fprintln(out, string(output))
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATE\tKIND\tTITLE")
		for _, task := range filtered {
			id := task.ID
			if len(id) > 8 {
				id = id[:8]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", id, task.State, task.Kind, task.Title)
		}
		w.Flush()
	}

	return nil
}
