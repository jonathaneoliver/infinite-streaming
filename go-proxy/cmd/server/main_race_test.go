package main

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestCloneSessionListIsolation verifies that mutations to the source after
// cloning do not affect the clone. Without this isolation the broadcast
// pipeline shares maps with request handlers and races json.Marshal.
func TestCloneSessionListIsolation(t *testing.T) {
	src := []SessionData{
		{
			"session_id": "1",
			"counter":    int64(1),
			"nested": map[string]interface{}{
				"throughput_mbps": float64(2.5),
			},
		},
	}

	snap := cloneSessionList(src)

	// Mutate the source after cloning.
	src[0]["counter"] = int64(99)
	src[0]["nested"].(map[string]interface{})["throughput_mbps"] = float64(99.9)

	if got := snap[0]["counter"]; got != int64(1) {
		t.Fatalf("snapshot leaked mutation on top-level field: got %v want 1", got)
	}
	if got := snap[0]["nested"].(map[string]interface{})["throughput_mbps"]; got != float64(2.5) {
		t.Fatalf("snapshot leaked mutation on nested map: got %v want 2.5", got)
	}
}

// TestCloneSessionListIsolatesBroadcast mirrors the production race that
// caused issue #81 and is still relevant on the per-event emit path
// (issue #470 replaced the debounced broadcast but the cloning
// invariant remained). In production:
//   - The "handler" goroutine owns sessionList and mutates session fields
//     between successive saveSessionList calls.
//   - saveSessionByIDReturning clones the merged session before handing
//     it to emitSessionEvent, so the SSE write goroutine works on its
//     own copy and never races with the handler's continued mutations.
//
// The test exercises cloneSessionList directly — the same primitive
// emitSessionEvent's normalizeSessionsForResponse path leans on. Run
// with `go test -race`.
func TestCloneSessionListIsolatesBroadcast(t *testing.T) {
	live := []SessionData{
		{
			"session_id": "race",
			"counter":    int64(0),
			"nested": map[string]interface{}{
				"throughput_mbps": float64(0),
			},
		},
	}

	var slot atomic.Pointer[[]SessionData]
	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Handler goroutine: mutates live, periodically publishes a snapshot.
	wg.Add(1)
	go func() {
		defer wg.Done()
		i := int64(0)
		for {
			select {
			case <-stop:
				return
			default:
				live[0]["counter"] = i
				live[0]["last_request"] = time.Now().Format(time.RFC3339Nano)
				live[0]["nested"].(map[string]interface{})["throughput_mbps"] = float64(i)
				i++
				if i%5 == 0 {
					snap := cloneSessionList(live)
					slot.Store(&snap)
				}
			}
		}
	}()

	// Broadcast goroutine: picks up the latest snapshot, mutates it
	// (mimicking normalizeSessionsForResponse), marshals it.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.Now().Add(200 * time.Millisecond)
		for time.Now().Before(deadline) {
			snapPtr := slot.Load()
			if snapPtr == nil {
				continue
			}
			snap := *snapPtr
			for _, session := range snap {
				session["normalized_at"] = time.Now().UnixNano()
				if nested, ok := session["nested"].(map[string]interface{}); ok {
					nested["normalized"] = true
				}
			}
			if _, err := json.Marshal(snap); err != nil {
				t.Errorf("marshal failed: %v", err)
				return
			}
		}
	}()

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestSessionListNoLostUpdatesUnderConcurrency is the #740 regression: it
// hammers the in-memory session list with every writer shape at once —
// bootstrap-style appends, per-port updates, single-session read-modify-write
// increments, a reaper-style filter, and pure reads — and asserts no update is
// lost. The old getSessionList→mutate→saveSessionList pattern held no lock
// across the read and the write, so concurrent writers clobbered each other
// (the nftk=100 fleet symptom). With mutateSessions' CAS each writer either
// commits cleanly or retries from a fresh clone, so:
//
//   - every appended session_id survives (a non-atomic append loses some), and
//   - the shared "marker" session's increment count equals the number of
//     increments performed (a lost RMW would leave it short), and never
//     regresses as readers observe it.
//
// Fails to compile on the pre-#740 tree (no mutateSessions), and a CAS-less
// reimplementation fails the count asserts. Run with `go test -race`.
func TestSessionListNoLostUpdatesUnderConcurrency(t *testing.T) {
	app := &App{}
	// Seed the shared single-session target for the RMW-increment race. The
	// port lets the real updateSessionsByPort writer match it too.
	seed := []SessionData{{
		"session_id":       "marker",
		"x_forwarded_port": "30181",
		"marker":           int64(0),
	}}
	app.sessionsSnap.Store(&seed)

	const (
		appenders        = 6
		appendsPer       = 40
		incrementers     = 6
		incrementsPer    = 80
		readers          = 4
		portUpdaters     = 2
		transientWorkers = 3
		transientPer     = 30
		reapers          = 2
	)
	totalAppends := appenders * appendsPer
	totalIncrements := int64(incrementers * incrementsPer)

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Appenders — bootstrap-style CAS appends of uniquely-keyed sessions.
	for g := 0; g < appenders; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < appendsPer; i++ {
				id := fmt.Sprintf("app-%d-%d", g, i)
				app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
					return append(sessions, SessionData{"session_id": id}), true
				})
			}
		}()
	}

	// Incrementers — single-session read-modify-write on the shared marker.
	// This is the canonical lost-update detector: a load→store without CAS
	// drops concurrent increments and the final count comes up short.
	for g := 0; g < incrementers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < incrementsPer; i++ {
				app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
					for _, s := range sessions {
						if getString(s, "session_id") == "marker" {
							s["marker"] = int64FromInterface(s["marker"]) + 1
							return sessions, true
						}
					}
					return sessions, false
				})
			}
		}()
	}

	// Per-port updaters — the real converted writer, targeting the marker
	// session's port. Writes a different field so it never races the marker
	// value itself, but contends on the same session under CAS.
	for g := 0; g < portUpdaters; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					app.updateSessionsByPort(30181, map[string]interface{}{"shaped": true})
				}
			}
		}()
	}

	// Transient adders + reapers — exercise concurrent filter-and-publish.
	// Transient sessions are tagged so the reaper can drop them without
	// touching the counted app-* sessions or the marker.
	for g := 0; g < transientWorkers; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < transientPer; i++ {
				id := fmt.Sprintf("transient-%d-%d", g, i)
				app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
					return append(sessions, SessionData{"session_id": id, "_transient": true}), true
				})
			}
		}()
	}
	for g := 0; g < reapers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					app.mutateSessions(func(sessions []SessionData) ([]SessionData, bool) {
						filtered := make([]SessionData, 0, len(sessions))
						removed := false
						for _, s := range sessions {
							if getBool(s, "_transient") {
								removed = true
								continue
							}
							filtered = append(filtered, s)
						}
						return filtered, removed
					})
				}
			}
		}()
	}

	// Readers — pure reads. The marker only ever climbs, so a clone read must
	// never observe it regress (a torn/lost write would).
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var last int64
			for {
				select {
				case <-stop:
					return
				default:
					for _, s := range app.getSessionList() {
						if getString(s, "session_id") == "marker" {
							cur := int64FromInterface(s["marker"])
							if cur < last {
								t.Errorf("marker regressed: saw %d after %d", cur, last)
							}
							last = cur
						}
					}
				}
			}
		}()
	}

	// The bounded producers (appenders, incrementers, transient adders) and
	// the open-ended workers (readers, port updaters, reapers) all hold wg
	// counts, so wg.Wait() can't tell them apart. Join the producers by
	// polling the committed state for convergence — all appends present and
	// all increments applied — then signal the open-ended workers to stop.
	deadline := time.Now().Add(10 * time.Second)
	for {
		list := app.getSessionList()
		appPresent := 0
		var marker int64
		for _, s := range list {
			id := getString(s, "session_id")
			if id == "marker" {
				marker = int64FromInterface(s["marker"])
			} else if len(id) > 4 && id[:4] == "app-" {
				appPresent++
			}
		}
		if appPresent == totalAppends && marker == totalIncrements {
			break
		}
		if time.Now().After(deadline) {
			close(stop)
			wg.Wait()
			t.Fatalf("did not converge: app sessions %d/%d, marker %d/%d",
				appPresent, totalAppends, marker, totalIncrements)
		}
		time.Sleep(time.Millisecond)
	}

	close(stop)
	wg.Wait()

	// Final assertions on the committed snapshot.
	final := app.getSessionList()
	seen := map[string]bool{}
	var marker int64
	transientLeft := 0
	for _, s := range final {
		id := getString(s, "session_id")
		switch {
		case id == "marker":
			marker = int64FromInterface(s["marker"])
		case getBool(s, "_transient"):
			transientLeft++
		default:
			seen[id] = true
		}
	}
	if marker != totalIncrements {
		t.Errorf("lost increments: marker=%d want=%d", marker, totalIncrements)
	}
	for g := 0; g < appenders; g++ {
		for i := 0; i < appendsPer; i++ {
			id := fmt.Sprintf("app-%d-%d", g, i)
			if !seen[id] {
				t.Errorf("lost append: %s missing from final snapshot", id)
			}
		}
	}
	if len(seen) != totalAppends {
		t.Errorf("unexpected appended-session count: got %d want %d (transient leftovers=%d)",
			len(seen), totalAppends, transientLeft)
	}
}
