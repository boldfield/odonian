package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
)

// boardFixture is the three-pane shell over a recording fake client. The client serves the
// tasks in listed (per project, filtered by the state and claimable options it is asked
// for) and records every call; commands run synchronously, and a poll tick is recorded
// rather than waited on, so a fixture never blocks.
//
//	f := newBoardFixture(t, 140, 40,
//		tuiclient.Task{ID: "t1", Title: "One", State: "review", Kind: "implement"},
//		tuiclient.Task{ID: "t2", Title: "Two", State: "ready", Kind: "implement"},
//	)
//	f.selectItem(t, "t1")          // move the items-pane selection
//	f.press("a", "enter", "y")     // keys; a multi-character string is typed
//	f.send(tickMsg{})              // deliver a message and everything it leads to
//	f.got()                        // "review approve t1; transition t1 done"
//	f.model.viewContent()          // the rendered shell
type boardFixture struct {
	t     *testing.T
	model *BoardModel
	mu    sync.Mutex
	// listed holds the tasks per project; edit it (under mu) and poll to change the board.
	listed map[string][]tuiclient.Task
	// listErr, when set, fails every ListTasks call.
	listErr error
	// prs maps a task id to the pr link GetTask reports for it.
	prs map[string]string
	// calls records the mutating client calls and merges; lists records each ListTasks
	// call; eventCalls records the ListEvents task ids.
	calls      []string
	lists      []listCall
	eventCalls []string
	// ticks counts the poll ticks the model armed.
	ticks int
}

// listCall is one ListTasks call the fixture's client received.
type listCall struct {
	project   string
	state     string
	fields    string
	claimable bool
}

// tickArmedMsg is what a recorded tick produces in place of waiting for the poll interval.
type tickArmedMsg struct{}

// maxFixtureMsgs bounds send, so a command chain that never goes quiet fails the test.
const maxFixtureMsgs = 1000

// newBoardFixture builds a shell board of the given size for project p1 ("One") holding
// tasks, and delivers the initial size, project list and task fetch.
func newBoardFixture(t *testing.T, width, height int, tasks ...tuiclient.Task) *boardFixture {
	t.Helper()
	f := &boardFixture{
		t:      t,
		listed: map[string][]tuiclient.Task{"p1": tasks},
		prs:    map[string]string{},
	}
	rec := func(s string) {
		f.mu.Lock()
		f.calls = append(f.calls, s)
		f.mu.Unlock()
	}
	client := &tuiclient.MockClient{
		ListProjectsFunc: func(ctx context.Context, o ...tuiclient.ProjectListOption) ([]tuiclient.Project, error) {
			return []tuiclient.Project{{ID: "p1", Name: "One"}}, nil
		},
		ListTasksFunc: func(ctx context.Context, projectID string, opts ...tuiclient.TaskListOption) ([]tuiclient.Task, error) {
			var o tuiclient.TaskListOptions
			for _, opt := range opts {
				opt(&o)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			f.lists = append(f.lists, listCall{project: projectID, state: o.State, fields: o.Fields, claimable: o.Claimable})
			if f.listErr != nil {
				return nil, f.listErr
			}
			var out []tuiclient.Task
			for _, task := range f.listed[projectID] {
				if o.State != "" && task.State != o.State {
					continue
				}
				if o.Claimable && task.State != "ready" {
					continue
				}
				out = append(out, task)
			}
			return out, nil
		},
		GetTaskFunc: func(ctx context.Context, id string) (tuiclient.TaskDetail, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			d := tuiclient.TaskDetail{ID: id}
			if pr, ok := f.prs[id]; ok {
				d.Links = []tuiclient.TaskLink{{Kind: "pr", Value: pr}}
			}
			for _, task := range f.listed[f.model.project.ID] {
				if task.ID == id {
					d.Title, d.State = task.Title, task.State
				}
			}
			return d, nil
		},
		ListEventsFunc: func(ctx context.Context, taskID string) ([]tuiclient.Event, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.eventCalls = append(f.eventCalls, taskID)
			return nil, nil
		},
		PromoteTaskFunc: func(ctx context.Context, id string) error { rec("promote " + id); return nil },
		ReviewTaskFunc: func(ctx context.Context, id, actor, verdict string, note *string) error {
			rec("review " + verdict + " " + id)
			return nil
		},
		TransitionTaskFunc: func(ctx context.Context, id, to string, note *string) error {
			rec("transition " + id + " " + to)
			return nil
		},
		ArchiveTaskFunc:    func(ctx context.Context, id string) error { rec("archive " + id); return nil },
		ArchiveProjectFunc: func(ctx context.Context, id string) error { rec("archive-project " + id); return nil },
		ReleaseTaskFunc:    func(ctx context.Context, id string) error { rec("release " + id); return nil },
		HoldTaskFunc:       func(ctx context.Context, id string) error { rec("hold " + id); return nil },
		SetTaskPriorityFunc: func(ctx context.Context, id string, req tuiclient.SetTaskPriorityRequest) (tuiclient.PriorityChange, error) {
			rec("priority " + id)
			return tuiclient.PriorityChange{Priority: req.Priority}, nil
		},
		MoveTaskToFrontFunc: func(ctx context.Context, id string, req tuiclient.MoveTaskToFrontRequest) (tuiclient.PriorityChange, error) {
			rec("front " + id)
			return tuiclient.PriorityChange{}, nil
		},
	}
	cfg := &tuiconfig.Config{URL: "http://test", Token: "t", Actor: "u", PollInterval: time.Hour}
	f.model = NewBoardModel(client, cfg, tuiclient.Project{ID: "p1", Name: "One"})
	f.model.shell.previewDebounce = 0
	f.model.newTickCmd = func() tea.Cmd {
		f.ticks++
		return func() tea.Msg { return tickArmedMsg{} }
	}
	f.model.ghMerger = func(ctx context.Context, prURL string) error { rec("merge " + prURL); return nil }
	f.send(tea.WindowSizeMsg{Width: width, Height: height})
	f.send(projectsMsg())
	f.send(f.run(f.model.fetchShellTasks())...)
	return f
}

func projectsMsg() tea.Msg {
	return projectsFetchedMsg{projects: []tuiclient.Project{{ID: "p1", Name: "One"}, {ID: "p2", Name: "Two"}}}
}

// run executes a command and returns its message(s), flattening batches.
func (f *boardFixture) run(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, f.run(c)...)
		}
		return out
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

