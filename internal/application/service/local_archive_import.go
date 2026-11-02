package service

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/infrastructure/archive"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// managedLocalArchiveDirs are sub-directories of the import root that the
// local bulk import owns: processed packages are moved into them, so
// enumeration never descends into them (that is what makes re-runs resume
// instead of re-importing).
var managedLocalArchiveDirs = map[string]struct{}{
	"imported": {},
	"failed":   {},
}

const (
	localImportedDir = "imported"
	localFailedDir   = "failed"
)

// localPackage is one import unit found under the import root: either a
// .zip archive or a directory whose contents look like an extracted zip
// (documents plus the images they reference).
type localPackage struct {
	Path string
	Kind string // types.LocalArchiveKindZip or types.LocalArchiveKindDir
}

// ImportLocalArchives imports every package under dir into the knowledge
// base kbID — the container-local bulk variant of the zip upload, run by the
// `import-archives` server subcommand and never exposed as an HTTP route.
//
// A package is either a .zip file or a directory: mounted folders whose
// sub-directories and files have the same shape as an extracted zip are
// imported directly, with no zip step (archive.OpenDirectory adapts them
// to the same read view the zip reader offers). When the mount itself is
// the content — no zips anywhere and at most one document-bearing
// directory below the root — the root as a whole is imported as one
// package and, being the mount point, kept in place instead of moved.
//
// Because there is no upload body, the HTTP upload caps (nginx body size,
// ARCHIVE_MAX_SIZE_MB) simply do not apply; packages are instead bounded by
// the relaxed, finite LOCAL_IMPORT_* limits (archive.LocalImportLimits).
// Everything downstream of the unpack — manifest scan, image storage with
// reference rewriting, duplicate detection, knowledge creation, parse-task
// enqueue — is the same pipeline the upload endpoint uses
// (importArchiveManifest), so a locally imported package is indistinguishable
// from an uploaded one.
//
// Per package: any per-document failure is recorded in the item and the rest
// of the package continues; after processing the package is moved to
// imported/ (all documents created or duplicate) or failed/ (any failure),
// preserving its path relative to the import root, so re-running the command
// resumes with the untouched packages only. The parse pipeline's own
// per-document inline budgets are NOT relaxed; documents exceeding them are
// warned about in the report and the log instead.
func (s *knowledgeService) ImportLocalArchives(
	ctx context.Context, kbID, dir string,
) (*types.LocalArchiveImportReport, error) {
	// The subcommand runs outside any request context: resolve the tenant
	// from the KB itself and build the context the import tail expects.
	kb, err := s.kbService.GetKnowledgeBaseByIDOnly(ctx, kbID)
	if err != nil {
		return nil, err
	}
	tenantInfo, err := s.tenantRepo.GetTenantByID(ctx, kb.TenantID)
	if err != nil {
		return nil, fmt.Errorf("加载空间 %d 失败: %w", kb.TenantID, err)
	}
	ctx = context.WithValue(ctx, types.TenantIDContextKey, kb.TenantID)
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenantInfo)

	if kb.Type == types.KnowledgeBaseTypeFAQ {
		return nil, werrors.NewBadRequestError("FAQ 知识库不支持文件上传，请使用 FAQ 导入功能")
	}
	if err := s.checkStorageEngineConfigured(ctx, kb); err != nil {
		return nil, err
	}

	info, err := os.Stat(dir)
	if err != nil {
		return nil, werrors.NewBadRequestError(fmt.Sprintf("导入目录不可访问: %v", err))
	}
	if !info.IsDir() {
		return nil, werrors.NewBadRequestError(fmt.Sprintf("导入路径不是目录: %s", dir))
	}
	// Work with one clean absolute root so enumeration paths and the
	// imported/failed destinations are always Rel-able against it.
	rootDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, werrors.NewBadRequestError(fmt.Sprintf("导入目录不可解析: %v", err))
	}
	rootDir = filepath.Clean(rootDir)

	packages, rootHasDocs, rootDirectDocs, err := enumerateLocalPackages(rootDir)
	if err != nil {
		return nil, werrors.NewBadRequestError(fmt.Sprintf("扫描导入目录失败: %v", err))
	}
	// Nothing else to import and the mount itself holds documents: treat the
	// import root as one big extracted package — the "mount the folder
	// instead of zipping it" case. The root cannot move into its own
	// imported/, so it stays in place after processing.
	rootAsUnit := len(packages) == 0 && rootHasDocs
	if rootAsUnit {
		packages = []localPackage{{Path: rootDir, Kind: types.LocalArchiveKindDir}}
	}
	if len(packages) == 0 {
		return nil, werrors.NewBadRequestError(fmt.Sprintf(
			"目录 %s 下未找到 .zip 压缩包或包含文档的目录", dir))
	}
	limits := archive.LocalImportLimits()
	logger.Infof(ctx, "Local archive import starting: dir=%s kb=%s packages=%d limits(documents=%d entries=%d expandedMB=%d)",
		dir, kb.ID, len(packages), limits.MaxDocuments, limits.MaxEntries, limits.MaxExpandedBytes>>20)

	fileSvc := s.resolveFileService(ctx, kb)
	opts := archiveDocumentOptions{
		maxImagesPerDocument: secutils.GetLocalImportMaxImagesPerDocument(),
		parserBudgetWarnings: true,
	}

	report := &types.LocalArchiveImportReport{
		Type:            types.TypeLocalArchiveImportReport,
		Dir:             rootDir,
		KnowledgeBaseID: kb.ID,
		Items:           make([]types.LocalArchiveImportItem, 0, len(packages)),
	}
	if rootAsUnit {
		report.Notes = append(report.Notes,
			"导入根目录本身按一个已解压的包整体导入，处理后保留在原位（无法移动挂载点自身）")
	} else if rootDirectDocs {
		report.Notes = append(report.Notes,
			"导入根目录下存在直属文档，但本次按子包导入模式未包含根目录本身；如需整体导入，请移走目录下的 .zip 与包目录后重跑")
	}
	for _, pkg := range packages {
		// Cooperate with SIGINT/SIGTERM between packages; the package in
		// flight is not aborted (its tail runs detached), untouched
		// packages stay in place for the next run.
		if ctx.Err() != nil {
			logger.Warnf(ctx, "Local archive import interrupted, remaining packages left in place: %v", ctx.Err())
			break
		}
		var item types.LocalArchiveImportItem
		if pkg.Kind == types.LocalArchiveKindDir {
			item = s.importLocalDir(ctx, kb, fileSvc, kb.TenantID, tenantInfo, rootDir, pkg.Path, opts, rootAsUnit)
		} else {
			item = s.importLocalZip(ctx, kb, fileSvc, kb.TenantID, tenantInfo, rootDir, pkg.Path, opts)
		}
		report.Items = append(report.Items, item)
		if item.Status == types.LocalArchiveStatusImported {
			if item.Kind == types.LocalArchiveKindDir {
				report.ImportedDirs++
			} else {
				report.ImportedZips++
			}
		} else {
			if item.Kind == types.LocalArchiveKindDir {
				report.FailedDirs++
			} else {
				report.FailedZips++
			}
		}
	}
	logger.Infof(ctx, "Local archive import finished: dir=%s packages=%d imported(zips=%d dirs=%d) failed(zips=%d dirs=%d)",
		dir, len(report.Items), report.ImportedZips, report.ImportedDirs, report.FailedZips, report.FailedDirs)
	return report, nil
}

