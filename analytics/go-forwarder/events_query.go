// events_query.go — single source of truth for the session-events
// taxonomy SQL. Both the legacy `/api/v2/session_events` endpoint
// (used by the v3 dashboard for backfill) and the v2 timeseries
// `streams=events` path call into this file so the kind/priority
// classification is computed in exactly one place.
//
// Extracted from the inline literal in main.go's sessionEventsHandler;
// behaviour is unchanged for that endpoint. New: timeseries.go's
// emitBackfillEvents + pollEventsLive consume the same query so live
// SSE consumers see the same taxonomy as the dashboard's archive
// queries.
package main

import (
	"context"
	"fmt"
	"strings"
)

// eventsQueryParams identifies which rows the taxonomy SQL should
// classify. Either playerID OR sessionID (or both) must be set; the
// optional playID narrows further. from/to bound the timeseries
// window (empty = unbounded on that side, with the limit acting as
// the safety cap).
type eventsQueryParams struct {
	PlayerID  string
	SessionID string
	PlayID    string
	From      string // ISO-8601 lower bound (inclusive), optional
	To        string // ISO-8601 upper bound (inclusive), optional
	Limit     int    // server-side cap (1..50000); 0 → 5000 default
}

// runEventsQuery executes the events taxonomy SQL and returns the
// rows. Each row has at least {ts, type, info, kind, priority,
// play_id, player_id, session_id}.
func runEventsQuery(ctx context.Context, cfg config, p eventsQueryParams) ([]map[string]any, error) {
	query, args, err := buildEventsQuery(cfg, p)
	if err != nil {
		return nil, err
	}
	return queryClickHouseRows(ctx, cfg, query, args)
}

// buildEventsQuery assembles the full ClickHouse query + the bound
// parameter map. Separated from runEventsQuery so the legacy
// session_events handler (which streams the result body directly via
// proxyClickHouseJSON) can use the same query string.
func buildEventsQuery(cfg config, p eventsQueryParams) (string, map[string]string, error) {
	if p.PlayerID == "" && p.SessionID == "" {
		return "", nil, errBadParam("events query requires player_id or session_id")
	}
	args := map[string]string{}
	clauses := []string{}
	if p.SessionID != "" {
		clauses = append(clauses, "session_id = {session:String}")
		args["session"] = p.SessionID
	}
	if p.PlayerID != "" {
		// Case-insensitive — device-reported player_ids and v2's
		// normalised lowercase form often disagree.
		clauses = append(clauses, "lowerUTF8(player_id) = lowerUTF8({player:String})")
		args["player"] = p.PlayerID
	}
	// network_requests on iOS strips play_id from variant/segment
	// URLs, so HAR is filtered by time range instead.
	harClauses := append([]string{}, clauses...)
	if p.PlayID != "" {
		var pidPred string
		if p.PlayID == "—" {
			pidPred = "play_id = ''"
		} else {
			pidPred = "lowerUTF8(play_id) = lowerUTF8({play:String})"
			args["play"] = p.PlayID
		}
		clauses = append(clauses, pidPred)
		idWhere := []string{}
		if p.SessionID != "" {
			idWhere = append(idWhere, "session_id = {session:String}")
		}
		if p.PlayerID != "" {
			idWhere = append(idWhere, "lowerUTF8(player_id) = lowerUTF8({player:String})")
		}
		idClause := strings.Join(idWhere, " AND ")
		harClauses = append(harClauses, fmt.Sprintf(
			"nr.ts BETWEEN (SELECT min(ts) FROM %s.%s WHERE %s AND %s) "+
				"AND (SELECT max(ts) FROM %s.%s WHERE %s AND %s)",
			cfg.chDatabase, cfg.chTable, idClause, pidPred,
			cfg.chDatabase, cfg.chTable, idClause, pidPred))
	}
	// from/to apply to both the snapshot-based CTEs and the HAR
	// sub-selects. Timeseries live polling sets from=<highWaterTs>
	// each tick so only new events come back.
	if p.From != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		harClauses = append(harClauses, "nr.ts >= parseDateTime64BestEffort({from:String})")
		args["from"] = p.From
	}
	if p.To != "" {
		clauses = append(clauses, "ts <= parseDateTime64BestEffort({to:String})")
		harClauses = append(harClauses, "nr.ts <= parseDateTime64BestEffort({to:String})")
		args["to"] = p.To
	}

	limit := p.Limit
	if limit <= 0 {
		limit = 5000
	}
	if limit > 50000 {
		limit = 50000
	}

	where := strings.Join(clauses, " AND ")
	harWhere := strings.Join(harClauses, " AND ")
	query := fmt.Sprintf(eventsSQLTemplate,
		// stall_or_buffer_pairs / rate_shifts / base CTEs
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, cfg.chTable, where,
		// frozen/segment_stall, restart, playback_start, timejump, error
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, cfg.chTable, where,
		// user_marked + catch-all (2)
		cfg.chDatabase, cfg.chTable, where,
		cfg.chDatabase, cfg.chTable, where,
		// HAR events: http_5xx, http_4xx, faulted, slow_request,
		// slow_segment, request_retry
		cfg.chDatabase, harWhere,
		cfg.chDatabase, harWhere,
		cfg.chDatabase, harWhere,
		cfg.chDatabase, harWhere,
		cfg.chDatabase, harWhere,
		cfg.chDatabase, harWhere,
		limit)
	return query, args, nil
}

