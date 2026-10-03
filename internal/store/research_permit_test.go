package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/policy"
)

var rt0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func rtAt(sec float64) time.Time { return rt0.Add(time.Duration(sec * float64(time.Second))) }

func openPermitStore(t *testing.T, path string) Store {
	t.Helper()
	s, err := Open(path, defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newPermitStore(t *testing.T) Store {
	return openPermitStore(t, filepath.Join(t.TempDir(), "permits.db"))
}

func poolCfg(account string, rate float64, burst, limit, reserved int) ResearchPoolConfig {
	return ResearchPoolConfig{AccountID: account, StartRate: rate, BurstCapacity: burst, ConcurrentLimit: limit, CompletionReserved: reserved}
}

func mustPool(t *testing.T, s Store, now time.Time, cfg ResearchPoolConfig) ResearchPoolState {
	t.Helper()
	st, err := s.ConfigureResearchPool(context.Background(), now, cfg)
	if err != nil {
		t.Fatalf("ConfigureResearchPool: %v", err)
	}
	return st
}

func req(id, task string) PermitRequest {
	return PermitRequest{RequestID: id, TaskID: task, ProjectID: "proj", AgentID: "agent", Model: "opus", AccountID: "acct",
		Class: policy.ResearchWrite, LeaseTTL: 10 * time.Second}
}

func mustGrant(t *testing.T, s Store, now time.Time, r PermitRequest) PermitGrant {
	t.Helper()
	g, err := s.RequestResearchPermit(context.Background(), now, r)
	if err != nil {
		t.Fatalf("RequestResearchPermit(%s): %v", r.RequestID, err)
	}
	return g
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got error %v, want %v", err, target)
	}
}

func poolState(t *testing.T, s Store, now time.Time) ResearchPoolState {
	t.Helper()
	st, err := s.GetResearchPool(context.Background(), now, "acct")
	if err != nil {
		t.Fatalf("GetResearchPool: %v", err)
	}
	return st
}

func TestResearchMigrationAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	ctx := context.Background()
	s := openPermitStore(t, path)
	for _, table := range []string{"research_pool", "research_permit", "research_attempt"} {
		var n int
		if err := s.Conn().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("table %s missing (n=%d err=%v)", table, n, err)
		}
	}

	mustPool(t, s, rt0, poolCfg("acct", 0.001, 2, 2, 0))
	g1 := mustGrant(t, s, rt0, req("r1", "t1"))
	g2 := mustGrant(t, s, rt0, req("r2", "t2"))
	if _, err := s.RequestResearchPermit(ctx, rt0, req("r3", "t3")); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("third start should be denied, got %v", err)
	}
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(1), g2.Permit.ID, g2.Attempt.ID, ExitCompleted, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2 := openPermitStore(t, path)
	var migrations int
	if err := s2.Conn().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&migrations); err != nil || migrations != 29 {
		t.Fatalf("migrations = %d, %v; want 29", migrations, err)
	}
	st := poolState(t, s2, rtAt(1))
	if st.Tokens >= 1 {
		t.Fatalf("restart refilled the bucket: tokens = %v", st.Tokens)
	}
	if st.Active != 1 {
		t.Fatalf("occupancy after reopen = %d, want 1", st.Active)
	}
	// Reapplying the same policy at startup must not reset anything.
	st = mustPool(t, s2, rtAt(2), poolCfg("acct", 0.001, 2, 2, 0))
	if st.Tokens >= 1 || st.Active != 1 {
		t.Fatalf("reconfigure at startup reset state: tokens=%v active=%d", st.Tokens, st.Active)
	}
	// Even with a free slot (g2 finalized), no allowance means no start.
	if _, err := s2.RequestResearchPermit(ctx, rtAt(2), req("r4", "t4")); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("start after restart should still be rate-denied, got %v", err)
	}
	replay := mustGrant(t, s2, rtAt(3), req("r1", "t1"))
	if !replay.Replayed || replay.Permit.ID != g1.Permit.ID || replay.Attempt.ID != g1.Attempt.ID {
		t.Fatalf("replay after reopen = %+v, want original %+v", replay, g1)
	}
	p, a, err := s2.GetResearchPermit(ctx, g2.Permit.ID)
	if err != nil || p.CurrentAttemptID != g2.Attempt.ID || a.State != AttemptFinalized || a.ExitClass == nil || *a.ExitClass != ExitCompleted {
		t.Fatalf("persisted permit/attempt = %+v %+v %v", p, a, err)
	}
}

