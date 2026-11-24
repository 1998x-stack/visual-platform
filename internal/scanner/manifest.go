package scanner

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"math"
	"strings"

	"github.com/1998x-stack/visual-platform/internal/validator"
)

// Summary is available only when every manifest entry has been independently
// scanned and its content hash was verified against the validated Manifest.
type Summary struct {
	ScannedFiles int
	ScannedBytes uint64
}

// EntryError identifies which validated publish path failed scanning. The
// public API must translate this error into a stable, non-sensitive failure code.
type EntryError struct {
	Path      string
	Reason    string
	Signature string
	Cause     error
}

func (e *EntryError) Error() string {
	return fmt.Sprintf("artifact scan failed for %q: %s", e.Path, e.Reason)
}

func (e *EntryError) Unwrap() error {
	return e.Cause
}

type trackingReader struct {
	source io.Reader
	hash   hash.Hash
	count  uint64
}

func (r *trackingReader) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	if n > 0 {
		_, _ = r.hash.Write(buffer[:n])
		r.count += uint64(n)
	}
	return n, err
}

// ScanManifest scans exactly the logical files in the given Manifest.
// It does not persist a result or grant publication permission. Its source
// must be the same private, immutable spool used for ZIP/HTML validation.
func ScanManifest(
	ctx context.Context,
	source io.ReaderAt,
	sourceSize int64,
	sourceType string,
	manifest validator.Manifest,
	engine Scanner,
) (Summary, error) {
	if ctx == nil || source == nil || engine == nil || sourceSize < 0 {
		return Summary{}, errors.New("invalid artifact scan dependencies")
	}
	if err := ctx.Err(); err != nil {
		return Summary{}, err
	}
	if manifest.SchemaVersion != 1 ||
		manifest.SourceSize != sourceSize ||
		manifest.IndexFile != "index.html" ||
		manifest.EntryCount == 0 ||
		manifest.EntryCount != len(manifest.Entries) {
		return Summary{}, errors.New("manifest metadata is inconsistent")
	}
	if sourceType != "zip" && sourceType != "html" {
		return Summary{}, errors.New("unsupported artifact source type")
	}

	var archiveEntries map[string]*zip.File
	if sourceType == "zip" {
		archive, err := zip.NewReader(source, sourceSize)
		if err != nil {
			return Summary{}, fmt.Errorf("reopen validated ZIP: %w", err)
		}
		archiveEntries = make(map[string]*zip.File, len(archive.File))
		for _, file := range archive.File {
			if strings.HasSuffix(file.Name, "/") {
				continue
			}
			if _, duplicate := archiveEntries[file.Name]; duplicate {
				return Summary{}, errors.New("ZIP source contains duplicate paths")
			}
			archiveEntries[file.Name] = file
		}
		if len(archiveEntries) != manifest.EntryCount {
			return Summary{}, errors.New("ZIP and Manifest file counts differ")
		}
	} else if len(manifest.Entries) != 1 {
		return Summary{}, errors.New("standalone HTML must have one manifest entry")
	}

	seenPublish := make(map[string]struct{}, manifest.EntryCount)
	seenSource := make(map[string]struct{}, manifest.EntryCount)
	var summary Summary
	var previousPath string
	for i, entry := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return Summary{}, err
		}
		if entry.Path == "" || entry.SourcePath == "" || entry.Size > math.MaxInt64 ||
			len(entry.SHA256) != 64 {
			return Summary{}, errors.New("manifest entry is invalid")
		}
		if i > 0 && entry.Path <= previousPath {
			return Summary{}, errors.New("manifest paths are not strictly sorted")
		}
		previousPath = entry.Path
		if _, exists := seenPublish[entry.Path]; exists {
			return Summary{}, errors.New("duplicate publish path")
		}
		seenPublish[entry.Path] = struct{}{}
		if _, exists := seenSource[entry.SourcePath]; exists {
			return Summary{}, errors.New("duplicate source path")
		}
		seenSource[entry.SourcePath] = struct{}{}

		expectedHash, err := hex.DecodeString(entry.SHA256)
		if err != nil || len(expectedHash) != sha256.Size {
			return Summary{}, errors.New("manifest contains invalid SHA-256")
		}
		var body io.ReadCloser
		if sourceType == "html" {
			if entry.Path != "index.html" || entry.SourcePath != "index.html" ||
				entry.Size != uint64(sourceSize) {
				return Summary{}, errors.New("standalone HTML manifest path/size mismatch")
			}
			body = io.NopCloser(io.NewSectionReader(source, 0, sourceSize))
		} else {
			file := archiveEntries[entry.SourcePath]
			if file == nil || file.UncompressedSize64 != entry.Size {
				return Summary{}, &EntryError{Path: entry.Path, Reason: "source entry mismatch"}
			}
			body, err = file.Open()
			if err != nil {
				return Summary{}, &EntryError{Path: entry.Path, Reason: "source entry unreadable", Cause: err}
			}
		}

		tracked := &trackingReader{source: body, hash: sha256.New()}
		result, scanErr := engine.Scan(ctx, tracked, int64(entry.Size))
		// Enforce complete consumption even if a faulty scanner returned clean
		// without reading its input. A final EOF read also triggers ZIP CRC.
		var probe [1]byte
		probeN, probeErr := tracked.Read(probe[:])
		closeErr := body.Close()
		if scanErr != nil {
			return Summary{}, &EntryError{Path: entry.Path, Reason: "scanner unavailable or incomplete", Cause: scanErr}
		}
		if closeErr != nil || tracked.count != entry.Size || probeN != 0 || probeErr != io.EOF {
			return Summary{}, &EntryError{Path: entry.Path, Reason: "scan did not consume exact file bytes", Cause: closeErr}
		}
		if result.Verdict == VerdictInfected {
			return Summary{}, &EntryError{Path: entry.Path, Reason: "infected", Signature: result.Signature}
		}
		if result.Verdict != VerdictClean {
			return Summary{}, &EntryError{Path: entry.Path, Reason: "scanner did not return clean verdict"}
		}
		if !strings.EqualFold(hex.EncodeToString(tracked.hash.Sum(nil)), entry.SHA256) {
			return Summary{}, &EntryError{Path: entry.Path, Reason: "content hash mismatch"}
		}
		summary.ScannedFiles++
		summary.ScannedBytes += entry.Size
	}
	if summary.ScannedBytes != manifest.TotalUncompressedBytes {
		return Summary{}, errors.New("manifest aggregate bytes mismatch")
	}
	return summary, nil
}
