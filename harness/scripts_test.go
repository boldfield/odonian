package harness

import (
	"os/exec"
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
	for _, script := range []string{"harness_test.sh", "scheduling_test.sh", "research_admission_test.sh", "research_pacing_smoke_test.sh", "priority_scheduling_smoke_test.sh", "model_failover_test.sh"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command(bash, script).CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", script, err, out)
			}
		})
	}
}
