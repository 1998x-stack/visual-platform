package validator

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	defaultMaxArchiveBytes         int64  = 100 << 20
	defaultMaxHTMLBytes            int64  = 10 << 20
	defaultMaxCentralDirectorySize uint64 = 8 << 20
	defaultMaxEntries                     = 5000
	defaultMaxFiles                       = 4000
	defaultMaxFileBytes            uint64 = 64 << 20
	defaultMaxTotalBytes           uint64 = 512 << 20
	defaultMaxCompressionRatio     uint64 = 100
	defaultMaxPathBytes                   = 1024
	defaultMaxPathDepth                   = 32
	maxEOCDCommentBytes                   = 1<<16 - 1
	minEOCDSize                           = 22
	eocdSignature                  uint32 = 0x06054b50
)

type ErrorCode string

const (
	CodeInvalidArchive         ErrorCode = "invalid_archive"
	CodeArchiveTooLarge        ErrorCode = "archive_too_large"
	CodeCentralDirectorySize   ErrorCode = "central_directory_too_large"
	CodeTooManyEntries         ErrorCode = "too_many_entries"
	CodeTooManyFiles           ErrorCode = "too_many_files"
	CodeZIP64Unsupported       ErrorCode = "zip64_unsupported"
	CodeMultiDiskUnsupported   ErrorCode = "multi_disk_unsupported"
	CodeUnsafePath             ErrorCode = "unsafe_path"
	CodePathCollision          ErrorCode = "path_collision"
	CodeSymlinkUnsupported     ErrorCode = "symlink_unsupported"
	CodeSpecialFile            ErrorCode = "special_file_unsupported"
	CodeUnsupportedFileType    ErrorCode = "unsupported_file_type"
	CodeEncryptedEntry         ErrorCode = "encrypted_entry"
	CodeCompressionUnsupported ErrorCode = "compression_unsupported"
	CodeFileTooLarge           ErrorCode = "file_too_large"
	CodeTotalTooLarge          ErrorCode = "total_uncompressed_too_large"
	CodeCompressionRatio       ErrorCode = "compression_ratio_exceeded"
	CodeCorruptEntry           ErrorCode = "corrupt_entry"
	CodeSizeMismatch           ErrorCode = "entry_size_mismatch"
	CodeMissingIndex           ErrorCode = "missing_index_html"
	CodeNoFiles                ErrorCode = "archive_has_no_files"
	CodeCanceled               ErrorCode = "validation_canceled"
)

// ValidationError is a stable, non-sensitive error returned for rejected archives.
type ValidationError struct {
	Code  ErrorCode
	Entry string
	Cause error
}

func (e *ValidationError) Error() string {
	if e.Entry != "" {
		return fmt.Sprintf("ZIP validation rejected entry %q: %s", e.Entry, e.Code)
	}
	return fmt.Sprintf("ZIP validation failed: %s", e.Code)
}

func (e *ValidationError) Unwrap() error {
	return e.Cause
}

func reject(code ErrorCode, entry string, cause error) error {
	return &ValidationError{Code: code, Entry: entry, Cause: cause}
}

// Limits bounds parser memory, decompression work, and the generated manifest.
type Limits struct {
	MaxArchiveBytes          int64
	MaxCentralDirectoryBytes uint64
	MaxEntries               int
	MaxFiles                 int
	MaxFileBytes             uint64
	MaxTotalBytes            uint64
	MaxCompressionRatio      uint64
	MaxPathBytes             int
	MaxPathDepth             int
}

// DefaultLimits returns conservative limits for the initial static-site MVP.
// Tune these only from representative production ZIP measurements.
func DefaultLimits() Limits {
	return Limits{
		MaxArchiveBytes:          defaultMaxArchiveBytes,
		MaxCentralDirectoryBytes: defaultMaxCentralDirectorySize,
		MaxEntries:               defaultMaxEntries,
		MaxFiles:                 defaultMaxFiles,
		MaxFileBytes:             defaultMaxFileBytes,
		MaxTotalBytes:            defaultMaxTotalBytes,
		MaxCompressionRatio:      defaultMaxCompressionRatio,
		MaxPathBytes:             defaultMaxPathBytes,
		MaxPathDepth:             defaultMaxPathDepth,
	}
}

