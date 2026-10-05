# Writing a comparison-reviewer adapter (for example Pi connected to Spark)

A new reviewer backend is a new **adapter plus configuration**. It needs no new campaign, storage,
admission or report code: the report built from `GET /evaluation/campaigns/{id}/report` already has one
row per registered candidate version and no per-model columns. This guide describes what a trusted
adapter has to do, using "Pi connected to Spark" as the running example.

**What this guide does not know.** The repository does not contain Pi's command line, any flags, the
Spark endpoint, its protocol, model IDs or credentials, and none of those are assumed anywhere in the
code or here. Wherever the text below says `<...>`, the adapter author fills it in from Pi's and
Spark's own documentation. Do not guess a flag: if a capability cannot be confirmed without a paid
call, treat the preflight as failed (`capability_missing`) rather than assuming it works.

Existing references: `docs/runbooks/muse-code-adapter.md` (a complete adapter and preflight),
`internal/evaluation/adapter.go` (the protocol types), `internal/evaluation/fake.go` (a no-inference
fixture adapter used by the tests), and `internal/evaluation/registry.go` (registration rules).

## 1. The contract an adapter implements

The adapter is a trusted executable. The host (`evaluation.Pipeline`) stages a request, starts the
executable with an argument list (never a shell string), and reads the result.

1. **Request.** `{request_path}` is a JSON file (`evaluation.CandidateRequest`, `version` 1):
   `run_id`, `snapshot_path` (absolute; the frozen workspace to review, also the working directory
   to use), `blinded_prompt`, `tool_access` (what the review needs: source retrieval, PDF access,
   named tools) and `result_path` (absolute). Registered arguments may use the placeholders
   `{request_path}`, `{result_path}` and `{run_id}` and no others.
2. **Result.** Write one JSON object (`evaluation.CandidateResponse`, `version` 1) to `result_path`:
   - `run_id` equal to the request's;
   - `status`: `completed`, `incomplete`, `unsupported`, `interrupted` or `failed`; only `completed`
     may set `review_completed: true` and carry `findings`;
   - for every other status an `error_class` legal for that status (`capability_missing` for
     `unsupported`; `output_truncated`, `source_unavailable`, `budget_exhausted` for `incomplete`;
     `interrupted`, `timeout` for `interrupted`; `runtime_error`, `output_malformed`,
     `output_missing`, `auth_missing`, `launch_error`, `source_unavailable` for `failed`) and a
     short `error_message`;
   - `findings`: `{id, severity: material|minor|note, summary, claim?, ...}` with unique non-empty ids;
   - `identity`: what the runtime **observed** about itself, with every value it cannot report left
     `unknown` (an empty string is invalid);
   - `timing` (`started_at`, `finished_at`) and `usage`.
3. **Exit status.** Exit 0 whenever a valid result was written, whatever its status. A non-zero exit
   with no result is classified by the host (`runtime_error`); a non-zero exit with a `completed`
   result is rejected as `runtime_error`. Never report `completed` for a review that did not finish.
4. **Never fake a clean review.** If the runtime cannot fetch sources, open PDFs or emit structured
   output that the request requires, return `unsupported` / `capability_missing` with
   `missing_capabilities`. An empty `findings` list means "the review completed and found nothing",
   nothing else. The report counts failed, unsupported and incomplete runs as not clean.
5. **Secrets.** Credentials arrive only through the registration's explicit credential references and
   environment allowlist, never through the request, the result, argv or logs. The host redacts, but
   the adapter must not print them. The candidate model must not be given board credentials and cannot
   submit a verdict; the only thing that leaves a run is the result file.
6. **Cancellation.** On SIGINT/SIGTERM or the host timeout, stop the backend, including child
   processes, and write `interrupted` if you can. The registered timeout must be longer than any
   timeout the adapter applies itself, so the adapter's own cleanup runs first.

## 2. What to record for the report

The report groups by the candidate's immutable identity, so the identity must describe the reviewer
setup that actually runs, not just a model name. For Pi connected to Spark that is whatever you can
state truthfully, for example:

