// v2 adapter — bridges the v1 *App in-memory state to the v2 server's
// V1Adapter interface so internal/v2/server can stay free of v1
// implementation details. The interface itself lives in
// go-proxy/internal/v2/server/v1adapter.go; only read-side methods are
// implemented here for Phase B (read-only translator).

package main

import (
	"fmt"
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

// CreateSyntheticPlayer mints a synthetic player record by reusing
// v1's session-allocation helpers (allocateSessionNumber +
// replaceThirdFromLastDigit) and stamping a fully-populated session
// map into the v1 store. No HTTP redirect / no manifest traffic —
// the player is "born allocated" and CI scripts can attach faults /
// shaping / labels before the first request lands.
//
// Idempotency contract (matches OpenAPI):
//
//	201 — newly created
//	200 — playerID already exists with a v2-equivalent body
//	409 — playerID already exists with a different body
//
// Body equivalence currently checks `_v2_labels` only (the only v2
// field the synthetic flow accepts; shape/fault_rules apply via PATCH).
func (a *v2Adapter) CreateSyntheticPlayer(playerID string, payload map[string]any) (int, map[string]any, error) {
	if a == nil || a.app == nil {
		return 0, nil, errSyntheticPlayerNotImplemented
	}
	if playerID == "" {
		playerID = uuid.New().String()
	} else if _, err := uuid.Parse(playerID); err != nil {
		return 0, nil, errInvalidPlayerID
	}

	a.app.sessionsMu.Lock()
	defer a.app.sessionsMu.Unlock()

	current := a.app.getSessionList()
	for _, s := range current {
		if stored, perr := uuid.Parse(getString(s, "player_id")); perr == nil && stored.String() == playerID {
			existingLabels, _ := s["_v2_labels"].(map[string]any)
			incomingLabels, _ := payload["labels"].(map[string]any)
			if labelsEqual(existingLabels, incomingLabels) {
				return 200, map[string]any(cloneSession(s)), nil
			}
			return 409, nil, nil
		}
	}

	if len(current) >= a.app.maxSessions {
		return 0, nil, errSessionLimitReached
	}

	createdAt := nowISO()
	allocated := allocateSessionNumber(current, a.app.maxSessions)
	externalPort := replaceThirdFromLastDigit("30081", allocated)
	internalPort := externalPort
	if mapped, ok := a.app.portMap.MapExternalPort(externalPort); ok {
		internalPort = mapped
	}
	if a.app.traffic != nil {
		if portInt, err := strconv.Atoi(internalPort); err == nil {
			a.app.traffic.ClearPortShaping(portInt)
		}
	}

	sessionData := newSyntheticSessionTemplate(playerID, allocated, internalPort, externalPort, createdAt)
	if labels, ok := payload["labels"].(map[string]any); ok && len(labels) > 0 {
		sessionData["_v2_labels"] = labels
	}

	a.app.resetServerLoopState(fmt.Sprintf("%d", allocated))
	updated := append(current, sessionData)
	a.app.publishSnapshot(cloneSessionList(updated))
	a.app.recordSessionStart(sessionData, "/synthetic/v2")
	return 201, map[string]any(cloneSession(sessionData)), nil
}

// errInvalidPlayerID — sentinel for handler 400 mapping.
var errInvalidPlayerID = invalidPlayerIDError{}

type invalidPlayerIDError struct{}

func (invalidPlayerIDError) Error() string { return "player_id must be a UUID" }

// errSessionLimitReached — sentinel for handler 503 mapping.
var errSessionLimitReached = sessionLimitError{}

type sessionLimitError struct{}

func (sessionLimitError) Error() string { return "session limit reached; clear an existing player first" }

// labelsEqual returns true iff two label maps are byte-equivalent for
// the purposes of POST /players idempotency.
func labelsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// newSyntheticSessionTemplate is the v1 session map used for synthetic
// players. Mirrors the per-field defaults at main.go:4306 (real-player
// auto-registration) but without HTTP-request-derived fields.
func newSyntheticSessionTemplate(playerID string, allocated int, internalPort, externalPort, createdAt string) SessionData {
	id := fmt.Sprintf("%d", allocated)
	return SessionData{
		"session_number":   id,
		"sid":              id,
		"session_id":       id,
		"player_id":        playerID,
		"group_id":         "",
		"control_revision": newControlRevision(),

		"manifest_requests_count":        0,
		"master_manifest_requests_count": 0,
		"segments_count":                 0,
		"all_requests_count":             0,
		"last_request":                   createdAt,
		"first_request_time":             createdAt,
		"session_start_time":             createdAt,
		"origination_time":               createdAt,
		"origination_ip":                 "",
		"is_external_ip":                 false,
		"synthetic":                      true,

		"segment_failure_type":         "none",
		"segment_failure_frequency":    0,
		"segment_consecutive_failures": 0,
		"segment_failure_units":        "requests",
		"segment_consecutive_units":    "requests",
		"segment_frequency_units":      "seconds",
		"segment_failure_mode":         "failures_per_seconds",

		"manifest_failure_type":         "none",
		"manifest_failure_frequency":    0,
		"manifest_failure_units":        "requests",
		"manifest_consecutive_units":    "requests",
		"manifest_frequency_units":      "seconds",
		"manifest_failure_mode":         "failures_per_seconds",
		"manifest_consecutive_failures": 0,

		"master_manifest_failure_type":         "none",
		"master_manifest_failure_frequency":    0,
		"master_manifest_failure_units":        "requests",
		"master_manifest_consecutive_units":    "requests",
		"master_manifest_frequency_units":      "seconds",
		"master_manifest_failure_mode":         "failures_per_seconds",
		"master_manifest_consecutive_failures": 0,

		"all_failure_type":           "none",
		"all_failure_frequency":      0,
		"all_consecutive_failures":   0,
		"all_failure_units":          "requests",
		"all_consecutive_units":      "requests",
		"all_frequency_units":        "seconds",
		"all_failure_mode":           "failures_per_seconds",
		"current_failures":           0,
		"consecutive_failures_count": 0,

		"transport_failure_type":         "none",
		"transport_failure_frequency":    0,
		"transport_consecutive_failures": 1,
		"transport_failure_units":        "seconds",
		"transport_consecutive_units":    "seconds",
		"transport_frequency_units":      "seconds",
		"transport_failure_mode":         "failures_per_seconds",
		"transport_fault_type":           "none",
		"transport_fault_on_seconds":     1,
		"transport_fault_off_seconds":    0,
		"transport_consecutive_seconds":  1,
		"transport_frequency_seconds":    0,
		"transport_fault_active":         false,

		"x_forwarded_port":          internalPort,
		"x_forwarded_port_external": externalPort,
		"loop_count_server":         0,
	}
}

// ApplyShapeToPlayer drives the kernel-side rate/delay/loss state for
// the player's bound port via v1's existing applySessionShaping helper.
func (a *v2Adapter) ApplyShapeToPlayer(playerID string) error {
	if a == nil || a.app == nil {
		return nil
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return err
	}
	for _, s := range a.app.getSessionList() {
		stored, perr := uuid.Parse(getString(s, "player_id"))
		if perr != nil || stored != want {
			continue
		}
		portStr := getString(s, "x_forwarded_port")
		if portStr == "" {
			return nil
		}
		port, perr := strconv.Atoi(portStr)
		if perr != nil {
			return nil
		}
		a.app.applySessionShaping(s, port)
		return nil
	}
	return nil
}

// ApplyPatternToPlayer drives v1's pattern step-engine on the player's
// bound port via applyShapePattern. Empty steps disarm the loop.
func (a *v2Adapter) ApplyPatternToPlayer(playerID string, steps []server.ShapePatternStep, delayMs int, lossPct float64) error {
	if a == nil || a.app == nil {
		return nil
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return err
	}
	for _, s := range a.app.getSessionList() {
		stored, perr := uuid.Parse(getString(s, "player_id"))
		if perr != nil || stored != want {
			continue
		}
		portStr := getString(s, "x_forwarded_port")
		if portStr == "" {
			return nil
		}
		port, perr := strconv.Atoi(portStr)
		if perr != nil {
			return nil
		}
		v1Steps := make([]NftShapeStep, 0, len(steps))
		for _, st := range steps {
			v1Steps = append(v1Steps, NftShapeStep{
				DurationSeconds: st.DurationSeconds,
				RateMbps:        st.RateMbps,
				Enabled:         st.Enabled,
			})
		}
		return a.app.applyShapePattern(port, v1Steps, delayMs, lossPct)
	}
	return nil
}

// ApplyTransportFaultToPlayer arms (or disarms when faultType="none")
// the transport-fault loop on the player's port.
func (a *v2Adapter) ApplyTransportFaultToPlayer(playerID, faultType string, consecutive int, consecutiveUnits string, frequency int) error {
	if a == nil || a.app == nil {
		return nil
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return err
	}
	for _, s := range a.app.getSessionList() {
		stored, perr := uuid.Parse(getString(s, "player_id"))
		if perr != nil || stored != want {
			continue
		}
		portStr := getString(s, "x_forwarded_port")
		if portStr == "" {
			return nil
		}
		port, perr := strconv.Atoi(portStr)
		if perr != nil {
			return nil
		}
		if consecutive < 1 {
			consecutive = 1
		}
		a.app.armTransportFaultLoop(port, faultType, consecutive, consecutiveUnits, frequency)
		return nil
	}
	return nil
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

// ----- Group surface (Phase F) ---------------------------------------------

// GroupMembers scans the snapshot for sessions tagged with groupID
// and returns the bound player_ids. Order is the snapshot order.
func (a *v2Adapter) GroupMembers(groupID string) []string {
	if a == nil || a.app == nil || groupID == "" {
		return nil
	}
	var out []string
	for _, s := range a.app.getSessionList() {
		if getString(s, "group_id") == groupID {
			if pid := getString(s, "player_id"); pid != "" {
				out = append(out, pid)
			}
		}
	}
	return out
}

// LinkGroup tags each player_id's session with groupID. Players not
// currently connected are silently skipped — v2 callers can list
// /api/v2/players first and reject up-front if they care about that.
func (a *v2Adapter) LinkGroup(groupID string, playerIDs []string) []string {
	if a == nil || a.app == nil || groupID == "" || len(playerIDs) == 0 {
		return nil
	}
	wanted := map[string]struct{}{}
	for _, p := range playerIDs {
		if p != "" {
			wanted[p] = struct{}{}
		}
	}
	current := a.app.getSessionList()
	updated := cloneSessionList(current)
	var linked []string
	for i, s := range updated {
		pid := getString(s, "player_id")
		if _, ok := wanted[pid]; !ok {
			continue
		}
		updated[i]["group_id"] = groupID
		updated[i]["control_revision"] = newControlRevision()
		linked = append(linked, pid)
	}
	a.app.saveSessionList(updated)
	return linked
}

// UnlinkGroup clears group_id on every session currently tagged with
// the supplied group_id.
func (a *v2Adapter) UnlinkGroup(groupID string) []string {
	if a == nil || a.app == nil || groupID == "" {
		return nil
	}
	current := a.app.getSessionList()
	updated := cloneSessionList(current)
	var cleared []string
	for i, s := range updated {
		if getString(s, "group_id") != groupID {
			continue
		}
		updated[i]["group_id"] = ""
		updated[i]["control_revision"] = newControlRevision()
		if pid := getString(s, "player_id"); pid != "" {
			cleared = append(cleared, pid)
		}
	}
	if len(cleared) == 0 {
		return nil
	}
	a.app.saveSessionList(updated)
	return cleared
}

// RemoveFromGroup clears one player's group_id tag. Returns true if
// the player existed AND had a non-empty group_id.
func (a *v2Adapter) RemoveFromGroup(playerID string) bool {
	if a == nil || a.app == nil || playerID == "" {
		return false
	}
	want, err := uuid.Parse(playerID)
	if err != nil {
		return false
	}
	current := a.app.getSessionList()
	updated := cloneSessionList(current)
	for i, s := range updated {
		stored, perr := uuid.Parse(getString(s, "player_id"))
		if perr != nil || stored != want {
			continue
		}
		if getString(s, "group_id") == "" {
			return false
		}
		updated[i]["group_id"] = ""
		updated[i]["control_revision"] = newControlRevision()
		a.app.saveSessionList(updated)
		return true
	}
	return false
}

// BroadcastPatch applies fn to every group member except
// `excludePlayerID` and stamps each with the supplied `rev`. The
// caller's MutatePlayer for the originating player_id has already
// run; this completes the fan-out under one sessionsMu acquire.
func (a *v2Adapter) BroadcastPatch(groupID string, excludePlayerID string, rev string, fn func(map[string]any) error) ([]string, error) {
	if a == nil || a.app == nil || groupID == "" {
		return nil, nil
	}
	exclude, _ := uuid.Parse(excludePlayerID)
	a.app.sessionsMu.Lock()
	defer a.app.sessionsMu.Unlock()

	current := a.app.getSessionList()
	updated := make([]SessionData, len(current))
	copy(updated, current)
	var touched []string
	for i, s := range current {
		if getString(s, "group_id") != groupID {
			continue
		}
		stored, perr := uuid.Parse(getString(s, "player_id"))
		if perr == nil && stored == exclude {
			continue
		}
		mutable := cloneSession(s)
		if err := fn(mutable); err != nil {
			return touched, err
		}
		mutable["control_revision"] = rev
		updated[i] = mutable
		if pid := getString(mutable, "player_id"); pid != "" {
			touched = append(touched, pid)
		}
	}
	if len(touched) == 0 {
		return nil, nil
	}
	a.app.publishSnapshot(cloneSessionList(updated))
	return touched, nil
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
