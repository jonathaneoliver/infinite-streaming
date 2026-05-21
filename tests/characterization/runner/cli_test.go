package runner

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestCLIDiscover exercises whichever discovery tools are installed and
// prints the device list. Pure smoke — passes as long as the calls don't
// error (no-op when no tools are installed).
func TestCLIDiscover(t *testing.T) {
	if !haveAnyDeviceTool() {
		t.Skip("no xcrun and no adb on $PATH — nothing to discover")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c := NewCLILauncher()
	devs, err := c.Discover(ctx)
	if err != nil {
		t.Fatalf("cli discover: %v", err)
	}
	t.Logf("cli launcher discovered %d device(s)", len(devs))
	for _, d := range devs {
		t.Logf("  %-12s  %-30s  %s", d.Platform, d.Label, d.UDID)
	}
}

// TestPickMode validates the env-var → mode resolver.
func TestPickMode(t *testing.T) {
	cases := []struct {
		env      string
		wantMode LaunchMode
		wantErr  bool
	}{
		{"", LaunchCLI, false},
		{"cli", LaunchCLI, false},
		{"manual", LaunchManual, false},
		{"appium", LaunchAppium, false}, // returns AppiumLauncher; health check is lazy
		{"bogus", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv("LAUNCH_MODE", tc.env)
			mode, launcher, err := PickMode()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if mode != tc.wantMode {
				t.Errorf("mode=%s want=%s", mode, tc.wantMode)
			}
			if launcher == nil {
				t.Error("launcher is nil for non-error case")
			}
		})
	}
}

func haveAnyDeviceTool() bool {
	if _, err := exec.LookPath("xcrun"); err == nil {
		return true
	}
	if _, err := exec.LookPath("adb"); err == nil {
		return true
	}
	return false
}
