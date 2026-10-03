package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

// Evaluation pools are the admission side of evaluation: an account or compute
// pool owned by evaluation and persisted apart from research_pool, so an
// evaluation start can never spend or occupy production research allowance. They
// reuse the research admission primitives (token-bucket settle, admit and
// AdmissionDeniedError). Occupancy is derived from evaluation_attempt rows and
// is released only by finalize or expiry; a start is never refunded.

var ErrEvaluationPoolNotConfigured = errors.New("evaluation pool is not configured")

// EvaluationPoolConfig configures one evaluation pool. A rate pool needs
// StartRate and BurstCapacity; a ConcurrencyOnly pool (local compute) is bounded
// by ConcurrentLimit alone and has no bucket. Neither mode is ever unlimited.
type EvaluationPoolConfig struct {
	ID              string
	ConcurrencyOnly bool
	StartRate       float64 // sustained starts per second; rate pools only
	BurstCapacity   int     // rate pools only
	ConcurrentLimit int
}

// EvaluationPoolState is a pool's configuration with its settled allowance and
// live occupancy. Tokens is zero for concurrency-only pools.
type EvaluationPoolState struct {
	EvaluationPoolConfig
	Tokens    float64
	SettledAt string
	Active    int
}

func (c EvaluationPoolConfig) validate() error {
	switch {
	case c.ID == "":
		return fmt.Errorf("%w: pool ID must not be empty", ErrEvaluationInvalidInput)
	case c.ConcurrentLimit < 1:
		return fmt.Errorf("%w: concurrent limit must be at least 1", ErrEvaluationInvalidInput)
	case c.ConcurrencyOnly && (c.StartRate != 0 || c.BurstCapacity != 0):
		return fmt.Errorf("%w: a concurrency-only pool has no start rate or burst", ErrEvaluationInvalidInput)
	case !c.ConcurrencyOnly && (math.IsNaN(c.StartRate) || math.IsInf(c.StartRate, 0) || c.StartRate <= 0):
		return fmt.Errorf("%w: start rate must be finite and positive", ErrEvaluationInvalidInput)
	case !c.ConcurrencyOnly && c.BurstCapacity < 1:
		return fmt.Errorf("%w: burst capacity must be at least 1", ErrEvaluationInvalidInput)
	}
	return nil
}

func (c EvaluationPoolConfig) mode() string {
	if c.ConcurrencyOnly {
		return "concurrency_only"
	}
	return "rate"
}

type evalPoolRow struct {
	EvaluationPoolConfig
	tokens    float64
	settledAt time.Time
}

func loadEvaluationPool(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (*evalPoolRow, error) {
	var p evalPoolRow
	var mode, settled string
	var rate sql.NullFloat64
	var burst sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT id, mode, start_rate, burst_capacity, concurrent_limit, tokens, settled_at FROM evaluation_pool WHERE id = ?`, id).
		Scan(&p.ID, &mode, &rate, &burst, &p.ConcurrentLimit, &p.tokens, &settled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrEvaluationPoolNotConfigured
	}
	if err != nil {
		return nil, fmt.Errorf("load evaluation pool: %w", err)
	}
	p.ConcurrencyOnly = mode == "concurrency_only"
	p.StartRate = rate.Float64
	p.BurstCapacity = int(burst.Int64)
	if p.settledAt, err = parseTS(settled); err != nil {
		return nil, fmt.Errorf("invalid evaluation pool timestamp: %w", err)
	}
	return &p, nil
}

// poolRow adapts the evaluation pool to the research admission primitives. A
// concurrency-only pool presents one permanently available token and is never
// debited, so only its concurrent limit can deny a start.
func (p *evalPoolRow) poolRow() *poolRow {
	if p.ConcurrencyOnly {
		return &poolRow{
			ResearchPoolConfig: ResearchPoolConfig{AccountID: p.ID, StartRate: 1, BurstCapacity: 1, ConcurrentLimit: p.ConcurrentLimit},
			tokens:             1,
			settledAt:          p.settledAt,
		}
	}
	return &poolRow{
		ResearchPoolConfig: ResearchPoolConfig{AccountID: p.ID, StartRate: p.StartRate, BurstCapacity: p.BurstCapacity, ConcurrentLimit: p.ConcurrentLimit},
		tokens:             p.tokens,
		settledAt:          p.settledAt,
	}
}

func evaluationPoolOccupancy(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, poolID string, at time.Time) (int, error) {
	var active int
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_attempt WHERE account_pool_id = ? AND state = 'active' AND expires_at > ?`,
		poolID, formatTS(at)).Scan(&active)
	if err != nil {
		return 0, fmt.Errorf("count evaluation occupancy: %w", err)
	}
	return active, nil
}

