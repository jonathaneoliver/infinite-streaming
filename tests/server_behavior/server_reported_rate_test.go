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
			RequestKind  string  `json:"request_kind"`
			BytesOut     int64   `json:"bytes_out"`
			TransferMs   float64 `json:"transfer_ms"`
			RequestRange string  `json:"request_range"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &logResp); err != nil {
		t.Fatalf("network log parse: %v", err)
	}

	// Reported rate per size, matched by the Range header we sent (full
	// pulls have none — match segment entries bigger than the largest
	// ranged size).
	reportedFor := func(size int64) (medianMbps float64, matched int) {
		wantRange := ""
		if size > 0 {
			wantRange = fmt.Sprintf("bytes=0-%d", size-1)
		}
		var rates []float64
		for _, e := range logResp.Entries {
			if e.RequestKind != "segment" {
				continue
			}
			if size > 0 {
				if e.RequestRange != wantRange {
					continue
				}
			} else if e.RequestRange != "" || e.BytesOut <= 4*1024*1024 {
				continue
			}
			if e.TransferMs <= 0 {
				continue // sub-ms flush; rate would be ~infinite — count but can't quantify
			}
			rates = append(rates, float64(e.BytesOut)*8/1e6/(e.TransferMs/1000))
		}
		return median(rates), len(rates)
	}

	// --- assertions + matrix -------------------------------------------
	sm := serverMatrix{
		Title:   fmt.Sprintf("Per-request reported rate vs client-measured (cap %d Mbps)", capMbps),
		Columns: []string{"transfer_size", "reps", "client_med_mbps", "reported_med_mbps", "reported/client", "log_entries", "verdict"},
	}
	for _, r := range results {
		clientMed := median(r.clientMbps)
		reportedMed, matched := reportedFor(r.size)
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
		sizeLabel := fmt.Sprintf("%d B", r.size)
		if isFull {
			sizeLabel = fmt.Sprintf("full segment (%d B)", r.clientBytes)
		}
		ratioLabel := "—"
		if ratio > 0 {
			ratioLabel = fmt.Sprintf("%.2f×", ratio)
		}
		t.Logf("%-24s client=%.2f reported=%.2f ratio=%s entries=%d %s",
			sizeLabel, clientMed, reportedMed, ratioLabel, matched, verdict)
		sm.Rows = append(sm.Rows, []string{
			sizeLabel, strconv.Itoa(len(r.clientMbps)),
			fmt.Sprintf("%.2f", clientMed), fmt.Sprintf("%.2f", reportedMed),
			ratioLabel, strconv.Itoa(matched), verdict,
		})
	}
	if !strict {
		t.Logf("small-transfer over-read is RECORDED, not asserted (#850 is open); flip REPORTED_RATE_STRICT=1 once delivery_rate_mbps ships")
	}
	p.postServerReport(t, "server_reported_rate",
		fmt.Sprintf("%d sizes × %d reps at %d Mbps", len(sizes), reps, capMbps),
		startedAt, !t.Failed(), sm)
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
