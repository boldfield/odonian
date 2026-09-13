# Running Odonian

Build, run, test, and deploy the server; run the fleet on a laptop, in a sandbox, or on
Kubernetes. Configuration variables are in [`configuration.md`](./configuration.md).

## Build

Requires Go 1.25.6 or newer, `git`, and `make`. From the repository root:

```bash
make build      # ./bin/odonian        (server + CLI, one binary)
make tui        # ./bin/odonian-tui    (optional terminal UI)
make test       # go test ./...
make check      # gofmt, go vet, go mod tidy drift
```

CI runs `make check`, `make build`, and the tests on every push and pull request, then builds the
Docker image and smoke-tests `/healthz`.

## Server

```bash
export ODONIAN_TOKEN="your-secret-token"
export ODONIAN_DB="/path/to/odonian.db"     # optional, default ./odonian.db
export ODONIAN_ADDR=":8080"                 # optional
./bin/odonian server
```

The database is created on first run. The server is a single process: the REST API plus one
reconcile runner that ticks the notifier and PR-watch reconcilers. Nothing else needs to be
running.

The container image is distroless (`gcr.io/distroless/static:nonroot`) with the single static
binary as its entrypoint. There is no shell in it; anything that needs to touch the database file
directly runs as a separate pod that mounts the same volume.

## TUI

`odonian-tui` shows projects, documents, and tasks by state with filtering and search, and offers
confirm-gated archive and unarchive actions. It talks to the server over the same API; the URL and token come from
`~/.config/odonian/config.toml`, the `ODONIAN_URL` / `ODONIAN_TOKEN` environment, or `--url` / `--token` flags, in that order of precedence from lowest to highest.

```bash
./bin/odonian-tui
```

## The fleet in your development environment

Run workers and reviewers in a disposable sandbox or container with an authenticated Claude
Code CLI, `git`, `gh`, `jq`, and Bash 3.2+. They run with agent permission prompts disabled.
The [guided sandbox demo](./demo.md) is the first-run path. The setup below connects the harness
to your own repository and board; that repository must have an `origin/main` branch.

The harness is one engine, [`harness/agent.sh`](../harness/agent.sh), parameterized by `--kind`.
The wrappers are one-liners:

| Wrapper | Claims |
|---|---|
| `worker.sh [slot]` | `implement` tasks, any model; the task pins the model |
| `reviewer.sh [slot]` | `review` tasks, any model |
| `merger.sh [slot]` | `merge` tasks; no LLM, pure REST |
| `fleet.sh --kind <k> --count <n>` | spawns and manages several slots at once |

Each slot has a persistent agent id and its own detached git worktree, so parallel agents never
collide. One `claude -p` (or `codex exec`) dispatch per task; the prompt under
`harness/prompts/<delivery_mode>/<track>/<kind>.md` is read fresh each dispatch, so editing it
applies to the next task without a restart. For workers and reviewers, Ctrl-C sends `SIGTERM`
to the active agent session and exits; it does not wait for the task to finish. Cleanup removes
the slot worktree in pull-request mode, so unpushed work may be lost. To let a task finish, wait
until it has submitted before stopping its slot. A second Ctrl-C force-quits. The non-LLM merger
also has no guaranteed drain: Ctrl-C can interrupt its foreground `odonian merge` command
between merging on GitHub and recording completion on the board. Check both the PR and task
state after interrupting it.

Start the server as above. In the environment that will run the fleet, build Odonian.
Create the configuration directory and edit the example values before
launching a worker. Preserve any configuration you already have:

```bash
# From the Odonian checkout:
mkdir -p ~/.odonian
test -e ~/.odonian/env || cp harness/env.example ~/.odonian/env
chmod 600 ~/.odonian/env
${EDITOR:-vi} ~/.odonian/env
```

