package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// GetApiV2Events streams every v2 harness event to the connected
// client as Server-Sent Events.
//
// Reconnect contract (DESIGN.md § SSE replay window):
//   - First connect (no Last-Event-ID header) — live frames only.
//   - In-window reconnect (Last-Event-ID >= ring tail) — replay frames
//     with id > Last-Event-ID, then continue live.
//   - Out-of-window reconnect — emit a synthetic `replay.gap` frame
//     describing the lost id range, then replay the entire ring, then
//     continue live.
//
// Frame shape on the wire:
//
//	id: <uint64>
//	event: <type>
//	data: <pre-marshaled JSON>
//	\n
func (s *Server) GetApiV2Events(w http.ResponseWriter, r *http.Request, params oapigen.GetApiV2EventsParams) {
	if s.events == nil {
		writeProblem(w,
			http.StatusServiceUnavailable,
			"https://harness/errors/service-unavailable",
			"v2 event source not running",
			"the server was started without an event source — check Server.New wiring",
			nil,
		)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w,
			http.StatusInternalServerError,
			"https://harness/errors/internal",
			"streaming unsupported",
			"http.ResponseWriter doesn't support flushing — cannot stream SSE",
			nil,
		)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	// Subscribe to live frames before snapshotting the replay window
	// so no frame can slip between the snapshot and the subscription.
	_, liveCh, cancel := s.events.ring.Subscribe(256)
	defer cancel()

	// Resolve Last-Event-ID. The header is the SSE-standard reconnect
	// cursor; the spec also lists it as a `Last-Event-ID` header
	// parameter but oapigen marshals it as a custom header field.
	lastID := ParseLastEventID(r.Header.Get("Last-Event-ID"))
	since := s.events.ring.Since(lastID)

	// Track what we've already delivered so the live tail can skip
	// frames that overlapped the replay window.
	var lastDelivered uint64
	if since.Gap {
		gapBody, _ := json.Marshal(map[string]any{
			"type": "replay.gap",
			"data": map[string]any{
				"missed_from": since.MissedFrom,
				"missed_to":   since.MissedTo,
			},
		})
		// Stamp the gap frame with id = (first surviving frame id − 1)
		// — *not* the highest missed id. This way a client that
		// disconnects right after seeing `replay.gap` and reconnects
		// with that id as `Last-Event-ID` resumes from the first
		// surviving frame, instead of replaying the whole survivor
		// window again.
		gapID := since.MissedTo
		if len(since.Frames) > 0 && since.Frames[0].ID > 0 {
			gapID = since.Frames[0].ID - 1
		}
		writeFrame(w, gapID, "replay.gap", gapBody)
		lastDelivered = gapID
		flusher.Flush()
	}
	for _, f := range since.Frames {
		writeFrame(w, f.ID, f.Type, f.Payload)
		if f.ID > lastDelivered {
			lastDelivered = f.ID
		}
	}
	if len(since.Frames) > 0 {
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-liveCh:
			if !ok {
				return
			}
			if f.ID <= lastDelivered {
				// Already delivered during replay — skip.
				continue
			}
			writeFrame(w, f.ID, f.Type, f.Payload)
			lastDelivered = f.ID
			flusher.Flush()
		}
	}
}

// writeFrame emits one SSE frame to the wire. SSE format spec: each
// `id:` / `event:` / `data:` field is on its own line, terminated by
// LF; the frame is terminated by an empty line. RFC-style allows
// multi-line `data:` for embedded newlines, but our payloads are
// always single-line JSON so we don't bother.
func writeFrame(w http.ResponseWriter, id uint64, typ string, payload []byte) {
	fmt.Fprintf(w, "id: %s\nevent: %s\ndata: ", strconv.FormatUint(id, 10), typ)
	_, _ = w.Write(payload)
	_, _ = w.Write([]byte("\n\n"))
}
