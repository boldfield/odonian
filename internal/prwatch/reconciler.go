package prwatch

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/notify"
	"github.com/boldfield/odonian/internal/store"
)

type rateLimitBackoff struct {
	mu        sync.Mutex
	backoffAt map[string]time.Time
}

func newRateLimitBackoff() *rateLimitBackoff {
	return &rateLimitBackoff{
		backoffAt: make(map[string]time.Time),
	}
}

func (b *rateLimitBackoff) isBackedOff(owner string, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	notBefore, exists := b.backoffAt[owner]
	return exists && now.Before(notBefore)
}

func (b *rateLimitBackoff) setBackoff(owner string, notBefore time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.backoffAt[owner] = notBefore
}

type taskSource interface {
	ListProjects(ctx context.Context, filter store.ProjectListFilter) ([]store.Project, error)
	ListTasks(ctx context.Context, projectID string, filter store.TaskListFilter) ([]store.Task, error)
	GetTask(ctx context.Context, id string) (store.TaskWithDepsAndLinks, error)
	TransitionTask(ctx context.Context, taskID, to string, note *string) (store.Task, error)
	TombstoneLink(ctx context.Context, taskID, linkID string) error
}

type PRWatchReconciler struct {
	taskSource        taskSource
	notifier          notify.Notifier
	tokenLookup       func(owner string) (string, error)
	logger            *slog.Logger
	getPRState        func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error)
	getReviewDecision func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error)
	postPRComment     func(ctx context.Context, owner, repo string, prNumber int, token, comment string) error
	backoff           *rateLimitBackoff
}

func NewPRWatchReconciler(
	taskSource taskSource,
	notifier notify.Notifier,
	tokenLookup func(owner string) (string, error),
	logger *slog.Logger,
) *PRWatchReconciler {
	return &PRWatchReconciler{
		taskSource:        taskSource,
		notifier:          notifier,
		tokenLookup:       tokenLookup,
		logger:            logger,
		getPRState:        forge.GetPRState,
		getReviewDecision: forge.GetReviewDecision,
		postPRComment:     forge.PostPRComment,
		backoff:           newRateLimitBackoff(),
	}
}

func (r *PRWatchReconciler) Name() string {
	return "pr-watch"
}

func (r *PRWatchReconciler) Reconcile(ctx context.Context) error {
	projects, err := r.taskSource.ListProjects(ctx, store.ProjectListFilter{})
	if err != nil {
		return err
	}

	skippedPRsByOwner := make(map[string]int)
	now := time.Now()

	for _, project := range projects {
		if err := r.reconcileProject(ctx, project.ID, skippedPRsByOwner, now); err != nil {
			r.logger.Error("reconcile project error", "project_id", project.ID, "error", err)
		}
	}

	// Retrofit: close still-open pull requests belonging to tasks already in a
	// terminal state (superseded, abandoned). This drains the backlog of stale
	// pull requests that accrued before supersession started closing them
	// inline, and it's the same safety net for any inline close that failed.
	for _, project := range projects {
		if err := r.retrofitClosePRsForTerminalTasks(ctx, project.ID, skippedPRsByOwner, now); err != nil {
			r.logger.Error("retrofit close PR error", "project_id", project.ID, "error", err)
		}
	}

	// Log skipped PR checks once per owner
	for owner, count := range skippedPRsByOwner {
		r.logger.Warn("no forge token for owner", "owner", owner, "skipped_pr_checks", count)
	}

	return nil
}

func (r *PRWatchReconciler) reconcileProject(ctx context.Context, projectID string, skippedPRsByOwner map[string]int, now time.Time) error {
	approvedState := "approved"
	tasks, err := r.taskSource.ListTasks(ctx, projectID, store.TaskListFilter{
		State: &approvedState,
	})
	if err != nil {
		return err
	}

	for _, task := range tasks {
		if err := r.reconcileTask(ctx, task, skippedPRsByOwner, now); err != nil {
			r.logger.Error("reconcile task error", "task_id", task.ID, "error", err)
		}
	}

	return nil
}

