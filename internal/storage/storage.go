// Package storage defines private object storage contracts.
package storage

import (
	"context"
	"io"
)

// QuarantineStore can only write and delete opaque objects in the quarantine
// bucket. It intentionally exposes no public URL, list, read, or publish API.
type QuarantineStore interface {
	Put(ctx context.Context, objectKey string, body io.Reader, size int64) error
	Delete(ctx context.Context, objectKey string) error
}
