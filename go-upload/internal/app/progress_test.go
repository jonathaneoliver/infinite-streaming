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

// A dashboard re-encode stores duration_limit=0 for "no limit" and carries no
// metadata. That 0 used to become the source duration, which switched
// time-based progress off: the bar only stepped once per rung.
func TestProgressDurationLimitZeroIsNoLimit(t *testing.T) {
	cfg := map[string]interface{}{"codec_selection": "hevc", "duration_limit": 0.0} // JSON number
	tr := NewEncodingProgressTracker(cfg)
	lines := []string{
		scriptLog("Phase 1: Input Validation"),
		scriptLog("Duration: 120s"),
		ansiGreen + "✓" + ansiReset + " Selected 2 variants for encoding (ladder: x)",
	}
	lines = append(lines, encodeRungs(1, 120, true)...)
	vals := replay(t, tr, lines)

	if tr.sourceDuration != 120 {
		t.Fatalf("sourceDuration = %v, want 120 from the script's Duration line", tr.sourceDuration)
	}
	assertMonotonicWithin(t, vals, 0, encodingCeiling)
	mid := 18 + (encodingCeiling-18)/2
	if last := vals[len(vals)-1]; last < mid-1 || last > mid+1 {
		t.Fatalf("after 1 of 2 two-pass rungs progress = %d, want ~%d (all: %v)", last, mid, vals)
	}
}

// With no metadata and no limit (the first-run seed) the tracker starts from
// a 100s guess; a 120s clip then saturated each pass ~17% early. The script's
// Duration line must replace the guess, and other "duration" lines must not.
func TestProgressUsesScriptDurationOverFallback(t *testing.T) {
	tr := NewEncodingProgressTracker(map[string]interface{}{"codec_selection": "hevc"})
	if tr.sourceDuration != 100 {
		t.Fatalf("precondition: fallback sourceDuration = %v, want 100", tr.sourceDuration)
	}
	lines := []string{
		scriptLog("Duration: 120s"),
		scriptLog("Video duration: 118.500s"),
		scriptLog("Configured segment duration: 6s"),
		ansiGreen + "✓" + ansiReset + " Selected 1 variants for encoding (ladder: x)",
	}
	lines = append(lines, encodeRungs(1, 120, false)...) // out_time 0, 30, 60, 90, 120
	vals := replay(t, tr, lines)

	if tr.sourceDuration != 120 {
		t.Fatalf("sourceDuration = %v, want 120", tr.sourceDuration)
	}
	// out_time=90 of 120s is 75% through the only rung: 18 + 57*0.75 = 60.
	// Measured against 100s it would read 90% (69).
	if got := vals[len(vals)-2]; got < 59 || got > 61 {
		t.Fatalf("progress at out_time=90s = %d, want ~60 (all: %v)", got, vals)
	}
}

// A positive limit truncates the encode, so each pass covers only that long.
func TestProgressDurationCappedByLimit(t *testing.T) {
	cfg := map[string]interface{}{
		"codec_selection": "hevc",
		"duration_limit":  24,
		"metadata":        map[string]interface{}{"duration": 120.0},
	}
	tr := NewEncodingProgressTracker(cfg)
	if tr.sourceDuration != 24 {
		t.Fatalf("initial sourceDuration = %v, want the 24s limit, not the 120s clip", tr.sourceDuration)
	}
	replay(t, tr, []string{scriptLog("Duration: 120s")})
	if tr.sourceDuration != 24 {
		t.Fatalf("after Duration line sourceDuration = %v, want 24", tr.sourceDuration)
	}
	tr = NewEncodingProgressTracker(map[string]interface{}{"duration_limit": 300.0})
	replay(t, tr, []string{scriptLog("Duration: 120s")})
	if tr.sourceDuration != 120 {
		t.Fatalf("limit longer than clip: sourceDuration = %v, want 120", tr.sourceDuration)
	}
}
