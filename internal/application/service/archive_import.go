package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/url"
	"time"

	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/infrastructure/archive"
	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// maxArchiveDocumentImages caps how many archive images one document may
// inline. It matches the per-document image budget the parser applies to
// extracted images; references beyond the cap keep their original path and
// surface as warnings.
const maxArchiveDocumentImages = 1000

// CreateKnowledgeFromArchive expands an uploaded zip — Markdown documents
// plus their images directory — into one knowledge entry per document.
//
// Per document: images are stored through the catalog-decorated file service
// (content-hash deduplicated), the Markdown is rewritten so every relative
// reference points at the stored URL, the rewritten Markdown becomes the
// knowledge source file, and the images are bound to the knowledge so
// galleries and authorization work exactly as for parsed documents.
//
// Failure semantics: structural problems (bad zip, limits exceeded, no
// Markdown inside) fail the whole upload with a 4xx before anything is
// persisted; a per-document problem (duplicate, storage error) is recorded in
// the report and the remaining documents continue.
func (s *knowledgeService) CreateKnowledgeFromArchive(
	ctx context.Context, kbID string, fileHeader *multipart.FileHeader,
	metadata map[string]any, enableMultimodel *bool, tagIDs []string,
	channel string, processOverrides *types.KnowledgeProcessOverrides,
) (*types.ArchiveImportReport, error) {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	tenantInfo := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)

	kb, err := s.kbService.GetKnowledgeBaseByID(ctx, kbID)
	if err != nil {
		return nil, err
	}
	if kb.Type == types.KnowledgeBaseTypeFAQ {
		return nil, werrors.NewBadRequestError("FAQ 知识库不支持文件上传，请使用 FAQ 导入功能")
	}
	if err := s.checkStorageEngineConfigured(ctx, kb); err != nil {
		return nil, err
	}
	if fileHeader.Size > secutils.GetMaxArchiveSize() {
		return nil, werrors.NewBadRequestError(
			fmt.Sprintf("压缩包大小不能超过 %dMB", secutils.GetMaxArchiveSizeMB()))
	}

	file, err := fileHeader.Open()
	if err != nil {
		return nil, werrors.NewBadRequestError("无法读取上传的压缩包")
	}
	defer file.Close()

	arc, err := archive.Open(file, fileHeader.Size, archive.DefaultLimits())
	if err != nil {
		logger.Errorf(ctx, "Failed to open archive upload: %v", err)
		return nil, err
	}
	manifest, err := archive.BuildManifest(arc)
	if err != nil {
		logger.Errorf(ctx, "Failed to build archive manifest: %v", err)
		return nil, err
	}
	if len(manifest.Documents) == 0 && len(manifest.Attachments) == 0 {
		return nil, werrors.NewBadRequestError("压缩包中未找到可导入的文档，无法导入")
	}

	fileSvc := s.resolveFileService(ctx, kb)
	return s.importArchiveManifest(ctx, kb, fileSvc, tenantID, tenantInfo, manifest, archiveDocumentOptions{
		metadata:         metadata,
		enableMultimodel: enableMultimodel,
		tagIDs:           tagIDs,
		channel:          channel,
		processOverrides: processOverrides,
	}), nil
}

// importArchiveManifest persists every document and attachment of a scanned
// archive and assembles the report. It is the shared tail of the zip upload
// and the zip file-URL import; the archive must already have passed the
// structural checks (non-empty, within limits).
func (s *knowledgeService) importArchiveManifest(
	ctx context.Context, kb *types.KnowledgeBase, fileSvc interfaces.FileService,
	tenantID uint64, tenantInfo *types.Tenant, manifest *archive.Manifest,
	opts archiveDocumentOptions,
) *types.ArchiveImportReport {
	report := &types.ArchiveImportReport{
		Type:    types.TypeArchiveImportReport,
		Items:   make([]types.ArchiveImportItem, 0, len(manifest.Documents)+len(manifest.Attachments)),
		Skipped: make([]types.ArchiveImportItem, 0, len(manifest.Skipped)),
	}
	for _, entry := range manifest.Skipped {
		report.Skipped = append(report.Skipped, types.ArchiveImportItem{
			Title:  entry.Name,
			Status: types.ArchiveItemSkipped,
			Error:  entry.Reason,
		})
	}

	// The import is best-effort per document: one broken file must not cost
	// the user the rest of the package. Per-document steps run on a detached
	// context so a client disconnect after the upload cannot leave a document
	// half-written; errors are captured in the report instead.
	importCtx := context.WithoutCancel(ctx)
	for _, doc := range manifest.Documents {
		item := s.importArchiveDocument(importCtx, kb, fileSvc, tenantID, tenantInfo, doc, opts)
		report.Items = append(report.Items, item)
	}
	for i := range manifest.Attachments {
		item := s.importArchiveAttachment(importCtx, kb, fileSvc, tenantID, tenantInfo, &manifest.Attachments[i], opts)
		report.Items = append(report.Items, item)
	}
	logger.Infof(ctx, "Archive import finished: docs=%d attachments=%d skipped=%d",
		len(manifest.Documents), len(manifest.Attachments), len(report.Skipped))
	return report
}

