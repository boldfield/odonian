package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"testing/fstest"
)

type priorityFixture struct {
	t         *testing.T
	ctx       context.Context
	dbPath    string
	store     Store
	projectID string
	docID     string
	sequence  int
}

func newPriorityFixture(t *testing.T) *priorityFixture {
	t.Helper()
	f := &priorityFixture{t: t, ctx: context.Background(), dbPath: filepath.Join(t.TempDir(), "priority.db")}
	f.open()
	t.Cleanup(func() { f.store.Close() })
	project, err := f.store.CreateProject(f.ctx, "priority-project", "https://example.com/repo")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	f.projectID = project.ID
	f.docID = f.newDocument(project.ID)
	return f
}

func (f *priorityFixture) open() {
	f.t.Helper()
	store, err := Open(f.dbPath, defaultTestAllowedModels())
	if err != nil {
		f.t.Fatalf("open store: %v", err)
	}
	f.store = store
}

func (f *priorityFixture) reopen() {
	f.t.Helper()
	if err := f.store.Close(); err != nil {
		f.t.Fatalf("close store: %v", err)
	}
	f.open()
}

func (f *priorityFixture) newDocument(projectID string) string {
	f.t.Helper()
	doc, err := f.store.CreateDocument(f.ctx, projectID, "design", "doc", "DESIGN.md", nil)
	if err != nil {
		f.t.Fatalf("create document: %v", err)
	}
	return doc.ID
}

func (f *priorityFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.Conn().ExecContext(f.ctx, query, args...); err != nil {
		f.t.Fatalf("exec %q: %v", query, err)
	}
}

// task creates an independent topic in the given state with a raw stored priority, which
// may exceed 1000 the way a generated or reloaded value does.
func (f *priorityFixture) task(state string, priority int64) string {
	f.t.Helper()
	return f.taskIn(f.projectID, f.docID, state, priority)
}

func (f *priorityFixture) taskIn(projectID, docID, state string, priority int64) string {
	f.t.Helper()
	f.sequence++
	created, err := f.store.CreateTasks(f.ctx, projectID, []TaskInput{{Title: fmt.Sprintf("task-%d", f.sequence), Spec: "spec", DocumentID: docID}})
	if err != nil {
		f.t.Fatalf("create task: %v", err)
	}
	f.exec(`UPDATE task SET state = ?, priority = ? WHERE id = ?`, state, priority, created[0].ID)
	return created[0].ID
}

func (f *priorityFixture) get(id string) TaskWithDepsAndLinks {
	f.t.Helper()
	got, err := f.store.GetTask(f.ctx, id)
	if err != nil {
		f.t.Fatalf("get task %s: %v", id, err)
	}
	return got
}

func (f *priorityFixture) priorityOf(id string) int64 {
	f.t.Helper()
	return f.get(id).Priority
}

func (f *priorityFixture) front(key, taskID string) PriorityChange {
	f.t.Helper()
	change, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: key, TaskID: taskID, Actor: "operator", Reason: "urgent"})
	if err != nil {
		f.t.Fatalf("front %s: %v", key, err)
	}
	return change
}

func (f *priorityFixture) set(key, taskID string, priority int64) PriorityChange {
	f.t.Helper()
	change, err := f.store.SetTaskPriority(f.ctx, SetPriorityRequest{ActionKey: key, TaskID: taskID, Priority: priority, Actor: "operator", Reason: "adjust"})
	if err != nil {
		f.t.Fatalf("set %s: %v", key, err)
	}
	return change
}

