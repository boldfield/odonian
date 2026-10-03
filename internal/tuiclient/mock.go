package tuiclient

import (
	"context"
	"encoding/json"
)

// MockClient is a mock implementation of the Client interface for testing.
type MockClient struct {
	ListProjectsFunc                  func(ctx context.Context, options ...ProjectListOption) ([]Project, error)
	GetProjectFunc                    func(ctx context.Context, id string) (Project, error)
	ListTasksFunc                     func(ctx context.Context, projectID string, options ...TaskListOption) ([]Task, error)
	GetTaskFunc                       func(ctx context.Context, id string) (TaskDetail, error)
	ListEventsFunc                    func(ctx context.Context, taskID string) ([]Event, error)
	ListDocumentsFunc                 func(ctx context.Context, projectID string) ([]Document, error)
	PromoteTaskFunc                   func(ctx context.Context, id string) error
	ClaimTaskFunc                     func(ctx context.Context, id, agentID, model, requestID, accountID, workClass string) (*ResearchAdmission, error)
	ReviewTaskFunc                    func(ctx context.Context, id, actor, verdict string, note *string) error
	TransitionTaskFunc                func(ctx context.Context, id, to string, note *string) error
	HeartbeatTaskFunc                 func(ctx context.Context, id, agentID string) error
	SubmitTaskFunc                    func(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput) error
	HoldTaskFunc                      func(ctx context.Context, id string) error
	BeginLandingFunc                  func(ctx context.Context, id string, reviewRound int, commit, attempt string) error
	CancelLandingFunc                 func(ctx context.Context, id, attempt string) error
	CompleteLandingFunc               func(ctx context.Context, id, attempt string, note *string) error
	ReleaseTaskFunc                   func(ctx context.Context, id string) error
	ArchiveTaskFunc                   func(ctx context.Context, id string) error
	ArchiveProjectFunc                func(ctx context.Context, id string) error
	GetResearchReviewerScorecardsFunc func(ctx context.Context, projectID string) (ReviewerScorecards, error)
	GetResearchPolicyFunc             func(ctx context.Context) (ResearchPolicy, error)
	GetResearchStatusFunc             func(ctx context.Context) (ResearchStatus, error)
	RenewResearchPermitFunc           func(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID string) (json.RawMessage, error)
	FinalizeResearchPermitFunc        func(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID, exitClass string, usageTokens *int64) (json.RawMessage, error)
	CreateEvaluationCampaignFunc      func(ctx context.Context, id, name, description, projects, models, cohort string, cap int) (map[string]interface{}, error)
	GetEvaluationCampaignFunc         func(ctx context.Context, id string) (map[string]interface{}, error)
	GetEvaluationSampleFunc           func(ctx context.Context, campaignID, sampleID string) (map[string]interface{}, error)
	ClaimEvaluationJobFunc            func(ctx context.Context, sampleID, candidateID, requestID string, leaseTTLMs int64) (map[string]interface{}, error)
	RenewEvaluationAttemptFunc        func(ctx context.Context, jobID, attemptID string, expiresAtMs int64) (map[string]interface{}, error)
	FinalizeEvaluationAttemptFunc     func(ctx context.Context, jobID, attemptID, fenceAttemptID, exitClass, result string) (map[string]interface{}, error)
	PauseEvaluationCampaignFunc       func(ctx context.Context, id string) (map[string]interface{}, error)
	Tasks                             []Task // for simple test data
}

func (m *MockClient) ListProjects(ctx context.Context, options ...ProjectListOption) ([]Project, error) {
	if m.ListProjectsFunc != nil {
		return m.ListProjectsFunc(ctx, options...)
	}
	return nil, nil
}

func (m *MockClient) GetProject(ctx context.Context, id string) (Project, error) {
	if m.GetProjectFunc != nil {
		return m.GetProjectFunc(ctx, id)
	}
	return Project{}, nil
}

