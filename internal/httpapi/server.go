// Package httpapi exposes the M0/M1 HTTP surface. No production auth is
// implemented: absent an enterprise IdP, all business endpoints fail closed.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
	"github.com/1998x-stack/visual-platform/internal/permission"
	"github.com/1998x-stack/visual-platform/internal/requestmeta"
	"github.com/1998x-stack/visual-platform/internal/store/postgres"
)

type Repository interface {
	Ping(ctx context.Context) error
	User(ctx context.Context, id string) (domain.User, error)
	CreatePersonalWorkspace(ctx context.Context, userID, name string) (domain.Workspace, error)
	ListWorkspaces(ctx context.Context, userID string) ([]domain.Workspace, error)
	Workspace(ctx context.Context, id string) (domain.Workspace, error)
	Role(ctx context.Context, workspaceID, userID string) (domain.Role, error)
	Members(ctx context.Context, workspaceID string) ([]domain.Member, error)
	AddMember(ctx context.Context, workspaceID, actorID, targetID string, role domain.Role) error
	CreateArtifact(ctx context.Context, workspaceID, actorID, slug string) (domain.Artifact, error)
	Artifact(ctx context.Context, id string) (domain.Artifact, error)
	ListArtifacts(ctx context.Context, workspaceID string, includeDraft bool) ([]domain.Artifact, error)
	Grants(ctx context.Context, artifactID string) ([]domain.Grant, error)
	UpsertUserGrant(ctx context.Context, artifactID, actorID, targetID string, role domain.Role, expiry *time.Time) error
}

type Server struct {
	Repo    Repository
	Logger  *slog.Logger
	DevAuth bool
}

