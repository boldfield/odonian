package prwatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/notify"
	"github.com/boldfield/odonian/internal/store"
)

type fakeTaskSource struct {
	projects             []store.Project
	projectsErr          error
	tasks                map[string][]store.Task
	tasksErr             error
	taskWithDepsAndLinks map[string]store.TaskWithDepsAndLinks
	getTaskErrs          map[string]error
	transitionCalls      []transitionCall
	transitionErr        error
	tombstoneLinkCalls   []tombstoneLinkCall
	tombstoneLinkErr     error
}

type tombstoneLinkCall struct {
	taskID string
	linkID string
}

type transitionCall struct {
	taskID  string
	toState string
	note    *string
}

func (f *fakeTaskSource) ListProjects(ctx context.Context, filter store.ProjectListFilter) ([]store.Project, error) {
	return f.projects, f.projectsErr
}

func (f *fakeTaskSource) ListTasks(ctx context.Context, projectID string, filter store.TaskListFilter) ([]store.Task, error) {
	if f.tasksErr != nil {
		return nil, f.tasksErr
	}
	tasks := f.tasks[projectID]
	if filter.State == nil {
		return tasks, nil
	}
	// Mirror the real store's state filtering so tests exercising a specific
	// TaskListFilter.State (e.g. the retrofit pass's "superseded"/"abandoned"
	// queries) don't pick up fixture tasks meant for a different query.
	filtered := make([]store.Task, 0, len(tasks))
	for _, task := range tasks {
		if task.State == *filter.State {
			filtered = append(filtered, task)
		}
	}
	return filtered, nil
}

func (f *fakeTaskSource) GetTask(ctx context.Context, id string) (store.TaskWithDepsAndLinks, error) {
	if err, exists := f.getTaskErrs[id]; exists {
		return store.TaskWithDepsAndLinks{}, err
	}
	return f.taskWithDepsAndLinks[id], nil
}

func (f *fakeTaskSource) TransitionTask(ctx context.Context, taskID, to string, note *string) (store.Task, error) {
	f.transitionCalls = append(f.transitionCalls, transitionCall{
		taskID:  taskID,
		toState: to,
		note:    note,
	})
	return store.Task{}, f.transitionErr
}

func (f *fakeTaskSource) TombstoneLink(ctx context.Context, taskID, linkID string) error {
	f.tombstoneLinkCalls = append(f.tombstoneLinkCalls, tombstoneLinkCall{
		taskID: taskID,
		linkID: linkID,
	})
	return f.tombstoneLinkErr
}

type fakeNotifierForReconciler struct {
	publishCalls []notify.Notification
	publishErr   error
}

func (f *fakeNotifierForReconciler) Publish(ctx context.Context, n notify.Notification) error {
	f.publishCalls = append(f.publishCalls, n)
	return f.publishErr
}

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestReconcilerName(t *testing.T) {
	ts := &fakeTaskSource{}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, nil)

	if reconciler.Name() != "pr-watch" {
		t.Errorf("expected name 'pr-watch', got %q", reconciler.Name())
	}
}

func TestReconcileActionDone(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Test Task",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner/repo/pull/1"},
				},
			},
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var getStateCalled bool
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		getStateCalled = true
		return "merged", nil
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	err := reconciler.Reconcile(ctx)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !getStateCalled {
		t.Fatal("getPRState was not called")
	}

	if len(ts.transitionCalls) != 1 {
		t.Errorf("expected 1 transition call, got %d", len(ts.transitionCalls))
	}

	if ts.transitionCalls[0].taskID != "task-1" || ts.transitionCalls[0].toState != "done" {
		t.Errorf("expected transition to done, got %v", ts.transitionCalls[0])
	}

	if len(notifier.publishCalls) != 1 {
		t.Errorf("expected 1 notification, got %d", len(notifier.publishCalls))
	}

	if notifier.publishCalls[0].Event != "odonian-merged" {
		t.Errorf("expected event 'odonian-merged', got %q", notifier.publishCalls[0].Event)
	}
}

