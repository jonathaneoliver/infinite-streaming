// v2 adapter — bridges the v1 *App in-memory state to the v2 server's
// V1Adapter interface so internal/v2/server can stay free of v1
// implementation details. The interface itself lives in
// go-proxy/internal/v2/server/v1adapter.go; only read-side methods are
// implemented here for Phase B (read-only translator).

package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"sync"

	"github.com/google/uuid"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/server"
	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/v2translate"
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
//
// Sessions are passed through normalizeSessionsForResponse so the
// per-tick stamped fields (client_rtt_*, client_path_ping_rtt_ms,
// drained byte counters, etc.) are present on the raw_session payload
// — same as the v1 endpoints serve. Without this the RTT and ping
// charts have nothing to plot.
func (a *v2Adapter) SessionList() []map[string]any {
	if a == nil || a.app == nil {
		return nil
	}
	src := a.app.getSessionList()
	normalized := a.app.normalizeSessionsForResponse(src)
	out := make([]map[string]any, 0, len(normalized))
	for _, s := range normalized {
		out = append(out, map[string]any(s))
	}
	return out
}

// matchesPlayerID compares a stored v1 player_id against an incoming
// lookup token. Three resolution paths in priority order:
//
//  1. Direct string equality (v1 short-form passed through as-is).
//  2. UUID equality (v2 canonical UUID against a UUID-shaped stored).
//  3. Derived-UUID match — incoming canonical UUID matches the
//     v5(playerUUIDNamespace, stored_short_form) derivation. This is
//     how v2 reads (which returned the derived UUID) round-trip back
//     to the original v1 session on subsequent PATCH/DELETE.
func matchesPlayerID(stored, incoming string) bool {
	if stored == "" || incoming == "" {
		return false
	}
	if stored == incoming {
		return true
	}
	want, err := uuid.Parse(incoming)
	if err != nil {
		return false
	}
	if su, err := uuid.Parse(stored); err == nil && su == want {
		return true
	}
	return v2translate.PlayerUUIDForRawID(stored) == want
}

