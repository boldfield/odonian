package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/boldfield/odonian/internal/store"
)

func makeSetPriorityRequest(taskID string, actionKey string, priority interface{}, actor, reason string) *http.Request {
	payload := make(map[string]interface{})
	payload["action_key"] = actionKey
	if priority != nil {
		payload["priority"] = priority
	}
	payload["actor"] = actor
	payload["reason"] = reason

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/set", taskID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

func makeFrontRequest(taskID string, actionKey string, actor, reason string, extraFields map[string]interface{}) *http.Request {
	payload := map[string]interface{}{
		"action_key": actionKey,
		"actor":      actor,
		"reason":     reason,
	}
	for k, v := range extraFields {
		payload[k] = v
	}

	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/front", taskID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

func setupProjectAndDocumentForTest(t *testing.T, server *Server) (string, string) {
	projectPayload := map[string]string{
		"name": "test-project",
		"repo": "https://github.com/example/test-repo",
	}
	projectBody, _ := json.Marshal(projectPayload)
	projectReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projectBody))
	projectReq.Header.Set("Authorization", "Bearer test-token")
	projectReq.Header.Set("Content-Type", "application/json")
	projectW := httptest.NewRecorder()
	server.mux.ServeHTTP(projectW, projectReq)

	if projectW.Code != http.StatusCreated {
		t.Fatalf("failed to create project: status %d", projectW.Code)
	}

	var project store.Project
	json.NewDecoder(projectW.Body).Decode(&project)

	docPayload := map[string]interface{}{
		"kind":  "design",
		"title": "Design",
		"ref":   "DESIGN.md",
	}
	docBody, _ := json.Marshal(docPayload)
	docReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(docBody))
	docReq.Header.Set("Authorization", "Bearer test-token")
	docReq.Header.Set("Content-Type", "application/json")
	docW := httptest.NewRecorder()
	server.mux.ServeHTTP(docW, docReq)

	if docW.Code != http.StatusCreated {
		t.Fatalf("failed to create document: status %d", docW.Code)
	}

	var doc store.Document
	json.NewDecoder(docW.Body).Decode(&doc)

	return project.ID, doc.ID
}

func createTestTask(t *testing.T, server *Server, projectID, documentID string) store.Task {
	taskPayload := map[string]interface{}{
		"title":       "Test Task",
		"spec":        "Test spec",
		"document_id": documentID,
	}
	body, _ := json.Marshal([]interface{}{taskPayload})
	req := httptest.NewRequest("POST", fmt.Sprintf("/projects/%s/tasks", projectID), bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("failed to create test task: status %d, body: %s", w.Code, w.Body.String())
	}

	var tasks []store.Task
	if err := json.NewDecoder(w.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode created tasks: %v", err)
	}

	if len(tasks) == 0 {
		t.Fatalf("no tasks created")
	}

	return tasks[0]
}

func TestSetPriorityBoundaryValues(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	tests := []struct {
		name           string
		priority       interface{}
		expectedStatus int
		expectedCode   string
	}{
		{"missing priority", nil, http.StatusBadRequest, "JSON_DECODE_ERROR"},
		{"zero priority", int64(0), http.StatusBadRequest, "INVALID_PRIORITY"},
		{"negative priority", int64(-1), http.StatusBadRequest, "INVALID_PRIORITY"},
		{"min valid priority", int64(1), http.StatusOK, ""},
		{"default priority", int64(500), http.StatusOK, ""},
		{"max manual priority", int64(1000), http.StatusOK, ""},
		{"over max priority", int64(1001), http.StatusBadRequest, "INVALID_PRIORITY"},
		{"way over max", int64(2000), http.StatusBadRequest, "INVALID_PRIORITY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeSetPriorityRequest(taskID, fmt.Sprintf("key-%s", tt.name), tt.priority, "testactor", "test reason")
			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			if tt.expectedStatus != http.StatusOK {
				var errResp map[string]interface{}
				json.NewDecoder(w.Body).Decode(&errResp)
				errObj := errResp["error"].(map[string]interface{})
				if errObj["code"] != tt.expectedCode {
					t.Errorf("expected code %s, got %s", tt.expectedCode, errObj["code"])
				}
			}
		})
	}
}

func TestFrontRejectsNumericOverride(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	req := makeFrontRequest(taskID, "front-key-1", "testactor", "test reason", map[string]interface{}{
		"priority": 999,
	})
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for numeric override on Front, got %d", w.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "JSON_DECODE_ERROR" {
		t.Errorf("expected JSON_DECODE_ERROR for unknown priority field, got %s", errObj["code"])
	}
}

