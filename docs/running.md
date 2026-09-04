# Running Odonian

Build, run, test, and deploy the server; run the fleet on a laptop, in a sandbox, or on
Kubernetes. Configuration variables are in [`configuration.md`](./configuration.md).

## Build

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

## The fleet on a laptop

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
applies to the next task without a restart. Ctrl-C finishes the in-flight task and stops; a
second Ctrl-C force-quits.

```bash
cp harness/env.example ~/.odonian/env    # URL, token, project, repo
cd harness
./worker.sh worker-1      # each in its own terminal
./reviewer.sh reviewer-1
./merger.sh merger-1      # only if any task has agent_merge=true
```

`ODONIAN_PROJECT=all` switches to multi-project mode: the agent discovers every board with
claimable work and clones repos on demand under `$ODONIAN_HOME/repos`, evicting by disk watermark.
[`harness/README.md`](../harness/README.md) covers slots, code-versus-state layout, per-owner
GitHub auth, and project scope in depth.

## Everything inside an `sbx` sandbox

[`harness/sbx.sh`](../harness/sbx.sh) boots the whole stack, server plus fleet, inside an `sbx`
sandbox with all state under `/tmp/odonian`. Nothing touches `~/.odonian`, your repos, or GitHub
unless you ask for pull-request mode.

```bash
# Drain a project backed by a LOCAL git repo (local_commit mode: the CLI commits; no PR, no forge)
bash harness/sbx.sh --project <uuid> --repo <path-to-local-git-repo>

# A fully self-contained throwaway demo: creates its own repo, project, and board
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
