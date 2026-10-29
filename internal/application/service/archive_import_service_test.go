package service

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/infrastructure/archive"
	"github.com/Tencent/WeKnora/internal/types"
)

// archiveDoc builds an in-memory Document the way BuildManifest would.
func archiveDoc(content string, images map[string][]byte, missing []string) *archive.Document {
	doc := &archive.Document{
		Name:       "doc.md",
		BaseDir:    "",
		Title:      "doc.md",
		FolderPath: "",
		Content:    []byte(content),
		Missing:    missing,
	}
	for path, data := range images {
		doc.Images = append(doc.Images, archive.ImageFile{
			ArchivePath: path,
			Name:        path,
			Data:        data,
			Refs:        []string{path},
		})
	}
	return doc
}

// TestStoreArchiveImagesRewritesAndCounts pins the rewrite-at-upload contract:
// stored references become provider URLs, missing ones keep their original
// path, and the warnings explain exactly what could not be resolved.
func TestStoreArchiveImagesRewritesAndCounts(t *testing.T) {
	svc := &knowledgeService{}
	fake := &fakeFileService{}
	doc := archiveDoc(
		"![a](images/a.png)\n![b](images/b.jpg \"标题\")\n![gone](images/gone.png)\n",
		map[string][]byte{
			"images/a.png": []byte("A"),
			"images/b.jpg": []byte("B"),
		},
		[]string{"images/gone.png"},
	)

	rewritten, stored, warnings, err := svc.storeArchiveImages(context.Background(), fake, 7, doc)
	if err != nil {
		t.Fatalf("storeArchiveImages: %v", err)
	}
	if stored != 2 {
		t.Fatalf("stored = %d, want 2", stored)
	}
	if len(fake.saved) != 2 {
		t.Fatalf("fake saved = %d, want 2", len(fake.saved))
	}
	for _, key := range []string{"fake://tenant-images/a.png", "fake://tenant-images/b.jpg"} {
		if _, ok := fake.saved[key]; !ok {
			t.Fatalf("missing saved image %s in %v", key, byteMapKeys(fake.saved))
		}
	}
	if !strings.Contains(rewritten, "![a](fake://tenant-images/a.png)") {
		t.Fatalf("a.png not rewritten: %q", rewritten)
	}
	if !strings.Contains(rewritten, `![b](fake://tenant-images/b.jpg "标题")`) {
		t.Fatalf("b.jpg rewritten but title lost: %q", rewritten)
	}
	if !strings.Contains(rewritten, "![gone](images/gone.png)") {
		t.Fatalf("missing image path not preserved: %q", rewritten)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "images/gone.png") {
		t.Fatalf("warnings = %v, want one entry for images/gone.png", warnings)
	}
}

// TestStoreArchiveImagesPerDocumentCap keeps oversized batches from
// cascading: beyond the cap images are skipped with a warning, earlier ones
// stay stored, and the document still imports.
func TestStoreArchiveImagesPerDocumentCap(t *testing.T) {
	svc := &knowledgeService{}
	fake := &fakeFileService{}
	doc := archiveDoc("![a](images/a.png)\n![b](images/b.png)\n",
		map[string][]byte{"images/a.png": []byte("A"), "images/b.png": []byte("B")}, nil)
	doc.Images[0].Data = []byte("A")
	doc.Images[1].Data = []byte("B")
	// Shrink the cap through the package constant path: build 1001 images
	// instead so the real cap is exercised without touching the constant.
	doc.Images = nil
	content := strings.Builder{}
	images := map[string][]byte{}
	for i := 0; i < maxArchiveDocumentImages+1; i++ {
		name := "images/img" + string(rune('a'+i%26)) + strconv.Itoa(i) + ".png"
		content.WriteString("![" + strconv.Itoa(i) + "](" + name + ")\n")
		images[name] = []byte{byte(i % 255)}
	}
	doc.Content = []byte(content.String())
	for path, data := range images {
		doc.Images = append(doc.Images, archive.ImageFile{ArchivePath: path, Name: path, Data: data, Refs: []string{path}})
	}

	rewritten, stored, warnings, err := svc.storeArchiveImages(context.Background(), fake, 7, doc)
	if err != nil {
		t.Fatalf("storeArchiveImages: %v", err)
	}
	if stored != maxArchiveDocumentImages {
		t.Fatalf("stored = %d, want %d", stored, maxArchiveDocumentImages)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "保留原路径") {
		t.Fatalf("warnings = %v, want one cap warning", warnings)
	}
	// Everything above the cap keeps its original relative path.
	if !strings.Contains(rewritten, "](images/img") {
		t.Fatal("references beyond the cap were rewritten anyway")
	}
}

func byteMapKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestImportArchiveAttachmentSkipsImageHandling pins the mixed-package
// behavior: a pdf inside the zip goes through the plain file pipeline — the
// raw bytes are stored, no images are reported, and the item is created.
func TestImportArchiveAttachmentSkipsImageHandling(t *testing.T) {
	repo := &createKnowledgeFileRepoStub{}
	fake := &fakeFileService{}
	enqueuer := &createKnowledgeTaskEnqueuerStub{}
	att := &archive.Attachment{
		Name:       "notes/manual.pdf",
		Title:      "manual.pdf",
		FolderPath: "notes",
		Data:       []byte("%PDF-1.4 fake"),
	}
	ksb := &createKnowledgeFileKBServiceStub{kb: &types.KnowledgeBase{ID: "kb-1"}}
	svc := &knowledgeService{
		repo:      repo,
		kbService: ksb,
		fileSvc:   fake,
		task:      enqueuer,
	}
	item := svc.importArchiveAttachment(
		newCreateKnowledgeFileContext(), ksb.kb, fake, 1, &types.Tenant{}, att, archiveDocumentOptions{},
	)
	if item.Status != types.ArchiveItemCreated {
		t.Fatalf("status = %s, error = %s", item.Status, item.Error)
	}
	if item.ImagesStored != 0 || item.ImagesMissing != 0 {
		t.Fatalf("images = %d/%d, want 0/0", item.ImagesStored, item.ImagesMissing)
	}
	if item.FolderPath != "notes" {
		t.Fatalf("folder = %q", item.FolderPath)
	}
	if repo.createdKnowledge == nil || repo.createdKnowledge.FileName != "manual.pdf" {
		t.Fatalf("created knowledge = %+v", repo.createdKnowledge)
	}
	if enqueuer.calls != 1 {
		t.Fatalf("enqueue calls = %d, want 1", enqueuer.calls)
	}
}
