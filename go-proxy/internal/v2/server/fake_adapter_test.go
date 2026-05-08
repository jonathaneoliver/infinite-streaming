package server

import (
	"sync"
)

// fakeAdapter is an in-memory V1Adapter for handler tests. Mirrors the
// real *App-backed adapter's external semantics without the full v1
// runtime — sessions are stored as a flat slice keyed by player_id;
// group_id is just a string tag on each session.
//
// All methods are concurrent-safe via a single mutex (matches the v1
// store's sessionsMu single-mutex contract).
type fakeAdapter struct {
	mu       sync.Mutex
	sessions []map[string]any

	// SubscribeSessions delivers snapshots whenever sessionsChanged
	// fires; pretests can call it directly to drive the diff.
	subSessionsMu sync.Mutex
	subSessions   []chan SessionSnapshot

	subNetMu sync.Mutex
	subNet   []chan NetworkLogRow
}

func newFakeAdapter() *fakeAdapter {
	return &fakeAdapter{}
}

// addSession adds a session map to the in-memory store. Used by tests
// to prepare the world before exercising a handler.
func (a *fakeAdapter) addSession(s map[string]any) {
	a.mu.Lock()
	a.sessions = append(a.sessions, cloneMap(s))
	a.mu.Unlock()
	a.notifySessions()
}

func (a *fakeAdapter) snapshot() []map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]map[string]any, len(a.sessions))
	for i, s := range a.sessions {
		out[i] = cloneMap(s)
	}
	return out
}

func (a *fakeAdapter) notifySessions() {
	snap := SessionSnapshot{Sessions: a.snapshot()}
	a.subSessionsMu.Lock()
	defer a.subSessionsMu.Unlock()
	for _, ch := range a.subSessions {
		select {
		case ch <- snap:
		default:
		}
	}
}

// SessionList ----------------------------------------------------------

func (a *fakeAdapter) SessionList() []map[string]any {
	return a.snapshot()
}

func (a *fakeAdapter) SessionByPlayerID(playerID string) (map[string]any, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if asString(s["player_id"]) == playerID {
			return cloneMap(s), true
		}
	}
	return nil, false
}

func (a *fakeAdapter) NetworkLogForPlayer(playerID string, limit int) []map[string]any {
	return nil
}

func (a *fakeAdapter) Version() string          { return "fake" }
func (a *fakeAdapter) ContentDir() string       { return "/tmp/fake" }
func (a *fakeAdapter) AuthEnabled() bool        { return false }
func (a *fakeAdapter) AnalyticsEnabled() bool   { return false }

// Mutations ------------------------------------------------------------

func (a *fakeAdapter) MutatePlayer(playerID string, fn func(map[string]any) error) (map[string]any, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range a.sessions {
		if asString(s["player_id"]) != playerID {
			continue
		}
		mutable := cloneMap(s)
		if err := fn(mutable); err != nil {
			return nil, true, err
		}
		a.sessions[i] = mutable
		go a.notifySessions()
		return cloneMap(mutable), true, nil
	}
	return nil, false, nil
}

func (a *fakeAdapter) CreateSyntheticPlayer(playerID string, payload map[string]any) (int, map[string]any, error) {
	return 0, nil, fakeSyntheticErr{}
}

type fakeSyntheticErr struct{}

func (fakeSyntheticErr) Error() string { return "fake adapter doesn't implement synthetic players" }

func (a *fakeAdapter) DeletePlayer(playerID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range a.sessions {
		if asString(s["player_id"]) == playerID {
			a.sessions = append(a.sessions[:i], a.sessions[i+1:]...)
			go a.notifySessions()
			return true
		}
	}
	return false
}

func (a *fakeAdapter) ClearAllPlayers() {
	a.mu.Lock()
	a.sessions = nil
	a.mu.Unlock()
	a.notifySessions()
}

// Subscriptions ---------------------------------------------------------

