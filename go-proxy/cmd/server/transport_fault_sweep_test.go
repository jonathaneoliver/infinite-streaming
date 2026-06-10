package main

// transport_fault_sweep_test.go — issue #716.
//
// Regression guard for the session-start kernel sweep being made symmetric:
// it must clear leftover *nftables transport faults* (drop/reject), not just
// tc rate/delay/loss. Before the fix, a transport fault left on a port by a
// prior session whose teardown was skipped (crash / Ctrl-C / timeout) could
// carry over to the next session that reused the port — `ClearPortShaping`
// swept tc but nothing swept nftables.
//
// The fix wires `armTransportFaultLoop(port, "none", …)` into the new-session
// allocation branch (cmd/server/main.go, next to `ClearPortShaping`). These
// tests exercise that exact sweep call against a real nftables environment.
//
// Requires CAP_NET_ADMIN + an `nft` binary; SKIPS cleanly where the kernel
// surface isn't available (dev laptops, unprivileged CI). The companion
// black-box TestTransportFaults (tests/server_behavior/) can't set up this
// precondition — it has no way to drop a session record WITHOUT running the
// clean teardown that already clears the fault, which is the very path this
// issue is NOT about.

import (
	"context"
	"testing"
	"time"
)

// transportRulePresent reports whether a transport-fault rule is currently
// installed on `port`, by looking for the port's entry in the live nftables
// counter map (present even at zero packets — the rule line still parses).
func transportRulePresent(port int) bool {
	_, ok := getTransportFaultRuleCounters()[port]
	return ok
}

// newSweepTestApp builds the minimal App the sweep call touches: just the
// fault-loop registry. With an empty session snapshot, setTransportFaultSessionState
// matches no session and is a no-op, so none of the heavier App wiring
// (hubs, traffic manager, session store) is needed.
func newSweepTestApp() *App {
	return &App{faultLoops: map[int]context.CancelFunc{}}
}

// requireNftables arms the fault chain and skips the test if the kernel
// surface isn't reachable (no nft binary, or no CAP_NET_ADMIN).
func requireNftables(t *testing.T) {
	t.Helper()
	if err := ensureTransportFaultChain(); err != nil {
		t.Skipf("nftables transport-fault chain unavailable (need nft + CAP_NET_ADMIN): %v", err)
	}
}

// TestSessionStartSweepClearsLeftoverTransportFault is the core #716 guard:
// a leftover drop rule on a port (as a crashed session would leave) is gone
// after the session-start sweep runs on that port.
func TestSessionStartSweepClearsLeftoverTransportFault(t *testing.T) {
	requireNftables(t)
	const port = 39911 // outside the 30181–30881 session range; won't collide
	app := newSweepTestApp()
	t.Cleanup(func() { _ = clearTransportFaultRule(port) })

	// Stand in for the fault a prior (crashed) session left armed on this port.
	if err := applyTransportFaultRule(port, "drop"); err != nil {
		t.Fatalf("seed leftover drop rule: %v", err)
	}
	if !transportRulePresent(port) {
		t.Fatalf("precondition failed: seeded drop rule not present on port %d", port)
	}

	// The exact sweep the fix runs when a fresh session is allocated on a port.
	app.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)

	if transportRulePresent(port) {
		t.Errorf("session-start sweep did NOT clear leftover transport fault on port %d", port)
	}
}

// TestSessionStartSweepStopsLeakedFaultLoop covers the subtle half of the
// fix: when the prior session armed an on/off cadence loop, a still-running
// loop goroutine would RE-ARM the rule after a bare clear. The sweep uses
// armTransportFaultLoop("none", …) precisely because it also cancels that
// goroutine — a plain clearTransportFaultRule would not have been enough.
func TestSessionStartSweepStopsLeakedFaultLoop(t *testing.T) {
	requireNftables(t)
	const port = 39912
	app := newSweepTestApp()
	t.Cleanup(func() {
		app.stopTransportFaultLoop(port)
		_ = clearTransportFaultRule(port)
	})

	// A prior session's on/off drop loop (1s on, 1s off) — spawns a goroutine
	// that keeps re-applying the rule. This is the leak the bare-clear path
	// would lose to.
	app.armTransportFaultLoop(port, "drop", 1, transportUnitsSeconds, 1)
	app.faultMu.Lock()
	_, running := app.faultLoops[port]
	app.faultMu.Unlock()
	if !running {
		t.Fatalf("precondition failed: fault loop goroutine not registered for port %d", port)
	}

	// Session-start sweep on the reused port.
	app.armTransportFaultLoop(port, "none", 1, transportUnitsSeconds, 0)

	app.faultMu.Lock()
	_, stillRunning := app.faultLoops[port]
	app.faultMu.Unlock()
	if stillRunning {
		t.Errorf("session-start sweep left the prior fault loop goroutine running on port %d", port)
	}

	// Give the (now-cancelled) goroutine more than one on/off cycle to prove
	// it can't re-arm the rule behind the sweep's back.
	time.Sleep(2500 * time.Millisecond)
	if transportRulePresent(port) {
		t.Errorf("transport fault re-armed on port %d after sweep — leaked loop goroutine still active", port)
	}
}
