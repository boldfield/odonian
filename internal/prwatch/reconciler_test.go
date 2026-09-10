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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, nil)

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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	t.Run("error on task-1 should not affect task-2 processing", func(t *testing.T) {
		err := reconciler.reconcileProject(ctx, "proj-1", make(map[string]int), make(map[string]bool), time.Now())
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
	getStateCount int
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
			calls.getStateCount++
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
	reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, time.Minute, 0, newTestLogger())

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	calls.mu.Lock()
	if calls.closeCount != 1 {
		calls.mu.Unlock()
		t.Errorf("expected retrofit to close the stale PR exactly once, got %d", calls.closeCount)
	}
	if len(calls.comments) != 1 || !strings.Contains(calls.comments[0], replacementID) {
		calls.mu.Unlock()
		t.Errorf("expected retrofit comment to name the replacement task %s, got %v", replacementID, calls.comments)
	}
	wantBranch := "mr/" + task.ID[:8]
	if calls.deletedBranch != wantBranch {
		calls.mu.Unlock()
		t.Errorf("expected branch %q to be deleted, got %q", wantBranch, calls.deletedBranch)
	}
	callCountAfterFirstPass := calls.getStateCount
	calls.mu.Unlock()

	// Verify link is tombstoned after close
	fullTask, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	var prLink *store.TaskLink
	for i := range fullTask.Links {
		if fullTask.Links[i].Kind == "pr" {
			prLink = &fullTask.Links[i]
			break
		}
	}
	if prLink == nil {
		t.Fatalf("expected task to have a PR link")
	}
	if prLink.TombstonedAt == nil {
		t.Errorf("expected link to be tombstoned after close")
	}

	// Run second pass and verify zero GitHub calls
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.getStateCount != callCountAfterFirstPass {
		t.Errorf("expected no GitHub calls on second pass after tombstoning, but got %d calls (was %d)", calls.getStateCount, callCountAfterFirstPass)
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
	reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, time.Minute, 0, newTestLogger())

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
	reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, time.Minute, 0, newTestLogger())

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