// SessionByPlayerID returns the (cloned) session record for one player,
// or (nil, false) if no session matches. Honors v1 short-form
// player_ids via matchesPlayerID.
//
// Sessions are passed through normalizeSessionsForResponse so the
// per-tick stamped fields (client_rtt_*, client_path_ping_rtt_ms,
// drained byte counters, mbps_shaper_*, etc.) are present — same as
// SessionList(). Without this, augmentPlayerFrameWithRaw on the SSE
// path would overwrite the v2-events raw_session (which my
// SubscribeSessions normalized) with un-normalized data, hiding RTT
// and shaper fields from the dashboard.
func (a *v2Adapter) SessionByPlayerID(playerID string) (map[string]any, bool) {
	if playerID == "" || a == nil || a.app == nil {
		return nil, false
	}
	src := a.app.getSessionList()
	normalized := a.app.normalizeSessionsForResponse(src)
	for _, s := range normalized {
		if matchesPlayerID(getString(s, "player_id"), playerID) {
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
	// Match v1 /api/session/{id}/network: default to the entire ring
	// buffer (5000 entries) so the dashboard waterfall sees the same
	// time window. Caller can still pass a smaller `?limit=` to narrow.
	if limit <= 0 || limit > 5000 {
		limit = 5000
	}
	var sessionID string
	for _, s := range a.app.sessionsView() { // #740 read-only: matches player, reads port/fields
		if !matchesPlayerID(getString(s, "player_id"), playerID) {
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
	// GetAll returns oldest-first. Match v1 /api/session/{id}/network
	// which also returns oldest-first — the dashboard waterfall sorts
	// by timestamp internally either way, but matching the wire shape
	// removes a class of "field looked normal but rendered wrong"
	// debugging surprises.
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	out := make([]map[string]any, 0, len(all))
	for _, e := range all {
		out = append(out, networkEntryToMap(e))
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

func (a *v2Adapter) DefaultRateMbps() int {
	if a == nil || a.app == nil {
		return 0
	}
	return a.app.defaultRateMbps
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
		// Phase timings — surfaced when httptrace populated them.
		"dns_ms":         e.DNSMs,
		"connect_ms":     e.ConnectMs,
		"tls_ms":         e.TLSMs,
		"ttfb_ms":        e.TTFBMs,
		"transfer_ms":    e.TransferMs,
		"total_ms":       e.TotalMs,
		"client_wait_ms": e.ClientWaitMs,
		// Fault metadata — flagged on rows where the proxy injected one.
		"faulted":        e.Faulted,
		"fault_type":     e.FaultType,
		"fault_action":   e.FaultAction,
		"fault_category": e.FaultCategory,
	}
}

// ----- Mutation surface (Phase D) ------------------------------------------

// MutatePlayer locates the session matching playerID, hands its session
// map to fn, and persists the result via mutateSessions (lock-free CAS,
// #740 — replaces the old sessionsMu + publishSnapshot path so concurrent
// PATCHes can't clobber each other).
//
// fn may modify the map freely; returning an error from fn aborts the
// mutation cleanly (no v1 side-effects, nothing published). fn must be
// re-runnable — the CAS retries it on a fresh clone if a concurrent writer
// committed in between.
func (a *v2Adapter) MutatePlayer(playerID string, fn func(map[string]any) error) (map[string]any, bool, error) {
	if playerID == "" || a == nil || a.app == nil {
		return nil, false, nil
	}
	var (
		found     bool
		fnErr     error
		before    SessionData
		result    SessionData
		sessionID string
	)
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		found = false
		fnErr = nil
		before = nil
		result = nil
		idx := -1
		for i, s := range sessions {
			if matchesPlayerID(getString(s, "player_id"), playerID) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return sessions, false
		}
		found = true
		before = cloneSession(sessions[idx])
		mutable := cloneSession(sessions[idx])
		if err := fn(mutable); err != nil {
			fnErr = err
			return sessions, false
		}
		sessions[idx] = mutable
		result = mutable
		sessionID = getString(mutable, "session_id")
		return sessions, true
	})
	if fnErr != nil {
		return nil, true, fnErr
	}
	if !found {
		return nil, false, nil
	}
	// Emit control_events for operator-driven changes. The v2 PATCH
	// path does NOT route through applySessionSettingsUpdate, so
	// without this hook every dashboard slider / fault edit / label
	// change went unrecorded. Issue #474 follow-up. Hoisted out of the
	// CAS closure — it runs once on the committed result.
	a.app.emitControlEventsForDiff(sessionID, before, result)
	return map[string]any(cloneSession(result)), true, nil
}

// DeletePlayer removes the named player from the v1 store and frees any
// shaping/fault loops bound to its dedicated port.
//
// #740: the list removal is a mutateSessions CAS; the removed session is
// captured inside the closure and its teardown (loop-state, recordSessionEnd,
// pattern/transport-fault disarm) runs once on the committed result — the
// same hoist-side-effects-out-of-the-closure rule the v1 DELETE handler uses.
func (a *v2Adapter) DeletePlayer(playerID string) bool {
	if playerID == "" || a == nil || a.app == nil {
		return false
	}
	var target SessionData
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		target = nil
		idx := -1
		for i, s := range sessions {
			if matchesPlayerID(getString(s, "player_id"), playerID) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return sessions, false
		}
		target = sessions[idx]
		updated := make([]SessionData, 0, len(sessions)-1)
		updated = append(updated, sessions[:idx]...)
		updated = append(updated, sessions[idx+1:]...)
		return updated, true
	})
	if target == nil {
		return false
	}
	a.app.removeServerLoopState(getString(target, "session_id"))
	a.app.recordSessionEnd(target, "v2_delete")
	if portStr := getString(target, "x_forwarded_port"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			a.app.disablePatternForPort(port)
			a.app.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)
		}
	}
	return true
}

