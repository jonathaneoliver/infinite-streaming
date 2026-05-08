// v2 adapter — bridges the v1 *App in-memory state to the v2 server's
// V1Adapter interface so internal/v2/server can stay free of v1
// implementation details. The interface itself lives in
// go-proxy/internal/v2/server/v1adapter.go; only read-side methods are
// implemented here for Phase B (read-only translator).

package main

import (
	"os"
	"strconv"
	"sync"

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

// ----- Mutation surface (Phase D) ------------------------------------------

// MutatePlayer locates the session matching playerID, hands its session
// map to fn, and persists the result. Runs under sessionsMu so concurrent
// PATCHes serialise.
//
// fn may modify the map freely; returning an error from fn aborts the
// mutation cleanly (no v1 side-effects). A successful fn is followed by
// publishSnapshot — the same path v1's own write handlers use, so the
// existing SSE hub / network log / clients see the change identically.
func (a *v2Adapter) MutatePlayer(playerID string, fn func(map[string]any) error) (map[string]any, bool, error) {
	if playerID == "" || a == nil || a.app == nil {
		return nil, false, nil
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return nil, false, nil
	}
	a.app.sessionsMu.Lock()
	defer a.app.sessionsMu.Unlock()

	current := a.app.getSessionList()
	idx := -1
	for i, s := range current {
		stored, err := uuid.Parse(getString(s, "player_id"))
		if err == nil && stored == want {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, false, nil
	}
	mutable := cloneSession(current[idx])
	if err := fn(mutable); err != nil {
		return nil, true, err
	}
	updated := make([]SessionData, len(current))
	copy(updated, current)
	updated[idx] = mutable
	a.app.publishSnapshot(cloneSessionList(updated))
	return map[string]any(cloneSession(mutable)), true, nil
}

// DeletePlayer removes the named player from the v1 store and frees any
// shaping/fault loops bound to its dedicated port.
//
// Mirrors v1's `handleClearSessions` pattern: the session list is
// captured without holding sessionsMu, helpers (`disablePatternForPort`,
// `armTransportFaultLoop`) drive their own locking via saveSessionByID,
// and the final session-list write goes through saveSessionList (which
// acquires sessionsMu internally). Crucially, sessionsMu is *not* held
// across the helper calls — sync.Mutex is non-reentrant in Go, and the
// helpers ultimately re-enter through saveSessionList.
func (a *v2Adapter) DeletePlayer(playerID string) bool {
	if playerID == "" || a == nil || a.app == nil {
		return false
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return false
	}

	current := a.app.getSessionList()
	idx := -1
	for i, s := range current {
		stored, err := uuid.Parse(getString(s, "player_id"))
		if err == nil && stored == want {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false
	}
	target := current[idx]
	a.app.removeServerLoopState(getString(target, "session_id"))
	a.app.recordSessionEnd(target, "v2_delete")
	if portStr := getString(target, "x_forwarded_port"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			a.app.disablePatternForPort(port)
			a.app.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
		}
	}
	updated := make([]SessionData, 0, len(current)-1)
	updated = append(updated, current[:idx]...)
	updated = append(updated, current[idx+1:]...)
	a.app.saveSessionList(updated)
	return true
}

// ClearAllPlayers tears every player and live state down — same path
// as v1's /api/clear-sessions.
//
// Mirrors v1: snapshot under no lock, drive helpers without holding
// sessionsMu (they re-enter via saveSessionByID/saveSessionList), then
// write the empty list through saveSessionList which takes the lock
// briefly at the end.
func (a *v2Adapter) ClearAllPlayers() {
	if a == nil || a.app == nil {
		return
	}
	current := a.app.getSessionList()
	portSet := map[int]struct{}{}
	a.app.shapeMu.Lock()
	for port := range a.app.shapeLoops {
		portSet[port] = struct{}{}
	}
	a.app.shapeMu.Unlock()
	for _, sess := range current {
		a.app.removeServerLoopState(getString(sess, "session_id"))
		a.app.recordSessionEnd(sess, "cleared_v2")
		if portStr := getString(sess, "x_forwarded_port"); portStr != "" {
			if port, err := strconv.Atoi(portStr); err == nil {
				portSet[port] = struct{}{}
			}
		}
	}
	for port := range portSet {
		a.app.disablePatternForPort(port)
		a.app.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
	}
	a.app.saveSessionList([]SessionData{})
}

// CreateSyntheticPlayer is not yet wired. Synthetic players need port
// allocation + nftables setup without traffic — a Phase F deliverable.
// For now the adapter signals "not implemented" via status 0 and an
// explicit error so the v2 server returns 501 with a clear detail.
func (a *v2Adapter) CreateSyntheticPlayer(playerID string, payload map[string]any) (int, map[string]any, error) {
	return 0, nil, errSyntheticPlayerNotImplemented
}

// ----- SSE source surface (Phase E) ----------------------------------------

// SubscribeSessions wraps v1's SessionEventHub: each broadcast lands on
// our channel as a server.SessionSnapshot (sessions cloned to
// `[]map[string]any`, revision + dropped counters preserved).
func (a *v2Adapter) SubscribeSessions(buffer int) (<-chan server.SessionSnapshot, func()) {
	if a == nil || a.app == nil || a.app.sessionsHub == nil {
		ch := make(chan server.SessionSnapshot)
		close(ch)
		return ch, func() {}
	}
	if buffer <= 0 {
		buffer = 16
	}
	clientID, src := a.app.sessionsHub.AddClient("")
	out := make(chan server.SessionSnapshot, buffer)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case ev, ok := <-src:
				if !ok {
					return
				}
				snap := server.SessionSnapshot{
					Revision: ev.Revision,
					Dropped:  ev.Dropped,
					Sessions: make([]map[string]any, 0, len(ev.Sessions)),
				}
				for _, s := range ev.Sessions {
					snap.Sessions = append(snap.Sessions, map[string]any(cloneSession(s)))
				}
				select {
				case out <- snap:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			a.app.sessionsHub.RemoveClient(clientID)
			close(done)
		})
	}
	return out, cancel
}

// SubscribeNetwork wraps v1's NetworkEventHub: each per-request event
// lands as a server.NetworkLogRow (session_id + the entry as a map).
func (a *v2Adapter) SubscribeNetwork(buffer int) (<-chan server.NetworkLogRow, func()) {
	if a == nil || a.app == nil || a.app.networkHub == nil {
		ch := make(chan server.NetworkLogRow)
		close(ch)
		return ch, func() {}
	}
	if buffer <= 0 {
		buffer = 256
	}
	clientID, src := a.app.networkHub.AddClient(buffer)
	out := make(chan server.NetworkLogRow, buffer)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case ev, ok := <-src:
				if !ok {
					return
				}
				row := server.NetworkLogRow{
					SessionID: ev.SessionID,
					Entry:     networkEntryToMap(ev.Entry),
				}
				select {
				case out <- row:
				case <-done:
					return
				}
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			a.app.networkHub.RemoveClient(clientID)
			close(done)
		})
	}
	return out, cancel
}

// errSyntheticPlayerNotImplemented is an exported sentinel so the v2
// server can detect the "not yet wired" path and respond with 501 +
// problem detail rather than a generic 500.
var errSyntheticPlayerNotImplemented = syntheticNotImplementedError{}

type syntheticNotImplementedError struct{}

func (syntheticNotImplementedError) Error() string {
	return "synthetic player creation requires port allocation + nftables setup — not yet wired"
}
