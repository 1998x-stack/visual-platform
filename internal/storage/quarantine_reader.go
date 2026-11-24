package storage

import (
	"context"
	"io"
)

// QuarantineReader is intentionally separate from QuarantineStore.
// A worker has read-only object access and never receives upload/delete methods.
// The caller must resolve objectKey from an authorized database Upload Session,
// not from any client-controlled request or object listing.
type QuarantineReader interface {
	Open(ctx context.Context, objectKey string, expectedSize int64) (io.ReadCloser, error)
}
