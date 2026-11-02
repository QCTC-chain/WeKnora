package archive

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// DirectoryArchive adapts an already-extracted knowledge package — a folder
// whose sub-directories and files look exactly like a zip's unpacked content
// — to the same read view the zip reader offers (ArchiveSource), so
// BuildManifest and the whole import tail run unchanged. This is what lets
// the `import-archives` subcommand import a mounted folder directly, with no
// zip step.
//
// Entry names are slash-separated paths relative to the folder root — the
// same normalized shape zip entries get. The same junk filter applies
// (__MACOSX, ._*, .DS_Store, Thumbs.db), symlinks and other non-regular
// files are skipped (a link must not pull in content from outside the
// folder), and the Limits budgets are enforced exactly like the zip reader:
// MaxEntries at open, MaxExpandedBytes/MaxSingleEntryBytes lazily at Read.
type DirectoryArchive struct {
	root     string
	names    []string
	paths    map[string]string
	limits   Limits
	expanded int64
	skipped  []SkippedEntry
}

// OpenDirectory walks the extracted package folder and indexes its regular
// files. File contents are not read here; Read enforces the byte budgets
// lazily, so a manifest scan costs only what the Markdown files weigh.
func OpenDirectory(root string, limits Limits) (*DirectoryArchive, error) {
	if limits.MaxEntries <= 0 {
		limits = DefaultLimits()
	}
	// Resolve symlinks in the mounted path itself (container mounts may be
	// links); everything below is still walked without following links.
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	d := &DirectoryArchive{
		root:   realRoot,
		paths:  make(map[string]string),
		limits: limits,
	}
	err = filepath.WalkDir(realRoot, func(p string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if p == realRoot {
				return nil
			}
			// A directory named like junk metadata (e.g. __MACOSX) is not
			// descended into, mirroring the zip reader's entry filter.
			if isJunkEntry(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(realRoot, p)
		if relErr != nil {
			d.skipped = append(d.skipped, SkippedEntry{Name: p, Reason: "不安全的路径，已忽略"})
			return nil
		}
		name := path.Clean(filepath.ToSlash(rel))
		if name == "." || name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
			d.skipped = append(d.skipped, SkippedEntry{Name: name, Reason: "不安全的路径，已忽略"})
			return nil
		}
		if isJunkEntry(name) {
			d.skipped = append(d.skipped, SkippedEntry{Name: name, Reason: "系统元数据文件，已忽略"})
			return nil
		}
		if !entry.Type().IsRegular() {
			d.skipped = append(d.skipped, SkippedEntry{Name: name, Reason: "非常规文件（链接等），已忽略"})
			return nil
		}
		if _, dup := d.paths[name]; dup {
			d.skipped = append(d.skipped, SkippedEntry{Name: name, Reason: "重复路径，保留首个"})
			return nil
		}
		if len(d.paths) >= limits.MaxEntries {
			return ErrTooManyFiles
		}
		d.paths[name] = p
		d.names = append(d.names, name)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(d.names)
	return d, nil
}

// Names returns the loadable entry names, sorted.
func (d *DirectoryArchive) Names() []string {
	out := make([]string, len(d.names))
	copy(out, d.names)
	return out
}

// Has reports whether name is a loadable entry.
func (d *DirectoryArchive) Has(name string) bool {
	_, ok := d.paths[name]
	return ok
}

// Skipped lists the entries ignored during the walk, for the import report.
func (d *DirectoryArchive) Skipped() []SkippedEntry {
	out := make([]SkippedEntry, len(d.skipped))
	copy(out, d.skipped)
	return out
}

// imageByteLimit exposes the manifest-time per-image cap (ArchiveSource).
func (d *DirectoryArchive) imageByteLimit() int64 {
	return d.limits.imageByteLimit()
}

// documentLimit exposes the per-package document cap (ArchiveSource).
func (d *DirectoryArchive) documentLimit() int {
	return d.limits.MaxDocuments
}

// Read loads one file with the package's byte budgets enforced, mirroring
// the zip reader: single-file and total caps apply to the bytes actually
// read.
func (d *DirectoryArchive) Read(name string) ([]byte, error) {
	p, ok := d.paths[name]
	if !ok {
		return nil, fmt.Errorf("目录中不存在文件 %s", name)
	}
	if d.expanded >= d.limits.MaxExpandedBytes {
		return nil, ErrTooLarge
	}
	file, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	defer file.Close()

	data, err := io.ReadAll(io.LimitReader(file, d.limits.MaxSingleEntryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadArchive, err)
	}
	if int64(len(data)) > d.limits.MaxSingleEntryBytes {
		return nil, fmt.Errorf("%w: %s 超出单文件大小限制", ErrTooLarge, name)
	}
	if d.expanded+int64(len(data)) > d.limits.MaxExpandedBytes {
		return nil, ErrTooLarge
	}
	d.expanded += int64(len(data))
	return data, nil
}
