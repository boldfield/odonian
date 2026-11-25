# Feature: Odonian TUI

> **Status: the TUI runs the three-pane layout described under "Layout & navigation" and "Keybindings".**
> The [TUI implementation](../../cmd/odonian-tui/) includes capabilities beyond the original v1 scope, including priority controls and research scorecards. The goals, non-goals and task breakdown below are the historical v1 scope, not a current capability list. The design behind the layout is [`tui-redesign.md`](./tui-redesign.md). See the [main guide](../../README.md) and [documentation index](../README.md).

Status: feature spec, 2026-06-05
Kind: feature_spec (for project Odonian)

## What this is

A minimal terminal UI for Odonian — a keyboard-driven board you live next to in the
terminal. It fixes the one ergonomic hole in the system: today the human side of the loop
is `curl | jq`. The TUI lets a human **see the board** (what's in backlog, being worked,
waiting for review) and **act on it** (promote backlog → ready; approve/reject reviews)
without leaving the terminal.

It is **just another client of the existing HTTP API** — the same API the Haiku agents
drive. It holds no state of its own and talks to no database directly. This re-validates
the API-only design: humans and agents drive the same single source of truth.

## Goals (v1)

- **Pick a project** (`GET /projects`); auto-select if there's one or a configured default.
- **See the board** as states — `backlog`, `ready`, `in_progress`, `checks`, `review`, `done` —
  from a single `GET /projects/{id}/tasks` call, bucketed by state client-side.
- **Watch agents work**: `in_progress` cards show `assignee` and a live lease countdown.
- **Watch the checks gate**: a `checks` column sits between `in_progress` and `review`; its cards
  and the task detail show how long the task has been in `checks` (from `checks_started_at`).
- **Promote** a `backlog` task to `ready` (`p`).
- **Work the human gate** from the `review` column: approve (`a`) and reject (`x`). The API
  already supports this end to end (`POST /tasks/{id}/review`, `POST /tasks/{id}/transition`,
  both shipped) — no backend work, which is why it's v1.
- **Task detail** (`enter`): spec, deps, links, result, timestamps.
- **Refresh**: poll on a short interval + a manual refresh key, without disturbing your
  selection.

## Non-goals (v1)

- No web UI. Terminal-only, keyboard-driven.
- No live push/streaming — poll for v1 (an SSE endpoint can come later).
- No task creation/editing in the TUI.
- No event timeline (needs a `GET /tasks/{id}/events` endpoint that does not exist yet).
- No multi-project aggregate view — one project at a time.

## Stack & architecture

- **Go + Bubble Tea** (Charm): `bubbletea` for the loop, `lipgloss` for styling, `bubbles`
  for list/viewport/textinput widgets. New binary `cmd/odonian-tui` (builds to
  `bin/odonian-tui`); add a `tui` Makefile target.
- An **API client** behind an interface (`internal/tuiclient`), wrapping the HTTP API,
  configured from the resolved config (below). It defines its own small response structs
  (external client; does **not** import `internal/store`). Methods: `ListProjects`,
  `ListTasks(projectID)`, `GetTask(id)`, `ListDocuments(projectID)`, `PromoteTask(id)`,
  `ReviewTask(id, actor, verdict, note)`, `TransitionTask(id, to, note)`. The interface is
  the seam for testing (a mock client backs the model tests).
- Bubble Tea is the only state holder; all data is fetched, never cached to disk.

## Configuration

Resolution order (highest wins): **flags > environment > config file > defaults**.

- Config file (TOML) at `$XDG_CONFIG_HOME/odonian/config.toml` (default
  `~/.config/odonian/config.toml`). Keys: `url`, `token`, `actor`, `default_project`,
  `poll_interval` (default `2s`).
- Env: `ODONIAN_URL`, `ODONIAN_TOKEN`, `ODONIAN_ACTOR` (same vars the agents use).
- `actor` (the reviewer identity recorded on review events) defaults to the OS `$USER` when
  unset.
- **Token handling:** prefer `ODONIAN_TOKEN` (env). The file *may* hold `token`, but the
  TUI warns once if a token is read from a world-readable file. No token is ever written to
  disk by the TUI.
- A missing/invalid `url`/`token` produces a clear startup error, not a panic.

