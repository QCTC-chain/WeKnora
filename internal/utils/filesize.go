package utils

import (
	"os"
	"strconv"
)

const (
	defaultMaxFileSizeMB        = 50
	defaultMaxSkillBundleSizeMB = 256
	// maxSkillBundleSizeMBCeiling matches the install-time uncompressed
	// archive cap: a download larger than that cannot become a valid skill.
	maxSkillBundleSizeMBCeiling = 512
)

// GetMaxFileSize returns the maximum file upload size in bytes.
// Default is 50MB, can be configured via MAX_FILE_SIZE_MB environment variable.
//
// MAX_FILE_SIZE_MB is intentionally a deploy-time-only knob (NOT a
// runtime system_setting). The effective upload limit is gated by
// three other layers that all read this env at startup and cache the
// value:
//   - frontend nginx client_max_body_size (envsubst into nginx.conf)
//   - docreader gRPC max_send/recv_message_length
//   - frontend client-side check via window.__RUNTIME_CONFIG__
//
// Surfacing a SystemAdmin UI knob whose effect is silently capped by
// any of the above would mislead operators ("I raised it to 200MB but
// nginx still returns 413"). Until all four layers can be reconfigured
// in lockstep without container restarts, every call site must read
// the env directly via this helper.
func GetMaxFileSize() int64 {
	return GetMaxFileSizeMB() * 1024 * 1024
}

// GetMaxFileSizeMB returns the maximum file upload size in MB. Same
// caveat as GetMaxFileSize — handlers should prefer SystemSettingService.GetInt.
func GetMaxFileSizeMB() int64 {
	return envSizeMB("MAX_FILE_SIZE_MB", defaultMaxFileSizeMB)
}

// GetMaxSkillBundleSize is the compressed zip / source-download cap for
// skills. Knowledge uploads stay on GetMaxFileSize: packages such as
// ppt-master exceed 50MB, but raising the document limit would also
// inflate docreader gRPC messages. A GitHub zipball is the whole
// repository archive, not the SKILL.md subtree, so a huge monorepo can
// still be refused here even when the skill itself is small.
func GetMaxSkillBundleSize() int64 {
	return GetMaxSkillBundleSizeMB() * 1024 * 1024
}

// GetMaxSkillBundleSizeMB returns the skill zip cap in MB.
// MAX_SKILL_BUNDLE_SIZE_MB defaults to 256, is never below MAX_FILE_SIZE_MB,
// and is capped at 512.
func GetMaxSkillBundleSizeMB() int64 {
	skillMB := envSizeMB("MAX_SKILL_BUNDLE_SIZE_MB", defaultMaxSkillBundleSizeMB)
	if fileMB := GetMaxFileSizeMB(); skillMB < fileMB {
		skillMB = fileMB
	}
	if skillMB > maxSkillBundleSizeMBCeiling {
		return maxSkillBundleSizeMBCeiling
	}
	return skillMB
}

func envSizeMB(key string, fallback int64) int64 {
	if sizeStr := os.Getenv(key); sizeStr != "" {
		if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil && size > 0 {
			return size
		}
	}
	return fallback
}

const defaultMaxArchiveSizeMB = 200

// GetMaxArchiveSize returns the maximum size in bytes for a .zip knowledge
// archive upload (a package of Markdown documents plus their images).
// ARCHIVE_MAX_SIZE_MB defaults to 200. Like MAX_FILE_SIZE_MB this is a
// deploy-time-only knob: the frontend nginx client_max_body_size must be
// raised in lockstep or oversized archives are cut off by the proxy before
// the application ever sees them.
func GetMaxArchiveSize() int64 {
	return GetMaxArchiveSizeMB() * 1024 * 1024
}

// GetMaxArchiveSizeMB returns the archive upload cap in MB. Same caveat as
// GetMaxArchiveSize — deploy-time only.
func GetMaxArchiveSizeMB() int64 {
	return envSizeMB("ARCHIVE_MAX_SIZE_MB", defaultMaxArchiveSizeMB)
}

