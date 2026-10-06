package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/boldfield/odonian/internal/policy"
)

// Research admission at claim time. A research task claim is one transaction:
// ordinary claimability (state, holds, dependencies, model) is checked first,
// then the pool is checked and debited, the attempt and permit are inserted and
// the task is claimed. Any failure rolls the whole transaction back, so a failed
// claim never debits and a denied claim never changes the task. Every claim path
// (ClaimTask and ClaimResearchTask) goes through it, so the ceiling cannot be
// bypassed by a direct claim.
//
// A lease is a coordination bound, not proof that a remote process stopped: a
// worker partitioned from the server may still be running when its attempt
// expires and a replacement is admitted. The fence below rejects its later
// heartbeats and submissions, but cannot stop work it already did.

type researchAttemptKey struct{}

// WithResearchAttempt returns a context carrying the research attempt ID that
// HeartbeatTask and the submit methods must fence against. Without it they are
// legacy calls: the task's current attempt is renewed on heartbeat, and
// ownership is checked by agent only.
func WithResearchAttempt(ctx context.Context, attemptID string) context.Context {
	if attemptID == "" {
		return ctx
	}
	return context.WithValue(ctx, researchAttemptKey{}, attemptID)
}

func researchAttemptFromContext(ctx context.Context) string {
	id, _ := ctx.Value(researchAttemptKey{}).(string)
	return id
}

func (s *sqliteStore) nowTime() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}

// researchPolicyState is the in-memory policy, swapped atomically by SetResearchPolicy.
type researchPolicyState struct {
	mu        sync.RWMutex
	mode      policy.Mode
	accounts  map[string]string // model -> account ID
	retryHint time.Duration
}

type researchPolicySnapshot struct {
	Mode      policy.Mode
	Accounts  map[string]string
	RetryHint time.Duration
}

func (s *sqliteStore) researchPolicy() researchPolicySnapshot {
	s.research.mu.RLock()
	defer s.research.mu.RUnlock()
	mode := s.research.mode
	if mode == "" {
		mode = policy.ModeDisabled
	}
	return researchPolicySnapshot{Mode: mode, Accounts: s.research.accounts, RetryHint: s.research.retryHint}
}

// SetResearchPolicy installs the admission policy. In observe and enforce mode
// each pool is created with a full burst or updated without refilling (see
// ConfigureResearchPool), so calling it at every start never refills allowance
// or interrupts live attempts. In disabled mode nothing is written. On error the
// previous policy stays in effect.
func (s *sqliteStore) SetResearchPolicy(ctx context.Context, now time.Time, cfg policy.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	accounts := make(map[string]string)
	if cfg.Mode != policy.ModeDisabled {
		for _, p := range cfg.Pools {
			if _, err := s.ConfigureResearchPool(ctx, now, ResearchPoolConfig{
				AccountID: p.AccountID, StartRate: p.StartRate, BurstCapacity: p.BurstCapacity,
				ConcurrentLimit: p.ConcurrentDispatchLimit, CompletionReserved: p.CompletionReserved,
			}); err != nil {
				return err
			}
			for _, m := range p.Models {
				accounts[m] = p.AccountID
			}
		}
	}
	s.research.mu.Lock()
	s.research.mode = cfg.Mode
	s.research.accounts = accounts
	s.research.retryHint = cfg.ConcurrencyRetry
	s.research.mu.Unlock()
	return nil
}

// GetResearchPolicyMode returns the current research admission policy mode.
func (s *sqliteStore) GetResearchPolicyMode(ctx context.Context) (policy.Mode, error) {
	snap := s.researchPolicy()
	return snap.Mode, nil
}

// ResearchClaim is one claim of a specific task by one agent. RequestID is the
// caller's stable idempotency key: a transport retry with the same ID recovers
// the original admission instead of spending again. Empty means a legacy claim
// with no retry recovery. AccountID and Class are optional assertions checked
// against the server's own pool mapping and the task's derived work class.
type ResearchClaim struct {
	RequestID string
	TaskID    string
	AgentID   string
	Model     string
	AccountID string
	Class     policy.WorkClass
	LeaseTTL  time.Duration
}