## Layout & navigation

**Three panes:** projects on the left, work items in the middle, a preview of the selected work
item on the right. The board shows one project at a time (the one selected in the projects
pane). Each work item is one row: an implement task, with its review and merge tasks nested
under it. A glyph shows its status: `○` ready to claim, `⛓` waiting on a dependency, `·` in
backlog, `●` in progress, `◷` checks, `◆` review, `✔` approved, `✖` blocked, `!` failed, `✓`
done, `⊗` abandoned; a held item carries a hold prefix.

Wide terminal (120 columns or more), a backlog item selected:

```
 PROJECTS        │ syndic                                    │ D-36 Author prompt
─────────────────┼───────────────────────────────────────────┼──────────────────────────────
   ohand         │ ◷ D-31 Repair loop            checks  18m │ backlog
 ▸ syndic        │ ● D-35 Replace prompts   sonnet @2bllr 12m│
   llm-gw        │ ◆ D-30 Rubric loader          review r2   │ History
   reala         │ · D-36 Author prompt          backlog     │   events of the selected task
 ──────────────────────────────────────────────────────────────────────────────────────────
 p promote · enter detail · tab pane · ↑/↓ move · t hold/release · i priority · w front · ...
```

The row and preview contents are illustrative; the help line is the real text for a backlog
item with the items pane focused (before truncation to the terminal width):

```
p promote · enter detail · tab pane · ↑/↓ move · t hold/release · i priority · w front · e reset · z archive · v preview · V projects · c scorecards · r refresh · ? help · q quit
```

The help bar is generated from the key bindings for the focused pane and the selected item's
state, so it lists only keys that work now. When it is wider than the terminal it is cut at the
end with `…`.

### Width

The layout follows the terminal width (`internal/tuiview/layout.go`):

| Terminal width | Panes shown |
|----------------|-------------|
| 120 columns or more | Projects (17 columns), items, and the preview when a work item is selected |
| 80 to 119 | A 10-column project rail with abbreviated names, and items. No preview pane |
| 40 to 79 | Items only |
| under 40 | "Terminal too narrow. Please resize." |

Press `enter` on a work item at any width to open its full-screen detail view. `V` hides and
shows the projects pane (or rail); `v` hides and shows the preview (wide terminals only).

### Focus

`tab` moves focus to the next visible pane and `shift+tab` to the previous one, wrapping around.
`↑/↓` (and `j/k`) act on the focused pane: they move the project selection, the work-item
selection, or scroll the preview. Item actions (`p`, `a`, `x`, ...) work when the items pane has
focus; the same keys do nothing in other panes, except that `z` in the projects pane archives
the selected project (with a confirmation). `enter` in the projects pane opens the selected
project in the items pane and moves focus there. Panes that are not visible at the current width
cannot take focus.

### Not yet built

The design in [`tui-redesign.md`](./tui-redesign.md) also describes an **"All"** entry at the top
of the projects pane: a cross-project view in four bands (Needs You, In Flight, Queued,
Finished in the Last Hour), and per-project status badges. None of these exists yet. The items
pane always shows one project, the projects pane lists projects only, and the project badges stay
empty. The bands and badges are not in the key tables below because they have no keys.

## Data & refresh

- One `ListTasks(projectID)` call per refresh; bucket by `state` client-side.
- Poll every `poll_interval` (default 2s); `r` forces an immediate refresh.
- **Selection is keyed by task ID and preserved across refreshes** — a poll must never move
  your cursor or reset scroll. If the selected task disappears (state changed/closed), select
  the nearest remaining task in the same column.
- After an action, refetch (no optimistic mutation) so the board reflects server truth.
- States: a brief loading indicator on first fetch; an "empty" message for an empty column /
  no tasks; an error banner (with the status/message) on failure that retries on the next
  tick.

## Actions

All actions go through the API; the board you see can be stale, so **every action handles a
409 gracefully** — surface the message, refetch, never crash.

- **Promote** (`p`, on a `backlog` task): `PromoteTask(id)` → refetch. 409 → "not in backlog
  (already moved?)".