func (f *priorityFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.store.Conn().QueryRowContext(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

// ledger counts the durable side effects of priority actions so a rejected action can be
// shown to have changed nothing.
func (f *priorityFixture) ledger() [3]int {
	f.t.Helper()
	return [3]int{
		f.count(`SELECT COUNT(*) FROM priority_action`),
		f.count(`SELECT COUNT(*) FROM event WHERE kind = 'priority'`),
		f.count(`SELECT COALESCE(SUM(priority % 1000003), 0) FROM task`),
	}
}

func requireConflictCode(t *testing.T, err error, code string) {
	t.Helper()
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) || conflictErr.Code != code {
		t.Fatalf("expected conflict %s, got %v", code, err)
	}
}

func TestPriorityDefaultsOnFreshDatabase(t *testing.T) {
	f := newPriorityFixture(t)
	created, err := f.store.CreateTasks(f.ctx, f.projectID, []TaskInput{{Title: "omitted", Spec: "s", DocumentID: f.docID}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created[0].Priority != 500 {
		t.Fatalf("created priority = %d, want 500", created[0].Priority)
	}
	got := f.get(created[0].ID)
	if got.Priority != 500 || got.TopicAnchorID != created[0].ID {
		t.Fatalf("get priority=%d anchor=%s, want 500 and own id", got.Priority, got.TopicAnchorID)
	}
	listed, err := f.store.ListTasks(f.ctx, f.projectID, TaskListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Priority != 500 || listed[0].TopicAnchorID != listed[0].ID {
		t.Fatalf("listed = %+v", listed)
	}
}

func TestPriorityDefaultsOnMigratedDatabase(t *testing.T) {
	legacyFS := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0032" {
			continue
		}
		data, err := fs.ReadFile(migrationsFS, "migrations/"+entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		legacyFS["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}

	conn, err := sql.Open("sqlite", buildDSN(filepath.Join(t.TempDir(), "legacy.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetMaxOpenConns(1)
	legacy := &sqliteStore{conn: conn}
	if err := legacy.migrate(legacyFS); err != nil {
		t.Fatalf("legacy migrate: %v", err)
	}
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO project (id, name, repo, created_at) VALUES ('p', 'p', 'r', 't')`,
		`INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES ('d', 'p', 'design', 't', 'r', 't', 't')`,
		`INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at) VALUES ('old', 'p', 'd', 't', 's', 'ready', 't', 't')`,
	} {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("seed legacy row: %v", err)
		}
	}
	if err := legacy.migrate(migrationsFS); err != nil {
		t.Fatalf("upgrade migrate: %v", err)
	}

	var priority int64
	var anchor sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT priority, topic_anchor_id FROM task WHERE id = 'old'`).Scan(&priority, &anchor); err != nil {
		t.Fatal(err)
	}
	if priority != 500 || anchor.Valid {
		t.Fatalf("migrated row priority=%d anchor=%v, want 500 and NULL", priority, anchor)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE task SET priority = 0 WHERE id = 'old'`); err == nil {
		t.Fatal("priority below 1 must violate the column CHECK")
	}
}

func TestCreateTasksPriorityBoundaries(t *testing.T) {
	f := newPriorityFixture(t)
	for _, accepted := range []int64{1, 500, 1000} {
		value := accepted
		created, err := f.store.CreateTasks(f.ctx, f.projectID, []TaskInput{{Title: "t", Spec: "s", DocumentID: f.docID, Priority: &value}})
		if err != nil {
			t.Fatalf("priority %d rejected: %v", accepted, err)
		}
		if created[0].Priority != accepted || f.priorityOf(created[0].ID) != accepted {
			t.Fatalf("priority %d not persisted", accepted)
		}
	}
	before := f.count(`SELECT COUNT(*) FROM task`)
	for _, rejected := range []int64{0, -1, 1001, math.MaxInt64, math.MinInt64} {
		value := rejected
		_, err := f.store.CreateTasks(f.ctx, f.projectID, []TaskInput{
			{Title: "valid sibling", Spec: "s", DocumentID: f.docID},
			{Title: "t", Spec: "s", DocumentID: f.docID, Priority: &value},
		})
		requireValidationCode(t, err, "INVALID_PRIORITY")
	}
	if after := f.count(`SELECT COUNT(*) FROM task`); after != before {
		t.Fatalf("rejected batches left %d new tasks", after-before)
	}
}

func TestPriorityJSONBoundaryRejectsFractionsAndOverflow(t *testing.T) {
	for _, raw := range []string{`{"priority":1.5}`, `{"priority":1000.0}`, `{"priority":1e30}`, `{"priority":9223372036854775808}`, `{"priority":"500"}`} {
		var input TaskInput
		if err := json.Unmarshal([]byte(raw), &input); err == nil {
			t.Fatalf("%s decoded to %v, want an error", raw, input.Priority)
		}
	}
	var input TaskInput
	if err := json.Unmarshal([]byte(`{"priority":1000}`), &input); err != nil || input.Priority == nil || *input.Priority != 1000 {
		t.Fatalf("1000 decode = %v, %v", input.Priority, err)
	}
	if err := json.Unmarshal([]byte(`{"title":"x"}`), &input); err != nil {
		t.Fatal(err)
	}
}

func TestSetPriorityValidatesManualRange(t *testing.T) {
	f := newPriorityFixture(t)
	id := f.task("ready", 500)
	for _, accepted := range []int64{1, 1000, 500} {
		f.set(fmt.Sprintf("accept-%d", accepted), id, accepted)
		if got := f.priorityOf(id); got != accepted {
			t.Fatalf("priority = %d, want %d", got, accepted)
		}
	}
	before := f.ledger()
	for _, rejected := range []int64{0, -1, 1001, math.MaxInt64, math.MinInt64} {
		_, err := f.store.SetTaskPriority(f.ctx, SetPriorityRequest{ActionKey: fmt.Sprintf("reject-%d", rejected), TaskID: id, Priority: rejected, Actor: "operator", Reason: "r"})
		requireValidationCode(t, err, "INVALID_PRIORITY")
	}
	if after := f.ledger(); after != before {
		t.Fatalf("rejected sets changed state: %v -> %v", before, after)
	}
}

func TestPriorityActionInputValidation(t *testing.T) {
	f := newPriorityFixture(t)
	id := f.task("ready", 500)
	_, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{TaskID: id, Actor: "a", Reason: "r"})
	requireValidationCode(t, err, "INVALID_ACTION_KEY")
	_, err = f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "k", TaskID: id, Reason: "r"})
	requireValidationCode(t, err, "ACTOR_REQUIRED")
	_, err = f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "k", TaskID: id, Actor: "a", Reason: "  "})
	requireValidationCode(t, err, "REASON_REQUIRED")
	_, err = f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "k", TaskID: "missing", Actor: "a", Reason: "r"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task error = %v", err)
	}
	f.exec(`UPDATE task SET archived_at = '2026-01-01T00:00:00Z' WHERE id = ?`, id)
	_, err = f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "k", TaskID: id, Actor: "a", Reason: "r"})
	requireConflictCode(t, err, "ARCHIVED")
	_, err = f.store.SetTaskPriority(f.ctx, SetPriorityRequest{ActionKey: "k", TaskID: id, Priority: 5, Actor: "a", Reason: "r"})
	requireConflictCode(t, err, "ARCHIVED")
	if f.ledger() != [3]int{0, 0, 500} {
		t.Fatalf("rejected actions left state: %v", f.ledger())
	}
}

func TestFrontComputesFlooredQueueMaximum(t *testing.T) {
	cases := []struct {
		name      string
		queueMax  int64
		wantFront int64
	}{
		{"queue maximum 500", 500, 1001},
		{"queue maximum 730", 730, 1001},
		{"queue maximum 1000", 1000, 1001},
		{"inherited maximum 1042", 1042, 1043},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPriorityFixture(t)
			selected := f.task("ready", 500)
			f.task("ready", tc.queueMax)
			change := f.front("front-1", selected)
			if change.Priority != tc.wantFront || change.OldPriority != 500 || change.Action != PriorityActionFront {
				t.Fatalf("change = %+v, want front %d", change, tc.wantFront)
			}
			if change.QueueMaxPriority == nil || *change.QueueMaxPriority != tc.queueMax {
				t.Fatalf("queue max = %v, want %d", change.QueueMaxPriority, tc.queueMax)
			}
			if got := f.priorityOf(selected); got != tc.wantFront {
				t.Fatalf("stored priority = %d, want %d", got, tc.wantFront)
			}
		})
	}
}

func TestFrontIsNotOvertakenByManualPriorities(t *testing.T) {
	f := newPriorityFixture(t)
	first := f.task("ready", 500)
	if got := f.front("front-first", first).Priority; got != 1001 {
		t.Fatalf("first front = %d, want 1001", got)
	}
	later505 := f.task("ready", 500)
	later1000 := f.task("ready", 500)
	f.set("set-505", later505, 505)
	f.set("set-1000", later1000, 1000)

	listed, err := f.store.ListTasks(f.ctx, f.projectID, TaskListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].ID != first || listed[1].ID != later1000 || listed[2].ID != later505 {
		t.Fatalf("order = %s %s %s, want fronted, 1000, 505", listed[0].ID, listed[1].ID, listed[2].ID)
	}
	if got := f.front("front-second", later505).Priority; got != 1002 {
		t.Fatalf("subsequent front = %d, want 1002", got)
	}
	listed, _ = f.store.ListTasks(f.ctx, f.projectID, TaskListFilter{})
	if listed[0].ID != later505 || listed[1].ID != first {
		t.Fatalf("a subsequent front must overtake: %s %s", listed[0].ID, listed[1].ID)
	}
}

func TestFrontCountsEveryOutstandingTopicServerWide(t *testing.T) {
	cases := []struct {
		name  string
		state string
		held  bool
	}{
		{"backlog", "backlog", false},
		{"held", "ready", true},
		{"blocked", "blocked", false},
		{"in flight", "in_progress", false},
		{"waiting review", "review", false},
		{"approved", "approved", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPriorityFixture(t)
			selected := f.task("ready", 500)
			hidden := f.task(tc.state, 1042)
			if tc.held {
				f.exec(`UPDATE task SET held = 1 WHERE id = ?`, hidden)
			}
			if got := f.front("k", selected).Priority; got != 1043 {
				t.Fatalf("front = %d, want 1043 (the %s topic at 1042 must count)", got, tc.name)
			}
		})
	}

	t.Run("other project", func(t *testing.T) {
		f := newPriorityFixture(t)
		otherProject, err := f.store.CreateProject(f.ctx, "other", "https://example.com/other")
		if err != nil {
			t.Fatal(err)
		}
		f.taskIn(otherProject.ID, f.newDocument(otherProject.ID), "ready", 1042)
		if got := f.front("k", f.task("ready", 500)).Priority; got != 1043 {
			t.Fatalf("front = %d, want 1043 from a task in another project", got)
		}
	})
}

func TestFrontExcludesFinishedAndArchivedTopics(t *testing.T) {
	f := newPriorityFixture(t)
	selected := f.task("ready", 500)
	for _, state := range []string{"done", "failed", "abandoned", "superseded"} {
		f.task(state, 5000)
	}
	archived := f.task("ready", 6000)
	f.exec(`UPDATE task SET archived_at = '2026-01-01T00:00:00Z' WHERE id = ?`, archived)
	if got := f.front("k", selected).Priority; got != 1001 {
		t.Fatalf("front = %d, want 1001: finished and archived topics must not inflate the maximum", got)
	}
}

func TestFrontRejectsTopicWithNoOutstandingTask(t *testing.T) {
	f := newPriorityFixture(t)
	finished := f.task("done", 500)
	f.task("ready", 800)
	before := f.ledger()
	_, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "k", TaskID: finished, Actor: "operator", Reason: "r"})
	requireConflictCode(t, err, "TOPIC_NOT_OUTSTANDING")
	if after := f.ledger(); after != before {
		t.Fatalf("rejected front changed state: %v -> %v", before, after)
	}

	// A normal setter may still reset a retained, finished anchor without reviving it.
	f.set("reset", finished, 500)
	if got := f.get(finished); got.State != "done" || got.Priority != 500 {
		t.Fatalf("reset anchor = state %s priority %d", got.State, got.Priority)
	}
}

type topicLineage struct {
	anchor, replacement, review, merge, child, grandchild, childReplacement string
	unrelated, followUp, dependent, sibling                                 string
}

func (f *priorityFixture) buildLineage() topicLineage {
	f.t.Helper()
	var l topicLineage
	l.anchor = f.task("ready", 500)
	replacement, err := f.store.SupersedeTask(f.ctx, l.anchor, nil)
	if err != nil {
		f.t.Fatalf("supersede anchor: %v", err)
	}
	l.replacement = replacement.ID

	l.review = f.task("ready", 500)
	f.exec(`UPDATE task SET kind = 'review', target_task_id = ? WHERE id = ?`, l.replacement, l.review)
	l.merge = f.task("ready", 500)
	f.exec(`UPDATE task SET kind = 'merge', target_task_id = ? WHERE id = ?`, l.replacement, l.merge)

	l.child = f.task("ready", 500)
	f.exec(`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'continuation_parent', ?)`, GenerateID(), l.child, l.replacement)
	l.grandchild = f.task("backlog", 500)
	f.exec(`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'continuation_parent', ?)`, GenerateID(), l.grandchild, l.child)
	childReplacement, err := f.store.SupersedeTask(f.ctx, l.child, nil)
	if err != nil {
		f.t.Fatalf("supersede child: %v", err)
	}
	l.childReplacement = childReplacement.ID

	l.unrelated = f.task("ready", 730)
	l.followUp = f.task("ready", 500)
	f.exec(`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'research_parent', ?)`, GenerateID(), l.followUp, l.replacement)
	l.dependent = f.task("ready", 500)
	f.exec(`INSERT INTO task_dep (task_id, depends_on_id) VALUES (?, ?)`, l.dependent, l.replacement)
	l.sibling = f.task("ready", 500)
	return l
}

func (l topicLineage) members() []string {
	return []string{l.anchor, l.replacement, l.review, l.merge, l.child, l.grandchild, l.childReplacement}
}

func (l topicLineage) outsiders() []string {
	return []string{l.unrelated, l.followUp, l.dependent, l.sibling}
}

func TestTopicLineageResolvesAnchorAndCoversTheWholeTopic(t *testing.T) {
	f := newPriorityFixture(t)
	l := f.buildLineage()

	// Pre-feature lineage: nothing recorded, yet every descendant resolves to the root.
	for _, member := range l.members() {
		if got := f.get(member).TopicAnchorID; got != l.anchor {
			t.Fatalf("anchor of %s = %s, want %s", member, got, l.anchor)
		}
	}
	for _, outsider := range l.outsiders() {
		if got := f.get(outsider).TopicAnchorID; got != outsider {
			t.Fatalf("outsider %s resolved to anchor %s", outsider, got)
		}
	}

	change := f.front("front-via-grandchild", l.grandchild)
	if change.TopicAnchorID != l.anchor || change.TaskID != l.grandchild || change.Priority != 1001 {
		t.Fatalf("change = %+v", change)
	}
	for _, member := range l.members() {
		got := f.get(member)
		if got.Priority != 1001 || got.TopicAnchorID != l.anchor {
			t.Fatalf("member %s priority=%d anchor=%s", member, got.Priority, got.TopicAnchorID)
		}
	}
	wantOutsider := map[string]int64{l.unrelated: 730, l.followUp: 500, l.dependent: 500, l.sibling: 500}
	for outsider, want := range wantOutsider {
		if got := f.priorityOf(outsider); got != want {
			t.Fatalf("outsider %s priority = %d, want %d", outsider, got, want)
		}
	}
	listed, err := f.store.ListTasks(f.ctx, f.projectID, TaskListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range listed {
		for _, member := range l.members() {
			if task.ID == member && (task.Priority != 1001 || task.TopicAnchorID != l.anchor) {
				t.Fatalf("listed member %s priority=%d anchor=%s", task.ID, task.Priority, task.TopicAnchorID)
			}
		}
	}

	f.set("reset-via-review", l.review, 500)
	for _, member := range l.members() {
		if got := f.priorityOf(member); got != 500 {
			t.Fatalf("member %s priority after reset = %d", member, got)
		}
	}
	if got := f.priorityOf(l.unrelated); got != 730 {
		t.Fatalf("unrelated topic changed to %d", got)
	}
}

func TestFrontCountsFinishedAnchorThroughActiveDescendant(t *testing.T) {
	f := newPriorityFixture(t)
	selected := f.task("ready", 700)

	doneAnchor := f.task("done", 1042)
	activeChild := f.task("ready", 500)
	f.exec(`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'continuation_parent', ?)`, GenerateID(), activeChild, doneAnchor)

	finishedAnchor := f.task("done", 9000)
	finishedReview := f.task("done", 9000)
	f.exec(`UPDATE task SET kind = 'review', target_task_id = ? WHERE id = ?`, finishedAnchor, finishedReview)

	if got := f.front("k", selected).Priority; got != 1043 {
		t.Fatalf("front = %d, want 1043: a done anchor with an active descendant counts, a wholly finished topic does not", got)
	}
}

func TestFrontLeavesLeasesHoldsAndStatesUntouched(t *testing.T) {
	f := newPriorityFixture(t)
	held := f.task("in_progress", 500)
	f.exec(`UPDATE task SET held = 1, assignee = 'worker-1', lease_expires_at = '2099-01-01T00:00:00Z', result = 'partial' WHERE id = ?`, held)
	review := f.task("ready", 500)
	f.exec(`UPDATE task SET kind = 'review', target_task_id = ? WHERE id = ?`, held, review)
	f.task("ready", 800)

	snapshot := func() string {
		rows, err := f.store.Conn().QueryContext(f.ctx, `SELECT id, state, held, assignee, lease_expires_at, result, review_round, verdict, created_at, updated_at, archived_at FROM task ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out string
		for rows.Next() {
			var id, state string
			var heldFlag bool
			var assignee, lease, result, verdict, archived sql.NullString
			var round int
			var created, updated string
			if err := rows.Scan(&id, &state, &heldFlag, &assignee, &lease, &result, &round, &verdict, &created, &updated, &archived); err != nil {
				t.Fatal(err)
			}
			out += fmt.Sprint(id, state, heldFlag, assignee, lease, result, round, verdict, created, updated, archived, "\n")
		}
		return out
	}
	before := snapshot()
	f.front("front", review)
	f.set("set", held, 3)
	if after := snapshot(); after != before {
		t.Fatalf("priority actions changed lease, hold, state or timestamps:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if f.priorityOf(held) != 3 || f.priorityOf(review) != 3 {
		t.Fatal("topic priority was not applied")
	}
}

func TestFrontIdempotentReplayReturnsOriginalResult(t *testing.T) {
	f := newPriorityFixture(t)
	selected := f.task("ready", 500)
	other := f.task("ready", 600)

	original := f.front("key-1", selected)
	f.set("later-set", selected, 500)
	eventsBefore := f.ledger()

	replayed, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "key-1", TaskID: selected, Actor: "operator", Reason: "urgent"})
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Priority != original.Priority || replayed.OldPriority != original.OldPriority || replayed.TopicAnchorID != original.TopicAnchorID {
		t.Fatalf("replay = %+v, want original %+v", replayed, original)
	}
	if f.priorityOf(selected) != 500 || f.ledger() != eventsBefore {
		t.Fatal("a replay must not change priority or add audit rows")
	}

	f.reopen()
	again, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "key-1", TaskID: selected, Actor: "operator", Reason: "urgent"})
	if err != nil || !again.Replayed || again.Priority != original.Priority {
		t.Fatalf("replay after restart = %+v, %v", again, err)
	}

	// A distinct key is a distinct action: it computes a fresh maximum and can overtake.
	second := f.front("key-2", other)
	if second.Priority != 1001 || second.Replayed {
		t.Fatalf("second action = %+v", second)
	}
	third := f.front("key-3", selected)
	if third.Priority != 1002 {
		t.Fatalf("a fresh front for an already-fronted task = %d, want 1002", third.Priority)
	}
}

