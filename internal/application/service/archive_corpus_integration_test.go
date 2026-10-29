package service

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/infrastructure/archive"
	"github.com/Tencent/WeKnora/internal/types"
)

// corpusArchiveBytes packs the real export corpus (markdown + images/) into a
// zip the way an export tool would, so the archive pipeline is exercised with
// production-shaped input.
func corpusArchiveBytes(t *testing.T) ([]byte, int) {
	t.Helper()
	// Host path is mounted read-only at /corpus inside the build container;
	// both spellings work so the test also runs on a bare host checkout.
	corpusDir := "/corpus/hybrid_auto"
	if _, err := os.Stat(corpusDir); err != nil {
		corpusDir = "/home/destplus/corpus/hybrid_auto"
	}
	md, err := os.ReadFile(filepath.Join(corpusDir, "“e-交易”APP四川电力用户注册操作手册-20240617.md"))
	if err != nil {
		t.Skipf("real corpus not available: %v", err)
	}
	imagesDir := filepath.Join(corpusDir, "images")
	entries, err := os.ReadDir(imagesDir)
	if err != nil {
		t.Skipf("corpus images not available: %v", err)
	}

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	mustCreate := func(name string, data []byte) {
		t.Helper()
		fw, err := w.Create("manual/" + name)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustCreate("“e-交易”APP四川电力用户注册操作手册-20240617.md", md)
	imageCount := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(imagesDir, e.Name()))
		if err != nil {
			t.Fatalf("read corpus image: %v", err)
		}
		mustCreate("images/"+e.Name(), data)
		imageCount++
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes(), imageCount
}

// TestRealCorpusArchiveImport is the P0 integration gate: the real 29-image
// export zip must expand into one knowledge whose stored markdown references
// only provider URLs — no relative path survives, nothing is missing.
func TestRealCorpusArchiveImport(t *testing.T) {
	data, imageCount := corpusArchiveBytes(t)

	arc, err := archive.Open(bytes.NewReader(data), int64(len(data)), archive.DefaultLimits())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	manifest, err := archive.BuildManifest(arc)
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(manifest.Documents) != 1 {
		t.Fatalf("documents = %d, want 1", len(manifest.Documents))
	}
	doc := manifest.Documents[0]
	t.Logf("corpus zip: images packaged=%d, referenced refs=%d", imageCount, strings.Count(string(doc.Content), "](images/"))
	// The disk has 41 image files but only 29 are referenced by the markdown;
	// the manifest collects exactly the referenced set and ignores orphans.
	if len(doc.Images) != 29 {
		t.Fatalf("referenced images = %d, want 29", len(doc.Images))
	}
	if len(doc.Missing) != 0 {
		t.Fatalf("missing = %v", doc.Missing)
	}
	refs := strings.Count(string(doc.Content), "](images/")
	if refs != 29 {
		t.Fatalf("relative refs in source = %d, want 29", refs)
	}

	repo := &createKnowledgeFileRepoStub{}
	fake := &fakeFileService{}
	enqueuer := &createKnowledgeTaskEnqueuerStub{}
	ksb := &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}}
	svc := &knowledgeService{repo: repo, kbService: ksb, fileSvc: fake, task: enqueuer}
	item := svc.importArchiveDocument(
		newCreateKnowledgeFileContext(), ksb.kb, fake, 1, &types.Tenant{}, doc, archiveDocumentOptions{},
	)
	if item.Status != types.ArchiveItemCreated {
		t.Fatalf("status = %s, error = %s", item.Status, item.Error)
	}
	if item.ImagesStored != 29 || item.ImagesMissing != 0 {
		t.Fatalf("images = %d stored / %d missing", item.ImagesStored, item.ImagesMissing)
	}
	if len(item.Warnings) != 0 {
		t.Fatalf("warnings = %v", item.Warnings)
	}
	stored, ok := fake.saved["fake://tenant-"+doc.Title]
	if !ok {
		t.Fatalf("markdown not stored under %q; keys=%v", doc.Title, byteMapKeys(fake.saved))
	}
	remaining := strings.Count(string(stored), "](images/")
	if remaining != 0 {
		t.Fatalf("%d relative image refs survived the rewrite", remaining)
	}
	if !strings.Contains(string(stored), "](fake://tenant-") {
		t.Fatal("no provider URLs in the stored markdown")
	}
	if repo.createdKnowledge == nil || repo.createdKnowledge.FileSize != int64(len(stored)) {
		t.Fatalf("knowledge = %+v", repo.createdKnowledge)
	}
	if enqueuer.calls != 1 {
		t.Fatalf("parse task enqueued %d times, want 1", enqueuer.calls)
	}
}
