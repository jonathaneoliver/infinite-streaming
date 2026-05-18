package eventclass

// Stateless snapshot classifiers that fire on a single row's
// last_event marker. Ports the legacy events_query.go UNION ALL
// branches that read `FROM session_snapshots WHERE last_event = ...`
// without window functions or prev-state comparisons.

func init() {
	RegisterSnapshot("snapshot_state", snapshotStateClassifier{})
}

type snapshotStateClassifier struct{}

// Closed set of last_event markers the catch-all classifier should
// NOT re-emit (already covered by their own dedicated branches above
// or explicitly suppressed as noise). Same list as the NOT IN clause
// in the legacy SQL's catch-all UNION branch.
var knownLastEvents = map[string]struct{}{
	"heartbeat":             {},
	"state_change":          {},
	"playing":               {},
	"video_bitrate_change":  {},
	"stall_start":           {},
	"stall_end":             {},
	"buffering_start":       {},
	"buffering_end":         {},
	"frozen":                {},
	"segment_stall":         {},
	"restart":               {},
	"video_first_frame":     {},
	"video_start_time":      {},
	"rate_shift_down":       {},
	"rate_shift_up":         {},
	"timejump":              {},
	"error":                 {},
	"user_marked":           {},
}

func (snapshotStateClassifier) Classify(prev, cur *Snapshot) []Event {
	if cur == nil {
		return nil
	}
	// Issue #470: go-proxy is now 1:1 — every metrics POST from the
	// player produces exactly one frame, no debounced re-emission of
	// stale markers. Each call to this classifier corresponds to one
	// player event, so the edge-trigger guard the previous commit
	// added is no longer needed (and would suppress legitimate
	// back-to-back events of the same type from a chatty player).
	base := Event{
		Ts: cur.Ts, PlayerID: cur.PlayerID, PlayID: cur.PlayID,
		AttemptID: cur.AttemptID, SessionID: cur.SessionID,
		Classification: cur.Classification,
	}
	var out []Event
	switch cur.LastEvent {
	case "frozen":
		e := base
		e.Type = TypeStall
		e.Info = "(frozen)"
		out = append(out, e)
	case "segment_stall":
		e := base
		e.Type = TypeStall
		e.Info = "(segment)"
		out = append(out, e)
	case "restart":
		e := base
		e.Type = TypeRestart
		out = append(out, e)
	case "video_start_time", "video_first_frame":
		e := base
		e.Type = TypePlaybackStart
		out = append(out, e)
	case "timejump":
		e := base
		e.Type = TypeTimejump
		out = append(out, e)
	case "error":
		e := base
		e.Type = TypeError
		e.Info = cur.PlayerError
		out = append(out, e)
	case "user_marked":
		e := base
		e.Type = TypeUserMarked
		out = append(out, e)
	}

	// Catch-all branch — surface any non-empty last_event marker
	// that doesn't match the known closed set above as its own type.
	// Same intent as the legacy SQL's "NOT IN (...) " UNION branch:
	// keep the event surface open to new markers without code
	// changes when the player adds them.
	if cur.LastEvent != "" {
		if _, known := knownLastEvents[cur.LastEvent]; !known {
			e := base
			e.Type = cur.LastEvent
			out = append(out, e)
		}
	}

	return out
}
