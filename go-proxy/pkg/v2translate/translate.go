// Package v2translate projects v1 SessionData (map[string]any with ~80
// fields) into v2 typed records. Lives outside go-proxy/internal/ so
// the analytics forwarder (a separate Go module) can reuse the same
// projection — see analytics/go-forwarder/v2_handlers.go and
// api/openapi/v2/forwarder.yaml#SnapshotRow. Sharing avoids drift
// between the live SSE wire shape and what archived rows decode to.
//
// The functions are read-only — they never mutate the input map.
package v2translate

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	oapigen "github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/v2oapigen"
)

// PlayerFromSession projects a v1 session record into a v2 PlayerRecord.
// Returns ok=false when the session has no player_id (v1 sessions
// occasionally land before the player has self-registered an ID).
//
// **Non-UUID v1 player_ids.** v2 spec defines player_id as UUIDv4, but
// existing v1 clients (Apple TV / Roku / Android TV / older app builds)
// frequently use 8-char short hex forms like "c4723433". To keep those
// players visible through the v2 read API we synthesize a deterministic
// UUIDv5 from the short form under a fixed namespace. The same string
// always derives the same UUID; reverse-resolution lives on the
// adapter side (see SessionByPlayerID).
//
// Phase B scope: identity, lifecycle, control_revision, labels.
// Mutation-side fields (fault_rules, shape) come back via the
// v2-shadow projections added in later phases. CurrentPlay is nil
// until Phase E surfaces play boundaries from the network log.
func PlayerFromSession(s map[string]any) (oapigen.PlayerRecord, bool) {
	rawPlayerID := getString(s, "player_id")
	if rawPlayerID == "" {
		return oapigen.PlayerRecord{}, false
	}
	playerUUID, err := uuid.Parse(rawPlayerID)
	if err != nil {
		// v1-compat fallback: derive a stable UUIDv5 so this player is
		// still addressable through v2's UUID-typed API surface.
		playerUUID = derivePlayerUUID(rawPlayerID)
	}

	rec := oapigen.PlayerRecord{
		Id:              playerUUID,
		DisplayId:       getInt(s, "session_number"),
		ControlRevision: getString(s, "control_revision"),
	}

	if ip := getString(s, "origination_ip"); ip != "" {
		rec.OriginationIp = &ip
	}
	if ip := getString(s, "player_ip"); ip != "" {
		rec.PlayerIp = &ip
	}
	if ua := getString(s, "user_agent"); ua != "" {
		rec.UserAgent = &ua
	}
	if v, ok := numericFloatTranslate(s["loop_count_server"]); ok {
		i := int(v)
		rec.LoopCountServer = &i
	}
	if v, ok := numericFloatTranslate(s["server_received_at_ms"]); ok && v > 0 {
		i := int(v)
		rec.ServerReceivedAtMs = &i
	}
	if t, ok := getTime(s, "session_start_time", "first_request_time"); ok {
		rec.FirstSeenAt = &t
	}
	if t, ok := getTime(s, "updated_at", "last_request_time"); ok {
		rec.LastSeenAt = &t
	}

	// v2-shadow fields written by the PATCH translators round-trip
	// through the GET so the v2 console / harness CLI sees what they
	// just wrote. Each field is opt-in and absent when the player has
	// never had the corresponding patch applied.
	if labels, ok := s["_v2_labels"].(map[string]any); ok && len(labels) > 0 {
		out := oapigen.Labels{}
		for k, v := range labels {
			if str, ok := v.(string); ok {
				out[k] = str
			}
		}
		if len(out) > 0 {
			rec.Labels = &out
		}
	}
	if rules, ok := s["_v2_fault_rules"].([]any); ok && len(rules) > 0 {
		out := make([]oapigen.FaultRule, 0, len(rules))
		for _, raw := range rules {
			rule, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, faultRuleFromMap(rule))
		}
		if len(out) > 0 {
			rec.FaultRules = &out
		}
	}
	if shape := shapeFromSession(s); shape != nil {
		rec.Shape = shape
	}
	if pm := playerMetricsFromSession(s); pm != nil {
		rec.PlayerMetrics = pm
	}
	if sm := serverMetricsFromSession(s); sm != nil {
		rec.ServerMetrics = sm
	}
	if fc := faultCountersFromSession(s); fc != nil {
		rec.FaultCounters = fc
	}
	if play := currentPlayFromSession(s, playerUUID); play != nil {
		rec.CurrentPlay = play
	}
	if tt := transferTimeoutsFromSession(s); tt != nil {
		rec.TransferTimeouts = tt
	}
	if c := contentManipulationFromSession(s); c != nil {
		rec.Content = c
	}
	return rec, true
}

