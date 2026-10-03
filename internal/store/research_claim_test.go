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

type claimClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *claimClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *claimClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type claimFixture struct {
	t     *testing.T
	s     Store
	ctx   context.Context
	clock *claimClock
	proj  string
	doc   string
	path  string
}

const claimTTL = 10 * time.Second

func claimPolicy(mode policy.Mode, burst, limit, reserved int, models ...string) policy.Config {
	if len(models) == 0 {
		models = defaultTestAllowedModels()
	}
	return policy.Config{
		Mode:          mode,
		AllowedModels: defaultTestAllowedModels(),
		Pools: []policy.Pool{{
			Name: "pool", AccountID: "acct", Models: models,
			StartRate: 0.0001, BurstCapacity: burst, ConcurrentDispatchLimit: limit, CompletionReserved: reserved,
		}},
	}
}

func newClaimFixture(t *testing.T, cfg policy.Config) *claimFixture {
	t.Helper()
	f := &claimFixture{t: t, ctx: context.Background(), clock: &claimClock{t: rt0}, path: filepath.Join(t.TempDir(), "claim.db")}
	f.open(cfg)
	proj, err := f.s.CreateProject(f.ctx, "p", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	doc, err := f.s.CreateDocument(f.ctx, proj.ID, "feature_spec", "doc", "doc.md", nil)
	if err != nil {
		t.Fatalf("CreateDocument: %v", err)
	}
	f.proj, f.doc = proj.ID, doc.ID
	return f
}

func (f *claimFixture) open(cfg policy.Config) {
	f.t.Helper()
	s, err := Open(f.path, defaultTestAllowedModels(), WithClock(f.clock.Now))
	if err != nil {
		f.t.Fatalf("Open: %v", err)
	}
	f.t.Cleanup(func() { s.Close() })
	if err := s.SetResearchPolicy(f.ctx, f.clock.Now(), cfg); err != nil {
		f.t.Fatalf("SetResearchPolicy: %v", err)
	}
	f.s = s
}

func (f *claimFixture) task(track, model string, deps ...string) string {
	f.t.Helper()
	n := 0
	tasks, err := f.s.CreateTasks(f.ctx, f.proj, []TaskInput{{
		Title: fmt.Sprintf("task-%s-%d", track, time.Now().UnixNano()+int64(n)), Spec: "spec", DocumentID: f.doc,
		Model: model, Track: track, DependsOn: deps,
	}})
	if err != nil {
		f.t.Fatalf("CreateTasks: %v", err)
	}
	if _, err := f.s.PromoteTask(f.ctx, tasks[0].ID); err != nil {
		f.t.Fatalf("PromoteTask: %v", err)
	}
	return tasks[0].ID
}

func (f *claimFixture) research() string { return f.task("research", "opus") }

func (f *claimFixture) claim(requestID, taskID, agent string) (ResearchClaimResult, error) {
	return f.s.ClaimResearchTask(f.ctx, ResearchClaim{RequestID: requestID, TaskID: taskID, AgentID: agent, Model: "opus", LeaseTTL: claimTTL})
}

func (f *claimFixture) mustClaim(requestID, taskID, agent string) ResearchClaimResult {
	f.t.Helper()
	res, err := f.claim(requestID, taskID, agent)
	if err != nil {
		f.t.Fatalf("claim %s: %v", requestID, err)
	}
	return res
}

func (f *claimFixture) tokens() float64 {
	f.t.Helper()
	st, err := f.s.GetResearchPool(f.ctx, f.clock.Now(), "acct")
	if err != nil {
		f.t.Fatalf("GetResearchPool: %v", err)
	}
	return st.Tokens
}

func (f *claimFixture) active() int {
	f.t.Helper()
	st, err := f.s.GetResearchPool(f.ctx, f.clock.Now(), "acct")
	if err != nil {
		f.t.Fatalf("GetResearchPool: %v", err)
	}
	return st.Active
}

func (f *claimFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.s.Conn().QueryRow(query, args...).Scan(&n); err != nil {
		f.t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func (f *claimFixture) get(id string) TaskWithDepsAndLinks {
	f.t.Helper()
	tk, err := f.s.GetTask(f.ctx, id)
	if err != nil {
		f.t.Fatalf("GetTask: %v", err)
	}
	return tk
}

func (f *claimFixture) wantUntouched(id string) {
	f.t.Helper()
	tk := f.get(id)
	if tk.State != "ready" || tk.Assignee != nil || tk.LeaseExpiresAt != nil {
		f.t.Fatalf("task %s changed: state=%s assignee=%v lease=%v", id, tk.State, tk.Assignee, tk.LeaseExpiresAt)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE task_id = ? AND kind = 'claim'`, id); n != 0 {
		f.t.Fatalf("task %s has %d claim events", id, n)
	}
}

func wantConflictCode(t *testing.T, err error, code string) {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("got error %v, want ConflictError %s", err, code)
	}
}

func TestClaimResearchDenialLeavesTaskUntouched(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 1, 5, 0))
	a, b := f.research(), f.research()
	f.mustClaim("r-a", a, "agent-a")
	tokens := f.tokens()

	_, err := f.claim("r-b", b, "agent-b")
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || denied.Outcome != policy.OutcomeDefer || denied.Reason != policy.ReasonRate || denied.NotBefore.IsZero() {
		t.Fatalf("got %v, want a rate deferral with not-before", err)
	}
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("denial should match ErrInsufficientCapacity")
	}
	f.wantUntouched(b)
	if f.tokens() != tokens {
		t.Fatalf("denial changed the allowance")
	}
	if n := f.count(`SELECT COUNT(*) FROM research_permit WHERE task_id = ?`, b); n != 0 {
		t.Fatalf("denied task has %d permits", n)
	}

	// Repeated polling keeps exactly one bounded diagnostic row and writes no event.
	events := f.count(`SELECT COUNT(*) FROM event WHERE task_id = ?`, b)
	for i := 0; i < 3; i++ {
		if _, err := f.claim(fmt.Sprintf("r-b-%d", i), b, "agent-b"); !errors.Is(err, ErrInsufficientCapacity) {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM research_admission_diagnostic WHERE task_id = ?`, b); n != 1 {
		t.Fatalf("diagnostic rows = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE task_id = ?`, b); n != events {
		t.Fatalf("denial added task events")
	}
	d, err := f.s.GetResearchAdmissionDiagnostic(f.ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if d.DenialCount != 4 || d.Outcome != policy.OutcomeDefer || d.Reason != policy.ReasonRate || d.Hypothetical || d.NotBefore == nil {
		t.Fatalf("diagnostic = %+v", d)
	}

	// Once time alone allows it, the same task claims normally.
	f.clock.Advance(denied.NotBefore.Sub(f.clock.Now()) + time.Second)
	f.mustClaim("r-b-ok", b, "agent-b")
}

func TestClaimResearchDeniedConcurrencyAndReservation(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 10, 2, 1))
	w1, w2, rework := f.research(), f.research(), f.research()
	if _, err := f.s.Conn().Exec(`UPDATE task SET review_round = 1 WHERE id = ?`, rework); err != nil {
		t.Fatal(err)
	}

	f.mustClaim("r1", w1, "a")
	_, err := f.claim("r2", w2, "a")
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || denied.Outcome != policy.OutcomeRetry || denied.Reason != policy.ReasonReservedCapacity || denied.RetryAfter <= 0 {
		t.Fatalf("got %v, want reserved-capacity retry", err)
	}
	f.wantUntouched(w2)

	// Rework is completion work and may use the reserved slot; it counts as a start.
	before := f.tokens()
	res := f.mustClaim("r3", rework, "a")
	if res.Grant == nil || !res.Grant.Permit.Completion {
		t.Fatalf("rework grant = %+v", res.Grant)
	}
	if f.tokens() >= before {
		t.Fatalf("rework did not spend a start")
	}
	d, _ := f.s.GetResearchAdmissionDiagnostic(f.ctx, rework)
	if d.WorkClass != policy.ResearchRework {
		t.Fatalf("work class = %s", d.WorkClass)
	}

	_, err = f.claim("r4", w2, "a")
	if !errors.As(err, &denied) || denied.Reason != policy.ReasonConcurrency {
		t.Fatalf("got %v, want concurrency retry", err)
	}
}

func TestClaimResearchRaceAdmitsAtMostBurst(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 3, 50, 0))
	const n = 12
	ids := make([]string, n)
	for i := range ids {
		ids[i] = f.research()
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = f.claim(fmt.Sprintf("race-%d", i), ids[i], fmt.Sprintf("agent-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	granted := 0
	for _, err := range errs {
		switch {
		case err == nil:
			granted++
		case !errors.Is(err, ErrInsufficientCapacity):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if granted != 3 {
		t.Fatalf("granted = %d, want 3", granted)
	}
	if got := f.count(`SELECT COUNT(*) FROM task WHERE state = 'in_progress'`); got != 3 {
		t.Fatalf("in_progress tasks = %d, want 3", got)
	}
	if got := f.count(`SELECT COUNT(*) FROM research_attempt`); got != 3 {
		t.Fatalf("attempts = %d, want 3", got)
	}
	ready := 0
	for _, id := range ids {
		if f.get(id).State == "ready" {
			f.wantUntouched(id)
			ready++
		}
	}
	if ready != n-3 {
		t.Fatalf("ready tasks = %d, want %d", ready, n-3)
	}
	if tk := f.tokens(); tk > 1e-6 {
		t.Fatalf("tokens = %v, want exhausted", tk)
	}
}

func TestClaimResearchRaceOnOneTaskClaimsOnce(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 10, 50, 0))
	id := f.research()
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = f.claim(fmt.Sprintf("same-%d", i), id, fmt.Sprintf("agent-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()
	granted := 0
	for _, err := range errs {
		if err == nil {
			granted++
		} else if !errors.Is(err, ErrConflict) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if granted != 1 {
		t.Fatalf("granted = %d, want 1", granted)
	}
	if got := f.count(`SELECT COUNT(*) FROM research_attempt`); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
	if got := f.count(`SELECT COUNT(*) FROM event WHERE task_id = ? AND kind = 'claim'`, id); got != 1 {
		t.Fatalf("claim events = %d, want 1", got)
	}
	if tk := f.tokens(); tk < 8.99 || tk > 9.01 {
		t.Fatalf("tokens = %v, want one start spent", tk)
	}
}

func TestClaimResearchSameRequestRecoversOriginalAdmission(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	first := f.mustClaim("req-1", id, "agent-a")
	tokens := f.tokens()

	again := f.mustClaim("req-1", id, "agent-a")
	if again.Grant == nil || !again.Grant.Replayed || again.Grant.Attempt.ID != first.Grant.Attempt.ID || again.Grant.Permit.ID != first.Grant.Permit.ID {
		t.Fatalf("replay = %+v, want the original admission", again.Grant)
	}
	if again.Task.State != "in_progress" || again.Task.Assignee == nil || *again.Task.Assignee != "agent-a" {
		t.Fatalf("replayed task = %+v", again.Task)
	}
	if f.tokens() != tokens || f.count(`SELECT COUNT(*) FROM research_attempt`) != 1 {
		t.Fatalf("replay spent another start")
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE task_id = ? AND kind = 'claim'`, id); n != 1 {
		t.Fatalf("claim events = %d, want 1", n)
	}

	if _, err := f.claim("req-1", id, "agent-b"); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("other agent reusing the request: %v", err)
	}
	other := f.research()
	if _, err := f.claim("req-1", other, "agent-a"); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("other task reusing the request: %v", err)
	}
	f.wantUntouched(other)
}

func TestClaimResearchConcurrentSameRequestReplays(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	const n = 6
	var wg sync.WaitGroup
	results := make([]ResearchClaimResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = f.claim("one-request", id, "agent-a")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("call %d: %v", i, errs[i])
		}
		if results[i].Grant.Attempt.ID != results[0].Grant.Attempt.ID {
			t.Fatalf("calls produced different attempts")
		}
	}
	if got := f.count(`SELECT COUNT(*) FROM research_attempt`); got != 1 {
		t.Fatalf("attempts = %d, want 1", got)
	}
}

func TestClaimResearchReplayAfterReplacementConflicts(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	f.mustClaim("old", id, "agent-a")
	f.clock.Advance(claimTTL + time.Second)
	f.mustClaim("new", id, "agent-b")
	if _, err := f.claim("old", id, "agent-a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("replay of a replaced admission: %v, want ErrConflict", err)
	}
	if got := *f.get(id).Assignee; got != "agent-b" {
		t.Fatalf("assignee = %s", got)
	}
}

func TestClaimResearchFailedClaimDoesNotDebit(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	before := f.tokens()

	dep := f.task("build", "haiku")
	blocked := f.task("research", "opus", dep)
	held := f.research()
	if _, err := f.s.HoldTask(f.ctx, held); err != nil {
		t.Fatal(err)
	}
	mismatch := f.task("research", "sonnet")
	claimed := f.research()
	f.mustClaim("first", claimed, "agent-a")
	before = f.tokens()

	cases := []struct {
		name  string
		id    string
		model string
		check func(error) bool
	}{
		{"unfinished dependency", blocked, "opus", func(e error) bool { return errors.Is(e, ErrConflict) }},
		{"held", held, "opus", func(e error) bool { return errors.Is(e, ErrConflict) }},
		{"model mismatch", mismatch, "opus", func(e error) bool {
			var ce *ConflictError
			return errors.As(e, &ce) && ce.Code == "MODEL_MISMATCH"
		}},
		{"already claimed", claimed, "opus", func(e error) bool { return errors.Is(e, ErrConflict) }},
		{"missing", "does-not-exist", "opus", func(e error) bool { return errors.Is(e, ErrNotFound) }},
	}
	for i, c := range cases {
		_, err := f.s.ClaimResearchTask(f.ctx, ResearchClaim{RequestID: fmt.Sprintf("bad-%d", i), TaskID: c.id, AgentID: "agent-x", Model: c.model, LeaseTTL: claimTTL})
		if err == nil || !c.check(err) {
			t.Fatalf("%s: got %v", c.name, err)
		}
	}
	if f.tokens() != before {
		t.Fatalf("failed claims debited the pool")
	}
	if n := f.count(`SELECT COUNT(*) FROM research_attempt`); n != 1 {
		t.Fatalf("attempts = %d, want only the one real claim", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM research_permit`); n != 1 {
		t.Fatalf("permits = %d, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM research_admission_diagnostic`); n != 1 {
		t.Fatalf("a failed claim recorded an admission decision")
	}
}

func TestClaimResearchInputValidation(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	before := f.tokens()
	bad := []struct {
		name string
		req  ResearchClaim
	}{
		{"zero lease", ResearchClaim{RequestID: "r", TaskID: id, AgentID: "a", Model: "opus"}},
		{"negative lease", ResearchClaim{RequestID: "r", TaskID: id, AgentID: "a", Model: "opus", LeaseTTL: -time.Second}},
		{"unpaced class", ResearchClaim{RequestID: "r", TaskID: id, AgentID: "a", Model: "opus", Class: policy.BuildWork, LeaseTTL: claimTTL}},
		{"wrong class", ResearchClaim{RequestID: "r", TaskID: id, AgentID: "a", Model: "opus", Class: policy.ResearchReview, LeaseTTL: claimTTL}},
		{"wrong account", ResearchClaim{RequestID: "r", TaskID: id, AgentID: "a", Model: "opus", AccountID: "other", LeaseTTL: claimTTL}},
		{"no agent", ResearchClaim{RequestID: "r", TaskID: id, Model: "opus", LeaseTTL: claimTTL}},
	}
	for _, c := range bad {
		if _, err := f.s.ClaimResearchTask(f.ctx, c.req); !errors.Is(err, ErrInvalidResearchInput) {
			t.Fatalf("%s: got %v, want ErrInvalidResearchInput", c.name, err)
		}
	}
	f.wantUntouched(id)
	if f.tokens() != before {
		t.Fatalf("invalid input debited")
	}
	f.mustClaim("good", id, "a")
}

func TestClaimResearchLegacyClaimObeysCeiling(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 1, 5, 0))
	a, b := f.research(), f.research()
	if _, err := f.s.ClaimTask(f.ctx, a, "agent", "opus", claimTTL); err != nil {
		t.Fatalf("first legacy claim: %v", err)
	}
	if got := f.count(`SELECT COUNT(*) FROM research_attempt WHERE task_id = ? AND state = 'active'`, a); got != 1 {
		t.Fatalf("legacy claim created %d attempts, want 1", got)
	}
	_, err := f.s.ClaimTask(f.ctx, b, "agent", "opus", claimTTL)
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("second legacy claim: %v, want a denial", err)
	}
	f.wantUntouched(b)
}

func TestClaimResearchDisabledPreservesLegacyBehavior(t *testing.T) {
	f := newClaimFixture(t, policy.Config{Mode: policy.ModeDisabled})
	for i := 0; i < 5; i++ {
		id := f.research()
		res, err := f.s.ClaimTask(f.ctx, id, "agent", "opus", claimTTL)
		if err != nil || res.State != "in_progress" {
			t.Fatalf("claim %d: %v", i, err)
		}
	}
	id := f.research()
	res, err := f.claim("req", id, "agent")
	if err != nil || res.Grant != nil {
		t.Fatalf("explicit claim in disabled mode: %v grant=%v", err, res.Grant)
	}
	for _, table := range []string{"research_pool", "research_permit", "research_attempt", "research_admission_diagnostic"} {
		if n := f.count(`SELECT COUNT(*) FROM ` + table); n != 0 {
			t.Fatalf("%s has %d rows in disabled mode", table, n)
		}
	}
}

func TestClaimResearchObserveRecordsHypotheticalAndGrants(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeObserve, 1, 5, 0))
	a, b := f.research(), f.research()
	first := f.mustClaim("a", a, "agent")
	if first.Observed != nil {
		t.Fatalf("first claim observed a denial: %v", first.Observed)
	}
	second, err := f.claim("b", b, "agent")
	if err != nil {
		t.Fatalf("observe mode refused eligible work: %v", err)
	}
	if second.Observed == nil || second.Observed.Reason != policy.ReasonRate || second.Task.State != "in_progress" || second.Grant == nil {
		t.Fatalf("second claim = %+v", second)
	}
	d, err := f.s.GetResearchAdmissionDiagnostic(f.ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Hypothetical || d.Mode != policy.ModeObserve || d.Outcome != policy.OutcomeDefer || d.DenialCount != 1 {
		t.Fatalf("diagnostic = %+v", d)
	}
	if f.tokens() > 1e-6 {
		t.Fatalf("hypothetical denial must not be debited past empty, tokens=%v", f.tokens())
	}
	// A legacy claim without context is evaluated and recorded too.
	c := f.research()
	if _, err := f.s.ClaimTask(f.ctx, c, "agent", "opus", claimTTL); err != nil {
		t.Fatalf("legacy claim in observe mode: %v", err)
	}
	if d, err := f.s.GetResearchAdmissionDiagnostic(f.ctx, c); err != nil || !d.Hypothetical || d.Outcome != policy.OutcomeDefer {
		t.Fatalf("legacy observe diagnostic = %+v, %v", d, err)
	}
	// Observed work occupies the pool like real work.
	if f.active() != 3 {
		t.Fatalf("active = %d, want 3", f.active())
	}
}

func TestClaimResearchObserveUnmappedModelStillGranted(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeObserve, 1, 5, 0, "opus"))
	id := f.task("research", "sonnet")
	res, err := f.s.ClaimResearchTask(f.ctx, ResearchClaim{RequestID: "r", TaskID: id, AgentID: "a", Model: "sonnet", LeaseTTL: claimTTL})
	if err != nil || res.Grant != nil || res.Observed == nil || res.Observed.Outcome != policy.OutcomeUnmapped {
		t.Fatalf("got %+v, %v", res, err)
	}
	if res.Task.State != "in_progress" {
		t.Fatalf("task = %s", res.Task.State)
	}
}

func TestClaimNonResearchUnaffectedByPolicy(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 1, 1, 0))
	f.mustClaim("r", f.research(), "agent")
	for i := 0; i < 4; i++ {
		id := f.task("build", "opus")
		if _, err := f.s.ClaimTask(f.ctx, id, fmt.Sprintf("agent-%d", i), "opus", claimTTL); err != nil {
			t.Fatalf("build claim %d: %v", i, err)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM research_attempt`); n != 1 {
		t.Fatalf("build claims created attempts: %d", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM research_admission_diagnostic`); n != 1 {
		t.Fatalf("build claims recorded diagnostics: %d", n)
	}
	if _, err := f.s.ClaimTask(f.ctx, "missing", "agent", "opus", claimTTL); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing task: %v", err)
	}
}

