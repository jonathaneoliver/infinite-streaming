package main

import "testing"

// A trimmed capture of the live /device-farm/api/device response (issue #948):
// one busy real iPhone, one free booted iPad sim, one shutdown tvOS sim, one
// offline sim, and one userBlocked sim — covering every availability branch.
const farmRosterJSON = `[
  {"udid":"00008120-REAL","name":"Jonathans iPhone","platform":"ios","realDevice":true,"deviceType":"real","sdk":"26.5","host":"http://h:4723","busy":true,"offline":false,"userBlocked":false},
  {"udid":"IPAD-SIM-1","name":"iPad Pro","platform":"ios","realDevice":false,"deviceType":"simulator","sdk":"26.4","state":"Booted","host":"http://h:4723","busy":false,"offline":false,"userBlocked":false},
  {"udid":"TV-SIM-1","name":"Apple TV","platform":"tvos","realDevice":false,"deviceType":"simulator","sdk":"26.4","state":"Shutdown","host":"http://h:4723","busy":false,"offline":false,"userBlocked":false},
  {"udid":"OFFLINE-SIM","name":"Dead sim","platform":"ios","realDevice":false,"deviceType":"simulator","sdk":"26.4","state":"Booted","host":"http://h:4723","busy":false,"offline":true,"userBlocked":false},
  {"udid":"BLOCKED-SIM","name":"Reserved sim","platform":"ios","realDevice":false,"deviceType":"simulator","sdk":"26.4","state":"Booted","host":"http://h:4723","busy":false,"offline":false,"userBlocked":true}
]`

func TestParseFarmRoster(t *testing.T) {
	devs := parseFarmRoster([]byte(farmRosterJSON))
	if len(devs) != 5 {
		t.Fatalf("got %d devices, want 5", len(devs))
	}
	byUDID := map[string]DeviceCapability{}
	for _, d := range devs {
		byUDID[d.UDID] = d
	}

	// Real device: always booted, platform lower-cased, version from sdk, busy ⇒ not free.
	real := byUDID["00008120-REAL"]
	if !real.Real || real.Platform != "ios" || real.Version != "26.5" || !real.Booted {
		t.Errorf("real iPhone mapped wrong: %+v", real)
	}
	if real.Free {
		t.Errorf("busy real iPhone should not be Free: %+v", real)
	}

	// Booted free sim ⇒ Free.
	sim := byUDID["IPAD-SIM-1"]
	if sim.Real || !sim.Booted || !sim.Free {
		t.Errorf("free booted iPad sim should be Free & booted: %+v", sim)
	}

	// Shutdown sim ⇒ NOT booted but STILL Free (farm boots on session create).
	tv := byUDID["TV-SIM-1"]
	if tv.Booted {
		t.Errorf("shutdown sim should report Booted=false: %+v", tv)
	}
	if !tv.Free {
		t.Errorf("shutdown sim should still be Free (bootable on demand): %+v", tv)
	}
	if tv.Platform != "tvos" {
		t.Errorf("tv platform = %q, want tvos", tv.Platform)
	}

	// Offline and blocked ⇒ not Free.
	if byUDID["OFFLINE-SIM"].Free {
		t.Errorf("offline sim should not be Free")
	}
	if byUDID["BLOCKED-SIM"].Free {
		t.Errorf("userBlocked sim should not be Free")
	}

	// FreeDevices returns exactly the two allocatable ones (booted sim + shutdown sim).
	free := FreeDevices(devs)
	if len(free) != 2 {
		t.Fatalf("FreeDevices = %d, want 2 (IPAD-SIM-1, TV-SIM-1)", len(free))
	}
	freeSet := map[string]bool{}
	for _, d := range free {
		freeSet[d.UDID] = true
	}
	if !freeSet["IPAD-SIM-1"] || !freeSet["TV-SIM-1"] {
		t.Errorf("FreeDevices missing expected UDIDs: %+v", freeSet)
	}
}

func TestParseFarmRosterBadJSON(t *testing.T) {
	if got := parseFarmRoster([]byte(`{"not":"an array"}`)); got != nil {
		t.Errorf("non-array body should parse to nil, got %+v", got)
	}
	if got := parseFarmRoster(nil); got != nil {
		t.Errorf("nil body should parse to nil, got %+v", got)
	}
}
