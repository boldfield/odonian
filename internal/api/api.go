package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"strings"
	"time"

	"github.com/boldfield/odonian/internal/store"
)

// taskToSummary converts a Task to a summary representation, omitting Spec and Result.
func taskToSummary(task store.Task) map[string]interface{} {
	return map[string]interface{}{
		"id":               task.ID,
		"project_id":       task.ProjectID,
		"document_id":      task.DocumentID,
		"title":            task.Title,
		"state":            task.State,
		"assignee":         task.Assignee,
		"lease_expires_at": task.LeaseExpiresAt,
		"model":            task.Model,
		"kind":             task.Kind,
		"review_models":    task.ReviewModels,
		"review_round":     task.ReviewRound,
		"target_task_id":   task.TargetTaskID,
		"verdict":          task.Verdict,
		"agent_merge":      task.AgentMerge,
		"held":             task.Held,
		"escalate":         task.Escalate,
		"track":            task.Track,
		"created_at":       task.CreatedAt,
		"updated_at":       task.UpdatedAt,
		"archived_at":      task.ArchivedAt,
		"superseded_by":    task.SupersededBy,
	}
}

// Server wraps the HTTP server with its dependencies: store, auth token, and lease TTL.
type Server struct {
	mux                  *http.ServeMux
	store                store.Store
	authToken            string
	leaseTTL             time.Duration
	maxReviewRounds      int
	escalationThresholds map[string]int
}

// New creates a new API server with the given store, auth token, lease TTL, max review rounds,
// escalation thresholds, and whether pprof debug endpoints should be registered.
func New(s store.Store, authToken string, leaseTTL time.Duration, maxReviewRounds int, escalationThresholds map[string]int, pprofEnabled bool) *Server {
	mux := http.NewServeMux()
	server := &Server{
		mux:                  mux,
		store:                s,
		authToken:            authToken,
		leaseTTL:             leaseTTL,
		maxReviewRounds:      maxReviewRounds,
		escalationThresholds: escalationThresholds,
	}

	// Register handlers
	// GET /healthz is exempted from auth
	mux.HandleFunc("GET /healthz", server.handleHealthz)

	// Project endpoints (protected)
	mux.HandleFunc("POST /projects", server.authMiddleware(server.handleCreateProject))
	mux.HandleFunc("GET /projects", server.authMiddleware(server.handleListProjects))
	mux.HandleFunc("GET /projects/{id}", server.authMiddleware(server.handleGetProject))

	// Document endpoints (protected)
	mux.HandleFunc("POST /projects/{id}/documents", server.authMiddleware(server.handleCreateDocument))
	mux.HandleFunc("GET /projects/{id}/documents", server.authMiddleware(server.handleListDocuments))

	// Task endpoints (protected)
	mux.HandleFunc("POST /projects/{id}/tasks", server.authMiddleware(server.handleCreateTasks))
	mux.HandleFunc("GET /projects/{id}/tasks", server.authMiddleware(server.handleListTasks))
	mux.HandleFunc("GET /tasks/{id}", server.authMiddleware(server.handleGetTask))
	mux.HandleFunc("GET /tasks/{id}/events", server.authMiddleware(server.handleGetTaskEvents))
	mux.HandleFunc("POST /tasks/{id}/claim", server.authMiddleware(server.handleClaimTask))
	mux.HandleFunc("POST /tasks/{id}/heartbeat", server.authMiddleware(server.handleHeartbeat))
	mux.HandleFunc("POST /tasks/{id}/promote", server.authMiddleware(server.handlePromoteTask))
	mux.HandleFunc("POST /tasks/{id}/submit", server.authMiddleware(server.handleSubmit))
	mux.HandleFunc("POST /tasks/{id}/review", server.authMiddleware(server.handleReview))
	mux.HandleFunc("POST /tasks/{id}/transition", server.authMiddleware(server.handleTransition))
	mux.HandleFunc("POST /tasks/{id}/supersede", server.authMiddleware(server.handleSupersede))
	mux.HandleFunc("PATCH /tasks/{id}", server.authMiddleware(server.handleUpdateTask))
	mux.HandleFunc("PATCH /tasks/{id}/escalation", server.authMiddleware(server.handleUpdateEscalation))
	mux.HandleFunc("POST /tasks/{id}/hold", server.authMiddleware(server.handleHold))
	mux.HandleFunc("POST /tasks/{id}/release", server.authMiddleware(server.handleRelease))
	mux.HandleFunc("POST /tasks/{id}/archive", server.authMiddleware(server.handleArchiveTask))
	mux.HandleFunc("POST /tasks/{id}/unarchive", server.authMiddleware(server.handleUnarchiveTask))
	mux.HandleFunc("POST /projects/{id}/archive", server.authMiddleware(server.handleArchiveProject))
	mux.HandleFunc("POST /projects/{id}/unarchive", server.authMiddleware(server.handleUnarchiveProject))

	// Pprof endpoints (protected), registered only when ODONIAN_PPROF=true.
	if pprofEnabled {
		mux.HandleFunc("GET /debug/pprof/", server.authMiddleware(pprof.Index))
		mux.HandleFunc("GET /debug/pprof/cmdline", server.authMiddleware(pprof.Cmdline))
		mux.HandleFunc("GET /debug/pprof/profile", server.authMiddleware(pprof.Profile))
		mux.HandleFunc("GET /debug/pprof/symbol", server.authMiddleware(pprof.Symbol))
		mux.HandleFunc("POST /debug/pprof/symbol", server.authMiddleware(pprof.Symbol))
		mux.HandleFunc("GET /debug/pprof/trace", server.authMiddleware(pprof.Trace))
		mux.HandleFunc("GET /debug/pprof/{profile}", server.authMiddleware(func(w http.ResponseWriter, r *http.Request) {
			pprof.Handler(r.PathValue("profile")).ServeHTTP(w, r)
		}))
	}

	return server
}

