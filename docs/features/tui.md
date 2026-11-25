# Odonian TUI

The TUI is a keyboard-driven terminal UI for inspecting and operating the board without leaving the terminal. It is **just another client of the HTTP API** — the same API workers and reviewers drive. It holds no state and talks to no database directly.

## Layout

The TUI shows three panes: **projects**, **work items**, and **preview**. The layout adapts to terminal width:

### Wide (120 columns and up): all three panes
```
 PROJECTS        │ syndic · 59 open                          │ D-31 Repair loop
─────────────────┼───────────────────────────────────────────┼──────────────────────────────
 ▸ syndic        │ ◷ D-31 Repair loop            checks  18m │ checks · round 1 · sonnet
   ohand         │ ● D-35 Replace prompts   sonnet @2bllr 12m│ PR #212 · in checks 18m
   llm-gw        │ ● D-34 Repair fixture    haiku  @s9jtd  4m│
   reala         │ ◆ D-30 Rubric loader          review r2   │ Reviews (round 1)
   dumpdem       │     opus ✔ approve · gpt-5.6-sol … 6m     │   none yet: waiting for CI
   foia          │ ○ D-36 Author prompt          ready       │
   notblue       │ ⛓ D-37 Rubric export          waits D-35  │ History
   manifests     │ ⛓ D-39 Index page             waits D-36  │   12:41 submitted, PR opened
                 │ ○ D-38 Export                 ready       │   12:41 entered checks
                 │ ↓ 50 more                                 │   12:12 claimed by @c4wqd
─────────────────┴───────────────────────────────────────────┴──────────────────────────────
 p promote · enter detail · tab pane · ↑↓ move · t hold/release · i priority · w front · e reset · z archive · v preview · ? help · q quit
```

- **Projects pane** (left): projects in the board, one per row. One project is selected; focus shifts to the projects pane with `tab`. Selecting a project loads its tasks in the work items pane.
- **Work items pane** (center): one implement task per row; review and merge tasks nest as indented lines under their target. The selected item's state determines which actions are available (see Actions below).
- **Preview pane** (right): events and details for the selected item. Only shown when an item is selected; `v` toggles it.

**Note:** An "All" entry for cross-project view and status badges are not yet implemented. The TUI is currently project-scoped.

### Medium (80–119 columns): project rail + items
The project pane collapses to a 10-column rail on the left; detail opens full-screen over the items on `enter`.

### Narrow (under 80 columns): items only
Only the work items pane is shown. The detail view opens full-screen on `enter`. Projects pane is not available in narrow layout.

## Work items

Each row is one implement task. Status is shown with a glyph (◷ in checks, ● in progress, ◆ in review, ○ ready, ⛓ waiting, ✔ approved, ✖ blocked, ! failed) and optional notes (model, assignee, time). Review and merge tasks for the current round attach to their implement task and render indented.

## Pane focus & navigation

- `tab` / `shift+tab` cycle focus between panes.
- `↑/↓` (and `j/k`) move within the focused pane.
- `enter` opens the full-screen detail view (or selects a project if focus is in the projects pane).
- `q` or `esc` in the detail view closes it and returns to the board.
- `v` toggles the preview pane (wide layout only, only when an item is selected).
- `V` (shift+v) toggles the projects pane.

## Actions on work items

| Key | Action |
|---|---|
| `p` | promote (backlog → ready) |
| `a` | approve (review state only) |
| `x` | reject (review state only) |
| `m` | merge & complete (approved state only) |
| `b` | bounce (approved state only) |
| `u` | unblock (blocked state only) |
| `f` | fail (blocked state only) |
| `t` | hold/release |
| `i` | set priority |
| `w` | move to front |
| `e` | reset priority |
| `z` | archive |
| `c` | open research scorecards |

**On projects:** `z` to archive a project.

**Global:**
- `r` refreshes now
- `?` shows/hides the help overlay
- `q` or `ctrl+c` quits

## Detail view

`enter` opens a full-screen detail pane showing: title, state, spec (scrollable), dependencies (resolved to titles), links, result, priority, assignee, lease info, and timestamps. From detail:
- `a` / `x` on a review task to approve/reject with optional reason
- `m` on an approved task to merge & complete
- `o` to open the task's PR link in `$BROWSER`/`open`/`xdg-open`
- `s` to open the task's source document in the browser
- `d` to open the project's base design document in the browser
- `esc` or `q` to return to the board

## Configuration

Resolution order (highest wins): **flags > environment > config file > defaults**.

- Config file (TOML): `$XDG_CONFIG_HOME/odonian/config.toml` (default `~/.config/odonian/config.toml`)
  - Keys: `url`, `token`, `actor`, `default_project`, `poll_interval` (default `2s`)
- Environment: `ODONIAN_URL`, `ODONIAN_TOKEN`, `ODONIAN_ACTOR` (same as agents)
- `actor` defaults to `$USER` when unset
- Token from env is preferred; config file token triggers a one-time warning if world-readable
- Missing/invalid `url` or `token` produces a clear startup error

## Data & refresh

- Work items for a project are fetched once per manual refresh or poll interval (default 2s)
- Selection is keyed by task ID and preserved across refreshes — a poll never moves your cursor
- If the selected task disappears, the nearest remaining task in the list becomes selected
- After an action, the board refetches to reflect server truth
- Empty states, loading spinners, and error banners are shown as appropriate

## Actions resilience

All actions go through the API; the board may be stale. Every action handles a 409 (conflict) gracefully: surface the message, refetch, never crash. Example: promoting a task that was already promoted shows "not in backlog (already moved?)" and refreshes the board.

## Terminal size requirements

- **Minimum 40 columns** to render anything usable
- **80–119 columns**: project rail (10 columns) + items pane; detail opens full-screen over the items
- **Below 80 columns**: items pane only; detail opens full-screen over the items. Projects pane is not available below 80 columns.
- **120 columns and up**: full three-pane layout
- Resizes are live; panes adapt to new width on the fly