func TestReconcileActionAbandon(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Test Task",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner/repo/pull/1"},
				},
			},
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		return "closed", nil
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "pending", time.Time{}, nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	err := reconciler.Reconcile(ctx)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(ts.transitionCalls) != 1 {
		t.Errorf("expected 1 transition call, got %d", len(ts.transitionCalls))
	}

	call := ts.transitionCalls[0]
	if call.taskID != "task-1" || call.toState != "abandoned" {
		t.Errorf("expected transition to abandoned, got %v", call)
	}

	if call.note == nil || *call.note != "PR closed without merging" {
		t.Errorf("expected note 'PR closed without merging', got %v", call.note)
	}
}

func TestReconcileActionBounce(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Test Task",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner/repo/pull/1"},
				},
			},
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		return "open", nil
	}

	latestReviewAt := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "changes_requested", latestReviewAt, nil
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id": 1}`))
	}))
	defer server.Close()

	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = server.URL
	defer func() {
		forge.GitHubBaseURL = oldBaseURL
	}()

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	err := reconciler.Reconcile(ctx)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(ts.transitionCalls) != 1 {
		t.Errorf("expected 1 transition call, got %d", len(ts.transitionCalls))
	}

	call := ts.transitionCalls[0]
	if call.taskID != "task-1" || call.toState != "ready" {
		t.Errorf("expected transition to ready, got %v", call)
	}

	if call.note == nil || *call.note != "changes requested — bouncing back to ready for rework" {
		t.Errorf("expected note 'changes requested — bouncing back to ready for rework', got %v", call.note)
	}
}

func TestReconcileActionNoop(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Test Task",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner/repo/pull/1"},
				},
			},
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		return "open", nil
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "pending", time.Time{}, nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	err := reconciler.Reconcile(ctx)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(ts.transitionCalls) != 0 {
		t.Errorf("expected 0 transition calls (noop), got %d", len(ts.transitionCalls))
	}

	if len(notifier.publishCalls) != 0 {
		t.Errorf("expected 0 notifications (noop), got %d", len(notifier.publishCalls))
	}
}

func TestReconcileSkipAgentMerge(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: true,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, newTestLogger())
	err := reconciler.Reconcile(ctx)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(ts.transitionCalls) != 0 {
		t.Errorf("expected 0 transition calls (skipped due to AgentMerge), got %d", len(ts.transitionCalls))
	}
}

func TestReconcileSkipNoPRLink(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Test Task",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "branch", Value: "https://github.com/owner/repo"},
				},
			},
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, newTestLogger())
	err := reconciler.Reconcile(ctx)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if len(ts.transitionCalls) != 0 {
		t.Errorf("expected 0 transition calls (skipped due to no PR link), got %d", len(ts.transitionCalls))
	}
}

func TestReconcilePerTaskErrorIsolation(t *testing.T) {
	ctx := context.Background()

	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		return "merged", nil
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
				{
					ID:         "task-2",
					Title:      "Test Task 2",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner/repo/pull/1"},
				},
			},
			"task-2": {
				ID:        "task-2",
				Title:     "Test Task 2",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner/repo/pull/2"},
				},
			},
		},
		getTaskErrs: map[string]error{
			"task-1": errors.New("get task error for task-1"),
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	t.Run("error on task-1 should not affect task-2 processing", func(t *testing.T) {
		err := reconciler.reconcileProject(ctx, "proj-1", make(map[string]int), time.Now())
		if err != nil {
			t.Fatalf("expected no error from reconcileProject, got %v", err)
		}

		if len(ts.transitionCalls) == 0 {
			t.Fatal("expected some transition call from task-2 despite task-1 error")
		}
	})
}

func TestParsePRURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		owner   string
		repo    string
		number  int
		wantErr bool
	}{
		{
			name:   "valid URL",
			url:    "https://github.com/owner/repo/pull/123",
			owner:  "owner",
			repo:   "repo",
			number: 123,
		},
		{
			name:   "valid URL with trailing slash",
			url:    "https://github.com/owner/repo/pull/456/",
			owner:  "owner",
			repo:   "repo",
			number: 456,
		},
		{
			name:    "invalid host",
			url:     "https://gitlab.com/owner/repo/pull/123",
			wantErr: true,
		},
		{
			name:    "invalid path",
			url:     "https://github.com/owner/repo/issues/123",
			wantErr: true,
		},
		{
			name:    "invalid PR number",
			url:     "https://github.com/owner/repo/pull/abc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, number, err := parsePRURL(tt.url)
			if (err != nil) != tt.wantErr {
				t.Errorf("parsePRURL() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if owner != tt.owner || repo != tt.repo || number != tt.number {
					t.Errorf("parsePRURL() = (%q, %q, %d), want (%q, %q, %d)", owner, repo, number, tt.owner, tt.repo, tt.number)
				}
			}
		})
	}
}

// retrofitCalls records which GitHub API calls a fake forge server observed during a
// retrofit pass, for asserting invocation rather than just the absence of an error.
type retrofitCalls struct {
	mu            sync.Mutex
	closeCount    int
	comments      []string
	deletedBranch string
}

// newRetrofitTestServer starts a fake GitHub API server that reports the given PR state
// for GET /pulls/{n} and records comment/close/branch-delete calls. It fails the test on
// any request it doesn't recognize, so a stray real network call can't pass silently.
func newRetrofitTestServer(t *testing.T, prState string) (*httptest.Server, *retrofitCalls) {
	t.Helper()
	calls := &retrofitCalls{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.mu.Lock()
		defer calls.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			w.Header().Set("Content-Type", "application/json")
			switch prState {
			case "merged":
				fmt.Fprint(w, `{"merged_at": "2026-08-01T00:00:00Z", "state": "closed"}`)
			case "closed":
				fmt.Fprint(w, `{"merged_at": null, "state": "closed"}`)
			default:
				fmt.Fprint(w, `{"merged_at": null, "state": "open"}`)
			}
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			calls.comments = append(calls.comments, string(body))
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/pulls/"):
			calls.closeCount++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/git/refs/heads/"):
			idx := strings.Index(r.URL.Path, "/git/refs/heads/")
			calls.deletedBranch = r.URL.Path[idx+len("/git/refs/heads/"):]
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	return server, calls
}

// createTerminalTaskWithPRLink drives a real task through review with a recorded "pr"
// link, then flips it directly to the given terminal state via raw SQL, bypassing
// store.SupersedeTask so the live inline-close path never fires. That keeps this
// fixture isolated to exercising the reconciler's retrofit path on its own.
func createTerminalTaskWithPRLink(t *testing.T, ctx context.Context, st store.Store, projID, docID, prURL, state string, supersededBy *string) store.Task {
	t.Helper()

	tasks, err := st.CreateTasks(ctx, projID, []store.TaskInput{{Title: "Task", Spec: "Spec", DocumentID: docID}})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	task := tasks[0]

	if _, err := st.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", task.ID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err := st.ClaimTask(ctx, task.ID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	links := []store.LinkInput{{Kind: "pr", Value: prURL}}
	if _, err := st.SubmitTask(ctx, task.ID, "agent-1", "Implementation complete", nil, links, 5, nil); err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	if supersededBy != nil {
		if _, err := st.Conn().ExecContext(ctx, "UPDATE task SET state = ?, superseded_by = ? WHERE id = ?", state, *supersededBy, task.ID); err != nil {
			t.Fatalf("failed to force task terminal state: %v", err)
		}
	} else {
		if _, err := st.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", state, task.ID); err != nil {
			t.Fatalf("failed to force task terminal state: %v", err)
		}
	}

	return task
}