// transferTimeoutsFromSession projects the v1 `transfer_*_timeout_seconds`
// + `transfer_timeout_applies_*` family. Returns nil only when EVERY
// field is at its default (segments-only, both timeouts disabled), so
// the dashboard can skip the section when nothing is configured.
func transferTimeoutsFromSession(s map[string]any) *oapigen.TransferTimeouts {
	active := 0
	idle := 0
	if v, ok := numericFloatTranslate(s["transfer_active_timeout_seconds"]); ok {
		active = int(v)
	}
	if v, ok := numericFloatTranslate(s["transfer_idle_timeout_seconds"]); ok {
		idle = int(v)
	}
	appliesSegments := true
	appliesManifests := false
	appliesMaster := false
	if v, ok := s["transfer_timeout_applies_segments"].(bool); ok {
		appliesSegments = v
	}
	if v, ok := s["transfer_timeout_applies_manifests"].(bool); ok {
		appliesManifests = v
	}
	if v, ok := s["transfer_timeout_applies_master"].(bool); ok {
		appliesMaster = v
	}
	// Default-everywhere → suppress.
	if active == 0 && idle == 0 && appliesSegments && !appliesManifests && !appliesMaster {
		return nil
	}
	out := oapigen.TransferTimeouts{
		ActiveTimeoutSeconds: &active,
		IdleTimeoutSeconds:   &idle,
		AppliesSegments:      &appliesSegments,
		AppliesManifests:     &appliesManifests,
		AppliesMaster:        &appliesMaster,
	}
	return &out
}

// contentManipulationFromSession projects the v1 `content_*` family.
// Returns nil when no manipulation is enabled — the dashboard hides
// the section in that case.
func contentManipulationFromSession(s map[string]any) *oapigen.ContentManipulation {
	stripCodecs, _ := s["content_strip_codecs"].(bool)
	stripAvgBw, _ := s["content_strip_average_bandwidth"].(bool)
	stripResolution, _ := s["content_strip_resolution"].(bool)
	overstate, _ := s["content_overstate_bandwidth"].(bool)
	offset := 0
	if v, ok := numericFloatTranslate(s["content_live_offset"]); ok {
		offset = int(v)
	}
	var allowed []string
	if raw, ok := s["content_allowed_variants"].([]any); ok {
		for _, v := range raw {
			if str, ok := v.(string); ok {
				allowed = append(allowed, str)
			}
		}
	} else if raw, ok := s["content_allowed_variants"].([]string); ok {
		allowed = append([]string{}, raw...)
	}
	if !stripCodecs && !stripAvgBw && !stripResolution && !overstate && offset == 0 && len(allowed) == 0 {
		return nil
	}
	off := oapigen.ContentManipulationLiveOffset(offset)
	out := oapigen.ContentManipulation{
		StripCodecs:           &stripCodecs,
		StripAverageBandwidth: &stripAvgBw,
		StripResolution:       &stripResolution,
		OverstateBandwidth:    &overstate,
		LiveOffset:            &off,
	}
	if allowed != nil {
		out.AllowedVariants = &allowed
	}
	return &out
}

