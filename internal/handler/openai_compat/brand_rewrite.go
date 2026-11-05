package openai_compat

import "strings"

// brandProductName is the vendor name the default prompt templates use for
// the assistant. Published agents are white-labelled: the gateway replaces it
// with brandReplacement in every client-visible text channel, so third-party
// clients (pi, deepseek harness, …) never see the product name.
const brandProductName = "weknora"

// brandReplacement is the fixed label that replaces brandProductName.
const brandReplacement = "qctc-chain-ai"

// brandRewriter rewrites case-insensitive occurrences of brandProductName in a
// streamed text channel. Text arrives in arbitrary chunks, so it holds back a
// trailing fragment that could still grow into the term; flush releases the
// held fragment at the end of the channel.
type brandRewriter struct {
	pending string
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

// feed rewrites one chunk and returns its safe prefix. A term split across
// chunks is rewritten once its tail arrives.
func (r *brandRewriter) feed(chunk string) string {
	data := r.pending + chunk
	r.pending = ""
	hold := brandPrefixSuffixLen(data)
	r.pending = data[len(data)-hold:]
	return replaceBrandFold(data[:len(data)-hold])
}

// flush releases the held fragment at the end of a channel.
func (r *brandRewriter) flush() string {
	if r.pending == "" {
		return ""
	}
	out := replaceBrandFold(r.pending)
	r.pending = ""
	return out
}

// replaceBrandFold replaces every case-insensitive occurrence of the product
// name, leaving every other byte untouched. ASCII-only folding keeps byte
// offsets aligned, so a multibyte character before the term cannot shift the
// match window.
func replaceBrandFold(s string) string {
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
		b.WriteString(brandReplacement)
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
