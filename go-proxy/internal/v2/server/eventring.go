package server

import (
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Frame is one published SSE event ready for the wire.
//
//   - id is the monotonically-increasing server-issued sequence number
//     (decimal-encoded uint64 in the SSE `id:` field).
//   - typ is the v2 event type (e.g. "player.updated", "heartbeat").
//   - payload is the *fully-marshaled JSON body* — the SSE `data:` line
//     copies it verbatim, no re-marshaling per subscriber.
//
// Pre-marshaling means a thousand connected SSE clients all share one
// JSON byte slice instead of N copies of the same encode.
type Frame struct {
	ID      uint64
	Type    string
	Payload []byte
	Created time.Time
}

// EventRing is a bounded in-memory ring of recent frames + a fan-out
// hub for live subscribers. Reconnecting clients with a `Last-Event-ID`
// older than the ring's tail get a synthetic `replay.gap` frame instead
// of silent loss; clients with no header (first connect) skip replay.
//
// Bounds: the ring keeps at most maxFrames OR maxAge (whichever is
// reached first). Defaults: 10 000 frames / 5 minutes — plenty for
// transient disconnects (CI proxy hiccups, mobile blips, dashboard tab
// background-throttling) without taking on long-term storage.
type EventRing struct {
	mu        sync.RWMutex
	frames    []Frame
	maxFrames int
	maxAge    time.Duration
	seq       uint64

	// subs is the live-subscriber set. Each subscriber gets a buffered
	// channel; if the channel fills, the oldest frame is dropped (slow
	// client falls behind, the publisher never blocks).
	subsMu sync.Mutex
	subs   map[int]chan Frame
	nextID int
}

// NewEventRing returns a fresh ring with the supplied bounds. Pass
// zero/negative values to use the defaults.
func NewEventRing(maxFrames int, maxAge time.Duration) *EventRing {
	if maxFrames <= 0 {
		maxFrames = 10_000
	}
	if maxAge <= 0 {
		maxAge = 5 * time.Minute
	}
	return &EventRing{
		maxFrames: maxFrames,
		maxAge:    maxAge,
		frames:    make([]Frame, 0, maxFrames),
		subs:      map[int]chan Frame{},
	}
}

// Publish appends a new frame to the ring with a freshly-allocated id,
// evicts any over-bound frames, and fans out to every live subscriber.
// Returns the frame as published.
func (r *EventRing) Publish(typ string, payload []byte) Frame {
	id := atomic.AddUint64(&r.seq, 1)
	f := Frame{ID: id, Type: typ, Payload: payload, Created: time.Now()}

	r.mu.Lock()
	r.frames = append(r.frames, f)
	r.evictLocked()
	r.mu.Unlock()

	r.fanOut(f)
	return f
}

// evictLocked drops frames that exceed maxFrames or maxAge. Caller
// must hold r.mu (write).
func (r *EventRing) evictLocked() {
	if len(r.frames) > r.maxFrames {
		r.frames = r.frames[len(r.frames)-r.maxFrames:]
	}
	if r.maxAge > 0 && len(r.frames) > 0 {
		cutoff := time.Now().Add(-r.maxAge)
		drop := 0
		for _, f := range r.frames {
			if f.Created.Before(cutoff) {
				drop++
				continue
			}
			break
		}
		if drop > 0 {
			r.frames = r.frames[drop:]
		}
	}
}

// Subscribe registers a live subscriber. Returns a buffered channel of
// new frames + a cancel func that the caller MUST call to release the
// slot.
func (r *EventRing) Subscribe(buffer int) (id int, ch <-chan Frame, cancel func()) {
	if buffer <= 0 {
		buffer = 256
	}
	c := make(chan Frame, buffer)
	r.subsMu.Lock()
	r.nextID++
	id = r.nextID
	r.subs[id] = c
	r.subsMu.Unlock()
	cancel = func() {
		r.subsMu.Lock()
		if existing, ok := r.subs[id]; ok && existing == c {
			delete(r.subs, id)
			close(c)
		}
		r.subsMu.Unlock()
	}
	return id, c, cancel
}

// fanOut delivers a frame to every live subscriber. Slow subscribers
// (full channels) drop their oldest frame to keep the publisher
// non-blocking. The dropped frames are recoverable via Last-Event-ID
// reconnect.
func (r *EventRing) fanOut(f Frame) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	for _, c := range r.subs {
		select {
		case c <- f:
		default:
			select {
			case <-c:
			default:
			}
			select {
			case c <- f:
			default:
			}
		}
	}
}

// SinceResult is the data needed to reply to a reconnecting client:
// any frames newer than `lastID` (in chronological order), and a hint
// about whether the requested ID fell outside the ring (gap=true).
type SinceResult struct {
	// Frames are the recovered events with id > lastID.
	Frames []Frame
	// Gap is true when lastID is older than the ring's tail —
	// meaning some frames are gone and the caller should emit a
	// synthetic replay.gap event before the recovered frames.
	Gap bool
	// MissedFrom / MissedTo bound the lost id range when Gap is true.
	// MissedFrom = lastID + 1; MissedTo = ringTail.id - 1.
	MissedFrom uint64
	MissedTo   uint64
}

// Since returns the replay payload for a reconnecting client. Pass
// `lastID` = 0 to mean "no Last-Event-ID header" — the caller skips
// replay entirely and just streams live frames.
func (r *EventRing) Since(lastID uint64) SinceResult {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if lastID == 0 || len(r.frames) == 0 {
		return SinceResult{}
	}
	tail := r.frames[0].ID
	// Reconnect within the live window: replay frames with id > lastID.
	if lastID >= tail-1 {
		out := make([]Frame, 0, 16)
		for _, f := range r.frames {
			if f.ID > lastID {
				out = append(out, f)
			}
		}
		return SinceResult{Frames: out}
	}
	// Out of window: the client's lastID is older than the ring's tail.
	missedFrom := lastID + 1
	missedTo := tail - 1
	out := make([]Frame, 0, len(r.frames))
	out = append(out, r.frames...)
	return SinceResult{
		Frames:     out,
		Gap:        true,
		MissedFrom: missedFrom,
		MissedTo:   missedTo,
	}
}

// ParseLastEventID returns the uint64 from the SSE Last-Event-ID
// header, or 0 if the header is absent / malformed (no replay).
func ParseLastEventID(h string) uint64 {
	if h == "" {
		return 0
	}
	n, err := strconv.ParseUint(h, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
