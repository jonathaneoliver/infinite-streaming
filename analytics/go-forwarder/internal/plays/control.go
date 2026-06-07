package plays

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ControlEventsFilter narrows a control_events query. At least one of
// PlayerID / PlayID / Events / Labels.Has must be set (so the query can
// never scan the whole table); the rest are optional narrowings. PlayerID
// is the usual key, but a global / session-less event (e.g. server_start,
// which carries no player_id, #671) is reachable via Events or Labels.Has.
type ControlEventsFilter struct {
	PlayerID string
	PlayID   string
	Events   []string // filter to these event names (OR); enables session-less lookups
	From     string   // CH datetime string
	To       string
	Labels   LabelFilter
	Limit    int // 0 = use default (1000)
}

const (
	defaultControlEventsLimit = 1000
	maxControlEventsLimit     = 10000
)

// GetControlEvents returns control_events rows for a player, in the
// same shape /api/v2/control_events serves today. Each row is a
// map[string]any so the JSON output stays byte-identical.
//
// Sort is ascending by ts (matches the dashboard's PlayLog rendering
// order). Caller is expected to canonicalise PlayerID/PlayID before
// calling — matches the HTTP handler's contract.
func GetControlEvents(ctx context.Context, b Backend, f ControlEventsFilter) ([]map[string]any, error) {
	// At least one discriminating predicate, else we'd scan the whole table.
	if f.PlayerID == "" && f.PlayID == "" && len(f.Events) == 0 && len(f.Labels.Has) == 0 {
		return nil, errors.New("one of player_id, play_id, event, or labels_has required")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultControlEventsLimit
	}
	if limit > maxControlEventsLimit {
		limit = maxControlEventsLimit
	}

	params := map[string]string{"limit": fmt.Sprintf("%d", limit)}
	var where []string
	if f.PlayerID != "" {
		params["player_id"] = f.PlayerID
		where = append(where, "player_id = {player_id:String}")
	}
	if f.PlayID != "" {
		params["play_id"] = f.PlayID
		where = append(where, "play_id = {play_id:String}")
	}
	if len(f.Events) > 0 {
		names := make([]string, len(f.Events))
		for i, ev := range f.Events {
			key := "event_" + fmt.Sprintf("%d", i)
			params[key] = ev
			names[i] = "{" + key + ":String}"
		}
		where = append(where, "event IN ("+strings.Join(names, ", ")+")")
	}
	if f.From != "" {
		params["from"] = f.From
		where = append(where, "ts >= parseDateTime64BestEffortOrNull({from:String}, 3)")
	}
	if f.To != "" {
		params["to"] = f.To
		where = append(where, "ts <= parseDateTime64BestEffortOrNull({to:String}, 3)")
	}
	where, params = f.Labels.applyTo(where, params, "labels")

	query := "SELECT ts, player_id, play_id, attempt_id, session_id, source, event, info, labels, event_fingerprint, classification " +
		"FROM " + b.Database + ".control_events WHERE " +
		strings.Join(where, " AND ") +
		" ORDER BY ts ASC LIMIT {limit:UInt32}"
	return b.queryRows(ctx, query, params)
}
