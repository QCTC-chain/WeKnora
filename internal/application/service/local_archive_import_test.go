package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func TestEnumerateLocalArchives(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.zip", "a")
	write("nested/b.ZIP", "b") // extension match is case-insensitive
	write("nested/deep/c.zip", "c")
	write("skip-me.bin", "not a zip, not a document")
	write("imported/done.zip", "already imported")
	write("failed/broken.zip", "previously failed")
	write(".hidden/secret.zip", "hidden dir")
	write("notes/.DS_Store", "junk")

	packages, rootHasDocs, rootDirectDocs, err := enumerateLocalPackages(root)
	if err != nil {
		t.Fatalf("enumerateLocalPackages: %v", err)
	}
	want := []string{
		filepath.Join(root, "a.zip"),
		filepath.Join(root, "nested", "b.ZIP"),
		filepath.Join(root, "nested", "deep", "c.zip"),
	}
	got := make([]string, len(packages))
	for i, pkg := range packages {
		got[i] = pkg.Path
		if pkg.Kind != types.LocalArchiveKindZip {
			t.Fatalf("package %s kind = %s, want zip", pkg.Path, pkg.Kind)
		}
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("packages = %v, want %v (sorted, managed/hidden dirs skipped)", got, want)
	}
	if rootHasDocs || rootDirectDocs {
		t.Fatalf("rootHasDocs=%v rootDirectDocs=%v, want both false (no documents in tree)", rootHasDocs, rootDirectDocs)
	}
}

// TestEnumerateLocalPackages_DirUnits pins the directory-package discovery
// rule: the topmost directory whose subtree holds a supported document is
// one package (its own subtree is never searched further), zips outside
// unit subtrees stay importable, and a zip inside a unit is part of that
// unit's content — exactly like an unsupported entry inside a zip.
func TestEnumerateLocalPackages_DirUnits(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// unit1 mirrors an extracted zip: docs nested, images sibling-rooted.
	write("unit1/docs/a.md", "![x](../images/x.png)")
	write("unit1/images/x.png", "png")
	write("unit1/inner.zip", "ignored: inside a unit")
	// A pure container dir: no documents anywhere, its zips stay visible.
	write("plain/b.zip", "b")
	// A batch dir without direct documents: the topmost doc-bearing level
	// (batch2) wins, its sub-units are not picked separately.
	write("batch2/u1/a.md", "a")
	write("batch2/u2/b.md", "b")
	// Images-only dirs are not documents (BuildManifest would not import
	// them as such).
	write("picsonly/pic.png", "png")

	packages, _, _, err := enumerateLocalPackages(root)
	if err != nil {
		t.Fatalf("enumerateLocalPackages: %v", err)
	}
	type wantPkg struct {
		path string
		kind string
	}
	want := []wantPkg{
		{filepath.Join(root, "batch2"), types.LocalArchiveKindDir},
		{filepath.Join(root, "plain", "b.zip"), types.LocalArchiveKindZip},
		{filepath.Join(root, "unit1"), types.LocalArchiveKindDir},
	}
	if len(packages) != len(want) {
		t.Fatalf("packages = %+v, want %d entries", packages, len(want))
	}
	for i, w := range want {
		if packages[i].Path != w.path || packages[i].Kind != w.kind {
			t.Fatalf("packages[%d] = {%s %s}, want {%s %s}", i,
				packages[i].Path, packages[i].Kind, w.path, w.kind)
		}
	}
}

