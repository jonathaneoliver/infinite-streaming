package runner

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestPreflight is a diagnostic — not a real test. It checks every
// tool the characterization framework can use, reports what's
// available, and recommends a LAUNCH_MODE matching the operator's
// install. Never fails; just logs. Run with:
//
//   go test -C tests/characterization ./runner/... -v -run TestPreflight
//
// The output answers "what kind of operator am I, and what should
// LAUNCH_MODE be?" without forcing anyone to install tools they
// don't need.
func TestPreflight(t *testing.T) {
	type check struct {
		name      string
		ok        bool
		detail    string
		fix       string // command/instruction to remediate
		platforms []string
	}
	var checks []check

	// harness CLI on $PATH
	harnessPath, harnessErr := exec.LookPath(HarnessBin)
	checks = append(checks, check{
		name:      "harness CLI",
		ok:        harnessErr == nil,
		detail:    harnessPath,
		fix:       "run `make harness-cli` in the repo root",
		platforms: []string{"required (talks to the proxy)"},
	})

	// harness can reach the proxy
	if harnessErr == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, perr := runHarness(ctx, "info")
		cancel()
		detail := "reachable"
		if perr != nil {
			detail = fmt.Sprintf("UNREACHABLE: %v", perr)
		}
		checks = append(checks, check{
			name:      "proxy via harness",
			ok:        perr == nil,
			detail:    detail,
			fix:       "ensure the test-dev proxy is up (make test-deploy-dev) or set HARNESS_BASE_URL",
			platforms: []string{"required for any live test"},
		})
	}

	// xcrun (covers iOS / tvOS real + simulator paths)
	_, xcrunErr := exec.LookPath("xcrun")
	checks = append(checks, check{
		name:      "xcrun",
		ok:        xcrunErr == nil,
		detail:    "Xcode command-line tools",
		fix:       "install Xcode (App Store) or `xcode-select --install`",
		platforms: []string{"iPad sim, iPhone sim, iPhone real, Apple TV real"},
	})

	// adb (Android TV)
	_, adbErr := exec.LookPath("adb")
	checks = append(checks, check{
		name:      "adb",
		ok:        adbErr == nil,
		detail:    "Android Platform Tools",
		fix:       "`brew install --cask android-platform-tools` (macOS)",
		platforms: []string{"Android TV"},
	})

	// Appium server reachable (for Apple TV automation + screenshots)
	appiumURL := os.Getenv("APPIUM_URL")
	if appiumURL == "" {
		appiumURL = "http://localhost:4723"
	}
	appiumOK, appiumDetail := probeAppium(appiumURL)
	checks = append(checks, check{
		name:      "Appium server",
		ok:        appiumOK,
		detail:    appiumDetail,
		fix:       "`npm install -g appium && appium` (only needed if you want Apple TV auto-wake / screenshots)",
		platforms: []string{"optional — adds Apple TV reliability + per-step screenshots"},
	})

	// --- emit -----------------------------------------------------

	t.Log("")
	t.Log("PREFLIGHT — what your environment supports")
	t.Log("")
	for _, c := range checks {
		mark := "✗"
		if c.ok {
			mark = "✓"
		}
		t.Logf("  %s %-20s %s", mark, c.name, c.detail)
		t.Logf("        platforms: %s", strings.Join(c.platforms, ", "))
		if !c.ok {
			t.Logf("        fix:       %s", c.fix)
		}
	}

	// Discover what's actually wired right now
	t.Log("")
	t.Log("DEVICES (currently discoverable)")
	t.Log("")
	cl := NewCLILauncher()
	cl.Out = &silentWriter{} // drop the launcher's own progress logging
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if devs, err := cl.Discover(ctx); err == nil && len(devs) > 0 {
		for _, d := range devs {
			t.Logf("  %-12s  %-30s  %s", d.Platform, d.Label, d.UDID)
		}
	} else {
		t.Log("  (none reachable — see fixes above)")
	}

	// Recommend a mode.
	t.Log("")
	mode, reason := recommendMode(xcrunErr == nil, adbErr == nil, appiumOK, harnessErr == nil)
	t.Logf("RECOMMENDED LAUNCH_MODE=%s", mode)
	t.Logf("  why: %s", reason)
	t.Log("")
	t.Logf("Usage examples:")
	if mode == "manual" {
		t.Logf("  open Chrome to https://dev.jeoliver.com:21000/dashboard/testing-session.html")
		t.Logf("  go test -C tests/characterization ./modes/... -v -run TestSmoothWeb -timeout 90m -count=1")
	}
	if xcrunErr == nil {
		t.Logf("  go test -C tests/characterization ./modes/... -v -run TestSmoothIPadSim -timeout 90m -count=1")
	}
	if appiumOK {
		t.Logf("  go test -C tests/characterization ./modes/... -v -run TestSmoothAppleTV -timeout 90m -count=1 -launch-mode=appium")
	}
}

type silentWriter struct{}

func (silentWriter) Write(p []byte) (int, error) { return len(p), nil }

func probeAppium(url string) (bool, string) {
	hc := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := hc.Get(url + "/status")
	if err != nil {
		return false, fmt.Sprintf("not reachable at %s", url)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return true, "reachable at " + url
	}
	return false, fmt.Sprintf("%s returned HTTP %d", url, resp.StatusCode)
}

func recommendMode(haveXcrun, haveAdb, haveAppium, haveHarness bool) (mode, reason string) {
	if !haveHarness {
		return "(none)", "install harness CLI first — nothing works without it"
	}
	// Appium adds capability (Apple TV automation + screenshots) but
	// CLI is sufficient for sim + real iOS + Android TV. Only
	// recommend appium when xcrun isn't present (otherwise CLI is
	// simpler and faster for the common case).
	if haveXcrun || haveAdb {
		base := "cli"
		bonus := ""
		if haveAppium {
			bonus = " — add -launch-mode=appium for Apple TV automation or screenshots when needed"
		}
		return base, "Xcode and/or adb available; CLI handles sim + real iOS + Android TV" + bonus
	}
	return "manual", "no device tooling installed — use Chrome + the web testing page, framework prompts you to launch playback"
}
