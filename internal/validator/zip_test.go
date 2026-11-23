package validator

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io/fs"
	"strings"
	"testing"
)

type testEntry struct {
	name string
	body string
	mode fs.FileMode
}

func makeZIP(t *testing.T, entries ...testEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		fileWriter, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create ZIP entry %q: %v", entry.name, err)
		}
		if _, err := fileWriter.Write([]byte(entry.body)); err != nil {
			t.Fatalf("write ZIP entry %q: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close ZIP: %v", err)
	}
	return buffer.Bytes()
}

func validateBytes(data []byte, limits Limits) (Manifest, error) {
	return ValidateZIP(context.Background(), bytes.NewReader(data), int64(len(data)), limits)
}

func validationCode(t *testing.T, err error) ErrorCode {
	t.Helper()
	if err == nil {
		t.Fatal("expected validation error")
	}
	validationErr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	return validationErr.Code
}

func TestValidateZIPBuildsDeterministicManifest(t *testing.T) {
	data := makeZIP(t,
		testEntry{name: "dist/index.html", body: "<html>ok</html>"},
		testEntry{name: "dist/assets/app.js", body: "console.log('ok')"},
		testEntry{name: "dist/assets/site.css", body: "body{margin:0}"},
	)

	manifest, err := validateBytes(data, DefaultLimits())
	if err != nil {
		t.Fatalf("ValidateZIP() error = %v", err)
	}
	if manifest.SchemaVersion != 1 {
		t.Fatalf("SchemaVersion = %d, want 1", manifest.SchemaVersion)
	}
	if manifest.IndexFile != "index.html" {
		t.Fatalf("IndexFile = %q, want index.html", manifest.IndexFile)
	}
	if manifest.StrippedRoot != "dist" {
		t.Fatalf("StrippedRoot = %q, want dist", manifest.StrippedRoot)
	}
	if manifest.EntryCount != 3 || len(manifest.Entries) != 3 {
		t.Fatalf("EntryCount = %d, entries = %d, want 3", manifest.EntryCount, len(manifest.Entries))
	}
	if manifest.Entries[0].Path != "assets/app.js" ||
		manifest.Entries[1].Path != "assets/site.css" ||
		manifest.Entries[2].Path != "index.html" {
		t.Fatalf("manifest entries are not sorted: %+v", manifest.Entries)
	}

	expected := sha256.Sum256([]byte("<html>ok</html>"))
	if manifest.Entries[2].SHA256 != hex.EncodeToString(expected[:]) {
		t.Fatalf("index SHA-256 = %q, want %x", manifest.Entries[2].SHA256, expected)
	}
	if manifest.Entries[2].ContentType != "text/html" {
		t.Fatalf("index content type = %q", manifest.Entries[2].ContentType)
	}
	if manifest.Entries[2].SourcePath != "dist/index.html" {
		t.Fatalf("index source path = %q, want dist/index.html", manifest.Entries[2].SourcePath)
	}
	if manifest.TotalUncompressedBytes != uint64(len("<html>ok</html>")+len("console.log('ok')")+len("body{margin:0}")) {
		t.Fatalf("unexpected uncompressed total: %d", manifest.TotalUncompressedBytes)
	}
}

func TestValidateZIPAcceptsRootIndexAndDirectoryEntries(t *testing.T) {
	data := makeZIP(t,
		testEntry{name: "index.html", body: "<html>root</html>"},
		testEntry{name: "assets/", mode: fs.ModeDir | 0o755},
		testEntry{name: "assets/app.js", body: "window.ready=true"},
	)
	manifest, err := validateBytes(data, DefaultLimits())
	if err != nil {
		t.Fatalf("ValidateZIP() error = %v", err)
	}
	if manifest.StrippedRoot != "" || manifest.EntryCount != 2 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
}

func TestValidateZIPRejectsUnsafePaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "parent traversal", path: "../escape.txt"},
		{name: "nested traversal", path: "assets/../../escape.txt"},
		{name: "absolute path", path: "/etc/passwd"},
		{name: "windows drive", path: "C:/Windows/system.ini"},
		{name: "backslash", path: "assets\\escape.js"},
		{name: "duplicate separator", path: "assets//app.js"},
		{name: "dot segment", path: "assets/./app.js"},
		{name: "hidden file", path: ".env"},
		{name: "windows device", path: "CON.txt"},
		{name: "percent encoded path", path: "%2e%2e/escape.txt"},
		{name: "query delimiter", path: "assets/app.js?x=1"},
		{name: "trailing dot", path: "assets/app.js."},
		{name: "trailing space", path: "assets/app.js "},
		{name: "non NFC unicode", path: "assets/cafe\u0301.js"},
		{name: "null byte", path: "assets/bad\x00name.js"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			data := makeZIP(t,
				testEntry{name: "index.html", body: "<html></html>"},
				testEntry{name: testCase.path, body: "payload"},
			)
			_, err := validateBytes(data, DefaultLimits())
			if code := validationCode(t, err); code != CodeUnsafePath {
				t.Fatalf("error code = %q, want %q", code, CodeUnsafePath)
			}
		})
	}
}

func TestValidateZIPRejectsPathCollisions(t *testing.T) {
	tests := []struct {
		name    string
		entries []testEntry
	}{
		{
			name: "duplicate entry",
			entries: []testEntry{
				{name: "index.html", body: "one"},
				{name: "index.html", body: "two"},
			},
		},
		{
			name: "case insensitive collision",
			entries: []testEntry{
				{name: "index.html", body: "one"},
				{name: "Index.html", body: "two"},
			},
		},
		{
			name: "file is parent directory",
			entries: []testEntry{
				{name: "index.html", body: "one"},
				{name: "assets", body: "file"},
				{name: "assets/app.js", body: "child"},
			},
		},
		{
			name: "file conflicts with directory",
			entries: []testEntry{
				{name: "index.html", body: "one"},
				{name: "assets/", mode: fs.ModeDir | 0o755},
				{name: "assets", body: "file"},
			},
		},
		{
			name: "directory case collision",
			entries: []testEntry{
				{name: "index.html", body: "one"},
				{name: "Assets/app.js", body: "one"},
				{name: "assets/site.css", body: "two"},
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			data := makeZIP(t, testCase.entries...)
			_, err := validateBytes(data, DefaultLimits())
			if code := validationCode(t, err); code != CodePathCollision {
				t.Fatalf("error code = %q, want %q", code, CodePathCollision)
			}
		})
	}
}

func TestValidateZIPRejectsSymlinksAndUnsupportedTypes(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		data := makeZIP(t,
			testEntry{name: "index.html", body: "<html></html>"},
			testEntry{name: "assets/current", body: "../secret", mode: fs.ModeSymlink | 0o777},
		)
		_, err := validateBytes(data, DefaultLimits())
		if code := validationCode(t, err); code != CodeSymlinkUnsupported {
			t.Fatalf("error code = %q, want %q", code, CodeSymlinkUnsupported)
		}
	})

	for _, name := range []string{"shell.php", "run.py", "nested.zip", "archive.tar", "payload.exe"} {
		t.Run(name, func(t *testing.T) {
			data := makeZIP(t,
				testEntry{name: "index.html", body: "<html></html>"},
				testEntry{name: name, body: "payload"},
			)
			_, err := validateBytes(data, DefaultLimits())
			if code := validationCode(t, err); code != CodeUnsupportedFileType {
				t.Fatalf("error code = %q, want %q", code, CodeUnsupportedFileType)
			}
		})
	}
}

func TestValidateZIPRejectsMissingIndexAndEmptyArchive(t *testing.T) {
	data := makeZIP(t, testEntry{name: "readme.txt", body: "not a site"})
	_, err := validateBytes(data, DefaultLimits())
	if code := validationCode(t, err); code != CodeMissingIndex {
		t.Fatalf("error code = %q, want %q", code, CodeMissingIndex)
	}

	empty := makeZIP(t, testEntry{name: "empty/", mode: fs.ModeDir | 0o755})
	_, err = validateBytes(empty, DefaultLimits())
	if code := validationCode(t, err); code != CodeNoFiles {
		t.Fatalf("error code = %q, want %q", code, CodeNoFiles)
	}
}

