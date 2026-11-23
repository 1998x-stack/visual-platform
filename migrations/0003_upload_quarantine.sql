-- M2: upload sessions, workspace quota reservations and quarantine metadata.
-- Apply with psql -v ON_ERROR_STOP=1 -f. Safe to reapply.
BEGIN;

DO $m2$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'artifacts_workspace_id_id_unique'
      AND conrelid = 'artifacts'::regclass
  ) THEN
    ALTER TABLE artifacts
      ADD CONSTRAINT artifacts_workspace_id_id_unique UNIQUE(workspace_id,id);
  END IF;
END $m2$;

CREATE TABLE IF NOT EXISTS workspace_quotas (
  workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  max_bytes BIGINT NOT NULL DEFAULT 1073741824 CHECK(max_bytes > 0),
  used_bytes BIGINT NOT NULL DEFAULT 0 CHECK(used_bytes >= 0),
  reserved_bytes BIGINT NOT NULL DEFAULT 0 CHECK(reserved_bytes >= 0),
  max_single_upload_bytes BIGINT NOT NULL DEFAULT 104857600 CHECK(max_single_upload_bytes > 0),
  max_active_uploads INTEGER NOT NULL DEFAULT 5 CHECK(max_active_uploads > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK(used_bytes + reserved_bytes <= max_bytes),
  CHECK(max_single_upload_bytes <= max_bytes)
);

INSERT INTO workspace_quotas(workspace_id)
SELECT id FROM workspaces
ON CONFLICT(workspace_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS upload_sessions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL,
  artifact_id UUID NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id),
  source_name TEXT NOT NULL CHECK(length(source_name) BETWEEN 1 AND 255),
  source_type TEXT NOT NULL CHECK(source_type IN('html','zip')),
  expected_size BIGINT NOT NULL CHECK(expected_size > 0),
  expected_sha256 TEXT NOT NULL CHECK(expected_sha256 ~ '^[0-9a-f]{64}$'),
  actual_size BIGINT,
  actual_sha256 TEXT,
  object_key TEXT NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'created'
    CHECK(status IN('created','receiving','received','rejected','expired')),
  lease_until TIMESTAMPTZ,
  expires_at TIMESTAMPTZ NOT NULL,
  received_at TIMESTAMPTZ,
  failure_code TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY(workspace_id,artifact_id)
    REFERENCES artifacts(workspace_id,id) ON DELETE CASCADE,
  CHECK(actual_size IS NULL OR actual_size >= 0),
  CHECK(actual_sha256 IS NULL OR actual_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX IF NOT EXISTS idx_upload_sessions_workspace_active
  ON upload_sessions(workspace_id,status,expires_at);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_creator
  ON upload_sessions(created_by,created_at DESC);
CREATE INDEX IF NOT EXISTS idx_upload_sessions_expiry
  ON upload_sessions(expires_at)
  WHERE status IN('created','receiving');

COMMIT;
