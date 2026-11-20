package checkwatch

import (
	"fmt"
	"strings"
	"time"

	"github.com/boldfield/odonian/internal/forge"
)

type Decision string

const (
	Wait     Decision = "wait"
	Pass     Decision = "pass"
	Fail     Decision = "fail"
	Timeout  Decision = "timeout"
	PRClosed Decision = "prclosed"
)

type Config struct {
	Settle      time.Duration
	NoRunsGrace time.Duration
	Timeout     time.Duration
}

type Result struct {
	Decision Decision
	Summary  string
}

func Decide(checks *forge.PRChecks, now time.Time, checksStartedAt time.Time, config Config) Result {
	// Rule 1: Pull request merged → Wait: PR-watch converges an externally merged pull request to done.
	// Pull request closed without merging → PRClosed.
	if checks.State == "merged" {
		return Result{Decision: Wait, Summary: "pull request merged"}
	}
	if checks.State == "closed" {
		return Result{Decision: PRClosed, Summary: "pull request closed without merging"}
	}

	// Rule 2: Any context concluded failure, timed_out, cancelled, action_required or startup_failure,
	// or a commit status in failure/error → Fail, without waiting for the other contexts.
	for _, ctx := range checks.Contexts {
		if isFailedConclusion(ctx.Conclusion) {
			failedChecks := getFailedChecks(checks.Contexts)
			return Result{Decision: Fail, Summary: failedChecks}
		}
	}

	// Rule 3: Rollup state SUCCESS and the task has been in checks for at least CHECKS_SETTLE → Pass.
	if checks.RollupState == "SUCCESS" && now.Sub(checksStartedAt) >= config.Settle {
		return Result{Decision: Pass, Summary: "checks passed"}
	}

	// Rule 4: No contexts at all: Wait until CHECKS_NO_RUNS_GRACE has passed since checksStartedAt,
	// then Pass with the summary "no checks reported".
	if len(checks.Contexts) == 0 {
		if now.Sub(checksStartedAt) >= config.NoRunsGrace {
			return Result{Decision: Pass, Summary: "no checks reported"}
		}
		return Result{Decision: Wait, Summary: "waiting for first check to appear"}
	}

	// Rule 5: In checks longer than CHECKS_TIMEOUT → Timeout.
	if now.Sub(checksStartedAt) >= config.Timeout {
		return Result{Decision: Timeout, Summary: "checks timeout exceeded"}
	}

	// Rule 6: Otherwise Wait.
	return Result{Decision: Wait, Summary: "waiting for checks to complete"}
}

func isFailedConclusion(conclusion string) bool {
	c := strings.ToLower(conclusion)
	switch c {
	case "failure", "timed_out", "cancelled", "action_required", "startup_failure", "error":
		return true
	}
	return false
}

func getFailedChecks(contexts []forge.CheckContext) string {
	var failed []string
	for _, ctx := range contexts {
		if isFailedConclusion(ctx.Conclusion) {
			failed = append(failed, fmt.Sprintf("%s (%s)", ctx.Name, ctx.URL))
		}
	}
	if len(failed) == 0 {
		return "checks failed"
	}
	return "failed checks: " + strings.Join(failed, ", ")
}
