package modes

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/tests/characterization/runner"
)

// resolveFleet resolves the roster of devices a fleet-aware mode runs
// against, in priority order:
//
//  1. CHAR_FLEET_UDIDS="u1,u2,u3" — those exact sims, FleetIndex by position.
//  2. CHAR_FLEET_COUNT=N (N>1)    — the first N available sims of platform p.
//     CHAR_FLEET_AUTOBOOT=1 boots any that aren't running yet; otherwise only
//     already-booted sims are eligible.
//  3. default                     — today's single first-match device,
//     honouring CHARACTERIZATION_DEVICE_UDID. Returns a one-element slice so
//     callers stay byte-for-byte backward-compatible.
//
// FleetIndex is set on every returned device (it seeds the per-sim
// wdaLocalPort/mjpegServerPort in appiumCapabilities). Returns nil only when
// a helper has already issued t.Skip — the caller then returns early.
func resolveFleet(t *testing.T, p runner.Platform) []runner.Device {
	t.Helper()
	if raw := strings.TrimSpace(os.Getenv("CHAR_FLEET_UDIDS")); raw != "" {
		return resolveFleetFromUDIDs(t, p, splitFleetCSV(raw))
	}
	if n := envInt("CHAR_FLEET_COUNT", 1); n > 1 {
		return resolveFleetByCount(t, p, n)
	}
	return resolveSingleDevice(t, p)
}

