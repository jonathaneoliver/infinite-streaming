package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// CLILauncher drives xcrun (devicectl + simctl) and adb to kill/launch the
// player apps. It does NOT touch the UI — it relies on the apps having
// skipHomeOnLaunch=true so that a cold launch auto-resumes lastPlayed.
//
// Discovery returns the union of (paired Apple devices known to devicectl)
// + (booted iOS/tvOS simulators) + (adb-connected Android devices). The
// caller picks one and hands it to Launch.
type CLILauncher struct {
	// Out is the log sink for progress prints. Defaults to os.Stderr.
	Out io.Writer
	// HeartbeatTimeout is how long Launch waits for the player app to
	// register a heartbeat with the harness after a cold launch. Defaults
	// to 60s.
	HeartbeatTimeout time.Duration
	// BundleIDs maps Platform → app bundle id. Defaults are set in
	// NewCLILauncher; override for tests or alt builds.
	BundleIDs map[Platform]string
}

// NewCLILauncher returns a CLILauncher with the production bundle IDs.
// Override BundleIDs[<platform>] before calling Launch if you need to
// target a TestFlight or local-dev build with a different ID.
func NewCLILauncher() *CLILauncher {
	return &CLILauncher{
		Out:              os.Stderr,
		HeartbeatTimeout: 60 * time.Second,
		BundleIDs: map[Platform]string{
			PlatformIPhone:    "com.jeoliver.InfiniteStreamPlayer",
			PlatformIPad:      "com.jeoliver.InfiniteStreamPlayer",
			PlatformIPadSim:   "com.jeoliver.InfiniteStreamPlayer",
			PlatformAppleTV:   "com.jeoliver.InfiniteStreamPlayerTV",
			PlatformAndroidTV: "com.infinitestream.player",
		},
	}
}

func (c *CLILauncher) Mode() LaunchMode { return LaunchCLI }

func (c *CLILauncher) writer() io.Writer {
	if c.Out != nil {
		return c.Out
	}
	return os.Stderr
}

// Discover returns every device the CLI tools can see. Tools that aren't
// installed (no adb, no xcrun) are silently skipped — we never want a
// missing tool to fail the whole sweep when the operator only uses one
// platform.
func (c *CLILauncher) Discover(ctx context.Context) ([]Device, error) {
	var out []Device
	if devs, err := discoverDevicectl(ctx); err == nil {
		out = append(out, devs...)
	} else {
		fmt.Fprintf(c.writer(), "cli launcher: devicectl discovery: %v\n", err)
	}
	if devs, err := discoverSimctl(ctx); err == nil {
		out = append(out, devs...)
	} else {
		fmt.Fprintf(c.writer(), "cli launcher: simctl discovery: %v\n", err)
	}
	if devs, err := discoverAdb(ctx); err == nil {
		out = append(out, devs...)
	} else {
		fmt.Fprintf(c.writer(), "cli launcher: adb discovery: %v\n", err)
	}
	return out, nil
}

