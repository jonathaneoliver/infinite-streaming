package modes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Pyramid — rampup followed by rampdown, ending where it started.
//
// Cap sequence (using bottom variant avg=0.725 as example):
//
//   0.797 (bottom+10%) ──┐
//   ... ascending ...    │  rampup phase
//   31.948 (top+50%)    ─┘
//   ... descending ...   │  rampdown phase
//   0.797 (bottom+10%) ──┘  same as start
//
// The peak (top+50%) appears once, not twice. So a 33-step ascending
// sweep becomes a 65-step pyramid (33 + 32 reversed).
//
// Why this shape: tests both the player's ABR climb behaviour (rampup
// phase) AND its descent behaviour (rampdown phase) within one session,
// against the same player + content + buffer state. The bookend at the
// same cap lets you compare "starting at floor → ending at floor" —
// the descent should return the player to the same variant it was at
// when we started.

func TestPyramidIPadSim(t *testing.T)   { runPyramid(t, runner.PlatformIPadSim) }
func TestPyramidIPhone(t *testing.T)    { runPyramid(t, runner.PlatformIPhone) }
func TestPyramidAppleTV(t *testing.T)   { runPyramid(t, runner.PlatformAppleTV) }
func TestPyramidAndroidTV(t *testing.T) { runPyramid(t, runner.PlatformAndroidTV) }
func TestPyramidWeb(t *testing.T)       { runPyramid(t, runner.PlatformWeb) }

func runPyramid(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)

	runID := time.Now().UTC().Format("20060102T150405Z")
	startLabels := map[string]string{
		"test":     "pyramid",
		"platform": string(p),
		"run_id":   runID,
	}
	if err := sess.LabelPlay(context.Background(), startLabels); err != nil {
		t.Logf("label play (start): %v (test continues)", err)
	} else {
		t.Logf("labeled play with %v", startLabels)
	}

	playID, err := sess.CurrentPlayID(context.Background())
	if err != nil {
		t.Logf("CurrentPlayID: %v (test continues)", err)
	} else {
		t.Logf("play_id: %s   (find later: harness query play %s)", playID, playID)
	}

	// Pyramid is ~2× the steps of rampup → bump the overall budget so
	// the per-test deadline isn't tight when most steps don't early-exit.
	overall := rampdownWarmupHold + 120*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	if err := sess.ApplyRate(ctx, rampdownWarmupMbps); err != nil {
		t.Fatalf("warmup apply %d Mbps: %v", rampdownWarmupMbps, err)
	}
	t.Logf("warmup: %d Mbps × %s", rampdownWarmupMbps, rampdownWarmupHold)
	if err := holdContext(ctx, rampdownWarmupHold); err != nil {
		t.Fatalf("warmup hold: %v", err)
	}

	rec, err := sess.PlayerState(ctx)
	if err != nil {
		t.Fatalf("PlayerState: %v", err)
	}
	desc, err := runner.VariantSweep(rec, rampupMargins)
	if err != nil {
		t.Fatalf("VariantSweep: %v", err)
	}
	desc = dropOverlapsWithLowerVariant(desc)
	if len(desc) == 0 {
		t.Fatal("VariantSweep returned no entries")
	}

	// Filter by variant identity, not numeric floor — see the long
	// comment in rampup_test.go for why.
	bottomRes := desc[len(desc)-1].Resolution
	asc := make([]runner.VariantRate, 0, len(desc))
	for _, v := range desc {
		if v.Resolution == bottomRes {
			continue
		}
		asc = append(asc, v)
	}
	floor := 0.0
	if len(asc) > 0 {
		floor = asc[len(asc)-1].CapMbps
	}
	for i, j := 0, len(asc)-1; i < j; i, j = i+1, j-1 {
		asc[i], asc[j] = asc[j], asc[i]
	}

	// Pyramid = asc + reverse(asc minus its last element). The last
	// element of asc is the peak; we don't repeat it on the descent.
	pyramid := append([]runner.VariantRate{}, asc...)
	for i := len(asc) - 2; i >= 0; i-- {
		pyramid = append(pyramid, asc[i])
	}

	t.Logf("pyramid sweep: %d steps total (%d up + %d down), floor=%.3f Mbps",
		len(pyramid), len(asc), len(asc)-1, floor)
	for i, v := range pyramid {
		phase := "↑"
		if i >= len(asc) {
			phase = "↓"
		}
		t.Logf("  [%2d] %s %-10s  %+3d%%  cap=%6.3f Mbps   avg=%.3f peak=%.3f Mbps",
			i, phase, v.Resolution, v.MarginPct, v.CapMbps,
			float64(v.AvgBps)/1_000_000, float64(v.PeakBps)/1_000_000)
	}

	steps := make([]runner.Step, len(pyramid))
	for i, v := range pyramid {
		v := v
		steps[i] = runner.Step{RateMbps: v.CapMbps, Hold: rampdownMaxHold, Variant: &v}
	}

	report := RunVariantSweep(ctx, t, sess, "pyramid", steps, time.Second,
		rampdownMinHold, rampdownMaxHold, rampdownEarlyExitWindow, rampdownEarlyExitTol)
	report.Variants = unionRungs(desc)
	if playID != "" {
		report.PlayIDs = []string{playID}
	}
	report.Finalize(time.Now())

	out := runner.DefaultOutDir(t.TempDir())
	playerShort := sess.PlayerID
	if len(playerShort) > 8 {
		playerShort = playerShort[:8]
	}
	base := fmt.Sprintf("pyramid-%s-%s-%s", p, playerShort, runID)
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

	t.Logf("lowest sustainable cap: %.3f Mbps", report.Summary.LowestSustainableCapMbps)
	if report.Summary.HighestStallingCapMbps > 0 {
		t.Logf("highest stalling cap:   %.3f Mbps", report.Summary.HighestStallingCapMbps)
	}
	t.Logf("total stalls: %d / stall seconds: %.1f / profile shifts: %d",
		report.Summary.TotalStalls, report.Summary.TotalStallSeconds, report.Summary.ProfileShifts)

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

	// Pass criteria — same shape as rampdown/rampup.
	if report.Summary.SampleCount == 0 {
		t.Fatal("no samples collected")
	}
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

	bottomReportRes := ""
	if n := len(report.Variants); n > 0 {
		bottomReportRes = report.Variants[n-1].Resolution
	}
	upperFailures := []string{}
	for i := range report.Steps {
		st := &report.Steps[i]
		if st.Variant == nil || st.Variant.Resolution == bottomReportRes {
			continue
		}
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