// TestEnumerateLocalPackages_RootAsUnit pins the fallback that makes
// "mount the folder instead of zipping it" work: with no packages anywhere
// but documents below the root, nothing is enumerated and the caller
// imports the root itself as one package.
func TestEnumerateLocalPackages_RootAsUnit(t *testing.T) {
	root := t.TempDir()
	write := func(rel string, content string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/a.md", "![x](images/x.png)")
	write("docs/images/x.png", "png")

	packages, rootHasDocs, rootDirectDocs, err := enumerateLocalPackages(root)
	if err != nil {
		t.Fatalf("enumerateLocalPackages: %v", err)
	}
	if len(packages) != 0 {
		t.Fatalf("packages = %+v, want none (root is the package)", packages)
	}
	if !rootHasDocs {
		t.Fatal("rootHasDocs = false, want true")
	}
	if rootDirectDocs {
		t.Fatal("rootDirectDocs = true, want false (documents live in docs/)")
	}
}

func TestEnumerateLocalArchives_EmptyAndMissing(t *testing.T) {
	empty := t.TempDir()
	packages, rootHasDocs, _, err := enumerateLocalPackages(empty)
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if len(packages) != 0 || rootHasDocs {
		t.Fatalf("packages = %+v rootHasDocs=%v, want none/false", packages, rootHasDocs)
	}
	if _, _, _, err := enumerateLocalPackages(filepath.Join(empty, "nope")); err == nil {
		t.Fatal("missing dir should error")
	}
}

func TestMoveLocalArchive(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested", "deep")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	zipPath := filepath.Join(nested, "pkg.zip")
	if err := os.WriteFile(zipPath, []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}

	dest, err := moveLocalArchive(root, zipPath, "imported")
	if err != nil {
		t.Fatalf("moveLocalArchive: %v", err)
	}
	wantDest := filepath.Join(root, "imported", "nested", "deep", "pkg.zip")
	if dest != wantDest {
		t.Fatalf("dest = %s, want %s", dest, wantDest)
	}
	if _, err := os.Stat(zipPath); !os.IsNotExist(err) {
		t.Fatalf("source still exists (err=%v)", err)
	}
	if data, err := os.ReadFile(dest); err != nil || string(data) != "zip" {
		t.Fatalf("dest content = %q, err=%v", data, err)
	}
}

func TestMoveLocalArchive_FlatRootFile(t *testing.T) {
	root := t.TempDir()
	zipPath := filepath.Join(root, "pkg.zip")
	if err := os.WriteFile(zipPath, []byte("zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest, err := moveLocalArchive(root, zipPath, "failed")
	if err != nil {
		t.Fatalf("moveLocalArchive: %v", err)
	}
	if dest != filepath.Join(root, "failed", "pkg.zip") {
		t.Fatalf("dest = %s", dest)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("dest missing: %v", err)
	}
}

// The parse budgets must stay a warning only: the import limits are relaxed,
// the parser budgets are not, and the gap is surfaced to the operator.
func TestParserBudgetWarning(t *testing.T) {
	t.Run("tiny usage stays silent", func(t *testing.T) {
		warn, over := parserBudgetWarning(1000, 100<<20)
		if over || warn != "" {
			t.Fatalf("at-budget usage must not warn, got %q %v", warn, over)
		}
	})

	t.Run("image count overrun warns", func(t *testing.T) {
		warn, over := parserBudgetWarning(1001, 1<<20)
		if !over || !strings.Contains(warn, "解析器单文档预算") {
			t.Fatalf("want budget warning, got %q %v", warn, over)
		}
	})

	t.Run("byte overrun warns", func(t *testing.T) {
		warn, over := parserBudgetWarning(10, (100<<20)+1)
		if !over || !strings.Contains(warn, "解析器单文档预算") {
			t.Fatalf("want budget warning, got %q %v", warn, over)
		}
	})
}

// localGuardKBService / localGuardTenantRepo are the minimal fakes the
// ImportLocalArchives guard path touches before any heavy dependency.
type localGuardKBService struct {
	interfaces.KnowledgeBaseService
	kb *types.KnowledgeBase
}

func (s *localGuardKBService) GetKnowledgeBaseByIDOnly(context.Context, string) (*types.KnowledgeBase, error) {
	return s.kb, nil
}

type localGuardTenantRepo struct {
	interfaces.TenantRepository
}

func (r *localGuardTenantRepo) GetTenantByID(context.Context, uint64) (*types.Tenant, error) {
	return &types.Tenant{}, nil
}

// TestImportLocalArchivesGuards pins the operator-facing validation of the
// import-archives entry point: FAQ KBs, unusable directories and zip-less
// directories are rejected with actionable errors before any storage or
// task-queue dependency is touched.
func TestImportLocalArchivesGuards(t *testing.T) {
	newKB := func(kbType string) *types.KnowledgeBase {
		return &types.KnowledgeBase{ID: "kb-1", TenantID: 7, Type: kbType}
	}

	t.Run("FAQ knowledge base is rejected", func(t *testing.T) {
		svc := &knowledgeService{fileSvc: &fakeFileService{}, kbService: &localGuardKBService{kb: newKB(types.KnowledgeBaseTypeFAQ)}, tenantRepo: &localGuardTenantRepo{}}
		dir := t.TempDir()
		_, err := svc.ImportLocalArchives(context.Background(), "kb-1", dir)
		if err == nil || !strings.Contains(err.Error(), "FAQ") {
			t.Fatalf("err = %v, want FAQ rejection", err)
		}
	})

	t.Run("missing directory is rejected", func(t *testing.T) {
		svc := &knowledgeService{fileSvc: &fakeFileService{}, kbService: &localGuardKBService{kb: newKB(types.KnowledgeBaseTypeDocument)}, tenantRepo: &localGuardTenantRepo{}}
		_, err := svc.ImportLocalArchives(context.Background(), "kb-1", filepath.Join(t.TempDir(), "nope"))
		if err == nil || !strings.Contains(err.Error(), "导入目录不可访问") {
			t.Fatalf("err = %v, want missing-directory rejection", err)
		}
	})

	t.Run("file path is rejected", func(t *testing.T) {
		svc := &knowledgeService{fileSvc: &fakeFileService{}, kbService: &localGuardKBService{kb: newKB(types.KnowledgeBaseTypeDocument)}, tenantRepo: &localGuardTenantRepo{}}
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := svc.ImportLocalArchives(context.Background(), "kb-1", file)
		if err == nil || !strings.Contains(err.Error(), "导入路径不是目录") {
			t.Fatalf("err = %v, want not-a-directory rejection", err)
		}
	})

	t.Run("directory without zips is rejected", func(t *testing.T) {
		svc := &knowledgeService{fileSvc: &fakeFileService{}, kbService: &localGuardKBService{kb: newKB(types.KnowledgeBaseTypeDocument)}, tenantRepo: &localGuardTenantRepo{}}
		_, err := svc.ImportLocalArchives(context.Background(), "kb-1", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "未找到 .zip") {
			t.Fatalf("err = %v, want zip-less-directory rejection", err)
		}
	})
}

// TestMoveLocalArchive_Directory pins that directory packages move with the
// same nested relative-path preservation as zips.
func TestMoveLocalArchive_Directory(t *testing.T) {
	root := t.TempDir()
	unit := filepath.Join(root, "nested", "unit1")
	if err := os.MkdirAll(filepath.Join(unit, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unit, "a.md"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest, err := moveLocalArchive(root, unit, localImportedDir)
	if err != nil {
		t.Fatalf("moveLocalArchive: %v", err)
	}
	if dest != filepath.Join(root, "imported", "nested", "unit1") {
		t.Fatalf("dest = %s, want imported/nested/unit1", dest)
	}
	if _, err := os.Stat(filepath.Join(dest, "a.md")); err != nil {
		t.Fatalf("unit content missing after move: %v", err)
	}
	if _, err := os.Stat(unit); !os.IsNotExist(err) {
		t.Fatalf("source dir still present: %v", err)
	}
}

// TestImportLocalDir_MovesEmptyUnitsToFailed keeps unusable directory
// packages traceable: a unit without documents fails with a clear error and
// moves to failed/, while the import root as a unit stays in place (it
// cannot move into its own imported/).
func TestImportLocalDir_MovesEmptyUnitsToFailed(t *testing.T) {
	svc := &knowledgeService{}
	kb := &types.KnowledgeBase{ID: "kb-1", TenantID: 7, Type: types.KnowledgeBaseTypeDocument}
	ctx := context.Background()
	opts := archiveDocumentOptions{}

	t.Run("empty unit moves to failed", func(t *testing.T) {
		root := t.TempDir()
		unit := filepath.Join(root, "unit1")
		if err := os.MkdirAll(filepath.Join(unit, "images"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unit, "images", "x.png"), []byte("png"), 0o644); err != nil {
			t.Fatal(err)
		}
		item := svc.importLocalDir(ctx, kb, &fakeFileService{}, 7, &types.Tenant{}, root, unit, opts, false)
		if item.Status != types.LocalArchiveStatusFailed || !strings.Contains(item.Error, "未找到可导入的文档") {
			t.Fatalf("item = %+v, want failed with no-documents error", item)
		}
		if item.Kind != types.LocalArchiveKindDir {
			t.Fatalf("kind = %q, want dir", item.Kind)
		}
		if item.MovedTo != filepath.Join(root, "failed", "unit1") {
			t.Fatalf("moved to %q, want failed/unit1", item.MovedTo)
		}
	})

	t.Run("root unit stays in place on failure", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "notes.bin"), []byte("unsupported type keeps the manifest empty"), 0o644); err != nil {
			t.Fatal(err)
		}
		item := svc.importLocalDir(ctx, kb, &fakeFileService{}, 7, &types.Tenant{}, root, root, opts, true)
		if item.Status != types.LocalArchiveStatusFailed {
			t.Fatalf("status = %q, want failed", item.Status)
		}
		if item.MovedTo != "" {
			t.Fatalf("moved to %q, want empty (mount root stays in place)", item.MovedTo)
		}
		if _, err := os.Stat(filepath.Join(root, "notes.bin")); err != nil {
			t.Fatalf("root content missing: %v", err)
		}
	})
}
