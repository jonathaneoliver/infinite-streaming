package eventclass

import (
	"sync"
	"time"
)

// Pair-style snapshot classifiers — emit one event when a START
// marker is followed by an END marker. The legacy SQL did this with
// a leadInFrame window function over the full play's row set; the
// streaming ingest variant holds the open START in memory and emits
// when it sees the matching END.
//
// Event ts = start_ts (matches legacy SQL semantics — the operator
// sees "stall at 18:29:27" referring to when playback first paused,
// not when it resumed). Duration is computed from end_ts - start_ts
// at emit time.

func init() {
	pc := &pairClassifier{
		openStalls:    make(map[string]openPair),
		openBuffering: make(map[string]openPair),
	}
	RegisterSnapshot("snapshot_pairs", pc)
}

type openPair struct {
	startTs   string    // for the emitted event's Ts
	startTime time.Time // for duration math
	source    Snapshot  // for identity provenance on the emitted event
}

type pairClassifier struct {
	mu            sync.Mutex
	openStalls    map[string]openPair // key: player_id|play_id
	openBuffering map[string]openPair
}

func pairKey(s *Snapshot) string {
	return s.PlayerID + "|" + s.PlayID
}

// parseChTs parses CH's "YYYY-MM-DD HH:MM:SS.fff" format. Returns
// zero time if the string doesn't parse — duration math then yields 0
// and the event is dropped by the `duration_s > 0` guard the legacy
// SQL used.
func parseChTs(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	// Try RFC3339 (the timeseries SSE form) first, then CH's
	// space-separator form. ParseInLocation with UTC keeps both
	// shapes anchored to the same wall clock.
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05.000", s); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

func (p *pairClassifier) Classify(prev, cur *Snapshot) []Event {
	if cur == nil {
		return nil
	}
	// Edge-trigger on the start markers — only record the open pair
	// when last_event TRANSITIONS to stall_start / buffering_start.
	// Without this guard, every subsequent snapshot carrying the
	// stale marker would overwrite the open entry and push start_ts
	// forward, shrinking the eventually-emitted duration.
	if prev != nil && prev.LastEvent == cur.LastEvent {
		return nil
	}
	switch cur.LastEvent {
	case "stall_start":
		p.mu.Lock()
		p.openStalls[pairKey(cur)] = openPair{
			startTs:   cur.Ts,
			startTime: parseChTs(cur.Ts),
			source:    *cur,
		}
		p.mu.Unlock()
		return nil

	case "stall_end":
		p.mu.Lock()
		open, ok := p.openStalls[pairKey(cur)]
		if ok {
			delete(p.openStalls, pairKey(cur))
		}
		p.mu.Unlock()
		if !ok {
			return nil
		}
		dur := parseChTs(cur.Ts).Sub(open.startTime).Seconds()
		if dur <= 0 {
			return nil
		}
		return []Event{{
			Ts:             open.startTs,
			PlayerID:       open.source.PlayerID,
			PlayID:         open.source.PlayID,
			AttemptID:      open.source.AttemptID,
			SessionID:      open.source.SessionID,
			Classification: open.source.Classification,
			Type:           TypeStall,
			Info:           FormatDurationS(dur),
		}}

	case "buffering_start":
		p.mu.Lock()
		p.openBuffering[pairKey(cur)] = openPair{
			startTs:   cur.Ts,
			startTime: parseChTs(cur.Ts),
			source:    *cur,
		}
		p.mu.Unlock()
		return nil

	case "buffering_end":
		p.mu.Lock()
		open, ok := p.openBuffering[pairKey(cur)]
		if ok {
			delete(p.openBuffering, pairKey(cur))
		}
		p.mu.Unlock()
		if !ok {
			return nil
		}
		dur := parseChTs(cur.Ts).Sub(open.startTime).Seconds()
		if dur <= 0 {
			return nil
		}
		return []Event{{
			Ts:             open.startTs,
			PlayerID:       open.source.PlayerID,
			PlayID:         open.source.PlayID,
			AttemptID:      open.source.AttemptID,
			SessionID:      open.source.SessionID,
			Classification: open.source.Classification,
			Type:           TypeBuffering,
			Info:           FormatDurationS(dur),
		}}
	}
	return nil
}
