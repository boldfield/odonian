package checkwatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/forge"
	"github.com/boldfield/odonian/internal/ratelimit"
	"github.com/boldfield/odonian/internal/store"
)

type fakeStore struct {
	tasks []store.Task
	links []store.TaskLink
	calls []string
}

func (f *fakeStore) ListProjects(context.Context, store.ProjectListFilter) ([]store.Project, error) {
	return []store.Project{{ID: "p1"}}, nil
}
func (f *fakeStore) ListTasks(context.Context, string, store.TaskListFilter) ([]store.Task, error) {
	return f.tasks, nil
}
func (f *fakeStore) GetTask(_ context.Context, id string) (store.TaskWithDepsAndLinks, error) {
	var t store.TaskWithDepsAndLinks
	t.ID = id
	t.Links = f.links
	return t, nil
}
func (f *fakeStore) PassChecks(_ context.Context, id, sha, s string) (store.Task, error) {
	f.calls = append(f.calls, "pass")
	return store.Task{}, nil
}
func (f *fakeStore) FailChecks(_ context.Context, id, sha, s string, _ int, _ map[string]int, _ map[string]int) (store.Task, error) {
	f.calls = append(f.calls, "fail")
	return store.Task{}, nil
}
func (f *fakeStore) TimeoutChecks(_ context.Context, id, note string) (store.Task, error) {
	f.calls = append(f.calls, "timeout:"+note)
	return store.Task{}, nil
}
func (f *fakeStore) TombstoneLink(context.Context, string, string) error {
	f.calls = append(f.calls, "tombstone")
	return nil
}

type harness struct {
	r        *Reconciler
	st       *fakeStore
	now      time.Time
	forges   int
	comment  string
	checks   *forge.PRChecks
	checkErr error
	postErr  error
	token    string
}

