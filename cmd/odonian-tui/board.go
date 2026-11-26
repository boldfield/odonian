package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	keybind "charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textinput"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/boldfield/odonian/internal/localcommit"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
	"github.com/boldfield/odonian/internal/tuiview"
	"github.com/charmbracelet/x/ansi"
	"github.com/google/uuid"
)

// boardMode identifies the current interaction mode of the board.
// In any mode other than modeNormal, regular navigation keys are suppressed.
type boardMode int

const (
	// modeNormal is the default navigation mode.
	modeNormal boardMode = iota
	// modeDetail is the full-screen task detail view.
	modeDetail
	// modeScorecards is the research reviewer scorecard view.
	modeScorecards
	// modeApproveNote is the optional note input step for an approve action.
	modeApproveNote
	// modeApproveConfirm is the "Approve → done? [y/N]" confirmation step.
	modeApproveConfirm
	// modeRejectReason is the required reason input step for a reject action.
	modeRejectReason
	// modeMergeConfirm is the "Merge PR and complete? [y/N]" confirmation step.
	modeMergeConfirm
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
	// modeSetPriorityReason is the required reason input step for a set priority action.
	modeSetPriorityReason
	// modeSetPriorityInput is the priority value input step for a set priority action.
	modeSetPriorityInput
	// modeSetPriorityConfirm is the confirmation step for a set priority action.
	modeSetPriorityConfirm
	// modeMoveToFrontReason is the required reason input step for a move to front action.
	modeMoveToFrontReason
	// modeMoveToFrontConfirm is the confirmation step for a move to front action.
	modeMoveToFrontConfirm
	// modeResetPriorityReason is the required reason input step for a reset priority action.
	modeResetPriorityReason
	// modeResetPriorityConfirm is the confirmation step for a reset priority action.
	modeResetPriorityConfirm
)

// BoardModel is the Bubble Tea model for the task board view.
type BoardModel struct {
	client         tuiclient.Client
	config         *tuiconfig.Config
	project        tuiclient.Project
	tasks          map[string][]tuiclient.Task // keyed by state
	selectedTaskID string                      // current selection, keyed by ID
	selectedColumn int                         // index into stateOrder
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

	// Priority action state
	pendingPriorityReason string          // captured reason for priority action
	pendingPriority       int64           // captured priority value for set priority action
	lastPriorityAttempt   priorityAttempt // most recent unconfirmed-outcome request, kept so a retry resends the same key
	statusMessage         string          // success text from the last priority action, shown above the board list

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

	// Scorecard view state
	scorecardViewport viewport.Model
	scorecardMessage  string
	scorecardLoading  bool                          // true when a fetch is in progress
	lastScorecards    *tuiclient.ReviewerScorecards // cached for rebuild on resize

	// projects is the cached list of all projects.
	projects []tuiclient.Project

	// shell is the three-pane layout.
	shell *shellState

	// Key bindings and help
	keyBindings  KeyBindings
	helpModel    help.Model
	showFullHelp bool
}

const (
	stateBacklog    = "backlog"
	stateReady      = "ready"
	stateInProgress = "in_progress"
	stateChecks     = "checks"
	stateReview     = "review"
	stateApproved   = "approved"
	stateDone       = "done"
	stateBlocked    = "blocked"
	stateFailed     = "failed"
	stateAbandoned  = "abandoned"
)

var stateOrder = []string{stateBacklog, stateReady, stateInProgress, stateChecks, stateReview, stateApproved, stateDone, stateBlocked, stateFailed, stateAbandoned}

// columnIndex returns the position of state in stateOrder (the board's column order).
func columnIndex(state string) int {
	for i, s := range stateOrder {
		if s == state {
			return i
		}
	}
	return -1
}

// v1InputStyles returns text input styles that match the Bubbles v1 defaults: an unstyled
// prompt and text, a grey placeholder and a cursor with no colour of its own.
func v1InputStyles() textinput.Styles {
	s := textinput.DefaultDarkStyles()
	s.Focused.Prompt = lipgloss.NewStyle()
	s.Blurred.Prompt = lipgloss.NewStyle()
	s.Blurred.Text = lipgloss.NewStyle()
	s.Cursor.Color = nil
	return s
}

// reviewInputView renders the shared text input. Bubbles v2 cuts the placeholder to its
// first character when the input has no width, so while the placeholder shows, render a
// copy sized to it; the placeholder then shows in full with no padding, as in v1.
func (m *BoardModel) reviewInputView() string {
	input := m.reviewInput
	if input.Value() == "" && input.Placeholder != "" {
		input.SetWidth(len([]rune(input.Placeholder)) - 1)
	}
	return input.View()
}

