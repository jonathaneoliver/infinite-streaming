package main

import (
	"encoding/json"
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
// caused issue #81. In production:
//   - The "handler" goroutine owns sessionList and mutates session fields
//     between successive saveSessionList calls.
//   - On each saveSessionList → queueSessionsBroadcast call, the handler
//     publishes a snapshot to a shared "broadcast" slot.
//   - A "broadcast" goroutine (the AfterFunc timer in production) reads
//     the latest snapshot and mutates it (normalizeSessionsForResponse)
//     and marshals it.
//
// With the snapshot in queueSessionsBroadcast, the broadcast goroutine
// works on its own copy and must never race with the handler's continued
// mutations. Run with `go test -race`.
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