func TestPriorityIdempotencyRejectsMismatchedPayload(t *testing.T) {
	f := newPriorityFixture(t)
	a := f.task("ready", 500)
	b := f.task("ready", 500)
	f.front("shared-key", a)
	before := f.ledger()

	longReason := "urgent"
	mismatches := map[string]func() error{
		"different task": func() error {
			_, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "shared-key", TaskID: b, Actor: "operator", Reason: longReason})
			return err
		},
		"different actor": func() error {
			_, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "shared-key", TaskID: a, Actor: "someone-else", Reason: longReason})
			return err
		},
		"reason differing only late": func() error {
			_, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "shared-key", TaskID: a, Actor: "operator", Reason: longReason + " but with a different tail"})
			return err
		},
		"different action": func() error {
			_, err := f.store.SetTaskPriority(f.ctx, SetPriorityRequest{ActionKey: "shared-key", TaskID: a, Priority: 10, Actor: "operator", Reason: longReason})
			return err
		},
	}
	for name, attempt := range mismatches {
		requireConflictCode(t, attempt(), "IDEMPOTENCY_MISMATCH")
		_ = name
	}
	if after := f.ledger(); after != before {
		t.Fatalf("mismatched requests changed state: %v -> %v", before, after)
	}
	if f.priorityOf(b) != 500 {
		t.Fatal("the other task must not have been touched or reported as fronted")
	}

	f.set("set-key", b, 40)
	replayed, err := f.store.SetTaskPriority(f.ctx, SetPriorityRequest{ActionKey: "set-key", TaskID: b, Priority: 40, Actor: "operator", Reason: "adjust"})
	if err != nil || !replayed.Replayed || replayed.Priority != 40 {
		t.Fatalf("set replay = %+v, %v", replayed, err)
	}
	_, err = f.store.SetTaskPriority(f.ctx, SetPriorityRequest{ActionKey: "set-key", TaskID: b, Priority: 41, Actor: "operator", Reason: "adjust"})
	requireConflictCode(t, err, "IDEMPOTENCY_MISMATCH")
}

