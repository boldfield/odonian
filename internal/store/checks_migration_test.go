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
	"time"
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
		t.Fatalf("migrate to latest: %v", err)
	}

	afterRows, afterIdx := snapshot(), indexes()
	if !reflect.DeepEqual(beforeRows, afterRows) {
		t.Fatalf("task rows changed:\nbefore %v\nafter  %v", beforeRows, afterRows)
	}
	sort.Strings(beforeIdx)
	sort.Strings(afterIdx)
	// Filter out indexes added by migrations after 0035 (from 0036)
	newIn0036 := map[string]bool{
		"idx_event_task":          true,
		"idx_task_link_task":      true,
		"idx_task_target":         true,
		"idx_task_dep_depends_on": true,
	}
	var afterIdxFiltered []string
	for _, idx := range afterIdx {
		idxName := strings.Split(idx, "|")[0]
		if !newIn0036[idxName] {
			afterIdxFiltered = append(afterIdxFiltered, idx)
		}
	}
	if !reflect.DeepEqual(beforeIdx, afterIdxFiltered) {
		t.Fatalf("indexes changed:\nbefore %v\nafter  %v", beforeIdx, afterIdxFiltered)
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

func TestMigration0036LatencyIndexes(t *testing.T) {
	legacyFS := fstest.MapFS{}
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() >= "0036" {
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
		t.Fatalf("migrate to 0035: %v", err)
	}

	for _, stmt := range []string{
		`INSERT INTO project (id, name, repo, created_at) VALUES ('p', 'p', 'r', 't')`,
		`INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES ('d', 'p', 'design', 't', 'r', 't', 't')`,
	} {
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	// Create seed data for testing - use large dataset similar to production
	// Create a task that will have events, links, and dependents
	taskID := "task-1"
	targetTaskID := "target-task-1"

	// Create all seed tasks (2,000 tasks to test at scale)
	for i := 0; i < 2000; i++ {
		taskNum := fmt.Sprintf("task-%d", i+1)
		if _, err := conn.Exec(`
			INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at, review_round)
			VALUES (?, 'p', 'd', 'title', 'spec', 'ready', 'ct', 'ut', 0)
		`, taskNum); err != nil {
			t.Fatal(err)
		}
	}

	// Create dependent tasks (200 to test dependencies)
	for i := 0; i < 200; i++ {
		depTaskID := fmt.Sprintf("dep-task-%d", i+1)
		if _, err := conn.Exec(`
			INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at, review_round)
			VALUES (?, 'p', 'd', 'dep-title', 'dep-spec', 'ready', 'ct', 'ut', 0)
		`, depTaskID); err != nil {
			t.Fatal(err)
		}
	}

	// Create target task
	if _, err := conn.Exec(`
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at, review_round)
		VALUES (?, 'p', 'd', 'target-title', 'target-spec', 'ready', 'ct', 'ut', 0)
	`, targetTaskID); err != nil {
		t.Fatal(err)
	}

	// Create review task with target_task_id pointing to target task
	if _, err := conn.Exec(`
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at, target_task_id, kind, review_round)
		VALUES (?, 'p', 'd', 'review-title', 'review-spec', 'review', 'ct', 'ut', ?, 'review', 1)
	`, targetTaskID+"_review_1", targetTaskID); err != nil {
		t.Fatal(err)
	}

	// Create events for task-1 (5,000 events at scale)
	for i := 0; i < 5000; i++ {
		eventID := fmt.Sprintf("event-%d", i+1)
		if _, err := conn.Exec(`
			INSERT INTO event (id, task_id, actor, kind, verdict, note, created_at)
			VALUES (?, ?, 'actor', 'submit', NULL, NULL, ?)
		`, eventID, taskID, fmt.Sprintf("2026-01-01T%02d:00:00Z", i%24)); err != nil {
			t.Fatal(err)
		}
	}

	// Create task links for task-1 (1,000 links at scale)
	for i := 0; i < 1000; i++ {
		linkID := fmt.Sprintf("link-%d", i+1)
		if _, err := conn.Exec(`
			INSERT INTO task_link (id, task_id, kind, value)
			VALUES (?, ?, 'pr', 'https://example.com/')
		`, linkID, taskID); err != nil {
			t.Fatal(err)
		}
	}

	// Create task dependencies for testing idx_task_dep_depends_on (100)
	for i := 0; i < 100; i++ {
		depTaskID := fmt.Sprintf("dep-task-%d", i+1)
		if _, err := conn.Exec(`
			INSERT INTO task_dep (depends_on_id, task_id)
			VALUES (?, ?)
		`, taskID, depTaskID); err != nil {
			t.Fatal(err)
		}
	}

	// Check indexes before migration
	indexesBefore := func() []string {
		rows, err := conn.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name IN ('event', 'task_link', 'task', 'task_dep') AND sql IS NOT NULL ORDER BY name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			out = append(out, name)
		}
		return out
	}

	// Run migration
	start := time.Now()
	if err := s.migrate(migrationsFS); err != nil {
		t.Fatalf("migrate to 0036: %v", err)
	}
	duration := time.Since(start)
	t.Logf("Migration 0036 took %v", duration)

	// Verify the four indexes exist
	afterIndexes := indexesBefore()
	expectedIndexes := []string{
		"idx_event_task",
		"idx_task_dep_depends_on",
		"idx_task_link_task",
		"idx_task_target",
	}
	for _, expected := range expectedIndexes {
		found := false
		for _, idx := range afterIndexes {
			if idx == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("expected index %s not found in %v", expected, afterIndexes)
		}
	}

	// Test EXPLAIN QUERY PLAN for each index
	tests := []struct {
		name        string
		explainStmt string
		expectedIdx string
		noScanTable string
	}{
		{
			name:        "events by task",
			explainStmt: fmt.Sprintf(`EXPLAIN QUERY PLAN SELECT id FROM event WHERE task_id = '%s' ORDER BY created_at, id`, taskID),
			expectedIdx: "idx_event_task",
			noScanTable: "event",
		},
		{
			name:        "task links by task",
			explainStmt: fmt.Sprintf(`EXPLAIN QUERY PLAN SELECT id FROM task_link WHERE task_id = '%s' ORDER BY id`, taskID),
			expectedIdx: "idx_task_link_task",
			noScanTable: "task_link",
		},
		{
			name:        "review tasks by target",
			explainStmt: fmt.Sprintf(`EXPLAIN QUERY PLAN SELECT id FROM task WHERE target_task_id = '%s' AND kind = 'review' AND review_round = 1`, targetTaskID),
			expectedIdx: "idx_task_target",
			noScanTable: "task",
		},
		{
			name:        "dependents of task",
			explainStmt: fmt.Sprintf(`EXPLAIN QUERY PLAN SELECT task_id FROM task_dep WHERE depends_on_id = '%s' ORDER BY task_id`, taskID),
			expectedIdx: "idx_task_dep_depends_on",
			noScanTable: "task_dep",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := conn.Query(test.explainStmt)
			if err != nil {
				t.Fatalf("failed to explain query: %v", err)
			}
			defer rows.Close()

			var plans []string
			for rows.Next() {
				var id, parent, notused int
				var detail string
				if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
					t.Fatal(err)
				}
				plans = append(plans, detail)
			}

			plan := strings.Join(plans, " | ")
			t.Logf("Query plan: %s", plan)

			// Check that the plan uses the expected index
			if !strings.Contains(plan, test.expectedIdx) {
				t.Fatalf("query plan should use %s, but got: %s", test.expectedIdx, plan)
			}
			// Check that the plan does not scan the table
			if strings.Contains(plan, "SCAN "+test.noScanTable) {
				t.Fatalf("query plan should not scan %s, but got: %s", test.noScanTable, plan)
			}
		})
	}

	// Verify fresh database also has the four indexes
	freshDB := filepath.Join(t.TempDir(), "fresh.db")
	freshConn, err := sql.Open("sqlite", buildDSN(freshDB))
	if err != nil {
		t.Fatal(err)
	}
	defer freshConn.Close()
	freshConn.SetMaxOpenConns(1)
	freshStore := &sqliteStore{conn: freshConn}
	if err := freshStore.migrate(migrationsFS); err != nil {
		t.Fatalf("fresh migrate: %v", err)
	}

	// Check that fresh database has all four indexes
	var freshIdx []string
	rows, err := freshConn.Query(`SELECT name FROM sqlite_master WHERE type='index' AND tbl_name IN ('event', 'task_link', 'task', 'task_dep') AND sql IS NOT NULL ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		freshIdx = append(freshIdx, name)
	}

	for _, expected := range expectedIndexes {
		found := false
		for _, idx := range freshIdx {
			if idx == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("fresh database missing index %s, has %v", expected, freshIdx)
		}
	}
}
