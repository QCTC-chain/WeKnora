package archive

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// writeZip builds an in-memory archive from name -> content pairs. When
// rawName is true the name bytes are stored verbatim without the UTF-8 flag,
// which is how GBK-producing zip tools write Chinese entry names.
func writeZip(t *testing.T, files map[string][]byte, nonUTF8Names map[string]bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		if nonUTF8Names[name] {
			h := &zip.FileHeader{Name: name, Method: zip.Store, NonUTF8: true}
			w, err := zw.CreateHeader(h)
			if err != nil {
				t.Fatalf("create header %q: %v", name, err)
			}
			if _, err := w.Write(content); err != nil {
				t.Fatalf("write %q: %v", name, err)
			}
			continue
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatalf("write %q: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func openArchive(t *testing.T, data []byte, limits Limits) *Archive {
	t.Helper()
	a, err := Open(bytes.NewReader(data), int64(len(data)), limits)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return a
}

func TestOpen_NormalizesAndSkipsJunk(t *testing.T) {
	data := writeZip(t, map[string][]byte{
		"doc.md":                   []byte("# hi"),
		"__MACOSX/doc.md/._doc.md": []byte("junk"),
		".DS_Store":                []byte("junk"),
		"folder/._page.png":        []byte("junk"),
		"Thumbs.db":                []byte("junk"),
	}, nil)
	a := openArchive(t, data, DefaultLimits())

	names := a.Names()
	if len(names) != 1 || names[0] != "doc.md" {
		t.Fatalf("names = %v, want [doc.md]", names)
	}
	skipped := a.Skipped()
	if len(skipped) != 4 {
		t.Fatalf("skipped = %v, want 4 entries", skipped)
	}
}

func TestOpen_RejectsTraversalEntries(t *testing.T) {
	data := writeZip(t, map[string][]byte{
		"doc.md":         []byte("# hi"),
		"../evil.md":     []byte("evil"),
		"a/../../top.md": []byte("evil"),
		"nested/ok.md":   []byte("ok"),
	}, nil)
	a := openArchive(t, data, DefaultLimits())

	names := a.Names()
	if len(names) != 2 || names[0] != "doc.md" || names[1] != "nested/ok.md" {
		t.Fatalf("names = %v, want traversal entries dropped", names)
	}
	for _, name := range names {
		if strings.Contains(name, "..") {
			t.Fatalf("traversal survived: %q", name)
		}
	}
}

func TestOpen_RecoversGBKEntryNames(t *testing.T) {
	// Encode the Chinese directory and file name the way a Chinese Windows
	// zip tool does: raw GBK bytes without the UTF-8 flag. Go's reader then
	// hands us a CP437-mangled name; Open must recover the original.
	gbkName, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte("手册/说明.md"))
	if err != nil {
		t.Fatalf("encode GBK: %v", err)
	}
	data := writeZip(t, map[string][]byte{string(gbkName): []byte("# 内容")}, map[string]bool{string(gbkName): true})
	a := openArchive(t, data, DefaultLimits())

	names := a.Names()
	if len(names) != 1 || names[0] != "手册/说明.md" {
		t.Fatalf("names = %q, want [手册/说明.md]", names)
	}
	content, err := a.Read(names[0])
	if err != nil || string(content) != "# 内容" {
		t.Fatalf("read recovered name: %v %q", err, content)
	}
}

func TestOpen_KeepsUTF8FlaggedNames(t *testing.T) {
	data := writeZip(t, map[string][]byte{"手册/说明.md": []byte("x")}, nil)
	a := openArchive(t, data, DefaultLimits())
	names := a.Names()
	if len(names) != 1 || names[0] != "手册/说明.md" {
		t.Fatalf("names = %q, want [手册/说明.md]", names)
	}
}

func TestOpen_SkipsNonRegularEntries(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: "link.md", Method: zip.Store}
	h.SetMode(fs.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(h)
	if err != nil {
		t.Fatalf("create symlink header: %v", err)
	}
	if _, err := w.Write([]byte("../../etc/passwd")); err != nil {
		t.Fatalf("write symlink: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	a := openArchive(t, buf.Bytes(), DefaultLimits())
	if names := a.Names(); len(names) != 0 {
		t.Fatalf("names = %v, want symlink skipped", names)
	}
}

func TestRead_EnforcesSingleEntryLimit(t *testing.T) {
	// Highly compressible content far beyond the per-entry cap: the declared
	// size cannot be trusted, so the reader must stop at the byte cap.
	data := writeZip(t, map[string][]byte{
		"big.md": bytes.Repeat([]byte("a"), 1<<20),
	}, nil)
	a := openArchive(t, data, Limits{MaxDocuments: 10, MaxEntries: 100, MaxExpandedBytes: 512 << 20, MaxSingleEntryBytes: 1024})
	if _, err := a.Read("big.md"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestRead_EnforcesCumulativeBudget(t *testing.T) {
	data := writeZip(t, map[string][]byte{
		"one.md": bytes.Repeat([]byte("a"), 600),
		"two.md": bytes.Repeat([]byte("b"), 600),
	}, nil)
	a := openArchive(t, data, Limits{MaxDocuments: 10, MaxEntries: 100, MaxExpandedBytes: 1000, MaxSingleEntryBytes: 1 << 20})
	if _, err := a.Read("one.md"); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if _, err := a.Read("two.md"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("second read err = %v, want ErrTooLarge", err)
	}
}

func TestOpen_EnforcesEntryCount(t *testing.T) {
	files := map[string][]byte{}
	for i := 0; i < 5; i++ {
		files[strconv.Itoa(i)+".md"] = []byte("x")
	}
	data := writeZip(t, files, nil)
	_, err := Open(bytes.NewReader(data), int64(len(data)), Limits{MaxDocuments: 10, MaxEntries: 3, MaxExpandedBytes: 1 << 20, MaxSingleEntryBytes: 1 << 20})
	if !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("err = %v, want ErrTooManyFiles", err)
	}
}

func TestOpen_RejectsNonZip(t *testing.T) {
	_, err := Open(bytes.NewReader([]byte("this is not a zip file at all")), 28, DefaultLimits())
	if !errors.Is(err, ErrBadArchive) {
		t.Fatalf("err = %v, want ErrBadArchive", err)
	}
}
