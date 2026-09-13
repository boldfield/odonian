package tuiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListProjects(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects" {
			t.Errorf("expected /projects, got %s", r.URL.Path)
		}
		// No query params should be present
		if r.URL.RawQuery != "" {
			t.Errorf("expected no query params, got %s", r.URL.RawQuery)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		projects := []Project{
			{ID: "proj1", Name: "Project 1", Repo: "repo1", CreatedAt: "2024-01-01T00:00:00Z"},
		}
		json.NewEncoder(w).Encode(projects)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	projects, err := client.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects failed: %v", err)
	}

	if len(projects) != 1 {
		t.Errorf("expected 1 project, got %d", len(projects))
	}

	if projects[0].ID != "proj1" {
		t.Errorf("expected ID proj1, got %s", projects[0].ID)
	}
}

func TestListProjectsWithFilters(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects" {
			t.Errorf("expected /projects, got %s", r.URL.Path)
		}
		// Verify query params contain all filters
		query := r.URL.Query()
		if query.Get("model") != "haiku" {
			t.Errorf("expected model=haiku, got %s", query.Get("model"))
		}
		if query.Get("kind") != "implement" {
			t.Errorf("expected kind=implement, got %s", query.Get("kind"))
		}
		if query.Get("claimable") != "true" {
			t.Errorf("expected claimable=true, got %s", query.Get("claimable"))
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		projects := []Project{
			{ID: "proj2", Name: "Project 2", Repo: "repo2", CreatedAt: "2024-01-01T00:00:00Z"},
		}
		json.NewEncoder(w).Encode(projects)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with all filters
	projects, err := client.ListProjects(context.Background(),
		WithProjectModel("haiku"),
		WithProjectKind("implement"),
		WithProjectClaimable(true),
	)
	if err != nil {
		t.Fatalf("ListProjects with filters failed: %v", err)
	}

	if len(projects) != 1 {
		t.Errorf("expected 1 project, got %d", len(projects))
	}

	if projects[0].ID != "proj2" {
		t.Errorf("expected ID proj2, got %s", projects[0].ID)
	}
}

func TestListProjectsWithPartialFilters(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects" {
			t.Errorf("expected /projects, got %s", r.URL.Path)
		}
		// Verify only set filters appear in query params
		query := r.URL.Query()
		if query.Get("model") != "opus" {
			t.Errorf("expected model=opus, got %s", query.Get("model"))
		}
		if query.Get("kind") != "" {
			t.Errorf("expected no kind filter, got %s", query.Get("kind"))
		}
		if query.Get("claimable") != "true" {
			t.Errorf("expected claimable=true, got %s", query.Get("claimable"))
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]Project{})
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with only model and claimable filters
	_, err := client.ListProjects(context.Background(),
		WithProjectModel("opus"),
		WithProjectClaimable(true),
	)
	if err != nil {
		t.Fatalf("ListProjects with partial filters failed: %v", err)
	}
}

func TestGetProject(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123" {
			t.Errorf("expected /projects/proj123, got %s", r.URL.Path)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		project := Project{
			ID:        "proj123",
			Name:      "Test Project",
			Repo:      "test-repo",
			CreatedAt: "2024-01-01T00:00:00Z",
		}
		json.NewEncoder(w).Encode(project)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	project, err := client.GetProject(context.Background(), "proj123")
	if err != nil {
		t.Fatalf("GetProject failed: %v", err)
	}

	if project.ID != "proj123" {
		t.Errorf("expected ID proj123, got %s", project.ID)
	}

	if project.Name != "Test Project" {
		t.Errorf("expected Name 'Test Project', got %s", project.Name)
	}
}

