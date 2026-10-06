package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/boldfield/odonian/internal/forge"
)

// defaultTestAllowedModels returns the default allowed models for tests (matching main.go default).
func defaultTestAllowedModels() []string {
	return []string{"haiku", "sonnet", "opus"}
}

// testUnlimitedResearchBudget is passed to SubmitTask/ReleaseTask by tests that
// don't exercise docs/features/research-track.md section 6's chain-wide research
// round budget, so it never trips: build/design tasks ignore it outright, and
// research tests that push past a handful of rounds (e.g. the per-tier circuit
// breaker tests) keep testing that breaker rather than the budget.
const testUnlimitedResearchBudget = 999999

func ptrStr(s string) *string {
	return &s
}

func ptrBool(b bool) *bool {
	return &b
}

func ptrString(s string) *string {
	return &s
}

// createTestFSWithBadMigration creates a test filesystem with the standard migrations
// plus a bad migration (0003_bad.sql) that leaves a dangling foreign key.
// It wraps the embedded migrations and adds the bad migration on top.
func createTestFSWithBadMigration() fs.FS {
	// Read the actual migrations from the embedded FS
	// Return a custom fs that includes the embedded migrations plus the bad one
	return &compositeFS{
		first: migrationsFS,
		bad: fstest.MapFS{
			"migrations/0003_bad.sql": &fstest.MapFile{
				Data: []byte("INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at) VALUES ('dummy-task', 'non-existent-project', 'non-existent-doc', 'Dummy Task', 'spec', 'backlog', datetime('now'), datetime('now'));"),
			},
		},
	}
}

// compositeFS is a custom fs.FS that combines two filesystems, preferring files from the first.
type compositeFS struct {
	first fs.FS
	bad   fs.FS
}

func (c *compositeFS) Open(name string) (fs.File, error) {
	// Try the bad migrations first (0003_bad.sql)
	if strings.Contains(name, "0003_bad") {
		return c.bad.Open(name)
	}
	// Fall back to the embedded migrations
	return c.first.Open(name)
}

func (c *compositeFS) ReadDir(name string) ([]fs.DirEntry, error) {
	// Read from the first FS and add bad migrations
	entries, err := fs.ReadDir(c.first, name)
	if err != nil {
		return nil, err
	}

	// For the migrations directory, also include 0003_bad.sql
	if name == "migrations" {
		badEntries, _ := fs.ReadDir(c.bad, "migrations")
		if badEntries != nil {
			entries = append(entries, badEntries...)
			// Sort to ensure consistent order
			sort.Slice(entries, func(i, j int) bool {
				return entries[i].Name() < entries[j].Name()
			})
		}
	}
	return entries, nil
}

// TestMigrations verifies that migrations can be applied to a fresh database
// and that re-applying is idempotent.
func TestMigrations(t *testing.T) {
	// Use in-memory database for testing
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Verify all expected tables exist
	expectedTables := []string{
		"project",
		"document",
		"task",
		"task_dep",
		"task_link",
		"event",
		"schema_migrations",
	}

	for _, tableName := range expectedTables {
		var count int
		err := store.Conn().QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?",
			tableName,
		).Scan(&count)
		if err != nil {
			t.Fatalf("failed to check if table %s exists: %v", tableName, err)
		}
		if count != 1 {
			t.Errorf("expected table %s to exist, but it doesn't", tableName)
		}
	}

	// Verify expected indexes exist
	expectedIndexes := []string{
		"idx_task_link_kind_value",
		"idx_task_project_state",
		"idx_document_one_design_per_project",
	}

	for _, indexName := range expectedIndexes {
		var count int
		err := store.Conn().QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?",
			indexName,
		).Scan(&count)
		if err != nil {
			t.Fatalf("failed to check if index %s exists: %v", indexName, err)
		}
		if count != 1 {
			t.Errorf("expected index %s to exist, but it doesn't", indexName)
		}
	}

	// Verify that schema_migrations table has the migrations recorded
	var migrationCount int
	err = store.Conn().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount)
	if err != nil {
		t.Fatalf("failed to count migrations: %v", err)
	}
	if migrationCount != 33 {
		t.Errorf("expected 33 migrations to be recorded, but got %d", migrationCount)
	}

	// Verify idempotency: re-open the same database and it should work
	store2, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to re-open database: %v", err)
	}
	defer store2.Close()

	// Verify that we still have exactly 33 migrations recorded (idempotency)
	err = store2.Conn().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount)
	if err != nil {
		t.Fatalf("failed to count migrations after re-open: %v", err)
	}
	if migrationCount != 33 {
		t.Errorf("expected 33 migrations after re-open (idempotency), but got %d", migrationCount)
	}
}

// TestWALEnabled verifies that WAL mode is enabled on the database.
func TestWALEnabled(t *testing.T) {
	// Use a file-based DB for WAL test since in-memory doesn't support WAL
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "wal_test.db")

	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	var journalMode string
	err = store.Conn().QueryRow("PRAGMA journal_mode").Scan(&journalMode)
	if err != nil {
		t.Fatalf("failed to query PRAGMA journal_mode: %v", err)
	}

	if journalMode != "wal" {
		t.Errorf("expected journal_mode to be 'wal', but got '%s'", journalMode)
	}
}

// TestForeignKeysEnforced verifies that foreign key constraints are enforced.
func TestForeignKeysEnforced(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Try to insert a task with a non-existent project_id (bad FK)
	// This should fail because foreign_keys is enabled
	_, err = store.Conn().Exec(`
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "test-task", "non-existent-project", "non-existent-doc", "Test Task", "Test spec", "backlog", "2026-06-04T00:00:00Z", "2026-06-04T00:00:00Z")

	if err == nil {
		t.Error("expected foreign key constraint violation, but insert succeeded")
	}
}

// TestOpenSamePath verifies that opening the same database path twice sequentially works.
func TestOpenSamePath(t *testing.T) {
	// Create a temporary file for the database
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Open the database the first time
	store1, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database for the first time: %v", err)
	}

	// Verify table exists
	var count int
	err = store1.Conn().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='project'").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query after first open: %v", err)
	}
	if count != 1 {
		t.Error("expected project table to exist after first open")
	}

	// Close the first connection
	if err := store1.Close(); err != nil {
		t.Fatalf("failed to close first connection: %v", err)
	}

	// Open the database the second time (same path)
	store2, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database for the second time: %v", err)
	}
	defer store2.Close()

	// Verify table still exists
	err = store2.Conn().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='project'").Scan(&count)
	if err != nil {
		t.Fatalf("failed to query after second open: %v", err)
	}
	if count != 1 {
		t.Error("expected project table to exist after second open")
	}

	// Verify that schema_migrations was not re-applied (idempotency)
	var migrationCount int
	err = store2.Conn().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount)
	if err != nil {
		t.Fatalf("failed to count migrations after second open: %v", err)
	}
	if migrationCount != 33 {
		t.Errorf("expected 33 migrations after second open, but got %d", migrationCount)
	}
}

// TestAppendEventAtomicity tests that AppendEvent works within a transaction
// and that rolling back the transaction drops both the state change and the event.
func TestAppendEventAtomicity(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	// Insert parent rows (project, document, task) via raw SQL first
	// This is required because foreign key constraints are enforced (T03).
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO project (id, name, repo, created_at)
		VALUES (?, ?, ?, ?)
	`, "proj-1", "test-project", "https://github.com/example/repo", now)
	if err != nil {
		t.Fatalf("failed to insert project: %v", err)
	}

	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "doc-1", "proj-1", "design", "Test Design", "DESIGN.md", now, now)
	if err != nil {
		t.Fatalf("failed to insert document: %v", err)
	}

	taskID := "task-1"
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, taskID, "proj-1", "doc-1", "Test Task", "Test spec", "ready", now, now)
	if err != nil {
		t.Fatalf("failed to insert task: %v", err)
	}

	// Begin a transaction
	tx, err := store.Conn().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	// Within the transaction:
	// 1. Update task state to in_progress (simulating a state change)
	_, err = tx.ExecContext(ctx, `
		UPDATE task SET state = ?, updated_at = ? WHERE id = ?
	`, "in_progress", now, taskID)
	if err != nil {
		t.Fatalf("failed to update task state: %v", err)
	}

	// 2. Append an event using AppendEvent
	actor := "test-agent"
	kind := "claim"
	_, err = store.AppendEvent(ctx, tx, taskID, actor, kind, nil, nil)
	if err != nil {
		t.Fatalf("failed to append event: %v", err)
	}

	// Rollback the transaction without committing
	if err := tx.Rollback(); err != nil {
		t.Fatalf("failed to rollback transaction: %v", err)
	}

	// Verify that the task state is still "ready" (not "in_progress")
	var state string
	err = store.Conn().QueryRowContext(ctx, "SELECT state FROM task WHERE id = ?", taskID).Scan(&state)
	if err != nil {
		t.Fatalf("failed to query task state: %v", err)
	}
	if state != "ready" {
		t.Errorf("expected task state to be 'ready' after rollback, but got '%s'", state)
	}

	// Verify that no events were inserted
	var eventCount int
	err = store.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM event WHERE task_id = ?", taskID).Scan(&eventCount)
	if err != nil {
		t.Fatalf("failed to count events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("expected 0 events after rollback, but got %d", eventCount)
	}
}

// TestListEvents tests that ListEvents returns events in chronological order (created_at, id).
func TestListEvents(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	// Insert parent rows
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO project (id, name, repo, created_at)
		VALUES (?, ?, ?, ?)
	`, "proj-2", "test-project-2", "https://github.com/example/repo2", now)
	if err != nil {
		t.Fatalf("failed to insert project: %v", err)
	}

	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "doc-2", "proj-2", "design", "Test Design 2", "DESIGN.md", now, now)
	if err != nil {
		t.Fatalf("failed to insert document: %v", err)
	}

	taskID := "task-2"
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, taskID, "proj-2", "doc-2", "Test Task 2", "Test spec 2", "ready", now, now)
	if err != nil {
		t.Fatalf("failed to insert task: %v", err)
	}

	// Insert events in separate transactions with explicit timestamps to ensure ordering
	// Use progressively later timestamps to guarantee order
	events := []struct {
		actor   string
		kind    string
		verdict *string
		note    *string
		offset  time.Duration
	}{
		{"agent-1", "claim", nil, nil, 0 * time.Millisecond},
		{"agent-1", "heartbeat", nil, nil, 10 * time.Millisecond},
		{"human", "review", strPtr("approve"), strPtr("looks good"), 20 * time.Millisecond},
	}

	for _, evt := range events {
		tx, err := store.Conn().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("failed to begin transaction: %v", err)
		}

		_, err = store.AppendEvent(ctx, tx, taskID, evt.actor, evt.kind, evt.verdict, evt.note)
		if err != nil {
			t.Fatalf("failed to append event: %v", err)
		}

		if err := tx.Commit(); err != nil {
			t.Fatalf("failed to commit transaction: %v", err)
		}

		// Sleep to ensure timestamps differ
		time.Sleep(evt.offset + 5*time.Millisecond)
	}

	// List all events
	listedEvents, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	// Verify we got exactly 3 events
	if len(listedEvents) != 3 {
		t.Errorf("expected 3 events, but got %d", len(listedEvents))
		for i, e := range listedEvents {
			t.Logf("event %d: id=%s, actor=%s, kind=%s, created_at=%s", i, e.ID, e.Actor, e.Kind, e.CreatedAt)
		}
	}

	// Verify that events are in chronological order (created_at, id)
	for i := 0; i < len(listedEvents)-1; i++ {
		current := listedEvents[i]
		next := listedEvents[i+1]

		// created_at should be <= next created_at
		if current.CreatedAt > next.CreatedAt {
			t.Errorf("events not in chronological order: event %d has created_at %s, event %d has created_at %s",
				i, current.CreatedAt, i+1, next.CreatedAt)
		}

		// If created_at is equal, id should be < next id
		if current.CreatedAt == next.CreatedAt && current.ID > next.ID {
			t.Errorf("events not in chronological order by id: event %d has id %s, event %d has id %s",
				i, current.ID, i+1, next.ID)
		}
	}

	// Verify the actors and kinds are in expected order
	// (only verify that we have 3 events with the right properties, not necessarily in order)
	foundActorKindPairs := make(map[string]bool)
	for _, e := range listedEvents {
		key := e.Actor + ":" + e.Kind
		foundActorKindPairs[key] = true
	}

	expectedPairs := []string{"agent-1:claim", "agent-1:heartbeat", "human:review"}
	for _, expected := range expectedPairs {
		if !foundActorKindPairs[expected] {
			t.Errorf("expected to find event with %s, but didn't", expected)
		}
	}
}

// TestListEventsRapidOrdering appends many events back-to-back with NO sleep and
// asserts they come back in insertion order. This is the regression guard for the
// event-spine ordering bug: under second-granularity timestamps all of these inserts
// share the same created_at, so ORDER BY (created_at, id) sorts by random UUID and
// scrambles them. With fixed-width nanosecond timestamps and the single-writer store,
// each insert gets a distinct increasing timestamp, preserving order.
func TestListEventsRapidOrdering(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := nowTimestamp()

	// Parent rows (FKs are enforced).
	if _, err = store.Conn().ExecContext(ctx,
		`INSERT INTO project (id, name, repo, created_at) VALUES (?, ?, ?, ?)`,
		"proj-rapid", "rapid", "https://example.com/r", now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err = store.Conn().ExecContext(ctx,
		`INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"doc-rapid", "proj-rapid", "design", "d", "DESIGN.md", now, now); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	taskID := "task-rapid"
	if _, err = store.Conn().ExecContext(ctx,
		`INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		taskID, "proj-rapid", "doc-rapid", "t", "s", "ready", now, now); err != nil {
		t.Fatalf("insert task: %v", err)
	}

	const n = 25
	for i := 0; i < n; i++ {
		tx, err := store.Conn().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		// kind encodes insertion order; no sleep between appends.
		if _, err = store.AppendEvent(ctx, tx, taskID, "agent", fmt.Sprintf("evt-%02d", i), nil, nil); err != nil {
			t.Fatalf("append event %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	listed, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(listed) != n {
		t.Fatalf("expected %d events, got %d", n, len(listed))
	}
	for i, e := range listed {
		want := fmt.Sprintf("evt-%02d", i)
		if e.Kind != want {
			t.Fatalf("event at position %d out of order: got kind %q, want %q "+
				"(events scrambled — timestamp ordering regression)", i, e.Kind, want)
		}
	}
}

// TestClaimTaskSuccessful tests that claiming a ready task succeeds and
// sets state, assignee, and lease_expires_at correctly.
func TestClaimTaskSuccessful(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and ready task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	if err != nil {
		t.Fatalf("failed to set task to ready: %v", err)
	}

	// Claim the task
	agentID := "test-agent"
	leaseTTL := 5 * time.Minute
	claimedTask, err := store.ClaimTask(ctx, taskID, agentID, "haiku", leaseTTL)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Verify claimed task state
	if claimedTask.State != "in_progress" {
		t.Errorf("expected state='in_progress', got '%s'", claimedTask.State)
	}
	if claimedTask.Assignee == nil || *claimedTask.Assignee != agentID {
		t.Errorf("expected assignee='%s', got %v", agentID, claimedTask.Assignee)
	}
	if claimedTask.LeaseExpiresAt == nil {
		t.Error("expected lease_expires_at to be set")
	}

	// Verify that a claim event was recorded
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event, got %d", len(events))
	}
	if events[0].Kind != "claim" {
		t.Errorf("expected event kind='claim', got '%s'", events[0].Kind)
	}
	if events[0].Actor != agentID {
		t.Errorf("expected event actor='%s', got '%s'", agentID, events[0].Actor)
	}
}

// TestClaimTaskAlreadyClaimed tests that claiming an already-claimed task returns ErrConflict.
func TestClaimTaskAlreadyClaimed(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and ready task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	if err != nil {
		t.Fatalf("failed to set task to ready: %v", err)
	}

	// Claim the task once
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("first claim failed: %v", err)
	}

	// Try to claim it again
	_, err = store.ClaimTask(ctx, taskID, "agent-2", "haiku", 5*time.Minute)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict on second claim, got %v", err)
	}
}

// TestClaimTaskWithUnfinishedDependency tests that claiming a task with an unfinished dependency returns ErrConflict.
func TestClaimTaskWithUnfinishedDependency(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create two tasks: one to depend on, one that depends
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Key: "dep-task", Title: "Dependency Task", Spec: "spec", DocumentID: doc.ID},
		{Title: "Dependent Task", Spec: "spec", DocumentID: doc.ID, DependsOn: []string{"dep-task"}},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	depTaskID := tasks[0].ID
	dependentTaskID := tasks[1].ID

	// Promote both to ready
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id IN (?, ?)", "ready", depTaskID, dependentTaskID)
	if err != nil {
		t.Fatalf("failed to set tasks to ready: %v", err)
	}

	// Try to claim dependent task (should fail because dependency is not done)
	_, err = store.ClaimTask(ctx, dependentTaskID, "agent-1", "haiku", 5*time.Minute)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict when dependency is not done, got %v", err)
	}

	// Mark the dependency as done
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "done", depTaskID)
	if err != nil {
		t.Fatalf("failed to set dependency to done: %v", err)
	}

	// Now claiming should succeed
	_, err = store.ClaimTask(ctx, dependentTaskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Errorf("claim should succeed after dependency is done, got error: %v", err)
	}
}

// TestClaimTaskNotFound tests that claiming a non-existent task returns ErrNotFound.
func TestClaimTaskNotFound(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Try to claim a non-existent task
	_, err = store.ClaimTask(ctx, "non-existent-task", "agent-1", "haiku", 5*time.Minute)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

// TestClaimTaskConcurrency is the critical concurrency test: N goroutines attempt to claim
// the same ready task concurrently. Exactly one should succeed (ErrConflict=nil), and the
// other N-1 should get ErrConflict. This proves the atomic UPDATE design works.
func TestClaimTaskConcurrency(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and ready task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	if err != nil {
		t.Fatalf("failed to set task to ready: %v", err)
	}

	// Launch N goroutines that try to claim the task concurrently
	const numGoroutines = 20
	var wg sync.WaitGroup
	results := make([]error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			agentID := fmt.Sprintf("agent-%d", index)
			_, err := store.ClaimTask(ctx, taskID, agentID, "haiku", 5*time.Minute)
			results[index] = err
		}(i)
	}

	wg.Wait()

	// Count successes and conflicts
	successCount := 0
	conflictCount := 0

	for _, err := range results {
		if err == nil {
			successCount++
		} else if errors.Is(err, ErrConflict) {
			conflictCount++
		} else {
			t.Errorf("unexpected error: %v", err)
		}
	}

	// Exactly one should succeed, rest should get ErrConflict
	if successCount != 1 {
		t.Errorf("expected exactly 1 success, got %d", successCount)
	}
	if conflictCount != numGoroutines-1 {
		t.Errorf("expected %d conflicts, got %d", numGoroutines-1, conflictCount)
	}

	// Verify that exactly one claim event was recorded
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) != 1 {
		t.Errorf("expected 1 claim event, got %d", len(events))
	}
}

// TestClaimTaskExpiredLease tests that a task with an expired lease can be re-claimed.
func TestClaimTaskExpiredLease(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := nowTimestamp()

	// Create a project, document, and task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Manually set the task to ready with an expired lease (in the past)
	pastTime := time.Now().UTC().Add(-1 * time.Hour).Format(timestampLayout)
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET state = ?, assignee = ?, lease_expires_at = ?, updated_at = ?
		WHERE id = ?
	`, "ready", "dead-agent", pastTime, now, taskID)
	if err != nil {
		t.Fatalf("failed to set task with expired lease: %v", err)
	}

	// Try to claim the task (should succeed because lease is expired)
	claimedTask, err := store.ClaimTask(ctx, taskID, "new-agent", "haiku", 5*time.Minute)
	if err != nil {
		t.Errorf("expected to claim task with expired lease, got error: %v", err)
	}

	// Verify the new agent is now the assignee
	if claimedTask.Assignee == nil || *claimedTask.Assignee != "new-agent" {
		t.Errorf("expected assignee='new-agent', got %v", claimedTask.Assignee)
	}
}

// TestClaimTaskModelMismatch tests that claiming with a mismatched model returns MODEL_MISMATCH conflict.
func TestClaimTaskModelMismatch(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and task with model='sonnet'
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID, Model: "sonnet"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	if err != nil {
		t.Fatalf("failed to set task to ready: %v", err)
	}

	// Try to claim with haiku model (mismatch)
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) || conflictErr.Code != "MODEL_MISMATCH" {
		t.Errorf("expected MODEL_MISMATCH conflict, got: %v", err)
	}

	// Try to claim with sonnet model (match) - should succeed
	claimedTask, err := store.ClaimTask(ctx, taskID, "agent-2", "sonnet", 5*time.Minute)
	if err != nil {
		t.Errorf("expected to claim task with matching model, got error: %v", err)
	}
	if claimedTask.State != "in_progress" {
		t.Errorf("expected state='in_progress', got '%s'", claimedTask.State)
	}
}

// TestClaimTaskConcurrencyMixedModels tests concurrency with mixed models.
// Verifies that when a haiku task is ready, haiku agents can claim it (one winner),
// and all other agents (matching or not) lose the race. The test adds a secondary verification:
// after the task is claimed, subsequent sonnet attempts get ErrConflict (not otherwise-claimable).
func TestClaimTaskConcurrencyMixedModels(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a single task with model="haiku"
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Haiku Task", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	if err != nil {
		t.Fatalf("failed to set task to ready: %v", err)
	}

	// Launch 10 haiku agents and 10 sonnet agents all contending for the same haiku task
	const numPerModel = 10
	var wg sync.WaitGroup

	haikuResults := make([]error, numPerModel)
	sonnetResults := make([]error, numPerModel)

	// Haiku agents (matching model) contend for the task
	for i := 0; i < numPerModel; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			agentID := fmt.Sprintf("haiku-agent-%d", index)
			_, err := store.ClaimTask(ctx, taskID, agentID, "haiku", 5*time.Minute)
			haikuResults[index] = err
		}(i)
	}

	// Sonnet agents (mismatched model) also try to claim
	for i := 0; i < numPerModel; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			agentID := fmt.Sprintf("sonnet-agent-%d", index)
			_, err := store.ClaimTask(ctx, taskID, agentID, "sonnet", 5*time.Minute)
			sonnetResults[index] = err
		}(i)
	}

	wg.Wait()

	// Count successes and conflicts for each model
	haikuSuccess := 0
	haikuConflict := 0
	for _, err := range haikuResults {
		if err == nil {
			haikuSuccess++
		} else if errors.Is(err, ErrConflict) {
			haikuConflict++
		} else {
			t.Errorf("unexpected haiku error: %v", err)
		}
	}

	// Count sonnet errors: mixture of MODEL_MISMATCH and ErrConflict, depending on race timing
	sonnetErrors := 0
	for _, err := range sonnetResults {
		if err != nil {
			sonnetErrors++
			// Verify it's one of the expected error types
			var conflictErr *ConflictError
			if !errors.As(err, &conflictErr) && !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected sonnet error type: %v", err)
			}
		}
	}

	// Exactly one haiku agent should succeed (the one that wins the race)
	if haikuSuccess != 1 {
		t.Errorf("expected 1 haiku success, got %d", haikuSuccess)
	}

	// The rest of the haiku agents should get conflicts (task already claimed)
	if haikuConflict != numPerModel-1 {
		t.Errorf("expected %d haiku conflicts, got %d", numPerModel-1, haikuConflict)
	}

	// All sonnet agents should get an error (either MODEL_MISMATCH if task was still ready,
	// or ErrConflict if it was already claimed by the time they tried)
	if sonnetErrors != numPerModel {
		t.Errorf("expected all %d sonnet agents to error, got %d errors", numPerModel, sonnetErrors)
	}
}

// Helper function to create string pointers
func strPtr(s string) *string {
	return &s
}

// TestMigrationForeignKeysDisabled verifies that foreign keys are disabled during migration
// and that a table rebuild migration (which temporarily drops tables) works correctly.
func TestMigrationForeignKeysDisabled(t *testing.T) {
	// Use file-based DB since in-memory with multiple connections behaves differently
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "migration_fk_test.db")

	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Verify that foreign keys are ON after migrations complete
	var fkEnabled int
	err = store.Conn().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkEnabled)
	if err != nil {
		t.Fatalf("failed to check foreign keys pragma: %v", err)
	}
	if fkEnabled != 1 {
		t.Error("expected foreign_keys to be ON after migrations, but it's OFF")
	}

	// Verify that trying to insert a task with non-existent FK fails (FK is enforced)
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "test-task", "non-existent-proj", "non-existent-doc", "Task", "spec", "backlog",
		"2026-06-06T00:00:00Z", "2026-06-06T00:00:00Z")

	if err == nil {
		t.Error("expected FK constraint violation when inserting with bad FK, but insert succeeded")
	}
}

// TestMigrationForeignKeyIntegrityCheck verifies that the integrity check rejects
// migrations that would leave dangling foreign key references.
func TestMigrationForeignKeyIntegrityCheck(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "integrity_check_test.db")

	// Create a test FS with a migration that intentionally leaves a dangling FK
	testFS := createTestFSWithBadMigration()

	conn, err := sql.Open("sqlite", buildDSN(dbPath))
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)
	store := &sqliteStore{conn: conn}

	// Try to apply migrations with the bad migration included
	err = store.migrate(testFS)

	// The migration should fail due to FK constraint violation detected by integrity check
	if err == nil {
		t.Error("expected migration with dangling FK to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "foreign key violations") {
		t.Errorf("expected error to mention foreign key violations, got: %v", err)
	}
}

// TestMigrationRoundTrip verifies that:
// 1. Existing migrations apply cleanly
// 2. All rows survive the migration
// 3. Foreign keys remain intact and enforced
func TestMigrationRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "roundtrip_test.db")

	// Open fresh DB and populate it
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}

	ctx := context.Background()

	// Create multiple projects, documents, and tasks in various states
	proj1, err := store.CreateProject(ctx, "proj-1", "https://example.com/repo1")
	if err != nil {
		t.Fatalf("failed to create project 1: %v", err)
	}

	proj2, err := store.CreateProject(ctx, "proj-2", "https://example.com/repo2")
	if err != nil {
		t.Fatalf("failed to create project 2: %v", err)
	}

	doc1, err := store.CreateDocument(ctx, proj1.ID, "design", "Design 1", "DESIGN1.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 1: %v", err)
	}

	doc2, err := store.CreateDocument(ctx, proj2.ID, "feature_spec", "Spec 2", "SPEC2.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 2: %v", err)
	}

	// Create tasks in various states
	tasks1, err := store.CreateTasks(ctx, proj1.ID, []TaskInput{
		{Key: "task-1a", Title: "Task 1A", Spec: "Spec 1A", DocumentID: doc1.ID},
		{Title: "Task 1B", Spec: "Spec 1B", DocumentID: doc1.ID, DependsOn: []string{"task-1a"}},
	})
	if err != nil {
		t.Fatalf("failed to create tasks for proj1: %v", err)
	}

	tasks2, err := store.CreateTasks(ctx, proj2.ID, []TaskInput{
		{Title: "Task 2A", Spec: "Spec 2A", DocumentID: doc2.ID},
		{Title: "Task 2B", Spec: "Spec 2B", DocumentID: doc2.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks for proj2: %v", err)
	}

	// Set tasks to various states
	_, err = store.Conn().ExecContext(ctx,
		`UPDATE task SET state = ? WHERE id = ?`,
		"ready", tasks1[0].ID)
	if err != nil {
		t.Fatalf("failed to set task state: %v", err)
	}

	_, err = store.Conn().ExecContext(ctx,
		`UPDATE task SET state = ? WHERE id IN (?, ?)`,
		"in_progress", tasks1[1].ID, tasks2[0].ID)
	if err != nil {
		t.Fatalf("failed to set task states: %v", err)
	}

	_, err = store.Conn().ExecContext(ctx,
		`UPDATE task SET state = ? WHERE id = ?`,
		"done", tasks2[1].ID)
	if err != nil {
		t.Fatalf("failed to set task state: %v", err)
	}

	// Add some links
	_, err = store.Conn().ExecContext(ctx,
		`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, ?, ?)`,
		"link-1", tasks1[0].ID, "pr", "#123")
	if err != nil {
		t.Fatalf("failed to add task link: %v", err)
	}

	// Add some events
	tx, err := store.Conn().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	_, err = store.AppendEvent(ctx, tx, tasks1[0].ID, "agent-1", "claim", nil, nil)
	if err != nil {
		tx.Rollback()
		t.Fatalf("failed to append event: %v", err)
	}
	tx.Commit()

	// Count initial rows
	var initialProjectCount, initialDocCount, initialTaskCount, initialTaskDepCount, initialTaskLinkCount, initialEventCount int

	store.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM project").Scan(&initialProjectCount)
	store.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM document").Scan(&initialDocCount)
	store.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM task").Scan(&initialTaskCount)
	store.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM task_dep").Scan(&initialTaskDepCount)
	store.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM task_link").Scan(&initialTaskLinkCount)
	store.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM event").Scan(&initialEventCount)

	store.Close()

	// Reopen the database (this will trigger migrations again, but they should be idempotent)
	store2, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to reopen database: %v", err)
	}
	defer store2.Close()

	// Count rows after migration
	var finalProjectCount, finalDocCount, finalTaskCount, finalTaskDepCount, finalTaskLinkCount, finalEventCount int

	store2.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM project").Scan(&finalProjectCount)
	store2.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM document").Scan(&finalDocCount)
	store2.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM task").Scan(&finalTaskCount)
	store2.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM task_dep").Scan(&finalTaskDepCount)
	store2.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM task_link").Scan(&finalTaskLinkCount)
	store2.Conn().QueryRowContext(ctx, "SELECT COUNT(*) FROM event").Scan(&finalEventCount)

	// Verify all rows survived
	if finalProjectCount != initialProjectCount {
		t.Errorf("project count mismatch: initial=%d, final=%d", initialProjectCount, finalProjectCount)
	}
	if finalDocCount != initialDocCount {
		t.Errorf("document count mismatch: initial=%d, final=%d", initialDocCount, finalDocCount)
	}
	if finalTaskCount != initialTaskCount {
		t.Errorf("task count mismatch: initial=%d, final=%d", initialTaskCount, finalTaskCount)
	}
	if finalTaskDepCount != initialTaskDepCount {
		t.Errorf("task_dep count mismatch: initial=%d, final=%d", initialTaskDepCount, finalTaskDepCount)
	}
	if finalTaskLinkCount != initialTaskLinkCount {
		t.Errorf("task_link count mismatch: initial=%d, final=%d", initialTaskLinkCount, finalTaskLinkCount)
	}
	if finalEventCount != initialEventCount {
		t.Errorf("event count mismatch: initial=%d, final=%d", initialEventCount, finalEventCount)
	}

	// Verify FK constraints are still enforced
	_, err = store2.Conn().ExecContext(ctx, `
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "bad-task", "non-existent-proj", "non-existent-doc", "Bad", "bad", "backlog",
		"2026-06-06T00:00:00Z", "2026-06-06T00:00:00Z")

	if err == nil {
		t.Error("expected FK constraint violation after migration, but insert succeeded")
	}
}

// TestMigration0003ApprovedState verifies that migration 0003 (widening state CHECK to include 'approved'):
// 1. Applies cleanly to a database populated with tasks in various states (with deps, links, events)
// 2. All existing rows remain intact with identical column values
// 3. Foreign keys still resolve and are enforced
// 4. A task can now be set to 'approved' state
// 5. Invalid states are still rejected by the CHECK constraint
//
// This test builds a PRE-0003 schema, seeds it with data, then applies 0003 to verify the table
// rebuild succeeds and preserves all rows/columns/FKs. This differs from TestMigrationRoundTrip
// which relies on Open() (which applies all migrations upfront against empty DBs); this test
// directly subjects populated data to the rebuild.
func TestMigration0003ApprovedState(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "migration_0003_test.db")

	// Step 1: Build a fresh DB at PRE-0003 schema by executing 0001 and 0002 migrations
	conn, err := sql.Open("sqlite", buildDSN(dbPath))
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)

	// Disable FK for initial schema creation
	if _, err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("failed to disable foreign keys: %v", err)
	}

	// Execute 0001 migration to create initial schema
	migration0001, err := fs.ReadFile(migrationsFS, "migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("failed to read migration 0001: %v", err)
	}
	for _, stmt := range splitStatements(string(migration0001)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("failed to execute 0001: %v", err)
		}
	}

	// Execute 0002 migration
	migration0002, err := fs.ReadFile(migrationsFS, "migrations/0002_document_one_design.sql")
	if err != nil {
		t.Fatalf("failed to read migration 0002: %v", err)
	}
	for _, stmt := range splitStatements(string(migration0002)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("failed to execute 0002: %v", err)
		}
	}

	// Record that 0001 and 0002 were applied
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		t.Fatalf("failed to create schema_migrations: %v", err)
	}
	now := nowTimestamp()
	if _, err := conn.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", "0001", now); err != nil {
		t.Fatalf("failed to record 0001: %v", err)
	}
	if _, err := conn.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", "0002", now); err != nil {
		t.Fatalf("failed to record 0002: %v", err)
	}

	// Re-enable FK for data validation
	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("failed to enable foreign keys: %v", err)
	}

	// Step 2: Seed test data into the PRE-0003 schema
	// Create projects
	if _, err := conn.Exec(`
		INSERT INTO project (id, name, repo, created_at) VALUES (?, ?, ?, ?)
	`, "proj-0003-1", "repo1", "https://example.com/repo1", now); err != nil {
		t.Fatalf("failed to insert project 1: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO project (id, name, repo, created_at) VALUES (?, ?, ?, ?)
	`, "proj-0003-2", "repo2", "https://example.com/repo2", now); err != nil {
		t.Fatalf("failed to insert project 2: %v", err)
	}

	// Create documents
	if _, err := conn.Exec(`
		INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "doc-0003-1", "proj-0003-1", "design", "Design 1", "DESIGN.md", now, now); err != nil {
		t.Fatalf("failed to insert document 1: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "doc-0003-2", "proj-0003-2", "feature_spec", "Spec 2", "SPEC.md", now, now); err != nil {
		t.Fatalf("failed to insert document 2: %v", err)
	}

	// Create tasks in various PRE-0003 states (no 'approved' yet)
	taskData := []struct {
		id       string
		projID   string
		docID    string
		title    string
		spec     string
		state    string
		assignee *string
	}{
		{"task-0003-1a", "proj-0003-1", "doc-0003-1", "Task 1A", "Spec 1A", "backlog", nil},
		{"task-0003-1b", "proj-0003-1", "doc-0003-1", "Task 1B", "Spec 1B", "ready", nil},
		{"task-0003-2a", "proj-0003-2", "doc-0003-2", "Task 2A", "Spec 2A", "in_progress", strPtr("agent-1")},
		{"task-0003-2b", "proj-0003-2", "doc-0003-2", "Task 2B", "Spec 2B", "done", nil},
	}

	for _, td := range taskData {
		assigneeVal := sql.NullString{}
		if td.assignee != nil {
			assigneeVal = sql.NullString{String: *td.assignee, Valid: true}
		}
		if _, err := conn.Exec(`
			INSERT INTO task (id, project_id, document_id, title, spec, state, assignee, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, td.id, td.projID, td.docID, td.title, td.spec, td.state, assigneeVal, now, now); err != nil {
			t.Fatalf("failed to insert task %s: %v", td.id, err)
		}
	}

	// Create task dependency (1b depends on 1a)
	if _, err := conn.Exec(`
		INSERT INTO task_dep (task_id, depends_on_id) VALUES (?, ?)
	`, "task-0003-1b", "task-0003-1a"); err != nil {
		t.Fatalf("failed to insert task_dep: %v", err)
	}

	// Create task links
	if _, err := conn.Exec(`
		INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, ?, ?)
	`, "link-0003-1", "task-0003-1a", "pr", "#123"); err != nil {
		t.Fatalf("failed to insert task_link 1: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, ?, ?)
	`, "link-0003-2", "task-0003-2a", "commit", "abc123"); err != nil {
		t.Fatalf("failed to insert task_link 2: %v", err)
	}

	// Create events
	if _, err := conn.Exec(`
		INSERT INTO event (id, task_id, actor, kind, created_at) VALUES (?, ?, ?, ?, ?)
	`, "event-0003-1", "task-0003-1a", "agent-1", "claim", now); err != nil {
		t.Fatalf("failed to insert event 1: %v", err)
	}
	if _, err := conn.Exec(`
		INSERT INTO event (id, task_id, actor, kind, verdict, note, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "event-0003-2", "task-0003-2b", "human", "review", "approve", "looks good", now); err != nil {
		t.Fatalf("failed to insert event 2: %v", err)
	}

	// Record initial row counts and values before migration
	initialCounts := make(map[string]int)
	for _, table := range []string{"project", "document", "task", "task_dep", "task_link", "event"} {
		var count int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("failed to count %s: %v", table, err)
		}
		initialCounts[table] = count
	}

	// Record task field values for field-by-field comparison
	type TaskRow struct {
		id       string
		title    string
		spec     string
		state    string
		assignee sql.NullString
		result   sql.NullString
	}
	initialTasks := make(map[string]TaskRow)
	rows, err := conn.Query(`
		SELECT id, title, spec, state, assignee, result FROM task ORDER BY id
	`)
	if err != nil {
		t.Fatalf("failed to query initial tasks: %v", err)
	}
	for rows.Next() {
		var tr TaskRow
		if err := rows.Scan(&tr.id, &tr.title, &tr.spec, &tr.state, &tr.assignee, &tr.result); err != nil {
			t.Fatalf("failed to scan task: %v", err)
		}
		initialTasks[tr.id] = tr
	}
	rows.Close()

	// Step 3: Apply migration 0003 to the populated database
	// Disable FK during migration (following the runner's pattern)
	if _, err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("failed to disable FK for migration: %v", err)
	}

	migration0003, err := fs.ReadFile(migrationsFS, "migrations/0003_approved_state.sql")
	if err != nil {
		t.Fatalf("failed to read migration 0003: %v", err)
	}
	for _, stmt := range splitStatements(string(migration0003)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("failed to execute 0003: %v", err)
		}
	}

	// Re-enable FK and check integrity
	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("failed to enable FK after migration: %v", err)
	}

	// Record that 0003 was applied
	if _, err := conn.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", "0003", now); err != nil {
		t.Fatalf("failed to record 0003: %v", err)
	}

	// Step 4: Verify row counts survived
	for table, initialCount := range initialCounts {
		var finalCount int
		if err := conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&finalCount); err != nil {
			t.Fatalf("failed to count %s after migration: %v", table, err)
		}
		if finalCount != initialCount {
			t.Errorf("%s count mismatch after 0003: initial=%d, final=%d", table, initialCount, finalCount)
		}
	}

	// Step 5: Verify task field values survived (field-by-field check catches column-order bugs)
	finalTasks := make(map[string]TaskRow)
	rows, err = conn.Query(`
		SELECT id, title, spec, state, assignee, result FROM task ORDER BY id
	`)
	if err != nil {
		t.Fatalf("failed to query final tasks: %v", err)
	}
	for rows.Next() {
		var tr TaskRow
		if err := rows.Scan(&tr.id, &tr.title, &tr.spec, &tr.state, &tr.assignee, &tr.result); err != nil {
			t.Fatalf("failed to scan final task: %v", err)
		}
		finalTasks[tr.id] = tr
	}
	rows.Close()

	for taskID, initialTask := range initialTasks {
		finalTask, exists := finalTasks[taskID]
		if !exists {
			t.Errorf("task %s missing after migration 0003", taskID)
			continue
		}
		if initialTask.title != finalTask.title {
			t.Errorf("task %s title mismatch: initial=%q, final=%q", taskID, initialTask.title, finalTask.title)
		}
		if initialTask.spec != finalTask.spec {
			t.Errorf("task %s spec mismatch: initial=%q, final=%q", taskID, initialTask.spec, finalTask.spec)
		}
		if initialTask.state != finalTask.state {
			t.Errorf("task %s state mismatch: initial=%q, final=%q", taskID, initialTask.state, finalTask.state)
		}
		if initialTask.assignee != finalTask.assignee {
			t.Errorf("task %s assignee mismatch: initial=%v, final=%v", taskID, initialTask.assignee, finalTask.assignee)
		}
		if initialTask.result != finalTask.result {
			t.Errorf("task %s result mismatch: initial=%v, final=%v", taskID, initialTask.result, finalTask.result)
		}
	}

	// Step 6: Verify FK integrity (PRAGMA foreign_key_check should return no violations)
	fkRows, err := conn.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("failed to run foreign_key_check: %v", err)
	}
	defer fkRows.Close()

	fkViolationCount := 0
	for fkRows.Next() {
		fkViolationCount++
		var table, rowid, parent, fkid string
		if err := fkRows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Errorf("failed to scan FK violation: %v", err)
		}
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("error iterating FK check results: %v", err)
	}
	if fkViolationCount > 0 {
		t.Errorf("found %d foreign key violations after migration 0003", fkViolationCount)
	}

	// Step 7: Verify FK constraints still enforced
	_, err = conn.Exec(`
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "bad-task", "non-existent-proj", "non-existent-doc", "Bad Task", "bad spec", "backlog", now, now)
	if err == nil {
		t.Error("expected FK constraint violation after 0003, but insert succeeded")
	}

	// Step 8: Verify 'approved' state is now accepted by the widened CHECK constraint
	testTaskID := "task-0003-1a"
	if _, err := conn.Exec(`UPDATE task SET state = ? WHERE id = ?`, "approved", testTaskID); err != nil {
		t.Errorf("failed to update task to 'approved' state: %v", err)
	}

	var state string
	if err := conn.QueryRow("SELECT state FROM task WHERE id = ?", testTaskID).Scan(&state); err != nil {
		t.Fatalf("failed to query task state: %v", err)
	}
	if state != "approved" {
		t.Errorf("expected task state='approved', got '%s'", state)
	}

	// Step 9: Verify invalid states are still rejected
	_, err = conn.Exec(`UPDATE task SET state = ? WHERE id = ?`, "invalid-state", testTaskID)
	if err == nil {
		t.Error("expected CHECK constraint violation for invalid state, but update succeeded")
	}

	// Step 10: Verify a new task can be inserted with 'approved' state
	if _, err := conn.Exec(`
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "new-approved-task", "proj-0003-1", "doc-0003-1", "New Approved", "spec", "approved", now, now); err != nil {
		t.Errorf("failed to insert task with 'approved' state: %v", err)
	}

	var approvedCount int
	if err := conn.QueryRow("SELECT COUNT(*) FROM task WHERE state = ?", "approved").Scan(&approvedCount); err != nil {
		t.Fatalf("failed to count approved tasks: %v", err)
	}
	if approvedCount != 2 {
		t.Errorf("expected 2 approved tasks, got %d", approvedCount)
	}
}

// TestMigration0004AddTaskColumns verifies that migration 0004 (adding model, kind, review_models, review_round, target_task_id, verdict columns):
// 1. Applies cleanly to a database populated with tasks
// 2. All existing rows remain intact
// 3. New columns exist with correct defaults (model='haiku', kind='implement', review_round=0, nullable columns empty)
// 4. The model-matched claimable index exists
// 5. Foreign keys still resolve
func TestMigration0004AddTaskColumns(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "migration_0004_test.db")

	// Step 1: Build a fresh DB at PRE-0004 schema by executing 0001, 0002, and 0003 migrations
	conn, err := sql.Open("sqlite", buildDSN(dbPath))
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer conn.Close()

	conn.SetMaxOpenConns(1)

	// Disable FK for initial schema creation
	if _, err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("failed to disable foreign keys: %v", err)
	}

	// Execute 0001 migration
	migration0001, err := fs.ReadFile(migrationsFS, "migrations/0001_init.sql")
	if err != nil {
		t.Fatalf("failed to read migration 0001: %v", err)
	}
	for _, stmt := range splitStatements(string(migration0001)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("failed to execute 0001: %v", err)
		}
	}

	// Execute 0002 migration
	migration0002, err := fs.ReadFile(migrationsFS, "migrations/0002_document_one_design.sql")
	if err != nil {
		t.Fatalf("failed to read migration 0002: %v", err)
	}
	for _, stmt := range splitStatements(string(migration0002)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("failed to execute 0002: %v", err)
		}
	}

	// Execute 0003 migration (table rebuild for 'approved' state)
	migration0003, err := fs.ReadFile(migrationsFS, "migrations/0003_approved_state.sql")
	if err != nil {
		t.Fatalf("failed to read migration 0003: %v", err)
	}
	for _, stmt := range splitStatements(string(migration0003)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("failed to execute 0003: %v", err)
		}
	}

	// Record that 0001, 0002, 0003 were applied
	if _, err := conn.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		t.Fatalf("failed to create schema_migrations: %v", err)
	}
	now := nowTimestamp()
	for _, version := range []string{"0001", "0002", "0003"} {
		if _, err := conn.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", version, now); err != nil {
			t.Fatalf("failed to record %s: %v", version, err)
		}
	}

	// Re-enable FK for data validation
	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("failed to enable foreign keys: %v", err)
	}

	// Step 2: Seed test data into the PRE-0004 schema
	// Create project
	if _, err := conn.Exec(`
		INSERT INTO project (id, name, repo, created_at) VALUES (?, ?, ?, ?)
	`, "proj-0004", "test-repo", "https://example.com/repo", now); err != nil {
		t.Fatalf("failed to insert project: %v", err)
	}

	// Create document
	if _, err := conn.Exec(`
		INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
	`, "doc-0004", "proj-0004", "design", "Design", "DESIGN.md", now, now); err != nil {
		t.Fatalf("failed to insert document: %v", err)
	}

	// Create tasks in various states
	taskData := []struct {
		id    string
		state string
	}{
		{"task-0004-1", "backlog"},
		{"task-0004-2", "ready"},
		{"task-0004-3", "in_progress"},
		{"task-0004-4", "review"},
		{"task-0004-5", "approved"},
		{"task-0004-6", "done"},
	}

	for _, td := range taskData {
		if _, err := conn.Exec(`
			INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, td.id, "proj-0004", "doc-0004", td.id, "spec for "+td.id, td.state, now, now); err != nil {
			t.Fatalf("failed to insert task %s: %v", td.id, err)
		}
	}

	// Record initial row counts
	var initialTaskCount int
	if err := conn.QueryRow("SELECT COUNT(*) FROM task").Scan(&initialTaskCount); err != nil {
		t.Fatalf("failed to count initial tasks: %v", err)
	}

	// Step 3: Apply migration 0004 to the populated database
	if _, err := conn.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatalf("failed to disable FK for migration: %v", err)
	}

	migration0004, err := fs.ReadFile(migrationsFS, "migrations/0004_add_task_columns.sql")
	if err != nil {
		t.Fatalf("failed to read migration 0004: %v", err)
	}
	for _, stmt := range splitStatements(string(migration0004)) {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := conn.Exec(stmt); err != nil {
			t.Fatalf("failed to execute 0004: %v", err)
		}
	}

	// Re-enable FK and check integrity
	if _, err := conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatalf("failed to enable FK after migration: %v", err)
	}

	// Record that 0004 was applied
	if _, err := conn.Exec("INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)", "0004", now); err != nil {
		t.Fatalf("failed to record 0004: %v", err)
	}

	// Step 4: Verify row counts survived
	var finalTaskCount int
	if err := conn.QueryRow("SELECT COUNT(*) FROM task").Scan(&finalTaskCount); err != nil {
		t.Fatalf("failed to count final tasks: %v", err)
	}
	if finalTaskCount != initialTaskCount {
		t.Errorf("task count mismatch after 0004: initial=%d, final=%d", initialTaskCount, finalTaskCount)
	}

	// Step 5: Verify new columns exist with correct defaults
	type TaskWithNewColumns struct {
		id           string
		model        string
		kind         string
		reviewModels sql.NullString
		reviewRound  int
		targetTaskID sql.NullString
		verdict      sql.NullString
	}

	rows, err := conn.Query(`
		SELECT id, model, kind, review_models, review_round, target_task_id, verdict
		FROM task ORDER BY id
	`)
	if err != nil {
		t.Fatalf("failed to query tasks with new columns: %v", err)
	}
	defer rows.Close()

	var tasksWithNewCols []TaskWithNewColumns
	for rows.Next() {
		var row TaskWithNewColumns
		if err := rows.Scan(&row.id, &row.model, &row.kind, &row.reviewModels, &row.reviewRound, &row.targetTaskID, &row.verdict); err != nil {
			t.Fatalf("failed to scan task: %v", err)
		}
		tasksWithNewCols = append(tasksWithNewCols, row)
	}

	if len(tasksWithNewCols) != initialTaskCount {
		t.Errorf("expected %d tasks, got %d", initialTaskCount, len(tasksWithNewCols))
	}

	// Verify defaults on all tasks
	for _, task := range tasksWithNewCols {
		if task.model != "haiku" {
			t.Errorf("task %s model: expected 'haiku', got '%s'", task.id, task.model)
		}
		if task.kind != "implement" {
			t.Errorf("task %s kind: expected 'implement', got '%s'", task.id, task.kind)
		}
		if task.reviewRound != 0 {
			t.Errorf("task %s review_round: expected 0, got %d", task.id, task.reviewRound)
		}
		if task.reviewModels.Valid || task.targetTaskID.Valid || task.verdict.Valid {
			t.Errorf("task %s nullable columns should be NULL: review_models.Valid=%v, target_task_id.Valid=%v, verdict.Valid=%v",
				task.id, task.reviewModels.Valid, task.targetTaskID.Valid, task.verdict.Valid)
		}
	}

	// Step 6: Verify the model-matched claimable index exists
	var indexCount int
	err = conn.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_task_claimable'
	`).Scan(&indexCount)
	if err != nil {
		t.Fatalf("failed to check for idx_task_claimable: %v", err)
	}
	if indexCount != 1 {
		t.Errorf("expected idx_task_claimable to exist, but count=%d", indexCount)
	}

	// Step 7: Verify FK integrity
	fkRows, err := conn.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("failed to run foreign_key_check: %v", err)
	}
	defer fkRows.Close()

	fkViolationCount := 0
	for fkRows.Next() {
		fkViolationCount++
	}
	if fkViolationCount > 0 {
		t.Errorf("found %d foreign key violations after migration 0004", fkViolationCount)
	}

	// Step 8: Verify FK constraints still enforced
	_, err = conn.Exec(`
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, "bad-task", "non-existent-proj", "non-existent-doc", "Bad Task", "bad spec", "ready", now, now)
	if err == nil {
		t.Error("expected FK constraint violation after 0004, but insert succeeded")
	}

	// Step 9: Verify target_task_id FK works
	// First insert a task that will be the target
	if _, err := conn.Exec(`
		INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at, kind)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "task-0004-review", "proj-0004", "doc-0004", "Review Task", "review spec", "ready", now, now, "review"); err != nil {
		t.Fatalf("failed to insert review task: %v", err)
	}

	// Update it with a valid target_task_id FK
	if _, err := conn.Exec(`
		UPDATE task SET target_task_id = ? WHERE id = ?
	`, "task-0004-1", "task-0004-review"); err != nil {
		t.Fatalf("failed to set target_task_id with valid FK: %v", err)
	}

	// Verify the FK is enforced
	_, err = conn.Exec(`
		UPDATE task SET target_task_id = ? WHERE id = ?
	`, "non-existent-task", "task-0004-review")
	if err == nil {
		t.Error("expected FK constraint violation for target_task_id, but update succeeded")
	}
}

// TestTaskFieldsRoundTrip verifies that the new fields (model, kind, review_models, review_round, target_task_id)
// are properly persisted and retrieved through the Go layer.
func TestTaskFieldsRoundTrip(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Test 1: Create task with explicit model and review_models
	reviewModels := []string{"opus", "sonnet"}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Task with model",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "sonnet",
			ReviewModels: reviewModels,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	taskWithModel := tasks[0]
	if taskWithModel.Model != "sonnet" {
		t.Errorf("expected model='sonnet', got '%s'", taskWithModel.Model)
	}
	if taskWithModel.Kind != "implement" {
		t.Errorf("expected kind='implement', got '%s'", taskWithModel.Kind)
	}
	if len(taskWithModel.ReviewModels) != 2 || taskWithModel.ReviewModels[0] != "opus" || taskWithModel.ReviewModels[1] != "sonnet" {
		t.Errorf("expected review_models=['opus','sonnet'], got %v", taskWithModel.ReviewModels)
	}
	if taskWithModel.ReviewRound != 0 {
		t.Errorf("expected review_round=0, got %d", taskWithModel.ReviewRound)
	}
	if taskWithModel.TargetTaskID != nil {
		t.Errorf("expected target_task_id=nil, got %v", taskWithModel.TargetTaskID)
	}

	// Test 2: GetTask and verify fields are returned
	retrieved, err := store.GetTask(ctx, taskWithModel.ID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if retrieved.Model != "sonnet" {
		t.Errorf("GetTask: expected model='sonnet', got '%s'", retrieved.Model)
	}
	if retrieved.Kind != "implement" {
		t.Errorf("GetTask: expected kind='implement', got '%s'", retrieved.Kind)
	}
	if len(retrieved.ReviewModels) != 2 || retrieved.ReviewModels[0] != "opus" || retrieved.ReviewModels[1] != "sonnet" {
		t.Errorf("GetTask: expected review_models=['opus','sonnet'], got %v", retrieved.ReviewModels)
	}
	if retrieved.ReviewRound != 0 {
		t.Errorf("GetTask: expected review_round=0, got %d", retrieved.ReviewRound)
	}
	if retrieved.TargetTaskID != nil {
		t.Errorf("GetTask: expected target_task_id=nil, got %v", retrieved.TargetTaskID)
	}

	// Test 3: ListTasks and verify fields are returned
	listedTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	if len(listedTasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(listedTasks))
	}

	listedTask := listedTasks[0]
	if listedTask.Model != "sonnet" {
		t.Errorf("ListTasks: expected model='sonnet', got '%s'", listedTask.Model)
	}
	if listedTask.Kind != "implement" {
		t.Errorf("ListTasks: expected kind='implement', got '%s'", listedTask.Kind)
	}
	if len(listedTask.ReviewModels) != 2 || listedTask.ReviewModels[0] != "opus" || listedTask.ReviewModels[1] != "sonnet" {
		t.Errorf("ListTasks: expected review_models=['opus','sonnet'], got %v", listedTask.ReviewModels)
	}

	// Test 4: Create task with default model (no explicit value)
	defaultTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Task with default model",
			Spec:       "Test spec",
			DocumentID: doc.ID,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task with defaults: %v", err)
	}

	defaultTask := defaultTasks[0]
	if defaultTask.Model != "haiku" {
		t.Errorf("expected default model='haiku', got '%s'", defaultTask.Model)
	}
	if defaultTask.Kind != "implement" {
		t.Errorf("expected default kind='implement', got '%s'", defaultTask.Kind)
	}
	if len(defaultTask.ReviewModels) != 0 {
		t.Errorf("expected empty review_models when not specified, got %v", defaultTask.ReviewModels)
	}
	if defaultTask.ReviewRound != 0 {
		t.Errorf("expected default review_round=0, got %d", defaultTask.ReviewRound)
	}

	// Test 5: Verify JSON marshaling normalizes empty review_models to [] not null
	jsonData, err := json.Marshal(defaultTask)
	if err != nil {
		t.Fatalf("failed to marshal task: %v", err)
	}
	// Unmarshal to check structure
	var jsonObj map[string]interface{}
	if err := json.Unmarshal(jsonData, &jsonObj); err != nil {
		t.Fatalf("failed to unmarshal json: %v", err)
	}
	reviewModelsVal := jsonObj["review_models"]
	if reviewModelsVal == nil {
		t.Errorf("review_models should not be null in JSON, should be []")
	}
	// Check it's an array (even if empty)
	if reviewModelsVal != nil {
		switch reviewModelsVal.(type) {
		case []interface{}:
			// OK
		default:
			t.Errorf("review_models should be a JSON array, got type %T", reviewModelsVal)
		}
	}
}

// TestGetTaskPrefixResolution verifies that GetTask resolves a unique
// 8-to-35-character id prefix to the full task, that dependencies and links
// are looked up against the resolved full id (not the truncated prefix),
// that no match or an id shorter than 8 characters returns ErrNotFound, that
// several matches return a *ConflictError with Code AMBIGUOUS_ID listing
// every candidate, and that the 36-character exact-id path is unchanged.
func TestGetTaskPrefixResolution(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	proj, err := store.CreateProject(ctx, "prefix-test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	insertTask := func(id, title string) {
		if _, err := store.Conn().ExecContext(ctx, `
			INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, id, proj.ID, doc.ID, title, "spec", "backlog", now, now); err != nil {
			t.Fatalf("failed to insert task %s: %v", id, err)
		}
	}

	const (
		depTaskID     = "dep-task-full-id-000001"
		uniqueTaskID  = "uniq-task-full-id-000001"
		ambiguousID1  = "ambig0001-task-full-a"
		ambiguousID2  = "ambig0001-task-full-b"
		ambiguousPfx  = "ambig0001"
		uniquePfx     = "uniq-tas" // uniqueTaskID[:8]
		noMatchPfx    = "zzzzzzzz"
		tooShortInput = "ambig" // shorter than 8, must not even query
	)

	insertTask(depTaskID, "Dependency Task")
	insertTask(uniqueTaskID, "Unique Prefix Task")
	insertTask(ambiguousID1, "Ambiguous Task 1")
	insertTask(ambiguousID2, "Ambiguous Task 2")

	if _, err := store.Conn().ExecContext(ctx, `
		INSERT INTO task_dep (task_id, depends_on_id) VALUES (?, ?)
	`, uniqueTaskID, depTaskID); err != nil {
		t.Fatalf("failed to insert task_dep: %v", err)
	}
	if _, err := store.Conn().ExecContext(ctx, `
		INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, ?, ?)
	`, "prefix-test-link", uniqueTaskID, "pr", "#456"); err != nil {
		t.Fatalf("failed to insert task_link: %v", err)
	}

	t.Run("exact id unchanged", func(t *testing.T) {
		got, err := store.GetTask(ctx, uniqueTaskID)
		if err != nil {
			t.Fatalf("GetTask(exact id) failed: %v", err)
		}
		if got.ID != uniqueTaskID {
			t.Errorf("expected id %s, got %s", uniqueTaskID, got.ID)
		}
	})

	t.Run("unique prefix resolves and preserves deps and links", func(t *testing.T) {
		got, err := store.GetTask(ctx, uniquePfx)
		if err != nil {
			t.Fatalf("GetTask(prefix) failed: %v", err)
		}
		if got.ID != uniqueTaskID {
			t.Errorf("expected resolved id %s, got %s", uniqueTaskID, got.ID)
		}
		if len(got.DependsOn) != 1 || got.DependsOn[0] != depTaskID {
			t.Errorf("expected DependsOn=[%s], got %v (prefix lookup must resolve the full id before querying task_dep)", depTaskID, got.DependsOn)
		}
		if len(got.Links) != 1 || got.Links[0].Value != "#456" {
			t.Errorf("expected one link with value #456, got %v (prefix lookup must resolve the full id before querying task_link)", got.Links)
		}
	})

	t.Run("no match returns not found", func(t *testing.T) {
		_, err := store.GetTask(ctx, noMatchPfx)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound, got %v", err)
		}
	})

	t.Run("shorter than 8 chars returns not found", func(t *testing.T) {
		_, err := store.GetTask(ctx, tooShortInput)
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for a %d-char id, got %v", len(tooShortInput), err)
		}
	})

	t.Run("ambiguous prefix returns conflict with candidates", func(t *testing.T) {
		_, err := store.GetTask(ctx, ambiguousPfx)
		var conflictErr *ConflictError
		if !errors.As(err, &conflictErr) {
			t.Fatalf("expected *ConflictError, got %v", err)
		}
		if conflictErr.Code != "AMBIGUOUS_ID" {
			t.Errorf("expected Code=AMBIGUOUS_ID, got %s", conflictErr.Code)
		}
		gotCandidates := append([]string(nil), conflictErr.Candidates...)
		sort.Strings(gotCandidates)
		wantCandidates := []string{ambiguousID1, ambiguousID2}
		if len(gotCandidates) != len(wantCandidates) || gotCandidates[0] != wantCandidates[0] || gotCandidates[1] != wantCandidates[1] {
			t.Errorf("expected candidates %v, got %v", wantCandidates, gotCandidates)
		}
		for _, id := range wantCandidates {
			if !strings.Contains(conflictErr.Message, id) {
				t.Errorf("expected message to mention candidate %s, got %q", id, conflictErr.Message)
			}
		}
	})

	t.Run("LIKE wildcards in prefix are matched literally", func(t *testing.T) {
		// A prefix of eight underscores must not behave as LIKE single-char
		// wildcards matching every row; an unescaped `LIKE '________%'` would
		// match all four tasks and report AMBIGUOUS_ID. Escaped, it matches no
		// id (no stored id contains a literal underscore) -> not found.
		if _, err := store.GetTask(ctx, "________"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for all-underscore prefix (wildcards must be escaped), got %v", err)
		}
		// '_' standing in for a real character must not match either:
		// uniqueTaskID is "uniq-task-...", so "uniq_tas" resolves to it only if
		// '_' is treated as a wildcard. It must be literal -> not found.
		if _, err := store.GetTask(ctx, "uniq_tas"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for underscore-as-wildcard prefix, got %v", err)
		}
		// '%' (multi-char wildcard) must likewise be literal.
		if _, err := store.GetTask(ctx, "%%%%%%%%"); !errors.Is(err, ErrNotFound) {
			t.Errorf("expected ErrNotFound for all-percent prefix (wildcards must be escaped), got %v", err)
		}
	})
}

// TestCreateTasksWithConfiguredAllowlist verifies that model allowlist validation works.
// Models not in the allowlist are rejected with UNKNOWN_MODEL.
func TestCreateTasksWithConfiguredAllowlist(t *testing.T) {
	ctx := context.Background()

	// Test 1: Create a store with a custom allowlist (opus,sonnet only, no haiku)
	customAllowlist := []string{"opus", "sonnet"}
	store, err := Open("file::memory:?cache=shared", customAllowlist)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Test 2: Try to create a task with a model not in the allowlist (haiku)
	// Should fail with UNKNOWN_MODEL
	_, err = store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Haiku Task",
			Spec:       "Test spec",
			DocumentID: doc.ID,
			Model:      "haiku",
		},
	})
	if err == nil {
		t.Error("expected UNKNOWN_MODEL error for haiku model, but creation succeeded")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Errorf("expected ValidationError, got %T", err)
	} else if valErr.Code != "UNKNOWN_MODEL" {
		t.Errorf("expected error code UNKNOWN_MODEL, got %s", valErr.Code)
	}

	// Test 3: Create a task with a model in the allowlist (opus) should succeed
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Opus Task",
			Spec:       "Test spec",
			DocumentID: doc.ID,
			Model:      "opus",
		},
	})
	if err != nil {
		t.Errorf("expected opus task creation to succeed, got error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Model != "opus" {
		t.Errorf("expected task with model='opus', got %v", tasks)
	}

	// Test 4: Create a task with sonnet model (also in allowlist) should succeed
	tasks, err = store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Sonnet Task",
			Spec:       "Test spec",
			DocumentID: doc.ID,
			Model:      "sonnet",
		},
	})
	if err != nil {
		t.Errorf("expected sonnet task creation to succeed, got error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Model != "sonnet" {
		t.Errorf("expected task with model='sonnet', got %v", tasks)
	}

	// Test 5: Create a task with no explicit model (should default to first in allowlist, opus)
	tasks, err = store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Default Model Task",
			Spec:       "Test spec",
			DocumentID: doc.ID,
		},
	})
	if err != nil {
		t.Errorf("expected default model task creation to succeed, got error: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Model != "opus" {
		t.Errorf("expected task with default model='opus', got model='%s'", tasks[0].Model)
	}

	// Test 6: Create a task with review_models not in allowlist should fail
	_, err = store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Task with bad review model",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "opus",
			ReviewModels: []string{"haiku"}, // haiku not in allowlist
		},
	})
	if err == nil {
		t.Error("expected UNKNOWN_MODEL error for review_models with haiku, but creation succeeded")
	}
	if !errors.As(err, &valErr) {
		t.Errorf("expected ValidationError, got %T", err)
	} else if valErr.Code != "UNKNOWN_MODEL" {
		t.Errorf("expected error code UNKNOWN_MODEL, got %s", valErr.Code)
	}

	// Test 7: Create a task with review_models in allowlist should succeed
	tasks, err = store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Task with good review models",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "opus",
			ReviewModels: []string{"opus", "sonnet"},
		},
	})
	if err != nil {
		t.Errorf("expected task with review_models creation to succeed, got error: %v", err)
	}
	if len(tasks) != 1 || len(tasks[0].ReviewModels) != 2 {
		t.Errorf("expected task with 2 review_models, got %v", tasks)
	}
}

// TestSubmitImplementTaskAutoSpawnsReviewTasks_MultiReviewer tests that submitting an implement task
// with multiple required reviewers creates exactly that many review tasks.
func TestSubmitImplementTaskAutoSpawnsReviewTasks_MultiReviewer(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and implement task with two reviewers
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implementation task",
			Spec:         "Implement feature X",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus", "sonnet"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote and claim the task
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit the task
	result := "Implementation complete"
	links := []LinkInput{{Kind: "pr", Value: "#123"}}
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", result, nil, links, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Verify the task is in review and review_round is 1
	if submitted.State != "review" {
		t.Errorf("expected state='review', got '%s'", submitted.State)
	}
	if submitted.ReviewRound != 1 {
		t.Errorf("expected review_round=1, got %d", submitted.ReviewRound)
	}

	// Verify exactly 2 review tasks were created with the correct models
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	reviewTaskModels := []string{}
	reviewTasksForParent := 0
	for _, rt := range reviewTasks {
		if rt.Kind == "review" && rt.TargetTaskID != nil && *rt.TargetTaskID == taskID {
			reviewTasksForParent++
			reviewTaskModels = append(reviewTaskModels, rt.Model)

			// Verify the review task properties
			if rt.State != "ready" {
				t.Errorf("expected review task state='ready', got '%s'", rt.State)
			}
			if rt.ReviewRound != 1 {
				t.Errorf("expected review task review_round=1, got %d", rt.ReviewRound)
			}
		}
	}

	if reviewTasksForParent != 2 {
		t.Errorf("expected 2 review tasks, got %d", reviewTasksForParent)
	}

	// Verify the exact set of models: one opus and one sonnet
	sort.Strings(reviewTaskModels)
	expectedModels := []string{"opus", "sonnet"}
	modelsMatch := len(reviewTaskModels) == 2 && reviewTaskModels[0] == expectedModels[0] && reviewTaskModels[1] == expectedModels[1]
	if !modelsMatch {
		t.Errorf("expected review task models to be [opus, sonnet], got %v", reviewTaskModels)
	}

	// Verify a spawn_review event was recorded
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	spawnReviewFound := false
	for _, e := range events {
		if e.Kind == "spawn_review" {
			spawnReviewFound = true
			break
		}
	}
	if !spawnReviewFound {
		t.Error("expected spawn_review event not found")
	}
}

// TestSubmitImplementTaskAutoSpawnsReviewTasks_TrackPropagation tests that spawned review tasks
// inherit the parent task's track field.
func TestSubmitImplementTaskAutoSpawnsReviewTasks_TrackPropagation(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Test case 1: design-track parent
	designTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Design implementation task",
			Spec:         "Implement design feature",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"sonnet"},
			Track:        "design",
		},
	})
	if err != nil {
		t.Fatalf("failed to create design-track task: %v", err)
	}
	designTaskID := designTasks[0].ID

	// Promote and claim the design-track task
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", designTaskID)
	if err != nil {
		t.Fatalf("failed to promote design-track task: %v", err)
	}

	_, err = store.ClaimTask(ctx, designTaskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim design-track task: %v", err)
	}

	// Submit the design-track task
	_, err = store.SubmitTask(ctx, designTaskID, "agent-1", "Design complete", nil, []LinkInput{{Kind: "pr", Value: "#123"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit design-track task: %v", err)
	}

	// Test case 2: build-track parent (explicit)
	buildTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Build implementation task",
			Spec:         "Implement build feature",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Track:        "build",
		},
	})
	if err != nil {
		t.Fatalf("failed to create build-track task: %v", err)
	}
	buildTaskID := buildTasks[0].ID

	// Promote and claim the build-track task
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", buildTaskID)
	if err != nil {
		t.Fatalf("failed to promote build-track task: %v", err)
	}

	_, err = store.ClaimTask(ctx, buildTaskID, "agent-2", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim build-track task: %v", err)
	}

	// Submit the build-track task
	_, err = store.SubmitTask(ctx, buildTaskID, "agent-2", "Build complete", nil, []LinkInput{{Kind: "pr", Value: "#456"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit build-track task: %v", err)
	}

	// Test case 3: default track (no track specified, should default to 'build')
	defaultTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Default implementation task",
			Spec:         "Implement default feature",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			// Track not specified - should default to 'build'
		},
	})
	if err != nil {
		t.Fatalf("failed to create default-track task: %v", err)
	}
	defaultTaskID := defaultTasks[0].ID

	// Promote and claim the default-track task
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", defaultTaskID)
	if err != nil {
		t.Fatalf("failed to promote default-track task: %v", err)
	}

	_, err = store.ClaimTask(ctx, defaultTaskID, "agent-3", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim default-track task: %v", err)
	}

	// Submit the default-track task
	_, err = store.SubmitTask(ctx, defaultTaskID, "agent-3", "Default complete", nil, []LinkInput{{Kind: "pr", Value: "#789"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit default-track task: %v", err)
	}

	// Verify all review tasks have the correct track
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	// Check design-track spawned reviews
	var designReviewCount int
	for _, task := range allTasks {
		if task.Kind == "review" && task.TargetTaskID != nil && *task.TargetTaskID == designTaskID {
			designReviewCount++
			if task.Track != "design" {
				t.Errorf("design-track parent spawned review with track='%s', expected 'design'", task.Track)
			}
		}
	}
	if designReviewCount != 1 {
		t.Errorf("expected 1 design-track review, got %d", designReviewCount)
	}

	// Check build-track spawned reviews
	var buildReviewCount int
	for _, task := range allTasks {
		if task.Kind == "review" && task.TargetTaskID != nil && *task.TargetTaskID == buildTaskID {
			buildReviewCount++
			if task.Track != "build" {
				t.Errorf("build-track parent spawned review with track='%s', expected 'build'", task.Track)
			}
		}
	}
	if buildReviewCount != 1 {
		t.Errorf("expected 1 build-track review, got %d", buildReviewCount)
	}

	// Check default-track spawned reviews (should be 'build')
	var defaultReviewCount int
	for _, task := range allTasks {
		if task.Kind == "review" && task.TargetTaskID != nil && *task.TargetTaskID == defaultTaskID {
			defaultReviewCount++
			if task.Track != "build" {
				t.Errorf("default-track parent spawned review with track='%s', expected 'build'", task.Track)
			}
		}
	}
	if defaultReviewCount != 1 {
		t.Errorf("expected 1 default-track review (should be 'build'), got %d", defaultReviewCount)
	}
}

// TestSubmitImplementTaskAutoSpawnsReviewTasks_DefaultSingleOpus tests that submitting
// an implement task with no reviewers specified creates exactly one Opus review task.
func TestSubmitImplementTaskAutoSpawnsReviewTasks_DefaultSingleOpus(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and implement task with no review models specified
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Implementation task",
			Spec:       "Implement feature Y",
			DocumentID: doc.ID,
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote and claim the task
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit the task
	result := "Implementation complete"
	links := []LinkInput{{Kind: "pr", Value: "#456"}}
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", result, nil, links, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Verify the task is in review and review_round is 1
	if submitted.State != "review" {
		t.Errorf("expected state='review', got '%s'", submitted.State)
	}
	if submitted.ReviewRound != 1 {
		t.Errorf("expected review_round=1, got %d", submitted.ReviewRound)
	}

	// Verify exactly 1 review task was created with model=opus
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	reviewTaskCount := 0
	for _, rt := range reviewTasks {
		if rt.Kind == "review" && rt.TargetTaskID != nil && *rt.TargetTaskID == taskID {
			reviewTaskCount++
			if rt.Model != "opus" {
				t.Errorf("expected default review model to be 'opus', got '%s'", rt.Model)
			}
		}
	}

	if reviewTaskCount != 1 {
		t.Errorf("expected 1 review task, got %d", reviewTaskCount)
	}
}

// TestSubmitImplementTaskResubmitAfterBounce tests that resubmitting after a bounce
// creates a fresh round and leaves the prior round's review tasks untouched.
func TestSubmitImplementTaskResubmitAfterBounce(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and implement task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implementation task",
			Spec:         "Implement feature Z",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// First submission cycle
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("first claim failed: %v", err)
	}

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "First implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("first submit failed: %v", err)
	}

	// Get the review task ID from round 1
	var round1ReviewTaskID string
	err = store.Conn().QueryRowContext(ctx, `
		SELECT id FROM task WHERE kind='review' AND target_task_id=? AND review_round=1
	`, taskID).Scan(&round1ReviewTaskID)
	if err != nil {
		t.Fatalf("failed to get round 1 review task: %v", err)
	}

	// Simulate a bounce back to ready
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET state='ready', assignee=NULL, lease_expires_at=NULL WHERE id=?
	`, taskID)
	if err != nil {
		t.Fatalf("failed to bounce task: %v", err)
	}

	// Second submission cycle (rework)
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("second claim failed: %v", err)
	}

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Fixed implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("second submit failed: %v", err)
	}

	// Verify we now have review tasks from both rounds
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	round1Count := 0
	round2Count := 0
	for _, rt := range reviewTasks {
		if rt.Kind == "review" && rt.TargetTaskID != nil && *rt.TargetTaskID == taskID {
			if rt.ReviewRound == 1 {
				round1Count++
			} else if rt.ReviewRound == 2 {
				round2Count++
			}
		}
	}

	if round1Count != 1 {
		t.Errorf("expected 1 review task from round 1, got %d", round1Count)
	}
	if round2Count != 1 {
		t.Errorf("expected 1 review task from round 2, got %d", round2Count)
	}

	// Verify the round 1 review task still exists
	var stillExists int
	err = store.Conn().QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task WHERE id=? AND review_round=1
	`, round1ReviewTaskID).Scan(&stillExists)
	if err != nil {
		t.Fatalf("failed to check round 1 review task: %v", err)
	}
	if stillExists != 1 {
		t.Error("round 1 review task should still exist")
	}
}

// TestSubmitTaskIdempotentLinks verifies that submitting a task multiple times with the same
// links does not create duplicate link rows.
func TestSubmitTaskIdempotentLinks(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Feature", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: doc.ID,
			Model:      "opus",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	task := tasks[0]

	// Promote task from backlog to ready
	task, err = store.PromoteTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	// Claim the task
	claimedTask, err := store.ClaimTask(ctx, task.ID, "agent-1", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if claimedTask.State != "in_progress" {
		t.Errorf("expected task state 'in_progress', got %s", claimedTask.State)
	}

	// First submission with PR and branch links
	links := []LinkInput{
		{Kind: "pr", Value: "https://github.com/test/repo/pull/123"},
		{Kind: "branch", Value: "feature/test-branch"},
	}
	submittedTask, err := store.SubmitTask(ctx, task.ID, "agent-1", "result of work", nil, links, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("first submit failed: %v", err)
	}
	if submittedTask.State != "review" {
		t.Errorf("expected task state 'review', got %s", submittedTask.State)
	}

	// Verify we have exactly 2 links after first submission
	if len(submittedTask.Links) != 2 {
		t.Errorf("expected 2 links after first submission, got %d", len(submittedTask.Links))
	}

	// Verify the link values are correct
	linkMap := make(map[string]string)
	for _, link := range submittedTask.Links {
		linkMap[link.Kind] = link.Value
	}
	if linkMap["pr"] != "https://github.com/test/repo/pull/123" {
		t.Errorf("PR link mismatch: got %s", linkMap["pr"])
	}
	if linkMap["branch"] != "feature/test-branch" {
		t.Errorf("branch link mismatch: got %s", linkMap["branch"])
	}

	// Move task back to in_progress to simulate rework
	now := nowTimestamp()
	leaseExpiry := leaseExpiryTimestamp(5 * time.Minute)
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET state='in_progress', assignee='agent-1', lease_expires_at=?, updated_at=? WHERE id=?
	`, leaseExpiry, now, task.ID)
	if err != nil {
		t.Fatalf("failed to reset task state: %v", err)
	}

	// Second submission with same links (testing idempotency)
	submittedTask2, err := store.SubmitTask(ctx, task.ID, "agent-1", "updated result", nil, links, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("second submit failed: %v", err)
	}

	// Verify we still have exactly 2 links (not 4)
	if len(submittedTask2.Links) != 2 {
		t.Errorf("expected 2 links after second submission with same links, got %d", len(submittedTask2.Links))
	}

	// Move task back to in_progress again for a third submission with a new link
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET state='in_progress', assignee='agent-1', lease_expires_at=?, updated_at=? WHERE id=?
	`, leaseExpiry, now, task.ID)
	if err != nil {
		t.Fatalf("failed to reset task state for third submission: %v", err)
	}

	// Third submission: same PR/branch links + a new commit link
	linksWithCommit := []LinkInput{
		{Kind: "pr", Value: "https://github.com/test/repo/pull/123"},
		{Kind: "branch", Value: "feature/test-branch"},
		{Kind: "commit", Value: "abc123def456"},
	}
	submittedTask3, err := store.SubmitTask(ctx, task.ID, "agent-1", "result with commit", nil, linksWithCommit, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("third submit failed: %v", err)
	}

	// Verify we now have exactly 3 links (old PR and branch + new commit)
	if len(submittedTask3.Links) != 3 {
		t.Errorf("expected 3 links after adding new commit link, got %d", len(submittedTask3.Links))
	}

	// Verify all three links are present
	linkMap = make(map[string]string)
	for _, link := range submittedTask3.Links {
		linkMap[link.Kind] = link.Value
	}
	if linkMap["pr"] != "https://github.com/test/repo/pull/123" {
		t.Errorf("PR link missing or wrong: got %s", linkMap["pr"])
	}
	if linkMap["branch"] != "feature/test-branch" {
		t.Errorf("branch link missing or wrong: got %s", linkMap["branch"])
	}
	if linkMap["commit"] != "abc123def456" {
		t.Errorf("commit link missing or wrong: got %s", linkMap["commit"])
	}
}

// TestSubmitReviewTaskWithVerdictApprove tests submitting a review task with approve verdict.
func TestSubmitReviewTaskWithVerdictApprove(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project, doc, implement task, claim it, and submit to review
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit the implement task
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Verify review task was created
	if submitted.ReviewRound != 1 {
		t.Errorf("expected review_round=1, got %d", submitted.ReviewRound)
	}

	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Claim and submit the review task with approve verdict (review tasks are already in ready state)
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	reviewResult, err := store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task with verdict: %v", err)
	}

	// Verify review task is in done state with verdict stored
	if reviewResult.State != "done" {
		t.Errorf("expected review task state='done', got '%s'", reviewResult.State)
	}
	if reviewResult.Verdict == nil || *reviewResult.Verdict != "approve" {
		t.Errorf("expected verdict='approve', got %v", reviewResult.Verdict)
	}

	// Verify a review event was appended on the parent task
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var reviewEvent *Event
	for i := range events {
		if events[i].Kind == "review" {
			reviewEvent = &events[i]
			break
		}
	}
	if reviewEvent == nil {
		t.Fatalf("review event not found on parent task")
	}
	if reviewEvent.Verdict == nil || *reviewEvent.Verdict != "approve" {
		t.Errorf("expected review event verdict='approve', got %v", reviewEvent.Verdict)
	}

	// Verify parent task moved to approved state (single reviewer all approved)
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "approved" {
		t.Errorf("expected parent task state='approved' (single reviewer approved), got '%s'", parentTask.State)
	}
}

// TestSubmitReviewTaskWithVerdictReject tests submitting a review task with reject verdict.
func TestSubmitReviewTaskWithVerdictReject(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project, doc, implement task, claim it, and submit to review
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Get the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Claim and submit review task with reject verdict (review tasks are already in ready state)
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	reject := "reject"
	reviewResult, err := store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task with verdict: %v", err)
	}

	// Verify review task is in done state with verdict stored
	if reviewResult.State != "done" {
		t.Errorf("expected review task state='done', got '%s'", reviewResult.State)
	}
	if reviewResult.Verdict == nil || *reviewResult.Verdict != "reject" {
		t.Errorf("expected verdict='reject', got %v", reviewResult.Verdict)
	}

	// Verify parent task moved to ready state (single reviewer rejected)
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "ready" {
		t.Errorf("expected parent task state='ready' (single reviewer rejected), got '%s'", parentTask.State)
	}
}

// TestSubmitImplementTaskRejectsVerdict tests that submitting an implement task with a verdict is rejected.
func TestSubmitImplementTaskRejectsVerdict(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Implement feature",
			Spec:       "Do the thing",
			DocumentID: doc.ID,
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Try to submit an implement task with a verdict - should be rejected
	approve := "approve"
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", &approve, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err == nil {
		t.Fatalf("expected error when submitting implement task with verdict")
	}

	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Code != "FORBIDDEN_VERDICT" {
		t.Errorf("expected FORBIDDEN_VERDICT validation error, got: %v", err)
	}
}

// TestSubmitReviewTaskWithoutVerdictRejected tests that submitting a review task without a verdict is rejected.
func TestSubmitReviewTaskWithoutVerdictRejected(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Get the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Claim the review task (review tasks are already in ready state)
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	// Try to submit a review task without a verdict - should be rejected
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Reviewed", nil, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err == nil {
		t.Fatalf("expected error when submitting review task without verdict")
	}

	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Code != "MISSING_VERDICT" {
		t.Errorf("expected MISSING_VERDICT validation error, got: %v", err)
	}
}

// newClaimedReviewTaskForFindings sets up a project with an implement task that has
// been submitted to review, returning the store, context, the claimed review task's
// id and the parent (implement) task's id, ready for a SubmitTask call carrying
// findings.
func newClaimedReviewTaskForFindings(t *testing.T) (Store, context.Context, string, string) {
	t.Helper()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var reviewTaskID string
	for _, task := range allTasks {
		if task.Kind == "review" && task.TargetTaskID != nil && *task.TargetTaskID == taskID {
			reviewTaskID = task.ID
			break
		}
	}
	if reviewTaskID == "" {
		t.Fatalf("review task not found")
	}

	if _, err = store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	return store, ctx, reviewTaskID, taskID
}

// TestSubmitReviewFindingsValidRoundTrip verifies that a valid findings array
// submitted with a review verdict is stored on the parent's review event and
// round-trips correctly through ListEvents.
func TestSubmitReviewFindingsValidRoundTrip(t *testing.T) {
	store, ctx, reviewTaskID, parentTaskID := newClaimedReviewTaskForFindings(t)

	approve := "approve"
	findings := json.RawMessage(`[
		{"id":"f1","severity":"P2","file":"src/main.go","line":42,"summary":"Issue one","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P1","file":"src/other.go","line":7,"summary":"Issue two","in_changed_text":false,"status":"still_open","prior_id":"old-1"}
	]`)

	if _, err := store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Looks mostly good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget, findings); err != nil {
		t.Fatalf("failed to submit review with findings: %v", err)
	}

	events, err := store.ListEvents(ctx, parentTaskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var reviewEvent *Event
	for i := range events {
		if events[i].Kind == "review" {
			reviewEvent = &events[i]
		}
	}
	if reviewEvent == nil {
		t.Fatalf("review event not found")
	}
	if reviewEvent.Findings == nil {
		t.Fatalf("expected findings to be stored on the review event")
	}

	var stored []Finding
	if err := json.Unmarshal(*reviewEvent.Findings, &stored); err != nil {
		t.Fatalf("failed to unmarshal stored findings: %v", err)
	}
	if len(stored) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(stored))
	}
	if stored[0].ID != "f1" || stored[0].Severity != "P2" || stored[0].Line != 42 || !stored[0].InChangedText || stored[0].Status != "new" || stored[0].PriorID != nil {
		t.Errorf("finding 0 round-tripped incorrectly: %+v", stored[0])
	}
	if stored[1].ID != "f2" || stored[1].PriorID == nil || *stored[1].PriorID != "old-1" {
		t.Errorf("finding 1 round-tripped incorrectly: %+v", stored[1])
	}
}

// TestSubmitReviewWithoutFindingsUnchanged verifies that a review submission with
// no findings argument behaves exactly as before: no findings are stored.
func TestSubmitReviewWithoutFindingsUnchanged(t *testing.T) {
	store, ctx, reviewTaskID, parentTaskID := newClaimedReviewTaskForFindings(t)

	approve := "approve"
	if _, err := store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit review without findings: %v", err)
	}

	events, err := store.ListEvents(ctx, parentTaskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	for _, e := range events {
		if e.Kind == "review" && e.Findings != nil {
			t.Errorf("expected no findings on review event, got %s", string(*e.Findings))
		}
	}
}

// TestSubmitFindingsRejectedOnNonReviewTask verifies that findings on a non-review
// (implement) task submission are always rejected with FINDINGS_NOT_ALLOWED,
// regardless of whether the findings payload itself is well-formed.
func TestSubmitFindingsRejectedOnNonReviewTask(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Implement feature", Spec: "Do the thing", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	if _, err = store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	cases := []struct {
		name     string
		findings json.RawMessage
	}{
		{"well-formed findings", json.RawMessage(`[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`)},
		{"malformed findings", json.RawMessage(`[{"id":""}]`)},
		{"findings not an array", json.RawMessage(`"bogus"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget, tc.findings)
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
			if verr.Code != "FINDINGS_NOT_ALLOWED" {
				t.Errorf("expected FINDINGS_NOT_ALLOWED, got %s (%s)", verr.Code, verr.Message)
			}
		})
	}
}

// TestSubmitReviewFindingsValidationFailures exercises every validation rule from
// the section 3 finding format, asserting each invalid payload is rejected with
// INVALID_FINDINGS and a message naming the expected field.
func TestSubmitReviewFindingsValidationFailures(t *testing.T) {
	cases := []struct {
		name       string
		findings   string
		wantSubstr string
	}{
		{"missing id", `[{"severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].id"},
		{"empty id", `[{"id":"","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].id"},
		{"duplicate id", `[
			{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"},
			{"id":"f1","severity":"P2","file":"y.go","line":2,"summary":"s2","in_changed_text":true,"status":"new"}
		]`, "findings[1].id"},
		{"non-string id", `[{"id":5,"severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].id"},
		{"invalid severity", `[{"id":"f1","severity":"P4","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].severity"},
		{"missing severity", `[{"id":"f1","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].severity"},
		{"missing file", `[{"id":"f1","severity":"P2","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].file"},
		{"empty file", `[{"id":"f1","severity":"P2","file":"","line":1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].file"},
		{"missing line", `[{"id":"f1","severity":"P2","file":"x.go","summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].line"},
		{"zero line", `[{"id":"f1","severity":"P2","file":"x.go","line":0,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].line"},
		{"negative line", `[{"id":"f1","severity":"P2","file":"x.go","line":-1,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].line"},
		{"fractional line", `[{"id":"f1","severity":"P2","file":"x.go","line":1.5,"summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].line"},
		{"string line", `[{"id":"f1","severity":"P2","file":"x.go","line":"1","summary":"s","in_changed_text":true,"status":"new"}]`, "findings[0].line"},
		{"missing summary", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"in_changed_text":true,"status":"new"}]`, "findings[0].summary"},
		{"empty summary", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"","in_changed_text":true,"status":"new"}]`, "findings[0].summary"},
		{"missing in_changed_text", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","status":"new"}]`, "findings[0].in_changed_text"},
		{"null in_changed_text", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":null,"status":"new"}]`, "findings[0].in_changed_text"},
		{"string in_changed_text", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":"yes","status":"new"}]`, "findings[0].in_changed_text"},
		{"invalid status", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"done"}]`, "findings[0].status"},
		{"missing status", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true}]`, "findings[0].status"},
		{"prior_id required for still_open", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"still_open"}]`, "findings[0].prior_id"},
		{"prior_id required for resolved", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"resolved"}]`, "findings[0].prior_id"},
		{"prior_id present when new", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new","prior_id":"old"}]`, "findings[0].prior_id"},
		{"prior_id null when new", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new","prior_id":null}]`, "findings[0].prior_id"},
		{"prior_id empty when still_open", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"still_open","prior_id":""}]`, "findings[0].prior_id"},
		{"prior_id non-string", `[{"id":"f1","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"still_open","prior_id":7}]`, "findings[0].prior_id"},
		{"findings not an array", `{"id":"f1"}`, "findings"},
		{"findings element not an object", `[1]`, "findings[0]"},
		{"ordering: semantic error in findings[0] beats type error in findings[1]", `[
			{"id":"","severity":"P2","file":"x.go","line":1,"summary":"s","in_changed_text":true,"status":"new"},
			{"id":"f2","severity":"P2","file":"x.go","line":1.5,"summary":"s","in_changed_text":true,"status":"new"}
		]`, "findings[0].id"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, ctx, reviewTaskID, _ := newClaimedReviewTaskForFindings(t)

			approve := "approve"
			_, err := store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "review", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget, json.RawMessage(tc.findings))

			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
			if verr.Code != "INVALID_FINDINGS" {
				t.Fatalf("expected INVALID_FINDINGS, got %s (%s)", verr.Code, verr.Message)
			}
			if !strings.Contains(verr.Message, tc.wantSubstr) {
				t.Errorf("expected message to contain %q, got %q", tc.wantSubstr, verr.Message)
			}
		})
	}
}

// newResearchTask creates a project, document, and a research-track implement task
// with two independent reviewers (opus and sonnet). The task is promoted, claimed
// and submitted with a PR link, so its round-1 review tasks are ready to claim.
func newResearchTask(t *testing.T, escalate bool) (Store, context.Context, string, string) {
	t.Helper()
	return newResearchTaskWithReviewers(t, escalate, []string{"opus", "sonnet"})
}

// newResearchTaskWithReviewers is newResearchTask with an explicit review_models list.
func newResearchTaskWithReviewers(t *testing.T, escalate bool, reviewModels []string) (Store, context.Context, string, string) {
	t.Helper()
	// Uses the default empty research escalation ladder (no escalation).
	// Tests that need escalation should create their own store with an explicit ladder.
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Verify claims",
			Spec:         "Verify the claims in the doc",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: reviewModels,
			Track:        "research",
			Escalate:     &escalate,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	return store, ctx, proj.ID, taskID
}

// findResearchReviewTasks locates the ready opus and sonnet review tasks for the
// given parent and review round.
func findResearchReviewTasks(t *testing.T, store Store, ctx context.Context, projID, parentID string, round int) (opusTask, sonnetTask *Task) {
	t.Helper()
	allTasks, err := store.ListTasks(ctx, projID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	for i := range allTasks {
		tk := allTasks[i]
		if tk.Kind != "review" || tk.TargetTaskID == nil || *tk.TargetTaskID != parentID || tk.ReviewRound != round || tk.State != "ready" {
			continue
		}
		switch tk.Model {
		case "opus":
			t := tk
			opusTask = &t
		case "sonnet":
			t := tk
			sonnetTask = &t
		}
	}
	return opusTask, sonnetTask
}

// submitResearchReview claims and submits a research review task with the given
// verdict and findings.
func submitResearchReview(t *testing.T, store Store, ctx context.Context, reviewTask *Task, agent, verdict string, findings json.RawMessage) {
	t.Helper()
	submitResearchReviewWithThresholds(t, store, ctx, reviewTask, agent, verdict, findings, 8, nil, nil)
}

func submitResearchReviewWithThresholds(t *testing.T, store Store, ctx context.Context, reviewTask *Task, agent, verdict string, findings json.RawMessage, maxReviewRounds int, escalationThresholds map[string]int, researchEscalationThresholds map[string]int) {
	t.Helper()
	if _, err := store.ClaimTask(ctx, reviewTask.ID, agent, reviewTask.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task %s: %v", reviewTask.ID, err)
	}
	v := verdict
	if _, err := store.SubmitTask(ctx, reviewTask.ID, agent, "review notes", &v, []LinkInput{}, maxReviewRounds, escalationThresholds, researchEscalationThresholds, testUnlimitedResearchBudget, findings); err != nil {
		t.Fatalf("failed to submit review task %s: %v", reviewTask.ID, err)
	}
}

// submitResearchReviewWithBudget is submitResearchReview with an explicit
// docs/features/research-track.md section 6 research round budget, for tests that
// exercise the chain-wide budget itself.
func submitResearchReviewWithBudget(t *testing.T, store Store, ctx context.Context, reviewTask *Task, agent, verdict string, findings json.RawMessage, researchRoundBudget int) {
	t.Helper()
	if _, err := store.ClaimTask(ctx, reviewTask.ID, agent, reviewTask.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task %s: %v", reviewTask.ID, err)
	}
	v := verdict
	if _, err := store.SubmitTask(ctx, reviewTask.ID, agent, "review notes", &v, []LinkInput{}, 8, nil, nil, researchRoundBudget, findings); err != nil {
		t.Fatalf("failed to submit review task %s: %v", reviewTask.ID, err)
	}
}

// resubmitResearchImplementTask claims and resubmits the parent implement task,
// spawning the next round's review tasks.
func resubmitResearchImplementTask(t *testing.T, store Store, ctx context.Context, parentID string) {
	t.Helper()
	if _, err := store.ClaimTask(ctx, parentID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim parent for resubmit: %v", err)
	}
	if _, err := store.SubmitTask(ctx, parentID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to resubmit parent: %v", err)
	}
}

// TestResearchAggregation_VerdictIndependentOfFindings verifies that research
// aggregation (docs/features/research-track.md section 3) is driven by structured
// findings, not the approve/reject verdict: a reject with no findings alongside an
// approve still passes, and an approve carrying a blocking finding still fails.
func TestResearchAggregation_VerdictIndependentOfFindings(t *testing.T) {
	t.Run("reject with no findings plus approve passes", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		if opus == nil || sonnet == nil {
			t.Fatalf("expected both review tasks to be ready")
		}
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[]`))
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Errorf("expected approved (no blocking findings from either reviewer), got %s", parent.State)
		}
	})

	t.Run("approve with a blocking P1 fails", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		if opus == nil || sonnet == nil {
			t.Fatalf("expected both review tasks to be ready")
		}
		blockingP1 := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}]`)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", blockingP1)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("expected ready (a blocking finding fails the round even though both reviewers approved), got %s", parent.State)
		}
	})
}

// TestResearchAggregation_RoundScopeAndStatus covers the round-scope and status
// rules from section 3: round 1 blocks on any P1/P2 regardless of in_changed_text;
// after round 1, a new P1/P2 in unchanged text and P3 findings don't block, a
// resolved finding never blocks, and a still_open finding always blocks.
func TestResearchAggregation_RoundScopeAndStatus(t *testing.T) {
	t.Run("round 1 P2 in unchanged text still blocks", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		f := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":10,"summary":"unsupported claim","in_changed_text":false,"status":"new"}]`)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", f)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("round 1 P1/P2 must block regardless of in_changed_text, got %s", parent.State)
		}
	})

	t.Run("round 2 resolved, unchanged P2, and P3 all pass", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", blocking)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Fatalf("expected round 1 to fail, got %s", parent.State)
		}
		resubmitResearchImplementTask(t, store, ctx, parentID)

		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		if opus2 == nil || sonnet2 == nil {
			t.Fatalf("expected round 2 review tasks")
		}
		round2Findings := json.RawMessage(`[
			{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"bad claim, now fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"},
			{"id":"f2","severity":"P2","file":"b.md","line":5,"summary":"newly found, but text unchanged","in_changed_text":false,"status":"new"},
			{"id":"f3","severity":"P3","file":"c.md","line":9,"summary":"wrong footnote","in_changed_text":true,"status":"new"}
		]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2Findings)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err = store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Errorf("expected approved (resolved/unchanged-P2/P3 findings don't block), got %s", parent.State)
		}
	})

	t.Run("still_open P3 does not block", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		blocking := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":1,"summary":"missing qualification","in_changed_text":true,"status":"new"}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", blocking)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
		resubmitResearchImplementTask(t, store, ctx, parentID)

		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		if opus2 == nil || sonnet2 == nil {
			t.Fatalf("expected round 2 review tasks")
		}
		stillOpen := json.RawMessage(`[{"id":"f1c","severity":"P3","file":"a.md","line":1,"summary":"still missing qualification","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", stillOpen)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Errorf("expected approved (still_open P3 does not block), got %s", parent.State)
		}

		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 1 {
			t.Errorf("expected 1 follow-up task for the still_open P3, got %d", len(followUps))
		}
	})

	t.Run("regression: P2 still_open in unchanged text round 2 blocks", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		blocking := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":1,"summary":"missing qualification","in_changed_text":true,"status":"new"}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", blocking)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
		resubmitResearchImplementTask(t, store, ctx, parentID)

		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		if opus2 == nil || sonnet2 == nil {
			t.Fatalf("expected round 2 review tasks")
		}
		stillOpen := json.RawMessage(`[{"id":"f1b","severity":"P2","file":"a.md","line":1,"summary":"still missing qualification","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", stillOpen)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("expected ready (P2 still_open blocks even in unchanged text), got %s", parent.State)
		}

		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 0 {
			t.Errorf("expected no follow-up task for blocking P2 still_open, got %d", len(followUps))
		}
	})
}

// TestResearchAggregation_NoFindingsApproves verifies that an empty findings array
// from both reviewers passes the round.
func TestResearchAggregation_NoFindingsApproves(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", json.RawMessage(`[]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Errorf("expected approved with no findings from either reviewer, got %s", parent.State)
	}
}

// TestResearchReviewMissingFindingsRejected verifies that a research review
// submission without a findings array is rejected with MISSING_FINDINGS: section 3
// requires reviewers to submit an explicit array, including [] when there are none.
func TestResearchReviewMissingFindingsRejected(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if _, err := store.ClaimTask(ctx, opus.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	approve := "approve"
	_, err := store.SubmitTask(ctx, opus.ID, "opus-reviewer", "looks fine", &approve, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget, nil)

	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
	if verr.Code != "MISSING_FINDINGS" {
		t.Errorf("expected MISSING_FINDINGS, got %s (%s)", verr.Code, verr.Message)
	}
}

// TestResearchReviewMalformedFindingsRejected verifies that malformed or
// non-array findings on a research review are rejected with INVALID_FINDINGS.
func TestResearchReviewMalformedFindingsRejected(t *testing.T) {
	cases := []struct {
		name     string
		findings json.RawMessage
	}{
		{"object instead of array", json.RawMessage(`{}`)},
		{"string instead of array", json.RawMessage(`"x"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, ctx, projID, parentID := newResearchTask(t, false)
			opus, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
			if _, err := store.ClaimTask(ctx, opus.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
				t.Fatalf("failed to claim: %v", err)
			}
			approve := "approve"
			_, err := store.SubmitTask(ctx, opus.ID, "opus-reviewer", "looks fine", &approve, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget, tc.findings)

			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("expected ValidationError, got %v", err)
			}
			if verr.Code != "INVALID_FINDINGS" {
				t.Errorf("expected INVALID_FINDINGS, got %s (%s)", verr.Code, verr.Message)
			}
		})
	}
}

// TestResearchAggregation_AddReviewNoiseIgnored is a regression test: a human/API
// AddReview call on the parent (which writes a kind='review' event with no
// source_task_id and no findings) must never be mistaken for a review task's own
// submission. Flooding the parent with such events between the two independent
// reviewers must not spuriously clear, hide, or error out on the first reviewer's
// blocking finding.
func TestResearchAggregation_AddReviewNoiseIgnored(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blocking)

	for i := 0; i < 25; i++ {
		if _, err := store.AddReview(ctx, parentID, "human", "approve", nil); err != nil {
			t.Fatalf("AddReview %d failed: %v", i, err)
		}
	}

	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Errorf("expected ready (opus's P1 must still block despite AddReview noise), got %s", parent.State)
	}
}

// TestResearchAggregation_MissingReviewEventFailsClosed is a regression test for
// the fail-closed guard: if aggregation can't account for every review task's
// findings (e.g. a review event that lost its link to its review task), it must
// report a blocking finding rather than risk a silent approval.
func TestResearchAggregation_MissingReviewEventFailsClosed(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", json.RawMessage(`[]`))

	if _, err := store.Conn().ExecContext(ctx, `
		UPDATE event SET source_task_id = NULL WHERE task_id = ? AND kind = 'review' AND source_task_id = ?
	`, parentID, opus.ID); err != nil {
		t.Fatalf("failed to corrupt review event: %v", err)
	}

	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Errorf("expected fail-closed (ready) when a review task's own event can't be matched, got %s", parent.State)
	}
}

// TestResearchAggregation_CircuitBreakerBlocks verifies that repeated failing
// research rounds hit the existing circuit breaker (reused, not reimplemented):
// rounds 1-8 return to ready, round 9 blocks (haiku's default threshold is 8).
func TestResearchAggregation_CircuitBreakerBlocks(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)

	blockingRound := func(round int) {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, round)
		if opus == nil || sonnet == nil {
			t.Fatalf("round %d: expected both review tasks", round)
		}
		blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}]`)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blocking)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	}

	for i := 1; i <= 8; i++ {
		blockingRound(i)
		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Fatalf("round %d: expected ready, got %s", i, parent.State)
		}
		resubmitResearchImplementTask(t, store, ctx, parentID)
	}

	blockingRound(9)
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent (round 9): %v", err)
	}
	if parent.State != "blocked" {
		t.Errorf("round 9: expected blocked (circuit breaker), got %s", parent.State)
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var blockedEvent *Event
	for i := range events {
		if events[i].Kind == "transition" && events[i].Note != nil && strings.Contains(*events[i].Note, "auto-blocked") {
			blockedEvent = &events[i]
			break
		}
	}
	if blockedEvent == nil {
		t.Errorf("expected auto-blocked transition event")
	}
}

// newResearchTaskWithEscalationLadder creates a research task with an escalation ladder configured.
func newResearchTaskWithEscalationLadder(t *testing.T, escalate bool, reviewModels []string) (Store, context.Context, string, string) {
	t.Helper()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithResearchEscalationLadder([]string{"haiku", "sonnet", "opus"}))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Verify claims",
			Spec:         "Verify the claims in the doc",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: reviewModels,
			Track:        "research",
			Escalate:     &escalate,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	return store, ctx, proj.ID, taskID
}

// TestResearchAggregation_CircuitBreakerEscalates verifies that with escalate=true,
// a research task past its threshold escalates to the next model tier exactly like
// build/design, instead of blocking.
func TestResearchAggregation_CircuitBreakerEscalates(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus", "sonnet"})

	blockingRound := func(round int) {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, round)
		if opus == nil || sonnet == nil {
			t.Fatalf("round %d: expected both review tasks", round)
		}
		blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}]`)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blocking)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	}

	for i := 1; i <= 8; i++ {
		blockingRound(i)
		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Fatalf("round %d: expected ready, got %s", i, parent.State)
		}
		resubmitResearchImplementTask(t, store, ctx, parentID)
	}

	blockingRound(9)
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent (round 9): %v", err)
	}
	if parent.State != "superseded" {
		t.Fatalf("round 9: expected superseded (escalated), got %s", parent.State)
	}
	if parent.SupersededBy == nil {
		t.Fatalf("expected SupersededBy to be set")
	}
	escalated, err := store.GetTask(ctx, *parent.SupersededBy)
	if err != nil {
		t.Fatalf("failed to get escalated task: %v", err)
	}
	if escalated.Model != "sonnet" {
		t.Errorf("expected escalated task model 'sonnet', got %s", escalated.Model)
	}
	if escalated.Track != "research" {
		t.Errorf("expected escalated task to keep track 'research', got %s", escalated.Track)
	}
}

// TestResearchAggregation_NoEscalationLadder verifies that research tasks with no escalation
// ladder (the default) block on rejection and never escalate. Unset research thresholds
// use maxReviewRounds, not build defaults.
func TestResearchAggregation_NoEscalationLadder(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Verify claims",
			Spec:         "Verify the claims in the doc",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus", "sonnet"},
			Track:        "research",
			Escalate:     ptrBool(true),
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	parentID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, parentID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, parentID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit with maxReviewRounds=2, so research task will block after 2 rounds
	if _, err = store.SubmitTask(ctx, parentID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 2, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Helper to resubmit with the correct maxReviewRounds
	resubmit := func(round int) {
		if _, err := store.ClaimTask(ctx, parentID, "agent-1", "haiku", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim parent round %d: %v", round, err)
		}
		if _, err := store.SubmitTask(ctx, parentID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 2, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to resubmit round %d: %v", round, err)
		}
	}

	// Run two rejection rounds with blocking findings
	for i := 1; i <= 2; i++ {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, proj.ID, parentID, i)
		if opus == nil || sonnet == nil {
			t.Fatalf("round %d: expected both review tasks", i)
		}
		blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"issue","in_changed_text":true,"status":"new"}]`)
		// Pass thresholds explicitly so the parent's threshold (2) is used, not the review task's default
		submitResearchReviewWithThresholds(t, store, ctx, opus, "opus-reviewer", "reject", blocking, 2, nil, nil)
		submitResearchReviewWithThresholds(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`), 2, nil, nil)

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent (round %d): %v", i, err)
		}
		// Rounds 1-2 should stay ready (not yet exceeding threshold)
		if parent.State != "ready" {
			t.Fatalf("round %d: expected ready, got %s", i, parent.State)
		}
		if parent.Model != "haiku" {
			t.Errorf("round %d: expected model haiku, got %s", i, parent.Model)
		}
		resubmit(i)
	}

	// Third round should block (exceeds threshold of 2, no escalation ladder)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, proj.ID, parentID, 3)
	if opus == nil || sonnet == nil {
		t.Fatalf("round 3: expected both review tasks")
	}
	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"issue","in_changed_text":true,"status":"new"}]`)
	// Pass thresholds explicitly so the parent's threshold (2) is used
	submitResearchReviewWithThresholds(t, store, ctx, opus, "opus-reviewer", "reject", blocking, 2, nil, nil)
	submitResearchReviewWithThresholds(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`), 2, nil, nil)

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 3: %v", err)
	}
	if parent.State != "blocked" {
		t.Fatalf("round 3: expected blocked, got %s", parent.State)
	}
	if parent.Model != "haiku" {
		t.Errorf("round 3: expected model haiku (not escalated), got %s", parent.Model)
	}
}

// TestResearchAggregation_EmptyLadderIgnoresConfiguredThreshold verifies that a
// configured researchEscalationThresholds entry has no effect when no research
// escalation ladder is configured: the task must fall back to maxReviewRounds.
func TestResearchAggregation_EmptyLadderIgnoresConfiguredThreshold(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, true)

	researchThresholds := map[string]int{"haiku": 1}
	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"issue","in_changed_text":true,"status":"new"}]`)
	round := func(n int) {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, n)
		if opus == nil || sonnet == nil {
			t.Fatalf("round %d: expected both review tasks", n)
		}
		submitResearchReviewWithThresholds(t, store, ctx, opus, "opus-reviewer", "reject", blocking, 5, nil, researchThresholds)
		submitResearchReviewWithThresholds(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`), 5, nil, researchThresholds)
	}

	round(1)
	resubmitResearchImplementTask(t, store, ctx, parentID)
	round(2)

	// The configured threshold for haiku is 1, so round 2 would block if it
	// applied. With no research ladder configured, the threshold must be
	// ignored in favor of maxReviewRounds (5), so the task stays ready.
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 2: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("round 2: expected ready (empty ladder ignores configured threshold), got %s", parent.State)
	}
}

// TestResearchAggregation_OffLadderModelIgnoresThreshold verifies that a research
// task whose model is not on the configured research escalation ladder falls
// back to maxReviewRounds, ignoring any configured threshold entry for that model.
func TestResearchAggregation_OffLadderModelIgnoresThreshold(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithResearchEscalationLadder([]string{"sonnet", "opus"}))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	proj, err := store.CreateProject(ctx, "test", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// haiku is not on the configured research ladder (sonnet, opus).
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Research task",
			Spec:         "research",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus", "sonnet"},
			Track:        "research",
			Escalate:     ptrBool(true),
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Impl", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	researchThresholds := map[string]int{"haiku": 1}
	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"issue","in_changed_text":true,"status":"new"}]`)
	round := func(n int) {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, proj.ID, taskID, n)
		if opus == nil || sonnet == nil {
			t.Fatalf("round %d: expected both review tasks", n)
		}
		submitResearchReviewWithThresholds(t, store, ctx, opus, "opus-reviewer", "reject", blocking, 5, nil, researchThresholds)
		submitResearchReviewWithThresholds(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`), 5, nil, researchThresholds)
	}

	round(1)
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim for resubmit: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to resubmit: %v", err)
	}
	round(2)

	// The configured threshold for haiku is 1, but haiku is off the research
	// ladder, so the threshold must be ignored in favor of maxReviewRounds (5).
	// Round 2 <= 5, so the task stays ready.
	final, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if final.State != "ready" {
		t.Errorf("round 2: expected ready (off-ladder model ignores threshold), got %s", final.State)
	}
}

// TestResearchAggregation_IndependentLadders verifies that research and build escalation
// ladders can be configured independently. Research tasks escalate along the research ladder
// using research thresholds, while build tasks escalate along the build ladder using build
// thresholds, independently.
func TestResearchAggregation_IndependentLadders(t *testing.T) {
	// Build ladder: haiku -> sonnet
	// Research ladder: sonnet -> opus (no haiku)
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithEscalationLadder([]string{"haiku", "sonnet"}),
		WithResearchEscalationLadder([]string{"sonnet", "opus"}))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a research task that will escalate from sonnet to opus
	researchTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Research task",
			Spec:         "research something",
			DocumentID:   doc.ID,
			Model:        "sonnet",
			ReviewModels: []string{"opus"},
			Track:        "research",
			Escalate:     ptrBool(true),
		},
	})
	if err != nil {
		t.Fatalf("failed to create research task: %v", err)
	}

	researchTaskID := researchTasks[0].ID

	// Promote and claim research task
	if _, err := store.PromoteTask(ctx, researchTaskID); err != nil {
		t.Fatalf("failed to promote research task: %v", err)
	}
	if _, err := store.ClaimTask(ctx, researchTaskID, "agent-1", "sonnet", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim research task: %v", err)
	}
	// Submit with threshold of 1 so escalation happens quickly
	if _, err := store.SubmitTask(ctx, researchTaskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 1, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit research task: %v", err)
	}

	// Get the review task for round 1
	opus1, _ := findResearchReviewTasks(t, store, ctx, proj.ID, researchTaskID, 1)
	if opus1 == nil {
		t.Fatalf("failed to find research review task round 1")
	}

	// Reject with blocking findings (pass thresholds so parent's threshold is used)
	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"issue","in_changed_text":true,"status":"new"}]`)
	submitResearchReviewWithThresholds(t, store, ctx, opus1, "opus-reviewer", "reject", blocking, 1, nil, map[string]int{"sonnet": 2, "opus": 2})

	// Helper to resubmit with correct params
	resubmitResearch := func() {
		if _, err := store.ClaimTask(ctx, researchTaskID, "agent-1", "sonnet", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim research task for resubmit: %v", err)
		}
		if _, err := store.SubmitTask(ctx, researchTaskID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 1, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to resubmit research task: %v", err)
		}
	}

	// Resubmit to trigger round 2
	resubmitResearch()

	// Get review task for round 2
	opus2, _ := findResearchReviewTasks(t, store, ctx, proj.ID, researchTaskID, 2)
	if opus2 == nil {
		t.Fatalf("failed to find research review task round 2")
	}

	// Reject round 2 (still not exceeding threshold of 2)
	submitResearchReviewWithThresholds(t, store, ctx, opus2, "opus-reviewer", "reject", blocking, 1, nil, map[string]int{"sonnet": 2, "opus": 2})

	// Resubmit to trigger round 3
	resubmitResearch()

	// Get review task for round 3
	opus3, _ := findResearchReviewTasks(t, store, ctx, proj.ID, researchTaskID, 3)
	if opus3 == nil {
		t.Fatalf("failed to find research review task round 3")
	}

	// Reject round 3 (exceeds threshold of 2, should escalate)
	submitResearchReviewWithThresholds(t, store, ctx, opus3, "opus-reviewer", "reject", blocking, 1, nil, map[string]int{"sonnet": 2, "opus": 2})

	// Check research task escalated to opus
	originalTask, err := store.GetTask(ctx, researchTaskID)
	if err != nil {
		t.Fatalf("failed to get research task: %v", err)
	}

	// The original task should be superseded
	if originalTask.State != "superseded" {
		t.Errorf("original research task state: expected superseded, got %s", originalTask.State)
	}

	// Get the escalated task (superseded_by)
	if originalTask.SupersededBy == nil {
		t.Fatalf("original task should have been superseded")
	}
	escalatedTask, err := store.GetTask(ctx, *originalTask.SupersededBy)
	if err != nil {
		t.Fatalf("failed to get escalated research task: %v", err)
	}

	if escalatedTask.State != "ready" {
		t.Errorf("escalated research task state: expected ready, got %s", escalatedTask.State)
	}
	if escalatedTask.Model != "opus" {
		t.Errorf("escalated research task model: expected opus, got %s", escalatedTask.Model)
	}
}

// TestResearchAggregation_TopTierBlocks verifies that a research task at the top
// tier of a configured research ladder blocks instead of escalating when its
// threshold is exceeded.
func TestResearchAggregation_TopTierBlocks(t *testing.T) {
	// Research ladder: sonnet -> opus (opus is top tier)
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithResearchEscalationLadder([]string{"sonnet", "opus"}))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	proj, err := store.CreateProject(ctx, "test", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create research task starting at opus (top tier)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Research task",
			Spec:         "research",
			DocumentID:   doc.ID,
			Model:        "opus",
			ReviewModels: []string{"opus", "sonnet"},
			Track:        "research",
			Escalate:     ptrBool(true),
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	// maxReviewRounds is generously large so the research threshold below (not
	// the fallback) is what decides when the breaker trips.
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Impl", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 10, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"issue","in_changed_text":true,"status":"new"}]`)
	researchThresholds := map[string]int{"sonnet": 1, "opus": 1}

	// Round 1: reject without exceeding the threshold of 1, task stays ready.
	opusRev, sonnetRev := findResearchReviewTasks(t, store, ctx, proj.ID, taskID, 1)
	if opusRev == nil {
		t.Fatalf("no opus review task")
	}
	if sonnetRev == nil {
		t.Fatalf("no sonnet review task")
	}
	submitResearchReviewWithThresholds(t, store, ctx, opusRev, "opus-reviewer", "reject", blocking, 10, nil, researchThresholds)
	submitResearchReviewWithThresholds(t, store, ctx, sonnetRev, "sonnet-reviewer", "reject", blocking, 10, nil, researchThresholds)

	mid, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task after round 1: %v", err)
	}
	if mid.State != "ready" {
		t.Fatalf("research task should still be ready at round 1 (threshold 1), got state %s", mid.State)
	}

	// Resubmit to trigger round 2, which exceeds the threshold of 1.
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim for resubmit: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 10, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to resubmit: %v", err)
	}

	opusRev2, sonnetRev2 := findResearchReviewTasks(t, store, ctx, proj.ID, taskID, 2)
	if opusRev2 == nil {
		t.Fatalf("no opus review task round 2")
	}
	if sonnetRev2 == nil {
		t.Fatalf("no sonnet review task round 2")
	}
	submitResearchReviewWithThresholds(t, store, ctx, opusRev2, "opus-reviewer", "reject", blocking, 10, nil, researchThresholds)
	submitResearchReviewWithThresholds(t, store, ctx, sonnetRev2, "sonnet-reviewer", "reject", blocking, 10, nil, researchThresholds)

	// Check that task blocked (not escalated) at round 2, since round(2) > threshold(1).
	final, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if final.State != "blocked" {
		t.Errorf("top-tier research task should block, got state %s", final.State)
	}
	if final.SupersededBy != nil {
		t.Errorf("top-tier research task should not escalate, but was superseded by %s", *final.SupersededBy)
	}
}

// TestResearchAggregation_EscalateFalseBlocks verifies that a research task with
// escalate=false blocks instead of escalating, even when a research ladder is configured.
func TestResearchAggregation_EscalateFalseBlocks(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithResearchEscalationLadder([]string{"sonnet", "opus"}))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	proj, err := store.CreateProject(ctx, "test", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create research task with escalate=false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Research task",
			Spec:         "research",
			DocumentID:   doc.ID,
			Model:        "sonnet",
			ReviewModels: []string{"opus", "sonnet"},
			Track:        "research",
			Escalate:     ptrBool(false), // Explicitly disabled
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "sonnet", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	// Use maxReviewRounds=0 so any review triggers the circuit breaker
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Impl", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 0, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	// Get and reject reviews to exceed threshold
	opusRev, sonnetRev := findResearchReviewTasks(t, store, ctx, proj.ID, taskID, 1)
	if opusRev == nil {
		t.Fatalf("no opus review task")
	}
	if sonnetRev == nil {
		t.Fatalf("no sonnet review task")
	}

	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"issue","in_changed_text":true,"status":"new"}]`)
	submitResearchReviewWithThresholds(t, store, ctx, opusRev, "opus-reviewer", "reject", blocking, 0, nil, nil)
	submitResearchReviewWithThresholds(t, store, ctx, sonnetRev, "sonnet-reviewer", "reject", blocking, 0, nil, nil)

	// Check that task blocked (not escalated) even though it's below top tier
	final, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if final.State != "blocked" {
		t.Errorf("escalate=false research task should block, got state %s", final.State)
	}
	if final.SupersededBy != nil {
		t.Errorf("escalate=false research task should not escalate, but was superseded by %s", *final.SupersededBy)
	}
}

// TestResearchAggregation_BuildTasksUnchanged verifies that build/design tasks
// continue to escalate via the build ladder and thresholds even when a different
// research ladder and thresholds are configured.
func TestResearchAggregation_BuildTasksUnchanged(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithEscalationLadder([]string{"haiku", "sonnet"}),
		WithResearchEscalationLadder([]string{"sonnet", "opus"}))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()
	proj, err := store.CreateProject(ctx, "test", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a build task (not research)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Build task",
			Spec:         "build something",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"sonnet"},
			Track:        "build",
			Escalate:     ptrBool(true),
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	buildTaskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, buildTaskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, buildTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	// Submit with haiku threshold of 0 so escalation triggers immediately on rejection
	if _, err := store.SubmitTask(ctx, buildTaskID, "agent-1", "Impl", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 5, map[string]int{"haiku": 0}, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	// Get review task and reject it
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var buildRev *Task
	for i := range allTasks {
		tk := allTasks[i]
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == buildTaskID && tk.State == "ready" {
			buildRev = &tk
			break
		}
	}
	if buildRev == nil {
		t.Fatalf("no build review task")
	}

	if _, err := store.ClaimTask(ctx, buildRev.ID, "sonnet-reviewer", "sonnet", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim build review: %v", err)
	}

	reject := "reject"
	if _, err := store.SubmitTask(ctx, buildRev.ID, "sonnet-reviewer", "Needs work", &reject, []LinkInput{}, 5, map[string]int{"haiku": 0}, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit build review: %v", err)
	}

	// Check that build task escalated (not blocked)
	buildFinal, err := store.GetTask(ctx, buildTaskID)
	if err != nil {
		t.Fatalf("failed to get build task: %v", err)
	}
	if buildFinal.State != "superseded" {
		t.Errorf("build task should escalate, got state %s", buildFinal.State)
	}
	if buildFinal.SupersededBy == nil {
		t.Fatalf("build task should have been superseded")
	}
	escalatedBuild, err := store.GetTask(ctx, *buildFinal.SupersededBy)
	if err != nil {
		t.Fatalf("failed to get escalated build task: %v", err)
	}
	if escalatedBuild.Model != "sonnet" {
		t.Errorf("build task should escalate to sonnet, got %s", escalatedBuild.Model)
	}
}

// findResearchFollowUps returns every backlog follow-up task linked (via a
// research_parent task_link) to the given research parent.
func findResearchFollowUps(t *testing.T, store Store, ctx context.Context, projID, parentID string) []Task {
	t.Helper()
	allTasks, err := store.ListTasks(ctx, projID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var followUps []Task
	for _, tk := range allTasks {
		if tk.Kind != "implement" || tk.Track != "research" {
			continue
		}
		for _, v := range taskLinkValues(t, store, ctx, tk.ID, "research_parent") {
			if v == parentID {
				followUps = append(followUps, tk)
				break
			}
		}
	}
	sort.Slice(followUps, func(i, j int) bool { return followUps[i].Title < followUps[j].Title })
	return followUps
}

// taskLinkValues returns the task_link.value for every row of the given kind on taskID.
func taskLinkValues(t *testing.T, store Store, ctx context.Context, taskID, kind string) []string {
	t.Helper()
	rows, err := store.Conn().QueryContext(ctx, `SELECT value FROM task_link WHERE task_id = ? AND kind = ?`, taskID, kind)
	if err != nil {
		t.Fatalf("failed to query task_link: %v", err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("failed to scan task_link value: %v", err)
		}
		values = append(values, v)
	}
	return values
}

// TestIsBlockingResearchFinding covers the blocking rules from
// docs/features/research-track.md section 3 with table-driven tests across severity
// (P1, P2, P3), status (new, still_open, resolved), round (1, 2), and location
// (changed text and unchanged text).
func TestIsBlockingResearchFinding(t *testing.T) {
	cases := []struct {
		name        string
		severity    string
		status      string
		round       int
		inChanged   bool
		shouldBlock bool
	}{
		// P1 findings
		{"P1 new in changed text round 1", "P1", "new", 1, true, true},
		{"P1 new in unchanged text round 1", "P1", "new", 1, false, true},
		{"P1 new in changed text round 2", "P1", "new", 2, true, true},
		{"P1 new in unchanged text round 2", "P1", "new", 2, false, false},
		{"P1 still_open in changed text round 1", "P1", "still_open", 1, true, true},
		{"P1 still_open in unchanged text round 1", "P1", "still_open", 1, false, true},
		{"P1 still_open in changed text round 2", "P1", "still_open", 2, true, true},
		{"P1 still_open in unchanged text round 2", "P1", "still_open", 2, false, true},
		{"P1 resolved in changed text round 2", "P1", "resolved", 2, true, false},
		{"P1 resolved in unchanged text round 2", "P1", "resolved", 2, false, false},

		// P2 findings
		{"P2 new in changed text round 1", "P2", "new", 1, true, true},
		{"P2 new in unchanged text round 1", "P2", "new", 1, false, true},
		{"P2 new in changed text round 2", "P2", "new", 2, true, true},
		{"P2 new in unchanged text round 2", "P2", "new", 2, false, false},
		{"P2 still_open in changed text round 1", "P2", "still_open", 1, true, true},
		{"P2 still_open in unchanged text round 1", "P2", "still_open", 1, false, true},
		{"P2 still_open in changed text round 2", "P2", "still_open", 2, true, true},
		{"P2 still_open in unchanged text round 2", "P2", "still_open", 2, false, true},
		{"P2 resolved in changed text round 2", "P2", "resolved", 2, true, false},
		{"P2 resolved in unchanged text round 2", "P2", "resolved", 2, false, false},

		// P3 findings (never block)
		{"P3 new in changed text round 1", "P3", "new", 1, true, false},
		{"P3 new in unchanged text round 1", "P3", "new", 1, false, false},
		{"P3 new in changed text round 2", "P3", "new", 2, true, false},
		{"P3 new in unchanged text round 2", "P3", "new", 2, false, false},
		{"P3 still_open in changed text round 1", "P3", "still_open", 1, true, false},
		{"P3 still_open in unchanged text round 1", "P3", "still_open", 1, false, false},
		{"P3 still_open in changed text round 2", "P3", "still_open", 2, true, false},
		{"P3 still_open in unchanged text round 2", "P3", "still_open", 2, false, false},
		{"P3 resolved in changed text round 2", "P3", "resolved", 2, true, false},
		{"P3 resolved in unchanged text round 2", "P3", "resolved", 2, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := Finding{
				Severity:      tc.severity,
				Status:        tc.status,
				InChangedText: tc.inChanged,
			}
			got := isBlockingResearchFinding(f, tc.round)
			if got != tc.shouldBlock {
				t.Errorf("expected %v, got %v", tc.shouldBlock, got)
			}
		})
	}
}

// TestResearchFollowUps_NonBlockingCreatesFollowUp covers acceptance criterion 3
// (P3 findings) and criterion 4 (a P2 in unchanged text after round 1) from
// docs/features/research-track.md section 4: a round that passes with non-blocking
// findings creates one follow-up task per finding, in backlog state, on the research
// track, in the parent's project and document, linked to the parent.
func TestResearchFollowUps_NonBlockingCreatesFollowUp(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	findings := json.RawMessage(`[
		{"id":"f1","severity":"P3","file":"a.md","line":10,"summary":"wrong footnote","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", findings)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up task, got %d", len(followUps))
	}
	fu := followUps[0]
	if fu.State != "backlog" {
		t.Errorf("expected follow-up in backlog, got %s", fu.State)
	}
	if fu.Track != "research" {
		t.Errorf("expected follow-up track 'research', got %s", fu.Track)
	}
	if fu.ProjectID != projID {
		t.Errorf("expected follow-up project %s, got %s", projID, fu.ProjectID)
	}
	if fu.DocumentID != parent.DocumentID {
		t.Errorf("expected follow-up document %s, got %s", parent.DocumentID, fu.DocumentID)
	}
	if got := taskLinkValues(t, store, ctx, fu.ID, "research_parent"); len(got) != 1 || got[0] != parentID {
		t.Errorf("expected follow-up linked to parent %s via research_parent task_link, got %v", parentID, got)
	}
	if !strings.Contains(fu.Spec, "wrong footnote") || !strings.Contains(fu.Spec, "a.md") || !strings.Contains(fu.Spec, "P3") {
		t.Errorf("expected follow-up spec to carry finding text/file/severity, got %q", fu.Spec)
	}
}

// TestResearchFollowUps_UnchangedTextP2AfterRoundOne covers acceptance criterion 4:
// after round 1, a P2 finding in unchanged text doesn't fail the round, and it
// creates a follow-up task.
func TestResearchFollowUps_UnchangedTextP2AfterRoundOne(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", blocking)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	round2 := json.RawMessage(`[
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"bad claim, now fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"},
		{"id":"f2","severity":"P2","file":"b.md","line":5,"summary":"overstated but unchanged","in_changed_text":false,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up for the unchanged-text P2 (the resolved P1 must not get one), got %d", len(followUps))
	}
	if !strings.Contains(followUps[0].Spec, "overstated but unchanged") {
		t.Errorf("expected the follow-up to be for the P2 finding, got spec %q", followUps[0].Spec)
	}
}

// TestResearchFollowUps_BlockingFindingsNoFollowUp verifies that a round that fails
// on a blocking finding never creates follow-ups for that finding, even once the task
// is eventually approved in a later round with no other findings raised again.
func TestResearchFollowUps_BlockingFindingsNoFollowUp(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	blockingP1 := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", blockingP1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 1 to fail, got %s", parent.State)
	}
	if got := findResearchFollowUps(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Fatalf("expected no follow-ups while the round is failing, got %d", len(got))
	}

	resubmitResearchImplementTask(t, store, ctx, parentID)
	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	resolved := json.RawMessage(`[{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"fabricated source, fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", resolved)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}
	if got := findResearchFollowUps(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no follow-ups: the only finding raised was blocking and was resolved, got %d", len(got))
	}
}

// TestResearchFollowUps_DedupAcrossReviewers verifies that when both reviewers
// independently raise the same finding (same file, line and summary) in the same
// round, exactly one follow-up task is created.
func TestResearchFollowUps_DedupAcrossReviewers(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	sameFinding := json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":10,"summary":"wrong footnote","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", sameFinding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve",
		json.RawMessage(`[{"id":"g1","severity":"P3","file":"a.md","line":10,"summary":"wrong footnote","in_changed_text":true,"status":"new"}]`))

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up deduplicated across both reviewers, got %d", len(followUps))
	}
	if !strings.Contains(followUps[0].Spec, "Raised by: opus, sonnet\n") {
		t.Errorf("expected the follow-up to credit both raising reviewers, got spec %q", followUps[0].Spec)
	}
}

// TestResearchFollowUps_DedupAcrossRounds covers the "repeated rounds" half of the
// parent acceptance criterion, and the round-scope bug found in review: a non-blocking
// finding raised in a round that failed for an unrelated (blocking) reason, and never
// mentioned again, must still get a follow-up once the parent is finally approved —
// it must not be silently dropped just because the passing round's own review events
// don't repeat it. It also proves the same finding raised again in the later round
// still yields exactly one follow-up (dedup across rounds).
func TestResearchFollowUps_DedupAcrossRounds(t *testing.T) {
	t.Run("non-blocking finding from a failed round is not dropped", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		round1 := json.RawMessage(`[
			{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"},
			{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"typo","in_changed_text":true,"status":"new"}
		]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Fatalf("expected round 1 to fail, got %s", parent.State)
		}
		resubmitResearchImplementTask(t, store, ctx, parentID)

		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		// Round 2 only reports the P1 as resolved; the round-1 P3 is not repeated,
		// as required by the spec (a reviewer isn't asked about it here).
		round2 := json.RawMessage(`[{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"fabricated source, fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"}]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err = store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Fatalf("expected approved, got %s", parent.State)
		}

		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 1 {
			t.Fatalf("expected 1 follow-up for the round-1 P3 (must not be dropped), got %d", len(followUps))
		}
		if !strings.Contains(followUps[0].Spec, "typo") {
			t.Errorf("expected the follow-up to be for the round-1 P3, got spec %q", followUps[0].Spec)
		}
	})

	t.Run("same finding raised in two rounds yields one follow-up", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		round1 := json.RawMessage(`[
			{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"},
			{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"typo","in_changed_text":true,"status":"new"}
		]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
		resubmitResearchImplementTask(t, store, ctx, parentID)

		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		// Round 2 resolves the P1 and repeats the same P3 (still_open P3 does not block).
		round2 := json.RawMessage(`[
			{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"fabricated source, fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"},
			{"id":"f2b","severity":"P3","file":"b.md","line":5,"summary":"typo","in_changed_text":true,"status":"new"}
		]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Fatalf("expected approved, got %s", parent.State)
		}

		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 1 {
			t.Fatalf("expected the same finding raised in rounds 1 and 2 to dedup to 1 follow-up, got %d", len(followUps))
		}
	})
}

// TestResearchFollowUps_StructuredLinks verifies the data-model requirement in
// docs/features/research-track.md ("A link from a follow-up task to the parent task
// and finding it came from"): a research_parent task_link points at the parent,
// and a research_finding_source task_link points at the specific review task and
// finding id it was raised on.
func TestResearchFollowUps_StructuredLinks(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve",
		json.RawMessage(`[{"id":"finding-123","severity":"P3","file":"a.md","line":10,"summary":"wrong footnote","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up, got %d", len(followUps))
	}
	fu := followUps[0]

	if got := taskLinkValues(t, store, ctx, fu.ID, "research_parent"); len(got) != 1 || got[0] != parentID {
		t.Fatalf("expected research_parent task_link to link the follow-up to the parent, got %v", got)
	}

	sourceValues := taskLinkValues(t, store, ctx, fu.ID, "research_finding_source")
	if len(sourceValues) != 1 {
		t.Fatalf("expected 1 research_finding_source link, got %d", len(sourceValues))
	}
	if !strings.Contains(sourceValues[0], "finding-123") || !strings.Contains(sourceValues[0], opus.ID) {
		t.Errorf("expected source link to reference the review task and finding id, got %q", sourceValues[0])
	}
	var source researchFindingSourceValue
	if err := json.Unmarshal([]byte(sourceValues[0]), &source); err != nil {
		t.Fatalf("failed to unmarshal research_finding_source value: %v", err)
	}
	if source.FindingID != "finding-123" || source.ReviewTaskID != opus.ID {
		t.Errorf("expected source link {%s, %s}, got %+v", opus.ID, "finding-123", source)
	}

	dedupValues := taskLinkValues(t, store, ctx, fu.ID, "research_finding_dedup")
	if len(dedupValues) != 1 {
		t.Fatalf("expected 1 research_finding_dedup link, got %d", len(dedupValues))
	}
	var dedup researchFindingDedupKey
	if err := json.Unmarshal([]byte(dedupValues[0]), &dedup); err != nil {
		t.Fatalf("failed to unmarshal research_finding_dedup value: %v", err)
	}
	if dedup.ParentID != parentID || dedup.File != "a.md" || dedup.Line != 10 || dedup.Summary != "wrong footnote" {
		t.Errorf("expected dedup key scoped to parent %s at a.md:10, got %+v", parentID, dedup)
	}
}

// TestResearchFollowUps_ParentScopedDedup verifies that the dedup key is scoped per
// parent task, not global: two different research parents that each independently
// raise a finding with the same file, line and summary must each get their own
// follow-up, not share one.
func TestResearchFollowUps_ParentScopedDedup(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalate := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Verify claims A", Spec: "Verify A", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"opus", "sonnet"}, Track: "research", Escalate: &escalate},
		{Title: "Verify claims B", Spec: "Verify B", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"opus", "sonnet"}, Track: "research", Escalate: &escalate},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}
	parentA, parentB := tasks[0].ID, tasks[1].ID

	for _, id := range []string{parentA, parentB} {
		if _, err := store.PromoteTask(ctx, id); err != nil {
			t.Fatalf("failed to promote %s: %v", id, err)
		}
		if _, err := store.ClaimTask(ctx, id, "agent-1", "haiku", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim %s: %v", id, err)
		}
		if _, err := store.SubmitTask(ctx, id, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to submit %s: %v", id, err)
		}
	}

	sameFinding := json.RawMessage(`[{"id":"f1","severity":"P3","file":"README.md","line":1,"summary":"typo","in_changed_text":true,"status":"new"}]`)
	for _, parentID := range []string{parentA, parentB} {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, proj.ID, parentID, 1)
		if opus == nil || sonnet == nil {
			t.Fatalf("expected review tasks for %s", parentID)
		}
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", sameFinding)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	}

	followUpsA := findResearchFollowUps(t, store, ctx, proj.ID, parentA)
	followUpsB := findResearchFollowUps(t, store, ctx, proj.ID, parentB)
	if len(followUpsA) != 1 {
		t.Fatalf("expected parent A to get its own follow-up, got %d", len(followUpsA))
	}
	if len(followUpsB) != 1 {
		t.Fatalf("expected parent B to get its own follow-up, got %d", len(followUpsB))
	}
	if followUpsA[0].ID == followUpsB[0].ID {
		t.Errorf("expected distinct follow-up tasks per parent, got the same task %s for both", followUpsA[0].ID)
	}
	if got := taskLinkValues(t, store, ctx, followUpsA[0].ID, "research_parent"); len(got) != 1 || got[0] != parentA {
		t.Errorf("expected parent A's follow-up to link to parent A, got %v", got)
	}
	if got := taskLinkValues(t, store, ctx, followUpsB[0].ID, "research_parent"); len(got) != 1 || got[0] != parentB {
		t.Errorf("expected parent B's follow-up to link to parent B, got %v", got)
	}
}

// TestResearchFollowUps_DedupKeyUnambiguous verifies that the persisted dedup key is
// JSON-encoded rather than delimiter-joined, so two distinct findings whose fields
// happen to contain the delimiter don't collapse onto the same key. Naively joining
// with ":" would make file="a", line=1, summary="b:2:c" collide with file="a:1:b",
// line=2, summary="c".
func TestResearchFollowUps_DedupKeyUnambiguous(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	findings := json.RawMessage(`[
		{"id":"f1","severity":"P3","file":"a","line":1,"summary":"b:2:c","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"a:1:b","line":2,"summary":"c","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", findings)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 2 {
		t.Fatalf("expected 2 distinct follow-ups (colon-ambiguous keys must not collapse), got %d", len(followUps))
	}
}

// TestResearchFollowUps_Idempotency verifies that re-running aggregation for the same
// already-approved round doesn't create duplicate follow-up tasks, doesn't append to
// the parent's result again, and doesn't append another follow_up_created event.
func TestResearchFollowUps_Idempotency(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	findings := json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":10,"summary":"wrong footnote","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", findings)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up after round 1, got %d", len(followUps))
	}
	parentBefore, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	eventsBefore, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	followUpEventsBefore := 0
	for _, e := range eventsBefore {
		if e.Kind == "follow_up_created" {
			followUpEventsBefore++
		}
	}
	if followUpEventsBefore != 1 {
		t.Fatalf("expected exactly 1 follow_up_created event, got %d", followUpEventsBefore)
	}

	// Re-run aggregation for the same (already-approved) round directly, as would
	// happen if aggregation were retried or triggered twice for the same submission.
	ss := store.(*sqliteStore)
	tx, err := ss.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}
	if _, err := ss.aggregateReviewRound(ctx, tx, parentID, 5, map[string]int{}, []string{}, map[string]int{}, testUnlimitedResearchBudget); err != nil {
		tx.Rollback()
		t.Fatalf("failed to re-run aggregation: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	followUpsAfter := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUpsAfter) != 1 {
		t.Fatalf("expected repeated aggregation to stay at 1 follow-up, got %d", len(followUpsAfter))
	}
	if followUpsAfter[0].ID != followUps[0].ID {
		t.Errorf("expected the same follow-up task to be reused, got a different id")
	}

	parentAfter, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parentAfter.Result == nil || parentBefore.Result == nil || *parentAfter.Result != *parentBefore.Result {
		t.Errorf("expected parent result to be unchanged by repeated aggregation, before=%v after=%v", parentBefore.Result, parentAfter.Result)
	}

	eventsAfter, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	followUpEventsAfter := 0
	for _, e := range eventsAfter {
		if e.Kind == "follow_up_created" {
			followUpEventsAfter++
		}
	}
	if followUpEventsAfter != 1 {
		t.Errorf("expected follow_up_created event count to stay at 1 after repeated aggregation, got %d", followUpEventsAfter)
	}
}

// TestResearchFollowUps_ParentResultAndEvent verifies that follow-up IDs are recorded
// both in the parent's final result and in a follow_up_created event whose note names
// the actual created task ids (not just a filler string).
func TestResearchFollowUps_ParentResultAndEvent(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	findings := json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":10,"summary":"wrong footnote","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", findings)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up, got %d", len(followUps))
	}
	followUpID := followUps[0].ID

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.Result == nil || !strings.Contains(*parent.Result, followUpID) {
		t.Errorf("expected parent result to name the follow-up id %s, got %v", followUpID, parent.Result)
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var followUpEvent *Event
	for i := range events {
		if events[i].Kind == "follow_up_created" {
			followUpEvent = &events[i]
			break
		}
	}
	if followUpEvent == nil {
		t.Fatalf("expected a follow_up_created event")
	}
	if followUpEvent.Note == nil || !strings.Contains(*followUpEvent.Note, followUpID) {
		t.Errorf("expected follow_up_created event note to name the follow-up id %s, got %v", followUpID, followUpEvent.Note)
	}
}

// TestResearchFollowUps_Model verifies that a follow-up task's model resolves the
// same way a research task's does at creation (docs/features/research-track.md
// section 6): the deployment's research default model if configured, else the
// store's general default. It must not inherit the parent's own model, and must not
// fall back to the task table's raw column default.
func TestResearchFollowUps_Model(t *testing.T) {
	t.Run("uses configured research default model", func(t *testing.T) {
		store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(), WithResearchDefaultModel("opus"))
		if err != nil {
			t.Fatalf("failed to open test database: %v", err)
		}
		t.Cleanup(func() { store.Close() })
		ctx := context.Background()

		proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
		if err != nil {
			t.Fatalf("failed to create project: %v", err)
		}
		doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
		if err != nil {
			t.Fatalf("failed to create document: %v", err)
		}
		escalate := false
		tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{Title: "Verify claims", Spec: "Verify", DocumentID: doc.ID, Model: "sonnet", ReviewModels: []string{"opus", "sonnet"}, Track: "research", Escalate: &escalate},
		})
		if err != nil {
			t.Fatalf("failed to create task: %v", err)
		}
		parentID := tasks[0].ID
		if _, err := store.PromoteTask(ctx, parentID); err != nil {
			t.Fatalf("failed to promote: %v", err)
		}
		if _, err := store.ClaimTask(ctx, parentID, "agent-1", "sonnet", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim: %v", err)
		}
		if _, err := store.SubmitTask(ctx, parentID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to submit: %v", err)
		}

		opus, sonnet := findResearchReviewTasks(t, store, ctx, proj.ID, parentID, 1)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve",
			json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":1,"summary":"typo","in_changed_text":true,"status":"new"}]`))
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		followUps := findResearchFollowUps(t, store, ctx, proj.ID, parentID)
		if len(followUps) != 1 {
			t.Fatalf("expected 1 follow-up, got %d", len(followUps))
		}
		if followUps[0].Model != "opus" {
			t.Errorf("expected follow-up model 'opus' (research default), got %q (parent model was 'sonnet')", followUps[0].Model)
		}
	})

	t.Run("falls back to the store default without a research default configured", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve",
			json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":1,"summary":"typo","in_changed_text":true,"status":"new"}]`))
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 1 {
			t.Fatalf("expected 1 follow-up, got %d", len(followUps))
		}
		if followUps[0].Model != "haiku" {
			t.Errorf("expected follow-up model to fall back to the store default 'haiku', got %q", followUps[0].Model)
		}
	})
}

// findSingleResearchReviewTask locates the single ready review task for the given
// target task and round. Follow-up tasks don't set review_models, so they get exactly
// one reviewer (the "opus" default).
func findSingleResearchReviewTask(t *testing.T, store Store, ctx context.Context, projID, targetID string, round int) *Task {
	t.Helper()
	allTasks, err := store.ListTasks(ctx, projID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	for i := range allTasks {
		tk := allTasks[i]
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == targetID && tk.ReviewRound == round && tk.State == "ready" {
			return &allTasks[i]
		}
	}
	t.Fatalf("no ready review task found for target %s round %d", targetID, round)
	return nil
}

// TestResearchFollowUps_DoNotPolluteParentReviewTally is a regression test for a bug
// found in review: a follow-up used to be linked to its parent via task.target_task_id,
// the same column review and merge tasks use to point at the task they act on. Round
// tallies (aggregateReviewRound, reconcile) count every task with a matching
// target_task_id and review_round, without filtering on kind='review'. Since a
// follow-up is an ordinary implement task that goes through its own review rounds, its
// review_round eventually collides with a later round of the parent (for example after
// a human sends an approved parent back to ready and it's resubmitted), and the
// follow-up's own pending review got counted as one of the parent's reviewers, leaving
// the parent stuck in review. Follow-ups now link to their parent via a research_parent
// task_link instead, so this must no longer happen.
func TestResearchFollowUps_DoNotPolluteParentReviewTally(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "approve",
		json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":1,"summary":"typo","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up, got %d", len(followUps))
	}
	fu := followUps[0]

	// Advance the follow-up through its own review rounds until its review_round
	// reaches 2 - the same round number the parent will be resubmitted into below - and
	// leave its round-2 review pending (not done), so a tally that wrongly picks it up
	// would see a non-approving review still outstanding.
	if _, err := store.PromoteTask(ctx, fu.ID); err != nil {
		t.Fatalf("failed to promote follow-up: %v", err)
	}
	if _, err := store.ClaimTask(ctx, fu.ID, "agent-2", fu.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim follow-up: %v", err)
	}
	if _, err := store.SubmitTask(ctx, fu.ID, "agent-2", "Working on follow-up", nil, []LinkInput{{Kind: "pr", Value: "#200"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit follow-up round 1: %v", err)
	}
	fuReviewOpus1, fuReviewSonnet1 := findResearchReviewTasks(t, store, ctx, projID, fu.ID, 1)
	submitResearchReview(t, store, ctx, fuReviewOpus1, "fu-opus-reviewer", "reject",
		json.RawMessage(`[{"id":"g1","severity":"P1","file":"x.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, fuReviewSonnet1, "fu-sonnet-reviewer", "approve", json.RawMessage(`[]`))

	fuAfterRound1, err := store.GetTask(ctx, fu.ID)
	if err != nil {
		t.Fatalf("failed to get follow-up: %v", err)
	}
	if fuAfterRound1.State != "ready" {
		t.Fatalf("expected follow-up round 1 to fail, got %s", fuAfterRound1.State)
	}
	if _, err := store.ClaimTask(ctx, fu.ID, "agent-2", fu.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim follow-up for round 2: %v", err)
	}
	if _, err := store.SubmitTask(ctx, fu.ID, "agent-2", "Reworked follow-up", nil, []LinkInput{{Kind: "pr", Value: "#200"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to resubmit follow-up into round 2: %v", err)
	}

	note := "reopen for follow-up"
	if _, err := store.TransitionTask(ctx, parentID, "ready", &note); err != nil {
		t.Fatalf("failed to send parent back to ready: %v", err)
	}
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opus2 == nil || sonnet2 == nil {
		t.Fatalf("expected 2 round-2 review tasks for the parent")
	}
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", json.RawMessage(`[]`))
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parentAfter, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parentAfter.State != "approved" {
		t.Fatalf("expected the parent to reach approved once both its own round-2 reviewers approved (got %s); the follow-up's own pending round-2 review must not be tallied into the parent's round", parentAfter.State)
	}
}

// TestResearchFollowUps_ResolvedByPriorIDDespiteRewording is a regression test for a
// bug found in review: resolution was matched by the resolving report's own
// file/line/summary, but section 3 defines prior_id as the link back to the earlier
// finding, and a reviewer isn't required to repeat the original summary verbatim when
// marking it resolved. A P3 resolved with a reworded summary must not get a stale
// follow-up.
func TestResearchFollowUps_ResolvedByPriorIDDespiteRewording(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	round1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	round2 := json.RawMessage(`[
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"bad claim, now fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"},
		{"id":"f2b","severity":"P3","file":"b.md","line":5,"summary":"wrong locator, fixed","in_changed_text":true,"status":"resolved","prior_id":"f2"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	if got := findResearchFollowUps(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no follow-ups: the P3 was explicitly resolved via prior_id, even though its resolution summary was reworded, got %d", len(got))
	}
}

// TestResearchFollowUps_ResolvedThroughStillOpenChain verifies that a finding
// carried forward across several rounds as still_open, reworded each time before
// finally being reported resolved, doesn't produce a follow-up. prior_id only names
// the immediately preceding report, so resolving the chain's last link must
// transitively suppress every earlier identity in it (docs/features/research-track.md
// section 3's prior_id chains through still_open, not just through a single resolved
// report).
func TestResearchFollowUps_ResolvedThroughStillOpenChain(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	round1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	// Both findings are carried forward as still_open, reworded, which blocks round 2.
	round2 := json.RawMessage(`[
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"bad claim, still unresolved","in_changed_text":true,"status":"still_open","prior_id":"f1"},
		{"id":"f2b","severity":"P3","file":"b.md","line":5,"summary":"locator still wrong","in_changed_text":true,"status":"still_open","prior_id":"f2"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", round2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus3, sonnet3 := findResearchReviewTasks(t, store, ctx, projID, parentID, 3)
	// Round 3 finally resolves both, each report's prior_id pointing at round 2's
	// (already reworded) instance, not at the original round-1 finding.
	round3 := json.RawMessage(`[
		{"id":"f1c","severity":"P1","file":"a.md","line":1,"summary":"bad claim, now fixed","in_changed_text":true,"status":"resolved","prior_id":"f1b"},
		{"id":"f2c","severity":"P3","file":"b.md","line":5,"summary":"locator now correct","in_changed_text":true,"status":"resolved","prior_id":"f2b"}
	]`)
	submitResearchReview(t, store, ctx, opus3, "opus-reviewer", "approve", round3)
	submitResearchReview(t, store, ctx, sonnet3, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	if got := findResearchFollowUps(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no follow-ups: the P3 was resolved through a still_open->resolved chain spanning three rounds, got %d", len(got))
	}
}

// TestResearchFollowUps_ResolvedWithReusedFindingID verifies that a resolved report
// which reuses its finding's own earlier id as prior_id (a self-reference, which
// current validation permits since ids are only required to be unique within one
// submission, not across a reviewer's rounds) still resolves against that earlier
// instance rather than against itself. A naive "one id -> one identity" map would
// have the resolved report's own (reworded) identity overwrite the entry it's trying
// to resolve, so the original identity would never be looked up correctly.
func TestResearchFollowUps_ResolvedWithReusedFindingID(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	round1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	// Round 2 resolves both findings, reusing the same finding ids as their own
	// prior_id (a valid, if unusual, input shape) with reworded summaries.
	round2 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim, fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong locator, fixed","in_changed_text":true,"status":"resolved","prior_id":"f2"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	if got := findResearchFollowUps(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no follow-ups: the P3 was resolved via a self-referential prior_id reusing its own finding id, got %d", len(got))
	}
}

// TestResearchFollowUps_StillOpenChainYieldsOneFollowUp verifies that a P3 carried
// forward as still_open with reworded summaries, and never resolved, produces exactly
// one follow-up (not one per wording), that the follow-up describes the latest
// wording, and that re-running aggregation doesn't add a second one.
func TestResearchFollowUps_StillOpenChainYieldsOneFollowUp(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	round1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	round2 := json.RawMessage(`[
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"bad claim, still unresolved","in_changed_text":true,"status":"still_open","prior_id":"f1"},
		{"id":"f2b","severity":"P3","file":"b.md","line":5,"summary":"locator still wrong","in_changed_text":true,"status":"still_open","prior_id":"f2"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", round2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus3, sonnet3 := findResearchReviewTasks(t, store, ctx, projID, parentID, 3)
	// Round 3 resolves only the P1 and doesn't mention the P3 again.
	round3 := json.RawMessage(`[
		{"id":"f1c","severity":"P1","file":"a.md","line":1,"summary":"bad claim, now fixed","in_changed_text":true,"status":"resolved","prior_id":"f1b"}
	]`)
	submitResearchReview(t, store, ctx, opus3, "opus-reviewer", "approve", round3)
	submitResearchReview(t, store, ctx, sonnet3, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected exactly 1 follow-up for the one unresolved P3 chain, got %d", len(followUps))
	}
	if !strings.Contains(followUps[0].Spec, "locator still wrong") {
		t.Errorf("expected follow-up to describe the latest wording, got spec %q", followUps[0].Spec)
	}
	if strings.Contains(followUps[0].Spec, "Finding: wrong locator") {
		t.Errorf("expected follow-up not to use the superseded wording, got spec %q", followUps[0].Spec)
	}

	ss := store.(*sqliteStore)
	tx, err := ss.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}
	if _, err := ss.aggregateReviewRound(ctx, tx, parentID, 5, map[string]int{}, []string{}, map[string]int{}, testUnlimitedResearchBudget); err != nil {
		tx.Rollback()
		t.Fatalf("failed to re-run aggregation: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}
	after := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(after) != 1 || after[0].ID != followUps[0].ID {
		t.Errorf("expected repeated aggregation to keep the single follow-up %s, got %d follow-ups", followUps[0].ID, len(after))
	}
}

// TestResearchFollowUps_ResolvedMarkSurvivesLaterUnion verifies that resolving one
// chain doesn't leak into, or get undone by, another chain that shares a wording.
// Round 2 resolves f2 (reworded to "wrong locator, fixed") and carries f3 forward as
// still_open under that exact same wording. f2 must get no follow-up; f3, never
// resolved, gets exactly one.
func TestResearchFollowUps_ResolvedMarkSurvivesLaterUnion(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	round1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"new"},
		{"id":"f3","severity":"P3","file":"b.md","line":5,"summary":"locator format","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	round2 := json.RawMessage(`[
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"bad claim, fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"},
		{"id":"f2b","severity":"P3","file":"b.md","line":5,"summary":"wrong locator, fixed","in_changed_text":true,"status":"resolved","prior_id":"f2"},
		{"id":"f3b","severity":"P3","file":"b.md","line":5,"summary":"wrong locator, fixed","in_changed_text":true,"status":"still_open","prior_id":"f3"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved (still_open P3 does not block), got %s", parent.State)
	}

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	for _, f := range followUps {
		if strings.Contains(f.Spec, "Finding: wrong locator\n") {
			t.Errorf("expected no follow-up for resolved finding f2, got %s: %q", f.ID, f.Spec)
		}
	}
	if len(followUps) != 1 {
		t.Errorf("expected exactly 1 follow-up for the unresolved f3 chain, got %d", len(followUps))
	}
}

// TestResearchFollowUps_ReRaisedAfterResolved verifies that resolution belongs to a
// prior_id chain, not to a file/line/summary tuple: a P3 resolved in round 2 and then
// raised again as a fresh finding with identical text in the passing round 3 still
// gets its follow-up.
func TestResearchFollowUps_ReRaisedAfterResolved(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	round1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	round2 := json.RawMessage(`[
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"still_open","prior_id":"f1"},
		{"id":"f2b","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"resolved","prior_id":"f2"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", round2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus3, sonnet3 := findResearchReviewTasks(t, store, ctx, projID, parentID, 3)
	round3 := json.RawMessage(`[
		{"id":"f1c","severity":"P1","file":"a.md","line":1,"summary":"bad claim, fixed","in_changed_text":true,"status":"resolved","prior_id":"f1b"},
		{"id":"f3","severity":"P3","file":"b.md","line":5,"summary":"wrong locator","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus3, "opus-reviewer", "approve", round3)
	submitResearchReview(t, store, ctx, sonnet3, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}
	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up for the P3 re-raised after its earlier resolution, got %d", len(followUps))
	}
	if !strings.Contains(followUps[0].Spec, "Finding: wrong locator\n") {
		t.Errorf("expected follow-up for the re-raised P3, got spec %q", followUps[0].Spec)
	}
}

// TestResearchFollowUps_PriorIDResolvesAgainstEarlierRoundOnly verifies that a
// prior_id resolves against a finding from a strictly earlier round, never against a
// report in the same round's submission. Round 2 renumbers its findings so that each
// id collides with the other finding's round-1 id.
func TestResearchFollowUps_PriorIDResolvesAgainstEarlierRoundOnly(t *testing.T) {
	t.Run("swapped ids resolve the earlier findings", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		round1 := json.RawMessage(`[
			{"id":"f1","severity":"P3","file":"b.md","line":5,"summary":"typo","in_changed_text":true,"status":"new"},
			{"id":"f2","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"}
		]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
		resubmitResearchImplementTask(t, store, ctx, parentID)

		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		round2 := json.RawMessage(`[
			{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim fixed","in_changed_text":true,"status":"resolved","prior_id":"f2"},
			{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"typo fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"}
		]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Fatalf("expected approved, got %s", parent.State)
		}
		if got := findResearchFollowUps(t, store, ctx, projID, parentID); len(got) != 0 {
			t.Errorf("expected no follow-ups: both round-1 findings were resolved, got %d", len(got))
		}
	})

	t.Run("fresh finding reusing a same-round prior_id still gets a follow-up", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		round1 := json.RawMessage(`[
			{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad claim","in_changed_text":true,"status":"new"}
		]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", round1)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
		resubmitResearchImplementTask(t, store, ctx, parentID)

		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		// Round 2's fresh P3 reuses id f1, which the resolving report names as its
		// prior_id. The prior_id means round 1's f1, not the fresh P3.
		round2 := json.RawMessage(`[
			{"id":"f1","severity":"P3","file":"b.md","line":5,"summary":"typo","in_changed_text":true,"status":"new"},
			{"id":"f2","severity":"P1","file":"a.md","line":1,"summary":"bad claim fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"}
		]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", round2)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Fatalf("expected approved, got %s", parent.State)
		}
		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 1 {
			t.Fatalf("expected exactly 1 follow-up for the fresh P3, got %d", len(followUps))
		}
		if !strings.Contains(followUps[0].Spec, "Finding: typo\n") {
			t.Errorf("expected follow-up for the fresh P3, got spec %q", followUps[0].Spec)
		}
	})
}

// researchReviewTasksInSlotOrder returns the parent's review task ids for a round in
// creation order, which is how reviewer slots are assigned.
func researchReviewTasksInSlotOrder(t *testing.T, store Store, ctx context.Context, parentID string, round int) []string {
	t.Helper()
	rows, err := store.Conn().QueryContext(ctx, `
		SELECT id FROM task WHERE target_task_id = ? AND kind = 'review' AND review_round = ? ORDER BY rowid
	`, parentID, round)
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("failed to scan review task id: %v", err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestResearchFollowUps_SameModelReviewersKeepOwnLineage verifies that prior_id is
// resolved within one reviewer's own lineage, not across every reviewer sharing a
// model. With review_models ["opus","opus"], reviewer A raises P3 f1 and reviewer B
// raises a blocking P1 also called f1. In round 2 only B reports, resolving its own
// f1. A's P3 must still get exactly one follow-up. Review-task ids are random, so the
// scenario is repeated until both relative id orderings of A and B have been seen.
func TestResearchFollowUps_SameModelReviewersKeepOwnLineage(t *testing.T) {
	seenOrder := map[bool]bool{}
	for attempt := 0; attempt < 64 && len(seenOrder) < 2; attempt++ {
		t.Run(fmt.Sprintf("attempt=%d", attempt), func(t *testing.T) {
			store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "opus"})
			getTask := func(id string) *Task {
				tk, err := store.GetTask(ctx, id)
				if err != nil {
					t.Fatalf("failed to get task %s: %v", id, err)
				}
				return &Task{ID: tk.ID, Model: tk.Model}
			}

			round1 := researchReviewTasksInSlotOrder(t, store, ctx, parentID, 1)
			if len(round1) != 2 {
				t.Fatalf("expected 2 round-1 review tasks, got %d", len(round1))
			}
			seenOrder[round1[0] < round1[1]] = true
			submitResearchReview(t, store, ctx, getTask(round1[0]), "opus-a", "approve",
				json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":1,"summary":"typo","in_changed_text":true,"status":"new"}]`))
			submitResearchReview(t, store, ctx, getTask(round1[1]), "opus-b", "reject",
				json.RawMessage(`[{"id":"f1","severity":"P1","file":"c.md","line":3,"summary":"wrong claim","in_changed_text":true,"status":"new"}]`))
			resubmitResearchImplementTask(t, store, ctx, parentID)

			round2 := researchReviewTasksInSlotOrder(t, store, ctx, parentID, 2)
			if len(round2) != 2 {
				t.Fatalf("expected 2 round-2 review tasks, got %d", len(round2))
			}
			submitResearchReview(t, store, ctx, getTask(round2[0]), "opus-a", "approve", json.RawMessage(`[]`))
			submitResearchReview(t, store, ctx, getTask(round2[1]), "opus-b", "approve",
				json.RawMessage(`[{"id":"f1b","severity":"P1","file":"c.md","line":3,"summary":"wrong claim, fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"}]`))

			parent, err := store.GetTask(ctx, parentID)
			if err != nil {
				t.Fatalf("failed to get parent: %v", err)
			}
			if parent.State != "approved" {
				t.Fatalf("expected approved, got %s", parent.State)
			}
			followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
			if len(followUps) != 1 {
				t.Fatalf("expected exactly 1 follow-up for reviewer A's unresolved P3, got %d", len(followUps))
			}
			if !strings.Contains(followUps[0].Spec, "Finding: typo\n") {
				t.Errorf("expected the follow-up to be for A's P3, got spec %q", followUps[0].Spec)
			}
		})
	}
	if len(seenOrder) < 2 {
		t.Fatalf("expected to exercise both review-task id orderings, saw %v", seenOrder)
	}
}

// TestResearchFollowUps_InheritsParentReviewModels verifies that follow-up tasks
// created from a research parent inherit the parent's review_models.
func TestResearchFollowUps_InheritsParentReviewModels(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "sonnet"})
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	findings := json.RawMessage(`[
		{"id":"f1","severity":"P3","file":"a.md","line":10,"summary":"minor issue","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", findings)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 follow-up task, got %d", len(followUps))
	}

	fu := followUps[0]
	if len(fu.ReviewModels) != 2 || fu.ReviewModels[0] != "opus" || fu.ReviewModels[1] != "sonnet" {
		t.Errorf("expected follow-up to inherit parent's review_models ['opus','sonnet'], got %v", fu.ReviewModels)
	}
}

// TestBuildDesignAggregationUnchanged is a regression test: research aggregation
// must not change build/design behavior. It asserts the review event's Note is
// stored verbatim as the reviewer's result text (no envelope), and that a single
// approving reviewer still moves the parent straight to approved.
func TestBuildDesignAggregationUnchanged(t *testing.T) {
	for _, track := range []string{"build", "design", ""} {
		t.Run("track="+track, func(t *testing.T) {
			store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
			if err != nil {
				t.Fatalf("failed to open test database: %v", err)
			}
			defer store.Close()
			ctx := context.Background()

			proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
			if err != nil {
				t.Fatalf("failed to create project: %v", err)
			}
			doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
			if err != nil {
				t.Fatalf("failed to create document: %v", err)
			}
			tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
				{
					Title:        "Implement feature",
					Spec:         "Do the thing",
					DocumentID:   doc.ID,
					Model:        "haiku",
					ReviewModels: []string{"opus"},
					Track:        track,
				},
			})
			if err != nil {
				t.Fatalf("failed to create task: %v", err)
			}
			taskID := tasks[0].ID

			if _, err = store.PromoteTask(ctx, taskID); err != nil {
				t.Fatalf("failed to promote task: %v", err)
			}
			if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
				t.Fatalf("failed to claim task: %v", err)
			}
			if _, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
				t.Fatalf("failed to submit implement task: %v", err)
			}

			allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
			if err != nil {
				t.Fatalf("failed to list tasks: %v", err)
			}
			var reviewTaskID string
			for _, task := range allTasks {
				if task.Kind == "review" && task.TargetTaskID != nil && *task.TargetTaskID == taskID {
					reviewTaskID = task.ID
					break
				}
			}
			if reviewTaskID == "" {
				t.Fatalf("review task not found")
			}
			if _, err = store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
				t.Fatalf("failed to claim review task: %v", err)
			}

			approve := "approve"
			resultText := "Detailed feedback\nline two, with more detail"
			if _, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", resultText, &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
				t.Fatalf("failed to submit review task: %v", err)
			}

			parent, err := store.GetTask(ctx, taskID)
			if err != nil {
				t.Fatalf("failed to get parent: %v", err)
			}
			if parent.State != "approved" {
				t.Errorf("expected approved (single reviewer approved), got %s", parent.State)
			}

			events, err := store.ListEvents(ctx, taskID)
			if err != nil {
				t.Fatalf("failed to list events: %v", err)
			}
			var reviewEvent *Event
			for i := range events {
				if events[i].Kind == "review" {
					reviewEvent = &events[i]
				}
			}
			if reviewEvent == nil {
				t.Fatalf("review event not found")
			}
			if reviewEvent.Note == nil || *reviewEvent.Note != resultText {
				t.Errorf("expected review event note to be stored verbatim as %q, got %v", resultText, reviewEvent.Note)
			}
		})
	}
}

// TestReviewRoundCircuitBreaker tests the circuit breaker for repeated rejections.
// Tasks rejected up to the threshold return to ready; beyond the threshold, they go to blocked.
func TestReviewRoundCircuitBreaker(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateFalse := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Escalate:     &escalateFalse,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Helper to submit, get review task, and submit review with verdict
	submitAndReject := func(roundNum int, maxReviewRounds int) {
		// Promote task if it's in backlog
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", roundNum, err)
		}
		if task.State == "backlog" {
			_, err := store.PromoteTask(ctx, taskID)
			if err != nil {
				t.Fatalf("failed to promote task (round %d): %v", roundNum, err)
			}
		}

		// Claim the task
		_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim task (round %d): %v", roundNum, err)
		}

		// Submit implementation
		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit implement task (round %d): %v", roundNum, err)
		}

		// Get the review task
		allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks (round %d): %v", roundNum, err)
		}

		var reviewTask *Task
		for i := range allTasks {
			if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID && allTasks[i].State == "ready" {
				reviewTask = &allTasks[i]
				break
			}
		}
		if reviewTask == nil {
			t.Fatalf("review task not found (round %d)", roundNum)
		}

		// Claim and reject
		_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim review task (round %d): %v", roundNum, err)
		}

		reject := "reject"
		_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit review task (round %d): %v", roundNum, err)
		}
	}

	// Test with default thresholds. Since task model is haiku, threshold is 8.
	// The circuit breaker should trigger when review_round > 8, i.e., at round 9.
	maxReviewRounds := 5

	// Rounds 1-8: should transition to ready
	for i := 1; i <= 8; i++ {
		submitAndReject(i, maxReviewRounds)

		// Check parent state
		parent, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Errorf("round %d: expected parent state 'ready', got '%s'", i, parent.State)
		}
		if parent.ReviewRound != i {
			t.Errorf("round %d: expected review_round %d, got %d", i, i, parent.ReviewRound)
		}
	}

	// Round 9: should transition to blocked (circuit breaker, since threshold=8)
	submitAndReject(9, maxReviewRounds)

	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task (round 9): %v", err)
	}
	if parent.State != "blocked" {
		t.Errorf("round 9: expected parent state 'blocked', got '%s'", parent.State)
	}
	if parent.ReviewRound != 9 {
		t.Errorf("round 9: expected review_round 9, got %d", parent.ReviewRound)
	}

	// Check that a transition event was appended with the correct note
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var blockedEvent *Event
	for i := range events {
		if events[i].Kind == "transition" && events[i].Note != nil && strings.Contains(*events[i].Note, "auto-blocked") {
			blockedEvent = &events[i]
			break
		}
	}
	if blockedEvent == nil {
		t.Errorf("expected auto-blocked transition event")
	} else if !strings.Contains(*blockedEvent.Note, "9 consecutive review rounds") {
		t.Errorf("expected event note about 9 rounds, got: %s", *blockedEvent.Note)
	}
}

// TestEscalateHaikuToSonnet verifies that a haiku task with escalate=true
// is superseded to sonnet when review_round exceeds the threshold.
func TestEscalateHaikuToSonnet(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateTrue := true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Escalate:     &escalateTrue,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	maxReviewRounds := 5

	submitAndReject := func(roundNum int) {
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", roundNum, err)
		}
		if task.State == "backlog" {
			_, err := store.PromoteTask(ctx, taskID)
			if err != nil {
				t.Fatalf("failed to promote task (round %d): %v", roundNum, err)
			}
		}

		_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim task (round %d): %v", roundNum, err)
		}

		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit implement task (round %d): %v", roundNum, err)
		}

		allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks (round %d): %v", roundNum, err)
		}

		var reviewTask *Task
		for i := range allTasks {
			if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID && allTasks[i].State == "ready" {
				reviewTask = &allTasks[i]
				break
			}
		}
		if reviewTask == nil {
			t.Fatalf("review task not found (round %d)", roundNum)
		}

		_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim review task (round %d): %v", roundNum, err)
		}

		reject := "reject"
		_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit review task (round %d): %v", roundNum, err)
		}
	}

	// Rounds 1-8: should transition to ready
	for i := 1; i <= 8; i++ {
		submitAndReject(i)

		parent, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Errorf("round %d: expected parent state 'ready', got '%s'", i, parent.State)
		}
	}

	// Round 9: should escalate to sonnet
	submitAndReject(9)

	// Original task should be superseded
	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get original task: %v", err)
	}
	if parent.State != "superseded" {
		t.Errorf("expected original task state 'superseded', got '%s'", parent.State)
	}
	if parent.SupersededBy == nil {
		t.Errorf("expected original task SupersededBy to be set")
	} else {
		// Verify the new task exists and has correct properties
		newTask, err := store.GetTask(ctx, *parent.SupersededBy)
		if err != nil {
			t.Fatalf("failed to get escalated task: %v", err)
		}
		if newTask.Model != "sonnet" {
			t.Errorf("expected escalated task model 'sonnet', got '%s'", newTask.Model)
		}
		if newTask.State != "ready" {
			t.Errorf("expected escalated task state 'ready', got '%s'", newTask.State)
		}
		if newTask.ReviewRound != 0 {
			t.Errorf("expected escalated task review_round 0, got %d", newTask.ReviewRound)
		}
	}

	// Check for escalation event on original task
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var escalationEvent *Event
	for i := range events {
		if events[i].Kind == "escalation" {
			escalationEvent = &events[i]
			break
		}
	}
	if escalationEvent == nil {
		t.Errorf("expected escalation event on original task")
	}
}

// TestEscalateSonnetToOpus verifies that a sonnet task with escalate=true
// is superseded to opus when review_round exceeds the threshold (6 for sonnet).
func TestEscalateSonnetToOpus(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateTrue := true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "sonnet",
			ReviewModels: []string{"opus"},
			Escalate:     &escalateTrue,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	maxReviewRounds := 5

	submitAndReject := func(roundNum int) {
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", roundNum, err)
		}
		if task.State == "backlog" {
			_, err := store.PromoteTask(ctx, taskID)
			if err != nil {
				t.Fatalf("failed to promote task (round %d): %v", roundNum, err)
			}
		}

		_, err = store.ClaimTask(ctx, taskID, "agent-1", "sonnet", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim task (round %d): %v", roundNum, err)
		}

		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit implement task (round %d): %v", roundNum, err)
		}

		allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks (round %d): %v", roundNum, err)
		}

		var reviewTask *Task
		for i := range allTasks {
			if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID && allTasks[i].State == "ready" {
				reviewTask = &allTasks[i]
				break
			}
		}
		if reviewTask == nil {
			t.Fatalf("review task not found (round %d)", roundNum)
		}

		_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim review task (round %d): %v", roundNum, err)
		}

		reject := "reject"
		_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit review task (round %d): %v", roundNum, err)
		}
	}

	// Rounds 1-6: should transition to ready (sonnet threshold is 6)
	for i := 1; i <= 6; i++ {
		submitAndReject(i)

		parent, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Errorf("round %d: expected parent state 'ready', got '%s'", i, parent.State)
		}
	}

	// Round 7: should escalate to opus
	submitAndReject(7)

	// Original task should be superseded
	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get original task: %v", err)
	}
	if parent.State != "superseded" {
		t.Errorf("expected original task state 'superseded', got '%s'", parent.State)
	}
	if parent.SupersededBy == nil {
		t.Errorf("expected original task SupersededBy to be set")
	} else {
		// Verify the new task exists and has correct properties
		newTask, err := store.GetTask(ctx, *parent.SupersededBy)
		if err != nil {
			t.Fatalf("failed to get escalated task: %v", err)
		}
		if newTask.Model != "opus" {
			t.Errorf("expected escalated task model 'opus', got '%s'", newTask.Model)
		}
		if newTask.State != "ready" {
			t.Errorf("expected escalated task state 'ready', got '%s'", newTask.State)
		}
		if newTask.ReviewRound != 0 {
			t.Errorf("expected escalated task review_round 0, got %d", newTask.ReviewRound)
		}
	}

	// Check for escalation event on original task
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var escalationEvent *Event
	for i := range events {
		if events[i].Kind == "escalation" {
			escalationEvent = &events[i]
			break
		}
	}
	if escalationEvent == nil {
		t.Errorf("expected escalation event on original task")
	}
}

// TestEscalateOpusBlock verifies that an opus (top-tier) task with escalate=true
// is blocked, not escalated, when review_round exceeds the threshold (4 for opus).
func TestEscalateOpusBlock(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateTrue := true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "opus",
			ReviewModels: []string{"sonnet"},
			Escalate:     &escalateTrue,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	maxReviewRounds := 5

	submitAndReject := func(roundNum int) {
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", roundNum, err)
		}
		if task.State == "backlog" {
			_, err := store.PromoteTask(ctx, taskID)
			if err != nil {
				t.Fatalf("failed to promote task (round %d): %v", roundNum, err)
			}
		}

		_, err = store.ClaimTask(ctx, taskID, "agent-1", "opus", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim task (round %d): %v", roundNum, err)
		}

		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit implement task (round %d): %v", roundNum, err)
		}

		allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks (round %d): %v", roundNum, err)
		}

		var reviewTask *Task
		for i := range allTasks {
			if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID && allTasks[i].State == "ready" {
				reviewTask = &allTasks[i]
				break
			}
		}
		if reviewTask == nil {
			t.Fatalf("review task not found (round %d)", roundNum)
		}

		_, err = store.ClaimTask(ctx, reviewTask.ID, "sonnet-reviewer", "sonnet", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim review task (round %d): %v", roundNum, err)
		}

		reject := "reject"
		_, err = store.SubmitTask(ctx, reviewTask.ID, "sonnet-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit review task (round %d): %v", roundNum, err)
		}
	}

	// Rounds 1-4: should transition to ready (opus threshold is 4)
	for i := 1; i <= 4; i++ {
		submitAndReject(i)

		parent, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Errorf("round %d: expected parent state 'ready', got '%s'", i, parent.State)
		}
	}

	// Round 5: should transition to blocked (not escalated, since opus is top-tier)
	submitAndReject(5)

	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task (round 5): %v", err)
	}
	if parent.State != "blocked" {
		t.Errorf("expected parent state 'blocked', got '%s'", parent.State)
	}
	if parent.SupersededBy != nil {
		t.Errorf("expected SupersededBy to be nil for top-tier task, got %s", *parent.SupersededBy)
	}

	// Check that a transition event was appended
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var blockedEvent *Event
	for i := range events {
		if events[i].Kind == "transition" && events[i].Note != nil && strings.Contains(*events[i].Note, "auto-blocked") {
			blockedEvent = &events[i]
			break
		}
	}
	if blockedEvent == nil {
		t.Errorf("expected auto-blocked transition event")
	}
}

// TestThresholdFor tests the thresholdFor function for per-model escalation thresholds.
func TestThresholdFor(t *testing.T) {
	tests := []struct {
		name                 string
		model                string
		escalationThresholds map[string]int
		maxReviewRounds      int
		expectedThreshold    int
	}{
		{
			name:                 "haiku default threshold",
			model:                "haiku",
			escalationThresholds: nil,
			maxReviewRounds:      5,
			expectedThreshold:    8,
		},
		{
			name:                 "sonnet default threshold",
			model:                "sonnet",
			escalationThresholds: nil,
			maxReviewRounds:      5,
			expectedThreshold:    6,
		},
		{
			name:                 "opus default threshold",
			model:                "opus",
			escalationThresholds: nil,
			maxReviewRounds:      5,
			expectedThreshold:    4,
		},
		{
			name:                 "unknown model falls back to maxReviewRounds",
			model:                "claude",
			escalationThresholds: nil,
			maxReviewRounds:      5,
			expectedThreshold:    5,
		},
		{
			name:                 "override haiku default with custom threshold",
			model:                "haiku",
			escalationThresholds: map[string]int{"haiku": 3},
			maxReviewRounds:      5,
			expectedThreshold:    3,
		},
		{
			name:                 "custom thresholds for all models",
			model:                "sonnet",
			escalationThresholds: map[string]int{"haiku": 10, "sonnet": 7, "opus": 5},
			maxReviewRounds:      5,
			expectedThreshold:    7,
		},
		{
			name:                 "unknown model with custom thresholds falls back to maxReviewRounds",
			model:                "claude",
			escalationThresholds: map[string]int{"haiku": 10, "sonnet": 7},
			maxReviewRounds:      5,
			expectedThreshold:    5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := thresholdFor(tt.model, tt.escalationThresholds, tt.maxReviewRounds)
			if result != tt.expectedThreshold {
				t.Errorf("expected threshold %d, got %d", tt.expectedThreshold, result)
			}
		})
	}
}

// TestWaitForAllAggregation_FirstApproveSecondApprove tests MR-9: with two reviewers,
// the first approve leaves the parent in review, the second approve moves it to approved.
func TestWaitForAllAggregation_FirstApproveSecondApprove(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task with two reviewers
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus", "sonnet"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit the implement task
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find the two review tasks
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTasks []*Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTasks = append(reviewTasks, &allTasks[i])
		}
	}
	if len(reviewTasks) != 2 {
		t.Fatalf("expected 2 review tasks, got %d", len(reviewTasks))
	}

	// Find which review task is opus and which is sonnet
	var opusTask, sonnetTask *Task
	for i := range reviewTasks {
		if reviewTasks[i].Model == "opus" {
			opusTask = reviewTasks[i]
		} else if reviewTasks[i].Model == "sonnet" {
			sonnetTask = reviewTasks[i]
		}
	}
	if opusTask == nil || sonnetTask == nil {
		t.Fatalf("could not find both opus and sonnet review tasks")
	}

	// First reviewer (opus) approves
	_, err = store.ClaimTask(ctx, opusTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim first review task: %v", err)
	}
	approve := "approve"
	_, err = store.SubmitTask(ctx, opusTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit first review task: %v", err)
	}

	// Verify parent task is still in review state (first approval doesn't move it)
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "review" {
		t.Errorf("expected parent task state='review' after first approve, got '%s'", parentTask.State)
	}

	// Second reviewer (sonnet) approves
	_, err = store.ClaimTask(ctx, sonnetTask.ID, "sonnet-reviewer", "sonnet", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim second review task: %v", err)
	}
	_, err = store.SubmitTask(ctx, sonnetTask.ID, "sonnet-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit second review task: %v", err)
	}

	// Verify parent task moved to approved state (all approved)
	parentTask, err = store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "approved" {
		t.Errorf("expected parent task state='approved' after all approve, got '%s'", parentTask.State)
	}
}

// TestWaitForAllAggregation_ApproveAndReject tests MR-9: with one approve and one reject,
// the parent moves to ready for rework.
func TestWaitForAllAggregation_ApproveAndReject(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task with two reviewers
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus", "sonnet"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit the implement task
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find the two review tasks
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTasks []*Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTasks = append(reviewTasks, &allTasks[i])
		}
	}
	if len(reviewTasks) != 2 {
		t.Fatalf("expected 2 review tasks, got %d", len(reviewTasks))
	}

	// Find which review task is opus and which is sonnet
	var opusTask, sonnetTask *Task
	for i := range reviewTasks {
		if reviewTasks[i].Model == "opus" {
			opusTask = reviewTasks[i]
		} else if reviewTasks[i].Model == "sonnet" {
			sonnetTask = reviewTasks[i]
		}
	}
	if opusTask == nil || sonnetTask == nil {
		t.Fatalf("could not find both opus and sonnet review tasks")
	}

	// First reviewer (opus) approves
	_, err = store.ClaimTask(ctx, opusTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim first review task: %v", err)
	}
	approve := "approve"
	_, err = store.SubmitTask(ctx, opusTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit first review task: %v", err)
	}

	// Verify parent task is still in review state
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "review" {
		t.Errorf("expected parent task state='review' after one approve, got '%s'", parentTask.State)
	}

	// Second reviewer (sonnet) rejects
	_, err = store.ClaimTask(ctx, sonnetTask.ID, "sonnet-reviewer", "sonnet", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim second review task: %v", err)
	}
	reject := "reject"
	_, err = store.SubmitTask(ctx, sonnetTask.ID, "sonnet-reviewer", "Needs changes", &reject, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit second review task: %v", err)
	}

	// Verify parent task moved to ready state (at least one reject)
	parentTask, err = store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "ready" {
		t.Errorf("expected parent task state='ready' after reject, got '%s'", parentTask.State)
	}
}

// TestTransitionBlockedToReady tests that blocked tasks can transition to ready,
// become claimable with no stale assignee/lease, and that other transitions still work as expected.
func TestTransitionBlockedToReady(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a document
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks in backlog state
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 1 - Blocked", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 2 - Approved", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskID1 := tasks[0].ID
	taskID2 := tasks[1].ID

	// Transition task 1 to blocked state (from backlog)
	blockedNote := "blocker: dependency failed"
	task1, err := store.TransitionTask(ctx, taskID1, "blocked", &blockedNote)
	if err != nil {
		t.Fatalf("failed to transition to blocked: %v", err)
	}
	if task1.State != "blocked" {
		t.Errorf("expected task state='blocked', got '%s'", task1.State)
	}

	// Manually update task 1 to have stale assignee and expired lease
	pastTime := time.Now().UTC().Add(-1 * time.Hour).Format(timestampLayout)
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET assignee = ?, lease_expires_at = ? WHERE id = ?
	`, "stale-agent", pastTime, taskID1)
	if err != nil {
		t.Fatalf("failed to set stale assignee and lease: %v", err)
	}

	// Verify task 1 has stale assignee/lease
	stalledTask, err := store.GetTask(ctx, taskID1)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if stalledTask.Assignee == nil || *stalledTask.Assignee != "stale-agent" {
		t.Errorf("expected task assignee='stale-agent', got %v", stalledTask.Assignee)
	}
	if stalledTask.LeaseExpiresAt == nil {
		t.Errorf("expected task to have lease_expires_at, got nil")
	}

	// Test 1: Transition blocked→ready succeeds and clears assignee/lease
	unblockNote := "blocker cleared"
	unblocked, err := store.TransitionTask(ctx, taskID1, "ready", &unblockNote)
	if err != nil {
		t.Fatalf("failed to transition blocked→ready: %v", err)
	}
	if unblocked.State != "ready" {
		t.Errorf("expected task state='ready' after transition, got '%s'", unblocked.State)
	}
	if unblocked.Assignee != nil {
		t.Errorf("expected task assignee=nil after blocked→ready, got %v", unblocked.Assignee)
	}
	if unblocked.LeaseExpiresAt != nil {
		t.Errorf("expected task lease_expires_at=nil after blocked→ready, got %v", unblocked.LeaseExpiresAt)
	}

	// Test 2: Transition event is recorded with the note
	events, err := store.ListEvents(ctx, taskID1)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events (transition to blocked + transition to ready), got %d", len(events))
	}
	lastEvent := events[len(events)-1]
	if lastEvent.Kind != "transition" {
		t.Errorf("expected last event kind='transition', got '%s'", lastEvent.Kind)
	}
	if lastEvent.Note == nil || *lastEvent.Note != unblockNote {
		t.Errorf("expected event note='%s', got %v", unblockNote, lastEvent.Note)
	}

	// Test 3: approved→ready still works (unchanged behavior)
	// Manually update task 2 to approved state
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET state = 'approved' WHERE id = ?
	`, taskID2)
	if err != nil {
		t.Fatalf("failed to set task to approved: %v", err)
	}

	approvedNote := "ready to claim"
	readyFromApproved, err := store.TransitionTask(ctx, taskID2, "ready", &approvedNote)
	if err != nil {
		t.Fatalf("failed to transition approved→ready: %v", err)
	}
	if readyFromApproved.State != "ready" {
		t.Errorf("expected task state='ready' after approved→ready, got '%s'", readyFromApproved.State)
	}

	// Test 4: Illegal transitions still return 409
	// Create another task and transition it to done
	tasks2, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 3 - Done", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task 3: %v", err)
	}
	taskID3 := tasks2[0].ID

	// Manually set it to approved, then to done
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET state = 'approved' WHERE id = ?
	`, taskID3)
	if err != nil {
		t.Fatalf("failed to set task to approved: %v", err)
	}
	_, err = store.TransitionTask(ctx, taskID3, "done", nil)
	if err != nil {
		t.Fatalf("failed to transition to done: %v", err)
	}

	// Try to transition done→ready (should fail)
	_, err = store.TransitionTask(ctx, taskID3, "ready", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected done→ready to return ErrConflict, got %v", err)
	}

	// Try to transition ready→done (should fail - task 1 is in ready state)
	_, err = store.TransitionTask(ctx, taskID1, "done", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ready→done to return ErrConflict, got %v", err)
	}
}

func TestTransitionBlockedToFailed(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a document
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks in backlog state
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 1 - Blocked", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 2 - Ready", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 3 - InProgress", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 4 - Done", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskID1 := tasks[0].ID
	taskID2 := tasks[1].ID
	taskID3 := tasks[2].ID
	taskID4 := tasks[3].ID

	// Test 1: Transition a task to blocked state
	blockedNote := "blocker: unresolvable"
	task1, err := store.TransitionTask(ctx, taskID1, "blocked", &blockedNote)
	if err != nil {
		t.Fatalf("failed to transition to blocked: %v", err)
	}
	if task1.State != "blocked" {
		t.Errorf("expected task state='blocked', got '%s'", task1.State)
	}

	// Test 2: blocked→failed succeeds (main test case)
	failedNote := "dead-end blocker"
	failed, err := store.TransitionTask(ctx, taskID1, "failed", &failedNote)
	if err != nil {
		t.Fatalf("failed to transition blocked→failed: %v", err)
	}
	if failed.State != "failed" {
		t.Errorf("expected task state='failed' after transition, got '%s'", failed.State)
	}

	// Test 3: Transition event is recorded with the note
	events, err := store.ListEvents(ctx, taskID1)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) < 2 {
		t.Fatalf("expected at least 2 events, got %d", len(events))
	}
	lastEvent := events[len(events)-1]
	if lastEvent.Kind != "transition" {
		t.Errorf("expected last event kind='transition', got '%s'", lastEvent.Kind)
	}
	if lastEvent.Note == nil || *lastEvent.Note != failedNote {
		t.Errorf("expected event note='%s', got %v", failedNote, lastEvent.Note)
	}

	// Test 4: active→failed still works (unchanged behavior)
	// Task 2 is in ready state, transition to failed
	readyFailedNote := "ready to fail"
	readyFailed, err := store.TransitionTask(ctx, taskID2, "failed", &readyFailedNote)
	if err != nil {
		t.Fatalf("failed to transition ready→failed: %v", err)
	}
	if readyFailed.State != "failed" {
		t.Errorf("expected task state='failed' after ready→failed, got '%s'", readyFailed.State)
	}

	// Test 5: blocked→ready still works (unchanged behavior)
	_, err = store.TransitionTask(ctx, taskID3, "blocked", nil)
	if err != nil {
		t.Fatalf("failed to transition to blocked: %v", err)
	}
	unblockNote := "unblock and retry"
	unblocked, err := store.TransitionTask(ctx, taskID3, "ready", &unblockNote)
	if err != nil {
		t.Fatalf("failed to transition blocked→ready: %v", err)
	}
	if unblocked.State != "ready" {
		t.Errorf("expected task state='ready', got '%s'", unblocked.State)
	}

	// Test 6: done→failed still fails (should return ErrConflict)
	// First set task 4 to approved, then done
	_, err = store.Conn().ExecContext(ctx, `
		UPDATE task SET state = 'approved' WHERE id = ?
	`, taskID4)
	if err != nil {
		t.Fatalf("failed to set task to approved: %v", err)
	}
	_, err = store.TransitionTask(ctx, taskID4, "done", nil)
	if err != nil {
		t.Fatalf("failed to transition to done: %v", err)
	}

	// Now try to transition done→failed (should fail)
	_, err = store.TransitionTask(ctx, taskID4, "failed", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected done→failed to return ErrConflict, got %v", err)
	}

	// Test 7: blocked→blocked is rejected (no-op, should fail)
	task5, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 5 - NoOp", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task 5: %v", err)
	}
	taskID5 := task5[0].ID
	_, err = store.TransitionTask(ctx, taskID5, "blocked", nil)
	if err != nil {
		t.Fatalf("failed to transition to blocked: %v", err)
	}
	_, err = store.TransitionTask(ctx, taskID5, "blocked", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected blocked→blocked to return ErrConflict, got %v", err)
	}
}

// TestTransitionToSuperseded verifies that tasks can transition to the 'superseded' state
// from active states but not from terminal states.
func TestTransitionToSuperseded(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a document
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks in various states
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 1 - Backlog", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 2 - Ready", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 3 - Failed", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskID1 := tasks[0].ID
	taskID2 := tasks[1].ID
	taskID3 := tasks[2].ID

	// Test 1: backlog→superseded succeeds
	supersededNote := "superseded by task 999"
	superseded, err := store.TransitionTask(ctx, taskID1, "superseded", &supersededNote)
	if err != nil {
		t.Fatalf("failed to transition backlog→superseded: %v", err)
	}
	if superseded.State != "superseded" {
		t.Errorf("expected task state='superseded', got '%s'", superseded.State)
	}

	// Test 2: ready→superseded succeeds
	supersededNote2 := "replaced by newer task"
	superseded2, err := store.TransitionTask(ctx, taskID2, "superseded", &supersededNote2)
	if err != nil {
		t.Fatalf("failed to transition ready→superseded: %v", err)
	}
	if superseded2.State != "superseded" {
		t.Errorf("expected task state='superseded', got '%s'", superseded2.State)
	}

	// Test 3: superseded→superseded should fail (superseded is terminal)
	_, err = store.TransitionTask(ctx, taskID1, "superseded", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected superseded→superseded to return ErrConflict, got %v", err)
	}

	// Test 4: failed→superseded should fail (terminal state)
	// First transition task 3 to failed
	_, err = store.TransitionTask(ctx, taskID3, "blocked", nil)
	if err != nil {
		t.Fatalf("failed to transition to blocked: %v", err)
	}
	_, err = store.TransitionTask(ctx, taskID3, "failed", nil)
	if err != nil {
		t.Fatalf("failed to transition to failed: %v", err)
	}

	// Verify task 3 cannot transition to superseded
	_, err = store.TransitionTask(ctx, taskID3, "superseded", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected failed→superseded to return ErrConflict, got %v", err)
	}
}

// TestTransitionApprovedToAbandoned verifies that tasks can transition from 'approved' to 'abandoned',
// that 'abandoned' is a terminal state (no transitions out), and that only 'approved' can transition to 'abandoned'.
func TestTransitionApprovedToAbandoned(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a document
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 1 - Will be approved", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 2 - Backlog", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 3 - Ready", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskID1 := tasks[0].ID
	taskID2 := tasks[1].ID
	taskID3 := tasks[2].ID

	// Manually set task 1 to approved state (the only state from which we can transition to abandoned)
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "approved", taskID1)
	if err != nil {
		t.Fatalf("failed to set task to approved state: %v", err)
	}

	// Test 1: approved→abandoned succeeds
	abandonedNote := "no longer needed"
	abandoned, err := store.TransitionTask(ctx, taskID1, "abandoned", &abandonedNote)
	if err != nil {
		t.Fatalf("failed to transition approved→abandoned: %v", err)
	}
	if abandoned.State != "abandoned" {
		t.Errorf("expected task state='abandoned', got '%s'", abandoned.State)
	}

	// Test 2: abandoned→* should fail (abandoned is terminal)
	// abandoned→done should fail
	_, err = store.TransitionTask(ctx, taskID1, "done", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected abandoned→done to return ErrConflict, got %v", err)
	}

	// abandoned→blocked should fail
	_, err = store.TransitionTask(ctx, taskID1, "blocked", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected abandoned→blocked to return ErrConflict, got %v", err)
	}

	// abandoned→failed should fail
	_, err = store.TransitionTask(ctx, taskID1, "failed", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected abandoned→failed to return ErrConflict, got %v", err)
	}

	// Test 3: backlog→abandoned should fail (only approved can transition to abandoned)
	_, err = store.TransitionTask(ctx, taskID2, "abandoned", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected backlog→abandoned to return ErrConflict, got %v", err)
	}

	// Test 4: ready→abandoned should fail (only approved can transition to abandoned)
	// Manually set task 3 to ready state
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID3)
	if err != nil {
		t.Fatalf("failed to set task to ready state: %v", err)
	}
	_, err = store.TransitionTask(ctx, taskID3, "abandoned", nil)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ready→abandoned to return ErrConflict, got %v", err)
	}
}

// TestListProjectsWithClaimableFilter verifies that ListProjects with filter.Claimable=true
// returns only projects with at least one claimable task matching the model and kind.
func TestListProjectsWithClaimableFilter(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create two projects
	proj1, err := store.CreateProject(ctx, "project-with-claimable", "https://github.com/example/repo1")
	if err != nil {
		t.Fatalf("failed to create project 1: %v", err)
	}

	proj2, err := store.CreateProject(ctx, "project-blocked-only", "https://github.com/example/repo2")
	if err != nil {
		t.Fatalf("failed to create project 2: %v", err)
	}

	// Create documents for both projects
	doc1, err := store.CreateDocument(ctx, proj1.ID, "design", "DESIGN.md", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 1: %v", err)
	}

	doc2, err := store.CreateDocument(ctx, proj2.ID, "design", "DESIGN.md", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 2: %v", err)
	}

	// Create tasks in both projects
	tasks1, err := store.CreateTasks(ctx, proj1.ID, []TaskInput{
		{Title: "task1", Spec: "spec1", DocumentID: doc1.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks in project 1: %v", err)
	}

	tasks2, err := store.CreateTasks(ctx, proj2.ID, []TaskInput{
		{Title: "task2", Spec: "spec2", DocumentID: doc2.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks in project 2: %v", err)
	}

	// Promote both tasks to ready
	_, err = store.PromoteTask(ctx, tasks1[0].ID)
	if err != nil {
		t.Fatalf("failed to promote task 1: %v", err)
	}

	_, err = store.PromoteTask(ctx, tasks2[0].ID)
	if err != nil {
		t.Fatalf("failed to promote task 2: %v", err)
	}

	// Transition task 2 to blocked to exclude it from claimable results
	_, err = store.TransitionTask(ctx, tasks2[0].ID, "blocked", nil)
	if err != nil {
		t.Fatalf("failed to transition task 2 to blocked: %v", err)
	}

	// List projects with claimable filter
	filter := ProjectListFilter{Claimable: true, Model: &[]string{"haiku"}[0]}
	projects, err := store.ListProjects(ctx, filter)
	if err != nil {
		t.Fatalf("failed to list projects: %v", err)
	}

	// Verify only proj1 is returned
	if len(projects) != 1 {
		t.Errorf("expected 1 project, got %d", len(projects))
	}
	if len(projects) > 0 && projects[0].ID != proj1.ID {
		t.Errorf("expected project %q, got %q", proj1.ID, projects[0].ID)
	}
}

// TestListProjectsClaimableFilterBothModelAndKind verifies that model and kind filters AND-compose.
func TestListProjectsClaimableFilterBothModelAndKind(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create a project
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Create a document
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "DESIGN.md", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement and review tasks
	taskInputs := []TaskInput{
		{Title: "implement-task", Spec: "spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "review-task", Spec: "spec", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"sonnet"}},
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, taskInputs)
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	// Promote both to ready
	for _, task := range tasks {
		_, err = store.PromoteTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("failed to promote task: %v", err)
		}
	}

	// Test 1: Filter for haiku implement - should return project
	implementKind := "implement"
	haikuModel := "haiku"
	filter1 := ProjectListFilter{Claimable: true, Model: &haikuModel, Kind: &implementKind}
	projects1, err := store.ListProjects(ctx, filter1)
	if err != nil {
		t.Fatalf("failed to list projects: %v", err)
	}
	if len(projects1) != 1 {
		t.Errorf("expected 1 project for implement filter, got %d", len(projects1))
	}

	// Test 2: Filter for haiku review - should return project (review kind isn't created yet in this test, but the filter should work)
	reviewKind := "review"
	filter2 := ProjectListFilter{Claimable: true, Model: &haikuModel, Kind: &reviewKind}
	projects2, err := store.ListProjects(ctx, filter2)
	if err != nil {
		t.Fatalf("failed to list projects: %v", err)
	}
	// The project should be included if there's a review task (but we only created implement tasks)
	// In this case, there are no review tasks, so the project should not be returned
	if len(projects2) != 0 {
		t.Errorf("expected 0 projects for review filter, got %d (there should be no review tasks)", len(projects2))
	}
}

// TestListProjectsWithoutClaimableFilterReturnAll verifies that ListProjects without
// claimable filter returns all projects regardless of task state.
func TestListProjectsWithoutClaimableFilterReturnAll(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create two projects
	proj1, err := store.CreateProject(ctx, "project1", "https://github.com/example/repo1")
	if err != nil {
		t.Fatalf("failed to create project 1: %v", err)
	}

	proj2, err := store.CreateProject(ctx, "project2", "https://github.com/example/repo2")
	if err != nil {
		t.Fatalf("failed to create project 2: %v", err)
	}

	// List projects without filter
	filter := ProjectListFilter{}
	projects, err := store.ListProjects(ctx, filter)
	if err != nil {
		t.Fatalf("failed to list projects: %v", err)
	}

	// Both projects should be returned
	if len(projects) != 2 {
		t.Errorf("expected 2 projects without filter, got %d", len(projects))
	}

	// Verify both projects are in the result
	foundProj1 := false
	foundProj2 := false
	for _, p := range projects {
		if p.ID == proj1.ID {
			foundProj1 = true
		}
		if p.ID == proj2.ID {
			foundProj2 = true
		}
	}
	if !foundProj1 {
		t.Errorf("project 1 not found in result")
	}
	if !foundProj2 {
		t.Errorf("project 2 not found in result")
	}
}

// TestArchiveTaskExcludedByDefault tests that an archived task is excluded from default list
// but included when IncludeArchived=true, and archive does not alter task state.
func TestArchiveTaskExcludedByDefault(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create project, document, and task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Visible Task", Spec: "spec1", DocumentID: doc.ID},
		{Title: "Task to Archive", Spec: "spec2", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskToArchiveID := tasks[1].ID
	originalState := tasks[1].State

	// Archive the second task
	archivedTask, err := store.ArchiveTask(ctx, taskToArchiveID)
	if err != nil {
		t.Fatalf("failed to archive task: %v", err)
	}

	// Verify state is unchanged
	if archivedTask.State != originalState {
		t.Errorf("expected state to remain %q, but got %q", originalState, archivedTask.State)
	}
	if archivedTask.ArchivedAt == nil {
		t.Error("expected ArchivedAt to be set, but it's nil")
	}

	// List tasks without IncludeArchived (default) - should exclude archived task
	filter := TaskListFilter{}
	tasks1, err := store.ListTasks(ctx, proj.ID, filter)
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(tasks1) != 1 {
		t.Errorf("expected 1 task (excluding archived), got %d", len(tasks1))
	}
	if len(tasks1) > 0 && tasks1[0].ID == taskToArchiveID {
		t.Error("archived task should be excluded from default list")
	}

	// List tasks with IncludeArchived=true - should include archived task
	filter2 := TaskListFilter{IncludeArchived: true}
	tasks2, err := store.ListTasks(ctx, proj.ID, filter2)
	if err != nil {
		t.Fatalf("failed to list tasks with IncludeArchived: %v", err)
	}
	if len(tasks2) != 2 {
		t.Errorf("expected 2 tasks (including archived), got %d", len(tasks2))
	}

	// Verify the archived task is in the result
	found := false
	for _, task := range tasks2 {
		if task.ID == taskToArchiveID {
			found = true
			break
		}
	}
	if !found {
		t.Error("archived task not found in list with IncludeArchived=true")
	}
}

// TestUnarchiveTaskRestoresVisibility tests that unarchiving a task restores it to default visibility.
func TestUnarchiveTaskRestoresVisibility(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create project, document, and task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	taskID := tasks[0].ID

	// Archive the task
	_, err = store.ArchiveTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to archive task: %v", err)
	}

	// Verify it's excluded from default list
	filter := TaskListFilter{}
	tasksAfterArchive, err := store.ListTasks(ctx, proj.ID, filter)
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(tasksAfterArchive) != 0 {
		t.Errorf("expected 0 tasks after archive, got %d", len(tasksAfterArchive))
	}

	// Unarchive the task
	unarchivedTask, err := store.UnarchiveTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to unarchive task: %v", err)
	}

	// Verify ArchivedAt is cleared
	if unarchivedTask.ArchivedAt != nil {
		t.Errorf("expected ArchivedAt to be nil after unarchive, but got %v", *unarchivedTask.ArchivedAt)
	}

	// List tasks again - should include unarchived task
	tasksAfterUnarchive, err := store.ListTasks(ctx, proj.ID, filter)
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(tasksAfterUnarchive) != 1 {
		t.Errorf("expected 1 task after unarchive, got %d", len(tasksAfterUnarchive))
	}
	if len(tasksAfterUnarchive) > 0 && tasksAfterUnarchive[0].ID != taskID {
		t.Error("unarchived task not found in list")
	}
}

// TestArchiveProjectExcludedByDefault tests that an archived project is excluded from default list
// but included when IncludeArchived=true.
func TestArchiveProjectExcludedByDefault(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create two projects
	_, err = store.CreateProject(ctx, "visible-project", "https://github.com/example/repo1")
	if err != nil {
		t.Fatalf("failed to create project 1: %v", err)
	}

	proj2, err := store.CreateProject(ctx, "project-to-archive", "https://github.com/example/repo2")
	if err != nil {
		t.Fatalf("failed to create project 2: %v", err)
	}

	// Archive the second project
	archivedProj, err := store.ArchiveProject(ctx, proj2.ID)
	if err != nil {
		t.Fatalf("failed to archive project: %v", err)
	}

	if archivedProj.ArchivedAt == nil {
		t.Error("expected ArchivedAt to be set, but it's nil")
	}

	// List projects without IncludeArchived (default) - should exclude archived project
	filter := ProjectListFilter{}
	projects1, err := store.ListProjects(ctx, filter)
	if err != nil {
		t.Fatalf("failed to list projects: %v", err)
	}
	if len(projects1) != 1 {
		t.Errorf("expected 1 project (excluding archived), got %d", len(projects1))
	}
	if len(projects1) > 0 && projects1[0].ID == proj2.ID {
		t.Error("archived project should be excluded from default list")
	}

	// List projects with IncludeArchived=true - should include archived project
	filter2 := ProjectListFilter{IncludeArchived: true}
	projects2, err := store.ListProjects(ctx, filter2)
	if err != nil {
		t.Fatalf("failed to list projects with IncludeArchived: %v", err)
	}
	if len(projects2) != 2 {
		t.Errorf("expected 2 projects (including archived), got %d", len(projects2))
	}

	// Verify the archived project is in the result
	found := false
	for _, p := range projects2 {
		if p.ID == proj2.ID {
			found = true
			break
		}
	}
	if !found {
		t.Error("archived project not found in list with IncludeArchived=true")
	}
}

// TestUnarchiveProjectRestoresVisibility tests that unarchiving a project restores it to default visibility.
func TestUnarchiveProjectRestoresVisibility(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create a project
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	// Archive the project
	_, err = store.ArchiveProject(ctx, proj.ID)
	if err != nil {
		t.Fatalf("failed to archive project: %v", err)
	}

	// Verify it's excluded from default list
	filter := ProjectListFilter{}
	projectsAfterArchive, err := store.ListProjects(ctx, filter)
	if err != nil {
		t.Fatalf("failed to list projects: %v", err)
	}
	if len(projectsAfterArchive) != 0 {
		t.Errorf("expected 0 projects after archive, got %d", len(projectsAfterArchive))
	}

	// Unarchive the project
	unarchivedProj, err := store.UnarchiveProject(ctx, proj.ID)
	if err != nil {
		t.Fatalf("failed to unarchive project: %v", err)
	}

	// Verify ArchivedAt is cleared
	if unarchivedProj.ArchivedAt != nil {
		t.Errorf("expected ArchivedAt to be nil after unarchive, but got %v", *unarchivedProj.ArchivedAt)
	}

	// List projects again - should include unarchived project
	projectsAfterUnarchive, err := store.ListProjects(ctx, filter)
	if err != nil {
		t.Fatalf("failed to list projects: %v", err)
	}
	if len(projectsAfterUnarchive) != 1 {
		t.Errorf("expected 1 project after unarchive, got %d", len(projectsAfterUnarchive))
	}
	if len(projectsAfterUnarchive) > 0 && projectsAfterUnarchive[0].ID != proj.ID {
		t.Error("unarchived project not found in list")
	}
}

// TestClaimableProjectsExcludeArchived tests that the claimable-projects poll excludes archived projects,
// ensuring archived/orphan projects are never dispatched to workers.
func TestClaimableProjectsExcludeArchived(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create two projects
	proj1, err := store.CreateProject(ctx, "claimable-project", "https://github.com/example/repo1")
	if err != nil {
		t.Fatalf("failed to create project 1: %v", err)
	}

	proj2, err := store.CreateProject(ctx, "archived-project", "https://github.com/example/repo2")
	if err != nil {
		t.Fatalf("failed to create project 2: %v", err)
	}

	// Create documents in both projects
	doc1, err := store.CreateDocument(ctx, proj1.ID, "design", "Design 1", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 1: %v", err)
	}

	doc2, err := store.CreateDocument(ctx, proj2.ID, "design", "Design 2", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 2: %v", err)
	}

	// Create claimable tasks in both projects
	tasks1, err := store.CreateTasks(ctx, proj1.ID, []TaskInput{
		{Title: "Task 1", Spec: "spec1", DocumentID: doc1.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks in project 1: %v", err)
	}

	tasks2, err := store.CreateTasks(ctx, proj2.ID, []TaskInput{
		{Title: "Task 2", Spec: "spec2", DocumentID: doc2.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks in project 2: %v", err)
	}

	// Promote both tasks to ready
	for _, taskID := range []string{tasks1[0].ID, tasks2[0].ID} {
		_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", taskID)
		if err != nil {
			t.Fatalf("failed to promote task: %v", err)
		}
	}

	// Verify both projects are claimable before archiving
	haikuModel := "haiku"
	implementKind := "implement"
	filter := ProjectListFilter{Claimable: true, Model: &haikuModel, Kind: &implementKind}
	projectsBeforeArchive, err := store.ListProjects(ctx, filter)
	if err != nil {
		t.Fatalf("failed to list claimable projects: %v", err)
	}
	if len(projectsBeforeArchive) != 2 {
		t.Errorf("expected 2 claimable projects before archive, got %d", len(projectsBeforeArchive))
	}

	// Archive the second project
	_, err = store.ArchiveProject(ctx, proj2.ID)
	if err != nil {
		t.Fatalf("failed to archive project: %v", err)
	}

	// Verify only the non-archived project is claimable now
	projectsAfterArchive, err := store.ListProjects(ctx, filter)
	if err != nil {
		t.Fatalf("failed to list claimable projects: %v", err)
	}
	if len(projectsAfterArchive) != 1 {
		t.Errorf("expected 1 claimable project after archive, got %d", len(projectsAfterArchive))
	}
	if len(projectsAfterArchive) > 0 && projectsAfterArchive[0].ID == proj2.ID {
		t.Error("archived project should be excluded from claimable poll")
	}
}

// TestClaimableTasksExcludeArchived tests that claimable task queries exclude archived tasks.
func TestClaimableTasksExcludeArchived(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	// Create project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create two claimable tasks
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 1", Spec: "spec1", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Task 2", Spec: "spec2", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	// Promote both to ready
	for _, task := range tasks {
		_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", task.ID)
		if err != nil {
			t.Fatalf("failed to promote task: %v", err)
		}
	}

	// Verify both are claimable before archiving
	filter := TaskListFilter{Claimable: true, Kind: strPtr("implement")}
	claimableBeforeArchive, err := store.ListTasks(ctx, proj.ID, filter)
	if err != nil {
		t.Fatalf("failed to list claimable tasks: %v", err)
	}
	if len(claimableBeforeArchive) != 2 {
		t.Errorf("expected 2 claimable tasks before archive, got %d", len(claimableBeforeArchive))
	}

	// Archive one task
	_, err = store.ArchiveTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatalf("failed to archive task: %v", err)
	}

	// Verify only one is claimable now
	claimableAfterArchive, err := store.ListTasks(ctx, proj.ID, filter)
	if err != nil {
		t.Fatalf("failed to list claimable tasks: %v", err)
	}
	if len(claimableAfterArchive) != 1 {
		t.Errorf("expected 1 claimable task after archive, got %d", len(claimableAfterArchive))
	}
	if len(claimableAfterArchive) > 0 && claimableAfterArchive[0].ID == tasks[0].ID {
		t.Error("archived task should be excluded from claimable list")
	}
}

// TestHeldTaskNotClaimable verifies that a held task does not appear in the claimable listing.
func TestHeldTaskNotClaimable(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Test task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	// Verify task is claimable before holding
	claimableBefore, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	foundBefore := false
	for _, task := range claimableBefore {
		if task.ID == taskID {
			foundBefore = true
			break
		}
	}
	if !foundBefore {
		t.Error("task should be claimable before hold")
	}

	// Hold the task
	_, err = store.HoldTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to hold task: %v", err)
	}

	// Verify task is NOT in claimable list after holding
	claimableAfter, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	foundAfter := false
	for _, task := range claimableAfter {
		if task.ID == taskID {
			foundAfter = true
			break
		}
	}
	if foundAfter {
		t.Error("held task should not appear in claimable list")
	}

	// Verify the task still exists and is held
	heldTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if !heldTask.Held {
		t.Error("task should have held=true")
	}
}

// TestRejectVerdictOnHeldTaskDoesNotAutoTransition verifies that a reject verdict on a held task
// does NOT move it to ready state - it stays put for manual operator intervention.
func TestRejectVerdictOnHeldTaskDoesNotAutoTransition(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Get the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Hold the parent task
	_, err = store.HoldTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to hold task: %v", err)
	}

	// Claim and submit review task with reject verdict
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	reject := "reject"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task with verdict: %v", err)
	}

	// Verify parent task stayed in review state (not auto-transitioned to ready due to hold)
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "review" {
		t.Errorf("expected held parent task state to stay in 'review', got '%s'", parentTask.State)
	}
	if !parentTask.Held {
		t.Error("parent task should still be held")
	}
}

// TestReleaseRestoresFlow verifies that releasing a held task restores normal automated flow.
func TestReleaseRestoresFlow(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Test task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	// Hold the task
	_, err = store.HoldTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to hold task: %v", err)
	}

	// Verify task is not claimable while held
	claimableWhileHeld, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	for _, task := range claimableWhileHeld {
		if task.ID == taskID {
			t.Error("held task should not be claimable")
		}
	}

	// Release the task
	_, err = store.ReleaseTask(ctx, taskID, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to release task: %v", err)
	}

	// Verify task is claimable again after release
	claimableAfterRelease, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	foundAfterRelease := false
	for _, task := range claimableAfterRelease {
		if task.ID == taskID {
			foundAfterRelease = true
			break
		}
	}
	if !foundAfterRelease {
		t.Error("released task should be claimable again")
	}

	// Verify held flag is cleared
	releasedTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if releasedTask.Held {
		t.Error("task should have held=false after release")
	}
}

// TestReleaseAggregatesReviewWithApproval verifies that releasing a held review task aggregates verdicts when all reviews are done.
func TestReleaseAggregatesReviewWithApproval(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create an implement task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Test implementation",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote and claim
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit for review
	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Verify task is in review
	reviewTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if reviewTask.State != "review" {
		t.Errorf("task should be in review state, got %s", reviewTask.State)
	}

	// Get the review task
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	if len(reviewTasks) != 1 {
		t.Fatalf("expected 1 review task, got %d", len(reviewTasks))
	}
	reviewTaskID := reviewTasks[0].ID

	// Claim and approve the review
	_, err = store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Task should be approved now
	approvedTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if approvedTask.State != "approved" {
		t.Errorf("task should be approved, got %s", approvedTask.State)
	}

	// Now hold the task and re-run the test with SubmitTask that skips aggregation due to hold
	// First, transition back to review by superseding and re-submitting (not part of test scenario)
	// Instead, we'll test the scenario where task is held before final review verdict is submitted

	// Create a new test scenario: hold task, submit final verdict while held, release and aggregate
	tasks2, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Test implementation 2",
			Spec:         "Test spec 2",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task 2: %v", err)
	}
	taskID2 := tasks2[0].ID

	_, err = store.PromoteTask(ctx, taskID2)
	if err != nil {
		t.Fatalf("failed to promote task 2: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID2, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task 2: %v", err)
	}

	_, err = store.SubmitTask(ctx, taskID2, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#101"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task 2: %v", err)
	}

	// Get the review task
	reviewTasks2, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks 2: %v", err)
	}
	var reviewTaskID2 string
	for _, rt := range reviewTasks2 {
		if rt.TargetTaskID != nil && *rt.TargetTaskID == taskID2 {
			reviewTaskID2 = rt.ID
			break
		}
	}
	if reviewTaskID2 == "" {
		t.Fatalf("could not find review task for task 2")
	}

	// Hold the parent task
	_, err = store.HoldTask(ctx, taskID2)
	if err != nil {
		t.Fatalf("failed to hold task 2: %v", err)
	}

	// Claim and approve the review while parent is held
	_, err = store.ClaimTask(ctx, reviewTaskID2, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task 2: %v", err)
	}

	_, err = store.SubmitTask(ctx, reviewTaskID2, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict 2: %v", err)
	}

	// Task should still be in review because it was held
	heldReviewTask, err := store.GetTask(ctx, taskID2)
	if err != nil {
		t.Fatalf("failed to get held task: %v", err)
	}
	if heldReviewTask.State != "review" {
		t.Errorf("held task should remain in review, got %s", heldReviewTask.State)
	}

	// Release the task - should trigger aggregation and move to approved
	releasedTask2, err := store.ReleaseTask(ctx, taskID2, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to release task 2: %v", err)
	}

	if releasedTask2.State != "approved" {
		t.Errorf("released task should be approved after aggregation, got %s", releasedTask2.State)
	}
}

// TestReleaseAggregatesReviewWithRejection verifies that releasing a held review task with rejection moves to ready.
func TestReleaseAggregatesReviewWithRejection(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Test implementation",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Get the review task
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	var reviewTaskID string
	for _, rt := range reviewTasks {
		if rt.TargetTaskID != nil && *rt.TargetTaskID == taskID {
			reviewTaskID = rt.ID
			break
		}
	}

	// Hold the parent task
	_, err = store.HoldTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to hold task: %v", err)
	}

	// Claim and reject the review while parent is held
	_, err = store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	reject := "reject"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Task should still be in review because it was held
	heldReviewTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get held task: %v", err)
	}
	if heldReviewTask.State != "review" {
		t.Errorf("held task should remain in review, got %s", heldReviewTask.State)
	}

	// Release the task - should trigger aggregation and move to ready
	releasedTask, err := store.ReleaseTask(ctx, taskID, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to release task: %v", err)
	}

	if releasedTask.State != "ready" {
		t.Errorf("released task should be ready after rejection, got %s", releasedTask.State)
	}

	// Verify review_round was incremented
	if releasedTask.ReviewRound != 1 {
		t.Errorf("review_round should be 1 after rejection, got %d", releasedTask.ReviewRound)
	}
}

// TestReleaseNonReviewTaskUnchanged verifies that releasing a non-review task doesn't trigger aggregation.
func TestReleaseNonReviewTaskUnchanged(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Test implementation",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	// Hold the task in ready state
	_, err = store.HoldTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to hold task: %v", err)
	}

	// Release the task
	releasedTask, err := store.ReleaseTask(ctx, taskID, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to release task: %v", err)
	}

	// Task should still be in ready state
	if releasedTask.State != "ready" {
		t.Errorf("non-review task should remain in ready state, got %s", releasedTask.State)
	}
}

// TestHoldFromDifferentStates verifies that hold works from ready, in_progress, and review states.
func TestHoldFromDifferentStates(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Test hold from ready state
	tasks1, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Task in ready",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	task1ID := tasks1[0].ID
	_, err = store.PromoteTask(ctx, task1ID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	heldTask1, err := store.HoldTask(ctx, task1ID)
	if err != nil {
		t.Fatalf("failed to hold ready task: %v", err)
	}
	if !heldTask1.Held {
		t.Error("ready task should be held")
	}
	if heldTask1.State != "ready" {
		t.Error("task state should remain ready when held")
	}

	// Test hold from in_progress state
	tasks2, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Task in progress",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	task2ID := tasks2[0].ID
	_, err = store.PromoteTask(ctx, task2ID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, task2ID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	heldTask2, err := store.HoldTask(ctx, task2ID)
	if err != nil {
		t.Fatalf("failed to hold in_progress task: %v", err)
	}
	if !heldTask2.Held {
		t.Error("in_progress task should be held")
	}
	if heldTask2.State != "in_progress" {
		t.Error("task state should remain in_progress when held")
	}

	// Test hold from review state
	tasks3, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Task in review",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	task3ID := tasks3[0].ID
	_, err = store.PromoteTask(ctx, task3ID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, task3ID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	_, err = store.SubmitTask(ctx, task3ID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	heldTask3, err := store.HoldTask(ctx, task3ID)
	if err != nil {
		t.Fatalf("failed to hold review task: %v", err)
	}
	if !heldTask3.Held {
		t.Error("review task should be held")
	}
	if heldTask3.State != "review" {
		t.Error("task state should remain review when held")
	}
}

// TestRejectVerdictOnTerminalTaskDoesNotResurrect verifies that a review verdict
// (approve or reject) on a parent task already in a terminal state (failed or blocked)
// does NOT move it back to ready or approved. Terminal states must stay terminal.
func TestRejectVerdictOnTerminalTaskDoesNotResurrect(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Test with failed state
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature - failed test",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Get the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Transition parent to failed state
	_, err = store.TransitionTask(ctx, taskID, "failed", nil)
	if err != nil {
		t.Fatalf("failed to transition task to failed: %v", err)
	}

	// Claim and submit review task with reject verdict
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	reject := "reject"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task with reject verdict: %v", err)
	}

	// Verify parent task stayed in failed state (not resurrected to ready)
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "failed" {
		t.Errorf("expected parent task in failed state to stay failed, got '%s'", parentTask.State)
	}

	// Now test with blocked state and approve verdict
	tasks2, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature - blocked test",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create second task: %v", err)
	}
	taskID2 := tasks2[0].ID

	_, err = store.PromoteTask(ctx, taskID2)
	if err != nil {
		t.Fatalf("failed to promote second task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID2, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim second task: %v", err)
	}
	_, err = store.SubmitTask(ctx, taskID2, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#101"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit second implement task: %v", err)
	}

	// Get the second review task
	allTasks2, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks for second task: %v", err)
	}

	var reviewTask2 *Task
	for i := range allTasks2 {
		if allTasks2[i].Kind == "review" && allTasks2[i].TargetTaskID != nil && *allTasks2[i].TargetTaskID == taskID2 {
			reviewTask2 = &allTasks2[i]
			break
		}
	}
	if reviewTask2 == nil {
		t.Fatalf("second review task not found")
	}

	// Transition second parent to blocked state
	_, err = store.TransitionTask(ctx, taskID2, "blocked", nil)
	if err != nil {
		t.Fatalf("failed to transition second task to blocked: %v", err)
	}

	// Claim and submit review task with approve verdict
	_, err = store.ClaimTask(ctx, reviewTask2.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim second review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask2.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit second review task with approve verdict: %v", err)
	}

	// Verify second parent task stayed in blocked state (not resurrected to approved)
	parentTask2, err := store.GetTask(ctx, taskID2)
	if err != nil {
		t.Fatalf("failed to get second parent task: %v", err)
	}
	if parentTask2.State != "blocked" {
		t.Errorf("expected parent task in blocked state to stay blocked, got '%s'", parentTask2.State)
	}
}

// TestSubmitImplementTaskNoOpResolution covers the review-verified no-op path:
// a worker that finds the acceptance already satisfied on main (empty diff) submits
// with a no_op marker and NO pr link. The submit must be accepted, a review task must
// still auto-spawn (flagged as no-op), and an agent_merge parent must reach done via
// approved -> done with no PR/merge once the reviewer approves.
func TestSubmitImplementTaskNoOpResolution(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// agent_merge=true so the reviewer drives it straight to done.
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Already-satisfied task",
			Spec:         "Acceptance already met on main",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			AgentMerge:   true,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// No-op submit: a no_op marker and NO pr link.
	noOpLinks := []LinkInput{{Kind: "no_op", Value: "already-satisfied"}}
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "acceptance already satisfied on main; no changes needed", nil, noOpLinks, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("no-op submit should be accepted, got error: %v", err)
	}
	if submitted.State != "review" {
		t.Errorf("expected state 'review' after submit, got %q", submitted.State)
	}
	if submitted.ReviewRound != 1 {
		t.Errorf("expected review_round 1, got %d", submitted.ReviewRound)
	}

	// The no_op link is recorded and there is no pr link.
	var sawNoOp, sawPR bool
	for _, l := range submitted.Links {
		if l.Kind == "no_op" && l.Value == "already-satisfied" {
			sawNoOp = true
		}
		if l.Kind == "pr" {
			sawPR = true
		}
	}
	if !sawNoOp {
		t.Errorf("expected a no_op link on the task, links=%v", submitted.Links)
	}
	if sawPR {
		t.Errorf("did not expect a pr link on a no-op submit, links=%v", submitted.Links)
	}

	// A review task auto-spawned even without a PR, and its brief flags the no-op.
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("expected a review task to auto-spawn on a no-op submit")
	}
	if !strings.Contains(reviewTask.Spec, "NO-OP submission") {
		t.Errorf("expected review brief to flag the no-op, spec=%q", reviewTask.Spec)
	}

	// Reviewer verifies the claim holds and approves.
	if _, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}
	approve := "approve"
	if _, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "verified satisfied on main", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parent.State != "done" {
		t.Fatalf("expected parent 'done' after sole reviewer approves (agent_merge=true no-op auto-finalizes), got %q", parent.State)
	}
}

// TestSubmitNoOpLinkKindAccepted verifies the no_op link kind passes link validation.
func TestSubmitNoOpLinkKindAccepted(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "T", Spec: "S", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	if _, err = store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	if _, err = store.SubmitTask(ctx, taskID, "agent-1", "noop", nil, []LinkInput{{Kind: "no_op", Value: "already-satisfied"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("expected no_op link kind to be accepted, got: %v", err)
	}
}

// TestSuperseededByNilByDefault verifies that SupersededBy is nil by default.
func TestSuperseededByNilByDefault(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Verify SupersededBy is nil by default
	if tasks[0].SupersededBy != nil {
		t.Errorf("expected SupersededBy to be nil by default, got %v", tasks[0].SupersededBy)
	}

	// Also verify via GetTask
	taskWithDeps, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if taskWithDeps.SupersededBy != nil {
		t.Errorf("expected SupersededBy to be nil in GetTask, got %v", taskWithDeps.SupersededBy)
	}

	// And via ListTasks
	listTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	if len(listTasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(listTasks))
	}

	if listTasks[0].SupersededBy != nil {
		t.Errorf("expected SupersededBy to be nil in ListTasks, got %v", listTasks[0].SupersededBy)
	}
}

// TestSuperseededByWithValue verifies that SupersededBy is properly propagated when set.
func TestSuperseededByWithValue(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Create a second task to be referenced as the superseding task
	otherTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Other Task", Spec: "Other spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create other task: %v", err)
	}
	otherTaskID := otherTasks[0].ID

	// Manually set superseded_by in the database
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET superseded_by = ? WHERE id = ?", otherTaskID, taskID)
	if err != nil {
		t.Fatalf("failed to set superseded_by: %v", err)
	}

	// Verify GetTask returns the superseded_by value
	taskWithDeps, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if taskWithDeps.SupersededBy == nil {
		t.Errorf("expected SupersededBy to be set in GetTask, got nil")
	} else if *taskWithDeps.SupersededBy != otherTaskID {
		t.Errorf("expected SupersededBy to be %s, got %s", otherTaskID, *taskWithDeps.SupersededBy)
	}

	// Verify ListTasks returns the superseded_by value
	listTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var foundTask *Task
	for i := range listTasks {
		if listTasks[i].ID == taskID {
			foundTask = &listTasks[i]
			break
		}
	}

	if foundTask == nil {
		t.Fatalf("expected to find task %s in ListTasks", taskID)
	}

	if foundTask.SupersededBy == nil {
		t.Errorf("expected SupersededBy to be set in ListTasks, got nil")
	} else if *foundTask.SupersededBy != otherTaskID {
		t.Errorf("expected SupersededBy to be %s in ListTasks, got %s", otherTaskID, *foundTask.SupersededBy)
	}
}

func TestListDependents(t *testing.T) {
	dbPath := ":memory:"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-proj", "github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks: A (no dependencies), B and C (both depend on A)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Key: "task-a", Title: "Task A", Spec: "Spec A", DocumentID: doc.ID},
		{Title: "Task B", Spec: "Spec B", DocumentID: doc.ID, DependsOn: []string{"task-a"}},
		{Title: "Task C", Spec: "Spec C", DocumentID: doc.ID, DependsOn: []string{"task-a"}},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskA := tasks[0]
	taskB := tasks[1]
	taskC := tasks[2]

	// ListDependents(A) should return [B, C]
	dependents, err := store.ListDependents(ctx, taskA.ID)
	if err != nil {
		t.Fatalf("failed to list dependents: %v", err)
	}

	if len(dependents) != 2 {
		t.Errorf("expected 2 dependents, got %d", len(dependents))
	}

	// Check that both B and C are in the dependents
	dependentSet := make(map[string]bool)
	for _, id := range dependents {
		dependentSet[id] = true
	}

	if !dependentSet[taskB.ID] {
		t.Errorf("expected task B (%s) in dependents, got %v", taskB.ID, dependents)
	}
	if !dependentSet[taskC.ID] {
		t.Errorf("expected task C (%s) in dependents, got %v", taskC.ID, dependents)
	}

	// ListDependents(B) should return empty (B has no dependents)
	dependentsB, err := store.ListDependents(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("failed to list dependents of B: %v", err)
	}

	if len(dependentsB) != 0 {
		t.Errorf("expected 0 dependents for task B, got %d", len(dependentsB))
	}
}

// TestSetTaskDepends verifies that setTaskDepends correctly replaces a task's dependencies within a transaction.
func TestSetTaskDepends(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks: A, B, C, D (B initially depends on A)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Key: "task-a", Title: "Task A", Spec: "Spec A", DocumentID: doc.ID},
		{Title: "Task B", Spec: "Spec B", DocumentID: doc.ID, DependsOn: []string{"task-a"}},
		{Key: "task-c", Title: "Task C", Spec: "Spec C", DocumentID: doc.ID},
		{Key: "task-d", Title: "Task D", Spec: "Spec D", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskA := tasks[0]
	taskB := tasks[1]
	taskC := tasks[2]
	taskD := tasks[3]

	// Verify initial state: B depends on A
	taskBWithDeps, err := store.GetTask(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("failed to get task B: %v", err)
	}
	if len(taskBWithDeps.DependsOn) != 1 || taskBWithDeps.DependsOn[0] != taskA.ID {
		t.Errorf("expected B to depend on A, got %v", taskBWithDeps.DependsOn)
	}

	// Use setTaskDepends to change B's dependencies from [A] to [C, D]
	tx, err := store.Conn().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	err = store.(*sqliteStore).setTaskDepends(ctx, tx, taskB.ID, []string{taskC.ID, taskD.ID})
	if err != nil {
		t.Fatalf("failed to set task depends: %v", err)
	}

	err = tx.Commit()
	if err != nil {
		t.Fatalf("failed to commit transaction: %v", err)
	}

	// Verify the change: B should now depend on C and D
	taskBAfter, err := store.GetTask(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("failed to get task B after update: %v", err)
	}

	if len(taskBAfter.DependsOn) != 2 {
		t.Errorf("expected B to have 2 dependencies, got %d", len(taskBAfter.DependsOn))
	}

	// Check that both C and D are in the dependencies
	depSet := make(map[string]bool)
	for _, id := range taskBAfter.DependsOn {
		depSet[id] = true
	}

	if !depSet[taskC.ID] {
		t.Errorf("expected B to depend on C (%s), got %v", taskC.ID, taskBAfter.DependsOn)
	}
	if !depSet[taskD.ID] {
		t.Errorf("expected B to depend on D (%s), got %v", taskD.ID, taskBAfter.DependsOn)
	}

	// Verify that A is no longer in B's dependencies
	if depSet[taskA.ID] {
		t.Errorf("expected A to not be in B's dependencies, but it is: %v", taskBAfter.DependsOn)
	}

	// Test replacing with an empty list (removing all dependencies)
	tx2, err := store.Conn().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin second transaction: %v", err)
	}
	defer tx2.Rollback()

	err = store.(*sqliteStore).setTaskDepends(ctx, tx2, taskB.ID, []string{})
	if err != nil {
		t.Fatalf("failed to clear dependencies: %v", err)
	}

	err = tx2.Commit()
	if err != nil {
		t.Fatalf("failed to commit second transaction: %v", err)
	}

	// Verify that B has no dependencies
	taskBEmpty, err := store.GetTask(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("failed to get task B after clearing: %v", err)
	}

	if len(taskBEmpty.DependsOn) != 0 {
		t.Errorf("expected B to have 0 dependencies, got %d: %v", len(taskBEmpty.DependsOn), taskBEmpty.DependsOn)
	}
}

func TestUpdateTaskDependsOn(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks: A, B, C, D
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Key: "task-a", Title: "Task A", Spec: "Spec A", DocumentID: doc.ID},
		{Title: "Task B", Spec: "Spec B", DocumentID: doc.ID},
		{Key: "task-c", Title: "Task C", Spec: "Spec C", DocumentID: doc.ID},
		{Key: "task-d", Title: "Task D", Spec: "Spec D", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	taskA := tasks[0]
	taskB := tasks[1]
	taskC := tasks[2]
	taskD := tasks[3]

	// Test 1: Successfully update task B to depend on C
	updatedTask, err := store.UpdateTaskDependsOn(ctx, taskB.ID, []string{taskC.ID})
	if err != nil {
		t.Errorf("UpdateTaskDependsOn should succeed for valid deps, got error: %v", err)
	}
	if updatedTask.ID != taskB.ID {
		t.Errorf("expected updated task ID to be %s, got %s", taskB.ID, updatedTask.ID)
	}

	// Verify the update took effect
	taskBAfter, err := store.GetTask(ctx, taskB.ID)
	if err != nil {
		t.Fatalf("failed to get task B after update: %v", err)
	}
	if len(taskBAfter.DependsOn) != 1 || taskBAfter.DependsOn[0] != taskC.ID {
		t.Errorf("expected B to depend on C, got %v", taskBAfter.DependsOn)
	}

	// Test 2: Reject self-dependency
	_, err = store.UpdateTaskDependsOn(ctx, taskA.ID, []string{taskA.ID})
	if err == nil {
		t.Error("UpdateTaskDependsOn should reject self-dependency")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) || valErr.Code != "SELF_DEPENDENCY" {
		t.Errorf("expected ValidationError with code SELF_DEPENDENCY, got %v (type %T)", err, err)
	}

	// Test 3: Reject cycle creation
	// First set A -> B
	_, err = store.UpdateTaskDependsOn(ctx, taskA.ID, []string{taskB.ID})
	if err != nil {
		t.Fatalf("failed to set A depends on B: %v", err)
	}
	// Now try to make B -> A (would create cycle since A -> B -> A)
	_, err = store.UpdateTaskDependsOn(ctx, taskB.ID, []string{taskA.ID})
	if err == nil {
		t.Error("UpdateTaskDependsOn should reject cycle creation")
	}
	var confErr *ConflictError
	if !errors.As(err, &confErr) || confErr.Code != "CYCLE_DETECTED" {
		t.Errorf("expected ConflictError with code CYCLE_DETECTED, got %v (type %T)", err, err)
	}

	// Test 4: Update to multiple dependencies
	_, err = store.UpdateTaskDependsOn(ctx, taskD.ID, []string{taskA.ID, taskB.ID, taskC.ID})
	if err != nil {
		t.Errorf("UpdateTaskDependsOn should succeed for multiple valid deps, got error: %v", err)
	}

	taskDAfter, err := store.GetTask(ctx, taskD.ID)
	if err != nil {
		t.Fatalf("failed to get task D after update: %v", err)
	}
	if len(taskDAfter.DependsOn) != 3 {
		t.Errorf("expected D to have 3 dependencies, got %d", len(taskDAfter.DependsOn))
	}

	// Test 5: Clear dependencies (update with empty list)
	_, err = store.UpdateTaskDependsOn(ctx, taskD.ID, []string{})
	if err != nil {
		t.Errorf("UpdateTaskDependsOn should succeed for empty deps, got error: %v", err)
	}

	taskDCleared, err := store.GetTask(ctx, taskD.ID)
	if err != nil {
		t.Fatalf("failed to get task D after clearing: %v", err)
	}
	if len(taskDCleared.DependsOn) != 0 {
		t.Errorf("expected D to have 0 dependencies, got %d: %v", len(taskDCleared.DependsOn), taskDCleared.DependsOn)
	}
}

func TestSupersededTask(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create tasks: oldTask (to be superseded), upstreamDep, dependent (depends on oldTask)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Upstream Dep", Spec: "Spec", DocumentID: doc.ID},
		{Title: "Old Task", Spec: "Spec", DocumentID: doc.ID, DependsOn: []string{} /* will be set */},
		{Title: "Dependent Task", Spec: "Spec", DocumentID: doc.ID, DependsOn: []string{} /* will be set */},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	upstreamDep := tasks[0]
	oldTask := tasks[1]
	dependent := tasks[2]

	// Set oldTask -> upstreamDep
	_, err = store.UpdateTaskDependsOn(ctx, oldTask.ID, []string{upstreamDep.ID})
	if err != nil {
		t.Fatalf("failed to set oldTask dependency: %v", err)
	}

	// Set dependent -> oldTask
	_, err = store.UpdateTaskDependsOn(ctx, dependent.ID, []string{oldTask.ID})
	if err != nil {
		t.Fatalf("failed to set dependent: %v", err)
	}

	// Verify setup
	oldTaskBefore, err := store.GetTask(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("failed to get oldTask before supersede: %v", err)
	}
	if len(oldTaskBefore.DependsOn) != 1 || oldTaskBefore.DependsOn[0] != upstreamDep.ID {
		t.Errorf("expected oldTask to depend on upstreamDep, got %v", oldTaskBefore.DependsOn)
	}

	dependentBefore, err := store.GetTask(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("failed to get dependent before supersede: %v", err)
	}
	if len(dependentBefore.DependsOn) != 1 || dependentBefore.DependsOn[0] != oldTask.ID {
		t.Errorf("expected dependent to depend on oldTask, got %v", dependentBefore.DependsOn)
	}

	// Supersede oldTask
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	// Verify new task
	if newTask.ID == oldTask.ID {
		t.Errorf("expected new task ID to be different from old task ID")
	}
	if newTask.Title != oldTask.Title {
		t.Errorf("expected new task title to match old task, got %q", newTask.Title)
	}
	if newTask.Spec != oldTask.Spec {
		t.Errorf("expected new task spec to match old task, got %q", newTask.Spec)
	}
	if newTask.Model != oldTask.Model {
		t.Errorf("expected new task model to match old task, got %q", newTask.Model)
	}
	if newTask.Kind != oldTask.Kind {
		t.Errorf("expected new task kind to match old task, got %q", newTask.Kind)
	}
	if newTask.AgentMerge != oldTask.AgentMerge {
		t.Errorf("expected new task agent_merge to match old task")
	}
	if newTask.State != "backlog" {
		t.Errorf("expected new task state to be backlog, got %q", newTask.State)
	}
	if newTask.ReviewRound != 0 {
		t.Errorf("expected new task review_round to be 0, got %d", newTask.ReviewRound)
	}

	// Verify new task has upstream dependencies copied
	newTaskFull, err := store.GetTask(ctx, newTask.ID)
	if err != nil {
		t.Fatalf("failed to get new task: %v", err)
	}
	if len(newTaskFull.DependsOn) != 1 || newTaskFull.DependsOn[0] != upstreamDep.ID {
		t.Errorf("expected new task to have copied dependencies, got %v", newTaskFull.DependsOn)
	}

	// Verify old task is superseded
	oldTaskAfter, err := store.GetTask(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("failed to get oldTask after supersede: %v", err)
	}
	if oldTaskAfter.State != "superseded" {
		t.Errorf("expected oldTask state to be superseded, got %q", oldTaskAfter.State)
	}
	if oldTaskAfter.SupersededBy == nil || *oldTaskAfter.SupersededBy != newTask.ID {
		t.Errorf("expected oldTask.SupersededBy to be %s, got %v", newTask.ID, oldTaskAfter.SupersededBy)
	}

	// Verify dependent has been re-pointed to new task
	dependentAfter, err := store.GetTask(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("failed to get dependent after supersede: %v", err)
	}
	if len(dependentAfter.DependsOn) != 1 || dependentAfter.DependsOn[0] != newTask.ID {
		t.Errorf("expected dependent to now depend on newTask, got %v", dependentAfter.DependsOn)
	}

	// Verify task_superseded event was emitted
	events, err := store.ListEvents(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "task_superseded" {
			found = true
			if e.Actor != "system" {
				t.Errorf("expected event actor to be system, got %q", e.Actor)
			}
			if e.Note == nil || *e.Note != fmt.Sprintf("Superseded by %s", newTask.ID) {
				t.Errorf("expected event note to mention new task, got %v", e.Note)
			}
		}
	}
	if !found {
		t.Errorf("expected task_superseded event to be emitted")
	}
}

func TestSupersededTaskWithModelOverride(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a task with default model
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Old Task", Spec: "Spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	oldTask := tasks[0]

	if oldTask.Model != "haiku" {
		t.Errorf("expected oldTask model to be haiku, got %q", oldTask.Model)
	}

	// Supersede with model override to sonnet
	newTaskModel := "sonnet"
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, &newTaskModel)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	if newTask.Model != "sonnet" {
		t.Errorf("expected new task model to be sonnet, got %q", newTask.Model)
	}
}

func TestSupersededTaskNotFound(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	_, err = store.SupersedeTask(ctx, "nonexistent-task-id", nil)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSupersededTaskTerminalState(t *testing.T) {
	tests := []string{"done", "failed", "abandoned", "superseded"}

	for _, terminalState := range tests {
		t.Run("state_"+terminalState, func(t *testing.T) {
			ctx := context.Background()
			store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
			if err != nil {
				t.Fatalf("failed to open database: %v", err)
			}
			defer store.Close()

			proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
			if err != nil {
				t.Fatalf("failed to create project: %v", err)
			}

			doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
			if err != nil {
				t.Fatalf("failed to create document: %v", err)
			}

			// Create task and dependent
			tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
				{Title: "Task", Spec: "Spec", DocumentID: doc.ID},
				{Title: "Dependent", Spec: "Spec", DocumentID: doc.ID},
			})
			if err != nil {
				t.Fatalf("failed to create tasks: %v", err)
			}
			task := tasks[0]
			dependent := tasks[1]

			// Set dependent to depend on task
			_, err = store.UpdateTaskDependsOn(ctx, dependent.ID, []string{task.ID})
			if err != nil {
				t.Fatalf("failed to set dependency: %v", err)
			}

			// Manually update task state to terminal state
			conn := store.Conn()
			_, err = conn.ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", terminalState, task.ID)
			if err != nil {
				t.Fatalf("failed to update task state: %v", err)
			}

			// Attempt to supersede terminal task should return ErrConflict
			_, err = store.SupersedeTask(ctx, task.ID, nil)
			if !errors.Is(err, ErrConflict) {
				t.Errorf("expected ErrConflict for terminal state %s, got %v", terminalState, err)
			}

			// Verify dependent is unchanged
			depAfter, err := store.GetTask(ctx, dependent.ID)
			if err != nil {
				t.Fatalf("failed to get dependent: %v", err)
			}
			if len(depAfter.DependsOn) != 1 || depAfter.DependsOn[0] != task.ID {
				t.Errorf("expected dependent to still depend on original task, got %v", depAfter.DependsOn)
			}

			// Verify task state is unchanged
			taskAfter, err := store.GetTask(ctx, task.ID)
			if err != nil {
				t.Fatalf("failed to get task: %v", err)
			}
			if taskAfter.State != terminalState {
				t.Errorf("expected task state to remain %s, got %s", terminalState, taskAfter.State)
			}
		})
	}
}

func TestSupersededTaskNonTerminal(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create task and dependent in backlog state
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task", Spec: "Spec", DocumentID: doc.ID},
		{Title: "Dependent", Spec: "Spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}
	task := tasks[0]
	dependent := tasks[1]

	// Set dependent to depend on task
	_, err = store.UpdateTaskDependsOn(ctx, dependent.ID, []string{task.ID})
	if err != nil {
		t.Fatalf("failed to set dependency: %v", err)
	}

	// Supersede non-terminal task should succeed
	newTask, err := store.SupersedeTask(ctx, task.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	if newTask.ID == task.ID {
		t.Errorf("expected new task ID to be different from old task ID")
	}

	// Verify dependent now depends on new task
	depAfter, err := store.GetTask(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("failed to get dependent: %v", err)
	}
	if len(depAfter.DependsOn) != 1 || depAfter.DependsOn[0] != newTask.ID {
		t.Errorf("expected dependent to depend on new task, got %v", depAfter.DependsOn)
	}
}

func TestSupersededTaskWithPriorFeedback(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create task with original spec
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task", Spec: "Original spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	oldTask := tasks[0]

	// Add reject feedback events manually (simulating review feedback)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}

	// Add first reject feedback
	verdict1 := "reject"
	note1 := "Found critical bug in line 42"
	_, err = store.AppendEvent(ctx, tx, oldTask.ID, "reviewer1", "review", &verdict1, &note1)
	if err != nil {
		tx.Rollback()
		t.Fatalf("failed to append event: %v", err)
	}

	// Add second reject feedback
	verdict2 := "reject"
	note2 := "Tests are failing"
	_, err = store.AppendEvent(ctx, tx, oldTask.ID, "reviewer2", "review", &verdict2, &note2)
	if err != nil {
		tx.Rollback()
		t.Fatalf("failed to append event: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit tx: %v", err)
	}

	// Supersede the task
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	// Verify new task spec contains prior feedback
	if !strings.Contains(newTask.Spec, "## Prior attempt feedback") {
		t.Errorf("expected spec to contain feedback header, got: %q", newTask.Spec)
	}

	if !strings.Contains(newTask.Spec, "reviewer1") {
		t.Errorf("expected spec to contain reviewer1, got: %q", newTask.Spec)
	}

	if !strings.Contains(newTask.Spec, "reviewer2") {
		t.Errorf("expected spec to contain reviewer2, got: %q", newTask.Spec)
	}

	if !strings.Contains(newTask.Spec, "Found critical bug in line 42") {
		t.Errorf("expected spec to contain first feedback note, got: %q", newTask.Spec)
	}

	if !strings.Contains(newTask.Spec, "Tests are failing") {
		t.Errorf("expected spec to contain second feedback note, got: %q", newTask.Spec)
	}

	// Verify spec starts with original spec
	if !strings.HasPrefix(newTask.Spec, "Original spec") {
		t.Errorf("expected spec to start with original spec, got: %q", newTask.Spec)
	}
}

func TestSupersededTaskWithoutFeedback(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create task without feedback
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task", Spec: "Original spec unchanged", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	oldTask := tasks[0]

	// Supersede the task (without adding any feedback)
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	// Verify new task spec is unchanged (no feedback header added)
	if newTask.Spec != "Original spec unchanged" {
		t.Errorf("expected spec to be unchanged, got: %q", newTask.Spec)
	}

	if strings.Contains(newTask.Spec, "## Prior attempt feedback") {
		t.Errorf("expected spec to not contain feedback header when no feedback, got: %q", newTask.Spec)
	}
}

// completeTask transitions a task to done state.
func completeTask(ctx context.Context, store *sqliteStore, taskID string, t *testing.T) {
	// Use raw SQL to set task to approved state (shortcut for testing)
	_, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'approved' WHERE id = ?", taskID)
	if err != nil {
		t.Fatalf("failed to set task to approved: %v", err)
	}

	// Transition approved -> done
	_, err = store.TransitionTask(ctx, taskID, "done", nil)
	if err != nil {
		t.Fatalf("failed to transition to done: %v", err)
	}
}

// TestSupersedeDependentClaimability proves that superseding a task re-gates
// dependents so they are claimable only when the NEW task is done, not the old.
func TestSupersedeDependentClaimability(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create: oldTask <- dependent
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Old Task", Spec: "Spec", DocumentID: doc.ID},
		{Title: "Dependent Task", Spec: "Spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	oldTask := tasks[0]
	dependent := tasks[1]

	// Set dependent -> oldTask
	_, err = store.UpdateTaskDependsOn(ctx, dependent.ID, []string{oldTask.ID})
	if err != nil {
		t.Fatalf("failed to set dependency: %v", err)
	}

	// Promote dependent to ready (but it's not claimable yet because oldTask is not done)
	_, err = store.PromoteTask(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("failed to promote dependent: %v", err)
	}

	// Verify initial state: dependent is NOT claimable (oldTask is not done)
	claimableBefore, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list claimable tasks: %v", err)
	}
	for _, task := range claimableBefore {
		if task.ID == dependent.ID {
			t.Errorf("expected dependent to not be claimable before oldTask is done")
		}
	}

	// Supersede oldTask
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	// Verify dependent now depends on newTask (re-gated)
	dependentAfterSupersede, err := store.GetTask(ctx, dependent.ID)
	if err != nil {
		t.Fatalf("failed to get dependent after supersede: %v", err)
	}
	if len(dependentAfterSupersede.DependsOn) != 1 || dependentAfterSupersede.DependsOn[0] != newTask.ID {
		t.Errorf("expected dependent to depend on newTask, got %v", dependentAfterSupersede.DependsOn)
	}

	// Dependent should still NOT be claimable (newTask is not done yet)
	claimableAfterSupersede1, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list claimable tasks: %v", err)
	}
	for _, task := range claimableAfterSupersede1 {
		if task.ID == dependent.ID {
			t.Errorf("expected dependent to not be claimable when newTask is not done")
		}
	}

	// Transition newTask to done
	completeTask(ctx, store.(*sqliteStore), newTask.ID, t)

	// Now dependent SHOULD be claimable (newTask is done)
	claimableAfterNewTaskDone, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list claimable tasks: %v", err)
	}
	found := false
	for _, task := range claimableAfterNewTaskDone {
		if task.ID == dependent.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected dependent to be claimable after newTask is done")
	}
}

// TestSupersededTaskExcludedFromListTasks proves that superseded old tasks
// are excluded from active queries by default.
func TestSupersededTaskExcludedFromListTasks(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create oldTask
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Old Task", Spec: "Spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	oldTask := tasks[0]

	// Verify oldTask appears in ListTasks before superseding
	listBefore, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	found := false
	for _, task := range listBefore {
		if task.ID == oldTask.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected oldTask to appear in ListTasks before superseding")
	}

	// Supersede oldTask
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	// Verify oldTask is excluded from ListTasks by default
	listAfter, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	for _, task := range listAfter {
		if task.ID == oldTask.ID {
			t.Errorf("expected oldTask to be excluded from ListTasks after superseding")
		}
	}

	// Verify newTask appears in ListTasks
	found = false
	for _, task := range listAfter {
		if task.ID == newTask.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected newTask to appear in ListTasks after superseding")
	}

	// Verify oldTask can still be retrieved by GetTask directly
	oldTaskDirect, err := store.GetTask(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("failed to get oldTask directly: %v", err)
	}
	if oldTaskDirect.State != "superseded" {
		t.Errorf("expected oldTask state to be superseded, got %q", oldTaskDirect.State)
	}
}

// TestSupersededLineageTracingWithMultipleDependents proves that lineage
// (SupersededBy field) correctly traces to the replacement, and that
// re-gating works for multiple dependents.
func TestSupersededLineageTracingWithMultipleDependents(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create: oldTask <- dep1, oldTask <- dep2
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Old Task", Spec: "Spec", DocumentID: doc.ID},
		{Title: "Dependent 1", Spec: "Spec", DocumentID: doc.ID},
		{Title: "Dependent 2", Spec: "Spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	oldTask := tasks[0]
	dep1 := tasks[1]
	dep2 := tasks[2]

	// Set up dependencies
	_, err = store.UpdateTaskDependsOn(ctx, dep1.ID, []string{oldTask.ID})
	if err != nil {
		t.Fatalf("failed to set dep1 dependency: %v", err)
	}

	_, err = store.UpdateTaskDependsOn(ctx, dep2.ID, []string{oldTask.ID})
	if err != nil {
		t.Fatalf("failed to set dep2 dependency: %v", err)
	}

	// Promote dependents to ready (but not claimable yet because oldTask is not done)
	_, err = store.PromoteTask(ctx, dep1.ID)
	if err != nil {
		t.Fatalf("failed to promote dep1: %v", err)
	}

	_, err = store.PromoteTask(ctx, dep2.ID)
	if err != nil {
		t.Fatalf("failed to promote dep2: %v", err)
	}

	// Supersede oldTask
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	// Verify lineage: oldTask.SupersededBy == newTask.ID
	oldTaskAfter, err := store.GetTask(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("failed to get oldTask: %v", err)
	}
	if oldTaskAfter.SupersededBy == nil || *oldTaskAfter.SupersededBy != newTask.ID {
		t.Errorf("expected oldTask.SupersededBy to be %s, got %v", newTask.ID, oldTaskAfter.SupersededBy)
	}

	// Verify both dependents are re-gated to newTask
	dep1After, err := store.GetTask(ctx, dep1.ID)
	if err != nil {
		t.Fatalf("failed to get dep1: %v", err)
	}
	if len(dep1After.DependsOn) != 1 || dep1After.DependsOn[0] != newTask.ID {
		t.Errorf("expected dep1 to depend on newTask, got %v", dep1After.DependsOn)
	}

	dep2After, err := store.GetTask(ctx, dep2.ID)
	if err != nil {
		t.Fatalf("failed to get dep2: %v", err)
	}
	if len(dep2After.DependsOn) != 1 || dep2After.DependsOn[0] != newTask.ID {
		t.Errorf("expected dep2 to depend on newTask, got %v", dep2After.DependsOn)
	}

	// Verify claimability for both dependents only when newTask is done
	claimableBefore, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list claimable tasks: %v", err)
	}
	for _, task := range claimableBefore {
		if task.ID == dep1.ID || task.ID == dep2.ID {
			t.Errorf("expected dependents to not be claimable when newTask is not done")
		}
	}

	// Transition newTask to done
	completeTask(ctx, store.(*sqliteStore), newTask.ID, t)

	// Now both dependents should be claimable
	claimableAfter, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Claimable: true})
	if err != nil {
		t.Fatalf("failed to list claimable tasks: %v", err)
	}
	dep1Found := false
	dep2Found := false
	for _, task := range claimableAfter {
		if task.ID == dep1.ID {
			dep1Found = true
		}
		if task.ID == dep2.ID {
			dep2Found = true
		}
	}
	if !dep1Found {
		t.Errorf("expected dep1 to be claimable after newTask is done")
	}
	if !dep2Found {
		t.Errorf("expected dep2 to be claimable after newTask is done")
	}
}

// TestTaskEscalateFlag verifies that the escalate flag defaults to true and can be set to false.
func TestTaskEscalateFlag(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Test 1: Default escalate=true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 1", Spec: "Spec 1", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task 1: %v", err)
	}

	task1, err := store.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatalf("failed to get task 1: %v", err)
	}
	if !task1.Escalate {
		t.Errorf("expected escalate=true by default, got %v", task1.Escalate)
	}

	// Test 2: Create with escalate=false
	escalateFalse := false
	tasks2, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 2", Spec: "Spec 2", DocumentID: doc.ID, Escalate: &escalateFalse},
	})
	if err != nil {
		t.Fatalf("failed to create task 2: %v", err)
	}

	task2, err := store.GetTask(ctx, tasks2[0].ID)
	if err != nil {
		t.Fatalf("failed to get task 2: %v", err)
	}
	if task2.Escalate {
		t.Errorf("expected escalate=false, got %v", task2.Escalate)
	}

	// Test 3: Create with escalate=true (explicit)
	escalateTrue := true
	tasks3, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 3", Spec: "Spec 3", DocumentID: doc.ID, Escalate: &escalateTrue},
	})
	if err != nil {
		t.Fatalf("failed to create task 3: %v", err)
	}

	task3, err := store.GetTask(ctx, tasks3[0].ID)
	if err != nil {
		t.Fatalf("failed to get task 3: %v", err)
	}
	if !task3.Escalate {
		t.Errorf("expected escalate=true (explicit), got %v", task3.Escalate)
	}

	// Test 4: ListTasks includes escalate field
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	found := map[string]bool{}
	for _, task := range allTasks {
		if task.ID == task1.ID {
			found["task1"] = task.Escalate
		}
		if task.ID == task2.ID {
			found["task2"] = !task.Escalate
		}
		if task.ID == task3.ID {
			found["task3"] = task.Escalate
		}
	}

	if !found["task1"] {
		t.Errorf("task1 escalate should be true")
	}
	if !found["task2"] {
		t.Errorf("task2 escalate should be false")
	}
	if !found["task3"] {
		t.Errorf("task3 escalate should be true")
	}
}

// TestEscalateRoundTrip verifies that the escalate flag is preserved through all task operations.
func TestEscalateRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create an implement task with escalate=false
	escalateFalse := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Implement Task", Spec: "Implementation spec", DocumentID: doc.ID, Model: "haiku", Escalate: &escalateFalse},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready so it can be claimed
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	// Test ClaimTask preserves escalate
	claimedTask, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if claimedTask.Escalate {
		t.Errorf("claimed task should have escalate=false, got %v", claimedTask.Escalate)
	}

	// Test HeartbeatTask preserves escalate
	heartbeatTask, err := store.HeartbeatTask(ctx, taskID, "agent-1", time.Minute)
	if err != nil {
		t.Fatalf("failed to heartbeat task: %v", err)
	}
	if heartbeatTask.Escalate {
		t.Errorf("heartbeat task should have escalate=false, got %v", heartbeatTask.Escalate)
	}

	// Test SubmitTask preserves escalate
	submittedTask, err := store.SubmitTask(ctx, taskID, "agent-1", "implementation result", nil, []LinkInput{{Kind: "pr", Value: "https://github.com/test/test/pull/1"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}
	if submittedTask.Escalate {
		t.Errorf("submitted task should have escalate=false, got %v", submittedTask.Escalate)
	}

	// Create a ready task with escalate=false for other operations
	escalateFalse2 := false
	tasks2, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Ready Task", Spec: "Ready spec", DocumentID: doc.ID, Model: "haiku", Escalate: &escalateFalse2},
	})
	if err != nil {
		t.Fatalf("failed to create ready task: %v", err)
	}
	readyTaskID := tasks2[0].ID

	// Test HoldTask preserves escalate
	heldTask, err := store.HoldTask(ctx, readyTaskID)
	if err != nil {
		t.Fatalf("failed to hold task: %v", err)
	}
	if heldTask.Escalate {
		t.Errorf("held task should have escalate=false, got %v", heldTask.Escalate)
	}

	// Test ReleaseTask preserves escalate
	releasedTask, err := store.ReleaseTask(ctx, readyTaskID, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to release task: %v", err)
	}
	if releasedTask.Escalate {
		t.Errorf("released task should have escalate=false, got %v", releasedTask.Escalate)
	}

	// Create two tasks for dependency update: one with escalate=false, one with escalate=true
	escalateFalse3 := false
	tasks3, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task with escalate false", Spec: "Spec", DocumentID: doc.ID, Model: "haiku", Escalate: &escalateFalse3},
	})
	if err != nil {
		t.Fatalf("failed to create task 3: %v", err)
	}
	task3ID := tasks3[0].ID

	escalateTrue := true
	tasks4, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task with escalate true", Spec: "Spec", DocumentID: doc.ID, Model: "haiku", Escalate: &escalateTrue},
	})
	if err != nil {
		t.Fatalf("failed to create task 4: %v", err)
	}
	task4ID := tasks4[0].ID

	// Test UpdateTaskDependsOn preserves escalate
	updatedTask, err := store.UpdateTaskDependsOn(ctx, task3ID, []string{task4ID})
	if err != nil {
		t.Fatalf("failed to update task dependencies: %v", err)
	}
	if updatedTask.Escalate {
		t.Errorf("updated task should have escalate=false, got %v", updatedTask.Escalate)
	}

	// Test SupersedeTask preserves escalate
	// First promote task4, then claim and submit it to put it in review
	_, err = store.PromoteTask(ctx, task4ID)
	if err != nil {
		t.Fatalf("failed to promote task 4: %v", err)
	}

	_, err = store.ClaimTask(ctx, task4ID, "agent-2", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task 4: %v", err)
	}
	_, err = store.SubmitTask(ctx, task4ID, "agent-2", "result", nil, []LinkInput{{Kind: "pr", Value: "https://github.com/test/test/pull/2"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task 4: %v", err)
	}

	// Now reject it to put it back in ready, then supersede it
	_, err = store.AddReview(ctx, task4ID, "reviewer", "reject", nil)
	if err != nil {
		t.Fatalf("failed to add review: %v", err)
	}

	supersededTask, err := store.SupersedeTask(ctx, task4ID, nil)
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}
	if !supersededTask.Escalate {
		t.Errorf("superseded task should have escalate=true (original), got %v", supersededTask.Escalate)
	}

	// Verify the replacement task preserves escalate
	// We'll just verify that the escalate is preserved by checking GetTask on task4
	getTask4, err := store.GetTask(ctx, task4ID)
	if err != nil {
		t.Fatalf("failed to get task 4: %v", err)
	}
	if !getTask4.Escalate {
		t.Errorf("original task 4 should still have escalate=true, got %v", getTask4.Escalate)
	}

	// Create a task with escalate=false and verify it through supersede
	escalateFalse4 := false
	tasks5, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task to supersede", Spec: "Spec", DocumentID: doc.ID, Model: "haiku", Escalate: &escalateFalse4},
	})
	if err != nil {
		t.Fatalf("failed to create task 5: %v", err)
	}
	task5ID := tasks5[0].ID

	// Promote to ready
	_, err = store.PromoteTask(ctx, task5ID)
	if err != nil {
		t.Fatalf("failed to promote task 5: %v", err)
	}

	// Claim, submit, reject, then supersede
	claimedTask5, err := store.ClaimTask(ctx, task5ID, "agent-3", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task 5: %v", err)
	}
	if claimedTask5.Escalate {
		t.Errorf("claimed task 5 should have escalate=false, got %v", claimedTask5.Escalate)
	}

	_, err = store.SubmitTask(ctx, task5ID, "agent-3", "result", nil, []LinkInput{{Kind: "pr", Value: "https://github.com/test/test/pull/3"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task 5: %v", err)
	}

	_, err = store.AddReview(ctx, task5ID, "reviewer", "reject", nil)
	if err != nil {
		t.Fatalf("failed to add review to task 5: %v", err)
	}

	newTask, err := store.SupersedeTask(ctx, task5ID, nil)
	if err != nil {
		t.Fatalf("failed to supersede task 5: %v", err)
	}
	if newTask.Escalate {
		t.Errorf("new task from supersede should have escalate=false, got %v", newTask.Escalate)
	}
}

func TestPruneEventsRemovesTerminalTaskEvents(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a terminal task (done state)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Terminal Task", Spec: "Spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	terminalTaskID := tasks[0].ID

	// Promote, claim, submit to get events
	_, err = store.PromoteTask(ctx, terminalTaskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, terminalTaskID, "agent-1", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Verify the terminal task has events
	events, err := store.ListEvents(ctx, terminalTaskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) == 0 {
		t.Errorf("terminal task should have events before pruning, got %d", len(events))
	}
	initialEventCount := len(events)

	// Manually set task to done state for testing
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = 'done' WHERE id = ?", terminalTaskID)
	if err != nil {
		t.Fatalf("failed to update task state: %v", err)
	}

	// Run prune with 0-day retention for terminal tasks (should delete all terminal task events)
	pruned, err := store.PruneEvents(ctx, 0)
	if err != nil {
		t.Fatalf("failed to prune events: %v", err)
	}

	if pruned == 0 {
		t.Errorf("PruneEvents should have removed at least %d terminal task events, but removed %d", initialEventCount, pruned)
	}

	// Verify events are actually gone
	afterEvents, err := store.ListEvents(ctx, terminalTaskID)
	if err != nil {
		t.Fatalf("failed to list events after prune: %v", err)
	}
	if len(afterEvents) > 0 {
		t.Errorf("terminal task should have no events after pruning with 0-day retention, got %d", len(afterEvents))
	}
}

func TestPruneEventsKeepsActiveTaskEvents(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create an active task (in_progress state)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Active Task", Spec: "Spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	activeTaskID := tasks[0].ID

	// Promote to ready
	_, err = store.PromoteTask(ctx, activeTaskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	// Claim task to make it in_progress (generates claim event)
	_, err = store.ClaimTask(ctx, activeTaskID, "agent-1", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Verify the active task has events
	events, err := store.ListEvents(ctx, activeTaskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) == 0 {
		t.Errorf("active task should have events after claim, got %d", len(events))
	}
	initialEventCount := len(events)

	// Run prune with 0-day retention (should NOT delete active task events)
	_, err = store.PruneEvents(ctx, 0)
	if err != nil {
		t.Fatalf("failed to prune events: %v", err)
	}

	// Verify events are NOT deleted for active tasks
	afterEvents, err := store.ListEvents(ctx, activeTaskID)
	if err != nil {
		t.Fatalf("failed to list events after prune: %v", err)
	}
	if len(afterEvents) != initialEventCount {
		t.Errorf("active task events should be preserved during pruning, had %d before, got %d after", initialEventCount, len(afterEvents))
	}
}

func TestPruneEventsPreservesRecentTerminalTaskEvents(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a terminal task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Terminal Task", Spec: "Spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	terminalTaskID := tasks[0].ID

	// Promote and claim to generate events
	_, err = store.PromoteTask(ctx, terminalTaskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, terminalTaskID, "agent-1", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Mark as done
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = 'done' WHERE id = ?", terminalTaskID)
	if err != nil {
		t.Fatalf("failed to update task state: %v", err)
	}

	// Verify events exist
	events, err := store.ListEvents(ctx, terminalTaskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) == 0 {
		t.Errorf("terminal task should have events, got %d", len(events))
	}
	initialEventCount := len(events)

	// Run prune with high retention (7 days) - recent events should be kept
	_, err = store.PruneEvents(ctx, 7)
	if err != nil {
		t.Fatalf("failed to prune events: %v", err)
	}

	// Verify events are preserved (pruned == 0 since events are recent)
	afterEvents, err := store.ListEvents(ctx, terminalTaskID)
	if err != nil {
		t.Fatalf("failed to list events after prune: %v", err)
	}
	if len(afterEvents) != initialEventCount {
		t.Errorf("recent terminal task events should be preserved, had %d before, got %d after", initialEventCount, len(afterEvents))
	}
}

func TestPruneEventsListEventsStillReturnsKeptEvents(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create two tasks: one active, one terminal
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Active Task", Spec: "Spec", DocumentID: doc.ID, Model: "haiku"},
		{Title: "Terminal Task", Spec: "Spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}
	activeTaskID := tasks[0].ID
	terminalTaskID := tasks[1].ID

	// Set up both tasks with events
	for _, taskID := range []string{activeTaskID, terminalTaskID} {
		_, err = store.PromoteTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to promote task: %v", err)
		}

		_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", time.Minute)
		if err != nil {
			t.Fatalf("failed to claim task: %v", err)
		}
	}

	// Mark terminal task as done
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = 'done' WHERE id = ?", terminalTaskID)
	if err != nil {
		t.Fatalf("failed to update task state: %v", err)
	}

	// Get event counts before prune
	activeEventsBefore, err := store.ListEvents(ctx, activeTaskID)
	if err != nil {
		t.Fatalf("failed to list active task events: %v", err)
	}

	// Run prune
	_, err = store.PruneEvents(ctx, 0)
	if err != nil {
		t.Fatalf("failed to prune events: %v", err)
	}

	// Verify ListEvents still returns kept events
	activeEventsAfter, err := store.ListEvents(ctx, activeTaskID)
	if err != nil {
		t.Fatalf("failed to list active task events after prune: %v", err)
	}
	if len(activeEventsAfter) != len(activeEventsBefore) {
		t.Errorf("active task events not preserved in ListEvents: had %d before, got %d after", len(activeEventsBefore), len(activeEventsAfter))
	}

	terminalEventsAfter, err := store.ListEvents(ctx, terminalTaskID)
	if err != nil {
		t.Fatalf("failed to list terminal task events after prune: %v", err)
	}
	if len(terminalEventsAfter) > 0 {
		t.Errorf("terminal task events should be pruned, but ListEvents returned %d", len(terminalEventsAfter))
	}
}

func TestPruneEventsPreservesResearchTrackTaskEvents(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a research task and a build task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Research Task",
			Spec:       "Spec",
			DocumentID: doc.ID,
			Model:      "haiku",
			Track:      "research",
		},
		{
			Title:      "Build Task",
			Spec:       "Spec",
			DocumentID: doc.ID,
			Model:      "haiku",
		},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}
	researchTaskID := tasks[0].ID
	buildTaskID := tasks[1].ID

	// Set up both tasks with events
	for _, taskID := range []string{researchTaskID, buildTaskID} {
		_, err = store.PromoteTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to promote task: %v", err)
		}

		_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", time.Minute)
		if err != nil {
			t.Fatalf("failed to claim task: %v", err)
		}
	}

	// Mark both tasks as done
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = 'done' WHERE id IN (?, ?)", researchTaskID, buildTaskID)
	if err != nil {
		t.Fatalf("failed to update task states: %v", err)
	}

	// Get event counts before prune
	researchEventsBefore, err := store.ListEvents(ctx, researchTaskID)
	if err != nil {
		t.Fatalf("failed to list research task events: %v", err)
	}
	if len(researchEventsBefore) == 0 {
		t.Errorf("research task should have events before pruning")
	}

	buildEventsBefore, err := store.ListEvents(ctx, buildTaskID)
	if err != nil {
		t.Fatalf("failed to list build task events: %v", err)
	}
	if len(buildEventsBefore) == 0 {
		t.Errorf("build task should have events before pruning")
	}

	// Run prune with 0-day retention for terminal tasks
	pruned, err := store.PruneEvents(ctx, 0)
	if err != nil {
		t.Fatalf("failed to prune events: %v", err)
	}

	if pruned == 0 {
		t.Errorf("PruneEvents should have removed build task events")
	}

	// Verify research task events are preserved
	researchEventsAfter, err := store.ListEvents(ctx, researchTaskID)
	if err != nil {
		t.Fatalf("failed to list research task events after prune: %v", err)
	}
	if len(researchEventsAfter) != len(researchEventsBefore) {
		t.Errorf("research task events should be preserved: had %d before, got %d after", len(researchEventsBefore), len(researchEventsAfter))
	}

	// Verify build task events are deleted
	buildEventsAfter, err := store.ListEvents(ctx, buildTaskID)
	if err != nil {
		t.Fatalf("failed to list build task events after prune: %v", err)
	}
	if len(buildEventsAfter) > 0 {
		t.Errorf("build task events should be pruned, but got %d events", len(buildEventsAfter))
	}
}

func TestPruneEventsPreservesScorecardData(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a research task with a reviewer model
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Research Task",
			Spec:         "Verify claims",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Track:        "research",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Submit the task
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Find the opus review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var opusReviewTask *Task
	for i := range allTasks {
		tk := allTasks[i]
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == taskID && tk.State == "ready" {
			opusReviewTask = &tk
			break
		}
	}

	if opusReviewTask == nil {
		t.Fatalf("expected opus review task")
	}

	// Submit a review with findings
	findingsJSON := json.RawMessage(`[{"id":"f1","severity":"P1","file":"test.txt","line":1,"summary":"test finding","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opusReviewTask, "opus-reviewer", "reject", findingsJSON)

	// Mark the research task as done
	_, err = store.Conn().ExecContext(ctx, "UPDATE task SET state = 'done' WHERE id = ?", taskID)
	if err != nil {
		t.Fatalf("failed to update task state: %v", err)
	}

	// Get scorecard before prune
	scorecardBefore, err := store.GetResearchReviewerScorecards(ctx, proj.ID)
	if err != nil {
		t.Fatalf("failed to get scorecard before prune: %v", err)
	}

	// Run prune with 0-day retention
	_, err = store.PruneEvents(ctx, 0)
	if err != nil {
		t.Fatalf("failed to prune events: %v", err)
	}

	// Get scorecard after prune
	scorecardAfter, err := store.GetResearchReviewerScorecards(ctx, proj.ID)
	if err != nil {
		t.Fatalf("failed to get scorecard after prune: %v", err)
	}

	// Verify scorecards are identical
	if len(scorecardBefore.Scorecards) != len(scorecardAfter.Scorecards) {
		t.Errorf("scorecard length changed: had %d before, got %d after", len(scorecardBefore.Scorecards), len(scorecardAfter.Scorecards))
	}

	if len(scorecardBefore.Scorecards) > 0 {
		sc := scorecardAfter.Scorecards[0]
		scBefore := scorecardBefore.Scorecards[0]

		if sc.Model != scBefore.Model {
			t.Errorf("model changed: was %s, now %s", scBefore.Model, sc.Model)
		}

		// Compare FindingsRaised maps
		if len(sc.FindingsRaised) != len(scBefore.FindingsRaised) {
			t.Errorf("findings_raised length changed: had %d before, got %d after", len(scBefore.FindingsRaised), len(sc.FindingsRaised))
		} else {
			for k, v := range scBefore.FindingsRaised {
				if sc.FindingsRaised[k] != v {
					t.Errorf("findings_raised[%s] changed: was %d, now %d", k, v, sc.FindingsRaised[k])
				}
			}
		}

		if sc.FindingsHeld != scBefore.FindingsHeld {
			t.Errorf("findings_held changed: was %d, now %d", scBefore.FindingsHeld, sc.FindingsHeld)
		}
		if sc.FindingsUnresolved != scBefore.FindingsUnresolved {
			t.Errorf("findings_unresolved changed: was %d, now %d", scBefore.FindingsUnresolved, sc.FindingsUnresolved)
		}
		if sc.TotalReviewRounds != scBefore.TotalReviewRounds {
			t.Errorf("total_review_rounds changed: was %d, now %d", scBefore.TotalReviewRounds, sc.TotalReviewRounds)
		}
		if sc.SampleSize != scBefore.SampleSize {
			t.Errorf("sample_size changed: was %d, now %d", scBefore.SampleSize, sc.SampleSize)
		}
	}
}

func TestHeartbeatTaskDoesNotCreateEvent(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and document
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote to ready
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	// Claim task
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Get event count after claim
	eventsBefore, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events before heartbeat: %v", err)
	}
	countBefore := len(eventsBefore)

	// Heartbeat the task
	_, err = store.HeartbeatTask(ctx, taskID, "agent-1", time.Minute)
	if err != nil {
		t.Fatalf("failed to heartbeat task: %v", err)
	}

	// Get event count after heartbeat
	eventsAfter, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events after heartbeat: %v", err)
	}
	countAfter := len(eventsAfter)

	// Verify heartbeat did NOT create an event
	if countAfter != countBefore {
		t.Errorf("heartbeat should not create an event, had %d events before, got %d after", countBefore, countAfter)
	}

	// Verify we have at least the claim event
	if countAfter < 1 {
		t.Errorf("expected at least 1 event (claim), got %d", countAfter)
	}
}

// TestAgentMergeNoOpAutoFinalizesToDone verifies that an approved agent_merge=true no-op
// task automatically transitions to "done" instead of "approved".
func TestAgentMergeNoOpAutoFinalizesToDone(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project and doc
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task with agent_merge=true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			AgentMerge:   true,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit as no-op
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit as no-op (no PR link, only no_op marker)
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "No changes needed", nil, []LinkInput{{Kind: "no_op", Value: "acceptance already satisfied"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task as no-op: %v", err)
	}

	// Verify review task was created
	if submitted.ReviewRound != 1 {
		t.Errorf("expected review_round=1, got %d", submitted.ReviewRound)
	}

	// Find the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Claim and submit review with approve
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task: %v", err)
	}

	// Verify parent task went to "done" (not "approved") because it's agent_merge=true no-op
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "done" {
		t.Errorf("expected parent task state='done' for agent_merge no-op, got '%s'", parentTask.State)
	}

	// Verify transition event was created with appropriate message (find the last transition event)
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var transitionEvent *Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "transition" {
			transitionEvent = &events[i]
			break
		}
	}
	if transitionEvent == nil {
		t.Fatalf("transition event not found on parent task")
	}
	if transitionEvent.Note == nil {
		t.Fatalf("transition event note is nil")
	}
	if !strings.Contains(*transitionEvent.Note, "auto-finalized") {
		t.Errorf("expected transition event note to mention 'auto-finalized', got %q", *transitionEvent.Note)
	}
}

// TestAgentMergePRStaysApproved verifies that an approved agent_merge=true PR task
// stays in "approved" state (does not auto-finalize to "done").
func TestAgentMergePRStaysApproved(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project and doc
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task with agent_merge=true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			AgentMerge:   true,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit with PR link
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit with PR link (not a no-op)
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Verify review task was created
	if submitted.ReviewRound != 1 {
		t.Errorf("expected review_round=1, got %d", submitted.ReviewRound)
	}

	// Find the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Claim and submit review with approve
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task: %v", err)
	}

	// Verify parent task stayed in "approved" (not "done") because it has a PR link
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "approved" {
		t.Errorf("expected parent task state='approved' for agent_merge PR, got '%s'", parentTask.State)
	}

	// Verify transition event was created with "Aggregation" message (not "auto-finalized") (find the last transition event)
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var transitionEvent *Event
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == "transition" {
			transitionEvent = &events[i]
			break
		}
	}
	if transitionEvent == nil {
		t.Fatalf("transition event not found on parent task")
	}
	if transitionEvent.Note == nil {
		t.Fatalf("transition event note is nil")
	}
	if !strings.Contains(*transitionEvent.Note, "Aggregation") {
		t.Errorf("expected transition event note to mention 'Aggregation', got %q", *transitionEvent.Note)
	}
}

// TestCreateTasksWithTrack verifies that track field is persisted and defaults to 'build'.
func TestCreateTasksWithTrack(t *testing.T) {
	ctx := context.Background()

	store, err := Open("file::memory:?cache=shared", []string{"haiku", "opus", "sonnet"})
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-proj", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Doc", "docs/test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Test 1: Create task with track=design
	tasksWithTrack, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Design Task", Spec: "Design spec", DocumentID: doc.ID, Track: "design"},
	})
	if err != nil {
		t.Fatalf("failed to create task with track: %v", err)
	}

	if len(tasksWithTrack) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasksWithTrack))
	}

	if tasksWithTrack[0].Track != "design" {
		t.Errorf("expected track='design', got '%s'", tasksWithTrack[0].Track)
	}

	// Verify track persists when retrieved
	retrieved, err := store.GetTask(ctx, tasksWithTrack[0].ID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if retrieved.Track != "design" {
		t.Errorf("expected track='design' after retrieval, got '%s'", retrieved.Track)
	}

	// Test 2: Create task without track (should default to 'build')
	tasksDefault, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Build Task", Spec: "Build spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task without track: %v", err)
	}

	if len(tasksDefault) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasksDefault))
	}

	if tasksDefault[0].Track != "build" {
		t.Errorf("expected track='build' (default), got '%s'", tasksDefault[0].Track)
	}

	// Verify default persists when retrieved
	retrievedDefault, err := store.GetTask(ctx, tasksDefault[0].ID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if retrievedDefault.Track != "build" {
		t.Errorf("expected track='build' (default) after retrieval, got '%s'", retrievedDefault.Track)
	}

	// Test 3: Create task with track=research
	tasksResearch, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Research Task", Spec: "Research spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create task with research track: %v", err)
	}

	if len(tasksResearch) != 1 {
		t.Fatalf("expected 1 task, got %d", len(tasksResearch))
	}

	if tasksResearch[0].Track != "research" {
		t.Errorf("expected track='research', got '%s'", tasksResearch[0].Track)
	}

	// Verify research track persists when retrieved
	retrievedResearch, err := store.GetTask(ctx, tasksResearch[0].ID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if retrievedResearch.Track != "research" {
		t.Errorf("expected track='research' after retrieval, got '%s'", retrievedResearch.Track)
	}
}

// TestCreateTasksWithUnknownTrack verifies that track field is validated.
// Tracks not in {build, design, research} are rejected with UNKNOWN_TRACK.
func TestCreateTasksWithUnknownTrack(t *testing.T) {
	ctx := context.Background()

	store, err := Open("file::memory:?cache=shared", []string{"haiku", "opus", "sonnet"})
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "test-proj", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Doc", "docs/test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Should fail with UNKNOWN_TRACK for invalid track value
	_, err = store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Bad Track Task", Spec: "Bad spec", DocumentID: doc.ID, Track: "testing"},
	})
	if err == nil {
		t.Error("expected UNKNOWN_TRACK error for testing track, but creation succeeded")
	}
	var valErr *ValidationError
	if !errors.As(err, &valErr) {
		t.Errorf("expected ValidationError, got %T", err)
	} else if valErr.Code != "UNKNOWN_TRACK" {
		t.Errorf("expected error code UNKNOWN_TRACK, got %s", valErr.Code)
	}

	// Verify that build, design, and research are accepted
	for _, track := range []string{"build", "design", "research"} {
		tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{Title: "Track Task " + track, Spec: "Spec", DocumentID: doc.ID, Track: track},
		})
		if err != nil {
			t.Errorf("failed to create task with track=%s: %v", track, err)
		}
		if len(tasks) != 1 {
			t.Fatalf("expected 1 task with track=%s, got %d", track, len(tasks))
		}
		if tasks[0].Track != track {
			t.Errorf("expected track=%s, got %s", track, tasks[0].Track)
		}
	}
}

// TestAgentMergePRSpawnsMergeTask verifies that when a task with agent_merge=true
// and a PR link transitions to "approved", exactly one merge task is spawned.
func TestAgentMergePRSpawnsMergeTask(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project and doc
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task with agent_merge=true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			AgentMerge:   true,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit with PR link
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find the review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	// Count merge tasks before approval
	var mergeTasksBefore int
	for i := range allTasks {
		if allTasks[i].Kind == "merge" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			mergeTasksBefore++
		}
	}
	if mergeTasksBefore != 0 {
		t.Errorf("expected no merge tasks before approval, found %d", mergeTasksBefore)
	}

	// Claim and submit review with approve
	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task: %v", err)
	}

	// Verify parent task is in "approved" state
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "approved" {
		t.Errorf("expected parent task state='approved', got '%s'", parentTask.State)
	}

	// List tasks and find merge tasks
	allTasks, err = store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var mergeTasks []*Task
	for i := range allTasks {
		if allTasks[i].Kind == "merge" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			mergeTasks = append(mergeTasks, &allTasks[i])
		}
	}

	// Verify exactly one merge task was spawned
	if len(mergeTasks) != 1 {
		t.Errorf("expected exactly 1 merge task, found %d", len(mergeTasks))
	} else {
		mergeTask := mergeTasks[0]

		// Verify merge task properties
		if mergeTask.State != "ready" {
			t.Errorf("expected merge task state='ready', got '%s'", mergeTask.State)
		}
		if mergeTask.Model != "haiku" {
			t.Errorf("expected merge task model='haiku', got '%s'", mergeTask.Model)
		}
		if !strings.HasPrefix(mergeTask.Title, "Merge: ") {
			t.Errorf("expected merge task title to start with 'Merge: ', got '%s'", mergeTask.Title)
		}
		if mergeTask.TargetTaskID == nil || *mergeTask.TargetTaskID != taskID {
			t.Errorf("expected merge task target_task_id='%s', got %v", taskID, mergeTask.TargetTaskID)
		}
	}
}

// TestAgentMergeDisabledNoMergeTask verifies that no merge task is spawned
// when agent_merge=false, even when all reviewers approve.
func TestAgentMergeDisabledNoMergeTask(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project and doc
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task with agent_merge=false (default)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			AgentMerge:   false,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit with PR link
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find and claim review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	// Submit review with approve
	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task: %v", err)
	}

	// List tasks and verify no merge task was spawned
	allTasks, err = store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var mergeTaskCount int
	for i := range allTasks {
		if allTasks[i].Kind == "merge" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			mergeTaskCount++
		}
	}

	if mergeTaskCount != 0 {
		t.Errorf("expected 0 merge tasks for agent_merge=false, found %d", mergeTaskCount)
	}
}

// TestAgentMergeNoOpNoMergeTask verifies that no merge task is spawned
// for no-op submissions (no PR link), even when agent_merge=true.
func TestAgentMergeNoOpNoMergeTask(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create project and doc
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create implement task with agent_merge=true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			AgentMerge:   true,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote, claim, and submit with no-op (no PR link)
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Already satisfied on main", nil, []LinkInput{{Kind: "no_op", Value: "commit-hash"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	// Find and claim review task
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var reviewTask *Task
	for i := range allTasks {
		if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			reviewTask = &allTasks[i]
			break
		}
	}
	if reviewTask == nil {
		t.Fatalf("review task not found")
	}

	_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	// Submit review with approve
	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Verified", &approve, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review task: %v", err)
	}

	// Verify parent task went to "done" (auto-finalized for no-op)
	parentTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent task: %v", err)
	}
	if parentTask.State != "done" {
		t.Errorf("expected parent task state='done' (auto-finalized for no-op), got '%s'", parentTask.State)
	}

	// List tasks and verify no merge task was spawned
	allTasks, err = store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	var mergeTaskCount int
	for i := range allTasks {
		if allTasks[i].Kind == "merge" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID {
			mergeTaskCount++
		}
	}

	if mergeTaskCount != 0 {
		t.Errorf("expected 0 merge tasks for no-op submission, found %d", mergeTaskCount)
	}
}

// TestClaimReclaimsExpiredInProgressTask pins the lease-reclaim fix: an in_progress
// task whose lease has lapsed (stalled/dead worker) is reclaimable by another worker;
// one with a live lease is not.
func TestClaimReclaimsExpiredInProgressTask(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	now := nowTimestamp()
	past := leaseExpiryTimestamp(-time.Hour)  // now - 1h -> expired lease (store's format/UTC)
	future := leaseExpiryTimestamp(time.Hour) // now + 1h -> live lease
	conn := store.Conn()
	if _, err := conn.ExecContext(ctx, `INSERT INTO project (id, name, repo, created_at) VALUES (?, ?, ?, ?)`,
		"p1", "proj", "https://github.com/example/repo", now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"d1", "p1", "design", "D", "DESIGN.md", now, now); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	insertInProgress := func(id, lease string) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO task (id, project_id, document_id, title, spec, state, kind, model, assignee, lease_expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'in_progress', 'implement', 'haiku', 'old-worker', ?, ?, ?)`,
			id, "p1", "d1", "T", "s", lease, now, now); err != nil {
			t.Fatalf("insert task %s: %v", id, err)
		}
	}

	// EXPIRED lease -> reclaimable by a new worker (and reassigned).
	insertInProgress("expired", past)
	tk, err := store.ClaimTask(ctx, "expired", "new-worker", "haiku", time.Minute)
	if err != nil {
		t.Fatalf("expired-lease in_progress task should be reclaimable, got: %v", err)
	}
	if tk.Assignee == nil || *tk.Assignee != "new-worker" {
		t.Errorf("expected reassignment to new-worker, got %v", tk.Assignee)
	}

	// LIVE lease -> NOT claimable.
	insertInProgress("live", future)
	if _, err := store.ClaimTask(ctx, "live", "new-worker", "haiku", time.Minute); err == nil {
		t.Error("live-lease in_progress task should NOT be claimable, but the claim succeeded")
	}
}

// TestTransitionMergeTaskInProgressToDone pins the merge-transition fix: a merge task's
// lifecycle is ready->in_progress->done (no review), so in_progress->done must be
// allowed for kind=merge -- but still rejected for ordinary kinds.
func TestTransitionMergeTaskInProgressToDone(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	now := nowTimestamp()
	conn := store.Conn()
	if _, err := conn.ExecContext(ctx, `INSERT INTO project (id, name, repo, created_at) VALUES (?, ?, ?, ?)`,
		"pm", "proj", "https://github.com/example/repo", now); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO document (id, project_id, kind, title, ref, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"dm", "pm", "design", "D", "DESIGN.md", now, now); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	insertTask := func(id, kind string) {
		if _, err := conn.ExecContext(ctx, `INSERT INTO task (id, project_id, document_id, title, spec, state, kind, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 'in_progress', ?, ?, ?)`,
			id, "pm", "dm", "T", "s", kind, now, now); err != nil {
			t.Fatalf("insert task %s: %v", id, err)
		}
	}

	// merge task: in_progress -> done is ALLOWED (the fix).
	insertTask("mt", "merge")
	if _, err := store.TransitionTask(ctx, "mt", "done", nil); err != nil {
		t.Fatalf("merge task in_progress->done should be allowed, got: %v", err)
	}

	// ordinary task: in_progress -> done is still REJECTED.
	insertTask("it", "implement")
	if _, err := store.TransitionTask(ctx, "it", "done", nil); err == nil {
		t.Error("non-merge in_progress->done should be rejected, but it was allowed")
	}
}

// prCloseCalls records which GitHub API calls a fake forge server observed, so tests
// can assert that closing a superseded task's pull request was (or wasn't) invoked
// rather than just that SupersedeTask returned no error.
type prCloseCalls struct {
	mu            sync.Mutex
	getStateCount int
	comments      []string
	closeCount    int
	deletedBranch string
}

// newSupersedePRTestServer starts a fake GitHub API server that reports the given PR
// state for GET /pulls/{n} and records comment/close/branch-delete calls. It fails the
// test on any request it doesn't recognize.
func newSupersedePRTestServer(t *testing.T, prState string) (*httptest.Server, *prCloseCalls) {
	t.Helper()
	calls := &prCloseCalls{}

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
			case "error":
				w.WriteHeader(http.StatusInternalServerError)
				fmt.Fprint(w, `{"message": "internal error"}`)
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

// setForgeToken points FORGE_TOKENS at a temp file so forge.OwnerToken resolves a
// token for owner without touching the real ~/.odonian/forge-tokens.
func setForgeToken(t *testing.T, owner, token string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "forge-tokens")
	content := fmt.Sprintf("%s=%s\n", owner, token)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write forge tokens file: %v", err)
	}
	t.Setenv("FORGE_TOKENS", path)
}

// createSupersedableTaskWithPRLink creates a project/doc/task, drives it through
// ready -> claimed -> in_progress -> review with a recorded "pr" link, and returns the
// task in its post-submit (review) state, ready to be passed to SupersedeTask.
func createSupersedableTaskWithPRLink(t *testing.T, ctx context.Context, st Store, prURL string) Task {
	t.Helper()

	proj, err := st.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := st.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := st.CreateTasks(ctx, proj.ID, []TaskInput{{Title: "Old Task", Spec: "Spec", DocumentID: doc.ID}})
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
	links := []LinkInput{{Kind: "pr", Value: prURL}}
	if _, err := st.SubmitTask(ctx, task.ID, "agent-1", "Implementation complete", nil, links, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// The task's ID is stable across the state transitions above; the caller only
	// needs it (to supersede the task and to derive its deterministic branch name).
	return task
}

func withMockForge(t *testing.T, server *httptest.Server) {
	t.Helper()
	oldBaseURL := forge.GitHubBaseURL
	forge.GitHubBaseURL = server.URL
	t.Cleanup(func() { forge.GitHubBaseURL = oldBaseURL })
}

// armSupersedeCloseSync arms st's background-close completion hook and returns a
// function that blocks until the async closeSupersededPR spawned by the next
// SupersedeTask call has finished. SupersedeTask runs that cleanup in a goroutine
// so a slow forge call can never add latency to the request; tests need this to
// deterministically observe its effects instead of racing it.
func armSupersedeCloseSync(t *testing.T, st Store) func() {
	t.Helper()
	done := make(chan struct{})
	st.(*sqliteStore).supersedeCloseHook = func() { close(done) }
	return func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for background superseded-PR close to finish")
		}
	}
}

func TestSupersedeTaskWithOpenPRLinkClosesIt(t *testing.T) {
	ctx := context.Background()
	setForgeToken(t, "testowner", "test-token")

	server, calls := newSupersedePRTestServer(t, "open")
	defer server.Close()
	withMockForge(t, server)

	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer st.Close()

	oldTask := createSupersedableTaskWithPRLink(t, ctx, st, "https://github.com/testowner/testrepo/pull/123")

	waitForClose := armSupersedeCloseSync(t, st)
	newTask, err := st.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}
	waitForClose()

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.getStateCount == 0 {
		t.Errorf("expected GetPRState to be called")
	}
	if calls.closeCount != 1 {
		t.Errorf("expected ClosePR to be called exactly once, got %d", calls.closeCount)
	}
	if len(calls.comments) != 1 || !strings.Contains(calls.comments[0], newTask.ID) {
		t.Errorf("expected a comment naming the replacement task %s, got %v", newTask.ID, calls.comments)
	}
	wantBranch := "mr/" + oldTask.ID[:8]
	if calls.deletedBranch != wantBranch {
		t.Errorf("expected branch %q to be deleted, got %q", wantBranch, calls.deletedBranch)
	}
}

func TestSupersedeTaskWithMergedPRDoesNotCloseIt(t *testing.T) {
	ctx := context.Background()
	setForgeToken(t, "testowner", "test-token")

	server, calls := newSupersedePRTestServer(t, "merged")
	defer server.Close()
	withMockForge(t, server)

	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer st.Close()

	oldTask := createSupersedableTaskWithPRLink(t, ctx, st, "https://github.com/testowner/testrepo/pull/123")

	waitForClose := armSupersedeCloseSync(t, st)
	if _, err := st.SupersedeTask(ctx, oldTask.ID, nil); err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}
	waitForClose()

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.closeCount != 0 {
		t.Errorf("expected ClosePR not to be called for a merged PR, got %d calls", calls.closeCount)
	}
	if len(calls.comments) != 0 {
		t.Errorf("expected no comment to be posted for a merged PR, got %v", calls.comments)
	}
	if calls.deletedBranch != "" {
		t.Errorf("expected no branch delete for a merged PR, got %q", calls.deletedBranch)
	}
}

func TestSupersedeTaskWithAlreadyClosedPRIsNotAnError(t *testing.T) {
	ctx := context.Background()
	setForgeToken(t, "testowner", "test-token")

	server, calls := newSupersedePRTestServer(t, "closed")
	defer server.Close()
	withMockForge(t, server)

	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer st.Close()

	oldTask := createSupersedableTaskWithPRLink(t, ctx, st, "https://github.com/testowner/testrepo/pull/123")

	waitForClose := armSupersedeCloseSync(t, st)
	if _, err := st.SupersedeTask(ctx, oldTask.ID, nil); err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}
	waitForClose()

	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.closeCount != 0 {
		t.Errorf("expected ClosePR not to be called for an already-closed PR, got %d calls", calls.closeCount)
	}
}

func TestSupersedeTaskWithNoPRLinkIsCleanNoop(t *testing.T) {
	ctx := context.Background()

	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer st.Close()

	proj, err := st.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := st.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := st.CreateTasks(ctx, proj.ID, []TaskInput{{Title: "Old Task", Spec: "Spec", DocumentID: doc.ID}})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}
	oldTask := tasks[0]

	waitForClose := armSupersedeCloseSync(t, st)
	newTask, err := st.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed for a task with no pr link: %v", err)
	}
	waitForClose()
	if newTask.ID == oldTask.ID {
		t.Errorf("expected a new task to be created")
	}
}

func TestSupersedeTaskForgeFailureStillSucceeds(t *testing.T) {
	ctx := context.Background()
	setForgeToken(t, "testowner", "test-token")

	server, _ := newSupersedePRTestServer(t, "error")
	defer server.Close()
	withMockForge(t, server)

	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer st.Close()

	oldTask := createSupersedableTaskWithPRLink(t, ctx, st, "https://github.com/testowner/testrepo/pull/123")

	waitForClose := armSupersedeCloseSync(t, st)
	newTask, err := st.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask should succeed even when the forge call fails: %v", err)
	}
	waitForClose()
	if newTask.ID == oldTask.ID || newTask.State != "backlog" {
		t.Errorf("expected a fresh replacement task despite the forge failure, got %+v", newTask)
	}

	oldTaskAfter, err := st.GetTask(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("failed to get old task: %v", err)
	}
	if oldTaskAfter.State != "superseded" || oldTaskAfter.SupersededBy == nil || *oldTaskAfter.SupersededBy != newTask.ID {
		t.Errorf("expected old task to still be superseded by the new task despite the forge failure, got %+v", oldTaskAfter)
	}
}

// TestSupersededTaskPreservesTrack verifies that the track field is preserved
// when directly superseding a task via store.SupersedeTask.
func TestSupersededTaskPreservesTrack(t *testing.T) {
	ctx := context.Background()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	proj, err := store.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a task with track="design"
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Design Task", Spec: "Spec", DocumentID: doc.ID, Track: "design"},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	oldTask := tasks[0]

	// Verify the original task has track="design"
	if oldTask.Track != "design" {
		t.Errorf("expected oldTask.Track to be 'design', got %q", oldTask.Track)
	}

	// Supersede the task
	newTask, err := store.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}

	// Verify the replacement task preserves track="design"
	if newTask.Track != "design" {
		t.Errorf("expected newTask.Track to be 'design', got %q", newTask.Track)
	}

	// Verify via GetTask as well
	newTaskFull, err := store.GetTask(ctx, newTask.ID)
	if err != nil {
		t.Fatalf("failed to get new task: %v", err)
	}
	if newTaskFull.Track != "design" {
		t.Errorf("expected newTaskFull.Track to be 'design', got %q", newTaskFull.Track)
	}
}

// TestSupersededTaskPreservesTrackEscalation verifies that the track field is preserved
// when superseding a task via the circuit-breaker escalation path.
func TestSupersededTaskPreservesTrackEscalation(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateTrue := true
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Design Task",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Escalate:     &escalateTrue,
			Track:        "design",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	maxReviewRounds := 5

	submitAndReject := func(roundNum int) {
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", roundNum, err)
		}
		if task.State == "backlog" {
			_, err := store.PromoteTask(ctx, taskID)
			if err != nil {
				t.Fatalf("failed to promote task (round %d): %v", roundNum, err)
			}
		}

		_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim task (round %d): %v", roundNum, err)
		}

		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit implement task (round %d): %v", roundNum, err)
		}

		allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks (round %d): %v", roundNum, err)
		}

		var reviewTask *Task
		for i := range allTasks {
			if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID && allTasks[i].State == "ready" {
				reviewTask = &allTasks[i]
				break
			}
		}
		if reviewTask == nil {
			t.Fatalf("review task not found (round %d)", roundNum)
		}

		_, err = store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
		if err != nil {
			t.Fatalf("failed to claim review task (round %d): %v", roundNum, err)
		}

		reject := "reject"
		_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
		if err != nil {
			t.Fatalf("failed to submit review task (round %d): %v", roundNum, err)
		}
	}

	// Rounds 1-8: should transition to ready
	for i := 1; i <= 8; i++ {
		submitAndReject(i)

		parent, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Errorf("round %d: expected parent state 'ready', got '%s'", i, parent.State)
		}
	}

	// Round 9: should escalate to sonnet
	submitAndReject(9)

	// Original task should be superseded
	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get original task: %v", err)
	}
	if parent.State != "superseded" {
		t.Errorf("expected original task state 'superseded', got '%s'", parent.State)
	}
	if parent.SupersededBy == nil {
		t.Errorf("expected original task SupersededBy to be set")
	} else {
		// Verify the escalated task exists and preserves track="design"
		escalatedTask, err := store.GetTask(ctx, *parent.SupersededBy)
		if err != nil {
			t.Fatalf("failed to get escalated task: %v", err)
		}
		if escalatedTask.Model != "sonnet" {
			t.Errorf("expected escalated task model 'sonnet', got '%s'", escalatedTask.Model)
		}
		if escalatedTask.State != "ready" {
			t.Errorf("expected escalated task state 'ready', got '%s'", escalatedTask.State)
		}
		if escalatedTask.Track != "design" {
			t.Errorf("expected escalated task Track to be 'design', got %q", escalatedTask.Track)
		}
	}
}

// TestReadsNotBlockedByWrites verifies that read queries do not block behind write transactions.
// Opens a store, starts a writer holding a transaction, then concurrently issues reads
// and verifies they complete promptly without waiting for the write to finish.
func TestSupersedeTaskWithEmptyForgeTokenSkipsCleanup(t *testing.T) {
	ctx := context.Background()
	// Explicitly set an empty FORGE_TOKENS file so the owner token lookup fails
	path := filepath.Join(t.TempDir(), "forge-tokens")
	if err := os.WriteFile(path, []byte(""), 0o600); err != nil {
		t.Fatalf("failed to write forge tokens file: %v", err)
	}
	t.Setenv("FORGE_TOKENS", path)

	server, calls := newSupersedePRTestServer(t, "open")
	defer server.Close()
	withMockForge(t, server)

	st, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer st.Close()

	oldTask := createSupersedableTaskWithPRLink(t, ctx, st, "https://github.com/testowner/testrepo/pull/123")

	waitForClose := armSupersedeCloseSync(t, st)
	newTask, err := st.SupersedeTask(ctx, oldTask.ID, nil)
	if err != nil {
		t.Fatalf("SupersedeTask failed: %v", err)
	}
	waitForClose()

	// Verify that SupersedeTask still succeeds and creates a new task
	if newTask.ID == oldTask.ID {
		t.Errorf("expected a new task ID, got the same as old task")
	}
	if newTask.State != "backlog" {
		t.Errorf("expected new task state to be backlog, got %q", newTask.State)
	}

	// Verify that no forge calls were made (no token was available)
	calls.mu.Lock()
	defer calls.mu.Unlock()
	if calls.getStateCount != 0 {
		t.Errorf("expected GetPRState not to be called when token is empty, got %d calls", calls.getStateCount)
	}
	if calls.closeCount != 0 {
		t.Errorf("expected ClosePR not to be called when token is empty, got %d calls", calls.closeCount)
	}
	if len(calls.comments) != 0 {
		t.Errorf("expected no comments to be posted when token is empty, got %v", calls.comments)
	}
	if calls.deletedBranch != "" {
		t.Errorf("expected no branch delete when token is empty, got %q", calls.deletedBranch)
	}
}

// TestReadsNotBlockedByWrites verifies that read queries do not block behind write transactions.
// Opens a store, starts a writer holding a transaction, then concurrently issues reads
// and verifies they complete promptly without waiting for the write to finish.
func TestReadsNotBlockedByWrites(t *testing.T) {
	ctx := context.Background()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "test.db")

	st, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer st.Close()

	// Create a project and some tasks
	proj, err := st.CreateProject(ctx, "Test Project", "test-repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := st.CreateDocument(ctx, proj.ID, "feature_spec", "Test Doc", "main", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := st.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Task 1", Spec: "Spec 1", DocumentID: doc.ID},
		{Title: "Task 2", Spec: "Spec 2", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}

	// Start a writer goroutine that holds a transaction for a bit
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		conn := st.Conn()
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			t.Errorf("failed to begin transaction: %v", err)
			return
		}

		// Hold the transaction open for 500ms
		time.Sleep(500 * time.Millisecond)

		if err := tx.Commit(); err != nil {
			t.Errorf("failed to commit transaction: %v", err)
		}
	}()

	// Measure time for ListTasks to complete
	start := time.Now()
	tasks2, err := st.ListTasks(ctx, proj.ID, TaskListFilter{})
	duration := time.Since(start)

	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	// Verify reads returned promptly (well under the 500ms writer hold)
	if duration >= 400*time.Millisecond {
		t.Errorf("ListTasks took too long (%v), suggests it was blocked by the writer", duration)
	}

	// Verify the read returned correct data
	if len(tasks2) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(tasks2))
	}

	// Build a map of tasks by ID for easy lookup
	tasksById := make(map[string]Task)
	for _, t := range tasks2 {
		tasksById[t.ID] = t
	}

	// Verify task details match what we created
	task1, ok := tasksById[tasks[0].ID]
	if !ok {
		t.Errorf("task 1 not found in results")
	} else if task1.Title != "Task 1" {
		t.Errorf("expected task 1 title 'Task 1', got '%s'", task1.Title)
	}

	task2, ok := tasksById[tasks[1].ID]
	if !ok {
		t.Errorf("task 2 not found in results")
	} else if task2.Title != "Task 2" {
		t.Errorf("expected task 2 title 'Task 2', got '%s'", task2.Title)
	}

	// Wait for writer to finish
	<-writerDone

	// Verify GetTask also returns correct data
	fullTask, err := st.GetTask(ctx, tasks[0].ID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}

	if fullTask.ID != tasks[0].ID || fullTask.Title != "Task 1" {
		t.Errorf("unexpected task from GetTask: %+v", fullTask)
	}
}

// TestUpdateTaskEscalateTrueToFalseDefault tests updating escalate flag from true (default) to false.
func TestUpdateTaskEscalateTrueToFalseDefault(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if !tasks[0].Escalate {
		t.Fatalf("expected initial escalate=true, got false")
	}

	updated, err := store.UpdateTaskEscalate(ctx, taskID, false)
	if err != nil {
		t.Fatalf("failed to update escalate: %v", err)
	}
	if updated.Escalate {
		t.Errorf("expected escalate=false after update, got true")
	}

	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("expected at least one event, got none")
	}
	lastEvent := events[len(events)-1]
	if lastEvent.Kind != "policy-change" {
		t.Errorf("expected last event kind='policy-change', got '%s'", lastEvent.Kind)
	}
	if lastEvent.Note == nil || *lastEvent.Note != "escalate policy changed: true → false" {
		t.Errorf("expected note 'escalate policy changed: true → false', got %v", lastEvent.Note)
	}
}

// TestUpdateTaskEscalateFalseToTrue tests updating escalate flag from false to true.
func TestUpdateTaskEscalateFalseToTrue(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateFalse := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID, Escalate: &escalateFalse},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if tasks[0].Escalate {
		t.Fatalf("expected initial escalate=false, got true")
	}

	updated, err := store.UpdateTaskEscalate(ctx, taskID, true)
	if err != nil {
		t.Fatalf("failed to update escalate to true: %v", err)
	}
	if !updated.Escalate {
		t.Errorf("expected escalate=true after update, got false")
	}

	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("expected at least one event, got none")
	}
	lastEvent := events[len(events)-1]
	if lastEvent.Kind != "policy-change" {
		t.Errorf("expected last event kind='policy-change', got '%s'", lastEvent.Kind)
	}
	if lastEvent.Note == nil || *lastEvent.Note != "escalate policy changed: false → true" {
		t.Errorf("expected note 'escalate policy changed: false → true', got %v", lastEvent.Note)
	}
}

// TestUpdateTaskEscalateNoop tests that setting escalate to its current value is idempotent
// and does not append a duplicate policy-change event.
func TestUpdateTaskEscalateNoop(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Real change: true -> false.
	if _, err := store.UpdateTaskEscalate(ctx, taskID, false); err != nil {
		t.Fatalf("failed to set escalate to false: %v", err)
	}

	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	initialEventCount := len(events)

	// No-op: false -> false.
	updated, err := store.UpdateTaskEscalate(ctx, taskID, false)
	if err != nil {
		t.Fatalf("failed to update escalate (noop): %v", err)
	}
	if updated.Escalate {
		t.Errorf("expected escalate=false after noop update, got true")
	}

	events, err = store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) != initialEventCount {
		t.Errorf("expected %d events (no new event for noop), got %d", initialEventCount, len(events))
	}
}

// TestUpdateTaskEscalateBlockedState tests that the escalate flag can be changed on a blocked task.
func TestUpdateTaskEscalateBlockedState(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateFalse := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID, Escalate: &escalateFalse},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if _, err := store.TransitionTask(ctx, taskID, "blocked", nil); err != nil {
		t.Fatalf("failed to transition task to blocked: %v", err)
	}

	updated, err := store.UpdateTaskEscalate(ctx, taskID, true)
	if err != nil {
		t.Fatalf("failed to update escalate on blocked task: %v", err)
	}
	if !updated.Escalate {
		t.Errorf("expected escalate=true, got false")
	}
	if updated.State != "blocked" {
		t.Errorf("expected state to remain 'blocked', got %q", updated.State)
	}

	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == "policy-change" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a policy-change event after updating escalate on blocked task")
	}
}

// TestUpdateTaskEscalateTerminalStateFails tests that updating escalate on terminal tasks fails
// with a TERMINAL_STATE conflict, for every terminal state.
func TestUpdateTaskEscalateTerminalStateFails(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	terminalStates := []string{"done", "failed", "abandoned", "superseded"}

	for _, state := range terminalStates {
		proj, err := store.CreateProject(ctx, fmt.Sprintf("test-project-%s", state), "https://github.com/example/repo")
		if err != nil {
			t.Fatalf("failed to create project: %v", err)
		}

		doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
		if err != nil {
			t.Fatalf("failed to create document: %v", err)
		}

		tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{Title: fmt.Sprintf("Test Task %s", state), Spec: "Test spec", DocumentID: doc.ID},
		})
		if err != nil {
			t.Fatalf("failed to create task: %v", err)
		}
		taskID := tasks[0].ID

		now := nowTimestamp()
		if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = ?, updated_at = ? WHERE id = ?", state, now, taskID); err != nil {
			t.Fatalf("failed to set task to state %s: %v", state, err)
		}

		_, err = store.UpdateTaskEscalate(ctx, taskID, false)
		if err == nil {
			t.Errorf("expected error when updating escalate on %s task, got nil", state)
			continue
		}

		var conflictErr *ConflictError
		if !errors.As(err, &conflictErr) {
			t.Errorf("expected ConflictError, got %T: %v", err, err)
		} else if conflictErr.Code != "TERMINAL_STATE" {
			t.Errorf("expected TERMINAL_STATE error code, got %s", conflictErr.Code)
		}
	}
}

// TestUpdateTaskEscalateMissingTask tests that updating escalate on a non-existent task fails.
func TestUpdateTaskEscalateMissingTask(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	_, err = store.UpdateTaskEscalate(ctx, "non-existent-task-id", true)
	if err == nil {
		t.Fatalf("expected error when updating escalate on non-existent task, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %T: %v", err, err)
	}
}

// TestUpdateTaskEscalatePreservesFields tests that a real policy change (false -> true) preserves
// every other task field, including assignment/lease.
func TestUpdateTaskEscalatePreservesFields(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateFalse := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Test Task", Spec: "Test spec", DocumentID: doc.ID, Model: "haiku", Escalate: &escalateFalse},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	originalTask := tasks[0]
	if originalTask.Escalate {
		t.Fatalf("expected initial escalate=false, got true")
	}

	now := nowTimestamp()
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = ?, updated_at = ? WHERE id = ?", "ready", now, originalTask.ID); err != nil {
		t.Fatalf("failed to promote task to ready: %v", err)
	}

	agentID := "test-agent"
	leaseTTL := 5 * time.Minute
	if _, err := store.ClaimTask(ctx, originalTask.ID, agentID, "haiku", leaseTTL); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	updated, err := store.UpdateTaskEscalate(ctx, originalTask.ID, true)
	if err != nil {
		t.Fatalf("failed to update escalate: %v", err)
	}
	if !updated.Escalate {
		t.Fatalf("expected escalate=true after a real policy change, got false")
	}

	if updated.ID != originalTask.ID {
		t.Errorf("ID changed: %s vs %s", updated.ID, originalTask.ID)
	}
	if updated.ProjectID != originalTask.ProjectID {
		t.Errorf("ProjectID changed: %s vs %s", updated.ProjectID, originalTask.ProjectID)
	}
	if updated.DocumentID != originalTask.DocumentID {
		t.Errorf("DocumentID changed: %s vs %s", updated.DocumentID, originalTask.DocumentID)
	}
	if updated.Title != originalTask.Title {
		t.Errorf("Title changed: %s vs %s", updated.Title, originalTask.Title)
	}
	if updated.Spec != originalTask.Spec {
		t.Errorf("Spec changed: %s vs %s", updated.Spec, originalTask.Spec)
	}
	if updated.Model != originalTask.Model {
		t.Errorf("Model changed: %s vs %s", updated.Model, originalTask.Model)
	}
	if updated.Kind != originalTask.Kind {
		t.Errorf("Kind changed: %s vs %s", updated.Kind, originalTask.Kind)
	}
	if updated.ReviewRound != originalTask.ReviewRound {
		t.Errorf("ReviewRound changed: %d vs %d", updated.ReviewRound, originalTask.ReviewRound)
	}
	if updated.Assignee == nil || *updated.Assignee != agentID {
		t.Errorf("Assignee not preserved: expected %s, got %v", agentID, updated.Assignee)
	}
	if updated.LeaseExpiresAt == nil {
		t.Errorf("LeaseExpiresAt not preserved: expected set, got nil")
	}
}

// TestUpdateTaskEscalatePreservesReviewHistoryPRLinksAndDependencies tests that a real policy
// change (false -> true) preserves existing review history, PR/branch links, and upstream and
// downstream dependencies.
func TestUpdateTaskEscalatePreservesReviewHistoryPRLinksAndDependencies(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	upstreamTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Upstream Task", Spec: "spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create upstream task: %v", err)
	}
	upstreamID := upstreamTasks[0].ID

	escalateFalse := false
	mainTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Main Task", Spec: "spec", DocumentID: doc.ID, Model: "haiku", DependsOn: []string{upstreamID}, Escalate: &escalateFalse},
	})
	if err != nil {
		t.Fatalf("failed to create main task: %v", err)
	}
	mainTaskID := mainTasks[0].ID

	downstreamTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Downstream Task", Spec: "spec", DocumentID: doc.ID, DependsOn: []string{mainTaskID}},
	})
	if err != nil {
		t.Fatalf("failed to create downstream task: %v", err)
	}
	downstreamID := downstreamTasks[0].ID

	// Mark the upstream dependency done (claimableSQL requires it) and force the
	// main task to ready so it can be claimed and driven through a review round.
	now := nowTimestamp()
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = ?, updated_at = ? WHERE id = ?", "done", now, upstreamID); err != nil {
		t.Fatalf("failed to force upstream task to done: %v", err)
	}
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = ?, updated_at = ? WHERE id = ?", "ready", now, mainTaskID); err != nil {
		t.Fatalf("failed to force main task to ready: %v", err)
	}

	if _, err := store.ClaimTask(ctx, mainTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim main task: %v", err)
	}
	if _, err := store.SubmitTask(ctx, mainTaskID, "agent-1", "Implementation", nil,
		[]LinkInput{{Kind: "pr", Value: "#123"}, {Kind: "branch", Value: "mr/main-task"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit main task: %v", err)
	}

	// Drive one review round to rejection so the task has genuine review history
	// before the escalate flag changes.
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var reviewTaskID string
	for _, tk := range allTasks {
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == mainTaskID && tk.State == "ready" {
			reviewTaskID = tk.ID
			break
		}
	}
	if reviewTaskID == "" {
		t.Fatalf("review task not found for main task")
	}
	if _, err := store.ClaimTask(ctx, reviewTaskID, "reviewer-1", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}
	reject := "reject"
	if _, err := store.SubmitTask(ctx, reviewTaskID, "reviewer-1", "Needs work", &reject, []LinkInput{}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit review task: %v", err)
	}

	preEvents, err := store.ListEvents(ctx, mainTaskID)
	if err != nil {
		t.Fatalf("failed to list events before escalate update: %v", err)
	}
	if len(preEvents) == 0 {
		t.Fatalf("expected review history events before escalate update, got none")
	}

	preTask, err := store.GetTask(ctx, mainTaskID)
	if err != nil {
		t.Fatalf("failed to get main task before escalate update: %v", err)
	}
	if preTask.Escalate {
		t.Fatalf("expected escalate=false before update, got true")
	}

	updated, err := store.UpdateTaskEscalate(ctx, mainTaskID, true)
	if err != nil {
		t.Fatalf("failed to update escalate: %v", err)
	}
	if !updated.Escalate {
		t.Fatalf("expected escalate=true after a real policy change, got false")
	}

	// Existing review history must be retained, plus the new policy-change event.
	postEvents, err := store.ListEvents(ctx, mainTaskID)
	if err != nil {
		t.Fatalf("failed to list events after escalate update: %v", err)
	}
	if len(postEvents) != len(preEvents)+1 {
		t.Fatalf("expected %d events after escalate update (original history + 1 policy-change), got %d", len(preEvents)+1, len(postEvents))
	}
	for i, e := range preEvents {
		if postEvents[i].ID != e.ID || postEvents[i].Kind != e.Kind {
			t.Errorf("original event %d changed: before=%+v after=%+v", i, e, postEvents[i])
		}
	}
	if postEvents[len(postEvents)-1].Kind != "policy-change" {
		t.Errorf("expected final event kind='policy-change', got '%s'", postEvents[len(postEvents)-1].Kind)
	}

	// PR/branch links must survive.
	mainTaskWithLinks, err := store.GetTask(ctx, mainTaskID)
	if err != nil {
		t.Fatalf("failed to get main task after escalate update: %v", err)
	}
	prLinkFound, branchLinkFound := false, false
	for _, link := range mainTaskWithLinks.Links {
		if link.Kind == "pr" && link.Value == "#123" {
			prLinkFound = true
		}
		if link.Kind == "branch" && link.Value == "mr/main-task" {
			branchLinkFound = true
		}
	}
	if !prLinkFound {
		t.Errorf("PR link #123 not preserved after escalate update")
	}
	if !branchLinkFound {
		t.Errorf("branch link mr/main-task not preserved after escalate update")
	}

	// Upstream and downstream dependencies must survive.
	if len(mainTaskWithLinks.DependsOn) != 1 || mainTaskWithLinks.DependsOn[0] != upstreamID {
		t.Errorf("upstream dependency not preserved: expected [%s], got %v", upstreamID, mainTaskWithLinks.DependsOn)
	}
	downstreamTask, err := store.GetTask(ctx, downstreamID)
	if err != nil {
		t.Fatalf("failed to get downstream task: %v", err)
	}
	if len(downstreamTask.DependsOn) != 1 || downstreamTask.DependsOn[0] != mainTaskID {
		t.Errorf("downstream dependency broken: expected [%s], got %v", mainTaskID, downstreamTask.DependsOn)
	}
}

// TestUpdateTaskEscalateReviewAggregationRegression is the required existing-style deterministic
// regression: a task built with escalate=false, flipped to true via UpdateTaskEscalate, then
// driven through repeated qualifying review rejections must use the configured escalation ladder
// on the round that crosses the threshold, exactly like a task created with escalate=true, while
// retaining its original review history. Mirrors TestEscalateHaikuToSonnet.
func TestUpdateTaskEscalateReviewAggregationRegression(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(), WithEscalationLadder([]string{"haiku", "sonnet", "opus"}))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	escalateFalse := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Escalate:     &escalateFalse,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	maxReviewRounds := 5

	if tasks[0].Escalate {
		t.Fatalf("expected initial escalate=false")
	}

	updated, err := store.UpdateTaskEscalate(ctx, taskID, true)
	if err != nil {
		t.Fatalf("failed to update escalate to true: %v", err)
	}
	if !updated.Escalate {
		t.Fatalf("expected escalate=true after update")
	}

	preAggregationEvents, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	hasPolicyChangeEvent := false
	for _, e := range preAggregationEvents {
		if e.Kind == "policy-change" {
			hasPolicyChangeEvent = true
		}
	}
	if !hasPolicyChangeEvent {
		t.Fatalf("expected policy-change event after escalate update")
	}

	submitAndReject := func(roundNum int) {
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", roundNum, err)
		}
		if task.State == "backlog" {
			if _, err := store.PromoteTask(ctx, taskID); err != nil {
				t.Fatalf("failed to promote task (round %d): %v", roundNum, err)
			}
		}

		if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim task (round %d): %v", roundNum, err)
		}

		if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to submit implement task (round %d): %v", roundNum, err)
		}

		allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks (round %d): %v", roundNum, err)
		}

		var reviewTask *Task
		for i := range allTasks {
			if allTasks[i].Kind == "review" && allTasks[i].TargetTaskID != nil && *allTasks[i].TargetTaskID == taskID && allTasks[i].State == "ready" {
				reviewTask = &allTasks[i]
				break
			}
		}
		if reviewTask == nil {
			t.Fatalf("review task not found (round %d)", roundNum)
		}

		if _, err := store.ClaimTask(ctx, reviewTask.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim review task (round %d): %v", roundNum, err)
		}

		reject := "reject"
		if _, err := store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to submit review task (round %d): %v", roundNum, err)
		}
	}

	// Rounds 1-8: below the escalation threshold, task cycles back to ready.
	for i := 1; i <= 8; i++ {
		submitAndReject(i)

		parent, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task (round %d): %v", i, err)
		}
		if parent.State != "ready" {
			t.Errorf("round %d: expected parent state 'ready', got '%s'", i, parent.State)
		}
	}

	// Round 9: crosses the threshold. Because escalate was flipped to true via
	// UpdateTaskEscalate, this must escalate exactly as it would for a task created
	// with escalate=true (see TestEscalateHaikuToSonnet).
	submitAndReject(9)

	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get original task: %v", err)
	}
	if parent.State != "superseded" {
		t.Fatalf("expected original task state 'superseded', got '%s'", parent.State)
	}
	if parent.SupersededBy == nil {
		t.Fatalf("expected original task SupersededBy to be set")
	}

	escalatedTask, err := store.GetTask(ctx, *parent.SupersededBy)
	if err != nil {
		t.Fatalf("failed to get escalated task: %v", err)
	}
	if escalatedTask.Model != "sonnet" {
		t.Errorf("expected escalated task model 'sonnet', got '%s'", escalatedTask.Model)
	}
	if escalatedTask.State != "ready" {
		t.Errorf("expected escalated task state 'ready', got '%s'", escalatedTask.State)
	}
	if escalatedTask.ReviewRound != 0 {
		t.Errorf("expected escalated task review_round 0, got %d", escalatedTask.ReviewRound)
	}

	// The original task's history, including the policy-change event and every
	// review round leading up to escalation, must be retained.
	finalEvents, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list final events: %v", err)
	}
	if len(finalEvents) < len(preAggregationEvents) {
		t.Fatalf("expected original history to be retained, got fewer events (%d) than before aggregation (%d)", len(finalEvents), len(preAggregationEvents))
	}
	hasPolicyChangeEventAfter, hasEscalationEvent := false, false
	for _, e := range finalEvents {
		if e.Kind == "policy-change" {
			hasPolicyChangeEventAfter = true
		}
		if e.Kind == "escalation" {
			hasEscalationEvent = true
		}
	}
	if !hasPolicyChangeEventAfter {
		t.Errorf("policy-change event missing from original task after escalation")
	}
	if !hasEscalationEvent {
		t.Errorf("expected escalation event on original task")
	}
}

// researchBlockingFinding builds a single blocking P1 finding for the chain-wide
// research round budget tests below, so each round's rejection is traceable to a
// distinct tag in the eventual block note.
func researchBlockingFinding(tag string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":%q,"in_changed_text":true,"status":"new"}]`, tag))
}

// TestResearchBudget_RoundsBeforeAndAtBudget verifies docs/features/research-track.md
// section 6: a research task stays ready while its chain-wide rejected-round count is
// below the budget, and blocks with reason "decompose" the round the count reaches
// the budget (not the round after).
func TestResearchBudget_RoundsBeforeAndAtBudget(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	const budget = 3

	blockingRound := func(round int, tag string) {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, round)
		if opus == nil || sonnet == nil {
			t.Fatalf("round %d: expected both review tasks", round)
		}
		submitResearchReviewWithBudget(t, store, ctx, opus, "opus-reviewer", "reject", researchBlockingFinding(tag), budget)
		submitResearchReviewWithBudget(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`), budget)
	}

	for i := 1; i < budget; i++ {
		blockingRound(i, fmt.Sprintf("R%d", i))
		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("round %d: failed to get parent: %v", i, err)
		}
		if parent.State != "ready" {
			t.Fatalf("round %d: expected ready (below budget %d), got %s", i, budget, parent.State)
		}
		resubmitResearchImplementTask(t, store, ctx, parentID)
	}

	// The round that brings the chain-wide count to the budget blocks, not the round
	// after (docs/features/research-track.md: "reaches the budget").
	blockingRound(budget, fmt.Sprintf("R%d", budget))
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "blocked" {
		t.Fatalf("expected blocked at budget %d, got %s", budget, parent.State)
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var blockedNote string
	for _, e := range events {
		if e.Kind == "transition" && e.Note != nil && strings.Contains(*e.Note, "decompose") {
			blockedNote = *e.Note
		}
	}
	if blockedNote == "" {
		t.Fatalf("expected a blocked transition event with reason decompose")
	}
	if !strings.Contains(blockedNote, "reason decompose") {
		t.Errorf("expected block note to carry the machine-readable reason decompose, got %q", blockedNote)
	}
	for i := 1; i <= budget; i++ {
		tag := fmt.Sprintf("Round %d: P1[a.md:1]: R%d", i, i)
		if !strings.Contains(blockedNote, tag) {
			t.Errorf("expected block note to include %q (findings from each round), got %q", tag, blockedNote)
		}
	}
}

// TestResearchBudget_UnresolvedSupersededRoundNotCounted verifies that a round left
// in review when a task is manually superseded (no reviewer has rejected it yet) is
// not counted toward the chain-wide budget: only rounds that actually completed with
// a blocking finding are rejected rounds.
func TestResearchBudget_UnresolvedSupersededRoundNotCounted(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	const budget = 2

	// Round 1 is left in review: only one of the two reviewers submits.
	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil {
		t.Fatalf("expected opus review task")
	}
	submitResearchReviewWithBudget(t, store, ctx, opus1, "opus-reviewer", "reject", researchBlockingFinding("partial"), budget)

	successor, err := store.SupersedeTask(ctx, parentID, nil)
	if err != nil {
		t.Fatalf("failed to supersede: %v", err)
	}
	if _, err := store.PromoteTask(ctx, successor.ID); err != nil {
		t.Fatalf("failed to promote successor: %v", err)
	}
	if _, err := store.ClaimTask(ctx, successor.ID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim successor: %v", err)
	}
	if _, err := store.SubmitTask(ctx, successor.ID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, budget); err != nil {
		t.Fatalf("failed to submit successor: %v", err)
	}

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, successor.ID, 1)
	if opus2 == nil || sonnet2 == nil {
		t.Fatalf("expected both review tasks on successor")
	}
	submitResearchReviewWithBudget(t, store, ctx, opus2, "opus-reviewer", "reject", researchBlockingFinding("real"), budget)
	submitResearchReviewWithBudget(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`), budget)

	final, err := store.GetTask(ctx, successor.ID)
	if err != nil {
		t.Fatalf("failed to get successor: %v", err)
	}
	// Only 1 completed, rejected round exists in the whole chain (the successor's),
	// which is below budget 2: the predecessor's unreviewed round must not count.
	if final.State != "ready" {
		t.Fatalf("expected ready (1 rejected round < budget %d; unreviewed predecessor round must not count), got %s", budget, final.State)
	}
}

// TestResearchBudget_MultipleSupersessions verifies that the chain-wide count
// accumulates correctly across both an automatic (escalation) supersession and a
// manual SupersedeTask call, never resetting, and that the block note lists every
// round from every task in the chain, oldest first.
func TestResearchBudget_MultipleSupersessions(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus", "sonnet"})
	// haiku threshold=1: a second rejected round on haiku (review_round=2 > 1)
	// auto-escalates to sonnet.
	thresholds := map[string]int{"haiku": 1}
	const budget = 4

	submitReview := func(task *Task, agent, verdict string, findings json.RawMessage) {
		t.Helper()
		if _, err := store.ClaimTask(ctx, task.ID, agent, task.Model, 5*time.Minute); err != nil {
			t.Fatalf("failed to claim %s: %v", task.ID, err)
		}
		v := verdict
		if _, err := store.SubmitTask(ctx, task.ID, agent, "notes", &v, []LinkInput{}, 8, nil, thresholds, budget, findings); err != nil {
			t.Fatalf("failed to submit %s: %v", task.ID, err)
		}
	}
	resubmitImplement := func(taskID, model string) {
		t.Helper()
		if _, err := store.ClaimTask(ctx, taskID, "agent-1", model, 5*time.Minute); err != nil {
			t.Fatalf("failed to claim %s for resubmit: %v", taskID, err)
		}
		if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, thresholds, budget); err != nil {
			t.Fatalf("failed to submit %s: %v", taskID, err)
		}
	}
	blockingRoundOn := func(taskID string, round int, tag string) {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, taskID, round)
		if opus == nil || sonnet == nil {
			t.Fatalf("expected both review tasks for %s round %d", taskID, round)
		}
		submitReview(opus, "opus-reviewer", "reject", researchBlockingFinding(tag))
		submitReview(sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	}

	// Chain-wide round 1: haiku round 1, rejected, stays ready.
	blockingRoundOn(parentID, 1, "HAIKU-R1")
	if p, err := store.GetTask(ctx, parentID); err != nil || p.State != "ready" {
		t.Fatalf("expected ready after haiku round 1, got state=%v err=%v", p.State, err)
	}
	resubmitImplement(parentID, "haiku")

	// Chain-wide round 2: haiku round 2, rejected, auto-escalates haiku->sonnet.
	blockingRoundOn(parentID, 2, "HAIKU-R2")
	original, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get original task: %v", err)
	}
	if original.State != "superseded" || original.SupersededBy == nil {
		t.Fatalf("expected auto-escalation supersede after haiku round 2, got state=%s", original.State)
	}
	sonnetTaskID := *original.SupersededBy
	sonnetTask, err := store.GetTask(ctx, sonnetTaskID)
	if err != nil {
		t.Fatalf("failed to get escalated task: %v", err)
	}
	if sonnetTask.Model != "sonnet" {
		t.Fatalf("expected escalated task model sonnet, got %s", sonnetTask.Model)
	}
	resubmitImplement(sonnetTaskID, "sonnet")

	// Chain-wide round 3: sonnet round 1, rejected, stays ready.
	blockingRoundOn(sonnetTaskID, 1, "SONNET-R1")
	if s, err := store.GetTask(ctx, sonnetTaskID); err != nil || s.State != "ready" {
		t.Fatalf("expected ready after sonnet round 1, got state=%v err=%v", s.State, err)
	}
	resubmitImplement(sonnetTaskID, "sonnet")

	// Manually supersede the sonnet task (an operator decision, not a
	// rejection-triggered escalation) and continue on the manual successor.
	manualSuccessor, err := store.SupersedeTask(ctx, sonnetTaskID, nil)
	if err != nil {
		t.Fatalf("failed to manually supersede: %v", err)
	}
	if _, err := store.PromoteTask(ctx, manualSuccessor.ID); err != nil {
		t.Fatalf("failed to promote manual successor: %v", err)
	}
	resubmitImplement(manualSuccessor.ID, "sonnet")

	// Chain-wide round 4: manual successor round 1, rejected. Chain-wide count
	// reaches the budget (4), so it blocks instead of continuing.
	blockingRoundOn(manualSuccessor.ID, 1, "MANUAL-R1")
	final, err := store.GetTask(ctx, manualSuccessor.ID)
	if err != nil {
		t.Fatalf("failed to get final task: %v", err)
	}
	if final.State != "blocked" {
		t.Fatalf("expected blocked at chain-wide budget %d across 2 supersessions (1 automatic, 1 manual), got %s", budget, final.State)
	}

	events, err := store.ListEvents(ctx, manualSuccessor.ID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var blockedNote string
	for _, e := range events {
		if e.Kind == "transition" && e.Note != nil && strings.Contains(*e.Note, "decompose") {
			blockedNote = *e.Note
		}
	}
	if blockedNote == "" {
		t.Fatalf("expected a blocked transition event with reason decompose")
	}
	// The note must carry every round from every task in the chain, oldest first,
	// chain-wide numbered — not just the final task's own round.
	wantInOrder := []string{
		"Round 1: P1[a.md:1]: HAIKU-R1",
		"Round 2: P1[a.md:1]: HAIKU-R2",
		"Round 3: P1[a.md:1]: SONNET-R1",
		"Round 4: P1[a.md:1]: MANUAL-R1",
	}
	lastIdx := -1
	for _, want := range wantInOrder {
		idx := strings.Index(blockedNote, want)
		if idx == -1 {
			t.Errorf("expected block note to include %q, got %q", want, blockedNote)
			continue
		}
		if idx <= lastIdx {
			t.Errorf("expected %q to appear after the prior round in the note, got %q", want, blockedNote)
		}
		lastIdx = idx
	}
}

// TestResearchBudget_ResolvedFindingExcludedFromDecomposeNote is a regression test
// for a bug found in review: the decompose note replayed every rejected round's
// findings verbatim, so a finding that failed an early round but was explicitly
// reported resolved (via prior_id) in a later round still appeared in the final
// block note as if it were still an unresolved blocker. docs/features/research-
// track.md section 6 says the note lists "the unresolved blocking findings so the
// owner can see whether they were shrinking or recurring" — a resolved finding must
// not be presented as still blocking.
func TestResearchBudget_ResolvedFindingExcludedFromDecomposeNote(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	const budget = 2

	// Round 1: opus raises OLD, a blocking P1. Chain-wide count: 1 (below budget).
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil || sonnet1 == nil {
		t.Fatalf("round 1: expected both review tasks")
	}
	submitResearchReviewWithBudget(t, store, ctx, opus1, "opus-reviewer", "reject", researchBlockingFinding("OLD"), budget)
	submitResearchReviewWithBudget(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`), budget)

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 1: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("round 1: expected ready (below budget %d), got %s", budget, parent.State)
	}
	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Round 2: opus resolves OLD via prior_id, but also raises a new blocking P1,
	// NEW. The round still fails on NEW, bringing the chain-wide count to the
	// budget (2), so the task blocks. The block note must carry NEW but not OLD.
	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opus2 == nil || sonnet2 == nil {
		t.Fatalf("round 2: expected both review tasks")
	}
	round2 := json.RawMessage(`[
		{"id":"f2","severity":"P1","file":"b.md","line":2,"summary":"NEW","in_changed_text":true,"status":"new"},
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"OLD, now fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"}
	]`)
	submitResearchReviewWithBudget(t, store, ctx, opus2, "opus-reviewer", "reject", round2, budget)
	submitResearchReviewWithBudget(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`), budget)

	final, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 2: %v", err)
	}
	if final.State != "blocked" {
		t.Fatalf("expected blocked at chain-wide budget %d, got %s", budget, final.State)
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var blockedNote string
	for _, e := range events {
		if e.Kind == "transition" && e.Note != nil && strings.Contains(*e.Note, "decompose") {
			blockedNote = *e.Note
		}
	}
	if blockedNote == "" {
		t.Fatalf("expected a blocked transition event with reason decompose")
	}
	if !strings.Contains(blockedNote, "NEW") {
		t.Errorf("expected block note to include the still-outstanding finding NEW, got %q", blockedNote)
	}
	if strings.Contains(blockedNote, "OLD") {
		t.Errorf("expected block note to exclude OLD, resolved in round 2 via prior_id, got %q", blockedNote)
	}
}

// TestResearchBudget_UnaggregatedRoundDoesNotMisalignRoundNumbers is a regression
// test for a bug found in review: describeChainWideBlockingFindings inferred each
// research_round_rejected event's round number from its position among that task's
// events (the Nth event is local round N), rather than the round it actually
// happened on. TransitionTask allows review->blocked and blocked->ready, so an
// operator can send a round that never finished review back to ready and the worker
// can resubmit, advancing review_round without ever appending a rejected event for
// the skipped round. Every later rejected event then gets assigned the wrong round
// number, which breaks the round-based matching describeOutstandingResearchRoundFindings
// uses to decide whether a stored finding was later resolved: a resolved finding can
// wrongly still show up in the decompose note as unresolved.
func TestResearchBudget_UnaggregatedRoundDoesNotMisalignRoundNumbers(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	const budget = 2

	// Round 1 is opened but only one of two reviewers submits. The operator blocks
	// and unblocks the parent without waiting for the second reviewer, and the
	// worker resubmits: review_round advances to 2 with no research_round_rejected
	// event ever recorded for round 1.
	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil {
		t.Fatalf("expected opus review task for round 1")
	}
	submitResearchReviewWithBudget(t, store, ctx, opus1, "opus-reviewer", "reject", researchBlockingFinding("partial"), budget)

	if _, err := store.TransitionTask(ctx, parentID, "blocked", nil); err != nil {
		t.Fatalf("failed to block parent: %v", err)
	}
	if _, err := store.TransitionTask(ctx, parentID, "ready", nil); err != nil {
		t.Fatalf("failed to unblock parent: %v", err)
	}
	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Real round 2: opus raises OLD, a blocking P1. This is the chain's first
	// rejected event, but its true round number is 2, not 1.
	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opus2 == nil || sonnet2 == nil {
		t.Fatalf("round 2: expected both review tasks")
	}
	submitResearchReviewWithBudget(t, store, ctx, opus2, "opus-reviewer", "reject", researchBlockingFinding("OLD"), budget)
	submitResearchReviewWithBudget(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`), budget)

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 2: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("round 2: expected ready (below budget %d), got %s", budget, parent.State)
	}
	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Real round 3: opus resolves OLD via prior_id, but also raises a new blocking
	// P1, NEW. The round still fails on NEW, bringing the chain-wide rejected-round
	// count to the budget (rounds 2 and 3), so the task blocks.
	opus3, sonnet3 := findResearchReviewTasks(t, store, ctx, projID, parentID, 3)
	if opus3 == nil || sonnet3 == nil {
		t.Fatalf("round 3: expected both review tasks")
	}
	round3 := json.RawMessage(`[
		{"id":"f2","severity":"P1","file":"b.md","line":2,"summary":"NEW","in_changed_text":true,"status":"new"},
		{"id":"f1b","severity":"P1","file":"a.md","line":1,"summary":"OLD, now fixed","in_changed_text":true,"status":"resolved","prior_id":"f1"}
	]`)
	submitResearchReviewWithBudget(t, store, ctx, opus3, "opus-reviewer", "reject", round3, budget)
	submitResearchReviewWithBudget(t, store, ctx, sonnet3, "sonnet-reviewer", "approve", json.RawMessage(`[]`), budget)

	final, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 3: %v", err)
	}
	if final.State != "blocked" {
		t.Fatalf("expected blocked at chain-wide budget %d, got %s", budget, final.State)
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var blockedNote string
	for _, e := range events {
		if e.Kind == "transition" && e.Note != nil && strings.Contains(*e.Note, "decompose") {
			blockedNote = *e.Note
		}
	}
	if blockedNote == "" {
		t.Fatalf("expected a blocked transition event with reason decompose")
	}
	if !strings.Contains(blockedNote, "NEW") {
		t.Errorf("expected block note to include the still-outstanding finding NEW, got %q", blockedNote)
	}
	if strings.Contains(blockedNote, "OLD") {
		t.Errorf("expected block note to exclude OLD, resolved in round 3 via prior_id — a round left unaggregated by a blocked/ready retry must not shift later rounds' numbering, got %q", blockedNote)
	}
}

// TestResearchBudget_EscalationOnlyIfBudgetRemains verifies docs/features/research-
// track.md section 6: "a task escalates at its tier's threshold only if the budget
// still has rounds left. The budget always takes precedence." A rejection that would
// otherwise trip the per-tier circuit breaker and escalate instead blocks once it
// brings the chain-wide count to the budget.
func TestResearchBudget_EscalationOnlyIfBudgetRemains(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus", "sonnet"})
	// haiku threshold=1: round 2 (review_round=2 > 1) would normally escalate.
	thresholds := map[string]int{"haiku": 1}
	const budget = 2

	submitReview := func(task *Task, agent, verdict string, findings json.RawMessage) {
		t.Helper()
		if _, err := store.ClaimTask(ctx, task.ID, agent, task.Model, 5*time.Minute); err != nil {
			t.Fatalf("failed to claim %s: %v", task.ID, err)
		}
		v := verdict
		if _, err := store.SubmitTask(ctx, task.ID, agent, "notes", &v, []LinkInput{}, 8, nil, thresholds, budget, findings); err != nil {
			t.Fatalf("failed to submit %s: %v", task.ID, err)
		}
	}
	blockingRound := func(round int, tag string) {
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, round)
		if opus == nil || sonnet == nil {
			t.Fatalf("round %d: expected both review tasks", round)
		}
		submitReview(opus, "opus-reviewer", "reject", researchBlockingFinding(tag))
		submitReview(sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	}

	// Round 1: below both the tier threshold (1) and the budget (2). Stays ready.
	blockingRound(1, "R1")
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("round 1: expected ready, got %s", parent.State)
	}
	if _, err := store.ClaimTask(ctx, parentID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim for resubmit: %v", err)
	}
	if _, err := store.SubmitTask(ctx, parentID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, thresholds, budget); err != nil {
		t.Fatalf("failed to resubmit: %v", err)
	}

	// Round 2: the tier threshold (1) is exceeded, which would normally escalate
	// haiku->sonnet. But the chain-wide count also reaches the budget (2) on this
	// same round, and the budget takes precedence: it blocks instead of escalating.
	blockingRound(2, "R2")
	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "blocked" {
		t.Fatalf("round 2: expected blocked (budget reached, takes precedence over tier escalation), got %s", parent.State)
	}
}

// TestResearchBudget_InvalidConfiguration is covered at the configuration-parsing
// layer by TestParseResearchRoundBudget in cmd/odonian; parseResearchRoundBudget
// rejects non-numeric, zero and negative values before a store is ever opened.

// TestResearchBudget_BuildDesignUnchanged verifies that a build task run past a
// small research round budget follows the existing build/design circuit breaker
// exactly as before: the research round budget must never apply to non-research
// tracks (docs/features/research-track.md section 6 and this task's "preserve
// build/design thresholds and their existing behavior" requirement).
func TestResearchBudget_BuildDesignUnchanged(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	escalateFalse := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Implement feature",
			Spec:         "Do the thing",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
			Escalate:     &escalateFalse,
			// Track defaults to "build".
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// A research round budget of 2: if it were mistakenly applied to a build task,
	// it would block by round 2. The build circuit breaker's own threshold
	// (ODONIAN_ESCALATION_THRESHOLDS default haiku=8) must be what governs instead.
	const researchBudget = 2

	submitAndReject := func(round int) {
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("round %d: failed to get task: %v", round, err)
		}
		if task.State == "backlog" {
			if _, err := store.PromoteTask(ctx, taskID); err != nil {
				t.Fatalf("round %d: failed to promote task: %v", round, err)
			}
		}
		if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
			t.Fatalf("round %d: failed to claim task: %v", round, err)
		}
		if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil, nil, researchBudget); err != nil {
			t.Fatalf("round %d: failed to submit implement task: %v", round, err)
		}

		reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
		if err != nil {
			t.Fatalf("round %d: failed to list tasks: %v", round, err)
		}
		var reviewTaskID string
		for _, rt := range reviewTasks {
			if rt.Kind == "review" && rt.TargetTaskID != nil && *rt.TargetTaskID == taskID && rt.ReviewRound == round && rt.State == "ready" {
				reviewTaskID = rt.ID
			}
		}
		if reviewTaskID == "" {
			t.Fatalf("round %d: expected a ready review task", round)
		}
		if _, err := store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
			t.Fatalf("round %d: failed to claim review task: %v", round, err)
		}
		reject := "reject"
		if _, err := store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "needs work", &reject, []LinkInput{}, 5, nil, nil, researchBudget); err != nil {
			t.Fatalf("round %d: failed to submit review task: %v", round, err)
		}
	}

	// Rounds 1-8 stay ready under the default haiku threshold of 8, well past the
	// research budget of 2 — proof the budget never applied to this build task.
	for i := 1; i <= 8; i++ {
		submitAndReject(i)
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("round %d: failed to get task: %v", i, err)
		}
		if task.State != "ready" {
			t.Fatalf("round %d: expected ready (build circuit breaker, threshold 8), got %s", i, task.State)
		}
	}

	// Round 9 crosses the build threshold (8) and blocks via the pre-existing
	// build circuit breaker, with its usual note — not the research decompose note.
	submitAndReject(9)
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.State != "blocked" {
		t.Fatalf("round 9: expected blocked (build circuit breaker), got %s", task.State)
	}

	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var blockedNote string
	for _, e := range events {
		if e.Kind == "transition" && e.Note != nil && strings.Contains(*e.Note, "auto-blocked") {
			blockedNote = *e.Note
		}
	}
	if blockedNote == "" {
		t.Fatalf("expected the existing auto-blocked transition note")
	}
	if strings.Contains(blockedNote, "decompose") {
		t.Errorf("build task must never use the research decompose note, got %q", blockedNote)
	}
}

// TestResearchDefaultModel tests the research default model behavior.
// Acceptance criteria:
// 1. A research task without a model gets the configured default
// 2. A research task with an explicit model keeps it
// 3. A build task without a model still gets the existing fallback
// 4. A research task without a model and no setting gets the existing fallback
// 5. An unallowlisted setting fails startup validation
func TestResearchDefaultModel(t *testing.T) {
	ctx := context.Background()

	// Test 1: Research task with configured default model
	allowlist := []string{"haiku", "sonnet", "opus"}
	store1, err := Open("file::memory:?cache=shared", allowlist,
		WithResearchDefaultModel("opus"))
	if err != nil {
		t.Fatalf("failed to open store: %v", err)
	}
	defer store1.Close()

	proj1, err := store1.CreateProject(ctx, "test-project-1", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc1, err := store1.CreateDocument(ctx, proj1.ID, "design", "Test Doc", "TEST.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a research task without specifying a model
	tasks1, err := store1.CreateTasks(ctx, proj1.ID, []TaskInput{
		{
			Title:      "Research task without model",
			Spec:       "Test spec",
			DocumentID: doc1.ID,
			Track:      "research",
		},
	})
	if err != nil {
		t.Fatalf("failed to create research task: %v", err)
	}

	if tasks1[0].Model != "opus" {
		t.Errorf("expected research task to get configured default model 'opus', got '%s'", tasks1[0].Model)
	}

	// Test 2: Research task with explicit model
	tasks2, err := store1.CreateTasks(ctx, proj1.ID, []TaskInput{
		{
			Title:      "Research task with explicit model",
			Spec:       "Test spec",
			DocumentID: doc1.ID,
			Track:      "research",
			Model:      "sonnet",
		},
	})
	if err != nil {
		t.Fatalf("failed to create research task with explicit model: %v", err)
	}

	if tasks2[0].Model != "sonnet" {
		t.Errorf("expected research task to keep explicit model 'sonnet', got '%s'", tasks2[0].Model)
	}

	// Test 3: Build task without model gets existing fallback
	tasks3, err := store1.CreateTasks(ctx, proj1.ID, []TaskInput{
		{
			Title:      "Build task without model",
			Spec:       "Test spec",
			DocumentID: doc1.ID,
			Track:      "build",
		},
	})
	if err != nil {
		t.Fatalf("failed to create build task: %v", err)
	}

	if tasks3[0].Model != "haiku" {
		t.Errorf("expected build task to get fallback model 'haiku', got '%s'", tasks3[0].Model)
	}

	// Test 4: Research task without model and no configured default gets fallback
	store2, err := Open("file::memory:?cache=shared", allowlist)
	if err != nil {
		t.Fatalf("failed to open store without research default: %v", err)
	}
	defer store2.Close()

	proj2, err := store2.CreateProject(ctx, "test-project-2", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc2, err := store2.CreateDocument(ctx, proj2.ID, "design", "Test Doc", "TEST.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks4, err := store2.CreateTasks(ctx, proj2.ID, []TaskInput{
		{
			Title:      "Research task without configured default",
			Spec:       "Test spec",
			DocumentID: doc2.ID,
			Track:      "research",
		},
	})
	if err != nil {
		t.Fatalf("failed to create research task: %v", err)
	}

	if tasks4[0].Model != "haiku" {
		t.Errorf("expected research task without configured default to get fallback 'haiku', got '%s'", tasks4[0].Model)
	}
}

// TestApprovedNoOpFinalizationAgentMergeFalse verifies that a no-op task with agent_merge=false
// transitions to done when all reviewers approve, regardless of agent_merge setting.
func TestApprovedNoOpFinalizationAgentMergeFalse(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create an implement task with agent_merge=false (default)
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "No-op task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Verify agent_merge defaults to false
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.AgentMerge != false {
		t.Errorf("task should have agent_merge=false, got %v", task.AgentMerge)
	}

	// Promote and claim
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit for review with no_op link (no PR)
	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Get the review task
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	if len(reviewTasks) != 1 {
		t.Fatalf("expected 1 review task, got %d", len(reviewTasks))
	}
	reviewTaskID := reviewTasks[0].ID

	// Claim and approve the review
	_, err = store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Task should be done, not just approved
	doneTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if doneTask.State != "done" {
		t.Errorf("no-op task should transition to done after approval, got %s", doneTask.State)
	}
}

// TestApprovedNoOpFinalizationAgentMergeTrue verifies that a no-op task with agent_merge=true
// also transitions to done when all reviewers approve.
func TestApprovedNoOpFinalizationAgentMergeTrue(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a task in the test
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "No-op task with agent_merge",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Update task to have agent_merge=true
	_, err = store.Conn().ExecContext(ctx, `UPDATE task SET agent_merge = true WHERE id = ?`, taskID)
	if err != nil {
		t.Fatalf("failed to set agent_merge: %v", err)
	}

	// Promote and claim
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit for review with no_op link
	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Get the review task
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	if len(reviewTasks) != 1 {
		t.Fatalf("expected 1 review task, got %d", len(reviewTasks))
	}
	reviewTaskID := reviewTasks[0].ID

	// Claim and approve the review
	_, err = store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Task should be done
	doneTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if doneTask.State != "done" {
		t.Errorf("no-op task with agent_merge=true should transition to done, got %s", doneTask.State)
	}
}

// TestTwoReviewersOneApproveOneReject verifies that partial approvals do not auto-finalize no-op tasks.
func TestTwoReviewersOneApproveOneReject(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a task with two reviewers
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Two reviewer task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus", "sonnet"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote and claim
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit for review with no_op link
	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Get the review tasks
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	if len(reviewTasks) != 2 {
		t.Fatalf("expected 2 review tasks, got %d", len(reviewTasks))
	}

	// Find the opus and sonnet review tasks
	var opusReviewTask, sonnetReviewTask Task
	for _, rt := range reviewTasks {
		if rt.Model == "opus" {
			opusReviewTask = rt
		} else if rt.Model == "sonnet" {
			sonnetReviewTask = rt
		}
	}

	// First reviewer (opus) approves
	_, err = store.ClaimTask(ctx, opusReviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim opus review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, opusReviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit opus review verdict: %v", err)
	}

	// Task should still be in review (not all reviewers approved yet)
	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.State != "review" {
		t.Errorf("task should still be in review with one approval, got %s", task.State)
	}

	// Second reviewer (sonnet) rejects
	_, err = store.ClaimTask(ctx, sonnetReviewTask.ID, "sonnet-reviewer", "sonnet", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim sonnet review task: %v", err)
	}

	reject := "reject"
	_, err = store.SubmitTask(ctx, sonnetReviewTask.ID, "sonnet-reviewer", "Needs more work", &reject, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit sonnet review verdict: %v", err)
	}

	// Task should move back to ready for rework
	finalTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get final task: %v", err)
	}
	if finalTask.State != "ready" {
		t.Errorf("task should move to ready after rejection, got %s", finalTask.State)
	}
}

// TestNoOpWithBothLinksDoesNotFinalize verifies that a task with both no-op and PR links does not auto-finalize.
func TestNoOpWithBothLinksDoesNotFinalize(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "No-op with PR task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Promote and claim
	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	// Submit for review with both no_op AND PR links
	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{
		{Kind: "no_op", Value: "acceptance-already-met"},
		{Kind: "pr", Value: "#100"},
	}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Get the review task
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	if len(reviewTasks) != 1 {
		t.Fatalf("expected 1 review task, got %d", len(reviewTasks))
	}

	// Approve the review
	_, err = store.ClaimTask(ctx, reviewTasks[0].ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTasks[0].ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Task should be approved (not done) because it has a PR link
	approvedTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if approvedTask.State != "approved" {
		t.Errorf("task with both no-op and PR should stay at approved, got %s", approvedTask.State)
	}
}

// TestNoOpDependentTaskClaimability verifies that a dependent task becomes claimable after no-op finalization.
func TestNoOpDependentTaskClaimability(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create a no-op task
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "No-op task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create no-op task: %v", err)
	}
	noOpTaskID := tasks[0].ID

	// Create a dependent task
	depTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Dependent task",
			Spec:       "Test spec",
			DocumentID: doc.ID,
			Model:      "haiku",
			DependsOn:  []string{noOpTaskID},
		},
	})
	if err != nil {
		t.Fatalf("failed to create dependent task: %v", err)
	}
	depTaskID := depTasks[0].ID

	// Promote the dependent task to ready so it's waiting on the dependency
	_, err = store.PromoteTask(ctx, depTaskID)
	if err != nil {
		t.Fatalf("failed to promote dependent task: %v", err)
	}

	// Verify dependent task is in ready state waiting on dependency
	depTaskBeforeParent, err := store.GetTask(ctx, depTaskID)
	if err != nil {
		t.Fatalf("failed to get dependent task before parent: %v", err)
	}
	if depTaskBeforeParent.State != "ready" {
		t.Errorf("dependent task should be ready before parent is done, got %s", depTaskBeforeParent.State)
	}

	// Submit the no-op task through the full flow
	_, err = store.PromoteTask(ctx, noOpTaskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, noOpTaskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, noOpTaskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Try to claim the dependent task while parent is still in review - should fail due to unmet dependencies
	_, err = store.ClaimTask(ctx, depTaskID, "agent-2", "haiku", 5*time.Minute)
	if !errors.Is(err, ErrConflict) {
		t.Errorf("expected ErrConflict while parent is in review, got: %v", err)
	}

	// Get the review task and approve it
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}

	_, err = store.ClaimTask(ctx, reviewTasks[0].ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTasks[0].ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Verify the no-op task is done
	noOpTask, err := store.GetTask(ctx, noOpTaskID)
	if err != nil {
		t.Fatalf("failed to get no-op task: %v", err)
	}
	if noOpTask.State != "done" {
		t.Errorf("no-op task should be done, got %s", noOpTask.State)
	}

	// Dependent task should still be in ready state and claimable after parent is done
	depTask, err := store.GetTask(ctx, depTaskID)
	if err != nil {
		t.Fatalf("failed to get dependent task: %v", err)
	}
	if depTask.State != "ready" {
		t.Errorf("dependent task should still be ready after parent is done, got %s", depTask.State)
	}

	// Verify it can be claimed (dependencies satisfied since parent is done)
	_, err = store.ClaimTask(ctx, depTaskID, "agent-2", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim dependent task after parent done: %v", err)
	}
}

// TestTwoReviewersBothApproveFinalizesNoOp verifies that a no-op task finalizes to done when all reviewers approve.
func TestTwoReviewersBothApproveFinalizesNoOp(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Two reviewer no-op task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus", "sonnet"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}
	if len(reviewTasks) != 2 {
		t.Fatalf("expected 2 review tasks, got %d", len(reviewTasks))
	}

	var opusReviewTask, sonnetReviewTask Task
	for _, rt := range reviewTasks {
		if rt.Model == "opus" {
			opusReviewTask = rt
		} else if rt.Model == "sonnet" {
			sonnetReviewTask = rt
		}
	}

	// Both reviewers approve
	_, err = store.ClaimTask(ctx, opusReviewTask.ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim opus review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, opusReviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit opus review verdict: %v", err)
	}

	_, err = store.ClaimTask(ctx, sonnetReviewTask.ID, "sonnet-reviewer", "sonnet", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim sonnet review task: %v", err)
	}

	_, err = store.SubmitTask(ctx, sonnetReviewTask.ID, "sonnet-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit sonnet review verdict: %v", err)
	}

	// Task should transition to done when all reviewers approve a no-op task
	finalTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get final task: %v", err)
	}
	if finalTask.State != "done" {
		t.Errorf("task should transition to done when both reviewers approve no-op, got %s", finalTask.State)
	}
}

// TestNoOpNoMergeTaskCreated verifies that no merge task is created on no-op finalization.
func TestNoOpNoMergeTaskCreated(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "No-op task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}

	_, err = store.ClaimTask(ctx, reviewTasks[0].ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTasks[0].ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Verify no merge task is created on no-op finalization
	mergeTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Kind: ptrStr("merge")})
	if err != nil {
		t.Fatalf("failed to list merge tasks: %v", err)
	}
	if len(mergeTasks) != 0 {
		t.Errorf("expected 0 merge tasks on no-op finalization, got %d", len(mergeTasks))
	}
}

// TestAgentMergeFalseWithPRRemainsApproved verifies that agent_merge=false tasks with PR links remain approved.
func TestAgentMergeFalseWithPRRemainsApproved(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "PR task with agent_merge=false",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#123"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}

	_, err = store.ClaimTask(ctx, reviewTasks[0].ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTasks[0].ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.State != "approved" {
		t.Errorf("agent_merge=false PR task should remain approved, got %s", task.State)
	}

	// Verify no merge task is created for agent_merge=false
	mergeTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Kind: ptrStr("merge")})
	if err != nil {
		t.Fatalf("failed to list merge tasks: %v", err)
	}
	if len(mergeTasks) != 0 {
		t.Errorf("expected 0 merge tasks for agent_merge=false, got %d", len(mergeTasks))
	}
}

// TestAgentMergeTrueWithPRCreatesExactlyOneMergeTask verifies that agent_merge=true tasks with PR links create exactly one merge task.
func TestAgentMergeTrueWithPRCreatesExactlyOneMergeTask(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "PR task with agent_merge=true",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	// Set agent_merge=true
	_, err = store.Conn().ExecContext(ctx, `UPDATE task SET agent_merge = true WHERE id = ?`, taskID)
	if err != nil {
		t.Fatalf("failed to set agent_merge: %v", err)
	}

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#123"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}

	_, err = store.ClaimTask(ctx, reviewTasks[0].ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTasks[0].ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	task, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if task.State != "approved" {
		t.Errorf("agent_merge=true PR task should remain approved, got %s", task.State)
	}

	// Verify exactly one merge task is created
	mergeTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Kind: ptrStr("merge")})
	if err != nil {
		t.Fatalf("failed to list merge tasks: %v", err)
	}
	if len(mergeTasks) != 1 {
		t.Errorf("expected 1 merge task for agent_merge=true with PR, got %d", len(mergeTasks))
	}
}

// TestNoOpWithOnlyTombstonedPRFinalizes verifies that a no-op task finalizes when the only PR link is tombstoned.
func TestNoOpWithOnlyTombstonedPRFinalizes(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "No-op task with tombstoned PR",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	// Submit with no_op only
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Manually add a tombstoned PR link to verify it's filtered
	now := time.Now().Format(time.RFC3339Nano)
	_, err = store.Conn().ExecContext(ctx, `
		INSERT INTO task_link (id, task_id, kind, value, tombstoned_at)
		VALUES (?, ?, ?, ?, ?)
	`, GenerateID(), taskID, "pr", "#123", now)
	if err != nil {
		t.Fatalf("failed to insert tombstoned link: %v", err)
	}

	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}

	_, err = store.ClaimTask(ctx, reviewTasks[0].ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTasks[0].ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Task should still finalize to done since tombstoned PR is ignored
	finalTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get final task: %v", err)
	}
	if finalTask.State != "done" {
		t.Errorf("task with no_op and only tombstoned PR should finalize to done, got %s", finalTask.State)
	}
}

// TestTombstonedNoOpDoesNotFinalize verifies that a task with only a tombstoned no_op link stays approved.
func TestTombstonedNoOpDoesNotFinalize(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "No-op task",
			Spec:         "Test spec",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	_, err = store.PromoteTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}

	_, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}

	maxReviewRounds := 5
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "no_op", Value: "acceptance-already-met"}}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	// Get the no_op link and tombstone it
	var noOpLinkID string
	err = store.Conn().QueryRowContext(ctx, "SELECT id FROM task_link WHERE task_id = ? AND kind = ? AND tombstoned_at IS NULL", taskID, "no_op").Scan(&noOpLinkID)
	if err != nil {
		t.Fatalf("failed to get no_op link: %v", err)
	}

	err = store.TombstoneLink(ctx, taskID, noOpLinkID)
	if err != nil {
		t.Fatalf("failed to tombstone no_op link: %v", err)
	}

	// Get the review task and approve it
	reviewTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{State: ptrStr("ready"), Kind: ptrStr("review")})
	if err != nil {
		t.Fatalf("failed to list review tasks: %v", err)
	}

	_, err = store.ClaimTask(ctx, reviewTasks[0].ID, "opus-reviewer", "opus", 5*time.Minute)
	if err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}

	approve := "approve"
	_, err = store.SubmitTask(ctx, reviewTasks[0].ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, maxReviewRounds, nil, nil, testUnlimitedResearchBudget)
	if err != nil {
		t.Fatalf("failed to submit review verdict: %v", err)
	}

	// Task should remain approved (not finalized) because the only no_op link is tombstoned
	finalTask, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get final task: %v", err)
	}
	if finalTask.State != "approved" {
		t.Errorf("task with only a tombstoned no_op should remain approved, got %s", finalTask.State)
	}

	// Verify no merge task was created for no_op
	mergeTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Kind: ptrStr("merge")})
	if err != nil {
		t.Fatalf("failed to list merge tasks: %v", err)
	}
	if len(mergeTasks) > 0 {
		t.Errorf("no merge task should be created for no_op finalization, got %d", len(mergeTasks))
	}
}

// TestResearchSupersessionSpecCompaction verifies that research task supersession
// compacts the spec to include only unresolved findings from the last review round,
// per docs/features/research-track.md section 7, not all prior feedback.
func TestResearchSupersessionSpecCompaction(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil {
		t.Fatalf("round 1: expected review task")
	}

	// One P1 (blocking) and one P3 (non-blocking) finding, both new.
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong page number","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 1: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("round 1: expected ready, got %s", parent.State)
	}

	resubmitResearchImplementTask(t, store, ctx, parentID)

	// Round 2: f1 resolved, f2 still open, plus new non-blocking f3.
	opus2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opus2 == nil {
		t.Fatalf("round 2: expected review task")
	}
	findings2 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"resolved","prior_id":"f1"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong page number","in_changed_text":false,"status":"still_open","prior_id":"f2"},
		{"id":"f3","severity":"P3","file":"c.md","line":10,"summary":"missing context","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", findings2)

	// Manually supersede the task to test spec compaction, regardless of current state
	// (the state machine is secondary to the spec compaction feature under test).
	escalated, err := store.SupersedeTask(ctx, parentID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}

	if !strings.Contains(escalated.Spec, "Unresolved findings from last review round") {
		t.Errorf("spec should contain 'Unresolved findings from last review round' section")
	}
	if !strings.Contains(escalated.Spec, `"id":"f2"`) {
		t.Errorf("spec should contain unresolved finding f2")
	}
	if !strings.Contains(escalated.Spec, `"id":"f3"`) {
		t.Errorf("spec should contain unresolved finding f3")
	}
	if strings.Contains(escalated.Spec, `"id":"f1"`) {
		t.Errorf("spec should not include resolved finding f1 in unresolved findings")
	}

	oldTask, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get old task: %v", err)
	}
	if oldTask.State != "superseded" {
		t.Errorf("old task should be superseded, got %s", oldTask.State)
	}
	if oldTask.SupersededBy == nil || *oldTask.SupersededBy != escalated.ID {
		t.Errorf("old task should reference new task in SupersededBy")
	}

	if escalated.ProjectID != parent.ProjectID {
		t.Errorf("project should be preserved")
	}
	if escalated.DocumentID != parent.DocumentID {
		t.Errorf("document should be preserved")
	}
	if escalated.Track != "research" {
		t.Errorf("track should be preserved as research")
	}
	if len(escalated.ReviewModels) != len(parent.ReviewModels) {
		t.Errorf("review models should be preserved")
	}
}

// TestResearchSupersessionAllFindingsResolved verifies that research task supersession
// doesn't add a findings section if all findings are resolved.
func TestResearchSupersessionAllFindingsResolved(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus", "sonnet"})

	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil || sonnet1 == nil {
		t.Fatalf("round 1: expected review tasks")
	}

	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 1: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("round 1: expected ready, got %s", parent.State)
	}

	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opus2 == nil || sonnet2 == nil {
		t.Fatalf("round 2: expected review tasks")
	}
	findings2 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"resolved","prior_id":"f1"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", findings2)
	submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent after round 2: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected approved, got %s", parent.State)
	}

	escalated, err := store.SupersedeTask(ctx, parentID, ptrStr("sonnet"))
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}

	if strings.Contains(escalated.Spec, "Unresolved findings from last review round") {
		t.Errorf("spec should not contain unresolved findings section when all findings are resolved")
	}
}

// TestBuildTaskSupersessionPrependsFeedback verifies that build tasks still append
// all prior feedback, unchanged from before, per docs/features/research-track.md.
func TestBuildTaskSupersessionPrependsFeedback(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Build something",
			Spec:       "Original spec",
			DocumentID: doc.ID,
			Model:      "haiku",
			Track:      "build",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	buildTaskID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, buildTaskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, buildTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err = store.SubmitTask(ctx, buildTaskID, "agent-1", "Attempt 1", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 1, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	review1, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Kind: ptrStr("review")})
	if err != nil || len(review1) == 0 {
		t.Fatalf("failed to find review task: %v", err)
	}
	reviewTask1 := review1[0]

	if _, err = store.ClaimTask(ctx, reviewTask1.ID, "reviewer", reviewTask1.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review: %v", err)
	}

	v := "reject"
	if _, err = store.SubmitTask(ctx, reviewTask1.ID, "reviewer", "Feedback 1", &v, []LinkInput{}, 1, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit review: %v", err)
	}

	buildTask, err := store.GetTask(ctx, buildTaskID)
	if err != nil {
		t.Fatalf("failed to get build task: %v", err)
	}
	if buildTask.State != "ready" {
		t.Fatalf("expected ready, got %s", buildTask.State)
	}

	escalated, err := store.SupersedeTask(ctx, buildTaskID, ptrStr("haiku"))
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}

	if !strings.Contains(escalated.Spec, "Prior attempt feedback") {
		t.Errorf("build task spec should contain 'Prior attempt feedback' section")
	}
	if !strings.Contains(escalated.Spec, "Feedback 1") {
		t.Errorf("build task spec should contain feedback text")
	}
	if strings.Contains(escalated.Spec, "Unresolved findings from last review round") {
		t.Errorf("build task should not use research-style findings compaction")
	}
}

// TestDesignTaskSupersessionPrependsFeedback verifies that design tasks still append
// all prior feedback, unchanged from before.
func TestDesignTaskSupersessionPrependsFeedback(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:      "Design something",
			Spec:       "Original design spec",
			DocumentID: doc.ID,
			Model:      "haiku",
			Track:      "design",
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	designTaskID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, designTaskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, designTaskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err = store.SubmitTask(ctx, designTaskID, "agent-1", "Attempt 1", nil, []LinkInput{{Kind: "pr", Value: "#200"}}, 1, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	review1, err := store.ListTasks(ctx, proj.ID, TaskListFilter{Kind: ptrStr("review")})
	if err != nil || len(review1) == 0 {
		t.Fatalf("failed to find review task: %v", err)
	}
	reviewTask1 := review1[0]

	if _, err = store.ClaimTask(ctx, reviewTask1.ID, "reviewer", reviewTask1.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review: %v", err)
	}

	v := "reject"
	if _, err = store.SubmitTask(ctx, reviewTask1.ID, "reviewer", "Design feedback", &v, []LinkInput{}, 1, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit review: %v", err)
	}

	designTask, err := store.GetTask(ctx, designTaskID)
	if err != nil {
		t.Fatalf("failed to get design task: %v", err)
	}
	if designTask.State != "ready" {
		t.Fatalf("expected ready, got %s", designTask.State)
	}

	escalated, err := store.SupersedeTask(ctx, designTaskID, ptrStr("haiku"))
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}

	if !strings.Contains(escalated.Spec, "Prior attempt feedback") {
		t.Errorf("design task spec should contain 'Prior attempt feedback' section")
	}
	if !strings.Contains(escalated.Spec, "Design feedback") {
		t.Errorf("design task spec should contain feedback text")
	}
	if strings.Contains(escalated.Spec, "Unresolved findings from last review round") {
		t.Errorf("design task should not use research-style findings compaction")
	}
}

// TestResearchSpecCompactionHistoryLinks verifies that replacement specs link back to
// the predecessor task, so the complete event history can be traced from the spec.
func TestResearchSpecCompactionHistoryLinks(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil {
		t.Fatalf("round 1: expected review task")
	}
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	parent, _ := store.GetTask(ctx, parentID)
	if parent.State != "ready" {
		t.Fatalf("round 1: expected ready, got %s", parent.State)
	}

	resubmitResearchImplementTask(t, store, ctx, parentID)

	opus2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opus2 == nil {
		t.Fatalf("round 2: expected review task")
	}
	findings2 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"still_open","prior_id":"f1"},
		{"id":"f2","severity":"P3","file":"b.md","line":5,"summary":"wrong page","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", findings2)

	escalated, err := store.SupersedeTask(ctx, parentID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}

	if !strings.Contains(escalated.Spec, "## Research task history") {
		t.Errorf("replacement spec should contain research task history section")
	}
	if !strings.Contains(escalated.Spec, "Predecessor task") {
		t.Errorf("replacement spec should contain a predecessor link")
	}
	if !strings.Contains(escalated.Spec, parentID) {
		t.Errorf("replacement spec should link to the original task ID: %s", parentID)
	}
	if !strings.Contains(escalated.Spec, "## Unresolved findings from last review round") {
		t.Errorf("replacement spec should contain unresolved findings section")
	}
	if !strings.Contains(escalated.Spec, `"id":"f1"`) {
		t.Errorf("spec should contain unresolved finding f1")
	}
	if !strings.Contains(escalated.Spec, `"id":"f2"`) {
		t.Errorf("spec should contain unresolved finding f2")
	}
}

// TestResearchSupersessionOriginalAssignmentPreserved verifies that the original
// assignment is preserved verbatim, ahead of the generated history section, when a
// research task is superseded.
func TestResearchSupersessionOriginalAssignmentPreserved(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	parent, _ := store.GetTask(ctx, parentID)
	originalSpec := parent.Spec

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil {
		t.Fatalf("round 1: expected review task")
	}
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	escalated, err := store.SupersedeTask(ctx, parentID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}

	if !strings.HasPrefix(escalated.Spec, originalSpec) {
		t.Errorf("replacement spec should start with the original assignment")
	}

	historyIdx := strings.Index(escalated.Spec, "## Research task history")
	if historyIdx < len(originalSpec) {
		t.Errorf("research task history section appears before original assignment ends")
	}
}

// TestResearchSupersessionPolicyPreservation verifies that task policies
// (agent_merge, escalate, track, review_models) are preserved during research supersession.
func TestResearchSupersessionPolicyPreservation(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	original, _ := store.GetTask(ctx, parentID)
	originalAgentMerge := original.AgentMerge
	originalEscalate := original.Escalate

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus1 == nil {
		t.Fatalf("expected review task")
	}
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	escalated, err := store.SupersedeTask(ctx, parentID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("failed to supersede task: %v", err)
	}

	escalatedTask, _ := store.GetTask(ctx, escalated.ID)
	if escalatedTask.AgentMerge != originalAgentMerge {
		t.Errorf("agent_merge should be preserved on supersession: was %v, got %v",
			originalAgentMerge, escalatedTask.AgentMerge)
	}
	if escalatedTask.Escalate != originalEscalate {
		t.Errorf("escalate flag should be preserved on supersession: was %v, got %v",
			originalEscalate, escalatedTask.Escalate)
	}
	if escalatedTask.Track != "research" {
		t.Errorf("track should be preserved as research, got %s", escalatedTask.Track)
	}
	if len(escalatedTask.ReviewModels) != len(original.ReviewModels) {
		t.Errorf("review models should be preserved: was %v, got %v",
			original.ReviewModels, escalatedTask.ReviewModels)
	}
}

// TestResearchSupersessionChainedSpecCompaction verifies that several successive
// research supersessions keep the spec bounded and contain only the latest round's
// unresolved findings, with an exact, idempotent structure rather than accumulating
// history or findings blocks from earlier rounds.
func TestResearchSupersessionChainedSpecCompaction(t *testing.T) {
	store, ctx, projID, taskA := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	taskAObj, _ := store.GetTask(ctx, taskA)
	originalSpec := taskAObj.Spec

	conn := store.(*sqliteStore).conn
	resubmit := func(id, model string) {
		t.Helper()
		if _, err := conn.ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", id); err != nil {
			t.Fatalf("failed to set task %s to ready: %v", id, err)
		}
		if _, err := store.ClaimTask(ctx, id, "agent-1", model, 5*time.Minute); err != nil {
			t.Fatalf("failed to claim task %s: %v", id, err)
		}
		if _, err := store.SubmitTask(ctx, id, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to resubmit task %s: %v", id, err)
		}
	}

	// A -> B with finding f1.
	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"finding one","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)
	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("first supersession failed: %v", err)
	}
	if !strings.Contains(taskB.Spec, `"id":"f1"`) {
		t.Errorf("task B spec should contain f1")
	}
	if !strings.HasPrefix(taskB.Spec, originalSpec) {
		t.Errorf("task B spec should start with original assignment")
	}

	// B -> C: f1 resolved, new finding f2.
	taskBObj, _ := store.GetTask(ctx, taskB.ID)
	resubmit(taskB.ID, taskBObj.Model)
	opus2, _ := findResearchReviewTasks(t, store, ctx, projID, taskB.ID, 1)
	findings2 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"finding one","in_changed_text":true,"status":"resolved","prior_id":"f1"},
		{"id":"f2","severity":"P2","file":"b.md","line":2,"summary":"finding two","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", findings2)
	taskC, err := store.SupersedeTask(ctx, taskB.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("second supersession failed: %v", err)
	}
	if strings.Contains(taskC.Spec, `"id":"f1"`) {
		t.Errorf("task C spec should not contain resolved f1 in structured findings")
	}
	if !strings.Contains(taskC.Spec, `"id":"f2"`) {
		t.Errorf("task C spec should contain f2")
	}
	if strings.Count(taskC.Spec, "## Research task history") != 1 {
		t.Errorf("task C should have exactly one history section")
	}

	// C -> D: f2 still open, new finding f3.
	taskCObj, _ := store.GetTask(ctx, taskC.ID)
	resubmit(taskC.ID, taskCObj.Model)
	opus3, _ := findResearchReviewTasks(t, store, ctx, projID, taskC.ID, 1)
	findings3 := json.RawMessage(`[
		{"id":"f2","severity":"P2","file":"b.md","line":2,"summary":"finding two","in_changed_text":false,"status":"still_open","prior_id":"f2"},
		{"id":"f3","severity":"P3","file":"c.md","line":3,"summary":"finding three","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus3, "opus-reviewer", "reject", findings3)
	taskD, err := store.SupersedeTask(ctx, taskC.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("third supersession failed: %v", err)
	}

	initialSpecSize := len(originalSpec)
	sizeD := len(taskD.Spec)
	if sizeD > initialSpecSize*50 {
		t.Errorf("chained supersession spec has unreasonable growth, initial: %d, final: %d", initialSpecSize, sizeD)
	}
	if !strings.Contains(taskD.Spec, `"id":"f2"`) {
		t.Errorf("task D spec should contain f2 in structured findings")
	}
	if !strings.Contains(taskD.Spec, `"id":"f3"`) {
		t.Errorf("task D spec should contain f3 in structured findings")
	}
	if strings.Contains(taskD.Spec, `"id":"f1"`) {
		t.Errorf("task D spec should not contain f1 in structured findings")
	}
	if historyCount := strings.Count(taskD.Spec, "## Research task history"); historyCount != 1 {
		t.Errorf("task D should have exactly one history section, got %d (spec compaction failed)", historyCount)
	}
	if !strings.Contains(taskD.Spec, taskC.ID) {
		t.Errorf("task D should link to immediate predecessor task C (%s)", taskC.ID)
	}
	if !strings.HasPrefix(taskD.Spec, originalSpec) {
		t.Errorf("task D spec should still start with the original assignment")
	}

	// D -> E with the same findings: the spec must be byte-identical in structure
	// (same length; the only textual difference is the predecessor ID, which is the
	// same fixed length as D's own predecessor ID), proving the spec doesn't grow
	// across repeated supersessions.
	taskDObj, _ := store.GetTask(ctx, taskD.ID)
	resubmit(taskD.ID, taskDObj.Model)
	opus4, _ := findResearchReviewTasks(t, store, ctx, projID, taskD.ID, 1)
	findings4 := json.RawMessage(`[
		{"id":"f2","severity":"P2","file":"b.md","line":2,"summary":"finding two","in_changed_text":false,"status":"still_open","prior_id":"f2"},
		{"id":"f3","severity":"P3","file":"c.md","line":3,"summary":"finding three","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus4, "opus-reviewer", "reject", findings4)
	taskE, err := store.SupersedeTask(ctx, taskD.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("fourth supersession failed: %v", err)
	}

	if len(taskE.Spec) != len(taskD.Spec) {
		t.Errorf("spec should not grow between D->E with same findings: D=%d bytes, E=%d bytes", len(taskD.Spec), len(taskE.Spec))
	}
	if historyCountE := strings.Count(taskE.Spec, "## Research task history"); historyCountE != 1 {
		t.Errorf("task E should have exactly one history section, got %d", historyCountE)
	}
}

// TestResearchSupersessionUserHeadingPreservation verifies that a research
// assignment that happens to contain the literal text "## Research task history" (an
// ordinary Markdown heading a user could plausibly write) is preserved verbatim on
// supersession. The compaction marker is a generated HTML-comment sentinel that a
// user's Markdown can't collide with, so extractOriginalAssignment must not truncate
// at the user's own heading.
func TestResearchSupersessionUserHeadingPreservation(t *testing.T) {
	store, ctx, projID, _ := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	userSpec := `Research task description

## Prior research

We found some information here.

## Research task history

This is a section in the original assignment explaining prior history.
It happens to contain the same header as our generated marker.

Some more content here.`

	doc, err := store.CreateDocument(ctx, projID, "feature_spec", "test-doc-marker", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, projID, []TaskInput{
		{
			Title:        "research with marker in spec",
			Spec:         userSpec,
			DocumentID:   doc.ID,
			Model:        "haiku",
			Track:        "research",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil || len(tasks) == 0 {
		t.Fatalf("failed to create task: %v", err)
	}
	taskWithMarker := tasks[0].ID

	if _, err := store.PromoteTask(ctx, taskWithMarker); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskWithMarker, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskWithMarker, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	opus, _ := findResearchReviewTasks(t, store, ctx, projID, taskWithMarker, 1)
	findings := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"test finding","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", findings)

	replacement, err := store.SupersedeTask(ctx, taskWithMarker, ptrStr("opus"))
	if err != nil {
		t.Fatalf("supersession failed: %v", err)
	}

	if !strings.HasPrefix(replacement.Spec, userSpec) {
		t.Errorf("replacement spec should start with the full original user-authored spec, including its own '## Research task history' heading, but got:\n%s", replacement.Spec)
	}
	if !strings.Contains(replacement.Spec, "This is a section in the original assignment") {
		t.Errorf("user-authored content after the colliding heading was truncated")
	}

	// Second supersession, to prove the collision is avoided across chained rounds too.
	replacementObj, _ := store.GetTask(ctx, replacement.ID)
	conn := store.(*sqliteStore).conn
	if _, err := conn.ExecContext(ctx, "UPDATE task SET state = ? WHERE id = ?", "ready", replacement.ID); err != nil {
		t.Fatalf("failed to set replacement to ready: %v", err)
	}
	if _, err := store.ClaimTask(ctx, replacement.ID, "agent-1", replacementObj.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim replacement: %v", err)
	}
	if _, err := store.SubmitTask(ctx, replacement.ID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to resubmit replacement: %v", err)
	}
	opus2, _ := findResearchReviewTasks(t, store, ctx, projID, replacement.ID, 1)
	findings2 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"test finding","in_changed_text":true,"status":"still_open","prior_id":"f1"}
	]`)
	submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", findings2)

	replacement2, err := store.SupersedeTask(ctx, replacement.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("second supersession failed: %v", err)
	}
	if !strings.HasPrefix(replacement2.Spec, userSpec) {
		t.Errorf("second replacement spec should still start with the full original user-authored spec, but got:\n%s", replacement2.Spec)
	}
}

// TestResearchSupersessionDependencyPreservation verifies that dependency edges
// are properly preserved and re-pointed during research supersessions.
func TestResearchSupersessionDependencyPreservation(t *testing.T) {
	store, ctx, projID, taskA := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	taskAObj, _ := store.GetTask(ctx, taskA)
	extraTasks, err := store.CreateTasks(ctx, projID, []TaskInput{
		{
			Title:      "dependent task",
			Spec:       "test",
			DocumentID: taskAObj.DocumentID,
			Track:      "build",
		},
		{
			Title:      "upstream task",
			Spec:       "test",
			DocumentID: taskAObj.DocumentID,
			Track:      "build",
		},
	})
	if err != nil || len(extraTasks) != 2 {
		t.Fatalf("failed to create dependency fixture tasks: %v", err)
	}
	depTaskID := extraTasks[0].ID
	upstreamTaskID := extraTasks[1].ID

	conn := store.(*sqliteStore).conn
	if _, err := conn.Exec(`
		INSERT INTO task_dep (task_id, depends_on_id) VALUES (?, ?)
	`, depTaskID, taskA); err != nil {
		t.Fatalf("failed to add dependency: %v", err)
	}
	// taskA's own upstream dependency: must still be depended on by taskA's replacement.
	if _, err := conn.Exec(`
		INSERT INTO task_dep (task_id, depends_on_id) VALUES (?, ?)
	`, taskA, upstreamTaskID); err != nil {
		t.Fatalf("failed to add upstream dependency: %v", err)
	}

	var dep string
	err = conn.QueryRow(`SELECT depends_on_id FROM task_dep WHERE task_id = ?`, depTaskID).Scan(&dep)
	if err != nil || dep != taskA {
		t.Errorf("expected dependency on %s, got %v or %v", taskA, dep, err)
	}

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"finding one","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("supersession failed: %v", err)
	}

	err = conn.QueryRow(`SELECT depends_on_id FROM task_dep WHERE task_id = ?`, depTaskID).Scan(&dep)
	if err != nil || dep != taskB.ID {
		t.Errorf("expected dependency re-pointed to %s, got %v or %v", taskB.ID, dep, err)
	}

	var upstreamDep string
	err = conn.QueryRow(`SELECT depends_on_id FROM task_dep WHERE task_id = ?`, taskB.ID).Scan(&upstreamDep)
	if err != nil || upstreamDep != upstreamTaskID {
		t.Errorf("expected replacement to keep upstream dependency on %s, got %v or %v", upstreamTaskID, upstreamDep, err)
	}

	oldTask, _ := store.GetTask(ctx, taskA)
	if oldTask.State != "superseded" {
		t.Errorf("old task should be superseded, got %s", oldTask.State)
	}
	if oldTask.SupersededBy == nil || *oldTask.SupersededBy != taskB.ID {
		t.Errorf("old task should reference new task")
	}
}

// TestResearchSupersessionCarriesUnreviewedFindings verifies that a research
// replacement which is itself superseded before ever reaching a review round of its
// own does not silently drop the unresolved findings it was carrying forward from its
// predecessor. A gets a P1 reject with f1 and is superseded to B (B's spec carries
// f1). B is then superseded to C immediately, with no review round on B. C's spec
// must still carry f1, not just the bare history section.
func TestResearchSupersessionCarriesUnreviewedFindings(t *testing.T) {
	store, ctx, projID, taskA := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	if opus1 == nil {
		t.Fatalf("round 1: expected review task")
	}
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("first supersession failed: %v", err)
	}
	if !strings.Contains(taskB.Spec, `"id":"f1"`) {
		t.Fatalf("B's spec should carry forward unresolved finding f1, got: %s", taskB.Spec)
	}

	// B is superseded again without ever getting a review round of its own.
	taskC, err := store.SupersedeTask(ctx, taskB.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("second supersession failed: %v", err)
	}

	if !strings.Contains(taskC.Spec, `"id":"f1"`) {
		t.Errorf("C's spec should still carry unreviewed-forward finding f1, got: %s", taskC.Spec)
	}
	if !strings.Contains(taskC.Spec, "Unresolved findings from last review round") {
		t.Errorf("C's spec should contain the unresolved findings section, got: %s", taskC.Spec)
	}
}

// TestResearchSupersessionMidRoundKeepsPriorFindings verifies that superseding a
// research task while its latest review round has been spawned but not yet submitted
// (the normal "re-route a task that is stuck waiting in review" case for `odonian
// supersede`) does not drop the previous, completed round's unresolved findings.
// getUnresolvedFindingsFromLastRound's notion of "last round" must be the last round
// with a submitted review event, not merely the last round whose review tasks exist.
func TestResearchSupersessionMidRoundKeepsPriorFindings(t *testing.T) {
	store, ctx, projID, taskA := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	if opus1 == nil {
		t.Fatalf("round 1: expected review task")
	}
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	// Rework and resubmit, spawning round 2's review task, then supersede while it is
	// still pending: round 2 has no submitted findings of its own yet.
	resubmitResearchImplementTask(t, store, ctx, taskA)
	opus2, _ := findResearchReviewTasks(t, store, ctx, projID, taskA, 2)
	if opus2 == nil {
		t.Fatalf("round 2: expected review task to be spawned")
	}

	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("supersession failed: %v", err)
	}
	if !strings.Contains(taskB.Spec, `"id":"f1"`) {
		t.Errorf("B's spec should carry forward round 1's unresolved finding f1 even though round 2 is still pending, got: %s", taskB.Spec)
	}
}

// TestResearchSupersessionMidRoundReplacementKeepsCarriedFindings verifies the
// replacement-side half of the same bug: a replacement task that has reached its own
// pending (spawned, not yet submitted) review round is superseded again, and the
// finding it was carrying forward from its predecessor must not be dropped just
// because it now has review tasks of its own.
func TestResearchSupersessionMidRoundReplacementKeepsCarriedFindings(t *testing.T) {
	store, ctx, projID, taskA := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("first supersession failed: %v", err)
	}
	if !strings.Contains(taskB.Spec, `"id":"f1"`) {
		t.Fatalf("B's spec should carry forward unresolved finding f1, got: %s", taskB.Spec)
	}

	// B is claimed and resubmitted, spawning its own round 1 review task, which is
	// left pending (not yet reviewed) before B is superseded again.
	if _, err := store.PromoteTask(ctx, taskB.ID); err != nil {
		t.Fatalf("failed to promote B: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskB.ID, "agent-1", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim B: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskB.ID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#101"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to resubmit B: %v", err)
	}
	bOpus1, _ := findResearchReviewTasks(t, store, ctx, projID, taskB.ID, 1)
	if bOpus1 == nil {
		t.Fatalf("expected B's round 1 review task to be spawned")
	}

	taskC, err := store.SupersedeTask(ctx, taskB.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("second supersession failed: %v", err)
	}
	if !strings.Contains(taskC.Spec, `"id":"f1"`) {
		t.Errorf("C's spec should still carry forward finding f1 from B's pending round, got: %s", taskC.Spec)
	}
}

// TestResearchSupersessionCarriedFindingsIgnoreUserContent verifies that
// extractCarriedFindings, used when a replacement is superseded before its own review
// round, only recovers findings from the generated block after researchHistorySentinel
// and never parses a coincidental "**Structured findings (JSON):**" fenced block that
// happens to appear in the user-authored original assignment.
func TestResearchSupersessionCarriedFindingsIgnoreUserContent(t *testing.T) {
	store, ctx, projID, _ := newResearchTaskWithEscalationLadder(t, true, []string{"opus"})

	userSpec := "Research task description\n\n" +
		"**Structured findings (JSON):**\n```json\n" +
		`[{"id":"bogus-user-finding","severity":"P1","file":"z.md","line":1,"summary":"not a real finding","in_changed_text":true,"status":"new"}]` +
		"\n```\n\nMore of the assignment."

	doc, err := store.CreateDocument(ctx, projID, "feature_spec", "test-doc-carried", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, projID, []TaskInput{
		{
			Title:        "research with bogus findings block in spec",
			Spec:         userSpec,
			DocumentID:   doc.ID,
			Model:        "haiku",
			Track:        "research",
			ReviewModels: []string{"opus"},
		},
	})
	if err != nil || len(tasks) == 0 {
		t.Fatalf("failed to create task: %v", err)
	}
	taskA := tasks[0].ID

	if _, err := store.PromoteTask(ctx, taskA); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskA, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskA, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit task: %v", err)
	}

	opus1, _ := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	findings1 := json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"real finding","in_changed_text":true,"status":"new"}
	]`)
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)

	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("first supersession failed: %v", err)
	}

	// B is superseded again immediately, with no review round of its own, exercising
	// extractCarriedFindings against B's spec (which still contains the user's bogus
	// block ahead of the generated one).
	taskC, err := store.SupersedeTask(ctx, taskB.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("second supersession failed: %v", err)
	}
	if !strings.Contains(taskC.Spec, `"id":"f1"`) {
		t.Errorf("C's spec should carry forward the real finding f1, got: %s", taskC.Spec)
	}
	// The literal text "bogus-user-finding" legitimately survives inside the preserved,
	// verbatim original assignment. What must NOT happen is extractCarriedFindings
	// parsing it as a real carried finding and rendering it into the generated
	// "Unresolved findings" bullet list or structured-findings array.
	if strings.Contains(taskC.Spec, "- **bogus-user-finding**") {
		t.Errorf("C's spec should not render the user-authored bogus block as a carried finding, got: %s", taskC.Spec)
	}
	structuredIdx := strings.Index(taskC.Spec, researchHistorySentinel)
	if structuredIdx == -1 {
		t.Fatalf("expected generated compaction block in C's spec, got: %s", taskC.Spec)
	}
	if strings.Contains(taskC.Spec[structuredIdx:], "bogus-user-finding") {
		t.Errorf("C's generated block should not contain the user-authored bogus finding, got: %s", taskC.Spec)
	}
}

// setupTwoReviewerResearchRound1 creates a two-reviewer (opus, sonnet) research task
// whose round 1 is rejected by both reviewers: opus with P1 f1, sonnet with P1 g1.
func setupTwoReviewerResearchRound1(t *testing.T) (Store, context.Context, string, string) {
	t.Helper()
	store, ctx, projID, taskA := newResearchTaskWithEscalationLadder(t, false, []string{"opus", "sonnet"})
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	if opus1 == nil || sonnet1 == nil {
		t.Fatalf("round 1: expected both review tasks")
	}
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`))
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "reject", json.RawMessage(`[
		{"id":"g1","severity":"P1","file":"b.md","line":2,"summary":"misquoted statistic","in_changed_text":true,"status":"new"}
	]`))
	return store, ctx, projID, taskA
}

// TestResearchSupersessionPartialRoundKeepsPendingReviewersFindings verifies that
// superseding a research task while its latest round is only partly submitted keeps
// the pending reviewer's still-open findings from its own last submitted round, while
// the reviewer who did re-review has its latest report honored. "Last round" is per
// reviewer lineage, not per task.
func TestResearchSupersessionPartialRoundKeepsPendingReviewersFindings(t *testing.T) {
	t.Run("opus resolves f1, sonnet pending", func(t *testing.T) {
		store, ctx, projID, taskA := setupTwoReviewerResearchRound1(t)
		resubmitResearchImplementTask(t, store, ctx, taskA)
		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, taskA, 2)
		if opus2 == nil || sonnet2 == nil {
			t.Fatalf("round 2: expected both review tasks")
		}
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", json.RawMessage(`[
			{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"resolved","prior_id":"f1"}
		]`))

		taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
		if err != nil {
			t.Fatalf("supersession failed: %v", err)
		}
		if !strings.Contains(taskB.Spec, `"id":"g1"`) {
			t.Errorf("B's spec should carry sonnet's un-re-reviewed finding g1, got: %s", taskB.Spec)
		}
		if strings.Contains(taskB.Spec, `"id":"f1"`) {
			t.Errorf("B's spec should not carry f1, which opus resolved in round 2, got: %s", taskB.Spec)
		}
	})

	t.Run("sonnet re-reviews clean, opus pending", func(t *testing.T) {
		store, ctx, projID, taskA := setupTwoReviewerResearchRound1(t)
		resubmitResearchImplementTask(t, store, ctx, taskA)
		_, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, taskA, 2)
		if sonnet2 == nil {
			t.Fatalf("round 2: expected sonnet review task")
		}
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
		if err != nil {
			t.Fatalf("supersession failed: %v", err)
		}
		if !strings.Contains(taskB.Spec, `"id":"f1"`) {
			t.Errorf("B's spec should carry opus's un-re-reviewed finding f1, got: %s", taskB.Spec)
		}
		if strings.Contains(taskB.Spec, `"id":"g1"`) {
			t.Errorf("B's spec should not carry g1, which sonnet dropped in its round-2 review, got: %s", taskB.Spec)
		}
	})
}

// TestResearchSupersessionPartialRoundReplacementKeepsCarriedFindings is the
// replacement-side variant: B carries f1 (opus) and g1 (sonnet) from A. B's round 1
// has opus submit a clean review while sonnet is still pending, and B is superseded
// again. C must keep g1 (sonnet never re-reviewed it on B) and drop f1 (opus did).
func TestResearchSupersessionPartialRoundReplacementKeepsCarriedFindings(t *testing.T) {
	store, ctx, projID, taskA := setupTwoReviewerResearchRound1(t)

	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("first supersession failed: %v", err)
	}
	for _, id := range []string{"f1", "g1"} {
		if !strings.Contains(taskB.Spec, `"id":"`+id+`"`) {
			t.Fatalf("B's spec should carry %s, got: %s", id, taskB.Spec)
		}
	}
	if !strings.Contains(taskB.Spec, `"reviewers":[{"model":"sonnet","slot":`) {
		t.Errorf("B's carried findings should record the raising reviewer, got: %s", taskB.Spec)
	}

	if _, err := store.PromoteTask(ctx, taskB.ID); err != nil {
		t.Fatalf("failed to promote B: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskB.ID, "agent-1", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim B: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskB.ID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#101"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit B: %v", err)
	}
	bOpus1, bSonnet1 := findResearchReviewTasks(t, store, ctx, projID, taskB.ID, 1)
	if bOpus1 == nil || bSonnet1 == nil {
		t.Fatalf("expected B's round 1 review tasks")
	}
	submitResearchReview(t, store, ctx, bOpus1, "opus-reviewer", "approve", json.RawMessage(`[]`))

	taskC, err := store.SupersedeTask(ctx, taskB.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("second supersession failed: %v", err)
	}
	if !strings.Contains(taskC.Spec, `"id":"g1"`) {
		t.Errorf("C's spec should still carry g1, which sonnet has not re-reviewed on B, got: %s", taskC.Spec)
	}
	if strings.Contains(taskC.Spec, `"id":"f1"`) {
		t.Errorf("C's spec should not carry f1, which opus re-reviewed clean on B, got: %s", taskC.Spec)
	}
	if strings.Count(taskC.Spec, researchHistorySentinel) != 1 {
		t.Errorf("C's spec should have exactly one generated block, got: %s", taskC.Spec)
	}
}

// TestResearchSupersessionDedupedFindingKeepsAllReviewerLineages verifies that a
// finding raised identically by two reviewers, deduplicated into one carried record,
// keeps both reviewer lineages. When only one of those reviewers re-reviews the
// replacement clean while the other is still pending, the finding must still be
// carried forward, since the pending reviewer has not re-reviewed it.
func TestResearchSupersessionDedupedFindingKeepsAllReviewerLineages(t *testing.T) {
	store, ctx, projID, taskA := newResearchTaskWithEscalationLadder(t, false, []string{"opus", "sonnet"})
	opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, taskA, 1)
	if opus1 == nil || sonnet1 == nil {
		t.Fatalf("round 1: expected both review tasks")
	}
	submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", json.RawMessage(`[
		{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`))
	submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "reject", json.RawMessage(`[
		{"id":"g1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}
	]`))

	taskB, err := store.SupersedeTask(ctx, taskA, ptrStr("opus"))
	if err != nil {
		t.Fatalf("first supersession failed: %v", err)
	}
	if n := strings.Count(taskB.Spec, "- **"); n != 1 {
		t.Errorf("B's spec should list the duplicated finding once, got %d bullets: %s", n, taskB.Spec)
	}
	carried := extractCarriedFindings(taskB.Spec)
	if len(carried) != 1 {
		t.Fatalf("B should carry exactly one deduplicated finding, got %d: %s", len(carried), taskB.Spec)
	}
	models := map[string]bool{}
	for _, r := range carried[0].Reviewers {
		models[r.Model] = true
	}
	if !models["opus"] || !models["sonnet"] || len(carried[0].Reviewers) != 2 {
		t.Fatalf("deduplicated finding should keep both reviewer lineages, got %+v", carried[0].Reviewers)
	}

	if _, err := store.PromoteTask(ctx, taskB.ID); err != nil {
		t.Fatalf("failed to promote B: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskB.ID, "agent-1", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim B: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskB.ID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#101"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit B: %v", err)
	}
	bOpus1, bSonnet1 := findResearchReviewTasks(t, store, ctx, projID, taskB.ID, 1)
	if bOpus1 == nil || bSonnet1 == nil {
		t.Fatalf("expected B's round 1 review tasks")
	}
	submitResearchReview(t, store, ctx, bOpus1, "opus-reviewer", "approve", json.RawMessage(`[]`))

	taskC, err := store.SupersedeTask(ctx, taskB.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("second supersession failed: %v", err)
	}
	carriedC := extractCarriedFindings(taskC.Spec)
	if len(carriedC) != 1 || carriedC[0].Summary != "fabricated source" {
		t.Fatalf("C should still carry the finding sonnet has not re-reviewed, got: %s", taskC.Spec)
	}
	if len(carriedC[0].Reviewers) != 1 || carriedC[0].Reviewers[0].Model != "sonnet" {
		t.Errorf("C's carried finding should keep only the pending sonnet lineage, got %+v", carriedC[0].Reviewers)
	}

	// Once sonnet also re-reviews clean on C, the finding is settled and dropped.
	if _, err := store.PromoteTask(ctx, taskC.ID); err != nil {
		t.Fatalf("failed to promote C: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskC.ID, "agent-1", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim C: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskC.ID, "agent-1", "Reworked again", nil, []LinkInput{{Kind: "pr", Value: "#102"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit C: %v", err)
	}
	_, cSonnet1 := findResearchReviewTasks(t, store, ctx, projID, taskC.ID, 1)
	if cSonnet1 == nil {
		t.Fatalf("expected C's round 1 sonnet review task")
	}
	submitResearchReview(t, store, ctx, cSonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	taskD, err := store.SupersedeTask(ctx, taskC.ID, ptrStr("opus"))
	if err != nil {
		t.Fatalf("third supersession failed: %v", err)
	}
	if carriedD := extractCarriedFindings(taskD.Spec); len(carriedD) != 0 {
		t.Errorf("D should carry nothing once both reviewers re-reviewed clean, got: %s", taskD.Spec)
	}
}

// resubmitResearchImplementTaskWithDisputes is resubmitResearchImplementTask but
// carries a disputes payload on the rework submission, per
// docs/features/research-track.md section 5.
func resubmitResearchImplementTaskWithDisputes(t *testing.T, store Store, ctx context.Context, parentID string, disputes json.RawMessage) (TaskWithDepsAndLinks, error) {
	t.Helper()
	if _, err := store.ClaimTask(ctx, parentID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim parent for resubmit: %v", err)
	}
	return store.SubmitTaskWithDisputes(ctx, parentID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget, nil, disputes)
}

// TestResearchDisputes_ValidDisputePersistedAndDoesNotAlterFinding verifies that a
// well-formed dispute of a real round-1 finding is accepted and recorded, without
// altering the original review event's finding (docs/features/research-track.md
// section 5: a dispute never overturns a finding on its own).
func TestResearchDisputes_ValidDisputePersistedAndDoesNotAlterFinding(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil || sonnet == nil {
		t.Fatalf("expected both review tasks to be ready")
	}

	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"overstates source","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 1 to be rejected (parent back to ready), got %s", parent.State)
	}

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"the source supports the claim as written"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("expected dispute submission to succeed, got: %v", err)
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	var disputeEvent *Event
	var opusReviewEvent *Event
	for i := range events {
		e := &events[i]
		if e.Kind == "submit" && e.Disputes != nil {
			disputeEvent = e
		}
		if e.Kind == "review" && e.Findings != nil {
			var findings []Finding
			if err := json.Unmarshal(*e.Findings, &findings); err == nil {
				for _, f := range findings {
					if f.ID == "f1" {
						opusReviewEvent = e
					}
				}
			}
		}
	}
	if disputeEvent == nil {
		t.Fatalf("expected a submit event with disputes recorded")
	}
	var storedDisputes []Dispute
	if err := json.Unmarshal(*disputeEvent.Disputes, &storedDisputes); err != nil {
		t.Fatalf("failed to unmarshal stored disputes: %v", err)
	}
	if len(storedDisputes) != 1 || storedDisputes[0].FindingID != "f1" || storedDisputes[0].Evidence == "" {
		t.Fatalf("unexpected stored disputes: %+v", storedDisputes)
	}

	if opusReviewEvent == nil {
		t.Fatalf("expected original opus review event with f1 still present")
	}
	var origFindings []Finding
	if err := json.Unmarshal(*opusReviewEvent.Findings, &origFindings); err != nil {
		t.Fatalf("failed to unmarshal original findings: %v", err)
	}
	if origFindings[0].Status != "new" || origFindings[0].Severity != "P2" {
		t.Errorf("dispute must not alter the original finding, got %+v", origFindings[0])
	}

	// The dispute must reach opus's (the raiser's) round-2 review spec, and not
	// sonnet's, since sonnet never raised f1.
	opusRound2, sonnetRound2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusRound2 == nil || sonnetRound2 == nil {
		t.Fatalf("expected round-2 review tasks for both reviewers")
	}
	if !strings.Contains(opusRound2.Spec, "Prior Review Round") || !strings.Contains(opusRound2.Spec, "f1") {
		t.Errorf("expected opus round-2 spec to include the dispute, got: %s", opusRound2.Spec)
	}
	if strings.Contains(sonnetRound2.Spec, "Prior Review Round") {
		t.Errorf("expected sonnet round-2 spec to have no dispute context, got: %s", sonnetRound2.Spec)
	}
}

// TestResearchDisputes_UnknownFindingID verifies that disputing a finding_id never
// raised in the round being reworked is rejected.
func TestResearchDisputes_UnknownFindingID(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"does-not-exist","evidence":"e"}]`)
	_, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "UNKNOWN_FINDING_ID" {
		t.Fatalf("expected UNKNOWN_FINDING_ID validation error, got: %v", err)
	}
}

// TestResearchDisputes_AmbiguousFindingID verifies that when two reviewers both use
// the same finding_id in the same round, disputing it is rejected as ambiguous
// rather than silently resolved against whichever reviewer's finding is scanned
// first.
func TestResearchDisputes_AmbiguousFindingID(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	sameID := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", sameID)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "reject", sameID)

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	_, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "AMBIGUOUS_FINDING_ID" {
		t.Fatalf("expected AMBIGUOUS_FINDING_ID validation error, got: %v", err)
	}
}

// TestResearchDisputes_EmptyEvidence verifies that a dispute with empty (or
// whitespace-only) evidence is rejected.
func TestResearchDisputes_EmptyEvidence(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"   "}]`)
	_, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "INVALID_DISPUTES" {
		t.Fatalf("expected INVALID_DISPUTES validation error, got: %v", err)
	}
}

// TestResearchDisputes_DuplicateWithinPayload verifies that disputing the same
// finding_id twice within one submission is rejected.
func TestResearchDisputes_DuplicateWithinPayload(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e1"},{"finding_id":"f1","evidence":"e2"}]`)
	_, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "INVALID_DISPUTES" {
		t.Fatalf("expected INVALID_DISPUTES validation error, got: %v", err)
	}
}

// TestResearchDisputes_DuplicateAcrossRounds verifies that a finding_id already
// disputed on an earlier rework round cannot be disputed a second time, even when
// the raising reviewer reuses the same id in a later round (e.g. reporting it
// still_open).
func TestResearchDisputes_DuplicateAcrossRounds(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"first"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("expected first dispute to succeed, got: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusR2 == nil || sonnetR2 == nil {
		t.Fatalf("expected round-2 review tasks")
	}
	stillOpen := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", stillOpen)
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 to be rejected (still_open blocks), got %s", parent.State)
	}

	disputesAgain := json.RawMessage(`[{"finding_id":"f1","evidence":"second attempt"}]`)
	_, err = resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputesAgain)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "DUPLICATE_DISPUTE" {
		t.Fatalf("expected DUPLICATE_DISPUTE validation error, got: %v", err)
	}
}

// TestResearchDisputes_NotAllowedOnNonResearchTrack verifies that a disputes payload
// on a build-track implement submission is rejected, and never affects ordinary
// build rework.
func TestResearchDisputes_NotAllowedOnNonResearchTrack(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Build feature", Spec: "Do it", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"opus"}},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	_, err = store.SubmitTaskWithDisputes(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 8, nil, nil, testUnlimitedResearchBudget, nil, disputes)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "DISPUTES_NOT_ALLOWED" {
		t.Fatalf("expected DISPUTES_NOT_ALLOWED validation error, got: %v", err)
	}
}

// TestResearchDisputes_NoPriorRound verifies that a research task's first implement
// submission, with no prior review round, rejects a disputes payload.
func TestResearchDisputes_NoPriorRound(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	escalate := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Verify claims", Spec: "Verify", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"opus"}, Track: "research", Escalate: &escalate},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	_, err = store.SubmitTaskWithDisputes(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 8, nil, nil, testUnlimitedResearchBudget, nil, disputes)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "NO_PRIOR_ROUND" {
		t.Fatalf("expected NO_PRIOR_ROUND validation error, got: %v", err)
	}
}

// TestResearchDisputes_NotAllowedOnReviewKindTask verifies that a disputes payload
// on a review-kind task submission is rejected, regardless of track.
func TestResearchDisputes_NotAllowedOnReviewKindTask(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil || sonnet == nil {
		t.Fatalf("expected both review tasks to be ready")
	}
	if _, err := store.ClaimTask(ctx, opus.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}
	verdict := "approve"
	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	_, err := store.SubmitTaskWithDisputes(ctx, opus.ID, "opus-reviewer", "notes", &verdict, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget, json.RawMessage(`[]`), disputes)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "DISPUTES_NOT_ALLOWED" {
		t.Fatalf("expected DISPUTES_NOT_ALLOWED validation error, got: %v", err)
	}
}

// TestResearchDisputes_BuildReworkSpecUnaffected verifies that ordinary build-track
// rework (no disputes involved anywhere in the flow) produces a round-2 review spec
// with no "Prior Review Round" section, confirming the disputes feature never
// changes build/design behavior.
func TestResearchDisputes_BuildReworkSpecUnaffected(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Build feature", Spec: "Do it", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"opus"}},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var reviewTaskID string
	for _, tk := range allTasks {
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == taskID {
			reviewTaskID = tk.ID
		}
	}
	if reviewTaskID == "" {
		t.Fatalf("expected a review task to be spawned")
	}
	if _, err := store.ClaimTask(ctx, reviewTaskID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim review task: %v", err)
	}
	verdict := "reject"
	if _, err := store.SubmitTask(ctx, reviewTaskID, "opus-reviewer", "needs work", &verdict, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit review: %v", err)
	}

	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected build task rejected round to return to ready, got %s", parent.State)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim for rework: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Reworked", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to resubmit: %v", err)
	}

	allTasks, err = store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var round2Spec string
	found := false
	for _, tk := range allTasks {
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == taskID && tk.ReviewRound == 2 {
			round2Spec = tk.Spec
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a round-2 review task")
	}
	if strings.Contains(round2Spec, "Prior Review Round") {
		t.Errorf("expected no dispute context in build-track review spec, got: %s", round2Spec)
	}
}

// TestResearchDisputes_StaleDisputeDoesNotLeakIntoLaterRound verifies that a
// round-3 rework with NO disputes does not carry a round-1 dispute forward into the
// round-3 review spec: only the triggering (latest) submit event's own disputes are
// ever surfaced, never an earlier round's.
func TestResearchDisputes_StaleDisputeDoesNotLeakIntoLaterRound(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})
	opus, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil {
		t.Fatalf("expected round-1 opus review task")
	}
	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)

	// Round-2 rework disputes f1.
	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e1"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("expected round-2 dispute to succeed, got: %v", err)
	}

	opusR2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusR2 == nil {
		t.Fatalf("expected round-2 opus review task")
	}
	if !strings.Contains(opusR2.Spec, "f1") {
		t.Fatalf("expected round-2 spec to include the round-1 dispute")
	}
	// Opus maintains the finding (still_open), so round 2 is rejected too.
	stillOpen := json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", stillOpen)

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 to be rejected (still_open blocks), got %s", parent.State)
	}

	// Round-3 rework submits ordinary rework with NO disputes.
	resubmitResearchImplementTask(t, store, ctx, parentID)

	opusR3, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 3)
	if opusR3 == nil {
		t.Fatalf("expected round-3 opus review task")
	}
	if strings.Contains(opusR3.Spec, "Prior Review Round") {
		t.Errorf("expected no stale dispute context in round-3 spec (round 3 submitted no disputes), got: %s", opusR3.Spec)
	}
}

// TestResearchDisputes_DifferentReviewerReusingIDIsNotADuplicate verifies that
// disputing a finding_id is scoped to the specific reviewer lineage and prior_id
// chain it resolved against, not the bare finding_id string: a different reviewer's
// unrelated finding that happens to reuse an already-disputed id is a genuinely new
// finding and must not be rejected as DUPLICATE_DISPUTE.
func TestResearchDisputes_DifferentReviewerReusingIDIsNotADuplicate(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil || sonnet == nil {
		t.Fatalf("expected both review tasks to be ready")
	}

	// Round 1: opus raises f1 and it's disputed; sonnet has nothing to report.
	opusF1 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"overstates source","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", opusF1)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"the source supports the claim as written"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("expected round-1 dispute to succeed, got: %v", err)
	}

	// Round 2: opus reports its finding resolved under a new id (r1, prior_id f1).
	// sonnet raises its OWN, unrelated finding, and happens to reuse the id "f1" —
	// reviewers pick their own ids independently, so this collision is expected.
	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusR2 == nil || sonnetR2 == nil {
		t.Fatalf("expected round-2 review tasks")
	}
	opusResolved := json.RawMessage(`[{"id":"r1","severity":"P2","file":"a.md","line":3,"summary":"overstates source","in_changed_text":false,"status":"resolved","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "approve", opusResolved)
	sonnetF1 := json.RawMessage(`[{"id":"f1","severity":"P2","file":"b.md","line":9,"summary":"a different, unrelated defect","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "reject", sonnetF1)

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 to be rejected (sonnet's new finding blocks), got %s", parent.State)
	}

	// Disputing sonnet's round-2 "f1" must succeed: it is a different reviewer's
	// different finding, never disputed before, even though the raw id matches
	// opus's already-disputed round-1 "f1".
	disputesAgain := json.RawMessage(`[{"finding_id":"f1","evidence":"b.md line 9 is unrelated to a.md"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputesAgain); err != nil {
		t.Fatalf("expected sonnet's new finding to be disputable, got: %v", err)
	}
}

// TestResearchDisputes_MaintainedFindingCannotBeDisputedAgainUnderNewID verifies
// that once a finding has been disputed, the same finding carried forward under a new
// id in a later round (still_open, linked by prior_id) cannot be disputed again: the
// duplicate check must follow the reviewer's prior_id chain, not just the bare id.
func TestResearchDisputes_MaintainedFindingCannotBeDisputedAgainUnderNewID(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithReviewers(t, false, []string{"opus"})
	opus, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil {
		t.Fatalf("expected round-1 opus review task")
	}

	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"first"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("expected round-1 dispute to succeed, got: %v", err)
	}

	// Round 2: opus maintains the same finding under a new id, linked by prior_id.
	opusR2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusR2 == nil {
		t.Fatalf("expected round-2 opus review task")
	}
	maintained := json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", maintained)

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 to be rejected (still_open blocks), got %s", parent.State)
	}

	// Disputing "f2" must fail: it is the same finding chain as the already-disputed
	// "f1", just carried forward under a new id.
	disputesAgain := json.RawMessage(`[{"finding_id":"f2","evidence":"second attempt"}]`)
	_, err = resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputesAgain)
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != "DUPLICATE_DISPUTE" {
		t.Fatalf("expected DUPLICATE_DISPUTE validation error, got: %v", err)
	}
}

// newResearchTaskWithAdjudicator is newResearchTaskWithReviewers with
// ODONIAN_RESEARCH_ADJUDICATOR configured, for docs/features/research-track.md
// section 5 adjudication tests.
func newResearchTaskWithAdjudicator(t *testing.T, reviewModels []string, adjudicator string) (Store, context.Context, string, string) {
	t.Helper()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithResearchAdjudicator(adjudicator))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	escalate := false
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{
			Title:        "Verify claims",
			Spec:         "Verify the claims in the doc",
			DocumentID:   doc.ID,
			Model:        "haiku",
			ReviewModels: reviewModels,
			Track:        "research",
			Escalate:     &escalate,
		},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID

	if _, err = store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err = store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	if _, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit implement task: %v", err)
	}

	return store, ctx, proj.ID, taskID
}

// findAdjudicationTasks returns every section 5 adjudication task targeting parentID,
// identified by their distinctive title: spawnAdjudicationTask always titles them
// "Adjudicate disputed finding <id> [...]".
func findAdjudicationTasks(t *testing.T, store Store, ctx context.Context, projID, parentID string) []Task {
	t.Helper()
	allTasks, err := store.ListTasks(ctx, projID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var out []Task
	for _, tk := range allTasks {
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == parentID && strings.HasPrefix(tk.Title, "Adjudicate disputed finding") {
			out = append(out, tk)
		}
	}
	return out
}

// submitAdjudication claims and submits an adjudication task with the given verdict:
// "approve" overturns the disputed finding, "reject" upholds it. Adjudication tasks
// are track=research review tasks, so submission still requires a findings array, even
// though it's unused.
func submitAdjudication(t *testing.T, store Store, ctx context.Context, task *Task, verdict string) {
	t.Helper()
	if _, err := store.ClaimTask(ctx, task.ID, "adjudicator-1", task.Model, 5*time.Minute); err != nil {
		t.Fatalf("failed to claim adjudication task %s: %v", task.ID, err)
	}
	v := verdict
	if _, err := store.SubmitTask(ctx, task.ID, "adjudicator-1", "ruling", &v, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget, json.RawMessage(`[]`)); err != nil {
		t.Fatalf("failed to submit adjudication task %s: %v", task.ID, err)
	}
}

// countEventsOfKind counts events of the given kind on taskID.
func countEventsOfKind(t *testing.T, store Store, ctx context.Context, taskID, kind string) int {
	t.Helper()
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	n := 0
	for _, e := range events {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// TestResearchAdjudication_DefersRoundWhilePending verifies that once a worker-disputed
// finding is maintained by its reviewer and a valid adjudicator is configured, the
// server spawns exactly one adjudication task scoped to that finding (with the
// finding's summary and the worker's evidence in its spec) and leaves the round
// entirely undecided — no state change on the parent — until the adjudicator rules.
func TestResearchAdjudication_DefersRoundWhilePending(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil || sonnet == nil {
		t.Fatalf("expected round-1 review tasks")
	}
	blockingFinding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"overstates the source","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 1 rejected (parent back to ready), got %s", parent.State)
	}

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"the cited passage supports the claim as written"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("expected dispute submission to succeed, got: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusR2 == nil || sonnetR2 == nil {
		t.Fatalf("expected round-2 review tasks")
	}
	maintained := json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"overstates the source","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", maintained)
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "review" {
		t.Fatalf("expected round 2 to be deferred pending adjudication (parent stays in review), got %s", parent.State)
	}

	adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
	if len(adjTasks) != 1 {
		t.Fatalf("expected exactly one adjudication task, got %d", len(adjTasks))
	}
	adj := adjTasks[0]
	if adj.Model != "haiku" {
		t.Errorf("expected adjudication task assigned to configured adjudicator haiku, got %s", adj.Model)
	}
	if adj.State != "ready" {
		t.Errorf("expected adjudication task ready to claim, got %s", adj.State)
	}
	if !strings.Contains(adj.Spec, "overstates the source") || !strings.Contains(adj.Spec, "the cited passage supports the claim as written") {
		t.Errorf("expected adjudication task spec to carry the finding and the worker's evidence, got: %s", adj.Spec)
	}

	if n := countEventsOfKind(t, store, ctx, parentID, "research_round_rejected"); n != 1 {
		t.Errorf("expected round 2 not yet decided (still 1 rejected round from round 1), got %d", n)
	}
}

// TestResearchAdjudication_Overturned verifies that when the adjudicator overturns a
// maintained disputed finding (verdict approve), the finding no longer blocks and,
// with nothing else blocking, the round is approved.
func TestResearchAdjudication_Overturned(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"resolves it"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
	if len(adjTasks) != 1 {
		t.Fatalf("expected exactly one adjudication task, got %d", len(adjTasks))
	}
	submitAdjudication(t, store, ctx, &adjTasks[0], "approve")

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected the overturned finding to no longer block, and nothing else blocking, got %s", parent.State)
	}
}

// TestResearchAdjudication_Upheld verifies that when the adjudicator upholds a
// maintained disputed finding (verdict reject), the finding still blocks and the round
// is rejected, exactly as an ordinary still_open finding would.
func TestResearchAdjudication_Upheld(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"does not resolve it"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
	if len(adjTasks) != 1 {
		t.Fatalf("expected exactly one adjudication task, got %d", len(adjTasks))
	}
	submitAdjudication(t, store, ctx, &adjTasks[0], "reject")

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected the upheld finding to keep blocking (round 2 rejected), got %s", parent.State)
	}
	if n := countEventsOfKind(t, store, ctx, parentID, "research_round_rejected"); n != 2 {
		t.Errorf("expected exactly 2 rejected rounds recorded (round 1 and round 2, no double count), got %d", n)
	}
}

// TestResearchAdjudication_MultipleSimultaneousDisputes verifies that two findings
// disputed in the same round, raised by different reviewers and both maintained, each
// get their own adjudication task, and the round waits for both rulings before it is
// decided. A mixed outcome (one overturned, one upheld) still fails the round: an
// adjudicator's ruling is binding only for the one finding it rules on, and any other
// blocking finding still determines the round on its own.
func TestResearchAdjudication_MultipleSimultaneousDisputes(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"fa","severity":"P2","file":"a.md","line":3,"summary":"claim a","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "reject", json.RawMessage(`[{"id":"fb","severity":"P2","file":"b.md","line":9,"summary":"claim b","in_changed_text":true,"status":"new"}]`))

	disputes := json.RawMessage(`[{"finding_id":"fa","evidence":"a resolves"},{"finding_id":"fb","evidence":"b resolves"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"fa2","severity":"P2","file":"a.md","line":3,"summary":"claim a","in_changed_text":false,"status":"still_open","prior_id":"fa"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "reject", json.RawMessage(`[{"id":"fb2","severity":"P2","file":"b.md","line":9,"summary":"claim b","in_changed_text":false,"status":"still_open","prior_id":"fb"}]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "review" {
		t.Fatalf("expected round 2 deferred pending both adjudications, got %s", parent.State)
	}

	adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
	if len(adjTasks) != 2 {
		t.Fatalf("expected exactly two adjudication tasks, got %d", len(adjTasks))
	}
	var adjA, adjB *Task
	for i := range adjTasks {
		if strings.Contains(adjTasks[i].Title, "fa2") {
			adjA = &adjTasks[i]
		}
		if strings.Contains(adjTasks[i].Title, "fb2") {
			adjB = &adjTasks[i]
		}
	}
	if adjA == nil || adjB == nil {
		t.Fatalf("expected one adjudication task per disputed finding, got titles: %q, %q", adjTasks[0].Title, adjTasks[1].Title)
	}

	// Resolving only one of the two must not decide the round yet.
	submitAdjudication(t, store, ctx, adjA, "approve")
	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "review" {
		t.Fatalf("expected round to remain deferred with one adjudication still pending, got %s", parent.State)
	}

	// The second ruling upholds its finding: the round still fails, on that finding
	// alone, even though the other was overturned.
	submitAdjudication(t, store, ctx, adjB, "reject")
	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 rejected by the upheld finding alone, got %s", parent.State)
	}
	if n := countEventsOfKind(t, store, ctx, parentID, "research_round_rejected"); n != 2 {
		t.Errorf("expected exactly 2 rejected rounds recorded (round 1 and round 2, no double count), got %d", n)
	}
}

// TestResearchAdjudication_DuplicateSpawnPrevention verifies that spawning an
// adjudication task for the same disputed finding twice — simulating a retried
// aggregation call — creates exactly one task rather than erroring or duplicating.
func TestResearchAdjudication_DuplicateSpawnPrevention(t *testing.T) {
	store, ctx, _, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	ss := store.(*sqliteStore)

	anchor := Dispute{FindingID: "f1", Evidence: "cited source text", Round: 1, Lineage: researchReviewerLineage("opus", 0)}
	finding := Finding{ID: "f1", Severity: "P2", File: "a.md", Line: 3, Summary: "x", Status: "still_open"}

	tx, err := ss.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin tx: %v", err)
	}
	defer tx.Rollback()

	now := nowTimestamp()
	if err := ss.spawnAdjudicationTask(ctx, tx, parentID, parent.ProjectID, parent.DocumentID, parent.Track, anchor, finding, 2, now); err != nil {
		t.Fatalf("first spawn failed: %v", err)
	}
	if err := ss.spawnAdjudicationTask(ctx, tx, parentID, parent.ProjectID, parent.DocumentID, parent.Track, anchor, finding, 2, now); err != nil {
		t.Fatalf("retried spawn for the same disputed finding must not error, got: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("failed to commit: %v", err)
	}

	var count int
	if err := ss.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM task WHERE target_task_id = ? AND adjudicate_finding_id = ?`, parentID, "f1").Scan(&count); err != nil {
		t.Fatalf("failed to count adjudication tasks: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one adjudication task after the retried spawn, got %d", count)
	}
}

// TestResearchAdjudication_AbsentAdjudicatorKeepsBlocking verifies that with no
// ODONIAN_RESEARCH_ADJUDICATOR configured, a maintained disputed finding stays
// blocking, the round is decided immediately (never deferred), and the parent gets an
// explicit event recording why adjudication did not run.
func TestResearchAdjudication_AbsentAdjudicatorKeepsBlocking(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 decided immediately (no adjudicator to wait on), got %s", parent.State)
	}
	if got := findAdjudicationTasks(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no adjudication task without a configured adjudicator, got %d", len(got))
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "research_adjudication_unavailable" && e.Note != nil && strings.Contains(*e.Note, "not configured") && strings.Contains(*e.Note, "f1") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an explicit research_adjudication_unavailable event naming the finding, got events: %+v", events)
	}
}

// TestResearchAdjudication_InvalidAdjudicatorNotInAllowlist verifies that a configured
// adjudicator outside the model allowlist is treated as unavailable (defense in depth;
// ODONIAN_RESEARCH_ADJUDICATOR is also validated against the allowlist at startup).
func TestResearchAdjudication_InvalidAdjudicatorNotInAllowlist(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "not-a-real-model")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 decided immediately (adjudicator not allowlisted), got %s", parent.State)
	}
	if got := findAdjudicationTasks(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no adjudication task spawned for a non-allowlisted adjudicator, got %d", len(got))
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "research_adjudication_unavailable" && e.Note != nil && strings.Contains(*e.Note, "not in the model allowlist") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an explicit research_adjudication_unavailable event citing the allowlist, got events: %+v", events)
	}
}

// TestResearchAdjudication_AdjudicatorSameAsReviewer verifies that a configured
// adjudicator equal to one of the task's two reviewers is treated as unavailable
// (decision 2: the adjudicator must differ from both reviewers).
func TestResearchAdjudication_AdjudicatorSameAsReviewer(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "opus")

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 decided immediately (adjudicator is one of the reviewers), got %s", parent.State)
	}
	if got := findAdjudicationTasks(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no adjudication task spawned when the adjudicator is one of the reviewers, got %d", len(got))
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "research_adjudication_unavailable" && e.Note != nil && strings.Contains(*e.Note, "one of this task's reviewers") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an explicit research_adjudication_unavailable event citing the reviewer conflict, got events: %+v", events)
	}
}

// TestResearchAdjudication_AdjudicatorSameAsDefaultReviewer verifies that the
// adjudicator-differs-from-reviewers check (decision 2) also catches a conflict when
// the task has no explicit ReviewModels: SubmitTask defaults an empty review_models to
// ["opus"] when spawning the round's review tasks, so the adjudicator check must apply
// that same default rather than comparing against an empty list, which would pass
// vacuously and let the raising reviewer adjudicate its own maintained finding.
func TestResearchAdjudication_AdjudicatorSameAsDefaultReviewer(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, nil, "opus")

	opus, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil {
		t.Fatalf("expected a default opus review task in round 1")
	}
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":true,"status":"new"}]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"e"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	opusR2, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opusR2 == nil {
		t.Fatalf("expected a default opus review task in round 2")
	}
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f2","severity":"P2","file":"a.md","line":3,"summary":"x","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 decided immediately (adjudicator equals the default reviewer), got %s", parent.State)
	}
	if got := findAdjudicationTasks(t, store, ctx, projID, parentID); len(got) != 0 {
		t.Errorf("expected no adjudication task spawned when the adjudicator equals the default reviewer, got %d", len(got))
	}

	events, err := store.ListEvents(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "research_adjudication_unavailable" && e.Note != nil && strings.Contains(*e.Note, "one of this task's reviewers") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an explicit research_adjudication_unavailable event citing the reviewer conflict, got events: %+v", events)
	}
}

// TestResearchAdjudication_OverturnPersistsAcrossLaterCarriedForwardRounds verifies
// that once an adjudicator overturns a maintained disputed finding, the ruling stays
// binding for that finding's chain no matter how many further rounds the same
// reviewer (unaware of the ruling, since the review prompt itself is unchanged in this
// milestone) carries it forward under a new id — and that no second adjudication task
// is ever spawned for it.
func TestResearchAdjudication_OverturnPersistsAcrossLaterCarriedForwardRounds(t *testing.T) {
	store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

	// Round 1: opus raises f1 (later disputed and overturned); sonnet raises an
	// unrelated fS that stays open into round 2, so round 2 still fails on its own
	// merits even after f1 is overturned, giving us a round 3 to carry f1 forward into.
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"claim a","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "reject", json.RawMessage(`[{"id":"fs","severity":"P2","file":"b.md","line":9,"summary":"claim b","in_changed_text":true,"status":"new"}]`))

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"resolves it"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("dispute submission failed: %v", err)
	}

	// Round 2: opus maintains f1 (disputed) as f1b; sonnet's fs is still unresolved.
	opusR2, sonnetR2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	submitResearchReview(t, store, ctx, opusR2, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1b","severity":"P2","file":"a.md","line":3,"summary":"claim a","in_changed_text":false,"status":"still_open","prior_id":"f1"}]`))
	submitResearchReview(t, store, ctx, sonnetR2, "sonnet-reviewer", "reject", json.RawMessage(`[{"id":"fsb","severity":"P2","file":"b.md","line":9,"summary":"claim b","in_changed_text":false,"status":"still_open","prior_id":"fs"}]`))

	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "review" {
		t.Fatalf("expected round 2 deferred pending adjudication of f1b, got %s", parent.State)
	}

	adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
	if len(adjTasks) != 1 {
		t.Fatalf("expected exactly one adjudication task, got %d", len(adjTasks))
	}
	submitAdjudication(t, store, ctx, &adjTasks[0], "approve")

	// f1b is overturned, but sonnet's fsb is still open on its own: round 2 fails.
	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected round 2 rejected by fsb alone (f1b overturned), got %s", parent.State)
	}
	if n := countEventsOfKind(t, store, ctx, parentID, "research_round_rejected"); n != 2 {
		t.Errorf("expected exactly 2 rejected rounds recorded, got %d", n)
	}

	// Round 3: opus (unaware of the ruling) carries f1 forward again as f1c; sonnet
	// fixes fs and reports it resolved.
	resubmitResearchImplementTask(t, store, ctx, parentID)
	opusR3, sonnetR3 := findResearchReviewTasks(t, store, ctx, projID, parentID, 3)
	if opusR3 == nil || sonnetR3 == nil {
		t.Fatalf("expected round-3 review tasks")
	}
	submitResearchReview(t, store, ctx, opusR3, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1c","severity":"P2","file":"a.md","line":3,"summary":"claim a","in_changed_text":false,"status":"still_open","prior_id":"f1b"}]`))
	submitResearchReview(t, store, ctx, sonnetR3, "sonnet-reviewer", "approve", json.RawMessage(`[{"id":"fsc","severity":"P2","file":"b.md","line":9,"summary":"claim b","in_changed_text":false,"status":"resolved","prior_id":"fsb"}]`))

	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("expected round 3 approved: fsb fixed, f1c still covered by the round-2 overturn ruling, got %s", parent.State)
	}
	if n := countEventsOfKind(t, store, ctx, parentID, "research_round_rejected"); n != 2 {
		t.Errorf("expected still exactly 2 rejected rounds (round 3 passed), got %d", n)
	}
	if got := findAdjudicationTasks(t, store, ctx, projID, parentID); len(got) != 1 {
		t.Errorf("expected no new adjudication task for f1c (already ruled on via its chain), got %d total", len(got))
	}
}

// TestResearchAdjudication_BuildTrackNeverAdjudicates verifies that a build-track
// task's review round is unaffected by adjudication logic even when a research
// adjudicator is configured: build aggregation still uses the plain approve/reject
// verdict tally, and no adjudication task is ever spawned for it.
func TestResearchAdjudication_BuildTrackNeverAdjudicates(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
		WithResearchAdjudicator("opus"))
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()

	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Build feature", Spec: "Do it", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"opus", "sonnet"}, Track: "build"},
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	taskID := tasks[0].ID
	if _, err := store.PromoteTask(ctx, taskID); err != nil {
		t.Fatalf("failed to promote: %v", err)
	}
	if _, err := store.ClaimTask(ctx, taskID, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	if _, err := store.SubmitTask(ctx, taskID, "agent-1", "Impl", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	var opusRev, sonnetRev *Task
	for i := range allTasks {
		tk := allTasks[i]
		if tk.Kind != "review" || tk.TargetTaskID == nil || *tk.TargetTaskID != taskID {
			continue
		}
		switch tk.Model {
		case "opus":
			t := tk
			opusRev = &t
		case "sonnet":
			t := tk
			sonnetRev = &t
		}
	}
	if opusRev == nil || sonnetRev == nil {
		t.Fatalf("expected both build review tasks")
	}

	if _, err := store.ClaimTask(ctx, opusRev.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	v := "reject"
	if _, err := store.SubmitTask(ctx, opusRev.ID, "opus-reviewer", "no good", &v, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}
	if _, err := store.ClaimTask(ctx, sonnetRev.ID, "sonnet-reviewer", "sonnet", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim: %v", err)
	}
	v2 := "approve"
	if _, err := store.SubmitTask(ctx, sonnetRev.ID, "sonnet-reviewer", "fine by me", &v2, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("failed to submit: %v", err)
	}

	parent, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get parent: %v", err)
	}
	if parent.State != "ready" {
		t.Fatalf("expected plain verdict tally to reject (one reject, one approve), got %s", parent.State)
	}

	var total int
	if err := store.(*sqliteStore).conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM task WHERE target_task_id = ?`, taskID).Scan(&total); err != nil {
		t.Fatalf("failed to count tasks: %v", err)
	}
	if total != 2 {
		t.Errorf("expected exactly the 2 ordinary review tasks and no adjudication task, got %d", total)
	}
}

// TestResearchTrack_EndToEndVerification verifies the complete research track workflow
// covering all acceptance criteria from docs/features/research-track.md.
// This test exercises: track validation, review aggregation, follow-up creation,
// rework, round budget, and spec compaction. See docs/features/research-track-verification.md
// for the detailed verification scenario with step-by-step commands.
func TestResearchTrack_EndToEndVerification(t *testing.T) {
	// AC1: Research task with no Makefile requirement passes review when correct
	t.Run("acceptance_1_research_track_no_makefile", func(t *testing.T) {
		store, ctx, _, taskID := newResearchTask(t, false)
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task: %v", err)
		}
		if task.Track != "research" {
			t.Errorf("expected track 'research', got %q", task.Track)
		}
	})

	// AC2: Confirmed claim with inaccessible source fails; pending with access record does not
	t.Run("acceptance_2_confirmed_source_inaccessible_fails", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		// P1 finding for inaccessible confirmed source blocks the round
		inaccessibleFinding := json.RawMessage(`[{
			"id":"f-inaccessible",
			"severity":"P1",
			"file":"claims.md",
			"line":10,
			"summary":"Source marked confirmed but not accessible",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", inaccessibleFinding)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("expected ready (P1 blocking finding), got %s", parent.State)
		}
	})

	// AC2b: Pending source with access record does not block the round
	t.Run("acceptance_2_pending_source_with_access_record_passes", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		// Round 1: P1 finding for inaccessible confirmed source blocks
		blockingFinding := json.RawMessage(`[{
			"id":"f-confirmed-inaccessible",
			"severity":"P1",
			"file":"claims.md",
			"line":10,
			"summary":"Source marked confirmed but not accessible",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blockingFinding)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("expected ready after round 1 (P1 blocking), got %s", parent.State)
		}

		// Rework to round 2 - mark source as pending with access record
		resubmitResearchImplementTask(t, store, ctx, parentID)
		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)

		// Round 2: the claim was downgraded from confirmed to pending with an access
		// attempt recorded. Per docs/features/research-track.md section 2 ("If the
		// claim is already marked pending, with a record of the access attempt,
		// inaccessibility alone is not a finding"), the reviewer raises no new
		// finding for it and reports the round-1 finding as resolved, since
		// downgrading the claim fixed the confirmed-but-unverifiable violation.
		resolvedFinding := json.RawMessage(`[{
			"id":"f-pending-with-access",
			"severity":"P1",
			"file":"claims.md",
			"line":10,
			"summary":"Source marked pending with access attempt recorded",
			"in_changed_text":false,
			"status":"resolved",
			"prior_id":"f-confirmed-inaccessible"
		}]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", resolvedFinding)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err = store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Errorf("expected approved (pending with access record doesn't block), got %s", parent.State)
		}

		// No follow-up: the round-1 finding is resolved, and the pending-with-access
		// state is not itself a finding, so nothing is outstanding to follow up on.
		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 0 {
			t.Errorf("expected no follow-up tasks (finding resolved, no new finding raised), got %d", len(followUps))
		}
	})

	// AC3: Round with only P3 findings passes and creates follow-up tasks
	t.Run("acceptance_3_p3_findings_create_followups", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		p3Findings := json.RawMessage(`[
			{
				"id":"f-p3-1",
				"severity":"P3",
				"file":"doc.md",
				"line":5,
				"summary":"Wrong page number",
				"in_changed_text":true,
				"status":"new"
			},
			{
				"id":"f-p3-2",
				"severity":"P3",
				"file":"doc.md",
				"line":15,
				"summary":"Formatting issue",
				"in_changed_text":true,
				"status":"new"
			}
		]`)
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", p3Findings)
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Errorf("expected approved (P3 findings don't block), got %s", parent.State)
		}

		// Verify follow-up tasks were created (research_parent linked research tasks)
		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 2 {
			t.Errorf("expected 2 follow-up tasks for P3 findings, got %d", len(followUps))
		}
		for _, fu := range followUps {
			if fu.State != "backlog" {
				t.Errorf("expected follow-up in backlog, got %s", fu.State)
			}
			if fu.Track != "research" {
				t.Errorf("expected follow-up track 'research', got %s", fu.Track)
			}
		}
	})

	// AC4: After round 1, P2 in unchanged text doesn't fail the round but creates follow-up
	t.Run("acceptance_4_p2_unchanged_creates_followup", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		// Round 1: Opus reports P2 in changed text (blocks), needs rework
		p2Changed := json.RawMessage(`[{
			"id":"f-p2-r1",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"Claim overstates source evidence",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", p2Changed)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Fatalf("expected ready (P2 in changed text blocks round 1), got %s", parent.State)
		}

		// Rework to round 2
		resubmitResearchImplementTask(t, store, ctx, parentID)
		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)

		// Round 2: P2 is now resolved in changed text (marked as resolved),
		// but opus reports a NEW P2 in UNCHANGED text - this should NOT block
		p2Resolved := json.RawMessage(`[{
			"id":"f-p2-r2-resolved",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"Claim fixed from round 1",
			"in_changed_text":true,
			"status":"resolved",
			"prior_id":"f-p2-r1"
		},{
			"id":"f-p2-r2-unchanged",
			"severity":"P2",
			"file":"doc.md",
			"line":10,
			"summary":"Another claim slightly overstates evidence",
			"in_changed_text":false,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "approve", p2Resolved)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err = store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Errorf("expected approved (P2 in unchanged text in round 2 doesn't block), got %s", parent.State)
		}

		// Verify follow-up created for the P2 in unchanged text
		followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
		if len(followUps) != 1 {
			t.Errorf("expected 1 follow-up task for P2 in unchanged text, got %d", len(followUps))
		}
	})

	// AC5: P2 in changed text fails the round from either reviewer
	t.Run("acceptance_5_p2_changed_text_blocks_opus_rejects", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		// P2 in changed text from opus - should block despite sonnet approve
		p2Changed := json.RawMessage(`[{
			"id":"f-p2-changed",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"New claim unsupported",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", p2Changed)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("expected ready (P2 in changed text blocks round), got %s", parent.State)
		}
	})

	// AC5b: P2 in changed text from sonnet also blocks round
	t.Run("acceptance_5_p2_changed_text_blocks_sonnet_rejects", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		// P2 in changed text from sonnet - should block despite opus approve
		p2Changed := json.RawMessage(`[{
			"id":"f-p2-changed",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"New claim unsupported",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "approve", json.RawMessage(`[]`))
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "reject", p2Changed)

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("expected ready (P2 in changed text from sonnet blocks), got %s", parent.State)
		}
	})

	// AC6: Disputed finding that reviewer maintains triggers adjudication and ruling decides the finding
	t.Run("acceptance_6_dispute_triggers_adjudication", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")

		// Round 1: Opus raises a P2 finding
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		finding := json.RawMessage(`[{
			"id":"f-dispute",
			"severity":"P2",
			"file":"claims.md",
			"line":10,
			"summary":"Source interpretation is debatable",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", finding)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		// Worker disputes the finding in rework
		disputes := json.RawMessage(`[{"finding_id":"f-dispute","evidence":"Our interpretation is supported by pages 23-24 of the source"}]`)
		if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
			t.Fatalf("dispute submission failed: %v", err)
		}

		// Round 2: Opus maintains the finding (should trigger adjudication)
		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
		maintained := json.RawMessage(`[{
			"id":"f-dispute-r2",
			"severity":"P2",
			"file":"claims.md",
			"line":10,
			"summary":"Source interpretation is debatable",
			"in_changed_text":false,
			"status":"still_open",
			"prior_id":"f-dispute"
		}]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", maintained)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		// Verify adjudication task created
		adjTasks := findAdjudicationTasks(t, store, ctx, projID, parentID)
		if len(adjTasks) != 1 {
			t.Fatalf("expected 1 adjudication task, got %d", len(adjTasks))
		}
		if adjTasks[0].Model != "haiku" {
			t.Errorf("expected adjudication on 'haiku', got %q", adjTasks[0].Model)
		}

		// Adjudicator upholds the finding (rejects the dispute)
		submitAdjudication(t, store, ctx, &adjTasks[0], "reject")

		// Verify adjudication ruling decides the finding - parent should be blocked
		// because the upheld P2 finding in changed text blocks the round
		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "ready" {
			t.Errorf("expected ready (adjudication upheld finding blocks), got %s", parent.State)
		}

		// Test case 2: Adjudicator overturns the finding (approves the dispute)
		// Create another task to test the overturn case
		store2, ctx2, projID2, parentID2 := newResearchTaskWithAdjudicator(t, []string{"opus", "sonnet"}, "haiku")
		opus1b, sonnet1b := findResearchReviewTasks(t, store2, ctx2, projID2, parentID2, 1)
		finding2 := json.RawMessage(`[{
			"id":"f-dispute2",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"Questionable claim",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store2, ctx2, opus1b, "opus-reviewer", "reject", finding2)
		submitResearchReview(t, store2, ctx2, sonnet1b, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		disputes2 := json.RawMessage(`[{"finding_id":"f-dispute2","evidence":"Supported by source section 3"}]`)
		if _, err := resubmitResearchImplementTaskWithDisputes(t, store2, ctx2, parentID2, disputes2); err != nil {
			t.Fatalf("dispute submission failed: %v", err)
		}

		opus2b, sonnet2b := findResearchReviewTasks(t, store2, ctx2, projID2, parentID2, 2)
		maintained2 := json.RawMessage(`[{
			"id":"f-dispute2-r2",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"Questionable claim",
			"in_changed_text":false,
			"status":"still_open",
			"prior_id":"f-dispute2"
		}]`)
		submitResearchReview(t, store2, ctx2, opus2b, "opus-reviewer", "reject", maintained2)
		submitResearchReview(t, store2, ctx2, sonnet2b, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		adjTasks2 := findAdjudicationTasks(t, store2, ctx2, projID2, parentID2)
		if len(adjTasks2) != 1 {
			t.Fatalf("expected 1 adjudication task for overturn case, got %d", len(adjTasks2))
		}

		// Adjudicator overturns the finding (approves the dispute)
		submitAdjudication(t, store2, ctx2, &adjTasks2[0], "approve")

		// Parent should be approved since the overturned finding no longer blocks
		parent2, err := store2.GetTask(ctx2, parentID2)
		if err != nil {
			t.Fatalf("failed to get parent2: %v", err)
		}
		if parent2.State != "approved" {
			t.Errorf("expected approved (adjudication overturned finding), got %s", parent2.State)
		}
	})

	// AC7: Round budget blocks task with "decompose" reason
	t.Run("acceptance_7_round_budget_blocks_with_decompose", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		budget := 3 // Use small budget for quick test

		// Get initial model to verify it doesn't change
		initialTask, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get initial task: %v", err)
		}
		initialModel := initialTask.Model

		// Cycle through rounds, rejecting each time
		for round := 1; round <= budget; round++ {
			opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, round)
			if opus == nil || sonnet == nil {
				t.Fatalf("round %d: expected both review tasks", round)
			}

			finding := json.RawMessage(fmt.Sprintf(`[{
				"id":"f-%d",
				"severity":"P2",
				"file":"doc.md",
				"line":%d,
				"summary":"Issue round %d",
				"in_changed_text":true,
				"status":"new"
			}]`, round, 10+round, round))

			submitResearchReviewWithBudget(t, store, ctx, opus, "opus-reviewer", "reject", finding, budget)
			submitResearchReviewWithBudget(t, store, ctx, sonnet, "sonnet-reviewer", "reject", finding, budget)

			if round < budget {
				// Rework to next round
				resubmitResearchImplementTask(t, store, ctx, parentID)
			}
		}

		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "blocked" {
			t.Errorf("expected blocked state after budget exhaustion, got state=%s", parent.State)
		}

		// Verify the block reason is "decompose"
		events, err := store.ListEvents(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get events: %v", err)
		}
		var foundDecomposeBlock bool
		for _, e := range events {
			if e.Kind == "transition" && e.Note != nil && strings.Contains(*e.Note, "decompose") {
				foundDecomposeBlock = true
				break
			}
		}
		if !foundDecomposeBlock {
			t.Errorf("expected decompose block event in task history")
		}

		// Verify model tier stays constant (no escalation without ladder)
		if parent.Model != initialModel {
			t.Errorf("expected model tier unchanged without escalation ladder, was %q, now %q", initialModel, parent.Model)
		}
	})

	// AC7b: Configured-ladder case - a task escalates across tiers instead of the
	// model staying fixed. This subtest uses an unlimited research round budget, so
	// it exercises only the escalation half of AC7b (tier change). The other half —
	// that rounds on every tier count toward the same chain-wide budget, which
	// eventually blocks with reason "decompose" after crossing tiers — is exercised
	// by TestResearchBudget_MultipleSupersessions, which escalates haiku->sonnet,
	// manually supersedes again, and blocks at the chain-wide budget on round 4,
	// with the decompose note listing all 4 rounds across every tier in order.
	t.Run("acceptance_7_configured_ladder_shared_budget", func(t *testing.T) {
		// Use the existing TestResearchAggregation_CircuitBreakerEscalates test pattern
		// which verifies escalation with a configured ladder and thresholds
		store, ctx, projID, parentID := newResearchTaskWithEscalationLadder(t, true, []string{"opus", "sonnet"})

		// The circuit breaker escalates after 8 rounds (default maxReviewRounds threshold)
		// Here we verify the same task can escalate with proper thresholds
		initialTask, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get initial task: %v", err)
		}
		initialModel := initialTask.Model

		// Helper to run rejection rounds with the ladder
		blockingRound := func(round int) {
			opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, round)
			if opus == nil || sonnet == nil {
				t.Fatalf("round %d: expected both review tasks", round)
			}
			blocking := json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"fabricated source","in_changed_text":true,"status":"new"}]`)
			submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", blocking)
			submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
		}

		// Run through rounds until escalation happens
		// The circuit breaker escalates at round 9 with default threshold of 8
		for i := 1; i <= 8; i++ {
			blockingRound(i)
			parent, err := store.GetTask(ctx, parentID)
			if err != nil {
				t.Fatalf("failed to get parent (round %d): %v", i, err)
			}
			if parent.State != "ready" {
				t.Fatalf("round %d: expected ready, got %s", i, parent.State)
			}
			resubmitResearchImplementTask(t, store, ctx, parentID)
		}

		// Round 9 should trigger escalation
		blockingRound(9)
		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent (round 9): %v", err)
		}

		// Verify escalation happened
		if parent.State != "superseded" {
			t.Errorf("expected escalation at round 9 (superseded state), got state=%s", parent.State)
		}
		if parent.SupersededBy == nil {
			t.Fatalf("expected SupersededBy to be set when escalated")
		}

		// Verify the new (escalated) task is on the next tier
		escalated, err := store.GetTask(ctx, *parent.SupersededBy)
		if err != nil {
			t.Fatalf("failed to get escalated task: %v", err)
		}
		if escalated.Model == initialModel {
			t.Errorf("escalated task should have moved to next tier, but stayed on %s", initialModel)
		}
		if escalated.Track != "research" {
			t.Errorf("escalated task should keep track 'research', got %s", escalated.Track)
		}

		if escalated.State != "review" && escalated.State != "ready" && escalated.State != "backlog" {
			t.Errorf("expected escalated task in review, ready, or backlog state, got %s", escalated.State)
		}
	})

	// AC8: Model assignment and preservation
	// Test that explicit model is preserved, task without model gets research default,
	// and build/design defaults are unchanged.
	t.Run("acceptance_8_explicit_model_preserved", func(t *testing.T) {
		store, ctx, _, taskID := newResearchTask(t, false)
		task, err := store.GetTask(ctx, taskID)
		if err != nil {
			t.Fatalf("failed to get task: %v", err)
		}
		// Task created with explicit model "haiku" keeps that model
		if task.Model != "haiku" {
			t.Errorf("expected explicit model 'haiku', got %q", task.Model)
		}
		// Verify it's a research task
		if task.Track != "research" {
			t.Errorf("expected research track, got %q", task.Track)
		}
	})

	// AC8b: Research task created without a model gets ODONIAN_RESEARCH_DEFAULT_MODEL
	t.Run("acceptance_8_research_default_model_applied", func(t *testing.T) {
		// Create store with explicit research default model
		store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
			WithResearchDefaultModel("opus"))
		if err != nil {
			t.Fatalf("failed to open test database: %v", err)
		}
		t.Cleanup(func() { store.Close() })

		ctx := context.Background()
		proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
		if err != nil {
			t.Fatalf("failed to create project: %v", err)
		}
		doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
		if err != nil {
			t.Fatalf("failed to create document: %v", err)
		}

		escalate := false
		// Create research task WITHOUT specifying a model (Model field omitted)
		tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{
				Title:      "Verify claims",
				Spec:       "Verify the claims in the doc",
				DocumentID: doc.ID,
				// Model is intentionally omitted - should default to ODONIAN_RESEARCH_DEFAULT_MODEL
				ReviewModels: []string{"opus", "sonnet"},
				Track:        "research",
				Escalate:     &escalate,
			},
		})
		if err != nil {
			t.Fatalf("failed to create task: %v", err)
		}

		if len(tasks) != 1 {
			t.Fatalf("expected 1 task, got %d", len(tasks))
		}

		// Verify the task got the research default model ("opus" in this test)
		if tasks[0].Model != "opus" {
			t.Errorf("expected research default model 'opus', got %q", tasks[0].Model)
		}
	})

	// AC8c: Build and design defaults are unchanged (not affected by research default)
	t.Run("acceptance_8_build_design_defaults_unchanged", func(t *testing.T) {
		// Create store with explicit research default (opus) different from build/design default (haiku)
		store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels(),
			WithResearchDefaultModel("opus"))
		if err != nil {
			t.Fatalf("failed to open test database: %v", err)
		}
		t.Cleanup(func() { store.Close() })

		ctx := context.Background()
		proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
		if err != nil {
			t.Fatalf("failed to create project: %v", err)
		}
		doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "other-doc", "other.md", nil)
		if err != nil {
			t.Fatalf("failed to create document: %v", err)
		}

		escalate := false
		// Create build and design tasks without explicit model
		tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{
				Title:      "Build task without model",
				Spec:       "build spec",
				DocumentID: doc.ID,
				// Model field omitted - should use build default (first allowed model)
				ReviewModels: []string{"opus", "sonnet"},
				Track:        "build",
				Escalate:     &escalate,
			},
			{
				Title:      "Design task without model",
				Spec:       "design spec",
				DocumentID: doc.ID,
				// Model field omitted - should use design default (first allowed model)
				ReviewModels: []string{"opus", "sonnet"},
				Track:        "design",
				Escalate:     &escalate,
			},
		})
		if err != nil {
			t.Fatalf("failed to create tasks: %v", err)
		}

		if len(tasks) != 2 {
			t.Fatalf("expected 2 tasks, got %d", len(tasks))
		}

		buildModel := tasks[0].Model
		designModel := tasks[1].Model

		if buildModel == "" {
			t.Errorf("build task model should not be empty")
		}
		if designModel == "" {
			t.Errorf("design task model should not be empty")
		}

		// They should match each other (same default logic)
		if buildModel != designModel {
			t.Errorf("build and design should use same default model logic, got build=%q design=%q",
				buildModel, designModel)
		}

		// Verify build/design use the first allowed model (haiku), NOT the research default (opus)
		if buildModel == "opus" {
			t.Errorf("build task should not use research default model opus, got %q", buildModel)
		}
		if designModel == "opus" {
			t.Errorf("design task should not use research default model opus, got %q", designModel)
		}

		// Create a research task to verify it DOES use the research default
		researchTasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
			{
				Title:      "Research task without model",
				Spec:       "research spec",
				DocumentID: doc.ID,
				// Model field omitted - should use RESEARCH default (opus, not build/design default)
				ReviewModels: []string{"opus", "sonnet"},
				Track:        "research",
				Escalate:     &escalate,
			},
		})
		if err != nil {
			t.Fatalf("failed to create research task: %v", err)
		}
		researchModel := researchTasks[0].Model
		if researchModel != "opus" {
			t.Errorf("research task should use research default model opus, got %q", researchModel)
		}
		if researchModel == buildModel {
			t.Errorf("research model %q should differ from build model %q when research default is set",
				researchModel, buildModel)
		}
	})

	// AC9: Superseded research spec is compacted
	t.Run("acceptance_9_supersede_spec_compaction", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)

		originalAssignment := "Verify the claims in the doc"

		// Round 1: Get findings that will trigger rejection
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
		findings1 := json.RawMessage(`[{
			"id":"f1",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"Issue in round 1",
			"in_changed_text":true,
			"status":"new"
		},{
			"id":"f2",
			"severity":"P2",
			"file":"doc.md",
			"line":10,
			"summary":"Another issue in round 1",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings1)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		// Parent is now in ready state - verify state before rework
		readyTask, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get task after round 1: %v", err)
		}
		if readyTask.State != "ready" {
			t.Fatalf("expected ready after round 1 rejection, got %s", readyTask.State)
		}

		// Rework to round 2
		resubmitResearchImplementTask(t, store, ctx, parentID)
		opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)

		// Round 2: Only one finding remains unresolved from round 1
		findings2 := json.RawMessage(`[{
			"id":"f1-r2",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"Issue still present",
			"in_changed_text":false,
			"status":"still_open",
			"prior_id":"f1"
		},{
			"id":"f2-r2",
			"severity":"P2",
			"file":"doc.md",
			"line":10,
			"summary":"Another issue resolved",
			"in_changed_text":false,
			"status":"resolved",
			"prior_id":"f2"
		}]`)
		submitResearchReview(t, store, ctx, opus2, "opus-reviewer", "reject", findings2)
		submitResearchReview(t, store, ctx, sonnet2, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		// Parent is back to ready after second rejection
		readyTask2, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get task after round 2: %v", err)
		}
		if readyTask2.State != "ready" {
			t.Fatalf("expected ready after round 2 rejection, got %s", readyTask2.State)
		}

		// Now supersede the rejected parent task
		newTask, err := store.SupersedeTask(ctx, parentID, nil)
		if err != nil {
			t.Fatalf("SupersedeTask failed: %v", err)
		}
		if newTask.ID == "" {
			t.Fatalf("SupersedeTask returned empty ID")
		}

		// Verify original parent is now superseded
		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "superseded" {
			t.Errorf("expected superseded state, got %s", parent.State)
		}
		if parent.SupersededBy == nil || *parent.SupersededBy != newTask.ID {
			t.Errorf("expected SupersededBy to point to %s", newTask.ID)
		}

		// Verify the new task's spec contains original assignment and unresolved findings
		if !strings.Contains(newTask.Spec, originalAssignment) {
			t.Errorf("new task spec should contain original assignment %q, got: %s",
				originalAssignment, newTask.Spec)
		}

		// Spec should contain the still_open finding f1
		if !strings.Contains(newTask.Spec, "f1") || !strings.Contains(newTask.Spec, "Issue still present") {
			t.Errorf("new task spec should contain unresolved finding f1, got: %s", newTask.Spec)
		}

		// Spec should NOT contain the resolved finding f2 from round 1
		if strings.Contains(newTask.Spec, "Another issue in round 1") {
			t.Errorf("new task spec should NOT contain resolved finding f2 feedback, got: %s", newTask.Spec)
		}

		// Spec should NOT contain feedback that only ever appeared in round 1's
		// still_open finding text (superseded by round 2's own wording), confirming
		// compaction keeps only the last round's unresolved findings, not the full
		// history.
		if strings.Contains(newTask.Spec, "Issue in round 1") {
			t.Errorf("new task spec should not contain round-1-only finding wording, got: %s", newTask.Spec)
		}
	})

	// AC10: Build and design tracks unchanged
	t.Run("acceptance_10_build_design_unchanged", func(t *testing.T) {
		store, ctx, projID, _ := newResearchTask(t, false)

		// Get a document for the build task
		doc, err := store.CreateDocument(ctx, projID, "feature_spec", "build-doc", "build.md", nil)
		if err != nil {
			t.Fatalf("failed to create document: %v", err)
		}

		// Create a parallel build task to verify independence
		buildTasks, err := store.CreateTasks(ctx, projID, []TaskInput{
			{
				Title:        "Build task",
				Spec:         "build spec",
				DocumentID:   doc.ID,
				Model:        "haiku",
				ReviewModels: []string{"opus", "sonnet"},
				Track:        "build",
			},
		})
		if err != nil {
			t.Fatalf("failed to create build task: %v", err)
		}

		buildID := buildTasks[0].ID
		if _, err := store.PromoteTask(ctx, buildID); err != nil {
			t.Fatalf("failed to promote build task: %v", err)
		}
		if _, err := store.ClaimTask(ctx, buildID, "agent-1", "haiku", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim build task: %v", err)
		}
		if _, err := store.SubmitTask(ctx, buildID, "agent-1", "Done", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to submit build task: %v", err)
		}

		// Verify build task behavior is unchanged (not affected by research track)
		buildTask, err := store.GetTask(ctx, buildID)
		if err != nil {
			t.Fatalf("failed to get build task: %v", err)
		}
		if buildTask.Track != "build" {
			t.Errorf("expected build track, got %q", buildTask.Track)
		}

		// Get build reviews
		allTasks, err := store.ListTasks(ctx, projID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks: %v", err)
		}

		// Count build review tasks
		buildReviewCount := 0
		for _, tk := range allTasks {
			if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == buildID {
				buildReviewCount++
			}
		}

		if buildReviewCount != 2 {
			t.Errorf("expected 2 build review tasks (both reviewers), got %d", buildReviewCount)
		}

		// Create a parallel design task to verify the design track is also unaffected.
		designTasks, err := store.CreateTasks(ctx, projID, []TaskInput{
			{
				Title:        "Design task",
				Spec:         "design spec",
				DocumentID:   doc.ID,
				Model:        "haiku",
				ReviewModels: []string{"opus", "sonnet"},
				Track:        "design",
			},
		})
		if err != nil {
			t.Fatalf("failed to create design task: %v", err)
		}

		designID := designTasks[0].ID
		if _, err := store.PromoteTask(ctx, designID); err != nil {
			t.Fatalf("failed to promote design task: %v", err)
		}
		if _, err := store.ClaimTask(ctx, designID, "agent-1", "haiku", 5*time.Minute); err != nil {
			t.Fatalf("failed to claim design task: %v", err)
		}
		if _, err := store.SubmitTask(ctx, designID, "agent-1", "Done", nil, []LinkInput{{Kind: "pr", Value: "#101"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
			t.Fatalf("failed to submit design task: %v", err)
		}

		designTask, err := store.GetTask(ctx, designID)
		if err != nil {
			t.Fatalf("failed to get design task: %v", err)
		}
		if designTask.Track != "design" {
			t.Errorf("expected design track, got %q", designTask.Track)
		}
	})

	// AC-M4: Scorecard API read and TUI view
	t.Run("acceptance_scorecard_api_and_tui_read", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus1, sonnet1 := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		// Submit some findings to populate scorecard
		findings := json.RawMessage(`[{
			"id":"f-scorecard",
			"severity":"P2",
			"file":"doc.md",
			"line":5,
			"summary":"Test finding for scorecard",
			"in_changed_text":true,
			"status":"new"
		}]`)
		submitResearchReview(t, store, ctx, opus1, "opus-reviewer", "reject", findings)
		submitResearchReview(t, store, ctx, sonnet1, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		// Get scorecard data via API (GetResearchReviewerScorecards)
		scorecardData, err := store.GetResearchReviewerScorecards(ctx, projID)
		if err != nil {
			t.Fatalf("failed to get research reviewer scorecards: %v", err)
		}
		if len(scorecardData.Scorecards) == 0 {
			t.Fatalf("expected at least one scorecard, got none")
		}

		// Verify scorecard contains expected fields
		for _, sc := range scorecardData.Scorecards {
			if sc.Model == "" {
				t.Errorf("scorecard missing Model")
			}
		}

		// Opus raised one P2 finding in round 1: assert the real counts, not just presence.
		var opusScorecard, sonnetScorecard *ReviewerScorecard
		for i := range scorecardData.Scorecards {
			switch scorecardData.Scorecards[i].Model {
			case "opus":
				opusScorecard = &scorecardData.Scorecards[i]
			case "sonnet":
				sonnetScorecard = &scorecardData.Scorecards[i]
			}
		}
		if opusScorecard == nil {
			t.Fatalf("expected an opus scorecard, got none")
		}
		if opusScorecard.FindingsRaised["p2"] != 1 {
			t.Errorf("expected opus FindingsRaised[p2] == 1, got %d (%v)", opusScorecard.FindingsRaised["p2"], opusScorecard.FindingsRaised)
		}
		if opusScorecard.TotalReviewRounds != 1 {
			t.Errorf("expected opus TotalReviewRounds == 1, got %d", opusScorecard.TotalReviewRounds)
		}
		if sonnetScorecard == nil {
			t.Fatalf("expected a sonnet scorecard, got none")
		}
		if sonnetScorecard.FindingsRaised["p1"] != 0 || sonnetScorecard.FindingsRaised["p2"] != 0 {
			t.Errorf("expected sonnet to have raised no P1/P2 findings, got %v", sonnetScorecard.FindingsRaised)
		}
	})

	// Human merge gate verification
	t.Run("acceptance_human_merge_gate", func(t *testing.T) {
		store, ctx, projID, parentID := newResearchTask(t, false)
		opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)

		// Submit approvals to get to approved state
		submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", json.RawMessage(`[]`))
		submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))

		// Get parent task - should be in approved state
		parent, err := store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Fatalf("expected approved state, got %s", parent.State)
		}

		// Verify no merge-kind task is spawned (human merge gate enforced)
		// agent_merge=false means no automatic merge-kind task is created
		allTasks, err := store.ListTasks(ctx, projID, TaskListFilter{})
		if err != nil {
			t.Fatalf("failed to list tasks: %v", err)
		}

		var mergeTask *Task
		for _, tk := range allTasks {
			if tk.Kind == "merge" && tk.TargetTaskID != nil && *tk.TargetTaskID == parentID {
				mergeTask = &tk
				break
			}
		}

		// Research tasks have agent_merge=false by default, so no merge task should be spawned
		// The approved task stays in approved state awaiting human decision
		if mergeTask != nil {
			t.Errorf("expected no merge task spawned for research task (human gate), but found one: %s", mergeTask.ID)
		}

		// Verify the approved parent is still in approved state (not auto-merged)
		parent, err = store.GetTask(ctx, parentID)
		if err != nil {
			t.Fatalf("failed to re-get parent: %v", err)
		}
		if parent.State != "approved" {
			t.Errorf("expected approved state (not auto-merged), got %s", parent.State)
		}
	})
}