func TestClaimResearchExpiredReclaimConsumesNewStart(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	first := f.mustClaim("r1", id, "agent-a")
	tokens := f.tokens()

	// While the lease is live nobody else can take the task.
	if _, err := f.claim("r-early", id, "agent-b"); !errors.Is(err, ErrConflict) {
		t.Fatalf("early reclaim: %v", err)
	}
	f.clock.Advance(claimTTL + time.Second)
	second := f.mustClaim("r2", id, "agent-b")
	if second.Grant.Replayed || second.Grant.Attempt.ID == first.Grant.Attempt.ID {
		t.Fatalf("reclaim reused the old attempt")
	}
	if second.Grant.Attempt.PreviousAttemptID == nil || *second.Grant.Attempt.PreviousAttemptID != first.Grant.Attempt.ID {
		t.Fatalf("reclaim attempt is not chained to the previous one: %+v", second.Grant.Attempt)
	}
	if got := f.tokens(); got > tokens-0.99 {
		t.Fatalf("tokens %v -> %v: reclaim did not spend a start", tokens, got)
	}
	_, old, err := f.s.GetResearchPermit(f.ctx, first.Grant.Permit.ID)
	if err != nil || old.State != AttemptExpired {
		t.Fatalf("old attempt = %+v, %v", old, err)
	}
	if got := *f.get(id).Assignee; got != "agent-b" {
		t.Fatalf("assignee = %s", got)
	}
}

