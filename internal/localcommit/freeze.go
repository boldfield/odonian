package localcommit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// IntegrateOptions controls how a task's work is landed.
type IntegrateOptions struct {
	// Progress receives status lines and the merge gate's streamed output.
	Progress io.Writer
	// GateTimeout bounds the merge gate; zero means DefaultGateTimeout.
	GateTimeout time.Duration
	// AllowHostGate lets the merge gate run outside a sandbox (see InSandbox).
	AllowHostGate bool
}

func (options IntegrateOptions) withDefaults() IntegrateOptions {
	if options.Progress == nil {
		options.Progress = io.Discard
	}
	if options.GateTimeout <= 0 {
		options.GateTimeout = DefaultGateTimeout
	}
	return options
}

// LockDuration is how long an approve using these options holds its branch lock (LockBranch):
// the merge gate's limit plus a margin for the work around it.
func (options IntegrateOptions) LockDuration() time.Duration {
	return options.withDefaults().GateTimeout + lockMargin
}

// Integration is how a task's work lands on its MR branch: built by Prepare, applied by Publish.
type Integration struct {
	// Kind is "created" (new MR branch), "fast-forward", "merge", or "already" (the work is
	// already on the branch; nothing to publish).
	Kind string
	// Branch is the MR branch, wi/<slug>.
	Branch string
	// Source is the task's commit being landed: wip/<iid>'s tip when Prepare read it. Everything
	// after Prepare works from this SHA, never from the wip branch, which can move under rework.
	Source string
	// Commit is the branch tip once published (for a merge, the gated merge commit).
	Commit string
	// Previous is the branch tip Prepare saw ("" if the branch did not exist). Publish moves the
	// branch only if it still points here.
	Previous string
}

// Note is a one-line description suitable for the task's done transition.
func (integration Integration) Note() string {
	switch integration.Kind {
	case "created":
		return fmt.Sprintf("local_commit: created %s at %s", integration.Branch, integration.Commit)
	case "fast-forward":
		return fmt.Sprintf("local_commit: fast-forwarded %s to %s", integration.Branch, integration.Commit)
	case "merge":
		return fmt.Sprintf("local_commit: merged into %s as %s (make check and make test passed on the merge)", integration.Branch, integration.Commit)
	default:
		return fmt.Sprintf("local_commit: already on %s at %s", integration.Branch, integration.Commit)
	}
}

// Freeze lands task iid's approved work on its MR branch wi/<slug> (Integrate), then removes the
// task's worktree and wip/<iid> branch, with default options (progress to stderr, no host gate).
// approve does not use it: it marks the task done between preparing and publishing.
func Freeze(repoDir, slug, iid string) error {
	integration, err := Integrate(repoDir, slug, iid, IntegrateOptions{Progress: os.Stderr})
	if err != nil {
		return err
	}
	return Cleanup(repoDir, iid, integration.Source)
}

// Integrate prepares and publishes task iid's work on wi/<slug> under the branch lock, for
// callers with no board step between the two.
func Integrate(repoDir, slug, iid string, options IntegrateOptions) (Integration, error) {
	lock, err := LockBranch(repoDir, slug, options.LockDuration())
	if err != nil {
		return Integration{}, err
	}
	defer lock.Release()

	integration, err := Prepare(repoDir, slug, iid, options)
	if err != nil {
		return Integration{}, err
	}
	return integration, Publish(repoDir, integration, lock)
}

