# Research pacing and model-agnostic reviewer evaluation

## Agreement and constraints

Approved direction, 2026-10-02: preserve the existing research quality process while automatically pacing research consumption; build a model-agnostic reviewer comparison system, with Meta Muse as its first candidate using the owner's existing Power subscription. A later adapter, such as Pi connected to the owner's Spark deployment, must fit the same process without changing evaluation storage, sampling or reporting. Opus remains the research writer and Astra/Fable remain the production reviewers. Do not change claim limits, source-verification requirements, acceptance rules, escalation, continuation creation, or the human merge gate. Do not route work to cheaper models based on assumed difficulty. Any future replacement requires a separately reviewed decision supported by matched evaluations.

This feature has two milestones: automatic research pacing, then bounded Muse evaluation. All implementation tasks begin on Haiku with escalation enabled and independent Opus/GPT-5.5 reviews. For this implementation experiment, the owner explicitly authorizes agent_merge=true: both automated reviewers must approve, then the merger may merge without a human review or merge checkpoint. This exception applies only to building this feature; existing production research publication and human merge gates remain unchanged. Live rollout is separate from implementing the feature. This document does not set unmeasured production rates, start a paid experiment, install credentials, or authorize pay-as-you-go fallback.

## Why

Research currently launches an LLM before the LLM claims a task. Limiting only the claim endpoint would still spend usage on agents discovering that they are throttled. Small research tasks, rework, and continuation children all create new writer/reviewer sessions. A shared automatic limiter should spread that work without adding manual task promotion or weakening evidence standards.

The current server has atomic ClaimTask in internal/store/store.go, SQLite migrations in internal/store/migrations, routes in internal/api/api.go, and CLI commands in cmd/odonian. Fleet dispatch and both discovery loops are in harness/agent.sh. Research prompts are in harness/prompts/pull_request/research. The continuation validator in internal/manifest/manifest.go requires exactly two reviewers; leave that contract intact. Existing research scorecards in internal/store/scorecard.go are observational and are not ground truth for evaluator accuracy.

## Milestone 1: automatic research pacing

### Policy

Use explicitly configured account/quota pools, shared across all projects and fleet workers on the same Odonian deployment. Map model IDs to pools; aliases consuming the same account share a pool. A model using another provider/account can have a different allowance. Research only is subject to this policy; build, design, and non-LLM merges retain existing behavior. Other activity on the same external subscription is not visible and requires operator headroom.

Each enabled pool has a sustained start rate, bounded burst, and concurrent-dispatch limit. Separate configured capacity for completion work prevents new first-pass writers from exhausting the allowance needed by reviews, rework, and adjudication. Completion work remains bounded; it cannot bypass the total pool ceiling. Reserved completion capacity is not borrowed by fresh work in the initial release. Validation rejects impossible reservations, invalid numbers, duplicate mappings, and unmapped research models when enforcement is enabled. Unlimited behavior exists only when the feature is explicitly disabled. Support disabled, observe, and enforce modes; observe reports hypothetical denials without claiming that it limits spending.

Policy evaluation uses server time with an injectable clock. Time moving backward must not refill allowance. No periodic bulk catch-up burst beyond configured capacity. Defaults do not invent a production rate or claim to translate task starts into exact subscription percentages.

### Admission and attempt lifecycle

Before launching any research LLM, the harness requests admission for one specific eligible task, model, and agent. The server atomically checks ordinary claim eligibility, checks the pool, debits a start, creates a durable attempt/permit, and claims the task. A competing request cannot claim the same task or overspend the pool. Admission binds the task, model, agent, and attempt. Stable request IDs make an ambiguous transport retry return the original admission rather than spending again. A genuinely new attempt, including a retry or expired-lease reclaim, consumes a new start. Never automatically refund starts on uncertain launch outcomes; conservative accounting is preferable to overspending.

No admission means no model process. A deferred task remains ready with no lease, rejection, round increment, failure, or artificial blocked state. The response distinguishes rate, concurrency, and reserved-capacity deferral. Return not-before when time alone can permit progress; when awaiting an active dispatch, return a bounded retry interval without inventing a finish time. Both single-project and multi-project discovery must skip deferred candidates and continue to other eligible work rather than stop on the first denied project/model.

The harness supplies the preclaimed task ID to research prompts. The model works that task and never searches for or claims a different one. Existing manual/legacy research claim paths must obey the same enabled policy; enforcement cannot be bypassed through direct claims. Upgraded fleet paths must wait before model launch. Non-research paths remain compatible.

