package v2translate

import "testing"

// #919 regression guard: faultRuleFromMap must round-trip the FULL filter —
// request_kind AND variant AND url_match. Dropping variant/url_match on read
// made the dashboard's scope selector snap back after a moment (the response
// returned a filter-less rule, reverting the optimistic update).
func TestFaultRuleFromMap_FilterRoundTrip(t *testing.T) {
	rule := map[string]any{
		"id":   "r1",
		"type": "500",
		"filter": map[string]any{
			"request_kind": []any{"segment"},
			"variant": map[string]any{
				"rung_positions":  []any{"top", "bottom"},
				"rung_indexes":    []any{float64(0), float64(4)},
				"resolutions":     []any{"1920x1080"},
				"bandwidth_above": float64(6_000_000),
				"codec_prefix":    "avc1.",
			},
			"url_match": map[string]any{"mode": "regex", "patterns": []any{`seg_\d+`}},
		},
	}
	out := faultRuleFromMap(rule)
	if out.Filter == nil {
		t.Fatal("filter dropped entirely")
	}
	f := out.Filter
	if f.RequestKind == nil || len(*f.RequestKind) != 1 || string((*f.RequestKind)[0]) != "segment" {
		t.Errorf("request_kind not round-tripped: %v", f.RequestKind)
	}
	if f.Variant == nil {
		t.Fatal("variant predicate dropped — dashboard scope selector would revert")
	}
	v := f.Variant
	if v.RungPositions == nil || len(*v.RungPositions) != 2 {
		t.Errorf("rung_positions not round-tripped: %v", v.RungPositions)
	}
	if v.RungIndexes == nil || len(*v.RungIndexes) != 2 {
		t.Errorf("rung_indexes not round-tripped: %v", v.RungIndexes)
	}
	if v.Resolutions == nil || len(*v.Resolutions) != 1 || (*v.Resolutions)[0] != "1920x1080" {
		t.Errorf("resolutions not round-tripped: %v", v.Resolutions)
	}
	if v.BandwidthAbove == nil || *v.BandwidthAbove != 6_000_000 {
		t.Errorf("bandwidth_above not round-tripped: %v", v.BandwidthAbove)
	}
	if v.CodecPrefix == nil || *v.CodecPrefix != "avc1." {
		t.Errorf("codec_prefix not round-tripped: %v", v.CodecPrefix)
	}
	if f.UrlMatch == nil || string(f.UrlMatch.Mode) != "regex" || len(f.UrlMatch.Patterns) != 1 {
		t.Errorf("url_match not round-tripped: %v", f.UrlMatch)
	}
}

// TestFaultRuleFromMap_EmptyResolutionsPreserved: `variant.resolutions: []`
// (present-but-empty) means "match no video variant" — the scope selector's OFF
// state. It MUST round-trip as a present empty array, not be dropped, or the
// dashboard reads it back as "no narrowing" (all in scope) and the OFF flips
// back ON — the "changes being lost" symptom.
func TestFaultRuleFromMap_EmptyResolutionsPreserved(t *testing.T) {
	rule := map[string]any{
		"id":   "off",
		"type": "500",
		"filter": map[string]any{
			"request_kind": []any{"segment", "manifest", "master_manifest"},
			"variant":      map[string]any{"resolutions": []any{}},
		},
	}
	out := faultRuleFromMap(rule)
	if out.Filter == nil || out.Filter.Variant == nil {
		t.Fatal("variant predicate dropped for empty-resolutions OFF state")
	}
	if out.Filter.Variant.Resolutions == nil {
		t.Fatal("resolutions:[] dropped — OFF state would revert to ON")
	}
	if len(*out.Filter.Variant.Resolutions) != 0 {
		t.Errorf("resolutions should be empty, got %v", *out.Filter.Variant.Resolutions)
	}
}
