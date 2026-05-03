// Session-bundle download: fetches snapshots + HAR + events for a single
// (session_id, play_id) and streams them back as a ZIP. The HAR file is
// constructed inside the bundle so it loads directly in Chrome DevTools'
// Network panel; the rest is JSON / NDJSON / a README so the bundle is
// self-describing for offline triage long after the 30-day TTL has
// dropped the source rows.
//
// Streamed end-to-end: ClickHouse query → JSON parse → ZIP writer →
// http.ResponseWriter. Peak memory is dominated by a single page of
// network rows (~10 MB), not the full bundle.

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Header names that can carry credentials. Stripped from HAR entries
// before they reach the browser. The proxy's capture path already
// drops most of these, but defense-in-depth: the bundle is meant to
// be attachable to a bug report without further scrubbing.
var sensitiveHeaderNames = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"cookie":              {},
	"set-cookie":          {},
	"x-api-key":           {},
}

// Query-string param names whose values are redacted (replaced with
// "[REDACTED]"). Matched case-insensitively against the param name.
var sensitiveQueryNames = regexp.MustCompile(`(?i)^(token|auth|access[_-]?token|api[_-]?key|key|password|secret|signature|sig)$`)

func registerBundleHandler(mux *http.ServeMux, cfg config) {
	// GET /api/session_bundle?session=<id>&play_id=<id>
	mux.HandleFunc("/api/session_bundle", func(w http.ResponseWriter, r *http.Request) {
		sessionID := r.URL.Query().Get("session")
		if sessionID == "" {
			http.Error(w, "missing session", http.StatusBadRequest)
			return
		}
		playID := r.URL.Query().Get("play_id")
		// Sentinel "—" means "rows ingested before play_id was
		// stamped" — translate back to empty string.
		if playID == "—" {
			playID = ""
		}

		// Build a useful filename — small enough to fit in a Slack
		// upload column without needing a tooltip, but with enough
		// detail that a folder of bundles is still scannable.
		shortPlay := playID
		if len(shortPlay) > 8 {
			shortPlay = shortPlay[:8]
		}
		if shortPlay == "" {
			shortPlay = "noplay"
		}
		stamp := time.Now().UTC().Format("20060102-1504")
		safeSession := safeFilenameSegment(sessionID)
		filename := fmt.Sprintf("session-%s-%s-%s.zip", safeSession, shortPlay, stamp)

		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
		w.Header().Set("Cache-Control", "no-store")

		zw := zip.NewWriter(w)
		// Always close — finalises the central directory. If we error
		// mid-stream the client gets a truncated zip, but headers are
		// already on the wire so we can't switch to plain http.Error.
		defer zw.Close()

		ctx := r.Context()

		if err := writeSessionMetadata(ctx, zw, cfg, sessionID, playID); err != nil {
			fmt.Fprintf(w, "\n[bundle error: session.json: %v]\n", err)
			return
		}
		if err := writeSnapshotsNDJSON(ctx, zw, cfg, sessionID, playID); err != nil {
			fmt.Fprintf(w, "\n[bundle error: snapshots.ndjson: %v]\n", err)
			return
		}
		if err := writeNetworkHAR(ctx, zw, cfg, sessionID, playID); err != nil {
			fmt.Fprintf(w, "\n[bundle error: network.har: %v]\n", err)
			return
		}
		if err := writeEventsJSON(ctx, zw, cfg, sessionID, playID); err != nil {
			fmt.Fprintf(w, "\n[bundle error: events.json: %v]\n", err)
			return
		}
		if err := writeREADME(zw, sessionID, playID); err != nil {
			return
		}
	})
}

