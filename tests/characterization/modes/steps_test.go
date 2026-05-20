package modes

import (
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// Steps holds each rate longer than smooth, giving the player time to fully
// settle on a variant before the cap changes. Useful for distinguishing
// "player picked the right variant" from "player was still adapting".
// 6 steps × 30 s. Total runtime ~3 min + warmup.

func TestStepsIPadSim(t *testing.T)   { runSteps(t, runner.PlatformIPadSim) }
func TestStepsIPhone(t *testing.T)    { runSteps(t, runner.PlatformIPhone) }
func TestStepsAppleTV(t *testing.T)   { runSteps(t, runner.PlatformAppleTV) }
func TestStepsAndroidTV(t *testing.T) { runSteps(t, runner.PlatformAndroidTV) }

func runSteps(t *testing.T, p runner.Platform) {
	steps := LinearSteps(6.0, 0.5, 6, 30*time.Second)
	RunMode(t, p, "steps", steps, time.Second, 5*time.Second)
}
