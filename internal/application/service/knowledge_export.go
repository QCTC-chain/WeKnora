package service

import (
	"context"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/infrastructure/docparser"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// Export round-trip bounds. A zip-imported markdown re-downloads as a plain
// file plus an images/ directory, so the whole document must be buffered
// before its resource:// references are rewritten; these caps keep one
// oversized entry from exhausting memory.
const (
	maxKnowledgeExportMarkdownBytes = 64 << 20
	maxKnowledgeExportImageBytes    = 100 << 20
	maxKnowledgeExportImages        = 1000
)

// GetKnowledgeFileForExport retrieves a knowledge original for download and,
// when the document embeds content-bound resource:// images (archive imports,
// manual documents with stored images), resolves every referenced payload.
// The content itself is returned unrewritten — the download archive writer
// decides the final relative layout, so per-document and cross-document name
// collisions can be settled with the zip's global namespace knowledge.
func (s *knowledgeService) GetKnowledgeFileForExport(
	ctx context.Context, id string,
) (*types.KnowledgeExportFile, error) {
	tenantID := ctx.Value(types.TenantIDContextKey).(uint64)
	knowledge, err := s.repo.GetKnowledgeByID(ctx, tenantID, id)
	if err != nil {
		return nil, err
	}

	var (
		content string
		name    string
		isMD    bool
	)
	if knowledge.IsManual() {
		meta, err := knowledge.ManualMetadata()
		if err != nil {
			return nil, err
		}
		if meta != nil {
			content = meta.Content
		}
		name = sanitizeManualDownloadFilename(knowledge.Title)
		isMD = true
	} else {
		kb, _ := s.kbService.GetKnowledgeBaseByID(ctx, knowledge.KnowledgeBaseID)
		fileSvc := s.resolveFileServiceForPath(ctx, kb, knowledge.FilePath)
		file, err := fileSvc.GetFile(ctx, knowledge.FilePath)
		if err != nil {
			return nil, err
		}
		data, overflow, readErr := readBounded(file, maxKnowledgeExportMarkdownBytes)
		_ = file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if overflow {
			// Too large to buffer for a rewrite: stream the original
			// unchanged rather than risk a truncated download.
			file, err := fileSvc.GetFile(ctx, knowledge.FilePath)
			if err != nil {
				return nil, err
			}
			return &types.KnowledgeExportFile{Content: file, Name: knowledge.FileName}, nil
		}
		content = string(data)
		name = knowledge.FileName
		isMD = isMarkdownExportName(name)
	}

	export := &types.KnowledgeExportFile{
		Content: io.NopCloser(strings.NewReader(content)),
		Name:    name,
	}
	if !isMD {
		return export, nil
	}

	refs := docparser.CollectProviderImageRefs(content)
	if len(refs) == 0 {
		return export, nil
	}

	kb, _ := s.kbService.GetKnowledgeBaseByID(ctx, knowledge.KnowledgeBaseID)
	images, err := s.resolveExportImages(ctx, kb, refs)
	if err != nil {
		return nil, err
	}
	export.Images = images
	return export, nil
}

// resolveExportImages loads the distinct resource:// handles referenced by one
// document. A handle that no longer resolves keeps its reference in the
// content (the reader sees the raw URL) instead of failing the download.
func (s *knowledgeService) resolveExportImages(
	ctx context.Context, kb *types.KnowledgeBase, refs []docparser.ArchiveImageRef,
) ([]types.KnowledgeExportImage, error) {
	seen := make(map[string]struct{}, len(refs))
	usedNames := make(map[string]struct{}, len(refs))
	images := make([]types.KnowledgeExportImage, 0, len(refs))
	for _, ref := range refs {
		if _, dup := seen[ref.Ref]; dup {
			continue
		}
		seen[ref.Ref] = struct{}{}
		if len(images) >= maxKnowledgeExportImages {
			logger.Warnf(ctx, "Export image cap %d reached, remaining references stay as-is", maxKnowledgeExportImages)
			break
		}

		resource, err := s.resourceCatalog.Resolve(ctx, ref.Ref)
		if err != nil || resource == nil {
			logger.Warnf(ctx, "Export image %s could not be resolved, keeping the reference", ref.Ref)
			continue
		}
		fileSvc := s.resolveFileServiceForPath(ctx, kb, ref.Ref)
		file, err := fileSvc.GetFile(ctx, ref.Ref)
		if err != nil {
			logger.Warnf(ctx, "Export image %s could not be read, keeping the reference: %v", ref.Ref, err)
			continue
		}
		data, overflow, err := readBounded(file, maxKnowledgeExportImageBytes)
		_ = file.Close()
		if err != nil {
			logger.Warnf(ctx, "Export image %s read failed, keeping the reference: %v", ref.Ref, err)
			continue
		}
		if overflow {
			logger.Warnf(ctx, "Export image %s exceeds %d bytes, keeping the reference", ref.Ref, maxKnowledgeExportImageBytes)
			continue
		}

		name := uniqueExportImageName(exportImageName(resource.OriginalName, resource.MimeType, ref.Ref), usedNames)
		images = append(images, types.KnowledgeExportImage{Ref: ref.Ref, Name: name, Data: data})
	}
	return images, nil
}

// exportImageName turns a stored resource's original name into a safe flat
// file name for the images/ directory, falling back to the handle plus a
// mime-derived extension when the name was never recorded.
func exportImageName(originalName, mimeType, ref string) string {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(originalName), "\\", "/"))
	if base == "" || base == "." || base == "/" {
		ext := mimeExtensionFor(mimeType)
		handle := strings.TrimPrefix(ref, types.ResourceScheme)
		if len(handle) > 8 {
			handle = handle[:8]
		}
		return "image-" + strings.ToLower(handle) + ext
	}
	safe, err := utils.SafeFileName(base)
	if err != nil || safe == "" {
		ext := filepath.Ext(base)
		return "image" + ext
	}
	return safe
}

func mimeExtensionFor(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "image/bmp":
		return ".bmp"
	default:
		return ".bin"
	}
}

// uniqueExportImageName resolves name collisions inside one document's
// images/ directory with a numeric suffix.
func uniqueExportImageName(name string, used map[string]struct{}) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	candidate := name
	for index := 2; ; index++ {
		if _, taken := used[strings.ToLower(candidate)]; !taken {
			used[strings.ToLower(candidate)] = struct{}{}
			return candidate
		}
		candidate = stem + "-" + strconv.Itoa(index) + ext
	}
}

// isMarkdownExportName reports whether a download file name is a Markdown
// document whose image references are worth restoring.
func isMarkdownExportName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return true
	default:
		return false
	}
}

// readBounded reads r fully unless it exceeds limit bytes; overflow reports
// that the content was longer than limit and the returned bytes are incomplete.
func readBounded(r io.Reader, limit int64) (data []byte, overflow bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}
