-- Landing reservation held by `odonian approve` while it lands an approved local_commit task's
-- reviewed work on its MR branch. landing_round is the review round the approve prepared and
-- landing_commit the reviewed commit it lands; both are set together, atomically conditional on
-- the task still being approved in that round, so a task reworked and re-approved while the merge
-- gate ran cannot be finalised with its obsolete version. While set, the task can only move to
-- done (it cannot be rejected after its work reached the branch), and since it is not done yet,
-- its dependents stay blocked until the work has actually landed. It has no expiry: an
-- interrupted approve is resumed by re-running it. Cleared on any transition.
-- landing_attempt identifies the approve that currently owns the reservation (its branch-lock
-- token). A resumed approve re-reserving the same round and commit takes ownership, and a
-- reservation can only be cancelled by naming its current attempt, so a stalled approve that
-- comes back cannot clear the reservation of the approve that replaced it.
ALTER TABLE task ADD COLUMN landing_round INTEGER;
ALTER TABLE task ADD COLUMN landing_commit TEXT;
ALTER TABLE task ADD COLUMN landing_attempt TEXT;
