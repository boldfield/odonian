package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/store"
)

func defaultTestAllowedModels() []string {
	return []string{"haiku", "sonnet", "opus"}
}

func setupTestServer(t *testing.T, authToken string) *Server {
	// Use in-memory database for testing
	s, err := store.Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	return New(s, authToken, 5*time.Minute, 5, nil, false)
}

func setupTestServerWithThresholds(t *testing.T, authToken string, thresholds map[string]int) *Server {
	// Use in-memory database for testing
	s, err := store.Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	return New(s, authToken, 5*time.Minute, 5, thresholds, false)
}

// TestHealthzWithoutAuth verifies GET /healthz returns 200 without auth.
func TestHealthzWithoutAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	req := httptest.NewRequest("GET", "/healthz", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %q", resp["status"])
	}
}

// TestProtectedRouteWithoutAuth verifies a protected route returns 401 without auth.
func TestProtectedRouteWithoutAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	// Register a test protected handler
	server.mux.HandleFunc("GET /test-protected", server.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
	}))

	req := httptest.NewRequest("GET", "/test-protected", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	code, ok := errObj["code"].(string)
	if !ok || code == "" {
		t.Errorf("error response missing 'code' field")
	}

	message, ok := errObj["message"].(string)
	if !ok || message == "" {
		t.Errorf("error response missing 'message' field")
	}
}

// TestProtectedRouteWithWrongToken verifies a protected route returns 401 with wrong token.
func TestProtectedRouteWithWrongToken(t *testing.T) {
	server := setupTestServer(t, "correct-token")

	// Register a test protected handler
	server.mux.HandleFunc("GET /test-protected", server.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
	}))

	req := httptest.NewRequest("GET", "/test-protected", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "INVALID_TOKEN" {
		t.Errorf("expected error code 'INVALID_TOKEN', got %q", code)
	}
}

// TestProtectedRouteWithCorrectToken verifies a protected route proceeds with correct token.
func TestProtectedRouteWithCorrectToken(t *testing.T) {
	server := setupTestServer(t, "correct-token")

	// Register a test protected handler
	server.mux.HandleFunc("GET /test-protected", server.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
	}))

	req := httptest.NewRequest("GET", "/test-protected", nil)
	req.Header.Set("Authorization", "Bearer correct-token")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp["result"] != "ok" {
		t.Errorf("expected result 'ok', got %q", resp["result"])
	}
}

// TestMalformedJSONResponse verifies that a malformed JSON body returns 400 with error envelope.
func TestMalformedJSONResponse(t *testing.T) {
	server := setupTestServer(t, "test-token")

	// Register a test protected handler that tries to decode JSON
	server.mux.HandleFunc("POST /test-json", server.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		if err := server.decodeJSON(w, r, &payload); err != nil {
			return // decodeJSON already wrote error response
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"result": "ok"})
	}))

	// Send malformed JSON
	body := []byte(`{"invalid json}`)
	req := httptest.NewRequest("POST", "/test-json", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "JSON_DECODE_ERROR" {
		t.Errorf("expected error code 'JSON_DECODE_ERROR', got %q", code)
	}
}

// TestCreateAndGetProjectRoundTrip verifies that creating and retrieving a project round-trips all fields.
func TestCreateAndGetProjectRoundTrip(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project
	createPayload := map[string]string{
		"name": "test-project",
		"repo": "https://github.com/example/test-repo",
	}
	createBody, _ := json.Marshal(createPayload)
	createReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", createW.Code)
	}

	var createResp store.Project
	if err := json.NewDecoder(createW.Body).Decode(&createResp); err != nil {
		t.Fatalf("failed to decode create response: %v", err)
	}

	// Verify created project fields
	if createResp.ID == "" {
		t.Error("created project missing id")
	}
	if createResp.Name != "test-project" {
		t.Errorf("expected name 'test-project', got %q", createResp.Name)
	}
	if createResp.Repo != "https://github.com/example/test-repo" {
		t.Errorf("expected repo 'https://github.com/example/test-repo', got %q", createResp.Repo)
	}
	if createResp.CreatedAt == "" {
		t.Error("created project missing created_at")
	}

	// Get the project
	getReq := httptest.NewRequest("GET", "/projects/"+createResp.ID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", getW.Code)
	}

	var getResp store.Project
	if err := json.NewDecoder(getW.Body).Decode(&getResp); err != nil {
		t.Fatalf("failed to decode get response: %v", err)
	}

	// Verify retrieved project matches created project
	if getResp.ID != createResp.ID {
		t.Errorf("expected id %q, got %q", createResp.ID, getResp.ID)
	}
	if getResp.Name != createResp.Name {
		t.Errorf("expected name %q, got %q", createResp.Name, getResp.Name)
	}
	if getResp.Repo != createResp.Repo {
		t.Errorf("expected repo %q, got %q", createResp.Repo, getResp.Repo)
	}
	if getResp.CreatedAt != createResp.CreatedAt {
		t.Errorf("expected created_at %q, got %q", createResp.CreatedAt, getResp.CreatedAt)
	}
}

// TestGetProjectNotFound verifies that getting an unknown project returns 404.
func TestGetProjectNotFound(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	getReq := httptest.NewRequest("GET", "/projects/nonexistent-id", nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", getW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(getW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "NOT_FOUND" {
		t.Errorf("expected error code 'NOT_FOUND', got %q", code)
	}
}

// TestCreateProjectEmptyName verifies that creating a project with empty name returns 400.
func TestCreateProjectEmptyName(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	createPayload := map[string]string{
		"name": "",
		"repo": "https://github.com/example/test-repo",
	}
	createBody, _ := json.Marshal(createPayload)
	createReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", createW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(createW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "EMPTY_NAME" {
		t.Errorf("expected error code 'EMPTY_NAME', got %q", code)
	}
}

// TestCreateDocumentFeatureSpec verifies that registering a feature_spec and listing returns it.
func TestCreateDocumentFeatureSpec(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project first
	projectPayload := map[string]string{
		"name": "test-project",
		"repo": "https://github.com/example/test-repo",
	}
	projectBody, _ := json.Marshal(projectPayload)
	projectReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projectBody))
	projectReq.Header.Set("Authorization", authHeader)
	projectReq.Header.Set("Content-Type", "application/json")
	projectW := httptest.NewRecorder()
	server.mux.ServeHTTP(projectW, projectReq)

	var project store.Project
	json.NewDecoder(projectW.Body).Decode(&project)

	// Create a feature_spec document
	docPayload := map[string]interface{}{
		"kind":   "feature_spec",
		"title":  "Test Feature",
		"ref":    "docs/features/test.md",
		"commit": "abc123",
	}
	docBody, _ := json.Marshal(docPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(docBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", createW.Code)
	}

	var createdDoc store.Document
	if err := json.NewDecoder(createW.Body).Decode(&createdDoc); err != nil {
		t.Fatalf("failed to decode created document: %v", err)
	}

	if createdDoc.ID == "" {
		t.Error("created document missing id")
	}
	if createdDoc.Kind != "feature_spec" {
		t.Errorf("expected kind 'feature_spec', got %q", createdDoc.Kind)
	}
	if createdDoc.Title != "Test Feature" {
		t.Errorf("expected title 'Test Feature', got %q", createdDoc.Title)
	}

	// List documents
	listReq := httptest.NewRequest("GET", "/projects/"+project.ID+"/documents", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var docs []store.Document
	if err := json.NewDecoder(listW.Body).Decode(&docs); err != nil {
		t.Fatalf("failed to decode documents list: %v", err)
	}

	if len(docs) != 1 {
		t.Errorf("expected 1 document, got %d", len(docs))
	}
	if docs[0].ID != createdDoc.ID {
		t.Errorf("expected document id %q, got %q", createdDoc.ID, docs[0].ID)
	}
}

// TestSecondDesignConflict verifies that a second design for the same project returns 409.
func TestSecondDesignConflict(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project
	projectPayload := map[string]string{
		"name": "test-project",
		"repo": "https://github.com/example/test-repo",
	}
	projectBody, _ := json.Marshal(projectPayload)
	projectReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projectBody))
	projectReq.Header.Set("Authorization", authHeader)
	projectReq.Header.Set("Content-Type", "application/json")
	projectW := httptest.NewRecorder()
	server.mux.ServeHTTP(projectW, projectReq)

	var project store.Project
	json.NewDecoder(projectW.Body).Decode(&project)

	// Create first design document
	firstDocPayload := map[string]interface{}{
		"kind":  "design",
		"title": "Design Doc 1",
		"ref":   "DESIGN.md",
	}
	firstDocBody, _ := json.Marshal(firstDocPayload)
	firstReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(firstDocBody))
	firstReq.Header.Set("Authorization", authHeader)
	firstReq.Header.Set("Content-Type", "application/json")
	firstW := httptest.NewRecorder()
	server.mux.ServeHTTP(firstW, firstReq)

	if firstW.Code != http.StatusCreated {
		t.Errorf("expected first design to succeed with 201, got %d", firstW.Code)
	}

	// Try to create second design document
	secondDocPayload := map[string]interface{}{
		"kind":  "design",
		"title": "Design Doc 2",
		"ref":   "DESIGN2.md",
	}
	secondDocBody, _ := json.Marshal(secondDocPayload)
	secondReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(secondDocBody))
	secondReq.Header.Set("Authorization", authHeader)
	secondReq.Header.Set("Content-Type", "application/json")
	secondW := httptest.NewRecorder()
	server.mux.ServeHTTP(secondW, secondReq)

	if secondW.Code != http.StatusConflict {
		t.Errorf("expected second design to return 409, got %d", secondW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(secondW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "CONFLICT" {
		t.Errorf("expected error code 'CONFLICT', got %q", code)
	}
}

// TestInvalidDocumentKind verifies that an invalid kind returns 400.
func TestInvalidDocumentKind(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project
	projectPayload := map[string]string{
		"name": "test-project",
		"repo": "https://github.com/example/test-repo",
	}
	projectBody, _ := json.Marshal(projectPayload)
	projectReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projectBody))
	projectReq.Header.Set("Authorization", authHeader)
	projectReq.Header.Set("Content-Type", "application/json")
	projectW := httptest.NewRecorder()
	server.mux.ServeHTTP(projectW, projectReq)

	var project store.Project
	json.NewDecoder(projectW.Body).Decode(&project)

	// Try to create document with invalid kind
	docPayload := map[string]interface{}{
		"kind":  "invalid_kind",
		"title": "Test",
		"ref":   "test.md",
	}
	docBody, _ := json.Marshal(docPayload)
	docReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(docBody))
	docReq.Header.Set("Authorization", authHeader)
	docReq.Header.Set("Content-Type", "application/json")
	docW := httptest.NewRecorder()
	server.mux.ServeHTTP(docW, docReq)

	if docW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", docW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(docW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "INVALID_KIND" {
		t.Errorf("expected error code 'INVALID_KIND', got %q", code)
	}
}

// TestListDocumentsWithKindFilter verifies that the kind filter works.
func TestListDocumentsWithKindFilter(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project
	projectPayload := map[string]string{
		"name": "test-project",
		"repo": "https://github.com/example/test-repo",
	}
	projectBody, _ := json.Marshal(projectPayload)
	projectReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projectBody))
	projectReq.Header.Set("Authorization", authHeader)
	projectReq.Header.Set("Content-Type", "application/json")
	projectW := httptest.NewRecorder()
	server.mux.ServeHTTP(projectW, projectReq)

	var project store.Project
	json.NewDecoder(projectW.Body).Decode(&project)

	// Create a design document
	designPayload := map[string]interface{}{
		"kind":  "design",
		"title": "Design",
		"ref":   "DESIGN.md",
	}
	designBody, _ := json.Marshal(designPayload)
	designReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(designBody))
	designReq.Header.Set("Authorization", authHeader)
	designReq.Header.Set("Content-Type", "application/json")
	designW := httptest.NewRecorder()
	server.mux.ServeHTTP(designW, designReq)

	// Create feature_spec documents
	for i := 1; i <= 2; i++ {
		featurePayload := map[string]interface{}{
			"kind":  "feature_spec",
			"title": "Feature " + string(rune(48+i)),
			"ref":   "docs/features/f" + string(rune(48+i)) + ".md",
		}
		featureBody, _ := json.Marshal(featurePayload)
		featureReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(featureBody))
		featureReq.Header.Set("Authorization", authHeader)
		featureReq.Header.Set("Content-Type", "application/json")
		featureW := httptest.NewRecorder()
		server.mux.ServeHTTP(featureW, featureReq)
	}

	// List all documents
	allReq := httptest.NewRequest("GET", "/projects/"+project.ID+"/documents", nil)
	allReq.Header.Set("Authorization", authHeader)
	allW := httptest.NewRecorder()
	server.mux.ServeHTTP(allW, allReq)

	var allDocs []store.Document
	json.NewDecoder(allW.Body).Decode(&allDocs)
	if len(allDocs) != 3 {
		t.Errorf("expected 3 total documents, got %d", len(allDocs))
	}

	// List only feature_spec documents
	filterReq := httptest.NewRequest("GET", "/projects/"+project.ID+"/documents?kind=feature_spec", nil)
	filterReq.Header.Set("Authorization", authHeader)
	filterW := httptest.NewRecorder()
	server.mux.ServeHTTP(filterW, filterReq)

	var filteredDocs []store.Document
	json.NewDecoder(filterW.Body).Decode(&filteredDocs)
	if len(filteredDocs) != 2 {
		t.Errorf("expected 2 feature_spec documents, got %d", len(filteredDocs))
	}
	for _, doc := range filteredDocs {
		if doc.Kind != "feature_spec" {
			t.Errorf("expected kind 'feature_spec', got %q", doc.Kind)
		}
	}

	// List only design documents
	designFilterReq := httptest.NewRequest("GET", "/projects/"+project.ID+"/documents?kind=design", nil)
	designFilterReq.Header.Set("Authorization", authHeader)
	designFilterW := httptest.NewRecorder()
	server.mux.ServeHTTP(designFilterW, designFilterReq)

	var designDocs []store.Document
	json.NewDecoder(designFilterW.Body).Decode(&designDocs)
	if len(designDocs) != 1 {
		t.Errorf("expected 1 design document, got %d", len(designDocs))
	}
}

// Helper to create project and document for task tests
func setupProjectAndDocument(t *testing.T, server *Server, authHeader string) (string, string) {
	// Create a project
	projectPayload := map[string]string{
		"name": "test-project",
		"repo": "https://github.com/example/test-repo",
	}
	projectBody, _ := json.Marshal(projectPayload)
	projectReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projectBody))
	projectReq.Header.Set("Authorization", authHeader)
	projectReq.Header.Set("Content-Type", "application/json")
	projectW := httptest.NewRecorder()
	server.mux.ServeHTTP(projectW, projectReq)

	var project store.Project
	json.NewDecoder(projectW.Body).Decode(&project)

	// Create a document
	docPayload := map[string]interface{}{
		"kind":  "design",
		"title": "Design",
		"ref":   "DESIGN.md",
	}
	docBody, _ := json.Marshal(docPayload)
	docReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/documents", bytes.NewReader(docBody))
	docReq.Header.Set("Authorization", authHeader)
	docReq.Header.Set("Content-Type", "application/json")
	docW := httptest.NewRecorder()
	server.mux.ServeHTTP(docW, docReq)

	var doc store.Document
	json.NewDecoder(docW.Body).Decode(&doc)

	return project.ID, doc.ID
}

