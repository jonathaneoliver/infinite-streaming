// v2 adapter — bridges the v1 *App in-memory state to the v2 server's
// V1Adapter interface so internal/v2/server can stay free of v1
// implementation details. The interface itself lives in
// go-proxy/internal/v2/server/v1adapter.go; only read-side methods are
// implemented here for Phase B (read-only translator).

package main

import (
	"os"

	"github.com/google/uuid"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/server"
)

// v2Adapter is the App-side bridge satisfying server.V1Adapter.
type v2Adapter struct {
	app *App
}

// NewV2Adapter returns an adapter ready to feed v2 read handlers.
func NewV2Adapter(app *App) server.V1Adapter {
	return &v2Adapter{app: app}
}

// SessionList exposes the v1 in-memory session snapshot as a slice of
// type-erased maps. Each entry is a clone — safe for v2 to read without
// holding any locks. Returns an empty slice when the app is not yet
// initialised (defensive — should not happen at request time).
func (a *v2Adapter) SessionList() []map[string]any {
	if a == nil || a.app == nil {
		return nil
	}
	src := a.app.getSessionList()
	out := make([]map[string]any, 0, len(src))
	for _, s := range src {
		out = append(out, map[string]any(s))
	}
	return out
}

// SessionByPlayerID returns the (cloned) session record for one player,
// or (nil, false) if no session matches. Comparison is UUID-equality
// (not string-equality) so non-canonical-case stored player_ids still
// match — Roku and a few historical clients have written uppercase.
func (a *v2Adapter) SessionByPlayerID(playerID string) (map[string]any, bool) {
	if playerID == "" || a == nil || a.app == nil {
		return nil, false
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return nil, false
	}
	for _, s := range a.app.getSessionList() {
		stored, err := uuid.Parse(getString(s, "player_id"))
		if err == nil && stored == want {
			return map[string]any(s), true
		}
	}
	return nil, false
}

// NetworkLogForPlayer reads up to `limit` ring-buffer entries for the
// player's bound session. Returns nil when the session has no captured
// requests (e.g. immediately after self-registration).
func (a *v2Adapter) NetworkLogForPlayer(playerID string, limit int) []map[string]any {
	if playerID == "" || a == nil || a.app == nil {
		return nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return nil
	}
	var sessionID string
	for _, s := range a.app.getSessionList() {
		stored, err := uuid.Parse(getString(s, "player_id"))
		if err != nil || stored != want {
			continue
		}
		sessionID = getString(s, "session_id")
		break
	}
	if sessionID == "" {
		return nil
	}
	a.app.networkLogsMu.RLock()
	rb, ok := a.app.networkLogs[sessionID]
	a.app.networkLogsMu.RUnlock()
	if !ok || rb == nil {
		return nil
	}
	all := rb.GetAll()
	// GetAll returns oldest-first. Spec wants newest-first up to limit.
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	out := make([]map[string]any, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, networkEntryToMap(all[i]))
	}
	return out
}

// Version, ContentDir, AuthEnabled, AnalyticsEnabled are simple shims
// over the v1 build-time / env-time configuration.
func (a *v2Adapter) Version() string { return versionString }

func (a *v2Adapter) ContentDir() string {
	if v := os.Getenv("CONTENT_DIR"); v != "" {
		return v
	}
	return "/boss/dynamic_content"
}

func (a *v2Adapter) AuthEnabled() bool {
	v := os.Getenv("INFINITE_STREAM_AUTH_HTPASSWD")
	if v == "" {
		return false
	}
	if _, err := os.Stat(v); err != nil {
		return false
	}
	return true
}

func (a *v2Adapter) AnalyticsEnabled() bool {
	// Analytics is enabled when the forwarder URL is configured; the
	// nginx config drops the /analytics/* routes when the env is empty.
	return os.Getenv("FORWARDER_URL") != ""
}

// networkEntryToMap converts a typed v1 ring-buffer entry into the
// loosely-typed map shape v2's translator consumes. Mirrors the keys
// in NetworkLogEntry.
func networkEntryToMap(e NetworkLogEntry) map[string]any {
	return map[string]any{
		"timestamp":    e.Timestamp,
		"method":       e.Method,
		"url":          e.URL,
		"upstream_url": e.UpstreamURL,
		"path":         e.Path,
		"request_kind": e.RequestKind,
		"status":       e.Status,
		"bytes_in":     e.BytesIn,
		"bytes_out":    e.BytesOut,
		"content_type": e.ContentType,
		"play_id":      e.PlayID,
	}
}
