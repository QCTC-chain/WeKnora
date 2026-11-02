package types

// LocalArchiveImportReport is the outcome of one container-local bulk zip
// import run (the `import-archives` server subcommand): one item per
// processed .zip package found under the import directory. It is an
// operator-facing report, printed by the subcommand; it is never returned
// over an HTTP route.
type LocalArchiveImportReport struct {
	// Type marks this payload as a local bulk-import report.
	Type string `json:"type"`
	// Dir is the import directory the packages were scanned from.
	Dir string `json:"dir"`
	// KnowledgeBaseID is the KB every package was imported into.
	KnowledgeBaseID string                   `json:"knowledge_base_id"`
	Items           []LocalArchiveImportItem `json:"items"`
	// ImportedZips / FailedZips summarize the per-package outcomes for zip
	// packages; ImportedDirs / FailedDirs the same for directory packages
	// (mounted folders imported without a zip step).
	ImportedZips int `json:"imported_zips"`
	FailedZips   int `json:"failed_zips"`
	ImportedDirs int `json:"imported_dirs"`
	FailedDirs   int `json:"failed_dirs"`
	// Notes carry operator-relevant remarks about the run shape, e.g. that
	// the import root itself was imported as one package and kept in place.
	Notes []string `json:"notes,omitempty"`
}

// TypeLocalArchiveImportReport is the value of LocalArchiveImportReport.Type.
const TypeLocalArchiveImportReport = "local_archive"

// LocalArchiveImportItem is one package's outcome: a .zip file or a
// directory imported without a zip step.
type LocalArchiveImportItem struct {
	// Path is the package's path at scan time (before it was moved).
	Path string `json:"path"`
	// Kind is LocalArchiveKindZip or LocalArchiveKindDir.
	Kind string `json:"kind,omitempty"`
	// Status is LocalArchiveStatusImported when every document of the
	// package was created or recognized as a duplicate, and
	// LocalArchiveStatusFailed when the package could not be opened/scanned
	// or any document failed.
	Status string `json:"status"`
	// Error carries the first failure reason (package-level or document
	// count summary); empty for imported packages.
	Error string `json:"error,omitempty"`
	// MovedTo is the destination the package was moved to after processing
	// (imported/ or failed/ under the import directory). Empty when the
	// move failed — the package then stays in place.
	MovedTo      string               `json:"moved_to,omitempty"`
	Created      int                  `json:"created"`
	Duplicate    int                  `json:"duplicate"`
	Failed       int                  `json:"failed"`
	SkippedFiles int                  `json:"skipped_files"`
	Report       *ArchiveImportReport `json:"report,omitempty"`
}

// Local bulk-import package statuses and kinds.
const (
	LocalArchiveStatusImported = "imported"
	LocalArchiveStatusFailed   = "failed"

	LocalArchiveKindZip = "zip"
	LocalArchiveKindDir = "dir"
)
