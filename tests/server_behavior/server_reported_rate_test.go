// server_reported_rate_test.go — is the proxy's own per-request
// throughput figure honest? (issue #850)
//
// The network log's implied rate (bytes_out / transfer_ms) times only
// the proxy's write+flush, which returns once the kernel accepts the
// bytes into the socket send buffer — NOT when they reach the client.
// tc HTB drains the qdisc BELOW the socket, so a sub-buffer transfer
// (init segments, small ranges) is absorbed instantly and can read
// ~1000× the actual wire rate. That over-read is what fed the iOS
// cold-start over-selection wedge investigation.
//
// This test calibrates the divergence curve: under a fixed cap it pulls
// a sweep of transfer sizes, measures each client-side (wall clock for
// the full body), then reads the proxy's network log for the same
// requests (matched by their Range header) and compares the server's
// implied rate against the client's measured one.
//
// It also asserts delivery_rate_mbps (#850) is honest — but ONLY for
// transfers that were genuinely wire-metered (≥1 MB and transfer_ms
// ≥5 ms). delivery_rate is connection-level, so a buffer-absorbed
// transfer (write returns before the wire drains) reads stale residue
// of the connection's recent history (53–179 Mbps under a 20 Mbps cap
// observed). Same gate the dashboard chart uses; see §1.14. Smaller /
// absorbed sizes are recorded, not asserted.
//
// Asserted (always):
//   - the client-measured full-segment rate ≈ the cap (sanity: shaping on);
//   - the full-segment reported/client ratio stays within a generous
//     honesty band — large transfers dominate wire time, so if even
//     THEY over-read wildly, per-request reporting is broken outright.
//
// Recorded only (the known #850 dishonesty, until delivery_rate ships):
//   - small-transfer ratios. Flip REPORTED_RATE_STRICT=1 to enforce
//     ratio ≤ 3 at every size — that's the acceptance switch for #850's
//     delivery_rate_mbps fix.
//
//	REPORTED_RATE_CAP_MBPS=20
//	REPORTED_RATE_REPS=3
//	REPORTED_RATE_LARGE_BAND=2.5   full-segment reported/client ceiling
//	REPORTED_RATE_STRICT=0
//
// Skipped in short mode. Run:
//
//	cd tests/server_behavior && go test -v -run TestServerReportedRate -timeout 10m
package server_behavior

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"testing"
	"time"
)

// Gates for asserting delivery_rate honesty — the transfer must be a real
// per-segment wire measurement, not a buffer-absorbed one where
// delivery_rate is stale connection residue. Same thresholds as the
// dashboard chart's extractDeliveryMarkers (see §1.14): ≥1 MB and the
// write took ≥5 ms (not instantly absorbed into the socket send buffer).
const (
	deliveryHonestMinBytes      = 1 << 20 // 1 MB
	deliveryHonestMinTransferMs = 5.0
)