func TestResearchConcurrentCompetingReservations(t *testing.T) {
	s := newPermitStore(t)
	const limit, contenders = 3, 12
	mustPool(t, s, rt0, poolCfg("acct", 0.001, 50, limit, 0))

	var wg sync.WaitGroup
	results := make([]error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = s.RequestResearchPermit(context.Background(), rt0, req(fmt.Sprintf("r%d", i), fmt.Sprintf("t%d", i)))
		}(i)
	}
	wg.Wait()
	ok, denied := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrInsufficientCapacity):
			denied++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != limit || denied != contenders-limit {
		t.Fatalf("admitted %d denied %d, want %d and %d", ok, denied, limit, contenders-limit)
	}
	if st := poolState(t, s, rt0); st.Active != limit || st.Tokens != 50-limit {
		t.Fatalf("state = active %d tokens %v", st.Active, st.Tokens)
	}
}

func TestResearchConcurrentSameTaskAdmitsOne(t *testing.T) {
	s := newPermitStore(t)
	mustPool(t, s, rt0, poolCfg("acct", 1, 50, 50, 0))
	const contenders = 8
	var wg sync.WaitGroup
	results := make([]error, contenders)
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = s.RequestResearchPermit(context.Background(), rt0, req(fmt.Sprintf("r%d", i), "same-task"))
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrTaskBusy):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("admitted %d for one task, want 1", ok)
	}
	if st := poolState(t, s, rt0); st.Tokens != 49 {
		t.Fatalf("tokens = %v, want exactly one debit", st.Tokens)
	}
}

func TestResearchConcurrentSameRequestReplays(t *testing.T) {
	s := newPermitStore(t)
	mustPool(t, s, rt0, poolCfg("acct", 1, 5, 5, 0))
	const callers = 8
	var wg sync.WaitGroup
	grants := make([]PermitGrant, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			grants[i], errs[i] = s.RequestResearchPermit(context.Background(), rt0, req("same", "t1"))
		}(i)
	}
	wg.Wait()
	replayed := 0
	for i := range grants {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if grants[i].Permit.ID != grants[0].Permit.ID || grants[i].Attempt.ID != grants[0].Attempt.ID {
			t.Fatalf("callers got different permits")
		}
		if grants[i].Replayed {
			replayed++
		}
	}
	if replayed != callers-1 {
		t.Fatalf("replayed = %d, want %d", replayed, callers-1)
	}
	if st := poolState(t, s, rt0); st.Tokens != 4 {
		t.Fatalf("tokens = %v, want a single debit", st.Tokens)
	}
}

func TestResearchRefillWithFakeTime(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 1, 1, 5, 0))
	g := mustGrant(t, s, rt0, req("r1", "t1"))
	if _, err := s.FinalizeResearchAttempt(ctx, rt0, g.Permit.ID, g.Attempt.ID, ExitCompleted, nil); err != nil {
		t.Fatal(err)
	}

	_, err := s.RequestResearchPermit(ctx, rtAt(0.4), req("r2", "t2"))
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || denied.Outcome != policy.OutcomeDefer || denied.Reason != policy.ReasonRate {
		t.Fatalf("want rate deferral, got %v", err)
	}
	if want := rtAt(1); !denied.NotBefore.Equal(want) {
		t.Fatalf("NotBefore = %v, want %v", denied.NotBefore, want)
	}
	// Refill is honored exactly at NotBefore; the same request ID then admits.
	mustGrant(t, s, rtAt(1), req("r2", "t2"))
	// Idle time refills only up to burst.
	if st := poolState(t, s, rtAt(1000)); st.Tokens != 1 {
		t.Fatalf("tokens after long idle = %v, want burst 1", st.Tokens)
	}
}

