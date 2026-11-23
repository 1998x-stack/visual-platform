// Package permission is the single domain-level permission decision point.
// It never trusts role assertions from the HTTP request; callers load grants
// and workspace roles from the repository for the selected resource.
package permission

import (
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
)

func roleAllows(role domain.Role, action domain.Action) bool {
	switch role {
	case domain.Owner:
		switch action {
		case domain.WorkspaceRead, domain.WorkspaceManageMembers,
			domain.ArtifactCreate, domain.ArtifactRead, domain.ArtifactPublish,
			domain.ArtifactManageACL, domain.ArtifactTransfer, domain.ArtifactDelete:
			return true
		}
		return false
	case domain.Maintainer:
		switch action {
		case domain.WorkspaceRead, domain.ArtifactCreate, domain.ArtifactRead, domain.ArtifactPublish:
			return true
		}
	case domain.Editor:
		switch action {
		case domain.WorkspaceRead, domain.ArtifactCreate, domain.ArtifactRead, domain.ArtifactPublish:
			return true
		}
	case domain.Viewer:
		return action == domain.WorkspaceRead || action == domain.ArtifactRead
	}
	return false
}

func Decide(
	principal domain.Principal,
	action domain.Action,
	workspaceStatus, artifactStatus string,
	workspaceRole domain.Role,
	grants []domain.Grant,
	now time.Time,
) domain.Decision {
	deny := func(reason string) domain.Decision { return domain.Decision{Reason: reason} }
	if principal.UserID == "" || !principal.Active {
		return deny("inactive_or_missing_identity")
	}
	if workspaceStatus != "active" {
		return deny("workspace_unavailable")
	}
	if action == domain.ArtifactRead && artifactStatus != "published" {
		return deny("artifact_not_published")
	}
	if artifactStatus == "archived" || artifactStatus == "suspended" {
		return deny("artifact_unavailable")
	}
	if roleAllows(workspaceRole, action) {
		return domain.Decision{Allowed: true, Reason: "workspace_role"}
	}
	// ACL does not confer workspace-level rights or artifact owner powers.
	if action != domain.ArtifactRead && action != domain.ArtifactPublish {
		return deny("no_matching_grant")
	}
	for _, grant := range grants {
		if grant.ExpiresAt != nil && !grant.ExpiresAt.After(now) {
			continue
		}
		switch grant.SubjectType {
		case "user":
			if grant.SubjectID != principal.UserID {
				continue
			}
		case "group":
			found := false
			for _, group := range principal.Groups {
				if group == grant.SubjectID {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		default:
			continue
		}
		if roleAllows(grant.Role, action) {
			return domain.Decision{Allowed: true, Reason: "artifact_acl"}
		}
	}
	return deny("no_matching_grant")
}
