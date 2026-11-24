package worker

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/1998x-stack/visual-platform/internal/domain"
	"github.com/1998x-stack/visual-platform/internal/scanner"
	"github.com/1998x-stack/visual-platform/internal/store/postgres"
)

type fakeRepo struct {
	job        domain.ValidationJob
	claimed    bool
	completion *postgres.ValidationCompletion
	failCode   string
	retryable  bool
	enqueue    int
	claimErr   error
	writeErr   error
}

func (r *fakeRepo) EnqueueReceivedBatch(_ context.Context, limit int) (int, error) {
	r.enqueue++
	if limit < 1 || limit > 100 { return 0, errors.New("invalid batch") }
	return 0, nil
}
func (r *fakeRepo) ClaimNextValidation(_ context.Context) (domain.ValidationJob, error) {
	if r.claimErr != nil { return domain.ValidationJob{}, r.claimErr }
	if r.claimed { return domain.ValidationJob{}, postgres.ErrNotFound }
	r.claimed = true
	return r.job, nil
}
func (r *fakeRepo) CompleteValidation(_ context.Context, result postgres.ValidationCompletion) error {
	if r.writeErr != nil { return r.writeErr }
	r.completion = &result
	return nil
}
func (r *fakeRepo) FinishValidationFailure(_ context.Context, _, _, code string, retryable bool) (domain.ValidationStatus, error) {
	if r.writeErr != nil { return "", r.writeErr }
	r.failCode, r.retryable = code, retryable
	if retryable { return domain.ValidationRetryable, nil }
	return domain.ValidationRejected, nil
}

type fakeReader struct {
	data []byte
	opened int
	fail error
}
func (r *fakeReader) Open(_ context.Context, objectKey string, _ int64) (io.ReadCloser, error) {
	r.opened++
	if r.fail != nil { return nil, r.fail }
	if objectKey != "quarantine/test/source" { return nil, errors.New("unexpected object key") }
	return io.NopCloser(bytes.NewReader(r.data)), nil
}

type fakeEngine struct {
	verdict scanner.Verdict
	scanErr error
	scanned [][]byte
	probes int
	changed bool
	signatureAt time.Time
}
func (e *fakeEngine) Scan(_ context.Context, src io.Reader, size int64) (scanner.Result, error) {
	if e.scanErr != nil { return scanner.Result{}, e.scanErr }
	data, err := io.ReadAll(src)
	if err != nil { return scanner.Result{}, err }
	if int64(len(data)) != size { return scanner.Result{}, io.ErrUnexpectedEOF }
	e.scanned = append(e.scanned, data)
	if e.verdict == scanner.VerdictInfected {
		return scanner.Result{Verdict: scanner.VerdictInfected, Signature: "test-signature"}, nil
	}
	return scanner.Result{Verdict: scanner.VerdictClean}, nil
}
func (e *fakeEngine) Probe(_ context.Context) (scanner.Evidence, error) {
	e.probes++
	if e.signatureAt.IsZero() { e.signatureAt = time.Now().UTC().Add(-time.Hour).Truncate(time.Second) }
	signature := "db-30000"
	if e.changed && e.probes > 1 { signature = "db-30001" }
	return scanner.Evidence{
		EngineVersion: "clamav-1.4.3",
		SignatureVersion: signature,
		SignaturesUpdatedAt: e.signatureAt,
	}, nil
}