// resolveSingleDevice reproduces the legacy discover→first-match selection:
// the first device of platform p, honouring CHARACTERIZATION_DEVICE_UDID. It
// runs PickMode purely for the Discover call and discards the launcher — the
// real launcher is minted per subtest in runPyramidOnDevice.
func resolveSingleDevice(t *testing.T, p runner.Platform) []runner.Device {
	t.Helper()
	mode, launcher, err := runner.PickMode()
	if err != nil {
		t.Skipf("PickMode: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	devs, err := launcher.Discover(ctx)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	wantUDID := strings.TrimSpace(os.Getenv("CHARACTERIZATION_DEVICE_UDID"))
	for i := range devs {
		if devs[i].Platform != p {
			continue
		}
		if wantUDID != "" && !strings.EqualFold(devs[i].UDID, wantUDID) {
			continue
		}
		d := devs[i]
		d.FleetIndex = 0
		return []runner.Device{d}
	}
	if wantUDID != "" {
		t.Skipf("no %s device with UDID=%s discovered (mode=%s)", p, wantUDID, mode)
	}
	t.Skipf("no %s device discovered (mode=%s)", p, mode)
	return nil
}

// resolveFleetByCount takes the first N available sims of platform p. With
// CHAR_FLEET_AUTOBOOT=1 it enumerates sims regardless of boot state and boots
// the ones it picks; otherwise it uses only already-booted sims (a fast start
// with no UI-focus surprises).
func resolveFleetByCount(t *testing.T, p runner.Platform, n int) []runner.Device {
	t.Helper()
	autoboot := fleetAutoboot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var pool []runner.Device
	if autoboot {
		all, err := runner.AvailableSims(ctx, p)
		if err != nil {
			t.Fatalf("enumerate available sims: %v", err)
		}
		pool = all
	} else {
		// Already-booted sims only — the default-discovery (Booted) set.
		_, launcher, err := runner.PickMode()
		if err != nil {
			t.Skipf("PickMode: %v", err)
		}
		devs, derr := launcher.Discover(ctx)
		if derr != nil {
			t.Fatalf("discover: %v", derr)
		}
		for _, d := range devs {
			if d.Platform == p {
				pool = append(pool, d)
			}
		}
	}
	if len(pool) < n {
		t.Skipf("CHAR_FLEET_COUNT=%d but only %d %s sim(s) available (autoboot=%v) — boot more sims or set CHAR_FLEET_AUTOBOOT=1",
			n, len(pool), p, autoboot)
	}

	fleet := make([]runner.Device, 0, n)
	for i := 0; i < n; i++ {
		d := pool[i]
		d.FleetIndex = i
		if autoboot {
			if err := runner.BootSim(ctx, d.UDID); err != nil {
				t.Fatalf("boot fleet sim %d %q (%s): %v", i, d.Label, d.UDID, err)
			}
			t.Logf("fleet[%d] booted %s (%s)", i, d.Label, d.UDID)
			seedFleetServer(ctx, t, d)
		}
		fleet = append(fleet, d)
	}
	return fleet
}

// resolveFleetFromUDIDs binds the explicit UDID list to devices (FleetIndex by
// position). Each UDID is resolved against the available-sim set for its label;
// an unknown UDID falls back to a bare device on platform p so explicit
// non-sim targets still work. CHAR_FLEET_AUTOBOOT=1 boots any known sim.
func resolveFleetFromUDIDs(t *testing.T, p runner.Platform, udids []string) []runner.Device {
	t.Helper()
	if len(udids) == 0 {
		t.Skip("CHAR_FLEET_UDIDS set but contained no UDIDs")
	}
	autoboot := fleetAutoboot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	all, err := runner.AvailableSims(ctx, "")
	if err != nil {
		t.Fatalf("enumerate available sims: %v", err)
	}
	byUDID := make(map[string]runner.Device, len(all))
	for _, d := range all {
		byUDID[strings.ToLower(d.UDID)] = d
	}

	fleet := make([]runner.Device, 0, len(udids))
	for i, u := range udids {
		d, known := byUDID[strings.ToLower(u)]
		if !known {
			d = runner.Device{Platform: p, UDID: u}
		}
		d.FleetIndex = i
		if autoboot && known {
			if err := runner.BootSim(ctx, d.UDID); err != nil {
				t.Fatalf("boot fleet sim %d (%s): %v", i, d.UDID, err)
			}
			t.Logf("fleet[%d] booted %s (%s)", i, d.Label, d.UDID)
			seedFleetServer(ctx, t, d)
		}
		fleet = append(fleet, d)
	}
	return fleet
}

func fleetAutoboot() bool {
	return strings.TrimSpace(os.Getenv("CHAR_FLEET_AUTOBOOT")) == "1"
}

// staggerFleetLaunch spreads parallel fleet launches out by fleet index so N
// XCUITest sessions don't all cold-build WDA on one Appium server at the same
// instant — the simultaneous peak is what pushed the 3rd/4th session-create
// past its timeout in a 4-sim run. Index 0 doesn't wait, so single-device runs
// are unaffected. Tunable via CHAR_FLEET_STAGGER_SEC (default 30; 0 disables).
func staggerFleetLaunch(t *testing.T, fleetIndex int) {
	t.Helper()
	if fleetIndex <= 0 {
		return
	}
	per := envInt("CHAR_FLEET_STAGGER_SEC", 30)
	if per <= 0 {
		return
	}
	d := time.Duration(fleetIndex*per) * time.Second
	t.Logf("fleet[%d] staggering launch by %s (avoid simultaneous WDA cold-build)", fleetIndex, d)
	time.Sleep(d)
}

// seedFleetServer writes the harness's server profile into a freshly-booted
// sim's app UserDefaults so it skips the blocking ServerPickerScreen and
// connects straight to HARNESS_BASE_URL. Best-effort: a failure is logged (the
// Appium launcher's in-app picker navigation is the fallback) and never fails
// the run. Opt out with CHAR_FLEET_SEED_SERVER=0. Only sims have a reachable
// data container, so non-sim platforms are skipped.
func seedFleetServer(ctx context.Context, t *testing.T, d runner.Device) {
	t.Helper()
	if d.Platform != runner.PlatformIPadSim {
		return
	}
	if strings.TrimSpace(os.Getenv("CHAR_FLEET_SEED_SERVER")) == "0" {
		return
	}
	bundleID := runner.DefaultBundleID(d.Platform)
	base := runner.HarnessBaseURL()
	if err := runner.SeedServerProfile(ctx, d.UDID, bundleID, base); err != nil {
		t.Logf("fleet[%d] seed server profile: %v (app will fall back to picker navigation)", d.FleetIndex, err)
		return
	}
	t.Logf("fleet[%d] seeded server profile → %s", d.FleetIndex, base)
}

// splitFleetCSV splits a comma-separated UDID list, trimming whitespace and
// dropping empty entries (so trailing commas / spaces are harmless).
func splitFleetCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
