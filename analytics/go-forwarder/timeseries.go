// /api/v2/timeseries SSE endpoint — see plans/shiny-exploring-jellyfish.md
// for the architecture writeup. (Wire path used to be /api/v3/timeseries
// during early development; renamed to v2 because it's purely additive
// on the v2 contract.)
//
// One EventSource per (player_id, play_id). The handler:
//
//  1. Resolves liveness (does the (player, play) still have activity
//     in the ring within the last 30s, or does the row still exist
//     in CH with no play.ended_at).
//  2. Subscribes to the ring BEFORE running the backfill, so any
//     delta that lands during backfill is queued (not dropped).
//  3. Emits a `meta` event describing the wire shape the caller is
//     about to receive.
//  4. Runs per-stream ClickHouse SELECTs for the requested window,
//     using the column projection from streambundles.resolveSelection.
//     Rows are emitted as `sample` / `network` SSE events in CH's
//     ts-ascending order.
//  5. (live only) Drains the subscription channel, emitting each new
//     ring entry as it arrives. Periodic heartbeat events keep
//     proxies from idle-closing the connection.
//  6. (archive only) Emits `event:complete` and closes.
//
// Events stream: the kind/priority taxonomy SQL lives in
// events_query.go (extracted from the legacy /api/v2/session_events
// handler so both consumers compute it identically). Backfill runs
// the SQL once over [from, to]; the live loop re-runs it on a
// short poll interval with `from=highWaterTs` so new events past
// the last seen timestamp surface as they appear. Dedupe uses
// ts|type|info as the fingerprint (CH ts has ms precision, type +
// info disambiguate events that land in the same millisecond).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// timeseriesParams is the parsed view of the query string. Captured
// upfront so we can validate before we commit to SSE headers (a 400
// after `Content-Type: text/event-stream` is harder for the client).
type timeseriesParams struct {
	playerID       string
	sessionID      string // optional alt-key; honoured for v2 callers, but the ring path needs playerID
	playID         string
	from           string // ISO-8601 (passed through to CH; CH parses with parseDateTime64BestEffort)
	to             string
	limit          int
	streams        string
	bundles        string
	fieldsByStream map[streamKind][]string
	strideMs       int
	maxHz          int
}

// keepaliveInterval — period between SSE heartbeat frames. 15s
// matches nginx's default proxy_read_timeout of 60s with margin.
const keepaliveInterval = 15 * time.Second

// initialBackfillCap — server-side ceiling on rows returned per
// stream during the backfill burst. Surfaced in `meta.server_caps`
// so the client can adjust its memory budget honestly.
const (
	backfillCapEvents = 50000
	backfillCapNetwork = 5000
)

// mountTimeseriesHandlers wires the timeseries SSE endpoint into mux.
//
// Mounted at /api/v2/timeseries; nginx exposes it externally as
// /analytics/api/v2/timeseries via the same rewrite that handles every
// other /analytics/api/v2/* endpoint.
//
// Naming note: the wire path used to be /api/v3/timeseries during early
// development. It was renamed to /api/v2/timeseries because it doesn't
// break any v2 contract — same player_id/play_id identity, same backing
// tables, same ProblemDetails error envelope, reusable BasicAuth scheme.
// REST major versions mark breaking changes; this endpoint is purely
// additive (a new transport + a new query convention) on top of v2.
func mountTimeseriesHandlers(mux *http.ServeMux, cfg config, ring *Ring) {
	mux.HandleFunc("/api/v2/timeseries", makeTimeseriesHandler(cfg, ring))
}