func (r *PRWatchReconciler) reconcileTask(ctx context.Context, task store.Task, skippedPRsByOwner map[string]int, now time.Time) error {
	if task.AgentMerge {
		return nil
	}

	fullTask, err := r.taskSource.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}

	var prLink *store.TaskLink
	for i := range fullTask.Links {
		if fullTask.Links[i].Kind == "pr" {
			prLink = &fullTask.Links[i]
			break
		}
	}

	if prLink == nil {
		return nil
	}

	if prLink.TombstonedAt != nil {
		return nil
	}

	owner, repo, prNumber, err := parsePRURL(prLink.Value)
	if err != nil {
		r.logger.Error("parse PR URL error", "task_id", task.ID, "pr_url", prLink.Value, "error", err)
		return nil
	}

	token, err := r.tokenLookup(owner)
	if err != nil {
		r.logger.Error("token lookup error", "task_id", task.ID, "owner", owner, "error", err)
		return nil
	}

	if token == "" {
		skippedPRsByOwner[owner]++
		return nil
	}

	if r.backoff.isBackedOff(owner, now) {
		return nil
	}

	state, err := r.getPRState(ctx, owner, repo, prNumber, token)
	if err != nil {
		if isRateLimitError(err) {
			r.logger.Warn("rate limit exceeded, skipping owner", "owner", owner)
			r.backoff.setBackoff(owner, now.Add(30*time.Second))
			return nil
		}
		if is404Error(err) {
			r.logger.Warn("PR owner/repo#N gone (404); will not retry", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber)
			if err := r.taskSource.TombstoneLink(ctx, task.ID, prLink.ID); err != nil {
				r.logger.Error("tombstone link error", "task_id", task.ID, "link_id", prLink.ID, "error", err)
			}
			return nil
		}
		r.logger.Error("get PR state error", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		return nil
	}

	decision, latestReviewAt, err := r.getReviewDecision(ctx, owner, repo, prNumber, token)
	if err != nil {
		if isRateLimitError(err) {
			r.logger.Warn("rate limit exceeded, skipping owner", "owner", owner)
			r.backoff.setBackoff(owner, now.Add(30*time.Second))
			return nil
		}
		if is404Error(err) {
			r.logger.Warn("PR owner/repo#N gone (404); will not retry", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber)
			if err := r.taskSource.TombstoneLink(ctx, task.ID, prLink.ID); err != nil {
				r.logger.Error("tombstone link error", "task_id", task.ID, "link_id", prLink.ID, "error", err)
			}
			return nil
		}
		r.logger.Error("get review decision error", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		return nil
	}

	approvedAt, err := parseTime(task.UpdatedAt)
	if err != nil {
		r.logger.Error("parse approved at time error", "task_id", task.ID, "updated_at", task.UpdatedAt, "error", err)
		return nil
	}

	action := decideAction(state, decision, latestReviewAt, approvedAt)

	switch action {
	case Done:
		if err := applyMerged(ctx, r.taskSource, r.notifier, taskWithDepsLinksToTask(fullTask)); err != nil {
			r.logger.Error("apply merged error", "task_id", task.ID, "error", err)
			return nil
		}
	case Abandon:
		reason := "PR closed without merging"
		if err := applyAbandoned(ctx, r.taskSource, taskWithDepsLinksToTask(fullTask), reason); err != nil {
			r.logger.Error("apply abandoned error", "task_id", task.ID, "error", err)
			return nil
		}
	case Bounce:
		if err := applyBounce(ctx, r.taskSource, taskWithDepsLinksToTask(fullTask), owner, repo, prNumber, token); err != nil {
			r.logger.Error("apply bounce error", "task_id", task.ID, "error", err)
			return nil
		}
	case Noop:
	}

	return nil
}

// terminalPRCleanupStates lists the task states whose still-open pull requests should
// be closed by the retrofit pass: tasks superseded by a replacement, and tasks
// abandoned outright (e.g. their pull request was closed without merging).
var terminalPRCleanupStates = []string{"superseded", "abandoned"}

// retrofitClosePRsForTerminalTasks closes still-open pull requests belonging to tasks
// already in a terminal state. It's the one-shot backlog drain plus the ongoing safety
// net for any inline close (on supersession) that failed.
func (r *PRWatchReconciler) retrofitClosePRsForTerminalTasks(ctx context.Context, projectID string, skippedPRsByOwner map[string]int, now time.Time) error {
	for _, state := range terminalPRCleanupStates {
		tasks, err := r.taskSource.ListTasks(ctx, projectID, store.TaskListFilter{
			State:             &state,
			IncludeSuperseded: true,
			IncludeArchived:   true,
		})
		if err != nil {
			r.logger.Error("retrofit list tasks error", "project_id", projectID, "state", state, "error", err)
			continue
		}

		for _, task := range tasks {
			r.retrofitCloseTaskPR(ctx, task, skippedPRsByOwner, now)
		}
	}

	return nil
}

// retrofitCloseTaskPR closes the still-open pull request belonging to a task that has
// already reached a terminal state. Every failure is logged and swallowed: a stale
// pull request that can't be closed this pass is picked up again on the next one.
func (r *PRWatchReconciler) retrofitCloseTaskPR(ctx context.Context, task store.Task, skippedPRsByOwner map[string]int, now time.Time) {
	fullTask, err := r.taskSource.GetTask(ctx, task.ID)
	if err != nil {
		r.logger.Error("retrofit get task error", "task_id", task.ID, "error", err)
		return
	}

	var prLink *store.TaskLink
	for i := range fullTask.Links {
		if fullTask.Links[i].Kind == "pr" {
			prLink = &fullTask.Links[i]
			break
		}
	}

	if prLink == nil {
		return
	}

	if prLink.TombstonedAt != nil {
		return
	}

	owner, repo, prNumber, err := forge.ParsePRURL(prLink.Value)
	if err != nil {
		r.logger.Error("retrofit parse PR URL error", "task_id", task.ID, "pr_url", prLink.Value, "error", err)
		return
	}

	token, err := r.tokenLookup(owner)
	if err != nil {
		r.logger.Error("retrofit token lookup error", "task_id", task.ID, "owner", owner, "error", err)
		return
	}

	if token == "" {
		skippedPRsByOwner[owner]++
		return
	}

	if r.backoff.isBackedOff(owner, now) {
		return
	}

	state, err := r.getPRState(ctx, owner, repo, prNumber, token)
	if err != nil {
		if isRateLimitError(err) {
			r.logger.Warn("rate limit exceeded, skipping owner", "owner", owner)
			r.backoff.setBackoff(owner, now.Add(30*time.Second))
			return
		}
		if is404Error(err) {
			r.logger.Warn("PR owner/repo#N gone (404); will not retry", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber)
			if err := r.taskSource.TombstoneLink(ctx, task.ID, prLink.ID); err != nil {
				r.logger.Error("tombstone link error", "task_id", task.ID, "link_id", prLink.ID, "error", err)
			}
			return
		}
		r.logger.Error("retrofit get PR state error", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		return
	}

	// Only close open PRs; never touch a merged or already-closed one.
	if state != "open" {
		return
	}

	comment := terminalTaskComment(task, fullTask.SupersededBy)
	if err := r.postPRComment(ctx, owner, repo, prNumber, token, comment); err != nil {
		r.logger.Error("retrofit post comment error", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		// Continue to close the PR even if the comment failed.
	}

	if err := forge.ClosePR(ctx, owner, repo, prNumber, token); err != nil {
		r.logger.Error("retrofit close PR error", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
		return
	}

	branch := "mr/" + task.ID[:8]
	if err := forge.DeleteBranch(ctx, owner, repo, branch, token); err != nil {
		r.logger.Error("retrofit delete branch error", "task_id", task.ID, "owner", owner, "repo", repo, "branch", branch, "error", err)
	}

	r.logger.Info("retrofit closed PR for terminal task", "task_id", task.ID, "state", task.State, "owner", owner, "repo", repo, "pr_number", prNumber)
}

// terminalTaskComment builds the pull-request-close comment for a task in a terminal
// state, naming the replacement task when one exists so the history stays readable.
func terminalTaskComment(task store.Task, supersededBy *string) string {
	if supersededBy != nil {
		return fmt.Sprintf("Superseded by task %s. Closing this pull request; the current attempt continues there.", *supersededBy)
	}
	return fmt.Sprintf("This task is now %s; closing this stale pull request.", task.State)
}

// parsePRURL delegates to forge.ParsePRURL. Kept as a thin wrapper so existing
// call sites and tests in this package don't need to reference forge directly.
func parsePRURL(prURL string) (owner, repo string, number int, err error) {
	return forge.ParsePRURL(prURL)
}

func parseTime(timeStr string) (time.Time, error) {
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05Z",
	}

	for _, layout := range layouts {
		if t, err := time.Parse(layout, timeStr); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse time: %s", timeStr)
}

func taskWithDepsLinksToTask(t store.TaskWithDepsAndLinks) store.Task {
	return store.Task{
		ID:             t.ID,
		ProjectID:      t.ProjectID,
		DocumentID:     t.DocumentID,
		Title:          t.Title,
		Spec:           t.Spec,
		State:          t.State,
		Assignee:       t.Assignee,
		LeaseExpiresAt: t.LeaseExpiresAt,
		Result:         t.Result,
		Model:          t.Model,
		Kind:           t.Kind,
		ReviewModels:   t.ReviewModels,
		ReviewRound:    t.ReviewRound,
		TargetTaskID:   t.TargetTaskID,
		Verdict:        t.Verdict,
		AgentMerge:     t.AgentMerge,
		Held:           t.Held,
		Escalate:       t.Escalate,
		Track:          t.Track,
		CreatedAt:      t.CreatedAt,
		UpdatedAt:      t.UpdatedAt,
		ArchivedAt:     t.ArchivedAt,
		SupersededBy:   t.SupersededBy,
	}
}

func is404Error(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 404")
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return (strings.Contains(errStr, "status 403") || strings.Contains(errStr, "status 429")) &&
		strings.Contains(errStr, "API rate limit exceeded")
}
