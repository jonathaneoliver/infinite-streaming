package modes

import (
	"os"
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// TestMain is the clean-exit half of the session-leak safety net (the signal
// backstop in runner/interrupt_release.go is the killed-exit half). After every
// test in this package finishes — normally OR after a t.Fatal — it frees any
// appium/WDA session (device-farm slot) and proxy session (config-on-connect
// slot) a test forgot to release, so a leaked slot never wedges the next run.
// No-op when every test cleaned up after itself, which is the intent.
func TestMain(m *testing.M) {
	code := m.Run()
	runner.ReleaseAllRegistered()
	os.Exit(code)
}