// playerMetricsFromSession projects v1's `player_metrics_*` family back
// into the typed v2 PlayerMetrics shape. Every player-reported field
// in the v1 testing-session UI is surfaced here.
//
// Returns nil when no field is set — keeps the wire compact.
func playerMetricsFromSession(s map[string]any) *oapigen.PlayerMetrics {
	pm := oapigen.PlayerMetrics{}
	any := false

	// String fields
	for _, m := range []struct {
		key string
		dst **string
	}{
		{"player_metrics_video_resolution", &pm.VideoResolution},
		{"player_metrics_display_resolution", &pm.DisplayResolution},
		{"player_metrics_last_event", &pm.LastEvent},
		{"player_metrics_trigger_type", &pm.TriggerType},
		{"player_metrics_state", &pm.State},
		{"player_metrics_waiting_reason", &pm.WaitingReason},
		{"player_metrics_browser_family", &pm.BrowserFamily},
		{"player_metrics_playback_engine", &pm.PlaybackEngine},
		{"player_metrics_error", &pm.Error},
		{"player_metrics_source", &pm.Source},
		// #550 Phase 2: outcome + error string fields.
		{"player_metrics_playback_status", &pm.PlaybackStatus},
		{"player_metrics_playback_reason", &pm.PlaybackReason},
		{"player_metrics_error_domain", &pm.ErrorDomain},
		{"player_metrics_error_details", &pm.ErrorDetails},
		{"player_metrics_terminal_error_domain", &pm.TerminalErrorDomain},
		{"player_metrics_terminal_error_details", &pm.TerminalErrorDetails},
		// #550 Phase 4: device taxonomy string fields.
		{"player_metrics_app_version", &pm.AppVersion},
		{"player_metrics_device_class", &pm.DeviceClass},
		{"player_metrics_device_model", &pm.DeviceModel},
		{"player_metrics_player_tech", &pm.PlayerTech},
	} {
		if v, ok := s[m.key].(string); ok && v != "" {
			vv := v
			*m.dst = &vv
			any = true
		}
	}

	// Float fields (fractional or 0+)
	for _, m := range []struct {
		key string
		dst **float32
	}{
		{"player_metrics_video_bitrate_mbps", &pm.VideoBitrateMbps},
		{"player_metrics_video_quality_pct", &pm.VideoQualityPct},
		{"player_metrics_avg_network_bitrate_mbps", &pm.AvgNetworkBitrateMbps},
		{"player_metrics_network_bitrate_mbps", &pm.NetworkBitrateMbps},
		{"player_metrics_buffer_depth_s", &pm.BufferDepthS},
		{"player_metrics_buffer_end_s", &pm.BufferEndS},
		{"player_metrics_seekable_end_s", &pm.SeekableEndS},
		{"player_metrics_live_edge_s", &pm.LiveEdgeS},
		{"player_metrics_live_offset_s", &pm.LiveOffsetS},
		{"player_metrics_true_offset_s", &pm.TrueOffsetS},
		{"player_metrics_position_s", &pm.PositionS},
		{"player_metrics_playback_rate", &pm.PlaybackRate},
		{"player_metrics_video_first_frame_time_s", &pm.FirstFrameTimeS},
		{"player_metrics_video_start_time_s", &pm.VideoStartTimeS},
		{"player_metrics_stall_time_s", &pm.StallTimeS},
		{"player_metrics_last_stall_time_s", &pm.LastStallTimeS},
		// #550 Phase 4: only float field in device taxonomy.
		{"player_metrics_screen_density", &pm.ScreenDensity},
	} {
		if v, ok := numericFloatTranslate(s[m.key]); ok {
			f := float32(v)
			*m.dst = &f
			any = true
		}
	}

	// Integer counter fields
	for _, m := range []struct {
		key string
		dst **int
	}{
		{"player_metrics_stalls", &pm.Stalls},
		{"player_metrics_stall_count", &pm.Stalls}, // v1 alias
		{"player_metrics_frames_displayed", &pm.FramesDisplayed},
		{"player_metrics_dropped_frames", &pm.DroppedFrames},
		{"player_restarts", &pm.PlayerRestarts},
		{"player_metrics_loop_count_player", &pm.LoopCountPlayer},
		{"player_metrics_loop_count_increment", &pm.LoopCountIncrement},
		{"player_metrics_profile_shift_count", &pm.ProfileShiftCount},
		{"player_metrics_playhead_wallclock_ms", &pm.PlayheadWallclockMs},
		// #550 Phase 1: residency accumulators + per-event durations.
		{"player_metrics_playing_time_ms", &pm.PlayingTimeMs},
		{"player_metrics_playing_count", &pm.PlayingCount},
		{"player_metrics_pausing_time_ms", &pm.PausingTimeMs},
		{"player_metrics_pausing_count", &pm.PausingCount},
		{"player_metrics_buffering_time_ms", &pm.BufferingTimeMs},
		{"player_metrics_buffering_count", &pm.BufferingCount},
		{"player_metrics_stalling_time_ms", &pm.StallingTimeMs},
		{"player_metrics_stalling_count", &pm.StallingCount},
		{"player_metrics_idling_time_ms", &pm.IdlingTimeMs},
		{"player_metrics_idling_count", &pm.IdlingCount},
		{"player_metrics_seeking_time_ms", &pm.SeekingTimeMs},
		{"player_metrics_seeking_count", &pm.SeekingCount},
		{"player_metrics_trickplaying_time_ms", &pm.TrickplayingTimeMs},
		{"player_metrics_trickplaying_count", &pm.TrickplayingCount},
		{"player_metrics_stall_duration_ms", &pm.StallDurationMs},
		{"player_metrics_buffering_duration_ms", &pm.BufferingDurationMs},
		{"player_metrics_video_first_frame_time_ms", &pm.VideoFirstFrameTimeMs},
		{"player_metrics_video_start_time_ms", &pm.VideoStartTimeMs},
		// #550 Phase 2: error code + counter (signed code via int; NSError codes are negative).
		{"player_metrics_error_code", &pm.ErrorCode},
		{"player_metrics_terminal_error_code", &pm.TerminalErrorCode},
		{"player_metrics_error_count", &pm.ErrorCount},
		// #550 Phase 4: integer device taxonomy fields.
		{"player_metrics_os_version_major", &pm.OsVersionMajor},
		{"player_metrics_os_version_minor", &pm.OsVersionMinor},
		{"player_metrics_screen_width_px", &pm.ScreenWidthPx},
		{"player_metrics_screen_height_px", &pm.ScreenHeightPx},
	} {
		if v, ok := numericFloatTranslate(s[m.key]); ok {
			i := int(v)
			*m.dst = &i
			any = true
		}
	}

	if t, ok := getTime(s, "player_metrics_event_time"); ok {
		pm.EventTime = &t
		any = true
	}
	if !any {
		return nil
	}
	return &pm
}

