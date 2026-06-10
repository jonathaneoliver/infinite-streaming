package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportFinalizeAndWrite(t *testing.T) {
	start := time.Now().Add(-30 * time.Second)
	r := &Report{
		Mode:      "smooth",
		Platform:  PlatformIPadSim,
		Device:    Device{Platform: PlatformIPadSim, UDID: "ABCDEF", Label: "iPad sim"},
		PlayerID:  "00000000-0000-0000-0000-000000000001",
		StartedAt: start,
		Samples: []Sample{
			{Ts: start, AppliedRateMbps: 5.0, BufferDepthS: 8.5, Stalls: 0, StallTimeS: 0, VideoBitrateMbps: 4.9, ProfileShiftCount: 0, FramesDropped: 0},
			{Ts: start.Add(10 * time.Second), AppliedRateMbps: 3.0, BufferDepthS: 6.0, Stalls: 0, StallTimeS: 0, VideoBitrateMbps: 2.4, ProfileShiftCount: 1, FramesDropped: 0},
			{Ts: start.Add(20 * time.Second), AppliedRateMbps: 1.5, BufferDepthS: 4.0, Stalls: 1, StallTimeS: 2.3, VideoBitrateMbps: 1.1, ProfileShiftCount: 2, FramesDropped: 5},
		},
		Steps: []Step{
			{StartedAt: start, RateMbps: 5.0, Hold: 10 * time.Second},
			{StartedAt: start.Add(10 * time.Second), RateMbps: 3.0, Hold: 10 * time.Second},
			{StartedAt: start.Add(20 * time.Second), RateMbps: 1.5, Hold: 10 * time.Second},
		},
	}
	r.Finalize(start.Add(30 * time.Second))

	if r.Summary.SampleCount != 3 {
		t.Errorf("SampleCount=%d want 3", r.Summary.SampleCount)
	}
	if r.Summary.TotalStalls != 1 {
		t.Errorf("TotalStalls=%d want 1", r.Summary.TotalStalls)
	}
	if r.Summary.ProfileShifts != 2 {
		t.Errorf("ProfileShifts=%d want 2", r.Summary.ProfileShifts)
	}
	if r.Summary.MaxBufferDepthS != 8.5 {
		t.Errorf("MaxBufferDepthS=%.2f want 8.5", r.Summary.MaxBufferDepthS)
	}
	if r.Summary.MinBufferDepthS != 4.0 {
		t.Errorf("MinBufferDepthS=%.2f want 4.0", r.Summary.MinBufferDepthS)
	}
	if r.Summary.MinBitrateMbps != 1.1 {
		t.Errorf("MinBitrateMbps=%.2f want 1.1", r.Summary.MinBitrateMbps)
	}
	if r.Summary.MaxBitrateMbps != 4.9 {
		t.Errorf("MaxBitrateMbps=%.2f want 4.9", r.Summary.MaxBitrateMbps)
	}

	out := t.TempDir()
	jsonPath, err := WriteReport(out, "smooth-test", r)
	if err != nil {
		t.Fatalf("WriteReport: %v", err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("json missing: %v", err)
	}
	mdPath := filepath.Join(out, "smooth-test.md")
	mdRaw, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("md missing: %v", err)
	}
	md := string(mdRaw)
	for _, needle := range []string{
		"# smooth — ipad-sim/iPad sim",
		"00000000-0000-0000-0000-000000000001",
		"| stalls               | 1 |",
		"| profile shifts       | 2 |",
	} {
		if !strings.Contains(md, needle) {
			t.Errorf("md missing %q\n---\n%s", needle, md)
		}
	}

	// JSON round-trips back into an equivalent Report.
	var round Report
	raw, _ := os.ReadFile(jsonPath)
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.Mode != r.Mode || round.Summary.TotalStalls != r.Summary.TotalStalls {
		t.Errorf("round-trip mismatch: %+v vs %+v", round.Summary, r.Summary)
	}
}

