# Evaluation rollout runbook

This runbook covers deploying and operating the model-agnostic evaluation system for comparing research reviewers. Read [research-pacing-and-reviewer-evaluation.md](../features/research-pacing-and-reviewer-evaluation.md) for architecture and design intent.

## Before starting

- Ensure milestone 1 (research pacing) is deployed and verified in observe or enforce mode.
- Confirm the operator's Muse Power subscription is valid and has sufficient quota.
- Have the configured Meta API key available (do not commit it to the repository).
- Designate a finite cohort cap (e.g., 100 samples) and per-candidate attempt cap (e.g., 20 attempts per model).
- Decide which research projects are eligible for evaluation (at least one).

## Checklist: Power subscription and auth

1. **Verify Meta Muse Power subscription routing**
   - Check with the account holder that the Power subscription is active.
   - Confirm the Muse Code CLI is installed and the version is pinned (e.g., muse-spark-1.3).
   ```bash
   muse --version
   ```

2. **Test subscription authentication without using quota**
   ```bash
   odonian evaluation-pool-set \
     --id meta-muse-evaluation \
     --start-rate 1 \
     --burst 10 \
     --concurrent-limit 2
   ```
   This creates a pool but does not launch evaluations yet.

3. **Verify subscription credentials are securely stored**
   - The Meta API key must not be in the deployment manifest or repository.
   - Use environment-variable injection or a secure secret store.
   - Document the secret name for operational runbooks.

## Creating a campaign

1. **Select a finite cohort and cap**
   - Use `FirstRoundCensus` (internal library call) to examine eligible tasks.
   - Choose a seed and cap (e.g., 50–100 samples) to fit the evaluation window.
   - Export the cohort manifest as JSON.
   ```bash
   # Example: select first 100 research tasks from project ABC123 with clean or rejected round 1
   # (This uses internal library; wrap in a small CLI script if needed)
   cohort=$(odonian evaluation-census \
     --project ABC123 \
     --cap 100 \
     --seed 42)
   ```

2. **Create the evaluation campaign**
   ```bash
   odonian evaluation-create-campaign \
     --id eval-muse-2026-10-03 \
     --name "Muse evaluation baseline study" \
     --projects ABC123 \
     --models muse-spark-1.3 \
     --cohort "$cohort" \
     --cap 100
   ```
   `--cap` is the total attempt limit across all samples and candidates.

3. **Create a candidate version**
   ```bash
   # Identity JSON includes adapter name, model, account pool, runtime, etc.
   identity_json=$(cat <<'EOF'
   {
     "adapter": "muse",
     "model": "muse-spark-1.3",
     "account_pool": "meta-muse-evaluation",
     "runtime": "muse-code-cli-v1.2",
     "tool_config": {
       "tools": ["source_retrieval", "pdf_access"],
       "observers": ["muse_metrics"]
     },
     "prompt_version": "research_reviewer_1.0"
   }
   EOF
   )

   odonian evaluation-create-candidate \
     --campaign eval-muse-2026-10-03 \
     --id candidate-muse-1.3-v1 \
     --identity "$identity_json" \
     --cap 20
   ```
   `--cap` is the per-candidate attempt limit (must not exceed campaign cap).

4. **Optionally add a fake candidate for testing**
   ```bash
   fake_identity=$(cat <<'EOF'
   {
     "adapter": "fake",
     "model": "fake-responder-v1",
     "account_pool": "local-only",
     "runtime": "local",
     "tool_config": {"tools": []},
     "prompt_version": "research_reviewer_1.0"
   }
   EOF
   )

   odonian evaluation-create-candidate \
     --campaign eval-muse-2026-10-03 \
     --id candidate-fake-v1 \
     --identity "$fake_identity" \
     --cap 20
   ```

## Running evaluations: slow start

**Always start with one process at a time.** Avoid parallel submissions and full-scale execution until metrics are stable.

1. **Claim and run one evaluation job**
   ```bash
   sample_id=$(odonian evaluation-get-campaign --id eval-muse-2026-10-03 | \
     jq -r '.campaign.cohort_manifest' | \
     jq -r '.samples[0].id')

   claim_result=$(odonian evaluation-claim-job \
     --sample "$sample_id" \
     --candidate candidate-muse-1.3-v1 \
     --request-id "run-1-$(date +%s)" \
     --ttl-ms 60000)

   job_id=$(echo "$claim_result" | jq -r '.job.id')
   attempt_id=$(echo "$claim_result" | jq -r '.attempt.id')
   ```

