package runner

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// AppiumLauncher drives an Appium server (default http://localhost:4723)
// via the WebDriver protocol to launch + kill player apps. Minimum-
// viable scope: relies on skipHomeOnLaunch / lastPlayed in each app
// for auto-resume (same as CLILauncher). The added value over CLI is:
//
//   - Apple TV wake-from-sleep before launch (devicectl can fail with
//     "system is asleep" — Appium drives the device's wake)
//   - Screenshot capture per step (saved to artifacts)
//   - Foundation for Phase 4b: content selection, settings UI tests,
//     911 / Reload button taps. Those need accessibility identifiers in
//     the apps; not implemented in this minimum cut.
//
// Implementation talks WebDriver JSON over stdlib net/http — no
// external dep. The Selenium / Appium client libraries add a lot for
// little value at this scope; revisit when we need more of the API.
type AppiumLauncher struct {
	// URL is the Appium server base, e.g. "http://localhost:4723".
	// Override with $APPIUM_URL.
	URL string
	// HeartbeatTimeout — how long after Launch we wait for a harness
	// player heartbeat before failing. Defaults to 90 s.
	HeartbeatTimeout time.Duration
	// BundleIDs maps Platform → app bundle id. Same defaults as
	// CLILauncher; override for TestFlight / local-dev builds.
	BundleIDs map[Platform]string
	// closeBeat — pause after each close tap / back press so the app
	// can navigate and fire its terminal metrics POST before the next
	// probe (and before session teardown). 800ms in production; tests
	// shrink it. #627/#660.
	closeBeat time.Duration

	// launchArgs — process arguments passed to the app on launch
	// (XCUITest `appium:processArguments`). iOS maps `-key value` args
	// into UserDefaults' NSArgumentDomain (highest precedence), so the
	// segment axis forces e.g. `-is.segment s2` for a single, deterministic
	// cold launch on that segment — no UI pre-set, no second session.
	// Set via SetLaunchArgs before the launch. #segments.
	launchArgs []string

	mu       sync.Mutex
	sessions map[string]string // device UDID → Appium session id
	hc       *http.Client
}

// NewAppiumLauncher returns a launcher pointing at the configured server.
func NewAppiumLauncher() *AppiumLauncher {
	url := os.Getenv("APPIUM_URL")
	if url == "" {
		url = "http://localhost:4723"
	}
	return &AppiumLauncher{
		URL:              url,
		HeartbeatTimeout: 90 * time.Second,
		closeBeat:        800 * time.Millisecond,
		BundleIDs:        cloneBundleIDs(),
		sessions:         map[string]string{},
		// 180s, not 60s: a session-create cold-builds WDA on the sim, and an
		// N-sim fleet queues N of those on one Appium server — the later ones
		// blow past 60s. doRequest passes the caller's ctx (NewRequestWithContext)
		// so per-call deadlines still apply; this is just the backstop ceiling.
		hc: &http.Client{Timeout: 180 * time.Second},
	}
}

func (a *AppiumLauncher) Mode() LaunchMode { return LaunchAppium }

// Discover returns devices Appium can target. We delegate to xcrun /
// adb (the same source CLILauncher uses) — Appium doesn't expose its
// own discovery API. If the platform tools aren't installed we silently
// skip them so a partial install still works for the device the
// operator has wired.
//
// We deliberately DON'T health-check the Appium server here — a
// LAUNCH_MODE=cli user shouldn't pay the connect cost. Health check
// happens at Launch time so the error surfaces only when Appium is
// actually being used.
func (a *AppiumLauncher) Discover(ctx context.Context) ([]Device, error) {
	var out []Device
	if devs, err := discoverDevicectl(ctx); err == nil {
		out = append(out, devs...)
	}
	if devs, err := discoverSimctl(ctx); err == nil {
		out = append(out, devs...)
	}
	if devs, err := discoverAdb(ctx); err == nil {
		out = append(out, devs...)
	}
	return out, nil
}

// Launch starts a WebDriver session, drives the UI to home, then resumes
// playback. Convenience for callers that don't need to inject anything
// between those two phases. Tests that want to *throttle before
// playback* (e.g. rampup, pyramid) call LaunchToHome + ResumePlayback
// separately, with their own ApplyRate in between.
func (a *AppiumLauncher) Launch(ctx context.Context, d Device) (*Session, error) {
	sess, err := a.LaunchToHome(ctx, d)
	if err != nil {
		return nil, err
	}
	if err := a.ResumePlayback(ctx, d); err != nil {
		return nil, err
	}
	if err := a.waitForHeartbeat(ctx, sess); err != nil {
		return nil, err
	}
	return sess, nil
}

