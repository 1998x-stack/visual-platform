# M3-A · ZIP and Standalone HTML Validation Core Specification

- Status: Implementation in review on `feat/m3-zip-validation-core`
- Date: 2026-11-24
- Scope: deterministic ZIP structure validation, standalone HTML byte validation, and Manifest generation only
- Non-goal: production publication, antivirus verdict, browser sandbox, Worker orchestration

## 1. Purpose

M2 receives a byte-exact upload into a private Quarantine bucket. It does not establish that the bytes are a valid static site. M3-A adds a bounded, deterministic validator for a constrained static-site ZIP or a standalone HTML file and emits the same content Manifest contract.

A Manifest is an inventory and integrity record. It is **not** a malware verdict, trust label, authorization grant, or publication command.

## 2. Processing contract

Input:

- A seekable `io.ReaderAt` over a private, bounded source.
- The exact source byte length from the Upload Session.
- Explicit server-controlled resource limits.
- ZIP uploads use the ZIP limits below; standalone HTML defaults to 10 MiB.

Output:

- Manifest schema version 1.
- A sorted list of regular files with normalized publish path, original `source_path`, actual byte length, SHA-256, and server-derived MIME.
- Aggregate uncompressed bytes and source archive size.

For standalone HTML, the validator maps the exact uploaded bytes to `index.html`, checks the declared source length against the bounded reader, and calculates SHA-256. It does not parse or sanitize HTML.

The validator does not:

- Extract files to disk.
- Execute scripts, HTML, or server-side code.
- Read from S3 directly.
- Change Upload Session state or persist the Manifest.
- Claim that content is malware-free.

The caller must spool the exact-key Quarantine object to a private temporary file or another bounded random-access source. A future Worker must use a dedicated read-only identity, create temporary files with mode 0600 on a dedicated no-exec volume, enforce a disk quota, remove the spool on every exit path, and enforce a job deadline and concurrency limit.

## 3. Parse and validation sequence

1. Validate source presence and compressed archive size.
2. Read the bounded ZIP End of Central Directory (EOCD) window.
3. Preflight central-directory count and byte length before calling `archive/zip.NewReader`.
4. Reject ZIP64 and multi-disk archives in the MVP. The current product limits do not require ZIP64.
5. Inspect every central-directory entry:
   - UTF-8 and NFC path requirements.
   - No absolute path, drive letter, backslash, NUL, percent escape, query / fragment delimiter, Windows-invalid character, hidden segment, dot segment, trailing dot / space, or reserved Windows device name.
   - No symlink or special file.
   - No encrypted entry.
   - Only Store and Deflate compression.
   - No server-side executable or nested-archive extension.
   - No duplicate, case-fold collision, Unicode-normalization collision, or file/directory prefix conflict.
   - Enforce declared file / aggregate sizes and compression ratio.
   - Check compressed data ranges for overlap and bounds.
6. Open each regular entry and read through EOF with a hard limit of declared size + 1. This forces decompression and CRC verification while calculating SHA-256. Compare actual and declared lengths.
7. Require a root `index.html`. If there is no root index and all regular files share one top-level directory, strip exactly that one common wrapper directory and then require `index.html`.
8. Sort Manifest entries by UTF-8 path and return schema version 1.

No extraction path is ever constructed. ZIP names are logical object paths only.

## 4. Initial resource limits

| Limit | Default | Purpose |
|---|---:|---|
| Compressed archive | 100 MiB | Align with current M2 single-upload default |
| Standalone HTML | 10 MiB | Bound a single active HTML document |
| Central directory | 8 MiB | Bound parser metadata allocation |
| Total ZIP entries | 5,000 | Bound directory metadata |
| Regular files | 4,000 | Bound Manifest and file-open count |
| One uncompressed file | 64 MiB | Bound per-entry read / scan cost |
| Total uncompressed bytes | 512 MiB | Bound decompression work |
| Compression ratio | 100:1 | Reject extreme expansion |
| Path bytes | 1,024 | Bound path processing |
| Path depth | 32 | Bound path tree complexity |

These are initial guardrails, not capacity promises. Validate them against representative Vite, static-report, chart-heavy, and documentation-site archives before increasing them. Limits must remain server-controlled and must never be increased by client-supplied metadata.

## 5. Path and file policy

- Paths must be UTF-8 and already NFC-normalized. Non-NFC names are rejected rather than silently rewritten.
- A Unicode case-folded path key is used for collision detection.
- Hidden path segments are rejected in the MVP. This excludes `.env`, `.git`, `.htaccess`, and similar dotfiles.
- Windows reserved device names and trailing dots / spaces are rejected for cross-platform determinism.
- Server-side executable extensions and nested archive extensions are rejected.
- Common static extensions receive deterministic MIME values. Unknown extensions map to `application/octet-stream`.
- HTML, JavaScript, CSS, SVG, and WebAssembly remain active browser content. They are allowed only because the eventual content host is isolated from the management Origin; this validator does not sanitize their behavior.

