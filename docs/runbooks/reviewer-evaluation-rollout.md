# Reviewer evaluation: rollout, operation and reading the report

This is the operator procedure for a comparison campaign: checking the subscription route, choosing
finite caps, running **one** evaluation process at a time, stopping cleanly, and turning the results
into a report with evidence-backed labels. It sets no production values: every number below is a
choice you make, and nothing here changes reviewer assignments, research acceptance, source
requirements or live deployment settings. A candidate result can never vote on a task, and the report
never replaces a reviewer or routes work to another model.

The campaign interface is `odonian evaluation-*` (see `AGENT-API.md`, "Evaluation campaign CLI
commands", and `docs/api.md`, "Evaluation Campaigns"). The part that actually runs candidates is a
host program built on `internal/evalrun` (`evalrun.New(cfg).RunCampaign(ctx, campaignID)`) with a
registry of trusted runtimes. This repository ships the library, the Muse Code adapter
(`cmd/muse-adapter`) and no separate launcher for it: the deployment's own process (kept in the
separate manifests project) is the "evaluation process" below.

## 1. Check the billing route first (no model call)

For the Muse candidate, follow `docs/runbooks/muse-code-adapter.md`: a dedicated credential home, a
session manifest the owner recorded after confirming the Power subscription in the interactive
session, and then the preflight:

```
./muse-adapter --preflight --muse-home /home/eval/muse-home \
  --session-manifest /home/eval/muse-session.json --auth-route browser-session
```

Proceed only on `"outcome": "ready"`. Any other outcome (`auth_override_present`,
`auth_route_unconfirmed`, `auth_missing`, `auth_ambiguous`, `capability_missing`, `runtime_error`,
`runtime_missing`) means do not start: fix the cause, then re-run the preflight. `META_API_KEY` must be
absent from the evaluation process's environment. Preflight proves the route is configured, not that
the subscription is what is billed; that is the owner's interactive check, and the first runs must be
watched in the vendor's usage view.

Any other adapter (see `docs/runbooks/evaluation-adapter-guide.md`) needs its own no-cost preflight
that fails closed in the same way, and its own dedicated pool.

## 2. Choose a finite cohort and attempt caps

Decide the numbers before creating anything; there is no unlimited value.

1. **Cohort.** A small first-round cohort, drawn and frozen as described in "First-round cohorts and
   frozen workspaces" in `docs/features/research-pacing-and-reviewer-evaluation.md`. Samples whose
   commit cannot be read stay visible as unavailable instead of being replaced.
2. **Caps.** Create the campaign with `--cap` no larger than
   `samples x candidates x attempts you accept per pair`, and give each candidate its own `--cap`
   (`evaluation-create-candidate --cap`). A retry consumes another attempt. Start low: one attempt per
   pair plus a handful of retries for the whole campaign. Exhaustion refuses further claims
   (`CAPACITY_EXHAUSTED`), it does not reroute a sample to another candidate.
3. **Pools.** Give the paid candidate its own pool (`evaluation-pool-set`), with a rate and a
   concurrency limit well under the subscription's published limits; a local or compute-only
   candidate may use `--concurrency-only`. Never share a pool with production research reviewers.
4. Create the campaign, then each candidate version (`evaluation-create-candidate`) with the full
   identity JSON including tool and observer configuration. A changed configuration is a new
   candidate version, never an edit.

## 3. Start one evaluation process at a time

Run a single process for the campaign. `RunCampaign` makes one pass: it runs each sample on each
candidate one at a time and claims every attempt through the admission API (pool and cap checked on
the server). Each run may retry a retryable failure up to the runner's `MaxAttempts` (default 3), and
every retry spends cap. Calling `RunCampaign` again starts another full pass that runs every pair
again, so do not loop it: the caps are the only thing that bounds a second pass, and a pair that
already completed would be run a second time. Run one pass, read the report, and decide.

- Do not start a second process against the same campaign (or the same pool) until the first has
  exited. The store's fencing keeps results correct if you do, but two processes defeat the pacing you
  chose.
- Start it with the process's logs visible, and check progress with
  `odonian evaluation-get-campaign-status --id <campaign>`: `state`, `attempts_remaining` and each
  candidate's `attempts_remaining`, `active_attempts` and `pool`.
- After the first attempt finishes, run `odonian evaluation-get-report --id <campaign>` and confirm
  the row shows a recorded duration, the provider's usage unit (or an honest `usage_unknown_attempts`)
  and `runtime_drift: false` before letting it continue.

## 4. Stop selection cleanly

```
odonian evaluation-pause-campaign --id <campaign>
```

From then on every claim is refused with `PAUSED_WAITING` and `RunCampaign` returns without starting
another attempt. The API has no resume route, so a pause is final for this campaign: create a new
campaign to continue. An attempt already running finishes and is recorded; stopping the process mid-attempt
instead leaves a live attempt that expires and is reported as failed (`lease_expired`), which is a
legitimate non-clean outcome but wastes the attempt. So pause first, wait for
`active_attempts` to reach 0 in the status, then stop the process. Exhausted caps also stop selection
without any action. None of this touches production tasks.

## 5. Record dispositions and read the report

1. `odonian evaluation-get-report --id <campaign>`. Each group's members carry a `ref`.
2. Open the source at the sample's pinned commit and judge each finding. Record it:

   ```
   odonian evaluation-record-disposition --campaign <campaign> --ref <ref> \
     --label valid|invalid|unresolved --severity P1|P2|P3 \
     --claim "what the finding asserts" --evidence "what you checked" --actor <you>
   ```

   Record the same `--claim` wording (case and spacing are ignored) for findings from different
   reviewers that assert the same issue on the same sample; that is how they are matched and
   deduplicated. Never label from agreement ("both Astra and Fable said so") or because a writer
   changed the text. Use `unresolved` when you cannot decide. A wrong label is corrected by recording
   another one; the earlier row stays.
3. Read the report as follows.
   - **Coverage first.** Compare `coverage` and `outcomes`: only completed samples count, and failed,
     unavailable, incomplete, unfinished, not-run and excluded samples are never clean. Use
     `common` for a like-for-like comparison when coverage is unequal.
   - **Metrics** are relative to the known adjudicated set: `misses` and `recall_vs_known` count only
     what you labeled valid, so unlabeled work lowers nothing and the set is not exhaustive ground
     truth. Look at the sample denominators before any ratio.
   - **Usage** is in the provider's units (tokens, steps, seconds, ...) and is never converted. Do not
     derive a dollar comparison from it.
   - **`attention`** lists material candidate-only findings for you to read. Nothing was sent to
     anyone and no task changed; deciding what to do about one is yours, through the normal board
     workflow.
4. The report informs a human decision about future reviewer choice. It never replaces a reviewer.

## What not to do

- Do not run a paid pilot before steps 1 and 2 are done and written down.
- Do not raise a cap to finish a campaign; end it and report the coverage you have.
- Do not change a production reviewer assignment, the research acceptance rules or source
  requirements as part of this evaluation.
- Do not edit stored results; corrected artifacts and later rounds are intentionally excluded from the
  comparison.