// LaunchToHome opens the Appium session, terminates+relaunches the app
// (via forceAppLaunch capability), then taps the back chevron so we
// land on the home picker — playback NOT yet started. The returned
// Session has PlayerID set to "" because no heartbeat is expected at
// this stage; call ResumePlayback to start playback and bind the
// player_id.
//
// Use case: rampup / pyramid where the test wants to ApplyRate(floor)
// BEFORE the first segment fetch, so playback starts cold under the
// constraint instead of cliff-diving from "no cap" to a low cap.
// SetLaunchArgs sets process arguments applied to every subsequent app
// launch (see the launchArgs field). Pass nil to clear. #segments uses
// this to force the segment via `-is.segment <rawValue>` on cold launch.
func (a *AppiumLauncher) SetLaunchArgs(args []string) {
	a.mu.Lock()
	a.launchArgs = args
	a.mu.Unlock()
}

// baselineTestFlags are config-on-startup flags EVERY characterization launch
// forces to a known value, so a sim's stale persisted setting can't silently
// leak into a run. These launch args land in NSArgumentDomain, which outranks
// the app's persistent UserDefaults — so `-is.flag.4k true` overrides a sim
// whose "4K" toggle was left off (it would otherwise cap itself at 1080p), and
// `-is.flag.peak_bitrate_mbps 0` clears a leftover startup peak-bitrate clamp
// from a prior capped run (e.g. pyramid's floor clamp). A mode that sets one of
// these explicitly wins — withBaselineTestFlags only fills the ones it omitted.
var baselineTestFlags = [][2]string{
	{"-is.flag.4k", "true"},
	{"-is.flag.peak_bitrate_mbps", "0"},
}

func withBaselineTestFlags(args []string) []string {
	present := map[string]bool{}
	for i := 0; i+1 < len(args); i += 2 {
		present[args[i]] = true
	}
	out := append([]string{}, args...)
	for _, kv := range baselineTestFlags {
		if !present[kv[0]] {
			out = append(out, kv[0], kv[1])
		}
	}
	return out
}

// intentExtrasFromLaunchArgs converts `-is.X Y` launch-arg pairs into the
// `--es is.X Y` form Android's appium:optionalIntentArguments expects (String
// intent extras appended to the start intent). Values in the #714 vocab
// (UUIDs, "s2", "0") are space-free, so no quoting is needed.
func intentExtrasFromLaunchArgs(args []string) string {
	var b strings.Builder
	for i := 0; i+1 < len(args); i += 2 {
		key := strings.TrimPrefix(args[i], "-")
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "--es %s %s", key, args[i+1])
	}
	return b.String()
}

func (a *AppiumLauncher) LaunchToHome(ctx context.Context, d Device) (*Session, error) {
	bundleID := a.BundleIDs[d.Platform]
	if bundleID == "" {
		return nil, fmt.Errorf("appium launcher: no bundle id for platform %s", d.Platform)
	}
	if err := a.healthCheck(ctx); err != nil {
		return nil, fmt.Errorf("appium server not reachable at %s: %w (start with `appium`, or unset LAUNCH_MODE=appium)", a.URL, err)
	}
	caps := appiumCapabilities(d, bundleID)
	// Always fold in the baseline test flags (4K on, peak clamp off unless the
	// mode set it) so a sim's stale persisted UserDefaults can't leak in.
	effectiveArgs := withBaselineTestFlags(a.launchArgs)
	if len(effectiveArgs) > 0 {
		if d.Platform == PlatformAndroidTV {
			// UiAutomator2 ignores processArguments. Deliver the launch args
			// as intent extras appended to the start intent: `-is.X Y` →
			// `--es is.X Y`. The Android app reads `is.player_id` off the
			// launch intent (config-on-connect, #714), mirroring how iOS reads
			// it from NSArgumentDomain.
			caps["appium:optionalIntentArguments"] = intentExtrasFromLaunchArgs(effectiveArgs)
		} else {
			// XCUITest passes these to the app on launch; iOS folds `-key value`
			// pairs into UserDefaults (NSArgumentDomain). #segments forces the
			// segment this way so a single cold launch lands on it.
			caps["appium:processArguments"] = map[string]any{"args": effectiveArgs}
		}
	}
	sessID, err := a.createSession(ctx, caps)
	if err != nil {
		return nil, fmt.Errorf("appium create session: %w", err)
	}
	a.mu.Lock()
	a.sessions[d.UDID] = sessID
	a.mu.Unlock()

	// A freshly-installed/erased sim can come up on the blocking
	// ServerPickerScreen (no saved server) instead of playback/home. Drive
	// past it by adding the harness's server URL. No-op when a server is
	// already saved (the seeded common path). iOS only.
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformIPadSim:
		if err := a.navigateServerPickerIfPresent(ctx, sessID, bootstrapBaseURL()); err != nil {
			return nil, fmt.Errorf("server picker navigation: %w", err)
		}
	}

	// Drive the UI back to home. Best-effort — if we're already on
	// home (skipHomeOnLaunch=false, or some other path) the back button
	// element isn't visible and the tap is a no-op.
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformIPadSim:
		_ = a.tapByAccessibilityID(ctx, sessID, "playback-back-button")
		time.Sleep(800 * time.Millisecond)
	}
	return &Session{Device: d, Launcher: a}, nil
}