2. **Launch the candidate adapter**
   Use the adapter contract in [research-pacing-and-reviewer-evaluation.md](../features/research-pacing-and-reviewer-evaluation.md) section "Candidate runtime adapter contract."
   - Pass the claim result to the adapter.
   - The adapter reads the staged workspace and generates findings.
   - The adapter returns a JSON response with findings, usage, timing.

3. **Finalize the attempt**
   ```bash
   result=$(cat <<'EOF'
   {
     "status": "completed",
     "duration_ms": 12345,
     "usage_tokens": 5000,
     "findings": [
       {
         "id": "f1",
         "severity": "material",
         "claim": "example claim",
         "summary": "example issue",
         "evidence": "example evidence"
       }
     ]
   }
   EOF
   )

   odonian evaluation-finalize-attempt \
     --job "$job_id" \
     --attempt "$attempt_id" \
     --exit-class completed \
     --result "$result"
   ```

4. **Inspect results**
   ```bash
   findings=$(odonian evaluation-finalize-attempt ... | jq '.finding_count')
   echo "Sample $sample_id: candidate found $findings material issues"
   ```

## Recording dispositions and generating reports

After evaluations complete:

1. **Record operator dispositions**
   Operators review candidate findings against the baseline (production Astra/Fable) and record decisions.
   ```bash
   odonian evaluation-record-disposition \
     --campaign eval-muse-2026-10-03 \
     --sample "$sample_id" \
     --candidate candidate-muse-1.3-v1 \
     --finding f1 \
     --disposition valid \
     --evidence "Astra also found this; confirmed in source." \
     --decided-by "ops-team@example.com"
   ```

   Valid disposition values:
   - `valid`: the finding is correct; it matches or exceeds baseline rigor.
   - `invalid`: the finding is incorrect or a false positive.
   - `unresolved`: the finding needs further review or context.

2. **Generate a report**
   ```bash
   report=$(odonian evaluation-get-report --campaign eval-muse-2026-10-03)
   echo "$report" | jq '.report.metrics'
   ```

   The report includes:
   - Per-candidate completion counts and latency.
   - Grouped findings: the same issue across candidates and baseline.
   - Disposition tally: valid, invalid, unresolved counts.
   - Recall metrics relative to the adjudicated baseline finding set.

3. **Interpret results**
   - **Recall**: what fraction of material baseline findings were caught by the candidate.
   - **Precision**: what fraction of candidate findings were valid (not false positives).
   - **Unique findings**: material issues found by candidate but not baseline (potential improvements).
   - **Coverage**: what fraction of cohort samples were completed vs. incomplete or failed.

## Monitoring and rollback

- **Pause a campaign**: if issues arise, pause automatic selection without halting active evaluations.
  ```bash
  odonian evaluation-pause-campaign --id eval-muse-2026-10-03
  ```

- **Inspect pool state**: check active attempt count and bucket tokens.
  ```bash
  odonian evaluation-pool-get --id meta-muse-evaluation
  ```

- **Check attempt details**: for failed attempts, review error class and exit code.
  ```bash
  attempt=$(odonian evaluation-get-attempt --job "$job_id" --attempt "$attempt_id")
  echo "$attempt" | jq '.attempt | {exit_class, error_class, error_message}'
  ```

- **Drain and stop**: allow active attempts to finish; do not claim new jobs.
  ```bash
  odonian evaluation-pause-campaign --id eval-muse-2026-10-03
  # Wait for active attempts to complete (check pool active count).
  # Then disable the evaluation feature or decommission the campaign.
  ```

## Troubleshooting

**Pool not configured**
- Ensure `evaluation-pool-set` has been called for the candidate's account pool.
- The pool must match the `account_pool` in the candidate identity JSON.

**Permission denied**
- Verify the Meta API key is valid and has Muse Code access.
- Check that the API key is not expired.

**Low completion rate**
- Confirm the candidate adapter (e.g., muse-code) is installed and working.
- Check logs for network or credential errors.
- Reduce per-candidate or campaign caps and retry.

**Findings not matching baseline**
- Verify the cohort was built with the correct baseline (Astra/Fable round 1).
- Check that the staged workspace contains the correct source context.
- Review disposition logic: ensure evidence standards are consistent.

## Next steps

Once evaluations and reports are stable:
1. Analyze accuracy metrics (recall, precision, unique findings).
2. Document the candidate's strengths and weaknesses.
3. Decide whether to approve the candidate for production or iterate.
4. Archive the campaign and report for audit.