func TestResearchClockRollbackDoesNotRefill(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 1, 1, 5, 0))
	mustGrant(t, s, rtAt(100), req("r1", "t1"))
	if _, err := s.RequestResearchPermit(ctx, rtAt(50), req("r2", "t2")); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("rollback must not refill, got %v", err)
	}
	if st := poolState(t, s, rtAt(50)); st.Tokens != 0 {
		t.Fatalf("tokens = %v", st.Tokens)
	}
	// Time resumes from the high-water mark, not from the rolled-back time.
	if st := poolState(t, s, rtAt(100.5)); st.Tokens != 0.5 {
		t.Fatalf("tokens = %v, want 0.5", st.Tokens)
	}
}

func TestResearchExpiryReleasesOccupancy(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 100, 10, 1, 0))
	g := mustGrant(t, s, rt0, req("r1", "t1"))

	_, err := s.RequestResearchPermit(ctx, rtAt(5), req("r2", "t2"))
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || denied.Outcome != policy.OutcomeRetry || denied.Reason != policy.ReasonConcurrency || denied.RetryAfter != policy.DefaultConcurrencyRetry {
		t.Fatalf("want concurrency retry, got %v", err)
	}
	if denied.NotBefore != (time.Time{}) {
		t.Fatalf("concurrency denial must not invent a finish time")
	}

	// At the lease deadline the old attempt no longer occupies the pool.
	if st := poolState(t, s, rtAt(10)); st.Active != 0 {
		t.Fatalf("occupancy at expiry = %d", st.Active)
	}
	g2 := mustGrant(t, s, rtAt(10), req("r2", "t2"))
	_, old, err := s.GetResearchPermit(ctx, g.Permit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.State != AttemptExpired || old.ExitClass == nil || *old.ExitClass != ExitLeaseExpired || old.DurationMS == nil || *old.DurationMS != 10000 || old.UsageTokens != nil {
		t.Fatalf("expired attempt = %+v", old)
	}
	if g2.Attempt.State != AttemptActive {
		t.Fatalf("new attempt = %+v", g2.Attempt)
	}
}

func TestResearchExpireSweep(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 1, 10, 10, 0))
	mustGrant(t, s, rt0, req("r1", "t1"))
	long := req("r2", "t2")
	long.LeaseTTL = time.Hour
	mustGrant(t, s, rt0, long)

	n, err := s.ExpireResearchAttempts(ctx, rtAt(11))
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v; want 1", n, err)
	}
	if n, _ := s.ExpireResearchAttempts(ctx, rtAt(11)); n != 0 {
		t.Fatalf("second sweep expired %d", n)
	}
	if st := poolState(t, s, rtAt(11)); st.Active != 1 {
		t.Fatalf("active = %d", st.Active)
	}
}

func TestResearchSameRequestReplay(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 0.001, 3, 3, 0))
	g := mustGrant(t, s, rt0, req("r1", "t1"))
	tokens := poolState(t, s, rt0).Tokens

	r := mustGrant(t, s, rtAt(1), req("r1", "t1"))
	if !r.Replayed || r.Permit != g.Permit || r.Attempt.ID != g.Attempt.ID {
		t.Fatalf("replay = %+v, want %+v", r, g)
	}
	if got := poolState(t, s, rt0).Tokens; got != tokens {
		t.Fatalf("replay charged: tokens %v -> %v", tokens, got)
	}

	// Replay still recovers the original after finalize and after expiry.
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(2), g.Permit.ID, g.Attempt.ID, ExitFailed, nil); err != nil {
		t.Fatal(err)
	}
	if r := mustGrant(t, s, rtAt(3), req("r1", "t1")); !r.Replayed || r.Attempt.State != AttemptFinalized {
		t.Fatalf("replay after finalize = %+v", r)
	}

	mismatches := map[string]func(*PermitRequest){
		"task":    func(r *PermitRequest) { r.TaskID = "other" },
		"project": func(r *PermitRequest) { r.ProjectID = "other" },
		"agent":   func(r *PermitRequest) { r.AgentID = "other" },
		"model":   func(r *PermitRequest) { r.Model = "sonnet" },
		"pool":    func(r *PermitRequest) { r.AccountID = "other" },
		"class":   func(r *PermitRequest) { r.Class = policy.ResearchReview },
	}
	for name, mutate := range mismatches {
		r := req("r1", "t1")
		mutate(&r)
		if _, err := s.RequestResearchPermit(ctx, rtAt(4), r); !errors.Is(err, ErrBindingMismatch) {
			t.Errorf("%s mismatch: got %v, want ErrBindingMismatch", name, err)
		}
	}
}