// ResumePlayback taps the home-continue-watching tile to start
// playback. Caller has had the chance to ApplyRate (or any other
// pre-playback setup) since LaunchToHome returned. Does NOT wait for a
// heartbeat — Launch does that as part of its convenience flow; tests
// that drive the phases separately call Session.WaitForHeartbeat
// themselves afterward.
func (a *AppiumLauncher) ResumePlayback(ctx context.Context, d Device) error {
	a.mu.Lock()
	sessID := a.sessions[d.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return errors.New("ResumePlayback: no active appium session for device")
	}
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformIPadSim, PlatformAndroidTV:
		// Wait for the continue-watching control to render before tapping —
		// the catalogue fetch is async, so a just-forced-to-Home screen
		// can show an empty content row for a few seconds (observed after
		// a fresh launch / segment-forced launch); tapping immediately
		// 404s on the not-yet-rendered tile. On Android the id is the
		// content-desc on the hero's Resume button — same "accessibility
		// id" locator (UiAutomator2 maps it to content-desc).
		elID, err := a.waitForAccessibilityID(ctx, sessID, "home-continue-watching", 30*time.Second)
		if err != nil {
			return fmt.Errorf("wait for home-continue-watching: %w", err)
		}
		if err := a.clickElement(ctx, sessID, elID); err != nil {
			return fmt.Errorf("tap home-continue-watching: %w", err)
		}
	}
	return nil
}

// ResumePlaybackClip starts playback of a SPECIFIC clip by tapping its
// home-tile-<clipID> tile (waiting for that tile to render first), instead of
// the continue-watching hero. The hero resolves to lastPlayed only AFTER the
// catalogue loads; until then it falls back to the featured clip, so a pinned
// run (CHAR_CONTENT) racing the hero can land on the wrong content. Tapping the
// clip-specific tile is deterministic about WHICH content plays. Empty clipID,
// or the tile never rendering, falls back to ResumePlayback so the run isn't
// dead in the water.
func (a *AppiumLauncher) ResumePlaybackClip(ctx context.Context, d Device, clipID string) error {
	if clipID == "" {
		return a.ResumePlayback(ctx, d)
	}
	a.mu.Lock()
	sessID := a.sessions[d.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return errors.New("ResumePlaybackClip: no active appium session for device")
	}
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformIPadSim, PlatformAndroidTV:
		id := "home-tile-" + clipID
		elID, err := a.waitForAccessibilityID(ctx, sessID, id, 30*time.Second)
		if err != nil {
			// Tile never rendered (clip absent from the LIVE row / catalogue not
			// loaded) — fall back to the continue-watching hero.
			return a.ResumePlayback(ctx, d)
		}
		if err := a.clickElement(ctx, sessID, elID); err != nil {
			return fmt.Errorf("tap %s: %w", id, err)
		}
	}
	return nil
}

// WaitForBind polls the harness players list until the device shows
// up as heartbeating, then sets sess.PlayerID. Used by tests that
// drive launch in phases (LaunchToHome → ResumePlayback → bind) and
// don't have a known player_id to pre-set on the session.
func (a *AppiumLauncher) WaitForBind(ctx context.Context, sess *Session) error {
	return a.waitForHeartbeat(ctx, sess)
}

