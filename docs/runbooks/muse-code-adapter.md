# Muse Code adapter runbook

Audience: the operator who installs and registers the Muse Code comparison-reviewer adapter. Design background is in
[`docs/features/research-pacing-and-reviewer-evaluation.md`](../features/research-pacing-and-reviewer-evaluation.md);
the host-side contract it implements is `internal/evaluation/adapter.go`.

The adapter (`cmd/muse-adapter`, code in `internal/evaluation/muse.go`) runs Meta's Muse Code CLI, pinned to
`muse-spark-1.3`, through the owner's Power **subscription**. It is an isolated candidate runtime: it is not in
`review_models`, not in dispatch, and changes no production reviewer. It never installs software, never writes or
copies a credential, and never falls back to another provider or to pay-as-you-go billing.

## What the official documentation does and does not say

Sources: <https://dev.meta.ai/docs/muse-code>, `/extending`, `/auth`, `/subscriptions` (checked 2026-10-03).

Documented, and relied on:

- `muse --version` verifies an install.
- `muse exec` is the headless mode. Documented options: `--json` (JSONL events on stdout), `--prompt-file <path>`,
  `--disable-approval` (skips approval prompts, keeps the sandbox), `--max-model-steps`, `--session-id`,
  `--allow-workspace-switch`, `--yolo` (never used here).
- Exit codes: `0` turn completed, `1` failure or cancellation (including the step limit), `2` usage error,
  `130`/`143` SIGINT/SIGTERM.
- Credential precedence: `META_API_KEY` if set, then a stored key (`muse auth set`), and only then a stored browser
  session. An API key always outranks the browser session. The subscription covers only the CLI signed in with the
  Meta Model API account; usage through any API key is billed pay-as-you-go. `muse logout` removes stored credentials.

**Not documented**, so never assumed:

- A model-selection flag (the default model is `muse-spark-1.2`). The adapter uses `--model` and confirms at preflight
  that the installed `muse exec --help` lists it. If it does not, preflight reports `capability_missing` and nothing runs.
- The JSONL event schema. The adapter requires every stdout line to be one JSON object, but takes results from its own
  marker protocol (below), not from event field names.
- Any command that reports which credential is active, and where credentials are stored.

## Install and version

The adapter does not install Muse. As the operator, on the machine that runs the evaluation (documented by Meta):

```
curl -fsSL https://dev.meta.ai/install.sh | sh
muse --version
```

The version string printed by `muse --version` (stdout, falling back to stderr) is recorded verbatim as
`runtime_version` in every candidate identity. A different version is a different candidate version: register it as a
new candidate rather than rewriting prior results.

## Billing route: how it is enforced

A stored API key silently outranks a subscription session and cannot be detected from outside the CLI, so the adapter
fails closed instead of guessing:

1. `META_API_KEY` present in the adapter's environment (even empty) is a hard failure, `auth_override_present`. It is
   never forwarded. The key's value is never read into any output.
2. The operator must attest the route with `--auth-route browser-session`. Without it preflight reports
   `auth_route_unconfirmed` and muse is never started. Do this only after:
   1. `muse logout` (removes any stored API key, which would otherwise outrank the session),
   2. signing in with the Power account's browser flow from an interactive `muse`,
   3. confirming no stored key was recreated (`muse auth set` was not run).
3. `HOME` must be set so muse can find its stored browser session (`auth_missing` otherwise).
4. The muse child runs with an allowlisted environment only (`PATH`, `HOME`, `USER`, `LOGNAME`, `LANG`, `LC_*`, `TERM`,
   `TZ`, `TMPDIR`, `XDG_*`, TLS certificate variables, proxy variables). Every other variable, including any API
   key, `ODONIAN_*`, and cloud credentials, is dropped.

The attestation is a statement by the owner, not proof of entitlement. Browser sign-in alone does not prove the Power
subscription is what is billed. Before a paid pilot, the owner validates in an interactive session and in the Meta
Accounts Center (`/upgrade` inside `muse`) that evaluation usage lands on the subscription. This adapter performs no
paid probe to establish that.

## Preflight (no model call)

```
go build -o muse-adapter ./cmd/muse-adapter
./muse-adapter --preflight --auth-route browser-session [--muse-bin /abs/path/to/muse]
```

Preflight runs only `muse --version` and `muse exec --help`, each bounded to 20 seconds, and prints a JSON report to
stdout (exit `0` when ready, `1` otherwise). The report never contains credentials. Outcomes:

