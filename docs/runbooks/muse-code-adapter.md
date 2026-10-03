# Muse Code adapter runbook

Audience: the operator who installs and registers the Muse Code comparison-reviewer adapter. Design background is in
[`docs/features/research-pacing-and-reviewer-evaluation.md`](../features/research-pacing-and-reviewer-evaluation.md);
the host-side contract it implements is `internal/evaluation/adapter.go`.

The adapter (`cmd/muse-adapter`, code in `internal/evaluation/muse.go`) runs Meta's Muse Code CLI, pinned to
`muse-spark-1.3`, through the owner's Power **subscription**. It is an isolated candidate runtime: it is not in
`review_models`, not in dispatch, and changes no production reviewer. It never installs software, never writes or
copies a credential, and never falls back to another provider or to pay-as-you-go billing.

## What the official documentation does and does not say

Sources: <https://dev.meta.ai/docs/muse-code>, `/extending`, `/auth`, `/subscriptions`, `/configuration` (checked
2026-10-03).

Documented, and relied on:

- `muse --version` verifies an install.
- `muse exec` is the headless mode. Documented options: `--json` (JSONL events on stdout), `--prompt-file <path>`,
  `--disable-approval` (skips approval prompts, keeps the sandbox), `--max-model-steps`, `--session-id`,
  `--allow-workspace-switch`, `--yolo` (never used here).
- `--model <id>` selects the model on both `muse` and `muse exec` (configuration page; the default model is
  `muse-spark-1.2`). Preflight still confirms that the installed `muse exec --help` lists it, and reports
  `capability_missing` (nothing runs) if it does not.
- Exit codes: `0` turn completed, `1` failure or cancellation (including the step limit), `2` usage error,
  `130`/`143` SIGINT/SIGTERM.
- Credential precedence: `META_API_KEY` if set, then a stored key (`muse auth set`), and only then a stored browser
  session. An API key always outranks the browser session. The subscription covers only the CLI signed in with the
  Meta Model API account; usage through any API key is billed pay-as-you-go. `muse logout` removes stored credentials.

**Not documented**, so never assumed:

- The JSONL event schema. The adapter requires every stdout line to be one JSON object, but takes results from its own
  marker protocol (below), not from event field names.
- Any command that reports which credential is active, and where credentials (browser session or stored key) are
  stored. Only `~/.config/muse/settings.json` is documented, and it holds settings, not credentials.

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

Muse documents no command that reports the active credential, and a stored API key silently outranks the browser
session. The adapter cannot ask the CLI, so it prevents the bypass instead and fails closed on anything it cannot rule
out:

1. `META_API_KEY` present in the adapter's environment (even empty) is a hard failure, `auth_override_present`. It is
   never forwarded. The key's value is never read into any output.
2. **Dedicated credential home (`--muse-home DIR`, required).** The muse child's `HOME` is always this directory; the
   adapter's own `HOME` and every `XDG_*` variable are dropped. An API key someone stored in the operator's normal home
   therefore cannot be seen by the evaluation runs. One-time setup, done by the owner (the adapter never signs in,
   installs or writes credentials):
   ```
   mkdir -m 700 /home/eval/muse-home
   HOME=/home/eval/muse-home muse logout          # clears any stored key or session in this home only
   HOME=/home/eval/muse-home muse                 # sign in with the Power account's browser flow, then exit
   ```
   Never run `muse auth set` with that `HOME`.
3. **Session manifest (`--session-manifest FILE`, required).** Because Muse does not document where it stores the
   browser session, no file pattern can prove that a home holds one. The adapter therefore runs only against state the
   owner verified. Right after the sign-in in step 2, still in that interactive session, the owner confirms the route:
   Muse reports when an environment key hides a browser session, and `/upgrade` opens Accounts Center, where the Power
   subscription must be shown. Then the owner records what the home looks like:
   ```
   ./muse-adapter --record-session --muse-home /home/eval/muse-home \
     --session-manifest /home/eval/muse-session.json --auth-route browser-session
   ```
   Recording applies every check below except the manifest itself, never starts muse, and writes (mode `0600`) only the
   names of the entries directly under `.config/muse`, never their contents. Preflight then requires that exact entry
   set. A home that was never recorded fails `auth_route_unconfirmed`, and any later change fails `auth_ambiguous`:
   an added stray file, a key stored with `muse auth set` in a new file, or a logout that removes the session. After
   signing in again, verify the route again and re-record. The manifest must be an absolute path outside the
   credential home, also after resolving symlinks in its parent directories, a regular file (not a symlink) with a
   single hard link, private, and owned by the adapter's user. Recording checks the written file again. If Muse itself adds or
   removes entries under `.config/muse` during normal runs, preflight fails closed after such a run until the owner
   verifies and re-records. That is expected, not a reason to loosen the check.
