package server

// Translation between v1 SessionData (map[string]any with ~80 fields)
// and v2 typed records. Only the keys consulted here are part of the
// v1→v2 contract; everything else in the v1 map is ignored.
//
// The functions below are read-only — they never mutate the input map.
// Callers (the read handlers in handlers_read.go) build the response
// from the returned typed record.

import (
	"strings"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// playerFromSession projects a v1 session record into a v2 PlayerRecord.
// Returns ok=false when the session has no player_id (v1 sessions
// occasionally land before the player has self-registered an ID).
//
// Phase B scope: identity, lifecycle, control_revision, labels (empty
// for now — v1 has no labels concept), origination_ip. Mutation-side
// fields (fault_rules, shape) come back nil/empty until Phase C/D
// shapes them properly. CurrentPlay is nil until Phase E surfaces
// play boundaries from the network log.
func playerFromSession(s map[string]any) (oapigen.PlayerRecord, bool) {
	rawPlayerID := getString(s, "player_id")
	if rawPlayerID == "" {
		return oapigen.PlayerRecord{}, false
	}
	playerUUID, err := uuid.Parse(rawPlayerID)
	if err != nil {
		return oapigen.PlayerRecord{}, false
	}

	rec := oapigen.PlayerRecord{
		Id:              playerUUID,
		DisplayId:       getInt(s, "session_number"),
		ControlRevision: getString(s, "control_revision"),
	}

	if ip := getString(s, "origination_ip"); ip != "" {
		rec.OriginationIp = &ip
	}
	if t, ok := getTime(s, "session_start_time", "first_request_time"); ok {
		rec.FirstSeenAt = &t
	}
	if t, ok := getTime(s, "updated_at", "last_request_time"); ok {
		rec.LastSeenAt = &t
	}
	return rec, true
}

// networkEntryFromV1 projects a v1 network ring-buffer row into a v2
// NetworkLogEntry. The v1 row is itself a map produced by the network
// log subsystem; only HAR-shaped fields are copied through.
func networkEntryFromV1(row map[string]any) oapigen.NetworkLogEntry {
	out := oapigen.NetworkLogEntry{}
	if v := getString(row, "method"); v != "" {
		out.Method = &v
	}
	if v := getString(row, "url"); v != "" {
		out.Url = &v
	}
	if v := getString(row, "upstream_url"); v != "" {
		out.UpstreamUrl = &v
	}
	if v := getString(row, "path"); v != "" {
		out.Path = &v
	}
	if v := getString(row, "request_kind"); v != "" {
		out.RequestKind = &v
	}
	if v := getString(row, "content_type"); v != "" {
		out.ContentType = &v
	}
	if v := getInt(row, "status"); v != 0 {
		out.Status = &v
	}
	// bytes_in / bytes_out: 0 is a real value (HEAD-style or empty body),
	// so surface the field whenever the key is present.
	if _, ok := row["bytes_in"]; ok {
		v := getInt(row, "bytes_in")
		out.BytesIn = &v
	}
	if _, ok := row["bytes_out"]; ok {
		v := getInt(row, "bytes_out")
		out.BytesOut = &v
	}
	if t, ok := getTime(row, "timestamp"); ok {
		out.Timestamp = &t
	}
	if v := getString(row, "play_id"); v != "" {
		if u, err := uuid.Parse(v); err == nil {
			out.PlayId = &u
		}
	}
	return out
}

// groupsFromSessions builds the live group set by walking every session
// and gathering the distinct group_id tags. Each tag becomes one
// PlayerGroup with members = players that share the tag.
//
// v1 has no separate group resource — group_id is just a string
// stored on each session. v2 surfaces it as a first-class collection.
func groupsFromSessions(sessions []map[string]any) []oapigen.PlayerGroup {
	byID := map[string]*oapigen.PlayerGroup{}
	order := []string{}
	for _, s := range sessions {
		gid := getString(s, "group_id")
		if gid == "" {
			continue
		}
		pid := getString(s, "player_id")
		if pid == "" {
			continue
		}
		playerUUID, err := uuid.Parse(pid)
		if err != nil {
			continue
		}
		g, exists := byID[gid]
		if !exists {
			groupUUID, err := stableGroupUUID(gid)
			if err != nil {
				continue
			}
			label := gid
			g = &oapigen.PlayerGroup{
				Id:              groupUUID,
				Label:           &label,
				MemberPlayerIds: []openapi_types.UUID{},
			}
			byID[gid] = g
			order = append(order, gid)
		}
		g.MemberPlayerIds = append(g.MemberPlayerIds, playerUUID)
	}
	out := make([]oapigen.PlayerGroup, 0, len(order))
	for _, gid := range order {
		out = append(out, *byID[gid])
	}
	return out
}

// stableGroupUUID maps a v1 string group_id (e.g. "G1234") to a
// deterministic v5 UUID under a fixed namespace, so the same group_id
// always produces the same v2 GroupId across requests.
//
// v5 chosen over hashing-then-format because it lands in a real UUID
// version slot — Scalar / clients won't reject it as malformed.
func stableGroupUUID(s string) (openapi_types.UUID, error) {
	// Namespace is the v5 namespace for the v2 group resource
	// (arbitrary but fixed; chosen to avoid collisions with the
	// standard URL/DNS namespaces).
	ns, err := uuid.Parse("d3a8c0d2-1c51-4b6a-9b3a-ff7e2f5b2aa1")
	if err != nil {
		return openapi_types.UUID{}, err
	}
	return uuid.NewSHA1(ns, []byte(s)), nil
}

// ----- Shared field accessors -----------------------------------------

func getString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	default:
		return ""
	}
}

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		// session_number sometimes round-trips through JSON as a
		// string ("3") rather than a numeric. Tolerate it.
		var n int
		for _, r := range x {
			if r < '0' || r > '9' {
				return 0
			}
			n = n*10 + int(r-'0')
		}
		return n
	default:
		return 0
	}
}

// getTime returns the first non-zero time parsable from any of the
// supplied keys. Tolerates RFC3339 / RFC3339Nano / unix-millis / time.Time.
func getTime(m map[string]any, keys ...string) (time.Time, bool) {
	if m == nil {
		return time.Time{}, false
	}
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch x := v.(type) {
		case time.Time:
			if !x.IsZero() {
				return x, true
			}
		case string:
			if x == "" {
				continue
			}
			// Trim whitespace/quoting we might see if a stamp got
			// re-marshalled through JSON.
			s := strings.TrimSpace(x)
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				return t, true
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t, true
			}
		case int64:
			if x > 0 {
				return time.UnixMilli(x), true
			}
		case float64:
			if x > 0 {
				return time.UnixMilli(int64(x)), true
			}
		}
	}
	return time.Time{}, false
}
