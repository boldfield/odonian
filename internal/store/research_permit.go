package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/boldfield/odonian/internal/policy"
)

// Research permit lifecycle. A permit is the durable admission of one research
// dispatch: it binds a task, project, agent, model, account pool and the
// caller's request ID. Each start of the model is an attempt; the attempt ID is
// the fencing identity for renew and finalize. All helpers take the server time
// as an argument (an injectable clock) and are not wired into claim handlers.
//
// Allowance is a token bucket persisted per account, so a restart never
// refills it. Occupancy is the number of attempts that are active and not past
// expires_at; it is released only by finalize or expiry, never by the task
// leaving in_progress. A start is never refunded.

var (
	ErrPoolNotFound           = errors.New("research pool not found")
	ErrPermitNotFound         = errors.New("research permit not found")
	ErrFenceMismatch          = errors.New("research attempt identity does not match the permit's current attempt")
	ErrPermitExpired          = errors.New("research attempt lease expired")
	ErrPermitFinalized        = errors.New("research attempt already finalized")
	ErrPermitIdentityMismatch = errors.New("permit identity does not match the provided task, model, or agent")
	ErrBindingMismatch        = errors.New("request ID is already bound to a different task, agent, model or pool")
	ErrTaskBusy               = errors.New("task already has a live research attempt")
	ErrAttemptLive            = errors.New("previous research attempt is still live")
	ErrInsufficientCapacity   = errors.New("research pool has insufficient capacity")
	ErrInvalidResearchInput   = errors.New("invalid research permit input")
)

// Exit classes for a finalized attempt. ExitLeaseExpired is recorded by expiry
// and cannot be supplied by a caller.
const (
	ExitCompleted    = "completed"
	ExitFailed       = "failed"
	ExitCancelled    = "cancelled"
	ExitUnknown      = "unknown"
	ExitLeaseExpired = "lease_expired"
)

// Attempt states.
const (
	AttemptActive    = "active"
	AttemptFinalized = "finalized"
	AttemptExpired   = "expired"
)

// AdmissionDeniedError reports a pool refusing a start. It matches
// ErrInsufficientCapacity. Outcome is policy.OutcomeDefer (retry at NotBefore)
// or policy.OutcomeRetry (retry after RetryAfter).
type AdmissionDeniedError struct {
	Outcome    policy.Outcome
	Reason     policy.Reason
	NotBefore  time.Time
	RetryAfter time.Duration
}

func (e *AdmissionDeniedError) Error() string {
	return fmt.Sprintf("research start denied: %s (%s)", e.Outcome, e.Reason)
}

func (e *AdmissionDeniedError) Is(target error) bool { return target == ErrInsufficientCapacity }

// ResearchPoolConfig is the persisted limit side of one account pool; it
// mirrors policy.Pool for the pool's AccountID.
type ResearchPoolConfig struct {
	AccountID          string
	StartRate          float64 // sustained starts per second
	BurstCapacity      int
	ConcurrentLimit    int
	CompletionReserved int
}

// ResearchPoolState is a pool's configuration plus its allowance and live
// occupancy as of the time it was read.
type ResearchPoolState struct {
	ResearchPoolConfig
	Tokens           float64
	SettledAt        string
	Active           int
	ActiveCompletion int
	Deferred         int
}

// PermitRequest asks for one research start. Class must be a paced research
// class; completion classes may use reserved capacity.
type PermitRequest struct {
	RequestID string
	TaskID    string
	ProjectID string
	AgentID   string
	Model     string
	AccountID string
	Class     policy.WorkClass
	LeaseTTL  time.Duration
	// RetryHint is the RetryAfter returned for concurrency denials; zero means
	// policy.DefaultConcurrencyRetry.
	RetryHint time.Duration
}

type ResearchPermit struct {
	ID               string
	RequestID        string
	TaskID           string
	ProjectID        string
	AgentID          string
	Model            string
	AccountID        string
	Completion       bool
	CurrentAttemptID string
	CreatedAt        string
}

// ResearchAttempt is one start. UsageTokens nil means unknown, distinct from 0.
type ResearchAttempt struct {
	ID                string
	PermitID          string
	PreviousAttemptID *string
	SequenceNumber    int
	TaskID            string
	AccountID         string
	Completion        bool
	State             string
	StartedAt         string
	ExpiresAt         string
	EndedAt           *string
	ExitClass         *string
	DurationMS        *int64
	UsageTokens       *int64
}

// PermitGrant is an admitted attempt. Replayed is true when an earlier
// identical request or next-attempt call was recovered instead of charged.
type PermitGrant struct {
	Permit   ResearchPermit
	Attempt  ResearchAttempt
	Replayed bool
}