// Manifest is a deterministic description of accepted files, not a publication grant.
type Manifest struct {
	SchemaVersion          int             `json:"schema_version"`
	IndexFile              string          `json:"index_file"`
	StrippedRoot           string          `json:"stripped_root,omitempty"`
	SourceSize             int64           `json:"source_size"`
	EntryCount             int             `json:"entry_count"`
	TotalUncompressedBytes uint64          `json:"total_uncompressed_bytes"`
	Entries                []ManifestEntry `json:"entries"`
}

// ManifestEntry describes one regular file in the validated archive.
type ManifestEntry struct {
	Path        string `json:"path"`
	SourcePath  string `json:"source_path"`
	Size        uint64 `json:"size"`
	SHA256      string `json:"sha256"`
	ContentType string `json:"content_type"`
}

type candidate struct {
	file       *zip.File
	path       string
	sourcePath string
	size       uint64
}

type dataRange struct {
	start uint64
	end   uint64
}

// ValidateZIP validates a ZIP without extracting it to the host filesystem.
// The caller must provide a bounded, seekable source such as a private temp file.
func ValidateZIP(ctx context.Context, source io.ReaderAt, archiveSize int64, limits Limits) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil || archiveSize < minEOCDSize {
		return Manifest{}, reject(CodeInvalidArchive, "", errors.New("missing or truncated ZIP source"))
	}

	normalized, err := normalizeLimits(limits)
	if err != nil {
		return Manifest{}, reject(CodeInvalidArchive, "", err)
	}
	if archiveSize > normalized.MaxArchiveBytes {
		return Manifest{}, reject(CodeArchiveTooLarge, "", nil)
	}

	entryCount, err := inspectEndRecord(source, archiveSize, normalized)
	if err != nil {
		return Manifest{}, err
	}

	zipReader, err := zip.NewReader(source, archiveSize)
	if err != nil && (!errors.Is(err, zip.ErrInsecurePath) || zipReader == nil) {
		return Manifest{}, reject(CodeInvalidArchive, "", err)
	}
	// Go may return a usable Reader together with ErrInsecurePath. Continue only
	// so this validator can return its own stable path rejection after checking
	// every raw entry name; no insecure path is ever extracted or accepted.
	if len(zipReader.File) != entryCount {
		return Manifest{}, reject(CodeInvalidArchive, "", errors.New("central directory entry count mismatch"))
	}
	if len(zipReader.File) == 0 {
		return Manifest{}, reject(CodeNoFiles, "", nil)
	}

	candidates := make([]candidate, 0, len(zipReader.File))
	ranges := make([]dataRange, 0, len(zipReader.File))
	explicitEntries := make(map[string]struct{}, len(zipReader.File))
	filePaths := make(map[string]struct{}, len(zipReader.File))
	directoryPaths := make(map[string]struct{}, len(zipReader.File))
	directorySpellings := make(map[string]string, len(zipReader.File))
	var compressedTotal uint64
	var declaredTotal uint64

	for _, file := range zipReader.File {
		if err := ctx.Err(); err != nil {
			return Manifest{}, reject(CodeCanceled, file.Name, err)
		}
		if file.NonUTF8 || !utf8.ValidString(file.Name) || !norm.NFC.IsNormalString(file.Name) {
			return Manifest{}, reject(CodeUnsafePath, file.Name, nil)
		}

		mode := file.Mode()
		if mode&fs.ModeSymlink != 0 {
			return Manifest{}, reject(CodeSymlinkUnsupported, file.Name, nil)
		}
		if mode.Type() != 0 && !mode.IsDir() && !mode.IsRegular() {
			return Manifest{}, reject(CodeSpecialFile, file.Name, nil)
		}

		isDirectory := strings.HasSuffix(file.Name, "/")
		if mode.IsDir() && !isDirectory {
			return Manifest{}, reject(CodeUnsafePath, file.Name, nil)
		}
		cleanPath, pathErr := validateEntryPath(file.Name, isDirectory, normalized)
		if pathErr != nil {
			return Manifest{}, pathErr
		}
		if err := registerEntryPath(cleanPath, isDirectory, explicitEntries, filePaths, directoryPaths, directorySpellings); err != nil {
			return Manifest{}, err
		}

		if isDirectory {
			if file.UncompressedSize64 != 0 || file.CompressedSize64 != 0 {
				return Manifest{}, reject(CodeCorruptEntry, file.Name, nil)
			}
			continue
		}
		if len(candidates) >= normalized.MaxFiles {
			return Manifest{}, reject(CodeTooManyFiles, file.Name, nil)
		}
		if file.Flags&0x0001 != 0 || file.Flags&0x0040 != 0 {
			return Manifest{}, reject(CodeEncryptedEntry, file.Name, nil)
		}
		if file.Method != zip.Store && file.Method != zip.Deflate {
			return Manifest{}, reject(CodeCompressionUnsupported, file.Name, nil)
		}
		if isForbiddenExtension(cleanPath) {
			return Manifest{}, reject(CodeUnsupportedFileType, file.Name, nil)
		}
		if file.UncompressedSize64 > normalized.MaxFileBytes {
			return Manifest{}, reject(CodeFileTooLarge, file.Name, nil)
		}
		if file.UncompressedSize64 > normalized.MaxTotalBytes-declaredTotal {
			return Manifest{}, reject(CodeTotalTooLarge, file.Name, nil)
		}
		declaredTotal += file.UncompressedSize64

		if file.CompressedSize64 == 0 {
			if file.UncompressedSize64 != 0 {
				return Manifest{}, reject(CodeCompressionRatio, file.Name, nil)
			}
		} else if ratioExceeded(file.UncompressedSize64, file.CompressedSize64, normalized.MaxCompressionRatio) {
			return Manifest{}, reject(CodeCompressionRatio, file.Name, nil)
		}

		if file.CompressedSize64 > ^uint64(0)-compressedTotal {
			return Manifest{}, reject(CodeInvalidArchive, file.Name, nil)
		}
		compressedTotal += file.CompressedSize64

		offset, offsetErr := file.DataOffset()
		if offsetErr != nil || offset < 0 {
			return Manifest{}, reject(CodeCorruptEntry, file.Name, offsetErr)
		}
		start := uint64(offset)
		if start > uint64(archiveSize) || file.CompressedSize64 > uint64(archiveSize)-start {
			return Manifest{}, reject(CodeCorruptEntry, file.Name, nil)
		}
		if file.CompressedSize64 > 0 {
			ranges = append(ranges, dataRange{start: start, end: start + file.CompressedSize64})
		}
		candidates = append(candidates, candidate{
			file:       file,
			path:       cleanPath,
			sourcePath: cleanPath,
			size:       file.UncompressedSize64,
		})
	}

	if len(candidates) == 0 {
		return Manifest{}, reject(CodeNoFiles, "", nil)
	}
	if compressedTotal > uint64(archiveSize) {
		return Manifest{}, reject(CodeInvalidArchive, "", nil)
	}
	if err := validateDataRanges(ranges); err != nil {
		return Manifest{}, err
	}

	candidates, strippedRoot, err := normalizeSiteRoot(candidates)
	if err != nil {
		return Manifest{}, err
	}

	manifest := Manifest{
		SchemaVersion:          1,
		IndexFile:              "index.html",
		StrippedRoot:           strippedRoot,
		SourceSize:             archiveSize,
		EntryCount:             len(candidates),
		TotalUncompressedBytes: declaredTotal,
		Entries:                make([]ManifestEntry, 0, len(candidates)),
	}
	var actualTotal uint64
	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			return Manifest{}, reject(CodeCanceled, item.path, err)
		}
		entryReader, openErr := item.file.Open()
		if openErr != nil {
			return Manifest{}, reject(CodeCorruptEntry, item.path, openErr)
		}

		hasher := sha256.New()
		limited := io.LimitReader(&contextReader{ctx: ctx, reader: entryReader}, int64(item.size)+1)
		actualSize, copyErr := io.Copy(hasher, limited)
		closeErr := entryReader.Close()
		if copyErr != nil {
			if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
				return Manifest{}, reject(CodeCanceled, item.path, copyErr)
			}
			return Manifest{}, reject(CodeCorruptEntry, item.path, copyErr)
		}
		if closeErr != nil {
			return Manifest{}, reject(CodeCorruptEntry, item.path, closeErr)
		}
		if actualSize < 0 || uint64(actualSize) != item.size {
			return Manifest{}, reject(CodeSizeMismatch, item.path, nil)
		}
		if uint64(actualSize) > normalized.MaxTotalBytes-actualTotal {
			return Manifest{}, reject(CodeTotalTooLarge, item.path, nil)
		}
		actualTotal += uint64(actualSize)

		manifest.Entries = append(manifest.Entries, ManifestEntry{
			Path:        item.path,
			SourcePath:  item.sourcePath,
			Size:        uint64(actualSize),
			SHA256:      hex.EncodeToString(hasher.Sum(nil)),
			ContentType: contentTypeFor(item.path),
		})
	}
	if actualTotal != declaredTotal {
		return Manifest{}, reject(CodeSizeMismatch, "", nil)
	}

	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].Path < manifest.Entries[j].Path
	})
	return manifest, nil
}

