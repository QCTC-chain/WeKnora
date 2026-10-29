package docparser

import (
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/searchutil"
)

// ArchiveImageRef locates one relative image reference inside a Markdown
// document. The archive import service uses these spans both to look the
// reference up in the uploaded archive and to rewrite it in place once the
// image bytes have been persisted, so Kind records which syntax produced it.
type ArchiveImageRef struct {
	// Ref is the reference exactly as written in the document (percent
	// escapes and "./" prefixes included). Callers that need to match it
	// against archive entry names must normalize it themselves.
	Ref string
	// Start and End are byte offsets of the reference inside the Markdown
	// text; End is exclusive. The title suffix of an inline image
	// (![alt](path "title")) is deliberately outside the span so a rewrite
	// keeps the title.
	Start, End int
	// Kind tells markdown inline (![alt](ref)) from HTML <img src="ref">.
	Kind ImageRefKind
}

// ImageRefKind distinguishes the syntaxes an image reference can use.
type ImageRefKind int

const (
	// RefKindMarkdown is a ![alt](ref) inline image.
	RefKindMarkdown ImageRefKind = iota
	// RefKindHTML is an <img src="ref"> tag inside the Markdown text.
	RefKindHTML
	// RefKindReference is the definition line of a reference-style image
	// (![alt][label] used with [label]: ref); rewriting the definition
	// rewrites every usage that points at it.
	RefKindReference
)

// CollectRelativeImageRefs returns every relative-path image reference in the
// markdown, in document order. Remote (http/https), provider-scheme
// (local://, resource://, ...), data-URI and fragment-only references are
// already resolved by construction and are skipped, as are references inside
// fenced, indented or inline code — the same literal ranges the image
// resolver protects. Broken syntax (missing brackets, unbalanced parens) is
// ignored rather than guessed at.
func CollectRelativeImageRefs(markdown string) []ArchiveImageRef {
	return collectImageRefs(markdown, isRelativeImageRef)
}

// CollectProviderImageRefs returns every image reference that already uses a
// provider scheme (resource://, local://, minio://, ...) outside code
// literals — the inverse of CollectRelativeImageRefs. The download/export
// path uses it to turn stored handles back into files laid out beside the
// document.
func CollectProviderImageRefs(markdown string) []ArchiveImageRef {
	return collectImageRefs(markdown, isProviderScheme)
}

// collectImageRefs walks every image syntax and keeps the references the
// predicate accepts, in document order.
func collectImageRefs(markdown string, keep func(ref string) bool) []ArchiveImageRef {
	codeRanges := markdownCodeLiteralRanges(markdown)
	refs := make([]ArchiveImageRef, 0, 8)

	for _, span := range scanMarkdownImageTargets(markdown) {
		if _, inCode := codeLiteralRangeAt(codeRanges, span.ImageStart); inCode {
			continue
		}
		raw := markdown[span.TargetStart:span.TargetEnd]
		ref, start, end, ok := splitArchiveImageTarget(raw)
		if !ok || !keep(ref) {
			continue
		}
		refs = append(refs, ArchiveImageRef{
			Ref:   ref,
			Start: span.TargetStart + start,
			End:   span.TargetStart + end,
			Kind:  RefKindMarkdown,
		})
	}

	refs = append(refs, collectReferenceStyleImageRefs(markdown, keep, codeRanges)...)

	matches := searchutil.HTMLImageSrcRegex.FindAllStringSubmatchIndex(markdown, -1)
	for _, m := range matches {
		if _, inCode := codeLiteralRangeAt(codeRanges, m[0]); inCode {
			continue
		}
		ref := strings.TrimSpace(markdown[m[4]:m[5]])
		if ref == "" || !keep(ref) {
			continue
		}
		refs = append(refs, ArchiveImageRef{
			Ref:   ref,
			Start: m[4],
			End:   m[5],
			Kind:  RefKindHTML,
		})
	}

	sortArchiveImageRefs(refs)
	return refs
}

var (
	// Full (![alt][label]) and collapsed (![alt][]) reference-style usages.
	// The bare shortcut form (![alt]) is deliberately not treated as an image
	// reference: it is indistinguishable from ordinary text.
	referenceImageUsageRegex = regexp.MustCompile(`!\[[^\]\n]*\]\[([^\]\n]*)\]`)
	// A link definition at line start: [label]: target (optionally wrapped in
	// angle brackets, optionally followed by a title).
	referenceDefinitionRegex = regexp.MustCompile(`(?m)^[ \t]{0,3}\[([^\]\n]+)\]:[ \t]*<?([^\s>]+)>?`)
)

// collectReferenceStyleImageRefs returns the definition targets that back
// reference-style image usages. Only the first definition per label counts
// (CommonMark semantics), only labels actually used by an image occurrence
// are collected, and code literals are skipped for usages and definitions
// alike.
func collectReferenceStyleImageRefs(
	markdown string, keep func(ref string) bool, codeRanges []markdownCodeLiteralRange,
) []ArchiveImageRef {
	used := make(map[string]struct{})
	for _, m := range referenceImageUsageRegex.FindAllStringSubmatchIndex(markdown, -1) {
		if _, inCode := codeLiteralRangeAt(codeRanges, m[0]); inCode {
			continue
		}
		label := markdownLabelKey(markdown[m[2]:m[3]])
		if label == "" {
			// A collapsed usage ![alt][] falls back to the alt text.
			open := strings.Index(markdown[m[0]:m[1]], "]")
			if open <= 1 {
				continue
			}
			label = markdownLabelKey(markdown[m[0]+2 : m[0]+open])
		}
		if label != "" {
			used[label] = struct{}{}
		}
	}
	if len(used) == 0 {
		return nil
	}

	seenLabels := make(map[string]struct{})
	var refs []ArchiveImageRef
	for _, m := range referenceDefinitionRegex.FindAllStringSubmatchIndex(markdown, -1) {
		if _, inCode := codeLiteralRangeAt(codeRanges, m[0]); inCode {
			continue
		}
		label := markdownLabelKey(markdown[m[2]:m[3]])
		if _, isImageLabel := used[label]; !isImageLabel {
			continue
		}
		if _, dup := seenLabels[label]; dup {
			continue
		}
		seenLabels[label] = struct{}{}
		ref := markdown[m[4]:m[5]]
		if ref == "" || !keep(ref) {
			continue
		}
		refs = append(refs, ArchiveImageRef{
			Ref:   ref,
			Start: m[4],
			End:   m[5],
			Kind:  RefKindReference,
		})
	}
	return refs
}

