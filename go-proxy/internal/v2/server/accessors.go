package server

// getString reads a string field from a map[string]any, returning ""
// when missing/nil/non-string. Mirror of v2translate.getString — kept
// here so server-package code can use it without an import. Translation
// callers should use v2translate.* directly; this is for the
// non-translation code paths (handlers_groups, handlers_mutate,
// handlers_plays, events) that need a quick string-field lookup.
func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