// ResearchPermitStore is the narrow persistence surface for research admission.
type ResearchPermitStore interface {
	ConfigureResearchPool(ctx context.Context, now time.Time, cfg ResearchPoolConfig) (ResearchPoolState, error)
	GetResearchPool(ctx context.Context, now time.Time, accountID string) (ResearchPoolState, error)
	ListResearchPools(ctx context.Context) ([]ResearchPoolConfig, error)
	ListResearchPoolStates(ctx context.Context, now time.Time) ([]ResearchPoolState, error)
	RequestResearchPermit(ctx context.Context, now time.Time, req PermitRequest) (PermitGrant, error)
	StartNextResearchAttempt(ctx context.Context, now time.Time, permitID, priorAttemptID string, leaseTTL, retryHint time.Duration) (PermitGrant, error)
	RenewResearchAttempt(ctx context.Context, now time.Time, permitID, attemptID string, leaseTTL time.Duration) (ResearchAttempt, error)
	FinalizeResearchAttempt(ctx context.Context, now time.Time, permitID, attemptID, exitClass string, usageTokens *int64) (ResearchAttempt, error)
	ExpireResearchAttempts(ctx context.Context, now time.Time) (int, error)
	GetResearchPermit(ctx context.Context, permitID string) (ResearchPermit, ResearchAttempt, error)
}

func invalidResearch(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidResearchInput, fmt.Sprintf(format, args...))
}

func formatTS(t time.Time) string { return t.UTC().Format(timestampLayout) }

func parseTS(s string) (time.Time, error) { return time.Parse(timestampLayout, s) }

func (c ResearchPoolConfig) validate() error {
	switch {
	case c.AccountID == "":
		return invalidResearch("account ID must not be empty")
	case math.IsNaN(c.StartRate) || math.IsInf(c.StartRate, 0) || c.StartRate <= 0:
		return invalidResearch("start rate must be finite and positive")
	case c.BurstCapacity < 1:
		return invalidResearch("burst capacity must be at least 1")
	case c.ConcurrentLimit < 1:
		return invalidResearch("concurrent limit must be at least 1")
	case c.CompletionReserved < 0 || c.CompletionReserved > c.ConcurrentLimit:
		return invalidResearch("completion reserved must be between 0 and the concurrent limit")
	}
	return nil
}

type poolRow struct {
	ResearchPoolConfig
	tokens    float64
	settledAt time.Time
}

// settle credits refill for time past the high-water mark, capped at burst.
// Time at or before the mark credits nothing, so a clock rollback never
// refills; the returned time is the effective (never earlier) time.
func (p *poolRow) settle(now time.Time) time.Time {
	if !now.After(p.settledAt) {
		return p.settledAt
	}
	p.tokens = math.Min(float64(p.BurstCapacity), p.tokens+now.Sub(p.settledAt).Seconds()*p.StartRate)
	p.settledAt = now
	return now
}

func loadPool(ctx context.Context, tx *sql.Tx, accountID string) (*poolRow, error) {
	var p poolRow
	var settled string
	err := tx.QueryRowContext(ctx, `
		SELECT account_id, start_rate, burst_capacity, concurrent_limit, completion_reserved, tokens, settled_at
		FROM research_pool WHERE account_id = ?`, accountID).
		Scan(&p.AccountID, &p.StartRate, &p.BurstCapacity, &p.ConcurrentLimit, &p.CompletionReserved, &p.tokens, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPoolNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to load research pool: %w", err)
	}
	if p.settledAt, err = parseTS(settled); err != nil {
		return nil, fmt.Errorf("invalid research pool timestamp: %w", err)
	}
	return &p, nil
}

func savePoolBucket(ctx context.Context, tx *sql.Tx, p *poolRow) error {
	_, err := tx.ExecContext(ctx, `UPDATE research_pool SET tokens=?, settled_at=?, updated_at=? WHERE account_id=?`,
		p.tokens, formatTS(p.settledAt), formatTS(p.settledAt), p.AccountID)
	if err != nil {
		return fmt.Errorf("failed to save research pool: %w", err)
	}
	return nil
}

