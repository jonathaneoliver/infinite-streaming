// transport_fault_test.go — issue #523.
//
// CHARACTERIZATION (not pass/fail on a single connect outcome): for each
// transport fault, observe and LIST exactly what it produces across three
// independent dimensions, because the obvious "fresh TCP connect" signal is
// MASKED by Docker's userland proxy and would otherwise read as a false
// negative.
//
// Why the fresh connect lies: on test-dev (Docker Compose) the published
// session port is fronted by `docker-proxy`, which completes the client TCP
// handshake on the host and *then* opens a separate connection into the
// container. The proxy's nftables rule (`tcp dport <port> counter drop|reject`,
// no `ct state new` qualifier — it matches ALL packets, not just SYNs) lives
// on the container leg. So a brand-new outside-in connect SUCCEEDS at
// docker-proxy even while the fault is fully armed — the failure only shows
// up once bytes have to traverse the container leg (the request/response).
//
// The three dimensions we report per fault:
//
//	fresh connect : raw net.Dial to the published port (what docker-proxy
//	                surfaces — usually "connected", the masked signal).
//	data level    : an actual HTTP GET through the proxy with the fault held
//	                — does it stall (drop), reset (reject), or complete?
//	kernel pkts   : delta on the nftables rule's drop/reject packet counters
//	                (`transport_fault_drop_packets`/`_reject_packets`), the
//	                un-maskable ground truth that the rule matched traffic.
//
// `hang` is included as the HTTP-layer contrast: it produces a data-level
// stall with ZERO kernel packets (it's request_connect_hang, not a kernel
// rule).
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestTransportFaults -timeout 5m
//
// Env:
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	TRANSPORT_PROBE_TIMEOUT_S=4   fresh-connect timeout
//	TRANSPORT_DATA_TIMEOUT_S=8    data-level GET deadline (drop stalls to here)
//	TRANSPORT_ARM_WINDOW_S=10     how long to wait for the kernel rule to arm
package server_behavior

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// classifyConnect attempts a raw TCP connect and buckets the outcome.
func classifyConnect(addr string, timeout time.Duration) string {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err == nil {
		conn.Close()
		return "connected"
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "refused"):
		return "refused"
	case strings.Contains(msg, "reset"):
		return "reset"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "timed out"):
		return "timeout"
	default:
		return "other:" + msg
	}
}

func transportSet(faultType string) map[string]any {
	return map[string]any{
		"transport_fault_type": faultType,
		// on-seconds = 0 takes runTransportFaultLoop's hold path: apply the
		// kernel rule once and keep it until cleared. A positive value runs
		// the on/off cadence loop instead.
		"transport_consecutive_failures": 0,
		"transport_failure_frequency":    0,
		"transport_consecutive_units":    "seconds",
	}
}

// transportPacketCounters reads the live nftables rule counters off the
// session record (refreshed from the kernel by /api/sessions).
func (p *probe) transportPacketCounters() (drop, reject float64, active bool) {
	m, err := getSessionMap(p.c, p.apiBase, p.playerID)
	if err != nil {
		return 0, 0, false
	}
	drop, _ = mapFloat(m, "transport_fault_drop_packets")
	reject, _ = mapFloat(m, "transport_fault_reject_packets")
	if a, ok := m["transport_fault_active"].(bool); ok {
		active = a
	}
	return drop, reject, active
}

