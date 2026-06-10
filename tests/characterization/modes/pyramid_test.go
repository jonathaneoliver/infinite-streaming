package modes

import (
	"context"
	"fmt"
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
	// Resolve the fleet roster (1 device by default, N under CHAR_FLEET_*).
	devs := resolveFleet(t, p)
	switch len(devs) {
	case 0:
		return // resolveFleet already issued a Skip
	case 1:
		runPyramidOnDevice(t, p, devs[0], nil) // today's single-device path, unchanged
		return
	}
	// Fleet: run each sim as a parallel subtest. Each gets its own launcher
	// (see runPyramidOnDevice) so the per-sim player_id can't race. Two sync
	// points (sims get ready at different times; the test fires together):
	//   - home  → every sim waits at the home screen, then all start playback
	//             at the same instant.
	//   - sweep → every sim is warmed up + ladder-read, then all begin the
	//             pyramid shaping at the same instant.
	bars := newFleetBarriers(len(devs))
	for _, dev := range devs {
		dev := dev
		name := dev.Label
		if name == "" {
			name = dev.UDID
		}
		if name == "" {
			name = fmt.Sprintf("fleet%d", dev.FleetIndex)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runPyramidOnDevice(t, p, dev, bars)
		})
	}
}

// runPyramidOnDevice runs the pyramid sweep against ONE explicit device.
// It calls PickMode itself so every parallel fleet subtest gets its OWN
// AppiumLauncher: wireConfigOnConnect stores the per-sim player_id in the
// launcher's launchArgs, so a shared launcher under t.Parallel() would let
// one sim clobber another's identity before createSession reads it.
// dev.FleetIndex rides into appiumCapabilities to pin a distinct
// wdaLocalPort/mjpegServerPort (default index 0 → 8100/9100, unchanged).
func runPyramidOnDevice(t *testing.T, p runner.Platform, dev runner.Device, bars *fleetBarriers) {
	// --- pick launcher (own instance per subtest) ----------------
	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	t.Logf("launch mode: %s", mode)
	picked := &dev
	t.Logf("device: %s (fleet index %d)", picked, dev.FleetIndex)

	// If this sim dies before reaching a barrier, drop it from that barrier's
	// expected set so the survivors still release together instead of each
	// timing out individually. Two flags = two sync points (home, sweep).
	homeArrived, sweepArrived := false, false
	if bars != nil {
		defer func() {
			if !homeArrived {
				bars.home.giveUp()
			}
			if !sweepArrived {
				bars.sweep.giveUp()
			}
		}()
	}

	// Spread parallel fleet launches so N sims don't cold-build WDA at once
	// (no-op for index 0 / single-device runs).
	staggerFleetLaunch(t, dev.FleetIndex)

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
		// conservativeStart dance.
		pid := runner.NewPlayerID()
		// Fleet runs need a generous setup window: a sim that reaches home
		// early holds at the home barrier until the last (most-staggered) sim
		// gets there, which can be a couple of minutes. Single runs keep 2m.
		setupTimeout := 2 * time.Minute
		if bars != nil {
			setupTimeout = 12 * time.Minute
		}
		setupCtx, setupCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer setupCancel()
		floor := resolveFloor(setupCtx, t, preFloor, pyramidFloorFrom)
		wireConfigOnConnect(setupCtx, t, appium, cfg.launchArgs(), pid, floor)

		s, lerr := appium.LaunchToHome(setupCtx, *picked)
		if lerr != nil {
			t.Fatalf("LaunchToHome: %v", lerr)
		}
		s.PlayerID = pid

		// Fleet sync #1 (home): this sim is at the home screen, not yet
		// playing. Hold here until every sim is at home, then all tap play at
		// the same instant — playback starts together across the fleet.
		if bars != nil {
			homeArrived = true
			t.Logf("at home — waiting at fleet HOME barrier (playback starts together)")
			bars.home.arriveAndWait(setupCtx)
			t.Logf("HOME barrier released — starting playback")
		}

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
			// #714: free the session slot immediately (config-on-connect mints a
			// fresh player_id per run; don't wait for the 5-min reaper).
			if cerr := sess.Release(cleanCtx); cerr != nil {
				t.Logf("release session: %v", cerr)
			}
			if cerr := launcher.Close(); cerr != nil {
				t.Logf("close launcher: %v", cerr)
			}
		})
	} else {
		t.Logf("config-on-connect unavailable (non-Appium launcher) — legacy warmup path (pyramid step 1 will cliff)")
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
		"test":     "pyramid",
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

	// Pyramid is ~2× the steps of rampup → bump the overall budget so
	// the per-test deadline isn't tight when most steps don't early-exit.
	overall := rampdownWarmupHold + 120*time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	// --- variant ladder (post-launch — definitive) --------------
	if isAppium {
		// Player came up under the config-on-connect cap; give the manifest a
		// moment to land in the player's session record before reading the ladder.
		t.Logf("waiting %s for manifest to populate under config-on-connect cap", rampdownWarmupHold)
		if err := holdContext(ctx, rampdownWarmupHold); err != nil {
			t.Fatalf("manifest-fetch hold: %v", err)
		}
	} else {
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

	// Fleet sync point: this sim is fully ready (bound, warmed up, ladder
	// known). Hold here until every fleet member is ready too, so the actual
	// streaming + shaping sweep starts on all sims at the same instant. Bounded
	// so a sim that failed bring-up can't hang the rest. No-op for single runs.
	// Register this sim as a prospective group member before the barrier so the
	// leader sees every player_id once the fleet is assembled (#fleet group).
	if bars != nil {
		bars.registerPlayer(sess.PlayerID)
	}

	// Fleet sync #2 (sweep): this sim is warmed up with its ladder read. Hold
	// until every sim is here, then all begin the pyramid shaping at once.
	if bars != nil {
		sweepArrived = true
		t.Logf("ready — waiting at fleet SWEEP barrier (shaping starts together across the fleet)")
		bctx, bcancel := context.WithTimeout(ctx, 5*time.Minute)
		bars.sweep.arriveAndWait(bctx)
		bcancel()
		t.Logf("SWEEP barrier released — beginning synchronized sweep")
	}

	// Group mode (CHAR_FLEET_GROUP=1): drive ONE pyramid for the whole fleet.
	// The leader creates the player-group and runs the sweep below — every
	// ApplyRate is broadcast by the proxy to all members, so the shaping lands
	// identically on every sim. Observers don't drive (that would collide);
	// they hold playback under the broadcast until the leader is done. Their
	// samples are archived + grouped for side-by-side comparison in the
	// dashboard (#579 compare-charts).
	if bars != nil && bars.group {
		if dev.FleetIndex != 0 {
			t.Logf("group observer — holding playback under the leader's broadcast pyramid (compare in dashboard)")
			bars.waitSweepDone(ctx)
			t.Logf("leader finished — observer done")
			return
		}
		// Release the observers when the leader returns — registered FIRST so a
		// failure in CreateGroup (or the sweep) can't leave them blocked.
		defer bars.signalSweepDone()

		gctx, gcancel := context.WithTimeout(ctx, 30*time.Second)
		members := bars.members()
		groupID, gerr := runner.CreateGroup(gctx, "pyramid-"+runID, members)
		gcancel()
		if gerr != nil {
			t.Fatalf("create fleet group: %v", gerr)
		}
		t.Logf("created fleet group %s (%d members) — leader drives one broadcast pyramid", groupID, len(members))
		t.Cleanup(func() {
			cctx, c := context.WithTimeout(context.Background(), 10*time.Second)
			defer c()
			if err := runner.DisbandGroup(cctx, groupID); err != nil {
				t.Logf("disband group: %v", err)
			}
		})
	}

	// #cycles — run the full up-then-down pyramid REPS times on the SAME
	// live play (default 2, CHAR_PYRAMID_REPS). Between cycles the cap
	// returns floor→floor; the player carries its state across, so the
	// second traverse shows whether the climb/descent behaviour is stable
	// run-to-run (n=1 isn't a pattern).
	reps := envInt("CHAR_PYRAMID_REPS", 2)
	reports := RunCycledVariantSweep(ctx, t, sess, "pyramid", steps, reps,
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
		base := fmt.Sprintf("pyramid-%s-%s%s-%s-cyc%d", p, playerShort, segTag, runID, cyc)
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

		// Pass criteria — same shape as rampdown/rampup.
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
}
