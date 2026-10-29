package types

// ArchiveImportReport is the response payload of a zip archive upload: one
// item per Markdown document found in the archive, plus the entries that were
// skipped without creating a knowledge record.
type ArchiveImportReport struct {
	// Type marks this payload as an archive-expansion report so upload
	// clients can tell it apart from a single knowledge payload.
	Type    string              `json:"type"`
	Items   []ArchiveImportItem `json:"items"`
	Skipped []ArchiveImportItem `json:"skipped"`
}

// TypeArchiveImportReport is the value of ArchiveImportReport.Type.
const TypeArchiveImportReport = "archive"

// ArchiveImportItem is the per-document result inside an archive import.
// Status is one of "created", "duplicate", "failed" or "skipped".
type ArchiveImportItem struct {
	KnowledgeID   string   `json:"knowledge_id,omitempty"`
	Title         string   `json:"title"`
	FolderPath    string   `json:"folder_path,omitempty"`
	Status        string   `json:"status"`
	ImagesStored  int      `json:"images_stored"`
	ImagesMissing int      `json:"images_missing"`
	Warnings      []string `json:"warnings,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// Archive import item statuses.
const (
	ArchiveItemCreated   = "created"
	ArchiveItemDuplicate = "duplicate"
	ArchiveItemFailed    = "failed"
	ArchiveItemSkipped   = "skipped"
)

// CreatedCount returns how many documents were created successfully.
func (r *ArchiveImportReport) CreatedCount() int {
	return r.countStatus(ArchiveItemCreated)
}

// FailedCount returns how many documents failed to import.
func (r *ArchiveImportReport) FailedCount() int {
	return r.countStatus(ArchiveItemFailed)
}

// DuplicateCount returns how many documents were skipped as duplicates.
func (r *ArchiveImportReport) DuplicateCount() int {
	return r.countStatus(ArchiveItemDuplicate)
}

func (r *ArchiveImportReport) countStatus(status string) int {
	if r == nil {
		return 0
	}
	count := 0
	for i := range r.Items {
		if r.Items[i].Status == status {
			count++
		}
	}
	return count
}
