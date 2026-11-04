# Feature: sbx agent bootstrap (provision the sandbox: toolchain, agents, auth, board config)

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Implemented components with a boot-order limitation; historical specification.**
> Container-local builds, toolchain checks, model routing, authentication helpers and board setup exist in [sbx.sh](../../harness/sbx.sh), [sbx-agent-setup.sh](../../harness/sbx-agent-setup.sh) and the [Makefile](../../Makefile). The standalone setup script can install both CLIs, but sbx.sh still requires Claude on PATH before it invokes that setup; the original blanket fresh-container promise is not fully met. Model defaults below have [subsequently changed](../specs/2026-09-30-sandbox-and-task-model-defaults.md). See the [harness guide](../../harness/README.md) for current operation.

Status: feature spec, 2026-08-10
Kind: feature_spec (for project Odonian)

## What this is

`harness/sbx.sh` already boots the whole Odonian stack — server + worker/reviewer fleet — inside an
`sbx` sandbox container with all state under `/tmp/odonian`. What it does **not** do is provision
the container it runs in.

**The sandbox image is externally defined.** It is supplied by Docker Sandboxes, not built from this
repo. `deploy/fleet/Dockerfile.fleet` — which does install both CLIs and the whole toolchain — is the
**cluster** fleet image and has nothing to do with the sandbox. Nothing in this repository controls
what the sandbox image contains, and its contents can change underneath us when Docker updates it.

Everything the stack needs must therefore be **installed into the running container at boot time and
verified**, never assumed. That includes the `claude` CLI itself: a fresh sandbox happening to ship an
authenticated `claude` today is incidental, not a contract. This feature closes that gap so that a
sandbox can be provisioned from whatever the external image happens to be, and an interactive
`claude` session inside it can drive the local board and create tasks reviewed by **both `opus` and
`gpt-5.5`**.

Scope is the **sandbox only**. The cluster fleet is untouched.

## Measured starting state (verified in the running `claude-odonian-sbx` container)

Treat this table as *observed today*, not as a contract. Every row is a property of an external
image that can change; the bootstrap must verify each rather than rely on it.

