package main

// v2 forwarder endpoints — see api/openapi/v2/forwarder.yaml.
//
// These wrap the same ClickHouse rows the legacy /analytics/api/*
// endpoints serve, but project them through the shared v2 translator
// (go-proxy/pkg/v2translate) so the wire shape matches the live SSE
// pipeline. That means the dashboard's PlayersStore can absorb archive
// rows through the same _absorbFromRemote path it uses for live SSE
// frames — see content/shared/v2-archive-models.js.
//
// Scope (Phase 3b MVP):
//   GET /api/v2/healthz          — liveness
//   GET /api/v2/info             — version, retention
//   GET /api/v2/snapshots        — SnapshotRow[] (PlayerRecord + {ts, revision, play_id})
//   GET /api/v2/network_requests — NetworkRequestRow[] (NetworkLogEntry + {ts, player_id})
//
// Out of scope here:
//   /api/v2/plays + /api/v2/plays/aggregate (Level 3a follow-up)
//   /api/v2/session_events + /api/v2/session_heatmap (no schema change needed; thin wrappers)
//   /api/v2/plays/{id}/bundle (delegates to existing bundle.go)
//   Cursor pagination (current impl is offset-style; cursor is a follow-up)
//   label.* filter pushdown (the v1 endpoints ignore labels too)

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/v2translate"
)

func mountV2Handlers(mux *http.ServeMux, cfg config) {
	mux.HandleFunc("/api/v2/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok"))
	})

	mux.HandleFunc("/api/v2/info", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONv2(w, http.StatusOK, map[string]any{
			"version":                "forwarder-v2",
			"api_versions":           []string{"v2"},
			"clickhouse_target":      cfg.clickhouseURL,
			"raw_retention_days":     30,
			"summary_retention_days": 365,
		})
	})

	mux.HandleFunc("/api/v2/snapshots", func(w http.ResponseWriter, r *http.Request) {
		v2SnapshotsHandler(w, r, cfg)
	})

	mux.HandleFunc("/api/v2/network_requests", func(w http.ResponseWriter, r *http.Request) {
		v2NetworkRequestsHandler(w, r, cfg)
	})
}

