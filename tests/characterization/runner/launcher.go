package runner

import "context"

// Launcher is the interface every launch mode satisfies. Implementations:
//   - ManualLauncher (LaunchManual) — operator drives, framework observes
//   - CLILauncher  (LaunchCLI)     — xcrun/simctl/adb
//   - AppiumLauncher (LaunchAppium) — WebDriver, Phase 4
//
// All methods are best-effort against the operator's environment. Discovery
// failures are reported but should not fatal a sweep that already has a
// running player to talk to.
type Launcher interface {
	// Mode returns the launcher's mode for logging.
	Mode() LaunchMode
	// Discover returns devices the launcher knows how to talk to. For
	// Manual this is "whatever harness sees heartbeating right now"; for
	// CLI this is the union of (heartbeating players) and (paired devices
	// known to xcrun / adb).
	Discover(ctx context.Context) ([]Device, error)
	// Launch starts (or resumes) the player app on the device. Manual mode
	// prompts the operator; CLI mode shells out; Appium mode drives
	// WebDriver. Returns when the player has a fresh heartbeat against the
	// harness — i.e. a usable Session.
	Launch(ctx context.Context, d Device) (*Session, error)
	// Kill terminates the player app. Used for wedge recovery.
	// Manual mode prompts the operator; CLI shells out; Appium calls
	// terminateApp. Returns nil after the kill succeeds; nil is also OK
	// when the process was already dead.
	Kill(ctx context.Context, d Device) error
	// Close releases launcher-side resources (chromedp browser, Appium
	// session). Safe to call multiple times.
	Close() error
}
