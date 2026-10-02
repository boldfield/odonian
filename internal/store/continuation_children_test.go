package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/boldfield/odonian/internal/manifest"
)

var testManifestValidation = map[string]bool{
	"haiku":  true,
	"sonnet": true,
	"opus":   true,
}

func testChild(key, title, spec, track, model string, fileScope ...string) manifest.Child {
	falseBool := false
	trueBool := true
	return manifest.Child{
		Key:                key,
		Title:              title,
		Spec:               spec,
		Track:              track,
		Model:              model,
		ReviewModels:       []string{"opus", "sonnet"},
		AgentMerge:         &falseBool,
		Escalate:           &trueBool,
		ClaimIDs:           []string{"claim1"},
		SourceStartPoints:  []string{"source1"},
		FileScope:          fileScope,
		AcceptanceCriteria: []string{"criterion1"},
	}
}

func testPendingCandidates(claimIDs ...string) []manifest.PendingCandidate {
	result := make([]manifest.PendingCandidate, len(claimIDs))
	for i, claimID := range claimIDs {
		result[i] = manifest.PendingCandidate{
			ClaimID:     claimID,
			Disposition: manifest.Assigned,
		}
	}
	return result
}

func TestInsertManifestChildrenBasic(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and parent task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create a manifest with one research child
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			testChild("child1", "Research Child", "Child spec", "research", "haiku", "file1.go"),
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	// Validate manifest
	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children
	sqlStore := store.(*sqliteStore)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest123", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}

	if len(createdIDs) != 1 {
		t.Errorf("expected 1 created child, got %d", len(createdIDs))
	}

	tx.Commit()

	// Verify the child was created (after commit to avoid deadlock)
	child, err := store.GetTask(ctx, createdIDs[0])
	if err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}

	if child.Title != "Research Child" {
		t.Errorf("expected title='Research Child', got '%s'", child.Title)
	}
	if child.Track != "research" {
		t.Errorf("expected track='research', got '%s'", child.Track)
	}
	if child.State != "ready" {
		t.Errorf("expected state='ready', got '%s'", child.State)
	}
	if child.Model != "haiku" {
		t.Errorf("expected model='haiku', got '%s'", child.Model)
	}

	// Verify review models
	if len(child.ReviewModels) != 2 {
		t.Errorf("expected 2 review models, got %d", len(child.ReviewModels))
	}
}

func TestInsertManifestChildrenBuildStartsBacklog(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and parent task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create a manifest with a build child
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			testChild("build_child", "Build Child", "Build spec", "build", "haiku", "file1.go"),
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	// Validate manifest
	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children
	sqlStore := store.(*sqliteStore)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest456", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}

	if len(createdIDs) != 1 {
		t.Errorf("expected 1 created child, got %d", len(createdIDs))
	}

	tx.Commit()

	// Verify the build child starts in backlog state (after commit to avoid deadlock)
	child, err := store.GetTask(ctx, createdIDs[0])
	if err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}

	if child.State != "backlog" {
		t.Errorf("expected build child state='backlog', got '%s'", child.State)
	}
}

func TestInsertManifestChildrenDependencies(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and parent task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
		{Title: "External Task", Spec: "External spec", DocumentID: doc.ID},
	})
	if err != nil {
		t.Fatalf("failed to create tasks: %v", err)
	}
	parentID := tasks[0].ID
	externalTaskID := tasks[1].ID

	// Create a manifest with children and dependencies
	child1 := testChild("child1", "Child 1", "Child 1 spec", "research", "haiku", "file1.go")
	child1.Dependencies = []manifest.Dependency{
		{Kind: manifest.DependencyParent, Ref: parentID},
	}

	child2 := testChild("child2", "Child 2", "Child 2 spec", "research", "haiku", "file2.go")
	child2.ClaimIDs = []string{"claim2"}
	child2.SourceStartPoints = []string{"source2"}
	child2.Dependencies = []manifest.Dependency{
		{Kind: manifest.DependencyChild, Ref: "child1"},
		{Kind: manifest.DependencyTask, Ref: externalTaskID},
	}

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			child1,
			child2,
		},
		PendingCandidates: testPendingCandidates("claim1", "claim2"),
	}

	// Validate manifest
	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children
	sqlStore := store.(*sqliteStore)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest789", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}

	if len(createdIDs) != 2 {
		t.Errorf("expected 2 created children, got %d", len(createdIDs))
	}

	tx.Commit()

	// Verify dependencies were created (after commit to avoid deadlock)
	child1ID := createdIDs[0]
	child2ID := createdIDs[1]

	task1, err := store.GetTask(ctx, child1ID)
	if err != nil {
		t.Fatalf("failed to get child1: %v", err)
	}

	// Child1 depends on parent
	if len(task1.DependsOn) != 1 {
		t.Errorf("expected child1 to depend on 1 task, got %d", len(task1.DependsOn))
	} else if task1.DependsOn[0] != parentID {
		t.Errorf("expected child1 to depend on parent %s, got %s", parentID, task1.DependsOn[0])
	}

	task2, err := store.GetTask(ctx, child2ID)
	if err != nil {
		t.Fatalf("failed to get child2: %v", err)
	}

	// Child2 depends on child1 and external task (in creation order: child1, then external)
	if len(task2.DependsOn) != 2 {
		t.Errorf("expected child2 to depend on 2 tasks, got %d", len(task2.DependsOn))
	}
}