func newRealTestStoreWithProject(t *testing.T, ctx context.Context) (store.Store, store.Project, store.Document) {
	t.Helper()

	st, err := store.Open("file::memory:?cache=shared", []string{"haiku", "sonnet", "opus"})
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	proj, err := st.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := st.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	return st, proj, doc
}

// TestRetrofitClosesOpenPRForSupersededTaskThroughRealStore drives the retrofit pass
// through Reconcile -> reconcileProject/retrofitClosePRsForTerminalTasks -> the real
// store's ListTasks, rather than a fake task source, so it catches filter bugs (e.g. a
// missing IncludeSuperseded) that a fake ignoring TaskListFilter would miss.
func TestRetrofitClosesOpenPRForSupersededTaskThroughRealStore(t *testing.T) {
	ctx := context.Background()

	server, calls := newRetrofitTestServer(t, "open")
	defer server.Close()
	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = server.URL
	defer func() { forge.GitHubBaseURL = oldBaseURL }()

	st, proj, doc := newRealTestStoreWithProject(t, ctx)

	replacementID := "11111111-2222-3333-4444-555555555555"
	task := createTerminalTaskWithPRLink(t, ctx, st, proj.ID, doc.ID, "https://github.com/testowner/testrepo/pull/123", "superseded", &replacementID)

	tokenLookup := func(owner string) (string, error) { return "test-token", nil }
	reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, newTestLogger())

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.closeCount != 1 {
		t.Errorf("expected retrofit to close the stale PR exactly once, got %d", calls.closeCount)
	}
	if len(calls.comments) != 1 || !strings.Contains(calls.comments[0], replacementID) {
		t.Errorf("expected retrofit comment to name the replacement task %s, got %v", replacementID, calls.comments)
	}
	wantBranch := "mr/" + task.ID[:8]
	if calls.deletedBranch != wantBranch {
		t.Errorf("expected branch %q to be deleted, got %q", wantBranch, calls.deletedBranch)
	}
}

// TestRetrofitClosesOpenPRForAbandonedTask covers the "by the same argument abandoned"
// half of the spec's terminal-state retrofit requirement.
func TestRetrofitClosesOpenPRForAbandonedTask(t *testing.T) {
	ctx := context.Background()

	server, calls := newRetrofitTestServer(t, "open")
	defer server.Close()
	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = server.URL
	defer func() { forge.GitHubBaseURL = oldBaseURL }()

	st, proj, doc := newRealTestStoreWithProject(t, ctx)

	createTerminalTaskWithPRLink(t, ctx, st, proj.ID, doc.ID, "https://github.com/testowner/testrepo/pull/456", "abandoned", nil)

	tokenLookup := func(owner string) (string, error) { return "test-token", nil }
	reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, newTestLogger())

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.closeCount != 1 {
		t.Errorf("expected retrofit to close the abandoned task's stale PR, got %d close calls", calls.closeCount)
	}
	if len(calls.comments) != 1 || !strings.Contains(calls.comments[0], "abandoned") {
		t.Errorf("expected comment to reference the abandoned state, got %v", calls.comments)
	}
}

// TestRetrofitLeavesDoneTaskMergedPRUntouched covers the spec's other required
// reconciler case: a done task's merged PR is left alone.
func TestRetrofitLeavesDoneTaskMergedPRUntouched(t *testing.T) {
	ctx := context.Background()

	server, calls := newRetrofitTestServer(t, "merged")
	defer server.Close()
	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = server.URL
	defer func() { forge.GitHubBaseURL = oldBaseURL }()

	st, proj, doc := newRealTestStoreWithProject(t, ctx)

	createTerminalTaskWithPRLink(t, ctx, st, proj.ID, doc.ID, "https://github.com/testowner/testrepo/pull/789", "done", nil)

	tokenLookup := func(owner string) (string, error) { return "test-token", nil }
	reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, newTestLogger())

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.closeCount != 0 {
		t.Errorf("expected retrofit not to touch a done task's PR, got %d close calls", calls.closeCount)
	}
	if len(calls.comments) != 0 {
		t.Errorf("expected no comment on a done task's PR, got %v", calls.comments)
	}
}

