package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// boardMode identifies the current interaction mode of the board.
// In any mode other than modeNormal, regular navigation keys are suppressed.
type boardMode int

const (
	// modeNormal is the default navigation mode.
	modeNormal boardMode = iota
	// modeDetail is the full-screen task detail view.
	modeDetail
	// modeApproveNote is the optional note input step for an approve action.
	modeApproveNote
	// modeApproveConfirm is the "Approve → done? [y/N]" confirmation step.
	modeApproveConfirm
	// modeRejectReason is the required reason input step for a reject action.
	modeRejectReason
	// modeMergeConfirm is the "Merge PR and complete? [y/N]" confirmation step.
	modeMergeConfirm
	// modeProjectSwitch is the project selection overlay.
	modeProjectSwitch
	// modeApprovedRejectNote is the optional note input step for approved→ready send-back.
	modeApprovedRejectNote
	// modeApprovedRejectConfirm is the "Send back to ready? [y/N]" confirmation step.
	modeApprovedRejectConfirm
	// modeUnblockNote is the optional note input step for an unblock action.
	modeUnblockNote
	// modeUnblockConfirm is the "Unblock → ready? [y/N]" confirmation step.
	modeUnblockConfirm
	// modeFailNote is the optional note input step for a fail action.
	modeFailNote
	// modeFailConfirm is the "Fail → failed? [y/N]" confirmation step.
	modeFailConfirm
	// modeArchiveTaskConfirm is the "Archive task? [y/N]" confirmation step.
	modeArchiveTaskConfirm
	// modeArchiveProjectConfirm is the "Archive project? [y/N]" confirmation step.
	modeArchiveProjectConfirm
	// modeHoldTaskConfirm is the "Hold task? [y/N]" confirmation step.
	modeHoldTaskConfirm
	// modeReleaseTaskConfirm is the "Release task? [y/N]" confirmation step.
	modeReleaseTaskConfirm
)

// BoardModel is the Bubble Tea model for the task board view.
type BoardModel struct {
	client         tuiclient.Client
	config         *tuiconfig.Config
	project        tuiclient.Project
	tasks          map[string][]tuiclient.Task // keyed by state
	selectedTaskID string                      // current selection, keyed by ID
	selectedIndex  int                         // index position within selected column, for nearest-selection on disappear
	selectedColumn int                         // 0=backlog, 1=ready, 2=in_progress, 3=review, 4=done
	scrollOffset   int                         // vertical scroll offset for the task list
	loading        bool
	error          string
	width          int
	height         int
	lastRefresh    time.Time
	// newTickCmd is the function used to arm the next poll tick.
	// Overridable in tests to avoid real timers and to introspect arming.
	newTickCmd func() tea.Cmd

	// Review action state
	mode             boardMode
	reviewInput      textinput.Model // shared textinput for note/reason
	pendingNote      *string         // captured note (nil = omitted) when in modeApproveConfirm
	pendingPRURL     string          // PR URL to merge when in modeMergeConfirm
	pendingTaskID    string          // ID of the task being reviewed
	pendingProjectID string          // ID of the project being archived
	inputHint        string          // hint displayed below the input (e.g. "reason required")
	reviewFromDetail bool            // true when the review flow was started from the detail view

	// Detail view state
	detailTask      tuiclient.TaskDetail // the currently displayed task detail
	detailDocuments []tuiclient.Document // cached documents for opener actions
	detailEvents    []tuiclient.Event    // cached events for the event timeline
	detailViewport  viewport.Model       // scrollable spec viewport
	detailMessage   string               // brief status message (opener result, error, etc.)
	// urlOpener is called to open a URL in the user's browser.
	// In production it is defaultURLOpener; tests inject a recorder to assert the URL.
	urlOpener func(rawURL string) error
	// ghMerger is called to merge a PR via `gh pr merge`.
	// In production it is defaultGHMerger; tests inject a mock to avoid shell execution.
	ghMerger func(ctx context.Context, prURL string) error

	// Project switcher state
	projects           []tuiclient.Project // cached list of all projects
	projectSwitchIndex int                 // current selection in project switcher
}

const (
	stateBacklog    = "backlog"
	stateReady      = "ready"
	stateInProgress = "in_progress"
	stateReview     = "review"
	stateApproved   = "approved"
	stateDone       = "done"
	stateBlocked    = "blocked"
	stateFailed     = "failed"
	stateAbandoned  = "abandoned"
)

var stateOrder = []string{stateBacklog, stateReady, stateInProgress, stateReview, stateApproved, stateDone, stateBlocked, stateFailed, stateAbandoned}
var stateColors = map[string]lipgloss.Color{
	stateBacklog:    lipgloss.Color("8"),
	stateReady:      lipgloss.Color("4"),
	stateInProgress: lipgloss.Color("3"),
	stateReview:     lipgloss.Color("5"),
	stateApproved:   lipgloss.Color("6"),
	stateDone:       lipgloss.Color("2"),
	stateBlocked:    lipgloss.Color("1"),
	stateFailed:     lipgloss.Color("9"),
	stateAbandoned:  lipgloss.Color("8"),
}

// NewBoardModel creates a new board model and starts the initial fetch.
func NewBoardModel(client tuiclient.Client, config *tuiconfig.Config, project tuiclient.Project) *BoardModel {
	inputWidget := textinput.New()
	inputWidget.CharLimit = 512

	m := &BoardModel{
		client:         client,
		config:         config,
		project:        project,
		tasks:          make(map[string][]tuiclient.Task),
		selectedColumn: 2, // in_progress by default
		loading:        true,
		reviewInput:    inputWidget,
		urlOpener:      defaultURLOpener,
		ghMerger:       defaultGHMerger,
	}
	m.newTickCmd = m.defaultTickCmd
	return m
}

// defaultTickCmd is the production tick: fires after PollInterval.
func (m *BoardModel) defaultTickCmd() tea.Cmd {
	return tea.Tick(m.config.PollInterval, func(t time.Time) tea.Msg {
		return tickMsg{}
	})
}

// Init starts the initial fetch and the polling loop.
// Exactly one tick chain is started here; it perpetuates itself in the tickMsg handler.
func (m *BoardModel) Init() tea.Cmd {
	return tea.Batch(
		m.fetchTasksFullRefresh(),
		m.fetchProjects(),
		m.newTickCmd(),
	)
}

// promoteTask creates a command that promotes a task and then refetches.
func (m *BoardModel) promoteTask(taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		err := m.client.PromoteTask(ctx, taskID)
		if err != nil {
			// Use typed error inspection so the friendly branch is reached even when the
			// server returns a structured body (e.g. code=CONFLICT) rather than a raw "409".
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				return promoteErrorMsg{
					taskID: taskID,
					err:    "not in backlog (already moved?)",
				}
			}
			return promoteErrorMsg{
				taskID: taskID,
				err:    fmt.Sprintf("promote failed: %v", err),
			}
		}

		// Promotion succeeded; refetch to get updated board state
		// Issue a new fetch command and return its result
		return m.fetchTasksFullRefresh()()
	}
}