func occupancy(ctx context.Context, tx *sql.Tx, accountID string) (active, activeCompletion int, err error) {
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(completion), 0) FROM research_attempt
		WHERE account_id = ? AND state = 'active'`, accountID).Scan(&active, &activeCompletion)
	if err != nil {
		err = fmt.Errorf("failed to count research occupancy: %w", err)
	}
	return
}

const attemptCols = `id, permit_id, previous_attempt_id, sequence_number, task_id, account_id, completion, state,
	started_at, expires_at, ended_at, exit_class, duration_ms, usage_tokens`

type rowScanner interface{ Scan(dest ...any) error }

func scanAttempt(r rowScanner) (ResearchAttempt, error) {
	var a ResearchAttempt
	err := r.Scan(&a.ID, &a.PermitID, &a.PreviousAttemptID, &a.SequenceNumber, &a.TaskID, &a.AccountID, &a.Completion, &a.State,
		&a.StartedAt, &a.ExpiresAt, &a.EndedAt, &a.ExitClass, &a.DurationMS, &a.UsageTokens)
	return a, err
}

func getAttempt(ctx context.Context, tx *sql.Tx, id string) (ResearchAttempt, error) {
	a, err := scanAttempt(tx.QueryRowContext(ctx, `SELECT `+attemptCols+` FROM research_attempt WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrFenceMismatch
	}
	if err != nil {
		return a, fmt.Errorf("failed to load research attempt: %w", err)
	}
	return a, nil
}

func getPermit(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, where string, arg string) (ResearchPermit, error) {
	var p ResearchPermit
	err := q.QueryRowContext(ctx, `
		SELECT id, request_id, task_id, project_id, agent_id, model, account_id, completion, current_attempt_id, created_at
		FROM research_permit WHERE `+where+` = ?`, arg).
		Scan(&p.ID, &p.RequestID, &p.TaskID, &p.ProjectID, &p.AgentID, &p.Model, &p.AccountID, &p.Completion, &p.CurrentAttemptID, &p.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrPermitNotFound
	}
	if err != nil {
		return p, fmt.Errorf("failed to load research permit: %w", err)
	}
	return p, nil
}

