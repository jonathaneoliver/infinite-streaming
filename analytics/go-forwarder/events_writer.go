// events_writer.go — write-time event classification (issue #469).
//
// As each snapshot / network row lands on the inbound channels (in
// batchInserter / batchInsertNet), this file's hooks project the row
// into the eventclass package's typed view, run the registered
// classifiers, and forward the emitted Events to a dedicated batch
// inserter that writes session_events.
//
// Replaces the read-time multi-CTE UNION-ALL SQL in events_query.go.
// Same event taxonomy (type / info / kind / priority strings); see
// eventclass/types.go for the closed set.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder/eventclass"
)

// eventsChanBuf sizes the events channel. Events are emitted at a
// fraction of the snapshot/network row rate (most rows yield zero
// events; some yield 1–3), so we don't need the parent channels'
// capacity to keep up.
const eventsChanBuf = 4096

// prevSnapshotCache holds the most-recent eventclass.Snapshot per
// (player_id, play_id) so the stateful snapshot classifiers (counter
// bumps, fault edge, error transition, rate shift) can compare cur
// against prev. Keyed identically to the eventclass package's
// pair-classifier internal map.
//
// Memory bound: entries are pruned on a 5-minute GC sweep when no
// new snapshot has been seen for that pair (matches the network
// retry classifier's prune cadence).
type prevSnapshotCache struct {
	mu       sync.Mutex
	entries  map[string]prevEntry
	lastGCAt time.Time
}

type prevEntry struct {
	snap eventclass.Snapshot
	seen time.Time
}

func newPrevSnapshotCache() *prevSnapshotCache {
	return &prevSnapshotCache{entries: make(map[string]prevEntry)}
}

func (c *prevSnapshotCache) updateAndPrev(cur eventclass.Snapshot) *eventclass.Snapshot {
	key := cur.PlayerID + "|" + cur.PlayID
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	// Periodic GC. Amortised over every snapshot ingest.
	if now.Sub(c.lastGCAt) > 5*time.Minute {
		cutoff := now.Add(-5 * time.Minute)
		for k, e := range c.entries {
			if e.seen.Before(cutoff) {
				delete(c.entries, k)
			}
		}
		c.lastGCAt = now
	}
	var prevPtr *eventclass.Snapshot
	if prev, ok := c.entries[key]; ok {
		p := prev.snap
		prevPtr = &p
	}
	c.entries[key] = prevEntry{snap: cur, seen: now}
	return prevPtr
}

// snapshotToEventclass projects the forwarder's internal `row` (the
// raw session_snapshots row about to be written) into the slim
// eventclass.Snapshot the classifiers consume. New classifier fields
// require adding them here AND on eventclass.Snapshot — keep the two
// in sync.
func snapshotToEventclass(r *row) eventclass.Snapshot {
	return eventclass.Snapshot{
		Ts:        r.Ts,
		PlayerID:  r.PlayerID,
		PlayID:    r.PlayID,
		AttemptID: r.AttemptID,
		SessionID: r.SessionID,
		// Classification stays empty here; the chEvent encoder omits
		// it with omitempty so CH applies the DEFAULT 'other' and
		// the reclassification path upgrades it later.

		LastEvent:    r.LastEvent,
		PlayerError:  r.PlayerError,
		VideoBitrate: r.VideoBitrateMbps,
		RateFromMbps: r.RateFromMbps,
		RateToMbps:   r.RateToMbps,

		ManifestConsecutiveFailures:       r.ManifestConsecutiveFailures,
		SegmentConsecutiveFailures:        r.SegmentConsecutiveFailures,
		MasterManifestConsecutiveFailures: r.MasterManifestConsecutiveFailures,
		AllConsecutiveFailures:            r.AllConsecutiveFailures,
		TransportConsecutiveFailures:      r.TransportConsecutiveFailures,
		FaultCountTransferActiveTimeout:   r.FaultCountTransferActiveTimeout,
		FaultCountTransferIdleTimeout:     r.FaultCountTransferIdleTimeout,
		LoopCountServer:                   r.LoopCountServer,

		TransportFaultActive: r.TransportFaultActive,
	}
}

// netRowToEventclass projects the forwarder's internal `netRow` into
// eventclass.NetworkRequest. Same sync invariant as
// snapshotToEventclass — add fields to both structs together.
func netRowToEventclass(r *netRow) eventclass.NetworkRequest {
	return eventclass.NetworkRequest{
		Ts:        r.Ts,
		PlayerID:  r.PlayerID,
		PlayID:    r.PlayID,
		AttemptID: r.AttemptID,
		SessionID: r.SessionID,
		// Classification: see comment in snapshotToEventclass above.

		Method:       r.Method,
		Path:         r.Path,
		URL:          r.URL,
		Status:       r.Status,
		Faulted:      r.Faulted,
		FaultType:    r.FaultType,
		ClientWaitMs: r.ClientWaitMs,
		TransferMs:   r.TransferMs,
	}
}