// safeFilenameSegment trims any character likely to confuse a shell or
// download manager. Session IDs are usually small integers so this is
// belt-and-braces.
func safeFilenameSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// session.json — at-a-glance summary that opens first. Reuses the
// /api/sessions per-play row plus a couple of derived fields the picker
// already computes, so the bundle reads naturally next to a screenshot
// of the picker row.
func writeSessionMetadata(ctx context.Context, zw *zip.Writer, cfg config, sessionID, playID string) error {
	params := map[string]string{"session": sessionID}
	pidPred := "play_id = ''"
	if playID != "" {
		pidPred = "play_id = {play:String}"
		params["play"] = playID
	}
	query := fmt.Sprintf(`
		SELECT
		  toString(min(ts)) AS started,
		  toString(max(ts)) AS last_seen,
		  count() AS snapshots,
		  any(player_id) AS player_id,
		  any(group_id) AS group_id,
		  any(content_id) AS content_id,
		  argMax(player_state, ts) AS last_player_state,
		  argMax(player_error, ts) AS last_player_error,
		  max(stall_count) AS stalls,
		  max(dropped_frames) AS dropped_frames,
		  max(loop_count_server) AS loops_server,
		  round(avgIf(video_quality_pct, video_quality_pct > 0), 1) AS avg_quality_pct,
		  max(master_manifest_consecutive_failures) AS master_manifest_failures,
		  max(manifest_consecutive_failures) AS manifest_failures,
		  max(segment_consecutive_failures) AS segment_failures,
		  max(transport_consecutive_failures) AS transport_failures
		FROM %s.%s
		WHERE session_id = {session:String} AND %s
		FORMAT JSONEachRow`, cfg.chDatabase, cfg.chTable, pidPred)
	body, err := chQueryBytes(ctx, cfg, query, params)
	if err != nil {
		return err
	}
	row := bytes.TrimSpace(body)
	if len(row) == 0 {
		row = []byte("{}")
	}
	// Wrap the row with our own envelope so the file is self-describing
	// (vs. a bare per-table aggregate row that needs context).
	envelope := map[string]json.RawMessage{
		"session_id": jsonString(sessionID),
		"play_id":    jsonString(playID),
		"summary":    row,
	}
	out, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return err
	}
	return writeZipFile(zw, "session.json", out)
}

// snapshots.ndjson — one raw session_json blob per line, the same
// payload the go-proxy SSE stream emitted before the forwarder split
// hot fields into typed columns. We deliberately drop the typed
// columns (and `revision`, `ts`) here: the blobs already carry every
// field the live session-viewer reads, and offline replay should see
// exactly what the proxy sent — not a denormalised view of it.
//
// TabSeparatedRaw emits each session_json value as a single newline-
// terminated line, no escaping. The proxy serialises blobs as one
// line of compact JSON so this round-trips cleanly into NDJSON.
func writeSnapshotsNDJSON(ctx context.Context, zw *zip.Writer, cfg config, sessionID, playID string) error {
	params := map[string]string{"session": sessionID}
	pidPred := "play_id = ''"
	if playID != "" {
		pidPred = "play_id = {play:String}"
		params["play"] = playID
	}
	query := fmt.Sprintf(`
		SELECT session_json
		FROM %s.%s
		WHERE session_id = {session:String} AND %s
		ORDER BY ts ASC
		FORMAT TabSeparatedRaw`, cfg.chDatabase, cfg.chTable, pidPred)
	body, err := chQueryBytes(ctx, cfg, query, params)
	if err != nil {
		return err
	}
	return writeZipFile(zw, "snapshots.ndjson", body)
}