Set `ODONIAN_URL` to a server reachable from this environment, `ODONIAN_TOKEN` to that server's
token, `ODONIAN_PROJECT` to the full project UUID, and `ODONIAN_REPO` to the absolute path of
that project's local checkout. A server on your host is not the sandbox's `localhost`.
Use the [API](./api.md#full-lifecycle-walkthrough) to create a project, register a document,
create tasks, and promote the ones you want worked. The API walkthrough uses example work;
replace its repository and specs with your own when preparing a real board.

For worker and reviewer pull-request operations, authenticate `gh` in the fleet environment or configure the
[per-owner forge tokens](../harness/README.md#per-owner-github-auth-forge-tokens).
The server also needs its own `FORGE_TOKENS` file for PR-watch to observe merges and reviews.
The merger requires its own matching owner token in `FORGE_TOKENS` (default
`~/.odonian/forge-tokens`); it does not use `gh auth login` or `GH_TOKEN` as a fallback.

In each fleet terminal, from the Odonian checkout, add the built CLI to `PATH` and run one wrapper:

```bash
export PATH="$PWD/bin:$PATH"
cd harness
./worker.sh worker-1      # each in its own terminal
./reviewer.sh reviewer-1
./merger.sh merger-1      # only if any task has agent_merge=true
```

Run one wrapper per terminal; each is a foreground loop. The wrappers source `~/.odonian/env`.

`ODONIAN_PROJECT=all` switches to multi-project mode: the agent discovers every board with
claimable work and clones repos on demand under `$ODONIAN_HOME/repos`, evicting by disk watermark.
[`harness/README.md`](../harness/README.md) covers slots, code-versus-state layout, per-owner
GitHub auth, and project scope in depth.

## Everything inside an `sbx` sandbox

[`harness/sbx.sh`](../harness/sbx.sh) boots the whole stack, server plus fleet, inside an `sbx`
sandbox with all harness state under `/tmp/odonian`. The demo uses a throwaway repository;
`--repo` instead lets the CLI create branches and worktrees in the repository you provide.
Only pull-request mode pushes to GitHub. See the [sandbox shell setup](./demo.md#1-enter-a-sandbox-with-the-repository-mounted)
before running these commands.

```bash
# Drain a project backed by a LOCAL git repo (local_commit mode: the CLI commits; no PR, no forge)
bash harness/sbx.sh --project <uuid> --repo <path-to-local-git-repo>

# A fully self-contained throwaway demo: creates its own repo, project, and board, and posts one
# example task (harness/seed-demo.sh). Walkthrough: docs/demo.md
bash harness/sbx.sh --seed-demo

# Drain every board over GitHub (needs forge tokens)
bash harness/sbx.sh --project all --delivery-mode pull_request

# Manage without Ctrl-C (useful when launched from inside an agent)
bash harness/sbx.sh status
bash harness/sbx.sh stop
```

Options: `--workers N` and `--reviewers N` (default 2 each), `--port P` (8080),
`--delivery-mode pull_request|local_commit` (default `local_commit`), `--reviewer-model TIER` to
pin reviewers to one tier (default: dynamic, the task's own model), `--worktree-home <path>`.
`CLAUDE_CODE_OAUTH_TOKEN` in the environment is forwarded to the fleet; otherwise the sandbox's own
`claude` login is used.

A nested `claude -p` inside a sandbox needs `--allow-dangerously-skip-permissions` alongside
`--dangerously-skip-permissions`; `sbx.sh` passes it through `AGENT_CLAUDE_FLAGS`, which
`agent.sh` appends and which is empty and harmless outside a sandbox.

## Delivery modes

- **`pull_request`** (default): the worker pushes a branch named from the task id and opens a PR.
  Review happens on the PR; the merge gate is the human on GitHub, PR-watch, or the merger for
  `agent_merge=true` tasks.
- **`local_commit`**: no forge. The CLI creates a per-task worktree (`odonian wt-ensure`), the
  worker commits into it, and `odonian approve` freezes the branch and cleans the worktree for a
  human to assemble. Serial by design; used for sandboxed and offline work.

## Release and deployment

The server ships as a single container image, and this repository's responsibility is building
and pushing it (`make release`). Deploying it is owned by your own infrastructure repo: a
kustomization with a namespace, PVC, deployment, and service is all it takes. The deployment needs
`replicas: 1` and `strategy: Recreate`, because SQLite is single-writer. Relevant targets here:

| Target | Purpose |
|---|---|
| `make release` | Build and push a versioned server image |
| `make deploy` | Roll the server image out |
| `make versions` | Show what is built, tagged, and deployed |
| `make fleet-image`, `make merger-image` | Build and push the worker/reviewer and merger images |
| `make fleet-deploy`, `make merger-deploy`, `make diff-fleet` | Apply or diff the fleet manifests in `deploy/fleet/` |
| `make codex-auth`, `make codex-auth-check` | Seed and verify the Codex credential secret |

`deploy/fleet/` stays in this repository because it holds build inputs (`Dockerfile.fleet`,
`Dockerfile.merger`, the fleet entrypoint) that are coupled to the harness. Cluster topology,
image registry, secrets, and the Codex credential rotation hazard are documented in
[`deploy/fleet/README.md`](../deploy/fleet/README.md).
