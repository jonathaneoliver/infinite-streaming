package app

import (
	"fmt"
	"strings"
	"testing"
)

// Lines as create_abr_ladder.sh actually prints them, colour codes included.
const (
	ansiGreen = "\x1b[0;32m"
	ansiReset = "\x1b[0m"
)

func scriptLog(msg string) string { return ansiGreen + "[11:47:48]" + ansiReset + " " + msg }

// ffmpegProgress is one `-progress pipe:1` out_time line.
func ffmpegProgress(sec float64) string {
	h := int(sec) / 3600
	m := (int(sec) % 3600) / 60
	s := sec - float64(h*3600+m*60)
	return fmt.Sprintf("out_time=%02d:%02d:%09.6f", h, m, s)
}

// replay feeds lines to a tracker and returns every progress value it emitted.
func replay(t *testing.T, tr *EncodingProgressTracker, lines []string) []int {
	t.Helper()
	var out []int
	for _, l := range lines {
		if p := tr.ParseLine(l); p != nil {
			out = append(out, *p)
		}
	}
	return out
}

// encodeRungs builds the Phase 3 output for n rungs, each two-pass with
// out_time progress through a clip of dur seconds.
func encodeRungs(n int, dur float64, twoPass bool) []string {
	lines := []string{scriptLog("Phase 3: Encoding Video Variants")}
	for i := 1; i <= n; i++ {
		lines = append(lines, scriptLog(fmt.Sprintf("Encoding: HEVC %dp | 30.00fps @ 0.60Mbps (600kbps target, preset medium) - libx265/libx264 (software)", 200+i*20)))
		passes := []string{""}
		if twoPass {
			passes = []string{"Two-pass: pass 1/2 (complexity analysis, output discarded)", "Two-pass: pass 2/2 (final encode to target average)"}
		}
		for _, pass := range passes {
			if pass != "" {
				lines = append(lines, scriptLog("  "+pass))
			}
			for _, frac := range []float64{0, 0.25, 0.5, 0.75, 1} {
				lines = append(lines, ffmpegProgress(frac*dur))
			}
		}
	}
	return lines
}

func assertMonotonicWithin(t *testing.T, vals []int, lo, hi int) {
	t.Helper()
	prev := -1
	for i, v := range vals {
		if v < lo || v > hi {
			t.Fatalf("progress[%d] = %d, want within [%d, %d] (all: %v)", i, v, lo, hi, vals)
		}
		if v < prev {
			t.Fatalf("progress went backwards at [%d]: %d -> %d (all: %v)", i, prev, v, vals)
		}
		prev = v
	}
}

// Regression for #1018: codec=both with no max_resolution estimates 4 variants,
// the apple-uniq-live-xs ladder selects 14, and progress used to reach ~217%.
func TestProgressUsesSelectedVariantCount(t *testing.T) {
	cfg := map[string]interface{}{
		"codec_selection": "both",
		"metadata":        map[string]interface{}{"duration": 120.0},
	}
	tr := NewEncodingProgressTracker(cfg)
	if tr.totalVariants != 4 {
		t.Fatalf("precondition: estimate = %d, want 4", tr.totalVariants)
	}

	lines := []string{
		scriptLog("Phase 2b: Selecting Resolution Tiers"),
		ansiGreen + "✓" + ansiReset + " Selected 14 variants for encoding (ladder: apple-uniq-live-xs)",
	}
	lines = append(lines, encodeRungs(14, 120, true)...)
	vals := replay(t, tr, lines)

	if tr.totalVariants != 14 {
		t.Fatalf("totalVariants = %d, want 14 from the script's Selected line", tr.totalVariants)
	}
	assertMonotonicWithin(t, vals, 0, encodingCeiling)
	if last := vals[len(vals)-1]; last < encodingCeiling-2 {
		t.Fatalf("progress after the last rung = %d, want ~%d", last, encodingCeiling)
	}
	if msg := tr.ProgressMessage(); !strings.Contains(msg, "/14:") {
		t.Fatalf("ProgressMessage = %q, want it to report N/14", msg)
	}
}

// Without the Selected line the estimate remains the fallback, but progress
// must still never leave the encoding share.
func TestProgressFallbackEstimateIsClamped(t *testing.T) {
	cfg := map[string]interface{}{"codec_selection": "both", "metadata": map[string]interface{}{"duration": 120.0}}
	tr := NewEncodingProgressTracker(cfg)
	vals := replay(t, tr, encodeRungs(14, 120, false))
	assertMonotonicWithin(t, vals, 0, encodingCeiling)
}

// Pass 2 restarts out_time at zero; it must continue from pass 1, not replay it.
func TestTwoPassProgressAdvancesThroughBothPasses(t *testing.T) {
	cfg := map[string]interface{}{"codec_selection": "hevc", "metadata": map[string]interface{}{"duration": 100.0}}
	tr := NewEncodingProgressTracker(cfg)
	replay(t, tr, []string{ansiGreen + "✓" + ansiReset + " Selected 2 variants for encoding (ladder: x)"})
	vals := replay(t, tr, encodeRungs(1, 100, true))
	assertMonotonicWithin(t, vals, 0, encodingCeiling)

	// One of two rungs done = half of the 18..75 encoding share.
	mid := 18 + (encodingCeiling-18)/2
	if last := vals[len(vals)-1]; last < mid-1 || last > mid+1 {
		t.Fatalf("after 1 of 2 two-pass rungs progress = %d, want ~%d", last, mid)
	}
}

// Later phases set fixed values; the bar must not jump back to them.
func TestProgressNeverDecreasesAcrossPhases(t *testing.T) {
	cfg := map[string]interface{}{"codec_selection": "both", "metadata": map[string]interface{}{"duration": 120.0}}
	tr := NewEncodingProgressTracker(cfg)
	lines := encodeRungs(14, 120, true) // no Selected line: stresses the fallback path too
	lines = append(lines,
		scriptLog("Phase 4: Creating Audio Mezzanine"),
		ffmpegProgress(60),
		scriptLog("Phase 5: Packaging"),
		scriptLog("Phase 6: Packaging"),
		scriptLog("Phase 7: Generating HLS"),
		scriptLog("Encoding Complete"),
	)
	vals := replay(t, tr, lines)
	assertMonotonicWithin(t, vals, 0, 100)
	if last := vals[len(vals)-1]; last != 95 {
		t.Fatalf("final progress = %d, want 95 at Encoding Complete", last)
	}
}