func TestFrontOverflowFailsAtomically(t *testing.T) {
	f := newPriorityFixture(t)
	selected := f.task("ready", 500)
	f.task("ready", math.MaxInt64)
	before := f.ledger()
	_, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "k", TaskID: selected, Actor: "operator", Reason: "r"})
	requireConflictCode(t, err, "PRIORITY_OVERFLOW")
	if after := f.ledger(); after != before {
		t.Fatalf("overflow changed state: %v -> %v", before, after)
	}
	if got := f.priorityOf(selected); got != 500 {
		t.Fatalf("selected priority = %d after failed front", got)
	}
	// The failed action left no idempotency record, so the key is still usable.
	f.exec(`UPDATE task SET priority = 800 WHERE priority = ?`, int64(math.MaxInt64))
	if got := f.front("k", selected).Priority; got != 1001 {
		t.Fatalf("front after recovery = %d", got)
	}
}

func TestFrontPreservesLargeIntegersExactly(t *testing.T) {
	f := newPriorityFixture(t)
	selected := f.task("ready", 500)
	const beyondFloat53 = int64(1)<<53 + 1
	f.task("ready", beyondFloat53)
	if got := f.front("k", selected).Priority; got != beyondFloat53+1 {
		t.Fatalf("front = %d, want %d", got, beyondFloat53+1)
	}

	f.reopen()
	got := f.get(selected)
	if got.Priority != beyondFloat53+1 {
		t.Fatalf("after restart priority = %d, want %d", got.Priority, beyondFloat53+1)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TaskWithDepsAndLinks
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Priority != beyondFloat53+1 {
		t.Fatalf("JSON round trip priority = %d, %v", decoded.Priority, err)
	}

	// One below the representable maximum still fronts; the next action cannot.
	f.exec(`UPDATE task SET priority = ? WHERE id = ?`, int64(math.MaxInt64-1), selected)
	if got := f.front("edge", selected).Priority; got != math.MaxInt64 {
		t.Fatalf("front at MaxInt64-1 = %d, want MaxInt64", got)
	}
	_, err = f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "past-edge", TaskID: selected, Actor: "operator", Reason: "r"})
	requireConflictCode(t, err, "PRIORITY_OVERFLOW")
	if f.priorityOf(selected) != math.MaxInt64 {
		t.Fatal("overflow must not wrap or clamp")
	}
}

