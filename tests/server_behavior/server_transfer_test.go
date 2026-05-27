// transfer_timeout_test.go — issue #524.
//
// Verifies the proxy's two transfer guards kill in-flight requests at the
// configured wall-clock deadline, that the applies_* flags scope which
// request kinds are subject to them, and that the fault counters tick.
//
//   - active timeout : bounds TOTAL response duration. We rate-cap the
//     session so the top-variant segment can't finish inside the timeout,
//     request it, and assert the transfer is cut ~active_timeout seconds
//     in, having delivered only a partial body.
//   - idle timeout   : bounds time since the last byte the server wrote.
//     We read one chunk then stop reading; TCP backpressure stalls the
//     proxy's writes, and the idle guard should cut the connection
//     ~idle_timeout after we went quiet.
//   - applies_segments scoping : with the flag OFF, the same slow segment
//     must run to completion (the guard is gated off), proving the flag
//     actually scopes enforcement rather than being decorative.
//
// The rate cap is derived from the discovered segment size so the segment
// always takes several times the timeout to pull — otherwise it'd finish
// before the guard fires and the test would be vacuous.
//
// Run:
//
//	cd tests/server_behavior && go test -v -run TestServerTransfer -timeout 5m
//
// Env:
//
//	THROUGHPUT_HOST, THROUGHPUT_API_PORT, THROUGHPUT_INSECURE, THROUGHPUT_CONTENT
//	TIMEOUT_ACTIVE_S=3   active timeout under test
//	TIMEOUT_IDLE_S=2     idle timeout under test
package server_behavior

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

func transferSet(activeS, idleS int, segs, mans, master bool) map[string]any {
	return map[string]any{
		"transfer_active_timeout_seconds":    activeS,
		"transfer_idle_timeout_seconds":      idleS,
		"transfer_timeout_applies_segments":  segs,
		"transfer_timeout_applies_manifests": mans,
		"transfer_timeout_applies_master":    master,
	}
}

// slowClient has no overall client-side deadline so the SERVER's transfer
// guard is what ends the request, not us.
func slowClient(insecure bool) *http.Client {
	return &http.Client{
		Timeout:   0,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}},
	}
}

func TestServerTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("transfer timeouts skipped in short mode")
	}
	activeS := envInt("TIMEOUT_ACTIVE_S", 3)
	idleS := envInt("TIMEOUT_IDLE_S", 2)

	p := newProbe(t)
	startedAt := time.Now()
	insecure := envBool("THROUGHPUT_INSECURE", defaultInsecure)
	sc := slowClient(insecure)
	var trows [][]string

	// Derive a rate cap so the full segment pull is ~5x the active
	// timeout — guarantees the guard fires mid-transfer.
	segBytesEst := int64(p.top.BandwidthBps) * 6 / 8
	fullMbit := float64(segBytesEst) * 8 / 1e6
	cap := int(fullMbit / float64(activeS*5))
	if cap < 1 {
		cap = 1
	}
	if cap > 10 {
		cap = 10
	}
	t.Logf("segment ~%.1f MB; rate cap %d Mbps → full pull ~%.0fs vs active timeout %ds",
		float64(segBytesEst)/(1<<20), cap, fullMbit/float64(cap), activeS)

	segs := p.pullOnce(t)
	if len(segs) == 0 {
		t.Fatalf("no segments available to probe")
	}
	segURL := segs[0]

	// --- active timeout: slow segment cut at ~activeS -------------------
	t.Run("active", func(t *testing.T) {
		if err := setShapeFull(p.c, p.apiBase, p.sess.InternalPort, cap, 0, 0); err != nil {
			t.Fatalf("set rate cap: %v", err)
		}
		if err := patchSession(p.c, p.apiBase, p.sess.SessionID, transferSet(activeS, 0, true, false, false)); err != nil {
			t.Fatalf("arm active timeout: %v", err)
		}
		defer func() {
			patchSession(p.c, p.apiBase, p.sess.SessionID, transferSet(0, 0, true, false, false))
			setShapeFull(p.c, p.apiBase, p.sess.InternalPort, 0, 0, 0)
		}()
		time.Sleep(settleKernel)

		before := counterValue(p.c, p.apiBase, p.playerID, "fault_count_transfer_active_timeout")
		elapsed, n, completed := pullWithServerDeadline(sc, segURL)
		after := counterValue(p.c, p.apiBase, p.playerID, "fault_count_transfer_active_timeout")
		t.Logf("active: cut after %.1fs, %d bytes, completed=%v, counter %d→%d",
			elapsed.Seconds(), n, completed, before, after)

		if completed {
			t.Errorf("active timeout: segment completed (%d bytes) — guard did not fire (cap too high?)", n)
		}
		if elapsed > time.Duration(activeS+4)*time.Second {
			t.Errorf("active timeout: transfer ran %.1fs, expected to be cut near %ds", elapsed.Seconds(), activeS)
		}
		if after <= before {
			t.Errorf("active timeout: fault_count_transfer_active_timeout did not increment (%d→%d)", before, after)
		}
		trows = append(trows, []string{"active", fmt.Sprintf("%d", activeS),
			fmt.Sprintf("cut after %.1fs, %d bytes, completed=%v", elapsed.Seconds(), n, completed),
			fmt.Sprintf("%d→%d", before, after)})
	})

	// --- scoping: applies_segments=false → segment runs to completion ---
	t.Run("scoping_off", func(t *testing.T) {
		if err := setShapeFull(p.c, p.apiBase, p.sess.InternalPort, cap, 0, 0); err != nil {
			t.Fatalf("set rate cap: %v", err)
		}
		// Active timeout armed, but NOT applied to segments.
		if err := patchSession(p.c, p.apiBase, p.sess.SessionID, transferSet(activeS, 0, false, false, false)); err != nil {
			t.Fatalf("arm scoped-off timeout: %v", err)
		}
		defer func() {
			patchSession(p.c, p.apiBase, p.sess.SessionID, transferSet(0, 0, true, false, false))
			setShapeFull(p.c, p.apiBase, p.sess.InternalPort, 0, 0, 0)
		}()
		time.Sleep(settleKernel)

		elapsed, n, completed := pullWithServerDeadline(sc, segURL)
		t.Logf("scoping_off: ran %.1fs, %d bytes, completed=%v", elapsed.Seconds(), n, completed)
		if !completed {
			t.Errorf("scoping: segment was cut after %.1fs despite applies_segments=false — flag not honoured", elapsed.Seconds())
		}
		trows = append(trows, []string{"active(scoped_off)", fmt.Sprintf("%d", activeS),
			fmt.Sprintf("ran %.1fs, %d bytes, completed=%v", elapsed.Seconds(), n, completed), "-"})
	})

	// --- idle timeout: stall the client, server cuts at ~idleS ----------
	t.Run("idle", func(t *testing.T) {
		if err := setShapeFull(p.c, p.apiBase, p.sess.InternalPort, cap, 0, 0); err != nil {
			t.Fatalf("set rate cap: %v", err)
		}
		if err := patchSession(p.c, p.apiBase, p.sess.SessionID, transferSet(0, idleS, true, false, false)); err != nil {
			t.Fatalf("arm idle timeout: %v", err)
		}
		defer func() {
			patchSession(p.c, p.apiBase, p.sess.SessionID, transferSet(0, 0, true, false, false))
			setShapeFull(p.c, p.apiBase, p.sess.InternalPort, 0, 0, 0)
		}()
		time.Sleep(settleKernel)

		before := counterValue(p.c, p.apiBase, p.playerID, "fault_count_transfer_idle_timeout")
		stalledFor, cut := pullThenStall(sc, segURL, time.Duration(idleS+4)*time.Second)
		after := counterValue(p.c, p.apiBase, p.playerID, "fault_count_transfer_idle_timeout")
		t.Logf("idle: connection cut=%v ~%.1fs after going quiet, counter %d→%d",
			cut, stalledFor.Seconds(), before, after)

		if !cut {
			t.Errorf("idle timeout: connection survived %.1fs of client silence — guard did not fire", stalledFor.Seconds())
		}
		if after <= before {
			t.Errorf("idle timeout: fault_count_transfer_idle_timeout did not increment (%d→%d)", before, after)
		}
		trows = append(trows, []string{"idle", fmt.Sprintf("%d", idleS),
			fmt.Sprintf("cut=%v ~%.1fs after quiet", cut, stalledFor.Seconds()),
			fmt.Sprintf("%d→%d", before, after)})
	})

	p.postServerReport(t, "server_transfer", "active + idle + scoping", startedAt, !t.Failed(), serverMatrix{
		Title:   "Transfer timeouts (active/idle cut timing + applies_* scoping + counters)",
		Columns: []string{"guard", "timeout_s", "observed", "counter"},
		Rows:    trows,
	})
}

// pullWithServerDeadline reads the body to completion or until the server
// cuts it. Returns wall-clock elapsed, bytes received, and whether the
// body completed cleanly.
func pullWithServerDeadline(c *http.Client, segURL string) (time.Duration, int64, bool) {
	start := time.Now()
	resp, err := c.Get(segURL)
	if err != nil {
		return time.Since(start), 0, false
	}
	defer resp.Body.Close()
	n, rerr := io.Copy(io.Discard, resp.Body)
	return time.Since(start), n, rerr == nil
}

// pullThenStall reads one chunk, stops reading for `stall`, then tries to
// resume. If the server cut the connection during the silence the resume
// read errors. Returns how long we were quiet and whether the connection
// was cut.
func pullThenStall(c *http.Client, segURL string, stall time.Duration) (time.Duration, bool) {
	resp, err := c.Get(segURL)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()

	buf := make([]byte, 16*1024)
	if _, err := resp.Body.Read(buf); err != nil {
		// Couldn't even get the first chunk — treat as no clean idle test.
		return 0, err != io.EOF
	}
	quietStart := time.Now()
	time.Sleep(stall)
	// Resume reading; a cut connection surfaces as a read error here.
	_, rerr := io.Copy(io.Discard, resp.Body)
	return time.Since(quietStart), rerr != nil
}

// counterValue reads an integer fault counter off the session record.
func counterValue(c *http.Client, apiBase, playerID, key string) int {
	m, err := getSessionMap(c, apiBase, playerID)
	if err != nil {
		return 0
	}
	if v, ok := mapFloat(m, key); ok {
		return int(v)
	}
	return 0
}
