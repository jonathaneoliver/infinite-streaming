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
		bringupStart := time.Now()
		// cold: relaunch the app every rep after the first (rep 0 is already cold
		// from LaunchToHome). warm: never relaunch — resume in the running app.
		if i > 0 && !warm {
			if err := appium.RelaunchApp(ctx, *dev, args); err != nil {
				t.Fatalf("rep %d RelaunchApp: %v", i, err)
			}
		}
		if err := appium.ResumePlaybackClip(ctx, *dev, clipID); err != nil {
			t.Fatalf("rep %d ResumePlaybackClip: %v", i, err)
		}
		bringupMs := time.Since(bringupStart).Milliseconds()

		time.Sleep(time.Duration(durationS) * time.Second)

		playID, perr := sess.CurrentPlayID(ctx)
		if perr != nil || playID == "" {
			t.Logf("REPBATCH rep=%d mode=%s bringup_ms=%d play_id= (no play: %v)", i, repMode(warm, i), bringupMs, perr)
		} else {
			t.Logf("REPBATCH rep=%d mode=%s bringup_ms=%d play_id=%s", i, repMode(warm, i), bringupMs, playID)
		}
		// End this play → home, so the next rep starts a fresh play.
		_ = appium.ClosePlaybackViaUI(ctx, *dev)
	}
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