// awaitTransportActive polls until the proxy reports the kernel rule live, so
// the probe doesn't race the PATCH→nft install.
func (p *probe) awaitTransportActive(window time.Duration) bool {
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if _, _, active := p.transportPacketCounters(); active {
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

// terseErr collapses Go's verbose net error (which embeds the full URL +
// IPv6 endpoints) down to the meaningful reason, for a readable matrix.
func terseErr(msg string) string {
	low := strings.ToLower(msg)
	for _, phrase := range []string{
		"connection reset by peer", "connection refused", "broken pipe",
		"i/o timeout", "unexpected eof",
	} {
		if strings.Contains(low, phrase) {
			return phrase
		}
	}
	if strings.HasSuffix(msg, "EOF") {
		return "eof"
	}
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		return msg[i+2:]
	}
	return msg
}

// describeDataOutcome turns a socketObs (from probeSocketFault) into a plain
// description of what the client saw at the data level under the fault.
func describeDataOutcome(o socketObs) string {
	switch {
	case o.completed:
		return fmt.Sprintf("COMPLETED — no disruption (%d bytes, %.1fs)", o.bodyBytes, o.elapsed.Seconds())
	case o.timedOut && !o.gotHeaders:
		return fmt.Sprintf("STALLED — request sent, no response, timed out (%.1fs)", o.elapsed.Seconds())
	case !o.gotHeaders:
		return fmt.Sprintf("CUT before response — %s (%.1fs)", terseErr(o.rawErr), o.elapsed.Seconds())
	case o.timedOut:
		return fmt.Sprintf("headers then STALLED mid-body (%d bytes, %.1fs)", o.bodyBytes, o.elapsed.Seconds())
	default:
		return fmt.Sprintf("headers then CUT (%d bytes, %.1fs, %s)", o.bodyBytes, o.elapsed.Seconds(), terseErr(o.rawErr))
	}
}

func TestTransportFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("transport faults skipped in short mode")
	}
	probeTimeout := time.Duration(envInt("TRANSPORT_PROBE_TIMEOUT_S", 4)) * time.Second
	dataTimeout := time.Duration(envInt("TRANSPORT_DATA_TIMEOUT_S", 8)) * time.Second
	armWindow := time.Duration(envInt("TRANSPORT_ARM_WINDOW_S", 10)) * time.Second

	p := newProbe(t)
	if p.sess.ExternalPort == 0 {
		t.Fatalf("session has no external port; cannot run raw TCP probes")
	}
	addr := fmt.Sprintf("%s:%d", p.host, p.sess.ExternalPort)
	insecure := envBool("THROUGHPUT_INSECURE", defaultInsecure)
	dataClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: insecure},
			DisableKeepAlives: true,
		},
	}
	t.Logf("transport probe target: %s (data probe → %s)", addr, p.top.URL)

	if got := classifyConnect(addr, probeTimeout); got != "connected" {
		t.Fatalf("baseline connect to %s = %q, want connected — can't characterize transport faults", addr, got)
	}
	t.Logf("baseline: fresh connect OK")

	// row records exactly what one fault produced across the three dimensions.
	type row struct {
		fault      string
		fresh      string
		data       string
		dropDelta  float64
		rejectDelta float64
		active     bool
	}
	var rows []row

	// runKernelFault arms a kernel fault (drop/reject), measures all three
	// dimensions, then clears. Returns the populated row.
	runKernelFault := func(t *testing.T, fault string) row {
		if err := patchSession(p.c, p.apiBase, p.sess.SessionID, transportSet(fault)); err != nil {
			t.Fatalf("arm %s: %v", fault, err)
		}
		defer patchSession(p.c, p.apiBase, p.sess.SessionID, transportSet("none"))

		active := p.awaitTransportActive(armWindow)
		drop0, reject0, _ := p.transportPacketCounters()

		fresh := classifyConnect(addr, probeTimeout)
		obs := probeSocketFault(dataClient, p.top.URL, dataTimeout)
		p.heartbeat(0, 0, 0, "rebuffering")

		drop1, reject1, activeNow := p.transportPacketCounters()
		return row{
			fault:      fault,
			fresh:      fresh,
			data:       describeDataOutcome(obs),
			dropDelta:  drop1 - drop0,
			rejectDelta: reject1 - reject0,
			active:     active || activeNow,
		}
	}

	t.Run("drop", func(t *testing.T) {
		r := runKernelFault(t, "drop")
		rows = append(rows, r)
		t.Logf("drop: fresh=%s | data=%s | kernel drop_pkts+%.0f reject_pkts+%.0f | active=%v",
			r.fresh, r.data, r.dropDelta, r.rejectDelta, r.active)
		// Ground truth: a held drop must produce SOME observable effect —
		// either the kernel matched packets, or the data transfer was
		// disrupted. (The fresh connect alone is expected to be masked.)
		if r.dropDelta == 0 && strings.HasPrefix(r.data, "COMPLETED") {
			t.Errorf("drop produced NO observable effect: kernel matched 0 packets AND the transfer completed normally")
		}
	})

	t.Run("reject", func(t *testing.T) {
		r := runKernelFault(t, "reject")
		rows = append(rows, r)
		t.Logf("reject: fresh=%s | data=%s | kernel drop_pkts+%.0f reject_pkts+%.0f | active=%v",
			r.fresh, r.data, r.dropDelta, r.rejectDelta, r.active)
		if r.rejectDelta == 0 && strings.HasPrefix(r.data, "COMPLETED") {
			t.Errorf("reject produced NO observable effect: kernel matched 0 packets AND the transfer completed normally")
		}
	})

	// hang: HTTP-layer (request_connect_hang via the "all" rule). No kernel
	// rule — expect a data-level stall with ZERO kernel packets.
	t.Run("hang", func(t *testing.T) {
		if err := patchSession(p.c, p.apiBase, p.sess.SessionID, faultSet("all", "hung", 1, 1)); err != nil {
			t.Fatalf("arm hang: %v", err)
		}
		defer patchSession(p.c, p.apiBase, p.sess.SessionID, faultClear("all"))
		time.Sleep(settleKernel)

		drop0, reject0, _ := p.transportPacketCounters()
		fresh := classifyConnect(addr, probeTimeout)
		obs := probeSocketFault(dataClient, p.top.URL, dataTimeout)
		drop1, reject1, _ := p.transportPacketCounters()
		r := row{
			fault: "hang", fresh: fresh, data: describeDataOutcome(obs),
			dropDelta: drop1 - drop0, rejectDelta: reject1 - reject0, active: false,
		}
		rows = append(rows, r)
		t.Logf("hang: fresh=%s | data=%s | kernel drop_pkts+%.0f reject_pkts+%.0f (HTTP-layer, expect 0)",
			r.fresh, r.data, r.dropDelta, r.rejectDelta)
	})

	t.Logf("\n=== transport fault characterization ===")
	t.Logf("%-8s %-12s %-52s %-10s %-12s %s", "fault", "fresh-connect", "data-level effect", "drop_pkts", "reject_pkts", "active")
	for _, r := range rows {
		t.Logf("%-8s %-12s %-52s +%-9.0f +%-11.0f %v",
			r.fault, r.fresh, r.data, r.dropDelta, r.rejectDelta, r.active)
	}
}