func TestListTasks(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123/tasks" {
			t.Errorf("expected /projects/proj123/tasks, got %s", r.URL.Path)
		}
		// No query params should be present
		if r.URL.RawQuery != "" {
			t.Errorf("expected no query params, got %s", r.URL.RawQuery)
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		tasks := []Task{
			{
				ID:        "task1",
				ProjectID: "proj123",
				Title:     "Task 1",
				State:     "backlog",
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
			},
		}
		json.NewEncoder(w).Encode(tasks)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	tasks, err := client.ListTasks(context.Background(), "proj123")
	if err != nil {
		t.Fatalf("ListTasks failed: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	if tasks[0].ID != "task1" {
		t.Errorf("expected ID task1, got %s", tasks[0].ID)
	}
}

func TestListTasksWithFilters(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123/tasks" {
			t.Errorf("expected /projects/proj123/tasks, got %s", r.URL.Path)
		}
		// Verify query params contain all filters
		query := r.URL.Query()
		if query.Get("model") != "haiku" {
			t.Errorf("expected model=haiku, got %s", query.Get("model"))
		}
		if query.Get("kind") != "implement" {
			t.Errorf("expected kind=implement, got %s", query.Get("kind"))
		}
		if query.Get("claimable") != "true" {
			t.Errorf("expected claimable=true, got %s", query.Get("claimable"))
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		tasks := []Task{
			{
				ID:        "task2",
				ProjectID: "proj123",
				Title:     "Task 2",
				State:     "ready",
				Model:     "haiku",
				Kind:      "implement",
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
			},
		}
		json.NewEncoder(w).Encode(tasks)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with all filters
	tasks, err := client.ListTasks(context.Background(), "proj123",
		WithModel("haiku"),
		WithKind("implement"),
		WithClaimable(true),
	)
	if err != nil {
		t.Fatalf("ListTasks with filters failed: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	if tasks[0].ID != "task2" {
		t.Errorf("expected ID task2, got %s", tasks[0].ID)
	}
}

func TestListTasksWithPartialFilters(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123/tasks" {
			t.Errorf("expected /projects/proj123/tasks, got %s", r.URL.Path)
		}
		// Verify only set filters appear in query params
		query := r.URL.Query()
		if query.Get("model") != "opus" {
			t.Errorf("expected model=opus, got %s", query.Get("model"))
		}
		if query.Get("kind") != "" {
			t.Errorf("expected no kind filter, got %s", query.Get("kind"))
		}
		if query.Get("claimable") != "true" {
			t.Errorf("expected claimable=true, got %s", query.Get("claimable"))
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode([]Task{})
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with only model and claimable filters
	_, err := client.ListTasks(context.Background(), "proj123",
		WithModel("opus"),
		WithClaimable(true),
	)
	if err != nil {
		t.Fatalf("ListTasks with partial filters failed: %v", err)
	}
}

func TestListTasksWithState(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123/tasks" {
			t.Errorf("expected /projects/proj123/tasks, got %s", r.URL.Path)
		}
		// Verify query params contain state filter
		query := r.URL.Query()
		if query.Get("state") != "ready" {
			t.Errorf("expected state=ready, got %s", query.Get("state"))
		}
		if query.Get("model") != "" {
			t.Errorf("expected no model filter, got %s", query.Get("model"))
		}
		if query.Get("kind") != "" {
			t.Errorf("expected no kind filter, got %s", query.Get("kind"))
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		tasks := []Task{
			{
				ID:        "task3",
				ProjectID: "proj123",
				Title:     "Task 3",
				State:     "ready",
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
			},
		}
		json.NewEncoder(w).Encode(tasks)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with state filter alone
	tasks, err := client.ListTasks(context.Background(), "proj123",
		WithState("ready"),
	)
	if err != nil {
		t.Fatalf("ListTasks with state filter failed: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	if tasks[0].State != "ready" {
		t.Errorf("expected State 'ready', got %s", tasks[0].State)
	}
}

func TestListTasksWithStateAndKind(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123/tasks" {
			t.Errorf("expected /projects/proj123/tasks, got %s", r.URL.Path)
		}
		// Verify query params contain both state and kind filters
		query := r.URL.Query()
		if query.Get("state") != "in_progress" {
			t.Errorf("expected state=in_progress, got %s", query.Get("state"))
		}
		if query.Get("kind") != "review" {
			t.Errorf("expected kind=review, got %s", query.Get("kind"))
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		tasks := []Task{
			{
				ID:        "task4",
				ProjectID: "proj123",
				Title:     "Task 4",
				State:     "in_progress",
				Kind:      "review",
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
			},
		}
		json.NewEncoder(w).Encode(tasks)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with state and kind filters combined
	tasks, err := client.ListTasks(context.Background(), "proj123",
		WithState("in_progress"),
		WithKind("review"),
	)
	if err != nil {
		t.Fatalf("ListTasks with state and kind filters failed: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	if tasks[0].State != "in_progress" {
		t.Errorf("expected State 'in_progress', got %s", tasks[0].State)
	}

	if tasks[0].Kind != "review" {
		t.Errorf("expected Kind 'review', got %s", tasks[0].Kind)
	}
}

func TestGetTask(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123" {
			t.Errorf("expected /tasks/task123, got %s", r.URL.Path)
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		task := TaskDetail{
			ID:        "task123",
			ProjectID: "proj1",
			Title:     "Task 1",
			Spec:      "Do something",
			State:     "backlog",
			DependsOn: []string{},
			Links:     []TaskLink{},
			CreatedAt: "2024-01-01T00:00:00Z",
			UpdatedAt: "2024-01-01T00:00:00Z",
		}
		json.NewEncoder(w).Encode(task)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	task, err := client.GetTask(context.Background(), "task123")
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}

	if task.ID != "task123" {
		t.Errorf("expected ID task123, got %s", task.ID)
	}
}

func TestListDocuments(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123/documents" {
			t.Errorf("expected /projects/proj123/documents, got %s", r.URL.Path)
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		docs := []Document{
			{
				ID:        "doc1",
				ProjectID: "proj123",
				Kind:      "design",
				Title:     "Design Doc",
				Ref:       "docs/design.md",
				CreatedAt: "2024-01-01T00:00:00Z",
				UpdatedAt: "2024-01-01T00:00:00Z",
			},
		}
		json.NewEncoder(w).Encode(docs)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	docs, err := client.ListDocuments(context.Background(), "proj123")
	if err != nil {
		t.Fatalf("ListDocuments failed: %v", err)
	}

	if len(docs) != 1 {
		t.Errorf("expected 1 document, got %d", len(docs))
	}

	if docs[0].ID != "doc1" {
		t.Errorf("expected ID doc1, got %s", docs[0].ID)
	}
}

func TestPromoteTask(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/promote" {
			t.Errorf("expected /tasks/task123/promote, got %s", r.URL.Path)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Write response
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	err := client.PromoteTask(context.Background(), "task123")
	if err != nil {
		t.Fatalf("PromoteTask failed: %v", err)
	}
}

func TestReviewTask(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/review" {
			t.Errorf("expected /tasks/task123/review, got %s", r.URL.Path)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Verify request body
		var req reviewTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.Actor != "test-actor" {
			t.Errorf("expected actor test-actor, got %s", req.Actor)
		}

		if req.Verdict != "approve" {
			t.Errorf("expected verdict approve, got %s", req.Verdict)
		}

		if req.Note == nil || *req.Note != "looks good" {
			t.Errorf("expected note 'looks good', got %v", req.Note)
		}

		// Write response
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with note
	note := "looks good"
	err := client.ReviewTask(context.Background(), "task123", "test-actor", "approve", &note)
	if err != nil {
		t.Fatalf("ReviewTask failed: %v", err)
	}
}

func TestReviewTaskWithoutNote(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read raw body to verify "note" key is genuinely absent
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read raw body: %v", err)
		}
		bodyStr := string(rawBody)

		// Verify request body via JSON decode
		var req reviewTaskRequest
		if err := json.Unmarshal(rawBody, &req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.Note != nil {
			t.Errorf("expected note to be omitted (nil), got %v", req.Note)
		}

		// Verify "note" key is genuinely absent from the raw JSON body
		if strings.Contains(bodyStr, `"note"`) {
			t.Errorf("expected 'note' key to be absent from raw body, but found it: %s", bodyStr)
		}

		// Write response
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test without note (nil)
	err := client.ReviewTask(context.Background(), "task123", "test-actor", "approve", nil)
	if err != nil {
		t.Fatalf("ReviewTask failed: %v", err)
	}
}

func TestTransitionTask(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/transition" {
			t.Errorf("expected /tasks/task123/transition, got %s", r.URL.Path)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Verify request body
		var req transitionTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.To != "done" {
			t.Errorf("expected to=done, got %s", req.To)
		}

		if req.Note == nil || *req.Note != "completed" {
			t.Errorf("expected note 'completed', got %v", req.Note)
		}

		// Write response
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with note
	note := "completed"
	err := client.TransitionTask(context.Background(), "task123", "done", &note)
	if err != nil {
		t.Fatalf("TransitionTask failed: %v", err)
	}
}

func TestTransitionTaskWithoutNote(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read raw body to verify "note" key is genuinely absent
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read raw body: %v", err)
		}
		bodyStr := string(rawBody)

		// Verify request body via JSON decode
		var req transitionTaskRequest
		if err := json.Unmarshal(rawBody, &req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.Note != nil {
			t.Errorf("expected note to be omitted (nil), got %v", req.Note)
		}

		// Verify "note" key is genuinely absent from the raw JSON body
		if strings.Contains(bodyStr, `"note"`) {
			t.Errorf("expected 'note' key to be absent from raw body, but found it: %s", bodyStr)
		}

		// Write response
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test without note (nil)
	err := client.TransitionTask(context.Background(), "task123", "blocked", nil)
	if err != nil {
		t.Fatalf("TransitionTask failed: %v", err)
	}
}

func TestHeartbeatTask(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/heartbeat" {
			t.Errorf("expected /tasks/task123/heartbeat, got %s", r.URL.Path)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Verify request body
		var req heartbeatTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.AgentID != "agent123" {
			t.Errorf("expected agent_id=agent123, got %s", req.AgentID)
		}

		// Write response
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	err := client.HeartbeatTask(context.Background(), "task123", "agent123")
	if err != nil {
		t.Fatalf("HeartbeatTask failed: %v", err)
	}
}

// TestAPIError_StructuredBody verifies that do() returns *APIError with the correct StatusCode,
// Code, and Message when the server returns a non-2xx with a structured JSON error body.
func TestAPIError_StructuredBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "CONFLICT",
				"message": "Task is not in backlog",
			},
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	err := client.PromoteTask(context.Background(), "task123")
	if err == nil {
		t.Fatal("Expected error from 409 response, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusConflict {
		t.Errorf("Expected StatusCode 409, got %d", apiErr.StatusCode)
	}
	if apiErr.Code != "CONFLICT" {
		t.Errorf("Expected Code CONFLICT, got %q", apiErr.Code)
	}
	if apiErr.Message != "Task is not in backlog" {
		t.Errorf("Expected Message 'Task is not in backlog', got %q", apiErr.Message)
	}
	// Error() string must remain human-readable (used in generic error display).
	if !strings.Contains(err.Error(), "CONFLICT") || !strings.Contains(err.Error(), "Task is not in backlog") {
		t.Errorf("APIError.Error() does not include server code/message: %q", err.Error())
	}
}

// TestAPIError_UndecodableBody verifies that do() returns *APIError with only StatusCode set
// when the server returns a non-2xx with a non-JSON body.
func TestAPIError_UndecodableBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal server error"))
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	err := client.PromoteTask(context.Background(), "task123")
	if err == nil {
		t.Fatal("Expected error from 500 response, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected StatusCode 500, got %d", apiErr.StatusCode)
	}
	// Code and Message should be empty when body is not structured JSON.
	if apiErr.Code != "" {
		t.Errorf("Expected empty Code for undecodable body, got %q", apiErr.Code)
	}
	// Error() must still be useful.
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("APIError.Error() for fallback should include status code, got: %q", err.Error())
	}
}

