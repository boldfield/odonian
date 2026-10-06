package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestFrontRegressionOrdering verifies the spec requirement: Front returns value > 1000,
// and successive Front calls return strictly increasing values, exceeding all manual priorities.
func TestFrontRegressionOrdering(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)

	topic := createTestTask(t, server, projectID, docID)
	task2 := createTestTask(t, server, projectID, docID)
	task3 := createTestTask(t, server, projectID, docID)

	// Set topic to max manual priority (500)
	req1 := makeSetPriorityRequest(topic.ID, "setup-500", int64(500), "testactor", "set to 500")
	w1 := httptest.NewRecorder()
	server.mux.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("setup request failed: %d", w1.Code)
	}

	// Move topic to front: should be > 1000
	reqFront1 := makeFrontRequest(topic.ID, "front-1", "testactor", "move to front", nil)
	wFront1 := httptest.NewRecorder()
	server.mux.ServeHTTP(wFront1, reqFront1)
	if wFront1.Code != http.StatusOK {
		t.Fatalf("front request 1 failed: %d, %s", wFront1.Code, wFront1.Body.String())
	}

	var result1 map[string]interface{}
	json.NewDecoder(wFront1.Body).Decode(&result1)
	priority1 := int64(result1["priority"].(float64))

	if priority1 <= 1000 {
		t.Errorf("expected Front to return > 1000, got %d", priority1)
	}

	// Try to set manual priority 505 on task2: should succeed
	req2 := makeSetPriorityRequest(task2.ID, "set-505", int64(505), "testactor", "set to 505")
	w2 := httptest.NewRecorder()
	server.mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("manual set 505 failed: %d", w2.Code)
	}

	// Try to set manual priority 1000 on task3: should succeed
	req3 := makeSetPriorityRequest(task3.ID, "set-1000", int64(1000), "testactor", "set to 1000")
	w3 := httptest.NewRecorder()
	server.mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("manual set 1000 failed: %d", w3.Code)
	}

	// Move topic to front again: should be > first Front value
	reqFront2 := makeFrontRequest(topic.ID, "front-2", "testactor", "move to front again", nil)
	wFront2 := httptest.NewRecorder()
	server.mux.ServeHTTP(wFront2, reqFront2)
	if wFront2.Code != http.StatusOK {
		t.Fatalf("front request 2 failed: %d, %s", wFront2.Code, wFront2.Body.String())
	}

	var result2 map[string]interface{}
	json.NewDecoder(wFront2.Body).Decode(&result2)
	priority2 := int64(result2["priority"].(float64))

	if priority2 <= priority1 {
		t.Errorf("expected second Front to be higher than first (%d), got %d", priority1, priority2)
	}

	if priority2 <= 1000 {
		t.Errorf("expected Front to always return > 1000, got %d", priority2)
	}
}

// TestSetPriorityAuth verifies 401 response for missing/invalid auth.
func TestSetPriorityAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	tests := []struct {
		name       string
		authHeader string
	}{
		{"missing auth", ""},
		{"invalid token", "Bearer wrong-token"},
		{"invalid format", "NotBearer test-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"action_key": "auth-test-key",
				"priority":   int64(500),
				"actor":      "testactor",
				"reason":     "test",
			}
			body, _ := json.Marshal(payload)
			req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/set", task.ID), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", w.Code)
			}
		})
	}
}

// TestFrontAuth verifies 401 response for missing/invalid auth on Front endpoint.
func TestFrontAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	tests := []struct {
		name       string
		authHeader string
	}{
		{"missing auth", ""},
		{"invalid token", "Bearer wrong-token"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := makeFrontRequest(task.ID, "auth-key", "testactor", "test", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			} else {
				req.Header.Del("Authorization")
			}

			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d", w.Code)
			}
		})
	}
}