func makeTimeseriesHandler(cfg config, ring *Ring) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params, err := parseTimeseriesParams(r)
		if err != nil {
			writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
			return
		}

		selections, err := resolveSelection(params.streams, params.bundles, params.fieldsByStream)
		if err != nil {
			if isBadParam(err) {
				writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
				return
			}
			writeProblemv2(w, http.StatusInternalServerError, "resolve error", err.Error())
			return
		}

		ctx := r.Context()
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		setSSEHeaders(w)

		// Ring is keyed by player_id only. play_id (if the caller
		// passed one) is enforced as an emission filter in
		// streamLiveDeltas below — entries for OTHER plays of the
		// same player are dropped on send. This way a subscription
		// stays valid across play rotations: if no play_id was
		// requested, every play's rows flow through; if a specific
		// play_id was requested, the live tail for THAT play does.
		key := ringKey{PlayerID: params.playerID}

		// Liveness: subscribe BEFORE looking at activity so any deltas
		// landing during the activity check are queued, not dropped.
		// If we conclude !live we unsubscribe before returning.
		subCh, cancelSub := ring.Subscribe(key)
		defer cancelSub()

		live := isStreamLive(ring, key, params.playerID, params.playID)

		// `to` was supplied AND is in the past → this is an archive
		// read of a closed window. Even if the player happens to
		// still be active, the caller has asked for a bounded view,
		// so the live tail (ring deltas + events poller) would push
		// rows past `to` into the client's cache and the UI's brush
		// rail / NetworkLog / PlayLog would then show rows outside
		// the focus window. Treat as !live so we complete after
		// backfill.
		if params.to != "" {
			if t, err := time.Parse(time.RFC3339Nano, params.to); err == nil {
				if time.Since(t) > 5*time.Second {
					live = false
				}
			}
		}

		emitMeta(w, flusher, selections, live, ring, params)

		// Track the high-water timestamp emitted during backfill so
		// the live loop can dedupe against ring entries it already
		// saw via the backfill scan.
		emittedIDs := newEmittedSet()

		// Backfill per stream.
		ringCutoffMs := ring.windowMs
		for _, sel := range selections {
			if err := emitBackfill(ctx, w, flusher, cfg, ring, key, sel, params, emittedIDs, ringCutoffMs); err != nil {
				if ctx.Err() != nil {
					return // client gone
				}
				// Wire an error event then bail. Backfill failures
				// usually mean CH is down; client will reconnect.
				emitError(w, flusher, "backfill failed: "+err.Error())
				return
			}
		}

		if !live {
			emitComplete(w, flusher, "archive_or_play_ended")
			return
		}

		// Events are derived from samples + network rows so there's no
		// ring channel for them. If the caller asked for events, run
		// a poller alongside the ring-driven live loop. The poller
		// tracks the highest emitted ts and uses it as `from=` on each
		// poll so subsequent runs only return new rows.
		// SSE writes from the live ring loop and the events poll
		// goroutine could otherwise interleave bytes mid-frame; share
		// a mutex when both paths are active.
		writeMu := &sync.Mutex{}
		if selectionsHasEvents(selections) {
			cancel := startEventsPoller(ctx, w, flusher, writeMu, cfg, params, emittedIDs)
			defer cancel()
		}

		streamLiveDeltas(ctx, w, flusher, writeMu, subCh, selections, emittedIDs, params.playID)
	}
}

// parseTimeseriesParams extracts and validates the query params.
// Returns badParamError for client-fixable problems.
func parseTimeseriesParams(r *http.Request) (timeseriesParams, error) {
	q := r.URL.Query()
	p := timeseriesParams{
		playerID:       strings.TrimSpace(q.Get("player_id")),
		sessionID:      strings.TrimSpace(q.Get("session_id")),
		playID:         strings.TrimSpace(q.Get("play_id")),
		from:           strings.TrimSpace(q.Get("from")),
		to:             strings.TrimSpace(q.Get("to")),
		streams:        strings.TrimSpace(q.Get("streams")),
		bundles:        strings.TrimSpace(q.Get("bundles")),
		fieldsByStream: map[streamKind][]string{},
	}

	// archive:<sessionID>:<playID> prefix shorthand — keeps the v3
	// endpoint accepting the same id shape getPlayer(archivePlayerId)
	// uses on the client side.
	if strings.HasPrefix(p.playerID, "archive:") {
		rest := strings.TrimPrefix(p.playerID, "archive:")
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) == 2 {
			if p.sessionID == "" {
				p.sessionID = parts[0]
			}
			if p.playID == "" {
				p.playID = parts[1]
			}
			p.playerID = "" // archive ids aren't real player ids
		}
	}

	if p.playerID == "" && p.sessionID == "" {
		return p, errBadParam("one of player_id or session_id is required")
	}
	if p.streams == "" {
		return p, errBadParam("streams=events,network,markers is required")
	}

	p.limit = parseLimit(q.Get("limit"), 50000, 200000)

	if v := q.Get("stride_ms"); v != "" {
		if n, err := atoiInRange(v, 1, 60000); err == nil {
			p.strideMs = n
		}
	}
	if v := q.Get("max_hz"); v != "" {
		if n, err := atoiInRange(v, 1, 1000); err == nil {
			p.maxHz = n
		}
	}

	// `fields=col1,col2` applies to whichever stream those columns
	// belong to. For simplicity in this first cut we accept a flat
	// list and let resolveSelection apply it to ALL enabled streams
	// (CH will reject unknown columns per-stream — clean 4xx surface).
	if f := strings.TrimSpace(q.Get("fields")); f != "" {
		list := splitCSV(f)
		for _, sk := range []streamKind{streamEvents, streamNetwork, streamMarkers} {
			p.fieldsByStream[sk] = list
		}
	}

	return p, nil
}

