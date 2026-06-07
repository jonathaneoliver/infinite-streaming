package modes

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Rampdown — variant-aware descending sweep over the shared limit ladder.
//
// The ladder (go-proxy/pkg/ladder via runner.StandardLadderRates) carries
// both a peak (BANDWIDTH) and an average (AVERAGE-BANDWIDTH) anchor per
// variant — each ×1.05 — plus geometric fills so no two consecutive caps
// differ by more than CHAR_LADDER_MAX_STEP (1.15×). We apply them in
// descending order. Each rung is attributed to the variant a peak-keyed
// player should sustain at that cap (#551).
//
// Each step is held for up to rampdownMaxHold (60 s) but exits early as
// soon as the buffer has been stable across the last rampdownEarlyExitWindow
// (15 s) — a stable buffer proves this cap is safe for the current variant
// without waiting for a stall that won't happen. Minimum hold is
// rampdownMinHold (15 s) so the early-exit predicate has enough data to fire.
//
// Pass condition: every variant in the ladder appears in
// Summary.VariantSampleCounts > 0. The headline operational finding is
// Summary.LowestSustainableCapMbps — the smallest cap that kept the buffer
// above SustainableBufferS (1 s) with zero stalls. Anything below that is
// where the player starts depleting / stalling.

// Fill density + headroom are controlled by CHAR_LADDER_MAX_STEP (default
// 1.15×) and CHAR_LADDER_BUMP_PCT (default 5%) — see runner.StandardLadderRates.

const (
	rampdownMaxHold         = 60 * time.Second
	rampdownMinHold         = 15 * time.Second
	rampdownEarlyExitWindow = 15 * time.Second
	rampdownEarlyExitTol    = 0.5 // s of buffer-drop tolerance over the window
	rampdownWarmupMbps      = 100
	rampdownWarmupHold      = 15 * time.Second
)

func TestRampdownIPadSim(t *testing.T)   { runRampdown(t, runner.PlatformIPadSim) }
func TestRampdownIPhone(t *testing.T)    { runRampdown(t, runner.PlatformIPhone) }
func TestRampdownAppleTV(t *testing.T)   { runRampdown(t, runner.PlatformAppleTV) }
func TestRampdownAndroidTV(t *testing.T) { runRampdown(t, runner.PlatformAndroidTV) }
func TestRampdownWeb(t *testing.T)       { runRampdown(t, runner.PlatformWeb) }