// waitForHeartbeat polls the harness players list until the device
// shows up as heartbeating, then binds its player_id to the session.
func (a *AppiumLauncher) waitForHeartbeat(ctx context.Context, sess *Session) error {
	deadline := time.Now().Add(a.HeartbeatTimeout)
	for {
		players, err := ListPlayers(ctx)
		if err == nil {
			if p, ok := pickPlayerFor(sess.Device, players); ok {
				sess.PlayerID = p.ID
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("appium launcher: %s did not heartbeat within %s",
				sess.Device, a.HeartbeatTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Kill terminates the player app via Appium. Falls back to closing the
// session entirely if terminate isn't supported on this driver.
func (a *AppiumLauncher) Kill(ctx context.Context, d Device) error {
	bundleID := a.BundleIDs[d.Platform]
	a.mu.Lock()
	sessID := a.sessions[d.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return nil // never launched; nothing to kill
	}
	// POST /session/{id}/appium/device/terminate_app
	body := map[string]string{"appId": bundleID, "bundleId": bundleID}
	_, err := a.doRequest(ctx, "POST",
		fmt.Sprintf("/session/%s/appium/device/terminate_app", sessID), body)
	return err
}

// SetSegmentLength switches the app's Segment Length via the settings UI
// (#630) so a sweep can run e.g. 6s then 2s back-to-back on one device.
// value is "2s" / "6s" / "ll" — matching the segment-<value> accessibility
// ids the app exposes (added in #630). Drives the real UI: settings button
// → Segment-length row → the value → back out of the drawer. Changing the
// segment rebuilds the manifest and rotates play_id, so the caller should
// re-bind (WaitForBind) and treat what follows as a fresh play.
//
// iOS only. Returns an error if a tap target is missing — typically an app
// that predates #630 (rebuild + redeploy so the AX ids are present).
func (a *AppiumLauncher) SetSegmentLength(ctx context.Context, d Device, value string) error {
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformIPadSim:
	default:
		return fmt.Errorf("SetSegmentLength: unsupported platform %s", d.Platform)
	}
	a.mu.Lock()
	sessID := a.sessions[d.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return errors.New("SetSegmentLength: no active appium session for device")
	}
	steps := []struct{ what, id string }{
		{"open settings", "playback-settings-button"},
		{"open segment picker", "settings-row-segment"},
		{"select " + value, "segment-" + value},
		{"close settings", "settings-back-button"},
	}
	for _, s := range steps {
		if err := a.tapByAccessibilityID(ctx, sessID, s.id); err != nil {
			return fmt.Errorf("SetSegmentLength %q: %s (%s): %w", value, s.what, s.id, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

// ClosePlaybackViaUI closes the playback screen the way a user does —
// tapping the back chevron on iOS, or pressing system Back on Android —
// so the app runs its normal exit path (endSessionForUserBack) and emits
// a real client play_end (#627). The app then sits on the home picker and
// rotates its play_id on the next ResumePlayback, so the just-ended play
// is bounded and cleanly terminal in the sessions view instead of
// dangling in_progress after a hard terminate_app.
//
// Best-effort and idempotent: if no session exists, or the back
// affordance isn't present (already on home / play already ended), it's a
// no-op rather than an error where possible. Satisfies runner.UICloser.
func (a *AppiumLauncher) ClosePlaybackViaUI(ctx context.Context, d Device) error {
	a.mu.Lock()
	sessID := a.sessions[d.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return nil // never launched; nothing to close
	}
	switch d.Platform {
	case PlatformIPhone, PlatformIPad, PlatformIPadSim:
		// The iOS playback overlay's back chevron — the same element
		// LaunchToHome taps — invokes vm.endSessionForUserBack() before
		// navigating home, which is what emits play_end.
		//
		// #660 — tap-verify-retry: the chevron stays FINDABLE in the AX
		// tree while the controls overlay is auto-hidden, so a click can
		// "succeed" yet only reveal the overlay instead of activating
		// the button (observed at the end of a 36-min pyramid run; the
		// play kept streaming and dangled in_progress). The chevron does
		// NOT exist on the home screen, so find-fails is the reliable
		// "screen closed" probe — the same property LaunchToHome's
		// best-effort tap relies on. Tap, give the app a beat, re-probe;
		// still findable means the previous tap only woke the overlay,
		// so tap again (now hittable). Surface an error if the screen
		// never closes so the cleanup logs instead of silently no-opping.
		const maxCloseAttempts = 3
		for attempt := 1; attempt <= maxCloseAttempts; attempt++ {
			elementID, ferr := a.findByAccessibilityID(ctx, sessID, "playback-back-button")
			if ferr != nil {
				// Chevron not in the tree: already on home (first probe)
				// or the previous tap closed the screen. Either way the
				// post-tap beat below has already let the terminal
				// metrics POST fire.
				return nil
			}
			if cerr := a.clickElement(ctx, sessID, elementID); cerr != nil {
				return fmt.Errorf("close playback: click attempt %d: %w", attempt, cerr)
			}
			// Give the app a beat to navigate + fire its terminal metrics
			// POST before the re-probe (and before the caller tears the
			// Appium session and the app's network path down).
			time.Sleep(a.closeBeat)
		}
		return fmt.Errorf("close playback: screen still open after %d back taps (hidden-controls overlay?)", maxCloseAttempts)
	case PlatformAndroidTV:
		// Android has no visible back button; the playback screen's
		// Compose BackHandler calls endSessionForUserBack() on system
		// Back, so drive the W3C/UiAutomator2 back press.
		err := a.pressBack(ctx, sessID)
		// Same post-close beat as iOS — terminal POST before teardown.
		time.Sleep(a.closeBeat)
		return err
	default:
		return nil // tvOS (onExitCommand) / web not driven here yet
	}
}

// pressBack issues the W3C "back" navigation (Android system Back) on the
// given Appium session.
func (a *AppiumLauncher) pressBack(ctx context.Context, sessID string) error {
	_, err := a.doRequest(ctx, "POST", "/session/"+sessID+"/back", map[string]any{})
	return err
}

// wdaRunnerExecutableMatch is a substring of the WebDriverAgent runner's
// executable URL on a real device. The runner ships as
// WebDriverAgentRunner-Runner.app, so its on-device executable name shares
// this prefix — unlike its bundle-id leaf ("xctrunner"), which is why the
// bundle-leaf lookup in devicectlTerminate can't find it.
const wdaRunnerExecutableMatch = "WebDriverAgent"

// ReleaseDevice fully releases a real iOS device after a run by
// terminating the WebDriverAgent runner, so iOS's system "Automation
// Running" overlay clears. Appium leaves WDA resident between sessions
// (useNewWDA=false) for fast reuse, so ending the WebDriver session does
// NOT stop WDA — we shell to devicectl, the same tool the CLI launcher
// uses for real devices. No-op for simulators (no overlay, and devicectl
// can't target them) and non-iOS platforms; best-effort, since
// terminating a WDA that isn't running is itself a no-op. Satisfies
// runner.DeviceReleaser. Gated opt-in by the caller (Session.ReleaseDevice
// reads CHAR_RELEASE_DEVICE) so it never kills WDA mid-suite.
func (a *AppiumLauncher) ReleaseDevice(ctx context.Context, d Device) error {
	switch d.Platform {
	case PlatformIPhone, PlatformIPad:
		return devicectlTerminateMatching(ctx, d.UDID, wdaRunnerExecutableMatch)
	default:
		return nil
	}
}

// Screenshot saves a PNG of the device's current screen to path.
// Returns the path on success. Intended to be called from a sweep
// runner to attach visual context to interesting steps.
func (a *AppiumLauncher) Screenshot(ctx context.Context, d Device, path string) (string, error) {
	a.mu.Lock()
	sessID := a.sessions[d.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return "", errors.New("screenshot: no active session for device")
	}
	raw, err := a.doRequest(ctx, "GET", fmt.Sprintf("/session/%s/screenshot", sessID), nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Value string `json:"value"` // base64 PNG
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	png, err := base64.StdEncoding.DecodeString(resp.Value)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, png, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Close tears down every Appium session this launcher opened.
func (a *AppiumLauncher) Close() error {
	a.mu.Lock()
	sessions := make(map[string]string, len(a.sessions))
	for k, v := range a.sessions {
		sessions[k] = v
	}
	a.sessions = map[string]string{}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var firstErr error
	for _, sessID := range sessions {
		if _, err := a.doRequest(ctx, "DELETE", "/session/"+sessID, nil); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// readAccessibilityValue finds an element by its accessibility id and
// returns its `value` attribute. Used to read out the persistent
// player_id from the iOS app's home screen BEFORE tapping into
// playback — see ReadPlayerID below.
func (a *AppiumLauncher) readAccessibilityValue(ctx context.Context, sessID, id, attr string) (string, error) {
	findBody := map[string]any{"using": "accessibility id", "value": id}
	raw, err := a.doRequest(ctx, "POST", "/session/"+sessID+"/element", findBody)
	if err != nil {
		return "", err
	}
	var findResp struct {
		Value map[string]string `json:"value"`
	}
	if err := json.Unmarshal(raw, &findResp); err != nil {
		return "", fmt.Errorf("decode find element %q: %w", id, err)
	}
	var elementID string
	for _, v := range findResp.Value {
		elementID = v
		break
	}
	if elementID == "" {
		return "", fmt.Errorf("find element %q returned no id", id)
	}
	// Which attribute carries the value differs by driver: XCUITest exposes
	// the iOS accessibilityValue under "value"; UiAutomator2 exposes the
	// Compose node's text under "text".
	raw, err = a.doRequest(ctx, "GET",
		fmt.Sprintf("/session/%s/element/%s/attribute/%s", sessID, elementID, attr), nil)
	if err != nil {
		return "", fmt.Errorf("read value %q: %w", id, err)
	}
	var valResp struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &valResp); err != nil {
		return "", fmt.Errorf("decode value %q: %w", id, err)
	}
	return strings.TrimSpace(valResp.Value), nil
}

// TapTileByClipID taps a specific content tile in the Home screen's
// LIVE row, identified by its clip_id (e.g. "bucks_bunny"). Used by
// the startup characterization test's channel_change mode to switch
// from one content item to a different one without going through the
// Continue Watching shortcut.
//
// Requires the iOS app to surface `home-tile-<clip_id>` accessibility
// identifiers on each LivePreviewTile (see
// .claude/standards/startup-characterization-test.md).
// TapByAccessibilityID is the public wrapper around tapByAccessibilityID
// for tests that need to drive arbitrary AX-tagged UI (Retry / Reload /
// 911 / settings buttons) outside the home-tile flow.
//
// Returns an error if no element with the given identifier is visible
// — useful for tests that conditionally tap (e.g. "tap retry only when
// state went to paused").
func (a *AppiumLauncher) TapByAccessibilityID(ctx context.Context, sess *Session, id string) error {
	if sess == nil {
		return errors.New("TapByAccessibilityID: nil session")
	}
	if id == "" {
		return errors.New("TapByAccessibilityID: empty id")
	}
	a.mu.Lock()
	sessID := a.sessions[sess.Device.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return errors.New("TapByAccessibilityID: no active appium session for device")
	}
	return a.tapByAccessibilityID(ctx, sessID, id)
}

func (a *AppiumLauncher) TapTileByClipID(ctx context.Context, sess *Session, clipID string) error {
	if sess == nil {
		return errors.New("TapTileByClipID: nil session")
	}
	if clipID == "" {
		return errors.New("TapTileByClipID: empty clip_id")
	}
	a.mu.Lock()
	sessID := a.sessions[sess.Device.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return errors.New("TapTileByClipID: no active appium session for device")
	}
	return a.tapByAccessibilityID(ctx, sessID, "home-tile-"+clipID)
}

// ReadPlayerID reads the iOS app's persistent player_id off the
// `home-player-id` accessibility node on the Home screen. The app
// generates a UUID on first launch and persists it in UserDefaults,
// then exposes it as a hidden AX node so test code can apply pre-
// playback shape caps to the right player_id and avoid the cold-
// start variant-pick race that wedges rampup step 1. See plan:
// ~/.claude/plans/cold-start-shape-cap-race-fix.md.
//
// Returns an error if no Appium session is active for the device, or
// the AX node isn't present (app build predates the iOS-side change).
func (a *AppiumLauncher) ReadPlayerID(ctx context.Context, sess *Session) (string, error) {
	if sess == nil {
		return "", errors.New("ReadPlayerID: nil session")
	}
	a.mu.Lock()
	sessID := a.sessions[sess.Device.UDID]
	a.mu.Unlock()
	if sessID == "" {
		return "", errors.New("ReadPlayerID: no active appium session for device")
	}
	// XCUITest carries the value under "value"; UiAutomator2 (Android)
	// exposes the Compose node's text under "text".
	attr := "value"
	if sess.Device.Platform == PlatformAndroidTV {
		attr = "text"
	}
	pid, err := a.readAccessibilityValue(ctx, sessID, "home-player-id", attr)
	if err != nil {
		return "", err
	}
	if pid == "" {
		return "", errors.New("ReadPlayerID: home-player-id node was empty")
	}
	return pid, nil
}

// tapByAccessibilityID finds an element by its accessibility identifier
// and clicks it. Returns an error if the element isn't found (the
// caller decides whether that's fatal). Used by Launch to drive the
// app's UI from playback → home → playback for a clean per-test state.
func (a *AppiumLauncher) tapByAccessibilityID(ctx context.Context, sessID, id string) error {
	elementID, err := a.findByAccessibilityID(ctx, sessID, id)
	if err != nil {
		return err
	}
	if err := a.clickElement(ctx, sessID, elementID); err != nil {
		return fmt.Errorf("click element %q: %w", id, err)
	}
	return nil
}

// findByAccessibilityID resolves an accessibility id to a W3C element id.
// Errors when the element is not in the AX tree — which doubles as the
// "screen not showing this element" probe (#660).
func (a *AppiumLauncher) findByAccessibilityID(ctx context.Context, sessID, id string) (string, error) {
	// POST /session/{id}/element — find the element
	findBody := map[string]any{
		"using": "accessibility id",
		"value": id,
	}
	raw, err := a.doRequest(ctx, "POST", "/session/"+sessID+"/element", findBody)
	if err != nil {
		return "", err
	}
	var findResp struct {
		Value map[string]string `json:"value"`
	}
	if err := json.Unmarshal(raw, &findResp); err != nil {
		return "", fmt.Errorf("decode find element %q: %w", id, err)
	}
	// W3C WebDriver returns the element under key "element-6066-11e4-a52e-4f735466cecf".
	var elementID string
	for _, v := range findResp.Value {
		elementID = v
		break
	}
	if elementID == "" {
		return "", fmt.Errorf("find element %q returned no id", id)
	}
	return elementID, nil
}

// clickElement issues the W3C click on a previously-found element.
func (a *AppiumLauncher) clickElement(ctx context.Context, sessID, elementID string) error {
	_, err := a.doRequest(ctx, "POST",
		fmt.Sprintf("/session/%s/element/%s/click", sessID, elementID), map[string]any{})
	return err
}

// sendKeysToElement types text into a previously-found element (W3C
// element/value). Clicks it first to focus the field.
func (a *AppiumLauncher) sendKeysToElement(ctx context.Context, sessID, elementID, text string) error {
	if err := a.clickElement(ctx, sessID, elementID); err != nil {
		return fmt.Errorf("focus field: %w", err)
	}
	_, err := a.doRequest(ctx, "POST",
		fmt.Sprintf("/session/%s/element/%s/value", sessID, elementID),
		map[string]any{"text": text})
	return err
}

// navigateServerPickerIfPresent drives the iOS ServerPickerScreen (#fleet)
// when a freshly-installed/erased sim comes up on it instead of Home: tap
// "Add by URL", type the harness base URL, tap "Add". No-op (returns nil) when
// the picker isn't showing — the normal case where a server is already saved
// (e.g. seeded via SeedServerProfile). Best-effort UI fallback to the
// UserDefaults seed; requires the app to carry the server-* accessibility ids.
func (a *AppiumLauncher) navigateServerPickerIfPresent(ctx context.Context, sessID, baseURL string) error {
	// Short probe — if the picker root isn't in the AX tree we're already past
	// it (on Home), so this is a cheap no-op on the common path.
	if _, err := a.waitForAccessibilityID(ctx, sessID, "server-picker-screen", 4*time.Second); err != nil {
		return nil
	}
	if err := a.tapByAccessibilityID(ctx, sessID, "server-add-by-url"); err != nil {
		return fmt.Errorf("tap add-by-url: %w", err)
	}
	fieldID, err := a.waitForAccessibilityID(ctx, sessID, "server-url-field", 8*time.Second)
	if err != nil {
		return fmt.Errorf("wait url field: %w", err)
	}
	if err := a.sendKeysToElement(ctx, sessID, fieldID, baseURL); err != nil {
		return fmt.Errorf("type server url: %w", err)
	}
	if err := a.tapByAccessibilityID(ctx, sessID, "server-url-add"); err != nil {
		return fmt.Errorf("tap add: %w", err)
	}
	// Let the sheet dismiss and Home render before the caller's back-chevron tap.
	time.Sleep(time.Second)
	return nil
}

// waitForAccessibilityID polls for an element by accessibility id until
// it appears or the deadline passes, then returns its element id. Rides
// out async UI population — e.g. the Home content row renders its
// continue-watching tile only after the catalogue fetch completes, so a
// freshly-launched Home can be empty for a few seconds; tapping into it
// immediately 404s. Poll instead of racing.
func (a *AppiumLauncher) waitForAccessibilityID(ctx context.Context, sessID, id string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		elID, err := a.findByAccessibilityID(ctx, sessID, id)
		if err == nil {
			return elID, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return "", fmt.Errorf("element %q not present after %s: %w", id, timeout, lastErr)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// --- WebDriver protocol plumbing -------------------------------------------

func (a *AppiumLauncher) healthCheck(ctx context.Context) error {
	raw, err := a.doRequest(ctx, "GET", "/status", nil)
	if err != nil {
		return err
	}
	var resp struct {
		Value struct {
			Ready bool `json:"ready"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return err
	}
	if !resp.Value.Ready {
		return errors.New("appium reports not-ready")
	}
	return nil
}

func (a *AppiumLauncher) createSession(ctx context.Context, caps map[string]any) (string, error) {
	body := map[string]any{
		"capabilities": map[string]any{
			"alwaysMatch": caps,
			"firstMatch":  []any{map[string]any{}},
		},
	}
	raw, err := a.doRequest(ctx, "POST", "/session", body)
	if err != nil {
		return "", err
	}
	var resp struct {
		Value struct {
			SessionID string `json:"sessionId"`
		} `json:"value"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", err
	}
	if resp.Value.SessionID == "" {
		return "", fmt.Errorf("appium returned empty sessionId; body: %s", string(raw))
	}
	return resp.Value.SessionID, nil
}

func (a *AppiumLauncher) doRequest(ctx context.Context, method, path string, body any) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.URL+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return raw, fmt.Errorf("appium %s %s: %d %s",
			method, path, resp.StatusCode, truncate(string(raw), 240))
	}
	return raw, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// appiumCapabilities returns the per-platform WebDriver "alwaysMatch"
// capabilities object Appium expects in session creation. noReset=true
// keeps the app's state (so skipHomeOnLaunch + lastPlayed survive across
// sessions); fullReset=false avoids wiping settings between runs.
func appiumCapabilities(d Device, bundleID string) map[string]any {
	caps := map[string]any{
		"appium:noReset":   true,
		"appium:fullReset": false,
		"appium:udid":      d.UDID,
		"appium:bundleId":  bundleID,
		// newCommandTimeout default is 60 s — Appium auto-terminates the
		// session (and the app with it) if no WebDriver command lands
		// inside that window. Our sweeps run 10-30 min without sending
		// any UI commands (Launch is the only Appium call), so we'd hit
		// this every time. 2 h covers the longest realistic sweep with
		// plenty of slack.
		"appium:newCommandTimeout": 7200,
		// forceAppLaunch terminates the app first if it's already
		// running, then launches it fresh. Without this, Appium just
		// attaches to whatever state the app is in. CLI launcher
		// kill+launches every test for the same reason — known-good
		// starting state per run. App data (settings, lastPlayed) is
		// preserved because noReset=true.
		"appium:forceAppLaunch": true,
	}
	if d.Label != "" {
		caps["appium:deviceName"] = d.Label
	}
	switch d.Platform {
	case PlatformIPhone, PlatformIPad:
		caps["platformName"] = "iOS"
		caps["appium:automationName"] = "XCUITest"
		setXCUITestFleetPorts(caps, d.FleetIndex)
		// Real-device WDA bring-up is the slow part: by default Appium runs
		// xcodebuild + deploys WebDriverAgent into a throwaway DerivedData dir
		// every session. Pin a STABLE derivedDataPath so the build persists
		// across runs — xcodebuild goes incremental and the WDA app stays
		// installed, cutting per-session bring-up. Always safe (still builds on
		// first run). Per fleet index so parallel real devices don't share one
		// build dir. (Sims are fast enough not to need this.)
		caps["appium:derivedDataPath"] = iosWDADerivedDataPath(d.FleetIndex)
		// CHAR_IOS_PREBUILT_WDA=1 skips the xcodebuild step entirely and reuses
		// the WDA already built at derivedDataPath — the big speedup. Off by
		// default because Appium ERRORS when usePrebuiltWDA is set but nothing
		// has been built there yet: run once WITHOUT it to populate the path,
		// then flip it on. (See the run-prebuilt-wda guide.)
		if os.Getenv("CHAR_IOS_PREBUILT_WDA") == "1" {
			caps["appium:usePrebuiltWDA"] = true
		}
	case PlatformIPadSim:
		caps["platformName"] = "iOS"
		caps["appium:automationName"] = "XCUITest"
		// Sim launches are faster than real-device; trim the WDA install
		// step Appium does by default — WDA only needs (re)deploy on
		// real devices.
		caps["appium:useNewWDA"] = false
		setXCUITestFleetPorts(caps, d.FleetIndex)
	case PlatformAppleTV:
		caps["platformName"] = "tvOS"
		caps["appium:automationName"] = "XCUITest"
		setXCUITestFleetPorts(caps, d.FleetIndex)
	case PlatformAndroidTV:
		caps["platformName"] = "Android"
		caps["appium:automationName"] = "UiAutomator2"
		caps["appium:appPackage"] = bundleID
		// The real launcher activity (matches the deploy's
		// `am start -n <pkg>/.MainActivity`). An intent CATEGORY is not a
		// valid appActivity — UiAutomator2 fails to start the app with it.
		caps["appium:appActivity"] = ".MainActivity"
		// Don't block session creation on a specific post-launch activity
		// (splash → home transitions vary); any activity in our package is fine.
		caps["appium:appWaitActivity"] = "*"
	}
	return caps
}

// setXCUITestFleetPorts pins this session's WebDriverAgent and MJPEG
// screenshot-stream ports off the device's fleet index. Concurrent
// XCUITest sessions otherwise default to wdaLocalPort 8100 /
// mjpegServerPort 9100 and collide — the 2nd+ sim never binds. Index 0
// → 8100/9100, unchanged for single-device runs.
func setXCUITestFleetPorts(caps map[string]any, fleetIndex int) {
	caps["appium:wdaLocalPort"] = 8100 + fleetIndex
	caps["appium:mjpegServerPort"] = 9100 + fleetIndex
}

// iosWDADerivedDataPath returns a STABLE DerivedData dir for the real-device
// WebDriverAgent build so it persists across runs (incremental builds, and
// prebuilt reuse under CHAR_IOS_PREBUILT_WDA=1) instead of Appium's default
// throwaway temp dir. Base overridable via CHAR_IOS_WDA_DERIVED_DATA; default
// ~/.appium-wda-deriveddata. Suffixed by fleet index so parallel real devices
// each build into their own dir (no concurrent-xcodebuild conflict).
func iosWDADerivedDataPath(fleetIndex int) string {
	base := strings.TrimSpace(os.Getenv("CHAR_IOS_WDA_DERIVED_DATA"))
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".appium-wda-deriveddata")
	}
	return filepath.Join(base, fmt.Sprintf("wda-%d", fleetIndex))
}
