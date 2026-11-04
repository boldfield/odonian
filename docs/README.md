# Odonian documentation

Verified against source baseline `b3da896` (tag `v0.24.0`), not against any production deployment.
Paths are relative to this directory; the project README is [`../README.md`](../README.md).

Odonian is a self-hosted control plane for AI agent work: workers claim tasks from a per-project
board, reviewer agents check the result, and human approval is the default merge gate. Work is organised in three
**tracks**, `build`, `design` and `research`, that share one state machine and one claiming model.
If you are new, read the project README first; it holds the model, the state machine, the merge gate
and the trust model, and nothing below repeats them.

## Start here, by what you are trying to do

**Run it for the first time.** [`demo.md`](./demo.md) takes one task from board to approval in a
throwaway sandbox. Then [`running.md`](./running.md) for building the binary, running the server and
TUI, and pointing a fleet at your own repository.

**Write tasks for the board.** [`skills.md`](./skills.md) describes the `odonian-breakdown` skill
that turns a design into sized tasks, and the three skills for watching, operating and gating the
board. Task fields, dependencies, priority and the delivery-mode/track table are in
[`api.md`](./api.md#post-projectsidtasks). Local, forge-less delivery is in
[`running.md`](./running.md#delivery-modes).

**Run or author research work.** [`research.md`](./research.md): tracks versus kinds, what a research
task delivers, structured findings and what blocks a round, disputes and adjudication, the chain-wide
round budget, continuations, pacing, scorecards, and why evaluation never routes production work.

**Operate a board and a fleet.** [`operations.md`](./operations.md) is the operator guide.
[`configuration.md`](./configuration.md) lists configuration variables for the server, notifier,
PR-watch, CLI and harness. [`../harness/README.md`](../harness/README.md) covers the fleet engine
(slots, worktrees, per-owner forge tokens, project scope, lease renewal, dispatch backoff).
[`../deploy/fleet/README.md`](../deploy/fleet/README.md) covers the fleet on Kubernetes. The rollout
and cutover procedures are under [Runbooks](#runbooks).

**Integrate with the API or write an agent prompt.** [`api.md`](./api.md) is the REST reference and
matches the registered routes at this baseline. [`../AGENT-API.md`](../AGENT-API.md) is the runbook
an execution agent follows over the live API: the implement and review loops, research admission and
continuation contracts, evaluation CLI, exit codes and the state machine as the agent sees it.

**Maintain Odonian itself.** Design and feature documents, dated specs and the bootstrap board are
historical; see [History](#history) for how to read their status.

## Current guides

| Guide | What it covers |
|---|---|
| [`demo.md`](./demo.md) | The guided first run in an `sbx` sandbox, with expected output at each step. |
| [`running.md`](./running.md) | Build, server, CLI, TUI, the fleet on a laptop or in a sandbox, delivery modes, release and deployment targets. |
| [`operations.md`](./operations.md) | Operating a board and fleet day to day. |
| [`configuration.md`](./configuration.md) | Configuration variables, with defaults as the code applies them. |
| [`research.md`](./research.md) | The research track end to end. |
| [`api.md`](./api.md) | REST reference: projects, documents, tasks, research admission, scorecards, evaluation campaigns, errors, state machine, leases. |
| [`skills.md`](./skills.md) | The four Claude Code skills in `skills/` and how to install them. |
| [`../AGENT-API.md`](../AGENT-API.md) | Agent runbook over the live API. |
| [`../harness/README.md`](../harness/README.md) | The fleet harness engine. |
| [`../deploy/fleet/README.md`](../deploy/fleet/README.md) | Fleet images and Kubernetes deployment inputs. |

## Runbooks

Operator procedures for turning a shipped capability on, with rollback. Each sets no production
value of its own.

| Runbook | Use it when |
|---|---|
| [`runbooks/research-track-smoke-test.md`](./runbooks/research-track-smoke-test.md) | Checking a deployment runs research tasks end to end. |
| [`runbooks/research-pacing-rollout.md`](./runbooks/research-pacing-rollout.md) | Moving research pacing from `disabled` through `observe` to `enforce`. |
| [`runbooks/priority-rollout.md`](./runbooks/priority-rollout.md) | Enabling priority-aware dispatch. |
| [`runbooks/reviewer-evaluation-rollout.md`](./runbooks/reviewer-evaluation-rollout.md) | Running one isolated reviewer-comparison campaign and reading its report. |
| [`runbooks/evaluation-adapter-guide.md`](./runbooks/evaluation-adapter-guide.md) | Writing a new candidate-reviewer adapter against the host contract. |
| [`runbooks/muse-code-adapter.md`](./runbooks/muse-code-adapter.md) | Installing and registering the bundled Muse Code candidate adapter. |
| [`runbooks/odonian-cutover.md`](./runbooks/odonian-cutover.md) | Historical cutover procedure; see its status banner. |

## History

Everything below records what was designed or decided at a point in time. Each file carries a
status banner saying whether it shipped, was partly shipped, was not built, or was superseded, with a
pointer to the current guide that describes today's behaviour. Where a historical document and a
current guide disagree, the current guide and the code win.

- [`../DESIGN.md`](../DESIGN.md): the approved MVP design (June 2026): state machine, atomic
  claiming, review semantics.
- [`local-commit-tasks.md`](./local-commit-tasks.md): the original local-delivery task breakdown;
  current operation is documented in `running.md` and `AGENT-API.md`.
- [`design-orchestration-restructure.md`](./design-orchestration-restructure.md): the June 2026
  design for splitting substrate and orchestration across repositories.
- [`features/`](./features/): feature designs, one file per capability. At this baseline:
  `model-and-review`, `tui`, `sbx-agent-bootstrap`, `claude-fleet-auth-targets`,
  `harness-lease-renewal`, `urgent-work-queue`, `dispatch-failure-handling`, `research-track`,
  `research-track-verification`, `research-continuations`,
  `research-pacing-and-reviewer-evaluation`.
- [`specs/`](./specs/): dated specs and postmortems, indexed with their status in
  [`specs/README.md`](./specs/README.md). This is where the incident write-ups live
  (`2026-07-20-stall-detection.md` for the July review-queue deadlock; the October rate-limit
  orphaning is in `features/dispatch-failure-handling.md`).
- [`../AGENT.md`](../AGENT.md) and [`../board/`](../board/README.md): the text-file board and runbook
  used to bootstrap the first release before the API existed.

## Conventions used across these docs

- **Tracks versus kinds.** `kind` is `implement`, `review` or `merge`; `track` is `build`, `design`
  or `research`. The README and [`research.md`](./research.md) explain the difference; the API
  reference uses both words precisely.
- **Model names are strings.** Tier names (`haiku`, `sonnet`, `opus`) are the examples in overviews.
  A deployment's real allowlist (`ODONIAN_MODELS`), ladder and reviewer identities appear only in
  configured examples and runbooks.
- **"Verified against source baseline."** A guide marked this way was checked against the code at the
  commit it names. It does not claim that any particular deployment is configured that way.
- **Defaults are what the code does when unset.** Where a guide and
  [`configuration.md`](./configuration.md) differ on a default, treat it as a docs bug and report it.