| `outcome` | Meaning | Contract response in run mode |
|---|---|---|
| `ready` | runtime found, version recorded, every required flag listed, route attested | runs |
| `runtime_missing` | `muse` not found or not executable | `failed` / `launch_error` |
| `runtime_error` | `--version` or `exec --help` failed, timed out or printed nothing | `failed` / `runtime_error` |
| `capability_missing` | `exec --help` lacks one of `--json --prompt-file --model --disable-approval --max-model-steps` | `unsupported` / `capability_missing`, `missing_capabilities: ["cli_flag:--model", ...]` |
| `auth_override_present` | `META_API_KEY` is set | `failed` / `auth_missing` |
| `auth_route_unconfirmed` | `--auth-route browser-session` not given | `failed` / `auth_missing` |
| `auth_missing` | `HOME` unset | `failed` / `auth_missing` |

The response `error_message` always starts with the specific outcome name.

## Registering the runtime

The adapter is a trusted registration (`Runtime` in `internal/evaluation/registry.go`): an absolute executable plus argv.
Example argv (the host substitutes `{request_path}`):

```
--request {request_path} --auth-route browser-session --muse-bin /home/eval/.local/bin/muse \
  --account-pool meta-eval --max-model-steps 100 --timeout 25m
```

Pass `HOME` (and any `XDG_*` your muse install needs) through `PassThroughEnv`; register no credential references,
because the subscription session lives in muse's own store. Declare only `structured_output`: the adapter does not
claim source retrieval, PDF access or named tools, and returns `unsupported` / `capability_missing` for a request that
needs them. Set the host runtime timeout longer than `--timeout`: the adapter's own timeout kills muse and its process
group cleanly, whereas a host-side kill of the adapter cannot reach muse's descendants.

Flags: `--muse-bin` (default `muse` on `PATH`), `--auth-route`, `--account-pool` (recorded as `account_pool`; `unknown`
when omitted), `--max-model-steps` (default 100), `--timeout` (default 30m).

## How a run works

1. Validate the request; reject capabilities the adapter does not declare.
2. Preflight as above.
3. Stage the blinded prompt plus an output contract in a private temp file and run, with the snapshot as the working
   directory:
   `muse exec --json --model muse-spark-1.3 --disable-approval --max-model-steps N --prompt-file <file>`.
4. Collect stdout as JSONL. Every line must be one JSON object; a bad line, or more than 16 MiB, kills the run
   (`output_malformed`) and the rest is discarded so muse cannot block on a full pipe.
5. Completion is **separate from exit zero**. The prompt asks Muse to end with
   `ODONIAN_RESULT_BEGIN:<nonce>` / a JSON object / `ODONIAN_RESULT_END:<nonce>`, the nonce being a fresh random value
   per run. The block is found by scanning string values of the events (each key path as its own stream, so streamed
   deltas reassemble), so it does not depend on the undocumented event schema. A run is `completed` only if muse exited
   `0` AND exactly one distinct block parses strictly (`odonian_result_version` 1, matching `nonce`, `complete: true`,
   a `findings` array, known severities, unique ids, non-empty summaries, no unknown fields).
   Exit `0` with no events or no block is `failed` / `output_missing`; an invalid or contradictory block is
   `failed` / `output_malformed`.
6. Exit codes: `1` and unknown codes `failed` / `runtime_error`; `2` `failed` / `runtime_error` naming a likely
   incompatible flag; `130`, `143` or death by signal `interrupted` / `interrupted`.
7. Interruption: SIGINT or SIGTERM to the adapter, or the host cancelling it, kills muse's whole process group and
   writes `interrupted` / `interrupted`; the adapter's `--timeout` writes `interrupted` / `timeout`. Timing is measured
   either way.

The adapter exits `0` whenever it wrote a contract-valid response (the response carries the outcome) and `2` when it
could not operate (unreadable request, unwritable result path).

## What the candidate identity records

| Field | Value |
|---|---|
| `adapter_name` / `adapter_version` | `muse-code` / `1` |
| `model_id` | `muse-spark-1.3` (the model the adapter passes; the CLI does not report an effective model) |
| `model_revision` | `unknown` |
| `runtime_name` / `runtime_version` | `muse-code-cli` / the `muse --version` output |
| `generation_settings` | known: `max_model_steps` |
| `reasoning_settings`, `tools`, `observers` | `unknown` (not reported by the CLI) |
| `prompt_version` | `odonian-muse-review/v1` |
| `account_pool` | `--account-pool`, else `unknown` |

Usage is left unset: Muse does not report usage in any documented form, and nothing is converted into dollars.
Responses written before a preflight succeeds carry unknown effective values.

## Tests

`go test ./internal/evaluation` exercises the adapter against fake `muse` shell scripts (missing runtime, incompatible
flags, auth ambiguity, malformed and missing output, exit zero without completion, non-zero exits, timeout, SIGTERM
with a surviving grandchild). No test installs Muse, reads a credential or makes a paid call.
