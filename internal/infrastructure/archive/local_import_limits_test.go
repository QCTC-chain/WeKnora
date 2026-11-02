package archive

import (
	"testing"
)

// The local bulk import (import-archives subcommand) must keep FINITE limits:
// generous relative to the upload caps, but never unlimited, so a runaway
// package cannot exhaust disk or memory.

func TestLocalImportLimits_FiniteDefaultsAboveUpload(t *testing.T) {
	for _, key := range []string{
		"LOCAL_IMPORT_MAX_DOCUMENTS", "LOCAL_IMPORT_MAX_ENTRIES",
		"LOCAL_IMPORT_MAX_EXPANDED_SIZE_MB", "LOCAL_IMPORT_MAX_SINGLE_ENTRY_SIZE_MB",
		"LOCAL_IMPORT_MAX_IMAGE_SIZE_MB",
	} {
		t.Setenv(key, "")
	}
	limits := LocalImportLimits()
	upload := DefaultLimits()

	if limits.MaxDocuments <= upload.MaxDocuments {
		t.Fatalf("MaxDocuments = %d, want above upload cap %d", limits.MaxDocuments, upload.MaxDocuments)
	}
	if limits.MaxEntries <= upload.MaxEntries {
		t.Fatalf("MaxEntries = %d, want above upload cap %d", limits.MaxEntries, upload.MaxEntries)
	}
	if limits.MaxExpandedBytes <= upload.MaxExpandedBytes {
		t.Fatalf("MaxExpandedBytes = %d, want above upload cap %d", limits.MaxExpandedBytes, upload.MaxExpandedBytes)
	}
	if limits.MaxSingleEntryBytes <= upload.MaxSingleEntryBytes {
		t.Fatalf("MaxSingleEntryBytes = %d, want above upload cap %d", limits.MaxSingleEntryBytes, upload.MaxSingleEntryBytes)
	}
	if limits.MaxImageBytes <= upload.MaxImageBytes {
		t.Fatalf("MaxImageBytes = %d, want above upload cap %d", limits.MaxImageBytes, upload.MaxImageBytes)
	}
}

func TestLocalImportLimits_EnvOverrides(t *testing.T) {
	t.Setenv("LOCAL_IMPORT_MAX_DOCUMENTS", "42")
	t.Setenv("LOCAL_IMPORT_MAX_ENTRIES", "99")
	t.Setenv("LOCAL_IMPORT_MAX_EXPANDED_SIZE_MB", "16")
	t.Setenv("LOCAL_IMPORT_MAX_SINGLE_ENTRY_SIZE_MB", "8")
	t.Setenv("LOCAL_IMPORT_MAX_IMAGE_SIZE_MB", "2")

	limits := LocalImportLimits()
	if limits.MaxDocuments != 42 || limits.MaxEntries != 99 {
		t.Fatalf("counts = %d/%d, want 42/99", limits.MaxDocuments, limits.MaxEntries)
	}
	if limits.MaxExpandedBytes != 16<<20 || limits.MaxSingleEntryBytes != 8<<20 {
		t.Fatalf("bytes = %d/%d, want %d/%d",
			limits.MaxExpandedBytes, limits.MaxSingleEntryBytes, 16<<20, 8<<20)
	}
	if limits.MaxImageBytes != 2<<20 {
		t.Fatalf("MaxImageBytes = %d, want %d", limits.MaxImageBytes, 2<<20)
	}
}

func TestDefaultLimits_KeepsUploadImageCap(t *testing.T) {
	if got := DefaultLimits().MaxImageBytes; got != maxImageBytes {
		t.Fatalf("upload MaxImageBytes = %d, want %d", got, maxImageBytes)
	}
}

// The manifest builder must honour the limits-based per-image cap so the
// local import can admit images the upload path would flag as oversized.
func TestBuildManifest_RespectsLimitsImageCap(t *testing.T) {
	big := make([]byte, 64<<20)
	files := map[string][]byte{
		"doc.md":       []byte("![a](images/a.png)\n"),
		"images/a.png": big,
	}

	t.Run("local limits admit the image", func(t *testing.T) {
		limits := Limits{
			MaxDocuments:        10,
			MaxEntries:          100,
			MaxExpandedBytes:    256 << 20,
			MaxSingleEntryBytes: 128 << 20,
			MaxImageBytes:       128 << 20,
		}
		a := openArchive(t, writeZip(t, files, nil), limits)
		m, err := BuildManifest(a)
		if err != nil {
			t.Fatalf("BuildManifest: %v", err)
		}
		if len(m.Documents) != 1 || len(m.Documents[0].Images) != 1 {
			t.Fatalf("want 1 document with 1 stored image, got %+v", m.Documents)
		}
		if len(m.Documents[0].Missing) != 0 {
			t.Fatalf("missing = %v, want none", m.Documents[0].Missing)
		}
	})

	t.Run("tight cap keeps the reference as missing", func(t *testing.T) {
		limits := Limits{
			MaxDocuments:        10,
			MaxEntries:          100,
			MaxExpandedBytes:    256 << 20,
			MaxSingleEntryBytes: 128 << 20,
			MaxImageBytes:       1 << 20,
		}
		a := openArchive(t, writeZip(t, files, nil), limits)
		m, err := BuildManifest(a)
		if err != nil {
			t.Fatalf("BuildManifest: %v", err)
		}
		if len(m.Documents) != 1 || len(m.Documents[0].Images) != 0 {
			t.Fatalf("want 1 document with no stored image, got %+v", m.Documents)
		}
		if len(m.Documents[0].Missing) != 1 {
			t.Fatalf("missing = %v, want the oversized reference", m.Documents[0].Missing)
		}
	})
}