func TestValidateZIPEnforcesResourceLimits(t *testing.T) {
	data := makeZIP(t,
		testEntry{name: "index.html", body: "<html></html>"},
		testEntry{name: "assets/app.js", body: strings.Repeat("A", 4096)},
	)

	limits := DefaultLimits()
	limits.MaxArchiveBytes = int64(len(data) - 1)
	_, err := validateBytes(data, limits)
	if code := validationCode(t, err); code != CodeArchiveTooLarge {
		t.Fatalf("archive limit code = %q", code)
	}

	limits = DefaultLimits()
	limits.MaxEntries = 1
	limits.MaxFiles = 1
	_, err = validateBytes(data, limits)
	if code := validationCode(t, err); code != CodeTooManyEntries {
		t.Fatalf("entry limit code = %q", code)
	}

	limits = DefaultLimits()
	limits.MaxFiles = 1
	_, err = validateBytes(data, limits)
	if code := validationCode(t, err); code != CodeTooManyFiles {
		t.Fatalf("file limit code = %q", code)
	}

	limits = DefaultLimits()
	limits.MaxFileBytes = 128
	limits.MaxTotalBytes = 1024
	_, err = validateBytes(data, limits)
	if code := validationCode(t, err); code != CodeFileTooLarge {
		t.Fatalf("single file limit code = %q", code)
	}

	totalData := makeZIP(t,
		testEntry{name: "index.html", body: strings.Repeat("i", 1200)},
		testEntry{name: "assets/app.js", body: strings.Repeat("j", 1200)},
	)
	limits = DefaultLimits()
	limits.MaxFileBytes = 1500
	limits.MaxTotalBytes = 2000
	_, err = validateBytes(totalData, limits)
	if code := validationCode(t, err); code != CodeTotalTooLarge {
		t.Fatalf("total size limit code = %q", code)
	}

	limits = DefaultLimits()
	limits.MaxCompressionRatio = 2
	_, err = validateBytes(data, limits)
	if code := validationCode(t, err); code != CodeCompressionRatio {
		t.Fatalf("compression ratio code = %q", code)
	}

	limits = DefaultLimits()
	limits.MaxCentralDirectoryBytes = 1
	_, err = validateBytes(data, limits)
	if code := validationCode(t, err); code != CodeCentralDirectorySize {
		t.Fatalf("central directory limit code = %q", code)
	}
}

func TestValidateZIPRejectsEncryptedEntries(t *testing.T) {
	data := makeZIP(t, testEntry{name: "index.html", body: "<html></html>"})
	mutateZIPFlags(data)
	_, err := validateBytes(data, DefaultLimits())
	if code := validationCode(t, err); code != CodeEncryptedEntry {
		t.Fatalf("error code = %q, want %q", code, CodeEncryptedEntry)
	}
}

func TestValidateZIPRejectsZIP64(t *testing.T) {
	data := makeZIP(t, testEntry{name: "index.html", body: "<html></html>"})
	end := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	if end < 0 {
		t.Fatal("EOCD not found")
	}
	binary.LittleEndian.PutUint16(data[end+8:end+10], ^uint16(0))
	binary.LittleEndian.PutUint16(data[end+10:end+12], ^uint16(0))
	_, err := validateBytes(data, DefaultLimits())
	if code := validationCode(t, err); code != CodeZIP64Unsupported {
		t.Fatalf("error code = %q, want %q", code, CodeZIP64Unsupported)
	}
}

func TestValidateZIPRejectsUnsupportedCompression(t *testing.T) {
	data := makeZIP(t, testEntry{name: "index.html", body: "<html></html>"})
	mutateZIPMethod(data, 99)
	_, err := validateBytes(data, DefaultLimits())
	if code := validationCode(t, err); code != CodeCompressionUnsupported {
		t.Fatalf("error code = %q, want %q", code, CodeCompressionUnsupported)
	}
}

func TestValidateZIPRejectsCRCFailure(t *testing.T) {
	data := makeZIP(t, testEntry{name: "index.html", body: strings.Repeat("hello", 100)})
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open test ZIP: %v", err)
	}
	offset, err := reader.File[0].DataOffset()
	if err != nil {
		t.Fatalf("get entry offset: %v", err)
	}
	data[offset] ^= 0xff

	_, err = validateBytes(data, DefaultLimits())
	if code := validationCode(t, err); code != CodeCorruptEntry {
		t.Fatalf("error code = %q, want %q", code, CodeCorruptEntry)
	}
}