Dispatch concurrency is associated with the attempt, not released merely because the agent submits its verdict. The harness renews the permit during the process lifetime and renews the task lease while still owned/in progress. Submission may end task ownership before the process exits; this must not invalidate permit renewal. On process exit the harness idempotently finalizes the permit and records the exit class. A lost permit/ownership must not let an old worker submit into a replacement attempt. Normal rate exhaustion never kills active research. Crash recovery uses expiring permits, with fencing of stale operations. A lease is a coordination bound, not proof that a remote process has physically stopped after a network partition; document this limit and the harness's ownership-loss behavior.

Persist allowance state and active attempts across server restart. Restart must not refill buckets or duplicate launches. Policy changes must not mint a burst larger than the new capacity or interrupt active work. Rollout must drain old research launchers before enforce mode so old LLM-first launchers do not repeatedly spend usage on rejected claims.

### Visibility and validation

Expose configured mode/pools and effective capacity without credentials, active attempts, deferred reasons, and next retry information in API/CLI. Record task/project/model/account-pool, attempt ID, admission and completion timestamps, exit class, and optional actual usage when the runtime supplies it. Missing usage remains unknown. Starts, durations, and exits are proxies, not billing measurements. Keep telemetry bounded and avoid creating a task event on every poll.

Required tests use fake clocks, concurrent requests, restarted stores, and fake model executables. Verify no model invocation on deferral; idempotent retries; continued polling of another project/model; research rework and review counting; completion reservations; non-research compatibility; no late permit release corrupting a newer attempt; task submission not prematurely releasing dispatch concurrency. Do not spend subscription usage for implementation tests.

## Milestone 2: model-agnostic comparison reviews

### Candidate runtime adapter contract

The evaluation core has no Muse, Pi, or provider-specific branching. A registered candidate identifies an adapter, immutable effective model identity, model revision when available, runtime/version, reasoning and generation settings, prompt version, tool/source-access configuration, and account or compute pool. A display name alone is not a reproducible candidate identity. Unknown provider revisions remain explicitly unknown. Changing any effective configuration creates a new candidate version rather than rewriting prior results.

An adapter consumes a versioned evaluation request containing a run ID, frozen snapshot/workspace, blinded prompt, declared source-access requirements and output destination. It returns versioned normalized lifecycle/result records: completed review, structured findings, incomplete/unsupported/failed status, error class, effective model/runtime/configuration, timing and usage where available. Capability preflight validates requirements before admission/launch without making a paid inference call. The host controls admission, workspace staging, secret isolation, leases, result validation and persistence; the candidate model cannot obtain board credentials or directly submit a verdict. Do not accept a fake clean review when an adapter lacks required source retrieval, PDF access or structured-result support.

Adapters may wrap a CLI agent or a tool-capable API runtime. Register a trusted executable plus an argument list and explicit credential references; do not evaluate arbitrary shell strings from campaign data. Keep provider-specific auth, invocation, output parsing and cancellation in the adapter. Runtime configuration records tool availability and any observers/subagents, because the experiment measures the entire reviewer setup, not a bare model in isolation. Record declared versus observed configuration separately if the runtime cannot expose everything.

The first adapter is Muse Code. A future Pi-to-Spark adapter should need only the contract implementation and configuration, not a new campaign, storage or report design. This release does not invent Pi flags, Spark endpoint protocols, model IDs or credentials, and does not implement or launch that future backend. Local/compute-only candidates can use explicitly configured concurrency capacity without a subscription-rate bucket, but still obey finite campaign attempt caps; missing quota configuration must never silently imply unlimited execution.

A finite campaign can contain multiple candidate versions. Each receives an independent run of the same frozen samples and cannot see other candidates' outputs. Use unique campaign/sample/candidate/attempt identities. Exhausting or failing one candidate's quota does not reroute its sample to another model or mutate other results. Preserve per-candidate completion denominators and report unmatched samples explicitly. No model is implicitly the oracle; production reviewers are comparison baselines with evidence-backed human dispositions.

### First adapter: Muse runtime and billing

Use the Muse Code CLI, explicitly pinned to muse-spark-1.3 and a recorded CLI version, through the owner's Power subscription credential. Meta documents muse exec for unattended runs and JSONL output. An arbitrary META_API_KEY can override stored subscription authentication and may be billed pay-as-you-go. Do not copy or print credentials, generate a new billing key, silently fall back to an API client, or infer subscription entitlement from successful authentication alone. Provide a preflight that distinguishes configured subscription routing, missing/ambiguous auth, unsupported CLI capability, and runtime errors without a paid probe. Validate actual subscription operation during owner-controlled rollout.

Reference documentation checked on 2026-10-02:
- https://dev.meta.ai/docs/models
- https://dev.meta.ai/docs/muse-code
- https://dev.meta.ai/docs/muse-code/extending
- https://dev.meta.ai/docs/muse-code/auth
- https://dev.meta.ai/docs/muse-code/subscriptions

