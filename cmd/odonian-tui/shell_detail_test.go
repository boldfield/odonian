package main

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
)

// newDetailFixture is an 80x24 shell over tasks, with the client serving detail for GetTask
// and docs for ListDocuments, the project repo set and the URL opener recording into
// *opened. It selects detail.ID and opens its detail view.
func newDetailFixture(t *testing.T, detail tuiclient.TaskDetail, docs []tuiclient.Document, opened *[]string, extra ...tuiclient.Task) *boardFixture {
	t.Helper()
	tasks := append([]tuiclient.Task{
		{ID: detail.ID, Title: detail.Title, State: detail.State, Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
	}, extra...)
	f := newBoardFixture(t, 80, 24, tasks...)
	f.mock().GetTaskFunc = func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
		return detail, nil
	}
	f.mock().ListDocumentsFunc = func(ctx context.Context, projectID string) ([]tuiclient.Document, error) {
		return docs, nil
	}
	f.model.project.Repo = "https://github.com/example/repo"
	f.model.urlOpener = func(rawURL string) error {
		if opened != nil {
			*opened = append(*opened, rawURL)
		}
		return nil
	}
	f.selectItem(t, detail.ID)
	f.press("enter")
	if f.model.mode != modeDetail {
		t.Fatalf("expected modeDetail after enter, got %d", f.model.mode)
	}
	return f
}

func plainDetail(id, title string) tuiclient.TaskDetail {
	return tuiclient.TaskDetail{
		ID:        id,
		Title:     title,
		Spec:      "spec",
		State:     "in_progress",
		CreatedAt: "2026-01-01T00:00:00Z",
		UpdatedAt: "2026-01-01T00:00:00Z",
	}
}

