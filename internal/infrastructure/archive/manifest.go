package archive

import (
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/utils"
)

// maxImageBytes caps one image pulled out of an archive. It matches the
// remote-image cap the document parser applies to fetched images, so archive
// images and URL-fetched images behave identically downstream.
const maxImageBytes = 10 << 20

// imageExtensions decides whether a referenced entry may be inlined as an
// image. A reference that hits a non-image entry (another .md, a .pdf) is
// reported missing rather than inlined.
var imageExtensions = map[string]struct{}{
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {}, ".webp": {}, ".bmp": {},
	".svg": {}, ".tif": {}, ".tiff": {}, ".avif": {}, ".ico": {},
	".heic": {}, ".heif": {},
}

// ImageFile is one archive image that at least one reference in a document
// resolves to. Refs lists every reference spelling (raw, as written in the
// Markdown) that points at it, so the importer can map each spelling to the
// stored URL after saving the bytes once.
type ImageFile struct {
	ArchivePath string
	Name        string
	Data        []byte
	Refs        []string
}

// Document is one Markdown file discovered in an archive, with its resolved
// images and the references that could not be resolved.
type Document struct {
	// Name is the normalized archive path of the Markdown file.
	Name string
	// BaseDir is the directory holding it inside the archive ("" at root).
	BaseDir string
	// Title is the base filename, used as the knowledge title.
	Title string
	// FolderPath is the document's directory inside the archive; it becomes
	// the KB folder_path so multi-folder zips recreate their structure.
	FolderPath string
	// Content is the raw Markdown bytes.
	Content []byte
	// Images are the resolved images in first-reference order.
	Images []ImageFile
	// Missing lists raw references that matched no archive image, in
	// first-appearance order.
	Missing []string
}

// Attachment is one non-Markdown document (pdf, docx, ...) discovered in an
// archive. It imports directly through the same file-upload pipeline, with no
// image handling of its own.
type Attachment struct {
	// Name is the normalized archive path of the file.
	Name string
	// Title is the base filename, used as the knowledge title.
	Title string
	// FolderPath is the document's directory inside the archive; it becomes
	// the KB folder_path so multi-folder zips recreate their structure.
	FolderPath string
	// Data is the raw file bytes.
	Data []byte
}

// Manifest is the outcome of scanning an archive: Documents are Markdown
// files expanded with their images, Attachments are other supported documents
// imported as-is, and Skipped carries entries the import deliberately ignores.
type Manifest struct {
	Documents   []*Document
	Attachments []Attachment
	Skipped     []SkippedEntry
}

// BuildManifest discovers Markdown documents in the archive and, for each,
// resolves every relative image reference against sibling entries. Content is
// read here, so the archive's byte budgets apply; structural limit violations
// (too many documents, budget exhausted) abort the whole import, which the
// handler turns into a 400 before anything is persisted.
func BuildManifest(a *Archive) (*Manifest, error) {
	limits := a.limits
	manifest := &Manifest{Skipped: a.Skipped()}

	var docNames []string
	var attachmentNames []string
	for _, name := range a.Names() {
		ext := strings.ToLower(path.Ext(name))
		switch {
		case ext == ".md" || ext == ".markdown":
			docNames = append(docNames, name)
		case isImageExtension(ext):
			// Image candidates are resolved per document; unreferenced ones
			// are ignored.
		case utils.IsSupportedImportExtension(ext):
			// A non-Markdown document: imported directly, like a single-file
			// upload of the same type.
			attachmentNames = append(attachmentNames, name)
		default:
			manifest.Skipped = append(manifest.Skipped, SkippedEntry{
				Name: name, Reason: "不支持的文件类型，已忽略",
			})
		}
	}
	if len(docNames)+len(attachmentNames) == 0 {
		return manifest, nil
	}
	if len(docNames)+len(attachmentNames) > limits.MaxDocuments {
		return nil, fmt.Errorf("%w（%d > %d）", ErrTooManyDocs, len(docNames)+len(attachmentNames), limits.MaxDocuments)
	}

	// Deterministic order: archive order is central-directory order, which
	// varies by producer; name order keeps reports and folder iteration stable.
	sortStrings(docNames)
	sortStrings(attachmentNames)

	for _, name := range docNames {
		content, err := a.Read(name)
		if err != nil {
			return nil, err
		}
		doc := buildDocument(a, name, content)
		manifest.Documents = append(manifest.Documents, doc)
	}
	for _, name := range attachmentNames {
		data, err := a.Read(name)
		if err != nil {
			return nil, err
		}
		baseDir := ""
		if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
			baseDir = name[:idx]
		}
		manifest.Attachments = append(manifest.Attachments, Attachment{
			Name: name, Title: path.Base(name), FolderPath: baseDir, Data: data,
		})
	}
	return manifest, nil
}

