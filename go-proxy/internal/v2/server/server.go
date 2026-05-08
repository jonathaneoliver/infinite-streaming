// Package server implements the v2 harness HTTP API.
//
// The exported Server type satisfies oapigen.ServerInterface generated
// from api/openapi/v2/proxy.yaml. v2 mounts under /api/v2/...; v1 paths
// continue to work unchanged on the same router. See DESIGN.md for the
// design principles.
//
// Phases delivered:
//   - A: codegen pipeline + 501 stubs
//   - B: read-only handlers backed by V1Adapter
//   - C: concurrency primitives (FieldRevisions, Merge Patch, ETag)
//   - D: mutations (PATCH players, DELETE single & all, label round-trip)
//
// Pending: synthetic POST upsert (needs v1 port allocator), shape /
// fault_rules translation (v1 storage shape doesn't map 1:1), unified
// /events SSE replay, group auto-broadcast.
package server

import (
	"sync"

	"github.com/gorilla/mux"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// Server implements oapigen.ServerInterface.
type Server struct {
	v1 V1Adapter

	// Per-player field-revision trackers, keyed by player_id (canonical
	// lowercase UUID). Lazily created on first Patch / first observed
	// player and never garbage-collected — accepted leak for Phase D
	// (typical session population is bounded; cleanup lands when the
	// adapter exposes a deletion hook).
	revsMu sync.Mutex
	revs   map[string]*FieldRevisions

	// events owns the v2 SSE pipeline (subscription + transform + ring).
	// Nil-safe: the GET /api/v2/events handler returns 503 if absent.
	events *EventSource
}

// New returns a Server backed by a V1Adapter (typically *App in package
// main). Pass a nil adapter to mount the 501-stub variant — useful for
// tests that exercise the wiring without a real proxy backing it.
//
// New also boots the EventSource that powers /api/v2/events. Call
// Close on the returned Server to shut it down cleanly.
func New(v1 V1Adapter) *Server {
	ring := NewEventRing(0, 0)
	return &Server{
		v1:     v1,
		revs:   map[string]*FieldRevisions{},
		events: NewEventSource(v1, ring),
	}
}

// Close releases the EventSource subscriptions. Safe to call on a nil
// Server. Idempotent.
func (s *Server) Close() {
	if s == nil {
		return
	}
	s.events.Close()
}

// fieldRevs returns the (lazily-allocated) per-field revision tracker
// for one player. Callers should treat the returned pointer as
// process-lived.
func (s *Server) fieldRevs(playerID string) *FieldRevisions {
	s.revsMu.Lock()
	defer s.revsMu.Unlock()
	fr, ok := s.revs[playerID]
	if !ok {
		fr = NewFieldRevisions()
		s.revs[playerID] = fr
	}
	return fr
}

// dropFieldRevs is called when a player is deleted via v2 to free its
// revision tracker. v1-side deletes don't reach this — accepted leak.
func (s *Server) dropFieldRevs(playerID string) {
	s.revsMu.Lock()
	delete(s.revs, playerID)
	s.revsMu.Unlock()
}

// Mount registers every v2 route on the supplied gorilla/mux router under
// /api/v2/. Call once during proxy startup, before any catch-all handler.
func Mount(r *mux.Router, s *Server) {
	oapigen.HandlerFromMux(s, r)
}