// ClearAllPlayers tears every player and live state down — same path
// as v1's /api/clear-sessions.
//
// #740: the list is emptied via a mutateSessions CAS; the cleared sessions
// are captured inside the closure and their teardown runs once on the
// committed result (hoist-side-effects-out-of-the-closure rule).
func (a *v2Adapter) ClearAllPlayers() {
	if a == nil || a.app == nil {
		return
	}
	var removed []SessionData
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		removed = append(removed[:0], sessions...)
		return []SessionData{}, true
	})
	portSet := map[int]struct{}{}
	a.app.shapeMu.Lock()
	for port := range a.app.shapeLoops {
		portSet[port] = struct{}{}
	}
	a.app.shapeMu.Unlock()
	for _, sess := range removed {
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

	// #740: the existing-check + allocate + append run inside ONE mutateSessions
	// CAS closure so two concurrent CreateSyntheticPlayer calls can't claim the
	// same slot (the v2 analogue of the bootstrap reserve race). The closure
	// is re-runnable; kernel sweep (ClearPortShaping), loop-state reset, and
	// recordSessionStart are hoisted out and run once on the committed result.
	createdAt := nowISO()
	const (
		codeConflict = -1
		codeLimit    = -2
	)
	var (
		code         int
		existingBody map[string]any
		newSession   SessionData
		internalPort string
	)
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		code = 0
		existingBody = nil
		newSession = nil
		internalPort = ""
		for _, s := range sessions {
			if stored, perr := uuid.Parse(getString(s, "player_id")); perr == nil && stored.String() == playerID {
				existingLabels, _ := s["_v2_labels"].(map[string]any)
				incomingLabels, _ := payload["labels"].(map[string]any)
				if labelsEqual(existingLabels, incomingLabels) {
					code = 200
					existingBody = map[string]any(cloneSession(s))
					return sessions, false
				}
				code = codeConflict
				return sessions, false
			}
		}
		if len(sessions) >= a.app.maxSessions {
			code = codeLimit
			return sessions, false
		}
		allocated := allocateSessionNumber(sessions, a.app.maxSessions)
		externalPort := replaceThirdFromLastDigit("30081", allocated)
		ip := externalPort
		if mapped, ok := a.app.portMap.MapExternalPort(externalPort); ok {
			ip = mapped
		}
		internalPort = ip
		sd := newSyntheticSessionTemplate(playerID, allocated, ip, externalPort, createdAt)
		if labels, ok := payload["labels"].(map[string]any); ok && len(labels) > 0 {
			sd["_v2_labels"] = labels
		}
		newSession = sd
		code = 201
		// Publish a clone so newSession stays a private object the hoisted
		// recordSessionStart can use without mutating the live snapshot —
		// the pre-#740 publishSnapshot(cloneSessionList(...)) did the same.
		return append(sessions, cloneSession(sd)), true
	})
	switch code {
	case 200:
		return 200, existingBody, nil
	case codeConflict:
		return 409, nil, nil
	case codeLimit:
		return 0, nil, errSessionLimitReached
	}
	if a.app.traffic != nil {
		if portInt, err := strconv.Atoi(internalPort); err == nil {
			a.app.traffic.ClearPortShaping(portInt)
		}
	}
	a.app.resetServerLoopState(getString(newSession, "session_id"))
	a.app.recordSessionStart(newSession, "/synthetic/v2")
	return 201, map[string]any(cloneSession(newSession)), nil
}

// errInvalidPlayerID — sentinel for handler 400 mapping.
var errInvalidPlayerID = invalidPlayerIDError{}

type invalidPlayerIDError struct{}

func (invalidPlayerIDError) Error() string { return "player_id must be a UUID" }

// errSessionLimitReached — sentinel for handler 503 mapping.
var errSessionLimitReached = sessionLimitError{}

type sessionLimitError struct{}

