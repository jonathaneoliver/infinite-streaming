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

// GetApiV2Players lists every connected player.
func (s *Server) GetApiV2Players(w http.ResponseWriter, r *http.Request, params oapigen.GetApiV2PlayersParams) {
	if s.v1 == nil {
		writeJSON(w, http.StatusOK, map[string]any{"items": []oapigen.PlayerRecord{}})
		return
	}
	sessions := s.v1.SessionList()
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

// ----- Plays (reads) -------------------------------------------------------

// GetApiV2PlaysPlayId returns one live play record.
//
// Phase B: v1 has no first-class play resource — play_id is captured
// per network request but not promoted to a session-level field.
// Returning 404 unconditionally until Phase E surfaces play boundaries
// from the SSE event stream and the v1 store grows a play index.
func (*Server) GetApiV2PlaysPlayId(w http.ResponseWriter, r *http.Request, playId oapigen.PlayId) {
	writeProblem(
		w,
		http.StatusNotFound,
		"https://harness/errors/play-not-found",
		"play not found",
		"live play lookup is not yet wired in this build; query the analytics archive at /analytics/api/v2/plays/{play_id} for finished plays",
		map[string]any{"play_id": playId.String()},
	)
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