// archiveDocumentOptions carries the shared upload form options that apply to
// every document in the package.
type archiveDocumentOptions struct {
	metadata         map[string]any
	enableMultimodel *bool
	tagIDs           []string
	channel          string
	processOverrides *types.KnowledgeProcessOverrides
}

// archiveEntry is the import-relevant projection of one archive file.
type archiveEntry struct {
	Name       string
	Title      string
	FolderPath string
	Content    []byte
}

// archivePrepared carries what an entry's preparation step contributes before
// the knowledge record is written: the bytes to store as the source file, the
// images bound for the report, and the resource claims to run after creation.
type archivePrepared struct {
	stored        []byte
	imagesStored  int
	imagesMissing int
	warnings      []string
	bind          func(ctx context.Context, knowledgeID string)
}

// importArchiveDocument creates one knowledge entry from one archive Markdown
// document, storing and rewriting its images first. Any failure is reported
// in the returned item instead of aborting the batch.
func (s *knowledgeService) importArchiveDocument(
	ctx context.Context, kb *types.KnowledgeBase, fileSvc interfaces.FileService,
	tenantID uint64, tenantInfo *types.Tenant, doc *archive.Document, opts archiveDocumentOptions,
) types.ArchiveImportItem {
	prepare := func(ctx context.Context, fileSvc interfaces.FileService) (*archivePrepared, error) {
		rewritten, imagesStored, warnings, err := s.storeArchiveImages(ctx, fileSvc, tenantID, doc)
		if err != nil {
			return nil, err
		}
		return &archivePrepared{
			stored:        []byte(rewritten),
			imagesStored:  imagesStored,
			imagesMissing: len(doc.Missing),
			warnings:      warnings,
			bind: func(ctx context.Context, knowledgeID string) {
				s.bindContentResources(ctx, tenantID, knowledgeID, rewritten)
			},
		}, nil
	}
	return s.importArchiveEntry(ctx, kb, fileSvc, tenantID, tenantInfo, archiveEntry{
		Name: doc.Name, Title: doc.Title, FolderPath: doc.FolderPath, Content: doc.Content,
	}, prepare, opts)
}

// importArchiveAttachment imports a non-Markdown document (pdf, docx, ...)
// from an archive exactly like a single-file upload of the same type: the
// bytes become the source file and the regular parse task takes over.
func (s *knowledgeService) importArchiveAttachment(
	ctx context.Context, kb *types.KnowledgeBase, fileSvc interfaces.FileService,
	tenantID uint64, tenantInfo *types.Tenant, att *archive.Attachment, opts archiveDocumentOptions,
) types.ArchiveImportItem {
	return s.importArchiveEntry(ctx, kb, fileSvc, tenantID, tenantInfo, archiveEntry{
		Name: att.Name, Title: att.Title, FolderPath: att.FolderPath, Content: att.Data,
	}, nil, opts)
}

