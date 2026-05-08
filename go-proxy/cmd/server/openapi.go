// OpenAPI annotations for go-proxy's HTTP surface.
//
// All swag (https://github.com/swaggo/swag) annotations live in this file
// instead of inline on each handler — keeps main.go free of comment churn
// and gives the spec a single review surface. The stub functions below have
// no runtime role; swag scans the comment blocks above them.
//
// Regenerate via `make openapi` at the repo root. The generated spec lands
// in api/openapi/proxy/.

//	@title       go-proxy harness API
//	@version     1.0
//	@description Per-session HTTP fault injection, transport faults via nftables,
//	@description and live streams of session + network events. Sits behind
//	@description nginx; clients reach it via http://<host>:21000/api/...
//	@servers.url             /
//	@servers.description     Same origin as wherever this spec is served

package main

//	@Summary  List active sessions
//	@Tags     sessions
//	@Produce  json
//	@Success  200 {array}  object  "Each item is a normalized session record (~80 fields). Subset returned to the dashboard; harness-cli reads the full record from /api/session/{id}."
//	@Router   /api/sessions [get]
func docsListSessions() {}

//	@Summary  Get one session
//	@Tags     sessions
//	@Param    id   path      string true "session_id"
//	@Produce  json
//	@Success  200  {object}  object "Full session record including fault counters, shaping state, player metrics, manifest_variants."
//	@Failure  404  {object}  map[string]string
//	@Router   /api/session/{id} [get]
func docsGetSession() {}

//	@Summary  Delete one session
//	@Description Removes the session from the in-memory map and frees its dedicated proxy port.
//	@Tags     sessions
//	@Param    id   path  string true "session_id"
//	@Success  204  "No Content"
//	@Router   /api/session/{id} [delete]
func docsDeleteSession() {}

//	@Summary  Patch session settings
//	@Description Optimistic-concurrency PATCH used for fault config + shaping flags.
//	@Description Body envelope: `{set: {field: value, ...}, fields: [field, ...], base_revision: "<iso8601>"}`.
//	@Description Returns 409 with the current control_revision when base_revision is stale.
//	@Tags     sessions
//	@Param    id        path    string                 true  "session_id"
//	@Param    envelope  body    PatchSessionRequest    true  "Set+fields+base_revision envelope"
//	@Produce  json
//	@Success  200       {object} PatchSessionResponse
//	@Failure  409       {object} ConflictResponse      "control_revision conflict"
//	@Router   /api/session/{id} [patch]
func docsPatchSession() {}

//	@Summary  Wipe every session
//	@Description Destructive: clears all sessions, fault config, and nftables shaping. Confirm before calling.
//	@Tags     sessions
//	@Success  200
//	@Router   /api/clear-sessions [post]
func docsClearSessions() {}

//	@Summary  Stream session-state diffs (SSE)
//	@Description Long-lived `text/event-stream` connection. Server emits one `data:` frame per session-record change (debounced ~250ms). Optional `player_id` query param filters to one player.
//	@Description
//	@Description Each `data:` frame is a JSON-encoded `SessionStreamFrame` (see schemas).
//	@Description Heartbeat: comment line `: ping` every ~15s. Reconnect on disconnect; the proxy does not replay missed frames.
//	@Tags     streams
//	@Param    player_id query string false "filter frames to one player_id"
//	@Produce  text/event-stream
//	@Success  200 {object} SessionStreamFrame "JSON payload of one SSE data frame"
//	@Router   /api/sessions/stream [get]
func docsSessionStream() {}

//	@Summary  Stream per-request network events (SSE)
//	@Description Long-lived `text/event-stream` connection. Server emits one `data:` frame per HTTP request as it lands in any session's ring buffer.
//	@Description
//	@Description Each `data:` frame is a JSON-encoded `NetworkStreamFrame` (see schemas), shape `{session_id, entry: NetworkLogEntry}`.
//	@Description Heartbeat: comment line `: ping` every 15s. Reconnect on disconnect; nothing is replayed. Wrapped by `harness-cli tail`.
//	@Tags     streams
//	@Produce  text/event-stream
//	@Success  200 {object} NetworkStreamFrame "JSON payload of one SSE data frame"
//	@Router   /api/network/stream [get]
func docsNetworkStream() {}