func newHarness(t *testing.T, checks *forge.PRChecks) *harness {
	t.Helper()
	started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ts := started.Format(time.RFC3339Nano)
	h := &harness{
		st:     &fakeStore{tasks: []store.Task{{ID: "t1", State: "checks", ChecksStartedAt: &ts, UpdatedAt: ts}}, links: []store.TaskLink{{ID: "l1", Kind: "pr", Value: "https://github.com/o/r/pull/7"}}},
		now:    started.Add(time.Hour),
		checks: checks,
		token:  "tok",
	}
	r := NewReconciler(h.st, func(string) (string, error) { return h.token, nil },
		ratelimit.New(time.Minute, 0), Config{Settle: time.Minute, NoRunsGrace: time.Minute, Timeout: 3 * time.Hour}, Options{}, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.now = func() time.Time { return h.now }
	r.getPRChecks = func(context.Context, string, string, int, string) (*forge.PRChecks, error) {
		h.forges++
		return h.checks, h.checkErr
	}
	r.postPRComment = func(_ context.Context, _, _ string, _ int, _, c string) error {
		h.st.calls = append(h.st.calls, "comment")
		h.comment = c
		return h.postErr
	}
	h.r = r
	return h
}

func TestReconcilePass(t *testing.T) {
	h := newHarness(t, &forge.PRChecks{State: "open", HeadSHA: "abc", RollupState: "SUCCESS", Contexts: []forge.CheckContext{{Name: "ci", Conclusion: "SUCCESS"}}})
	h.r.Reconcile(context.Background())
	if len(h.st.calls) != 1 || h.st.calls[0] != "pass" {
		t.Fatalf("calls = %v", h.st.calls)
	}
}

func TestReconcileFailCommentsBeforeTransition(t *testing.T) {
	h := newHarness(t, &forge.PRChecks{State: "open", HeadSHA: "abc123", Contexts: []forge.CheckContext{{Name: "build", Conclusion: "FAILURE", URL: "https://ci/1"}, {Name: "lint", Conclusion: "SUCCESS", URL: "https://ci/2"}}})
	h.r.Reconcile(context.Background())
	if strings.Join(h.st.calls, ",") != "comment,fail" {
		t.Fatalf("calls = %v", h.st.calls)
	}
	if !strings.HasPrefix(h.comment, "odonian-checks:") || !strings.Contains(h.comment, "abc123") || !strings.Contains(h.comment, "build") || !strings.Contains(h.comment, "https://ci/1") || strings.Contains(h.comment, "lint") {
		t.Fatalf("comment = %q", h.comment)
	}
}

func TestReconcileFailCommentErrorLeavesTask(t *testing.T) {
	h := newHarness(t, &forge.PRChecks{State: "open", HeadSHA: "abc", Contexts: []forge.CheckContext{{Name: "build", Conclusion: "FAILURE"}}})
	h.postErr = errors.New("boom")
	h.r.Reconcile(context.Background())
	if strings.Join(h.st.calls, ",") != "comment" {
		t.Fatalf("calls = %v", h.st.calls)
	}
}

func TestReconcileTimeout(t *testing.T) {
	h := newHarness(t, &forge.PRChecks{State: "open", HeadSHA: "abc", RollupState: "PENDING", Contexts: []forge.CheckContext{{Name: "ci", Conclusion: ""}}})
	h.now = h.now.Add(4 * time.Hour)
	h.r.Reconcile(context.Background())
	if len(h.st.calls) != 1 || !strings.HasPrefix(h.st.calls[0], "timeout:") {
		t.Fatalf("calls = %v", h.st.calls)
	}
}

func TestReconcilePRClosed(t *testing.T) {
	h := newHarness(t, &forge.PRChecks{State: "closed"})
	h.r.Reconcile(context.Background())
	if len(h.st.calls) != 1 || !strings.Contains(h.st.calls[0], "closed unmerged") {
		t.Fatalf("calls = %v", h.st.calls)
	}
}

func TestReconcileWait(t *testing.T) {
	h := newHarness(t, &forge.PRChecks{State: "open", HeadSHA: "abc", RollupState: "PENDING", Contexts: []forge.CheckContext{{Name: "ci"}}})
	h.r.Reconcile(context.Background())
	if len(h.st.calls) != 0 || h.forges != 1 {
		t.Fatalf("calls = %v forges = %d", h.st.calls, h.forges)
	}
}

func TestReconcileNoPRLinkBlocks(t *testing.T) {
	h := newHarness(t, nil)
	h.st.links = nil
	h.r.Reconcile(context.Background())
	if len(h.st.calls) != 1 || !strings.Contains(h.st.calls[0], "no pull request link") || h.forges != 0 {
		t.Fatalf("calls = %v", h.st.calls)
	}
}

func TestReconcileRateLimitStopsOwnerAndMarksGate(t *testing.T) {
	h := newHarness(t, nil)
	h.st.tasks = append(h.st.tasks, store.Task{ID: "t2", State: "checks", UpdatedAt: h.st.tasks[0].UpdatedAt})
	h.checkErr = &forge.RateLimitError{StatusCode: 403, Reset: h.now.Add(10 * time.Minute)}
	h.r.Reconcile(context.Background())
	if h.forges != 1 {
		t.Fatalf("forge calls = %d, want 1", h.forges)
	}
	if backed, _ := h.r.limiter.Check("o", h.now); !backed {
		t.Fatal("shared gate not marked")
	}
}

func TestReconcileMinInterval(t *testing.T) {
	h := newHarness(t, &forge.PRChecks{State: "open", HeadSHA: "abc", RollupState: "PENDING", Contexts: []forge.CheckContext{{Name: "ci"}}})
	h.r.Reconcile(context.Background())
	h.now = h.now.Add(10 * time.Second)
	h.r.Reconcile(context.Background())
	if h.forges != 1 {
		t.Fatalf("forge calls = %d, want 1", h.forges)
	}
	h.now = h.now.Add(2 * time.Minute)
	h.r.Reconcile(context.Background())
	if h.forges != 2 {
		t.Fatalf("forge calls = %d, want 2", h.forges)
	}
}

func TestReconcileNoTokenSkips(t *testing.T) {
	h := newHarness(t, nil)
	h.token = ""
	h.r.Reconcile(context.Background())
	if h.forges != 0 || len(h.st.calls) != 0 {
		t.Fatalf("forges=%d calls=%v", h.forges, h.st.calls)
	}
}
