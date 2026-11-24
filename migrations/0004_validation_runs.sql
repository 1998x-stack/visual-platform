-- M3-C: one validation result lifecycle per immutable received Upload Session.
-- Apply after 0003_upload_quarantine.sql. Idempotent on reapplication.
BEGIN;

CREATE TABLE IF NOT EXISTS validation_runs (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  upload_id UUID NOT NULL UNIQUE REFERENCES upload_sessions(id) ON DELETE RESTRICT,
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
  artifact_id UUID NOT NULL REFERENCES artifacts(id) ON DELETE RESTRICT,
  source_type TEXT NOT NULL CHECK(source_type IN ('html','zip')),
  source_size BIGINT NOT NULL CHECK(source_size > 0),
  source_sha256 TEXT NOT NULL CHECK(source_sha256 ~ '^[0-9a-f]{64}$'),
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK(status IN ('pending','running','retryable','validated','rejected','failed')),
  attempt_count INTEGER NOT NULL DEFAULT 0 CHECK(attempt_count BETWEEN 0 AND 3),
  max_attempts INTEGER NOT NULL DEFAULT 3 CHECK(max_attempts BETWEEN 1 AND 3),
  lease_token UUID,
  lease_until TIMESTAMPTZ,
  next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  validated_manifest JSONB,
  manifest_sha256 TEXT CHECK(manifest_sha256 IS NULL OR manifest_sha256 ~ '^[0-9a-f]{64}$'),
  scanned_files INTEGER CHECK(scanned_files IS NULL OR scanned_files > 0),
  scanned_bytes BIGINT CHECK(scanned_bytes IS NULL OR scanned_bytes >= 0),
  scanner_version TEXT,
  signature_version TEXT,
  signatures_updated_at TIMESTAMPTZ,
  failure_code TEXT CHECK(failure_code IS NULL OR failure_code IN
    ('invalid_content','malware_detected','source_mismatch','scanner_unavailable',
     'scanner_incomplete','signature_stale','internal_error','retry_exhausted')),
  completed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (workspace_id,artifact_id)
    REFERENCES artifacts(workspace_id,id) ON DELETE RESTRICT,
  CHECK ((status = 'running') = (lease_token IS NOT NULL AND lease_until IS NOT NULL)),
  CHECK ((status = 'validated') = (
    validated_manifest IS NOT NULL AND manifest_sha256 IS NOT NULL AND
    scanned_files IS NOT NULL AND scanned_bytes IS NOT NULL AND
    scanner_version IS NOT NULL AND signature_version IS NOT NULL AND
    signatures_updated_at IS NOT NULL)),
  CHECK ((status IN ('validated','rejected','failed')) = (completed_at IS NOT NULL)),
  CHECK ((status IN ('retryable','rejected','failed')) = (failure_code IS NOT NULL)),
  CHECK (status <> 'validated' OR failure_code IS NULL),
  CHECK (status <> 'validated' OR scanned_bytes <= 536870912)
);

CREATE INDEX IF NOT EXISTS idx_validation_runs_due
  ON validation_runs(next_attempt_at,created_at,id)
  WHERE status IN ('pending','retryable');

CREATE INDEX IF NOT EXISTS idx_validation_runs_expired
  ON validation_runs(lease_until)
  WHERE status = 'running';

CREATE INDEX IF NOT EXISTS idx_validation_runs_artifact
  ON validation_runs(artifact_id,created_at DESC);

COMMIT;
