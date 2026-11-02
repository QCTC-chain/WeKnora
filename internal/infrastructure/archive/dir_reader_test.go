package archive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDir builds an extracted-package folder from rel -> content pairs and
// returns its root.
func writeDir(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// TestOpenDirectory_NamesJunkAndSkips pins the walk contract: junk metadata
// is filtered, symlinks and other non-regular files are skipped with a
// reason, and names are slash-separated paths relative to the root.
func TestOpenDirectory_NamesJunkAndSkips(t *testing.T) {
	root := writeDir(t, map[string]string{
		"docs/a.md":        "# hi",
		"images/a.png":     "png",
		"__MACOSX/junk":    "junk",
		"docs/._a.md":      "junk",
		".DS_Store":        "junk",
		"Thumbs.db":        "junk",
		"notes/keep.md":    "keep",
		"notes/nested.txt": "unsupported type stays an entry (manifest skips it)",
	})

	link := filepath.Join(root, "docs", "linked.md")
	if err := os.Symlink(filepath.Join(root, "docs", "a.md"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	d, err := OpenDirectory(root, Limits{MaxDocuments: 10, MaxEntries: 100})
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	got := strings.Join(d.Names(), "|")
	want := strings.Join([]string{
		"docs/a.md",
		"images/a.png",
		"notes/keep.md",
		"notes/nested.txt",
	}, "|")
	if got != want {
		t.Fatalf("names = %q, want %q (junk and symlinks skipped)", got, want)
	}
	for _, s := range d.Skipped() {
		switch s.Name {
		case "docs/linked.md", ".DS_Store", "Thumbs.db":
			if !strings.Contains(s.Reason, "忽略") {
				t.Fatalf("skip reason for %s = %q", s.Name, s.Reason)
			}
		}
	}
	if d.Has("docs/._a.md") || d.Has("__MACOSX/junk") {
		t.Fatal("junk entries must not be indexed")
	}
}

// TestOpenDirectory_BuildManifest keeps the directory path honest: the same
// manifest builder that scans zips must resolve relative image references
// against sibling entries and classify attachments identically.
func TestOpenDirectory_BuildManifest(t *testing.T) {
	root := writeDir(t, map[string]string{
		"docs/a.md":       "![x](../images/a.png)\n![gone](../images/missing.png)",
		"images/a.png":    "PNGDATA",
		"docs/spec.pdf":   "%PDF",
		"docs/ignore.bin": "unsupported",
	})
	d, err := OpenDirectory(root, Limits{MaxDocuments: 10, MaxEntries: 100, MaxExpandedBytes: 1 << 20, MaxSingleEntryBytes: 1 << 20, MaxImageBytes: 1 << 20})
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	m, err := BuildManifest(d)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(m.Documents) != 1 || m.Documents[0].Name != "docs/a.md" {
		t.Fatalf("documents = %+v, want exactly docs/a.md", m.Documents)
	}
	doc := m.Documents[0]
	if doc.FolderPath != "docs" {
		t.Fatalf("folder path = %q, want docs", doc.FolderPath)
	}
	if len(doc.Images) != 1 || doc.Images[0].ArchivePath != "images/a.png" || string(doc.Images[0].Data) != "PNGDATA" {
		t.Fatalf("images = %+v, want images/a.png resolved", doc.Images)
	}
	if len(doc.Missing) != 1 || doc.Missing[0] != "../images/missing.png" {
		t.Fatalf("missing = %v, want the missing reference", doc.Missing)
	}
	if len(m.Attachments) != 1 || m.Attachments[0].Name != "docs/spec.pdf" {
		t.Fatalf("attachments = %+v, want docs/spec.pdf", m.Attachments)
	}
}

// TestOpenDirectory_Limits pins the budget behaviour the zip reader offers:
// too many entries aborts the open, single-file and total byte caps abort
// the read, and the per-image cap turns oversized images into missing
// references instead of failing the document.
func TestOpenDirectory_Limits(t *testing.T) {
	root := writeDir(t, map[string]string{
		"a.md":  "![x](a.png)",
		"b.md":  "b",
		"a.png": "PNG",
	})

	if _, err := OpenDirectory(root, Limits{MaxDocuments: 10, MaxEntries: 1}); err != ErrTooManyFiles {
		t.Fatalf("MaxEntries=1 err = %v, want ErrTooManyFiles", err)
	}

	tight := Limits{MaxDocuments: 10, MaxEntries: 100, MaxExpandedBytes: 2, MaxSingleEntryBytes: 1 << 20}
	d, err := OpenDirectory(root, tight)
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	if _, err := d.Read("a.md"); err != ErrTooLarge {
		t.Fatalf("read over expanded budget err = %v, want ErrTooLarge", err)
	}

	big := writeDir(t, map[string]string{"big.bin": strings.Repeat("x", 4096)})
	d2, err := OpenDirectory(big, Limits{MaxDocuments: 10, MaxEntries: 100, MaxExpandedBytes: 1 << 20, MaxSingleEntryBytes: 1024})
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	if _, err := d2.Read("big.bin"); err == nil || !strings.Contains(err.Error(), "单文件大小限制") {
		t.Fatalf("read over single-entry budget err = %v, want single-entry cap error", err)
	}

	// An image over the manifest's per-image cap becomes a missing ref.
	small := writeDir(t, map[string]string{
		"a.md":  "![x](a.png)",
		"a.png": strings.Repeat("P", 100),
	})
	d3, err := OpenDirectory(small, Limits{MaxDocuments: 10, MaxEntries: 100, MaxExpandedBytes: 1 << 20, MaxSingleEntryBytes: 1 << 20, MaxImageBytes: 10})
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	m, err := BuildManifest(d3)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if len(m.Documents) != 1 || len(m.Documents[0].Images) != 0 || len(m.Documents[0].Missing) != 1 {
		t.Fatalf("manifest = %+v, want one doc with the oversized image reported missing", m.Documents)
	}
}

// TestOpenDirectory_MissingRoot turns a bad mount into the same user-facing
// error the zip path produces for a bad archive.
func TestOpenDirectory_MissingRoot(t *testing.T) {
	_, err := OpenDirectory(filepath.Join(t.TempDir(), "nope"), Limits{MaxEntries: 10})
	if err == nil || !strings.Contains(err.Error(), "无法读取") {
		t.Fatalf("err = %v, want ErrBadArchive wrap", err)
	}
}

// TestDirectoryArchive_ImplementsArchiveSource guards the interface that
// lets BuildManifest scan zips and folders through one signature.
func TestDirectoryArchive_ImplementsArchiveSource(t *testing.T) {
	root := writeDir(t, map[string]string{"a.md": "a"})
	d, err := OpenDirectory(root, Limits{MaxDocuments: 5, MaxEntries: 10})
	if err != nil {
		t.Fatalf("OpenDirectory: %v", err)
	}
	var src ArchiveSource = d
	if len(src.Names()) != 1 || !src.Has("a.md") || src.documentLimit() != 5 {
		t.Fatalf("ArchiveSource mismatch: names=%v has=%v limit=%d",
			src.Names(), src.Has("a.md"), src.documentLimit())
	}
}
