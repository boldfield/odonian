package harness

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The fleet launcher's scheduling logic lives in shell, so these suites drive the real agent.sh
// against fake odonian/claude binaries. Running them from `go test` keeps CI from letting a
// scheduler regression through. These suites SKIP (exit 0) when jq or git is missing.
func TestHarnessScripts(t *testing.T) {
	if testing.Short() {
		t.Skip("harness shell suites are skipped in -short mode")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	for _, script := range []string{"harness_test.sh", "scheduling_test.sh", "research_admission_test.sh", "research_pacing_smoke_test.sh", "priority_scheduling_smoke_test.sh", "model_failover_test.sh", "lease_renewal_test.sh", "session_resume_test.sh", "dispatch_reaping_test.sh", "account_backoff_test.sh", "account_dispatch_test.sh"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command(bash, script).CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", script, err, out)
			}
		})
	}
}

// Each implement prompt's branch-setup step must tell a replacement task that the closed pull
// request named under "Prior attempt feedback" is not its own.
func TestImplementPromptsScopeClosedPR(t *testing.T) {
	for _, track := range []string{"build", "design", "research"} {
		b, err := os.ReadFile("prompts/pull_request/" + track + "/implement.md")
		if err != nil {
			t.Fatal(err)
		}
		text := string(b)
		start := strings.Index(text, "**REWORK — ")
		end := strings.Index(text, "\n7. **Address")
		if start < 0 || end < start {
			t.Fatalf("%s implement prompt: branch-setup step not found", track)
		}
		if !strings.Contains(text[start:end], "Prior attempt feedback") {
			t.Errorf("%s implement prompt branch-setup step does not mention Prior attempt feedback", track)
		}
	}
}
