package postgres

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/1998x-stack/visual-platform/internal/domain"
	"github.com/1998x-stack/visual-platform/internal/requestmeta"
)

var (
	ErrNotFound      = errors.New("resource not found")
	ErrDenied        = errors.New("permission denied")
	ErrConflict      = errors.New("resource conflict")
	ErrQuotaExceeded = errors.New("upload quota exceeded")
)

type Store struct{ DB *sql.DB }

func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

func (s *Store) User(ctx context.Context, id string) (domain.User, error) {
	var u domain.User
	err := s.DB.QueryRowContext(ctx, `SELECT id::text, oidc_subject, display_name, status FROM users WHERE id = $1::uuid`, id).
		Scan(&u.ID, &u.Subject, &u.DisplayName, &u.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) CreatePersonalWorkspace(ctx context.Context, userID, name string) (domain.Workspace, error) {
	var w domain.Workspace
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return w, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx,
		`INSERT INTO workspaces (kind, name, owner_ref, created_by)
		 VALUES ('personal',$1,($2::uuid)::text,$2::uuid)
		 RETURNING id::text,kind,name,owner_ref,created_by::text,status`,
		name, userID).Scan(&w.ID, &w.Kind, &w.Name, &w.OwnerRef, &w.CreatedBy, &w.Status)
	if err != nil {
		return w, writeError(err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO workspace_members (workspace_id,user_id,role)
		 VALUES ($1::uuid,$2::uuid,'owner')`, w.ID, userID)
	if err != nil {
		return w, writeError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO workspace_quotas(workspace_id) VALUES($1::uuid) ON CONFLICT DO NOTHING`, w.ID); err != nil {
		return w, err
	}
	if err = appendAudit(ctx, tx, userID, "workspace.create", "workspace", w.ID, map[string]any{"kind": "personal", "name": name}); err != nil {
		return w, err
	}
	return w, tx.Commit()
}

func (s *Store) ListWorkspaces(ctx context.Context, userID string) ([]domain.Workspace, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT w.id::text,w.kind,w.name,w.owner_ref,w.created_by::text,w.status
		 FROM workspaces w JOIN workspace_members m ON m.workspace_id=w.id
		 WHERE m.user_id=$1::uuid AND m.status='active' AND w.status='active'
		 ORDER BY w.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Workspace, 0)
	for rows.Next() {
		var w domain.Workspace
		if err := rows.Scan(&w.ID, &w.Kind, &w.Name, &w.OwnerRef, &w.CreatedBy, &w.Status); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (s *Store) Workspace(ctx context.Context, id string) (domain.Workspace, error) {
	var w domain.Workspace
	err := s.DB.QueryRowContext(ctx, `SELECT id::text,kind,name,owner_ref,created_by::text,status FROM workspaces WHERE id=$1::uuid`, id).
		Scan(&w.ID, &w.Kind, &w.Name, &w.OwnerRef, &w.CreatedBy, &w.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}
func (s *Store) Role(ctx context.Context, workspaceID, userID string) (domain.Role, error) {
	var role domain.Role
	err := s.DB.QueryRowContext(ctx,
		`SELECT m.role FROM workspace_members m JOIN workspaces w ON w.id=m.workspace_id
		 WHERE m.workspace_id=$1::uuid AND m.user_id=$2::uuid
		   AND m.status='active' AND w.status='active'`, workspaceID, userID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return role, err
}
func (s *Store) Members(ctx context.Context, workspaceID string) ([]domain.Member, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT user_id::text,role FROM workspace_members WHERE workspace_id=$1::uuid AND status='active' ORDER BY created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]domain.Member, 0)
	for rows.Next() {
		var m domain.Member
		if err := rows.Scan(&m.UserID, &m.Role); err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// AddMember checks the acting owner's membership inside the write transaction.
// FOR SHARE prevents a concurrent removal from invalidating the authorization
// between the check and commit.
func (s *Store) AddMember(ctx context.Context, workspaceID, actorID, targetID string, role domain.Role) error {
	if role != domain.Viewer && role != domain.Editor && role != domain.Maintainer {
		return ErrDenied
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = lockOwner(ctx, tx, workspaceID, actorID); err != nil {
		return err
	}
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT status='active' FROM users WHERE id=$1::uuid`, targetID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) || !active {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO workspace_members(workspace_id,user_id,role)
		 VALUES($1::uuid,$2::uuid,$3)
		 ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=EXCLUDED.role,status='active',updated_at=now()
		 WHERE workspace_members.role <> 'owner'`, workspaceID, targetID, string(role))
	if err != nil {
		return writeError(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDenied
	}
	if err = appendAudit(ctx, tx, actorID, "workspace.member.upsert", "workspace", workspaceID, map[string]any{"target_user_id": targetID, "role": role}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateArtifact(ctx context.Context, workspaceID, actorID, slug string) (domain.Artifact, error) {
	var a domain.Artifact
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	var role domain.Role
	err = tx.QueryRowContext(ctx,
		`SELECT m.role FROM workspace_members m JOIN workspaces w ON w.id=m.workspace_id
		 WHERE m.workspace_id=$1::uuid AND m.user_id=$2::uuid AND m.status='active' AND w.status='active'
		 FOR SHARE OF m,w`, workspaceID, actorID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrDenied
	}
	if err != nil {
		return a, err
	}
	if role != domain.Owner && role != domain.Maintainer && role != domain.Editor {
		return a, ErrDenied
	}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO artifacts(workspace_id,created_by,slug)
		 VALUES($1::uuid,$2::uuid,$3)
		 RETURNING id::text,workspace_id::text,created_by::text,slug,status`,
		workspaceID, actorID, slug).Scan(&a.ID, &a.WorkspaceID, &a.CreatedBy, &a.Slug, &a.Status)
	if err != nil {
		return a, writeError(err)
	}
	if err = appendAudit(ctx, tx, actorID, "artifact.create", "artifact", a.ID, map[string]any{"slug": slug}); err != nil {
		return a, err
	}
	return a, tx.Commit()
}
func (s *Store) Artifact(ctx context.Context, id string) (domain.Artifact, error) {
	var a domain.Artifact
	var version sql.NullString
	err := s.DB.QueryRowContext(ctx,
		`SELECT id::text,workspace_id::text,created_by::text,slug,status,current_version_id::text
		 FROM artifacts WHERE id=$1::uuid`, id).
		Scan(&a.ID, &a.WorkspaceID, &a.CreatedBy, &a.Slug, &a.Status, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if version.Valid {
		a.CurrentVersionID = version.String
	}
	return a, err
}
func (s *Store) ListArtifacts(ctx context.Context, workspaceID string, includeDraft bool) ([]domain.Artifact, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT id::text,workspace_id::text,created_by::text,slug,status,current_version_id::text
		 FROM artifacts WHERE workspace_id=$1::uuid AND status <> 'archived'
		 AND ($2::boolean OR status='published') ORDER BY created_at DESC`, workspaceID, includeDraft)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Artifact, 0)
	for rows.Next() {
		var a domain.Artifact
		var version sql.NullString
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.CreatedBy, &a.Slug, &a.Status, &version); err != nil {
			return nil, err
		}
		if version.Valid {
			a.CurrentVersionID = version.String
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
func (s *Store) Grants(ctx context.Context, artifactID string) ([]domain.Grant, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT subject_type,subject_id,role,expires_at FROM artifact_acl
		 WHERE artifact_id=$1::uuid`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]domain.Grant, 0)
	for rows.Next() {
		var g domain.Grant
		var expires sql.NullTime
		if err := rows.Scan(&g.SubjectType, &g.SubjectID, &g.Role, &expires); err != nil {
			return nil, err
		}
		if expires.Valid {
			g.ExpiresAt = &expires.Time
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Store) UpsertUserGrant(ctx context.Context, artifactID, actorID, targetID string, role domain.Role, expiry *time.Time) error {
	if role != domain.Viewer && role != domain.Editor && role != domain.Maintainer {
		return ErrDenied
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workspaceID, status string
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text,status FROM artifacts WHERE id=$1::uuid FOR SHARE`, artifactID).
		Scan(&workspaceID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "archived" || status == "suspended" {
		return ErrDenied
	}
	if err = lockOwner(ctx, tx, workspaceID, actorID); err != nil {
		return err
	}
	var active bool
	err = tx.QueryRowContext(ctx, `SELECT status='active' FROM users WHERE id=$1::uuid`, targetID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) || !active {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO artifact_acl(artifact_id,subject_type,subject_id,role,expires_at,created_by)
		 VALUES($1::uuid,'user',$2,$3,$4,$5::uuid)
		 ON CONFLICT(artifact_id,subject_type,subject_id)
		 DO UPDATE SET role=EXCLUDED.role,expires_at=EXCLUDED.expires_at,created_by=EXCLUDED.created_by`,
		artifactID, targetID, string(role), expiry, actorID)
	if err != nil {
		return writeError(err)
	}
	if err = appendAudit(ctx, tx, actorID, "artifact.acl.upsert", "artifact", artifactID, map[string]any{"target_user_id": targetID, "role": role, "expires_at": expiry}); err != nil {
		return err
	}
	return tx.Commit()
}

func lockOwner(ctx context.Context, tx *sql.Tx, workspaceID, actorID string) error {
	var role domain.Role
	err := tx.QueryRowContext(ctx,
		`SELECT m.role FROM workspace_members m JOIN workspaces w ON w.id=m.workspace_id
		 WHERE m.workspace_id=$1::uuid AND m.user_id=$2::uuid AND m.status='active' AND w.status='active'
		 FOR SHARE OF m,w`, workspaceID, actorID).Scan(&role)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if role != domain.Owner {
		return ErrDenied
	}
	return nil
}
func appendAudit(ctx context.Context, tx *sql.Tx, userID, action, resourceType, resourceID string, details map[string]any) error {
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode audit event: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO audit_events(actor_user_id,action,resource_type,resource_id,request_id,details)
		 VALUES($1::uuid,$2,$3,$4::uuid,$5,$6::jsonb)`,
		userID, action, resourceType, resourceID, requestmeta.ID(ctx), string(raw))
	if err != nil {
		return fmt.Errorf("write audit event: %w", err)
	}
	return nil
}

func writeError(err error) error {
	if err == nil {
		return nil
	}
	var state interface{ SQLState() string }
	if errors.As(err, &state) {
		switch state.SQLState() {
		case "23505":
			return fmt.Errorf("%w: %v", ErrConflict, err)
		case "23503":
			return fmt.Errorf("%w: %v", ErrNotFound, err)
		}
	}
	return err
}

const uploadSelectColumns = `id::text,workspace_id::text,artifact_id::text,created_by::text,
	source_name,source_type,expected_size,expected_sha256,COALESCE(actual_size,0),
	COALESCE(actual_sha256,''),object_key,status,COALESCE(failure_code,''),expires_at,
	received_at,created_at`

type uploadScanner interface{ Scan(dest ...any) error }

func scanUpload(row uploadScanner) (domain.UploadSession, error) {
	var u domain.UploadSession
	var received sql.NullTime
	err := row.Scan(&u.ID, &u.WorkspaceID, &u.ArtifactID, &u.CreatedBy,
		&u.SourceName, &u.SourceType, &u.ExpectedSize, &u.ExpectedSHA256,
		&u.ActualSize, &u.ActualSHA256, &u.ObjectKey, &u.Status, &u.FailureCode,
		&u.ExpiresAt, &received, &u.CreatedAt)
	if received.Valid {
		u.ReceivedAt = &received.Time
	}
	return u, err
}

func validUploadInput(name, kind string, size int64, checksum string) bool {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "/\\") ||
		strings.IndexFunc(name, unicode.IsControl) >= 0 || size <= 0 {
		return false
	}
	lowerName := strings.ToLower(name)
	if kind == "html" {
		if !strings.HasSuffix(lowerName, ".html") && !strings.HasSuffix(lowerName, ".htm") {
			return false
		}
	} else if kind == "zip" {
		if !strings.HasSuffix(lowerName, ".zip") {
			return false
		}
	} else {
		return false
	}
	checksum = strings.ToLower(strings.TrimSpace(checksum))
	if len(checksum) != 64 {
		return false
	}
	_, err := hex.DecodeString(checksum)
	return err == nil
}

func (s *Store) CreateUploadSession(ctx context.Context, actorID, artifactID, sourceName, sourceType string, expectedSize int64, expectedSHA256 string) (domain.UploadSession, error) {
	var result domain.UploadSession
	if !validUploadInput(sourceName, sourceType, expectedSize, expectedSHA256) {
		return result, ErrConflict
	}
	expectedSHA256 = strings.ToLower(strings.TrimSpace(expectedSHA256))
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	var workspaceID, artifactStatus, workspaceStatus string
	var role domain.Role
	// Lock order is Workspace Quota -> Artifact / Workspace / Membership -> Upload.
	// Finalization and rejection use the same order to avoid quota/session deadlocks.
	err = tx.QueryRowContext(ctx,
		`SELECT a.workspace_id::text,a.status,w.status,m.role
		 FROM artifacts a
		 JOIN workspaces w ON w.id=a.workspace_id
		 JOIN workspace_members m ON m.workspace_id=w.id
		 JOIN users u ON u.id=m.user_id
		 WHERE a.id=$1::uuid AND m.user_id=$2::uuid
		   AND m.status='active' AND u.status='active'`, artifactID, actorID).
		Scan(&workspaceID, &artifactStatus, &workspaceStatus, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrDenied
	}
	if err != nil {
		return result, err
	}
	if workspaceStatus != "active" || artifactStatus != "draft" ||
		(role != domain.Owner && role != domain.Maintainer && role != domain.Editor) {
		return result, ErrDenied
	}
	if _, err = tx.ExecContext(ctx,
		`INSERT INTO workspace_quotas(workspace_id) VALUES($1::uuid) ON CONFLICT DO NOTHING`, workspaceID); err != nil {
		return result, err
	}
	var quota domain.WorkspaceQuota
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text,max_bytes,used_bytes,reserved_bytes,
		 max_single_upload_bytes,max_active_uploads
		 FROM workspace_quotas WHERE workspace_id=$1::uuid FOR UPDATE`, workspaceID).
		Scan(&quota.WorkspaceID, &quota.MaxBytes, &quota.UsedBytes, &quota.ReservedBytes,
			&quota.MaxSingleUploadBytes, &quota.MaxActiveUploads)
	if err != nil {
		return result, err
	}

	err = tx.QueryRowContext(ctx,
		`SELECT a.status,w.status,m.role
		 FROM artifacts a
		 JOIN workspaces w ON w.id=a.workspace_id
		 JOIN workspace_members m ON m.workspace_id=w.id
		 JOIN users u ON u.id=m.user_id
		 WHERE a.id=$1::uuid AND a.workspace_id=$2::uuid
		   AND m.user_id=$3::uuid AND m.status='active' AND u.status='active'
		 FOR SHARE OF a,w,m,u`, artifactID, workspaceID, actorID).
		Scan(&artifactStatus, &workspaceStatus, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrDenied
	}
	if err != nil {
		return result, err
	}
	if workspaceStatus != "active" || artifactStatus != "draft" ||
		(role != domain.Owner && role != domain.Maintainer && role != domain.Editor) {
		return result, ErrDenied
	}

	expiredRows, err := tx.QueryContext(ctx,
		`UPDATE upload_sessions SET status='expired',failure_code='session_expired',
		 lease_until=NULL,updated_at=now()
		 WHERE workspace_id=$1::uuid AND status IN('created','receiving') AND expires_at<=now()
		 RETURNING expected_size`, workspaceID)
	if err != nil {
		return result, err
	}
	var expiredBytes int64
	for expiredRows.Next() {
		var n int64
		if err := expiredRows.Scan(&n); err != nil {
			expiredRows.Close()
			return result, err
		}
		expiredBytes += n
	}
	if err := expiredRows.Err(); err != nil {
		expiredRows.Close()
		return result, err
	}
	expiredRows.Close()
	if expiredBytes > 0 {
		res, err := tx.ExecContext(ctx,
			`UPDATE workspace_quotas SET reserved_bytes=reserved_bytes-$2,updated_at=now()
			 WHERE workspace_id=$1::uuid AND reserved_bytes >= $2`, workspaceID, expiredBytes)
		if err != nil {
			return result, err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return result, ErrConflict
		}
		quota.ReservedBytes -= expiredBytes
	}
	// A crashed receiver may be retried only after its short lease expires.
	if _, err = tx.ExecContext(ctx,
		`UPDATE upload_sessions SET status='created',lease_until=NULL,updated_at=now()
		 WHERE workspace_id=$1::uuid AND status='receiving'
		   AND lease_until<=now() AND expires_at>now()`, workspaceID); err != nil {
		return result, err
	}

	if expectedSize > quota.MaxSingleUploadBytes || quota.UsedBytes+quota.ReservedBytes+expectedSize > quota.MaxBytes {
		return result, ErrQuotaExceeded
	}
	var active int
	if err = tx.QueryRowContext(ctx,
		`SELECT count(*) FROM upload_sessions
		 WHERE workspace_id=$1::uuid AND status IN('created','receiving') AND expires_at>now()`,
		workspaceID).Scan(&active); err != nil {
		return result, err
	}
	if active >= quota.MaxActiveUploads {
		return result, ErrQuotaExceeded
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE workspace_quotas SET reserved_bytes=reserved_bytes+$2,updated_at=now()
		 WHERE workspace_id=$1::uuid`, workspaceID, expectedSize); err != nil {
		return result, err
	}

	row := tx.QueryRowContext(ctx,
		`WITH new_id AS (SELECT gen_random_uuid() AS id)
		 INSERT INTO upload_sessions(id,workspace_id,artifact_id,created_by,source_name,source_type,
		   expected_size,expected_sha256,object_key,expires_at)
		 SELECT new_id.id,$1::uuid,$2::uuid,$3::uuid,$4,$5,$6,$7,
		   'quarantine/'||($1::uuid)::text||'/'||($2::uuid)::text||'/'||new_id.id::text||'/source',
		   now()+interval '30 minutes'
		 FROM new_id
		 RETURNING `+uploadSelectColumns,
		workspaceID, artifactID, actorID, strings.TrimSpace(sourceName), sourceType, expectedSize, expectedSHA256)
	result, err = scanUpload(row)
	if err != nil {
		return result, writeError(err)
	}
	if err = appendAudit(ctx, tx, actorID, "upload.session.create", "upload", result.ID,
		map[string]any{"artifact_id": artifactID, "source_type": sourceType, "expected_size": expectedSize}); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Store) GetUploadSession(ctx context.Context, id, actorID string) (domain.UploadSession, error) {
	row := s.DB.QueryRowContext(ctx,
		`SELECT us.id::text,us.workspace_id::text,us.artifact_id::text,us.created_by::text,
		 us.source_name,us.source_type,us.expected_size,us.expected_sha256,
		 COALESCE(us.actual_size,0),COALESCE(us.actual_sha256,''),us.object_key,
		 us.status,COALESCE(us.failure_code,''),us.expires_at,us.received_at,us.created_at
		 FROM upload_sessions us
		 JOIN workspace_members m ON m.workspace_id=us.workspace_id AND m.user_id=$2::uuid
		 JOIN users u ON u.id=m.user_id
		 WHERE us.id=$1::uuid AND m.status='active' AND u.status='active'`, id, actorID)
	u, err := scanUpload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) ClaimUpload(ctx context.Context, id, actorID string) (domain.UploadSession, error) {
	var result domain.UploadSession
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var workspaceID string
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text FROM upload_sessions
		 WHERE id=$1::uuid AND created_by=$2::uuid`, id, actorID).Scan(&workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	var quotaID string
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text FROM workspace_quotas
		 WHERE workspace_id=$1::uuid FOR UPDATE`, workspaceID).Scan(&quotaID)
	if err != nil {
		return result, err
	}
	var role domain.Role
	var sessionStatus string
	var leaseUntil sql.NullTime
	var expiresAt time.Time
	var artifactStatus, workspaceStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT us.status,us.lease_until,us.expires_at,a.status,w.status,m.role
		 FROM upload_sessions us
		 JOIN artifacts a ON a.id=us.artifact_id AND a.workspace_id=us.workspace_id
		 JOIN workspaces w ON w.id=us.workspace_id
		 JOIN workspace_members m ON m.workspace_id=us.workspace_id AND m.user_id=us.created_by
		 JOIN users u ON u.id=us.created_by
		 WHERE us.id=$1::uuid AND us.created_by=$2::uuid
		   AND u.status='active' AND m.status='active'
		 FOR UPDATE OF us,a,w,m,u`, id, actorID).
		Scan(&sessionStatus, &leaseUntil, &expiresAt, &artifactStatus, &workspaceStatus, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if role != domain.Owner && role != domain.Maintainer && role != domain.Editor {
		return result, ErrNotFound
	}
	if artifactStatus != "draft" || workspaceStatus != "active" {
		return result, ErrNotFound
	}
	if expiresAt.Before(time.Now()) || (sessionStatus != "created" && !(sessionStatus == "receiving" && leaseUntil.Valid && !leaseUntil.Time.After(time.Now()))) {
		return result, ErrNotFound
	}
	row := tx.QueryRowContext(ctx,
		`UPDATE upload_sessions SET status='receiving',
		 lease_until=now()+interval '10 minutes',updated_at=now()
		 WHERE id=$1::uuid AND created_by=$2::uuid AND expires_at>now()
		   AND (status='created' OR (status='receiving' AND lease_until<=now()))
		 RETURNING `+uploadSelectColumns, id, actorID)
	result, err = scanUpload(row)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	if err = appendAudit(ctx, tx, actorID, "upload.claim", "upload", id, map[string]any{"lease_seconds": 600}); err != nil {
		return result, err
	}
	return result, tx.Commit()
}

func (s *Store) ResetUploadClaim(ctx context.Context, id, actorID string) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE upload_sessions SET status='created',lease_until=NULL,updated_at=now()
		 WHERE id=$1::uuid AND created_by=$2::uuid AND status='receiving' AND expires_at>now()`,
		id, actorID)
	return err
}

func (s *Store) MarkUploadReceived(ctx context.Context, id, actorID string, actualSize int64, actualSHA256 string) error {
	if actualSize <= 0 || len(actualSHA256) != 64 {
		return ErrConflict
	}
	actualSHA256 = strings.ToLower(actualSHA256)
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workspaceID string
	var expectedSize int64
	var expectedSHA256 string
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text,expected_size,expected_sha256 FROM upload_sessions
		 WHERE id=$1::uuid AND created_by=$2::uuid`, id, actorID).Scan(&workspaceID, &expectedSize, &expectedSHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var quotaID string
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text FROM workspace_quotas WHERE workspace_id=$1::uuid FOR UPDATE`,
		workspaceID).Scan(&quotaID)
	if err != nil {
		return err
	}
	var sessionID, artifactID string
	err = tx.QueryRowContext(ctx,
		`SELECT us.id::text,us.artifact_id::text
		 FROM upload_sessions us
		 JOIN artifacts a ON a.id=us.artifact_id AND a.workspace_id=us.workspace_id
		 JOIN workspaces w ON w.id=us.workspace_id
		 JOIN workspace_members m ON m.workspace_id=us.workspace_id AND m.user_id=us.created_by
		 JOIN users u ON u.id=us.created_by
		 WHERE us.id=$1::uuid AND us.created_by=$2::uuid
		   AND us.status='receiving' AND us.lease_until>now() AND us.expires_at>now()
		   AND a.status='draft' AND w.status='active'
		   AND m.status='active' AND m.role IN('owner','maintainer','editor')
		   AND u.status='active'
		 FOR UPDATE OF us,a,w,m,u`, id, actorID).Scan(&sessionID, &artifactID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if actualSize != expectedSize || actualSHA256 != expectedSHA256 {
		return ErrConflict
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE upload_sessions SET status='received',actual_size=$2,actual_sha256=$3,
		 lease_until=NULL,received_at=now(),updated_at=now()
		 WHERE id=$1::uuid AND status='receiving'`, id, actualSize, actualSHA256)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE workspace_quotas SET reserved_bytes=reserved_bytes-$2,
		 used_bytes=used_bytes+$2,updated_at=now()
		 WHERE workspace_id=$1::uuid AND reserved_bytes >= $2`, workspaceID, expectedSize)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	if err = appendAudit(ctx, tx, actorID, "upload.received", "upload", sessionID,
		map[string]any{"artifact_id": artifactID, "size": actualSize, "sha256": actualSHA256}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) MarkUploadRejected(ctx context.Context, id, actorID, reason string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var workspaceID string
	var expectedSize int64
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text,expected_size FROM upload_sessions
		 WHERE id=$1::uuid AND created_by=$2::uuid`, id, actorID).Scan(&workspaceID, &expectedSize)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var quotaID string
	err = tx.QueryRowContext(ctx,
		`SELECT workspace_id::text FROM workspace_quotas WHERE workspace_id=$1::uuid FOR UPDATE`,
		workspaceID).Scan(&quotaID)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE upload_sessions SET status='rejected',failure_code=$3,lease_until=NULL,updated_at=now()
		 WHERE id=$1::uuid AND created_by=$2::uuid AND status='receiving'`, id, actorID, reason)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	res, err = tx.ExecContext(ctx,
		`UPDATE workspace_quotas SET reserved_bytes=reserved_bytes-$2,updated_at=now()
		 WHERE workspace_id=$1::uuid AND reserved_bytes >= $2`, workspaceID, expectedSize)
	if err != nil {
		return err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	if err = appendAudit(ctx, tx, actorID, "upload.rejected", "upload", id, map[string]any{"failure_code": reason}); err != nil {
		return err
	}
	return tx.Commit()
}
