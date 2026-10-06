package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/manifest"
)

const generatedTopicPriority int64 = 1002

type inheritanceFixture struct {
	*priorityFixture
}

func newInheritanceFixture(t *testing.T) *inheritanceFixture {
	return &inheritanceFixture{newPriorityFixture(t)}
}

func (f *inheritanceFixture) rawPriority(id string) int64 {
	f.t.Helper()
	var priority int64
	if err := f.store.Conn().QueryRowContext(f.ctx, `SELECT priority FROM task WHERE id = ?`, id).Scan(&priority); err != nil {
		f.t.Fatalf("read stored priority of %s: %v", id, err)
	}
	return priority
}

func (f *inheritanceFixture) requireRaw(label, id string, want int64) {
	f.t.Helper()
	if got := f.rawPriority(id); got != want {
		f.t.Fatalf("%s: stored priority = %d, want %d", label, got, want)
	}
}

// buildTask creates a promoted implement task through the public API.
func (f *inheritanceFixture) buildTask(track string, agentMerge bool) string {
	f.t.Helper()
	f.sequence++
	created, err := f.store.CreateTasks(f.ctx, f.projectID, []TaskInput{{
		Title: fmt.Sprintf("topic-%d", f.sequence), Spec: "spec", DocumentID: f.docID,
		Model: "haiku", Track: track, ReviewModels: []string{"opus"}, AgentMerge: agentMerge,
	}})
	if err != nil {
		f.t.Fatalf("create task: %v", err)
	}
	if _, err := f.store.PromoteTask(f.ctx, created[0].ID); err != nil {
		f.t.Fatalf("promote: %v", err)
	}
	return created[0].ID
}

// frontTo1002 fronts a decoy and then the topic, so the topic ends at 1002 by the
// server's own generation rule and never passes through manual validation.
func (f *inheritanceFixture) frontTo1002(taskID string) string {
	f.t.Helper()
	decoy := f.task("ready", 500)
	if got := f.front("decoy-front", decoy).Priority; got != 1001 {
		f.t.Fatalf("decoy front = %d, want 1001", got)
	}
	if got := f.front("topic-front", taskID).Priority; got != generatedTopicPriority {
		f.t.Fatalf("topic front = %d, want %d", got, generatedTopicPriority)
	}
	return decoy
}

func (f *inheritanceFixture) claimAndSubmit(taskID, agent, model, pr string) {
	f.t.Helper()
	if _, err := f.store.ClaimTask(f.ctx, taskID, agent, model, 5*time.Minute); err != nil {
		f.t.Fatalf("claim %s: %v", taskID, err)
	}
	var links []LinkInput
	if pr != "" {
		links = []LinkInput{{Kind: "pr", Value: pr}}
	}
	if _, err := f.store.SubmitTask(f.ctx, taskID, agent, "done", nil, links, 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		f.t.Fatalf("submit %s: %v", taskID, err)
	}
}

func (f *inheritanceFixture) reviewTask(targetID string, round int) string {
	f.t.Helper()
	var id string
	err := f.store.Conn().QueryRowContext(f.ctx,
		`SELECT id FROM task WHERE kind = 'review' AND target_task_id = ? AND review_round = ? AND adjudicate_finding_id IS NULL`,
		targetID, round).Scan(&id)
	if err != nil {
		f.t.Fatalf("expected review task for %s round %d: %v", targetID, round, err)
	}
	return id
}

func (f *inheritanceFixture) review(reviewID, verdict string, findings ...json.RawMessage) {
	f.t.Helper()
	if _, err := f.store.ClaimTask(f.ctx, reviewID, "reviewer", "opus", 5*time.Minute); err != nil {
		f.t.Fatalf("claim review: %v", err)
	}
	if _, err := f.store.SubmitTask(f.ctx, reviewID, "reviewer", "done", &verdict, nil, 8, nil, nil, testUnlimitedResearchBudget, findings...); err != nil {
		f.t.Fatalf("submit review verdict %s: %v", verdict, err)
	}
}

