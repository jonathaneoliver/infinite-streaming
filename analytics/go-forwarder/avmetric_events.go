// avmetric_events.go — write-time ingest path for ios_avmetric_events
// (issue #486 spike).
//
// Sibling of batchInsertControl. Subscribes to go-proxy's
// /api/avmetrics/stream SSE, dedupes by event_fingerprint, batch-inserts
// into infinite_streaming.ios_avmetric_events. labels[] is left empty
// for the spike — once the comparison finding lands we'll know which
// AVMetric event types deserve a severity classifier (parallel to
// computeControlLabels).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// avmRow mirrors the ios_avmetric_events CH schema. JSON tags are the
// column names; the JSONEachRow INSERT body uses them as-is.
type avmRow struct {
	Ts               string   `json:"ts"`
	PlayerID         string   `json:"player_id"`
	PlayID           string   `json:"play_id"`
	AttemptID        uint32   `json:"attempt_id"`
	SessionID        string   `json:"session_id"`
	EventType        string   `json:"event_type"`
	EventTsMs        int64    `json:"event_ts_ms"`
	RawJSON          string   `json:"raw_json"`
	Labels           []string `json:"labels,omitempty"`
	// `,string` JSON tag — UInt64 fingerprints exceed JS's 2^53; see
	// the same field on ctrlRow.
	EventFingerprint uint64   `json:"event_fingerprint,string"`
	Classification   string   `json:"classification,omitempty"`
}

// avmStreamEvent matches the JSON envelope go-proxy emits on
// /api/avmetrics/stream — one event per SSE `data:` line.
type avmStreamEvent struct {
	SessionID string `json:"session_id"`
	Entry     avmEnt `json:"entry"`
}

// avmEnt is the proxy-side AVMetricEvent envelope. Mirrors the CH row
// but uses time.Time for Ts so we can format consistently. Raw is left
// as RawMessage to avoid a parse/re-serialise round trip — the iOS
// payload goes into CH verbatim.
type avmEnt struct {
	Ts        time.Time       `json:"ts"`
	PlayerID  string          `json:"player_id"`
	PlayID    string          `json:"play_id"`
	AttemptID uint32          `json:"attempt_id"`
	EventType string          `json:"event_type"`
	EventTsMs int64           `json:"event_ts_ms"`
	Raw       json.RawMessage `json:"raw"`
}

// fingerprintAVM over (session_id, event_ts_ms, event_type). SSE
// reconnects can replay the tail of go-proxy's hub buffer — dedupe
// keeps the resulting INSERT idempotent.
func fingerprintAVM(sessionID string, e *avmEnt) uint64 {
	h := fnv.New64a()
	h.Write([]byte(sessionID))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", e.EventTsMs)
	h.Write([]byte{0})
	h.Write([]byte(e.EventType))
	return h.Sum64()
}

// avmSeen mirrors ctrlSeen / netSeen — bounded LRU-by-time dedupe set.
type avmSeen struct {
	mu       sync.Mutex
	entries  map[uint64]time.Time
	capacity int
}

func newAVMSeen(capacity int) *avmSeen {
	return &avmSeen{entries: make(map[uint64]time.Time), capacity: capacity}
}

func (s *avmSeen) check(fp uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[fp]; ok {
		return true
	}
	if len(s.entries) >= s.capacity {
		cutoff := time.Now().Add(-5 * time.Minute)
		for k, t := range s.entries {
			if t.Before(cutoff) {
				delete(s.entries, k)
			}
		}
	}
	s.entries[fp] = time.Now()
	return false
}

// entryToAVMRow translates the SSE envelope into the CH row. player_id /
// play_id pass through canonicalV2ID — same reason as ctrlRow: iOS
// emits uppercase UUIDs, the dashboard filters on lowercase, so the
// row only joins to session_events after normalisation.
func entryToAVMRow(sessionID string, e *avmEnt) avmRow {
	raw := string(e.Raw)
	if raw == "" {
		raw = "{}"
	}
	return avmRow{
		Ts:               e.Ts.UTC().Format("2006-01-02 15:04:05.000"),
		PlayerID:         canonicalV2ID(e.PlayerID),
		PlayID:           canonicalV2ID(e.PlayID),
		AttemptID:        e.AttemptID,
		SessionID:        sessionID,
		EventType:        e.EventType,
		EventTsMs:        e.EventTsMs,
		RawJSON:          raw,
		EventFingerprint: fingerprintAVM(sessionID, e),
	}
}

