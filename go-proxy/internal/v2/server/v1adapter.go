package server

// V1Adapter exposes the subset of v1 in-memory state that the v2 read
// handlers consult. The implementation lives in package main on *App;
// this interface keeps v2/server free of v1-internal types (NetworkLogEntry,
// NetworkLogRingBuffer, App, etc.) — the v2 package depends only on
// `map[string]any` for v1 records plus a couple of standard libs.
//
// Methods that hand out []map[string]any return SessionData-shaped slices.
// v2 only reads these maps — never mutates — so the loose typing is
// deliberate. The keys consulted are documented inline in translate.go.
type V1Adapter interface {
	// SessionList returns a snapshot of every active session.
	SessionList() []map[string]any

	// SessionByPlayerID returns the session record for a given player_id,
	// or (nil, false) if no such player is connected.
	SessionByPlayerID(playerID string) (map[string]any, bool)

	// NetworkLogForPlayer returns up to `limit` entries from the player's
	// network ring buffer, newest first. Returns nil if the player has no
	// captured requests yet.
	NetworkLogForPlayer(playerID string, limit int) []map[string]any

	// Version is the build commit string baked into the binary.
	Version() string

	// ContentDir is the configured content root (e.g. /boss/dynamic_content).
	ContentDir() string

	// AuthEnabled reports whether HTTP basic auth is configured.
	AuthEnabled() bool

	// AnalyticsEnabled reports whether the forwarder sidecar is reachable.
	AnalyticsEnabled() bool
}
