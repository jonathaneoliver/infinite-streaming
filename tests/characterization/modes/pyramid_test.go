package modes

import (
	"context"
	"fmt"
	"os"
	"strings"
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

// pyramidFloorFrom returns the pyramid's floor cap: the bottom variant's
// PEAK anchor (peak × bump). Both the ascent start and the descent end sit
// here, and the player cold-starts at it. Falls back to the bottom variant's
// lowest rung when the manifest carries no peak anchor (AVERAGE-BANDWIDTH
// only). 0 when desc is empty. Counterpart to rampup's rampupFloorFrom,
// which instead excludes the bottom variant entirely (it starts ABOVE the
// floor; the pyramid settles ON it). See #632.
func pyramidFloorFrom(desc []runner.VariantRate) float64 {
	if len(desc) == 0 {
		return 0
	}
	bottomRes := desc[len(desc)-1].Resolution
	for _, v := range desc {
		if v.Resolution == bottomRes && v.Source == "peak" {
			return v.CapMbps
		}
	}
	return desc[len(desc)-1].CapMbps
}

func runPyramid(t *testing.T, p runner.Platform) {
	// --- pick launcher + device ----------------------------------
	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	t.Logf("launch mode: %s", mode)

	discCtx, discCancel := context.WithTimeout(context.Background(), 90*time.Second)
	devs, err := launcher.Discover(discCtx)
	discCancel()
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	wantUDID := strings.TrimSpace(os.Getenv("CHARACTERIZATION_DEVICE_UDID"))
	var picked *runner.Device
	for i := range devs {
		if devs[i].Platform != p {
			continue
		}
		if wantUDID != "" && !strings.EqualFold(devs[i].UDID, wantUDID) {
			continue
		}
		picked = &devs[i]
		break
	}
	if picked == nil {
		t.Skipf("no %s device discovered (mode=%s)", p, mode)
	}
	t.Logf("picked device: %s", picked)

	// --- bootstrap: read the manifest BEFORE kill+launch so we can
	// cold-start at the pyramid floor. #632: the ascent must BEGIN on the
	// bottom variant, and the only stall-free way to get there is to apply
	// the floor cap before the first segment — a warm launch leaves the
	// player on 4K (100 Mbps warmup) and slamming to the ~1 Mbps floor
	// strands it mid-4K-segment and wedges. Same machinery rampup uses.
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 15*time.Second)
	preRec, preErr := runner.PreLaunchInfo(bootCtx, *picked)
	bootCancel()

	var preFloor float64
	if preErr != nil {
		t.Logf("pre-launch info: %v (cold-start unavailable; conservative/fallback path)", preErr)
	} else if preRec.CurrentPlay == nil || len(preRec.CurrentPlay.Manifest.Variants) == 0 {
		t.Logf("pre-launch: no current play / no variants yet (cold-start unavailable)")
	} else if preDesc, derr := runner.StandardLadderRates(preRec); derr != nil {
		t.Logf("pre-launch StandardLadderRates: %v (cold-start unavailable)", derr)
	} else {
		preFloor = pyramidFloorFrom(preDesc)
		t.Logf("bootstrap: pre-launch floor = %.3f Mbps (from current player's manifest)", preFloor)
	}

	appium, isAppium := launcher.(*runner.AppiumLauncher)
	coldStart := isAppium && preFloor > 0
	conservativeStart := isAppium && !coldStart

	var sess *runner.Session
	switch {
	case coldStart:
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer setupCancel()
		s, lerr := appium.LaunchToHome(setupCtx, *picked)
		if lerr != nil {
			t.Fatalf("LaunchToHome: %v", lerr)
		}
		s.PlayerID = preRec.ID
		t.Logf("parked on home; applying floor %.3f Mbps before resuming playback", preFloor)
		if aerr := s.ApplyRate(setupCtx, preFloor); aerr != nil {
			t.Fatalf("apply floor pre-resume: %v", aerr)
		}
		time.Sleep(2 * time.Second) // let tc engage before the first fetch
		if rerr := appium.ResumePlayback(setupCtx, *picked); rerr != nil {
			t.Fatalf("ResumePlayback: %v", rerr)
		}
		if herr := s.WaitForHeartbeat(setupCtx, 90*time.Second); herr != nil {
			t.Fatalf("WaitForHeartbeat: %v", herr)
		}
		sess = s
		t.Cleanup(func() {
			cleanCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			if cerr := sess.ClearShape(cleanCtx); cerr != nil {
				t.Logf("clear shape: %v", cerr)
			}
			if cerr := launcher.Close(); cerr != nil {
				t.Logf("close launcher: %v", cerr)
			}
		})
	case conservativeStart:
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer setupCancel()
		s, lerr := appium.LaunchToHome(setupCtx, *picked)
		if lerr != nil {
			t.Fatalf("LaunchToHome: %v", lerr)
		}
		pid, perr := appium.ReadPlayerID(setupCtx, s)
		if perr != nil {
			t.Fatalf("ReadPlayerID: %v (iOS app may predate the home-player-id AX node; rebuild app in Xcode)", perr)
		}
		s.PlayerID = pid
		t.Logf("parked on home — discovered player_id %s via AX node", pid)
		if aerr := s.ApplyRate(setupCtx, conservativeWarmupCap); aerr != nil {
			t.Fatalf("apply conservative cap: %v", aerr)
		}
		t.Logf("applied conservative %.3f Mbps cap BEFORE resume playback", conservativeWarmupCap)
		time.Sleep(2 * time.Second)
		if rerr := appium.ResumePlayback(setupCtx, *picked); rerr != nil {
			t.Fatalf("ResumePlayback: %v", rerr)
		}
		if herr := s.WaitForHeartbeat(setupCtx, 90*time.Second); herr != nil {
			t.Fatalf("WaitForHeartbeat: %v", herr)
		}
		sess = s
		t.Cleanup(func() {
			cleanCtx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			if cerr := sess.ClearShape(cleanCtx); cerr != nil {
				t.Logf("clear shape: %v", cerr)
			}
			if cerr := launcher.Close(); cerr != nil {
				t.Logf("close launcher: %v", cerr)
			}
		})
	default:
		t.Logf("cold-start unavailable (non-Appium launcher) — using legacy warmup path (pyramid step 1 will cliff)")
		sess = OpenSession(t, p)
	}

	// --- labels + play_id ---------------------------------------
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

	// --- variant ladder (post-launch — definitive) --------------
	switch {
	case conservativeStart:
		// Player came up under the conservative cap; give the manifest a
		// moment to land in the player's session record.
		t.Logf("waiting %s for manifest to populate under conservative cap", rampdownWarmupHold)
		if err := holdContext(ctx, rampdownWarmupHold); err != nil {
			t.Fatalf("manifest-fetch hold: %v", err)
		}
	case !coldStart:
		// Legacy fallback (non-Appium): 100 Mbps warmup so the manifest
		// fetches; step 1 will cliff.
		if err := sess.ApplyRate(ctx, rampdownWarmupMbps); err != nil {
			t.Fatalf("warmup apply: %v", err)
		}
		t.Logf("warmup: %d Mbps × %s", rampdownWarmupMbps, rampdownWarmupHold)
		if err := holdContext(ctx, rampdownWarmupHold); err != nil {
			t.Fatalf("warmup hold: %v", err)
		}
	}

	rec, err := sess.PlayerState(ctx)
	if err != nil {
		t.Fatalf("PlayerState: %v", err)
	}
	desc, err := runner.StandardLadderRates(rec)
	if err != nil {
		t.Fatalf("StandardLadderRates: %v", err)
	}
	if len(desc) == 0 {
		t.Fatal("StandardLadderRates returned no entries")
	}

	// #632: end the sweep ON the lowest variant without stalling. The floor
	// is the bottom variant's PEAK anchor (peak × bump, e.g. 360p
	// 0.998 ×1.05 ≈ 1.05 Mbps): below the next variant's peak so the player
	// is forced down to the bottom variant, yet above the bottom variant's
	// own peak so it stays sustainable (no underrun). The player cold-starts
	// at this floor (above) so the ascent BEGINS on the bottom variant with
	// no cliff. The bottom variant's avg rung and the sub-peak fills below it
	// are dropped — a cap under the bottom variant's peak can't be delivered
	// and would stall.
	floor := pyramidFloorFrom(desc)
	asc := make([]runner.VariantRate, 0, len(desc))
	for _, v := range desc {
		if v.CapMbps+1e-9 < floor {
			continue // drop rungs below the bottom variant's peak (stall risk)
		}
		asc = append(asc, v)
	}
	// desc is descending; reverse so asc runs low → high, starting at floor.
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
		t.Logf("  [%2d] %s %-10s  cap=%6.3f Mbps   avg=%.3f peak=%.3f Mbps  (source=%s)",
			i, phase, v.Resolution, v.CapMbps,
			float64(v.AvgBps)/1_000_000, float64(v.PeakBps)/1_000_000, v.Source)
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
		// #632: only an ACTUAL stall fails a non-bottom rung. A transient
		// min_buf=0 with stalls=0 is the expected dip-and-recover when the
		// player upshifts onto a thin-margin peak rung (+5% over the
		// variant's peak): it drains the buffer fetching the bigger
		// segments, then refills without ever underrunning. Climbing from
		// the 360p floor exposes this (modest buffers); a warm 4K start
		// masked it. We tolerate the dip and key on real stalls.
		if st.StallsDelta == 0 {
			continue
		}
		upperFailures = append(upperFailures, fmt.Sprintf(
			"step %d cap=%.3f Mbps %s/%+d%%: stalls=%d min_buf=%.1fs",
			i+1, st.RateMbps, st.Variant.Resolution, st.Variant.MarginPct,
			st.StallsDelta, st.MinBufferS))
	}
	if len(upperFailures) > 0 {
		t.Errorf("player stalled at %d non-bottom variant(s):\n  %s",
			len(upperFailures), joinLines(upperFailures))
	}
}