// v2SnapshotsHandler queries session_snapshots, parses session_json
// back into the v1 map[string]any shape the proxy emitted, and runs it
// through v2translate.PlayerFromSession to land on the same
// PlayerRecord shape live SSE delivers. The row's ts/revision/play_id
// columns are added on top to satisfy SnapshotRow.
//
// Pragmatic extensions over the spec for session-viewer's snapshot
// replay (the page consumes very large windows progressively):
//   - `session_id` filter — accepts the legacy v1 numeric id alongside
//     `player_id`, so session-viewer's existing URL minting (which
//     passes `?session=N`) keeps working without rewriting the picker.
//   - `stride_ms` downsampling — same semantics as the legacy endpoint.
//   - `order=asc|desc`.
//   - `include=raw` — passthrough of the original `session_json` blob
//     so the existing renderer (which JSON.parses session_json to read
//     v1 flat fields) keeps working unchanged. Without this flag the
//     row is the typed PlayerRecord projection only.
//   - `Accept: application/x-ndjson` — streams JSONEachRow lines
//     directly so a 200K-snapshot window doesn't buffer in the
//     forwarder before painting starts.
func v2SnapshotsHandler(w http.ResponseWriter, r *http.Request, cfg config) {
	playerID := r.URL.Query().Get("player_id")
	sessionID := r.URL.Query().Get("session_id")
	playID := r.URL.Query().Get("play_id")
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	limit := parseLimit(r.URL.Query().Get("limit"), 500, 200000)
	includeRaw := r.URL.Query().Get("include") == "raw"
	stream := strings.Contains(r.Header.Get("Accept"), "application/x-ndjson")
	order := "ASC"
	if strings.EqualFold(r.URL.Query().Get("order"), "desc") {
		order = "DESC"
	}
	strideMs := 0
	if v := r.URL.Query().Get("stride_ms"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 60000 {
			strideMs = n
		}
	}

	params := map[string]string{}
	clauses := []string{}
	if playerID != "" {
		clauses = append(clauses, "player_id = {player:String}")
		params["player"] = playerID
	}
	if sessionID != "" {
		clauses = append(clauses, "session_id = {sess:String}")
		params["sess"] = sessionID
	}
	if playID != "" {
		// Sentinel "—" matches rows ingested before play_id was stamped.
		if playID == "—" {
			clauses = append(clauses, "play_id = ''")
		} else {
			clauses = append(clauses, "play_id = {play:String}")
			params["play"] = playID
		}
	}
	if from != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		params["from"] = from
	}
	if to != "" {
		clauses = append(clauses, "ts < parseDateTime64BestEffort({to:String})")
		params["to"] = to
	}
	if len(clauses) == 0 {
		clauses = append(clauses, "ts >= now() - INTERVAL 1 HOUR")
	}

	// IMPORTANT: don't alias the outer projection as `ts` (or `revision`)
	// when those names match real columns referenced in WHERE — ClickHouse
	// resolves the unqualified name to the SELECT alias (a String here)
	// instead of the underlying DateTime64 column, which yields:
	//   Code: 43. DB::Exception: No operation greaterOrEquals between
	//   String and DateTime64(3) (ILLEGAL_TYPE_OF_ARGUMENT)
	// on `ts >= parseDateTime64BestEffort({from:String})`. Wrap the
	// query so WHERE / ORDER BY see the typed column and the outer
	// SELECT does the toString() rename. Same trap the legacy
	// /analytics/api/snapshots handler hit (see main.go § "Don't alias
	// the projection as `ts`").
	var query string
	if strideMs > 0 {
		query = fmt.Sprintf(`
			SELECT toString(ts_max) AS ts, toString(rev_max) AS revision,
			       any(play_id) AS play_id, session_json_max AS session_json
			FROM (
			  SELECT
			    argMax(ts, ts) AS ts_max,
			    argMax(revision, ts) AS rev_max,
			    play_id,
			    argMax(session_json, ts) AS session_json_max
			  FROM %s.%s
			  WHERE %s
			  GROUP BY intDiv(toUnixTimestamp64Milli(ts), %d), play_id
			)
			GROUP BY ts_max, rev_max, session_json_max
			ORDER BY ts_max %s
			LIMIT %d
			FORMAT JSONEachRow`,
			cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "),
			strideMs, order, limit)
	} else {
		query = fmt.Sprintf(`
			SELECT toString(ts_raw) AS ts, toString(rev_raw) AS revision,
			       play_id, session_json
			FROM (
			  SELECT
			    ts AS ts_raw,
			    revision AS rev_raw,
			    play_id,
			    session_json
			  FROM %s.%s
			  WHERE %s
			  ORDER BY ts %s
			  LIMIT %d
			)
			FORMAT JSONEachRow`,
			cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "),
			order, limit)
	}

	if stream {
		streamSnapshotsNDJSON(w, r.Context(), cfg, query, params, includeRaw)
		return
	}

	rows, err := queryClickHouseRows(r.Context(), cfg, query, params)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "clickhouse query failed", err.Error())
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		item, ok := buildSnapshotItem(row, includeRaw)
		if !ok {
			continue
		}
		out = append(out, item)
	}

	writeJSONv2(w, http.StatusOK, map[string]any{
		"items":       out,
		"next_cursor": nil,
	})
}