func TestClaimResearchLiveAttemptBlocksReclaimWithoutDebit(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	first := f.mustClaim("r1", id, "agent-a")
	f.clock.Advance(claimTTL / 2)
	// The dispatch is renewed independently of the task lease and outlives it.
	if _, err := f.s.RenewResearchAttempt(f.ctx, f.clock.Now(), first.Grant.Permit.ID, first.Grant.Attempt.ID, time.Hour); err != nil {
		t.Fatalf("renew: %v", err)
	}
	f.clock.Advance(claimTTL)
	tokens := f.tokens()
	if _, err := f.claim("r2", id, "agent-b"); !errors.Is(err, ErrTaskBusy) {
		t.Fatalf("got %v, want ErrTaskBusy", err)
	}
	if f.tokens() != tokens || f.count(`SELECT COUNT(*) FROM research_attempt`) != 1 {
		t.Fatalf("busy claim changed pool state")
	}
	if got := *f.get(id).Assignee; got != "agent-a" {
		t.Fatalf("assignee = %s", got)
	}
}

func TestResearchHeartbeatRenewsAttemptWithoutSpendingStart(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 1, 0))
	a, b := f.research(), f.research()
	res := f.mustClaim("r1", a, "agent-a")
	tokens := f.tokens()

	for i := 0; i < 3; i++ {
		f.clock.Advance(claimTTL / 2)
		if _, err := f.s.HeartbeatTask(WithResearchAttempt(f.ctx, res.Grant.Attempt.ID), a, "agent-a", claimTTL); err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
	}
	// Well past the claim-time lease, the heartbeated attempt still holds the only slot.
	f.clock.Advance(claimTTL / 2)
	_, err := f.claim("r2", b, "agent-b")
	var denied *AdmissionDeniedError
	if !errors.As(err, &denied) || denied.Reason != policy.ReasonConcurrency {
		t.Fatalf("got %v, want concurrency denial while the first claim is heartbeating", err)
	}
	if got := f.tokens(); got < tokens-1e-3 {
		t.Fatalf("heartbeats spent allowance: %v -> %v", tokens, got)
	}
	if n := f.count(`SELECT COUNT(*) FROM research_attempt`); n != 1 {
		t.Fatalf("attempts = %d", n)
	}

	// A legacy heartbeat (no attempt ID) renews the attempt as well.
	f.clock.Advance(claimTTL * 4 / 10)
	if _, err := f.s.HeartbeatTask(f.ctx, a, "agent-a", claimTTL); err != nil {
		t.Fatalf("legacy heartbeat: %v", err)
	}
	f.clock.Advance(claimTTL / 2)
	if _, err := f.claim("r3", b, "agent-b"); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("legacy heartbeat did not keep the slot: %v", err)
	}
}

