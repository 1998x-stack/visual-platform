package minio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	minioSDK "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Store writes to one pre-provisioned, private quarantine bucket.
// It deliberately has no Get, List, presign, or publish operation.
type Store struct {
	client *minioSDK.Client
	bucket string
}

type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Region    string
	Secure    bool
}

func New(ctx context.Context, cfg Config) (*Store, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" ||
		strings.TrimSpace(cfg.AccessKey) == "" ||
		strings.TrimSpace(cfg.SecretKey) == "" ||
		strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("incomplete S3 quarantine configuration")
	}
	if strings.Contains(cfg.Endpoint, "://") || strings.ContainsAny(cfg.Endpoint, "/?#") {
		return nil, errors.New("S3_ENDPOINT must be a host:port without scheme or path")
	}
	client, err := minioSDK.New(cfg.Endpoint, &minioSDK.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("create S3 client: %w", err)
	}
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("check quarantine bucket: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("quarantine bucket %q does not exist; provision it privately before starting the API", cfg.Bucket)
	}
	policy, err := client.GetBucketPolicy(ctx, cfg.Bucket)
	if err != nil {
		response := minioSDK.ToErrorResponse(err)
		if response.Code != "NoSuchBucketPolicy" {
			return nil, fmt.Errorf("verify quarantine bucket policy: %w", err)
		}
	}
	if strings.TrimSpace(policy) != "" && strings.TrimSpace(policy) != "null" {
		return nil, errors.New("quarantine bucket has an explicit bucket policy; remove public or unreviewed policies before starting")
	}
	return &Store{client: client, bucket: cfg.Bucket}, nil
}

func (s *Store) Put(ctx context.Context, objectKey string, body io.Reader, size int64) error {
	if !validObjectKey(objectKey) || size <= 0 {
		return errors.New("invalid quarantine object request")
	}
	_, err := s.client.PutObject(ctx, s.bucket, objectKey, body, size,
		minioSDK.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return fmt.Errorf("put quarantine object: %w", err)
	}
	return nil
}

func (s *Store) Delete(ctx context.Context, objectKey string) error {
	if !validObjectKey(objectKey) {
		return errors.New("invalid quarantine object key")
	}
	if err := s.client.RemoveObject(ctx, s.bucket, objectKey, minioSDK.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete quarantine object: %w", err)
	}
	return nil
}

func validObjectKey(key string) bool {
	if !strings.HasPrefix(key, "quarantine/") || strings.Contains(key, "..") || strings.Contains(key, "\\") {
		return false
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}