// v2NetworkRequestsHandler reads network_requests, reshapes columnar
// rows into the v1 row map the translator expects, then projects to
// NetworkLogEntry. Adds ts/player_id row keys on top.
func v2NetworkRequestsHandler(w http.ResponseWriter, r *http.Request, cfg config) {
	// network_requests table columns: ts, session_id, play_id, method,
	// url, upstream_url, request_kind, content_type, status,
	// bytes_in, bytes_out, dns/connect/tls/ttfb/transfer/total/
	// client_wait_ms, faulted, fault_type, fault_action, fault_category.
	// NB: no `player_id` column — that's a session-level identifier
	// only on session_snapshots. session_id is the dashboard's lookup
	// key for live + replay parity.
	playerID := r.URL.Query().Get("player_id")
	sessionID := r.URL.Query().Get("session_id")
	if sessionID == "" {
		// legacy alias — testing-session-ui.js passed `session=` before
		sessionID = r.URL.Query().Get("session")
	}
	playID := r.URL.Query().Get("play_id")
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	limit := parseLimit(r.URL.Query().Get("limit"), 500, 5000)
	faultedOnly := r.URL.Query().Get("faulted_only") == "true"

	params := map[string]string{}
	clauses := []string{}
	// player_id is now stamped on every row by the forwarder (see
	// net_log.go § sessionToPlayerID + handlePayload). Direct filter,
	// no subquery. session_id remains as a transitional bridge for
	// dashboards that still pass the small numeric port slot.
	if playerID != "" {
		clauses = append(clauses, "player_id = {pid:String}")
		params["pid"] = playerID
	}
	if sessionID != "" {
		clauses = append(clauses, "session_id = {sess:String}")
		params["sess"] = sessionID
	}
	if playID != "" {
		// "—" sentinel = pre-stamp rows; same as snapshots handler.
		if playID == "—" {
			clauses = append(clauses, "play_id = ''")
		} else {
			clauses = append(clauses, "play_id = {play:String}")
			params["play"] = playID
		}
	}
	if from != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		params["from"] = from
	}
	if to != "" {
		clauses = append(clauses, "ts < parseDateTime64BestEffort({to:String})")
		params["to"] = to
	}
	if faultedOnly {
		clauses = append(clauses, "faulted = 1")
	}
	if len(clauses) == 0 {
		clauses = append(clauses, "ts >= now() - INTERVAL 1 HOUR")
	}

	// Wrap in a sub-select so the outer toString aliases don't shadow
	// the typed `ts` column referenced by WHERE / ORDER BY (same trap
	// the snapshots handler addresses; see commit 9cfca6f).
	query := fmt.Sprintf(`
		SELECT
		  toString(ts_raw) AS timestamp,
		  session_id, player_id, play_id,
		  method, url, upstream_url, request_kind, content_type,
		  status, bytes_in, bytes_out,
		  ttfb_ms, total_ms, dns_ms, connect_ms, tls_ms, transfer_ms, client_wait_ms,
		  faulted, fault_type, fault_action, fault_category
		FROM (
		  SELECT
		    ts AS ts_raw,
		    session_id, player_id, play_id,
		    method, url, upstream_url, request_kind, content_type,
		    status, bytes_in, bytes_out,
		    ttfb_ms, total_ms, dns_ms, connect_ms, tls_ms, transfer_ms, client_wait_ms,
		    faulted, fault_type, fault_action, fault_category
		  FROM %s.network_requests
		  WHERE %s
		  ORDER BY ts ASC
		  LIMIT %d
		)
		FORMAT JSONEachRow`,
		cfg.chDatabase, strings.Join(clauses, " AND "), limit)

	rows, err := queryClickHouseRows(r.Context(), cfg, query, params)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "clickhouse query failed", err.Error())
		return
	}

	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		// Normalize ClickHouse "YYYY-MM-DD HH:MM:SS.fff" to RFC3339
		// before handing to NetworkEntryFromV1 — its getTime only
		// parses RFC3339 / RFC3339Nano, so the raw CH form would
		// silently drop the Timestamp field via omitempty and the
		// dashboard renderer would skip every row (it filters on
		// `entry.timestamp`).
		if ts, ok := row["timestamp"].(string); ok && ts != "" {
			row["timestamp"] = strings.Replace(ts, " ", "T", 1) + "Z"
		}
		entry := v2translate.NetworkEntryFromV1(row)
		entryJSON, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var item map[string]any
		if err := json.Unmarshal(entryJSON, &item); err != nil {
			continue
		}
		// ts row-key (mirrors entry.timestamp; same value, just under
		// the row-level key for callers that key off `ts`).
		if ts, ok := row["timestamp"].(string); ok && ts != "" {
			item["ts"] = ts
		}
		if sid, _ := row["session_id"].(string); sid != "" {
			item["session_id"] = sid
		}
		if pid, _ := row["player_id"].(string); pid != "" {
			item["player_id"] = pid
		}
		if pid, _ := row["play_id"].(string); pid != "" {
			item["play_id"] = pid
		}
		out = append(out, item)
	}

	writeJSONv2(w, http.StatusOK, map[string]any{
		"items":       out,
		"next_cursor": nil,
	})
}

// ---- helpers --------------------------------------------------------

func parseLimit(s string, def, max int) int {
	if s == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		if n > max {
			return max
		}
		return n
	}
	return def
}

