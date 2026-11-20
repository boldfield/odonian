package checkwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/ratelimit"
	"github.com/boldfield/odonian/internal/store"
)

// CommentMarker begins every pull-request comment this reconciler posts, so the
// worker's feedback listing can recognise it.
const CommentMarker = "odonian-checks:"

type taskSource interface {
	ListProjects(ctx context.Context, filter store.ProjectListFilter) ([]store.Project, error)
	ListTasks(ctx context.Context, projectID string, filter store.TaskListFilter) ([]store.Task, error)
	GetTask(ctx context.Context, id string) (store.TaskWithDepsAndLinks, error)
	PassChecks(ctx context.Context, taskID, headSHA, summary string) (store.Task, error)
	FailChecks(ctx context.Context, taskID, headSHA, summary string, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int) (store.Task, error)
	TimeoutChecks(ctx context.Context, taskID, note string) (store.Task, error)
	TombstoneLink(ctx context.Context, taskID, linkID string) error
}

// Options carries the settings FailChecks needs for its circuit breaker.
type Options struct {
	MaxReviewRounds              int
	EscalationThresholds         map[string]int
	ResearchEscalationThresholds map[string]int
}

type Reconciler struct {
	taskSource           taskSource
	tokenLookup          func(owner string) (string, error)
	logger               *slog.Logger
	config               Config
	options              Options
	minInterval          time.Duration
	getPRChecks          func(ctx context.Context, owner, repo string, prNumber int, token string) (*forge.PRChecks, error)
	postPRComment        func(ctx context.Context, owner, repo string, prNumber int, token, comment string) error
	remainingQuotaLookup func(ctx context.Context, token string) (*forge.QuotaInfo, error)
	now                  func() time.Time
	limiter              *ratelimit.Limiter

	mu                      sync.Mutex
	lastPass                time.Time
	lastLoggedSkippedCounts map[string]int
}

// NewReconciler builds the checks reconciler. limiter is the per-owner GitHub
// gate shared with PR-watch. minInterval is the least time between passes.
func NewReconciler(
	taskSource taskSource,
	tokenLookup func(owner string) (string, error),
	limiter *ratelimit.Limiter,
	config Config,
	options Options,
	minInterval time.Duration,
	logger *slog.Logger,
) *Reconciler {
	return &Reconciler{
		taskSource:              taskSource,
		tokenLookup:             tokenLookup,
		logger:                  logger,
		config:                  config,
		options:                 options,
		minInterval:             minInterval,
		getPRChecks:             forge.GetPRChecks,
		postPRComment:           forge.PostPRComment,
		remainingQuotaLookup:    forge.GetRemainingQuota,
		now:                     time.Now,
		limiter:                 limiter,
		lastLoggedSkippedCounts: make(map[string]int),
	}
}

func (r *Reconciler) Name() string {
	return "checkwatch"
}

func (r *Reconciler) Reconcile(ctx context.Context) error {
	now := r.now()

	r.mu.Lock()
	if !r.lastPass.IsZero() && now.Sub(r.lastPass) < r.minInterval {
		r.mu.Unlock()
		return nil
	}
	r.lastPass = now
	r.mu.Unlock()

	projects, err := r.taskSource.ListProjects(ctx, store.ProjectListFilter{})
	if err != nil {
		return err
	}

	skipped := make(map[string]int)
	quotaChecked := make(map[string]bool)

	state := "checks"
	for _, project := range projects {
		tasks, err := r.taskSource.ListTasks(ctx, project.ID, store.TaskListFilter{State: &state})
		if err != nil {
			r.logger.Error("checkwatch list tasks error", "project_id", project.ID, "error", err)
			continue
		}
		for _, task := range tasks {
			if err := r.reconcileTask(ctx, task, skipped, quotaChecked, now); err != nil {
				r.logger.Error("checkwatch reconcile task error", "task_id", task.ID, "error", err)
			}
		}
	}

	for owner, count := range skipped {
		last, seen := r.lastLoggedSkippedCounts[owner]
		if !seen || last != count {
			r.logger.Warn("no forge token for owner", "owner", owner, "skipped_check_tasks", count)
			r.lastLoggedSkippedCounts[owner] = count
		}
	}
	for owner := range r.lastLoggedSkippedCounts {
		if _, still := skipped[owner]; !still {
			r.logger.Info("no longer skipping owner (token restored or no tasks in checks)", "owner", owner)
			delete(r.lastLoggedSkippedCounts, owner)
		}
	}
	return nil
}

