package charmatrix

import (
	"encoding/json"
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

// recipeKey is the comparable subset of an Experiment: the knobs BOTH models
// carry. It excludes the fields the bridge legitimately re-stamps (ID, Group,
// Arm/Role, CreatedAt, Score, Kind, LaunchMode) so the round-trip is judged on
// recipe losslessness, not bookkeeping.
func recipeKey(t *testing.T, e *sweep.Experiment) string {
	t.Helper()
	norm := struct {
		Class     sweep.Class
		Platform  string
		Protocol  string
		Content   string
		Segment   string
		Mode      string
		DurationS int
		Reps      int
		Muted     *bool
		Shape     *sweep.Shape
		Fault     *sweep.Fault
		CM        *sweep.ContentManipulation
		Xfer      *sweep.TransferTimeouts
	}{
		e.ClassOrDefault(), e.Platform, e.Protocol, e.Content, e.Segment, e.Mode,
		e.DurationS, e.Reps, e.Muted, e.Shape, e.Fault, e.ContentManipulation, e.TransferTimeouts,
	}
	b, err := json.Marshal(norm)
	if err != nil {
		t.Fatalf("marshal recipe key: %v", err)
	}
	return string(b)
}

func expsFromYAML(t *testing.T, src []byte) []*sweep.Experiment {
	t.Helper()
	spec, err := Load(src)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	out := make([]*sweep.Experiment, len(arms))
	for i, a := range arms {
		out[i] = a.ToExperiment()
	}
	return out
}

func keyMultiset(t *testing.T, exps []*sweep.Experiment) map[string]int {
	t.Helper()
	m := map[string]int{}
	for _, e := range exps {
		m[recipeKey(t, e)]++
	}
	return m
}

// TestBridgeRoundTripLossless: YAML → Experiments → Spec → YAML → Experiments
// preserves every shared recipe knob (shape pattern, fault, content-manipulation,
// segment, transfer-timeouts), across the group-pairing form.
func TestBridgeRoundTripLossless(t *testing.T) {
	src := []byte(`
name: bridge-rt
class: config
defaults:
  platform: ipad-sim
  content: clip_x
  mode: pyramid
  is.segment: s2
  proxy.shape:
    pattern: pyramid
    step_seconds: 12
    rate_mbps: 1.5
groups:
  - id: g-shape
    control: {}
    variants:
      - proxy.shape:
          pattern: valley
          step_seconds: 6
      - proxy.content_manipulation:
          strip_avg_bandwidth: true
      - proxy.transfer_timeouts:
          active_seconds: 10
          applies_segments: true
`)
	first := expsFromYAML(t, src)
	if len(first) != 4 { // control + 3 variants
		t.Fatalf("want 4 arms from the group, got %d", len(first))
	}

	spec, err := SpecFromExperiments("bridge-rt", first)
	if err != nil {
		t.Fatalf("SpecFromExperiments: %v", err)
	}
	// The control+variants pairing survives as a Group, not scattered arms.
	if len(spec.Groups) != 1 || len(spec.Arms) != 0 {
		t.Fatalf("want 1 group / 0 flat arms, got %d groups / %d arms", len(spec.Groups), len(spec.Arms))
	}
	if len(spec.Groups[0].Variants) != 3 || spec.Groups[0].Control == nil {
		t.Fatalf("group must keep control + 3 variants, got control=%v variants=%d",
			spec.Groups[0].Control != nil, len(spec.Groups[0].Variants))
	}

	out, err := Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second := expsFromYAML(t, out)

	if a, b := keyMultiset(t, first), keyMultiset(t, second); !mapsEqual(a, b) {
		t.Fatalf("round-trip lost a recipe knob.\n exported yaml:\n%s\n first=%v\n second=%v", out, a, b)
	}
}

// TestBridgeMarshalDeterministic: the same spec marshals byte-identically twice
// (golden tests depend on this — yaml.v3 sorts map keys).
func TestBridgeMarshalDeterministic(t *testing.T) {
	src := []byte("name: det\nclass: config\ndefaults:\n  platform: ipad-sim\n  mode: steps\naxes:\n  is.segment: [s2, s6]\n")
	exps := expsFromYAML(t, src)
	spec, err := SpecFromExperiments("det", exps)
	if err != nil {
		t.Fatalf("spec: %v", err)
	}
	a, err := Marshal(spec)
	if err != nil {
		t.Fatalf("marshal a: %v", err)
	}
	b, err := Marshal(spec)
	if err != nil {
		t.Fatalf("marshal b: %v", err)
	}
	if string(a) != string(b) {
		t.Fatalf("marshal not deterministic:\n--- a ---\n%s\n--- b ---\n%s", a, b)
	}
}

// TestDroppedClientKnobsGuard: client-only is.* knobs that ToExperiment can't
// carry are reported (so import refuses), while carried knobs are not.
func TestDroppedClientKnobsGuard(t *testing.T) {
	peak := 3
	off := 5.0
	yes := true
	a := &Arm{
		Codec:              "hevc",
		PeakBitrateMbps:    peak,
		AppLiveOffset:      &off,
		StartsFirstVariant: &yes,
		// carried knobs — must NOT be flagged:
		Segment: "s2", Muted: &yes, Shape: &sweep.Shape{Pattern: "valley"},
	}
	got := a.DroppedClientKnobs()
	want := map[string]bool{"is.codec": true, "is.peak_bitrate_mbps": true, "is.live_offset": true, "is.starts_first_variant": true}
	if len(got) != len(want) {
		t.Fatalf("want %d dropped knobs, got %v", len(want), got)
	}
	for _, k := range got {
		if !want[k] {
			t.Fatalf("unexpected dropped knob %q (carried knobs must not be flagged)", k)
		}
	}
	// An arm with only carried knobs drops nothing.
	clean := &Arm{Segment: "s6", Muted: &yes, Protocol: "hls"}
	if d := clean.DroppedClientKnobs(); len(d) != 0 {
		t.Fatalf("clean arm should drop nothing, got %v", d)
	}
}

func mapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
