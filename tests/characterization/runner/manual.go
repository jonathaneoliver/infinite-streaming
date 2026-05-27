package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// ManualLauncher discovers heartbeating players via the harness and
// delegates launch/kill to the operator. Useful as a no-setup fallback
// when CLI or Appium tooling isn't installed, and as the launcher for
// platforms the CLI layer hasn't covered yet.
type ManualLauncher struct {
	// In is the prompt source. Defaults to os.Stdin.
	In io.Reader
	// Out is where prompts print. Defaults to os.Stderr so they survive
	// stdout redirection (e.g. when piping report JSON).
	Out io.Writer
	// HeartbeatTimeout is how long Launch waits for the operator's player
	// to show up. Defaults to 90s.
	HeartbeatTimeout time.Duration
}

// NewManualLauncher returns a ManualLauncher with stdin/stderr defaults.
func NewManualLauncher() *ManualLauncher {
	return &ManualLauncher{
		In:               os.Stdin,
		Out:              os.Stderr,
		HeartbeatTimeout: 90 * time.Second,
	}
}

func (m *ManualLauncher) Mode() LaunchMode { return LaunchManual }

// Discover returns every heartbeating player the proxy currently sees. The
// manual launcher can't do better than this — it has no platform-side
// knowledge of devices that aren't running the app yet.
func (m *ManualLauncher) Discover(ctx context.Context) ([]Device, error) {
	players, err := ListPlayers(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]Device, 0, len(players))
	for _, p := range players {
		if !p.IsHeartbeating(now) {
			continue
		}
		out = append(out, deviceFromPlayer(p))
	}
	return out, nil
}

// Launch prompts the operator to start the app, then waits for a fresh
// heartbeat. Caller specifies the target Device — if its UDID matches an
// already-heartbeating player, we accept immediately without prompting.
func (m *ManualLauncher) Launch(ctx context.Context, d Device) (*Session, error) {
	out := m.writer()
	// Fast path: device already heartbeating? Bind and return.
	if p, ok := m.findHeartbeating(ctx, d); ok {
		fmt.Fprintf(out, "manual launcher: %s already heartbeating as player %s\n",
			d, shortID(p.ID))
		return &Session{Device: d, PlayerID: p.ID, Launcher: m}, nil
	}

	fmt.Fprintf(out, "\nmanual launcher: please start the %s app on %s now\n", d.Platform, d.Label)
	fmt.Fprintf(out, "  - skipHomeOnLaunch + lastPlayed should auto-resume the previous play\n")
	fmt.Fprintf(out, "  - waiting up to %s for a heartbeat...\n", m.HeartbeatTimeout)

	deadline := time.Now().Add(m.HeartbeatTimeout)
	for {
		if p, ok := m.findHeartbeating(ctx, d); ok {
			fmt.Fprintf(out, "  ✓ found player %s\n", shortID(p.ID))
			return &Session{Device: d, PlayerID: p.ID, Launcher: m}, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("manual launcher: no heartbeat for %s within %s",
				d, m.HeartbeatTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Kill prompts the operator to force-quit the app. We can't enforce it, so
// we wait for the operator to confirm with <enter>.
func (m *ManualLauncher) Kill(ctx context.Context, d Device) error {
	out := m.writer()
	fmt.Fprintf(out, "\nmanual launcher: please force-quit the %s app on %s, then press <enter>: ",
		d.Platform, d.Label)
	r := bufio.NewReader(m.reader())
	_, err := r.ReadString('\n')
	return err
}

func (m *ManualLauncher) Close() error { return nil }

func (m *ManualLauncher) reader() io.Reader {
	if m.In != nil {
		return m.In
	}
	return os.Stdin
}

func (m *ManualLauncher) writer() io.Writer {
	if m.Out != nil {
		return m.Out
	}
	return os.Stderr
}

// findHeartbeating walks the players list and picks the one matching d.
// Match preference (most specific first): explicit player ID prefix on
// d.UDID; user-agent contains d.Platform; sole heartbeater of any kind.
func (m *ManualLauncher) findHeartbeating(ctx context.Context, d Device) (PlayerRecord, bool) {
	players, err := ListPlayers(ctx)
	if err != nil {
		return PlayerRecord{}, false
	}
	now := time.Now()
	live := make([]PlayerRecord, 0, len(players))
	for _, p := range players {
		if p.IsHeartbeating(now) {
			live = append(live, p)
		}
	}
	// 1) UDID prefix exact-match against player id
	if d.UDID != "" {
		for _, p := range live {
			if strings.HasPrefix(p.ID, strings.ToLower(d.UDID)) {
				return p, true
			}
		}
	}
	// 2) Platform substring in UA
	if d.Platform != "" {
		needle := platformUAHint(d.Platform)
		if needle != "" {
			for _, p := range live {
				if strings.Contains(strings.ToLower(p.UserAgent), needle) {
					return p, true
				}
			}
		}
	}
	// 3) Sole heartbeater is unambiguous
	if len(live) == 1 {
		return live[0], true
	}
	return PlayerRecord{}, false
}

func platformUAHint(p Platform) string {
	switch p {
	case PlatformIPhone:
		return "iphone"
	case PlatformIPad, PlatformIPadSim:
		return "ipad"
	case PlatformAppleTV:
		return "appletv"
	case PlatformAndroidTV:
		return "android"
	case PlatformWeb:
		return "mozilla"
	}
	return ""
}

func deviceFromPlayer(p PlayerRecord) Device {
	platform := classifyUA(p.UserAgent)
	label := p.Labels["device"]
	if label == "" {
		label = shortID(p.ID)
	}
	return Device{
		Platform: platform,
		UDID:     p.ID,
		Label:    label,
	}
}

// classifyUA mirrors the dashboard's UA classifier (composables/useSessionLabels.ts).
// Keep in sync with that file; the regexes diverging silently caused #469.
func classifyUA(ua string) Platform {
	ualc := strings.ToLower(ua)
	switch {
	case strings.Contains(ualc, "apple tv") || strings.Contains(ualc, "tvos"):
		return PlatformAppleTV
	case strings.Contains(ualc, "iphone"):
		return PlatformIPhone
	case strings.Contains(ualc, "ipad"):
		return PlatformIPad
	case strings.Contains(ualc, "android"):
		return PlatformAndroidTV
	case strings.Contains(ualc, "mozilla") || strings.Contains(ualc, "chrome"):
		return PlatformWeb
	}
	return ""
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}
