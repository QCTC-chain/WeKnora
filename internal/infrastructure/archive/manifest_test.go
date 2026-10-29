package archive

import (
	"errors"
	"path"
	"strings"
	"testing"
)

func manifestZip(t *testing.T, files map[string][]byte) (*Archive, *Manifest) {
	t.Helper()
	data := writeZip(t, files, nil)
	a := openArchive(t, data, DefaultLimits())
	m, err := BuildManifest(a)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	return a, m
}

func TestBuildManifest_RootDocumentWithImages(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md":       []byte("# t\n\n![a](images/a.png)\n![b](images/b.jpg)\n"),
		"images/a.png": []byte("png-a"),
		"images/b.jpg": []byte("jpg-b"),
		"images/c.png": []byte("unused"),
	})
	if len(m.Documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(m.Documents))
	}
	doc := m.Documents[0]
	if doc.Title != "doc.md" || doc.FolderPath != "" || doc.BaseDir != "" {
		t.Fatalf("doc meta = %q/%q/%q", doc.Title, doc.FolderPath, doc.BaseDir)
	}
	if len(doc.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(doc.Images))
	}
	if doc.Images[0].ArchivePath != "images/a.png" || string(doc.Images[0].Data) != "png-a" {
		t.Fatalf("first image = %+v", doc.Images[0])
	}
	if len(doc.Missing) != 0 {
		t.Fatalf("missing = %v", doc.Missing)
	}
}

func TestBuildManifest_SubfolderDocumentGetsFolderPath(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"docs/guide/readme.md":      []byte("![p](assets/pic.jpg)\n"),
		"docs/guide/assets/pic.jpg": []byte("jpg"),
		"root.md":                   []byte("![p](pic.jpg)\n"),
		"pic.jpg":                   []byte("jpg"),
	})
	if len(m.Documents) != 2 {
		t.Fatalf("documents = %d, want 2: %+v", len(m.Documents), m.Documents)
	}
	byName := map[string]*Document{}
	for _, doc := range m.Documents {
		byName[doc.Name] = doc
	}
	sub := byName["docs/guide/readme.md"]
	if sub == nil || sub.FolderPath != "docs/guide" {
		t.Fatalf("sub doc = %+v", sub)
	}
	if len(sub.Images) != 1 || sub.Images[0].ArchivePath != "docs/guide/assets/pic.jpg" {
		t.Fatalf("sub images = %+v", sub.Images)
	}
	root := byName["root.md"]
	if root == nil || root.FolderPath != "" || len(root.Images) != 1 {
		t.Fatalf("root doc = %+v", root)
	}
}

func TestBuildManifest_PercentEscapedReferenceMatches(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md":         []byte("![](images/a%20b.jpg)\n"),
		"images/a b.jpg": []byte("jpg"),
	})
	doc := m.Documents[0]
	if len(doc.Images) != 1 || doc.Images[0].ArchivePath != "images/a b.jpg" {
		t.Fatalf("images = %+v", doc.Images)
	}
	if len(doc.Missing) != 0 {
		t.Fatalf("missing = %v", doc.Missing)
	}
	// The reported reference spelling must be the RAW one, so the rewriter
	// can find it in the document.
	if doc.Images[0].Refs[0] != "images/a%20b.jpg" {
		t.Fatalf("refs = %v, want raw escaped spelling", doc.Images[0].Refs)
	}
}

func TestBuildManifest_BackslashReferenceMatches(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md":       []byte("![](images\\a.png)\n"),
		"images/a.png": []byte("png"),
	})
	doc := m.Documents[0]
	if len(doc.Images) != 1 {
		t.Fatalf("images = %+v, want 1", doc.Images)
	}
}

func TestBuildManifest_RepeatedReferencesDeduplicatePerEntry(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md":        []byte("![1](images/a.png)\n![2](images/a.png)\n![3](images/a%20.png)\n"),
		"images/a.png":  []byte("png"),
		"images/a .png": []byte("png2"),
	})
	doc := m.Documents[0]
	if len(doc.Images) != 2 {
		t.Fatalf("images = %+v, want 2 distinct files", doc.Images)
	}
	// Refs holds the distinct raw spellings pointing at each entry.
	if refs := doc.Images[0].Refs; len(refs) != 1 || refs[0] != "images/a.png" {
		t.Fatalf("refs of first image = %v", refs)
	}
	if refs := doc.Images[1].Refs; len(refs) != 1 || refs[0] != "images/a%20.png" {
		t.Fatalf("refs of second image = %v", refs)
	}
}

