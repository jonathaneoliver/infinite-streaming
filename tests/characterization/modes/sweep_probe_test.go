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

// TestSweepProbe is the automated fault-sweep's generic probe (issue #772,
// docs/sweep-design.md §7). Unlike the characterization modes — which mint
// their own player_id and apply their own shape recipe — this probe REATTACHES
// to a session the sweep already materialised via `harness sweep bootstrap`
// (config-on-connect): the full {shape × fault × content_manipulation × labels}
// recipe is live on the session BEFORE this launches. The probe's only job is
// to drive playback on that session for a fixed window so the proxy/forwarder
// records a play the sweep's `analyze` step can classify into a verdict.
//
// It is the "mode-reattach glue": the bridge between the sweep's pre-launch
// bootstrap and the existing appium launch path. No cap matrix, no pattern, no
// Report JSON — analyze reads QoE labels from ClickHouse, not from a report.
//
// Required env:
//
//	CHAR_PLAYER_ID       the bootstrapped session id to reattach to (required)
//	HARNESS_BASE_URL     test-dev origin (also where the viewer link points)
//	LAUNCH_MODE=appium   (iOS sim / Apple TV)
//
// Optional:
//
//	CHAR_CONTENT             clip to resume (pins -is.lastPlayed)
//	CHAR_SWEEP_DURATION_S    seconds to let it play (default 60)
//	CHAR_SWEEP_PLATFORM      platform enum (default ipad-sim — any booted iOS sim)
//	CHARACTERIZATION_DEVICE_UDID  pin a specific device
func TestSweepProbe(t *testing.T) {
	playerID := strings.TrimSpace(os.Getenv("CHAR_PLAYER_ID"))
	if playerID == "" {
		t.Skip("TestSweepProbe needs CHAR_PLAYER_ID (the session id from `harness sweep bootstrap`)")
	}
	platform := runner.Platform(envOr("CHAR_SWEEP_PLATFORM", string(runner.PlatformIPadSim)))
	durationS := envInt("CHAR_SWEEP_DURATION_S", 60)
	if durationS <= 0 {
		durationS = 60
	}

	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	appium, isAppium := launcher.(*runner.AppiumLauncher)
	if !isAppium {
		t.Skipf("sweep probe requires -launch-mode=appium (got %s)", mode)
	}

	setupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Pick the booted device for this platform (honouring an explicit UDID).
	devs, err := appium.Discover(setupCtx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	wantUDID := strings.TrimSpace(os.Getenv("CHARACTERIZATION_DEVICE_UDID"))
	var picked *runner.Device
	for i := range devs {
		if devs[i].Platform != platform {
			continue
		}
		if wantUDID != "" && !strings.EqualFold(devs[i].UDID, wantUDID) {
			continue
		}
		picked = &devs[i]
		break
	}
	if picked == nil {
		t.Skipf("no %s device discovered (udid=%q)", platform, wantUDID)
	}
	t.Logf("sweep probe: reattaching player_id=%s on %s for %ds", playerID, picked, durationS)

	// Launch args bind the app to the bootstrapped session and pin the same
	// launch-state flags the characterization modes force (rotation off so the
	// run stays one play; land on home so ResumePlayback drives the intended
	// play; HUD on for live observation). We do NOT call wireConfigOnConnect —
	// the session is already configured, so re-running ConfigureOnConnect would
	// overwrite the sweep's shape with a no-op default.
	args := []string{
		"-is.player_id", playerID,
		"-is.flag.play_id_rotation_s", "0",
		"-is.flag.skip_home", "false",
		"-is.flag.dev_mode", "true",
	}
	if clip := strings.TrimSpace(os.Getenv("CHAR_CONTENT")); clip != "" {
		args = append(args, "-is.lastPlayed", clip)
	}
	// Segment variant (#793 live-offset matrix): s2|s6|ll selects which
	// master the app requests (master_2s/master_6s/master). Empty leaves the
	// app default (s6). The hold-back floor scales with segment duration, so a
	// given live_offset can be legal on one and sub-spec on another.
	if seg := strings.TrimSpace(os.Getenv("CHAR_SWEEP_SEGMENT")); seg != "" {
		args = append(args, "-is.segment", seg)
	}
	// App-side live-offset override (#793): is.flag.live_offset_s sets the
	// player's own target — it seeks to liveEdge−N and OVERRIDES the manifest
	// HOLD-BACK when >0. ALWAYS pin it (default "0") so a run never inherits the
	// app's persisted/stepper value, which would silently confound a
	// manifest-only test (the launch-arg domain wins over the saved default).
	// CHAR_SWEEP_LIVE_OFFSET sets a non-zero value to exercise the app lever /
	// the manifest × app-override combination matrix.
	lo := strings.TrimSpace(os.Getenv("CHAR_SWEEP_LIVE_OFFSET"))
	if lo == "" {
		lo = "0"
	}
	args = append(args, "-is.flag.live_offset_s", lo)
	appium.SetLaunchArgs(args)

	sess, err := appium.LaunchToHome(setupCtx, *picked)
	if err != nil {
		t.Fatalf("LaunchToHome: %v", err)
	}
	sess.PlayerID = playerID
	// Free the session slot at the end (config-on-connect pool is small); the
	// ClickHouse archive — what the session-viewer reads — persists regardless.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sess.CloseViaUI(cleanupCtx) // clean client play_end
		_ = sess.Release(cleanupCtx)    // delete the proxy session, free the slot
	})
	if err := appium.ResumePlayback(setupCtx, *picked); err != nil {
		t.Fatalf("ResumePlayback: %v", err)
	}

	// Drive the bandwidth motion for a config-class pattern recipe: once the
	// master is fetched (variants known), arm the pattern — the same path the
	// characterization modes use. This is what makes a `mode: pyramid` /
	// `shape.pattern` experiment actually sweep bandwidth, not just plain-play.
	if pattern := strings.TrimSpace(os.Getenv("CHAR_SWEEP_PATTERN")); pattern != "" {
		stepS := envInt("CHAR_SWEEP_STEP_S", 12)
		margin := envInt("CHAR_SWEEP_MARGIN", 5)
		if err := sess.WaitForManifest(setupCtx, 45*time.Second); err != nil {
			t.Fatalf("waiting for manifest before pattern: %v", err)
		}
		if err := sess.ApplyPattern(setupCtx, pattern, stepS, margin); err != nil {
			t.Fatalf("ApplyPattern(%s): %v", pattern, err)
		}
		t.Logf("armed %s pattern (step=%ds margin=%d%%)", pattern, stepS, margin)
	}

	// Let it play. The recipe (content/shape/transfer) is already live, so this
	// window is what the oracle later reads.
	t.Logf("playing for %ds…", durationS)
	time.Sleep(time.Duration(durationS) * time.Second)

	playID, err := sess.CurrentPlayID(context.Background())
	if err != nil {
		t.Logf("warning: could not read play_id: %v", err)
	}

	base := strings.TrimRight(envOr("HARNESS_BASE_URL", "https://dev.jeoliver.com:21000"), "/")
	viewer := fmt.Sprintf("%s/dashboard/session-viewer.html?player_id=%s", base, playerID)
	if playID != "" {
		viewer += "&play_id=" + playID
	}

	// The headline output the sweep + operator key off (also satisfies the
	// "always log player_id, play_id, and a viewer URL" requirement).
	t.Logf("SWEEP PROBE RESULT")
	t.Logf("  player_id: %s", playerID)
	t.Logf("  play_id:   %s   (analyze: harness sweep analyze <exp> --play %s)", playID, playID)
	t.Logf("  session-viewer: %s", viewer)
	if playID == "" {
		t.Fatal("no play_id captured — playback never registered a play (inconclusive, not a player fault)")
	}
}