func TestFrontRejectsOtherUnknownFields(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	payload := map[string]interface{}{
		"action_key":  "front-key-unknown",
		"actor":       "testactor",
		"reason":      "test reason",
		"extra_field": "should be rejected",
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/front", taskID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400 for unknown field on Front, got %d", w.Code)
	}
}

func TestIdempotentReplay(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	req1 := makeSetPriorityRequest(taskID, "idempotent-key-1", int64(500), "testactor", "first call")
	w1 := httptest.NewRecorder()
	server.mux.ServeHTTP(w1, req1)

	var result1 map[string]interface{}
	json.NewDecoder(w1.Body).Decode(&result1)

	req2 := makeSetPriorityRequest(taskID, "idempotent-key-1", int64(500), "testactor", "first call")
	w2 := httptest.NewRecorder()
	server.mux.ServeHTTP(w2, req2)

	var result2 map[string]interface{}
	json.NewDecoder(w2.Body).Decode(&result2)

	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Fatalf("expected both calls to succeed: %d, %d", w1.Code, w2.Code)
	}

	if result1["priority"] != result2["priority"] {
		t.Errorf("expected replayed result to be identical, got %v and %v", result1["priority"], result2["priority"])
	}

	if result2["replayed"] != true {
		t.Errorf("expected replayed flag to be true on second call, got %v", result2["replayed"])
	}
}

func TestIdempotencyMismatch(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	req1 := makeSetPriorityRequest(taskID, "mismatch-key", int64(500), "testactor", "first call")
	w1 := httptest.NewRecorder()
	server.mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("first call failed: %d", w1.Code)
	}

	req2 := makeSetPriorityRequest(taskID, "mismatch-key", int64(600), "testactor", "second call with different priority")
	w2 := httptest.NewRecorder()
	server.mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Errorf("expected 409 for idempotency mismatch, got %d", w2.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(w2.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "IDEMPOTENCY_MISMATCH" {
		t.Errorf("expected IDEMPOTENCY_MISMATCH, got %s", errObj["code"])
	}
}

func TestSetPriorityValidationErrors(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	tests := []struct {
		name         string
		actionKey    string
		expectedCode string
	}{
		{"empty action_key", "", "INVALID_ACTION_KEY"},
		{"long action_key", strings.Repeat("a", 201), "INVALID_ACTION_KEY"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeSetPriorityRequest(taskID, tt.actionKey, int64(500), "testactor", "test reason")
			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected status 400, got %d", w.Code)
			}

			var errResp map[string]interface{}
			json.NewDecoder(w.Body).Decode(&errResp)
			errObj := errResp["error"].(map[string]interface{})
			if errObj["code"] != tt.expectedCode {
				t.Errorf("expected code %s, got %s", tt.expectedCode, errObj["code"])
			}
		})
	}
}

func TestFrontReturnsHighPriority(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)

	task1 := createTestTask(t, server, projectID, docID)
	task2 := createTestTask(t, server, projectID, docID)

	req1 := makeSetPriorityRequest(task2.ID, "setup-key-1", int64(500), "testactor", "setup")
	w1 := httptest.NewRecorder()
	server.mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("setup request failed: %d", w1.Code)
	}

	req2 := makeFrontRequest(task1.ID, "front-key-1", "testactor", "move to front", nil)
	w2 := httptest.NewRecorder()
	server.mux.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("front request failed: %d, %s", w2.Code, w2.Body.String())
	}

	var result map[string]interface{}
	json.NewDecoder(w2.Body).Decode(&result)

	priority := int64(result["priority"].(float64))
	if priority <= 1000 {
		t.Errorf("expected Front to return priority > 1000, got %d", priority)
	}
}

func TestSetPriorityNotFound(t *testing.T) {
	server := setupTestServer(t, "test-token")

	req := makeSetPriorityRequest("nonexistent-task-id", "key-notfound", int64(500), "testactor", "test")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "NOT_FOUND" {
		t.Errorf("expected NOT_FOUND, got %s", errObj["code"])
	}
}

