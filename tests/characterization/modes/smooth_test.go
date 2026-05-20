package modes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Smooth — variant-aware ramp_down at multiple margins.
//
// For each variant in the current play's manifest we apply 6 caps in
// descending order (×1.50, ×1.25, ×1.10, ×1.05, ×1.00, ×0.95), merged
// across all variants into one strictly-descending series. Any candidate
// that would have caused the cap to *increase* vs. the previous step gets
// dropped (happens on tight ladders where var_low×1.50 > var_high×0.95).
//
// Each step is held for up to smoothMaxHold (60 s) but exits early as
// soon as the buffer has been stable across the last smoothEarlyExitWindow
// (15 s) — a stable buffer proves this cap is safe for the current variant
// without waiting for a stall that won't happen. Minimum hold is
// smoothMinHold (15 s) so the early-exit predicate has enough data to fire.
//
// Pass condition: every variant in the ladder appears in
// Summary.VariantSampleCounts > 0. The headline operational finding is
// Summary.LowestSustainableCapMbps — the smallest cap that kept the buffer
// above SustainableBufferS (1 s) with zero stalls. Anything below that is
// where the player starts depleting / stalling.

var smoothMargins = []int{50, 25, 10, 5, 0, -5}

const (
	smoothMaxHold         = 60 * time.Second
	smoothMinHold         = 15 * time.Second
	smoothEarlyExitWindow = 15 * time.Second
	smoothEarlyExitTol    = 0.5 // s of buffer-drop tolerance over the window
	smoothWarmupMbps      = 100
	smoothWarmupHold      = 15 * time.Second
)

func TestSmoothIPadSim(t *testing.T)   { runSmooth(t, runner.PlatformIPadSim) }
func TestSmoothIPhone(t *testing.T)    { runSmooth(t, runner.PlatformIPhone) }
func TestSmoothAppleTV(t *testing.T)   { runSmooth(t, runner.PlatformAppleTV) }
func TestSmoothAndroidTV(t *testing.T) { runSmooth(t, runner.PlatformAndroidTV) }
func TestSmoothWeb(t *testing.T)       { runSmooth(t, runner.PlatformWeb) }

