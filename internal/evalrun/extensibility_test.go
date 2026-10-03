package evalrun

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"

	"github.com/boldfield/odonian/internal/evaluation"
	"github.com/boldfield/odonian/internal/store"
)

// A second, differently-shaped candidate joins through registration alone: its
// own account pool (concurrency-only, no start rate), its own usage units and
// its own observers. No file outside this test changes.
func TestSecondAdapterNeedsOnlyRegistration(t *testing.T) {
	h := newHarness(t, 2)
	h.pool(store.EvaluationPoolConfig{ID: "metered", StartRate: 10, BurstCapacity: 10, ConcurrentLimit: 4})
	h.pool(store.EvaluationPoolConfig{ID: "local-gpu", ConcurrencyOnly: true, ConcurrentLimit: 1})

	idA := ident("alpha", "model-a", "metered")
	idB := ident("beta", "model-b", "local-gpu")
	idB.Observers = evaluation.KnownNames("critic")
	idB.Tools = evaluation.KnownNames("web_fetch", "grep")
	idA2 := idA
	idA2.RuntimeVersion = "2.1"

	a := h.addCandidate("alpha", evaluation.FakeModeSuccess, idA, 5, withUsage(`{"tokens": 1200}`))
	b := h.addCandidate("beta", evaluation.FakeModeSuccess, idB, 5, withUsage(`{"gpu_seconds": 4.5, "critic_calls": 2}`))
	a2 := h.addCandidate("alpha-2-1", evaluation.FakeModeSuccess, idA2, 5, withUsage(`{"tokens": 900}`))
	if a.Digest() == a2.Digest() || a.Digest() == b.Digest() {
		t.Fatal("distinct candidate versions share a digest")
	}
	r, _, _ := h.runner()

	reports, err := r.RunCampaign(context.Background(), h.campaign.ID)
	if err != nil || len(reports) != 6 {
		t.Fatalf("reports = %d, err = %v", len(reports), err)
	}
	for _, rep := range reports {
		if rep.Kind != Kind(store.EvalExitCompleted) {
			t.Fatalf("report = %+v", rep)
		}
	}

	wantUsage := map[string]map[string]float64{
		a.ID:  {"tokens": 1200},
		b.ID:  {"gpu_seconds": 4.5, "critic_calls": 2},
		a2.ID: {"tokens": 900},
	}
	for _, rep := range reports {
		d := h.detail(rep.Attempts[0].AttemptID)
		if !reflect.DeepEqual(d.Usage, wantUsage[rep.CandidateID]) {
			t.Errorf("candidate %s usage = %v, want %v", rep.CandidateID, d.Usage, wantUsage[rep.CandidateID])
		}
		stored, err := h.st.GetEvaluationCandidate(context.Background(), rep.CandidateID)
		if err != nil {
			t.Fatal(err)
		}
		if d.CandidateDigest != stored.Digest() || d.EffectiveIdentity == nil || d.EffectiveIdentity.AccountPool != stored.AccountPoolID() ||
			d.EffectiveIdentity.RuntimeVersion != stored.Config.Identity().RuntimeVersion {
			t.Errorf("candidate %s detail lost its settings: %+v", rep.CandidateID, d)
		}
	}
	if d := h.detail(reports[2].Attempts[0].AttemptID); !reflect.DeepEqual(d.EffectiveIdentity.Observers, idB.Observers) {
		t.Errorf("observers not preserved: %+v", d.EffectiveIdentity)
	}

	sums := GroupByCandidate(reports)
	if len(sums) != 3 {
		t.Fatalf("summaries = %d, want one per candidate version, got %+v", len(sums), sums)
	}
	for _, s := range sums {
		if s.Samples != 2 || s.Attempts != 2 || s.Kinds[Kind(store.EvalExitCompleted)] != 2 || len(s.EffectiveDigests) != 1 ||
			s.EffectiveDigests[0] != s.CandidateDigest {
			t.Errorf("summary = %+v", s)
		}
	}
	if sums[0].Declared.RuntimeVersion != "2.0" || sums[2].Declared.RuntimeVersion != "2.1" {
		t.Errorf("versions of one adapter were merged: %+v", sums)
	}
}

func TestOneBlockedCandidateDoesNotStarveTheOthers(t *testing.T) {
	h := newHarness(t, 2)
	h.openPool("pool-a")
	h.addCandidate("alpha", evaluation.FakeModeSuccess, ident("alpha", "model-a", "pool-a"), 5)
	h.addCandidate("beta", evaluation.FakeModeSuccess, ident("beta", "model-b", "no-such-pool"), 5)
	h.addCandidate("gamma", evaluation.FakeModeSuccess, ident("gamma", "model-c", "pool-a"), 5)
	r, _, _ := h.runner()

	reports, err := r.RunCampaign(context.Background(), h.campaign.ID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[Kind]int{}
	for _, rep := range reports {
		counts[rep.Kind]++
	}
	if counts[Kind(store.EvalExitCompleted)] != 4 || counts[KindPoolNotConfigured] != 1 || len(reports) != 5 {
		t.Fatalf("outcomes = %v", counts)
	}
}

func TestGroupByCandidateNeverMergesVersions(t *testing.T) {
	idA, idB := ident("alpha", "model-a", "p"), ident("alpha", "model-a", "p")
	idB.ModelRevision = "rev-2"
	cfgA, _ := evaluation.NewCandidateConfig(idA)
	cfgB, _ := evaluation.NewCandidateConfig(idB)
	rep := func(cfg evaluation.CandidateConfig, kind Kind) Report {
		return Report{CandidateDigest: cfg.Digest(), Declared: cfg.Identity(), Kind: kind}
	}
	sums := GroupByCandidate([]Report{rep(cfgA, "completed"), rep(cfgB, "failed"), rep(cfgA, "failed"), rep(cfgA, "completed")})
	if len(sums) != 2 || sums[0].Samples != 3 || sums[1].Samples != 1 ||
		sums[0].Kinds["completed"] != 2 || sums[0].Kinds["failed"] != 1 || sums[1].Kinds["failed"] != 1 {
		t.Fatalf("summaries = %+v", sums)
	}
}

// The runner finds candidates by identity digest and records what adapters
// report; it must not know any model, vendor CLI or runtime flag by name.
func TestCoreNamesNoModelOrRuntime(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v", err)
	}
	forbidden := regexp.MustCompile(`(?i)\b(muse|spark|claude|sonnet|opus|haiku|gpt|gemini|codex)\b|--pi\b|"pi"`)
	for _, f := range files {
		if filepath.Ext(f) != ".go" || len(f) > len("_test.go") && f[len(f)-len("_test.go"):] == "_test.go" {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := forbidden.Find(data); m != nil {
			t.Errorf("%s names %q; the core must stay model-agnostic", f, m)
		}
	}
}
