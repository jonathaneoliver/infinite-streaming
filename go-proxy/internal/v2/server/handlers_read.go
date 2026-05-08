package server

import (
	"net/http"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// ----- Diagnostics ---------------------------------------------------------

// GetApiV2Healthz is a real liveness probe.
func (*Server) GetApiV2Healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// GetApiV2Info returns build / config introspection.
func (s *Server) GetApiV2Info(w http.ResponseWriter, r *http.Request) {
	versions := []string{"v1", "v2"}
	resp := oapigen.Info{
		ApiVersions: &versions,
	}
	if s.v1 != nil {
		v := s.v1.Version()
		resp.Version = &v
		c := s.v1.ContentDir()
		resp.ContentDir = &c
		ae := s.v1.AuthEnabled()
		resp.AuthEnabled = &ae
		an := s.v1.AnalyticsEnabled()
		resp.AnalyticsEnabled = &an
	}
	writeJSON(w, http.StatusOK, resp)
}

// ----- Players (reads) -----------------------------------------------------

// wantsRaw reports whether the caller asked for the v1-shape full
// session map alongside the typed v2 record. Read off the raw URL
// (the typed params don't include this transitional flag).
//
// The `?include=raw` flag is a transitional concession to the existing
// v1 dashboard JS during the v1→v2 UI migration. New clients should
// not depend on it; it will go away once the dashboard fully consumes
// typed v2 fields.
func wantsRaw(r *http.Request) bool {
	return r.URL.Query().Get("include") == "raw"
}

// playerRecordWithRaw is the response shape when ?include=raw is set.
// Anonymous-embeds PlayerRecord so the typed v2 fields serialize
// at the top level; raw_session adds the v1 SessionData passthrough.
type playerRecordWithRaw struct {
	oapigen.PlayerRecord
	RawSession map[string]any `json:"raw_session,omitempty"`
}

// GetApiV2Players lists every connected player.
func (s *Server) GetApiV2Players(w http.ResponseWriter, r *http.Request, params oapigen.GetApiV2PlayersParams) {
	if s.v1 == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []oapigen.PlayerRecord{}})
		return
	}
	raw := wantsRaw(r)
	sessions := s.v1.SessionList()
	if raw {
		items := make([]playerRecordWithRaw, 0, len(sessions))
		for _, sess := range sessions {
			rec, ok := playerFromSession(sess)
			if !ok {
				continue
			}
			items = append(items, playerRecordWithRaw{PlayerRecord: rec, RawSession: sess})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
		return
	}
	items := make([]oapigen.PlayerRecord, 0, len(sessions))
	for _, sess := range sessions {
		rec, ok := playerFromSession(sess)
		if !ok {
			continue
		}
		items = append(items, rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// GetApiV2PlayersPlayerId returns one player record.
func (s *Server) GetApiV2PlayersPlayerId(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId) {
	if s.v1 == nil {
		writePlayerNotFound(w, playerId.String())
		return
	}
	sess, ok := s.v1.SessionByPlayerID(playerId.String())
	if !ok {
		writePlayerNotFound(w, playerId.String())
		return
	}
	rec, ok := playerFromSession(sess)
	if !ok {
		writePlayerNotFound(w, playerId.String())
		return
	}
	if rec.ControlRevision != "" {
		w.Header().Set("ETag", `"`+rec.ControlRevision+`"`)
	}
	if wantsRaw(r) {
		writeJSON(w, http.StatusOK, playerRecordWithRaw{PlayerRecord: rec, RawSession: sess})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// GetApiV2PlayersPlayerIdNetwork returns the player's network ring buffer.
func (s *Server) GetApiV2PlayersPlayerIdNetwork(w http.ResponseWriter, r *http.Request, playerId oapigen.PlayerId, params oapigen.GetApiV2PlayersPlayerIdNetworkParams) {
	limit := 200
	if params.Limit != nil {
		limit = *params.Limit
	}
	if s.v1 == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []oapigen.NetworkLogEntry{}})
		return
	}
	if _, ok := s.v1.SessionByPlayerID(playerId.String()); !ok {
		writePlayerNotFound(w, playerId.String())
		return
	}
	rows := s.v1.NetworkLogForPlayer(playerId.String(), limit)
	items := make([]oapigen.NetworkLogEntry, 0, len(rows))
	for _, row := range rows {
		items = append(items, networkEntryFromV1(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// ----- Player groups (reads) -----------------------------------------------

// GetApiV2PlayerGroups lists every active group.
func (s *Server) GetApiV2PlayerGroups(w http.ResponseWriter, r *http.Request) {
	if s.v1 == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []oapigen.PlayerGroup{}})
		return
	}
	groups := groupsFromSessions(s.v1.SessionList())
	writeJSON(w, http.StatusOK, map[string]any{"items": groups})
}

// GetApiV2PlayerGroupsGroupId returns one group.
func (s *Server) GetApiV2PlayerGroupsGroupId(w http.ResponseWriter, r *http.Request, groupId oapigen.GroupId) {
	if s.v1 == nil {
		writeGroupNotFound(w, groupId.String())
		return
	}
	groups := groupsFromSessions(s.v1.SessionList())
	for _, g := range groups {
		if g.Id == groupId {
			writeJSON(w, http.StatusOK, g)
			return
		}
	}
	writeGroupNotFound(w, groupId.String())
}

// ----- 404 helpers ---------------------------------------------------------

func writePlayerNotFound(w http.ResponseWriter, playerID string) {
	writeProblem(
		w,
		http.StatusNotFound,
		"https://harness/errors/player-not-found",
		"player not found",
		"no player with that id is currently connected",
		map[string]any{"player_id": playerID},
	)
}

func writeGroupNotFound(w http.ResponseWriter, groupID string) {
	writeProblem(
		w,
		http.StatusNotFound,
		"https://harness/errors/group-not-found",
		"group not found",
		"no group with that id is currently active",
		map[string]any{"group_id": groupID},
	)
}