func TestFrontNotFound(t *testing.T) {
	server := setupTestServer(t, "test-token")

	req := makeFrontRequest("nonexistent-task-id", "key-notfound", "testactor", "test", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "NOT_FOUND" {
		t.Errorf("expected NOT_FOUND, got %s", errObj["code"])
	}
}

func TestSetPriorityArchivedTask(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	archiveReq := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/archive", task.ID), nil)
	archiveReq.Header.Set("Authorization", "Bearer test-token")
	archiveW := httptest.NewRecorder()
	server.mux.ServeHTTP(archiveW, archiveReq)

	if archiveW.Code != http.StatusOK {
		t.Fatalf("failed to archive task: status %d", archiveW.Code)
	}

	req := makeSetPriorityRequest(task.ID, "key-archived", int64(500), "testactor", "test")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected status 409 for archived task, got %d", w.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "ARCHIVED" {
		t.Errorf("expected ARCHIVED, got %s", errObj["code"])
	}
}

func TestTaskSummaryIncludesPriority(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	req := makeSetPriorityRequest(taskID, "summary-key", int64(750), "testactor", "set priority")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set priority failed: %d", w.Code)
	}

	getReq := httptest.NewRequest("GET", fmt.Sprintf("/tasks/%s", taskID), nil)
	getReq.Header.Set("Authorization", "Bearer test-token")
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Fatalf("get task failed: %d", getW.Code)
	}

	var result map[string]interface{}
	json.NewDecoder(getW.Body).Decode(&result)

	if priority, ok := result["priority"]; !ok {
		t.Errorf("expected priority field in task summary")
	} else if int64(priority.(float64)) != 750 {
		t.Errorf("expected priority 750 in summary, got %v", priority)
	}

	if topicAnchorID, ok := result["topic_anchor_id"]; !ok {
		t.Errorf("expected topic_anchor_id field in task summary")
	} else if topicAnchorID != taskID {
		t.Errorf("expected topic_anchor_id to be %s, got %v", taskID, topicAnchorID)
	}
}

func TestListTasksIncludesTopicFields(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	req := makeSetPriorityRequest(task.ID, "list-key", int64(600), "testactor", "set priority")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("set priority failed: %d", w.Code)
	}

	listReq := httptest.NewRequest("GET", fmt.Sprintf("/projects/%s/tasks", projectID), nil)
	listReq.Header.Set("Authorization", "Bearer test-token")
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Fatalf("list tasks failed: %d", listW.Code)
	}

	var tasks []map[string]interface{}
	json.NewDecoder(listW.Body).Decode(&tasks)

	found := false
	for _, taskObj := range tasks {
		if taskObj["id"] == task.ID {
			found = true
			if _, ok := taskObj["priority"]; !ok {
				t.Errorf("expected priority field in task list item")
			}
			if _, ok := taskObj["topic_anchor_id"]; !ok {
				t.Errorf("expected topic_anchor_id field in task list item")
			}
		}
	}
	if !found {
		t.Errorf("task not found in list")
	}
}

func TestSetPriorityMissingFields(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)
	taskID := task.ID

	tests := []struct {
		name           string
		payload        map[string]interface{}
		expectedStatus int
		expectedCode   string
	}{
		{
			"missing action_key",
			map[string]interface{}{
				"priority": int64(500),
				"actor":    "testactor",
				"reason":   "test",
			},
			http.StatusBadRequest,
			"INVALID_ACTION_KEY",
		},
		{
			"missing priority",
			map[string]interface{}{
				"action_key": "key-missing-priority",
				"actor":      "testactor",
				"reason":     "test",
			},
			http.StatusBadRequest,
			"JSON_DECODE_ERROR",
		},
		{
			"missing actor",
			map[string]interface{}{
				"action_key": "key-missing-actor",
				"priority":   int64(500),
				"reason":     "test",
			},
			http.StatusBadRequest,
			"ACTOR_REQUIRED",
		},
		{
			"missing reason",
			map[string]interface{}{
				"action_key": "key-missing-reason",
				"priority":   int64(500),
				"actor":      "testactor",
			},
			http.StatusBadRequest,
			"REASON_REQUIRED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.payload)
			req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/set", taskID), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-token")

			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)

			if w.Code != tt.expectedStatus {
				t.Errorf("expected status %d, got %d", tt.expectedStatus, w.Code)
			}

			var errResp map[string]interface{}
			json.NewDecoder(w.Body).Decode(&errResp)
			errObj := errResp["error"].(map[string]interface{})
			if errObj["code"] != tt.expectedCode {
				t.Errorf("expected code %s, got %s", tt.expectedCode, errObj["code"])
			}
		})
	}
}