// serverMetricsFromSession projects v1's TCP_INFO / ICMP / byte-counter
// / shaper-bookkeeping family back into the typed v2 ServerMetrics
// shape. Returns nil when no server-observed telemetry exists (e.g.
// macOS dev builds where TCP_INFO is a no-op).
func serverMetricsFromSession(s map[string]any) *oapigen.ServerMetrics {
	sm := oapigen.ServerMetrics{}
	any := false

	if v, ok := s["player_metrics_video_url"].(string); ok && v != "" {
		sm.RenditionUrl = &v
		any = true
	}
	if v, ok := s["server_video_rendition"].(string); ok && v != "" {
		sm.ServerRendition = &v
		any = true
	}
	if v, ok := numericFloatTranslate(s["server_video_rendition_mbps"]); ok && v > 0 {
		f := float32(v)
		sm.RenditionMbps = &f
		any = true
	} else if v, ok := numericFloatTranslate(s["player_metrics_video_bitrate_mbps"]); ok && v > 0 {
		f := float32(v)
		sm.RenditionMbps = &f
		any = true
	}
	for key, set := range map[string]func(float32){
		"client_rtt_ms":              func(f float32) { sm.RttMs = &f },
		"client_rtt_min_ms":          func(f float32) { sm.RttMinMs = &f },
		"client_rtt_max_ms":          func(f float32) { sm.RttMaxMs = &f },
		"client_rtt_min_lifetime_ms": func(f float32) { sm.RttMinLifetimeMs = &f },
		"client_rtt_var_ms":          func(f float32) { sm.RttVarMs = &f },
		"client_rto_ms":              func(f float32) { sm.RtoMs = &f },
		"client_path_ping_rtt_ms":    func(f float32) { sm.PathPingRttMs = &f },
		// Shaper / transfer measurements (developer-mode in v1).
		"mbps_shaper_avg":            func(f float32) { sm.MbpsShaperAvg = &f },
		"mbps_shaper_rate":           func(f float32) { sm.MbpsShaperRate = &f },
		"mbps_transfer_rate":         func(f float32) { sm.MbpsTransferRate = &f },
		"mbps_transfer_complete":     func(f float32) { sm.MbpsTransferComplete = &f },
		"mbps_in":                    func(f float32) { sm.MbpsIn = &f },
		"mbps_out":                   func(f float32) { sm.MbpsOut = &f },
		"mbps_in_avg":                func(f float32) { sm.MbpsInAvg = &f },
		"mbps_in_active":             func(f float32) { sm.MbpsInActive = &f },
		"measured_mbps":              func(f float32) { sm.MeasuredMbps = &f },
		"measurement_window_io":      func(f float32) { sm.MeasurementWindowIo = &f },
		"measurement_window_active":  func(f float32) { sm.MeasurementWindowActive = &f },
	} {
		if v, ok := numericFloatTranslate(s[key]); ok {
			set(float32(v))
			any = true
		}
	}
	if v, ok := s["client_rtt_stale"].(bool); ok {
		sm.RttStale = &v
		any = true
	}
	for key, set := range map[string]func(int){
		"bytes_in_total":  func(i int) { sm.BytesInTotal = &i },
		"bytes_out_total": func(i int) { sm.BytesOutTotal = &i },
		"bytes_in_last":   func(i int) { sm.BytesInLast = &i },
		"bytes_out_last":  func(i int) { sm.BytesOutLast = &i },
		"bytes_last_ts":   func(i int) { sm.BytesLastTs = &i },
	} {
		if v, ok := numericFloatTranslate(s[key]); ok {
			set(int(v))
			any = true
		}
	}
	if !any {
		return nil
	}
	return &sm
}

// faultCountersFromSession projects v1's `fault_count_*` family into a
// `FaultCounters` map (string → int). Returns nil when no counter is
// non-zero — saves the dashboard from rendering a sea of zeros.
func faultCountersFromSession(s map[string]any) *oapigen.FaultCounters {
	out := oapigen.FaultCounters{}
	for k, v := range s {
		if !strings.HasPrefix(k, "fault_count_") {
			continue
		}
		n, ok := numericFloatTranslate(v)
		if !ok {
			continue
		}
		// Strip the `fault_count_` prefix so the v2 map reads
		// `{total: 42, socket_drop: 3}` not `{fault_count_total: 42, ...}`.
		out[strings.TrimPrefix(k, "fault_count_")] = int(n)
	}
	if len(out) == 0 {
		return nil
	}
	// Suppress when every counter is zero (typical for a fresh session).
	allZero := true
	for _, v := range out {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return nil
	}
	return &out
}

