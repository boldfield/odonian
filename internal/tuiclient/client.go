package tuiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var ErrAlreadyClaimed = errors.New("task already claimed")

// Client is the interface for TUI interactions with the Odonian API.
type Client interface {
	ListProjects(ctx context.Context, options ...ProjectListOption) ([]Project, error)
	GetProject(ctx context.Context, id string) (Project, error)
	ListTasks(ctx context.Context, projectID string, options ...TaskListOption) ([]Task, error)
	GetTask(ctx context.Context, id string) (TaskDetail, error)
	ListEvents(ctx context.Context, taskID string) ([]Event, error)
	ListDocuments(ctx context.Context, projectID string) ([]Document, error)
	PromoteTask(ctx context.Context, id string) error
	ClaimTask(ctx context.Context, id, agentID, model string) error
	ReviewTask(ctx context.Context, id, actor, verdict string, note *string) error
	TransitionTask(ctx context.Context, id, to string, note *string) error
	HeartbeatTask(ctx context.Context, id, agentID string) error
	SubmitTask(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput) error
	HoldTask(ctx context.Context, id string) error
	ReleaseTask(ctx context.Context, id string) error
	ArchiveTask(ctx context.Context, id string) error
	ArchiveProject(ctx context.Context, id string) error
}

// Response structs for the TUI client (distinct from internal/store)

type Project struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	CreatedAt string `json:"created_at"`
}

