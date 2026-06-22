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

// armProbeConfig is one arm's reattach knobs, read from CHAR_ARM_<idx>_* env the
// CLI emitted. The server recipe is already bootstrapped onto playerID; these
// are only what the probe needs to bind + cold-launch with the right client
// knobs.
type armProbeConfig struct {
	playerID           string
	platform           string
	segment            string
	liveOffsetS        string
	protocol           string
	codec              string
	peakBitrate        int
	startsFirstVariant string
	pattern            string
	stepS              int
	margin             int
	patternMaster      bool
	content            string
	durationS          int
}

func readArmProbeConfig(fleetIndex int) armProbeConfig {
	p := func(suffix string) string {
		return strings.TrimSpace(os.Getenv(fmt.Sprintf("CHAR_ARM_%d_%s", fleetIndex, suffix)))
	}
	return armProbeConfig{
		playerID:           p("PLAYER_ID"),
		platform:           envOr(fmt.Sprintf("CHAR_ARM_%d_PLATFORM", fleetIndex), string(runner.PlatformIPadSim)),
		segment:            p("SEGMENT"),
		liveOffsetS:        envOr(fmt.Sprintf("CHAR_ARM_%d_LIVE_OFFSET", fleetIndex), "0"),
		protocol:           p("PROTOCOL"),
		codec:              p("CODEC"),
		peakBitrate:        envInt(fmt.Sprintf("CHAR_ARM_%d_PEAK_BITRATE", fleetIndex), 0),
		startsFirstVariant: p("FIRST_VARIANT"),
		pattern:            p("PATTERN"),
		stepS:              envInt(fmt.Sprintf("CHAR_ARM_%d_STEP_S", fleetIndex), 12),
		margin:             envInt(fmt.Sprintf("CHAR_ARM_%d_MARGIN", fleetIndex), 5),
		patternMaster:      p("PATTERN_MASTER") == "true",
		content:            envOr(fmt.Sprintf("CHAR_ARM_%d_CONTENT", fleetIndex), strings.TrimSpace(os.Getenv("CHAR_CONTENT"))),
		durationS:          envInt("CHAR_SWEEP_DURATION_S", 60),
	}
}