func TestInsertManifestChildrenIdempotency(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and parent task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create a manifest
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			testChild("child1", "Research Child", "Child spec", "research", "haiku", "file1.go"),
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children first time
	conn := store.Conn()
	tx1, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	sqlStore := store.(*sqliteStore)
	createdIDs1, err := sqlStore.InsertManifestChildren(ctx, tx1, m, "digest123", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		tx1.Rollback()
		t.Fatalf("failed to insert children first time: %v", err)
	}
	tx1.Commit()

	if len(createdIDs1) != 1 {
		t.Errorf("expected 1 created child on first call, got %d", len(createdIDs1))
	}

	// Insert children again with same manifest (should return same ID)
	tx2, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	createdIDs2, err := sqlStore.InsertManifestChildren(ctx, tx2, m, "digest123", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		tx2.Rollback()
		t.Fatalf("failed to insert children second time: %v", err)
	}
	tx2.Commit()

	if len(createdIDs2) != 1 {
		t.Errorf("expected 1 created child on second call, got %d", len(createdIDs2))
	}

	if createdIDs1[0] != createdIDs2[0] {
		t.Errorf("expected idempotent call to return same ID: first=%s, second=%s", createdIDs1[0], createdIDs2[0])
	}

	// Verify only one child was actually created in the database
	tasks, err = store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	// Should have 2 tasks: parent + 1 child
	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks total, got %d", len(tasks))
	}
}

// TestInsertManifestChildrenIdempotencyWithDependencies tests idempotency when children have dependencies
func TestInsertManifestChildrenIdempotencyWithDependencies(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and parent task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create a manifest with a child that has dependencies
	child := testChild("child1", "Child with dependencies", "Child spec", "research", "haiku", "file1.go")
	child.Dependencies = []manifest.Dependency{
		{Kind: manifest.DependencyParent, Ref: parentID},
	}

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			child,
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children first time
	conn := store.Conn()
	tx1, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	sqlStore := store.(*sqliteStore)
	createdIDs1, err := sqlStore.InsertManifestChildren(ctx, tx1, m, "digest_dep", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		tx1.Rollback()
		t.Fatalf("failed to insert children first time: %v", err)
	}
	tx1.Commit()

	if len(createdIDs1) != 1 {
		t.Errorf("expected 1 created child on first call, got %d", len(createdIDs1))
	}

	// Insert children again with same manifest and dependencies (should be idempotent)
	tx2, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}

	createdIDs2, err := sqlStore.InsertManifestChildren(ctx, tx2, m, "digest_dep", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		tx2.Rollback()
		t.Fatalf("failed to insert children second time: %v", err)
	}
	tx2.Commit()

	if len(createdIDs2) != 1 {
		t.Errorf("expected 1 created child on second call, got %d", len(createdIDs2))
	}

	if createdIDs1[0] != createdIDs2[0] {
		t.Errorf("expected idempotent call to return same ID: first=%s, second=%s", createdIDs1[0], createdIDs2[0])
	}

	// Verify only one child was created and it has exactly one dependency
	childTask, err := store.GetTask(ctx, createdIDs1[0])
	if err != nil {
		t.Fatalf("failed to get child: %v", err)
	}

	if len(childTask.DependsOn) != 1 {
		t.Errorf("expected child to have 1 dependency, got %d", len(childTask.DependsOn))
	}
}