func atoiInRange(s string, lo, hi int) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, err
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("out of range [%d,%d]", lo, hi)
	}
	return n, nil
}

// isStreamLive returns true if the (player, play) appears to still
// be actively producing rows. Heuristic: the ring's bucket has been
// touched within `liveActivityWindow`, OR (if we have no ring data)
// the most recent CH row for this key is recent.
//
// First-cut: ring-only check. CH-fallback for "freshly opened
// session with no ring history yet" comes once we have the v2
// proxy-state probe wired. The cost of getting this wrong (returning
// !live when actually live) is the client closes the SSE early and
// has to reconnect after the next sample — annoying but recoverable.
//
// Note: we read the bucket lastActivityMs without holding any locks
// the ring exposes; the data race is benign (read of int64 on
// platforms where Go's memory model permits torn reads is irrelevant
// here — at worst we mis-classify by one tick and reconnect).
func isStreamLive(ring *Ring, key ringKey, playerID, playID string) bool {
	const liveActivityWindow = 30 * 1000 // ms
	b := ring.bucket(key)
	b.mu.RLock()
	last := b.lastActivityMs
	b.mu.RUnlock()
	if last == 0 {
		// No ring activity yet — assume live so the client subscribes
		// and waits. If the play actually ended before we ever saw
		// it, the subscription will idle until the SSE timeout cycles
		// it. Better than wrongly closing early on a fresh play.
		return true
	}
	return nowMs()-last < liveActivityWindow
}

// emittedSet is a small fingerprint dedupe used to skip ring deltas
// the backfill scan already covered. Bounded; we expect very few
// overlaps in practice (the backfill ends roughly at "now" and the
// next delta lands a few hundred ms later).
type emittedSet struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	limit int
}

func newEmittedSet() *emittedSet {
	return &emittedSet{seen: map[string]struct{}{}, limit: 100000}
}

func (s *emittedSet) Add(kind streamKind, fp string) bool {
	if fp == "" {
		return true
	}
	k := string(kind) + "|" + fp
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[k]; ok {
		return false
	}
	if len(s.seen) >= s.limit {
		// Cheap pressure relief: drop everything. Worst case is
		// re-emitting a handful of duplicate rows; client dedupes.
		s.seen = map[string]struct{}{}
	}
	s.seen[k] = struct{}{}
	return true
}

// emitBackfill runs the per-stream CH SELECT for [from, to] and emits
// each row as an SSE event. Also pulls ring entries in the same
// window and emits any that CH hasn't covered (the recently-ingested
// rows that haven't been INSERTed yet — or just confirmed-but-not-
// evicted, which CH covers, but the emittedSet dedupes either way).
//
// Returns nil even when no rows match — an empty backfill is a valid
// state (e.g. brand-new play). Returns error only on transport
// failures the caller should surface.
func emitBackfill(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	cfg config,
	ring *Ring,
	key ringKey,
	sel streamSelection,
	params timeseriesParams,
	seen *emittedSet,
	ringWindowMs int64,
) error {
	switch sel.Stream {
	case streamEvents:
		if err := emitBackfillSamples(ctx, w, flusher, cfg, sel, params, seen); err != nil {
			return err
		}
		return emitBackfillFromRing(w, flusher, ring, key, sel.Stream, kindSample, params, seen, ringWindowMs)
	case streamNetwork:
		if err := emitBackfillNetwork(ctx, w, flusher, cfg, sel, params, seen); err != nil {
			return err
		}
		return emitBackfillFromRing(w, flusher, ring, key, sel.Stream, kindNetwork, params, seen, ringWindowMs)
	case streamMarkers:
		// Events are derived at query time — no ring; backfill is the
		// taxonomy SQL over [from, to]. Live continuation is handled
		// by startEventsPoller after backfill returns.
		return emitBackfillEvents(ctx, w, flusher, cfg, params, seen)
	}
	return nil
}

