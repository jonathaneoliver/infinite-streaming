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

	// ----- Mutation surface (Phase D) ----------------------------------

	// MutatePlayer atomically applies fn to the player's session under
	// the v1 store's write lock. fn may modify the supplied map freely;
	// any returned error aborts the write. Returns the post-mutation
	// session (a clone, safe to read), found=true if the player exists.
	MutatePlayer(playerID string, fn func(map[string]any) error) (post map[string]any, found bool, err error)

	// CreateSyntheticPlayer provisions a synthetic player record. If
	// `playerID` is empty the adapter allocates a new UUIDv4. The
	// returned status is one of:
	//
	//   201 - newly created
	//   200 - upsert hit: a player with that id already exists with a
	//         body byte-equivalent to `payload`
	//   409 - player exists with a different body; client should PATCH
	//
	// The returned record is the (cloned) session map after creation
	// or look-up. On 409 the record is nil.
	CreateSyntheticPlayer(playerID string, payload map[string]any) (status int, record map[string]any, err error)

	// DeletePlayer drops the named player. Returns true if the player
	// existed prior to this call.
	DeletePlayer(playerID string) bool

	// ClearAllPlayers tears down every active player and live state.
	// Mirrors v1's /api/clear-sessions.
	ClearAllPlayers()

	// ----- SSE source surface (Phase E) --------------------------------

	// SubscribeSessions registers a v2 listener on v1's session
	// broadcast hub. Returns a buffered channel of session-list
	// snapshots and a cancel func that *must* be called to release
	// the slot. Snapshots are owned by the caller — do not mutate.
	SubscribeSessions(buffer int) (<-chan SessionSnapshot, func())

	// SubscribeNetwork registers a v2 listener on v1's per-request
	// network event hub. Returns a buffered channel of network-log
	// rows and a cancel func.
	SubscribeNetwork(buffer int) (<-chan NetworkLogRow, func())
}

// SessionSnapshot is the v2-friendly shape of a SessionsEvent: the
// list of session records (each a `map[string]any` clone) plus the
// publisher's revision counter and any drop count carried since the
// listener last drained the channel.
type SessionSnapshot struct {
	Sessions []map[string]any
	Revision uint64
	Dropped  uint64
}

// NetworkLogRow is the v2-friendly shape of a NetworkEvent: the
// session_id (v1 routing key) plus the captured request as a map.
type NetworkLogRow struct {
	SessionID string
	Entry     map[string]any
}
