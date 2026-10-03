package evaluation

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func censusOf(project string, clean, rejected int) FirstRoundCensus {
	c := FirstRoundCensus{Examined: clean + rejected + 3, Excluded: map[string]int{ExcludedNotSubmitted: 3}}
	for i := 0; i < clean; i++ {
		c.Records = append(c.Records, FirstRoundRecord{ProjectID: project, TaskID: fmt.Sprintf("%s-c%02d", project, i), ReviewRound: 1, Outcome: OutcomeClean, OutcomeReason: "no blocking findings"})
	}
	for i := 0; i < rejected; i++ {
		c.Records = append(c.Records, FirstRoundRecord{ProjectID: project, TaskID: fmt.Sprintf("%s-r%02d", project, i), ReviewRound: 1, Outcome: OutcomeRejectedMaterial, OutcomeReason: "1 P1 finding"})
	}
	return c
}

func taskIDs(m CohortManifest) []string {
	var ids []string
	for _, s := range m.Samples {
		ids = append(ids, s.TaskID)
	}
	return ids
}

func sealable(t *testing.T, m CohortManifest) CohortManifest {
	t.Helper()
	for i := range m.Samples {
		if m.Samples[i].Available {
			m.Samples[i].SubmittedSHA = strings.Repeat("a", 40)
			m.Samples[i].SnapshotDigest = strings.Repeat("b", 64)
		}
	}
	if err := m.Seal(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSelectCohortIsDeterministicWhateverTheCensusOrder(t *testing.T) {
	crit := CohortCriteria{ProjectIDs: []string{"p2", "p1"}, SampleCap: 6, Seed: "seed-1"}
	census := censusOf("p1", 8, 8)
	more := censusOf("p2", 4, 4)
	census.Records = append(census.Records, more.Records...)

	first, err := SelectCohort(crit, census)
	if err != nil {
		t.Fatal(err)
	}
	first = sealable(t, first)
	for i := 0; i < 20; i++ {
		shuffled := census
		shuffled.Records = append([]FirstRoundRecord(nil), census.Records...)
		rand.New(rand.NewSource(int64(i))).Shuffle(len(shuffled.Records), func(a, b int) {
			shuffled.Records[a], shuffled.Records[b] = shuffled.Records[b], shuffled.Records[a]
		})
		again, err := SelectCohort(CohortCriteria{ProjectIDs: []string{"p1", "p2"}, SampleCap: 6, Seed: "seed-1"}, shuffled)
		if err != nil {
			t.Fatal(err)
		}
		again = sealable(t, again)
		if again.Digest != first.Digest {
			t.Fatalf("shuffle %d changed the cohort digest: %v vs %v", i, taskIDs(again), taskIDs(first))
		}
	}
}

func TestSelectCohortSeedChangesSelection(t *testing.T) {
	census := censusOf("p1", 20, 20)
	a, _ := SelectCohort(CohortCriteria{ProjectIDs: []string{"p1"}, SampleCap: 6, Seed: "a"}, census)
	b, _ := SelectCohort(CohortCriteria{ProjectIDs: []string{"p1"}, SampleCap: 6, Seed: "b"}, census)
	if strings.Join(taskIDs(a), ",") == strings.Join(taskIDs(b), ",") {
		t.Fatalf("different seeds selected the same samples: %v", taskIDs(a))
	}
}

func TestSelectCohortStratifiesAndRedistributes(t *testing.T) {
	tests := []struct {
		name                         string
		clean, rejected, cap         int
		wantClean, wantRejected      int
		wantEligibleC, wantEligibleR int
	}{
		{"balanced", 6, 6, 5, 3, 2, 6, 6},
		{"clean short", 1, 6, 4, 1, 3, 1, 6},
		{"rejected short", 6, 0, 4, 4, 0, 6, 0},
		{"cap above eligible", 2, 1, 50, 2, 1, 2, 1},
		{"empty", 0, 0, 3, 0, 0, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := SelectCohort(CohortCriteria{ProjectIDs: []string{"p1"}, SampleCap: tc.cap, Seed: "s"}, censusOf("p1", tc.clean, tc.rejected))
			if err != nil {
				t.Fatal(err)
			}
			d := m.Denominators
			if d.SelectedClean != tc.wantClean || d.SelectedRejected != tc.wantRejected || d.EligibleClean != tc.wantEligibleC || d.EligibleRejected != tc.wantEligibleR {
				t.Fatalf("denominators = %+v", d)
			}
			if len(m.Samples) != tc.wantClean+tc.wantRejected || len(m.Samples) > tc.cap {
				t.Fatalf("selected %d samples, cap %d", len(m.Samples), tc.cap)
			}
			if d.Examined != tc.clean+tc.rejected+3 || d.Excluded[ExcludedNotSubmitted] != 3 {
				t.Fatalf("examined/excluded not carried: %+v", d)
			}
			for _, s := range m.Samples {
				if s.SelectionReason == "" || !strings.Contains(s.SelectionReason, string(s.Outcome)) {
					t.Fatalf("selection reason %q does not explain the outcome stratum", s.SelectionReason)
				}
			}
		})
	}
}

func TestSelectCohortRejectsBadInput(t *testing.T) {
	good := CohortCriteria{ProjectIDs: []string{"p1"}, SampleCap: 2, Seed: "s"}
	for name, mut := range map[string]func(*CohortCriteria){
		"no projects":   func(c *CohortCriteria) { c.ProjectIDs = nil },
		"dup project":   func(c *CohortCriteria) { c.ProjectIDs = []string{"p1", "p1"} },
		"blank project": func(c *CohortCriteria) { c.ProjectIDs = []string{" "} },
		"zero cap":      func(c *CohortCriteria) { c.SampleCap = 0 },
		"huge cap":      func(c *CohortCriteria) { c.SampleCap = MaxCohortCap + 1 },
		"no seed":       func(c *CohortCriteria) { c.Seed = "  " },
	} {
		c := good
		mut(&c)
		if _, err := SelectCohort(c, censusOf("p1", 2, 2)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	other := censusOf("p9", 1, 1)
	if _, err := SelectCohort(good, other); !errors.Is(err, ErrInvalid) {
		t.Errorf("unselected project: err = %v", err)
	}
	later := censusOf("p1", 1, 0)
	later.Records[0].ReviewRound = 2
	if _, err := SelectCohort(good, later); !errors.Is(err, ErrInvalid) {
		t.Errorf("round 2: err = %v", err)
	}
	odd := censusOf("p1", 1, 0)
	odd.Records[0].Outcome = "approved"
	if _, err := SelectCohort(good, odd); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad outcome: err = %v", err)
	}
}

func TestResolveSubmittedCommitNeverSubstitutes(t *testing.T) {
	sha := strings.Repeat("c", 40)
	tests := []struct {
		name     string
		links    []RecordedLink
		untagged int
		want     string
		reason   UnavailableReason
	}{
		{"one commit", []RecordedLink{{"pr", "https://x/o/r/pull/1"}, {"commit", sha}}, 0, sha, ""},
		{"same commit twice", []RecordedLink{{"commit", sha}, {"commit", sha}}, 0, sha, ""},
		{"none", []RecordedLink{{"pr", "https://x/o/r/pull/1"}, {"branch", "main"}}, 0, "", UnavailNoCommit},
		{"none but untagged", nil, 2, "", UnavailNoCommit},
		{"two commits", []RecordedLink{{"commit", sha}, {"commit", strings.Repeat("d", 40)}}, 0, "", UnavailAmbiguousCommit},
		{"abbreviated", []RecordedLink{{"commit", "abc1234"}}, 0, "", UnavailInvalidCommit},
		{"branch name", []RecordedLink{{"commit", "main"}}, 0, "", UnavailInvalidCommit},
		{"uppercase", []RecordedLink{{"commit", strings.Repeat("C", 40)}}, 0, "", UnavailInvalidCommit},
	}
	for _, tc := range tests {
		got, reason, detail := ResolveSubmittedCommit(tc.links, tc.untagged)
		if got != tc.want || reason != tc.reason {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, got, reason, tc.want, tc.reason)
		}
		if tc.reason != "" && detail == "" {
			t.Errorf("%s: no detail for an unavailable sample", tc.name)
		}
	}
}

func TestManifestSealVerifyAndTamper(t *testing.T) {
	m, err := SelectCohort(CohortCriteria{ProjectIDs: []string{"p1"}, SampleCap: 4, Seed: "s"}, censusOf("p1", 4, 4))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.MarkUnavailable(m.Samples[0].Key, UnavailNoCommit, "no commit link"); err != nil {
		t.Fatal(err)
	}
	m = sealable(t, m)
	if err := m.Verify(); err != nil {
		t.Fatal(err)
	}
	if m.Denominators.Unavailable != 1 || m.Denominators.Available != 3 || m.Denominators.UnavailableByReason[string(UnavailNoCommit)] != 1 {
		t.Fatalf("availability not accounted: %+v", m.Denominators)
	}
	if len(m.Samples) != 4 {
		t.Fatal("an unavailable sample must stay in the cohort")
	}

	enc, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeCohortManifest(enc)
	if err != nil || back.Digest != m.Digest {
		t.Fatalf("round trip: %v", err)
	}
	if s, ok := back.Sample(m.Samples[0].Key); !ok || s.Available || s.UnavailableReason != UnavailNoCommit {
		t.Fatalf("unavailability lost across persistence: %+v", s)
	}

	tampered := strings.Replace(enc, `"seed":"s"`, `"seed":"t"`, 1)
	if _, err := DecodeCohortManifest(tampered); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered manifest accepted: %v", err)
	}
	shaSwap := strings.Replace(enc, strings.Repeat("a", 40), strings.Repeat("e", 40), 1)
	if _, err := DecodeCohortManifest(shaSwap); !errors.Is(err, ErrInvalid) {
		t.Fatalf("substituted commit accepted: %v", err)
	}
	if _, err := DecodeCohortManifest("{"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("garbage accepted: %v", err)
	}
}