func TestResearchRequestValidation(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 1, 1, 1, 0))

	bad := req("r", "t")
	bad.Class = policy.BuildWork
	if _, err := s.RequestResearchPermit(ctx, rt0, bad); !errors.Is(err, ErrInvalidResearchInput) {
		t.Errorf("build class: %v", err)
	}
	bad = req("r", "t")
	bad.LeaseTTL = 0
	if _, err := s.RequestResearchPermit(ctx, rt0, bad); !errors.Is(err, ErrInvalidResearchInput) {
		t.Errorf("zero ttl: %v", err)
	}
	bad = req("", "t")
	if _, err := s.RequestResearchPermit(ctx, rt0, bad); !errors.Is(err, ErrInvalidResearchInput) {
		t.Errorf("empty request id: %v", err)
	}
	bad = req("r", "t")
	bad.AccountID = "missing"
	if _, err := s.RequestResearchPermit(ctx, rt0, bad); !errors.Is(err, ErrPoolNotFound) {
		t.Errorf("missing pool: %v", err)
	}
	if st := poolState(t, s, rt0); st.Tokens != 1 {
		t.Errorf("failed requests debited: %v", st.Tokens)
	}
	for _, cfg := range []ResearchPoolConfig{poolCfg("", 1, 1, 1, 0), poolCfg("a", 0, 1, 1, 0), poolCfg("a", 1, 0, 1, 0), poolCfg("a", 1, 1, 0, 0), poolCfg("a", 1, 1, 1, 2), poolCfg("a", 1, 1, 1, -1)} {
		if _, err := s.ConfigureResearchPool(ctx, rt0, cfg); !errors.Is(err, ErrInvalidResearchInput) {
			t.Errorf("config %+v: %v", cfg, err)
		}
	}
}