var uuid = regexp.MustCompile(`^[[:xdigit:]]{8}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{4}-[[:xdigit:]]{12}$`)
var slug = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("GET /api/v1/me", s.me)
	mux.HandleFunc("GET /api/v1/workspaces", s.workspaces)
	mux.HandleFunc("POST /api/v1/workspaces", s.workspaces)
	mux.HandleFunc("GET /api/v1/workspaces/{id}", s.workspace)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/members", s.members)
	mux.HandleFunc("POST /api/v1/workspaces/{id}/members", s.members)
	mux.HandleFunc("GET /api/v1/workspaces/{id}/artifacts", s.workspaceArtifacts)
	mux.HandleFunc("POST /api/v1/artifacts", s.createArtifact)
	mux.HandleFunc("GET /api/v1/artifacts/{id}", s.artifact)
	mux.HandleFunc("GET /api/v1/artifacts/{id}/acl", s.acl)
	mux.HandleFunc("PUT /api/v1/artifacts/{id}/acl", s.acl)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var buf [16]byte
		if _, err := rand.Read(buf[:]); err != nil {
			http.Error(w, "request ID unavailable", http.StatusInternalServerError)
			return
		}
		id := hex.EncodeToString(buf[:])
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r.WithContext(requestmeta.WithID(r.Context(), id)))
	})
}
func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "component": "api"})
}
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Repo.Ping(ctx); err != nil {
		s.logError("ready check", err)
		writeProblem(w, r, http.StatusServiceUnavailable, "NOT_READY")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (domain.Principal, bool) {
	var p domain.Principal
	if !s.DevAuth {
		writeProblem(w, r, http.StatusServiceUnavailable, "IDENTITY_PROVIDER_NOT_CONFIGURED")
		return p, false
	}
	// Only direct loopback calls are allowed in insecure development auth.
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(ip) == nil || !net.ParseIP(ip).IsLoopback() ||
		r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		writeProblem(w, r, http.StatusForbidden, "DEVELOPMENT_AUTH_LOCAL_ONLY")
		return p, false
	}
	id := r.Header.Get("X-Dev-User-ID")
	if !uuid.MatchString(id) {
		writeProblem(w, r, http.StatusUnauthorized, "AUTH_REQUIRED")
		return p, false
	}
	u, err := s.Repo.User(r.Context(), id)
	if err != nil || u.Status != "active" {
		writeProblem(w, r, http.StatusUnauthorized, "AUTH_REQUIRED")
		return p, false
	}
	return domain.Principal{UserID: u.ID, Active: true}, true
}
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	u, err := s.Repo.User(r.Context(), p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, u)
}
func (s *Server) workspaces(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	if r.Method == "GET" {
		items, err := s.Repo.ListWorkspaces(r.Context(), p.UserID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	var req struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	// Nonpersonal workspaces require a separately controlled provisioning flow.
	if req.Kind != "personal" {
		writeProblem(w, r, http.StatusForbidden, "PROVISIONING_REQUIRED")
		return
	}
	if len(req.Name) < 1 || len(req.Name) > 120 {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_WORKSPACE_NAME")
		return
	}
	item, err := s.Repo.CreatePersonalWorkspace(r.Context(), p.UserID, req.Name)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, item)
}
func (s *Server) authorizedWorkspace(w http.ResponseWriter, r *http.Request, p domain.Principal, id string, action domain.Action) (domain.Workspace, domain.Role, bool) {
	var wspace domain.Workspace
	if !uuid.MatchString(id) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
		return wspace, "", false
	}
	var err error
	wspace, err = s.Repo.Workspace(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return wspace, "", false
	}
	role, err := s.Repo.Role(r.Context(), id, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return wspace, "", false
	}
	d := permission.Decide(p, action, wspace.Status, "", role, nil, time.Now())
	if !d.Allowed {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
		return wspace, role, false
	}
	return wspace, role, true
}
func (s *Server) workspace(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	item, _, allowed := s.authorizedWorkspace(w, r, p, r.PathValue("id"), domain.WorkspaceRead)
	if !allowed {
		return
	}
	writeJSON(w, http.StatusOK, item)
}
func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	_, _, allowed := s.authorizedWorkspace(w, r, p, id, domain.WorkspaceManageMembers)
	if !allowed {
		return
	}
	if r.Method == "GET" {
		items, err := s.Repo.Members(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	var req struct {
		UserID string      `json:"user_id"`
		Role   domain.Role `json:"role"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !uuid.MatchString(req.UserID) || !isGrantableRole(req.Role) {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_MEMBER")
		return
	}
	if err := s.Repo.AddMember(r.Context(), id, p.UserID, req.UserID, req.Role); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
func (s *Server) workspaceArtifacts(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	_, role, allowed := s.authorizedWorkspace(w, r, p, id, domain.WorkspaceRead)
	if !allowed {
		return
	}
	items, err := s.Repo.ListArtifacts(r.Context(), id, role == domain.Owner || role == domain.Maintainer || role == domain.Editor)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
func (s *Server) createArtifact(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	var req struct {
		WorkspaceID string `json:"workspace_id"`
		Slug        string `json:"slug"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !uuid.MatchString(req.WorkspaceID) || len(req.Slug) > 63 || !slug.MatchString(req.Slug) {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_ARTIFACT")
		return
	}
	_, _, allowed := s.authorizedWorkspace(w, r, p, req.WorkspaceID, domain.ArtifactCreate)
	if !allowed {
		return
	}
	a, err := s.Repo.CreateArtifact(r.Context(), req.WorkspaceID, p.UserID, req.Slug)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, a)
}
func (s *Server) authorizedArtifact(w http.ResponseWriter, r *http.Request, p domain.Principal, id string, action domain.Action) (domain.Artifact, bool) {
	var a domain.Artifact
	if !uuid.MatchString(id) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
		return a, false
	}
	var err error
	a, err = s.Repo.Artifact(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return a, false
	}
	wspace, err := s.Repo.Workspace(r.Context(), a.WorkspaceID)
	if err != nil {
		s.fail(w, r, err)
		return a, false
	}
	role, err := s.Repo.Role(r.Context(), a.WorkspaceID, p.UserID)
	if err != nil {
		s.fail(w, r, err)
		return a, false
	}
	if action == domain.ArtifactRead && a.Status != "published" {
		// Draft metadata is available only to workspace editors and above;
		// artifact.read for actual content still rejects all unpublished work.
		if wspace.Status != "active" || (role != domain.Owner && role != domain.Maintainer && role != domain.Editor) {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
			return a, false
		}
		if a.Status == "suspended" || a.Status == "archived" {
			writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
			return a, false
		}
		return a, true
	}
	grants := []domain.Grant(nil)
	if action == domain.ArtifactRead {
		grants, err = s.Repo.Grants(r.Context(), a.ID)
		if err != nil {
			s.fail(w, r, err)
			return a, false
		}
	}
	decision := permission.Decide(p, action, wspace.Status, a.Status, role, grants, time.Now())
	if !decision.Allowed {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
		return a, false
	}
	return a, true
}
func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	a, allowed := s.authorizedArtifact(w, r, p, r.PathValue("id"), domain.ArtifactRead)
	if !allowed {
		return
	}
	writeJSON(w, http.StatusOK, a)
}
func (s *Server) acl(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	_, allowed := s.authorizedArtifact(w, r, p, id, domain.ArtifactManageACL)
	if !allowed {
		return
	}
	if r.Method == "GET" {
		items, err := s.Repo.Grants(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	var req struct {
		UserID    string      `json:"user_id"`
		Role      domain.Role `json:"role"`
		ExpiresAt *time.Time  `json:"expires_at"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !uuid.MatchString(req.UserID) || !isGrantableRole(req.Role) ||
		(req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now())) {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_GRANT")
		return
	}
	if err := s.Repo.UpsertUserGrant(r.Context(), id, p.UserID, req.UserID, req.Role, req.ExpiresAt); err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
func isGrantableRole(role domain.Role) bool {
	return role == domain.Maintainer || role == domain.Editor || role == domain.Viewer
}
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	kind, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || kind != "application/json" {
		writeProblem(w, r, http.StatusUnsupportedMediaType, "JSON_REQUIRED")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_JSON")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest, "INVALID_JSON")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code string) {
	id := requestmeta.ID(r.Context())
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "request_id": id}})
}
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, postgres.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
	case errors.Is(err, postgres.ErrDenied):
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND")
	case errors.Is(err, postgres.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "CONFLICT")
	default:
		s.logError("database operation", err)
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL_ERROR")
	}
}
func (s *Server) logError(op string, err error) {
	if s.Logger != nil {
		s.Logger.Error(op, "error", err)
	}
}