// currentPlayFromSession projects the active play (if any) from the
// session's player-supplied play_id + manifest_variants snapshot.
// Returns nil when the session has no active play to surface.
//
// playerUUID is the v2-canonical UUID for the parent player (used for
// PlayRecord.PlayerId — we don't re-derive it).
//
// The proxy NEVER synthesises a play_id. play_id (and attempt_id) are
// driven by the player at well-defined boundaries (new content for
// play_id; restart event increments attempt_id) and propagated as
// URL query params on every request. If the player hasn't yet supplied a
// play_id, PlayRecord.Id is left zero — downstream tables get blank
// rather than the proxy guessing. See ticket on the bug where the
// previous control_revision-seeded synthesis caused snapshots-side
// play_id to rotate on every label change.
func currentPlayFromSession(s map[string]any, playerUUID uuid.UUID) *oapigen.PlayRecord {
	master := getString(s, "master_manifest_url")
	manifest := getString(s, "manifest_url")
	playIDRaw := getString(s, "play_id")
	if playIDRaw == "" {
		playIDRaw = getString(s, "current_play_id")
	}
	attemptIDRaw := getString(s, "attempt_id")

	// Surface a play whenever we have an explicit id OR enough manifest
	// info to describe one. Web sessions (legacy testing.html, v3 grid)
	// rarely emit play_id but the master URL is always present — that
	// powers SessionDetails.master_manifest_url in the dashboard.
	if playIDRaw == "" && master == "" && manifest == "" {
		return nil
	}

	var playUUID uuid.UUID
	if playIDRaw != "" {
		if parsed, err := uuid.Parse(playIDRaw); err == nil {
			playUUID = parsed
		} else {
			// Player sent a non-UUID identifier (older client / test
			// harness). Hash it deterministically so the same id always
			// maps to the same UUID — but don't invent one from session
			// state. If the client didn't send a play_id at all, Id
			// stays zero (uuid.Nil).
			playUUID = derivePlayerUUID(playIDRaw)
		}
	}
	// playUUID == uuid.Nil when no player_id was supplied; that's fine —
	// downstream consumers treat the zero UUID as "no play yet".

	rec := &oapigen.PlayRecord{
		Id:              playUUID,
		PlayerId:        playerUUID,
		ControlRevision: getString(s, "control_revision"),
	}
	if attemptIDRaw != "" {
		if n, err := strconv.ParseUint(attemptIDRaw, 10, 32); err == nil {
			a := int(n)
			rec.AttemptId = &a
		}
	}
	if t, ok := getTime(s, "session_start_time", "first_request_time"); ok {
		rec.StartedAt = t
	}
	// Manifest projection: master_manifest_url is the master playlist
	// the player loaded; manifest_url is the variant playlist most
	// recently fetched. Prefer the explicit master, fall back to the
	// variant (matches the legacy `session_master_manifest_url` /
	// `session_manifest_url` distinction).
	variants := manifestVariantsFromSession(s)
	if variants != nil || master != "" || manifest != "" {
		m := &oapigen.Manifest{}
		if variants != nil {
			m.Variants = variants
		}
		switch {
		case master != "":
			m.MasterUrl = &master
		case manifest != "":
			m.MasterUrl = &manifest
		}
		rec.Manifest = m
	}
	if pm := playerMetricsFromSession(s); pm != nil {
		rec.PlayerMetrics = pm
	}
	if sm := serverMetricsFromSession(s); sm != nil {
		rec.ServerMetrics = sm
	}
	return rec
}

// manifestVariantsFromSession projects v1's `manifest_variants` slice
// into the typed v2 ManifestVariant array. The field lands here in one
// of three concrete shapes:
//
//   - []any of map[string]any   — set when the session map crossed a
//     JSON boundary (e.g. round-tripped through /api/sessions, or
//     replayed from SSE).
//   - []map[string]any          — alternate map form from the
//     /api/setup bootstrap path.
//   - []main.PlaylistInfo       — set DIRECTLY by go-proxy's manifest
//     parsing path (main.go § handleProxiedRequest) with no JSON in
//     between. This was the silent gap that made
//     /api/v2/players.current_play.manifest.variants come up empty
//     while the same player's /api/sessions row had variants. The
//     reflection-free `default` branch below catches it (and any
//     future typed-slice form) by re-marshalling through JSON.
//
// Round-tripping through json.Marshal/Unmarshal is the cheapest
// type-agnostic adapter that doesn't pull a reflection dependency
// into the package, and doesn't force v2translate to import the
// proxy's main package for the PlaylistInfo type.
func manifestVariantsFromSession(s map[string]any) *[]oapigen.ManifestVariant {
	raw, ok := s["manifest_variants"]
	if !ok || raw == nil {
		return nil
	}
	var out []oapigen.ManifestVariant
	switch arr := raw.(type) {
	case []any:
		for _, v := range arr {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, manifestVariantFromMap(m))
		}
	case []map[string]any:
		for _, m := range arr {
			out = append(out, manifestVariantFromMap(m))
		}
	default:
		// Typed slice — re-marshal through JSON so we read it as a
		// generic []map[string]any regardless of the concrete type.
		// Cost: one tiny marshal per /api/v2/players request (5–10
		// rows × ~80 bytes); negligible.
		buf, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		var maps []map[string]any
		if err := json.Unmarshal(buf, &maps); err != nil {
			return nil
		}
		for _, m := range maps {
			out = append(out, manifestVariantFromMap(m))
		}
	}
	if len(out) == 0 {
		return nil
	}
	return &out
}

