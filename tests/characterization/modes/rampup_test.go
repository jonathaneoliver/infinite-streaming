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

// Rampup — variant-aware ascending sweep.
//
// Bootstrap (Option A): before any kill+launch, read the current
// player record's manifest variants from the harness. Compute the
// floor cap from those variants. THEN (for Appium) kill+launch the
// app, park on home, apply the floor cap, resume playback. The player
// starts cold under throttle — no cliff from "unconstrained warmup"
// to "constrained sweep". For non-Appium launchers we fall back to
// the legacy warmup-then-sweep path which has a brief cliff at step 1.
//
// The ladder is the shared dual-rung (avg+peak) + geometrically-filled
// limit ladder (runner.StandardLadderRates); rampup walks it ascending
// from the floor.
//
// rampup deliberately skips the bottom variant — testing caps at
// the bottom variant's range is rampdown's territory ("how low can we
// go"). Rampup tests "given a constrained start, when does the player
// climb?" — the floor for that is the second-from-bottom variant's
// lowest surviving cap in the filled ladder.
//
// conservativeWarmupCap is the rate cap we apply when bootstrap can't
// find a previous heartbeating player (typically right after a wedge
// purged the proxy's player record). 1.5 Mbps is well above any
// realistic 360p variant rate and below most 540p rates — the player
// will pick the bottom variant on cold start, fetch the manifest, then
// we adjust to the real floor before the sweep.
const conservativeWarmupCap = 1.5

func TestRampupIPadSim(t *testing.T)   { runRampup(t, runner.PlatformIPadSim) }
func TestRampupIPhone(t *testing.T)    { runRampup(t, runner.PlatformIPhone) }
func TestRampupAppleTV(t *testing.T)   { runRampup(t, runner.PlatformAppleTV) }
func TestRampupAndroidTV(t *testing.T) { runRampup(t, runner.PlatformAndroidTV) }
func TestRampupWeb(t *testing.T)       { runRampup(t, runner.PlatformWeb) }