// network.har — HAR 1.2 envelope built from network_requests rows. The
// header / query-string sanitiser runs over each entry before it's
// written so the on-disk bundle is safe to share. Custom proxy fields
// (fault_type, upstream_url, etc.) live under `_extensions`.
func writeNetworkHAR(ctx context.Context, zw *zip.Writer, cfg config, sessionID, playID string) error {
	params := map[string]string{"session": sessionID}
	clauses := []string{"session_id = {session:String}"}
	// HAR rows are scoped to the play's time range (not play_id) for
	// the same reason /api/session_events does — iOS strips play_id
	// from variant/segment URLs. We use a subquery on the snapshots
	// table to find min/max ts for the play.
	if playID != "" {
		params["play"] = playID
		clauses = append(clauses, fmt.Sprintf(
			"nr.ts BETWEEN (SELECT min(ts) FROM %s.%s WHERE session_id = {session:String} AND play_id = {play:String}) "+
				"AND (SELECT max(ts) FROM %s.%s WHERE session_id = {session:String} AND play_id = {play:String})",
			cfg.chDatabase, cfg.chTable, cfg.chDatabase, cfg.chTable))
	} else {
		clauses = append(clauses, "play_id = ''")
	}
	query := fmt.Sprintf(`
		SELECT
		  toString(ts) AS ts, method, url, upstream_url, path, request_kind,
		  status, bytes_in, bytes_out, content_type,
		  request_range, response_content_range,
		  dns_ms, connect_ms, tls_ms, ttfb_ms, transfer_ms, total_ms, client_wait_ms,
		  faulted = 1 AS faulted, fault_type, fault_action, fault_category,
		  request_headers, response_headers, query_string
		FROM %s.network_requests AS nr
		WHERE %s
		ORDER BY nr.ts ASC
		FORMAT JSONEachRow`, cfg.chDatabase, strings.Join(clauses, " AND "))
	body, err := chQueryBytes(ctx, cfg, query, params)
	if err != nil {
		return err
	}
	entries := []json.RawMessage{}
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		entry, err := harEntryFromRow(line)
		if err != nil {
			// Best effort: skip a single malformed row rather than
			// fail the whole bundle.
			continue
		}
		entries = append(entries, entry)
	}
	har := map[string]any{
		"log": map[string]any{
			"version": "1.2",
			"creator": map[string]any{
				"name":    "InfiniteStream forwarder",
				"version": "1",
			},
			"entries": entries,
		},
	}
	out, err := json.MarshalIndent(har, "", "  ")
	if err != nil {
		return err
	}
	return writeZipFile(zw, "network.har", out)
}

