//go:build disabled_unchecked
// +build disabled_unchecked

package modes

import (
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Downshift-severity puts the player on a high variant (high cap), then
// drops to a single low cap and holds. The report's profile_shifts counter
// measures how badly the player overshoots — a "clean" downshift settles
// in ≤ ladder-depth shifts; oscillation looks like 4-8+ shifts before
// stability.
//
// 60 s @ 8 Mbps → 120 s @ 1 Mbps.

func TestDownshiftSeverityIPadSim(t *testing.T)   { runDownshiftSeverity(t, runner.PlatformIPadSim) }
func TestDownshiftSeverityIPhone(t *testing.T)    { runDownshiftSeverity(t, runner.PlatformIPhone) }
func TestDownshiftSeverityAppleTV(t *testing.T)   { runDownshiftSeverity(t, runner.PlatformAppleTV) }
func TestDownshiftSeverityAndroidTV(t *testing.T) { runDownshiftSeverity(t, runner.PlatformAndroidTV) }

func runDownshiftSeverity(t *testing.T, p runner.Platform) {
	steps := []runner.Step{
		{RateMbps: 8.0, Hold: 60 * time.Second},
		{RateMbps: 1.0, Hold: 120 * time.Second},
	}
	RunMode(t, p, "downshift-severity", steps, time.Second, 5*time.Second)
}
