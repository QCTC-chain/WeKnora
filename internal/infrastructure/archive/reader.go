// Package archive safely expands uploaded ZIP knowledge packages: a Markdown
// document (or several, one per directory) together with the image files its
// references point at. The reader enforces size, count and path-safety limits
// before anything is handed to the import service, and normalizes the entry
// names produced by the zip tools commonly used for Chinese documents
// (UTF-8, GBK).
package archive

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/utils"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// Limits bound one archive expansion. They exist so a hostile archive cannot
// exhaust memory or disk: every entry read is counted against
// MaxExpandedBytes and capped at MaxSingleEntryBytes, and the number of
// documents and entries is capped outright.
type Limits struct {
	// MaxDocuments caps how many Markdown documents one archive may yield.
	// The batch-download pipeline uses the same 200-document ceiling.
	MaxDocuments int
	// MaxEntries caps the number of regular files considered, whatever
	// their type.
	MaxEntries int
	// MaxExpandedBytes caps the total uncompressed bytes actually read.
	MaxExpandedBytes int64
	// MaxSingleEntryBytes caps one entry's uncompressed size.
	MaxSingleEntryBytes int64
}

// DefaultLimits returns the production limits for knowledge archives.
func DefaultLimits() Limits {
	return Limits{
		MaxDocuments:        200,
		MaxEntries:          utils.GetMaxArchiveEntries(),
		MaxExpandedBytes:    512 << 20, // 512 MiB, same ceiling as batch download
		MaxSingleEntryBytes: 100 << 20, // 100 MiB, matches the document-wide inline image budget
	}
}

// Errors surfaced by Open and Read. They are value-tested by callers that
// turn them into user-facing report entries.
var (
	ErrBadArchive   = errors.New("无法读取压缩包，请确认文件是有效的 zip 归档")
	ErrUnsafePath   = errors.New("压缩包含有不安全的文件路径")
	ErrTooLarge     = errors.New("压缩包解压后超出大小限制")
	ErrTooManyDocs  = errors.New("压缩包中的 Markdown 文档数量超出限制")
	ErrTooManyFiles = errors.New("压缩包中的文件数量超出限制")
)

// SkippedEntry records an archive member that was not loaded, and why.
type SkippedEntry struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Archive is an opened, safety-checked zip. Entry names are normalized to
// slash-separated, clean, relative paths; lookups use those names.
type Archive struct {
	reader   *zip.Reader
	files    map[string]*zip.File
	names    []string
	limits   Limits
	expanded int64
	skipped  []SkippedEntry
}

// Open parses and validates the archive structure. Entry contents are not
// read here; Read enforces the byte budgets lazily so a manifest scan costs
// only what the Markdown files weigh.
func Open(r io.ReaderAt, size int64, limits Limits) (*Archive, error) {
	if limits.MaxEntries <= 0 {
		limits = DefaultLimits()
	}
	reader, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	a := &Archive{reader: reader, files: make(map[string]*zip.File, len(reader.File)), limits: limits}

	// Only entries that are actually loaded count against MaxEntries;
	// directories, junk metadata, links and duplicates are skipped without
	// consuming the budget.
	for _, f := range reader.File {
		mode := f.Mode()
		if mode.IsDir() {
			continue
		}
		name, err := normalizeEntryName(f.Name, f.NonUTF8)
		if err != nil {
			a.skipped = append(a.skipped, SkippedEntry{Name: f.Name, Reason: "不安全的路径，已忽略"})
			continue
		}
		if isJunkEntry(name) {
			a.skipped = append(a.skipped, SkippedEntry{Name: name, Reason: "系统元数据文件，已忽略"})
			continue
		}
		if !mode.IsRegular() {
			a.skipped = append(a.skipped, SkippedEntry{Name: name, Reason: "非常规文件（链接等），已忽略"})
			continue
		}
		if _, dup := a.files[name]; dup {
			a.skipped = append(a.skipped, SkippedEntry{Name: name, Reason: "重复路径，保留首个"})
			continue
		}
		if len(a.files) >= limits.MaxEntries {
			return nil, ErrTooManyFiles
		}
		a.files[name] = f
		a.names = append(a.names, name)
	}
	return a, nil
}