func manifestVariantFromMap(m map[string]any) oapigen.ManifestVariant {
	mv := oapigen.ManifestVariant{}
	if u, ok := m["url"].(string); ok {
		mv.Url = u
	}
	if v, ok := numericFloatTranslate(m["bandwidth"]); ok {
		mv.Bandwidth = int(v)
	}
	if v, ok := numericFloatTranslate(m["average_bandwidth"]); ok && v > 0 {
		ab := int(v)
		mv.AverageBandwidth = &ab
	}
	if r, ok := m["resolution"].(string); ok {
		mv.Resolution = r
	}
	return mv
}

// faultRuleFromMap projects a v2 fault rule (stored as map[string]any
// on `_v2_fault_rules`) back into the typed schema.
func faultRuleFromMap(rule map[string]any) oapigen.FaultRule {
	out := oapigen.FaultRule{}
	if id, ok := rule["id"].(string); ok {
		out.Id = &id
	}
	if t, ok := rule["type"].(string); ok {
		out.Type = oapigen.FaultRuleType(t)
	}
	if v, ok := numericFloatTranslate(rule["frequency"]); ok {
		freq := int(v)
		out.Frequency = &freq
	}
	if v, ok := numericFloatTranslate(rule["consecutive"]); ok {
		consec := int(v)
		out.Consecutive = &consec
	}
	if mode, ok := rule["mode"].(string); ok && mode != "" {
		m := oapigen.FaultRuleMode(mode)
		out.Mode = &m
	}
	if filter, ok := rule["filter"].(map[string]any); ok && len(filter) > 0 {
		f := oapigen.FaultFilter{}
		if kinds, ok := filter["request_kind"].([]any); ok && len(kinds) > 0 {
			rk := make([]oapigen.FaultFilterRequestKind, 0, len(kinds))
			for _, k := range kinds {
				if s, ok := k.(string); ok {
					rk = append(rk, oapigen.FaultFilterRequestKind(s))
				}
			}
			if len(rk) > 0 {
				f.RequestKind = &rk
			}
		}
		out.Filter = &f
	}
	return out
}

// shapeFromSession projects v1's nftables_* fields + transport_*
// fields + `_v2_shape_pattern` stash back into a v2 Shape. Returns nil
// when no shape is configured (rate=0, delay=0, loss=0, no transport
// fault, no pattern).
func shapeFromSession(s map[string]any) *oapigen.Shape {
	rate, _ := numericFloatTranslate(s["nftables_bandwidth_mbps"])
	delay, _ := numericFloatTranslate(s["nftables_delay_ms"])
	loss, _ := numericFloatTranslate(s["nftables_packet_loss"])
	tfType, _ := s["transport_failure_type"].(string)
	pattern, _ := s["_v2_shape_pattern"].(map[string]any)

	if rate == 0 && delay == 0 && loss == 0 && (tfType == "" || tfType == "none") && pattern == nil {
		return nil
	}
	out := &oapigen.Shape{}
	if rate > 0 {
		r := float32(rate)
		out.RateMbps = &r
	}
	if delay > 0 {
		d := float32(delay)
		out.DelayMs = &d
	}
	if loss > 0 {
		l := float32(loss)
		out.LossPct = &l
	}
	if tfType != "" && tfType != "none" {
		tf := oapigen.TransportFault{Type: oapigen.TransportFaultType(tfType)}
		if v, ok := numericFloatTranslate(s["transport_failure_frequency"]); ok && v > 0 {
			f := int(v)
			tf.Frequency = &f
		}
		if v, ok := numericFloatTranslate(s["transport_consecutive_failures"]); ok && v >= 1 {
			c := int(v)
			tf.Consecutive = &c
		}
		if mode, ok := s["transport_failure_mode"].(string); ok && mode != "" {
			m := oapigen.TransportFaultMode(mode)
			tf.Mode = &m
		}
		out.TransportFault = &tf
	}
	if pattern != nil {
		p := oapigen.Pattern{}
		if t, ok := pattern["template"].(string); ok && t != "" {
			tmpl := oapigen.PatternTemplate(t)
			p.Template = &tmpl
		}
		if v, ok := numericFloatTranslate(pattern["margin_pct"]); ok {
			mp := oapigen.PatternMarginPct(int(v))
			p.MarginPct = &mp
		}
		if v, ok := numericFloatTranslate(pattern["default_step_seconds"]); ok {
			ds := oapigen.PatternDefaultStepSeconds(int(v))
			p.DefaultStepSeconds = &ds
		}
		if stepsAny, ok := pattern["steps"].([]any); ok {
			for _, raw := range stepsAny {
				step, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				ps := oapigen.PatternStep{}
				if v, ok := numericFloatTranslate(step["duration_seconds"]); ok {
					ps.DurationSeconds = int(v)
				}
				if v, ok := numericFloatTranslate(step["rate_mbps"]); ok {
					ps.RateMbps = float32(v)
				}
				if v, ok := step["enabled"].(bool); ok {
					e := v
					ps.Enabled = &e
				}
				p.Steps = append(p.Steps, ps)
			}
		}
		out.Pattern = &p
	}
	// Pattern runtime telemetry — surfaced even outside the pattern
	// block so the dashboard can read them without a nil check on
	// pattern. Only populated when the kernel cycle is in flight.
	if v, ok := numericFloatTranslate(s["nftables_pattern_step"]); ok && v > 0 {
		i := int(v)
		out.PatternStep = &i
	}
	if v, ok := numericFloatTranslate(s["nftables_pattern_step_runtime"]); ok && v > 0 {
		i := int(v)
		out.PatternStepRuntime = &i
	}
	if v, ok := numericFloatTranslate(s["nftables_pattern_rate_runtime_mbps"]); ok && v >= 0 {
		f := float32(v)
		out.PatternRateRuntimeMbps = &f
	}
	return out
}