// TestRetrofitSkipsAlreadyClosedOrMergedSupersededPR proves the retrofit pass, like the
// supersede-time close, never touches a PR that isn't open.
func TestRetrofitSkipsAlreadyClosedOrMergedSupersededPR(t *testing.T) {
	for _, prState := range []string{"merged", "closed"} {
		t.Run(prState, func(t *testing.T) {
			ctx := context.Background()

			server, calls := newRetrofitTestServer(t, prState)
			defer server.Close()
			oldBaseURL := forge.GitHubBaseURL
			forge.GitHubBaseURL = server.URL
			defer func() { forge.GitHubBaseURL = oldBaseURL }()

			st, proj, doc := newRealTestStoreWithProject(t, ctx)

			replacementID := "22222222-3333-4444-5555-666666666666"
			createTerminalTaskWithPRLink(t, ctx, st, proj.ID, doc.ID, "https://github.com/testowner/testrepo/pull/321", "superseded", &replacementID)

			tokenLookup := func(owner string) (string, error) { return "test-token", nil }
			reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, newTestLogger())

			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			calls.mu.Lock()
			defer calls.mu.Unlock()
			if calls.closeCount != 0 {
				t.Errorf("expected retrofit not to close an already-%s PR, got %d close calls", prState, calls.closeCount)
			}
		})
	}
}

func TestSkipsOwnersWithoutForgeToken(t *testing.T) {
	ctx := context.Background()
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Task with token",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
				{
					ID:         "task-2",
					Title:      "Task without token",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Task with token",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner-with-token/repo/pull/1"},
				},
			},
			"task-2": {
				ID:        "task-2",
				Title:     "Task without token",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{Kind: "pr", Value: "https://github.com/owner-without-token/repo/pull/2"},
				},
			},
		},
	}

	notifier := &fakeNotifierForReconciler{}
	var getStateCallCount int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		getStateCallCount++
		if owner != "owner-with-token" {
			t.Errorf("getPRState called for owner %q without token", owner)
		}
		return "merged", nil
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))

	tokenLookup := func(owner string) (string, error) {
		if owner == "owner-with-token" {
			return "test-token", nil
		}
		return "", nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, logger)
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Verify getPRState was called exactly once (only for owner-with-token)
	if getStateCallCount != 1 {
		t.Errorf("expected getPRState to be called 1 time, got %d", getStateCallCount)
	}

	// Verify log contains warning about skipped owner
	logStr := logOutput.String()
	if !strings.Contains(logStr, "no forge token for owner") {
		t.Errorf("expected log to contain 'no forge token for owner', got: %s", logStr)
	}
	if !strings.Contains(logStr, "owner-without-token") {
		t.Errorf("expected log to contain 'owner-without-token', got: %s", logStr)
	}
	if !strings.Contains(logStr, "skipped_pr_checks=1") {
		t.Errorf("expected log to contain 'skipped_pr_checks=1', got: %s", logStr)
	}
}