// TestReportSustainableCapOrderIndependent proves LowestSustainableCapMbps
// (min over sustainable steps) and HighestStallingCapMbps (max over failing
// steps) are computed independent of the sweep direction. The pre-#676 code
// used last-writer-wins in iteration order, so an ascending (rampup) sweep
// reported the ladder TOP as the sustainable floor.
func TestReportSustainableCapOrderIndependent(t *testing.T) {
	// A sustainable step clears the buffer threshold with zero stalls; a
	// failing step stalls. Caps: 2.0 fails, 4.0/6.0 sustain. The expected
	// answers are fixed regardless of the order we list the steps in.
	sustain := func(rate float64) Step {
		return Step{RateMbps: rate, StallsDelta: 0, MinBufferS: SustainableBufferS + 1.0, SampleCount: 10}
	}
	fail := func(rate float64) Step {
		return Step{RateMbps: rate, StallsDelta: 1, MinBufferS: 0.2, SampleCount: 10}
	}

	cases := []struct {
		name  string
		steps []Step
	}{
		{"descending", []Step{sustain(6.0), sustain(4.0), fail(2.0)}},
		{"ascending", []Step{fail(2.0), sustain(4.0), sustain(6.0)}},
		{"pyramid", []Step{sustain(4.0), sustain(6.0), sustain(4.0), fail(2.0)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			r := &Report{
				StartedAt: start,
				Samples:   []Sample{{Ts: start, BufferDepthS: 5.0, VideoBitrateMbps: 4.0}},
				Steps:     tc.steps,
			}
			r.Finalize(start.Add(time.Second))
			if r.Summary.LowestSustainableCapMbps != 4.0 {
				t.Errorf("LowestSustainableCapMbps=%.2f want 4.0 (the floor, not the top)", r.Summary.LowestSustainableCapMbps)
			}
			if r.Summary.HighestStallingCapMbps != 2.0 {
				t.Errorf("HighestStallingCapMbps=%.2f want 2.0", r.Summary.HighestStallingCapMbps)
			}
		})
	}
}

// TestReportBottomVariantFloor confirms the variant-keyed floor block is
// untouched by the #676 min/max refactor — it keys on resolution, not order.
func TestReportBottomVariantFloor(t *testing.T) {
	start := time.Now()
	bottom := &VariantRate{Resolution: "320x180"}
	r := &Report{
		StartedAt: start,
		Samples:   []Sample{{Ts: start, BufferDepthS: 5.0, VideoBitrateMbps: 1.0}},
		Variants:  []VariantRate{{Resolution: "1920x1080"}, {Resolution: "320x180"}},
		Steps: []Step{
			// Two failing steps target the bottom rung; the higher cap wins.
			{RateMbps: 1.5, StallsDelta: 1, MinBufferS: 0.1, SampleCount: 10, Variant: bottom},
			{RateMbps: 0.8, StallsDelta: 1, MinBufferS: 0.1, SampleCount: 10, Variant: bottom},
		},
	}
	r.Finalize(start.Add(time.Second))
	if r.Summary.BottomVariantFloorMbps != 1.5 {
		t.Errorf("BottomVariantFloorMbps=%.2f want 1.5", r.Summary.BottomVariantFloorMbps)
	}
}

func TestDefaultOutDir(t *testing.T) {
	// Env var set → always wins, regardless of fallback.
	t.Setenv("CHARACTERIZATION_OUTDIR", "/var/reports")
	if got := DefaultOutDir("/tmp/fallback"); got != "/var/reports" {
		t.Errorf("DefaultOutDir(set)=%q want /var/reports", got)
	}

	// Env var unset → resolves to ./artifacts under whatever directory
	// the test happens to be running from (cwd-relative). This is the
	// "no env var needed" recipe.
	t.Setenv("CHARACTERIZATION_OUTDIR", "")
	tmp := t.TempDir()
	prev, _ := os.Getwd()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir tmp: %v", err)
	}
	defer os.Chdir(prev)
	if got := DefaultOutDir("/tmp/fallback"); got != "./artifacts" {
		t.Errorf("DefaultOutDir(unset)=%q want ./artifacts", got)
	}
}