// Handler returns the HTTP handler for the API server.
func (s *Server) Handler() http.Handler {
	return s.mux
}

// ListenAndServe starts the HTTP server on the given address.
func (s *Server) ListenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.mux)
}

// handleHealthz handles GET /healthz (no auth required).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// authMiddleware checks the Authorization header and returns a handler that requires auth.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			s.errorResponse(w, http.StatusUnauthorized, "MISSING_AUTH", "Missing Authorization header")
			return
		}

		// Extract bearer token
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) != 2 || parts[0] != "Bearer" {
			s.errorResponse(w, http.StatusUnauthorized, "INVALID_AUTH_FORMAT", "Authorization header must be 'Bearer <token>'")
			return
		}

		token := parts[1]
		// Constant-time compare to avoid leaking the token via timing.
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) != 1 {
			s.errorResponse(w, http.StatusUnauthorized, "INVALID_TOKEN", "Invalid authentication token")
			return
		}

		next(w, r)
	}
}

// decodeJSON decodes a JSON body and handles errors with appropriate responses.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, v interface{}) error {
	if r.Body == nil {
		return errors.New("empty body")
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "READ_ERROR", "Failed to read request body")
		return err
	}

	if err := json.Unmarshal(body, v); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "JSON_DECODE_ERROR", "Invalid JSON in request body")
		return err
	}

	return nil
}

// encodeJSON encodes a value as JSON with the given status code.
func (s *Server) encodeJSON(w http.ResponseWriter, statusCode int, v interface{}) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	return json.NewEncoder(w).Encode(v)
}

