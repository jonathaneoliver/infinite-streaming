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
// timeseries endpoint can emit. Mirrors ringKind for samples/network
// (events are derived at query time so they don't pass through the
// ring).
type streamKind string

const (
	streamSamples streamKind = "samples"
	streamNetwork streamKind = "network"
	streamEvents  streamKind = "events"
)

func parseStreamKind(s string) (streamKind, bool) {
	switch s {
	case "samples":
		return streamSamples, true
	case "network":
		return streamNetwork, true
	case "events":
		return streamEvents, true
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
		Stream: streamSamples,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id",
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
			// Buffer & offsets
			"buffer_depth_s",
			"buffer_end_s",
			"live_offset_s",
			"true_offset_s",
			// FPS-derived counters
			"frames_displayed",
			"dropped_frames",
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
			// Server's view of the player's active variant — the
			// "Server Variant" line that pairs with "Player Variant"
			// for catching server↔player rendition disagreement.
			"server_video_rendition_mbps",
		},
	},

	// lanes_v1 — what EventsTimeline.vue needs to derive its swim
	// lanes (PLAYERSTATE / VARIANT / DISPLAY_RES / PLAYBACK /
	// IMPAIRMENT / CONTROL). A future `lanes_v2` can ship as a
	// pre-segmented MV without breaking lanes_v1 consumers.
	"lanes_v1": {
		Name:   "lanes_v1",
		Stream: streamSamples,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id",
			"player_state",
			"waiting_reason",
			"video_resolution",
			"video_bitrate_mbps",
			"display_resolution",
			"stall_count",
			"dropped_frames",
			"player_error",
			"last_event",
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
		Stream: streamSamples,
		Columns: []string{
			"ts",
			"session_id", "play_id", "player_id", "group_id",
			"content_id",
			"user_agent",
			"manifest_url",
			"last_request_url",
			"player_state",
			"player_error",
			"last_event",
			"classification",
			"control_revision",
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

	// events — the kind/priority-classified rows from the derived SQL
	// in main.go (`/api/session_events` taxonomy). Columns here are
	// the post-projection wire shape, not the raw `session_snapshots`
	// columns. The timeseries handler delegates to the same SQL.
	"events": {
		Name:   "events",
		Stream: streamEvents,
		Columns: []string{
			"ts", "type", "info", "kind", "priority",
			"play_id", "player_id", "session_id",
		},
	},
}

// bundleAliases — named composites that expand to a set of bundle
// names. Keeps the wire ergonomic without compounding the resolver.
var bundleAliases = map[string][]string{
	// `all` covers the three streams with their primary bundles.
	"all": {"lanes_v1", "network", "events"},
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
	case streamSamples:
		return []string{"ts", "session_id", "play_id", "player_id"}
	case streamNetwork:
		return []string{"ts", "session_id", "play_id", "player_id", "entry_fingerprint"}
	case streamEvents:
		return []string{"ts", "play_id", "player_id", "session_id"}
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