func TestFrontRestartPersistsGeneratedValues(t *testing.T) {
	f := newPriorityFixture(t)
	selected := f.task("ready", 500)
	f.task("ready", 1042)
	f.front("k", selected)
	f.reopen()
	if got := f.get(selected); got.Priority != 1043 || got.TopicAnchorID != selected {
		t.Fatalf("after restart priority=%d anchor=%s", got.Priority, got.TopicAnchorID)
	}
	listed, err := f.store.ListTasks(f.ctx, f.projectID, TaskListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if listed[0].ID != selected {
		t.Fatal("a reloaded value above 1000 must still sort first")
	}
	// A reloaded value above 1000 is valid for inheritance and a later front; it never
	// passes through the manual validator.
	other := f.task("ready", 500)
	if got := f.front("k2", other).Priority; got != 1044 {
		t.Fatalf("front after restart = %d, want 1044", got)
	}
}

func TestConcurrentFrontSerializes(t *testing.T) {
	f := newPriorityFixture(t)
	const workers = 8
	ids := make([]string, workers)
	for i := range ids {
		ids[i] = f.task("ready", 500)
	}
	results := make([]int64, workers)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			change, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: fmt.Sprintf("key-%d", i), TaskID: ids[i], Actor: "operator", Reason: "r"})
			if err != nil {
				t.Errorf("front %d: %v", i, err)
				return
			}
			results[i] = change.Priority
		}()
	}
	wg.Wait()
	sorted := append([]int64(nil), results...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	for i, got := range sorted {
		if want := int64(1001 + i); got != want {
			t.Fatalf("concurrent fronts produced %v, want 1001..%d each exactly once", sorted, 1000+workers)
		}
	}
	for i, id := range ids {
		if f.priorityOf(id) != results[i] {
			t.Fatalf("task %d stored %d but was told %d", i, f.priorityOf(id), results[i])
		}
	}

	// Concurrent retries of one action key apply it exactly once.
	target := f.task("ready", 500)
	var applied, replays int
	var mu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			change, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "retry", TaskID: target, Actor: "operator", Reason: "r"})
			if err != nil {
				t.Errorf("retry: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if change.Replayed {
				replays++
			} else {
				applied++
			}
		}()
	}
	wg.Wait()
	if applied != 1 || replays != workers-1 {
		t.Fatalf("applied=%d replays=%d", applied, replays)
	}
	if got := f.priorityOf(target); got != int64(1001+workers) {
		t.Fatalf("retried front stored %d, want %d", got, 1001+workers)
	}
}