// promoteErrorMsg carries an error from a promote action.
type promoteErrorMsg struct {
	taskID string
	err    string
}

// reviewActionMsg is returned when a review action (approve/reject) completes.
// It carries either a successful refetch (tasks != nil) or an error string.
// fromDetail is true when the action was initiated from the full-screen detail view;
// the handler uses this to return to modeNormal (the board) so the result is visible.
type reviewActionMsg struct {
	// tasks is non-nil on success; it holds the refreshed board data.
	tasks      map[string][]tuiclient.Task
	err        string
	fromDetail bool
}

// projectArchiveMsg is returned when a project archive completes.
// It carries the refreshed projects list or an error.
type projectArchiveMsg struct {
	projects []tuiclient.Project
	err      string
}

// prResolvedMsg is returned when a PR is resolved from a deterministic branch.
// It carries the resolved PR URL and task ID, or an error string.
type prResolvedMsg struct {
	taskID string
	prURL  string
	err    string
}

// reviewApprove creates a command that calls ReviewTask(approve) then TransitionTask(done),
// in that order. If ReviewTask fails, TransitionTask is not called. Both calls use note,
// which may be nil. fromDetail records whether the action was initiated from the detail view
// so the handler can return the user to the board where the result is visible.
// On success, the command triggers a refetch.
func (m *BoardModel) reviewApprove(taskID string, note *string, fromDetail bool) tea.Cmd {
	actor := m.config.Actor
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Step 1: record the review verdict.
		if err := m.client.ReviewTask(ctx, taskID, actor, "approve", note); err != nil {
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("approve review failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		// Step 2: transition to done (terminal state).
		if err := m.client.TransitionTask(ctx, taskID, "done", note); err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				// 409: the task already moved (race). Refetch so the board reflects reality.
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("approve transition 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("approve transition failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		// Success: refetch to reflect the updated board.
		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// reviewReject creates a command that calls ReviewTask(reject) then TransitionTask(ready),
// in that order. If ReviewTask fails, TransitionTask is not called. reason is required and
// must not be empty (the caller must enforce this before invoking). fromDetail records whether
// the action was initiated from the detail view so the handler can return the user to the board.
// On success, the command triggers a refetch.
func (m *BoardModel) reviewReject(taskID string, reason string, fromDetail bool) tea.Cmd {
	actor := m.config.Actor
	reasonPtr := &reason
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Step 1: record the review verdict.
		if err := m.client.ReviewTask(ctx, taskID, actor, "reject", reasonPtr); err != nil {
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("reject review failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		// Step 2: transition back to ready so the task can be reworked.
		if err := m.client.TransitionTask(ctx, taskID, "ready", reasonPtr); err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("reject transition 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("reject transition failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// mergePRCmd creates a command that merges the task's GitHub PR via gh, then transitions
// to done. If the task has no PR link, shows a clear message and doesn't transition.
// On merge failure, surfaces the error and leaves the task un-transitioned.
func (m *BoardModel) mergePRCmd(taskID string, prURL string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		// Step 1: merge the PR via gh.
		if err := m.ghMerger(ctx, prURL); err != nil {
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("PR merge failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		// Step 2: transition to done only after merge succeeds.
		if err := m.client.TransitionTask(ctx, taskID, "done", nil); err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("transition 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("transition failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		// Success: refetch to reflect the updated board.
		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// resolvePRAndMerge creates a command that resolves a PR from the deterministic branch.
// It returns a prResolvedMsg with either the resolved PR URL or an error.
func (m *BoardModel) resolvePRAndMerge(taskID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Extract owner and repo from the project's repo URL.
		owner, repo, err := extractGitHubOwnerRepo(m.project.Repo)
		if err != nil {
			return prResolvedMsg{taskID: taskID, err: fmt.Sprintf("cannot resolve PR: %v", err)}
		}

		// Build the deterministic branch name.
		branch := "mr/" + taskID[:8]

		// Resolve the PR URL from the branch.
		prURL, err := findOpenPRURL(ctx, owner, repo, branch)
		if err != nil {
			return prResolvedMsg{taskID: taskID, err: fmt.Sprintf("no PR found on branch %s: %v", branch, err)}
		}

		return prResolvedMsg{taskID: taskID, prURL: prURL}
	}
}

// approvedRejectCmd creates a command that transitions an approved task back to ready
// with an optional note. This is a plain state transition (approved→ready) that does NOT
// record a review verdict — it's for bouncing approved tasks back for rework.
// On success, the command triggers a refetch.
func (m *BoardModel) approvedRejectCmd(taskID string, note *string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Transition directly without recording a review verdict.
		if err := m.client.TransitionTask(ctx, taskID, "ready", note); err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("bounce transition 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("bounce transition failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// unblockCmd creates a command that transitions a blocked task to ready with an optional note.
func (m *BoardModel) unblockCmd(taskID string, note *string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Transition directly without recording a review verdict.
		if err := m.client.TransitionTask(ctx, taskID, "ready", note); err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("unblock transition 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("unblock transition failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

func (m *BoardModel) failCmd(taskID string, note *string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Transition directly without recording a review verdict.
		if err := m.client.TransitionTask(ctx, taskID, "failed", note); err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("fail transition 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("fail transition failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// archiveTaskCmd creates a command that archives a task.
func (m *BoardModel) archiveTaskCmd(taskID string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := m.client.ArchiveTask(ctx, taskID); err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("archive 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("archive failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// archiveProjectCmd creates a command that archives a project.
func (m *BoardModel) archiveProjectCmd(projectID string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := m.client.ArchiveProject(ctx, projectID); err != nil {
			return projectArchiveMsg{err: fmt.Sprintf("archive failed: %v", err)}
		}

		// Refetch projects to reflect the archived status
		projects, err := m.client.ListProjects(ctx)
		if err != nil {
			return projectArchiveMsg{err: fmt.Sprintf("failed to refetch projects: %v", err)}
		}
		return projectArchiveMsg{projects: projects}
	}
}

// holdTaskCmd creates a command that holds a task (pins it out of automated flow).
func (m *BoardModel) holdTaskCmd(taskID string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := m.client.HoldTask(ctx, taskID); err != nil {
			// Check if it's an API error
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("hold 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("hold failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// releaseTaskCmd creates a command that releases a task (restores automated flow).
func (m *BoardModel) releaseTaskCmd(taskID string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := m.client.ReleaseTask(ctx, taskID); err != nil {
			// Check if it's an API error
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("release 409: %s", apiErr.Message))
				msg.fromDetail = fromDetail
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("release failed: %v", err))
			msg.fromDetail = fromDetail
			return msg
		}

		msg := m.fetchTasksInline(ctx, "")
		msg.fromDetail = fromDetail
		return msg
	}
}

// fetchTasksInline performs a synchronous ListTasks call within an already-running command
// closure (i.e. uses an already-created context) and returns a reviewActionMsg so the
// result surfaces through the reviewActionMsg handler instead of tasksFetchedMsg. This
// avoids starting a second tea.Cmd goroutine from within the first.
func (m *BoardModel) fetchTasksInline(ctx context.Context, errPrefix string) reviewActionMsg {
	tasks, fetchErr := m.client.ListTasks(ctx, m.project.ID)
	if fetchErr != nil {
		errMsg := fmt.Sprintf("refetch failed: %v", fetchErr)
		if errPrefix != "" {
			errMsg = errPrefix + "; " + errMsg
		}
		return reviewActionMsg{err: errMsg}
	}

	bucketed := bucketTasksByState(tasks)

	msg := reviewActionMsg{tasks: bucketed}
	if errPrefix != "" {
		msg.err = errPrefix
	}
	return msg
}

// activeStates are the states re-fetched on every poll tick. Terminal states change
// far less often, so they are excluded from the steady-state poll to keep it cheap.
var activeStates = []string{stateBacklog, stateReady, stateInProgress, stateReview, stateApproved, stateBlocked}

// terminalStates are fetched only at startup, on manual refresh, and after project switches.
var terminalStates = []string{stateDone, stateFailed, stateAbandoned}

// fetchStatesSummary issues one ListTasks call per state with fields=summary and
// concatenates the results. Splitting into per-state calls lets each call ask the server
// to filter server-side, and fields=summary drops the Spec/Result payload per task.
func (m *BoardModel) fetchStatesSummary(ctx context.Context, states []string) ([]tuiclient.Task, error) {
	var all []tuiclient.Task
	for _, state := range states {
		tasks, err := m.client.ListTasks(ctx, m.project.ID, tuiclient.WithState(state), tuiclient.WithFields("summary"))
		if err != nil {
			return nil, err
		}
		all = append(all, tasks...)
	}
	return all, nil
}

// fetchActiveTasks creates a command that fetches only the active-state tasks (backlog,
// ready, in_progress, review, approved, blocked). Used as the steady-state poll body when
// there is no existing terminal-column data to preserve (e.g. tests exercising it directly).
func (m *BoardModel) fetchActiveTasks() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		tasks, err := m.fetchStatesSummary(ctx, activeStates)
		if err != nil {
			return tasksFetchedMsg{err: err}
		}

		return tasksFetchedMsg{tasks: bucketTasksByState(tasks)}
	}
}

// fetchActiveTasksAndMerge creates a command that fetches only the active-state tasks and
// merges them with the existing terminal columns (done, failed, abandoned), which are not
// re-fetched on every tick. The terminal columns are captured here, on the goroutine calling
// this method (the main Bubble Tea event loop), rather than inside the returned tea.Cmd's
// closure: tea.Cmd bodies run on their own goroutine concurrently with Update, and Update is
// the only other place that mutates m.tasks, so reading m.tasks from inside the closure would
// be a data race.
func (m *BoardModel) fetchActiveTasksAndMerge() tea.Cmd {
	terminalCols := make(map[string][]tuiclient.Task, len(terminalStates))
	for _, state := range terminalStates {
		terminalCols[state] = m.tasks[state]
	}

	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		tasks, err := m.fetchStatesSummary(ctx, activeStates)
		if err != nil {
			return tasksFetchedMsg{err: err}
		}

		bucketed := bucketTasksByState(tasks)
		for _, state := range terminalStates {
			bucketed[state] = terminalCols[state]
		}

		return tasksFetchedMsg{tasks: bucketed}
	}
}

// fetchTasksFullRefresh creates a command that fetches every state (active and terminal),
// one ListTasks call per state with fields=summary. Used for the initial load, manual
// refresh ('r'), and project switches, none of which are on the steady-state poll path.
func (m *BoardModel) fetchTasksFullRefresh() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		tasks, err := m.fetchStatesSummary(ctx, stateOrder)
		if err != nil {
			return tasksFetchedMsg{err: err}
		}

		return tasksFetchedMsg{tasks: bucketTasksByState(tasks)}
	}
}

// bucketTasksByState groups tasks into buckets by state, filtering out review-kind
// and merge-kind tasks from the done column. This ensures the done column shows only
// implement deliverables, not the bookkeeping tasks the server spawns alongside them.
func bucketTasksByState(tasks []tuiclient.Task) map[string][]tuiclient.Task {
	bucketed := make(map[string][]tuiclient.Task)
	for _, state := range stateOrder {
		bucketed[state] = []tuiclient.Task{}
	}

	for _, task := range tasks {
		if _, exists := bucketed[task.State]; !exists {
			continue
		}
		// Skip review-kind and merge-kind tasks from the done column
		if task.State == stateDone && (task.Kind == "review" || task.Kind == "merge") {
			continue
		}
		bucketed[task.State] = append(bucketed[task.State], task)
	}

	// Sort each state's tasks in natural title order
	for _, taskList := range bucketed {
		sortTasksNatural(taskList)
	}

	return bucketed
}

// sortTasksNatural orders tasks within a column by title in natural order, so MR-1 < MR-2 <
// ... < MR-10 (a plain lexicographic sort puts MR-10 before MR-2). Ties break by ID so the
// order is stable across refreshes. Note: tasks created in one batch share a created_at, so
// sorting by time can't disambiguate them — title order is the predictable choice.
func sortTasksNatural(tasks []tuiclient.Task) {
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Title != tasks[j].Title {
			return naturalLess(tasks[i].Title, tasks[j].Title)
		}
		return tasks[i].ID < tasks[j].ID
	})
}

// naturalLess reports whether a sorts before b in natural order: runs of digits are compared
// numerically (so "MR-2" < "MR-10"), all other characters byte-by-byte.
func naturalLess(a, b string) bool {
	ia, ib := 0, 0
	for ia < len(a) && ib < len(b) {
		da := a[ia] >= '0' && a[ia] <= '9'
		db := b[ib] >= '0' && b[ib] <= '9'
		if da && db {
			ja, jb := ia, ib
			for ja < len(a) && a[ja] >= '0' && a[ja] <= '9' {
				ja++
			}
			for jb < len(b) && b[jb] >= '0' && b[jb] <= '9' {
				jb++
			}
			na := strings.TrimLeft(a[ia:ja], "0")
			nb := strings.TrimLeft(b[ib:jb], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			ia, ib = ja, jb
			continue
		}
		if a[ia] != b[ib] {
			return a[ia] < b[ib]
		}
		ia++
		ib++
	}
	return len(a)-ia < len(b)-ib
}

// fetchProjects creates a command that fetches the list of projects.
func (m *BoardModel) fetchProjects() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		projects, err := m.client.ListProjects(ctx)
		if err != nil {
			return projectsFetchedMsg{
				err: err,
			}
		}

		return projectsFetchedMsg{
			projects: projects,
		}
	}
}

type tasksFetchedMsg struct {
	tasks map[string][]tuiclient.Task
	err   error
}

// projectsFetchedMsg is returned when the projects list is fetched.
type projectsFetchedMsg struct {
	projects []tuiclient.Project
	err      error
}

// tickMsg is sent by the poll tick to trigger a fetch and re-arm the next tick.
type tickMsg struct{}

// Update handles messages from Bubble Tea.
func (m *BoardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		// Reinitialize the detail viewport if we're in detail mode.
		if m.mode == modeDetail {
			m.initDetailViewport(m.detailTask)
		}
		return m, nil

	case tea.KeyMsg:
		// Detail mode: all keys go to the detail handler — board nav must not fire.
		if m.mode == modeDetail {
			return m.updateDetailMode(msg)
		}

		// Project switcher mode: all keys go to the switcher handler.
		if m.mode == modeProjectSwitch {
			return m.updateProjectSwitchMode(msg)
		}

		// When a text-input or confirm mode is active, route keys to the review flow
		// instead of the normal navigation handlers.
		if m.mode != modeNormal {
			return m.updateReviewMode(msg)
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		// Column navigation
		case "left", "h":
			if m.selectedColumn > 0 {
				m.selectedColumn--
				m.selectedIndex = 0 // Reset to top when changing columns
				m.scrollOffset = 0  // Reset scroll to top
				m.ensureSelectionInColumn()
			}

		case "right", "l":
			if m.selectedColumn < len(stateOrder)-1 {
				m.selectedColumn++
				m.selectedIndex = 0 // Reset to top when changing columns
				m.scrollOffset = 0  // Reset scroll to top
				m.ensureSelectionInColumn()
			}

		// Selection within column
		case "up", "k":
			tasksInColumn := m.getTasksInSelectedColumn()
			if len(tasksInColumn) == 0 {
				return m, nil
			}

			if m.selectedTaskID == "" {
				m.selectedTaskID = tasksInColumn[0].ID
				m.selectedIndex = 0
			} else {
				// Find current position
				for i, t := range tasksInColumn {
					if t.ID == m.selectedTaskID && i > 0 {
						m.selectedTaskID = tasksInColumn[i-1].ID
						m.selectedIndex = i - 1
						break
					}
				}
			}
			m.clampScrollToSelection()

		case "down", "j":
			tasksInColumn := m.getTasksInSelectedColumn()
			if len(tasksInColumn) == 0 {
				return m, nil
			}

			if m.selectedTaskID == "" {
				m.selectedTaskID = tasksInColumn[0].ID
				m.selectedIndex = 0
			} else {
				// Find current position
				for i, t := range tasksInColumn {
					if t.ID == m.selectedTaskID && i < len(tasksInColumn)-1 {
						m.selectedTaskID = tasksInColumn[i+1].ID
						m.selectedIndex = i + 1
						break
					}
				}
			}
			m.clampScrollToSelection()

		// Refresh: issue one-shot fetch of all columns (active and terminal); do NOT arm
		// a new tick. The single perpetual tick chain started in Init re-arms itself from
		// tickMsg.
		case "r":
			return m, m.fetchTasksFullRefresh()

		// Open detail view for the selected task.
		case "enter":
			if m.selectedTaskID != "" {
				m.mode = modeDetail
				m.detailMessage = ""
				return m, m.fetchDetailCmd(m.selectedTaskID)
			}

		// Promote: only on backlog tasks
		case "p":
			if m.selectedColumn == 0 && m.selectedTaskID != "" {
				return m, m.promoteTask(m.selectedTaskID)
			}

		// Switch project
		case "P":
			m.mode = modeProjectSwitch
			m.projectSwitchIndex = 0
			// Find the current project's index for re-selection purposes
			for i, proj := range m.projects {
				if proj.ID == m.project.ID {
					m.projectSwitchIndex = i
					break
				}
			}
			return m, m.fetchProjects()

		// Approve: only on review column tasks
		case "a":
			if m.selectedColumn == 3 && m.selectedTaskID != "" {
				m.pendingTaskID = m.selectedTaskID
				m.reviewInput.Placeholder = "optional note (enter to skip)"
				m.reviewInput.SetValue("")
				m.reviewInput.Focus()
				m.inputHint = ""
				m.mode = modeApproveNote
				var cmd tea.Cmd
				m.reviewInput, cmd = m.reviewInput.Update(nil)
				return m, cmd
			}

		// Reject: only on review column tasks
		case "x":
			if m.selectedColumn == 3 && m.selectedTaskID != "" {
				m.pendingTaskID = m.selectedTaskID
				m.reviewInput.Placeholder = "rejection reason (required)"
				m.reviewInput.SetValue("")
				m.reviewInput.Focus()
				m.inputHint = ""
				m.mode = modeRejectReason
				var cmd tea.Cmd
				m.reviewInput, cmd = m.reviewInput.Update(nil)
				return m, cmd
			}

		// Bounce back: only on approved column tasks
		case "b":
			if m.selectedColumn == 4 && m.selectedTaskID != "" {
				m.pendingTaskID = m.selectedTaskID
				m.reviewInput.Placeholder = "optional note (enter to skip)"
				m.reviewInput.SetValue("")
				m.reviewInput.Focus()
				m.inputHint = ""
				m.mode = modeApprovedRejectNote
				var cmd tea.Cmd
				m.reviewInput, cmd = m.reviewInput.Update(nil)
				return m, cmd
			}

		// Unblock: only on blocked column tasks
		case "u":
			if m.selectedColumn == 6 && m.selectedTaskID != "" {
				m.pendingTaskID = m.selectedTaskID
				m.reviewInput.Placeholder = "optional note (enter to skip)"
				m.reviewInput.SetValue("")
				m.reviewInput.Focus()
				m.inputHint = ""
				m.mode = modeUnblockNote
				var cmd tea.Cmd
				m.reviewInput, cmd = m.reviewInput.Update(nil)
				return m, cmd
			}

		case "f":
			if m.selectedColumn == 6 && m.selectedTaskID != "" {
				m.pendingTaskID = m.selectedTaskID
				m.reviewInput.Placeholder = "optional note (enter to skip)"
				m.reviewInput.SetValue("")
				m.reviewInput.Focus()
				m.inputHint = ""
				m.mode = modeFailNote
				var cmd tea.Cmd
				m.reviewInput, cmd = m.reviewInput.Update(nil)
				return m, cmd
			}

		// Archive: archive the selected task
		case "z":
			if m.selectedTaskID != "" {
				m.pendingTaskID = m.selectedTaskID
				m.mode = modeArchiveTaskConfirm
				return m, nil
			}

		// Hold/Release: hold or release the selected task
		case "t":
			if m.selectedTaskID != "" {
				// Determine whether to hold or release based on current held status
				for _, taskList := range m.tasks {
					for _, t := range taskList {
						if t.ID == m.selectedTaskID {
							m.pendingTaskID = m.selectedTaskID
							if t.Held {
								m.mode = modeReleaseTaskConfirm
							} else {
								m.mode = modeHoldTaskConfirm
							}
							return m, nil
						}
					}
				}
			}

		// Help (stub for TUI-3+)
		case "?":
			// TODO: show help overlay
		}

	case detailFetchedMsg:
		if msg.err != nil {
			// Fall back to board view with an error banner.
			m.mode = modeNormal
			m.error = fmt.Sprintf("detail load failed: %v", msg.err)
			return m, nil
		}
		m.detailTask = msg.task
		m.detailDocuments = msg.documents
		m.detailEvents = msg.events
		// (Re)initialize the viewport with the full detail content.
		m.initDetailViewport(msg.task)
		return m, nil

	case openerResultMsg:
		m.detailMessage = msg.message
		return m, nil

	case prResolvedMsg:
		if msg.err != "" {
			m.detailMessage = msg.err
			return m, nil
		}
		// Enter merge confirm mode with the resolved PR.
		m.pendingTaskID = msg.taskID
		m.pendingPRURL = msg.prURL
		m.reviewFromDetail = true
		m.reviewInput.Placeholder = fmt.Sprintf("merge %s and complete? [y/N]", msg.prURL)
		m.reviewInput.SetValue("")
		m.reviewInput.Focus()
		m.inputHint = ""
		m.mode = modeMergeConfirm
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(nil)
		return m, cmd

	case promoteErrorMsg:
		m.error = msg.err
		return m, nil

	case reviewActionMsg:
		if msg.err != "" {
			m.error = msg.err
		} else {
			m.error = ""
		}
		if msg.tasks != nil {
			m.loading = false
			m.tasks = msg.tasks
			m.lastRefresh = time.Now()
			m.ensureSelectionInColumn()
		}
		// When the action originated from the detail view, the task has left "review"
		// (or a race was detected). Return to the board so m.error and the refreshed
		// state are both visible — the board view renders m.error; the detail view does not.
		if msg.fromDetail {
			m.mode = modeNormal
		}
		return m, nil

	case tasksFetchedMsg:
		if msg.err != nil {
			m.error = fmt.Sprintf("Error: %v", msg.err)
			// Return without arming a tick; the single tick chain in tickMsg handles
			// re-arming itself independently.
			return m, nil
		}

		m.loading = false
		m.error = ""
		m.tasks = msg.tasks
		m.lastRefresh = time.Now()
		m.ensureSelectionInColumn()

		// Just update data; do NOT arm a tick. The single tick chain in tickMsg handles
		// re-arming itself.
		return m, nil

	case projectsFetchedMsg:
		if msg.err != nil {
			m.error = fmt.Sprintf("Error fetching projects: %v", msg.err)
			if m.mode == modeProjectSwitch {
				m.mode = modeNormal
			}
			return m, nil
		}
		m.projects = msg.projects
		// If we're in project switcher mode, preserve the current project selection by ID
		if m.mode == modeProjectSwitch {
			for i, proj := range m.projects {
				if proj.ID == m.project.ID {
					m.projectSwitchIndex = i
					break
				}
			}
		}
		return m, nil

	case projectArchiveMsg:
		if msg.err != "" {
			m.error = msg.err
		} else {
			m.error = ""
			m.projects = msg.projects
			// If the archived project was the current one, switch to the first available
			currentProjectFound := false
			for _, p := range msg.projects {
				if p.ID == m.project.ID {
					currentProjectFound = true
					break
				}
			}
			if !currentProjectFound && len(msg.projects) > 0 {
				m.project = msg.projects[0]
				// Refetch tasks for the new project; a project switch needs the terminal
				// columns too, so use the full refresh rather than the active-only poll.
				m.loading = true
				m.tasks = make(map[string][]tuiclient.Task)
				return m, m.fetchTasksFullRefresh()
			}
		}
		m.mode = modeNormal
		return m, nil

	case tickMsg:
		// Re-arm exactly one next tick and issue a fetch.
		// This is the ONLY place (besides Init) where a new tick is armed.
		// The fetch merges freshly-polled active states with the existing terminal
		// columns rather than re-fetching them.
		return m, tea.Batch(
			m.fetchActiveTasksAndMerge(),
			m.newTickCmd(),
		)
	}

	return m, nil
}

// updateDetailMode handles key events while the full-screen detail view is active.
// Board navigation is fully suppressed in this mode.
func (m *BoardModel) updateDetailMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc", "q":
		// Return to the board.
		m.mode = modeNormal
		m.detailMessage = ""
		return m, nil

	// Spec viewport scrolling.
	case "up", "k":
		m.detailViewport.LineUp(1)
		return m, nil
	case "down", "j":
		m.detailViewport.LineDown(1)
		return m, nil
	case "pgup":
		m.detailViewport.HalfViewUp()
		return m, nil
	case "pgdown":
		m.detailViewport.HalfViewDown()
		return m, nil

	// Open PR link.
	case "o":
		return m, m.openPRCmd(m.detailTask)

	// Open the task's source document.
	case "s":
		return m, m.openSourceDocCmd(m.detailTask, m.detailDocuments)

	// Open the project's base design document.
	case "d":
		return m, m.openDesignDocCmd(m.detailDocuments)

	// Approve / reject: only available for review tasks; reuse existing review flow.
	case "a":
		if m.detailTask.State == stateReview {
			m.pendingTaskID = m.detailTask.ID
			m.reviewFromDetail = true
			m.reviewInput.Placeholder = "optional note (enter to skip)"
			m.reviewInput.SetValue("")
			m.reviewInput.Focus()
			m.inputHint = ""
			m.mode = modeApproveNote
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(nil)
			return m, cmd
		}

	case "x":
		if m.detailTask.State == stateReview {
			m.pendingTaskID = m.detailTask.ID
			m.reviewFromDetail = true
			m.reviewInput.Placeholder = "rejection reason (required)"
			m.reviewInput.SetValue("")
			m.reviewInput.Focus()
			m.inputHint = ""
			m.mode = modeRejectReason
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(nil)
			return m, cmd
		}

	// Merge PR and complete: only available for approved tasks.
	case "m":
		if m.detailTask.State == stateApproved {
			// Find the PR link.
			var prURL string
			for _, link := range m.detailTask.Links {
				if link.Kind == "pr" {
					prURL = link.Value
					break
				}
			}

			// If no PR link, try to resolve from the deterministic branch.
			if prURL == "" {
				return m, m.resolvePRAndMerge(m.detailTask.ID)
			}

			m.pendingTaskID = m.detailTask.ID
			m.pendingPRURL = prURL
			m.reviewFromDetail = true
			m.reviewInput.Placeholder = fmt.Sprintf("merge %s and complete? [y/N]", prURL)
			m.reviewInput.SetValue("")
			m.reviewInput.Focus()
			m.inputHint = ""
			m.mode = modeMergeConfirm
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(nil)
			return m, cmd
		}

	// Switch project from detail view
	case "P":
		// Exit detail view and enter project switcher mode
		m.mode = modeProjectSwitch
		m.projectSwitchIndex = 0
		// Find the current project's index for re-selection purposes
		for i, proj := range m.projects {
			if proj.ID == m.project.ID {
				m.projectSwitchIndex = i
				break
			}
		}
		return m, m.fetchProjects()
	}

	return m, nil
}

// updateProjectSwitchMode handles key events while the project switcher overlay is active.
func (m *BoardModel) updateProjectSwitchMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		// Cancel and return to normal mode
		m.mode = modeNormal
		return m, nil

	case "up", "k":
		if m.projectSwitchIndex > 0 {
			m.projectSwitchIndex--
		}
		return m, nil

	case "down", "j":
		if m.projectSwitchIndex < len(m.projects)-1 {
			m.projectSwitchIndex++
		}
		return m, nil

	case "enter":
		if m.projectSwitchIndex >= 0 && m.projectSwitchIndex < len(m.projects) {
			selectedProject := m.projects[m.projectSwitchIndex]
			// Only switch if it's a different project
			if selectedProject.ID != m.project.ID {
				m.project = selectedProject
				// Reset all board state to avoid leaking data across projects
				m.selectedTaskID = ""
				m.selectedIndex = 0
				m.selectedColumn = 2 // Reset to in_progress column
				m.tasks = make(map[string][]tuiclient.Task)
				m.detailTask = tuiclient.TaskDetail{}
				m.detailDocuments = nil
				m.detailEvents = nil
				m.mode = modeNormal
				m.loading = true
				// Refetch tasks for the new project; use the full refresh so the
				// terminal columns (done, failed, abandoned) are populated too.
				return m, m.fetchTasksFullRefresh()
			}
			// Same project selected: just close the switcher
			m.mode = modeNormal
		}
		return m, nil

	case "z":
		// Archive the selected project
		if m.projectSwitchIndex >= 0 && m.projectSwitchIndex < len(m.projects) {
			selectedProject := m.projects[m.projectSwitchIndex]
			m.pendingProjectID = selectedProject.ID
			m.mode = modeArchiveProjectConfirm
		}
		return m, nil
	}

	return m, nil
}

// updateReviewMode handles key events when the model is in one of the review input modes.
// It returns the updated model and any command to run. Normal navigation is suppressed.
func (m *BoardModel) updateReviewMode(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeApproveNote:
		switch msg.String() {
		case "esc":
			// Cancel the approve action entirely.
			m.cancelReviewMode()
			return m, nil
		case "enter":
			// Capture the note (nil if empty), then move to confirm step.
			noteValue := strings.TrimSpace(m.reviewInput.Value())
			if noteValue == "" {
				m.pendingNote = nil
			} else {
				m.pendingNote = &noteValue
			}
			m.mode = modeApproveConfirm
			return m, nil
		default:
			// Pass the key to the text input.
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(msg)
			return m, cmd
		}

	case modeApproveConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture origin before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			note := m.pendingNote
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.reviewApprove(taskID, note, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeRejectReason:
		switch msg.String() {
		case "esc":
			m.cancelReviewMode()
			return m, nil
		case "enter":
			reasonValue := strings.TrimSpace(m.reviewInput.Value())
			if reasonValue == "" {
				// Empty reason is not allowed — show hint and stay in mode.
				m.inputHint = "reason is required"
				return m, nil
			}
			// Capture origin before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.reviewReject(taskID, reasonValue, originFromDetail)
		default:
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(msg)
			return m, cmd
		}

	case modeMergeConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture values before cancelReviewMode clears them.
			taskID := m.pendingTaskID
			prURL := m.pendingPRURL
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.mergePRCmd(taskID, prURL, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeApprovedRejectNote:
		switch msg.String() {
		case "esc":
			// Cancel the send-back action entirely.
			m.cancelReviewMode()
			return m, nil
		case "enter":
			// Capture the note (nil if empty), then move to confirm step.
			noteValue := strings.TrimSpace(m.reviewInput.Value())
			if noteValue == "" {
				m.pendingNote = nil
			} else {
				m.pendingNote = &noteValue
			}
			m.mode = modeApprovedRejectConfirm
			return m, nil
		default:
			// Pass the key to the text input.
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(msg)
			return m, cmd
		}

	case modeApprovedRejectConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture origin before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			note := m.pendingNote
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.approvedRejectCmd(taskID, note, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeUnblockNote:
		switch msg.String() {
		case "esc":
			// Cancel the unblock action entirely.
			m.cancelReviewMode()
			return m, nil
		case "enter":
			// Capture the note (nil if empty), then move to confirm step.
			noteValue := strings.TrimSpace(m.reviewInput.Value())
			if noteValue == "" {
				m.pendingNote = nil
			} else {
				m.pendingNote = &noteValue
			}
			m.mode = modeUnblockConfirm
			return m, nil
		default:
			// Pass the key to the text input.
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(msg)
			return m, cmd
		}

	case modeUnblockConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture origin before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			note := m.pendingNote
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.unblockCmd(taskID, note, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeFailNote:
		switch msg.String() {
		case "esc":
			// Cancel the fail action entirely.
			m.cancelReviewMode()
			return m, nil
		case "enter":
			// Capture the note (nil if empty), then move to confirm step.
			noteValue := strings.TrimSpace(m.reviewInput.Value())
			if noteValue == "" {
				m.pendingNote = nil
			} else {
				m.pendingNote = &noteValue
			}
			m.mode = modeFailConfirm
			return m, nil
		default:
			// Pass the key to the text input.
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(msg)
			return m, cmd
		}

	case modeFailConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture origin before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			note := m.pendingNote
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.failCmd(taskID, note, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeArchiveTaskConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture origin before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.archiveTaskCmd(taskID, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeHoldTaskConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture the task ID before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.holdTaskCmd(taskID, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeReleaseTaskConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture the task ID before cancelReviewMode clears it.
			taskID := m.pendingTaskID
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.releaseTaskCmd(taskID, originFromDetail)
		}
		// Ignore all other keys in confirm mode.
		return m, nil

	case modeArchiveProjectConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			// User declined or pressed escape — cancel.
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			// Capture the project ID before cancelReviewMode clears it.
			projectID := m.pendingProjectID
			m.cancelReviewMode()
			return m, m.archiveProjectCmd(projectID)
		}
		// Ignore all other keys in confirm mode.
		return m, nil
	}

	return m, nil
}

// cancelReviewMode resets all review-mode state and returns to the appropriate mode.
// If the review was started from the detail view, we return to modeDetail; otherwise modeNormal.
func (m *BoardModel) cancelReviewMode() {
	if m.reviewFromDetail {
		m.mode = modeDetail
	} else {
		m.mode = modeNormal
	}
	m.reviewFromDetail = false
	m.pendingNote = nil
	m.pendingPRURL = ""
	m.pendingTaskID = ""
	m.pendingProjectID = ""
	m.inputHint = ""
	m.reviewInput.SetValue("")
	m.reviewInput.Blur()
}

// getTasksInSelectedColumn returns the tasks in the currently selected column.
func (m *BoardModel) getTasksInSelectedColumn() []tuiclient.Task {
	state := stateOrder[m.selectedColumn]
	return m.tasks[state]
}

// getVisibleTaskHeight returns the number of tasks that fit in the visible area.
// Each task renders 1-2 lines (assignee-bearing columns show 2 lines).
// Account for project name, tabs, separator, and help bar.
func (m *BoardModel) getVisibleTaskHeight() int {
	// Convert line budget to task count: (height - 5) lines / 2 lines per task.
	// At least 1 task visible, at most (height-5)/2 tasks.
	if m.height < 6 {
		return 1
	}
	tasksVisible := (m.height - 5) / 2
	if tasksVisible < 1 {
		return 1
	}
	return tasksVisible
}

// clampScrollToSelection ensures the scroll offset shows the selected task.
// If the selected index is above the viewport, scroll up; if below, scroll down.
func (m *BoardModel) clampScrollToSelection() {
	visibleHeight := m.getVisibleTaskHeight()
	// If selected index is above the scroll window, scroll up
	if m.selectedIndex < m.scrollOffset {
		m.scrollOffset = m.selectedIndex
	}
	// If selected index is below the scroll window, scroll down
	if m.selectedIndex >= m.scrollOffset+visibleHeight {
		m.scrollOffset = m.selectedIndex - visibleHeight + 1
	}
	// Ensure scroll offset doesn't go negative
	if m.scrollOffset < 0 {
		m.scrollOffset = 0
	}
	// Ensure scroll offset doesn't show blank area at the bottom
	tasksInColumn := m.getTasksInSelectedColumn()
	maxOffset := len(tasksInColumn) - visibleHeight
	if maxOffset < 0 {
		maxOffset = 0
	}
	if m.scrollOffset > maxOffset {
		m.scrollOffset = maxOffset
	}
}

// ensureSelectionInColumn ensures the selected task exists in the current column.
// If the task is gone, select the nearest task at the prior index position (clamped to available).
// If no tasks, clear selection.
func (m *BoardModel) ensureSelectionInColumn() {
	tasksInColumn := m.getTasksInSelectedColumn()

	if len(tasksInColumn) == 0 {
		m.selectedTaskID = ""
		m.selectedIndex = 0
		m.scrollOffset = 0
		return
	}

	// Check if selected task is still in this column
	for i, t := range tasksInColumn {
		if t.ID == m.selectedTaskID {
			m.selectedIndex = i
			m.clampScrollToSelection()
			return // Selection is valid
		}
	}

	// Selection lost (task disappeared). Select at the same index position,
	// clamped to the available range.
	newIdx := m.selectedIndex
	if newIdx >= len(tasksInColumn) {
		newIdx = len(tasksInColumn) - 1
	}
	m.selectedIndex = newIdx
	m.selectedTaskID = tasksInColumn[newIdx].ID
	m.clampScrollToSelection()
}

// View renders the board (or the detail view if in modeDetail).
func (m *BoardModel) View() string {
	if m.width < 40 {
		return "Terminal too narrow. Please resize."
	}

	// Full-screen detail view.
	if m.mode == modeDetail {
		var b strings.Builder
		b.WriteString(m.renderDetailView())
		b.WriteString("\n")
		b.WriteString(strings.Repeat("─", m.width))
		b.WriteString("\n")
		b.WriteString(m.renderDetailHelpBar())
		return b.String()
	}

	// Project switcher overlay.
	if m.mode == modeProjectSwitch {
		var b strings.Builder
		if m.project.Name != "" {
			projName := truncateString(m.project.Name, m.width)
			b.WriteString(projName)
			b.WriteString("\n")
		}
		b.WriteString(m.renderTabs())
		b.WriteString("\n")
		b.WriteString(strings.Repeat("─", m.width))
		b.WriteString("\n")
		b.WriteString(m.renderProjectSwitchOverlay())
		return b.String()
	}

	var b strings.Builder

	// Render project name
	if m.project.Name != "" {
		projName := truncateString(m.project.Name, m.width)
		b.WriteString(projName)
		b.WriteString("\n")
	}

	// Render tabs (column headers)
	b.WriteString(m.renderTabs())
	b.WriteString("\n")

	// Separator
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")

	// When a review mode is active, overlay the input/confirm prompt instead of the task list.
	if m.mode != modeNormal {
		b.WriteString(m.renderReviewOverlay())
	} else {
		b.WriteString(m.renderColumnTasks())
	}

	// Help bar
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")
	b.WriteString(m.renderHelpBar())

	return b.String()
}

// renderReviewOverlay renders the text input or confirm prompt used during review actions.
func (m *BoardModel) renderReviewOverlay() string {
	var b strings.Builder
	switch m.mode {
	case modeApproveNote:
		b.WriteString("Approve — add an optional note:\n")
		b.WriteString(m.reviewInput.View())
		b.WriteString("\n")
		b.WriteString("(enter to continue, esc to cancel)\n")
	case modeApproveConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Approve %s → done? [y/N] ", taskID))
		b.WriteString("(done is terminal and cannot be undone)\n")
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeRejectReason:
		b.WriteString("Reject — reason (required):\n")
		b.WriteString(m.reviewInput.View())
		b.WriteString("\n")
		if m.inputHint != "" {
			b.WriteString("hint: " + m.inputHint + "\n")
		}
		b.WriteString("(enter to submit, esc to cancel)\n")
	case modeMergeConfirm:
		b.WriteString(m.reviewInput.View())
		b.WriteString("\n")
		b.WriteString("(y to confirm merge and complete, n/esc to cancel)\n")
	case modeApprovedRejectNote:
		b.WriteString("Bounce back to ready — optional note:\n")
		b.WriteString(m.reviewInput.View())
		b.WriteString("\n")
		b.WriteString("(enter to continue, esc to cancel)\n")
	case modeApprovedRejectConfirm:
		b.WriteString("Send back to ready? [y/N] ")
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeUnblockNote:
		b.WriteString("Unblock — add an optional note:\n")
		b.WriteString(m.reviewInput.View())
		b.WriteString("\n")
		b.WriteString("(enter to continue, esc to cancel)\n")
	case modeUnblockConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Unblock %s → ready? [y/N] ", taskID))
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeFailNote:
		b.WriteString("Fail — add an optional note:\n")
		b.WriteString(m.reviewInput.View())
		b.WriteString("\n")
		b.WriteString("(enter to continue, esc to cancel)\n")
	case modeFailConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Fail %s → failed? [y/N] ", taskID))
		b.WriteString("(failed is terminal, y to confirm, n/esc to cancel)\n")
	case modeArchiveTaskConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Archive %s? [y/N] ", taskID))
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeHoldTaskConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Hold %s (pin out of automated flow)? [y/N] ", taskID))
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeReleaseTaskConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Release %s (restore automated flow)? [y/N] ", taskID))
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeArchiveProjectConfirm:
		b.WriteString("Archive project? [y/N] ")
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	}
	return b.String()
}

// renderProjectSwitchOverlay renders the project switcher overlay.
func (m *BoardModel) renderProjectSwitchOverlay() string {
	var b strings.Builder
	b.WriteString("Switch project (↑/↓ or k/j to select, enter to switch, z to archive, esc to cancel):\n\n")
	for i, p := range m.projects {
		cursor := "  "
		if i == m.projectSwitchIndex {
			cursor = "> "
		}
		b.WriteString(fmt.Sprintf("%s%s\n", cursor, p.Name))
	}
	return b.String()
}

// truncateString truncates a string to the given width, adding "…" if truncated.
func truncateString(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return "…"
	}
	return s[:width-1] + "…"
}

// renderTabs renders the column tabs with counts.
func (m *BoardModel) renderTabs() string {
	var tabs []string
	for i, state := range stateOrder {
		count := len(m.tasks[state])
		tab := fmt.Sprintf("%s(%d)", state, count)

		if i == m.selectedColumn {
			// Active tab: surrounded by angle brackets
			tab = fmt.Sprintf("‹%s›", tab)
		}

		tabs = append(tabs, tab)
	}

	return strings.Join(tabs, "  ")
}

// renderColumnTasks renders the tasks in the selected column, respecting scroll offset.
func (m *BoardModel) renderColumnTasks() string {
	if m.loading {
		return "Loading..."
	}

	if m.error != "" {
		return fmt.Sprintf("Error: %s\nPress 'r' to retry.", m.error)
	}

	tasksInColumn := m.getTasksInSelectedColumn()

	if len(tasksInColumn) == 0 {
		return "(empty)"
	}

	// Calculate the visible task range
	visibleHeight := m.getVisibleTaskHeight()
	endOffset := m.scrollOffset + visibleHeight
	if endOffset > len(tasksInColumn) {
		endOffset = len(tasksInColumn)
	}

	var b strings.Builder
	for i := m.scrollOffset; i < endOffset; i++ {
		task := tasksInColumn[i]
		isSelected := task.ID == m.selectedTaskID
		prefix := " "
		if isSelected {
			prefix = "▸"
		}

		taskIDDisplay := task.ID
		if len(task.ID) > 8 {
			taskIDDisplay = task.ID[:8]
		}
		modelBadge := fmt.Sprintf("[%s]", task.Model)
		b.WriteString(fmt.Sprintf("%s %s %s  %s\n", prefix, taskIDDisplay, modelBadge, task.Title))

		// Show assignee for in_progress, review, approved, and done states
		shouldShowAssignee := task.State == stateInProgress || task.State == stateReview ||
			task.State == stateApproved || task.State == stateDone
		if shouldShowAssignee && task.Assignee != nil {
			assignee := *task.Assignee
			// Truncate long agent IDs to keep layout compact
			if len(assignee) > 20 {
				assignee = assignee[:17] + "…"
			}

			// For in_progress, also show lease countdown and updated time
			if task.State == stateInProgress {
				leaseStatus := "no lease"
				if task.LeaseExpiresAt != nil {
					leaseStatus = m.formatLeaseCountdown(*task.LeaseExpiresAt)
				}
				b.WriteString(fmt.Sprintf("    @%s · lease %s · updated %s ago\n", assignee, leaseStatus, m.formatTime(task.UpdatedAt)))
			} else {
				// For other states, just show assignee and updated time
				b.WriteString(fmt.Sprintf("    @%s · updated %s ago\n", assignee, m.formatTime(task.UpdatedAt)))
			}
		}
	}

	return b.String()
}

// formatLeaseCountdown formats the lease expiration time as a countdown.
func (m *BoardModel) formatLeaseCountdown(expiresAt string) string {
	// Parse the RFC3339 timestamp
	t, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return "invalid"
	}

	now := time.Now()
	if now.After(t) {
		return "EXPIRED"
	}

	remaining := t.Sub(now)
	hours := int(remaining.Hours())
	minutes := int(remaining.Minutes()) % 60
	seconds := int(remaining.Seconds()) % 60

	if hours > 0 {
		return fmt.Sprintf("%dh%dm%ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

// formatTime formats a timestamp relative to now.
func (m *BoardModel) formatTime(timestamp string) string {
	t, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "unknown"
	}

	now := time.Now()
	diff := now.Sub(t)

	if diff < time.Minute {
		return fmt.Sprintf("%ds", int(diff.Seconds()))
	}
	if diff < time.Hour {
		return fmt.Sprintf("%dm", int(diff.Minutes()))
	}
	if diff < 24*time.Hour {
		return fmt.Sprintf("%dh", int(diff.Hours()))
	}

	return fmt.Sprintf("%dd", int(diff.Hours()/24))
}

// renderHelpBar renders the bottom help bar.
// Show column-specific actions: `p promote` on backlog, `a approve` / `x reject` on review, `b bounce` on approved.
func (m *BoardModel) renderHelpBar() string {
	// While in a review input mode, show mode-specific hints.
	if m.mode != modeNormal {
		return "esc cancel"
	}
	switch m.selectedColumn {
	case 0: // backlog
		return "←/→ column   ↑/↓ select   enter detail   p promote   t hold/release   z archive   P switch project   r refresh   q quit"
	case 3: // review
		return "←/→ column   ↑/↓ select   enter detail   a approve   x reject   t hold/release   z archive   P switch project   r refresh   q quit"
	case 4: // approved
		return "←/→ column   ↑/↓ select   enter detail   b bounce   t hold/release   z archive   P switch project   r refresh   q quit"
	case 6: // blocked
		return "←/→ column   ↑/↓ select   enter detail   u unblock   f fail   t hold/release   z archive   P switch project   r refresh   q quit"
	default:
		return "←/→ column   ↑/↓ select   enter detail   t hold/release   z archive   P switch project   r refresh   q quit"
	}
}
