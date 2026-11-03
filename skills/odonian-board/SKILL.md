---
name: odonian-board
description: Use to SEE an Odonian board from an interactive session — render the task board by
  state (the TUI's columns, in text), show one task/project/document in detail, and watch work
  move. Read-only — it never changes board state (that's odonian-ops). Built for headless/sandbox
  use where no TUI is available. Triggers on "show me the board", "board status", "what's in
  flight?", "what's in review?", "anything blocked?", "show task <id>", "show the board for <project>".
---

# Odonian board (read-only visibility)

A conversational stand-in for the Odonian TUI. When you run the fleet headless — e.g. inside an
`sbx` sandbox where the terminal UI isn't available — this is how you get eyes on the board: it reads
the board over the API/CLI and renders it the way the TUI would, so you can see what's queued, in
flight, in review, and done without leaving the `claude` session.

## The one rule that overrides everything

**This skill only reads. It never mutates.** No promote, transition, archive, claim, approve, or
create — ever. If the human wants to *act* on what they see (unstick a task, approve, archive),
that's `odonian-ops`; hand off, don't reach for the CLI's mutating verbs here. Reading is always
safe; that's the whole point of keeping this skill clean.

## Phase 0 — Configuration

- `ODONIAN_URL` and `ODONIAN_TOKEN` must be in the environment; the `odonian` CLI reads them.
  If either is missing, ask the human.
- You need a project to focus on. Use `$ODONIAN_PROJECT` if set; otherwise run `odonian projects`
  and let the human pick, or render the multi-project overview (below).
- **Never print a token value** — not the bearer token, not a forge token.

## The board overview (all projects)

For "what's going on overall": `odonian projects --json` (add `--claimable` to see only boards with
claimable work). Render a compact table — project name, id (first 8), repo, and a quick count of
claimable work — so the human can pick where to look. The projects response does not include
counts; fetch `odonian tasks --project <id> --claimable --json` for each displayed count, or omit
the count if you have not fetched it.

## The task board (one project) — render it like the TUI

Pull the tasks once: `odonian tasks --project <id> --json`, then render by **state** in the TUI's
column order:

```
backlog → ready → in_progress → review → approved → done        (blocked / failed shown off to the side)
```

Build it in two steps — the first is not optional:

1. **FILTER FIRST — drop the bookkeeping before you render.** A `kind=review` or `kind=merge` task
   is server-spawned plumbing, not a deliverable. The **`done` column shows ONLY `implement` (and
   design/research-track) deliverables** — **every `review`- and `merge`-kind task is excluded from `done`**,
   exactly as the TUI does. (Their `review` and `in_progress` lanes still show, because that's where
   live review/merge work is; it's specifically the **`done` lane** that must never list a
   `review`/`merge` task.) Apply this drop *before* counting or listing anything in `done` — do not
   render and then apologize. If asked, report the hidden done-bucket review/merge counts separately.

2. **Render** each column as a short list, leading with a per-column count. For each task show, at
   minimum:
   - the **short id** (first 8 chars) and `[model]` badge and **title** — same as the TUI line
     (`<id8> [haiku] Implement the foo`);
   - for `in_progress` and `review`: the **assignee** and the **lease** — and **flag an expired
     lease** (`lease_expires_at` in the past) as a likely-stuck task worth a closer look;
   - for `review`: the `review_round` and how many reviewers have voted, if visible.

Keep it scannable: a human reading this in a chat window wants columns with counts and one line per
task, not a JSON dump. Summarize empty columns as "(none)".

## One task / document in detail

- A specific task: `odonian show <id>` — render its state, kind, model, assignee, lease (flag if
  expired), `review_round`/verdict, `track`, `held`, priority, **links** (`pr` / `branch` / `commit` / `ci` /
  `no_op`), and dependencies.
  If a link is a PR, offer to surface its diff (`odonian diff <id>`).
- A project: `odonian project <id>` shows project metadata. For its documents, read
  `GET /projects/{id}/documents` and show kind, title, and ref; the project command does not list
  documents.

## Watching work move

- Re-render on request ("refresh", "what changed?") by re-pulling and diffing against what you last
  showed — call out tasks that changed state, got claimed, or whose lease expired.
- In a sandbox run (`sbx.sh`), the fleet's per-kind logs live under `$ODONIAN_HOME/logs/`
  (`workers.log`, `reviewers.log`, `server.log`). For "what is the fleet actually doing right now?",
  tail those alongside the board — each line is prefixed with the agent's slot id.

## When the board shows something wrong

If a render surfaces a problem — a `merge` task stuck `in_progress`, an expired lease that wasn't
reclaimed, a task ping-ponging in review — **say so and hand off to `odonian-ops`**. Diagnosing and
fixing is that skill's job (and it carries the guardrails for mutating the board). This skill points;
it does not act.