// TestGetTaskPrefixResolution verifies that GET /tasks/{id} accepts a unique
// 8-to-35-character id prefix (returning the resolved task), 404s when the
// prefix matches nothing or is shorter than 8 characters, and returns 409
// AMBIGUOUS_ID with the candidate ids in the response body when the prefix
// matches several tasks.
func TestGetTaskPrefixResolution(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	now := time.Now().UTC().Format(time.RFC3339)
	insertTask := func(id, title string) {
		if _, err := server.store.Conn().Exec(`
			INSERT INTO task (id, project_id, document_id, title, spec, state, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, id, projectID, docID, title, "spec", "backlog", now, now); err != nil {
			t.Fatalf("failed to insert task %s: %v", id, err)
		}
	}

	const (
		uniqueTaskID = "uniq-task-full-id-000001"
		ambiguousID1 = "ambig0001-task-full-a"
		ambiguousID2 = "ambig0001-task-full-b"
		uniquePfx    = "uniq-tas" // uniqueTaskID[:8]
		ambiguousPfx = "ambig0001"
	)

	insertTask(uniqueTaskID, "Unique Prefix Task")
	insertTask(ambiguousID1, "Ambiguous Task 1")
	insertTask(ambiguousID2, "Ambiguous Task 2")

	t.Run("unique prefix resolves", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks/"+uniquePfx, nil)
		req.Header.Set("Authorization", authHeader)
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		var task store.TaskWithDepsAndLinks
		if err := json.NewDecoder(w.Body).Decode(&task); err != nil {
			t.Fatalf("failed to decode task: %v", err)
		}
		if task.ID != uniqueTaskID {
			t.Errorf("expected resolved id %s, got %s", uniqueTaskID, task.ID)
		}
	})

	t.Run("no match returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks/zzzzzzzz", nil)
		req.Header.Set("Authorization", authHeader)
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("short prefix returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks/ambig", nil)
		req.Header.Set("Authorization", authHeader)
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)

		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404, got %d", w.Code)
		}
	})

	t.Run("ambiguous prefix returns 409 with candidates", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/tasks/"+ambiguousPfx, nil)
		req.Header.Set("Authorization", authHeader)
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)

		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
		}

		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		errObj, ok := resp["error"].(map[string]interface{})
		if !ok {
			t.Fatalf("response missing 'error' field: %v", resp)
		}
		if code, _ := errObj["code"].(string); code != "AMBIGUOUS_ID" {
			t.Errorf("expected code AMBIGUOUS_ID, got %v", errObj["code"])
		}
		candidatesRaw, ok := errObj["candidates"].([]interface{})
		if !ok {
			t.Fatalf("expected candidates array in error response, got %v", errObj["candidates"])
		}
		candidates := make([]string, 0, len(candidatesRaw))
		for _, c := range candidatesRaw {
			s, _ := c.(string)
			candidates = append(candidates, s)
		}
		sort.Strings(candidates)
		want := []string{ambiguousID1, ambiguousID2}
		if len(candidates) != 2 || candidates[0] != want[0] || candidates[1] != want[1] {
			t.Errorf("expected candidates %v, got %v", want, candidates)
		}
	})
}

// TestTaskRoutePrefixResolution verifies that prefix resolution is wired through
// the operational /tasks/{id}/... routes, not just GET /tasks/{id}. It drives
// POST /tasks/{id}/claim — an exact-id store operation — with a unique prefix
// (must resolve and claim), an ambiguous prefix (must return 409 AMBIGUOUS_ID
// with candidates), and a no-match prefix (must return 404).
func TestTaskRoutePrefixResolution(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	now := time.Now().UTC().Format(time.RFC3339)
	insertReadyTask := func(id, title string) {
		if _, err := server.store.Conn().Exec(`
			INSERT INTO task (id, project_id, document_id, title, spec, state, model, held, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'ready', 'haiku', 0, ?, ?)
		`, id, projectID, docID, title, "spec", now, now); err != nil {
			t.Fatalf("failed to insert task %s: %v", id, err)
		}
	}

	const (
		uniqueTaskID = "clmuniq0-task-full-id-01"
		ambiguousID1 = "clmambg0-task-full-a"
		ambiguousID2 = "clmambg0-task-full-b"
		uniquePfx    = "clmuniq0" // uniqueTaskID[:8]
		ambiguousPfx = "clmambg0"
	)

	insertReadyTask(uniqueTaskID, "Claim Unique Prefix Task")
	insertReadyTask(ambiguousID1, "Claim Ambiguous Task 1")
	insertReadyTask(ambiguousID2, "Claim Ambiguous Task 2")

	claim := func(idOrPrefix string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]string{"agent_id": "test-agent", "model": "haiku"})
		req := httptest.NewRequest("POST", "/tasks/"+idOrPrefix+"/claim", bytes.NewReader(payload))
		req.Header.Set("Authorization", authHeader)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)
		return w
	}

	t.Run("unique prefix resolves and claims", func(t *testing.T) {
		w := claim(uniquePfx)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 claiming by unique prefix, got %d: %s", w.Code, w.Body.String())
		}
		var claimed store.Task
		if err := json.NewDecoder(w.Body).Decode(&claimed); err != nil {
			t.Fatalf("failed to decode claimed task: %v", err)
		}
		if claimed.ID != uniqueTaskID {
			t.Errorf("expected resolved id %s, got %s", uniqueTaskID, claimed.ID)
		}
		if claimed.State != "in_progress" {
			t.Errorf("expected state in_progress after claim, got %s", claimed.State)
		}
	})

	t.Run("no match returns 404", func(t *testing.T) {
		w := claim("zzzzzzzz")
		if w.Code != http.StatusNotFound {
			t.Errorf("expected 404 claiming by non-matching prefix, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("ambiguous prefix returns 409 with candidates", func(t *testing.T) {
		w := claim(ambiguousPfx)
		if w.Code != http.StatusConflict {
			t.Fatalf("expected 409 claiming by ambiguous prefix, got %d: %s", w.Code, w.Body.String())
		}
		var resp map[string]interface{}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		errObj, ok := resp["error"].(map[string]interface{})
		if !ok {
			t.Fatalf("response missing 'error' field: %v", resp)
		}
		if code, _ := errObj["code"].(string); code != "AMBIGUOUS_ID" {
			t.Errorf("expected code AMBIGUOUS_ID, got %v", errObj["code"])
		}
		candidatesRaw, ok := errObj["candidates"].([]interface{})
		if !ok {
			t.Fatalf("expected candidates array in error response, got %v", errObj["candidates"])
		}
		candidates := make([]string, 0, len(candidatesRaw))
		for _, c := range candidatesRaw {
			s, _ := c.(string)
			candidates = append(candidates, s)
		}
		sort.Strings(candidates)
		want := []string{ambiguousID1, ambiguousID2}
		if len(candidates) != 2 || candidates[0] != want[0] || candidates[1] != want[1] {
			t.Errorf("expected candidates %v, got %v", want, candidates)
		}
	})
}

// TestBulkCreateTasksWithIntraBatchDependency verifies bulk create persists tasks and edges.
func TestBulkCreateTasksWithIntraBatchDependency(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Bulk create two tasks where B depends on A (via A's batch key)
	taskPayload := []store.TaskInput{
		{
			Key:        "task-a",
			Title:      "Task A",
			Spec:       "Spec A",
			DocumentID: docID,
			DependsOn:  []string{},
		},
		{
			Key:        "task-b",
			Title:      "Task B",
			Spec:       "Spec B",
			DocumentID: docID,
			DependsOn:  []string{"task-a"},
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", w.Code)
	}

	var createdTasks []store.Task
	if err := json.NewDecoder(w.Body).Decode(&createdTasks); err != nil {
		t.Fatalf("failed to decode created tasks: %v", err)
	}

	if len(createdTasks) != 2 {
		t.Errorf("expected 2 created tasks, got %d", len(createdTasks))
	}

	if createdTasks[0].Title != "Task A" || createdTasks[1].Title != "Task B" {
		t.Error("task titles do not match")
	}

	taskAID := createdTasks[0].ID
	taskBID := createdTasks[1].ID

	// Get Task B and verify its dependency on Task A
	getReq := httptest.NewRequest("GET", "/tasks/"+taskBID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", getW.Code)
	}

	var taskB store.TaskWithDepsAndLinks
	if err := json.NewDecoder(getW.Body).Decode(&taskB); err != nil {
		t.Fatalf("failed to decode task B: %v", err)
	}

	if len(taskB.DependsOn) != 1 || taskB.DependsOn[0] != taskAID {
		t.Errorf("expected task B to depend on %q, got %v", taskAID, taskB.DependsOn)
	}

	// Verify both tasks are in backlog state
	if createdTasks[0].State != "backlog" || createdTasks[1].State != "backlog" {
		t.Error("tasks not in backlog state")
	}
}

// TestListTasksWithStateFilter verifies state filter works.
func TestListTasksWithStateFilter(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create two tasks
	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "Spec 1",
			DocumentID: docID,
		},
		{
			Title:      "Task 2",
			Spec:       "Spec 2",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	// List with state=backlog filter
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?state=backlog", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []store.Task
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 2 {
		t.Errorf("expected 2 backlog tasks, got %d", len(tasks))
	}

	for _, task := range tasks {
		if task.State != "backlog" {
			t.Errorf("expected task state 'backlog', got %q", task.State)
		}
	}
}

// TestClaimableFilterExcludesUnfinishedDeps verifies a task with unfinished dependencies is excluded.
func TestClaimableFilterExcludesUnfinishedDeps(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create two tasks where B depends on A
	taskPayload := []store.TaskInput{
		{
			Key:        "task-a",
			Title:      "Task A",
			Spec:       "Spec A",
			DocumentID: docID,
		},
		{
			Key:        "task-b",
			Title:      "Task B",
			Spec:       "Spec B",
			DocumentID: docID,
			DependsOn:  []string{"task-a"},
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskAID := createdTasks[0].ID
	taskBID := createdTasks[1].ID

	// Update both to state=ready (directly in store for test setup)
	conn := server.store.Conn()
	_, err := conn.ExecContext(context.Background(), "UPDATE task SET state = 'ready' WHERE id IN (?, ?)", taskAID, taskBID)
	if err != nil {
		t.Fatalf("failed to promote tasks: %v", err)
	}

	// Query with claimable=true — A should be claimable (no deps, ready), B should NOT be (A not done)
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?claimable=true", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var claimableTasks []store.Task
	json.NewDecoder(listW.Body).Decode(&claimableTasks)

	// Only A should be claimable (no deps, in ready state)
	if len(claimableTasks) != 1 {
		t.Errorf("expected 1 claimable task (A), got %d", len(claimableTasks))
	}
	if claimableTasks[0].ID != taskAID {
		t.Errorf("expected task A to be claimable, got %q", claimableTasks[0].ID)
	}

	// Now mark A as done
	_, err = conn.ExecContext(context.Background(), "UPDATE task SET state = 'done' WHERE id = ?", taskAID)
	if err != nil {
		t.Fatalf("failed to mark task A as done: %v", err)
	}

	// Query again — B should now be claimable (A is done, B is ready)
	listReq2 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?claimable=true", nil)
	listReq2.Header.Set("Authorization", authHeader)
	listW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW2, listReq2)

	var claimableTasks2 []store.Task
	json.NewDecoder(listW2.Body).Decode(&claimableTasks2)

	if len(claimableTasks2) != 1 {
		t.Errorf("expected 1 claimable task (B with A done), got %d", len(claimableTasks2))
	}
	if claimableTasks2[0].ID != taskBID {
		t.Errorf("expected task B to be claimable, got %q", claimableTasks2[0].ID)
	}
}

// TestListTasksWithModelFilter verifies model filter works.
func TestListTasksWithModelFilter(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create tasks with different models
	taskPayload := []store.TaskInput{
		{
			Title:      "Haiku Task",
			Spec:       "Spec 1",
			DocumentID: docID,
			Model:      "haiku",
		},
		{
			Title:      "Sonnet Task",
			Spec:       "Spec 2",
			DocumentID: docID,
			Model:      "sonnet",
		},
		{
			Title:      "Opus Task",
			Spec:       "Spec 3",
			DocumentID: docID,
			Model:      "opus",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	// List with model=haiku filter
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?model=haiku", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []store.Task
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 haiku task, got %d", len(tasks))
	}

	if tasks[0].Model != "haiku" {
		t.Errorf("expected model 'haiku', got %q", tasks[0].Model)
	}

	// List with model=sonnet filter
	listReq2 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?model=sonnet", nil)
	listReq2.Header.Set("Authorization", authHeader)
	listW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW2, listReq2)

	var tasks2 []store.Task
	if err := json.NewDecoder(listW2.Body).Decode(&tasks2); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks2) != 1 {
		t.Errorf("expected 1 sonnet task, got %d", len(tasks2))
	}

	if tasks2[0].Model != "sonnet" {
		t.Errorf("expected model 'sonnet', got %q", tasks2[0].Model)
	}
}

// TestListTasksWithModelAndClaimableFilters verifies model and claimable filters compose.
func TestListTasksWithModelAndClaimableFilters(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create tasks with different models
	taskPayload := []store.TaskInput{
		{
			Title:      "Haiku Task 1",
			Spec:       "Spec 1",
			DocumentID: docID,
			Model:      "haiku",
		},
		{
			Title:      "Haiku Task 2",
			Spec:       "Spec 2",
			DocumentID: docID,
			Model:      "haiku",
		},
		{
			Title:      "Sonnet Task",
			Spec:       "Spec 3",
			DocumentID: docID,
			Model:      "sonnet",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)

	// Promote all tasks to ready state
	conn := server.store.Conn()
	for _, task := range createdTasks {
		_, err := conn.ExecContext(context.Background(), "UPDATE task SET state = 'ready' WHERE id = ?", task.ID)
		if err != nil {
			t.Fatalf("failed to promote task: %v", err)
		}
	}

	// List with model=haiku and claimable=true
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?model=haiku&claimable=true", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []store.Task
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	// Should get 2 claimable haiku tasks and 0 sonnet tasks
	if len(tasks) != 2 {
		t.Errorf("expected 2 claimable haiku tasks, got %d", len(tasks))
	}

	for _, task := range tasks {
		if task.Model != "haiku" {
			t.Errorf("expected model 'haiku', got %q", task.Model)
		}
		if task.State != "ready" {
			t.Errorf("expected state 'ready', got %q", task.State)
		}
	}
}

// TestListTasksWithKindFilter verifies that the kind filter works.
func TestListTasksWithKindFilter(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create tasks (all default to implement kind)
	taskPayload := []store.TaskInput{
		{
			Title:      "Implement Task 1",
			Spec:       "Spec 1",
			DocumentID: docID,
		},
		{
			Title:      "Implement Task 2",
			Spec:       "Spec 2",
			DocumentID: docID,
		},
		{
			Title:      "Task to be Review",
			Spec:       "Spec 3",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", createW.Code)
	}

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)

	// Manually convert one task to review kind
	conn := server.store.Conn()
	reviewTaskID := createdTasks[2].ID
	_, err := conn.ExecContext(context.Background(), "UPDATE task SET kind = 'review' WHERE id = ?", reviewTaskID)
	if err != nil {
		t.Fatalf("failed to set task to review kind: %v", err)
	}

	// List with kind=implement filter
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?kind=implement", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []store.Task
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 2 {
		t.Errorf("expected 2 implement tasks, got %d", len(tasks))
	}

	for _, task := range tasks {
		if task.Kind != "implement" {
			t.Errorf("expected kind 'implement', got %q", task.Kind)
		}
	}

	// List with kind=review filter
	listReq2 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?kind=review", nil)
	listReq2.Header.Set("Authorization", authHeader)
	listW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW2, listReq2)

	var reviewTasks []store.Task
	if err := json.NewDecoder(listW2.Body).Decode(&reviewTasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(reviewTasks) != 1 {
		t.Errorf("expected 1 review task, got %d", len(reviewTasks))
	}

	if reviewTasks[0].Kind != "review" {
		t.Errorf("expected kind 'review', got %q", reviewTasks[0].Kind)
	}
}

// TestListTasksWithKindAndModelFilters verifies that kind and model filters compose (AND).
func TestListTasksWithKindAndModelFilters(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create tasks with different models
	taskPayload := []store.TaskInput{
		{
			Title:      "Opus Implement Task",
			Spec:       "Spec 1",
			DocumentID: docID,
			Model:      "opus",
		},
		{
			Title:      "Opus Task to be Review",
			Spec:       "Spec 2",
			DocumentID: docID,
			Model:      "opus",
		},
		{
			Title:      "Haiku Implement Task",
			Spec:       "Spec 3",
			DocumentID: docID,
			Model:      "haiku",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", createW.Code)
	}

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)

	// Manually convert one opus task to review kind
	conn := server.store.Conn()
	reviewTaskID := createdTasks[1].ID
	_, err := conn.ExecContext(context.Background(), "UPDATE task SET kind = 'review' WHERE id = ?", reviewTaskID)
	if err != nil {
		t.Fatalf("failed to set task to review kind: %v", err)
	}

	// List with model=opus and kind=review (should return only 1 task)
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?model=opus&kind=review", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []store.Task
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task (opus review), got %d", len(tasks))
	}

	if tasks[0].Model != "opus" || tasks[0].Kind != "review" {
		t.Errorf("expected model='opus' and kind='review', got model=%q, kind=%q", tasks[0].Model, tasks[0].Kind)
	}

	// List with model=opus (no kind filter) - should return 2 opus tasks of any kind
	listReq2 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?model=opus", nil)
	listReq2.Header.Set("Authorization", authHeader)
	listW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW2, listReq2)

	var opusTasks []store.Task
	if err := json.NewDecoder(listW2.Body).Decode(&opusTasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(opusTasks) != 2 {
		t.Errorf("expected 2 opus tasks (no kind filter), got %d", len(opusTasks))
	}
}

// TestListTasksNoKindFilter verifies that omitting kind returns all kinds (unchanged behavior).
func TestListTasksNoKindFilter(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create tasks (all default to implement kind)
	taskPayload := []store.TaskInput{
		{
			Title:      "Implement Task",
			Spec:       "Spec 1",
			DocumentID: docID,
		},
		{
			Title:      "Task to be Review",
			Spec:       "Spec 2",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", createW.Code)
	}

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)

	// Manually convert one task to review kind
	conn := server.store.Conn()
	reviewTaskID := createdTasks[1].ID
	_, err := conn.ExecContext(context.Background(), "UPDATE task SET kind = 'review' WHERE id = ?", reviewTaskID)
	if err != nil {
		t.Fatalf("failed to set task to review kind: %v", err)
	}

	// List without kind filter - should return both tasks
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []store.Task
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 2 {
		t.Errorf("expected 2 tasks (no kind filter), got %d", len(tasks))
	}

	// Verify we got both kinds
	foundImplement := false
	foundReview := false
	for _, task := range tasks {
		if task.Kind == "implement" {
			foundImplement = true
		}
		if task.Kind == "review" {
			foundReview = true
		}
	}
	if !foundImplement {
		t.Error("expected to find an 'implement' task in results")
	}
	if !foundReview {
		t.Error("expected to find a 'review' task in results")
	}
}

// TestListTasksWithInvalidKind verifies that an invalid kind returns 400.
func TestListTasksWithInvalidKind(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, _ := setupProjectAndDocument(t, server, authHeader)

	// List with invalid kind filter
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?kind=invalid_kind", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", listW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(listW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "INVALID_KIND" {
		t.Errorf("expected error code 'INVALID_KIND', got %q", code)
	}
}

// TestCreateTasksUnknownDependency returns 400.
func TestCreateTasksUnknownDependency(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Try to create task with unknown dependency
	taskPayload := []store.TaskInput{
		{
			Title:      "Task A",
			Spec:       "Spec A",
			DocumentID: docID,
			DependsOn:  []string{"nonexistent-id"},
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	code, ok := errObj["code"].(string)
	if !ok || code != "UNKNOWN_DEPENDENCY" {
		t.Errorf("expected error code 'UNKNOWN_DEPENDENCY', got %q", code)
	}
}

// TestCreateTasksMissingDocumentID returns 400.
func TestCreateTasksMissingDocumentID(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, _ := setupProjectAndDocument(t, server, authHeader)

	// Try to create task without document_id
	taskPayload := []store.TaskInput{
		{
			Title: "Task A",
			Spec:  "Spec A",
			// Missing DocumentID
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

// TestCreateTasksAcceptsBuildTrack verifies CreateTasks accepts build track.
func TestCreateTasksAcceptsBuildTrack(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Build Track Task",
			Spec:       "Spec",
			DocumentID: docID,
			Track:      "build",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", w.Code)
	}

	var createdTasks []store.Task
	if err := json.NewDecoder(w.Body).Decode(&createdTasks); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(createdTasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(createdTasks))
	}

	if createdTasks[0].Track != "build" {
		t.Errorf("expected track 'build', got %q", createdTasks[0].Track)
	}
}

// TestCreateTasksAcceptsDesignTrack verifies CreateTasks accepts design track.
func TestCreateTasksAcceptsDesignTrack(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Design Track Task",
			Spec:       "Spec",
			DocumentID: docID,
			Track:      "design",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", w.Code)
	}

	var createdTasks []store.Task
	if err := json.NewDecoder(w.Body).Decode(&createdTasks); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(createdTasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(createdTasks))
	}

	if createdTasks[0].Track != "design" {
		t.Errorf("expected track 'design', got %q", createdTasks[0].Track)
	}
}

// TestCreateTasksDefaultsTrackToBuild verifies CreateTasks defaults track to build when empty.
func TestCreateTasksDefaultsTrackToBuild(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Default Track Task",
			Spec:       "Spec",
			DocumentID: docID,
			// Track not specified - should default to build
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", w.Code)
	}

	var createdTasks []store.Task
	if err := json.NewDecoder(w.Body).Decode(&createdTasks); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(createdTasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(createdTasks))
	}

	if createdTasks[0].Track != "build" {
		t.Errorf("expected track to default to 'build', got %q", createdTasks[0].Track)
	}
}

// TestCreateTasksRejectsUnknownTrack verifies CreateTasks rejects unknown track with UNKNOWN_TRACK error.
func TestCreateTasksRejectsUnknownTrack(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Unknown Track Task",
			Spec:       "Spec",
			DocumentID: docID,
			Track:      "testing",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	code, ok := errObj["code"].(string)
	if !ok || code != "UNKNOWN_TRACK" {
		t.Errorf("expected error code 'UNKNOWN_TRACK', got %q", code)
	}
}

// TestPromoteTaskBacklogToReady verifies promoting a backlog task to ready succeeds.
func TestPromoteTaskBacklogToReady(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a backlog task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task to Promote",
			Spec:       "Promote this task",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Verify task is in backlog state
	if createdTasks[0].State != "backlog" {
		t.Errorf("expected initial state 'backlog', got %q", createdTasks[0].State)
	}

	// Promote the task
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	if promoteW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", promoteW.Code)
	}

	var promotedTask store.Task
	if err := json.NewDecoder(promoteW.Body).Decode(&promotedTask); err != nil {
		t.Fatalf("failed to decode promoted task: %v", err)
	}

	// Verify state changed to ready
	if promotedTask.State != "ready" {
		t.Errorf("expected state 'ready', got %q", promotedTask.State)
	}
	if promotedTask.ID != taskID {
		t.Errorf("expected task id %q, got %q", taskID, promotedTask.ID)
	}
}

// TestPromoteTaskNotInBacklog verifies promoting a non-backlog task returns 409.
func TestPromoteTaskNotInBacklog(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a backlog task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task",
			Spec:       "Spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// First promotion: backlog -> ready
	promoteReq1 := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq1.Header.Set("Authorization", authHeader)
	promoteW1 := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW1, promoteReq1)

	if promoteW1.Code != http.StatusOK {
		t.Errorf("expected first promotion to succeed with 200, got %d", promoteW1.Code)
	}

	// Second promotion: already in ready, should fail
	promoteReq2 := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq2.Header.Set("Authorization", authHeader)
	promoteW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW2, promoteReq2)

	if promoteW2.Code != http.StatusConflict {
		t.Errorf("expected second promotion to return 409, got %d", promoteW2.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(promoteW2.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "CONFLICT" {
		t.Errorf("expected error code 'CONFLICT', got %q", code)
	}
}

// TestPromoteTaskUnknownID verifies promoting an unknown task returns 404.
func TestPromoteTaskUnknownID(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	promoteReq := httptest.NewRequest("POST", "/tasks/nonexistent-id/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	if promoteW.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", promoteW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(promoteW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "NOT_FOUND" {
		t.Errorf("expected error code 'NOT_FOUND', got %q", code)
	}
}

// TestPromoteTaskAppendsTransitionEvent verifies that promoting a task creates a transition event.
func TestPromoteTaskAppendsTransitionEvent(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a backlog task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task for Event",
			Spec:       "Check events",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Promote the task
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	if promoteW.Code != http.StatusOK {
		t.Errorf("expected promotion to succeed with 200, got %d", promoteW.Code)
	}

	// Get the task with events
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var taskWithDeps store.TaskWithDepsAndLinks
	if err := json.NewDecoder(getW.Body).Decode(&taskWithDeps); err != nil {
		t.Fatalf("failed to decode task: %v", err)
	}

	// Fetch events directly from the store
	events, err := server.store.ListEvents(context.Background(), taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}

	if len(events) == 0 {
		t.Errorf("expected at least 1 event (transition), got %d", len(events))
	}

	// Verify the transition event
	transitionFound := false
	for _, event := range events {
		if event.Kind == "transition" && event.Actor == "system" {
			transitionFound = true
			if event.Note == nil || *event.Note != "backlog->ready" {
				t.Errorf("expected transition note 'backlog->ready', got %v", event.Note)
			}
		}
	}

	if !transitionFound {
		t.Error("transition event not found")
	}
}

// TestPromoteTaskRequiresAuth verifies promote endpoint requires auth.
func TestPromoteTaskRequiresAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	promoteReq := httptest.NewRequest("POST", "/tasks/some-id/promote", nil)
	// No Authorization header
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	if promoteW.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", promoteW.Code)
	}
}

// Helper to set up a project, document, and an in_progress task with a lease
func setupClaimedTask(t *testing.T, server *Server, authHeader string) (string, *string) {
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task to Claim",
			Spec:       "Test task for claiming",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Promote the task to ready
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	// Claim the task
	claimPayload := map[string]string{"agent_id": "test-agent", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	if claimW.Code != http.StatusOK {
		t.Fatalf("failed to claim task: got status %d", claimW.Code)
	}

	var claimedTask store.Task
	json.NewDecoder(claimW.Body).Decode(&claimedTask)

	return taskID, claimedTask.LeaseExpiresAt
}

// TestHeartbeatExtendsLease verifies that heartbeat by the assignee extends the lease.
func TestHeartbeatExtendsLease(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID, oldLeaseExpiry := setupClaimedTask(t, server, authHeader)

	// Small sleep to ensure time passes and new timestamp will be > old
	time.Sleep(10 * time.Millisecond)

	// Heartbeat the task with the same agent
	heartbeatPayload := map[string]string{"agent_id": "test-agent"}
	heartbeatBody, _ := json.Marshal(heartbeatPayload)
	heartbeatReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Authorization", authHeader)
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatW := httptest.NewRecorder()
	server.mux.ServeHTTP(heartbeatW, heartbeatReq)

	if heartbeatW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", heartbeatW.Code)
	}

	var heartbeatTask store.Task
	if err := json.NewDecoder(heartbeatW.Body).Decode(&heartbeatTask); err != nil {
		t.Fatalf("failed to decode heartbeat response: %v", err)
	}

	// Verify lease was extended
	if heartbeatTask.LeaseExpiresAt == nil {
		t.Fatal("lease_expires_at is nil after heartbeat")
	}
	if oldLeaseExpiry == nil {
		t.Fatal("old lease_expires_at is nil")
	}
	if *heartbeatTask.LeaseExpiresAt <= *oldLeaseExpiry {
		t.Errorf("new lease expiry (%q) not greater than old (%q)", *heartbeatTask.LeaseExpiresAt, *oldLeaseExpiry)
	}
}

// TestHeartbeatByDifferentAgentReturns409 verifies that heartbeat by a non-assignee returns 409.
func TestHeartbeatByDifferentAgentReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID, _ := setupClaimedTask(t, server, authHeader)

	// Heartbeat with a different agent ID
	heartbeatPayload := map[string]string{"agent_id": "different-agent"}
	heartbeatBody, _ := json.Marshal(heartbeatPayload)
	heartbeatReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Authorization", authHeader)
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatW := httptest.NewRecorder()
	server.mux.ServeHTTP(heartbeatW, heartbeatReq)

	if heartbeatW.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", heartbeatW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(heartbeatW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "CONFLICT" {
		t.Errorf("expected error code 'CONFLICT', got %q", code)
	}
}

// TestHeartbeatOnNotInProgressReturns409 verifies that heartbeat on non-in_progress task returns 409.
func TestHeartbeatOnNotInProgressReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task but keep it in backlog (don't promote/claim)
	taskPayload := []store.TaskInput{
		{
			Title:      "Backlog Task",
			Spec:       "This stays in backlog",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Try to heartbeat a backlog task
	heartbeatPayload := map[string]string{"agent_id": "test-agent"}
	heartbeatBody, _ := json.Marshal(heartbeatPayload)
	heartbeatReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Authorization", authHeader)
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatW := httptest.NewRecorder()
	server.mux.ServeHTTP(heartbeatW, heartbeatReq)

	if heartbeatW.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", heartbeatW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(heartbeatW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "CONFLICT" {
		t.Errorf("expected error code 'CONFLICT', got %q", code)
	}
}

// TestHeartbeatUnknownTaskReturns404 verifies that heartbeat on unknown task returns 404.
func TestHeartbeatUnknownTaskReturns404(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	heartbeatPayload := map[string]string{"agent_id": "test-agent"}
	heartbeatBody, _ := json.Marshal(heartbeatPayload)
	heartbeatReq := httptest.NewRequest("POST", "/tasks/nonexistent-id/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Authorization", authHeader)
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatW := httptest.NewRecorder()
	server.mux.ServeHTTP(heartbeatW, heartbeatReq)

	if heartbeatW.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", heartbeatW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(heartbeatW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "NOT_FOUND" {
		t.Errorf("expected error code 'NOT_FOUND', got %q", code)
	}
}

// TestHeartbeatEmptyAgentIDReturns400 verifies that empty agent_id returns 400.
func TestHeartbeatEmptyAgentIDReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID, _ := setupClaimedTask(t, server, authHeader)

	// Heartbeat with empty agent_id
	heartbeatPayload := map[string]string{"agent_id": ""}
	heartbeatBody, _ := json.Marshal(heartbeatPayload)
	heartbeatReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Authorization", authHeader)
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatW := httptest.NewRecorder()
	server.mux.ServeHTTP(heartbeatW, heartbeatReq)

	if heartbeatW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", heartbeatW.Code)
	}

	var errResp map[string]interface{}
	if err := json.NewDecoder(heartbeatW.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error response missing 'error' field")
	}

	if code, ok := errObj["code"].(string); !ok || code != "EMPTY_AGENT_ID" {
		t.Errorf("expected error code 'EMPTY_AGENT_ID', got %q", code)
	}
}

// TestHeartbeatDoesNotAppendEvent verifies that a heartbeat does NOT append an event.
func TestHeartbeatDoesNotAppendEvent(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID, _ := setupClaimedTask(t, server, authHeader)

	// Get events before heartbeat
	eventsBefore, err := server.store.ListEvents(context.Background(), taskID)
	if err != nil {
		t.Fatalf("failed to list events before heartbeat: %v", err)
	}

	// Heartbeat the task
	heartbeatPayload := map[string]string{"agent_id": "test-agent"}
	heartbeatBody, _ := json.Marshal(heartbeatPayload)
	heartbeatReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/heartbeat", bytes.NewReader(heartbeatBody))
	heartbeatReq.Header.Set("Authorization", authHeader)
	heartbeatReq.Header.Set("Content-Type", "application/json")
	heartbeatW := httptest.NewRecorder()
	server.mux.ServeHTTP(heartbeatW, heartbeatReq)

	if heartbeatW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", heartbeatW.Code)
	}

	// Get events after heartbeat
	eventsAfter, err := server.store.ListEvents(context.Background(), taskID)
	if err != nil {
		t.Fatalf("failed to list events after heartbeat: %v", err)
	}

	// Verify no new event was appended for heartbeat
	if len(eventsAfter) != len(eventsBefore) {
		t.Errorf("expected same number of events after heartbeat, got %d before and %d after", len(eventsBefore), len(eventsAfter))
	}

	// Verify no heartbeat event exists
	for _, event := range eventsAfter {
		if event.Kind == "heartbeat" {
			t.Error("unexpected heartbeat event found in events")
		}
	}
}

// TestHeartbeatRequiresAuth verifies that heartbeat endpoint requires auth.
func TestHeartbeatRequiresAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	heartbeatPayload := map[string]string{"agent_id": "test-agent"}
	heartbeatBody, _ := json.Marshal(heartbeatPayload)
	heartbeatReq := httptest.NewRequest("POST", "/tasks/some-id/heartbeat", bytes.NewReader(heartbeatBody))
	// No Authorization header
	heartbeatW := httptest.NewRecorder()
	server.mux.ServeHTTP(heartbeatW, heartbeatReq)

	if heartbeatW.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", heartbeatW.Code)
	}
}

// TestSubmitTaskMovesToReviewAndPersistsLinks verifies that submit moves a task to review,
// stores the result, persists links, and clears the lease.
func TestSubmitTaskMovesToReviewAndPersistsLinks(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Setup: project, document, and claimed task
	taskID, _ := setupClaimedTask(t, server, authHeader)

	// Submit with a result and two links (pr and commit)
	submitPayload := map[string]interface{}{
		"agent_id": "test-agent",
		"result":   "Work completed successfully",
		"links": []map[string]string{
			{"kind": "pr", "value": "https://github.com/repo/pull/123"},
			{"kind": "commit", "value": "abc123def456"},
		},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", submitW.Code, submitW.Body.String())
	}

	var submittedTask store.TaskWithDepsAndLinks
	if err := json.NewDecoder(submitW.Body).Decode(&submittedTask); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify task state is review
	if submittedTask.State != "review" {
		t.Errorf("expected state 'review', got %q", submittedTask.State)
	}

	// Verify result is stored
	if submittedTask.Result == nil || *submittedTask.Result != "Work completed successfully" {
		t.Errorf("expected result 'Work completed successfully', got %v", submittedTask.Result)
	}

	// Verify lease is cleared (NULL)
	if submittedTask.LeaseExpiresAt != nil {
		t.Errorf("expected lease_expires_at to be NULL, got %v", submittedTask.LeaseExpiresAt)
	}

	// Verify links are persisted and returned
	if len(submittedTask.Links) != 2 {
		t.Fatalf("expected 2 links, got %d", len(submittedTask.Links))
	}

	// Check the first link (pr)
	found := false
	for _, link := range submittedTask.Links {
		if link.Kind == "pr" && link.Value == "https://github.com/repo/pull/123" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected link (pr, https://github.com/repo/pull/123) not found in response")
	}

	// Check the second link (commit)
	found = false
	for _, link := range submittedTask.Links {
		if link.Kind == "commit" && link.Value == "abc123def456" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected link (commit, abc123def456) not found in response")
	}

	// Verify links are retrievable via GET /tasks/{id}
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	if getW.Code != http.StatusOK {
		t.Fatalf("GET /tasks/{id} failed with status %d", getW.Code)
	}

	var retrievedTask store.TaskWithDepsAndLinks
	if err := json.NewDecoder(getW.Body).Decode(&retrievedTask); err != nil {
		t.Fatalf("failed to decode GET response: %v", err)
	}

	if len(retrievedTask.Links) != 2 {
		t.Errorf("expected 2 links via GET, got %d", len(retrievedTask.Links))
	}

	// Verify links are queryable by (kind, value) via the store's Conn
	// For this test, we just verify they exist in the retrieved task
	linkFound := false
	for _, link := range retrievedTask.Links {
		if link.Kind == "pr" && link.Value == "https://github.com/repo/pull/123" {
			linkFound = true
			break
		}
	}
	if !linkFound {
		t.Errorf("link (pr, ...) not retrievable via GET")
	}
}

// TestSubmitByDifferentAgentReturns409 verifies that submit by a different agent returns 409.
func TestSubmitByDifferentAgentReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Setup: project, document, and claimed task
	taskID, _ := setupClaimedTask(t, server, authHeader)

	// Try to submit with a different agent_id
	submitPayload := map[string]interface{}{
		"agent_id": "different-agent",
		"result":   "Work completed",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", submitW.Code)
	}
}

// TestSubmitFromNonInProgressReturns409 verifies that submit from a non-in_progress task returns 409.
func TestSubmitFromNonInProgressReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task (it starts in backlog)
	taskPayload := []store.TaskInput{
		{
			Title:      "Task in backlog",
			Spec:       "Test task",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Try to submit without claiming (task is still in backlog, not in_progress)
	submitPayload := map[string]interface{}{
		"agent_id": "test-agent",
		"result":   "Work completed",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", submitW.Code)
	}
}

// TestSubmitUnknownTaskReturns404 verifies that submit on an unknown task returns 404.
func TestSubmitUnknownTaskReturns404(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	submitPayload := map[string]interface{}{
		"agent_id": "test-agent",
		"result":   "Work completed",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/unknown-task-id/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", submitW.Code)
	}
}

// TestSubmitWithInvalidLinkKindReturns400 verifies that submit with an invalid link kind returns 400
// and nothing changes (task still in_progress, no links).
func TestSubmitWithInvalidLinkKindReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Setup: project, document, and claimed task
	taskID, _ := setupClaimedTask(t, server, authHeader)

	// Try to submit with an invalid link kind
	submitPayload := map[string]interface{}{
		"agent_id": "test-agent",
		"result":   "Work completed",
		"links": []map[string]string{
			{"kind": "invalid_kind", "value": "some-value"},
		},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d; body: %s", submitW.Code, submitW.Body.String())
	}

	// Verify the error code is INVALID_LINK_KIND
	var errResp map[string]interface{}
	json.NewDecoder(submitW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "INVALID_LINK_KIND" {
		t.Errorf("expected error code 'INVALID_LINK_KIND', got %v", errObj["code"])
	}

	// Verify task is still in_progress and unchanged
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var task store.TaskWithDepsAndLinks
	json.NewDecoder(getW.Body).Decode(&task)

	if task.State != "in_progress" {
		t.Errorf("expected task to still be in_progress, got %q", task.State)
	}

	if len(task.Links) != 0 {
		t.Errorf("expected no links, but task has %d links", len(task.Links))
	}
}

// TestSubmitEmptyAgentIDReturns400 verifies that submit with empty agent_id returns 400.
func TestSubmitEmptyAgentIDReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task",
			Spec:       "Test task",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Promote to ready
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	// Claim the task
	claimPayload := map[string]string{"agent_id": "test-agent", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Try to submit with empty agent_id
	submitPayload := map[string]interface{}{
		"agent_id": "",
		"result":   "Work completed",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", submitW.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(submitW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "EMPTY_AGENT_ID" {
		t.Errorf("expected error code 'EMPTY_AGENT_ID', got %v", errObj["code"])
	}
}

// TestSubmitRequiresAuth verifies that submit endpoint requires auth.
func TestSubmitRequiresAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	submitPayload := map[string]interface{}{
		"agent_id": "test-agent",
		"result":   "Work completed",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/some-id/submit", bytes.NewReader(submitBody))
	// No Authorization header
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", submitW.Code)
	}
}

// Helper to set up a task in review state (for review and transition tests).
func setupTaskInReview(t *testing.T, server *Server, authHeader string) string {
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task for Review",
			Spec:       "Test task for review",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Promote the task to ready
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	// Claim the task
	claimPayload := map[string]string{"agent_id": "test-agent", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Submit the task to move it to review state
	submitPayload := map[string]interface{}{
		"agent_id": "test-agent",
		"result":   "Work completed",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	if submitW.Code != http.StatusOK {
		t.Fatalf("failed to submit task to review state: got status %d", submitW.Code)
	}

	return taskID
}

// TestReviewWithApproveVerdictSucceeds verifies that posting an approve review succeeds.
func TestReviewWithApproveVerdictSucceeds(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID := setupTaskInReview(t, server, authHeader)

	// Post an approve review
	reviewPayload := map[string]interface{}{
		"actor":   "reviewer-1",
		"verdict": "approve",
		"note":    "Looks good!",
	}
	reviewBody, _ := json.Marshal(reviewPayload)
	reviewReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/review", bytes.NewReader(reviewBody))
	reviewReq.Header.Set("Authorization", authHeader)
	reviewReq.Header.Set("Content-Type", "application/json")
	reviewW := httptest.NewRecorder()
	server.mux.ServeHTTP(reviewW, reviewReq)

	if reviewW.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d; body: %s", reviewW.Code, reviewW.Body.String())
	}

	var event store.Event
	if err := json.NewDecoder(reviewW.Body).Decode(&event); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify event details
	if event.TaskID != taskID {
		t.Errorf("expected task_id %q, got %q", taskID, event.TaskID)
	}
	if event.Actor != "reviewer-1" {
		t.Errorf("expected actor 'reviewer-1', got %q", event.Actor)
	}
	if event.Kind != "review" {
		t.Errorf("expected kind 'review', got %q", event.Kind)
	}
	if event.Verdict == nil || *event.Verdict != "approve" {
		t.Errorf("expected verdict 'approve', got %v", event.Verdict)
	}
	if event.Note == nil || *event.Note != "Looks good!" {
		t.Errorf("expected note 'Looks good!', got %v", event.Note)
	}
}

// TestTransitionToDoneAfterApproveSucceeds verifies that transition to done succeeds from approved state.
func TestTransitionToDoneAfterApproveSucceeds(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID := setupTaskInReview(t, server, authHeader)

	// Manually set task state to approved (simulating MR-9 verdict aggregation)
	_, err := server.store.Conn().ExecContext(context.Background(),
		"UPDATE task SET state = ? WHERE id = ?", "approved", taskID)
	if err != nil {
		t.Fatalf("failed to set task to approved: %v", err)
	}

	// Now transition from approved to done
	transitionPayload := map[string]interface{}{
		"to":   "done",
		"note": "Approved and merged to main",
	}
	transitionBody, _ := json.Marshal(transitionPayload)
	transitionReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/transition", bytes.NewReader(transitionBody))
	transitionReq.Header.Set("Authorization", authHeader)
	transitionReq.Header.Set("Content-Type", "application/json")
	transitionW := httptest.NewRecorder()
	server.mux.ServeHTTP(transitionW, transitionReq)

	if transitionW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", transitionW.Code, transitionW.Body.String())
	}

	var task store.Task
	if err := json.NewDecoder(transitionW.Body).Decode(&task); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if task.State != "done" {
		t.Errorf("expected state 'done', got %q", task.State)
	}
}

// TestTransitionFromReviewToDoneReturns409 verifies that transition to done from review state is no longer allowed.
func TestTransitionFromReviewToDoneReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID := setupTaskInReview(t, server, authHeader)

	// Try to transition directly from review to done (should fail)
	transitionPayload := map[string]interface{}{
		"to": "done",
	}
	transitionBody, _ := json.Marshal(transitionPayload)
	transitionReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/transition", bytes.NewReader(transitionBody))
	transitionReq.Header.Set("Authorization", authHeader)
	transitionReq.Header.Set("Content-Type", "application/json")
	transitionW := httptest.NewRecorder()
	server.mux.ServeHTTP(transitionW, transitionReq)

	if transitionW.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d", transitionW.Code)
	}

	// Verify task state is still review
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var task store.TaskWithDepsAndLinks
	json.NewDecoder(getW.Body).Decode(&task)

	if task.State != "review" {
		t.Errorf("expected task to still be in 'review', got %q", task.State)
	}
}

func TestTransitionFromReviewToReadyReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID := setupTaskInReview(t, server, authHeader)

	// Try to transition from review to ready (should fail, only allowed from approved)
	transitionPayload := map[string]interface{}{
		"to": "ready",
	}
	transitionBody, _ := json.Marshal(transitionPayload)
	transitionReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/transition", bytes.NewReader(transitionBody))
	transitionReq.Header.Set("Authorization", authHeader)
	transitionReq.Header.Set("Content-Type", "application/json")
	transitionW := httptest.NewRecorder()
	server.mux.ServeHTTP(transitionW, transitionReq)

	if transitionW.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d", transitionW.Code)
	}
}

// TestTransitionToReadyFromApprovedReturnsToClaimablePool verifies that transition to ready from approved allows reclaiming.
func TestTransitionToReadyFromApprovedReturnsToClaimablePool(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID := setupTaskInReview(t, server, authHeader)

	// Manually set task state to approved (simulating MR-9 verdict aggregation)
	_, err := server.store.Conn().ExecContext(context.Background(),
		"UPDATE task SET state = ? WHERE id = ?", "approved", taskID)
	if err != nil {
		t.Fatalf("failed to set task to approved: %v", err)
	}

	// Transition back to ready from approved (human overrides approval for rework)
	transitionPayload := map[string]interface{}{
		"to":   "ready",
		"note": "Needs rework",
	}
	transitionBody, _ := json.Marshal(transitionPayload)
	transitionReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/transition", bytes.NewReader(transitionBody))
	transitionReq.Header.Set("Authorization", authHeader)
	transitionReq.Header.Set("Content-Type", "application/json")
	transitionW := httptest.NewRecorder()
	server.mux.ServeHTTP(transitionW, transitionReq)

	if transitionW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", transitionW.Code)
	}

	var task store.Task
	if err := json.NewDecoder(transitionW.Body).Decode(&task); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if task.State != "ready" {
		t.Errorf("expected state 'ready', got %q", task.State)
	}

	// Verify it can be claimed again
	claimPayload := map[string]string{"agent_id": "test-agent-2", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	if claimW.Code != http.StatusOK {
		t.Errorf("expected claim to succeed (status 200), got %d", claimW.Code)
	}
}

// TestReviewOnNonReviewTaskReturns409 verifies that posting a review on a non-review task fails.
func TestReviewOnNonReviewTaskReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task but leave it in backlog (no promote, no claim)
	taskPayload := []store.TaskInput{
		{
			Title:      "Backlog Task",
			Spec:       "This stays in backlog",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Try to post a review on a backlog task
	reviewPayload := map[string]interface{}{
		"actor":   "reviewer",
		"verdict": "approve",
	}
	reviewBody, _ := json.Marshal(reviewPayload)
	reviewReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/review", bytes.NewReader(reviewBody))
	reviewReq.Header.Set("Authorization", authHeader)
	reviewReq.Header.Set("Content-Type", "application/json")
	reviewW := httptest.NewRecorder()
	server.mux.ServeHTTP(reviewW, reviewReq)

	if reviewW.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", reviewW.Code)
	}
}

// TestReviewWithInvalidVerdictReturns400 verifies that posting a review with invalid verdict returns 400.
func TestReviewWithInvalidVerdictReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID := setupTaskInReview(t, server, authHeader)

	// Post a review with invalid verdict
	reviewPayload := map[string]interface{}{
		"actor":   "reviewer",
		"verdict": "invalid-verdict",
	}
	reviewBody, _ := json.Marshal(reviewPayload)
	reviewReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/review", bytes.NewReader(reviewBody))
	reviewReq.Header.Set("Authorization", authHeader)
	reviewReq.Header.Set("Content-Type", "application/json")
	reviewW := httptest.NewRecorder()
	server.mux.ServeHTTP(reviewW, reviewReq)

	if reviewW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", reviewW.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(reviewW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "INVALID_VERDICT" {
		t.Errorf("expected error code 'INVALID_VERDICT', got %v", errObj["code"])
	}
}

// TestTransitionWithInvalidTargetStateReturns400 verifies that transition with invalid target state returns 400.
func TestTransitionWithInvalidTargetStateReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	taskID := setupTaskInReview(t, server, authHeader)

	// Try to transition to an invalid state
	transitionPayload := map[string]interface{}{
		"to": "invalid-state",
	}
	transitionBody, _ := json.Marshal(transitionPayload)
	transitionReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/transition", bytes.NewReader(transitionBody))
	transitionReq.Header.Set("Authorization", authHeader)
	transitionReq.Header.Set("Content-Type", "application/json")
	transitionW := httptest.NewRecorder()
	server.mux.ServeHTTP(transitionW, transitionReq)

	if transitionW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", transitionW.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(transitionW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "INVALID_TARGET_STATE" {
		t.Errorf("expected error code 'INVALID_TARGET_STATE', got %v", errObj["code"])
	}
}

// TestTransitionToBlockedFromActiveStateSucceeds verifies that transition to blocked from an active state succeeds.
func TestTransitionToBlockedFromActiveStateSucceeds(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Set up a task in in_progress state
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Task to Block",
			Spec:       "Test task",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Promote and claim
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	claimPayload := map[string]string{"agent_id": "test-agent", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Transition to blocked
	transitionPayload := map[string]interface{}{
		"to":   "blocked",
		"note": "Blocked on external dependency",
	}
	transitionBody, _ := json.Marshal(transitionPayload)
	transitionReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/transition", bytes.NewReader(transitionBody))
	transitionReq.Header.Set("Authorization", authHeader)
	transitionReq.Header.Set("Content-Type", "application/json")
	transitionW := httptest.NewRecorder()
	server.mux.ServeHTTP(transitionW, transitionReq)

	if transitionW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", transitionW.Code)
	}

	var task store.Task
	if err := json.NewDecoder(transitionW.Body).Decode(&task); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if task.State != "blocked" {
		t.Errorf("expected state 'blocked', got %q", task.State)
	}
}

// TestListProjectsReturnsAllProjects verifies that GET /projects returns all created projects ordered by created_at.
func TestListProjectsReturnsAllProjects(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create multiple projects
	projects := []map[string]string{
		{"name": "list-test-project-a", "repo": "https://github.com/example/repo-a"},
		{"name": "list-test-project-b", "repo": "https://github.com/example/repo-b"},
		{"name": "list-test-project-c", "repo": "https://github.com/example/repo-c"},
	}

	createdProjects := make([]store.Project, len(projects))
	for i, p := range projects {
		payload := map[string]string{"name": p["name"], "repo": p["repo"]}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest("POST", "/projects", bytes.NewReader(body))
		req.Header.Set("Authorization", authHeader)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.mux.ServeHTTP(w, req)

		var resp store.Project
		json.NewDecoder(w.Body).Decode(&resp)
		createdProjects[i] = resp
	}

	// List all projects
	listReq := httptest.NewRequest("GET", "/projects", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var respProjects []store.Project
	if err := json.NewDecoder(listW.Body).Decode(&respProjects); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify all created projects are in the response
	for _, created := range createdProjects {
		found := false
		for _, resp := range respProjects {
			if resp.ID == created.ID {
				// Verify the fields match
				if resp.Name != created.Name {
					t.Errorf("expected name %q, got %q", created.Name, resp.Name)
				}
				if resp.Repo != created.Repo {
					t.Errorf("expected repo %q, got %q", created.Repo, resp.Repo)
				}
				if resp.CreatedAt != created.CreatedAt {
					t.Errorf("expected created_at %q, got %q", created.CreatedAt, resp.CreatedAt)
				}
				found = true
				break
			}
		}
		if !found {
			t.Errorf("created project %q not found in list response", created.ID)
		}
	}

	// Verify projects are ordered by created_at
	for i := 1; i < len(respProjects); i++ {
		if respProjects[i].CreatedAt < respProjects[i-1].CreatedAt {
			t.Errorf("projects not ordered by created_at: %s >= %s", respProjects[i-1].CreatedAt, respProjects[i].CreatedAt)
		}
	}
}

// TestListProjectsWithoutAuth verifies that GET /projects returns 401 without a token.
func TestListProjectsWithoutAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	listReq := httptest.NewRequest("GET", "/projects", nil)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", listW.Code)
	}
}

// TestListProjectsReturnsEmptyArray verifies that GET /projects returns [] (not null) when no projects exist.
// This test uses a fresh database to ensure no prior projects exist.
func TestListProjectsReturnsEmptyArray(t *testing.T) {
	tmpdb := t.TempDir() + "/test.db"
	s, err := store.Open(tmpdb, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	server := New(s, "test-token", 5*time.Minute, 5, nil, false)
	authHeader := "Bearer test-token"

	// List projects without creating any
	listReq := httptest.NewRequest("GET", "/projects", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var respProjects []store.Project
	if err := json.NewDecoder(listW.Body).Decode(&respProjects); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if respProjects == nil {
		t.Error("expected empty array, got nil")
	}
	if len(respProjects) != 0 {
		t.Errorf("expected 0 projects, got %d", len(respProjects))
	}
}

// TestListProjectsWithClaimableFilter verifies that GET /projects?claimable=true&model=haiku&kind=implement
// returns only projects with at least one claimable haiku implement task.
func TestListProjectsWithClaimableFilter(t *testing.T) {
	tmpdb := t.TempDir() + "/test.db"
	s, err := store.Open(tmpdb, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	server := New(s, "test-token", 5*time.Minute, 5, nil, false)
	authHeader := "Bearer test-token"

	// Create project 1 with a claimable haiku implement task
	proj1Payload := map[string]string{"name": "project-with-claimable", "repo": "https://github.com/example/repo1"}
	proj1Body, _ := json.Marshal(proj1Payload)
	proj1Req := httptest.NewRequest("POST", "/projects", bytes.NewReader(proj1Body))
	proj1Req.Header.Set("Authorization", authHeader)
	proj1Req.Header.Set("Content-Type", "application/json")
	proj1W := httptest.NewRecorder()
	server.mux.ServeHTTP(proj1W, proj1Req)
	var proj1 store.Project
	json.NewDecoder(proj1W.Body).Decode(&proj1)

	// Create project 2 with only blocked tasks
	proj2Payload := map[string]string{"name": "project-blocked-only", "repo": "https://github.com/example/repo2"}
	proj2Body, _ := json.Marshal(proj2Payload)
	proj2Req := httptest.NewRequest("POST", "/projects", bytes.NewReader(proj2Body))
	proj2Req.Header.Set("Authorization", authHeader)
	proj2Req.Header.Set("Content-Type", "application/json")
	proj2W := httptest.NewRecorder()
	server.mux.ServeHTTP(proj2W, proj2Req)
	var proj2 store.Project
	json.NewDecoder(proj2W.Body).Decode(&proj2)

	// Create documents for both projects
	for _, proj := range []store.Project{proj1, proj2} {
		docPayload := map[string]string{
			"kind":  "design",
			"title": "DESIGN.md",
			"ref":   "DESIGN.md",
		}
		docBody, _ := json.Marshal(docPayload)
		docReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/documents", bytes.NewReader(docBody))
		docReq.Header.Set("Authorization", authHeader)
		docReq.Header.Set("Content-Type", "application/json")
		docW := httptest.NewRecorder()
		server.mux.ServeHTTP(docW, docReq)
		var doc store.Document
		json.NewDecoder(docW.Body).Decode(&doc)

		// Create a task in each project
		taskPayload := []store.TaskInput{
			{
				Title:      "test-task",
				Spec:       "test spec",
				DocumentID: doc.ID,
				Model:      "haiku",
			},
		}
		taskBody, _ := json.Marshal(taskPayload)
		taskReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/tasks", bytes.NewReader(taskBody))
		taskReq.Header.Set("Authorization", authHeader)
		taskReq.Header.Set("Content-Type", "application/json")
		taskW := httptest.NewRecorder()
		server.mux.ServeHTTP(taskW, taskReq)
		var tasks []store.Task
		json.NewDecoder(taskW.Body).Decode(&tasks)

		// Promote the task to ready (needed for claimable)
		promoteReq := httptest.NewRequest("POST", "/tasks/"+tasks[0].ID+"/promote", nil)
		promoteReq.Header.Set("Authorization", authHeader)
		promoteW := httptest.NewRecorder()
		server.mux.ServeHTTP(promoteW, promoteReq)

		// For proj2, transition the task to blocked to exclude it from claimable filter
		if proj.ID == proj2.ID {
			blockReq := httptest.NewRequest("POST", "/tasks/"+tasks[0].ID+"/transition", bytes.NewReader([]byte(`{"to":"blocked"}`)))
			blockReq.Header.Set("Authorization", authHeader)
			blockReq.Header.Set("Content-Type", "application/json")
			blockW := httptest.NewRecorder()
			server.mux.ServeHTTP(blockW, blockReq)
		}
	}

	// List projects with claimable filter
	listReq := httptest.NewRequest("GET", "/projects?claimable=true&model=haiku&kind=implement", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var respProjects []store.Project
	if err := json.NewDecoder(listW.Body).Decode(&respProjects); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify only proj1 is returned
	if len(respProjects) != 1 {
		t.Errorf("expected 1 project, got %d", len(respProjects))
	}
	if len(respProjects) > 0 && respProjects[0].ID != proj1.ID {
		t.Errorf("expected project %q, got %q", proj1.ID, respProjects[0].ID)
	}
}

// TestListProjectsClaimableWithMultipleFilters verifies that model and kind filters AND-compose.
func TestListProjectsClaimableWithMultipleFilters(t *testing.T) {
	tmpdb := t.TempDir() + "/test.db"
	s, err := store.Open(tmpdb, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	server := New(s, "test-token", 5*time.Minute, 5, nil, false)
	authHeader := "Bearer test-token"

	// Create a project with two tasks: one haiku, one sonnet
	projPayload := map[string]string{"name": "multi-model-project", "repo": "https://github.com/example/repo"}
	projBody, _ := json.Marshal(projPayload)
	projReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projBody))
	projReq.Header.Set("Authorization", authHeader)
	projReq.Header.Set("Content-Type", "application/json")
	projW := httptest.NewRecorder()
	server.mux.ServeHTTP(projW, projReq)
	var proj store.Project
	json.NewDecoder(projW.Body).Decode(&proj)

	// Create document
	docPayload := map[string]string{
		"kind":  "design",
		"title": "DESIGN.md",
		"ref":   "DESIGN.md",
	}
	docBody, _ := json.Marshal(docPayload)
	docReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/documents", bytes.NewReader(docBody))
	docReq.Header.Set("Authorization", authHeader)
	docReq.Header.Set("Content-Type", "application/json")
	docW := httptest.NewRecorder()
	server.mux.ServeHTTP(docW, docReq)
	var doc store.Document
	json.NewDecoder(docW.Body).Decode(&doc)

	// Create haiku and sonnet tasks
	taskPayload := []store.TaskInput{
		{
			Title:      "haiku-task",
			Spec:       "test spec",
			DocumentID: doc.ID,
			Model:      "haiku",
		},
		{
			Title:      "sonnet-task",
			Spec:       "test spec",
			DocumentID: doc.ID,
			Model:      "sonnet",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	taskReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/tasks", bytes.NewReader(taskBody))
	taskReq.Header.Set("Authorization", authHeader)
	taskReq.Header.Set("Content-Type", "application/json")
	taskW := httptest.NewRecorder()
	server.mux.ServeHTTP(taskW, taskReq)
	var tasks []store.Task
	json.NewDecoder(taskW.Body).Decode(&tasks)

	// Promote both to ready
	for _, task := range tasks {
		promoteReq := httptest.NewRequest("POST", "/tasks/"+task.ID+"/promote", nil)
		promoteReq.Header.Set("Authorization", authHeader)
		promoteW := httptest.NewRecorder()
		server.mux.ServeHTTP(promoteW, promoteReq)
	}

	// Filter for haiku model - should return the project
	haikuReq := httptest.NewRequest("GET", "/projects?claimable=true&model=haiku&kind=implement", nil)
	haikuReq.Header.Set("Authorization", authHeader)
	haikuW := httptest.NewRecorder()
	server.mux.ServeHTTP(haikuW, haikuReq)

	var haikuProjects []store.Project
	json.NewDecoder(haikuW.Body).Decode(&haikuProjects)
	if len(haikuProjects) != 1 {
		t.Errorf("expected 1 project for haiku filter, got %d", len(haikuProjects))
	}

	// Filter for sonnet model - should also return the project
	sonnetReq := httptest.NewRequest("GET", "/projects?claimable=true&model=sonnet&kind=implement", nil)
	sonnetReq.Header.Set("Authorization", authHeader)
	sonnetW := httptest.NewRecorder()
	server.mux.ServeHTTP(sonnetW, sonnetReq)

	var sonnetProjects []store.Project
	json.NewDecoder(sonnetW.Body).Decode(&sonnetProjects)
	if len(sonnetProjects) != 1 {
		t.Errorf("expected 1 project for sonnet filter, got %d", len(sonnetProjects))
	}

	// Filter for both haiku and implement - should return the project (AND logic)
	bothReq := httptest.NewRequest("GET", "/projects?claimable=true&model=haiku&kind=implement", nil)
	bothReq.Header.Set("Authorization", authHeader)
	bothW := httptest.NewRecorder()
	server.mux.ServeHTTP(bothW, bothReq)

	var bothProjects []store.Project
	json.NewDecoder(bothW.Body).Decode(&bothProjects)
	if len(bothProjects) != 1 {
		t.Errorf("expected 1 project for both filters, got %d", len(bothProjects))
	}
}

// TestListProjectsClaimableUnchangedWithoutFilters verifies backward compatibility:
// GET /projects without filters returns all projects.
func TestListProjectsClaimableUnchangedWithoutFilters(t *testing.T) {
	tmpdb := t.TempDir() + "/test.db"
	s, err := store.Open(tmpdb, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	server := New(s, "test-token", 5*time.Minute, 5, nil, false)
	authHeader := "Bearer test-token"

	// Create two projects
	for i := 0; i < 2; i++ {
		projPayload := map[string]string{
			"name": "test-project-" + string(rune('a'+i)),
			"repo": "https://github.com/example/repo-" + string(rune('a'+i)),
		}
		projBody, _ := json.Marshal(projPayload)
		projReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projBody))
		projReq.Header.Set("Authorization", authHeader)
		projReq.Header.Set("Content-Type", "application/json")
		projW := httptest.NewRecorder()
		server.mux.ServeHTTP(projW, projReq)
	}

	// List without filters
	listReq := httptest.NewRequest("GET", "/projects", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var respProjects []store.Project
	if err := json.NewDecoder(listW.Body).Decode(&respProjects); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// Verify both projects are returned (no filter applied)
	if len(respProjects) != 2 {
		t.Errorf("expected 2 projects without filters, got %d", len(respProjects))
	}
}

// TestClaimTaskWithModelMismatchReturns409ModelMismatch tests that claiming a task
// with a mismatched model returns HTTP 409 with error code MODEL_MISMATCH.
func TestClaimTaskWithModelMismatchReturns409ModelMismatch(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project and document
	projPayload := map[string]string{"name": "test-project", "repo": "https://github.com/example/repo"}
	projBody, _ := json.Marshal(projPayload)
	projReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projBody))
	projReq.Header.Set("Authorization", authHeader)
	projReq.Header.Set("Content-Type", "application/json")
	projW := httptest.NewRecorder()
	server.mux.ServeHTTP(projW, projReq)

	var proj store.Project
	json.NewDecoder(projW.Body).Decode(&proj)

	// Register a document
	docPayload := map[string]string{
		"kind":  "design",
		"title": "DESIGN.md",
		"ref":   "DESIGN.md",
	}
	docBody, _ := json.Marshal(docPayload)
	docReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/documents", bytes.NewReader(docBody))
	docReq.Header.Set("Authorization", authHeader)
	docReq.Header.Set("Content-Type", "application/json")
	docW := httptest.NewRecorder()
	server.mux.ServeHTTP(docW, docReq)

	var doc store.Document
	json.NewDecoder(docW.Body).Decode(&doc)

	// Create a task with model="haiku"
	tasksPayload := []map[string]interface{}{
		{
			"title":       "Test Task",
			"spec":        "Test spec",
			"document_id": doc.ID,
			"model":       "haiku",
		},
	}
	tasksBody, _ := json.Marshal(tasksPayload)
	tasksReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/tasks", bytes.NewReader(tasksBody))
	tasksReq.Header.Set("Authorization", authHeader)
	tasksReq.Header.Set("Content-Type", "application/json")
	tasksW := httptest.NewRecorder()
	server.mux.ServeHTTP(tasksW, tasksReq)

	var tasks []store.Task
	json.NewDecoder(tasksW.Body).Decode(&tasks)
	taskID := tasks[0].ID

	// Promote the task to ready
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	// Try to claim with mismatched model "sonnet"
	claimPayload := map[string]string{"agent_id": "test-agent", "model": "sonnet"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Should return 409 Conflict with MODEL_MISMATCH error code
	if claimW.Code != http.StatusConflict {
		t.Errorf("expected status 409, got %d", claimW.Code)
	}

	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(claimW.Body).Decode(&errResp)
	if errResp.Error.Code != "MODEL_MISMATCH" {
		t.Errorf("expected error code MODEL_MISMATCH, got %s", errResp.Error.Code)
	}
}

// TestClaimTaskWithEmptyModelReturns400EmptyModel tests that claiming a task
// with an empty model field returns HTTP 400 with error code EMPTY_MODEL.
func TestClaimTaskWithEmptyModelReturns400EmptyModel(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project and document
	projPayload := map[string]string{"name": "test-project", "repo": "https://github.com/example/repo"}
	projBody, _ := json.Marshal(projPayload)
	projReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(projBody))
	projReq.Header.Set("Authorization", authHeader)
	projReq.Header.Set("Content-Type", "application/json")
	projW := httptest.NewRecorder()
	server.mux.ServeHTTP(projW, projReq)

	var proj store.Project
	json.NewDecoder(projW.Body).Decode(&proj)

	// Register a document
	docPayload := map[string]string{
		"kind":  "design",
		"title": "DESIGN.md",
		"ref":   "DESIGN.md",
	}
	docBody, _ := json.Marshal(docPayload)
	docReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/documents", bytes.NewReader(docBody))
	docReq.Header.Set("Authorization", authHeader)
	docReq.Header.Set("Content-Type", "application/json")
	docW := httptest.NewRecorder()
	server.mux.ServeHTTP(docW, docReq)

	var doc store.Document
	json.NewDecoder(docW.Body).Decode(&doc)

	// Create a task with model="haiku"
	tasksPayload := []map[string]interface{}{
		{
			"title":       "Test Task",
			"spec":        "Test spec",
			"document_id": doc.ID,
			"model":       "haiku",
		},
	}
	tasksBody, _ := json.Marshal(tasksPayload)
	tasksReq := httptest.NewRequest("POST", "/projects/"+proj.ID+"/tasks", bytes.NewReader(tasksBody))
	tasksReq.Header.Set("Authorization", authHeader)
	tasksReq.Header.Set("Content-Type", "application/json")
	tasksW := httptest.NewRecorder()
	server.mux.ServeHTTP(tasksW, tasksReq)

	var tasks []store.Task
	json.NewDecoder(tasksW.Body).Decode(&tasks)
	taskID := tasks[0].ID

	// Promote the task to ready
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	// Try to claim with empty model
	claimPayload := map[string]string{"agent_id": "test-agent", "model": ""}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Should return 400 Bad Request with EMPTY_MODEL error code
	if claimW.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", claimW.Code)
	}

	var errResp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.NewDecoder(claimW.Body).Decode(&errResp)
	if errResp.Error.Code != "EMPTY_MODEL" {
		t.Errorf("expected error code EMPTY_MODEL, got %s", errResp.Error.Code)
	}
}

// TestGetTaskEventsReturnsEmptyArray verifies GET /tasks/{id}/events returns [] for task with no events.
func TestGetTaskEventsReturnsEmptyArray(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test Spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var tasks []store.Task
	json.NewDecoder(createW.Body).Decode(&tasks)
	taskID := tasks[0].ID

	// Get events for a task with no events (still in backlog)
	eventsReq := httptest.NewRequest("GET", "/tasks/"+taskID+"/events", nil)
	eventsReq.Header.Set("Authorization", authHeader)
	eventsW := httptest.NewRecorder()
	server.mux.ServeHTTP(eventsW, eventsReq)

	if eventsW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", eventsW.Code)
	}

	var events []store.Event
	if err := json.NewDecoder(eventsW.Body).Decode(&events); err != nil {
		t.Fatalf("failed to decode events: %v", err)
	}

	if events == nil {
		t.Error("expected empty array, got nil")
	}
	if len(events) != 0 {
		t.Errorf("expected 0 events, got %d", len(events))
	}
}

// TestGetTaskEventsReturnsChronologicalOrder verifies events are in chronological order.
func TestGetTaskEventsReturnsChronologicalOrder(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Use the helper to set up a claimed task
	taskID, _ := setupClaimedTask(t, server, authHeader)

	// Submit the task
	submitBody, _ := json.Marshal(map[string]interface{}{
		"agent_id": "test-agent",
		"result":   "Completed",
	})
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)
	if submitW.Code != http.StatusOK {
		t.Errorf("submit failed with status %d", submitW.Code)
	}

	// Get events
	eventsReq := httptest.NewRequest("GET", "/tasks/"+taskID+"/events", nil)
	eventsReq.Header.Set("Authorization", authHeader)
	eventsW := httptest.NewRecorder()
	server.mux.ServeHTTP(eventsW, eventsReq)

	if eventsW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", eventsW.Code)
	}

	var events []store.Event
	if err := json.NewDecoder(eventsW.Body).Decode(&events); err != nil {
		t.Fatalf("failed to decode events: %v", err)
	}

	if events == nil {
		t.Error("expected array, got nil")
	}

	if len(events) < 3 {
		t.Errorf("expected at least 3 events (promote, claim, submit), got %d", len(events))
	}

	// Verify chronological order (string comparison works for RFC3339 timestamps)
	for i := 1; i < len(events); i++ {
		if events[i].CreatedAt < events[i-1].CreatedAt {
			t.Errorf("events not in chronological order: event %d (%s) is before event %d (%s)", i, events[i].CreatedAt, i-1, events[i-1].CreatedAt)
		}
	}

	// Verify expected event kinds exist
	kinds := make(map[string]bool)
	for _, event := range events {
		kinds[event.Kind] = true
	}

	expectedKinds := []string{"transition", "claim", "submit"}
	for _, kind := range expectedKinds {
		if !kinds[kind] {
			t.Errorf("expected event kind %q not found", kind)
		}
	}
}

// TestGetTaskEventsRequiresAuth verifies GET /tasks/{id}/events requires authentication.
func TestGetTaskEventsRequiresAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	eventsReq := httptest.NewRequest("GET", "/tasks/some-id/events", nil)
	// No Authorization header
	eventsW := httptest.NewRecorder()
	server.mux.ServeHTTP(eventsW, eventsReq)

	if eventsW.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", eventsW.Code)
	}
}

// TestAPIResponseStrictlyValidJSONWithStoredControlChars verifies the read path
// always returns strictly-valid JSON even when a task's free-text fields contain
// raw control characters in the store (e.g. legacy data that predates write-side
// sanitization). The bad bytes are injected directly into the DB to bypass the
// write path, simulating an existing bad row. Both encoding/json (json.Valid uses
// the same scanner jq uses, which rejects unescaped U+0000–U+001F) and a full
// json.Unmarshal must accept the response, and the control chars must be ESCAPED —
// not stripped — so the spec round-trips losslessly on the wire.
func TestAPIResponseStrictlyValidJSONWithStoredControlChars(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a clean task first.
	taskPayload := []store.TaskInput{{
		Title:      "Control char task",
		Spec:       "clean spec",
		DocumentID: docID,
	}}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created []store.Task
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	taskID := created[0].ID

	// Inject raw control characters directly into the store, bypassing the write
	// path's sanitization, to simulate pre-existing bad data.
	badSpec := "line one\nline two\x00\x0bnull-and-vtab"
	badResult := "result with\x01\x1fcontrol"
	if _, err := server.store.Conn().ExecContext(context.Background(),
		"UPDATE task SET spec = ?, result = ? WHERE id = ?", badSpec, badResult, taskID); err != nil {
		t.Fatalf("failed to inject control chars: %v", err)
	}

	// Both GET /tasks/{id} and GET /projects/{id}/tasks must return strictly-valid JSON.
	for _, ep := range []string{"/tasks/" + taskID, "/projects/" + projectID + "/tasks"} {
		getReq := httptest.NewRequest("GET", ep, nil)
		getReq.Header.Set("Authorization", authHeader)
		getW := httptest.NewRecorder()
		server.mux.ServeHTTP(getW, getReq)
		if getW.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", ep, getW.Code)
		}
		body := getW.Body.Bytes()

		// Strict-parser check: equivalent to what jq enforces.
		if !json.Valid(body) {
			t.Errorf("%s: response is not strictly-valid JSON: %q", ep, string(body))
		}
		// No literal control bytes may appear in the wire body (json.Encoder writes a
		// single trailing newline, which is the only legitimate raw \n).
		for i, b := range body {
			if b < 0x20 && b != '\n' {
				t.Errorf("%s: raw response contains unescaped control byte 0x%02x at offset %d", ep, b, i)
				break
			}
		}
	}

	// Field readability + lossless escaping: spec/result must unmarshal back to exactly
	// what was stored — control chars preserved, just escaped on the wire.
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)
	var got store.TaskWithDepsAndLinks
	if err := json.Unmarshal(getW.Body.Bytes(), &got); err != nil {
		t.Fatalf("strict unmarshal failed: %v", err)
	}
	if got.Spec != badSpec {
		t.Errorf("spec not preserved through escaping:\n got %q\nwant %q", got.Spec, badSpec)
	}
	if got.Result == nil || *got.Result != badResult {
		t.Errorf("result not preserved through escaping: got %v want %q", got.Result, badResult)
	}
}

// TestCreateTaskSanitizesControlChars verifies the write path strips raw control
// characters from free-text on create while preserving legitimate newlines and tabs,
// and that the response is strictly-valid JSON.
func TestCreateTaskSanitizesControlChars(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	spec := "first line\nsecond\tline\x00\x0b\x1fjunk"
	wantSpec := "first line\nsecond\tlinejunk" // \n and \t kept; \x00 \x0b \x1f dropped
	taskPayload := []store.TaskInput{{
		Title:      "Title\x00with\x07controls",
		Spec:       spec,
		DocumentID: docID,
	}}
	taskBody, _ := json.Marshal(taskPayload)
	req := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("create response is not strictly-valid JSON")
	}
	var created []store.Task
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created[0].Spec != wantSpec {
		t.Errorf("spec not sanitized:\n got %q\nwant %q", created[0].Spec, wantSpec)
	}
	if created[0].Title != "Titlewithcontrols" {
		t.Errorf("title not sanitized: got %q", created[0].Title)
	}
}

// TestUpdateTaskDependsOnSucceeds verifies that updating depends_on succeeds.
func TestUpdateTaskDependsOnSucceeds(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create two tasks
	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "First task",
			DocumentID: docID,
		},
		{
			Title:      "Task 2",
			Spec:       "Second task",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	task1ID := createdTasks[0].ID
	task2ID := createdTasks[1].ID

	// Update task2 to depend on task1
	updatePayload := map[string]interface{}{
		"depends_on": []string{task1ID},
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+task2ID, bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var updatedTask store.Task
	if err := json.NewDecoder(updateW.Body).Decode(&updatedTask); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if updatedTask.ID != task2ID {
		t.Errorf("expected task id %q, got %q", task2ID, updatedTask.ID)
	}
}

// TestUpdateTaskDependsOnSelfDependencyReturns400 verifies that self-dependency returns 400 with SELF_DEPENDENCY code.
func TestUpdateTaskDependsOnSelfDependencyReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "First task",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Try to make task depend on itself
	updatePayload := map[string]interface{}{
		"depends_on": []string{taskID},
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID, bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var errResp map[string]interface{}
	json.NewDecoder(updateW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "SELF_DEPENDENCY" {
		t.Errorf("expected error code SELF_DEPENDENCY, got %q", errObj["code"])
	}
}

// TestUpdateTaskDependsOnCycleReturns409 verifies that cycle detection returns 409 with CYCLE_DETECTED code.
func TestUpdateTaskDependsOnCycleReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create three tasks
	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "First task",
			DocumentID: docID,
		},
		{
			Title:      "Task 2",
			Spec:       "Second task",
			DocumentID: docID,
		},
		{
			Title:      "Task 3",
			Spec:       "Third task",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	task1ID := createdTasks[0].ID
	task2ID := createdTasks[1].ID
	task3ID := createdTasks[2].ID

	// Set up: task1 -> task2 -> task3
	// First, make task2 depend on task1
	update1Payload := map[string]interface{}{
		"depends_on": []string{task1ID},
	}
	update1Body, _ := json.Marshal(update1Payload)
	update1Req := httptest.NewRequest("PATCH", "/tasks/"+task2ID, bytes.NewReader(update1Body))
	update1Req.Header.Set("Authorization", authHeader)
	update1Req.Header.Set("Content-Type", "application/json")
	update1W := httptest.NewRecorder()
	server.mux.ServeHTTP(update1W, update1Req)

	// Make task3 depend on task2
	update2Payload := map[string]interface{}{
		"depends_on": []string{task2ID},
	}
	update2Body, _ := json.Marshal(update2Payload)
	update2Req := httptest.NewRequest("PATCH", "/tasks/"+task3ID, bytes.NewReader(update2Body))
	update2Req.Header.Set("Authorization", authHeader)
	update2Req.Header.Set("Content-Type", "application/json")
	update2W := httptest.NewRecorder()
	server.mux.ServeHTTP(update2W, update2Req)

	// Try to make task1 depend on task3, creating a cycle: task1 -> task3 -> task2 -> task1
	cyclePayload := map[string]interface{}{
		"depends_on": []string{task3ID},
	}
	cycleBody, _ := json.Marshal(cyclePayload)
	cycleReq := httptest.NewRequest("PATCH", "/tasks/"+task1ID, bytes.NewReader(cycleBody))
	cycleReq.Header.Set("Authorization", authHeader)
	cycleReq.Header.Set("Content-Type", "application/json")
	cycleW := httptest.NewRecorder()
	server.mux.ServeHTTP(cycleW, cycleReq)

	if cycleW.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d; body: %s", cycleW.Code, cycleW.Body.String())
	}

	var errResp map[string]interface{}
	json.NewDecoder(cycleW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "CYCLE_DETECTED" {
		t.Errorf("expected error code CYCLE_DETECTED, got %q", errObj["code"])
	}
}

// TestUpdateTaskDependsOnUnknownTaskReturns404 verifies that unknown task returns 404.
func TestUpdateTaskDependsOnUnknownTaskReturns404(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Try to update a non-existent task
	updatePayload := map[string]interface{}{
		"depends_on": []string{},
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/unknown-task-id", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var errResp map[string]interface{}
	json.NewDecoder(updateW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "NOT_FOUND" {
		t.Errorf("expected error code NOT_FOUND, got %q", errObj["code"])
	}
}

// TestUpdateTaskDependsOnRequiresAuth verifies that update endpoint requires auth.
func TestUpdateTaskDependsOnRequiresAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	// Try to update without auth
	updatePayload := map[string]interface{}{
		"depends_on": []string{},
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/some-task-id", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", updateW.Code)
	}
}

// TestSupersedTaskCreatesNewTask verifies that superseding a task creates a new task with copied fields.
func TestSupersedTaskCreatesNewTask(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task to supersede
	taskPayload := []store.TaskInput{
		{
			Title:      "Original Task",
			Spec:       "Original specification",
			DocumentID: docID,
			Model:      "haiku",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	originalTaskID := createdTasks[0].ID
	originalModel := createdTasks[0].Model

	// Supersede the task without model override
	supersedePayload := map[string]interface{}{}
	supersedeBody, _ := json.Marshal(supersedePayload)
	supersedeReq := httptest.NewRequest("POST", "/tasks/"+originalTaskID+"/supersede", bytes.NewReader(supersedeBody))
	supersedeReq.Header.Set("Authorization", authHeader)
	supersedeReq.Header.Set("Content-Type", "application/json")
	supersedeW := httptest.NewRecorder()
	server.mux.ServeHTTP(supersedeW, supersedeReq)

	if supersedeW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d; body: %s", supersedeW.Code, supersedeW.Body.String())
	}

	var newTask store.Task
	if err := json.NewDecoder(supersedeW.Body).Decode(&newTask); err != nil {
		t.Fatalf("failed to decode supersede response: %v", err)
	}

	// Verify new task has different ID
	if newTask.ID == originalTaskID {
		t.Errorf("expected new task ID to be different from original, got same ID %q", newTask.ID)
	}

	// Verify new task has same title and spec
	if newTask.Title != "Original Task" {
		t.Errorf("expected title 'Original Task', got %q", newTask.Title)
	}
	if newTask.Spec != "Original specification" {
		t.Errorf("expected spec 'Original specification', got %q", newTask.Spec)
	}

	// Verify model is preserved
	if newTask.Model != originalModel {
		t.Errorf("expected model %q, got %q", originalModel, newTask.Model)
	}

	// Verify new task is in backlog state
	if newTask.State != "backlog" {
		t.Errorf("expected state 'backlog', got %q", newTask.State)
	}
}

// TestSupersedTaskWithModelOverride verifies that superseding a task with model override uses the new model.
func TestSupersedTaskWithModelOverride(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task with haiku model
	taskPayload := []store.TaskInput{
		{
			Title:      "Task with Haiku",
			Spec:       "Test specification",
			DocumentID: docID,
			Model:      "haiku",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	originalTaskID := createdTasks[0].ID

	// Supersede with model override to sonnet
	supersedePayload := map[string]interface{}{
		"model": "sonnet",
	}
	supersedeBody, _ := json.Marshal(supersedePayload)
	supersedeReq := httptest.NewRequest("POST", "/tasks/"+originalTaskID+"/supersede", bytes.NewReader(supersedeBody))
	supersedeReq.Header.Set("Authorization", authHeader)
	supersedeReq.Header.Set("Content-Type", "application/json")
	supersedeW := httptest.NewRecorder()
	server.mux.ServeHTTP(supersedeW, supersedeReq)

	if supersedeW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", supersedeW.Code)
	}

	var newTask store.Task
	if err := json.NewDecoder(supersedeW.Body).Decode(&newTask); err != nil {
		t.Fatalf("failed to decode supersede response: %v", err)
	}

	// Verify new task has the overridden model
	if newTask.Model != "sonnet" {
		t.Errorf("expected model 'sonnet', got %q", newTask.Model)
	}
}

// TestSupersedTaskUnknownIDReturns404 verifies that superseding an unknown task returns 404.
func TestSupersedTaskUnknownIDReturns404(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	supersedePayload := map[string]interface{}{}
	supersedeBody, _ := json.Marshal(supersedePayload)
	supersedeReq := httptest.NewRequest("POST", "/tasks/nonexistent-id/supersede", bytes.NewReader(supersedeBody))
	supersedeReq.Header.Set("Authorization", authHeader)
	supersedeReq.Header.Set("Content-Type", "application/json")
	supersedeW := httptest.NewRecorder()
	server.mux.ServeHTTP(supersedeW, supersedeReq)

	if supersedeW.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", supersedeW.Code)
	}

	var errResp map[string]interface{}
	json.NewDecoder(supersedeW.Body).Decode(&errResp)
	errObj := errResp["error"].(map[string]interface{})
	if errObj["code"] != "NOT_FOUND" {
		t.Errorf("expected error code 'NOT_FOUND', got %q", errObj["code"])
	}
}

// TestSupersedTaskRequiresAuth verifies that supersede endpoint requires auth.
func TestSupersedTaskRequiresAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")

	supersedePayload := map[string]interface{}{}
	supersedeBody, _ := json.Marshal(supersedePayload)
	supersedeReq := httptest.NewRequest("POST", "/tasks/some-id/supersede", bytes.NewReader(supersedeBody))
	// No Authorization header
	supersedeW := httptest.NewRecorder()
	server.mux.ServeHTTP(supersedeW, supersedeReq)

	if supersedeW.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", supersedeW.Code)
	}
}

// TestSupersedTaskTerminalStateReturns409 verifies that superseding a task in a terminal state returns 409.
func TestSupersedTaskTerminalStateReturns409(t *testing.T) {
	terminalStates := []string{"done", "failed", "abandoned", "superseded"}

	for _, terminalState := range terminalStates {
		t.Run("state_"+terminalState, func(t *testing.T) {
			server := setupTestServer(t, "test-token")
			authHeader := "Bearer test-token"

			projectID, docID := setupProjectAndDocument(t, server, authHeader)

			// Create a task and a dependent
			taskPayload := []store.TaskInput{
				{
					Title:      "Task to Supersede",
					Spec:       "Test specification",
					DocumentID: docID,
					Model:      "haiku",
				},
				{
					Title:      "Dependent Task",
					Spec:       "Dependent specification",
					DocumentID: docID,
					Model:      "haiku",
				},
			}
			taskBody, _ := json.Marshal(taskPayload)
			createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
			createReq.Header.Set("Authorization", authHeader)
			createReq.Header.Set("Content-Type", "application/json")
			createW := httptest.NewRecorder()
			server.mux.ServeHTTP(createW, createReq)

			var createdTasks []store.Task
			json.NewDecoder(createW.Body).Decode(&createdTasks)
			taskID := createdTasks[0].ID
			dependentID := createdTasks[1].ID

			// Set the dependent to depend on the task
			depPayload := map[string]interface{}{"depends_on": []string{taskID}}
			depBody, _ := json.Marshal(depPayload)
			depReq := httptest.NewRequest("PATCH", "/tasks/"+dependentID, bytes.NewReader(depBody))
			depReq.Header.Set("Authorization", authHeader)
			depReq.Header.Set("Content-Type", "application/json")
			depW := httptest.NewRecorder()
			server.mux.ServeHTTP(depW, depReq)

			// Manually set the task to terminal state
			conn := server.store.Conn()
			_, err := conn.ExecContext(context.Background(), "UPDATE task SET state = ? WHERE id = ?", terminalState, taskID)
			if err != nil {
				t.Fatalf("failed to set task to terminal state: %v", err)
			}

			// Attempt to supersede the task in terminal state
			supersedePayload := map[string]interface{}{}
			supersedeBody, _ := json.Marshal(supersedePayload)
			supersedeReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/supersede", bytes.NewReader(supersedeBody))
			supersedeReq.Header.Set("Authorization", authHeader)
			supersedeReq.Header.Set("Content-Type", "application/json")
			supersedeW := httptest.NewRecorder()
			server.mux.ServeHTTP(supersedeW, supersedeReq)

			// Verify 409 status
			if supersedeW.Code != http.StatusConflict {
				t.Errorf("expected status 409, got %d; body: %s", supersedeW.Code, supersedeW.Body.String())
			}

			// Verify error response
			var errResp map[string]interface{}
			json.NewDecoder(supersedeW.Body).Decode(&errResp)
			if errResp["error"] == nil {
				t.Errorf("expected error in response, got none")
			}

			// Verify task state is unchanged
			getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
			getReq.Header.Set("Authorization", authHeader)
			getW := httptest.NewRecorder()
			server.mux.ServeHTTP(getW, getReq)

			var taskAfter store.TaskWithDepsAndLinks
			json.NewDecoder(getW.Body).Decode(&taskAfter)
			if taskAfter.State != terminalState {
				t.Errorf("expected task state to remain %q, got %q", terminalState, taskAfter.State)
			}

			// Verify dependent still depends on original task
			getDepReq := httptest.NewRequest("GET", "/tasks/"+dependentID, nil)
			getDepReq.Header.Set("Authorization", authHeader)
			getDepW := httptest.NewRecorder()
			server.mux.ServeHTTP(getDepW, getDepReq)

			var depAfter store.TaskWithDepsAndLinks
			json.NewDecoder(getDepW.Body).Decode(&depAfter)
			if len(depAfter.DependsOn) != 1 || depAfter.DependsOn[0] != taskID {
				t.Errorf("expected dependent to still depend on task %q, got %v", taskID, depAfter.DependsOn)
			}
		})
	}
}

// TestListTasksWithIncludeSupersededFilter verifies that superseded tasks are excluded by default
// but can be included with the include_superseded filter.
func TestListTasksWithIncludeSupersededFilter(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task
	taskPayload := []store.TaskInput{
		{
			Title:      "Task to Supersede",
			Spec:       "Test specification",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	oldTaskID := createdTasks[0].ID

	// Verify old task appears in list before superseding
	listReq1 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks", nil)
	listReq1.Header.Set("Authorization", authHeader)
	listW1 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW1, listReq1)

	var listBefore []store.Task
	json.NewDecoder(listW1.Body).Decode(&listBefore)
	found := false
	for _, task := range listBefore {
		if task.ID == oldTaskID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected oldTask to appear in list before superseding")
	}

	// Supersede the task
	supersedePayload := map[string]interface{}{}
	supersedeBody, _ := json.Marshal(supersedePayload)
	supersedeReq := httptest.NewRequest("POST", "/tasks/"+oldTaskID+"/supersede", bytes.NewReader(supersedeBody))
	supersedeReq.Header.Set("Authorization", authHeader)
	supersedeReq.Header.Set("Content-Type", "application/json")
	supersedeW := httptest.NewRecorder()
	server.mux.ServeHTTP(supersedeW, supersedeReq)

	if supersedeW.Code != http.StatusCreated {
		t.Errorf("expected status 201, got %d", supersedeW.Code)
	}

	var newTask store.Task
	json.NewDecoder(supersedeW.Body).Decode(&newTask)

	// Verify old task is excluded from list by default
	listReq2 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks", nil)
	listReq2.Header.Set("Authorization", authHeader)
	listW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW2, listReq2)

	var listAfter []store.Task
	json.NewDecoder(listW2.Body).Decode(&listAfter)
	for _, task := range listAfter {
		if task.ID == oldTaskID {
			t.Errorf("expected oldTask to be excluded from list after superseding")
		}
	}

	// Verify new task appears in list
	found = false
	for _, task := range listAfter {
		if task.ID == newTask.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected newTask to appear in list after superseding")
	}

	// Verify old task can be included with include_superseded=true
	listReq3 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?include_superseded=true", nil)
	listReq3.Header.Set("Authorization", authHeader)
	listW3 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW3, listReq3)

	var listWithSuperseded []store.Task
	json.NewDecoder(listW3.Body).Decode(&listWithSuperseded)
	found = false
	for _, task := range listWithSuperseded {
		if task.ID == oldTaskID {
			found = true
			if task.State != "superseded" {
				t.Errorf("expected oldTask state to be superseded, got %q", task.State)
			}
			if task.SupersededBy == nil || *task.SupersededBy != newTask.ID {
				t.Errorf("expected oldTask.SupersededBy to be %s, got %v", newTask.ID, task.SupersededBy)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected oldTask to appear in list with include_superseded=true")
	}
}

// Helper to submit a review verdict from a review task.
func submitReviewVerdict(t *testing.T, server *Server, authHeader string, reviewTaskID string, verdict string, note string) {
	claimPayload := map[string]string{"agent_id": "reviewer-agent", "model": "opus"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+reviewTaskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)
	if claimW.Code != http.StatusOK {
		t.Fatalf("failed to claim review task: got status %d", claimW.Code)
	}

	submitPayload := map[string]interface{}{
		"agent_id": "reviewer-agent",
		"result":   note,
		"verdict":  verdict,
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+reviewTaskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)
	if submitW.Code != http.StatusOK {
		t.Fatalf("failed to submit review verdict: got status %d; body: %s", submitW.Code, submitW.Body.String())
	}
}

// TestEndToEndEscalationLadder verifies the escalation ladder: haiku → sonnet → opus, then blocks.
func TestEndToEndEscalationLadder(t *testing.T) {
	// Use custom thresholds for testing: haiku=2, sonnet=2, opus=2
	thresholds := map[string]int{"haiku": 2, "sonnet": 2, "opus": 2}
	server := setupTestServerWithThresholds(t, "test-token", thresholds)
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create an implement task with haiku model
	taskPayload := []store.TaskInput{
		{
			Title:      "Escalation Test Task",
			Spec:       "Test task for escalation ladder",
			DocumentID: docID,
			Model:      "haiku",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Verify initial model is haiku
	if createdTasks[0].Model != "haiku" {
		t.Fatalf("expected model 'haiku', got %q", createdTasks[0].Model)
	}

	// Promote to ready
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	// Helper to promote a backlog task to ready (skip if already ready)
	promoteTask := func(taskID string) {
		// Check current state first
		getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
		getReq.Header.Set("Authorization", authHeader)
		getW := httptest.NewRecorder()
		server.mux.ServeHTTP(getW, getReq)
		var task store.TaskWithDepsAndLinks
		json.NewDecoder(getW.Body).Decode(&task)

		// Only promote if in backlog state
		if task.State == "backlog" {
			promReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
			promReq.Header.Set("Authorization", authHeader)
			promW := httptest.NewRecorder()
			server.mux.ServeHTTP(promW, promReq)
			if promW.Code != http.StatusOK {
				t.Fatalf("failed to promote task: got status %d", promW.Code)
			}
		}
	}

	// Helper to claim, submit, and get review tasks for rejecting
	rejectAndEscalate := func(currentTaskID string, expectedModel string, expectedReviewRound int) (string, *string) {
		// Claim as the current model
		claimPayload := map[string]string{"agent_id": "impl-agent", "model": expectedModel}
		claimBody, _ := json.Marshal(claimPayload)
		claimReq := httptest.NewRequest("POST", "/tasks/"+currentTaskID+"/claim", bytes.NewReader(claimBody))
		claimReq.Header.Set("Authorization", authHeader)
		claimReq.Header.Set("Content-Type", "application/json")
		claimW := httptest.NewRecorder()
		server.mux.ServeHTTP(claimW, claimReq)
		if claimW.Code != http.StatusOK {
			t.Fatalf("failed to claim task as %s: got status %d", expectedModel, claimW.Code)
		}

		// Submit to move to review (spawns review tasks)
		submitPayload := map[string]interface{}{
			"agent_id": "impl-agent",
			"result":   "Work in progress",
			"links":    []map[string]string{},
		}
		submitBody, _ := json.Marshal(submitPayload)
		submitReq := httptest.NewRequest("POST", "/tasks/"+currentTaskID+"/submit", bytes.NewReader(submitBody))
		submitReq.Header.Set("Authorization", authHeader)
		submitReq.Header.Set("Content-Type", "application/json")
		submitW := httptest.NewRecorder()
		server.mux.ServeHTTP(submitW, submitReq)
		if submitW.Code != http.StatusOK {
			t.Fatalf("failed to submit task: got status %d; body: %s", submitW.Code, submitW.Body.String())
		}

		var submittedTask store.TaskWithDepsAndLinks
		json.NewDecoder(submitW.Body).Decode(&submittedTask)
		if submittedTask.ReviewRound != expectedReviewRound {
			t.Errorf("expected review_round %d, got %d", expectedReviewRound, submittedTask.ReviewRound)
		}

		// Get the review tasks for the current round (list all review tasks for project, then filter)
		listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?kind=review&state=ready", nil)
		listReq.Header.Set("Authorization", authHeader)
		listW := httptest.NewRecorder()
		server.mux.ServeHTTP(listW, listReq)

		var allReviewTasks []store.Task
		json.NewDecoder(listW.Body).Decode(&allReviewTasks)

		// Find the review task for this parent
		var reviewTaskID string
		for _, task := range allReviewTasks {
			if task.TargetTaskID != nil && *task.TargetTaskID == currentTaskID {
				reviewTaskID = task.ID
				break
			}
		}
		if reviewTaskID == "" {
			t.Fatalf("expected 1 ready review task for %s, got none", currentTaskID)
		}

		// Submit reject verdict
		submitReviewVerdict(t, server, authHeader, reviewTaskID, "reject", "Needs improvement")

		// Get the parent task to see if it escalated
		getReq := httptest.NewRequest("GET", "/tasks/"+currentTaskID, nil)
		getReq.Header.Set("Authorization", authHeader)
		getW := httptest.NewRecorder()
		server.mux.ServeHTTP(getW, getReq)

		var parentTask store.TaskWithDepsAndLinks
		json.NewDecoder(getW.Body).Decode(&parentTask)

		return currentTaskID, parentTask.SupersededBy
	}

	// Rounds 1-2: haiku task, submit and reject (no escalation yet)
	currentTaskID := taskID
	currentModel := "haiku"
	for round := 1; round <= 2; round++ {
		_, supersededBy := rejectAndEscalate(currentTaskID, currentModel, round)
		if supersededBy != nil {
			t.Fatalf("expected no escalation at round %d, but task was superseded by %s", round, *supersededBy)
		}
	}

	// Round 3: haiku task should escalate to sonnet
	_, supersededBy := rejectAndEscalate(currentTaskID, currentModel, 3)
	if supersededBy == nil {
		t.Fatalf("expected task to escalate at round 3, but SupersededBy is nil")
	}
	escalatedTaskID := *supersededBy

	// Promote the escalated task to ready
	promoteTask(escalatedTaskID)

	// Verify the escalated task is sonnet
	getReq := httptest.NewRequest("GET", "/tasks/"+escalatedTaskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)
	var escalatedTask store.TaskWithDepsAndLinks
	json.NewDecoder(getW.Body).Decode(&escalatedTask)

	if escalatedTask.Model != "sonnet" {
		t.Errorf("expected escalated model 'sonnet', got %q", escalatedTask.Model)
	}
	if escalatedTask.ReviewRound != 0 {
		t.Errorf("expected escalated task to start at review_round 0, got %d", escalatedTask.ReviewRound)
	}

	// Continue with sonnet: rounds 1-2 before escalating to opus
	currentTaskID = escalatedTaskID
	currentModel = "sonnet"
	for round := 1; round <= 2; round++ {
		_, supersededBy := rejectAndEscalate(currentTaskID, currentModel, round)
		if supersededBy != nil {
			t.Fatalf("expected no escalation for sonnet at round %d, but task was superseded", round)
		}
	}

	// Round 3: sonnet task should escalate to opus
	_, supersededBy = rejectAndEscalate(currentTaskID, currentModel, 3)
	if supersededBy == nil {
		t.Fatalf("expected sonnet task to escalate to opus at round 3, but SupersededBy is nil")
	}
	opusTaskID := *supersededBy

	// Promote the escalated task to ready
	promoteTask(opusTaskID)

	// Verify the escalated task is opus
	getReq = httptest.NewRequest("GET", "/tasks/"+opusTaskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW = httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)
	json.NewDecoder(getW.Body).Decode(&escalatedTask)

	if escalatedTask.Model != "opus" {
		t.Errorf("expected final escalated model 'opus', got %q", escalatedTask.Model)
	}

	// Opus at rounds 1-2: should not block yet
	currentTaskID = opusTaskID
	currentModel = "opus"
	for round := 1; round <= 2; round++ {
		_, supersededBy := rejectAndEscalate(currentTaskID, currentModel, round)
		if supersededBy != nil {
			t.Fatalf("expected no escalation for opus at round %d, but task was superseded", round)
		}
	}

	// At round 3, opus should block (no escalation since it's the top tier)
	claimPayload := map[string]string{"agent_id": "impl-agent", "model": "opus"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+currentTaskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	submitPayload := map[string]interface{}{
		"agent_id": "impl-agent",
		"result":   "Work in progress",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+currentTaskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	var submittedTask store.TaskWithDepsAndLinks
	json.NewDecoder(submitW.Body).Decode(&submittedTask)

	// Get and reject via review task
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?kind=review&state=ready", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	var allReviewTasks []store.Task
	json.NewDecoder(listW.Body).Decode(&allReviewTasks)

	var reviewTaskID string
	for _, task := range allReviewTasks {
		if task.TargetTaskID != nil && *task.TargetTaskID == currentTaskID {
			reviewTaskID = task.ID
			break
		}
	}
	if reviewTaskID == "" {
		t.Fatalf("expected 1 ready review task for final round, got none")
	}

	submitReviewVerdict(t, server, authHeader, reviewTaskID, "reject", "Final rejection")

	// Check that opus task is now blocked
	getReq = httptest.NewRequest("GET", "/tasks/"+currentTaskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW = httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var finalTask store.TaskWithDepsAndLinks
	json.NewDecoder(getW.Body).Decode(&finalTask)

	if finalTask.State != "blocked" {
		t.Errorf("expected opus task to be 'blocked' at threshold, got %q", finalTask.State)
	}
	if finalTask.SupersededBy != nil {
		t.Errorf("expected no supersession for top-tier opus, but SupersededBy is %v", finalTask.SupersededBy)
	}
}

// TestEscalateOptOutBlocksAtFirstThreshold verifies that escalate=false blocks at first threshold.
func TestEscalateOptOutBlocksAtFirstThreshold(t *testing.T) {
	// Use custom thresholds for testing: haiku=2
	thresholds := map[string]int{"haiku": 2}
	server := setupTestServerWithThresholds(t, "test-token", thresholds)
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a haiku task with escalate=false
	escalateDisabled := false
	taskPayload := []store.TaskInput{
		{
			Title:      "No Escalation Task",
			Spec:       "Task with escalation disabled",
			DocumentID: docID,
			Model:      "haiku",
			Escalate:   &escalateDisabled,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Promote to ready
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	// Claim and submit at threshold (round 5) with a reject
	claimPayload := map[string]string{"agent_id": "impl-agent", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Cycle through rejection loops until at threshold (2 rounds)
	for round := 0; round < 2; round++ {
		submitPayload := map[string]interface{}{
			"agent_id": "impl-agent",
			"result":   "Work in progress",
			"links":    []map[string]string{},
		}
		submitBody, _ := json.Marshal(submitPayload)
		submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
		submitReq.Header.Set("Authorization", authHeader)
		submitReq.Header.Set("Content-Type", "application/json")
		submitW := httptest.NewRecorder()
		server.mux.ServeHTTP(submitW, submitReq)

		// Get review task
		listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?kind=review&state=ready", nil)
		listReq.Header.Set("Authorization", authHeader)
		listW := httptest.NewRecorder()
		server.mux.ServeHTTP(listW, listReq)

		var allReviewTasks []store.Task
		json.NewDecoder(listW.Body).Decode(&allReviewTasks)

		var reviewTaskID string
		for _, task := range allReviewTasks {
			if task.TargetTaskID != nil && *task.TargetTaskID == taskID {
				reviewTaskID = task.ID
				break
			}
		}
		if reviewTaskID == "" {
			t.Fatalf("round %d: expected 1 review task, got none", round)
		}

		// Reject
		submitReviewVerdict(t, server, authHeader, reviewTaskID, "reject", "Needs work")

		// Re-claim for next round
		if round <= 5 {
			claimReq = httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
			claimReq.Header.Set("Authorization", authHeader)
			claimReq.Header.Set("Content-Type", "application/json")
			claimW = httptest.NewRecorder()
			server.mux.ServeHTTP(claimW, claimReq)
		}
	}

	// At round 3 (threshold exceeded), it should block instead of escalate
	submitPayload := map[string]interface{}{
		"agent_id": "impl-agent",
		"result":   "Final attempt",
		"links":    []map[string]string{},
	}
	submitBody, _ := json.Marshal(submitPayload)
	submitReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/submit", bytes.NewReader(submitBody))
	submitReq.Header.Set("Authorization", authHeader)
	submitReq.Header.Set("Content-Type", "application/json")
	submitW := httptest.NewRecorder()
	server.mux.ServeHTTP(submitW, submitReq)

	// Get review task
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?kind=review&state=ready", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	var allReviewTasks []store.Task
	json.NewDecoder(listW.Body).Decode(&allReviewTasks)

	var reviewTaskID string
	for _, task := range allReviewTasks {
		if task.TargetTaskID != nil && *task.TargetTaskID == taskID {
			reviewTaskID = task.ID
			break
		}
	}

	// Reject at threshold
	submitReviewVerdict(t, server, authHeader, reviewTaskID, "reject", "Still needs work")

	// Verify task is blocked (not escalated)
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var finalTask store.TaskWithDepsAndLinks
	json.NewDecoder(getW.Body).Decode(&finalTask)

	if finalTask.State != "blocked" {
		t.Errorf("expected task with escalate=false to be 'blocked' at threshold, got %q", finalTask.State)
	}
	if finalTask.SupersededBy != nil {
		t.Errorf("expected no supersession for escalate=false task, but SupersededBy is %v", finalTask.SupersededBy)
	}
}

// TestArchiveProjectAndGetArchivedAt verifies that archiving a project sets archived_at and GET returns it.
func TestArchiveProjectAndGetArchivedAt(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create a project
	createPayload := map[string]string{
		"name": "archive-test-project",
		"repo": "https://github.com/example/test-repo",
	}
	createBody, _ := json.Marshal(createPayload)
	createReq := httptest.NewRequest("POST", "/projects", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var project store.Project
	json.NewDecoder(createW.Body).Decode(&project)

	// Verify initial archived_at is nil
	if project.ArchivedAt != nil {
		t.Errorf("expected ArchivedAt to be nil for new project, got %v", project.ArchivedAt)
	}

	// Archive the project
	archiveReq := httptest.NewRequest("POST", "/projects/"+project.ID+"/archive", nil)
	archiveReq.Header.Set("Authorization", authHeader)
	archiveW := httptest.NewRecorder()
	server.mux.ServeHTTP(archiveW, archiveReq)

	if archiveW.Code != http.StatusOK {
		t.Errorf("expected status 200 for archive, got %d", archiveW.Code)
	}

	var archivedProject store.Project
	json.NewDecoder(archiveW.Body).Decode(&archivedProject)

	// Verify archived_at is set
	if archivedProject.ArchivedAt == nil {
		t.Error("expected ArchivedAt to be set after archiving, got nil")
	}

	// Get the project and verify archived_at is returned
	getReq := httptest.NewRequest("GET", "/projects/"+project.ID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var retrievedProject store.Project
	json.NewDecoder(getW.Body).Decode(&retrievedProject)

	if retrievedProject.ArchivedAt == nil {
		t.Error("expected ArchivedAt in GET response, got nil")
	}
	// Compare string values, not pointers
	if *archivedProject.ArchivedAt != *retrievedProject.ArchivedAt {
		t.Errorf("archived_at mismatch: archive response %q != get response %q", *archivedProject.ArchivedAt, *retrievedProject.ArchivedAt)
	}
}

// TestListProjectsExcludesArchivedByDefault verifies that GET /projects excludes archived projects.
func TestListProjectsExcludesArchivedByDefault(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create two projects
	p1 := map[string]string{"name": "active-project", "repo": "https://github.com/example/active"}
	p1Body, _ := json.Marshal(p1)
	p1Req := httptest.NewRequest("POST", "/projects", bytes.NewReader(p1Body))
	p1Req.Header.Set("Authorization", authHeader)
	p1Req.Header.Set("Content-Type", "application/json")
	p1W := httptest.NewRecorder()
	server.mux.ServeHTTP(p1W, p1Req)
	var activeProject store.Project
	json.NewDecoder(p1W.Body).Decode(&activeProject)

	p2 := map[string]string{"name": "archived-project", "repo": "https://github.com/example/archived"}
	p2Body, _ := json.Marshal(p2)
	p2Req := httptest.NewRequest("POST", "/projects", bytes.NewReader(p2Body))
	p2Req.Header.Set("Authorization", authHeader)
	p2Req.Header.Set("Content-Type", "application/json")
	p2W := httptest.NewRecorder()
	server.mux.ServeHTTP(p2W, p2Req)
	var archivedProject store.Project
	json.NewDecoder(p2W.Body).Decode(&archivedProject)

	// Archive the second project
	archiveReq := httptest.NewRequest("POST", "/projects/"+archivedProject.ID+"/archive", nil)
	archiveReq.Header.Set("Authorization", authHeader)
	archiveW := httptest.NewRecorder()
	server.mux.ServeHTTP(archiveW, archiveReq)

	// List projects without include_archived
	listReq := httptest.NewRequest("GET", "/projects", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	var projects []store.Project
	json.NewDecoder(listW.Body).Decode(&projects)

	// Find our projects in the list (filter by name to handle test isolation)
	var foundActive, foundArchived bool
	for _, p := range projects {
		if p.ID == activeProject.ID && p.Name == "active-project" {
			foundActive = true
		}
		if p.ID == archivedProject.ID && p.Name == "archived-project" {
			foundArchived = true
		}
	}

	// Verify only the active project is returned
	if !foundActive {
		t.Error("expected active project in list")
	}
	if foundArchived {
		t.Error("did not expect archived project in default list (should exclude archived)")
	}
}

// TestListProjectsIncludesArchivedWithFlag verifies that GET /projects?include_archived=true returns archived projects.
func TestListProjectsIncludesArchivedWithFlag(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	// Create two projects
	p1 := map[string]string{"name": "active-project-2", "repo": "https://github.com/example/active2"}
	p1Body, _ := json.Marshal(p1)
	p1Req := httptest.NewRequest("POST", "/projects", bytes.NewReader(p1Body))
	p1Req.Header.Set("Authorization", authHeader)
	p1Req.Header.Set("Content-Type", "application/json")
	p1W := httptest.NewRecorder()
	server.mux.ServeHTTP(p1W, p1Req)
	var activeProject store.Project
	json.NewDecoder(p1W.Body).Decode(&activeProject)

	p2 := map[string]string{"name": "archived-project-2", "repo": "https://github.com/example/archived2"}
	p2Body, _ := json.Marshal(p2)
	p2Req := httptest.NewRequest("POST", "/projects", bytes.NewReader(p2Body))
	p2Req.Header.Set("Authorization", authHeader)
	p2Req.Header.Set("Content-Type", "application/json")
	p2W := httptest.NewRecorder()
	server.mux.ServeHTTP(p2W, p2Req)
	var archivedProject store.Project
	json.NewDecoder(p2W.Body).Decode(&archivedProject)

	// Archive the second project
	archiveReq := httptest.NewRequest("POST", "/projects/"+archivedProject.ID+"/archive", nil)
	archiveReq.Header.Set("Authorization", authHeader)
	archiveW := httptest.NewRecorder()
	server.mux.ServeHTTP(archiveW, archiveReq)

	// List projects WITH include_archived=true
	listReq := httptest.NewRequest("GET", "/projects?include_archived=true", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	var projects []store.Project
	json.NewDecoder(listW.Body).Decode(&projects)

	// Verify archived_at is set on archived project (filter by name to handle test isolation)
	hasActiveProject := false
	hasArchivedProject := false
	for _, p := range projects {
		if p.ID == activeProject.ID && p.Name == "active-project-2" {
			hasActiveProject = true
			if p.ArchivedAt != nil {
				t.Errorf("expected ArchivedAt to be nil for active project, got %v", p.ArchivedAt)
			}
		}
		if p.ID == archivedProject.ID && p.Name == "archived-project-2" {
			hasArchivedProject = true
			if p.ArchivedAt == nil {
				t.Error("expected ArchivedAt to be set for archived project, got nil")
			}
		}
	}

	if !hasActiveProject {
		t.Error("active project not found in list")
	}
	if !hasArchivedProject {
		t.Error("archived project not found in list")
	}
}

// TestListTasksWithFieldsSummary verifies fields=summary omits spec and result.
func TestListTasksWithFieldsSummary(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task with spec and result
	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "This is the spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", createW.Code)
	}

	// List with fields=summary
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?fields=summary", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []map[string]interface{}
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	task := tasks[0]
	// Verify spec and result are absent
	if _, hasSpec := task["spec"]; hasSpec {
		t.Error("spec should be absent in summary response")
	}
	if _, hasResult := task["result"]; hasResult {
		t.Error("result should be absent in summary response")
	}

	// Verify other fields are present
	if _, hasID := task["id"]; !hasID {
		t.Error("id should be present in summary response")
	}
	if _, hasTitle := task["title"]; !hasTitle {
		t.Error("title should be present in summary response")
	}
	if _, hasState := task["state"]; !hasState {
		t.Error("state should be present in summary response")
	}
}

// TestListTasksWithoutFieldsSummary verifies spec and result are present without fields=summary.
func TestListTasksWithoutFieldsSummary(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task with spec
	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "This is the spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	// List without fields parameter
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []map[string]interface{}
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	task := tasks[0]
	// Verify spec is present
	if _, hasSpec := task["spec"]; !hasSpec {
		t.Error("spec should be present in normal task response")
	}
	if spec, ok := task["spec"].(string); !ok || spec != "This is the spec" {
		t.Errorf("expected spec 'This is the spec', got %v", task["spec"])
	}
	// Verify result field is present (even if nil)
	if _, hasResult := task["result"]; !hasResult {
		t.Error("result field should be present in normal task response")
	}
}

// TestListTasksFieldsSummaryComposesWithFilters verifies fields=summary composes with state= and kind=.
func TestListTasksFieldsSummaryComposesWithFilters(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create tasks
	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "Spec 1",
			DocumentID: docID,
		},
		{
			Title:      "Task 2",
			Spec:       "Spec 2",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	if createW.Code != http.StatusCreated {
		t.Fatalf("expected status 201, got %d", createW.Code)
	}

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)

	// Manually convert one task to review kind
	conn := server.store.Conn()
	reviewTaskID := createdTasks[1].ID
	_, err := conn.ExecContext(context.Background(), "UPDATE task SET kind = 'review' WHERE id = ?", reviewTaskID)
	if err != nil {
		t.Fatalf("failed to set task to review kind: %v", err)
	}

	// Test 1: List with fields=summary and kind=implement filter
	listReq := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?fields=summary&kind=implement", nil)
	listReq.Header.Set("Authorization", authHeader)
	listW := httptest.NewRecorder()
	server.mux.ServeHTTP(listW, listReq)

	if listW.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW.Code)
	}

	var tasks []map[string]interface{}
	if err := json.NewDecoder(listW.Body).Decode(&tasks); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	if len(tasks) != 1 {
		t.Errorf("expected 1 task with kind=implement, got %d", len(tasks))
	}

	task := tasks[0]
	// Verify spec and result are absent
	if _, hasSpec := task["spec"]; hasSpec {
		t.Error("spec should be absent in summary response")
	}
	if _, hasResult := task["result"]; hasResult {
		t.Error("result should be absent in summary response")
	}

	// Verify kind filter was applied
	if kind, ok := task["kind"]; !ok || kind != "implement" {
		t.Errorf("expected kind 'implement', got %v", kind)
	}

	// Test 2: List with fields=summary and state=backlog filter
	listReq2 := httptest.NewRequest("GET", "/projects/"+projectID+"/tasks?fields=summary&state=backlog", nil)
	listReq2.Header.Set("Authorization", authHeader)
	listW2 := httptest.NewRecorder()
	server.mux.ServeHTTP(listW2, listReq2)

	if listW2.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", listW2.Code)
	}

	var tasks2 []map[string]interface{}
	if err := json.NewDecoder(listW2.Body).Decode(&tasks2); err != nil {
		t.Fatalf("failed to decode tasks: %v", err)
	}

	// Both tasks should be in backlog state (initial state after creation)
	if len(tasks2) != 2 {
		t.Errorf("expected 2 tasks with state=backlog, got %d", len(tasks2))
	}

	for i, task2 := range tasks2 {
		// Verify spec and result are absent
		if _, hasSpec := task2["spec"]; hasSpec {
			t.Error("spec should be absent in summary response with state= filter")
		}
		if _, hasResult := task2["result"]; hasResult {
			t.Error("result should be absent in summary response with state= filter")
		}

		// Verify state filter was applied
		if state, ok := task2["state"]; !ok || state != "backlog" {
			t.Errorf("task %d: expected state 'backlog', got %v", i, state)
		}
	}
}

// TestUpdateEscalationSucceedsWithTrue verifies that updating escalate to true succeeds.
func TestUpdateEscalationSucceedsWithTrue(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task with escalate=false
	escalateFalse := false
	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
			Model:      "haiku",
			Escalate:   &escalateFalse,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Update escalate to true
	updatePayload := map[string]interface{}{
		"escalate": true,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var updatedTask store.Task
	if err := json.NewDecoder(updateW.Body).Decode(&updatedTask); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if updatedTask.ID != taskID {
		t.Errorf("expected task id %q, got %q", taskID, updatedTask.ID)
	}
	if updatedTask.Escalate != true {
		t.Errorf("expected escalate=true, got %v", updatedTask.Escalate)
	}
}

// TestUpdateEscalationSucceedsWithFalse verifies that updating escalate to false succeeds.
func TestUpdateEscalationSucceedsWithFalse(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	// Create a task with escalate=true (default)
	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
			Model:      "haiku",
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Update escalate to false
	updatePayload := map[string]interface{}{
		"escalate": false,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var updatedTask store.Task
	if err := json.NewDecoder(updateW.Body).Decode(&updatedTask); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if updatedTask.ID != taskID {
		t.Errorf("expected task id %q, got %q", taskID, updatedTask.ID)
	}
	if updatedTask.Escalate != false {
		t.Errorf("expected escalate=false, got %v", updatedTask.Escalate)
	}
}

// TestUpdateEscalationRequiresAuth verifies that the escalation endpoint requires auth.
func TestUpdateEscalationRequiresAuth(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Try to update without auth
	updatePayload := map[string]interface{}{
		"escalate": true,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", updateW.Code)
	}
}

// escalationBaseline captures the pre-request state of a task so a rejected escalation PATCH can
// be proven to have mutated nothing.
type escalationBaseline struct {
	escalate bool
	state    string
	events   []store.Event
}

// captureEscalationBaseline records a task's current escalate flag, state, and full event history
// through the API so it can be compared after an invalid request.
func captureEscalationBaseline(t *testing.T, server *Server, authHeader, taskID string) escalationBaseline {
	t.Helper()
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)
	var task store.TaskWithDepsAndLinks
	if err := json.NewDecoder(getW.Body).Decode(&task); err != nil {
		t.Fatalf("failed to decode baseline task: %v", err)
	}

	eventsReq := httptest.NewRequest("GET", "/tasks/"+taskID+"/events", nil)
	eventsReq.Header.Set("Authorization", authHeader)
	eventsW := httptest.NewRecorder()
	server.mux.ServeHTTP(eventsW, eventsReq)
	var events []store.Event
	if err := json.NewDecoder(eventsW.Body).Decode(&events); err != nil {
		t.Fatalf("failed to decode baseline events: %v", err)
	}

	return escalationBaseline{escalate: task.Escalate, state: task.State, events: events}
}

// assertEscalationUnchanged verifies that a rejected escalation PATCH did not mutate the task:
// the escalate flag, state, and prior event history must all be identical to the captured baseline.
func assertEscalationUnchanged(t *testing.T, server *Server, authHeader, taskID string, baseline escalationBaseline) {
	t.Helper()
	getReq := httptest.NewRequest("GET", "/tasks/"+taskID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)
	var after store.TaskWithDepsAndLinks
	if err := json.NewDecoder(getW.Body).Decode(&after); err != nil {
		t.Fatalf("failed to decode task after rejected request: %v", err)
	}
	if after.Escalate != baseline.escalate {
		t.Errorf("escalate mutated by rejected request: want %v, got %v", baseline.escalate, after.Escalate)
	}
	if after.State != baseline.state {
		t.Errorf("state mutated by rejected request: want %q, got %q", baseline.state, after.State)
	}

	eventsReq := httptest.NewRequest("GET", "/tasks/"+taskID+"/events", nil)
	eventsReq.Header.Set("Authorization", authHeader)
	eventsW := httptest.NewRecorder()
	server.mux.ServeHTTP(eventsW, eventsReq)
	var eventsAfter []store.Event
	if err := json.NewDecoder(eventsW.Body).Decode(&eventsAfter); err != nil {
		t.Fatalf("failed to decode events after rejected request: %v", err)
	}
	if len(eventsAfter) != len(baseline.events) {
		t.Errorf("event history changed by rejected request: want %d events, got %d", len(baseline.events), len(eventsAfter))
	}
	for i, want := range baseline.events {
		if i >= len(eventsAfter) {
			break
		}
		if got := eventsAfter[i]; got.ID != want.ID || got.Kind != want.Kind || got.Actor != want.Actor {
			t.Errorf("prior event %d changed by rejected request: want %+v, got %+v", i, want, got)
		}
	}
}

// TestUpdateEscalationMissingFieldReturns400 verifies that missing escalate field returns 400.
func TestUpdateEscalationMissingFieldReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	baseline := captureEscalationBaseline(t, server, authHeader, taskID)

	// Try to update with empty payload
	updatePayload := map[string]interface{}{}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var errResp map[string]interface{}
	json.NewDecoder(updateW.Body).Decode(&errResp)
	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error field is not a map: %v", errResp)
	}
	if code, ok := errObj["code"]; !ok || code != "MISSING_FIELD" {
		t.Errorf("expected error code MISSING_FIELD, got %v", code)
	}

	// The rejected request must not have mutated the task.
	assertEscalationUnchanged(t, server, authHeader, taskID, baseline)
}

// TestUpdateEscalationNullFieldReturns400 verifies that null escalate field returns 400.
func TestUpdateEscalationNullFieldReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	baseline := captureEscalationBaseline(t, server, authHeader, taskID)

	// Try to update with null escalate
	updatePayload := map[string]interface{}{
		"escalate": nil,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var errResp map[string]interface{}
	json.NewDecoder(updateW.Body).Decode(&errResp)
	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error field is not a map: %v", errResp)
	}
	if code, ok := errObj["code"]; !ok || code != "NULL_FIELD" {
		t.Errorf("expected error code NULL_FIELD, got %v", code)
	}

	// The rejected request must not have mutated the task.
	assertEscalationUnchanged(t, server, authHeader, taskID, baseline)
}

// TestUpdateEscalationStringFieldReturns400 verifies that string escalate field returns 400.
func TestUpdateEscalationStringFieldReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	baseline := captureEscalationBaseline(t, server, authHeader, taskID)

	// Try to update with string escalate
	updateBody := []byte(`{"escalate": "true"}`)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var errResp map[string]interface{}
	json.NewDecoder(updateW.Body).Decode(&errResp)
	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error field is not a map: %v", errResp)
	}
	if code, ok := errObj["code"]; !ok || code != "INVALID_FIELD_TYPE" {
		t.Errorf("expected error code INVALID_FIELD_TYPE, got %v", code)
	}

	// The rejected request must not have mutated the task.
	assertEscalationUnchanged(t, server, authHeader, taskID, baseline)
}

// TestUpdateEscalationUnknownFieldReturns400 verifies that unknown fields return 400.
func TestUpdateEscalationUnknownFieldReturns400(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	baseline := captureEscalationBaseline(t, server, authHeader, taskID)

	// Try to update with unknown field
	updateBody := []byte(`{"escalate": true, "unknown_field": "value"}`)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var errResp map[string]interface{}
	json.NewDecoder(updateW.Body).Decode(&errResp)
	errObj, ok := errResp["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("error field is not a map: %v", errResp)
	}
	if code, ok := errObj["code"]; !ok || code != "UNKNOWN_FIELD" {
		t.Errorf("expected error code UNKNOWN_FIELD, got %v", code)
	}

	// The rejected request must not have mutated the task.
	assertEscalationUnchanged(t, server, authHeader, taskID, baseline)
}

// TestUpdateEscalationMissingTaskReturns404 verifies that missing task returns 404.
func TestUpdateEscalationMissingTaskReturns404(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"

	updatePayload := map[string]interface{}{
		"escalate": true,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/non-existent-id/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", updateW.Code)
	}
}

// TestUpdateEscalationTerminalTaskReturns409 verifies that terminal tasks cannot be updated.
func TestUpdateEscalationTerminalTaskReturns409(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Manually set task to a terminal state (done) to test rejection
	_, err := server.store.Conn().ExecContext(context.Background(),
		"UPDATE task SET state = ? WHERE id = ?", "done", taskID)
	if err != nil {
		t.Fatalf("failed to set task to done: %v", err)
	}

	// Try to update escalate on terminal task
	updatePayload := map[string]interface{}{
		"escalate": false,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusConflict {
		t.Fatalf("expected status 409, got %d; body: %s", updateW.Code, updateW.Body.String())
	}
}

// TestUpdateEscalationBlockedTaskSucceeds verifies that escalate can be changed on a task in
// state=blocked (reached via /transition, not the unrelated held flag set by /hold) and that the
// update does not unblock or otherwise resume the task.
func TestUpdateEscalationBlockedTaskSucceeds(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Promote and claim so the task is in an active state that can transition to blocked.
	promoteReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	claimPayload := map[string]string{"agent_id": "test-agent", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Transition to state=blocked via the transition API.
	transitionPayload := map[string]interface{}{
		"to":   "blocked",
		"note": "Blocked on external dependency",
	}
	transitionBody, _ := json.Marshal(transitionPayload)
	transitionReq := httptest.NewRequest("POST", "/tasks/"+taskID+"/transition", bytes.NewReader(transitionBody))
	transitionReq.Header.Set("Authorization", authHeader)
	transitionReq.Header.Set("Content-Type", "application/json")
	transitionW := httptest.NewRecorder()
	server.mux.ServeHTTP(transitionW, transitionReq)

	var blockedTask store.Task
	if err := json.NewDecoder(transitionW.Body).Decode(&blockedTask); err != nil {
		t.Fatalf("failed to decode transition response: %v", err)
	}
	if blockedTask.State != "blocked" {
		t.Fatalf("precondition failed: expected state 'blocked', got %q", blockedTask.State)
	}

	// Update escalate on the now-blocked task.
	updatePayload := map[string]interface{}{
		"escalate": false,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var updatedTask store.Task
	if err := json.NewDecoder(updateW.Body).Decode(&updatedTask); err != nil {
		t.Fatalf("failed to decode update response: %v", err)
	}
	if updatedTask.State != "blocked" {
		t.Errorf("expected state to remain 'blocked', got %q", updatedTask.State)
	}
	if updatedTask.Escalate != false {
		t.Errorf("expected escalate to be false, got %v", updatedTask.Escalate)
	}
}

// TestUpdateEscalationPreservesOtherFields verifies through the API that state, dependencies, and
// prior event history remain intact after changing the escalation policy.
func TestUpdateEscalationPreservesOtherFields(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Task 1",
			Spec:       "Task 1 spec",
			DocumentID: docID,
		},
		{
			Title:      "Task 2",
			Spec:       "Task 2 spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	task1ID := createdTasks[0].ID
	task2ID := createdTasks[1].ID

	// Set task2 to depend on task1
	depPayload := map[string]interface{}{
		"depends_on": []string{task1ID},
	}
	depBody, _ := json.Marshal(depPayload)
	depReq := httptest.NewRequest("PATCH", "/tasks/"+task2ID, bytes.NewReader(depBody))
	depReq.Header.Set("Authorization", authHeader)
	depReq.Header.Set("Content-Type", "application/json")
	depW := httptest.NewRecorder()
	server.mux.ServeHTTP(depW, depReq)

	// Record prior history on task2 by promoting and claiming it, which appends a "claim" event.
	promoteReq := httptest.NewRequest("POST", "/tasks/"+task2ID+"/promote", nil)
	promoteReq.Header.Set("Authorization", authHeader)
	promoteW := httptest.NewRecorder()
	server.mux.ServeHTTP(promoteW, promoteReq)

	claimPayload := map[string]string{"agent_id": "test-agent", "model": "haiku"}
	claimBody, _ := json.Marshal(claimPayload)
	claimReq := httptest.NewRequest("POST", "/tasks/"+task2ID+"/claim", bytes.NewReader(claimBody))
	claimReq.Header.Set("Authorization", authHeader)
	claimReq.Header.Set("Content-Type", "application/json")
	claimW := httptest.NewRecorder()
	server.mux.ServeHTTP(claimW, claimReq)

	// Get the original task (with deps) and its event history to compare against.
	getReq := httptest.NewRequest("GET", "/tasks/"+task2ID, nil)
	getReq.Header.Set("Authorization", authHeader)
	getW := httptest.NewRecorder()
	server.mux.ServeHTTP(getW, getReq)

	var originalTask store.TaskWithDepsAndLinks
	if err := json.NewDecoder(getW.Body).Decode(&originalTask); err != nil {
		t.Fatalf("failed to decode original task: %v", err)
	}
	if len(originalTask.DependsOn) != 1 || originalTask.DependsOn[0] != task1ID {
		t.Fatalf("precondition failed: expected task2 to depend on task1, got %v", originalTask.DependsOn)
	}

	eventsReq := httptest.NewRequest("GET", "/tasks/"+task2ID+"/events", nil)
	eventsReq.Header.Set("Authorization", authHeader)
	eventsW := httptest.NewRecorder()
	server.mux.ServeHTTP(eventsW, eventsReq)

	var originalEvents []store.Event
	if err := json.NewDecoder(eventsW.Body).Decode(&originalEvents); err != nil {
		t.Fatalf("failed to decode original events: %v", err)
	}
	if len(originalEvents) == 0 {
		t.Fatalf("precondition failed: expected at least one prior event")
	}

	// Update escalate
	updatePayload := map[string]interface{}{
		"escalate": false,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+task2ID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	var updatedTask store.Task
	json.NewDecoder(updateW.Body).Decode(&updatedTask)

	// Verify state, model, and review round are preserved
	if updatedTask.State != originalTask.State {
		t.Errorf("expected state %q, got %q", originalTask.State, updatedTask.State)
	}
	if updatedTask.Model != originalTask.Model {
		t.Errorf("expected model %q, got %q", originalTask.Model, updatedTask.Model)
	}
	if updatedTask.ReviewRound != originalTask.ReviewRound {
		t.Errorf("expected review_round %d, got %d", originalTask.ReviewRound, updatedTask.ReviewRound)
	}
	if updatedTask.Title != originalTask.Title {
		t.Errorf("expected title %q, got %q", originalTask.Title, updatedTask.Title)
	}

	// Verify dependencies remain intact through the API.
	getAfterReq := httptest.NewRequest("GET", "/tasks/"+task2ID, nil)
	getAfterReq.Header.Set("Authorization", authHeader)
	getAfterW := httptest.NewRecorder()
	server.mux.ServeHTTP(getAfterW, getAfterReq)

	var taskAfter store.TaskWithDepsAndLinks
	if err := json.NewDecoder(getAfterW.Body).Decode(&taskAfter); err != nil {
		t.Fatalf("failed to decode task after update: %v", err)
	}
	if len(taskAfter.DependsOn) != 1 || taskAfter.DependsOn[0] != task1ID {
		t.Errorf("expected depends_on to remain [%q], got %v", task1ID, taskAfter.DependsOn)
	}

	// Verify prior event history remains intact (the escalation update may append its own
	// policy-change event, but every event that existed before it must still be present).
	eventsAfterReq := httptest.NewRequest("GET", "/tasks/"+task2ID+"/events", nil)
	eventsAfterReq.Header.Set("Authorization", authHeader)
	eventsAfterW := httptest.NewRecorder()
	server.mux.ServeHTTP(eventsAfterW, eventsAfterReq)

	var eventsAfter []store.Event
	if err := json.NewDecoder(eventsAfterW.Body).Decode(&eventsAfter); err != nil {
		t.Fatalf("failed to decode events after update: %v", err)
	}
	if len(eventsAfter) < len(originalEvents) {
		t.Fatalf("expected at least %d events after update, got %d", len(originalEvents), len(eventsAfter))
	}
	for i, want := range originalEvents {
		got := eventsAfter[i]
		if got.ID != want.ID || got.Kind != want.Kind || got.Actor != want.Actor {
			t.Errorf("prior event %d changed: want %+v, got %+v", i, want, got)
		}
	}
}

// TestUpdateEscalationIdempotent verifies that repeating the same value is idempotent.
func TestUpdateEscalationIdempotent(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	escalateFalse := false
	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
			Escalate:   &escalateFalse,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Update escalate to false (same value)
	updatePayload := map[string]interface{}{
		"escalate": false,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d; body: %s", updateW.Code, updateW.Body.String())
	}

	var updatedTask store.Task
	if err := json.NewDecoder(updateW.Body).Decode(&updatedTask); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if updatedTask.Escalate != false {
		t.Errorf("expected escalate=false, got %v", updatedTask.Escalate)
	}
}

// TestUpdateEscalationFullIDResolution verifies that full task ID works.
func TestUpdateEscalationFullIDResolution(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Update using full ID
	updatePayload := map[string]interface{}{
		"escalate": true,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskID+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", updateW.Code)
	}
}

// TestUpdateEscalationPrefixIDResolution verifies that unique prefix ID resolution works.
func TestUpdateEscalationPrefixIDResolution(t *testing.T) {
	server := setupTestServer(t, "test-token")
	authHeader := "Bearer test-token"
	projectID, docID := setupProjectAndDocument(t, server, authHeader)

	taskPayload := []store.TaskInput{
		{
			Title:      "Test Task",
			Spec:       "Test spec",
			DocumentID: docID,
		},
	}
	taskBody, _ := json.Marshal(taskPayload)
	createReq := httptest.NewRequest("POST", "/projects/"+projectID+"/tasks", bytes.NewReader(taskBody))
	createReq.Header.Set("Authorization", authHeader)
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	server.mux.ServeHTTP(createW, createReq)

	var createdTasks []store.Task
	json.NewDecoder(createW.Body).Decode(&createdTasks)
	taskID := createdTasks[0].ID

	// Get the first 8 characters as a unique prefix
	taskIDPrefix := taskID[:8]

	// Update using prefix ID
	updatePayload := map[string]interface{}{
		"escalate": true,
	}
	updateBody, _ := json.Marshal(updatePayload)
	updateReq := httptest.NewRequest("PATCH", "/tasks/"+taskIDPrefix+"/escalation", bytes.NewReader(updateBody))
	updateReq.Header.Set("Authorization", authHeader)
	updateReq.Header.Set("Content-Type", "application/json")
	updateW := httptest.NewRecorder()
	server.mux.ServeHTTP(updateW, updateReq)

	if updateW.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", updateW.Code)
	}
}

func setupTestServerWithPprof(t *testing.T, authToken string, pprofEnabled bool) *Server {
	s, err := store.Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test store: %v", err)
	}
	return New(s, authToken, 5*time.Minute, 5, nil, pprofEnabled)
}

// TestPprofDisabledReturns404 verifies GET /debug/pprof/ returns 404 when pprof is disabled.
func TestPprofDisabledReturns404(t *testing.T) {
	server := setupTestServerWithPprof(t, "test-token", false)
	authHeader := "Bearer test-token"

	req := httptest.NewRequest("GET", "/debug/pprof/", nil)
	req.Header.Set("Authorization", authHeader)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", w.Code)
	}
}

// TestPprofEnabledWithoutAuthReturns401 verifies GET /debug/pprof/ returns 401 without auth.
func TestPprofEnabledWithoutAuthReturns401(t *testing.T) {
	server := setupTestServerWithPprof(t, "test-token", true)

	req := httptest.NewRequest("GET", "/debug/pprof/", nil)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}
}

// TestPprofEnabledWithAuthReturns200 verifies GET /debug/pprof/ returns 200 with auth when enabled.
func TestPprofEnabledWithAuthReturns200(t *testing.T) {
	server := setupTestServerWithPprof(t, "test-token", true)
	authHeader := "Bearer test-token"

	req := httptest.NewRequest("GET", "/debug/pprof/", nil)
	req.Header.Set("Authorization", authHeader)
	w := httptest.NewRecorder()
	server.mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}
}