func TestResearchStaleAttemptCannotHeartbeatOrSubmit(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	first := f.mustClaim("r1", id, "agent-a")
	f.clock.Advance(claimTTL + time.Second)
	// The same agent identity reclaims under a new request: only the attempt ID tells the two apart.
	second := f.mustClaim("r2", id, "agent-a")
	leaseBefore := *f.get(id).LeaseExpiresAt

	oldCtx := WithResearchAttempt(f.ctx, first.Grant.Attempt.ID)
	_, err := f.s.HeartbeatTask(oldCtx, id, "agent-a", time.Hour)
	wantConflictCode(t, err, "ATTEMPT_FENCED")
	_, err = f.s.SubmitTask(oldCtx, id, "agent-a", "stale result", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 8, nil, nil, testUnlimitedResearchBudget)
	wantConflictCode(t, err, "ATTEMPT_FENCED")

	tk := f.get(id)
	if tk.State != "in_progress" || *tk.LeaseExpiresAt != leaseBefore || tk.Result != nil {
		t.Fatalf("stale operations changed the replacement: state=%s lease=%v result=%v", tk.State, tk.LeaseExpiresAt, tk.Result)
	}
	if _, cur, _ := f.s.GetResearchPermit(f.ctx, second.Grant.Permit.ID); cur.State != AttemptActive || cur.ExpiresAt != second.Grant.Attempt.ExpiresAt {
		t.Fatalf("replacement attempt changed: %+v", cur)
	}

	// A different agent claiming a research task cannot be impersonated either.
	_, err = f.s.HeartbeatTask(WithResearchAttempt(f.ctx, second.Grant.Attempt.ID), id, "agent-b", time.Hour)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong agent: %v", err)
	}

	// The current attempt works.
	newCtx := WithResearchAttempt(f.ctx, second.Grant.Attempt.ID)
	if _, err := f.s.HeartbeatTask(newCtx, id, "agent-a", claimTTL); err != nil {
		t.Fatalf("current heartbeat: %v", err)
	}
	if _, err := f.s.SubmitTask(newCtx, id, "agent-a", "real result", nil, []LinkInput{{Kind: "pr", Value: "#2"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("current submit: %v", err)
	}
}

func TestResearchHeartbeatRejectsExpiredAttempt(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 5, 0))
	id := f.research()
	res := f.mustClaim("r1", id, "agent-a")
	f.clock.Advance(claimTTL + time.Second)
	_, err := f.s.HeartbeatTask(WithResearchAttempt(f.ctx, res.Grant.Attempt.ID), id, "agent-a", claimTTL)
	wantConflictCode(t, err, "ATTEMPT_EXPIRED")
}