// importLocalZip runs one zip package through the archive pipeline and moves
// it to imported/ or failed/ afterwards.
func (s *knowledgeService) importLocalZip(
	ctx context.Context, kb *types.KnowledgeBase, fileSvc interfaces.FileService,
	tenantID uint64, tenantInfo *types.Tenant, rootDir, zipPath string, opts archiveDocumentOptions,
) types.LocalArchiveImportItem {
	item := types.LocalArchiveImportItem{
		Path:   zipPath,
		Kind:   types.LocalArchiveKindZip,
		Status: types.LocalArchiveStatusFailed,
	}
	fail := func(err error) types.LocalArchiveImportItem {
		logger.Warnf(ctx, "Local zip %s failed: %v", zipPath, err)
		item.Error = err.Error()
		return finishLocalPackage(ctx, rootDir, zipPath, item, false)
	}

	file, err := os.Open(zipPath)
	if err != nil {
		return fail(fmt.Errorf("打开文件失败: %w", err))
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("读取文件信息失败: %w", err))
	}

	arc, err := archive.Open(file, info.Size(), archive.LocalImportLimits())
	if err != nil {
		return fail(err)
	}
	item, err = s.importLocalManifest(ctx, kb, fileSvc, tenantID, tenantInfo, arc, item, opts)
	if err != nil {
		return fail(err)
	}
	if item.Failed > 0 {
		item.Status = types.LocalArchiveStatusFailed
		item.Error = fmt.Sprintf("%d 个文档导入失败，详见报告", item.Failed)
		logger.Warnf(ctx, "Local zip %s failed: %s", zipPath, item.Error)
		return finishLocalPackage(ctx, rootDir, zipPath, item, false)
	}
	item.Status = types.LocalArchiveStatusImported
	return finishLocalPackage(ctx, rootDir, zipPath, item, false)
}