## 6. Rejection codes

The package returns a typed `ValidationError` with a stable code. Initial codes include:

- `invalid_archive`, `archive_too_large`, `central_directory_too_large`
- `too_many_entries`, `too_many_files`, `zip64_unsupported`, `multi_disk_unsupported`
- `unsafe_path`, `path_collision`, `symlink_unsupported`, `special_file_unsupported`
- `encrypted_entry`, `compression_unsupported`, `unsupported_file_type`
- `file_too_large`, `total_uncompressed_too_large`, `compression_ratio_exceeded`
- `corrupt_entry`, `entry_size_mismatch`, `missing_index_html`, `archive_has_no_files`
- `validation_canceled`

Do not return parser internals or raw object keys to the browser. Persist the stable code and a correlation ID in the future validation result / audit record.

## 7. Manifest v1

Top-level fields:

- `schema_version`
- `index_file` (always `index.html`)
- `stripped_root` (present only when a common wrapper directory was removed)
- `source_size`
- `entry_count` (regular files only)
- `total_uncompressed_bytes`
- `entries`

Each entry:

- `path` (normalized publish path)
- `source_path` (original ZIP entry path; `index.html` for standalone HTML)
- `size`
- `sha256`
- `content_type`

Entries are sorted by publish path. For ZIP publication, the Publisher must read the exact `source_path` from the source archive and write those bytes to `path`; it must never infer the source entry by reversing the normalized path. `source_path` is validated and unique. Timestamps, owner IDs, ZIP permissions, source object keys, and other nondeterministic / internal fields are excluded.

## 8. Security boundary and follow-up

M3-A only validates ZIP structure and content integrity. It does not detect all malicious JavaScript, phishing pages, data exfiltration, or browser-side abuse.

M3-B must add:

- A separate Worker-only Quarantine reader with exact-key read-only permissions.
- ClamAV (or an approved scanner) integration with explicit scan timeout, max file size, max total scan size, recursion, and file-count limits.
- Configure scanner limits so oversized / skipped entries are reported (for ClamAV, review `AlertExceedsMax` / `--alert-exceeds-max` behavior); an incomplete scan must never be treated as clean.
- Fail-closed behavior for scanner unavailable, timeout, unsupported archive, or incomplete scan.
- No public VirusTotal upload or other third-party content disclosure by default.

M3-C / M4 must add:

- Validation state transitions and durable result records.
- Manifest persistence with schema version and source SHA-256 binding.
- Idempotent job processing, leases, retry policy, and transactional audit / outbox.
- A hard rule that Publisher accepts only a Manifest bound to the exact Upload Session and a passed scan result.

The management site and user Artifact content must use different registrable domains and per-Artifact Origins. Antivirus success does not make browser content safe.

## 9. Test requirements

Automated tests cover:

- Valid root index and one-level wrapper directory.
- Standalone HTML exact-length read, SHA-256, size limit, and cancellation.
- ZIP Slip, absolute paths, Windows drive / device names, backslashes, hidden paths, encoded delimiters, malformed segments.
- Duplicate entries, case-fold collisions, file / directory conflicts.
- Symlink and special-file rejection.
- Unsupported extensions, encryption, unsupported compression, ZIP64, multi-disk.
- Archive, central-directory, entry-count, file-count, per-file, aggregate-size, and compression-ratio limits.
- CRC / decompression failure and actual-vs-declared size mismatch.
- Canceled context and malformed input fuzz seeds.

Before M3-A is merged, run `gofmt`, `go mod tidy`, `go vet ./...`, `go build ./...`, and `go test -race -count=1 ./...` on Go 1.27.4 with PostgreSQL 18. CI also runs a 10-second ZIP-validator fuzz smoke test. Run an extended local fuzz campaign separately and record duration / coverage / corpus; the CI smoke test is not a substitute.


## 10. References

- OWASP File Upload Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/File_Upload_Cheat_Sheet.html
- OWASP Path Traversal Prevention Cheat Sheet: https://cheatsheetseries.owasp.org/cheatsheets/Path_Traversal_Prevention_Cheat_Sheet.html
- Go 1.27.4 archive/zip: https://pkg.go.dev/archive/zip@go1.27.4
- ClamAV clamd.conf reference (scan size, file size, recursion, file count and scan time): https://github.com/Cisco-Talos/clamav/blob/main/docs/man/clamd.conf.5.in