func TestServerReportedRate(t *testing.T) {
	if testing.Short() {
		t.Skip("reported-rate calibration skipped in short mode")
	}
	capMbps := envInt("REPORTED_RATE_CAP_MBPS", 20)
	reps := envInt("REPORTED_RATE_REPS", 3)
	largeBand := envFloat("REPORTED_RATE_LARGE_BAND", 2.5)
	strict := envBool("REPORTED_RATE_STRICT", false)
	startedAt := time.Now()

	p := newProbe(t)
	if err := setRateLimit(p.c, p.apiBase, p.sess.InternalPort, capMbps); err != nil {
		t.Fatalf("set cap: %v", err)
	}
	defer setRateLimit(p.c, p.apiBase, p.sess.InternalPort, 0)
	time.Sleep(settleKernel)

	// Size sweep: 1 KB is the init-segment analog (the #850 headline
	// case); 0 means "the whole segment" (large enough that wire time
	// dominates the socket-buffer absorption).
	sizes := []int64{1024, 64 * 1024, 256 * 1024, 1024 * 1024, 4 * 1024 * 1024, 0}

	type sizeResult struct {
		size        int64
		clientMbps  []float64 // per rep
		clientBytes int64
	}
	results := make([]*sizeResult, 0, len(sizes))

	for _, size := range sizes {
		r := &sizeResult{size: size}
		for rep := 0; rep < reps; rep++ {
			// Fresh segment URL each rep so live-window roll-off can't
			// break a fetch mid-sweep.
			segURL, err := firstSegment(p.c, p.top.URL)
			if err != nil {
				t.Fatalf("segment refresh: %v", err)
			}
			t0 := time.Now()
			var n int64
			if size > 0 {
				n, err = rangeGet(p.c, segURL, size, 2*time.Minute)
			} else {
				var body []byte
				body, _, err = httpGet(p.c, segURL)
				n = int64(len(body))
			}
			dt := time.Since(t0).Seconds()
			if err != nil {
				t.Errorf("size %d rep %d: fetch: %v", size, rep, err)
				continue
			}
			if dt > 0 && n > 0 {
				r.clientMbps = append(r.clientMbps, float64(n)*8/1e6/dt)
				r.clientBytes = n
			}
			p.heartbeat(round2(median(r.clientMbps)), round2(median(r.clientMbps)), time.Since(startedAt).Seconds(), "playing")
		}
		results = append(results, r)
	}

	// --- read back the proxy's view of the same requests ---------------
	body, _, err := httpGet(p.c, fmt.Sprintf("https://%s/api/session/%s/network", p.apiBase, p.sess.SessionID))
	if err != nil {
		t.Fatalf("network log fetch: %v", err)
	}
	var logResp struct {
		Entries []struct {
			Timestamp        time.Time `json:"timestamp"`
			RequestKind      string    `json:"request_kind"`
			BytesOut         int64     `json:"bytes_out"`
			TransferMs       float64   `json:"transfer_ms"`
			RequestRange     string    `json:"request_range"`
			DeliveryRateMbps float64   `json:"delivery_rate_mbps"` // kernel tcpi_delivery_rate (#850); 0 pre-fix / not sampled
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &logResp); err != nil {
		t.Fatalf("network log parse: %v", err)
	}
	// The proxy's per-session network ring buffer is keyed by the numeric
	// port slot, which is RECYCLED across runs — so it carries STALE
	// entries from earlier plays on the same slot (especially the
	// empty-Range full-segment match, which would otherwise scoop up every
	// prior run's absorbed full fetches and poison the median). Scope every
	// match to entries produced during THIS run.
	runStart := startedAt.Add(-2 * time.Second) // small skew guard

	// Reported + kernel delivery rate per size, matched by the Range
	// header we sent (full pulls have none — match segment entries bigger
	// than the largest ranged size).
	//
	// delivery_rate_mbps is CONNECTION-level (tcpi_delivery_rate): it only
	// meters THIS transfer when the write was actually drained over the
	// wire, not absorbed whole into the socket send buffer. A buffer-
	// absorbed transfer (transfer_ms below the metered floor) returns
	// instantly and delivery_rate is stale residue of the connection's
	// recent history — 53–179 Mbps observed under a 20 Mbps cap. So the
	// delivery median is taken ONLY from metered entries; `meteredN` /
	// `deliveryN` let the caller tell "all absorbed → can't verify" apart
	// from "metered but delivery missing → regression". Mirrors the
	// dashboard chart's extractDeliveryMarkers gate and §1.14.
	reportedFor := func(size int64) (reportedMed, deliveryMed float64, impliedN, meteredN, deliveryN int) {
		wantRange := ""
		if size > 0 {
			wantRange = fmt.Sprintf("bytes=0-%d", size-1)
		}
		var rates, deliveries []float64
		for _, e := range logResp.Entries {
			if e.RequestKind != "segment" {
				continue
			}
			if !e.Timestamp.IsZero() && e.Timestamp.Before(runStart) {
				continue // stale entry from a prior run on this recycled slot
			}
			// Within this run the only segment requests are the test's own,
			// so RequestRange alone tells ranged sweeps and full pulls apart
			// (live-edge segment sizes vary run to run — don't gate on bytes).
			if e.RequestRange != wantRange {
				continue
			}
			metered := e.TransferMs >= deliveryHonestMinTransferMs
			if metered {
				meteredN++
				if e.DeliveryRateMbps > 0 {
					deliveries = append(deliveries, e.DeliveryRateMbps)
					deliveryN++
				}
			}
			if e.TransferMs <= 0 {
				continue // sub-ms flush; implied rate would be ~infinite — can't quantify
			}
			rates = append(rates, float64(e.BytesOut)*8/1e6/(e.TransferMs/1000))
		}
		return median(rates), median(deliveries), len(rates), meteredN, deliveryN
	}

	// --- assertions + matrix -------------------------------------------
	sm := serverMatrix{
		Title:   fmt.Sprintf("Per-request reported rate vs client-measured (cap %d Mbps)", capMbps),
		Columns: []string{"transfer_size", "reps", "cap_mbps", "client_http_get_mbps", "reported_med_mbps", "reported/client", "delivery_rate_med_mbps", "delivery/client", "log_entries", "verdict"},
	}
	for _, r := range results {
		clientMed := median(r.clientMbps)
		reportedMed, deliveryMed, matched, meteredN, deliveryN := reportedFor(r.size)
		deliveryRatio := 0.0
		if clientMed > 0 && deliveryMed > 0 {
			deliveryRatio = deliveryMed / clientMed
		}
		ratio := 0.0
		if clientMed > 0 && reportedMed > 0 {
			ratio = reportedMed / clientMed
		}
		isFull := r.size == 0
		verdict := "recorded"
		if isFull {
			verdict = "PASS"
			if clientMed < float64(capMbps)*0.5 || clientMed > float64(capMbps)*1.2 {
				verdict = "cap sanity fail"
				t.Errorf("full segment: client-measured %.2f Mbps is not ≈ the %d Mbps cap — shaping sanity failed, honesty numbers unusable", clientMed, capMbps)
			} else if matched == 0 {
				verdict = "no log entries"
				t.Errorf("full segment: no matching network-log entries — cannot verify reporting")
			} else if ratio > largeBand || (ratio > 0 && ratio < 1/largeBand) {
				verdict = "dishonest"
				t.Errorf("full segment: proxy reports %.2f Mbps vs client-measured %.2f (ratio %.2f, band ×%.1f) — per-request throughput reporting is broken even for large transfers", reportedMed, clientMed, ratio, largeBand)
			}
		} else if strict && ratio > 3 {
			verdict = "FAIL (strict)"
			t.Errorf("size %d: reported %.2f Mbps vs client %.2f (ratio %.2f > 3) — #850 over-read still present under REPORTED_RATE_STRICT", r.size, reportedMed, clientMed, ratio)
		}
		// delivery_rate_mbps (#850's fix) is asserted honest only where it's
		// a real per-segment wire measurement: transfers ≥1 MB that were
		// actually metered (not buffer-absorbed). This is the same gate the
		// dashboard chart uses; the §1.14 256 KB floor held only under
		// continuous pulling. Buffer-absorbed transfers (all sizes ≤256 KB
		// here, and any larger one the send buffer swallowed) are recorded,
		// not asserted — delivery there is connection residue.
		if (r.size == 0 || r.size >= deliveryHonestMinBytes) && meteredN > 0 {
			if deliveryN == 0 {
				verdict = "no delivery rate"
				t.Errorf("size %s: %d metered transfers but delivery_rate_mbps absent on all — the #850 sampling regressed (or a pre-#850 proxy is deployed)", sizeLabelFor(r.size, r.clientBytes), meteredN)
			} else if deliveryRatio > 1.5 || deliveryRatio < 0.5 {
				verdict = "delivery dishonest"
				t.Errorf("size %s: metered delivery_rate %.2f Mbps vs client-measured %.2f (ratio %.2f outside [0.5,1.5]) — the honest signal is no longer honest", sizeLabelFor(r.size, r.clientBytes), deliveryMed, clientMed, deliveryRatio)
			}
		}
		sizeLabel := sizeLabelFor(r.size, r.clientBytes)
		ratioLabel, deliveryLabel, deliveryRatioLabel := "—", "—", "—"
		if ratio > 0 {
			ratioLabel = fmt.Sprintf("%.2f×", ratio)
		}
		if deliveryMed > 0 {
			deliveryLabel = fmt.Sprintf("%.2f", deliveryMed)
		}
		if deliveryRatio > 0 {
			deliveryRatioLabel = fmt.Sprintf("%.2f×", deliveryRatio)
		}
		t.Logf("%-24s cap=%d client=%.2f reported=%.2f ratio=%s delivery=%s d_ratio=%s entries=%d %s",
			sizeLabel, capMbps, clientMed, reportedMed, ratioLabel, deliveryLabel, deliveryRatioLabel, matched, verdict)
		sm.Rows = append(sm.Rows, []string{
			sizeLabel, strconv.Itoa(len(r.clientMbps)),
			strconv.Itoa(capMbps),
			fmt.Sprintf("%.2f", clientMed), fmt.Sprintf("%.2f", reportedMed),
			ratioLabel, deliveryLabel, deliveryRatioLabel, strconv.Itoa(matched), verdict,
		})
	}
	if !strict {
		t.Logf("small-transfer over-read is RECORDED, not asserted (#850 is open); flip REPORTED_RATE_STRICT=1 once delivery_rate_mbps ships")
	}
	p.postServerReport(t, "server_reported_rate",
		fmt.Sprintf("%d sizes × %d reps at %d Mbps", len(sizes), reps, capMbps),
		startedAt, !t.Failed(), sm)
}

func sizeLabelFor(size, fullBytes int64) string {
	if size == 0 {
		return fmt.Sprintf("full segment (%d B)", fullBytes)
	}
	return fmt.Sprintf("%d B", size)
}

// median of a small float slice; 0 when empty.
func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}