// TestSetPriorityFractionalPayloads verifies rejection of fractional values like 1.5.
func TestSetPriorityFractionalPayloads(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	tests := []struct {
		name    string
		payload string
	}{
		{"fractional 1.5", `{"action_key":"key1","priority":1.5,"actor":"testactor","reason":"test"}`},
		{"exponent form 1e3", `{"action_key":"key2","priority":1e3,"actor":"testactor","reason":"test"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/set", task.ID), bytes.NewReader([]byte(tt.payload)))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-token")

			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)

			if w.Code != http.StatusBadRequest {
				t.Errorf("expected 400 for fractional/exponent, got %d", w.Code)
			}
		})
	}
}

// TestSetPriorityTrailingJSON verifies rejection of trailing JSON data.
func TestSetPriorityTrailingJSON(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	// Valid JSON followed by more JSON
	payload := `{"action_key":"key1","priority":500,"actor":"testactor","reason":"test"}{"priority":999}`
	req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/set", task.ID), bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for trailing JSON, got %d", w.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(w.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "JSON_DECODE_ERROR" {
		t.Errorf("expected JSON_DECODE_ERROR, got %s", errObj["code"])
	}
}

// TestFrontTrailingJSON verifies rejection of trailing JSON on Front endpoint.
func TestFrontTrailingJSON(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	payload := `{"action_key":"key1","actor":"testactor","reason":"test"}{"priority":999}`
	req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/front", task.ID), bytes.NewReader([]byte(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")

	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for trailing JSON on Front, got %d", w.Code)
	}
}

// TestHeldTopicStaysHeldAfterSetPriority verifies that holding a task is preserved after priority set.
func TestHeldTopicStaysHeldAfterSetPriority(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	// Hold the task
	holdReq := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/hold", task.ID), bytes.NewReader([]byte(`{"reason":"test hold"}`)))
	holdReq.Header.Set("Content-Type", "application/json")
	holdReq.Header.Set("Authorization", "Bearer test-token")
	holdW := httptest.NewRecorder()
	server.mux.ServeHTTP(holdW, holdReq)

	if holdW.Code != http.StatusOK {
		t.Fatalf("failed to hold task: %d", holdW.Code)
	}

	// Set priority
	req := makeSetPriorityRequest(task.ID, "held-set-key", int64(600), "testactor", "set priority on held task")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("set priority on held task failed: %d", w.Code)
	}

	// Verify task is still held
	getReq := httptest.NewRequest("GET", fmt.Sprintf("/tasks/%s", task.ID), nil)
	getReq.Header.Set("Authorization", "Bearer test-token")
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var result map[string]interface{}
	json.NewDecoder(getW.Body).Decode(&result)

	if result["held"] != true {
		t.Errorf("expected task to remain held after set priority, got held=%v", result["held"])
	}
}

// TestHeldTopicStaysHeldAfterFront verifies that holding a task is preserved after Front.
func TestHeldTopicStaysHeldAfterFront(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	// Hold the task
	holdReq := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/hold", task.ID), bytes.NewReader([]byte(`{"reason":"test hold"}`)))
	holdReq.Header.Set("Content-Type", "application/json")
	holdReq.Header.Set("Authorization", "Bearer test-token")
	holdW := httptest.NewRecorder()
	server.mux.ServeHTTP(holdW, holdReq)

	if holdW.Code != http.StatusOK {
		t.Fatalf("failed to hold task: %d", holdW.Code)
	}

	// Move to front
	req := makeFrontRequest(task.ID, "held-front-key", "testactor", "front on held task", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("front on held task failed: %d, %s", w.Code, w.Body.String())
	}

	// Verify task is still held
	getReq := httptest.NewRequest("GET", fmt.Sprintf("/tasks/%s", task.ID), nil)
	getReq.Header.Set("Authorization", "Bearer test-token")
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var result map[string]interface{}
	json.NewDecoder(getW.Body).Decode(&result)

	if result["held"] != true {
		t.Errorf("expected task to remain held after Front, got held=%v", result["held"])
	}
}

// TestCreateTaskWithPriority verifies the create endpoint accepts and validates priority.
func TestCreateTaskWithPriority(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)

	testCases := []struct {
		name           string
		priority       interface{}
		expectedStatus int
	}{
		{"default (missing)", nil, http.StatusCreated},
		{"valid min", int64(1), http.StatusCreated},
		{"valid max", int64(1000), http.StatusCreated},
		{"valid mid", int64(500), http.StatusCreated},
		{"out of range low", int64(0), http.StatusBadRequest},
		{"out of range high", int64(1001), http.StatusBadRequest},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]interface{}{
				"title":       fmt.Sprintf("Task %s", tc.name),
				"spec":        "Test spec",
				"document_id": docID,
			}
			if tc.priority != nil {
				payload["priority"] = tc.priority
			}

			body, _ := json.Marshal([]interface{}{payload})
			req := httptest.NewRequest("POST", fmt.Sprintf("/projects/%s/tasks", projectID), bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer test-token")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			server.mux.ServeHTTP(w, req)

			if w.Code != tc.expectedStatus {
				t.Fatalf("expected status %d, got %d: %s", tc.expectedStatus, w.Code, w.Body.String())
			}

			if tc.expectedStatus == http.StatusBadRequest {
				var result map[string]interface{}
				json.NewDecoder(w.Body).Decode(&result)
				if errObj, ok := result["error"].(map[string]interface{}); ok {
					if code, ok := errObj["code"].(string); !ok || code != "INVALID_PRIORITY" {
						t.Errorf("expected error code INVALID_PRIORITY, got %v", errObj["code"])
					}
				} else {
					t.Errorf("expected error field in response, got %v", result)
				}
			}
		})
	}
}

// TestSetPriorityOverflow verifies overflow values are rejected.
func TestSetPriorityOverflow(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	// Send raw JSON with a priority larger than int64 max
	body := []byte(`{"action_key":"overflow-key","priority":9223372036854775808,"actor":"testactor","reason":"overflow"}`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/set", task.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for overflow, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)

	if errObj, ok := result["error"].(map[string]interface{}); ok {
		if code, ok := errObj["code"].(string); !ok || code != "JSON_DECODE_ERROR" {
			t.Errorf("expected JSON_DECODE_ERROR for overflow, got %v", errObj["code"])
		}
	}
}

// TestStrayClosingToken verifies trailing close brackets are rejected.
func TestStrayClosingToken(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)
	task := createTestTask(t, server, projectID, docID)

	// Send JSON with stray closing brace
	body := []byte(`{"action_key":"key1","priority":500,"actor":"op","reason":"test"}}`)
	req := httptest.NewRequest("POST", fmt.Sprintf("/tasks/%s/priority/set", task.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for stray closing token, got %d: %s", w.Code, w.Body.String())
	}

	var result map[string]interface{}
	json.NewDecoder(w.Body).Decode(&result)

	if errObj, ok := result["error"].(map[string]interface{}); ok {
		if code, ok := errObj["code"].(string); !ok || code != "JSON_DECODE_ERROR" {
			t.Errorf("expected JSON_DECODE_ERROR, got %v", errObj["code"])
		}
	} else {
		t.Errorf("expected error field in response, got %v", result)
	}
}

// TestFrontIncreasesOnSuccessiveCalls verifies each Front call returns a higher priority.
func TestFrontIncreasesOnSuccessiveCalls(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)

	// Create tasks
	task1 := createTestTask(t, server, projectID, docID)
	task2 := createTestTask(t, server, projectID, docID)

	// Move task1 to front
	frontReq1 := makeFrontRequest(task1.ID, "front-call-1", "testactor", "first front", nil)
	frontW1 := httptest.NewRecorder()
	server.mux.ServeHTTP(frontW1, frontReq1)
	var frontResult1 map[string]interface{}
	json.NewDecoder(frontW1.Body).Decode(&frontResult1)
	priority1 := int64(frontResult1["priority"].(float64))

	// Priority should be > 1000
	if priority1 <= 1000 {
		t.Errorf("expected front priority > 1000, got %d", priority1)
	}

	// Move task2 to front
	frontReq2 := makeFrontRequest(task2.ID, "front-call-2", "testactor", "second front", nil)
	frontW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(frontW2, frontReq2)
	var frontResult2 map[string]interface{}
	json.NewDecoder(frontW2.Body).Decode(&frontResult2)
	priority2 := int64(frontResult2["priority"].(float64))

	// Second front call should return higher priority
	if priority2 <= priority1 {
		t.Errorf("second front should have priority > %d, got %d", priority1, priority2)
	}
}

func doAuthedJSON(t *testing.T, server *Server, method, path string, payload interface{}) (int, map[string]interface{}) {
	t.Helper()
	var reader *bytes.Reader
	if payload != nil {
		body, _ := json.Marshal(payload)
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	var out map[string]interface{}
	json.NewDecoder(w.Body).Decode(&out)
	return w.Code, out
}

func promoteTestTask(t *testing.T, server *Server, taskID string) {
	t.Helper()
	code, body := doAuthedJSON(t, server, "POST", "/tasks/"+taskID+"/promote", nil)
	if code != http.StatusOK {
		t.Fatalf("promote %s failed: %d %v", taskID, code, body)
	}
}

func listTaskIDsInOrder(t *testing.T, server *Server, projectID string) []string {
	t.Helper()
	req := httptest.NewRequest("GET", fmt.Sprintf("/projects/%s/tasks?claimable=true&model=haiku", projectID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list tasks failed: %d", w.Code)
	}
	var tasks []map[string]interface{}
	json.NewDecoder(w.Body).Decode(&tasks)
	ids := make([]string, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task["id"].(string))
	}
	return ids
}

func indexOfID(ids []string, id string) int {
	for i, candidate := range ids {
		if candidate == id {
			return i
		}
	}
	return -1
}

// TestFrontRegressionExactOrdering pins the spec regression: queue max 500 -> Front is exactly
// 1001; later manual 505 and 1000 cannot overtake it; a later Front on another topic can.
func TestFrontRegressionExactOrdering(t *testing.T) {
	// The shared in-memory test store accumulates other tests' Front values, so use a private DB.
	isolatedStore, err := store.Open(filepath.Join(t.TempDir(), "front-regression.db"), defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open isolated store: %v", err)
	}
	t.Cleanup(func() { isolatedStore.Close() })
	server := New(isolatedStore, "test-token", 5*time.Minute, 5, nil, nil, 999999, false, 500, nil)
	projectID, docID := setupProjectAndDocumentForTest(t, server)

	fronted := createTestTask(t, server, projectID, docID)
	manual505 := createTestTask(t, server, projectID, docID)
	manual1000 := createTestTask(t, server, projectID, docID)
	laterFront := createTestTask(t, server, projectID, docID)
	for _, task := range []store.Task{fronted, manual505, manual1000, laterFront} {
		promoteTestTask(t, server, task.ID)
	}

	code, result := doAuthedJSON(t, server, "POST", "/tasks/"+fronted.ID+"/priority/front",
		map[string]interface{}{"action_key": "regress-front-1", "actor": "op", "reason": "urgent"})
	if code != http.StatusOK {
		t.Fatalf("front failed: %d %v", code, result)
	}
	if got := int64(result["priority"].(float64)); got != 1001 {
		t.Fatalf("expected Front on queue max 500 to return exactly 1001, got %d (queue_max_priority=%v)", got, result["queue_max_priority"])
	}
	if got := int64(result["queue_max_priority"].(float64)); got != 500 {
		t.Fatalf("expected queue_max_priority 500, got %d", got)
	}

	for i, manual := range []struct {
		task     store.Task
		priority int64
	}{{manual505, 505}, {manual1000, 1000}} {
		code, body := doAuthedJSON(t, server, "POST", "/tasks/"+manual.task.ID+"/priority/set",
			map[string]interface{}{"action_key": fmt.Sprintf("regress-set-%d", i), "priority": manual.priority, "actor": "op", "reason": "manual"})
		if code != http.StatusOK {
			t.Fatalf("manual set %d failed: %d %v", manual.priority, code, body)
		}
	}

	order := listTaskIDsInOrder(t, server, projectID)
	if len(order) < 4 {
		t.Fatalf("expected 4 claimable tasks, got %v", order)
	}
	wantPrefix := []string{fronted.ID, manual1000.ID, manual505.ID, laterFront.ID}
	for i, id := range wantPrefix {
		if order[i] != id {
			t.Fatalf("order mismatch at %d: want %v got %v", i, wantPrefix, order)
		}
	}

	code, result = doAuthedJSON(t, server, "POST", "/tasks/"+laterFront.ID+"/priority/front",
		map[string]interface{}{"action_key": "regress-front-2", "actor": "op", "reason": "more urgent"})
	if code != http.StatusOK {
		t.Fatalf("later front failed: %d %v", code, result)
	}
	if got := int64(result["priority"].(float64)); got != 1002 {
		t.Fatalf("expected later Front to return 1002, got %d", got)
	}

	order = listTaskIDsInOrder(t, server, projectID)
	if order[0] != laterFront.ID || order[1] != fronted.ID {
		t.Fatalf("later Front should move ahead of earlier Front; got order %v", order)
	}
}

// TestPriorityEndpointsPrefixResolution covers unique-prefix success and ambiguous-prefix errors
// for both priority routes.
func TestPriorityEndpointsPrefixResolution(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)

	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range []string{"prioamb1-task-full-a", "prioamb1-task-full-b"} {
		if _, err := server.store.Conn().Exec(`
			INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, id, projectID, docID, "Ambiguous "+id, "spec", "backlog", now, now); err != nil {
			t.Fatalf("failed to insert task %s: %v", id, err)
		}
	}

	task := createTestTask(t, server, projectID, docID)
	prefix := task.ID[:12]

	t.Run("set via unique prefix", func(t *testing.T) {
		code, body := doAuthedJSON(t, server, "POST", "/tasks/"+prefix+"/priority/set",
			map[string]interface{}{"action_key": "prefix-set", "priority": 321, "actor": "op", "reason": "r"})
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d %v", code, body)
		}
		if body["task_id"] != task.ID {
			t.Errorf("expected resolved task_id %s, got %v", task.ID, body["task_id"])
		}
		if int64(body["priority"].(float64)) != 321 {
			t.Errorf("expected priority 321, got %v", body["priority"])
		}
	})

	t.Run("front via unique prefix", func(t *testing.T) {
		code, body := doAuthedJSON(t, server, "POST", "/tasks/"+prefix+"/priority/front",
			map[string]interface{}{"action_key": "prefix-front", "actor": "op", "reason": "r"})
		if code != http.StatusOK {
			t.Fatalf("expected 200, got %d %v", code, body)
		}
		if body["task_id"] != task.ID {
			t.Errorf("expected resolved task_id %s, got %v", task.ID, body["task_id"])
		}
		if int64(body["priority"].(float64)) <= 1000 {
			t.Errorf("expected Front priority > 1000, got %v", body["priority"])
		}
	})

	for _, route := range []struct {
		name    string
		path    string
		payload map[string]interface{}
	}{
		{"set", "/tasks/prioamb1/priority/set", map[string]interface{}{"action_key": "amb-set", "priority": 100, "actor": "op", "reason": "r"}},
		{"front", "/tasks/prioamb1/priority/front", map[string]interface{}{"action_key": "amb-front", "actor": "op", "reason": "r"}},
	} {
		t.Run("ambiguous prefix "+route.name, func(t *testing.T) {
			code, body := doAuthedJSON(t, server, "POST", route.path, route.payload)
			if code != http.StatusConflict {
				t.Fatalf("expected 409, got %d %v", code, body)
			}
			errObj, _ := body["error"].(map[string]interface{})
			if errObj["code"] != "AMBIGUOUS_ID" {
				t.Errorf("expected AMBIGUOUS_ID, got %v", errObj["code"])
			}
		})
	}

	t.Run("unknown prefix", func(t *testing.T) {
		code, _ := doAuthedJSON(t, server, "POST", "/tasks/zzzzzzzz/priority/front",
			map[string]interface{}{"action_key": "unk", "actor": "op", "reason": "r"})
		if code != http.StatusNotFound {
			t.Fatalf("expected 404, got %d", code)
		}
	})
}

