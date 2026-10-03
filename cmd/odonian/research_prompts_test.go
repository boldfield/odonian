package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readResearchPrompt(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "harness", "prompts", "pull_request", "research", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func requireAll(t *testing.T, label, text string, wants ...string) {
	t.Helper()
	text = strings.Join(strings.Fields(text), " ")
	for _, w := range wants {
		if !strings.Contains(text, strings.Join(strings.Fields(w), " ")) {
			t.Errorf("%s: missing %q", label, w)
		}
	}
}

func TestResearchPromptsPreclaimedModeCannotSelectAnotherTask(t *testing.T) {
	for _, name := range []string{"implement.md", "review.md"} {
		p := readResearchPrompt(t, name)
		requireAll(t, name, p,
			"ODONIAN_PRECLAIMED_TASK_ID",
			"ODONIAN_PRECLAIMED_ATTEMPT_ID",
			"not call `odonian next` or `odonian claim`",
			"other than `ODONIAN_PRECLAIMED_TASK_ID`",
		)
	}
}

func TestResearchPromptsValidatePreclaimedOwnership(t *testing.T) {
	for _, name := range []string{"implement.md", "review.md"} {
		p := readResearchPrompt(t, name)
		requireAll(t, name, p,
			"odonian show --json",
			"compare the task's assignee to `$AGENT_ID`",
			"state is `in_progress`",
		)
	}
}

func TestResearchPromptsOrdinaryModeStillDocumented(t *testing.T) {
	impl := readResearchPrompt(t, "implement.md")
	requireAll(t, "implement.md", impl,
		`odonian next --project "$ODONIAN_PROJECT" --model "$AGENT_MODEL" --kind implement`,
		"`odonian claim <id>`",
		"**Ordinary (legacy) mode.**",
		"valid only while the research",
		"not `enforce`",
		"exits 2 (scheduling denial)",
	)
	rev := readResearchPrompt(t, "review.md")
	requireAll(t, "review.md", rev,
		`odonian next --project "$ODONIAN_PROJECT" --model "$AGENT_MODEL"`,
		"--kind review",
		"`odonian claim <id>`",
		"**Ordinary (legacy) mode.**",
		"valid only while the research admission",
		"not `enforce`",
		"exits 2 (scheduling denial)",
	)
}

func TestResearchImplementPromptFencesHeartbeatAndSubmit(t *testing.T) {
	p := readResearchPrompt(t, "implement.md")
	requireAll(t, "implement.md", p,
		`odonian heartbeat <id> --attempt "$ODONIAN_PRECLAIMED_ATTEMPT_ID"`,
		`also pass `+"`--attempt \"$ODONIAN_PRECLAIMED_ATTEMPT_ID\"`"+` to bind the submission`,
	)
}

func TestResearchReviewPromptRegularAndAdjudicationSubmitUseAttempt(t *testing.T) {
	p := readResearchPrompt(t, "review.md")
	regularIdx := strings.Index(p, "\n6. **Decide the verdict")
	adjIdx := strings.Index(p, "\n6-adjudicate.")
	endIdx := strings.Index(p, "\n7. **Do NOT merge")
	if regularIdx < 0 || adjIdx < 0 || endIdx < 0 || !(regularIdx < adjIdx && adjIdx < endIdx) {
		t.Fatalf("review.md step layout unexpected: %d %d %d", regularIdx, adjIdx, endIdx)
	}
	attempt := "`--attempt \"$ODONIAN_PRECLAIMED_ATTEMPT_ID\"`"
	requireAll(t, "review.md regular step 6", p[regularIdx:adjIdx], "odonian submit <review-task-id>", attempt)
	requireAll(t, "review.md step 6-adjudicate", p[adjIdx:endIdx], "odonian submit <review-task-id>", attempt)

	// Adjudication is detected from the supplied task's spec, not from a separately selected task.
	requireAll(t, "review.md adjudication detection", p,
		"Adjudicate one disputed research review finding",
		"If `ODONIAN_PRECLAIMED_TASK_ID` is set, use that as your task ID",
	)
}
