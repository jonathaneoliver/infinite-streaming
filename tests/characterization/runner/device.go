package runner

import "fmt"

// Platform names the player runtime. Used to pick the right CLI launcher
// (xcrun / simctl / adb / chromedp).
type Platform string

const (
	PlatformIPhone    Platform = "iphone"      // real iOS device via xcrun devicectl
	PlatformIPad      Platform = "ipad"        // iPadOS — real device or simulator
	PlatformIPadSim   Platform = "ipad-sim"    // iPad simulator (xcrun simctl)
	PlatformAppleTV   Platform = "appletv"     // tvOS real device via xcrun devicectl
	PlatformAndroidTV Platform = "androidtv"   // Android TV via adb
	PlatformWeb       Platform = "web"         // browser via chromedp
)

// LaunchMode picks the layer that brings the player up.
type LaunchMode int

const (
	// LaunchManual: operator launches everything; framework only observes
	// (polls harness for heartbeat).
	LaunchManual LaunchMode = iota
	// LaunchCLI: framework drives xcrun/simctl/adb to kill+launch. Relies on
	// the app's skipHomeOnLaunch=true + lastPlayed to auto-resume the sweep.
	LaunchCLI
	// LaunchAppium: full UI automation via WebDriver. Needed for content
	// switching, UI assertions, screenshots.
	LaunchAppium
)

func (m LaunchMode) String() string {
	switch m {
	case LaunchManual:
		return "manual"
	case LaunchCLI:
		return "cli"
	case LaunchAppium:
		return "appium"
	default:
		return fmt.Sprintf("LaunchMode(%d)", int(m))
	}
}

// Device is a stable handle on one running (or runnable) player instance.
// The framework treats Device as an opaque target for harness mutations and
// (for CLI/Appium modes) as the address of a process to kill+relaunch.
type Device struct {
	// Platform names the runtime.
	Platform Platform
	// UDID is the platform-native identifier:
	//   - iOS / tvOS real devices: xcrun devicectl identifier (UUID)
	//   - iPad simulator: xcrun simctl UDID
	//   - Android TV: adb serial
	//   - Web: empty (chromedp manages its own profile dir)
	UDID string
	// Label is the human-friendly name reported by the device tool (Apple
	// Configurator name, Android model+serial, etc.). Surfaced in reports.
	Label string
	// BundleID is the app identifier used for kill/launch.
	//   - iOS/tvOS: com.example.InfiniteStreamPlayer
	//   - Android: com.example.infinitestreamplayer
	//   - Web: ignored
	BundleID string
}

// String is for log lines and report headings.
func (d Device) String() string {
	if d.Label != "" {
		return fmt.Sprintf("%s/%s", d.Platform, d.Label)
	}
	if d.UDID != "" {
		return fmt.Sprintf("%s/%s", d.Platform, d.UDID)
	}
	return string(d.Platform)
}
