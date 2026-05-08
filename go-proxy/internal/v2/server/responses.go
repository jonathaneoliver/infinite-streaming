package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// writeJSON encodes body as JSON with the supplied status. Always sets
// Content-Type to application/json.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeProblem writes an RFC 7807 problem+json response. extra fields
// (e.g. existing_player_id, current_revision, conflicts) are merged
// into the body so handlers can return endpoint-specific error context.
func writeProblem(w http.ResponseWriter, status int, problemType, title, detail string, extra map[string]any) {
	body := map[string]any{
		"type":   problemType,
		"title":  title,
		"status": status,
	}
	if detail != "" {
		body["detail"] = detail
	}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// notImplemented writes an RFC 7807 problem with status 501. Stub for
// endpoints whose handler lands in a later phase.
func notImplemented(w http.ResponseWriter, opID string) {
	writeProblem(
		w,
		http.StatusNotImplemented,
		"https://harness/errors/not-implemented",
		"v2 endpoint not implemented yet",
		fmt.Sprintf("operation %q is part of v2 scaffolding; handler lands in a later phase", opID),
		map[string]any{"operation": opID},
	)
}
