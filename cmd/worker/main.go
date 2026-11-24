// Command worker runs one fail-closed private validation consumer.
// It does not publish sites or expose HTTP ports.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/1998x-stack/visual-platform/internal/scanner/clamd"
	"github.com/1998x-stack/visual-platform/internal/storage/minio"
	"github.com/1998x-stack/visual-platform/internal/store/postgres"
	"github.com/1998x-stack/visual-platform/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	once := flag.Bool("once", false, "validate at most one queued upload and exit")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *once, logger); err != nil {
		logger.Error("validation worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, once bool, logger *slog.Logger) error {
	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	socket := strings.TrimSpace(os.Getenv("CLAMD_SOCKET"))
	spoolDir := strings.TrimSpace(os.Getenv("WORKER_TEMP_DIR"))
	endpoint := strings.TrimSpace(os.Getenv("S3_ENDPOINT"))
	bucket := strings.TrimSpace(os.Getenv("S3_BUCKET"))
	accessKey := os.Getenv("WORKER_S3_ACCESS_KEY")
	secretKey := os.Getenv("WORKER_S3_SECRET_KEY")
	if dbURL == "" || socket == "" || spoolDir == "" || endpoint == "" ||
		bucket == "" || accessKey == "" || secretKey == "" {
		return errors.New("DATABASE_URL, CLAMD_SOCKET, WORKER_TEMP_DIR, S3_ENDPOINT, S3_BUCKET and dedicated WORKER_S3_* credentials are required")
	}
	secure, err := strconv.ParseBool(envOr("S3_USE_SSL", "true"))
	if err != nil {
		return fmt.Errorf("parse S3_USE_SSL: %w", err)
	}
	devHTTP := !secure && isDevelopmentLoopback(endpoint)
	if !secure && !devHTTP {
		return errors.New("worker non-TLS object reads require APP_ENV=development and a loopback S3 endpoint")
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return fmt.Errorf("open PostgreSQL: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	pingCtx, pingCancel := context.WithTimeout(ctx, 8*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	reader, err := minio.NewReader(minio.ReaderConfig{
		Endpoint: endpoint, AccessKey: accessKey, SecretKey: secretKey,
		Bucket: bucket, Region: os.Getenv("S3_REGION"), Secure: secure,
		AllowInsecureDevelopment: devHTTP,
	})
	if err != nil {
		return err
	}
	daemon, err := clamd.New(clamd.Config{
		SocketPath: socket, Timeout: 45 * time.Second, MaxStreamBytes: 64 << 20,
	})
	if err != nil {
		return err
	}
	healthCtx, healthCancel := context.WithTimeout(ctx, 10*time.Second)
	defer healthCancel()
	evidence, err := daemon.Probe(healthCtx)
	if err != nil {
		return fmt.Errorf("clamd version/signature freshness check: %w", err)
	}
	logger.Info("clamd verified", "engine", evidence.EngineVersion,
		"signatures", evidence.SignatureVersion, "signature_time", evidence.SignaturesUpdatedAt)
	processor, err := worker.NewProcessor(&postgres.Store{DB: db}, reader, daemon,
		worker.Config{TempDir: spoolDir}, logger)
	if err != nil {
		return err
	}
	if once {
		processed, err := processor.ProcessNext(ctx)
		if err == nil {
			logger.Info("worker single iteration complete", "processed", processed)
		}
		return err
	}
	logger.Info("validation worker started", "mode", "single-consumer")
	return processor.Run(ctx)
}

func isDevelopmentLoopback(endpoint string) bool {
	if os.Getenv("APP_ENV") != "development" {
		return false
	}
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
