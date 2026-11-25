package store

import (
	"context"
	"strings"
	"testing"
)

// supersedeFixture creates one task with the given spec/track and returns the store and task id.
func supersedeFixture(t *testing.T, spec, track string) (*sqliteStore, context.Context, string) {
	t.Helper()
	ctx := context.Background()
	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	proj, err := st.CreateProject(ctx, "P", "repo")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	doc, err := st.CreateDocument(ctx, proj.ID, "feature_spec", "Doc", "main", nil)
	if err != nil {
		t.Fatalf("document: %v", err)
	}
	tasks, err := st.CreateTasks(ctx, proj.ID, []TaskInput{{Title: "T", Spec: spec, DocumentID: doc.ID, Track: track}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return st.(*sqliteStore), ctx, tasks[0].ID
}

func addFeedbackEvent(t *testing.T, st *sqliteStore, ctx context.Context, taskID, actor, kind string, verdict *string, note string) {
	t.Helper()
	tx, err := st.Conn().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := st.AppendEvent(ctx, tx, taskID, actor, kind, verdict, &note); err != nil {
		tx.Rollback()
		t.Fatalf("event: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func addPRLink(t *testing.T, st *sqliteStore, ctx context.Context, taskID, url string, tombstone bool) {
	t.Helper()
	id := GenerateID()
	if _, err := st.Conn().ExecContext(ctx, `INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'pr', ?)`, id, taskID, url); err != nil {
		t.Fatalf("link: %v", err)
	}
	if tombstone {
		if err := st.TombstoneLink(ctx, taskID, id); err != nil {
			t.Fatalf("tombstone: %v", err)
		}
	}
}

const supersedeEntry = "**rev (verdict: reject)**\nbad thing\n\n"

func TestSupersedePreambleWithPRLink(t *testing.T) {
	st, ctx, id := supersedeFixture(t, "Spec", "")
	reject := "reject"
	addFeedbackEvent(t, st, ctx, id, "rev", "review", &reject, "bad thing")
	url := "https://github.com/o/r/pull/255"
	addPRLink(t, st, ctx, id, url, false)
	repl, err := st.SupersedeTask(ctx, id, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Spec\n\n## Prior attempt feedback\n\n" + priorAttemptFeedbackPreamble(id, url) + supersedeEntry
	if repl.Spec != want {
		t.Errorf("spec = %q, want %q", repl.Spec, want)
	}
	for _, s := range []string{id, url, "do not block because it is closed"} {
		if !strings.Contains(repl.Spec, s) {
			t.Errorf("spec lacks %q", s)
		}
	}
}

func TestSupersedePreambleWithoutOrTombstonedPRLink(t *testing.T) {
	for _, tombstoned := range []bool{false, true} {
		st, ctx, id := supersedeFixture(t, "Spec", "")
		reject := "reject"
		addFeedbackEvent(t, st, ctx, id, "rev", "review", &reject, "bad thing")
		if tombstoned {
			addPRLink(t, st, ctx, id, "https://github.com/o/r/pull/9", true)
		}
		repl, err := st.SupersedeTask(ctx, id, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := "Spec\n\n## Prior attempt feedback\n\n" + priorAttemptFeedbackPreamble(id, "") + supersedeEntry
		if repl.Spec != want {
			t.Errorf("tombstoned=%v spec = %q, want %q", tombstoned, repl.Spec, want)
		}
		if strings.Contains(repl.Spec, "pull/9") || strings.Contains(repl.Spec, "()") || strings.Contains(repl.Spec, "pull request (") {
			t.Errorf("tombstoned=%v: dangling URL clause in %q", tombstoned, repl.Spec)
		}
	}
}

func TestSupersedeChecksFailedPreamble(t *testing.T) {
	st, ctx, id := supersedeFixture(t, "Spec", "")
	addFeedbackEvent(t, st, ctx, id, "board", "checks_failed", nil, "lint failed")
	repl, err := st.SupersedeTask(ctx, id, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "Spec\n\n## Prior attempt feedback\n\n" + priorAttemptFeedbackPreamble(id, "") + "**board (checks failed)**\nlint failed\n\n"
	if repl.Spec != want {
		t.Errorf("spec = %q, want %q", repl.Spec, want)
	}
}

func TestSupersedeResearchHasNoPreamble(t *testing.T) {
	st, ctx, id := supersedeFixture(t, "Spec", "research")
	reject := "reject"
	addFeedbackEvent(t, st, ctx, id, "rev", "review", &reject, "bad thing")
	repl, err := st.SupersedeTask(ctx, id, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(repl.Spec, "This task replaces task") {
		t.Errorf("research spec has preamble: %q", repl.Spec)
	}
}

func TestSupersedeReplacementKeepsOldBlockUntouched(t *testing.T) {
	st, ctx, id := supersedeFixture(t, "Spec", "")
	reject := "reject"
	addFeedbackEvent(t, st, ctx, id, "rev", "review", &reject, "bad thing")
	repl1, err := st.SupersedeTask(ctx, id, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	addFeedbackEvent(t, st, ctx, repl1.ID, "rev2", "review", &reject, "worse thing")
	repl2, err := st.SupersedeTask(ctx, repl1.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(repl2.Spec, repl1.Spec+"\n\n## Prior attempt feedback\n\n"+priorAttemptFeedbackPreamble(repl1.ID, "")) {
		t.Errorf("unexpected spec: %q", repl2.Spec)
	}
	if strings.Count(repl2.Spec, "This task replaces task") != 2 {
		t.Errorf("want exactly two preambles: %q", repl2.Spec)
	}
}
