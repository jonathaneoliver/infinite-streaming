package modes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Rampup — variant-aware ascending sweep.
//
// Mirror image of Rampdown. Start at a cap just above the *bottom*
// variant's playable rate (its AVERAGE-BANDWIDTH × 1.10, the same +10%
// margin row Rampdown lands on at its tail) and ramp UP through every
// (variant, margin) step in ascending order, ending at top variant ×
// 1.50.
//
// Why start at bottom+10% (not at bottom -5% / 0 / +5% like Rampdown):
// those lower caps deliberately stall the bottom variant, which is
// useful for "how low can we go" characterization. Rampup is the
// opposite question — "given a starting throughput that's barely
// enough for the lowest variant, can the player walk UP the ladder
// as the cap rises?" Stalling at the start would just delay the data
// we care about.
//
// Pass condition: same as Rampdown — every variant visited, no
// stalls/depletion on non-bottom variants. Headline metric becomes
// the FIRST cap that the player started visiting a higher variant
// (vs. Rampdown's "lowest sustainable cap"). The framework reports
// the same Summary fields; reading them inverts naturally.

// Margins reuse the Rampdown set; the only difference is the order.
// We filter out caps below the +10% floor for the bottom variant
// (those are deliberate-stall territory, not interesting for rampup).
var rampupMargins = rampdownMargins

// rampupBottomMargin is the minimum margin we test on the bottom
// variant. 0 means "start at the variant's AVG × TCP-overhead-only
// cap" — i.e. the just-barely-sustainable rate. Below this we'd be
// testing "how badly can the bottom variant stall" — that's Rampdown's
// job. Higher variants get the full margin range (including negatives,
// which test "approaching the variant from below").
const rampupBottomMargin = 0

func TestRampupIPadSim(t *testing.T)   { runRampup(t, runner.PlatformIPadSim) }
func TestRampupIPhone(t *testing.T)    { runRampup(t, runner.PlatformIPhone) }
func TestRampupAppleTV(t *testing.T)   { runRampup(t, runner.PlatformAppleTV) }
func TestRampupAndroidTV(t *testing.T) { runRampup(t, runner.PlatformAndroidTV) }
func TestRampupWeb(t *testing.T)       { runRampup(t, runner.PlatformWeb) }

func runRampup(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)

	runID := time.Now().UTC().Format("20060102T150405Z")
	startLabels := map[string]string{
		"test":     "rampup",
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

	overall := rampdownWarmupHold + 60*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	// Warmup is still high — the test EVENTUALLY needs to know the
	// ladder, and the warmup cap of 100 Mbps lets the player fetch
	// the full manifest. We could start the actual sweep low, but
	// the warmup itself stays loose to ensure manifest discovery.
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

	// Find the bottom variant (last in desc). Filter by *variant identity*
	// rather than recomputing a numeric floor — recomputing produces a
	// value slightly above CapMbps thanks to float-precision differences
	// between (raw_avg × margin/100 / 1e6) and the same quantity rounded
	// to 3 dp at VariantRate-build time. That ">=" comparison would drop
	// the boundary entry (bottom variant +10%) — exactly the row we want
	// to start at.
	if len(desc) == 0 {
		t.Fatal("VariantSweep returned no entries")
	}
	bottomRes := desc[len(desc)-1].Resolution

	// Ascending sweep: keep all non-bottom-variant entries; for the
	// bottom variant keep only entries with margin ≥ rampupBottomMargin.
	// Then reverse to ascending order.
	asc := make([]runner.VariantRate, 0, len(desc))
	for _, v := range desc {
		if v.Resolution == bottomRes && v.MarginPct < rampupBottomMargin {
			continue
		}
		asc = append(asc, v)
	}
	floor := 0.0
	if len(asc) > 0 {
		floor = asc[len(asc)-1].CapMbps // last in desc-order = lowest cap = the floor
	}
	// reverse in place
	for i, j := 0, len(asc)-1; i < j; i, j = i+1, j-1 {
		asc[i], asc[j] = asc[j], asc[i]
	}

	t.Logf("sweep plan: %d steps ascending from %.3f Mbps (bottom variant + %d%%)",
		len(asc), floor, rampupBottomMargin)
	for i, v := range asc {
		t.Logf("  [%2d] %-10s  %+3d%%  cap=%6.3f Mbps   avg=%.3f peak=%.3f Mbps  (source=%s)",
			i, v.Resolution, v.MarginPct, v.CapMbps,
			float64(v.AvgBps)/1_000_000, float64(v.PeakBps)/1_000_000, v.Source)
	}

	steps := make([]runner.Step, len(asc))
	for i, v := range asc {
		v := v
		steps[i] = runner.Step{RateMbps: v.CapMbps, Hold: rampdownMaxHold, Variant: &v}
	}

	report := RunVariantSweep(ctx, t, sess, "rampup", steps, time.Second,
		rampdownMinHold, rampdownMaxHold, rampdownEarlyExitWindow, rampdownEarlyExitTol)
	report.Variants = unionRungs(desc) // same ladder, descending — Finalize is order-agnostic
	if playID != "" {
		report.PlayIDs = []string{playID}
	}
	report.Finalize(time.Now())

	out := runner.DefaultOutDir(t.TempDir())
	playerShort := sess.PlayerID
	if len(playerShort) > 8 {
		playerShort = playerShort[:8]
	}
	base := fmt.Sprintf("rampup-%s-%s-%s", p, playerShort, runID)
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

	// Pass criteria — identical to Rampdown.
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
