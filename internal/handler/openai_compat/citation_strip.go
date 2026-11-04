package openai_compat

import "strings"

// WeKnora emits inline citations as self-closing XML-ish tags in the answer
// body: <kb doc="…" chunk_id="…" kb_id="…" /> and <web url="…" title="…" />
// (see internal/modelcontext/citations.go). The tags are the WeKnora web
// frontend's citation contract; OpenAI-compatible clients cannot render
// them and they break Markdown display. The gateway strips them from the
// answer body; the appended numbered source list (renderCitations) carries
// the citations instead.
var citationStripTagNames = []string{"kb", "web"}

// citationTagStripper removes <kb …/> / <web …/> citation tags from answer
// text. It withholds a trailing run that could still grow into a tag
// opening, so a tag split across answer chunks is removed whole regardless
// of where the event boundary falls. Methods must be called under the same
// lock that guards the accumulated answer (streamTranslator.mu).
type citationTagStripper struct {
	pending string
}

// feed strips one chunk of answer text. Call flush once after the last
// chunk to dispose of a held fragment.
func (s *citationTagStripper) feed(chunk string) string {
	data := s.pending + chunk
	s.pending = ""
	var out strings.Builder
	for data != "" {
		idx := strings.IndexByte(data, '<')
		if idx < 0 {
			out.WriteString(data)
			break
		}
		out.WriteString(data[:idx])
		data = data[idx:]
		if end := citationTagEnd(data); end >= 0 {
			// A citation tag: drop it whole, attributes included.
			data = data[end+1:]
			continue
		}
		if isCitationTagOpening(data) || couldGrowIntoCitationTag(data) {
			// An unclosed tag opening, or a fragment that might become one
			// in the next chunk; hold it back.
			s.pending = data
			break
		}
		out.WriteByte('<')
		data = data[1:]
	}
	return out.String()
}

// flush disposes of the held fragment at end of stream. Only potential tag
// openings or unclosed tag bodies are ever held; both are dropped (an
// unclosed tag carries no visible text), matching the disposition of
// incomplete source tags in internal/modelcontext/citations.go.
func (s *citationTagStripper) flush() string {
	s.pending = ""
	return ""
}

// citationTagEnd reports the index of the closing '>' when data opens a
// citation tag, or -1 when it does not (yet).
func citationTagEnd(data string) int {
	lower := strings.ToLower(data)
	for _, name := range citationStripTagNames {
		prefix := "<" + name
		if !strings.HasPrefix(lower, prefix) || len(data) == len(prefix) {
			continue
		}
		switch data[len(prefix)] {
		case ' ', '\t', '\r', '\n', '/', '>':
			if end := strings.IndexByte(data, '>'); end >= 0 {
				return end
			}
			return -1
		}
	}
	return -1
}

// isCitationTagOpening reports whether data starts a citation tag whose
// closing '>' has not arrived yet ("<kb", "<kb doc=…" with no '>').
func isCitationTagOpening(data string) bool {
	lower := strings.ToLower(data)
	for _, name := range citationStripTagNames {
		prefix := "<" + name
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		if len(data) == len(prefix) {
			return true
		}
		switch data[len(prefix)] {
		case ' ', '\t', '\r', '\n', '/', '>':
			return true
		}
	}
	return false
}

// couldGrowIntoCitationTag reports whether data could still grow into a
// citation tag opening in a later chunk ("<", "<K", "<we", "<kb…", …).
func couldGrowIntoCitationTag(data string) bool {
	lower := strings.ToLower(data)
	for _, name := range citationStripTagNames {
		if strings.HasPrefix("<"+name, lower) {
			return true
		}
	}
	return false
}

// stripCitationTagsComplete strips a complete answer string (non-streaming
// path): feed plus flush in one step.
func stripCitationTagsComplete(text string) string {
	s := &citationTagStripper{}
	return s.feed(text) + s.flush()
}
