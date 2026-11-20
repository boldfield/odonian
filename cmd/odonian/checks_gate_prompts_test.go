package main

import (
	"os"
	"path/filepath"
	"testing"
)

func readPullRequestPrompt(t *testing.T, track, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "harness", "prompts", "pull_request", track, name))
	if err != nil {
		t.Fatalf("read %s/%s: %v", track, name, err)
	}
	return string(b)
}

func TestImplementPromptsCarryAwaitChecksInstruction(t *testing.T) {
	for _, track := range []string{"build", "design"} {
		p := readPullRequestPrompt(t, track, "implement.md")
		requireAll(t, track+"/implement.md", p,
			"`Await Checks: true`",
			"**`await_checks: true`** — do NOT wait for CI.",
			"submit (step 9) at once",
			"a failed check appears as an item in `odonian pr-feedback list`",
			"`gh run view <run-id> --log-failed`",
			"`odonian pr-feedback ack <pr-url> <item-id> <sha>`",
			"`odonian pr-feedback ack --disputed <pr-url> <item-id>`",
			"**No `await_checks`** (`Await Checks: false`) — nothing changes",
		)
	}
}

func TestReviewPromptsCarryAwaitChecksInstruction(t *testing.T) {
	for _, track := range []string{"build", "design"} {
		p := readPullRequestPrompt(t, track, "review.md")
		requireAll(t, track+"/review.md", p,
			"**Parent with `await_checks: true`**",
			"the board created this review only after the head commit's checks were green",
			"Still run `gh pr checks \"<pr-url>\"`",
			"head changed after the checks gate",
			"Never wait for pending checks on such a task.",
			"A parent without `await_checks`",
		)
	}
}

func TestBuildPromptsKeepForegroundCIWaitForOtherTasks(t *testing.T) {
	for _, name := range []string{"implement.md", "review.md"} {
		p := readPullRequestPrompt(t, "build", name)
		requireAll(t, "build/"+name, p,
			"You are running headless.",
			"To wait for CI or a long command, wait in the foreground",
		)
	}
	requireAll(t, "build/review.md", readPullRequestPrompt(t, "build", "review.md"),
		"If checks are still **pending** and the parent has no `await_checks`, wait and re-check before deciding",
	)
}
