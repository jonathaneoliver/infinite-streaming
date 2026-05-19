// control_events.go — write-time ingest path for control_events
// (issue #474 Milestone B).
//
// Sibling of batchInserter / batchInsertNet. Subscribes to go-proxy's
// /api/control/stream SSE, dedupes by event_fingerprint, stamps
// `labels[]` via computeControlLabels, batch-inserts into
// infinite_streaming.control_events.
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

// ctrlRow mirrors the control_events CH schema. JSON tags are the
// column names; the JSONEachRow INSERT body uses them as-is.
type ctrlRow struct {
	Ts               string   `json:"ts"`
	PlayerID         string   `json:"player_id"`
	PlayID           string   `json:"play_id"`
	AttemptID        uint32   `json:"attempt_id"`
	SessionID        string   `json:"session_id"`
	Source           string   `json:"source"`
	Event            string   `json:"event"`
	Info             string   `json:"info"`
	Labels           []string `json:"labels,omitempty"`
	// `,string` JSON tag — see the same field on netRow.EntryFingerprint.
	// UInt64 values exceed JS's 2^53; without `,string` the SSE-live
	// overlay would lose precision and never match the CH-backfill
	// fingerprint, double-rendering the row.
	EventFingerprint uint64   `json:"event_fingerprint,string"`
	Classification   string   `json:"classification,omitempty"`
}

// ctrlStreamEvent matches the JSON envelope go-proxy emits on
// /api/control/stream — same shape as the network-stream pattern.
type ctrlStreamEvent struct {
	SessionID string  `json:"session_id"`
	Entry     ctrlEnt `json:"entry"`
}

// ctrlEnt is the proxy-side control_events envelope. Mirrors the CH
// row but uses time.Time for Ts so we can format consistently.
type ctrlEnt struct {
	Ts        time.Time `json:"ts"`
	PlayerID  string    `json:"player_id"`
	PlayID    string    `json:"play_id"`
	AttemptID uint32    `json:"attempt_id"`
	Source    string    `json:"source"`
	Event     string    `json:"event"`
	Info      string    `json:"info"`
}

// fingerprintCtrl over (player_id, play_id, ts ms, source, event, info)
// — same role as entry_fingerprint on network_requests. Lets the
// forwarder replay the SSE on reconnect without double-inserting.
func fingerprintCtrl(e *ctrlEnt) uint64 {
	h := fnv.New64a()
	h.Write([]byte(e.PlayerID))
	h.Write([]byte{0})
	h.Write([]byte(e.PlayID))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", e.Ts.UnixMilli())
	h.Write([]byte{0})
	h.Write([]byte(e.Source))
	h.Write([]byte{0})
	h.Write([]byte(e.Event))
	h.Write([]byte{0})
	h.Write([]byte(e.Info))
	return h.Sum64()
}

// ctrlSeen is a fingerprint dedupe set, mirroring netSeen. Bounded
// LRU-by-time with a periodic GC sweep.
type ctrlSeen struct {
	mu       sync.Mutex
	entries  map[uint64]time.Time
	capacity int
}

func newCtrlSeen(capacity int) *ctrlSeen {
	return &ctrlSeen{entries: make(map[uint64]time.Time), capacity: capacity}
}

