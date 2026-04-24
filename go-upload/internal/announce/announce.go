// Package announce heartbeats the server's public-facing URL to the
// pairing rendezvous Worker so the standalone pair page can list it.
//
// Opt-in: only runs when INFINITE_STREAM_ANNOUNCE_URL is set to the URL
// the server should advertise (e.g. http://lenovo.local:30000). Two extra
// inputs:
//   - INFINITE_STREAM_RENDEZVOUS_URL — the Worker base URL (must be set).
//   - INFINITE_STREAM_ANNOUNCE_LABEL — optional human-readable label.
//
// A stable per-install server_id is stored at <data_dir>/server_id so the
// same server keeps the same identity across restarts (otherwise the
// listing would show duplicates as old entries TTL out).
//
// Cadence: announces fire on boot, every defaultInterval (12h), and on
// demand when Trigger() is called (e.g. when the user opens the dashboard
// "Server Info" modal — covers a missed boot announce).
package announce

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// 12h heartbeat pairs with the Worker's 24h TTL. Each server costs
	// only ~2 KV writes/day, so hundreds of servers fit under Cloudflare
	// KV's free-plan 1,000-writes/day account-wide budget. Manual Trigger()
	// (called from the dashboard's Server Info modal) covers the case
	// where the boot announce was lost.
	defaultInterval = 12 * time.Hour
	requestTimeout  = 10 * time.Second
)

// Manager owns the announce loop and lets external callers (e.g. an HTTP
// handler) request an immediate re-announce via Trigger(). Construct with
// New() once at startup, then call Run() in a goroutine.
type Manager struct {
	dataDir   string
	triggerCh chan struct{}
	startOnce sync.Once
}

// New returns a Manager. dataDir is where the persistent server_id file
// lives (typically the same dir as the SQLite DB).
func New(dataDir string) *Manager {
	return &Manager{
		dataDir: dataDir,
		// Buffered so Trigger() never blocks the caller; if a trigger is
		// already pending it's coalesced.
		triggerCh: make(chan struct{}, 1),
	}
}

// Trigger asks the running announce loop to send an extra announce as
// soon as it can. Safe to call from any goroutine; non-blocking. If an
// announce is already pending, additional calls are coalesced.
func (m *Manager) Trigger() {
	if m == nil {
		return
	}
	select {
	case m.triggerCh <- struct{}{}:
	default:
		// already pending — coalesce.
	}
}

// Run blocks (typically as a goroutine) sending announces until ctx is
// cancelled. Logs and skips if the required env is missing.
func (m *Manager) Run(ctx context.Context) {
	announceURL := strings.TrimSpace(os.Getenv("INFINITE_STREAM_ANNOUNCE_URL"))
	if announceURL == "" {
		log.Printf("announce: disabled (set INFINITE_STREAM_ANNOUNCE_URL to enable)")
		return
	}
	rendezvous := strings.TrimSpace(os.Getenv("INFINITE_STREAM_RENDEZVOUS_URL"))
	if rendezvous == "" {
		log.Printf("announce: skipped — INFINITE_STREAM_RENDEZVOUS_URL is empty")
		return
	}
	rendezvous = strings.TrimRight(rendezvous, "/")

	serverID, err := loadOrCreateServerID(m.dataDir)
	if err != nil {
		log.Printf("announce: cannot persist server_id (%v) — generating ephemeral", err)
		serverID = randomID()
	}

	label := strings.TrimSpace(os.Getenv("INFINITE_STREAM_ANNOUNCE_LABEL"))
	if label == "" {
		// Fall back to host:port from the announce URL so dashboards still
		// show something meaningful (container hostnames are random IDs).
		if u, err := url.Parse(announceURL); err == nil && u.Host != "" {
			label = u.Host
		}
	}
	if label == "" {
		if h, _ := os.Hostname(); h != "" {
			label = h
		}
	}

	endpoint := rendezvous + "/announce"
	client := &http.Client{Timeout: requestTimeout}

	log.Printf("announce: heartbeating %s as %q to %s every %s (boot + on-demand)", announceURL, label, endpoint, defaultInterval)

	// First beat right away so the server appears immediately.
	postOnce(ctx, client, endpoint, serverID, announceURL, label)

	t := time.NewTicker(defaultInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			postOnce(ctx, client, endpoint, serverID, announceURL, label)
		case <-m.triggerCh:
			log.Printf("announce: on-demand trigger fired")
			postOnce(ctx, client, endpoint, serverID, announceURL, label)
		}
	}
}

func postOnce(ctx context.Context, client *http.Client, endpoint, serverID, url, label string) {
	body, _ := json.Marshal(map[string]string{
		"server_id": serverID,
		"url":       url,
		"label":     label,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		log.Printf("announce: build request failed: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		// Network blips happen — log at debug-ish level and try again next tick.
		log.Printf("announce: heartbeat failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("announce: heartbeat HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
}

func loadOrCreateServerID(dataDir string) (string, error) {
	if dataDir == "" {
		return "", fmt.Errorf("dataDir empty")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, "server_id")
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if isValidID(id) {
			return id, nil
		}
	}
	id := randomID()
	if err := os.WriteFile(path, []byte(id+"\n"), 0o644); err != nil {
		return id, err
	}
	return id, nil
}

func randomID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isValidID(s string) bool {
	if len(s) < 4 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}
