# Odonian CLI Reference

## Overview

The Odonian CLI (`odonian` command) provides direct access to Odonian board functionality including task management and evaluation campaigns. Authentication uses `ODONIAN_TOKEN` environment variable.

## Environment Variables

- `ODONIAN_URL` (required): Odonian server URL (e.g., `http://localhost:8080`)
- `ODONIAN_TOKEN` (required): Bearer token for authentication
- `ODONIAN_PROJECT` (required): Default project ID for operations

## Task Management Commands

See `odonian <command> -h` for full option lists.

### Claiming and Managing Work

```bash
# Claim a task
odonian claim <task_id>

# Heartbeat to extend lease
odonian heartbeat <task_id>

# Submit work
odonian submit <task_id> --result "..." --pr "https://..."

# Transition task state
odonian transition <task_id> --to blocked --note "reason"
```

## Evaluation Campaign Commands

These commands manage model-agnostic reviewer evaluation campaigns.

### Create Campaign

```bash
odonian evaluation-create-campaign \
  --name "Muse Spark 1.3 evaluation" \
  --description "Initial comparison study" \
  --cohort-manifest "{...}" \
  --attempt-cap 100 \
  --allowed-projects proj-1,proj-2 \
  --allowed-models muse-spark-1.3
```

### Get Campaign

```bash
odonian evaluation-get-campaign <campaign_id>
```

### Get Campaign Status

```bash
odonian evaluation-get-campaign-status <campaign_id>
```

Returns compact machine-readable status including pause state, attempt capacity, and per-candidate information.

### Pause Campaign

```bash
odonian evaluation-pause-campaign <campaign_id>
```

Pauses the campaign, blocking new job admission.

## Evaluation Job Commands

### Claim Job

```bash
odonian evaluation-claim-job \
  --sample-id sample-123 \
  --candidate-id cand-456 \
  --request-id req-789 \
  --lease-ttl-ms 300000
```

**Response:**
```json
{
  "job": {
    "id": "job-123",
    "sample_id": "sample-123",
    "candidate_id": "cand-456",
    "current_attempt_id": "att-001"
  },
  "attempt": {
    "id": "att-001",
    "expires_at": "2026-10-03T12:05:00Z"
  }
}
```

### Renew Attempt

```bash
odonian evaluation-renew-attempt \
  --job <job_id> \
  --attempt <attempt_id> \
  --lease-ttl-ms 300000
```

Extends the lease for the current attempt. Uses relative TTL (1-3600000 ms).

### Finalize Attempt

```bash
odonian evaluation-finalize-attempt \
  --job <job_id> \
  --attempt <attempt_id> \
  --fence <fence_attempt_id> \
  --exit-class completed \
  --result '{
    "status": "completed",
    "duration_ms": 45000,
    "usage_tokens": 5000,
    "findings": [
      {
        "id": "f-001",
        "severity": "material",
        "claim": "Variable not initialized",
        "summary": "Missing initialization",
        "evidence": "Line 42"
      }
    ]
  }'
```

**Findings Validation:**
- All finding fields (id, severity, claim, summary, evidence) must be strings
- Unknown fields are rejected
- Findings are structured strictly according to evaluation.Finding

### Get Sample

```bash
odonian evaluation-get-sample \
  --campaign <campaign_id> \
  --sample <sample_id>
```

## Authentication & Error Handling

All commands use `ODONIAN_TOKEN` and `ODONIAN_URL` for authentication. Errors return non-zero exit codes and descriptive messages.

**Common Error Cases:**
- `401`: Missing or invalid token
- `404`: Resource not found
- `409`: Conflict (e.g., already finalized, wrong attempt)
- `400`: Invalid input (validation errors)

## Example Workflow

```bash
# 1. Claim a job
RESULT=$(odonian job claim \
  --sample-id sample-123 \
  --candidate-id cand-456 \
  --request-id req-$(date +%s) \
  --lease-ttl-ms 300000)
JOB_ID=$(echo $RESULT | jq -r '.job.id')
ATTEMPT_ID=$(echo $RESULT | jq -r '.attempt.id')

# 2. Renew lease periodically
odonian job renew \
  --job-id $JOB_ID \
  --attempt-id $ATTEMPT_ID \
  --lease-ttl-ms 300000

# 3. Finalize with results
odonian job finalize \
  --job-id $JOB_ID \
  --attempt-id $ATTEMPT_ID \
  --fence-attempt-id $ATTEMPT_ID \
  --exit-class completed \
  --status completed \
  --result '{"findings": [...]}'
```
