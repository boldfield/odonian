package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const optedInSpec = "Verify the claims.\n\n## Continuation manifest\nPropose follow-up children.\n"

func newManifestStore(t *testing.T) (Store, context.Context, string, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "manifest.db"), defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("failed to create document: %v", err)
	}
	return store, ctx, proj.ID, doc.ID
}

// claimedManifestTask creates, promotes and claims a task so it is ready to submit.
func claimedManifestTask(t *testing.T, store Store, ctx context.Context, projID, docID string, input TaskInput) string {
	t.Helper()
	input.DocumentID = docID
	input.Model = "haiku"
	if input.Title == "" {
		input.Title = "Verify claims"
	}
	if input.Spec == "" {
		input.Spec = optedInSpec
	}
	if input.Track == "" {
		input.Track = "research"
	}
	if input.ReviewModels == nil {
		input.ReviewModels = []string{"opus", "sonnet"}
	}
	tasks, err := store.CreateTasks(ctx, projID, []TaskInput{input})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}
	id := tasks[0].ID
	if _, err := store.PromoteTask(ctx, id); err != nil {
		t.Fatalf("failed to promote task: %v", err)
	}
	if _, err := store.ClaimTask(ctx, id, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to claim task: %v", err)
	}
	return id
}

// manifestJSON builds a valid manifest for parentID whose single child carries claimID.
func manifestJSON(parentID, claimID string) string {
	return `{"version":1,"parent_task_id":"` + parentID + `","children":[{"key":"c1","title":"Verify ` + claimID + `","spec":"Check ` + claimID + ` against primary sources","track":"research","model":"haiku","review_models":["opus","sonnet"],"agent_merge":false,"escalate":true,"claim_ids":["` + claimID + `"],"source_start_points":["https://example.com/a"],"file_scope":["docs/` + claimID + `.md"],"acceptance_criteria":["claim verified"],"dependencies":[{"kind":"parent","ref":"` + parentID + `"}]}],"pending_candidates":[{"claim_id":"` + claimID + `","disposition":"assigned"}]}`
}

func submitWithManifest(store Store, ctx context.Context, taskID string, manifest json.RawMessage) (TaskWithDepsAndLinks, error) {
	return store.SubmitTaskWithManifest(ctx, taskID, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#100"}}, 8, nil, nil, testUnlimitedResearchBudget, nil, nil, manifest)
}

func requireValidationCode(t *testing.T, err error, code string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected a ValidationError with code %s, got %v", code, err)
	}
	if ve.Code != code {
		t.Fatalf("expected validation code %s, got %s (%s)", code, ve.Code, ve.Message)
	}
}

// requireUnchanged asserts a rejected submission left the task exactly as it was.
func requireUnchanged(t *testing.T, store Store, ctx context.Context, taskID string, wantEvents int) {
	t.Helper()
	got, err := store.GetTask(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to get task: %v", err)
	}
	if got.State != "in_progress" || got.ReviewRound != 0 {
		t.Errorf("expected in_progress at review_round 0, got %s at round %d", got.State, got.ReviewRound)
	}
	if len(got.Links) != 0 {
		t.Errorf("expected no links, got %d", len(got.Links))
	}
	if len(got.SubmissionManifests) != 0 {
		t.Errorf("expected no stored manifests, got %d", len(got.SubmissionManifests))
	}
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	if len(events) != wantEvents {
		t.Errorf("expected %d events, got %d", wantEvents, len(events))
	}
}

func eventCount(t *testing.T, store Store, ctx context.Context, taskID string) int {
	t.Helper()
	events, err := store.ListEvents(ctx, taskID)
	if err != nil {
		t.Fatalf("failed to list events: %v", err)
	}
	return len(events)
}

