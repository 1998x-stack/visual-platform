# M3-C · Validation State, Source Binding and Fenced Leases

Updated: 2026-11-24. Status: implementation on draft stacked PR #6.

## Goal

Keep the existing M2 `upload_sessions.status='received'` immutable as a byte-integrity receipt. Establish an **independent** validation lifecycle with durable results and strict worker fencing. A validated result **never** authorizes browser publication by itself.

Stacked dependencies: PR #4 ZIP/HTML Validator -> PR #5 Quarantine Reader/ClamAV Adapter -> PR #6 Validation State (this change). No API or production Worker wiring in this PR.

## Storage

Migration: `migrations/0004_validation_runs.sql`.

Exactly one `validation_runs` record per Upload Session (`UNIQUE(upload_id)`), with source `workspace_id`, `artifact_id`, `source_type`, `source_size`, `source_sha256` frozen when enqueued. The database lifecycle is independent of the Upload Session lifecycle.

The stored scan evidence includes:

- `validated_manifest` (JSONB Manifest v1) and `manifest_sha256` of the byte serialization accepted by the Worker.
- `scanned_files` and `scanned_bytes`, required to match all Manifest entries.
- Scanner build/version, signature version and last signature update timestamp. The current admission policy rejects signature evidence older than 48 hours or more than 5 minutes in the future.
- `attempt_count`, `max_attempts` (= 3), `lease_token` (UUID), `lease_until`, `next_attempt_at`, `failure_code`, completion timestamp.
- A durable `audit_events` record for enqueue, claim, exhausted leases, validation success or failure. Worker actor is NULL (machine action), with a request ID for correlation.

Validated rows require all evidence by database CHECK constraints. Non-validated rows cannot carry successful scan evidence. Only terminal statuses have `completed_at`.

## Lifecycle

```text
Upload received & matching expected hash/size
  |
  v
validation.pending
  | ClaimNext(): SKIP LOCKED, DB-generated lease token, attempt=1
  v
validation.running
  |-- successful Worker with matching source+Manifest+scan+signature evidence
  |       -> validated (terminal, no publication grant)
  |-- deterministic invalid ZIP / malware / changed source
  |       -> rejected (terminal)
  |-- ClamAV down / incomplete scan / stale signatures / internal error
  |       -> retryable (1m, 2m exponential backoff)
  |-- transient error on final attempt
  |       -> failed (terminal, retry_exhausted)
  |-- lease expired
          -> re-claimed under a fresh UUID token if attempt budget remains
          -> failed if 3 attempts exhausted
```

**Fencing:** A successful or failed completion requires `status='running'`, the exact current `lease_token`, and `lease_until > now()`. Completion and new claims lock the row; stale workers lose authority even if they finish a scan afterward. Lease expiry cannot mark a result as clean.

**Idempotency:** Enqueue uses `ON CONFLICT(upload_id) DO NOTHING` and returns the same existing lifecycle if its source fingerprint still matches. Repeated successful completion is intentionally rejected, not a second transition. Only one Worker can receive a lease at a time.

**Source binding:** Enqueue verifies that the M2 source actual SHA-256/size matches its declared expected SHA-256/size. Claim verifies it again. Completion re-reads Upload Session under lock and checks its fingerprint against both the persisted snapshot and the completion input.

**Failure classification:** Deterministic failures (`invalid_content`, `malware_detected`, `source_mismatch`) cannot be marked retryable. Infrastructure failures must be transient until the attempt budget is exhausted. The caller is not permitted to classify malware as a temporary error.

**Resource bounds:** Manifest v1 maximum 4000 files, declared and scan sizes match, SHA-256 strings and paths are checked, JSONB size <= 8 MiB, scan bytes <= int64 capacity, scanner identifiers are character and length bounded.

## Non-goals and outstanding security gates

- No real S3 object stream is read by M3-C. `CompleteValidation` trusts a privileged future Worker to hash the actual downloaded object and stream all Manifest entries through the real configured scanner. A caller-provided SHA alone is not proof of scanned bytes.
- No HTTP handler exposes enqueue, claim or completion. In particular there is no user-controlled way to send a fake scanner verdict.
- No Worker loop, signature freshness probing from `clamd VERSION`, lease heartbeats, running-job cancellation, reconciliation for source changes, lifecycle cleanup, or gateway/publishing code.
- Do not deploy the Worker or publish an artifact until the real MinIO read-only IAM tests and actual ClamAV EICAR / limits tests from M3-B are complete.
- Do not merge PR #6 until all PostgreSQL integration and Race tests pass; independent M3-A/M3-B gates remain before upstream merges.

## CI acceptance matrix

| Test | Required behavior |
|---|---|
| Migration replay | `0004` applied twice without DDL or data loss |
| Enqueue | only `received` with exact M2 source hash/size; one record per upload |
| Claim | SKIP LOCKED concurrency yields one lease; expired lease gets a new token |
| Fence | expired/wrong token cannot report success; duplicate completion conflicts |
| Evidence | validates manifest counts/bytes/source fingerprint/scan version/signature age |
| Failures | malware is terminal; scanner down is retried, at most three total attempts |
| Audit | machine events persisted without revealing S3 keys or lease tokens |
| Code | gofmt, go vet, go build, go test -race, ZIP fuzz smoke |

## Next after M3-C

M4 needs an actual Worker with a local, private, bounded spool, raw source SHA-256 recheck, deterministic validation, per-file real ClamAV scan, scanner VERSION/freshness evidence, short heartbeat extension, cancellation and cleanup, then a source-bound call to `CompleteValidation`. M5 Publisher and M6 per-Origin Static Gateway must use **additional** approval and permission gates before a browser can view content.
