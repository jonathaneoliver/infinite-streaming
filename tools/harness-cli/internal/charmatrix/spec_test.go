package charmatrix

import (
	"strings"
	"testing"
)

func TestLoad_AcceptanceMatrix(t *testing.T) {
	// The #811 acceptance matrix: segment:[s6] × live_offset:[24,30] ×
	// lever:[proxy,app], platform pinned via defaults → 4 arms.
	yaml := `
name: live-offset-acceptance
class: config
duration_s: 90
defaults:
  platform: ipad-sim
  content: insane_new_p200_h264
axes:
  segment: [s6]
  live_offset: [24, 30]
  lever: [proxy, app]
`
	spec, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if spec.Name != "live-offset-acceptance" {
		t.Errorf("name = %q", spec.Name)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 4 {
		t.Fatalf("got %d arms, want 4", len(arms))
	}
	for _, a := range arms {
		if a.Platform != "ipad-sim" || a.Content != "insane_new_p200_h264" {
			t.Errorf("arm %s missing defaults: %+v", a.ID, a)
		}
		if a.Segment != "s6" {
			t.Errorf("arm %s segment = %q, want s6", a.ID, a.Segment)
		}
		e := a.ToExperiment()
		if e.DurationS != 90 {
			t.Errorf("arm %s duration_s = %d, want 90", a.ID, e.DurationS)
		}
	}
}

func TestLoad_NestedRecipeBlocks(t *testing.T) {
	// The reused sweep types' json tags must drive the nested YAML blocks with no
	// dual-tagging: shape.rate_mbps, content_manipulation.strip_average_bandwidth,
	// transfer_timeouts.active_seconds, fault.type.
	yaml := `
name: recipe
arms:
  - platform: ipad-sim
    shape:
      rate_mbps: 4.5
    content_manipulation:
      strip_avg_bandwidth: true
    transfer_timeouts:
      active_seconds: 8
      applies_segments: true
  - platform: appletv
    class: fault
    fault:
      type: "500"
      request_kind: segment
      frequency: 3
`
	spec, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	a0 := arms[0]
	if a0.Shape == nil || a0.Shape.RateMbps == nil || *a0.Shape.RateMbps != 4.5 {
		t.Errorf("shape.rate_mbps not decoded: %+v", a0.Shape)
	}
	if a0.ContentManipulation == nil || !a0.ContentManipulation.StripAvgBandwidth {
		t.Errorf("content_manipulation.strip_average_bandwidth not decoded")
	}
	if a0.TransferTimeouts == nil || a0.TransferTimeouts.ActiveSeconds != 8 || !a0.TransferTimeouts.AppliesSegments {
		t.Errorf("transfer_timeouts not decoded: %+v", a0.TransferTimeouts)
	}
	// The recipe blocks must survive ToExperiment for the server config path.
	e0 := a0.ToExperiment()
	if e0.Shape == nil || e0.Shape.RateMbps == nil {
		t.Error("ToExperiment dropped shape")
	}
	a1 := arms[1]
	if a1.Fault == nil || a1.Fault.Type != "500" || a1.Fault.RequestKind != "segment" || a1.Fault.Frequency != 3 {
		t.Errorf("fault block not decoded: %+v", a1.Fault)
	}
	if a1.ToExperiment().Class != "fault" {
		t.Error("arm 1 class should be fault")
	}
}

func TestLoad_UnknownFieldRejected(t *testing.T) {
	// DisallowUnknownFields catches a typo'd key instead of silently ignoring it.
	yaml := `
name: typo
arms:
  - platfrom: ipad-sim
`
	if _, err := Load([]byte(yaml)); err == nil || !strings.Contains(err.Error(), "decode spec") {
		t.Fatalf("expected decode error for unknown field, got %v", err)
	}
}

func TestLoad_MissingNameRejected(t *testing.T) {
	if _, err := Load([]byte("axes:\n  segment: [s6]\n")); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name-required error, got %v", err)
	}
}

func TestLoad_BadYAML(t *testing.T) {
	if _, err := Load([]byte("name: [unterminated")); err == nil {
		t.Fatal("expected parse error for malformed yaml")
	}
}
