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

	// `?include=raw` augments player.*-shaped frames with the full v1
	// session map under data.raw_session. Transitional flag for the
	// v1 dashboard JS migration.
	raw := wantsRaw(r)

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
		s.writeFrameMaybeRaw(w, f, raw)
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
			s.writeFrameMaybeRaw(w, f, raw)
			lastDelivered = f.ID
			flusher.Flush()
		}
	}
}

// writeFrameMaybeRaw writes a frame to the SSE stream. When raw is
// true and the frame is a player.* type, the payload is rewritten to
// include `data.raw_session` from the v1 store. For all other frame
// types (heartbeat, replay.gap, play.*) the original payload is
// passed through verbatim — the raw passthrough is player-shaped only.
func (s *Server) writeFrameMaybeRaw(w http.ResponseWriter, f Frame, raw bool) {
	if !raw || s.v1 == nil {
		writeFrame(w, f.ID, f.Type, f.Payload)
		return
	}
	switch f.Type {
	case "player.created", "player.updated":
		augmented := s.augmentPlayerFrameWithRaw(f.Payload)
		writeFrame(w, f.ID, f.Type, augmented)
	default:
		writeFrame(w, f.ID, f.Type, f.Payload)
	}
}

// augmentPlayerFrameWithRaw decodes a player.* frame, looks up the
// session map for that player_id, and re-marshals with raw_session
// embedded under data. Falls back to the original payload on any
// unmarshal/lookup error so a malformed frame never crashes the
// stream.
func (s *Server) augmentPlayerFrameWithRaw(payload []byte) []byte {
	var env struct {
		Type string                 `json:"type"`
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil || env.Data == nil {
		return payload
	}
	pidStr, _ := env.Data["id"].(string)
	if pidStr == "" {
		return payload
	}
	// events.go::publishPlayerEvent already attaches raw_session from
	// the same normalized session it derived the typed PlayerRecord
	// from. Re-fetching via SessionByPlayerID would call
	// normalizeSessionsForResponse a second time on the same broadcast
	// tick, which double-drains drainAndReset (consumed-on-read window
	// aggregator) and surfaces rtt_stale=true on the wire even when
	// the player is actively streaming. Trust the existing
	// raw_session and skip the augmentation.
	if _, alreadyHasRaw := env.Data["raw_session"]; alreadyHasRaw {
		return payload
	}
	sess, ok := s.v1.SessionByPlayerID(pidStr)
	if !ok {
		return payload
	}
	env.Data["raw_session"] = sess
	out, err := json.Marshal(env)
	if err != nil {
		return payload
	}
	return out
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
