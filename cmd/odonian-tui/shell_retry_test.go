package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/tuiclient"
)

// retryHintFor is the hint the shell shows for the bound refresh key.
func retryHintFor(m *BoardModel) string {
	return "Press '" + m.keyBindings.Shell.binding(actRefresh).Help().Key + "' to retry."
}

// pressRefresh sends the bound refresh key, written from the binding rather than a letter.
func pressRefresh(f *shellFixture) {
	f.send(key(f.model.keyBindings.Shell.binding(actRefresh).Help().Key))
}

// setListTasks replaces what the fixture's client returns for a task fetch.
func setListTasks(f *shellFixture, fn func() ([]tuiclient.Task, error)) {
	f.model.client.(*tuiclient.MockClient).ListTasksFunc = func(ctx context.Context, projectID string, opts ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
		return fn()
	}
}

func TestShellFetchErrorShowsRetryHintAndRefreshClearsIt(t *testing.T) {
	f := newShellFixture(t, 140, 24)
	hint := retryHintFor(f.model)
	normal := f.model.shellKeyHints()

	f.send(shellTasksMsg{projectID: "p1", err: errors.New("boom")})
	out := f.model.View().Content
	if !strings.Contains(out, "Error: boom") || !strings.Contains(out, hint) {
		t.Fatalf("want error and %q in view:\n%s", hint, out)
	}
	assertWidth(t, out, 140)

	// The refresh key refetches; a successful fetch clears the error and the hint.
	pressRefresh(f)
	out = f.model.View().Content
	if strings.Contains(out, "boom") || strings.Contains(out, hint) {
		t.Errorf("error or hint still shown after a successful refresh:\n%s", out)
	}
	if !strings.Contains(out, strings.Fields(normal)[0]) {
		t.Errorf("normal key hints not back:\n%s", out)
	}
}

func TestShellRefreshKeyFailureShowsHint(t *testing.T) {
	f := newShellFixture(t, 140, 24)
	hint := retryHintFor(f.model)
	setListTasks(f, func() ([]tuiclient.Task, error) { return nil, errors.New("network down") })
	pressRefresh(f)
	out := f.model.View().Content
	if !strings.Contains(out, "Error: network down") || !strings.Contains(out, hint) {
		t.Errorf("want error and hint after a failed refresh:\n%s", out)
	}
	// A retry that fails with a different error still shows the hint.
	setListTasks(f, func() ([]tuiclient.Task, error) { return nil, errors.New("different") })
	pressRefresh(f)
	out = f.model.View().Content
	if !strings.Contains(out, "Error: different") || !strings.Contains(out, hint) || strings.Contains(out, "network down") {
		t.Errorf("want the new error and hint:\n%s", out)
	}
}

func TestShellFetchErrorKeepsHintAtNarrowWidths(t *testing.T) {
	for _, w := range []int{60, 40, 29, 25, 21, 20, 10} {
		f := newShellFixture(t, w, 20)
		hint := retryHintFor(f.model)
		f.send(shellTasksMsg{projectID: "p1", err: errors.New(strings.Repeat("x", 150))})
		if w >= 40 { // narrower than that the shell shows only a resize notice
			assertWidth(t, f.model.View().Content, w)
		}
		bar := f.model.shellHelpBar()
		assertWidth(t, bar, w)
		if w >= len(hint) && !strings.Contains(bar, hint) {
			t.Errorf("width %d: hint missing from %q", w, bar)
		}
		if strings.HasPrefix(bar, ". ") || strings.HasPrefix(bar, " ") {
			t.Errorf("width %d: leading separator in %q", w, bar)
		}
	}
}

func TestShellLaterErrorDoesNotInheritRetryHint(t *testing.T) {
	cases := []struct{ name, fetchErr, later string }{
		{"short fetch error then long error", "boom", strings.Repeat("p", 155)},
		{"long fetch error then short error", strings.Repeat("x", 100), "promote failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newShellFixture(t, 60, 24)
			hint := retryHintFor(f.model)
			f.send(shellTasksMsg{projectID: "p1", err: errors.New(c.fetchErr)})
			f.send(promoteErrorMsg{taskID: "a2", err: c.later})
			out := f.model.View().Content
			assertWidth(t, out, 60)
			if strings.Contains(out, hint) {
				t.Errorf("stale retry hint shown:\n%s", out)
			}
			want := c.later
			if len(want) > 40 {
				want = want[:40]
			}
			if !strings.Contains(out, want) {
				t.Errorf("current error %q not shown:\n%s", want, out)
			}
		})
	}
}

func TestShellPostActionRefetchFailureShowsHint(t *testing.T) {
	f := newShellFixture(t, 140, 24)
	hint := retryHintFor(f.model)
	setListTasks(f, func() ([]tuiclient.Task, error) { return nil, errors.New("network down") })

	msg := f.model.fetchTasksInline(context.Background(), "")
	if !msg.refetchFailed {
		t.Fatalf("a failed refetch must be marked as one: %+v", msg)
	}
	f.send(msg)
	out := f.model.View().Content
	if !strings.Contains(out, "refetch failed: network down") || !strings.Contains(out, hint) {
		t.Errorf("want refetch error and %q in view:\n%s", hint, out)
	}

	// An action error that is not a refetch failure gets no hint.
	f.send(reviewActionMsg{err: "approve review failed: nope"})
	out = f.model.View().Content
	if !strings.Contains(out, "approve review failed: nope") || strings.Contains(out, hint) {
		t.Errorf("want the action error without the hint:\n%s", out)
	}
}