// Enter opens the detail view, which renders spec, state, deps as titles, links, result and meta.
func TestDetail_EnterOpensDetail(t *testing.T) {
	depID := "dep-task-1111"
	depTitle := "Dep Task One"
	result := "see PR #42"
	leaseAt := time.Now().Add(3 * time.Minute).Format(time.RFC3339Nano)
	assignee := "agent-x"

	detail := tuiclient.TaskDetail{
		ID:             "task-detail-1",
		Title:          "My Feature Task",
		Spec:           "This is the full spec text for the task.",
		State:          "in_progress",
		Assignee:       &assignee,
		LeaseExpiresAt: &leaseAt,
		Result:         &result,
		CreatedAt:      "2026-01-01T00:00:00Z",
		UpdatedAt:      "2026-01-02T00:00:00Z",
		DependsOn:      []string{depID},
		Links: []tuiclient.TaskLink{
			{Kind: "pr", Value: "https://github.com/example/repo/pull/42"},
		},
	}
	// The board lists the dep task so its title can be resolved.
	f := newDetailFixture(t, detail, nil, nil,
		tuiclient.Task{ID: depID, Title: depTitle, State: "in_progress", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"})

	output := f.model.View().Content
	for _, want := range []string{
		"My Feature Task",
		"in_progress",
		"agent-x",
		"This is the full spec text",
		depTitle,
		"https://github.com/example/repo/pull/42",
		"see PR #42",
		"2026-01-01",
	} {
		if !strings.Contains(output, want) {
			t.Errorf("expected %q in detail view, got:\n%s", want, output)
		}
	}
}

// Esc returns to the shell with the same work item selected.
func TestDetail_EscReturnsToBoard(t *testing.T) {
	f := newDetailFixture(t, plainDetail("task-esc-1", "Esc Test Task"), nil, nil,
		tuiclient.Task{ID: "other", Title: "Other Task", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"})

	_, cmd := f.model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})

	if f.model.mode != modeNormal {
		t.Errorf("expected modeNormal after esc, got %d", f.model.mode)
	}
	if cmd != nil {
		t.Errorf("expected nil cmd after esc in detail, got non-nil")
	}
	if got := f.model.shell.items.SelectedTaskID(); got != "task-esc-1" {
		t.Errorf("selected %q after esc, want task-esc-1", got)
	}
	output := f.model.View().Content
	if !strings.Contains(output, "Esc Test Task") || !strings.Contains(output, "Other Task") {
		t.Errorf("expected the shell's items after esc, got:\n%s", output)
	}
	if strings.Contains(output, "Task:") && strings.Contains(output, "State:") {
		t.Errorf("expected the shell, not the detail view, after esc:\n%s", output)
	}
}

// Navigation keys do not move the shell's selection or leave detail mode.
func TestDetail_BoardNavKeysIgnoredInDetail(t *testing.T) {
	f := newDetailFixture(t, plainDetail("task-nav-isolation", "Nav Isolation"), nil, nil,
		tuiclient.Task{ID: "other", Title: "Other Task", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"})
	focus := f.model.shell.focus

	for _, k := range []tea.KeyPressMsg{
		{Code: tea.KeyLeft},
		{Code: tea.KeyRight},
		{Code: 'h', Text: "h"},
		{Code: 'l', Text: "l"},
		{Code: 'j', Text: "j"},
		{Code: 'k', Text: "k"},
		{Code: tea.KeyTab},
	} {
		f.send(k)
		if f.model.mode != modeDetail {
			t.Fatalf("key %v left modeDetail, got %d", k, f.model.mode)
		}
		if got := f.model.shell.items.SelectedTaskID(); got != "task-nav-isolation" {
			t.Errorf("key %v moved the selection to %q", k, got)
		}
		if f.model.selectedTaskID != "task-nav-isolation" {
			t.Errorf("key %v changed selectedTaskID to %q", k, f.model.selectedTaskID)
		}
		if f.model.shell.focus != focus {
			t.Errorf("key %v moved pane focus to %v", k, f.model.shell.focus)
		}
	}
}

// 'o' in detail opens the PR link.
func TestDetail_OpenPRLink(t *testing.T) {
	var opened []string
	prURL := "https://github.com/example/repo/pull/99"
	detail := plainDetail("task-pr-open", "PR Open Task")
	detail.Links = []tuiclient.TaskLink{{Kind: "pr", Value: prURL}}
	f := newDetailFixture(t, detail, nil, &opened)

	f.press("o")

	if len(opened) != 1 || opened[0] != prURL {
		t.Errorf("expected opener called once with %q, got %v", prURL, opened)
	}
	if f.model.detailMessage != "" {
		t.Errorf("expected empty detailMessage on success, got %q", f.model.detailMessage)
	}
}

// 'o' with no PR link shows a message and opens nothing.
func TestDetail_OpenPRLink_NoPR(t *testing.T) {
	var opened []string
	detail := plainDetail("task-no-pr", "No PR Task")
	detail.Links = []tuiclient.TaskLink{}
	f := newDetailFixture(t, detail, nil, &opened)

	f.press("o")

	if len(opened) != 0 {
		t.Errorf("opener called with %v but there is no PR link", opened)
	}
	if !strings.Contains(f.model.detailMessage, "no PR link") {
		t.Errorf("expected 'no PR link' message, got %q", f.model.detailMessage)
	}
	if out := f.model.View().Content; !strings.Contains(out, "no PR link") {
		t.Errorf("expected the message in the rendered detail view:\n%s", out)
	}
}

// 's' builds <repo>/blob/<branch>/<ref> for a path ref.
func TestDetail_OpenSourceDoc_RefPath(t *testing.T) {
	var opened []string
	detail := plainDetail("task-src-path", "Source Path Task")
	detail.DocumentID = "doc-source-1"
	docs := []tuiclient.Document{{
		ID:        "doc-source-1",
		ProjectID: "p1",
		Kind:      "feature_spec",
		Title:     "Feature Doc",
		Ref:       "docs/features/my-feature.md",
		Commit:    nil, // no commit pin → use "main"
	}}
	f := newDetailFixture(t, detail, docs, &opened)

	f.press("s")

	want := "https://github.com/example/repo/blob/main/docs/features/my-feature.md"
	if len(opened) != 1 || opened[0] != want {
		t.Errorf("Source doc URL: expected %q, got %v", want, opened)
	}
	if f.model.detailMessage != "" {
		t.Errorf("Expected no error message, got %q", f.model.detailMessage)
	}
}

// 's' uses the commit hash when set.
func TestDetail_OpenSourceDoc_CommitPin(t *testing.T) {
	var opened []string
	commit := "abc1234def567890"
	detail := plainDetail("task-src-commit", "Source Commit Task")
	detail.DocumentID = "doc-source-2"
	docs := []tuiclient.Document{{
		ID:        "doc-source-2",
		ProjectID: "p1",
		Kind:      "feature_spec",
		Title:     "Feature Doc",
		Ref:       "docs/features/pinned.md",
		Commit:    &commit,
	}}
	f := newDetailFixture(t, detail, docs, &opened)

	f.press("s")

	want := "https://github.com/example/repo/blob/abc1234def567890/docs/features/pinned.md"
	if len(opened) != 1 || opened[0] != want {
		t.Errorf("Commit-pinned source doc URL: expected %q, got %v", want, opened)
	}
}

// 's' opens a ref that is already a URL directly.
func TestDetail_OpenSourceDoc_RefIsURL(t *testing.T) {
	var opened []string
	refURL := "https://docs.example.com/feature-spec"
	detail := plainDetail("task-src-url", "Source URL Task")
	detail.DocumentID = "doc-source-url"
	docs := []tuiclient.Document{{
		ID:        "doc-source-url",
		ProjectID: "p1",
		Kind:      "feature_spec",
		Title:     "External Doc",
		Ref:       refURL,
	}}
	f := newDetailFixture(t, detail, docs, &opened)

	f.press("s")

	// Must open the ref URL directly, not build a blob URL.
	if len(opened) != 1 || opened[0] != refURL {
		t.Errorf("Ref-as-URL: expected opener called with %q, got %v", refURL, opened)
	}
}

// 'd' finds the kind=design doc and builds the correct URL.
func TestDetail_OpenDesignDoc(t *testing.T) {
	var opened []string
	docs := []tuiclient.Document{
		{ID: "doc-spec-1", ProjectID: "p1", Kind: "feature_spec", Title: "Some Feature", Ref: "docs/features/some.md"},
		{ID: "doc-design-1", ProjectID: "p1", Kind: "design", Title: "Project Design", Ref: "DESIGN.md"},
	}
	f := newDetailFixture(t, plainDetail("task-design-open", "Design Doc Task"), docs, &opened)

	f.press("d")

	want := "https://github.com/example/repo/blob/main/DESIGN.md"
	if len(opened) != 1 || opened[0] != want {
		t.Errorf("Design doc URL: expected %q, got %v", want, opened)
	}
}

// 'd' shows a message when no design doc exists.
func TestDetail_OpenDesignDoc_NoDesignDoc(t *testing.T) {
	var opened []string
	// Only a feature_spec doc, no design doc.
	docs := []tuiclient.Document{
		{ID: "doc-feat-1", ProjectID: "p1", Kind: "feature_spec", Title: "A Feature", Ref: "docs/features/feat.md"},
	}
	f := newDetailFixture(t, plainDetail("task-no-design", "No Design Task"), docs, &opened)

	f.press("d")

	if len(opened) != 0 {
		t.Errorf("opener called with %v but there is no design doc", opened)
	}
	if !strings.Contains(f.model.detailMessage, "no design document") {
		t.Errorf("Expected 'no design document' message, got %q", f.model.detailMessage)
	}
}

// reviewDetailFixture is a detail view over a task in review.
func reviewDetailFixture(t *testing.T, id, title string) *boardFixture {
	t.Helper()
	detail := plainDetail(id, title)
	detail.State = "review"
	return newDetailFixture(t, detail, nil, nil)
}

// 'a' in detail for a review task runs the approve flow, and the result lands back on the shell.
func TestDetail_ReviewApproveFromDetail(t *testing.T) {
	f := reviewDetailFixture(t, "review-detail-task-1", "Detail Review Task")
	f.capture()
	f.resetCalls()

	f.press("a")
	if f.model.mode != modeApproveNote {
		t.Fatalf("Expected modeApproveNote after 'a' in detail, got %d", f.model.mode)
	}
	if !f.model.reviewFromDetail {
		t.Errorf("Expected reviewFromDetail=true")
	}

	f.press("enter") // skip the note
	if f.model.mode != modeApproveConfirm {
		t.Fatalf("Expected modeApproveConfirm, got %d", f.model.mode)
	}

	// Confirming returns to detail until the command's result arrives.
	_, cmd := f.model.Update(key("y"))
	if f.model.mode != modeDetail {
		t.Errorf("Expected modeDetail after confirm (before cmd runs), got %d", f.model.mode)
	}
	f.send(f.run(cmd)...)

	if got := f.got(); got != "review approve review-detail-task-1; transition review-detail-task-1 done" {
		t.Errorf("calls = %q, want the review and transition calls", got)
	}
	// Once the command completes the model is back on the shell, not stuck in stale detail.
	if f.model.mode != modeNormal {
		t.Errorf("Expected modeNormal after from-detail approve cmd, got %d", f.model.mode)
	}
	// The refetch lists open items only, so the approved task has left the shell.
	if got := f.itemState("review-detail-task-1"); got != "" {
		t.Errorf("task state = %q after the refetch, want it gone from the open items", got)
	}
	if out := f.model.View().Content; strings.Contains(out, "Task:") && strings.Contains(out, "State:") {
		t.Errorf("expected the shell, not the detail view:\n%s", out)
	}
}

// A 409 during a from-detail approve is visible on the shell (modeNormal), not swallowed in
// the detail view, which does not render m.error.
func TestDetail_ReviewApproveFromDetail_409Visible(t *testing.T) {
	f := reviewDetailFixture(t, "review-detail-409", "Detail 409 Task")
	transition := f.mock().TransitionTaskFunc
	f.mock().TransitionTaskFunc = func(ctx context.Context, id, to string, note *string) error {
		_ = transition(ctx, id, to, note)
		// Simulate a race: the task is already done.
		f.moveTask(id, "done")
		return &tuiclient.APIError{StatusCode: http.StatusConflict, Code: "CONFLICT", Message: "task already transitioned"}
	}
	f.resetCalls()

	f.press("a", "enter", "y")

	if f.model.mode != modeNormal {
		t.Errorf("Expected modeNormal after from-detail 409, got %d — error is invisible in detail mode", f.model.mode)
	}
	if f.model.error == "" {
		t.Errorf("Expected non-empty error after 409 from-detail action, got empty")
	}
	output := f.model.View().Content
	if !strings.Contains(output, "409") && !strings.Contains(output, "task already transitioned") {
		t.Errorf("Expected 409 error in the shell output, got:\n%s", output)
	}
	if len(f.listCalls()) == 0 {
		t.Errorf("expected a refetch after the 409")
	}
	if got := f.itemState("review-detail-409"); got != "" {
		t.Errorf("task state = %q after the refetch, want it gone from the open items", got)
	}
}

// Up and down scroll the spec viewport.
func TestDetail_SpecViewportScrolls(t *testing.T) {
	var specLines []string
	for i := 0; i < 50; i++ {
		specLines = append(specLines, fmt.Sprintf("Spec line %d: some content here to fill space.", i))
	}
	detail := plainDetail("task-scroll", "Scroll Test Task")
	detail.Spec = strings.Join(specLines, "\n")
	f := newDetailFixture(t, detail, nil, nil)

	if got := f.model.detailViewport.YOffset(); got != 0 {
		t.Errorf("Expected initial YOffset=0, got %d", got)
	}
	f.press("down")
	if got := f.model.detailViewport.YOffset(); got != 1 {
		t.Errorf("Expected YOffset=1 after one down, got %d", got)
	}
	f.press("up")
	if got := f.model.detailViewport.YOffset(); got != 0 {
		t.Errorf("Expected YOffset=0 after up, got %d", got)
	}
}

// initDetailViewport clamps the carried-over YOffset to the new content, so opening a
// shorter task after scrolling a longer one neither panics nor leaves the offset out of range.
func TestDetail_ViewportOffsetClamping(t *testing.T) {
	long := plainDetail("long-task", "Long Task")
	long.Spec = strings.Repeat("Line\n", 100)
	short := plainDetail("short-task", "Short Task")
	short.Spec = "Short\nTask"

	f := newDetailFixture(t, long, nil, nil,
		tuiclient.Task{ID: "short-task", Title: "Short Task", State: "in_progress", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"})
	f.mock().GetTaskFunc = func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
		if id == "long-task" {
			return long, nil
		}
		return short, nil
	}

	for i := 0; i < 10; i++ {
		f.press("down")
	}
	if f.model.detailViewport.YOffset() == 0 {
		t.Fatalf("Expected YOffset > 0 after scrolling, got 0")
	}

	f.send(tea.KeyPressMsg{Code: tea.KeyEscape})
	if f.model.mode != modeNormal || f.model.shell.items.SelectedTaskID() != "long-task" {
		t.Fatalf("esc: mode=%d selected=%q, want the shell with long-task selected", f.model.mode, f.model.shell.items.SelectedTaskID())
	}

	f.selectItem(t, "short-task")
	f.press("enter")
	if f.model.mode != modeDetail {
		t.Fatalf("Expected modeDetail for short task, got %d", f.model.mode)
	}

	vp := f.model.detailViewport
	if maxValid := max(0, vp.TotalLineCount()-vp.VisibleLineCount()); vp.YOffset() > maxValid {
		t.Errorf("YOffset %d exceeds max valid %d for content with %d lines and viewport height %d",
			vp.YOffset(), maxValid, vp.TotalLineCount(), vp.VisibleLineCount())
	}
	// Rendering panics if the slice bounds are out of range.
	if out := f.model.View().Content; !strings.Contains(out, "Short Task") {
		t.Errorf("Expected the short task's detail view, got:\n%s", out)
	}
}

// The detail view shows the project name.
func TestBoardModel_ProjectNameInDetailView(t *testing.T) {
	f := newDetailFixture(t, plainDetail("task-detail-1", "My Feature Task"), nil, nil)

	// The fixture's project is "One"; the detail view shows it.
	if out := f.model.View().Content; !strings.Contains(out, "Project: One") {
		t.Errorf("Expected 'Project: One' in detail view, got:\n%s", out)
	}
}

// The detail view shows the task's model.
func TestDetailView_ModelVisibility(t *testing.T) {
	detail := plainDetail("task-1", "Test Task")
	detail.Model = "opus"
	f := newDetailFixture(t, detail, nil, nil)

	if out := f.model.View().Content; !strings.Contains(out, "Model: opus") {
		t.Errorf("Expected 'Model: opus' in detail output, got:\n%s", out)
	}
}

// The detail view's help lists the priority action keys.
func TestBoardModel_DetailViewKeyboardHelp(t *testing.T) {
	detail := plainDetail("task-1", "Task")
	detail.State = "ready"
	f := newDetailFixture(t, detail, nil, nil)

	output := f.model.View().Content
	for _, want := range []string{"i set priority", "w move to front", "e reset priority"} {
		if !strings.Contains(output, want) {
			t.Errorf("Expected %q in detail help, got:\n%s", want, output)
		}
	}
}
