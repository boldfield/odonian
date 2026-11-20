package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMigration0035ChecksState(t *testing.T) {
	legacyFS := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0035" {
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
	s := &sqliteStore{conn: conn}
	if err := s.migrate(legacyFS); err != nil {
		t.Fatalf("migrate to 0034: %v", err)
	}

	for _, stmt := range []string{
		`INSERT INTO project (id, name, repo, created_at) VALUES ('p', 'p', 'r', 't')`,
		`INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES ('d', 'p', 'design', 't', 'r', 't', 't')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	states := []string{"backlog", "ready", "in_progress", "review", "approved", "done", "blocked", "failed", "superseded", "abandoned"}
	for i, state := range states {
		id := "t-" + state
		if _, err := conn.Exec(`INSERT INTO task (id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind,
			review_models, review_round, target_task_id, verdict, agent_merge, held, created_at, updated_at, archived_at, superseded_by,
			escalate, track, adjudicate_finding_round, adjudicate_finding_reviewer_model, adjudicate_finding_reviewer_slot,
			adjudicate_finding_id, branch, landing_round, landing_commit, landing_attempt, priority, replaces_review_task_id)
			VALUES (?, 'p', 'd', 'title', 'spec', ?, 'agent', 'lease', 'result', 'sonnet', 'implement',
			'["opus"]', ?, NULL, 'verdict', 1, 1, 'c', 'u', 'arch', 'sup', 0, 'build', ?, 'opus', 2, ?, 'mr/x', 3, 'sha', 'att', ?, 'rep')`,
			id, state, i+1, i+1, "finding-"+state, 100+i); err != nil {
			t.Fatalf("seed %s: %v", state, err)
		}
	}
	if _, err := conn.Exec(`INSERT INTO task (id, project_id, document_id, title, spec, state, target_task_id, created_at, updated_at)
		VALUES ('child', 'p', 'd', 't', 's', 'ready', 't-ready', 't', 't')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES ('t-checks', 'p', 'd', 't', 's', 'checks', 't', 't')`); err == nil {
		t.Fatal("state 'checks' must be rejected before 0035")
	}

	columns := []string{"id", "project_id", "document_id", "title", "spec", "state", "assignee", "lease_expires_at", "result", "model", "kind",
		"review_models", "review_round", "target_task_id", "verdict", "agent_merge", "held", "created_at", "updated_at", "archived_at",
		"superseded_by", "escalate", "track", "adjudicate_finding_round", "adjudicate_finding_reviewer_model", "adjudicate_finding_reviewer_slot",
		"adjudicate_finding_id", "branch", "landing_round", "landing_commit", "landing_attempt", "priority", "replaces_review_task_id"}
	snapshot := func() []string {
		rows, err := conn.Query(fmt.Sprintf(`SELECT %s FROM task ORDER BY id`, strings.Join(columns, ", ")))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			vals := make([]any, len(columns))
			ptrs := make([]any, len(columns))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%#v", vals))
		}
		return out
	}
	indexes := func() []string {
		rows, err := conn.Query(`SELECT name, sql FROM sqlite_master WHERE type='index' AND tbl_name='task' AND sql IS NOT NULL ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				t.Fatal(err)
			}
			out = append(out, name+"|"+strings.Join(strings.Fields(def), " "))
		}
		return out
	}
	beforeRows, beforeIdx := snapshot(), indexes()
	if len(beforeRows) != len(states)+1 || len(beforeIdx) == 0 {
		t.Fatalf("seed rows=%d indexes=%d", len(beforeRows), len(beforeIdx))
	}

	if err := s.migrate(migrationsFS); err != nil {
		t.Fatalf("migrate to 0035: %v", err)
	}

	afterRows, afterIdx := snapshot(), indexes()
	if !reflect.DeepEqual(beforeRows, afterRows) {
		t.Fatalf("task rows changed:\nbefore %v\nafter  %v", beforeRows, afterRows)
	}
	sort.Strings(beforeIdx)
	sort.Strings(afterIdx)
	if !reflect.DeepEqual(beforeIdx, afterIdx) {
		t.Fatalf("indexes changed:\nbefore %v\nafter  %v", beforeIdx, afterIdx)
	}

	var nonDefault int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM task WHERE await_checks != 0 OR checks_started_at IS NOT NULL`).Scan(&nonDefault); err != nil {
		t.Fatal(err)
	}
	if nonDefault != 0 {
		t.Fatalf("%d migrated rows have await_checks/checks_started_at set", nonDefault)
	}

	if _, err := conn.Exec(`INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at, await_checks, checks_started_at)
		VALUES ('t-checks', 'p', 'd', 't', 's', 'checks', 't', 't', 1, 'now')`); err != nil {
		t.Fatalf("state 'checks' must be accepted after 0035: %v", err)
	}
	if _, err := conn.Exec(`INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES ('t-bogus', 'p', 'd', 't', 's', 'bogus', 't', 't')`); err == nil {
		t.Fatal("unknown state must still be rejected")
	}
}
