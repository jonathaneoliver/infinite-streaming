package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

func TestAggregatorMatrix(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, r runner.Report) {
		raw, err := json.MarshalIndent(r, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC()
	write("smooth-ipad-sim.json", runner.Report{
		Mode: "smooth", Platform: runner.PlatformIPadSim, StartedAt: base,
		Summary: runner.Summary{TotalStalls: 0, ProfileShifts: 3, SampleCount: 120, MaxBufferDepthS: 8.0, MeanBitrateMbps: 2.4, MinBitrateMbps: 0.7, MaxBitrateMbps: 4.5},
	})
	write("smooth-iphone.json", runner.Report{
		Mode: "smooth", Platform: runner.PlatformIPhone, StartedAt: base,
		Summary: runner.Summary{TotalStalls: 2, TotalStallSeconds: 4.5, ProfileShifts: 5, SampleCount: 120},
	})
	write("emergency-iphone.json", runner.Report{
		Mode: "emergency-downshift", Platform: runner.PlatformIPhone, StartedAt: base,
		Summary: runner.Summary{TotalStalls: 1, TotalStallSeconds: 12.3},
	})
	// Bogus json that isn't a report — aggregator must skip silently.
	if err := os.WriteFile(filepath.Join(dir, "noise.json"), []byte(`{"unrelated":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	reports, err := loadReports(dir)
	if err != nil {
		t.Fatalf("loadReports: %v", err)
	}
	if len(reports) != 3 {
		t.Fatalf("loaded %d reports, want 3 (noise.json should be skipped)", len(reports))
	}

	md := renderMatrix(reports)
	for _, needle := range []string{
		"## emergency-downshift",
		"## smooth",
		"| iphone | 2 | 4.5 | 5 |",
		"| ipad-sim | 0 |",
	} {
		if !strings.Contains(md, needle) {
			t.Errorf("matrix missing %q\n---\n%s", needle, md)
		}
	}
}
