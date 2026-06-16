package ladder

import (
	"math"
	"testing"
)

// tearsH264 is the real published ladder of tears-of-steel-4k_p200_h264
// as served by test-dev (dev.jeoliver.com:21000/go-live/.../master.m3u8).
// It is the golden fixture all three runtimes' ladders must reproduce.
var tearsH264 = []Variant{
	{AvgBps: 793601, PeakBps: 1033456, Resolution: "640x360"},
	{AvgBps: 1387608, PeakBps: 1852154, Resolution: "960x540"},
	{AvgBps: 2579294, PeakBps: 3523216, Resolution: "1280x720"},
	{AvgBps: 5147499, PeakBps: 6841582, Resolution: "1920x1080"},
	{AvgBps: 11125902, PeakBps: 14972084, Resolution: "2560x1440"},
	{AvgBps: 21741872, PeakBps: 29584915, Resolution: "3840x2160"},
}

func TestAnchorCaps(t *testing.T) {
	got := AnchorCaps(tearsH264, DefaultBumpPct)
	if len(got) != 12 {
		t.Fatalf("anchors: got %d want 12 (2 per variant)", len(got))
	}
	// Descending, top = 2160p peak, bottom = 360p avg, both ×1.05.
	if got[0].Mbps != 31.064 || got[0].Kind != "peak" {
		t.Errorf("top anchor = %.3f %s, want 31.064 peak", got[0].Mbps, got[0].Kind)
	}
	if last := got[len(got)-1]; last.Mbps != 0.833 || last.Kind != "avg" {
		t.Errorf("bottom anchor = %.3f %s, want 0.833 avg", last.Mbps, last.Kind)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Mbps > got[i-1].Mbps {
			t.Errorf("anchors not descending at %d: %.3f > %.3f", i, got[i].Mbps, got[i-1].Mbps)
		}
	}
}

func TestAnchorCapsNoAverage(t *testing.T) {
	// AVERAGE-BANDWIDTH absent ⇒ one (peak) anchor per variant.
	vs := []Variant{{PeakBps: 2_000_000, Resolution: "a"}, {PeakBps: 6_000_000, Resolution: "b"}}
	if got := AnchorCaps(vs, DefaultBumpPct); len(got) != 2 {
		t.Fatalf("got %d anchors, want 2", len(got))
	}
}

func TestStandardLadderFilled(t *testing.T) {
	got := StandardLadder(tearsH264, DefaultBumpPct, DefaultMaxStep, 0)
	if len(got) != 34 {
		t.Fatalf("filled ladder: got %d rungs want 34 (12 anchors + 22 fills)", len(got))
	}
	if got[0].Mbps != 31.064 || got[len(got)-1].Mbps != 0.833 {
		t.Errorf("ladder bounds = %.3f..%.3f, want 31.064..0.833", got[0].Mbps, got[len(got)-1].Mbps)
	}
	// Every consecutive step within the target ratio (tiny slack for the
	// 3-dp rounding of each cap).
	for i := 1; i < len(got); i++ {
		ratio := got[i-1].Mbps / got[i].Mbps
		if ratio > DefaultMaxStep+0.002 {
			t.Errorf("step %d→%d ratio %.4fx exceeds %.2fx", i-1, i, ratio, DefaultMaxStep)
		}
		if got[i].Mbps > got[i-1].Mbps {
			t.Errorf("ladder not descending at %d", i)
		}
	}
	// No fill escapes the anchor envelope.
	for _, r := range got {
		if r.Mbps > 31.064 || r.Mbps < 0.833 {
			t.Errorf("rung %.3f outside [0.833, 31.064]", r.Mbps)
		}
	}
}

func TestValidateLadderClean(t *testing.T) {
	if hz := ValidateLadder(tearsH264); len(hz) != 0 {
		t.Errorf("deployed ladder should be clean, got %v", hz)
	}
}

func TestValidateLadderHazards(t *testing.T) {
	cases := []struct {
		name string
		vs   []Variant
		kind string
	}{
		{"duplicate", []Variant{{PeakBps: 2_000_000, Resolution: "a"}, {PeakBps: 2_000_000, Resolution: "b"}}, "duplicate_bandwidth"},
		{"tight", []Variant{{PeakBps: 2_000_000, Resolution: "a"}, {PeakBps: 2_400_000, Resolution: "b"}}, "tight_spacing"},
		{"inversion", []Variant{
			{AvgBps: 4_500_000, PeakBps: 6_000_000, Resolution: "A"},
			{AvgBps: 4_000_000, PeakBps: 6_500_000, Resolution: "B"}, // peak↑ but avg↓
		}, "inversion"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hz := ValidateLadder(c.vs)
			if !hasHazard(hz, c.kind) {
				t.Errorf("expected %s hazard, got %v", c.kind, hz)
			}
		})
	}
}

func TestValidateOverlapNotFlagged(t *testing.T) {
	// Deliberate avg→peak band overlap with healthy peak spacing: rung A
	// avg 4 / peak 9, rung B avg 8 / peak 16. B's avg (8) sits inside A's
	// band (4–9) so the bands overlap, but peak spacing is 1.78× and the
	// avg order matches the peak order ⇒ must NOT be flagged.
	vs := []Variant{
		{AvgBps: 4_000_000, PeakBps: 9_000_000, Resolution: "A"},
		{AvgBps: 8_000_000, PeakBps: 16_000_000, Resolution: "B"},
	}
	if hz := ValidateLadder(vs); len(hz) != 0 {
		t.Errorf("overlap must not be flagged, got %v", hz)
	}
}

