package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/boldfield/odonian/internal/localcommit"
	"github.com/boldfield/odonian/internal/tuiclient"
)

func executeApprove(ctx context.Context, baseURL, token string, args []string) error {
	if baseURL == "" {
		return fmt.Errorf("ODONIAN_URL environment variable not set")
	}
	if token == "" {
		return fmt.Errorf("ODONIAN_TOKEN environment variable not set")
	}

	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoFlag := fs.String("repo", "", "repository directory")
	freezeOnlyFlag := fs.Bool("freeze-only", false, "for a task already done: finish landing its work and cleaning up, skip the transition")
	cancelLandingFlag := fs.Bool("cancel-landing", false, "drop an interrupted approve's landing reservation, if its work never reached the branch")
	gateTimeoutFlag := fs.Duration("gate-timeout", localcommit.DefaultGateTimeout, "limit for the merge gate (make check + make test) when landing needs a merge")
	allowHostGateFlag := fs.Bool("allow-host-gate", false, "allow the merge gate to run the tasks' code outside a sandbox")
	positionals, err := parseFlagsWithPositionals(fs, args)
	if err != nil {
		return fmt.Errorf("failed to parse flags: %w", err)
	}

	if len(positionals) < 1 {
		return fmt.Errorf("task id required")
	}
	if *gateTimeoutFlag <= 0 {
		return fmt.Errorf("--gate-timeout must be positive, got %s", *gateTimeoutFlag)
	}

	taskID := positionals[0]

	client := tuiclient.NewHTTPClient(baseURL, token)
	task, err := client.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("failed to get task: %w", err)
	}

	if !localcommit.IsLocalCommit() {
		if *freezeOnlyFlag || *cancelLandingFlag {
			return nil
		}
		if task.State != "approved" {
			return fmt.Errorf("task is in %q state, expected approved", task.State)
		}
		if err := client.TransitionTask(ctx, taskID, "done", nil); err != nil {
			return fmt.Errorf("failed to transition task to done: %w", err)
		}
		return nil
	}

	repoDir := *repoFlag
	if repoDir == "" {
		repoDir = os.Getenv("ODONIAN_REPO")
	}
	if repoDir == "" {
		return fmt.Errorf("--repo flag or ODONIAN_REPO environment variable required")
	}
	slug := localcommit.BranchSlug(task.Branch, task.Title)
	integrateOptions := localcommit.IntegrateOptions{
		Progress:      os.Stderr,
		GateTimeout:   *gateTimeoutFlag,
		AllowHostGate: *allowHostGateFlag,
	}

	if *cancelLandingFlag {
		return cancelLanding(ctx, client, repoDir, slug, taskID, task, integrateOptions)
	}
	if *freezeOnlyFlag && task.State != "done" {
		return fmt.Errorf("--freeze-only is for a task that is already done; this one is %q (use plain approve for an approved task)", task.State)
	}
	switch task.State {
	case "approved":
		return approveLocalCommit(ctx, client, repoDir, slug, taskID, task, integrateOptions)
	case "done":
		return landDoneTask(repoDir, slug, taskID, task, integrateOptions)
	default:
		return fmt.Errorf("task is in %q state, expected approved", task.State)
	}
}