//	@Summary  Get one session's network ring buffer
//	@Description One-shot snapshot of the per-session in-memory request log (HAR-shaped). Bounded; older entries fall off as new ones arrive.
//	@Tags     network
//	@Param    id   path      string  true  "session_id"
//	@Produce  json
//	@Success  200  {array}   NetworkLogEntry
//	@Router   /api/session/{id}/network [get]
func docsGetNetworkLog() {}

//	@Summary  Apply transport shaping to a port
//	@Description nftables/tc rate + delay + loss on the proxy's egress for one session port. Setting any axis disables an active pattern on the same port.
//	@Tags     shaping
//	@Param    port  path  string         true  "session-bound port (e.g. 30281)"
//	@Param    body  body  ShapeRequest   true  "rate / delay / loss"
//	@Produce  json
//	@Success  200   {object} object
//	@Router   /api/nftables/shape/{port} [post]
func docsNftShape() {}

//	@Summary  Install a step pattern on a port
//	@Description Time-boxed sequence of rate steps (ramp_up, square_wave, etc). steps=[] clears.
//	@Tags     shaping
//	@Param    port  path  string         true  "session-bound port"
//	@Param    body  body  PatternRequest true  "steps + template_mode"
//	@Produce  json
//	@Success  200   {object} object
//	@Router   /api/nftables/pattern/{port} [post]
func docsNftPattern() {}

//	@Summary  Set port bandwidth alone
//	@Tags     shaping
//	@Param    port  path  string  true  "session-bound port"
//	@Param    body  body  BandwidthRequest true "{rate_mbps}"
//	@Success  200
//	@Router   /api/nftables/bandwidth/{port} [post]
func docsNftBandwidth() {}

//	@Summary  Set port packet loss alone
//	@Tags     shaping
//	@Param    port  path  string  true  "session-bound port"
//	@Param    body  body  LossRequest true "{loss_pct}"
//	@Success  200
//	@Router   /api/nftables/loss/{port} [post]
func docsNftLoss() {}

//	@Summary  Inspect current shaping for a port
//	@Tags     shaping
//	@Param    port path string true "session-bound port"
//	@Produce  json
//	@Success  200  {object} object
//	@Router   /api/nftables/port/{port} [get]
func docsNftPort() {}

//	@Summary  Global nftables status
//	@Tags     shaping
//	@Produce  json
//	@Success  200 {object} object
//	@Router   /api/nftables/status [get]
func docsNftStatus() {}

//	@Summary  Kernel shaping primitives available
//	@Description Reports whether netem, htb, and per-port rules are operational.
//	@Tags     shaping
//	@Produce  json
//	@Success  200 {object} object
//	@Router   /api/nftables/capabilities [get]
func docsNftCapabilities() {}

//	@Summary  Link sessions into a fault-propagation group
//	@Tags     groups
//	@Param    body  body  LinkSessionsRequest true "{session_ids: [...]}"
//	@Success  200
//	@Router   /api/session-group/link [post]
func docsLinkSessions() {}

//	@Summary  Unlink a session from its group
//	@Tags     groups
//	@Param    body body UnlinkSessionRequest true "{session_id}"
//	@Success  200
//	@Router   /api/session-group/unlink [post]
func docsUnlinkSession() {}

//	@Summary  Get a session group
//	@Tags     groups
//	@Param    groupId path string true "group id"
//	@Produce  json
//	@Success  200 {object} object
//	@Router   /api/session-group/{groupId} [get]
func docsGetGroup() {}

//	@Summary  External IPs the harness is reachable from
//	@Tags     diagnostics
//	@Produce  json
//	@Success  200 {object} object
//	@Router   /api/external-ips [get]
func docsExternalIPs() {}

//	@Summary  Proxy version
//	@Tags     diagnostics
//	@Produce  json
//	@Success  200 {object} object
//	@Router   /api/version [get]
func docsVersion() {}

//	@Summary  Player-side metrics ingestion
//	@Description Players post their internal metrics here. Not for ops use.
//	@Tags     internal
//	@Param    id path string true "session_id"
//	@Param    body body object true "player metrics"
//	@Success  200
//	@Router   /api/session/{id}/metrics [post]
func docsPostMetrics() {}