func writeJSONv2(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// buildSnapshotItem turns one ClickHouse row into a SnapshotRow-shaped
// map. Returns ok=false when the row's session_json doesn't translate
// to a v2 PlayerRecord (no player_id yet, parse failure, …).
//
// When includeRaw is true the original `session_json` blob is left on
// the row so legacy renderers (session-replay.js) can keep reading
// flat v1 fields without the dashboard learning the typed shape.
func buildSnapshotItem(row map[string]any, includeRaw bool) (map[string]any, bool) {
	sessionJSON, _ := row["session_json"].(string)
	if sessionJSON == "" {
		return nil, false
	}
	var session map[string]any
	if err := json.Unmarshal([]byte(sessionJSON), &session); err != nil {
		return nil, false
	}
	rec, ok := v2translate.PlayerFromSession(session)
	if !ok {
		return nil, false
	}
	recJSON, err := json.Marshal(rec)
	if err != nil {
		return nil, false
	}
	var item map[string]any
	if err := json.Unmarshal(recJSON, &item); err != nil {
		return nil, false
	}
	// `ts` invariant: the chart's event timestamps must match what the
	// live SSE consumer (testing.html) reads from the raw frame, byte
	// for byte — anything that re-formats event_time can introduce
	// drift. The ClickHouse `ts` column is reformatted on write
	// (snapshotEventTimeAsCHTimestamp normalizes to "YYYY-MM-DD …"),
	// so emitting it on the wire would surface that conversion to the
	// client. Pull the ORIGINAL player_metrics_event_time string out
	// of session_json verbatim and use that as the row's ts.
	//
	// Daisy invariant 2026-05-10: noone can introduce jitter; the
	// player's event_time is the single source of truth.
	if origET, ok := session["player_metrics_event_time"].(string); ok && origET != "" {
		item["ts"] = origET
	} else if rowTs, ok := row["ts"].(string); ok && rowTs != "" {
		// Defensive fallback — if the snapshot somehow has no
		// player_metrics_event_time in session_json, fall back to the
		// CH column. Shouldn't happen for any real player frame; if
		// it does, surface the CH format so debugging is possible
		// instead of dropping the row entirely.
		item["ts"] = rowTs
	} else {
		// No timestamp anywhere — schema requires `ts`, so drop.
		return nil, false
	}
	item["revision"], _ = row["revision"].(string)
	if pid, _ := row["play_id"].(string); pid != "" {
		item["play_id"] = pid
	}
	if includeRaw {
		item["session_json"] = sessionJSON
	}
	return item, true
}

// streamSnapshotsNDJSON pipes the ClickHouse JSONEachRow output line
// by line, transforming each row through buildSnapshotItem. The wire
// format is one JSON object per line (Content-Type: application/x-ndjson)
// — matches the shape the legacy /analytics/api/snapshots endpoint
// already emits, so session-replay.js's existing line reader keeps
// working unchanged.
func streamSnapshotsNDJSON(w http.ResponseWriter, ctx context.Context, cfg config, query string, params map[string]string, includeRaw bool) {
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		writeProblemv2(w, http.StatusInternalServerError, "url parse", err.Error())
		return
	}
	qs := u.Query()
	qs.Set("query", query)
	qs.Set("default_format", "JSONEachRow")
	for k, v := range params {
		qs.Set("param_"+k, v)
	}
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		writeProblemv2(w, http.StatusInternalServerError, "request build", err.Error())
		return
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "clickhouse fetch", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		writeProblemv2(w, resp.StatusCode, "clickhouse status", strings.TrimSpace(string(body)))
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	// 64MB token cap — large session_json blobs (long manifest_variants
	// + _v2_* stash) routinely exceed the default 64KB and have been
	// observed past 16MB during incident testing (review finding #6).
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		// Cancel the read loop if the client disconnected — long replay
		// windows can emit 200K+ rows; without this we keep parsing for
		// nothing (review finding #3).
		if err := ctx.Err(); err != nil {
			return
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		item, ok := buildSnapshotItem(row, includeRaw)
		if !ok {
			continue
		}
		out, err := json.Marshal(item)
		if err != nil {
			continue
		}
		out = append(out, '\n')
		if _, err := w.Write(out); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// Surface a truncation error to the client as a final NDJSON line so
	// dashboard hydration can detect partial reads instead of silently
	// rendering an incomplete window (review finding #2).
	if err := scanner.Err(); err != nil {
		errLine, _ := json.Marshal(map[string]any{
			"_error":  "stream truncated",
			"_detail": err.Error(),
		})
		_, _ = w.Write(append(errLine, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// queryClickHouseRows runs a JSONEachRow query and returns parsed rows.
// Used by the v2 handlers — they need to transform each row through
// the v2 translator, which the streaming proxyClickHouseJSON path
// can't do.
func queryClickHouseRows(ctx context.Context, cfg config, query string, params map[string]string) ([]map[string]any, error) {
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return nil, err
	}
	qs := u.Query()
	qs.Set("query", query)
	qs.Set("default_format", "JSONEachRow")
	for k, v := range params {
		qs.Set("param_"+k, v)
	}
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out []map[string]any
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal(line, &row); err != nil {
			continue
		}
		out = append(out, row)
	}
	return out, scanner.Err()
}

func writeProblemv2(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "about:blank",
		"title":  title,
		"status": status,
		"detail": detail,
	})
}