func TestListEvents(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/events" {
			t.Errorf("expected /tasks/task123/events, got %s", r.URL.Path)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Write response
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		events := []Event{
			{
				ID:        "event1",
				TaskID:    "task123",
				Actor:     "system",
				Kind:      "transition",
				Verdict:   nil,
				Note:      stringPtr("backlog->ready"),
				CreatedAt: "2026-06-07T00:00:00Z",
			},
			{
				ID:        "event2",
				TaskID:    "task123",
				Actor:     "agent-1",
				Kind:      "claim",
				Verdict:   nil,
				Note:      nil,
				CreatedAt: "2026-06-07T00:01:00Z",
			},
			{
				ID:        "event3",
				TaskID:    "task123",
				Actor:     "agent-1",
				Kind:      "submit",
				Verdict:   nil,
				Note:      nil,
				CreatedAt: "2026-06-07T00:02:00Z",
			},
		}
		json.NewEncoder(w).Encode(events)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	events, err := client.ListEvents(context.Background(), "task123")
	if err != nil {
		t.Fatalf("ListEvents failed: %v", err)
	}

	if len(events) != 3 {
		t.Errorf("expected 3 events, got %d", len(events))
	}

	if events[0].Kind != "transition" {
		t.Errorf("expected first event kind 'transition', got %s", events[0].Kind)
	}

	if events[1].Kind != "claim" {
		t.Errorf("expected second event kind 'claim', got %s", events[1].Kind)
	}

	if events[2].Kind != "submit" {
		t.Errorf("expected third event kind 'submit', got %s", events[2].Kind)
	}
}