// Prepare works out how wip/<iid> lands on wi/<slug> and builds the commit to publish, without
// moving wi/<slug>. Every task works on its own wip/<iid> branch, started from wi/<slug>'s tip at
// the time (or origin/main before the branch exists), so tasks sharing a branch can run
// concurrently:
//   - wi/<slug> absent: create it at wip/<iid>.
//   - wip/<iid> already contained in wi/<slug>: nothing to do (a re-run after a later step failed).
//   - wi/<slug> is an ancestor of wip/<iid>: fast-forward. This is exactly the commit that was
//     reviewed and tested, so no further gate runs.
//   - otherwise wi/<slug> moved since the task started: merge wip/<iid> into it in a throwaway
//     worktree, then run the repository's `make check` and `make test` on the merged tree,
//     because the combination was never built or reviewed together. A conflict, a failing gate,
//     or a gate that cannot run refuses the landing.
//
// The caller must hold the branch lock (LockBranch) from before Prepare until after Publish, so
// no other approve moves wi/<slug> in between.
func Prepare(repoDir, slug, iid string, options IntegrateOptions) (Integration, error) {
	options = options.withDefaults()
	mrBranch := MRBranch(slug)
	wipBranch := WIPBranch(iid)

	if err := refuseIfCheckedOut(repoDir, mrBranch); err != nil {
		return Integration{}, err
	}

	wipTip, err := git(repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+wipBranch)
	if err != nil {
		return Integration{}, fmt.Errorf("WIP branch %s not found", wipBranch)
	}
	mrTip, err := git(repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+mrBranch)
	if err != nil {
		return Integration{Kind: "created", Branch: mrBranch, Source: wipTip, Commit: wipTip}, nil
	}
	if isAncestor(repoDir, wipTip, mrTip) {
		return Integration{Kind: "already", Branch: mrBranch, Source: wipTip, Commit: mrTip, Previous: mrTip}, nil
	}
	if isAncestor(repoDir, mrTip, wipTip) {
		return Integration{Kind: "fast-forward", Branch: mrBranch, Source: wipTip, Commit: wipTip, Previous: mrTip}, nil
	}

	if !options.AllowHostGate && !InSandbox() {
		return Integration{}, fmt.Errorf("%s has moved since %s started, so landing it needs a merge, and the merge gate runs the tasks' code (make check, make test); refusing to run that outside a sandbox. Approve from inside the sandbox, or pass --allow-host-gate to run it here", mrBranch, wipBranch)
	}
	mergeCommit, err := mergeAndVerify(repoDir, iid, mrBranch, mrTip, wipBranch, wipTip, options)
	if err != nil {
		return Integration{}, err
	}
	return Integration{Kind: "merge", Branch: mrBranch, Source: wipTip, Commit: mergeCommit, Previous: mrTip}, nil
}

// Publish moves the MR branch to integration.Commit ("already" needs nothing). It re-checks right
// before the move that no worktree has the branch checked out (update-ref would silently leave
// that checkout's index and files behind). The move is one atomic ref transaction that also
// verifies lock is still held by this approve, and moves the branch only if it still points at
// integration.Previous ("" = must not exist yet): an approve whose lock expired and was taken over
// (e.g. by --cancel-landing, after which the task may have been rejected) publishes nothing, and
// a branch changed by hand since Prepare is never overwritten.
func Publish(repoDir string, integration Integration, lock *BranchLock) error {
	if integration.Kind == "already" {
		return nil
	}
	if err := refuseIfCheckedOut(repoDir, integration.Branch); err != nil {
		return err
	}
	expectedPrevious := integration.Previous
	if expectedPrevious == "" {
		expectedPrevious = strings.Repeat("0", len(integration.Commit)) // zero id: must not exist yet
	}
	// update-ref --stdin applies all its commands in one transaction, all or nothing.
	transaction := fmt.Sprintf("verify %s %s\nupdate refs/heads/%s %s %s\n", lock.ref, lock.owner, integration.Branch, integration.Commit, expectedPrevious)
	if _, err := gitWithInput(repoDir, []byte(transaction), "update-ref", "--stdin"); err != nil {
		if heldBy, _ := git(repoDir, "rev-parse", "--verify", "--quiet", lock.ref); heldBy != lock.owner {
			return fmt.Errorf("this approve's lock on %s expired and was taken over, so it published nothing (another approve may be landing the task now): %w", integration.Branch, ErrLockLost)
		}
		return fmt.Errorf("failed to create/advance MR branch %s (it changed since the landing was prepared): %w", integration.Branch, err)
	}
	return nil
}

