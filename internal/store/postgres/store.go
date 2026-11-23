package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrDenied   = errors.New("permission denied")
	ErrConflict = errors.New("resource conflict")
)

type Store struct { DB *sql.DB }

func (s *Store) Ping(ctx context.Context) error { return s.DB.PingContext(ctx) }

func (s *Store) User(ctx context.Context, id string) (domain.User, error) {
	var u domain.User
	err := s.DB.QueryRowContext(ctx, `SELECT id::text, oidc_subject, display_name, status FROM users WHERE id = $1::uuid`, id).
		Scan(&u.ID, &u.Subject, &u.DisplayName, &u.Status)
	if errors.Is(err, sql.ErrNoRows) { return u, ErrNotFound }
	return u, err
}

func (s *Store) CreatePersonalWorkspace(ctx context.Context, userID, name string) (domain.Workspace, error) {
	var w domain.Workspace
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil { return w, err }
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx,
		`INSERT INTO workspaces (kind, name, owner_ref, created_by)
		 VALUES ('personal',$1,($2::uuid)::text,$2::uuid)
		 RETURNING id::text,kind,name,owner_ref,created_by::text,status`,
		name, userID).Scan(&w.ID,&w.Kind,&w.Name,&w.OwnerRef,&w.CreatedBy,&w.Status)
	if err != nil { return w, writeError(err) }
	_, err = tx.ExecContext(ctx,
		`INSERT INTO workspace_members (workspace_id,user_id,role)
		 VALUES ($1::uuid,$2::uuid,'owner')`,w.ID,userID)
	if err != nil { return w, writeError(err) }
	if err = appendAudit(ctx, tx, userID, "workspace.create", "workspace", w.ID); err != nil { return w, err }
	return w, tx.Commit()
}