func TestListEventsEmpty(t *testing.T) {
	// Create a test server that returns an empty array
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test
	events, err := client.ListEvents(context.Background(), "task123")
	if err != nil {
		t.Fatalf("ListEvents failed: %v", err)
	}

	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

func stringPtr(s string) *string {
	return &s
}

func TestArchiveTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/archive" {
			t.Errorf("expected /tasks/task123/archive, got %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	err := client.ArchiveTask(context.Background(), "task123")
	if err != nil {
		t.Fatalf("ArchiveTask failed: %v", err)
	}
}

func TestArchiveProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/projects/proj123/archive" {
			t.Errorf("expected /projects/proj123/archive, got %s", r.URL.Path)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	err := client.ArchiveProject(context.Background(), "proj123")
	if err != nil {
		t.Fatalf("ArchiveProject failed: %v", err)
	}
}

func TestClaimTask(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/claim" {
			t.Errorf("expected /tasks/task123/claim, got %s", r.URL.Path)
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		var req claimTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.AgentID != "agent-1" {
			t.Errorf("expected agent_id agent-1, got %s", req.AgentID)
		}

		if req.Model != "haiku" {
			t.Errorf("expected model haiku, got %s", req.Model)
		}

		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	err := client.ClaimTask(context.Background(), "task123", "agent-1", "haiku")
	if err != nil {
		t.Fatalf("ClaimTask failed: %v", err)
	}
}