func (a *fakeAdapter) SubscribeSessions(buffer int) (<-chan SessionSnapshot, func()) {
	if buffer <= 0 {
		buffer = 8
	}
	ch := make(chan SessionSnapshot, buffer)
	a.subSessionsMu.Lock()
	a.subSessions = append(a.subSessions, ch)
	a.subSessionsMu.Unlock()
	cancel := func() {
		a.subSessionsMu.Lock()
		for i, c := range a.subSessions {
			if c == ch {
				a.subSessions = append(a.subSessions[:i], a.subSessions[i+1:]...)
				close(c)
				break
			}
		}
		a.subSessionsMu.Unlock()
	}
	return ch, cancel
}

func (a *fakeAdapter) SubscribeNetwork(buffer int) (<-chan NetworkLogRow, func()) {
	ch := make(chan NetworkLogRow, buffer)
	a.subNetMu.Lock()
	a.subNet = append(a.subNet, ch)
	a.subNetMu.Unlock()
	cancel := func() {
		a.subNetMu.Lock()
		for i, c := range a.subNet {
			if c == ch {
				a.subNet = append(a.subNet[:i], a.subNet[i+1:]...)
				close(c)
				break
			}
		}
		a.subNetMu.Unlock()
	}
	return ch, cancel
}

// Groups ---------------------------------------------------------------

func (a *fakeAdapter) GroupMembers(groupID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, s := range a.sessions {
		if asString(s["group_id"]) == groupID {
			if pid := asString(s["player_id"]); pid != "" {
				out = append(out, pid)
			}
		}
	}
	return out
}

func (a *fakeAdapter) LinkGroup(groupID string, playerIDs []string) []string {
	want := map[string]struct{}{}
	for _, p := range playerIDs {
		if p != "" {
			want[p] = struct{}{}
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var linked []string
	for i, s := range a.sessions {
		pid := asString(s["player_id"])
		if _, ok := want[pid]; !ok {
			continue
		}
		mutable := cloneMap(s)
		mutable["group_id"] = groupID
		a.sessions[i] = mutable
		linked = append(linked, pid)
	}
	if len(linked) > 0 {
		go a.notifySessions()
	}
	return linked
}

func (a *fakeAdapter) UnlinkGroup(groupID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var cleared []string
	for i, s := range a.sessions {
		if asString(s["group_id"]) != groupID {
			continue
		}
		mutable := cloneMap(s)
		mutable["group_id"] = ""
		a.sessions[i] = mutable
		if pid := asString(s["player_id"]); pid != "" {
			cleared = append(cleared, pid)
		}
	}
	if len(cleared) > 0 {
		go a.notifySessions()
	}
	return cleared
}

func (a *fakeAdapter) RemoveFromGroup(playerID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, s := range a.sessions {
		if asString(s["player_id"]) != playerID {
			continue
		}
		if asString(s["group_id"]) == "" {
			return false
		}
		mutable := cloneMap(s)
		mutable["group_id"] = ""
		a.sessions[i] = mutable
		go a.notifySessions()
		return true
	}
	return false
}

func (a *fakeAdapter) BroadcastPatch(groupID, excludePlayerID, rev string, fn func(map[string]any) error) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var touched []string
	for i, s := range a.sessions {
		if asString(s["group_id"]) != groupID {
			continue
		}
		if asString(s["player_id"]) == excludePlayerID {
			continue
		}
		mutable := cloneMap(s)
		if err := fn(mutable); err != nil {
			return touched, err
		}
		mutable["control_revision"] = rev
		a.sessions[i] = mutable
		if pid := asString(s["player_id"]); pid != "" {
			touched = append(touched, pid)
		}
	}
	if len(touched) > 0 {
		go a.notifySessions()
	}
	return touched, nil
}

// helpers --------------------------------------------------------------

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case map[string]any:
			out[k] = cloneMap(x)
		default:
			out[k] = v
		}
	}
	return out
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
