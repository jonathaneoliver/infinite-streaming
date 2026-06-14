// Column-projection bundles for the /api/v2/timeseries endpoint.
//
// Each bundle names a curated subset of columns from one of the three
// underlying stream tables (session_snapshots → samples,
// network_requests → network, derived → events). Renderers ask for
// bundles by name so they don't have to enumerate columns; new
// bundles can be added without client changes.
//
// Bundles are versioned ("lanes_v1") so a future server-side
// pre-derivation (e.g. a materialised view that pre-segments player-
// state lanes) can ship as `lanes_v2` without disturbing in-flight
// clients on v1.
//
// File is named `streambundles.go` to avoid collision with the
// existing `bundle.go` (which is the per-session ZIP-bundle exporter
// — a completely unrelated artifact).
package main

import (
	"sort"
	"strings"
)

// streamKind names the three top-level data streams the v2
// timeseries endpoint can emit. Mirrors ringKind for events/network;
// control_events is read at query time and doesn't pass through the
// ring (very low volume).
//
// Issue #474 Milestone C dropped streamMarkers (the derived
// session_markers table retired) and added streamControl.
type streamKind string

const (
	streamEvents    streamKind = "events"
	streamNetwork   streamKind = "network"
	streamControl   streamKind = "control"
	// streamAVMetrics — iOS 18 AVMetrics raw events (issue #486 spike).
	// Sibling of streamControl: low-volume CH-only stream (no ring),
	// backfill via SQL + live continuation via a poller.
	streamAVMetrics streamKind = "avmetrics"
)

func parseStreamKind(s string) (streamKind, bool) {
	switch s {
	case "events":
		return streamEvents, true
	case "network":
		return streamNetwork, true
	case "control":
		return streamControl, true
	case "avmetrics":
		return streamAVMetrics, true
	}
	return "", false
}

// bundleDef declares the column set a named bundle exposes for one
// stream. `Columns` is in the order rows are emitted to the wire.
type bundleDef struct {
	Name    string
	Stream  streamKind
	Columns []string
}