// ResearchClaimResult is a granted claim. Grant is nil when no attempt was
// created (non-research task, disabled policy, or an unmapped model in observe
// mode). Observed is the hypothetical denial recorded in observe mode while the
// claim was granted anyway.
type ResearchClaimResult struct {
	Task     Task
	Grant    *PermitGrant
	Observed *AdmissionDeniedError
}

// ResearchAdmissionDiagnostic is the latest admission decision for a task.
type ResearchAdmissionDiagnostic struct {
	TaskID       string
	Mode         policy.Mode
	WorkClass    policy.WorkClass
	Model        string
	AccountID    string
	Outcome      policy.Outcome
	Reason       policy.Reason
	NotBefore    *string
	RetryAfterMS *int64
	Hypothetical bool
	DenialCount  int
	DecidedAt    string
}

type claimTaskInfo struct {
	ProjectID   string
	State       string
	Assignee    *string
	Kind        string
	Track       string
	ReviewRound int
	Adjudicates bool
}

func loadClaimTaskInfo(ctx context.Context, tx *sql.Tx, taskID string) (claimTaskInfo, error) {
	var i claimTaskInfo
	err := tx.QueryRowContext(ctx, `
		SELECT project_id, state, assignee, kind, track, review_round, adjudicate_finding_id IS NOT NULL
		FROM task WHERE id = ?`, taskID).
		Scan(&i.ProjectID, &i.State, &i.Assignee, &i.Kind, &i.Track, &i.ReviewRound, &i.Adjudicates)
	if errors.Is(err, sql.ErrNoRows) {
		return i, ErrNotFound
	}
	if err != nil {
		return i, fmt.Errorf("failed to load task for claim: %w", err)
	}
	return i, nil
}

// researchWorkClass derives the work class the server charges for a research
// task: reviews and adjudications are completion work, a re-claimed implement
// task after a rejected round is rework, and a first-round implement is a write.
func researchWorkClass(i claimTaskInfo) policy.WorkClass {
	switch {
	case i.Kind == "review" && i.Adjudicates:
		return policy.ResearchAdjudication
	case i.Kind == "review":
		return policy.ResearchReview
	case i.ReviewRound > 0:
		return policy.ResearchRework
	}
	return policy.ResearchWrite
}

