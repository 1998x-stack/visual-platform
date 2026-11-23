package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/1998x-stack/visual-platform/internal/domain"
	"github.com/1998x-stack/visual-platform/internal/requestmeta"
)

func TestM2UploadQuotaIsolationAndStateTransitions(t *testing.T) {
	url := os.Getenv("DB_TEST_URL")
	if url == "" {
		t.Skip("DB_TEST_URL not set; PostgreSQL integration test skipped")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	ctx = requestmeta.WithID(ctx, "integration-m2-upload")
	s := &Store{DB: db}
	seed := func(name string) string {
		t.Helper()
		var id string
		subject := fmt.Sprintf("m2:%s:%d", name, time.Now().UnixNano())
		if err := db.QueryRowContext(ctx,
			`INSERT INTO users(oidc_subject,display_name) VALUES($1,$2) RETURNING id::text`,
			subject, name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner := seed("m2-owner")
	viewer := seed("m2-viewer")
	w, err := s.CreatePersonalWorkspace(ctx, owner, "M2 Quarantine")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateArtifact(ctx, w.ID, owner, "quarantine-demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, w.ID, owner, viewer, domain.Viewer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUploadSession(ctx, viewer, a.ID, "viewer-upload-0001", "site.zip", "zip", 1, strings.Repeat("a", 64)); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer must not upload: %v", err)
	}
	if _, err := s.CreateUploadSession(ctx, seed("m2-outsider"), a.ID, "outsider-upload-01", "site.zip", "zip", 1, strings.Repeat("a", 64)); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-workspace upload must fail: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE workspace_quotas SET max_bytes=10,max_single_upload_bytes=10,max_active_uploads=1 WHERE workspace_id=$1::uuid`, w.ID); err != nil {
		t.Fatal(err)
	}

	first, err := s.CreateUploadSession(ctx, owner, a.ID, "first-upload-0001", "dashboard.zip", "zip", 8, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first.ObjectKey, "quarantine/") || strings.Contains(first.ObjectKey, "dashboard.zip") {
		t.Fatalf("object key must be generated, not derived from filename: %s", first.ObjectKey)
	}
	// A retry with the same key and payload returns the original session and
	// does not reserve the same bytes twice.
	retry, err := s.CreateUploadSession(ctx, owner, a.ID, "first-upload-0001", "dashboard.zip", "zip", 8, strings.Repeat("a", 64))
	if err != nil || retry.ID != first.ID {
		t.Fatalf("idempotent retry=%+v first=%+v err=%v", retry, first, err)
	}
	if _, err := s.CreateUploadSession(ctx, owner, a.ID, "first-upload-0001", "different.zip", "zip", 8, strings.Repeat("a", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("same idempotency key with a different payload must conflict: %v", err)
	}
	if _, err := s.CreateUploadSession(ctx, owner, a.ID, "too-large-upload-01", "too-large.zip", "zip", 3, strings.Repeat("b", 64)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("reserved quota must prevent overcommit: %v", err)
	}
	if _, err := s.ClaimUpload(ctx, first.ID, viewer); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another member must not claim the uploader's session: %v", err)
	}
	claimed, err := s.ClaimUpload(ctx, first.ID, owner)
	if err != nil || claimed.Status != "receiving" {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if err := s.MarkUploadReceived(ctx, first.ID, owner, 8, strings.Repeat("b", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("wrong checksum must not finalize: %v", err)
	}
	if err := s.MarkUploadReceived(ctx, first.ID, owner, 8, strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetUploadSession(ctx, first.ID, owner)
	if err != nil || got.Status != "received" || got.ActualSize != 8 || got.ActualSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("received session=%+v err=%v", got, err)
	}
	var used, reserved int64
	if err := db.QueryRowContext(ctx,
		`SELECT used_bytes,reserved_bytes FROM workspace_quotas WHERE workspace_id=$1::uuid`, w.ID).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if used != 8 || reserved != 0 {
		t.Fatalf("quota after receive used=%d reserved=%d", used, reserved)
	}
	if _, err := s.CreateUploadSession(ctx, owner, a.ID, "still-large-upload-1", "still-too-large.zip", "zip", 3, strings.Repeat("c", 64)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("used bytes must count against quota: %v", err)
	}

	second, err := s.CreateUploadSession(ctx, owner, a.ID, "reject-upload-0001", "reject.zip", "zip", 2, strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimUpload(ctx, second.ID, owner); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUploadRejected(ctx, second.ID, owner, "checksum_mismatch"); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetUploadSession(ctx, second.ID, owner)
	if err != nil || got.Status != "rejected" || got.FailureCode != "checksum_mismatch" {
		t.Fatalf("rejected=%+v err=%v", got, err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT used_bytes,reserved_bytes FROM workspace_quotas WHERE workspace_id=$1::uuid`, w.ID).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if used != 8 || reserved != 0 {
		t.Fatalf("quota after reject used=%d reserved=%d", used, reserved)
	}

	expiring, err := s.CreateUploadSession(ctx, owner, a.ID, "expire-upload-0001", "expire.zip", "zip", 1, strings.Repeat("e", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE upload_sessions SET expires_at=now()-interval '1 second' WHERE id=$1::uuid`, expiring.ID); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.CreateUploadSession(ctx, owner, a.ID, "replace-upload-0001", "replacement.zip", "zip", 1, strings.Repeat("f", 64))
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetUploadSession(ctx, expiring.ID, owner)
	if err != nil || got.Status != "expired" {
		t.Fatalf("expired=%+v err=%v", got, err)
	}
	if _, err := s.CreateUploadSession(ctx, owner, a.ID, "parallel-upload-0001", "parallel.zip", "zip", 1, strings.Repeat("1", 64)); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("max active upload count must be enforced: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT used_bytes,reserved_bytes FROM workspace_quotas WHERE workspace_id=$1::uuid`, w.ID).Scan(&used, &reserved); err != nil {
		t.Fatal(err)
	}
	if used != 8 || reserved != 1 {
		t.Fatalf("quota after expiry used=%d reserved=%d", used, reserved)
	}
	if replacement.Status != "created" {
		t.Fatalf("replacement status=%s", replacement.Status)
	}
}

func TestValidUploadInput(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	cases := []struct {
		name, kind, digest string
		size int64
		want bool
	}{
		{"site.html", "html", checksum, 10, true},
		{"site.htm", "html", checksum, 10, true},
		{"site.zip", "zip", checksum, 10, true},
		{"site.php", "html", checksum, 10, false},
		{"site.html.zip", "html", checksum, 10, false},
		{"../site.html", "html", checksum, 10, false},
		{ "C:\\temp\\site.zip", "zip", checksum, 10, false},
		{"site.zip", "zip", strings.ToUpper(checksum), 10, true},
		{"site.zip", "zip", checksum, 0, false},
		{"site.zip", "zip", "not-a-sha", 10, false},
	}
	for _, tc := range cases {
		t.Run(tc.name+"-"+tc.kind, func(t *testing.T) {
			if got := validUploadInput(tc.name, tc.kind, tc.size, tc.digest); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestValidIdempotencyKey(t *testing.T) {
	for _, tc := range []struct{ key string; want bool }{
		{"request-0001",true},{"upload.v1:request_1",true},
		{"short",false},{"has space-0001",false},{"slash/key-0001",false},
		{strings.Repeat("a",129),false},
	} {
		if got:=validIdempotencyKey(tc.key);got!=tc.want {
			t.Errorf("validIdempotencyKey(%q)=%v want %v",tc.key,got,tc.want)
		}
	}
}
