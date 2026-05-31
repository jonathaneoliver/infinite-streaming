package plays

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// FindPlays returns one row per play matching the filter, in the
// same shape /api/v2/plays renders today. Each row is a
// map[string]any so the JSON output stays byte-for-byte identical
// to the v1-era response shape clients depend on.
//
// The SQL is the same as the original queryPlaySummaries: snapshot
// aggregation in `agg`, network counts joined in, label histogram
// joined in via the labels_unioned/labels_per_play/labels_agg CTEs.
// Label filters apply AFTER the labels_agg join.
//
// The 24h auto-bound fires only when the caller supplied NO scope
// (no player_id, no play_id, no attempt_id, no from/to) — same
// behaviour the v1 handler had to keep the snapshots partition scan
// bounded.
func FindPlays(ctx context.Context, b Backend, f PlayFilter) ([]map[string]any, error) {
	clauses, params, err := buildPlaysFilter(f)
	if err != nil {
		return nil, err
	}

	// Post-join label filter — applied against the per-play
	// labels_distinct array assembled in labels_agg. Same tristate
	// semantics as the per-row filter on the events/network tables.
	var post []string
	post, params = f.Labels.applyTo(post, params, "labels_agg.labels_distinct")

	limit := f.Limit
	if limit <= 0 {
		limit = defaultPlaysLimit
	}
	if limit > maxPlaysLimit {
		limit = maxPlaysLimit
	}

	return runPlaysQuery(ctx, b, clauses, params, post, limit)
}