// Cleanup removes task iid's worktree (tolerating an already-removed one) and deletes its
// wip/<iid> branch, once landedCommit (the Integration's Source) is on the MR branch. If wip/<iid>
// has moved past landedCommit, it holds newer work (a rework), so nothing is removed.
func Cleanup(repoDir, iid, landedCommit string) error {
	wipBranch := WIPBranch(iid)
	if wipTip, err := git(repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+wipBranch); err == nil && wipTip != landedCommit {
		return fmt.Errorf("%s has moved to %s since %s was landed; it holds newer work, so it and its worktree were left in place", wipBranch, wipTip, landedCommit)
	}
	home, err := WorktreeHome()
	if err != nil {
		return err
	}
	worktreePath := filepath.Join(home, iid)
	if err := removeTaskWorktree(repoDir, worktreePath); err != nil {
		return fmt.Errorf("%w; %s was kept so that worktree stays usable: fix the cause and re-run approve", err, wipBranch)
	}
	// update-ref (below) would delete a branch some other worktree has checked out, leaving that
	// worktree with an unresolvable HEAD.
	checkedOutAt, err := worktreeWithBranch(repoDir, "refs/heads/"+wipBranch)
	if err != nil {
		return fmt.Errorf("failed to list worktrees: %w", err)
	}
	if checkedOutAt != "" {
		return fmt.Errorf("%s is checked out at %s, so it was kept: remove that worktree, then re-run approve", wipBranch, checkedOutAt)
	}

	// Not `git branch -d`: it checks reachability from the CHECKED-OUT branch (HEAD — typically
	// `main`), where the wip commit is NOT reachable, so it refuses with "not fully merged". The
	// wip commit is already on the MR branch, so deleting the wip *label* loses nothing.
	// update-ref -d with the expected value deletes it only if it still points at landedCommit.
	if _, err := git(repoDir, "update-ref", "-d", "refs/heads/"+wipBranch, landedCommit); err != nil {
		return fmt.Errorf("failed to delete WIP branch: %w", err)
	}
	return nil
}

// removeTaskWorktree removes the task's worktree. --force: the work is already on the MR branch, so
// leftover untracked files (build output) must not keep it alive. It is fine for the worktree to be
// gone already, or for the path to be a plain directory that is not a worktree (no .git); any
// other failure (a locked worktree, a permission error) is returned, so the caller keeps the wip
// branch that worktree has checked out instead of leaving it with an unresolvable HEAD.
func removeTaskWorktree(repoDir, worktreePath string) error {
	_, removeErr := git(repoDir, "worktree", "remove", "--force", worktreePath)
	_, _ = git(repoDir, "worktree", "prune") // forget registrations whose directory is gone
	if removeErr == nil {
		return nil
	}
	if _, err := os.Lstat(filepath.Join(worktreePath, ".git")); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("could not remove worktree %s: %w", worktreePath, removeErr)
}

func refuseIfCheckedOut(repoDir, mrBranch string) error {
	checkedOutPath, err := worktreeWithBranch(repoDir, "refs/heads/"+mrBranch)
	if err != nil {
		return fmt.Errorf("failed to list worktrees: %w", err)
	}
	if checkedOutPath != "" {
		return fmt.Errorf("MR branch %s is checked out at %s; cd out or run 'git checkout --detach' there, then re-approve", mrBranch, checkedOutPath)
	}
	return nil
}

// mergeWorktreePath is the fixed scratch worktree for merging task iid. It is fixed (not a fresh
// temp dir) so a run killed mid-gate leaves something the next run for the task removes.
func mergeWorktreePath(iid string) (string, error) {
	home, err := WorktreeHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".merge-"+iid), nil
}

