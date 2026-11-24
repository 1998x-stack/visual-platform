package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/1998x-stack/visual-platform/internal/domain"
	"github.com/1998x-stack/visual-platform/internal/scanner"
	"github.com/1998x-stack/visual-platform/internal/validator"
)

type validationFixture struct {
	db       *sql.DB
	store    *Store
	ctx      context.Context
	owner    string
	artifact string
}

func newValidationFixture(t *testing.T) validationFixture {
	t.Helper()
	dbURL := os.Getenv("DB_TEST_URL")
	if dbURL == "" {
		t.Skip("DB_TEST_URL is not set: PostgreSQL integration test NOT RUN")
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	store := &Store{DB: db}
	var owner string
	if err := db.QueryRowContext(ctx,
		`INSERT INTO users(oidc_subject,display_name) VALUES($1,'Validator owner') RETURNING id::text`,
		fmt.Sprintf("m3c-%s-%d", t.Name(), time.Now().UnixNano())).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	workspace, err := store.CreatePersonalWorkspace(ctx, owner, "M3C lifecycle test")
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.CreateArtifact(ctx, workspace.ID, owner, "validation-case")
	if err != nil {
		t.Fatal(err)
	}
	return validationFixture{db: db, store: store, ctx: ctx, owner: owner, artifact: artifact.ID}
}

func (f validationFixture) received(t *testing.T, idempotency string, data []byte) string {
	t.Helper()
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	session, err := f.store.CreateUploadSession(f.ctx, f.owner, f.artifact,
		idempotency, "index.html", "html", int64(len(data)), hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimUpload(f.ctx, session.ID, f.owner); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkUploadReceived(f.ctx, session.ID, f.owner, int64(len(data)), hash); err != nil {
		t.Fatal(err)
	}
	return session.ID
}

func (f validationFixture) enqueue(t *testing.T, idempotency string, data []byte) (domain.ValidationRecord, string) {
	t.Helper()
	uploadID := f.received(t, idempotency, data)
	record, err := f.store.EnqueueValidation(f.ctx, uploadID)
	if err != nil {
		t.Fatal(err)
	}
	return record, uploadID
}

func completeHTML(t *testing.T, f validationFixture, job domain.ValidationJob, data []byte) error {
	t.Helper()
	manifest, err := validator.ValidateHTML(f.ctx, bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	return f.store.CompleteValidation(f.ctx, ValidationCompletion{
		JobID: job.ID, LeaseToken: job.LeaseToken, SourceSHA256: job.SourceSHA256,
		Manifest: manifest,
		Scan: scanner.Summary{
			ScannedFiles: len(manifest.Entries),
			ScannedBytes: manifest.TotalUncompressedBytes,
		},
		ScannerVersion:      "clamav-1.4.3",
		SignatureVersion:    "daily-1234",
		SignaturesUpdatedAt: time.Now().UTC().Add(-time.Hour),
	})
}

func TestM3CEnqueueAndFencedCompletion(t *testing.T) {
	f := newValidationFixture(t)
	data := []byte("<html><body>hello</body></html>")
	record, uploadID := f.enqueue(t, "m3c-completion-0001", data)
	if record.Status != domain.ValidationPending {
		t.Fatalf("unexpected queued record: %+v", record)
	}
	replayed, err := f.store.EnqueueValidation(f.ctx, uploadID)
	if err != nil || replayed.ID != record.ID {
		t.Fatalf("idempotent enqueue failed: %+v %v", replayed, err)
	}
	var count int
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM validation_runs WHERE upload_id=$1::uuid`, uploadID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate validation lifecycle: count=%d err=%v", count, err)
	}

	job, err := f.store.ClaimNextValidation(f.ctx)
	if err != nil || job.UploadID != uploadID || job.ObjectKey == "" ||
		job.Status != domain.ValidationRunning || job.AttemptCount != 1 || job.LeaseToken == "" {
		t.Fatalf("claim=%+v error=%v", job, err)
	}
	if _, err := f.store.ClaimNextValidation(f.ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("active lease should block duplicate claim: %v", err)
	}
	if err := completeHTML(t, f, domain.ValidationJob{
		ID: job.ID, LeaseToken: "00000000-0000-4000-8000-000000000000",
		SourceSHA256: job.SourceSHA256,
	}, data); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong lease succeeded: %v", err)
	}

	// Force the first lease to expire; the second claim must revoke it.
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE validation_runs SET lease_until=now()-interval '1 second'
		 WHERE id=$1::uuid`, job.ID); err != nil {
		t.Fatal(err)
	}
	next, err := f.store.ClaimNextValidation(f.ctx)
	if err != nil || next.ID != job.ID || next.LeaseToken == job.LeaseToken || next.AttemptCount != 2 {
		t.Fatalf("expired lease was not fenced: next=%+v err=%v", next, err)
	}
	if err := completeHTML(t, f, job, data); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired token incorrectly succeeded: %v", err)
	}
	if err := completeHTML(t, f, next, data); err != nil {
		t.Fatalf("current worker failed completion: %v", err)
	}
	if err := completeHTML(t, f, next, data); !errors.Is(err, ErrConflict) {
		t.Fatalf("completion replay must conflict: %v", err)
	}
	var status, manifestHash, manifestJSON string
	var attempts int
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT status,manifest_sha256,validated_manifest::text,attempt_count
		 FROM validation_runs WHERE id=$1::uuid`, next.ID).
		Scan(&status, &manifestHash, &manifestJSON, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "validated" || attempts != 2 || len(manifestHash) != 64 {
		t.Fatalf("validation evidence not recorded: status=%s attempts=%d hash=%s", status, attempts, manifestHash)
	}
	var decoded validator.Manifest
	if err := json.Unmarshal([]byte(manifestJSON), &decoded); err != nil || decoded.SourceSize != int64(len(data)) {
		t.Fatalf("stored manifest invalid: %+v %v", decoded, err)
	}
	if _, err := f.store.ClaimNextValidation(f.ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("successful job must not be re-claimed: %v", err)
	}
	var events int
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT count(*) FROM audit_events
		 WHERE resource_type='validation' AND resource_id=$1::uuid`, next.ID).
		Scan(&events); err != nil || events != 4 {
		t.Fatalf("expected enqueue+2 claims+success audit events; got %d, %v", events, err)
	}
}

