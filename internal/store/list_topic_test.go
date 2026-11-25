package store

import (
	"testing"
)

// lineageList builds one of each lineage kind straight in SQL: a review of an implement
// task, a superseding replacement, a continuation child, a task with no lineage and a
// two-task cycle. Returns every task id created.
func (f *priorityFixture) lineageList() []string {
	f.t.Helper()
	root := f.task("ready", 900)
	review := f.task("ready", 500)
	old := f.task("superseded", 300)
	replacement := f.task("ready", 500)
	child := f.task("ready", 500)
	loner := f.task("ready", 700)
	cycA := f.task("ready", 650)
	cycB := f.task("ready", 800)
	merge := f.task("ready", 500)
	f.exec(`UPDATE task SET target_task_id = ?, kind = 'review' WHERE id = ?`, root, review)
	f.exec(`UPDATE task SET target_task_id = ?, kind = 'merge' WHERE id = ?`, root, merge)
	f.exec(`UPDATE task SET superseded_by = ? WHERE id = ?`, replacement, old)
	f.exec(`INSERT INTO task_link (id, task_id, kind, value) VALUES (?, ?, 'continuation_parent', ?)`, GenerateID(), child, root)
	f.exec(`UPDATE task SET target_task_id = ? WHERE id = ?`, cycB, cycA)
	f.exec(`UPDATE task SET target_task_id = ? WHERE id = ?`, cycA, cycB)
	return []string{root, review, merge, old, replacement, child, loner, cycA, cycB}
}

func TestListTasksTopicMatchesGetTask(t *testing.T) {
	f := newPriorityFixture(t)
	ids := f.lineageList()
	f.exec(`UPDATE task SET model = 'haiku' WHERE id IN (?, ?)`, ids[0], ids[6])

	state := "ready"
	model := "haiku"
	filters := map[string]TaskListFilter{
		"none":       {},
		"superseded": {IncludeSuperseded: true},
		"state":      {State: &state},
		"claimable":  {Claimable: true},
		"model":      {Model: &model},
		"omitbodies": {OmitBodies: true},
		"all":        {State: &state, Claimable: true, Model: &model, OmitBodies: true, IncludeSuperseded: true},
	}
	for name, filter := range filters {
		t.Run(name, func(t *testing.T) {
			got, err := f.store.ListTasks(f.ctx, f.projectID, filter)
			if err != nil {
				t.Fatalf("ListTasks: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("filter matched no tasks")
			}
			for i, task := range got {
				want := f.get(task.ID)
				if task.Priority != want.Priority || task.TopicAnchorID != want.TopicAnchorID {
					t.Fatalf("%s: list (priority %d, anchor %s), GetTask (priority %d, anchor %s)",
						task.ID, task.Priority, task.TopicAnchorID, want.Priority, want.TopicAnchorID)
				}
				if i == 0 {
					continue
				}
				prev := got[i-1]
				switch {
				case prev.Priority < task.Priority:
					t.Fatalf("order: %s (%d) before %s (%d)", prev.ID, prev.Priority, task.ID, task.Priority)
				case prev.Priority == task.Priority && prev.CreatedAt > task.CreatedAt:
					t.Fatalf("order: created_at not ascending within priority at %s", task.ID)
				case prev.Priority == task.Priority && prev.CreatedAt == task.CreatedAt && prev.ID > task.ID:
					t.Fatalf("order: id not ascending within ties at %s", task.ID)
				}
			}
		})
	}
}

func BenchmarkListTasksLineage(b *testing.B) {
	f := newPriorityFixture(b)
	for i := 0; i < 250; i++ {
		f.lineageList()
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := f.store.ListTasks(f.ctx, f.projectID, TaskListFilter{IncludeSuperseded: true}); err != nil {
			b.Fatal(err)
		}
	}
}