func TestManifestSealRejectsIncompleteSamples(t *testing.T) {
	m, _ := SelectCohort(CohortCriteria{ProjectIDs: []string{"p1"}, SampleCap: 2, Seed: "s"}, censusOf("p1", 2, 2))
	if err := m.Seal(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("available sample without a pinned commit sealed: %v", err)
	}
	m.Samples[0].Available = false
	for i := range m.Samples[1:] {
		m.Samples[i+1].SubmittedSHA, m.Samples[i+1].SnapshotDigest = "a", "b"
	}
	if err := m.Seal(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unavailable sample without a reason sealed: %v", err)
	}
	if err := m.MarkUnavailable("nope", UnavailNoCommit, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown sample: %v", err)
	}
	if _, err := m.Encode(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unsealed manifest encoded: %v", err)
	}
}

func TestRecordedManifestValid(t *testing.T) {
	body := []byte(`{"children":[]}`)
	good := RecordedManifest{JSON: body, Digest: "ea9dd1c1a6dc0a38f0c3e5cc9d0d8a6b0a2b6a3f2c8d6f3e5a1b6f6e3f7c1d28"}
	if good.Valid() {
		t.Fatal("wrong digest accepted")
	}
	good.Digest = digestHex(body)
	if !good.Valid() {
		t.Fatal("matching digest rejected")
	}
}

func digestHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
