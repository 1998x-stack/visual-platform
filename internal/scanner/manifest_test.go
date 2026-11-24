package scanner

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/1998x-stack/visual-platform/internal/validator"
)

type fakeEngine struct {
	scans   []string
	verdict Verdict
	lazy    bool
}

func (e *fakeEngine) Scan(_ context.Context, source io.Reader, declaredSize int64) (Result, error) {
	if e.lazy {
		return Result{Verdict: VerdictClean}, nil
	}
	data, err := io.ReadAll(source)
	if err != nil {
		return Result{}, err
	}
	if int64(len(data)) != declaredSize {
		return Result{}, io.ErrUnexpectedEOF
	}
	e.scans = append(e.scans, string(data))
	if e.verdict == VerdictInfected {
		return Result{Verdict: VerdictInfected, Signature: "test-detection"}, nil
	}
	if e.verdict == "" {
		return Result{}, nil
	}
	return Result{Verdict: e.verdict}, nil
}

func fixtureZIP(t *testing.T) []byte {
	t.Helper()
	var out bytes.Buffer
	writer := zip.NewWriter(&out)
	for _, item := range [][2]string{
		{"dist/index.html", "<html>ok</html>"},
		{"dist/assets/app.js", "console.log(1)"},
	} {
		entry, err := writer.Create(item[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, item[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestScanManifestZIPVerifiesEveryFile(t *testing.T) {
	data := fixtureZIP(t)
	manifest, err := validator.ValidateZIP(context.Background(), bytes.NewReader(data), int64(len(data)), validator.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	engine := &fakeEngine{verdict: VerdictClean}
	summary, err := ScanManifest(context.Background(), bytes.NewReader(data), int64(len(data)), "zip", manifest, engine)
	if err != nil {
		t.Fatalf("ScanManifest() failed: %v", err)
	}
	if summary.ScannedFiles != 2 || summary.ScannedBytes != manifest.TotalUncompressedBytes {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if len(engine.scans) != 2 || engine.scans[0] != "console.log(1)" || engine.scans[1] != "<html>ok</html>" {
		t.Fatalf("files not scanned in manifest order: %+v", engine.scans)
	}
}

func TestScanManifestHTML(t *testing.T) {
	data := []byte("<!doctype html><html><body>ready</body></html>")
	manifest, err := validator.ValidateHTML(context.Background(), bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	engine := &fakeEngine{verdict: VerdictClean}
	summary, err := ScanManifest(context.Background(), bytes.NewReader(data), int64(len(data)), "html", manifest, engine)
	if err != nil {
		t.Fatalf("ScanManifest() failed: %v", err)
	}
	if summary.ScannedFiles != 1 || len(engine.scans) != 1 || engine.scans[0] != string(data) {
		t.Fatalf("unexpected HTML summary or scan content: %+v %+v", summary, engine.scans)
	}
}

func TestScanManifestFailsClosed(t *testing.T) {
	data := fixtureZIP(t)
	manifest, err := validator.ValidateZIP(context.Background(), bytes.NewReader(data), int64(len(data)), validator.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		modify func(*validator.Manifest)
		engine *fakeEngine
	}{
		{name: "malformed manifest hash", modify: func(m *validator.Manifest) { m.Entries[0].SHA256 = strings.Repeat("0", 64) }, engine: &fakeEngine{verdict: VerdictClean}},
		{name: "incorrect source mapping", modify: func(m *validator.Manifest) { m.Entries[0].SourcePath = "missing.js" }, engine: &fakeEngine{verdict: VerdictClean}},
		{name: "duplicate publish path", modify: func(m *validator.Manifest) { m.Entries[1].Path = m.Entries[0].Path }, engine: &fakeEngine{verdict: VerdictClean}},
		{name: "lazy scanner", modify: func(m *validator.Manifest) {}, engine: &fakeEngine{lazy: true}},
		{name: "unknown verdict", modify: func(m *validator.Manifest) {}, engine: &fakeEngine{}},
		{name: "virus detected", modify: func(m *validator.Manifest) {}, engine: &fakeEngine{verdict: VerdictInfected}},
		{name: "inconsistent size", modify: func(m *validator.Manifest) { m.Entries[0].Size++ }, engine: &fakeEngine{verdict: VerdictClean}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copyManifest := manifest
			copyManifest.Entries = append([]validator.ManifestEntry(nil), manifest.Entries...)
			tc.modify(&copyManifest)
			result, err := ScanManifest(context.Background(), bytes.NewReader(data), int64(len(data)), "zip", copyManifest, tc.engine)
			if err == nil || result.ScannedFiles != 0 {
				t.Fatalf("expected fail closed result; got %+v, %v", result, err)
			}
		})
	}
}

func TestScanManifestCanceledContext(t *testing.T) {
	data := []byte("<html></html>")
	manifest, err := validator.ValidateHTML(context.Background(), bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanManifest(ctx, bytes.NewReader(data), int64(len(data)), "html", manifest, &fakeEngine{verdict: VerdictClean}); err == nil {
		t.Fatal("canceled scan did not fail")
	}
}