func TestPriorityAuditRecordsStructuredFields(t *testing.T) {
	f := newPriorityFixture(t)
	selected := f.task("ready", 500)
	f.task("ready", 730)
	if _, err := f.store.MoveTaskToFront(f.ctx, FrontPriorityRequest{ActionKey: "audit-front", TaskID: selected, Actor: "alice", Reason: "board meeting"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetTaskPriority(f.ctx, SetPriorityRequest{ActionKey: "audit-set", TaskID: selected, Priority: 500, Actor: "bob", Reason: "done"}); err != nil {
		t.Fatal(err)
	}

	events, err := f.store.ListEvents(f.ctx, selected)
	if err != nil {
		t.Fatal(err)
	}
	var notes []priorityEventNote
	var actors []string
	for _, event := range events {
		if event.Kind != "priority" {
			continue
		}
		var note priorityEventNote
		if err := json.Unmarshal([]byte(*event.Note), &note); err != nil {
			t.Fatalf("priority event note is not structured JSON: %v", err)
		}
		notes = append(notes, note)
		actors = append(actors, event.Actor)
	}
	if len(notes) != 2 {
		t.Fatalf("priority events = %d, want 2", len(notes))
	}
	front, set := notes[0], notes[1]
	if front.Action != "front" || front.OldPriority != 500 || front.NewPriority != 1001 || front.Reason != "board meeting" || front.Actor != "alice" || front.TopicAnchorID != selected || front.QueueMaxPriority == nil || *front.QueueMaxPriority != 730 {
		t.Fatalf("front audit = %+v", front)
	}
	if set.Action != "set" || set.OldPriority != 1001 || set.NewPriority != 500 || set.Reason != "done" || set.Actor != "bob" || set.QueueMaxPriority != nil {
		t.Fatalf("set audit = %+v", set)
	}
	if actors[0] != "alice" || actors[1] != "bob" {
		t.Fatalf("event actors = %v", actors)
	}

	var storedOld, storedNew int64
	var storedActor, storedReason string
	if err := f.store.Conn().QueryRowContext(f.ctx, `SELECT old_priority, new_priority, actor, reason FROM priority_action WHERE action_key = 'audit-front'`).Scan(&storedOld, &storedNew, &storedActor, &storedReason); err != nil {
		t.Fatal(err)
	}
	if storedOld != 500 || storedNew != 1001 || storedActor != "alice" || storedReason != "board meeting" {
		t.Fatalf("priority_action row = %d %d %s %s", storedOld, storedNew, storedActor, storedReason)
	}
}

func TestListTasksOrdersByPriorityThenCreatedAtThenID(t *testing.T) {
	f := newPriorityFixture(t)
	type row struct {
		id       string
		priority int64
		created  string
	}
	specs := []struct {
		priority int64
		created  string
	}{
		{500, "2026-01-03T00:00:00Z"},
		{500, "2026-01-01T00:00:00Z"},
		{500, "2026-01-02T00:00:00Z"},
		{500, "2026-01-02T00:00:00Z"},
		{1043, "2026-01-05T00:00:00Z"},
		{1043, "2026-01-04T00:00:00Z"},
		{1000, "2026-01-01T00:00:00Z"},
		{1000, "2026-01-01T00:00:00Z"},
		{7, "2026-01-01T00:00:00Z"},
	}
	var rows []row
	for _, spec := range specs {
		id := f.task("ready", spec.priority)
		f.exec(`UPDATE task SET created_at = ? WHERE id = ?`, spec.created, id)
		rows = append(rows, row{id, spec.priority, spec.created})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].priority != rows[j].priority {
			return rows[i].priority > rows[j].priority
		}
		if rows[i].created != rows[j].created {
			return rows[i].created < rows[j].created
		}
		return rows[i].id < rows[j].id
	})
	listed, err := f.store.ListTasks(f.ctx, f.projectID, TaskListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != len(rows) {
		t.Fatalf("listed %d tasks, want %d", len(listed), len(rows))
	}
	for i := range rows {
		if listed[i].ID != rows[i].id {
			t.Fatalf("position %d = %s, want %s", i, listed[i].ID, rows[i].id)
		}
	}
}