// ValidateHTML creates the same Manifest shape for a standalone HTML upload.
// It hashes the exact source bytes and does not parse, sanitize, or execute HTML.
func ValidateHTML(ctx context.Context, source io.ReaderAt, sourceSize, maxHTMLBytes int64) (Manifest, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil || sourceSize < 0 || maxHTMLBytes < 0 {
		return Manifest{}, reject(CodeInvalidArchive, "", errors.New("invalid HTML source or size"))
	}
	if maxHTMLBytes == 0 {
		maxHTMLBytes = defaultMaxHTMLBytes
	}
	if sourceSize > maxHTMLBytes {
		return Manifest{}, reject(CodeFileTooLarge, "index.html", nil)
	}

	hasher := sha256.New()
	section := io.NewSectionReader(source, 0, sourceSize)
	actualSize, err := io.Copy(hasher, &contextReader{ctx: ctx, reader: section})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return Manifest{}, reject(CodeCanceled, "index.html", err)
		}
		return Manifest{}, reject(CodeCorruptEntry, "index.html", err)
	}
	if actualSize != sourceSize {
		return Manifest{}, reject(CodeSizeMismatch, "index.html", nil)
	}

	return Manifest{
		SchemaVersion:          1,
		IndexFile:              "index.html",
		SourceSize:             sourceSize,
		EntryCount:             1,
		TotalUncompressedBytes: uint64(actualSize),
		Entries: []ManifestEntry{{
			Path:        "index.html",
			SourcePath:  "index.html",
			Size:        uint64(actualSize),
			SHA256:      hex.EncodeToString(hasher.Sum(nil)),
			ContentType: "text/html",
		}},
	}, nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	defaults := DefaultLimits()
	if limits.MaxArchiveBytes == 0 {
		limits.MaxArchiveBytes = defaults.MaxArchiveBytes
	}
	if limits.MaxCentralDirectoryBytes == 0 {
		limits.MaxCentralDirectoryBytes = defaults.MaxCentralDirectoryBytes
	}
	if limits.MaxEntries == 0 {
		limits.MaxEntries = defaults.MaxEntries
	}
	if limits.MaxFiles == 0 {
		limits.MaxFiles = defaults.MaxFiles
	}
	if limits.MaxFileBytes == 0 {
		limits.MaxFileBytes = defaults.MaxFileBytes
	}
	if limits.MaxTotalBytes == 0 {
		limits.MaxTotalBytes = defaults.MaxTotalBytes
	}
	if limits.MaxCompressionRatio == 0 {
		limits.MaxCompressionRatio = defaults.MaxCompressionRatio
	}
	if limits.MaxPathBytes == 0 {
		limits.MaxPathBytes = defaults.MaxPathBytes
	}
	if limits.MaxPathDepth == 0 {
		limits.MaxPathDepth = defaults.MaxPathDepth
	}
	if limits.MaxArchiveBytes < minEOCDSize ||
		limits.MaxCentralDirectoryBytes == 0 ||
		limits.MaxEntries < 1 ||
		limits.MaxFiles < 1 ||
		limits.MaxFiles > limits.MaxEntries ||
		limits.MaxFileBytes == 0 ||
		limits.MaxTotalBytes == 0 ||
		limits.MaxFileBytes > limits.MaxTotalBytes ||
		limits.MaxFileBytes >= uint64(1<<63-1) ||
		limits.MaxTotalBytes >= uint64(1<<63-1) ||
		limits.MaxCompressionRatio == 0 ||
		limits.MaxPathBytes < 1 ||
		limits.MaxPathDepth < 1 {
		return Limits{}, errors.New("invalid ZIP validation limits")
	}
	return limits, nil
}

