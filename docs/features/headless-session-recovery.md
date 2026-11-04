# Headless session recovery and dispatch cleanup

## Problem

A fleet dispatch runs one headless model process (`claude -p` or `codex exec`) in `dispatch()` in `harness/agent.sh`. When that process exits 0, the harness treats the dispatch as finished: it stops the lease monitor and asks for the next task. It does not check whether the model finished its task.

Models regularly end their turn while their task is still `in_progress`, because they expect a background notification to wake them. Observed on the ohand board on 2026-10-15, across all four worker pods and at least one reviewer pod, in at least 9 sessions in about four hours:

- "Still waiting on the iOS run for the new head (a597939); I'll resume when its notification arrives." The next dispatch started 4 seconds later.
- "The loop is at run 9 of 50 with no failures. I'm waiting for the scheduled wakeup."
- "Local checks are still running in the background. I'll be notified when they finish, and then I'll submit the verdict." (an Opus reviewer)

In headless mode nothing reliably resumes the session. The process exits, and the task stays `in_progress` under an agent that is no longer working on it. Two things then go wrong.

1. **Lost time and lost context.** The task waits for its lease to lapse (30 minutes), then a fresh agent claims it and starts cold. The ohand tasks A04, A07, C07 and S05b each lost one to three and a half hours this way on one day. One reviewer finished a verdict after its lease had already lapsed and been reclaimed, and the verdict was discarded.
2. **Processes that outlive the dispatch.** The Bash tool runs commands in their own process groups, so background work survives the model process. Killing the model's process group does not reach it. One worker left `while pgrep -f f17_load.sh >/dev/null; do odonian heartbeat <task> ...; sleep 60; done`. Its `pgrep` pattern matched the loop's own command line, so the loop never ended and kept a task nobody was working on leased indefinitely. An operator had to kill it. Leftover test runs and load generators also steal CPU from the next dispatch on the same pod, which skews timing-sensitive tests and soaks.

The prompts already say "Never start a background heartbeat loop or any process meant to outlive your session" (`harness/prompts/*/build/*.md`). The model above did it anyway. Prompt text alone does not prevent either failure, so the harness has to.

The fleet image pins Claude Code 2.1.281 (`deploy/fleet/Dockerfile.fleet:54`). That version warns that `claude-sonnet-5-5` "isn't described by this version's model catalog" and limits auto-compaction to a 200k-token window. This is unrelated to the early exits but is fixed here as well.

## Change

### 1. Resume a dispatch whose task is still owned (`harness/agent.sh`)

After the model process exits 0 on the legacy (build, design, review) path, the harness reads the selected task (`ODONIAN_SELECTED_TASK_ID`). If it is still `in_progress` with assignee `$AGENT_ID`, the harness resumes the same model session instead of dispatching new work.

- Start each dispatch with a known session id so it can be resumed: for Claude, `--session-id <uuid>` and then `claude -p --resume <uuid> "<resume prompt>"`; for Codex, `codex exec resume <session id>`, using the id Codex reports. Check the flags against the pinned CLI versions; if a CLI cannot resume a headless session, record that in the PR and use the fallback below for that CLI.
- The resume prompt says: you are running headless; ending your turn ends your session, and no notification, wakeup or monitor will resume you. Your task is still `in_progress`. Check the state of whatever you were waiting on, wait for it in the foreground, then submit, or block with a reason.
- At most `ODONIAN_MAX_RESUMES` resumes per dispatch (default 3). The lease monitor runs for each resumed process exactly as it does for the first.
- **Fallback.** If the task is still owned after the last resume, or the session cannot be resumed, the harness moves the task to `blocked` with a note that names the agent, the number of sessions, and the last 500 characters of the model's final message with secrets redacted. It does not leave the task `in_progress` to wait for lease expiry.
- A task that is no longer `in_progress` for this agent (submitted, blocked, or claimed by another agent) is never resumed. A non-zero exit or a stop request keeps today's handling.
- Research dispatches (`P_TASK` set) are unchanged.