func (sessionLimitError) Error() string {
	return "session limit reached; clear an existing player first"
}

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
	for _, s := range a.app.sessionsView() { // #740 read-only: matches player, reads port/fields
		if !matchesPlayerID(getString(s, "player_id"), playerID) {
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
	for _, s := range a.app.sessionsView() { // #740 read-only: matches player, reads port/fields
		if !matchesPlayerID(getString(s, "player_id"), playerID) {
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
	for _, s := range a.app.sessionsView() { // #740 read-only: matches player, reads port/fields
		if !matchesPlayerID(getString(s, "player_id"), playerID) {
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
				// Treat the broadcast event as a trigger only — its
				// `ev.Sessions` are POST-normalize, so `_rttWindow` /
				// `_pingRTTUs` pointers were already stripped and we
				// can't drain a fresh sample from them. Pull a fresh
				// clone of the live snapshot (which retains the window
				// pointers) and normalize THAT — so the v2 SSE consumer
				// sees the same client_rtt_*/_path_ping_*/mbps_shaper_*
				// fields the polled `/api/sessions` endpoint shows.
				freshClone := a.app.getSessionList()
				normalized := a.app.normalizeSessionsForResponse(freshClone)
				if len(normalized) > 0 {
					rtt, _ := normalized[0]["client_rtt_ms"].(float64)
					_, hasWin := normalized[0]["_rttWindow"]
					_, hasConn := normalized[0]["_lastTCPConn"]
					_, hasPing := normalized[0]["_pingRTTUs"]
					log.Printf("V2_SSE_DBG rtt=%.2f keys=%d hasWin=%v hasConn=%v hasPing=%v", rtt, len(normalized[0]), hasWin, hasConn, hasPing)
				}
				snap := server.SessionSnapshot{
					Revision: ev.Revision,
					Dropped:  ev.Dropped,
					Sessions: make([]map[string]any, 0, len(normalized)),
				}
				for _, s := range normalized {
					snap.Sessions = append(snap.Sessions, map[string]any(s))
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
	for _, s := range a.app.sessionsView() { // #740 read-only: collects group members' player_ids
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
	// Match each wanted player_id via matchesPlayerID rather than a
	// plain string-equality map. Devices report their player_id in
	// whatever case they like (iPad reports the UUID uppercase), but
	// oapigen canonicalises incoming UUIDs to lowercase, so a raw
	// `wanted[pid]` lookup misses every time and link returns 0 → 409
	// "no eligible members". matchesPlayerID handles UUID equality
	// (case-insensitive) AND the v1 short-form → v5(playerUUIDNamespace)
	// derivation that every other v2 mutation path uses.
	wantedRaw := make([]string, 0, len(playerIDs))
	for _, p := range playerIDs {
		if p != "" {
			wantedRaw = append(wantedRaw, p)
		}
	}
	var linked []string
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		linked = linked[:0]
		for i, s := range sessions {
			pid := getString(s, "player_id")
			matched := false
			for _, want := range wantedRaw {
				if matchesPlayerID(pid, want) {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
			sessions[i]["group_id"] = groupID
			sessions[i]["control_revision"] = newControlRevision()
			linked = append(linked, pid)
		}
		return sessions, len(linked) > 0
	})
	return linked
}

// UnlinkGroup clears group_id on every session currently tagged with
// the supplied group_id.
func (a *v2Adapter) UnlinkGroup(groupID string) []string {
	if a == nil || a.app == nil || groupID == "" {
		return nil
	}
	var cleared []string
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		cleared = cleared[:0]
		for i, s := range sessions {
			if getString(s, "group_id") != groupID {
				continue
			}
			sessions[i]["group_id"] = ""
			sessions[i]["control_revision"] = newControlRevision()
			if pid := getString(s, "player_id"); pid != "" {
				cleared = append(cleared, pid)
			}
		}
		return sessions, len(cleared) > 0
	})
	if len(cleared) == 0 {
		return nil
	}
	return cleared
}

// RemoveFromGroup clears one player's group_id tag. Returns true if
// the player existed AND had a non-empty group_id.
func (a *v2Adapter) RemoveFromGroup(playerID string) bool {
	if a == nil || a.app == nil || playerID == "" {
		return false
	}
	var removed bool
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		removed = false
		for i, s := range sessions {
			if !matchesPlayerID(getString(s, "player_id"), playerID) {
				continue
			}
			if getString(s, "group_id") == "" {
				return sessions, false
			}
			sessions[i]["group_id"] = ""
			sessions[i]["control_revision"] = newControlRevision()
			removed = true
			return sessions, true
		}
		return sessions, false
	})
	return removed
}

// BroadcastPatch applies fn to every group member except
// `excludePlayerID` and stamps each with the supplied `rev`. The
// caller's MutatePlayer for the originating player_id has already run;
// this completes the fan-out in one mutateSessions CAS (#740 — replaces
// the sessionsMu + publishSnapshot path). fn must be re-runnable; a fn
// error aborts the whole fan-out with nothing published.
func (a *v2Adapter) BroadcastPatch(groupID string, excludePlayerID string, rev string, fn func(map[string]any) error) ([]string, error) {
	if a == nil || a.app == nil || groupID == "" {
		return nil, nil
	}
	var (
		touched []string
		fnErr   error
	)
	a.app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
		touched = touched[:0]
		fnErr = nil
		for i, s := range sessions {
			if getString(s, "group_id") != groupID {
				continue
			}
			if matchesPlayerID(getString(s, "player_id"), excludePlayerID) {
				continue
			}
			mutable := cloneSession(s)
			if err := fn(mutable); err != nil {
				fnErr = err
				return sessions, false
			}
			mutable["control_revision"] = rev
			sessions[i] = mutable
			if pid := getString(mutable, "player_id"); pid != "" {
				touched = append(touched, pid)
			}
		}
		return sessions, len(touched) > 0
	})
	if fnErr != nil {
		return touched, fnErr
	}
	if len(touched) == 0 {
		return nil, nil
	}
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