// expireAttempt closes an overdue active attempt at its own expiry time.
func expireAttempt(ctx context.Context, tx *sql.Tx, a ResearchAttempt) error {
	started, err := parseTS(a.StartedAt)
	if err != nil {
		return fmt.Errorf("invalid attempt timestamp: %w", err)
	}
	expires, err := parseTS(a.ExpiresAt)
	if err != nil {
		return fmt.Errorf("invalid attempt timestamp: %w", err)
	}
	dur := expires.Sub(started).Milliseconds()
	if dur < 0 {
		dur = 0
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE research_attempt SET state='expired', ended_at=expires_at, exit_class=?, duration_ms=?
		WHERE id = ? AND state = 'active'`, ExitLeaseExpired, dur, a.ID)
	if err != nil {
		return fmt.Errorf("failed to expire research attempt: %w", err)
	}
	return nil
}

// sweepOverdue expires every active attempt whose lease ended at or before at.
func sweepOverdue(ctx context.Context, tx *sql.Tx, at time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+attemptCols+` FROM research_attempt WHERE state='active' AND expires_at <= ?`, formatTS(at))
	if err != nil {
		return 0, fmt.Errorf("failed to find overdue research attempts: %w", err)
	}
	var overdue []ResearchAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("failed to scan research attempt: %w", err)
		}
		overdue = append(overdue, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, a := range overdue {
		if err := expireAttempt(ctx, tx, a); err != nil {
			return 0, err
		}
	}
	return len(overdue), nil
}

// withResearchTx runs fn in a write transaction. fn may set *post to an error
// to hand back after the transaction still commits (for example a denial that
// must persist an expiry sweep); returning an error rolls everything back.
func (s *sqliteStore) withResearchTx(ctx context.Context, fn func(tx *sql.Tx, post *error) error) error {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	var post error
	if err := fn(tx, &post); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}
	return post
}

// ConfigureResearchPool creates a pool with a full burst, or updates an
// existing pool's limits without resetting anything: allowance is first settled
// under the old rate, then clamped to the new burst, and the high-water mark
// and active attempts are untouched, so a policy change can neither mint a
// burst larger than the new capacity nor interrupt live work.
func (s *sqliteStore) ConfigureResearchPool(ctx context.Context, now time.Time, cfg ResearchPoolConfig) (ResearchPoolState, error) {
	if err := cfg.validate(); err != nil {
		return ResearchPoolState{}, err
	}
	var state ResearchPoolState
	err := s.withResearchTx(ctx, func(tx *sql.Tx, post *error) error {
		p, err := loadPool(ctx, tx, cfg.AccountID)
		switch {
		case errors.Is(err, ErrPoolNotFound):
			ts := formatTS(now)
			_, err = tx.ExecContext(ctx, `
				INSERT INTO research_pool (account_id, start_rate, burst_capacity, concurrent_limit, completion_reserved, tokens, settled_at, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				cfg.AccountID, cfg.StartRate, cfg.BurstCapacity, cfg.ConcurrentLimit, cfg.CompletionReserved, float64(cfg.BurstCapacity), ts, ts, ts)
			if err != nil {
				return fmt.Errorf("failed to create research pool: %w", err)
			}
		case err != nil:
			return err
		default:
			p.settle(now)
			p.tokens = math.Min(p.tokens, float64(cfg.BurstCapacity))
			_, err = tx.ExecContext(ctx, `
				UPDATE research_pool SET start_rate=?, burst_capacity=?, concurrent_limit=?, completion_reserved=?, tokens=?, settled_at=?, updated_at=?
				WHERE account_id=?`,
				cfg.StartRate, cfg.BurstCapacity, cfg.ConcurrentLimit, cfg.CompletionReserved, p.tokens, formatTS(p.settledAt), formatTS(now), cfg.AccountID)
			if err != nil {
				return fmt.Errorf("failed to update research pool: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return ResearchPoolState{}, err
	}
	state, err = s.GetResearchPool(ctx, now, cfg.AccountID)
	return state, err
}

// GetResearchPool reads a pool without changing it: allowance is settled to
// now in the returned view only, and occupancy counts attempts still live at now.
func (s *sqliteStore) GetResearchPool(ctx context.Context, now time.Time, accountID string) (ResearchPoolState, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return ResearchPoolState{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	p, err := loadPool(ctx, tx, accountID)
	if err != nil {
		return ResearchPoolState{}, err
	}
	at := p.settle(now)
	st := ResearchPoolState{ResearchPoolConfig: p.ResearchPoolConfig, Tokens: p.tokens, SettledAt: formatTS(p.settledAt)}
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(completion), 0) FROM research_attempt
		WHERE account_id = ? AND state = 'active' AND expires_at > ?`, accountID, formatTS(at)).Scan(&st.Active, &st.ActiveCompletion)
	if err != nil {
		return ResearchPoolState{}, fmt.Errorf("failed to count research occupancy: %w", err)
	}
	return st, nil
}

// ListResearchPools reads all configured pools without changing them.
func (s *sqliteStore) ListResearchPools(ctx context.Context) ([]ResearchPoolConfig, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT account_id, start_rate, burst_capacity, concurrent_limit, completion_reserved
		FROM research_pool
		ORDER BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("failed to query research pools: %w", err)
	}
	defer rows.Close()

	var pools []ResearchPoolConfig
	for rows.Next() {
		var cfg ResearchPoolConfig
		if err := rows.Scan(&cfg.AccountID, &cfg.StartRate, &cfg.BurstCapacity, &cfg.ConcurrentLimit, &cfg.CompletionReserved); err != nil {
			return nil, fmt.Errorf("failed to scan research pool: %w", err)
		}
		pools = append(pools, cfg)
	}
	return pools, rows.Err()
}

// ListResearchPoolStates reads all pools with their current state without changing them.
func (s *sqliteStore) ListResearchPoolStates(ctx context.Context, now time.Time) ([]ResearchPoolState, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT account_id, start_rate, burst_capacity, concurrent_limit, completion_reserved, tokens, settled_at
		FROM research_pool
		ORDER BY account_id`)
	if err != nil {
		return nil, fmt.Errorf("failed to query research pools: %w", err)
	}
	defer rows.Close()

	var states []ResearchPoolState
	for rows.Next() {
		var state ResearchPoolState
		var settledAtStr string
		if err := rows.Scan(&state.AccountID, &state.StartRate, &state.BurstCapacity, &state.ConcurrentLimit, &state.CompletionReserved, &state.Tokens, &settledAtStr); err != nil {
			return nil, fmt.Errorf("failed to scan research pool: %w", err)
		}

		// Parse settled_at timestamp
		settledAt, err := parseTS(settledAtStr)
		if err != nil {
			return nil, fmt.Errorf("failed to parse settled_at: %w", err)
		}

		// Settle the pool to the current time
		at := settledAt
		if now.After(settledAt) {
			state.Tokens = math.Min(float64(state.BurstCapacity), state.Tokens+now.Sub(settledAt).Seconds()*state.StartRate)
			at = now
		}
		state.SettledAt = formatTS(at)

		// Count active attempts
		var active, activeCompletion int
		err = tx.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(completion), 0) FROM research_attempt
			WHERE account_id = ? AND state = 'active' AND expires_at > ?`, state.AccountID, formatTS(at)).Scan(&active, &activeCompletion)
		if err != nil {
			return nil, fmt.Errorf("failed to count research occupancy: %w", err)
		}
		state.Active = active
		state.ActiveCompletion = activeCompletion

		// Count deferred tasks
		var deferred int
		err = tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM research_admission_diagnostic
			WHERE account_id = ? AND outcome = 'defer'`, state.AccountID).Scan(&deferred)
		if err != nil {
			return nil, fmt.Errorf("failed to count deferred tasks: %w", err)
		}
		state.Deferred = deferred

		states = append(states, state)
	}
	return states, rows.Err()
}

