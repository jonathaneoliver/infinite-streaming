package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/har"
)

// HARBuildFilter scopes which entries land in the HAR document. Empty
// PlayID + IncludeAllPlays=false means "most recent play_id only", which
// is the default for the live timeline endpoint.
type HARBuildFilter struct {
	PlayID          string
	IncludeAllPlays bool
	SinceWindow     time.Duration
}

// buildHARForSession reads the session's network ring buffer and returns a HAR
// document. Caller is expected to have already validated the session exists.
// `context` is optional — when non-nil, it lands at log._extensions.context.
func (a *App) buildHARForSession(session SessionData, incident *har.Incident, filter HARBuildFilter, context *har.Context) har.HAR {
	sessionID := getString(session, "session_id")

	a.networkLogsMu.RLock()
	rb, exists := a.networkLogs[sessionID]
	a.networkLogsMu.RUnlock()

	// Resolve the play_id we'll filter on, unless the caller opts out.
	resolvedPlayID := filter.PlayID
	if !filter.IncludeAllPlays && resolvedPlayID == "" && exists {
		resolvedPlayID = mostRecentPlayID(rb.GetAll())
	}

	var sources []har.Source
	var playStartedAt time.Time
	if exists {
		entries := rb.GetAll()
		// Compute the time floor when SinceWindow is set: drop entries
		// whose timestamp is older than (latest_entry_time - window).
		var sinceFloor time.Time
		if filter.SinceWindow > 0 && len(entries) > 0 {
			latest := entries[0].Timestamp
			for _, e := range entries {
				if e.Timestamp.After(latest) {
					latest = e.Timestamp
				}
			}
			sinceFloor = latest.Add(-filter.SinceWindow)
		}
		sources = make([]har.Source, 0, len(entries))
		for _, e := range entries {
			if !filter.IncludeAllPlays && resolvedPlayID != "" && e.PlayID != resolvedPlayID {
				continue
			}
			if !sinceFloor.IsZero() && e.Timestamp.Before(sinceFloor) {
				continue
			}
			if playStartedAt.IsZero() || e.Timestamp.Before(playStartedAt) {
				playStartedAt = e.Timestamp
			}
			sources = append(sources, har.Source{
				Timestamp:       e.Timestamp,
				Method:          e.Method,
				URL:             e.URL,
				RequestKind:     e.RequestKind,
				Status:          e.Status,
				BytesIn:         e.BytesIn,
				BytesOut:        e.BytesOut,
				ContentType:     e.ContentType,
				ClientWaitMs:    e.ClientWaitMs,
				TransferMs:      e.TransferMs,
				TotalMs:         e.TotalMs,
				UpstreamURL:     e.UpstreamURL,
				DNSMs:           e.DNSMs,
				ConnectMs:       e.ConnectMs,
				TLSMs:           e.TLSMs,
				TTFBMs:          e.TTFBMs,
				RequestHeaders:  toNameValueSlice(e.RequestHeaders),
				ResponseHeaders: toNameValueSlice(e.ResponseHeaders),
				QueryString:     toNameValueSlice(e.QueryString),
				Faulted:         e.Faulted,
				FaultType:       e.FaultType,
				FaultAction:     e.FaultAction,
				FaultCategory:   e.FaultCategory,
			})
		}
	}

	opts := har.BuildOptions{
		SessionID: sessionID,
		PlayerID:  getString(session, "player_id"),
		GroupID:   getString(session, "group_id"),
		Incident:  incident,
		Context:   context,
	}
	if incident != nil {
		if incident.PlayerID == "" {
			incident.PlayerID = opts.PlayerID
		}
		if incident.SessionID == "" {
			incident.SessionID = sessionID
		}
		if incident.GroupID == "" {
			incident.GroupID = opts.GroupID
		}
		// Surface play_id scope in the incident block so HAR consumers
		// know what they're looking at without inspecting individual
		// entries' URLs.
		if incident.Metadata == nil {
			incident.Metadata = map[string]interface{}{}
		}
		if resolvedPlayID != "" {
			incident.Metadata["play_id"] = resolvedPlayID
		}
		if filter.IncludeAllPlays {
			incident.Metadata["include_all_plays"] = true
		}
		if !playStartedAt.IsZero() {
			incident.Metadata["play_started_at"] = playStartedAt.UTC().Format(time.RFC3339Nano)
		}
	}

	return har.Build(sources, opts)
}

// toNameValueSlice converts a []HeaderPair (main package's HTTP capture
// type) to a []har.NameValue without sharing memory. Returns nil for an
// empty slice so the HAR builder can apply its own defaults.
func toNameValueSlice(in []HeaderPair) []har.NameValue {
	if len(in) == 0 {
		return nil
	}
	out := make([]har.NameValue, len(in))
	for i, p := range in {
		out[i] = har.NameValue{Name: p.Name, Value: p.Value}
	}
	return out
}

// mostRecentPlayID walks the ring buffer newest-first looking for the
// last play_id seen. Empty string if no entry carried one (e.g., older
// players that don't yet emit play_id query param).
func mostRecentPlayID(entries []NetworkLogEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].PlayID != "" {
			return entries[i].PlayID
		}
	}
	return ""
}

// findSessionByID returns the session map matching session_id, or nil.
func (a *App) findSessionByID(sessionID string) SessionData {
	for _, s := range a.getSessionList() {
		if getString(s, "session_id") == sessionID {
			return s
		}
	}
	return nil
}

// handleGetSessionTimelineHAR fetches the live timeline for a player_id as HAR.
// GET /api/sessions/{player_id}/timeline.har
//
// This is the on-demand path — it builds a HAR from the in-memory ring
// buffer at request time. For archived / historical timelines, use the
// analytics tier's /analytics/api/session_bundle (which produces a ZIP
// with snapshots + HAR + README).
func (a *App) handleGetSessionTimelineHAR(w http.ResponseWriter, r *http.Request) {
	playerID := mux.Vars(r)["player_id"]
	if playerID == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "player_id required"})
		return
	}

	sessions := a.getSessionList()
	session := findSessionByPlayerID(sessions, playerID)
	if session == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "session not found for player_id"})
		return
	}

	// timeline.har accepts ?play_id=X (specific play) and
	// ?include_all_plays=1 (forensic). Default: most-recent play_id.
	filter := HARBuildFilter{
		PlayID:          strings.TrimSpace(r.URL.Query().Get("play_id")),
		IncludeAllPlays: strings.EqualFold(r.URL.Query().Get("include_all_plays"), "1") || strings.EqualFold(r.URL.Query().Get("include_all_plays"), "true"),
	}
	doc := a.buildHARForSession(session, nil, filter, nil)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="timeline-%s.har"`, safeFilename(playerID)))
	_ = json.NewEncoder(w).Encode(doc)
}

// safeFilename trims any character likely to confuse a shell or
// download manager so it's safe to splice into Content-Disposition.
func safeFilename(s string) string {
	if s == "" {
		return "unknown"
	}
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		}
		return '_'
	}
	out := strings.Map(repl, s)
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}