func TestSubmissionManifest_RoundBinding(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)
	id := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})

	if _, err := submitWithManifest(store, ctx, id, json.RawMessage(manifestJSON(id, "claim-a"))); err != nil {
		t.Fatalf("round 1 submit failed: %v", err)
	}
	first, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.SubmissionManifests) != 1 {
		t.Fatalf("expected 1 stored manifest after round 1, got %d", len(first.SubmissionManifests))
	}
	r1 := first.SubmissionManifests[0]
	if r1.ReviewRound != 1 || r1.ParentTaskID != id || r1.ManifestDigest == "" || len(r1.ManifestJSON) == 0 {
		t.Fatalf("round 1 manifest not bound correctly: %+v", r1)
	}

	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, id, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"bad child","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	if parent, _ := store.GetTask(ctx, id); parent.State != "ready" {
		t.Fatalf("expected rejected round to bounce the task to ready, got %s", parent.State)
	}

	if _, err := store.ClaimTask(ctx, id, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("failed to reclaim: %v", err)
	}
	if _, err := submitWithManifest(store, ctx, id, json.RawMessage(manifestJSON(id, "claim-b"))); err != nil {
		t.Fatalf("round 2 submit failed: %v", err)
	}

	second, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.SubmissionManifests) != 2 {
		t.Fatalf("expected 2 stored manifests after rework, got %d", len(second.SubmissionManifests))
	}
	if got := second.SubmissionManifests[0]; got.ReviewRound != 1 || got.ManifestDigest != r1.ManifestDigest || string(got.ManifestJSON) != string(r1.ManifestJSON) {
		t.Errorf("round 1 evidence was mutated by the rework: before %+v, after %+v", r1, got)
	}
	r2 := second.SubmissionManifests[1]
	if r2.ReviewRound != 2 || r2.ManifestDigest == r1.ManifestDigest || !strings.Contains(string(r2.ManifestJSON), "claim-b") {
		t.Errorf("round 2 manifest not bound to the new round: %+v", r2)
	}
}