func TestClaimTaskAlreadyClaimed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "ALREADY_CLAIMED",
				"message": "Task is already claimed",
			},
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	err := client.ClaimTask(context.Background(), "task123", "agent-1", "haiku")
	if err == nil {
		t.Fatal("Expected error from 409 response, got nil")
	}

	if !errors.Is(err, ErrAlreadyClaimed) {
		t.Fatalf("Expected ErrAlreadyClaimed, got %T: %v", err, err)
	}
}

func TestClaimTaskServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "INTERNAL_ERROR",
				"message": "Internal server error",
			},
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	err := client.ClaimTask(context.Background(), "task123", "agent-1", "haiku")
	if err == nil {
		t.Fatal("Expected error from 500 response, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("Expected StatusCode 500, got %d", apiErr.StatusCode)
	}
}

func TestSubmitTask(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/tasks/task123/submit" {
			t.Errorf("expected /tasks/task123/submit, got %s", r.URL.Path)
		}

		// Check authorization header
		auth := r.Header.Get("Authorization")
		if auth != "Bearer testtoken" {
			t.Errorf("expected Bearer testtoken, got %s", auth)
		}

		// Verify request body
		var req submitTaskRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.AgentID != "agent123" {
			t.Errorf("expected agent_id=agent123, got %s", req.AgentID)
		}

		if req.Result != "task completed" {
			t.Errorf("expected result='task completed', got %s", req.Result)
		}

		if req.Verdict == nil || *req.Verdict != "approve" {
			t.Errorf("expected verdict='approve', got %v", req.Verdict)
		}

		if len(req.Links) != 2 {
			t.Errorf("expected 2 links, got %d", len(req.Links))
		}

		if req.Links[0].Kind != "pr" || req.Links[0].Value != "https://github.com/test/pr" {
			t.Errorf("expected first link to be pr with value https://github.com/test/pr, got %+v", req.Links[0])
		}

		if req.Links[1].Kind != "branch" || req.Links[1].Value != "feature" {
			t.Errorf("expected second link to be branch with value feature, got %+v", req.Links[1])
		}

		// Write response
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test with verdict and links
	verdict := "approve"
	links := []LinkInput{
		{Kind: "pr", Value: "https://github.com/test/pr"},
		{Kind: "branch", Value: "feature"},
	}
	err := client.SubmitTask(context.Background(), "task123", "agent123", "task completed", &verdict, links)
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}
}

