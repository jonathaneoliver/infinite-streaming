package modes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Transient-shock — graduated-drop staircase: warm up UNCAPPED to the top
// variant (4K), then build a full 4K buffer under a high cap (headroom ABOVE
// the 4K peak) and drop to a sequence of escalating depths — the peak cap (×1.05 TCP
// overhead) of the published variant 3/4, 1/2, 1/4 up from the bottom, and
// the bottom variant — returning to the high cap between drops so the
// buffer refills. Finds WHERE the player breaks, not just the worst case.
//
// This is the purpose-built probe for the sudden-drop wedge: a hard drop
// while the player is warm on 4K is exactly the transition that strands
// AVPlayer (segment fetches time out at the live-edge 2s/5s limits, the
// connection can go dead, the player attempts a big downshift and can hit
// CoreMedia -12880 "Can not proceed after removing variants"). Pair the run
// with the network-request view (proxy→client delivery + the dead window)
// and the AVMetrics feed (CoreMedia errors + variant-switch start/complete)
// to see whether the link survives the drop and whether the player recovers.
//
// Shares most of rampup's machinery: the run-config axes (CHAR_SEGMENT /
// CHAR_LOCAL_PROXY / CHAR_TRANSFER_TIMEOUT) forced at launch + applied
// server-side, play labels, and UI-close teardown. It deliberately does NOT
// cold-start at a floor (rampup/pyramid do) — it warms up uncapped so the
// player reaches 4K before the first shock.
//
// Env axes (in addition to the shared CHAR_SEGMENT / CHAR_LOCAL_PROXY /
// CHAR_TRANSFER_TIMEOUT):
//
//	CHAR_SHOCK_HIGH_MBPS   build/recover cap, headroom above 4K peak (default 60)
//	CHAR_SHOCK_HIGH_HOLD_S hold at the high cap, build + recovery (default 120)
//	CHAR_SHOCK_DIP_HOLD_S  hold at each drop (default 90; must outlast the
//	                       buffer coast + wedge-cycle to see the outcome)

func TestTransientShockIPadSim(t *testing.T)   { runTransientShock(t, runner.PlatformIPadSim) }
func TestTransientShockIPhone(t *testing.T)    { runTransientShock(t, runner.PlatformIPhone) }
func TestTransientShockAppleTV(t *testing.T)   { runTransientShock(t, runner.PlatformAppleTV) }
func TestTransientShockAndroidTV(t *testing.T) { runTransientShock(t, runner.PlatformAndroidTV) }
func TestTransientShockWeb(t *testing.T)       { runTransientShock(t, runner.PlatformWeb) }

func runTransientShock(t *testing.T, p runner.Platform) {
	// Fleet dispatch (1 device by default, N under CHAR_FLEET_*). runFleet
	// resolves the roster and either runs the single-device path unchanged or
	// fans out one parallel subtest per device with the home + sweep barriers
	// wired (see runTransientShockOnDevice). Under CHAR_FLEET_GROUP=1 the leader
	// drives one broadcast staircase for the whole group (like pyramid).
	runFleet(t, p, runTransientShockOnDevice)
}