// NewBoardModel creates a board model that renders the three-pane layout.
func NewBoardModel(client tuiclient.Client, config *tuiconfig.Config, project tuiclient.Project) *BoardModel {
	inputWidget := textinput.New()
	inputWidget.CharLimit = 512
	inputWidget.SetStyles(v1InputStyles())

	helpModel := help.New()
	helpModel.ShowAll = false
	helpModel.Ellipsis = "…"

	theme := NewTheme()
	m := &BoardModel{
		client:         client,
		config:         config,
		project:        project,
		tasks:          make(map[string][]tuiclient.Task),
		selectedColumn: columnIndex(stateInProgress), // in_progress by default
		loading:        true,
		reviewInput:    inputWidget,
		urlOpener:      defaultURLOpener,
		ghMerger:       defaultGHMerger,
		keyBindings:    DefaultKeyBindings(),
		helpModel:      helpModel,
		showFullHelp:   false,
		shell: &shellState{
			theme:           theme,
			projects:        NewProjectsPane(theme, []tuiclient.Project{project}),
			items:           NewWorkItemsPane(theme),
			preview:         NewPreviewPane(nil, 0, 0),
			focus:           paneItems,
			previewDebounce: previewDebounce,
		},
	}
	m.shell.preview.SetTheme(theme)
	m.shell.filter = stateFilterFromConfig(config)
	m.shell.items.SetFilter(m.shell.filter)
	m.shell.items.SetProjectName(project.Name)
	m.newTickCmd = m.defaultTickCmd
	return m
}

// stateFilterFromConfig builds the startup state filter: the config's hidden_states when
// set, otherwise the default. Ignored names are reported once on stderr.
func stateFilterFromConfig(config *tuiconfig.Config) tuiview.StateFilter {
	if config == nil || !config.HiddenStatesSet {
		return tuiview.DefaultStateFilter()
	}
	filter, ignored := tuiview.ParseHiddenStates(config.HiddenStates)
	if len(ignored) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: hidden_states: ignoring unknown state names: %s\n", strings.Join(ignored, ", "))
	}
	return filter
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
	return tea.Batch(m.fetchShellTasks(), m.fetchProjects(), m.newTickCmd())
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

// reviewActionMsg is returned when a review action (approve/reject/priority) completes.
// It carries either a successful refetch (tasks != nil) or an error string.
// fromDetail is true when the action was initiated from the full-screen detail view;
// the handler uses this to return to modeNormal (the board) so the result is visible.
type reviewActionMsg struct {
	// tasks is non-nil on success; it holds the refreshed board data.
	tasks         map[string][]tuiclient.Task
	err           string
	refetchFailed bool // true when err is a failed task refetch after the action
	fromDetail    bool
	actionSuccess bool   // true only when the priority action itself succeeded
	status        string // success text shown above the refreshed list (not an error)
	// actionRejected is true when the server definitively refused the priority action
	// (4xx other than timeout/rate-limit), so its idempotency key cannot be retried.
	actionRejected bool
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
	// fromShell marks a resolution requested from the three-pane layout rather than the detail view.
	fromShell bool
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

		// In local_commit mode approving must also land the task's work on its MR branch, which
		// only `odonian approve` does. Marking the task done here would unblock its dependents
		// onto a branch that lacks its code, so refuse before recording anything.
		if localcommit.IsLocalCommit() {
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("local_commit: approve with `odonian approve %s` so its work lands on its branch; the TUI cannot do that", taskID))
			msg.fromDetail = fromDetail
			return msg
		}

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

// setTaskPriorityCmd creates a command that sets a task's priority.
func (m *BoardModel) setTaskPriorityCmd(taskID string, priority int64, reason string, actionKey string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := tuiclient.SetTaskPriorityRequest{
			ActionKey: actionKey,
			Priority:  priority,
			Actor:     "tui-worker",
			Reason:    reason,
		}

		change, err := m.client.SetTaskPriority(ctx, taskID, req)
		if err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("set priority %d: %s", apiErr.StatusCode, apiErr.Message))
				msg.fromDetail = fromDetail
				msg.actionSuccess = false
				msg.actionRejected = isDefinitiveRejection(apiErr.StatusCode)
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("set priority failed: %v", err))
			msg.fromDetail = fromDetail
			msg.actionSuccess = false
			return msg
		}

		statusMsg := fmt.Sprintf("priority set to %d for %s", change.Priority, change.TopicAnchorID)
		msg := m.fetchTasksInline(ctx, "")
		msg.status = statusMsg
		msg.fromDetail = fromDetail
		msg.actionSuccess = true
		return msg
	}
}