// markdownLabelKey normalizes a link label for matching: CommonMark matches
// labels case-insensitively after trimming and whitespace collapsing.
func markdownLabelKey(label string) string {
	return strings.ToLower(strings.Join(strings.Fields(label), " "))
}

// RewriteRelativeImageRefs replaces every relative image reference for which
// resolve returns true with the URL resolve provides, leaving all other bytes
// — including titles, alt text and surrounding whitespace — untouched.
// Unresolved references are returned once each, in first-appearance order, so
// callers can report them; the document keeps their original paths.
// Rewriting an already-rewritten document is a no-op: rewritten references
// carry a scheme and are skipped by CollectRelativeImageRefs.
func RewriteRelativeImageRefs(
	markdown string, resolve func(ref string) (string, bool),
) (string, []string) {
	return rewriteImageRefs(CollectRelativeImageRefs(markdown), markdown, resolve)
}

// RewriteProviderImageRefs replaces every provider-scheme image reference for
// which resolve returns true with the URL resolve provides, under the same
// rules as RewriteRelativeImageRefs (titles kept, code skipped, back-to-front
// application, unresolved references returned once each).
func RewriteProviderImageRefs(
	markdown string, resolve func(ref string) (string, bool),
) (string, []string) {
	return rewriteImageRefs(CollectProviderImageRefs(markdown), markdown, resolve)
}

// rewriteImageRefs applies resolve to the collected spans and splices the
// replacements in back to front.
func rewriteImageRefs(
	refs []ArchiveImageRef, markdown string, resolve func(ref string) (string, bool),
) (string, []string) {
	if len(refs) == 0 {
		return markdown, nil
	}

	type rewrite struct {
		start, end int
		value      string
	}
	rewrites := make([]rewrite, 0, len(refs))
	seenUnresolved := make(map[string]struct{}, len(refs))
	var unresolved []string

	for _, ref := range refs {
		url, ok := resolve(ref.Ref)
		if !ok {
			if _, dup := seenUnresolved[ref.Ref]; !dup {
				seenUnresolved[ref.Ref] = struct{}{}
				unresolved = append(unresolved, ref.Ref)
			}
			continue
		}
		rewrites = append(rewrites, rewrite{start: ref.Start, end: ref.End, value: url})
	}

	// Apply back to front so earlier offsets stay valid.
	for i := len(rewrites) - 1; i >= 0; i-- {
		r := rewrites[i]
		markdown = markdown[:r.start] + r.value + markdown[r.end:]
	}
	return markdown, unresolved
}

// splitArchiveImageTarget extracts the destination path from the raw target
// text of an inline image (everything between the parentheses). It returns
// the path and its offsets within raw. Title suffixes — quoted, single-quoted
// or parenthesized, as parsed by parseMarkdownImageTitleSuffix — are excluded.
func splitArchiveImageTarget(raw string) (path string, start int, end int, ok bool) {
	start, end = trimMarkdownSpaceBounds(raw, 0, len(raw))
	if start >= end {
		return "", 0, 0, false
	}

	if raw[start] == '<' {
		closeIdx := -1
		for i := start + 1; i < end; i++ {
			if raw[i] == '>' && !isEscaped(raw, i) {
				closeIdx = i
				break
			}
		}
		if closeIdx == -1 {
			return "", 0, 0, false
		}
		if !isEmptyOrMarkdownImageTitleSuffix(raw[closeIdx+1 : end]) {
			return "", 0, 0, false
		}
		return raw[start+1 : closeIdx], start + 1, closeIdx, true
	}

	trimmed := raw[start:end]
	if titleStart, hasTitle := parseMarkdownImageTitleSuffix(trimmed); hasTitle {
		pathEnd := start + titleStart
		for pathEnd > start && isMarkdownSpace(raw[pathEnd-1]) {
			pathEnd--
		}
		if pathEnd == start {
			return "", 0, 0, false
		}
		return raw[start:pathEnd], start, pathEnd, true
	}
	return trimmed, start, end, true
}

// isRelativeImageRef reports whether a reference points at a file that could
// live next to the document. Everything with a scheme, a protocol-relative
// prefix or a bare fragment is already resolved (or unreachable from an
// archive) and must be left alone.
func isRelativeImageRef(ref string) bool {
	if ref == "" {
		return false
	}
	lower := strings.ToLower(ref)
	for _, prefix := range []string{
		"http://", "https://", "data:", "mailto:", "ftp://", "//", "#",
	} {
		if strings.HasPrefix(lower, prefix) {
			return false
		}
	}
	if isProviderScheme(ref) {
		return false
	}
	// A lone fragment like "#fig-1" is an in-page anchor, not a file.
	return true
}

func sortArchiveImageRefs(refs []ArchiveImageRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j].Start < refs[j-1].Start; j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}