func runSmooth(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)

	// Tag the current play with searchable metadata BEFORE the sweep
	// starts. If the test crashes mid-sweep these still survive — at
	// least we know which play_id corresponds to which test run.
	runID := time.Now().UTC().Format("20060102T150405Z")
	startLabels := map[string]string{
		"test":     "smooth",
		"platform": string(p),
		"run_id":   runID,
	}
	if err := sess.LabelPlay(context.Background(), startLabels); err != nil {
		t.Logf("label play (start): %v (test continues)", err)
	} else {
		t.Logf("labeled play with %v", startLabels)
	}

	// Capture the active play_id so it's findable later via
	// `harness query play <id>` or `harness play show <id>` (while live).
	playID, err := sess.CurrentPlayID(context.Background())
	if err != nil {
		t.Logf("CurrentPlayID: %v (test continues)", err)
	} else {
		t.Logf("play_id: %s   (find later: harness query play %s)", playID, playID)
	}

	// Worst case: 8-rung ladder × 6 margins = 48 steps × 60 s = 48 min,
	// plus warmup + slack. Realistically half of those exit early in <30s
	// so usual runtime is ~20 min, but we set the upper bound to avoid
	// flake from a slow stall-recovery scenario.
	overall := smoothWarmupHold + 60*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	// Warmup at 100 Mbps so the proxy's avg_network_bitrate is real (not
	// "uncapped, infinite") and the player picks its preferred top variant.
	if err := sess.ApplyRate(ctx, smoothWarmupMbps); err != nil {
		t.Fatalf("warmup apply %d Mbps: %v", smoothWarmupMbps, err)
	}
	t.Logf("warmup: %d Mbps × %s", smoothWarmupMbps, smoothWarmupHold)
	if err := holdContext(ctx, smoothWarmupHold); err != nil {
		t.Fatalf("warmup hold: %v", err)
	}

	// Pull variants AFTER warmup so we get the manifest the player just
	// fetched.
	rec, err := sess.PlayerState(ctx)
	if err != nil {
		t.Fatalf("PlayerState: %v", err)
	}
	sweep, err := runner.VariantSweep(rec, smoothMargins)
	if err != nil {
		t.Fatalf("VariantSweep: %v", err)
	}

	// Pre-sweep dump — every limit gets logged so the operator can sanity
	// check before the sweep runs.
	t.Logf("sweep plan: %d steps (margins %v, max-hold %s, early-exit when buffer stable %s)",
		len(sweep), smoothMargins, smoothMaxHold, smoothEarlyExitWindow)
	for i, v := range sweep {
		t.Logf("  [%2d] %-10s  %+3d%%  cap=%6.3f Mbps   avg=%.3f peak=%.3f Mbps  (source=%s)",
			i, v.Resolution, v.MarginPct, v.CapMbps,
			float64(v.AvgBps)/1_000_000, float64(v.PeakBps)/1_000_000, v.Source)
	}

	// Materialize the step list — one Step per cap, each carries its
	// variant identity so the report can break results down by rung.
	steps := make([]runner.Step, len(sweep))
	for i, v := range sweep {
		v := v
		steps[i] = runner.Step{RateMbps: v.CapMbps, Hold: smoothMaxHold, Variant: &v}
	}

	report := RunVariantSweep(ctx, t, sess, "smooth", steps, time.Second,
		smoothMinHold, smoothMaxHold, smoothEarlyExitWindow, smoothEarlyExitTol)
	// We need the per-variant rung list (not the per-step list) for
	// Finalize's classify-by-variant pass.
	report.Variants = unionRungs(sweep)
	if playID != "" {
		report.PlayIDs = []string{playID}
	}
	report.Finalize(time.Now())

	out := runner.DefaultOutDir(t.TempDir())
	// Filename pattern: smooth-<platform>-<player8>-<run_id>.<ext>
	// Including the 8-char player_id prefix makes parallel runs on
	// different devices land in distinct files without collision.
	playerShort := sess.PlayerID
	if len(playerShort) > 8 {
		playerShort = playerShort[:8]
	}
	base := fmt.Sprintf("smooth-%s-%s-%s", p, playerShort, runID)
	jsonPath, err := runner.WriteReport(out, base, report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	LogReport(t, jsonPath)
	if htmlPath, err := runner.WriteChart(out, base, report); err == nil {
		t.Logf("chart: %s", htmlPath)
	} else {
		t.Logf("chart write skipped: %v", err)
	}

	// Headline findings.
	t.Logf("lowest sustainable cap: %.3f Mbps", report.Summary.LowestSustainableCapMbps)
	if report.Summary.HighestStallingCapMbps > 0 {
		t.Logf("highest stalling cap:   %.3f Mbps", report.Summary.HighestStallingCapMbps)
	}
	t.Logf("total stalls: %d / stall seconds: %.1f / profile shifts: %d",
		report.Summary.TotalStalls, report.Summary.TotalStallSeconds, report.Summary.ProfileShifts)

	// Post-sweep labels — record headline numbers on the play so they
	// appear next to the metadata when queried later.
	endLabels := map[string]string{
		"completed":            time.Now().UTC().Format("20060102T150405Z"),
		"lowest_sustainable":   fmt.Sprintf("%.3f", report.Summary.LowestSustainableCapMbps),
		"bottom_variant_floor": fmt.Sprintf("%.3f", report.Summary.BottomVariantFloorMbps),
		"total_stalls":         fmt.Sprintf("%d", report.Summary.TotalStalls),
		"profile_shifts":       fmt.Sprintf("%d", report.Summary.ProfileShifts),
	}
	if err := sess.LabelPlay(context.Background(), endLabels); err != nil {
		t.Logf("label play (end): %v", err)
	}

	// Pass criteria.
	if report.Summary.SampleCount == 0 {
		t.Fatal("no samples collected")
	}
	// (1) Every variant must have been observed at some point during the
	// sweep — proves the player actually walked the ladder.
	missing := []string{}
	for i, v := range report.Variants {
		if i >= len(report.Summary.VariantSampleCounts) || report.Summary.VariantSampleCounts[i] == 0 {
			missing = append(missing, v.Resolution)
		}
	}
	if len(missing) > 0 {
		t.Errorf("player did not visit every variant — missing: %v", missing)
	}
	t.Logf("variants visited (samples per rung): %v", report.Summary.VariantSampleCounts)

	// (2) Any stall or buffer depletion on a step whose target variant
	// is NOT the bottom rung is a failure. At the bottom rung the player
	// has no escape downshift, so failures there are expected when the
	// cap drops below what that variant needs. At higher rungs a failure
	// means the player couldn't recover fast enough by downshifting —
	// that's the bug. We don't short-circuit; we want the recovery data
	// in the report.
	bottomRes := ""
	if n := len(report.Variants); n > 0 {
		bottomRes = report.Variants[n-1].Resolution
	}
	upperFailures := []string{}
	for i := range report.Steps {
		st := &report.Steps[i]
		if st.Variant == nil || st.Variant.Resolution == bottomRes {
			continue
		}
		// Skipped steps didn't actually run — zero MinBuffer is the
		// default value, not a measured stall. Don't flag them.
		if st.ExitReason == "skipped-player-wedged" {
			continue
		}
		failed := st.StallsDelta > 0 || st.MinBufferS < runner.SustainableBufferS
		if !failed {
			continue
		}
		upperFailures = append(upperFailures, fmt.Sprintf(
			"step %d cap=%.3f Mbps %s/%+d%%: stalls=%d min_buf=%.1fs",
			i+1, st.RateMbps, st.Variant.Resolution, st.Variant.MarginPct,
			st.StallsDelta, st.MinBufferS))
	}
	if len(upperFailures) > 0 {
		t.Errorf("player stalled / depleted buffer at %d non-bottom variant(s):\n  %s",
			len(upperFailures), joinLines(upperFailures))
	}
}

func joinLines(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += "\n  "
		}
		out += s
	}
	return out
}

// unionRungs collapses the per-step VariantSweep slice (one entry per
// (variant, margin) pair) down to one entry per unique variant — what
// Finalize's per-rung sample classification needs.
func unionRungs(sweep []runner.VariantRate) []runner.VariantRate {
	seen := map[string]int{} // resolution → index in out
	out := []runner.VariantRate{}
	for _, v := range sweep {
		if _, ok := seen[v.Resolution]; ok {
			continue
		}
		seen[v.Resolution] = len(out)
		out = append(out, v)
	}
	return out
}