func (s *Store) ListWorkspaces(ctx context.Context, userID string) ([]domain.Workspace, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT w.id::text,w.kind,w.name,w.owner_ref,w.created_by::text,w.status
		 FROM workspaces w JOIN workspace_members m ON m.workspace_id=w.id
		 WHERE m.user_id=$1::uuid AND m.status='active' AND w.status='active'
		 ORDER BY w.created_at DESC`,userID)
	if err != nil { return nil, err }
	defer rows.Close()
	out := make([]domain.Workspace,0)
	for rows.Next() {
		var w domain.Workspace
		if err := rows.Scan(&w.ID,&w.Kind,&w.Name,&w.OwnerRef,&w.CreatedBy,&w.Status); err != nil { return nil, err }
		out=append(out,w)
	}
	return out,rows.Err()
}

func (s *Store) Workspace(ctx context.Context,id string) (domain.Workspace,error) {
	var w domain.Workspace
	err:=s.DB.QueryRowContext(ctx,`SELECT id::text,kind,name,owner_ref,created_by::text,status FROM workspaces WHERE id=$1::uuid`,id).
		Scan(&w.ID,&w.Kind,&w.Name,&w.OwnerRef,&w.CreatedBy,&w.Status)
	if errors.Is(err,sql.ErrNoRows) { return w,ErrNotFound }
	return w,err
}
func (s *Store) Role(ctx context.Context, workspaceID,userID string) (domain.Role,error) {
	var role domain.Role
	err:=s.DB.QueryRowContext(ctx,
		`SELECT m.role FROM workspace_members m JOIN workspaces w ON w.id=m.workspace_id
		 WHERE m.workspace_id=$1::uuid AND m.user_id=$2::uuid
		   AND m.status='active' AND w.status='active'`,workspaceID,userID).Scan(&role)
	if errors.Is(err,sql.ErrNoRows) { return "",nil }
	return role,err
}
func (s *Store) Members(ctx context.Context, workspaceID string) ([]domain.Member,error) {
	rows,err:=s.DB.QueryContext(ctx,
		`SELECT user_id::text,role FROM workspace_members WHERE workspace_id=$1::uuid AND status='active' ORDER BY created_at`,workspaceID)
	if err!=nil { return nil,err }
	defer rows.Close()
	result:=make([]domain.Member,0)
	for rows.Next() {
		var m domain.Member
		if err:=rows.Scan(&m.UserID,&m.Role);err!=nil {return nil,err}
		result=append(result,m)
	}
	return result,rows.Err()
}

// AddMember checks the acting owner's membership inside the write transaction.
// FOR SHARE prevents a concurrent removal from invalidating the authorization
// between the check and commit.
func (s *Store) AddMember(ctx context.Context,workspaceID,actorID,targetID string,role domain.Role) error {
	if role!=domain.Viewer && role!=domain.Editor && role!=domain.Maintainer {return ErrDenied}
	tx,err:=s.DB.BeginTx(ctx,nil)
	if err!=nil{return err}
	defer tx.Rollback()
	if err=lockOwner(ctx,tx,workspaceID,actorID);err!=nil{return err}
	var active bool
	err=tx.QueryRowContext(ctx,`SELECT status='active' FROM users WHERE id=$1::uuid`,targetID).Scan(&active)
	if errors.Is(err,sql.ErrNoRows)||!active {return ErrNotFound}
	if err!=nil{return err}
	res,err:=tx.ExecContext(ctx,
		`INSERT INTO workspace_members(workspace_id,user_id,role)
		 VALUES($1::uuid,$2::uuid,$3)
		 ON CONFLICT(workspace_id,user_id) DO UPDATE SET role=EXCLUDED.role,status='active',updated_at=now()
		 WHERE workspace_members.role <> 'owner'`,workspaceID,targetID,string(role))
	if err!=nil{return writeError(err)}
	n,err:=res.RowsAffected();if err!=nil{return err};if n!=1{return ErrDenied}
	if err=appendAudit(ctx,tx,actorID,"workspace.member.upsert","workspace",workspaceID);err!=nil{return err}
	return tx.Commit()
}

func (s *Store) CreateArtifact(ctx context.Context,workspaceID,actorID,slug string) (domain.Artifact,error) {
	var a domain.Artifact
	tx,err:=s.DB.BeginTx(ctx,nil)
	if err!=nil{return a,err}
	defer tx.Rollback()
	var role domain.Role
	err=tx.QueryRowContext(ctx,
		`SELECT m.role FROM workspace_members m JOIN workspaces w ON w.id=m.workspace_id
		 WHERE m.workspace_id=$1::uuid AND m.user_id=$2::uuid AND m.status='active' AND w.status='active'
		 FOR SHARE OF m,w`,workspaceID,actorID).Scan(&role)
	if errors.Is(err,sql.ErrNoRows) {return a,ErrDenied}
	if err!=nil{return a,err}
	if role!=domain.Owner&&role!=domain.Maintainer&&role!=domain.Editor {return a,ErrDenied}
	err=tx.QueryRowContext(ctx,
		`INSERT INTO artifacts(workspace_id,created_by,slug)
		 VALUES($1::uuid,$2::uuid,$3)
		 RETURNING id::text,workspace_id::text,created_by::text,slug,status`,
		workspaceID,actorID,slug).Scan(&a.ID,&a.WorkspaceID,&a.CreatedBy,&a.Slug,&a.Status)
	if err!=nil{return a,writeError(err)}
	if err=appendAudit(ctx,tx,actorID,"artifact.create","artifact",a.ID);err!=nil{return a,err}
	return a,tx.Commit()
}
func (s *Store) Artifact(ctx context.Context,id string) (domain.Artifact,error) {
	var a domain.Artifact
	var version sql.NullString
	err:=s.DB.QueryRowContext(ctx,
		`SELECT id::text,workspace_id::text,created_by::text,slug,status,current_version_id::text
		 FROM artifacts WHERE id=$1::uuid`,id).
		Scan(&a.ID,&a.WorkspaceID,&a.CreatedBy,&a.Slug,&a.Status,&version)
	if errors.Is(err,sql.ErrNoRows) {return a,ErrNotFound}
	if version.Valid {a.CurrentVersionID=version.String}
	return a,err
}
func (s *Store) ListArtifacts(ctx context.Context,workspaceID string, includeDraft bool) ([]domain.Artifact,error) {
	rows,err:=s.DB.QueryContext(ctx,
		`SELECT id::text,workspace_id::text,created_by::text,slug,status,current_version_id::text
		 FROM artifacts WHERE workspace_id=$1::uuid AND status <> 'archived'
		 AND ($2::boolean OR status='published') ORDER BY created_at DESC`,workspaceID,includeDraft)
	if err!=nil{return nil,err}
	defer rows.Close()
	out:=make([]domain.Artifact,0)
	for rows.Next() {
		var a domain.Artifact
		var version sql.NullString
		if err:=rows.Scan(&a.ID,&a.WorkspaceID,&a.CreatedBy,&a.Slug,&a.Status,&version);err!=nil{return nil,err}
		if version.Valid {a.CurrentVersionID=version.String}
		out=append(out,a)
	}
	return out,rows.Err()
}
func (s *Store) Grants(ctx context.Context,artifactID string) ([]domain.Grant,error) {
	rows,err:=s.DB.QueryContext(ctx,
		`SELECT subject_type,subject_id,role,expires_at FROM artifact_acl
		 WHERE artifact_id=$1::uuid`,artifactID)
	if err!=nil{return nil,err}
	defer rows.Close()
	out:=make([]domain.Grant,0)
	for rows.Next() {
		var g domain.Grant
		var expires sql.NullTime
		if err:=rows.Scan(&g.SubjectType,&g.SubjectID,&g.Role,&expires);err!=nil{return nil,err}
		if expires.Valid {g.ExpiresAt=&expires.Time}
		out=append(out,g)
	}
	return out,rows.Err()
}

func (s *Store) UpsertUserGrant(ctx context.Context,artifactID,actorID,targetID string,role domain.Role,expiry *time.Time) error {
	if role!=domain.Viewer && role!=domain.Editor && role!=domain.Maintainer {return ErrDenied}
	tx,err:=s.DB.BeginTx(ctx,nil)
	if err!=nil{return err}
	defer tx.Rollback()
	var workspaceID,status string
	err=tx.QueryRowContext(ctx,
		`SELECT workspace_id::text,status FROM artifacts WHERE id=$1::uuid FOR SHARE`,artifactID).
		Scan(&workspaceID,&status)
	if errors.Is(err,sql.ErrNoRows) {return ErrNotFound}
	if err!=nil{return err}
	if status=="archived"||status=="suspended"{return ErrDenied}
	if err=lockOwner(ctx,tx,workspaceID,actorID);err!=nil{return err}
	var active bool
	err=tx.QueryRowContext(ctx,`SELECT status='active' FROM users WHERE id=$1::uuid`,targetID).Scan(&active)
	if errors.Is(err,sql.ErrNoRows)||!active {return ErrNotFound}
	if err!=nil{return err}
	_,err=tx.ExecContext(ctx,
		`INSERT INTO artifact_acl(artifact_id,subject_type,subject_id,role,expires_at,created_by)
		 VALUES($1::uuid,'user',$2,$3,$4,$5::uuid)
		 ON CONFLICT(artifact_id,subject_type,subject_id)
		 DO UPDATE SET role=EXCLUDED.role,expires_at=EXCLUDED.expires_at,created_by=EXCLUDED.created_by`,
		artifactID,targetID,string(role),expiry,actorID)
	if err!=nil{return writeError(err)}
	if err=appendAudit(ctx,tx,actorID,"artifact.acl.upsert","artifact",artifactID);err!=nil{return err}
	return tx.Commit()
}

func lockOwner(ctx context.Context,tx *sql.Tx,workspaceID,actorID string) error {
	var role domain.Role
	err:=tx.QueryRowContext(ctx,
		`SELECT m.role FROM workspace_members m JOIN workspaces w ON w.id=m.workspace_id
		 WHERE m.workspace_id=$1::uuid AND m.user_id=$2::uuid AND m.status='active' AND w.status='active'
		 FOR SHARE OF m,w`,workspaceID,actorID).Scan(&role)
	if errors.Is(err,sql.ErrNoRows){return ErrDenied}
	if err!=nil{return err}
	if role!=domain.Owner{return ErrDenied}
	return nil
}
func appendAudit(ctx context.Context,tx *sql.Tx,userID,action,resourceType,resourceID string) error {
	_,err:=tx.ExecContext(ctx,
		`INSERT INTO audit_events(actor_user_id,action,resource_type,resource_id)
		 VALUES($1::uuid,$2,$3,$4::uuid)`,userID,action,resourceType,resourceID)
	if err!=nil{return fmt.Errorf("write audit event: %w",err)}
	return nil
}

func writeError(err error) error {
	if err==nil{return nil}
	var state interface{SQLState() string}
	if errors.As(err,&state) {
		switch state.SQLState(){
		case "23505":return fmt.Errorf("%w: %v",ErrConflict,err)
		case "23503":return fmt.Errorf("%w: %v",ErrNotFound,err)
		}
	}
	return err
}