// numericFloatTranslate is the read-side numeric coercer (mirror of
// numericFloat in handlers_mutate.go but living here so translate.go
// stays self-contained).
func numericFloatTranslate(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	}
	return 0, false
}

// NetworkEntryFromV1 projects a v1 network ring-buffer row into a v2
// NetworkLogEntry. The v1 row is itself a map produced by the network
// log subsystem; only HAR-shaped fields are copied through.
func NetworkEntryFromV1(row map[string]any) oapigen.NetworkLogEntry {
	out := oapigen.NetworkLogEntry{}
	if v := getString(row, "method"); v != "" {
		out.Method = &v
	}
	if v := getString(row, "url"); v != "" {
		out.Url = &v
	}
	if v := getString(row, "upstream_url"); v != "" {
		out.UpstreamUrl = &v
	}
	if v := getString(row, "path"); v != "" {
		out.Path = &v
	}
	if v := getString(row, "request_kind"); v != "" {
		out.RequestKind = &v
	}
	if v := getString(row, "content_type"); v != "" {
		out.ContentType = &v
	}
	if v := getInt(row, "status"); v != 0 {
		out.Status = &v
	}
	// bytes_in / bytes_out: 0 is a real value (HEAD-style or empty body),
	// so surface the field whenever the key is present.
	if _, ok := row["bytes_in"]; ok {
		v := getInt(row, "bytes_in")
		out.BytesIn = &v
	}
	if _, ok := row["bytes_out"]; ok {
		v := getInt(row, "bytes_out")
		out.BytesOut = &v
	}
	if t, ok := getTime(row, "timestamp"); ok {
		out.Timestamp = &t
	}
	if v := getString(row, "play_id"); v != "" {
		if u, err := uuid.Parse(v); err == nil {
			out.PlayId = &u
		}
	}
	// Phase timings — surfaced when httptrace populated them. Useful
	// for the dashboard's tooltip phase-breakdown bar.
	for _, m := range []struct {
		key string
		dst **float32
	}{
		{"ttfb_ms", &out.TtfbMs},
		{"total_ms", &out.TotalMs},
		{"dns_ms", &out.DnsMs},
		{"connect_ms", &out.ConnectMs},
		{"tls_ms", &out.TlsMs},
		{"transfer_ms", &out.TransferMs},
		{"client_wait_ms", &out.ClientWaitMs},
	} {
		if v, ok := numericFloatTranslate(row[m.key]); ok {
			f := float32(v)
			*m.dst = &f
		}
	}
	// Fault metadata — flagged on rows where the proxy injected a fault.
	if v, ok := row["faulted"].(bool); ok && v {
		t := true
		out.Faulted = &t
	}
	if v := getString(row, "fault_type"); v != "" {
		out.FaultType = &v
	}
	if v := getString(row, "fault_action"); v != "" {
		out.FaultAction = &v
	}
	if v := getString(row, "fault_category"); v != "" {
		out.FaultCategory = &v
	}
	return out
}