func (m *MockClient) ListTasks(ctx context.Context, projectID string, options ...TaskListOption) ([]Task, error) {
	if m.ListTasksFunc != nil {
		return m.ListTasksFunc(ctx, projectID, options...)
	}
	return m.Tasks, nil
}

func (m *MockClient) GetTask(ctx context.Context, id string) (TaskDetail, error) {
	if m.GetTaskFunc != nil {
		return m.GetTaskFunc(ctx, id)
	}
	return TaskDetail{}, nil
}

func (m *MockClient) ListEvents(ctx context.Context, taskID string) ([]Event, error) {
	if m.ListEventsFunc != nil {
		return m.ListEventsFunc(ctx, taskID)
	}
	return nil, nil
}

func (m *MockClient) ListDocuments(ctx context.Context, projectID string) ([]Document, error) {
	if m.ListDocumentsFunc != nil {
		return m.ListDocumentsFunc(ctx, projectID)
	}
	return nil, nil
}

func (m *MockClient) PromoteTask(ctx context.Context, id string) error {
	return m.PromoteTaskFunc(ctx, id)
}

func (m *MockClient) ClaimTask(ctx context.Context, id, agentID, model, requestID, accountID, workClass string) (*ResearchAdmission, error) {
	if m.ClaimTaskFunc != nil {
		return m.ClaimTaskFunc(ctx, id, agentID, model, requestID, accountID, workClass)
	}
	return nil, nil
}

func (m *MockClient) ReviewTask(ctx context.Context, id, actor, verdict string, note *string) error {
	return m.ReviewTaskFunc(ctx, id, actor, verdict, note)
}

func (m *MockClient) TransitionTask(ctx context.Context, id, to string, note *string) error {
	return m.TransitionTaskFunc(ctx, id, to, note)
}

func (m *MockClient) HeartbeatTask(ctx context.Context, id, agentID string) error {
	if m.HeartbeatTaskFunc != nil {
		return m.HeartbeatTaskFunc(ctx, id, agentID)
	}
	return nil
}

func (m *MockClient) SubmitTask(ctx context.Context, id, agentID, result string, verdict *string, links []LinkInput) error {
	if m.SubmitTaskFunc != nil {
		return m.SubmitTaskFunc(ctx, id, agentID, result, verdict, links)
	}
	return nil
}

func (m *MockClient) HoldTask(ctx context.Context, id string) error {
	if m.HoldTaskFunc != nil {
		return m.HoldTaskFunc(ctx, id)
	}
	return nil
}

func (m *MockClient) BeginLanding(ctx context.Context, id string, reviewRound int, commit, attempt string) error {
	if m.BeginLandingFunc != nil {
		return m.BeginLandingFunc(ctx, id, reviewRound, commit, attempt)
	}
	return nil
}

func (m *MockClient) CompleteLanding(ctx context.Context, id, attempt string, note *string) error {
	if m.CompleteLandingFunc != nil {
		return m.CompleteLandingFunc(ctx, id, attempt, note)
	}
	return nil
}

func (m *MockClient) CancelLanding(ctx context.Context, id, attempt string) error {
	if m.CancelLandingFunc != nil {
		return m.CancelLandingFunc(ctx, id, attempt)
	}
	return nil
}

func (m *MockClient) ReleaseTask(ctx context.Context, id string) error {
	if m.ReleaseTaskFunc != nil {
		return m.ReleaseTaskFunc(ctx, id)
	}
	return nil
}

func (m *MockClient) ArchiveTask(ctx context.Context, id string) error {
	if m.ArchiveTaskFunc != nil {
		return m.ArchiveTaskFunc(ctx, id)
	}
	return nil
}

func (m *MockClient) ArchiveProject(ctx context.Context, id string) error {
	if m.ArchiveProjectFunc != nil {
		return m.ArchiveProjectFunc(ctx, id)
	}
	return nil
}

