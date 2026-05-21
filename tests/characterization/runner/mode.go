package runner

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// launchModeFlag is a `go test` flag — `go test … -launch-mode=appium`.
// Lets the operator pick a launch mode without prefixing the command
// with a `LAUNCH_MODE=…` env var, which would knock `go` off the first
// token of the bash invocation and break the project's command-allowlist
// in Claude Code (and similar tooling).
var launchModeFlag = flag.String("launch-mode", "",
	"manual|cli|appium — overrides $LAUNCH_MODE when set")

// PickMode resolves the LaunchMode + Launcher to use, in priority:
//  1. -launch-mode test flag (preferred — first token stays `go`)
//  2. $LAUNCH_MODE env var
//  3. default = LaunchCLI
//
// Returns an error for an unknown mode value rather than silently falling
// back, so a typo in CI config doesn't quietly use the wrong launcher.
func PickMode() (LaunchMode, Launcher, error) {
	v := strings.ToLower(strings.TrimSpace(*launchModeFlag))
	if v == "" {
		v = strings.ToLower(strings.TrimSpace(os.Getenv("LAUNCH_MODE")))
	}
	switch v {
	case "", "cli":
		return LaunchCLI, NewCLILauncher(), nil
	case "manual":
		return LaunchManual, NewManualLauncher(), nil
	case "appium":
		// Minimum-viable AppiumLauncher: WebDriver-protocol launch +
		// kill + screenshot, no UI automation yet. Health check is
		// lazy (happens at Launch time) so a missing Appium server
		// only surfaces when actually used.
		return LaunchAppium, NewAppiumLauncher(), nil
	}
	return 0, nil, fmt.Errorf("unknown LAUNCH_MODE=%q (expected manual|cli|appium)", v)
}