func TestInsertManifestChildrenAllOrNothing(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project, document, and parent task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create a manifest with 2 children, where the second child has an invalid dependency
	// This ensures that the first child is inserted (along with its task_link rows)
	// but then the failure occurs on the second child, testing all-or-nothing rollback
	nonExistentTaskID := "00000000-0000-0000-0000-000000000000"

	child1 := testChild("child1", "First Child", "First spec", "research", "haiku", "file1.go")
	child1.ClaimIDs = []string{"claim1"}

	child2 := testChild("child2", "Second Child", "Second spec", "research", "haiku", "file2.go")
	child2.ClaimIDs = []string{"claim2"} // Different claim ID from child1
	child2.Dependencies = []manifest.Dependency{
		{Kind: manifest.DependencyTask, Ref: nonExistentTaskID},
	}

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			child1,
			child2,
		},
		PendingCandidates: testPendingCandidates("claim1", "claim2"),
	}

	// Validate manifest (it should be valid - validation doesn't check task existence)
	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children - should fail due to invalid task dependency on second child
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	sqlStore := store.(*sqliteStore)
	_, err = sqlStore.InsertManifestChildren(ctx, tx, m, "digest_bad", parentID, proj.ID, doc.ID, nowTimestamp())
	if err == nil {
		t.Fatal("expected error for invalid task dependency, got none")
	}

	tx.Rollback()

	// Verify that no child was created - transaction must have rolled back completely
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	if len(allTasks) != 1 {
		t.Errorf("expected only parent task to exist, got %d tasks", len(allTasks))
	}

	// Verify that no task_link rows for children were created
	var linkCount int
	err = conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task_link
		WHERE kind IN ('continuation_child_dedup', 'continuation_parent', 'continuation_child_claim_ids', 'continuation_child_source_start_points', 'continuation_child_file_scope', 'continuation_child_acceptance_criteria')
	`).Scan(&linkCount)
	if err != nil {
		t.Fatalf("failed to query task_link: %v", err)
	}
	if linkCount != 0 {
		t.Errorf("expected no continuation links, got %d", linkCount)
	}

	// Verify that no orphan task_dep rows were created
	var depCount int
	err = conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM task_dep
		WHERE task_id NOT IN (SELECT id FROM task)
	`).Scan(&depCount)
	if err != nil {
		t.Fatalf("failed to query task_dep: %v", err)
	}
	if depCount != 0 {
		t.Errorf("expected no orphan task_dep rows, got %d", depCount)
	}
}

func TestInsertManifestChildrenEmptyManifest(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
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

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Empty manifest
	m := &manifest.Manifest{
		Version:           1,
		ParentTaskID:      parentID,
		Children:          []manifest.Child{},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	// Insert children with empty manifest
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	sqlStore := store.(*sqliteStore)
	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest_empty", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert empty manifest children: %v", err)
	}

	if len(createdIDs) != 0 {
		t.Errorf("expected no children from empty manifest, got %d", len(createdIDs))
	}

	tx.Commit()
}

func TestInsertManifestChildrenParentLink(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create a project and parent task
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/example/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create manifest
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			testChild("child1", "Child", "Child spec", "research", "haiku", "file1.go"),
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	sqlStore := store.(*sqliteStore)
	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest_parent", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}
	tx.Commit()

	if len(createdIDs) != 1 {
		t.Errorf("expected 1 created child, got %d", len(createdIDs))
	}

	// Get the child and verify it has a continuation_parent link (after commit to avoid deadlock)
	child, err := store.GetTask(ctx, createdIDs[0])
	if err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}

	// Check task links for continuation_parent
	hasParentLink := false
	for _, link := range child.Links {
		if link.Kind == "continuation_parent" && link.Value == parentID {
			hasParentLink = true
			break
		}
	}

	if !hasParentLink {
		t.Errorf("expected child to have continuation_parent link to %s", parentID)
	}
}

