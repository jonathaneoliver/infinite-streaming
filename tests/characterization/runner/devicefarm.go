package runner

import (
	"context"
	"os"
	"strings"
)

// Device Farm launch path (Stage 1, see tools/appium-device-farm/HANDOFF.md).
//
// When CHAR_DEVICE_FARM=1, the Appium launcher stops hand-picking a UDID and
// offsetting WDA/MJPEG ports off FleetIndex. Instead it requests a device by
// capability (platformName [+ latest platformVersion for sims]) and lets the
// `appium-device-farm` plugin allocate the device, queue when the pool is busy,
// and auto-assign the per-session ports. The non-DF path (plain Appium) stays
// intact as the fallback for CI / no-DF machines.

// deviceFarmEnabled reports whether the Device Farm allocation path is on.
func deviceFarmEnabled() bool {
	return strings.TrimSpace(os.Getenv("CHAR_DEVICE_FARM")) == "1"
}

// dfPlatformVersion returns the appium:platformVersion to pin for a Device-Farm
// session, or "" to leave it unconstrained. Simulators are pinned to the latest
// installed runtime so DF never allocates an old-OS sim (HANDOFF §3.1 / §4.1) —
// overridable via CHAR_DF_IOS_VERSION / CHAR_DF_TVOS_VERSION. Real hardware is
// left unconstrained: a physical device runs whatever OS it runs. A failure to
// compute the latest runtime degrades to "" (unconstrained) rather than failing
// the run — DF then arbitrates among whatever matches the platform.
func dfPlatformVersion(ctx context.Context, p Platform) string {
	switch p {
	case PlatformIPadSim:
		if v := strings.TrimSpace(os.Getenv("CHAR_DF_IOS_VERSION")); v != "" {
			return v
		}
		v, _ := LatestSimRuntimeVersion(ctx, "iOS")
		// Device Farm matches appium:platformVersion against its device `sdk`
		// field, which it reports as the runtime's major.minor (e.g. the
		// iOS-26-4 runtime — whose simctl `version` is "26.4.1" — surfaces as
		// sdk "26.4"). Pinning the full "26.4.1" matches no device and DF
		// queues to a timeout, so we pin major.minor.
		return majorMinor(v)
	case PlatformAppleTV:
		// PlatformAppleTV covers BOTH a real Apple TV and a tvOS sim, and we
		// can't tell which here — so only pin a version when the operator sets
		// an explicit tvOS override. (Stage 1 targets iOS sims; the tvOS pool
		// isn't set up yet — HANDOFF §7.)
		return strings.TrimSpace(os.Getenv("CHAR_DF_TVOS_VERSION"))
	default:
		// Real iOS/iPad hardware, Android — unconstrained.
		return ""
	}
}

// majorMinor truncates a dotted version to its first two segments ("26.4.1" →
// "26.4", "26.5" → "26.5"), matching the major.minor form Device Farm reports
// as a device's sdk and matches platformVersion against. A version with fewer
// than two segments is returned unchanged.
func majorMinor(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}