// Test404TombstonedPRSkippedOnNextPass tests that when a PR returns 404 on the first
// reconcile pass, it is marked as tombstoned and skipped entirely on the second pass,
// making zero GitHub API calls.
func Test404TombstonedPRSkippedOnNextPass(t *testing.T) {
	ctx := context.Background()

	// Track API calls
	var apiCallCount int

	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		apiCallCount++
		// Return 404 error to trigger tombstoning
		return "", fmt.Errorf("status 404")
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	// Set up task source with tombstoning capability
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Test Task",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{
						ID:    "link-1",
						Kind:  "pr",
						Value: "https://github.com/owner/repo/pull/1",
					},
				},
			},
		},
	}

	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, logger)
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	// First pass: should see 404 and tombstone
	err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("expected no error on first reconcile, got %v", err)
	}

	// Verify tombstoning was called
	if len(ts.tombstoneLinkCalls) != 1 {
		t.Errorf("expected 1 tombstone call on first pass, got %d", len(ts.tombstoneLinkCalls))
	} else if ts.tombstoneLinkCalls[0].taskID != "task-1" || ts.tombstoneLinkCalls[0].linkID != "link-1" {
		t.Errorf("expected tombstone call for task-1/link-1, got %v", ts.tombstoneLinkCalls[0])
	}

	// Verify WARN was logged
	logStr := logOutput.String()
	if !strings.Contains(logStr, "PR owner/repo#N gone (404); will not retry") {
		t.Errorf("expected log to contain tombstone warning, got: %s", logStr)
	}

	// Second pass: update the task to have a tombstoned link and reset tracking
	secondPassAPICallCount := 0
	getPRState = func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		secondPassAPICallCount++
		return "open", nil
	}
	reconciler.getPRState = getPRState

	// Update task link to have tombstoned_at set
	ts.taskWithDepsAndLinks["task-1"] = store.TaskWithDepsAndLinks{
		ID:        "task-1",
		Title:     "Test Task",
		State:     "approved",
		UpdatedAt: "2024-01-01T00:00:00Z",
		Links: []store.TaskLink{
			{
				ID:           "link-1",
				Kind:         "pr",
				Value:        "https://github.com/owner/repo/pull/1",
				TombstonedAt: &[]string{"2024-01-01T00:00:00Z"}[0],
			},
		},
	}
	logOutput.Reset()

	// Second pass: should skip the tombstoned PR
	err = reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("expected no error on second reconcile, got %v", err)
	}

	// Verify no API calls were made for the tombstoned PR
	if secondPassAPICallCount > 0 {
		t.Errorf("expected 0 getPRState calls for tombstoned PR on second pass, got %d", secondPassAPICallCount)
	}

	// Verify no additional tombstone calls were made
	if len(ts.tombstoneLinkCalls) != 1 {
		t.Errorf("expected still only 1 tombstone call total, got %d", len(ts.tombstoneLinkCalls))
	}
}

// Test403NotTombstoned tests that a 403 error does NOT trigger tombstoning,
// so the PR is retried on the next pass.
func Test403NotTombstoned(t *testing.T) {
	ctx := context.Background()

	var apiCallCount int

	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		apiCallCount++
		// Return 403 error (should NOT be tombstoned)
		return "", fmt.Errorf("status 403")
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
					Title:      "Test Task",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID:        "task-1",
				Title:     "Test Task",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{
						ID:    "link-1",
						Kind:  "pr",
						Value: "https://github.com/owner/repo/pull/1",
					},
				},
			},
		},
	}

	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, logger)
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Verify NO tombstoning occurred (403 should NOT trigger tombstoning)
	if len(ts.tombstoneLinkCalls) != 0 {
		t.Errorf("expected 0 tombstone calls for 403 error, got %d", len(ts.tombstoneLinkCalls))
	}

	// Verify error was logged but NOT as a tombstone warning
	logStr := logOutput.String()
	if strings.Contains(logStr, "PR owner/repo#N gone (404); will not retry") {
		t.Errorf("expected NO tombstone warning for 403, got: %s", logStr)
	}
	if !strings.Contains(logStr, "get PR state error") {
		t.Errorf("expected error log for 403, got: %s", logStr)
	}
}

