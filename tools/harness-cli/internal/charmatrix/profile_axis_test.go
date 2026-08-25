package charmatrix

import (
	"strings"
	"testing"
)

// TestProfileAxisExpands proves the #826 named-profile form: an object-valued
// proxy.shape axis can carry profile NAMES (strings), each resolving to the
// full Shape, with a clean profile-name id slug (not a hash).
func TestProfileAxisExpands(t *testing.T) {
	yaml := []byte(`
name: impairment-overlay
axes:
  proxy.shape: [clean, home, mobile-poor]
`)
	spec, err := Load(yaml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 3 {
		t.Fatalf("got %d arms, want 3", len(arms))
	}

	// clean → all-zero Shape (or nil impairment).
	clean := findArmBySlug(t, arms, "clean")
	if clean.Shape == nil {
		t.Fatalf("clean arm has nil Shape")
	}
	if clean.Shape.DelayMs != nil || clean.Shape.LossPct != nil {
		t.Errorf("clean profile should be unimpaired, got delay=%v loss=%v", clean.Shape.DelayMs, clean.Shape.LossPct)
	}

	// mobile-poor → delay 150 / loss 3 / jitter 80 + correlations.
	poor := findArmBySlug(t, arms, "mobile-poor")
	if poor.Shape == nil || poor.Shape.DelayMs == nil || *poor.Shape.DelayMs != 150 {
		t.Fatalf("mobile-poor delay_ms = %v, want 150", shapeField(poor))
	}
	if poor.Shape.LossPct == nil || *poor.Shape.LossPct != 3 {
		t.Errorf("mobile-poor loss_pct = %v, want 3", poor.Shape.LossPct)
	}
	if poor.Shape.JitterMs == nil || *poor.Shape.JitterMs != 80 {
		t.Errorf("mobile-poor jitter_ms = %v, want 80", poor.Shape.JitterMs)
	}
	if poor.Shape.LossCorrelationPct == nil || *poor.Shape.LossCorrelationPct != 50 {
		t.Errorf("mobile-poor loss_correlation_pct = %v, want 50", poor.Shape.LossCorrelationPct)
	}
	// Pure-impairment overlay: no rate cap.
	if poor.Shape.RateMbps != nil {
		t.Errorf("mobile-poor should not set rate_mbps, got %v", *poor.Shape.RateMbps)
	}
}

// TestProfileAxisUnknownNameFails proves a typo'd profile fails fast (at Expand,
// where axis values are decoded into Shapes).
func TestProfileAxisUnknownNameFails(t *testing.T) {
	yaml := []byte(`
name: bad
axes:
  proxy.shape: [home, mobil-poor]
`)
	spec, err := Load(yaml)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Expand(spec); err == nil {
		t.Fatal("expected error for unknown profile 'mobil-poor', got nil")
	}
}

// TestObjectShapeStillStrict proves the object form keeps rejecting typo'd knobs.
func TestObjectShapeStillStrict(t *testing.T) {
	yaml := []byte(`
name: typo
defaults:
  proxy.shape: { delay_ms: 20, lss_pct: 1 }
`)
	if _, err := Load(yaml); err == nil {
		t.Fatal("expected error for typo'd shape knob 'lss_pct', got nil")
	}
}

func findArmBySlug(t *testing.T, arms []*Arm, slug string) *Arm {
	t.Helper()
	for _, a := range arms {
		if a.ID == slug || strings.HasSuffix(a.ID, "-"+slug) {
			return a
		}
	}
	t.Fatalf("no arm with id slug %q (ids: %v)", slug, armIDs(arms))
	return nil
}

func armIDs(arms []*Arm) []string {
	out := make([]string, len(arms))
	for i, a := range arms {
		out[i] = a.ID
	}
	return out
}

func shapeField(a *Arm) any {
	if a.Shape == nil || a.Shape.DelayMs == nil {
		return nil
	}
	return *a.Shape.DelayMs
}
