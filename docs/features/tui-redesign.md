# TUI redesign: three panes, cross-project, grouped by work item

Status: **draft for review, not built.** Supersedes the layout in [`tui.md`](./tui.md); the
actions and their server calls described there are kept.

## Problem

The TUI (`cmd/odonian-tui`) is a Bubble Tea program that renders everything as hand-built
strings. It shows one project at a time as ten state tabs, each a flat list.

- **One project at a time.** There are 28 projects. "What needs me?" and "what is stuck?" are
  fleet-wide questions and today mean switching projects one by one.
- **State tabs hide the work item.** Implement, review and merge tasks are mixed in one list.
  A row shows id, model, priority and title, but not the kind, so two reviews and a merge do
  not read as belonging to one implement task.
- **"Ready" hides the real status.** A claimable task and one waiting on a dependency look
  the same.
- **Nothing is laid out.** The tab row and the help bar are each a single unwrapped line and
  overflow on a normal-width terminal. A per-state colour map exists and is never applied.
- **No view of what is in flight.** Nothing shows which agent holds what, or for how long.
  Workers parked for hours on OhAnd CI were invisible here.

The operator uses the TUI for three jobs in roughly equal measure: triage (what needs me),
watching (where is work, what is moving) and acting (promote, approve, unblock, merge). The
layout has to serve all three without a mode switch.

## Decision

Three panes: **projects**, **work items**, **preview**. An "All" entry at the top of the
project pane gives the cross-project view, laid out as four bands (needs you, in flight,
queued, finished in the last hour). Reviews and merges nest under their implement task. The existing full-screen detail
view and every existing action are kept.

Rejected alternatives:

- **Inbox only** (one cross-project list in three bands). Best for triage, but it flattens
  one project's flow into a list. Kept as the content of "All".
- **Pipeline** (five stage columns, cards flowing left to right). Best single view of flow,
  but needs about 100 columns, truncates titles hard and pushes triage behind a strip. May
  return later as an alternate rendering of the work-item pane; out of scope here.
- **Strict drill-down columns** (each pane appears only after a selection in the previous
  one). The screen jumps as you move, and project badges disappear exactly when you are deep
  in one project.

## Layout

Mockups are drawn at 100 columns. Data is illustrative.

### Wide (120 columns and up): all three panes

```
 PROJECTS        │ syndic · 59 open                          │ D-31 Repair loop
─────────────────┼───────────────────────────────────────────┼──────────────────────────────
   All      ⚠5   │ ◷ D-31 Repair loop            checks  18m │ checks · round 1 · sonnet
 ▸ syndic   ●3   │ ● D-35 Replace prompts   sonnet @2bllr 12m│ PR #212 · in checks 18m
   ohand    ⚠1   │ ● D-34 Repair fixture    haiku  @s9jtd  4m│
   llm-gw   ●2   │ ◆ D-30 Rubric loader          review r2   │ Reviews (round 1)
   reala    ⚠1   │     opus ✔ approve · gpt-5.6-sol … 6m     │   none yet: waiting for CI
   dumpdem  ✔1   │ ○ D-36 Author prompt          ready       │
   foia     ✔1   │ ⛓ D-37 Rubric export          waits D-35  │ History
   notblue  ●1   │ ⛓ D-39 Index page             waits D-36  │   12:41 submitted, PR opened
   manifests !1  │ ○ D-38 Export                 ready       │   12:41 entered checks
   +19 quiet     │ ↓ 50 more                                 │   12:12 claimed by @c4wqd
─────────────────┴───────────────────────────────────────────┴──────────────────────────────
 tab pane · ↑↓ move · enter detail · a approve · u unblock · / filter · v preview · ? help
```

### Wide, "All" selected: the bands

```
 PROJECTS        │ All projects                               │ Fix simulator signing
─────────────────┼────────────────────────────────────────────┼─────────────────────────────
 ▸ All      ⚠5   │ NEEDS YOU (5)                              │ ohand · blocked 2h
   syndic   ●3   │ ▸ ✖ ohand     Fix simulator signing    2h  │ round 2 · sonnet · PR #88
   ohand    ⚠1   │   ✖ reala     Import listings feed     1d  │
   llm-gw   ●2   │   ✔ dumpdem   Add donate footer       40m  │ Why
   reala    ⚠1   │   ✔ foia      Agency index page        3h  │   checks timed out after 6h
   dumpdem  ✔1   │   ! manifests Pin triage image         5h  │
   foia     ✔1   │ IN FLIGHT (6)                              │ History
   notblue  ●1   │   ● syndic    D-35 Replace prompts    12m  │   09:02 blocked: checks
   manifests !1  │   ● llm-gw    Drain on preempt        31m  │         timeout
   +19 quiet     │   ◷ syndic    D-31 Repair loop        18m  │   03:01 entered checks
                 │   ◷ ohand     Watch complication    1h04m  │   03:01 submitted, PR opened
                 │   ◆ llm-gw    Quota probe       r2     6m  │
                 │   ◆ notblue   Signup form       r1     2m  │
                 │ QUEUED (177)                               │
                 │   syndic 56 · foia 26 · ohand 21 · +10     │
                 │ FINISHED, LAST HOUR (3)                    │
                 │   ✓ syndic    D-29 Fixture loader     14m  │
                 │   ✓ llm-gw    Retry budget            37m  │
                 │   ✓ foia      Agency search           52m  │
─────────────────┴────────────────────────────────────────────┴─────────────────────────────
 tab pane · ↑↓ move · enter detail · u unblock · m merge · / filter · v preview · ? help
```