type Task struct {
	ID             string  `json:"id"`
	ProjectID      string  `json:"project_id"`
	DocumentID     string  `json:"document_id"`
	Title          string  `json:"title"`
	Spec           string  `json:"spec"`
	State          string  `json:"state"`
	Kind           string  `json:"kind"`
	Model          string  `json:"model"`
	Track          string  `json:"track"`
	Assignee       *string `json:"assignee"`
	LeaseExpiresAt *string `json:"lease_expires_at"`
	Result         *string `json:"result"`
	Held           bool    `json:"held"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type TaskDetail struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"project_id"`
	DocumentID     string     `json:"document_id"`
	Title          string     `json:"title"`
	Spec           string     `json:"spec"`
	State          string     `json:"state"`
	Model          string     `json:"model"`
	Kind           string     `json:"kind"`
	Track          string     `json:"track"`
	Assignee       *string    `json:"assignee"`
	LeaseExpiresAt *string    `json:"lease_expires_at"`
	Result         *string    `json:"result"`
	Held           bool       `json:"held"`
	ReviewRound    int        `json:"review_round"`
	TargetTaskID   *string    `json:"target_task_id"`
	AgentMerge     bool       `json:"agent_merge"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
	DependsOn      []string   `json:"depends_on"`
	Links          []TaskLink `json:"links"`
}

type TaskLink struct {
	ID     string `json:"id"`
	TaskID string `json:"task_id"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

type LinkInput struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

type Document struct {
	ID        string  `json:"id"`
	ProjectID string  `json:"project_id"`
	Kind      string  `json:"kind"`
	Title     string  `json:"title"`
	Ref       string  `json:"ref"`
	Commit    *string `json:"commit"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type Event struct {
	ID        string  `json:"id"`
	TaskID    string  `json:"task_id"`
	Actor     string  `json:"actor"`
	Kind      string  `json:"kind"`
	Verdict   *string `json:"verdict"`
	Note      *string `json:"note"`
	CreatedAt string  `json:"created_at"`
}

// HTTPClient implements the Client interface.
type HTTPClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewHTTPClient creates a new HTTP client for the Odonian API.
func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// APIError is returned by do() for non-2xx responses. It carries the HTTP status code
// and the server's structured error code and message (when available). Callers can use
// errors.As to inspect the status code and take action — for example, detecting a 409
// conflict without string-matching on the error message.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != "" || e.Message != "" {
		return fmt.Sprintf("server error (%s): %s", e.Code, e.Message)
	}
	return fmt.Sprintf("unexpected status %d", e.StatusCode)
}

// errorResponse represents the structured error response from the server.
type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// do performs an HTTP request with bearer token authentication.
// On non-2xx status, it reads the response body, decodes the structured error if possible,
// and returns an *APIError carrying the HTTP status code plus server code/message.
func (c *HTTPClient) do(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	url := c.baseURL + path
	var req *http.Request
	var err error

	if body != nil {
		jsonBody, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to marshal request body: %w", marshalErr)
		}
		req, err = http.NewRequestWithContext(ctx, method, url, bytes.NewReader(jsonBody))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return resp, err
	}

	// On non-2xx status, read the body and return a typed *APIError so callers can
	// inspect StatusCode directly (e.g. via errors.As) without string-matching.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		apiErr := &APIError{StatusCode: resp.StatusCode}

		// Try to decode the structured error envelope; fall back to status-only if we can't.
		var errResp errorResponse
		if readErr == nil && len(bodyBytes) > 0 {
			if unmarshalErr := json.Unmarshal(bodyBytes, &errResp); unmarshalErr == nil && errResp.Error.Message != "" {
				apiErr.Code = errResp.Error.Code
				apiErr.Message = errResp.Error.Message
			}
		}

		return nil, apiErr
	}

	return resp, nil
}

// ProjectListOptions holds optional filters for ListProjects.
type ProjectListOptions struct {
	Model     string
	Kind      string
	Claimable bool
}

// ProjectListOption is a functional option for ListProjects.
type ProjectListOption func(*ProjectListOptions)

// WithProjectModel sets the model filter for projects.
func WithProjectModel(model string) ProjectListOption {
	return func(opts *ProjectListOptions) {
		opts.Model = model
	}
}

// WithProjectKind sets the kind filter for projects.
func WithProjectKind(kind string) ProjectListOption {
	return func(opts *ProjectListOptions) {
		opts.Kind = kind
	}
}

// WithProjectClaimable sets the claimable filter for projects.
func WithProjectClaimable(claimable bool) ProjectListOption {
	return func(opts *ProjectListOptions) {
		opts.Claimable = claimable
	}
}

// ListProjects fetches all projects, optionally filtered by model, kind, and claimable status.
func (c *HTTPClient) ListProjects(ctx context.Context, options ...ProjectListOption) ([]Project, error) {
	opts := &ProjectListOptions{}
	for _, opt := range options {
		opt(opts)
	}

	path := "/projects"
	if opts.Model != "" || opts.Kind != "" || opts.Claimable {
		var params []string
		if opts.Model != "" {
			params = append(params, fmt.Sprintf("model=%s", opts.Model))
		}
		if opts.Kind != "" {
			params = append(params, fmt.Sprintf("kind=%s", opts.Kind))
		}
		if opts.Claimable {
			params = append(params, "claimable=true")
		}
		if len(params) > 0 {
			path += "?" + join(params, "&")
		}
	}

	resp, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var projects []Project
	if err := json.NewDecoder(resp.Body).Decode(&projects); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return projects, nil
}

// GetProject fetches a single project by ID.
func (c *HTTPClient) GetProject(ctx context.Context, id string) (Project, error) {
	resp, err := c.do(ctx, "GET", fmt.Sprintf("/projects/%s", id), nil)
	if err != nil {
		return Project{}, err
	}
	defer resp.Body.Close()

	var project Project
	if err := json.NewDecoder(resp.Body).Decode(&project); err != nil {
		return Project{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return project, nil
}

// TaskListOptions holds optional filters for ListTasks.
type TaskListOptions struct {
	Model     string
	Kind      string
	Claimable bool
	State     string
}

// TaskListOption is a functional option for ListTasks.
type TaskListOption func(*TaskListOptions)

// WithModel sets the model filter.
func WithModel(model string) TaskListOption {
	return func(opts *TaskListOptions) {
		opts.Model = model
	}
}

// WithKind sets the kind filter.
func WithKind(kind string) TaskListOption {
	return func(opts *TaskListOptions) {
		opts.Kind = kind
	}
}

// WithClaimable sets the claimable filter.
func WithClaimable(claimable bool) TaskListOption {
	return func(opts *TaskListOptions) {
		opts.Claimable = claimable
	}
}

// WithState sets the state filter.
func WithState(state string) TaskListOption {
	return func(opts *TaskListOptions) {
		opts.State = state
	}
}

// ListTasks fetches tasks for a project, optionally filtered by model, kind, claimable status, and state.
func (c *HTTPClient) ListTasks(ctx context.Context, projectID string, options ...TaskListOption) ([]Task, error) {
	opts := &TaskListOptions{}
	for _, opt := range options {
		opt(opts)
	}

	path := fmt.Sprintf("/projects/%s/tasks", projectID)
	if opts.Model != "" || opts.Kind != "" || opts.Claimable || opts.State != "" {
		var params []string
		if opts.Model != "" {
			params = append(params, fmt.Sprintf("model=%s", opts.Model))
		}
		if opts.Kind != "" {
			params = append(params, fmt.Sprintf("kind=%s", opts.Kind))
		}
		if opts.Claimable {
			params = append(params, "claimable=true")
		}
		if opts.State != "" {
			params = append(params, fmt.Sprintf("state=%s", opts.State))
		}
		if len(params) > 0 {
			path += "?" + join(params, "&")
		}
	}

	resp, err := c.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var tasks []Task
	if err := json.NewDecoder(resp.Body).Decode(&tasks); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return tasks, nil
}

// join is a simple string joiner since we don't have strings.Join imported.
func join(ss []string, sep string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += sep + s
	}
	return result
}

// GetTask fetches a single task with full details including dependencies and links.
func (c *HTTPClient) GetTask(ctx context.Context, id string) (TaskDetail, error) {
	resp, err := c.do(ctx, "GET", fmt.Sprintf("/tasks/%s", id), nil)
	if err != nil {
		return TaskDetail{}, err
	}
	defer resp.Body.Close()

	var task TaskDetail
	if err := json.NewDecoder(resp.Body).Decode(&task); err != nil {
		return TaskDetail{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return task, nil
}

// ListEvents fetches all events for a task.
func (c *HTTPClient) ListEvents(ctx context.Context, taskID string) ([]Event, error) {
	resp, err := c.do(ctx, "GET", fmt.Sprintf("/tasks/%s/events", taskID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var events []Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return events, nil
}

// ListDocuments fetches all documents for a project.
func (c *HTTPClient) ListDocuments(ctx context.Context, projectID string) ([]Document, error) {
	resp, err := c.do(ctx, "GET", fmt.Sprintf("/projects/%s/documents", projectID), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var docs []Document
	if err := json.NewDecoder(resp.Body).Decode(&docs); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	return docs, nil
}

// PromoteTask promotes a backlog task to ready.
func (c *HTTPClient) PromoteTask(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/promote", id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// claimTaskRequest is the request body for ClaimTask.
type claimTaskRequest struct {
	AgentID string `json:"agent_id"`
	Model   string `json:"model"`
}

// ClaimTask claims a task as in_progress by the given agent and model.
// Returns ErrAlreadyClaimed if the task is already claimed by another worker (409 status).
func (c *HTTPClient) ClaimTask(ctx context.Context, id, agentID, model string) error {
	body := claimTaskRequest{
		AgentID: agentID,
		Model:   model,
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/claim", id), body)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 409 {
			return ErrAlreadyClaimed
		}
		return err
	}
	defer resp.Body.Close()

	return nil
}

// reviewTaskRequest is the request body for ReviewTask.
type reviewTaskRequest struct {
	Actor   string  `json:"actor"`
	Verdict string  `json:"verdict"`
	Note    *string `json:"note,omitempty"`
}

// ReviewTask posts a review verdict on a task.
func (c *HTTPClient) ReviewTask(ctx context.Context, id, actor, verdict string, note *string) error {
	body := reviewTaskRequest{
		Actor:   actor,
		Verdict: verdict,
		Note:    note,
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/review", id), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// transitionTaskRequest is the request body for TransitionTask.
type transitionTaskRequest struct {
	To   string  `json:"to"`
	Note *string `json:"note,omitempty"`
}

// TransitionTask moves a task to a new state.
func (c *HTTPClient) TransitionTask(ctx context.Context, id, to string, note *string) error {
	body := transitionTaskRequest{
		To:   to,
		Note: note,
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/transition", id), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// heartbeatTaskRequest is the request body for HeartbeatTask.
type heartbeatTaskRequest struct {
	AgentID string `json:"agent_id"`
}

// HeartbeatTask extends a task's lease.
func (c *HTTPClient) HeartbeatTask(ctx context.Context, id, agentID string) error {
	body := heartbeatTaskRequest{
		AgentID: agentID,
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/heartbeat", id), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// submitTaskRequest is the request body for SubmitTask.
type submitTaskRequest struct {
	AgentID string      `json:"agent_id"`
	Result  string      `json:"result"`
	Verdict *string     `json:"verdict,omitempty"`
	Links   []LinkInput `json:"links"`
}

// SubmitTask submits a task result with optional verdict and links.
func (c *HTTPClient) SubmitTask(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput) error {
	body := submitTaskRequest{
		AgentID: agentID,
		Result:  result,
		Verdict: verdict,
		Links:   links,
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/submit", id), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// ArchiveTask archives a task.
func (c *HTTPClient) ArchiveTask(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/archive", id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// ArchiveProject archives a project.
func (c *HTTPClient) ArchiveProject(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/projects/%s/archive", id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// HoldTask pins a task out of automated flow.
func (c *HTTPClient) HoldTask(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/hold", id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// ReleaseTask restores normal automated flow for a task.
func (c *HTTPClient) ReleaseTask(ctx context.Context, id string) error {
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/release", id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}
