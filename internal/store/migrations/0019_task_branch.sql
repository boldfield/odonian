-- Optional local_commit target branch for an implement task. Empty means the task gets its
-- own MR branch, wi/<slug of its title> (the previous behaviour). A non-empty value names a
-- branch shared across tasks: every task with branch 'foo' starts its worktree from, and
-- freezes onto, wi/foo, so dependent tasks build on each other's approved work without
-- anything landing on main.
ALTER TABLE task ADD COLUMN branch TEXT NOT NULL DEFAULT '';
