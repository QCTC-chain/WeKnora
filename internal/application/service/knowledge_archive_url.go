package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	werrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/infrastructure/archive"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// CreateKnowledgeFromArchiveURL imports a .zip file URL the same way the
// direct upload does: the archive is downloaded synchronously, expanded, and
// one knowledge entry is created per Markdown document (plus direct imports
// for other supported documents inside). The zip itself never becomes a
// knowledge record; the returned report carries the per-document outcomes.
func (s *knowledgeService) CreateKnowledgeFromArchiveURL(
	ctx context.Context, kbID, fileURL string, fileName string, enableMultimodel *bool,
	title string, tagIDs []string, channel string,
	processOverrides *types.KnowledgeProcessOverrides,
) (*types.ArchiveImportReport, error) {
	if !isValidURL(fileURL) || !secutils.IsValidURL(fileURL) {
		return nil, ErrInvalidURL
	}
	// The handler validates the user-supplied URL already; the service
	// re-checks so no caller can bypass the SSRF policy.
	if err := secutils.ValidateURLForSSRF(fileURL); err != nil {
		logger.Errorf(ctx, "Archive URL rejected for SSRF protection: %s, err: %v", fileURL, err)
		return nil, ErrInvalidURL
	}

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
	data, err := downloadArchiveFromURL(ctx, fileURL)
	if err != nil {
		return nil, err
	}
	return s.createKnowledgeFromArchiveBytes(ctx, kbID, data, enableMultimodel, title, tagIDs, channel, processOverrides)
}

// createKnowledgeFromArchiveBytes is the transport-independent tail of the
// zip URL import: it expands an in-memory archive and imports each document.
func (s *knowledgeService) createKnowledgeFromArchiveBytes(
	ctx context.Context, kbID string, data []byte, enableMultimodel *bool,
	title string, tagIDs []string, channel string,
	processOverrides *types.KnowledgeProcessOverrides,
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

	arc, err := archive.Open(bytes.NewReader(data), int64(len(data)), archive.DefaultLimits())
	if err != nil {
		logger.Errorf(ctx, "Failed to open archive from URL: %v", err)
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
	// title/tags/channel and process options apply to every expanded
	// document; the per-entry title comes from each file name.
	report := s.importArchiveManifest(ctx, kb, fileSvc, tenantID, tenantInfo, manifest, archiveDocumentOptions{
		enableMultimodel: enableMultimodel,
		tagIDs:           tagIDs,
		channel:          channel,
		processOverrides: processOverrides,
	})
	logger.Infof(ctx, "Archive URL import finished: title=%s docs=%d",
		secutils.SanitizeForLog(title), len(report.Items))
	return report, nil
}

// downloadArchiveFromURL fetches a zip file over HTTP with the SSRF-safe
// client, capped at the configured archive size limit.
func downloadArchiveFromURL(ctx context.Context, fileURL string) ([]byte, error) {
	limit := secutils.GetMaxArchiveSize()
	httpClient := secutils.NewSSRFSafeHTTPClient(secutils.SSRFSafeHTTPClientConfig{
		Timeout:      5 * time.Minute,
		MaxRedirects: 10,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request for archive URL: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download archive from URL: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, werrors.NewBadRequestError(fmt.Sprintf("下载压缩包失败：远端返回状态码 %d", resp.StatusCode))
	}
	if resp.ContentLength > limit {
		return nil, werrors.NewBadRequestError(
			fmt.Sprintf("压缩包大小不能超过 %dMB", secutils.GetMaxArchiveSizeMB()))
	}
	if ct := strings.ToLower(resp.Header.Get("Content-Type")); strings.Contains(ct, "text/html") {
		return nil, werrors.NewBadRequestError("URL 指向的不是可下载的压缩包文件")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read archive from URL: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, werrors.NewBadRequestError(
			fmt.Sprintf("压缩包大小不能超过 %dMB", secutils.GetMaxArchiveSizeMB()))
	}
	return data, nil
}

// isZipURLRequest reports whether a file-URL import should be expanded as a
// zip archive instead of downloaded as a single document.
func isZipURLRequest(rawURL, fileName, fileType string) bool {
	if secutils.IsZipArchiveExtension(fileType) {
		return true
	}
	if fileName != "" && secutils.IsZipArchiveExtension(getFileType(fileName)) {
		return true
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return secutils.IsZipArchiveExtension(strings.TrimPrefix(path.Ext(u.Path), "."))
}