const tokenEpsilon = 1e-9

// admit decides one start against the already-settled pool and live occupancy,
// mirroring policy.Evaluator: concurrency is checked before the bucket so no
// allowance is spent while waiting on an active dispatch.
func admit(p *poolRow, active, activeCompletion int, completion bool, retryHint time.Duration) *AdmissionDeniedError {
	if retryHint <= 0 {
		retryHint = policy.DefaultConcurrencyRetry
	}
	retry := func(r policy.Reason) *AdmissionDeniedError {
		return &AdmissionDeniedError{Outcome: policy.OutcomeRetry, Reason: r, RetryAfter: retryHint}
	}
	if active >= p.ConcurrentLimit {
		return retry(policy.ReasonConcurrency)
	}
	if !completion && active-activeCompletion >= p.ConcurrentLimit-p.CompletionReserved {
		return retry(policy.ReasonReservedCapacity)
	}
	if p.tokens >= 1-tokenEpsilon {
		return nil
	}
	wait := (1 - p.tokens) / p.StartRate * float64(time.Second)
	d := time.Duration(math.MaxInt64 / 2)
	if wait < float64(d) {
		d = time.Duration(math.Ceil(wait))
	}
	return &AdmissionDeniedError{Outcome: policy.OutcomeDefer, Reason: policy.ReasonRate, NotBefore: p.settledAt.Add(d)}
}

// startAttempt runs the shared reserve path: sweep overdue leases, refuse a task
// that already has a live attempt, settle and check the pool, debit one start
// and insert the attempt. A denial writes nothing but the overdue sweep.
func startAttempt(ctx context.Context, tx *sql.Tx, now time.Time, permit ResearchPermit, seq int, prev *string, ttl, retryHint time.Duration) (ResearchAttempt, *AdmissionDeniedError, error) {
	return reserveAttempt(ctx, tx, now, permit, seq, prev, ttl, retryHint, false)
}

// reserveAttempt is startAttempt with an observe switch. In observe mode a
// denial is hypothetical: the attempt is still inserted (the work really starts
// and occupies the pool, like policy.ModeObserve) but no start is debited, and
// the returned denial is what enforcement would have said.
func reserveAttempt(ctx context.Context, tx *sql.Tx, now time.Time, permit ResearchPermit, seq int, prev *string, ttl, retryHint time.Duration, observe bool) (ResearchAttempt, *AdmissionDeniedError, error) {
	p, err := loadPool(ctx, tx, permit.AccountID)
	if err != nil {
		return ResearchAttempt{}, nil, err
	}
	at := p.settle(now)
	if _, err := sweepOverdue(ctx, tx, at); err != nil {
		return ResearchAttempt{}, nil, err
	}

	var taskLive int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM research_attempt WHERE task_id = ? AND state = 'active'`, permit.TaskID).Scan(&taskLive); err != nil {
		return ResearchAttempt{}, nil, fmt.Errorf("failed to check task attempts: %w", err)
	}
	// In observe mode a live attempt on the task (typically a dispatch preserved
	// past submit) must not change the claim outcome: record the hypothetical busy
	// refusal and supersede the stale attempt so the claim proceeds as it would
	// with the policy disabled.
	var busy *AdmissionDeniedError
	if taskLive > 0 {
		if !observe {
			return ResearchAttempt{}, nil, ErrTaskBusy
		}
		if err := supersedeTaskAttempts(ctx, tx, permit.TaskID, at); err != nil {
			return ResearchAttempt{}, nil, err
		}
		busy = &AdmissionDeniedError{Outcome: policy.OutcomeRetry, Reason: policy.ReasonConcurrency, RetryAfter: retryHint}
	}

	active, activeCompletion, err := occupancy(ctx, tx, permit.AccountID)
	if err != nil {
		return ResearchAttempt{}, nil, err
	}
	denied := admit(p, active, activeCompletion, permit.Completion, retryHint)
	if denied != nil && !observe {
		return ResearchAttempt{}, denied, nil
	}

	if denied == nil {
		p.tokens = math.Max(0, p.tokens-1)
		if err := savePoolBucket(ctx, tx, p); err != nil {
			return ResearchAttempt{}, nil, err
		}
	}
	a := ResearchAttempt{
		ID: GenerateID(), PermitID: permit.ID, PreviousAttemptID: prev, SequenceNumber: seq,
		TaskID: permit.TaskID, AccountID: permit.AccountID, Completion: permit.Completion,
		State: AttemptActive, StartedAt: formatTS(at), ExpiresAt: formatTS(at.Add(ttl)),
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO research_attempt (id, permit_id, previous_attempt_id, sequence_number, task_id, account_id, completion, state, started_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'active', ?, ?)`,
		a.ID, a.PermitID, a.PreviousAttemptID, a.SequenceNumber, a.TaskID, a.AccountID, a.Completion, a.StartedAt, a.ExpiresAt)
	if err != nil {
		return ResearchAttempt{}, nil, fmt.Errorf("failed to insert research attempt: %w", err)
	}
	if denied == nil {
		denied = busy
	}
	return a, denied, nil
}

