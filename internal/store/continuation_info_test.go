package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// continuationInfoManifest proposes three children: a research child depending on the parent, a
// build child depending on the first child, and an independent research child. Claim c4 is
// carried forward to an owner and c5 is excluded.
func continuationInfoManifest(parentID string) json.RawMessage {
	child := func(key, track, claim, file, deps string) string {
		return `{"key":"` + key + `","title":"Title ` + key + `","spec":"Spec ` + key + `","track":"` + track + `","model":"haiku","review_models":["opus","sonnet"],"agent_merge":false,"escalate":true,"claim_ids":["` + claim + `"],"source_start_points":["https://example.com/` + key + `"],"file_scope":["docs/` + file + `.md"],"acceptance_criteria":["` + key + ` verified"],"dependencies":` + deps + `}`
	}
	return json.RawMessage(`{"version":1,"parent_task_id":"` + parentID + `","children":[` +
		child("a", "research", "c1", "a", `[{"kind":"parent","ref":"`+parentID+`"}]`) + `,` +
		child("b", "build", "c2", "b", `[{"kind":"child","ref":"a"}]`) + `,` +
		child("c", "research", "c3", "c", `[]`) +
		`],"pending_candidates":[{"claim_id":"c1","disposition":"assigned"},{"claim_id":"c2","disposition":"assigned"},{"claim_id":"c3","disposition":"assigned"},{"claim_id":"c4","disposition":"carried_forward","owner":"alice"},{"claim_id":"c5","disposition":"excluded","reason":"out of scope"}]}`)
}