func emitBackfillSamples(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	cfg config, sel streamSelection, params timeseriesParams, seen *emittedSet) error {
	q, args, err := buildSamplesQuery(cfg, sel, params)
	if err != nil {
		return err
	}
	rows, err := queryClickHouseRows(ctx, cfg, q, args)
	if err != nil {
		return err
	}
	for _, row := range rows {
		ts := stringField(row, "ts")
		if !seen.Add(streamEvents, ts) {
			continue
		}
		writeSSEEvent(w, "event", ts, row)
	}
	flusher.Flush()
	return nil
}

func emitBackfillNetwork(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	cfg config, sel streamSelection, params timeseriesParams, seen *emittedSet) error {
	q, args, err := buildNetworkQuery(cfg, sel, params)
	if err != nil {
		return err
	}
	rows, err := queryClickHouseRows(ctx, cfg, q, args)
	if err != nil {
		return err
	}
	for _, row := range rows {
		fp := stringField(row, "entry_fingerprint")
		if !seen.Add(streamNetwork, fp) {
			continue
		}
		writeSSEEvent(w, "network", fp, row)
	}
	flusher.Flush()
	return nil
}

// emitBackfillFromRing scans the ring for entries that CH hasn't
// produced (typically only the unconfirmed tail). Anything CH already
// emitted is skipped by the seen-set.
func emitBackfillFromRing(w http.ResponseWriter, flusher http.Flusher,
	ring *Ring, key ringKey, stream streamKind, kind ringKind,
	params timeseriesParams, seen *emittedSet, ringWindowMs int64) error {
	// Window: the SSE backfill ostensibly covers [from, to], but
	// for the ring overlay we always scan the full ring (cheap, and
	// guarantees we don't miss a row whose ts falls outside the
	// caller's coarse from/to but inside our ring's retention).
	from := nowMs() - ringWindowMs
	to := nowMs() + 1000 // small lookahead for clock skew
	for _, e := range ring.Range(key, from, to, []ringKind{kind}) {
		var payload any
		var fp string
		switch stream {
		case streamEvents:
			r, ok := e.Payload.(*row)
			if !ok {
				continue
			}
			payload = r
			fp = r.Ts
		case streamNetwork:
			r, ok := e.Payload.(*netRow)
			if !ok {
				continue
			}
			payload = r
			fp = fmt.Sprintf("%d", r.EntryFingerprint)
		}
		if !seen.Add(stream, fp) {
			continue
		}
		writeSSEEvent(w, string(streamToEventName(stream)), fp, payload)
	}
	flusher.Flush()
	return nil
}

