// delay_accuracy_test.go — issue #519.
//
// Verifies that the operator-configured `nftables_delay_ms` is actually
// imposed on the wire. For each delay in the sweep we PATCH the session's
// shape, let the kernel apply, then read back the two RTT signals the
// proxy already computes per session:
//
//   - client_path_ping_rtt_ms : ICMP path-ping RTT (the clean signal —
//     independent of TCP congestion control).
//   - client_rtt_ms           : TCP_INFO smoothed RTT (can inflate under
//     load from bufferbloat; recorded for contrast).
//
// Contract: path-ping RTT ≈ baseline_rtt + configured_delay (within a
// few ms of noise). We measure the baseline at delay=0 and report each
// higher delay's observed increase over that baseline.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerDelay -timeout 5m
//
// Env (connection params shared with TestRateSweep):
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	DELAY_SWEEP_MS=0,50,100,250,500   configured delays to sweep
//	DELAY_SETTLE_S=8                  per-delay measurement window
package server_behavior

import (
	"fmt"
	"strconv"
	"testing"
	"time"
)

type delayResult struct {
	configuredMs int
	pathPingMs   float64
	tcpRttMs     float64
	samples      int
}

func TestServerDelay(t *testing.T) {
	if testing.Short() {
		t.Skip("delay sweep skipped in short mode")
	}
	delays, err := parseRates(env("DELAY_SWEEP_MS", "0,50,100,250,500"))
	if err != nil {
		t.Fatalf("parse delays: %v", err)
	}
	windowS := envInt("DELAY_SETTLE_S", 8)

	p := newProbe(t)
	startedAt := time.Now()
	results := make([]delayResult, 0, len(delays))

	for _, delayMs := range delays {
		t.Logf("\n=== delay %d ms — measuring for %ds ===", delayMs, windowS)
		// rate=0 → baseline cap (don't let a rate cap distort RTT); just
		// the configured delay.
		if err := setShapeFull(p.c, p.apiBase, p.sess.InternalPort, 0, delayMs, 0); err != nil {
			t.Errorf("set delay %d: %v", delayMs, err)
			continue
		}
		time.Sleep(settleKernel)
		res := p.measureRTT(t, delayMs, time.Duration(windowS)*time.Second)
		res.configuredMs = delayMs
		results = append(results, res)
		t.Logf("delay=%-6d path_ping=%.2f ms tcp_rtt=%.2f ms (n=%d)",
			delayMs, res.pathPingMs, res.tcpRttMs, res.samples)
	}

	// Clear the delay so we don't leave the session pinned.
	_ = setShapeFull(p.c, p.apiBase, p.sess.InternalPort, 0, 0, 0)

	printDelayMatrix(t, results)

	m := serverMatrix{
		Title:   "Delay accuracy (configured nftables_delay_ms vs observed RTT)",
		Columns: []string{"configured_ms", "path_ping_ms", "tcp_rtt_ms", "samples"},
	}
	for _, r := range results {
		m.Rows = append(m.Rows, []string{
			strconv.Itoa(r.configuredMs),
			fmt.Sprintf("%.2f", r.pathPingMs),
			fmt.Sprintf("%.2f", r.tcpRttMs),
			strconv.Itoa(r.samples),
		})
	}
	p.postServerReport(t, "server_delay", fmt.Sprintf("%d delays swept", len(results)), startedAt, !t.Failed(), m)
}

// measureRTT keeps the connection warm (one segment pull per second so
// the TCP_INFO RTT stays fresh and path-ping has live traffic to race)
// and polls /api/sessions for the two RTT signals, averaging the
// non-zero samples collected across the window.
func (p *probe) measureRTT(t *testing.T, delayMs int, window time.Duration) delayResult {
	t.Helper()
	deadline := time.Now().Add(window)
	segs := p.pullOnce(t)
	var segIdx int

	var pathSum, tcpSum float64
	var pathN, tcpN int
	for time.Now().Before(deadline) {
		// Touch a small slice of a segment to keep TCP_INFO RTT fresh.
		// Bounded (256KB / 4s) so an added-latency or lossy link can't
		// wedge the sweep on a multi-MB full-segment pull. Path-ping RTT
		// is sampled independently by the proxy regardless.
		if len(segs) > 0 {
			if err := boundedTouch(p.c, segs[segIdx%len(segs)], 256*1024, 4*time.Second); err != nil {
				segs = p.pullOnce(t) // playlist may have rolled
			}
			segIdx++
		}
		if m, err := getSessionMap(p.c, p.apiBase, p.playerID); err == nil {
			if v, ok := mapFloat(m, "client_path_ping_rtt_ms"); ok && v > 0 {
				pathSum += v
				pathN++
			}
			if v, ok := mapFloat(m, "client_rtt_ms"); ok && v > 0 {
				tcpSum += v
				tcpN++
			}
		}
		p.heartbeat(0, 0, time.Since(deadline.Add(-window)).Seconds(), "playing")
		time.Sleep(1 * time.Second)
	}
	res := delayResult{samples: pathN}
	if pathN > 0 {
		res.pathPingMs = round2(pathSum / float64(pathN))
	}
	if tcpN > 0 {
		res.tcpRttMs = round2(tcpSum / float64(tcpN))
	}
	return res
}

// pullOnce fetches the variant playlist and returns its segment URLs.
func (p *probe) pullOnce(t *testing.T) []string {
	t.Helper()
	body, final, err := httpGet(p.c, p.top.URL)
	if err != nil {
		t.Logf("variant playlist fetch: %v", err)
		return nil
	}
	segs, err := parseMediaPlaylist(body, final)
	if err != nil {
		t.Logf("variant playlist parse: %v", err)
		return nil
	}
	return segs
}

func printDelayMatrix(t *testing.T, results []delayResult) {
	t.Logf("\n=== delay calibration matrix ===")
	t.Logf("%-14s %-16s %-16s %-16s %-10s",
		"configured", "path_ping_ms", "tcp_rtt_ms", "delta_vs_base", "accuracy")
	var baseline float64
	var haveBase bool
	for _, r := range results {
		if r.configuredMs == 0 {
			baseline = r.pathPingMs
			haveBase = true
		}
	}
	for _, r := range results {
		deltaStr, accStr := "—", "—"
		if haveBase && r.configuredMs > 0 {
			delta := r.pathPingMs - baseline
			deltaStr = fmt.Sprintf("%+.2f", delta)
			// accuracy: how close the observed RTT increase is to the
			// configured delay (capped at 100%).
			acc := delta / float64(r.configuredMs) * 100
			if acc > 100 {
				acc = 100
			}
			accStr = fmt.Sprintf("%.0f%%", acc)
		}
		t.Logf("%-14d %-16.2f %-16.2f %-16s %-10s",
			r.configuredMs, r.pathPingMs, r.tcpRttMs, deltaStr, accStr)
	}
}