func TestSubmissionManifest_ReworkWithoutManifestKeepsEarlierRound(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)
	id := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})
	if _, err := submitWithManifest(store, ctx, id, json.RawMessage(manifestJSON(id, "claim-a"))); err != nil {
		t.Fatal(err)
	}
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, id, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "reject", json.RawMessage(`[{"id":"f1","severity":"P1","file":"a.md","line":1,"summary":"x","in_changed_text":true,"status":"new"}]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "reject", json.RawMessage(`[]`))
	resubmitResearchImplementTask(t, store, ctx, id)

	got, err := store.GetTask(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.SubmissionManifests) != 1 || got.SubmissionManifests[0].ReviewRound != 1 {
		t.Fatalf("expected only the round 1 manifest to remain, got %+v", got.SubmissionManifests)
	}
}

func TestSubmissionManifest_Rejections(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)

	// Every case submits a manifest on a task shaped as the case describes and must fail with
	// the given code, leaving the task exactly as it was.
	cases := []struct {
		name     string
		input    TaskInput
		manifest func(id string) string
		code     string
	}{
		{"malformed_json", TaskInput{}, func(string) string { return `{"version":1,` }, "INVALID_MANIFEST_JSON"},
		{"not_an_object", TaskInput{}, func(string) string { return `[]` }, "INVALID_MANIFEST_JSON"},
		{"unknown_field", TaskInput{}, func(id string) string {
			return strings.Replace(manifestJSON(id, "c"), `"version":1`, `"version":1,"extra":true`, 1)
		}, "INVALID_MANIFEST_JSON"},
		{"trailing_data", TaskInput{}, func(id string) string { return manifestJSON(id, "c") + ` {}` }, "INVALID_MANIFEST_JSON"},
		{"mismatched_parent", TaskInput{}, func(string) string { return manifestJSON("some-other-task", "c") }, "MISMATCHED_PARENT_TASK"},
		{"build_track", TaskInput{Track: "build"}, func(id string) string { return manifestJSON(id, "c") }, "MANIFEST_NOT_ALLOWED"},
		{"design_track", TaskInput{Track: "design"}, func(id string) string { return manifestJSON(id, "c") }, "MANIFEST_NOT_ALLOWED"},
		{"not_opted_in", TaskInput{Spec: "Verify the claims."}, func(id string) string { return manifestJSON(id, "c") }, "PARENT_NOT_OPTED_IN"},
		{"phrase_in_prose", TaskInput{Spec: "Do not add a continuation manifest here."}, func(id string) string { return manifestJSON(id, "c") }, "PARENT_NOT_OPTED_IN"},
		{"deeper_heading", TaskInput{Spec: "### continuation manifest\n"}, func(id string) string { return manifestJSON(id, "c") }, "PARENT_NOT_OPTED_IN"},
		{"mid_line_heading", TaskInput{Spec: "see ## continuation manifest later\n"}, func(id string) string { return manifestJSON(id, "c") }, "PARENT_NOT_OPTED_IN"},
		{"disallowed_child_model", TaskInput{}, func(id string) string {
			return strings.Replace(manifestJSON(id, "c"), `"model":"haiku"`, `"model":"gpt-9"`, 1)
		}, "UNKNOWN_MODEL"},
		{"disallowed_review_model", TaskInput{}, func(id string) string {
			return strings.Replace(manifestJSON(id, "c"), `"review_models":["opus","sonnet"]`, `"review_models":["opus","gpt-9"]`, 1)
		}, "UNKNOWN_REVIEW_MODEL"},
		{"disallowed_child_track", TaskInput{}, func(id string) string {
			return strings.Replace(manifestJSON(id, "c"), `"track":"research"`, `"track":"ops"`, 1)
		}, "UNKNOWN_TRACK"},
		{"unsupported_version", TaskInput{}, func(id string) string {
			return strings.Replace(manifestJSON(id, "c"), `"version":1`, `"version":2`, 1)
		}, "INVALID_VERSION"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := claimedManifestTask(t, store, ctx, projID, docID, tc.input)
			before := eventCount(t, store, ctx, id)
			_, err := submitWithManifest(store, ctx, id, json.RawMessage(tc.manifest(id)))
			requireValidationCode(t, err, tc.code)
			requireUnchanged(t, store, ctx, id, before)
		})
	}
}

func TestSubmissionManifest_ImplementKindOnly(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)
	id := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})
	if _, err := submitWithManifest(store, ctx, id, nil); err != nil {
		t.Fatal(err)
	}
	opus, _ := findResearchReviewTasks(t, store, ctx, projID, id, 1)
	if _, err := store.ClaimTask(ctx, opus.ID, "opus-reviewer", "opus", 5*time.Minute); err != nil {
		t.Fatal(err)
	}
	verdict := "approve"
	_, err := store.SubmitTaskWithManifest(ctx, opus.ID, "opus-reviewer", "notes", &verdict, []LinkInput{}, 8, nil, nil, testUnlimitedResearchBudget, json.RawMessage(`[]`), nil, json.RawMessage(manifestJSON(opus.ID, "c")))
	requireValidationCode(t, err, "MANIFEST_NOT_ALLOWED")
	got, err := store.GetTask(ctx, opus.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "in_progress" || len(got.SubmissionManifests) != 0 {
		t.Errorf("review task changed by rejected manifest: state %s, manifests %d", got.State, len(got.SubmissionManifests))
	}
}

func TestSubmissionManifest_CanonicalDigest(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)

	submit := func(raw func(id string) string) SubmissionManifest {
		id := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})
		if _, err := submitWithManifest(store, ctx, id, json.RawMessage(raw(id))); err != nil {
			t.Fatalf("submit failed: %v", err)
		}
		got, err := store.GetTask(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return got.SubmissionManifests[0]
	}

	// The same manifest as the compact form, with a pretty-printed, key-reordered body and
	// null lists where the compact form has empty ones.
	compact := func(id string) string {
		return strings.Replace(manifestJSON(id, "c"), `"dependencies":[{"kind":"parent","ref":"`+id+`"}]`, `"dependencies":[]`, 1)
	}
	reordered := func(id string) string {
		return `{
  "pending_candidates": [ {"disposition": "assigned", "claim_id": "c"} ],
  "children": [ {
    "dependencies": null,
    "acceptance_criteria": ["claim verified"],
    "file_scope": ["docs/c.md"],
    "source_start_points": ["https://example.com/a"],
    "claim_ids": ["c"],
    "escalate": true,
    "agent_merge": false,
    "review_models": ["opus", "sonnet"],
    "model": "haiku",
    "track": "research",
    "spec": "Check c against primary sources",
    "title": "Verify c",
    "key": "c1"
  } ],
  "parent_task_id": "` + id + `",
  "version": 1
}`
	}

	a, b := submit(compact), submit(reordered)
	if a.ParentTaskID == b.ParentTaskID {
		t.Fatal("expected two distinct parent tasks")
	}
	// Task ids differ, so compare after substituting each id for a placeholder.
	norm := func(m SubmissionManifest) string {
		return strings.ReplaceAll(string(m.ManifestJSON), m.ParentTaskID, "<id>")
	}
	if norm(a) != norm(b) {
		t.Errorf("canonical JSON differs:\n%s\n%s", a.ManifestJSON, b.ManifestJSON)
	}

	// The digest is the SHA-256 of exactly the stored canonical JSON.
	for _, m := range []SubmissionManifest{a, b} {
		sum := sha256Hex(string(m.ManifestJSON))
		if m.ManifestDigest != sum {
			t.Errorf("digest %s is not the SHA-256 of the stored JSON (%s)", m.ManifestDigest, sum)
		}
	}
	if strings.Contains(string(a.ManifestJSON), "\n") || strings.Contains(string(a.ManifestJSON), "  ") {
		t.Errorf("canonical JSON should be compact: %s", a.ManifestJSON)
	}
}

func TestSubmissionManifest_DigestEqualForSemanticallyIdenticalManifests(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)
	id := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})

	s := store.(*sqliteStore)
	compact := manifestJSON(id, "c")
	var generic map[string]any
	if err := json.Unmarshal([]byte(compact), &generic); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(generic, "", "    ") // map keys are emitted sorted, i.e. reordered
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.canonicalizeManifest(json.RawMessage(compact), id)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.canonicalizeManifest(pretty, id)
	if err != nil {
		t.Fatal(err)
	}
	if a.json != b.json || a.digest != b.digest {
		t.Errorf("semantically identical manifests differ:\n%s (%s)\n%s (%s)", a.json, a.digest, b.json, b.digest)
	}
}

func TestSubmissionManifest_LegacySubmitUnchanged(t *testing.T) {
	for name, manifest := range map[string]json.RawMessage{"absent": nil, "explicit_null": json.RawMessage(`null`)} {
		t.Run(name, func(t *testing.T) {
			store, ctx, projID, docID := newManifestStore(t)
			// A build task with no opt-in: a manifest is not involved at all.
			id := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{Track: "build", Spec: "Plain spec", ReviewModels: []string{"opus"}})
			got, err := submitWithManifest(store, ctx, id, manifest)
			if err != nil {
				t.Fatalf("legacy submit failed: %v", err)
			}
			if got.State != "review" || got.ReviewRound != 1 {
				t.Errorf("expected review at round 1, got %s at round %d", got.State, got.ReviewRound)
			}
			if got.SubmissionManifests == nil || len(got.SubmissionManifests) != 0 {
				t.Errorf("expected submission_manifests to be an empty list, got %#v", got.SubmissionManifests)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(raw), `"submission_manifests":[]`) {
				t.Errorf("expected submission_manifests:[] in JSON, got %s", raw)
			}
		})
	}

	// The long-standing SubmitTask entry point still works for an opted-in research task.
	store, ctx, projID, docID := newManifestStore(t)
	id := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})
	if _, err := store.SubmitTask(ctx, id, "agent-1", "Implemented", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("SubmitTask failed: %v", err)
	}
}

func TestSubmissionManifest_TrackSourceShared(t *testing.T) {
	for _, track := range []string{"build", "design", "research"} {
		if !validTracks[track] {
			t.Errorf("track %q missing from validTracks", track)
		}
	}
	store, ctx, projID, docID := newManifestStore(t)
	_, err := store.CreateTasks(ctx, projID, []TaskInput{{Title: "t", Spec: "s", DocumentID: docID, Model: "haiku", Track: "ops"}})
	requireValidationCode(t, err, "UNKNOWN_TRACK")
}

func TestSpecOptsIntoContinuations(t *testing.T) {
	for spec, want := range map[string]bool{
		"## continuation manifest":               true,
		"## Continuation Manifest":               true,
		"intro\n  ## CONTINUATION MANIFEST  \nx": true,
		"intro\r\n## continuation manifest\r\n":  true,
		"### continuation manifest":              false,
		"# continuation manifest":                false,
		"x ## continuation manifest":             false,
		"## continuation manifest later":         false,
		"a continuation manifest is optional":    false,
		"":                                       false,
	} {
		if got := specOptsIntoContinuations(spec); got != want {
			t.Errorf("specOptsIntoContinuations(%q) = %v, want %v", spec, got, want)
		}
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
