package main

import (
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
)

func TestSortTasksByPriority(t *testing.T) {
	p500 := int64(500)
	p1000 := int64(1000)
	p750 := int64(750)

	now := time.Now().Format(time.RFC3339)
	before := time.Now().Add(-time.Hour).Format(time.RFC3339)

	tasks := []tuiclient.Task{
		{ID: "d", Priority: nil, CreatedAt: now, Title: "default priority, newer"},
		{ID: "c", Priority: &p500, CreatedAt: before, Title: "explicit 500, older"},
		{ID: "b", Priority: &p1000, CreatedAt: now, Title: "1000 priority"},
		{ID: "a", Priority: &p750, CreatedAt: now, Title: "750 priority"},
	}

	sortTasksByPriority(tasks)

	// Expected order: p1000 (b), p750 (a), p500 older (c), p500 default newer (d)
	want := []string{"b", "a", "c", "d"}
	for i := range want {
		if tasks[i].ID != want[i] {
			t.Errorf("position %d: got ID %q want %q", i, tasks[i].ID, want[i])
		}
	}
}

func TestSortTasksByPriorityDescending(t *testing.T) {
	p100 := int64(100)
	p200 := int64(200)
	p300 := int64(300)

	tasks := []tuiclient.Task{
		{ID: "a", Priority: &p100, CreatedAt: "2025-01-01T00:00:00Z", Title: "100"},
		{ID: "b", Priority: &p300, CreatedAt: "2025-01-01T00:00:00Z", Title: "300"},
		{ID: "c", Priority: &p200, CreatedAt: "2025-01-01T00:00:00Z", Title: "200"},
	}

	sortTasksByPriority(tasks)

	// Higher priority first
	want := []string{"b", "c", "a"}
	for i := range want {
		if tasks[i].ID != want[i] {
			t.Errorf("position %d: got ID %q want %q", i, tasks[i].ID, want[i])
		}
	}
}

func TestSortTasksByPrioritySameValueOldestFirst(t *testing.T) {
	p500 := int64(500)
	old := "2025-01-01T00:00:00Z"
	new := "2025-01-02T00:00:00Z"

	tasks := []tuiclient.Task{
		{ID: "new", Priority: &p500, CreatedAt: new, Title: "newer"},
		{ID: "old", Priority: &p500, CreatedAt: old, Title: "older"},
	}

	sortTasksByPriority(tasks)

	// Older should come first when priority is the same
	want := []string{"old", "new"}
	for i := range want {
		if tasks[i].ID != want[i] {
			t.Errorf("position %d: got ID %q want %q", i, tasks[i].ID, want[i])
		}
	}
}