// ClaimResearchTask claims one specific task for one agent under the research
// admission policy; see the package comment above. It returns:
//   - ErrNotFound, ErrConflict or a MODEL_MISMATCH ConflictError when the task is
//     not claimable, checked before the pool is touched;
//   - *AdmissionDeniedError (matches ErrInsufficientCapacity) when the pool
//     refuses. The task, its lease and its history are untouched; only the
//     bounded per-task diagnostic and the overdue-lease sweep are committed;
//   - ErrTaskBusy when the task still has a live attempt (for example a lease
//     that lapsed while the dispatch is still renewed), ErrBindingMismatch when
//     RequestID was used for something else, and ErrConflict when a retried
//     RequestID no longer owns the task;
//   - ErrInvalidResearchInput for a bad lease TTL, class or account assertion.
//
// A retry with the same RequestID while the original attempt still owns the
// task returns the original claim with Grant.Replayed set and charges nothing.
// A new RequestID is a new attempt and spends a new start, linked to the task's
// previous attempt, so rework, adjudication, reclaim and retry all count as work.
func (s *sqliteStore) ClaimResearchTask(ctx context.Context, req ResearchClaim) (ResearchClaimResult, error) {
	pol := s.researchPolicy()
	now := s.nowTime()
	var res ResearchClaimResult

	err := s.withResearchTx(ctx, func(tx *sql.Tx, post *error) error {
		info, err := loadClaimTaskInfo(ctx, tx, req.TaskID)
		if err != nil {
			return err
		}
		if info.Track != "research" || pol.Mode == policy.ModeDisabled {
			t, err := s.claimTaskTx(ctx, tx, now, req.TaskID, req.AgentID, req.Model, req.LeaseTTL)
			res.Task = t
			return err
		}

		switch {
		case req.AgentID == "" || req.Model == "":
			return invalidResearch("agent and model are required")
		case req.LeaseTTL <= 0:
			return invalidResearch("lease TTL must be positive")
		case req.Class != "" && !req.Class.Paced():
			return invalidResearch("work class %q is not paced research work", req.Class)
		}
		class := researchWorkClass(info)
		if req.Class != "" && req.Class != class {
			return invalidResearch("work class %q does not match the task, which is %q work", req.Class, class)
		}
		accountID, mapped := pol.Accounts[req.Model]
		if mapped && req.AccountID != "" && req.AccountID != accountID {
			return invalidResearch("account %q does not match the pool for model %q", req.AccountID, req.Model)
		}

		if req.RequestID != "" {
			existing, err := getPermit(ctx, tx, "request_id", req.RequestID)
			if err == nil {
				return s.replayClaim(ctx, tx, now, req, class, accountID, existing, &res)
			}
			if !errors.Is(err, ErrPermitNotFound) {
				return err
			}
		}

		if err := claimPreflight(ctx, tx, now, req.TaskID, req.Model); err != nil {
			return err
		}

		diag := ResearchAdmissionDiagnostic{
			TaskID: req.TaskID, Mode: pol.Mode, WorkClass: class, Model: req.Model, AccountID: accountID,
			Outcome: policy.OutcomeAdmit, Hypothetical: pol.Mode == policy.ModeObserve,
		}

		if !mapped {
			denied := &AdmissionDeniedError{Outcome: policy.OutcomeUnmapped}
			if err := recordAdmission(ctx, tx, now, diag, denied); err != nil {
				return err
			}
			if pol.Mode == policy.ModeEnforce {
				*post = denied
				return nil
			}
			t, err := s.claimTaskTx(ctx, tx, now, req.TaskID, req.AgentID, req.Model, req.LeaseTTL)
			res.Task, res.Observed = t, denied
			return err
		}

		requestID := req.RequestID
		if requestID == "" {
			requestID = "srv-" + GenerateID()
		}
		permit := ResearchPermit{
			ID: GenerateID(), RequestID: requestID, TaskID: req.TaskID, ProjectID: info.ProjectID, AgentID: req.AgentID,
			Model: req.Model, AccountID: accountID, Completion: class.Completion(), CreatedAt: formatTS(now),
		}
		prev, err := latestTaskAttemptID(ctx, tx, req.TaskID)
		if err != nil {
			return err
		}
		attempt, denied, err := reserveAttempt(ctx, tx, now, permit, 1, prev, req.LeaseTTL, pol.RetryHint, pol.Mode == policy.ModeObserve)
		if err != nil {
			return err
		}
		if err := recordAdmission(ctx, tx, now, diag, denied); err != nil {
			return err
		}
		if denied != nil && pol.Mode == policy.ModeEnforce {
			*post = denied
			return nil
		}

		permit.CurrentAttemptID = attempt.ID
		_, err = tx.ExecContext(ctx, `
			INSERT INTO research_permit (id, request_id, task_id, project_id, agent_id, model, account_id, completion, current_attempt_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			permit.ID, permit.RequestID, permit.TaskID, permit.ProjectID, permit.AgentID, permit.Model, permit.AccountID, permit.Completion, permit.CurrentAttemptID, permit.CreatedAt)
		if err != nil {
			return fmt.Errorf("failed to insert research permit: %w", err)
		}
		t, err := s.claimTaskTx(ctx, tx, now, req.TaskID, req.AgentID, req.Model, req.LeaseTTL)
		if err != nil {
			return err
		}
		res.Task = t
		res.Grant = &PermitGrant{Permit: permit, Attempt: attempt}
		res.Observed = denied
		return nil
	})
	if err != nil {
		return ResearchClaimResult{}, err
	}
	return res, nil
}

// replayClaim returns the original admission for a retried request ID, but only
// while that admission still owns the task.
func (s *sqliteStore) replayClaim(ctx context.Context, tx *sql.Tx, now time.Time, req ResearchClaim, class policy.WorkClass, accountID string, existing ResearchPermit, res *ResearchClaimResult) error {
	if existing.TaskID != req.TaskID || existing.AgentID != req.AgentID || existing.Model != req.Model ||
		existing.AccountID != accountID || existing.Completion != class.Completion() {
		return ErrBindingMismatch
	}
	latest, attempt, found, err := latestTaskAttempt(ctx, tx, req.TaskID)
	if err != nil {
		return err
	}
	if !found || latest.ID != existing.ID || attempt.State != AttemptActive {
		return ErrConflict
	}
	expires, err := parseTS(attempt.ExpiresAt)
	if err != nil {
		return fmt.Errorf("invalid attempt timestamp: %w", err)
	}
	if !expires.After(now) {
		return ErrConflict
	}
	t, err := s.getTaskTx(ctx, tx, req.TaskID)
	if err != nil {
		return err
	}
	if t.State != "in_progress" || t.Assignee == nil || *t.Assignee != req.AgentID {
		return ErrConflict
	}
	res.Task = t
	res.Grant = &PermitGrant{Permit: existing, Attempt: attempt, Replayed: true}
	return nil
}

func (s *sqliteStore) getTaskTx(ctx context.Context, tx *sql.Tx, taskID string) (Task, error) {
	var t Task
	var reviewModelsJSON *string
	err := tx.QueryRowContext(ctx, `
		SELECT id, project_id, document_id, title, spec, state, assignee, lease_expires_at, result, model, kind, review_models, review_round, target_task_id, verdict, agent_merge, held, escalate, track, branch, priority, created_at, updated_at, archived_at, superseded_by
		FROM task WHERE id = ?
	`, taskID).Scan(&t.ID, &t.ProjectID, &t.DocumentID, &t.Title, &t.Spec, &t.State, &t.Assignee, &t.LeaseExpiresAt, &t.Result, &t.Model, &t.Kind, &reviewModelsJSON, &t.ReviewRound, &t.TargetTaskID, &t.Verdict, &t.AgentMerge, &t.Held, &t.Escalate, &t.Track, &t.Branch, &t.Priority, &t.CreatedAt, &t.UpdatedAt, &t.ArchivedAt, &t.SupersededBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("failed to fetch task: %w", err)
	}
	t.ReviewModels = []string{}
	if reviewModelsJSON != nil {
		if err := json.Unmarshal([]byte(*reviewModelsJSON), &t.ReviewModels); err != nil {
			return Task{}, fmt.Errorf("failed to unmarshal review_models: %w", err)
		}
	}
	return t, nil
}

// latestTaskAttempt returns the newest permit for a task and that permit's
// current attempt. found is false when the task never had a permit.
func latestTaskAttempt(ctx context.Context, tx *sql.Tx, taskID string) (ResearchPermit, ResearchAttempt, bool, error) {
	var p ResearchPermit
	err := tx.QueryRowContext(ctx, `
		SELECT id, request_id, task_id, project_id, agent_id, model, account_id, completion, current_attempt_id, created_at
		FROM research_permit WHERE task_id = ? ORDER BY rowid DESC LIMIT 1`, taskID).
		Scan(&p.ID, &p.RequestID, &p.TaskID, &p.ProjectID, &p.AgentID, &p.Model, &p.AccountID, &p.Completion, &p.CurrentAttemptID, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ResearchPermit{}, ResearchAttempt{}, false, nil
	}
	if err != nil {
		return ResearchPermit{}, ResearchAttempt{}, false, fmt.Errorf("failed to load task permit: %w", err)
	}
	a, err := getAttempt(ctx, tx, p.CurrentAttemptID)
	if err != nil {
		return ResearchPermit{}, ResearchAttempt{}, false, err
	}
	return p, a, true, nil
}

func latestTaskAttemptID(ctx context.Context, tx *sql.Tx, taskID string) (*string, error) {
	_, a, found, err := latestTaskAttempt(ctx, tx, taskID)
	if err != nil || !found {
		return nil, err
	}
	return &a.ID, nil
}

// fenceResearchAttempt rejects an operation from a research attempt that is no
// longer the task's current one, and, when renewTTL > 0, extends that attempt's
// lease to now+renewTTL (never shortening it). With an explicit attemptID the
// caller must be the current attempt of the task's newest permit and the same
// agent (ATTEMPT_FENCED), and a heartbeat must find it live (ATTEMPT_EXPIRED).
// Without one (a legacy caller) the call passes, and a live current attempt held
// by the same agent is still renewed. A task that never had a permit is untouched.
func fenceResearchAttempt(ctx context.Context, tx *sql.Tx, now time.Time, taskID, agentID, attemptID string, renewTTL time.Duration) error {
	permit, attempt, found, err := latestTaskAttempt(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if attemptID != "" {
		if !found || attempt.ID != attemptID || permit.AgentID != agentID {
			return conflict("ATTEMPT_FENCED", "research attempt is not the task's current attempt for this agent")
		}
	} else if !found || permit.AgentID != agentID {
		return nil
	}
	if renewTTL <= 0 {
		return nil
	}
	live := attempt.State == AttemptActive
	var expires time.Time
	if live {
		if expires, err = parseTS(attempt.ExpiresAt); err != nil {
			return fmt.Errorf("invalid attempt timestamp: %w", err)
		}
		live = expires.After(now)
	}
	if !live {
		if attemptID != "" {
			return conflict("ATTEMPT_EXPIRED", "research attempt is no longer live")
		}
		return nil
	}
	if next := now.Add(renewTTL); next.After(expires) {
		if _, err := tx.ExecContext(ctx, `UPDATE research_attempt SET expires_at = ? WHERE id = ? AND state = 'active'`, formatTS(next), attempt.ID); err != nil {
			return fmt.Errorf("failed to renew research attempt: %w", err)
		}
	}
	return nil
}

// recordAdmission upserts the task's single diagnostic row with this decision.
// denied nil means admit. The denial count only grows on a denial.
func recordAdmission(ctx context.Context, tx *sql.Tx, now time.Time, d ResearchAdmissionDiagnostic, denied *AdmissionDeniedError) error {
	var reason *string
	var notBefore *string
	var retryMS *int64
	denials := 0
	if denied != nil {
		d.Outcome = denied.Outcome
		if denied.Reason != "" {
			r := string(denied.Reason)
			reason = &r
		}
		if !denied.NotBefore.IsZero() {
			nb := formatTS(denied.NotBefore)
			notBefore = &nb
		}
		if denied.RetryAfter > 0 {
			ms := denied.RetryAfter.Milliseconds()
			retryMS = &ms
		}
		denials = 1
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO research_admission_diagnostic
			(task_id, mode, work_class, model, account_id, outcome, reason, not_before, retry_after_ms, hypothetical, denial_count, decided_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(task_id) DO UPDATE SET
			mode = excluded.mode, work_class = excluded.work_class, model = excluded.model, account_id = excluded.account_id,
			outcome = excluded.outcome, reason = excluded.reason, not_before = excluded.not_before,
			retry_after_ms = excluded.retry_after_ms, hypothetical = excluded.hypothetical,
			denial_count = research_admission_diagnostic.denial_count + excluded.denial_count,
			decided_at = excluded.decided_at`,
		d.TaskID, string(d.Mode), string(d.WorkClass), d.Model, d.AccountID, string(d.Outcome), reason, notBefore, retryMS,
		d.Hypothetical, denials, formatTS(now))
	if err != nil {
		return fmt.Errorf("failed to record research admission: %w", err)
	}
	return nil
}

// GetResearchAdmissionDiagnostic returns the latest admission decision for a
// task, or ErrNotFound when none was ever recorded.
func (s *sqliteStore) GetResearchAdmissionDiagnostic(ctx context.Context, taskID string) (ResearchAdmissionDiagnostic, error) {
	var d ResearchAdmissionDiagnostic
	var mode, class, outcome string
	var reason *string
	err := s.conn.QueryRowContext(ctx, `
		SELECT task_id, mode, work_class, model, account_id, outcome, reason, not_before, retry_after_ms, hypothetical, denial_count, decided_at
		FROM research_admission_diagnostic WHERE task_id = ?`, taskID).
		Scan(&d.TaskID, &mode, &class, &d.Model, &d.AccountID, &outcome, &reason, &d.NotBefore, &d.RetryAfterMS, &d.Hypothetical, &d.DenialCount, &d.DecidedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ResearchAdmissionDiagnostic{}, ErrNotFound
	}
	if err != nil {
		return ResearchAdmissionDiagnostic{}, fmt.Errorf("failed to load research admission diagnostic: %w", err)
	}
	d.Mode, d.WorkClass, d.Outcome = policy.Mode(mode), policy.WorkClass(class), policy.Outcome(outcome)
	if reason != nil {
		d.Reason = policy.Reason(*reason)
	}
	return d, nil
}
