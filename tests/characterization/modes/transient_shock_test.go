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

// Transient-shock — graduated-drop staircase: cold-start at the floor,
// then build a full 4K buffer under a high cap (headroom ABOVE the 4K peak)
// and drop to a sequence of escalating depths — the peak cap (×1.05 TCP
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
// Shares all of rampup's machinery: cold-start bootstrap, the run-config
// axes (CHAR_SEGMENT / CHAR_LOCAL_PROXY / CHAR_TRANSFER_TIMEOUT) forced at
// launch + applied server-side, play labels, and UI-close teardown.
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

	// --- bootstrap: read manifest BEFORE kill+launch so we cold-start
	// at the floor (clean startup), then climb to the top under the high
	// cap before the first shock. Same machinery rampup uses.
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
		preFloor = rampupFloorFrom(preDesc)
		t.Logf("bootstrap: pre-launch floor = %.3f Mbps (from current player's manifest)", preFloor)
	}

	appium, isAppium := launcher.(*runner.AppiumLauncher)

	// #config — read the per-run configuration axes (segment, LocalProxy,
	// transfer-timeout). The launch-arg ones plus the minted -is.player_id are
	// forced on the cold launch in the Appium block below.
	cfg := readRunConfig(t, isAppium)
	segment := cfg.segment

	var sess *runner.Session
	if isAppium {
		// #714 config-on-connect: mint the player_id, resolve the cold-start
		// floor, and wire the driver (default = harness pre-configures via a
		// curl, so the variant ladder is known up front; CHAR_PROXY_CONFIG=app →
		// the player emits proxy.* on its own bootstrap URL). Replaces the old
		// coldStart / conservativeStart split — the Android special-casing is
		// gone now that Android honors the launch arg.
		pid := runner.NewPlayerID()
		// Generous budget — Android adds a catalogue-load settle on top of launch.
		setupCtx, setupCancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer setupCancel()
		floor := resolveFloor(setupCtx, t, preFloor, rampupFloorFrom)
		wireConfigOnConnect(setupCtx, t, appium, cfg.launchArgs(), pid, floor, 0, 0)

		s, lerr := appium.LaunchToHome(setupCtx, *picked)
		if lerr != nil {
			t.Fatalf("LaunchToHome: %v", lerr)
		}
		s.PlayerID = pid
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