func TestSubmitTaskWithoutVerdict(t *testing.T) {
	// Create a test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read raw body to verify "verdict" key is genuinely absent
		rawBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read raw body: %v", err)
		}
		bodyStr := string(rawBody)

		// Verify request body via JSON decode
		var req submitTaskRequest
		if err := json.Unmarshal(rawBody, &req); err != nil {
			t.Errorf("failed to decode request body: %v", err)
		}

		if req.Verdict != nil {
			t.Errorf("expected verdict to be omitted (nil), got %v", req.Verdict)
		}

		// Verify "verdict" key is genuinely absent from the raw JSON body
		if strings.Contains(bodyStr, `"verdict"`) {
			t.Errorf("expected 'verdict' key to be absent from raw body, but found it: %s", bodyStr)
		}

		// Verify other fields are present
		if req.AgentID != "agent456" {
			t.Errorf("expected agent_id=agent456, got %s", req.AgentID)
		}

		if req.Result != "failed" {
			t.Errorf("expected result='failed', got %s", req.Result)
		}

		// Write response
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// Create client
	client := NewHTTPClient(server.URL, "testtoken")

	// Test without verdict (nil)
	links := []LinkInput{{Kind: "no_op", Value: "already-satisfied"}}
	err := client.SubmitTask(context.Background(), "task123", "agent456", "failed", nil, links)
	if err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}
}

func TestSubmitTaskError(t *testing.T) {
	// Create a test server that returns an error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "VALIDATION_ERROR",
				"message": "Invalid input",
			},
		})
	}))
	defer server.Close()

	client := NewHTTPClient(server.URL, "testtoken")
	links := []LinkInput{{Kind: "pr", Value: "https://github.com/test/pr"}}
	err := client.SubmitTask(context.Background(), "task123", "agent123", "result", nil, links)
	if err == nil {
		t.Fatal("Expected error from 400 response, got nil")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Expected *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("Expected StatusCode 400, got %d", apiErr.StatusCode)
	}
}