### 2. Reap a dispatch's leftover processes (`harness/agent.sh`)

Every process the dispatch started is killed before the harness asks for new work.

- `dispatch()` exports a unique marker in the model's environment, `ODONIAN_DISPATCH_ID=<uuid>`. Descendants inherit it even after `setsid`.
- After the model process exits, including after the last resume, the harness finds every process whose environment carries that marker. On Linux it reads `/proc/<pid>/environ`; on macOS it uses `ps -E` or `ps eww`. It sends TERM, waits a short grace period, then sends KILL. It never signals itself, its own monitors, or any process without the marker.
- `cleanup()` does the same for the in-flight dispatch on shutdown.
- The harness logs how many processes it reaped, with each one's pid and the first 120 characters of its command, so leftover work stays visible.
- This must be safe on a developer machine (`sbx`, local runs). Only marked processes are touched. There is no "kill everything reparented to PID 1" shortcut.

### 3. Tell models they are headless (`harness/prompts/*/build/*.md`)

Add one paragraph to each of the four build prompts:

> You are running headless. Your session ends when you end your turn, and no background notification, scheduled wakeup or monitor will resume it. Never end your turn while your task is `in_progress`. To wait for CI or a long command, wait in the foreground: for example, `gh run watch <id> --exit-status --interval 60` or a polling loop, each call kept under the tool timeout and repeated until done. Then submit, or block with a reason. Anything you start in the background is killed when your session ends.

### 4. Update the CLIs in the fleet image (`deploy/fleet/Dockerfile.fleet`)

Pin a Claude Code release whose model catalog includes every Claude model on the default allowlist, and the current Codex CLI. Use exact versions, as today.

## Acceptance

1. **Resume.** With fake `odonian` and fake `claude`/`codex` (the pattern in `harness/lease_renewal_test.sh`), a model that exits 0 while its task is still `in_progress` for this agent is resumed with the same session id and the resume prompt. A model that submits on its second session ends the dispatch with no further resume. A model that never submits is resumed exactly `ODONIAN_MAX_RESUMES` times, and then the task is moved to `blocked` with the note described above. A task that was submitted, blocked or raced away is not resumed. Non-zero exits, stop requests and research dispatches behave exactly as before. The lease monitor runs during every resumed session and none survives the dispatch.
2. **Reaping.** A fake model that starts a detached background process, including one in a new session via `setsid` that loops forever, leaves no live process carrying its marker after `dispatch()` returns. A process without the marker, started by the test, is untouched. This holds on a successful exit, a non-zero exit, after resumes, and on a stop request, and the test checks it by process liveness, not by log text.
3. **Prompts.** All four build prompts contain the headless paragraph, and none still implies the model will be notified or woken later.
4. **Image.** `claude --version` and `codex --version` in the built fleet image report the pinned versions, and a `claude -p` with each default Claude model id prints no unknown-model warning. The image build in CI passes. Deploying the image (the `manifests` repo) is out of scope and stays an operator step.
5. The new tests are listed in `harness/scripts_test.go` so `go test ./...` runs them. `make check` (or the repository's documented checks) passes.

## Out of scope

- Fencing legacy heartbeats to an attempt id on the server. Reaping and the blocked fallback remove the live cause, which was an orphaned heartbeat loop for a task nobody owned. Revisit if a heartbeat from a dead session shows up again.
- Lease TTLs and claim rules.
- Deploying the new image.

## Tasks

1. **Resume owned tasks** (Change 1). Owns `dispatch()` and its helpers in `harness/agent.sh`, and a new `harness/session_resume_test.sh`.
2. **Reap leftover processes** (Change 2). Owns the reaping helpers in `harness/agent.sh`, `cleanup()`, and a new `harness/dispatch_reaping_test.sh`. Depends on task 1, because both edit `dispatch()`.
3. **Headless prompt paragraph** (Change 3). Owns the four build prompts.
4. **Fleet image CLI bump** (Change 4). Owns `deploy/fleet/Dockerfile.fleet`.
