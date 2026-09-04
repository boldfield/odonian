# Claude Code skills

Odonian ships four [Claude Code](https://docs.claude.com/claude-code) skills under
[`skills/`](../skills/). Each is a directory with a `SKILL.md` and triggers on natural-language
requests inside an interactive `claude` session. Together they cover the human-facing ends of the
workflow: filling the board, watching it, unsticking it, and draining the merge gate. The
model-pinned fleet does everything in between.

Every skill needs `ODONIAN_URL` and `ODONIAN_TOKEN` in the environment. `review` in
`local_commit` mode also needs `ODONIAN_REPO`.

## `odonian-breakdown`: intent → board

[`skills/odonian-breakdown/`](../skills/odonian-breakdown/SKILL.md) turns a design into an
executable board: formalize a `design` or `feature_spec` document, optionally create the repo for
greenfield work, decompose into bite-size, model-pinned tasks, and register the project, document,
and tasks through the API.

It proposes and takes positions but stops for the human's decision at every design choice, task
boundary, and spec. It never chooses what to build; the human brings the work. Its decomposition
rules encode the system's conventions: no code in a spec, every coding task sized for the smallest
model tier (decompose finer rather than escalate up front), and same-file tasks dependency-ordered
so parallel workers do not collide on merge. Helper scripts (`scripts/odonian.sh`,
`scripts/create-repo.sh`) travel with the directory and must stay executable.

Triggers on requests like "let's break this down for the board" or "decompose this feature into
Odonian tasks".

## `odonian-board`: see the board

[`skills/odonian-board/`](../skills/odonian-board/SKILL.md) renders the board by state (the TUI's
columns, as text), shows one task, project, or document in detail, and watches work move. It is
read-only and built for headless or sandboxed sessions where the TUI is unavailable.

Triggers on "show me the board", "what's in flight?", "anything blocked?", "show task <id>".

## `odonian-ops`: diagnose and operate

[`skills/odonian-ops/`](../skills/odonian-ops/SKILL.md) works out why a task is wedged and, only
on the human's explicit instruction, remediates it through the CLI: `transition`, `promote`,
`archive`. It reads freely, mutates only when told, verifies the precondition before any
irreversible action, and never touches tokens.

Triggers on "why is <task> stuck?", "unstick <task>", "sweep for zombie merge tasks", "promote
<task>", "move <task> to <state>".

## `review`: the human merge gate

[`skills/review/`](../skills/review/SKILL.md) is a conversational wrapper for the merge gate:
show the queue of tasks awaiting a decision (`odonian pending`), show one task's diff
(`odonian diff`), and, only on the human's explicit instruction, record the verdict
(`odonian approve` / `odonian reject --note …`). It never forms its own opinion. The human supplies
the judgment; the CLI does the mechanics, which in `local_commit` mode includes the branch freeze
and worktree cleanup.

Triggers on "what's waiting for review?", "show me the diff for <task>", "approve <task>",
"reject <task> because …".

## Installing

Pick one of three ways. There is no separate enable step; a skill triggers automatically on
requests matching its `description`.

**1. Plugin marketplace (recommended).** This repo is a Claude Code plugin marketplace
([`.claude-plugin/marketplace.json`](../.claude-plugin/marketplace.json)); the `odonian` plugin
bundles all four skills, installed globally and namespaced by plugin:

```text
/plugin marketplace add boldfield/odonian
/plugin install odonian@odonian
/reload-plugins
```

They are then `/odonian:odonian-breakdown`, `/odonian:odonian-board`, `/odonian:odonian-ops`, and
`/odonian:review` in every session. A marketplace can also be added from a local path or any git
URL (`/plugin marketplace add ./` from a checkout) and managed from the interactive `/plugin` menu.

**2. Symlink or copy into a skills directory.** A skill is auto-discovered under
`~/.claude/skills/` (every project) or `<repo>/.claude/skills/` (one project). This gives bare
names (`/odonian-breakdown`, `/review`) rather than the plugin namespace:

```bash
mkdir -p ~/.claude/skills
for s in odonian-breakdown odonian-board odonian-ops review; do
  ln -s "$PWD/skills/$s" ~/.claude/skills/$s     # or cp -r for a frozen copy
done
```

**3. Project-local in another repo.** Copy or symlink a skill into a target repo's
`.claude/skills/` so it is available only there. Useful for `odonian-breakdown` in whatever repo
you are scaffolding boards from.