// importArchiveEntry is the shared knowledge-creation core for both archive
// kinds: duplicate detection against the original bytes, quota and process
// config, source-file persistence, tags, resource claims and the parse-task
// enqueue. The optional prepare step lets Markdown documents store and inline
// their images first.
func (s *knowledgeService) importArchiveEntry(
	ctx context.Context, kb *types.KnowledgeBase, fileSvc interfaces.FileService,
	tenantID uint64, tenantInfo *types.Tenant, entry archiveEntry,
	prepare func(ctx context.Context, fileSvc interfaces.FileService) (*archivePrepared, error),
	opts archiveDocumentOptions,
) types.ArchiveImportItem {
	item := types.ArchiveImportItem{
		Title:      entry.Title,
		FolderPath: entry.FolderPath,
		Status:     types.ArchiveItemFailed,
	}
	fail := func(err error) types.ArchiveImportItem {
		logger.Errorf(ctx, "Archive document %s failed: %v", entry.Name, err)
		item.Error = err.Error()
		return item
	}

	var prepared *archivePrepared
	if prepare != nil {
		var err error
		prepared, err = prepare(ctx, fileSvc)
		if err != nil {
			return fail(err)
		}
		item.Warnings = append(item.Warnings, prepared.warnings...)
		item.ImagesStored = prepared.imagesStored
		item.ImagesMissing = prepared.imagesMissing
	}
	storedContent := entry.Content
	if prepared != nil {
		storedContent = prepared.stored
	}

	// Duplicate detection hashes the ORIGINAL document bytes, matching the
	// plain-upload semantics: re-uploading the same package is recognized
	// even though the rewritten content differs (stored image URLs are
	// generated per import).
	contentHash := sha256Hex(entry.Content)
	folderPath := types.NormalizeKnowledgeFolderPath(entry.FolderPath)
	if folderPath != "" {
		if _, valid := secutils.ValidateInput(folderPath); !valid {
			item.Warnings = append(item.Warnings, "文件夹路径包含非法字符，已忽略目录信息")
			folderPath = ""
		}
	}
	safeTitle, titleValid := secutils.ValidateInput(entry.Title)
	if !titleValid {
		return fail(werrors.NewValidationError(fmt.Sprintf("文件名包含非法字符：%s", entry.Title)))
	}

	exists, existing, err := s.checkKnowledgeForCreation(ctx, tenantID, kb.ID, nil, &types.KnowledgeCheckParams{
		Type:     "file",
		FileName: safeTitle,
		FileType: getFileType(safeTitle),
		FileSize: int64(len(entry.Content)),
		FileHash: contentHash,
	})
	if err != nil {
		return fail(err)
	}
	if exists {
		if err := s.repo.UpdateKnowledgeColumn(ctx, existing.ID, "created_at", time.Now()); err != nil {
			return fail(err)
		}
		item.Status = types.ArchiveItemDuplicate
		item.KnowledgeID = existing.ID
		item.Error = fmt.Sprintf("与已有知识「%s」内容重复，已跳过", existing.Title)
		return item
	}
	if tenantInfo.StorageQuota > 0 && tenantInfo.StorageUsed >= tenantInfo.StorageQuota {
		return fail(types.NewStorageQuotaExceededError())
	}

	eff, err := resolveFileImportProcessConfig(ctx, kb, getFileType(safeTitle), opts.processOverrides, opts.enableMultimodel)
	if err != nil {
		return fail(err)
	}

	var metadataJSON types.JSON
	if opts.metadata != nil {
		metadataBytes, err := json.Marshal(opts.metadata)
		if err != nil {
			return fail(err)
		}
		metadataJSON = types.JSON(metadataBytes)
	}

	knowledge := &types.Knowledge{
		ID:               uuid.New().String(),
		TenantID:         tenantID,
		KnowledgeBaseID:  kb.ID,
		Type:             "file",
		Channel:          defaultChannel(opts.channel),
		Title:            safeTitle,
		FileName:         safeTitle,
		FolderPath:       folderPath,
		FileType:         getFileType(safeTitle),
		FileSize:         int64(len(storedContent)),
		FileHash:         contentHash,
		ParseStatus:      "pending",
		EnableStatus:     "disabled",
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
		EmbeddingModelID: kb.EmbeddingModelID,
		Metadata:         metadataJSON,
	}
	if opts.processOverrides != nil {
		if err := knowledge.SetProcessOverrides(opts.processOverrides); err != nil {
			return fail(err)
		}
	}

	filePath, err := fileSvc.SaveBytes(ctx, storedContent, tenantID, safeTitle, false)
	if err != nil {
		return fail(err)
	}
	knowledge.FilePath = filePath

	if err := s.repo.CreateKnowledge(ctx, knowledge); err != nil {
		if deleteErr := fileSvc.DeleteFile(context.WithoutCancel(ctx), filePath); deleteErr != nil {
			logger.Errorf(ctx, "Failed to delete saved archive file after knowledge creation failed, path: %s: %v",
				filePath, deleteErr)
		}
		return fail(err)
	}
	if err := s.setAndAttachKnowledgeTags(ctx, tenantID, kb.ID, knowledge, opts.tagIDs); err != nil {
		return fail(err)
	}

	// Claim the stored images so galleries and cross-workspace authorization
	// treat them exactly like parse-extracted images.
	if prepared != nil && prepared.bind != nil {
		prepared.bind(ctx, knowledge.ID)
	}

	item.KnowledgeID = knowledge.ID
	item.Status = types.ArchiveItemCreated

	if err := s.enqueueArchiveDocumentTask(ctx, kb, knowledge, filePath, safeTitle, eff); err != nil {
		item.Error = err.Error()
		item.Warnings = append(item.Warnings, "文档已保存，但解析任务入队失败，请稍后手动重试解析")
		return item
	}
	return item
}