### Medium (80 to 119 columns): project rail and items; preview opens over the list

```
 PROJ     │ syndic · 59 open
──────────┼────────────────────────────────────────────────────────────
   All ⚠5 │ ◷ D-31 Repair loop                          checks    18m
 ▸ syn ●3 │ ● D-35 Replace prompts            sonnet    @2bllr    12m
   oha ⚠1 │ ● D-34 Repair fixture             haiku     @s9jtd     4m
   llm ●2 │ ◆ D-30 Rubric loader                        review r2
   rea ⚠1 │     opus ✔ approve · gpt-5.6-sol … 6m
   dum ✔1 │ ○ D-36 Author prompt                        ready
   foi ✔1 │ ⛓ D-37 Rubric export                        waits D-35
   +21    │ ↓ 52 more
──────────┴────────────────────────────────────────────────────────────
 tab pane · ↑↓ move · enter preview · a approve · / filter · ? help
```

### Narrow (under 80 columns): items only

```
 syndic ▾ · 59 open · ⚠5 elsewhere
──────────────────────────────────────────────
 ◷ D-31 Repair loop              checks 18m
 ● D-35 Replace prompts          sonnet 12m
 ● D-34 Repair fixture           haiku   4m
 ◆ D-30 Rubric loader            review r2
 ○ D-36 Author prompt            ready
 ⛓ D-37 Rubric export            waits D-35
 ↓ 53 more
──────────────────────────────────────────────
 p projects · enter detail · ? help
```

Projects and detail are full-screen views at this width. The header keeps one cross-project
number so attention items elsewhere are not lost.

## Pane behaviour

| Pane | Renders | Notes |
|---|---|---|
| Projects | Always at 80+ columns; a key hides it | Full names at 120+, a short rail below. Projects with nothing open collapse into one "+N quiet" line that expands on select. |
| Work items | Always | The main content. An empty project shows an empty-state line, never a missing pane. |
| Preview | Only when an item is selected and width is 120+ | On by default at that width; `v` toggles it. With nothing selected the item list takes the full width. Below 120 it opens over the list on Enter. |

Width thresholds are checked on every resize, so the layout changes live.

## What a row is

**Work item.** One row per implement task. Review tasks for the current round and the merge
task attach to their target (`target_task_id`) and render as one indented line under it. A
review or merge task never appears as a top-level row.

**Glyphs and colour.** One glyph per status, coloured by status. Colour is redundant with the
glyph, so the board stays readable without colour.