// TestInheritedPrioritySerialization verifies a descendant (spawned review task) reports its
// topic anchor's effective priority, including a server-generated value above 1000.
func TestInheritedPrioritySerialization(t *testing.T) {
	server := setupTestServer(t, "test-token")
	projectID, docID := setupProjectAndDocumentForTest(t, server)

	anchor := createTestTask(t, server, projectID, docID)
	promoteTestTask(t, server, anchor.ID)

	code, front := doAuthedJSON(t, server, "POST", "/tasks/"+anchor.ID+"/priority/front",
		map[string]interface{}{"action_key": "inherit-front", "actor": "op", "reason": "urgent"})
	if code != http.StatusOK {
		t.Fatalf("front failed: %d %v", code, front)
	}
	frontPriority := front["priority"].(float64)
	if frontPriority <= 1000 {
		t.Fatalf("expected Front > 1000, got %v", frontPriority)
	}

	code, body := doAuthedJSON(t, server, "POST", "/tasks/"+anchor.ID+"/claim",
		map[string]interface{}{"agent_id": "agent-1", "model": "haiku"})
	if code != http.StatusOK {
		t.Fatalf("claim failed: %d %v", code, body)
	}
	code, body = doAuthedJSON(t, server, "POST", "/tasks/"+anchor.ID+"/submit", map[string]interface{}{
		"agent_id": "agent-1",
		"result":   "done",
		"links":    []map[string]string{{"kind": "pr", "value": "https://github.com/example/test-repo/pull/1"}},
	})
	if code != http.StatusOK {
		t.Fatalf("submit failed: %d %v", code, body)
	}

	req := httptest.NewRequest("GET", fmt.Sprintf("/projects/%s/tasks", projectID), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	var tasks []map[string]interface{}
	json.NewDecoder(w.Body).Decode(&tasks)

	var reviewID string
	for _, task := range tasks {
		if task["kind"] == "review" && task["target_task_id"] == anchor.ID {
			reviewID = task["id"].(string)
			if task["priority"] != frontPriority {
				t.Errorf("list: review priority = %v, want anchor's %v", task["priority"], frontPriority)
			}
			if task["topic_anchor_id"] != anchor.ID {
				t.Errorf("list: review topic_anchor_id = %v, want %s", task["topic_anchor_id"], anchor.ID)
			}
		}
	}
	if reviewID == "" {
		t.Fatalf("review descendant not found in list")
	}

	code, detail := doAuthedJSON(t, server, "GET", "/tasks/"+reviewID, nil)
	if code != http.StatusOK {
		t.Fatalf("get review failed: %d", code)
	}
	if detail["priority"] != frontPriority {
		t.Errorf("detail: review priority = %v, want anchor's %v", detail["priority"], frontPriority)
	}
	if detail["topic_anchor_id"] != anchor.ID {
		t.Errorf("detail: review topic_anchor_id = %v, want %s", detail["topic_anchor_id"], anchor.ID)
	}

	code, detail = doAuthedJSON(t, server, "GET", "/tasks/"+anchor.ID, nil)
	if code != http.StatusOK || detail["priority"] != frontPriority || detail["topic_anchor_id"] != anchor.ID {
		t.Errorf("anchor detail priority/topic_anchor_id wrong: %v", detail)
	}
}
