package domain

import "time"

type User struct {
	ID          string `json:"id"`
	Subject     string `json:"subject,omitempty"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
}
type Workspace struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	OwnerRef  string `json:"owner_ref"`
	CreatedBy string `json:"created_by"`
	Status    string `json:"status"`
}
type Member struct {
	UserID string `json:"user_id"`
	Role   Role   `json:"role"`
}
type Artifact struct {
	ID               string `json:"id"`
	WorkspaceID      string `json:"workspace_id"`
	CreatedBy        string `json:"created_by"`
	Slug             string `json:"slug"`
	Status           string `json:"status"`
	CurrentVersionID string `json:"current_version_id,omitempty"`
}
type Grant struct {
	SubjectType string     `json:"subject_type"`
	SubjectID   string     `json:"subject_id"`
	Role        Role       `json:"role"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}
type Principal struct {
	UserID string
	Active bool
	Groups []string
}
type Role string

const (
	Owner      Role = "owner"
	Maintainer Role = "maintainer"
	Editor     Role = "editor"
	Viewer     Role = "viewer"
)

type Action string

const (
	WorkspaceRead          Action = "workspace.read"
	WorkspaceManageMembers Action = "workspace.manage_members"
	ArtifactCreate         Action = "artifact.create"
	ArtifactRead           Action = "artifact.read"
	ArtifactPublish        Action = "artifact.publish"
	ArtifactManageACL      Action = "artifact.manage_acl"
	ArtifactTransfer       Action = "artifact.transfer"
	ArtifactDelete         Action = "artifact.delete"
)

type Decision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
}