// Launch kills the app (best effort), starts it cold, and waits for the
// player to heartbeat against the harness. Returns the bound Session.
func (c *CLILauncher) Launch(ctx context.Context, d Device) (*Session, error) {
	bundleID := c.BundleIDs[d.Platform]
	if bundleID == "" {
		return nil, fmt.Errorf("cli launcher: no bundle id for platform %s", d.Platform)
	}
	out := c.writer()
	fmt.Fprintf(out, "cli launcher: %s — killing %s\n", d, bundleID)
	if err := killApp(ctx, d, bundleID); err != nil {
		// Kill failures are non-fatal (process may already be dead).
		fmt.Fprintf(out, "cli launcher: kill warning: %v\n", err)
	}
	fmt.Fprintf(out, "cli launcher: %s — launching %s\n", d, bundleID)
	if err := launchApp(ctx, d, bundleID); err != nil {
		return nil, fmt.Errorf("cli launcher: launch %s: %w", d, err)
	}
	// Wait for a fresh harness heartbeat — that's our proof the app came
	// up and resumed lastPlayed.
	deadline := time.Now().Add(c.HeartbeatTimeout)
	for {
		players, err := ListPlayers(ctx)
		if err == nil {
			if p, ok := pickPlayerFor(d, players); ok {
				fmt.Fprintf(out, "cli launcher: %s — heartbeating as %s\n", d, shortID(p.ID))
				return &Session{Device: d, PlayerID: p.ID, Launcher: c}, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("cli launcher: %s did not heartbeat within %s",
				d, c.HeartbeatTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (c *CLILauncher) Kill(ctx context.Context, d Device) error {
	bundleID := c.BundleIDs[d.Platform]
	if bundleID == "" {
		return fmt.Errorf("cli launcher: no bundle id for platform %s", d.Platform)
	}
	return killApp(ctx, d, bundleID)
}

func (c *CLILauncher) Close() error { return nil }

// pickPlayerFor matches a CLI-discovered Device against the harness's player
// list. Match: heartbeating + UA contains the platform hint. When multiple
// match (multiple Apple devices on one network), fall back to label match.
func pickPlayerFor(d Device, players []PlayerRecord) (PlayerRecord, bool) {
	now := time.Now()
	hint := platformUAHint(d.Platform)
	var candidates []PlayerRecord
	for _, p := range players {
		if !p.IsHeartbeating(now) {
			continue
		}
		if hint != "" && !strings.Contains(strings.ToLower(p.UserAgent), hint) {
			continue
		}
		candidates = append(candidates, p)
	}
	switch len(candidates) {
	case 0:
		return PlayerRecord{}, false
	case 1:
		return candidates[0], true
	}
	// Multiple candidates: try label match.
	if d.Label != "" {
		for _, p := range candidates {
			if dev, ok := p.Labels["device"]; ok && dev == d.Label {
				return p, true
			}
		}
	}
	return PlayerRecord{}, false
}

// --- platform dispatch ------------------------------------------------------

func killApp(ctx context.Context, d Device, bundleID string) error {
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformAppleTV:
		return devicectlTerminate(ctx, d.UDID, bundleID)
	case PlatformIPadSim:
		return simctlTerminate(ctx, d.UDID, bundleID)
	case PlatformAndroidTV:
		return adbForceStop(ctx, d.UDID, bundleID)
	}
	return fmt.Errorf("kill: unsupported platform %s", d.Platform)
}

func launchApp(ctx context.Context, d Device, bundleID string) error {
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformAppleTV:
		return devicectlLaunch(ctx, d.UDID, bundleID)
	case PlatformIPadSim:
		return simctlLaunch(ctx, d.UDID, bundleID)
	case PlatformAndroidTV:
		return adbLaunch(ctx, d.UDID, bundleID)
	}
	return fmt.Errorf("launch: unsupported platform %s", d.Platform)
}

// --- xcrun devicectl (real iOS / tvOS) --------------------------------------

type devicectlListResult struct {
	Result struct {
		Devices []struct {
			Identifier         string `json:"identifier"`
			DeviceProperties struct {
				Name string `json:"name"`
			} `json:"deviceProperties"`
			HardwareProperties struct {
				Platform string `json:"platform"`
			} `json:"hardwareProperties"`
			ConnectionProperties struct {
				PairingState string `json:"pairingState"`
			} `json:"connectionProperties"`
		} `json:"devices"`
	} `json:"result"`
}

func discoverDevicectl(ctx context.Context) ([]Device, error) {
	if _, err := exec.LookPath("xcrun"); err != nil {
		return nil, errors.New("xcrun not on $PATH")
	}
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "list", "devices", "--json-output", "-")
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("xcrun devicectl: %w", err)
	}
	var resp devicectlListResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode devicectl: %w", err)
	}
	var out []Device
	for _, d := range resp.Result.Devices {
		platform := mapApplePlatform(d.HardwareProperties.Platform)
		if platform == "" {
			continue
		}
		out = append(out, Device{
			Platform: platform,
			UDID:     d.Identifier,
			Label:    d.DeviceProperties.Name,
		})
	}
	return out, nil
}

func mapApplePlatform(p string) Platform {
	switch strings.ToLower(p) {
	case "ios":
		// devicectl doesn't distinguish iPhone vs iPad in the platform
		// field — keep them as iPhone here; tests that need explicit
		// iPad targeting use the simulator path.
		return PlatformIPhone
	case "tvos":
		return PlatformAppleTV
	}
	return ""
}

