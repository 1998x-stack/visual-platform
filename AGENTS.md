# AGENTS.md — Visual Platform

This repository contains public design documentation and the beginnings of a private enterprise platform. Do not claim M0/M1 is a production-ready hosting service.

## Engineering principles

- Treat uploaded HTML/JS and ZIP as untrusted executable browser content.
- Keep static hosting (M2–M5) and dynamic server-side execution (future runtime) separate.
- Never expose MinIO buckets or allow object-key construction from a URL path.
- All API actions require server-side authorization; no role from client JSON may be trusted.
- Every cross-workspace action must be covered by negative tests.
- A workspace's created_by is not permanent ownership; ownership is tied to workspace kind / owner_ref.
- An artifact's created_by never implies ownership or publication rights.
- Do not add production bypass headers. Development identity simulation must only be active with APP_ENV=development and a loopback listener.
- Restrict personal workspace creation to the authenticated user; nonpersonal scopes need separate enterprise provisioning.
- Prefer migrations and small ADRs for changes affecting authorization, origin boundaries, or publication consistency.
- Keep all SQL parameterized and check authorization inside write transactions.
- If an action changes ACL, membership, publication or ownership, write an audit event in the same transaction.
- Follow Go standard library net/http and keep request, header and body limits explicit.
- Do not add dependencies unless necessary; pin module versions, check licenses and advisories.

## Current implementation status

M0: Go API skeleton, PostgreSQL schema, dev setup, CI, fail-closed worker/gateway stubs — implemented baseline.
M1: Workspace / Artifact metadata, role and ACL policy, developer-only REST API — implemented baseline.
M2: Upload Session, quota reservation, streamed checksum-verified private Quarantine storage — implemented baseline; production operations are not complete.
M3-A: ZIP + standalone HTML Validation Core is under review in draft PR #4 on `feat/m3-zip-validation-core`; Go / PostgreSQL CI and 10-second fuzz smoke pass, but extended local fuzz and representative archives are NOT RUN. Not merged or integrated with Worker.
M3-B/C and M4–M10: NOT IMPLEMENTED.

## Review commands

    go mod tidy
    gofmt -w cmd internal
    go vet ./...
    go test -race ./...
    make dev-db && make migrate && DB_TEST_URL=... go test -race ./...

Prefer PRs with tests, README updates and explicit security acceptance notes.


## ZIP validation safety rules

- Never extract user ZIP entries to a host filesystem path. Validate through `archive/zip` and bounded readers; publish from a verified Manifest in a later stage.
- Enforce archive, central-directory, entry-count, per-file, aggregate-uncompressed, compression-ratio, path-length and path-depth limits before / during reads.
- Reject ZIP64 and multi-disk archives in the initial MVP until there is a demonstrated product need and a separately tested bounded parser.
- A successful structural Manifest is not an antivirus verdict and is not permission to publish.
- Keep Quarantine API credentials write/delete-only. A future Validation Worker must use a separate exact-key read-only S3 identity.
- Do not expose user HTML / JavaScript on the management Origin. Keep per-Artifact origin isolation.
- Any scanner outage, timeout, unsupported archive format or incomplete scan must fail closed; never silently skip scanning in production.
