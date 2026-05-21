//go:build disabled_unchecked
// +build disabled_unchecked

package modes

import (
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Transient-shock: high → brief dip → high. Tests whether the player can
// ride out a short bandwidth dip on buffered media without downshifting.
// 30 s @ 8 Mbps, 10 s @ 0.5 Mbps, 60 s @ 8 Mbps.
//
// The dip is short enough that a well-buffered player should NOT downshift —
// any profile_shifts >0 in the report is the player being too eager.

func TestTransientShockIPadSim(t *testing.T)   { runTransientShock(t, runner.PlatformIPadSim) }
func TestTransientShockIPhone(t *testing.T)    { runTransientShock(t, runner.PlatformIPhone) }
func TestTransientShockAppleTV(t *testing.T)   { runTransientShock(t, runner.PlatformAppleTV) }
func TestTransientShockAndroidTV(t *testing.T) { runTransientShock(t, runner.PlatformAndroidTV) }

func runTransientShock(t *testing.T, p runner.Platform) {
	steps := []runner.Step{
		{RateMbps: 8.0, Hold: 30 * time.Second},
		{RateMbps: 0.5, Hold: 10 * time.Second},
		{RateMbps: 8.0, Hold: 60 * time.Second},
	}
	RunMode(t, p, "transient-shock", steps, time.Second, 5*time.Second)
}