func TestBuildManifest_MissingImageReported(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md":       []byte("![ok](images/a.png)\n![gone](images/missing.png)\n"),
		"images/a.png": []byte("png"),
	})
	doc := m.Documents[0]
	if len(doc.Missing) != 1 || doc.Missing[0] != "images/missing.png" {
		t.Fatalf("missing = %v", doc.Missing)
	}
	// A reference to a non-image entry is also "missing", not inlined.
	_, m2 := manifestZip(t, map[string][]byte{
		"doc.md":   []byte("![x](other.md)\n"),
		"other.md": []byte("text"),
	})
	if len(m2.Documents[0].Missing) != 1 {
		t.Fatalf("missing = %v, want non-image entry treated as missing", m2.Documents[0].Missing)
	}
}

func TestBuildManifest_CodeBlockReferencesIgnored(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md": []byte("```\n![ghost](images/nope.png)\n```\n\n    ![ghost2](images/nope2.png)\n"),
	})
	if len(m.Documents[0].Missing) != 0 {
		t.Fatalf("missing = %v, want code-literal refs ignored", m.Documents[0].Missing)
	}
}

func TestBuildManifest_SupportedNonMarkdownImportedAsAttachments(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md":           []byte("![a](images/a.png)\n"),
		"images/a.png":     []byte("png"),
		"manual.pdf":       []byte("pdf"),
		"notes/sheet.xlsx": []byte("xlsx"),
		"readme.txt":       []byte("notes"),
	})
	if len(m.Documents) != 1 {
		t.Fatalf("documents = %d", len(m.Documents))
	}
	if len(m.Attachments) != 3 {
		t.Fatalf("attachments = %d, want 3", len(m.Attachments))
	}
	// Name-sorted order keeps the report stable across producers.
	if m.Attachments[0].Name != "manual.pdf" || m.Attachments[1].Name != "notes/sheet.xlsx" || m.Attachments[2].Name != "readme.txt" {
		t.Fatalf("attachment order = %v", m.Attachments)
	}
	if m.Attachments[1].FolderPath != "notes" || m.Attachments[1].Title != "sheet.xlsx" {
		t.Fatalf("attachment fields = %+v", m.Attachments[1])
	}
	if len(m.Skipped) != 0 {
		t.Fatalf("skipped = %v, want none", m.Skipped)
	}
}

func TestBuildManifest_UnsupportedExtensionsReported(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"doc.md":   []byte("hello\n"),
		"data.bin": []byte("\x00\x01"),
	})
	if len(m.Documents) != 1 || len(m.Attachments) != 0 {
		t.Fatalf("docs=%d attachments=%d", len(m.Documents), len(m.Attachments))
	}
	if len(m.Skipped) != 1 || m.Skipped[0].Name != "data.bin" {
		t.Fatalf("skipped = %v, want data.bin", m.Skipped)
	}
}

func TestBuildManifest_TooManyDocsAborts(t *testing.T) {
	data := writeZip(t, map[string][]byte{
		"a.md": []byte("a"),
		"b.md": []byte("b"),
	}, nil)
	a := openArchive(t, data, Limits{MaxDocuments: 1, MaxEntries: 100, MaxExpandedBytes: 1 << 20, MaxSingleEntryBytes: 1 << 20})
	_, err := BuildManifest(a)
	if !errors.Is(err, ErrTooManyDocs) {
		t.Fatalf("err = %v, want ErrTooManyDocs", err)
	}
}

func TestBuildManifest_DocumentOrderIsStable(t *testing.T) {
	_, m := manifestZip(t, map[string][]byte{
		"c.md": []byte("c"), "a.md": []byte("a"), "b/b.md": []byte("b"),
	})
	var names []string
	for _, doc := range m.Documents {
		names = append(names, doc.Name)
	}
	if strings.Join(names, ",") != "a.md,b/b.md,c.md" {
		t.Fatalf("order = %v, want name-sorted", names)
	}
	if path.Base("b/b.md") != "b.md" {
		t.Fatal("sanity")
	}
}