// approveLocalCommit lands an approved task's reviewed work on its MR branch, holding the branch
// lock throughout so approves on one branch run one at a time:
//
//  1. Prepare the landing (a fast-forward, or a merge that passed the gate) without moving the
//     branch. A reject during the gate needs no undo: nothing has been published.
//  2. Reserve the task for landing, atomically conditional on it still being approved in the
//     review round read before Prepare read the wip commit. If it was reworked and re-approved
//     meanwhile, the round has moved and this refuses, so an obsolete version never lands.
//  3. Publish: move the branch. The reservation keeps the task from being rejected from here on.
//  4. Mark the task done. Only now are its dependents claimable, and the work is already on the
//     branch they will start from.
//  5. Clean up the worktree and wip branch, but only if wip still holds the landed commit.
//
// An approve interrupted anywhere after step 2 leaves a reserved task; re-running approve
// resumes it (each step is idempotent), and --cancel-landing drops the reservation if the work
// never reached the branch.
func approveLocalCommit(ctx context.Context, client tuiclient.Client, repoDir, slug, taskID string, task tuiclient.TaskDetail, options localcommit.IntegrateOptions) error {
	lock, err := localcommit.LockBranch(repoDir, slug, options.LockDuration())
	if err != nil {
		return fmt.Errorf("not approved, task left in approved: %w", err)
	}
	defer lock.Release()

	reviewRound := task.ReviewRound
	// Land only the commit this round reviewed. Checked here so a mismatch fails before a
	// possibly long merge gate; the board re-checks it atomically when the task is reserved.
	reviewed, err := reviewedCommit(task)
	if err != nil {
		return fmt.Errorf("not approved, task left in approved: %w", err)
	}
	if err := requireWipAt(repoDir, taskID, reviewed, reviewRound); err != nil {
		return fmt.Errorf("not approved, task left in approved: %w", err)
	}
	integration, err := localcommit.Prepare(repoDir, slug, taskID, options)
	if err != nil {
		return fmt.Errorf("not approved, task left in approved: %w", err)
	}
	if integration.Source != reviewed {
		return fmt.Errorf("not approved, task left in approved: %s moved to %s while the landing was prepared; round %d reviewed %s", localcommit.WIPBranch(taskID), integration.Source, reviewRound, reviewed)
	}
	if task.LandingRound != nil {
		if *task.LandingRound != reviewRound || task.LandingCommit == nil || *task.LandingCommit != integration.Source {
			return fmt.Errorf("the task is reserved for landing round %d (commit %s), but it is in round %d and %s is at %s; this does not match an interrupted approve, so inspect it by hand",
				*task.LandingRound, stringOrNone(task.LandingCommit), reviewRound, localcommit.WIPBranch(taskID), integration.Source)
		}
		fmt.Fprintf(os.Stderr, "resuming an interrupted approve of round %d\n", reviewRound)
	}

	if err := reserveForLanding(ctx, client, taskID, reviewRound, integration, lock.Token()); err != nil {
		return err
	}

	if err := localcommit.Publish(repoDir, integration, lock); err != nil {
		if errors.Is(err, localcommit.ErrLockLost) {
			// Another approve took the lock over and may already have resumed this landing
			// (re-reserving it and publishing), so the reservation is no longer ours to give back.
			return fmt.Errorf("not landed by this approve: %w; the landing reservation was left as it is, so check the task and re-run `odonian approve %s` if it is not done", err, taskID)
		}
		// Publish moves the branch all-or-nothing and this approve still held the lock, so nothing
		// landed: give the reservation back, but only if this attempt still owns it.
		if cancelErr := client.CancelLanding(ctx, taskID, lock.Token()); cancelErr != nil {
			return fmt.Errorf("not approved, nothing was put on %s: %w; the task's landing reservation could not be dropped (%v), so fix the cause and re-run `odonian approve %s`, or drop it with `odonian approve %s --cancel-landing`",
				integration.Branch, err, cancelErr, taskID, taskID)
		}
		return fmt.Errorf("not approved, task left in approved, nothing was put on %s: %w", integration.Branch, err)
	}

	note := integration.Note()
	if err := client.CompleteLanding(ctx, taskID, lock.Token(), &note); err != nil {
		// The transition may have been applied with only its response lost: ask before reporting.
		current, getErr := client.GetTask(ctx, taskID)
		if getErr != nil || current.State != "done" {
			return fmt.Errorf("the task's work is on %s, but the task could not be marked done (%v); it stays reserved for landing, so it cannot be rejected and its dependents stay blocked. Re-run `odonian approve %s` to finish",
				integration.Branch, err, taskID)
		}
		fmt.Fprintln(os.Stderr, "the done transition's response was lost, but the task is done")
	}
	fmt.Fprintln(os.Stderr, note)

	if err := localcommit.Cleanup(repoDir, taskID, integration.Source); err != nil {
		return fmt.Errorf("task is done and its work is on %s, but cleanup failed (re-run `odonian approve %s`): %w", integration.Branch, taskID, err)
	}
	return nil
}

// reserveForLanding takes the task's landing reservation for reviewRound and the prepared commit,
// as approve attempt `attempt`. A refusal from the board (wrong state, stale round) ends the
// approve with nothing published. With no answer at all, it asks the board whether the
// reservation took and is this attempt's.
func reserveForLanding(ctx context.Context, client tuiclient.Client, taskID string, reviewRound int, integration localcommit.Integration, attempt string) error {
	err := client.BeginLanding(ctx, taskID, reviewRound, integration.Source, attempt)
	if err == nil {
		return nil
	}
	var apiErr *tuiclient.APIError
	if errors.As(err, &apiErr) {
		return fmt.Errorf("not approved, nothing was put on %s: %w", integration.Branch, err)
	}
	current, getErr := client.GetTask(ctx, taskID)
	if getErr != nil {
		return fmt.Errorf("could not tell whether the task was reserved for landing (%v; checking it: %v); nothing was put on %s. Re-run `odonian approve %s`",
			err, getErr, integration.Branch, taskID)
	}
	if current.State == "approved" && current.LandingRound != nil && *current.LandingRound == reviewRound &&
		current.LandingCommit != nil && *current.LandingCommit == integration.Source &&
		current.LandingAttempt != nil && *current.LandingAttempt == attempt {
		return nil
	}
	return fmt.Errorf("not approved, nothing was put on %s: reserving the task for landing failed: %w", integration.Branch, err)
}

