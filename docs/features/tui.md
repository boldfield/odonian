# Odonian TUI

The TUI is a keyboard-driven terminal UI for inspecting and operating the board without leaving the terminal. It is **just another client of the HTTP API** — the same API workers and reviewers drive. It holds no state and talks to no database directly.

## Layout

The TUI shows three panes: **projects**, **work items**, and **preview**. The layout adapts to terminal width:

### Wide (120 columns and up): all three panes
```
 PROJECTS        │ syndic · 59 open                          │ D-31 Repair loop
─────────────────┼───────────────────────────────────────────┼──────────────────────────────
 ▸ syndic   ●3   │ ◷ D-31 Repair loop            checks  18m │ checks · round 1 · sonnet
   ohand    ⚠1   │ ● D-35 Replace prompts   sonnet @2bllr 12m│ PR #212 · in checks 18m
   llm-gw   ●2   │ ● D-34 Repair fixture    haiku  @s9jtd  4m│
   reala    ⚠1   │ ◆ D-30 Rubric loader          review r2   │ Reviews (round 1)
   dumpdem  ✔1   │     opus ✔ approve · gpt-5.6-sol … 6m     │   none yet: waiting for CI
   foia     ✔1   │ ○ D-36 Author prompt          ready       │
   notblue  ●1   │ ⛓ D-37 Rubric export          waits D-35  │ History
   manifests !1  │ ⛓ D-39 Index page             waits D-36  │   12:41 submitted, PR opened
   +19 quiet     │ ○ D-38 Export                 ready       │   12:41 entered checks
                 │ ↓ 50 more                                 │   12:12 claimed by @c4wqd
─────────────────┴───────────────────────────────────────────┴──────────────────────────────
 tab pane · ↑↓ move · enter detail · a approve · u unblock · t hold · v preview · ? help
```

- **Projects pane** (left): projects in the board with badges showing the highest-priority status (⚠ needs you, ! failed, ✔ awaiting merge, ● in flight). One project is selected; focus shifts to the projects pane when needed.
- **Work items pane** (center): one implement task per row; review and merge tasks nest as indented lines under their target. Focus here with `tab`/`shift+tab`.
- **Preview pane** (right): events and details for the selected item. Only shown when an item is selected; `v` toggles it.

### Medium (80–119 columns): project rail + items
The project pane collapses to a 10-column rail on the left; detail opens full-screen over the items on `enter`.

### Narrow (under 80 columns): items only
Only the work items pane is shown; projects and detail open full-screen.

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

| State | Key | Action |
|---|---|---|
| backlog | `p` | promote → ready |
| ready, in progress, review, approved, blocked | `t` | hold/release (prefix on status) |
| any item | `i` | set priority |
| any item | `w` | move to front (highest priority) |
| any item | `e` | reset priority |
| any item | `z` | archive |
| review | `a` | approve (with optional note; confirm required) |
| review | `x` | reject (reason required) |
| approved | `m` | merge & complete |
| approved | `b` | bounce (back to ready) |
| blocked | `u` | unblock |
| blocked | `f` | fail |
| any item | `c` | open research scorecards |

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
- Below 80 columns the layout degrades to items-only (projects and detail open full-screen)
- Resizes are live; panes adapt to new width on the fly