// devicectlTerminate kills the process whose executable URL contains the
// bundle's leaf name. devicectl's terminate command needs a PID — there's
// no "by bundle id" shortcut — so we list processes and look up the PID
// matching the bundle's leaf component (the part after the last dot).
// Returns nil when no matching process is found (app already dead = OK).
func devicectlTerminate(ctx context.Context, identifier, bundleID string) error {
	leaf := bundleLeaf(bundleID)
	if leaf == "" {
		return fmt.Errorf("devicectl terminate: bad bundle id %q", bundleID)
	}
	pid, err := devicectlPID(ctx, identifier, leaf)
	if err != nil {
		return err
	}
	if pid == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "process", "terminate",
		"--device", identifier, "--pid", fmt.Sprintf("%d", pid))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("devicectl terminate pid=%d: %w: %s", pid, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func devicectlLaunch(ctx context.Context, identifier, bundleID string) error {
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "process", "launch",
		"--device", identifier, bundleID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("devicectl launch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// devicectlPID returns the PID of a process whose executable URL contains
// /<leaf>.app/. Returns (0, nil) when no match — caller treats that as
// "app not running".
func devicectlPID(ctx context.Context, identifier, leaf string) (int, error) {
	cmd := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "info", "processes",
		"--device", identifier, "--json-output", "-")
	raw, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("devicectl info processes: %w", err)
	}
	var resp struct {
		Result struct {
			RunningProcesses []struct {
				Executable        string `json:"executable"`
				ProcessIdentifier int    `json:"processIdentifier"`
			} `json:"runningProcesses"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return 0, fmt.Errorf("decode processes: %w", err)
	}
	needle := "/" + leaf + ".app/"
	for _, p := range resp.Result.RunningProcesses {
		if strings.Contains(p.Executable, needle) {
			return p.ProcessIdentifier, nil
		}
	}
	return 0, nil
}

// bundleLeaf extracts "InfiniteStreamPlayer" from "com.jeoliver.InfiniteStreamPlayer".
func bundleLeaf(bundleID string) string {
	if i := strings.LastIndex(bundleID, "."); i >= 0 {
		return bundleID[i+1:]
	}
	return bundleID
}

// --- xcrun simctl (iOS / tvOS simulators) ----------------------------------

type simctlListResult struct {
	Devices map[string][]struct {
		UDID                 string `json:"udid"`
		Name                 string `json:"name"`
		State                string `json:"state"`
		DeviceTypeIdentifier string `json:"deviceTypeIdentifier"`
	} `json:"devices"`
}

func discoverSimctl(ctx context.Context) ([]Device, error) {
	if _, err := exec.LookPath("xcrun"); err != nil {
		return nil, errors.New("xcrun not on $PATH")
	}
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "list", "devices", "--json")
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("xcrun simctl: %w", err)
	}
	var resp simctlListResult
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("decode simctl: %w", err)
	}
	var out []Device
	for runtime, devs := range resp.Devices {
		platform := mapSimRuntime(runtime)
		if platform == "" {
			continue
		}
		for _, d := range devs {
			// Only surface booted sims by default. Non-booted entries
			// would force a `simctl boot` before launch, which is slow
			// and changes the operator's UI focus — keep that as an
			// explicit opt-in (Phase 1+).
			if d.State != "Booted" {
				continue
			}
			out = append(out, Device{
				Platform: platform,
				UDID:     d.UDID,
				Label:    d.Name,
			})
		}
	}
	return out, nil
}

func mapSimRuntime(rt string) Platform {
	switch {
	case strings.Contains(rt, "iOS"):
		return PlatformIPadSim
	case strings.Contains(rt, "tvOS"):
		return PlatformAppleTV
	}
	return ""
}

func simctlTerminate(ctx context.Context, udid, bundleID string) error {
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "terminate", udid, bundleID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("simctl terminate: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func simctlLaunch(ctx context.Context, udid, bundleID string) error {
	cmd := exec.CommandContext(ctx, "xcrun", "simctl", "launch", udid, bundleID)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("simctl launch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- adb (Android TV) -------------------------------------------------------

func discoverAdb(ctx context.Context) ([]Device, error) {
	if _, err := exec.LookPath("adb"); err != nil {
		return nil, errors.New("adb not on $PATH")
	}
	cmd := exec.CommandContext(ctx, "adb", "devices", "-l")
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("adb devices: %w", err)
	}
	var out []Device
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "device" {
			// Skip "offline" / "unauthorized" entries.
			continue
		}
		serial := fields[0]
		label := adbLabelFromLong(fields[2:])
		out = append(out, Device{
			Platform: PlatformAndroidTV,
			UDID:     serial,
			Label:    label,
		})
	}
	return out, nil
}

func adbLabelFromLong(fields []string) string {
	// "model:Tab_A8 device:gta8wifi product:gta8wifixx transport_id:1" → "gta8wifi"
	for _, f := range fields {
		if strings.HasPrefix(f, "model:") {
			return strings.TrimPrefix(f, "model:")
		}
	}
	return ""
}

func adbForceStop(ctx context.Context, serial, pkg string) error {
	cmd := exec.CommandContext(ctx, "adb", "-s", serial, "shell", "am", "force-stop", pkg)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("adb force-stop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func adbLaunch(ctx context.Context, serial, pkg string) error {
	// `monkey -p <pkg> -c android.intent.category.LAUNCHER 1` is the
	// portable way to launch an app without knowing its main activity.
	cmd := exec.CommandContext(ctx, "adb", "-s", serial, "shell",
		"monkey", "-p", pkg, "-c", "android.intent.category.LAUNCHER", "1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("adb launch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