// moveTaskToFrontCmd creates a command that moves a task to the front of the queue.
func (m *BoardModel) moveTaskToFrontCmd(taskID string, reason string, actionKey string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := tuiclient.MoveTaskToFrontRequest{
			ActionKey: actionKey,
			Actor:     "tui-worker",
			Reason:    reason,
		}

		change, err := m.client.MoveTaskToFront(ctx, taskID, req)
		if err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("move to front %d: %s", apiErr.StatusCode, apiErr.Message))
				msg.fromDetail = fromDetail
				msg.actionSuccess = false
				msg.actionRejected = isDefinitiveRejection(apiErr.StatusCode)
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("move to front failed: %v", err))
			msg.fromDetail = fromDetail
			msg.actionSuccess = false
			return msg
		}

		statusMsg := fmt.Sprintf("moved to front with priority %d for %s", change.Priority, change.TopicAnchorID)
		msg := m.fetchTasksInline(ctx, "")
		msg.status = statusMsg
		msg.fromDetail = fromDetail
		msg.actionSuccess = true
		return msg
	}
}

// resetTaskPriorityCmd creates a command that resets a task's priority to 500.
func (m *BoardModel) resetTaskPriorityCmd(taskID string, reason string, actionKey string, fromDetail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		req := tuiclient.SetTaskPriorityRequest{
			ActionKey: actionKey,
			Priority:  500,
			Actor:     "tui-worker",
			Reason:    reason,
		}

		change, err := m.client.SetTaskPriority(ctx, taskID, req)
		if err != nil {
			var apiErr *tuiclient.APIError
			if errors.As(err, &apiErr) {
				msg := m.fetchTasksInline(ctx, fmt.Sprintf("reset priority %d: %s", apiErr.StatusCode, apiErr.Message))
				msg.fromDetail = fromDetail
				msg.actionSuccess = false
				msg.actionRejected = isDefinitiveRejection(apiErr.StatusCode)
				return msg
			}
			msg := m.fetchTasksInline(ctx, fmt.Sprintf("reset priority failed: %v", err))
			msg.fromDetail = fromDetail
			msg.actionSuccess = false
			return msg
		}

		statusMsg := fmt.Sprintf("priority reset to %d for %s", change.Priority, change.TopicAnchorID)
		msg := m.fetchTasksInline(ctx, "")
		msg.status = statusMsg
		msg.fromDetail = fromDetail
		msg.actionSuccess = true
		return msg
	}
}

// isDefinitiveRejection reports whether an HTTP status means the server refused the request
// itself (so its idempotency key can never succeed on retry), as opposed to a timeout or
// rate limit where an identical retry is still meaningful.
func isDefinitiveRejection(statusCode int) bool {
	return statusCode >= 400 && statusCode < 500 &&
		statusCode != http.StatusRequestTimeout && statusCode != http.StatusTooManyRequests
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
		return reviewActionMsg{err: errMsg, refetchFailed: true}
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
var activeStates = []string{stateBacklog, stateReady, stateInProgress, stateChecks, stateReview, stateApproved, stateBlocked}

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

	// Sort each state's tasks by priority (descending), then created_at (ascending), then ID (ascending)
	for _, taskList := range bucketed {
		sortTasksByPriority(taskList)
	}

	return bucketed
}

// sortTasksByPriority orders tasks within a column by numeric priority (descending),
// then by created_at (ascending) for ties, then by ID (ascending) for final tie-breaking.
func sortTasksByPriority(tasks []tuiclient.Task) {
	sort.Slice(tasks, func(i, j int) bool {
		// Get priority values (default 500 if nil)
		priI := int64(500)
		if tasks[i].Priority != nil {
			priI = *tasks[i].Priority
		}
		priJ := int64(500)
		if tasks[j].Priority != nil {
			priJ = *tasks[j].Priority
		}

		// Sort by priority descending (higher priority first)
		if priI != priJ {
			return priI > priJ
		}

		// Tie-break by created_at ascending (older first)
		if createdBefore, differs := compareCreatedAt(tasks[i].CreatedAt, tasks[j].CreatedAt); differs {
			return createdBefore
		}

		// Final tie-break by ID ascending
		return tasks[i].ID < tasks[j].ID
	})
}