| Identity field | Value |
|---|---|
| `adapter_name`, `adapter_version` | your adapter and its version |
| `model_id`, `model_revision` | the model the adapter selects on Spark, from Spark's documentation; `unknown` if the revision is not reported |
| `runtime_name`, `runtime_version` | Pi and the version it reports (`<Pi's version command>`) |
| `reasoning_settings`, `generation_settings` | the settings the adapter passes, as a known set; unknown if it passes none and the defaults cannot be read |
| `tools` | the tools or skills Pi has enabled for the review (known set), including whether it can retrieve sources |
| `observers` | any subagent, critic or monitor that also runs (known set, possibly empty); the experiment measures the whole reviewer setup |
| `prompt_version` | the prompt version the adapter uses |
| `account_pool` | the pool name (see below) |

Declared configuration goes in the registered candidate; observed configuration goes in each result's
`identity`. If they differ, the report shows `runtime_drift: true` and the effective digests, and the
runs stay attributed to the declared candidate; do not edit a candidate to hide drift. A changed
configuration is a new candidate version.

**Usage.** Put whatever the backend reports in `usage` as `unit name -> non-negative number`, using
the backend's own unit names (for a local or Spark-hosted model that may be tokens, GPU seconds,
steps or something else). Omit usage the backend does not report; the report counts those attempts as
`usage_unknown_attempts`. Do not convert units, sum different units, or estimate a price.

## 3. Register and configure it

1. **Runtime registration** (`evaluation.Registry.Register`, in the host program that runs the
   campaign): an absolute executable path and an argument list, with the placeholders above; the
   environment variables to pass through (an explicit allowlist) and credential references; the
   capabilities the adapter really provides (`structured_output` always; `source_retrieval`,
   `pdf_access` and `tool:<name>` only if verified); a timeout; and the frozen candidate
   configuration from `evaluation.NewCandidateConfig(identity)`. The registry keys runtimes by that
   identity's digest, so a candidate that does not match a registration is "not registered" and is
   never run through another one. Registration refuses shell interpreters and shell text.
2. **Pool.** Create a dedicated evaluation pool with `odonian evaluation-pool-set`. A local or
   Spark-hosted candidate with no subscription rate limit uses `--concurrency-only` and a small
   `--concurrent-limit`; an account-backed one uses `--start-rate`, `--burst` and a limit. Omitting
   quota configuration never means unlimited.
3. **Candidate.** `odonian evaluation-create-candidate --campaign <c> --id <v> --cap <n>
   --identity-file identity.json` with the identity above. Caps are finite.
4. **Preflight.** Give the adapter a mode that checks the runtime and its configuration **without a
   model call** and reports distinct outcomes (runtime missing, runtime error, a required capability
   missing, auth missing or ambiguous, ready). The Muse adapter's `--preflight` is the model to copy.
   The preflight may run only commands that the backend's own documentation says are free and
   side-effect free; if you cannot find such a command for Pi, the preflight can only check that the
   executable exists and your configuration is complete, and it must say so in its output instead of
   claiming `ready`.
5. **Rollout.** Follow `docs/runbooks/reviewer-evaluation-rollout.md`: finite cohort and caps, one
   evaluation process at a time, pause to stop selection.

## 4. Test it without a paid or remote call

- Run the adapter against a **fake backend** (a script standing in for Pi) for each outcome:
  completed with and without findings, `incomplete`, `unsupported`, a crash, a hang past the timeout,
  a malformed result, and a result for another `run_id`. `internal/evaluation/fake.go` and
  `internal/evaluation/pipeline_test.go` show the pattern through a real registered executable.
- Check that no secret appears in argv, the result or stderr.
- Check that a missing capability gives `unsupported`, not an empty clean review.
- Build a report from the adapter's recorded runs and confirm the new candidate appears as its own
  row with no code change to the report (`TestReportHasNoFixedColumnsPerCandidate` and
  `TestReportTwoFakeCandidatesAgainstBaselines` in `internal/evaluation/report_test.go` do this for
  two fake candidates).

No test in this repository calls a paid or remote model, and an adapter's tests should not either.
