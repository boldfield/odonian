package store

import (
	"context"
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
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
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

	// Verify the child was created
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

	tx.Commit()
}

func TestInsertManifestChildrenBuildStartsBacklog(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
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

	// Verify the build child starts in backlog state
	child, err := store.GetTask(ctx, createdIDs[0])
	if err != nil {
		t.Fatalf("failed to get child task: %v", err)
	}

	if child.State != "backlog" {
		t.Errorf("expected build child state='backlog', got '%s'", child.State)
	}

	tx.Commit()
}

func TestInsertManifestChildrenDependencies(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
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
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			{
				Key:                "child1",
				Title:              "Child 1",
				Spec:               "Child 1 spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				ClaimIDs:           []string{"claim1"},
				SourceStartPoints:  []string{"source1"},
				FileScope:          []string{"file1.go"},
				AcceptanceCriteria: []string{"criterion1"},
				Dependencies: []manifest.Dependency{
					{Kind: manifest.DependencyParent, Ref: parentID},
				},
			},
			{
				Key:                "child2",
				Title:              "Child 2",
				Spec:               "Child 2 spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				ClaimIDs:           []string{"claim2"},
				SourceStartPoints:  []string{"source2"},
				FileScope:          []string{"file2.go"},
				AcceptanceCriteria: []string{"criterion2"},
				Dependencies: []manifest.Dependency{
					{Kind: manifest.DependencyChild, Ref: "child1"},
					{Kind: manifest.DependencyTask, Ref: externalTaskID},
				},
			},
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

	// Verify dependencies were created
	child1ID := createdIDs[0]
	child2ID := createdIDs[1]

	child1, err := store.GetTask(ctx, child1ID)
	if err != nil {
		t.Fatalf("failed to get child1: %v", err)
	}

	// Child1 depends on parent
	if len(child1.DependsOn) != 1 {
		t.Errorf("expected child1 to depend on 1 task, got %d", len(child1.DependsOn))
	} else if child1.DependsOn[0] != parentID {
		t.Errorf("expected child1 to depend on parent %s, got %s", parentID, child1.DependsOn[0])
	}

	child2, err := store.GetTask(ctx, child2ID)
	if err != nil {
		t.Fatalf("failed to get child2: %v", err)
	}

	// Child2 depends on child1 and external task (in creation order: child1, then external)
	if len(child2.DependsOn) != 2 {
		t.Errorf("expected child2 to depend on 2 tasks, got %d", len(child2.DependsOn))
	}

	tx.Commit()
}

func TestInsertManifestChildrenIdempotency(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
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

func TestInsertManifestChildrenAllOrNothing(t *testing.T) {
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
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

	// Create a manifest with an invalid external task dependency
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			{
				Key:                "child1",
				Title:              "Research Child",
				Spec:               "Child spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				ClaimIDs:           []string{"claim1"},
				SourceStartPoints:  []string{"source1"},
				FileScope:          []string{"file1.go"},
				AcceptanceCriteria: []string{"criterion1"},
				Dependencies: []manifest.Dependency{
					{Kind: manifest.DependencyTask, Ref: "invalid-task-id"},
				},
			},
		},
		PendingCandidates: testPendingCandidates("claim1"),
	}

	// Validate manifest (it should be valid - validation doesn't check task existence)
	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		t.Fatalf("manifest validation failed: %v", err)
	}

	// Insert children - should fail due to invalid task dependency
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

	// Verify that no child was created
	allTasks, err := store.ListTasks(ctx, proj.ID, TaskListFilter{})
	if err != nil {
		t.Fatalf("failed to list tasks: %v", err)
	}

	if len(allTasks) != 1 {
		t.Errorf("expected only parent task to exist, got %d tasks", len(allTasks))
	}
}

func TestInsertManifestChildrenEmptyManifest(t *testing.T) {
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
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
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

	// Get the child and verify it has a continuation_parent link
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

	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			{
				Key:                "child_agent_merge_true",
				Title:              "Child 1",
				Spec:               "Child spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				AgentMerge:         &trueBool,
				Escalate:           &falseBool,
				ClaimIDs:           []string{"claim1"},
				SourceStartPoints:  []string{"source1"},
				FileScope:          []string{"file1.go"},
				AcceptanceCriteria: []string{"criterion1"},
			},
			{
				Key:                "child_defaults",
				Title:              "Child 2",
				Spec:               "Child spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				ClaimIDs:           []string{"claim2"},
				SourceStartPoints:  []string{"source2"},
				FileScope:          []string{"file2.go"},
				AcceptanceCriteria: []string{"criterion2"},
			},
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

	// Verify agent_merge and escalate flags
	child1, err := store.GetTask(ctx, createdIDs[0])
	if err != nil {
		t.Fatalf("failed to get child1: %v", err)
	}

	if !child1.AgentMerge {
		t.Errorf("expected child1 agent_merge=true, got %v", child1.AgentMerge)
	}
	if child1.Escalate {
		t.Errorf("expected child1 escalate=false, got %v", child1.Escalate)
	}

	child2, err := store.GetTask(ctx, createdIDs[1])
	if err != nil {
		t.Fatalf("failed to get child2: %v", err)
	}

	// Default escalate should be true, agent_merge should be false
	if child2.AgentMerge {
		t.Errorf("expected child2 agent_merge=false, got %v", child2.AgentMerge)
	}
	if !child2.Escalate {
		t.Errorf("expected child2 escalate=true (default), got %v", child2.Escalate)
	}
}

// TestInsertManifestChildrenNilManifest tests that a nil manifest returns empty result.
func TestInsertManifestChildrenNilManifest(t *testing.T) {
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
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			testChild("research_child", "Research Child", "Spec 1", "research", "haiku", "file1.go"),
			testChild("build_child", "Build Child", "Spec 2", "build", "haiku", "file2.go"),
			testChild("design_child", "Design Child", "Spec 3", "design", "haiku", "file3.go"),
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
	createdIDs, err := sqlStore.InsertManifestChildren(ctx, tx, m, "digest_multi", parentID, proj.ID, doc.ID, nowTimestamp())
	if err != nil {
		t.Fatalf("failed to insert children: %v", err)
	}
	tx.Commit()

	if len(createdIDs) != 3 {
		t.Errorf("expected 3 created children, got %d", len(createdIDs))
	}

	// Verify each child has correct state
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
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
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
	m := &manifest.Manifest{
		Version:      1,
		ParentTaskID: parentID,
		Children: []manifest.Child{
			{
				Key:                "child1",
				Title:              "Child",
				Spec:               "Child spec",
				Track:              "research",
				Model:              "haiku",
				ReviewModels:       []string{"opus", "sonnet"},
				ClaimIDs:           []string{"claim1"},
				SourceStartPoints:  []string{"source1"},
				FileScope:          []string{"file1.go"},
				AcceptanceCriteria: []string{"criterion1"},
				Dependencies: []manifest.Dependency{
					{Kind: manifest.DependencyTask, Ref: externalTaskID},
				},
			},
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
