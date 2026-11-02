package utils

import "testing"

func TestGetMaxSkillBundleSizeMB(t *testing.T) {
	t.Run("defaults above the knowledge upload cap", func(t *testing.T) {
		t.Setenv("MAX_FILE_SIZE_MB", "")
		t.Setenv("MAX_SKILL_BUNDLE_SIZE_MB", "")
		if got := GetMaxSkillBundleSizeMB(); got != defaultMaxSkillBundleSizeMB {
			t.Fatalf("GetMaxSkillBundleSizeMB() = %d, want %d", got, defaultMaxSkillBundleSizeMB)
		}
		if GetMaxFileSizeMB() != defaultMaxFileSizeMB {
			t.Fatalf("knowledge cap must stay %d MB", defaultMaxFileSizeMB)
		}
	})

	t.Run("honours an explicit skill cap above the knowledge cap", func(t *testing.T) {
		t.Setenv("MAX_FILE_SIZE_MB", "50")
		t.Setenv("MAX_SKILL_BUNDLE_SIZE_MB", "300")
		if got := GetMaxSkillBundleSizeMB(); got != 300 {
			t.Fatalf("GetMaxSkillBundleSizeMB() = %d, want 300", got)
		}
	})

	t.Run("never below the knowledge upload cap", func(t *testing.T) {
		t.Setenv("MAX_FILE_SIZE_MB", "200")
		t.Setenv("MAX_SKILL_BUNDLE_SIZE_MB", "64")
		if got := GetMaxSkillBundleSizeMB(); got != 200 {
			t.Fatalf("GetMaxSkillBundleSizeMB() = %d, want 200", got)
		}
	})

	t.Run("caps at the uncompressed archive ceiling", func(t *testing.T) {
		t.Setenv("MAX_FILE_SIZE_MB", "50")
		t.Setenv("MAX_SKILL_BUNDLE_SIZE_MB", "4096")
		if got := GetMaxSkillBundleSizeMB(); got != maxSkillBundleSizeMBCeiling {
			t.Fatalf("GetMaxSkillBundleSizeMB() = %d, want %d", got, maxSkillBundleSizeMBCeiling)
		}
	})
}

func TestGetLocalImportDefaults(t *testing.T) {
	t.Run("finite defaults well above the upload caps", func(t *testing.T) {
		t.Setenv("LOCAL_IMPORT_MAX_DOCUMENTS", "")
		t.Setenv("LOCAL_IMPORT_MAX_ENTRIES", "")
		t.Setenv("LOCAL_IMPORT_MAX_EXPANDED_SIZE_MB", "")
		t.Setenv("LOCAL_IMPORT_MAX_SINGLE_ENTRY_SIZE_MB", "")
		t.Setenv("LOCAL_IMPORT_MAX_IMAGE_SIZE_MB", "")
		t.Setenv("LOCAL_IMPORT_MAX_IMAGES_PER_DOCUMENT", "")

		if got := GetLocalImportMaxDocuments(); got != defaultLocalImportMaxDocuments {
			t.Fatalf("documents = %d, want %d", got, defaultLocalImportMaxDocuments)
		}
		if got := GetLocalImportMaxEntries(); got != defaultLocalImportMaxEntries {
			t.Fatalf("entries = %d, want %d", got, defaultLocalImportMaxEntries)
		}
		if got := GetLocalImportMaxExpandedSizeMB(); got != defaultLocalImportMaxExpandedSizeMB {
			t.Fatalf("expanded = %d, want %d", got, defaultLocalImportMaxExpandedSizeMB)
		}
		if got := GetLocalImportMaxSingleEntrySizeMB(); got != defaultLocalImportMaxSingleEntryMB {
			t.Fatalf("single entry = %d, want %d", got, defaultLocalImportMaxSingleEntryMB)
		}
		if got := GetLocalImportMaxImageSizeMB(); got != defaultLocalImportMaxImageSizeMB {
			t.Fatalf("image = %d, want %d", got, defaultLocalImportMaxImageSizeMB)
		}
		if got := GetLocalImportMaxImagesPerDocument(); got != defaultLocalImportMaxImagesPerDoc {
			t.Fatalf("images/doc = %d, want %d", got, defaultLocalImportMaxImagesPerDoc)
		}
	})

	t.Run("honours explicit env overrides", func(t *testing.T) {
		t.Setenv("LOCAL_IMPORT_MAX_DOCUMENTS", "10")
		t.Setenv("LOCAL_IMPORT_MAX_ENTRIES", "20")
		t.Setenv("LOCAL_IMPORT_MAX_EXPANDED_SIZE_MB", "30")
		t.Setenv("LOCAL_IMPORT_MAX_SINGLE_ENTRY_SIZE_MB", "40")
		t.Setenv("LOCAL_IMPORT_MAX_IMAGE_SIZE_MB", "5")
		t.Setenv("LOCAL_IMPORT_MAX_IMAGES_PER_DOCUMENT", "50")

		if got := GetLocalImportMaxDocuments(); got != 10 {
			t.Fatalf("documents = %d, want 10", got)
		}
		if got := GetLocalImportMaxEntries(); got != 20 {
			t.Fatalf("entries = %d, want 20", got)
		}
		if got := GetLocalImportMaxExpandedSizeMB(); got != 30 {
			t.Fatalf("expanded = %d, want 30", got)
		}
		if got := GetLocalImportMaxSingleEntrySizeMB(); got != 40 {
			t.Fatalf("single entry = %d, want 40", got)
		}
		if got := GetLocalImportMaxImageSizeMB(); got != 5 {
			t.Fatalf("image = %d, want 5", got)
		}
		if got := GetLocalImportMaxImagesPerDocument(); got != 50 {
			t.Fatalf("images/doc = %d, want 50", got)
		}
	})

	t.Run("ignores invalid env values", func(t *testing.T) {
		t.Setenv("LOCAL_IMPORT_MAX_DOCUMENTS", "not-a-number")
		t.Setenv("LOCAL_IMPORT_MAX_IMAGE_SIZE_MB", "-3")
		if got := GetLocalImportMaxDocuments(); got != defaultLocalImportMaxDocuments {
			t.Fatalf("documents = %d, want default %d", got, defaultLocalImportMaxDocuments)
		}
		if got := GetLocalImportMaxImageSizeMB(); got != defaultLocalImportMaxImageSizeMB {
			t.Fatalf("image = %d, want default %d", got, defaultLocalImportMaxImageSizeMB)
		}
	})
}
