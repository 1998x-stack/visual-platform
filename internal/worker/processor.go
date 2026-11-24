// Package worker runs the private, source-bound validation pipeline.
package worker

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
	"github.com/1998x-stack/visual-platform/internal/scanner"
	"github.com/1998x-stack/visual-platform/internal/storage"
	"github.com/1998x-stack/visual-platform/internal/store/postgres"
	"github.com/1998x-stack/visual-platform/internal/validator"
)

const (
	defaultMaxSourceBytes int64 = 100 << 20
	defaultMaxHTMLBytes   int64 = 10 << 20
	defaultJobTimeout           = 4 * time.Minute
	defaultPollInterval         = 3 * time.Second
	resultWriteTimeout          = 10 * time.Second
)

type Repository interface {
	EnqueueReceivedBatch(ctx context.Context, limit int) (int, error)
	ClaimNextValidation(ctx context.Context) (domain.ValidationJob, error)
	CompleteValidation(ctx context.Context, result postgres.ValidationCompletion) error
	FinishValidationFailure(ctx context.Context, jobID, leaseToken, code string, retryable bool) (domain.ValidationStatus, error)
}

// Config must be supplied with a private spool directory owned by the worker.
// It must not point to an HTTP-accessible path or the ordinary shared /tmp.
type Config struct {
	TempDir          string
	MaxSourceBytes   int64
	MaxHTMLBytes     int64
	JobTimeout       time.Duration
	PollInterval     time.Duration
	ValidationLimits validator.Limits
}

// Processor owns a single-job pipeline. Every uploaded byte must first be
// verified on the private spool before it can reach the ZIP parser or scanner.
type Processor struct {
	repository Repository
	quarantine storage.QuarantineReader
	engine     scanner.EvidenceScanner
	config     Config
	logger     *slog.Logger
}