// inspectEndRecord bounds central-directory allocation before archive/zip parses it.
func inspectEndRecord(source io.ReaderAt, archiveSize int64, limits Limits) (int, error) {
	tailSize := int64(minEOCDSize + maxEOCDCommentBytes)
	if archiveSize < tailSize {
		tailSize = archiveSize
	}
	tail := make([]byte, int(tailSize))
	if n, err := source.ReadAt(tail, archiveSize-tailSize); err != nil || n != len(tail) {
		return 0, reject(CodeInvalidArchive, "", err)
	}

	for offset := len(tail) - minEOCDSize; offset >= 0; offset-- {
		if binary.LittleEndian.Uint32(tail[offset:offset+4]) != eocdSignature {
			continue
		}
		commentLength := int(binary.LittleEndian.Uint16(tail[offset+20 : offset+22]))
		if offset+minEOCDSize+commentLength != len(tail) {
			continue
		}

		diskNumber := binary.LittleEndian.Uint16(tail[offset+4 : offset+6])
		directoryDisk := binary.LittleEndian.Uint16(tail[offset+6 : offset+8])
		entriesOnDisk := binary.LittleEndian.Uint16(tail[offset+8 : offset+10])
		totalEntries := binary.LittleEndian.Uint16(tail[offset+10 : offset+12])
		directorySize := binary.LittleEndian.Uint32(tail[offset+12 : offset+16])
		directoryOffset := binary.LittleEndian.Uint32(tail[offset+16 : offset+20])

		if diskNumber != 0 || directoryDisk != 0 || entriesOnDisk != totalEntries {
			return 0, reject(CodeMultiDiskUnsupported, "", nil)
		}
		if totalEntries == ^uint16(0) || directorySize == ^uint32(0) || directoryOffset == ^uint32(0) {
			return 0, reject(CodeZIP64Unsupported, "", nil)
		}
		if int(totalEntries) > limits.MaxEntries {
			return 0, reject(CodeTooManyEntries, "", nil)
		}
		if uint64(directorySize) > limits.MaxCentralDirectoryBytes {
			return 0, reject(CodeCentralDirectorySize, "", nil)
		}

		endRecordOffset := uint64(archiveSize-tailSize) + uint64(offset)
		directoryEnd := uint64(directoryOffset) + uint64(directorySize)
		if directoryEnd < uint64(directoryOffset) || directoryEnd > endRecordOffset {
			return 0, reject(CodeInvalidArchive, "", nil)
		}
		return int(totalEntries), nil
	}
	return 0, reject(CodeInvalidArchive, "", errors.New("end of central directory not found"))
}