// Names returns the loadable entry names, sorted.
func (a *Archive) Names() []string {
	if a.names == nil {
		return nil
	}
	out := make([]string, len(a.names))
	copy(out, a.names)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Has reports whether name is a loadable entry.
func (a *Archive) Has(name string) bool {
	_, ok := a.files[name]
	return ok
}

// Skipped lists the entries ignored during Open, for the import report.
func (a *Archive) Skipped() []SkippedEntry {
	out := make([]SkippedEntry, len(a.skipped))
	copy(out, a.skipped)
	return out
}

// Read loads one entry with the archive's byte budgets enforced. Reads are
// on-demand: a manifest pass pays only for the Markdown files it inspects.
// Entry headers can lie about sizes, so the caps are applied to the bytes
// actually read, never to the declared size.
func (a *Archive) Read(name string) ([]byte, error) {
	f, ok := a.files[name]
	if !ok {
		return nil, fmt.Errorf("压缩包中不存在文件 %s", name)
	}
	if a.expanded >= a.limits.MaxExpandedBytes {
		return nil, ErrTooLarge
	}
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	defer rc.Close()

	data, err := io.ReadAll(io.LimitReader(rc, a.limits.MaxSingleEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	if int64(len(data)) > a.limits.MaxSingleEntryBytes {
		return nil, fmt.Errorf("%w: %s 超出单文件大小限制", ErrTooLarge, name)
	}
	if a.expanded+int64(len(data)) > a.limits.MaxExpandedBytes {
		return nil, ErrTooLarge
	}
	a.expanded += int64(len(data))
	return data, nil
}

// normalizeEntryName turns a raw zip entry name into a safe lookup key:
// backslashes become slashes, traversal segments are rejected outright, and
// the result is a clean, relative slash path. nonUTF8 marks entries produced
// without the zip UTF-8 flag — typically GBK names from Chinese Windows zip
// tools, which Go has already mangled via its CP437 fallback.
func normalizeEntryName(raw string, nonUTF8 bool) (string, error) {
	if strings.Contains(raw, "\x00") {
		return "", ErrUnsafePath
	}
	decoded := decodeEntryName(raw, nonUTF8)
	decoded = strings.ReplaceAll(decoded, "\\", "/")
	for _, segment := range strings.Split(decoded, "/") {
		if segment == ".." {
			return "", ErrUnsafePath
		}
	}
	cleaned := path.Clean(decoded)
	if cleaned == "." || cleaned == "/" || strings.HasPrefix(cleaned, "/") {
		return "", ErrUnsafePath
	}
	return cleaned, nil
}

// decodeEntryName recovers the real name for entries produced without the
// zip UTF-8 flag. Two shapes occur, depending on the producer:
//
//   - Go passes the producer's raw bytes through verbatim when they are not
//     valid UTF-8 — typically GBK names from Chinese Windows zip tools.
//   - Some producers write names Go can CP437-decode into a valid UTF-8
//     string; re-encoding with CP437 recovers the raw bytes there.
//
// Both shapes get one GBK interpretation attempt, matching the de-facto
// behaviour of the tools that write such archives. Any round-trip failure
// keeps the name as received.
func decodeEntryName(raw string, nonUTF8 bool) string {
	if !nonUTF8 {
		return raw
	}
	if !utf8.ValidString(raw) {
		decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes([]byte(raw))
		if err == nil && len(decoded) > 0 && utf8.Valid(decoded) &&
			!strings.ContainsRune(string(decoded), utf8.RuneError) {
			return string(decoded)
		}
		return raw
	}
	if isASCII(raw) {
		return raw
	}
	encoded, err := charmap.CodePage437.NewEncoder().Bytes([]byte(raw))
	if err != nil || strings.ContainsRune(string(encoded), substitutionByte) {
		// A rune outside CP437 was replaced: the round trip is lossy, keep
		// the name as received.
		return raw
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().Bytes(encoded)
	if err != nil || !utf8.Valid(decoded) || len(decoded) == 0 ||
		strings.ContainsRune(string(decoded), utf8.RuneError) {
		return raw
	}
	return string(decoded)
}

// substitutionByte is CP437's SUB, which x/text emits for unmappable runes.
const substitutionByte = '\x1a'

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// isJunkEntry filters archive metadata that macOS and Windows tooling drops
// into packages and no knowledge flow should ever ingest.
func isJunkEntry(name string) bool {
	for _, segment := range strings.Split(name, "/") {
		switch {
		case segment == "__MACOSX":
			return true
		case strings.HasPrefix(segment, "._"):
			return true
		case segment == ".DS_Store":
			return true
		case segment == "Thumbs.db":
			return true
		}
	}
	return false
}
