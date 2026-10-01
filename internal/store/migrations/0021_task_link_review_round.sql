-- The review round a link was submitted in. Each implement submission starts a new review round
-- and may add links (a local_commit rework adds a new `commit` link per round, and old ones stay),
-- so without this a task's links cannot say which commit the current round reviews. Set on links
-- an implement task submits (a re-submitted identical link moves to the new round); NULL for links
-- recorded before this column existed and for links on other kinds of task. Readers use the links
-- tagged with the task's current review_round, and fall back to the untagged ones only for a task
-- none of whose links are tagged (one only ever submitted before the upgrade).
ALTER TABLE task_link ADD COLUMN review_round INTEGER;