// errorResponse writes a consistent error response.
func (s *Server) errorResponse(w http.ResponseWriter, statusCode int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := map[string]interface{}{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

// errorResponseWithCandidates writes a conflict error whose payload also
// lists candidate ids, e.g. AMBIGUOUS_ID from an unresolved task-id prefix.
func (s *Server) errorResponseWithCandidates(w http.ResponseWriter, statusCode int, code, message string, candidates []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := map[string]interface{}{
		"error": map[string]interface{}{
			"code":       code,
			"message":    message,
			"candidates": candidates,
		},
	}
	json.NewEncoder(w).Encode(resp)
}

// resolveTaskID reads the {id} path value and resolves any unique task-id
// prefix (8-35 chars) to the full stored id so every /tasks/{id}/... route
// accepts a table-truncated id, not just GET /tasks/{id}. It writes the
// appropriate error response and returns ok=false on failure: 404 for a
// no-match or too-short prefix, and 409 AMBIGUOUS_ID (with candidate ids) when
// the prefix matches several tasks. A full 36-char id is returned unchanged.
func (s *Server) resolveTaskID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	resolved, err := s.store.ResolveTaskID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return "", false
	}
	var conflictErr *store.ConflictError
	if errors.As(err, &conflictErr) {
		s.errorResponseWithCandidates(w, http.StatusConflict, conflictErr.Code, conflictErr.Message, conflictErr.Candidates)
		return "", false
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "GET_ERROR", "Failed to resolve task id")
		return "", false
	}
	return resolved, true
}

// Mux returns the underlying http.ServeMux for testing or direct access.
func (s *Server) Mux() *http.ServeMux {
	return s.mux
}

// handleCreateProject handles POST /projects to create a new project.
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name string `json:"name"`
		Repo string `json:"repo"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Validate name is non-empty
	if payload.Name == "" {
		s.errorResponse(w, http.StatusBadRequest, "EMPTY_NAME", "Project name cannot be empty")
		return
	}

	// Create the project (repo may be empty string)
	project, err := s.store.CreateProject(r.Context(), payload.Name, payload.Repo)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "CREATE_ERROR", "Failed to create project")
		return
	}

	s.encodeJSON(w, http.StatusCreated, project)
}

// handleGetProject handles GET /projects/{id} to retrieve a project.
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	project, err := s.store.GetProject(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Project not found")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "GET_ERROR", "Failed to get project")
		return
	}

	s.encodeJSON(w, http.StatusOK, project)
}

// handleListProjects handles GET /projects to list projects with optional filters.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	filter := store.ProjectListFilter{}

	if claimable := r.URL.Query().Get("claimable"); claimable == "true" {
		filter.Claimable = true
	}

	if model := r.URL.Query().Get("model"); model != "" {
		filter.Model = &model
	}

	if kind := r.URL.Query().Get("kind"); kind != "" {
		if kind != "implement" && kind != "review" && kind != "merge" {
			s.errorResponse(w, http.StatusBadRequest, "INVALID_KIND", "kind must be 'implement', 'review', or 'merge'")
			return
		}
		filter.Kind = &kind
	}

	if includeArchived := r.URL.Query().Get("include_archived"); includeArchived == "true" {
		filter.IncludeArchived = true
	}

	if includeSuperseded := r.URL.Query().Get("include_superseded"); includeSuperseded == "true" {
		filter.IncludeSuperseded = true
	}

	projects, err := s.store.ListProjects(r.Context(), filter)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "LIST_ERROR", "Failed to list projects")
		return
	}

	// Ensure we return an empty array, not null
	if projects == nil {
		projects = make([]store.Project, 0)
	}

	s.encodeJSON(w, http.StatusOK, projects)
}

// handleCreateDocument handles POST /projects/{id}/documents to register a document.
func (s *Server) handleCreateDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var payload struct {
		Kind   string  `json:"kind"`
		Title  string  `json:"title"`
		Ref    string  `json:"ref"`
		Commit *string `json:"commit"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Validate kind is one of the allowed values
	if payload.Kind != "design" && payload.Kind != "feature_spec" {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_KIND", "kind must be 'design' or 'feature_spec'")
		return
	}

	// Create the document
	doc, err := s.store.CreateDocument(r.Context(), id, payload.Kind, payload.Title, payload.Ref, payload.Commit)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Project not found")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		s.errorResponse(w, http.StatusConflict, "CONFLICT", "A design document already exists for this project")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "CREATE_ERROR", "Failed to create document")
		return
	}

	s.encodeJSON(w, http.StatusCreated, doc)
}