// supersedeTaskAttempts ends every live attempt on a task at now.
func supersedeTaskAttempts(ctx context.Context, tx *sql.Tx, taskID string, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT `+attemptCols+` FROM research_attempt WHERE task_id = ? AND state = 'active'`, taskID)
	if err != nil {
		return fmt.Errorf("failed to find live research attempts: %w", err)
	}
	var live []ResearchAttempt
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			rows.Close()
			return fmt.Errorf("failed to scan research attempt: %w", err)
		}
		live = append(live, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, a := range live {
		started, err := parseTS(a.StartedAt)
		if err != nil {
			return fmt.Errorf("invalid attempt timestamp: %w", err)
		}
		dur := max(now.Sub(started).Milliseconds(), 0)
		if _, err := tx.ExecContext(ctx, `
			UPDATE research_attempt SET state='expired', ended_at=?, exit_class=?, duration_ms=?
			WHERE id = ? AND state = 'active'`, formatTS(now), ExitCancelled, dur, a.ID); err != nil {
			return fmt.Errorf("failed to supersede research attempt: %w", err)
		}
	}
	return nil
}

// RequestResearchPermit admits one start. A request ID already recorded returns
// the original permit and attempt without charging, whatever state they are in,
// provided the binding matches (else ErrBindingMismatch). Otherwise a task with a
// live attempt is refused (ErrTaskBusy), and the pool must have capacity: a
// denial is an *AdmissionDeniedError matching ErrInsufficientCapacity. Admission
// debits one start from the persisted bucket.
func (s *sqliteStore) RequestResearchPermit(ctx context.Context, now time.Time, req PermitRequest) (PermitGrant, error) {
	switch {
	case req.RequestID == "" || req.TaskID == "" || req.ProjectID == "" || req.AgentID == "" || req.Model == "" || req.AccountID == "":
		return PermitGrant{}, invalidResearch("request ID, task, project, agent, model and account are required")
	case !req.Class.Paced():
		return PermitGrant{}, invalidResearch("work class %q is not paced research work", req.Class)
	case req.LeaseTTL <= 0:
		return PermitGrant{}, invalidResearch("lease TTL must be positive")
	}
	var grant PermitGrant
	err := s.withResearchTx(ctx, func(tx *sql.Tx, post *error) error {
		existing, err := getPermit(ctx, tx, "request_id", req.RequestID)
		if err == nil {
			if existing.TaskID != req.TaskID || existing.ProjectID != req.ProjectID || existing.AgentID != req.AgentID ||
				existing.Model != req.Model || existing.AccountID != req.AccountID || existing.Completion != req.Class.Completion() {
				return ErrBindingMismatch
			}
			cur, err := getAttempt(ctx, tx, existing.CurrentAttemptID)
			if err != nil {
				return err
			}
			grant = PermitGrant{Permit: existing, Attempt: cur, Replayed: true}
			return nil
		}
		if !errors.Is(err, ErrPermitNotFound) {
			return err
		}

		permit := ResearchPermit{
			ID: GenerateID(), RequestID: req.RequestID, TaskID: req.TaskID, ProjectID: req.ProjectID, AgentID: req.AgentID,
			Model: req.Model, AccountID: req.AccountID, Completion: req.Class.Completion(), CreatedAt: formatTS(now),
		}
		a, denied, err := startAttempt(ctx, tx, now, permit, 1, nil, req.LeaseTTL, req.RetryHint)
		if err != nil {
			return err
		}
		if denied != nil {
			*post = denied
			return nil
		}
		permit.CurrentAttemptID = a.ID
		_, err = tx.ExecContext(ctx, `
			INSERT INTO research_permit (id, request_id, task_id, project_id, agent_id, model, account_id, completion, current_attempt_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			permit.ID, permit.RequestID, permit.TaskID, permit.ProjectID, permit.AgentID, permit.Model, permit.AccountID, permit.Completion, permit.CurrentAttemptID, permit.CreatedAt)
		if err != nil {
			return fmt.Errorf("failed to insert research permit: %w", err)
		}
		grant = PermitGrant{Permit: permit, Attempt: a}
		return nil
	})
	if err != nil {
		return PermitGrant{}, err
	}
	return grant, nil
}

