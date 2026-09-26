package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/boldfield/odonian/internal/api"
	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/localcommit"
	"github.com/boldfield/odonian/internal/notify"
	"github.com/boldfield/odonian/internal/prwatch"
	"github.com/boldfield/odonian/internal/reconcile"
	"github.com/boldfield/odonian/internal/store"
	"github.com/boldfield/odonian/internal/tuiclient"
)

var version = "dev"

// noopNotifier discards notifications. It's used when NOTIFY_URL isn't configured, so
// that PR-watch reconciliation (including stale pull request cleanup) still runs.
type noopNotifier struct{}

func (noopNotifier) Publish(ctx context.Context, n notify.Notification) error {
	return nil
}

type claimError struct {
	message string
	code    int
}

func (e *claimError) Error() string {
	return e.message
}

type handledError struct {
	code int
}

func (e *handledError) Error() string {
	return "handled"
}

func main() {
	if err := run(os.Args); err != nil {
		var claimErr *claimError
		if errors.As(err, &claimErr) {
			fmt.Fprintf(os.Stderr, "error: %v\n", claimErr.Error())
			os.Exit(claimErr.code)
		}
		var handledErr *handledError
		if errors.As(err, &handledErr) {
			os.Exit(handledErr.code)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		printUsage()
		return nil
	}

	switch args[1] {
	case "server":
		runServer()
		return nil
	case "-h", "--help", "help":
		printUsage()
		return nil
	case "projects", "tasks", "show", "claim", "submit", "heartbeat", "next", "promote", "transition", "project", "merge", "pending", "diff", "approve", "reject", "wt-ensure", "pr-feedback":
		return runClient(args[1], args[2:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", args[1])
		printUsageWriter(os.Stderr)
		return &handledError{code: 1}
	}
}

func printUsage() {
	printUsageWriter(os.Stdout)
}

func printUsageWriter(w io.Writer) {
	fmt.Fprintf(w, `odonian version %s

usage: odonian <command> [options]

Commands:
  server                 Start the odonian server
  projects               List all projects
  project                Show project details
  tasks                  List tasks for a project
  pending                List pending tasks in review or approved states
  show                   Show task details
  diff                   Show pull request diff for a task
  approve                Approve a task (transition from approved to done)
  reject                 Reject a task (transition from review or approved to ready)
  claim                  Claim a task
  submit                 Submit a task for review
  heartbeat              Extend task lease
  next                   Find and optionally claim next claimable task
  promote                Promote a task from backlog to ready
  transition             Transition a task to a new state
  merge                  Merge a pull request and transition tasks
  wt-ensure              Ensure worktree for task (local_commit mode)
  pr-feedback            Manage PR feedback (list, ack)
  help, -h, --help       Show this help message
`, version)
}

// pprofEnabledFromEnv reports whether the pprof debug endpoints should be registered,
// reading ODONIAN_PPROF directly so this function reflects runServer's actual wiring.
// Only the exact literal "true" enables them; any other value (including case variants
// like "TRUE" or values like "1") leaves them disabled, matching /debug/pprof/ returning
// 404 as if the feature didn't exist.
func pprofEnabledFromEnv() bool {
	return os.Getenv("ODONIAN_PPROF") == "true"
}

func runServer() {
	// Print version
	fmt.Printf("odonian version %s\n", version)

	// Read configuration from environment
	authToken := os.Getenv("ODONIAN_TOKEN")
	if authToken == "" {
		log.Fatal("ODONIAN_TOKEN environment variable not set")
	}

	dbPath := os.Getenv("ODONIAN_DB")
	if dbPath == "" {
		dbPath = "odonian.db"
	}

	addr := os.Getenv("ODONIAN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	leaseTTLStr := os.Getenv("ODONIAN_LEASE_TTL")
	if leaseTTLStr == "" {
		leaseTTLStr = "5m"
	}
	leaseTTL, err := time.ParseDuration(leaseTTLStr)
	if err != nil {
		log.Fatalf("failed to parse ODONIAN_LEASE_TTL: %v", err)
	}

	maxReviewRoundsStr := os.Getenv("ODONIAN_MAX_REVIEW_ROUNDS")
	if maxReviewRoundsStr == "" {
		maxReviewRoundsStr = "5"
	}
	maxReviewRounds, err := strconv.Atoi(maxReviewRoundsStr)
	if err != nil {
		log.Fatalf("failed to parse ODONIAN_MAX_REVIEW_ROUNDS: %v", err)
	}

	// Parse escalation thresholds
	escalationThresholds := parseEscalationThresholds(os.Getenv("ODONIAN_ESCALATION_THRESHOLDS"))

	// Parse model allowlist
	allowedModels := parseAllowedModels(os.Getenv("ODONIAN_MODELS"))
	if len(allowedModels) == 0 {
		log.Fatal("ODONIAN_MODELS configuration resulted in empty allowlist")
	}

	// Parse escalation ladder
	escalationLadder := parseEscalationLadder(os.Getenv("ODONIAN_ESCALATION_LADDER"), allowedModels)

	// Parse research default model
	researchDefaultModel, err := validateResearchDefaultModel(os.Getenv("ODONIAN_RESEARCH_DEFAULT_MODEL"), allowedModels)
	if err != nil {
		log.Fatalf("invalid research default model: %v", err)
	}

	// Parse event retention configuration
	eventTerminalRetentionDaysStr := os.Getenv("ODONIAN_EVENT_TERMINAL_RETENTION_DAYS")
	if eventTerminalRetentionDaysStr == "" {
		eventTerminalRetentionDaysStr = "1"
	}
	eventTerminalRetentionDays, err := strconv.Atoi(eventTerminalRetentionDaysStr)
	if err != nil {
		log.Fatalf("failed to parse ODONIAN_EVENT_TERMINAL_RETENTION_DAYS: %v", err)
	}

	// Parse notification configuration
	notifyURL := os.Getenv("NOTIFY_URL")
	notifyToken := os.Getenv("NOTIFY_TOKEN")
	notifyIntervalStr := os.Getenv("NOTIFY_INTERVAL")
	if notifyIntervalStr == "" {
		notifyIntervalStr = "30s"
	}
	notifyInterval, err := time.ParseDuration(notifyIntervalStr)
	if err != nil {
		log.Fatalf("failed to parse NOTIFY_INTERVAL: %v", err)
	}

	notifyFailedWindowStr := os.Getenv("NOTIFY_FAILED_WINDOW")
	if notifyFailedWindowStr == "" {
		notifyFailedWindowStr = "1h"
	}
	notifyFailedWindow, err := time.ParseDuration(notifyFailedWindowStr)
	if err != nil {
		log.Fatalf("failed to parse NOTIFY_FAILED_WINDOW: %v", err)
	}

	rateLimitFloorStr := os.Getenv("PRWATCH_RATE_LIMIT_FLOOR")
	if rateLimitFloorStr == "" {
		rateLimitFloorStr = "1500"
	}
	rateLimitFloor, err := strconv.Atoi(rateLimitFloorStr)
	if err != nil {
		log.Fatalf("failed to parse PRWATCH_RATE_LIMIT_FLOOR: %v", err)
	}

	slowRequestThresholdMs := parseSlowRequestThreshold(os.Getenv("ODONIAN_SLOW_REQUEST_MS"))

	pprofEnabled := pprofEnabledFromEnv()

	// Set up logger (used by API and reconcilers)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Open the store
	s, err := store.Open(dbPath, allowedModels,
		store.WithEscalationLadder(escalationLadder),
		store.WithResearchDefaultModel(researchDefaultModel))
	if err != nil {
		log.Fatalf("failed to open store: %v", err)
	}
	defer s.Close()

	// Prune old events on startup
	ctx := context.Background()
	deletedCount, err := s.PruneEvents(ctx, eventTerminalRetentionDays)
	if err != nil {
		log.Fatalf("failed to prune events: %v", err)
	}
	if deletedCount > 0 {
		log.Printf("pruned %d old events", deletedCount)
	}

	// Create API server
	apiServer := api.New(s, authToken, leaseTTL, maxReviewRounds, escalationThresholds, pprofEnabled, slowRequestThresholdMs, logger)

	// Set up graceful shutdown with signal handling
	sigCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Set up the PR-watch reconciler unconditionally: it closes stale pull requests
	// belonging to superseded/abandoned tasks, which must happen regardless of whether
	// change notifications are configured. NOTIFY_URL only controls the separate
	// notify reconciler (and, when set, lets PR-watch publish notifications too).

	var notifier notify.Notifier = noopNotifier{}
	reconcilers := []reconcile.Reconciler{}

	if notifyURL != "" {
		if notifyToken == "" {
			log.Fatal("NOTIFY_TOKEN environment variable not set")
		}

		notifyClient := notify.NewClient(notifyURL, notifyToken, nil, logger)
		notifier = notifyClient
		reconcilers = append(reconcilers, notify.NewNotifyReconciler(s, notifyClient, notifyFailedWindow, time.Now, logger))
	}

	reconcilers = append(reconcilers, prwatch.NewPRWatchReconciler(s, notifier, forge.OwnerToken, notifyInterval, rateLimitFloor, logger))
	runner := reconcile.NewRunner(notifyInterval, logger, reconcilers...)

	go func() {
		runner.Run(sigCtx)
	}()

	// Create HTTP server
	httpServer := &http.Server{
		Addr:    addr,
		Handler: apiServer.Handler(),
	}

	// Start HTTP server in a goroutine
	log.Printf("listening on %s", addr)
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	<-sigCtx.Done()

	// Gracefully shutdown the server
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown error: %v", err)
	}
}

// splitJSONFlag pulls the global --json flag out of args so per-verb FlagSets
// never see it. Returns whether --json was present and the remaining args.
func splitJSONFlag(args []string) (bool, []string) {
	jsonOutput := false
	rest := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" {
			jsonOutput = true
			continue
		}
		rest = append(rest, arg)
	}
	return jsonOutput, rest
}

func runClient(verb string, args []string) error {
	// Strip the global --json flag so no verb's FlagSet sees it (verbs with their
	// own FlagSet, e.g. projects, would otherwise error on a trailing --json).
	jsonOutput, args := splitJSONFlag(args)

	// Read configuration from environment
	baseURL := os.Getenv("ODONIAN_URL")
	token := os.Getenv("ODONIAN_TOKEN")
	ctx := context.Background()

	// Dispatch to verb handler
	switch verb {
	case "projects":
		return executeProjects(ctx, baseURL, token, jsonOutput, args, os.Stdout)
	case "project":
		return executeProject(ctx, baseURL, token, jsonOutput, args, os.Stdout)
	case "tasks":
		return executeTasks(ctx, baseURL, token, jsonOutput, args, os.Stdout)
	case "pending":
		return executePending(ctx, baseURL, token, jsonOutput, args, os.Stdout)
	case "show":
		return executeShow(ctx, baseURL, token, jsonOutput, args, os.Stdout)
	case "transition":
		return executeTransition(ctx, baseURL, token, args)
	case "claim":
		err := executeClaim(ctx, baseURL, token, args)
		if err != nil {
			var claimErr *claimError
			if errors.As(err, &claimErr) {
				return claimErr
			}
			return err
		}
		return nil
	case "submit":
		return executeSubmit(ctx, baseURL, token, args)
	case "heartbeat":
		return executeHeartbeat(ctx, baseURL, token, args)
	case "next":
		err := executeNext(ctx, baseURL, token, jsonOutput, args)
		if err != nil {
			var claimErr *claimError
			if errors.As(err, &claimErr) {
				return claimErr
			}
			return err
		}
		return nil
	case "promote":
		return executePromote(ctx, baseURL, token, args)
	case "merge":
		return executeMerge(ctx, baseURL, token, args)
	case "diff":
		return executeDiff(ctx, baseURL, token, args, os.Stdout)
	case "approve":
		return executeApprove(ctx, baseURL, token, args)
	case "reject":
		return executeReject(ctx, baseURL, token, args)
	case "wt-ensure":
		return executeWtEnsure(ctx, baseURL, token, args)
	case "pr-feedback":
		return executePRFeedback(ctx, args, os.Stdout)
	default:
		return fmt.Errorf("unknown command %q", verb)
	}
}

func executeProjects(ctx context.Context, baseURL, token string, jsonOutput bool, args []string, out io.Writer) error {
	// Validate configuration
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	// Parse flags
	fs := flag.NewFlagSet("projects", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	modelFlag := fs.String("model", "", "filter by model")
	kindFlag := fs.String("kind", "", "filter by kind")
	claimableFlag := fs.Bool("claimable", false, "filter by claimable status")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	// Create client and list projects
	client := tuiclient.NewHTTPClient(baseURL, token)
	var opts []tuiclient.ProjectListOption
	if *modelFlag != "" {
		opts = append(opts, tuiclient.WithProjectModel(*modelFlag))
	}
	if *kindFlag != "" {
		opts = append(opts, tuiclient.WithProjectKind(*kindFlag))
	}
	if *claimableFlag {
		opts = append(opts, tuiclient.WithProjectClaimable(true))
	}

	projects, err := client.ListProjects(ctx, opts...)
	if err != nil {
		return fmt.Errorf("failed to list projects: %w", err)
	}

	// Output results
	if jsonOutput {
		output, err := json.MarshalIndent(projects, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Fprintln(out, string(output))
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tREPO\tCREATED AT")
		for _, p := range projects {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.ID, p.Name, p.Repo, p.CreatedAt)
		}
		w.Flush()
	}

	return nil
}

func executeProject(ctx context.Context, baseURL, token string, jsonOutput bool, args []string, out io.Writer) error {
	// Validate configuration
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	// Extract project ID from arguments
	projectID := ""
	for _, arg := range args {
		if arg != "--json" {
			projectID = arg
			break
		}
	}

	if projectID == "" {
		return fmt.Errorf("project id required")
	}

	// Create client and get project
	client := tuiclient.NewHTTPClient(baseURL, token)
	project, err := client.GetProject(ctx, projectID)
	if err != nil {
		return fmt.Errorf("failed to get project: %w", err)
	}

	// Output results
	if jsonOutput {
		output, err := json.MarshalIndent(project, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Fprintln(out, string(output))
	} else {
		fmt.Fprintf(out, "ID: %s\n", project.ID)
		fmt.Fprintf(out, "Name: %s\n", project.Name)
		fmt.Fprintf(out, "Repo: %s\n", project.Repo)
		fmt.Fprintf(out, "Created At: %s\n", project.CreatedAt)
	}

	return nil
}

func executeTasks(ctx context.Context, baseURL, token string, jsonOutput bool, args []string, out io.Writer) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	projectFlag := fs.String("project", "", "project ID")
	stateFlag := fs.String("state", "", "filter by state")
	modelFlag := fs.String("model", "", "filter by model")
	kindFlag := fs.String("kind", "", "filter by kind")
	claimableFlag := fs.Bool("claimable", false, "filter by claimable status")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	if *projectFlag == "" {
		return fmt.Errorf("--project flag is required")
	}

	client := tuiclient.NewHTTPClient(baseURL, token)
	var opts []tuiclient.TaskListOption
	if *stateFlag != "" {
		opts = append(opts, tuiclient.WithState(*stateFlag))
	}
	if *modelFlag != "" {
		opts = append(opts, tuiclient.WithModel(*modelFlag))
	}
	if *kindFlag != "" {
		opts = append(opts, tuiclient.WithKind(*kindFlag))
	}
	if *claimableFlag {
		opts = append(opts, tuiclient.WithClaimable(true))
	}
	tasks, err := client.ListTasks(ctx, *projectFlag, opts...)
	if err != nil {
		return fmt.Errorf("failed to list tasks: %w", err)
	}

	filtered := tasks

	// Output results
	if jsonOutput {
		output, err := json.MarshalIndent(filtered, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Fprintln(out, string(output))
	} else {
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATE\tMODEL\tKIND\tTITLE")
		for _, task := range filtered {
			id := task.ID
			if len(id) > 8 {
				id = id[:8]
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", id, task.State, task.Model, task.Kind, task.Title)
		}
		w.Flush()
	}

	return nil
}

func executeShow(ctx context.Context, baseURL, token string, jsonOutput bool, args []string, out io.Writer) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	taskID := ""
	for _, arg := range args {
		if arg != "--json" {
			taskID = arg
			break
		}
	}

	if taskID == "" {
		return fmt.Errorf("task id required")
	}

	client := tuiclient.NewHTTPClient(baseURL, token)
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}

	// Fetch events for rework tasks to display review findings
	var events []tuiclient.Event
	if task.ReviewRound > 0 {
		events, err = client.ListEvents(ctx, taskID)
		if err != nil {
			return fmt.Errorf("failed to get task events: %w", err)
		}
	}

	// Extract review findings from events
	reviewFindings := extractReviewFindings(events, task.ReviewRound)

	if jsonOutput {
		// Build output with review findings added to the task
		output := map[string]interface{}{}
		taskJSON, err := json.Marshal(task)
		if err != nil {
			return fmt.Errorf("failed to marshal task: %w", err)
		}
		if err := json.Unmarshal(taskJSON, &output); err != nil {
			return fmt.Errorf("failed to unmarshal task: %w", err)
		}
		if len(reviewFindings) > 0 {
			output["review_findings"] = reviewFindings
		}
		finalOutput, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return fmt.Errorf("failed to marshal JSON: %w", err)
		}
		fmt.Fprintln(out, string(finalOutput))
	} else {
		fmt.Fprintf(out, "ID: %s\n", task.ID)
		fmt.Fprintf(out, "State: %s\n", task.State)
		fmt.Fprintf(out, "Model: %s\n", task.Model)
		fmt.Fprintf(out, "Kind: %s\n", task.Kind)
		fmt.Fprintf(out, "Title: %s\n", task.Title)
		fmt.Fprintf(out, "Spec: %s\n", task.Spec)
		if task.ReviewRound > 0 {
			fmt.Fprintf(out, "Review Round: %d\n", task.ReviewRound)
		}
		if task.TargetTaskID != nil {
			fmt.Fprintf(out, "Target Task ID: %s\n", *task.TargetTaskID)
		}
		if len(task.Links) > 0 {
			fmt.Fprintf(out, "Links:\n")
			for _, link := range task.Links {
				fmt.Fprintf(out, "  - %s: %s\n", link.Kind, link.Value)
			}
		}
		if len(reviewFindings) > 0 {
			fmt.Fprintf(out, "Review Findings:\n")
			for _, finding := range reviewFindings {
				fmt.Fprintf(out, "  Round %d:\n", finding.Round)
				fmt.Fprintf(out, "    Verdicts:\n")
				for _, verdict := range finding.Verdicts {
					fmt.Fprintf(out, "      - %s: %s\n", verdict.Actor, verdict.Verdict)
				}
				if len(finding.Findings) > 0 {
					fmt.Fprintf(out, "    Findings:\n")
					for _, f := range finding.Findings {
						if f.Severity != "" && f.File != "" {
							filePos := fmt.Sprintf("%s:%d", f.File, f.Line)
							fmt.Fprintf(out, "      - [%s] %s [%s] %s (%s): %s\n", f.Reviewer, f.Kind, f.Severity, filePos, f.Status, f.Summary)
						} else {
							fmt.Fprintf(out, "      - [%s] %s: %s\n", f.Reviewer, f.Kind, f.Text)
						}
						if f.IsHistory {
							fmt.Fprintf(out, "        (historical)\n")
						}
					}
				}
			}
		}
	}

	return nil
}

func executeTransition(ctx context.Context, baseURL, token string, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	if len(args) < 1 {
		return fmt.Errorf("missing task id")
	}

	taskID := args[0]
	var toState string
	var note *string
	var i int

	for i = 1; i < len(args); i++ {
		switch args[i] {
		case "--to":
			i++
			if i >= len(args) {
				return fmt.Errorf("--to requires a value")
			}
			toState = args[i]
		case "--note":
			i++
			if i >= len(args) {
				return fmt.Errorf("--note requires a value")
			}
			note = &args[i]
		case "--json":
		default:
			return fmt.Errorf("unknown flag: %s", args[i])
		}
	}

	if toState == "" {
		return fmt.Errorf("--to flag is required")
	}

	client := tuiclient.NewHTTPClient(baseURL, token)
	if err := client.TransitionTask(ctx, taskID, toState, note); err != nil {
		return fmt.Errorf("failed to transition task: %w", err)
	}

	return nil
}

// parseFlagsWithPositionals parses args allowing flags and positional arguments
// to appear in ANY order. Go's flag package stops at the first positional, so
// `submit <id> --result x` would leave --result unparsed (then fail with
// "--result flag is required"). This re-runs fs.Parse over the args following
// each positional so flags are honored wherever they appear. Returns the
// positionals in order; flag values are populated on fs as usual.
func parseFlagsWithPositionals(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positionals, nil
		}
		positionals = append(positionals, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func executeClaim(ctx context.Context, baseURL, token string, args []string) error {
	// Validate configuration
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	// Parse flags
	fs := flag.NewFlagSet("claim", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agentFlag := fs.String("agent", "", "agent ID")
	modelFlag := fs.String("model", "", "model")
	positionals, err := parseFlagsWithPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	// Get positional argument (task ID)
	if len(positionals) < 1 {
		return fmt.Errorf("task ID is required")
	}
	taskID := positionals[0]

	// Resolve identity
	agentID, model, err := resolveAgentIdentity(*agentFlag, *modelFlag)
	if err != nil {
		return err
	}

	// Create client and claim task
	client := tuiclient.NewHTTPClient(baseURL, token)
	if err := client.ClaimTask(ctx, taskID, agentID, model); err != nil {
		if errors.Is(err, tuiclient.ErrAlreadyClaimed) {
			return &claimError{message: "already claimed", code: 3}
		}
		return err
	}

	return nil
}

func executeSubmit(ctx context.Context, baseURL, token string, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	resultFlag := fs.String("result", "", "result/writeup")
	verdictFlag := fs.String("verdict", "", "verdict (approve or reject)")
	prFlag := fs.String("pr", "", "PR URL")
	branchFlag := fs.String("branch", "", "branch name")
	noOpFlag := fs.Bool("no-op", false, "mark as already-satisfied (no-op)")
	messageFlag := fs.String("message", "", "commit message override (local_commit mode)")
	agentFlag := fs.String("agent", "", "agent ID")
	findingsFileFlag := fs.String("findings-file", "", "path to JSON file with structured findings")
	skipFeedbackGateFlag := fs.Bool("skip-feedback-gate", false, "bypass the mechanical PR-feedback rework gate (humans/emergencies only)")
	positionals, err := parseFlagsWithPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	if len(positionals) < 1 {
		return fmt.Errorf("task ID is required")
	}
	taskID := positionals[0]

	if *resultFlag == "" {
		return fmt.Errorf("--result flag is required")
	}

	agentID := *agentFlag
	if agentID == "" {
		agentID = os.Getenv("AGENT_ID")
	}
	if agentID == "" {
		return fmt.Errorf("agent ID is required (set --agent flag or AGENT_ID environment variable)")
	}

	if *noOpFlag && (*prFlag != "" || *branchFlag != "") {
		return fmt.Errorf("--no-op cannot be combined with --pr or --branch")
	}

	// Validate findings file early, before any HTTP requests
	var findings []byte
	if *findingsFileFlag != "" {
		fileData, err := os.ReadFile(*findingsFileFlag)
		if err != nil {
			return fmt.Errorf("failed to read findings file: %w", err)
		}

		var f interface{}
		if err := json.Unmarshal(fileData, &f); err != nil {
			return fmt.Errorf("findings file is not valid JSON: %w", err)
		}

		if _, isArray := f.([]interface{}); !isArray {
			return fmt.Errorf("findings file must be a JSON array")
		}

		findings = fileData
	}

	client := tuiclient.NewHTTPClient(baseURL, token)

	task, taskErr := client.GetTask(ctx, taskID)
	if taskErr != nil && localcommit.IsLocalCommit() {
		return fmt.Errorf("failed to get task: %w", taskErr)
	}
	if taskErr != nil {
		// Task metadata (review round, pr link) is what tells the gate whether this is a
		// rework that must be checked. A fetch failure means that is unknown, not that it
		// is safe to skip — an unavailable lookup must never be read as "this is an initial
		// submission" or "there is no outstanding feedback". Stop with a retryable error;
		// --skip-feedback-gate remains the explicit human/emergency override.
		if *skipFeedbackGateFlag {
			fmt.Fprintf(os.Stderr, "WARNING: --skip-feedback-gate set; bypassing the PR feedback gate check (task lookup failed: %v)\n", taskErr)
		} else {
			return fmt.Errorf("could not load task for pr-feedback gate check (%w); this is retryable — retry the submit once the lookup succeeds, or pass --skip-feedback-gate to bypass (humans/emergencies only)", taskErr)
		}
	} else if err := enforceFeedbackGate(ctx, task, *skipFeedbackGateFlag, os.Stderr); err != nil {
		return err
	}

	var links []tuiclient.LinkInput

	if localcommit.IsLocalCommit() {
		switch {
		case task.Kind == "review" || task.Kind == "merge":
			// review/merge-kind tasks own NO worktree, so there is nothing to commit — a review
			// task's payload IS its --verdict. `wt-ensure` is never run for one, so
			// <worktree-home>/<id> does not exist and the commit path below died on
			// `git -C <missing> add -A` (exit 128), which made submitting ANY reviewer verdict
			// impossible in local_commit mode and wedged every task in `review`. Attach no links
			// and fall through to the verdict.
			//
			// Deliberately an allowlist of worktree-LESS kinds rather than `!= "implement"`: an
			// unset/unknown kind must fall through to the commit path and fail loudly if there is
			// no worktree, never silently skip the commit and discard the worker's edits.

		case *noOpFlag:
			// A no-op submit asserts the acceptance criteria are already met on the base with an
			// unchanged tree. CommitAll errors on an empty commit, so the documented no-op path
			// (implement prompt, step 6) has to bypass the commit entirely — same as pull_request
			// mode. The reviewer verifies the claim against the base.
			links = []tuiclient.LinkInput{
				{Kind: "no_op", Value: "already-satisfied"},
			}

		default:
			message := *messageFlag
			if message == "" {
				message = task.Title
			}

			wtHome, err := localcommit.WorktreeHome()
			if err != nil {
				return fmt.Errorf("failed to get worktree home: %w", err)
			}

			wtPath := filepath.Join(wtHome, taskID)

			// Resolve tip for rework detection: use MR branch if it exists, else origin/main
			slug := localcommit.Slugify(task.Title)
			tip, err := localcommit.ResolveTip(wtPath, slug)
			if err != nil {
				return fmt.Errorf("failed to resolve tip: %w", err)
			}

			isRework := false
			cmd := exec.Command("git", "-C", wtPath, "rev-list", "--count", tip+"..HEAD")
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			if err := cmd.Run(); err == nil {
				commitCount := strings.TrimSpace(stdout.String())
				if commitCount != "0" && commitCount != "" {
					isRework = true
				}
			}

			var sha string
			if isRework {
				sha, err = localcommit.AmendAll(wtPath, message)
			} else {
				sha, err = localcommit.CommitAll(wtPath, message)
			}
			if err != nil {
				return fmt.Errorf("failed to commit: %w", err)
			}

			links = []tuiclient.LinkInput{
				{Kind: "commit", Value: sha},
			}
		}
	} else {
		if *noOpFlag {
			links = []tuiclient.LinkInput{
				{Kind: "no_op", Value: "already-satisfied"},
			}
		} else {
			if *prFlag != "" && *branchFlag != "" {
				links = []tuiclient.LinkInput{
					{Kind: "pr", Value: *prFlag},
					{Kind: "branch", Value: *branchFlag},
				}
			} else if *prFlag != "" || *branchFlag != "" {
				return fmt.Errorf("--pr and --branch must be provided together")
			}
		}
	}

	var verdict *string
	if *verdictFlag != "" {
		if *verdictFlag != "approve" && *verdictFlag != "reject" {
			return fmt.Errorf("verdict must be 'approve' or 'reject', got %q", *verdictFlag)
		}
		verdict = verdictFlag
	}

	if err := client.SubmitTaskWithFindings(ctx, taskID, agentID, *resultFlag, verdict, links, findings); err != nil {
		return fmt.Errorf("failed to submit task: %w", err)
	}

	return nil
}

func executeHeartbeat(ctx context.Context, baseURL, token string, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	fs := flag.NewFlagSet("heartbeat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	agentFlag := fs.String("agent", "", "agent ID")
	positionals, err := parseFlagsWithPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	if len(positionals) < 1 {
		return fmt.Errorf("task ID is required")
	}
	taskID := positionals[0]

	// Resolve agent ID
	agentID := *agentFlag
	if agentID == "" {
		agentID = os.Getenv("AGENT_ID")
	}
	if agentID == "" {
		return fmt.Errorf("agent ID is required (set --agent flag or AGENT_ID environment variable)")
	}

	client := tuiclient.NewHTTPClient(baseURL, token)
	if err := client.HeartbeatTask(ctx, taskID, agentID); err != nil {
		return fmt.Errorf("failed to heartbeat task: %w", err)
	}

	return nil
}

// parseSlowRequestThreshold parses ODONIAN_SLOW_REQUEST_MS.
// Returns the threshold in milliseconds. Default is 500ms;
// negative or non-integer values fall back to default with warning.
func parseSlowRequestThreshold(thresholdStr string) int {
	if thresholdStr == "" {
		return 500
	}

	threshold, err := strconv.Atoi(thresholdStr)
	if err != nil {
		log.Printf("warning: ODONIAN_SLOW_REQUEST_MS is not an integer, using default 500ms")
		return 500
	}

	if threshold < 0 {
		log.Printf("warning: ODONIAN_SLOW_REQUEST_MS is negative, using default 500ms")
		return 500
	}

	return threshold
}

func parseAllowedModels(modelsStr string) []string {
	const defaultModels = "haiku,sonnet,opus"
	if modelsStr == "" {
		modelsStr = defaultModels
	}

	seen := make(map[string]bool)
	var result []string
	for _, model := range strings.Split(modelsStr, ",") {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			seen[model] = true
			result = append(result, model)
		}
	}
	return result
}

func parseEscalationLadder(ladderStr string, allowedModels []string) []string {
	if ladderStr == "" {
		return append([]string{}, allowedModels...)
	}

	allowedModelsM := make(map[string]bool)
	for _, m := range allowedModels {
		allowedModelsM[m] = true
	}

	seen := make(map[string]bool)
	var result []string
	for _, model := range strings.Split(ladderStr, ",") {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if !allowedModelsM[model] {
			log.Fatalf("escalation ladder contains model %q not in ODONIAN_MODELS allowlist", model)
		}
		if !seen[model] {
			seen[model] = true
			result = append(result, model)
		}
	}
	return result
}

func parseEscalationThresholds(thresholdsStr string) map[string]int {
	defaults := map[string]int{"haiku": 8, "sonnet": 6, "opus": 4}
	if thresholdsStr == "" {
		return defaults
	}

	result := make(map[string]int)
	for _, pair := range strings.Split(thresholdsStr, ",") {
		pair = strings.TrimSpace(pair)
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			log.Printf("warning: invalid threshold format %q, using defaults", pair)
			return defaults
		}
		model := strings.TrimSpace(parts[0])
		thresholdStr := strings.TrimSpace(parts[1])
		threshold, err := strconv.Atoi(thresholdStr)
		if err != nil {
			log.Printf("warning: invalid threshold value %q, using defaults", thresholdStr)
			return defaults
		}
		result[model] = threshold
	}
	return result
}

func validateResearchDefaultModel(modelStr string, allowedModels []string) (string, error) {
	if modelStr == "" {
		return "", nil
	}

	modelStr = strings.TrimSpace(modelStr)
	if modelStr == "" {
		return "", nil
	}

	allowedModelsM := make(map[string]bool)
	for _, m := range allowedModels {
		allowedModelsM[m] = true
	}

	if !allowedModelsM[modelStr] {
		return "", fmt.Errorf("model %q not in ODONIAN_MODELS allowlist", modelStr)
	}

	return modelStr, nil
}

func executeNext(ctx context.Context, baseURL, token string, jsonOutput bool, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	fs := flag.NewFlagSet("next", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	projectFlag := fs.String("project", "", "project ID")
	modelFlag := fs.String("model", "", "model")
	kindFlag := fs.String("kind", "", "kind (implement or review)")
	claimFlag := fs.Bool("claim", false, "claim the task")
	agentFlag := fs.String("agent", "", "agent ID")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	if *projectFlag == "" {
		return fmt.Errorf("--project flag is required")
	}
	if *kindFlag == "" {
		return fmt.Errorf("--kind flag is required")
	}

	client := tuiclient.NewHTTPClient(baseURL, token)
	opts := []tuiclient.TaskListOption{
		tuiclient.WithKind(*kindFlag),
		tuiclient.WithClaimable(true),
	}
	if *modelFlag != "" {
		opts = append(opts, tuiclient.WithModel(*modelFlag))
	}
	tasks, err := client.ListTasks(ctx, *projectFlag, opts...)
	if err != nil {
		return fmt.Errorf("failed to list tasks: %w", err)
	}

	if len(tasks) == 0 {
		return &claimError{message: "nothing claimable", code: 2}
	}

	task := tasks[0]

	if *claimFlag {
		// Use provided model, or fall back to task's model, or environment variable
		model := *modelFlag
		if model == "" {
			model = task.Model
		}

		agentID, _, err := resolveAgentIdentity(*agentFlag, model)
		if err != nil {
			return err
		}

		if err := client.ClaimTask(ctx, task.ID, agentID, model); err != nil {
			if errors.Is(err, tuiclient.ErrAlreadyClaimed) {
				return &claimError{message: "raced, none claimed", code: 2}
			}
			return err
		}

		if jsonOutput {
			task, err := client.GetTask(ctx, task.ID)
			if err != nil {
				return fmt.Errorf("failed to get task details: %w", err)
			}
			output, err := json.MarshalIndent(task, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal JSON: %w", err)
			}
			fmt.Println(string(output))
		} else {
			fmt.Println(task.ID)
		}
	} else {
		if jsonOutput {
			output, err := json.MarshalIndent(task, "", "  ")
			if err != nil {
				return fmt.Errorf("failed to marshal JSON: %w", err)
			}
			fmt.Println(string(output))
		} else {
			fmt.Println(task.ID)
		}
	}

	return nil
}

func executePromote(ctx context.Context, baseURL, token string, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	if len(args) < 1 {
		return fmt.Errorf("task ID is required")
	}

	taskID := args[0]

	client := tuiclient.NewHTTPClient(baseURL, token)
	if err := client.PromoteTask(ctx, taskID); err != nil {
		return fmt.Errorf("failed to promote task: %w", err)
	}

	return nil
}

func executeMerge(ctx context.Context, baseURL, token string, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	if len(args) < 1 {
		return fmt.Errorf("merge task ID is required")
	}

	mergeTaskID := args[0]

	client := tuiclient.NewHTTPClient(baseURL, token)

	mergeTask, err := client.GetTask(ctx, mergeTaskID)
	if err != nil {
		return fmt.Errorf("failed to get merge task: %w", err)
	}

	// Idempotent fast path: a prior run already finalized this merge task.
	if mergeTask.State == "done" {
		return nil
	}

	if mergeTask.TargetTaskID == nil {
		return fmt.Errorf("merge task has no target_task_id")
	}

	parentTaskID := *mergeTask.TargetTaskID

	parentTask, err := client.GetTask(ctx, parentTaskID)
	if err != nil {
		return fmt.Errorf("failed to get parent task: %w", err)
	}

	if !parentTask.AgentMerge {
		return fmt.Errorf("parent task has agent_merge=false")
	}

	// `odonian merge` MUST be safely re-runnable: a merge job that merged the PR but
	// died before finalizing the merge task gets reclaimed and runs again, so a retry
	// has to converge instead of erroring. The parent task advances to 'done' as part
	// of a successful merge, so its state tells us whether the merge still needs doing:
	//   - approved: PR not yet merged -> do the squash merge, then advance the parent.
	//   - done:     a prior run already merged the PR -> skip the merge, just finalize.
	//   - other:    not a valid precondition to merge from.
	// (The old code required parent=="approved" and erred otherwise, which made a
	// partially-completed run UNrecoverable: once the parent reached 'done', every
	// retry died here before it could finalize the merge task — a permanent zombie.)
	switch parentTask.State {
	case "approved":
		var prURL string
		for _, link := range parentTask.Links {
			if link.Kind == "pr" {
				prURL = link.Value
				break
			}
		}

		if prURL == "" {
			return fmt.Errorf("parent task has no pr link")
		}

		owner, repo, prNumber, err := parsePRURL(prURL)
		if err != nil {
			return fmt.Errorf("failed to parse PR URL: %w", err)
		}

		forgeToken, err := forge.OwnerToken(owner)
		if err != nil {
			return fmt.Errorf("failed to get forge token: %w", err)
		}

		if err := forge.SquashMerge(ctx, owner, repo, prNumber, forgeToken); err != nil {
			// A real merge conflict (or an out-of-date head under require-branches-up-to-date)
			// cannot be fixed by retrying the merge — the PR must be REWORKED: synced with the
			// base branch and resolved. Detect that via the PR's mergeable_state and bounce the
			// parent task back to 'ready' so a worker reclaims and reworks it. Transient/other
			// failures fall through to the error and the merge job retries as before.
			if state, mErr := forge.PRMergeability(ctx, owner, repo, prNumber, forgeToken); mErr == nil && forge.NeedsRework(state) {
				return bounceForRework(ctx, client, parentTaskID, mergeTaskID, owner, repo, prNumber, forgeToken, state)
			}
			return fmt.Errorf("failed to squash merge PR: %w", err)
		}

		// Transition parent task to done. Refresh its state first to handle retries safely.
		parentTask, err = client.GetTask(ctx, parentTaskID)
		if err != nil {
			return fmt.Errorf("failed to refresh parent task state: %w", err)
		}
		if parentTask.State != "done" {
			if err := client.TransitionTask(ctx, parentTaskID, "done", nil); err != nil {
				return fmt.Errorf("failed to transition parent task to done: %w", err)
			}
		}

	case "done":
		// PR already merged and parent already finalized by a prior run; fall through
		// to finalize this merge task.

	default:
		// The parent isn't in a mergeable state. If it's actively being reworked — e.g. a
		// prior conflict bounce already moved it back to ready/in_progress/review — then THIS
		// merge task is stale: retire it (a fresh merge task spawns when the reworked parent
		// is re-approved) instead of erroring forever. This also closes the crash window where
		// the parent was bounced but this merge task wasn't yet retired. Any other state is
		// genuinely unexpected and surfaces as an error.
		reworkStates := map[string]bool{"backlog": true, "ready": true, "in_progress": true, "review": true}
		if reworkStates[parentTask.State] {
			note := fmt.Sprintf("Retired stale merge task: parent is %q (being reworked), not approved; a new merge task spawns on re-approval.", parentTask.State)
			if err := client.TransitionTask(ctx, mergeTaskID, "failed", &note); err != nil {
				return fmt.Errorf("failed to retire stale merge task: %w", err)
			}
			return nil
		}
		return fmt.Errorf("parent task state is %q, expected approved or done", parentTask.State)
	}

	// Transition merge task to done. Refresh its state first to handle retries safely.
	mergeTask, err = client.GetTask(ctx, mergeTaskID)
	if err != nil {
		return fmt.Errorf("failed to refresh merge task state: %w", err)
	}
	if mergeTask.State != "done" {
		if err := client.TransitionTask(ctx, mergeTaskID, "done", nil); err != nil {
			return fmt.Errorf("failed to transition merge task to done: %w", err)
		}
	}

	return nil
}

// bounceForRework handles a PR that cannot be merged because it conflicts with (or is
// behind) its base branch. The merge can't be retried into success, so the parent task
// is bounced approved->ready for a worker to rework (sync the base in + resolve), a PR
// comment records why (the worker's rework reads the latest PR comment), and this
// now-stale merge task is retired. A fresh merge task spawns when the reworked parent is
// re-approved.
//
// Order matters for crash-safety: the parent is bounced FIRST, so if the process dies
// before retiring the merge task, the merge task's lease lapses, it is reclaimed, and
// executeMerge's default case sees the parent already in a rework state and retires it
// (rather than re-attempting a doomed merge).
func bounceForRework(ctx context.Context, client *tuiclient.HTTPClient, parentTaskID, mergeTaskID, owner, repo string, prNumber int, forgeToken, mergeableState string) error {
	reason := fmt.Sprintf("merge conflict with the base branch (mergeable_state=%q)", mergeableState)
	note := fmt.Sprintf("Bounced for rework: %s. The rework must `git fetch origin && git merge origin/main`, resolve conflicts, and resubmit the PR.", reason)

	// Best-effort PR comment so the worker's rework reads this as the latest actionable
	// comment. The worker's unconditional merge-with-main resolves the conflict regardless,
	// so a failed comment (e.g. token lacks scope) does not break the bounce.
	_ = forge.PostPRComment(ctx, owner, repo, prNumber, forgeToken, "🔀 merge bot: CHANGES REQUESTED — "+note)

	// Bounce the parent (approved -> ready) so a worker reclaims and reworks it.
	if err := client.TransitionTask(ctx, parentTaskID, "ready", &note); err != nil {
		return fmt.Errorf("failed to bounce parent task for rework: %w", err)
	}

	// Retire this merge task; a new one spawns when the reworked parent is re-approved.
	retire := fmt.Sprintf("Retired after %s: parent bounced to ready for rework.", reason)
	if err := client.TransitionTask(ctx, mergeTaskID, "failed", &retire); err != nil {
		return fmt.Errorf("failed to retire merge task after conflict bounce: %w", err)
	}
	return nil
}

// parsePRURL parses a GitHub PR URL into its components.
// Expected format: https://github.com/<owner>/<repo>/pull/<number>
func parsePRURL(prURL string) (owner, repo string, number int, err error) {
	prURL = strings.TrimSuffix(prURL, "/")

	u, err := url.Parse(prURL)
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid URL: %w", err)
	}

	if u.Host != "github.com" {
		return "", "", 0, fmt.Errorf("not a github.com URL")
	}

	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")

	if len(parts) != 4 || parts[2] != "pull" {
		return "", "", 0, fmt.Errorf("not a pull request URL")
	}

	owner = parts[0]
	repo = parts[1]

	number, err = strconv.Atoi(parts[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid pull request number: %w", err)
	}

	return owner, repo, number, nil
}

func executeWtEnsure(ctx context.Context, baseURL, token string, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	// Check if in local_commit mode
	if !localcommit.IsLocalCommit() {
		return fmt.Errorf("wt-ensure requires local_commit mode (set ODONIAN_DELIVERY_MODE=local_commit)")
	}

	// Parse flags
	fs := flag.NewFlagSet("wt-ensure", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoFlag := fs.String("repo", "", "repository directory")
	positionals, err := parseFlagsWithPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	// Get task ID from positional argument
	if len(positionals) < 1 {
		return fmt.Errorf("task ID is required")
	}
	taskID := positionals[0]

	// Resolve repoDir from --repo flag or ODONIAN_REPO
	repoDir := *repoFlag
	if repoDir == "" {
		repoDir = os.Getenv("ODONIAN_REPO")
	}
	if repoDir == "" {
		return fmt.Errorf("--repo flag or ODONIAN_REPO environment variable required")
	}

	// Get task details to retrieve the title
	client := tuiclient.NewHTTPClient(baseURL, token)
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}

	// Compute slug from task title
	slug := localcommit.Slugify(task.Title)

	// Resolve tip (MR branch or origin/main)
	tip, err := localcommit.ResolveTip(repoDir, slug)
	if err != nil {
		return fmt.Errorf("failed to resolve tip: %w", err)
	}

	// Add worktree
	wtPath, err := localcommit.AddWorktree(repoDir, taskID, tip)
	if err != nil {
		return fmt.Errorf("failed to add worktree: %w", err)
	}

	// Print worktree path to stdout
	fmt.Println(wtPath)

	return nil
}

// ReviewVerdictInfo holds a single reviewer's verdict
type ReviewVerdictInfo struct {
	Actor   string `json:"actor"`
	Verdict string `json:"verdict"`
}

// ReviewFindingInfo holds a single review finding (prose or structured)
type ReviewFindingInfo struct {
	Reviewer      string `json:"reviewer"`
	Kind          string `json:"kind"`
	Text          string `json:"text,omitempty"`
	IsHistory     bool   `json:"is_history,omitempty"`
	Severity      string `json:"severity,omitempty"`
	File          string `json:"file,omitempty"`
	Line          int    `json:"line,omitempty"`
	Summary       string `json:"summary,omitempty"`
	Status        string `json:"status,omitempty"`
	InChangedText bool   `json:"in_changed_text,omitempty"`
}

// ReviewRoundFindings holds all findings for a single review round
type ReviewRoundFindings struct {
	Round    int                 `json:"round"`
	Verdicts []ReviewVerdictInfo `json:"verdicts,omitempty"`
	Findings []ReviewFindingInfo `json:"findings,omitempty"`
}

// extractReviewFindings extracts review context from task events, grouped by review round.
// It returns findings from the latest completed round, and marks earlier findings as historical.
func extractReviewFindings(events []tuiclient.Event, currentReviewRound int) []ReviewRoundFindings {
	if currentReviewRound <= 0 {
		return nil
	}

	// Track round boundaries via spawn_review events, which carry "Round N with models: ..."
	roundMap := make(map[int]*ReviewRoundFindings)

	// Initialize all rounds up to currentReviewRound
	for i := 1; i <= currentReviewRound; i++ {
		roundMap[i] = &ReviewRoundFindings{Round: i}
	}

	// Single pass: assign events to rounds and extract verdicts/findings
	activeRound := 1
	for _, event := range events {
		switch event.Kind {
		case "spawn_review":
			// Update active round based on spawn_review events
			if event.Note != nil {
				roundNum := parseRoundFromSpawnReview(*event.Note)
				if roundNum > 0 {
					activeRound = roundNum
				}
			}
		case "review":
			// Review events belong to the current active round
			if activeRound <= currentReviewRound {
				if event.Verdict != nil {
					roundMap[activeRound].Verdicts = append(roundMap[activeRound].Verdicts, ReviewVerdictInfo{
						Actor:   event.Actor,
						Verdict: *event.Verdict,
					})
				}
				// Extract structured findings if present
				if event.Findings != nil && len(*event.Findings) > 0 {
					kind := "approval"
					if event.Verdict != nil && *event.Verdict == "reject" {
						kind = "rejection"
					}
					for _, f := range *event.Findings {
						roundMap[activeRound].Findings = append(roundMap[activeRound].Findings, ReviewFindingInfo{
							Reviewer:      event.Actor,
							Kind:          kind,
							Severity:      f.Severity,
							File:          f.File,
							Line:          f.Line,
							Summary:       f.Summary,
							Status:        f.Status,
							InChangedText: f.InChangedText,
						})
					}
				}
				// Extract findings from the note field of review events. Only reject
				// verdicts are labeled as rejection findings the worker must address;
				// approve verdicts that also carry a note are labeled distinctly so
				// they are not presented as unresolved work.
				if event.Note != nil {
					kind := "approval"
					if event.Verdict != nil && *event.Verdict == "reject" {
						kind = "rejection"
					}
					roundMap[activeRound].Findings = append(roundMap[activeRound].Findings, ReviewFindingInfo{
						Reviewer: event.Actor,
						Kind:     kind,
						Text:     *event.Note,
					})
				}
			}
		}
	}

	// Mark older rounds as historical
	var results []ReviewRoundFindings
	for i := 1; i <= currentReviewRound; i++ {
		if rf := roundMap[i]; len(rf.Verdicts) > 0 || len(rf.Findings) > 0 {
			if i < currentReviewRound {
				for j := range rf.Findings {
					rf.Findings[j].IsHistory = true
				}
			}
			results = append(results, *rf)
		}
	}

	return results
}

// parseRoundFromSpawnReview extracts the round number from a spawn_review event note.
// The format is "Round N with models: [...]"
func parseRoundFromSpawnReview(note string) int {
	parts := strings.Fields(note)
	if len(parts) >= 2 && parts[0] == "Round" {
		if round, err := strconv.Atoi(parts[1]); err == nil {
			return round
		}
	}
	return -1
}

func resolveAgentIdentity(agentFlag, modelFlag string) (agentID, model string, err error) {
	// Resolve agent ID: prefer flag, fallback to AGENT_ID env
	if agentFlag != "" {
		agentID = agentFlag
	} else {
		agentID = os.Getenv("AGENT_ID")
	}

	// Resolve model: prefer flag, fallback to AGENT_MODEL env
	if modelFlag != "" {
		model = modelFlag
	} else {
		model = os.Getenv("AGENT_MODEL")
	}

	// Validate required fields
	if agentID == "" {
		return "", "", fmt.Errorf("agent ID is required (set --agent flag or AGENT_ID environment variable)")
	}
	if model == "" {
		return "", "", fmt.Errorf("model is required (set --model flag or AGENT_MODEL environment variable)")
	}

	return agentID, model, nil
}
