package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeFarm serves a fixed /device roster and records block/unblock calls, so the
// reservation primitive (#952) is testable without a live farm. failBlock names
// a UDID whose block returns 500 (to exercise rollback).
type fakeFarm struct {
	mu        sync.Mutex
	roster    string
	blocked   []string
	unblocked []string
	failBlock string
}

func (f *fakeFarm) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/device-farm/api/device", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, f.roster)
	})
	rec := func(target *[]string, failOn string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var m map[string]string
			_ = json.Unmarshal(body, &m)
			f.mu.Lock()
			defer f.mu.Unlock()
			if failOn != "" && m["udid"] == failOn {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			*target = append(*target, m["udid"])
			w.WriteHeader(http.StatusOK)
		}
	}
	mux.HandleFunc("/device-farm/api/block", rec(&f.blocked, f.failBlock))
	mux.HandleFunc("/device-farm/api/unblock", rec(&f.unblocked, ""))
	return mux
}

// three free ios sims + one busy sim (must be excluded from reservation).
const reserveRoster = `[
  {"udid":"SIM-1","platform":"ios","realDevice":false,"name":"Fleet iPhone 15 #1","state":"Booted","host":"h","busy":false,"offline":false,"userBlocked":false},
  {"udid":"SIM-2","platform":"ios","realDevice":false,"name":"Fleet iPhone 15 #2","state":"Booted","host":"h","busy":false,"offline":false,"userBlocked":false},
  {"udid":"SIM-3","platform":"ios","realDevice":false,"name":"Fleet iPhone 15 #3","state":"Booted","host":"h","busy":false,"offline":false,"userBlocked":false},
  {"udid":"SIM-BUSY","platform":"ios","realDevice":false,"name":"Fleet iPhone 15 #4","state":"Booted","host":"h","busy":true,"offline":false,"userBlocked":false}
]`

func TestReserveDevices_BlocksN(t *testing.T) {
	farm := &fakeFarm{roster: reserveRoster}
	srv := httptest.NewServer(farm.handler())
	defer srv.Close()

	udids, err := reserveDevices(srv.URL, 2, nil)
	if err != nil {
		t.Fatalf("reserveDevices: %v", err)
	}
	if len(udids) != 2 {
		t.Fatalf("reserved %v, want 2 UDIDs", udids)
	}
	// Exactly the 2 reserved got blocked; the busy one was never a candidate.
	if len(farm.blocked) != 2 {
		t.Errorf("blocked %v, want 2 block calls", farm.blocked)
	}
	for _, u := range udids {
		if u == "SIM-BUSY" {
			t.Errorf("reserved the busy device %s", u)
		}
	}
}

func TestReserveDevices_InsufficientCandidates(t *testing.T) {
	farm := &fakeFarm{roster: reserveRoster}
	srv := httptest.NewServer(farm.handler())
	defer srv.Close()

	// Only 3 free sims, ask for 4 → error, and NOTHING blocked (atomic).
	_, err := reserveDevices(srv.URL, 4, nil)
	if err == nil {
		t.Fatal("expected error when fewer than n free devices")
	}
	if len(farm.blocked) != 0 {
		t.Errorf("insufficient-candidate reserve should block nothing, blocked %v", farm.blocked)
	}
}

func TestReserveDevices_RollsBackOnPartialFailure(t *testing.T) {
	// The 3rd block fails → the 2 already blocked must be unblocked (rollback), and
	// the call errors with no net reservation.
	farm := &fakeFarm{roster: reserveRoster, failBlock: "SIM-3"}
	srv := httptest.NewServer(farm.handler())
	defer srv.Close()

	_, err := reserveDevices(srv.URL, 3, nil)
	if err == nil {
		t.Fatal("expected error when a block fails mid-reservation")
	}
	// SIM-1 and SIM-2 were blocked then rolled back.
	if len(farm.blocked) != 2 {
		t.Errorf("expected 2 successful blocks before the failure, got %v", farm.blocked)
	}
	if len(farm.unblocked) != 2 {
		t.Errorf("rollback should unblock the 2 taken, unblocked %v", farm.unblocked)
	}
}

func TestReserveDevices_MatchFilter(t *testing.T) {
	farm := &fakeFarm{roster: reserveRoster}
	srv := httptest.NewServer(farm.handler())
	defer srv.Close()

	// Match only iphone-sim tokens — all 3 free sims qualify; ask for 1.
	match := func(d DeviceCapability) bool {
		return len(intersectTokens(sweepPlatformsForDevice(d), []string{"iphone-sim"})) > 0
	}
	udids, err := reserveDevices(srv.URL, 1, match)
	if err != nil {
		t.Fatalf("reserveDevices: %v", err)
	}
	if len(udids) != 1 || !strings.HasPrefix(udids[0], "SIM-") {
		t.Errorf("expected 1 matching sim, got %v", udids)
	}
}