// storeArchiveImages persists every resolved image of one document and
// returns the Markdown with all references rewritten to the stored URLs, the
// number of images stored, and the warnings for references that matched
// nothing (or exceeded the per-document cap). Unstored references keep their
// original path in the rewritten Markdown.
func (s *knowledgeService) storeArchiveImages(
	ctx context.Context, fileSvc interfaces.FileService, tenantID uint64, doc *archive.Document,
) (string, int, []string, error) {
	refURLs := make(map[string]string, len(doc.Images))
	warnings := make([]string, 0, len(doc.Missing))
	stored := 0
	for i := range doc.Images {
		img := &doc.Images[i]
		if stored >= maxArchiveDocumentImages {
			warnings = append(warnings, fmt.Sprintf("图片数量超出每文档 %d 张上限，保留原路径：%s",
				maxArchiveDocumentImages, img.Name))
			continue
		}
		ref, err := fileSvc.SaveBytes(ctx, img.Data, tenantID, img.Name, false)
		if err != nil {
			return "", 0, nil, fmt.Errorf("存储图片 %s 失败: %w", img.Name, err)
		}
		stored++
		for _, rawRef := range img.Refs {
			refURLs[rawRef] = ref
		}
	}
	for _, missing := range doc.Missing {
		warnings = append(warnings, fmt.Sprintf("压缩包中未找到图片 %s，保留原路径", archiveDisplayRef(missing)))
	}

	rewritten, _ := docparser.RewriteRelativeImageRefs(string(doc.Content), func(ref string) (string, bool) {
		url, ok := refURLs[ref]
		return url, ok && url != ""
	})
	return rewritten, stored, warnings, nil
}

// archiveDisplayRef collapses percent escapes in a reference before it is
// shown in a warning, so users see the filename they recognize.
func archiveDisplayRef(ref string) string {
	decoded, err := url.PathUnescape(ref)
	if err != nil {
		return ref
	}
	return decoded
}

// enqueueArchiveDocumentTask mirrors the file-upload enqueue: same payload,
// same retry policy, same failure marking.
func (s *knowledgeService) enqueueArchiveDocumentTask(
	ctx context.Context, kb *types.KnowledgeBase, knowledge *types.Knowledge,
	filePath, fileName string, eff types.EffectiveProcessConfig,
) error {
	tenantID := knowledge.TenantID
	questionCount := eff.QuestionGenerationConfig.QuestionCount
	if questionCount <= 0 {
		questionCount = 3
	}
	lang := types.LanguageFromContextOrDefault(ctx)
	taskPayload := types.DocumentProcessPayload{
		TenantID:                 tenantID,
		KnowledgeID:              knowledge.ID,
		KnowledgeBaseID:          knowledge.KnowledgeBaseID,
		FilePath:                 filePath,
		FileName:                 fileName,
		FileType:                 knowledge.FileType,
		EnableMultimodel:         eff.EnableMultimodel,
		EnableQuestionGeneration: eff.QuestionGenerationConfig.Enabled,
		QuestionCount:            questionCount,
		Language:                 lang,
	}
	langfuse.InjectTracing(ctx, &taskPayload)
	payloadBytes, err := json.Marshal(taskPayload)
	if err != nil {
		s.markKnowledgeEnqueueFailed(ctx, knowledge)
		recordKBActivity(ctx, s.audit, tenantID, knowledge.KnowledgeBaseID, types.AuditActionKnowledgeCreated,
			"knowledge", knowledge.ID, types.AuditOutcomeFailed, map[string]any{
				"title": knowledge.Title, "source_type": "file", "file_type": knowledge.FileType,
				"processing_status": "failed", "failure_stage": "enqueue",
			})
		return fmt.Errorf("序列化解析任务失败: %w", err)
	}
	task := asynq.NewTask(
		types.TypeDocumentProcess,
		payloadBytes,
		documentProcessTaskOptions(s.config, asynq.MaxRetry(3))...,
	)
	info, err := s.task.Enqueue(task)
	if err != nil {
		s.markKnowledgeEnqueueFailed(ctx, knowledge)
		recordKBActivity(ctx, s.audit, tenantID, knowledge.KnowledgeBaseID, types.AuditActionKnowledgeCreated,
			"knowledge", knowledge.ID, types.AuditOutcomeFailed, map[string]any{
				"title": knowledge.Title, "source_type": "file", "file_type": knowledge.FileType,
				"processing_status": "failed", "failure_stage": "enqueue",
			})
		return fmt.Errorf("解析任务入队失败: %w", err)
	}
	recordKBActivity(ctx, s.audit, tenantID, knowledge.KnowledgeBaseID, types.AuditActionKnowledgeCreated,
		"knowledge", knowledge.ID, types.AuditOutcomeAccepted, map[string]any{
			"title": knowledge.Title, "source_type": "file", "file_type": knowledge.FileType,
			"processing_status": "pending", "task_id": info.ID, "trigger": kbActivityTrigger(ctx),
		})
	enqueueDataTableSummaryIfNeeded(ctx, s.task, tenantID, knowledge.ID, fileName,
		knowledge.FileType, kb.SummaryModelID, kb.EmbeddingModelID)
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