// TestRateLimitBackoff tests that when a rate-limit error (403 or 429 with rate-limit body)
// is returned, no further API calls are made for that owner in the current pass, and the
// owner is skipped on subsequent passes until the backoff period expires.
func TestRateLimitBackoff(t *testing.T) {
	ctx := context.Background()

	var apiCallCount int
	apiCallsForOwner := make(map[string]int)

	// Create two tasks for two different owners
	ts := &fakeTaskSource{
		projects: []store.Project{
			{ID: "proj-1"},
		},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-owner1-1",
					Title:      "Test Task for owner1",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
				{
					ID:         "task-owner1-2",
					Title:      "Another Test Task for owner1",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
				{
					ID:         "task-owner2-1",
					Title:      "Test Task for owner2",
					State:      "approved",
					UpdatedAt:  "2024-01-01T00:00:00Z",
					AgentMerge: false,
				},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-owner1-1": {
				ID:        "task-owner1-1",
				Title:     "Test Task for owner1",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{
						ID:    "link-1",
						Kind:  "pr",
						Value: "https://github.com/owner1/repo/pull/1",
					},
				},
			},
			"task-owner1-2": {
				ID:        "task-owner1-2",
				Title:     "Another Test Task for owner1",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{
						ID:    "link-2",
						Kind:  "pr",
						Value: "https://github.com/owner1/repo/pull/2",
					},
				},
			},
			"task-owner2-1": {
				ID:        "task-owner2-1",
				Title:     "Test Task for owner2",
				State:     "approved",
				UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{
					{
						ID:    "link-3",
						Kind:  "pr",
						Value: "https://github.com/owner2/repo/pull/3",
					},
				},
			},
		},
	}

	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		apiCallCount++
		apiCallsForOwner[owner]++
		// First call for owner1 returns rate-limit error, others return success
		if owner == "owner1" && apiCallsForOwner[owner] == 1 {
			return "", fmt.Errorf("API request failed with status 403: API rate limit exceeded for 1.2.3.4")
		}
		if owner == "owner2" {
			return "open", nil
		}
		return "open", nil
	}

	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "pending", time.Time{}, nil
	}

	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, logger)
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	// Use a controllable clock for testing
	mockTime := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	reconciler.now = func() time.Time { return mockTime }

	// First reconcile pass - owner1's first task hits rate limit
	apiCallCount = 0
	apiCallsForOwner = make(map[string]int)
	err := reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// Verify that owner1 only had 1 call (rate limit on first task)
	// and owner2 had 1 call (no rate limit)
	if apiCallsForOwner["owner1"] != 1 {
		t.Errorf("expected 1 API call for owner1, got %d", apiCallsForOwner["owner1"])
	}
	if apiCallsForOwner["owner2"] != 1 {
		t.Errorf("expected 1 API call for owner2, got %d", apiCallsForOwner["owner2"])
	}

	// Verify rate limit warning was logged
	logStr := logOutput.String()
	if !strings.Contains(logStr, "rate limit exceeded, skipping owner") {
		t.Errorf("expected rate limit warning log, got: %s", logStr)
	}

	// Second reconcile pass - at same time, before backoff expires
	// owner1 should still be skipped (backoff not expired yet)
	apiCallCount = 0
	apiCallsForOwner = make(map[string]int)
	err = reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// owner1 should have 0 calls (backed off), owner2 should have 1 call
	if apiCallsForOwner["owner1"] != 0 {
		t.Errorf("expected 0 API calls for owner1 during backoff period, got %d", apiCallsForOwner["owner1"])
	}
	if apiCallsForOwner["owner2"] != 1 {
		t.Errorf("expected 1 API call for owner2, got %d", apiCallsForOwner["owner2"])
	}

	// Third reconcile pass - advance time past backoff period (31 seconds later)
	mockTime = time.Date(2024, 1, 1, 12, 0, 31, 0, time.UTC)
	apiCallCount = 0
	apiCallsForOwner = make(map[string]int)
	err = reconciler.Reconcile(ctx)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	// owner1 should now have 1 call (backoff expired), owner2 should have 1 call
	if apiCallsForOwner["owner1"] != 1 {
		t.Errorf("expected 1 API call for owner1 after backoff expires, got %d", apiCallsForOwner["owner1"])
	}
	if apiCallsForOwner["owner2"] != 1 {
		t.Errorf("expected 1 API call for owner2, got %d", apiCallsForOwner["owner2"])
	}
}