func setup(t *testing.T, kind string, bytesOnDisk []byte) (*Processor, *fakeRepo, *fakeReader, *fakeEngine, string) {
	t.Helper()
	hash := sha256.Sum256(bytesOnDisk)
	job := domain.ValidationJob{
		ID: "test-run", UploadID: "test-upload", LeaseToken: "test-lease",
		ObjectKey: "quarantine/test/source", SourceType: kind, SourceSize: int64(len(bytesOnDisk)),
		SourceSHA256: hex.EncodeToString(hash[:]), AttemptCount: 1,
	}
	repo := &fakeRepo{job: job}
	reader := &fakeReader{data: bytesOnDisk}
	engine := &fakeEngine{}
	dir := t.TempDir()
	p, err := NewProcessor(repo, reader, engine, Config{TempDir: dir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil { t.Fatal(err) }
	return p, repo, reader, engine, dir
}

func TestWorkerValidHTMLUsesSamePrivateSpool(t *testing.T) {
	content := []byte("<!doctype html><html>OK</html>")
	p, repo, _, engine, dir := setup(t, "html", content)
	processed, err := p.ProcessNext(context.Background())
	if err != nil || !processed { t.Fatalf("iteration=%v %v", processed, err) }
	if repo.completion == nil || repo.failCode != "" { t.Fatalf("unexpected result: %+v, %s", repo.completion, repo.failCode) }
	if len(engine.scanned) != 1 || !bytes.Equal(engine.scanned[0], content) || engine.probes != 2 {
		t.Fatalf("source bytes not fully scanned: files=%d probes=%d", len(engine.scanned), engine.probes)
	}
	if repo.completion.Manifest.Entries[0].Path != "index.html" || repo.completion.Scan.ScannedFiles != 1 {
		t.Fatalf("invalid evidence: %+v", repo.completion)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 { t.Fatalf("private spool not removed: %+v %v", entries, err) }
	if repo.enqueue != 1 { t.Fatal("received upload reconciliation must run before claim") }
	processed, err = p.ProcessNext(context.Background())
	if processed || err != nil { t.Fatalf("empty queue returned %v %v", processed, err) }
}

func TestWorkerZIPFilesAreIndividuallyScanned(t *testing.T) {
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for name, body := range map[string]string{"dist/index.html": "<html>ok</html>", "dist/assets/app.js": "console.log('ok')"} {
		f, err := w.Create(name)
		if err != nil { t.Fatal(err) }
		if _, err := io.WriteString(f, body); err != nil { t.Fatal(err) }
	}
	if err := w.Close(); err != nil { t.Fatal(err) }
	p, repo, _, engine, _ := setup(t, "zip", out.Bytes())
	processed, err := p.ProcessNext(context.Background())
	if err != nil || !processed { t.Fatalf("ZIP result=%v %v", processed, err) }
	if repo.completion == nil || repo.completion.Scan.ScannedFiles != 2 ||
		repo.completion.Manifest.StrippedRoot != "dist" || len(engine.scanned) != 2 {
		t.Fatalf("ZIP scan bypassed entries: completion=%+v scans=%d", repo.completion, len(engine.scanned))
	}
}

func TestWorkerClassifiesFailuresWithoutGrantingSuccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		change func(*fakeRepo,*fakeReader,*fakeEngine)
		code string
		retryable bool
	}{
		{name: "changed object hash", change: func(_ *fakeRepo, r *fakeReader, _ *fakeEngine) {
			r.data = []byte("<html>corrupted</html>")
		}, code: "source_mismatch"},
		{name: "short object", change: func(_ *fakeRepo, r *fakeReader, _ *fakeEngine) {
			r.data = []byte("short")
		}, code: "source_mismatch"},
		{name: "extra object bytes", change: func(_ *fakeRepo, r *fakeReader, _ *fakeEngine) {
			r.data = append(r.data, 'X')
		}, code: "source_mismatch"},
		{name: "object backend failure", change: func(_ *fakeRepo, r *fakeReader, _ *fakeEngine) {
			r.fail = errors.New("S3 not available")
		}, code: "internal_error", retryable: true},
		{name: "infected", change: func(_ *fakeRepo, _ *fakeReader, e *fakeEngine) {
			e.verdict = scanner.VerdictInfected
		}, code: "malware_detected"},
		{name: "scanner unavailable", change: func(_ *fakeRepo, _ *fakeReader, e *fakeEngine) {
			e.scanErr = errors.New("clamd socket closed")
		}, code: "scanner_incomplete", retryable: true},
		{name: "scanner version changed", change: func(_ *fakeRepo, _ *fakeReader, e *fakeEngine) {
			e.changed = true
		}, code: "scanner_incomplete", retryable: true},
		{name: "unsupported source type", change: func(r *fakeRepo, _ *fakeReader, _ *fakeEngine) {
			r.job.SourceType = "php"
		}, code: "invalid_content"},
		{name: "declared HTML size over limit", change: func(r *fakeRepo, _ *fakeReader, _ *fakeEngine) {
			r.job.SourceSize = 11 << 20
		}, code: "invalid_content"},
	} {
		t.Run(tc.name,func(t *testing.T) {
			p, repo, reader, engine, dir := setup(t, "html", []byte("<html>test</html>"))
			tc.change(repo,reader,engine)
			processed,err:=p.ProcessNext(context.Background())
			if err!=nil || !processed || repo.completion!=nil ||
				repo.failCode!=tc.code || repo.retryable!=tc.retryable {
				t.Fatalf("expected %s/%v got processed=%v err=%v completion=%+v failure=%s/%v",
					tc.code,tc.retryable,processed,err,repo.completion,repo.failCode,repo.retryable)
			}
			entries,err:=os.ReadDir(dir)
			if err!=nil || len(entries)!=0 { t.Fatalf("spool leak: %d %v",len(entries),err) }
		})
	}
}

func TestWorkerRejectsInvalidZIP(t *testing.T) {
	p,repo,_,_,_:=setup(t,"zip",[]byte("this is not a ZIP archive"))
	_,err:=p.ProcessNext(context.Background())
	if err!=nil || repo.failCode!="invalid_content" || repo.completion!=nil {
		t.Fatalf("invalid ZIP was accepted: %v, %+v",err,repo.completion)
	}
}

func TestWorkerFailurePersistenceMustNotBeSilenced(t *testing.T) {
	p,repo,reader,_,_:=setup(t,"html",[]byte("<html>test</html>"))
	reader.fail=errors.New("storage unavailable")
	repo.writeErr=errors.New("PostgreSQL unavailable")
	if processed,err:=p.ProcessNext(context.Background()); !processed || err==nil {
		t.Fatalf("lost rejection update: %v %v",processed,err)
	}
}

func TestPrivateSpoolDirectoryPermissions(t *testing.T) {
	dir:=t.TempDir()
	r:=&fakeRepo{}
	reader:=&fakeReader{}
	engine:=&fakeEngine{}
	if err:=os.Chmod(dir,0755);err!=nil { t.Fatal(err) }
	_,err:=NewProcessor(r,reader,engine,Config{TempDir:dir},nil)
	if err==nil { t.Fatal("world-readable spool directory was accepted") }
	if err:=os.Chmod(dir,0700);err!=nil { t.Fatal(err) }
	if _,err:=NewProcessor(r,reader,engine,Config{TempDir:dir},nil);err!=nil { t.Fatal(err) }
	symlink:=filepath.Join(t.TempDir(),"alias")
	if err:=os.Symlink(dir,symlink);err!=nil { t.Fatal(err) }
	if _,err:=NewProcessor(r,reader,engine,Config{TempDir:symlink},nil);err==nil { t.Fatal("symlink spool accepted") }
}

func TestWorkerProcessTimeoutMustBeShorterThanDBLease(t *testing.T) {
	p,repo,reader,engine,dir:=setup(t,"html",[]byte("<html>test</html>"))
	_ = p
	if _,err:=NewProcessor(repo,reader,engine,Config{TempDir:dir,JobTimeout:5*time.Minute},nil);err==nil {
		t.Fatal("job timeout >= DB lease accepted")
	}
	if _,err:=NewProcessor(repo,reader,engine,Config{TempDir:dir,MaxHTMLBytes:200<<20},nil);err==nil {
		t.Fatal("HTML limit > source limit accepted")
	}
}

func TestWorkerInvalidSourceSHARejected(t *testing.T) {
	p,repo,_,_,_:=setup(t,"html",[]byte("<html>test</html>"))
	repo.job.SourceSHA256=strings.Repeat("G",64)
	_,err:=p.ProcessNext(context.Background())
	if err!=nil || repo.failCode!="source_mismatch" { t.Fatalf("bad SHA accepted: %v %s",err,repo.failCode) }
}
