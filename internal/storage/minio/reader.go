package minio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	minioSDK "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/1998x-stack/visual-platform/internal/storage"
)

// Reader has a separate S3 identity from the upload API. Its interface grants
// only exact-key GetObject operations, never Put, Delete, or bucket listing.
type Reader struct {
	client *minioSDK.Client
	bucket string
}

// ReaderConfig requires an independently provisioned read-only S3 credential.
// AllowInsecureDevelopment is for isolated loopback integration tests only.
type ReaderConfig struct {
	Endpoint                 string
	AccessKey                string
	SecretKey                string
	Bucket                   string
	Region                   string
	Secure                   bool
	AllowInsecureDevelopment bool
}

// NewReader does not call BucketExists or GetBucketPolicy: the Worker identity
// must only need s3:GetObject for the quarantine prefix, not ListBucket.
func NewReader(cfg ReaderConfig) (*Reader, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" ||
		strings.TrimSpace(cfg.AccessKey) == "" ||
		strings.TrimSpace(cfg.SecretKey) == "" ||
		strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("incomplete read-only quarantine S3 configuration")
	}
	if strings.Contains(cfg.Endpoint, "://") || strings.ContainsAny(cfg.Endpoint, "/?#") {
		return nil, errors.New("quarantine S3 endpoint must be a host:port")
	}
	if !cfg.Secure && !cfg.AllowInsecureDevelopment {
		return nil, errors.New("quarantine reader requires TLS outside isolated development")
	}
	client, err := minioSDK.New(cfg.Endpoint, &minioSDK.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create read-only quarantine client: %w", err)
	}
	return &Reader{client: client, bucket: cfg.Bucket}, nil
}

// Open reads only the exact DB-owned quarantine key. It rejects a stale or
// changed object whose remotely reported size differs from the Upload Session.
// The caller must also verify the source SHA-256 before publishing anything.
func (r *Reader) Open(ctx context.Context, objectKey string, expectedSize int64) (io.ReadCloser, error) {
	if r == nil || r.client == nil || !validObjectKey(objectKey) || expectedSize <= 0 {
		return nil, errors.New("invalid read-only quarantine request")
	}
	if ctx == nil {
		return nil, errors.New("quarantine read context is required")
	}
	object, err := r.client.GetObject(ctx, r.bucket, objectKey, minioSDK.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("open quarantine object: %w", err)
	}
	stat, err := object.Stat()
	if err != nil {
		_ = object.Close()
		return nil, fmt.Errorf("stat quarantine object: %w", err)
	}
	if stat.Size != expectedSize {
		_ = object.Close()
		return nil, fmt.Errorf("quarantine object size mismatch")
	}
	return &exactSizeReadCloser{source: object, expected: expectedSize}, nil
}

// exactSizeReadCloser detects truncated or unexpectedly oversized response
// streams even if the object changed between Stat and Get.
type exactSizeReadCloser struct {
	source   io.ReadCloser
	expected int64
	read     int64
}

func (r *exactSizeReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return 0, nil
	}
	if r.read == r.expected {
		var extra [1]byte
		n, err := r.source.Read(extra[:])
		if n > 0 {
			return 0, errors.New("quarantine object longer than expected")
		}
		return 0, err
	}
	remaining := r.expected - r.read
	if int64(len(buffer)) > remaining {
		buffer = buffer[:int(remaining)]
	}
	n, err := r.source.Read(buffer)
	r.read += int64(n)
	if err == io.EOF && r.read < r.expected {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func (r *exactSizeReadCloser) Close() error {
	return r.source.Close()
}

var _ storage.QuarantineReader = (*Reader)(nil)
