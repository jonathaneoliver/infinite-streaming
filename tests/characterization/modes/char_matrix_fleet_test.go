package modes

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/charplan"
	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// interruptContext returns a process-wide context cancelled on SIGINT/SIGTERM.
// A fleet arm's play window selects on it, so an operator stop (Ctrl-C, or a
// SIGTERM forwarded by the `harness char matrix` CLI) ends the arm EARLY and its
// deferred t.Cleanup runs — releasing the appium session instead of orphaning
// it. Orphaned sessions leave the device-farm thinking the sims are busy and
// block the next run with create-session timeouts (#853). Best-effort: SIGKILL
// can't be caught, so a hard kill still needs the appium-restart backstop.
var (
	interruptOnce sync.Once
	interruptC    context.Context
)

func interruptContext() context.Context {
	interruptOnce.Do(func() {
		var cancel context.CancelFunc
		interruptC, cancel = context.WithCancel(context.Background())
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-ch
			cancel()
			// Keep draining so a repeated Ctrl-C (or a CLI-forwarded signal)
			// doesn't hit the default handler and kill the binary before
			// t.Cleanup finishes.
			for range ch {
			}
		}()
	})
	return interruptC
}

// TestCharMatrixFleet is the parallel backend for `harness char matrix` on a
// parallel:true spec (issue #811). The CLI bootstraps every arm's server-side
// recipe up front (one config-on-connect session per arm, like the sequential
// path) and then runs THIS test once with CHAR_FLEET_COUNT=N and the per-arm
// knobs in CHAR_ARM_<fleetIndex>_* env. runFleet fans the work out one parallel
// subtest per device; each subtest reattaches to its arm's pre-configured
// session and drives playback — so every arm streams simultaneously, gated to a
// common start by the fleet HOME barrier.
//
// Like TestSweepProbe this is a pure reattach probe: the recipe is already live
// on the session, so we never call wireConfigOnConnect (that would overwrite it).
// The client-side knobs (segment / app live_offset / protocol) ride in via
// runner.ProbeLaunchArgs, the same projection the sequential probe + the matrix
// runner share.
//
// Skips cleanly (CHAR_MATRIX_FLEET unset) so a plain `go test ./modes` run never
// triggers it — it's an orchestration target, not a standalone characterization.
func TestCharMatrixFleet(t *testing.T) {
	if strings.TrimSpace(os.Getenv("CHAR_MATRIX_FLEET")) == "" {
		t.Skip("TestCharMatrixFleet is the `harness char matrix` parallel backend — set CHAR_MATRIX_FLEET=1 (the CLI does)")
	}
	platform := runner.Platform(envOr("CHAR_SWEEP_PLATFORM", string(runner.PlatformIPadSim)))
	runFleet(t, platform, runCharMatrixArmOnDevice)
}

// The per-arm reattach knobs (segment / live_offset / protocol / codec / peak /
// first_variant / muted / startup-buffer / local_proxy / auto_recovery / pattern
// / content) now arrive as one typed charplan.RunPlan the CLI wrote to
// CHAR_RUN_PLAN_FILE — replacing the flat CHAR_ARM_<idx>_* env the probe used to
// re-parse field by field. The server recipe is already bootstrapped onto each
// arm's PlayerID; the plan is only what the probe needs to bind + cold-launch.
// (CHAR_ARM_<i>_PLATFORM + CHAR_FLEET_COUNT stay — they're the generic fleet
// resolver's interface in fleet.go, shared by every fleet mode, not just this one.)
var (
	runPlanOnce sync.Once
	runPlanVal  *charplan.RunPlan
	runPlanErr  error
)

func loadRunPlan() (*charplan.RunPlan, error) {
	runPlanOnce.Do(func() {
		runPlanVal, runPlanErr = charplan.Load(os.Getenv("CHAR_RUN_PLAN_FILE"))
	})
	return runPlanVal, runPlanErr
}