// handleListDocuments handles GET /projects/{id}/documents to list documents.
func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// Parse optional kind query parameter
	var kind *string
	if kindQuery := r.URL.Query().Get("kind"); kindQuery != "" {
		kind = &kindQuery
	}

	// List documents
	docs, err := s.store.ListDocuments(r.Context(), id, kind)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "LIST_ERROR", "Failed to list documents")
		return
	}

	// Ensure we return an empty array, not null
	if docs == nil {
		docs = make([]store.Document, 0)
	}

	s.encodeJSON(w, http.StatusOK, docs)
}

// handleCreateTasks handles POST /projects/{id}/tasks to bulk-create tasks.
func (s *Server) handleCreateTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	var payload []store.TaskInput
	if err := s.decodeJSON(w, r, &payload); err != nil {
		return
	}

	tasks, err := s.store.CreateTasks(r.Context(), projectID, payload)
	if err != nil {
		// Client-input errors map to 400 with their specific code; everything else is 500.
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			s.errorResponse(w, http.StatusBadRequest, ve.Code, ve.Error())
			return
		}
		s.errorResponse(w, http.StatusInternalServerError, "CREATE_ERROR", "Failed to create tasks")
		return
	}

	s.encodeJSON(w, http.StatusCreated, tasks)
}

// handleGetTask handles GET /tasks/{id} to retrieve a task with dependencies and links.
func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	task, err := s.store.GetTask(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	var conflictErr *store.ConflictError
	if errors.As(err, &conflictErr) {
		s.errorResponseWithCandidates(w, http.StatusConflict, conflictErr.Code, conflictErr.Message, conflictErr.Candidates)
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "GET_ERROR", "Failed to get task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleGetTaskEvents handles GET /tasks/{id}/events to retrieve the task's event log.
func (s *Server) handleGetTaskEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	events, err := s.store.ListEvents(r.Context(), id)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "GET_ERROR", "Failed to get task events")
		return
	}

	// Ensure we return an empty array, not null
	if events == nil {
		events = make([]store.Event, 0)
	}

	s.encodeJSON(w, http.StatusOK, events)
}

// handleListTasks handles GET /projects/{id}/tasks with optional filters.
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	filter := store.TaskListFilter{}

	if state := r.URL.Query().Get("state"); state != "" {
		filter.State = &state
	}

	if assignee := r.URL.Query().Get("assignee"); assignee != "" {
		filter.Assignee = &assignee
	}

	if model := r.URL.Query().Get("model"); model != "" {
		filter.Model = &model
	}

	if kind := r.URL.Query().Get("kind"); kind != "" {
		if kind != "implement" && kind != "review" && kind != "merge" {
			s.errorResponse(w, http.StatusBadRequest, "INVALID_KIND", "kind must be 'implement', 'review', or 'merge'")
			return
		}
		filter.Kind = &kind
	}

	if claimable := r.URL.Query().Get("claimable"); claimable == "true" {
		filter.Claimable = true
	}

	if includeArchived := r.URL.Query().Get("include_archived"); includeArchived == "true" {
		filter.IncludeArchived = true
	}

	if includeSuperseded := r.URL.Query().Get("include_superseded"); includeSuperseded == "true" {
		filter.IncludeSuperseded = true
	}

	tasks, err := s.store.ListTasks(r.Context(), projectID, filter)
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "LIST_ERROR", "Failed to list tasks")
		return
	}

	// Ensure we return an empty array, not null
	if tasks == nil {
		tasks = make([]store.Task, 0)
	}

	fieldsSummary := r.URL.Query().Get("fields") == "summary"
	if fieldsSummary {
		summaries := make([]map[string]interface{}, len(tasks))
		for i, task := range tasks {
			summaries[i] = taskToSummary(task)
		}
		s.encodeJSON(w, http.StatusOK, summaries)
		return
	}

	s.encodeJSON(w, http.StatusOK, tasks)
}

