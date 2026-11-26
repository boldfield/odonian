package main

import (
	"strings"
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/tuiclient"
)

func TestDetailContent_ShowsChecksDuration(t *testing.T) {
	model := newBoardFixture(t, 80, 24).model
	started := time.Now().Add(-7 * time.Minute).UTC().Format(time.RFC3339Nano)
	detail := model.buildDetailContent(tuiclient.TaskDetail{ID: "task-c1", State: "checks", ChecksStartedAt: &started})
	if !strings.Contains(detail, "In checks: 7m") {
		t.Fatalf("expected 'In checks: 7m' in detail, got:\n%s", detail)
	}
}

func TestDetailContent_ShowsTopicAnchor(t *testing.T) {
	model := newBoardFixture(t, 80, 24).model
	anchor := "topic-42"
	detail := model.buildDetailContent(tuiclient.TaskDetail{ID: "task-a1", State: "ready", TopicAnchorID: &anchor})
	if !strings.Contains(detail, "Topic Anchor: topic-42") {
		t.Fatalf("expected topic anchor in detail, got:\n%s", detail)
	}
}