// importLocalDir runs one directory package — a folder whose contents look
// like an extracted zip — through the same archive pipeline, and moves it to
// imported/ or failed/ afterwards. The import root itself (rootUnit) cannot
// move into its own imported/, so it is kept in place instead.
func (s *knowledgeService) importLocalDir(
	ctx context.Context, kb *types.KnowledgeBase, fileSvc interfaces.FileService,
	tenantID uint64, tenantInfo *types.Tenant, rootDir, dirPath string,
	opts archiveDocumentOptions, rootUnit bool,
) types.LocalArchiveImportItem {
	item := types.LocalArchiveImportItem{
		Path:   dirPath,
		Kind:   types.LocalArchiveKindDir,
		Status: types.LocalArchiveStatusFailed,
	}
	fail := func(err error) types.LocalArchiveImportItem {
		logger.Warnf(ctx, "Local directory package %s failed: %v", dirPath, err)
		item.Error = err.Error()
		return finishLocalPackage(ctx, rootDir, dirPath, item, rootUnit)
	}

	arc, err := archive.OpenDirectory(dirPath, archive.LocalImportLimits())
	if err != nil {
		return fail(err)
	}
	item, err = s.importLocalManifest(ctx, kb, fileSvc, tenantID, tenantInfo, arc, item, opts)
	if err != nil {
		return fail(err)
	}
	if item.Failed > 0 {
		item.Status = types.LocalArchiveStatusFailed
		item.Error = fmt.Sprintf("%d 个文档导入失败，详见报告", item.Failed)
		logger.Warnf(ctx, "Local directory package %s failed: %s", dirPath, item.Error)
		return finishLocalPackage(ctx, rootDir, dirPath, item, rootUnit)
	}
	item.Status = types.LocalArchiveStatusImported
	return finishLocalPackage(ctx, rootDir, dirPath, item, rootUnit)
}

// importLocalManifest is the shared middle of both package kinds: build the
// manifest from the opened source and run the standard import tail
// (persist documents with duplicate detection, quota check and parse
// enqueue), then copy the per-document outcomes into the item.
func (s *knowledgeService) importLocalManifest(
	ctx context.Context, kb *types.KnowledgeBase, fileSvc interfaces.FileService,
	tenantID uint64, tenantInfo *types.Tenant, arc archive.ArchiveSource,
	item types.LocalArchiveImportItem, opts archiveDocumentOptions,
) (types.LocalArchiveImportItem, error) {
	manifest, err := archive.BuildManifest(arc)
	if err != nil {
		return item, err
	}
	if len(manifest.Documents) == 0 && len(manifest.Attachments) == 0 {
		return item, werrors.NewBadRequestError("包中未找到可导入的文档")
	}

	pkgReport := s.importArchiveManifest(ctx, kb, fileSvc, tenantID, tenantInfo, manifest, opts)
	item.Report = pkgReport
	item.Created = pkgReport.CreatedCount()
	item.Duplicate = pkgReport.DuplicateCount()
	item.Failed = pkgReport.FailedCount()
	item.SkippedFiles = len(pkgReport.Skipped)
	return item, nil
}

// finishLocalPackage logs the outcome and moves the processed package out of
// the scan root — imported/ on success, failed/ on any failure — preserving
// its path relative to the import root. The move is best-effort: a failure
// keeps the package in place and is logged, never fatal. skipMove covers the
// import root as a package: it cannot move into its own imported/.
func finishLocalPackage(ctx context.Context, rootDir, pkgPath string, item types.LocalArchiveImportItem, skipMove bool) types.LocalArchiveImportItem {
	subdir := localFailedDir
	if item.Status == types.LocalArchiveStatusImported {
		subdir = localImportedDir
	}
	if !skipMove {
		moved, err := moveLocalArchive(rootDir, pkgPath, subdir)
		if err != nil {
			logger.Errorf(ctx, "Failed to move processed package %s to %s: %v", pkgPath, subdir, err)
			return item
		}
		item.MovedTo = moved
	}
	if item.Status == types.LocalArchiveStatusImported {
		logger.Infof(ctx, "Local %s imported: %s created=%d duplicate=%d skipped_files=%d moved_to=%s",
			item.Kind, pkgPath, item.Created, item.Duplicate, item.SkippedFiles, item.MovedTo)
	} else {
		logger.Warnf(ctx, "Local %s failed: %s error=%s moved_to=%s",
			item.Kind, pkgPath, item.Error, item.MovedTo)
	}
	return item
}