func TestInsertManifestChildrenAgentMergeEscalate(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
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

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create manifest with specific agent_merge and escalate flags
	trueBool := true
	falseBool := false

	child1 := testChild("child_agent_merge_true", "Child 1", "Child spec", "research", "haiku", "file1.go")
	child1.AgentMerge = &trueBool
	child1.Escalate = &falseBool
	child1.ClaimIDs = []string{"claim1"}

	child2 := testChild("child_defaults", "Child 2", "Child spec", "research", "haiku", "file2.go")
	child2.ClaimIDs = []string{"claim2"}

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			child1,
			child2,
		},
		PendingCandidates: testPendingCandidates("claim1", "claim2"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	sqlStore := store.(*sqliteStore)
	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest_flags", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}
	tx.Commit()

	if len(createdIDs) != 2 {
		t.Errorf("expected 2 created children, got %d", len(createdIDs))
	}

	// Verify agent_merge and escalate flags (after commit to avoid deadlock)
	task1, err := store.GetTask(ctx, createdIDs[0])
	if err != nil {
		t.Fatalf("failed to get child1: %v", err)
	}

	if !task1.AgentMerge {
		t.Errorf("expected child1 agent_merge=true, got %v", task1.AgentMerge)
	}
	if task1.Escalate {
		t.Errorf("expected child1 escalate=false, got %v", task1.Escalate)
	}

	task2, err := store.GetTask(ctx, createdIDs[1])
	if err != nil {
		t.Fatalf("failed to get child2: %v", err)
	}

	// Default escalate should be true, agent_merge should be false
	if task2.AgentMerge {
		t.Errorf("expected child2 agent_merge=false, got %v", task2.AgentMerge)
	}
	if !task2.Escalate {
		t.Errorf("expected child2 escalate=true (default), got %v", task2.Escalate)
	}
}

// TestInsertManifestChildrenNilManifest tests that a nil manifest returns empty result.
func TestInsertManifestChildrenNilManifest(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
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

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Insert nil manifest
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	sqlStore := store.(*sqliteStore)
	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, nil, "digest_nil", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert nil manifest children: %v", err)
	}

	if len(createdIDs) != 0 {
		t.Errorf("expected no children from nil manifest, got %d", len(createdIDs))
	}

	tx.Commit()
}

