package v2translate

import "testing"

// A single-owner group SLAVE carries only the driven markers — no local
// rate / delay / loss / fault / pattern of its own (the master drives its
// kernel cap via the fan-out). shapeFromSession must still build a Shape so
// the "driven by master" badge + the runtime cap reach the v2 player record
// the shaping panel reads.
//
// Regression for the #849 follow-up: the early nil-guard fired on a driven
// slave (rate==0 && … && pattern==nil), dropping the whole shape — so the
// panel showed an empty 0-Mbps session even while the cap was being applied.
func TestShapeFromSession_DrivenSlaveSurfacesMarkers(t *testing.T) {
	s := map[string]any{
		"nftables_pattern_driven_by":         "19f66fd3",
		"nftables_pattern_driven_template":   "valley",
		"nftables_pattern_rate_runtime_mbps": 12.5,
	}
	shape := shapeFromSession(s)
	if shape == nil {
		t.Fatal("driven slave: shapeFromSession returned nil — driven markers dropped")
	}
	if shape.GroupDrivenBy == nil || *shape.GroupDrivenBy != "19f66fd3" {
		t.Errorf("GroupDrivenBy = %v, want 19f66fd3", shape.GroupDrivenBy)
	}
	if shape.GroupDrivenTemplate == nil || *shape.GroupDrivenTemplate != "valley" {
		t.Errorf("GroupDrivenTemplate = %v, want valley", shape.GroupDrivenTemplate)
	}
	if shape.PatternRateRuntimeMbps == nil || *shape.PatternRateRuntimeMbps != 12.5 {
		t.Errorf("PatternRateRuntimeMbps = %v, want 12.5", shape.PatternRateRuntimeMbps)
	}
}

// A session with no shaping at all still returns nil — the early-out we keep
// so unshaped sessions don't carry an empty Shape object.
func TestShapeFromSession_EmptyReturnsNil(t *testing.T) {
	if shape := shapeFromSession(map[string]any{}); shape != nil {
		t.Errorf("empty session: shapeFromSession = %+v, want nil", shape)
	}
}