//	@Summary  Legacy fault-settings POST (use PATCH instead)
//	@Description Predates the PATCH envelope. Kept for backwards compat with older dashboard code.
//	@Tags     internal
//	@Deprecated
//	@Param    id   path  string true "session_id"
//	@Param    body body  object true "fault settings"
//	@Success  200
//	@Router   /api/failure-settings/{id} [post]
func docsFailureSettings() {}

//	@Summary  Legacy session-update POST (use PATCH instead)
//	@Tags     internal
//	@Deprecated
//	@Param    id path string true "session_id"
//	@Param    body body object true "settings"
//	@Success  200
//	@Router   /api/session/{id}/update [post]
func docsSessionUpdate() {}

// Request/response types referenced from the @Param / @Success blocks above.
// These are *documentation* types — real handlers may use map[string]any
// internally; we model the canonical shape here so codegen has something
// concrete to consume.

// PatchSessionRequest is the optimistic-concurrency envelope.
type PatchSessionRequest struct {
	Set          map[string]any `json:"set"`
	Fields       []string       `json:"fields"`
	BaseRevision string         `json:"base_revision"`
}

// PatchSessionResponse wraps the updated session record + new control_revision.
type PatchSessionResponse struct {
	Session         map[string]any `json:"session"`
	ControlRevision string         `json:"control_revision"`
}

// ConflictResponse is returned with HTTP 409 when base_revision is stale.
type ConflictResponse struct {
	Error           string         `json:"error"`
	Session         map[string]any `json:"session"`
	ControlRevision string         `json:"control_revision"`
}

// ShapeRequest is the body of POST /api/nftables/shape/{port}.
type ShapeRequest struct {
	RateMbps float64 `json:"rate_mbps"`
	DelayMs  float64 `json:"delay_ms"`
	LossPct  float64 `json:"loss_pct"`
}

// PatternStep is one step in a shaping pattern.
type PatternStep struct {
	RateMbps        float64 `json:"rate_mbps"`
	DurationSeconds int     `json:"duration_seconds"`
	Enabled         bool    `json:"enabled"`
}

// PatternRequest installs or clears a shaping pattern. steps=[] clears.
type PatternRequest struct {
	Steps        []PatternStep `json:"steps"`
	TemplateMode string        `json:"template_mode"`
	DelayMs      float64       `json:"delay_ms"`
	LossPct      float64       `json:"loss_pct"`
}

// BandwidthRequest sets only the rate cap.
type BandwidthRequest struct {
	RateMbps float64 `json:"rate_mbps"`
}

// LossRequest sets only the packet-loss percentage.
type LossRequest struct {
	LossPct float64 `json:"loss_pct"`
}

// LinkSessionsRequest groups sessions for fault propagation.
type LinkSessionsRequest struct {
	SessionIDs []string `json:"session_ids"`
}

// UnlinkSessionRequest removes one session from its current group.
type UnlinkSessionRequest struct {
	SessionID string `json:"session_id"`
}

// NetworkLogEntry is defined in main.go; swag picks up the real type
// (including its rich field-level comments) and surfaces it in the spec.

// NetworkStreamFrame is one SSE `data:` frame from /api/network/stream.
// One frame per HTTP request as it lands in any session's ring buffer;
// `harness-cli tail` consumes this exact shape.
type NetworkStreamFrame struct {
	SessionID string          `json:"session_id"`
	Entry     NetworkLogEntry `json:"entry"`
}

// SessionStreamFrame is one SSE `data:` frame from /api/sessions/stream.
//
// When no `player_id` query filter is set, `sessions` carries the
// normalised session list; when a filter is set, `session` carries the
// one matching record and `active_summary` carries a compact summary of
// every active session for the dashboard's session picker.
//
// Shapes use additionalProperties (`map[string]any`) because the live
// session record has ~80 fields; see GET /api/session/{id} for the
// canonical field set.
type SessionStreamFrame struct {
	// Revision is a monotonically increasing counter; useful for
	// client-side dedup across reconnects.
	Revision uint64 `json:"revision"`
	// Sessions is the unfiltered list (player_id query absent).
	Sessions []map[string]any `json:"sessions,omitempty"`
	// Session is the single filtered record (player_id query present).
	Session map[string]any `json:"session,omitempty"`
	// ActiveSummary is the compact "what else is going on" list for
	// the dashboard's filtered view. Present only when filtered.
	ActiveSummary []map[string]any `json:"active_summary,omitempty"`
}
