package main

import (
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

func TestCharModeCamel(t *testing.T) {
	for in, want := range map[string]string{
		"rampup":              "Rampup",
		"downshift_severity":  "DownshiftSeverity",
		"hysteresis_gap":      "HysteresisGap",
		"transient_shock":     "TransientShock",
		"emergency_downshift": "EmergencyDownshift",
		"startup_caps":        "StartupCaps",
	} {
		if got := charModeCamel(in); got != want {
			t.Errorf("charModeCamel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCharTestName(t *testing.T) {
	for k, want := range map[[2]string]string{
		{"rampup", "ipad-sim"}:           "TestRampupIPadSim",
		{"downshift_severity", "iphone"}: "TestDownshiftSeverityIPhone",
		{"pyramid", "appletv"}:           "TestPyramidAppleTV",
		{"steps", "androidtv"}:           "TestStepsAndroidTV",
	} {
		got, err := charTestName(k[0], k[1])
		if err != nil || got != want {
			t.Errorf("charTestName(%q,%q) = %q,%v want %q", k[0], k[1], got, err, want)
		}
	}
	if _, err := charTestName("rampup", "nonsense"); err == nil {
		t.Error("unknown platform should error")
	}
}

func TestCharWorkItems(t *testing.T) {
	items, err := charWorkItems([]string{"rampup", "pyramid"}, []string{"iphone"}, 2, "clip")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 { // 2 modes × 1 platform × 2 reps
		t.Fatalf("want 4 items, got %d", len(items))
	}
	for _, it := range items {
		if it.Job != "char" {
			t.Errorf("item %s Job=%q, want char", it.ID, it.Job)
		}
		if it.Content != "clip" || it.LaunchMode != "appium" {
			t.Errorf("item %s missing content/launch_mode: %+v", it.ID, it)
		}
	}
	// Empty platform list → one any-device item per mode.
	items2, _ := charWorkItems([]string{"rampup"}, nil, 1, "")
	if len(items2) != 1 || items2[0].Platform != "" {
		t.Fatalf("empty platforms → 1 any-platform item; got %d %+v", len(items2), items2)
	}
}

func TestCharClaimer(t *testing.T) {
	items, _ := charWorkItems([]string{"rampup"}, []string{"iphone"}, 1, "")
	c := &charClaimer{items: items}
	// A worker whose platform can't service the filter gets nothing.
	if e, _ := c.ClaimNext("w", "androidtv"); e != nil {
		t.Error("androidtv worker must not claim an iphone-filtered item")
	}
	// A matching worker claims it, exactly once.
	e, _ := c.ClaimNext("w", "iphone")
	if e == nil || e.Mode != "rampup" || e.Owner != "w" {
		t.Fatalf("iphone worker should claim rampup: %+v", e)
	}
	if e2, _ := c.ClaimNext("w", "iphone"); e2 != nil {
		t.Error("item must be consumed once")
	}
}

func TestCharClaimerAnyPlatform(t *testing.T) {
	items, _ := charWorkItems([]string{"rampup"}, nil, 1, "") // Platform=""
	c := &charClaimer{items: items}
	if e, _ := c.ClaimNext("w", "iphone-sim", "iphone"); e == nil {
		t.Fatal("empty-filter item must match any worker (sim tokens here)")
	}
}

// TestCombinedClaimer reuses fakeClaimer (from sweep_pool_test.go) as the base.
func TestCombinedClaimer(t *testing.T) {
	charItems, _ := charWorkItems([]string{"rampup"}, nil, 1, "")
	base := &fakeClaimer{backlog: []*sweep.Experiment{{ID: "sweep-1"}}}
	cc := &combinedClaimer{char: &charClaimer{items: charItems}, base: base}

	// Char items drain first.
	e1, _ := cc.ClaimNext("w")
	if e1 == nil || e1.Job != "char" {
		t.Fatalf("first claim should be the char item: %+v", e1)
	}
	// Then it falls through to the sweep backlog.
	e2, _ := cc.ClaimNext("w")
	if e2 == nil || e2.ID != "sweep-1" {
		t.Fatalf("second claim should be the sweep item: %+v", e2)
	}
	// Move routes: char → no-op (base untouched), sweep → the base store.
	_ = cc.Move(sweep.StatusRunning, sweep.StatusBacklog, e1)
	if len(base.requeued) != 0 {
		t.Error("char Move must not touch the base store")
	}
	_ = cc.Move(sweep.StatusRunning, sweep.StatusBacklog, e2)
	if len(base.requeued) != 1 || base.requeued[0] != "sweep-1" {
		t.Errorf("sweep Move must delegate to the base store: %v", base.requeued)
	}
}
