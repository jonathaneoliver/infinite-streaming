package charmatrix

import (
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

func TestToArmConfig(t *testing.T) {
	yes := true
	a := &Arm{
		Platform: "ipad-sim", Segment: "s2", Protocol: "hls", Codec: "hevc",
		PeakBitrateMbps: 3, Muted: &yes,
		Shape: &sweep.Shape{Pattern: "valley", StepSeconds: 18, MarginPct: 10},
	}
	rl := RunLevel{StartupFwdBufferS: "2", AutoRecovery: &yes}
	ac := a.ToArmConfig("pid-1", "clip_x", rl, true)

	if ac.PlayerID != "pid-1" || ac.Platform != "ipad-sim" || ac.Content != "clip_x" {
		t.Fatalf("binding wrong: %+v", ac)
	}
	if ac.Segment != "s2" || ac.Protocol != "hls" || ac.Codec != "hevc" || ac.PeakBitrateMbps != 3 {
		t.Fatalf("client knobs wrong: %+v", ac)
	}
	if ac.Pattern != "valley" || ac.StepS != 18 || ac.MarginPct != 10 || !ac.PatternMaster {
		t.Fatalf("pattern wrong: %+v", ac)
	}
	if ac.StartupFwdBufferS != "2" {
		t.Fatalf("run-level not carried: %+v", ac)
	}
	if ac.Muted == nil || !*ac.Muted {
		t.Fatalf("muted not carried")
	}
	// WithDefaults: local_proxy defaults to false (non-nil), auto_recovery true.
	if ac.LocalProxy == nil || *ac.LocalProxy {
		t.Fatalf("WithDefaults local_proxy not applied: %v", ac.LocalProxy)
	}
	if ac.AutoRecovery == nil || !*ac.AutoRecovery {
		t.Fatalf("auto_recovery wrong: %v", ac.AutoRecovery)
	}
}

func TestToArmConfigShapeDefaults(t *testing.T) {
	// No shape → empty pattern, but StepS/MarginPct default via WithDefaults.
	ac := (&Arm{Platform: "ipad-sim"}).ToArmConfig("p", "c", RunLevel{}, false)
	if ac.Pattern != "" {
		t.Fatalf("no shape should mean no pattern, got %q", ac.Pattern)
	}
	if ac.StepS != 12 || ac.MarginPct != 5 {
		t.Fatalf("WithDefaults step/margin wrong: step=%d margin=%d", ac.StepS, ac.MarginPct)
	}
}

func TestPatternMasterIndex(t *testing.T) {
	full := func() *Arm { return &Arm{Shape: &sweep.Shape{Pattern: "valley"}} }
	none := func() *Arm { return &Arm{} }
	thinned := func() *Arm { return &Arm{Shape: &sweep.Shape{Pattern: "valley"}, AllowedVariants: "drop-top-rung"} }

	// A full-ladder pattern arm is preferred even when a thinned one precedes it.
	if m, th := PatternMasterIndex([]*Arm{thinned(), full(), none()}); m != 1 || th {
		t.Fatalf("full-ladder preferred: want 1/false, got %d/%v", m, th)
	}
	// Only thinned pattern arms → fall back to the first, flagged thinned.
	if m, th := PatternMasterIndex([]*Arm{none(), thinned()}); m != 1 || !th {
		t.Fatalf("thinned fallback: want 1/true, got %d/%v", m, th)
	}
	// No pattern anywhere → -1.
	if m, th := PatternMasterIndex([]*Arm{none(), none()}); m != -1 || th {
		t.Fatalf("no pattern: want -1/false, got %d/%v", m, th)
	}
	// First full-ladder wins among several.
	if m, _ := PatternMasterIndex([]*Arm{full(), full()}); m != 0 {
		t.Fatalf("first full-ladder: want 0, got %d", m)
	}
}
