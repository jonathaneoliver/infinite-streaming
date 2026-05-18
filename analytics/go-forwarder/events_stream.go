// events_stream.go — bridges the kind/priority taxonomy SQL (events_query.go)
// onto the v2 /api/v2/timeseries SSE pipeline.
//
// The events stream is fundamentally different from samples / network:
// rows are derived at query time from session_snapshots + network_requests
// via a multi-CTE classification, not stored in their own CH table or
// pushed through the in-memory ring. So this file owns:
//
//   - emitBackfillEvents: run the taxonomy SQL once over [from, to]
//     and emit each row as an SSE `event` frame (parallel to the
//     `sample` / `network` event names emitted by the ring path).
//
//   - startEventsPoller: spawn a goroutine that re-runs the SQL on a
//     short interval, dropping anything older than the high-water ts
//     it has already emitted. Dedupe uses the shared emittedSet so a
//     row that appears in both the backfill query and the first poll
//     window (race: ingest completes mid-flight) isn't double-emitted.
//
// Live derivation is intentionally NOT done in Go: re-implementing
// the stall pairing + lag-based counter detection in process would
// fork the taxonomy from the one the dashboard archive queries use.
// Polling CH every couple seconds is good enough for operator-facing
// tools and keeps the classification in one place.
package main

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// eventsPollInterval — how often startEventsPoller re-runs the taxonomy
// SQL while the SSE connection is open. 2s matches the dashboard's
// archive-refresh cadence on the events lane; tightening to 1s adds
// CH load without meaningfully changing operator perception, and
// slackening past ~5s makes "fault add → fault_on event" feel laggy.
const eventsPollInterval = 2 * time.Second

// eventsLiveLimit caps each poll's row count. The SQL's ORDER BY ts
// DESC keeps the newest N if the cap fires, so we never lose recent
// activity. 1000 is comfortably above any real polling-interval
// burst — operators typically see < 50 events per second even under
// heavy fault injection.
const eventsLiveLimit = 1000

// emitBackfillEvents runs the taxonomy SQL once with params' from/to
// window and emits each row as an SSE `event` frame. The high-water
// ts is folded into the shared emittedSet so a near-simultaneous
// first poll (see startEventsPoller) doesn't re-emit it.
func emitBackfillEvents(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	cfg config,
	params timeseriesParams,
	seen *emittedSet,
) error {
	rows, err := runEventsQuery(ctx, cfg, eventsQueryParams{
		PlayerID:  params.playerID,
		SessionID: params.sessionID,
		PlayID:    params.playID,
		From:      params.from,
		To:        params.to,
		Limit:     params.limit,
	})
	if err != nil {
		return err
	}
	// CH returns ts DESC so the LIMIT keeps the most-recent N. Emit
	// in chronological (ascending) order so consumers see events
	// in the order they occurred.
	for i := len(rows) - 1; i >= 0; i-- {
		row := rows[i]
		fp := eventFingerprint(row)
		if !seen.Add(streamMarkers, fp) {
			continue
		}
		writeSSEEvent(w, "marker", fp, row)
	}
	flusher.Flush()
	return nil
}

// startEventsPoller launches a background goroutine that re-runs the
// taxonomy SQL every eventsPollInterval until ctx is cancelled. The
// returned cancel func stops the goroutine and returns synchronously
// after the in-flight tick (if any) finishes; the caller defers it.
//
// Each tick:
//  1. Queries events with From = max(ts already emitted) — the SQL's
//     "ts >= parseDateTime64BestEffort(from)" clause excludes already-
//     emitted rows.
//  2. Adds each row to the shared emittedSet (which also dedupes
//     against the backfill). New rows get written through writeMu so
//     they don't interleave with ring-driven sample/network frames.
//  3. Advances the local highWater ts.
//
// CH ts has millisecond precision; we use it as an inclusive lower
// bound, so events that landed in the same millisecond as the
// previous high-water are re-fetched and then filtered by the
// emittedSet's ts|type|info fingerprint. Cheaper than tracking
// per-tick exclusive cursors.
func startEventsPoller(
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
		highWater := params.from // may be ""; first tick covers from beginning
		ticker := time.NewTicker(eventsPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			ep := eventsQueryParams{
				PlayerID:  params.playerID,
				SessionID: params.sessionID,
				PlayID:    params.playID,
				From:      highWater,
				Limit:     eventsLiveLimit,
			}
			rows, err := runEventsQuery(ctx, cfg, ep)
			if err != nil {
				// Transient CH errors during polling are NOT
				// connection-fatal — the next tick will retry. We log
				// at debug level only; surfacing a stream_error on
				// every transient hiccup would close otherwise-fine
				// long-lived subscriptions.
				continue
			}
			// Walk ascending so the high-water always moves forward.
			for i := len(rows) - 1; i >= 0; i-- {
				row := rows[i]
				fp := eventFingerprint(row)
				if !seen.Add(streamMarkers, fp) {
					continue
				}
				lockedWrite(writeMu, func() {
					writeSSEEvent(w, "marker", fp, row)
					flusher.Flush()
				})
				if ts := stringField(row, "ts"); ts > highWater {
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

// eventFingerprint produces a dedupe key stable across the backfill
// query and the live poll. ts alone isn't enough because multiple
// event types can land in the same ms (e.g. fault_on + transport_failure
// from the same tick). Combining ts|type|info — info carries
// distinguishing context like "rate 2.5→1.0 Mbps" or the request
// URL — gives a unique-enough key without pulling in heavier hashing.
func eventFingerprint(row map[string]any) string {
	ts := stringField(row, "ts")
	typ := stringField(row, "type")
	info := stringField(row, "info")
	return fmt.Sprintf("%s|%s|%s", ts, typ, info)
}
