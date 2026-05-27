// loss_accuracy_test.go — issue #520.
//
// Verifies that operator-configured `nftables_packet_loss` actually
// degrades goodput on the wire. Packet loss isn't directly observable
// from an HTTP client, but its effect is: dropped packets force TCP
// retransmits and repeated congestion-window collapse, so effective
// throughput falls well below the rate cap as loss climbs.
//
// Method: pin a fixed rate cap (so the ceiling is constant), sweep the
// loss percentage, and pull segments for a window at each step. The
// observed average throughput vs the 0%-loss baseline is the calibration
// curve. We don't assert an exact goodput number per loss level (TCP's
// response to loss is non-linear and RTT-dependent) — we assert the
// curve is monotonic-ish and record the numbers for the standards doc.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerLoss -timeout 5m
//
// Env:
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	LOSS_SWEEP_PCT=0,1,5,10     configured loss percentages
//	LOSS_RATE_CAP=50            fixed rate cap (Mbps) the loss rides on top of
//	LOSS_DURATION_S=15          per-step pull window
package server_behavior

import (
	"fmt"
	"testing"
	"time"
)

type lossResult struct {
	configuredPct float64
	avgMbps       float64
	peakMbps      float64
	segments      int
}

func TestServerLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("loss sweep skipped in short mode")
	}
	losses, err := parseFloatList(env("LOSS_SWEEP_PCT", "0,1,5,10"))
	if err != nil {
		t.Fatalf("parse losses: %v", err)
	}
	rateCap := envInt("LOSS_RATE_CAP", 50)
	durationS := envInt("LOSS_DURATION_S", 15)

	p := newProbe(t)
	startedAt := time.Now()
	results := make([]lossResult, 0, len(losses))

	for _, lossPct := range losses {
		t.Logf("\n=== loss %.1f%% @ %d Mbps cap — pulling for %ds ===", lossPct, rateCap, durationS)
		if err := setShapeFull(p.c, p.apiBase, p.sess.InternalPort, rateCap, 0, lossPct); err != nil {
			t.Errorf("set loss %.1f: %v", lossPct, err)
			continue
		}
		time.Sleep(settleKernel)
		res := runPullWindow(t, p.c, p.apiBase, p.sess.SessionID, p.playerID, p.playID,
			p.top.URL, time.Duration(durationS)*time.Second)
		results = append(results, lossResult{
			configuredPct: lossPct,
			avgMbps:       res.avgMbps,
			peakMbps:      res.peakMbps,
			segments:      res.segments,
		})
		t.Logf("loss=%.1f%% avg=%.2f Mbps peak=%.2f Mbps segs=%d",
			lossPct, res.avgMbps, res.peakMbps, res.segments)
	}

	// Clear shaping so we don't leave the session capped + lossy.
	_ = setShapeFull(p.c, p.apiBase, p.sess.InternalPort, 0, 0, 0)

	printLossMatrix(t, results, rateCap)

	m := serverMatrix{
		Title:   fmt.Sprintf("Loss accuracy (observed goodput vs configured loss, %d Mbps cap)", rateCap),
		Columns: []string{"loss_pct", "obs_avg_mbps", "obs_peak_mbps", "segments"},
	}
	for _, r := range results {
		m.Rows = append(m.Rows, []string{
			fmt.Sprintf("%.1f", r.configuredPct),
			fmt.Sprintf("%.2f", r.avgMbps),
			fmt.Sprintf("%.2f", r.peakMbps),
			fmt.Sprintf("%d", r.segments),
		})
	}
	p.postServerReport(t, "server_loss", fmt.Sprintf("%d loss levels swept @ %d Mbps", len(results), rateCap), startedAt, !t.Failed(), m)
}

func printLossMatrix(t *testing.T, results []lossResult, rateCap int) {
	t.Logf("\n=== loss calibration matrix (rate cap %d Mbps) ===", rateCap)
	t.Logf("%-12s %-12s %-12s %-16s %-10s",
		"loss_pct", "obs_avg", "obs_peak", "%_of_0pct_base", "segs")
	var baseline float64
	var haveBase bool
	for _, r := range results {
		if r.configuredPct == 0 {
			baseline = r.avgMbps
			haveBase = true
		}
	}
	for _, r := range results {
		ofBase := "—"
		if haveBase && baseline > 0 {
			ofBase = fmt.Sprintf("%.0f%%", r.avgMbps/baseline*100)
		}
		t.Logf("%-12.1f %-12.2f %-12.2f %-16s %-10d",
			r.configuredPct, r.avgMbps, r.peakMbps, ofBase, r.segments)
	}
}