// step delivers one message and returns the messages its command produced, undelivered.
func (f *boardFixture) step(msg tea.Msg) []tea.Msg {
	_, cmd := f.model.Update(msg)
	return f.run(cmd)
}

// send delivers messages and every message their commands produce, until quiet. A recorded
// tick is not delivered: the next poll happens only when a test sends tickMsg.
func (f *boardFixture) send(msgs ...tea.Msg) {
	f.t.Helper()
	queue := append([]tea.Msg{}, msgs...)
	for n := 0; len(queue) > 0; n++ {
		if n == maxFixtureMsgs {
			f.t.Fatalf("send: no quiet after %d messages, next %T", n, queue[0])
		}
		msg := queue[0]
		queue = queue[1:]
		if _, ok := msg.(tickArmedMsg); ok {
			continue
		}
		queue = append(queue, f.step(msg)...)
	}
}

// press sends keys; each element is one key, except text, which is typed per character.
func (f *boardFixture) press(keys ...string) {
	f.t.Helper()
	for _, k := range keys {
		if len(k) > 1 && k != "enter" && k != "tab" && k != "down" && k != "up" {
			for _, ch := range k {
				f.send(key(string(ch)))
			}
			continue
		}
		f.send(key(k))
	}
}

// selectItem moves the items-pane selection to the task.
func (f *boardFixture) selectItem(t *testing.T, id string) {
	t.Helper()
	n := len(f.model.shell.items.items)
	for i := 0; i < n && f.model.shell.items.SelectedTaskID() != id; i++ {
		f.press("down")
	}
	for i := 0; i < n && f.model.shell.items.SelectedTaskID() != id; i++ {
		f.press("up")
	}
	if got := f.model.shell.items.SelectedTaskID(); got != id {
		t.Fatalf("could not select %s, selected %q", id, got)
	}
}

// setTasks replaces the tasks the client lists for project p1; send tickMsg to poll them.
func (f *boardFixture) setTasks(tasks ...tuiclient.Task) {
	f.mu.Lock()
	f.listed["p1"] = tasks
	f.mu.Unlock()
}

// got is the recorded mutating calls, in order, joined by "; ".
func (f *boardFixture) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, "; ")
}

// listCalls returns the ListTasks calls recorded since the last resetCalls.
func (f *boardFixture) listCalls() []listCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]listCall{}, f.lists...)
}

// resetCalls forgets every recorded call and tick.
func (f *boardFixture) resetCalls() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.lists, f.eventCalls, f.ticks = nil, nil, nil, 0
}

func key(s string) tea.KeyPressMsg {
	switch s {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	return tea.KeyPressMsg{Code: rune(s[0]), Text: s}
}

// newShellFixture is the board at the given size with three p1 tasks and one p2 task.
func newShellFixture(t *testing.T, width, height int) *boardFixture {
	t.Helper()
	f := newBoardFixture(t, width, height,
		tuiclient.Task{ID: "a1", Title: "Alpha", State: "in_progress", Kind: "implement", Model: "sonnet", CreatedAt: "2026-01-01T00:00:00Z"},
		tuiclient.Task{ID: "a2", Title: "Bravo", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"},
		tuiclient.Task{ID: "a3", Title: "Charlie", State: "blocked", Kind: "implement", CreatedAt: "2026-01-01T00:00:02Z"},
	)
	f.mu.Lock()
	f.listed["p2"] = []tuiclient.Task{{ID: "b1", Title: "Delta", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"}}
	f.mu.Unlock()
	return f
}

// newActionFixture is the board with one implement task per actionable state, each with a pr link.
func newActionFixture(t *testing.T) *boardFixture {
	t.Helper()
	tasks := []tuiclient.Task{
		{ID: "t-backlog", Title: "Backlog", State: "backlog", Kind: "implement", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "t-ready", Title: "Ready", State: "ready", Kind: "implement", CreatedAt: "2026-01-01T00:00:01Z"},
		{ID: "t-review", Title: "Review", State: "review", Kind: "implement", CreatedAt: "2026-01-01T00:00:02Z"},
		{ID: "t-approved", Title: "Approved", State: "approved", Kind: "implement", CreatedAt: "2026-01-01T00:00:03Z"},
		{ID: "t-blocked", Title: "Blocked", State: "blocked", Kind: "implement", CreatedAt: "2026-01-01T00:00:04Z"},
		{ID: "t-held", Title: "Held", State: "ready", Held: true, Kind: "implement", CreatedAt: "2026-01-01T00:00:05Z"},
	}
	f := newBoardFixture(t, 140, 40, tasks...)
	for _, task := range tasks {
		f.prs[task.ID] = "https://github.com/o/r/pull/9"
	}
	return f
}