// compareCreatedAt compares RFC3339 timestamps chronologically (raw string order is wrong when
// fractional-second precision differs). Unparseable values fall back to string comparison.
func compareCreatedAt(a, b string) (aBeforeB bool, differs bool) {
	timeA, errA := time.Parse(time.RFC3339Nano, a)
	timeB, errB := time.Parse(time.RFC3339Nano, b)
	if errA == nil && errB == nil {
		return timeA.Before(timeB), !timeA.Equal(timeB)
	}
	return a < b, a != b
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
	return m.updateShell(msg)
}

// updateShared handles the messages and the keys of the modes the shell does not handle itself.
func (m *BoardModel) updateShared(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.helpModel.SetWidth(m.width)
		// Reinitialize the detail viewport if we're in detail mode.
		if m.mode == modeDetail {
			m.initDetailViewport(m.detailTask)
		}
		// Reinitialize the scorecard viewport if we're in scorecard mode.
		if m.mode == modeScorecards && m.lastScorecards != nil {
			m.initScorecardViewport(*m.lastScorecards)
		}
		return m, nil

	case tea.PasteMsg:
		// Bubble Tea v2 delivers a bracketed paste as its own message rather than as a key.
		// Pasted text goes only to the modes that type into the text input.
		m.statusMessage = ""
		switch m.mode {
		case modeApproveNote, modeRejectReason, modeApprovedRejectNote, modeUnblockNote, modeFailNote,
			modeSetPriorityInput, modeSetPriorityReason, modeMoveToFrontReason, modeResetPriorityReason:
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.KeyPressMsg:
		m.statusMessage = ""

		// Detail mode: all keys go to the detail handler — board nav must not fire.
		if m.mode == modeDetail {
			return m.updateDetailMode(msg)
		}

		// Scorecard mode: all keys go to the scorecard handler.
		if m.mode == modeScorecards {
			return m.updateScorecardMode(msg)
		}

		// When a text-input or confirm mode is active, route keys to the review flow
		// instead of the normal navigation handlers.
		if m.mode != modeNormal {
			return m.updateReviewMode(msg)
		}

		return m, nil

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

	case scorecardFetchedMsg:
		m.scorecardLoading = false
		if msg.err != nil {
			// Stay in scorecard mode but show error message.
			m.scorecardMessage = fmt.Sprintf("Error: %v", msg.err)
			m.scorecardViewport.SetContent("")
			m.lastScorecards = nil
			return m, nil
		}
		// Initialize the viewport with the scorecard content.
		m.initScorecardViewport(msg.scorecards)
		m.scorecardMessage = ""
		return m, nil

	case openerResultMsg:
		m.detailMessage = msg.message
		return m, nil

	case prResolvedMsg:
		fromDetail := m.mode == modeDetail
		if msg.err != "" {
			if fromDetail {
				m.detailMessage = msg.err
			} else {
				m.error = msg.err
			}
			return m, nil
		}
		// A shell merge resolves asynchronously; drop the answer if the user has moved on.
		if msg.fromShell && (m.mode != modeNormal || m.shell.items.SelectedTaskID() != msg.taskID) {
			return m, nil
		}
		if fromDetail && m.detailTask.ID != msg.taskID {
			return m, nil
		}
		if !fromDetail && m.mode != modeNormal {
			return m, nil
		}
		// Enter merge confirm mode with the resolved PR.
		m.pendingTaskID = msg.taskID
		m.pendingPRURL = msg.prURL
		m.reviewFromDetail = fromDetail
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
		if msg.tasks != nil {
			m.loading = false
			m.tasks = msg.tasks
			m.lastRefresh = time.Now()
		}
		if msg.err != "" {
			m.error = msg.err
		} else {
			m.error = ""
		}
		m.statusMessage = msg.status
		// Keep the attempt (and its key) across retryable failures so an identical
		// retry replays rather than double-applies; drop it on success or when the
		// server rejected the request outright.
		if msg.actionSuccess || msg.actionRejected {
			m.clearPriorityActionState()
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

		// Just update data; do NOT arm a tick. The single tick chain in tickMsg handles
		// re-arming itself.
		return m, nil

	case projectsFetchedMsg:
		if msg.err != nil {
			m.error = fmt.Sprintf("Error fetching projects: %v", msg.err)
			return m, nil
		}
		m.projects = msg.projects
		return m, nil

	case tickMsg:
		return m, tea.Batch(m.fetchShellTasks(), m.newTickCmd())
	}

	return m, nil
}

// boardAction runs the task actions, the refresh and the scorecards on the shell's selected task.
func (m *BoardModel) boardAction(act action) (tea.Model, tea.Cmd) {
	switch act {
	// Refresh: issue one-shot fetch of all columns (active and terminal); do NOT arm
	// a new tick. The single perpetual tick chain started in Init re-arms itself from
	// tickMsg.
	case actRefresh:
		return m, m.fetchTasksFullRefresh()

	// Promote: only on backlog tasks
	case actPromote:
		if m.selectedColumn == 0 && m.selectedTaskID != "" {
			return m, m.promoteTask(m.selectedTaskID)
		}

	// Approve: only on review column tasks
	case actApprove:
		if m.selectedColumn == columnIndex(stateReview) && m.selectedTaskID != "" {
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
	case actReject:
		if m.selectedColumn == columnIndex(stateReview) && m.selectedTaskID != "" {
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
	case actBounce:
		if m.selectedColumn == columnIndex(stateApproved) && m.selectedTaskID != "" {
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
	case actUnblock:
		if m.selectedColumn == columnIndex(stateBlocked) && m.selectedTaskID != "" {
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

	case actFail:
		if m.selectedColumn == columnIndex(stateBlocked) && m.selectedTaskID != "" {
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
	case actArchive:
		if m.selectedTaskID != "" {
			m.pendingTaskID = m.selectedTaskID
			m.mode = modeArchiveTaskConfirm
			return m, nil
		}

	// Hold/Release: hold or release the selected task
	case actHold:
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

	// Set Priority: set priority for the selected task
	case actSetPriority:
		if m.selectedTaskID != "" {
			m.pendingTaskID = m.selectedTaskID
			m.reviewInput.Placeholder = "priority (1-1000)"
			m.reviewInput.SetValue("")
			m.reviewInput.Focus()
			m.inputHint = ""
			m.mode = modeSetPriorityInput
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(nil)
			return m, cmd
		}

	// Move to Front: move selected task to front (server-calculated priority)
	case actMoveToFront:
		if m.selectedTaskID != "" {
			m.pendingTaskID = m.selectedTaskID
			m.reviewInput.Placeholder = "reason for moving to front (required)"
			m.reviewInput.SetValue("")
			m.reviewInput.Focus()
			m.inputHint = ""
			m.mode = modeMoveToFrontReason
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(nil)
			return m, cmd
		}

	// Reset Priority: reset selected task priority to 500
	case actResetPriority:
		if m.selectedTaskID != "" {
			m.pendingTaskID = m.selectedTaskID
			m.reviewInput.Placeholder = "reason for resetting priority (required)"
			m.reviewInput.SetValue("")
			m.reviewInput.Focus()
			m.inputHint = ""
			m.mode = modeResetPriorityReason
			var cmd tea.Cmd
			m.reviewInput, cmd = m.reviewInput.Update(nil)
			return m, cmd
		}

	// Scorecards: show research reviewer scorecards
	case actScorecards:
		m.mode = modeScorecards
		m.scorecardMessage = ""
		m.scorecardViewport.SetContent("")
		m.lastScorecards = nil
		m.scorecardLoading = true
		return m, m.fetchScorecardCmd()

	}
	return m, nil
}

// updateDetailMode handles key events while the full-screen detail view is active.
// Board navigation is fully suppressed in this mode.
func (m *BoardModel) updateDetailMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	act, _ := m.keyBindings.Detail.lookup(msg, m.detailKeyCtx())
	// While the full key list is open it takes the keys: ? closes it, the rest are swallowed.
	if m.showFullHelp {
		if act == actHelp {
			m.showFullHelp = false
		}
		return m, nil
	}

	switch act {
	case actBack:
		// Return to the board.
		m.mode = modeNormal
		m.detailMessage = ""
		return m, nil

	// Spec viewport scrolling.
	case actUp:
		m.detailViewport.ScrollUp(1)
		return m, nil
	case actDown:
		m.detailViewport.ScrollDown(1)
		return m, nil
	case actPageUp:
		m.detailViewport.HalfPageUp()
		return m, nil
	case actPageDown:
		m.detailViewport.HalfPageDown()
		return m, nil

	// Open PR link.
	case actOpenPR:
		return m, m.openPRCmd(m.detailTask)

	// Open the task's source document.
	case actSourceDoc:
		return m, m.openSourceDocCmd(m.detailTask, m.detailDocuments)

	// Open the project's base design document.
	case actDesignDoc:
		return m, m.openDesignDocCmd(m.detailDocuments)

	// Help toggle
	case actHelp:
		m.showFullHelp = true
		return m, nil

	// Approve / reject: only available for review tasks; reuse existing review flow.
	case actApprove:
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

	case actReject:
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
	case actMerge:
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

	// Set Priority: set priority for the task (detail view)
	case actSetPriority:
		m.pendingTaskID = m.detailTask.ID
		m.reviewInput.Placeholder = "priority (1-1000)"
		m.reviewInput.SetValue("")
		m.reviewInput.Focus()
		m.inputHint = ""
		m.mode = modeSetPriorityInput
		m.reviewFromDetail = true
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(nil)
		return m, cmd

	// Move to Front: move task to front (server-calculated priority, detail view)
	case actMoveToFront:
		m.pendingTaskID = m.detailTask.ID
		m.reviewInput.Placeholder = "reason for moving to front (required)"
		m.reviewInput.SetValue("")
		m.reviewInput.Focus()
		m.inputHint = ""
		m.mode = modeMoveToFrontReason
		m.reviewFromDetail = true
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(nil)
		return m, cmd

	// Reset Priority: reset task priority to 500 (detail view)
	case actResetPriority:
		m.pendingTaskID = m.detailTask.ID
		m.reviewInput.Placeholder = "reason for resetting priority (required)"
		m.reviewInput.SetValue("")
		m.reviewInput.Focus()
		m.inputHint = ""
		m.mode = modeResetPriorityReason
		m.reviewFromDetail = true
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(nil)
		return m, cmd

	}

	return m, nil
}

// updateReviewMode handles key events when the model is in one of the review input modes.
// It returns the updated model and any command to run. Normal navigation is suppressed.
func (m *BoardModel) updateReviewMode(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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

	case modeSetPriorityInput:
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(msg)
		switch msg.String() {
		case "esc":
			// Cancel priority input
			m.clearPriorityActionState()
			m.cancelReviewMode()
			return m, nil
		case "enter":
			// Validate and move to reason input
			input := m.reviewInput.Value()
			if input == "" {
				m.inputHint = "priority value required"
				return m, nil
			}
			priority, err := strconv.ParseInt(input, 10, 64)
			if err != nil || priority < 1 || priority > 1000 {
				m.inputHint = "priority must be an integer between 1 and 1000"
				return m, nil
			}
			m.pendingPriority = priority
			// Move to reason input
			m.reviewInput.Placeholder = "reason for priority change (required)"
			m.reviewInput.SetValue("")
			m.inputHint = ""
			m.mode = modeSetPriorityReason
			m.reviewInput.Focus()
			return m, nil
		}
		return m, cmd

	case modeSetPriorityReason:
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(msg)
		switch msg.String() {
		case "esc":
			// Cancel priority change
			m.clearPriorityActionState()
			m.cancelReviewMode()
			return m, nil
		case "enter":
			// Capture reason and show confirmation
			reason := m.reviewInput.Value()
			if reason == "" {
				m.inputHint = "reason required"
				return m, nil
			}
			m.pendingPriorityReason = reason
			m.mode = modeSetPriorityConfirm
			m.inputHint = ""
			return m, nil
		}
		return m, cmd

	case modeSetPriorityConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			m.clearPriorityActionState()
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			taskID := m.pendingTaskID
			priority := m.pendingPriority
			reason := m.pendingPriorityReason
			actionKey := m.priorityActionKey(taskID, priorityActionSet, priority, reason)
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.setTaskPriorityCmd(taskID, priority, reason, actionKey, originFromDetail)
		}
		return m, nil

	case modeMoveToFrontReason:
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(msg)
		switch msg.String() {
		case "esc":
			m.clearPriorityActionState()
			m.cancelReviewMode()
			return m, nil
		case "enter":
			reason := m.reviewInput.Value()
			if reason == "" {
				m.inputHint = "reason required"
				return m, nil
			}
			m.pendingPriorityReason = reason
			m.mode = modeMoveToFrontConfirm
			m.inputHint = ""
			return m, nil
		}
		return m, cmd

	case modeMoveToFrontConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			m.clearPriorityActionState()
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			taskID := m.pendingTaskID
			reason := m.pendingPriorityReason
			actionKey := m.priorityActionKey(taskID, priorityActionFront, 0, reason)
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.moveTaskToFrontCmd(taskID, reason, actionKey, originFromDetail)
		}
		return m, nil

	case modeResetPriorityReason:
		var cmd tea.Cmd
		m.reviewInput, cmd = m.reviewInput.Update(msg)
		switch msg.String() {
		case "esc":
			m.clearPriorityActionState()
			m.cancelReviewMode()
			return m, nil
		case "enter":
			reason := m.reviewInput.Value()
			if reason == "" {
				m.inputHint = "reason required"
				return m, nil
			}
			m.pendingPriorityReason = reason
			m.mode = modeResetPriorityConfirm
			m.inputHint = ""
			return m, nil
		}
		return m, cmd

	case modeResetPriorityConfirm:
		switch msg.String() {
		case "esc", "n", "N":
			m.clearPriorityActionState()
			m.cancelReviewMode()
			return m, nil
		case "y", "Y":
			taskID := m.pendingTaskID
			reason := m.pendingPriorityReason
			actionKey := m.priorityActionKey(taskID, priorityActionReset, 500, reason)
			originFromDetail := m.reviewFromDetail
			m.cancelReviewMode()
			return m, m.resetTaskPriorityCmd(taskID, reason, actionKey, originFromDetail)
		}
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
	// Priority attempt state is intentionally left alone: confirm handlers call this
	// before the command runs, and a failed attempt must survive for an identical retry.
	// Explicit cancels call clearPriorityActionState.
}

// clearPriorityActionState drops captured priority input and any remembered attempt.
func (m *BoardModel) clearPriorityActionState() {
	m.lastPriorityAttempt = priorityAttempt{}
	m.pendingPriority = 0
	m.pendingPriorityReason = ""
}

const (
	priorityActionSet   = "set"
	priorityActionFront = "front"
	priorityActionReset = "reset"
)

// priorityAttempt identifies one priority request by everything the server hashes into
// its idempotency check (action, task, value, reason) plus the key sent for it.
type priorityAttempt struct {
	key      string
	taskID   string
	action   string
	priority int64
	reason   string
}

// priorityActionKey returns the idempotency key for the request. An identical repeat of the
// last attempt reuses its key (server replays instead of re-applying); any difference mints
// a fresh key because the server would otherwise answer IDEMPOTENCY_MISMATCH.
func (m *BoardModel) priorityActionKey(taskID, action string, priority int64, reason string) string {
	attempt := m.lastPriorityAttempt
	if attempt.key != "" && attempt.taskID == taskID && attempt.action == action &&
		attempt.priority == priority && attempt.reason == reason {
		return attempt.key
	}
	m.lastPriorityAttempt = priorityAttempt{
		key:      fmt.Sprintf("odonian-tui-%s", uuid.New().String()),
		taskID:   taskID,
		action:   action,
		priority: priority,
		reason:   reason,
	}
	return m.lastPriorityAttempt.key
}

// View renders the board in the alternate screen buffer.
func (m *BoardModel) View() tea.View {
	v := tea.NewView(m.viewContent())
	v.AltScreen = true
	return v
}

// viewContent renders the board (or the detail view if in modeDetail).
func (m *BoardModel) viewContent() string {
	if m.width < 40 {
		return "Terminal too narrow. Please resize."
	}

	// Full help view
	if m.showFullHelp {
		var b strings.Builder
		b.WriteString(m.renderFullHelp())
		b.WriteString("\n")
		b.WriteString(strings.Repeat("─", m.width))
		b.WriteString("\n")
		b.WriteString("? to close")
		return b.String()
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

	if m.mode != modeScorecards {
		return m.shellView()
	}

	// Full-screen scorecard view.
	var b strings.Builder
	b.WriteString(m.renderScorecardView())
	b.WriteString("\n")
	b.WriteString(strings.Repeat("─", m.width))
	b.WriteString("\n")
	b.WriteString(m.renderScorecardHelpBar())
	return b.String()
}

// renderReviewOverlay renders the text input or confirm prompt used during review actions.
func (m *BoardModel) renderReviewOverlay() string {
	var b strings.Builder
	switch m.mode {
	case modeApproveNote:
		b.WriteString("Approve — add an optional note:\n")
		b.WriteString(m.reviewInputView())
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
		b.WriteString(m.reviewInputView())
		b.WriteString("\n")
		if m.inputHint != "" {
			b.WriteString("hint: " + m.inputHint + "\n")
		}
		b.WriteString("(enter to submit, esc to cancel)\n")
	case modeMergeConfirm:
		b.WriteString(m.reviewInputView())
		b.WriteString("\n")
		b.WriteString("(y to confirm merge and complete, n/esc to cancel)\n")
	case modeApprovedRejectNote:
		b.WriteString("Bounce back to ready — optional note:\n")
		b.WriteString(m.reviewInputView())
		b.WriteString("\n")
		b.WriteString("(enter to continue, esc to cancel)\n")
	case modeApprovedRejectConfirm:
		b.WriteString("Send back to ready? [y/N] ")
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeUnblockNote:
		b.WriteString("Unblock — add an optional note:\n")
		b.WriteString(m.reviewInputView())
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
		b.WriteString(m.reviewInputView())
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
	case modeSetPriorityInput:
		b.WriteString("Set priority — enter a value (1-1000):\n")
		b.WriteString(m.reviewInputView())
		b.WriteString("\n")
		if m.inputHint != "" {
			b.WriteString("hint: " + m.inputHint + "\n")
		}
		b.WriteString("(enter to continue, esc to cancel)\n")
	case modeSetPriorityReason:
		b.WriteString("Set priority — reason (required):\n")
		b.WriteString(m.reviewInputView())
		b.WriteString("\n")
		if m.inputHint != "" {
			b.WriteString("hint: " + m.inputHint + "\n")
		}
		b.WriteString("(enter to submit, esc to cancel)\n")
	case modeSetPriorityConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Set priority to %d for %s? [y/N] ", m.pendingPriority, taskID))
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeMoveToFrontReason:
		b.WriteString("Move to front — reason (required):\n")
		b.WriteString(m.reviewInputView())
		b.WriteString("\n")
		if m.inputHint != "" {
			b.WriteString("hint: " + m.inputHint + "\n")
		}
		b.WriteString("(enter to submit, esc to cancel)\n")
	case modeMoveToFrontConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Move %s to front? [y/N] ", taskID))
		b.WriteString("(server will assign priority > all current priorities)\n")
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeResetPriorityReason:
		b.WriteString("Reset priority to 500 — reason (required):\n")
		b.WriteString(m.reviewInputView())
		b.WriteString("\n")
		if m.inputHint != "" {
			b.WriteString("hint: " + m.inputHint + "\n")
		}
		b.WriteString("(enter to submit, esc to cancel)\n")
	case modeResetPriorityConfirm:
		taskID := m.pendingTaskID
		if len(taskID) > 8 {
			taskID = taskID[:8]
		}
		b.WriteString(fmt.Sprintf("Reset priority to 500 for %s? [y/N] ", taskID))
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	case modeArchiveProjectConfirm:
		b.WriteString("Archive project? [y/N] ")
		b.WriteString("(y to confirm, n/esc to cancel)\n")
	}
	return b.String()
}

// helpLine renders bindings as one help line no wider than width (0 means no limit),
// truncating with an ellipsis instead of overflowing.
func (m *BoardModel) helpLine(bindings []keybind.Binding, width int) string {
	m.helpModel.SetWidth(width)
	m.helpModel.ShowAll = false
	line := ansi.Strip(m.helpModel.ShortHelpView(bindings))
	if width > 0 {
		line = ansi.Truncate(line, width, "…")
	}
	return line
}

func (m *BoardModel) detailKeyCtx() keyCtx { return keyCtx{state: m.detailTask.State} }

// helpBindings is the full list of bindings live in the current view.
func (m *BoardModel) helpBindings() []keybind.Binding {
	if m.mode == modeDetail {
		return m.keyBindings.Detail.help(m.detailKeyCtx())
	}
	return m.keyBindings.Shell.help(m.shellKeyCtx())
}

// renderDetailHelpBar renders the help bar for the detail view.
func (m *BoardModel) renderDetailHelpBar() string {
	return m.helpLine(m.helpBindings(), m.width)
}

// renderFullHelp renders every binding live in the current view, one per row, in columns.
func (m *BoardModel) renderFullHelp() string {
	const perColumn = 8
	bindings := m.helpBindings()
	var groups [][]keybind.Binding
	for len(bindings) > perColumn {
		groups = append(groups, bindings[:perColumn])
		bindings = bindings[perColumn:]
	}
	groups = append(groups, bindings)
	m.helpModel.SetWidth(m.width)
	return ansi.Strip(m.helpModel.FullHelpView(groups))
}