// bundleRegistry — initial set per the v2 timeseries design. Each entry's column
// list is a curated projection of the underlying CH table. The `all`
// bundle is an alias that expands to lanes_v1 + network + events.
//
// When a renderer asks for `bundles=foo,bar` plus optional
// `fields=col1,col2`, the resolver below unions all named column sets
// for each stream, deduplicates, and produces the actual SELECT
// projection.
var bundleRegistry = map[string]bundleDef{

	// charts_minimal — what the four line charts (Bandwidth / RTT /
	// Buffer / FPS) need, nothing more. Roughly an order of magnitude
	// fewer bytes per sample than the full row, so a long brush
	// window stays cheap.
	"charts_minimal": {
		Name:   "charts_minimal",
		Stream: streamEvents,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id",
			"start_time", // client play start (#587) → rail left edge
			"event_time",
			// Bandwidth chart series (server + player side)
			"video_bitrate_mbps",
			"network_bitrate_mbps",
			"avg_network_bitrate_mbps",
			"measured_mbps",
			"mbps_shaper_rate",
			"mbps_shaper_avg",
			"mbps_transfer_rate",
			"mbps_transfer_complete",
			// RTT family
			"client_rtt_ms",
			"client_rtt_min_ms",
			"client_rtt_min_lifetime_ms",
			"client_rtt_max_ms",
			"client_path_ping_rtt_ms",
			"client_rto_ms",
			// Client-side RTT proxy from AVMetrics TTFB (issue #486).
			// Sits alongside the server-side TCP_INFO RTT on the
			// chart so gaps between the two views are visible.
			"client_rtt_avmetrics_ms",
			// Buffer & offsets
			"buffer_depth_s",
			"buffer_end_s",
			"live_offset_s",
			"true_offset_s",
			// FPS-derived counters
			"frames_displayed",
			"frames_dropped",
			"stall_count",
			"stall_time_s",
			// Player + manifest identity (used by hover tooltips +
			// the variant-resolution lookup in EventsTimeline if it
			// ever re-uses charts_minimal). Keep here so a single
			// charts_minimal subscription drives every renderer.
			"video_resolution",
			"manifest_url",
			"player_state",
			"player_error",
			// Shaper config so BandwidthChart's "Limit (rate_mbps)"
			// series can be drawn with the same value the user set.
			// Effective limit at any sample = pattern_rate_runtime_mbps
			// when a pattern is active, else the static
			// nftables_bandwidth_mbps. Pattern step index + steps JSON
			// let the adapter resolve the active step's rate when
			// pattern_rate_runtime_mbps is zeroed (legacy
			// "between-steps" fallback). Without these, archived plays
			// can never reconstruct the Limit line.
			"nftables_bandwidth_mbps",
			"nftables_pattern_enabled",
			"nftables_pattern_rate_runtime_mbps",
			"nftables_pattern_step",
			"nftables_pattern_steps",
			// effective_rate_limit_mbps — kernel-enforced cap at this
			// instant: max(operator override, deployment baseline).
			// Distinct from the operator-intent nftables_bandwidth_mbps
			// above. Lets the dashboard chart's "Effective Limit"
			// series read direct from CH instead of deriving
			// client-side. Issue #480.
			"effective_rate_limit_mbps",
			// Server's view of the player's active variant — the
			// "Server Variant" line that pairs with "Player Variant"
			// for catching server↔player rendition disagreement.
			"server_video_rendition_mbps",
		},
	},

	// panel_v1 — fills the PlayerMetrics grid on the testing /
	// session-viewer pages with the fields charts_minimal + lanes_v1
	// don't already cover. Without this bundle the brush-end-row
	// projection (SessionDisplay → chRowToPlayerRecord → archive cache)
	// can only fill ~19 of the 28 PlayerMetrics labels, and the panel
	// shows "—" for fields the data actually has.
	// panel_v1 — fills the PlayerMetrics grid + PlayLog chip rows
	// with extended player_metrics fields. Issue #486 added
	// `time_per_variant_s` (iOS per-variant watch-time breakdown,
	// a JSON-string keyed by `<res>@<kbps>kbps` → seconds) so
	// PlayLog's JSON-field expansion can render one chip per variant.
	"panel_v1": {
		Name:   "panel_v1",
		Stream: streamEvents,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id",
			"position_s",
			"playback_rate",
			"seekable_end_s",
			"live_edge_s",
			"metrics_source",
			"loop_count_player",
			"loop_count_delta",
			"state_from",
			"state_to",
			"content_name",
			"user_marked_at",
			"frames_rate",
			"video_quality_pct",
			"video_quality_60s_pct",
			"video_quality_avg_pct",
			"playhead_wallclock_ms",
			"trigger_type",
			"player_restarts",
			"profile_shift_count",
			"time_per_variant_s",
			// Issue #486 follow-up: manifest HOLD-BACK + active
			// variant nominal fps. PlayLog renders them as chips;
			// future "true offset" chart can derive
			// live_offset_s + recommended_offset_s.
			"recommended_offset_s",
			"configured_offset_s",
			"frames_rate",
			// #550 Phase 1: residency accumulators + sticky durations
			// + ms-renamed video startup. PlayerMetrics panel reads
			// these via chRowAdapter; without them the per-state
			// tiles render as "—" in the session viewer.
			"playing_time_ms", "playing_count",
			"pausing_time_ms", "pausing_count",
			"buffering_time_ms", "buffering_count",
			"stalling_time_ms", "stalling_count",
			"idling_time_ms", "idling_count",
			"seeking_time_ms", "seeking_count",
			"trickplaying_time_ms", "trickplaying_count",
			"stall_duration_ms",
			"buffering_duration_ms",
			"video_first_frame_time_ms",
			"video_start_time_ms",
			// #550 Phase 2: outcome + error fields (per-snapshot;
			// SessionDetails + PlayerMetrics both consume).
			"playback_status", "playback_reason",
			"error_code", "error_domain",
			"terminal_error_code", "terminal_error_domain",
			"error_count",
		},
	},

	// lanes_v1 — what EventsTimeline.vue needs to derive its swim
	// lanes (PLAYERSTATE / VARIANT / DISPLAY_RES / PLAYBACK /
	// IMPAIRMENT / CONTROL). A future `lanes_v2` can ship as a
	// pre-segmented MV without breaking lanes_v1 consumers.
	"lanes_v1": {
		Name:   "lanes_v1",
		Stream: streamEvents,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id",
			"player_state",
			"waiting_reason",
			"video_resolution",
			"video_bitrate_mbps",
			"display_resolution",
			"fetching_resolution",
			"stall_count",
			"frames_dropped",
			"player_error",
			// #703a — error_code/_domain so the IMPAIRMENT ERROR marker can show
			// the actual NSError code/domain (e.g. -1008 / NSURLErrorDomain), not
			// just the message string.
			"error_code",
			"error_domain",
			"last_event",
			// #703a — player_restarts revives the PLAYBACK-lane RESTART marker
			// (counter-diff; the column IS persisted — it's in panel_v1 too),
			// and last_event above carries the new `live_resync` nudge event.
			"player_restarts",
			// #703a — playback_rate drives the PLAYBACK-lane RATE→0 / RATE→1
			// markers (rate 0 == AVPlayer paused/stuck; distinguishes a stuck
			// stall from a transient one, which `player_state` masks). In
			// panel_v1 too; added here so EventsTimeline gets it per-tick.
			"playback_rate",
			"manifest_variants",
			"master_manifest_consecutive_failures",
			"manifest_consecutive_failures",
			"segment_consecutive_failures",
			"transport_consecutive_failures",
			"all_consecutive_failures",
			"fault_count_transfer_active_timeout",
			"fault_count_transfer_idle_timeout",
			"control_revision",
			"video_first_frame_time_s",
			"video_start_time_s",
			"loop_count_server",
		},
	},

	// session_details — the panel that lists "what is this player /
	// session". Static-ish fields the user reads, not chart-fed.
	"session_details": {
		Name:   "session_details",
		Stream: streamEvents,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id", "group_id",
			"content_id",
			"user_agent",
			"manifest_url",
			// master_manifest_url is the player-loaded MASTER playlist;
			// manifest_url above is the most-recently-fetched VARIANT.
			// SessionDetails' "Master Manifest URL" tile reads the
			// master — was missing here so it showed a variant URL.
			"master_manifest_url",
			"last_request_url",
			"player_state",
			"player_error",
			"last_event",
			"classification",
			"control_revision",
			// Identity fields SessionDetails reads at top level —
			// were missing from the projection so the panel showed
			// "—" for User Agent / Player IP / Port even though the
			// CH row had them. (Fix: projection-gap parity with the
			// Testing dashboard's PlayerRecord shape.)
			"player_ip", "origination_ip",
			"session_number",
			"attempt_id",
			"x_forwarded_port", "x_forwarded_port_external",
			"server_received_at_ms",
			// #550 Phase 4: device taxonomy — per-session stable
			// fields. SessionDetails.vue renders them alongside
			// identity tiles. Costs are minimal: LowCardinality
			// columns compress repeats to near-nothing.
			"device_class", "device_model", "player_tech", "player_tech_version",
			"app_version", "os_version_major", "os_version_minor",
		},
	},

	// network — every typed column on network_requests; the table's
	// JSON-string columns (headers, query_string) are excluded by
	// default and can be opted in via `fields=`.
	"network": {
		Name:   "network",
		Stream: streamNetwork,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id",
			"method", "url", "upstream_url", "path", "request_kind",
			"status",
			"bytes_in", "bytes_out", "content_type",
			"request_range", "response_content_range",
			"dns_ms", "connect_ms", "tls_ms",
			"ttfb_ms", "transfer_ms", "total_ms", "client_wait_ms",
			"faulted", "fault_type", "fault_action", "fault_category",
			"entry_fingerprint",
		},
	},

	// control — the proxy/harness action log (control_events). After
	// issue #474 Milestone C this replaces the old `events` (markers)
	// bundle. Lets the dashboard render fault_on/off, pattern_step,
	// session lifecycle, etc. on the same brush rail as other streams.
	"control": {
		Name:   "control",
		Stream: streamControl,
		Columns: []string{
			"ts", "play_id", "player_id", "session_id",
			"attempt_id", "source", "event", "info", "labels",
			"event_fingerprint",
		},
	},

	// avmetrics — iOS 18 AVMetrics raw event stream (issue #486 spike).
	// Sibling of `control` — low-volume CH-only stream, surfaced on
	// the same brush rail so the comparison "what AVMetrics says vs
	// what the heartbeat says" reads at a glance.
	"avmetrics": {
		Name:   "avmetrics",
		Stream: streamAVMetrics,
		Columns: []string{
			"ts", "play_id", "player_id", "session_id",
			"attempt_id", "event_type", "event_ts_ms", "raw_json",
			"labels", "event_fingerprint",
		},
	},
}

