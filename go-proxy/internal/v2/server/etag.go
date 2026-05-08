package server

import "strings"

// parseIfMatch extracts the bare revision token from an RFC 7232 strong
// ETag header (`"<rev>"`). Returns empty string if the header is empty,
// malformed, or wildcard. Wildcard `*` is rejected per the v2 design
// (DESIGN.md § ETag quoting): clients must echo the previous response's
// strong tag verbatim.
//
// Comma-separated multi-tag lists are not supported in v2 either; the
// first comma-separated entry is used.
func parseIfMatch(h string) string {
	h = strings.TrimSpace(h)
	if h == "" || h == "*" {
		return ""
	}
	if comma := strings.IndexByte(h, ','); comma >= 0 {
		h = strings.TrimSpace(h[:comma])
	}
	// Strong tag is `"<value>"`. Strip the surrounding double quotes.
	if len(h) >= 2 && h[0] == '"' && h[len(h)-1] == '"' {
		inner := h[1 : len(h)-1]
		// Reject any embedded quotes — strong tags can't contain
		// unescaped `"` per RFC 7232.
		if strings.ContainsRune(inner, '"') {
			return ""
		}
		return inner
	}
	// Tolerate unquoted form (some HTTP libraries strip the quotes
	// before exposing the header), but reject anything that contains
	// raw quotes elsewhere.
	if strings.ContainsRune(h, '"') {
		return ""
	}
	return h
}

// formatETag wraps a revision in RFC 7232 strong-tag quotes.
func formatETag(rev string) string {
	return `"` + rev + `"`
}
