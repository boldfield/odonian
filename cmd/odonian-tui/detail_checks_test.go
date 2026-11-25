package main

import (
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
	"github.com/boldfield/odonian/internal/tuiconfig"
)

func TestDetailContent_ShowsChecksDuration(t *testing.T) {
	config := &tuiconfig.Config{URL: "http://test", Token: "test", Actor: "testuser", PollInterval: 100 * time.Millisecond}
	model := NewBoardModel(&tuiclient.MockClient{}, config, tuiclient.Project{ID: "project-1", Name: "Test Project"})
	started := time.Now().Add(-7 * time.Minute).UTC().Format(time.RFC3339Nano)
	detail := model.buildDetailContent(tuiclient.TaskDetail{ID: "task-c1", State: "checks", ChecksStartedAt: &started})
	if !strings.Contains(detail, "In checks: 7m") {
		t.Fatalf("expected 'In checks: 7m' in detail, got:\n%s", detail)
	}
}