// bundleAliases — named composites that expand to a set of bundle
// names. Keeps the wire ergonomic without compounding the resolver.
var bundleAliases = map[string][]string{
	// `all` covers the three streams with their primary bundles.
	"all": {"lanes_v1", "network", "control"},
}

// streamSelection is the resolved projection for one stream — the
// distinct set of columns the renderer asked for (via bundles + ad-
// hoc `fields`), in canonical order.
type streamSelection struct {
	Stream  streamKind
	Columns []string
}

// resolveSelection turns the raw query params into a per-stream
// projection. Returns one entry per enabled stream, with columns
// deduplicated and sorted (sort is stable across calls so SELECT
// fingerprints don't churn with input order). Unknown bundle names
// are returned as errors; unknown field names are passed through
// (CH-side will reject if invalid, surfacing a clean 4xx).
//
// `streamsParam` is the comma list from `?streams=…`. Required:
// callers must opt in to what they want. `bundlesParam` and
// `fieldsByStream` are optional.
//
// `fieldsByStream` maps stream → list of column names, parsed by the
// caller (since field lists can in principle be per-stream; v3
// initial cut just splits the flat ?fields= list across all enabled
// streams as a convenience).
func resolveSelection(
	streamsParam string,
	bundlesParam string,
	fieldsByStream map[streamKind][]string,
) ([]streamSelection, error) {
	wanted := map[streamKind]bool{}
	for _, s := range splitCSV(streamsParam) {
		k, ok := parseStreamKind(s)
		if !ok {
			return nil, errBadParam("unknown stream: " + s)
		}
		wanted[k] = true
	}
	if len(wanted) == 0 {
		return nil, errBadParam("at least one of streams=samples,network,events is required")
	}

	// Expand bundle aliases.
	bundleNames := []string{}
	for _, b := range splitCSV(bundlesParam) {
		if expanded, ok := bundleAliases[b]; ok {
			bundleNames = append(bundleNames, expanded...)
			continue
		}
		bundleNames = append(bundleNames, b)
	}

	colsByStream := map[streamKind]map[string]struct{}{}
	for _, b := range bundleNames {
		def, ok := bundleRegistry[b]
		if !ok {
			return nil, errBadParam("unknown bundle: " + b)
		}
		if !wanted[def.Stream] {
			// Caller asked for a bundle whose stream they didn't
			// enable. Treat as a config bug, not a silent no-op —
			// 400 so the dashboard sees the mismatch loudly.
			return nil, errBadParam("bundle " + b + " is for stream " + string(def.Stream) + " which is not in streams=")
		}
		set, ok := colsByStream[def.Stream]
		if !ok {
			set = map[string]struct{}{}
			colsByStream[def.Stream] = set
		}
		for _, c := range def.Columns {
			set[c] = struct{}{}
		}
	}

	for stream, fields := range fieldsByStream {
		if !wanted[stream] {
			continue
		}
		set, ok := colsByStream[stream]
		if !ok {
			set = map[string]struct{}{}
			colsByStream[stream] = set
		}
		for _, c := range fields {
			if c = strings.TrimSpace(c); c != "" {
				set[c] = struct{}{}
			}
		}
	}

	// Ensure every enabled stream has at least its minimum identity
	// columns. Saves callers from having to enumerate them in every
	// `fields=` list.
	for stream := range wanted {
		set, ok := colsByStream[stream]
		if !ok {
			set = map[string]struct{}{}
			colsByStream[stream] = set
		}
		for _, c := range mandatoryColumns(stream) {
			set[c] = struct{}{}
		}
	}

	out := []streamSelection{}
	for stream := range wanted {
		set := colsByStream[stream]
		cols := make([]string, 0, len(set))
		for c := range set {
			cols = append(cols, c)
		}
		sort.Strings(cols)
		out = append(out, streamSelection{Stream: stream, Columns: cols})
	}
	// Stable iteration order: samples, network, events.
	sort.Slice(out, func(i, j int) bool { return out[i].Stream < out[j].Stream })
	return out, nil
}

