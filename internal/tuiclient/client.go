package tuiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
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
	ClaimTask(ctx context.Context, id, agentID, model, requestID, accountID, workClass string) (*ResearchAdmission, error)
	ReviewTask(ctx context.Context, id, actor, verdict string, note *string) error
	TransitionTask(ctx context.Context, id, to string, note *string) error
	HeartbeatTask(ctx context.Context, id, agentID string) error
	SubmitTask(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput) error
	HoldTask(ctx context.Context, id string) error
	BeginLanding(ctx context.Context, id string, reviewRound int, commit, attempt string) error
	CancelLanding(ctx context.Context, id, attempt string) error
	CompleteLanding(ctx context.Context, id, attempt string, note *string) error
	ReleaseTask(ctx context.Context, id string) error
	ArchiveTask(ctx context.Context, id string) error
	ArchiveProject(ctx context.Context, id string) error
	GetResearchReviewerScorecards(ctx context.Context, projectID string) (ReviewerScorecards, error)
	GetResearchPolicy(ctx context.Context) (ResearchPolicy, error)
	GetResearchStatus(ctx context.Context) (ResearchStatus, error)
	RenewResearchPermit(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID string) (ResearchAttempt, error)
	FinalizeResearchPermit(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID, exitClass string, usageTokens *int64) (ResearchAttempt, error)
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
	Branch         string  `json:"branch"`
	Assignee       *string `json:"assignee"`
	LeaseExpiresAt *string `json:"lease_expires_at"`
	Result         *string `json:"result"`
	Held           bool    `json:"held"`
	CreatedAt      string  `json:"created_at"`
	UpdatedAt      string  `json:"updated_at"`
}

type TaskDetail struct {
	ID             string  `json:"id"`
	ProjectID      string  `json:"project_id"`
	DocumentID     string  `json:"document_id"`
	Title          string  `json:"title"`
	Spec           string  `json:"spec"`
	State          string  `json:"state"`
	Model          string  `json:"model"`
	Kind           string  `json:"kind"`
	Track          string  `json:"track"`
	Branch         string  `json:"branch"`
	LandingRound   *int    `json:"landing_round"`
	LandingCommit  *string `json:"landing_commit"`
	LandingAttempt *string `json:"landing_attempt"`
	// CurrentRoundLinks is the submission under review, as the server determines it.
	CurrentRoundLinks   []TaskLink           `json:"current_round_links"`
	Assignee            *string              `json:"assignee"`
	LeaseExpiresAt      *string              `json:"lease_expires_at"`
	Result              *string              `json:"result"`
	Held                bool                 `json:"held"`
	ReviewRound         int                  `json:"review_round"`
	TargetTaskID        *string              `json:"target_task_id"`
	AgentMerge          bool                 `json:"agent_merge"`
	CreatedAt           string               `json:"created_at"`
	UpdatedAt           string               `json:"updated_at"`
	DependsOn           []string             `json:"depends_on"`
	Links               []TaskLink           `json:"links"`
	SubmissionManifests []SubmissionManifest `json:"submission_manifests"`
	Continuation        *ContinuationInfo    `json:"continuation,omitempty"`
	FindingFollowUps    []FindingFollowUp    `json:"finding_follow_ups,omitempty"`
}

// ContinuationInfo mirrors store.ContinuationInfo: the planned and created research
// continuation children of a task, separate from review-finding follow-ups.
type ContinuationInfo struct {
	ManifestDigest   string                    `json:"manifest_digest,omitempty"`
	ProposedChildren []ProposedChild           `json:"proposed_children,omitempty"`
	CreatedChildren  []CreatedContinuationTask `json:"created_children,omitempty"`
	DeferredClaims   []DeferredClaim           `json:"deferred_claims,omitempty"`
	ExcludedClaims   []ExcludedClaim           `json:"excluded_claims,omitempty"`
	ActionItems      []ActionItem              `json:"action_items,omitempty"`
	ParentInfo       *ContinuationParent       `json:"parent_info,omitempty"`
}

type ContinuationDependency struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type ProposedChild struct {
	Key           string                   `json:"key"`
	Title         string                   `json:"title"`
	Track         string                   `json:"track"`
	Model         string                   `json:"model"`
	InitialState  string                   `json:"initial_state"`
	Dependencies  []ContinuationDependency `json:"dependencies"`
	Status        string                   `json:"status"`
	CreatedTaskID string                   `json:"created_task_id,omitempty"`
}

