package runner

import (
	"fmt"
	"os"
	"strings"
)

// PickMode resolves the LaunchMode + Launcher to use, in priority:
//  1. $LAUNCH_MODE env var (manual | cli | appium)
//  2. default = LaunchCLI
//
// Returns an error for an unknown mode value rather than silently falling
// back, so a typo in CI config doesn't quietly use the wrong launcher.
func PickMode() (LaunchMode, Launcher, error) {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LAUNCH_MODE")))
	switch v {
	case "", "cli":
		return LaunchCLI, NewCLILauncher(), nil
	case "manual":
		return LaunchManual, NewManualLauncher(), nil
	case "appium":
		// Phase 4. Once AppiumLauncher lands, wire it here.
		return LaunchAppium, nil, fmt.Errorf("LAUNCH_MODE=appium not implemented yet (Phase 4)")
	}
	return 0, nil, fmt.Errorf("unknown LAUNCH_MODE=%q (expected manual|cli|appium)", v)
}