- **Approve** (`a`, on a `review` task): optional note → **confirm** (because `done` is
  terminal in the state machine and cannot be undone) → `ReviewTask(id, actor, "approve",
  note)` then `TransitionTask(id, "done", note)`.
- **Reject** (`x`, on a `review` task): a small text input for the **required reason** →
  `ReviewTask(id, actor, "reject", reason)` then `TransitionTask(id, "ready", reason)`. The
  reason lands in the event log, and the task returns to the claimable pool for rework.
- `actor` on review events comes from config (`actor`, default `$USER`).

## Detail view

`enter` opens a **full-screen** detail (not a cramped split); `esc` (or `q`) returns to the board.
Shows: title, state, the full `spec` (scrollable viewport), `depends_on` resolved to task
titles, `links` (with `o` to open the `pr` link in `$BROWSER`/`open`/`xdg-open`), `result`,
`assignee`, lease countdown, and created/updated timestamps. On `review` tasks the same
`a`/`x` actions are available from the detail view, and on `approved` tasks `m` merges. `i`, `w`
and `e` set, front-load and reset the task's priority, `↑/↓` (or `j/k`) scroll the spec,
`pgup/pgdn` page it, and `?` shows help.

From detail you can also open the task's **source documents** in the browser:

- `s` — the task's **own source doc** (its `document_id`: the design/feature_spec it was
  decomposed from).
- `d` — the project's **base design** doc (`kind=design`).

Both resolve a `Document.ref` to a VCS browse URL the same way `o` resolves a PR link: the
TUI fetches `GET /projects/{id}/documents` (to map `document_id → ref` and find the design
doc), then builds `project.repo` + `/blob/` + (`Document.commit` if pinned, else the default
branch) + `/ref` and opens it. If a `ref` is already a URL, open it directly; if there's no
repo, no design doc, or the ref can't be resolved, show a brief message. Odonian stores only
the ref — the TUI builds the URL (GitHub-shaped; abstract for other forges later).

## Keybindings (full)

Checked against the `Shell` and `Detail` key maps in `cmd/odonian-tui/keybindings.go`.

### Board (all panes)

| Key | Action |
|-----|--------|
| `tab`, `shift+tab` | focus the next / previous visible pane |
| `↑/↓`, `j/k` | move the selection in the focused pane; scroll the preview when it has focus |
| `v` | show or hide the preview |
| `V` | show or hide the projects pane |
| `c` | scorecards |
| `r` | refresh now |
| `?` | toggle the full key list |
| `q`, `ctrl+c` | quit |

### Items pane

| Key | Action |
|-----|--------|
| `enter` | open the detail view (also from the preview pane) |
| `p` | promote (backlog → ready) |
| `a` | approve (review → done, confirmed) |
| `x` | reject (review → ready, reason required) |
| `m` | merge an approved task's PR |
| `b` | bounce an approved task |
| `u` | unblock (blocked) |
| `f` | fail (blocked) |
| `t` | hold or release the item (any state) |
| `i` | set priority |
| `w` | move to front |
| `e` | reset priority |
| `z` | archive (confirmed) |

`p`, `a`, `x`, `m`, `b`, `u` and `f` apply only to an item in the state named in the table; the
rest apply to any item.

### Projects pane

| Key | Action |
|-----|--------|
| `enter` | open the selected project |
| `z` | archive the selected project (confirmed) |

### Detail view

| Key | Action |
|-----|--------|
| `esc`, `q` | back to the board |
| `a`, `x` | approve / reject (review tasks) |
| `m` | merge (approved tasks) |
| `i`, `w`, `e` | set priority / move to front / reset priority |
| `↑/↓`, `j/k` | scroll the spec |
| `pgup/pgdn` | page the spec |
| `o` | open the task's PR link in the browser |
| `s` | open the task's source doc in the browser |
| `d` | open the project's base design doc in the browser |
| `?` | help |

## Visual design

- A status glyph per work item, a pane separator, and a persistent help bar built from the key
  bindings.
- Lease rendered as a live countdown (`2m11s`) and `EXPIRED` (red) when past — an expired
  lease on an `in_progress` task means it's reclaimable.
- Styling via `lipgloss`; degrade gracefully on narrow/low-color terminals.

## Testing