// StartNextResearchAttempt starts a retry or expired-lease reclaim on an
// existing permit. It is fenced by priorAttemptID, which must be the permit's
// current attempt, and that attempt must be finalized or expired (ErrAttemptLive
// otherwise). It charges a new start and checks capacity exactly like a fresh
// request, and never refunds the prior one. Replaying the call after it
// succeeded returns the attempt it created.
func (s *sqliteStore) StartNextResearchAttempt(ctx context.Context, now time.Time, permitID, priorAttemptID string, leaseTTL, retryHint time.Duration) (PermitGrant, error) {
	if leaseTTL <= 0 {
		return PermitGrant{}, invalidResearch("lease TTL must be positive")
	}
	var grant PermitGrant
	err := s.withResearchTx(ctx, func(tx *sql.Tx, post *error) error {
		permit, err := getPermit(ctx, tx, "id", permitID)
		if err != nil {
			return err
		}
		cur, err := getAttempt(ctx, tx, permit.CurrentAttemptID)
		if err != nil {
			return err
		}
		if cur.ID != priorAttemptID {
			if cur.PreviousAttemptID != nil && *cur.PreviousAttemptID == priorAttemptID {
				grant = PermitGrant{Permit: permit, Attempt: cur, Replayed: true}
				return nil
			}
			return ErrFenceMismatch
		}
		if cur.State == AttemptActive {
			expires, err := parseTS(cur.ExpiresAt)
			if err != nil {
				return fmt.Errorf("invalid attempt timestamp: %w", err)
			}
			if expires.After(now) {
				return ErrAttemptLive
			}
			if err := expireAttempt(ctx, tx, cur); err != nil {
				return err
			}
		}
		a, denied, err := startAttempt(ctx, tx, now, permit, cur.SequenceNumber+1, &cur.ID, leaseTTL, retryHint)
		if err != nil {
			return err
		}
		if denied != nil {
			*post = denied
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE research_permit SET current_attempt_id = ? WHERE id = ?`, a.ID, permit.ID); err != nil {
			return fmt.Errorf("failed to link research attempt: %w", err)
		}
		permit.CurrentAttemptID = a.ID
		grant = PermitGrant{Permit: permit, Attempt: a}
		return nil
	})
	if err != nil {
		return PermitGrant{}, err
	}
	return grant, nil
}

// fencedAttempt loads the permit's current attempt and verifies attemptID is it.
func fencedAttempt(ctx context.Context, tx *sql.Tx, permitID, attemptID string) (ResearchAttempt, error) {
	permit, err := getPermit(ctx, tx, "id", permitID)
	if err != nil {
		return ResearchAttempt{}, err
	}
	if attemptID == "" || attemptID != permit.CurrentAttemptID {
		return ResearchAttempt{}, ErrFenceMismatch
	}
	return getAttempt(ctx, tx, attemptID)
}

// RenewResearchAttempt extends a live attempt's lease to now+leaseTTL (never
// shortening it); renewing twice with the same arguments is a no-op. It fails
// with ErrFenceMismatch for anything but the permit's current attempt,
// ErrPermitFinalized after finalize, and ErrPermitExpired once the lease is
// past, which also persists the expiry. It does not look at the task's state.
func (s *sqliteStore) RenewResearchAttempt(ctx context.Context, now time.Time, permitID, attemptID string, leaseTTL time.Duration) (ResearchAttempt, error) {
	if leaseTTL <= 0 {
		return ResearchAttempt{}, invalidResearch("lease TTL must be positive")
	}
	var out ResearchAttempt
	err := s.withResearchTx(ctx, func(tx *sql.Tx, post *error) error {
		a, err := fencedAttempt(ctx, tx, permitID, attemptID)
		if err != nil {
			return err
		}
		switch a.State {
		case AttemptFinalized:
			*post = ErrPermitFinalized
			return nil
		case AttemptExpired:
			*post = ErrPermitExpired
			return nil
		}
		expires, err := parseTS(a.ExpiresAt)
		if err != nil {
			return fmt.Errorf("invalid attempt timestamp: %w", err)
		}
		if !expires.After(now) {
			if err := expireAttempt(ctx, tx, a); err != nil {
				return err
			}
			*post = ErrPermitExpired
			return nil
		}
		if next := now.Add(leaseTTL); next.After(expires) {
			a.ExpiresAt = formatTS(next)
			if _, err := tx.ExecContext(ctx, `UPDATE research_attempt SET expires_at = ? WHERE id = ? AND state = 'active'`, a.ExpiresAt, a.ID); err != nil {
				return fmt.Errorf("failed to renew research attempt: %w", err)
			}
		}
		out = a
		return nil
	})
	if err != nil {
		return ResearchAttempt{}, err
	}
	return out, nil
}

func validExitClass(c string) bool {
	switch c {
	case ExitCompleted, ExitFailed, ExitCancelled, ExitUnknown:
		return true
	}
	return false
}

// FinalizeResearchAttempt records the process outcome and releases occupancy.
// It is idempotent: replaying it for an already finalized current attempt
// returns the recorded outcome unchanged (a later exit class or usage is
// ignored). A superseded attempt gets ErrFenceMismatch, and a lease already past
// gets ErrPermitExpired (the expiry is persisted). usageTokens nil records
// unknown usage, which is distinct from zero. Duration is derived from the
// attempt's start to now.
func (s *sqliteStore) FinalizeResearchAttempt(ctx context.Context, now time.Time, permitID, attemptID, exitClass string, usageTokens *int64) (ResearchAttempt, error) {
	if !validExitClass(exitClass) {
		return ResearchAttempt{}, invalidResearch("exit class %q is not one of completed, failed, cancelled, unknown", exitClass)
	}
	if usageTokens != nil && *usageTokens < 0 {
		return ResearchAttempt{}, invalidResearch("usage must not be negative")
	}
	var out ResearchAttempt
	err := s.withResearchTx(ctx, func(tx *sql.Tx, post *error) error {
		a, err := fencedAttempt(ctx, tx, permitID, attemptID)
		if err != nil {
			return err
		}
		switch a.State {
		case AttemptFinalized:
			out = a
			return nil
		case AttemptExpired:
			*post = ErrPermitExpired
			return nil
		}
		expires, err := parseTS(a.ExpiresAt)
		if err != nil {
			return fmt.Errorf("invalid attempt timestamp: %w", err)
		}
		if !expires.After(now) {
			if err := expireAttempt(ctx, tx, a); err != nil {
				return err
			}
			*post = ErrPermitExpired
			return nil
		}
		started, err := parseTS(a.StartedAt)
		if err != nil {
			return fmt.Errorf("invalid attempt timestamp: %w", err)
		}
		dur := now.Sub(started).Milliseconds()
		if dur < 0 {
			dur = 0
		}
		ended := formatTS(now)
		if _, err := tx.ExecContext(ctx, `
			UPDATE research_attempt SET state='finalized', ended_at=?, exit_class=?, duration_ms=?, usage_tokens=?
			WHERE id = ? AND state = 'active'`, ended, exitClass, dur, usageTokens, a.ID); err != nil {
			return fmt.Errorf("failed to finalize research attempt: %w", err)
		}
		a.State, a.EndedAt, a.ExitClass, a.DurationMS, a.UsageTokens = AttemptFinalized, &ended, &exitClass, &dur, usageTokens
		out = a
		return nil
	})
	if err != nil {
		return ResearchAttempt{}, err
	}
	return out, nil
}

// ExpireResearchAttempts marks every attempt whose lease ended at or before now
// as expired, releasing its occupancy, and returns how many it expired. It is
// optional housekeeping: reserve, renew and finalize already treat overdue
// attempts as expired.
func (s *sqliteStore) ExpireResearchAttempts(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.withResearchTx(ctx, func(tx *sql.Tx, post *error) error {
		var err error
		n, err = sweepOverdue(ctx, tx, now)
		return err
	})
	return n, err
}

// GetResearchPermit returns a permit and its current attempt.
func (s *sqliteStore) GetResearchPermit(ctx context.Context, permitID string) (ResearchPermit, ResearchAttempt, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return ResearchPermit{}, ResearchAttempt{}, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	p, err := getPermit(ctx, tx, "id", permitID)
	if err != nil {
		return ResearchPermit{}, ResearchAttempt{}, err
	}
	a, err := getAttempt(ctx, tx, p.CurrentAttemptID)
	if err != nil {
		return ResearchPermit{}, ResearchAttempt{}, err
	}
	return p, a, nil
}