| Thing | Observed |
|---|---|
| container user / home | `agent`, `HOME=/home/agent` |
| `claude` | present at `/home/agent/.local/bin/claude`, authenticated via `~/.claude/.credentials.json` (307 bytes, mode 600, **not a mount** — written by sbx's own login flow, and unreproducible from this repo) |
| `codex` | absent; `~/.codex` does not exist |
| `node`, `npm`, `git`, `gh`, `jq` | present |
| `go` | `go1.26.0 linux/arm64` (`go.mod` requires `1.25.6`) |
| npm global prefix | `/usr/local/share/npm-global`, root-owned (not writable by `agent`) |
| `sudo -n` | works — passwordless |
| `~/.claude/skills` | does not exist |
| repo mounts | bind-mounted **read-write at the same absolute paths as the host**, e.g. `/Users/boldfield/projects/odonian` |
| container arch | `aarch64` |

## The gaps

### 1. `sbx.sh` trusts a pre-built binary whose architecture it never checks

`harness/sbx.sh` builds the CLI only when the mounted repo's `bin/odonian` is not executable. The
test is the **executable bit alone** — architecture is never considered. Because the repo is mounted
read-write at the same path on both sides, that binary is shared between a macOS host and a Linux
container, and the check is wrong in both directions:

- A **host-built** binary is present and executable, so the container skips the build and puts a
  Mach-O binary on `PATH`. Every `odonian` call inside the container then dies with an exec format
  error — at dispatch time, deep in a worker log, not at boot.
- A **container-built** binary is written back through the mount and **clobbers the host's**
  `bin/odonian`.

This is not hypothetical; it is the current state of the working copy. `bin/odonian` on the host is
an `ELF 64-bit LSB executable, ARM aarch64` — built inside the container and written back out.
Running it on the host gives `exec format error`; the identical file inside the container reports
`odonian version v0.15.0-17-g99af944`.

The script's own stated invariant is that everything it writes lives under `/tmp/odonian`; the
mounted `bin/` was carved out as a deliberate exception, and that exception is the bug.

### 2. Neither agent CLI is installed — and the boot now hard-fails without them

`sbx.sh` gained a presence preflight in #292: it dies if either `claude` or `codex` is missing from
`PATH`, deliberately, so a missing CLI surfaces at boot instead of as `dispatch exited rc=127`
buried in a worker log behind the engine's backoff. That is the right behaviour, and it raises the
stakes here: **`codex` is not installed in the sandbox**, so `sbx.sh` now fails closed on a fresh
sandbox until something installs it. The bootstrap must install both CLIs, not inherit them.

There is also no toolchain preflight: the build needs a Go new enough for `go.mod`, which today the
image happens to satisfy and tomorrow may not.

### 3. `claude` has no reproducible authentication path

The cluster authenticates `claude` through a `CLAUDE_CODE_OAUTH_TOKEN` environment variable sourced
from a secret (see the worker and reviewer deployments). The sandbox has **no equivalent** — its
credentials file is opaque state written by sbx's interactive login, which this repo cannot create,
refresh, or validate.

#292 closed half of this: `sbx.sh` now checks that both CLIs are **present**. It does not check that
they are **authenticated**, and presence is the weaker half. An installed-but-expired `claude` still
boots **green** — health check passes, fleet starts, agents claim tasks — and every dispatch fails
inside `agent.sh`, discoverable only by reading worker logs. Closing that remaining half is what
this gap is now about.

Note the resulting asymmetry this feature must remove: of the four provisioning concerns, three have
a repo-side mechanism for the cluster and this feature adds two for the sandbox — but sandbox
`claude` auth has **no mechanism anywhere**.

### 4. `codex` is not installed

The npm global prefix is root-owned, but passwordless `sudo` is available, so a global install is
the simple correct path — no per-user prefix workaround needed.

### 5. `codex` has no credentials, and `codex login` cannot run headless

It is an interactive browser flow. Auth must be *seeded* by copying the host's `~/.codex/auth.json`
in — the same move `make codex-auth` already makes for the cluster, including its validation that a
refresh token is present. Because the copy originates on the host, this half **cannot run inside the
container**; it needs `sbx cp` and is therefore a separate host-side entry point.

### 6. The sandbox server rejects `gpt-5.5`, and would misroute it anyway

`sbx.sh` starts the server with `ODONIAN_MODELS="haiku,sonnet,opus,fable"` — no `gpt-5.5` — so a
task carrying `review_models: ["opus","gpt-5.5"]` fails at create time with `UNKNOWN_MODEL`. And
`sbx.sh` never sets `AGENT_CODEX_MODELS`, so even once allowed, `agent.sh` would dispatch that review
as `claude -p --model gpt-5.5` instead of routing it through `codex exec`.

Its escalation config has also drifted from production: it carries the original thresholds and
predates `ODONIAN_ESCALATION_LADDER` existing at all. Production defines the ladder separately and
deliberately **excludes** `gpt-5.5`, so a review-only model never becomes an escalation tier for
implementation work.

### 7. Nothing teaches the in-container Claude the two-reviewer default

The server's fallback when `review_models` is empty is hardcoded to a single Claude reviewer, and
`skills/odonian-breakdown/scripts/odonian.sh` documents exactly that. An interactive Claude reading
it will keep creating single-reviewer tasks. Nor are the repo's four Odonian skills visible to an
in-container session at all.

## Model choice: `gpt-5.5`, not `gpt-5.6`

`gpt-5.6` was requested but is **not available on this account**. Verified against codex-cli 0.147.0:
`codex exec -m gpt-5.6` returns `warning: Model metadata for 'gpt-5.6' not found` followed by
`400 invalid_request_error: The 'gpt-5.6' model is not supported when using Codex with a ChatGPT
account`; `gpt-5.5` succeeds. `gpt-5.5` is also what the production allowlist and reviewer deployment
already use. If the account later gains `gpt-5.6` it is a one-constant swap.

## `claude` auth: `CLAUDE_CODE_OAUTH_TOKEN` is transplantable (verified)

Deliverable 3 asked whether sbx's `claude` login is deliberately sandbox-scoped — i.e. whether a
token minted outside the sandbox (`claude setup-token`) can even be handed to the `claude` binary
running inside one, the way `CLAUDE_CODE_OAUTH_TOKEN` already works for the cluster. It is not
sandbox-scoped: `CLAUDE_CODE_OAUTH_TOKEN` is read by the `claude` binary itself, generically,
independent of whatever sandboxing tool launched the process — there is no `sbx`-side gate on it.
Verified two ways:

- Setting the env var to an arbitrary (invalid) token value changes `claude auth status`'s
  `authMethod` from `none` to `oauth_token` and `loggedIn` from `false` to `true` — the binary
  picks the var up unconditionally, before any validation happens.
- `claude -p` with that same invalid token makes a live call and fails fast with `Failed to
  authenticate. API Error: 401 OAuth access token is invalid.` — proof the token actually reaches
  the OAuth-validation path rather than being silently ignored.

This is why `harness/sbx.sh` can simply forward an inherited `CLAUDE_CODE_OAUTH_TOKEN` into the
fleet env (§ Deliverable 3) rather than needing an `sbx cp`/`sbx exec` host-side seeding step like
codex's `auth.json` does (Deliverable 4) — codex's *credential file* needs a host-to-container copy
because there's no equivalent env-var entry point for it, not because sandboxes generically block
transplanted auth.

The same testing also showed `claude auth status` is the wrong signal for the preflight: it only
inspects local credential *shape* and never calls the network, so a structurally-valid-but-garbage
`~/.claude/.credentials.json` (bogus token, `expiresAt` years in the past) still reports
`loggedIn: true`. It cannot distinguish "authenticated" from "was, once, configured" — exactly the
expired-but-present case the preflight exists to catch. `harness/sbx.sh`'s preflight therefore uses
a trivial live `claude -p` call instead (bounded by `--max-budget-usd`), which does exercise the
real validation path and fails fast on bad/expired/revoked auth.

## Guiding principle

**Verify, never assume.** Every step provisions against an image this repo does not control. Each
step must check the resulting state and fail loudly at boot with an actionable message, rather than
letting a missing or wrong-architecture component surface later as an opaque dispatch failure. A
green boot must mean the stack can actually do work.

## Scope

### Deliverable 1 — arch-correct, container-local build of the `odonian` binary

Fix the build step in `harness/sbx.sh` so the binary it runs is always built for the machine running
it, and so it never writes into the mounted repo.

- Build into a container-local location under the harness state directory rather than the mounted
  repo's `bin/`, and put that on `PATH` ahead of anything else. This restores the script's own
  "everything under the state directory" invariant and removes the cross-contamination in both
  directions at once.
- Never trust a pre-existing binary on the basis of the executable bit. Either rebuild
  unconditionally, or validate that an existing binary actually runs on this machine and rebuild if
  it does not — pick one and say why in a comment.
- Preflight the toolchain before building: a Go toolchain must be present and satisfy `go.mod`. If it
  is absent or too old, fail immediately with a message naming the required version, rather than
  emitting a wall of compiler output.
- Do not modify or delete the host's existing `bin/odonian`. Leaving a stale wrong-arch binary in
  the mounted repo is acceptable; silently overwriting it is not.

### Deliverable 2 — install and verify both agent CLIs (`harness/sbx-agent-setup.sh`, in-container)

A new harness script that provisions agent tooling and board config inside the container. Model its
structure on the existing harness scripts: same real-directory resolution preamble, same argument
parsing style, same logging helpers. Idempotent — re-running is a clean no-op.

- **Install both `claude` and `codex`** as first-class steps. Neither is assumed present. Use the
  system package manager for global installs with `sudo`; do not build a per-user prefix workaround.
- **Never disturb a working install.** If a CLI is already present, leave it and its stored
  credentials alone. The goal is a guaranteed-present CLI, not a guaranteed-fresh one.
- **Verify after installing.** Both binaries must resolve on `PATH` and report a version. A package
  manager exiting zero is not evidence of a working binary.
- **Wire the Odonian skills in.** Link the repo's four skills — `odonian-board`,
  `odonian-breakdown`, `odonian-ops`, `review` — into the location an in-container `claude` session
  reads skills from. Symlink rather than copy, so editing a skill in the mounted repo takes effect
  with no re-provisioning; this mirrors the harness's existing split between versioned code in the
  repo and unversioned state elsewhere. Resolve the repo path from the script's own location, never
  a hardcoded absolute path.
- **Write board config for the in-container Claude** — a project instructions file stating that the
  local board is at localhost on the configured port with the fixed sandbox token, that it is a
  throwaway local board and explicitly not the production cluster, and that every task it creates
  must carry both review models unless the human overrides. State the reason for the pair — two
  independent reviewers, one Claude and one Codex — so the rule survives paraphrase.
- **Write a `claude` settings file** granting a permission allowlist for routine local-board traffic:
  HTTP calls to the local server port, the `odonian` CLI, and git. Merge into existing settings
  rather than overwriting.
- Take the port as an argument, defaulting to the boot script's default.

### Deliverable 3 — reproducible `claude` authentication + a real preflight

Give `claude` the same explicit, repo-side, verifiable auth path this feature gives `codex`.

- Support supplying claude credentials via the **same OAuth token environment variable the cluster
  deployments already use**, plumbed through the sandbox fleet environment the way `sbx.sh` already
  handles its other fleet variables.
- Fall back to an existing valid credentials file when one is present, so a working sandbox keeps
  working and nobody is forced to mint a token for the common case.
- **Extend `sbx.sh`'s existing preflight from presence to authentication.** #292 already added the
  `command -v` checks for both CLIs — do not re-add them. What is missing is verifying that `claude`
  is actually *authenticated*, failing with an actionable message when it is not. This is the
  highest-value part of this deliverable: it converts a silent, log-only dispatch failure into a
  loud boot failure. Prefer a check that does not consume meaningful quota on every boot; if a
  trivial invocation is the only reliable signal, that is acceptable but should be justified in a
  comment.
- If sbx's login proves to be sandbox-scoped such that a token cannot be transplanted, then detect
  and report that clearly instead of seeding. A loud preflight is the requirement; seeding is the
  preferred implementation, not the goal.

### Deliverable 4 — host-side `codex` auth seeding

An entry point that runs **on the host** and copies `~/.codex/auth.json` into the container at the
agent user's codex config directory, mode `0600`, owned by that user.

- Preflight: the host file must exist and must contain a refresh token, with the same two distinct,
  actionable messages `make codex-auth` already emits.
- Accept the sandbox name as an argument.
- Verify after seeding by running a trivial `gpt-5.5` invocation inside the container and reporting
  the result. A silent copy that leaves codex unusable is the failure this step exists to prevent.
- Place it wherever best matches existing conventions — a Makefile target beside the existing codex
  targets, or a mode of the harness script — and justify the choice in a comment.

### Deliverable 5 — `sbx.sh` model config alignment

- Add `gpt-5.5` to the server's `ODONIAN_MODELS`.
- Set `AGENT_CODEX_MODELS` to `gpt-5.5` in the generated fleet environment file **and** in the
  exported environment the fleet children inherit, matching how the script already handles its other
  fleet variables, so `agent.sh` routes those reviews through `codex exec`.
- Adopt the separate escalation ladder that excludes `gpt-5.5`, and update thresholds to current
  production values. The production manifest is the source of truth for both — read it, do not guess.
  Carry over its rationale comments.

### Deliverable 6 — correct the documented review default

`skills/odonian-breakdown/scripts/odonian.sh` states the default is a single Claude reviewer.
Update its usage text and example task object so the two-reviewer pair is what a reader copies, and
correct any matching claim in the skill's own instructions. Be precise: the server's fallback is
unchanged and remains a single reviewer — the accurate statement is that callers should pass the pair
explicitly. Documentation only; no server or deployment changes.

## Non-scope

- **The cluster fleet, its image, its deployments, and the `manifests` repo.**
  `deploy/fleet/Dockerfile.fleet` is the cluster image and is explicitly **not** the sandbox image;
  nothing here changes it.
- **No change to the server's hardcoded single-reviewer fallback.** Making it configurable is a
  reasonable follow-up but affects every deployment including production.
- **Foreman** is untouched.
- **No `gpt-5.6`** until the account supports it.
- **Not building our own sandbox image.** This feature provisions the external image at runtime.
  Publishing a purpose-built sandbox image is a legitimate alternative worth considering later, but
  it is a much larger change and is out of scope here.

## Acceptance criteria

1. After boot, the `odonian` binary on `PATH` inside the container executes there, and the host's
   `bin/odonian` is not modified by the boot.
2. Booting with a wrong-architecture binary already present in the mounted repo succeeds rather than
   failing at dispatch with an exec format error.
3. A missing or too-old Go toolchain fails at boot with a message naming the required version.
4. On a sandbox lacking either CLI, one run of the setup script leaves **both** `claude` and `codex`
   resolvable on `PATH` and reporting a version.
5. Re-running the setup script changes nothing and exits successfully; pre-existing installs and
   stored credentials are untouched; existing claude settings are preserved rather than clobbered.
6. `codex exec` with `gpt-5.5` completes successfully inside the container after auth seeding.
7. Missing host codex credentials and a present-but-tokenless file each produce their own clear
   message, and neither copies anything.
8. `sbx.sh` refuses to start the fleet — with an actionable message — when `claude` is missing or
   unauthenticated, instead of booting green and failing at dispatch.
9. All four skills are visible to an in-container `claude` session, and editing a skill in the
   mounted repo is reflected without re-provisioning.
10. Creating a task on the sandbox board with `review_models: ["opus","gpt-5.5"]` succeeds, and when
    it reaches review the server spawns two review tasks — the `opus` one dispatching via `claude -p`
    and the `gpt-5.5` one via `codex exec`, verified by observation.
11. `gpt-5.5` is in the sandbox allowlist but **not** in the escalation ladder: a repeatedly-rejected
    implement task escalates through the Claude tiers and never to `gpt-5.5`.
12. `harness/harness_test.sh` still passes.