// TestRetrofitDoesNotTombstoneLinkIfClosePRFails verifies that when the close PR call fails,
// the link is NOT tombstoned and subsequent passes will retry the GitHub call.
func TestRetrofitDoesNotTombstoneLinkIfClosePRFails(t *testing.T) {
	ctx := context.Background()

	// Create a test server that fails on close (PATCH) calls
	calls := &retrofitCalls{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.mu.Lock()
		defer calls.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/pulls/"):
			calls.getStateCount++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"merged_at": null, "state": "open"}`)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/comments"):
			body, _ := io.ReadAll(r.Body)
			calls.comments = append(calls.comments, string(body))
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/pulls/"):
			calls.closeCount++
			// Fail the close call with a non-2xx status
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/git/refs/heads/"):
			idx := strings.Index(r.URL.Path, "/git/refs/heads/")
			calls.deletedBranch = r.URL.Path[idx+len("/git/refs/heads/"):]
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = server.URL
	defer func() { forge.GitHubBaseURL = oldBaseURL }()

	st, proj, doc := newRealTestStoreWithProject(t, ctx)

	replacementID := "33333333-4444-5555-6666-777777777777"
	task := createTerminalTaskWithPRLink(t, ctx, st, proj.ID, doc.ID, "https://github.com/testowner/testrepo/pull/567", "superseded", &replacementID)

	tokenLookup := func(owner string) (string, error) { return "test-token", nil }
	reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, time.Minute, 0, newTestLogger())

	// First pass: close fails, so link should NOT be tombstoned
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	calls.mu.Lock()
	if calls.closeCount != 1 {
		calls.mu.Unlock()
		t.Errorf("expected one close attempt, got %d", calls.closeCount)
	}
	callCountAfterFirstPass := calls.getStateCount
	calls.mu.Unlock()

	// Verify link is NOT tombstoned after failed close
	fullTask, err := st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	var prLink *store.TaskLink
	for i := range fullTask.Links {
		if fullTask.Links[i].Kind == "pr" {
			prLink = &fullTask.Links[i]
			break
		}
	}
	if prLink == nil {
		t.Fatalf("expected task to have a PR link")
	}
	if prLink.TombstonedAt != nil {
		t.Errorf("expected link to NOT be tombstoned after close failure")
	}

	// Run second pass and verify GitHub is called again
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.getStateCount == callCountAfterFirstPass {
		t.Errorf("expected GitHub calls on second pass since link wasn't tombstoned, but call count stayed at %d", callCountAfterFirstPass)
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
			task := createTerminalTaskWithPRLink(t, ctx, st, proj.ID, doc.ID, "https://github.com/testowner/testrepo/pull/321", "superseded", &replacementID)

			tokenLookup := func(owner string) (string, error) { return "test-token", nil }
			reconciler := NewPRWatchReconciler(st, &fakeNotifierForReconciler{}, tokenLookup, time.Minute, 0, newTestLogger())

			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			// Verify link is tombstoned after first pass
			fullTask, err := st.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("failed to get task: %v", err)
			}
			var prLink *store.TaskLink
			for i := range fullTask.Links {
				if fullTask.Links[i].Kind == "pr" {
					prLink = &fullTask.Links[i]
					break
				}
			}
			if prLink == nil {
				t.Fatalf("expected task to have a PR link")
			}
			if prLink.TombstonedAt == nil {
				t.Errorf("expected link to be tombstoned after first pass")
			}

			calls.mu.Lock()
			callCountAfterFirstPass := calls.getStateCount
			calls.mu.Unlock()

			// Run second pass and verify zero calls to GitHub
			if err := reconciler.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile failed: %v", err)
			}

			calls.mu.Lock()
			defer calls.mu.Unlock()
			if calls.closeCount != 0 {
				t.Errorf("expected retrofit not to close an already-%s PR, got %d close calls", prState, calls.closeCount)
			}
			if calls.getStateCount != callCountAfterFirstPass {
				t.Errorf("expected no GitHub calls on second pass after tombstoning, but got %d calls (was %d)", calls.getStateCount, callCountAfterFirstPass)
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, logger)
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, logger)
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

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, logger)
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

// TestRateLimitBackoffPersistsAcrossReconcilePasses drives the reconciler through
// three passes to prove the per-owner backoff is real reconciler state (not scoped
// to a single Reconcile call): pass 1 hits the rate limit and aborts the rest of
// that owner's checks for the pass; pass 2, before the reset, still skips the
// owner entirely; pass 3, after the reset, resumes. A second owner is checked on
// every pass throughout, unaffected by the first owner's backoff.
func TestRateLimitBackoffPersistsAcrossReconcilePasses(t *testing.T) {
	ctx := context.Background()

	ts := &fakeTaskSource{
		projects: []store.Project{{ID: "proj-1"}},
		tasks: map[string][]store.Task{
			"proj-1": {
				{ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
				{ID: "task-2", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
				{ID: "task-3", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-1", Kind: "pr", Value: "https://github.com/owner1/repo/pull/1"}},
			},
			"task-2": {
				ID: "task-2", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-2", Kind: "pr", Value: "https://github.com/owner1/repo/pull/2"}},
			},
			"task-3": {
				ID: "task-3", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-3", Kind: "pr", Value: "https://github.com/owner2/repo/pull/3"}},
			},
		},
	}

	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var owner1Calls, owner2Calls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		if owner == "owner1" {
			owner1Calls++
			if owner1Calls == 1 {
				return "", &forge.RateLimitError{StatusCode: 403, Body: "API rate limit exceeded"}
			}
			return "open", nil
		}
		owner2Calls++
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))

	backoffInterval := time.Minute
	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, backoffInterval, 0, logger)
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	t0 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	nowVal := t0
	reconciler.now = func() time.Time { return nowVal }

	// Pass 1: owner1's first check hits the rate limit and aborts the rest of the
	// pass for owner1 (task-2 is skipped); owner2 is unaffected.
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 1: expected no error, got %v", err)
	}
	if owner1Calls != 1 {
		t.Errorf("pass 1: expected 1 getPRState call for owner1, got %d", owner1Calls)
	}
	if owner2Calls != 1 {
		t.Errorf("pass 1: expected 1 getPRState call for owner2, got %d", owner2Calls)
	}
	logStr := logOutput.String()
	if !strings.Contains(logStr, "entering rate limit backoff") || !strings.Contains(logStr, "owner1") {
		t.Errorf("pass 1: expected backoff WARN for owner1, got: %s", logStr)
	}
	if strings.Contains(logStr, "resumed") {
		t.Errorf("pass 1: did not expect a resume log, got: %s", logStr)
	}

	// Pass 2: before the backoff interval elapses, owner1 is still skipped
	// entirely (this is the cross-pass persistence the fix is for); owner2
	// keeps working.
	logOutput.Reset()
	nowVal = t0.Add(30 * time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 2: expected no error, got %v", err)
	}
	if owner1Calls != 1 {
		t.Errorf("pass 2: expected still 1 getPRState call for owner1 (before reset), got %d", owner1Calls)
	}
	if owner2Calls != 2 {
		t.Errorf("pass 2: expected 2 getPRState calls for owner2, got %d", owner2Calls)
	}
	logStr = logOutput.String()
	if strings.Contains(logStr, "entering rate limit backoff") {
		t.Errorf("pass 2: did not expect a new backoff WARN, got: %s", logStr)
	}
	if strings.Contains(logStr, "resumed") {
		t.Errorf("pass 2: did not expect a resume log before reset, got: %s", logStr)
	}

	// Pass 3: after the backoff interval elapses, owner1 resumes: task-1's retry
	// succeeds, and task-2 (no longer backed off) is checked too, for 2 more
	// calls. The resume transition logs exactly one INFO.
	logOutput.Reset()
	nowVal = t0.Add(90 * time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 3: expected no error, got %v", err)
	}
	if owner1Calls != 3 {
		t.Errorf("pass 3: expected 3 getPRState calls for owner1 (resumed, both tasks checked), got %d", owner1Calls)
	}
	if owner2Calls != 3 {
		t.Errorf("pass 3: expected 3 getPRState calls for owner2, got %d", owner2Calls)
	}
	logStr = logOutput.String()
	if !strings.Contains(logStr, "rate limit backoff resumed") || !strings.Contains(logStr, "owner1") {
		t.Errorf("pass 3: expected resume INFO for owner1, got: %s", logStr)
	}
}

// TestRateLimitBackoffUsesXRateLimitResetHeader proves the not-before time comes
// from the rate-limit response's X-RateLimit-Reset when present, not the fixed
// fallback interval: with a short fallback interval and a reset header minutes
// out, the owner must still be backed off once the fallback interval alone would
// have expired, and must resume once the header's reset time passes.
func TestRateLimitBackoffUsesXRateLimitResetHeader(t *testing.T) {
	ctx := context.Background()

	ts := &fakeTaskSource{
		projects: []store.Project{{ID: "proj-1"}},
		tasks: map[string][]store.Task{
			"proj-1": {
				{ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-1", Kind: "pr", Value: "https://github.com/owner1/repo/pull/1"}},
			},
		},
	}

	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	t0 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	resetAt := t0.Add(5 * time.Minute)

	var calls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		calls++
		if calls == 1 {
			return "", &forge.RateLimitError{StatusCode: 403, Body: "API rate limit exceeded", Reset: resetAt}
		}
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	// A fallback interval much shorter than the reset window: if it were used
	// instead of the header, the owner would incorrectly resume long before
	// resetAt.
	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, 10*time.Second, 0, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision

	nowVal := t0
	reconciler.now = func() time.Time { return nowVal }

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 1: expected no error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("pass 1: expected 1 getPRState call, got %d", calls)
	}

	// Past the fallback interval (10s) but well before the header's reset time
	// (5 minutes out): must still be backed off.
	nowVal = t0.Add(30 * time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 2: expected no error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("pass 2: expected still 1 getPRState call (backed off until reset header), got %d", calls)
	}

	// Past the header's reset time: resumes.
	nowVal = resetAt.Add(time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 3: expected no error, got %v", err)
	}
	if calls != 2 {
		t.Errorf("pass 3: expected 2 getPRState calls (resumed after reset header), got %d", calls)
	}
}

// TestRateLimitBackoffAbortsCurrentPassEvenWhenResetIsNotInFuture proves the
// within-pass abort guarantee holds even when the rate-limit response's
// X-RateLimit-Reset is at or before the captured pass time (GitHub clock skew, or
// a reset that's already elapsed by the time the error is handled): task-1's rate
// limit must still skip task-2 for the same owner in this same pass, rather than
// treating the owner as immediately resumed and letting task-2 call GitHub again.
func TestRateLimitBackoffAbortsCurrentPassEvenWhenResetIsNotInFuture(t *testing.T) {
	ctx := context.Background()

	ts := &fakeTaskSource{
		projects: []store.Project{{ID: "proj-1"}},
		tasks: map[string][]store.Task{
			"proj-1": {
				{ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
				{ID: "task-2", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-1", Kind: "pr", Value: "https://github.com/owner1/repo/pull/1"}},
			},
			"task-2": {
				ID: "task-2", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-2", Kind: "pr", Value: "https://github.com/owner1/repo/pull/2"}},
			},
		},
	}

	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	t0 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)

	var calls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		calls++
		if calls == 1 {
			// Reset equals the pass time captured by Reconcile: not in the future.
			return "", &forge.RateLimitError{StatusCode: 403, Body: "API rate limit exceeded", Reset: t0}
		}
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision
	reconciler.now = func() time.Time { return t0 }

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 getPRState call (task-2 skipped this pass despite reset==now), got %d", calls)
	}
}

// quotaFloorFakeSource returns a fakeTaskSource with two tasks for owner1 and one
// for owner2, mirroring TestRateLimitBackoffPersistsAcrossReconcilePasses so the
// quota-floor tests can reuse the same cross-owner, cross-pass shape.
func quotaFloorFakeSource() *fakeTaskSource {
	return &fakeTaskSource{
		projects: []store.Project{{ID: "proj-1"}},
		tasks: map[string][]store.Task{
			"proj-1": {
				{ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
				{ID: "task-2", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
				{ID: "task-3", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-1", Kind: "pr", Value: "https://github.com/owner1/repo/pull/1"}},
			},
			"task-2": {
				ID: "task-2", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-2", Kind: "pr", Value: "https://github.com/owner1/repo/pull/2"}},
			},
			"task-3": {
				ID: "task-3", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-3", Kind: "pr", Value: "https://github.com/owner2/repo/pull/3"}},
			},
		},
	}
}

// TestQuotaFloorBelowFloorSkipsCallsAndPersistsAcrossPasses proves that when the
// remaining-quota lookup reports a value below the floor for an owner, the
// reconciler makes zero PR-state/review-decision calls for that owner in the
// triggering pass, the backoff persists on a second pass before the reported
// reset, and a pass after the reset resumes (and re-checks quota, which by then
// reports comfortably above the floor). A second owner is unaffected throughout.
func TestQuotaFloorBelowFloorSkipsCallsAndPersistsAcrossPasses(t *testing.T) {
	ctx := context.Background()

	ts := quotaFloorFakeSource()
	notifier := &fakeNotifierForReconciler{}
	// Distinct tokens per owner so the quota lookup below can tell them apart.
	tokenLookup := func(owner string) (string, error) { return owner + "-token", nil }

	t0 := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	resetAt := t0.Add(5 * time.Minute)

	var owner1Calls, owner2Calls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		if owner == "owner1" {
			owner1Calls++
		} else {
			owner2Calls++
		}
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	var nowVal time.Time
	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 100, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision
	reconciler.remainingQuotaLookup = func(ctx context.Context, token string) (*forge.QuotaInfo, error) {
		// owner1 is below the floor until the reset; owner2 is always comfortably
		// above it, so it's never affected.
		if token == "owner1-token" && nowVal.Before(resetAt) {
			return &forge.QuotaInfo{Remaining: 50, Reset: resetAt}, nil
		}
		return &forge.QuotaInfo{Remaining: 5000, Reset: resetAt}, nil
	}
	reconciler.now = func() time.Time { return nowVal }
	nowVal = t0

	// Pass 1: the quota check for owner1 (whichever task hits it first) reports
	// remaining below the floor, so both of owner1's tasks are skipped this pass;
	// owner2 proceeds normally.
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 1: expected no error, got %v", err)
	}
	if owner1Calls != 0 {
		t.Errorf("pass 1: expected 0 getPRState calls for owner1, got %d", owner1Calls)
	}
	if owner2Calls != 1 {
		t.Errorf("pass 1: expected 1 getPRState call for owner2, got %d", owner2Calls)
	}

	// Pass 2: before the reported reset, owner1 is still backed off (checkBackoff
	// short-circuits before any new lookup), so no new calls for owner1.
	nowVal = t0.Add(30 * time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 2: expected no error, got %v", err)
	}
	if owner1Calls != 0 {
		t.Errorf("pass 2: expected still 0 getPRState calls for owner1 (before reset), got %d", owner1Calls)
	}
	if owner2Calls != 2 {
		t.Errorf("pass 2: expected 2 getPRState calls for owner2, got %d", owner2Calls)
	}

	// Pass 3: after the reset, owner1 resumes, the quota check is re-run and now
	// reports comfortably above the floor, so both of owner1's tasks proceed.
	nowVal = resetAt.Add(time.Second)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 3: expected no error, got %v", err)
	}
	if owner1Calls != 2 {
		t.Errorf("pass 3: expected 2 getPRState calls for owner1 (resumed, both tasks checked), got %d", owner1Calls)
	}
	if owner2Calls != 3 {
		t.Errorf("pass 3: expected 3 getPRState calls for owner2, got %d", owner2Calls)
	}
}

// TestQuotaFloorAtExactFloorEntersBackoff proves the floor is inclusive: a
// remaining count exactly equal to the floor must not be treated as "above" it.
// Spending one more call at exactly the floor would drive the owner's remaining
// quota below the floor, which is the outcome the floor exists to prevent.
func TestQuotaFloorAtExactFloorEntersBackoff(t *testing.T) {
	ctx := context.Background()

	ts := &fakeTaskSource{
		projects: []store.Project{{ID: "proj-1"}},
		tasks: map[string][]store.Task{
			"proj-1": {
				{ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-1", Kind: "pr", Value: "https://github.com/owner1/repo/pull/1"}},
			},
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var prCalls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		prCalls++
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 100, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision
	reconciler.remainingQuotaLookup = func(ctx context.Context, token string) (*forge.QuotaInfo, error) {
		return &forge.QuotaInfo{Remaining: 100}, nil // exactly at the floor
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if prCalls != 0 {
		t.Errorf("expected 0 getPRState calls when remaining equals the floor, got %d", prCalls)
	}
}

// TestQuotaFloorAboveFloorChecksOncePerOwnerPerPass proves the lookup is called
// exactly once per owner per pass even when the owner has several tasks, and
// that the PR calls proceed normally for all of them.
func TestQuotaFloorAboveFloorChecksOncePerOwnerPerPass(t *testing.T) {
	ctx := context.Background()

	ts := quotaFloorFakeSource()
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var lookupCalls int
	var owner1Calls, owner2Calls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		if owner == "owner1" {
			owner1Calls++
		} else {
			owner2Calls++
		}
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 100, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision
	reconciler.remainingQuotaLookup = func(ctx context.Context, token string) (*forge.QuotaInfo, error) {
		lookupCalls++
		return &forge.QuotaInfo{Remaining: 5000}, nil
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if lookupCalls != 2 {
		t.Errorf("expected exactly 2 quota lookup calls (one per owner), got %d", lookupCalls)
	}
	if owner1Calls != 2 {
		t.Errorf("expected 2 getPRState calls for owner1 (both tasks proceed), got %d", owner1Calls)
	}
	if owner2Calls != 1 {
		t.Errorf("expected 1 getPRState call for owner2, got %d", owner2Calls)
	}
}

// TestQuotaFloorZeroDisablesLookup proves a floor of 0 disables the check
// entirely: the lookup is never called and every PR call proceeds normally.
func TestQuotaFloorZeroDisablesLookup(t *testing.T) {
	ctx := context.Background()

	ts := quotaFloorFakeSource()
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var lookupCalls int
	var prCalls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		prCalls++
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		return "approved", time.Time{}, nil
	}

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 0, newTestLogger())
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision
	reconciler.remainingQuotaLookup = func(ctx context.Context, token string) (*forge.QuotaInfo, error) {
		lookupCalls++
		return &forge.QuotaInfo{Remaining: 0}, nil
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if lookupCalls != 0 {
		t.Errorf("expected the quota lookup to never be called with floor=0, got %d calls", lookupCalls)
	}
	if prCalls != 3 {
		t.Errorf("expected all 3 tasks' getPRState calls to proceed, got %d", prCalls)
	}
}

// TestQuotaFloorLookupErrorProceedsWithNormalCall proves that a non-rate-limit
// error from the remaining-quota lookup is logged once and does not stall
// reconciliation: the PR-state and review-decision calls that would have
// followed the lookup still happen.
func TestQuotaFloorLookupErrorProceedsWithNormalCall(t *testing.T) {
	ctx := context.Background()

	ts := &fakeTaskSource{
		projects: []store.Project{{ID: "proj-1"}},
		tasks: map[string][]store.Task{
			"proj-1": {
				{ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z"},
			},
		},
		taskWithDepsAndLinks: map[string]store.TaskWithDepsAndLinks{
			"task-1": {
				ID: "task-1", State: "approved", UpdatedAt: "2024-01-01T00:00:00Z",
				Links: []store.TaskLink{{ID: "link-1", Kind: "pr", Value: "https://github.com/owner1/repo/pull/1"}},
			},
		},
	}
	notifier := &fakeNotifierForReconciler{}
	tokenLookup := func(owner string) (string, error) { return "token", nil }

	var prStateCalls, reviewDecisionCalls int
	getPRState := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, error) {
		prStateCalls++
		return "open", nil
	}
	getReviewDecision := func(ctx context.Context, owner, repo string, prNumber int, token string) (string, time.Time, error) {
		reviewDecisionCalls++
		return "approved", time.Time{}, nil
	}

	var logOutput strings.Builder
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))

	reconciler := NewPRWatchReconciler(ts, notifier, tokenLookup, time.Minute, 100, logger)
	reconciler.getPRState = getPRState
	reconciler.getReviewDecision = getReviewDecision
	reconciler.remainingQuotaLookup = func(ctx context.Context, token string) (*forge.QuotaInfo, error) {
		return nil, errors.New("rate_limit endpoint unreachable")
	}

	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if prStateCalls != 1 {
		t.Errorf("expected the getPRState call to proceed after a lookup error, got %d calls", prStateCalls)
	}
	if reviewDecisionCalls != 1 {
		t.Errorf("expected the getReviewDecision call to proceed after a lookup error, got %d calls", reviewDecisionCalls)
	}
	logStr := logOutput.String()
	if !strings.Contains(logStr, "remaining quota lookup error") || !strings.Contains(logStr, "owner1") {
		t.Errorf("expected a WARN logging the lookup error for owner1, got: %s", logStr)
	}
}

// captureLogRecords captures slog records using a custom Handler
type captureLogRecordsHandler struct {
	records []*slog.Record
}

func (h *captureLogRecordsHandler) Handle(ctx context.Context, r slog.Record) error {
	// Make a copy so we don't share state
	cp := r
	h.records = append(h.records, &cp)
	return nil
}

func (h *captureLogRecordsHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *captureLogRecordsHandler) WithGroup(name string) slog.Handler {
	return h
}

func (h *captureLogRecordsHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return true
}

// TestNoForgeTokenWarningOnChange verifies that WARN is emitted when an owner's
// skipped count appears for the first time, changes, or reappears after disappearing.
func TestNoForgeTokenWarningOnChange(t *testing.T) {
	ctx := context.Background()

	// Setup initial state with one owner missing a token
	ts := &fakeTaskSource{
		projects: []store.Project{{ID: "proj-1"}},
		tasks: map[string][]store.Task{
			"proj-1": {
				{
					ID:         "task-1",
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
					{Kind: "pr", Value: "https://github.com/owner-no-token/repo/pull/1"},
				},
			},
		},
	}

	logHandler := &captureLogRecordsHandler{}
	logger := slog.New(logHandler)

	tokenLookup := func(owner string) (string, error) {
		if owner == "owner-no-token" {
			return "", nil
		}
		return "token", nil
	}

	reconciler := NewPRWatchReconciler(ts, &fakeNotifierForReconciler{}, tokenLookup, time.Minute, 0, logger)

	// Pass 1: first time we see owner-no-token, should log WARN
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 1: expected no error, got %v", err)
	}

	warnCount1 := countRecordsByLevelAndMessage(logHandler.records, slog.LevelWarn, "no forge token for owner")
	if warnCount1 != 1 {
		t.Errorf("pass 1: expected 1 WARN for 'no forge token for owner', got %d", warnCount1)
	}

	// Pass 2: same owner with same count, should NOT log WARN
	logHandler.records = nil
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 2: expected no error, got %v", err)
	}

	warnCount2 := countRecordsByLevelAndMessage(logHandler.records, slog.LevelWarn, "no forge token for owner")
	if warnCount2 != 0 {
		t.Errorf("pass 2: expected 0 WARN for 'no forge token for owner' (same count), got %d", warnCount2)
	}

	// Pass 3: now add another task for the same owner, count changes, should log WARN again
	ts.tasks["proj-1"] = append(ts.tasks["proj-1"], store.Task{
		ID:         "task-2",
		State:      "approved",
		UpdatedAt:  "2024-01-01T00:00:00Z",
		AgentMerge: false,
	})
	ts.taskWithDepsAndLinks["task-2"] = store.TaskWithDepsAndLinks{
		ID:        "task-2",
		State:     "approved",
		UpdatedAt: "2024-01-01T00:00:00Z",
		Links: []store.TaskLink{
			{Kind: "pr", Value: "https://github.com/owner-no-token/repo/pull/2"},
		},
	}

	logHandler.records = nil
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 3: expected no error, got %v", err)
	}

	warnCount3 := countRecordsByLevelAndMessage(logHandler.records, slog.LevelWarn, "no forge token for owner")
	if warnCount3 != 1 {
		t.Errorf("pass 3: expected 1 WARN for 'no forge token for owner' (count changed), got %d", warnCount3)
	}

	// Pass 4: remove the missing-token owner, should log INFO
	ts.tasks["proj-1"] = []store.Task{} // Remove all tasks

	logHandler.records = nil
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 4: expected no error, got %v", err)
	}

	infoCount := countRecordsByLevelAndMessage(logHandler.records, slog.LevelInfo, "no longer skipping owner")
	if infoCount != 1 {
		t.Errorf("pass 4: expected 1 INFO for 'no longer skipping owner', got %d", infoCount)
	}

	// Pass 5: owner reappears (e.g., new task added), should log WARN again
	ts.tasks["proj-1"] = []store.Task{
		{
			ID:         "task-3",
			State:      "approved",
			UpdatedAt:  "2024-01-01T00:00:00Z",
			AgentMerge: false,
		},
	}
	ts.taskWithDepsAndLinks["task-3"] = store.TaskWithDepsAndLinks{
		ID:        "task-3",
		State:     "approved",
		UpdatedAt: "2024-01-01T00:00:00Z",
		Links: []store.TaskLink{
			{Kind: "pr", Value: "https://github.com/owner-no-token/repo/pull/3"},
		},
	}

	logHandler.records = nil
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatalf("pass 5: expected no error, got %v", err)
	}

	warnCount5 := countRecordsByLevelAndMessage(logHandler.records, slog.LevelWarn, "no forge token for owner")
	if warnCount5 != 1 {
		t.Errorf("pass 5: expected 1 WARN for 'no forge token for owner' (owner reappears), got %d", warnCount5)
	}
}

func countRecordsByLevelAndMessage(records []*slog.Record, level slog.Level, msgContains string) int {
	count := 0
	for _, r := range records {
		if r.Level == level && strings.Contains(r.Message, msgContains) {
			count++
		}
	}
	return count
}
