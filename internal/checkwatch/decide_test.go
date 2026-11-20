package checkwatch

import (
	"testing"
	"time"

	"github.com/boldfield/odonian/internal/forge"
)

func TestDecide(t *testing.T) {
	now := time.Now()
	settled := now.Add(-3 * time.Minute)
	unsettled := now.Add(-1 * time.Minute)
	graceExpired := now.Add(-6 * time.Minute)
	graceNotExpired := now.Add(-3 * time.Minute)
	timedOut := now.Add(-4 * time.Hour)

	config := Config{
		Settle:      2 * time.Minute,
		NoRunsGrace: 5 * time.Minute,
		Timeout:     3 * time.Hour,
	}

	tests := []struct {
		name             string
		checks           *forge.PRChecks
		checksStartedAt  time.Time
		expectedDecision Decision
		descContains     string
	}{
		// Rule 1: Merged PR
		{
			name: "merged pull request returns wait",
			checks: &forge.PRChecks{
				State:       "merged",
				RollupState: "SUCCESS",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "SUCCESS", URL: "http://x"}},
			},
			checksStartedAt:  settled,
			expectedDecision: Wait,
			descContains:     "merged",
		},
		// Rule 1: Closed PR (not merged)
		{
			name: "closed pull request returns prclosed",
			checks: &forge.PRChecks{
				State:       "closed",
				RollupState: "SUCCESS",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "SUCCESS", URL: "http://x"}},
			},
			checksStartedAt:  settled,
			expectedDecision: PRClosed,
			descContains:     "closed",
		},
		// Rule 2: Failed context (failure)
		{
			name: "failure conclusion returns fail immediately",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "FAILURE",
				Contexts: []forge.CheckContext{
					{Name: "build", Conclusion: "FAILURE", URL: "http://x/1"},
					{Name: "test", Conclusion: "PENDING", URL: "http://x/2"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "build",
		},
		// Rule 2: Timed out context
		{
			name: "timed_out conclusion returns fail immediately",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts: []forge.CheckContext{
					{Name: "job1", Conclusion: "timed_out", URL: "http://x/1"},
					{Name: "job2", Conclusion: "PENDING", URL: "http://x/2"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "job1",
		},
		// Rule 2: Cancelled context
		{
			name: "cancelled conclusion returns fail immediately",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts: []forge.CheckContext{
					{Name: "workflow", Conclusion: "cancelled", URL: "http://x/1"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "workflow",
		},
		// Rule 2: Action required
		{
			name: "action_required conclusion returns fail immediately",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts: []forge.CheckContext{
					{Name: "approval", Conclusion: "action_required", URL: "http://x/1"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "approval",
		},
		// Rule 2: Startup failure
		{
			name: "startup_failure conclusion returns fail immediately",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts: []forge.CheckContext{
					{Name: "runner", Conclusion: "startup_failure", URL: "http://x/1"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "runner",
		},
		// Rule 2: Commit status error
		{
			name: "commit status error returns fail immediately",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "FAILURE",
				Contexts: []forge.CheckContext{
					{Kind: "status", Name: "ci/legacy", Conclusion: "error", URL: "http://x/1"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "ci/legacy",
		},
		// Rule 2: Multiple failed checks
		{
			name: "multiple failed checks all named in summary",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "FAILURE",
				Contexts: []forge.CheckContext{
					{Name: "build", Conclusion: "FAILURE", URL: "http://x/1"},
					{Name: "lint", Conclusion: "FAILURE", URL: "http://x/2"},
					{Name: "test", Conclusion: "PENDING", URL: "http://x/3"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "build",
		},
		// Rule 3: Green and settled passes
		{
			name: "green rollup after settle window returns pass",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "SUCCESS",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "SUCCESS", URL: "http://x"}},
			},
			checksStartedAt:  settled,
			expectedDecision: Pass,
			descContains:     "passed",
		},
		// Rule 3: Green but not settled waits
		{
			name: "green rollup before settle window returns wait",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "SUCCESS",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "SUCCESS", URL: "http://x"}},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Wait,
			descContains:     "waiting",
		},
		// Rule 4: No checks within grace period
		{
			name: "no checks before grace period returns wait",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "",
				Contexts:    []forge.CheckContext{},
			},
			checksStartedAt:  graceNotExpired,
			expectedDecision: Wait,
			descContains:     "waiting for first check",
		},
		// Rule 4: No checks after grace period
		{
			name: "no checks after grace period returns pass with no checks reported summary",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "",
				Contexts:    []forge.CheckContext{},
			},
			checksStartedAt:  graceExpired,
			expectedDecision: Pass,
			descContains:     "no checks reported",
		},
		// Rule 5: Timeout
		{
			name: "exceeding timeout returns timeout",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "PENDING", URL: "http://x"}},
			},
			checksStartedAt:  timedOut,
			expectedDecision: Timeout,
			descContains:     "timeout",
		},
		// Rule 6: Pending contexts before timeout
		{
			name: "pending checks before timeout returns wait",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "PENDING", URL: "http://x"}},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Wait,
			descContains:     "waiting",
		},
		// Boundary: Exact settle time
		{
			name: "exact settle time boundary passes",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "SUCCESS",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "SUCCESS", URL: "http://x"}},
			},
			checksStartedAt:  now.Add(-2 * time.Minute),
			expectedDecision: Pass,
			descContains:     "passed",
		},
		// Boundary: Just before settle time
		{
			name: "just before settle time boundary waits",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "SUCCESS",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "SUCCESS", URL: "http://x"}},
			},
			checksStartedAt:  now.Add(-(2*time.Minute - 1*time.Second)),
			expectedDecision: Wait,
			descContains:     "waiting",
		},
		// Boundary: Exact grace period
		{
			name: "exact grace period for no checks passes",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "",
				Contexts:    []forge.CheckContext{},
			},
			checksStartedAt:  now.Add(-5 * time.Minute),
			expectedDecision: Pass,
			descContains:     "no checks reported",
		},
		// Boundary: Just before grace period for no checks
		{
			name: "just before grace period for no checks waits",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "",
				Contexts:    []forge.CheckContext{},
			},
			checksStartedAt:  now.Add(-(5*time.Minute - 1*time.Second)),
			expectedDecision: Wait,
			descContains:     "waiting",
		},
		// Boundary: Exact timeout
		{
			name: "exact timeout boundary returns timeout",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "PENDING", URL: "http://x"}},
			},
			checksStartedAt:  now.Add(-3 * time.Hour),
			expectedDecision: Timeout,
			descContains:     "timeout",
		},
		// Boundary: Just before timeout
		{
			name: "just before timeout boundary waits",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "PENDING",
				Contexts:    []forge.CheckContext{{Name: "build", Conclusion: "PENDING", URL: "http://x"}},
			},
			checksStartedAt:  now.Add(-(3*time.Hour - 1*time.Second)),
			expectedDecision: Wait,
			descContains:     "waiting",
		},
		// Failed context takes precedence over unsettled green
		{
			name: "failed context returns fail even with other green contexts",
			checks: &forge.PRChecks{
				State:       "open",
				RollupState: "FAILURE",
				Contexts: []forge.CheckContext{
					{Name: "build", Conclusion: "SUCCESS", URL: "http://x/1"},
					{Name: "test", Conclusion: "FAILURE", URL: "http://x/2"},
				},
			},
			checksStartedAt:  unsettled,
			expectedDecision: Fail,
			descContains:     "test",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := Decide(tt.checks, now, tt.checksStartedAt, config)
			if result.Decision != tt.expectedDecision {
				t.Errorf("got decision %q, want %q", result.Decision, tt.expectedDecision)
			}
			if !contains(result.Summary, tt.descContains) {
				t.Errorf("got summary %q, want to contain %q", result.Summary, tt.descContains)
			}
		})
	}
}

func contains(s, substr string) bool {
	for i := 0; i < len(s); i++ {
		if len(s[i:]) < len(substr) {
			return false
		}
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