// batchInsertAVM drains the AVM row channel into ios_avmetric_events.
// Same batch + tick pattern as batchInsertControl.
func batchInsertAVM(ctx context.Context, cfg config, in <-chan avmRow) {
	buf := make([]avmRow, 0, cfg.flushBatch)
	tick := time.NewTicker(cfg.flushEvery)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := insertAVM(ctx, cfg, buf); err != nil {
			log.Printf("avmetric insert failed (%d rows dropped): %v", len(buf), err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case r, ok := <-in:
			if !ok {
				flush()
				return
			}
			buf = append(buf, r)
			if len(buf) >= cfg.flushBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func insertAVM(ctx context.Context, cfg config, rows []avmRow) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range rows {
		if err := enc.Encode(&rows[i]); err != nil {
			return err
		}
	}
	q := fmt.Sprintf("INSERT INTO %s.ios_avmetric_events FORMAT JSONEachRow", cfg.chDatabase)
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return err
	}
	qs := u.Query()
	qs.Set("query", q)
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return err
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}

// streamAVMSSE subscribes to go-proxy's /api/avmetrics/stream. One SSE
// `data:` line per emitted AVMetrics event. Reconnect/backoff lives in
// runAVMStream, same shape as runControlStream.
func streamAVMSSE(ctx context.Context, cfg config, seen *avmSeen, out chan<- avmRow) error {
	base := proxyBaseFromSSE(cfg.sseURL)
	if base == "" {
		return fmt.Errorf("cannot derive proxy base from SSE URL %q", cfg.sseURL)
	}
	endpoint := strings.TrimRight(base, "/") + "/api/avmetrics/stream"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("avmetrics stream %d", resp.StatusCode)
	}
	br := newSSEReader(resp.Body)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		data, err := br.next()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			continue
		}
		var ev avmStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.Entry.Ts.IsZero() || ev.Entry.EventType == "" {
			continue
		}
		fp := fingerprintAVM(ev.SessionID, &ev.Entry)
		if seen.check(fp) {
			continue
		}
		select {
		case out <- entryToAVMRow(ev.SessionID, &ev.Entry):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// emitBackfillAVM runs the ios_avmetric_events backfill SQL for the
// /api/v2/timeseries handler. Parallel to emitBackfillControl.
func emitBackfillAVM(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	cfg config,
	params timeseriesParams,
	seen *emittedSet,
) error {
	rows, err := runAVMQuery(ctx, cfg, params, 0)
	if err != nil {
		return err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		fp := stringField(r, "event_fingerprint")
		if !seen.Add(streamAVMetrics, fp) {
			continue
		}
		writeSSEEvent(w, "avmetrics", fp, r)
	}
	flusher.Flush()
	return nil
}

// startAVMPoller mirrors startControlPoller — periodic CH polls fold
// new ios_avmetric_events rows onto the SSE stream alongside the
// ring-driven and control-events frames. AVMetrics writes are
// low-volume so polling is acceptable.
func startAVMPoller(
	parentCtx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	writeMu *sync.Mutex,
	cfg config,
	params timeseriesParams,
	seen *emittedSet,
) (cancel func()) {
	ctx, cancelFn := context.WithCancel(parentCtx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		highWater := params.from
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			cp := params
			cp.from = highWater
			rows, err := runAVMQuery(ctx, cfg, cp, 1000)
			if err != nil {
				continue
			}
			for i := len(rows) - 1; i >= 0; i-- {
				r := rows[i]
				fp := stringField(r, "event_fingerprint")
				if !seen.Add(streamAVMetrics, fp) {
					continue
				}
				lockedWrite(writeMu, func() {
					writeSSEEvent(w, "avmetrics", fp, r)
					flusher.Flush()
				})
				if ts := stringField(r, "ts"); ts > highWater {
					highWater = ts
				}
			}
		}
	}()
	return func() {
		cancelFn()
		<-done
	}
}

// runAVMQuery executes the SELECT against ios_avmetric_events with the
// caller's filters. limit==0 reuses the params' limit; otherwise wins.
func runAVMQuery(ctx context.Context, cfg config, params timeseriesParams, limit int) ([]map[string]any, error) {
	chParams := map[string]string{}
	where := []string{}
	if params.playerID != "" {
		chParams["player_id"] = params.playerID
		where = append(where, "player_id = {player_id:String}")
	}
	if params.sessionID != "" {
		chParams["session_id"] = params.sessionID
		where = append(where, "session_id = {session_id:String}")
	}
	if params.playID != "" {
		chParams["play_id"] = params.playID
		where = append(where, "play_id = {play_id:String}")
	}
	if params.from != "" {
		chParams["from"] = params.from
		where = append(where, "ts >= parseDateTime64BestEffortOrNull({from:String}, 3)")
	}
	if params.to != "" {
		chParams["to"] = params.to
		where = append(where, "ts <= parseDateTime64BestEffortOrNull({to:String}, 3)")
	}
	if len(where) == 0 {
		where = append(where, "1=1")
	}
	useLimit := limit
	if useLimit <= 0 {
		useLimit = params.limit
		if useLimit <= 0 {
			useLimit = 5000
		}
	}
	chParams["limit"] = fmt.Sprintf("%d", useLimit)
	q := "SELECT ts, player_id, play_id, attempt_id, session_id, event_type, event_ts_ms, raw_json, labels, event_fingerprint, classification " +
		"FROM " + cfg.chDatabase + ".ios_avmetric_events WHERE " +
		strings.Join(where, " AND ") +
		" ORDER BY ts DESC LIMIT {limit:UInt32} FORMAT JSONEachRow"
	return queryClickHouseRows(ctx, cfg, q, chParams)
}

func runAVMStream(ctx context.Context, cfg config, seen *avmSeen, out chan<- avmRow) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := streamAVMSSE(ctx, cfg, seen, out)
		if ctx.Err() != nil {
			return
		}
		log.Printf("avmetrics sse stream ended: %v (reconnecting in %s)", err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}