// admitEvaluationStart decides and, on success, debits one start from the named
// pool inside tx. A missing pool is ErrEvaluationPoolNotConfigured, never
// implicit unlimited capacity. A denial is an *AdmissionDeniedError.
func admitEvaluationStart(ctx context.Context, tx *sql.Tx, now time.Time, poolID string, retryHint time.Duration) error {
	ep, err := loadEvaluationPool(ctx, tx, poolID)
	if err != nil {
		return err
	}
	p := ep.poolRow()
	at := now
	if !ep.ConcurrencyOnly {
		at = p.settle(now)
	}
	active, err := evaluationPoolOccupancy(ctx, tx, poolID, at)
	if err != nil {
		return err
	}
	if denied := admit(p, active, 0, true, retryHint); denied != nil {
		return denied
	}
	if ep.ConcurrencyOnly {
		return nil
	}
	p.tokens = math.Max(0, p.tokens-1)
	_, err = tx.ExecContext(ctx, `UPDATE evaluation_pool SET tokens = ?, settled_at = ?, updated_at = ? WHERE id = ?`,
		p.tokens, formatTS(p.settledAt), formatTS(p.settledAt), poolID)
	if err != nil {
		return fmt.Errorf("save evaluation pool: %w", err)
	}
	return nil
}

// ConfigureEvaluationPool creates a pool with a full burst, or updates an
// existing pool's limits without resetting anything: allowance is settled under
// the old rate and clamped to the new burst, and active attempts are untouched.
// A pool's mode cannot change after creation.
func (s *sqliteStore) ConfigureEvaluationPool(ctx context.Context, cfg EvaluationPoolConfig) (EvaluationPoolState, error) {
	if err := cfg.validate(); err != nil {
		return EvaluationPoolState{}, err
	}
	now := s.Now()
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return EvaluationPoolState{}, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var rate any
	var burst any
	tokens := 0.0
	if !cfg.ConcurrencyOnly {
		rate, burst, tokens = cfg.StartRate, cfg.BurstCapacity, float64(cfg.BurstCapacity)
	}

	existing, err := loadEvaluationPool(ctx, tx, cfg.ID)
	switch {
	case errors.Is(err, ErrEvaluationPoolNotConfigured):
		ts := formatTS(now)
		_, err = tx.ExecContext(ctx,
			`INSERT INTO evaluation_pool (id, mode, start_rate, burst_capacity, concurrent_limit, tokens, settled_at, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			cfg.ID, cfg.mode(), rate, burst, cfg.ConcurrentLimit, tokens, ts, ts, ts)
		if err != nil {
			return EvaluationPoolState{}, fmt.Errorf("create evaluation pool: %w", err)
		}
	case err != nil:
		return EvaluationPoolState{}, err
	default:
		if existing.ConcurrencyOnly != cfg.ConcurrencyOnly {
			return EvaluationPoolState{}, fmt.Errorf("%w: evaluation pool mode cannot change", ErrEvaluationInvalidInput)
		}
		settledAt := existing.settledAt
		if !cfg.ConcurrencyOnly {
			p := existing.poolRow()
			p.settle(now)
			tokens = math.Min(p.tokens, float64(cfg.BurstCapacity))
			settledAt = p.settledAt
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE evaluation_pool SET start_rate = ?, burst_capacity = ?, concurrent_limit = ?, tokens = ?, settled_at = ?, updated_at = ? WHERE id = ?`,
			rate, burst, cfg.ConcurrentLimit, tokens, formatTS(settledAt), formatTS(now), cfg.ID)
		if err != nil {
			return EvaluationPoolState{}, fmt.Errorf("update evaluation pool: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return EvaluationPoolState{}, fmt.Errorf("commit transaction: %w", err)
	}
	return s.GetEvaluationPool(ctx, cfg.ID)
}

// GetEvaluationPool reads a pool without changing it: allowance is settled to
// now in the returned view only, and occupancy counts attempts still live at now.
func (s *sqliteStore) GetEvaluationPool(ctx context.Context, id string) (EvaluationPoolState, error) {
	now := s.Now()
	ep, err := loadEvaluationPool(ctx, s.conn, id)
	if err != nil {
		return EvaluationPoolState{}, err
	}
	st := EvaluationPoolState{EvaluationPoolConfig: ep.EvaluationPoolConfig}
	p := ep.poolRow()
	if !ep.ConcurrencyOnly {
		p.settle(now)
		st.Tokens = p.tokens
	}
	st.SettledAt = formatTS(p.settledAt)
	if st.Active, err = evaluationPoolOccupancy(ctx, s.conn, id, now); err != nil {
		return EvaluationPoolState{}, err
	}
	return st, nil
}