func TestResearchStaleRenewAndFinalize(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 100, 10, 5, 0))
	g := mustGrant(t, s, rt0, req("r1", "t1"))
	p, a := g.Permit.ID, g.Attempt.ID

	// Wrong identity and unknown permit.
	if _, err := s.RenewResearchAttempt(ctx, rtAt(1), p, "nope", time.Minute); !errors.Is(err, ErrFenceMismatch) {
		t.Errorf("renew wrong attempt: %v", err)
	}
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(1), p, "nope", ExitCompleted, nil); !errors.Is(err, ErrFenceMismatch) {
		t.Errorf("finalize wrong attempt: %v", err)
	}
	if _, err := s.RenewResearchAttempt(ctx, rtAt(1), "missing", a, time.Minute); !errors.Is(err, ErrPermitNotFound) {
		t.Errorf("renew unknown permit: %v", err)
	}

	// Renewal extends but never shortens, and is idempotent.
	r1, err := s.RenewResearchAttempt(ctx, rtAt(5), p, a, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if want := formatTS(rtAt(65)); r1.ExpiresAt != want {
		t.Fatalf("expires = %s, want %s", r1.ExpiresAt, want)
	}
	r2, err := s.RenewResearchAttempt(ctx, rtAt(5), p, a, time.Minute)
	if err != nil || r2.ExpiresAt != r1.ExpiresAt {
		t.Fatalf("idempotent renew = %+v %v", r2, err)
	}
	r3, err := s.RenewResearchAttempt(ctx, rtAt(6), p, a, time.Second)
	if err != nil || r3.ExpiresAt != r1.ExpiresAt {
		t.Fatalf("renew must not shorten: %+v %v", r3, err)
	}

	// A lease past its deadline cannot be renewed or finalized, even before any sweep.
	if _, err := s.RenewResearchAttempt(ctx, rtAt(65), p, a, time.Minute); !errors.Is(err, ErrPermitExpired) {
		t.Fatalf("renew after expiry: %v", err)
	}
	if _, err := s.RenewResearchAttempt(ctx, rtAt(66), p, a, time.Minute); !errors.Is(err, ErrPermitExpired) {
		t.Fatalf("expiry must not be revivable: %v", err)
	}
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(66), p, a, ExitCompleted, nil); !errors.Is(err, ErrPermitExpired) {
		t.Fatalf("finalize after expiry: %v", err)
	}
	if _, cur, _ := s.GetResearchPermit(ctx, p); cur.State != AttemptExpired || *cur.ExitClass != ExitLeaseExpired {
		t.Fatalf("attempt = %+v", cur)
	}

	// Finalize is idempotent: the first recorded outcome wins and renew is refused afterwards.
	g2 := mustGrant(t, s, rtAt(100), req("r2", "t2"))
	use := int64(0)
	f1, err := s.FinalizeResearchAttempt(ctx, rtAt(104), g2.Permit.ID, g2.Attempt.ID, ExitCompleted, &use)
	if err != nil {
		t.Fatal(err)
	}
	other := int64(99)
	f2, err := s.FinalizeResearchAttempt(ctx, rtAt(105), g2.Permit.ID, g2.Attempt.ID, ExitFailed, &other)
	if err != nil {
		t.Fatalf("replayed finalize: %v", err)
	}
	if f1.State != f2.State || *f1.ExitClass != *f2.ExitClass || *f1.UsageTokens != *f2.UsageTokens || *f1.DurationMS != *f2.DurationMS || *f1.EndedAt != *f2.EndedAt {
		t.Fatalf("finalize replay changed the outcome: %+v vs %+v", f1, f2)
	}
	if *f2.ExitClass != ExitCompleted || *f2.UsageTokens != 0 || *f2.DurationMS != 4000 {
		t.Fatalf("recorded outcome = %+v", f2)
	}
	if _, err := s.RenewResearchAttempt(ctx, rtAt(106), g2.Permit.ID, g2.Attempt.ID, time.Minute); !errors.Is(err, ErrPermitFinalized) {
		t.Fatalf("renew after finalize: %v", err)
	}
}

func TestResearchFinalizePersistsDurationAndUsage(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 100, 10, 5, 0))

	unknown := mustGrant(t, s, rt0, req("r1", "t1"))
	zero := mustGrant(t, s, rt0, req("r2", "t2"))
	z := int64(0)
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(3.5), unknown.Permit.ID, unknown.Attempt.ID, ExitUnknown, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(2), zero.Permit.ID, zero.Attempt.ID, ExitCompleted, &z); err != nil {
		t.Fatal(err)
	}
	var dur int64
	var usage *int64
	var exit string
	row := func(id string) {
		t.Helper()
		if err := s.Conn().QueryRow(`SELECT duration_ms, usage_tokens, exit_class FROM research_attempt WHERE id=?`, id).Scan(&dur, &usage, &exit); err != nil {
			t.Fatal(err)
		}
	}
	row(unknown.Attempt.ID)
	if dur != 3500 || usage != nil || exit != ExitUnknown {
		t.Fatalf("unknown usage row = %d %v %s", dur, usage, exit)
	}
	row(zero.Attempt.ID)
	if dur != 2000 || usage == nil || *usage != 0 || exit != ExitCompleted {
		t.Fatalf("zero usage row = %d %v %s", dur, usage, exit)
	}

	g := mustGrant(t, s, rt0, req("r3", "t3"))
	if _, err := s.FinalizeResearchAttempt(ctx, rt0, g.Permit.ID, g.Attempt.ID, ExitLeaseExpired, nil); !errors.Is(err, ErrInvalidResearchInput) {
		t.Errorf("caller-supplied lease_expired: %v", err)
	}
	neg := int64(-1)
	if _, err := s.FinalizeResearchAttempt(ctx, rt0, g.Permit.ID, g.Attempt.ID, ExitCompleted, &neg); !errors.Is(err, ErrInvalidResearchInput) {
		t.Errorf("negative usage: %v", err)
	}
}