func (m *MockClient) GetResearchReviewerScorecards(ctx context.Context, projectID string) (ReviewerScorecards, error) {
	if m.GetResearchReviewerScorecardsFunc != nil {
		return m.GetResearchReviewerScorecardsFunc(ctx, projectID)
	}
	return ReviewerScorecards{}, nil
}

func (m *MockClient) GetResearchPolicy(ctx context.Context) (ResearchPolicy, error) {
	if m.GetResearchPolicyFunc != nil {
		return m.GetResearchPolicyFunc(ctx)
	}
	return ResearchPolicy{}, nil
}

func (m *MockClient) GetResearchStatus(ctx context.Context) (ResearchStatus, error) {
	if m.GetResearchStatusFunc != nil {
		return m.GetResearchStatusFunc(ctx)
	}
	return ResearchStatus{}, nil
}

func (m *MockClient) RenewResearchPermit(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID string) (json.RawMessage, error) {
	if m.RenewResearchPermitFunc != nil {
		return m.RenewResearchPermitFunc(ctx, permitID, taskID, model, agentID, requestID, attemptID)
	}
	return nil, nil
}

func (m *MockClient) FinalizeResearchPermit(ctx context.Context, permitID, taskID, model, agentID, requestID, attemptID, exitClass string, usageTokens *int64) (json.RawMessage, error) {
	if m.FinalizeResearchPermitFunc != nil {
		return m.FinalizeResearchPermitFunc(ctx, permitID, taskID, model, agentID, requestID, attemptID, exitClass, usageTokens)
	}
	return nil, nil
}

func (m *MockClient) CreateEvaluationCampaign(ctx context.Context, id, name, description, projects, models, cohort string, cap int) (map[string]interface{}, error) {
	if m.CreateEvaluationCampaignFunc != nil {
		return m.CreateEvaluationCampaignFunc(ctx, id, name, description, projects, models, cohort, cap)
	}
	return make(map[string]interface{}), nil
}

func (m *MockClient) GetEvaluationCampaign(ctx context.Context, id string) (map[string]interface{}, error) {
	if m.GetEvaluationCampaignFunc != nil {
		return m.GetEvaluationCampaignFunc(ctx, id)
	}
	return make(map[string]interface{}), nil
}

func (m *MockClient) GetEvaluationSample(ctx context.Context, campaignID, sampleID string) (map[string]interface{}, error) {
	if m.GetEvaluationSampleFunc != nil {
		return m.GetEvaluationSampleFunc(ctx, campaignID, sampleID)
	}
	return make(map[string]interface{}), nil
}

func (m *MockClient) ClaimEvaluationJob(ctx context.Context, sampleID, candidateID, requestID string, leaseTTLMs int64) (map[string]interface{}, error) {
	if m.ClaimEvaluationJobFunc != nil {
		return m.ClaimEvaluationJobFunc(ctx, sampleID, candidateID, requestID, leaseTTLMs)
	}
	return make(map[string]interface{}), nil
}

func (m *MockClient) RenewEvaluationAttempt(ctx context.Context, jobID, attemptID string, expiresAtMs int64) (map[string]interface{}, error) {
	if m.RenewEvaluationAttemptFunc != nil {
		return m.RenewEvaluationAttemptFunc(ctx, jobID, attemptID, expiresAtMs)
	}
	return make(map[string]interface{}), nil
}

func (m *MockClient) FinalizeEvaluationAttempt(ctx context.Context, jobID, attemptID, fenceAttemptID, exitClass, result string) (map[string]interface{}, error) {
	if m.FinalizeEvaluationAttemptFunc != nil {
		return m.FinalizeEvaluationAttemptFunc(ctx, jobID, attemptID, fenceAttemptID, exitClass, result)
	}
	return make(map[string]interface{}), nil
}

func (m *MockClient) PauseEvaluationCampaign(ctx context.Context, id string) (map[string]interface{}, error) {
	if m.PauseEvaluationCampaignFunc != nil {
		return m.PauseEvaluationCampaignFunc(ctx, id)
	}
	return make(map[string]interface{}), nil
}
