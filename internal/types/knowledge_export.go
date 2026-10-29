package types

import "io"

// KnowledgeExportImage is one content-bound image resolved for a download
// export: Ref is the resource:// handle embedded in the document, Name the
// file name it should take inside the restored images/ directory.
type KnowledgeExportImage struct {
	Ref  string
	Name string
	Data []byte
}

// KnowledgeExportFile is the downloadable original of one knowledge entry:
// the document bytes plus every image needed to restore an images/ directory
// beside it, so a zip-uploaded archive round-trips through download.
type KnowledgeExportFile struct {
	Content io.ReadCloser
	Name    string
	// Images is empty when the document references no content-bound resources.
	Images []KnowledgeExportImage
}