func TestResearchSubmitPreservesActiveDispatchPermit(t *testing.T) {
	f := newClaimFixture(t, claimPolicy(policy.ModeEnforce, 5, 1, 0))
	id, other := f.research(), f.research()
	res := f.mustClaim("r1", id, "agent-a")
	ctx := WithResearchAttempt(f.ctx, res.Grant.Attempt.ID)
	if _, err := f.s.SubmitTask(ctx, id, "agent-a", "done", nil, []LinkInput{{Kind: "pr", Value: "#9"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if f.get(id).State == "in_progress" {
		t.Fatalf("submit did not end task ownership")
	}
	_, att, err := f.s.GetResearchPermit(f.ctx, res.Grant.Permit.ID)
	if err != nil || att.State != AttemptActive {
		t.Fatalf("attempt after submit = %+v, %v", att, err)
	}
	if f.active() != 1 {
		t.Fatalf("submission released dispatch concurrency")
	}
	if _, err := f.claim("r2", other, "agent-b"); !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("a new start was admitted while the dispatch is active: %v", err)
	}
	if _, err := f.s.RenewResearchAttempt(f.ctx, f.clock.Now(), res.Grant.Permit.ID, res.Grant.Attempt.ID, claimTTL); err != nil {
		t.Fatalf("permit renewal after submit: %v", err)
	}
	if _, err := f.s.FinalizeResearchAttempt(f.ctx, f.clock.Now(), res.Grant.Permit.ID, res.Grant.Attempt.ID, ExitCompleted, nil); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	f.mustClaim("r3", other, "agent-b")
}

func TestResearchPolicyPersistsAcrossRestartWithoutRefill(t *testing.T) {
	cfg := claimPolicy(policy.ModeEnforce, 2, 5, 0)
	f := newClaimFixture(t, cfg)
	f.mustClaim("r1", f.research(), "a")
	f.mustClaim("r2", f.research(), "a")
	pending := f.research()

	f.s.Close()
	f.open(cfg)
	_, err := f.claim("r3", pending, "a")
	if !errors.Is(err, ErrInsufficientCapacity) {
		t.Fatalf("restart refilled the allowance: %v", err)
	}
	f.wantUntouched(pending)
	if n := f.count(`SELECT COUNT(*) FROM research_attempt WHERE state = 'active'`); n != 2 {
		t.Fatalf("active attempts after restart = %d", n)
	}
}