// streamLiveDeltas blocks until ctx is canceled (client disconnect)
// or the subscription channel closes (bucket evicted). Emits each
// new ring entry as an SSE event; heartbeats every keepaliveInterval.
//
// `playIDFilter`, if non-empty, drops entries whose PlayID does not
// match (case-insensitive). Empty string means "all plays for this
// player", which is the default the dashboard uses.
func streamLiveDeltas(ctx context.Context, w http.ResponseWriter, flusher http.Flusher,
	writeMu *sync.Mutex, subCh <-chan *ringEntry, selections []streamSelection,
	seen *emittedSet, playIDFilter string) {

	enabled := map[streamKind]bool{}
	for _, s := range selections {
		enabled[s.Stream] = true
	}
	playFilterLower := strings.ToLower(playIDFilter)

	heartbeat := time.NewTicker(keepaliveInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			lockedWrite(writeMu, func() {
				writeSSEEvent(w, "heartbeat", "", map[string]any{
					"ts": time.Now().UTC().Format(time.RFC3339Nano),
				})
				flusher.Flush()
			})
		case e, ok := <-subCh:
			if !ok {
				return
			}
			stream := ringKindToStream(e.Kind)
			if !enabled[stream] {
				continue
			}
			if playFilterLower != "" && strings.ToLower(e.PlayID) != playFilterLower {
				// Caller asked for a specific play; this entry is
				// from a different play of the same player. Drop.
				continue
			}
			var payload any
			var fp string
			switch e.Kind {
			case kindSample:
				r, _ := e.Payload.(*row)
				payload = r
				if r != nil {
					fp = r.Ts
				}
			case kindNetwork:
				r, _ := e.Payload.(*netRow)
				payload = r
				if r != nil {
					fp = fmt.Sprintf("%d", r.EntryFingerprint)
				}
			}
			if payload == nil {
				continue
			}
			if !seen.Add(stream, fp) {
				continue
			}
			lockedWrite(writeMu, func() {
				writeSSEEvent(w, string(streamToEventName(stream)), fp, payload)
				flusher.Flush()
			})
		}
	}
}

// lockedWrite serializes SSE writes when the events poller is also
// active. If mu is nil it falls through — both branches are valid
// since the poller is only started when the caller asked for events.
func lockedWrite(mu *sync.Mutex, fn func()) {
	if mu != nil {
		mu.Lock()
		defer mu.Unlock()
	}
	fn()
}

// selectionsHasEvents reports whether the resolved selections include
// the events stream. The events stream is special because it doesn't
// come off the ring — see startEventsPoller.
func selectionsHasEvents(selections []streamSelection) bool {
	for _, s := range selections {
		if s.Stream == streamMarkers {
			return true
		}
	}
	return false
}

func ringKindToStream(k ringKind) streamKind {
	if k == kindNetwork {
		return streamNetwork
	}
	return streamEvents
}

// streamToEventName maps a streamKind to the SSE `event:` name the
// frame is written under. After issue #472's rename:
//   streamEvents  → "event"   (player event, was "sample")
//   streamNetwork → "network"
//   streamMarkers → "marker"  (classifier output, was "event")
func streamToEventName(s streamKind) string {
	switch s {
	case streamEvents:
		return "event"
	case streamNetwork:
		return "network"
	case streamMarkers:
		return "marker"
	}
	return "data"
}

// ---------- CH query builders ----------

func buildSamplesQuery(cfg config, sel streamSelection, params timeseriesParams) (string, map[string]string, error) {
	args := map[string]string{}
	clauses := []string{}
	if params.playerID != "" {
		clauses = append(clauses, "lowerUTF8(player_id) = lowerUTF8({player:String})")
		args["player"] = params.playerID
	}
	if params.sessionID != "" {
		clauses = append(clauses, "session_id = {sess:String}")
		args["sess"] = params.sessionID
	}
	if params.playID != "" {
		if params.playID == "—" {
			clauses = append(clauses, "play_id = ''")
		} else {
			clauses = append(clauses, "lowerUTF8(play_id) = lowerUTF8({play:String})")
			args["play"] = params.playID
		}
	}
	if params.from != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		args["from"] = params.from
	}
	if params.to != "" {
		clauses = append(clauses, "ts <= parseDateTime64BestEffort({to:String})")
		args["to"] = params.to
	}
	if len(clauses) == 0 {
		return "", nil, errBadParam("at least one identity clause required")
	}
	limit := params.limit
	if limit <= 0 || limit > backfillCapEvents {
		limit = backfillCapEvents
	}
	// Subquery wrap so WHERE/ORDER BY operate on the native DateTime64
	// `ts` column. Without the wrap, the outer SELECT's
	// `toString(ts) AS ts` projection shadows the source column and the
	// WHERE clause's `ts >= parseDateTime64BestEffort(...)` is then a
	// String/DateTime64 comparison that CH rejects with
	// ILLEGAL_TYPE_OF_ARGUMENT. Same pattern v2_handlers.go uses.
	cols := buildSelectColumns(sel.Columns)
	q := fmt.Sprintf(`SELECT %s FROM (SELECT * FROM %s.%s WHERE %s ORDER BY ts ASC LIMIT %d) FORMAT JSONEachRow`,
		cols, cfg.chDatabase, cfg.chTable, strings.Join(clauses, " AND "), limit)
	return q, args, nil
}

