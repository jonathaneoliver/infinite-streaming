package modes

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// TestSweepRepBatch runs N reps of ONE config in a single warm appium session
// (#946) — the warm rep-loop, the third execution shape. It implements both
// start modes and measures each rep's bring-up so the cold-vs-warm-start delta
// is a number:
//
//   - cold: every rep cold-launches the app (RelaunchApp) → fresh AVPlayer, so
//     each rep is an independent cold start on the reused session (config #2).
//   - warm: after rep 0 establishes playback, each subsequent rep starts a NEW
//     play from home WITHOUT relaunching (config #3) → warm buffers / ABR / decoder
//     carry over, the genuinely different warm start.
//
// All reps share ONE player_id/session (same config), producing N distinct
// play_ids. Emits REPBATCH lines the pool/operator parses. Gated: needs
// CHAR_PLAYER_ID (from `sweep bootstrap`) + a booted sim.
//
//	CHAR_PLAYER_ID=<uuid> CHAR_REP_COUNT=3 CHAR_START_MODE=warm \
//	CHARACTERIZATION_DEVICE_UDID=<sim> HARNESS_BASE_URL=… CHAR_SWEEP_SERVER_URL=… \
//	CHAR_CONTENT=insane_newer_p200_h264 CHAR_SWEEP_DURATION_S=30 \
//	go test ./modes -run TestSweepRepBatch -count=1 -v -timeout 12m
func TestSweepRepBatch(t *testing.T) {
	playerID := strings.TrimSpace(os.Getenv("CHAR_PLAYER_ID"))
	if playerID == "" {
		t.Skip("TestSweepRepBatch needs CHAR_PLAYER_ID (the session from `sweep bootstrap`)")
	}
	platform := runner.Platform(envOr("CHAR_SWEEP_PLATFORM", string(runner.PlatformIPadSim)))
	reps := envInt("CHAR_REP_COUNT", 3)
	if reps < 1 {
		reps = 1
	}
	warm := strings.EqualFold(strings.TrimSpace(os.Getenv("CHAR_START_MODE")), "warm")
	durationS := envInt("CHAR_SWEEP_DURATION_S", 30)
	clip := strings.TrimSpace(os.Getenv("CHAR_CONTENT"))
	clipID := clipIDFromContent(clip)

	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	appium, ok := launcher.(*runner.AppiumLauncher)
	if !ok {
		t.Skipf("rep batch needs -launch-mode=appium (got %s)", mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 11*time.Minute)
	defer cancel()

	dev := pickDeviceForBatch(ctx, t, appium, platform)

	args := runner.ProbeLaunchArgs(runner.ProbeConfig{
		PlayerID:  playerID,
		Content:   clip,
		Segment:   strings.TrimSpace(os.Getenv("CHAR_SWEEP_SEGMENT")),
		Protocol:  strings.TrimSpace(os.Getenv("CHAR_SWEEP_PROTOCOL")),
		Codec:     strings.TrimSpace(os.Getenv("CHAR_SWEEP_CODEC")),
		Muted:     strings.TrimSpace(os.Getenv("CHAR_SWEEP_MUTED")),
		ServerURL: strings.TrimSpace(os.Getenv("CHAR_SWEEP_SERVER_URL")),
	})

	// Establish the session (one-time cold launch — reported separately).
	appium.SetLaunchArgs(args)
	setupStart := time.Now()
	sess, err := appium.LaunchToHome(ctx, *dev)
	if err != nil {
		t.Fatalf("LaunchToHome: %v", err)
	}
	sess.PlayerID = playerID
	defer func() { _ = appium.Close() }()
	t.Logf("REPBATCH session_setup_ms=%d start_mode=%s reps=%d", time.Since(setupStart).Milliseconds(),
		map[bool]string{true: "warm", false: "cold"}[warm], reps)

	for i := 0; i < reps; i++ {
		// TEARDOWN (kept separate, #946): stop the previous play, and for cold
		// kill the app so the next launch is genuinely cold. Rep 0 has no
		// previous play → no teardown. This is NOT counted in startup.
		var teardownMs int64
		if i > 0 {
			tdStart := time.Now()
			_ = appium.ClosePlaybackViaUI(ctx, *dev) // end the prior play → home
			if !warm {
				if err := appium.TerminateApp(ctx, *dev); err != nil {
					t.Fatalf("rep %d TerminateApp: %v", i, err)
				}
			}
			teardownMs = time.Since(tdStart).Milliseconds()
		}

		// STARTUP: get the next play going. cold i>0 relaunches the app (on the
		// warm session — no WDA/session re-create); warm never relaunches; rep 0
		// is already launched by the one-time LaunchToHome (session_setup).
		startStart := time.Now()
		if i > 0 && !warm {
			if err := appium.LaunchAppWarmToHome(ctx, *dev, args); err != nil {
				t.Fatalf("rep %d LaunchAppWarmToHome: %v", i, err)
			}
		}
		if err := appium.ResumePlaybackClip(ctx, *dev, clipID); err != nil {
			t.Fatalf("rep %d ResumePlaybackClip: %v", i, err)
		}
		startupMs := time.Since(startStart).Milliseconds()

		time.Sleep(time.Duration(durationS) * time.Second)

		playID, perr := sess.CurrentPlayID(ctx)
		pidField := "play_id="
		if perr == nil && playID != "" {
			pidField += playID
		}
		// teardown_ms = stop the previous (0 for rep 0); startup_ms = launch+resume
		// the next; ttff comes from the archive per play_id (first_frame_s).
		t.Logf("REPBATCH rep=%d mode=%s teardown_ms=%d startup_ms=%d %s", i, repMode(warm, i), teardownMs, startupMs, pidField)
	}
	// The final play is left running; appium.Close (deferred) tears down the session.
}

// repMode names a rep's effective start: rep 0 is always cold (LaunchToHome
// established it); later reps are cold (relaunch) or warm (resume-in-place).
func repMode(warm bool, i int) string {
	if i == 0 || !warm {
		return "cold"
	}
	return "warm"
}

func pickDeviceForBatch(ctx context.Context, t *testing.T, appium *runner.AppiumLauncher, platform runner.Platform) *runner.Device {
	t.Helper()
	devs, err := appium.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	wantUDID := strings.TrimSpace(os.Getenv("CHARACTERIZATION_DEVICE_UDID"))
	for i := range devs {
		if devs[i].Platform != platform {
			continue
		}
		if wantUDID != "" && !strings.EqualFold(devs[i].UDID, wantUDID) {
			continue
		}
		return &devs[i]
	}
	t.Skipf("no %s device discovered (udid=%q)", platform, wantUDID)
	return nil
}