func NewProcessor(repo Repository, reader storage.QuarantineReader, engine scanner.EvidenceScanner, cfg Config, logger *slog.Logger) (*Processor, error) {
	if repo == nil || reader == nil || engine == nil {
		return nil, errors.New("validation worker dependencies are required")
	}
	if cfg.MaxSourceBytes < 0 || cfg.MaxHTMLBytes < 0 ||
		cfg.JobTimeout < 0 || cfg.PollInterval < 0 {
		return nil, errors.New("worker limits must not be negative")
	}
	if cfg.MaxSourceBytes == 0 {
		cfg.MaxSourceBytes = defaultMaxSourceBytes
	}
	if cfg.MaxHTMLBytes == 0 {
		cfg.MaxHTMLBytes = defaultMaxHTMLBytes
	}
	if cfg.JobTimeout == 0 {
		cfg.JobTimeout = defaultJobTimeout
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.MaxHTMLBytes > cfg.MaxSourceBytes || cfg.JobTimeout >= 5*time.Minute {
		return nil, errors.New("worker limits exceed the active database lease budget")
	}
	if err := validatePrivateTempDir(cfg.TempDir); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Processor{repository: repo, quarantine: reader, engine: engine, config: cfg, logger: logger}, nil
}

func validatePrivateTempDir(dir string) error {
	if dir == "" || !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("WORKER_TEMP_DIR must be a clean absolute private path")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect private spool directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("private spool must be a non-symlink 0700 directory")
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil || resolved != dir {
		return errors.New("spool path may not contain symlink ancestors")
	}
	ownership, ok := info.Sys().(*syscall.Stat_t)
	if !ok || ownership.Uid != uint32(os.Geteuid()) {
		return errors.New("private spool directory must be owned by current worker UID")
	}
	return nil
}

type classifiedFailure struct {
	code      string
	retryable bool
	cause     error
}

func (e *classifiedFailure) Error() string { return e.code + ": " + e.cause.Error() }
func (e *classifiedFailure) Unwrap() error { return e.cause }
func fail(code string, retryable bool, cause error) error {
	return &classifiedFailure{code: code, retryable: retryable, cause: cause}
}

// ProcessNext processes one DB-claimed job. It returns false only when there
// was no eligible job; no result, including a transient failure, is published.
func (p *Processor) ProcessNext(ctx context.Context) (bool, error) {
	if p == nil || ctx == nil {
		return false, errors.New("worker processor/context is missing")
	}
	if _, err := p.repository.EnqueueReceivedBatch(ctx, 25); err != nil {
		return false, fmt.Errorf("reconcile received uploads: %w", err)
	}
	job, err := p.repository.ClaimNextValidation(ctx)
	if errors.Is(err, postgres.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claim validation: %w", err)
	}
	workCtx, cancel := context.WithTimeout(ctx, p.config.JobTimeout)
	defer cancel()
	manifest, scanResult, evidence, workErr := p.evaluate(workCtx, job)

	// Use a fresh short-lived context to write a failure even when an internal
	// job timeout fired. A stale DB lease still refuses this write.
	commitCtx, commitCancel := context.WithTimeout(context.Background(), resultWriteTimeout)
	defer commitCancel()
	if workErr != nil {
		var classified *classifiedFailure
		if !errors.As(workErr, &classified) {
			classified = &classifiedFailure{
				code: "internal_error", retryable: true, cause: workErr,
			}
		}
		status, recordErr := p.repository.FinishValidationFailure(commitCtx,
			job.ID, job.LeaseToken, classified.code, classified.retryable)
		if recordErr != nil {
			return true, fmt.Errorf("validation failed (%s), but could not record failure: %w", classified.code, recordErr)
		}
		p.logger.Warn("validation rejected or deferred", "validation_id", job.ID,
			"failure_code", classified.code, "status", status, "attempt", job.AttemptCount)
		return true, nil
	}
	err = p.repository.CompleteValidation(commitCtx, postgres.ValidationCompletion{
		JobID: job.ID, LeaseToken: job.LeaseToken, SourceSHA256: job.SourceSHA256,
		Manifest: manifest, Scan: scanResult,
		ScannerVersion: evidence.EngineVersion, SignatureVersion: evidence.SignatureVersion,
		SignaturesUpdatedAt: evidence.SignaturesUpdatedAt,
	})
	if err != nil {
		return true, fmt.Errorf("persist validation evidence: %w", err)
	}
	p.logger.Info("validation completed", "validation_id", job.ID,
		"scanned_files", scanResult.ScannedFiles, "scanned_bytes", scanResult.ScannedBytes)
	return true, nil
}

func (p *Processor) evaluate(ctx context.Context, job domain.ValidationJob) (validator.Manifest, scanner.Summary, scanner.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{}, fail("internal_error", true, err)
	}
	if job.SourceSize <= 0 || job.SourceSize > p.config.MaxSourceBytes ||
		(job.SourceType == "html" && job.SourceSize > p.config.MaxHTMLBytes) ||
		(job.SourceType != "html" && job.SourceType != "zip") {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("invalid_content", false, errors.New("source type or size exceeds accepted bounds"))
	}
	if len(job.SourceSHA256) != 64 || job.ObjectKey == "" || job.LeaseToken == "" {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("source_mismatch", false, errors.New("invalid worker-owned source fingerprint"))
	}
	expectedSHA, err := hex.DecodeString(job.SourceSHA256)
	if err != nil || len(expectedSHA) != sha256.Size ||
		strings.ToLower(job.SourceSHA256) != job.SourceSHA256 {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("source_mismatch", false, errors.New("invalid source SHA-256"))
	}

	stream, err := p.quarantine.Open(ctx, job.ObjectKey, job.SourceSize)
	if err != nil {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("internal_error", true, fmt.Errorf("open quarantined source: %w", err))
	}
	defer stream.Close()

	// CreateTemp uses mode 0600 and a randomized filename under our private
	// 0700 directory. Never write an untrusted archive path to the host FS.
	spool, err := os.CreateTemp(p.config.TempDir, "validation-*")
	if err != nil {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("internal_error", true, fmt.Errorf("create private spool: %w", err))
	}
	defer func() {
		_ = spool.Close()
		_ = os.Remove(spool.Name())
	}()

	hasher := sha256.New()
	n, err := io.CopyN(io.MultiWriter(spool, hasher), &contextReader{ctx: ctx, source: stream}, job.SourceSize)
	if err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
				fail("source_mismatch", false, errors.New("truncated remote object"))
		}
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("internal_error", true, fmt.Errorf("stream quarantine bytes: %w", err))
	}
	if n != job.SourceSize {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("source_mismatch", false, errors.New("quarantine byte count changed"))
	}
	var extra [1]byte
	extraN, extraErr := stream.Read(extra[:])
	if extraN > 0 {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("source_mismatch", false, errors.New("quarantine response has trailing bytes"))
	}
	if extraErr != io.EOF {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("internal_error", true, errors.New("quarantine response did not confirm EOF"))
	}
	if subtle.ConstantTimeCompare(expectedSHA, hasher.Sum(nil)) != 1 {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("source_mismatch", false, errors.New("downloaded content SHA-256 mismatch"))
	}
	if err := ctx.Err(); err != nil {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{}, fail("internal_error", true, err)
	}

	var manifest validator.Manifest
	if job.SourceType == "html" {
		manifest, err = validator.ValidateHTML(ctx, spool, job.SourceSize, p.config.MaxHTMLBytes)
	} else {
		manifest, err = validator.ValidateZIP(ctx, spool, job.SourceSize, p.config.ValidationLimits)
	}
	if err != nil {
		var rejected *validator.ValidationError
		if errors.As(err, &rejected) && rejected.Code != validator.CodeCanceled {
			return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
				fail("invalid_content", false, err)
		}
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("internal_error", true, err)
	}

	before, err := p.engine.Probe(ctx)
	if err != nil {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("scanner_unavailable", true, fmt.Errorf("clamd probe before scan: %w", err))
	}
	summary, err := scanner.ScanManifest(ctx, spool, job.SourceSize, job.SourceType, manifest, p.engine)
	if err != nil {
		var entryErr *scanner.EntryError
		if errors.As(err, &entryErr) && entryErr.Reason == "infected" {
			return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
				fail("malware_detected", false, errors.New("malware was reported by scanner"))
		}
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("scanner_incomplete", true, err)
	}
	after, err := p.engine.Probe(ctx)
	if err != nil {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("scanner_unavailable", true, fmt.Errorf("clamd probe after scan: %w", err))
	}
	if before != after {
		return validator.Manifest{}, scanner.Summary{}, scanner.Evidence{},
			fail("scanner_incomplete", true, errors.New("daemon/version changed during scan"))
	}
	return manifest, summary, after, nil
}

type contextReader struct {
	ctx    context.Context
	source io.Reader
}

func (r *contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.source.Read(b)
}

// Run polls the private validation queue until cancellation. A transient
// database or scanning failure never changes an artifact publication status.
func (p *Processor) Run(ctx context.Context) error {
	if p == nil || ctx == nil {
		return errors.New("worker runner/context is missing")
	}
	ticker := time.NewTicker(p.config.PollInterval)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		processed, err := p.ProcessNext(ctx)
		if err != nil {
			p.logger.Error("validation iteration failed", "error", err)
		}
		if processed && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
