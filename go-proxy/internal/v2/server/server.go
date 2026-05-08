// Package server implements the v2 harness HTTP API.
//
// The exported Server type satisfies oapigen.ServerInterface generated
// from api/openapi/v2/proxy.yaml. v2 mounts under /api/v2/...; v1 paths
// continue to work unchanged on the same router. See DESIGN.md for the
// design principles.
//
// Phase A scaffolding: every endpoint stubbed with 501.
// Phase B (here): read-only handlers backed by V1Adapter.
// Phases C–F: concurrency primitives, mutations, SSE, group broadcast.
package server

import (
	"github.com/gorilla/mux"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// Server implements oapigen.ServerInterface.
type Server struct {
	v1 V1Adapter
}

// New returns a Server backed by a V1Adapter (typically *App in package
// main). Pass a nil adapter to mount the 501-stub variant — useful for
// tests that exercise the wiring without a real proxy backing it.
func New(v1 V1Adapter) *Server {
	return &Server{v1: v1}
}

// Mount registers every v2 route on the supplied gorilla/mux router under
// /api/v2/. Call once during proxy startup, before any catch-all handler.
func Mount(r *mux.Router, s *Server) {
	oapigen.HandlerFromMux(s, r)
}
