package domain

import "time"

// ValidationStatus describes persisted internal verification state. In
// particular, validated is not a browser publication grant.
type ValidationStatus string

const (
	ValidationPending   ValidationStatus = "pending"
	ValidationRunning   ValidationStatus = "running"
	ValidationRetryable ValidationStatus = "retryable"
	ValidationValidated ValidationStatus = "validated"
	ValidationRejected  ValidationStatus = "rejected"
	ValidationFailed    ValidationStatus = "failed"
)

// ValidationJob is a worker-only object. Never serialize this as a public API:
// both ObjectKey and LeaseToken are privileged internal capabilities.
type ValidationJob struct {
	ID            string
	UploadID      string
	WorkspaceID   string
	ArtifactID    string
	ObjectKey     string
	SourceType    string
	SourceSize    int64
	SourceSHA256  string
	Status        ValidationStatus
	AttemptCount  int
	MaxAttempts   int
	LeaseToken    string
	LeaseUntil    time.Time
	NextAttemptAt time.Time
}

// ValidationRecord exposes internal lifecycle metadata but no S3 object key,
// lease token or source manifest data to the management API.
type ValidationRecord struct {
	ID             string           `json:"id"`
	UploadID       string           `json:"upload_id"`
	ArtifactID     string           `json:"artifact_id"`
	Status         ValidationStatus `json:"status"`
	AttemptCount   int              `json:"attempt_count"`
	ManifestSHA256 string           `json:"manifest_sha256,omitempty"`
	FailureCode    string           `json:"failure_code,omitempty"`
	CompletedAt    *time.Time       `json:"completed_at,omitempty"`
}
