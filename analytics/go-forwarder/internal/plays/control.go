package plays

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ControlEventsFilter narrows a control_events query. PlayerID is
// required (the table is indexed on it and every operational view
// scopes to a player); the other fields are optional narrowings.
type ControlEventsFilter struct {
	PlayerID string
	PlayID   string
	From     string // CH datetime string
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
	if f.PlayerID == "" {
		return nil, errors.New("player_id required")
	}
	limit := f.Limit
	if limit <= 0 {
		limit = defaultControlEventsLimit
	}
	if limit > maxControlEventsLimit {
		limit = maxControlEventsLimit
	}

	params := map[string]string{
		"player_id": f.PlayerID,
		"limit":     fmt.Sprintf("%d", limit),
	}
	where := []string{"player_id = {player_id:String}"}
	if f.PlayID != "" {
		params["play_id"] = f.PlayID
		where = append(where, "play_id = {play_id:String}")
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
