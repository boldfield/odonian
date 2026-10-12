# Harness lease renewal for every dispatch

## Problem

A claimed task holds a lease. If the lease lapses, `claimableSQL` (`internal/store/store.go`) makes an `in_progress` task claimable again, so another agent can take over work that is still running.

Today only preclaimed research dispatches get their lease renewed by the harness: `dispatch()` in `harness/agent.sh` starts `monitor_dispatch` only when `P_TASK` is set. Build, design and review dispatches (the legacy path) rely on the model running `odonian heartbeat` itself:

- `harness/prompts/*/build/implement.md` tells implementers to heartbeat before and after slow steps.
- `harness/prompts/pull_request/build/review.md` does not mention heartbeats at all.
- A model blocked inside one long tool call cannot heartbeat, whatever the prompt says. Reviewers are required to wait for exact-head CI, and on projects with macOS CI that wait is 20 to 35 minutes inside one `gh run watch` or `gh pr checks --watch` call.

Observed on 2026-10-11 on the ohand board: both reviews of one PR (one Opus, one GPT) lost their leases after about 30 minutes, while both reviewers were alive and watching the macOS run. The other two reviewer pods were busy, so nobody reclaimed them before an operator renewed the leases by hand. With an idle pod, the same review would have been claimed twice, producing a duplicate review and a conflicting late verdict.

The workaround a model reaches for, a background `while true; do odonian heartbeat ...; sleep 300; done`, is worse. On 2026-10-10 a fable worker died while such a loop kept running with parent PID 1, renewing a dead task's lease for 27 hours.

## Change

The harness renews the lease of the task a legacy dispatch is working on, for the life of the model process, the same way it already does for research.

1. **Which task.** A legacy dispatch already has the harness-selected task: `dispatch()` exports `ODONIAN_SELECTED_TASK_ID` and `odonian next` is pinned to it. The monitor watches exactly that id. It does not search by assignee and never renews any other task.
2. **Before the claim.** The model claims inside its session, so the task starts out unowned. Each interval the monitor reads the task (`odonian show <id> --json`). It starts renewing only when the state is `in_progress` and the assignee equals `$AGENT_ID`. If the task goes `in_progress` under another agent, or reaches any state other than `ready` or `in_progress` before we own it, the monitor stops: the task was raced away or never claimed.
3. **While owned.** Each interval the monitor runs `odonian heartbeat <id> --agent "$AGENT_ID"`. On failure it reads the task. If it is still `in_progress` for us, the failure is transient: log it and retry next interval. If it is not, stop renewing. That covers submitted, blocked or transitioned states and a different assignee.
4. **No fencing on the legacy path.** Unlike research, the monitor never kills the model process for a legacy dispatch. It only renews or stops renewing. Process fencing stays research-only, and changing that is out of scope.
5. **Lifecycle.** The monitor runs as a background subshell in its own process group and ends when the model process ends. `dispatch()` kills it after the model exits on every path, including a non-zero model exit and a stop request. `cleanup()` kills it as the existing `MON_PID` backstop does. A monitor must never outlive its dispatch: that is the failure the 27-hour loop showed.
6. **Interval and cost.** Use the existing `ODONIAN_RENEW_INTERVAL_SECS`, which must stay well under the lease TTL. Calls go to the Odonian server only, never to GitHub, so they do not draw on the shared GitHub quota. Reads happen only before the claim and after a failed heartbeat.
7. **Prompts.** In the build implement prompts (`pull_request` and `local_commit`), replace "Keep your lease alive" with: the harness renews your lease while your session runs; explicit `odonian heartbeat` calls are optional and harmless; never start a background heartbeat loop or any process meant to outlive your session. Add the same sentence to the build review prompts. Other prompt text is unchanged.

## Acceptance

1. A legacy dispatch whose model claims the selected task and then runs longer than several renewal intervals produces repeated heartbeats for that task, fenced to nothing (no `--attempt`), while the model runs. No heartbeat is sent before the claim.
2. Renewal stops, without killing the model, when the task leaves `in_progress` (after a submit, for example) or is `in_progress` under another agent. A task that was never claimed gets no heartbeat.
3. After `dispatch()` returns, no monitor process remains. This holds for a successful exit, a failed model exit and a stop request, and is checked by process liveness, not by log text.
4. The research path behaves exactly as before. The existing `harness/research_admission_test.sh` scenarios pass unchanged.
5. Behaviour tests use the existing fake `odonian` and fake `claude` pattern (`harness/research_admission_test.sh` shows it) and cover criteria 1 to 3: claim then long run; never claimed; submit mid-run; raced away; model failure; stop. They are a new `harness/*_test.sh` script added to the list in `harness/scripts_test.go`, or new scenarios in an existing listed script, so `go test ./...` runs them. No vacuous assertions.
6. The prompt changes in Change step 7 are made, and no prompt still instructs heartbeats at fixed points as the only way to keep a lease.

## Out of scope

- Changing lease TTLs or server-side claim rules.
- Process fencing for legacy dispatches.
- Releasing and deploying a new fleet image. That follows the existing release procedure after merge.
