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

M0: Go API skeleton, PostgreSQL schema, dev setup, CI, fail-closed worker/gateway stubs.
M1: Workspace / Artifact metadata, role and ACL policy, developer-only REST API.
M2–M7: NOT IMPLEMENTED.

## Review commands

    go mod tidy
    gofmt -w cmd internal
    go vet ./...
    go test -race ./...
    make dev-db && make migrate && DB_TEST_URL=... go test -race ./...

Prefer PRs with tests, README updates and explicit security acceptance notes.
