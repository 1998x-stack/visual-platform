# Visual Platform · Engineering Gotchas

Last updated: 2026-11-24

## M3-B: ClamAV INSTREAM terminal frame

**Observed:** GitHub Actions run `45315169342` failed `TestScanAcceptsOnlyExplicitCleanReply` (timeout) and `TestScanReturnsInfectedVerdict` (EOF) even though the client could connect to the Unix socket.

**Root cause:** The client reused the 4-byte length header from the last non-empty data chunk as its terminal frame. The daemon waited for additional bytes instead of processing the scan. A timeout was therefore *not* evidence that ClamAV was unavailable or slow.

**Regression:** A local Unix socket test server reads the actual documented INSTREAM length-prefix sequence and requires the zero-size terminal frame. Fix: explicitly set the 4-byte big-endian length to zero before writing the terminator.

**Recheck:** `go test -race -count=1 ./internal/scanner/clamd` and full GitHub CI must both pass on the fixed commit. Do not claim PASS from runs before the fix.

## M3-B: ClamAV can skip oversized files

ClamAV size and time limits are independent of our ZIP validator. Some skipped files may be treated as clean unless the daemon is configured appropriately. Do not trust an `OK` verdict unless the daemon's `StreamMaxLength`, `MaxFileSize`, `MaxScanSize`, `MaxScanTime`, and exceeded-limit alert behavior have been audited against the worker limits. Missing signatures, daemon errors, and timeout must fail closed. A real daemon test remains NOT RUN.

See https://docs.clamav.net/manual/Usage/Scanning.html and https://docs.clamav.net/manual/Usage/ClamdProtocol.html.

## M3-B: Dedicated Reader IAM and lazy S3 GetObject

The upload API `QuarantineStore` deliberately has Put/Delete only. Do not add Get to it for scanner convenience. The Worker reader must use a separate prefix-scoped `s3:GetObject` identity without List/Put/Delete. The MinIO Go SDK returns a lazy object reader; call `Stat()` and compare `Size` with the database record, then enforce exact byte count as content is consumed. Do not treat ETag as SHA-256. Real MinIO identity-denial tests remain NOT RUN.

## M3-B: CI PostgreSQL client dependency

The existing Go workflow used `apt-get update && apt-get install postgresql-client` on each run, introducing a package-mirror availability dependency. The M3-B branch uses the pre-existing `postgres:18` service container's `psql` via `docker exec -i` with a strict `ON_ERROR_STOP=1` migration loop and repeats the same loop for idempotency. This change must be validated in GitHub Actions.

## M3-A: Validation is not a publication grant

The `received` Upload Session state and a valid Manifest do not prove that the content is safe. M3-A long fuzzing, real Vite artifacts, and resource-profile evidence remain NOT RUN. M3-B must not cause any unscanned file to reach a public Origin. Browser content isolation remains mandatory even after ClamAV reports clean.