// chEvent is the JSONEachRow representation of an Event for the
// INSERT body. Field tags map to the session_events column names; the
// Kind/Priority/Fingerprint helpers run at write time so callers
// don't have to duplicate the logic.
type chEvent struct {
	Ts               string `json:"ts"`
	PlayerID         string `json:"player_id"`
	PlayID           string `json:"play_id"`
	AttemptID        uint32 `json:"attempt_id"`
	SessionID        string `json:"session_id"`
	Type             string `json:"type"`
	Subtype          string `json:"subtype,omitempty"`
	Info             string `json:"info"`
	Kind             string `json:"kind"`
	Priority         uint8  `json:"priority"`
	EventFingerprint uint64 `json:"event_fingerprint"`
	// Classification is set to the column DEFAULT 'other' on insert;
	// the reclassification path (classification.go) upgrades it to
	// 'interesting' or 'favourite' alongside the parent snapshots /
	// network_requests rows via ALTER UPDATE. omitempty keeps the
	// JSONEachRow body free of the field so CH applies the DEFAULT.
	Classification string `json:"classification,omitempty"`
}

func toChEvent(e eventclass.Event) chEvent {
	var stallDur float64
	// Parse "N.NNs" back out of stall info so Priority can promote
	// long stalls to severity 1 — matches the legacy multiIf table
	// which had access to duration_s directly.
	if e.Type == eventclass.TypeStall && strings.HasSuffix(e.Info, "s") {
		fmt.Sscanf(e.Info, "%fs", &stallDur)
	}
	return chEvent{
		Ts:               e.Ts,
		PlayerID:         e.PlayerID,
		PlayID:           e.PlayID,
		AttemptID:        e.AttemptID,
		SessionID:        e.SessionID,
		Type:             e.Type,
		Subtype:          e.Subtype,
		Info:             e.Info,
		Kind:             e.Kind(),
		Priority:         e.Priority(stallDur),
		EventFingerprint: e.Fingerprint(),
		Classification:   e.Classification,
	}
}

// batchInsertEvents drains the events channel into session_events.
// Same batch + tick pattern as batchInserter / batchInsertNet so the
// CH write rate stays predictable.
func batchInsertEvents(ctx context.Context, cfg config, in <-chan eventclass.Event) {
	buf := make([]chEvent, 0, cfg.flushBatch)
	tick := time.NewTicker(cfg.flushEvery)
	defer tick.Stop()
	flush := func() {
		if len(buf) == 0 {
			return
		}
		if err := insertEvents(ctx, cfg, buf); err != nil {
			log.Printf("events insert failed (%d rows dropped): %v", len(buf), err)
		}
		buf = buf[:0]
	}
	for {
		select {
		case <-ctx.Done():
			flush()
			return
		case e, ok := <-in:
			if !ok {
				flush()
				return
			}
			buf = append(buf, toChEvent(e))
			if len(buf) >= cfg.flushBatch {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}

func insertEvents(ctx context.Context, cfg config, rows []chEvent) error {
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for i := range rows {
		if err := enc.Encode(&rows[i]); err != nil {
			return err
		}
	}
	q := fmt.Sprintf("INSERT INTO %s.session_markers FORMAT JSONEachRow", cfg.chDatabase)
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

// emitClassifiedEventsForSnapshot is the hook batchInserter calls per
// incoming row. Projects the row, runs the registered snapshot
// classifiers against (prev, cur), and pushes the resulting events on
// the channel. Non-blocking — drops events if the channel is full so
// a slow CH writer doesn't back-pressure the snapshot ingest.
func emitClassifiedEventsForSnapshot(
	r *row,
	prev *prevSnapshotCache, out chan<- eventclass.Event,
) {
	cur := snapshotToEventclass(r)
	prevSnap := prev.updateAndPrev(cur)
	for _, e := range eventclass.ClassifySnapshot(prevSnap, &cur) {
		select {
		case out <- e:
		default:
			// Channel full — drop. session_events is a derived
			// surface; losing some events under sustained
			// back-pressure is preferable to stalling the parent
			// snapshot insert path.
		}
	}
}

// emitClassifiedEventsForNetwork is the hook batchInsertNet calls per
// incoming netRow. Same back-pressure policy as the snapshot variant.
func emitClassifiedEventsForNetwork(
	r *netRow,
	out chan<- eventclass.Event,
) {
	cur := netRowToEventclass(r)
	for _, e := range eventclass.ClassifyNetwork(&cur) {
		select {
		case out <- e:
		default:
		}
	}
}