func TestContinuationView_PlannedThenCreated(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)
	parentID := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})

	// A task with no manifest or links has no continuation view at all.
	plain, err := store.CreateTasks(ctx, projID, []TaskInput{{Title: "Plain", Spec: "x", DocumentID: docID, Model: "haiku"}})
	if err != nil {
		t.Fatalf("CreateTasks: %v", err)
	}
	if got, err := store.GetTask(ctx, plain[0].ID); err != nil || got.Continuation != nil || got.FindingFollowUps != nil {
		t.Fatalf("plain task: err=%v continuation=%v follow-ups=%v, want nil/nil", err, got.Continuation, got.FindingFollowUps)
	}

	// Before the parent is submitted there is nothing planned.
	if got, _ := store.GetTask(ctx, parentID); got.Continuation != nil {
		t.Fatalf("unsubmitted parent has a continuation view: %+v", got.Continuation)
	}

	if _, err := submitWithManifest(store, ctx, parentID, continuationInfoManifest(parentID)); err != nil {
		t.Fatalf("submit with manifest: %v", err)
	}

	// --- Planned state: proposals are visible, nothing is created. ---
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	c := parent.Continuation
	if c == nil {
		t.Fatal("parent with a submitted manifest has no continuation view")
	}
	if len(parent.SubmissionManifests) != 1 || c.ManifestDigest != parent.SubmissionManifests[0].ManifestDigest {
		t.Errorf("continuation digest %q does not match the stored manifest digest", c.ManifestDigest)
	}
	if len(c.CreatedChildren) != 0 {
		t.Errorf("no child may exist before the merge, got %+v", c.CreatedChildren)
	}
	if n := helperCountChildren(t, store, ctx, parentID); n != 0 {
		t.Fatalf("%d children exist before the merge", n)
	}
	type planned struct{ key, state, status string }
	var got []planned
	for _, p := range c.ProposedChildren {
		got = append(got, planned{p.Key, p.InitialState, p.Status})
		if p.CreatedTaskID != "" {
			t.Errorf("proposal %s claims created task %s before the merge", p.Key, p.CreatedTaskID)
		}
	}
	want := []planned{{"a", "ready", "pending"}, {"b", "backlog", "pending"}, {"c", "ready", "pending"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("proposed children = %v, want %v", got, want)
	}
	if deps := c.ProposedChildren[1].Dependencies; len(deps) != 1 || deps[0].Kind != "child" || deps[0].Ref != "a" {
		t.Errorf("proposal b dependencies = %+v, want child:a", deps)
	}
	if len(c.DeferredClaims) != 1 || c.DeferredClaims[0] != (DeferredClaim{ClaimID: "c4", Owner: "alice"}) {
		t.Errorf("deferred claims = %+v, want c4 owned by alice", c.DeferredClaims)
	}
	if len(c.ExcludedClaims) != 1 || c.ExcludedClaims[0] != (ExcludedClaim{ClaimID: "c5", Reason: "out of scope"}) {
		t.Errorf("excluded claims = %+v, want c5 out of scope", c.ExcludedClaims)
	}
	if len(c.ActionItems) != 0 {
		t.Errorf("no legacy tasks exist yet, got action items %+v", c.ActionItems)
	}

	// --- A review-finding follow-up is a different thing from a continuation. ---
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	findings := json.RawMessage(`[{"id":"f1","severity":"P3","file":"a.md","line":10,"summary":"wrong footnote","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", findings)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	followUps := findResearchFollowUps(t, store, ctx, projID, parentID)
	if len(followUps) != 1 {
		t.Fatalf("expected 1 finding follow-up, got %d", len(followUps))
	}
	followUpID := followUps[0].ID

	// A held task depending on the parent, and an unheld one that is simply waiting.
	deps, err := store.CreateTasks(ctx, projID, []TaskInput{
		{Title: "Held dependent", Spec: "x", DocumentID: docID, Model: "haiku", DependsOn: []string{parentID}},
		{Title: "Waiting dependent", Spec: "x", DocumentID: docID, Model: "haiku", DependsOn: []string{parentID}},
	})
	if err != nil {
		t.Fatalf("CreateTasks dependents: %v", err)
	}
	if _, err := store.HoldTask(ctx, deps[0].ID); err != nil {
		t.Fatalf("HoldTask: %v", err)
	}

	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if parent.State != "approved" {
		t.Fatalf("parent state = %s, want approved", parent.State)
	}
	if len(parent.FindingFollowUps) != 1 || parent.FindingFollowUps[0].ID != followUpID || parent.FindingFollowUps[0].State != "backlog" {
		t.Errorf("finding follow-ups = %+v, want the backlog follow-up %s", parent.FindingFollowUps, followUpID)
	}
	c = parent.Continuation
	if len(c.CreatedChildren) != 0 || len(c.ProposedChildren) != 3 {
		t.Errorf("approved is not merged: created=%d proposed=%d", len(c.CreatedChildren), len(c.ProposedChildren))
	}
	for _, p := range c.ProposedChildren {
		if p.Status != "pending" || p.Key == "" || p.CreatedTaskID == followUpID {
			t.Errorf("proposal %+v must stay pending and never be the finding follow-up", p)
		}
	}
	actions := map[string]string{}
	for _, a := range c.ActionItems {
		actions[a.TaskID] = a.Type
		if !strings.Contains(a.Description, "manual") {
			t.Errorf("action item %+v does not tell the operator to act manually", a)
		}
	}
	wantActions := map[string]string{followUpID: "legacy_held_follow_up", deps[0].ID: "held_dependent"}
	if !reflect.DeepEqual(actions, wantActions) {
		t.Errorf("action items = %v, want %v (the unheld dependent must not appear)", actions, wantActions)
	}

	// --- The human merge creates the children. ---
	if _, err := store.TransitionTask(ctx, parentID, "done", nil); err != nil {
		t.Fatalf("merge (approved -> done): %v", err)
	}
	parent, err = store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	c = parent.Continuation
	if len(c.CreatedChildren) != 3 {
		t.Fatalf("created children = %d, want 3: %+v", len(c.CreatedChildren), c.CreatedChildren)
	}
	for _, f := range parent.FindingFollowUps {
		for _, ch := range c.CreatedChildren {
			if ch.ID == f.ID {
				t.Errorf("finding follow-up %s listed as a created continuation child", f.ID)
			}
		}
	}
	byKey := map[string]CreatedChild{}
	for i, ch := range c.CreatedChildren {
		byKey[ch.Key] = ch
		if ch.Key != []string{"a", "b", "c"}[i] {
			t.Errorf("created child %d is key %q, want manifest order a,b,c", i, ch.Key)
		}
		if ch.ParentTaskID != parentID || ch.ManifestDigest != c.ManifestDigest {
			t.Errorf("child %s provenance parent=%s digest=%s, want %s / %s", ch.Key, ch.ParentTaskID, ch.ManifestDigest, parentID, c.ManifestDigest)
		}
	}
	a, b, cc := byKey["a"], byKey["b"], byKey["c"]
	if a.State != "ready" || a.DependencyStatus != "satisfied" || !a.Claimable || len(a.BlockedBy) != 0 {
		t.Errorf("child a (parent is done) = %+v, want ready/satisfied/claimable", a)
	}
	if b.State != "backlog" || b.DependencyStatus != "blocked" || b.Claimable || !reflect.DeepEqual(b.BlockedBy, []string{a.ID}) {
		t.Errorf("child b (build, depends on a) = %+v, want backlog/blocked by %s", b, a.ID)
	}
	if cc.State != "ready" || cc.DependencyStatus != "none" || !cc.Claimable {
		t.Errorf("child c = %+v, want ready/none/claimable", cc)
	}
	if !reflect.DeepEqual(a.ClaimIDs, []string{"c1"}) || !reflect.DeepEqual(a.FileScope, []string{"docs/a.md"}) {
		t.Errorf("child a metadata = %+v", a)
	}
	for _, p := range c.ProposedChildren {
		if p.Status != "created" || p.CreatedTaskID != byKey[p.Key].ID {
			t.Errorf("proposal %s = %+v, want created as %s", p.Key, p, byKey[p.Key].ID)
		}
	}
	if len(c.DeferredClaims) != 1 || len(c.ExcludedClaims) != 1 {
		t.Errorf("claims dispositions lost after merge: %+v / %+v", c.DeferredClaims, c.ExcludedClaims)
	}
	// The legacy follow-up and held dependent still need manual replacement; the waiting dependent does not.
	if len(c.ActionItems) != 2 {
		t.Errorf("action items after merge = %+v, want the follow-up and the held dependent", c.ActionItems)
	}

	// Completing child a's dependency unblocks b without any server-side retargeting.
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'done' WHERE id = ?", a.ID); err != nil {
		t.Fatalf("force a done: %v", err)
	}
	parent, _ = store.GetTask(ctx, parentID)
	for _, ch := range parent.Continuation.CreatedChildren {
		if ch.Key == "b" && (ch.DependencyStatus != "satisfied" || len(ch.BlockedBy) != 0 || ch.State != "backlog" || ch.Claimable) {
			t.Errorf("child b after a is done = %+v, want satisfied dependencies but still backlog (not claimable)", ch)
		}
	}

	// --- Child-side view. ---
	child, err := store.GetTask(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetTask child: %v", err)
	}
	if child.Continuation == nil || child.Continuation.ParentInfo == nil {
		t.Fatalf("created child has no parent provenance: %+v", child.Continuation)
	}
	pi := child.Continuation.ParentInfo
	if pi.ID != parentID || pi.ChildKey != "b" || pi.ManifestDigest != c.ManifestDigest {
		t.Errorf("child parent info = %+v, want parent %s key b digest %s", pi, parentID, c.ManifestDigest)
	}
	if len(child.Continuation.CreatedChildren) != 0 || len(child.Continuation.ProposedChildren) != 0 {
		t.Errorf("a leaf child must not list children of its own: %+v", child.Continuation)
	}

	// The finding follow-up is a legacy follow-up, not a continuation child, from its own side too.
	fu, err := store.GetTask(ctx, followUpID)
	if err != nil {
		t.Fatalf("GetTask follow-up: %v", err)
	}
	if fu.Continuation != nil {
		t.Errorf("finding follow-up must have no continuation view, got %+v", fu.Continuation)
	}
}

func TestContinuationView_NotCreatedWhenParentFinishedWithoutChildren(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)
	parentID := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})
	if _, err := submitWithManifest(store, ctx, parentID, continuationInfoManifest(parentID)); err != nil {
		t.Fatalf("submit with manifest: %v", err)
	}
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task SET state = 'failed' WHERE id = ?", parentID); err != nil {
		t.Fatalf("force failed: %v", err)
	}
	parent, err := store.GetTask(ctx, parentID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	for _, p := range parent.Continuation.ProposedChildren {
		if p.Status != "not_created" {
			t.Errorf("proposal %s status = %s, want not_created for a failed parent", p.Key, p.Status)
		}
	}
	if len(parent.Continuation.CreatedChildren) != 0 {
		t.Errorf("a failed parent must not have created children")
	}
}

func TestContinuationView_CorruptStoredLinkIsAnError(t *testing.T) {
	store, ctx, projID, docID := newManifestStore(t)
	parentID := claimedManifestTask(t, store, ctx, projID, docID, TaskInput{})
	if _, err := submitWithManifest(store, ctx, parentID, continuationInfoManifest(parentID)); err != nil {
		t.Fatalf("submit with manifest: %v", err)
	}
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	submitResearchReview(t, store, ctx, opus, "opus-reviewer", "approve", json.RawMessage(`[]`))
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	if _, err := store.TransitionTask(ctx, parentID, "done", nil); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if _, err := store.Conn().ExecContext(ctx, "UPDATE task_link SET value = 'not json' WHERE kind = 'continuation_child_claim_ids'"); err != nil {
		t.Fatalf("corrupt link: %v", err)
	}
	if _, err := store.GetTask(ctx, parentID); err == nil {
		t.Fatal("GetTask must surface a corrupt provenance link instead of returning a partial view")
	}
}