func removeMergeWorktree(repoDir, mergeDir string) {
	_, _ = git(repoDir, "worktree", "remove", "--force", mergeDir)
	_ = os.RemoveAll(mergeDir)
	_, _ = git(repoDir, "worktree", "prune")
}

// mergeAndVerify merges wipCommit (wipBranch's tip, named in messages) onto mrTip in the task's
// scratch worktree, runs the merge gate there, and returns the merge commit. The scratch worktree is removed on the way in (a previous
// run may have been killed) and on the way out; the merge commit stays in the object store, to
// be published by ref.
func mergeAndVerify(repoDir, iid, mrBranch, mrTip, wipBranch, wipCommit string, options IntegrateOptions) (string, error) {
	mergeDir, err := mergeWorktreePath(iid)
	if err != nil {
		return "", err
	}
	removeMergeWorktree(repoDir, mergeDir)
	if _, err := git(repoDir, "worktree", "add", "--detach", mergeDir, mrTip); err != nil {
		return "", fmt.Errorf("failed to create merge worktree: %w", err)
	}
	defer removeMergeWorktree(repoDir, mergeDir)

	fmt.Fprintf(options.Progress, "%s has moved since %s started; merging it in\n", mrBranch, wipBranch)
	message := fmt.Sprintf("Merge %s into %s", wipBranch, mrBranch)
	if _, mergeErr := git(mergeDir, "merge", "--no-ff", "-m", message, wipCommit); mergeErr != nil {
		conflictedFiles, _ := git(mergeDir, "diff", "--name-only", "--diff-filter=U")
		_, _ = git(mergeDir, "merge", "--abort")
		if conflictedFiles != "" {
			return "", fmt.Errorf("%s conflicts with %s in: %s (another task changed the same lines since this one started); %s is unchanged; recover with 'reject --abandon' and re-running the task from the current branch",
				wipBranch, mrBranch, strings.ReplaceAll(conflictedFiles, "\n", ", "), mrBranch)
		}
		return "", fmt.Errorf("git could not merge %s into %s, and it is not a content conflict (check git's message; e.g. a missing user.name/user.email); %s is unchanged: %w",
			wipBranch, mrBranch, mrBranch, mergeErr)
	}

	if err := runGate(mergeDir, options.GateTimeout, options.Progress); err != nil {
		if gateErr, ok := err.(*gateError); ok && gateErr.testFailure {
			return "", fmt.Errorf("%s and the work already on %s each passed review on their own, but the gate fails on their combination: either they break when combined, or the gate's environment lacks something they need; %s is unchanged: %w", wipBranch, mrBranch, mrBranch, err)
		}
		return "", fmt.Errorf("%s is unchanged: %w", mrBranch, err)
	}

	mergeCommit, err := git(mergeDir, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to resolve merge commit: %w", err)
	}
	return mergeCommit, nil
}

// worktreeWithBranch returns the path of the worktree that has branchRef checked out, or "".
func worktreeWithBranch(repoDir, branchRef string) (string, error) {
	output, err := git(repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	var currentWorktreePath string
	for _, line := range strings.Split(output, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			currentWorktreePath = path
		}
		if branch, ok := strings.CutPrefix(line, "branch "); ok && strings.TrimSpace(branch) == branchRef {
			return currentWorktreePath, nil
		}
	}
	return "", nil
}

func isAncestor(repoDir, ancestor, descendant string) bool {
	_, err := git(repoDir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

// git runs a git command in dir and returns trimmed stdout. On failure the error carries git's
// stderr so callers can surface the real reason.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if message := strings.TrimSpace(stderr.String()); message != "" {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, message)
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// OnBranch reports whether commit is already on wi/<slug> (fast-forwarded or merged in).
func OnBranch(repoDir, slug, commit string) bool {
	mrTip, err := git(repoDir, "rev-parse", "--verify", "--quiet", "refs/heads/"+MRBranch(slug))
	return err == nil && isAncestor(repoDir, commit, mrTip)
}