func runRampdown(t *testing.T, p runner.Platform) {
	sess := OpenSession(t, p)

	// Tag the current play with searchable metadata BEFORE the sweep
	// starts. If the test crashes mid-sweep these still survive — at
	// least we know which play_id corresponds to which test run.
	runID := time.Now().UTC().Format("20060102T150405Z")
	startLabels := map[string]string{
		"test":     "rampdown",
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
	overall := rampdownWarmupHold + 60*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	// Warmup at 100 Mbps so the proxy's avg_network_bitrate is real (not
	// "uncapped, infinite") and the player picks its preferred top variant.
	if err := sess.ApplyRate(ctx, rampdownWarmupMbps); err != nil {
		t.Fatalf("warmup apply %d Mbps: %v", rampdownWarmupMbps, err)
	}
	t.Logf("warmup: %d Mbps × %s", rampdownWarmupMbps, rampdownWarmupHold)
	if err := holdContext(ctx, rampdownWarmupHold); err != nil {
		t.Fatalf("warmup hold: %v", err)
	}

	// Pull variants AFTER warmup so we get the manifest the player just
	// fetched.
	rec, err := sess.PlayerState(ctx)
	if err != nil {
		t.Fatalf("PlayerState: %v", err)
	}
	sweep, err := runner.StandardLadderRates(rec)
	if err != nil {
		t.Fatalf("StandardLadderRates: %v", err)
	}

	// Pre-sweep dump — every limit gets logged so the operator can sanity
	// check before the sweep runs.
	t.Logf("sweep plan: %d rungs (bump %.0f%%, max-step %.2fx, max-hold %s, early-exit when buffer stable %s)",
		len(sweep), runner.LadderBumpPct(), runner.LadderMaxStep(), rampdownMaxHold, rampdownEarlyExitWindow)
	for i, v := range sweep {
		t.Logf("  [%2d] %-10s  cap=%6.3f Mbps   avg=%.3f peak=%.3f Mbps  (source=%s)",
			i, v.Resolution, v.CapMbps,
			float64(v.AvgBps)/1_000_000, float64(v.PeakBps)/1_000_000, v.Source)
	}

	// Materialize the step list — one Step per cap, each carries its
	// variant identity so the report can break results down by rung.
	steps := make([]runner.Step, len(sweep))
	for i, v := range sweep {
		v := v
		steps[i] = runner.Step{RateMbps: v.CapMbps, Hold: rampdownMaxHold, Variant: &v}
	}

	// #cycles — run the descent REPS times on the SAME live play (default
	// 3, CHAR_RAMPDOWN_REPS). Between cycles the cap jumps floor→top; the
	// player carries its buffer + current variant across that jump, so we
	// see how it re-converges on the descent each time (the instructive
	// part — a single pass can't show it).
	reps := envInt("CHAR_RAMPDOWN_REPS", 3)
	reports := RunCycledVariantSweep(ctx, t, sess, "rampdown", steps, reps,
		unionRungs(sweep), playID, time.Second,
		rampdownMinHold, rampdownMaxHold, rampdownEarlyExitWindow, rampdownEarlyExitTol)

	out := runner.DefaultOutDir(t.TempDir())
	// Filename pattern: rampdown-<platform>-<player8>-<run_id>-cyc<n>.<ext>.
	// The 8-char player_id prefix keeps parallel device runs distinct; the
	// cycle suffix keeps each cycle's report in its own file.
	playerShort := sess.PlayerID
	if len(playerShort) > 8 {
		playerShort = playerShort[:8]
	}
	var last *runner.Report
	for ri, report := range reports {
		cyc := ri + 1
		last = report
		base := fmt.Sprintf("rampdown-%s-%s-%s-cyc%d", p, playerShort, runID, cyc)
		jsonPath, err := runner.WriteReport(out, base, report)
		if err != nil {
			t.Fatalf("cyc%d write report: %v", cyc, err)
		}
		LogReport(t, jsonPath)
		if htmlPath, err := runner.WriteChart(out, base, report); err == nil {
			t.Logf("chart: %s", htmlPath)
		} else {
			t.Logf("chart write skipped: %v", err)
		}

		// Headline findings (per cycle).
		t.Logf("cyc%d lowest sustainable cap: %.3f Mbps", cyc, report.Summary.LowestSustainableCapMbps)
		if report.Summary.HighestStallingCapMbps > 0 {
			t.Logf("cyc%d highest stalling cap:   %.3f Mbps", cyc, report.Summary.HighestStallingCapMbps)
		}
		t.Logf("cyc%d total stalls: %d / stall seconds: %.1f / profile shifts: %d",
			cyc, report.Summary.TotalStalls, report.Summary.TotalStallSeconds, report.Summary.ProfileShifts)

		// Pass criteria (per cycle).
		if report.Summary.SampleCount == 0 {
			t.Errorf("cyc%d: no samples collected", cyc)
			continue
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
			t.Errorf("cyc%d: player did not visit every variant — missing: %v", cyc, missing)
		}
		t.Logf("cyc%d variants visited (samples per rung): %v", cyc, report.Summary.VariantSampleCounts)

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
				"step %d cap=%.3f Mbps %s/%s: stalls=%d min_buf=%.1fs",
				i+1, st.RateMbps, st.Variant.Resolution, st.Variant.Source,
				st.StallsDelta, st.MinBufferS))
		}
		if len(upperFailures) > 0 {
			t.Errorf("cyc%d: player stalled / depleted buffer at %d non-bottom variant(s):\n  %s",
				cyc, len(upperFailures), joinLines(upperFailures))
		}
	}

	// Post-sweep labels — headline numbers from the LAST cycle on the play
	// (one play, so the labels reflect the final cycle; the per-cycle
	// reports + cycle bands carry the rest).
	if last != nil {
		endLabels := map[string]string{
			"completed":            time.Now().UTC().Format("20060102T150405Z"),
			"cycles":               fmt.Sprintf("%d", len(reports)),
			"lowest_sustainable":   fmt.Sprintf("%.3f", last.Summary.LowestSustainableCapMbps),
			"bottom_variant_floor": fmt.Sprintf("%.3f", last.Summary.BottomVariantFloorMbps),
			"total_stalls":         fmt.Sprintf("%d", last.Summary.TotalStalls),
			"profile_shifts":       fmt.Sprintf("%d", last.Summary.ProfileShifts),
		}
		if err := sess.LabelPlay(context.Background(), endLabels); err != nil {
			t.Logf("label play (end): %v", err)
		}
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

// unionRungs collapses the limit-ladder slice (multiple rungs per
// variant: peak/avg anchors + fills) down to one entry per unique
// variant — what Finalize's per-rung sample classification needs.
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
