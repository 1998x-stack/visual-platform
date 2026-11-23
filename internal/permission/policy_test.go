package permission

import (
	"testing"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
)

func TestRoleAndIsolationMatrix(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	user := domain.Principal{UserID: "alice", Active: true}
	tests := []struct {
		name   string
		wrole  domain.Role
		action domain.Action
		status string
		active bool
		want   bool
	}{
		{"owner manage members", domain.Owner, domain.WorkspaceManageMembers, "published", true, true},
		{"owner transfer", domain.Owner, domain.ArtifactTransfer, "published", true, true},
		{"maintainer publish", domain.Maintainer, domain.ArtifactPublish, "published", true, true},
		{"maintainer cannot grant acl", domain.Maintainer, domain.ArtifactManageACL, "published", true, false},
		{"editor creates", domain.Editor, domain.ArtifactCreate, "draft", true, true},
		{"editor cannot transfer", domain.Editor, domain.ArtifactTransfer, "published", true, false},
		{"viewer reads", domain.Viewer, domain.ArtifactRead, "published", true, true},
		{"viewer cannot publish", domain.Viewer, domain.ArtifactPublish, "published", true, false},
		{"no membership denied", "", domain.ArtifactRead, "published", true, false},
		{"draft unreadable even to owner", domain.Owner, domain.ArtifactRead, "draft", true, false},
		{"suspended unreadable", domain.Owner, domain.ArtifactRead, "suspended", true, false},
		{"inactive owner denied", domain.Owner, domain.ArtifactRead, "published", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := user
			p.Active = tc.active
			got := Decide(p, tc.action, "active", tc.status, tc.wrole, nil, now)
			if got.Allowed != tc.want {
				t.Fatalf("allowed=%v reason=%q want=%v", got.Allowed, got.Reason, tc.want)
			}
		})
	}
	if d := Decide(user, domain.ArtifactRead, "suspended", "published", domain.Owner, nil, now); d.Allowed {
		t.Fatal("suspended workspace must deny even owner")
	}
}

func TestACLSubjectExpirationAndScope(t *testing.T) {
	now := time.Now().UTC()
	p := domain.Principal{UserID: "alice", Active: true, Groups: []string{"project:42"}}
	past, future := now.Add(-time.Minute), now.Add(time.Minute)
	tests := []struct {
		name   string
		grant  domain.Grant
		action domain.Action
		want   bool
	}{
		{"direct user read", domain.Grant{SubjectType: "user", SubjectID: "alice", Role: domain.Viewer}, domain.ArtifactRead, true},
		{"direct user cannot publish as viewer", domain.Grant{SubjectType: "user", SubjectID: "alice", Role: domain.Viewer}, domain.ArtifactPublish, false},
		{"direct editor can publish", domain.Grant{SubjectType: "user", SubjectID: "alice", Role: domain.Editor}, domain.ArtifactPublish, true},
		{"group read", domain.Grant{SubjectType: "group", SubjectID: "project:42", Role: domain.Viewer, ExpiresAt: &future}, domain.ArtifactRead, true},
		{"unmatched group", domain.Grant{SubjectType: "group", SubjectID: "other", Role: domain.Viewer}, domain.ArtifactRead, false},
		{"expired user", domain.Grant{SubjectType: "user", SubjectID: "alice", Role: domain.Viewer, ExpiresAt: &past}, domain.ArtifactRead, false},
		{"other user", domain.Grant{SubjectType: "user", SubjectID: "bob", Role: domain.Viewer}, domain.ArtifactRead, false},
		{"acl never manages workspace", domain.Grant{SubjectType: "user", SubjectID: "alice", Role: domain.Maintainer}, domain.WorkspaceManageMembers, false},
		{"acl never manages acl", domain.Grant{SubjectType: "user", SubjectID: "alice", Role: domain.Maintainer}, domain.ArtifactManageACL, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(p, tc.action, "active", "published", "", []domain.Grant{tc.grant}, now)
			if got.Allowed != tc.want {
				t.Fatalf("allowed=%v reason=%q want=%v", got.Allowed, got.Reason, tc.want)
			}
		})
	}
}
