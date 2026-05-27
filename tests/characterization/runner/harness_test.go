package runner

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// TestHarnessReachable verifies the harness binary is on $PATH and the
// proxy is responding. Every other test in this module assumes this works;
// fail loudly here so triage is obvious.
func TestHarnessReachable(t *testing.T) {
	if _, err := exec.LookPath(HarnessBin); err != nil {
		t.Skipf("harness binary %q not on $PATH: %v (run `make harness-cli`)", HarnessBin, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	players, err := ListPlayers(ctx)
	if err != nil {
		t.Fatalf("ListPlayers: %v", err)
	}
	t.Logf("harness returned %d player records", len(players))
}

// TestManualDiscover hits the proxy and prints whatever's heartbeating.
// Useful as a developer smoke test ("is the framework wired up?"). Skipped
// if no live player is present — we don't fail CI just because no device
// happens to be running.
func TestManualDiscover(t *testing.T) {
	if _, err := exec.LookPath(HarnessBin); err != nil {
		t.Skipf("harness binary %q not on $PATH", HarnessBin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	m := NewManualLauncher()
	devs, err := m.Discover(ctx)
	if err != nil {
		t.Fatalf("manual discover: %v", err)
	}
	if len(devs) == 0 {
		t.Skip("no heartbeating players — start a player app to exercise this test")
	}
	for _, d := range devs {
		t.Logf("found %s (%s)", d, d.UDID)
	}
}
