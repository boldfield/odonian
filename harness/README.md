# Odonian fleet harness

The headless pull-worker fleet that drains an Odonian board: model-pinned agents claim their own
work, run it via `claude -p`, and submit — Haiku implements, Opus reviews, the human merges.

## Requirements

- `odonian` CLI must be on `PATH` for board polling and work discovery
- An authenticated `claude` CLI for worker/reviewer dispatches; run them in a disposable sandbox
  or container because permission prompts are disabled
- `gh` (GitHub CLI) for repo cloning and git authentication  
- `jq` for JSON processing
- Bash 3.2+ (macOS ships 3.2; Linux and others typically have 4+)

For the complete build, configuration, authentication, and launch sequence, see
[`docs/running.md`](../docs/running.md#the-fleet-in-your-development-environment).

## One engine

`agent.sh` is the whole loop, parameterized by `--kind`. The wrappers are one-liners:

| Wrapper | Role |
|---|---|
| `worker.sh [slot]`  | Generic implementer — claims `kind=implement` across all models | 
| `reviewer.sh [slot]` | Generic reviewer — claims `kind=review` across all models |

Run a few in separate terminals, each with a **distinct slot**:

    ODONIAN_PROJECT=<id> ODONIAN_REPO=~/projects/<repo> ./worker.sh worker-1

Each agent takes a persistent id (per slot), stands up its own detached git worktree, polls for
claimable work of its `kind`, and the task specifies the model. One `claude -p` dispatch per task,
then repeats. Ctrl-C interrupts the active worker/reviewer session and exits. Pull-request slot
worktrees are removed during cleanup, including unpushed changes; wait for submission first if
you want the task to finish. Ctrl-C again force-quits. The non-LLM merger finishes its current
merge operation before stopping.

## Code vs. state

The engine keeps **code** and **state** in separate trees, so they never mix:

- **Code + prompts** (`agent.sh`, the wrappers, `prompts/<delivery_mode>/<track>/<kind>.md` — e.g. `prompts/pull_request/build/review.md`) — versioned
  *here*, in the repo. The engine finds them via its own location, and reads the prompt **fresh each
  dispatch**, so editing it applies to the next task with no restart.
- **State + config** — lives under `$ODONIAN_HOME` (default `~/.odonian`), un-versioned: `env`
  (URL / token / project), `agents/<slot>.id` (persistent ids), `wt-*` (worktrees), `repos/`
  (on-demand repo clones in multi-project mode), and optionally `forge-tokens` (per-owner PATs).
  Create the directory with `mkdir -p ~/.odonian`, copy `env.example` → `~/.odonian/env` if no
  configuration exists, and fill in the URL, token, full project UUID, and repository path.

## Per-owner GitHub auth (`forge-tokens`)

A multi-project fleet may span repos owned by **different GitHub identities** (e.g. `boldfield/*`
and the dynamically-created `fAIctory/*`). A single `gh` login can't push to both, and you can't
pre-assign a "fAIctory fleet" because those repos appear at runtime — so the worker resolves the
right token **from the repo owner** (which Odonian already exposes via each project's `repo`).

Optional `~/.odonian/forge-tokens` pairs an owner with a PAT (`owner=token` per line; `#` comments
ok). The worker derives the owner from the project's repo URL, exports that owner's token as
`GH_TOKEN` for the clone + the dispatched worker's `git push`/`gh`, and **falls back to your default
`gh` auth** when an owner has no entry. The server separately reads its own `FORGE_TOKENS` file
for PR-watch and stale-PR cleanup; without a matching owner token it skips those checks, even
for public repositories. The merger process also needs forge credentials. Tokens are not stored
in the board database or returned by the API.

    cp harness/forge-tokens.example ~/.odonian/forge-tokens
    # add e.g.  fAIctory=ghp_…   then:
    chmod 600 ~/.odonian/forge-tokens

**Run the scripts straight from the repo — no symlinks.** Because the two trees are independent, you
invoke the code from `harness/` and it uses `~/.odonian` purely for state:

    cd ~/projects/odonian/harness
    ODONIAN_PROJECT=all ./worker.sh worker-1

(Symlinking the wrappers *into* `~/.odonian` would drop code into the state tree next to `repos/`
and `wt-*` — don't. If you want to invoke from anywhere, add `harness/` to `PATH` or alias the
wrappers; `$ODONIAN_HOME` still points the engine at your state.)

## Project scope: single vs. multi

Set by `ODONIAN_PROJECT`:

- **A project uuid → single-project** (the default; back-compat). The slot is pinned to that board
  and `ODONIAN_REPO`, with one worktree — exactly as before, including the repo-guard.
- **`all` (or empty) → multi-project.** The slot polls `GET /projects?claimable=true&model=&kind=`
  (the v0.4.0 work-discovery filter), shuffles the projects that have its work, and drains them all —
  cloning each project's repo on demand into `~/.odonian/repos/<owner-repo>/` and standing up a
  per-`(slot, repo)` worktree `wt-<slot>-<owner-repo>` (a worktree can't span repositories).
  `ODONIAN_PROJECTS=<id,id,…>` optionally restricts which projects multi-mode will touch.

      ODONIAN_PROJECT=all ./worker.sh worker-1     # one slot, all boards

  One `claude -p` task per discovery pass, then re-poll with a fresh shuffle — so N parallel slots
  spread across all work-bearing projects instead of serializing on one board. The repo-guard
  inverts from "refuse on mismatch" to "set up the project's repo." Assumes each repo's default
  branch is `main` (master-default repos need the prompt parameterized — not yet supported).