func (r *Reconciler) reconcileTask(ctx context.Context, task store.Task, skipped map[string]int, quotaChecked map[string]bool, now time.Time) error {
	full, err := r.taskSource.GetTask(ctx, task.ID)
	if err != nil {
		return err
	}

	var prLink *store.TaskLink
	for i := range full.Links {
		if full.Links[i].Kind == "pr" && full.Links[i].TombstonedAt == nil {
			prLink = &full.Links[i]
			break
		}
	}
	if prLink == nil {
		_, err := r.taskSource.TimeoutChecks(ctx, task.ID, "task is in checks but has no pull request link")
		return err
	}

	owner, repo, prNumber, err := forge.ParsePRURL(prLink.Value)
	if err != nil {
		r.logger.Error("parse PR URL error", "task_id", task.ID, "pr_url", prLink.Value, "error", err)
		return nil
	}

	backedOff, resumed := r.limiter.Check(owner, now)
	if resumed {
		r.logger.Info("rate limit backoff resumed", "owner", owner)
	}
	if backedOff {
		return nil
	}

	token, err := r.tokenLookup(owner)
	if err != nil {
		r.logger.Error("token lookup error", "task_id", task.ID, "owner", owner, "error", err)
		return nil
	}
	if token == "" {
		skipped[owner]++
		return nil
	}

	if r.checkQuotaFloor(ctx, owner, token, quotaChecked, now) {
		return nil
	}

	checks, err := r.getPRChecks(ctx, owner, repo, prNumber, token)
	if err != nil {
		r.handleForgeError(err, task, prLink, owner, repo, prNumber, now)
		return nil
	}

	startedAt := r.startedAt(task)
	result := Decide(checks, now, startedAt, r.config)

	switch result.Decision {
	case Pass:
		_, err = r.taskSource.PassChecks(ctx, task.ID, checks.HeadSHA, result.Summary)
	case Fail:
		comment := failComment(checks)
		if perr := r.postPRComment(ctx, owner, repo, prNumber, token, comment); perr != nil {
			if rle, ok := asRateLimitError(perr); ok {
				r.enterBackoff(owner, rle, now)
				return nil
			}
			r.logger.Error("post checks comment error", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", perr)
			return nil
		}
		_, err = r.taskSource.FailChecks(ctx, task.ID, checks.HeadSHA, result.Summary, r.options.MaxReviewRounds, r.options.EscalationThresholds, r.options.ResearchEscalationThresholds)
	case Timeout:
		_, err = r.taskSource.TimeoutChecks(ctx, task.ID, result.Summary)
	case PRClosed:
		_, err = r.taskSource.TimeoutChecks(ctx, task.ID, "pull request was closed unmerged")
	case Wait:
	}
	return err
}

// startedAt returns when the task entered checks, falling back to its last
// update when the timestamp is missing or unparseable.
func (r *Reconciler) startedAt(task store.Task) time.Time {
	for _, s := range []*string{task.ChecksStartedAt, &task.UpdatedAt} {
		if s == nil {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, *s); err == nil {
			return t
		}
	}
	return r.now()
}

func (r *Reconciler) handleForgeError(err error, task store.Task, prLink *store.TaskLink, owner, repo string, prNumber int, now time.Time) {
	if rle, ok := asRateLimitError(err); ok {
		r.enterBackoff(owner, rle, now)
		return
	}
	if strings.Contains(err.Error(), "status 404") || strings.Contains(err.Error(), "not found") {
		r.logger.Warn("PR gone; will not retry", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber)
		if terr := r.taskSource.TombstoneLink(context.Background(), task.ID, prLink.ID); terr != nil {
			r.logger.Error("tombstone link error", "task_id", task.ID, "link_id", prLink.ID, "error", terr)
		}
		return
	}
	r.logger.Error("get PR checks error", "task_id", task.ID, "owner", owner, "repo", repo, "pr_number", prNumber, "error", err)
}

// failComment builds the pull-request comment for a failed head commit.
func failComment(checks *forge.PRChecks) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s checks failed on commit %s.\n\nFailed checks:\n", CommentMarker, checks.HeadSHA)
	n := 0
	for _, c := range checks.Contexts {
		if isFailedConclusion(c.Conclusion) {
			fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.URL)
			n++
		}
	}
	if n == 0 {
		b.WriteString("- (none listed)\n")
	}
	return b.String()
}

func asRateLimitError(err error) (*forge.RateLimitError, bool) {
	var rle *forge.RateLimitError
	if errors.As(err, &rle) {
		return rle, true
	}
	return nil, false
}

func (r *Reconciler) enterBackoff(owner string, rle *forge.RateLimitError, now time.Time) {
	notBefore := r.limiter.Enter(owner, rle.Reset, now)
	r.logger.Warn("entering rate limit backoff", "owner", owner, "not_before", notBefore, "status", rle.StatusCode)
}

// checkQuotaFloor mirrors PR-watch: once per pass per owner it consults the
// remaining quota and enters the shared backoff when at or below the floor.
func (r *Reconciler) checkQuotaFloor(ctx context.Context, owner, token string, checked map[string]bool, now time.Time) bool {
	if r.limiter.Floor() <= 0 || checked[owner] {
		return false
	}
	checked[owner] = true

	quota, err := r.remainingQuotaLookup(ctx, token)
	if err != nil {
		if rle, ok := asRateLimitError(err); ok {
			r.enterBackoff(owner, rle, now)
			return true
		}
		r.logger.Warn("remaining quota lookup error", "owner", owner, "error", err)
		return false
	}
	if !r.limiter.BelowFloor(quota.Remaining) {
		return false
	}
	notBefore := r.limiter.Enter(owner, quota.Reset, now)
	r.logger.Warn("remaining quota below floor; entering backoff", "owner", owner, "remaining", quota.Remaining, "floor", r.limiter.Floor(), "not_before", notBefore)
	return true
}
