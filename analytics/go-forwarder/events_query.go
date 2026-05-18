// events_query.go — read-side facade for session events.
//
// Issue #469: events are now classified at ingest by the eventclass
// package (analytics/go-forwarder/eventclass) and written into
// infinite_streaming.session_events. This file is a thin SELECT over
// that table. The legacy 22-branch UNION-ALL CTE that derived events
// at read time has been retired; only the public function signatures
// (runEventsQuery / buildEventsQuery / eventsQueryParams) are kept so
// callers — emitBackfillEvents, startEventsPoller,
// /api/v2/session_events, /api/session_events — see no shape change.
//
// Old plays that landed before this rewrite have no rows in
// session_events; they show empty event lists in the dashboard. Per
// the cutover plan (option B in the design discussion) no backfill
// runs — those plays age out via the table's 30/90-day TTL.
package main

import (
	"context"
	"fmt"
	"strings"
)

// eventsQueryParams identifies which rows to return. Either playerID
// OR sessionID (or both) must be set; the optional playID narrows
// further. from/to bound the timeseries window (empty = unbounded on
// that side, with the limit acting as the safety cap).
type eventsQueryParams struct {
	PlayerID  string
	SessionID string
	PlayID    string
	From      string // ISO-8601 lower bound (inclusive), optional
	To        string // ISO-8601 upper bound (inclusive), optional
	Limit     int    // server-side cap (1..50000); 0 → 5000 default
}

// runEventsQuery executes the session_events SELECT and returns the
// rows. Each row has at least {ts, type, info, kind, priority,
// play_id, player_id, session_id, attempt_id} — same projection
// callers consumed from the legacy SQL plus the new attempt_id field
// that the write-time path can now plumb through without a temporal
// join.
func runEventsQuery(ctx context.Context, cfg config, p eventsQueryParams) ([]map[string]any, error) {
	query, args, err := buildEventsQuery(cfg, p)
	if err != nil {
		return nil, err
	}
	return queryClickHouseRows(ctx, cfg, query, args)
}

// buildEventsQuery assembles the SELECT + bound parameter map.
// Separated from runEventsQuery so the legacy /api/session_events
// handler (which streams the result body via proxyClickHouseJSON)
// uses the same query string.
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
	if p.PlayID != "" {
		if p.PlayID == "—" {
			clauses = append(clauses, "play_id = ''")
		} else {
			clauses = append(clauses, "lowerUTF8(play_id) = lowerUTF8({play:String})")
			args["play"] = p.PlayID
		}
	}
	if p.From != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		args["from"] = p.From
	}
	if p.To != "" {
		clauses = append(clauses, "ts <= parseDateTime64BestEffort({to:String})")
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
	// `toString(ts) AS ts` keeps the wire shape identical to the
	// legacy CTE output (callers consume ts as a string). The outer
	// subquery wrap is the same alias-shadowing dodge used in
	// buildSamplesQuery / buildNetworkQuery — without it the WHERE
	// would compare String to DateTime64 inside CH 24.x and bail
	// with ILLEGAL_TYPE_OF_ARGUMENT.
	query := fmt.Sprintf(
		`SELECT toString(ts) AS ts, player_id, play_id, attempt_id, session_id,
		        type, info, kind, priority
		 FROM (
		   SELECT * FROM %s.session_events
		   WHERE %s
		   ORDER BY ts DESC
		   LIMIT %d
		 )
		 FORMAT JSONEachRow`,
		cfg.chDatabase, where, limit)
	return query, args, nil
}
