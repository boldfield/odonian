# The guided demo: one task, from board to approval

This walkthrough boots a complete Odonian stack (server plus a small worker/reviewer fleet)
inside a throwaway sandbox, posts one example task, lets a real agent implement it and a real
reviewer vote on it, and ends with that task waiting for **your** approval. Demo work and board
state live under `/tmp/odonian` in the sandbox; the fleet does not push to GitHub. The Odonian
checkout you mount supplies the scripts and is shared with the host, so it remains writable
from the sandbox.

This is a real agent run, not a simulation: the worker is `claude -p` with the `haiku` model, the
reviewer is `claude -p` with `opus`. The only thing that is fake is the repository, which the
script creates for the purpose.

## What you need

| Requirement | Why |
|---|---|
| [Docker Sandboxes](https://docs.docker.com/ai/sandboxes/) (`sbx`) | The fleet runs `claude` with permission prompts disabled, so it must run in an isolated container, never on a workstation that holds other credentials. |
| Go 1.25.6 or newer inside the sandbox | The script builds the `odonian` binary for the container's own architecture. |
| `claude` (Claude Code CLI), **logged in** | Workers and reviewers are `claude -p` dispatches. Either the sandbox's own `claude` login or a `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token`. |
| `git`, `jq`, `curl`, `bash` 3.2+ | Used by the harness. `gh` is only needed for pull-request mode, which the demo does not use. |
| `codex` (OpenAI Codex CLI) | **Optional for the demo.** The demo task is reviewed by `opus`. Without `codex` the boot prints a warning and continues; a task you add with a `gpt-5.5` reviewer would fail to dispatch. |

**Usage and cost.** The run makes real model calls on your Claude account: one boot-time
authentication probe (capped at $0.02 with `--max-budget-usd`), then `haiku` implementation
and `opus` review sessions. Competing workers may also start sessions before one wins the claim;
rejections can add more sessions. On a Claude subscription this consumes plan usage; on
an API key it is billed. In three measured runs on 2026-09-12 (arm64 sandbox, `haiku` worker,
`opus` reviewer) the task took 68, 112, and 103 seconds from `ready` to `approved`; the fleet polls
every 30 seconds between steps, so most of that is model time plus one or two poll intervals.

## 1. Enter a sandbox with the repository mounted

```bash
mkdir -p ~/src
git clone https://github.com/boldfield/odonian ~/src/odonian
cd ~/src/odonian
sbx run --name odonian-demo claude .  # first run: creates the sandbox and logs claude in
```

`sbx run claude <path>` creates a sandbox with the repository bind-mounted at the same path and
starts an interactive Claude session; log in when prompted, then exit it. That leaves the sandbox
with an authenticated `claude`. To get a plain shell in the same sandbox afterwards:

```bash
# On the host, from the Odonian checkout (also use this for a second shell):
sbx exec -it --workdir "$PWD" odonian-demo bash
```

`--workdir "$PWD"` passes the host checkout's absolute path into the sandbox. Keep that working
directory: the sandbox's home is typically `/home/agent`, so `~/src/odonian` inside it is a
different path. The `-it` flags keep the shell interactive.

Inside that shell, make sure the agent tooling is present (idempotent; installs `claude` and
`codex` if missing and wires the repo's Claude Code skills). This setup script requires `npm`
and passwordless `sudo` when a CLI needs installing, even though Codex authentication is not
needed for the demo:

```bash
bash harness/sbx-agent-setup.sh
```

If you would rather not rely on the sandbox's interactive login, mint a long-lived token on the
host with `claude setup-token` and export it inside the sandbox as `CLAUDE_CODE_OAUTH_TOKEN`
before the next step; `sbx.sh` forwards it to the fleet.

## 2. Boot the stack with the demo board

```bash
bash harness/sbx.sh --seed-demo
```

What the script does, in order, and what you should see:

1. Checks the Go toolchain against `go.mod` and builds `odonian` into `/tmp/odonian/bin`.
2. Checks that `claude` is on `PATH` **and** authenticated, with a live one-word probe.
   `claude: authenticated` is the line you want. A missing or expired login stops the boot here
   with an actionable message.
3. Starts `odonian server` on `:8080` with SQLite at `/tmp/odonian/odonian.db` and
   the fixed token `sbx-local-token`, then waits for `/healthz`.
   The database is created on the first run and reused on later runs.
4. Creates a throwaway git repository at `/tmp/odonian/repo` (a `README.md`, a `GREETINGS.md`,
   and a `Makefile` whose `check` and `test` targets pass) with a bare `origin`.
5. Creates the project `sbx-local`, a `feature_spec` document, and **one task**:
   *Append a greeting line to GREETINGS.md*, pinned to `haiku`, reviewed by `opus`,
   `agent_merge=false`, promoted to `ready`. This step is `harness/seed-demo.sh` and is
   idempotent.
6. Starts 2 workers and 2 reviewers in `local_commit` delivery mode (the CLI makes the commits;
   there is no push and no pull request).

The boot ends with a banner like:

```
[sbx] Odonian fleet is UP.
[sbx]   server     : http://localhost:8080   (token: sbx-local-token)
[sbx]   project    : 3f2a…   (repo: /tmp/odonian/repo)
[sbx]   mode       : local_commit
[sbx]   fleet      : 2 worker(s) [dynamic], 2 reviewer(s) [model: dynamic (task-specified)]
[sbx]   state/logs : /tmp/odonian  /  /tmp/odonian/logs
[sbx]
[sbx] Demo board: one task is READY (d2ef1e55-…). A worker claims it, runs claude, and commits;
[sbx] a reviewer then votes. When it reaches APPROVED it waits for YOU. In another shell:
[sbx]   export ODONIAN_URL=http://localhost:8080 ODONIAN_TOKEN=sbx-local-token ODONIAN_HOME=/tmp/odonian
[sbx]   export ODONIAN_DELIVERY_MODE=local_commit ODONIAN_REPO=/tmp/odonian/repo ODONIAN_WORKTREE_HOME=...
[sbx]   odonian pending --project 3f2a…
[sbx]   ...
```

The script stays in the foreground managing the fleet. Open a second shell in the sandbox for
the rest. IDs in the banner above are abbreviated for readability; the real banner prints full
UUIDs. Use those full UUIDs in commands.

## 3. Watch the task move

Follow the fleet logs:

```bash
tail -f /tmp/odonian/logs/workers.log /tmp/odonian/logs/reviewers.log
```

Press Ctrl-C to stop **this log viewer** before entering the next commands. Keep the shell that
runs `sbx.sh` open: Ctrl-C there stops the fleet and interrupts active agent sessions.

Each line is prefixed with the agent's slot id. The sequence to expect:

```
[worker-2-odonian-demo-efdbc1] 08:38:54 claimable implement; dispatching (haiku/build)…
[reviewer-1-odonian-demo-98c547] 08:39:40 claimable review; dispatching (opus/build)…
```

Both workers race for the claim; one wins and the other's `claude -p` session reports that nothing
is claimable and exits. That is the atomic claim doing its job, not an error.

The task itself moves `ready → in_progress` (claimed, leased) `→ review` (the worker submitted;
the board spawned one review task per reviewer model) `→ approved` (every reviewer approved).
Check it from the CLI, using the environment the banner printed:

```bash
export ODONIAN_URL=http://localhost:8080 ODONIAN_TOKEN=sbx-local-token ODONIAN_HOME=/tmp/odonian
export ODONIAN_DELIVERY_MODE=local_commit ODONIAN_REPO=/tmp/odonian/repo ODONIAN_WORKTREE_HOME=/tmp/odonian/worktrees
export PATH=/tmp/odonian/bin:$PATH

odonian pending --project <project-id>
```

The table abbreviates task IDs to eight characters. The CLI requires full UUIDs; retrieve them
with JSON output before inspecting or approving a task:

```bash
odonian pending --project <project-id> --json | jq -r '.[] | [.id, .state, .title] | @tsv'
```

The table view looks like:

```
ID        STATE     KIND       TITLE
bc02c73f  approved  implement  Append greeting line #3 to GREETINGS.md
```

The task's event timeline (also under `enter` in the TUI) from one of the measured runs:

```
15:38:44 system                        transition   backlog->ready
15:39:05 worker-2-odonian-demo-efdbc1  claim
15:39:25 worker-2-odonian-demo-efdbc1  submit
15:39:25 system                        spawn_review Round 1 with models: ["opus"]
15:40:27 reviewer-1-odonian-demo-98c547 review      approve
15:40:27 system                        transition   Aggregation: all reviewers approved
```

The optional terminal UI shows the same board by state, with the task's event timeline
(claim, submit, spawn_review, the review verdict, the aggregation) under `enter`:

```bash
make tui && ./bin/odonian-tui
```

If the reviewer rejects, the task goes back to `ready` with `review_round` incremented, and a
worker picks it up again with the reviewer's feedback in the task. After the fourth rejection of a
`haiku` task the circuit breaker supersedes it with a copy pinned to `sonnet` (the sandbox's
ladder is `haiku → sonnet → opus → fable`; the server default is `haiku → sonnet → opus`).

## 4. Inspect the work and decide

In `local_commit` mode the worker's output is a commit on a per-task `wip/<task-id>` branch in a
worktree under `/tmp/odonian/worktrees`, recorded on the task as a `commit` link.

```bash
odonian show <task-id>          # spec, state, model, links, result
odonian diff <task-id>          # the diff of the worker's commit (add --full for the whole commit)
```

Then the human step. This is the merge gate: nothing the fleet did so far has changed `main`.

```bash
odonian approve <task-id>       # approved -> done; freezes the branch and cleans up the worktree
odonian reject  <task-id> --note "..."   # back to ready with your note; a worker reworks it
```

`approve` records `done` on the board, then freezes the work as a `wi/<title-slug>` branch in
`/tmp/odonian/repo` and removes the worktree, for you to merge however you like. The two halves are
separate: if the freeze fails (for example because `ODONIAN_WORKTREE_HOME` was not exported, which
is the error `neither ODONIAN_WORKTREE_HOME nor ODONIAN_HOME is set`), the board is already `done`;
export the variable and retry only the freeze with `odonian approve --freeze-only <task-id>`. In
pull-request mode the equivalent step is merging the PR on GitHub, which the PR-watch reconciler
notices and records as `done`.

To run it again:

```bash
bash harness/seed-demo.sh --project <project-id> --again    # posts "Append greeting line #N"
```

Each `--again` task has a distinct title and a distinct line. That matters: the CLI branches a
task's worktree from the frozen `wi/<slug>` branch of the same title if one exists, so a same-titled
repeat would find the line already present. The worker then submits a `no_op` link instead of a
commit and the reviewer verifies the claim against the repository before approving. That path is
real product behaviour (it is how a task whose work already landed on `main` gets closed) and you
can see it by posting a same-titled task through the API, but it is not the demo.

## 5. Clean up

```bash
bash harness/sbx.sh stop        # stops the fleet, then the server (from any shell in the sandbox)
rm -rf /tmp/odonian             # database, repo, worktrees, logs
exit                            # leave the sandbox
sbx rm odonian-demo             # on the host, if you are done with it
```

## What was real and what was not

- **Real:** the server, the board, the atomic claim and lease, the `claude -p` worker and
  reviewer sessions, the review aggregation, the commit, and the approval gate.
- **Local only:** the repository is a throwaway with a no-op `Makefile`, and delivery is
  `local_commit`, so there is no pull request. The production shape uses `pull_request` delivery:
  the worker pushes a branch named `mr/<first 8 chars of the task id>` and opens a PR, reviewers
  comment on it with `<model>-reviewer:` markers, and a human merges on GitHub. See
  [`running.md`](./running.md) for pointing the fleet at a real repository.