func (f *inheritanceFixture) mergeTask(targetID string) string {
	f.t.Helper()
	var id string
	if err := f.store.Conn().QueryRowContext(f.ctx, `SELECT id FROM task WHERE kind = 'merge' AND target_task_id = ?`, targetID).Scan(&id); err != nil {
		f.t.Fatalf("expected merge task for %s: %v", targetID, err)
	}
	return id
}

func (f *inheritanceFixture) insertContinuation(parentID, key string, priorityBlob string) string {
	f.t.Helper()
	child := testChild(key, "continuation "+key, "spec", "research", "haiku", key+".go")
	raw, err := json.Marshal(manifest.Manifest{
		Version: 1, ParentTaskID: parentID, Children: []manifest.Child{child},
		PendingCandidates: testPendingCandidates("claim1"),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	var m manifest.Manifest
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		f.t.Fatal(err)
	}
	if priorityBlob != "" {
		var injected any
		if err := json.Unmarshal([]byte(priorityBlob), &injected); err != nil {
			f.t.Fatal(err)
		}
		generic["priority"] = injected
		for _, c := range generic["children"].([]any) {
			c.(map[string]any)["priority"] = injected
		}
	}
	withPriority, err := json.Marshal(generic)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := json.Unmarshal(withPriority, &m); err != nil {
		f.t.Fatalf("manifest decode: %v", err)
	}
	if err := m.Validate(testManifestValidation, validTracks); err != nil {
		f.t.Fatalf("manifest validation: %v", err)
	}

	tx, err := f.store.Conn().BeginTx(f.ctx, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	ids, err := f.store.(*sqliteStore).InsertManifestChildren(f.ctx, tx, &m, "digest-"+key, parentID, f.projectID, f.docID, nowTimestamp())
	if err != nil {
		f.t.Fatalf("insert continuation children: %v", err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	if len(ids) != 1 {
		f.t.Fatalf("expected one continuation child, got %d", len(ids))
	}
	return ids[0]
}

func TestInheritedPriorityFutureReviewAndMergeAboveManualRange(t *testing.T) {
	f := newInheritanceFixture(t)
	task := f.buildTask("build", true)
	f.frontTo1002(task)
	f.claimAndSubmit(task, "worker", "haiku", "https://example.com/pr/1")

	review := f.reviewTask(task, 1)
	f.requireRaw("future review", review, generatedTopicPriority)

	f.review(review, "approve")
	f.requireRaw("merge task", f.mergeTask(task), generatedTopicPriority)
}

func TestInheritedPriorityExistingReviewFollowsLaterFront(t *testing.T) {
	f := newInheritanceFixture(t)
	task := f.buildTask("build", true)
	f.claimAndSubmit(task, "worker", "haiku", "https://example.com/pr/2")
	review := f.reviewTask(task, 1)
	f.requireRaw("review before front", review, DefaultPriority)

	f.frontTo1002(task)
	f.requireRaw("pre-existing review after front", review, generatedTopicPriority)

	f.review(review, "approve")
	f.requireRaw("merge task spawned after front", f.mergeTask(task), generatedTopicPriority)
}

func TestInheritedPriorityMergeReadsTopicNotStaleRow(t *testing.T) {
	f := newInheritanceFixture(t)
	anchor := f.buildTask("build", false)
	f.frontTo1002(anchor)

	// A pre-feature descendant whose own row still holds the old default.
	child := f.buildTask("build", true)
	f.exec(`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'continuation_parent', ?)`, GenerateID(), child, anchor)
	f.exec(`UPDATE task SET priority = ? WHERE id = ?`, DefaultPriority, child)
	if got := f.priorityOf(child); got != generatedTopicPriority {
		t.Fatalf("effective priority of stale descendant = %d, want %d", got, generatedTopicPriority)
	}

	f.claimAndSubmit(child, "worker", "haiku", "https://example.com/pr/3")
	review := f.reviewTask(child, 1)
	f.requireRaw("review of stale descendant", review, generatedTopicPriority)
	f.review(review, "approve")
	f.requireRaw("merge of stale descendant", f.mergeTask(child), generatedTopicPriority)
}

func TestInheritedPriorityRejectionReworkNewReviewRound(t *testing.T) {
	f := newInheritanceFixture(t)
	task := f.buildTask("build", false)
	f.frontTo1002(task)
	f.claimAndSubmit(task, "worker", "haiku", "https://example.com/pr/4")

	finding := json.RawMessage(`[{"id":"f1","severity":"P2","status":"new","summary":"fix","file":"x.go","line":1,"in_changed_text":false}]`)
	f.review(f.reviewTask(task, 1), "reject", finding)
	if got := f.get(task); got.State != "ready" {
		t.Fatalf("rejected task state = %s, want ready", got.State)
	}
	f.requireRaw("task after rejection", task, generatedTopicPriority)

	f.claimAndSubmit(task, "worker", "haiku", "https://example.com/pr/4")
	f.requireRaw("round 2 review", f.reviewTask(task, 2), generatedTopicPriority)
}

func TestInheritedPrioritySupersedeReplacement(t *testing.T) {
	f := newInheritanceFixture(t)
	task := f.buildTask("build", false)
	f.frontTo1002(task)

	replacement, err := f.store.SupersedeTask(f.ctx, task, nil)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	f.requireRaw("replacement", replacement.ID, generatedTopicPriority)
	if got := f.get(replacement.ID); got.TopicAnchorID != task || got.Priority != generatedTopicPriority {
		t.Fatalf("replacement anchor=%s priority=%d, want %s and %d", got.TopicAnchorID, got.Priority, task, generatedTopicPriority)
	}

	second, err := f.store.SupersedeTask(f.ctx, replacement.ID, nil)
	if err != nil {
		t.Fatalf("supersede replacement: %v", err)
	}
	f.requireRaw("second replacement", second.ID, generatedTopicPriority)
}

func TestInheritedPriorityAdjudicationSpawn(t *testing.T) {
	f := newInheritanceFixture(t)
	// Pre-feature shape: the disputed task's own row is stale while its anchor holds the topic value.
	anchor := f.buildTask("research", false)
	f.frontTo1002(anchor)
	task := f.buildTask("research", false)
	f.exec(`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'continuation_parent', ?)`, GenerateID(), task, anchor)
	f.exec(`UPDATE task SET priority = ? WHERE id = ?`, DefaultPriority, task)

	ss := f.store.(*sqliteStore)
	tx, err := ss.conn.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	parent := f.get(task)
	dispute := Dispute{FindingID: "f1", Evidence: "evidence", Round: 1, Lineage: researchReviewerLineage("opus", 0)}
	finding := Finding{ID: "f1", Severity: "P2", File: "a.md", Line: 3, Summary: "disputed", Status: "new"}
	if err := ss.spawnAdjudicationTask(f.ctx, tx, task, parent.ProjectID, parent.DocumentID, parent.Track, dispute, finding, 2, nowTimestamp()); err != nil {
		t.Fatalf("spawn adjudication: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var adjudicationID string
	if err := ss.conn.QueryRowContext(f.ctx, `SELECT id FROM task WHERE target_task_id = ? AND adjudicate_finding_id = 'f1'`, task).Scan(&adjudicationID); err != nil {
		t.Fatalf("find adjudication task: %v", err)
	}
	f.requireRaw("adjudication task", adjudicationID, generatedTopicPriority)
	if got := f.get(adjudicationID); got.TopicAnchorID != anchor {
		t.Fatalf("adjudication anchor = %s, want %s", got.TopicAnchorID, anchor)
	}
}

func TestInheritedPriorityTwoContinuationGenerations(t *testing.T) {
	f := newInheritanceFixture(t)
	root := f.buildTask("research", false)
	f.frontTo1002(root)

	child := f.insertContinuation(root, "gen1", "")
	f.requireRaw("first generation", child, generatedTopicPriority)
	grandchild := f.insertContinuation(child, "gen2", "")
	f.requireRaw("second generation", grandchild, generatedTopicPriority)

	for label, id := range map[string]string{"child": child, "grandchild": grandchild} {
		got := f.get(id)
		if got.Priority != generatedTopicPriority || got.TopicAnchorID != root {
			t.Fatalf("%s effective priority=%d anchor=%s, want %d and %s", label, got.Priority, got.TopicAnchorID, generatedTopicPriority, root)
		}
	}
}

func TestInheritedPriorityContinuationOfCompletedAnchor(t *testing.T) {
	f := newInheritanceFixture(t)
	root := f.buildTask("research", false)
	f.frontTo1002(root)
	f.exec(`UPDATE task SET state = 'done' WHERE id = ?`, root)

	child := f.insertContinuation(root, "after-done", "")
	f.requireRaw("continuation of done anchor", child, generatedTopicPriority)
	if got := f.get(child).TopicAnchorID; got != root {
		t.Fatalf("anchor = %s, want completed root %s", got, root)
	}
}

func TestManifestCannotOverridePriority(t *testing.T) {
	f := newInheritanceFixture(t)
	root := f.buildTask("research", false)
	f.frontTo1002(root)

	for i, injected := range []string{`1`, `1000`, `999999`, `"urgent"`, `{"p":1}`} {
		child := f.insertContinuation(root, fmt.Sprintf("override-%d", i), injected)
		f.requireRaw("manifest-supplied priority "+injected, child, generatedTopicPriority)
	}
}

func TestContinuationAtDefaultPriorityStaysDefault(t *testing.T) {
	f := newInheritanceFixture(t)
	root := f.buildTask("research", false)
	child := f.insertContinuation(root, "default", "")
	f.requireRaw("continuation at default", child, DefaultPriority)
}

func TestResetViaDescendantUpdatesWholeTopicAndFutureWork(t *testing.T) {
	f := newInheritanceFixture(t)
	root := f.buildTask("research", false)
	f.frontTo1002(root)
	child := f.insertContinuation(root, "gen1", "")
	grandchild := f.insertContinuation(child, "gen2", "")
	replacement, err := f.store.SupersedeTask(f.ctx, grandchild, nil)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := f.task("ready", 730)

	change := f.set("reset", grandchild, DefaultPriority)
	if change.TopicAnchorID != root || change.OldPriority != generatedTopicPriority || change.Priority != DefaultPriority {
		t.Fatalf("reset change = %+v", change)
	}
	for label, id := range map[string]string{"root": root, "child": child, "grandchild": grandchild, "replacement": replacement.ID} {
		f.requireRaw(label+" after reset", id, DefaultPriority)
		if got := f.priorityOf(id); got != DefaultPriority {
			t.Fatalf("%s effective priority after reset = %d", label, got)
		}
	}
	if got := f.priorityOf(unrelated); got != 730 {
		t.Fatalf("unrelated topic = %d, want 730", got)
	}

	f.requireRaw("continuation born after reset", f.insertContinuation(replacement.ID, "gen3", ""), DefaultPriority)
}

func TestDoneAnchorContributesToQueueMaximumOnlyWhileDescendantsUnfinished(t *testing.T) {
	setup := func(t *testing.T, childState string, archiveChild bool) (*inheritanceFixture, string) {
		f := newInheritanceFixture(t)
		root := f.buildTask("research", false)
		decoy := f.frontTo1002(root)
		child := f.insertContinuation(root, "gen1", "")
		f.exec(`UPDATE task SET state = 'done' WHERE id = ?`, root)
		f.exec(`UPDATE task SET state = ? WHERE id = ?`, childState, child)
		if archiveChild {
			f.exec(`UPDATE task SET archived_at = ? WHERE id = ?`, nowTimestamp(), child)
		}
		// The decoy fronted to 1001 is finished so only the done-root topic can raise the maximum.
		f.exec(`UPDATE task SET state = 'done' WHERE id = ?`, decoy)
		return f, f.task("ready", 500)
	}

	t.Run("unfinished descendant counts", func(t *testing.T) {
		f, other := setup(t, "ready", false)
		if got := f.front("other-front", other).Priority; got != generatedTopicPriority+1 {
			t.Fatalf("front = %d, want %d", got, generatedTopicPriority+1)
		}
	})
	t.Run("finished descendant does not count", func(t *testing.T) {
		f, other := setup(t, "done", false)
		if got := f.front("other-front", other).Priority; got != 1001 {
			t.Fatalf("front = %d, want 1001", got)
		}
	})
	t.Run("archived descendant does not count", func(t *testing.T) {
		f, other := setup(t, "ready", true)
		if got := f.front("other-front", other).Priority; got != 1001 {
			t.Fatalf("front = %d, want 1001", got)
		}
	})
}

func TestOptionalFindingFollowUpsKeepDefaultPriority(t *testing.T) {
	f := newInheritanceFixture(t)
	task := f.buildTask("research", false)
	f.frontTo1002(task)
	f.claimAndSubmit(task, "worker", "haiku", "https://example.com/pr/5")

	finding := json.RawMessage(`[{"id":"f1","severity":"P3","status":"new","summary":"optional","file":"x.go","line":1,"in_changed_text":false}]`)
	f.review(f.reviewTask(task, 1), "approve", finding)

	var followUp string
	err := f.store.Conn().QueryRowContext(f.ctx,
		`SELECT task_id FROM task_link WHERE kind = 'research_parent' AND value = ?`, task).Scan(&followUp)
	if err != nil {
		t.Fatalf("expected a research follow-up: %v", err)
	}
	f.requireRaw("follow-up stored priority", followUp, DefaultPriority)
	if got := f.get(followUp); got.Priority != DefaultPriority || got.TopicAnchorID != followUp {
		t.Fatalf("follow-up effective priority=%d anchor=%s, want %d and itself", got.Priority, got.TopicAnchorID, DefaultPriority)
	}
}

func TestPriorityActionsOnDescendantChangeOnlyPriority(t *testing.T) {
	f := newInheritanceFixture(t)
	root := f.buildTask("research", false)
	child := f.insertContinuation(root, "gen1", "")
	grandchild := f.insertContinuation(child, "gen2", "")
	f.exec(`UPDATE task SET state = 'in_progress', held = 1, assignee = 'worker-1', lease_expires_at = '2099-01-01T00:00:00Z', result = 'partial' WHERE id = ?`, grandchild)
	unrelated := f.task("ready", 730)

	snapshot := func() string {
		rows, err := f.store.Conn().QueryContext(f.ctx,
			`SELECT id, state, held, assignee, lease_expires_at, result, review_round, verdict, created_at, updated_at, archived_at FROM task ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out string
		for rows.Next() {
			var id, state, created, updated string
			var heldFlag bool
			var round int
			var assignee, lease, result, verdict, archived sql.NullString
			if err := rows.Scan(&id, &state, &heldFlag, &assignee, &lease, &result, &round, &verdict, &created, &updated, &archived); err != nil {
				t.Fatal(err)
			}
			out += fmt.Sprint(id, state, heldFlag, assignee, lease, result, round, verdict, created, updated, archived, "\n")
		}
		return out
	}

	before := snapshot()
	f.set("set", grandchild, 800)
	f.front("front", grandchild)
	f.set("reset", child, DefaultPriority)
	if after := snapshot(); after != before {
		t.Fatalf("priority actions changed state, lease, hold, result or timestamps:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if got := f.priorityOf(unrelated); got != 730 {
		t.Fatalf("unrelated topic = %d, want 730", got)
	}
	for _, id := range []string{root, child, grandchild} {
		f.requireRaw("member after actions", id, DefaultPriority)
	}
}
