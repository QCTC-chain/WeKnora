package openai_compat

import "strings"

// brandProductName is the vendor name the default prompt templates use for
// the assistant. Published agents are white-labelled: the gateway replaces it
// with the agent's own name in the reasoning channel, so third-party clients
// (pi, deepseek harness, …) never show the product name in the thinking trace.
const brandProductName = "weknora"

// brandRewriter rewrites case-insensitive occurrences of brandProductName in a
// streamed text channel. Reasoning arrives in arbitrary chunks, so it holds
// back a trailing fragment that could still grow into the term; flush releases
// the held fragment at the end of a thought phase.
type brandRewriter struct {
	name    string
	enabled bool
	pending string
}

// newBrandRewriter returns a rewriter for one run; an empty or identical name
// disables rewriting so the stream passes through untouched.
func newBrandRewriter(name string) *brandRewriter {
	name = strings.TrimSpace(name)
	return &brandRewriter{
		name:    name,
		enabled: name != "" && !strings.EqualFold(name, brandProductName),
	}
}

// feed rewrites one chunk and returns its safe prefix. A term split across
// chunks is rewritten once its tail arrives.
func (r *brandRewriter) feed(chunk string) string {
	if r == nil || !r.enabled {
		return chunk
	}
	data := r.pending + chunk
	r.pending = ""
	hold := brandPrefixSuffixLen(data)
	r.pending = data[len(data)-hold:]
	return replaceBrandFold(data[:len(data)-hold], r.name)
}

// flush releases the held fragment at the end of a thought phase.
func (r *brandRewriter) flush() string {
	if r == nil || !r.enabled || r.pending == "" {
		return ""
	}
	out := replaceBrandFold(r.pending, r.name)
	r.pending = ""
	return out
}

// brandPrefixSuffixLen returns the length of the longest suffix of data that
// could still grow into brandProductName. A complete term is not held: it is
// rewritten immediately.
func brandPrefixSuffixLen(data string) int {
	max := len(brandProductName) - 1
	if len(data) < max {
		max = len(data)
	}
	for n := max; n > 0; n-- {
		if asciiEqualFold(data[len(data)-n:], brandProductName[:n]) {
			return n
		}
	}
	return 0
}

// replaceBrandFold replaces every case-insensitive occurrence of the product
// name, leaving every other byte untouched. ASCII-only folding keeps byte
// offsets aligned, so a multibyte character before the term cannot shift the
// match window.
func replaceBrandFold(s, replacement string) string {
	if len(s) < len(brandProductName) {
		return s
	}
	var b strings.Builder
	start := 0
	for i := 0; i+len(brandProductName) <= len(s); {
		if !asciiEqualFold(s[i:i+len(brandProductName)], brandProductName) {
			i++
			continue
		}
		b.WriteString(s[start:i])
		b.WriteString(replacement)
		i += len(brandProductName)
		start = i
	}
	if start == 0 {
		return s
	}
	b.WriteString(s[start:])
	return b.String()
}

// asciiEqualFold compares ASCII case-insensitively; both inputs must have the
// same length.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if asciiLower(a[i]) != asciiLower(b[i]) {
			return false
		}
	}
	return true
}

func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}
