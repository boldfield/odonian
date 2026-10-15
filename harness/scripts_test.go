package harness

import (
	"os"
	"os/exec"
	"testing"
)

// The fleet launcher's scheduling logic lives in shell, so these suites drive the real agent.sh
// against fake odonian/claude binaries. Running them from `go test` keeps CI from letting a
// scheduler regression through. These suites SKIP (exit 0) when jq or git is missing.
//
// These suites predate headless session recovery. Their fake models exit 0 while their task is still
// owned, which recovery would resume or block, so they run with it bypassed; session_resume_test.sh
// covers recovery itself.
var recoveryBypassed = map[string]bool{
	"lease_renewal_test.sh":             true,
	"priority_scheduling_smoke_test.sh": true,
}

func TestHarnessScripts(t *testing.T) {
	if testing.Short() {
		t.Skip("harness shell suites are skipped in -short mode")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	for _, script := range []string{"harness_test.sh", "scheduling_test.sh", "research_admission_test.sh", "research_pacing_smoke_test.sh", "priority_scheduling_smoke_test.sh", "model_failover_test.sh", "lease_renewal_test.sh", "session_resume_test.sh"} {
		t.Run(script, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(bash, script)
			if recoveryBypassed[script] {
				cmd.Env = append(os.Environ(), "ODONIAN_SESSION_RECOVERY=off")
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s failed: %v\n%s", script, err, out)
			}
		})
	}
}