4. Preflight inspects the directory before muse is ever started, and reports:

   | `outcome` | Condition |
   |---|---|
   | `auth_route_unconfirmed` | `--muse-home` not given or not absolute; `--session-manifest` not given, not absolute or not recorded |
   | `auth_missing` | directory missing or empty, or no candidate session state: no non-empty file other than `settings.json` directly under `.config/muse` (a necessary condition only; the manifest is what ties the state to a verified sign-in) |
   | `auth_ambiguous` | symlink or not a directory; mode allows group/other access; owned by another user; same directory as the adapter's own `HOME`; **any symlink, FIFO, socket or device anywhere inside it** (the scan does not follow links, but muse would); unreadable or too large to inspect (over 5000 entries); any file name or file content (first 1 MiB, inspected in memory, never reported) matching `api key`-style names such as `api_key`, `apiKey`, `api-key`; manifest inside the home (directly or through a symlinked parent directory), a symlink, hard linked, readable by others, malformed, recorded for another home, or listing different entries than the home now holds |
5. `--auth-route browser-session` must also be passed. It is the owner's statement that step 2 was followed and
   `muse auth set` was never run against that home; without it, `auth_route_unconfirmed` and muse is never started.
6. The muse child runs with an allowlisted environment only (`PATH`, `USER`, `LOGNAME`, `LANG`, `LC_*`, `TERM`, `TZ`,
   `TMPDIR`, TLS certificate variables, proxy variables) plus `HOME=<muse-home>`. Every other variable, including any
   API key, `ODONIAN_*`, and cloud credentials, is dropped.

Residual limits, stated plainly: the key-detection scan is a heuristic over file names and contents, because the
storage format is undocumented, and a credential kept outside the home (for example an OS keychain, if muse used one)
cannot be seen. The manifest records entry names, not contents, so a key written into an existing file is caught only
by the content scan. The subscriptions page says the Power subscription is attached to "the Muse Code API key that is
automatically connected in the Muse Code CLI onboarding process"; if Muse stores that credential under an
`api key`-style name, a genuine session will fail `auth_ambiguous` (closed, not open), and the scan needs revisiting
against a real install. A signed-in session also does not prove the Power subscription is what is billed: the owner's
interactive verification in step 3 is what establishes that, before every record. This adapter performs no paid probe.

## Preflight (no model call)

```
go build -o muse-adapter ./cmd/muse-adapter
./muse-adapter --preflight --muse-home /home/eval/muse-home --session-manifest /home/eval/muse-session.json \
  --auth-route browser-session [--muse-bin /abs/path/to/muse]
```

Preflight runs only `muse --version` and `muse exec --help`, each bounded to 20 seconds, and prints a JSON report to
stdout (exit `0` when ready, `1` otherwise). The report never contains credentials. Outcomes:

| `outcome` | Meaning | Contract response in run mode |
|---|---|---|
| `ready` | runtime found, version recorded, every required flag listed, credential home matches its manifest, route attested | runs |
| `runtime_missing` | `muse` not found or not executable | `failed` / `launch_error` |
| `runtime_error` | `--version` or `exec --help` failed, timed out or printed nothing | `failed` / `runtime_error` |
| `capability_missing` | `exec --help` lacks one of `--json --prompt-file --model --disable-approval --max-model-steps` | `unsupported` / `capability_missing`, `missing_capabilities: ["cli_flag:--model", ...]` |
| `auth_override_present` | `META_API_KEY` is set | `failed` / `auth_missing` |
| `auth_route_unconfirmed` | `--auth-route browser-session`, a valid `--muse-home` or a recorded `--session-manifest` not given | `failed` / `auth_missing` |
| `auth_missing` | credential home missing, empty, or without candidate session state | `failed` / `auth_missing` |
| `auth_ambiguous` | credential home shared, symlinked or holding a symlink/special file, the adapter's own `HOME`, showing a stored key, or no longer matching its manifest | `failed` / `auth_missing` |

The response `error_message` always starts with the specific outcome name.

## Registering the runtime

The adapter is a trusted registration (`Runtime` in `internal/evaluation/registry.go`): an absolute executable plus argv.
Example argv (the host substitutes `{request_path}`):

```
--request {request_path} --muse-home /home/eval/muse-home --session-manifest /home/eval/muse-session.json \
  --auth-route browser-session --muse-bin /home/eval/.local/bin/muse \
  --account-pool meta-eval --max-model-steps 100 --timeout 25m
```

Pass `HOME` through `PassThroughEnv` only so the adapter can refuse a credential home equal to it; register no
credential references, because the subscription session lives in the dedicated credential home. Declare only `structured_output`: the adapter does not
claim source retrieval, PDF access or named tools, and returns `unsupported` / `capability_missing` for a request that
needs them. Set the host runtime timeout longer than `--timeout`: the adapter's own timeout kills muse and its process
group cleanly, whereas a host-side kill of the adapter cannot reach muse's descendants.

Flags: `--muse-bin` (default `muse` on `PATH`), `--muse-home`, `--session-manifest`, `--auth-route`, `--record-session`
(record mode, above), `--account-pool` (recorded as `account_pool`; `unknown`
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
flags, auth ambiguity (API-key override, missing/empty/shared/symlinked credential home, symlinks and special files inside
it, own-HOME home, stored-key hints, missing/unsafe/malformed/foreign session manifests, unrelated or changed session
entries), session recording, malformed and missing output, exit zero without completion, non-zero exits, timeout, SIGTERM
with a surviving grandchild). No test installs Muse, reads a credential or makes a paid call.