func buildNetworkQuery(cfg config, sel streamSelection, params timeseriesParams) (string, map[string]string, error) {
	args := map[string]string{}
	clauses := []string{}
	if params.playerID != "" {
		clauses = append(clauses, "lowerUTF8(player_id) = lowerUTF8({player:String})")
		args["player"] = params.playerID
	}
	if params.sessionID != "" {
		clauses = append(clauses, "session_id = {sess:String}")
		args["sess"] = params.sessionID
	}
	if params.playID != "" {
		if params.playID == "—" {
			clauses = append(clauses, "play_id = ''")
		} else {
			clauses = append(clauses, "lowerUTF8(play_id) = lowerUTF8({play:String})")
			args["play"] = params.playID
		}
	}
	if params.from != "" {
		clauses = append(clauses, "ts >= parseDateTime64BestEffort({from:String})")
		args["from"] = params.from
	}
	if params.to != "" {
		clauses = append(clauses, "ts <= parseDateTime64BestEffort({to:String})")
		args["to"] = params.to
	}
	if len(clauses) == 0 {
		return "", nil, errBadParam("at least one identity clause required")
	}
	limit := params.limit
	if limit <= 0 || limit > backfillCapNetwork {
		limit = backfillCapNetwork
	}
	// Subquery wrap — same DateTime64-vs-String aliasing reason as
	// buildSamplesQuery above.
	cols := buildSelectColumns(sel.Columns)
	q := fmt.Sprintf(`SELECT %s FROM (SELECT * FROM %s.network_requests WHERE %s ORDER BY ts ASC LIMIT %d) FORMAT JSONEachRow`,
		cols, cfg.chDatabase, strings.Join(clauses, " AND "), limit)
	return q, args, nil
}

// buildSelectColumns produces the SELECT projection list. `ts` is
// emitted as a String for round-trip stability — the CH default for
// DateTime64 in JSONEachRow is unfortunately locale-affected for
// some clients, and the dashboard always parses ts via Date.parse.
func buildSelectColumns(cols []string) string {
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		switch c {
		case "ts":
			parts = append(parts, "toString(ts) AS ts")
		default:
			parts = append(parts, c)
		}
	}
	return strings.Join(parts, ", ")
}

// stringField pulls a column value out of a JSONEachRow row map and
// returns it as a string. Used for fingerprinting (event id) only.
func stringField(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%v", t)
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}

// ---------- SSE wire helpers ----------

func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // disable nginx response buffering
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeSSEEvent emits one SSE frame. Best-effort: write errors mean
// the client is gone and the caller's next ctx check will return.
func writeSSEEvent(w io.Writer, event, id string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	if event != "" {
		_, _ = fmt.Fprintf(w, "event: %s\n", event)
	}
	if id != "" {
		_, _ = fmt.Fprintf(w, "id: %s\n", id)
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
}

func emitMeta(w http.ResponseWriter, flusher http.Flusher, selections []streamSelection,
	live bool, ring *Ring, params timeseriesParams) {
	streams := make([]string, 0, len(selections))
	cols := map[string][]string{}
	for _, s := range selections {
		streams = append(streams, string(s.Stream))
		cols[string(s.Stream)] = s.Columns
	}
	meta := map[string]any{
		"streams":     streams,
		"columns":     cols,
		"live":        live,
		"server_caps": map[string]int{"events": backfillCapEvents, "network": backfillCapNetwork},
		"ring":        map[string]any{"window_seconds": ring.windowMs / 1000},
		"from":        params.from,
		"to":          params.to,
	}
	writeSSEEvent(w, "meta", "", meta)
	flusher.Flush()
}

func emitComplete(w http.ResponseWriter, flusher http.Flusher, reason string) {
	writeSSEEvent(w, "complete", "", map[string]string{"reason": reason})
	flusher.Flush()
}

func emitError(w http.ResponseWriter, flusher http.Flusher, msg string) {
	writeSSEEvent(w, "stream_error", "", map[string]string{"message": msg})
	flusher.Flush()
}
