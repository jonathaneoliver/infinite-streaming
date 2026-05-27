// OpenAPI annotations for the analytics forwarder.
//
// All swag (https://github.com/swaggo/swag) annotations live here so the
// handler implementations in main.go stay focused on ClickHouse logic. The
// stub functions below have no runtime role; swag scans the comment blocks.
//
// Regenerate via `make openapi` at the repo root. Output: api/openapi/forwarder/.
//
// All endpoints are exposed externally under the /analytics/api/* prefix
// (nginx rewrites the leading /analytics off before proxying), so consumer
// URLs look like http://<host>:21000/analytics/api/sessions.

//	@title       Analytics forwarder API
//	@version     1.0
//	@description Read-only ClickHouse query layer over archived session
//	@description data: snapshots, network requests, heatmap, events, and
//	@description session-bundle ZIPs. Subscribes to go-proxy's SSE streams
//	@description and writes to ClickHouse with a 30-day TTL.
//	@servers.url             /analytics
//	@servers.description     Mounted under /analytics on the same origin as the spec

package main

//	@Summary  Health check
//	@Tags     diagnostics
//	@Produce  text/plain
//	@Success  200 {string} string "ok"
//	@Router   /healthz [get]
func docsHealthz() {}

//	@Summary  List archived sessions
//	@Description Returns one row per (session_id, play_id) seen in the archive within the time window.
//	@Tags     archive
//	@Produce  json
//	@Param    since query string false "ISO8601 lower bound (defaults to now-24h)"
//	@Param    until query string false "ISO8601 upper bound"
//	@Success  200 {array} object
//	@Router   /api/sessions [get]
func docsArchivedSessions() {}

//	@Summary  Count archived snapshots
//	@Tags     archive
//	@Produce  json
//	@Param    session  query string false "session_id filter"
//	@Param    play_id  query string false "play_id filter"
//	@Param    from     query string false "ISO8601 lower bound"
//	@Param    to       query string false "ISO8601 upper bound"
//	@Success  200 {object} object "{count: N}"
//	@Router   /api/snapshot_count [get]
func docsSnapshotCount() {}

//	@Summary  List session snapshots
//	@Description Each row is one normalized session-state record; the snapshot stream emits these on every relevant change.
//	@Tags     archive
//	@Produce  json
//	@Param    session    query string false "session_id filter"
//	@Param    play_id    query string false "play_id filter"
//	@Param    from       query string false "ISO8601 lower bound"
//	@Param    to         query string false "ISO8601 upper bound"
//	@Param    limit      query int    false "max rows (default server-side cap)"
//	@Param    order      query string false "asc|desc (default asc)"
//	@Param    stride_ms  query int    false "downsample to one row per stride"
//	@Success  200 {array} object
//	@Router   /api/snapshots [get]
func docsSnapshots() {}

//	@Summary  Per-second buffer + state heatmap
//	@Description Pre-binned grid for the dashboard's heatmap visualization. Useful for spotting stalls without pulling raw rows.
//	@Tags     archive
//	@Produce  json
//	@Param    session  query string false "session_id filter"
//	@Param    play_id  query string false "play_id filter"
//	@Param    buckets  query int    false "number of time buckets (default 240)"
//	@Success  200 {array} object
//	@Router   /api/session_heatmap [get]
func docsSessionHeatmap() {}

//	@Summary  Chronological session events
//	@Description Player + harness events: ABR shifts, stalls, fault firings, error events.
//	@Tags     archive
//	@Produce  json
//	@Param    session  query string false "session_id filter"
//	@Param    play_id  query string false "play_id filter"
//	@Param    limit    query int    false "max rows"
//	@Success  200 {array} object
//	@Router   /api/session_events [get]
func docsSessionEvents() {}

//	@Summary  Historical HAR-shaped network rows
//	@Description Per-request rows mirrored from go-proxy's `/api/network/stream`, with `fault_category` / `fault_action` columns. ~10s ingestion lag from live.
//	@Tags     archive
//	@Produce  json
//	@Param    session  query string false "session_id filter"
//	@Param    play_id  query string false "play_id filter"
//	@Param    from     query string false "ISO8601 lower bound"
//	@Param    to       query string false "ISO8601 upper bound"
//	@Param    limit    query int    false "max rows"
//	@Success  200 {array} object
//	@Router   /api/network_requests [get]
func docsNetworkRequests() {}

//	@Summary  Download a session bundle (ZIP)
//	@Description Streams a ZIP containing snapshots, events, network rows, and a HAR file for the given play_id (or full session_id). Useful for offline forensics.
//	@Tags     archive
//	@Produce  application/zip
//	@Param    session  query string false "session_id filter"
//	@Param    play_id  query string false "play_id filter (preferred — one play per bundle)"
//	@Success  200 {file} binary
//	@Router   /api/session_bundle [get]
func docsSessionBundle() {}

// Note: classification.go also registers `/api/sessions/` (with trailing
// slash) for per-session lookups. That handler is undocumented here
// pending a clearer canonical URL — the trailing-slash variant is a
// workaround for ServeMux prefix matching, not a public API.
