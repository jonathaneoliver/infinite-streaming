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
		BundleIDs: map[Platform]string{
			PlatformIPhone:    "com.jeoliver.InfiniteStreamPlayer",
			PlatformIPad:      "com.jeoliver.InfiniteStreamPlayer",
			PlatformIPadSim:   "com.jeoliver.InfiniteStreamPlayer",
			PlatformAppleTV:   "com.jeoliver.InfiniteStreamPlayerTV",
			PlatformAndroidTV: "com.infinitestream.player",
		},
		sessions: map[string]string{},
		hc:       &http.Client{Timeout: 60 * time.Second},
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
func (a *AppiumLauncher) LaunchToHome(ctx context.Context, d Device) (*Session, error) {
	bundleID := a.BundleIDs[d.Platform]
	if bundleID == "" {
		return nil, fmt.Errorf("appium launcher: no bundle id for platform %s", d.Platform)
	}
	if err := a.healthCheck(ctx); err != nil {
		return nil, fmt.Errorf("appium server not reachable at %s: %w (start with `appium`, or unset LAUNCH_MODE=appium)", a.URL, err)
	}
	caps := appiumCapabilities(d, bundleID)
	sessID, err := a.createSession(ctx, caps)
	if err != nil {
		return nil, fmt.Errorf("appium create session: %w", err)
	}
	a.mu.Lock()
	a.sessions[d.UDID] = sessID
	a.mu.Unlock()

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
	case PlatformIPhone, PlatformIPad, PlatformIPadSim:
		if err := a.tapByAccessibilityID(ctx, sessID, "home-continue-watching"); err != nil {
			return fmt.Errorf("tap home-continue-watching: %w", err)
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
func (a *AppiumLauncher) readAccessibilityValue(ctx context.Context, sessID, id string) (string, error) {
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
	raw, err = a.doRequest(ctx, "GET",
		fmt.Sprintf("/session/%s/element/%s/attribute/value", sessID, elementID), nil)
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
	pid, err := a.readAccessibilityValue(ctx, sessID, "home-player-id")
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
	// POST /session/{id}/element — find the element
	findBody := map[string]any{
		"using": "accessibility id",
		"value": id,
	}
	raw, err := a.doRequest(ctx, "POST", "/session/"+sessID+"/element", findBody)
	if err != nil {
		return err
	}
	var findResp struct {
		Value map[string]string `json:"value"`
	}
	if err := json.Unmarshal(raw, &findResp); err != nil {
		return fmt.Errorf("decode find element %q: %w", id, err)
	}
	// W3C WebDriver returns the element under key "element-6066-11e4-a52e-4f735466cecf".
	var elementID string
	for _, v := range findResp.Value {
		elementID = v
		break
	}
	if elementID == "" {
		return fmt.Errorf("find element %q returned no id", id)
	}
	// POST /session/{id}/element/{el}/click
	_, err = a.doRequest(ctx, "POST",
		fmt.Sprintf("/session/%s/element/%s/click", sessID, elementID), map[string]any{})
	if err != nil {
		return fmt.Errorf("click element %q: %w", id, err)
	}
	return nil
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
	case PlatformIPadSim:
		caps["platformName"] = "iOS"
		caps["appium:automationName"] = "XCUITest"
		// Sim launches are faster than real-device; trim the WDA install
		// step Appium does by default — WDA only needs (re)deploy on
		// real devices.
		caps["appium:useNewWDA"] = false
	case PlatformAppleTV:
		caps["platformName"] = "tvOS"
		caps["appium:automationName"] = "XCUITest"
	case PlatformAndroidTV:
		caps["platformName"] = "Android"
		caps["appium:automationName"] = "UiAutomator2"
		caps["appium:appPackage"] = bundleID
		// Android's session needs an activity too; LAUNCHER is the
		// portable choice that matches our CLI launcher's `monkey -c LAUNCHER`.
		caps["appium:appActivity"] = "android.intent.category.LAUNCHER"
	}
	return caps
}