func TestResearchConservativeRetryCharging(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 0.001, 3, 3, 0))
	g := mustGrant(t, s, rt0, req("r1", "t1"))
	if got := poolState(t, s, rt0).Tokens; got != 2 {
		t.Fatalf("tokens after first start = %v", got)
	}

	if _, err := s.StartNextResearchAttempt(ctx, rtAt(1), g.Permit.ID, g.Attempt.ID, time.Minute, 0); !errors.Is(err, ErrAttemptLive) {
		t.Fatalf("retry while prior live: %v", err)
	}
	// An uncertain launch outcome is finalized without a refund.
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(1), g.Permit.ID, g.Attempt.ID, ExitUnknown, nil); err != nil {
		t.Fatal(err)
	}
	if got := poolState(t, s, rtAt(1)).Tokens; got > 2.01 {
		t.Fatalf("finalize refunded a start: tokens = %v", got)
	}

	n, err := s.StartNextResearchAttempt(ctx, rtAt(2), g.Permit.ID, g.Attempt.ID, time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n.Replayed || n.Attempt.SequenceNumber != 2 || n.Attempt.PreviousAttemptID == nil || *n.Attempt.PreviousAttemptID != g.Attempt.ID || n.Permit.CurrentAttemptID != n.Attempt.ID {
		t.Fatalf("next attempt = %+v", n)
	}
	if got := poolState(t, s, rtAt(2)).Tokens; got < 0.99 || got > 1.01 {
		t.Fatalf("retry should debit a second start: tokens = %v", got)
	}

	// An ambiguous transport retry of the same call is recovered, not charged again.
	again, err := s.StartNextResearchAttempt(ctx, rtAt(2), g.Permit.ID, g.Attempt.ID, time.Minute, 0)
	if err != nil || !again.Replayed || again.Attempt.ID != n.Attempt.ID {
		t.Fatalf("replayed next = %+v %v", again, err)
	}
	if got := poolState(t, s, rtAt(2)).Tokens; got > 1.01 {
		t.Fatalf("replay charged: tokens = %v", got)
	}

	// The superseded attempt is fenced off.
	if _, err := s.RenewResearchAttempt(ctx, rtAt(3), g.Permit.ID, g.Attempt.ID, time.Minute); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("stale renew: %v", err)
	}
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(3), g.Permit.ID, g.Attempt.ID, ExitCompleted, nil); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("stale finalize: %v", err)
	}
	if _, err := s.StartNextResearchAttempt(ctx, rtAt(3), g.Permit.ID, "bogus", time.Minute, 0); !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("next with wrong prior: %v", err)
	}
	if _, err := s.StartNextResearchAttempt(ctx, rtAt(3), "missing", g.Attempt.ID, time.Minute, 0); !errors.Is(err, ErrPermitNotFound) {
		t.Fatalf("next on unknown permit: %v", err)
	}
	// The current attempt still works.
	if _, err := s.RenewResearchAttempt(ctx, rtAt(3), n.Permit.ID, n.Attempt.ID, time.Minute); err != nil {
		t.Fatal(err)
	}

	// Retry after an expired lease is a new start and checks capacity.
	mustGrant(t, s, rtAt(3), req("spender", "t9"))
	if _, err := s.StartNextResearchAttempt(ctx, rtAt(70), n.Permit.ID, n.Attempt.ID, time.Minute, 0); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("retry with spent allowance: %v", err)
	}
	if _, cur, _ := s.GetResearchPermit(ctx, n.Permit.ID); cur.ID != n.Attempt.ID || cur.State != AttemptExpired {
		t.Fatalf("denied retry must keep the expired attempt current: %+v", cur)
	}
}

