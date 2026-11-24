# M3-B · Quarantine Read Isolation and ClamAV Scanner Adapter

- Date: 2026-11-24
- Status: Draft implementation; based on M3-A PR #4
- Branch: `feat/m3b-quarantine-clamd-scanner`
- Scope: worker-only read interface, explicit single-key S3 adapter, per-entry ClamAV INSTREAM scanning, and fail-closed tests
- Not in scope: job orchestration, schema migrations, scan result persistence, production-ready worker, publishing or user-facing API.

## 1. Architecture

```text
Private Quarantine Bucket
  |
  | Worker identity: s3:GetObject on quarantine/* only; no ListBucket/Put/Delete
  v
QuarantineReader.Open(ctx, exactDBObjectKey, expectedSize)
  |
  v
Bounded private spool (future M4; no user-provided filesystem path)
  |
  v
M3-A validator.ValidateZIP / ValidateHTML -> Manifest v1
  |
  v
scanner.ScanManifest (same spool)
  |      verifies each entry SHA256 and exact stream length
  v
clamd.Client.Scan (Unix socket, zINSTREAM, length-prefixed chunks)
  |
  +-- all entries clean and fully read -> in-memory scan summary
  +-- infected / oversized / unavailable / timeout / error -> reject
```

The scanner Summary is not permission to publish. M3-C must persist the input source SHA256, validator version, Manifest hash, scanner signature freshness and complete terminal result before M4 can make a decision.

## 2. Quarantine Reader

- Interface in `internal/storage/quarantine_reader.go` is independent of the existing `QuarantineStore`.
- Implemented as `internal/storage/minio/reader.go` with a dedicated S3 credential.
- API upload Store remains write/delete only and is unchanged.
- Worker API only exposes `Open(ctx, objectKey, expectedSize)`; no listing, mutating, presigned URL or public URL.
- The exact key must be loaded from the authorized Upload Session database record, never user-provided.
- The object key must pass the existing quarantine-prefix validation.
- Reject S3 object sizes different from DB ExpectedSize; wrap response with exact-length verification.
- Production readers require TLS; insecure development is explicit and must remain isolated.
- NewReader avoids BucketExists / GetBucketPolicy so a prefix-scoped GetObject-only identity can be used.

### Required sample IAM policy

See `deploy/minio/readonly-worker-policy.example.json`. Replace the placeholder bucket with a privately provisioned bucket and use an *independent* service account. This is an identity policy, not an anonymous bucket policy.

## 3. ClamAV INSTREAM client

- Uses only a local Unix domain socket; TCP is intentionally not supported in this first adapter.
- Sends `zINSTREAM\0`, then each chunk as 4-byte big-endian size + bytes, terminated by a zero-size chunk.
- Reads exactly declaredSize bytes; a short source, extra bytes, unknown response, timeout, cancel, I/O failure or over-limit request is not clean.
- Only the exact NUL-terminated `stream: OK` response produces VerdictClean.
- `stream: ... FOUND` produces VerdictInfected, including `Heuristics.Limits.Exceeded`.
- `stream: ... ERROR`, malformed/truncated replies and missing terminal response produce a non-clean error.
- The socket must live in a protected local directory. No public TCP clamd listener.
- MaxStreamBytes and Timeout are separately configurable; the daemon's `StreamMaxLength` must be >= MaxStreamBytes.
- The daemon must be configured to alert on exceeded resource limits. No skipped or partial scan may count as clean.

**Critical scanner limitation:** ClamAV may skip files over its MaxFileSize / MaxScanTime and otherwise report them clean, depending on daemon configuration. The deployment must enable `AlertExceedsMax`, enforce per-file size at the Worker, and check scanner health/signature freshness separately. ClamAV cannot certify that HTML/JS is safe from phishing, XSS or exfiltration.

## 4. Per-entry verification

`scanner.ScanManifest` accepts an M3-A Manifest, exact immutable source and a Scanner interface.

- SourceType must be ZIP or standalone HTML.
- ZIP entries are opened by original `source_path`, never by guessed publish path.
- Manifest entries must have unique source/publish paths and matching lengths.
- Every entry is streamed through the scanner; a scanner that returns early without consuming bytes is rejected.
- ZIP CRC is reached by fully draining each entry.
- Every scanned byte stream must match the Manifest SHA-256.
- All entries must return VerdictClean. Any infected or unknown verdict fails the entire call.
- Only after all entries pass does the function return a Summary.
- No database mutation or ready-to-publish state is produced in this phase.

## 5. Test and acceptance gates

CI must run `gofmt`, `go mod tidy` consistency, `go vet ./...`, `go build ./...`, PostgreSQL migrations/integration tests, `go test -race -count=1 ./...`, and M3-A Fuzz smoke.

Unit tests include:

- Fake local Unix socket testing full INSTREAM framing across multiple chunks.
- Explicit clean, infected, ERROR, malformed, truncated, absent, and ambiguous scanner replies.
- Short / extra scan input, configured size limit, cancellation.
- Standalone HTML scan, common-root ZIP scan, source-path mapping.
- Early-return fake scanner, wrong SHA256, wrong source path, wrong file count, infected / unknown result.
- Read-only credentials and strict source byte count adapter checks.

Still **NOT RUN** unless separately demonstrated:

- A real ClamAV daemon EICAR test using a non-malicious test signature fixture.
- An actual MinIO read-only IAM integration test with denied List/Put/Delete operations.
- Real Vite build ZIP and long-duration Fuzz.
- Worker source spool, concurrency, retry, state-machine, audit and production failover tests.

Do not merge M3-B until these integration gaps have corresponding acceptance evidence.

## 6. Next steps

M3-C: add ValidationRun persistence and manifest/source SHA binding.
M4: implement lease-based job runner, exact-key spool, same-source validator+scanner execution, strict scanner health/version capture, retries, cleanup, idempotency and audit/outbox.
M5: only publish passed immutable versions under isolated Artifact Origins.

## References

- ClamD Protocol: https://docs.clamav.net/manual/Usage/ClamdProtocol.html
- ClamAV Scanning (size limits): https://docs.clamav.net/manual/Usage/Scanning.html
- MinIO Go SDK GetObject: https://docs.min.io/aistor/developers/sdk/go/api/
