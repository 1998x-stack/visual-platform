package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
	"github.com/1998x-stack/visual-platform/internal/requestmeta"
	"github.com/1998x-stack/visual-platform/internal/scanner"
	"github.com/1998x-stack/visual-platform/internal/validator"
)

const (
	validationLeaseMinutes = 5
	maxSignatureAge        = 48 * time.Hour
	maxClockSkew           = 5 * time.Minute
)

var (
	validHash     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	validEvidence = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:+/-]{0,127}$`)
)

type ValidationCompletion struct {
	JobID               string
	LeaseToken          string
	SourceSHA256        string
	Manifest            validator.Manifest
	Scan                scanner.Summary
	ScannerVersion      string
	SignatureVersion    string
	SignaturesUpdatedAt time.Time
}

// EnqueueValidation idempotently creates one immutable source-bound validation
// lifecycle. Only successfully received uploads are eligible.
func (s *Store) EnqueueValidation(ctx context.Context, uploadID string) (domain.ValidationRecord, error) {
	var record domain.ValidationRecord
	if s == nil || s.DB == nil || ctx == nil {
		return record, errors.New("validation database is not configured")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return record, err
	}
	defer tx.Rollback()
	var insertedID string
	err = tx.QueryRowContext(ctx, `
		INSERT INTO validation_runs(upload_id,workspace_id,artifact_id,source_type,source_size,source_sha256)
		SELECT id,workspace_id,artifact_id,source_type,actual_size,actual_sha256
		FROM upload_sessions
		WHERE id=$1::uuid AND status='received'
		  AND actual_size=expected_size AND actual_sha256=expected_sha256
		ON CONFLICT(upload_id) DO NOTHING
		RETURNING id::text`, uploadID).Scan(&insertedID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return record, writeError(err)
	}
	err = tx.QueryRowContext(ctx, `
		SELECT v.id::text,v.upload_id::text,v.artifact_id::text,v.status,
		  v.attempt_count,COALESCE(v.manifest_sha256,''),COALESCE(v.failure_code,''),v.completed_at
		FROM validation_runs v JOIN upload_sessions u ON u.id=v.upload_id
		WHERE v.upload_id=$1::uuid AND u.status='received'
		  AND v.workspace_id=u.workspace_id AND v.artifact_id=u.artifact_id
		  AND v.source_size=u.actual_size AND v.source_sha256=u.actual_sha256
		  AND v.source_type=u.source_type
		  AND u.actual_size=u.expected_size AND u.actual_sha256=u.expected_sha256`, uploadID).
		Scan(&record.ID, &record.UploadID, &record.ArtifactID, &record.Status,
			&record.AttemptCount, &record.ManifestSHA256, &record.FailureCode, &record.CompletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return record, ErrConflict
	}
	if err != nil {
		return record, err
	}
	if insertedID != "" {
		if err := appendValidationAudit(ctx, tx, "validation.enqueue", record.ID,
			map[string]any{"upload_id": uploadID}); err != nil {
			return domain.ValidationRecord{}, err
		}
	}
	return record, tx.Commit()
}

// ClaimNextValidation uses PostgreSQL row locks to grant a worker one fenced,
// time-limited claim. Expired leases are reclaimable only below max_attempts.
func (s *Store) ClaimNextValidation(ctx context.Context) (domain.ValidationJob, error) {
	var job domain.ValidationJob
	if s == nil || s.DB == nil || ctx == nil {
		return job, errors.New("validation database is not configured")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return job, err
	}
	defer tx.Rollback()

	exhausted, err := tx.QueryContext(ctx, `
		UPDATE validation_runs
		SET status='failed',failure_code='retry_exhausted',lease_token=NULL,
		  lease_until=NULL,completed_at=now(),updated_at=now()
		WHERE status='running' AND lease_until<=now()
		  AND attempt_count>=max_attempts
		RETURNING id::text`)
	if err != nil {
		return job, err
	}
	var exhaustedIDs []string
	for exhausted.Next() {
		var id string
		if err := exhausted.Scan(&id); err != nil {
			exhausted.Close()
			return job, err
		}
		exhaustedIDs = append(exhaustedIDs, id)
	}
	if err := exhausted.Err(); err != nil {
		exhausted.Close()
		return job, err
	}
	exhausted.Close()
	for _, id := range exhaustedIDs {
		if err := appendValidationAudit(ctx, tx, "validation.exhausted", id,
			map[string]any{"failure_code": "retry_exhausted"}); err != nil {
			return job, err
		}
	}

	err = tx.QueryRowContext(ctx, `
		WITH candidate AS (
		  SELECT v.id FROM validation_runs v
		  JOIN upload_sessions u ON u.id=v.upload_id
		  JOIN workspaces w ON w.id=v.workspace_id
		  JOIN artifacts a ON a.id=v.artifact_id AND a.workspace_id=v.workspace_id
		  WHERE (
		    (v.status IN ('pending','retryable') AND v.next_attempt_at<=now())
		    OR (v.status='running' AND v.lease_until<=now())
		  )
		    AND v.attempt_count<v.max_attempts
		    AND u.status='received'
		    AND u.expected_size=v.source_size AND u.actual_size=v.source_size
		    AND u.expected_sha256=v.source_sha256 AND u.actual_sha256=v.source_sha256
		    AND u.source_type=v.source_type
		    AND w.status='active' AND a.status='draft'
		  ORDER BY v.created_at,v.id
		  FOR UPDATE OF v SKIP LOCKED LIMIT 1
		), claimed AS (
		  UPDATE validation_runs v
		  SET status='running',lease_token=gen_random_uuid(),
		    lease_until=now()+interval '5 minutes',attempt_count=attempt_count+1,
		    failure_code=NULL,updated_at=now()
		  FROM candidate WHERE v.id=candidate.id
		  RETURNING v.*
		)
		SELECT c.id::text,c.upload_id::text,c.workspace_id::text,c.artifact_id::text,
		  u.object_key,c.source_type,c.source_size,c.source_sha256,c.status,
		  c.attempt_count,c.max_attempts,c.lease_token::text,c.lease_until,c.next_attempt_at
		FROM claimed c JOIN upload_sessions u ON u.id=c.upload_id`).
		Scan(&job.ID, &job.UploadID, &job.WorkspaceID, &job.ArtifactID,
			&job.ObjectKey, &job.SourceType, &job.SourceSize, &job.SourceSHA256,
			&job.Status, &job.AttemptCount, &job.MaxAttempts, &job.LeaseToken,
			&job.LeaseUntil, &job.NextAttemptAt)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return domain.ValidationJob{}, err
		}
		return job, ErrNotFound
	}
	if err != nil {
		return job, err
	}
	if err := appendValidationAudit(ctx, tx, "validation.claim", job.ID,
		map[string]any{"attempt": job.AttemptCount, "lease_seconds": validationLeaseMinutes * 60}); err != nil {
		return domain.ValidationJob{}, err
	}
	return job, tx.Commit()
}

// CompleteValidation records a success only for a live matching lease, matching
// immutable M2 source fingerprint, structurally sound Manifest, complete scan
// summary and fresh scanner evidence. This trusts the internal Worker to have
// actually scanned the bytes; the DB alone cannot prove that it did.
func (s *Store) CompleteValidation(ctx context.Context, completion ValidationCompletion) error {
	if s == nil || s.DB == nil || ctx == nil {
		return errors.New("validation database is not configured")
	}
	if !validHash.MatchString(completion.SourceSHA256) ||
		!validEvidence.MatchString(completion.ScannerVersion) ||
		!validEvidence.MatchString(completion.SignatureVersion) ||
		!validSignatureTimestamp(completion.SignaturesUpdatedAt) {
		return ErrConflict
	}
	rawManifest, err := json.Marshal(completion.Manifest)
	if err != nil || len(rawManifest) > 8<<20 {
		return ErrConflict
	}
	if err := verifyValidationManifest(completion.Manifest, completion.Scan); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	digest := sha256.Sum256(rawManifest)
	manifestHash := hex.EncodeToString(digest[:])

	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var sourceSize int64
	var sourceSHA, sourceType, uploadStatus, uploadSHA string
	var uploadSize int64
	err = tx.QueryRowContext(ctx, `
		SELECT v.source_size,v.source_sha256,v.source_type,
		  u.status,COALESCE(u.actual_sha256,''),COALESCE(u.actual_size,0)
		FROM validation_runs v JOIN upload_sessions u ON u.id=v.upload_id
		WHERE v.id=$1::uuid AND v.status='running'
		  AND v.lease_token=$2::uuid AND v.lease_until>now()
		FOR UPDATE OF v,u`, completion.JobID, completion.LeaseToken).
		Scan(&sourceSize, &sourceSHA, &sourceType, &uploadStatus, &uploadSHA, &uploadSize)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if sourceSize != completion.Manifest.SourceSize || sourceSize != uploadSize ||
		sourceSHA != uploadSHA || sourceSHA != completion.SourceSHA256 ||
		uploadStatus != "received" || (sourceType != "zip" && sourceType != "html") {
		return ErrConflict
	}
	if sourceType == "html" && (len(completion.Manifest.Entries) != 1 ||
		completion.Manifest.Entries[0].SourcePath != "index.html") {
		return ErrConflict
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE validation_runs
		SET status='validated',lease_token=NULL,lease_until=NULL,
		  validated_manifest=$3::jsonb,manifest_sha256=$4,
		  scanned_files=$5,scanned_bytes=$6,
		  scanner_version=$7,signature_version=$8,signatures_updated_at=$9,
		  failure_code=NULL,completed_at=now(),updated_at=now()
		WHERE id=$1::uuid AND lease_token=$2::uuid AND status='running' AND lease_until>now()`,
		completion.JobID, completion.LeaseToken, string(rawManifest), manifestHash,
		completion.Scan.ScannedFiles, int64(completion.Scan.ScannedBytes),
		completion.ScannerVersion, completion.SignatureVersion, completion.SignaturesUpdatedAt)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return ErrConflict
	}
	if err := appendValidationAudit(ctx, tx, "validation.validated", completion.JobID,
		map[string]any{"manifest_sha256": manifestHash,
			"scanned_files":     completion.Scan.ScannedFiles,
			"scanned_bytes":     completion.Scan.ScannedBytes,
			"scanner_version":   completion.ScannerVersion,
			"signature_version": completion.SignatureVersion}); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishValidationFailure classifies a scan/validation error while respecting