func TestResearchPolicyCapacityChange(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 0.001, 4, 4, 0))
	g1 := mustGrant(t, s, rt0, req("r1", "t1"))
	g2 := mustGrant(t, s, rt0, req("r2", "t2"))

	// Lowering limits and burst keeps live work and clamps allowance.
	st := mustPool(t, s, rtAt(1), poolCfg("acct", 0.001, 1, 1, 0))
	if st.Active != 2 || st.Tokens > 1 {
		t.Fatalf("after shrink: active=%d tokens=%v", st.Active, st.Tokens)
	}
	if _, err := s.RenewResearchAttempt(ctx, rtAt(2), g1.Permit.ID, g1.Attempt.ID, time.Minute); err != nil {
		t.Fatalf("shrink interrupted active work: %v", err)
	}
	if _, err := s.RequestResearchPermit(ctx, rtAt(2), req("r3", "t3")); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("over-limit start admitted: %v", err)
	}
	for _, g := range []PermitGrant{g1, g2} {
		if _, err := s.FinalizeResearchAttempt(ctx, rtAt(3), g.Permit.ID, g.Attempt.ID, ExitCompleted, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Raising capacity does not mint tokens, and keeps the high-water mark.
	mustGrant(t, s, rtAt(3), req("r4", "t4"))
	before := poolState(t, s, rtAt(3))
	after := mustPool(t, s, rtAt(3), poolCfg("acct", 0.001, 10, 10, 0))
	if after.Tokens != before.Tokens || after.SettledAt != before.SettledAt {
		t.Fatalf("raise changed allowance: %+v -> %+v", before, after)
	}
	if after.Active != 1 || after.BurstCapacity != 10 || after.ConcurrentLimit != 10 {
		t.Fatalf("after raise = %+v", after)
	}
	// An earlier time while reconfiguring must not refill either.
	back := mustPool(t, s, rtAt(1), poolCfg("acct", 100, 10, 10, 0))
	if back.Tokens != after.Tokens {
		t.Fatalf("reconfigure at earlier time refilled: %v -> %v", after.Tokens, back.Tokens)
	}
}

func TestResearchSubmissionIndependentOccupancy(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 100, 10, 1, 0))

	proj, err := s.CreateProject(ctx, "p", "r")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.CreateDocument(ctx, proj.ID, "design", "D", "DESIGN.md", nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.CreateTasks(ctx, proj.ID, []TaskInput{{Title: "t", Spec: "s", DocumentID: doc.ID, Model: "haiku", ReviewModels: []string{"opus", "sonnet"}}})
	if err != nil {
		t.Fatal(err)
	}
	taskID := tasks[0].ID
	if _, err := s.Conn().ExecContext(ctx, "UPDATE task SET state='ready' WHERE id=?", taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimTask(ctx, taskID, "agent", "haiku", time.Minute); err != nil {
		t.Fatal(err)
	}

	r := req("r1", taskID)
	r.ProjectID = proj.ID
	g := mustGrant(t, s, rt0, r)

	// The task leaves in_progress (submitted to review, then done) while the process still runs.
	sub, err := s.SubmitTask(ctx, taskID, "agent", "done", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 5, nil, nil, testUnlimitedResearchBudget)
	if err != nil || sub.State != "review" {
		t.Fatalf("submit: %v state=%s", err, sub.State)
	}
	check := func(label string, now time.Time) {
		t.Helper()
		if st := poolState(t, s, now); st.Active != 1 {
			t.Fatalf("%s: occupancy = %d, want 1", label, st.Active)
		}
		if _, err := s.RequestResearchPermit(ctx, now, req("other-"+label, "other-task-"+label)); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("%s: pool admitted past its limit: %v", label, err)
		}
	}
	check("review", rtAt(1))
	if _, err := s.Conn().ExecContext(ctx, "UPDATE task SET state='done' WHERE id=?", taskID); err != nil {
		t.Fatal(err)
	}
	check("done", rtAt(2))

	// Renewal still works after the task is gone from in_progress, until finalize.
	if _, err := s.RenewResearchAttempt(ctx, rtAt(3), g.Permit.ID, g.Attempt.ID, time.Minute); err != nil {
		t.Fatalf("renew after submit: %v", err)
	}
	if _, err := s.FinalizeResearchAttempt(ctx, rtAt(4), g.Permit.ID, g.Attempt.ID, ExitCompleted, nil); err != nil {
		t.Fatal(err)
	}
	if st := poolState(t, s, rtAt(4)); st.Active != 0 {
		t.Fatalf("occupancy after finalize = %d", st.Active)
	}
	mustGrant(t, s, rtAt(4), req("next", "next-task"))
}

