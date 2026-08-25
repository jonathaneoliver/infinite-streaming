package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultBundleIDs is the canonical Platform → app bundle id map. Both
// launchers (and the sim-seeding helper) read from it so the ids never drift.
var DefaultBundleIDs = map[Platform]string{
	PlatformIPhone:    "com.jeoliver.InfiniteStreamPlayer",
	PlatformIPad:      "com.jeoliver.InfiniteStreamPlayer",
	PlatformIPadSim:   "com.jeoliver.InfiniteStreamPlayer",
	PlatformAppleTV:   "com.jeoliver.InfiniteStreamPlayerTV",
	PlatformAndroidTV: "com.infinitestream.player",
}

// DefaultBundleID returns the default app bundle id for a platform ("" if
// unknown).
func DefaultBundleID(p Platform) string { return DefaultBundleIDs[p] }

// cloneBundleIDs returns a fresh copy of DefaultBundleIDs so each launcher can
// mutate its own map without affecting the shared default.
func cloneBundleIDs() map[Platform]string {
	m := make(map[Platform]string, len(DefaultBundleIDs))
	for k, v := range DefaultBundleIDs {
		m[k] = v
	}
	return m
}

// serverProfile mirrors the iOS app's ServerProfile Codable shape
// (ServerProfile.swift). The app persists an array of these as JSON-encoded
// Data under UserDefaults key "is.servers.v2", plus the active server's id
// (UUID string) under "is.servers.active".
type serverProfile struct {
	ContentURL  string `json:"contentURL"`
	ID          string `json:"id"`
	Label       string `json:"label"`
	PlaybackURL string `json:"playbackURL"`
}

// SeedServerProfile writes a server profile into a simulator's app
// UserDefaults so a freshly-installed (or erased) sim skips the blocking
// ServerPickerScreen and connects straight to baseURL. The app reads the
// server list via UserDefaults.data(forKey:) + JSONDecoder, so the value MUST
// be stored as a plist <data> blob — writing it as a string (or via
// `simctl spawn defaults write`, which lands in the wrong domain) is silently
// ignored and the picker still shows. We therefore merge the two keys
// directly into the app data-container's Preferences plist.
//
// baseURL is the dashboard/content URL (e.g. https://host:21000); the playback
// URL is derived as contentPort+81, matching ServerProfile.fromDashboardURL.
// best-effort: returns an error the caller can log without failing the run.
func SeedServerProfile(ctx context.Context, udid, bundleID, baseURL string) error {
	if _, err := exec.LookPath("xcrun"); err != nil {
		return fmt.Errorf("seed server: %w", err)
	}
	content, playback, err := contentAndPlaybackURLs(baseURL)
	if err != nil {
		return fmt.Errorf("seed server: %w", err)
	}

	// The app stores the active server by id; reuse a deterministic id so
	// re-seeding the same sim is idempotent (no duplicate profiles pile up).
	const sid = "00000000-0000-4000-8000-00000000feed"
	prof := serverProfile{ContentURL: content, ID: sid, Label: "test-dev", PlaybackURL: playback}
	js, err := json.Marshal([]serverProfile{prof})
	if err != nil {
		return fmt.Errorf("seed server: marshal: %w", err)
	}

	container, err := simAppDataContainer(ctx, udid, bundleID)
	if err != nil {
		return fmt.Errorf("seed server: %w", err)
	}
	plistPath := filepath.Join(container, "Library", "Preferences", bundleID+".plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("seed server: mkdir prefs: %w", err)
	}

	// Write the JSON to a temp file and Import it as a <data> value (the only
	// clean way to set a Data type from PlistBuddy). Set/Add the active id as a
	// string. PlistBuddy ships with macOS — no python/plist dependency.
	tmp, err := os.CreateTemp("", "is-servers-*.json")
	if err != nil {
		return fmt.Errorf("seed server: tempfile: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(js); err != nil {
		tmp.Close()
		return fmt.Errorf("seed server: write tempfile: %w", err)
	}
	tmp.Close()

	const pb = "/usr/libexec/PlistBuddy"
	// Delete any prior value so Import doesn't append/conflict; ignore the
	// "does not exist" error on a fresh plist.
	_ = exec.CommandContext(ctx, pb, "-c", "Delete :is.servers.v2", plistPath).Run()
	steps := [][]string{
		{"-c", "Import :is.servers.v2 " + tmp.Name(), plistPath},
		// Set if present, else Add — PlistBuddy has no upsert.
		{"-c", "Set :is.servers.active " + sid, plistPath},
	}
	for _, args := range steps {
		if out, err := exec.CommandContext(ctx, pb, args...).CombinedOutput(); err != nil {
			// Set fails when the key is absent — fall back to Add for the
			// active id. (Import always succeeds after the Delete above.)
			if strings.Contains(args[1], "Set :is.servers.active") {
				if out2, aerr := exec.CommandContext(ctx, pb,
					"-c", "Add :is.servers.active string "+sid, plistPath).CombinedOutput(); aerr != nil {
					return fmt.Errorf("seed server: add active id: %w: %s", aerr, strings.TrimSpace(string(out2)))
				}
				continue
			}
			return fmt.Errorf("seed server: PlistBuddy %v: %w: %s", args, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// simAppDataContainer returns the on-disk data container for an installed app
// on a simulator (where its UserDefaults Preferences plist lives).
func simAppDataContainer(ctx context.Context, udid, bundleID string) (string, error) {
	out, err := exec.CommandContext(ctx, "xcrun", "simctl",
		"get_app_container", udid, bundleID, "data").Output()
	if err != nil {
		return "", fmt.Errorf("get_app_container %s %s: %w (is the app installed?)", udid, bundleID, err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("get_app_container %s %s: empty path", udid, bundleID)
	}
	return path, nil
}

// contentAndPlaybackURLs normalises a dashboard base URL into the app's
// (contentURL, playbackURL) pair: scheme://host:port and
// scheme://host:(port+81), mirroring ServerProfile.fromDashboardURL.
func contentAndPlaybackURLs(baseURL string) (content, playback string, err error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	scheme := "https"
	rest := base
	if i := strings.Index(base, "://"); i >= 0 {
		scheme = base[:i]
		rest = base[i+3:]
	}
	host, port := splitHostPort(rest)
	if host == "" {
		return "", "", fmt.Errorf("no host in base URL %q", baseURL)
	}
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	pn, perr := strconv.Atoi(port)
	if perr != nil {
		return "", "", fmt.Errorf("bad port %q in base URL %q", port, baseURL)
	}
	content = fmt.Sprintf("%s://%s:%d", scheme, host, pn)
	playback = fmt.Sprintf("%s://%s:%d", scheme, host, pn+81)
	return content, playback, nil
}

// HarnessBaseURL is the resolved dashboard/content base URL the harness talks
// to (HARNESS_BASE_URL or the compiled default). Exposed so fleet seeding can
// point sims at the same server the test uses.
func HarnessBaseURL() string { return bootstrapBaseURL() }