func validateEntryPath(raw string, isDirectory bool, limits Limits) (string, error) {
	if raw == "" || len(raw) > limits.MaxPathBytes || !utf8.ValidString(raw) {
		return "", reject(CodeUnsafePath, raw, nil)
	}
	if isDirectory != strings.HasSuffix(raw, "/") {
		return "", reject(CodeUnsafePath, raw, nil)
	}
	name := strings.TrimSuffix(raw, "/")
	if name == "" || strings.HasPrefix(name, "/") || path.IsAbs(name) ||
		containsUnsafePathCharacter(name) ||
		!norm.NFC.IsNormalString(name) ||
		!fs.ValidPath(name) ||
		path.Clean(name) != name {
		return "", reject(CodeUnsafePath, raw, nil)
	}

	segments := strings.Split(name, "/")
	if len(segments) > limits.MaxPathDepth {
		return "", reject(CodeUnsafePath, raw, nil)
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." ||
			strings.HasPrefix(segment, ".") ||
			strings.HasSuffix(segment, ".") ||
			strings.HasSuffix(segment, " ") {
			return "", reject(CodeUnsafePath, raw, nil)
		}
		for _, character := range segment {
			if unicode.IsControl(character) {
				return "", reject(CodeUnsafePath, raw, nil)
			}
		}
		if isWindowsReservedName(segment) {
			return "", reject(CodeUnsafePath, raw, nil)
		}
	}
	return name, nil
}

// containsUnsafePathCharacter rejects path delimiters that are ambiguous across URLs and filesystems.
func containsUnsafePathCharacter(name string) bool {
	for _, character := range name {
		switch character {
		case 0, '"', '#', '%', '*', ':', '<', '>', '?', '\\', '|':
			return true
		}
	}
	return false
}