func TestValidateZIPHonorsCanceledContext(t *testing.T) {
	data := makeZIP(t, testEntry{name: "index.html", body: "<html></html>"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ValidateZIP(ctx, bytes.NewReader(data), int64(len(data)), DefaultLimits())
	if code := validationCode(t, err); code != CodeCanceled {
		t.Fatalf("error code = %q, want %q", code, CodeCanceled)
	}
}

func mutateZIPFlags(data []byte) {
	for offset := 0; offset+10 <= len(data); offset++ {
		signature := binary.LittleEndian.Uint32(data[offset : offset+4])
		switch signature {
		case 0x04034b50:
			flags := binary.LittleEndian.Uint16(data[offset+6 : offset+8])
			binary.LittleEndian.PutUint16(data[offset+6:offset+8], flags|0x0001)
		case 0x02014b50:
			flags := binary.LittleEndian.Uint16(data[offset+8 : offset+10])
			binary.LittleEndian.PutUint16(data[offset+8:offset+10], flags|0x0001)
		}
	}
}

func mutateZIPMethod(data []byte, method uint16) {
	for offset := 0; offset+12 <= len(data); offset++ {
		signature := binary.LittleEndian.Uint32(data[offset : offset+4])
		switch signature {
		case 0x04034b50:
			binary.LittleEndian.PutUint16(data[offset+8:offset+10], method)
		case 0x02014b50:
			binary.LittleEndian.PutUint16(data[offset+10:offset+12], method)
		}
	}
}

func FuzzValidateZIPDoesNotPanic(f *testing.F) {
	f.Add(makeZIPForFuzz("index.html", "<html></html>"))
	f.Add([]byte("not a ZIP archive"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		limits := DefaultLimits()
		limits.MaxArchiveBytes = 1 << 20
		limits.MaxCentralDirectoryBytes = 1 << 16
		limits.MaxEntries = 100
		limits.MaxFiles = 100
		limits.MaxFileBytes = 1 << 18
		limits.MaxTotalBytes = 1 << 19
		_, _ = ValidateZIP(context.Background(), bytes.NewReader(data), int64(len(data)), limits)
	})
}

func makeZIPForFuzz(name, body string) []byte {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	fileWriter, err := writer.Create(name)
	if err != nil {
		return nil
	}
	if _, err := fileWriter.Write([]byte(body)); err != nil {
		return nil
	}
	if err := writer.Close(); err != nil {
		return nil
	}
	return buffer.Bytes()
}

func TestValidateHTMLBuildsManifest(t *testing.T) {
	data := []byte("<!doctype html><html><body>hello</body></html>")
	manifest, err := ValidateHTML(context.Background(), bytes.NewReader(data), int64(len(data)), 0)
	if err != nil {
		t.Fatalf("ValidateHTML() error = %v", err)
	}
	if manifest.SchemaVersion != 1 || manifest.IndexFile != "index.html" {
		t.Fatalf("unexpected HTML manifest header: %+v", manifest)
	}
	if manifest.EntryCount != 1 || len(manifest.Entries) != 1 {
		t.Fatalf("HTML entry count = %d, entries = %d", manifest.EntryCount, len(manifest.Entries))
	}
	entry := manifest.Entries[0]
	expected := sha256.Sum256(data)
	if entry.Path != "index.html" || entry.SourcePath != "index.html" {
		t.Fatalf("unexpected HTML paths: %+v", entry)
	}
	if entry.SHA256 != hex.EncodeToString(expected[:]) || entry.Size != uint64(len(data)) {
		t.Fatalf("unexpected HTML integrity metadata: %+v", entry)
	}
	if entry.ContentType != "text/html" || manifest.TotalUncompressedBytes != uint64(len(data)) {
		t.Fatalf("unexpected HTML content metadata: %+v", manifest)
	}
}

func TestValidateHTMLEnforcesSizeAndExactSourceLength(t *testing.T) {
	data := []byte("<html>size check</html>")
	_, err := ValidateHTML(context.Background(), bytes.NewReader(data), int64(len(data)), 4)
	if code := validationCode(t, err); code != CodeFileTooLarge {
		t.Fatalf("size limit code = %q, want %q", code, CodeFileTooLarge)
	}

	_, err = ValidateHTML(context.Background(), bytes.NewReader(data), int64(len(data)+1), 0)
	if code := validationCode(t, err); code != CodeSizeMismatch {
		t.Fatalf("source length code = %q, want %q", code, CodeSizeMismatch)
	}
}

func TestValidateHTMLHonorsCanceledContext(t *testing.T) {
	data := []byte("<html>cancel</html>")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ValidateHTML(ctx, bytes.NewReader(data), int64(len(data)), 0)
	if code := validationCode(t, err); code != CodeCanceled {
		t.Fatalf("error code = %q, want %q", code, CodeCanceled)
	}
}
