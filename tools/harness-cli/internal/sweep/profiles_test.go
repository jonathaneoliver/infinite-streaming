package sweep

import (
	"encoding/json"
	"testing"
)

func f(v float64) *float64 { return &v }

// TestResolveLinkProfile locks the headline values for the four hand-tuned
// recipes + a couple of NLC presets so a retune is a deliberate, reviewed edit.
func TestResolveLinkProfile(t *testing.T) {
	cases := []struct {
		name string
		want Shape
	}{
		{"clean", Shape{}},
		{"home", Shape{DelayMs: f(20), LossPct: f(0.2), JitterMs: f(5), LossCorrelationPct: f(25), JitterCorrelationPct: f(25)}},
		{"mobile-poor", Shape{DelayMs: f(150), LossPct: f(3), JitterMs: f(80), LossCorrelationPct: f(50), JitterCorrelationPct: f(25)}},
		{"nlc-lte", Shape{RateMbps: f(50), DelayMs: f(65)}},
		{"nlc-100-loss", Shape{LossPct: f(100)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ResolveLinkProfile(tc.name)
			if !ok {
				t.Fatalf("profile %q not found", tc.name)
			}
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(tc.want)
			if string(gb) != string(wb) {
				t.Errorf("profile %q\n  got:  %s\n  want: %s", tc.name, gb, wb)
			}
		})
	}
}

// TestRecipeOverlaysHaveNoRate proves the four hand-tuned recipes are pure
// impairment overlays (no rate cap), so they layer on an existing bandwidth
// spec without clobbering it.
func TestRecipeOverlaysHaveNoRate(t *testing.T) {
	for _, name := range []string{"clean", "home", "mobile-good", "mobile-poor"} {
		s, ok := ResolveLinkProfile(name)
		if !ok {
			t.Fatalf("profile %q not found", name)
		}
		if s.RateMbps != nil {
			t.Errorf("recipe %q should not set rate_mbps, got %v", name, *s.RateMbps)
		}
	}
}

// TestDnsProfileSkipped proves the un-expressible DNS profile is absent.
func TestDnsProfileSkipped(t *testing.T) {
	if _, ok := ResolveLinkProfile("nlc-high-latency-dns"); ok {
		t.Fatal("nlc-high-latency-dns should NOT be a netem profile (DNS-only impairment)")
	}
}

// TestShapeUnmarshalProfileString proves proxy.shape can be a profile name.
func TestShapeUnmarshalProfileString(t *testing.T) {
	var s Shape
	if err := json.Unmarshal([]byte(`"mobile-poor"`), &s); err != nil {
		t.Fatalf("unmarshal profile string: %v", err)
	}
	if s.DelayMs == nil || *s.DelayMs != 150 {
		t.Errorf("delay_ms = %v, want 150", s.DelayMs)
	}

	// Object form still decodes.
	var o Shape
	if err := json.Unmarshal([]byte(`{"delay_ms":40,"loss_pct":1}`), &o); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if o.DelayMs == nil || *o.DelayMs != 40 {
		t.Errorf("object delay_ms = %v, want 40", o.DelayMs)
	}

	// Unknown profile errors.
	if err := json.Unmarshal([]byte(`"nope"`), &Shape{}); err == nil {
		t.Error("expected error for unknown profile name")
	}

	// Typo'd knob in object form is rejected (strict).
	if err := json.Unmarshal([]byte(`{"dly_ms":40}`), &Shape{}); err == nil {
		t.Error("expected strict-decode error for typo'd knob")
	}
}