// runTransientShockOnDevice runs the graduated-drop staircase against ONE
// explicit device. Like runPyramidOnDevice it mints its OWN launcher and uses
// the PASSED dev; when bars != nil it syncs at the home barrier (all sims start
// playback together) and the sweep barrier (all start shaping together).
//
// UNLIKE rampup/pyramid, transient-shock does NOT prime a cold-start floor: the
// probe must warm to the top variant (4K) UNCAPPED before the first shock, so
// there's a full forward buffer to drop FROM. We still mint a per-sim player_id
// and set the launch args (incl. the per-sim WDA ports via dev.FleetIndex) and
// bind the session — just with no config-on-connect rate curl and no peak clamp.
func runTransientShockOnDevice(t *testing.T, p runner.Platform, dev runner.Device, bars *fleetBarriers) {
	// --- pick launcher (own instance per subtest) ----------------
	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	t.Logf("launch mode: %s", mode)
	picked := &dev
	t.Logf("device: %s (fleet index %d)", picked, dev.FleetIndex)

	// If this sim dies before reaching a barrier, drop it from that barrier's
	// expected set so the survivors still release together (home, sweep).
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

	appium, isAppium := launcher.(*runner.AppiumLauncher)

	// #config — read the per-run configuration axes (segment, LocalProxy,
	// transfer-timeout). The launch-arg ones plus the minted -is.player_id are
	// forced on the cold launch in the Appium block below.
	cfg := readRunConfig(t, isAppium)
	segment := cfg.segment

	var sess *runner.Session
	if isAppium {
		// #714: mint a per-sim player_id and force it onto the cold launch via
		// the launch args — but DO NOT prime a config-on-connect rate cap. This
		// probe must warm to its top variant (4K) UNCAPPED before the first
		// shock (so it has a full forward buffer to drop FROM); a floor prime
		// would leave it pinned low. We wire the launch args (segment + the
		// minted -is.player_id) directly, skipping the rate curl / peak clamp
		// that rampup/pyramid use. The per-sim WDA ports come from
		// dev.FleetIndex flowing into the Appium caps.
		pid := runner.NewPlayerID()
		// Generous budget — Android adds a catalogue-load settle on top of
		// launch; fleet runs also hold at the home barrier until the last sim.
		setupTimeout := 4 * time.Minute
		if bars != nil {
			setupTimeout = 12 * time.Minute
		}
		setupCtx, setupCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer setupCancel()
		// No rate prime (uncapped) — this probe must warm to its top variant (4K)
		// before the first shock. When grouping, born-group the session via a
		// config-on-connect carrying ONLY player_id + group_id (capMbps=0 ⇒ no
		// shape), so the fleet is grouped at connect yet still uncapped. Player_id
		// stays a clean UUID. Otherwise a bare launch (no pre-created session).
		if gid := bars.fleetGroupID(); gid != "" {
			wireConfigOnConnect(setupCtx, t, appium, cfg.launchArgs(), pid, 0, 0, 0, gid, true, nil)
			t.Logf("transient-shock: born-grouped (group=%s), uncapped — player_id=%s", gid, pid)
		} else {
			launchArgs := append(append([]string{}, cfg.launchArgs()...), "-is.player_id", pid)
			appium.SetLaunchArgs(launchArgs)
			t.Logf("transient-shock: bare launch (no rate prime) — player_id=%s, args=%v", pid, launchArgs)
		}

		s, lerr := appium.LaunchToHome(setupCtx, *picked)
		if lerr != nil {
			t.Fatalf("LaunchToHome: %v", lerr)
		}
		s.PlayerID = pid

		// Fleet sync #1 (home): this sim is at home, not yet playing. Hold
		// until every sim is at home, then all start playback at once.
		if bars != nil {
			homeArrived = true
			t.Logf("at home — waiting at fleet HOME barrier (playback starts together)")
			bars.home.arriveAndWait(setupCtx)
			t.Logf("HOME barrier released — starting playback")
		}
		// Survive transient content-startup: right after a cold launch the
		// catalogue can be briefly empty ("No content available"), so tapping
		// Resume starts nothing. Retry resume → heartbeat until the play comes up.
		var herr error
		for attempt := 1; attempt <= 3; attempt++ {
			if rerr := appium.ResumePlayback(setupCtx, *picked); rerr != nil {
				t.Logf("resume attempt %d: %v", attempt, rerr)
			}
			if herr = s.WaitForHeartbeat(setupCtx, 50*time.Second); herr == nil {
				break
			}
			t.Logf("no heartbeat after resume attempt %d — content may still be loading; retrying", attempt)
		}
		if herr != nil {
			t.Fatalf("WaitForHeartbeat after resume retries: %v", herr)
		}
		sess = s
		t.Cleanup(func() {
			cleanCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
			defer c()
			if cerr := sess.ClearShape(cleanCtx); cerr != nil {
				t.Logf("clear shape: %v", cerr)
			}
			if cerr := sess.CloseViaUI(cleanCtx); cerr != nil {
				t.Logf("close playback via UI: %v", cerr)
			}
			// #714: free the session slot immediately (don't wait for the 5-min
			// reaper) — config-on-connect mints a fresh player_id per run.
			if cerr := sess.Release(cleanCtx); cerr != nil {
				t.Logf("release session: %v", cerr)
			}
			if cerr := launcher.Close(); cerr != nil {
				t.Logf("close launcher: %v", cerr)
			}
			if cerr := sess.ReleaseDevice(cleanCtx); cerr != nil {
				t.Logf("release device: %v", cerr)
			}
		})
	} else {
		t.Logf("config-on-connect unavailable (non-Appium launcher) — legacy warmup path")
		sess = OpenSession(t, p)
	}

	// #config — arm the server-side active transfer timeout for this run.
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
		"test":     "transient_shock",
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
	if isAppium {
		t.Logf("waiting %s for manifest to populate (uncapped warmup — no cold-start floor)", rampdownWarmupHold)
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
	// #segments — assert the cold start landed on the requested segment.
	if segment != "" && rec.CurrentPlay != nil {
		master := rec.CurrentPlay.Manifest.MasterURL
		want := "master_" + segment + "."
		if segment == "ll" {
			want = "master.m3u8"
		}
		if !strings.Contains(master, want) {
			t.Fatalf("requested segment %q but cold-started on %q (expected master to contain %q)",
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

	// --- graduated-drop staircase -------------------------------
	// `high` is a cap with headroom ABOVE the top variant's peak (default
	// 60 Mbps) so the player builds a FULL forward buffer at 4K before each
	// drop — at exactly the 4K peak there's no spare bandwidth to buffer
	// ahead. Then we drop to a sequence of escalating depths, returning to
	// `high` between each so the buffer refills.
	high := envFloat("CHAR_SHOCK_HIGH_MBPS", 60.0)
	highHold := time.Duration(envInt("CHAR_SHOCK_HIGH_HOLD_S", 120)) * time.Second
	// The dip must outlast the buffer coast (the player plays out its
	// buffered high-variant content before it's really at the low rate)
	// PLUS the wedge-or-recover resolution (~54s). 90s captures the full
	// outcome — reset, or a downshift that sustains.
	dipHold := time.Duration(envInt("CHAR_SHOCK_DIP_HOLD_S", 90)) * time.Second

	// Drop targets are PUBLISHED-VARIANT peak caps (peak × the standard 5%
	// TCP-overhead bump — the StandardLadder "peak" rung's CapMbps), picked
	// by position in the ladder: the variant 3/4, 1/2, 1/4 up from the
	// bottom, and the bottom variant. Escalating downshift depth to real
	// renditions finds WHERE the player breaks.
	peaks := variantPeakCapsAsc(desc)
	if len(peaks) == 0 {
		t.Fatal("no published-variant peak rungs in the ladder")
	}
	drops := pickVariantFractions(peaks, []float64{0.75, 0.5, 0.25, 0.0})

	// build → drop → build → drop → ... → build (final recover)
	steps := make([]runner.Step, 0, 2*len(drops)+1)
	for _, d := range drops {
		steps = append(steps,
			runner.Step{RateMbps: high, Hold: highHold},
			runner.Step{RateMbps: d, Hold: dipHold})
	}
	steps = append(steps, runner.Step{RateMbps: high, Hold: highHold})

	t.Logf("staircase: build@%.1f Mbps (%s); drops (variant peaks ×1.05, 3/4·1/2·1/4·bottom)=%.3f Mbps (hold %s each)",
		high, highHold, drops, dipHold)

	// Fleet sync #2 (sweep): this sim is warmed up with its ladder read. Hold
	// until every sim is here, then all begin the staircase shaping at once.
	// Bounded so a sim that failed bring-up can't hang the rest. No-op single.
	if bars != nil {
		sweepArrived = true
		t.Logf("ready — waiting at fleet SWEEP barrier (shaping starts together across the fleet)")
		bctx, bcancel := context.WithTimeout(ctx, 5*time.Minute)
		bars.sweep.arriveAndWait(bctx)
		bcancel()
		t.Logf("SWEEP barrier released — beginning synchronized sweep")
	}

	// Group mode (CHAR_FLEET_GROUP=1): drive ONE staircase for the whole fleet.
	// The leader (index 0) creates the player-group and runs the sweep below —
	// every ApplyRate is broadcast by the proxy to all members, so the shock
	// lands identically on every sim. Observers don't drive (that would collide);
	// they hold playback under the broadcast until the leader is done. Their
	// samples are archived + grouped for side-by-side comparison in the
	// dashboard (#579 compare-charts).
	if bars != nil && bars.group {
		if dev.FleetIndex != 0 {
			t.Logf("group observer — holding playback under the leader's broadcast staircase (compare in dashboard)")
			bars.waitSweepDone(ctx)
			t.Logf("leader finished — observer done")
			return
		}
		// The proxy already grouped the fleet by the `_G<num>` token in each
		// sim's player_id (born grouped at connect — no CreateGroup race). The
		// leader just drives; the proxy fans every ApplyRate to the group by
		// group_id. signalSweepDone (deferred FIRST) releases the observers when
		// the leader returns, even if the sweep below fails.
		defer bars.signalSweepDone()
		t.Logf("group leader — driving one broadcast staircase for group %s", bars.groupID)
	}

	report := RunSweep(ctx, t, sess, "transient-shock", steps, time.Second)
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
	segTag := ""
	if segment != "" {
		segTag = "-" + segment
	}
	base := fmt.Sprintf("transient-shock-%s-%s%s-%s", p, playerShort, segTag, runID)
	jsonPath, werr := runner.WriteReport(out, base, report)
	if werr != nil {
		t.Fatalf("write report: %v", werr)
	}
	LogReport(t, jsonPath)
	if htmlPath, herr := runner.WriteChart(out, base, report); herr == nil {
		t.Logf("chart: %s", htmlPath)
	} else {
		t.Logf("chart write skipped: %v", herr)
	}

	t.Logf("total stalls: %d / stall seconds: %.1f / profile shifts: %d",
		report.Summary.TotalStalls, report.Summary.TotalStallSeconds, report.Summary.ProfileShifts)

	if report.Summary.SampleCount == 0 {
		t.Errorf("no samples collected")
	}

	if last := report; last != nil {
		endLabels := map[string]string{
			"completed":      time.Now().UTC().Format("20060102T150405Z"),
			"drops":          fmt.Sprintf("%d", len(drops)),
			"total_stalls":   fmt.Sprintf("%d", report.Summary.TotalStalls),
			"profile_shifts": fmt.Sprintf("%d", report.Summary.ProfileShifts),
		}
		if err := sess.LabelPlay(context.Background(), endLabels); err != nil {
			t.Logf("label play (end): %v", err)
		}
	}
}

// variantPeakCapsAsc returns each PUBLISHED variant's peak cap — the
// StandardLadder "peak" rung's CapMbps, which is the variant's peak
// bandwidth × the standard 5% TCP-overhead bump — ordered bottom→top.
// `desc` is top→bottom, so we walk it in reverse and keep one cap per
// variant (the peak rung).
func variantPeakCapsAsc(desc []runner.VariantRate) []float64 {
	var peaks []float64
	for i := len(desc) - 1; i >= 0; i-- {
		if desc[i].Source == "peak" {
			peaks = append(peaks, desc[i].CapMbps)
		}
	}
	return peaks
}

// pickVariantFractions selects the cap of the variant at each fraction `f`
// of the way up from the bottom (0 = bottom, 1 = top): idx = round(f·(N-1)).
// e.g. fracs {0.75, 0.5, 0.25, 0} → the variant 3/4, 1/2, 1/4 up, and the
// bottom variant.
func pickVariantFractions(peaksAsc, fracs []float64) []float64 {
	n := len(peaksAsc)
	out := make([]float64, 0, len(fracs))
	if n == 0 {
		return out
	}
	for _, f := range fracs {
		idx := int(f*float64(n-1) + 0.5)
		if idx < 0 {
			idx = 0
		}
		if idx >= n {
			idx = n - 1
		}
		out = append(out, peaksAsc[idx])
	}
	return out
}