// buildDocument resolves one document's relative image references against the
// archive. A reference matches an entry when its percent-decoded or raw
// spelling, relative to the document's directory (or archive root), names an
// image entry. Unmatched references are recorded, never fatal: the document
// keeps its original path and the import reports a warning.
func buildDocument(a *Archive, name string, content []byte) *Document {
	baseDir := ""
	if idx := strings.LastIndexByte(name, '/'); idx >= 0 {
		baseDir = name[:idx]
	}
	doc := &Document{
		Name:       name,
		BaseDir:    baseDir,
		Title:      path.Base(name),
		FolderPath: baseDir,
		Content:    content,
	}

	indexByPath := make(map[string]int)
	seenMissing := make(map[string]struct{})

	for _, ref := range docparser.CollectRelativeImageRefs(string(content)) {
		entry, ok := resolveReference(a, baseDir, ref.Ref)
		if !ok {
			if _, dup := seenMissing[ref.Ref]; !dup {
				seenMissing[ref.Ref] = struct{}{}
				doc.Missing = append(doc.Missing, ref.Ref)
			}
			continue
		}
		if idx, dup := indexByPath[entry]; dup {
			if idx >= 0 && !containsString(doc.Images[idx].Refs, ref.Ref) {
				doc.Images[idx].Refs = append(doc.Images[idx].Refs, ref.Ref)
			}
			continue
		}
		data, err := a.Read(entry)
		if err != nil || len(data) > maxImageBytes {
			// Oversized or unreadable image: keep the original path and let
			// the report explain, instead of failing the document.
			if _, dup := seenMissing[ref.Ref]; !dup {
				seenMissing[ref.Ref] = struct{}{}
				doc.Missing = append(doc.Missing, ref.Ref)
			}
			indexByPath[entry] = -1 // do not retry this entry
			continue
		}
		doc.Images = append(doc.Images, ImageFile{
			ArchivePath: entry,
			Name:        path.Base(entry),
			Data:        data,
			Refs:        []string{ref.Ref},
		})
		indexByPath[entry] = len(doc.Images) - 1
	}
	return doc
}

// resolveReference maps a raw Markdown reference to an archive entry name.
// Candidate spellings (percent-decoded and raw, "./"-stripped, root-relative)
// are tried first relative to the document's directory, then relative to the
// archive root, mirroring how Markdown loaders resolve sibling assets.
func resolveReference(a *Archive, baseDir, rawRef string) (string, bool) {
	spellings := referenceSpellings(rawRef)
	for _, group := range spellings {
		if baseDir != "" {
			candidate := path.Clean(path.Join(baseDir, group))
			if a.Has(candidate) && isImageExtension(strings.ToLower(path.Ext(candidate))) {
				return candidate, true
			}
		}
		candidate := path.Clean(group)
		if a.Has(candidate) && isImageExtension(strings.ToLower(path.Ext(candidate))) {
			return candidate, true
		}
	}
	return "", false
}

// referenceSpellings returns the normalized candidate paths for one raw
// reference, decoded first (a "%20" in the Markdown is a space in the entry
// name), then raw. Both are anchored to the document directory by the caller.
func referenceSpellings(rawRef string) []string {
	seen := make(map[string]struct{}, 4)
	var out []string
	add := func(s string) {
		s = strings.ReplaceAll(s, "\\", "/")
		s = strings.TrimPrefix(s, "./")
		s = strings.TrimPrefix(s, "/")
		if s == "" {
			return
		}
		if _, dup := seen[s]; dup {
			return
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if decoded, err := url.PathUnescape(rawRef); err == nil {
		add(decoded)
	}
	add(rawRef)
	return out
}

func isImageExtension(ext string) bool {
	_, ok := imageExtensions[ext]
	return ok
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

func sortStrings(list []string) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j] < list[j-1]; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}