Use a separate Meta evaluation pool for the Muse candidate and finite per-candidate and campaign attempt caps. A retry consumes another attempt. Exhaustion pauses automatic selection; it does not alter production tasks. The experiment must not become an unbounded third reviewer on every continuation.

### Matched, blind evaluation

Store evaluation jobs separately from production review tasks. Each job names project, original task, original review round, exact submitted commit, source/manifest digests where present, immutable candidate version (adapter/model/runtime/effective configuration), prompt version, attempt metadata, structured findings, completion status, and usage if reported. Jobs have durable claim/lease/result handling, but cannot vote, reject, advance task states, create follow-ups, adjudicate production disputes, or create continuations. Do not extend the continuation manifest's two-reviewer field.

The first pilot uses Muse and compares first-round submissions only; the campaign and sample schema supports additional candidate adapters. Select a reproducible bounded cohort from the research projects with both clean and materially rejected submissions, keeping selection rationale and denominators. Selection can use sealed historical outcomes; the reviewer input cannot contain those outcomes. Use the original submitted SHA, never the corrected final artifact or current main. If the original artifact/source context cannot be reconstructed reliably, mark the sample unavailable and exclude it visibly, not as a success or miss. New live samples can use the same snapshot mechanism without blocking production review.

Give each candidate the same research evidence standard, task acceptance criteria, and source access. Stage only the frozen artifact and necessary source context in a clean workspace without repository history containing later fixes. Do not expose other reviewer comments, findings, verdicts, task event history, or production board/forge credentials to the model. Avoid links to PR discussions in the prompt. Public source retrieval remains available. The host runner, outside the model's credential environment, obtains snapshots and records results. Content-blinding limitations, including potentially public reviewer discussions discoverable on the web, must be documented; do not claim perfect blinding.

The model returns structured findings to a local result artifact. A deterministic host process validates and stores the result; the model cannot call production submit. Incomplete output, malformed output, unavailable sources, interruption, and failed execution are recorded separately from a clean completed review. Exit zero alone is not evidence that review succeeded. Any default runtime observers or nested agents that run are part of the measured configuration and usage, not ignored overhead.

### Reporting and decisions

Reports group by immutable candidate version and model/runtime/tool configuration, never by a hardcoded Muse column. Display provider-reported usage units as supplied; do not turn missing token counts, local compute time, or incomparable subscription units into fabricated dollar savings.

Compare findings only on the same submitted artifact/round. Human/evidence-backed dispositions record valid, invalid, or unresolved findings, claim association, material severity, evidence, and who decided. Agreement with Astra/Fable and whether a worker changed text are not automatic truth labels. Deduplicate the same issue across reviewers without erasing disagreements. Report confirmed material findings, confirmed unique findings, false positives, misses against the adjudicated finding set, unresolved findings, coverage/completion, wall time, and available usage with sample sizes and incomplete-run exclusions. Explicitly label recall as relative to the known adjudicated set, not all possible errors.

Surface potentially material Muse-only findings for human attention through the operator report, without automatic production state changes or unsolicited external messages. Existing Astra/Fable approvals and human merge remain authoritative. A report never automatically replaces a reviewer or routes future work to a cheaper model.

## Rollout and open operating values

Milestone 1 operating guide: [`docs/runbooks/research-pacing-rollout.md`](../runbooks/research-pacing-rollout.md) covers the illustrative configuration shapes, calibration from observed attempts, shared-account headroom, the safe rollout and rollback, the lease/partition limits and the smoke test (`bash harness/research_pacing_smoke_test.sh`). It sets no production values.

The implementation chain may advance automatically after its dependencies merge and automated checks pass, including between the two milestones. Land and verify milestone 1 before enabling a production pacing policy. Select numeric rates, bursts, concurrent limits, and completion reservations from observed dispatch durations and external quota trends with headroom; no safe production numbers have been established yet. Deploy updated launchers first and drain incompatible research launchers before enforcement. Deferred tasks continue automatically when eligible. Disabling admission stops new evaluation selection or restores the explicitly configured legacy research behavior; it must not mutate running tasks.

After milestone 1 passes automated review and merges, land the generic evaluation milestone and first Muse adapter and verify subscription auth/configuration without exposing secrets. Before a paid pilot starts, choose a finite cohort and attempt cap; start with one evaluation process at a time. Rate values and the paid pilot start remain deployment decisions rather than hidden implementation defaults. Deployment manifests live in a separate manifests project as described by deploy/fleet/README.md; this feature's Odonian tasks produce code, tests, configuration examples, and a rollout runbook, not an unrequested production deployment.