// cancelLanding is `approve --cancel-landing`: drop the landing reservation an interrupted approve
// left, which is only safe while its work is not on the branch. It takes the branch lock first,
// so it cannot race an approve that is publishing.
func cancelLanding(ctx context.Context, client tuiclient.Client, repoDir, slug, taskID string, task tuiclient.TaskDetail, options localcommit.IntegrateOptions) error {
	if task.LandingRound == nil || task.LandingCommit == nil {
		return fmt.Errorf("task holds no landing reservation (it is %q)", task.State)
	}
	lock, err := localcommit.LockBranch(repoDir, slug, options.LockDuration())
	if err != nil {
		return err
	}
	defer lock.Release()

	if localcommit.OnBranch(repoDir, slug, *task.LandingCommit) {
		return fmt.Errorf("the reserved work (%s) is already on %s, so the reservation cannot be dropped; finish with `odonian approve %s`",
			*task.LandingCommit, localcommit.MRBranch(slug), taskID)
	}
	// Cancel only the attempt checked above: an approve that resumes the landing re-reserves it
	// (taking it over) before it can publish, so if one does so meanwhile, this cancel is refused
	// instead of leaving its published work on the branch of a rejectable task.
	if err := client.CancelLanding(ctx, taskID, stringOrNone(task.LandingAttempt)); err != nil {
		return fmt.Errorf("failed to drop the landing reservation: %w", err)
	}
	fmt.Fprintf(os.Stderr, "dropped the landing reservation; the task is approved and its work is not on %s\n", localcommit.MRBranch(slug))
	return nil
}

// landDoneTask finishes a task that is done but whose wip branch is still there: approve's
// cleanup failed after the work landed, or the task was marked done some other way. It lands only
// what the final review round approved: that round's commit (a no-op if already on the branch),
// and nothing at all if that round was a no-op, since the wip branch then holds only work that
// was never approved (e.g. an earlier, rejected round's commit).
func landDoneTask(repoDir, slug, taskID string, task tuiclient.TaskDetail, options localcommit.IntegrateOptions) error {
	wipBranch := localcommit.WIPBranch(taskID)
	if exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+wipBranch).Run() != nil {
		fmt.Fprintf(os.Stderr, "task is done and %s is already gone; nothing left to land\n", wipBranch)
		return nil
	}
	if currentRoundIsNoOp(task) {
		fmt.Fprintf(os.Stderr, "task is done as a no-op (review round %d), so nothing is landed; %s holds work that was never approved and was left in place: delete it by hand if it is not needed\n", task.ReviewRound, wipBranch)
		return nil
	}
	reviewed, err := reviewedCommit(task)
	if err != nil {
		return fmt.Errorf("task is done, but which commit was approved is unknown, so nothing was landed: %w", err)
	}

	lock, err := localcommit.LockBranch(repoDir, slug, options.LockDuration())
	if err != nil {
		return err
	}
	defer lock.Release()
	if localcommit.OnBranch(repoDir, slug, reviewed) {
		fmt.Fprintf(os.Stderr, "task is done and its approved commit %s is already on %s; cleaning up\n", reviewed, localcommit.MRBranch(slug))
		return localcommit.Cleanup(repoDir, taskID, reviewed)
	}
	if err := requireWipAt(repoDir, taskID, reviewed, task.ReviewRound); err != nil {
		return fmt.Errorf("task is done, but nothing was landed: %w", err)
	}

	fmt.Fprintf(os.Stderr, "task is done but its approved commit %s is not on %s yet; landing it\n", reviewed, localcommit.MRBranch(slug))
	integration, err := localcommit.Prepare(repoDir, slug, taskID, options)
	if err != nil {
		return fmt.Errorf("task is done, but its work could not be put on %s: %w", localcommit.MRBranch(slug), err)
	}
	if integration.Source != reviewed {
		return fmt.Errorf("task is done, but nothing was landed: %s moved to %s while the landing was prepared; round %d approved %s", wipBranch, integration.Source, task.ReviewRound, reviewed)
	}
	if err := localcommit.Publish(repoDir, integration, lock); err != nil {
		return fmt.Errorf("task is done, but its work could not be put on %s: %w", localcommit.MRBranch(slug), err)
	}
	fmt.Fprintln(os.Stderr, integration.Note())
	return localcommit.Cleanup(repoDir, taskID, integration.Source)
}

// requireWipAt returns an error unless wip/<iid> points at reviewed, the commit reviewRound
// reviewed: a wip branch changed after it was submitted holds unreviewed work.
func requireWipAt(repoDir, taskID, reviewed string, reviewRound int) error {
	wipBranch := localcommit.WIPBranch(taskID)
	wipTip, err := exec.Command("git", "-C", repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+wipBranch).Output()
	if err != nil {
		return fmt.Errorf("%s not found", wipBranch)
	}
	if current := strings.TrimSpace(string(wipTip)); current != reviewed {
		return fmt.Errorf("%s is at %s, but review round %d reviewed %s; the wip branch changed after it was submitted", wipBranch, current, reviewRound, reviewed)
	}
	return nil
}

func stringOrNone(value *string) string {
	if value == nil {
		return "none"
	}
	return *value
}
