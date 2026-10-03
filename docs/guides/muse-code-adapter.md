# Muse Code Adapter Configuration

The Muse Code subscription adapter enables comparison reviews using Meta's Muse Code model through the Muse CLI. This document covers installation, configuration, and setup.

## Prerequisites

- **Muse CLI**: Install muse-spark-1.3 from Meta's official distribution
- **Meta Power Subscription**: The adapter requires an active Meta Power subscription, not pay-as-you-go
- **Access to Odonian**: Running alongside an Odonian evaluation deployment

## Installation

### Install muse-spark-1.3

Follow the [official Muse Code documentation](https://dev.meta.ai/docs/muse-code) to install the CLI:

```bash
# Example: download and install muse-spark-1.3
# Instructions vary by platform; consult Meta's official docs
```

Verify installation:
```bash
muse --version
```

Expected output: A version string indicating muse-spark-1.3

### Verify CLI Flags

The adapter invokes `muse exec` with the following flags. Confirm these are supported by your installed version:
```bash
muse exec --help
```

Required flags:
- `--model <model-id>`: Specify the model identifier (e.g., `muse-spark-1.3`)
- `--output-format jsonl`: Request JSONL output format
- `--workspace <path>`: Path to the code snapshot workspace
- `--input <prompt>`: The prompt/task to review

## Configuration

### Subscription Authentication

The adapter requires Meta Power subscription authentication, configured via one of these methods (checked in order):

1. **MUSE_AUTH environment variable**: Path to a Meta auth token or config file
   ```bash
   export MUSE_AUTH=/path/to/meta/auth
   ```

2. **MUSE_CONFIG environment variable**: Path to a Muse configuration file with auth
   ```bash
   export MUSE_CONFIG=/path/to/muse/config.json
   ```

3. **Default location**: `~/.muse/auth` in your home directory
   ```bash
   # Meta CLI tools store subscription auth here by default
   ```

### Credential Security

**IMPORTANT**: The adapter detects and rejects explicit Meta API keys:
- Setting `META_API_KEY` in the environment will cause the adapter to fail with an auth error
- This prevents accidental pay-as-you-go billing instead of using the configured subscription
- Always configure subscription auth via MUSE_AUTH, MUSE_CONFIG, or the default location

### Verify Configuration

Run the preflight check without starting a full review:
```bash
./muse-adapter \
  --request <path-to-request.json> \
  --preflight
```

Exit code 0 indicates preflight passed; non-zero indicates a configuration issue. The preflight writes a response to the `result_path` specified in the request, recording the effective runtime version and configuration.

## Usage

The Muse adapter is invoked automatically by the Odonian evaluation runner when a Muse candidate is selected for comparison review. No manual invocation is typically needed.

### Direct Invocation (for debugging)

```bash
./muse-adapter \
  --request /path/to/request.json
```

Where `request.json` contains:
```json
{
  "version": 1,
  "run_id": "run-muse-12345",
  "snapshot_path": "/path/to/code/snapshot",
  "blinded_prompt": "Review this code for correctness issues...",
  "tool_access": {
    "require_source_retrieval": true,
    "require_pdf_access": false,
    "tools": []
  },
  "result_path": "/path/to/result.json"
}
```

## Troubleshooting

### "muse cli not installed or not accessible"
- Verify `muse --version` works in your shell
- Check that the muse installation is in your PATH
- Install muse-spark-1.3 from Meta's official distribution

### "no subscription auth configured"
- Set one of: MUSE_AUTH, MUSE_CONFIG, or configure ~/.muse/auth
- Verify the file path exists and is readable
- Consult Meta's Muse Code documentation for subscription setup

### "META_API_KEY is set in environment"
- Unset the META_API_KEY variable; it indicates pay-as-you-go, not subscription auth
- Use MUSE_AUTH or MUSE_CONFIG instead
- `unset META_API_KEY`

### "muse exec failed: exit status ..."
- Check Odonian logs for the actual muse error message
- Verify the blinded prompt is valid
- Ensure the snapshot_path exists and contains the code to review

## Model and Configuration Recording

The adapter records the following for each review:

- **Model ID**: `muse-code` (the model family)
- **Model Revision**: `muse-spark-1.3` (the pinned model version)
- **Runtime Name**: `muse-code-cli` (the execution environment)
- **Runtime Version**: Automatically read from `muse --version` output
- **Account Pool**: Unknown (subscription routing is configured but not exposed by the CLI)
- **Prompt Version**: Unknown (Muse's internal prompt engineering is not exposed)
- **Tools**: Unknown (Muse's available tools are not exposed via CLI)

These values are immutable for a given candidate version; changing any setting creates a new candidate for evaluation.

## Reference

- [Muse Code Official Documentation](https://dev.meta.ai/docs/muse-code)
- [Muse Code Auth Configuration](https://dev.meta.ai/docs/muse-code/auth)
- [Muse Code Subscriptions](https://dev.meta.ai/docs/muse-code/subscriptions)
- [Odonian Evaluation Design](../features/research-pacing-and-reviewer-evaluation.md)
