package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
)

type fakeUploadRepo struct {
	session  domain.UploadSession
	status   string
	rejected string
}

func (f *fakeUploadRepo) CreateUploadSession(_ context.Context, actorID, artifactID, name, kind string, size int64, checksum string) (domain.UploadSession, error) {
	f.session = domain.UploadSession{
		ID: "00000000-0000-4000-8000-000000000099", WorkspaceID: "00000000-0000-4000-8000-000000000088",
		ArtifactID: artifactID, CreatedBy: actorID, SourceName: name, SourceType: kind,
		ExpectedSize: size, ExpectedSHA256: checksum, Status: "created",
		ObjectKey: "quarantine/workspace/artifact/upload/source",
		ExpiresAt: time.Now().Add(30 * time.Minute), CreatedAt: time.Now(),
	}
	f.status = "created"
	return f.session, nil
}
func (f *fakeUploadRepo) GetUploadSession(_ context.Context, id, actorID string) (domain.UploadSession, error) {
	if id != f.session.ID || actorID != f.session.CreatedBy {
		return domain.UploadSession{}, errors.New("not found")
	}
	f.session.Status = f.status
	return f.session, nil
}
func (f *fakeUploadRepo) ClaimUpload(_ context.Context, id, actorID string) (domain.UploadSession, error) {
	if id != f.session.ID || actorID != f.session.CreatedBy || f.status != "created" {
		return domain.UploadSession{}, errors.New("not found")
	}
	f.status = "receiving"
	f.session.Status = f.status
	return f.session, nil
}
func (f *fakeUploadRepo) ResetUploadClaim(context.Context, string, string) error {
	f.status = "created"
	return nil
}
func (f *fakeUploadRepo) MarkUploadReceived(_ context.Context, id, actorID string, size int64, checksum string) error {
	if id != f.session.ID || actorID != f.session.CreatedBy || f.status != "receiving" {
		return errors.New("invalid state")
	}
	f.status = "received"
	f.session.Status = f.status
	f.session.ActualSize = size
	f.session.ActualSHA256 = checksum
	return nil
}
func (f *fakeUploadRepo) MarkUploadRejected(_ context.Context, id, actorID, reason string) error {
	if id != f.session.ID || actorID != f.session.CreatedBy || f.status != "receiving" {
		return errors.New("invalid state")
	}
	f.status = "rejected"
	f.rejected = reason
	f.session.Status = f.status
	return nil
}

type fakeQuarantine struct {
	body    []byte
	deleted bool
	putErr  error
}

func (f *fakeQuarantine) Put(_ context.Context, key string, body io.Reader, size int64) error {
	if f.putErr != nil {
		return f.putErr
	}
	if !strings.HasPrefix(key, "quarantine/") {
		return errors.New("invalid key")
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return errors.New("size mismatch")
	}
	f.body = b
	return nil
}
func (f *fakeQuarantine) Delete(context.Context, string) error { f.deleted = true; return nil }

func uploadTestHandler(repo *fakeUploadRepo, store *fakeQuarantine) http.Handler {
	return (&Server{
		Repo: &fakeRepo{active: true}, Uploads: repo, Quarantine: store,
		MaxUploadBytes: 1024, DevAuth: true,
	}).Handler()
}
func setDevIdentity(r *http.Request) {
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("X-Dev-User-ID", "00000000-0000-4000-8000-000000000001")
}
func TestUploadSessionAndQuarantineStream(t *testing.T) {
	payload := []byte("<html><body>safe test</body></html>")
	sum := sha256.Sum256(payload)
	checksum := hex.EncodeToString(sum[:])
	repo := &fakeUploadRepo{}
	store := &fakeQuarantine{}
	h := uploadTestHandler(repo, store)
	artifactID := "00000000-0000-4000-8000-000000000010"
	createBody, _ := json.Marshal(map[string]any{
		"source_name": "dashboard.html", "source_type": "html",
		"expected_size": len(payload), "expected_sha256": checksum,
	})
	create := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artifactID+"/uploads", bytes.NewReader(createBody))
	setDevIdentity(create)
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	if strings.Contains(created.Body.String(), "object_key") {
		t.Fatal("internal object key leaked in API response")
	}
	content := httptest.NewRequest(http.MethodPut, "/api/v1/uploads/"+repo.session.ID+"/content", bytes.NewReader(payload))
	setDevIdentity(content)
	content.Header.Set("Content-Type", "application/octet-stream")
	received := httptest.NewRecorder()
	h.ServeHTTP(received, content)
	if received.Code != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s", received.Code, received.Body.String())
	}
	if repo.status != "received" || !bytes.Equal(store.body, payload) {
		t.Fatalf("upload state=%s body=%q", repo.status, store.body)
	}
}

func TestChecksumMismatchIsRejectedAndDeleted(t *testing.T) {
	payload := []byte("malicious or corrupted bytes")
	repo := &fakeUploadRepo{}
	store := &fakeQuarantine{}
	h := uploadTestHandler(repo, store)
	artifactID := "00000000-0000-4000-8000-000000000010"
	createBody, _ := json.Marshal(map[string]any{
		"source_name": "site.zip", "source_type": "zip",
		"expected_size": len(payload), "expected_sha256": strings.Repeat("0", 64),
	})
	create := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artifactID+"/uploads", bytes.NewReader(createBody))
	setDevIdentity(create)
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Code, created.Body.String())
	}
	content := httptest.NewRequest(http.MethodPut, "/api/v1/uploads/"+repo.session.ID+"/content", bytes.NewReader(payload))
	setDevIdentity(content)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, content)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if repo.status != "rejected" || !store.deleted {
		t.Fatalf("state=%s deleted=%v", repo.status, store.deleted)
	}
}

func TestUploadContentLengthMismatchDoesNotClaimSession(t *testing.T) {
	repo := &fakeUploadRepo{}
	store := &fakeQuarantine{}
	h := uploadTestHandler(repo, store)
	artifactID := "00000000-0000-4000-8000-000000000010"
	body := []byte("hello world")
	sum := sha256.Sum256(body)
	createBody, _ := json.Marshal(map[string]any{
		"source_name": "index.html", "source_type": "html",
		"expected_size": int64(20), "expected_sha256": hex.EncodeToString(sum[:]),
	})
	create := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artifactID+"/uploads", bytes.NewReader(createBody))
	setDevIdentity(create)
	create.Header.Set("Content-Type", "application/json")
	created := httptest.NewRecorder()
	h.ServeHTTP(created, create)
	if created.Code != http.StatusCreated {
		t.Fatal(created.Code, created.Body.String())
	}
	content := httptest.NewRequest(http.MethodPut, "/api/v1/uploads/"+repo.session.ID+"/content", bytes.NewReader(body))
	setDevIdentity(content)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, content)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if repo.status != "created" || len(store.body) != 0 {
		t.Fatalf("session was claimed: %s", repo.status)
	}
}

func TestUploadFilenameAndTypeAllowlist(t *testing.T) {
	cases := []struct {
		name, kind string
		want       bool
	}{
		{"index.html", "html", true}, {"report.htm", "html", true}, {"site.zip", "zip", true},
		{"../index.html", "html", false}, {"C:\\temp\\index.html", "html", false},
		{"index.html.zip", "html", false}, {"index.php", "zip", false}, {"", "html", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validUploadName(tc.name, tc.kind); got != tc.want {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestUploadUnavailableWithoutPrivateStorage(t *testing.T) {
	h := (&Server{Repo: &fakeRepo{active: true}, DevAuth: true}).Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/00000000-0000-4000-8000-000000000010/uploads", strings.NewReader(`{}`))
	setDevIdentity(req)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
