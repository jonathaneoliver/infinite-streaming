package main

import (
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/v2gen/proxy"
)

// unsetVariant is a faultFilterInput with the numeric variant bounds at
// their "unset" sentinel (-1), matching what the add-command flag
// defaults produce. Tests start from this and set only the fields under
// test.
func unsetVariant() faultFilterInput {
	return faultFilterInput{bandwidthAbove: -1, bandwidthBelow: -1}
}

func TestBuildFilter_EmptyIsNil(t *testing.T) {
	f, err := buildFilter(unsetVariant())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f != nil {
		t.Errorf("no flags → filter should be nil, got %+v", f)
	}
}

func TestBuildFilter_RequestKindOnly_NoVariant(t *testing.T) {
	in := unsetVariant()
	in.kindCSV = "segment,partial"
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f == nil || f.RequestKind == nil {
		t.Fatalf("request_kind not set: %+v", f)
	}
	if got := *f.RequestKind; len(got) != 2 || string(got[0]) != "segment" || string(got[1]) != "partial" {
		t.Errorf("request_kind = %v, want [segment partial]", got)
	}
	// A kind-only filter must NOT synthesize an empty variant predicate —
	// the server rejects `variant: {}`.
	if f.Variant != nil {
		t.Errorf("kind-only filter must leave Variant nil, got %+v", f.Variant)
	}
}

func TestBuildFilter_InvalidKind(t *testing.T) {
	in := unsetVariant()
	in.kindCSV = "segment,bogus"
	if _, err := buildFilter(in); err == nil {
		t.Error("expected error on invalid --kind, got nil")
	}
}

func TestBuildFilter_URLModesMutuallyExclusive(t *testing.T) {
	in := unsetVariant()
	in.urlSubstr = "2160p"
	in.urlRegex = `\d+p`
	if _, err := buildFilter(in); err == nil {
		t.Error("--url-substr + --url-regex should be rejected")
	}
}

func TestBuildFilter_URLSubstrMultiPattern(t *testing.T) {
	in := unsetVariant()
	in.urlSubstr = "2160p, 1440p ,1080p"
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.UrlMatch == nil || f.UrlMatch.Mode != proxy.Substring {
		t.Fatalf("url_match not substring: %+v", f.UrlMatch)
	}
	if got := f.UrlMatch.Patterns; len(got) != 3 || got[0] != "2160p" || got[1] != "1440p" || got[2] != "1080p" {
		t.Errorf("patterns = %v, want [2160p 1440p 1080p] (trimmed)", got)
	}
}

func TestBuildVariant_Resolutions(t *testing.T) {
	in := unsetVariant()
	in.resolutions = "3840x2160,2560x1440"
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Variant == nil || f.Variant.Resolutions == nil {
		t.Fatalf("resolutions not set: %+v", f.Variant)
	}
	if got := *f.Variant.Resolutions; len(got) != 2 || got[0] != "3840x2160" || got[1] != "2560x1440" {
		t.Errorf("resolutions = %v", got)
	}
}

func TestBuildVariant_RungIndexes(t *testing.T) {
	in := unsetVariant()
	in.rungIndexes = "0,4,5"
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Variant == nil || f.Variant.RungIndexes == nil {
		t.Fatalf("rung_indexes not set: %+v", f.Variant)
	}
	if got := *f.Variant.RungIndexes; len(got) != 3 || got[0] != 0 || got[1] != 4 || got[2] != 5 {
		t.Errorf("rung_indexes = %v, want [0 4 5]", got)
	}
}

func TestBuildVariant_RungIndexNonInt(t *testing.T) {
	in := unsetVariant()
	in.rungIndexes = "0,top"
	if _, err := buildFilter(in); err == nil {
		t.Error("non-integer --rung-index should error")
	}
}

func TestBuildVariant_RungPositions(t *testing.T) {
	in := unsetVariant()
	in.rungPositions = "top,bottom"
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Variant == nil || f.Variant.RungPositions == nil {
		t.Fatalf("rung_positions not set: %+v", f.Variant)
	}
	got := *f.Variant.RungPositions
	if len(got) != 2 || got[0] != proxy.Top || got[1] != proxy.Bottom {
		t.Errorf("rung_positions = %v, want [top bottom]", got)
	}
}

func TestBuildVariant_RungPositionInvalid(t *testing.T) {
	in := unsetVariant()
	in.rungPositions = "top,middle"
	if _, err := buildFilter(in); err == nil {
		t.Error("invalid --rung-position should error")
	}
}

func TestBuildVariant_BandwidthBounds(t *testing.T) {
	in := unsetVariant()
	in.bandwidthAbove = 5_000_000
	in.bandwidthBelow = 20_000_000
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Variant == nil || f.Variant.BandwidthAbove == nil || f.Variant.BandwidthBelow == nil {
		t.Fatalf("bandwidth bounds not set: %+v", f.Variant)
	}
	if *f.Variant.BandwidthAbove != 5_000_000 || *f.Variant.BandwidthBelow != 20_000_000 {
		t.Errorf("bandwidth = (%d,%d)", *f.Variant.BandwidthAbove, *f.Variant.BandwidthBelow)
	}
}

func TestBuildVariant_CodecPrefix(t *testing.T) {
	in := unsetVariant()
	in.codecPrefix = "avc1."
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Variant == nil || f.Variant.CodecPrefix == nil || *f.Variant.CodecPrefix != "avc1." {
		t.Fatalf("codec_prefix not set: %+v", f.Variant)
	}
}

// A variant predicate AND-combines its sub-predicates. Multiple flags
// populate one VariantPredicate, not several.
func TestBuildVariant_CombinedANDs(t *testing.T) {
	in := unsetVariant()
	in.resolutions = "3840x2160"
	in.bandwidthAbove = 10_000_000
	f, err := buildFilter(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Variant == nil || f.Variant.Resolutions == nil || f.Variant.BandwidthAbove == nil {
		t.Errorf("combined predicate incomplete: %+v", f.Variant)
	}
}

// The unset sentinel (-1) must never leak into the wire as bandwidth_above:
// with no variant flags set, Variant stays nil.
func TestBuildVariant_UnsetSentinelStaysNil(t *testing.T) {
	f, err := buildFilter(faultFilterInput{kindCSV: "segment", bandwidthAbove: -1, bandwidthBelow: -1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Variant != nil {
		t.Errorf("no variant flags → Variant must be nil, got %+v", f.Variant)
	}
}

func TestParseCSVInts(t *testing.T) {
	got, err := parseCSVInts(" 0, 4 ,5,")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 || got[0] != 0 || got[1] != 4 || got[2] != 5 {
		t.Errorf("parseCSVInts = %v, want [0 4 5]", got)
	}
	if _, err := parseCSVInts("1,x"); err == nil {
		t.Error("non-integer entry should error")
	}
}

func TestVariantSummary(t *testing.T) {
	res := []string{"3840x2160"}
	idx := []int{0, 4}
	pos := []proxy.VariantPredicateRungPositions{proxy.Top}
	above := 5_000_000
	v := &proxy.VariantPredicate{
		Resolutions:    &res,
		RungIndexes:    &idx,
		RungPositions:  &pos,
		BandwidthAbove: &above,
	}
	got := variantSummary(v)
	want := "res=3840x2160,rung=0|4,pos=top,bw>5000000"
	if got != want {
		t.Errorf("variantSummary = %q, want %q", got, want)
	}
}
