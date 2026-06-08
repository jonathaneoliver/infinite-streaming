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

// Transient-shock — variant-derived square wave: cold-start at the floor,
// climb to the TOP variant, then slam the cap down to a sub-floor "shock"
// and hold, then restore the top cap and watch recovery. Repeated
// CHAR_SHOCK_REPS times (each a fresh shock on the same live play).
//
// This is the purpose-built probe for the sudden-drop wedge: a hard
// top→sub-floor drop while the player is warm on 4K is exactly the
// transition that strands AVPlayer (segment fetches time out at the live-
// edge 2s/5s limits, the connection can go dead for ~10s, the player
// attempts a big downshift and can hit CoreMedia -12880 "Can not proceed
// after removing variants"). Pair the run with the network-request view
// (proxy→client delivery + the dead window) and the AVMetrics feed
// (CoreMedia errors + variant-switch start/complete) to see whether the
// link survives the drop and whether the player recovers.
//
// Shares all of rampup's machinery: cold-start bootstrap, the run-config
// axes (CHAR_SEGMENT / CHAR_LOCAL_PROXY / CHAR_TRANSFER_TIMEOUT) forced at
// launch + applied server-side, play labels, and UI-close teardown.
//
// Env axes (in addition to the shared CHAR_SEGMENT / CHAR_LOCAL_PROXY /
// CHAR_TRANSFER_TIMEOUT):
//
//	CHAR_SHOCK_REPS        number of dips (default 2)
//	CHAR_SHOCK_LOW_MBPS    the shock floor (default: the rampup floor)
//	CHAR_SHOCK_HIGH_HOLD_S hold at the top cap, warm + recovery (default 120)
//	CHAR_SHOCK_DIP_HOLD_S  hold at the shock floor (default 90; must outlast
//	                       the buffer coast + wedge-cycle to see the outcome)

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
	coldStart := isAppium && preFloor > 0
	conservativeStart := isAppium && !coldStart

	// #config — read the per-run configuration axes (segment, LocalProxy,
	// transfer-timeout) and force the launch-arg ones on the cold launch.
	cfg := readRunConfig(t, isAppium)
	if args := cfg.launchArgs(); len(args) > 0 {
		appium.SetLaunchArgs(args)
		t.Logf("forcing run config via launch args %v — cold start lands on it", args)
	}
	segment := cfg.segment

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
			cleanCtx, c := context.WithTimeout(context.Background(), 15*time.Second)
			defer c()
			if cerr := sess.ClearShape(cleanCtx); cerr != nil {
				t.Logf("clear shape: %v", cerr)
			}
			if cerr := sess.CloseViaUI(cleanCtx); cerr != nil {
				t.Logf("close playback via UI: %v", cerr)
			}
			if cerr := launcher.Close(); cerr != nil {
				t.Logf("close launcher: %v", cerr)
			}
			if cerr := sess.ReleaseDevice(cleanCtx); cerr != nil {
				t.Logf("release device: %v", cerr)
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
		time.Sleep(2 * time.Second)
		if rerr := appium.ResumePlayback(setupCtx, *picked); rerr != nil {
			t.Fatalf("ResumePlayback: %v", rerr)
		}
		if herr := s.WaitForHeartbeat(setupCtx, 90*time.Second); herr != nil {
			t.Fatalf("WaitForHeartbeat: %v", herr)
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
			if cerr := launcher.Close(); cerr != nil {
				t.Logf("close launcher: %v", cerr)
			}
			if cerr := sess.ReleaseDevice(cleanCtx); cerr != nil {
				t.Logf("release device: %v", cerr)
			}
		})
	default:
		t.Logf("cold-start unavailable (non-Appium launcher) — legacy warmup path")
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
	switch {
	case conservativeStart:
		t.Logf("waiting %s for manifest to populate under conservative cap", rampdownWarmupHold)
		if err := holdContext(ctx, rampdownWarmupHold); err != nil {
			t.Fatalf("manifest-fetch hold: %v", err)
		}
	case !coldStart:
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

	// --- square-wave step list ----------------------------------
	// high = the TOP variant cap (desc is descending, so desc[0]). low =
	// the shock floor: CHAR_SHOCK_LOW_MBPS if set, else the rampup floor
	// (the exact top→floor drop we dissected). The player warms to the top
	// under `high`, then each rep slams to `low` (the shock) and restores
	// `high` (recovery window).
	top := desc[0]
	bottom := desc[len(desc)-1]
	high := top.CapMbps
	low := envFloat("CHAR_SHOCK_LOW_MBPS", rampupFloorFrom(desc))
	highHold := time.Duration(envInt("CHAR_SHOCK_HIGH_HOLD_S", 120)) * time.Second
	// The dip must outlast the buffer coast (~20s on a 4K buffer, during
	// which the player just plays out buffered high-variant content and
	// isn't yet operating at the low rate) PLUS the wedge-or-recover
	// resolution (a wedge held position frozen ~54s before the player
	// reset). 90s ≈ buffer-coast + wedge-cycle + margin, so we capture the
	// full outcome — reset, or a downshift that sustains at the low variant.
	dipHold := time.Duration(envInt("CHAR_SHOCK_DIP_HOLD_S", 90)) * time.Second
	reps := envInt("CHAR_SHOCK_REPS", 2)
	if reps < 1 {
		reps = 1
	}

	// [warm-high] + reps × [dip-low, recover-high]
	steps := make([]runner.Step, 0, 1+2*reps)
	hi := top
	lo := bottom
	steps = append(steps, runner.Step{RateMbps: high, Hold: highHold, Variant: &hi})
	for i := 0; i < reps; i++ {
		steps = append(steps,
			runner.Step{RateMbps: low, Hold: dipHold, Variant: &lo},
			runner.Step{RateMbps: high, Hold: highHold, Variant: &hi})
	}

	t.Logf("square wave: warm→%.3f Mbps (%s), then %d × [shock→%.3f Mbps (%s) → recover→%.3f Mbps (%s)]",
		high, highHold, reps, low, dipHold, high, highHold)

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
			"shocks":         fmt.Sprintf("%d", reps),
			"total_stalls":   fmt.Sprintf("%d", report.Summary.TotalStalls),
			"profile_shifts": fmt.Sprintf("%d", report.Summary.ProfileShifts),
		}
		if err := sess.LabelPlay(context.Background(), endLabels); err != nil {
			t.Logf("label play (end): %v", err)
		}
	}
}