func TestResearchCompletionReservation(t *testing.T) {
	s := newPermitStore(t)
	ctx := context.Background()
	mustPool(t, s, rt0, poolCfg("acct", 100, 10, 2, 1))
	mustGrant(t, s, rt0, req("w1", "t1"))

	_, err := s.RequestResearchPermit(ctx, rt0, req("w2", "t2"))
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || denied.Reason != policy.ReasonReservedCapacity || denied.Outcome != policy.OutcomeRetry {
		t.Fatalf("fresh work should hit reserved capacity, got %v", err)
	}
	rev := req("rv1", "t3")
	rev.Class = policy.ResearchReview
	g := mustGrant(t, s, rt0, rev)
	if !g.Permit.Completion || !g.Attempt.Completion {
		t.Fatalf("completion flag not persisted: %+v", g)
	}
	rev2 := req("rv2", "t4")
	rev2.Class = policy.ResearchRework
	if _, err := s.RequestResearchPermit(ctx, rt0, rev2); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("completion work must not exceed the total ceiling: %v", err)
	}
}

func TestResearchStoreMatchesPolicyEvaluator(t *testing.T) {
	ctx := context.Background()
	p := policy.Pool{Name: "p", AccountID: "acct", Models: []string{"opus"}, StartRate: 0.5, BurstCapacity: 2, ConcurrentDispatchLimit: 2, CompletionReserved: 1}
	ev, err := policy.New(rt0, policy.Config{Mode: policy.ModeEnforce, AllowedModels: []string{"opus"}, Pools: []policy.Pool{p}})
	if err != nil {
		t.Fatal(err)
	}
	s := newPermitStore(t)
	mustPool(t, s, rt0, poolCfg("acct", 0.5, 2, 2, 1))

	steps := []struct {
		at    float64
		class policy.WorkClass
	}{
		{0, policy.ResearchWrite}, {0, policy.ResearchWrite}, {0, policy.ResearchReview}, {1, policy.ResearchReview},
		{2, policy.ResearchWrite}, {3, policy.ResearchWrite}, {4, policy.ResearchRework}, {30, policy.ResearchWrite},
	}
	for i, st := range steps {
		want, err := ev.Evaluate(rtAt(st.at), "opus", st.class)
		if err != nil {
			t.Fatal(err)
		}
		r := req(fmt.Sprintf("r%d", i), fmt.Sprintf("t%d", i))
		r.Class = st.class
		r.LeaseTTL = time.Hour
		_, err = s.RequestResearchPermit(ctx, rtAt(st.at), r)
		var denied *AdmissionDeniedError
		switch {
		case err == nil:
			if want.Verdict.Outcome != policy.OutcomeAdmit {
				t.Fatalf("step %d: store admitted, policy said %+v", i, want.Verdict)
			}
		case errors.As(err, &denied):
			if denied.Outcome != want.Verdict.Outcome || denied.Reason != want.Verdict.Reason ||
				!denied.NotBefore.Equal(want.Verdict.NotBefore) || denied.RetryAfter != want.Verdict.RetryAfter {
				t.Fatalf("step %d: store %+v, policy %+v", i, denied, want.Verdict)
			}
		default:
			t.Fatalf("step %d: %v", i, err)
		}
	}
}

func TestResearchStoresUTCTimestamps(t *testing.T) {
	s := newPermitStore(t)
	zone := time.FixedZone("x", -5*3600)
	now := rt0.In(zone)
	mustPool(t, s, now, poolCfg("acct", 1, 1, 1, 0))
	g := mustGrant(t, s, now, req("r1", "t1"))
	if g.Attempt.StartedAt != formatTS(rt0) || g.Attempt.ExpiresAt != formatTS(rt0.Add(10*time.Second)) {
		t.Fatalf("timestamps not UTC fixed-width: %+v", g.Attempt)
	}
}