// GetMaxArchiveEntries returns how many regular files one .zip knowledge
// archive may contain. Skipped entries (system metadata like __MACOSX,
// links, duplicates) do not count. ARCHIVE_MAX_ENTRIES defaults to 5000 and
// is a deploy-time knob for packages with unusually many assets.
func GetMaxArchiveEntries() int {
	if entriesStr := os.Getenv("ARCHIVE_MAX_ENTRIES"); entriesStr != "" {
		if entries, err := strconv.Atoi(entriesStr); err == nil && entries > 0 {
			return entries
		}
	}
	return defaultMaxArchiveEntries
}

const defaultMaxArchiveEntries = 5000

// Container-local bulk zip import (the `import-archives` server subcommand).
// The mounted-folder import has no HTTP upload frontier, so the upload caps
// (ARCHIVE_MAX_SIZE_MB and friends) do not apply to it; these knobs replace
// them. The defaults are deliberately finite — generous enough for
// image-heavy multi-GiB packages, but extraction can never run away — and
// every value is operator-tunable per deployment. Deploy-time env only,
// read by archive.LocalImportLimits and the import service; the public
// upload path keeps the stricter ARCHIVE_* limits.
const (
	defaultLocalImportMaxDocuments      = 2000
	defaultLocalImportMaxEntries        = 20000
	defaultLocalImportMaxExpandedSizeMB = 8192 // 8 GiB extracted per package
	defaultLocalImportMaxSingleEntryMB  = 1024 // 1 GiB per entry
	defaultLocalImportMaxImageSizeMB    = 64
	defaultLocalImportMaxImagesPerDoc   = 5000
)

// GetLocalImportMaxDocuments returns how many Markdown/attachment documents
// one local-import zip may yield (LOCAL_IMPORT_MAX_DOCUMENTS, default 2000).
func GetLocalImportMaxDocuments() int {
	return envInt("LOCAL_IMPORT_MAX_DOCUMENTS", defaultLocalImportMaxDocuments)
}

// GetLocalImportMaxEntries returns how many regular files one local-import
// zip may contain (LOCAL_IMPORT_MAX_ENTRIES, default 20000).
func GetLocalImportMaxEntries() int {
	return envInt("LOCAL_IMPORT_MAX_ENTRIES", defaultLocalImportMaxEntries)
}

// GetLocalImportMaxExpandedSizeMB returns the extracted-bytes budget for one
// local-import zip (LOCAL_IMPORT_MAX_EXPANDED_SIZE_MB, default 8192).
func GetLocalImportMaxExpandedSizeMB() int64 {
	return envSizeMB("LOCAL_IMPORT_MAX_EXPANDED_SIZE_MB", defaultLocalImportMaxExpandedSizeMB)
}

// GetLocalImportMaxSingleEntrySizeMB returns the per-entry extracted cap for
// one local-import zip (LOCAL_IMPORT_MAX_SINGLE_ENTRY_SIZE_MB, default 1024).
func GetLocalImportMaxSingleEntrySizeMB() int64 {
	return envSizeMB("LOCAL_IMPORT_MAX_SINGLE_ENTRY_SIZE_MB", defaultLocalImportMaxSingleEntryMB)
}

// GetLocalImportMaxImageSizeMB returns the per-image cap for referenced
// images in one local-import zip (LOCAL_IMPORT_MAX_IMAGE_SIZE_MB, default 64).
func GetLocalImportMaxImageSizeMB() int64 {
	return envSizeMB("LOCAL_IMPORT_MAX_IMAGE_SIZE_MB", defaultLocalImportMaxImageSizeMB)
}

// GetLocalImportMaxImagesPerDocument returns how many images one document of
// a local-import zip may inline (LOCAL_IMPORT_MAX_IMAGES_PER_DOCUMENT,
// default 5000). Note this stays independent of the parse pipeline's own
// per-document inline budget: excess images are stored but reported, not
// silently relaxed downstream.
func GetLocalImportMaxImagesPerDocument() int {
	return envInt("LOCAL_IMPORT_MAX_IMAGES_PER_DOCUMENT", defaultLocalImportMaxImagesPerDoc)
}

func envInt(key string, fallback int) int {
	if valueStr := os.Getenv(key); valueStr != "" {
		if value, err := strconv.Atoi(valueStr); err == nil && value > 0 {
			return value
		}
	}
	return fallback
}
