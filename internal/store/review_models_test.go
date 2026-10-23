package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type reviewModelsFixture struct {
	store Store
	ctx   context.Context
	proj  string
	task  string
}

// newReviewModelsFixture creates a build task with the given reviewers, left in backlog.
func newReviewModelsFixture(t *testing.T, reviewers []string) *reviewModelsFixture {
	t.Helper()
	store, err := Open("file::memory:?cache=shared", defaultTestAllowedModels())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	ctx := context.Background()
	proj, err := store.CreateProject(ctx, "test-project", "https://github.com/test/repo")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	doc, err := store.CreateDocument(ctx, proj.ID, "feature_spec", "test-doc", "test.md", nil)
	if err != nil {
		t.Fatalf("create document: %v", err)
	}
	tasks, err := store.CreateTasks(ctx, proj.ID, []TaskInput{{
		Title: "Work", Spec: "Do the work", DocumentID: doc.ID, Model: "haiku", ReviewModels: reviewers,
	}})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return &reviewModelsFixture{store: store, ctx: ctx, proj: proj.ID, task: tasks[0].ID}
}

func (f *reviewModelsFixture) toReview(t *testing.T) {
	t.Helper()
	if _, err := f.store.PromoteTask(f.ctx, f.task); err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, err := f.store.ClaimTask(f.ctx, f.task, "agent-1", "haiku", 5*time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := f.store.SubmitTask(f.ctx, f.task, "agent-1", "done", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("submit: %v", err)
	}
}

func (f *reviewModelsFixture) update(models []string) (Task, error) {
	return f.store.UpdateTaskReviewModels(f.ctx, f.task, models, "tester", 5, nil, nil, testUnlimitedResearchBudget)
}

// children returns the round's review tasks keyed by id.
func (f *reviewModelsFixture) children(t *testing.T, round int) []Task {
	t.Helper()
	all, err := f.store.ListTasks(f.ctx, f.proj, TaskListFilter{})
	if err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	var out []Task
	for _, tk := range all {
		if tk.Kind == "review" && tk.TargetTaskID != nil && *tk.TargetTaskID == f.task && tk.ReviewRound == round {
			out = append(out, tk)
		}
	}
	return out
}

func (f *reviewModelsFixture) childIn(t *testing.T, round int, model, state string) *Task {
	t.Helper()
	for _, c := range f.children(t, round) {
		if c.Model == model && c.State == state {
			c := c
			return &c
		}
	}
	return nil
}

func (f *reviewModelsFixture) review(t *testing.T, c *Task, verdict string) error {
	t.Helper()
	if _, err := f.store.ClaimTask(f.ctx, c.ID, "rev-"+c.Model, c.Model, 5*time.Minute); err != nil {
		return err
	}
	_, err := f.store.SubmitTask(f.ctx, c.ID, "rev-"+c.Model, verdict, &verdict, nil, 5, nil, nil, testUnlimitedResearchBudget)
	return err
}

func (f *reviewModelsFixture) get(t *testing.T) TaskWithDepsAndLinks {
	t.Helper()
	tk, err := f.store.GetTask(f.ctx, f.task)
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	return tk
}

func TestUpdateTaskReviewModelsPreReviewStates(t *testing.T) {
	for _, state := range []string{"backlog", "ready", "blocked"} {
		t.Run(state, func(t *testing.T) {
			f := newReviewModelsFixture(t, []string{"opus", "sonnet"})
			switch state {
			case "ready":
				f.store.PromoteTask(f.ctx, f.task)
			case "blocked":
				f.store.PromoteTask(f.ctx, f.task)
				if _, err := f.store.TransitionTask(f.ctx, f.task, "blocked", nil); err != nil {
					t.Fatalf("block: %v", err)
				}
			}
			got, err := f.update([]string{"haiku", "opus"})
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			if !reflect.DeepEqual(got.ReviewModels, []string{"haiku", "opus"}) {
				t.Fatalf("review_models = %v", got.ReviewModels)
			}
			if got.State != state {
				t.Errorf("state changed to %s", got.State)
			}
			if state == "blocked" {
				if _, err := f.store.TransitionTask(f.ctx, f.task, "ready", nil); err != nil {
					t.Fatalf("unblock: %v", err)
				}
			}
			if state == "backlog" {
				f.store.PromoteTask(f.ctx, f.task)
			}
			if _, err := f.store.ClaimTask(f.ctx, f.task, "agent-1", "haiku", 5*time.Minute); err != nil {
				t.Fatalf("claim: %v", err)
			}
			if _, err := f.store.SubmitTask(f.ctx, f.task, "agent-1", "done", nil, []LinkInput{{Kind: "pr", Value: "#1"}}, 5, nil, nil, testUnlimitedResearchBudget); err != nil {
				t.Fatalf("submit: %v", err)
			}
			var models []string
			for _, c := range f.children(t, 1) {
				models = append(models, c.Model)
			}
			sort.Strings(models)
			if !reflect.DeepEqual(models, []string{"haiku", "opus"}) {
				t.Errorf("next round spawned %v, want [haiku opus]", models)
			}
		})
	}
}

func TestUpdateTaskReviewModelsReshapesOpenRound(t *testing.T) {
	f := newReviewModelsFixture(t, []string{"opus", "sonnet"})
	f.toReview(t)
	opus := f.childIn(t, 1, "opus", "ready")
	sonnet := f.childIn(t, 1, "sonnet", "ready")
	if opus == nil || sonnet == nil {
		t.Fatalf("expected opus and sonnet children")
	}
	if err := f.review(t, opus, "approve"); err != nil {
		t.Fatalf("opus review: %v", err)
	}

	// sonnet is claimed (in flight) when it gets replaced by haiku.
	if _, err := f.store.ClaimTask(f.ctx, sonnet.ID, "rev-sonnet", "sonnet", 5*time.Minute); err != nil {
		t.Fatalf("claim sonnet: %v", err)
	}
	got, err := f.update([]string{"opus", "haiku"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.State != "review" {
		t.Fatalf("state = %s, want review", got.State)
	}

	retired, err := f.store.GetTask(f.ctx, sonnet.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.State != "abandoned" {
		t.Errorf("sonnet child state = %s, want abandoned", retired.State)
	}
	verdict := "approve"
	if _, err := f.store.SubmitTask(f.ctx, sonnet.ID, "rev-sonnet", "late", &verdict, nil, 5, nil, nil, testUnlimitedResearchBudget); !errors.Is(err, ErrConflict) {
		t.Errorf("retired claimed child submit: want ErrConflict, got %v", err)
	}

	haiku := f.childIn(t, 1, "haiku", "ready")
	if haiku == nil {
		t.Fatalf("expected replacement haiku child in round 1")
	}
	if !strings.Contains(haiku.Spec, "Implementation PR: #1") || !strings.Contains(haiku.Spec, "Parent task: "+f.task) {
		t.Errorf("replacement spec lacks the round's brief: %s", haiku.Spec)
	}
	if haiku.Title != "Review: Work [haiku]" {
		t.Errorf("replacement title = %q", haiku.Title)
	}

	// The round approves on the new pair.
	if err := f.review(t, haiku, "approve"); err != nil {
		t.Fatalf("haiku review: %v", err)
	}
	if st := f.get(t).State; st != "approved" {
		t.Errorf("parent state = %s, want approved", st)
	}

	events, err := f.store.ListEvents(f.ctx, f.task)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Kind == "review_models_changed" && e.Actor == "tester" && e.Note != nil &&
			strings.Contains(*e.Note, `["opus","sonnet"]`) && strings.Contains(*e.Note, `["opus","haiku"]`) {
			found = true
		}
	}
	if !found {
		t.Errorf("no review_models_changed event with old list, new list and actor")
	}
}

func TestUpdateTaskReviewModelsRejectsOnNewPair(t *testing.T) {
	f := newReviewModelsFixture(t, []string{"opus", "sonnet"})
	f.toReview(t)
	if _, err := f.update([]string{"opus", "haiku"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := f.review(t, f.childIn(t, 1, "opus", "ready"), "approve"); err != nil {
		t.Fatal(err)
	}
	if err := f.review(t, f.childIn(t, 1, "haiku", "ready"), "reject"); err != nil {
		t.Fatal(err)
	}
	if st := f.get(t).State; st != "ready" {
		t.Errorf("parent state = %s, want ready (rejected round)", st)
	}
}

func TestUpdateTaskReviewModelsAggregatesWhenRemainingChildrenDone(t *testing.T) {
	f := newReviewModelsFixture(t, []string{"opus", "sonnet"})
	f.toReview(t)
	if err := f.review(t, f.childIn(t, 1, "opus", "ready"), "approve"); err != nil {
		t.Fatal(err)
	}
	// Dropping the only open reviewer leaves a finished round that must aggregate.
	if _, err := f.update([]string{"opus"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if st := f.get(t).State; st != "approved" {
		t.Errorf("parent state = %s, want approved", st)
	}
}

func TestUpdateTaskReviewModelsRefusals(t *testing.T) {
	codeOf := func(err error) string {
		var v *ValidationError
		if errors.As(err, &v) {
			return v.Code
		}
		return conflictCode(err)
	}

	f := newReviewModelsFixture(t, []string{"opus"})
	for name, tc := range map[string]struct {
		models []string
		code   string
	}{
		"nil":     {nil, "INVALID_REVIEW_MODELS"},
		"empty":   {[]string{}, "INVALID_REVIEW_MODELS"},
		"unknown": {[]string{"nope"}, "UNKNOWN_MODEL"},
	} {
		if _, err := f.update(tc.models); codeOf(err) != tc.code {
			t.Errorf("%s: code = %q (%v), want %s", name, codeOf(err), err, tc.code)
		}
	}
	if got := f.get(t).ReviewModels; !reflect.DeepEqual(got, []string{"opus"}) {
		t.Errorf("refused updates changed review_models to %v", got)
	}

	// A review task.
	f.toReview(t)
	child := f.children(t, 1)[0]
	if _, err := f.store.UpdateTaskReviewModels(f.ctx, child.ID, []string{"sonnet"}, "t", 5, nil, nil, testUnlimitedResearchBudget); codeOf(err) != "FIELD_NOT_UPDATABLE" {
		t.Errorf("review task: code = %q (%v)", codeOf(err), err)
	}

	// An approved task.
	if err := f.review(t, f.childIn(t, 1, "opus", "ready"), "approve"); err != nil {
		t.Fatal(err)
	}
	if st := f.get(t).State; st != "approved" {
		t.Fatalf("setup: state = %s", st)
	}
	if _, err := f.update([]string{"sonnet"}); !errors.Is(err, ErrConflict) && conflictCode(err) == "" {
		t.Errorf("approved: want conflict, got %v", err)
	}

	// A terminal task.
	if _, err := f.store.TransitionTask(f.ctx, f.task, "failed", nil); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if _, err := f.update([]string{"sonnet"}); conflictCode(err) == "" {
		t.Errorf("terminal: want conflict, got %v", err)
	}
	if got := f.get(t).ReviewModels; !reflect.DeepEqual(got, []string{"opus"}) {
		t.Errorf("refused updates changed review_models to %v", got)
	}
}

func TestSupersedeTaskReviewModelsOverride(t *testing.T) {
	f := newReviewModelsFixture(t, []string{"opus", "sonnet"})
	copied, err := f.store.SupersedeTask(f.ctx, f.task, nil, nil)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if !reflect.DeepEqual(copied.ReviewModels, []string{"opus", "sonnet"}) {
		t.Errorf("copied review_models = %v", copied.ReviewModels)
	}

	overridden, err := f.store.SupersedeTask(f.ctx, copied.ID, nil, []string{"haiku"})
	if err != nil {
		t.Fatalf("supersede with override: %v", err)
	}
	if !reflect.DeepEqual(overridden.ReviewModels, []string{"haiku"}) {
		t.Errorf("override review_models = %v", overridden.ReviewModels)
	}

	if _, err := f.store.SupersedeTask(f.ctx, overridden.ID, nil, []string{"nope"}); err == nil || !strings.Contains(err.Error(), "unknown review model") {
		t.Errorf("unknown override: %v", err)
	}
	if st, _ := f.store.GetTask(f.ctx, overridden.ID); st.State == "superseded" {
		t.Errorf("refused supersede still superseded the task")
	}
}

// A terminal failed review child has no verdict and is not live: the reshape must retire it
// and spawn a replacement instead of counting it as open.
func TestUpdateTaskReviewModelsReplacesFailedChild(t *testing.T) {
	f := newReviewModelsFixture(t, []string{"opus", "sonnet"})
	f.toReview(t)
	opus := f.childIn(t, 1, "opus", "ready")
	if opus == nil {
		t.Fatalf("expected opus child")
	}
	if _, err := f.store.ClaimTask(f.ctx, opus.ID, "rev-opus", "opus", 5*time.Minute); err != nil {
		t.Fatalf("claim opus: %v", err)
	}
	if _, err := f.store.TransitionTask(f.ctx, opus.ID, "failed", nil); err != nil {
		t.Fatalf("fail opus child: %v", err)
	}
	if _, err := f.update([]string{"opus", "haiku"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if f.childIn(t, 1, "opus", "ready") == nil {
		t.Errorf("no replacement opus child spawned")
	}
	haiku := f.childIn(t, 1, "haiku", "ready")
	if haiku == nil {
		t.Fatalf("no haiku child spawned")
	}
	if err := f.review(t, haiku, "approve"); err != nil {
		t.Fatalf("haiku review: %v", err)
	}
	if st := f.get(t).State; st != "review" {
		t.Errorf("parent state = %s, want review while replacement opus is open", st)
	}
	if err := f.review(t, f.childIn(t, 1, "opus", "ready"), "approve"); err != nil {
		t.Fatalf("opus review: %v", err)
	}
	if st := f.get(t).State; st != "approved" {
		t.Errorf("parent state = %s, want approved", st)
	}
}

// A superseded review child is terminal and has no verdict, like a failed one: the reshape
// must not count it as live, so the round still gets a live child for every listed model
// and can finish.
func TestUpdateTaskReviewModelsReplacesSupersededChild(t *testing.T) {
	for _, tc := range []struct {
		name      string
		supersede func(f *reviewModelsFixture, child *Task) error
	}{
		{"transition", func(f *reviewModelsFixture, child *Task) error {
			if _, err := f.store.ClaimTask(f.ctx, child.ID, "rev-opus", "opus", 5*time.Minute); err != nil {
				return err
			}
			_, err := f.store.TransitionTask(f.ctx, child.ID, "superseded", nil)
			return err
		}},
		{"supersede", func(f *reviewModelsFixture, child *Task) error {
			_, err := f.store.SupersedeTask(f.ctx, child.ID, nil, nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewModelsFixture(t, []string{"opus", "sonnet"})
			f.toReview(t)
			opus := f.childIn(t, 1, "opus", "ready")
			if opus == nil {
				t.Fatalf("expected opus child")
			}
			if err := tc.supersede(f, opus); err != nil {
				t.Fatalf("supersede opus child: %v", err)
			}
			if _, err := f.update([]string{"opus", "haiku"}); err != nil {
				t.Fatalf("update: %v", err)
			}
			if st, _ := f.store.GetTask(f.ctx, opus.ID); st.State != "abandoned" {
				t.Errorf("superseded child state = %s, want abandoned (retired)", st.State)
			}
			if f.childIn(t, 1, "opus", "ready") == nil {
				t.Fatalf("no live opus child in round 1")
			}
			haiku := f.childIn(t, 1, "haiku", "ready")
			if haiku == nil {
				t.Fatalf("no haiku child spawned")
			}
			if err := f.review(t, haiku, "approve"); err != nil {
				t.Fatalf("haiku review: %v", err)
			}
			if st := f.get(t).State; st != "review" {
				t.Errorf("parent state = %s, want review while opus is open", st)
			}
			if err := f.review(t, f.childIn(t, 1, "opus", "ready"), "approve"); err != nil {
				t.Fatalf("opus review: %v", err)
			}
			if st := f.get(t).State; st != "approved" {
				t.Errorf("parent state = %s, want approved", st)
			}
		})
	}
}

// When a reshape retires a verdict-less research review child and spawns a same-model
// replacement in the same round, every slot assignment must give the replacement the
// retired child's lineage, so its findings stay on one lineage inside the server and
// into the next round (a dispute of its finding reaches the next round's reviewer).
func TestUpdateTaskReviewModelsResearchReplacementLineage(t *testing.T) {
	store, ctx, projID, parentID := newResearchTask(t, false)
	opus, sonnet := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if opus == nil || sonnet == nil {
		t.Fatalf("expected both round-1 review tasks")
	}
	if _, err := store.ClaimTask(ctx, opus.ID, "rev-opus", "opus", 5*time.Minute); err != nil {
		t.Fatalf("claim opus: %v", err)
	}
	if _, err := store.TransitionTask(ctx, opus.ID, "failed", nil); err != nil {
		t.Fatalf("fail opus child: %v", err)
	}
	if _, err := store.UpdateTaskReviewModels(ctx, parentID, []string{"opus", "sonnet"}, "tester", 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("update: %v", err)
	}
	replacement, _ := findResearchReviewTasks(t, store, ctx, projID, parentID, 1)
	if replacement == nil || replacement.ID == opus.ID {
		t.Fatalf("expected a replacement opus child")
	}
	finding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"overstates source","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, replacement, "opus-reviewer", "reject", finding)
	submitResearchReview(t, store, ctx, sonnet, "sonnet-reviewer", "approve", json.RawMessage(`[]`))
	if p, _ := store.GetTask(ctx, parentID); p.State != "ready" {
		t.Fatalf("parent state = %s, want ready after the rejected round", p.State)
	}

	want := researchReviewerLineage("opus", 0)
	s := store.(*sqliteStore)
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	lineages, err := researchReviewTaskLineages(ctx, tx, parentID)
	if err != nil {
		t.Fatalf("researchReviewTaskLineages: %v", err)
	}
	if _, ok := lineages[opus.ID]; ok {
		t.Errorf("retired child was assigned a lineage")
	}
	if got := lineages[replacement.ID]; got != want {
		t.Errorf("researchReviewTaskLineages: replacement lineage = %q, want %q", got, want)
	}
	collected, err := s.collectResearchReviewFindings(ctx, tx, parentID, 1)
	if err != nil {
		t.Fatalf("collectResearchReviewFindings: %v", err)
	}
	if len(collected) != 1 || collected[0].lineage != want {
		t.Errorf("collectResearchReviewFindings: got %+v, want one finding on lineage %q", collected, want)
	}
	_, blocking, blockingLineages, err := s.checkResearchBlockingFindings(ctx, tx, parentID, 1)
	if err != nil {
		t.Fatalf("checkResearchBlockingFindings: %v", err)
	}
	if len(blocking) != 1 || len(blockingLineages) != 1 || blockingLineages[0] != want {
		t.Errorf("checkResearchBlockingFindings: lineages = %q, want [%q]", blockingLineages, want)
	}
	tx.Rollback()

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"the source supports the claim as written"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("resubmit with dispute: %v", err)
	}
	opus2, sonnet2 := findResearchReviewTasks(t, store, ctx, projID, parentID, 2)
	if opus2 == nil || sonnet2 == nil {
		t.Fatalf("expected both round-2 review tasks")
	}
	if !strings.Contains(opus2.Spec, "Disputed Finding") || !strings.Contains(opus2.Spec, "overstates source") {
		t.Errorf("round-2 opus spec lacks the dispute of the replacement's finding:\n%s", opus2.Spec)
	}
	if strings.Contains(sonnet2.Spec, "Disputed Finding") {
		t.Errorf("round-2 sonnet spec carries a dispute it did not raise")
	}
}

// With duplicate same-model reviewers, a replacement for a retired earlier slot must take
// that slot, and the later slot's finished child must keep its own: no child's lineage
// shifts in any slot assignment, and a dispute of the later child's finding reaches the
// same slot in the next round.
func TestUpdateTaskReviewModelsDuplicateReviewerReplacementKeepsSlots(t *testing.T) {
	store, ctx, _, parentID := newResearchTaskWithReviewers(t, false, []string{"opus", "opus"})
	s := store.(*sqliteStore)
	roundChildren := func(round int) []*Task {
		t.Helper()
		rows, err := s.conn.QueryContext(ctx, `SELECT id FROM task WHERE target_task_id = ? AND kind = 'review' AND review_round = ? ORDER BY rowid`, parentID, round)
		if err != nil {
			t.Fatalf("list round %d children: %v", round, err)
		}
		defer rows.Close()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			ids = append(ids, id)
		}
		var out []*Task
		for _, id := range ids {
			tk, err := store.GetTask(ctx, id)
			if err != nil {
				t.Fatalf("get child: %v", err)
			}
			out = append(out, &Task{ID: tk.ID, Model: tk.Model, State: tk.State, Spec: tk.Spec})
		}
		return out
	}
	first := roundChildren(1)
	if len(first) != 2 {
		t.Fatalf("round 1 children = %d, want 2", len(first))
	}
	slot0, slot1 := first[0], first[1]
	finding := json.RawMessage(`[{"id":"f1","severity":"P2","file":"a.md","line":3,"summary":"overstates source","in_changed_text":true,"status":"new"}]`)
	submitResearchReview(t, store, ctx, slot1, "opus-reviewer-1", "reject", finding)
	if _, err := store.ClaimTask(ctx, slot0.ID, "opus-reviewer-0", "opus", 5*time.Minute); err != nil {
		t.Fatalf("claim slot 0: %v", err)
	}
	if _, err := store.TransitionTask(ctx, slot0.ID, "failed", nil); err != nil {
		t.Fatalf("fail slot 0: %v", err)
	}
	if _, err := store.UpdateTaskReviewModels(ctx, parentID, []string{"opus", "opus"}, "tester", 8, nil, nil, testUnlimitedResearchBudget); err != nil {
		t.Fatalf("update: %v", err)
	}
	after := roundChildren(1)
	if len(after) != 3 || after[0].State != "abandoned" || after[2].State != "ready" {
		t.Fatalf("round 1 after reshape: want retired slot 0, finished slot 1 and a ready replacement")
	}
	replacement := after[2]

	want0, want1 := researchReviewerLineage("opus", 0), researchReviewerLineage("opus", 1)
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	lineages, err := researchReviewTaskLineages(ctx, tx, parentID)
	if err != nil {
		t.Fatalf("researchReviewTaskLineages: %v", err)
	}
	if _, ok := lineages[slot0.ID]; ok {
		t.Errorf("retired child was assigned a lineage")
	}
	if got := lineages[slot1.ID]; got != want1 {
		t.Errorf("researchReviewTaskLineages: finished slot-1 child = %q, want %q", got, want1)
	}
	if got := lineages[replacement.ID]; got != want0 {
		t.Errorf("researchReviewTaskLineages: replacement = %q, want %q", got, want0)
	}
	collected, err := s.collectResearchReviewFindings(ctx, tx, parentID, 1)
	if err != nil {
		t.Fatalf("collectResearchReviewFindings: %v", err)
	}
	if len(collected) != 1 || collected[0].lineage != want1 {
		t.Errorf("collectResearchReviewFindings: got %+v, want one finding on lineage %q", collected, want1)
	}
	tx.Rollback()

	submitResearchReview(t, store, ctx, replacement, "opus-reviewer-0", "approve", json.RawMessage(`[]`))
	if p, _ := store.GetTask(ctx, parentID); p.State != "ready" {
		t.Fatalf("parent state = %s, want ready after the rejected round", p.State)
	}
	tx, err = s.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, _, blockingLineages, err := s.checkResearchBlockingFindings(ctx, tx, parentID, 1)
	tx.Rollback()
	if err != nil {
		t.Fatalf("checkResearchBlockingFindings: %v", err)
	}
	if len(blockingLineages) != 1 || blockingLineages[0] != want1 {
		t.Errorf("checkResearchBlockingFindings: lineages = %q, want [%q]", blockingLineages, want1)
	}

	disputes := json.RawMessage(`[{"finding_id":"f1","evidence":"the source supports the claim as written"}]`)
	if _, err := resubmitResearchImplementTaskWithDisputes(t, store, ctx, parentID, disputes); err != nil {
		t.Fatalf("resubmit with dispute: %v", err)
	}
	second := roundChildren(2)
	if len(second) != 2 {
		t.Fatalf("round 2 children = %d, want 2", len(second))
	}
	if strings.Contains(second[0].Spec, "Disputed Finding") {
		t.Errorf("round-2 slot-0 spec carries a dispute it did not raise")
	}
	if !strings.Contains(second[1].Spec, "Disputed Finding") || !strings.Contains(second[1].Spec, "overstates source") {
		t.Errorf("round-2 slot-1 spec lacks the dispute of its own finding:\n%s", second[1].Spec)
	}
}
