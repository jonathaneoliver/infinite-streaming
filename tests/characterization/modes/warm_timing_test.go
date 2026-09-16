package modes

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// TestWarmSessionTiming is a live A/B (#946): it times a COLD bring-up
// (LaunchToHome — which creates a fresh appium/WDA session) against a WARM
// bring-up (RelaunchApp — which reuses the session and only relaunches the app)
// on the SAME sim, so the config-#2 saving (what the pool's warm-session mode
// buys) is a measured number, not a guess. Also the first LIVE exercise of
// RelaunchApp. Gated: needs a booted sim + appium.
//
//	CHAR_WARM_TIMING=1 CHARACTERIZATION_DEVICE_UDID=<sim> HARNESS_BASE_URL=… \
//	CHAR_SWEEP_SERVER_URL=… CHAR_CONTENT=insane_newer_p200_h264 \
//	go test ./modes -run TestWarmSessionTiming -count=1 -v -timeout 8m
func TestWarmSessionTiming(t *testing.T) {
	if os.Getenv("CHAR_WARM_TIMING") != "1" {
		t.Skip("set CHAR_WARM_TIMING=1 to run the cold-vs-warm bring-up A/B")
	}
	platform := runner.Platform(envOr("CHAR_SWEEP_PLATFORM", string(runner.PlatformIPadSim)))
	clip := strings.TrimSpace(os.Getenv("CHAR_CONTENT"))
	serverURL := strings.TrimSpace(os.Getenv("CHAR_SWEEP_SERVER_URL"))
	reps := envInt("CHAR_WARM_REPS", 2)

	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	appium, ok := launcher.(*runner.AppiumLauncher)
	if !ok {
		t.Skipf("warm timing needs -launch-mode=appium (got %s)", mode)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()

	devs, err := appium.Discover(ctx)
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

	// player_ids don't need to be real sessions for a timing test — the app binds
	// them via NSArgumentDomain; we're measuring bring-up wall-time, not QoE.
	argsFor := func(pid string) []string {
		return runner.ProbeLaunchArgs(runner.ProbeConfig{PlayerID: pid, Content: clip, ServerURL: serverURL})
	}
	clipID := clipIDFromContent(clip)

	// --- COLD: fresh appium session (createSession + app launch + home) ---
	appium.SetLaunchArgs(argsFor("11111111-1111-1111-1111-111111111111"))
	t0 := time.Now()
	_, err = appium.LaunchToHome(ctx, *picked)
	coldBringup := time.Since(t0)
	if err != nil {
		t.Fatalf("cold LaunchToHome: %v", err)
	}
	defer func() { _ = appium.Close() }()
	if clip != "" {
		_ = appium.ResumePlaybackClip(ctx, *picked, clipID)
	}
	time.Sleep(3 * time.Second) // let a play establish so the relaunch is realistic

	// --- WARM: relaunch the app on the SAME session with a new binding ---
	var warmBringups []time.Duration
	for i := 0; i < reps; i++ {
		_ = appium.ClosePlaybackViaUI(ctx, *picked) // end the current play, back to home
		pid := "2222222" + string(rune('a'+i)) + "-2222-2222-2222-222222222222"
		t1 := time.Now()
		if err := appium.RelaunchApp(ctx, *picked, argsFor(pid)); err != nil {
			t.Fatalf("warm RelaunchApp rep %d: %v", i, err)
		}
		warmBringups = append(warmBringups, time.Since(t1))
		if clip != "" {
			_ = appium.ResumePlaybackClip(ctx, *picked, clipID)
		}
		time.Sleep(2 * time.Second)
	}

	var warmSum time.Duration
	for _, d := range warmBringups {
		warmSum += d
	}
	warmAvg := warmSum / time.Duration(len(warmBringups))

	t.Logf("WARM_TIMING device=%s", picked.UDID)
	t.Logf("WARM_TIMING cold_bringup_ms=%d   (LaunchToHome — creates the appium/WDA session)", coldBringup.Milliseconds())
	t.Logf("WARM_TIMING warm_bringup_ms=%d   (RelaunchApp avg over %d — reuses the session)", warmAvg.Milliseconds(), len(warmBringups))
	if coldBringup > 0 {
		saved := coldBringup - warmAvg
		t.Logf("WARM_TIMING saved_ms=%d  (%.0f%% faster bring-up)", saved.Milliseconds(), 100*float64(saved)/float64(coldBringup))
	}
	for i, d := range warmBringups {
		t.Logf("WARM_TIMING   warm rep %d = %dms", i, d.Milliseconds())
	}
}