// mandatoryColumns are forced into every selection for a stream so
// the timeseries handler doesn't have to special-case them.
//
//   - ts identifies position on the timeline (every event must be
//     placeable).
//   - session_id / play_id / player_id are the row's keys (used for
//     fingerprinting + dedup against the ring; also let the client
//     verify the row is on the expected play after a play_id
//     rotation).
//   - entry_fingerprint on network is the stable dedupe key for the
//     ring/CH boundary.
func mandatoryColumns(stream streamKind) []string {
	switch stream {
	case streamEvents:
		// attempt_id is the dashboard ATTEMPT_ID column; if the
		// active bundle didn't already pull it the column rendered
		// as "—" for every event row even though CH had the value.
		return []string{"ts", "session_id", "play_id", "player_id", "attempt_id", "labels"}
	case streamNetwork:
		return []string{"ts", "session_id", "play_id", "player_id", "attempt_id", "labels", "entry_fingerprint"}
	case streamControl:
		return []string{"ts", "play_id", "player_id", "session_id", "attempt_id", "labels", "event_fingerprint"}
	case streamAVMetrics:
		return []string{"ts", "play_id", "player_id", "session_id", "attempt_id", "labels", "event_fingerprint", "event_type"}
	}
	return nil
}

// splitCSV — comma-list parser tolerating whitespace + empty entries.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// errBadParam tags errors that should surface to the HTTP layer as
// 400. Plain `error` so the caller doesn't need a custom check.
type badParamError struct{ msg string }

func (e *badParamError) Error() string { return e.msg }
func errBadParam(msg string) error     { return &badParamError{msg: msg} }
func isBadParam(err error) bool        { _, ok := err.(*badParamError); return ok }