// handleClaimTask handles POST /tasks/{id}/claim to claim a task as in_progress.
func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	var payload struct {
		AgentID string `json:"agent_id"`
		Model   string `json:"model"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Validate agent_id is non-empty
	if payload.AgentID == "" {
		s.errorResponse(w, http.StatusBadRequest, "EMPTY_AGENT_ID", "agent_id cannot be empty")
		return
	}

	// Validate model is non-empty
	if payload.Model == "" {
		s.errorResponse(w, http.StatusBadRequest, "EMPTY_MODEL", "model cannot be empty")
		return
	}

	// Claim the task
	task, err := s.store.ClaimTask(r.Context(), taskID, payload.AgentID, payload.Model, s.leaseTTL)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}

	// Check for ConflictError with specific code
	var conflictErr *store.ConflictError
	if errors.As(err, &conflictErr) {
		s.errorResponse(w, http.StatusConflict, conflictErr.Code, conflictErr.Message)
		return
	}

	if errors.Is(err, store.ErrConflict) {
		s.errorResponse(w, http.StatusConflict, "CONFLICT", "Task is not claimable")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "CLAIM_ERROR", "Failed to claim task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleHeartbeat handles POST /tasks/{id}/heartbeat to extend a task's lease.
func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	var payload struct {
		AgentID string `json:"agent_id"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Validate agent_id is non-empty
	if payload.AgentID == "" {
		s.errorResponse(w, http.StatusBadRequest, "EMPTY_AGENT_ID", "agent_id cannot be empty")
		return
	}

	// Heartbeat the task
	task, err := s.store.HeartbeatTask(r.Context(), taskID, payload.AgentID, s.leaseTTL)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		s.errorResponse(w, http.StatusConflict, "CONFLICT", "Task is not in_progress or not assigned to this agent")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "HEARTBEAT_ERROR", "Failed to heartbeat task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handlePromoteTask handles POST /tasks/{id}/promote to promote a task from backlog to ready.
func (s *Server) handlePromoteTask(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	// Promote the task
	task, err := s.store.PromoteTask(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		s.errorResponse(w, http.StatusConflict, "CONFLICT", "Task is not in backlog")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "PROMOTE_ERROR", "Failed to promote task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleSubmit handles POST /tasks/{id}/submit to transition a task from in_progress to review.
func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	// First pass: decode with lenient handling to catch findings type errors
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "READ_ERROR", "Failed to read request body")
		return
	}

	var rawPayload map[string]interface{}
	if err := json.Unmarshal(body, &rawPayload); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "JSON_DECODE_ERROR", "Invalid JSON in request body")
		return
	}

	// Validate findings structure if present
	if rawFindingsIface, ok := rawPayload["findings"]; ok && rawFindingsIface != nil {
		rawFindings, ok := rawFindingsIface.([]interface{})
		if !ok {
			s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", "findings: must be an array")
			return
		}
		// Validate each finding completely (type + semantic) before moving to the next
		seenIDs := make(map[string]bool)
		for i, rawFinding := range rawFindings {
			findingMap, ok := rawFinding.(map[string]interface{})
			if !ok {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d]: must be an object", i))
				return
			}

			// Extract and validate each field, reporting the first error found
			// Order: type checks first, then semantic checks (as JSON would be processed)

			// id: must be a string, non-empty, unique
			idVal, hasID := findingMap["id"]
			if !hasID {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: must be a string", i))
				return
			}
			id, isStr := idVal.(string)
			if !isStr {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: must be a string", i))
				return
			}
			if id == "" {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: must be non-empty", i))
				return
			}
			if seenIDs[id] {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].id: duplicate id %q", i, id))
				return
			}
			seenIDs[id] = true

			// severity: must be a string, P1/P2/P3
			severityVal, hasSeverity := findingMap["severity"]
			if !hasSeverity {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].severity: must be a string", i))
				return
			}
			severity, isStr := severityVal.(string)
			if !isStr {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].severity: must be a string", i))
				return
			}
			if severity != "P1" && severity != "P2" && severity != "P3" {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].severity: must be P1, P2, or P3", i))
				return
			}

			// file: must be a string, non-empty
			fileVal, hasFile := findingMap["file"]
			if !hasFile {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].file: must be a string", i))
				return
			}
			file, isStr := fileVal.(string)
			if !isStr {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].file: must be a string", i))
				return
			}
			if file == "" {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].file: must be non-empty", i))
				return
			}

			// line: must be a number, positive integer
			lineVal, hasLine := findingMap["line"]
			if !hasLine {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be an integer", i))
				return
			}
			num, isNum := lineVal.(float64)
			if !isNum {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be an integer", i))
				return
			}
			if num != float64(int(num)) {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be an integer", i))
				return
			}
			if num <= 0 || num > float64(int(^uint(0)>>1)) {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].line: must be positive integer", i))
				return
			}

			// summary: must be a string, non-empty
			summaryVal, hasSummary := findingMap["summary"]
			if !hasSummary {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].summary: must be a string", i))
				return
			}
			summary, isStr := summaryVal.(string)
			if !isStr {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].summary: must be a string", i))
				return
			}
			if summary == "" {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].summary: must be non-empty", i))
				return
			}

			// in_changed_text: must be a boolean (required)
			inChangedVal, hasInChanged := findingMap["in_changed_text"]
			if !hasInChanged {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].in_changed_text: must be a boolean", i))
				return
			}
			if _, isBool := inChangedVal.(bool); !isBool {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].in_changed_text: must be a boolean", i))
				return
			}

			// status: must be a string, new/still_open/resolved
			statusVal, hasStatus := findingMap["status"]
			if !hasStatus {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].status: must be a string", i))
				return
			}
			status, isStr := statusVal.(string)
			if !isStr {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].status: must be a string", i))
				return
			}
			if status != "new" && status != "still_open" && status != "resolved" {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].status: must be new, still_open, or resolved", i))
				return
			}

			// prior_id: optional string, required for still_open/resolved, must be absent for new
			priorIDVal, hasPriorID := findingMap["prior_id"]
			if hasPriorID && priorIDVal != nil {
				priorID, isStr := priorIDVal.(string)
				if !isStr {
					s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: must be a string", i))
					return
				}
				if status == "new" {
					s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: must be absent for status=new", i))
					return
				}
				if priorID == "" {
					s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: must be non-empty", i))
					return
				}
			} else if status == "still_open" || status == "resolved" {
				s.errorResponse(w, http.StatusBadRequest, "INVALID_FINDINGS", fmt.Sprintf("findings[%d].prior_id: required for status=%s", i, status))
				return
			}
		}
	}

	var payload struct {
		AgentID  string            `json:"agent_id"`
		Result   string            `json:"result"`
		Verdict  *string           `json:"verdict"`
		Links    []store.LinkInput `json:"links"`
		Findings *[]store.Finding  `json:"findings"`
	}

	if err := json.Unmarshal(body, &payload); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "JSON_DECODE_ERROR", "Invalid JSON in request body")
		return
	}

	// Validate agent_id is non-empty
	if payload.AgentID == "" {
		s.errorResponse(w, http.StatusBadRequest, "EMPTY_AGENT_ID", "agent_id cannot be empty")
		return
	}

	// Submit the task
	task, err := s.store.SubmitTask(r.Context(), taskID, payload.AgentID, payload.Result, payload.Verdict, payload.Links, payload.Findings, s.maxReviewRounds, s.escalationThresholds)
	if err != nil {
		// Check if it's a ValidationError (invalid link kind)
		var validationErr *store.ValidationError
		if errors.As(err, &validationErr) {
			s.errorResponse(w, http.StatusBadRequest, validationErr.Code, validationErr.Message)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			s.errorResponse(w, http.StatusConflict, "CONFLICT", "Task is not in_progress or not assigned to this agent")
			return
		}
		s.errorResponse(w, http.StatusInternalServerError, "SUBMIT_ERROR", "Failed to submit task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleReview handles POST /tasks/{id}/review to record a review verdict event.
func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	var payload struct {
		Actor   string  `json:"actor"`
		Verdict string  `json:"verdict"`
		Note    *string `json:"note"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Validate actor is non-empty
	if payload.Actor == "" {
		s.errorResponse(w, http.StatusBadRequest, "EMPTY_ACTOR", "actor cannot be empty")
		return
	}

	// Add the review
	event, err := s.store.AddReview(r.Context(), taskID, payload.Actor, payload.Verdict, payload.Note)
	if err != nil {
		// Check if it's a ValidationError
		var validationErr *store.ValidationError
		if errors.As(err, &validationErr) {
			s.errorResponse(w, http.StatusBadRequest, validationErr.Code, validationErr.Message)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			s.errorResponse(w, http.StatusConflict, "CONFLICT", "Task is not in review state")
			return
		}
		s.errorResponse(w, http.StatusInternalServerError, "REVIEW_ERROR", "Failed to add review")
		return
	}

	s.encodeJSON(w, http.StatusCreated, event)
}

// handleTransition handles POST /tasks/{id}/transition to move a task to a new state.
func (s *Server) handleTransition(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	var payload struct {
		To   string  `json:"to"`
		Note *string `json:"note"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Transition the task
	task, err := s.store.TransitionTask(r.Context(), taskID, payload.To, payload.Note)
	if err != nil {
		// Check if it's a ValidationError
		var validationErr *store.ValidationError
		if errors.As(err, &validationErr) {
			s.errorResponse(w, http.StatusBadRequest, validationErr.Code, validationErr.Message)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			s.errorResponse(w, http.StatusConflict, "CONFLICT", "Transition is not allowed from the current state")
			return
		}
		s.errorResponse(w, http.StatusInternalServerError, "TRANSITION_ERROR", "Failed to transition task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleSupersede handles POST /tasks/{id}/supersede to create a new task with the same spec.
func (s *Server) handleSupersede(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	var payload struct {
		Model *string `json:"model"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Supersede the task
	task, err := s.store.SupersedeTask(r.Context(), taskID, payload.Model)
	if err != nil {
		// Check if it's a ValidationError
		var validationErr *store.ValidationError
		if errors.As(err, &validationErr) {
			s.errorResponse(w, http.StatusBadRequest, validationErr.Code, validationErr.Message)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			s.errorResponse(w, http.StatusConflict, "CONFLICT", "Task cannot be superseded")
			return
		}
		s.errorResponse(w, http.StatusInternalServerError, "SUPERSEDE_ERROR", "Failed to supersede task")
		return
	}

	s.encodeJSON(w, http.StatusCreated, task)
}

// handleUpdateTask handles PATCH /tasks/{id} to update a task's dependencies.
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	var payload struct {
		DependsOn []string `json:"depends_on"`
	}

	if err := s.decodeJSON(w, r, &payload); err != nil {
		return // decodeJSON already wrote error response
	}

	// Update the task's dependencies
	task, err := s.store.UpdateTaskDependsOn(r.Context(), taskID, payload.DependsOn)
	if err != nil {
		// Check if it's a ValidationError
		var validationErr *store.ValidationError
		if errors.As(err, &validationErr) {
			s.errorResponse(w, http.StatusBadRequest, validationErr.Code, validationErr.Message)
			return
		}
		// Check for ConflictError with specific code
		var conflictErr *store.ConflictError
		if errors.As(err, &conflictErr) {
			s.errorResponse(w, http.StatusConflict, conflictErr.Code, conflictErr.Message)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			s.errorResponse(w, http.StatusConflict, "CONFLICT", "Failed to update dependencies")
			return
		}
		s.errorResponse(w, http.StatusInternalServerError, "UPDATE_ERROR", "Failed to update task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleUpdateEscalation handles PATCH /tasks/{id}/escalation to update a task's escalation policy.
func (s *Server) handleUpdateEscalation(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	// Decode the JSON payload with strict validation
	if r.Body == nil {
		s.errorResponse(w, http.StatusBadRequest, "READ_ERROR", "Failed to read request body")
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.errorResponse(w, http.StatusBadRequest, "READ_ERROR", "Failed to read request body")
		return
	}

	// Parse as a map to check for unknown fields and validate structure
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		s.errorResponse(w, http.StatusBadRequest, "JSON_DECODE_ERROR", "Invalid JSON in request body")
		return
	}

	// Check for unknown fields
	for key := range payload {
		if key != "escalate" {
			s.errorResponse(w, http.StatusBadRequest, "UNKNOWN_FIELD", "Unknown field: "+key)
			return
		}
	}

	// Check that escalate field exists
	escalateValue, ok := payload["escalate"]
	if !ok {
		s.errorResponse(w, http.StatusBadRequest, "MISSING_FIELD", "Required field 'escalate' is missing")
		return
	}

	// Check that escalate is not null
	if escalateValue == nil {
		s.errorResponse(w, http.StatusBadRequest, "NULL_FIELD", "Field 'escalate' cannot be null")
		return
	}

	// Check that escalate is a boolean
	escalate, ok := escalateValue.(bool)
	if !ok {
		s.errorResponse(w, http.StatusBadRequest, "INVALID_FIELD_TYPE", "Field 'escalate' must be a boolean")
		return
	}

	// Update the task's escalation policy
	task, err := s.store.UpdateTaskEscalate(r.Context(), taskID, escalate)
	if err != nil {
		// Check if it's a ValidationError
		var validationErr *store.ValidationError
		if errors.As(err, &validationErr) {
			s.errorResponse(w, http.StatusBadRequest, validationErr.Code, validationErr.Message)
			return
		}
		// Check for ConflictError with specific code
		var conflictErr *store.ConflictError
		if errors.As(err, &conflictErr) {
			s.errorResponse(w, http.StatusConflict, conflictErr.Code, conflictErr.Message)
			return
		}
		if errors.Is(err, store.ErrNotFound) {
			s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
			return
		}
		if errors.Is(err, store.ErrConflict) {
			s.errorResponse(w, http.StatusConflict, "CONFLICT", "Failed to update escalation")
			return
		}
		s.errorResponse(w, http.StatusInternalServerError, "UPDATE_ERROR", "Failed to update task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleArchiveTask handles POST /tasks/{id}/archive to soft-archive a task.
func (s *Server) handleArchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	task, err := s.store.ArchiveTask(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "ARCHIVE_ERROR", "Failed to archive task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleUnarchiveTask handles POST /tasks/{id}/unarchive to restore an archived task.
func (s *Server) handleUnarchiveTask(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	task, err := s.store.UnarchiveTask(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "UNARCHIVE_ERROR", "Failed to unarchive task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleArchiveProject handles POST /projects/{id}/archive to soft-archive a project.
func (s *Server) handleArchiveProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	project, err := s.store.ArchiveProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Project not found")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "ARCHIVE_ERROR", "Failed to archive project")
		return
	}

	s.encodeJSON(w, http.StatusOK, project)
}

// handleUnarchiveProject handles POST /projects/{id}/unarchive to restore an archived project.
func (s *Server) handleUnarchiveProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")

	project, err := s.store.UnarchiveProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Project not found")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "UNARCHIVE_ERROR", "Failed to unarchive project")
		return
	}

	s.encodeJSON(w, http.StatusOK, project)
}

// handleHold handles POST /tasks/{id}/hold to pin a task out of automated flow.
func (s *Server) handleHold(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	task, err := s.store.HoldTask(r.Context(), taskID)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "HOLD_ERROR", "Failed to hold task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}

// handleRelease handles POST /tasks/{id}/release to restore normal automated flow.
func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	taskID, ok := s.resolveTaskID(w, r)
	if !ok {
		return
	}

	task, err := s.store.ReleaseTask(r.Context(), taskID, s.maxReviewRounds, s.escalationThresholds)
	if errors.Is(err, store.ErrNotFound) {
		s.errorResponse(w, http.StatusNotFound, "NOT_FOUND", "Task not found")
		return
	}
	if err != nil {
		s.errorResponse(w, http.StatusInternalServerError, "RELEASE_ERROR", "Failed to release task")
		return
	}

	s.encodeJSON(w, http.StatusOK, task)
}
