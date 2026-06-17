package charmatrix

import (
	"strings"
	"testing"
)

func TestLoad_AcceptanceMatrix(t *testing.T) {
	// A namespaced matrix: is.segment:[s6] × proxy.live_offset:[24,30] ×
	// is.protocol:[hls,dash], platform pinned via defaults → 4 arms.
	yaml := `
name: live-offset-acceptance
class: config
duration_s: 90
defaults:
  platform: ipad-sim
  content: insane_new_p200_h264
axes:
  is.segment: [s6]
  proxy.live_offset: [24, 30]
  is.protocol: [hls, dash]
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
	// The reused sweep types' json tags must drive the nested YAML blocks under the
	// proxy.* namespace with no dual-tagging: proxy.shape.rate_mbps,
	// proxy.content_manipulation.strip_avg_bandwidth, proxy.transfer_timeouts.*,
	// proxy.fault.type.
	yaml := `
name: recipe
arms:
  - platform: ipad-sim
    proxy.shape:
      rate_mbps: 4.5
    proxy.content_manipulation:
      strip_avg_bandwidth: true
    proxy.transfer_timeouts:
      active_seconds: 8
      applies_segments: true
  - platform: appletv
    class: fault
    proxy.fault:
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
		t.Errorf("proxy.shape.rate_mbps not decoded: %+v", a0.Shape)
	}
	if a0.ContentManipulation == nil || !a0.ContentManipulation.StripAvgBandwidth {
		t.Errorf("proxy.content_manipulation.strip_avg_bandwidth not decoded")
	}
	if a0.TransferTimeouts == nil || a0.TransferTimeouts.ActiveSeconds != 8 || !a0.TransferTimeouts.AppliesSegments {
		t.Errorf("proxy.transfer_timeouts not decoded: %+v", a0.TransferTimeouts)
	}
	// The recipe blocks must survive ToExperiment for the server config path.
	e0 := a0.ToExperiment()
	if e0.Shape == nil || e0.Shape.RateMbps == nil {
		t.Error("ToExperiment dropped shape")
	}
	a1 := arms[1]
	if a1.Fault == nil || a1.Fault.Type != "500" || a1.Fault.RequestKind != "segment" || a1.Fault.Frequency != 3 {
		t.Errorf("proxy.fault block not decoded: %+v", a1.Fault)
	}
	if a1.ToExperiment().Class != "fault" {
		t.Error("arm 1 class should be fault")
	}
}

func TestLoad_GroupsBlock(t *testing.T) {
	// The groups: control+variants form decodes and Expand pre-pairs it.
	yaml := `
name: manip
class: config
parallel: true
defaults:
  platform: ipad-sim
  content: c
groups:
  - id: avgbw
    control: {}
    variants:
      - proxy.content_manipulation: { strip_avg_bandwidth: true }
`
	spec, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 2 {
		t.Fatalf("got %d arms, want 2", len(arms))
	}
	if arms[0].Role != "control" || arms[1].Role != "variant" {
		t.Errorf("roles: %q, %q", arms[0].Role, arms[1].Role)
	}
	if arms[0].Group != "manip/avgbw" {
		t.Errorf("group = %q, want manip/avgbw", arms[0].Group)
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
	if _, err := Load([]byte("axes:\n  is.segment: [s6]\n")); err == nil || !strings.Contains(err.Error(), "name") {
		t.Fatalf("expected name-required error, got %v", err)
	}
}

func TestLoad_BadYAML(t *testing.T) {
	if _, err := Load([]byte("name: [unterminated")); err == nil {
		t.Fatal("expected parse error for malformed yaml")
	}
}