type CreatedContinuationTask struct {
	ID                 string   `json:"id"`
	Key                string   `json:"key,omitempty"`
	Title              string   `json:"title"`
	ParentTaskID       string   `json:"parent_task_id"`
	ManifestDigest     string   `json:"manifest_digest"`
	State              string   `json:"state"`
	Track              string   `json:"track"`
	DependencyStatus   string   `json:"dependency_status"`
	DependsOn          []string `json:"depends_on"`
	BlockedBy          []string `json:"blocked_by"`
	Claimable          bool     `json:"claimable"`
	ClaimIDs           []string `json:"claim_ids,omitempty"`
	SourceStartPoints  []string `json:"source_start_points,omitempty"`
	FileScope          []string `json:"file_scope,omitempty"`
	AcceptanceCriteria []string `json:"acceptance_criteria,omitempty"`
}

type DeferredClaim struct {
	ClaimID string `json:"claim_id"`
	Owner   string `json:"owner"`
}

type ExcludedClaim struct {
	ClaimID string `json:"claim_id"`
	Reason  string `json:"reason"`
}

type ActionItem struct {
	Type        string `json:"type"`
	TaskID      string `json:"task_id"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Description string `json:"description"`
}

type ContinuationParent struct {
	ID             string `json:"id"`
	ChildKey       string `json:"child_key,omitempty"`
	ManifestDigest string `json:"manifest_digest,omitempty"`
}

// FindingFollowUp mirrors store.FindingFollowUp: a task born from a non-blocking review
// finding, which is not a planned continuation.
type FindingFollowUp struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	State string `json:"state"`
	Track string `json:"track"`
	Held  bool   `json:"held"`
}

type SubmissionManifest struct {
	ReviewRound    int             `json:"review_round"`
	ParentTaskID   string          `json:"parent_task_id"`
	ManifestJSON   json.RawMessage `json:"manifest_json"`
	ManifestDigest string          `json:"manifest_digest"`
	SubmittedAt    string          `json:"submitted_at"`
}

type TaskLink struct {
	ID           string  `json:"id"`
	TaskID       string  `json:"task_id"`
	Kind         string  `json:"kind"`
	Value        string  `json:"value"`
	ReviewRound  *int    `json:"review_round"`  // the review round an implement submission added it in; nil for older links
	TombstonedAt *string `json:"tombstoned_at"` // set once the reconciler has nothing left to do for the link
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

type Finding struct {
	ID            string  `json:"id"`
	Severity      string  `json:"severity"`
	File          string  `json:"file"`
	Line          int     `json:"line"`
	Summary       string  `json:"summary"`
	InChangedText bool    `json:"in_changed_text"`
	Status        string  `json:"status"`
	PriorID       *string `json:"prior_id,omitempty"`
}

// Dispute is a worker's dispute of a prior-round review finding, submitted with
// cited source evidence during research-track rework.
type Dispute struct {
	FindingID string `json:"finding_id"`
	Evidence  string `json:"evidence"`
}

type Event struct {
	ID        string     `json:"id"`
	TaskID    string     `json:"task_id"`
	Actor     string     `json:"actor"`
	Kind      string     `json:"kind"`
	Verdict   *string    `json:"verdict"`
	Note      *string    `json:"note"`
	Findings  *[]Finding `json:"findings"`
	Disputes  *[]Dispute `json:"disputes"`
	CreatedAt string     `json:"created_at"`
}

type ReviewerScorecard struct {
	Model                                   string         `json:"model"`
	FindingsRaised                          map[string]int `json:"findings_raised"`
	FindingsHeld                            int            `json:"findings_held"`
	FindingsWithdrawn                       int            `json:"findings_withdrawn"`
	FindingsUnresolved                      int            `json:"findings_unresolved"`
	ApprovalsWithLaterFixedBlockingFindings int            `json:"approvals_with_later_fixed_blocking_findings"`
	TotalReviewRounds                       int            `json:"total_review_rounds"`
	SampleSize                              int            `json:"sample_size"`
}

type ReviewerScorecards struct {
	Scorecards []ReviewerScorecard `json:"reviewer_scorecards"`
}

type ResearchPoolConfig struct {
	AccountID          string  `json:"account_id"`
	StartRate          float64 `json:"start_rate"`
	BurstCapacity      int     `json:"burst_capacity"`
	ConcurrentLimit    int     `json:"concurrent_limit"`
	CompletionReserved int     `json:"completion_reserved"`
}

type ResearchPolicy struct {
	Mode  string               `json:"mode"`
	Pools []ResearchPoolConfig `json:"pools"`
}

type ResearchPoolState struct {
	AccountID        string  `json:"account_id"`
	Active           int     `json:"active"`
	ActiveCompletion int     `json:"active_completion"`
	Deferred         int     `json:"deferred"`
	Tokens           float64 `json:"tokens"`
	SettledAt        string  `json:"settled_at"`
}

type ResearchStatus struct {
	Mode  string              `json:"mode"`
	Pools []ResearchPoolState `json:"pools"`
}

type ResearchAdmission struct {
	PermitID  string `json:"permit_id"`
	AttemptID string `json:"attempt_id"`
	RequestID string `json:"request_id"`
	AccountID string `json:"account_id"`
	ExpiresAt string `json:"expires_at"`
	WorkClass string `json:"work_class"`
}

type ResearchAttempt struct {
	ID                string  `json:"id"`
	PermitID          string  `json:"permit_id"`
	TaskID            string  `json:"task_id"`
	AccountID         string  `json:"account_id"`
	State             string  `json:"state"`
	StartedAt         string  `json:"started_at"`
	ExpiresAt         string  `json:"expires_at"`
	EndedAt           *string `json:"ended_at,omitempty"`
	ExitClass         *string `json:"exit_class,omitempty"`
	DurationMS        *int64  `json:"duration_ms,omitempty"`
	UsageTokens       *int64  `json:"usage_tokens,omitempty"`
	SequenceNumber    int     `json:"sequence_number"`
	PreviousAttemptID *string `json:"previous_attempt_id,omitempty"`
	Completion        bool    `json:"completion"`
}

// HTTPClient implements the Client interface.
type HTTPClient struct {
	baseURL string
	token   string
	http    *http.Client

	// attempts maps task ID to the research attempt ID returned by its claim, so
	// heartbeat and submit from the same client fence against that attempt.
	attemptsMu sync.Mutex
	attempts   map[string]string
}

// AttemptID returns the research attempt this client holds for a task: the one
// returned by its claim, or one set with SetAttemptID. Empty for a task with no
// research attempt.
func (c *HTTPClient) AttemptID(taskID string) string {
	c.attemptsMu.Lock()
	defer c.attemptsMu.Unlock()
	return c.attempts[taskID]
}

// SetAttemptID makes heartbeat and submit for taskID send attemptID, for callers
// that claimed in another process. An empty ID clears it.
func (c *HTTPClient) SetAttemptID(taskID, attemptID string) {
	c.attemptsMu.Lock()
	defer c.attemptsMu.Unlock()
	if attemptID == "" {
		delete(c.attempts, taskID)
		return
	}
	if c.attempts == nil {
		c.attempts = make(map[string]string)
	}
	c.attempts[taskID] = attemptID
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
	StatusCode        int
	Code              string
	Message           string
	Reason            string
	NotBefore         *string
	RetryAfterSeconds *int64
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
		Code              string  `json:"code"`
		Message           string  `json:"message"`
		Reason            string  `json:"reason"`
		NotBefore         *string `json:"not_before"`
		RetryAfterSeconds *int64  `json:"retry_after_seconds"`
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
				apiErr.Reason = errResp.Error.Reason
				apiErr.NotBefore = errResp.Error.NotBefore
				apiErr.RetryAfterSeconds = errResp.Error.RetryAfterSeconds
			}
		}

		// If RetryAfterSeconds is not set in the body, try parsing the Retry-After header.
		if apiErr.RetryAfterSeconds == nil && resp.StatusCode == 429 {
			if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
				if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil {
					apiErr.RetryAfterSeconds = &seconds
				}
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

	projects := []Project{}
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
	Fields    string
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

// WithFields sets the fields parameter.
func WithFields(fields string) TaskListOption {
	return func(opts *TaskListOptions) {
		opts.Fields = fields
	}
}

// ListTasks fetches tasks for a project, optionally filtered by model, kind, claimable status, and state.
func (c *HTTPClient) ListTasks(ctx context.Context, projectID string, options ...TaskListOption) ([]Task, error) {
	opts := &TaskListOptions{}
	for _, opt := range options {
		opt(opts)
	}

	path := fmt.Sprintf("/projects/%s/tasks", projectID)
	if opts.Model != "" || opts.Kind != "" || opts.Claimable || opts.State != "" || opts.Fields != "" {
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
		if opts.Fields != "" {
			params = append(params, fmt.Sprintf("fields=%s", opts.Fields))
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

	tasks := []Task{}
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
	AgentID   string `json:"agent_id"`
	Model     string `json:"model"`
	RequestID string `json:"request_id,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	WorkClass string `json:"work_class,omitempty"`
}