- The API client is an interface; model tests use a **mock client** with canned data.
- Bubble Tea model tests via `teatest` (charmbracelet): drive key messages, assert on the
  rendered output (golden files) — column switching, selection stability across a simulated
  refresh, the promote/approve/reject flows (including the confirm and the reject-reason
  input), and error rendering.
- The HTTP client gets a thin test against an `httptest` server for request shape + parsing.

## Task breakdown

Tightly coupled UI (one Bubble Tea model), so a **linear chain** — one claimable at a time
(`depends_on` enforces it):

1. **TUI-1 — scaffold, config, client, project picker.** `cmd/odonian-tui`; config
   resolution (file + env + flags, precedence, `$USER` default for actor, token warning);
   the `tuiclient` interface + HTTP impl (`ListProjects/ListTasks/GetTask/ListDocuments/
   PromoteTask/ReviewTask/TransitionTask`) + a mock; a Bubble Tea app that connects, shows a project
   picker (auto-select on one/default), and quits on `q`; `tui` Makefile target.
   **Acceptance:** builds; `make tui` produces `bin/odonian-tui`; with valid config it lists
   projects (or auto-selects); bad config errors cleanly; client httptest + a model test pass.
   *(no deps)*
2. **TUI-2 — board view, nav, polling.** Focused-column/tabs layout; bucket `ListTasks` by
   state; cards with title and (in_progress) assignee + lease countdown; arrow/hjkl nav;
   poll + manual refresh with **ID-keyed selection stability**; loading/empty/error states.
   **Acceptance:** renders all five columns with counts; nav works; a simulated refresh keeps
   the selection; teatest goldens for layout + selection-stability + error. *(deps: TUI-1)*
3. **TUI-3 — promote.** `p` on a `backlog` task → `PromoteTask` → refetch; 409 surfaced.
   **Acceptance:** promoting a backlog task moves it to ready and the board updates; a 409 is
   shown, not fatal; model test covers both. *(deps: TUI-2)*
4. **TUI-4 — review actions.** `a` approve (optional note + confirm) → review-approve +
   transition-done; `x` reject (required reason) → review-reject + transition-ready; `actor`
   from config. **Acceptance:** approve moves a review task to done (with confirm); reject
   requires a reason and returns it to ready; both record the verdict; model tests cover the
   confirm and the reason input. *(deps: TUI-3)*
5. **TUI-5 — detail view + doc/PR opening.** Full-screen detail (`enter`/`esc`); scrollable
   spec; deps resolved to titles; review actions available from detail. Open links in the
   browser: `o` (PR), `s` (the task's source doc), `d` (the project's base design) — each
   resolving a stored ref (`pr` link, or `Document.ref` via `ListDocuments` + `project.repo`
   + optional `commit` pin) to a VCS URL; brief message when a ref can't be resolved.
   **Acceptance:** detail shows spec/deps/links/result/meta; `o`/`s`/`d` build the correct
   URLs and invoke the opener (asserted via an injected opener fn in tests); unresolvable
   refs show a message, not a crash; `esc` returns. *(deps: TUI-4)*

## Feature-level acceptance

- `odonian-tui` builds, connects to a live Odonian via the resolved config, shows the board
  for a chosen project, promotes a backlog task, and approves/rejects a review task — all
  verified against the deployed instance.
- Uses only the public HTTP API (no DB access, no `internal/store` import).

## Future (post-v1)

- **Inline PR diffs in the review/detail pane** — the TUI fetches the diff from the VCS host
  using the stored `pr` link (`gh pr diff`, or the GitHub diff API), rendered in a scrollable,
  syntax-colored viewport. **Odonian never stores the diff** — the `TaskLink(pr|commit)` is
  the ref, the client resolves it (consistent with "stores refs, not content"). Zero backend
  change; VCS-coupled (abstract the diff source if going multi-forge).
- **Event timeline** per task — requires a new `GET /tasks/{id}/events` endpoint (the event
  spine isn't API-exposed yet). The one item that needs backend work first.
- **Live** updates via an SSE/stream endpoint instead of polling.
- **Multi-project** aggregate view.
- Create/edit tasks from the TUI (shell out to `$EDITOR` for the spec).
- Block/fail a task from the TUI (the `transition` endpoint already supports it).
