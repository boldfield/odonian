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
	if migrationCount != 13 {
		t.Errorf("expected 13 migrations to be recorded, but got %d", migrationCount)
	}

	// Verify idempotency: re-open the same database and it should work
	store2, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to re-open database: %v", err)
	}
	defer store2.Close()

	// Verify that we still have exactly 13 migrations recorded (idempotency)
	err = store2.Conn().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&migrationCount)
	if err != nil {
		t.Fatalf("failed to count migrations after re-open: %v", err)
	}
	if migrationCount != 13 {
		t.Errorf("expected 13 migrations after re-open (idempotency), but got %d", migrationCount)
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
	if migrationCount != 13 {
		t.Errorf("expected 13 migrations after second open, but got %d", migrationCount)
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
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", result, nil, links, 5, nil)
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
	_, err = store.SubmitTask(ctx, designTaskID, "agent-1", "Design complete", nil, []LinkInput{{Kind: "pr", Value: "#123"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, buildTaskID, "agent-2", "Build complete", nil, []LinkInput{{Kind: "pr", Value: "#456"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, defaultTaskID, "agent-3", "Default complete", nil, []LinkInput{{Kind: "pr", Value: "#789"}}, 5, nil)
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
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", result, nil, links, 5, nil)
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

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "First implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Fixed implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	submittedTask, err := store.SubmitTask(ctx, task.ID, "agent-1", "result of work", nil, links, 5, nil)
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
	submittedTask2, err := store.SubmitTask(ctx, task.ID, "agent-1", "updated result", nil, links, 5, nil)
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
	submittedTask3, err := store.SubmitTask(ctx, task.ID, "agent-1", "result with commit", nil, linksWithCommit, 5, nil)
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
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	reviewResult, err := store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	reviewResult, err := store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, 5, nil)
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
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", &approve, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Reviewed", nil, []LinkInput{}, 5, nil)
	if err == nil {
		t.Fatalf("expected error when submitting review task without verdict")
	}

	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Code != "MISSING_VERDICT" {
		t.Errorf("expected MISSING_VERDICT validation error, got: %v", err)
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
		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil)
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
		_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil)
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

		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil)
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
		_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil)
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

		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil)
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
		_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil)
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

		_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implementation", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, maxReviewRounds, nil)
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
		_, err = store.SubmitTask(ctx, reviewTask.ID, "sonnet-reviewer", "Needs work", &reject, []LinkInput{}, maxReviewRounds, nil)
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
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, opusTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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
	_, err = store.SubmitTask(ctx, sonnetTask.ID, "sonnet-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, opusTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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
	_, err = store.SubmitTask(ctx, sonnetTask.ID, "sonnet-reviewer", "Needs changes", &reject, []LinkInput{}, 5, nil)
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
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, 5, nil)
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
	_, err = store.ReleaseTask(ctx, taskID)
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
	_, err = store.SubmitTask(ctx, task3ID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Needs work", &reject, []LinkInput{}, 5, nil)
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
	_, err = store.SubmitTask(ctx, taskID2, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#101"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask2.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "acceptance already satisfied on main; no changes needed", nil, noOpLinks, 5, nil)
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
	if _, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "verified satisfied on main", &approve, []LinkInput{}, 5, nil); err != nil {
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
	if _, err = store.SubmitTask(ctx, taskID, "agent-1", "noop", nil, []LinkInput{{Kind: "no_op", Value: "already-satisfied"}}, 5, nil); err != nil {
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
	submittedTask, err := store.SubmitTask(ctx, taskID, "agent-1", "implementation result", nil, []LinkInput{{Kind: "pr", Value: "https://github.com/test/test/pull/1"}}, 5, nil)
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
	releasedTask, err := store.ReleaseTask(ctx, readyTaskID)
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
	_, err = store.SubmitTask(ctx, task4ID, "agent-2", "result", nil, []LinkInput{{Kind: "pr", Value: "https://github.com/test/test/pull/2"}}, 5, nil)
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

	_, err = store.SubmitTask(ctx, task5ID, "agent-3", "result", nil, []LinkInput{{Kind: "pr", Value: "https://github.com/test/test/pull/3"}}, 5, nil)
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
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "No changes needed", nil, []LinkInput{{Kind: "no_op", Value: "acceptance already satisfied"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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
	submitted, err := store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Looks good", &approve, []LinkInput{}, 5, nil)
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

	_, err = store.SubmitTask(ctx, taskID, "agent-1", "Already satisfied on main", nil, []LinkInput{{Kind: "no_op", Value: "commit-hash"}}, 5, nil)
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
	_, err = store.SubmitTask(ctx, reviewTask.ID, "opus-reviewer", "Verified", &approve, []LinkInput{}, 5, nil)
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
	if _, err := st.SubmitTask(ctx, task.ID, "agent-1", "Implementation complete", nil, links, 5, nil); err != nil {
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
