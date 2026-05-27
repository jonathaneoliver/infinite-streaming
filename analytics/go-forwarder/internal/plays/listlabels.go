package plays

import (
	"context"
	"fmt"
)

// LabelCount is one row of the distinct-labels-with-counts aggregate.
type LabelCount struct {
	Label string `json:"label"`
	Count uint64 `json:"count"`
}

// LabelListFilter narrows the ListLabels scan.
type LabelListFilter struct {
	From  string // CH datetime string; default 24h ago via SQL
	To    string // CH datetime string; default now via SQL
	Like  string // optional SQL LIKE pattern to narrow ("critical=%", "%stall%"); empty = all
	Limit int    // default 200; max 2000
}

const (
	defaultLabelsLimit = 200
	maxLabelsLimit     = 2000
)

// ListLabels returns the distinct labels observed across the three
// label-carrying tables (session_events + network_requests +
// control_events) within the time window, with their occurrence
// counts (sum across the three tables). Sorted by count DESC.
//
// Cheap enough to call as a discovery step: 24h of labels is
// typically <500 distinct strings even on a busy day.
func ListLabels(ctx context.Context, b Backend, f LabelListFilter) ([]LabelCount, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = defaultLabelsLimit
	}
	if limit > maxLabelsLimit {
		limit = maxLabelsLimit
	}

	params := map[string]string{}
	timeClause := "ts >= now() - INTERVAL 24 HOUR"
	if f.From != "" {
		params["from"] = f.From
		if f.To != "" {
			params["to"] = f.To
			timeClause = "ts >= parseDateTime64BestEffort({from:String}) AND ts < parseDateTime64BestEffort({to:String})"
		} else {
			timeClause = "ts >= parseDateTime64BestEffort({from:String})"
		}
	}

	outerWhere := ""
	if f.Like != "" {
		params["like"] = escapeLikeUnderscores(f.Like)
		outerWhere = "WHERE label LIKE {like:String}"
	}

	query := fmt.Sprintf(`
		SELECT label, sum(c) AS count FROM (
		  SELECT arrayJoin(labels) AS label, count() AS c
		  FROM %s.%s
		  WHERE %s
		  GROUP BY label
		  UNION ALL
		  SELECT arrayJoin(labels) AS label, count() AS c
		  FROM %s.network_requests
		  WHERE %s
		  GROUP BY label
		  UNION ALL
		  SELECT arrayJoin(labels) AS label, count() AS c
		  FROM %s.control_events
		  WHERE %s
		  GROUP BY label
		) u
		%s
		GROUP BY label
		ORDER BY count DESC, label ASC
		LIMIT %d
		FORMAT JSONEachRow`,
		b.Database, b.EventsTable, timeClause,
		b.Database, timeClause,
		b.Database, timeClause,
		outerWhere,
		limit,
	)

	rows, err := b.queryRows(ctx, query, params)
	if err != nil {
		return nil, err
	}
	out := make([]LabelCount, 0, len(rows))
	for _, r := range rows {
		label, _ := r["label"].(string)
		if label == "" {
			continue
		}
		var c uint64
		switch v := r["count"].(type) {
		case float64:
			c = uint64(v)
		case string:
			// CH renders UInt64 as JSON string — re-parse.
			_, _ = fmt.Sscanf(v, "%d", &c)
		}
		out = append(out, LabelCount{Label: label, Count: c})
	}
	return out, nil
}
