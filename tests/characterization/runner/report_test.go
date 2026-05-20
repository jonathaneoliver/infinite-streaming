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
			{Ts: start, AppliedRateMbps: 5.0, BufferDepthS: 8.5, Stalls: 0, StallTimeS: 0, VideoBitrateMbps: 4.9, ProfileShiftCount: 0, DroppedFrames: 0},
			{Ts: start.Add(10 * time.Second), AppliedRateMbps: 3.0, BufferDepthS: 6.0, Stalls: 0, StallTimeS: 0, VideoBitrateMbps: 2.4, ProfileShiftCount: 1, DroppedFrames: 0},
			{Ts: start.Add(20 * time.Second), AppliedRateMbps: 1.5, BufferDepthS: 4.0, Stalls: 1, StallTimeS: 2.3, VideoBitrateMbps: 1.1, ProfileShiftCount: 2, DroppedFrames: 5},
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
