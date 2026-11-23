package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/1998x-stack/visual-platform/internal/httpapi"
	"github.com/1998x-stack/visual-platform/internal/store/postgres"
	"github.com/1998x-stack/visual-platform/internal/storage"
	minioStorage "github.com/1998x-stack/visual-platform/internal/storage/minio"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	var listen string
	flag.StringVar(&listen, "listen", getenv("LISTEN_ADDR", "127.0.0.1:8071"), "HTTP bind address")
	flag.Parse()
	dbURL := os.Getenv("DATABASE_URL")
	if strings.TrimSpace(dbURL) == "" {
		log.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	devAuth := os.Getenv("DEV_AUTH") == "true"
	if devAuth {
		host, _, err := net.SplitHostPort(listen)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() || os.Getenv("APP_ENV") != "development" {
			log.Error("DEV_AUTH requires APP_ENV=development and an explicit loopback LISTEN_ADDR")
			os.Exit(2)
		}
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Error("opening database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	db.SetMaxOpenConns(12)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = db.PingContext(ctx)
	cancel()
	if err != nil {
		log.Error("database not ready", "error", err)
		os.Exit(1)
	}
	store := &postgres.Store{DB: db}
	var quarantine storage.QuarantineStore
	endpoint := os.Getenv("S3_ENDPOINT")
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	bucket := os.Getenv("S3_BUCKET")
	if endpoint != "" || accessKey != "" || secretKey != "" || bucket != "" {
		if endpoint == "" || accessKey == "" || secretKey == "" || bucket == "" {
			log.Error("S3_ENDPOINT, S3_ACCESS_KEY, S3_SECRET_KEY and S3_BUCKET must be configured together")
			os.Exit(2)
		}
		secure, parseErr := strconv.ParseBool(getenv("S3_USE_SSL", "false"))
		if parseErr != nil {
			log.Error("invalid S3_USE_SSL", "error", parseErr)
			os.Exit(2)
		}
		storageCtx, storageCancel := context.WithTimeout(context.Background(), 8*time.Second)
		quarantine, err = minioStorage.New(storageCtx, minioStorage.Config{
			Endpoint: endpoint, AccessKey: accessKey, SecretKey: secretKey,
			Bucket: bucket, Region: os.Getenv("S3_REGION"), Secure: secure,
		})
		storageCancel()
		if err != nil {
			log.Error("quarantine storage not ready", "error", err)
			os.Exit(1)
		}
	}
	maxUploadBytes := int64(100 << 20)
	if raw := os.Getenv("MAX_UPLOAD_BYTES"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || parsed <= 0 {
			log.Error("invalid MAX_UPLOAD_BYTES")
			os.Exit(2)
		}
		maxUploadBytes = parsed
	}
	handler := (&httpapi.Server{
		Repo: store, Uploads: store, Quarantine: quarantine,
		MaxUploadBytes: maxUploadBytes, Logger: log, DevAuth: devAuth,
	}).Handler()
	srv := &http.Server{
		Addr: listen, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Minute,
		WriteTimeout: 5 * time.Minute, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}
	stop, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-stop.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Error("shutdown", "error", err)
		}
	}()
	log.Info("api starting", "listen", listen, "development_auth", devAuth)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("api server", "error", err)
		os.Exit(1)
	}
	log.Info("api stopped")
}
func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