func (s *ctrlSeen) check(fp uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.entries[fp]; ok {
		return true
	}
	if len(s.entries) >= s.capacity {
		// Cheap eviction: drop oldest 10%. Stops the map from growing
		// without bound on a long-running forwarder.
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

// entryToCtrlRow translates the SSE envelope into the CH row, stamps
// labels[]. Same role as entryToRow for the network path.
//
// player_id / play_id are run through canonicalV2ID so they match the
// lowercase UUID form session_events writes — otherwise the proxy's
// original-case strings (iOS emits uppercase) wouldn't match the
// dashboard's lowercase filter and the PlayLog "Control" bucket
// silently stayed empty. Issue #474 follow-up.
func entryToCtrlRow(sessionID string, e *ctrlEnt) ctrlRow {
	r := ctrlRow{
		Ts:               e.Ts.UTC().Format("2006-01-02 15:04:05.000"),
		PlayerID:         canonicalV2ID(e.PlayerID),
		PlayID:           canonicalV2ID(e.PlayID),
		AttemptID:        e.AttemptID,
		SessionID:        sessionID,
		Source:           e.Source,
		Event:            e.Event,
		Info:             e.Info,
		EventFingerprint: fingerprintCtrl(e),
	}
	r.Labels = computeControlLabels(&r)
	return r
}

// batchInsertControl drains the control row channel into control_events.
// Same batch + tick pattern as batchInserter / batchInsertNet.
func batchInsertControl(ctx context.Context, cfg config, in <-chan ctrlRow) {
	buf := make([]ctrlRow, 0, cfg.flushBatch)
	tick := time.NewTicker(cfg.flushEvery)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := insertCtrl(ctx, cfg, buf); err != nil {
			log.Printf("control insert failed (%d rows dropped): %v", len(buf), err)
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

func insertCtrl(ctx context.Context, cfg config, rows []ctrlRow) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range rows {
		if err := enc.Encode(&rows[i]); err != nil {
			return err
		}
	}
	q := fmt.Sprintf("INSERT INTO %s.control_events FORMAT JSONEachRow", cfg.chDatabase)
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

// streamControlSSE subscribes to go-proxy's /api/control/stream. One
// SSE `data:` line per emitted control event. Same shape as
// streamNetworkSSE; reconnect/backoff is in runControlStream.
func streamControlSSE(ctx context.Context, cfg config, seen *ctrlSeen, out chan<- ctrlRow) error {
	base := proxyBaseFromSSE(cfg.sseURL)
	if base == "" {
		return fmt.Errorf("cannot derive proxy base from SSE URL %q", cfg.sseURL)
	}
	endpoint := strings.TrimRight(base, "/") + "/api/control/stream"
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
		return fmt.Errorf("control stream %d", resp.StatusCode)
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
		var ev ctrlStreamEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			continue
		}
		if ev.Entry.Ts.IsZero() || ev.Entry.Event == "" {
			continue
		}
		fp := fingerprintCtrl(&ev.Entry)
		if seen.check(fp) {
			continue
		}
		select {
		case out <- entryToCtrlRow(ev.SessionID, &ev.Entry):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// emitBackfillControl runs the control_events backfill SQL for the
// timeseries SSE handler. Same shape as the retired markers backfill
// — one INSERT-time row per SSE frame, dedupe via event_fingerprint.
// Issue #474 Milestone C.
func emitBackfillControl(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	cfg config,
	params timeseriesParams,
	seen *emittedSet,
) error {
	rows, err := runControlQuery(ctx, cfg, params, 0)
	if err != nil {
		return err
	}
	for i := len(rows) - 1; i >= 0; i-- {
		r := rows[i]
		fp := stringField(r, "event_fingerprint")
		if !seen.Add(streamControl, fp) {
			continue
		}
		writeSSEEvent(w, "control", fp, r)
	}
	flusher.Flush()
	return nil
}

// startControlPoller mirrors the retired startEventsPoller — periodic
// CH polls fold new control_events rows onto the SSE stream alongside
// the ring-driven samples/network frames. control_events writes are
// cheap and low-volume so polling is acceptable.
func startControlPoller(
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
			rows, err := runControlQuery(ctx, cfg, cp, 1000)
			if err != nil {
				continue
			}
			for i := len(rows) - 1; i >= 0; i-- {
				r := rows[i]
				fp := stringField(r, "event_fingerprint")
				if !seen.Add(streamControl, fp) {
					continue
				}
				lockedWrite(writeMu, func() {
					writeSSEEvent(w, "control", fp, r)
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

// runControlQuery executes the SELECT against control_events with the
// caller's filters. limit==0 reuses the params' limit; otherwise wins.
func runControlQuery(ctx context.Context, cfg config, params timeseriesParams, limit int) ([]map[string]any, error) {
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
	q := "SELECT ts, player_id, play_id, attempt_id, session_id, source, event, info, labels, event_fingerprint, classification " +
		"FROM " + cfg.chDatabase + ".control_events WHERE " +
		strings.Join(where, " AND ") +
		" ORDER BY ts DESC LIMIT {limit:UInt32} FORMAT JSONEachRow"
	return queryClickHouseRows(ctx, cfg, q, chParams)
}

func runControlStream(ctx context.Context, cfg config, seen *ctrlSeen, out chan<- ctrlRow) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := streamControlSSE(ctx, cfg, seen, out)
		if ctx.Err() != nil {
			return
		}
		log.Printf("control sse stream ended: %v (reconnecting in %s)", err, backoff)
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
