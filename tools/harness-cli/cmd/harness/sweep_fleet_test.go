package main

import (
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/charmatrix"
	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

func fanExp(id, seg string, arm sweep.Arm, pattern bool) *sweep.Experiment {
	e := &sweep.Experiment{
		ID: id, Platform: "ipad-sim", Protocol: "hls", Content: "clip_x",
		Segment: seg, Group: "iso-abc", Arm: arm, Parent: "p1",
	}
	if pattern {
		e.Shape = &sweep.Shape{Pattern: "valley", StepSeconds: 12}
	}
	return e
}

func TestBuildFanRunPlan(t *testing.T) {
	fan := []*sweep.Experiment{
		fanExp("iso-abc-control", "s2", sweep.ArmControl, true),
		fanExp("iso-abc-segment", "s6", sweep.ArmVariant, true),
		fanExp("iso-abc-platform", "s2", sweep.ArmVariant, true),
	}
	playerIDs := []string{"pid0", "pid1", ""} // arm 2 bootstrap-failed → must skip

	plan := buildFanRunPlan(fan, playerIDs, charmatrix.RunLevel{}, "https://base", "/tmp/man", 90)

	if plan.FleetCount != 3 || len(plan.Arms) != 3 {
		t.Fatalf("fleet shape wrong: count=%d arms=%d", plan.FleetCount, len(plan.Arms))
	}
	if plan.DurationS != 90 || plan.Platform != "ipad-sim" || plan.BaseURL != "https://base" || plan.DeviceManifest != "/tmp/man" {
		t.Fatalf("run-level fields wrong: %+v", plan)
	}
	if plan.Arms[0].PlayerID != "pid0" || plan.Arms[1].PlayerID != "pid1" {
		t.Fatalf("player ids not bound: %+v", plan.Arms)
	}
	// The empty-playerID arm stays a zero-value ArmConfig so its fleet index skips.
	if plan.Arms[2].PlayerID != "" || plan.Arms[2].Segment != "" {
		t.Fatalf("empty-playerID arm must be zero-value, got %+v", plan.Arms[2])
	}
	// Per-arm segment survives the Experiment→Arm→ArmConfig round-trip.
	if plan.Arms[0].Segment != "s2" || plan.Arms[1].Segment != "s6" {
		t.Fatalf("segments wrong: %q %q", plan.Arms[0].Segment, plan.Arms[1].Segment)
	}
	// Exactly one arm masters the shared pattern, and it's a bound arm.
	masters := 0
	for _, a := range plan.Arms {
		if a.PatternMaster {
			masters++
		}
	}
	if masters != 1 || !plan.Arms[0].PatternMaster {
		t.Fatalf("want exactly arm 0 as pattern master, got masters=%d arm0=%v", masters, plan.Arms[0].PatternMaster)
	}
}

func TestBuildFanRunPlanWindowDefault(t *testing.T) {
	fan := []*sweep.Experiment{
		fanExp("c", "s2", sweep.ArmControl, false),
		fanExp("v", "s6", sweep.ArmVariant, false),
	}
	// override 0 + no per-arm duration → falls back to 60.
	plan := buildFanRunPlan(fan, []string{"a", "b"}, charmatrix.RunLevel{}, "", "", 0)
	if plan.DurationS != 60 {
		t.Fatalf("default window want 60, got %d", plan.DurationS)
	}
	// No pattern anywhere → no master.
	for i, a := range plan.Arms {
		if a.PatternMaster {
			t.Fatalf("no pattern arm should master, arm %d does", i)
		}
	}
}
