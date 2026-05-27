//go:build disabled_unchecked
// +build disabled_unchecked

package modes

import (
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Hysteresis-gap stairs the cap UP slowly from 0.5 Mbps to 6 Mbps, then
// back DOWN. By comparing the rate at which the player upshifted onto each
// variant on the way up vs. the rate at which it downshifted off it on
// the way back, the report reveals the deadband (hysteresis gap) for each
// variant boundary.
//
// 14 steps × 25 s ≈ 6 min + warmup.

func TestHysteresisGapIPadSim(t *testing.T)   { runHysteresisGap(t, runner.PlatformIPadSim) }
func TestHysteresisGapIPhone(t *testing.T)    { runHysteresisGap(t, runner.PlatformIPhone) }
func TestHysteresisGapAppleTV(t *testing.T)   { runHysteresisGap(t, runner.PlatformAppleTV) }
func TestHysteresisGapAndroidTV(t *testing.T) { runHysteresisGap(t, runner.PlatformAndroidTV) }

func runHysteresisGap(t *testing.T, p runner.Platform) {
	up := LinearSteps(0.5, 6.0, 7, 25*time.Second)
	down := LinearSteps(6.0, 0.5, 7, 25*time.Second)
	steps := append([]runner.Step{}, up...)
	steps = append(steps, down...)
	RunMode(t, p, "hysteresis-gap", steps, time.Second, 5*time.Second)
}