// GetPlaySummary returns the single PlaySummary for play_id, or nil
// (no error) when no archived rows exist for that play. Same
// shape as a single row from FindPlays.
func GetPlaySummary(ctx context.Context, b Backend, playID string) (map[string]any, error) {
	if playID == "" {
		return nil, errors.New("play_id required")
	}
	rows, err := FindPlays(ctx, b, PlayFilter{PlayID: playID, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// buildPlaysFilter shapes the base WHERE clauses + CH parameter map.
// Same logic as the v1 handler's buildPlaysFilter helper, lifted as-is.
func buildPlaysFilter(f PlayFilter) ([]string, map[string]string, error) {
	params := map[string]string{}
	// play_id != '' drops pre-stamp legacy rows. v1 surfaced them as
	// the literal "—" but v2 plays are defined to be proper UUIDs.
	clauses := []string{"play_id != ''"}
	if f.PlayerID != "" {
		clauses = append(clauses, "lowerUTF8(player_id) = lowerUTF8({player:String})")
		params["player"] = f.PlayerID
	}
	if f.PlayID != "" {
		clauses = append(clauses, "lowerUTF8(play_id) = lowerUTF8({play:String})")
		params["play"] = f.PlayID
	}
	if f.AttemptID != "" {
		clauses = append(clauses, "attempt_id = {attempt:UInt32}")
		params["attempt"] = f.AttemptID
	}
	if f.From != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		params["from"] = f.From
	}
	if f.To != "" {
		clauses = append(clauses, "ts < parseDateTime64BestEffort({to:String})")
		params["to"] = f.To
	}
	if f.Classification != "" {
		switch f.Classification {
		case "interesting", "other", "favourite":
			clauses = append(clauses, "classification = {classification:String}")
			params["classification"] = f.Classification
		default:
			return nil, nil, errors.New("classification must be one of: interesting, other, favourite")
		}
	}
	if f.PlayerID == "" && f.PlayID == "" && f.AttemptID == "" && f.From == "" && f.To == "" {
		// Bound the scan when caller didn't — the events table is
		// partitioned by toYYYYMMDD(ts).
		clauses = append(clauses, "ts >= now() - INTERVAL 24 HOUR")
	}
	return clauses, params, nil
}

// runPlaysQuery is the SQL template + execution. Kept private — the
// shape is awkward (3 clause/param threadings; postClauses for label
// filter pushdown) and there's no caller outside this file.
func runPlaysQuery(ctx context.Context, b Backend, clauses []string, params map[string]string, postClauses []string, limit int) ([]map[string]any, error) {
	where := "WHERE " + strings.Join(clauses, " AND ")
	// network_requests has no classification column; rebuild a
	// classification-free WHERE for the net_counts CTE.
	netClauses := []string{"play_id != ''"}
	for _, c := range clauses {
		if strings.Contains(c, "classification") {
			continue
		}
		if c == "play_id != ''" {
			continue
		}
		netClauses = append(netClauses, c)
	}
	netWhere := "WHERE " + strings.Join(netClauses, " AND ")

	query := fmt.Sprintf(`
		WITH base AS (
		  SELECT
		    session_id, play_id, attempt_id, ts,
		    player_id, group_id, content_id,
		    player_state, player_error, last_event,
		    stall_count, frames_dropped, frames_displayed,
		    video_bitrate_mbps, video_resolution, video_quality_pct,
		    video_first_frame_time_s,
		    master_manifest_consecutive_failures,
		    manifest_consecutive_failures,
		    segment_consecutive_failures,
		    all_consecutive_failures,
		    transport_consecutive_failures,
		    fault_count_transfer_active_timeout,
		    fault_count_transfer_idle_timeout,
		    classification,
		    -- #550 Phase 1+2+4 fields for the aggregation below.
		    playing_time_ms, buffering_time_ms, stalling_time_ms,
		    error_count, error_code, error_domain,
		    terminal_error_code, terminal_error_domain,
		    playback_status, playback_reason,
		    device_class, device_model, player_tech,
		    app_version, os_version_major, os_version_minor,
		    lagInFrame(video_bitrate_mbps, 1, video_bitrate_mbps) OVER w AS prev_bitrate,
		    lagInFrame(video_resolution,   1, video_resolution)   OVER w AS prev_resolution
		  FROM %s.%s
		  %s
		  WINDOW w AS (PARTITION BY play_id ORDER BY ts)
		),
		net_counts AS (
		  SELECT play_id,
		         count() AS net_rows,
		         countIf(status >= 400) AS net_errors,
		         countIf(faulted = 1)  AS net_faults
		  FROM %s.network_requests
		  %s
		  GROUP BY play_id
		),
		-- Per-play label histogram across all three source tables.
		-- lowerUTF8 on play_id makes the JOIN survive the case-
		-- sensitivity gap where some legacy rows landed uppercase.
		labels_unioned AS (
		  SELECT lowerUTF8(play_id) AS play_id, arrayJoin(labels) AS label
		  FROM %s.%s
		  %s
		  UNION ALL
		  SELECT lowerUTF8(play_id) AS play_id, arrayJoin(labels) AS label
		  FROM %s.network_requests
		  %s
		  UNION ALL
		  SELECT lowerUTF8(play_id) AS play_id, arrayJoin(labels) AS label
		  FROM %s.control_events
		  %s
		),
		labels_per_play AS (
		  SELECT play_id, label, count() AS n
		  FROM labels_unioned
		  GROUP BY play_id, label
		),
		labels_agg AS (
		  SELECT play_id,
		         sum(n) AS labels_total,
		         arrayDistinct(groupArray(label)) AS labels_distinct,
		         groupArray((label, n)) AS label_pairs
		  FROM labels_per_play
		  GROUP BY play_id
		),
		agg AS (
		  SELECT
		    play_id,
		    any(session_id) AS session_id,
		    any(player_id) AS player_id,
		    any(group_id) AS group_id,
		    any(content_id) AS content_id,
		    min(ts) AS started_at,
		    max(ts) AS last_seen_at,
		    count() AS metric_events,
		    max(stall_count) AS stalls,
		    max(frames_dropped) AS frames_dropped,
		    argMax(player_state, ts) AS last_state,
		    argMax(player_error, ts) AS last_player_error,
		    max(master_manifest_consecutive_failures) AS master_manifest_failures,
		    max(manifest_consecutive_failures) AS manifest_failures,
		    max(segment_consecutive_failures) AS segment_failures,
		    max(all_consecutive_failures) AS all_failures,
		    max(transport_consecutive_failures) AS transport_failures,
		    max(fault_count_transfer_active_timeout) AS active_timeouts,
		    max(fault_count_transfer_idle_timeout) AS idle_timeouts,
		    countIf(video_bitrate_mbps != prev_bitrate AND video_bitrate_mbps > 0 AND prev_bitrate > 0) AS bitrate_shifts,
		    countIf(video_bitrate_mbps < prev_bitrate AND prev_bitrate > 0 AND video_bitrate_mbps > 0) AS downshifts,
		    countIf(video_bitrate_mbps > prev_bitrate AND prev_bitrate > 0 AND video_bitrate_mbps > 0) AS upshifts,
		    countIf(video_resolution != prev_resolution AND video_resolution != '' AND prev_resolution != '') AS resolution_changes,
		    round(avgIf(video_quality_pct, video_quality_pct > 0), 1) AS avg_quality_pct,
		    round(minIf(video_quality_pct, video_quality_pct > 0), 1) AS min_quality_pct,
		    max(frames_displayed) AS frames_displayed,
		    round(max(video_first_frame_time_s), 2) AS first_frame_s,
		    countIf(last_event = 'user_marked')   AS user_marked_count,
		    countIf(last_event = 'frozen')        AS frozen_count,
		    countIf(last_event = 'segment_stall') AS segment_stall_count,
		    countIf(last_event = 'restart')       AS restart_count,
		    countIf(last_event = 'error')         AS error_event_count,
		    any(classification) AS classification,
		    maxIf(attempt_id, attempt_id > 0)       AS attempt_id_max,
		    -- #550 Phase 1: residency totals (max because cumulative)
		    max(playing_time_ms) AS playing_time_ms,
		    max(buffering_time_ms) AS buffering_time_ms,
		    max(stalling_time_ms) AS stalling_time_ms,
		    -- #550 Phase 2: outcome (argMax on terminal row; in_progress
		    -- mid-play rows return last value seen, which is what we
		    -- want for live sessions).
		    argMax(playback_status, ts) AS playback_status,
		    argMax(playback_reason, ts) AS playback_reason,
		    argMax(terminal_error_code, ts)   AS terminal_error_code,
		    argMax(terminal_error_domain, ts) AS terminal_error_domain,
		    argMax(error_code, ts)   AS last_error_code,
		    argMax(error_domain, ts) AS last_error_domain,
		    max(error_count) AS error_count,
		    -- #550 Phase 4: device taxonomy — stable per session;
		    -- argMax picks the most recent stamp (which equals every
		    -- stamp in practice).
		    argMax(device_class, ts) AS device_class,
		    argMax(device_model, ts) AS device_model,
		    argMax(player_tech, ts)  AS player_tech,
		    argMax(app_version, ts)  AS app_version,
		    argMax(os_version_major, ts) AS os_version_major,
		    argMax(os_version_minor, ts) AS os_version_minor
		  FROM base
		  GROUP BY play_id
		)
		SELECT
		  agg.play_id   AS play_id,
		  agg.player_id AS player_id,
		  agg.attempt_id_max AS attempt_id,
		  agg.attempt_id_max AS attempt_count,
		  agg.session_id, agg.group_id, agg.content_id,
		  toString(agg.started_at)   AS started_at,
		  toString(agg.last_seen_at) AS last_seen_at,
		  agg.metric_events, agg.stalls, agg.frames_dropped,
		  agg.last_state, agg.last_player_error,
		  agg.master_manifest_failures, agg.manifest_failures, agg.segment_failures,
		  agg.all_failures, agg.transport_failures, agg.active_timeouts, agg.idle_timeouts,
		  agg.bitrate_shifts, agg.downshifts, agg.upshifts, agg.resolution_changes,
		  agg.avg_quality_pct, agg.min_quality_pct,
		  agg.frames_displayed, agg.first_frame_s,
		  agg.user_marked_count, agg.frozen_count, agg.segment_stall_count,
		  agg.restart_count, agg.error_event_count,
		  -- #550 Phase 1+2+4 fields propagated to PlaySummary.
		  agg.playing_time_ms, agg.buffering_time_ms, agg.stalling_time_ms,
		  agg.playback_status, agg.playback_reason,
		  agg.terminal_error_code, agg.terminal_error_domain,
		  agg.last_error_code, agg.last_error_domain, agg.error_count,
		  agg.device_class, agg.device_model, agg.player_tech,
		  agg.app_version, agg.os_version_major, agg.os_version_minor,
		  agg.classification,
		  ifNull(net_counts.net_rows,   0) AS net_events,
		  ifNull(net_counts.net_errors, 0) AS net_errors,
		  ifNull(net_counts.net_faults, 0) AS net_faults,
		  ifNull(labels_agg.labels_total,           0)  AS labels_total,
		  ifNull(length(labels_agg.labels_distinct), 0) AS labels_distinct_count,
		  ifNull(labels_agg.label_pairs,           []) AS label_histogram
		FROM agg
		LEFT JOIN net_counts ON agg.play_id = net_counts.play_id
		LEFT JOIN labels_agg ON lowerUTF8(agg.play_id) = labels_agg.play_id
		%s
		ORDER BY agg.started_at DESC
		LIMIT %d
		FORMAT JSONEachRow`,
		b.Database, b.EventsTable, where,
		b.Database, netWhere,
		b.Database, b.EventsTable, where,
		b.Database, netWhere,
		b.Database, netWhere,
		postWhere(postClauses),
		limit,
	)
	return b.queryRows(ctx, query, params)
}

func postWhere(post []string) string {
	if len(post) == 0 {
		return ""
	}
	return "WHERE " + strings.Join(post, " AND ")
}
