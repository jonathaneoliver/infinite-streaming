package main

// v2 plays endpoints — see api/openapi/v2/forwarder.yaml § /api/v2/plays.
//
// Replaces v1's /api/sessions for the dashboard's session-picker use
// case. v1 grouped by (session_id, play_id) and accepted only since/until;
// v2 groups by play_id, accepts player_id / play_id / classification /
// from / to / limit filters, and wraps the result in the v2 envelope
// ({items, next_cursor}).
//
// Today the query reads session_snapshots + network_requests live, the
// same way v1 does. The aspirational play_summaries rollup table is
// blocked on `play.ended` SSE plumbing — when it lands, swap the FROM
// clause without changing the wire shape.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func mountV2PlaysHandlers(mux *http.ServeMux, cfg config) {
	// net/http's ServeMux treats trailing-slash and no-trailing-slash as
	// distinct patterns: exact `/api/v2/plays` matches only the list URL,
	// `/api/v2/plays/` is a prefix that catches `{play_id}` and anything
	// beneath it. Register both so a request to either lands in the
	// dispatcher below.
	mux.HandleFunc("/api/v2/plays", playsDispatcher(cfg))
	mux.HandleFunc("/api/v2/plays/", playsDispatcher(cfg))
}

func playsDispatcher(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v2/plays")
		switch {
		case rest == "" || rest == "/":
			v2PlaysListHandler(w, r, cfg)
		case strings.HasPrefix(rest, "/"):
			playID := strings.TrimPrefix(rest, "/")
			if strings.ContainsRune(playID, '/') {
				writeProblemv2(w, http.StatusNotFound, "not found", "no nested resources under /api/v2/plays/{play_id} yet")
				return
			}
			v2PlayDetailHandler(w, r, cfg, playID)
		default:
			writeProblemv2(w, http.StatusNotFound, "not found", "")
		}
	}
}

// v2PlaysListHandler answers GET /api/v2/plays.
func v2PlaysListHandler(w http.ResponseWriter, r *http.Request, cfg config) {
	q := r.URL.Query()
	clauses, params, err := buildPlaysFilter(
		q.Get("player_id"),
		q.Get("play_id"),
		q.Get("from"),
		q.Get("to"),
		q.Get("classification"),
	)
	if err != nil {
		writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
		return
	}
	limit := parseLimit(q.Get("limit"), 500, 5000)

	rows, err := queryPlaySummaries(r.Context(), cfg, clauses, params, limit)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "clickhouse query failed", err.Error())
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	writeJSONv2(w, http.StatusOK, map[string]any{
		"items":       rows,
		"next_cursor": nil,
	})
}

// v2PlayDetailHandler answers GET /api/v2/plays/{play_id}. Returns the
// same PlaySummary shape (no embedded events_summary / network_summary
// /_links yet — those are spec'd under PlayDetail for a later PR).
func v2PlayDetailHandler(w http.ResponseWriter, r *http.Request, cfg config, playID string) {
	if playID == "" {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "play_id required")
		return
	}
	clauses, params, err := buildPlaysFilter("", playID, "", "", "")
	if err != nil {
		writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
		return
	}
	rows, err := queryPlaySummaries(r.Context(), cfg, clauses, params, 1)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "clickhouse query failed", err.Error())
		return
	}
	if len(rows) == 0 {
		writeProblemv2(w, http.StatusNotFound, "not found", fmt.Sprintf("play %q has no archived snapshots", playID))
		return
	}
	writeJSONv2(w, http.StatusOK, rows[0])
}

// buildPlaysFilter shapes WHERE clauses + ClickHouse parameter values for
// the plays aggregation. Empty inputs are skipped. Returns an error only
// when a value is malformed in a way ClickHouse can't recover from
// (today: nothing — all values are passed through as parameters).
func buildPlaysFilter(playerID, playID, from, to, classification string) ([]string, map[string]string, error) {
	params := map[string]string{}
	// play_id != '' filters out pre-stamp legacy rows; the v1 endpoint
	// surfaces them as the literal "—" but v2 plays are defined to be
	// proper UUIDs so we drop them here.
	clauses := []string{"play_id != ''"}
	if playerID != "" {
		clauses = append(clauses, "lowerUTF8(player_id) = lowerUTF8({player:String})")
		params["player"] = playerID
	}
	if playID != "" {
		clauses = append(clauses, "lowerUTF8(play_id) = lowerUTF8({play:String})")
		params["play"] = playID
	}
	if from != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		params["from"] = from
	}
	if to != "" {
		clauses = append(clauses, "ts < parseDateTime64BestEffort({to:String})")
		params["to"] = to
	}
	if classification != "" {
		switch classification {
		case "interesting", "other", "favourite":
			clauses = append(clauses, "classification = {classification:String}")
			params["classification"] = classification
		default:
			return nil, nil, errors.New("classification must be one of: interesting, other, favourite")
		}
	}
	if playerID == "" && playID == "" && from == "" && to == "" {
		// Bound the scan when the caller didn't — the snapshots table
		// is partitioned by toYYYYMMDD(ts), so without a time bound
		// ClickHouse would read every partition.
		clauses = append(clauses, "ts >= now() - INTERVAL 24 HOUR")
	}
	return clauses, params, nil
}

// queryPlaySummaries runs the per-play aggregation. Reuses v1's SQL
// shape (lagInFrame window for ABR-shift counting, left-join against
// network_requests for the net_* counters) but groups by play_id only —
// session_id rides along as `any(session_id)` for the dashboard's
// stable row key.
func queryPlaySummaries(ctx context.Context, cfg config, clauses []string, params map[string]string, limit int) ([]map[string]any, error) {
	where := "WHERE " + strings.Join(clauses, " AND ")
	// network_requests doesn't carry a classification column, so the
	// net_counts CTE skips the classification clause if present. The
	// SQL templates handle this by only forwarding ts/player_id/play_id
	// clauses to net_counts.
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
		    session_id, play_id, ts,
		    player_id, group_id, content_id,
		    player_state, player_error, last_event,
		    stall_count, dropped_frames, frames_displayed,
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
		    max(dropped_frames) AS dropped_frames,
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
		    any(classification) AS classification
		  FROM base
		  GROUP BY play_id
		)
		SELECT
		  agg.play_id, agg.player_id,
		  agg.session_id, agg.group_id, agg.content_id,
		  toString(agg.started_at)   AS started_at,
		  toString(agg.last_seen_at) AS last_seen_at,
		  agg.metric_events, agg.stalls, agg.dropped_frames,
		  agg.last_state, agg.last_player_error,
		  agg.master_manifest_failures, agg.manifest_failures, agg.segment_failures,
		  agg.all_failures, agg.transport_failures, agg.active_timeouts, agg.idle_timeouts,
		  agg.bitrate_shifts, agg.downshifts, agg.upshifts, agg.resolution_changes,
		  agg.avg_quality_pct, agg.min_quality_pct,
		  agg.frames_displayed, agg.first_frame_s,
		  agg.user_marked_count, agg.frozen_count, agg.segment_stall_count,
		  agg.restart_count, agg.error_event_count,
		  agg.classification,
		  ifNull(net_counts.net_rows,   0) AS net_events,
		  ifNull(net_counts.net_errors, 0) AS net_errors,
		  ifNull(net_counts.net_faults, 0) AS net_faults
		FROM agg
		LEFT JOIN net_counts ON agg.play_id = net_counts.play_id
		ORDER BY agg.started_at DESC
		LIMIT %d
		FORMAT JSONEachRow`,
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, netWhere,
		limit,
	)
	return queryClickHouseRows(ctx, cfg, query, params)
}