func runRampup(t *testing.T, p runner.Platform) {
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

	// --- bootstrap: read manifest BEFORE kill+launch -------------
	bootCtx, bootCancel := context.WithTimeout(context.Background(), 15*time.Second)
	preRec, preErr := runner.PreLaunchInfo(bootCtx, *picked)
	bootCancel()

	var preDesc []runner.VariantRate
	var preFloor float64
	if preErr != nil {
		t.Logf("pre-launch info: %v (cold-start unavailable; will use warmup-then-adjust path)", preErr)
	} else if preRec.CurrentPlay == nil || len(preRec.CurrentPlay.Manifest.Variants) == 0 {
		t.Logf("pre-launch: no current play / no variants yet (cold-start unavailable)")
	} else {
		preDesc, err = runner.StandardLadderRates(preRec)
		if err != nil {
			t.Logf("pre-launch StandardLadderRates: %v (cold-start unavailable)", err)
		} else {
			preFloor = rampupFloorFrom(preDesc)
			t.Logf("bootstrap: pre-launch floor = %.3f Mbps (from current player's manifest)", preFloor)
		}
	}

	// --- open session ----------------------------------------------
	// Three paths:
	//   (1) Appium + bootstrap succeeded → cold-start at the true floor
	//       (`preFloor`). Most accurate; matches the operational
	//       intent of "player launches under the constraint".
	//   (2) Appium + bootstrap failed → cold-start at a fixed
	//       conservative cap (1.5 Mbps), then read the real manifest
	//       once the player is running, then adjust to the true floor
	//       before the sweep. No cliff; player picks the bottom variant
	//       on cold start. Recovers from "previous wedge purged the
	//       player record".
	//   (3) Non-Appium → fall back to OpenSession + 100 Mbps warmup.
	//       Cliff at sweep step 1 is inevitable; logged loudly.
	appium, isAppium := launcher.(*runner.AppiumLauncher)
	coldStart := isAppium && preFloor > 0
	conservativeStart := isAppium && !coldStart

	var sess *runner.Session
	switch {
	case coldStart:
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer setupCancel()
		s, err := appium.LaunchToHome(setupCtx, *picked)
		if err != nil {
			t.Fatalf("LaunchToHome: %v", err)
		}
		s.PlayerID = preRec.ID
		t.Logf("parked on home; applying floor %.3f Mbps before resuming playback", preFloor)
		if err := s.ApplyRate(setupCtx, preFloor); err != nil {
			t.Fatalf("apply floor pre-resume: %v", err)
		}
		// Settle gap: PATCH /shape returns 200 OK once the proxy accepts
		// the change, but tc rule installation happens slightly later.
		// Without this, ResumePlayback can fire before tc has engaged.
		time.Sleep(2 * time.Second)
		if err := appium.ResumePlayback(setupCtx, *picked); err != nil {
			t.Fatalf("ResumePlayback: %v", err)
		}
		if err := s.WaitForHeartbeat(setupCtx, 90*time.Second); err != nil {
			t.Fatalf("WaitForHeartbeat: %v", err)
		}
		sess = s
		t.Cleanup(func() {
			cleanCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
			defer c()
			if err := sess.ClearShape(cleanCtx); err != nil {
				t.Logf("clear shape: %v", err)
			}
			// #627: close the play via the app's own UI so it emits a real
			// client play_end (cleanly ended in the sessions view), before
			// Close() tears the Appium session down.
			if err := sess.CloseViaUI(cleanCtx); err != nil {
				t.Logf("close playback via UI: %v", err)
			}
			if err := launcher.Close(); err != nil {
				t.Logf("close launcher: %v", err)
			}
		})

	case conservativeStart:
		// Cold-start at a fixed-conservative cap so the player picks the
		// bottom variant from segment 1. The iOS app surfaces its
		// persistent player_id on the Home screen via an accessibility
		// node (home-player-id) — we read it BEFORE tapping Continue
		// Watching and apply the cap pre-playback. That closes the
		// race that wedged step 1 under the old WaitForBind-then-apply
		// flow. See plan: ~/.claude/plans/cold-start-shape-cap-race-fix.md.
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer setupCancel()
		s, err := appium.LaunchToHome(setupCtx, *picked)
		if err != nil {
			t.Fatalf("LaunchToHome: %v", err)
		}
		pid, err := appium.ReadPlayerID(setupCtx, s)
		if err != nil {
			t.Fatalf("ReadPlayerID: %v (iOS app may predate the home-player-id AX node; rebuild app in Xcode)", err)
		}
		s.PlayerID = pid
		t.Logf("parked on home — discovered player_id %s via AX node", pid)
		if err := s.ApplyRate(setupCtx, conservativeWarmupCap); err != nil {
			t.Fatalf("apply conservative cap: %v", err)
		}
		t.Logf("applied conservative %.3f Mbps cap BEFORE resume playback", conservativeWarmupCap)
		// Settle gap so the kernel tc/nftables rule is installed before
		// the first manifest fetch — matches the coldStart branch.
		time.Sleep(2 * time.Second)
		if err := appium.ResumePlayback(setupCtx, *picked); err != nil {
			t.Fatalf("ResumePlayback: %v", err)
		}
		if err := s.WaitForHeartbeat(setupCtx, 90*time.Second); err != nil {
			t.Fatalf("WaitForHeartbeat: %v", err)
		}
		sess = s
		t.Cleanup(func() {
			cleanCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
			defer c()
			if err := sess.ClearShape(cleanCtx); err != nil {
				t.Logf("clear shape: %v", err)
			}
			// #627: close the play via the app's own UI so it emits a real
			// client play_end (cleanly ended in the sessions view), before
			// Close() tears the Appium session down.
			if err := sess.CloseViaUI(cleanCtx); err != nil {
				t.Logf("close playback via UI: %v", err)
			}
			if err := launcher.Close(); err != nil {
				t.Logf("close launcher: %v", err)
			}
		})

	default:
		// Non-Appium fallback: OpenSession + legacy 100 Mbps warmup,
		// step 1 will cliff.
		t.Logf("cold-start unavailable (non-Appium launcher) — using legacy warmup path (rampup step 1 will cliff)")
		sess = OpenSession(t, p)
	}

	// --- labels + play_id ---------------------------------------
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

	// --- variant ladder (post-launch — definitive) --------------
	// coldStart: we already have preDesc; re-read for confirmation.
	// conservativeStart: player just came up under 1.5 Mbps cap, manifest
	//                    is being fetched; brief wait then read.
	// default (non-Appium): legacy 100 Mbps warmup so manifest fetches.
	switch {
	case conservativeStart:
		// Player is running under the conservative cap. Give the
		// manifest a moment to land in the player's session record.
		t.Logf("waiting %s for manifest to populate under conservative cap", rampdownWarmupHold)
		if err := holdContext(ctx, rampdownWarmupHold); err != nil {
			t.Fatalf("manifest-fetch hold: %v", err)
		}
	case !coldStart:
		// Legacy fallback (non-Appium): 100 Mbps warmup so the player
		// fetches the manifest fresh. Step 1 will cliff afterward.
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

	// --- ascending step list ------------------------------------
	// Skip the bottom variant entirely — its cap range is rampdown's
	// territory. Rampup starts at the second-from-bottom variant's
	// lowest surviving margin.
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
		floor = asc[len(asc)-1].CapMbps // last in desc-order = lowest = floor
	}
	// reverse to ascending
	for i, j := 0, len(asc)-1; i < j; i, j = i+1, j-1 {
		asc[i], asc[j] = asc[j], asc[i]
	}

	t.Logf("sweep plan: %d rungs ascending from %.3f Mbps (skipping bottom variant — rampdown's territory)",
		len(asc), floor)
	for i, v := range asc {
		t.Logf("  [%2d] %-10s  cap=%6.3f Mbps   avg=%.3f peak=%.3f Mbps  (source=%s)",
			i, v.Resolution, v.CapMbps,
			float64(v.AvgBps)/1_000_000, float64(v.PeakBps)/1_000_000, v.Source)
	}

	steps := make([]runner.Step, len(asc))
	for i, v := range asc {
		v := v
		steps[i] = runner.Step{RateMbps: v.CapMbps, Hold: rampdownMaxHold, Variant: &v}
	}

	report := RunVariantSweep(ctx, t, sess, "rampup", steps, time.Second,
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
			"step %d cap=%.3f Mbps %s/%s: stalls=%d min_buf=%.1fs",
			i+1, st.RateMbps, st.Variant.Resolution, st.Variant.Source,
			st.StallsDelta, st.MinBufferS))
	}
	if len(upperFailures) > 0 {
		t.Errorf("player stalled / depleted buffer at %d non-bottom variant(s):\n  %s",
			len(upperFailures), joinLines(upperFailures))
	}

	// Quiet the linter — preDesc is only used to compute preFloor
	// above when bootstrap succeeds.
	_ = preDesc
}

// rampupFloorFrom returns the lowest cap from a desc-order limit ladder,
// skipping the bottom variant entirely (its cap range is rampdown's
// territory). Floor = the second-from-bottom variant's lowest cap.
func rampupFloorFrom(desc []runner.VariantRate) float64 {
	if len(desc) == 0 {
		return 0
	}
	bottomRes := desc[len(desc)-1].Resolution
	floor := 0.0
	for _, v := range desc {
		if v.Resolution == bottomRes {
			continue // skip bottom variant entirely
		}
		floor = v.CapMbps // sweep is desc-sorted; last surviving = lowest
	}
	return floor
}
