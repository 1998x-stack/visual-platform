# M4-A: Private Validation Worker and Source-Bound Scanning

Status: Draft PR #7, not merged or cleared for production. Updated: 2026-11-24.

## 1. Scope and security boundary

M4-A establishes a single-consumer Worker using only privileged server-side DB metadata. It does **not** publish files, expose an HTTP scan endpoint, create a publicly readable bucket, or make HTML/JS safe to execute in a privileged origin.

Dependencies (stacked): PR #4 M3-A ZIP/HTML validator -> PR #5 M3-B read-only S3/ClamAV -> PR #6 M3-C validation state -> PR #7 Worker.

## 2. Implementation

```text
upload_sessions(received) -- durable committed M2 receipt
        |
        v
EnqueueReceivedBatch(limit=25) -- SKIP LOCKED + ON CONFLICT, auditable
        |
        v
ClaimNextValidation() -- lease_token UUID + 5 min DB lease
        |
        v
QuarantineReader.Open(DB object_key, expected_size) -- read-only IAM
        |
        v
private spool temp file (0700 owner directory / 0600 file)
        |
        +--> verify exact bytes, stream EOF, raw source SHA-256
        |
        +--> validator.ValidateHTML / ValidateZIP (M3-A)
        |
        +--> clamd zVERSION (fresh DB and supported protocol)
        |
        +--> scanner.ScanManifest (per file, source_path, hashes + CRC)
        |
        +--> clamd zVERSION again (identical engine/DB/date)
        |
        v
CompleteValidation(lease_token, source hash, manifest + scan evidence)
        |
        +-- validated only if current live lease and DB checks pass
        +-- otherwise no publication, fail closed
```

The same spool handle is passed as an `io.ReaderAt` to both the M3-A validator and M3-B scanner. The file is removed on every normal return, including source mismatch, scanner error, timeout or rejected archive. A crashed process may leave a private orphan; M4-B needs a startup janitor and disk quota. Do not mount the spool under a web root.

## 3. Fail-closed handling

| Condition | Classification | DB effect |
|---|---|---|
| Invalid ZIP, unsupported type, archive too large | `invalid_content` | rejected, no retry |
| Remote object changed, short, extra, wrong SHA-256 | `source_mismatch` | rejected, no retry |
| ClamAV `FOUND` verdict | `malware_detected` | rejected, no retry |
| Storage/network error or local I/O error | `internal_error` | retryable, capped |
| ClamAV unavailable, VERSION unavailable | `scanner_unavailable` | retryable, capped |
| Scanner short read, malformed response, daemon changed | `scanner_incomplete` | retryable, capped |
| Lease expired/was reclaimed | database rejects stale completion | no success |

The Worker never infers clean status from `nil` error alone: it requires all files to have explicit clean verdicts and matching file SHA-256 hashes, plus unchanged scanner engine/signature metadata. Parent PR #6 caps retries at three attempts with a 1m, then 2m delay.

The clamd VERSION response commonly has the form `ClamAV <engine>/<db-revision>/<db-date>`. Unknown formats, stale DB dates (older than 48h), future DB dates (more than 5m), disabled VERSION commands and network/socket errors fail closed. The parser currently interprets unzoned dates as UTC; operational ClamAV timezone and signature database provenance **must be verified with a real daemon** before deployment. Do not substitute a local wall-clock time or fake DB version as evidence.

## 4. Hard resource limits

- Max downloaded bytes 100 MiB and max standalone HTML 10 MiB, enforced before remote reads.
- Source is copied with exact count and SHA-256 into one private randomized file, never extracted into the host filesystem.
- ClamAV stream cap 64 MiB per file, matching M3-A's maximum uncompressed entry size; daemon's StreamMaxLength / MaxFileSize / MaxScanSize must be checked at deployment and `AlertExceedsMax` enabled.
- Job timeout 4 minutes and database lease 5 minutes (no heartbeat in M4-A).
- Default polling interval 3 seconds; one job at a time per process. Multiple processes coordinate through PostgreSQL SKIP LOCKED, but shared disk reservation/concurrency limits still require M4-B.
- Temporary storage must be a pre-created absolute 0700 non-symlink directory owned by the Worker UID, with no symlink ancestors.

## 5. Worker environment

No secrets are hard-coded. Example configuration in `deploy/worker.env.example`. The worker requires:

- `DATABASE_URL`: existing restricted PostgreSQL connection, with migration 0004 applied.
- `S3_ENDPOINT`, `S3_BUCKET`, `S3_REGION`, `S3_USE_SSL` (TLS true outside explicit loopback development).
- `WORKER_S3_ACCESS_KEY`, `WORKER_S3_SECRET_KEY`: **dedicated read-only credentials**. Do not use API write/delete keys.
- `CLAMD_SOCKET`: local Unix socket. No TCP support.
- `WORKER_TEMP_DIR`: absolute, UID-owned 0700 directory, provisioned separately from public/static directories.

```sh
make migrate
make worker-once  # perform at most one iteration
make worker-dev   # continuous single-consumer loop
```

Worker startup performs a fail-closed ClamAV VERSION freshness probe before accepting jobs. It logs sanitized job IDs, counts and failure codes; do not log raw object keys, worker lease tokens or full HTML.

## 6. Acceptance evidence and remaining limits

GitHub CI tests:
- gofmt, vet, build, repeatable PostgreSQL migrations 0001,0003,0004.
- Race tests for reconciliation, same-spool HTML/ZIP, scanner outcomes, bad SHA256 and excess bytes, private spool cleanup, permissions, M3-C DB fencing.
- ClamAV VERSION against a fake Unix socket with fresh/stale/malformed responses.
- ZIP validator 10-second fuzz smoke.

**NOT RUN:** real MinIO Get-only IAM denial tests, real ClamAV EICAR and limit-exceeded tests, real Vite archive long fuzz and CPU/RSS data, untrusted HTML browser-origin threat exercise, clamd DB timezone confirmation, disk-pressure tests, process SIGKILL orphan cleanup, lease heartbeat / outbox.

The API and Gateway cannot serve user-uploaded content in this phase. M4-B should add dedicated container integration tests, self-checking daemon config, resource-limited spool volume, janitor, lease heartbeats and rollout gating. M5 Publisher/M6 Static Gateway remain separate projects.

References:
- https://docs.clamav.net/manual/Usage/ClamdProtocol.html
- https://docs.clamav.net/manual/Usage/Scanning.html
