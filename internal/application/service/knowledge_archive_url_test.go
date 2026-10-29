package service

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

// buildTestArchiveZip builds an in-memory zip with one markdown doc, its
// image, and a pdf attachment — the same package shape the URL import must
// expand.
func buildTestArchiveZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	files := []struct{ name, body string }{
		{name: "pkg/readme.md", body: ""},
		{name: "pkg/images/logo.png", body: "PNG"},
		{name: "pkg/spec.pdf", body: "%PDF-1.4 fake"},
	}
	for _, f := range files {
		fw, err := w.Create(f.name)
		if err != nil {
			t.Fatalf("create zip entry: %v", err)
		}
		body := f.body
		if f.name == "pkg/readme.md" {
			body = "# 说明\n\n![logo](images/logo.png)\n"
		}
		if f.name == "pkg/spec.pdf" {
			body = "%PDF-1.4 fake"
		}
		if _, err := fw.Write([]byte(body)); err != nil {
			t.Fatalf("write zip entry: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

// TestCreateKnowledgeFromArchiveBytesExpandsPackage pins the zip file-URL
// tail: the archive is expanded into one markdown knowledge plus a direct
// pdf import, folder paths map from the top-level directory, and the image
// reference is rewritten at upload time.
func TestCreateKnowledgeFromArchiveBytesExpandsPackage(t *testing.T) {
	repo := &createKnowledgeFileRepoStub{}
	fake := &fakeFileService{}
	enqueuer := &createKnowledgeTaskEnqueuerStub{}
	ksb := &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}}
	svc := &knowledgeService{repo: repo, kbService: ksb, fileSvc: fake, task: enqueuer}

	report, err := svc.createKnowledgeFromArchiveBytes(
		newCreateKnowledgeFileContext(), "kb-1", buildTestArchiveZip(t),
		nil, "", []string{}, "", nil,
	)
	if err != nil {
		t.Fatalf("createKnowledgeFromArchiveBytes: %v", err)
	}
	if report.Type != types.TypeArchiveImportReport {
		t.Fatalf("report type = %q", report.Type)
	}
	if report.CreatedCount() != 2 || report.FailedCount() != 0 {
		t.Fatalf("counts: created=%d failed=%d items=%+v", report.CreatedCount(), report.FailedCount(), report.Items)
	}
	folders := map[string]string{}
	var mdItem *types.ArchiveImportItem
	for i := range report.Items {
		folders[report.Items[i].Title] = report.Items[i].FolderPath
		if strings.HasSuffix(report.Items[i].Title, ".md") {
			mdItem = &report.Items[i]
		}
	}
	if folders["readme.md"] != "pkg" || folders["spec.pdf"] != "pkg" {
		t.Fatalf("folder paths = %v", folders)
	}
	if mdItem == nil || mdItem.ImagesStored != 1 {
		t.Fatalf("markdown item = %+v", mdItem)
	}
	// fakeFileService records both the rewritten markdown and the image by
	// file name: readme.md + images/logo.png.
	mdBytes, ok := fake.saved["fake://tenant-readme.md"]
	if !ok {
		t.Fatalf("markdown not stored: %v", byteMapKeys(fake.saved))
	}
	if !strings.Contains(string(mdBytes), "fake://tenant-logo.png") {
		t.Fatalf("image not rewritten at upload: %q", mdBytes)
	}
	if _, ok := fake.saved["fake://tenant-logo.png"]; !ok {
		t.Fatalf("image bytes not stored: %v", byteMapKeys(fake.saved))
	}
	if enqueuer.calls != 2 {
		t.Fatalf("enqueue calls = %d, want 2 (md parse + pdf direct)", enqueuer.calls)
	}
}

// TestCreateKnowledgeFromArchiveBytesRejectsEmptyPackage: a zip without any
// importable document must fail with the user-facing reason.
func TestCreateKnowledgeFromArchiveBytesRejectsEmptyPackage(t *testing.T) {
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, _ := w.Create("only/notes.xyz")
	fw.Write([]byte("plain text"))
	w.Close()

	svc := &knowledgeService{
		kbService: &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}},
		fileSvc:   &fakeFileService{},
	}
	_, err := svc.createKnowledgeFromArchiveBytes(
		newCreateKnowledgeFileContext(), "kb-1", buf.Bytes(), nil, "", nil, "", nil,
	)
	if err == nil || !strings.Contains(err.Error(), "未找到可导入的文档") {
		t.Fatalf("err = %v, want empty-package rejection", err)
	}
}

// TestIsZipURLRequest pins the routing decision for URL imports.
func TestIsZipURLRequest(t *testing.T) {
	cases := []struct {
		url, fileName, fileType string
		want                    bool
	}{
		{"https://e.com/pkg.zip", "", "", true},
		{"https://e.com/docs", "bundle.ZIP", "", true},
		{"https://e.com/docs", "", "zip", true},
		{"https://e.com/report.pdf", "", "", false},
		{"https://e.com/report.pdf", "report.pdf", "pdf", false},
		{"https://e.com/page", "", "", false},
		{"://bad", "x.zip", "", true}, // fileType hint wins before URL parse
	}
	for _, tc := range cases {
		if got := isZipURLRequest(tc.url, tc.fileName, tc.fileType); got != tc.want {
			t.Errorf("isZipURLRequest(%q,%q,%q) = %v, want %v", tc.url, tc.fileName, tc.fileType, got, tc.want)
		}
	}
}