// TestInsertManifestChildrenMultipleChildren tests manifest with multiple children.
func TestInsertManifestChildrenMultipleChildren(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
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

	doc, err := store.CreateDocument(ctx, proj.ID, "design", "Test Design", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create manifest with 3 children
	child1 := testChild("research_child", "Research Child", "Spec 1", "research", "haiku", "file1.go")
	child2 := testChild("build_child", "Build Child", "Spec 2", "build", "haiku", "file2.go")
	child3 := testChild("design_child", "Design Child", "Spec 3", "design", "haiku", "file3.go")
	// Update claim IDs to be unique
	child1.ClaimIDs = []string{"claim1"}
	child2.ClaimIDs = []string{"claim2"}
	child3.ClaimIDs = []string{"claim3"}

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			child1,
			child2,
			child3,
		},
		PendingCandidates: testPendingCandidates("claim1", "claim2", "claim3"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	sqlStore := store.(*sqliteStore)
	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest_multi", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}
	tx.Commit()

	if len(createdIDs) != 3 {
		t.Errorf("expected 3 created children, got %d", len(createdIDs))
	}

	// Verify each child has correct state (after commit to avoid deadlock)
	expectedStates := map[int]string{
		0: "ready",   // research
		1: "backlog", // build
		2: "backlog", // design
	}

	for i, childID := range createdIDs {
		child, err := store.GetTask(ctx, childID)
		if err != nil {
			t.Fatalf("failed to get child %d: %v", i, err)
		}

		expectedState := expectedStates[i]
		if child.State != expectedState {
			t.Errorf("child %d: expected state=%s, got %s", i, expectedState, child.State)
		}
	}
}

// TestInsertManifestChildrenSelfDependencyError tests that dependencies don't cause issues.
// (Self-dependencies are validated by the manifest validator, not the store helper)
func TestInsertManifestChildrenExternalTaskNotInProject(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	// Create two projects
	proj1, err := store.CreateProject(ctx, "project-1", "https://github.com/example/repo1")
	if err != nil {
		t.Fatalf("failed to create project 1: %v", err)
	}

	proj2, err := store.CreateProject(ctx, "project-2", "https://github.com/example/repo2")
	if err != nil {
		t.Fatalf("failed to create project 2: %v", err)
	}

	doc1, err := store.CreateDocument(ctx, proj1.ID, "design", "Design 1", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 1: %v", err)
	}

	doc2, err := store.CreateDocument(ctx, proj2.ID, "design", "Design 2", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document 2: %v", err)
	}

	// Create parent in project 1
	tasks1, err := store.CreateTasks(ctx, proj1.ID, []TaskInput{
		{Title: "Parent", Spec: "Parent spec", DocumentID: doc1.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent: %v", err)
	}
	parentID := tasks1[0].ID

	// Create a task in project 2
	tasks2, err := store.CreateTasks(ctx, proj2.ID, []TaskInput{
		{Title: "External Task in Project 2", Spec: "External spec", DocumentID: doc2.ID},
	})
	if err != nil {
		t.Fatalf("failed to create external task: %v", err)
	}
	externalTaskID := tasks2[0].ID

	// Create manifest with dependency on external project's task
	child := testChild("child1", "Child", "Child spec", "research", "haiku", "file1.go")
	child.Dependencies = []manifest.Dependency{
		{Kind: manifest.DependencyTask, Ref: externalTaskID},
	}

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			child,
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Try to insert children - should fail because external task is not in the same project
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	sqlStore := store.(*sqliteStore)
	_, err = sqlStore.InsertManifestChildren(ctx, tx, m, "digest_cross_proj", parentID, proj1.ID, doc1.ID, nowTimestamp())
	if err == nil {
		t.Fatal("expected error for cross-project dependency, got none")
	}

	// Verify no child was created
	allTasks, err := store.ListTasks(ctx, proj1.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	if len(allTasks) != 1 {
		t.Errorf("expected only parent task, got %d tasks", len(allTasks))
	}
}

// TestInsertManifestChildrenMismatchedParentID tests that mismatched parent IDs are rejected
func TestInsertManifestChildrenMismatchedParentID(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
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
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Create manifest with mismatched parent ID
	wrongParentID := "00000000-0000-0000-0000-000000000000"
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: wrongParentID,
		Children: []manifest.Child{
			testChild("child1", "Research Child", "Child spec", "research", "haiku", "file1.go"),
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	sqlStore := store.(*sqliteStore)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, err = sqlStore.InsertManifestChildren(ctx, tx, m, "digest_mismatch", parentID, proj.ID, doc.ID, nowTimestamp())
	if err == nil {
		t.Fatal("expected error for mismatched parent ID, got none")
	}

	// Verify no child was created
	tx.Rollback()
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(allTasks) != 1 {
		t.Errorf("expected only parent task, got %d tasks", len(allTasks))
	}
}

// TestInsertManifestChildrenParentNotFound tests that non-existent parent is rejected
func TestInsertManifestChildrenParentNotFound(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
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

	nonExistentParentID := "00000000-0000-0000-0000-000000000001"

	// Create manifest with non-existent parent
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: nonExistentParentID,
		Children: []manifest.Child{
			testChild("child1", "Research Child", "Child spec", "research", "haiku", "file1.go"),
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	sqlStore := store.(*sqliteStore)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	_, err = sqlStore.InsertManifestChildren(ctx, tx, m, "digest_notfound", nonExistentParentID, proj.ID, doc.ID, nowTimestamp())
	if err == nil {
		t.Fatal("expected error for parent not found, got none")
	}
}

// TestInsertManifestChildrenMismatchedParentProjectDocument tests project/document mismatch is rejected
func TestInsertManifestChildrenMismatchedParentProjectDocument(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer store.Close()

	ctx := context.Background()

	proj1, err := store.CreateProject(ctx, "test-project-1", "https://github.com/example/repo1")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	proj2, err := store.CreateProject(ctx, "test-project-2", "https://github.com/example/repo2")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}

	doc1, err := store.CreateDocument(ctx, proj1.ID, "design", "Test Design 1", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	doc2, err := store.CreateDocument(ctx, proj2.ID, "design", "Test Design 2", "DESIGN.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}

	// Create parent in proj1
	tasks, err := store.CreateTasks(ctx, proj1.ID, []TaskInput{
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc1.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	// Try to insert with mismatched project
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			testChild("child1", "Research Child", "Child spec", "research", "haiku", "file1.go"),
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	sqlStore := store.(*sqliteStore)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	// Pass wrong project/document IDs
	_, err = sqlStore.InsertManifestChildren(ctx, tx, m, "digest_mismatch_proj", parentID, proj2.ID, doc2.ID, nowTimestamp())
	if err == nil {
		t.Fatal("expected error for mismatched project/document, got none")
	}

	// Verify no child was created
	tx.Rollback()
	allTasks, err := store.ListTasks(ctx, proj2.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}
	if len(allTasks) != 0 {
		t.Errorf("expected no tasks in proj2, got %d", len(allTasks))
	}
}

// TestInsertManifestChildrenChildMetadata tests that child metadata is persisted
func TestInsertManifestChildrenChildMetadata(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/test.db"
	store, err := Open(dbPath, defaultTestAllowedModels())
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
		{Title: "Parent Task", Spec: "Parent spec", DocumentID: doc.ID, Track: "research"},
	})
	if err != nil {
		t.Fatalf("failed to create parent task: %v", err)
	}
	parentID := tasks[0].ID

	child := testChild("child1", "Research Child", "Child spec", "research", "haiku", "file1.go", "file2.go")
	child.ClaimIDs = []string{"claim1", "claim2"}
	child.SourceStartPoints = []string{"source1", "source2"}
	child.FileScope = []string{"file1.go", "file2.go"}
	child.AcceptanceCriteria = []string{"criterion1", "criterion2"}

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			child,
		},
		PendingCandidates: testPendingCandidates("claim1", "claim2"),
	}

	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	sqlStore := store.(*sqliteStore)
	conn := store.Conn()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed to begin transaction: %v", err)
	}
	defer tx.Rollback()

	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest_metadata", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}

	if len(createdIDs) != 1 {
		t.Errorf("expected 1 created child, got %d", len(createdIDs))
	}

	tx.Commit()

	// Verify metadata was persisted as task_links
	taskID := createdIDs[0]

	// Check claim_ids link
	row := conn.QueryRowContext(ctx, `
		SELECT value FROM task_link
		WHERE task_id = ? AND kind = 'continuation_child_claim_ids' AND tombstoned_at IS NULL
	`, taskID)
	var claimIDsJSON string
	if err := row.Scan(&claimIDsJSON); err != nil {
		t.Fatalf("failed to get claim_ids link: %v", err)
	}
	var claimIDs []string
	if err := json.Unmarshal([]byte(claimIDsJSON), &claimIDs); err != nil {
		t.Fatalf("failed to unmarshal claim_ids: %v", err)
	}
	if len(claimIDs) != 2 || claimIDs[0] != "claim1" || claimIDs[1] != "claim2" {
		t.Errorf("expected claim_ids [claim1, claim2], got %v", claimIDs)
	}

	// Check source_start_points link
	row = conn.QueryRowContext(ctx, `
		SELECT value FROM task_link
		WHERE task_id = ? AND kind = 'continuation_child_source_start_points' AND tombstoned_at IS NULL
	`, taskID)
	var sourcePointsJSON string
	if err := row.Scan(&sourcePointsJSON); err != nil {
		t.Fatalf("failed to get source_start_points link: %v", err)
	}
	var sourcePoints []string
	if err := json.Unmarshal([]byte(sourcePointsJSON), &sourcePoints); err != nil {
		t.Fatalf("failed to unmarshal source_start_points: %v", err)
	}
	if len(sourcePoints) != 2 || sourcePoints[0] != "source1" || sourcePoints[1] != "source2" {
		t.Errorf("expected source_start_points [source1, source2], got %v", sourcePoints)
	}

	// Check file_scope link
	row = conn.QueryRowContext(ctx, `
		SELECT value FROM task_link
		WHERE task_id = ? AND kind = 'continuation_child_file_scope' AND tombstoned_at IS NULL
	`, taskID)
	var fileScopeJSON string
	if err := row.Scan(&fileScopeJSON); err != nil {
		t.Fatalf("failed to get file_scope link: %v", err)
	}
	var fileScope []string
	if err := json.Unmarshal([]byte(fileScopeJSON), &fileScope); err != nil {
		t.Fatalf("failed to unmarshal file_scope: %v", err)
	}
	if len(fileScope) != 2 || fileScope[0] != "file1.go" || fileScope[1] != "file2.go" {
		t.Errorf("expected file_scope [file1.go, file2.go], got %v", fileScope)
	}

	// Check acceptance_criteria link
	row = conn.QueryRowContext(ctx, `
		SELECT value FROM task_link
		WHERE task_id = ? AND kind = 'continuation_child_acceptance_criteria' AND tombstoned_at IS NULL
	`, taskID)
	var criteriaJSON string
	if err := row.Scan(&criteriaJSON); err != nil {
		t.Fatalf("failed to get acceptance_criteria link: %v", err)
	}
	var criteria []string
	if err := json.Unmarshal([]byte(criteriaJSON), &criteria); err != nil {
		t.Fatalf("failed to unmarshal acceptance_criteria: %v", err)
	}
	if len(criteria) != 2 || criteria[0] != "criterion1" || criteria[1] != "criterion2" {
		t.Errorf("expected acceptance_criteria [criterion1, criterion2], got %v", criteria)
	}
}