// enumerateLocalPackages lists the packages to import under rootDir, sorted
// by path for deterministic reports. A package is a .zip file or a
// directory whose subtree contains at least one supported document — the
// topmost such directory wins, mirroring "this folder is one extracted zip":
// its own subtree is never searched for further packages, and a zip inside
// it is ignored exactly like the unsupported entry it would be during a zip
// import. The managed imported/ and failed/ sub-directories and hidden
// directories are not descended into; symlinks and other non-regular files
// are ignored, mirroring the archive readers' own rules.
//
// Mount-as-package: when no zip exists anywhere and at most one
// document-bearing directory sits below the root, nothing is enumerated and
// rootHasDocs is returned instead — the caller then imports the import root
// itself as one package, so a folder mounted instead of a zip "just works".
// With two or more independent document-bearing directories (a batch of
// extracted zips), they are enumerated as separate packages as usual.
//
// Returned flags: rootHasDocs reports whether the root subtree holds any
// supported document at all, and rootDirectDocs whether documents sit
// directly in the root (which the sub-package mode cannot reach).
func enumerateLocalPackages(rootDir string) (packages []localPackage, rootHasDocs, rootDirectDocs bool, err error) {
	// dirDocs marks directories that directly contain a supported document;
	// dirParent records the tree so subtrees can be summarized.
	dirDocs := make(map[string]bool)
	dirParent := make(map[string]string)
	var zips []string

	walkErr := filepath.WalkDir(rootDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if p == rootDir {
				return nil
			}
			if _, managed := managedLocalArchiveDirs[d.Name()]; managed || strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			dirParent[p] = filepath.Dir(p)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".zip") {
			zips = append(zips, p)
			return nil
		}
		if isLocalSupportedDocumentName(d.Name()) {
			parent := filepath.Dir(p)
			if parent == rootDir {
				rootDirectDocs = true
			}
			dirDocs[parent] = true
		}
		return nil
	})
	if walkErr != nil {
		return nil, false, false, walkErr
	}

	// subtreeHasDoc: memoized post-order over the recorded directory tree.
	children := make(map[string][]string, len(dirParent))
	for p, parent := range dirParent {
		children[parent] = append(children[parent], p)
	}
	subtree := make(map[string]bool, len(dirParent))
	var compute func(string) bool
	compute = func(p string) bool {
		if v, ok := subtree[p]; ok {
			return v
		}
		has := dirDocs[p]
		for _, child := range children[p] {
			if compute(child) {
				has = true
			}
		}
		subtree[p] = has
		return has
	}
	rootHasDocs = compute(rootDir)

	// Units: the topmost directories (below the root) whose subtree holds a
	// document. Descending stops at the first hit.
	var unitDirs []string
	var pick func(string)
	pick = func(p string) {
		for _, child := range children[p] {
			if subtree[child] {
				unitDirs = append(unitDirs, child)
			} else {
				pick(child)
			}
		}
	}
	pick(rootDir)

	// Mount-as-package: without any zip the folder as a whole is the
	// natural unit — documents may sit directly in the root or inside one
	// extracted subtree (whose images may live at the root). Only a real
	// batch — two or more document-bearing directories — is split into
	// per-directory packages, and any zip present switches the root into
	// plain container mode.
	if len(zips) == 0 && len(unitDirs) <= 1 {
		return nil, rootHasDocs, rootDirectDocs, nil
	}

	units := make(map[string]struct{}, len(unitDirs))
	for _, u := range unitDirs {
		units[u] = struct{}{}
		packages = append(packages, localPackage{Path: u, Kind: types.LocalArchiveKindDir})
	}
	// Zips outside any unit subtree stay importable; a zip inside a unit is
	// part of that unit's content (an unsupported entry, like in a zip).
	for _, z := range zips {
		excluded := false
		for p := filepath.Dir(z); p != rootDir && len(p) > len(rootDir); p = filepath.Dir(p) {
			if _, hit := units[p]; hit {
				excluded = true
				break
			}
		}
		if !excluded {
			packages = append(packages, localPackage{Path: z, Kind: types.LocalArchiveKindZip})
		}
	}
	sort.Slice(packages, func(i, j int) bool { return packages[i].Path < packages[j].Path })
	return packages, rootHasDocs, rootDirectDocs, nil
}

// isLocalSupportedDocumentName reports whether a file counts as a document
// when deciding whether a directory is an importable package. It mirrors
// BuildManifest's own classification via archive.IsDocumentCandidateName,
// so discovery and the manifest scanner never disagree.
func isLocalSupportedDocumentName(name string) bool {
	return archive.IsDocumentCandidateName(name)
}

// moveLocalArchive moves pkgPath into rootDir/subdir, preserving its path
// relative to the import root so nested packages stay identifiable, and
// returns the destination path. Both dirs live under the same root, so a
// plain rename suffices — for zips and directories alike.
func moveLocalArchive(rootDir, pkgPath, subdir string) (string, error) {
	rel, err := filepath.Rel(rootDir, pkgPath)
	if err != nil || rel == "." || rel == "" {
		rel = filepath.Base(pkgPath)
	}
	dest := filepath.Join(rootDir, subdir, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(pkgPath, dest); err != nil {
		return "", err
	}
	return dest, nil
}
