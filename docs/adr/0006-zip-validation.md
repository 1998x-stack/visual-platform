# ADR-0006：M3-A 无磁盘解压的 ZIP Validation Core

- Status: Accepted for M3-A implementation
- Date: 2026-11-24

## Context

M2 stores uploaded HTML / ZIP bytes in a private Quarantine bucket. The `received` state only means that the byte count and SHA-256 match the upload declaration. It does not mean the ZIP is structurally safe or publishable.

The next stage must inspect untrusted ZIP metadata and decompressed content without allowing archive paths to control the host filesystem. It must also bound parser allocation and decompression work.

## Decision

1. Implement a pure `internal/validator` package that accepts a bounded `io.ReaderAt` and exact source size for ZIP and standalone HTML uploads.
2. Do not extract archive entries to the host filesystem. Read each entry through `archive/zip.File.Open`, with a hard reader limit, and compute SHA-256.
3. Preflight EOCD / central-directory metadata before `zip.NewReader` to bound entry count and central-directory memory. Reject ZIP64 and multi-disk archives in the MVP.
4. Reject absolute / traversal / ambiguous paths, non-NFC paths, hidden segments, Windows reserved names, duplicate / case-fold collisions, file-directory conflicts, symlinks, special files, encrypted entries, unsupported compression, server-side executable extensions, and nested archive extensions.
5. Enforce compressed archive, central directory, entry count, regular file count, per-file uncompressed bytes, aggregate uncompressed bytes, compression ratio, path byte length, and path depth limits.
6. Read every accepted ZIP file to EOF so decompression and CRC checks run; compare actual bytes with declared sizes and generate a deterministic Manifest v1. For a standalone HTML file, enforce a separate 10 MiB default size limit and hash the exact source bytes into the same Manifest shape.
7. Require root `index.html`, allowing exactly one common top-level wrapper directory to be stripped when every regular file is nested beneath it. Preserve the original ZIP `source_path` separately from the normalized publish `path` so a later Publisher can map bytes correctly.
8. Keep M2 Upload API storage permissions unchanged. A future Worker must receive a separate Quarantine identity with custom prefix-scoped `s3:GetObject` only (no ListBucket / PutObject / DeleteObject); application code reads only the exact ObjectKey loaded from the Upload Session. Use short-lived STS session policy or presigned GET if per-job IAM isolation is required.
9. Treat Manifest success as structural integrity only. It is not an antivirus verdict, browser-safety verdict, or publication authorization.

## Initial limits

- Archive: 100 MiB
- Central directory: 8 MiB
- Entries: 5,000
- Regular files: 4,000
- Single uncompressed file: 64 MiB
- Total uncompressed bytes: 512 MiB
- Compression ratio: 100:1
- Path: 1,024 bytes and 32 segments

Tune only after measuring representative enterprise static-site archives.

## Consequences

- No Zip Slip write primitive exists in this stage because no archive path is used as a filesystem destination.
- Parser metadata allocation is bounded before the standard library builds its entry list.
- ZIP64 and multi-disk ZIPs are intentionally unsupported until a concrete requirement exists.
- Manifest generation reads all content, so Worker CPU and wall-clock deadlines remain mandatory.
- Antivirus, validation-state persistence, and publication are separate follow-up stages.
- HTML / JavaScript / SVG remain active browser content; per-Artifact Origin isolation remains mandatory.

## Rejected alternatives

- Extracting ZIPs directly to a shared temp directory.
- Trusting only filename extensions, Content-Type, or central-directory size fields.
- Relying only on Go's optional `zipinsecurepath` behavior.
- Giving the upload API broad object-read permissions.
- Treating antivirus scan success as proof that browser content is safe.