// harEntryFromRow converts one network_requests row into a HAR 1.2
// `entries[]` element. Sanitises sensitive headers + query-string
// params; preserves proxy-specific fields under `_extensions`.
func harEntryFromRow(line []byte) (json.RawMessage, error) {
	var row struct {
		TS                   string `json:"ts"`
		Method               string `json:"method"`
		URL                  string `json:"url"`
		UpstreamURL          string `json:"upstream_url"`
		Path                 string `json:"path"`
		RequestKind          string `json:"request_kind"`
		Status               int    `json:"status"`
		BytesIn              int64  `json:"bytes_in,string"`
		BytesOut             int64  `json:"bytes_out,string"`
		ContentType          string `json:"content_type"`
		RequestRange         string `json:"request_range"`
		ResponseContentRange string `json:"response_content_range"`
		DNS_ms               float64
		Connect_ms           float64
		TLS_ms               float64
		TTFB_ms              float64
		Transfer_ms          float64
		Total_ms             float64
		ClientWait_ms        float64
		Faulted              int    `json:"faulted"`
		FaultType            string `json:"fault_type"`
		FaultAction          string `json:"fault_action"`
		FaultCategory        string `json:"fault_category"`
		RequestHeaders       string `json:"request_headers"`
		ResponseHeaders      string `json:"response_headers"`
		QueryString          string `json:"query_string"`
	}
	// Manual decode for the timing fields because JSONEachRow emits
	// floats as bare numbers but our struct doesn't have explicit
	// json tags for the *_ms fields (their JSON keys collide with
	// Go's default case-insensitive match).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(line, &row); err != nil {
		return nil, err
	}
	row.DNS_ms = numberFromRaw(raw["dns_ms"])
	row.Connect_ms = numberFromRaw(raw["connect_ms"])
	row.TLS_ms = numberFromRaw(raw["tls_ms"])
	row.TTFB_ms = numberFromRaw(raw["ttfb_ms"])
	row.Transfer_ms = numberFromRaw(raw["transfer_ms"])
	row.Total_ms = numberFromRaw(raw["total_ms"])
	row.ClientWait_ms = numberFromRaw(raw["client_wait_ms"])

	reqHeaders := sanitizeHeaders(parseHeaderArray(row.RequestHeaders))
	respHeaders := sanitizeHeaders(parseHeaderArray(row.ResponseHeaders))
	queryParams := sanitizeQueryParams(parseHeaderArray(row.QueryString))

	startedDateTime := normaliseTSToISO(row.TS)

	entry := map[string]any{
		"startedDateTime": startedDateTime,
		"time":            row.Total_ms,
		"request": map[string]any{
			"method":      defaultIfEmpty(row.Method, "GET"),
			"url":         row.URL,
			"httpVersion": "HTTP/1.1",
			"headers":     reqHeaders,
			"queryString": queryParams,
			"cookies":     []any{},
			"headersSize": -1,
			"bodySize":    row.BytesOut,
		},
		"response": map[string]any{
			"status":      row.Status,
			"statusText":  "",
			"httpVersion": "HTTP/1.1",
			"headers":     respHeaders,
			"cookies":     []any{},
			"content": map[string]any{
				"size":     row.BytesIn,
				"mimeType": defaultIfEmpty(row.ContentType, "application/octet-stream"),
			},
			"redirectURL":  "",
			"headersSize":  -1,
			"bodySize":     row.BytesIn,
		},
		"cache": map[string]any{},
		"timings": map[string]any{
			"blocked": -1,
			"dns":     row.DNS_ms,
			"connect": row.Connect_ms,
			"send":    0,
			"wait":    row.TTFB_ms,
			"receive": row.Transfer_ms,
			"ssl":     row.TLS_ms,
		},
		"_extensions": map[string]any{
			"upstream_url":           row.UpstreamURL,
			"path":                   row.Path,
			"request_kind":           row.RequestKind,
			"request_range":          row.RequestRange,
			"response_content_range": row.ResponseContentRange,
			"client_wait_ms":         row.ClientWait_ms,
			"faulted":                row.Faulted != 0,
			"fault_type":             row.FaultType,
			"fault_action":           row.FaultAction,
			"fault_category":         row.FaultCategory,
		},
	}
	return json.Marshal(entry)
}

// parseHeaderArray takes a serialised JSON array of {name,value} pairs
// (the way ClickHouse stores request_headers / response_headers / query_string)
// and returns it as a Go slice. Returns nil on parse error so the
// downstream sanitiser sees an empty slice.
func parseHeaderArray(s string) []map[string]string {
	if s == "" {
		return nil
	}
	var out []map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// sanitizeHeaders returns a copy with sensitive header values replaced
// by "[REDACTED]". Preserves the header name so the on-disk bundle
// still tells the reader "yes, an Authorization header was present"
// without exposing the value.
func sanitizeHeaders(in []map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, h := range in {
		name := h["name"]
		value := h["value"]
		if _, sensitive := sensitiveHeaderNames[strings.ToLower(name)]; sensitive {
			value = "[REDACTED]"
		}
		out = append(out, map[string]string{"name": name, "value": value})
	}
	return out
}

// sanitizeQueryParams redacts query-string values whose names look
// credential-shaped. The full list is in `sensitiveQueryNames`.
func sanitizeQueryParams(in []map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(in))
	for _, q := range in {
		name := q["name"]
		value := q["value"]
		if sensitiveQueryNames.MatchString(name) {
			value = "[REDACTED]"
		}
		out = append(out, map[string]string{"name": name, "value": value})
	}
	return out
}

func numberFromRaw(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	// Fall back to string-encoded numbers (some ClickHouse types emit
	// String JSON for large integers).
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v
		}
	}
	return 0
}

func defaultIfEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// normaliseTSToISO converts the ClickHouse "YYYY-MM-DD HH:MM:SS.mmm"
// format into the HAR-spec ISO 8601 with explicit Z suffix.
func normaliseTSToISO(s string) string {
	if s == "" {
		return ""
	}
	// "2026-05-03 12:34:56.789" → "2026-05-03T12:34:56.789Z"
	return strings.Replace(s, " ", "T", 1) + "Z"
}

