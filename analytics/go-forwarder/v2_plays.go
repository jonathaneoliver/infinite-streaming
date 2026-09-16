package main

// v2 plays endpoints — see api/openapi/v2/forwarder.yaml § /api/v2/plays.
//
// Replaces v1's /api/sessions for the dashboard's session-picker use
// case. v1 grouped by (session_id, play_id) and accepted only
// since/until; v2 groups by play_id, accepts player_id / play_id /
// classification / from / to / limit filters, and wraps the result in
// the v2 envelope ({items, next_cursor}).
//
// PATCH /api/v2/plays/{play_id} writes the tiered-retention
// classification (#342). Live-play mutations (labels / shape /
// fault_rules) live on go-proxy's PATCH /api/v2/plays/{id} — this
// forwarder handler exists because classification only applies once a
// play is archived in ClickHouse.
//
// Domain logic — labels-aware aggregation, classification mutation,
// auto-classifier predicate — lives in internal/plays. The handlers
// here are thin shims that parse the HTTP request, call the domain
// function, and serialise the result.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder/internal/plays"
)

func mountV2PlaysHandlers(mux *http.ServeMux, cfg config) {
	// net/http's ServeMux treats trailing-slash and no-trailing-slash as
	// distinct patterns: exact `/api/v2/plays` matches only the list URL,
	// `/api/v2/plays/` is a prefix that catches `{play_id}` and anything
	// beneath it. Register both so a request to either lands in the
	// dispatcher below.
	mux.HandleFunc("/api/v2/plays", playsDispatcher(cfg))
	mux.HandleFunc("/api/v2/plays/", playsDispatcher(cfg))
}

func playsDispatcher(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/v2/plays")
		switch {
		case rest == "" || rest == "/":
			v2PlaysListHandler(w, r, cfg)
		case strings.HasPrefix(rest, "/"):
			playID := strings.TrimPrefix(rest, "/")
			if strings.ContainsRune(playID, '/') {
				writeProblemv2(w, http.StatusNotFound, "not found", "no nested resources under /api/v2/plays/{play_id} yet")
				return
			}
			if r.Method == http.MethodPatch {
				v2PlayPatchHandler(w, r, cfg, canonicalV2ID(playID))
				return
			}
			v2PlayDetailHandler(w, r, cfg, playID)
		default:
			writeProblemv2(w, http.StatusNotFound, "not found", "")
		}
	}
}

// v2PlaysListHandler answers GET /api/v2/plays.
func v2PlaysListHandler(w http.ResponseWriter, r *http.Request, cfg config) {
	q := r.URL.Query()
	// Canonicalise — see canonicalV2ID()'s doc. Operators who curl
	// the endpoint with an uppercase UUID would otherwise see an
	// empty page silently.
	labelHas, labelNot := readLabelFilters(q)
	filter := plays.PlayFilter{
		PlayerID:       canonicalV2ID(q.Get("player_id")),
		PlayID:         canonicalV2ID(q.Get("play_id")),
		AttemptID:      q.Get("attempt_id"),
		GroupID:        q.Get("group"),
		From:           q.Get("from"),
		To:             q.Get("to"),
		Classification: q.Get("classification"),
		Labels:         plays.LabelFilter{Has: labelHas, Not: labelNot},
		Limit:          parseLimit(q.Get("limit"), 500, 5000),
	}
	rows, err := plays.FindPlays(r.Context(), playsBackend(cfg), filter)
	if err != nil {
		// classification validation rejects with a plain Go error from
		// the domain layer; map it to a 400 instead of a generic 502 so
		// curl operators see "bad request" not "clickhouse query failed".
		if strings.Contains(err.Error(), "classification must be") {
			writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
			return
		}
		writeProblemv2(w, http.StatusBadGateway, "clickhouse query failed", err.Error())
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	writeJSONv2(w, http.StatusOK, map[string]any{
		"items":       rows,
		"next_cursor": nil,
	})
}

// v2PlayDetailHandler answers GET /api/v2/plays/{play_id}. Returns
// the same PlaySummary shape (no embedded events_summary /
// network_summary / _links yet — those are spec'd under PlayDetail
// for a later PR).
func v2PlayDetailHandler(w http.ResponseWriter, r *http.Request, cfg config, playID string) {
	if playID == "" {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "play_id required")
		return
	}
	row, err := plays.GetPlaySummary(r.Context(), playsBackend(cfg), playID)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "clickhouse query failed", err.Error())
		return
	}
	if row == nil {
		writeProblemv2(w, http.StatusNotFound, "not found", fmt.Sprintf("play %q has no archived snapshots", playID))
		return
	}
	writeJSONv2(w, http.StatusOK, row)
}

// v2PlayPatchHandler answers PATCH /api/v2/plays/{play_id}. Today the
// only supported field is `classification`, which drives the tiered
// retention TTL (#342). Star = `favourite`; unstar = `auto` which
// re-runs the auto-classifier and writes whatever it returns
// (`interesting` | `other`). Explicit values `interesting` /
// `other` are also accepted for operator-driven reclassification.
func v2PlayPatchHandler(w http.ResponseWriter, r *http.Request, cfg config, playID string) {
	if playID == "" {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "play_id required")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "read body failed")
		return
	}
	var patch map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &patch); err != nil {
			writeProblemv2(w, http.StatusBadRequest, "bad request", "invalid json body")
			return
		}
	}
	if len(patch) == 0 {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "empty patch")
		return
	}
	for k := range patch {
		if k != "classification" {
			writeProblemv2(w, http.StatusNotImplemented, "not implemented",
				fmt.Sprintf("PATCH /api/v2/plays only supports {classification} today; got %q", k))
			return
		}
	}
	clsRaw, ok := patch["classification"].(string)
	if !ok {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "classification must be a string")
		return
	}
	cls := strings.ToLower(strings.TrimSpace(clsRaw))
	be := playsBackend(cfg)
	switch cls {
	case plays.ClassificationFavourite, plays.ClassificationInteresting, plays.ClassificationOther:
		if err := plays.SetPlayClassification(r.Context(), be, playID, cls, true); err != nil {
			writeProblemv2(w, http.StatusBadGateway, "clickhouse update failed", err.Error())
			return
		}
	case "auto":
		if err := plays.ReclassifyPlay(r.Context(), be, playID, true); err != nil {
			writeProblemv2(w, http.StatusBadGateway, "clickhouse update failed", err.Error())
			return
		}
	default:
		writeProblemv2(w, http.StatusBadRequest, "bad request",
			"classification must be one of: favourite, interesting, other, auto")
		return
	}
	// Round-trip the post-patch detail so the caller sees the settled
	// classification (auto mode runs the predicate before returning).
	row, err := plays.GetPlaySummary(r.Context(), be, playID)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "clickhouse query failed", err.Error())
		return
	}
	if row == nil {
		// Mutation went through but the play has no archived snapshots
		// yet — surface the requested classification so the UI can flip
		// optimistically. (Rare: the row would have to have been GC'd
		// between the ALTER UPDATE and the SELECT.)
		writeJSONv2(w, http.StatusOK, map[string]any{
			"play_id":        playID,
			"classification": cls,
		})
		return
	}
	writeJSONv2(w, http.StatusOK, row)
}