func TestStandardLadderTopHeadroom(t *testing.T) {
	base := StandardLadder(tearsH264, DefaultBumpPct, DefaultMaxStep, 0)
	got := StandardLadder(tearsH264, DefaultBumpPct, DefaultMaxStep, DefaultTopHeadroomPct)
	// Headroom adds a start rung above the +bump top anchor, plus the
	// geometric fill(s) bridging the 1.50×→1.05× gap.
	if len(got) <= len(base) {
		t.Fatalf("headroom ladder len %d should exceed base %d", len(got), len(base))
	}
	// Top rung = top peak (29.584915 Mbps) × 1.50 = 44.377.
	if got[0].Kind != "headroom" {
		t.Errorf("top rung kind = %q, want headroom", got[0].Kind)
	}
	if got[0].Mbps < 44.36 || got[0].Mbps > 44.39 {
		t.Errorf("headroom cap = %.3f, want ~44.377 (top peak × 1.50)", got[0].Mbps)
	}
	if got[0].Variant != "3840x2160" {
		t.Errorf("headroom rung variant = %q, want 3840x2160", got[0].Variant)
	}
	// Still strictly descending within the step ratio.
	for i := 1; i < len(got); i++ {
		if got[i].Mbps >= got[i-1].Mbps {
			t.Errorf("not descending at %d", i)
		}
		if r := got[i-1].Mbps / got[i].Mbps; r > DefaultMaxStep+0.002 {
			t.Errorf("step %d ratio %.4fx exceeds %.2fx", i, r, DefaultMaxStep)
		}
	}
}

func TestBuildPattern(t *testing.T) {
	rungs := StandardLadder(tearsH264, DefaultBumpPct, DefaultMaxStep, 0)
	n := len(rungs)

	up := BuildPattern(RampUp, rungs, 12)
	if len(up) != n || up[0].RateMbps >= up[len(up)-1].RateMbps {
		t.Errorf("ramp_up: len %d (want %d), first %.3f should be < last %.3f", len(up), n, up[0].RateMbps, up[len(up)-1].RateMbps)
	}
	down := BuildPattern(RampDown, rungs, 12)
	if len(down) != n || down[0].RateMbps <= down[len(down)-1].RateMbps {
		t.Errorf("ramp_down should descend, got %.3f..%.3f", down[0].RateMbps, down[len(down)-1].RateMbps)
	}
	pyr := BuildPattern(Pyramid, rungs, 12)
	if len(pyr) != 2*n-1 {
		t.Errorf("pyramid len %d, want %d (asc + desc without apex)", len(pyr), 2*n-1)
	}
	sq := BuildPattern(SquareWave, rungs, 12)
	if len(sq) != 2 || sq[0].RateMbps != 0.833 || sq[1].RateMbps != 31.064 {
		t.Errorf("square_wave = %v, want [0.833, 31.064]", sq)
	}
	// transient_shock: top + (n-1) deepening dips, each recovering to top →
	// length 2n-1. Even indices are the top baseline; odd indices are the
	// dips, deepening from the second-highest rung down to the bottom.
	ts := BuildPattern(TransientShock, rungs, 12)
	if len(ts) != 2*n-1 {
		t.Errorf("transient_shock len %d, want %d (top + n-1 recover-to-top dips)", len(ts), 2*n-1)
	}
	top := down[0].RateMbps // ramp_down starts at the highest cap
	for i := 0; i < len(ts); i += 2 {
		if ts[i].RateMbps != top {
			t.Errorf("transient_shock[%d] = %.3f, want top %.3f (recover-to-top between dips)", i, ts[i].RateMbps, top)
		}
	}
	for i := 3; i < len(ts); i += 2 {
		if ts[i].RateMbps >= ts[i-2].RateMbps {
			t.Errorf("transient_shock dips not deepening at %d: %.3f then %.3f", i, ts[i-2].RateMbps, ts[i].RateMbps)
		}
	}
	if ts[1].RateMbps != down[1].RateMbps {
		t.Errorf("transient_shock first dip = %.3f, want second-highest rung %.3f", ts[1].RateMbps, down[1].RateMbps)
	}
	if ts[len(ts)-2].RateMbps != sq[0].RateMbps {
		t.Errorf("transient_shock final dip = %.3f, want bottom %.3f", ts[len(ts)-2].RateMbps, sq[0].RateMbps)
	}
	if BuildPattern("sliders", rungs, 12) != nil {
		t.Errorf("unknown template should return nil")
	}
}

func hasHazard(hz []Hazard, kind string) bool {
	for _, h := range hz {
		if h.Kind == kind {
			return true
		}
	}
	return false
}

func TestRound3(t *testing.T) {
	if got := round3(1.0851288); got != 1.085 {
		t.Errorf("round3 = %v want 1.085", got)
	}
	if math.Abs(round3(0.83328105)-0.833) > 1e-9 {
		t.Errorf("round3 avg mismatch")
	}
}