// events.json — flat list of player-emitted events from last_event.
// This is the "simple half" of /api/session_events: the player tells
// us "stall_start", "rate_shift_down" etc. directly. The complex
// derived events (lag-based fault transitions, paired stall-start /
// stall-end durations, HAR-derived http_5xx etc.) are NOT included
// here — recompute them live via /api/session_events while the
// analytics tier is up. Note in README points the reader there.
func writeEventsJSON(ctx context.Context, zw *zip.Writer, cfg config, sessionID, playID string) error {
	params := map[string]string{"session": sessionID}
	pidPred := "play_id = ''"
	if playID != "" {
		pidPred = "play_id = {play:String}"
		params["play"] = playID
	}
	query := fmt.Sprintf(`
		SELECT toString(ts) AS ts, last_event AS type,
		       player_error AS info
		FROM %s.%s
		WHERE session_id = {session:String} AND %s
		  AND last_event != ''
		  AND last_event NOT IN ('heartbeat','state_change','playing','video_bitrate_change')
		ORDER BY ts ASC
		FORMAT JSONEachRow`, cfg.chDatabase, cfg.chTable, pidPred)
	body, err := chQueryBytes(ctx, cfg, query, params)
	if err != nil {
		return err
	}
	// Wrap NDJSON-style output as a JSON array for events.json so it's
	// easy to load (`json.load(open('events.json'))` from Python /
	// jq one-liners). Body is `{...}\n{...}\n...`; convert by joining.
	var out bytes.Buffer
	out.WriteByte('[')
	first := true
	for _, line := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !first {
			out.WriteByte(',')
		}
		out.WriteByte('\n')
		out.Write(line)
		first = false
	}
	if !first {
		out.WriteByte('\n')
	}
	out.WriteByte(']')
	out.WriteByte('\n')
	return writeZipFile(zw, "events.json", out.Bytes())
}

// writeREADME — short orientation file for whoever opens the zip in
// six months. Not auto-generated from a template because the prose
// belongs in source.
func writeREADME(zw *zip.Writer, sessionID, playID string) error {
	body := fmt.Sprintf(`# InfiniteStream session bundle

session_id: %s
play_id:    %s
generated:  %s

## Files

- session.json        Top-level summary (player, content, timing, fault counts).
- snapshots.ndjson    One JSON object per per-second snapshot — the
                      original blob the go-proxy SSE stream emitted,
                      pre-typed-column-extraction. Use jq:
                        cat snapshots.ndjson | jq 'select(.stall_count > 0)'
- network.har         HAR 1.2 envelope. Open in Chrome DevTools:
                        chrome://devtools  →  Network panel  →  Import HAR
                      Custom proxy fields (fault_type, upstream_url, etc.)
                      live under each entry's _extensions key.
- events.json         Player-emitted events (stall_start, rate_shift_down,
                      error, user_marked, etc.) — the "simple half" of
                      the events query. Derived events (paired stall
                      durations, lag-based fault transitions, HAR-derived
                      http_5xx, etc.) are recomputable live via
                      /analytics/api/session_events while the analytics
                      tier is up.

## Sanitisation

Authorization, Cookie, Set-Cookie, X-API-Key headers redacted.
Query-string parameters with names matching token / auth / api_key /
key / password / secret / signature / sig also redacted. The proxy
capture path drops most of these upstream; this is defense-in-depth.
`, sessionID, playID, time.Now().UTC().Format(time.RFC3339))
	return writeZipFile(zw, "README.md", []byte(body))
}

// writeZipFile creates one entry inside the zip writer with deflate
// compression. Returns the writer's error directly so the caller can
// abort the bundle.
func writeZipFile(zw *zip.Writer, name string, body []byte) error {
	header := &zip.FileHeader{
		Name:     name,
		Method:   zip.Deflate,
		Modified: time.Now(),
	}
	w, err := zw.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, bytes.NewReader(body))
	return err
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}