func TestM3CRetryAndTerminalFailures(t *testing.T) {
	f := newValidationFixture(t)
	data := []byte("<html>retry</html>")
	f.enqueue(t, "m3c-retry-0001", data)
	for attempt := 1; attempt <= 3; attempt++ {
		job, err := f.store.ClaimNextValidation(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if job.AttemptCount != attempt {
			t.Fatalf("attempt=%d want=%d", job.AttemptCount, attempt)
		}
		if _, err := f.store.FinishValidationFailure(f.ctx, job.ID, job.LeaseToken,
			"malware_detected", true); !errors.Is(err, ErrConflict) {
			t.Fatalf("malware cannot be classified transient: %v", err)
		}
		status, err := f.store.FinishValidationFailure(f.ctx, job.ID, job.LeaseToken,
			"scanner_unavailable", true)
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 3 && status != domain.ValidationRetryable {
			t.Fatalf("unexpected retry status: %s", status)
		}
		if attempt == 3 && status != domain.ValidationFailed {
			t.Fatalf("attempt 3 must exhaust retries: %s", status)
		}
		if _, err := f.store.FinishValidationFailure(f.ctx, job.ID, job.LeaseToken,
			"scanner_unavailable", true); !errors.Is(err, ErrConflict) {
			t.Fatalf("worker may only finish once: %v", err)
		}
		if attempt < 3 {
			if _, err := f.store.ClaimNextValidation(f.ctx); !errors.Is(err, ErrNotFound) {
				t.Fatalf("backoff must block immediate retry: %v", err)
			}
			if _, err := f.db.ExecContext(f.ctx,
				`UPDATE validation_runs SET next_attempt_at=now()-interval '1 second'
				 WHERE id=$1::uuid`, job.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := f.store.ClaimNextValidation(f.ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal failed run reclaimed: %v", err)
	}
	f.enqueue(t, "m3c-reject-0001", []byte("<html>bad</html>"))
	job, err := f.store.ClaimNextValidation(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.store.FinishValidationFailure(f.ctx, job.ID, job.LeaseToken,
		"malware_detected", false)
	if err != nil || status != domain.ValidationRejected {
		t.Fatalf("deterministic rejection failed: %s %v", status, err)
	}
}

func TestM3CSourceBindingAndEvidenceGate(t *testing.T) {
	f := newValidationFixture(t)
	data := []byte("<html>hash-bound</html>")
	record, uploadID := f.enqueue(t, "m3c-source-0001", data)
	job, err := f.store.ClaimNextValidation(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	wrong := job
	wrong.SourceSHA256 = strings.Repeat("f", 64)
	if err := completeHTML(t, f, wrong, data); !errors.Is(err, ErrConflict) {
		t.Fatalf("unbound source fingerprint accepted: %v", err)
	}
	manifest, err := validator.ValidateHTML(f.ctx, bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Entries[0].SHA256 = strings.Repeat("0", 64)
	if err := f.store.CompleteValidation(f.ctx, ValidationCompletion{
		JobID: job.ID, LeaseToken: job.LeaseToken,
		SourceSHA256: job.SourceSHA256, Manifest: manifest,
		Scan:           scanner.Summary{ScannedFiles: 0, ScannedBytes: manifest.TotalUncompressedBytes},
		ScannerVersion: "clamav-1", SignatureVersion: "daily-1",
		SignaturesUpdatedAt: time.Now().UTC(),
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("incomplete scan summary accepted: %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE upload_sessions SET actual_sha256=$2 WHERE id=$1::uuid`,
		uploadID, strings.Repeat("e", 64)); err != nil {
		t.Fatal(err)
	}
	if err := completeHTML(t, f, job, data); !errors.Is(err, ErrConflict) {
		t.Fatalf("mutated source was accepted: %v", err)
	}
	if _, err := f.db.ExecContext(f.ctx,
		`UPDATE upload_sessions SET actual_sha256=expected_sha256 WHERE id=$1::uuid`, uploadID); err != nil {
		t.Fatal(err)
	}
	if err := completeHTML(t, f, job, data); err != nil {
		t.Fatalf("valid lease should complete after restored source: %v", err)
	}
	var id string
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT id::text FROM validation_runs WHERE id=$1::uuid AND status='validated'`,
		record.ID).Scan(&id); err != nil || id != record.ID {
		t.Fatalf("missing validated record: %s %v", id, err)
	}
}

func TestM3CConcurrentClaims(t *testing.T) {
	f := newValidationFixture(t)
	f.enqueue(t, "m3c-concurrent-0001", []byte("<html>parallel</html>"))
	const workers = 8
	results := make(chan domain.ValidationJob, workers)
	errorsCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			job, err := f.store.ClaimNextValidation(f.ctx)
			if err == nil {
				results <- job
			} else if !errors.Is(err, ErrNotFound) {
				errorsCh <- err
			}
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatalf("concurrent claim failed: %v", err)
	}
	count := 0
	for range results {
		count++
	}
	if count != 1 {
		t.Fatalf("expected exactly one lease among %d claimants, got %d", workers, count)
	}
}

func TestM3CUnreceivedUploadCannotEnqueue(t *testing.T) {
	f := newValidationFixture(t)
	sum := strings.Repeat("a", 64)
	session, err := f.store.CreateUploadSession(f.ctx, f.owner, f.artifact,
		"m3c-unreceived-0001", "index.html", "html", 10, sum)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.EnqueueValidation(f.ctx, session.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unreceived upload was enqueued: %v", err)
	}
}

func TestM3CExpiredThirdLeaseExhaustsAttempts(t *testing.T) {
	f := newValidationFixture(t)
	f.enqueue(t, "m3c-expired-leases-0001", []byte("<html>lease</html>"))
	var last domain.ValidationJob
	for attempt := 1; attempt <= 3; attempt++ {
		job, err := f.store.ClaimNextValidation(f.ctx)
		if err != nil || job.AttemptCount != attempt {
			t.Fatalf("claim attempt %d: %+v %v", attempt, job, err)
		}
		if last.ID != "" && job.LeaseToken == last.LeaseToken {
			t.Fatal("expired lease token must be replaced")
		}
		if _, err := f.db.ExecContext(f.ctx,
			`UPDATE validation_runs SET lease_until=now()-interval '1 second'
			 WHERE id=$1::uuid`, job.ID); err != nil {
			t.Fatal(err)
		}
		last = job
	}
	if _, err := f.store.ClaimNextValidation(f.ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired third attempt must be terminal: %v", err)
	}
	var status, reason string
	if err := f.db.QueryRowContext(f.ctx,
		`SELECT status,failure_code FROM validation_runs WHERE id=$1::uuid`, last.ID).
		Scan(&status, &reason); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || reason != "retry_exhausted" {
		t.Fatalf("unexpected terminal outcome: %s/%s", status, reason)
	}
	if err := completeHTML(t, f, last, []byte("<html>lease</html>")); !errors.Is(err, ErrConflict) {
		t.Fatalf("expired third worker must not report clean: %v", err)
	}
}

func TestM3CRejectsStaleOrFutureSignatureEvidence(t *testing.T) {
	f := newValidationFixture(t)
	data := []byte("<html>signature</html>")
	f.enqueue(t, "m3c-signature-0001", data)
	job, err := f.store.ClaimNextValidation(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := validator.ValidateHTML(f.ctx, bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	completion := ValidationCompletion{
		JobID: job.ID, LeaseToken: job.LeaseToken, SourceSHA256: job.SourceSHA256,
		Manifest:         manifest,
		Scan:             scanner.Summary{ScannedFiles: 1, ScannedBytes: manifest.TotalUncompressedBytes},
		ScannerVersion:   "clamav-1.4.3",
		SignatureVersion: "daily-1234",
	}
	for _, bad := range []time.Time{time.Now().UTC().Add(-49 * time.Hour), time.Now().UTC().Add(10 * time.Minute)} {
		completion.SignaturesUpdatedAt = bad
		if err := f.store.CompleteValidation(f.ctx, completion); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid signature timestamp %v was accepted: %v", bad, err)
		}
	}
	completion.SignaturesUpdatedAt = time.Now().UTC().Add(-time.Minute)
	if err := f.store.CompleteValidation(f.ctx, completion); err != nil {
		t.Fatalf("valid current signatures rejected: %v", err)
	}
}
