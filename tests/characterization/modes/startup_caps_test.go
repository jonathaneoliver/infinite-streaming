//go:build disabled_unchecked
// +build disabled_unchecked

package modes

import (
	"context"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Startup-caps applies a cap before launch and watches what variant the
// player picks from a cold start. For each cap level it: applies the cap,
// kills the app, relaunches, samples for the hold duration. The startup
// variant is whatever the player settles on once stalls drop to zero.
//
// Caps: 6 / 3 / 1.5 / 0.8 / 0.5 Mbps. 45 s per level. Total ~5 min + relaunch
// overhead × 5.
//
// Requires Manual/CLI/Appium launcher (relaunches between caps); skipped
// when no device is reachable.

func TestStartupCapsIPadSim(t *testing.T)   { runStartupCaps(t, runner.PlatformIPadSim) }
func TestStartupCapsIPhone(t *testing.T)    { runStartupCaps(t, runner.PlatformIPhone) }
func TestStartupCapsAppleTV(t *testing.T)   { runStartupCaps(t, runner.PlatformAppleTV) }
func TestStartupCapsAndroidTV(t *testing.T) { runStartupCaps(t, runner.PlatformAndroidTV) }

func runStartupCaps(t *testing.T, p runner.Platform) {
	caps := []float64{6.0, 3.0, 1.5, 0.8, 0.5}
	hold := 45 * time.Second

	sess := OpenSession(t, p)
	overall := time.Duration(len(caps))*(hold+45*time.Second) + 60*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), overall)
	defer cancel()

	report := &runner.Report{
		Mode:      "startup-caps",
		Platform:  p,
		Device:    sess.Device,
		PlayerID:  sess.PlayerID,
		StartedAt: time.Now(),
	}
	sampler := runner.NewSampler(sess, time.Second)
	sampler.Start(ctx)
	defer func() {
		report.Samples = sampler.Stop()
		_ = sess.ClearShape(context.Background())
	}()

	for i, rate := range caps {
		// Apply the cap, then bounce the app so it starts fresh under it.
		if err := sess.ApplyRate(ctx, rate); err != nil {
			t.Errorf("step %d apply: %v", i, err)
			break
		}
		sampler.SetAppliedRate(rate)
		if err := sess.Launcher.Kill(ctx, sess.Device); err != nil {
			t.Logf("step %d kill (non-fatal): %v", i, err)
		}
		stepStart := time.Now()
		newSess, err := sess.Launcher.Launch(ctx, sess.Device)
		if err != nil {
			t.Errorf("step %d relaunch: %v", i, err)
			break
		}
		// Player ID may change if the proxy minted a new record on relaunch.
		if newSess.PlayerID != sess.PlayerID {
			t.Logf("step %d: player id changed %s → %s", i, sess.PlayerID, newSess.PlayerID)
			sess.PlayerID = newSess.PlayerID
		}
		t.Logf("step %d/%d: cap=%.2f Mbps, holding %s", i+1, len(caps), rate, hold)
		if err := holdContext(ctx, hold); err != nil {
			t.Logf("step %d hold cancelled: %v", i, err)
		}
		report.Steps = append(report.Steps, runner.Step{
			StartedAt: stepStart,
			EndedAt:   time.Now(),
			RateMbps:  rate,
			Hold:      hold,
		})
	}
	report.Finalize(time.Now())

	out := runner.DefaultOutDir(t.TempDir())
	base := "startup-caps-" + string(p) + "-" + time.Now().UTC().Format("20060102T150405Z")
	jsonPath, err := runner.WriteReport(out, base, report)
	if err != nil {
		t.Fatalf("write report: %v", err)
	}
	LogReport(t, jsonPath)
}