| Glyph | Meaning |
|---|---|
| `○` | ready and claimable |
| `⛓` | ready or backlog, waiting on a dependency |
| `·` | backlog, not promoted |
| `●` | in progress (shows model, holder, time held) |
| `◷` | in checks (shows time in checks) |
| `◆` | in review (shows round and each reviewer's verdict) |
| `✔` | approved, awaiting merge |
| `✖` | blocked |
| `!` | failed |
| `⏸` | held (prefix on any of the above) |

Priority is shown only when it differs from the default.

**Needs you.** A task is in the "needs you" band, and counts toward a project's `⚠`/`✔`
badge, when a human is the next actor: `blocked`, `failed`, or `approved` with
`agent_merge=false`. An approved task with `agent_merge=true` is in flight, not waiting on
the operator.

**Finished, last hour.** Implement tasks that reached `done` in the last hour, newest first,
so watching shows completions and not just rows disappearing. Shown in "All" only. It is the
last band and the first to be cut when the pane is short.

**Project badge.** The highest-priority non-zero count: `⚠` needs you (blocked or failed),
`!` failed only, `✔` awaiting your merge, `●` in flight. No badge means queued work only.

## Keys

Every existing action keeps its key and its server call: promote, approve, reject, bounce,
unblock, fail, hold/release, the three priority actions, archive, merge, scorecards, refresh,
quit. Changes:

- `tab` / `shift+tab` move focus between panes; arrows move within the focused pane.
- `/` filters the work-item pane by text.
- `v` toggles the preview; a second key hides the project pane.
- `?` opens the full key list. The bottom bar shows only the keys valid for the selected
  item's status, and truncates with `…` instead of overflowing.
- The project-switcher overlay goes away; the project pane replaces it.

## Libraries

Bubble Tea is already the framework. The redesign adopts the component libraries that are
already dependencies but almost unused, and moves to the v2 line while the view layer is
being rewritten anyway.

| Library | Today | Target |
|---|---|---|
| Bubble Tea | `github.com/charmbracelet/bubbletea` v1.3.10 | `charm.land/bubbletea/v2` |
| Bubbles | v1.0.0, only `textinput` and `viewport` | `charm.land/bubbles/v2`: `list` or `table`, `viewport`, `textinput`, `help`, `key`, `spinner` |
| Lip Gloss | v1.1.0, only the `Color` type | `charm.land/lipgloss/v2`: styles, borders, joins for pane layout |
| Glamour | not used | `charm.land/glamour/v2`: render task specs and results as Markdown in detail |

All four v2 modules have stable releases (checked 2026-11-23). The v2 migration cost against
this codebase has not been measured; the first task is a spike that ports the program shell
and reports what breaks.

Not adopted now: Huh (forms) and Bubblezone (mouse regions).

## Data the layout needs

The server lists tasks per project only (`GET /projects/{id}/tasks`, filterable by state,
kind, assignee, model and `claimable`, with `fields=summary`). The TUI polls one request per
state for the open project. That shapes what is cheap and what is not.

| Need | Available today | Gap |
|---|---|---|
| Group reviews and merges under their implement task | Yes: `kind`, `target_task_id`, `review_round`, `verdict`, `agent_merge` are in the summary | Client only: `tuiclient.Task` does not decode `target_task_id`, `review_round`, `verdict`, `agent_merge` or `review_models` yet |
| Time in checks, holder, lease | Yes: `checks_started_at`, `assignee`, `lease_expires_at` | None |
| Cross-project "All" view and project badges | Only by polling every project, one request per state each | **S1** |
| Which dependency a task is waiting on | `claimable=true` filter distinguishes claimable from not; the summary has no dependency ids | **S2** |
| Per-check CI status in the preview | No: the checks reconciler decides and moves the task; it does not store the rollup | **S3** |
| Worker pool size (busy and idle) | Busy is derivable from distinct assignees; idle is not, the server has no worker registry | Not shown (decided 2026-11-23) |

Server changes:

- **S1. Cross-project task listing.** `GET /tasks?fields=summary` with the same filters as the
  per-project list, plus `project_id` in each row. One request per poll serves "All" and
  every project badge. Without it the TUI would issue a request per state per project on
  every tick. It also takes `updated_since`, so the "finished, last hour" band is one small
  query and not a listing of every `done` task.
- **S2. Unmet dependencies in the summary.** Add `blocked_by`: the ids of dependencies that
  are not yet `done`. Lets a row say "waits D-35" and removes the second `claimable` query.
- **S3. Last check rollup on the task.** The checks reconciler records the most recent rollup
  (check name, status, conclusion) so the preview can list checks. Until then the preview
  shows time in checks and the pull request link only. Lowest priority.

## Code structure

`cmd/odonian-tui/board.go` is 2,599 lines holding state, update logic and rendering for every
mode. The rewrite separates:

- **View models** (pure functions, no Bubble Tea, in a new package `internal/tuiview`): group
  tasks into work items, compute the bands, compute project badges, choose the layout for a
  width. These carry the logic and are tested with plain table tests.
- **Panes** (one Bubble Tea model each): projects, work items, preview, detail.
- **Shell**: focus, resize, polling, and routing actions to the existing client calls.

`board_test.go` (6,217 lines) is written against the tab layout. Tests for actions and server
calls carry over; tests that assert rendered tab output are replaced by view-model tests.

## Phases

1. **Spike and shell.** Port to the v2 libraries; three-pane shell with Lip Gloss layout and
   the width thresholds; single-project work-item pane with grouping; preview pane; all
   existing actions working. Uses only today's API.
2. **Cross-project.** S1, then the "All" bands (including finished, last hour) and project
   badges.
3. **Dependencies.** S2, then "waits on" rows.
4. **Polish.** Glamour in detail, text filter, S3 and per-check status.

Each phase leaves a usable TUI. Phase 1 is at parity with today's TUI for one project.

## Out of scope

- The pipeline (stage-column) view.
- Mouse support.
- A worker or fleet view. That needs a worker registry on the server and is its own design.
- Changing any action's semantics or server call.

## Open questions

1. `list` or `table` for the work-item pane. `table` gives aligned columns; `list` gives
   multi-line items (the nested review line) and built-in filtering. Leaning `list` with a
   custom delegate; the spike decides.
2. Poll interval for the cross-project request once S1 exists, and whether terminal-state
   tasks are fetched at all outside the detail view.
