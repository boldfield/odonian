# Execution agent runbook

> **Status checked 2026-11-03 · v0.24.0 / `b3da896`: Historical bootstrap runbook.**
> The file-backed board loop below predates the running Odonian service; do not use it to operate the current queue. Use [AGENT-API.md](AGENT-API.md) for agent workflow and [the documentation index](docs/README.md) for current guides.

You are an execution agent draining the bootstrap board in `board/`. Work **one task at a
time**, end to end, following this loop exactly. Do not skip steps.

## The loop

1. **Pick a task.** List `board/ready/`. Choose exactly one file. If `board/ready/` is
   empty, stop — there is nothing to do.

2. **Branch first.** Create a branch off `main` named for the task, *before touching
   anything*. Every commit for this task lives on this branch so `main` stays pristine and
   only ever reflects merged state:
   ```bash
   git checkout main && git pull --ff-only
   git checkout -b odonian/<ID>-<slug>
   ```

3. **Claim it on the branch.** Move the file to `in_progress` and update its `state:`
   frontmatter to match. This is your branch's first commit:
   ```bash
   git mv board/ready/<file>.md board/in_progress/<file>.md
   # edit the file: set `state: in_progress`
   git add -A && git commit -m "claim <ID>: <title>"
   ```
   Never commit a claim (or anything else) onto `main`. Never work a task still in `ready/`.

4. **Read the spec.** Open the task file. Read its `## Spec` and `## Acceptance criteria`.
   Read the document it references (`document:` frontmatter, e.g. `DESIGN.md`) for context.
   The spec is the contract — build exactly what it says, no more.

5. **Do the work.** Implement it. Write tests where the acceptance criteria call for them.
   Keep the change scoped to this task; do not start other tasks' work.

6. **Verify before claiming done.** Actually run it:
   ```bash
   make build && go vet ./... && go test ./...
   ```
   Every acceptance-criteria bullet must be satisfied and demonstrable. If something fails,
   fix it — do not proceed with failing checks.

7. **Open a PR.**
   ```bash
   git push -u origin odonian/<ID>-<slug>
   gh pr create --fill --base main
   ```
   The PR title should be `<ID>: <title>`. The body should list which acceptance criteria
   are met and how you verified them.

8. **Move to review.** On your branch, so the board move travels with the PR:
   - `git mv board/in_progress/<file>.md board/review/<file>.md`
   - Edit the file: set `state: review`, and under a new `## Result` section add the PR URL,
     the branch name, and the head commit SHA.
   - Commit: `git commit -am "submit <ID>: in review (<PR URL>)"` and push.

9. **Stop.** Do not merge. Do not move the task to `done/` — a human reviews and makes that
   call. Report: the task ID, the PR URL, and a one-line summary of what you did. Then return
   to step 1 only if explicitly told to continue.

## Rules

- **One task at a time.** Finish or fail it before touching another.
- **Respect dependencies.** A task's `depends_on` tasks must all be in `board/done/` before
  you work it. If `board/ready/` only contains tasks with unmet deps, stop and report.
- **Keep `state:` frontmatter in sync** with the directory the file lives in, always.
- **Stay in scope.** Build what the spec says. If the spec is ambiguous or you discover it's
  wrong, do **not** guess or expand scope — see Blocked.

## Blocked or failed

- **Blocked** (missing info, ambiguous spec, broken dependency): `git mv` the file to
  `board/review/`, set `state: blocked`, add a `## Blocked` section explaining precisely what
  you need, commit, and stop. Do not push partial guesses.
- **Failed** (you attempted it and it cannot be done as specified): same, but `state: failed`
  with a `## Failed` section describing what you tried and why it failed.

In both cases a human decides next steps. Your job is to surface the problem clearly, not to
work around it silently.