// GroupsFromSessions builds the live group set by walking every session
// and gathering the distinct group_id tags. Each tag becomes one
// PlayerGroup with members = players that share the tag.
//
// v1 has no separate group resource — group_id is just a string
// stored on each session. v2 surfaces it as a first-class collection.
func GroupsFromSessions(sessions []map[string]any) []oapigen.PlayerGroup {
	byID := map[string]*oapigen.PlayerGroup{}
	order := []string{}
	for _, s := range sessions {
		gid := getString(s, "group_id")
		if gid == "" {
			continue
		}
		pid := getString(s, "player_id")
		if pid == "" {
			continue
		}
		playerUUID, err := uuid.Parse(pid)
		if err != nil {
			// v1 short-form (e.g. "427a6bf3") never parses as UUID, but
			// PlayerFromSession surfaces the same session under the
			// stable v5 derivation. Mirror that here so the group's
			// member list lines up with /api/v2/players — without this
			// the picker's groupIdOf map never resolves the short-form
			// pill back to its group, so only the natively-UUID member
			// gets the grouped highlight.
			playerUUID = derivePlayerUUID(pid)
		}
		g, exists := byID[gid]
		if !exists {
			// v2-created groups store the v1 tag as a canonical UUIDv4
			// (handlers_groups.PostApiV2PlayerGroups uses uuid.New()),
			// so when the tag parses we use it directly — this keeps
			// POST's `id` and GET's `id` identical, which is what the
			// v3 client relies on for disband/lookup. Legacy v1 tags
			// (e.g. "G1234") aren't parseable and fall through to the
			// stable v5 derivation as before.
			var groupUUID openapi_types.UUID
			if u, perr := uuid.Parse(gid); perr == nil {
				groupUUID = u
			} else {
				var err error
				groupUUID, err = StableGroupUUID(gid)
				if err != nil {
					continue
				}
			}
			label := gid
			g = &oapigen.PlayerGroup{
				Id:              groupUUID,
				Label:           &label,
				MemberPlayerIds: []openapi_types.UUID{},
			}
			byID[gid] = g
			order = append(order, gid)
		}
		g.MemberPlayerIds = append(g.MemberPlayerIds, playerUUID)
	}
	out := make([]oapigen.PlayerGroup, 0, len(order))
	for _, gid := range order {
		out = append(out, *byID[gid])
	}
	return out
}

// playerUUIDNamespace is the v5 namespace used to derive deterministic
// UUIDs from non-UUID v1 player_id strings (e.g. "c4723433"). Fixed
// once and never rotated so the derived UUID is stable across
// process restarts and proxy redeploys.
var playerUUIDNamespace = uuid.MustParse("4f0a8c14-2bb5-4a31-a2e1-0b6c63c83e88")

// derivePlayerUUID returns the stable UUIDv5 for a non-UUID v1
// player_id string. Pure — no side effects.
func derivePlayerUUID(rawPlayerID string) uuid.UUID {
	return uuid.NewSHA1(playerUUIDNamespace, []byte(rawPlayerID))
}

// PlayerUUIDForRawID exposes the derivation rule to the adapter so
// reverse-resolution can match incoming canonical UUIDs back to the
// v1 short-form session.
func PlayerUUIDForRawID(rawPlayerID string) uuid.UUID {
	if u, err := uuid.Parse(rawPlayerID); err == nil {
		return u
	}
	return derivePlayerUUID(rawPlayerID)
}

// StableGroupUUID maps a v1 string group_id (e.g. "G1234") to a
// deterministic v5 UUID under a fixed namespace, so the same group_id
// always produces the same v2 GroupId across requests.
//
// v5 chosen over hashing-then-format because it lands in a real UUID
// version slot — Scalar / clients won't reject it as malformed.
func StableGroupUUID(s string) (openapi_types.UUID, error) {
	// Namespace is the v5 namespace for the v2 group resource
	// (arbitrary but fixed; chosen to avoid collisions with the
	// standard URL/DNS namespaces).
	ns, err := uuid.Parse("d3a8c0d2-1c51-4b6a-9b3a-ff7e2f5b2aa1")
	if err != nil {
		return openapi_types.UUID{}, err
	}
	return uuid.NewSHA1(ns, []byte(s)), nil
}

// ----- Shared field accessors -----------------------------------------

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		// session_number sometimes round-trips through JSON as a
		// string ("3") rather than a numeric. Tolerate it.
		var n int
		for _, r := range x {
			if r < '0' || r > '9' {
				return 0
			}
			n = n*10 + int(r-'0')
		}
		return n
	default:
		return 0
	}
}

// getTime returns the first non-zero time parsable from any of the
// supplied keys. Tolerates RFC3339 / RFC3339Nano / unix-millis / time.Time.
func getTime(m map[string]any, keys ...string) (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case time.Time:
			if !x.IsZero() {
				return x, true
			}
		case string:
			if x == "" {
				continue
			}
			// Trim whitespace/quoting we might see if a stamp got
			// re-marshalled through JSON.
			s := strings.TrimSpace(x)
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t, true
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t, true
			}
			// Legacy v1 emits naive-ISO timestamps without a timezone
			// designator, e.g. "2026-05-12T13:21:19.490". RFC3339 parsers
			// reject those — fall back to fractional and second-precision
			// ISO formats and treat them as UTC (which is what v1 stores).
			if t, err := time.Parse("2006-01-02T15:04:05.999999999", s); err == nil {
				return t.UTC(), true
			}
			if t, err := time.Parse("2006-01-02T15:04:05", s); err == nil {
				return t.UTC(), true
			}
		case int64:
			if x > 0 {
				return time.UnixMilli(x), true
			}
		case float64:
			if x > 0 {
				return time.UnixMilli(int64(x)), true
			}
		}
	}
	return time.Time{}, false
}
