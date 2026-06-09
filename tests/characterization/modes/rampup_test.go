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

	// #config — read the per-run configuration axes (segment, LocalProxy,
	// transfer-timeout). The launch-arg ones (segment via -is.segment,
	// LocalProxy via -is.flag.local_proxy) plus the minted -is.player_id are
	// forced on the single cold launch in the Appium block below, so the
	// player starts on them from frame 1; iOS folds them into UserDefaults
	// (NSArgumentDomain). The transfer-timeout axis is applied server-side.
	cfg := readRunConfig(t, isAppium)
	segment := cfg.segment

	var sess *runner.Session
	if isAppium {
		// #714 config-on-connect: mint the player_id, resolve the cold-start
		// floor (prior manifest → master parse → conservative), and wire the
		// driver (default = harness pre-configures via a curl, so the variant
		// ladder is known up front; CHAR_PROXY_CONFIG=app → the player emits
		// proxy.* on its own bootstrap URL). Replaces the old coldStart /
		// conservativeStart dance — the app streams under the cap from segment 0.
		pid := runner.NewPlayerID()
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer setupCancel()
		floor := resolveFloor(setupCtx, t, preFloor, rampupFloorFrom)
		wireConfigOnConnect(setupCtx, t, appium, cfg.launchArgs(), pid, floor)

		s, err := appium.LaunchToHome(setupCtx, *picked)
		if err != nil {
			t.Fatalf("LaunchToHome: %v", err)
		}
		s.PlayerID = pid
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
			// #714: free the session slot immediately (don't wait for the 5-min
			// reaper) — config-on-connect mints a fresh player_id per run, so a
			// few back-to-back runs would otherwise exhaust the small pool.
			if err := sess.Release(cleanCtx); err != nil {
				t.Logf("release session: %v", err)
			}
			if err := launcher.Close(); err != nil {
				t.Logf("close launcher: %v", err)
			}
			// #627: opt-in device release (CHAR_RELEASE_DEVICE=1) — clears
			// the iOS "Automation Running" overlay by terminating WDA.
			if err := sess.ReleaseDevice(cleanCtx); err != nil {
				t.Logf("release device: %v", err)
			}
		})
	} else {
		// Non-Appium fallback: legacy warmup path (step 1 cliffs).
		// config-on-connect needs a launch-arg player_id, which only the
		// Appium launch path forces onto the app.
		t.Logf("config-on-connect unavailable (non-Appium launcher) — legacy warmup path (rampup step 1 will cliff)")
		sess = OpenSession(t, p)
	}

	// #config — arm the server-side active transfer timeout for this run
	// (CHAR_TRANSFER_TIMEOUT, default 20s; 0 clears). The proxy then cuts
	// any segment still in flight past the window so the player downshifts
	// instead of stalling — notably at the cyc→floor cap slam. Player is
	// bound + heartbeating here. Cleared at teardown.
	{
		tctx, tcancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := cfg.applyServerSide(tctx, sess); err != nil {
			t.Logf("set transfer timeout: %v (test continues)", err)
		} else {
			t.Logf("transfer timeout: %s on segments", cfg.labels()["xfer_timeout"])
		}
		tcancel()
		t.Cleanup(func() {
			cctx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			if err := sess.SetSegmentTimeout(cctx, 0); err != nil {
				t.Logf("clear transfer timeout: %v", err)
			}
		})
	}

	// --- labels + play_id ---------------------------------------
	runID := time.Now().UTC().Format("20060102T150405Z")
	startLabels := map[string]string{
		"test":     "rampup",
		"platform": string(p),
		"run_id":   runID,
	}
	for k, v := range cfg.labels() {
		startLabels[k] = v
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
	// Appium: the player just came up under the config-on-connect cap; give
	// the manifest a moment to land in the session record before reading the
	// ladder. Non-Appium: legacy 100 Mbps warmup so the manifest fetches
	// fresh (step 1 will cliff afterward).
	if isAppium {
		t.Logf("waiting %s for manifest to populate under config-on-connect cap", rampdownWarmupHold)
		if err := holdContext(ctx, rampdownWarmupHold); err != nil {
			t.Fatalf("manifest-fetch hold: %v", err)
		}
	} else {
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
	// #segments — assert the cold start actually landed on the requested
	// segment. master_2s.m3u8 / master_6s.m3u8 / master.m3u8 (ll). If the
	// segment didn't persist across the relaunch we'd silently sweep the
	// wrong segment — fail loudly instead.
	if segment != "" && rec.CurrentPlay != nil {
		master := rec.CurrentPlay.Manifest.MasterURL
		var want string
		switch segment {
		case "ll":
			want = "master.m3u8"
		default:
			want = "master_" + segment + "."
		}
		if !strings.Contains(master, want) {
			t.Fatalf("requested segment %q but cold-started on %q (expected master to contain %q) — did the segment persist across relaunch?",
				segment, master, want)
		}
		t.Logf("confirmed cold start on segment=%s (master=%s)", segment, master)
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

	// #cycles — run the climb REPS times on the SAME live play (default 3,
	// CHAR_RAMPUP_REPS). Cycle 1 is the cold-start climb set up above; on
	// each subsequent cycle the cap drops top→floor and the player climbs
	// again, carrying its buffer + current variant across the drop. That
	// inter-cycle drop is the instructive transition a single pass misses.
	reps := envInt("CHAR_RAMPUP_REPS", 3)
	reports := RunCycledVariantSweep(ctx, t, sess, "rampup", steps, reps,
		unionRungs(desc), playID, time.Second,
		rampdownMinHold, rampdownMaxHold, rampdownEarlyExitWindow, rampdownEarlyExitTol)

	out := runner.DefaultOutDir(t.TempDir())
	playerShort := sess.PlayerID
	if len(playerShort) > 8 {
		playerShort = playerShort[:8]
	}
	segTag := ""
	if segment != "" {
		segTag = "-" + segment
	}
	var last *runner.Report
	for ri, report := range reports {
		cyc := ri + 1
		last = report
		base := fmt.Sprintf("rampup-%s-%s%s-%s-cyc%d", p, playerShort, segTag, runID, cyc)
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

		t.Logf("cyc%d lowest sustainable cap: %.3f Mbps", cyc, report.Summary.LowestSustainableCapMbps)
		if report.Summary.HighestStallingCapMbps > 0 {
			t.Logf("cyc%d highest stalling cap:   %.3f Mbps", cyc, report.Summary.HighestStallingCapMbps)
		}
		t.Logf("cyc%d total stalls: %d / stall seconds: %.1f / profile shifts: %d",
			cyc, report.Summary.TotalStalls, report.Summary.TotalStallSeconds, report.Summary.ProfileShifts)

		if report.Summary.SampleCount == 0 {
			t.Errorf("cyc%d: no samples collected", cyc)
			continue
		}
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
			// #650 (port of #632): only an ACTUAL stall fails a non-bottom rung.
			// Rampup upshifts onto thin-margin peak rungs (+5% over the variant's
			// peak), where the buffer transiently drains to min_buf=0 fetching the
			// bigger segments and then refills without underrunning — expected ABR
			// behaviour, not a defect. Key on real stalls, not the dip. (rampdown
			// keeps the stricter min_buf check: a descent fills the buffer, so a
			// dip there is genuinely suspicious.)
			if st.StallsDelta == 0 {
				continue
			}
			upperFailures = append(upperFailures, fmt.Sprintf(
				"step %d cap=%.3f Mbps %s/%s: stalls=%d min_buf=%.1fs",
				i+1, st.RateMbps, st.Variant.Resolution, st.Variant.Source,
				st.StallsDelta, st.MinBufferS))
		}
		if len(upperFailures) > 0 {
			t.Errorf("cyc%d: player stalled at %d non-bottom variant(s):\n  %s",
				cyc, len(upperFailures), joinLines(upperFailures))
		}
	}

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