// runCharMatrixArmOnDevice reattaches one device to its arm's pre-configured
// session and drives playback for the arm's window. It mirrors
// runPyramidOnDevice's appium bring-up (own launcher per subtest, home barrier
// for a synchronized start, immediate slot release on cleanup) but skips all
// shaping — the recipe is already live from the CLI's bootstrap.
func runCharMatrixArmOnDevice(t *testing.T, p runner.Platform, dev runner.Device, bars *fleetBarriers) {
	// Register the barrier give-ups FIRST, before any Skip/Fatal (incl. the plan
	// load below): if this arm bails early (no player_id, PickMode fails, …) it
	// must drop itself from the HOME barrier or the survivors wait it out to their
	// deadline. We only use the HOME barrier (synchronized playback start); the
	// sweep barrier is unused (no shaping), so give it up up front.
	homeArrived := false
	if bars != nil {
		bars.sweep.giveUp()
		defer func() {
			if !homeArrived {
				bars.home.giveUp()
			}
		}()
	}

	plan, perr := loadRunPlan()
	if perr != nil {
		t.Fatalf("load run plan (CHAR_RUN_PLAN_FILE): %v", perr) // loud on a stale binary / missing plan
	}
	if dev.FleetIndex >= len(plan.Arms) {
		t.Skipf("arm %d beyond the run plan (%d arms)", dev.FleetIndex, len(plan.Arms))
	}
	cfg := plan.Arms[dev.FleetIndex]
	if cfg.PlayerID == "" {
		t.Skipf("arm %d has no player_id in the run plan (bootstrap failed or fewer arms than devices)", dev.FleetIndex)
	}
	durationS := plan.DurationS
	if durationS <= 0 {
		durationS = 60
	}

	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	appium, isAppium := launcher.(*runner.AppiumLauncher)
	if !isAppium {
		t.Skipf("char matrix fleet requires -launch-mode=appium (got %s)", mode)
	}
	picked := &dev
	t.Logf("arm %d: reattaching player_id=%s on %s for %ds", dev.FleetIndex, cfg.PlayerID, picked, durationS)

	staggerFleetLaunch(t, dev.FleetIndex)

	// Generous fleet bring-up window: an early sim holds at the home barrier
	// until the last, most-staggered sim arrives.
	setupTimeout := 3 * time.Minute
	if bars != nil {
		setupTimeout = 12 * time.Minute
	}
	// A real iOS device cold-builds WDA via xcodebuild (~190s observed) BEFORE the
	// app launches — over the 3-min single-device window. Give it room so the
	// first (cold) run doesn't fail the create; later runs reuse the build and are
	// fast. Aligns with the launcher's 300s HTTP ceiling + 240s wdaLaunchTimeout.
	if dev.Platform == runner.PlatformIPhone || dev.Platform == runner.PlatformIPad {
		if setupTimeout < 8*time.Minute {
			setupTimeout = 8 * time.Minute
		}
	}
	setupCtx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	// Bind to the pre-configured session: the same launch-state pins, the client
	// knobs (segment / app live_offset / protocol / …), AND the startup/recovery
	// knobs (forward-buffer cap, local_proxy, auto_recovery) — all resolved by the
	// CLI into this arm's ArmConfig and projected through the one ProbeLaunchArgs.
	// local_proxy/auto_recovery defaults (false/true) were already applied by the
	// producer's WithDefaults, so they ride in here non-nil and always emit.
	appium.SetLaunchArgs(runner.ProbeLaunchArgs(runner.ProbeConfigFromArm(cfg)))

	sess, lerr := appium.LaunchToHome(setupCtx, *picked)
	if lerr != nil {
		t.Fatalf("LaunchToHome: %v", lerr)
	}
	sess.PlayerID = cfg.PlayerID
	sess.ServerURL = cfg.ServerURL // #942: play_id read + release target the arm's own server
	// Record the device-farm UDID this arm acquired so the harness can release
	// EXACTLY this run's devices after the process exits (#853) — concurrent-run
	// safe. O_APPEND keeps parallel arms' lines from interleaving.
	if mf := strings.TrimSpace(os.Getenv("CHAR_DEVICE_MANIFEST")); mf != "" && sess.Device.UDID != "" {
		if f, ferr := os.OpenFile(mf, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); ferr == nil {
			fmt.Fprintln(f, sess.Device.UDID)
			f.Close()
		}
	}
	t.Cleanup(func() {
		cleanCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = sess.CloseViaUI(cleanCtx) // clean client play_end
		_ = sess.Release(cleanCtx)    // free the session slot
		_ = launcher.Close()
	})

	// Deferred config-on-connect (#937): the CLI built this arm's full proxy.cfg
	// blob but did NOT GET it up front — doing so would allocate every arm's session
	// before any device is reserved and exhaust the proxy pool (503) on a large
	// fleet. Now that a device is reserved (LaunchToHome, above) and t.Cleanup is
	// registered to release it on any failure, materialise the session HERE, before
	// playback. The fleet caps concurrent devices at ≤4, so ≤4 sessions live at once.
	// Empty BootstrapCfgB64 => the CLI already bootstrapped up front (legacy/sequential
	// path) — skip. A GET failure after a device is reserved is fatal for this arm;
	// t.Cleanup frees the device + session.
	if cfg.BootstrapCfgB64 != "" {
		// Bootstrap on the arm's OWN server (#942) so a per-arm server materialises
		// its config-on-connect session where that arm actually streams; falls back
		// to the run's base URL for arms with no explicit server.
		bootBase := cfg.ServerURL
		if bootBase == "" {
			bootBase = plan.BaseURL
		}
		if berr := runner.ConfigureOnConnectCfg(setupCtx, bootBase, cfg.Content, cfg.PlayerID, cfg.GroupID, cfg.BootstrapCfgB64); berr != nil {
			t.Fatalf("deferred config-on-connect (arm %d, player_id=%s): %v", dev.FleetIndex, cfg.PlayerID, berr)
		}
		t.Logf("arm %d: config-on-connect materialised (deferred, post-reserve)", dev.FleetIndex)
	}

	// NO proxy reset here. The flow is reset → configure-on-connect → play, and
	// config-on-connect IS the reset+configure step: each arm gets a fresh
	// player_id whose session is created AND fully provisioned (shape+cap+faults+
	// content) by the deferred config-on-connect GET above (or, on the legacy path,
	// by the bootstrap GET in char.go before this test runs). A reset
	// AFTER that bootstrap (what used to live here) reverted the session to the
	// global INFINITE_STREAM_DEFAULT_RATE_MBPS baseline (100 Mbps) — wiping the
	// config-on-connect rate cap — so the player streamed unthrottled for the ~2s
	// until ApplyPattern armed, over-selected a high variant, and wedged. Dropping
	// it lets the bootstrapped cap survive to the player's first byte; the pattern
	// then arms post-launch and climbs from that floor. (A separate pre-bootstrap
	// reset would be a no-op anyway — the player_id is freshly minted, so there is
	// no prior session to clear.)

	// Fleet HOME barrier: hold until every arm is at home, then all start
	// playback together — so the arms stream simultaneously, not staggered.
	if bars != nil {
		homeArrived = true
		t.Logf("arm %d at home — waiting at fleet HOME barrier", dev.FleetIndex)
		bars.home.arriveAndWait(setupCtx)
		t.Logf("arm %d HOME barrier released — starting playback", dev.FleetIndex)
	}

	// Rep loop (#963: warm/channel_change): CHAR_REP_COUNT plays in ONE app launch.
	// Rep 0 is the cold-launch play — where the constraints (is.peak_bitrate_mbps /
	// is.segment via launch args, proxy.shape via config-on-connect) were configured
	// above. For rep>0: CHAR_START_MODE=warm ends the prior play → home and starts a
	// NEW play WITHOUT relaunching (the app keeps the cap/segment, the proxy session
	// keeps the shape, AVPlayer keeps its learned estimate) = channel_change; cold
	// relaunches the app per rep (fresh AVPlayer, config re-applied). Each rep emits
	// its own ARM RESULT line tagged rep=/mode= (default reps=1 → the original path).
	reps := envInt("CHAR_REP_COUNT", 1)
	if reps < 1 {
		reps = 1
	}
	warm := strings.EqualFold(strings.TrimSpace(os.Getenv("CHAR_START_MODE")), "warm")
	repArgs := runner.ProbeLaunchArgs(runner.ProbeConfigFromArm(cfg))

	for rep := 0; rep < reps; rep++ {
		if rep > 0 {
			// End the prior play → home. Cold relaunches (fresh AVPlayer); warm stays
			// in the running app (channel_change). Not counted in startup.
			tdCtx, tdCancel := context.WithTimeout(context.Background(), 60*time.Second)
			_ = appium.ClosePlaybackViaUI(tdCtx, *picked)
			if !warm {
				if err := appium.LaunchAppWarmToHome(tdCtx, *picked, repArgs); err != nil {
					tdCancel()
					t.Fatalf("arm %d rep %d relaunch: %v", dev.FleetIndex, rep, err)
				}
			}
			tdCancel()
		}

		var rerr error
		if cfg.Content != "" {
			rerr = appium.ResumePlaybackClip(setupCtx, *picked, clipIDFromContent(cfg.Content))
		} else {
			rerr = appium.ResumePlayback(setupCtx, *picked)
		}
		if rerr != nil {
			t.Fatalf("arm %d rep %d ResumePlayback: %v", dev.FleetIndex, rep, rerr)
		}
		if herr := sess.WaitForHeartbeat(setupCtx, 90*time.Second); herr != nil {
			t.Fatalf("arm %d rep %d WaitForHeartbeat: %v", dev.FleetIndex, rep, herr)
		}

		// Capture the play_id NOW, while the player is confirmed connected. Reading it
		// only at the END of the window is fragile: a slow-starting arm — the Android
		// TV cold-starts ~50s and stops heartbeating a few seconds before its window
		// elapses — makes the end-read 404 and the play look unregistered even though
		// it streamed the whole time. The play_id is stable for a play, so the earliest
		// reliable read is the trustworthy source; the end-read only refreshes it.
		var earlyPlayID string
		for i := 0; i < 10; i++ {
			if pid, e := sess.CurrentPlayID(setupCtx); e == nil && pid != "" {
				earlyPlayID = pid
				break
			}
			time.Sleep(time.Second)
		}

		// Arm the bandwidth pattern post-launch (rep 0 only — the ladder is built from
		// the live manifest variants, so the master must fetch the master playlist
		// first). ONLY the master arms it; the proxy propagates to the group's slaves.
		if rep == 0 && cfg.Pattern != "" && cfg.PatternMaster {
			if err := sess.WaitForManifest(setupCtx, 45*time.Second); err != nil {
				t.Fatalf("arm %d (master): waiting for manifest before pattern: %v", dev.FleetIndex, err)
			}
			if err := sess.ApplyPattern(setupCtx, cfg.Pattern, cfg.StepS, cfg.MarginPct); err != nil {
				t.Fatalf("arm %d (master): ApplyPattern(%s): %v", dev.FleetIndex, cfg.Pattern, err)
			}
			t.Logf("arm %d MASTER: armed %s pattern (step=%ds margin=%d%%) — proxy propagates to the group", dev.FleetIndex, cfg.Pattern, cfg.StepS, cfg.MarginPct)
		} else if rep == 0 && cfg.Pattern != "" {
			t.Logf("arm %d slave: pattern driven by the group master (no local ApplyPattern)", dev.FleetIndex)
		}

		// Let it play. The recipe (content/shape/live_offset/transfer) is already live.
		t.Logf("arm %d rep %d playing for %ds…", dev.FleetIndex, rep, durationS)
		select {
		case <-time.After(time.Duration(durationS) * time.Second):
			// Normal: the full play window elapsed.
		case <-interruptContext().Done():
			// Operator stopped the run. Return EARLY so the deferred t.Cleanup
			// releases this arm's appium session instead of orphaning it (#853).
			t.Logf("arm %d: run interrupted — ending early so the appium session is released (#853)", dev.FleetIndex)
			return
		}

		playID, perr := sess.CurrentPlayID(context.Background())
		if playID == "" {
			if earlyPlayID != "" {
				playID = earlyPlayID
			} else if perr != nil {
				t.Logf("arm %d rep %d: could not read play_id: %v", dev.FleetIndex, rep, perr)
			}
		}
		// Point the viewer link at the arm's OWN server (#942); fall back to base.
		base := strings.TrimRight(cfg.ServerURL, "/")
		if base == "" {
			base = strings.TrimRight(envOr("HARNESS_BASE_URL", "https://dev.jeoliver.com:21000"), "/")
		}
		viewer := fmt.Sprintf("%s/dashboard/session-viewer.html?player_id=%s", base, cfg.PlayerID)
		if playID != "" {
			viewer += "&play_id=" + playID
		}
		mode := "cold"
		if warm && rep > 0 {
			mode = "channel_change"
		}
		t.Logf("ARM %d RESULT player_id=%s play_id=%s rep=%d mode=%s viewer=%s", dev.FleetIndex, cfg.PlayerID, playID, rep, mode, viewer)
		if playID == "" {
			t.Errorf("arm %d rep %d: no play_id captured — playback never registered a play", dev.FleetIndex, rep)
		}
	}
}
