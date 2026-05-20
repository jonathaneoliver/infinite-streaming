package modes

import (
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Emergency-downshift: high → near-zero (below the lowest variant's average
// bandwidth) → recovery. Tests whether the player can ride out a connection
// that briefly drops below playable capacity, or whether it stalls.
//
// 30 s @ 8 Mbps → 15 s @ 0.05 Mbps → 90 s @ 8 Mbps. The 0.05 Mbps step is
// below every 360p ladder rung; expect at least one stall on most platforms.
// The interesting metric is *recovery time*: how long after the rate returns
// does the player begin playing again.

func TestEmergencyDownshiftIPadSim(t *testing.T)   { runEmergencyDownshift(t, runner.PlatformIPadSim) }
func TestEmergencyDownshiftIPhone(t *testing.T)    { runEmergencyDownshift(t, runner.PlatformIPhone) }
func TestEmergencyDownshiftAppleTV(t *testing.T)   { runEmergencyDownshift(t, runner.PlatformAppleTV) }
func TestEmergencyDownshiftAndroidTV(t *testing.T) { runEmergencyDownshift(t, runner.PlatformAndroidTV) }

func runEmergencyDownshift(t *testing.T, p runner.Platform) {
	steps := []runner.Step{
		{RateMbps: 8.0, Hold: 30 * time.Second},
		{RateMbps: 0.05, Hold: 15 * time.Second},
		{RateMbps: 8.0, Hold: 90 * time.Second},
	}
	RunMode(t, p, "emergency-downshift", steps, time.Second, 5*time.Second)
}
