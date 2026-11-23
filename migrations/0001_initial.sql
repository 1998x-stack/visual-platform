-- Visual Platform M0/M1 schema. Apply with psql -v ON_ERROR_STOP=1 -f.
-- App-level authorization is mandatory. RLS is deliberately deferred until
-- a restricted DB role and a transaction-scoped actor context are designed.
BEGIN;

CREATE TABLE IF NOT EXISTS users (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  oidc_subject TEXT NOT NULL UNIQUE,
  display_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN('active','disabled')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS workspaces (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  kind TEXT NOT NULL CHECK(kind IN('personal','department','project','company')),
  name TEXT NOT NULL CHECK(length(btrim(name)) BETWEEN 1 AND 120),
  owner_ref TEXT NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id),
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN('active','suspended','archived')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(kind, owner_ref)
);

CREATE TABLE IF NOT EXISTS workspace_members (
  workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id),
  role TEXT NOT NULL CHECK(role IN('owner','maintainer','editor','viewer')),
  status TEXT NOT NULL DEFAULT 'active' CHECK(status IN('active','revoked')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(workspace_id,user_id)
);
CREATE INDEX IF NOT EXISTS idx_workspace_members_user ON workspace_members(user_id,workspace_id) WHERE status='active';

CREATE TABLE IF NOT EXISTS artifacts (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workspace_id UUID NOT NULL REFERENCES workspaces(id),
  created_by UUID NOT NULL REFERENCES users(id),
  slug TEXT NOT NULL CHECK(slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND length(slug)<=63),
  status TEXT NOT NULL DEFAULT 'draft' CHECK(status IN('draft','ready','published','suspended','archived')),
  current_version_id UUID,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(workspace_id,slug)
);

CREATE TABLE IF NOT EXISTS artifact_versions (
  id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  artifact_id UUID NOT NULL REFERENCES artifacts(id),
  version_no BIGINT NOT NULL CHECK(version_no>0),
  object_prefix TEXT NOT NULL UNIQUE,
  manifest_sha256 TEXT NOT NULL CHECK(length(manifest_sha256)=64),
  status TEXT NOT NULL CHECK(status IN('staging','ready','failed')),
  created_by UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE(artifact_id,version_no),
  UNIQUE(artifact_id,id)
);
ALTER TABLE artifacts ADD CONSTRAINT current_version_same_artifact
  FOREIGN KEY (id,current_version_id)
  REFERENCES artifact_versions(artifact_id,id) DEFERRABLE INITIALLY IMMEDIATE;

CREATE TABLE IF NOT EXISTS artifact_acl (
  artifact_id UUID NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  subject_type TEXT NOT NULL CHECK(subject_type IN('user','group')),
  subject_id TEXT NOT NULL,
  role TEXT NOT NULL CHECK(role IN('viewer','editor','maintainer')),
  expires_at TIMESTAMPTZ,
  created_by UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY(artifact_id,subject_type,subject_id)
);
CREATE INDEX IF NOT EXISTS idx_artifact_acl_subject ON artifact_acl(subject_type,subject_id);

CREATE TABLE IF NOT EXISTS audit_events (
  id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  actor_user_id UUID REFERENCES users(id),
  action TEXT NOT NULL,
  resource_type TEXT NOT NULL,
  resource_id UUID NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_audit_events_resource ON audit_events(resource_type,resource_id,created_at DESC);

COMMIT;