// eventsSQLTemplate is the taxonomy multi-CTE query, parameterised
// with %s placeholders for chDatabase / chTable / where / harWhere
// and a final %d for LIMIT. Format-args order is fixed in
// buildEventsQuery above — DO NOT reorder placeholders here without
// updating that call.
const eventsSQLTemplate = `
WITH stall_or_buffer_pairs AS (
  SELECT start_ts, start_event, duration_s
  FROM (
    SELECT
      ts AS start_ts,
      last_event AS start_event,
      multiIf(
        last_event IN ('stall_start','stall_end'),         'stall',
        last_event IN ('buffering_start','buffering_end'), 'buffer',
        ''
      ) AS family,
      leadInFrame(last_event, 1, '') OVER w AS next_event,
      dateDiff('millisecond', ts, leadInFrame(ts, 1, ts) OVER w) / 1000.0 AS duration_s
    FROM %s.%s
    WHERE %s
      AND last_event IN ('stall_start','stall_end','buffering_start','buffering_end')
    WINDOW w AS (PARTITION BY family ORDER BY ts
                 ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)
  )
  WHERE (start_event = 'stall_start'     AND next_event = 'stall_end')
     OR (start_event = 'buffering_start' AND next_event = 'buffering_end')
),
rate_shifts AS (
  SELECT ts, last_event, video_bitrate_mbps,
         lagInFrame(video_bitrate_mbps, 1, video_bitrate_mbps) OVER w AS prev_bitrate
  FROM %s.%s
  WHERE %s
  WINDOW w AS (ORDER BY ts)
),
base AS (
  SELECT
    ts,
    player_error, transport_fault_active,
    manifest_consecutive_failures,
    segment_consecutive_failures,
    master_manifest_consecutive_failures,
    all_consecutive_failures,
    transport_consecutive_failures,
    fault_count_transfer_active_timeout,
    fault_count_transfer_idle_timeout,
    loop_count_server,
    row_number() OVER w AS rn,
    lagInFrame(player_error, 1, '')                       OVER w AS prev_error,
    lagInFrame(transport_fault_active, 1, 0)              OVER w AS prev_fault,
    lagInFrame(manifest_consecutive_failures, 1, 0)       OVER w AS prev_manifest_fail,
    lagInFrame(segment_consecutive_failures, 1, 0)        OVER w AS prev_segment_fail,
    lagInFrame(master_manifest_consecutive_failures, 1, 0) OVER w AS prev_master_fail,
    lagInFrame(all_consecutive_failures, 1, 0)            OVER w AS prev_all_fail,
    lagInFrame(transport_consecutive_failures, 1, 0)      OVER w AS prev_transport_fail,
    lagInFrame(fault_count_transfer_active_timeout, 1, 0) OVER w AS prev_active_to,
    lagInFrame(fault_count_transfer_idle_timeout, 1, 0)   OVER w AS prev_idle_to,
    lagInFrame(loop_count_server, 1, 0)                   OVER w AS prev_loop_server
  FROM %s.%s
  WHERE %s
  WINDOW w AS (PARTITION BY play_id ORDER BY ts)
)
-- Outer SELECT re-aliases event_ts back to ts so callers (HTTP JSON,
-- the SSE event frame, the seen-set fingerprinter) keep seeing a
-- field named "ts" with no change. The inner UNION ALL branches
-- project event_ts (not ts) so that CH 24.x doesn't shadow the
-- DateTime64 column ts with the toString'd String alias inside the
-- per-branch WHERE — same ILLEGAL_TYPE_OF_ARGUMENT trap the comment
-- in buildSamplesQuery warns about.
SELECT
  event_ts AS ts, type, info,
  multiIf(
    type IN ('master_manifest_failure', 'all_failure',
             'manifest_failure', 'segment_failure',
             'transport_failure',
             'transfer_active_timeout', 'transfer_idle_timeout',
             'fault_on', 'fault_off',
             'http_5xx', 'http_4xx',
             'request_timeout', 'request_incomplete', 'request_faulted',
             'slow_request', 'slow_segment', 'request_retry',
             'loop_server'), 'cause',
    'effect'
  ) AS kind,
  multiIf(
    type = 'user_marked', 1,
    type = 'error', 1,
    type IN ('master_manifest_failure', 'all_failure'), 1,
    type = 'stall' AND duration_s >= 3, 1,
    type = 'stall', 2,
    type = 'restart', 2,
    type IN ('manifest_failure', 'segment_failure',
             'transport_failure',
             'transfer_active_timeout', 'transfer_idle_timeout',
             'fault_on', 'fault_off'), 3,
    type IN ('downshift', 'timejump', 'buffering'), 3,
    type IN ('http_5xx', 'request_timeout'), 2,
    type IN ('http_4xx', 'request_incomplete', 'request_faulted',
             'slow_request', 'slow_segment'), 3,
    type = 'request_retry', 4,
    type IN ('upshift', 'playback_start'), 4,
    type = 'loop_server', 4,
    3
  ) AS priority
FROM (
  SELECT toString(start_ts) AS event_ts, 'stall' AS type,
         concat(toString(round(duration_s, 2)), 's') AS info,
         duration_s
  FROM stall_or_buffer_pairs
  WHERE start_event = 'stall_start' AND duration_s > 0
  UNION ALL
  SELECT toString(start_ts) AS event_ts, 'buffering' AS type,
         concat(toString(round(duration_s, 2)), 's') AS info,
         duration_s
  FROM stall_or_buffer_pairs
  WHERE start_event = 'buffering_start' AND duration_s > 0
  UNION ALL
  SELECT toString(ts) AS event_ts, 'stall' AS type,
         if(last_event = 'frozen', '(frozen)', '(segment)') AS info,
         0 AS duration_s
  FROM %s.%s WHERE %s AND last_event IN ('frozen', 'segment_stall')
  UNION ALL
  SELECT toString(ts) AS event_ts, 'restart' AS type, '' AS info, 0 AS duration_s
  FROM %s.%s WHERE %s AND last_event = 'restart'
  UNION ALL
  SELECT toString(ts) AS event_ts, 'playback_start' AS type, '' AS info, 0
  FROM %s.%s WHERE %s AND last_event IN ('video_start_time', 'video_first_frame')
  UNION ALL
  SELECT toString(ts) AS event_ts, 'downshift' AS type,
         concat(toString(round(prev_bitrate, 2)), '→', toString(round(video_bitrate_mbps, 2)), ' Mbps') AS info,
         0
  FROM rate_shifts WHERE last_event = 'rate_shift_down' AND prev_bitrate > 0 AND video_bitrate_mbps > 0
  UNION ALL
  SELECT toString(ts) AS event_ts, 'upshift' AS type,
         concat(toString(round(prev_bitrate, 2)), '→', toString(round(video_bitrate_mbps, 2)), ' Mbps') AS info,
         0
  FROM rate_shifts WHERE last_event = 'rate_shift_up' AND prev_bitrate > 0 AND video_bitrate_mbps > 0
  UNION ALL
  SELECT toString(ts) AS event_ts, 'timejump' AS type, '' AS info, 0
  FROM %s.%s WHERE %s AND last_event = 'timejump'
  UNION ALL
  SELECT toString(ts) AS event_ts, 'error' AS type, player_error AS info, 0
  FROM %s.%s WHERE %s AND last_event = 'error'
  UNION ALL
  SELECT toString(ts) AS event_ts, 'user_marked' AS type, '' AS info, 0
  FROM %s.%s WHERE %s AND last_event = 'user_marked'
  UNION ALL
  SELECT toString(ts) AS event_ts, last_event AS type, '' AS info, 0
  FROM %s.%s WHERE %s
    AND last_event != ''
    AND last_event NOT IN (
      'heartbeat', 'state_change', 'playing', 'video_bitrate_change',
      'stall_start', 'stall_end',
      'buffering_start', 'buffering_end',
      'frozen', 'segment_stall',
      'restart', 'video_first_frame', 'video_start_time',
      'rate_shift_down', 'rate_shift_up',
      'timejump', 'error', 'user_marked'
    )
  UNION ALL
  SELECT toString(ts) AS event_ts, 'error' AS type, player_error AS info, 0
  FROM base WHERE rn > 1 AND player_error != '' AND prev_error != player_error
  UNION ALL
  SELECT toString(ts) AS event_ts, 'master_manifest_failure' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND master_manifest_consecutive_failures > prev_master_fail
  UNION ALL
  SELECT toString(ts) AS event_ts, 'all_failure' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND all_consecutive_failures > prev_all_fail
  UNION ALL
  SELECT toString(ts) AS event_ts, 'manifest_failure' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND manifest_consecutive_failures > prev_manifest_fail
  UNION ALL
  SELECT toString(ts) AS event_ts, 'segment_failure' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND segment_consecutive_failures > prev_segment_fail
  UNION ALL
  SELECT toString(ts) AS event_ts, 'transport_failure' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND transport_consecutive_failures > prev_transport_fail
  UNION ALL
  SELECT toString(ts) AS event_ts, 'transfer_active_timeout' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND fault_count_transfer_active_timeout > prev_active_to
  UNION ALL
  SELECT toString(ts) AS event_ts, 'transfer_idle_timeout' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND fault_count_transfer_idle_timeout > prev_idle_to
  UNION ALL
  SELECT toString(ts) AS event_ts, 'fault_on' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND transport_fault_active = 1 AND prev_fault = 0
  UNION ALL
  SELECT toString(ts) AS event_ts, 'fault_off' AS type, '' AS info, 0
  FROM base WHERE rn > 1 AND transport_fault_active = 0 AND prev_fault = 1
  UNION ALL
  SELECT toString(ts) AS event_ts, 'loop_server' AS type,
         concat('loop ', toString(loop_count_server)) AS info, 0
  FROM base WHERE rn > 1 AND loop_count_server > prev_loop_server
  UNION ALL
  SELECT toString(nr.ts) AS event_ts, 'http_5xx' AS type,
         concat(toString(status), ' ', method, ' ', path) AS info, 0 AS duration_s
  FROM %s.network_requests AS nr WHERE %s AND status >= 500
  UNION ALL
  SELECT toString(nr.ts) AS event_ts, 'http_4xx' AS type,
         concat(toString(status), ' ', method, ' ', path) AS info, 0
  FROM %s.network_requests AS nr WHERE %s AND status >= 400 AND status < 500
  UNION ALL
  SELECT toString(nr.ts) AS event_ts,
         multiIf(
           positionCaseInsensitive(fault_type, 'timeout') > 0, 'request_timeout',
           positionCaseInsensitive(fault_type, 'corrupt') > 0
             OR positionCaseInsensitive(fault_type, 'partial') > 0
             OR positionCaseInsensitive(fault_type, 'abandon') > 0
             OR (status >= 200 AND status < 300), 'request_incomplete',
           'request_faulted'
         ) AS type,
         concat(fault_type, ' ', method, ' ', path) AS info, 0
  FROM %s.network_requests AS nr WHERE %s AND faulted = 1
  UNION ALL
  SELECT toString(nr.ts) AS event_ts, 'slow_request' AS type,
         concat(toString(round(client_wait_ms, 0)), 'ms ', method, ' ', path) AS info, 0
  FROM %s.network_requests AS nr
  WHERE %s AND client_wait_ms > 2000 AND status < 400 AND faulted = 0
  UNION ALL
  SELECT toString(nr.ts) AS event_ts, 'slow_segment' AS type,
         concat(toString(round(transfer_ms, 0)), 'ms ', method, ' ', path) AS info, 0
  FROM %s.network_requests AS nr
  WHERE %s AND transfer_ms > 6000
    AND match(path, '\.(m4s|ts|mp4|m4a|m4v|aac|webm|mp3)($|\?)')
    AND status < 400 AND faulted = 0
  UNION ALL
  SELECT toString(retry_ts) AS event_ts, 'request_retry' AS type,
         concat(method, ' ', path) AS info, 0
  FROM (
    SELECT nr.ts AS retry_ts, method, url, path,
           lagInFrame(nr.ts, 1, nr.ts) OVER (PARTITION BY url ORDER BY nr.ts ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING) AS prev_url_ts
    FROM %s.network_requests AS nr
    WHERE %s
  )
  WHERE prev_url_ts != retry_ts
    AND dateDiff('millisecond', prev_url_ts, retry_ts) BETWEEN 1 AND 4000
)
ORDER BY event_ts DESC
LIMIT %d
FORMAT JSONEachRow`