// the active lease. Transient failures are delayed and capped; deterministic
// failures are terminal and never become validation success.
func (s *Store) FinishValidationFailure(ctx context.Context, jobID, leaseToken, code string, retryable bool) (domain.ValidationStatus, error) {
	if s == nil || s.DB == nil || ctx == nil {
		return "", errors.New("validation database is not configured")
	}
	if !validValidationFailure(code, retryable) {
		return "", ErrConflict
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var attempts, maximum int
	err = tx.QueryRowContext(ctx, `
		SELECT attempt_count,max_attempts FROM validation_runs
		WHERE id=$1::uuid AND status='running'
		  AND lease_token=$2::uuid AND lease_until>now()
		FOR UPDATE`, jobID, leaseToken).Scan(&attempts, &maximum)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrConflict
	}
	if err != nil {
		return "", err
	}
	status := domain.ValidationRejected
	failureCode := code
	delaySeconds := int64(0)
	if retryable {
		if attempts >= maximum {
			status = domain.ValidationFailed
			failureCode = "retry_exhausted"
		} else {
			status = domain.ValidationRetryable
			delaySeconds = int64(60) << uint(attempts-1)
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE validation_runs
		SET status=$3,lease_token=NULL,lease_until=NULL,failure_code=$4,
		  next_attempt_at=now()+($5::bigint * interval '1 second'),
		  completed_at=CASE WHEN $3='retryable' THEN NULL ELSE now() END,
		  updated_at=now()
		WHERE id=$1::uuid AND lease_token=$2::uuid
		  AND status='running' AND lease_until>now()`,
		jobID, leaseToken, string(status), failureCode, delaySeconds)
	if err != nil {
		return "", err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return "", ErrConflict
	}
	if err := appendValidationAudit(ctx, tx, "validation.failure", jobID,
		map[string]any{"status": status, "failure_code": code, "attempt": attempts,
			"retry_delay_seconds": delaySeconds}); err != nil {
		return "", err
	}
	return status, tx.Commit()
}

func validValidationFailure(code string, retryable bool) bool {
	switch code {
	case "invalid_content", "malware_detected", "source_mismatch":
		return !retryable
	case "scanner_unavailable", "scanner_incomplete", "signature_stale", "internal_error":
		return retryable
	default:
		return false
	}
}

func validSignatureTimestamp(updated time.Time) bool {
	if updated.IsZero() {
		return false
	}
	now := time.Now().UTC()
	return !updated.Before(now.Add(-maxSignatureAge)) && !updated.After(now.Add(maxClockSkew))
}

func verifyValidationManifest(manifest validator.Manifest, summary scanner.Summary) error {
	if manifest.SchemaVersion != 1 || manifest.IndexFile != "index.html" ||
		manifest.SourceSize <= 0 || manifest.EntryCount < 1 ||
		manifest.EntryCount > 4000 || manifest.EntryCount != len(manifest.Entries) ||
		summary.ScannedFiles != len(manifest.Entries) ||
		summary.ScannedBytes != manifest.TotalUncompressedBytes ||
		summary.ScannedBytes > math.MaxInt64 {
		return errors.New("manifest and scan summary disagree")
	}
	var previous string
	var total uint64
	var hasIndex bool
	seen := make(map[string]struct{}, len(manifest.Entries))
	for index, entry := range manifest.Entries {
		if entry.Path == "" || entry.SourcePath == "" ||
			(index > 0 && entry.Path <= previous) ||
			!validHash.MatchString(entry.SHA256) ||
			entry.Size > math.MaxInt64 || total > math.MaxInt64-entry.Size {
			return errors.New("invalid manifest file record")
		}
		if strings.HasPrefix(entry.Path, "/") || strings.Contains(entry.Path, "..") ||
			strings.HasPrefix(entry.SourcePath, "/") || strings.Contains(entry.SourcePath, "..") ||
			strings.ContainsAny(entry.Path+entry.SourcePath, "\\:%?#") {
			return errors.New("manifest contains unsafe path")
		}
		if _, duplicate := seen[entry.Path]; duplicate {
			return errors.New("duplicate publish path")
		}
		seen[entry.Path] = struct{}{}
		hasIndex = hasIndex || entry.Path == "index.html"
		total += entry.Size
		previous = entry.Path
	}
	if !hasIndex || total != manifest.TotalUncompressedBytes {
		return errors.New("missing index or incorrect aggregate")
	}
	return nil
}

func appendValidationAudit(ctx context.Context, tx *sql.Tx, action, validationID string, details map[string]any) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO audit_events(actor_user_id,action,resource_type,resource_id,request_id,details)
		VALUES(NULL,$1,'validation',$2::uuid,$3,$4::jsonb)`,
		action, validationID, requestmeta.ID(ctx), string(raw))
	return err
}