// ClaimTask claims a task as in_progress by the given agent and model.
// Returns ErrAlreadyClaimed if the task is already claimed by another worker (409 status).
// requestID, accountID, and workClass are optional; when provided they enable stable
// admission identity for transport retry recovery.
func (c *HTTPClient) ClaimTask(ctx context.Context, id, agentID, model, requestID, accountID, workClass string) (*ResearchAdmission, error) {
	body := claimTaskRequest{
		AgentID:   agentID,
		Model:     model,
		RequestID: requestID,
		AccountID: accountID,
		WorkClass: workClass,
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/claim", id), body)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 409 {
			return nil, ErrAlreadyClaimed
		}
		return nil, err
	}
	defer resp.Body.Close()

	// A research claim returns research_admission; keep the attempt_id so later
	// heartbeats and the submit are fenced to this attempt. Anything else (a bare
	// task, an unreadable body) means there is no admission.
	var claimed struct {
		ResearchAdmission *ResearchAdmission `json:"research_admission"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&claimed); err != nil {
		return nil, nil
	}
	if claimed.ResearchAdmission != nil {
		c.SetAttemptID(id, claimed.ResearchAdmission.AttemptID)
	}

	return claimed.ResearchAdmission, nil
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
	AgentID   string `json:"agent_id"`
	AttemptID string `json:"attempt_id,omitempty"`
}

// HeartbeatTask extends a task's lease.
func (c *HTTPClient) HeartbeatTask(ctx context.Context, id, agentID string) error {
	body := heartbeatTaskRequest{
		AgentID:   agentID,
		AttemptID: c.AttemptID(id),
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
	AgentID   string          `json:"agent_id"`
	AttemptID string          `json:"attempt_id,omitempty"`
	Result    string          `json:"result"`
	Verdict   *string         `json:"verdict,omitempty"`
	Links     []LinkInput     `json:"links"`
	Findings  json.RawMessage `json:"findings,omitempty"`
	Disputes  json.RawMessage `json:"disputes,omitempty"`
	Manifest  json.RawMessage `json:"manifest,omitempty"`
}

// SubmitTask submits a task result with optional verdict and links.
func (c *HTTPClient) SubmitTask(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput) error {
	return c.SubmitTaskWithFindings(ctx, id, agentID, result, verdict, links, nil)
}

// SubmitTaskWithFindings submits a task result with optional verdict, links, and findings.
func (c *HTTPClient) SubmitTaskWithFindings(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput, findings json.RawMessage) error {
	return c.SubmitTaskWithDisputesAndFindings(ctx, id, agentID, result, verdict, links, findings, nil)
}

// SubmitTaskWithDisputesAndFindings submits a task result with optional verdict,
// links, structured findings (review-kind tasks), and disputes (research-track
// implement rework, per docs/features/research-track.md section 5).
func (c *HTTPClient) SubmitTaskWithDisputesAndFindings(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput, findings json.RawMessage, disputes json.RawMessage) error {
	return c.SubmitTaskWithManifest(ctx, id, agentID, result, verdict, links, findings, disputes, nil)
}

// SubmitTaskWithManifest is SubmitTaskWithDisputesAndFindings plus an optional research
// continuation manifest (research-track implement, per docs/features/research-continuations.md).
func (c *HTTPClient) SubmitTaskWithManifest(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput, findings json.RawMessage, disputes json.RawMessage, manifest json.RawMessage) error {
	body := submitTaskRequest{
		AgentID:   agentID,
		AttemptID: c.AttemptID(id),
		Result:    result,
		Verdict:   verdict,
		Links:     links,
		Findings:  findings,
		Disputes:  disputes,
		Manifest:  manifest,
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

// BeginLanding reserves an approved local_commit task for approve attempt `attempt` to land the
// work reviewed in reviewRound (commit); it fails unless the task is still approved in that round.
func (c *HTTPClient) BeginLanding(ctx context.Context, id string, reviewRound int, commit, attempt string) error {
	body := map[string]any{"review_round": reviewRound, "commit": commit, "attempt": attempt}
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/landing", id), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// CompleteLanding marks a task done for approve attempt `attempt`, which holds its landing
// reservation and has published the reviewed work.
func (c *HTTPClient) CompleteLanding(ctx context.Context, id, attempt string, note *string) error {
	body := map[string]any{"attempt": attempt, "note": note}
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/tasks/%s/landing/complete", id), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return nil
}

// CancelLanding drops a landing reservation whose work never reached the branch, if approve
// attempt `attempt` still owns it.
func (c *HTTPClient) CancelLanding(ctx context.Context, id, attempt string) error {
	resp, err := c.do(ctx, "DELETE", fmt.Sprintf("/tasks/%s/landing?attempt=%s", id, url.QueryEscape(attempt)), nil)
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

// GetResearchReviewerScorecards fetches reviewer scorecards for a project.
func (c *HTTPClient) GetResearchReviewerScorecards(ctx context.Context, projectID string) (ReviewerScorecards, error) {
	resp, err := c.do(ctx, "GET", fmt.Sprintf("/projects/%s/research/reviewers", projectID), nil)
	if err != nil {
		return ReviewerScorecards{}, err
	}
	defer resp.Body.Close()

	var scorecards ReviewerScorecards
	if err := json.NewDecoder(resp.Body).Decode(&scorecards); err != nil {
		return ReviewerScorecards{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return scorecards, nil
}

// GetResearchPolicy fetches the research pacing policy configuration.
func (c *HTTPClient) GetResearchPolicy(ctx context.Context) (ResearchPolicy, error) {
	resp, err := c.do(ctx, "GET", "/research/policy", nil)
	if err != nil {
		return ResearchPolicy{}, err
	}
	defer resp.Body.Close()

	var policy ResearchPolicy
	if err := json.NewDecoder(resp.Body).Decode(&policy); err != nil {
		return ResearchPolicy{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return policy, nil
}

// GetResearchStatus fetches the current research pool status.
func (c *HTTPClient) GetResearchStatus(ctx context.Context) (ResearchStatus, error) {
	resp, err := c.do(ctx, "GET", "/research/status", nil)
	if err != nil {
		return ResearchStatus{}, err
	}
	defer resp.Body.Close()

	var status ResearchStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return ResearchStatus{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return status, nil
}

// RenewResearchPermit extends the lease on an active research attempt.
func (c *HTTPClient) RenewResearchPermit(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID string) (ResearchAttempt, error) {
	body := map[string]string{
		"task_id":    taskID,
		"model":      model,
		"agent_id":   agentID,
		"request_id": requestID,
		"attempt_id": attemptID,
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/research/permits/%s/renew", url.PathEscape(permitID)), body)
	if err != nil {
		return ResearchAttempt{}, err
	}
	defer resp.Body.Close()

	var result struct {
		Attempt ResearchAttempt `json:"attempt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ResearchAttempt{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Attempt, nil
}

// FinalizeResearchPermit ends an active research attempt and records the outcome.
func (c *HTTPClient) FinalizeResearchPermit(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID, exitClass string, usageTokens *int64) (ResearchAttempt, error) {
	body := map[string]interface{}{
		"task_id":    taskID,
		"model":      model,
		"agent_id":   agentID,
		"request_id": requestID,
		"attempt_id": attemptID,
		"exit_class": exitClass,
	}
	if usageTokens != nil {
		body["usage_tokens"] = usageTokens
	}

	resp, err := c.do(ctx, "POST", fmt.Sprintf("/research/permits/%s/finalize", url.PathEscape(permitID)), body)
	if err != nil {
		return ResearchAttempt{}, err
	}
	defer resp.Body.Close()

	var result struct {
		Attempt ResearchAttempt `json:"attempt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ResearchAttempt{}, fmt.Errorf("failed to decode response: %w", err)
	}

	return result.Attempt, nil
}
