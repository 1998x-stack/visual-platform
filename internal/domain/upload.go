package domain

import "time"

// UploadSession represents an opaque source object held only in private quarantine.
// ObjectKey is internal metadata and must never be serialized to an API response.
type UploadSession struct {
	ID             string     `json:"id"`
	WorkspaceID    string     `json:"workspace_id"`
	ArtifactID     string     `json:"artifact_id"`
	CreatedBy      string     `json:"created_by"`
	IdempotencyKey string     `json:"-"`
	SourceName     string     `json:"source_name"`
	SourceType     string     `json:"source_type"`
	ExpectedSize   int64      `json:"expected_size"`
	ExpectedSHA256 string     `json:"expected_sha256"`
	ActualSize     int64      `json:"actual_size,omitempty"`
	ActualSHA256   string     `json:"actual_sha256,omitempty"`
	Status         string     `json:"status"`
	FailureCode    string     `json:"failure_code,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
	ReceivedAt     *time.Time `json:"received_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	ObjectKey      string     `json:"-"`
}

type WorkspaceQuota struct {
	WorkspaceID          string `json:"workspace_id"`
	MaxBytes             int64  `json:"max_bytes"`
	UsedBytes            int64  `json:"used_bytes"`
	ReservedBytes        int64  `json:"reserved_bytes"`
	MaxSingleUploadBytes int64  `json:"max_single_upload_bytes"`
	MaxActiveUploads     int    `json:"max_active_uploads"`
}