func isWindowsReservedName(segment string) bool {
	base := strings.ToUpper(strings.SplitN(segment, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	return false
}

func canonicalPath(name string) string {
	return cases.Fold().String(norm.NFC.String(name))
}

func registerEntryPath(name string, isDirectory bool, explicit, files, directories map[string]struct{}, spellings map[string]string) error {
	key := canonicalPath(name)
	if _, exists := explicit[key]; exists {
		return reject(CodePathCollision, name, nil)
	}
	explicit[key] = struct{}{}

	if isDirectory {
		if _, exists := files[key]; exists {
			return reject(CodePathCollision, name, nil)
		}
		if err := registerDirectorySpelling(name, key, spellings); err != nil {
			return err
		}
		directories[key] = struct{}{}
	} else {
		if _, exists := directories[key]; exists {
			return reject(CodePathCollision, name, nil)
		}
		files[key] = struct{}{}
	}

	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		parentKey := canonicalPath(parent)
		if _, exists := files[parentKey]; exists {
			return reject(CodePathCollision, name, nil)
		}
		if err := registerDirectorySpelling(parent, parentKey, spellings); err != nil {
			return reject(CodePathCollision, name, err)
		}
		directories[parentKey] = struct{}{}
	}
	return nil
}

func registerDirectorySpelling(name, key string, spellings map[string]string) error {
	if previous, exists := spellings[key]; exists && previous != name {
		return reject(CodePathCollision, name, nil)
	}
	spellings[key] = name
	return nil
}

func ratioExceeded(uncompressed, compressed, maximum uint64) bool {
	quotient := uncompressed / compressed
	remainder := uncompressed % compressed
	return quotient > maximum || (quotient == maximum && remainder > 0)
}

func validateDataRanges(ranges []dataRange) error {
	sort.Slice(ranges, func(i, j int) bool {
		return ranges[i].start < ranges[j].start
	})
	for index := 1; index < len(ranges); index++ {
		if ranges[index].start < ranges[index-1].end {
			return reject(CodeCorruptEntry, "", errors.New("overlapping ZIP entry data"))
		}
	}
	return nil
}

func normalizeSiteRoot(entries []candidate) ([]candidate, string, error) {
	hasRootIndex := false
	for _, entry := range entries {
		if entry.path == "index.html" {
			hasRootIndex = true
			break
		}
	}

	strippedRoot := ""
	if !hasRootIndex {
		commonRoot := ""
		allNested := true
		for _, entry := range entries {
			separator := strings.IndexByte(entry.path, '/')
			if separator <= 0 {
				allNested = false
				break
			}
			root := entry.path[:separator]
			if commonRoot == "" {
				commonRoot = root
			} else if canonicalPath(root) != canonicalPath(commonRoot) {
				allNested = false
				break
			}
		}
		if allNested && commonRoot != "" {
			strippedRoot = commonRoot
			for index := range entries {
				entries[index].path = strings.TrimPrefix(entries[index].path, commonRoot+"/")
			}
		}
	}

	hasIndex := false
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.path == "index.html" {
			hasIndex = true
		}
		key := canonicalPath(entry.path)
		if _, exists := seen[key]; exists {
			return nil, "", reject(CodePathCollision, entry.path, nil)
		}
		seen[key] = struct{}{}
	}
	if !hasIndex {
		return nil, "", reject(CodeMissingIndex, "", nil)
	}
	return entries, strippedRoot, nil
}

func isForbiddenExtension(name string) bool {
	extension := strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
	switch extension {
	case "php", "php3", "php4", "php5", "php7", "phtml", "phar",
		"asp", "aspx", "asa", "asax", "ashx", "asmx",
		"jsp", "jspx", "jspf", "cgi", "fcgi", "pl",
		"py", "pyc", "pyw", "rb", "rbw", "sh", "bash", "zsh",
		"bat", "cmd", "ps1", "exe", "com", "dll", "so", "dylib",
		"msi", "jar", "war", "ear", "class",
		"zip", "rar", "7z", "tar", "gz", "tgz", "bz2", "xz", "zst":
		return true
	default:
		return false
	}
}

func contentTypeFor(name string) string {
	extension := strings.ToLower(path.Ext(name))
	switch extension {
	case ".html", ".htm":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js", ".mjs":
		return "text/javascript"
	case ".json", ".map":
		return "application/json"
	case ".svg":
		return "image/svg+xml"
	case ".txt", ".md", ".csv":
		return "text/plain"
	case ".xml":
		return "application/xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".ico":
		return "image/x-icon"
	case ".pdf":
		return "application/pdf"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".otf":
		return "font/otf"
	case ".mp3":
		return "audio/mpeg"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".wasm":
		return "application/wasm"
	default:
		return "application/octet-stream"
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