// runCharMatrixArmOnDevice reattaches one device to its arm's pre-configured
// session and drives playback for the arm's window. It mirrors
// runPyramidOnDevice's appium bring-up (own launcher per subtest, home barrier
// for a synchronized start, immediate slot release on cleanup) but skips all
// shaping — the recipe is already live from the CLI's bootstrap.
func runCharMatrixArmOnDevice(t *testing.T, p runner.Platform, dev runner.Device, bars *fleetBarriers) {
	cfg := readArmProbeConfig(dev.FleetIndex)

	// Register the barrier give-ups FIRST, before any Skip/Fatal: if this arm
	// bails early (no player_id, PickMode fails, …) it must drop itself from the
	// HOME barrier or the survivors wait it out to their deadline. We only use the
	// HOME barrier (synchronized playback start); the sweep barrier is unused (no
	// shaping), so give it up up front.
	homeArrived := false
	if bars != nil {
		bars.sweep.giveUp()
		defer func() {
			if !homeArrived {
				bars.home.giveUp()
			}
		}()
	}

	if cfg.playerID == "" {
		t.Skipf("arm %d has no CHAR_ARM_%d_PLAYER_ID (bootstrap failed or fewer arms than devices)", dev.FleetIndex, dev.FleetIndex)
	}
	if cfg.durationS <= 0 {
		cfg.durationS = 60
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
	t.Logf("arm %d: reattaching player_id=%s on %s for %ds", dev.FleetIndex, cfg.playerID, picked, cfg.durationS)

	staggerFleetLaunch(t, dev.FleetIndex)

	// Generous fleet bring-up window: an early sim holds at the home barrier
	// until the last, most-staggered sim arrives.
	setupTimeout := 3 * time.Minute
	if bars != nil {
		setupTimeout = 12 * time.Minute
	}
	setupCtx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	// Bind to the pre-configured session: same launch-state pins TestSweepProbe
	// uses, plus this arm's client knobs (segment / app live_offset / protocol),
	// all via the shared ProbeLaunchArgs projection.
	args := runner.ProbeLaunchArgs(runner.ProbeConfig{
		PlayerID:           cfg.playerID,
		Content:            cfg.content,
		Segment:            cfg.segment,
		LiveOffsetS:        cfg.liveOffsetS,
		Protocol:           cfg.protocol,
		Codec:              cfg.codec,
		PeakBitrateMbps:    cfg.peakBitrate,
		StartsFirstVariant: cfg.startsFirstVariant,
	})
	// Diagnostic toggle: CHAR_AUTO_RECOVERY=false feeds -is.flag.auto_recovery
	// false to every arm, disabling the iOS restart/live-resync ladder so we can
	// observe the player's NATURAL startup ABR behavior in isolation (does it
	// climb to 4K and wedge without any recovery papering over it?). Unset →
	// app default (auto-recovery ON).
	if v := strings.TrimSpace(os.Getenv("CHAR_AUTO_RECOVERY")); v != "" {
		args = append(args, "-is.flag.auto_recovery", v)
	}
	// Startup forward-buffer-cap experiment knobs (audio over-banking probe).
	// CHAR_FWD_BUFFER_S overrides the cap value (seconds); CHAR_FWD_RELEASE picks
	// when it's lifted (ttff | keepup | ttff_settle). Unset → app defaults
	// (3× max segment duration, released at TTFF+3s settle).
	if v := strings.TrimSpace(os.Getenv("CHAR_FWD_BUFFER_S")); v != "" {
		args = append(args, "-is.flag.startup_forward_buffer_s", v)
	}
	if v := strings.TrimSpace(os.Getenv("CHAR_FWD_RELEASE")); v != "" {
		args = append(args, "-is.flag.startup_fwd_release", v)
	}
	// Persistent (never-released) peak-bitrate ceiling, in Mbps — the floor the
	// startup variant cap relaxes TO after first frame. Unset → no permanent cap.
	if v := strings.TrimSpace(os.Getenv("CHAR_PERSIST_PEAK")); v != "" {
		args = append(args, "-is.flag.persistent_peak_bitrate_mbps", v)
	}
	// On-device LocalHTTPProxy — FORCED OFF for characterization by default. It
	// proxies over localhost, which skews AVPlayer's initial bitrate estimate and
	// drives cold-start over-selection wedges (see the STARTUP-FINDINGS). ALWAYS
	// passed (not only when the env is set) so a persisted ON in the sim's saved
	// UserDefaults can't leak in. Override with CHAR_LOCAL_PROXY=true.
	localProxy := strings.TrimSpace(os.Getenv("CHAR_LOCAL_PROXY"))
	if localProxy == "" {
		localProxy = "false"
	}
	args = append(args, "-is.flag.local_proxy", localProxy)

	// Auto-Recovery — FORCED OFF for characterization by default so a wedge is
	// OBSERVED, not silently restarted out from under the measurement. ALWAYS
	// passed so a persisted ON can't leak in. Override with CHAR_AUTO_RECOVERY=true.
	autoRecovery := strings.TrimSpace(os.Getenv("CHAR_AUTO_RECOVERY"))
	if autoRecovery == "" {
		autoRecovery = "false"
	}
	args = append(args, "-is.flag.auto_recovery", autoRecovery)

	appium.SetLaunchArgs(args)

	sess, lerr := appium.LaunchToHome(setupCtx, *picked)
	if lerr != nil {
		t.Fatalf("LaunchToHome: %v", lerr)
	}
	sess.PlayerID = cfg.playerID
	t.Cleanup(func() {
		cleanCtx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		_ = sess.CloseViaUI(cleanCtx) // clean client play_end
		_ = sess.Release(cleanCtx)    // free the session slot
		_ = launcher.Close()
	})

	// NO proxy reset here. The flow is reset → configure-on-connect → play, and
	// config-on-connect IS the reset+configure step: each arm gets a fresh
	// player_id whose session is created AND fully provisioned (shape+cap+faults+
	// content) by the bootstrap GET in char.go before this test runs. A reset
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

	var rerr error
	if cfg.content != "" {
		rerr = appium.ResumePlaybackClip(setupCtx, *picked, clipIDFromContent(cfg.content))
	} else {
		rerr = appium.ResumePlayback(setupCtx, *picked)
	}
	if rerr != nil {
		t.Fatalf("ResumePlayback: %v", rerr)
	}
	if herr := sess.WaitForHeartbeat(setupCtx, 90*time.Second); herr != nil {
		t.Fatalf("WaitForHeartbeat: %v", herr)
	}

	// Arm the bandwidth pattern post-launch (it can't ride config-on-connect — the
	// ladder is built from the live manifest variants, so the master playlist must
	// be fetched first). ONLY the master arms it; the proxy propagates the master's
	// pyramid to the group's slaves (NETSHAPE group pattern propagation), so all
	// arms share ONE bandwidth timeline. A slave arming its own would create an
	// independent, out-of-phase pyramid and confound the comparison.
	if cfg.pattern != "" && cfg.patternMaster {
		if err := sess.WaitForManifest(setupCtx, 45*time.Second); err != nil {
			t.Fatalf("arm %d (master): waiting for manifest before pattern: %v", dev.FleetIndex, err)
		}
		if err := sess.ApplyPattern(setupCtx, cfg.pattern, cfg.stepS, cfg.margin); err != nil {
			t.Fatalf("arm %d (master): ApplyPattern(%s): %v", dev.FleetIndex, cfg.pattern, err)
		}
		t.Logf("arm %d MASTER: armed %s pattern (step=%ds margin=%d%%) — proxy propagates to the group", dev.FleetIndex, cfg.pattern, cfg.stepS, cfg.margin)
	} else if cfg.pattern != "" {
		t.Logf("arm %d slave: pattern driven by the group master (no local ApplyPattern)", dev.FleetIndex)
	}

	// Let it play. The recipe (content/shape/live_offset/transfer) is already
	// live, so this window is what the CLI's measurement step later reads.
	t.Logf("arm %d playing for %ds…", dev.FleetIndex, cfg.durationS)
	time.Sleep(time.Duration(cfg.durationS) * time.Second)

	playID, perr := sess.CurrentPlayID(context.Background())
	if perr != nil {
		t.Logf("arm %d: could not read play_id: %v", dev.FleetIndex, perr)
	}
	base := strings.TrimRight(envOr("HARNESS_BASE_URL", "https://dev.jeoliver.com:21000"), "/")
	viewer := fmt.Sprintf("%s/dashboard/session-viewer.html?player_id=%s", base, cfg.playerID)
	if playID != "" {
		viewer += "&play_id=" + playID
	}
	t.Logf("ARM %d RESULT player_id=%s play_id=%s viewer=%s", dev.FleetIndex, cfg.playerID, playID, viewer)
	if playID == "" {
		t.Errorf("arm %d: no play_id captured — playback never registered a play", dev.FleetIndex)
	}
}
